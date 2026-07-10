package indexer_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func TestDenseVectorIsDroppedWhenPageVersionChanges(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("8", 32), pages.KindFailureEpisode, "version one", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{{PageID: page.ID, SessionID: "s", Vector: []float64{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	page.Version = 2
	page.SourceSeqMax = 2
	page.SourceDigest = pages.SourceDigest(strings.Repeat("b", 64))
	page.SearchText = "kind=failure_episode version two"
	page.Summary = "version two"
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", Limit: 3}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("stale vector ranked updated page: %#v", hits)
	}
}

func TestDenseNamespaceIsolation(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	pageA := samplePage("s", "wp_"+strings.Repeat("9", 32), pages.KindFailureEpisode, "namespace a", "main")
	pageB := samplePage("s", "wp_"+strings.Repeat("a", 32), pages.KindFailureEpisode, "namespace b", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: pageA}, {Op: pages.MutationUpsert, Page: pageB}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{
		{PageID: pageA.ID, SessionID: "s", EmbeddingNamespace: "embed/a", Vector: []float64{1, 0}},
		{PageID: pageB.ID, SessionID: "s", EmbeddingNamespace: "embed/b", Vector: []float64{1, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{
		SessionID: "s", EmbeddingNamespace: "embed/a", Limit: 3,
	}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != pageA.ID {
		t.Fatalf("namespace hits=%#v", hits)
	}
}

func TestDenseNamespacePartitionPreventsCandidateStarvation(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	target := samplePage("s", "wp_"+strings.Repeat("b", 32), pages.KindFailureEpisode, "correct namespace but farther vector", "main")
	target.SourceSeqMin = 1
	target.SourceSeqMax = 1
	target.SourceDigest = pages.SourceDigest(strings.Repeat("b", 64))
	mutations := []pages.PageMutation{{Op: pages.MutationUpsert, Page: target}}
	vectors := []indexer.VectorRecord{{
		PageID: target.ID, SessionID: "s", EmbeddingNamespace: "embed/needle", Vector: []float64{0, 1},
	}}
	for i := 0; i < 205; i++ {
		id := pages.PageID(fmt.Sprintf("wp_%032x", i+1))
		page := samplePage("s", string(id), pages.KindFailureEpisode, fmt.Sprintf("wrong namespace chaff %03d", i), "main")
		page.Version = pages.PageVersion(i + 2)
		page.SourceSeqMin = int64(i + 2)
		page.SourceSeqMax = int64(i + 2)
		page.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", i+1))
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
		vectors = append(vectors, indexer.VectorRecord{
			PageID: page.ID, SessionID: "s", EmbeddingNamespace: "embed/chaff", Vector: []float64{1, 0},
		})
	}
	if err := idx.Apply(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, vectors); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{
		SessionID: "s", EmbeddingNamespace: "embed/needle", Limit: 1,
	}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != target.ID {
		t.Fatalf("namespace candidate starvation: hits=%#v want target %s", hits, target.ID)
	}
}

func TestDenseLegacyProjectionRecreatesWithNamespacePartition(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	ctx := context.Background()
	idx, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "warm.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO index_meta(key, value) VALUES
  ('dense_enabled', '1'),
  ('dense_dimensions', '2'),
  ('dense_metric', 'cosine');
CREATE TABLE warm_page_vec_map (
  page_id    TEXT PRIMARY KEY NOT NULL,
  rowid      INTEGER NOT NULL UNIQUE,
  session_id TEXT NOT NULL
);
CREATE VIRTUAL TABLE warm_pages_vec USING vec0(
  session_id TEXT PARTITION KEY,
  embedding float[2] distance_metric=cosine
);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	idx, err = indexer.Open(dir, indexer.Options{DenseDimensions: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	page := samplePage("s", "wp_"+strings.Repeat("e", 32), pages.KindFailureEpisode, "migrated dense namespace", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{{
		PageID: page.ID, SessionID: "s", EmbeddingNamespace: "embed/current", Vector: []float64{1, 0},
	}}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{
		SessionID: "s", EmbeddingNamespace: "embed/current", Limit: 1,
	}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != page.ID {
		t.Fatalf("migrated dense hits=%#v", hits)
	}
}

func TestDenseRebuildPreservesOnlyCompatibleVectors(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	pageA := samplePage("s", "wp_"+strings.Repeat("c", 32), pages.KindFailureEpisode, "stable page", "main")
	pageB := samplePage("s", "wp_"+strings.Repeat("d", 32), pages.KindFailureEpisode, "changing page", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: pageA}, {Op: pages.MutationUpsert, Page: pageB}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{
		{PageID: pageA.ID, SessionID: "s", Vector: []float64{1, 0}},
		{PageID: pageB.ID, SessionID: "s", Vector: []float64{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}
	changed := pageB
	changed.Version = 2
	changed.SourceSeqMax = 2
	changed.SourceDigest = pages.SourceDigest(strings.Repeat("e", 64))
	changed.SearchText = "kind=failure_episode changed page"
	changed.Summary = "changed page"
	if err := idx.Rebuild(ctx, indexer.MutationList{
		{Op: pages.MutationUpsert, Page: pageA},
		{Op: pages.MutationUpsert, Page: changed},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", Limit: 5}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != pageA.ID {
		t.Fatalf("rebuilt dense hits=%#v", hits)
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
