// Package harness wires ledger, observers, WSL, scheduler, and faults.
package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wsms/internal/artifacts"
	"wsms/internal/coherence"
	"wsms/internal/config"
	"wsms/internal/embedder"
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
	appendMu          sync.Mutex
	closeOnce         sync.Once
	closeErr          error
	closed            bool
	failStopErr       error
	lastEvent         ledger.Event
	embeddingMu       sync.Mutex
	embeddingCategory string
	embeddingErr      error
	embeddingCancel   context.CancelFunc
	embeddingDone     chan struct{}
	embeddingWake     chan struct{}
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
	// Embedder is the optional Phase 7D dense projection source. It is never
	// authoritative and never gates L4 appends or direct page faults.
	Embedder embedder.Embedder
}

// EmbeddingStatus is the synchronized, redacted status for the optional dense
// projection. Category is one of indexer's EmbeddingStatus* constants.
type EmbeddingStatus struct {
	Degraded bool
	Category string
}

// OpenSession creates a session under cfg.DataDir.
func OpenSession(cfg config.Config) (*Session, error) {
	if cfg.DataDir == "" {
		cfg = config.Default()
	}
	if cfg.SessionID == "" {
		cfg.SessionID = "session-default"
	}
	if err := configureEmbedder(&cfg); err != nil {
		return nil, err
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
		Embedder:   cfg.Embedder,
	}
	// Warm index is disposable and optional. Open failures leave Index nil.
	// Dense projection is off by default (cfg.DenseDimensions == 0).
	embeddingNamespace := ""
	if cfg.Embedder != nil {
		embeddingNamespace = cfg.Embedder.Namespace().ID
	}
	if warm, err := indexer.Open(filepath.Join(cfg.DataDir, "index"), indexer.Options{
		DenseDimensions:    cfg.DenseDimensions,
		EmbeddingNamespace: embeddingNamespace,
	}); err == nil {
		s.Index = warm
	} else {
		s.IndexErr = err
	}
	if err := s.replay(context.Background()); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	s.startEmbeddingWorker()
	s.wakeEmbeddingWorker()
	return s, nil
}

func configureEmbedder(cfg *config.Config) error {
	if cfg.Embedder == nil {
		return nil
	}
	ns := cfg.Embedder.Namespace()
	if err := ns.Validate(); err != nil {
		return fmt.Errorf("embedding namespace: %w", err)
	}
	if cfg.DenseDimensions == 0 {
		cfg.DenseDimensions = ns.Profile.Dimensions
	} else if cfg.DenseDimensions != ns.Profile.Dimensions {
		return fmt.Errorf(
			"%w: dense dimensions %d do not match namespace dimensions %d",
			embedder.ErrInvalidNamespace,
			cfg.DenseDimensions,
			ns.Profile.Dimensions,
		)
	}
	if cfg.EmbeddingBatchSize < 0 {
		return fmt.Errorf("embedding batch size must be non-negative")
	}
	return nil
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
		s.lastEvent = ev
		s.indexAfterEvent(ctx, ev)
	}
	return nil
}

// Close releases resources.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.appendMu.Lock()
		s.closed = true
		cancel := s.embeddingCancel
		done := s.embeddingDone
		s.appendMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
		s.appendMu.Lock()
		defer s.appendMu.Unlock()
		var indexErr error
		if s.Index != nil {
			indexErr = s.Index.Close()
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
	s.lastEvent = stored
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
	watermark, _, err := s.Index.Watermark(ctx, s.Cfg.SessionID)
	if err != nil {
		s.IndexErr = err
		return
	}
	if ev.Seq <= watermark {
		return
	}
	if ev.Seq != watermark+1 {
		s.IndexErr = s.repairIndexFromLedger(ctx)
		return
	}
	if err := s.compileAndApplyIndexEvent(ctx, ev, s.State, s.Coherence); err != nil {
		if repairErr := s.repairIndexFromLedger(ctx); repairErr != nil {
			s.IndexErr = errors.Join(err, repairErr)
			return
		}
	}
	s.IndexErr = nil
}

func (s *Session) compileAndApplyIndexEvent(ctx context.Context, ev ledger.Event, state *wsl.WorkingState, coherent *coherence.State) error {
	snap := coherent.Snapshot()
	change := pages.LedgerChange{
		Event:     ev,
		State:     state.Clone(),
		Events:    s.Ledger,
		Artifacts: s.Artifacts,
		Coherence: coherent,
		RepoID:    firstNonEmpty(snap.Current.Repo, ev.Repo),
		TaskID:    firstNonEmpty(snap.Current.TaskID, ev.TaskID),
		Branch:    firstNonEmpty(snap.Current.Branch, ev.Branch),
		Commit:    firstNonEmpty(snap.Current.Commit, ev.Commit),
	}
	muts, err := pages.NewDeterministicCompiler().Compile(ctx, change)
	if err != nil {
		return err
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
		return err
	}
	s.wakeEmbeddingWorker()
	return nil
}

