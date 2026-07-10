package coherence

import (
	"errors"
	"reflect"
	"testing"

	"wsms/internal/ledger"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

func TestPrepareIsPureAndBranchRevalidationUsesCAS(t *testing.T) {
	state := NewState()
	applyCandidate(t, state, ledger.Event{
		ID: "E0001", Type: ledger.EventTaskStarted, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"goal": "test coherence"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "test coherence", Branch: "main", Commit: "aaaaaaa"}}})
	applyCandidate(t, state, ledger.Event{
		ID: "E0002", Type: ledger.EventCommandOutput,
		Payload: map[string]any{"cmd": "go test", "exit": 1, "output": "failed"},
	}, []wsl.Update{{Record: &wsl.FailureRecord{IDValue: "F1", Cmd: "go test", Exit: 1, Err: "failed"}}})

	before := state.Snapshot()
	branch := ledger.Event{
		ID: "E0003", Type: ledger.EventBranchChange, Repo: "repo", Branch: "feature", Commit: "bbbbbbb",
		Payload: map[string]any{"from_branch": "main", "from_commit": "aaaaaaa"},
	}
	candidate, err := state.Prepare(branch)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, state.Snapshot()) {
		t.Fatal("Prepare mutated committed coherence state")
	}
	if err := candidate.BindUpdates([]wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "test coherence", Branch: "feature", Commit: "bbbbbbb"}}}); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit(candidate); err != nil {
		t.Fatal(err)
	}
	if status, revision, ok := state.AddressStatus(ledger.TargetRecord, "F1"); !ok || status != StatusStale || revision != 1 {
		t.Fatalf("F1 after branch change=(%q,%d,%v), want stale revision 1", status, revision, ok)
	}

	applyCandidate(t, state, ledger.Event{
		ID: "E0004", Type: ledger.EventMemoryRevalidated,
		Payload: map[string]any{
			"target_kind": string(ledger.TargetRecord), "target": "F1",
			"evidence_ref": "T1", "expected_stale_revision": 1,
		},
	}, nil)
	if status, revision, _ := state.AddressStatus(ledger.TargetRecord, "F1"); status != StatusActive || revision != 1 {
		t.Fatalf("F1 after revalidation=(%q,%d), want active revision 1", status, revision)
	}

	applyCandidate(t, state, ledger.Event{
		ID: "E0005", Type: ledger.EventBranchChange, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"from_branch": "feature", "from_commit": "bbbbbbb"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "test coherence", Branch: "main", Commit: "aaaaaaa"}}})
	if status, revision, _ := state.AddressStatus(ledger.TargetRecord, "F1"); status != StatusStale || revision != 2 {
		t.Fatalf("F1 after ABA branch return=(%q,%d), want stale revision 2", status, revision)
	}
	staleBefore := state.Snapshot()
	err = state.WithCandidate(ledger.Event{
		ID: "E0006", Type: ledger.EventMemoryRevalidated,
		Payload: map[string]any{
			"target_kind": string(ledger.TargetRecord), "target": "F1",
			"evidence_ref": "T1", "expected_stale_revision": 1,
		},
	}, func(*Candidate) error { return nil })
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("old CAS error=%v, want ErrStaleRevision", err)
	}
	if !reflect.DeepEqual(staleBefore, state.Snapshot()) {
		t.Fatal("failed CAS mutated coherence state")
	}
}

