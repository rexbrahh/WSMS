package harness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"wsms/internal/coherence"
	"wsms/internal/faults"
	"wsms/internal/indexer"
	"wsms/internal/ledger"
	"wsms/internal/pages"
	"wsms/internal/renderer"
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

func TestSemanticSearchTreatsCorruptActiveAuthorityDescriptorAsOperational(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-corrupt-authority-descriptor")
	ctx := context.Background()
	const query = "branch-specific failure"

	before, err := s.SemanticSearch(ctx, query)
	if err != nil || len(before.Materialized) == 0 {
		t.Fatalf("valid authority fixture did not materialize evidence: result=%#v err=%v", before, err)
	}
	target := requireIndexedPageByKind(t, s, query, pages.KindFailureEpisode)
	health, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", health.Path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.ExecContext(ctx, `
UPDATE warm_pages
SET scope = 'unknown_scope'
WHERE session_id = ? AND page_id = ? AND status = ?`,
		s.Cfg.SessionID, string(target.ID), string(pages.StatusActive),
	)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if affected != 1 {
		_ = db.Close()
		t.Fatalf("corrupted descriptor rows=%d want 1", affected)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Index.ActivePageSnapshot(ctx, s.Cfg.SessionID); !errors.Is(err, indexer.ErrAuthorityDescriptorCorrupt) {
		t.Fatalf("active snapshot error=%v, want descriptor corruption sentinel", err)
	}
	got, err := s.SemanticSearch(ctx, query)
	if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("corrupt authority error=%v, want operational index failure", err)
	}
	if len(got.Materialized) != 0 {
		t.Fatalf("corrupt authority released exact evidence: %#v", got.Materialized)
	}
}

func TestSemanticSearchTreatsCorruptKindTrustChaffAsOperational(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-corrupt-kind-trust-chaff")
	ctx := context.Background()
	const query = "branch-specific failure"

	before, err := s.SemanticSearch(ctx, query)
	if err != nil || len(before.Materialized) == 0 {
		t.Fatalf("valid authority fixture did not materialize evidence: result=%#v err=%v", before, err)
	}
	base := requireIndexedPageByKind(t, s, query, pages.KindFailureEpisode)
	chaffCount := semanticCandidateLimit + 1
	chaffIDs := make(map[pages.PageID]struct{}, chaffCount)
	mutations := make([]pages.PageMutation, 0, chaffCount)
	for i := 1; i <= chaffCount; i++ {
		page := base
		page.ID = pages.PageID(fmt.Sprintf("wp_%032x", i))
		page.SearchText, page.Summary = query, query
		page.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", i))
		chaffIDs[page.ID] = struct{}{}
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
	}
	if err := s.Index.Apply(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Index.SearchLexical(ctx, indexer.SearchQuery{
		SessionID: s.Cfg.SessionID, Text: query, Limit: semanticCandidateLimit, ActiveOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != semanticCandidateLimit {
		t.Fatalf("high-ranked chaff hits=%d want %d", len(hits), semanticCandidateLimit)
	}
	for _, hit := range hits {
		if _, ok := chaffIDs[hit.Page.ID]; !ok {
			t.Fatalf("valid target entered chaff-saturated candidate window: %#v", hit)
		}
	}

	health, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", health.Path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.ExecContext(ctx, `
UPDATE warm_pages
SET trust = 'repo'
WHERE session_id = ? AND search_text = ? AND kind = ?`,
		s.Cfg.SessionID, query, string(pages.KindFailureEpisode),
	)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if affected != int64(chaffCount) {
		_ = db.Close()
		t.Fatalf("corrupted kind/trust rows=%d want %d", affected, chaffCount)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Index.ActivePageSnapshot(ctx, s.Cfg.SessionID); !errors.Is(err, indexer.ErrAuthorityDescriptorCorrupt) {
		t.Fatalf("active snapshot error=%v, want descriptor corruption sentinel", err)
	}
	got, err := s.SemanticSearch(ctx, query)
	if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("corrupt kind/trust error=%v, want operational index failure", err)
	}
	if len(got.Materialized) != 0 {
		t.Fatalf("corrupt kind/trust chaff released exact evidence: %#v", got.Materialized)
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

func TestSemanticSearchReturnsExactEvidenceWithoutInjectingL1OrDerivativeProse(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-evidence-boundary")
	ctx := context.Background()
	const resident = "preexisting L1 capsule"
	s.Hierarchy.SetL1Capsule(resident)

	result, err := s.SemanticSearch(ctx, "branch-specific failure")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Materialized) == 0 || len(result.Materialized[0].Evidence) == 0 {
		t.Fatalf("semantic hit returned no exact evidence: %#v", result)
	}
	if got := s.Hierarchy.L1Capsule(); got != resident {
		t.Fatalf("semantic fault mutated L1: got %q want %q", got, resident)
	}
	for _, candidate := range result.Candidates {
		if candidate.Page.SearchText != "" || candidate.Page.Summary != "" || len(candidate.Page.Refs) != 0 {
			t.Fatalf("derivative page prose or refs escaped retrieval: %#v", candidate.Page)
		}
	}
}

func TestSemanticMaterializationBudgetIsCumulative(t *testing.T) {
	page := pages.WarmPage{
		ID:   pages.PageID("wp_" + strings.Repeat("1", 32)),
		Refs: []pages.PageRef{{Kind: pages.RefEvent, ID: "E0001"}},
	}

	t.Run("tokens", func(t *testing.T) {
		s := openCoherenceFailureSession(t, "semantic-budget-tokens")
		calls := 0
		s.Tools = faults.NewTools(nil, func(context.Context, faults.Request) (string, error) {
			calls++
			return "one two three four", nil
		})
		budget := newSemanticMaterializationBudget(7)
		first, used, reason, err := s.materializeSemanticPage(context.Background(), page, budget, 7)
		if err != nil || reason != "" || used != 4 || len(first) != 1 {
			t.Fatalf("first materialization=(%q,%d,%q,%v)", first, used, reason, err)
		}
		if _, _, reason, err := s.materializeSemanticPage(context.Background(), page, budget, 3); err != nil || reason != "budget" {
			t.Fatalf("second materialization reason=%q err=%v, want budget suppression", reason, err)
		}
		if calls != 2 || budget.tokensRemaining != 0 {
			t.Fatalf("calls=%d remaining_tokens=%d", calls, budget.tokensRemaining)
		}
	})

	t.Run("bytes", func(t *testing.T) {
		s := openCoherenceFailureSession(t, "semantic-budget-bytes")
		calls := 0
		s.Tools = faults.NewTools(nil, func(context.Context, faults.Request) (string, error) {
			calls++
			return "four", nil
		})
		budget := newSemanticMaterializationBudget(8)
		budget.bytesRemaining = 3
		if _, _, reason, err := s.materializeSemanticPage(context.Background(), page, budget, 8); err != nil || reason != "budget" {
			t.Fatalf("byte-bounded materialization reason=%q err=%v", reason, err)
		}
		if calls != 1 || budget.bytesRemaining != 0 {
			t.Fatalf("calls=%d remaining_bytes=%d", calls, budget.bytesRemaining)
		}
	})

	t.Run("pages", func(t *testing.T) {
		s := openCoherenceFailureSession(t, "semantic-budget-pages")
		calls := 0
		s.Tools = faults.NewTools(nil, func(context.Context, faults.Request) (string, error) {
			calls++
			return "exact", nil
		})
		budget := newSemanticMaterializationBudget(100)
		for i := 0; i < semanticMaterializeLimit; i++ {
			if _, _, reason, err := s.materializeSemanticPage(context.Background(), page, budget, 100); err != nil || reason != "" {
				t.Fatalf("materialization %d reason=%q err=%v", i, reason, err)
			}
		}
		if _, _, reason, err := s.materializeSemanticPage(context.Background(), page, budget, 100); err != nil || reason != "budget" {
			t.Fatalf("over-limit materialization reason=%q err=%v", reason, err)
		}
		if calls != semanticMaterializeLimit || budget.pagesRemaining != 0 {
			t.Fatalf("calls=%d remaining_pages=%d", calls, budget.pagesRemaining)
		}
	})
}

func TestSemanticSearchSeparatesMaterializationMissFromOperationalFailure(t *testing.T) {
	t.Run("exact ref miss", func(t *testing.T) {
		s := openCoherenceFailureSession(t, "semantic-fault-miss")
		s.Tools = faults.NewTools(nil, func(context.Context, faults.Request) (string, error) {
			return faults.PageMiss, nil
		})
		_, err := s.SemanticSearch(context.Background(), "branch-specific failure")
		if !errors.Is(err, retrieval.ErrSemanticPageMiss) || errors.Is(err, retrieval.ErrIndexUnavailable) {
			t.Fatalf("materialization miss error=%v", err)
		}
	})

	t.Run("resolver error", func(t *testing.T) {
		s := openCoherenceFailureSession(t, "semantic-fault-error")
		s.Tools = faults.NewTools(nil, func(context.Context, faults.Request) (string, error) {
			return "", errors.New("resolver unavailable")
		})
		_, err := s.SemanticSearch(context.Background(), "branch-specific failure")
		if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
			t.Fatalf("operational materialization error=%v", err)
		}
	})
}

func TestSemanticSearchEvidenceStaysWithinConfiguredTokenBudget(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-token-budget")
	s.Cfg.PageFaultTokenBudget = 12
	result, err := s.SemanticSearch(context.Background(), "branch-specific failure")
	if err != nil {
		t.Fatal(err)
	}
	tokens := 0
	for _, materialized := range result.Materialized {
		for _, evidence := range materialized.Evidence {
			tokens += renderer.EstimateTokens(evidence)
		}
	}
	if tokens > s.Cfg.PageFaultTokenBudget || result.Trace.MaterializationTokensUsed > s.Cfg.PageFaultTokenBudget {
		t.Fatalf("evidence tokens=%d trace tokens=%d budget=%d", tokens, result.Trace.MaterializationTokensUsed, s.Cfg.PageFaultTokenBudget)
	}
}

func TestSemanticSearchRetriesOneCoherenceChange(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-coherence-retry")
	ctx := context.Background()
	hookCalls := 0
	var hookErr error
	result, err := s.semanticSearch(ctx, "branch-specific failure", func(attempt int) {
		hookCalls++
		if attempt == 0 {
			hookErr = s.IngestAssistant(ctx, "advance coherence between semantic planning and validation")
		}
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err != nil {
		t.Fatalf("bounded coherence retry failed: %v", err)
	}
	if hookCalls != semanticCoherenceAttempts || len(result.Materialized) == 0 {
		t.Fatalf("hook calls=%d materialized=%#v", hookCalls, result.Materialized)
	}
}

func TestSemanticSearchAbstainsAfterRepeatedCoherenceChange(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-coherence-churn")
	ctx := context.Background()
	hookCalls := 0
	var hookErr error
	_, err := s.semanticSearch(ctx, "branch-specific failure", func(int) {
		hookCalls++
		if hookErr == nil {
			hookErr = s.IngestAssistant(ctx, "continue changing coherence during semantic resolution")
		}
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, retrieval.ErrSemanticPageMiss) || errors.Is(err, retrieval.ErrIndexUnavailable) {
		t.Fatalf("coherence churn error=%v, want semantic abstention", err)
	}
	var miss *retrieval.SemanticMissError
	if !errors.As(err, &miss) || miss.Trace.Abstention != "scope" {
		t.Fatalf("coherence churn lost typed scope trace: %#v", miss)
	}
	if hookCalls != semanticCoherenceAttempts {
		t.Fatalf("hook calls=%d want %d", hookCalls, semanticCoherenceAttempts)
	}
}

func TestSemanticScopeEpochsCoverOnlyCurrentAuthorityGenerations(t *testing.T) {
	snap := coherence.Snapshot{
		Current:      coherence.Scope{Repo: "repo", Branch: "main", Commit: "abc1234"},
		BranchEpochs: map[string]uint64{"repo\x00main": 2, "repo\x00other": 99},
		CommitEpochs: map[string]uint64{"repo\x00main\x00abc1234": 4},
		PathEpochs: map[string]uint64{
			"repo\x00main\x00old.go":    3,
			"repo\x00main\x00new.go":    5,
			"repo\x00other\x00chaff.go": 100,
		},
	}
	got, err := semanticScopeEpochs(snap)
	if err != nil {
		t.Fatal(err)
	}
	want := []pages.ScopeEpoch{0, 2, 4, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("scope epochs=%v want %v", got, want)
	}
}

func TestSemanticSearchScopeEpochFilterPreventsStaleChaffTopKStarvation(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-scope-epoch-chaff")
	ctx := context.Background()
	snapshot := s.Coherence.Snapshot()
	allowed, err := semanticScopeEpochs(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := s.Index.SearchLexical(ctx, indexer.SearchQuery{
		SessionID: s.Cfg.SessionID, RepoID: snapshot.Current.Repo, TaskID: snapshot.Current.TaskID,
		Branch: snapshot.Current.Branch, Commit: snapshot.Current.Commit,
		Text: "branch-specific failure", Limit: 20, ActiveOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var current pages.WarmPage
	for _, candidate := range candidates {
		if candidate.Page.Kind == pages.KindFailureEpisode {
			current = candidate.Page
			break
		}
	}
	if current.ID == "" || !slices.Contains(allowed, current.ScopeEpoch) {
		t.Fatalf("current failure page=%#v allowed_epochs=%v", current, allowed)
	}
	staleEpoch := allowed[len(allowed)-1] + 1
	mutations := make([]pages.PageMutation, 0, semanticCandidateLimit)
	for i := 1; i <= semanticCandidateLimit; i++ {
		chaff := current
		chaff.ID = pages.PageID(fmt.Sprintf("wp_%032x", i))
		chaff.ScopeEpoch = staleEpoch
		chaff.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", i))
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: chaff})
	}
	if err := s.Index.Apply(ctx, mutations); err != nil {
		t.Fatal(err)
	}

	result, err := s.SemanticSearch(ctx, "branch-specific failure")
	if err != nil {
		t.Fatalf("current page starved behind stale epoch chaff: %v", err)
	}
	if len(result.Candidates) == 0 || result.Candidates[0].Page.ID != current.ID {
		t.Fatalf("selected candidates=%#v want current %s", result.Candidates, current.ID)
	}
	for _, candidate := range result.Candidates {
		if candidate.Page.ScopeEpoch == staleEpoch {
			t.Fatalf("stale epoch chaff escaped prelimit filter: %#v", candidate.Page)
		}
	}
}

func TestSemanticSearchExactEligibilityPreventsSecurityInvalidatedRefStarvation(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-exact-authority-chaff")
	ctx := context.Background()
	const query = "must preserve authority snapshot starvation needle"
	if err := s.RecordDecision(ctx, DecisionInput{
		Chosen: "retain transitive decoy", Because: "this decoy depends transitively on rejected failure evidence",
		Scope: types.ScopeTask, AvoidText: "do not reuse revoked evidence", AvoidRef: "F1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, query); err != nil {
		t.Fatal(err)
	}

	decision := requireIndexedPageByKind(t, s, "retain transitive decoy", pages.KindDecision)
	constraint := requireIndexedPageByKind(t, s, query, pages.KindConstraint)
	chaffIDs := make(map[pages.PageID]struct{}, semanticCandidateLimit)
	mutations := make([]pages.PageMutation, 0, semanticCandidateLimit)
	for i := 1; i <= semanticCandidateLimit; i++ {
		chaff := decision
		chaff.ID = pages.PageID(fmt.Sprintf("wp_%032x", i))
		chaff.SearchText = query
		chaff.Summary = query
		chaff.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", i))
		chaffIDs[chaff.ID] = struct{}{}
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: chaff})
	}
	if err := s.Index.Apply(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetRecord, Target: "F1", Reason: ledger.ReasonSecurityRevoked,
	}); err != nil {
		t.Fatal(err)
	}

	authority, err := s.Index.ActivePageSnapshot(ctx, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	activeChaff := 0
	for _, descriptor := range authority.Pages {
		if _, ok := chaffIDs[descriptor.Tuple.PageID]; ok {
			activeChaff++
		}
	}
	if activeChaff != semanticCandidateLimit {
		t.Fatalf("active invalidated-ref chaff=%d want %d", activeChaff, semanticCandidateLimit)
	}

	result, err := s.SemanticSearch(ctx, query)
	if err != nil {
		t.Fatalf("eligible constraint starved behind invalidated-ref chaff: %v", err)
	}
	if !resultContainsPage(result, constraint.ID) {
		t.Fatalf("selected candidates=%#v want eligible constraint %s", result.Candidates, constraint.ID)
	}
	for _, candidate := range result.Candidates {
		if _, ok := chaffIDs[candidate.Page.ID]; ok {
			t.Fatalf("security-invalidated derivative escaped exact eligibility: %#v", candidate)
		}
	}
}

func TestSemanticSearchExactEligibilityRejectsCrossPathEpochCollisionBeforeLimit(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-path-epoch-collision")
	ctx := context.Background()
	const (
		pathA = "src/retired/cache.go"
		pathB = "src/current/cache.go"
		query = pathB
	)
	if err := s.RecordFileSnapshot(ctx, FileSnapshot{
		Repo: "repo", Branch: "main", Commit: "aaaaaaa", Path: pathA,
		ContentDigest: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
	pageA := requireIndexedPageForPath(t, s, pathA)
	if err := s.RecordFileSnapshot(ctx, FileSnapshot{
		Repo: "repo", Branch: "main", Commit: "aaaaaaa", Path: pathB,
		ContentDigest: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatal(err)
	}
	pageB := requireIndexedPageForPath(t, s, pathB)
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetPath, Target: pathA, Reason: ledger.ReasonSecurityRevoked,
	}); err != nil {
		t.Fatal(err)
	}

	mutations := []pages.PageMutation{{Op: pages.MutationUpsert, Page: pageB}}
	chaffIDs := make(map[pages.PageID]struct{}, semanticCandidateLimit)
	for i := 1; i <= semanticCandidateLimit; i++ {
		chaff := pageA
		chaff.ID = pages.PageID(fmt.Sprintf("wp_%032x", i))
		chaff.Status = pages.StatusActive
		chaff.ScopeEpoch = pageB.ScopeEpoch
		chaff.SearchText, chaff.Summary = query, query
		chaff.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", 10_000+i))
		chaffIDs[chaff.ID] = struct{}{}
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: chaff})
	}
	if err := s.Index.Apply(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	authority, err := s.Index.ActivePageSnapshot(ctx, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	activeCollisions := 0
	for _, descriptor := range authority.Pages {
		if _, ok := chaffIDs[descriptor.Tuple.PageID]; ok {
			activeCollisions++
			if descriptor.Tuple.ScopeEpoch != pageB.ScopeEpoch {
				t.Fatalf("chaff tuple epoch=%d want colliding current-path epoch %d", descriptor.Tuple.ScopeEpoch, pageB.ScopeEpoch)
			}
		}
	}
	if activeCollisions != semanticCandidateLimit {
		t.Fatalf("active colliding path chaff=%d want %d", activeCollisions, semanticCandidateLimit)
	}
	_, intent, _, err := s.captureSemanticAttempt(ctx, query, semanticDefaultTokenBudget)
	if err != nil {
		t.Fatal(err)
	}
	if !intentContainsTuple(intent, pageB.ID) {
		t.Fatalf("current path page %s missing from complete eligibility snapshot: %#v", pageB.ID, intent.EligiblePageTuples)
	}

	result, err := s.SemanticSearch(ctx, query)
	if err != nil {
		t.Fatalf("current path page starved behind colliding stale path epochs: %v", err)
	}
	if !resultContainsPath(result, pathB) {
		t.Fatalf("selected candidates=%#v want current path %s", result.Candidates, pathB)
	}
	for _, candidate := range result.Candidates {
		if _, ok := chaffIDs[candidate.Page.ID]; ok {
			t.Fatalf("cross-path epoch collision escaped exact eligibility: %#v", candidate)
		}
	}
}

func TestSemanticSearchCompleteEmptyEligibilityIsGenuineMiss(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-complete-empty-authority")
	ctx := context.Background()
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetRecord, Target: "F1", Reason: ledger.ReasonSecurityRevoked,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetRecord, Target: "T1", Reason: ledger.ReasonSecurityRevoked,
	}); err != nil {
		t.Fatal(err)
	}

	_, intent, _, err := s.captureSemanticAttempt(ctx, "nothing remains eligible", semanticDefaultTokenBudget)
	if err != nil {
		t.Fatal(err)
	}
	if !intent.EligibilityComplete || len(intent.EligiblePageTuples) != 0 {
		t.Fatalf("eligibility snapshot complete=%t tuples=%#v, want complete-empty", intent.EligibilityComplete, intent.EligiblePageTuples)
	}
	if _, err := s.SemanticSearch(ctx, "nothing remains eligible"); !errors.Is(err, retrieval.ErrSemanticPageMiss) || errors.Is(err, retrieval.ErrIndexUnavailable) {
		t.Fatalf("complete-empty authority error=%v, want genuine semantic miss", err)
	}
}

func TestSemanticSearchOverBoundAuthoritySnapshotIsOperational(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-authority-over-bound")
	ctx := context.Background()
	base := requireIndexedPageByKind(t, s, "branch-specific failure", pages.KindFailureEpisode)
	mutations := make([]pages.PageMutation, 0, indexer.MaxAuthoritySnapshotPages+1)
	for i := 1; i <= indexer.MaxAuthoritySnapshotPages+1; i++ {
		page := base
		page.ID = pages.PageID(fmt.Sprintf("wp_%032x", i))
		page.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", i))
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
	}
	if err := s.Index.Apply(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	_, err := s.SemanticSearch(ctx, "branch-specific failure")
	if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("over-bound authority error=%v, want operational failure", err)
	}
}

func TestSemanticSearchClosedAuthoritativeLedgerIsOperational(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-closed-ledger")
	ctx := context.Background()
	if err := s.IngestAssistant(ctx, "advance beyond the failure evidence event"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ledger.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := s.SemanticSearch(ctx, "branch-specific failure")
	if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("closed authoritative ledger error=%v, want operational failure", err)
	}
}

func TestSemanticSearchKnownIndexFailureIsOperationalBeforeEmptySearch(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-known-index-failure")
	s.IndexErr = errors.New("injected page-index catch-up failure")
	_, err := s.SemanticSearch(context.Background(), "zxqv no indexed page contains this phrase")
	if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("known index failure error=%v, want operational failure", err)
	}
}

func TestSemanticSearchLaggingWatermarkIsOperationalBeforeEmptySearch(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-lagging-watermark")
	ctx := context.Background()
	indexDir := s.Index.Dir()
	if err := s.Index.Close(); err != nil {
		t.Fatal(err)
	}
	s.Index = nil
	if err := s.IngestAssistant(ctx, "durable event intentionally omitted from the disposable index"); err != nil {
		t.Fatal(err)
	}
	reopened, err := indexer.Open(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	s.Index = reopened
	watermark, _, err := s.Index.Watermark(ctx, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if watermark >= s.lastEvent.Seq {
		t.Fatalf("fixture watermark=%d latest=%d, want real lag", watermark, s.lastEvent.Seq)
	}
	_, err = s.SemanticSearch(ctx, "zxqv no indexed page contains this phrase")
	if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("lagging watermark error=%v, want operational failure", err)
	}
}

func TestSemanticSearchIndexFailureAfterFreshnessCheckIsOperational(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-index-failure-race")
	_, err := s.semanticSearch(context.Background(), "zxqv no indexed page contains this phrase", func(int) {
		s.appendMu.Lock()
		s.IndexErr = errors.New("injected concurrent page-index failure")
		s.appendMu.Unlock()
	})
	if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("concurrent index failure error=%v, want operational failure", err)
	}
}

func TestSemanticSearchWatermarkChangeAfterFreshnessCheckIsOperational(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-watermark-race")
	ctx := context.Background()
	var hookErr error
	_, err := s.semanticSearch(ctx, "zxqv no indexed page contains this phrase", func(int) {
		seq, version, getErr := s.Index.Watermark(ctx, s.Cfg.SessionID)
		if getErr != nil {
			hookErr = getErr
			return
		}
		hookErr = s.Index.SetWatermark(ctx, s.Cfg.SessionID, seq+1, version)
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("concurrent watermark change error=%v, want operational failure", err)
	}
}

func TestSemanticSearchPostResolutionFreshnessRejectsOtherwiseValidHit(t *testing.T) {
	t.Run("documented index failure", func(t *testing.T) {
		s := openCoherenceFailureSession(t, "semantic-index-failure-hit-race")
		result, err := s.semanticSearch(context.Background(), "branch-specific failure", func(int) {
			s.appendMu.Lock()
			s.IndexErr = errors.New("injected concurrent page-index failure")
			s.appendMu.Unlock()
		})
		if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
			t.Fatalf("concurrent index failure hit error=%v", err)
		}
		if len(result.Materialized) != 0 {
			t.Fatalf("stale projection hit escaped with evidence: %#v", result.Materialized)
		}
	})

	t.Run("watermark drift", func(t *testing.T) {
		s := openCoherenceFailureSession(t, "semantic-watermark-hit-race")
		ctx := context.Background()
		var hookErr error
		result, err := s.semanticSearch(ctx, "branch-specific failure", func(int) {
			seq, version, getErr := s.Index.Watermark(ctx, s.Cfg.SessionID)
			if getErr != nil {
				hookErr = getErr
				return
			}
			hookErr = s.Index.SetWatermark(ctx, s.Cfg.SessionID, seq+1, version)
		})
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
			t.Fatalf("concurrent watermark hit error=%v", err)
		}
		if len(result.Materialized) != 0 {
			t.Fatalf("watermark-drift hit escaped with evidence: %#v", result.Materialized)
		}
	})
}

func TestSemanticSearchMissingDerivativeRefRemainsSemanticMiss(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-missing-ref")
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
	page.SearchText = "kind=failure_episode error=missingrefneedle"
	page.Summary = "missingrefneedle"
	page.Refs = []pages.PageRef{{Kind: pages.RefWSLRecord, ID: "F404"}}
	page.SourceDigest = pages.SourceDigest(strings.Repeat("0", 64))
	if err := s.Index.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	_, err = s.SemanticSearch(ctx, "missingrefneedle")
	if !errors.Is(err, retrieval.ErrSemanticPageMiss) || errors.Is(err, retrieval.ErrIndexUnavailable) {
		t.Fatalf("missing derivative ref error=%v, want semantic miss", err)
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

func resultContainsPage(result retrieval.Result, pageID pages.PageID) bool {
	for _, candidate := range result.Candidates {
		if candidate.Page.ID == pageID {
			return true
		}
	}
	return false
}

func resultContainsPath(result retrieval.Result, path string) bool {
	for _, candidate := range result.Candidates {
		if slices.Contains(candidate.Page.PathScope, path) {
			return true
		}
	}
	return false
}

func intentContainsTuple(intent retrieval.QueryIntent, pageID pages.PageID) bool {
	for _, tuple := range intent.EligiblePageTuples {
		if tuple.PageID == pageID {
			return true
		}
	}
	return false
}

func requireIndexedPageByKind(t *testing.T, s *Session, text string, kind pages.PageKind) pages.WarmPage {
	t.Helper()
	snapshot := s.Coherence.Snapshot()
	candidates, err := s.Index.SearchLexical(context.Background(), indexer.SearchQuery{
		SessionID: s.Cfg.SessionID, RepoID: snapshot.Current.Repo, TaskID: snapshot.Current.TaskID,
		Branch: snapshot.Current.Branch, Commit: snapshot.Current.Commit,
		Text: text, Limit: 100, ActiveOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.Page.Kind == kind {
			return candidate.Page
		}
	}
	t.Fatalf("indexed page kind=%s text=%q not found in %#v", kind, text, candidates)
	return pages.WarmPage{}
}

func requireIndexedPageForPath(t *testing.T, s *Session, path string) pages.WarmPage {
	t.Helper()
	snapshot := s.Coherence.Snapshot()
	candidates, err := s.Index.SearchLexical(context.Background(), indexer.SearchQuery{
		SessionID: s.Cfg.SessionID, RepoID: snapshot.Current.Repo, TaskID: snapshot.Current.TaskID,
		Branch: snapshot.Current.Branch, Commit: snapshot.Current.Commit,
		Text: path, Limit: 100, ActiveOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if slices.Contains(candidate.Page.PathScope, path) {
			return candidate.Page
		}
	}
	t.Fatalf("indexed path page %q not found in %#v", path, candidates)
	return pages.WarmPage{}
}
