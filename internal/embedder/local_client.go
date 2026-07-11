// Package embedder also contains the local sidecar adapter for Phase 7D.
//
// WSMS owns the sidecar protocol even when the sidecar is backed by common
// serving stacks. The protocol deliberately asks the sidecar to echo namespace,
// profile revision, dimensions, normalization, input order, and SHA-256
// digests before vectors are accepted. This mirrors the operational shape of:
//   - Qwen/Qwen3-Embedding-0.6B, whose model card documents instruction-aware
//     query/document embeddings and 1024-dimensional normalized vectors:
//     https://huggingface.co/Qwen/Qwen3-Embedding-0.6B
//   - Hugging Face Text Embeddings Inference, whose local serving API exposes
//     an /embed endpoint for embedding batches:
//     https://huggingface.co/docs/text-embeddings-inference/en/index
//   - llama.cpp's local server, which documents local HTTP embedding service
//     operation under tools/server:
//     https://github.com/ggml-org/llama.cpp/tree/master/tools/server
package embedder

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
)

const (
	localHealthPath = "/wsms/v1/health"
	localEmbedPath  = "/wsms/v1/embed"
)

// LocalClientOptions configures a WSMS-owned local embedding sidecar client.
//
// Exactly one of Endpoint or SocketPath must be set. Endpoint must be loopback
// HTTP; SocketPath uses HTTP over a Unix-domain socket. MaxResponseBytes is
// required so malformed sidecars cannot force unbounded response buffering.
type LocalClientOptions struct {
	Namespace        EmbeddingNamespace
	Endpoint         string
	SocketPath       string
	MaxResponseBytes int64
}

// LocalClient implements Backend against the private WSMS local sidecar
// protocol. It intentionally does not implement Embedder directly; Supervised
// remains responsible for admission, caching, timeout, and circuit policy.
type LocalClient struct {
	namespace        EmbeddingNamespace
	baseURL          *url.URL
	client           *http.Client
	maxResponseBytes int64
}

var _ Backend = (*LocalClient)(nil)
var _ backendSelfChecker = (*LocalClient)(nil)

// NewLocalClient returns a fail-closed local sidecar backend.
func NewLocalClient(opts LocalClientOptions) (*LocalClient, error) {
	if err := opts.Namespace.Validate(); err != nil {
		return nil, err
	}
	if opts.MaxResponseBytes <= 0 {
		return nil, fmt.Errorf("local embedder max response bytes must be positive")
	}
	if (strings.TrimSpace(opts.Endpoint) == "") == (strings.TrimSpace(opts.SocketPath) == "") {
		return nil, fmt.Errorf("set exactly one local embedder endpoint or socket path")
	}

	client := &http.Client{CheckRedirect: rejectLocalRedirect}
	base := &url.URL{Scheme: "http", Host: "unix"}
	if opts.SocketPath != "" {
		socketPath := opts.SocketPath
		transport := &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		}
		client.Transport = transport
	} else {
		parsed, err := parseLoopbackEndpoint(opts.Endpoint)
		if err != nil {
			return nil, err
		}
		base = parsed
		client.Transport = &http.Transport{Proxy: nil}
	}

	return &LocalClient{
		namespace:        opts.Namespace,
		baseURL:          base,
		client:           client,
		maxResponseBytes: opts.MaxResponseBytes,
	}, nil
}

func rejectLocalRedirect(_ *http.Request, _ []*http.Request) error {
	return fmt.Errorf("%w: local embedder redirects are not allowed", ErrDegraded)
}

// EmbedDocuments embeds an ordered document batch through the local sidecar.
func (c *LocalClient) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: nil local client", ErrDegraded)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	return c.embed(ctx, RoleDocument, texts)
}

// EmbedQuery embeds one query through the local sidecar.
func (c *LocalClient) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.embed(ctx, RoleQuery, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("%w: query response count %d != 1", ErrInvalidEmbedding, len(vectors))
	}
	return vectors[0], nil
}

