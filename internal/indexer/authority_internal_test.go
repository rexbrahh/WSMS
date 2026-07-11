package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"wsms/internal/pages"
	"wsms/internal/types"
)

func TestEligibilityTupleBoundIsTypedAndNeverTruncated(t *testing.T) {
	tuples := make([]PageTuple, MaxAuthoritySnapshotPages+1)
	for i := range tuples {
		tuples[i] = PageTuple{
			PageID: pages.PageID(fmt.Sprintf("wp_%032x", i+1)), PageVersion: 1,
			SessionID: "bound", SourceDigest: pages.SourceDigest(strings.Repeat("a", 64)),
			CompilerVersion: pages.CurrentCompilerVersion,
		}
	}
	_, err := newSearchPlan(SearchQuery{
		SessionID: "bound", EligibilityComplete: true, EligiblePageTuples: tuples,
	})
	if !errors.Is(err, ErrEligibilitySetTooLarge) {
		t.Fatalf("eligibility bound error=%v", err)
	}
}

func TestActivePageSnapshotBoundIsTypedAndNeverTruncated(t *testing.T) {
	idx, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	idx.mu.RLock()
	tx, err := idx.db.Begin()
	idx.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`
INSERT INTO warm_pages(
  page_id, page_version, session_id, scope, kind, trust, status, salience,
  salience_reason, search_text, summary, refs_json, source_digest,
  source_seq_min, source_seq_max, compiler_version, scope_epoch, created_at
) VALUES(?, 1, 'snapshot-bound', 'global', 'constraint', 'system', 'active', 0.5,
         '', '', '', '[{"kind":"event","id":"EBound"}]', ?, 1, 1, ?, 0, ?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	created := time.Unix(1, 0).UTC().Format(time.RFC3339Nano)
	for i := 0; i <= MaxAuthoritySnapshotPages; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("wp_%032x", i+1), strings.Repeat("a", 64), string(pages.CurrentCompilerVersion), created); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := idx.ActivePageSnapshot(context.Background(), "snapshot-bound")
	if !errors.Is(err, ErrAuthoritySnapshotTooLarge) {
		t.Fatalf("snapshot=%#v error=%v", snapshot, err)
	}
	if len(snapshot.Pages) != 0 {
		t.Fatalf("over-bound snapshot leaked truncated authority: %d", len(snapshot.Pages))
	}
}

func TestActivePageSnapshotRejectsCorruptStoredDescriptors(t *testing.T) {
	tests := []struct {
		name             string
		statement        string
		value            any
		legacySearchable bool
	}{
		{name: "unknown kind", statement: `UPDATE warm_pages SET kind = ? WHERE page_id = ?`, value: "unknown", legacySearchable: true},
		{name: "unknown trust", statement: `UPDATE warm_pages SET trust = ? WHERE page_id = ?`, value: "unknown", legacySearchable: true},
		{name: "invalid kind trust pairing", statement: `UPDATE warm_pages SET kind = ? WHERE page_id = ?`, value: string(pages.KindFailureEpisode), legacySearchable: true},
		{name: "unknown scope", statement: `UPDATE warm_pages SET scope = ? WHERE page_id = ?`, value: "unknown", legacySearchable: true},
		{name: "scope relationship", statement: `UPDATE warm_pages SET scope = ? WHERE page_id = ?`, value: string(types.ScopeGlobal), legacySearchable: true},
		{name: "malformed logical ref", statement: `UPDATE warm_pages SET refs_json = ? WHERE page_id = ?`, value: `[{"kind":"event","id":"bad/id"}]`, legacySearchable: true},
		{name: "unknown ref field", statement: `UPDATE warm_pages SET refs_json = ? WHERE page_id = ?`, value: `[{"kind":"event","id":"ECorrupt","extra":"hidden"}]`},
		{name: "refs trailing json", statement: `UPDATE warm_pages SET refs_json = ? WHERE page_id = ?`, value: `[{"kind":"event","id":"ECorrupt"}] {}`},
		{name: "traversing path", statement: `UPDATE warm_pages SET path_scope_json = ? WHERE page_id = ?`, value: `["../escape"]`, legacySearchable: true},
		{name: "duplicate path", statement: `UPDATE warm_pages SET path_scope_json = ? WHERE page_id = ?`, value: `["src/corrupt.go","src/corrupt.go"]`, legacySearchable: true},
		{name: "path trailing json", statement: `UPDATE warm_pages SET path_scope_json = ? WHERE page_id = ?`, value: `["src/corrupt.go"] []`},
		{name: "uncanonical branch", statement: `UPDATE warm_pages SET branch = ? WHERE page_id = ?`, value: " main", legacySearchable: true},
		{name: "control commit", statement: `UPDATE warm_pages SET commit_id = ? WHERE page_id = ?`, value: "bad\ncommit", legacySearchable: true},
		{name: "invalid utf8 commit", statement: `UPDATE warm_pages SET commit_id = ? WHERE page_id = ?`, value: string([]byte{0xff}), legacySearchable: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			idx, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = idx.Close() })
			page := validAuthorityTestPage()
			if err := idx.Apply(context.Background(), []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
				t.Fatal(err)
			}
			before, err := idx.ActivePageSnapshot(context.Background(), page.SessionID)
			if err != nil || len(before.Pages) != 1 {
				t.Fatalf("valid snapshot=%#v err=%v", before, err)
			}

			idx.mu.RLock()
			_, err = idx.db.ExecContext(context.Background(), test.statement, test.value, string(page.ID))
			idx.mu.RUnlock()
			if err != nil {
				t.Fatal(err)
			}
			if test.legacySearchable {
				hits, searchErr := idx.SearchLexical(context.Background(), SearchQuery{
					SessionID: page.SessionID, Text: "descriptor corruption needle", Limit: 1,
				})
				if searchErr != nil || len(hits) != 1 {
					t.Fatalf("corrupt row should remain capable of filling legacy top-k: hits=%#v err=%v", hits, searchErr)
				}
			}
			snapshot, err := idx.ActivePageSnapshot(context.Background(), page.SessionID)
			if !errors.Is(err, ErrAuthorityDescriptorCorrupt) {
				t.Fatalf("snapshot=%#v error=%v", snapshot, err)
			}
			if len(snapshot.Pages) != 0 {
				t.Fatalf("corrupt snapshot leaked partial allowlist: %#v", snapshot.Pages)
			}
		})
	}
}

func TestMalformedKindTrustChaffCannotBecomePartialAuthority(t *testing.T) {
	idx, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	const chaffCount = maxSearchLimit + 1
	mutations := make([]pages.PageMutation, 0, chaffCount+1)
	for i := 1; i <= chaffCount+1; i++ {
		page := validAuthorityTestPage()
		page.ID = pages.PageID(fmt.Sprintf("wp_%032x", i))
		page.Version = pages.PageVersion(i)
		page.SourceSeqMin, page.SourceSeqMax = int64(i), int64(i)
		page.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", i))
		page.Refs = []pages.PageRef{{Kind: pages.RefEvent, ID: fmt.Sprintf("EChaff%d", i)}}
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
	}
	if err := idx.Apply(context.Background(), mutations); err != nil {
		t.Fatal(err)
	}
	target := mutations[len(mutations)-1].Page
	idx.mu.RLock()
	_, err = idx.db.ExecContext(context.Background(), `
UPDATE warm_pages
SET kind = ?
WHERE session_id = ? AND page_id <> ?`, string(pages.KindFailureEpisode), target.SessionID, string(target.ID))
	idx.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}

	hits, err := idx.SearchLexical(context.Background(), SearchQuery{
		SessionID: target.SessionID, Text: "descriptor corruption needle", Limit: maxSearchLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != maxSearchLimit {
		t.Fatalf("legacy top-k count=%d want %d", len(hits), maxSearchLimit)
	}
	for _, hit := range hits {
		if hit.Page.ID == target.ID || hit.Page.Kind != pages.KindFailureEpisode || hit.Page.Trust != pages.TrustRepo {
			t.Fatalf("malformed chaff did not fill top-k: %#v", hit.Page)
		}
	}
	snapshot, err := idx.ActivePageSnapshot(context.Background(), target.SessionID)
	if !errors.Is(err, ErrAuthorityDescriptorCorrupt) {
		t.Fatalf("snapshot=%#v error=%v", snapshot, err)
	}
	if len(snapshot.Pages) != 0 {
		t.Fatalf("malformed chaff produced partial authority: %#v", snapshot.Pages)
	}
}

func validAuthorityTestPage() pages.WarmPage {
	return pages.WarmPage{
		ID: pages.PageID("wp_" + strings.Repeat("c", 32)), Version: 1, SessionID: "authority-corrupt",
		RepoID: "repo", Branch: "main", Commit: "abcdef1", PathScope: []string{"src/corrupt.go"},
		Scope: types.ScopeFile, Kind: pages.KindFileContext, Trust: pages.TrustRepo, Status: pages.StatusActive,
		Salience: 0.5, SalienceReason: "corruption fixture",
		SearchText: "kind=file_context descriptor corruption needle", Summary: "descriptor corruption needle",
		Refs:         []pages.PageRef{{Kind: pages.RefEvent, ID: "ECorrupt"}},
		SourceSeqMin: 1, SourceSeqMax: 1, SourceDigest: pages.SourceDigest(strings.Repeat("c", 64)),
		CompilerVersion: pages.CurrentCompilerVersion, CreatedAt: time.Unix(1, 0).UTC(),
	}
}
