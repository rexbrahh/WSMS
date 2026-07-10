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
		{Type: EventDecision, Payload: map[string]any{"chosen": "keep branch policy", "scope": "branch"}},
		{Type: EventNextAction, Payload: map[string]any{"action": "inspect", "target": "worker.go"}},
		{Type: EventBranchChange, Repo: "repo", Branch: "feature", Commit: "bbbbbbb", Payload: map[string]any{"from_branch": "main", "from_commit": "aaaaaaa"}},
		{Type: EventCommitChange, Repo: "repo", Branch: "feature", Commit: "ccccccc", Payload: map[string]any{"from_commit": "bbbbbbb"}},
		{Type: EventFileSnapshot, Repo: "repo", Branch: "feature", Commit: "ccccccc", Payload: map[string]any{"path": "src/a.go", "content_digest": testDigest}},
		{Type: EventFileRenamed, Repo: "repo", Branch: "feature", Payload: map[string]any{"from_path": "src/a.go", "to_path": "src/b.go", "content_digest": testDigest}},
		{Type: EventMemoryInvalidated, Payload: map[string]any{"target_kind": "record", "target": "F1", "reason": "superseded"}},
		{Type: EventMemoryRevalidated, Payload: map[string]any{"target_kind": "record", "target": "F1", "evidence_ref": "E0002", "expected_stale_revision": 1}},
		// Legacy file_read remains permissive for replay compatibility.
		{Type: EventFileRead, Payload: map[string]any{"legacy": true}},
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
		{Type: EventDecision, Payload: map[string]any{"chosen": "inspect", "scope": "banana"}},
		{Type: EventDecision, Payload: map[string]any{"chosen": "inspect", "refs": "F1 ../../secret"}},
		{Type: EventDecision, Payload: map[string]any{"chosen": "inspect", "avoid_text": "retry", "avoid_ref": "F1 F2"}},
		{Type: EventNextAction, Payload: map[string]any{"action": "inspect"}},
		{Type: EventBranchChange, Repo: "repo", Branch: "main", Payload: map[string]any{"from_branch": "main"}},
		{Type: EventBranchChange, Repo: "repo", Branch: "feature", Payload: map[string]any{"from_branch": "main", "to_branch": "other"}},
		{Type: EventBranchChange, Repo: "repo", Branch: "feature", Commit: "bbbbbbb", Payload: map[string]any{"from_branch": "main"}},
		{Type: EventCommitChange, Repo: "repo", Branch: "main", Commit: "bbbbbbb", Payload: map[string]any{"from_commit": "bbbbbbb"}},
		{Type: EventFileSnapshot, Repo: "repo", Branch: "main", Commit: "aaaaaaa", Payload: map[string]any{"path": "../secret", "content_digest": testDigest}},
		{Type: EventFileSnapshot, Repo: "repo", Branch: "main", Commit: "aaaaaaa", Payload: map[string]any{"path": "src/a.go", "content_digest": "NOT-A-DIGEST"}},
		{Type: EventFileRenamed, Repo: "repo", Branch: "main", Payload: map[string]any{"from_path": "src/a.go", "to_path": "src/a.go"}},
		{Type: EventMemoryInvalidated, Payload: map[string]any{"target_kind": "record", "target": "F1", "reason": "branch_changed"}},
		{Type: EventMemoryRevalidated, Payload: map[string]any{"target_kind": "record", "target": "F1", "evidence_ref": "E1", "expected_stale_revision": 0}},
		{Type: EventMemoryRevalidated, Payload: map[string]any{"target_kind": "event", "target": "E1", "evidence_ref": "E2", "expected_stale_revision": 1}},
		{Type: EventMemoryRevalidated, Payload: map[string]any{"target_kind": "page", "target": "P1", "evidence_ref": "E2", "expected_stale_revision": 1}},
	}
	for _, ev := range tests {
		if err := ValidateEvent(ev); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("ValidateEvent(%q) error=%v, want ErrInvalidEvent", ev.Type, err)
		}
	}
}

const testDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestNormalizeRepoPathRejectsTraversalAndNonCanonicalInputs(t *testing.T) {
	for _, input := range []string{
		"", ".", "..", "../secret", "a/../../secret", "/etc/passwd", `C:/Windows/system.ini`,
		`src\\a.go`, "src//a.go", "src/./a.go", "src/../a.go", "src/a\x00.go", "src/a\nb.go",
	} {
		cleaned, err := NormalizeRepoPath(input)
		if err == nil && cleaned == input {
			t.Errorf("NormalizeRepoPath(%q) accepted unsafe/noncanonical input", input)
		}
	}
	for _, input := range []string{"src/a.go", "a", "docs/design/file name.md"} {
		cleaned, err := NormalizeRepoPath(input)
		if err != nil || cleaned != input {
			t.Errorf("NormalizeRepoPath(%q)=(%q,%v)", input, cleaned, err)
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
