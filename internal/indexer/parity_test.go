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
	const topK = 2

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
		vecs = append(vecs, denseRecord(page, "", vec))
		records = append(records, pages.VectorRecord{PageID: id, Vector: vec})
	}
	if err := idx.Apply(ctx, muts); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, vecs); err != nil {
		t.Fatal(err)
	}

	queries := [][]float64{
		normalize([]float64{1, 0.13, 0.07, 0.03, 0.02, 0.01, 0.005, 0.002}),
		normalize([]float64{0.11, 1, 0.07, 0.03, 0.02, 0.01, 0.005, 0.002}),
		normalize([]float64{1, 0.73, 0.21, 0.08, 0.04, 0.02, 0.01, 0.005}),
		normalize([]float64{0.9, 0.1, 0.03, 0.02, 0.01, 0.005, 0.002, 0.001}),
		normalize([]float64{0.01, 0.02, 0.03, 0.04, 0.05, 0.06, 0.07, 1}),
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

func TestDenseExtremeCanonicalizationOracleParity(t *testing.T) {
	tests := []struct {
		name          string
		stored        [][]float64
		query         []float64
		oracleVectors [][]float64
		oracleQuery   []float64
	}{
		{
			name: "huge finite",
			stored: [][]float64{
				{1e308, 1e307},
				{-1e308, 1e307},
				{1e307, 1e308},
			},
			query:         []float64{9e307, 1e307},
			oracleVectors: [][]float64{{1e308, 1e307}, {-1e308, 1e307}, {1e307, 1e308}},
			oracleQuery:   []float64{9e307, 1e307},
		},
		{
			name: "smallest subnormal directions",
			stored: [][]float64{
				{math.SmallestNonzeroFloat64, 0},
				{0, math.SmallestNonzeroFloat64},
				{-math.SmallestNonzeroFloat64, 0},
			},
			query:         []float64{math.SmallestNonzeroFloat64, 0},
			oracleVectors: [][]float64{{1, 0}, {0, 1}, {-1, 0}},
			oracleQuery:   []float64{1, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx, err := indexer.Open(filepath.Join(t.TempDir(), "index"), indexer.Options{DenseDimensions: 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = idx.Close() })
			ctx := context.Background()
			var mutations []pages.PageMutation
			var vectors []indexer.VectorRecord
			var oracleRecords []pages.VectorRecord
			for i := range tc.stored {
				id := pages.PageID(fmt.Sprintf("wp_%032x", i+1))
				page := samplePage(tc.name, string(id), pages.KindFailureEpisode, fmt.Sprintf("extreme doc %d", i), "main")
				page.Version = pages.PageVersion(i + 1)
				page.SourceSeqMin, page.SourceSeqMax = int64(i+1), int64(i+1)
				page.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", i+1))
				mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
				vectors = append(vectors, denseRecord(page, "", tc.stored[i]))
				oracleRecords = append(oracleRecords, pages.VectorRecord{PageID: id, Vector: tc.oracleVectors[i]})
			}
			if err := idx.Apply(ctx, mutations); err != nil {
				t.Fatal(err)
			}
			if err := idx.UpsertVectors(ctx, vectors); err != nil {
				t.Fatal(err)
			}
			oracle, err := pages.ExactCosineSearchContext(ctx, tc.oracleQuery, oracleRecords, len(oracleRecords))
			if err != nil {
				t.Fatal(err)
			}
			hits, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: tc.name, Limit: len(oracleRecords)}, tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != len(oracle) {
				t.Fatalf("hits=%d oracle=%d", len(hits), len(oracle))
			}
			for i := range oracle {
				if hits[i].Page.ID != oracle[i].PageID {
					t.Fatalf("rank %d dense=%s oracle=%s", i, hits[i].Page.ID, oracle[i].PageID)
				}
				if want := 1 - oracle[i].Score; math.Abs(hits[i].Rank-want) > 1e-5 {
					t.Fatalf("rank %d distance=%v want=%v", i, hits[i].Rank, want)
				}
			}
		})
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
