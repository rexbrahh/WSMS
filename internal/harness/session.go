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
	"wsms/internal/coherence"
	"wsms/internal/config"
	"wsms/internal/faults"
	"wsms/internal/indexer"
	"wsms/internal/ledger"
	"wsms/internal/memory"
	"wsms/internal/observers"
	"wsms/internal/pages"
	"wsms/internal/retrieval"
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
	Repo     string
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

// BranchChange is a compare-current transition to the post-scope carried in
// the durable event envelope. Commit fields are optional but must be paired.
type BranchChange struct {
	Repo       string
	FromBranch string
	ToBranch   string
	FromCommit string
	ToCommit   string
}

// CommitChange moves the active branch from one exact commit to another.
type CommitChange struct {
	Repo       string
	Branch     string
	FromCommit string
	ToCommit   string
}

// FileRename records a scope-coherence rename. ContentDigest is optional, but
// when supplied it is a lowercase SHA-256 digest.
type FileRename struct {
	Repo          string
	Branch        string
	FromPath      string
	ToPath        string
	ContentDigest string
}

// FileSnapshot is the strict Phase 7A-ready path/content evidence contract;
// legacy file_read events remain untouched for replay compatibility.
type FileSnapshot struct {
	Repo          string
	Branch        string
	Commit        string
	Path          string
	ContentDigest string
}

// MemoryInvalidation terminally revokes one existing logical target.
type MemoryInvalidation struct {
	Kind   ledger.MemoryTargetKind
	Target string
	Reason ledger.InvalidationReason
}

// MemoryRevalidation compare-and-swaps one stale (never invalidated) target
// back to active using preexisting eligible evidence.
type MemoryRevalidation struct {
	Kind                  ledger.MemoryTargetKind
	Target                string
	EvidenceRef           string
	ExpectedStaleRevision uint64
	SourceDigest          string
}

// Session is the composition root for a WSMS session.
type Session struct {
	appendMu    sync.Mutex
	closeOnce   sync.Once
	closeErr    error
	closed      bool
	failStopErr error
	// IndexErr is the last non-fatal warm-index error (indexing never fails L4).
	IndexErr   error
	Cfg        config.Config
	Ledger     *ledger.AppendOnlyLedger
	Artifacts  *artifacts.Store
	State      *wsl.WorkingState
	Coherence  *coherence.State
	Hierarchy  *memory.Hierarchy
	Dispatcher *observers.Dispatcher
	Scheduler  *scheduler.Scheduler
	Tools      *faults.Tools
	// Index is the disposable L3 warm index. Nil when unavailable.
	Index *indexer.Index
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
	coherent := coherence.NewState()
	ids := observers.NewSeqIDGen()
	disp := observers.Default(ids, st)
	res := &faults.Resolver{
		State:     st,
		Hierarchy: h,
		Ledger:    led,
		Artifacts: arts,
		Coherence: coherent,
	}
	sched := scheduler.New(cfg, st, h, disp, res, coherent)
	s := &Session{
		Cfg:        cfg,
		Ledger:     led,
		Artifacts:  arts,
		State:      st,
		Coherence:  coherent,
		Hierarchy:  h,
		Dispatcher: disp,
		Scheduler:  sched,
		Tools:      faults.NewTools(res, sched.PageFault),
	}
	// Warm index is disposable and optional. Open failures leave Index nil.
	// Dense projection is off by default (cfg.DenseDimensions == 0).
	if warm, err := indexer.Open(filepath.Join(cfg.DataDir, "index"), indexer.Options{
		DenseDimensions: cfg.DenseDimensions,
	}); err == nil {
		s.Index = warm
	} else {
		s.IndexErr = err
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
		s.indexAfterEvent(ctx, ev)
	}
	return nil
}

// Close releases resources.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.appendMu.Lock()
		defer s.appendMu.Unlock()
		s.closed = true
		var indexErr error
		if s.Index != nil {
			indexErr = s.Index.Close()
			s.Index = nil
		}
		s.closeErr = errors.Join(s.Ledger.Close(), s.Artifacts.Close(), indexErr)
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
	// Reject store-owned or foreign identity fields before artifact offload so
	// a request the ledger cannot append cannot leave an orphan blob behind.
	if ev.ID != "" {
		return ledger.Event{}, fmt.Errorf("validate event before append: caller-supplied event id is not allowed")
	}
	if ev.Seq != 0 {
		return ledger.Event{}, fmt.Errorf("validate event before append: caller-supplied append sequence is not allowed")
	}
	if ev.SessionID != "" && ev.SessionID != s.Cfg.SessionID {
		return ledger.Event{}, fmt.Errorf("validate event before append: session %q is outside session %q", ev.SessionID, s.Cfg.SessionID)
	}
	if err := ledger.ValidateEvent(ev); err != nil {
		return ledger.Event{}, fmt.Errorf("validate event before append: %w", err)
	}
	preview := ev
	preview.ID = "E-PREAPPEND"
	if _, err := s.Coherence.Prepare(preview); err != nil {
		return ledger.Event{}, fmt.Errorf("validate coherence before append: %w", err)
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
	// Best-effort L3 indexing: never fail the durable append path.
	s.indexAfterEvent(ctx, stored)
	return stored, nil
}