func TestTerminalInvalidationAndRawAuthorizationFollowProvenance(t *testing.T) {
	for _, tc := range []struct {
		reason     ledger.InvalidationReason
		rawAllowed bool
	}{
		{ledger.ReasonSuperseded, true},
		{ledger.ReasonUserRejected, true},
		{ledger.ReasonSourceDeleted, true},
		{ledger.ReasonPolicyChanged, false},
		{ledger.ReasonSecurityRevoked, false},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			state := seededFailureState(t)
			applyCandidate(t, state, ledger.Event{
				ID: "E0003", Type: ledger.EventMemoryInvalidated,
				Payload: map[string]any{
					"target_kind": string(ledger.TargetRecord), "target": "F1", "reason": string(tc.reason),
				},
			}, nil)
			if status, _, _ := state.AddressStatus(ledger.TargetRecord, "F1"); status != StatusInvalidated {
				t.Fatalf("status=%q, want invalidated", status)
			}
			if got := state.RawAllowed("F1"); got != tc.rawAllowed {
				t.Fatalf("RawAllowed(F1)=%v, want %v", got, tc.rawAllowed)
			}
			if got := state.RawAllowed("E0002"); got != tc.rawAllowed {
				t.Fatalf("RawAllowed(E0002)=%v, want linked result %v", got, tc.rawAllowed)
			}
			err := state.WithCandidate(ledger.Event{
				ID: "E0004", Type: ledger.EventMemoryRevalidated,
				Payload: map[string]any{
					"target_kind": string(ledger.TargetRecord), "target": "F1",
					"evidence_ref": "T1", "expected_stale_revision": 1,
				},
			}, func(*Candidate) error { return nil })
			if err == nil {
				t.Fatal("terminal invalidation was revalidated")
			}
		})
	}
}

func TestRenameUsesPathComponentsAndNeverRetargetsOldRefs(t *testing.T) {
	state := NewState()
	applyCandidate(t, state, ledger.Event{
		ID: "E0001", Type: ledger.EventTaskStarted, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"goal": "rename safely"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "rename safely", Branch: "main", Commit: "aaaaaaa"}}})
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for i, p := range []string{"src/dir/a.go", "src/directory/a.go"} {
		applyCandidate(t, state, ledger.Event{
			ID: []string{"E0002", "E0003"}[i], Type: ledger.EventFileSnapshot,
			Repo: "repo", Branch: "main", Commit: "aaaaaaa",
			Payload: map[string]any{"path": p, "content_digest": digest},
		}, nil)
	}
	applyCandidate(t, state, ledger.Event{
		ID: "E0004", Type: ledger.EventFileRenamed, Repo: "repo", Branch: "main",
		Payload: map[string]any{"from_path": "src/dir", "to_path": "src/moved"},
	}, nil)
	if status, _, _ := state.AddressStatus(ledger.TargetEvent, "E0002"); status != StatusStale {
		t.Fatalf("nested old-path event status=%q, want stale", status)
	}
	if status, _, _ := state.AddressStatus(ledger.TargetEvent, "E0003"); status != StatusActive {
		t.Fatalf("prefix-neighbor event status=%q, want active", status)
	}
	binding, ok := state.BindingFor(ledger.TargetEvent, "E0002")
	if !ok || len(binding.Paths) != 1 || binding.Paths[0] != "src/dir/a.go" {
		t.Fatalf("rename silently retargeted old binding: %#v", binding)
	}
}

func seededFailureState(t *testing.T) *State {
	t.Helper()
	state := NewState()
	applyCandidate(t, state, ledger.Event{
		ID: "E0001", Type: ledger.EventTaskStarted, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"goal": "diagnose"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "diagnose", Branch: "main", Commit: "aaaaaaa"}}})
	applyCandidate(t, state, ledger.Event{
		ID: "E0002", Type: ledger.EventCommandOutput,
		Payload: map[string]any{"cmd": "go test", "exit": 1, "output": "failed"},
	}, []wsl.Update{{Record: &wsl.FailureRecord{IDValue: "F1", Cmd: "go test", Exit: 1, Err: "failed"}}})
	return state
}

