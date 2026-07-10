package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// EventType enumerates ledger event kinds from the design docs.
type EventType string

const (
	EventTaskStarted       EventType = "task_started"
	EventUserInstruction   EventType = "user_instruction"
	EventAssistantMessage  EventType = "assistant_message"
	EventToolCall          EventType = "tool_call"
	EventToolResult        EventType = "tool_result"
	EventCommandRun        EventType = "command_run"
	EventCommandOutput     EventType = "command_output"
	EventFileRead          EventType = "file_read"
	EventFileWrite         EventType = "file_write"
	EventGitDiff           EventType = "git_diff"
	EventTestResult        EventType = "test_result"
	EventHumanCorrection   EventType = "human_correction"
	EventDecision          EventType = "decision"
	EventAssumption        EventType = "assumption"
	EventFailure           EventType = "failure"
	EventBranchChange      EventType = "branch_change"
	EventCommitChange      EventType = "commit_change"
	EventPageCreated       EventType = "page_created"
	EventPageAccess        EventType = "page_access"
	EventMemoryInvalidated EventType = "memory_invalidated"
	EventNextAction        EventType = "next_action"
)

// ErrInvalidEvent reports a malformed known event envelope or a missing type.
var ErrInvalidEvent = errors.New("invalid event")

// Event is one append-only ledger record.
type Event struct {
	ID           string         `json:"id"`
	Seq          int64          `json:"append_seq"`
	TS           time.Time      `json:"ts"`
	Type         EventType      `json:"type"`
	SessionID    string         `json:"session_id"`
	TaskID       string         `json:"task_id,omitempty"`
	Repo         string         `json:"repo,omitempty"`
	Branch       string         `json:"branch,omitempty"`
	Commit       string         `json:"commit,omitempty"`
	Payload      map[string]any `json:"payload,omitempty"`
	ArtifactHash string         `json:"artifact_hash,omitempty"`
	Scope        map[string]any `json:"scope,omitempty"`
}

// PayloadString returns a string field from Payload, or "".
func (e Event) PayloadString(key string) string {
	if e.Payload == nil {
		return ""
	}
	v, ok := e.Payload[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// PayloadInt returns an int field from Payload, or def if missing.
func (e Event) PayloadInt(key string, def int) int {
	if e.Payload == nil {
		return def
	}
	v, ok := e.Payload[key]
	if !ok || v == nil {
		return def
	}
	if value, ok := integerPayload(v); ok {
		return value
	}
	return def
}

// ValidateEvent validates the required payload contract for known MVP events.
// Unknown non-empty event types remain valid and inert for forward compatibility.
func ValidateEvent(ev Event) error {
	if ev.Type == "" {
		return fmt.Errorf("%w: event type is required", ErrInvalidEvent)
	}
	switch ev.Type {
	case EventTaskStarted:
		if err := requirePayloadString(ev, "goal", true); err != nil {
			return err
		}
		return validateOptionalStrings(ev, "task_id", "phase", "priority", "branch", "dirty")
	case EventUserInstruction, EventAssistantMessage:
		return requirePayloadString(ev, "text", true)
	case EventCommandOutput:
		if err := requirePayloadString(ev, "cmd", true); err != nil {
			return err
		}
		if value, ok := ev.Payload["exit"]; !ok {
			return invalidPayloadField(ev, "exit", "is required")
		} else if _, ok := integerPayload(value); !ok {
			return invalidPayloadField(ev, "exit", "must be an integer")
		}
		if err := requirePayloadString(ev, "output", false); err != nil {
			return err
		}
		return validateOptionalStrings(ev, "err", "file_hint", "raw")
	case EventDecision:
		if err := requirePayloadString(ev, "chosen", true); err != nil {
			return err
		}
		if err := validateOptionalStrings(ev, "because", "refs", "scope", "avoid_text", "avoid_ref"); err != nil {
			return err
		}
		avoidText := strings.TrimSpace(ev.PayloadString("avoid_text"))
		avoidRef := strings.TrimSpace(ev.PayloadString("avoid_ref"))
		if avoidText != "" && avoidRef == "" {
			return invalidPayloadField(ev, "avoid_ref", "is required when avoid_text is present")
		}
		if avoidRef != "" && avoidText == "" {
			return invalidPayloadField(ev, "avoid_text", "is required when avoid_ref is present")
		}
		return nil
	case EventNextAction:
		if err := requirePayloadString(ev, "action", true); err != nil {
			return err
		}
		if err := requirePayloadString(ev, "target", true); err != nil {
			return err
		}
		return validateOptionalStrings(ev, "question")
	default:
		return nil
	}
}

func requirePayloadString(ev Event, key string, nonEmpty bool) error {
	value, ok := ev.Payload[key]
	if !ok {
		return invalidPayloadField(ev, key, "is required")
	}
	text, ok := value.(string)
	if !ok {
		return invalidPayloadField(ev, key, "must be a string")
	}
	if nonEmpty && strings.TrimSpace(text) == "" {
		return invalidPayloadField(ev, key, "must not be empty")
	}
	return nil
}

func validateOptionalStrings(ev Event, keys ...string) error {
	for _, key := range keys {
		if value, ok := ev.Payload[key]; ok {
			if _, ok := value.(string); !ok {
				return invalidPayloadField(ev, key, "must be a string when present")
			}
		}
	}
	return nil
}

func invalidPayloadField(ev Event, field, reason string) error {
	return fmt.Errorf("%w: event type %q payload field %q %s", ErrInvalidEvent, ev.Type, field, reason)
}

func integerPayload(value any) (int, bool) {
	if value == nil {
		return 0, false
	}
	maxInt := int64(^uint(0) >> 1)
	minInt := -maxInt - 1
	fromInt64 := func(value int64) (int, bool) {
		if value < minInt || value > maxInt {
			return 0, false
		}
		return int(value), true
	}
	fromUint64 := func(value uint64) (int, bool) {
		if value > uint64(maxInt) {
			return 0, false
		}
		return int(value), true
	}

	switch value := value.(type) {
	case int:
		return value, true
	case int8:
		return int(value), true
	case int16:
		return int(value), true
	case int32:
		return int(value), true
	case int64:
		return fromInt64(value)
	case uint:
		return fromUint64(uint64(value))
	case uint8:
		return int(value), true
	case uint16:
		return int(value), true
	case uint32:
		return fromUint64(uint64(value))
	case uint64:
		return fromUint64(value)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
			return 0, false
		}
		integer := int64(value)
		if float64(integer) != value {
			return 0, false
		}
		return fromInt64(integer)
	case json.Number:
		integer, err := value.Int64()
		if err != nil {
			return 0, false
		}
		return fromInt64(integer)
	default:
		return 0, false
	}
}
