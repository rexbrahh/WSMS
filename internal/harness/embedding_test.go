package harness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	operational, err := s.SemanticSearch(ctx, "zxqv no lexical page contains this phrase")
	if !errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
		t.Fatalf("empty FTS plus unavailable query embedding error=%v", err)
	}
	if len(operational.Degraded) == 0 || operational.Trace.FusionVersion == "" {
		t.Fatalf("operational degradation lost categorical trace: %#v", operational)
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

func TestSemanticSearchUsesDenseChannelThroughHybridRetriever(t *testing.T) {
	ns := testNamespace(t, 2, "hybrid-query")
	emb := newControlledEmbedder(ns)
	s := openEmbeddingSession(t, "embedding-hybrid-query", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "hybrid query", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "second chance replacement keeps frequently reused pages warm"); err != nil {
		t.Fatal(err)
	}
	waitForVectorCountAtLeast(t, s, 1)

	result, err := s.SemanticSearch(ctx, "second chance replacement keeps frequently reused pages warm")
	if err != nil {
		t.Fatal(err)
	}
	if result.Trace.DenseCandidates == 0 {
		t.Fatalf("hybrid result did not consume dense candidates: %#v", result.Trace)
	}
	if len(result.Materialized) == 0 {
		t.Fatalf("hybrid result did not materialize exact evidence: %#v", result)
	}
	for _, candidate := range result.Candidates {
		if candidate.Page.SearchText != "" || candidate.Page.Summary != "" || len(candidate.Page.Refs) != 0 {
			t.Fatalf("hybrid result exposed indexed derivative content: %#v", candidate.Page)
		}
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

func TestBlockingQueryEmbedderDoesNotDelayAppend(t *testing.T) {
	ns := testNamespace(t, 2, "blocking-query")
	emb := newControlledEmbedder(ns)
	emb.blockQuery = true
	emb.queryEntered = make(chan struct{}, 1)
	s := openEmbeddingSession(t, "embedding-blocking-query", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "blocking query", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "query embedding must not own the append lock"); err != nil {
		t.Fatal(err)
	}
	waitForVectorCountAtLeast(t, s, 1)

	searchCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.SemanticSearch(searchCtx, "query embedding must not own the append lock")
		done <- err
	}()
	select {
	case <-emb.queryEntered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("semantic search did not enter query embedding")
	}

	start := time.Now()
	if err := s.IngestAssistant(ctx, "append progresses while the query provider is blocked"); err != nil {
		cancel()
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		cancel()
		t.Fatalf("append waited for blocked query embedder: %s", elapsed)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SemanticSearch error=%v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SemanticSearch did not stop after caller cancellation")
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

	done := make(chan error, 1)
	go func() {
		_, err := s.SemanticSearch(context.Background(), "semantic search can overlap close")
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
		t.Fatal("SemanticSearch did not finish after session close cancellation")
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

func TestAdmissionDeniedPageDoesNotStarveSafeEmbeddingBacklog(t *testing.T) {
	backend := &recordingBackend{docVector: []float32{1, 0}, queryVector: []float32{1, 0}}
	emb, ns := newSupervisedTestEmbedder(t, backend, embedder.NewDocumentCache(), 0)
	s := openEmbeddingSession(t, "embedding-admission-denied", emb, 0)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "admission denied", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	deniedText := "path: .env must remain lexical only"
	if err := s.IngestUser(ctx, deniedText); err != nil {
		t.Fatal(err)
	}
	safeText := "hard constraint: safe dense backlog should still backfill after denied page"
	if err := s.IngestUser(ctx, safeText); err != nil {
		t.Fatal(err)
	}
	deniedPage := requireWarmPageContaining(t, s, deniedText)
	safePage := requireWarmPageContaining(t, s, safeText)
	waitForNoMissingVectorPages(t, s, ns.ID)
	requireNoDenseTuple(t, s, deniedPage)
	requireDenseTuple(t, s, safePage, ns.ID)
	waitForEmbeddingStatus(t, s, false, indexer.EmbeddingStatusOK)
	if got := countPayloadContaining(backend.documentPayloads(), deniedText); got != 0 {
		t.Fatalf("admission-denied text reached backend %d times; payloads=%v", got, backend.documentPayloads())
	}
	if got := countPayloadContaining(backend.documentPayloads(), safeText); got == 0 {
		t.Fatalf("safe text never reached backend; payloads=%v", backend.documentPayloads())
	}
}

func TestAdmissionFallbackCheckpointsSafeAndSuppressedBeforeLaterTransient(t *testing.T) {
	ns := testNamespace(t, 2, "fallback-progress")
	safeText := "kind=constraint safe vector should checkpoint before later transient"
	deniedText := "kind=constraint path: .env should be suppressed before later transient"
	transientText := "kind=constraint transient backend page should remain missing"
	emb := &scriptedEmbedder{namespace: ns}
	emb.setDocFunc(func(ctx context.Context, texts []string, _ int) (embedder.EmbeddingBatch, error) {
		if len(texts) > 1 {
			return embedder.EmbeddingBatch{}, embedder.ErrAdmissionDenied
		}
		switch {
		case strings.Contains(texts[0], safeText):
			return documentBatchForTexts(ns, texts, []float32{1, 0}), nil
		case strings.Contains(texts[0], deniedText):
			return embedder.EmbeddingBatch{}, embedder.ErrAdmissionDenied
		case strings.Contains(texts[0], transientText):
			return embedder.EmbeddingBatch{}, errors.New("temporary embedder availability fault")
		default:
			return embedder.EmbeddingBatch{}, errors.New("unexpected embedding text")
		}
	})
	s := openEmbeddingSession(t, "embedding-fallback-progress", emb, 2)
	ctx := context.Background()
	safe := embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("1", 32), safeText, strings.Repeat("1", 64), 1)
	denied := embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("2", 32), deniedText, strings.Repeat("2", 64), 2)
	transient := embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("3", 32), transientText, strings.Repeat("3", 64), 3)
	if err := s.Index.Apply(ctx, []pages.PageMutation{
		{Op: pages.MutationUpsert, Page: safe},
		{Op: pages.MutationUpsert, Page: denied},
		{Op: pages.MutationUpsert, Page: transient},
	}); err != nil {
		t.Fatal(err)
	}
	s.wakeEmbeddingWorker()
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusEmbedder)

	requireDenseTuple(t, s, safe, ns.ID)
	requireNoDenseTuple(t, s, denied)
	requireNoDenseTuple(t, s, transient)
	missing, err := s.Index.MissingVectorPages(ctx, s.Cfg.SessionID, ns.ID, 16)
	if err != nil {
		t.Fatal(err)
	}
	if missingPageContains(missing, safeText) {
		t.Fatalf("checkpointed safe page still appears missing: %v", pageIDs(missing))
	}
	if missingPageContains(missing, deniedText) {
		t.Fatalf("suppressed denied page still appears missing: %v", pageIDs(missing))
	}
	if !missingPageContains(missing, transientText) {
		t.Fatalf("transient page should remain retryable/missing; got %v", pageIDs(missing))
	}
}

func TestEmbeddingTerminalFailureParksUntilNewWake(t *testing.T) {
	ns := testNamespace(t, 2, "terminal-park")
	emb := &scriptedEmbedder{namespace: ns}
	emb.setDocFunc(func(ctx context.Context, texts []string, _ int) (embedder.EmbeddingBatch, error) {
		return documentBatchForTexts(ns, texts, []float32{1}), nil
	})
	s := openEmbeddingSession(t, "embedding-terminal-park", emb, 2)
	ctx := context.Background()
	page := embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("4", 32), "kind=constraint terminal vector waits for new wake", strings.Repeat("4", 64), 1)
	if err := s.Index.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	s.wakeEmbeddingWorker()
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusVector)
	callsBeforeRecovery := waitForDocCallsStable(t, emb, 125*time.Millisecond)
	if callsBeforeRecovery == 0 || callsBeforeRecovery > 2 {
		t.Fatalf("terminal vector failure should consume only finite coalesced wakes before parking; calls=%d", callsBeforeRecovery)
	}

	emb.setDocFunc(func(ctx context.Context, texts []string, _ int) (embedder.EmbeddingBatch, error) {
		return documentBatchForTexts(ns, texts, []float32{1, 0}), nil
	})
	s.wakeEmbeddingWorker()
	waitForNoMissingVectorPages(t, s, ns.ID)
	requireDenseTuple(t, s, page, ns.ID)
	waitForEmbeddingStatus(t, s, false, indexer.EmbeddingStatusOK)
}

