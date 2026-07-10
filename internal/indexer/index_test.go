package indexer_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wsms/internal/indexer"
	"wsms/internal/pages"
	"wsms/internal/types"
)

func TestApplySearchAndIdempotent(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	page := samplePage("sess-a", "wp_"+strings.Repeat("a", 32), pages.KindFailureEpisode, "stream goroutine still blocked", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	// idempotent
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchLexical(ctx, indexer.SearchQuery{
		SessionID: "sess-a", Text: "stream goroutine blocked", Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != page.ID {
		t.Fatalf("hits=%#v", hits)
	}
	if !strings.Contains(hits[0].Explanation, "fts5") {
		t.Fatalf("explanation=%q", hits[0].Explanation)
	}
}

func TestCrossSessionIsolation(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	a := samplePage("sess-a", "wp_"+strings.Repeat("b", 32), pages.KindConstraint, "do not rewrite transport layer", "")
	b := samplePage("sess-b", "wp_"+strings.Repeat("c", 32), pages.KindConstraint, "do not rewrite transport layer", "")
	if err := idx.Apply(ctx, []pages.PageMutation{
		{Op: pages.MutationUpsert, Page: a},
		{Op: pages.MutationUpsert, Page: b},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchLexical(ctx, indexer.SearchQuery{SessionID: "sess-a", Text: "rewrite transport", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.SessionID != "sess-a" {
		t.Fatalf("isolation failed: %#v", hits)
	}
}

func TestInvalidateRemovesFromSearch(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	page := samplePage("sess-a", "wp_"+strings.Repeat("d", 32), pages.KindConstraint, "do not rewrite transport layer", "")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	inv := page
	inv.Status = pages.StatusInvalidated
	inv.Version++
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationInvalidate, Page: inv}}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchLexical(ctx, indexer.SearchQuery{SessionID: "sess-a", Text: "rewrite transport", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits after invalidate, got %#v", hits)
	}
}

func TestWatermarkAdvances(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	page := samplePage("sess-a", "wp_"+strings.Repeat("e", 32), pages.KindTaskCheckpoint, "fix transport goal", "")
	if err := idx.ApplyWithWatermark(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}, "sess-a", 1, 1); err != nil {
		t.Fatal(err)
	}
	seq, ver, err := idx.Watermark(ctx, "sess-a")
	if err != nil || seq != 1 || ver != 1 {
		t.Fatalf("watermark seq=%d ver=%d err=%v", seq, ver, err)
	}
}

func TestWatermarkRejectsGapUntilContiguousCatchup(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	if err := idx.ApplyWithWatermark(ctx, nil, "s", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := idx.ApplyWithWatermark(ctx, nil, "s", 3, 3); !errors.Is(err, indexer.ErrWatermarkGap) {
		t.Fatalf("gap error=%v", err)
	}
	seq, _, err := idx.Watermark(ctx, "s")
	if err != nil || seq != 1 {
		t.Fatalf("watermark after gap=(%d,%v), want 1", seq, err)
	}
	if err := idx.ApplyWithWatermark(ctx, nil, "s", 2, 2); err != nil {
		t.Fatal(err)
	}
	if err := idx.ApplyWithWatermark(ctx, nil, "s", 3, 3); err != nil {
		t.Fatal(err)
	}
	seq, _, err = idx.Watermark(ctx, "s")
	if err != nil || seq != 3 {
		t.Fatalf("watermark after catchup=(%d,%v), want 3", seq, err)
	}
}

func TestSetWatermarkRejectsGapAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	idx, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := idx.SetWatermark(ctx, "s", 100, 100); !errors.Is(err, indexer.ErrWatermarkGap) {
		t.Fatalf("forged watermark error=%v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	seq, _, err := reopened.Watermark(ctx, "s")
	if err != nil || seq != 0 {
		t.Fatalf("watermark after rejected jump=(%d,%v), want 0", seq, err)
	}
	if err := reopened.SetWatermark(ctx, "s", 1, 1); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildCutover(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	old := samplePage("sess-a", "wp_"+strings.Repeat("f", 32), pages.KindFailureEpisode, "old failure signature", "main")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: old}}); err != nil {
		t.Fatal(err)
	}
	newer := samplePage("sess-a", "wp_"+strings.Repeat("1", 32), pages.KindFailureEpisode, "stream goroutine still blocked", "main")
	if err := idx.Rebuild(ctx, indexer.MutationList{{Op: pages.MutationUpsert, Page: newer}}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.SearchLexical(ctx, indexer.SearchQuery{SessionID: "sess-a", Text: "stream goroutine", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != newer.ID {
		t.Fatalf("after rebuild hits=%#v", hits)
	}
	// old signature gone
	hits, err = idx.SearchLexical(ctx, indexer.SearchQuery{SessionID: "sess-a", Text: "old failure signature", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("stale rebuild residue: %#v", hits)
	}
	h, err := idx.Health(ctx)
	if err != nil || h.Generation < 2 {
		t.Fatalf("health=%#v err=%v", h, err)
	}
}

func TestOrphanRebuildNotServed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	idx, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page := samplePage("sess-a", "wp_"+strings.Repeat("2", 32), pages.KindConstraint, "do not rewrite transport", "")
	if err := idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	// Simulate incomplete rebuild artifact.
	orphan := filepath.Join(dir, "warm.rebuild.db")
	if err := os.WriteFile(orphan, []byte("not a real sqlite file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen must drop orphan and still serve old gen.
	idx2, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx2.Close() })
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still present: %v", err)
	}
	hits, err := idx2.SearchLexical(ctx, indexer.SearchQuery{SessionID: "sess-a", Text: "rewrite transport", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("serving gen lost: %#v", hits)
	}
}

func TestOpenRestoresPreviousGenerationAcrossCutoverCrash(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	idx, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	page := samplePage("s", "wp_"+strings.Repeat("6", 32), pages.KindConstraint, "restored generation", "")
	if err := idx.Apply(context.Background(), []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	serving := filepath.Join(dir, "warm.db")
	if err := os.Rename(serving, serving+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "warm.rebuild.db"), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	hits, err := restored.SearchLexical(context.Background(), indexer.SearchQuery{SessionID: "s", Text: "restored generation"})
	if err != nil || len(hits) != 1 || hits[0].Page.ID != page.ID {
		t.Fatalf("restored hits=%#v err=%v", hits, err)
	}
}

func TestOpenRemovesMalformedStaleRebuildLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	idx, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rebuild.lock"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Rebuild(context.Background(), indexer.MutationList{}); err != nil {
		t.Fatalf("rebuild after stale lock: %v", err)
	}
}

func TestConcurrentOpenSerializesDisposableSchemaReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
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
	if _, err := db.Exec(`UPDATE index_meta SET value = '1' WHERE key = 'schema_version'`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type openResult struct {
		idx *indexer.Index
		err error
	}
	results := make(chan openResult, 2)
	for range 2 {
		go func() {
			<-start
			opened, openErr := indexer.Open(dir)
			results <- openResult{idx: opened, err: openErr}
		}()
	}
	close(start)
	opened := make([]*indexer.Index, 0, 2)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent open: %v", result.err)
		}
		opened = append(opened, result.idx)
	}
	for _, handle := range opened {
		handle := handle
		t.Cleanup(func() { _ = handle.Close() })
		health, err := handle.Health(context.Background())
		if err != nil || health.SchemaVersion != "2" {
			t.Fatalf("replacement health=%#v err=%v", health, err)
		}
	}

	page := samplePage("s", "wp_"+strings.Repeat("d", 32), pages.KindConstraint, "serialized replacement", "")
	if err := opened[0].Apply(context.Background(), []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	hits, err := opened[1].SearchLexical(context.Background(), indexer.SearchQuery{SessionID: "s", Text: "serialized replacement"})
	if err != nil || len(hits) != 1 || hits[0].Page.ID != page.ID {
		t.Fatalf("cross-handle replacement hits=%#v err=%v", hits, err)
	}
}

func TestConcurrentApplyWaitsForRebuildAndLandsOnNewGeneration(t *testing.T) {
	idx := openTestIndex(t)
	ctx := context.Background()
	pageA := samplePage("s", "wp_"+strings.Repeat("7", 32), pages.KindConstraint, "rebuilt alpha", "")
	pageB := samplePage("s", "wp_"+strings.Repeat("8", 32), pages.KindConstraint, "applied beta", "")
	started := make(chan struct{})
	release := make(chan struct{})
	rebuildDone := make(chan error, 1)
	go func() {
		rebuildDone <- idx.Rebuild(ctx, blockingSource{
			mutations: indexer.MutationList{{Op: pages.MutationUpsert, Page: pageA}},
			started:   started, release: release,
		})
	}()
	<-started
	if err := idx.Rebuild(ctx, indexer.MutationList{}); !errors.Is(err, indexer.ErrRebuildInProgress) {
		t.Fatalf("concurrent rebuild error=%v", err)
	}
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- idx.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: pageB}})
	}()
	select {
	case err := <-applyDone:
		t.Fatalf("apply bypassed rebuild writer lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-rebuildDone; err != nil {
		t.Fatal(err)
	}
	if err := <-applyDone; err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"rebuilt alpha", "applied beta"} {
		hits, err := idx.SearchLexical(ctx, indexer.SearchQuery{SessionID: "s", Text: query})
		if err != nil || len(hits) != 1 {
			t.Fatalf("query %q hits=%#v err=%v", query, hits, err)
		}
	}
}

func TestMultiHandleRebuildBlocksStaleApplyAndRebindsInvalidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	idxA, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idxA.Close() })
	idxB, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idxB.Close() })
	idxC, err := indexer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idxC.Close() })

	ctx := context.Background()
	oldPage := samplePage("s", "wp_"+strings.Repeat("9", 32), pages.KindConstraint, "old detached generation", "")
	if err := idxA.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: oldPage}}); err != nil {
		t.Fatal(err)
	}
	rebuilt := samplePage("s", "wp_"+strings.Repeat("a", 32), pages.KindConstraint, "rebuilt alpha", "")
	applied := samplePage("s", "wp_"+strings.Repeat("b", 32), pages.KindConstraint, "applied beta", "")

	started := make(chan struct{})
	release := make(chan struct{})
	rebuildDone := make(chan error, 1)
	go func() {
		rebuildDone <- idxA.Rebuild(ctx, blockingSource{
			mutations: indexer.MutationList{{Op: pages.MutationUpsert, Page: rebuilt}},
			started:   started,
			release:   release,
		})
	}()
	<-started

	applyDone := make(chan error, 1)
	go func() {
		applyDone <- idxB.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: applied}})
	}()
	select {
	case err := <-applyDone:
		t.Fatalf("stale handle apply bypassed rebuild lease: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-rebuildDone; err != nil {
		t.Fatal(err)
	}
	if err := <-applyDone; err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{"rebuilt alpha", "applied beta"} {
		hits, err := idxA.SearchLexical(ctx, indexer.SearchQuery{SessionID: "s", Text: query, Limit: 5})
		if err != nil || len(hits) != 1 {
			t.Fatalf("query %q hits=%#v err=%v", query, hits, err)
		}
	}
	hits, err := idxA.SearchLexical(ctx, indexer.SearchQuery{SessionID: "s", Text: "old detached", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("stale old generation remained searchable: %#v", hits)
	}

	invalidated := rebuilt
	invalidated.Status = pages.StatusInvalidated
	invalidated.Version++
	if err := idxC.Apply(ctx, []pages.PageMutation{{Op: pages.MutationInvalidate, Page: invalidated}}); err != nil {
		t.Fatal(err)
	}
	hits, err = idxA.SearchLexical(ctx, indexer.SearchQuery{SessionID: "s", Text: "rebuilt alpha", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("stale handle invalidation did not land on serving generation: %#v", hits)
	}
}

func TestSymlinkAliasSharesRebuildLeaseAndRebinds(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real-index")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(root, "alias-index")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	realHandle, err := indexer.Open(realDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = realHandle.Close() })
	aliasHandle, err := indexer.Open(aliasDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = aliasHandle.Close() })

	ctx := context.Background()
	rebuilt := samplePage("s", "wp_"+strings.Repeat("e", 32), pages.KindConstraint, "symlink rebuilt alpha", "")
	applied := samplePage("s", "wp_"+strings.Repeat("f", 32), pages.KindConstraint, "symlink applied beta", "")
	started := make(chan struct{})
	release := make(chan struct{})
	rebuildDone := make(chan error, 1)
	go func() {
		rebuildDone <- realHandle.Rebuild(ctx, blockingSource{
			mutations: indexer.MutationList{{Op: pages.MutationUpsert, Page: rebuilt}},
			started:   started, release: release,
		})
	}()
	<-started
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- aliasHandle.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: applied}})
	}()
	select {
	case err := <-applyDone:
		t.Fatalf("symlink alias bypassed rebuild lease: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-rebuildDone; err != nil {
		t.Fatal(err)
	}
	if err := <-applyDone; err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"symlink rebuilt alpha", "symlink applied beta"} {
		hits, err := realHandle.SearchLexical(ctx, indexer.SearchQuery{SessionID: "s", Text: query})
		if err != nil || len(hits) != 1 {
			t.Fatalf("query %q hits=%#v err=%v", query, hits, err)
		}
	}
}

