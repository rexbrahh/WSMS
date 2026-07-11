package harness

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"wsms/internal/config"
	"wsms/internal/embedder"
	"wsms/internal/indexer"
	"wsms/internal/pages"
	"wsms/internal/retrieval"
	"wsms/internal/types"
)

func TestEmbedderTimeoutDegradesToFTSOnly(t *testing.T) {
	backend := &recordingBackend{blockUntilContextDone: true, docVector: []float32{1, 0}, queryVector: []float32{1, 0}}
	emb, _ := newSupervisedTestEmbedder(t, backend, embedder.NewDocumentCache(), time.Millisecond)
	s := openEmbeddingSession(t, "embedding-timeout", emb, 0)
	ctx := context.Background()

	if err := s.StartTask(ctx, TaskStart{Goal: "embedding timeout", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "do not evict pinned pages"); err != nil {
		t.Fatal(err)
	}
	if s.IndexErr != nil {
		t.Fatalf("embedding timeout must not poison page indexing: %v", s.IndexErr)
	}
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusSelfCheck)

	body, err := s.PageFault(ctx, "C1")
	if err != nil {
		t.Fatalf("direct page fault after embedding timeout: %v", err)
	}
	if !strings.Contains(body, "do not evict pinned pages") {
		t.Fatalf("page fault body=%q", body)
	}

	result, err := s.SemanticSearch(ctx, "do not evict pinned pages")
	if err != nil {
		t.Fatalf("semantic search should fall back to FTS: %v", err)
	}
	if !resultHasKind(result, pages.KindConstraint) {
		t.Fatalf("FTS fallback did not return constraint: %#v", result.Candidates)
	}
	if !stringSliceContains(result.Degraded, "dense=embedder") {
		t.Fatalf("missing dense degradation marker: %#v", result.Degraded)
	}
	seq, _, err := s.Index.Watermark(ctx, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if seq != s.lastEvent.Seq {
		t.Fatalf("watermark=%d latest=%d", seq, s.lastEvent.Seq)
	}
	health, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !health.EmbeddingDegraded || health.EmbeddingDegradedReason == "" {
		t.Fatalf("health did not expose embedding degradation: %#v", health)
	}
}

func TestDocumentEmbeddingCacheReusesCanonicalSearchText(t *testing.T) {
	cache := embedder.NewDocumentCache()
	backend := &recordingBackend{docVector: []float32{1, 0}, queryVector: []float32{1, 0}}
	emb, ns := newSupervisedTestEmbedder(t, backend, cache, 0)
	s := openEmbeddingSession(t, "embedding-cache", emb, 2)
	ctx := context.Background()
	muts := []pages.PageMutation{
		{Op: pages.MutationUpsert, Page: embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("1", 32), "kind=constraint do not duplicate cache text", strings.Repeat("1", 64), 1)},
		{Op: pages.MutationUpsert, Page: embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("2", 32), "kind=constraint do not duplicate cache text", strings.Repeat("2", 64), 2)},
	}
	if err := s.Index.Apply(ctx, muts); err != nil {
		t.Fatal(err)
	}
	s.wakeEmbeddingWorker()
	waitForVectorCount(t, s, 2)
	waitForEmbeddingStatus(t, s, false, indexer.EmbeddingStatusOK)
	if got := countPayloadContaining(backend.documentPayloads(), "do not duplicate cache text"); got != 1 {
		t.Fatalf("cache miss payloads=%d want one canonical document payload", got)
	}
	if got := cache.Len(); got != 1 {
		t.Fatalf("cache entries=%d want 1", got)
	}
	s.wakeEmbeddingWorker()
	time.Sleep(25 * time.Millisecond)
	if got := countPayloadContaining(backend.documentPayloads(), "do not duplicate cache text"); got != 1 {
		t.Fatalf("cached re-embed sent canonical payload %d times", got)
	}
	hits, err := s.Index.SearchDense(ctx, indexer.SearchQuery{
		SessionID: s.Cfg.SessionID, EmbeddingNamespace: ns.ID, Limit: 3,
	}, []float64{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("dense vector rows=%d want 2 hits: %#v", len(hits), hits)
	}
}

