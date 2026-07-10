package pages

import (
	"context"
	"fmt"
	"math"
	"sort"
)

const (
	// MaxVectorDimensions and MaxOracleRecords keep the exact reference backend
	// intentionally tiny and bound adversarial fixture cost.
	MaxVectorDimensions = 4096
	MaxOracleRecords    = 4096
)

// VectorRecord is one tiny-fixture document vector for the exact oracle.
type VectorRecord struct {
	PageID PageID    `json:"page_id"`
	Vector []float64 `json:"vector"`
}

// ScoredPage is one exact cosine result.
type ScoredPage struct {
	PageID PageID  `json:"page_id"`
	Score  float64 `json:"score"`
}

// ExactCosineSearch computes brute-force cosine similarity. It intentionally
// has no ANN behavior and is suitable only for tiny fixtures and backend
// correctness comparisons.
func ExactCosineSearch(query []float64, records []VectorRecord, limit int) ([]ScoredPage, error) {
	return ExactCosineSearchContext(context.Background(), query, records, limit)
}

// ExactCosineSearchContext is the cancellation-aware exact oracle. A zero
// limit deliberately returns zero results after validating the input; callers
// must opt into every candidate they request.
func ExactCosineSearchContext(ctx context.Context, query []float64, records []VectorRecord, limit int) ([]ScoredPage, error) {
	if limit < 0 {
		return nil, fmt.Errorf("%w: negative result limit", ErrInvalidVector)
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if len(query) > MaxVectorDimensions {
		return nil, fmt.Errorf("%w: query dimension %d exceeds %d", ErrInvalidVector, len(query), MaxVectorDimensions)
	}
	if len(records) > MaxOracleRecords {
		return nil, fmt.Errorf("%w: record count %d exceeds %d", ErrInvalidVector, len(records), MaxOracleRecords)
	}
	queryNorm, err := vectorNorm(query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	seen := make(map[PageID]bool, len(records))
	results := make([]ScoredPage, 0, len(records))
	for _, record := range records {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		if record.PageID == "" {
			return nil, fmt.Errorf("%w: empty page id", ErrInvalidVector)
		}
		if seen[record.PageID] {
			return nil, fmt.Errorf("%w: duplicate page id %s", ErrInvalidVector, record.PageID)
		}
		seen[record.PageID] = true
		if len(record.Vector) != len(query) {
			return nil, fmt.Errorf("%w: page %s dimension %d != query dimension %d", ErrInvalidVector, record.PageID, len(record.Vector), len(query))
		}
		norm, err := vectorNorm(record.Vector)
		if err != nil {
			return nil, fmt.Errorf("page %s: %w", record.PageID, err)
		}
		var dot float64
		for i, value := range query {
			dot += (value / queryNorm) * (record.Vector[i] / norm)
			if math.IsNaN(dot) || math.IsInf(dot, 0) {
				return nil, fmt.Errorf("%w: page %s cosine overflow", ErrInvalidVector, record.PageID)
			}
		}
		dot = max(-1, min(1, dot))
		results = append(results, ScoredPage{PageID: record.PageID, Score: dot})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].PageID < results[j].PageID
		}
		return results[i].Score > results[j].Score
	})
	if limit > len(results) {
		limit = len(results)
	}
	return append([]ScoredPage(nil), results[:limit]...), nil
}

func vectorNorm(vector []float64) (float64, error) {
	if len(vector) == 0 {
		return 0, fmt.Errorf("%w: empty vector", ErrInvalidVector)
	}
	var norm float64
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("%w: non-finite component", ErrInvalidVector)
		}
		norm = math.Hypot(norm, value)
	}
	if norm == 0 || math.IsInf(norm, 0) {
		return 0, fmt.Errorf("%w: zero or overflowing norm", ErrInvalidVector)
	}
	return norm, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("nil context")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