func TestEmbeddingStaleResultDoesNotOverwriteNewTuple(t *testing.T) {
	ns := testNamespace(t, 2, "stale-tuple")
	firstEntered := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{}, 1)
	releaseSecond := make(chan struct{})
	emb := &scriptedEmbedder{namespace: ns}
	emb.setDocFunc(func(ctx context.Context, texts []string, call int) (embedder.EmbeddingBatch, error) {
		switch call {
		case 1:
			firstEntered <- struct{}{}
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return embedder.EmbeddingBatch{}, ctx.Err()
			}
		case 2:
			secondEntered <- struct{}{}
			select {
			case <-releaseSecond:
			case <-ctx.Done():
				return embedder.EmbeddingBatch{}, ctx.Err()
			}
		}
		return documentBatchForTexts(ns, texts, []float32{1, 0}), nil
	})
	s := openEmbeddingSession(t, "embedding-stale-tuple", emb, 2)
	ctx := context.Background()
	pageV1 := embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("5", 32), "kind=constraint stale tuple old text", strings.Repeat("5", 64), 1)
	if err := s.Index.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: pageV1}}); err != nil {
		t.Fatal(err)
	}
	s.wakeEmbeddingWorker()
	waitForSignal(t, firstEntered, "first stale embedding call")

	pageV2 := embeddingWarmPage(s.Cfg.SessionID, string(pageV1.ID), "kind=constraint stale tuple new text", strings.Repeat("6", 64), 2)
	if err := s.Index.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: pageV2}}); err != nil {
		t.Fatal(err)
	}
	s.wakeEmbeddingWorker()
	close(releaseFirst)
	waitForSignal(t, secondEntered, "fresh tuple embedding call")
	requireNoDenseTuple(t, s, pageV2)

	close(releaseSecond)
	waitForNoMissingVectorPages(t, s, ns.ID)
	requireDenseTuple(t, s, pageV2, ns.ID)
	waitForEmbeddingStatus(t, s, false, indexer.EmbeddingStatusOK)
}