const embeddingWorkerMaxBackoff = 2 * time.Second

func (s *Session) startEmbeddingWorker() {
	if s.Embedder == nil || s.Index == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.embeddingCancel = cancel
	s.embeddingDone = make(chan struct{})
	s.embeddingWake = make(chan struct{}, 1)
	go s.embeddingWorker(ctx)
}

func (s *Session) wakeEmbeddingWorker() {
	if s.embeddingWake == nil {
		return
	}
	select {
	case s.embeddingWake <- struct{}{}:
	default:
	}
}

func (s *Session) embeddingWorker(ctx context.Context) {
	defer close(s.embeddingDone)
	backoff := 25 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.embeddingWake:
		}
		for {
			processed, category, err := s.embedMissingVectorBatch(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				s.recordEmbeddingError(category, err)
				if !embeddingWorkerShouldRetry(category, err) {
					break
				}
				timer := time.NewTimer(backoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				backoff *= 2
				if backoff > embeddingWorkerMaxBackoff {
					backoff = embeddingWorkerMaxBackoff
				}
				continue
			}
			backoff = 25 * time.Millisecond
			if processed == 0 {
				s.recordEmbeddingOK()
				break
			}
		}
	}
}

func (s *Session) embedMissingVectorBatch(ctx context.Context) (int, string, error) {
	if s.Embedder == nil || s.Index == nil {
		return 0, indexer.EmbeddingStatusOK, nil
	}
	ns := s.Embedder.Namespace()
	namespace := ns.ID
	if err := ns.Validate(); err != nil {
		return 0, indexer.EmbeddingStatusNamespace, err
	}
	if !s.Index.DenseEnabled() {
		return 0, indexer.EmbeddingStatusDenseUnavailable, indexer.ErrDenseUnavailable
	}
	if dims := s.Index.DenseDimensions(); dims != ns.Profile.Dimensions {
		return 0, indexer.EmbeddingStatusNamespace, fmt.Errorf("%w: index dimensions %d do not match namespace dimensions %d",
			embedder.ErrInvalidNamespace, dims, ns.Profile.Dimensions)
	}
	if checker, ok := s.Embedder.(embedder.SelfChecker); ok {
		if err := checker.SelfCheck(ctx); err != nil {
			return 0, embeddingFailureCategory(indexer.EmbeddingStatusSelfCheck, err), err
		}
	}
	batchSize := s.Cfg.EmbeddingBatchSize
	if batchSize <= 0 {
		batchSize = embedder.DefaultMaxBatchSize
	}
	pagesToEmbed, err := s.Index.MissingVectorPages(ctx, s.Cfg.SessionID, namespace, batchSize)
	if err != nil {
		category := indexer.EmbeddingStatusIndexer
		if errors.Is(err, indexer.ErrDenseUnavailable) {
			category = indexer.EmbeddingStatusDenseUnavailable
		}
		return 0, category, err
	}
	if len(pagesToEmbed) == 0 {
		return 0, indexer.EmbeddingStatusOK, nil
	}
	texts := make([]string, len(pagesToEmbed))
	for i, page := range pagesToEmbed {
		texts[i] = embeddingTextForPage(page)
	}
	batch, err := s.Embedder.EmbedDocuments(ctx, texts)
	if err != nil {
		if errors.Is(err, embedder.ErrAdmissionDenied) {
			return s.embedVectorPagesIndividually(ctx, ns, namespace, pagesToEmbed, texts)
		}
		return 0, embeddingFailureCategory(indexer.EmbeddingStatusEmbedder, err), err
	}
	if err := batch.Validate(ns, len(texts)); err != nil {
		return 0, indexer.EmbeddingStatusVector, err
	}
	records := make([]indexer.VectorRecord, len(pagesToEmbed))
	for i, page := range pagesToEmbed {
		records[i] = vectorRecordForPage(page, namespace, embeddingVector64(batch.Embeddings[i].Vector))
	}
	if err := s.Index.UpsertVectors(ctx, records); err != nil {
		return 0, indexer.EmbeddingStatusIndexer, err
	}
	return len(pagesToEmbed), indexer.EmbeddingStatusOK, nil
}

