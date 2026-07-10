package wsl

import (
	"testing"

	"wsms/internal/types"
)

func TestApplyUpdatesIsAtomicAndDoesNotLeakProvenance(t *testing.T) {
	st := NewWorkingState()
	st.NoteEvent("E0001")

	err := st.ApplyUpdates([]Update{
		{
			Op: "upsert",
			Record: &FailureRecord{
				IDValue: "F1",
				Cmd:     "go test ./...",
				Exit:    1,
				Err:     "failed",
			},
			EvidenceID: "E0001",
		},
		{
			Op: "upsert",
			Record: &AvoidRecord{
				IDValue: "A1",
				Reason:  "failed_attempt",
				Text:    "bad patch",
				Ref:     "F404",
			},
			EvidenceID: "E0001",
		},
	})
	if err == nil {
		t.Fatal("expected invalid second update to reject the batch")
	}
	if _, ok := st.Get("F1"); ok {
		t.Fatal("first record leaked from rejected batch")
	}
	if _, ok := st.EvidenceID("F1"); ok {
		t.Fatal("first record provenance leaked from rejected batch")
	}
	if got := st.Provenance(); len(got) != 0 {
		t.Fatalf("provenance leaked from rejected batch: %#v", got)
	}
}

func TestApplyAllIsAtomic(t *testing.T) {
	st := NewWorkingState()
	err := st.ApplyAll([]Record{
		&FailureRecord{IDValue: "F1", Cmd: "go test ./...", Exit: 1, Err: "failed"},
		&AvoidRecord{IDValue: "A1", Reason: "failed_attempt", Text: "bad patch", Ref: "F404"},
	})
	if err == nil {
		t.Fatal("expected invalid second record to reject ApplyAll")
	}
	if records := st.Records(); len(records) != 0 {
		t.Fatalf("ApplyAll committed a partial prefix: %#v", records)
	}
}

func TestApplyRejectsRecordKindReplacement(t *testing.T) {
	st := NewWorkingState()
	constraint := &ConstraintRecord{
		IDValue:  "C1",
		Strength: types.StrengthHard,
		Source:   types.SourceUser,
		Text:     "do not rewrite transport layer",
		Scope:    types.ScopeTask,
	}
	if err := st.Apply(constraint); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(&FailureRecord{
		IDValue: "C1",
		Cmd:     "go test ./...",
		Exit:    1,
		Err:     "failed",
	}); err == nil {
		t.Fatal("expected record kind replacement to fail")
	}

	got, ok := st.Get("C1")
	if !ok {
		t.Fatal("original constraint disappeared")
	}
	if got.Kind() != KindConstraint || got.(*ConstraintRecord).Text != constraint.Text {
		t.Fatalf("original constraint changed: %#v", got)
	}
}