func TestSupervisedInvalidEmbeddingParksUntilWakeAndRedactsHealth(t *testing.T) {
	backend := &recordingBackend{docVector: []float32{1, 0}, queryVector: []float32{1, 0}}
	backend.setDocErr(fmt.Errorf("%w: TOP_SECRET malformed local sidecar response", embedder.ErrInvalidEmbedding))
	emb, ns := newSupervisedTestEmbedder(t, backend, embedder.NewDocumentCache(), 0)
	s := openEmbeddingSession(t, "embedding-supervised-invalid-parks", emb, 2)
	ctx := context.Background()
	page := embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("8", 32), "kind=constraint supervised invalid vector parks", strings.Repeat("8", 64), 1)
	if err := s.Index.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	s.wakeEmbeddingWorker()
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusVector)
	callsBeforeRecovery := waitForRecordingDocCallsStable(t, backend, 125*time.Millisecond)
	if callsBeforeRecovery == 0 || callsBeforeRecovery > 2 {
		t.Fatalf("terminal supervised invalid embedding should consume only finite coalesced wakes before parking; calls=%d", callsBeforeRecovery)
	}
	health, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !health.EmbeddingDegraded || health.EmbeddingDegradedReason != indexer.EmbeddingStatusVector {
		t.Fatalf("health=%#v want redacted vector degradation", health)
	}
	if strings.Contains(health.EmbeddingDegradedReason, "invalid") || strings.Contains(health.EmbeddingDegradedReason, "TOP_SECRET") {
		t.Fatalf("health leaked raw vector validation text: %#v", health)
	}

	backend.setDocErr(nil)
	s.wakeEmbeddingWorker()
	waitForNoMissingVectorPages(t, s, ns.ID)
	requireDenseTuple(t, s, page, ns.ID)
	waitForEmbeddingStatus(t, s, false, indexer.EmbeddingStatusOK)
}

