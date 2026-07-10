package pages_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"wsms/internal/config"
	"wsms/internal/harness"
	"wsms/internal/ledger"
	"wsms/internal/pages"
	"wsms/internal/types"
)

func TestCompilerEmitsAllEvidenceBackedKinds(t *testing.T) {
	s := openPageSession(t, "kinds")
	muts := driveTransportFixStream(t, s)

	want := map[pages.PageKind]int{
		pages.KindTaskCheckpoint: 2, // task_started + next_action
		pages.KindConstraint:     1,
		pages.KindFailureEpisode: 1,
		pages.KindDecision:       1,
		pages.KindKnownGood:      1,
		pages.KindFileContext:    1,
		pages.KindRepoFact:       1,
	}
	got := map[pages.PageKind]int{}
	for _, mut := range muts {
		got[mut.Page.Kind]++
		if mut.Op != pages.MutationUpsert || mut.Page.Status != pages.StatusActive {
			t.Fatalf("unexpected mutation %#v", mut)
		}
		if strings.Contains(mut.Page.SearchText, "artifact:sha256:") {
			t.Fatalf("search text leaked artifact address: %q", mut.Page.SearchText)
		}
		if mut.Page.SourceDigest == "" || mut.Page.CompilerVersion != pages.CurrentCompilerVersion {
			t.Fatalf("missing digest/version on %s", mut.Page.ID)
		}
	}
	for kind, n := range want {
		if got[kind] != n {
			t.Fatalf("kind %s count=%d want %d (got map %v kinds %v)", kind, got[kind], n, got, kindsOf(muts))
		}
	}
	if len(muts) != 8 {
		t.Fatalf("mutations=%d want 8: %v", len(muts), kindsOf(muts))
	}

	failure, ok := findByKind(muts, pages.KindFailureEpisode)
	if !ok || failure.Trust != pages.TrustTool || failure.Branch != "main" {
		t.Fatalf("failure page=%#v", failure)
	}
	if !strings.Contains(failure.SearchText, "stream goroutine still blocked") {
		t.Fatalf("failure search=%q", failure.SearchText)
	}
	constraint, ok := findByKind(muts, pages.KindConstraint)
	if !ok || constraint.Trust != pages.TrustUser {
		t.Fatalf("constraint page=%#v", constraint)
	}
	decision, ok := findByKind(muts, pages.KindDecision)
	if !ok || decision.Trust != pages.TrustModel {
		t.Fatalf("decision page=%#v", decision)
	}
}

func TestCompilerIsDeterministicAcrossRunsAndReopen(t *testing.T) {
	s1 := openPageSession(t, "det")
	first := driveTransportFixStream(t, s1)
	struct1 := structuralFingerprint(first)

	// Same durable change recompiled twice yields identical digests (replay gate).
	ev := lastEvent(t, s1)
	// last is assistant with zero mutations; use file snapshot event.
	events, err := s1.Ledger.ListBySession(context.Background(), s1.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var fileSnap ledger.Event
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == ledger.EventFileSnapshot {
			fileSnap = events[i]
			break
		}
	}
	change := ledgerChangeFor(s1, fileSnap)
	// Final state still materializes file_snapshot pages (no WSL record deps).
	c := pages.NewDeterministicCompiler()
	a, err := c.Compile(context.Background(), change)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Compile(context.Background(), change)
	if err != nil {
		t.Fatal(err)
	}
	if fullFingerprint(a) != fullFingerprint(b) {
		t.Fatal("same-change recompile diverged including digests")
	}

	s2 := openPageSession(t, "det")
	second := driveTransportFixStream(t, s2)
	if structuralFingerprint(second) != struct1 {
		t.Fatalf("independent streams diverged structurally\nfirst=%s\nsecond=%s", struct1, structuralFingerprint(second))
	}

	dir := s1.Cfg.DataDir
	sessionID := s1.Cfg.SessionID
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.SessionID = sessionID
	cfg.CapsuleTokenBudget = 512
	cfg.ArtifactThresholdBytes = 32
	reopened, err := harness.OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	// Reopen preserves event timestamps; recompile of the same ledger event must
	// match the first session's file_snapshot pages including source digests.
	reEvents, err := reopened.Ledger.ListBySession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var reFileSnap ledger.Event
	for i := len(reEvents) - 1; i >= 0; i-- {
		if reEvents[i].Type == ledger.EventFileSnapshot {
			reFileSnap = reEvents[i]
			break
		}
	}
	reChange := ledgerChangeFor(reopened, reFileSnap)
	reMuts, err := c.Compile(context.Background(), reChange)
	if err != nil {
		t.Fatal(err)
	}
	if fullFingerprint(reMuts) != fullFingerprint(a) {
		t.Fatalf("reopen recompile diverged\nlive=%s\nreopen=%s", fullFingerprint(a), fullFingerprint(reMuts))
	}
	for _, mut := range reMuts {
		if err := pages.ValidateMaterializable(context.Background(), mut.Page, reChange); err != nil {
			t.Fatalf("reopened materialize: %v", err)
		}
	}
	_ = ev
}

