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
		denseRecord(pagesA[0], "", []float64{1, 0, 0}),
		denseRecord(pagesA[1], "", []float64{0.9, 0.1, 0}),
		denseRecord(pagesA[2], "", []float64{0, 1, 0}),
		denseRecord(pagesA[3], "", []float64{1, 0, 0}),
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
	record := denseRecord(page, "", []float64{1, 0})
	err := idx.UpsertVectors(ctx, []indexer.VectorRecord{record})
	if !errors.Is(err, pages.ErrInvalidVector) {
		t.Fatalf("dim err=%v", err)
	}
	record.Vector = []float64{math.NaN(), 0, 0}
	err = idx.UpsertVectors(ctx, []indexer.VectorRecord{record})
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
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, "", []float64{1, 0})}); err != nil {
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
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, "", []float64{0, 1})}); err != nil {
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
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, "", []float64{1, 0})}); err != nil {
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
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, "", []float64{1, 0})}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, "", []float64{0, 1})}); err != nil {
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
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, "", []float64{1, 0})}); err != nil {
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
		denseRecord(pageA, "embed/a", []float64{1, 0}),
		denseRecord(pageB, "embed/b", []float64{1, 0}),
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

func TestConfiguredDenseGenerationRejectsForeignNamespaceEverywhere(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	const namespace = "embed/configured"
	idx, err := indexer.Open(dir, indexer.Options{DenseDimensions: 2, EmbeddingNamespace: namespace})
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("4", 32), pages.KindConstraint, "configured namespace", "")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, namespace, []float64{1, 0})}); err != nil {
		t.Fatal(err)
	}
	foreignRecord := denseRecord(page, "embed/foreign", []float64{0, 1})
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{foreignRecord}); !errors.Is(err, indexer.ErrEmbeddingNamespaceMismatch) {
		t.Fatalf("foreign upsert error=%v want namespace mismatch", err)
	}
	foreignSuppression := denseSuppression(page, "embed/foreign", indexer.EmbeddingStatusAdmissionDenied)
	if err := idx.SuppressVectors(ctx, []indexer.VectorSuppression{foreignSuppression}); !errors.Is(err, indexer.ErrEmbeddingNamespaceMismatch) {
		t.Fatalf("foreign suppression error=%v want namespace mismatch", err)
	}
	if _, err := idx.MissingVectorPages(ctx, "s", "embed/foreign", 10); !errors.Is(err, indexer.ErrEmbeddingNamespaceMismatch) {
		t.Fatalf("foreign backlog error=%v want namespace mismatch", err)
	}
	if _, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", EmbeddingNamespace: "embed/foreign"}, []float64{1, 0}); !errors.Is(err, indexer.ErrEmbeddingNamespaceMismatch) {
		t.Fatalf("foreign search error=%v want namespace mismatch", err)
	}
	if _, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s"}, []float64{1, 0}); !errors.Is(err, indexer.ErrEmbeddingNamespaceMismatch) {
		t.Fatalf("implicit manual namespace error=%v want namespace mismatch", err)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", EmbeddingNamespace: namespace, Limit: 1}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != page.ID {
		t.Fatalf("foreign operations mutated configured projection: %#v", hits)
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
	vectors := []indexer.VectorRecord{denseRecord(target, "embed/needle", []float64{0, 1})}
	for i := 0; i < 205; i++ {
		id := pages.PageID(fmt.Sprintf("wp_%032x", i+1))
		page := samplePage("s", string(id), pages.KindFailureEpisode, fmt.Sprintf("wrong namespace chaff %03d", i), "main")
		page.Version = pages.PageVersion(i + 2)
		page.SourceSeqMin = int64(i + 2)
		page.SourceSeqMax = int64(i + 2)
		page.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", i+1))
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
		vectors = append(vectors, denseRecord(page, "embed/chaff", []float64{1, 0}))
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
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, "embed/current", []float64{1, 0})}); err != nil {
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
		denseRecord(pageA, "", []float64{1, 0}),
		denseRecord(pageB, "", []float64{0, 1}),
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

func TestDenseRebuildSkipsForeignNamespaceRows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	const namespace = "embed/rebuild-current"
	idx, err := indexer.Open(dir, indexer.Options{DenseDimensions: 2, EmbeddingNamespace: namespace})
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("5", 32), pages.KindFailureEpisode, "foreign rebuild row", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{denseRecord(page, namespace, []float64{1, 0})}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "warm.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE warm_page_vec_map SET embedding_namespace = 'embed/foreign' WHERE page_id = ?`, string(page.ID)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Rebuild(ctx, indexer.MutationList{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatalf("foreign derivative row poisoned rebuild: %v", err)
	}
	missing, err := idx.MissingVectorPages(ctx, "s", namespace, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].ID != page.ID {
		t.Fatalf("foreign vector row was copied into configured generation: %#v", missing)
	}
}