func TestSupervisedTransientBackendStillAutoRetries(t *testing.T) {
	backend := &recordingBackend{docVector: []float32{1, 0}, queryVector: []float32{1, 0}}
	backend.setDocErr(errors.New("TOP_SECRET transient backend outage"))
	emb, ns := newSupervisedTestEmbedder(t, backend, embedder.NewDocumentCache(), 0)
	s := openEmbeddingSession(t, "embedding-supervised-transient-retry", emb, 2)
	ctx := context.Background()
	page := embeddingWarmPage(s.Cfg.SessionID, "wp_"+strings.Repeat("9", 32), "kind=constraint supervised transient retries", strings.Repeat("9", 64), 1)
	if err := s.Index.Apply(ctx, []pages.PageMutation{{Op: pages.MutationUpsert, Page: page}}); err != nil {
		t.Fatal(err)
	}
	s.wakeEmbeddingWorker()
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusSelfCheck)
	health, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.EmbeddingDegradedReason != indexer.EmbeddingStatusSelfCheck || strings.Contains(health.EmbeddingDegradedReason, "TOP_SECRET") {
		t.Fatalf("health reason not redacted self-check category: %#v", health)
	}

	backend.setDocErr(nil)
	waitForNoMissingVectorPages(t, s, ns.ID)
	requireDenseTuple(t, s, page, ns.ID)
	waitForEmbeddingStatus(t, s, false, indexer.EmbeddingStatusOK)
	if got := backend.docCallCount(); got < 2 {
		t.Fatalf("transient backend did not auto-retry after outage cleared; calls=%d", got)
	}
}

func TestDenseProbeClassifiesSupervisedInvalidQueryVector(t *testing.T) {
	backend := &recordingBackend{docVector: []float32{1, 0}, queryVector: []float32{1}}
	emb, _ := newSupervisedTestEmbedder(t, backend, embedder.NewDocumentCache(), 0)
	s := openEmbeddingSession(t, "embedding-query-vector-classifier", emb, 2)
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: "query vector classifier", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	text := "do not rely on invalid query vector dense path"
	if err := s.IngestUser(ctx, text); err != nil {
		t.Fatal(err)
	}
	result, err := s.SemanticSearch(ctx, text)
	if err != nil {
		t.Fatalf("semantic search should keep FTS fallback after invalid dense query: %v", err)
	}
	if !stringSliceContains(result.Degraded, "dense=query-vector") {
		t.Fatalf("dense degradation markers=%#v want dense=query-vector", result.Degraded)
	}
	waitForEmbeddingStatus(t, s, true, indexer.EmbeddingStatusVector)
	health, err := s.Index.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.EmbeddingDegradedReason != indexer.EmbeddingStatusVector {
		t.Fatalf("health reason=%q want %q", health.EmbeddingDegradedReason, indexer.EmbeddingStatusVector)
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
	docErr                error
	queryErr              error
	docCalls              int
	queryCalls            int
	docPayloads           []string
	queryPayloads         []string
}

func (b *recordingBackend) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	b.mu.Lock()
	b.docCalls++
	b.docPayloads = append(b.docPayloads, texts...)
	docErr := b.docErr
	docVector := append([]float32(nil), b.docVector...)
	blockUntilContextDone := b.blockUntilContextDone
	b.mu.Unlock()
	if blockUntilContextDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if docErr != nil {
		return nil, docErr
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = append([]float32(nil), docVector...)
	}
	return vectors, nil
}

func (b *recordingBackend) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	b.mu.Lock()
	b.queryCalls++
	b.queryPayloads = append(b.queryPayloads, text)
	queryErr := b.queryErr
	queryVector := append([]float32(nil), b.queryVector...)
	blockUntilContextDone := b.blockUntilContextDone
	b.mu.Unlock()
	if blockUntilContextDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if queryErr != nil {
		return nil, queryErr
	}
	return queryVector, nil
}

func (b *recordingBackend) docCallCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.docCalls
}