func TestClonePreservesIndependentProvenance(t *testing.T) {
	st := NewWorkingState()
	st.NoteEvent("E0001")
	if err := st.ApplyUpdates([]Update{{
		Op: "upsert",
		Record: &ConstraintRecord{
			IDValue:  "C1",
			Strength: types.StrengthHard,
			Source:   types.SourceUser,
			Text:     "do not rewrite transport layer",
			Scope:    types.ScopeTask,
		},
		EvidenceID: "E0001",
	}}); err != nil {
		t.Fatal(err)
	}

	clone := st.Clone()
	assertEvidenceID(t, st, "C1", "E0001")
	assertEvidenceID(t, clone, "C1", "E0001")

	clone.NoteEvent("E0002")
	if err := clone.ApplyUpdates([]Update{{
		Op: "upsert",
		Record: &ConstraintRecord{
			IDValue:  "C2",
			Strength: types.StrengthHard,
			Source:   types.SourceUser,
			Text:     "never discard exact evidence",
			Scope:    types.ScopeTask,
		},
		EvidenceID: "E0002",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.EvidenceID("C2"); ok {
		t.Fatal("clone provenance mutation leaked into original")
	}
	assertEvidenceID(t, clone, "C2", "E0002")

	copy := clone.Provenance()
	copy["C1"] = "tampered"
	assertEvidenceID(t, clone, "C1", "E0001")
}

func TestApplyUpdatesRejectsUnnotedEvidence(t *testing.T) {
	st := NewWorkingState()
	err := st.ApplyUpdates([]Update{{
		Op: "upsert",
		Record: &ConstraintRecord{
			IDValue:  "C1",
			Strength: types.StrengthHard,
			Source:   types.SourceUser,
			Text:     "do not rewrite transport layer",
			Scope:    types.ScopeTask,
		},
		EvidenceID: "E9999",
	}})
	if err == nil {
		t.Fatal("expected unnoted evidence to fail")
	}
	if _, ok := st.Get("C1"); ok {
		t.Fatal("record with unnoted provenance was committed")
	}
}

func TestApplyDerivedUpdatesRequiresEvidence(t *testing.T) {
	st := NewWorkingState()
	st.NoteEvent("E0001")
	rec := &ConstraintRecord{
		IDValue:  "C1",
		Strength: types.StrengthHard,
		Source:   types.SourceUser,
		Text:     "do not rewrite transport layer",
		Scope:    types.ScopeTask,
	}

	if err := st.ApplyDerivedUpdates([]Update{{Op: "upsert", Record: rec}}); err == nil {
		t.Fatal("derived update without evidence succeeded")
	}
	if _, ok := st.Get("C1"); ok {
		t.Fatal("derived update without evidence mutated state")
	}

	if err := st.ApplyDerivedUpdates([]Update{{
		Op:         "upsert",
		Record:     rec,
		EvidenceID: "E0001",
	}}); err != nil {
		t.Fatal(err)
	}
	assertEvidenceID(t, st, "C1", "E0001")
}

func TestTrustedStaticReplacementClearsStaleProvenance(t *testing.T) {
	st := NewWorkingState()
	st.NoteEvent("E0001")
	rec := &ConstraintRecord{
		IDValue:  "C1",
		Strength: types.StrengthHard,
		Source:   types.SourceUser,
		Text:     "do not rewrite transport layer",
		Scope:    types.ScopeTask,
	}
	if err := st.ApplyDerivedUpdates([]Update{{
		Op:         "upsert",
		Record:     rec,
		EvidenceID: "E0001",
	}}); err != nil {
		t.Fatal(err)
	}
	assertEvidenceID(t, st, "C1", "E0001")

	if err := st.Apply(rec.Clone()); err != nil {
		t.Fatal(err)
	}
	if evidenceID, ok := st.EvidenceID("C1"); ok {
		t.Fatalf("static replacement retained stale evidence %q", evidenceID)
	}
}

func TestDerivedUpdatesRejectUnknownOpAndTypedNilWithoutPanic(t *testing.T) {
	st := NewWorkingState()
	st.NoteEvent("E0001")
	var typedNil *ConstraintRecord
	tests := []struct {
		name   string
		update Update
	}{
		{
			name: "unknown operation",
			update: Update{
				Op:         "invalidate",
				Record:     &ConstraintRecord{IDValue: "C1"},
				EvidenceID: "E0001",
			},
		},
		{
			name: "typed nil record",
			update: Update{
				Op:         "upsert",
				Record:     typedNil,
				EvidenceID: "E0001",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := st.ApplyDerivedUpdates([]Update{tt.update}); err == nil {
				t.Fatal("invalid update succeeded")
			}
			if records := st.Records(); len(records) != 0 {
				t.Fatalf("invalid update mutated state: %#v", records)
			}
		})
	}
}

func TestPageFieldOrderIncludesBranch(t *testing.T) {
	fields := FieldOrder[KindPage]
	if len(fields) == 0 || fields[len(fields)-1] != "branch" {
		t.Fatalf("page field order=%v, want trailing branch", fields)
	}
}

func assertEvidenceID(t *testing.T, st *WorkingState, recordID, want string) {
	t.Helper()
	got, ok := st.EvidenceID(recordID)
	if !ok || got != want {
		t.Fatalf("evidence for %s=(%q, %v), want (%q, true)", recordID, got, ok, want)
	}
}