func TestEmbeddingNamespaceMismatchIsRefused(t *testing.T) {
	good := testNamespace(t, 2, "good")
	wrong := testNamespace(t, 2, "wrong")
	manual := &manualEmbedder{
		namespace: good,
		docBatch: func(texts []string) embedder.EmbeddingBatch {
			embeddings := make([]embedder.Embedding, len(texts))
			for i, text := range texts {
				embeddings[i] = embedder.Embedding{Namespace: wrong, Role: embedder.RoleDocument, Vector: []float32{1, 0}, CanonicalText: text}
			}
			return embedder.EmbeddingBatch{Namespace: wrong, Role: embedder.RoleDocument, Embeddings: embeddings, CanonicalTexts: append([]string(nil), texts...)}
		},
		query: embedder.Embedding{Namespace: good, Role: embedder.RoleQuery, Vector: []float32{1, 0}, CanonicalText: "query"},
	}
	s := openEmbeddingSession(t, "embedding-namespace", manual, 2)
	ctx := context.Background()
	muts := []pages.PageMutation{{Op: pages.MutationUpsert, Page: embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("3", 32), "kind=constraint namespace mismatch", strings.Repeat("3", 64), 1)}}
	if err := s.Index.Apply(ctx, muts); err != nil {
		t.Fatal(err)
	}
	s.wakeEmbeddingWorker()
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusVector)
	health, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !health.EmbeddingDegraded || health.VectorCount != 0 {
		t.Fatalf("namespace mismatch health=%#v", health)
	}
}

func TestMalformedDocumentVectorDegradesWithoutVectorRows(t *testing.T) {
	ns := testNamespace(t, 2, "malformed")
	manual := &manualEmbedder{
		namespace: ns,
		docBatch: func(texts []string) embedder.EmbeddingBatch {
			return embedder.EmbeddingBatch{
				Namespace: ns,
				Role:      embedder.RoleDocument,
				Embeddings: []embedder.Embedding{{
					Namespace: ns, Role: embedder.RoleDocument, Vector: []float32{1}, CanonicalText: texts[0],
				}},
				CanonicalTexts: append([]string(nil), texts...),
			}
		},
		query: embedder.Embedding{Namespace: ns, Role: embedder.RoleQuery, Vector: []float32{1, 0}, CanonicalText: "query"},
	}
	s := openEmbeddingSession(t, "embedding-malformed", manual, 2)
	ctx := context.Background()
	muts := []pages.PageMutation{{Op: pages.MutationUpsert, Page: embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("4", 32), "kind=constraint malformed vector", strings.Repeat("4", 64), 1)}}
	if err := s.Index.Apply(ctx, muts); err != nil {
		t.Fatal(err)
	}
	s.wakeEmbeddingWorker()
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusVector)
	health, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.VectorCount != 0 {
		t.Fatalf("malformed vector wrote rows: %#v", health)
	}
}

func TestEmbeddingPayloadExcludesRawArtifactOutput(t *testing.T) {
	backend := &recordingBackend{docVector: []float32{1, 0}, queryVector: []float32{1, 0}}
	emb, _ := newSupervisedTestEmbedder(t, backend, embedder.NewDocumentCache(), 0)
	s := openEmbeddingSession(t, "embedding-redaction", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "embedding redaction", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("TOP_SECRET_RAW_OUTPUT ", 64)
	if err := s.IngestCommandOutput(ctx, "verify-redacted-command", 0, secret); err != nil {
		t.Fatal(err)
	}
	waitForDocCallsAtLeast(t, backend, 1)
	payloads := backend.documentPayloads()
	if len(payloads) == 0 {
		t.Fatal("embedder backend was never called")
	}
	for _, payload := range payloads {
		if strings.Contains(payload, "TOP_SECRET_RAW_OUTPUT") || strings.Contains(payload, "artifact:sha256:") {
			t.Fatalf("raw artifact escaped to embedder payload: %q", payload)
		}
	}
	if _, err := s.SemanticSearch(ctx, "verify-redacted-command"); err != nil && !errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("semantic search returned unexpected error after redacted embedding: %v", err)
	}
}

