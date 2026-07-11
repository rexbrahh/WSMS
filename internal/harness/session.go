// Package harness wires ledger, observers, WSL, scheduler, and faults.
package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	"wsms/internal/renderer"
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
	ErrSessionClosed          = errors.New("session closed")
	errSemanticAttemptChanged = errors.New("semantic scope changed during index freshness check")
)

const (
	semanticCoherenceAttempts           = 2
	semanticCandidateLimit              = 20
	semanticMaterializeLimit            = 3
	semanticMaterializationAttemptLimit = 20
	semanticMaterializationRefLimit     = 12
	semanticMaterializationByteLimit    = 64 * 1024
	semanticDefaultTokenBudget          = 256
	semanticMaxTokenBudget              = 64 * 1024
	semanticMaxScopeEpochs              = 64
)

type semanticMaterializationBudget struct {
	attemptsRemaining int
	pagesRemaining    int
	refsRemaining     int
	tokensRemaining   int
	bytesRemaining    int
}

func newSemanticMaterializationBudget(configuredTokens int) *semanticMaterializationBudget {
	tokens := configuredTokens
	if tokens <= 0 {
		tokens = semanticDefaultTokenBudget
	}
	if tokens > semanticMaxTokenBudget {
		tokens = semanticMaxTokenBudget
	}
	return &semanticMaterializationBudget{
		attemptsRemaining: semanticMaterializationAttemptLimit,
		pagesRemaining:    semanticMaterializeLimit,
		refsRemaining:     semanticMaterializationRefLimit,
		tokensRemaining:   tokens,
		bytesRemaining:    semanticMaterializationByteLimit,
	}
}

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
	semanticContext   context.Context
	semanticCancel    context.CancelFunc
	semanticWG        sync.WaitGroup
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
	semanticContext, semanticCancel := context.WithCancel(context.Background())
	s := &Session{
		Cfg:             cfg,
		Ledger:          led,
		Artifacts:       arts,
		State:           st,
		Coherence:       coherent,
		Hierarchy:       h,
		Dispatcher:      disp,
		Scheduler:       sched,
		Tools:           faults.NewTools(res, sched.PageFault),
		Embedder:        cfg.Embedder,
		semanticContext: semanticContext,
		semanticCancel:  semanticCancel,
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
		// Semantic work can be outside appendMu while embedding and searching.
		// Cancel it before waiting for the lock so a materializer holding appendMu
		// receives cancellation and cannot deadlock Close.
		if s.semanticCancel != nil {
			s.semanticCancel()
		}
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
		s.semanticWG.Wait()
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

func (s *Session) beginSemanticOperation(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("nil context")
	}
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if err := s.operationErrorLocked(); err != nil {
		return nil, nil, err
	}
	if s.semanticContext == nil || s.semanticContext.Err() != nil {
		return nil, nil, ErrSessionClosed
	}
	s.semanticWG.Add(1)
	semanticCtx, cancel := context.WithCancel(ctx)
	stopLifecycleCancel := context.AfterFunc(s.semanticContext, cancel)
	done := func() {
		stopLifecycleCancel()
		cancel()
		s.semanticWG.Done()
	}
	return semanticCtx, done, nil
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

// SemanticSearch resolves an unknown semantic address through the disposable
// warm index, then admits only exact evidence revalidated against current L4.
// Known-ID page faults must continue to use PageFault instead.
func (s *Session) SemanticSearch(ctx context.Context, text string) (retrieval.Result, error) {
	return s.semanticSearch(ctx, text, nil)
}

// beforeResolve is an internal deterministic concurrency-test seam. Production
// callers always enter through SemanticSearch with no hook.
func (s *Session) semanticSearch(ctx context.Context, text string, beforeResolve func(int)) (retrieval.Result, error) {
	semanticCtx, finishSemantic, err := s.beginSemanticOperation(ctx)
	if err != nil {
		return retrieval.Result{}, err
	}
	defer finishSemantic()
	ctx = semanticCtx

	s.appendMu.Lock()
	if err := s.operationErrorLocked(); err != nil {
		s.appendMu.Unlock()
		return retrieval.Result{}, err
	}
	index := s.Index
	emb := s.Embedder
	if index == nil {
		s.appendMu.Unlock()
		return retrieval.Result{}, retrieval.ErrIndexUnavailable
	}
	if s.lastEvent.ID == "" {
		s.appendMu.Unlock()
		return retrieval.Result{}, retrieval.ErrSemanticPageMiss
	}
	s.appendMu.Unlock()

	// Provider work is deliberately outside appendMu. Session.Close owns the
	// cancellation and joins this operation before closing the index or L4.
	dense, err := s.semanticDenseQuery(ctx, index, emb, text)
	if err != nil {
		return retrieval.Result{}, err
	}
	budget := newSemanticMaterializationBudget(s.Cfg.PageFaultTokenBudget)

	for attempt := 0; attempt < semanticCoherenceAttempts; attempt++ {
		index, intent, attemptState, err := s.captureSemanticAttempt(ctx, text, budget.tokensRemaining)
		if errors.Is(err, errSemanticAttemptChanged) {
			if attempt+1 < semanticCoherenceAttempts {
				continue
			}
			return retrieval.Result{}, semanticScopeMiss(retrieval.RetrievalTrace{})
		}
		if err != nil {
			return retrieval.Result{}, err
		}
		revision := attemptState.revision
		materialized := make(map[pages.PageID][]string)
		scopeChanged := false
		ret := &retrieval.HybridRetriever{
			Index: index,
			Dense: dense,
			DetailedRecheck: func(checkCtx context.Context, page pages.WarmPage, remainingTokens int) (retrieval.RecheckResult, error) {
				s.appendMu.Lock()
				defer s.appendMu.Unlock()
				if err := checkCtx.Err(); err != nil {
					return retrieval.RecheckResult{}, err
				}
				if err := s.operationErrorLocked(); err != nil {
					return retrieval.RecheckResult{}, err
				}
				snap := s.Coherence.Snapshot()
				if snap.Revision != revision {
					scopeChanged = true
					return retrieval.RecheckResult{Reason: "scope"}, nil
				}
				change := pages.LedgerChange{
					Event: s.lastEvent, State: s.State.Clone(), Events: s.Ledger, Artifacts: s.Artifacts,
					Coherence: s.Coherence, RepoID: snap.Current.Repo, TaskID: snap.Current.TaskID,
					Branch: snap.Current.Branch, Commit: snap.Current.Commit,
				}
				if err := pages.ValidateMaterializable(checkCtx, page, change); err != nil {
					if checkCtx.Err() != nil {
						return retrieval.RecheckResult{}, checkCtx.Err()
					}
					if errors.Is(err, pages.ErrUnmaterializableRef) || errors.Is(err, pages.ErrInvalidPage) {
						return retrieval.RecheckResult{Reason: "authority"}, nil
					}
					return retrieval.RecheckResult{}, err
				}
				evidence, tokensUsed, reason, err := s.materializeSemanticPage(checkCtx, page, budget, remainingTokens)
				if err != nil {
					return retrieval.RecheckResult{}, err
				}
				if reason != "" {
					return retrieval.RecheckResult{Reason: reason}, nil
				}
				materialized[page.ID] = evidence
				return retrieval.RecheckResult{Eligible: true, TokensUsed: tokensUsed}, nil
			},
		}

		if beforeResolve != nil {
			beforeResolve(attempt)
		}
		result, resolveErr := ret.ResolveSemantic(ctx, intent)
		if err := ctx.Err(); err != nil {
			return retrieval.Result{}, err
		}
		changed, checkErr := s.validateSemanticAttemptFreshness(ctx, attemptState)
		if checkErr != nil {
			return retrieval.Result{}, checkErr
		}
		scopeChanged = scopeChanged || changed
		if scopeChanged {
			if attempt+1 < semanticCoherenceAttempts {
				continue
			}
			if resolveErr != nil && !errors.Is(resolveErr, retrieval.ErrSemanticPageMiss) {
				return result, resolveErr
			}
			trace := semanticTraceFrom(resolveErr, result.Trace)
			return retrieval.Result{}, semanticScopeMiss(trace)
		}
		if resolveErr != nil {
			if errors.Is(resolveErr, retrieval.ErrSemanticPageMiss) {
				return retrieval.Result{}, resolveErr
			}
			return result, resolveErr
		}

		for _, candidate := range result.Candidates {
			evidence, ok := materialized[candidate.Page.ID]
			if !ok {
				return retrieval.Result{}, fmt.Errorf("%w: selected page lacks exact evidence", retrieval.ErrIndexUnavailable)
			}
			result.Materialized = append(result.Materialized, retrieval.MaterializedPage{
				PageID: candidate.Page.ID, Evidence: append([]string(nil), evidence...),
			})
		}
		return result, nil
	}
	return retrieval.Result{}, retrieval.ErrSemanticPageMiss
}

type semanticProjectionSnapshot struct {
	generation  int64
	sourceSeq   int64
	pageVersion int64
}

type semanticAttemptSnapshot struct {
	index      *indexer.Index
	revision   uint64
	sourceSeq  int64
	projection semanticProjectionSnapshot
}

func (s *Session) captureSemanticAttempt(ctx context.Context, text string, tokenBudget int) (*indexer.Index, retrieval.QueryIntent, semanticAttemptSnapshot, error) {
	s.appendMu.Lock()
	if err := s.operationErrorLocked(); err != nil {
		s.appendMu.Unlock()
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, err
	}
	if s.Index == nil {
		s.appendMu.Unlock()
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, retrieval.ErrIndexUnavailable
	}
	if s.lastEvent.ID == "" {
		s.appendMu.Unlock()
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, retrieval.ErrSemanticPageMiss
	}
	index := s.Index
	indexErr := s.IndexErr
	lastSourceSeq := s.lastEvent.Seq
	snap := s.Coherence.Snapshot()
	scopeEpochs, err := semanticScopeEpochs(snap)
	if err != nil {
		s.appendMu.Unlock()
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, err
	}
	intent := retrieval.QueryIntent{
		Mode:             retrieval.ModeSemanticFault,
		SessionID:        s.Cfg.SessionID,
		RepoID:           snap.Current.Repo,
		TaskID:           snap.Current.TaskID,
		Branch:           snap.Current.Branch,
		Commit:           snap.Current.Commit,
		ScopeEpochs:      scopeEpochs,
		UserText:         text,
		CandidateLimit:   semanticCandidateLimit,
		MaterializeLimit: semanticMaterializeLimit,
		TokenBudget:      tokenBudget,
	}
	s.appendMu.Unlock()

	if indexErr != nil {
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, semanticIndexUnavailable("documented source projection failure")
	}
	beforeProjection, err := readSemanticProjection(ctx, index, intent.SessionID)
	if err != nil {
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, err
	}
	authority, err := index.ActivePageSnapshot(ctx, intent.SessionID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, ctx.Err()
		}
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, semanticIndexUnavailable("eligible page snapshot unavailable")
	}
	eligible := make([]indexer.PageTuple, 0, len(authority.Pages))
	for _, descriptor := range authority.Pages {
		if descriptor.Tuple.CompilerVersion != pages.CurrentCompilerVersion {
			continue
		}
		if !s.Coherence.PageDescriptorEligible(
			string(descriptor.Tuple.PageID), descriptor.RefIDs, descriptor.Scope,
			descriptor.Branch, descriptor.Commit, descriptor.PathScope,
			string(descriptor.Tuple.SourceDigest), uint64(descriptor.Tuple.ScopeEpoch),
		) {
			continue
		}
		eligible = append(eligible, descriptor.Tuple)
	}
	afterProjection, err := readSemanticProjection(ctx, index, intent.SessionID)
	if err != nil {
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, err
	}
	s.appendMu.Lock()
	if err := s.operationErrorLocked(); err != nil {
		s.appendMu.Unlock()
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, err
	}
	currentIndex := s.Index
	currentRevision := s.Coherence.Snapshot().Revision
	currentSourceSeq := s.lastEvent.Seq
	currentIndexErr := s.IndexErr
	s.appendMu.Unlock()
	if currentRevision != snap.Revision || currentSourceSeq != lastSourceSeq {
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, errSemanticAttemptChanged
	}
	if currentIndex != index {
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, semanticIndexUnavailable("source projection changed")
	}
	if currentIndexErr != nil {
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, semanticIndexUnavailable("documented source projection failure")
	}
	authorityProjection := semanticProjectionSnapshot{
		generation:  authority.ServingGeneration,
		sourceSeq:   authority.Watermark.LastSourceSeq,
		pageVersion: int64(authority.Watermark.LastPageVersion),
	}
	if beforeProjection != authorityProjection || authorityProjection != afterProjection {
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, semanticIndexUnavailable("source projection changed during eligibility snapshot")
	}
	if afterProjection.sourceSeq != lastSourceSeq {
		return nil, retrieval.QueryIntent{}, semanticAttemptSnapshot{}, semanticIndexUnavailable("source watermark is not current")
	}
	intent.EligibilityComplete = true
	intent.EligiblePageTuples = eligible
	return index, intent, semanticAttemptSnapshot{
		index: index, revision: snap.Revision, sourceSeq: lastSourceSeq, projection: afterProjection,
	}, nil
}

