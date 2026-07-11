package indexer

import (
	"math"
	"reflect"
	"testing"
)

func TestCosineDistanceStableForExtremeFiniteVectors(t *testing.T) {
	tests := []struct {
		name string
		a    []float64
		b    []float64
		want float64
	}{
		{name: "overflowing norm identical", a: []float64{math.MaxFloat64, math.MaxFloat64}, b: []float64{math.MaxFloat64, math.MaxFloat64}, want: 0},
		{name: "huge identical", a: []float64{1e308, 1e307}, b: []float64{1e308, 1e307}, want: 0},
		{name: "tiny identical", a: []float64{math.SmallestNonzeroFloat64, math.SmallestNonzeroFloat64}, b: []float64{math.SmallestNonzeroFloat64, math.SmallestNonzeroFloat64}, want: 0},
		{name: "huge opposite", a: []float64{1e308, 1e307}, b: []float64{-1e308, -1e307}, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineDistance(tc.a, tc.b)
			if !isFinite(got) || math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("cosineDistance(%v, %v)=%v want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCanonicalizeVectorIsIdempotentForExtremeDirections(t *testing.T) {
	vectors := [][]float64{
		{math.MaxFloat64, math.MaxFloat64},
		{1e308, -1e307, 1},
		{math.SmallestNonzeroFloat64, math.SmallestNonzeroFloat64, 0},
		{math.MaxFloat64, -math.SmallestNonzeroFloat64, 17},
		{0.1, 0.2, 0.3, 0.4},
	}
	for _, vector := range vectors {
		first, err := canonicalizeVector(vector, len(vector))
		if err != nil {
			t.Fatalf("first canonicalization %v: %v", vector, err)
		}
		second, err := canonicalizeVector(first, len(first))
		if err != nil {
			t.Fatalf("second canonicalization %v: %v", first, err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("canonicalization drift: first=%v second=%v", first, second)
		}
	}
}