// SelfCheck verifies that the sidecar is serving the exact namespace profile
// expected by Supervised.
func (c *LocalClient) SelfCheck(ctx context.Context, namespace EmbeddingNamespace) error {
	if c == nil {
		return fmt.Errorf("%w: nil local client", ErrDegraded)
	}
	if err := namespace.Validate(); err != nil {
		return err
	}
	if namespace.ID != c.namespace.ID {
		return fmt.Errorf("%w: self-check namespace mismatch", ErrInvalidEmbedding)
	}
	var health localHealthResponse
	if err := c.doJSON(ctx, http.MethodGet, localHealthPath, nil, &health); err != nil {
		return err
	}
	if !health.Ready {
		return fmt.Errorf("%w: local embedder not ready", ErrDegraded)
	}
	if err := validateHealthResponse(health, namespace); err != nil {
		return err
	}
	return nil
}

func (c *LocalClient) embed(ctx context.Context, role Role, texts []string) ([][]float32, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: nil local client", ErrDegraded)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if role != RoleDocument && role != RoleQuery {
		return nil, fmt.Errorf("%w: unsupported role %q", ErrInvalidEmbedding, role)
	}
	requestID, err := newLocalRequestID()
	if err != nil {
		return nil, err
	}
	req := localEmbedRequest{
		RequestID: requestID,
		Namespace: c.namespace.ID,
		Profile:   c.namespace.Profile,
		Role:      role,
		Inputs:    make([]localEmbedInput, len(texts)),
	}
	for i, text := range texts {
		req.Inputs[i] = localEmbedInput{
			Index:  i,
			Text:   text,
			SHA256: sha256Hex(text),
		}
	}

	var resp localEmbedResponse
	if err := c.doJSON(ctx, http.MethodPost, localEmbedPath, req, &resp); err != nil {
		return nil, err
	}
	if err := validateEmbedResponse(resp, req, c.namespace); err != nil {
		return nil, err
	}
	out := make([][]float32, len(resp.Embeddings))
	for i, embedding := range resp.Embeddings {
		out[i] = copyVector(embedding.Vector)
	}
	return out, nil
}

func (c *LocalClient) doJSON(ctx context.Context, method, suffix string, body any, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(suffix), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: local embedder request failed", ErrDegraded)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, c.maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: local embedder response read failed", ErrDegraded)
	}
	if int64(len(data)) > c.maxResponseBytes {
		return fmt.Errorf("%w: local embedder response exceeds configured limit", ErrDegraded)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: local embedder status %d", ErrDegraded, resp.StatusCode)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%w: malformed local embedder response", ErrInvalidEmbedding)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("%w: trailing local embedder response data", ErrInvalidEmbedding)
	}
	return nil
}

