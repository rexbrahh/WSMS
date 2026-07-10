package pages

import (
	"errors"
	"math"
	"testing"
)

func TestExactCosineSearchStableTieBreakAndLimit(t *testing.T) {
	results, err := ExactCosineSearch([]float64{1, 0}, []VectorRecord{
		{PageID: "wp_b", Vector: []float64{1, 0}},
		{PageID: "wp_c", Vector: []float64{0, 1}},
		{PageID: "wp_a", Vector: []float64{2, 0}},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].PageID != "wp_a" || results[1].PageID != "wp_b" {
		t.Fatalf("stable results=%#v", results)
	}
	if math.Abs(results[0].Score-1) > 1e-12 {
		t.Fatalf("score=%v, want 1", results[0].Score)
	}
}

func TestExactCosineSearchRejectsMalformedVectors(t *testing.T) {
	tests := []struct {
		name    string
		query   []float64
		records []VectorRecord
		limit   int
	}{
		{name: "empty query"},
		{name: "zero query", query: []float64{0, 0}},
		{name: "nonfinite", query: []float64{math.NaN()}},
		{name: "dimension", query: []float64{1}, records: []VectorRecord{{PageID: "p", Vector: []float64{1, 2}}}},
		{name: "duplicate", query: []float64{1}, records: []VectorRecord{{PageID: "p", Vector: []float64{1}}, {PageID: "p", Vector: []float64{1}}}},
		{name: "negative limit", query: []float64{1}, limit: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExactCosineSearch(tc.query, tc.records, tc.limit)
			if !errors.Is(err, ErrInvalidVector) {
				t.Fatalf("error=%v, want ErrInvalidVector", err)
			}
		})
	}
}