func TestCloseRacesWithHealthWatermarkAndDenseSearch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	idx, err := indexer.Open(dir, indexer.Options{DenseDimensions: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page := samplePage("s", "wp_"+strings.Repeat("c", 32), pages.KindConstraint, "dense close race", "")
	if err := idx.ApplyWithWatermark(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}, "s", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, []indexer.VectorRecord{{
		PageID: page.ID, SessionID: "s", Vector: []float64{1, 0},
	}}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	stop := make(chan struct{})
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	run := func(fn func() error) {
		defer wg.Done()
		<-start
		for {
			select {
			case <-stop:
				return
			default:
			}
			err := fn()
			if err == nil {
				continue
			}
			if errors.Is(err, indexer.ErrClosed) {
				return
			}
			select {
			case errs <- err:
			default:
			}
			return
		}
	}
	for i := 0; i < 3; i++ {
		wg.Add(3)
		go run(func() error {
			_, err := idx.Health(ctx)
			return err
		})
		go run(func() error {
			_, _, err := idx.Watermark(ctx, "s")
			return err
		})
		go run(func() error {
			_, err := idx.SearchDense(ctx, indexer.SearchQuery{SessionID: "s", Limit: 2}, []float64{1, 0})
			return err
		})
	}
	close(start)
	time.Sleep(2 * time.Millisecond)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("unexpected concurrent close/read error: %v", err)
	default:
	}
}

