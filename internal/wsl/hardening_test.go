package wsl

import (
	"os"
	"strings"
	"testing"

	"wsms/internal/types"
)

func TestSerializeUsesOneBlankLineAndIsIdempotent(t *testing.T) {
	records := []Record{
		&ConstraintRecord{
			IDValue:  "C1",
			Strength: types.StrengthHard,
			Source:   types.SourceUser,
			Text:     "do not rewrite transport layer",
			Scope:    types.ScopeTask,
		},
		&FailureRecord{
			IDValue: "F1",
			Cmd:     "go test ./...",
			Exit:    1,
			Err:     "tests failed",
		},
	}

	serialized := Serialize(records)
	if strings.Contains(serialized, "\n\n\n") {
		t.Fatalf("more than one blank line between records:\n%s", serialized)
	}
	if count := strings.Count(serialized, "\n\n"); count != 1 {
		t.Fatalf("blank separators=%d, want 1:\n%s", count, serialized)
	}

	reparsed, err := Parse(serialized)
	if err != nil {
		t.Fatal(err)
	}
	if got := Serialize(reparsed); got != serialized {
		t.Fatalf("serialization is not idempotent:\nfirst:\n%s\nsecond:\n%s", serialized, got)
	}
}

func TestQuotedValuesRoundTripWithoutEscapeDrift(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "windows path", text: `do not edit C:\temp\file.go`},
		{name: "embedded quote", text: `preserve the "exact" error`},
		{name: "embedded backtick", text: "preserve `go test ./...` exactly"},
		{name: "control characters", text: "first line\nsecond line\tindented"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canonical := Serialize([]Record{&ConstraintRecord{
				IDValue:  "C1",
				Strength: types.StrengthHard,
				Source:   types.SourceUser,
				Text:     tt.text,
				Scope:    types.ScopeTask,
			}})

			current := canonical
			for pass := 1; pass <= 3; pass++ {
				records, err := Parse(current)
				if err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
				constraint := records[0].(*ConstraintRecord)
				if constraint.Text != tt.text {
					t.Fatalf("pass %d text=%q, want %q", pass, constraint.Text, tt.text)
				}
				current = Serialize(records)
				if current != canonical {
					t.Fatalf("pass %d changed canonical text:\n%s", pass, current)
				}
			}
		})
	}
}

func TestParseRejectsMalformedQuotedValues(t *testing.T) {
	for _, src := range []string{
		"@constraint C1 hard source=user\ntext: \"bad\\q escape\"\nscope: task\n",
		"@constraint C1 hard source=user\ntext: \"unterminated\nscope: task\n",
	} {
		if _, err := Parse(src); err == nil {
			t.Fatalf("expected malformed quoted value to fail:\n%s", src)
		}
	}
}

func TestImmutableExactFieldsCannotBeErased(t *testing.T) {
	t.Run("failure command", func(t *testing.T) {
		st := NewWorkingState()
		mustApply(t, st, &FailureRecord{IDValue: "F1", Cmd: "go test ./...", Exit: 1, Err: "failed"})
		if err := st.Apply(&FailureRecord{IDValue: "F1", Exit: 1, Err: "failed"}); err == nil {
			t.Fatal("expected command erasure to fail")
		}
	})

	t.Run("failure error", func(t *testing.T) {
		st := NewWorkingState()
		mustApply(t, st, &FailureRecord{IDValue: "F1", Cmd: "go test ./...", Exit: 1, Err: "failed"})
		if err := st.Apply(&FailureRecord{IDValue: "F1", Cmd: "go test ./...", Exit: 1}); err == nil {
			t.Fatal("expected error erasure to fail")
		}
	})

	t.Run("constraint text", func(t *testing.T) {
		st := NewWorkingState()
		mustApply(t, st, &ConstraintRecord{
			IDValue:  "C1",
			Strength: types.StrengthHard,
			Source:   types.SourceUser,
			Text:     "do not rewrite transport layer",
			Scope:    types.ScopeTask,
		})
		if err := st.Apply(&ConstraintRecord{
			IDValue:  "C1",
			Strength: types.StrengthHard,
			Source:   types.SourceUser,
			Scope:    types.ScopeTask,
		}); err == nil {
			t.Fatal("expected constraint text erasure to fail")
		}
	})
}

func TestEventReferenceRequiresNotedEvent(t *testing.T) {
	avoid := &AvoidRecord{
		IDValue: "A1",
		Reason:  "failed_attempt",
		Text:    "bad patch",
		Ref:     "E999",
	}

	st := NewWorkingState()
	if err := st.Apply(avoid); err == nil {
		t.Fatal("expected unknown event reference to fail")
	}

	st.NoteEvent("E999")
	if err := st.Apply(avoid); err != nil {
		t.Fatalf("noted event reference rejected: %v", err)
	}
}

func TestPoliteHardNegationStillContradictsDecision(t *testing.T) {
	st := NewWorkingState()
	mustApply(t, st, &ConstraintRecord{
		IDValue:  "C1",
		Strength: types.StrengthHard,
		Source:   types.SourceUser,
		Text:     "please do not rewrite transport layer",
		Scope:    types.ScopeTask,
	})
	if err := st.Apply(&DecisionRecord{
		IDValue: "D1",
		Chosen:  "rewrite transport layer",
		Because: "seems easier",
		Scope:   types.ScopeTask,
	}); err == nil {
		t.Fatal("expected polite hard constraint contradiction to fail")
	}
}

func TestSampleSessionAppliesCleanly(t *testing.T) {
	data, err := os.ReadFile(testdata("sample_session.wsl"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := Parse(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewWorkingState().ApplyAll(records); err != nil {
		t.Fatalf("sample session did not lint: %v", err)
	}
}

func mustApply(t *testing.T, st *WorkingState, record Record) {
	t.Helper()
	if err := st.Apply(record); err != nil {
		t.Fatal(err)
	}
}
