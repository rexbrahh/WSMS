package ledger

import (
	"errors"
	"testing"
)

func TestValidateEventKnownPayloads(t *testing.T) {
	valid := []Event{
		{Type: EventTaskStarted, Payload: map[string]any{"goal": "ship demo", "phase": "implementation"}},
		{Type: EventUserInstruction, Payload: map[string]any{"text": "do not lose evidence"}},
		{Type: EventHumanCorrection, Payload: map[string]any{"text": "keep the exact command"}},
		{Type: EventAssistantMessage, Payload: map[string]any{"text": "acknowledged"}},
		{Type: EventCommandOutput, Payload: map[string]any{"cmd": "go test ./...", "exit": int32(1), "output": "failed"}},
		{Type: EventTestResult, Payload: map[string]any{"command": "go test ./...", "exit": float64(1), "output": "failed"}},
		{Type: EventToolResult, Payload: map[string]any{"cmd": "go test ./...", "exit": 0, "output": "ok"}},
		{Type: EventDecision, Payload: map[string]any{"chosen": "inspect the worker", "avoid_text": "retry old patch", "avoid_ref": "F1"}},
		{Type: EventNextAction, Payload: map[string]any{"action": "inspect", "target": "worker.go"}},
		{Type: EventType("future_event"), Payload: map[string]any{"opaque": true}},
	}
	for _, ev := range valid {
		if err := ValidateEvent(ev); err != nil {
			t.Errorf("ValidateEvent(%q): %v", ev.Type, err)
		}
	}
}

func TestValidateEventRejectsMalformedKnownPayloads(t *testing.T) {
	tests := []Event{
		{},
		{Type: EventTaskStarted, Payload: map[string]any{}},
		{Type: EventUserInstruction, Payload: map[string]any{"text": "  "}},
		{Type: EventHumanCorrection, Payload: map[string]any{}},
		{Type: EventAssistantMessage, Payload: map[string]any{"text": 42}},
		{Type: EventCommandOutput, Payload: map[string]any{"cmd": "go test", "exit": 1.5, "output": "failed"}},
		{Type: EventCommandOutput, Payload: map[string]any{"cmd": "go test", "exit": 1}},
		{Type: EventTestResult, Payload: map[string]any{"command": "go test", "exit": 1.5, "output": "failed"}},
		{Type: EventToolResult, Payload: map[string]any{"cmd": 42, "command": "go test", "exit": 1, "output": "failed"}},
		{Type: EventDecision, Payload: map[string]any{"chosen": "inspect", "avoid_text": "retry"}},
		{Type: EventDecision, Payload: map[string]any{"chosen": "inspect", "avoid_ref": "F1"}},
		{Type: EventNextAction, Payload: map[string]any{"action": "inspect"}},
	}
	for _, ev := range tests {
		if err := ValidateEvent(ev); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("ValidateEvent(%q) error=%v, want ErrInvalidEvent", ev.Type, err)
		}
	}
}

func TestPayloadIntAcceptsValidatedIntegerForms(t *testing.T) {
	for _, value := range []any{int8(3), int32(3), int64(3), uint16(3), float64(3)} {
		ev := Event{Payload: map[string]any{"exit": value}}
		if got := ev.PayloadInt("exit", -1); got != 3 {
			t.Errorf("PayloadInt(%T)=%d", value, got)
		}
	}
	if got := (Event{Payload: map[string]any{"exit": 3.5}}).PayloadInt("exit", -1); got != -1 {
		t.Fatalf("fractional PayloadInt=%d", got)
	}
}