// indexAfterEvent compiles warm pages for one durable event and applies them to
// the disposable index. Errors are recorded on IndexErr only.
func (s *Session) indexAfterEvent(ctx context.Context, ev ledger.Event) {
	if s.Index == nil {
		return
	}
	snap := s.Coherence.Snapshot()
	change := pages.LedgerChange{
		Event:     ev,
		State:     s.State.Clone(),
		Events:    s.Ledger,
		Artifacts: s.Artifacts,
		RepoID:    firstNonEmpty(snap.Current.Repo, ev.Repo),
		TaskID:    firstNonEmpty(snap.Current.TaskID, ev.TaskID),
		Branch:    firstNonEmpty(snap.Current.Branch, ev.Branch),
		Commit:    firstNonEmpty(snap.Current.Commit, ev.Commit),
	}
	muts, err := pages.NewDeterministicCompiler().Compile(ctx, change)
	if err != nil {
		s.IndexErr = err
		return
	}
	var maxVersion int64
	for _, mut := range muts {
		if int64(mut.Page.Version) > maxVersion {
			maxVersion = int64(mut.Page.Version)
		}
	}
	if maxVersion == 0 {
		maxVersion = ev.Seq
	}
	if err := s.Index.ApplyWithWatermark(ctx, muts, s.Cfg.SessionID, ev.Seq, maxVersion); err != nil {
		s.IndexErr = err
	}
}

// SemanticSearch runs a lexical warm-index query for the current session.
// Known-ID page faults must continue to use PageFault instead.
func (s *Session) SemanticSearch(ctx context.Context, text string) (retrieval.Result, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if err := s.operationErrorLocked(); err != nil {
		return retrieval.Result{}, err
	}
	if s.Index == nil {
		return retrieval.Result{}, retrieval.ErrIndexUnavailable
	}
	snap := s.Coherence.Snapshot()
	ret := &retrieval.LexicalRetriever{
		Index: s.Index,
		Recheck: func(_ context.Context, page pages.WarmPage) (bool, string) {
			if page.Status != pages.StatusActive {
				return false, "status"
			}
			// Reject pages outside the live session authority when they carry
			// a repo/branch that no longer matches current scope.
			if page.RepoID != "" && snap.Current.Repo != "" && page.RepoID != snap.Current.Repo {
				return false, "repo"
			}
			if page.Branch != "" && snap.Current.Branch != "" && page.Branch != snap.Current.Branch {
				return false, "branch"
			}
			return true, ""
		},
	}
	return ret.ResolveSemantic(ctx, retrieval.QueryIntent{
		Mode:      retrieval.ModeSemanticFault,
		SessionID: s.Cfg.SessionID,
		RepoID:    snap.Current.Repo,
		TaskID:    snap.Current.TaskID,
		Branch:    snap.Current.Branch,
		Commit:    snap.Current.Commit,
		UserText:  text,
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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
		Repo:   task.Repo,
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

// ChangeBranch appends a validated post-scope branch transition.
func (s *Session) ChangeBranch(ctx context.Context, change BranchChange) error {
	payload := map[string]any{"from_branch": change.FromBranch}
	if change.FromCommit != "" || change.ToCommit != "" {
		payload["from_commit"] = change.FromCommit
	}
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventBranchChange, Repo: change.Repo,
		Branch: change.ToBranch, Commit: change.ToCommit, Payload: payload,
	})
	return err
}

// ChangeCommit appends a validated post-scope commit transition.
func (s *Session) ChangeCommit(ctx context.Context, change CommitChange) error {
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventCommitChange, Repo: change.Repo, Branch: change.Branch,
		Commit: change.ToCommit, Payload: map[string]any{"from_commit": change.FromCommit},
	})
	return err
}

// RenameFile appends a durable rename coherence event.
func (s *Session) RenameFile(ctx context.Context, rename FileRename) error {
	payload := map[string]any{"from_path": rename.FromPath, "to_path": rename.ToPath}
	if rename.ContentDigest != "" {
		payload["content_digest"] = rename.ContentDigest
	}
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventFileRenamed, Repo: rename.Repo, Branch: rename.Branch, Payload: payload,
	})
	return err
}

// RecordFileSnapshot appends strict repository-relative file evidence.
func (s *Session) RecordFileSnapshot(ctx context.Context, snapshot FileSnapshot) error {
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventFileSnapshot, Repo: snapshot.Repo, Branch: snapshot.Branch, Commit: snapshot.Commit,
		Payload: map[string]any{"path": snapshot.Path, "content_digest": snapshot.ContentDigest},
	})
	return err
}

// InvalidateMemory terminally invalidates one existing session-local target.
func (s *Session) InvalidateMemory(ctx context.Context, invalidation MemoryInvalidation) error {
	_, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventMemoryInvalidated,
		Payload: map[string]any{
			"target_kind": string(invalidation.Kind),
			"target":      invalidation.Target,
			"reason":      string(invalidation.Reason),
		},
	})
	return err
}

// RevalidateMemory restores a stale target when its stale revision and evidence
// still match the durable current state.
func (s *Session) RevalidateMemory(ctx context.Context, revalidation MemoryRevalidation) error {
	payload := map[string]any{
		"target_kind":             string(revalidation.Kind),
		"target":                  revalidation.Target,
		"evidence_ref":            revalidation.EvidenceRef,
		"expected_stale_revision": revalidation.ExpectedStaleRevision,
	}
	if revalidation.SourceDigest != "" {
		payload["source_digest"] = revalidation.SourceDigest
	}
	_, err := s.Append(ctx, ledger.Event{Type: ledger.EventMemoryRevalidated, Payload: payload})
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