func applyCandidate(t *testing.T, state *State, ev ledger.Event, updates []wsl.Update) {
	t.Helper()
	err := state.WithCandidate(ev, func(candidate *Candidate) error {
		return candidate.BindUpdates(updates)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDerivedBindingsUseDeclaredScope(t *testing.T) {
	state := NewState()
	applyCandidate(t, state, ledger.Event{
		ID: "E0001", Type: ledger.EventTaskStarted, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"goal": "bind"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "bind", Branch: "main", Commit: "aaaaaaa"}}})
	applyCandidate(t, state, ledger.Event{ID: "E0002", Type: ledger.EventType("derived")}, []wsl.Update{
		{Record: &wsl.ConstraintRecord{IDValue: "C1", Scope: types.ScopeTask, Text: "keep"}},
		{Record: &wsl.FailureRecord{IDValue: "F1", FileHint: "src/a.go:12-14"}},
	})
	failure, ok := state.BindingFor(ledger.TargetRecord, "F1")
	if !ok || failure.Scope != types.ScopeBranch || !reflect.DeepEqual(failure.Paths, []string{"src/a.go"}) {
		t.Fatalf("failure binding=%#v", failure)
	}
	if !state.RecordEligible("C1") || !state.RecordEligible("F1") {
		t.Fatal("current derived bindings are not eligible")
	}
	if state.RecordEligible("UNBOUND") {
		t.Fatal("unbound derived id did not fail closed")
	}
}

func TestSecurityRevocationOfSourceEventCascadesAfterRevalidation(t *testing.T) {
	state := seededFailureState(t)
	applyCandidate(t, state, ledger.Event{
		ID: "E0003", Type: ledger.EventBranchChange, Repo: "repo", Branch: "feature", Commit: "bbbbbbb",
		Payload: map[string]any{"from_branch": "main", "from_commit": "aaaaaaa"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "diagnose", Branch: "feature", Commit: "bbbbbbb"}}})
	applyCandidate(t, state, ledger.Event{
		ID: "E0004", Type: ledger.EventMemoryRevalidated,
		Payload: map[string]any{
			"target_kind": string(ledger.TargetRecord), "target": "F1",
			"evidence_ref": "T1", "expected_stale_revision": 1,
		},
	}, nil)
	binding, ok := state.BindingFor(ledger.TargetRecord, "F1")
	if !ok || binding.EvidenceEventID != "E0002" {
		t.Fatalf("revalidation rewrote immutable provenance: %#v", binding)
	}
	applyCandidate(t, state, ledger.Event{
		ID: "E0005", Type: ledger.EventMemoryInvalidated,
		Payload: map[string]any{
			"target_kind": string(ledger.TargetEvent), "target": "E0002", "reason": string(ledger.ReasonSecurityRevoked),
		},
	}, nil)
	if status, _, _ := state.AddressStatus(ledger.TargetRecord, "F1"); status != StatusInvalidated {
		t.Fatalf("derived alias status=%q, want invalidated", status)
	}
	if state.RecordEligible("F1") || state.RawAllowed("F1") || state.RawAllowed("E0002") {
		t.Fatal("source-event revocation remained reachable through a derived alias")
	}
}

