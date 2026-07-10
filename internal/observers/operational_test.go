package observers

import (
	"context"
	"testing"

	"wsms/internal/ledger"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

func TestOperationalDerivesTaskAndStableNext(t *testing.T) {
	observer := &Operational{IDs: NewSeqIDGen()}
	taskUpdates, err := observer.Handle(context.Background(), ledger.Event{
		Type:   ledger.EventTaskStarted,
		Branch: "main",
		Commit: "abc123",
		Payload: map[string]any{
			"goal":     "ship the runtime demo",
			"phase":    "implementation",
			"priority": "hot",
			"dirty":    "internal/harness/session.go",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(taskUpdates) != 1 {
		t.Fatalf("task update count=%d", len(taskUpdates))
	}
	task, ok := taskUpdates[0].Record.(*wsl.TaskRecord)
	if !ok {
		t.Fatalf("task record=%T", taskUpdates[0].Record)
	}
	if task.IDValue != "T1" || task.Goal != "ship the runtime demo" || task.Phase != "implementation" || task.Priority != types.PriorityHot || task.Branch != "main" || task.Commit != "abc123" {
		t.Fatalf("task=%#v", task)
	}

	nextUpdates, err := observer.Handle(context.Background(), ledger.Event{
		Type: ledger.EventNextAction,
		Payload: map[string]any{
			"action":   "inspect",
			"target":   "internal/harness/session.go",
			"question": "does replay match live state?",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	next, ok := nextUpdates[0].Record.(*wsl.NextRecord)
	if !ok || next.ID() != "next" || next.Action != "inspect" {
		t.Fatalf("next=%#v", nextUpdates[0].Record)
	}
}

func TestDecisionsDerivesDecisionAndGroundedAvoidance(t *testing.T) {
	observer := &Decisions{IDs: NewSeqIDGen()}
	updates, err := observer.Handle(context.Background(), ledger.Event{
		Type: ledger.EventDecision,
		Payload: map[string]any{
			"chosen":     "inspect cancellation ordering",
			"because":    "F1 preserves the exact failure",
			"refs":       "F1",
			"scope":      "task",
			"avoid_text": "retry the previous cleanup patch",
			"avoid_ref":  "F1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 {
		t.Fatalf("update count=%d", len(updates))
	}
	avoid, ok := updates[0].Record.(*wsl.AvoidRecord)
	if !ok || avoid.IDValue != "A1" || avoid.Ref != "F1" || avoid.Reason != "failed_attempt" {
		t.Fatalf("avoid=%#v", updates[0].Record)
	}
	decision, ok := updates[1].Record.(*wsl.DecisionRecord)
	if !ok || decision.IDValue != "D1" || decision.Chosen != "inspect cancellation ordering" || decision.Scope != types.ScopeTask {
		t.Fatalf("decision=%#v", updates[1].Record)
	}
}
