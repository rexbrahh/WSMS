package harness

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"wsms/internal/config"
	"wsms/internal/faults"
	"wsms/internal/indexer"
	"wsms/internal/ledger"
	"wsms/internal/memory"
	"wsms/internal/retrieval"
	"wsms/internal/types"
)

func TestPinnedTaskAndHardConstraintSurviveBoundedChurn(t *testing.T) {
	policy := memory.DefaultPolicy()
	policy.MaxResidentPages = 3
	policy.MaxResidentBytes = 32 * 1024
	policy.MaxPinnedPages = 2
	policy.MaxPinnedBytes = 16 * 1024
	policy.MaxPageBytes = 8 * 1024
	policy.MaxGhostEntries = 4
	policy.MaxShadowEntries = 4

	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "bounded-pins")
	cfg.SessionID = "bounded-pins"
	cfg.ResidencyPolicy = policy
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "keep the active anchor", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "do not evict the hard anchor"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 12; i++ {
		if err := s.IngestCommandOutput(ctx, fmt.Sprintf("failing-command-%02d", i), 1, fmt.Sprintf("failure-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := s.ResidencySnapshot()
	if snapshot.ResidentPages > policy.MaxResidentPages || snapshot.PinnedPages != 2 || snapshot.GhostPages > policy.MaxGhostEntries {
		t.Fatalf("bounded residency snapshot = %#v", snapshot)
	}
	for id, fragment := range map[string]string{"T1": "keep the active anchor", "C1": "do not evict the hard anchor"} {
		page, ok := s.Hierarchy.GetPage(id)
		if !ok || !strings.Contains(page.Body, fragment) {
			t.Fatalf("pinned %s = %#v, found=%v", id, page, ok)
		}
		if state := residentStateFor(snapshot, id); state != "pinned" {
			t.Fatalf("resident %s state = %q, want pinned", id, state)
		}
	}
}

func TestSupersededTaskAnchorIsUnpinnedBeforeNewAnchorAdmission(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "anchor-swap")
	cfg.SessionID = "anchor-swap"
	cfg.ResidencyPolicy.MaxPinnedPages = 1
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "first anchor", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if state := residentStateFor(s.ResidencySnapshot(), "T1"); state != "pinned" {
		t.Fatalf("first task state = %q, want pinned", state)
	}
	if err := s.StartTask(ctx, TaskStart{Goal: "second anchor", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	snapshot := s.ResidencySnapshot()
	if state := residentStateFor(snapshot, "T1"); state == "pinned" {
		t.Fatalf("superseded task remained pinned: %#v", snapshot)
	}
	if state := residentStateFor(snapshot, "T2"); state != "pinned" {
		t.Fatalf("new task state = %q, want pinned; snapshot=%#v", state, snapshot)
	}
}

func TestActiveTaskPinHasPriorityOverConstraintsAtCapacity(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "anchor-priority")
	cfg.SessionID = "anchor-priority"
	cfg.ResidencyPolicy.MaxPinnedPages = 1
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "first priority anchor", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "do not displace the active task pin"); err != nil {
		t.Fatal(err)
	}
	if state := residentStateFor(s.ResidencySnapshot(), "T1"); state != "pinned" {
		t.Fatalf("first task state = %q, want pinned", state)
	}
	if state := residentStateFor(s.ResidencySnapshot(), "C1"); state == "pinned" {
		t.Fatalf("constraint displaced active task at pin capacity")
	}

	if err := s.StartTask(ctx, TaskStart{Goal: "replacement priority anchor", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	snapshot := s.ResidencySnapshot()
	if state := residentStateFor(snapshot, "T2"); state != "pinned" {
		t.Fatalf("replacement task state = %q, want pinned; snapshot=%#v", state, snapshot)
	}
	if residentStateFor(snapshot, "T1") == "pinned" || residentStateFor(snapshot, "C1") == "pinned" {
		t.Fatalf("lower-priority anchor consumed sole pin: %#v", snapshot)
	}
	if snapshot.Metrics.PinCapacityRejections == 0 {
		t.Fatalf("constraint pin rejection was not observable: %#v", snapshot.Metrics)
	}
}

func TestDerivedFailureStartsColdUntilActualReuse(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "cold-failure")
	cfg.SessionID = "cold-failure"
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "measure failure reuse", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, "go test ./...", 1, "cold failure signature"); err != nil {
		t.Fatal(err)
	}
	before := s.ResidencySnapshot()
	resident := residentSnapshotFor(t, before, "F1")
	if resident.State != "cold" || resident.Ref || resident.RealUses != 0 {
		t.Fatalf("derived failure residency = %#v, want cold/ref0/use0", resident)
	}

	for use := 1; use <= 2; use++ {
		body, faultErr := s.PageFault(ctx, "F1")
		if faultErr != nil || !strings.Contains(body, "cold failure signature") {
			t.Fatalf("failure use %d = (%q, %v)", use, body, faultErr)
		}
	}
	after := s.ResidencySnapshot()
	resident = residentSnapshotFor(t, after, "F1")
	if resident.State != "hot" || resident.RealUses != 2 {
		t.Fatalf("reused failure residency = %#v, want hot/use2", resident)
	}
	if after.Metrics.DemandPageIns != 0 || after.Metrics.Hits < 2 {
		t.Fatalf("pre-admitted failure metrics = %#v", after.Metrics)
	}
}

func TestScopeTransitionPurgesBodylessEstimatorsAndClearsL1(t *testing.T) {
	policy := memory.DefaultPolicy()
	policy.MaxResidentPages = 2
	policy.MaxPinnedPages = 1
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "scope-estimator-shootdown")
	cfg.SessionID = "scope-estimator-shootdown"
	cfg.ResidencyPolicy = policy
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "scope shootdown", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"G1", "G2"} {
		if err := s.Hierarchy.AdmitDemand(&memory.Page{
			ID: id, PageVersion: 1, SessionID: cfg.SessionID,
			SourceDigest: id + "-digest", CompilerVersion: "compiler-v1", ScopeEpoch: 1,
			Body: "exact " + id,
		}); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}
	if err := s.Hierarchy.ObserveSemanticObservation(memory.SemanticObservation{
		Tuple: memory.PageTuple{
			ID: "S1", PageVersion: 1, SessionID: cfg.SessionID,
			SourceDigest: "shadow-digest", CompilerVersion: "compiler-v1", ScopeEpoch: 1,
		},
		EmbeddingNamespace: "lexical-only",
		CandidateOrdinal:   1,
	}); err != nil {
		t.Fatal(err)
	}
	before := s.ResidencySnapshot()
	if before.GhostPages == 0 || before.ShadowPages == 0 {
		t.Fatalf("shootdown precondition missing metadata: %#v", before)
	}
	s.Hierarchy.SetL1Capsule("stale capsule")
	if err := s.ChangeBranch(ctx, BranchChange{
		Repo: "repo", FromBranch: "main", ToBranch: "feature",
		FromCommit: "aaaaaaa", ToCommit: "bbbbbbb",
	}); err != nil {
		t.Fatal(err)
	}
	after := s.ResidencySnapshot()
	if after.GhostPages != 0 || after.ShadowPages != 0 {
		t.Fatalf("scope transition retained bodyless estimators: %#v", after)
	}
	if after.Metrics.ShadowCensored <= before.Metrics.ShadowCensored {
		t.Fatalf("scope transition did not censor pending shadow: before=%#v after=%#v", before.Metrics, after.Metrics)
	}
	if got := s.Hierarchy.L1Capsule(); got != "" {
		t.Fatalf("scope transition retained L1 %q", got)
	}
}

