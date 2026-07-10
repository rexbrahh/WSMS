// Package embedder defines the Phase 7D private embedding ABI.
//
// Embeddings are L3 working-set metadata. They are rebuildable, namespaced, and
// never authoritative evidence.
package embedder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DistanceMetric is the vector distance contract for one namespace.
type DistanceMetric string

const (
	MetricCosine DistanceMetric = "cosine"
)

// Normalization is the vector normalization contract for one namespace.
type Normalization string

const (
	NormalizationL2 Normalization = "l2"
)

const (
	// MaxInstructionBytes bounds namespace identity fields that are copied into
	// query payloads.
	MaxInstructionBytes = 2048
	// MaxTemplateBytes bounds namespace identity fields that are copied into
	// document payloads.
	MaxTemplateBytes = 2048
)

var (
	// ErrInvalidNamespace reports an incomplete or non-canonical embedding
	// namespace. Namespace mismatches must fail visibly rather than mixing
	// vectors across model/profile identities.
	ErrInvalidNamespace = errors.New("invalid embedding namespace")
)

// NamespaceProfile is the complete ABI identity for stored vectors.
//
// A change to any field produces a new EmbeddingNamespace and therefore a new
// index generation. Keep this struct field-only and map-free so its JSON form is
// canonical under encoding/json's struct ordering.
type NamespaceProfile struct {
	Provider          string         `json:"provider"`
	ModelRepository   string         `json:"model_repository"`
	ExactRevision     string         `json:"exact_revision"`
	Dimensions        int            `json:"dimensions"`
	DistanceMetric    DistanceMetric `json:"distance_metric"`
	Normalization     Normalization  `json:"normalization"`
	QueryInstruction  string         `json:"query_instruction"`
	DocumentTemplate  string         `json:"document_template"`
	TokenizerRevision string         `json:"tokenizer_revision"`
	PageSchemaVersion string         `json:"page_schema_version"`
	RedactionVersion  string         `json:"redaction_version"`
}

// EmbeddingNamespace is the canonical identity of an embedding profile.
type EmbeddingNamespace struct {
	ID      string           `json:"id"`
	Profile NamespaceProfile `json:"profile"`
}

// NewNamespace validates profile and returns its canonical digest identity.
func NewNamespace(profile NamespaceProfile) (EmbeddingNamespace, error) {
	if err := validateProfile(profile); err != nil {
		return EmbeddingNamespace{}, err
	}
	canonical, err := canonicalProfileJSON(profile)
	if err != nil {
		return EmbeddingNamespace{}, err
	}
	sum := sha256.Sum256(canonical)
	return EmbeddingNamespace{
		ID:      "emb_" + hex.EncodeToString(sum[:]),
		Profile: profile,
	}, nil
}

// MustNamespace is a convenience for tests and static package configuration.
func MustNamespace(profile NamespaceProfile) EmbeddingNamespace {
	ns, err := NewNamespace(profile)
	if err != nil {
		panic(err)
	}
	return ns
}

// Qwen3Embedding06BProfile returns the reference local profile shape.
//
// Callers must provide exact revisions from the locally installed model and
// tokenizer. Empty revision values are intentionally invalid once passed to
// NewNamespace; this helper does not guess mutable tags.
func Qwen3Embedding06BProfile(modelRevision, tokenizerRevision, pageSchemaVersion, redactionVersion string) NamespaceProfile {
	return NamespaceProfile{
		Provider:          "local",
		ModelRepository:   "Qwen/Qwen3-Embedding-0.6B",
		ExactRevision:     modelRevision,
		Dimensions:        1024,
		DistanceMetric:    MetricCosine,
		Normalization:     NormalizationL2,
		QueryInstruction:  "Represent this query for retrieving relevant WSMS working-state pages.",
		DocumentTemplate:  "{{.SearchText}}",
		TokenizerRevision: tokenizerRevision,
		PageSchemaVersion: pageSchemaVersion,
		RedactionVersion:  redactionVersion,
	}
}

// String returns the canonical namespace digest.
func (ns EmbeddingNamespace) String() string {
	return ns.ID
}

// CanonicalProfile returns the stable JSON bytes used to compute the namespace
// digest.
func (ns EmbeddingNamespace) CanonicalProfile() ([]byte, error) {
	if err := ns.Validate(); err != nil {
		return nil, err
	}
	return canonicalProfileJSON(ns.Profile)
}

// Validate checks that the namespace is complete and that ID matches Profile.
func (ns EmbeddingNamespace) Validate() error {
	if ns.ID == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidNamespace)
	}
	want, err := NewNamespace(ns.Profile)
	if err != nil {
		return err
	}
	if ns.ID != want.ID {
		return fmt.Errorf("%w: id %q does not match profile digest %q", ErrInvalidNamespace, ns.ID, want.ID)
	}
	return nil
}

func validateProfile(profile NamespaceProfile) error {
	required := map[string]string{
		"provider":            profile.Provider,
		"model_repository":    profile.ModelRepository,
		"exact_revision":      profile.ExactRevision,
		"query_instruction":   profile.QueryInstruction,
		"document_template":   profile.DocumentTemplate,
		"tokenizer_revision":  profile.TokenizerRevision,
		"page_schema_version": profile.PageSchemaVersion,
		"redaction_version":   profile.RedactionVersion,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidNamespace, name)
		}
	}
	if profile.Dimensions <= 0 {
		return fmt.Errorf("%w: dimensions must be positive", ErrInvalidNamespace)
	}
	if len(profile.QueryInstruction) > MaxInstructionBytes {
		return fmt.Errorf("%w: query instruction exceeds %d bytes", ErrInvalidNamespace, MaxInstructionBytes)
	}
	if len(profile.DocumentTemplate) > MaxTemplateBytes {
		return fmt.Errorf("%w: document template exceeds %d bytes", ErrInvalidNamespace, MaxTemplateBytes)
	}
	switch profile.DistanceMetric {
	case MetricCosine:
	default:
		return fmt.Errorf("%w: unsupported distance metric %q", ErrInvalidNamespace, profile.DistanceMetric)
	}
	switch profile.Normalization {
	case NormalizationL2:
	default:
		return fmt.Errorf("%w: unsupported normalization %q", ErrInvalidNamespace, profile.Normalization)
	}
	return nil
}

func canonicalProfileJSON(profile NamespaceProfile) ([]byte, error) {
	b, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical profile: %v", ErrInvalidNamespace, err)
	}
	return b, nil
}
