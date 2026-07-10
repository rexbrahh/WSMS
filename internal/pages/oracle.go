package pages

import (
	"fmt"
	"math"
	"sort"
)

// VectorRecord is one tiny-fixture document vector for the exact oracle.
type VectorRecord struct {
	PageID PageID   `json:"page_id"`
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
	if limit < 0 {
		return nil, fmt.Errorf("%w: negative result limit", ErrInvalidVector)
	}
	queryNorm, err := vectorNorm(query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	seen := make(map[PageID]bool, len(records))
	results := make([]ScoredPage, 0, len(records))
	for _, record := range records {
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
			dot += value * record.Vector[i]
		}
		results = append(results, ScoredPage{PageID: record.PageID, Score: dot / (queryNorm * norm)})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].PageID < results[j].PageID
		}
		return results[i].Score > results[j].Score
	})
	if limit == 0 || limit > len(results) {
		limit = len(results)
	}
	return append([]ScoredPage(nil), results[:limit]...), nil
}

func vectorNorm(vector []float64) (float64, error) {
	if len(vector) == 0 {
		return 0, fmt.Errorf("%w: empty vector", ErrInvalidVector)
	}
	var squared float64
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("%w: non-finite component", ErrInvalidVector)
		}
		squared += value * value
	}
	if squared == 0 || math.IsInf(squared, 0) {
		return 0, fmt.Errorf("%w: zero or overflowing norm", ErrInvalidVector)
	}
	return math.Sqrt(squared), nil
}