func TestBlockingDocumentEmbedderDoesNotDelayAppend(t *testing.T) {
	ns := testNamespace(t, 2, "blocking")
	emb := newControlledEmbedder(ns)
	emb.blockDocuments = true
	emb.entered = make(chan struct{}, 1)
	s := openEmbeddingSession(t, "embedding-blocking", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "blocking backend", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	waitControlledEntered(t, emb)

	start := time.Now()
	if err := s.IngestUser(ctx, "append must not wait for document embeddings"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("append waited for blocked embedder: %s", elapsed)
	}
}

func TestEmbeddingBacklogBackfillsSameSessionAndAfterReopen(t *testing.T) {
	ns := testNamespace(t, 2, "backlog")
	emb := newControlledEmbedder(ns)
	emb.setSelfCheckErr(errors.New("temporary backend outage"))
	dir := t.TempDir()
	cfg := embeddingTestConfig(dir, "embedding-backlog", emb, 0)
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "backlog", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "backlog constraint must eventually get a vector"); err != nil {
		t.Fatal(err)
	}
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusSelfCheck)
	seq, _, err := s.Index.Watermark(ctx, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if seq != s.lastEvent.Seq {
		t.Fatalf("watermark=%d latest=%d", seq, s.lastEvent.Seq)
	}
	if got := emb.docCallCount(); got != 0 {
		t.Fatalf("document embeddings ran during self-check outage: calls=%d payloads=%v", got, emb.documentPayloads())
	}
	initialMissing := requireMissingVectorPages(t, s, ns.ID)
	initialConstraint := requireMissingPageContaining(t, initialMissing, "backlog constraint must eventually get a vector")
	requirePageRef(t, initialConstraint, pages.RefWSLRecord, "C1")
	requireNoDenseTuple(t, s, initialConstraint)

	emb.setSelfCheckErr(nil)
	waitForNoMissingVectorPages(t, s, ns.ID)
	assertDenseSearchContainsCurrentPage(t, s, ns.ID, initialConstraint)
	requireDenseTuple(t, s, initialConstraint, ns.ID)
	waitForEmbeddingStatus(t, s, false, indexer.EmbeddingStatusOK)

	emb.setSelfCheckErr(errors.New("second temporary outage"))
	secondConstraint := "do not let this missing page skip backfill after reopen"
	if err := s.IngestUser(ctx, secondConstraint); err != nil {
		t.Fatal(err)
	}
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusSelfCheck)
	seq, _, err = s.Index.Watermark(ctx, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if seq != s.lastEvent.Seq {
		t.Fatalf("watermark after second failure=%d latest=%d", seq, s.lastEvent.Seq)
	}
	reopenMissing := requireMissingVectorPages(t, s, ns.ID)
	reopenConstraint := requireMissingPageContaining(t, reopenMissing, secondConstraint)
	requirePageRef(t, reopenConstraint, pages.RefWSLRecord, "C2")
	requireNoDenseTuple(t, s, reopenConstraint)
	if reopenConstraint.Version != pages.PageVersion(s.lastEvent.Seq) {
		t.Fatalf("missing page version=%d want latest event seq %d", reopenConstraint.Version, s.lastEvent.Seq)
	}
	if reopenConstraint.ID == initialConstraint.ID {
		t.Fatalf("second backlog page reused first logical page id %s", reopenConstraint.ID)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopenEmb := newControlledEmbedder(ns)
	reopened, err := OpenSession(embeddingTestConfig(dir, "embedding-backlog", reopenEmb, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestSession(t, reopened)
	waitForNoMissingVectorPages(t, reopened, ns.ID)
	assertDenseSearchContainsCurrentPage(t, reopened, ns.ID, reopenConstraint)
	requireDenseTuple(t, reopened, reopenConstraint, ns.ID)
	waitForEmbeddingStatus(t, reopened, false, indexer.EmbeddingStatusOK)
}

func TestEmbeddingWorkerAutomaticallyRetriesTransientBacklogFailure(t *testing.T) {
	ns := testNamespace(t, 2, "auto-retry")
	emb := newControlledEmbedder(ns)
	emb.setSelfCheckErr(errors.New("temporary self-check outage"))
	s := openEmbeddingSession(t, "embedding-auto-retry", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "auto retry", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "transient failure must not strand dense backlog"); err != nil {
		t.Fatal(err)
	}
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusSelfCheck)
	missing := requireMissingVectorPages(t, s, ns.ID)
	constraint := requireMissingPageContaining(t, missing, "transient failure must not strand dense backlog")
	requireNoDenseTuple(t, s, constraint)

	emb.setSelfCheckErr(nil)
	waitForNoMissingVectorPages(t, s, ns.ID)
	requireDenseTuple(t, s, constraint, ns.ID)
	waitForEmbeddingStatus(t, s, false, indexer.EmbeddingStatusOK)
	if got := emb.docCallCount(); got == 0 {
		t.Fatal("automatic retry never reached document embedding")
	}
}

func TestEmbeddingNamespaceChangeRecreatesGenerationAndReembeds(t *testing.T) {
	dir := t.TempDir()
	nsA := testNamespace(t, 2, "namespace-a")
	embA := newControlledEmbedder(nsA)
	s, err := OpenSession(embeddingTestConfig(dir, "embedding-namespace-recreate", embA, 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "namespace recreate", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	waitForVectorCountAtLeast(t, s, 1)
	healthA, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if healthA.EmbeddingNamespace != nsA.ID {
		t.Fatalf("namespace A not persisted: %#v", healthA)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	nsB := testNamespace(t, 2, "namespace-b")
	embB := newControlledEmbedder(nsB)
	reopened, err := OpenSession(embeddingTestConfig(dir, "embedding-namespace-recreate", embB, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestSession(t, reopened)
	waitForVectorCountAtLeast(t, reopened, 1)
	healthB, err := reopened.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if healthB.EmbeddingNamespace != nsB.ID {
		t.Fatalf("namespace B not persisted: %#v", healthB)
	}
	if healthB.Generation <= healthA.Generation {
		t.Fatalf("generation did not bump on namespace recreation: before=%d after=%d", healthA.Generation, healthB.Generation)
	}
	if healthB.PageCount == 0 {
		t.Fatal("namespace recreation did not replay pages/FTS")
	}
}

func TestEmbeddingWorkerCloseCancelsBlockedProjection(t *testing.T) {
	ns := testNamespace(t, 2, "close-cancel")
	emb := newControlledEmbedder(ns)
	emb.blockDocuments = true
	emb.entered = make(chan struct{}, 1)
	emb.canceled = make(chan struct{}, 1)
	s := openEmbeddingSession(t, "embedding-close-cancel", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "close cancel", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	waitControlledEntered(t, emb)
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Close did not promptly cancel worker: %s", elapsed)
	}
	select {
	case <-emb.canceled:
	default:
		t.Fatal("worker embed context was not canceled")
	}
}

func TestSemanticSearchConcurrentCloseIsRaceFree(t *testing.T) {
	ns := testNamespace(t, 2, "semantic-close")
	emb := newControlledEmbedder(ns)
	emb.blockQuery = true
	emb.queryEntered = make(chan struct{}, 1)
	emb.queryCanceled = make(chan struct{}, 1)
	s := openEmbeddingSession(t, "semantic-close", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "semantic close", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "semantic search can overlap close without use after close"); err != nil {
		t.Fatal(err)
	}
	waitForVectorCountAtLeast(t, s, 1)

	searchCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.SemanticSearch(searchCtx, "semantic search can overlap close")
		done <- err
	}()
	select {
	case <-emb.queryEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("semantic search did not enter dense query probe")
	}

	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Close waited for semantic dense query: %s", elapsed)
	}
	cancel()
	select {
	case <-emb.queryCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("semantic query context was not canceled")
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionClosed) && !errors.Is(err, context.Canceled) {
			t.Fatalf("SemanticSearch error after close = %v, want ErrSessionClosed or context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SemanticSearch did not finish after close and context cancellation")
	}
	if _, err := s.SemanticSearch(context.Background(), "after close"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("SemanticSearch after close error=%v, want ErrSessionClosed", err)
	}
}

func TestEmbeddingHealthUsesRedactedCategories(t *testing.T) {
	ns := testNamespace(t, 2, "health-redaction")
	emb := newControlledEmbedder(ns)
	emb.setDocErr(errors.New("TOP_SECRET backend token should not enter health"))
	s := openEmbeddingSession(t, "embedding-health-redaction", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "health redaction", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusEmbedder)
	health, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.EmbeddingDegradedReason != indexer.EmbeddingStatusEmbedder {
		t.Fatalf("health reason=%q want category %q", health.EmbeddingDegradedReason, indexer.EmbeddingStatusEmbedder)
	}
	if strings.Contains(health.EmbeddingDegradedReason, "TOP_SECRET") {
		t.Fatalf("health leaked raw error: %#v", health)
	}
}

func TestFailurePageSecretIsNotSentToEmbeddingBackend(t *testing.T) {
	backend := &recordingBackend{docVector: []float32{1, 0}, queryVector: []float32{1, 0}}
	emb, _ := newSupervisedTestEmbedder(t, backend, embedder.NewDocumentCache(), 0)
	s := openEmbeddingSession(t, "embedding-failure-secret", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "failure secret", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	secret := "TOP_SECRET failure line\n" + strings.Repeat("padding ", 20)
	if err := s.IngestCommandOutput(ctx, "secret-failing-command", 1, secret); err != nil {
		t.Fatal(err)
	}
	waitForVectorCountAtLeast(t, s, 1)
	for _, payload := range backend.documentPayloads() {
		if strings.Contains(payload, "TOP_SECRET") {
			t.Fatalf("secret failure text reached embedding backend: %q", payload)
		}
	}
}

func openEmbeddingSession(t *testing.T, id string, emb embedder.Embedder, denseDims int) *Session {
	t.Helper()
	cfg := embeddingTestConfig(t.TempDir(), id, emb, denseDims)
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	return s
}

func embeddingTestConfig(dir, id string, emb embedder.Embedder, denseDims int) config.Config {
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.SessionID = id
	cfg.ArtifactThresholdBytes = 32
	cfg.CapsuleTokenBudget = 1024
	cfg.PageFaultTokenBudget = 4096
	cfg.DenseDimensions = denseDims
	cfg.Embedder = emb
	cfg.EmbeddingBatchSize = 4
	return cfg
}

func newSupervisedTestEmbedder(t *testing.T, backend *recordingBackend, cache *embedder.DocumentCache, timeout time.Duration) (*embedder.Supervised, embedder.EmbeddingNamespace) {
	t.Helper()
	ns := testNamespace(t, 2, "supervised")
	emb, err := embedder.New(embedder.Options{
		Namespace: ns,
		Backend:   backend,
		Cache:     cache,
		Timeout:   timeout,
		Breaker:   embedder.BreakerOptions{FailureThreshold: 100, Cooldown: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	return emb, ns
}

func testNamespace(t *testing.T, dims int, revision string) embedder.EmbeddingNamespace {
	t.Helper()
	ns, err := embedder.NewNamespace(embedder.NamespaceProfile{
		Provider:          "test",
		ModelRepository:   "local/test-embedder",
		ExactRevision:     revision,
		Dimensions:        dims,
		DistanceMetric:    embedder.MetricCosine,
		Normalization:     embedder.NormalizationL2,
		QueryInstruction:  "Represent this query for WSMS retrieval.",
		DocumentTemplate:  "{{.SearchText}}",
		TokenizerRevision: "tok-" + revision,
		PageSchemaVersion: string(pages.CurrentCompilerVersion),
		RedactionVersion:  embedder.CurrentRedactionVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

type recordingBackend struct {
	mu                    sync.Mutex
	blockUntilContextDone bool
	docVector             []float32
	queryVector           []float32
	docCalls              int
	queryCalls            int
	docPayloads           []string
	queryPayloads         []string
}

func (b *recordingBackend) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	b.mu.Lock()
	b.docCalls++
	b.docPayloads = append(b.docPayloads, texts...)
	b.mu.Unlock()
	if b.blockUntilContextDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = append([]float32(nil), b.docVector...)
	}
	return vectors, nil
}

func (b *recordingBackend) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	b.mu.Lock()
	b.queryCalls++
	b.queryPayloads = append(b.queryPayloads, text)
	b.mu.Unlock()
	if b.blockUntilContextDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]float32(nil), b.queryVector...), nil
}

func (b *recordingBackend) docCallCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.docCalls
}

func (b *recordingBackend) documentPayloads() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.docPayloads...)
}

type controlledEmbedder struct {
	mu             sync.Mutex
	namespace      embedder.EmbeddingNamespace
	docVector      []float32
	queryVector    []float32
	docErr         error
	selfCheckErr   error
	blockDocuments bool
	blockQuery     bool
	entered        chan struct{}
	canceled       chan struct{}
	queryEntered   chan struct{}
	queryCanceled  chan struct{}
	docCalls       int
	docPayloads    []string
}

func newControlledEmbedder(ns embedder.EmbeddingNamespace) *controlledEmbedder {
	return &controlledEmbedder{
		namespace:   ns,
		docVector:   []float32{1, 0},
		queryVector: []float32{1, 0},
	}
}

func (e *controlledEmbedder) EmbedDocuments(ctx context.Context, texts []string) (embedder.EmbeddingBatch, error) {
	e.mu.Lock()
	e.docCalls++
	e.docPayloads = append(e.docPayloads, texts...)
	docErr := e.docErr
	block := e.blockDocuments
	entered := e.entered
	canceled := e.canceled
	vector := append([]float32(nil), e.docVector...)
	ns := e.namespace
	e.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block {
		<-ctx.Done()
		if canceled != nil {
			select {
			case canceled <- struct{}{}:
			default:
			}
		}
		return embedder.EmbeddingBatch{}, ctx.Err()
	}
	if docErr != nil {
		return embedder.EmbeddingBatch{}, docErr
	}
	embeddings := make([]embedder.Embedding, len(texts))
	for i, text := range texts {
		embeddings[i] = embedder.Embedding{
			Namespace: ns, Role: embedder.RoleDocument,
			Vector: append([]float32(nil), vector...), CanonicalText: text,
		}
	}
	return embedder.EmbeddingBatch{
		Namespace: ns, Role: embedder.RoleDocument,
		Embeddings: embeddings, CanonicalTexts: append([]string(nil), texts...),
	}, nil
}

func (e *controlledEmbedder) EmbedQuery(ctx context.Context, text string) (embedder.Embedding, error) {
	e.mu.Lock()
	block := e.blockQuery
	entered := e.queryEntered
	canceled := e.queryCanceled
	ns := e.namespace
	vector := append([]float32(nil), e.queryVector...)
	e.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block {
		<-ctx.Done()
		if canceled != nil {
			select {
			case canceled <- struct{}{}:
			default:
			}
		}
		return embedder.Embedding{}, ctx.Err()
	}
	return embedder.Embedding{
		Namespace: ns, Role: embedder.RoleQuery,
		Vector: vector, CanonicalText: text,
	}, nil
}

func (e *controlledEmbedder) Namespace() embedder.EmbeddingNamespace {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.namespace
}

func (e *controlledEmbedder) SelfCheck(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.selfCheckErr
}

func (e *controlledEmbedder) setDocErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.docErr = err
}

func (e *controlledEmbedder) setSelfCheckErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.selfCheckErr = err
}

func (e *controlledEmbedder) docCallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.docCalls
}

func (e *controlledEmbedder) documentPayloads() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.docPayloads...)
}