func TestSemanticSelectedPagesAreDemandResidentAndNeighborsStayShadowOnly(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "semantic-residency")
	cfg.SessionID = "semantic-residency"
	cfg.PageFaultTokenBudget = 4096
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{
		Goal: "shadowmarker taskunique", Repo: "repo", Branch: "main", Commit: "aaaaaaa",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "do not drop shadowmarker constraintunique"); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, "shadowmarker failureunique", 1, "shadowmarker failure signature"); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, "shadowmarker verifiedunique", 0, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDecision(ctx, DecisionInput{
		Chosen: "shadowmarker decisionunique", Because: "keep a measured working set", Scope: types.ScopeTask,
	}); err != nil {
		t.Fatal(err)
	}

	const l1 = "preexisting capsule must remain byte-for-byte stable"
	s.Hierarchy.SetL1Capsule(l1)
	result, err := s.SemanticSearch(ctx, "shadowmarker")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Materialized) == 0 {
		t.Fatalf("semantic result has no exact materialization: %#v", result)
	}
	if got := s.Hierarchy.L1Capsule(); got != l1 {
		t.Fatalf("semantic demand changed L1: got %q want %q", got, l1)
	}

	snapshot := s.ResidencySnapshot()
	selected := make(map[string]struct{}, len(result.Materialized))
	for _, materialized := range result.Materialized {
		id := string(materialized.PageID)
		selected[id] = struct{}{}
		page, ok := s.Hierarchy.GetPage(id)
		if !ok {
			t.Fatalf("selected page %s was not demand-resident", id)
		}
		if got, want := page.Body, strings.Join(materialized.Evidence, "\n\n"); got != want {
			t.Fatalf("resident selected body = %q, want exact evidence %q", got, want)
		}
	}
	if snapshot.ShadowPages == 0 {
		t.Fatalf("expected at least one non-selected shadow observation: trace=%#v snapshot=%#v", result.Trace, snapshot)
	}
	suppressed := make(map[string]struct{}, len(result.Trace.Suppressions))
	for _, suppression := range result.Trace.Suppressions {
		suppressed[string(suppression.PageID)] = struct{}{}
	}
	finalPositions := make(map[string]int, len(result.Trace.Candidates))
	for _, candidate := range result.Trace.Candidates {
		if candidate.FinalPosition > 0 {
			finalPositions[string(candidate.PageID)] = candidate.FinalPosition
		}
	}
	for _, shadow := range snapshot.Shadows {
		if _, wasSelected := selected[shadow.Tuple.ID]; wasSelected {
			t.Fatalf("selected page %s was also counted as speculative shadow", shadow.Tuple.ID)
		}
		if _, wasSuppressed := suppressed[shadow.Tuple.ID]; wasSuppressed {
			t.Fatalf("suppressed page %s was counted as eligible shadow", shadow.Tuple.ID)
		}
		if page, ok := s.Hierarchy.GetPage(shadow.Tuple.ID); ok {
			t.Fatalf("bodyless shadow %s leaked into resident lookup: %#v", shadow.Tuple.ID, page)
		}
		if want := finalPositions[shadow.Tuple.ID]; want <= 0 || shadow.CandidateOrdinal != want {
			t.Fatalf("shadow %s ordinal=%d, want final rank %d", shadow.Tuple.ID, shadow.CandidateOrdinal, want)
		}
	}

	shadow := snapshot.Shadows[0]
	uniqueQuery := uniqueQueryForShadow(t, s, shadow.Tuple.ID)
	second, err := s.SemanticSearch(ctx, uniqueQuery)
	if err != nil {
		t.Fatalf("later explicit demand for shadow %s (%q): %v", shadow.Tuple.ID, uniqueQuery, err)
	}
	if !materializedContains(second, shadow.Tuple.ID) {
		t.Fatalf("later query %q did not demand shadow %s: %#v", uniqueQuery, shadow.Tuple.ID, second.Materialized)
	}
	after := s.ResidencySnapshot()
	if !shadowUseful(after, shadow.Tuple.ID) {
		t.Fatalf("later explicit demand did not mark shadow useful: before=%#v after=%#v", shadow, after.Shadows)
	}
	if got := s.Hierarchy.L1Capsule(); got != l1 {
		t.Fatalf("later semantic demand changed L1: got %q want %q", got, l1)
	}
}