func TestDenseRebuildPreservesManualMultiNamespaceRows(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	pageA := samplePage("s", "wp_"+strings.Repeat("6", 32), pages.KindFailureEpisode, "manual namespace a", "main")
	pageB := samplePage("s", "wp_"+strings.Repeat("7", 32), pages.KindFailureEpisode, "manual namespace b", "main")
	pageB.Version = 2
	pageB.SourceSeqMin = 2
	pageB.SourceSeqMax = 2
	pageB.SourceDigest = pages.SourceDigest(strings.Repeat("7", 64))
	mutations := indexer.MutationList{
		{Op: pages.MutationUpsert, Page: pageA},
		{Op: pages.MutationUpsert, Page: pageB},
	}
	if err := idx.Apply(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{
		denseRecord(pageA, "embed/manual-a", []float64{1, 0}),
		denseRecord(pageB, "embed/manual-b", []float64{0, 1}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Rebuild(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		namespace string
		query     []float64
		want      pages.PageID
	}{
		{namespace: "embed/manual-a", query: []float64{1, 0}, want: pageA.ID},
		{namespace: "embed/manual-b", query: []float64{0, 1}, want: pageB.ID},
	} {
		hits, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", EmbeddingNamespace: test.namespace, Limit: 2}, test.query)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Page.ID != test.want {
			t.Fatalf("manual namespace %q rebuild hits=%#v want %s", test.namespace, hits, test.want)
		}
	}
}

func TestMissingVectorPagesDetectsAbsentWrongNamespaceAndStaleTuple(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	namespace := "ns/current"
	pagesToApply := []pages.WarmPage{
		samplePage("s", "wp_"+strings.Repeat("1", 32), pages.KindFailureEpisode, "missing vector", "main"),
		samplePage("s", "wp_"+strings.Repeat("2", 32), pages.KindFailureEpisode, "covered vector", "main"),
		samplePage("s", "wp_"+strings.Repeat("3", 32), pages.KindFailureEpisode, "wrong namespace", "main"),
		samplePage("s", "wp_"+strings.Repeat("4", 32), pages.KindFailureEpisode, "stale digest", "main"),
	}
	for i := range pagesToApply {
		pagesToApply[i].Version = pages.PageVersion(i + 1)
		pagesToApply[i].SourceDigest = pages.SourceDigest(strings.Repeat(fmt.Sprintf("%x", i+1), 64))
		pagesToApply[i].SourceSeqMin = int64(i + 1)
		pagesToApply[i].SourceSeqMax = int64(i + 1)
	}
	var muts []pages.PageMutation
	for _, page := range pagesToApply {
		muts = append(muts, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
	}
	if err := idx.Apply(ctx, muts); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{
		denseRecord(pagesToApply[1], namespace, []float64{1, 0}),
		denseRecord(pagesToApply[2], "ns/old", []float64{1, 0}),
		denseRecord(pagesToApply[3], namespace, []float64{1, 0}),
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(idx.Dir(), "warm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE warm_pages SET source_digest = ? WHERE page_id = ?`, strings.Repeat("f", 64), string(pagesToApply[3].ID)); err != nil {
		t.Fatal(err)
	}
	missing, err := idx.MissingVectorPages(ctx, "s", namespace, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[pages.PageID]bool{}
	for _, page := range missing {
		got[page.ID] = true
	}
	for _, want := range []pages.PageID{pagesToApply[0].ID, pagesToApply[2].ID, pagesToApply[3].ID} {
		if !got[want] {
			t.Fatalf("missing pages did not include %s: %#v", want, missing)
		}
	}
	if got[pagesToApply[1].ID] {
		t.Fatalf("covered page reported missing: %#v", missing)
	}
}

func TestMissingVectorPagesSkipsSuppressedUntilPageTupleChanges(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	namespace := "ns/suppressed"
	denied := samplePage("s", "wp_"+strings.Repeat("6", 32), pages.KindConstraint, "denied lexical only", "main")
	safe := samplePage("s", "wp_"+strings.Repeat("7", 32), pages.KindConstraint, "safe still missing", "main")
	denied.Version = 1
	denied.SourceSeqMin = 1
	denied.SourceSeqMax = 1
	denied.SourceDigest = pages.SourceDigest(strings.Repeat("6", 64))
	safe.Version = 2
	safe.SourceSeqMin = 2
	safe.SourceSeqMax = 2
	safe.SourceDigest = pages.SourceDigest(strings.Repeat("7", 64))
	if err := idx.Apply(ctx, []pages.PageMutation{
		{Op: pages.MutationUpsert, Page: denied},
		{Op: pages.MutationUpsert, Page: safe},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.SuppressVectors(ctx, []indexer.VectorSuppression{
		denseSuppression(denied, namespace, indexer.EmbeddingStatusAdmissionDenied),
	}); err != nil {
		t.Fatal(err)
	}
	missing, err := idx.MissingVectorPages(ctx, "s", namespace, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[pages.PageID]bool{}
	for _, page := range missing {
		got[page.ID] = true
	}
	if got[denied.ID] {
		t.Fatalf("suppressed current tuple reported missing: %#v", missing)
	}
	if !got[safe.ID] {
		t.Fatalf("safe unsuppressed page not reported missing: %#v", missing)
	}

	denied.Version = 3
	denied.SourceSeqMin = 3
	denied.SourceSeqMax = 3
	denied.SourceDigest = pages.SourceDigest(strings.Repeat("8", 64))
	denied.SearchText = "denied lexical only changed"
	denied.Summary = denied.SearchText
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: denied}}); err != nil {
		t.Fatal(err)
	}
	missing, err = idx.MissingVectorPages(ctx, "s", namespace, 10)
	if err != nil {
		t.Fatal(err)
	}
	got = map[pages.PageID]bool{}
	for _, page := range missing {
		got[page.ID] = true
	}
	if !got[denied.ID] {
		t.Fatalf("changed suppressed page tuple did not become eligible again: %#v", missing)
	}
}

func TestDenseWritebackRejectsStaleExpectedTupleWithoutMutation(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	namespace := "ns/tuple-cas"
	page := samplePage("s", "wp_"+strings.Repeat("d", 32), pages.KindConstraint, "tuple version one", "")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	staleVector := denseRecord(page, namespace, []float64{1, 0})
	staleSuppression := denseSuppression(page, namespace, indexer.EmbeddingStatusAdmissionDenied)

	updated := page
	updated.Version++
	updated.SourceSeqMax++
	updated.SourceDigest = pages.SourceDigest(strings.Repeat("e", 64))
	updated.SearchText = "kind=constraint tuple version two"
	updated.Summary = "tuple version two"
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: updated}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{staleVector}); !errors.Is(err, indexer.ErrStalePageTuple) {
		t.Fatalf("stale vector error=%v want ErrStalePageTuple", err)
	}
	if err := idx.SuppressVectors(ctx, []indexer.VectorSuppression{staleSuppression}); !errors.Is(err, indexer.ErrStalePageTuple) {
		t.Fatalf("stale suppression error=%v want ErrStalePageTuple", err)
	}
	missing, err := idx.MissingVectorPages(ctx, "s", namespace, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].ID != updated.ID || missing[0].Version != updated.Version {
		t.Fatalf("current tuple was covered by stale writeback: %#v", missing)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", EmbeddingNamespace: namespace, Limit: 10}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("stale vector became searchable: %#v", hits)
	}
}

func TestDenseWritesCompareCompleteExpectedTuple(t *testing.T) {
	tests := []struct {
		name string
		edit func(*indexer.VectorRecord, *indexer.VectorSuppression)
	}{
		{
			name: "session",
			edit: func(record *indexer.VectorRecord, suppression *indexer.VectorSuppression) {
				record.SessionID = "other"
				suppression.SessionID = "other"
			},
		},
		{
			name: "page_version",
			edit: func(record *indexer.VectorRecord, suppression *indexer.VectorSuppression) {
				record.PageVersion++
				suppression.PageVersion++
			},
		},
		{
			name: "source_digest",
			edit: func(record *indexer.VectorRecord, suppression *indexer.VectorSuppression) {
				record.SourceDigest = pages.SourceDigest(strings.Repeat("9", 64))
				suppression.SourceDigest = record.SourceDigest
			},
		},
		{
			name: "compiler_version",
			edit: func(record *indexer.VectorRecord, suppression *indexer.VectorSuppression) {
				record.CompilerVersion = pages.CompilerVersion("pages/v-foreign")
				suppression.CompilerVersion = record.CompilerVersion
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := openDenseIndex(t, 2)
			ctx := context.Background()
			page := samplePage("s", "wp_"+strings.Repeat("3", 32), pages.KindConstraint, "complete tuple", "")
			if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
				t.Fatal(err)
			}
			record := denseRecord(page, "ns/tuple-fields", []float64{1, 0})
			suppression := denseSuppression(page, "ns/tuple-fields", indexer.EmbeddingStatusAdmissionDenied)
			tt.edit(&record, &suppression)
			if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{record}); !errors.Is(err, indexer.ErrStalePageTuple) {
				t.Fatalf("vector mismatch error=%v want ErrStalePageTuple", err)
			}
			if err := idx.SuppressVectors(ctx, []indexer.VectorSuppression{suppression}); !errors.Is(err, indexer.ErrStalePageTuple) {
				t.Fatalf("suppression mismatch error=%v want ErrStalePageTuple", err)
			}
		})
	}
}

func TestDenseBatchStaleTupleRollsBackEarlierRecords(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	namespace := "ns/tuple-batch"
	first := samplePage("s", "wp_"+strings.Repeat("e", 32), pages.KindConstraint, "first current", "")
	second := samplePage("s", "wp_"+strings.Repeat("f", 32), pages.KindConstraint, "second old", "")
	second.Version = 2
	second.SourceSeqMin = 2
	second.SourceSeqMax = 2
	second.SourceDigest = pages.SourceDigest(strings.Repeat("f", 64))
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: first}, {Op: pages.MutationUpsert, Page: second}}); err != nil {
		t.Fatal(err)
	}
	staleSecond := denseRecord(second, namespace, []float64{0, 1})
	second.Version++
	second.SourceSeqMax++
	second.SourceDigest = pages.SourceDigest(strings.Repeat("1", 64))
	second.SearchText = "kind=constraint second current"
	second.Summary = "second current"
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: second}}); err != nil {
		t.Fatal(err)
	}
	err := idx.UpsertVectors(ctx, []indexer.VectorRecord{
		denseRecord(first, namespace, []float64{1, 0}),
		staleSecond,
	})
	if !errors.Is(err, indexer.ErrStalePageTuple) {
		t.Fatalf("batch error=%v want ErrStalePageTuple", err)
	}
	missing, err := idx.MissingVectorPages(ctx, "s", namespace, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(missing); got != 2 {
		t.Fatalf("partial vector batch committed before stale tuple: missing=%#v", missing)
	}
}