func TestRenameRejectsTerminalDestinationAndStalesReplacedDestination(t *testing.T) {
	seed := func(t *testing.T) *State {
		t.Helper()
		state := NewState()
		applyCandidate(t, state, ledger.Event{
			ID: "E0001", Type: ledger.EventTaskStarted, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
			Payload: map[string]any{"goal": "rename"},
		}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "rename", Branch: "main", Commit: "aaaaaaa"}}})
		for i, p := range []string{"src/old.go", "src/dst.go"} {
			applyCandidate(t, state, ledger.Event{
				ID: []string{"E0002", "E0003"}[i], Type: ledger.EventFileSnapshot,
				Repo: "repo", Branch: "main", Commit: "aaaaaaa",
				Payload: map[string]any{"path": p, "content_digest": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			}, nil)
		}
		return state
	}

	t.Run("terminal destination", func(t *testing.T) {
		state := seed(t)
		applyCandidate(t, state, ledger.Event{
			ID: "E0004", Type: ledger.EventMemoryInvalidated,
			Payload: map[string]any{
				"target_kind": string(ledger.TargetPath), "target": "src/dst.go", "reason": string(ledger.ReasonSecurityRevoked),
			},
		}, nil)
		before := state.Snapshot()
		err := state.WithCandidate(ledger.Event{
			ID: "E0005", Type: ledger.EventFileRenamed, Repo: "repo", Branch: "main",
			Payload: map[string]any{"from_path": "src/old.go", "to_path": "src/dst.go"},
		}, func(*Candidate) error { return nil })
		if err == nil || !reflect.DeepEqual(before, state.Snapshot()) {
			t.Fatalf("terminal destination rename err=%v mutated=%v", err, !reflect.DeepEqual(before, state.Snapshot()))
		}
	})

	t.Run("active destination replacement", func(t *testing.T) {
		state := seed(t)
		applyCandidate(t, state, ledger.Event{
			ID: "E0004", Type: ledger.EventFileRenamed, Repo: "repo", Branch: "main",
			Payload: map[string]any{"from_path": "src/old.go", "to_path": "src/dst.go"},
		}, nil)
		if status, revision, _ := state.AddressStatus(ledger.TargetEvent, "E0003"); status != StatusStale || revision != 1 {
			t.Fatalf("replaced destination evidence=(%q,%d), want stale revision 1", status, revision)
		}
		if status, _, _ := state.AddressStatus(ledger.TargetPath, "src/dst.go"); status != StatusActive {
			t.Fatalf("new destination status=%q, want active", status)
		}
	})
}

func TestBranchFactsSurviveCommitWhileAvoidInheritsGrounding(t *testing.T) {
	state := seededFailureState(t)
	applyCandidate(t, state, ledger.Event{ID: "E0003", Type: ledger.EventType("branch-facts")}, []wsl.Update{
		{Record: &wsl.ConstraintRecord{IDValue: "C1", Strength: types.StrengthHard, Scope: types.ScopeBranch, Text: "keep branch contract"}},
	})
	applyCandidate(t, state, ledger.Event{ID: "E0004", Type: ledger.EventDecision}, []wsl.Update{
		{Record: &wsl.AvoidRecord{IDValue: "A1", Text: "do not retry", Ref: "F1"}},
		{Record: &wsl.DecisionRecord{IDValue: "D1", Chosen: "stay on branch design", Scope: types.ScopeBranch}},
	})
	applyCandidate(t, state, ledger.Event{
		ID: "E0005", Type: ledger.EventCommitChange, Repo: "repo", Branch: "main", Commit: "bbbbbbb",
		Payload: map[string]any{"from_commit": "aaaaaaa"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "diagnose", Branch: "main", Commit: "bbbbbbb"}}})

	for _, id := range []string{"C1", "D1"} {
		if status, _, _ := state.AddressStatus(ledger.TargetRecord, id); status != StatusActive || !state.RecordEligible(id) {
			t.Fatalf("branch fact %s status=%q eligible=%v", id, status, state.RecordEligible(id))
		}
	}
	for _, id := range []string{"F1", "A1"} {
		if status, _, _ := state.AddressStatus(ledger.TargetRecord, id); status != StatusStale || state.RecordEligible(id) {
			t.Fatalf("commit-bound %s status=%q eligible=%v", id, status, state.RecordEligible(id))
		}
	}
}

