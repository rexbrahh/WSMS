// Package harness wires ledger, observers, WSL, scheduler, and faults.
package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"wsms/internal/artifacts"
	"wsms/internal/config"
	"wsms/internal/faults"
	"wsms/internal/ledger"
	"wsms/internal/memory"
	"wsms/internal/observers"
	"wsms/internal/scheduler"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

var (
	// ErrSessionUnavailable reports a fail-stop session whose durable append was
	// committed but whose derived mapping failed.
	ErrSessionUnavailable = errors.New("session unavailable after derivation failure")
	// ErrSessionClosed reports an operation attempted after Session.Close.
	ErrSessionClosed = errors.New("session closed")
)

// TaskStart is the explicit payload for a durable task_started event.
type TaskStart struct {
	Goal     string
	TaskID   string
	Phase    string
	Priority types.Priority
	Branch   string
	Commit   string
	Dirty    string
}

// NextAction is the explicit payload for the singleton durable next action.
type NextAction struct {
	Action   string
	Target   string
	Question string
}

// DecisionInput is the explicit payload for a durable decision and optional
// failure-grounded avoidance.
type DecisionInput struct {
	Chosen    string
	Because   string
	Refs      string
	Scope     types.Scope
	AvoidText string
	AvoidRef  string
}

// Session is the composition root for a WSMS session.
type Session struct {
	appendMu    sync.Mutex
	closeOnce   sync.Once
	closeErr    error
	closed      bool
	failStopErr error
	Cfg         config.Config
	Ledger      *ledger.AppendOnlyLedger
	Artifacts   *artifacts.Store
	State       *wsl.WorkingState
	Hierarchy   *memory.Hierarchy
	Dispatcher  *observers.Dispatcher
	Scheduler   *scheduler.Scheduler
	Tools       *faults.Tools
}

// OpenSession creates a session under cfg.DataDir.
func OpenSession(cfg config.Config) (*Session, error) {
	if cfg.DataDir == "" {
		cfg = config.Default()
	}
	if cfg.SessionID == "" {
		cfg.SessionID = "session-default"
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(cfg.DataDir, "ledger.db")
	artPath := filepath.Join(cfg.DataDir, "artifacts")

	led, err := ledger.Open(dbPath, cfg.SessionID)
	if err != nil {
		return nil, err
	}
	arts, err := artifacts.Open(artPath)
	if err != nil {
		return nil, errors.Join(err, led.Close())
	}
	st := wsl.NewWorkingState()
	h := memory.NewHierarchy()
	ids := observers.NewSeqIDGen()
	disp := observers.Default(ids)
	res := &faults.Resolver{
		State:     st,
		Hierarchy: h,
		Ledger:    led,
		Artifacts: arts,
	}
	sched := scheduler.New(cfg, st, h, disp, res)
	s := &Session{
		Cfg:        cfg,
		Ledger:     led,
		Artifacts:  arts,
		State:      st,
		Hierarchy:  h,
		Dispatcher: disp,
		Scheduler:  sched,
		Tools:      &faults.Tools{Resolver: res},
	}
	if err := s.replay(context.Background()); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

// replay reconstructs derived state from durable events without appending them.
func (s *Session) replay(ctx context.Context) error {
	events, err := s.Ledger.ListBySession(ctx, s.Cfg.SessionID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if err := s.Scheduler.AfterEvent(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// Close releases resources.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.appendMu.Lock()
		defer s.appendMu.Unlock()
		s.closed = true
		s.closeErr = errors.Join(s.Ledger.Close(), s.Artifacts.Close())
	})
	return s.closeErr
}

// Append records an event, optionally offloads large output, then runs AfterEvent.
func (s *Session) Append(ctx context.Context, ev ledger.Event) (ledger.Event, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if err := s.operationErrorLocked(); err != nil {
		return ledger.Event{}, err
	}
	if err := ledger.ValidateEvent(ev); err != nil {
		return ledger.Event{}, fmt.Errorf("validate event before append: %w", err)
	}

	// Offload large string fields
	if out, ok := ev.Payload["output"].(string); ok && len(out) > s.Cfg.ArtifactThresholdBytes {
		meta, err := s.Artifacts.Put([]byte(out), "text/plain")
		if err != nil {
			return ledger.Event{}, err
		}
		ev.ArtifactHash = meta.SHA256
		ev.Payload["output"] = out[:min(200, len(out))] + "…"
		ev.Payload["raw"] = artifacts.Ref(meta.SHA256)
	}
	stored, err := s.Ledger.Append(ctx, ev)
	if err != nil {
		return ledger.Event{}, err
	}
	if err := s.Scheduler.AfterEvent(ctx, stored); err != nil {
		s.failStopErr = fmt.Errorf(
			"%w: durable event %s: %w",
			ErrSessionUnavailable,
			stored.ID,
			err,
		)
		return stored, s.failStopErr
	}
	return stored, nil
}

// BeforeTurn returns the L1 capsule.
func (s *Session) BeforeTurn(ctx context.Context) (string, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if err := s.operationErrorLocked(); err != nil {
		return "", err
	}
	return s.Scheduler.BeforeTurn(ctx)
}

func (s *Session) operationErrorLocked() error {
	if s.closed {
		return ErrSessionClosed
	}
	return s.failStopErr
}

// PageFault demand-fetches a page.
func (s *Session) PageFault(ctx context.Context, id string) (string, error) {
	return s.Tools.ReadPage(ctx, id, s.Cfg.PageFaultTokenBudget)
}

// IngestUser appends a user_instruction event.
func (s *Session) IngestUser(ctx context.Context, text string) error {
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventUserInstruction,
		Payload: map[string]any{
			"text": text,
		},
	})
	return err
}

