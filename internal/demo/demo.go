// Package demo runs the deterministic, provider-free WSMS mechanism proof.
package demo

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"wsms/internal/artifacts"
	"wsms/internal/config"
	wsmserrors "wsms/internal/errors"
	"wsms/internal/faults"
	"wsms/internal/harness"
	"wsms/internal/ledger"
	"wsms/internal/renderer"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

const (
	primarySessionID   = "demo-primary"
	secondarySessionID = "demo-secondary"

	demoGoal       = "preserve exact failure evidence across restart"
	demoGoalRender = "Preserve exact failure evidence across restart"
	demoConstraint = "do not rewrite transport layer"
	demoCommand    = "go test ./runtime -run TestCancelStream"
	demoFailure    = "error: stream goroutine still blocked"
	demoAvoid      = "retrying the failed transport rewrite"
	demoTarget     = "src/runtime/stream.go:118-176"
	rawSentinel    = "RAW-EVIDENCE-AFTER-PREVIEW"
	loopUser       = "Continue with the selected in-place cancellation fix."
	loopReply      = "continue: inspect the cancellation waiter without rewriting transport"
)

// Options controls where the demo writes its durable ledger and artifacts.
// An empty DataDir uses a fresh temporary directory that is removed only after
// every assertion and resource close succeeds. An explicit directory is never
// removed, making the resulting ledger available for operator inspection.
type Options struct {
	DataDir string
}