func (s *Session) embedVectorPagesIndividually(ctx context.Context, ns embedder.EmbeddingNamespace, namespace string, pagesToEmbed []pages.WarmPage, texts []string) (int, string, error) {
	processed := 0
	for i, page := range pagesToEmbed {
		if err := ctx.Err(); err != nil {
			return processed, indexer.EmbeddingStatusEmbedder, err
		}
		batch, err := s.Embedder.EmbedDocuments(ctx, []string{texts[i]})
		if err != nil {
			if errors.Is(err, embedder.ErrAdmissionDenied) {
				suppression := vectorSuppressionForPage(page, namespace, indexer.EmbeddingStatusAdmissionDenied)
				if err := s.Index.SuppressVectors(ctx, []indexer.VectorSuppression{suppression}); err != nil {
					return processed, indexer.EmbeddingStatusIndexer, err
				}
				processed++
				continue
			}
			return processed, embeddingFailureCategory(indexer.EmbeddingStatusEmbedder, err), err
		}
		if err := batch.Validate(ns, 1); err != nil {
			return processed, indexer.EmbeddingStatusVector, err
		}
		record := vectorRecordForPage(page, namespace, embeddingVector64(batch.Embeddings[0].Vector))
		if err := s.Index.UpsertVectors(ctx, []indexer.VectorRecord{record}); err != nil {
			return processed, indexer.EmbeddingStatusIndexer, err
		}
		processed++
	}
	return processed, indexer.EmbeddingStatusOK, nil
}

func embeddingFailureCategory(defaultCategory string, err error) string {
	switch {
	case errors.Is(err, embedder.ErrInvalidNamespace), errors.Is(err, indexer.ErrEmbeddingNamespaceMismatch):
		return indexer.EmbeddingStatusNamespace
	case errors.Is(err, embedder.ErrInvalidEmbedding):
		return indexer.EmbeddingStatusVector
	case errors.Is(err, embedder.ErrAdmissionDenied):
		return indexer.EmbeddingStatusAdmissionDenied
	case errors.Is(err, indexer.ErrDenseUnavailable):
		return indexer.EmbeddingStatusDenseUnavailable
	default:
		return defaultCategory
	}
}

func embeddingWorkerShouldRetry(category string, err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, indexer.ErrStalePageTuple) {
		return false
	}
	switch embeddingFailureCategory(category, err) {
	case indexer.EmbeddingStatusSelfCheck, indexer.EmbeddingStatusEmbedder:
		return true
	default:
		return false
	}
}

func (s *Session) recordEmbeddingError(category string, err error) {
	if err == nil {
		return
	}
	if !indexer.ValidEmbeddingStatus(category) || category == indexer.EmbeddingStatusOK {
		category = indexer.EmbeddingStatusInternal
	}
	s.embeddingMu.Lock()
	s.embeddingErr = err
	s.embeddingCategory = category
	s.embeddingMu.Unlock()
	if s.Index != nil {
		s.Index.RecordEmbeddingStatus(category)
	}
}

func (s *Session) recordEmbeddingOK() {
	s.embeddingMu.Lock()
	s.embeddingErr = nil
	s.embeddingCategory = indexer.EmbeddingStatusOK
	s.embeddingMu.Unlock()
	if s.Index != nil {
		s.Index.RecordEmbeddingStatus(indexer.EmbeddingStatusOK)
	}
}

// EmbeddingStatus returns the synchronized non-authoritative dense worker
// status without exposing raw error text.
func (s *Session) EmbeddingStatus() EmbeddingStatus {
	s.embeddingMu.Lock()
	defer s.embeddingMu.Unlock()
	return EmbeddingStatus{
		Degraded: s.embeddingErr != nil,
		Category: s.embeddingCategory,
	}
}

func embeddingVector64(vector []float32) []float64 {
	if vector == nil {
		return nil
	}
	out := make([]float64, len(vector))
	for i, v := range vector {
		out[i] = float64(v)
	}
	return out
}

func vectorRecordForPage(page pages.WarmPage, namespace string, vector []float64) indexer.VectorRecord {
	return indexer.VectorRecord{
		PageID:             page.ID,
		SessionID:          page.SessionID,
		PageVersion:        page.Version,
		SourceDigest:       page.SourceDigest,
		CompilerVersion:    page.CompilerVersion,
		EmbeddingNamespace: namespace,
		Vector:             vector,
	}
}