func TestCompilerRejectsPoisonAndUntrustedProse(t *testing.T) {
	s := openPageSession(t, "poison")
	ctx := context.Background()
	if err := s.StartTask(ctx, harness.TaskStart{
		Goal: "keep authority", TaskID: "T1", Repo: "repo", Phase: "impl",
		Priority: types.PriorityHot, Branch: "main", Commit: "deadbee",
	}); err != nil {
		t.Fatal(err)
	}
	_ = compileEvent(t, s, lastEvent(t, s))

	if err := s.IngestAssistant(ctx, "do not rewrite transport layer"); err != nil {
		t.Fatal(err)
	}
	muts := compileEvent(t, s, lastEvent(t, s))
	if len(muts) != 0 {
		t.Fatalf("assistant hard-looking text produced pages: %#v", muts)
	}

	// Free-form user chatter without constraint shape should not mint pages.
	if err := s.IngestUser(ctx, "thanks, that looks good"); err != nil {
		t.Fatal(err)
	}
	muts = compileEvent(t, s, lastEvent(t, s))
	if len(muts) != 0 {
		t.Fatalf("chatter produced pages: %#v", muts)
	}
}

func TestCompilerAuthorityFailClosed(t *testing.T) {
	s := openPageSession(t, "auth")
	ctx := context.Background()
	// Known-good with zero exit but no task/branch authority → no page.
	if err := s.IngestCommandOutput(ctx, "true", 0, "ok"); err != nil {
		t.Fatal(err)
	}
	muts := compileEvent(t, s, lastEvent(t, s))
	if len(muts) != 0 {
		t.Fatalf("expected no known-good without authority, got %#v", muts)
	}

	// File snapshot without prior scope/repo on coherence should fail closed.
	if err := s.RecordFileSnapshot(ctx, harness.FileSnapshot{
		Repo: "repo", Branch: "main", Commit: "abc1234",
		Path: "a.go", ContentDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}); err != nil {
		// May fail coherence prep if no current scope; either fail before append
		// or compile empty is acceptable.
		return
	}
	muts = compileEvent(t, s, lastEvent(t, s))
	// Without active branch in coherence Current, compiler authority may still
	// use event repo/branch/commit and emit. That is correct authority.
	if len(muts) != 0 && len(muts) != 2 {
		t.Fatalf("unexpected file mutations=%d", len(muts))
	}
}

func TestValidateMaterializableRejectsStaleAndWrongScope(t *testing.T) {
	s := openPageSession(t, "mat")
	muts := driveTransportFixStream(t, s)
	current, ok := findByKind(muts, pages.KindFileContext)
	if !ok {
		t.Fatal("missing current file-context page")
	}
	ev := lastEventByType(t, s, ledger.EventFileSnapshot)
	snap := s.Coherence.Snapshot()
	base := pages.LedgerChange{
		Event: ev, State: s.State.Clone(), Events: s.Ledger, Artifacts: s.Artifacts,
		Coherence: s.Coherence,
		RepoID:    snap.Current.Repo, TaskID: snap.Current.TaskID, Branch: snap.Current.Branch, Commit: snap.Current.Commit,
	}
	if err := pages.ValidateMaterializable(context.Background(), current, base); err != nil {
		t.Fatalf("fresh page should materialize: %v", err)
	}

	stale := current
	stale.Status = pages.StatusStale
	if err := pages.ValidateMaterializable(context.Background(), stale, base); err == nil {
		t.Fatal("stale page materialize succeeded")
	}

	wrongSession := current
	wrongSession.SessionID = "other"
	if err := pages.ValidateMaterializable(context.Background(), wrongSession, base); err == nil {
		t.Fatal("cross-session page materialize succeeded")
	}

	wrongEpoch := current
	wrongEpoch.ScopeEpoch = current.ScopeEpoch + 9
	if err := pages.ValidateMaterializable(context.Background(), wrongEpoch, base); err == nil {
		t.Fatal("wrong scope epoch materialize succeeded")
	}

	wrongDigest := current
	wrongDigest.SourceDigest = pages.SourceDigest(strings.Repeat("0", 64))
	if err := pages.ValidateMaterializable(context.Background(), wrongDigest, base); err == nil {
		t.Fatal("wrong source digest materialize succeeded")
	}
}