// Run executes the complete vector-free ledger -> WSL -> capsule -> page-in
// vertical slice. It prints DEMO PASS only after all assertions and cleanup
// that can affect the command's exit status have succeeded.
func Run(ctx context.Context, out io.Writer, opts Options) (retErr error) {
	if ctx == nil {
		return errors.New("demo context is required")
	}
	if out == nil {
		return errors.New("demo output writer is required")
	}

	dataDir, temporary, err := prepareDataDir(opts.DataDir)
	if err != nil {
		return err
	}
	cleanupDir := temporary
	if !temporary {
		if err := requireReservedPathsAvailable(dataDir); err != nil {
			return err
		}
	}
	var primary, secondary *harness.Session
	defer func() {
		if secondary != nil {
			retErr = errors.Join(retErr, secondary.Close())
		}
		if primary != nil {
			retErr = errors.Join(retErr, primary.Close())
		}
		if cleanupDir {
			retErr = errors.Join(retErr, os.RemoveAll(dataDir))
		}
	}()

	locationKind := "persistent"
	if temporary {
		locationKind = "temporary"
	}
	if err := reportf(out, "DEMO DATA: %s (%s)\n", dataDir, locationKind); err != nil {
		return err
	}

	cfg := demoConfig(dataDir, primarySessionID)
	primary, err = harness.OpenSession(cfg)
	if err != nil {
		return fmt.Errorf("open primary session: %w", err)
	}
	if err := requireEmptySession(ctx, primary); err != nil {
		return err
	}
	secondaryCfg := demoConfig(dataDir, secondarySessionID)
	secondary, err = harness.OpenSession(secondaryCfg)
	if err != nil {
		return fmt.Errorf("open secondary session: %w", err)
	}
	if err := requireEmptySession(ctx, secondary); err != nil {
		return err
	}

	taskEvent, err := primary.Append(ctx, ledger.Event{
		Type:   ledger.EventTaskStarted,
		Branch: "main",
		Commit: "demo-commit",
		Payload: map[string]any{
			"goal":     demoGoal,
			"phase":    "debugging",
			"priority": string(types.PriorityHot),
			"branch":   "main",
			"dirty":    "src/runtime/stream.go",
		},
	})
	if err != nil {
		return fmt.Errorf("append task event: %w", err)
	}
	task := primary.State.ActiveTask()
	if task == nil || task.IDValue != "T1" || task.Goal != demoGoal {
		return fmt.Errorf("task mapping mismatch: %#v", task)
	}
	if err := requireEvidence(primary.State, task.IDValue, taskEvent.ID); err != nil {
		return err
	}
	if err := reportf(out, "BACKING STORE: EVENT %s %s -> %s\n", taskEvent.ID, taskEvent.Type, task.IDValue); err != nil {
		return err
	}

	constraintEvent, err := primary.Append(ctx, ledger.Event{
		Type:    ledger.EventUserInstruction,
		Payload: map[string]any{"text": demoConstraint},
	})
	if err != nil {
		return fmt.Errorf("append hard constraint: %w", err)
	}
	constraints := primary.State.HardConstraints()
	if len(constraints) != 1 || constraints[0].IDValue != "C1" || constraints[0].Text != demoConstraint {
		return fmt.Errorf("hard-constraint mapping mismatch: %#v", constraints)
	}
	if err := requireEvidence(primary.State, "C1", constraintEvent.ID); err != nil {
		return err
	}
	if err := reportf(out, "BACKING STORE: EVENT %s %s -> C1 hard\n", constraintEvent.ID, constraintEvent.Type); err != nil {
		return err
	}

	rawOutput := demoRawOutput()
	failureEvent, err := primary.Append(ctx, ledger.Event{
		Type: ledger.EventCommandOutput,
		Payload: map[string]any{
			"cmd":    demoCommand,
			"exit":   1,
			"output": rawOutput,
			"err":    demoFailure,
		},
	})
	if err != nil {
		return fmt.Errorf("append command failure: %w", err)
	}
	failure := primary.State.LastFailure()
	if failure == nil || failure.IDValue != "F1" || failure.Cmd != demoCommand || failure.Err != demoFailure || failure.Exit != 1 {
		return fmt.Errorf("failure mapping mismatch: %#v", failure)
	}
	if err := requireEvidence(primary.State, "F1", failureEvent.ID); err != nil {
		return err
	}
	artifactRef := failureEvent.PayloadString("raw")
	artifactHash, ok := artifacts.ParseRef(artifactRef)
	if !ok || artifactHash != failureEvent.ArtifactHash {
		return fmt.Errorf("artifact reference/hash mismatch: ref=%q hash=%q", artifactRef, failureEvent.ArtifactHash)
	}
	if strings.Contains(failureEvent.PayloadString("output"), rawSentinel) {
		return errors.New("raw sentinel unexpectedly remained inside the bounded ledger preview")
	}
	if err := reportf(out, "BACKING STORE: EVENT %s %s exit=1 -> F1\n", failureEvent.ID, failureEvent.Type); err != nil {
		return err
	}
	if err := reportf(out, "BACKING STORE: ARTIFACT sha256:%s (raw output offloaded)\n", artifactHash); err != nil {
		return err
	}

	decisionEvent, err := primary.Append(ctx, ledger.Event{
		Type: ledger.EventDecision,
		Payload: map[string]any{
			"chosen":     "patch cancellation cleanup within the existing boundary",
			"because":    "F1 preserves the exact blocked-waiter failure",
			"refs":       "F1",
			"scope":      string(types.ScopeTask),
			"avoid_text": demoAvoid,
			"avoid_ref":  "F1",
		},
	})
	if err != nil {
		return fmt.Errorf("append decision: %w", err)
	}
	decisions := primary.State.Decisions()
	avoids := primary.State.Avoids()
	if len(decisions) != 1 || decisions[0].IDValue != "D1" || len(avoids) != 1 || avoids[0].IDValue != "A1" || avoids[0].Ref != "F1" {
		return fmt.Errorf("decision/avoid mapping mismatch: decisions=%#v avoids=%#v", decisions, avoids)
	}
	if err := requireEvidence(primary.State, "D1", decisionEvent.ID); err != nil {
		return err
	}
	if err := requireEvidence(primary.State, "A1", decisionEvent.ID); err != nil {
		return err
	}
	if err := reportf(out, "BACKING STORE: EVENT %s %s -> D1,A1\n", decisionEvent.ID, decisionEvent.Type); err != nil {
		return err
	}

	nextEvent, err := primary.Append(ctx, ledger.Event{
		Type: ledger.EventNextAction,
		Payload: map[string]any{
			"action":   "inspect",
			"target":   demoTarget,
			"question": "does the waiter exit after cancellation?",
		},
	})
	if err != nil {
		return fmt.Errorf("append next action: %w", err)
	}
	next := primary.State.Next()
	if next == nil || next.Action != "inspect" || next.Target != demoTarget {
		return fmt.Errorf("next-action mapping mismatch: %#v", next)
	}
	if err := requireEvidence(primary.State, "next", nextEvent.ID); err != nil {
		return err
	}
	if err := reportf(out, "BACKING STORE: EVENT %s %s -> next\n", nextEvent.ID, nextEvent.Type); err != nil {
		return err
	}

	expectedProvenance := map[string]string{
		"T1": taskEvent.ID, "C1": constraintEvent.ID, "F1": failureEvent.ID,
		"D1": decisionEvent.ID, "A1": decisionEvent.ID, "next": nextEvent.ID,
	}
	if err := requireProvenance(primary.State, expectedProvenance); err != nil {
		return err
	}
	if err := reportf(out, "PAGE TABLE: T1<-%s C1<-%s F1<-%s D1<-%s A1<-%s next<-%s\n",
		taskEvent.ID, constraintEvent.ID, failureEvent.ID, decisionEvent.ID, decisionEvent.ID, nextEvent.ID); err != nil {
		return err
	}

	beforeState := wsl.Serialize(primary.State.Records())
	beforeProvenance := primary.State.Provenance()
	beforeCapsule, err := primary.BeforeTurn(ctx)
	if err != nil {
		return fmt.Errorf("render resident working set: %w", err)
	}
	if err := requireCapsule(beforeCapsule); err != nil {
		return err
	}
	if tokens := renderer.EstimateTokens(beforeCapsule); tokens > cfg.CapsuleTokenBudget {
		return fmt.Errorf("resident capsule exceeds budget: estimated=%d budget=%d", tokens, cfg.CapsuleTokenBudget)
	}
	if err := reportf(out, "RESIDENT WORKING SET: CAPSULE BEFORE REOPEN (%d estimated tokens)\n%s", renderer.EstimateTokens(beforeCapsule), beforeCapsule); err != nil {
		return err
	}

	beforeEvents, err := primary.Ledger.ListBySession(ctx, primarySessionID)
	if err != nil {
		return fmt.Errorf("list primary events before reopen: %w", err)
	}
	if len(beforeEvents) != 5 {
		return fmt.Errorf("primary event count before reopen=%d, want 5", len(beforeEvents))
	}
	if err := primary.Close(); err != nil {
		return fmt.Errorf("close primary session before runtime reset: %w", err)
	}
	primary = nil
	if err := reportf(out, "=== SESSION RUNTIME CLOSED / MEMORY DROPPED / REOPENED ===\n"); err != nil {
		return err
	}

	primary, err = harness.OpenSession(cfg)
	if err != nil {
		return fmt.Errorf("reopen primary session: %w", err)
	}
	afterEvents, err := primary.Ledger.ListBySession(ctx, primarySessionID)
	if err != nil {
		return fmt.Errorf("list primary events after reopen: %w", err)
	}
	if len(afterEvents) != len(beforeEvents) {
		return fmt.Errorf("replay changed durable event count: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}
	if afterState := wsl.Serialize(primary.State.Records()); afterState != beforeState {
		return fmt.Errorf("replayed WSL differs from live WSL\nlive:\n%s\nreplayed:\n%s", beforeState, afterState)
	}
	if afterProvenance := primary.State.Provenance(); !reflect.DeepEqual(afterProvenance, beforeProvenance) {
		return fmt.Errorf("replayed provenance differs: live=%v replayed=%v", beforeProvenance, afterProvenance)
	}
	if err := requireProvenance(primary.State, expectedProvenance); err != nil {
		return err
	}
	afterCapsule, err := primary.BeforeTurn(ctx)
	if err != nil {
		return fmt.Errorf("render replayed resident working set: %w", err)
	}
	if afterCapsule != beforeCapsule {
		return fmt.Errorf("reopened capsule differs from pre-restart capsule\nlive:\n%s\nreplayed:\n%s", beforeCapsule, afterCapsule)
	}
	if err := requireCapsule(afterCapsule); err != nil {
		return err
	}
	if err := reportf(out, "PAGE TABLE: DERIVED MAPPINGS RECONSTRUCTED (6/6 provenance links)\n"); err != nil {
		return err
	}
	if err := reportf(out, "RESIDENT WORKING SET: REOPENED CAPSULE VERIFIED\n"); err != nil {
		return err
	}

	page, err := primary.Tools.ReadPage(ctx, "F1", cfg.PageFaultTokenBudget)
	if err != nil {
		return fmt.Errorf("page-in F1: %w", err)
	}
	if page == faults.PageMiss || !strings.Contains(page, demoCommand) || !strings.Contains(page, "Exit: 1") || !strings.Contains(page, demoFailure) {
		return fmt.Errorf("structured F1 page mismatch: %q", page)
	}
	if err := reportf(out, "PAGE FAULT F1: PAGE-IN HIT (structured command/exit/error verified)\n"); err != nil {
		return err
	}

	raw, err := primary.Tools.ReadRawLog(ctx, "F1", 4096)
	if err != nil {
		return fmt.Errorf("read exact raw log F1: %w", err)
	}
	if raw != rawOutput || !strings.Contains(raw, rawSentinel) {
		return errors.New("recovered F1 raw log does not match the full offloaded bytes")
	}
	recomputed := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
	if recomputed != artifactHash {
		return fmt.Errorf("independent SHA-256 mismatch: recomputed=%s stored=%s", recomputed, artifactHash)
	}
	if err := reportf(out, "BACKING STORE F1: SHA256 VERIFIED (%s)\n", recomputed); err != nil {
		return err
	}
	if err := reportf(out, "BACKING STORE F1: RAW SENTINEL VERIFIED beyond ledger preview\n"); err != nil {
		return err
	}

	secondaryEvent, err := secondary.Append(ctx, ledger.Event{
		Type: ledger.EventTaskStarted,
		Payload: map[string]any{
			"goal":     "prove same-database session isolation",
			"phase":    "verification",
			"priority": string(types.PriorityHot),
		},
	})
	if err != nil {
		return fmt.Errorf("append secondary task: %w", err)
	}
	if secondaryEvent.ID != "E0001" {
		return fmt.Errorf("secondary first event=%s, want independent E0001", secondaryEvent.ID)
	}
	if err := requireProvenance(secondary.State, map[string]string{"T1": secondaryEvent.ID}); err != nil {
		return fmt.Errorf("secondary task provenance: %w", err)
	}
	primaryE1, err := primary.Ledger.Get(ctx, "E0001")
	if err != nil {
		return fmt.Errorf("get primary E0001: %w", err)
	}
	secondaryE1, err := secondary.Ledger.Get(ctx, "E0001")
	if err != nil {
		return fmt.Errorf("get secondary E0001: %w", err)
	}
	if primaryE1.SessionID == secondaryE1.SessionID || primaryE1.PayloadString("goal") == secondaryE1.PayloadString("goal") {
		return fmt.Errorf("session-scoped E0001 identities leaked: primary=%#v secondary=%#v", primaryE1, secondaryE1)
	}
	if _, err := primary.Ledger.ListBySession(ctx, secondarySessionID); err == nil {
		return errors.New("primary ledger listed the secondary session")
	}
	if _, err := secondary.Ledger.ListBySession(ctx, primarySessionID); err == nil {
		return errors.New("secondary ledger listed the primary session")
	}
	if _, err := secondary.Ledger.Get(ctx, "E0005"); !errors.Is(err, wsmserrors.ErrNotFound) {
		return fmt.Errorf("secondary lookup of primary-only E0005 error=%v, want not found", err)
	}
	if _, err := primary.Append(ctx, ledger.Event{
		Type:      ledger.EventAssistantMessage,
		SessionID: secondarySessionID,
		Payload:   map[string]any{"text": "must be rejected as a foreign append"},
	}); err == nil {
		return errors.New("primary session accepted a foreign-session append")
	}
	if _, err := secondary.Append(ctx, ledger.Event{
		Type:      ledger.EventAssistantMessage,
		SessionID: primarySessionID,
		Payload:   map[string]any{"text": "must be rejected in the reverse direction"},
	}); err == nil {
		return errors.New("secondary session accepted a foreign-session append")
	}
	primaryIsolationEvents, err := primary.Ledger.ListBySession(ctx, primarySessionID)
	if err != nil || len(primaryIsolationEvents) != 5 {
		return fmt.Errorf("primary event count changed during isolation probes: count=%d err=%v", len(primaryIsolationEvents), err)
	}
	secondaryIsolationEvents, err := secondary.Ledger.ListBySession(ctx, secondarySessionID)
	if err != nil || len(secondaryIsolationEvents) != 1 {
		return fmt.Errorf("secondary event count changed during isolation probes: count=%d err=%v", len(secondaryIsolationEvents), err)
	}
	if got := primary.State.ActiveTask(); got == nil || got.Goal != demoGoal {
		return fmt.Errorf("secondary state contaminated primary task: %#v", got)
	}
	if got := secondary.State.ActiveTask(); got == nil || got.Goal != "prove same-database session isolation" {
		return fmt.Errorf("primary state contaminated secondary task: %#v", got)
	}
	if err := reportf(out, "SESSION ISOLATION: VERIFIED independent E0001 identities and scoped access\n"); err != nil {
		return err
	}

	client := &verifyingClient{
		required:     []string{demoGoalRender, demoConstraint, demoCommand, demoFailure, demoAvoid, demoTarget, renderer.PageFaultInstruction},
		expectedUser: loopUser,
	}
	loop := &harness.Loop{Session: primary, Client: client}
	assistant, loopCapsule, err := loop.Turn(ctx, loopUser)
	if err != nil {
		return fmt.Errorf("foreground client turn: %w", err)
	}
	if client.calls != 1 || loopCapsule != afterCapsule || client.capsule != afterCapsule {
		return errors.New("foreground client did not receive the reconstructed capsule through harness.Loop")
	}
	if assistant != loopReply {
		return fmt.Errorf("unexpected deterministic client response %q", assistant)
	}
	turnEvents, err := primary.Ledger.ListBySession(ctx, primarySessionID)
	if err != nil {
		return fmt.Errorf("list primary events after foreground turn: %w", err)
	}
	if len(turnEvents) != 7 || turnEvents[5].ID != "E0006" || turnEvents[5].Type != ledger.EventUserInstruction || turnEvents[5].PayloadString("text") != loopUser ||
		turnEvents[6].ID != "E0007" || turnEvents[6].Type != ledger.EventAssistantMessage || turnEvents[6].PayloadString("text") != loopReply {
		return fmt.Errorf("foreground turn was not durably recorded as E0006/E0007: %#v", turnEvents)
	}
	if err := reportf(out, "CLIENT: CAPSULE RECEIVED through harness.Loop\n"); err != nil {
		return err
	}
	if err := reportf(out, "CLIENT: CONTINUATION RECEIVED %q\n", assistant); err != nil {
		return err
	}

	if err := secondary.Close(); err != nil {
		return fmt.Errorf("close secondary session: %w", err)
	}
	secondary = nil
	if err := primary.Close(); err != nil {
		return fmt.Errorf("close reopened primary session: %w", err)
	}
	primary = nil
	if cleanupDir {
		if err := os.RemoveAll(dataDir); err != nil {
			return fmt.Errorf("remove temporary demo data: %w", err)
		}
		cleanupDir = false
	}

	return reportf(out, "DEMO PASS\n")
}

func demoConfig(dataDir, sessionID string) config.Config {
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.SessionID = sessionID
	cfg.ArtifactThresholdBytes = 64
	cfg.CapsuleTokenBudget = 512
	cfg.PageFaultTokenBudget = 512
	return cfg
}

func prepareDataDir(requested string) (path string, temporary bool, err error) {
	if requested == "" {
		path, err = os.MkdirTemp("", "wsms-demo-")
		if err != nil {
			return "", false, fmt.Errorf("create temporary demo data directory: %w", err)
		}
		return path, true, nil
	}
	path, err = filepath.Abs(requested)
	if err != nil {
		return "", false, fmt.Errorf("resolve demo data directory: %w", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", false, fmt.Errorf("create demo data directory: %w", err)
	}
	return path, false, nil
}

func requireReservedPathsAvailable(dataDir string) error {
	for _, name := range []string{"ledger.db", "ledger.db-journal", "ledger.db-wal", "ledger.db-shm", "artifacts"} {
		path := filepath.Join(dataDir, name)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("reserved demo path already exists: %s; choose a fresh --data-dir", path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect reserved demo path %s: %w", path, err)
		}
	}
	return nil
}

func demoRawOutput() string {
	return demoFailure + "\n" +
		"src/runtime/stream.go:118: cancellation waiter did not exit\n" +
		strings.Repeat("diagnostic context retained in durable backing storage ", 32) +
		"\n" + rawSentinel
}

func requireEmptySession(ctx context.Context, session *harness.Session) error {
	events, err := session.Ledger.ListBySession(ctx, session.Cfg.SessionID)
	if err != nil {
		return fmt.Errorf("inspect demo session %q: %w", session.Cfg.SessionID, err)
	}
	if len(events) != 0 {
		return fmt.Errorf("demo session %q already contains %d events; choose a fresh --data-dir", session.Cfg.SessionID, len(events))
	}
	return nil
}

func requireEvidence(state *wsl.WorkingState, recordID, wantEventID string) error {
	got, ok := state.EvidenceID(recordID)
	if !ok {
		return fmt.Errorf("page-table mapping %s has no durable provenance", recordID)
	}
	if got != wantEventID {
		return fmt.Errorf("page-table mapping %s points to %s, want %s", recordID, got, wantEventID)
	}
	return nil
}

func requireProvenance(state *wsl.WorkingState, expected map[string]string) error {
	got := state.Provenance()
	if !reflect.DeepEqual(got, expected) {
		return fmt.Errorf("page-table provenance mismatch: got=%v want=%v", got, expected)
	}
	for recordID, eventID := range expected {
		if err := requireEvidence(state, recordID, eventID); err != nil {
			return err
		}
	}
	return nil
}

func requireCapsule(capsule string) error {
	required := []string{
		"TASK T1", demoGoalRender, "HARD CONSTRAINT C1", demoConstraint,
		"LAST FAILURE F1", demoCommand, demoFailure, "AVOID A1", demoAvoid,
		"NEXT", demoTarget, renderer.PageFaultInstruction,
	}
	for _, text := range required {
		if !strings.Contains(capsule, text) {
			return fmt.Errorf("resident capsule is missing %q:\n%s", text, capsule)
		}
	}
	if strings.Contains(capsule, rawSentinel) {
		return errors.New("resident capsule leaked the offloaded raw sentinel")
	}
	return nil
}

func reportf(out io.Writer, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	n, err := io.WriteString(out, message)
	if err != nil {
		return fmt.Errorf("write demo evidence: %w", err)
	}
	if n != len(message) {
		return fmt.Errorf("write demo evidence: %w", io.ErrShortWrite)
	}
	return nil
}

type verifyingClient struct {
	required     []string
	expectedUser string
	calls        int
	capsule      string
}

func (c *verifyingClient) Chat(ctx context.Context, messages []harness.Message) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(messages) != 2 || messages[0].Role != "system" || messages[1].Role != "user" {
		return "", fmt.Errorf("unexpected foreground message envelope: %#v", messages)
	}
	if c.calls != 0 {
		return "", errors.New("deterministic client called more than once")
	}
	if messages[1].Content != c.expectedUser {
		return "", fmt.Errorf("unexpected foreground user message %q", messages[1].Content)
	}
	for _, text := range c.required {
		if !strings.Contains(messages[0].Content, text) {
			return "", fmt.Errorf("client capsule missing %q", text)
		}
	}
	c.calls++
	c.capsule = messages[0].Content
	return loopReply, nil
}