type blockingSource struct {
	mutations indexer.MutationList
	started   chan struct{}
	release   chan struct{}
}

func (s blockingSource) ForEach(ctx context.Context, fn func(pages.PageMutation) error) error {
	close(s.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
	}
	return s.mutations.ForEach(ctx, fn)
}

func TestEmptyQuery(t *testing.T) {
	idx := openTestIndex(t)
	_, err := idx.SearchLexical(context.Background(), indexer.SearchQuery{SessionID: "s", Text: "!!!"})
	if !errors.Is(err, indexer.ErrEmptyQuery) {
		t.Fatalf("err=%v", err)
	}
}

func TestDenseUnavailable(t *testing.T) {
	idx := openTestIndex(t)
	_, err := idx.SearchDense(context.Background(), indexer.SearchQuery{SessionID: "s"}, nil)
	if !errors.Is(err, indexer.ErrDenseUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func openTestIndex(t *testing.T) *indexer.Index {
	t.Helper()
	idx, err := indexer.Open(filepath.Join(t.TempDir(), "index"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := idx.Close(); err != nil {
			t.Errorf("close index: %v", err)
		}
	})
	return idx
}

func samplePage(session, id string, kind pages.PageKind, search, branch string) pages.WarmPage {
	trust := pages.TrustTool
	scope := types.ScopeBranch
	repo, task, commit := "repo", "T1", "abc1234"
	switch kind {
	case pages.KindConstraint:
		trust = pages.TrustUser
		scope = types.ScopeTask
		branch, commit = "", ""
	case pages.KindTaskCheckpoint:
		trust = pages.TrustSystem
		scope = types.ScopeTask
		branch, commit = "", ""
	case pages.KindFailureEpisode:
		trust = pages.TrustTool
		scope = types.ScopeBranch
	}
	page := pages.WarmPage{
		ID: pages.PageID(id), Version: 1, SessionID: session,
		RepoID: repo, TaskID: task, Branch: branch, Commit: commit,
		Scope: scope, Kind: kind, Trust: trust, Status: pages.StatusActive,
		Salience: 0.9, SalienceReason: "test fixture",
		SearchText: "kind=" + string(kind) + " " + search, Summary: search,
		Refs:         []pages.PageRef{{Kind: pages.RefEvent, ID: "E0001"}},
		SourceSeqMin: 1, SourceSeqMax: 1,
		SourceDigest:    pages.SourceDigest(strings.Repeat("a", 64)),
		CompilerVersion: pages.CurrentCompilerVersion,
		CreatedAt:       time.Unix(1, 0).UTC(),
	}
	if err := (pages.PageMutation{Op: pages.MutationUpsert, Page: page}).Validate(); err != nil {
		panic(err)
	}
	return page
}
