package embedder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLocalClientHTTPBatchQueryAndHealth(t *testing.T) {
	ns := MustNamespace(testProfile())
	var seen []localEmbedRequest
	server := newLocalSidecarServer(t, ns, &seen, nil, nil)
	defer server.Close()

	client, err := NewLocalClient(LocalClientOptions{
		Namespace:        ns,
		Endpoint:         server.URL,
		MaxResponseBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("new local client: %v", err)
	}
	if err := client.SelfCheck(context.Background(), ns); err != nil {
		t.Fatalf("self check: %v", err)
	}
	docs, err := client.EmbedDocuments(context.Background(), []string{"alpha page", "beta page"})
	if err != nil {
		t.Fatalf("embed documents: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("document vectors = %d, want 2", len(docs))
	}
	query, err := client.EmbedQuery(context.Background(), "find alpha")
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	if len(query) != ns.Profile.Dimensions {
		t.Fatalf("query dims = %d, want %d", len(query), ns.Profile.Dimensions)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want document and query requests", len(seen))
	}
	if seen[0].Role != RoleDocument || seen[1].Role != RoleQuery {
		t.Fatalf("roles = %s/%s, want document/query", seen[0].Role, seen[1].Role)
	}
	if got := seen[0].Inputs[0].SHA256; got != sha256Hex("alpha page") {
		t.Fatalf("document digest = %q, want sha256(alpha page)", got)
	}
	if !reflect.DeepEqual(seen[0].Profile, ns.Profile) {
		t.Fatalf("request profile = %#v, want %#v", seen[0].Profile, ns.Profile)
	}
}

func TestLocalClientUnixSocketQuery(t *testing.T) {
	ns := MustNamespace(testProfile())
	var seen []localEmbedRequest
	handler := newLocalSidecarHandler(t, ns, &seen, nil, nil)
	socket := filepath.Join(t.TempDir(), "embedder.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	server := &http.Server{Handler: handler}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = os.Remove(socket)
		err := <-serveDone
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("unix server: %v", err)
		}
	})

	client, err := NewLocalClient(LocalClientOptions{
		Namespace:        ns,
		SocketPath:       socket,
		MaxResponseBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("new unix local client: %v", err)
	}
	vector, err := client.EmbedQuery(context.Background(), "unix query")
	if err != nil {
		t.Fatalf("embed query over unix socket: %v", err)
	}
	if len(vector) != ns.Profile.Dimensions {
		t.Fatalf("unix query dims = %d, want %d", len(vector), ns.Profile.Dimensions)
	}
	if len(seen) != 1 || seen[0].Role != RoleQuery {
		t.Fatalf("seen unix requests = %#v, want one query", seen)
	}
}

func TestLocalClientRejectsNonLoopbackAndAmbiguousEndpoints(t *testing.T) {
	ns := MustNamespace(testProfile())
	tests := []struct {
		name string
		opts LocalClientOptions
		want string
	}{
		{
			name: "non_loopback_tcp",
			opts: LocalClientOptions{
				Namespace:        ns,
				Endpoint:         "http://192.0.2.10:8080",
				MaxResponseBytes: 4096,
			},
			want: "loopback",
		},
		{
			name: "hosted_https",
			opts: LocalClientOptions{
				Namespace:        ns,
				Endpoint:         "https://127.0.0.1:8080",
				MaxResponseBytes: 4096,
			},
			want: "loopback http",
		},
		{
			name: "both_endpoint_and_socket",
			opts: LocalClientOptions{
				Namespace:        ns,
				Endpoint:         "http://127.0.0.1:8080",
				SocketPath:       "/tmp/wsms.sock",
				MaxResponseBytes: 4096,
			},
			want: "exactly one",
		},
		{
			name: "missing_response_bound",
			opts: LocalClientOptions{
				Namespace: ns,
				Endpoint:  "http://127.0.0.1:8080",
			},
			want: "max response bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewLocalClient(tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLocalClientCancellationDeadline(t *testing.T) {
	ns := MustNamespace(testProfile())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != localEmbedPath {
			t.Fatalf("path = %s, want %s", r.URL.Path, localEmbedPath)
		}
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()

	client, err := NewLocalClient(LocalClientOptions{
		Namespace:        ns,
		Endpoint:         server.URL,
		MaxResponseBytes: 4096,
	})
	if err != nil {
		t.Fatalf("new local client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err = client.EmbedQuery(ctx, "deadline query")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v, want context deadline", err)
	}
}

func TestLocalClientRejectsRedirectsWithoutProxyExfiltration(t *testing.T) {
	ns := MustNamespace(testProfile())
	const sensitive = "audit-secret-payload"
	proxyBodies := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		proxyBodies <- string(body)
		http.Error(w, "proxy should not be reached", http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != localEmbedPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", "http://203.0.113.10/wsms-exfil")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer sidecar.Close()

	client, err := NewLocalClient(LocalClientOptions{
		Namespace:        ns,
		Endpoint:         sidecar.URL,
		MaxResponseBytes: 4096,
	})
	if err != nil {
		t.Fatalf("new local client: %v", err)
	}
	if transport, ok := client.client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatalf("http local client transport must disable ambient proxies: %#v", client.client.Transport)
	}
	if client.client.CheckRedirect == nil {
		t.Fatal("http local client must install a fail-closed redirect policy")
	}
	_, err = client.EmbedQuery(context.Background(), sensitive)
	if !errors.Is(err, ErrDegraded) {
		t.Fatalf("redirect error = %v, want degraded", err)
	}
	if errStringContains(err, sensitive) {
		t.Fatalf("redirect error leaked request payload: %v", err)
	}
	select {
	case body := <-proxyBodies:
		t.Fatalf("redirect reached proxy with body %q", body)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLocalClientRejectsInvalidResponsesWithoutLeakingBodies(t *testing.T) {
	ns := MustNamespace(testProfile())
	const sensitive = "SENSITIVE_PAYLOAD_SHOULD_NOT_LEAK"
	tests := []struct {
		name    string
		max     int64
		handler http.HandlerFunc
		wantErr error
	}{
		{
			name: "status_body_redacted",
			max:  4096,
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, sensitive, http.StatusInternalServerError)
			},
			wantErr: ErrDegraded,
		},
		{
			name: "oversized_body_redacted",
			max:  64,
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"request_id":"` + strings.Repeat("x", 256) + sensitive + `"}`))
			},
			wantErr: ErrDegraded,
		},
		{
			name: "malformed_body_redacted",
			max:  4096,
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"request_id":` + sensitive))
			},
			wantErr: ErrInvalidEmbedding,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			client, err := NewLocalClient(LocalClientOptions{
				Namespace:        ns,
				Endpoint:         server.URL,
				MaxResponseBytes: tt.max,
			})
			if err != nil {
				t.Fatalf("new local client: %v", err)
			}
			_, err = client.EmbedQuery(context.Background(), "SENSITIVE_REQUEST_TEXT")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if errStringContains(err, sensitive) || errStringContains(err, "SENSITIVE_REQUEST_TEXT") {
				t.Fatalf("error leaked response or request payload: %v", err)
			}
		})
	}
}