func TestSuppressionRemovesProjectionAndWinsSameTupleUpsert(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	namespace := "ns/suppression-wins"
	idxA, err := indexer.Open(dir, indexer.Options{DenseDimensions: 2, EmbeddingNamespace: namespace})
	if err != nil {
		t.Fatal(err)
	}
	defer idxA.Close()
	idxB, err := indexer.Open(dir, indexer.Options{DenseDimensions: 2, EmbeddingNamespace: namespace})
	if err != nil {
		t.Fatal(err)
	}
	defer idxB.Close()
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("2", 32), pages.KindConstraint, "suppression owns tuple", "")
	if err := idxA.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	record := denseRecord(page, namespace, []float64{1, 0})
	suppression := denseSuppression(page, namespace, indexer.EmbeddingStatusAdmissionDenied)
	if err := idxA.UpsertVectors(ctx, []indexer.VectorRecord{record}); err != nil {
		t.Fatal(err)
	}
	if err := idxB.SuppressVectors(ctx, []indexer.VectorSuppression{suppression}); err != nil {
		t.Fatal(err)
	}
	if err := idxA.UpsertVectors(ctx, []indexer.VectorRecord{record}); err != nil {
		t.Fatalf("same-tuple upsert should be a suppressed no-op: %v", err)
	}
	if err := idxB.DeleteVector(ctx, page.ID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- idxA.UpsertVectors(ctx, []indexer.VectorRecord{record})
		}()
		go func() {
			defer wg.Done()
			errs <- idxB.SuppressVectors(ctx, []indexer.VectorSuppression{suppression})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	hits, err := idxA.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", EmbeddingNamespace: namespace, Limit: 10}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("suppressed tuple remained dense-searchable: %#v", hits)
	}
	missing, err := idxA.MissingVectorPages(ctx, "s", namespace, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("suppressed tuple returned to backlog: %#v", missing)
	}
	health, err := idxA.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.VectorCount != 0 {
		t.Fatalf("suppression coexists with %d dense map rows", health.VectorCount)
	}
}

