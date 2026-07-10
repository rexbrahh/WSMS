package faults

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wsms/internal/artifacts"
	"wsms/internal/ledger"
	"wsms/internal/memory"
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