// IngestAssistant appends an assistant_message event (no observer side effects required).
func (s *Session) IngestAssistant(ctx context.Context, text string) error {
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventAssistantMessage,
		Payload: map[string]any{
			"text": text,
		},
	})
	return err
}

// IngestCommandOutput appends a failed/successful command output.
func (s *Session) IngestCommandOutput(ctx context.Context, cmd string, exit int, output string) error {
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventCommandOutput,
		Payload: map[string]any{
			"cmd":    cmd,
			"exit":   exit,
			"output": output,
			"err":    firstErrLine(output),
		},
	})
	return err
}

// StartTask appends an explicit task_started event; task state is observer-derived.
func (s *Session) StartTask(ctx context.Context, task TaskStart) error {
	_, err := s.Append(ctx, ledger.Event{
		Type:   ledger.EventTaskStarted,
		TaskID: task.TaskID,
		Branch: task.Branch,
		Commit: task.Commit,
		Payload: map[string]any{
			"goal":     task.Goal,
			"task_id":  task.TaskID,
			"phase":    task.Phase,
			"priority": string(task.Priority),
			"branch":   task.Branch,
			"dirty":    task.Dirty,
		},
	})
	return err
}

// SetNext appends an explicit next_action event; the observer replaces @next.
func (s *Session) SetNext(ctx context.Context, next NextAction) error {
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventNextAction,
		Payload: map[string]any{
			"action":   next.Action,
			"target":   next.Target,
			"question": next.Question,
		},
	})
	return err
}

// RecordDecision appends an explicit decision event with optional avoidance.
func (s *Session) RecordDecision(ctx context.Context, decision DecisionInput) error {
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventDecision,
		Payload: map[string]any{
			"chosen":     decision.Chosen,
			"because":    decision.Because,
			"refs":       decision.Refs,
			"scope":      string(decision.Scope),
			"avoid_text": decision.AvoidText,
			"avoid_ref":  decision.AvoidRef,
		},
	})
	return err
}

func firstErrLine(output string) string {
	for _, line := range splitLines(output) {
		if line != "" {
			return line
		}
	}
	return ""
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