func vectorSuppressionForPage(page pages.WarmPage, namespace, reason string) indexer.VectorSuppression {
	return indexer.VectorSuppression{
		PageID:             page.ID,
		SessionID:          page.SessionID,
		PageVersion:        page.Version,
		SourceDigest:       page.SourceDigest,
		CompilerVersion:    page.CompilerVersion,
		EmbeddingNamespace: namespace,
		Reason:             reason,
	}
}

func embeddingTextForPage(page pages.WarmPage) string {
	if page.Kind != pages.KindFailureEpisode {
		return page.SearchText
	}
	text := page.SearchText
	start := strings.Index(text, " error=")
	if start < 0 {
		return text
	}
	suffix := ""
	rest := text[start+len(" error="):]
	if next := strings.Index(rest, " file="); next >= 0 {
		suffix = rest[next:]
	}
	return strings.TrimSpace(text[:start] + " error=[redacted]" + suffix)
}

func (s *Session) repairIndexFromLedger(ctx context.Context) error {
	if s.Index == nil {
		return retrieval.ErrIndexUnavailable
	}
	watermark, _, err := s.Index.Watermark(ctx, s.Cfg.SessionID)
	if err != nil {
		return err
	}
	events, err := s.Ledger.ListBySession(ctx, s.Cfg.SessionID)
	if err != nil {
		return err
	}
	state := wsl.NewWorkingState()
	coherent := coherence.NewState()
	dispatcher := observers.Default(observers.NewSeqIDGen(), state)
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		candidate, err := coherent.Prepare(ev)
		if err != nil {
			return err
		}
		updates, err := dispatcher.OnEvent(ctx, ev)
		if err != nil {
			return err
		}
		for i := range updates {
			updates[i].EvidenceID = ev.ID
		}
		if err := candidate.BindUpdates(updates); err != nil {
			return err
		}
		if err := state.ApplyEventUpdates(ev.ID, updates); err != nil {
			return err
		}
		if err := coherent.Commit(candidate); err != nil {
			return err
		}
		if ev.Seq <= watermark {
			continue
		}
		if err := s.compileAndApplyIndexEvent(ctx, ev, state, coherent); err != nil {
			return err
		}
	}
	return nil
}

// SemanticSearch runs a lexical warm-index query for the current session.
// Known-ID page faults must continue to use PageFault instead.
func (s *Session) SemanticSearch(ctx context.Context, text string) (retrieval.Result, error) {
	s.appendMu.Lock()
	if err := s.operationErrorLocked(); err != nil {
		s.appendMu.Unlock()
		return retrieval.Result{}, err
	}
	if s.Index == nil {
		s.appendMu.Unlock()
		return retrieval.Result{}, retrieval.ErrIndexUnavailable
	}
	if s.lastEvent.ID == "" {
		s.appendMu.Unlock()
		return retrieval.Result{}, retrieval.ErrSemanticPageMiss
	}
	probeSnap := s.Coherence.Snapshot()
	s.appendMu.Unlock()

	degraded := s.denseSemanticProbe(ctx, text, probeSnap)

	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if err := s.operationErrorLocked(); err != nil {
		return retrieval.Result{}, err
	}
	if s.Index == nil {
		return retrieval.Result{}, retrieval.ErrIndexUnavailable
	}
	if s.lastEvent.ID == "" {
		return retrieval.Result{}, retrieval.ErrSemanticPageMiss
	}
	snap := s.Coherence.Snapshot()
	change := pages.LedgerChange{
		Event: s.lastEvent, State: s.State.Clone(), Events: s.Ledger, Artifacts: s.Artifacts,
		Coherence: s.Coherence, RepoID: snap.Current.Repo, TaskID: snap.Current.TaskID,
		Branch: snap.Current.Branch, Commit: snap.Current.Commit,
	}
	materialized := map[pages.PageID][]string{}
	ret := &retrieval.LexicalRetriever{
		Index: s.Index,
		Recheck: func(checkCtx context.Context, page pages.WarmPage) (bool, string) {
			if err := pages.ValidateMaterializable(checkCtx, page, change); err != nil {
				return false, "authority"
			}
			evidence, err := s.materializeSemanticPage(checkCtx, page)
			if err != nil {
				return false, "fault"
			}
			materialized[page.ID] = evidence
			return true, ""
		},
	}
	result, err := ret.ResolveSemantic(ctx, retrieval.QueryIntent{
		Mode:      retrieval.ModeSemanticFault,
		SessionID: s.Cfg.SessionID,
		RepoID:    snap.Current.Repo,
		TaskID:    snap.Current.TaskID,
		Branch:    snap.Current.Branch,
		Commit:    snap.Current.Commit,
		UserText:  text,
	})
	if err != nil {
		return retrieval.Result{}, err
	}
	result.Degraded = append(result.Degraded, degraded...)
	for _, candidate := range result.Candidates {
		result.Materialized = append(result.Materialized, retrieval.MaterializedPage{
			PageID: candidate.Page.ID, Evidence: append([]string(nil), materialized[candidate.Page.ID]...),
		})
	}
	return result, nil
}