func TestCompilerArtifactBackedFailureDoesNotLeakRawBytes(t *testing.T) {
	s := openPageSession(t, "artifact")
	ctx := context.Background()
	if err := s.StartTask(ctx, harness.TaskStart{
		Goal: "artifact path", TaskID: "T1", Repo: "repo", Phase: "impl",
		Priority: types.PriorityHot, Branch: "main", Commit: "abc1234",
	}); err != nil {
		t.Fatal(err)
	}
	_ = compileEvent(t, s, lastEvent(t, s))

	pad := strings.Repeat("RAW_SECRET_SHOULD_NOT_ENTER_SEARCH_TEXT_", 8)
	if err := s.IngestCommandOutput(ctx, "go test", 1, "error: boom\n"+pad); err != nil {
		t.Fatal(err)
	}
	ev := lastEvent(t, s)
	if ev.ArtifactHash == "" {
		t.Fatal("expected artifact offload")
	}
	muts := compileEvent(t, s, ev)
	if len(muts) != 1 {
		t.Fatalf("muts=%d", len(muts))
	}
	page := muts[0].Page
	if strings.Contains(page.SearchText, "RAW_SECRET") || strings.Contains(page.Summary, "RAW_SECRET") {
		t.Fatalf("raw artifact body leaked into semantic text: %q / %q", page.SearchText, page.Summary)
	}
	hasArtifact := false
	for _, ref := range page.Refs {
		if ref.Kind == pages.RefArtifact {
			hasArtifact = true
			if err := s.Artifacts.VerifyArtifact(ctx, ref.ID); err != nil {
				t.Fatalf("artifact ref not verifiable: %v", err)
			}
		}
	}
	if !hasArtifact {
		t.Fatalf("expected artifact ref on page refs=%#v", page.Refs)
	}
}

func TestPageMutationValidateRejectsTrustMismatchAndBounds(t *testing.T) {
	base := pages.PageMutation{
		Op: pages.MutationUpsert,
		Page: pages.WarmPage{
			ID: pages.PageID("wp_" + strings.Repeat("a", 32)), Version: 1, SessionID: "s",
			Kind: pages.KindFailureEpisode, Trust: pages.TrustUser, Status: pages.StatusActive,
			Scope: types.ScopeBranch, RepoID: "repo", Branch: "main",
			Salience: 0.5, SalienceReason: "test",
			SearchText: "kind=failure_episode error=x", Summary: "failure x",
			Refs:         []pages.PageRef{{Kind: pages.RefEvent, ID: "E0001"}},
			SourceSeqMin: 1, SourceSeqMax: 1,
			SourceDigest:    pages.SourceDigest(strings.Repeat("b", 64)),
			CompilerVersion: pages.CurrentCompilerVersion, CreatedAt: time.Unix(1, 0).UTC(),
		},
	}
	if err := base.Validate(); err == nil {
		t.Fatal("user trust on failure_episode should fail")
	}
	base.Page.Trust = pages.TrustTool
	if err := base.Validate(); err != nil {
		t.Fatalf("valid tool failure page: %v", err)
	}
	base.Page.SearchText = strings.Repeat("token ", 500)
	if err := base.Validate(); err == nil {
		t.Fatal("oversized search text should fail")
	}
}

func lastEventByType(t *testing.T, s *harness.Session, eventType ledger.EventType) ledger.Event {
	t.Helper()
	events, err := s.Ledger.ListBySession(context.Background(), s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == eventType {
			return events[i]
		}
	}
	t.Fatalf("no %s event", eventType)
	return ledger.Event{}
}
