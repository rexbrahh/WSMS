package harness

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"wsms/internal/coherence"
	"wsms/internal/config"
	"wsms/internal/faults"
	"wsms/internal/ledger"
	"wsms/internal/observers"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

const coherenceDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestCoherenceLiveReplayBranchRevalidationAndRawEvidence(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "coherence-replay"
	cfg.ArtifactThresholdBytes = 32
	cfg.CapsuleTokenBudget = 1024
	cfg.PageFaultTokenBudget = 4096

	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartTask(ctx, TaskStart{Goal: "prove coherence", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "do not discard exact evidence"); err != nil {
		t.Fatal(err)
	}
	raw := "error: branch-specific failure\n" + strings.Repeat("diagnostic bytes ", 16)
	commandEvent, err := s.Append(ctx, ledger.Event{
		Type:    ledger.EventCommandOutput,
		Payload: map[string]any{"cmd": "go test ./...", "exit": 1, "output": raw, "err": "error: branch-specific failure"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if page, err := s.PageFault(ctx, "F1"); err != nil || page == faults.PageMiss {
		t.Fatalf("initial page=(%q,%v)", page, err)
	}
	if _, err := s.BeforeTurn(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Hierarchy.L1Capsule() == "" {
		t.Fatal("expected rendered L1 before transition")
	}

	if err := s.ChangeBranch(ctx, BranchChange{
		Repo: "repo", FromBranch: "main", ToBranch: "feature",
		FromCommit: "aaaaaaa", ToCommit: "bbbbbbb",
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.Hierarchy.L1Capsule(); got != "" {
		t.Fatalf("coherence transition retained old L1: %q", got)
	}
	if task := s.State.ActiveTask(); task == nil || task.Branch != "feature" || task.Commit != "bbbbbbb" {
		t.Fatalf("active task did not expose post-scope: %#v", task)
	}
	if status, revision, _ := s.Coherence.AddressStatus(ledger.TargetRecord, "F1"); status != coherence.StatusStale || revision != 1 {
		t.Fatalf("F1 status=(%q,%d), want stale revision 1", status, revision)
	}
	if page, err := s.PageFault(ctx, "F1"); err != nil || page != faults.PageMiss {
		t.Fatalf("stale page=(%q,%v), want PAGE_MISS", page, err)
	}
	resident, ok := s.Hierarchy.GetPage("F1")
	if !ok || !resident.Stale || resident.Invalidated {
		t.Fatalf("resident page not coherently stale: %#v, found=%v", resident, ok)
	}
	resident.Body = "POISONED OLD RESIDENT BODY"
	s.Hierarchy.PutL2(resident)
	for _, id := range []string{"F1", commandEvent.ID} {
		got, err := s.Tools.ReadRawLog(ctx, id, 4096)
		if err != nil || got != raw {
			t.Fatalf("raw %s=(%q,%v), want byte-exact evidence", id, got, err)
		}
	}
	capsule, err := s.BeforeTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capsule, "branch-specific failure") || !strings.Contains(capsule, "do not discard exact evidence") || !strings.Contains(capsule, "BRANCH: feature") {
		t.Fatalf("coherence-filtered capsule:\n%s", capsule)
	}

	if err := s.RevalidateMemory(ctx, MemoryRevalidation{
		Kind: ledger.TargetRecord, Target: "F1", EvidenceRef: "T1", ExpectedStaleRevision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	page, err := s.PageFault(ctx, "F1")
	if err != nil || page == faults.PageMiss || !strings.Contains(page, "branch-specific failure") || strings.Contains(page, "POISONED") {
		t.Fatalf("revalidated page=(%q,%v)", page, err)
	}
	resident, ok = s.Hierarchy.GetPage("F1")
	if !ok || resident.Stale || resident.Invalidated || resident.Branch != "feature" || resident.Commit != "bbbbbbb" {
		t.Fatalf("revalidated resident metadata=%#v, found=%v", resident, ok)
	}
	liveWSL := wsl.Serialize(s.State.Records())
	liveSnapshot := s.Coherence.Snapshot()
	liveCapsule, err := s.BeforeTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(liveCapsule, "branch-specific failure") {
		t.Fatalf("revalidated failure absent from capsule:\n%s", liveCapsule)
	}
	eventsBefore, err := s.Ledger.ListBySession(ctx, cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, reopened) })
	if got := wsl.Serialize(reopened.State.Records()); got != liveWSL {
		t.Fatalf("replayed WSL differs\nlive:\n%s\nreplay:\n%s", liveWSL, got)
	}
	if got := reopened.Coherence.Snapshot(); !reflect.DeepEqual(got, liveSnapshot) {
		t.Fatalf("replayed coherence differs\nlive=%#v\nreplay=%#v", liveSnapshot, got)
	}
	replayedCapsule, err := reopened.BeforeTurn(ctx)
	if err != nil || replayedCapsule != liveCapsule {
		t.Fatalf("replayed capsule differs err=%v\nlive:\n%s\nreplay:\n%s", err, liveCapsule, replayedCapsule)
	}
	eventsAfter, err := reopened.Ledger.ListBySession(ctx, cfg.SessionID)
	if err != nil || len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("replay appended events: before=%d after=%d err=%v", len(eventsBefore), len(eventsAfter), err)
	}
}

func TestLogicalInvalidationSuppressesReuseButPreservesAuthorizedRaw(t *testing.T) {
	ctx := context.Background()
	s := openCoherenceFailureSession(t, "logical-invalidation")
	rawBefore, err := s.Tools.ReadRawLog(ctx, "F1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetRecord, Target: "F1", Reason: ledger.ReasonSuperseded,
	}); err != nil {
		t.Fatal(err)
	}
	if page, err := s.PageFault(ctx, "F1"); err != nil || page != faults.PageMiss {
		t.Fatalf("invalidated page=(%q,%v), want miss", page, err)
	}
	rawAfter, err := s.Tools.ReadRawLog(ctx, "F1", 4096)
	if err != nil || rawAfter != rawBefore {
		t.Fatalf("logical invalidation changed raw=(%q,%v)", rawAfter, err)
	}
	capsule, err := s.BeforeTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capsule, "branch-specific failure") {
		t.Fatalf("invalidated failure leaked into capsule:\n%s", capsule)
	}
	if _, ok := s.State.Get("F1"); !ok {
		t.Fatal("logical invalidation deleted WSL evidence")
	}
	invalidations := s.State.Invalidated()
	if len(invalidations) != 1 || invalidations[0].Target != "F1" {
		t.Fatalf("visible invalidation records=%#v", invalidations)
	}
	if evidence, ok := s.State.EvidenceID(invalidations[0].IDValue); !ok || evidence == "" {
		t.Fatalf("invalidation provenance=(%q,%v)", evidence, ok)
	}
}

func TestSecurityInvalidationRevokesRawByRecordAndEvidenceEvent(t *testing.T) {
	ctx := context.Background()
	s := openCoherenceFailureSession(t, "security-invalidation")
	events, err := s.Ledger.ListBySession(ctx, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	commandEventID := events[len(events)-1].ID
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetRecord, Target: "F1", Reason: ledger.ReasonSecurityRevoked,
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"F1", commandEventID} {
		got, err := s.Tools.ReadRawLog(ctx, id, 4096)
		if !errors.Is(err, faults.ErrRawEvidenceRevoked) || got != "" {
			t.Fatalf("security raw %s=(%q,%v), want revoked", id, got, err)
		}
	}
}

func TestSecurityInvalidationOfEvidenceEventRevokesDerivedAliases(t *testing.T) {
	ctx := context.Background()
	s := openCoherenceFailureSession(t, "security-event-invalidation")
	events, err := s.Ledger.ListBySession(ctx, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	commandEventID := events[len(events)-1].ID
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetEvent, Target: commandEventID, Reason: ledger.ReasonSecurityRevoked,
	}); err != nil {
		t.Fatal(err)
	}
	if page, err := s.PageFault(ctx, "F1"); err != nil || page != faults.PageMiss {
		t.Fatalf("event-revoked alias page=(%q,%v), want miss", page, err)
	}
	capsule, err := s.BeforeTurn(ctx)
	if err != nil || strings.Contains(capsule, "branch-specific failure") {
		t.Fatalf("event-revoked alias leaked into capsule err=%v\n%s", err, capsule)
	}
	for _, id := range []string{"F1", commandEventID} {
		got, err := s.Tools.ReadRawLog(ctx, id, 4096)
		if !errors.Is(err, faults.ErrRawEvidenceRevoked) || got != "" {
			t.Fatalf("event security raw %s=(%q,%v), want revoked", id, got, err)
		}
	}
}

func TestTerminalRenameDestinationFailsBeforeDurableAppend(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "terminal-rename-destination"
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	if err := s.StartTask(ctx, TaskStart{Goal: "guard rename", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"src/old.go", "src/revoked"} {
		if err := s.RecordFileSnapshot(ctx, FileSnapshot{
			Repo: "repo", Branch: "main", Commit: "aaaaaaa", Path: path, ContentDigest: coherenceDigest,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetPath, Target: "src/revoked", Reason: ledger.ReasonSecurityRevoked,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Ledger.ListBySession(ctx, cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{
		func() error {
			return s.RenameFile(ctx, FileRename{
				Repo: "repo", Branch: "main", FromPath: "src/old.go", ToPath: "src/revoked/dst.go",
			})
		},
		func() error {
			return s.RecordFileSnapshot(ctx, FileSnapshot{
				Repo: "repo", Branch: "main", Commit: "aaaaaaa", Path: "src/revoked/new.go", ContentDigest: coherenceDigest,
			})
		},
	} {
		if err := call(); err == nil {
			t.Fatal("mutation below terminal destination succeeded")
		}
	}
	after, err := s.Ledger.ListBySession(ctx, cfg.SessionID)
	if err != nil || len(after) != len(before) {
		t.Fatalf("rejected rename persisted: before=%d after=%d err=%v", len(before), len(after), err)
	}
	if status, _, _ := s.Coherence.AddressStatus(ledger.TargetPath, "src/revoked"); status != coherence.StatusInvalidated {
		t.Fatalf("terminal destination status=%q", status)
	}
}

func TestMalformedCoherenceEventsFailBeforeAppendAndSessionRemainsUsable(t *testing.T) {
	ctx := context.Background()
	s := openCoherenceFailureSession(t, "preappend-coherence")
	before, err := s.Ledger.ListBySession(ctx, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	bad := []func() error{
		func() error {
			return s.ChangeBranch(ctx, BranchChange{Repo: "repo", FromBranch: "wrong", ToBranch: "feature"})
		},
		func() error {
			return s.RenameFile(ctx, FileRename{Repo: "repo", Branch: "main", FromPath: "../secret", ToPath: "safe"})
		},
		func() error {
			return s.InvalidateMemory(ctx, MemoryInvalidation{Kind: ledger.TargetRecord, Target: "F404", Reason: ledger.ReasonSuperseded})
		},
		func() error {
			return s.RecordFileSnapshot(ctx, FileSnapshot{Repo: "repo", Branch: "main", Commit: "aaaaaaa", Path: "src//a.go", ContentDigest: coherenceDigest})
		},
	}
	for _, call := range bad {
		if err := call(); err == nil {
			t.Fatal("malformed coherence operation succeeded")
		}
	}
	after, err := s.Ledger.ListBySession(ctx, s.Cfg.SessionID)
	if err != nil || len(after) != len(before) {
		t.Fatalf("malformed inputs persisted: before=%d after=%d err=%v", len(before), len(after), err)
	}
	if err := s.IngestAssistant(ctx, "session remains usable"); err != nil {
		t.Fatalf("valid append after prevalidation errors: %v", err)
	}
}

func TestConcurrentFaultsAfterInvalidationCannotObserveStaleHit(t *testing.T) {
	ctx := context.Background()
	s := openCoherenceFailureSession(t, "concurrent-invalidation")
	if err := s.InvalidateMemory(ctx, MemoryInvalidation{
		Kind: ledger.TargetRecord, Target: "F1", Reason: ledger.ReasonSuperseded,
	}); err != nil {
		t.Fatal(err)
	}
	const callers = 24
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page, err := s.PageFault(ctx, "F1")
			if err != nil {
				errs <- err
				return
			}
			if page != faults.PageMiss {
				errs <- errors.New("post-invalidation fault returned a stale hit")
				return
			}
			if _, err := s.Tools.ReadRawLog(ctx, "F1", 4096); err != nil {
				errs <- err
			}
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

func TestCommitAndRenameStalenessRequireExplicitRevalidation(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "commit-rename"
	cfg.CapsuleTokenBudget = 1024
	cfg.PageFaultTokenBudget = 1024
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	if err := s.StartTask(ctx, TaskStart{Goal: "track a file", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFileSnapshot(ctx, FileSnapshot{
		Repo: "repo", Branch: "main", Commit: "aaaaaaa", Path: "src/dir/a.go", ContentDigest: coherenceDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, "go test", 1, "src/dir/a.go:12: error: failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeCommit(ctx, CommitChange{Repo: "repo", Branch: "main", FromCommit: "aaaaaaa", ToCommit: "bbbbbbb"}); err != nil {
		t.Fatal(err)
	}
	if status, revision, _ := s.Coherence.AddressStatus(ledger.TargetRecord, "F1"); status != coherence.StatusStale || revision != 1 {
		t.Fatalf("commit staleness=(%q,%d)", status, revision)
	}
	if err := s.RevalidateMemory(ctx, MemoryRevalidation{
		Kind: ledger.TargetRecord, Target: "F1", EvidenceRef: "T1", ExpectedStaleRevision: 1,
	}); err == nil {
		t.Fatal("revalidation succeeded without current path evidence")
	}
	if err := s.RecordFileSnapshot(ctx, FileSnapshot{
		Repo: "repo", Branch: "main", Commit: "bbbbbbb", Path: "src/dir/a.go", ContentDigest: coherenceDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevalidateMemory(ctx, MemoryRevalidation{
		Kind: ledger.TargetRecord, Target: "F1", EvidenceRef: "T1", ExpectedStaleRevision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameFile(ctx, FileRename{
		Repo: "repo", Branch: "main", FromPath: "src/dir/a.go", ToPath: "src/moved/a.go", ContentDigest: coherenceDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if status, revision, _ := s.Coherence.AddressStatus(ledger.TargetRecord, "F1"); status != coherence.StatusStale || revision != 2 {
		t.Fatalf("rename staleness=(%q,%d), want stale revision 2", status, revision)
	}
	if page, err := s.PageFault(ctx, "F1"); err != nil || page != faults.PageMiss {
		t.Fatalf("renamed old ref page=(%q,%v)", page, err)
	}
	// New-path evidence cannot silently rewrite F1's old exact file reference.
	if err := s.RecordFileSnapshot(ctx, FileSnapshot{
		Repo: "repo", Branch: "main", Commit: "bbbbbbb", Path: "src/moved/a.go", ContentDigest: coherenceDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if status, revision, _ := s.Coherence.AddressStatus(ledger.TargetRecord, "F1"); status != coherence.StatusStale || revision != 2 {
		t.Fatalf("new-path snapshot implicitly revalidated F1=(%q,%d)", status, revision)
	}
}

func TestCrossSessionSameLogicalIDsRemainIsolated(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "wsms-data")
	open := func(id string) *Session {
		cfg := config.Default()
		cfg.DataDir, cfg.SessionID = dataDir, id
		cfg.PageFaultTokenBudget = 1024
		s, err := OpenSession(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeTestSession(t, s) })
		if err := s.StartTask(ctx, TaskStart{Goal: id, Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
			t.Fatal(err)
		}
		if err := s.IngestCommandOutput(ctx, "go test", 1, "error: "+id); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := open("session-a"), open("session-b")
	if err := a.InvalidateMemory(ctx, MemoryInvalidation{Kind: ledger.TargetRecord, Target: "F1", Reason: ledger.ReasonSuperseded}); err != nil {
		t.Fatal(err)
	}
	if page, err := a.PageFault(ctx, "F1"); err != nil || page != faults.PageMiss {
		t.Fatalf("session A page=(%q,%v), want miss", page, err)
	}
	page, err := b.PageFault(ctx, "F1")
	if err != nil || page == faults.PageMiss || !strings.Contains(page, "session-b") {
		t.Fatalf("session B page=(%q,%v), want isolated hit", page, err)
	}
	if status, _, _ := b.Coherence.AddressStatus(ledger.TargetRecord, "F1"); status != coherence.StatusActive {
		t.Fatalf("session B F1 status=%q", status)
	}
}

type rejectingCoherenceObserver struct{ ids *observers.SeqIDGen }

func (o *rejectingCoherenceObserver) Name() string { return "rejecting_coherence" }
func (o *rejectingCoherenceObserver) Handle(context.Context, ledger.Event) ([]wsl.Update, error) {
	return []wsl.Update{
		{Op: "upsert", Record: &wsl.ConstraintRecord{IDValue: o.ids.Next("C"), Strength: types.StrengthHard, Source: types.SourceUser, Text: "candidate only", Scope: types.ScopeTask}},
		{Op: "upsert", Record: &wsl.AvoidRecord{IDValue: "A1", Text: "reject", Ref: "F404"}},
	}, nil
}

func TestRejectedWSLBatchDoesNotAdvanceCoherenceOrNoteEvent(t *testing.T) {
	ctx := context.Background()
	s := openCoherenceFailureSession(t, "coherence-atomicity")
	before := s.Coherence.Snapshot()
	ids := observers.NewSeqIDGen()
	s.Dispatcher.Allocator = ids
	s.Dispatcher.Observers = []observers.Observer{&rejectingCoherenceObserver{ids: ids}}
	stored, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventBranchChange, Repo: "repo", Branch: "feature", Commit: "bbbbbbb",
		Payload: map[string]any{"from_branch": "main", "from_commit": "aaaaaaa"},
	})
	if !errors.Is(err, ErrSessionUnavailable) || stored.ID == "" {
		t.Fatalf("rejected durable transition=(%#v,%v)", stored, err)
	}
	if got := s.Coherence.Snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("rejected batch advanced coherence\nbefore=%#v\nafter=%#v", before, got)
	}
	if _, ok := s.State.KnownIDs()[stored.ID]; ok {
		t.Fatal("rejected batch leaked durable event into WSL known refs")
	}
	if snapshot := ids.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("rejected batch consumed ids: %#v", snapshot)
	}
	if task := s.State.ActiveTask(); task == nil || task.Branch != "main" {
		t.Fatalf("rejected batch changed task: %#v", task)
	}
	page, ok := s.Hierarchy.GetPage("F1")
	if !ok || page.Stale || page.Invalidated {
		t.Fatalf("rejected batch changed hierarchy: %#v found=%v", page, ok)
	}
}

func openCoherenceFailureSession(t *testing.T, id string) *Session {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = id
	cfg.ArtifactThresholdBytes = 32
	cfg.CapsuleTokenBudget = 1024
	cfg.PageFaultTokenBudget = 4096
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "coherence test", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, "go test ./...", 1, "error: branch-specific failure\n"+strings.Repeat("raw detail ", 16)); err != nil {
		t.Fatal(err)
	}
	return s
}