func TestSemanticFinalAuthorityReplacesOlderResidentTuple(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "semantic-authoritative-replace")
	cfg.SessionID = "semantic-authoritative-replace"
	cfg.PageFaultTokenBudget = 4096
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{
		Goal: "replacementmarker exact tuple", Repo: "repo", Branch: "main", Commit: "aaaaaaa",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, "replacementmarker command", 1, "replacementmarker failure"); err != nil {
		t.Fatal(err)
	}

	first, err := s.SemanticSearch(ctx, "replacementmarker")
	if err != nil || len(first.Materialized) == 0 {
		t.Fatalf("first semantic result = (%#v, %v)", first, err)
	}
	id := string(first.Materialized[0].PageID)
	current, ok := s.Hierarchy.GetPage(id)
	if !ok {
		t.Fatalf("selected current page %s not resident", id)
	}
	wantTuple := current.Tuple()
	obsolete := *current
	obsolete.Refs = append([]string(nil), current.Refs...)
	obsolete.Paths = append([]string(nil), current.Paths...)
	obsolete.SourceDigest = "obsolete-" + current.SourceDigest
	s.Hierarchy.Shootdown(id)
	if err := s.Hierarchy.AdmitDemand(&obsolete); err != nil {
		t.Fatalf("seed obsolete tuple: %v", err)
	}

	second, err := s.SemanticSearch(ctx, "replacementmarker")
	if err != nil || !materializedContains(second, id) {
		t.Fatalf("second semantic result for %s = (%#v, %v)", id, second, err)
	}
	replaced, ok := s.Hierarchy.GetPage(id)
	if !ok || replaced.Tuple() != wantTuple {
		t.Fatalf("resident tuple = %#v found=%v, want %#v", replaced, ok, wantTuple)
	}
	if s.ResidencySnapshot().Metrics.AuthoritativeReplacements == 0 {
		t.Fatalf("authoritative replacement was not observable")
	}
}