func TestLocalClientRejectsProtocolMismatches(t *testing.T) {
	ns := MustNamespace(testProfile())
	tests := []struct {
		name   string
		mutate func(*localEmbedResponse)
	}{
		{
			name: "namespace",
			mutate: func(resp *localEmbedResponse) {
				resp.Namespace = "emb_wrong"
			},
		},
		{
			name: "profile_revision",
			mutate: func(resp *localEmbedResponse) {
				resp.ProfileRevision = "rev-wrong"
			},
		},
		{
			name: "normalization",
			mutate: func(resp *localEmbedResponse) {
				resp.Normalization = "none"
			},
		},
		{
			name: "order",
			mutate: func(resp *localEmbedResponse) {
				resp.Embeddings[0].Index = 1
			},
		},
		{
			name: "digest",
			mutate: func(resp *localEmbedResponse) {
				resp.Embeddings[0].SHA256 = "bad-digest"
			},
		},
		{
			name: "vector_not_normalized",
			mutate: func(resp *localEmbedResponse) {
				resp.Embeddings[0].Vector = []float32{2, 0, 0}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newLocalSidecarServer(t, ns, nil, tt.mutate, nil)
			defer server.Close()
			client, err := NewLocalClient(LocalClientOptions{
				Namespace:        ns,
				Endpoint:         server.URL,
				MaxResponseBytes: 64 << 10,
			})
			if err != nil {
				t.Fatalf("new local client: %v", err)
			}
			_, err = client.EmbedQuery(context.Background(), "mismatch query")
			if !errors.Is(err, ErrInvalidEmbedding) {
				t.Fatalf("mismatch error = %v, want invalid embedding", err)
			}
		})
	}
}

