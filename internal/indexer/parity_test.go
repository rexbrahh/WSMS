package indexer_test

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"wsms/internal/indexer"
	"wsms/internal/pages"
)

// TestDenseExactOracleParity compares vec0 cosine KNN against the brute-force
// exact oracle using well-separated synthetic unit vectors.
func TestDenseExactOracleParity(t *testing.T) {
	const dim = 8
	const topK = 3

	// One-hot axes plus a few unambiguous combinations so ordering is stable.
	vectors := [][]float64{
		{1, 0, 0, 0, 0, 0, 0, 0},
		{0, 1, 0, 0, 0, 0, 0, 0},
		{0, 0, 1, 0, 0, 0, 0, 0},
		{0, 0, 0, 1, 0, 0, 0, 0},
		{0, 0, 0, 0, 1, 0, 0, 0},
		{0, 0, 0, 0, 0, 1, 0, 0},
		{0, 0, 0, 0, 0, 0, 1, 0},
		{0, 0, 0, 0, 0, 0, 0, 1},
		normalize([]float64{1, 1, 0, 0, 0, 0, 0, 0}),
		normalize([]float64{1, 0, 1, 0, 0, 0, 0, 0}),
		normalize([]float64{0, 1, 1, 0, 0, 0, 0, 0}),
		normalize([]float64{1, 1, 1, 0, 0, 0, 0, 0}),
		normalize([]float64{0, 0, 0, 1, 1, 0, 0, 0}),
		normalize([]float64{0.2, 0.8, 0, 0, 0, 0, 0, 0}),
		normalize([]float64{0.8, 0.2, 0, 0, 0, 0, 0, 0}),
		normalize([]float64{0, 0, 0.5, 0.5, 0, 0, 0, 0}),
	}

	idx, err := indexer.Open(filepath.Join(t.TempDir(), "index"), indexer.Options{DenseDimensions: dim})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	ctx := context.Background()

	records := make([]pages.VectorRecord, 0, len(vectors))
	var muts []pages.PageMutation
	var vecs []indexer.VectorRecord
	for i, vec := range vectors {
		id := pages.PageID(fmt.Sprintf("wp_%032x", i+1))
		page := samplePage("parity", string(id), pages.KindFailureEpisode, fmt.Sprintf("doc %d", i), "main")
		page.ID = id
		page.Version = pages.PageVersion(i + 1)
		page.SourceSeqMin = int64(i + 1)
		page.SourceSeqMax = int64(i + 1)
		page.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", i+1))
		muts = append(muts, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
		vecs = append(vecs, indexer.VectorRecord{PageID: id, SessionID: "parity", Vector: vec})
		records = append(records, pages.VectorRecord{PageID: id, Vector: vec})
	}
	if err := idx.Apply(ctx, muts); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, vecs); err != nil {
		t.Fatal(err)
	}

	queries := [][]float64{
		{1, 0, 0, 0, 0, 0, 0, 0},
		{0, 1, 0, 0, 0, 0, 0, 0},
		normalize([]float64{1, 1, 0, 0, 0, 0, 0, 0}),
		normalize([]float64{0.9, 0.1, 0, 0, 0, 0, 0, 0}),
		{0, 0, 0, 0, 0, 0, 0, 1},
	}
	for qi, query := range queries {
		oracle, err := pages.ExactCosineSearchContext(ctx, query, records, topK)
		if err != nil {
			t.Fatalf("query %d oracle: %v", qi, err)
		}
		hits, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "parity", Limit: topK}, query)
		if err != nil {
			t.Fatalf("query %d dense: %v", qi, err)
		}
		if len(hits) != len(oracle) {
			t.Fatalf("query %d len dense=%d oracle=%d", qi, len(hits), len(oracle))
		}
		for i := range oracle {
			if hits[i].Page.ID != oracle[i].PageID {
				t.Fatalf("query %d rank %d: dense=%s oracle=%s\ndense=%v\noracle=%v",
					qi, i, hits[i].Page.ID, oracle[i].PageID, idsOf(hits), oracleIDs(oracle))
			}
			wantDist := 1 - oracle[i].Score
			if math.Abs(hits[i].Rank-wantDist) > 1e-3 {
				t.Fatalf("query %d rank %d distance dense=%v want≈%v (sim=%v)",
					qi, i, hits[i].Rank, wantDist, oracle[i].Score)
			}
		}
	}
}

func normalize(v []float64) []float64 {
	var n float64
	for _, x := range v {
		n += x * x
	}
	n = math.Sqrt(n)
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x / n
	}
	return out
}

func idsOf(hits []indexer.Candidate) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = string(h.Page.ID)
	}
	return out
}

func oracleIDs(hits []pages.ScoredPage) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = string(h.PageID)
	}
	return out
}