func TestInvalidationEagerlyShootsDownResidentGhostAndShadowIdentity(t *testing.T) {
	s := openCoherenceFailureSession(t, "residency-invalidation")
	ctx := context.Background()
	if body, err := s.PageFault(ctx, "F1"); err != nil || body == faults.PageMiss {
		t.Fatalf("initial F1 fault = (%q, %v)", body, err)
	}
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetRecord, Target: "F1", Reason: ledger.ReasonUserRejected,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := s.ResidencySnapshot()
	if residencyContains(snapshot, "F1") {
		t.Fatalf("invalidated F1 survived residency shootdown: %#v", snapshot)
	}
	if body, err := s.PageFault(ctx, "F1"); err != nil || body != faults.PageMiss {
		t.Fatalf("invalidated F1 fault = (%q, %v), want PAGE_MISS", body, err)
	}
}

func TestCoherenceChangeBeforeResidencyCommitCannotAdmitOldSemanticTuple(t *testing.T) {
	s := openCoherenceFailureSession(t, "semantic-residency-race")
	ctx := context.Background()
	var firstAttempt []string
	_, err := s.semanticSearchWithHooks(ctx, "branch-specific failure", semanticSearchHooks{
		beforeResidencyCommit: func(attempt int, result retrieval.Result) {
			if attempt != 0 {
				return
			}
			for _, materialized := range result.Materialized {
				firstAttempt = append(firstAttempt, string(materialized.PageID))
			}
			if invalidateErr := s.InvalidateMemory(ctx, MemoryInvalidation{
				Kind: ledger.TargetRecord, Target: "F1", Reason: ledger.ReasonUserRejected,
			}); invalidateErr != nil {
				t.Fatalf("invalidation hook: %v", invalidateErr)
			}
		},
	})
	if !errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("semantic race error = %v, want semantic miss after invalidation", err)
	}
	if len(firstAttempt) == 0 {
		t.Fatal("test seam did not capture a first-attempt exact page")
	}
	snapshot := s.ResidencySnapshot()
	for _, id := range firstAttempt {
		if residencyContains(snapshot, id) {
			t.Fatalf("old first-attempt tuple %s committed after coherence change: %#v", id, snapshot)
		}
	}
}

func TestBenignAppendDuringResidencyCommitDoesNotDropValidSemanticResult(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "residency-commit-retry")
	cfg.SessionID = "residency-commit-retry"
	cfg.PageFaultTokenBudget = 4096
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "retrymarker taskunique", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "do not drop retrymarker constraintunique"); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, "retrymarker failureunique", 1, "retrymarker failure signature"); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, "retrymarker verifiedunique", 0, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDecision(ctx, DecisionInput{
		Chosen: "retrymarker decisionunique", Because: "keep a measured working set", Scope: types.ScopeTask,
	}); err != nil {
		t.Fatal(err)
	}

	firstAttemptMaterialized := -1
	injected := false
	result, err := s.semanticSearchWithHooks(ctx, "retrymarker", semanticSearchHooks{
		beforeResidencyCommit: func(attempt int, r retrieval.Result) {
			if attempt != 0 {
				return
			}
			firstAttemptMaterialized = len(r.Materialized)
			// A benign, in-scope append bumps the durable source sequence and
			// forces the commit-time freshness retry without changing coherence
			// scope. The retry must re-materialize against a fresh budget and
			// still return the valid, exact-evidence result rather than a
			// spurious semantic miss caused by an exhausted shared budget.
			if ingestErr := s.IngestCommandOutput(ctx, "retrymarker benignprobe", 0, "ok"); ingestErr != nil {
				t.Errorf("benign append hook: %v", ingestErr)
			}
			injected = true
		},
	})
	if !injected {
		t.Fatal("test seam never ran its first-attempt append")
	}
	if firstAttemptMaterialized < semanticMaterializeLimit {
		t.Fatalf("first attempt materialized %d pages; the full budget (%d) must be drained to exercise the retry", firstAttemptMaterialized, semanticMaterializeLimit)
	}
	if err != nil {
		t.Fatalf("commit-retry after a benign in-scope append dropped a valid result: %v", err)
	}
	if len(result.Materialized) == 0 {
		t.Fatalf("commit-retry returned no exact materialization: %#v", result)
	}
}

