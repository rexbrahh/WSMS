package faults

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wsms/internal/artifacts"
	"wsms/internal/coherence"
	"wsms/internal/ledger"
	"wsms/internal/memory"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

func TestPageMiss(t *testing.T) {
	r := &Resolver{
		State:     wsl.NewWorkingState(),
		Hierarchy: memory.NewHierarchy(),
	}
	got, err := r.Resolve(context.Background(), Request{Kind: "page", ID: "P404"})
	if err != nil {
		t.Fatal(err)
	}
	if got != PageMiss {
		t.Fatalf("got %q", got)
	}
}

func TestPageHitFailure(t *testing.T) {
	st := wsl.NewWorkingState()
	_ = st.Apply(&wsl.FailureRecord{
		IDValue: "F18",
		Cmd:     "go test",
		Exit:    1,
		Err:     "stream goroutine still blocked",
	})
	r := &Resolver{State: st, Hierarchy: memory.NewHierarchy()}
	got, err := r.Resolve(context.Background(), Request{Kind: "page", ID: "F18"})
	if err != nil {
		t.Fatal(err)
	}
	if got == PageMiss {
		t.Fatal("unexpected miss")
	}
	if !contains(got, "stream goroutine still blocked") {
		t.Fatalf("got %q", got)
	}
}

func TestKnownPageFaultAdmitsThenPromotesOnRepeatedUse(t *testing.T) {
	state := wsl.NewWorkingState()
	if err := state.Apply(&wsl.ConstraintRecord{
		IDValue: "C1", Strength: types.StrengthHard, Source: types.SourceUser,
		Text: "keep exact demand pages bounded", Scope: types.ScopeTask,
	}); err != nil {
		t.Fatal(err)
	}
	hierarchy := memory.NewHierarchy()
	resolver := &Resolver{State: state, Hierarchy: hierarchy}

	first, err := resolver.Resolve(context.Background(), Request{Kind: "page", ID: "C1", Budget: 256})
	if err != nil || !strings.Contains(first, "keep exact demand pages bounded") {
		t.Fatalf("first fault = (%q, %v)", first, err)
	}
	firstSnapshot := hierarchy.Snapshot()
	if firstSnapshot.ColdPages != 1 || firstSnapshot.HotPages != 0 || len(firstSnapshot.Resident) != 1 || firstSnapshot.Resident[0].RealUses != 1 {
		t.Fatalf("first demand snapshot = %#v", firstSnapshot)
	}

	second, err := resolver.Resolve(context.Background(), Request{Kind: "page", ID: "C1", Budget: 256})
	if err != nil || second != first {
		t.Fatalf("second fault = (%q, %v), want first body %q", second, err, first)
	}
	secondSnapshot := hierarchy.Snapshot()
	if secondSnapshot.HotPages != 1 || secondSnapshot.ColdPages != 0 || len(secondSnapshot.Resident) != 1 || secondSnapshot.Resident[0].RealUses != 2 {
		t.Fatalf("second demand snapshot = %#v", secondSnapshot)
	}
	if secondSnapshot.Metrics.DemandPageIns != 1 || secondSnapshot.Metrics.Hits != 1 {
		t.Fatalf("fault metrics = %#v, want one page-in then one L2 hit", secondSnapshot.Metrics)
	}
}

func TestMutableSameIDRecordMustMatchCurrentMaterialization(t *testing.T) {
	state := wsl.NewWorkingState()
	if err := state.Apply(&wsl.NextRecord{Action: "inspect", Target: "old-target"}); err != nil {
		t.Fatal(err)
	}
	hierarchy := memory.NewHierarchy()
	resolver := &Resolver{State: state, Hierarchy: hierarchy}

	first, err := resolver.Resolve(context.Background(), Request{Kind: "page", ID: "next", Budget: 256})
	if err != nil || !strings.Contains(first, "old-target") {
		t.Fatalf("first next fault = (%q, %v)", first, err)
	}
	if err := state.Apply(&wsl.NextRecord{Action: "verify", Target: "new-target"}); err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Resolve(context.Background(), Request{Kind: "page", ID: "next", Budget: 256})
	if err != nil || !strings.Contains(second, "new-target") || strings.Contains(second, "old-target") {
		t.Fatalf("updated next fault = (%q, %v)", second, err)
	}
	afterRefresh := hierarchy.Snapshot()
	if afterRefresh.Metrics.DemandPageIns != 2 || afterRefresh.Metrics.Hits != 0 {
		t.Fatalf("same-ID refresh metrics = %#v, want two page-ins and no false hit", afterRefresh.Metrics)
	}

	third, err := resolver.Resolve(context.Background(), Request{Kind: "page", ID: "next", Budget: 256})
	if err != nil || third != second {
		t.Fatalf("stable next fault = (%q, %v), want %q", third, err, second)
	}
	afterHit := hierarchy.Snapshot()
	if afterHit.Metrics.DemandPageIns != 2 || afterHit.Metrics.Hits != 1 {
		t.Fatalf("stable current hit metrics = %#v", afterHit.Metrics)
	}
}

