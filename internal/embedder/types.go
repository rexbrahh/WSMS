package embedder

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// Embedder is the Phase 7D ABI. Document and query embedding are deliberately
// separate so role inversions cannot silently contaminate dense search.
type Embedder interface {
	EmbedDocuments(ctx context.Context, texts []string) (EmbeddingBatch, error)
	EmbedQuery(ctx context.Context, text string) (Embedding, error)
	Namespace() EmbeddingNamespace
}

// Role identifies whether a vector came from document or query preprocessing.
type Role string

const (
	RoleDocument Role = "document"
	RoleQuery    Role = "query"
)

const normalizationTolerance = 1e-3

var (
	// ErrInvalidEmbedding reports malformed, mismatched, or role-inverted
	// vectors. Vectors are never truncated, padded, or reinterpreted silently.
	ErrInvalidEmbedding = errors.New("invalid embedding")
)

// Embedding is one validated vector plus its namespace and role.
type Embedding struct {
	Namespace     EmbeddingNamespace
	Role          Role
	Vector        []float32
	CanonicalText string
	CacheKey      string
}

// EmbeddingBatch is an ordered result for EmbedDocuments.
type EmbeddingBatch struct {
	Namespace      EmbeddingNamespace
	Role           Role
	Embeddings     []Embedding
	CanonicalTexts []string
	CacheHits      int
	CacheMisses    int
}

// Validate checks one embedding against the expected namespace and role.
func (e Embedding) Validate(expected EmbeddingNamespace, role Role) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if e.Namespace.ID != expected.ID {
		return fmt.Errorf("%w: namespace mismatch got %q want %q", ErrInvalidEmbedding, e.Namespace.ID, expected.ID)
	}
	if e.Role != role {
		return fmt.Errorf("%w: role mismatch got %q want %q", ErrInvalidEmbedding, e.Role, role)
	}
	if err := validateVector(e.Vector, expected.Profile.Dimensions, expected.Profile.Normalization); err != nil {
		return err
	}
	return nil
}

// Validate checks batch shape, namespace, role, vector dimensions, and order.
func (b EmbeddingBatch) Validate(expected EmbeddingNamespace, count int) error {
	if b.Namespace.ID != expected.ID {
		return fmt.Errorf("%w: batch namespace mismatch got %q want %q", ErrInvalidEmbedding, b.Namespace.ID, expected.ID)
	}
	if b.Role != RoleDocument {
		return fmt.Errorf("%w: batch role mismatch got %q want %q", ErrInvalidEmbedding, b.Role, RoleDocument)
	}
	if len(b.Embeddings) != count {
		return fmt.Errorf("%w: batch count %d != %d", ErrInvalidEmbedding, len(b.Embeddings), count)
	}
	if len(b.CanonicalTexts) != count {
		return fmt.Errorf("%w: canonical text count %d != %d", ErrInvalidEmbedding, len(b.CanonicalTexts), count)
	}
	for i, embedding := range b.Embeddings {
		if err := embedding.Validate(expected, RoleDocument); err != nil {
			return fmt.Errorf("embedding %d: %w", i, err)
		}
		if embedding.CanonicalText != b.CanonicalTexts[i] {
			return fmt.Errorf("%w: canonical text mismatch at %d", ErrInvalidEmbedding, i)
		}
	}
	return nil
}

func validateVector(vector []float32, dims int, normalization Normalization) error {
	if dims <= 0 {
		return fmt.Errorf("%w: invalid dimensions %d", ErrInvalidEmbedding, dims)
	}
	if len(vector) != dims {
		return fmt.Errorf("%w: vector dimension %d != %d", ErrInvalidEmbedding, len(vector), dims)
	}
	var norm float64
	for _, v := range vector {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("%w: non-finite component", ErrInvalidEmbedding)
		}
		norm += f * f
	}
	if norm == 0 || math.IsInf(norm, 0) || math.IsNaN(norm) {
		return fmt.Errorf("%w: zero or overflowing norm", ErrInvalidEmbedding)
	}
	switch normalization {
	case NormalizationL2:
		length := math.Sqrt(norm)
		if math.Abs(length-1) > normalizationTolerance {
			return fmt.Errorf("%w: vector is not l2-normalized", ErrInvalidEmbedding)
		}
	default:
		return fmt.Errorf("%w: unsupported normalization %q", ErrInvalidEmbedding, normalization)
	}
	return nil
}

func copyVector(vector []float32) []float32 {
	if vector == nil {
		return nil
	}
	out := make([]float32, len(vector))
	copy(out, vector)
	return out
}