func TestDenseShadowLifecycleConcurrent(t *testing.T) {
	idx := openDenseIndex(t, 2)
	ctx := context.Background()
	namespace := "ns/shadow-race"
	pagesToApply := make([]pages.WarmPage, 0, 8)
	mutations := make([]pages.PageMutation, 0, 8)
	for i := 0; i < 8; i++ {
		page := samplePage(
			"s",
			fmt.Sprintf("wp_%032x", 0x700+i),
			pages.KindFailureEpisode,
			fmt.Sprintf("shadow lifecycle page %02d", i),
			"main",
		)
		page.Version = pages.PageVersion(i + 1)
		page.SourceSeqMin = int64(i + 1)
		page.SourceSeqMax = int64(i + 1)
		page.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", 0x700+i))
		pagesToApply = append(pagesToApply, page)
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
	}
	if err := idx.Apply(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	vectorFor := func(i int) indexer.VectorRecord {
		return denseRecord(pagesToApply[i], namespace, []float64{float64(i%3) + 1, float64((i+1)%3) + 1})
	}
	initial := make([]indexer.VectorRecord, 0, len(pagesToApply))
	for i := range pagesToApply {
		initial = append(initial, vectorFor(i))
	}
	if err := idx.UpsertVectors(ctx, initial); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 512)
	for worker := 0; worker < 2; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 48; i++ {
				pageIndex := (i + worker) % len(pagesToApply)
				if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{vectorFor(pageIndex)}); err != nil {
					errs <- err
					continue
				}
				if i%3 == 0 {
					if err := idx.DeleteVector(ctx, pagesToApply[pageIndex].ID); err != nil {
						errs <- err
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 96; i++ {
			missing, err := idx.MissingVectorPages(ctx, "s", namespace, len(pagesToApply))
			if err != nil {
				errs <- err
				continue
			}
			for _, page := range missing {
				if page.SessionID != "s" {
					errs <- fmt.Errorf("missing page session=%q", page.SessionID)
					return
				}
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 96; i++ {
			hits, err := idx.SearchDense(ctx, indexer.SearchQuery{
				SessionID: "s", EmbeddingNamespace: namespace, Limit: 5,
			}, []float64{1, 1})
			if err != nil {
				errs <- err
				continue
			}
			for _, hit := range hits {
				if hit.Page.SessionID != "s" {
					errs <- fmt.Errorf("dense hit session=%q", hit.Page.SessionID)
					return
				}
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	allVectors := make([]indexer.VectorRecord, 0, len(pagesToApply))
	for i := range pagesToApply {
		allVectors = append(allVectors, vectorFor(i))
	}
	if err := idx.UpsertVectors(ctx, allVectors); err != nil {
		t.Fatal(err)
	}
	missing, err := idx.MissingVectorPages(ctx, "s", namespace, len(pagesToApply))
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("fully covered pages reported missing: %#v", missing)
	}
	deleted := pagesToApply[0].ID
	if err := idx.DeleteVector(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	missing, err = idx.MissingVectorPages(ctx, "s", namespace, len(pagesToApply))
	if err != nil {
		t.Fatal(err)
	}
	gotDeleted := false
	for _, page := range missing {
		if page.ID == deleted {
			gotDeleted = true
		}
	}
	if !gotDeleted {
		t.Fatalf("deleted vector page not reported missing: %#v", missing)
	}
	hits, err := idx.SearchDense(ctx, indexer.SearchQuery{
		SessionID: "s", EmbeddingNamespace: namespace, Limit: len(pagesToApply),
	}, []float64{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.Page.ID == deleted {
			t.Fatalf("deleted vector still returned by dense search: %#v", hits)
		}
	}
}

func TestRebuildPreservesEmbeddingNamespaceMeta(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	idx, err := indexer.Open(dir, indexer.Options{DenseDimensions: 2, EmbeddingNamespace: "ns/rebuild"})
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("5", 32), pages.KindFailureEpisode, "namespace meta", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Rebuild(ctx, indexer.MutationList{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	health, err := idx.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.EmbeddingNamespace != "ns/rebuild" {
		t.Fatalf("rebuild namespace=%q want ns/rebuild", health.EmbeddingNamespace)
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

func denseRecord(page pages.WarmPage, namespace string, vector []float64) indexer.VectorRecord {
	return indexer.VectorRecord{
		PageID:             page.ID,
		SessionID:          page.SessionID,
		PageVersion:        page.Version,
		SourceDigest:       page.SourceDigest,
		CompilerVersion:    page.CompilerVersion,
		EmbeddingNamespace: namespace,
		Vector:             vector,
	}
}

func denseSuppression(page pages.WarmPage, namespace, reason string) indexer.VectorSuppression {
	return indexer.VectorSuppression{
		PageID:             page.ID,
		SessionID:          page.SessionID,
		PageVersion:        page.Version,
		SourceDigest:       page.SourceDigest,
		CompilerVersion:    page.CompilerVersion,
		EmbeddingNamespace: namespace,
		Reason:             reason,
	}
}
