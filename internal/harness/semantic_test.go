package harness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"wsms/internal/indexer"
	"wsms/internal/ledger"
	"wsms/internal/pages"
	"wsms/internal/retrieval"
	"wsms/internal/types"
)

func TestSemanticSearchMaterializesThenSuppressesDurableInvalidationTransitively(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-invalidation")
	ctx := context.Background()
	if err := s.RecordDecision(ctx, DecisionInput{
		Chosen: "retain the second chance", Because: "the reference bit is set",
		Scope: types.ScopeTask, AvoidText: "evict immediately", AvoidRef: "F1",
	}); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{"branch-specific failure", "retain the second chance"} {
		result, err := s.SemanticSearch(ctx, query)
		if err != nil {
			t.Fatalf("fresh query %q: %v", query, err)
		}
		if len(result.Materialized) == 0 || len(result.Materialized[0].Evidence) == 0 {
			t.Fatalf("query %q returned no exact evidence: %#v", query, result)
		}
	}

	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetRecord, Target: "F1", Reason: ledger.ReasonUserRejected,
	}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"branch-specific failure", "retain the second chance"} {
		if _, err := s.SemanticSearch(ctx, query); !errors.Is(err, retrieval.ErrSemanticPageMiss) {
			t.Fatalf("invalidated query %q error=%v", query, err)
		}
	}
}

func TestSemanticSearchCommitGatePreservesTaskScopedConstraint(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-commit")
	ctx := context.Background()
	if err := s.IngestUser(ctx, "do not evict pinned pages"); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, "verify-current-command", 0, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SemanticSearch(ctx, "verify-current-command"); err != nil {
		t.Fatalf("current command: %v", err)
	}

	if err := s.ChangeCommit(ctx, CommitChange{
		Repo: "repo", Branch: "main", FromCommit: "aaaaaaa", ToCommit: "bbbbbbb",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SemanticSearch(ctx, "verify-current-command"); !errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("old-commit command error=%v", err)
	}
	result, err := s.SemanticSearch(ctx, "do not evict pinned pages")
	if err != nil {
		t.Fatalf("task constraint after commit: %v", err)
	}
	if !resultHasKind(result, pages.KindConstraint) {
		t.Fatalf("task constraint missing after commit: %#v", result.Candidates)
	}
}

func TestSemanticSearchRejectsCorruptDerivativeProjection(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-corrupt")
	ctx := context.Background()
	snapshot := s.Coherence.Snapshot()
	candidates, err := s.Index.SearchLexical(ctx, indexer.SearchQuery{
		SessionID: s.Cfg.SessionID, RepoID: snapshot.Current.Repo, TaskID: snapshot.Current.TaskID,
		Branch: snapshot.Current.Branch, Commit: snapshot.Current.Commit,
		Text: "branch-specific failure", Limit: 10, ActiveOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var page pages.WarmPage
	for _, candidate := range candidates {
		if candidate.Page.Kind == pages.KindFailureEpisode {
			page = candidate.Page
			break
		}
	}
	if page.ID == "" {
		t.Fatal("missing indexed failure page")
	}
	page.SearchText = "kind=failure_episode error=poisonneedle"
	page.Summary = "poisonneedle"
	page.SourceDigest = pages.SourceDigest(strings.Repeat("0", 64))
	if err := s.Index.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SemanticSearch(ctx, "poisonneedle"); !errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("corrupt derivative error=%v", err)
	}
}

func TestSemanticEventMaterializationDoesNotAutoFaultRawArtifact(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-redaction")
	ctx := context.Background()
	secret := strings.Repeat("TOP_SECRET_RAW_OUTPUT ", 32)
	if err := s.IngestCommandOutput(ctx, "verify-redacted-command", 0, secret); err != nil {
		t.Fatal(err)
	}
	result, err := s.SemanticSearch(ctx, "verify-redacted-command")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Materialized) == 0 {
		t.Fatal("missing materialized event metadata")
	}
	for _, materialized := range result.Materialized {
		for _, evidence := range materialized.Evidence {
			if strings.Contains(evidence, "TOP_SECRET_RAW_OUTPUT") || strings.Contains(evidence, "artifact:sha256:") {
				t.Fatalf("raw artifact leaked into semantic evidence: %q", evidence)
			}
		}
	}
}

func TestWarmIndexRepairsContiguousGapFromLedgerReplay(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-catchup")
	ctx := context.Background()
	indexDir := s.Index.Dir()
	if err := s.Index.Close(); err != nil {
		t.Fatal(err)
	}
	s.Index = nil
	if err := s.IngestUser(ctx, "do not discard contiguous evidence"); err != nil {
		t.Fatal(err)
	}
	reopened, err := indexer.Open(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	s.Index = reopened
	if err := s.IngestAssistant(ctx, "advance after the missed index event"); err != nil {
		t.Fatal(err)
	}
	if s.IndexErr != nil {
		t.Fatalf("catchup failed: %v", s.IndexErr)
	}
	result, err := s.SemanticSearch(ctx, "do not discard contiguous evidence")
	if err != nil || !resultHasKind(result, pages.KindConstraint) {
		t.Fatalf("repaired constraint result=%#v err=%v", result, err)
	}
	seq, _, err := s.Index.Watermark(ctx, s.Cfg.SessionID)
	if err != nil || seq != s.lastEvent.Seq {
		t.Fatalf("watermark=(%d,%v), latest=%d", seq, err, s.lastEvent.Seq)
	}
}

func resultHasKind(result retrieval.Result, kind pages.PageKind) bool {
	for _, candidate := range result.Candidates {
		if candidate.Page.Kind == kind {
			return true
		}
	}
	return false
}