func TestResidencyOverboundIsVisibleWithoutChangingExactFaultResponse(t *testing.T) {
	policy := memory.DefaultPolicy()
	policy.MaxResidentPages = 2
	policy.MaxResidentBytes = 512
	policy.MaxPinnedPages = 0
	policy.MaxPinnedBytes = 0
	policy.MaxPageBytes = 64
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "residency-overbound")
	cfg.SessionID = "residency-overbound"
	cfg.PageFaultTokenBudget = 4096
	cfg.ResidencyPolicy = policy
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "overbound test", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	const secretMarker = "exact-overbound-evidence-must-return"
	text := "do not discard " + secretMarker + " " + strings.Repeat("payload ", 40)
	if err := s.IngestUser(ctx, text); err != nil {
		t.Fatal(err)
	}
	body, err := s.PageFault(ctx, "C1")
	if err != nil || !strings.Contains(body, secretMarker) {
		t.Fatalf("overbound exact fault = (%q, %v)", body, err)
	}
	snapshot := s.ResidencySnapshot()
	if snapshot.Metrics.PageTooLargeRejections == 0 {
		t.Fatalf("overbound rejection was not observable: %#v", snapshot.Metrics)
	}
	if state := residentStateFor(snapshot, "C1"); state != "" {
		t.Fatalf("overbound C1 became resident in state %q", state)
	}
	for _, event := range s.ResidencyTrace() {
		if strings.Contains(fmt.Sprintf("%#v", event), secretMarker) {
			t.Fatalf("residency trace leaked exact body: %#v", event)
		}
	}
}

func residentStateFor(snapshot memory.Snapshot, id string) string {
	for _, resident := range snapshot.Resident {
		if resident.Tuple.ID == id {
			return resident.State
		}
	}
	return ""
}

func residentSnapshotFor(t *testing.T, snapshot memory.Snapshot, id string) memory.ResidentSnapshot {
	t.Helper()
	for _, resident := range snapshot.Resident {
		if resident.Tuple.ID == id {
			return resident
		}
	}
	t.Fatalf("resident %s not found in %#v", id, snapshot)
	return memory.ResidentSnapshot{}
}

func residencyContains(snapshot memory.Snapshot, id string) bool {
	for _, resident := range snapshot.Resident {
		if resident.Tuple.ID == id {
			return true
		}
	}
	for _, ghost := range snapshot.Ghosts {
		if ghost.Tuple.ID == id {
			return true
		}
	}
	for _, shadow := range snapshot.Shadows {
		if shadow.Tuple.ID == id {
			return true
		}
	}
	return false
}

func shadowUseful(snapshot memory.Snapshot, id string) bool {
	for _, shadow := range snapshot.Shadows {
		if shadow.Tuple.ID == id {
			return shadow.Useful
		}
	}
	return false
}

func materializedContains(result retrieval.Result, id string) bool {
	for _, materialized := range result.Materialized {
		if string(materialized.PageID) == id {
			return true
		}
	}
	return false
}

func uniqueQueryForShadow(t *testing.T, s *Session, id string) string {
	t.Helper()
	for _, query := range []string{"taskunique", "constraintunique", "failureunique", "verifiedunique", "decisionunique"} {
		candidates, err := s.Index.SearchLexical(context.Background(), indexSearchForResidency(s, query))
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range candidates {
			if string(candidate.Page.ID) == id {
				return query
			}
		}
	}
	t.Fatalf("no unique query maps to shadow %s", id)
	return ""
}

func indexSearchForResidency(s *Session, text string) indexer.SearchQuery {
	snapshot := s.Coherence.Snapshot()
	return indexer.SearchQuery{
		SessionID: s.Cfg.SessionID, RepoID: snapshot.Current.Repo, TaskID: snapshot.Current.TaskID,
		Branch: snapshot.Current.Branch, Commit: snapshot.Current.Commit,
		Text: text, Limit: 20, ActiveOnly: true,
	}
}