type manualEmbedder struct {
	namespace embedder.EmbeddingNamespace
	docBatch  func([]string) embedder.EmbeddingBatch
	query     embedder.Embedding
}

func (m *manualEmbedder) EmbedDocuments(_ context.Context, texts []string) (embedder.EmbeddingBatch, error) {
	return m.docBatch(texts), nil
}

func (m *manualEmbedder) EmbedQuery(_ context.Context, text string) (embedder.Embedding, error) {
	query := m.query
	query.CanonicalText = text
	return query, nil
}

func (m *manualEmbedder) Namespace() embedder.EmbeddingNamespace {
	return m.namespace
}

func embeddingWarmPage(session, id, search, digest string, version int64) pages.WarmPage {
	page := pages.WarmPage{
		ID: pages.PageID(id), Version: pages.PageVersion(version), SessionID: session,
		RepoID: "repo", TaskID: "task", Scope: types.ScopeTask,
		Kind: pages.KindConstraint, Trust: pages.TrustUser, Status: pages.StatusActive,
		Salience: 0.9, SalienceReason: "embedding test fixture",
		SearchText: search, Summary: search,
		Refs:         []pages.PageRef{{Kind: pages.RefEvent, ID: "E0001"}},
		SourceSeqMin: 1, SourceSeqMax: 1,
		SourceDigest:    pages.SourceDigest(digest),
		CompilerVersion: pages.CurrentCompilerVersion,
		CreatedAt:       time.Unix(version, 0).UTC(),
		LastVerifiedAt:  time.Unix(version, 0).UTC(),
	}
	if err := (pages.PageMutation{Op: pages.MutationUpsert, Page: page}).Validate(); err != nil {
		panic(err)
	}
	return page
}

