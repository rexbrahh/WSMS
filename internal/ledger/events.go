package ledger

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
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
	EventFileSnapshot      EventType = "file_snapshot"
	EventFileRenamed       EventType = "file_renamed"
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
	EventMemoryRevalidated EventType = "memory_revalidated"
	EventNextAction        EventType = "next_action"
)

// MemoryTargetKind identifies the authoritative address space affected by a
// coherence mutation. Record targets are session-local WSL/event addresses;
// path targets are canonical repository-relative paths.
type MemoryTargetKind string

const (
	TargetRecord MemoryTargetKind = "record"
	TargetEvent  MemoryTargetKind = "event"
	TargetPage   MemoryTargetKind = "page"
	TargetPath   MemoryTargetKind = "path"
)

// InvalidationReason is a closed policy set. Security and policy revocations
// are also consulted by raw-evidence authorization; the other reasons suppress
// reuse while preserving diagnostic L4 access.
type InvalidationReason string

const (
	ReasonSuperseded      InvalidationReason = "superseded"
	ReasonUserRejected    InvalidationReason = "user_rejected"
	ReasonSourceDeleted   InvalidationReason = "source_deleted"
	ReasonPolicyChanged   InvalidationReason = "policy_changed"
	ReasonSecurityRevoked InvalidationReason = "security_revoked"
)

var recordTargetRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,127}$`)

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
		if err := validateOptionalStrings(ev, "task_id", "phase", "priority", "branch", "dirty"); err != nil {
			return err
		}
		if payloadBranch := ev.PayloadString("branch"); payloadBranch != "" && ev.Branch != "" && payloadBranch != ev.Branch {
			return invalidPayloadField(ev, "branch", "must match the authoritative event branch")
		}
		if ev.Branch != "" {
			return validateScopeToken(ev, "branch", ev.Branch)
		}
		if payloadBranch := ev.PayloadString("branch"); payloadBranch != "" {
			return validateScopeToken(ev, "branch", payloadBranch)
		}
		return nil
	case EventUserInstruction, EventHumanCorrection, EventAssistantMessage:
		return requirePayloadString(ev, "text", true)
	case EventCommandOutput:
		return validateCommandOutput(ev, false)
	case EventTestResult, EventToolResult:
		return validateCommandOutput(ev, true)
	case EventDecision:
		if err := requirePayloadString(ev, "chosen", true); err != nil {
			return err
		}
		if err := validateOptionalStrings(ev, "because", "refs", "scope", "avoid_text", "avoid_ref"); err != nil {
			return err
		}
		if scope := ev.PayloadString("scope"); scope != "" {
			switch scope {
			case "global", "repo", "branch", "task", "file":
			default:
				return invalidPayloadField(ev, "scope", "must be global, repo, branch, task, or file")
			}
		}
		if refs := ev.PayloadString("refs"); refs != "" {
			for _, field := range strings.Fields(refs) {
				ref := strings.Trim(field, ",")
				if !recordTargetRE.MatchString(ref) {
					return invalidPayloadField(ev, "refs", "must contain only session-local logical addresses")
				}
			}
		}
		avoidText := strings.TrimSpace(ev.PayloadString("avoid_text"))
		avoidRef := strings.TrimSpace(ev.PayloadString("avoid_ref"))
		if avoidText != "" && avoidRef == "" {
			return invalidPayloadField(ev, "avoid_ref", "is required when avoid_text is present")
		}
		if avoidRef != "" && avoidText == "" {
			return invalidPayloadField(ev, "avoid_text", "is required when avoid_ref is present")
		}
		if avoidRef != "" && !recordTargetRE.MatchString(avoidRef) {
			return invalidPayloadField(ev, "avoid_ref", "must be one session-local logical address")
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
	case EventBranchChange:
		if err := validatePayloadKeys(ev, "from_branch", "from_commit"); err != nil {
			return err
		}
		if err := validateRequiredEnvelopeScope(ev, true, true, false); err != nil {
			return err
		}
		if err := requirePayloadString(ev, "from_branch", true); err != nil {
			return err
		}
		if err := validateScopeToken(ev, "from_branch", ev.PayloadString("from_branch")); err != nil {
			return err
		}
		if ev.PayloadString("from_branch") == ev.Branch {
			return invalidPayloadField(ev, "from_branch", "must differ from the authoritative post-transition branch")
		}
		fromCommit, hasFrom := ev.Payload["from_commit"]
		if ev.Commit == "" && hasFrom || ev.Commit != "" && !hasFrom {
			return invalidPayloadField(ev, "from_commit", "must be paired with the authoritative post-transition commit")
		}
		if hasFrom {
			if _, ok := fromCommit.(string); !ok {
				return invalidPayloadField(ev, "from_commit", "must be a string")
			}
			if err := validateScopeToken(ev, "from_commit", ev.PayloadString("from_commit")); err != nil {
				return err
			}
		}
		return nil
	case EventCommitChange:
		if err := validatePayloadKeys(ev, "from_commit"); err != nil {
			return err
		}
		if err := validateRequiredEnvelopeScope(ev, true, true, true); err != nil {
			return err
		}
		if err := requirePayloadString(ev, "from_commit", true); err != nil {
			return err
		}
		if err := validateScopeToken(ev, "from_commit", ev.PayloadString("from_commit")); err != nil {
			return err
		}
		if ev.PayloadString("from_commit") == ev.Commit {
			return invalidPayloadField(ev, "from_commit", "must differ from the authoritative post-transition commit")
		}
		return nil
	case EventFileSnapshot:
		if err := validatePayloadKeys(ev, "path", "content_digest"); err != nil {
			return err
		}
		if err := validateRequiredEnvelopeScope(ev, true, true, true); err != nil {
			return err
		}
		if err := validateCanonicalPathPayload(ev, "path"); err != nil {
			return err
		}
		return validateDigestPayload(ev, "content_digest")
	case EventFileRenamed:
		if err := validatePayloadKeys(ev, "from_path", "to_path", "content_digest"); err != nil {
			return err
		}
		if err := validateRequiredEnvelopeScope(ev, true, true, false); err != nil {
			return err
		}
		if err := validateCanonicalPathPayload(ev, "from_path"); err != nil {
			return err
		}
		if err := validateCanonicalPathPayload(ev, "to_path"); err != nil {
			return err
		}
		if ev.PayloadString("from_path") == ev.PayloadString("to_path") {
			return invalidPayloadField(ev, "to_path", "must differ from from_path")
		}
		if _, ok := ev.Payload["content_digest"]; ok {
			return validateDigestPayload(ev, "content_digest")
		}
		return nil
	case EventMemoryInvalidated:
		return validateMemoryMutation(ev, false)
	case EventMemoryRevalidated:
		return validateMemoryMutation(ev, true)
	default:
		return nil
	}
}

func validateMemoryMutation(ev Event, revalidation bool) error {
	allowed := []string{"target_kind", "target", "reason"}
	if revalidation {
		allowed = []string{"target_kind", "target", "evidence_ref", "expected_stale_revision", "source_digest"}
	}
	if err := validatePayloadKeys(ev, allowed...); err != nil {
		return err
	}
	required := []string{"target_kind", "target"}
	if revalidation {
		required = append(required, "evidence_ref")
	} else {
		required = append(required, "reason")
	}
	for _, key := range required {
		if err := requirePayloadString(ev, key, true); err != nil {
			return err
		}
	}
	kind := MemoryTargetKind(ev.PayloadString("target_kind"))
	switch kind {
	case TargetRecord, TargetEvent, TargetPage:
		if !recordTargetRE.MatchString(ev.PayloadString("target")) {
			return invalidPayloadField(ev, "target", "must be a session-local logical address")
		}
	case TargetPath:
		if err := validateCanonicalPathPayload(ev, "target"); err != nil {
			return err
		}
	default:
		return invalidPayloadField(ev, "target_kind", "must be record, event, page, or path")
	}
	if revalidation {
		if kind == TargetEvent {
			return invalidPayloadField(ev, "target_kind", "event targets are immutable and cannot be revalidated")
		}
		if !recordTargetRE.MatchString(ev.PayloadString("evidence_ref")) {
			return invalidPayloadField(ev, "evidence_ref", "must be a session-local record/event id")
		}
		revision, ok := integerPayload(ev.Payload["expected_stale_revision"])
		if !ok || revision <= 0 {
			return invalidPayloadField(ev, "expected_stale_revision", "must be a positive integer")
		}
		if kind == TargetPage || kind == TargetPath {
			return validateDigestPayload(ev, "source_digest")
		}
		if _, exists := ev.Payload["source_digest"]; exists {
			return invalidPayloadField(ev, "source_digest", "is only valid for page or path targets")
		}
		return nil
	}
	switch InvalidationReason(ev.PayloadString("reason")) {
	case ReasonSuperseded, ReasonUserRejected, ReasonSourceDeleted, ReasonPolicyChanged, ReasonSecurityRevoked:
		return nil
	default:
		return invalidPayloadField(ev, "reason", "must be superseded, user_rejected, source_deleted, policy_changed, or security_revoked")
	}
}

func validatePayloadKeys(ev Event, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	for key := range ev.Payload {
		if !set[key] {
			return invalidPayloadField(ev, key, "is not allowed by this event contract")
		}
	}
	return nil
}

func validateRequiredEnvelopeScope(ev Event, repo, branch, commit bool) error {
	for _, required := range []struct {
		name  string
		value string
		need  bool
	}{
		{name: "repo", value: ev.Repo, need: repo},
		{name: "branch", value: ev.Branch, need: branch},
		{name: "commit", value: ev.Commit, need: commit},
	} {
		if !required.need {
			continue
		}
		if err := validateScopeToken(ev, required.name, required.value); err != nil {
			return err
		}
	}
	return nil
}

func validateCanonicalPathPayload(ev Event, key string) error {
	if err := requirePayloadString(ev, key, true); err != nil {
		return err
	}
	value := ev.PayloadString(key)
	cleaned, err := NormalizeRepoPath(value)
	if err != nil {
		return invalidPayloadField(ev, key, err.Error())
	}
	if cleaned != value {
		return invalidPayloadField(ev, key, "must already be canonical")
	}
	return nil
}

func validateDigestPayload(ev Event, key string) error {
	if err := requirePayloadString(ev, key, true); err != nil {
		return err
	}
	digest := ev.PayloadString(key)
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return invalidPayloadField(ev, key, "must be a lowercase SHA-256 digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return invalidPayloadField(ev, key, "must be a lowercase SHA-256 digest")
	}
	return nil
}

func validateScopeToken(ev Event, key, value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 256 {
		return invalidPayloadField(ev, key, "must be a trimmed non-empty token of at most 256 bytes")
	}
	if strings.ContainsAny(value, "\x00\r\n\t") {
		return invalidPayloadField(ev, key, "must not contain control characters")
	}
	return nil
}

// NormalizeRepoPath validates and canonicalizes a repository-relative slash
// path. It rejects absolute, traversal, Windows-separator, and control inputs.
func NormalizeRepoPath(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 4096 {
		return "", errors.New("must be a trimmed repository-relative path")
	}
	if strings.Contains(value, `\`) || strings.HasPrefix(value, "/") || isWindowsDrivePath(value) {
		return "", errors.New("must not be absolute or contain backslash/control characters")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("must not contain control characters")
		}
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("must not traverse outside the repository")
	}
	return cleaned, nil
}

func isWindowsDrivePath(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && value[2] == '/'
}

func validateCommandOutput(ev Event, allowCommandAlias bool) error {
	if _, hasCmd := ev.Payload["cmd"]; hasCmd || !allowCommandAlias {
		if err := requirePayloadString(ev, "cmd", true); err != nil {
			return err
		}
	} else if err := requirePayloadString(ev, "command", true); err != nil {
		return invalidPayloadField(ev, "cmd", "or payload field \"command\" is required")
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
