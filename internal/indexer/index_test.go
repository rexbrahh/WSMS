package indexer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if err := idx.ApplyWithWatermark(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}, "sess-a", 3, 3); err != nil {
		t.Fatal(err)
	}
	seq, ver, err := idx.Watermark(ctx, "sess-a")
	if err != nil || seq != 3 || ver != 3 {
		t.Fatalf("watermark seq=%d ver=%d err=%v", seq, ver, err)
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
