package renderer

import (
	"strings"
	"testing"

	"wsms/internal/types"
	"wsms/internal/wsl"
)

func TestRenderCapsuleContainsPinned(t *testing.T) {
	st := wsl.NewWorkingState()
	_ = st.Apply(&wsl.TaskRecord{
		IDValue:  "T42",
		Phase:    "debugging",
		Priority: types.PriorityHot,
		Goal:     "fix(stream_cancel_hang)",
		Branch:   "solver-stream-cancel",
		Dirty:    "src/runtime/stream.go",
	})
	_ = st.Apply(&wsl.ConstraintRecord{
		IDValue:  "C7",
		Strength: types.StrengthHard,
		Source:   types.SourceUser,
		Text:     "do not rewrite transport layer",
		Scope:    types.ScopeTask,
	})
	_ = st.Apply(&wsl.FailureRecord{
		IDValue:  "F18",
		Cmd:      "go test ./runtime -run TestCancelStream",
		Exit:     1,
		Err:      "stream goroutine still blocked",
		FileHint: "src/runtime/stream.go:118-176",
	})
	_ = st.Apply(&wsl.NextRecord{
		Action:   "inspect",
		Target:   "src/runtime/stream.go:118-176",
		Question: "is cancellation propagated?",
	})

	cap := RenderCapsule(st, 512)
	for _, need := range []string{
		"<working_state>",
		"TASK T42",
		"HARD CONSTRAINT C7",
		"do not rewrite transport layer",
		"LAST FAILURE F18",
		"stream goroutine still blocked",
		"NEXT:",
		PageFaultInstruction,
		"</working_state>",
	} {
		if !strings.Contains(cap, need) {
			t.Fatalf("missing %q in:\n%s", need, cap)
		}
	}
}

func TestHardConstraintSurvivesTinyBudget(t *testing.T) {
	st := wsl.NewWorkingState()
	_ = st.Apply(&wsl.ConstraintRecord{
		IDValue:  "C7",
		Strength: types.StrengthHard,
		Source:   types.SourceUser,
		Text:     "do not rewrite transport layer",
		Scope:    types.ScopeTask,
	})
	_ = st.Apply(&wsl.FailureRecord{
		IDValue: "F18",
		Cmd:     "go test",
		Exit:    1,
		Err:     strings.Repeat("x", 500),
	})
	cap := RenderCapsule(st, 20)
	if !strings.Contains(cap, "do not rewrite transport layer") {
		t.Fatalf("hard constraint dropped:\n%s", cap)
	}
}
