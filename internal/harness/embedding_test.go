package harness

import (
	"context"
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
	if s.EmbeddingErr == nil {
		t.Fatal("missing embedding degradation")
	}

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
	s.embedIndexMutations(ctx, muts)
	if s.EmbeddingErr != nil {
		t.Fatalf("embedding error: %v", s.EmbeddingErr)
	}
	if got := backend.docCallCount(); got != 1 {
		t.Fatalf("backend document calls=%d want 1", got)
	}
	if got := len(backend.documentPayloads()); got != 1 {
		t.Fatalf("backend payload count=%d want one cache miss payload", got)
	}
	if got := cache.Len(); got != 1 {
		t.Fatalf("cache entries=%d want 1", got)
	}
	s.embedIndexMutations(ctx, muts)
	if got := backend.docCallCount(); got != 1 {
		t.Fatalf("cached re-embed called backend %d times", got)
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
	s.embedIndexMutations(ctx, muts)
	if !errors.Is(s.EmbeddingErr, embedder.ErrInvalidEmbedding) {
		t.Fatalf("EmbeddingErr=%v want ErrInvalidEmbedding", s.EmbeddingErr)
	}
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
	s.embedIndexMutations(ctx, muts)
	if !errors.Is(s.EmbeddingErr, embedder.ErrInvalidEmbedding) {
		t.Fatalf("EmbeddingErr=%v want ErrInvalidEmbedding", s.EmbeddingErr)
	}
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

func openEmbeddingSession(t *testing.T, id string, emb embedder.Embedder, denseDims int) *Session {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = id
	cfg.ArtifactThresholdBytes = 32
	cfg.CapsuleTokenBudget = 1024
	cfg.PageFaultTokenBudget = 4096
	cfg.DenseDimensions = denseDims
	cfg.Embedder = emb
	cfg.EmbeddingBatchSize = 4
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	return s
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

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