func waitForEmbeddingStatus(t *testing.T, s *Session, degraded bool, category string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := s.EmbeddingStatus()
		if status.Degraded == degraded && status.Category == category {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("embedding status=%#v want degraded=%v category=%q", status, degraded, category)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForVectorCount(t *testing.T, s *Session, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		health, err := s.Index.Health(context.Background())
		if err == nil && health.VectorCount == want {
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("vector health error: %v", err)
			}
			health, _ := s.Index.Health(context.Background())
			t.Fatalf("vector count=%d want %d", health.VectorCount, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForVectorCountAtLeast(t *testing.T, s *Session, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		health, err := s.Index.Health(context.Background())
		if err == nil && health.VectorCount >= want {
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("vector health error: %v", err)
			}
			health, _ := s.Index.Health(context.Background())
			t.Fatalf("vector count=%d want at least %d", health.VectorCount, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func requireMissingVectorPages(t *testing.T, s *Session, namespace string) []pages.WarmPage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		missing, err := s.Index.MissingVectorPages(context.Background(), s.Cfg.SessionID, namespace, 16)
		if err == nil && len(missing) > 0 {
			return missing
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("missing vector pages: %v", err)
			}
			health, _ := s.Index.Health(context.Background())
			t.Fatalf("expected at least one missing vector page; health=%#v", health)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForNoMissingVectorPages(t *testing.T, s *Session, namespace string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		missing, err := s.Index.MissingVectorPages(context.Background(), s.Cfg.SessionID, namespace, 16)
		if err == nil && len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("missing vector pages: %v", err)
			}
			t.Fatalf("missing vector pages after backfill: %v", pageIDs(missing))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func requireMissingPageContaining(t *testing.T, missing []pages.WarmPage, needle string) pages.WarmPage {
	t.Helper()
	for _, page := range missing {
		if strings.Contains(page.SearchText, needle) {
			return page
		}
	}
	t.Fatalf("missing pages %v did not include search text containing %q", pageIDs(missing), needle)
	return pages.WarmPage{}
}

func assertDenseSearchContainsCurrentPage(t *testing.T, s *Session, namespace string, want pages.WarmPage) {
	t.Helper()
	results, err := s.Index.SearchDense(context.Background(), indexer.SearchQuery{
		SessionID:          s.Cfg.SessionID,
		EmbeddingNamespace: namespace,
		Limit:              16,
	}, []float64{1, 0})
	if err != nil {
		t.Fatalf("dense search: %v", err)
	}
	for _, result := range results {
		page := result.Page
		if page.ID != want.ID {
			continue
		}
		if page.Version != want.Version || page.SourceDigest != want.SourceDigest || page.CompilerVersion != want.CompilerVersion {
			t.Fatalf("dense page tuple=%s/%d/%s/%s want %s/%d/%s/%s",
				page.ID, page.Version, page.SourceDigest, page.CompilerVersion,
				want.ID, want.Version, want.SourceDigest, want.CompilerVersion)
		}
		return
	}
	t.Fatalf("dense search did not return current page %s; got %v", want.ID, candidatePageIDs(results))
}

func requirePageRef(t *testing.T, page pages.WarmPage, kind pages.RefKind, id string) {
	t.Helper()
	for _, ref := range page.Refs {
		if ref.Kind == kind && ref.ID == id {
			return
		}
	}
	t.Fatalf("page %s refs=%#v missing %s:%s", page.ID, page.Refs, kind, id)
}

func requireDenseTuple(t *testing.T, s *Session, page pages.WarmPage, namespace string) {
	t.Helper()
	row := queryDenseTuple(t, s, page.ID)
	if row.sessionID != page.SessionID ||
		row.pageVersion != page.Version ||
		row.sourceDigest != page.SourceDigest ||
		row.compilerVersion != page.CompilerVersion ||
		row.namespace != namespace {
		t.Fatalf("dense tuple for %s = session=%q version=%d digest=%s compiler=%s namespace=%q; want session=%q version=%d digest=%s compiler=%s namespace=%q",
			page.ID, row.sessionID, row.pageVersion, row.sourceDigest, row.compilerVersion, row.namespace,
			page.SessionID, page.Version, page.SourceDigest, page.CompilerVersion, namespace)
	}
}

func requireNoDenseTuple(t *testing.T, s *Session, page pages.WarmPage) {
	t.Helper()
	if row, ok := lookupDenseTuple(t, s, page.ID); ok {
		t.Fatalf("dense tuple unexpectedly exists for missing page %s: session=%q version=%d digest=%s compiler=%s namespace=%q",
			page.ID, row.sessionID, row.pageVersion, row.sourceDigest, row.compilerVersion, row.namespace)
	}
}

type denseTuple struct {
	sessionID       string
	pageVersion     pages.PageVersion
	sourceDigest    pages.SourceDigest
	compilerVersion pages.CompilerVersion
	namespace       string
}

func queryDenseTuple(t *testing.T, s *Session, pageID pages.PageID) denseTuple {
	t.Helper()
	row, ok := lookupDenseTuple(t, s, pageID)
	if !ok {
		t.Fatalf("dense tuple for page %s not found", pageID)
	}
	return row
}

func lookupDenseTuple(t *testing.T, s *Session, pageID pages.PageID) (denseTuple, bool) {
	t.Helper()
	health, err := s.Index.Health(context.Background())
	if err != nil {
		t.Fatalf("index health: %v", err)
	}
	db, err := sql.Open("sqlite", health.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close dense tuple db: %v", err)
		}
	}()
	var row denseTuple
	err = db.QueryRowContext(context.Background(), `
SELECT m.session_id, m.page_version, m.source_digest, m.compiler_version, m.embedding_namespace
FROM warm_page_vec_map m
JOIN warm_pages_vec v ON v.rowid = m.rowid
WHERE m.page_id = ?`, string(pageID)).Scan(&row.sessionID, &row.pageVersion, &row.sourceDigest, &row.compilerVersion, &row.namespace)
	if err == sql.ErrNoRows {
		return denseTuple{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row, true
}

func pageIDs(warmPages []pages.WarmPage) []pages.PageID {
	ids := make([]pages.PageID, len(warmPages))
	for i, page := range warmPages {
		ids[i] = page.ID
	}
	return ids
}

func candidatePageIDs(results []indexer.Candidate) []pages.PageID {
	ids := make([]pages.PageID, len(results))
	for i, result := range results {
		ids[i] = result.Page.ID
	}
	return ids
}

func waitForDocCallsAtLeast(t *testing.T, backend *recordingBackend, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := backend.docCallCount(); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("doc calls=%d want at least %d", backend.docCallCount(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitControlledEntered(t *testing.T, emb *controlledEmbedder) {
	t.Helper()
	select {
	case <-emb.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("controlled embedder was not called")
	}
}

func countPayloadContaining(payloads []string, needle string) int {
	count := 0
	for _, payload := range payloads {
		if strings.Contains(payload, needle) {
			count++
		}
	}
	return count
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