func readSemanticProjection(ctx context.Context, index *indexer.Index, sessionID string) (semanticProjectionSnapshot, error) {
	health, err := index.Health(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return semanticProjectionSnapshot{}, ctx.Err()
		}
		return semanticProjectionSnapshot{}, semanticIndexUnavailable("source generation unavailable")
	}
	if !health.Ready || health.Generation <= 0 {
		return semanticProjectionSnapshot{}, semanticIndexUnavailable("source generation unavailable")
	}
	sourceSeq, pageVersion, err := index.Watermark(ctx, sessionID)
	if err != nil {
		if ctx.Err() != nil {
			return semanticProjectionSnapshot{}, ctx.Err()
		}
		return semanticProjectionSnapshot{}, semanticIndexUnavailable("source watermark unavailable")
	}
	return semanticProjectionSnapshot{
		generation: health.Generation, sourceSeq: sourceSeq, pageVersion: pageVersion,
	}, nil
}

func (s *Session) validateSemanticAttemptFreshness(ctx context.Context, attempt semanticAttemptSnapshot) (bool, error) {
	changed, err := s.semanticAuthorityChanged(attempt)
	if err != nil || changed {
		return changed, err
	}
	projection, err := readSemanticProjection(ctx, attempt.index, s.Cfg.SessionID)
	if err != nil {
		return false, err
	}
	changed, err = s.semanticAuthorityChanged(attempt)
	if err != nil || changed {
		return changed, err
	}
	if projection != attempt.projection || projection.sourceSeq != attempt.sourceSeq {
		return false, semanticIndexUnavailable("source projection changed during semantic resolution")
	}
	return false, nil
}