func (s *Session) denseSemanticProbe(ctx context.Context, text string, snap coherence.Snapshot) []string {
	if s.Embedder == nil || s.Index == nil || !s.Index.DenseEnabled() {
		return nil
	}
	ns := s.Embedder.Namespace()
	namespace := ns.ID
	if err := ns.Validate(); err != nil {
		s.recordEmbeddingError(indexer.EmbeddingStatusNamespace, err)
		return []string{"dense=namespace"}
	}
	if dims := s.Index.DenseDimensions(); dims != ns.Profile.Dimensions {
		err := fmt.Errorf("%w: index dimensions %d do not match namespace dimensions %d",
			embedder.ErrInvalidNamespace, dims, ns.Profile.Dimensions)
		s.recordEmbeddingError(indexer.EmbeddingStatusNamespace, err)
		return []string{"dense=namespace"}
	}
	query, err := s.Embedder.EmbedQuery(ctx, text)
	if err != nil {
		category := embeddingFailureCategory(indexer.EmbeddingStatusEmbedder, err)
		s.recordEmbeddingError(category, err)
		return []string{denseProbeDegradationMarker(category, "dense=embedder")}
	}
	if err := query.Validate(ns, embedder.RoleQuery); err != nil {
		s.recordEmbeddingError(indexer.EmbeddingStatusVector, err)
		return []string{"dense=query-vector"}
	}
	// Phase 7D keeps semantic resolution FTS-first. The dense probe verifies the
	// query/document ABI, namespace, and vec0 availability without letting a
	// vector score become authority; Phase 7E owns fusion/reranking.
	if _, err := s.Index.SearchDense(ctx, indexer.SearchQuery{
		SessionID:          s.Cfg.SessionID,
		RepoID:             snap.Current.Repo,
		TaskID:             snap.Current.TaskID,
		Branch:             snap.Current.Branch,
		Commit:             snap.Current.Commit,
		EmbeddingNamespace: namespace,
		Limit:              3,
		ActiveOnly:         true,
	}, embeddingVector64(query.Vector)); err != nil {
		category := embeddingFailureCategory(indexer.EmbeddingStatusSearch, err)
		s.recordEmbeddingError(category, err)
		return []string{denseProbeDegradationMarker(category, "dense=search")}
	}
	return nil
}

func denseProbeDegradationMarker(category, fallback string) string {
	switch category {
	case indexer.EmbeddingStatusNamespace:
		return "dense=namespace"
	case indexer.EmbeddingStatusVector:
		return "dense=query-vector"
	case indexer.EmbeddingStatusAdmissionDenied:
		return "dense=admission-denied"
	case indexer.EmbeddingStatusDenseUnavailable:
		return "dense=unavailable"
	default:
		return fallback
	}
}

func (s *Session) materializeSemanticPage(ctx context.Context, page pages.WarmPage) ([]string, error) {
	const maxRenderedRefs = 4
	refs := make([]pages.PageRef, 0, maxRenderedRefs)
	for _, ref := range page.Refs {
		if ref.Kind == pages.RefWSLRecord {
			refs = append(refs, ref)
			if len(refs) == maxRenderedRefs {
				break
			}
		}
	}
	if len(refs) == 0 {
		for _, ref := range page.Refs {
			if ref.Kind == pages.RefEvent {
				refs = append(refs, ref)
				break
			}
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("page %s has no renderable exact ref", page.ID)
	}
	budget := s.Cfg.PageFaultTokenBudget
	if budget <= 0 {
		budget = 256
	}
	perRef := budget / len(refs)
	if perRef < 32 {
		perRef = 32
	}
	evidence := make([]string, 0, len(refs))
	for _, ref := range refs {
		var (
			body string
			err  error
		)
		switch ref.Kind {
		case pages.RefWSLRecord:
			body, err = s.Tools.ReadPage(ctx, ref.ID, perRef)
		case pages.RefEvent:
			body, err = s.Tools.ReadEvent(ctx, ref.ID, perRef)
		}
		if err != nil {
			return nil, err
		}
		if body == "" || body == faults.PageMiss {
			return nil, fmt.Errorf("page %s ref %s did not materialize", page.ID, ref.Address())
		}
		evidence = append(evidence, body)
	}
	return evidence, nil
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
