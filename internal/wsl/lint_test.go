package wsl

import (
	"testing"

	"wsms/internal/types"
)

func TestLintDecisionVsHardConstraint(t *testing.T) {
	st := NewWorkingState()
	if err := st.Apply(&ConstraintRecord{
		IDValue:  "C7",
		Strength: types.StrengthHard,
		Source:   types.SourceUser,
		Text:     "do not rewrite transport layer",
		Scope:    types.ScopeTask,
	}); err != nil {
		t.Fatal(err)
	}
	err := st.Apply(&DecisionRecord{
		IDValue: "D9",
		Chosen:  "rewrite transport layer",
		Because: "seems easier",
	})
	if err == nil {
		t.Fatal("expected lint error")
	}
}

func TestLintDanglingAvoidRef(t *testing.T) {
	st := NewWorkingState()
	err := st.Apply(&AvoidRecord{
		IDValue: "A1",
		Reason:  "failed_attempt",
		Text:    "bad patch",
		Ref:     "F999",
	})
	if err == nil {
		t.Fatal("expected dangling ref error")
	}
}

func TestLintAllowAvoidWithKnownFailure(t *testing.T) {
	st := NewWorkingState()
	if err := st.Apply(&FailureRecord{
		IDValue: "F16",
		Cmd:     "go test",
		Exit:    1,
		Err:     "boom",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(&AvoidRecord{
		IDValue: "A4",
		Reason:  "failed_attempt",
		Text:    "previous patch",
		Ref:     "F16",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestImmutableFailureFields(t *testing.T) {
	st := NewWorkingState()
	if err := st.Apply(&FailureRecord{
		IDValue: "F1",
		Cmd:     "go test",
		Exit:    1,
		Err:     "first",
	}); err != nil {
		t.Fatal(err)
	}
	err := st.Apply(&FailureRecord{
		IDValue: "F1",
		Cmd:     "go test ./other",
		Exit:    1,
		Err:     "first",
	})
	if err == nil {
		t.Fatal("expected immutable cmd error")
	}
}