func (c *LocalClient) endpoint(suffix string) string {
	u := *c.baseURL
	basePath := strings.TrimRight(u.Path, "/")
	u.Path = path.Clean(basePath + suffix)
	if suffix == "/" {
		u.Path = basePath + suffix
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

type localEmbedRequest struct {
	RequestID string            `json:"request_id"`
	Namespace string            `json:"namespace"`
	Profile   NamespaceProfile  `json:"profile"`
	Role      Role              `json:"role"`
	Inputs    []localEmbedInput `json:"inputs"`
}

type localEmbedInput struct {
	Index  int    `json:"index"`
	Text   string `json:"text"`
	SHA256 string `json:"sha256"`
}

type localEmbedResponse struct {
	RequestID       string                 `json:"request_id"`
	Namespace       string                 `json:"namespace"`
	ProfileRevision string                 `json:"profile_revision"`
	Dimensions      int                    `json:"dimensions"`
	Normalization   Normalization          `json:"normalization"`
	Role            Role                   `json:"role"`
	Embeddings      []localEmbedVectorEcho `json:"embeddings"`
}

type localEmbedVectorEcho struct {
	Index  int       `json:"index"`
	SHA256 string    `json:"sha256"`
	Vector []float32 `json:"vector"`
}

type localHealthResponse struct {
	Ready           bool           `json:"ready"`
	Namespace       string         `json:"namespace"`
	Provider        string         `json:"provider"`
	ModelRepository string         `json:"model_repository"`
	ProfileRevision string         `json:"profile_revision"`
	Dimensions      int            `json:"dimensions"`
	DistanceMetric  DistanceMetric `json:"distance_metric"`
	Normalization   Normalization  `json:"normalization"`
}

func validateEmbedResponse(resp localEmbedResponse, req localEmbedRequest, namespace EmbeddingNamespace) error {
	if resp.RequestID != req.RequestID {
		return fmt.Errorf("%w: request id mismatch", ErrInvalidEmbedding)
	}
	if resp.Namespace != namespace.ID {
		return fmt.Errorf("%w: namespace mismatch", ErrInvalidEmbedding)
	}
	if resp.ProfileRevision != namespace.Profile.ExactRevision {
		return fmt.Errorf("%w: profile revision mismatch", ErrInvalidEmbedding)
	}
	if resp.Dimensions != namespace.Profile.Dimensions {
		return fmt.Errorf("%w: dimensions mismatch", ErrInvalidEmbedding)
	}
	if resp.Normalization != namespace.Profile.Normalization {
		return fmt.Errorf("%w: normalization mismatch", ErrInvalidEmbedding)
	}
	if resp.Role != req.Role {
		return fmt.Errorf("%w: role mismatch", ErrInvalidEmbedding)
	}
	if len(resp.Embeddings) != len(req.Inputs) {
		return fmt.Errorf("%w: response count %d != input count %d", ErrInvalidEmbedding, len(resp.Embeddings), len(req.Inputs))
	}
	for i, item := range resp.Embeddings {
		if item.Index != i || req.Inputs[i].Index != i {
			return fmt.Errorf("%w: order mismatch at input %d", ErrInvalidEmbedding, i)
		}
		if item.SHA256 != req.Inputs[i].SHA256 {
			return fmt.Errorf("%w: digest mismatch at input %d", ErrInvalidEmbedding, i)
		}
		if err := validateVector(item.Vector, namespace.Profile.Dimensions, namespace.Profile.Normalization); err != nil {
			return err
		}
	}
	return nil
}

func validateHealthResponse(resp localHealthResponse, namespace EmbeddingNamespace) error {
	profile := namespace.Profile
	if resp.Namespace != namespace.ID {
		return fmt.Errorf("%w: health namespace mismatch", ErrInvalidEmbedding)
	}
	if resp.Provider != profile.Provider {
		return fmt.Errorf("%w: health provider mismatch", ErrInvalidEmbedding)
	}
	if resp.ModelRepository != profile.ModelRepository {
		return fmt.Errorf("%w: health model repository mismatch", ErrInvalidEmbedding)
	}
	if resp.ProfileRevision != profile.ExactRevision {
		return fmt.Errorf("%w: health profile revision mismatch", ErrInvalidEmbedding)
	}
	if resp.Dimensions != profile.Dimensions {
		return fmt.Errorf("%w: health dimensions mismatch", ErrInvalidEmbedding)
	}
	if resp.DistanceMetric != profile.DistanceMetric {
		return fmt.Errorf("%w: health distance metric mismatch", ErrInvalidEmbedding)
	}
	if resp.Normalization != profile.Normalization {
		return fmt.Errorf("%w: health normalization mismatch", ErrInvalidEmbedding)
	}
	return nil
}

func parseLoopbackEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid local embedder endpoint")
	}
	if parsed.Scheme != "http" {
		return nil, fmt.Errorf("local embedder endpoint must use loopback http")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("local embedder endpoint must not contain user info")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("local embedder endpoint must not contain query or fragment")
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("local embedder endpoint host is required")
	}
	if !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("local embedder endpoint must be loopback")
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}

func newLocalRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("%w: request id generation failed", ErrDegraded)
	}
	return "req_" + hex.EncodeToString(b[:]), nil
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