func (s *Session) semanticAuthorityChanged(attempt semanticAttemptSnapshot) (bool, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if err := s.operationErrorLocked(); err != nil {
		return false, err
	}
	if s.Index != attempt.index {
		return false, semanticIndexUnavailable("source projection changed")
	}
	if s.IndexErr != nil {
		return false, semanticIndexUnavailable("documented source projection failure")
	}
	return s.Coherence.Snapshot().Revision != attempt.revision || s.lastEvent.Seq != attempt.sourceSeq, nil
}

func semanticIndexUnavailable(category string) error {
	return fmt.Errorf("%w: %s", retrieval.ErrIndexUnavailable, category)
}

func semanticScopeEpochs(snap coherence.Snapshot) ([]pages.ScopeEpoch, error) {
	epochs := map[pages.ScopeEpoch]struct{}{0: {}}
	repo, branch, commit := snap.Current.Repo, snap.Current.Branch, snap.Current.Commit
	branchEpoch := snap.BranchEpochs[repo+"\x00"+branch]
	baseEpoch := branchEpoch
	if commitEpoch := snap.CommitEpochs[repo+"\x00"+branch+"\x00"+commit]; commitEpoch > baseEpoch {
		baseEpoch = commitEpoch
	}
	epochs[pages.ScopeEpoch(branchEpoch)] = struct{}{}
	epochs[pages.ScopeEpoch(baseEpoch)] = struct{}{}
	pathPrefix := repo + "\x00" + branch + "\x00"
	for key, pathEpoch := range snap.PathEpochs {
		if !strings.HasPrefix(key, pathPrefix) {
			continue
		}
		generation := baseEpoch
		if pathEpoch > generation {
			generation = pathEpoch
		}
		epochs[pages.ScopeEpoch(generation)] = struct{}{}
		if len(epochs) > semanticMaxScopeEpochs {
			return nil, semanticIndexUnavailable("scope epoch set exceeds bound")
		}
	}
	out := make([]pages.ScopeEpoch, 0, len(epochs))
	for epoch := range epochs {
		out = append(out, epoch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (s *Session) semanticDenseQuery(ctx context.Context, index *indexer.Index, emb embedder.Embedder, text string) (*retrieval.DenseQuery, error) {
	if emb == nil || index == nil || !index.DenseEnabled() {
		return nil, nil
	}
	ns := emb.Namespace()
	if err := ns.Validate(); err != nil {
		s.recordEmbeddingError(indexer.EmbeddingStatusNamespace, err)
		return &retrieval.DenseQuery{UnavailableReason: retrieval.DenseUnavailableNamespace}, nil
	}
	if dims := index.DenseDimensions(); dims != ns.Profile.Dimensions {
		err := fmt.Errorf("%w: index dimensions %d do not match namespace dimensions %d",
			embedder.ErrInvalidNamespace, dims, ns.Profile.Dimensions)
		s.recordEmbeddingError(indexer.EmbeddingStatusNamespace, err)
		return &retrieval.DenseQuery{UnavailableReason: retrieval.DenseUnavailableNamespace}, nil
	}
	query, err := emb.EmbedQuery(ctx, text)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		category := embeddingFailureCategory(indexer.EmbeddingStatusEmbedder, err)
		s.recordEmbeddingError(category, err)
		return &retrieval.DenseQuery{UnavailableReason: denseUnavailableReason(category)}, nil
	}
	if err := query.Validate(ns, embedder.RoleQuery); err != nil {
		s.recordEmbeddingError(indexer.EmbeddingStatusVector, err)
		return &retrieval.DenseQuery{UnavailableReason: retrieval.DenseUnavailableQueryVector}, nil
	}
	return &retrieval.DenseQuery{Namespace: ns.ID, Vector: embeddingVector64(query.Vector)}, nil
}

func denseUnavailableReason(category string) retrieval.DenseUnavailableReason {
	switch category {
	case indexer.EmbeddingStatusNamespace:
		return retrieval.DenseUnavailableNamespace
	case indexer.EmbeddingStatusVector:
		return retrieval.DenseUnavailableQueryVector
	case indexer.EmbeddingStatusAdmissionDenied:
		return retrieval.DenseUnavailableAdmission
	case indexer.EmbeddingStatusDenseUnavailable:
		return retrieval.DenseUnavailableSearch
	default:
		return retrieval.DenseUnavailableEmbedder
	}
}

func semanticTraceFrom(err error, fallback retrieval.RetrievalTrace) retrieval.RetrievalTrace {
	var miss *retrieval.SemanticMissError
	if errors.As(err, &miss) {
		return miss.Trace
	}
	return fallback
}

func semanticScopeMiss(trace retrieval.RetrievalTrace) error {
	trace.Abstention = "scope"
	return &retrieval.SemanticMissError{
		Explanation: "mode=semantic_fault abstention=scope",
		Trace:       trace,
	}
}

func (s *Session) materializeSemanticPage(ctx context.Context, page pages.WarmPage, budget *semanticMaterializationBudget, retrievalTokens int) ([]string, int, string, error) {
	const maxRenderedRefs = 4
	if err := ctx.Err(); err != nil {
		return nil, 0, "", err
	}
	if budget == nil {
		return nil, 0, "", fmt.Errorf("nil semantic materialization budget")
	}
	if budget.attemptsRemaining <= 0 || budget.pagesRemaining <= 0 || budget.refsRemaining <= 0 ||
		budget.tokensRemaining <= 0 || budget.bytesRemaining <= 0 || retrievalTokens <= 0 {
		return nil, 0, "budget", nil
	}
	budget.attemptsRemaining--

	refs := make([]pages.PageRef, 0, maxRenderedRefs)
	for _, ref := range page.Refs {
		if ref.Kind == pages.RefWSLRecord {
			refs = append(refs, ref)
			if len(refs) == maxRenderedRefs || len(refs) == budget.refsRemaining {
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
		return nil, 0, "fault", nil
	}
	availableTokens := budget.tokensRemaining
	if retrievalTokens < availableTokens {
		availableTokens = retrievalTokens
	}
	evidence := make([]string, 0, len(refs))
	tokensUsed := 0
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, 0, "", err
		}
		if budget.refsRemaining <= 0 || availableTokens <= 0 || budget.bytesRemaining <= 0 {
			return nil, 0, "budget", nil
		}
		refsLeft := len(refs) - i
		perRef := availableTokens / refsLeft
		if perRef <= 0 {
			return nil, 0, "budget", nil
		}
		// Resolver truncation appends an ellipsis token, so leave one token of
		// headroom when possible instead of silently exceeding the shared cap.
		requestTokens := perRef
		if requestTokens > 1 {
			requestTokens--
		}
		budget.refsRemaining--
		var (
			body string
			err  error
		)
		switch ref.Kind {
		case pages.RefWSLRecord:
			body, err = s.Tools.ReadPage(ctx, ref.ID, requestTokens)
		case pages.RefEvent:
			body, err = s.Tools.ReadEvent(ctx, ref.ID, requestTokens)
		}
		if err != nil {
			return nil, 0, "", err
		}
		bodyTokens := renderer.EstimateTokens(body)
		bodyBytes := len(body)
		if bodyTokens > availableTokens || bodyTokens > budget.tokensRemaining || bodyBytes > budget.bytesRemaining {
			budget.tokensRemaining = max(0, budget.tokensRemaining-bodyTokens)
			budget.bytesRemaining = max(0, budget.bytesRemaining-bodyBytes)
			return nil, 0, "budget", nil
		}
		budget.tokensRemaining -= bodyTokens
		budget.bytesRemaining -= bodyBytes
		availableTokens -= bodyTokens
		tokensUsed += bodyTokens
		if body == "" || body == faults.PageMiss {
			return nil, 0, "fault", nil
		}
		evidence = append(evidence, body)
	}
	budget.pagesRemaining--
	return evidence, tokensUsed, "", nil
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
