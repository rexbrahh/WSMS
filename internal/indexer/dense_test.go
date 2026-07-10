package indexer_test

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"wsms/internal/indexer"
	"wsms/internal/pages"
)

func TestDenseUnavailableByDefault(t *testing.T) {
	idx := openTestIndex(t)
	_, err := idx.SearchDense(context.Background(), indexer.SearchQuery{SessionID: "s"}, []float64{1})
	if !errors.Is(err, indexer.ErrDenseUnavailable) {
		t.Fatalf("err=%v", err)
	}
	if idx.DenseEnabled() {
		t.Fatal("dense should be off")
	}
}

func TestDenseKNNAndFilters(t *testing.T) {
	idx := openDenseIndex(t, 3)
	ctx := context.Background()
	pagesA := []pages.WarmPage{
		samplePage("sess-a", "wp_"+strings.Repeat("a", 32), pages.KindFailureEpisode, "stream blocked alpha", "main"),
		samplePage("sess-a", "wp_"+strings.Repeat("b", 32), pages.KindFailureEpisode, "stream blocked beta", "main"),
		samplePage("sess-a", "wp_"+strings.Repeat("c", 32), pages.KindConstraint, "do not rewrite", ""),
		samplePage("sess-b", "wp_"+strings.Repeat("d", 32), pages.KindFailureEpisode, "other session", "main"),
	}
	// Fix versions and digests uniqueness for multi-page apply
	for i := range pagesA {
		pagesA[i].Version = pages.PageVersion(i + 1)
		pagesA[i].SourceDigest = pages.SourceDigest(strings.Repeat(string('a'+byte(i)), 64))
		pagesA[i].SourceSeqMin = int64(i + 1)
		pagesA[i].SourceSeqMax = int64(i + 1)
	}
	var muts []pages.PageMutation
	for _, p := range pagesA {
		muts = append(muts, pages.PageMutation{Op: pages.MutationUpsert, Page: p})
	}
	if err := idx.Apply(ctx, muts); err != nil {
		t.Fatal(err)
	}
	vecs := []indexer.VectorRecord{
		{PageID: pagesA[0].ID, SessionID: "sess-a", Vector: []float64{1, 0, 0}},
		{PageID: pagesA[1].ID, SessionID: "sess-a", Vector: []float64{0.9, 0.1, 0}},
		{PageID: pagesA[2].ID, SessionID: "sess-a", Vector: []float64{0, 1, 0}},
		{PageID: pagesA[3].ID, SessionID: "sess-b", Vector: []float64{1, 0, 0}},
	}
	if err := idx.UpsertVectors(ctx, vecs); err != nil {
		t.Fatal(err)
	}

	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{
		SessionID: "sess-a", Limit: 5, Kinds: []pages.PageKind{pages.KindFailureEpisode},
	}, []float64{1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits=%d want 2 failure_episode: %#v", len(hits), hits)
	}
	if hits[0].Page.ID != pagesA[0].ID {
		t.Fatalf("nearest=%s want %s", hits[0].Page.ID, pagesA[0].ID)
	}
	if hits[0].Rank > hits[1].Rank {
		t.Fatalf("rank order inverted: %v then %v", hits[0].Rank, hits[1].Rank)
	}
	if !strings.Contains(hits[0].Explanation, "vec0") {
		t.Fatalf("explanation=%q", hits[0].Explanation)
	}

	// Cross-session isolation via partition key.
	hits, err = idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "sess-b", Limit: 5}, []float64{1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.SessionID != "sess-b" {
		t.Fatalf("session isolation: %#v", hits)
	}
}

func TestDenseWrongDimensionAndNonFinite(t *testing.T) {
	idx := openDenseIndex(t, 3)
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("e", 32), pages.KindFailureEpisode, "x", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	err := idx.UpsertVectors(ctx, []indexer.VectorRecord{{
		PageID: page.ID, SessionID: "s", Vector: []float64{1, 0},
	}})
	if !errors.Is(err, pages.ErrInvalidVector) {
		t.Fatalf("dim err=%v", err)
	}
	err = idx.UpsertVectors(ctx, []indexer.VectorRecord{{
		PageID: page.ID, SessionID: "s", Vector: []float64{math.NaN(), 0, 0},
	}})
	if !errors.Is(err, pages.ErrInvalidVector) {
		t.Fatalf("nan err=%v", err)
	}
}

func TestDenseInvalidateRemovesVector(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("f", 32), pages.KindFailureEpisode, "blocked", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{{
		PageID: page.ID, SessionID: "s", Vector: []float64{1, 0},
	}}); err != nil {
		t.Fatal(err)
	}
	inv := page
	inv.Status = pages.StatusInvalidated
	inv.Version++
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationInvalidate, Page: inv}}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", Limit: 5}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected empty after invalidate: %#v", hits)
	}
}

func TestDenseRestartPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	ctx := context.Background()
	idx, err := indexer.Open(dir, indexer.Options{DenseDimensions: 2})
	if err != nil {
		t.Fatal(err)
	}
	page := samplePage("s", "wp_"+strings.Repeat("1", 32), pages.KindFailureEpisode, "persist", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{{
		PageID: page.ID, SessionID: "s", Vector: []float64{0, 1},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen without opts should restore dense from meta.
	idx2, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx2.Close() })
	if !idx2.DenseEnabled() || idx2.DenseDimensions() != 2 {
		t.Fatalf("dense not restored: enabled=%v dims=%d", idx2.DenseEnabled(), idx2.DenseDimensions())
	}
	hits, err := idx2.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", Limit: 3}, []float64{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != page.ID {
		t.Fatalf("restart hits=%#v", hits)
	}
}

func TestDenseConcurrentSearch(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("2", 32), pages.KindFailureEpisode, "concurrent stream blocked", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{{
		PageID: page.ID, SessionID: "s", Vector: []float64{1, 0},
	}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", Limit: 3}, []float64{1, 0})
			errs <- err
			_, err = idx.SearchLexical(ctx, indexer.SearchQuery{SessionID: "s", Text: "stream blocked", Limit: 3})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDenseCancel(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", Limit: 3}, []float64{1, 0})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestDenseBatchReplace(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("3", 32), pages.KindFailureEpisode, "replace", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{{
		PageID: page.ID, SessionID: "s", Vector: []float64{1, 0},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{{
		PageID: page.ID, SessionID: "s", Vector: []float64{0, 1},
	}}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", Limit: 1}, []float64{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Rank > 1e-5 {
		t.Fatalf("after replace: %#v", hits)
	}
}

func openDenseIndex(t *testing.T, dims int) *indexer.Index {
	t.Helper()
	idx, err := indexer.Open(filepath.Join(t.TempDir(), "index"), indexer.Options{DenseDimensions: dims})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := idx.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	h, err := idx.Health(context.Background())
	if err != nil || !h.DenseEnabled || h.DenseDimensions != dims {
		t.Fatalf("health=%#v err=%v", h, err)
	}
	return idx
}