func TestFileSnapshotGenerationIsIdempotentAndCASRecoverable(t *testing.T) {
	state := NewState()
	applyCandidate(t, state, ledger.Event{
		ID: "E0001", Type: ledger.EventTaskStarted, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"goal": "track digest"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "track digest", Branch: "main", Commit: "aaaaaaa"}}})
	applyCandidate(t, state, ledger.Event{
		ID: "E0002", Type: ledger.EventCommandOutput,
		Payload: map[string]any{"cmd": "go test", "exit": 1, "output": "failed"},
	}, []wsl.Update{{Record: &wsl.FailureRecord{IDValue: "F1", Cmd: "go test", Exit: 1, Err: "failed", FileHint: "src/a.go:12"}}})
	if !state.RecordEligible("F1") {
		t.Fatal("pre-snapshot binding unexpectedly ineligible")
	}
	digestA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	applyCandidate(t, state, ledger.Event{
		ID: "E0003", Type: ledger.EventFileSnapshot, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"path": "src/a.go", "content_digest": digestA},
	}, nil)
	if status, revision, _ := state.AddressStatus(ledger.TargetRecord, "F1"); status != StatusStale || revision != 1 {
		t.Fatalf("first snapshot status=(%q,%d), want stale revision 1", status, revision)
	}
	applyCandidate(t, state, ledger.Event{
		ID: "E0004", Type: ledger.EventMemoryRevalidated,
		Payload: map[string]any{
			"target_kind": string(ledger.TargetRecord), "target": "F1",
			"evidence_ref": "T1", "expected_stale_revision": 1,
		},
	}, nil)
	applyCandidate(t, state, ledger.Event{
		ID: "E0005", Type: ledger.EventFileSnapshot, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"path": "src/a.go", "content_digest": digestA},
	}, nil)
	if status, _, _ := state.AddressStatus(ledger.TargetRecord, "F1"); status != StatusActive || !state.RecordEligible("F1") {
		t.Fatalf("same digest changed eligibility: status=%q eligible=%v", status, state.RecordEligible("F1"))
	}
	applyCandidate(t, state, ledger.Event{
		ID: "E0006", Type: ledger.EventFileSnapshot, Repo: "repo", Branch: "main", Commit: "aaaaaaa",
		Payload: map[string]any{"path": "src/a.go", "content_digest": digestB},
	}, nil)
	if status, revision, _ := state.AddressStatus(ledger.TargetRecord, "F1"); status != StatusStale || revision != 2 {
		t.Fatalf("changed digest status=(%q,%d), want stale revision 2", status, revision)
	}
}

func TestPageStatusRequiresRematerializedScopeGeneration(t *testing.T) {
	state := seededFailureState(t)
	old, _ := state.BindingFor(ledger.TargetRecord, "F1")
	status, _ := state.PageStatus("F1", []string{"F1"}, old.Scope, old.Branch, old.Commit, old.Paths, old.SourceDigest, old.Generation())
	if status != StatusActive {
		t.Fatalf("current page status=%q", status)
	}
	applyCandidate(t, state, ledger.Event{
		ID: "E0003", Type: ledger.EventBranchChange, Repo: "repo", Branch: "feature", Commit: "bbbbbbb",
		Payload: map[string]any{"from_branch": "main", "from_commit": "aaaaaaa"},
	}, []wsl.Update{{Record: &wsl.TaskRecord{IDValue: "T1", Goal: "diagnose", Branch: "feature", Commit: "bbbbbbb"}}})
	applyCandidate(t, state, ledger.Event{
		ID: "E0004", Type: ledger.EventMemoryRevalidated,
		Payload: map[string]any{
			"target_kind": string(ledger.TargetRecord), "target": "F1",
			"evidence_ref": "T1", "expected_stale_revision": 1,
		},
	}, nil)
	status, _ = state.PageStatus("F1", []string{"F1"}, old.Scope, old.Branch, old.Commit, old.Paths, old.SourceDigest, old.Generation())
	if status != StatusStale {
		t.Fatalf("old resident generation status=%q, want stale", status)
	}
	current, _ := state.BindingFor(ledger.TargetRecord, "F1")
	status, _ = state.PageStatus("F1", []string{"F1"}, current.Scope, current.Branch, current.Commit, current.Paths, current.SourceDigest, current.Generation())
	if status != StatusActive {
		t.Fatalf("rematerialized generation status=%q, want active", status)
	}
}