func (b *recordingBackend) setDocVector(vector []float32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.docVector = append([]float32(nil), vector...)
}

func (b *recordingBackend) setDocErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.docErr = err
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

type scriptedEmbedder struct {
	mu          sync.Mutex
	namespace   embedder.EmbeddingNamespace
	docFunc     func(context.Context, []string, int) (embedder.EmbeddingBatch, error)
	docCalls    int
	docPayloads []string
	query       embedder.Embedding
}

func (e *scriptedEmbedder) EmbedDocuments(ctx context.Context, texts []string) (embedder.EmbeddingBatch, error) {
	e.mu.Lock()
	e.docCalls++
	call := e.docCalls
	e.docPayloads = append(e.docPayloads, texts...)
	fn := e.docFunc
	ns := e.namespace
	e.mu.Unlock()
	copied := append([]string(nil), texts...)
	if fn != nil {
		return fn(ctx, copied, call)
	}
	return documentBatchForTexts(ns, copied, []float32{1, 0}), nil
}

func (e *scriptedEmbedder) EmbedQuery(_ context.Context, text string) (embedder.Embedding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	query := e.query
	if query.Namespace.ID == "" {
		query = embedder.Embedding{Namespace: e.namespace, Role: embedder.RoleQuery, Vector: []float32{1, 0}}
	}
	query.CanonicalText = text
	return query, nil
}

func (e *scriptedEmbedder) Namespace() embedder.EmbeddingNamespace {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.namespace
}

func (e *scriptedEmbedder) setDocFunc(fn func(context.Context, []string, int) (embedder.EmbeddingBatch, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.docFunc = fn
}

func (e *scriptedEmbedder) docCallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.docCalls
}

func documentBatchForTexts(ns embedder.EmbeddingNamespace, texts []string, vector []float32) embedder.EmbeddingBatch {
	embeddings := make([]embedder.Embedding, len(texts))
	for i, text := range texts {
		embeddings[i] = embedder.Embedding{
			Namespace:     ns,
			Role:          embedder.RoleDocument,
			Vector:        append([]float32(nil), vector...),
			CanonicalText: text,
		}
	}
	return embedder.EmbeddingBatch{
		Namespace:      ns,
		Role:           embedder.RoleDocument,
		Embeddings:     embeddings,
		CanonicalTexts: append([]string(nil), texts...),
	}
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

func requireWarmPageContaining(t *testing.T, s *Session, needle string) pages.WarmPage {
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
			t.Errorf("close warm page db: %v", err)
		}
	}()
	rows, err := db.QueryContext(context.Background(), `
SELECT page_id, page_version, session_id, source_digest, compiler_version, search_text
FROM warm_pages
WHERE session_id = ?`, s.Cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var page pages.WarmPage
		var version int64
		if err := rows.Scan(&page.ID, &version, &page.SessionID, &page.SourceDigest, &page.CompilerVersion, &page.SearchText); err != nil {
			t.Fatal(err)
		}
		page.Version = pages.PageVersion(version)
		if strings.Contains(page.SearchText, needle) {
			return page
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("warm page containing %q not found", needle)
	return pages.WarmPage{}
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

func waitForSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitForDocCallsStable(t *testing.T, emb *scriptedEmbedder, stableFor time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	last := emb.docCallCount()
	stableSince := time.Now()
	for {
		got := emb.docCallCount()
		if got != last {
			last = got
			stableSince = time.Now()
		}
		if got > 2 {
			return got
		}
		if time.Since(stableSince) >= stableFor {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("document calls did not stabilize within deadline; last=%d", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForRecordingDocCallsStable(t *testing.T, backend *recordingBackend, stableFor time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	last := backend.docCallCount()
	stableSince := time.Now()
	for {
		got := backend.docCallCount()
		if got != last {
			last = got
			stableSince = time.Now()
		}
		if got > 2 {
			return got
		}
		if time.Since(stableSince) >= stableFor {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("recording backend document calls did not stabilize within deadline; last=%d", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func missingPageContains(warmPages []pages.WarmPage, needle string) bool {
	for _, page := range warmPages {
		if strings.Contains(page.SearchText, needle) {
			return true
		}
	}
	return false
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