func TestBodyClearedResidentFallsThroughToL4Rematerialization(t *testing.T) {
	state := wsl.NewWorkingState()
	if err := state.Apply(&wsl.FailureRecord{
		IDValue: "F1",
		Cmd:     "go test ./...",
		Exit:    1,
		Err:     "current nonempty L4 evidence",
	}); err != nil {
		t.Fatal(err)
	}
	hierarchy := memory.NewHierarchy()
	hierarchy.PutL2(&memory.Page{ID: "F1", Summary: "poison summary", Body: "poison body", Refs: []string{"F1"}})
	hierarchy.Reconcile(func(*memory.Page) memory.PageCoherence {
		return memory.PageCoherence{Stale: true, StaleRevision: 1}
	})
	hierarchy.Reconcile(func(*memory.Page) memory.PageCoherence {
		return memory.PageCoherence{}
	})
	if page, ok := hierarchy.GetPage("F1"); !ok || page.Body != "" || page.Summary != "" {
		t.Fatalf("precondition body-cleared resident=%#v ok=%v", page, ok)
	}
	resolver := &Resolver{State: state, Hierarchy: hierarchy}
	got, err := resolver.Resolve(context.Background(), Request{Kind: "page", ID: "F1", Budget: 512})
	if err != nil || !strings.Contains(got, "current nonempty L4 evidence") || strings.Contains(got, "poison") {
		t.Fatalf("rematerialized fault=(%q,%v)", got, err)
	}
	page, ok := hierarchy.GetPage("F1")
	if !ok || page.Body == "" || strings.Contains(page.Body, "poison") {
		t.Fatalf("resident not repopulated with current body: %#v ok=%v", page, ok)
	}
}