func TestLocalClientHealthValidationMismatches(t *testing.T) {
	ns := MustNamespace(testProfile())
	tests := []struct {
		name    string
		mutate  func(*localHealthResponse)
		wantErr error
	}{
		{
			name: "not_ready",
			mutate: func(resp *localHealthResponse) {
				resp.Ready = false
			},
			wantErr: ErrDegraded,
		},
		{
			name: "wrong_model",
			mutate: func(resp *localHealthResponse) {
				resp.ModelRepository = "wrong/model"
			},
			wantErr: ErrInvalidEmbedding,
		},
		{
			name: "wrong_dimensions",
			mutate: func(resp *localHealthResponse) {
				resp.Dimensions++
			},
			wantErr: ErrInvalidEmbedding,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newLocalSidecarServer(t, ns, nil, nil, tt.mutate)
			defer server.Close()
			client, err := NewLocalClient(LocalClientOptions{
				Namespace:        ns,
				Endpoint:         server.URL,
				MaxResponseBytes: 64 << 10,
			})
			if err != nil {
				t.Fatalf("new local client: %v", err)
			}
			err = client.SelfCheck(context.Background(), ns)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("health error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func newLocalSidecarServer(
	t *testing.T,
	ns EmbeddingNamespace,
	seen *[]localEmbedRequest,
	mutateEmbed func(*localEmbedResponse),
	mutateHealth func(*localHealthResponse),
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(newLocalSidecarHandler(t, ns, seen, mutateEmbed, mutateHealth))
}

func newLocalSidecarHandler(
	t *testing.T,
	ns EmbeddingNamespace,
	seen *[]localEmbedRequest,
	mutateEmbed func(*localEmbedResponse),
	mutateHealth func(*localHealthResponse),
) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(localHealthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("health method = %s, want GET", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		resp := localHealthResponse{
			Ready:           true,
			Namespace:       ns.ID,
			Provider:        ns.Profile.Provider,
			ModelRepository: ns.Profile.ModelRepository,
			ProfileRevision: ns.Profile.ExactRevision,
			Dimensions:      ns.Profile.Dimensions,
			DistanceMetric:  ns.Profile.DistanceMetric,
			Normalization:   ns.Profile.Normalization,
		}
		if mutateHealth != nil {
			mutateHealth(&resp)
		}
		writeLocalJSON(t, w, resp)
	})
	mux.HandleFunc(localEmbedPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("embed method = %s, want POST", r.Method)
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		var req localEmbedRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.RequestID == "" {
			t.Errorf("request id is empty")
		}
		if req.Namespace != ns.ID {
			t.Errorf("namespace = %s, want %s", req.Namespace, ns.ID)
		}
		if req.Role != RoleDocument && req.Role != RoleQuery {
			t.Errorf("role = %s, want document or query", req.Role)
		}
		if !reflect.DeepEqual(req.Profile, ns.Profile) {
			t.Errorf("profile = %#v, want %#v", req.Profile, ns.Profile)
		}
		for i, input := range req.Inputs {
			if input.Index != i {
				t.Errorf("input index %d = %d", i, input.Index)
			}
			if input.SHA256 != sha256Hex(input.Text) {
				t.Errorf("input digest %d = %s, want sha256(text)", i, input.SHA256)
			}
		}
		if seen != nil {
			*seen = append(*seen, req)
		}
		resp := localEmbedResponse{
			RequestID:       req.RequestID,
			Namespace:       ns.ID,
			ProfileRevision: ns.Profile.ExactRevision,
			Dimensions:      ns.Profile.Dimensions,
			Normalization:   ns.Profile.Normalization,
			Role:            req.Role,
			Embeddings:      make([]localEmbedVectorEcho, len(req.Inputs)),
		}
		for i, input := range req.Inputs {
			resp.Embeddings[i] = localEmbedVectorEcho{
				Index:  i,
				SHA256: input.SHA256,
				Vector: localUnitVector(ns.Profile.Dimensions, i),
			}
		}
		if mutateEmbed != nil {
			mutateEmbed(&resp)
		}
		writeLocalJSON(t, w, resp)
	})
	return mux
}

func writeLocalJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("write json: %v", err)
	}
}

func localUnitVector(dims, offset int) []float32 {
	if dims <= 0 {
		panic(fmt.Sprintf("invalid dims %d", dims))
	}
	vector := make([]float32, dims)
	vector[offset%dims] = 1
	return vector
}

func errStringContains(err error, marker string) bool {
	if err == nil || marker == "" {
		return false
	}
	return strings.Contains(err.Error(), marker)
}