func TestInvalidatedPageDependencyCannotFallBackToWSL(t *testing.T) {
	coherent := coherence.NewState()
	apply := func(ev ledger.Event, updates []wsl.Update) {
		t.Helper()
		if err := coherent.WithCandidate(ev, func(candidate *coherence.Candidate) error {
			return candidate.BindUpdates(updates)
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply(ledger.Event{
		ID: "E0001", Type: ledger.EventTaskStarted, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"goal": "dependency gate"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "dependency gate", Branch: "main", Commit: "aaaaaaa"}}})
	page := &wsl.PageRecord{IDValue: "P1", KindStr: "failure_episode", Summary: "cached failure", Refs: "F1", Scope: types.ScopeTask}
	apply(ledger.Event{ID: "E0002", Type: ledger.EventDecision}, []wsl.Update{
		{Record: &wsl.FailureRecord{IDValue: "F1", Cmd: "go test", Exit: 1, Err: "failed"}},
		{Record: page},
	})

	state := wsl.NewWorkingState()
	if err := state.Apply(page); err != nil {
		t.Fatal(err)
	}
	resolver := &Resolver{State: state, Hierarchy: memory.NewHierarchy(), Coherence: coherent}
	if got, err := resolver.Resolve(context.Background(), Request{Kind: "page", ID: "P1"}); err != nil || got == PageMiss {
		t.Fatalf("current page=(%q,%v), want WSL hit", got, err)
	}
	apply(ledger.Event{
		ID: "E0003", Type: ledger.EventMemoryInvalidated,
		Payload: map[string]any{
			"target_kind": string(ledger.TargetRecord), "target": "F1", "reason": string(ledger.ReasonSuperseded),
		},
	}, nil)
	if got, err := resolver.Resolve(context.Background(), Request{Kind: "page", ID: "P1"}); err != nil || got != PageMiss {
		t.Fatalf("invalidated dependency fallback=(%q,%v), want PAGE_MISS", got, err)
	}
}

func TestRawLogLiteralIsSupported(t *testing.T) {
	st := wsl.NewWorkingState()
	if err := st.Apply(&wsl.FailureRecord{
		IDValue: "F1",
		Cmd:     "go test",
		Exit:    1,
		Err:     "failed",
		Raw:     "literal raw diagnostic",
	}); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{State: st, Hierarchy: memory.NewHierarchy()}
	got, err := r.Resolve(context.Background(), Request{Kind: "raw_log", ID: "F1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "literal raw diagnostic" {
		t.Fatalf("got %q", got)
	}
}

func TestRawLogFailureWithoutEvidenceIsIntegrityError(t *testing.T) {
	st := wsl.NewWorkingState()
	if err := st.Apply(&wsl.FailureRecord{
		IDValue: "F1",
		Cmd:     "go test ./...",
		Exit:    1,
		Err:     "failed",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := (&Resolver{State: st}).Resolve(
		context.Background(),
		Request{Kind: "raw_log", ID: "F1"},
	)
	if !errors.Is(err, ErrMissingRawEvidence) || got == PageMiss {
		t.Fatalf("got (%q, %v), want missing-raw-evidence integrity error", got, err)
	}
}

func TestRawLogAbsentFailureIsPageMiss(t *testing.T) {
	got, err := (&Resolver{State: wsl.NewWorkingState()}).Resolve(
		context.Background(),
		Request{Kind: "raw_log", ID: "F404"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != PageMiss {
		t.Fatalf("got %q, want %q", got, PageMiss)
	}
}

func TestRawLogFailureUsesProvenanceForInlineOutput(t *testing.T) {
	ledgerStore, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"), "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ledgerStore.Close(); err != nil {
			t.Errorf("close ledger: %v", err)
		}
	})
	const output = "--- FAIL: TestWorkerShutdown (0.01s)\nworker still running\n"
	stored, err := ledgerStore.Append(context.Background(), ledger.Event{
		Type: ledger.EventCommandOutput,
		Payload: map[string]any{
			"cmd":    "go test ./worker -run TestWorkerShutdown",
			"exit":   1,
			"output": output,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	st := wsl.NewWorkingState()
	st.NoteEvent(stored.ID)
	if err := st.ApplyUpdates([]wsl.Update{{
		Op: "upsert",
		Record: &wsl.FailureRecord{
			IDValue: "F1",
			Cmd:     "go test ./worker -run TestWorkerShutdown",
			Exit:    1,
			Err:     "worker still running",
		},
		EvidenceID: stored.ID,
	}}); err != nil {
		t.Fatal(err)
	}

	r := &Resolver{State: st, Ledger: ledgerStore}
	got, err := r.Resolve(context.Background(), Request{Kind: "raw_log", ID: "F1", Budget: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if got != output {
		t.Fatalf("got inline output %q, want exact %q", got, output)
	}
}

func TestRawLogFailureMissingProvenanceEventIsError(t *testing.T) {
	ledgerStore, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"), "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ledgerStore.Close(); err != nil {
			t.Errorf("close ledger: %v", err)
		}
	})

	st := wsl.NewWorkingState()
	st.NoteEvent("E9999")
	if err := st.ApplyUpdates([]wsl.Update{{
		Op:         "upsert",
		Record:     &wsl.FailureRecord{IDValue: "F1", Cmd: "go test", Exit: 1, Err: "failed"},
		EvidenceID: "E9999",
	}}); err != nil {
		t.Fatal(err)
	}

	got, err := (&Resolver{State: st, Ledger: ledgerStore}).Resolve(
		context.Background(),
		Request{Kind: "raw_log", ID: "F1"},
	)
	if err == nil || got == PageMiss {
		t.Fatalf("got (%q, %v), want missing referenced-event error", got, err)
	}
}

func TestRawLogRejectsMalformedArtifactRef(t *testing.T) {
	st := wsl.NewWorkingState()
	if err := st.Apply(&wsl.FailureRecord{
		IDValue: "F1",
		Cmd:     "go test",
		Exit:    1,
		Err:     "failed",
		Raw:     artifacts.RefPrefix + "../../etc/passwd",
	}); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{State: st, Hierarchy: memory.NewHierarchy()}
	got, err := r.Resolve(context.Background(), Request{Kind: "raw_log", ID: "F1"})
	if !errors.Is(err, artifacts.ErrInvalidRef) {
		t.Fatalf("got (%q, %v), want invalid reference error", got, err)
	}
}

func TestRawLogMissingReferencedArtifactIsError(t *testing.T) {
	store := openTestArtifactStore(t, filepath.Join(t.TempDir(), "artifacts"))
	st := wsl.NewWorkingState()
	if err := st.Apply(&wsl.FailureRecord{
		IDValue: "F1",
		Cmd:     "go test",
		Exit:    1,
		Err:     "failed",
		Raw:     artifacts.Ref(strings.Repeat("0", 64)),
	}); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{State: st, Hierarchy: memory.NewHierarchy(), Artifacts: store}
	got, err := r.Resolve(context.Background(), Request{Kind: "raw_log", ID: "F1"})
	if !errors.Is(err, artifacts.ErrArtifactNotFound) {
		t.Fatalf("got (%q, %v), want missing artifact error", got, err)
	}
}

func TestRawLogCorruptReferencedArtifactIsError(t *testing.T) {
	store := openTestArtifactStore(t, filepath.Join(t.TempDir(), "artifacts"))
	meta, err := store.Put([]byte("exact raw evidence"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta.Path, []byte("corrupt raw evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := wsl.NewWorkingState()
	if err := st.Apply(&wsl.FailureRecord{
		IDValue: "F1",
		Cmd:     "go test",
		Exit:    1,
		Err:     "failed",
		Raw:     artifacts.Ref(meta.SHA256),
	}); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{State: st, Hierarchy: memory.NewHierarchy(), Artifacts: store}
	got, err := r.Resolve(context.Background(), Request{Kind: "raw_log", ID: "F1"})
	if !errors.Is(err, artifacts.ErrArtifactCorrupt) {
		t.Fatalf("got (%q, %v), want corrupt artifact error", got, err)
	}
}

func TestRawLogAbsentEventIsPageMiss(t *testing.T) {
	ledgerStore, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"), "session")
	if err != nil {
		t.Fatal(err)
	}
	defer ledgerStore.Close()
	r := &Resolver{Ledger: ledgerStore}
	got, err := r.Resolve(context.Background(), Request{Kind: "raw_log", ID: "E404"})
	if err != nil {
		t.Fatal(err)
	}
	if got != PageMiss {
		t.Fatalf("got %q, want %q", got, PageMiss)
	}
}

func TestRawLogPropagatesLedgerError(t *testing.T) {
	ledgerStore, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"), "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := ledgerStore.Close(); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Ledger: ledgerStore}
	got, err := r.Resolve(context.Background(), Request{Kind: "raw_log", ID: "E0001"})
	if err == nil || got == PageMiss {
		t.Fatalf("got (%q, %v), want propagated ledger error", got, err)
	}
}

func TestFileSliceFaultRequiresAuthorizedRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readable.txt")
	if err := os.WriteFile(path, []byte("must remain inaccessible"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (&Resolver{}).Resolve(context.Background(), Request{
		Kind:  "file_slice",
		Path:  path,
		Start: 1,
		End:   2,
	})
	if !errors.Is(err, ErrFileSliceUnauthorized) || got == PageMiss {
		t.Fatalf("got (%q, %v), want authorization error", got, err)
	}
}

func TestReadFileSliceLowLevelHelper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileSlice(path, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got != "two\nthree" {
		t.Fatalf("got %q", got)
	}
}

func TestUnknownFaultKindIsError(t *testing.T) {
	got, err := (&Resolver{}).Resolve(context.Background(), Request{Kind: "unknown"})
	if err == nil || got == PageMiss {
		t.Fatalf("got (%q, %v), want invalid request error", got, err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}

func openTestArtifactStore(t *testing.T, dir string) *artifacts.Store {
	t.Helper()
	store, err := artifacts.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close artifact store: %v", err)
		}
	})
	return store
}
