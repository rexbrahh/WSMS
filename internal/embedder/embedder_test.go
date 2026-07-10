package embedder

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNamespaceCoversNormativeIdentityFields(t *testing.T) {
	base := testProfile()
	ns := MustNamespace(base)
	if err := ns.Validate(); err != nil {
		t.Fatalf("namespace validate: %v", err)
	}
	tests := []struct {
		name string
		edit func(*NamespaceProfile)
	}{
		{"provider", func(p *NamespaceProfile) { p.Provider = "hosted" }},
		{"model_repository", func(p *NamespaceProfile) { p.ModelRepository = "other/model" }},
		{"exact_revision", func(p *NamespaceProfile) { p.ExactRevision = "rev-2" }},
		{"dimensions", func(p *NamespaceProfile) { p.Dimensions = 4 }},
		{"distance_metric", func(p *NamespaceProfile) { p.DistanceMetric = MetricCosine }},
		{"normalization", func(p *NamespaceProfile) { p.Normalization = NormalizationL2 }},
		{"query_instruction", func(p *NamespaceProfile) { p.QueryInstruction = "different query instruction" }},
		{"document_template", func(p *NamespaceProfile) { p.DocumentTemplate = "doc: {{.SearchText}}" }},
		{"tokenizer_revision", func(p *NamespaceProfile) { p.TokenizerRevision = "tok-2" }},
		{"page_schema_version", func(p *NamespaceProfile) { p.PageSchemaVersion = "pages/v-next" }},
		{"redaction_version", func(p *NamespaceProfile) { p.RedactionVersion = "redaction/v-next" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := base
			if tt.name == "distance_metric" {
				profile.DistanceMetric = "dot"
			} else if tt.name == "normalization" {
				profile.Normalization = "none"
			} else {
				tt.edit(&profile)
			}
			next, err := NewNamespace(profile)
			if tt.name == "distance_metric" || tt.name == "normalization" {
				if !errors.Is(err, ErrInvalidNamespace) {
					t.Fatalf("expected invalid namespace for unsupported %s, got %v", tt.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("new namespace: %v", err)
			}
			if next.ID == ns.ID {
				t.Fatalf("%s did not change namespace id %q", tt.name, ns.ID)
			}
		})
	}
	tampered := ns
	tampered.ID = "emb_not_the_digest"
	if err := tampered.Validate(); !errors.Is(err, ErrInvalidNamespace) {
		t.Fatalf("expected invalid tampered namespace, got %v", err)
	}
}

func TestEmbedDocumentsAndQueryAreDistinctAndCacheDocuments(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	cache := NewDocumentCache()
	emb, err := New(Options{Namespace: ns, Backend: backend, Cache: cache})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	ctx := context.Background()
	batch, err := emb.EmbedDocuments(ctx, []string{
		"  verify current command   before editing ",
		"verify current command before editing",
	})
	if err != nil {
		t.Fatalf("embed documents: %v", err)
	}
	if err := batch.Validate(ns, 2); err != nil {
		t.Fatalf("validate batch: %v", err)
	}
	if batch.CacheHits != 0 || batch.CacheMisses != 1 {
		t.Fatalf("cache stats = hits %d misses %d, want 0/1", batch.CacheHits, batch.CacheMisses)
	}
	if got := backend.docCalls(); got != 1 {
		t.Fatalf("backend document calls = %d, want 1", got)
	}
	if got := backend.lastDocumentPayload(); got != "doc<verify current command before editing>" {
		t.Fatalf("document payload = %q", got)
	}
	if len(cache.Snapshot()) != 1 || cache.Snapshot()[0].CanonicalText != "verify current command before editing" {
		t.Fatalf("unexpected cache snapshot: %#v", cache.Snapshot())
	}
	batch.Embeddings[0].Vector[0] = 999
	if batch.Embeddings[1].Vector[0] == 999 {
		t.Fatalf("duplicate result vectors alias each other")
	}
	again, err := emb.EmbedDocuments(ctx, []string{"verify current command before editing"})
	if err != nil {
		t.Fatalf("embed cached document: %v", err)
	}
	if again.CacheHits != 1 || again.CacheMisses != 0 {
		t.Fatalf("cached stats = hits %d misses %d, want 1/0", again.CacheHits, again.CacheMisses)
	}
	if again.Embeddings[0].Vector[0] == 999 {
		t.Fatalf("cached vector was mutated through caller slice")
	}
	query, err := emb.EmbedQuery(ctx, "verify current command")
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	if err := query.Validate(ns, RoleQuery); err != nil {
		t.Fatalf("validate query: %v", err)
	}
	if err := query.Validate(ns, RoleDocument); !errors.Is(err, ErrInvalidEmbedding) {
		t.Fatalf("expected role inversion failure, got %v", err)
	}
	if got := backend.lastQueryPayload(); got != "query instruction\nverify current command" {
		t.Fatalf("query payload = %q", got)
	}
	if _, err := emb.EmbedQuery(ctx, "verify current command"); err != nil {
		t.Fatalf("embed query second time: %v", err)
	}
	if got := backend.queryCalls(); got != 2 {
		t.Fatalf("query should not use document cache, calls = %d", got)
	}
}

func TestNamespaceAndDimensionMismatchesFail(t *testing.T) {
	ns := MustNamespace(testProfile())
	otherProfile := testProfile()
	otherProfile.ExactRevision = "other-revision"
	other := MustNamespace(otherProfile)
	embedding := Embedding{
		Namespace:     other,
		Role:          RoleDocument,
		Vector:        []float32{1, 2, 3},
		CanonicalText: "text",
	}
	if err := embedding.Validate(ns, RoleDocument); !errors.Is(err, ErrInvalidEmbedding) {
		t.Fatalf("expected namespace mismatch failure, got %v", err)
	}
	embedding.Namespace = ns
	embedding.Vector = []float32{1, 2}
	if err := embedding.Validate(ns, RoleDocument); !errors.Is(err, ErrInvalidEmbedding) {
		t.Fatalf("expected dimension mismatch failure, got %v", err)
	}
	if _, err := New(Options{Namespace: ns, Backend: newTestingBackend(3), MaxDimensions: 2}); !errors.Is(err, ErrInvalidNamespace) {
		t.Fatalf("expected namespace dimension bound failure, got %v", err)
	}
}

func TestMalformedVectorsFailVisibly(t *testing.T) {
	ns := MustNamespace(testProfile())
	tests := []struct {
		name   string
		vector []float32
	}{
		{"wrong_dimension", []float32{1, 2}},
		{"zero", []float32{0, 0, 0}},
		{"nan", []float32{1, float32(math.NaN()), 3}},
		{"inf", []float32{1, float32(math.Inf(1)), 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newTestingBackend(ns.Profile.Dimensions)
			backend.setDocumentVectors([][]float32{tt.vector})
			emb, err := New(Options{Namespace: ns, Backend: backend})
			if err != nil {
				t.Fatalf("new embedder: %v", err)
			}
			_, err = emb.EmbedDocuments(context.Background(), []string{"safe text"})
			if !errors.Is(err, ErrInvalidEmbedding) {
				t.Fatalf("expected invalid embedding, got %v", err)
			}
			h := emb.Health(context.Background())
			if !h.Degraded || h.Reason != "malformed_vector" {
				t.Fatalf("health = %#v, want malformed degraded", h)
			}
		})
	}
}

func TestAdmissionDeniesSecretsDeniedPathsTranscriptsAndArtifacts(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	emb, err := New(Options{Namespace: ns, Backend: backend})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	longBase64 := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo", 20)
	tests := []string{
		"OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwxyz123456",
		"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----",
		"path: .env\nDATABASE_URL=postgres://example",
		"user: hi\nassistant: hello\nuser: show me secrets",
		"content-transfer-encoding: base64\n" + longBase64,
	}
	for _, text := range tests {
		if _, err := emb.EmbedDocuments(context.Background(), []string{text}); !errors.Is(err, ErrAdmissionDenied) {
			t.Fatalf("expected admission denied for %q, got %v", text, err)
		}
	}
	if got := backend.docCalls(); got != 0 {
		t.Fatalf("backend document calls = %d, want 0", got)
	}
	if got := emb.Health(context.Background()).CacheEntries; got != 0 {
		t.Fatalf("cache entries = %d, want 0", got)
	}
	if _, err := emb.EmbedQuery(context.Background(), "password = hunter12345"); !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("expected query admission denied, got %v", err)
	}
}

func TestBoundsCancellationTimeoutCircuitAndCacheOnlyDegradedBehavior(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	emb, err := New(Options{
		Namespace:    ns,
		Backend:      backend,
		MaxBatchSize: 1,
		Timeout:      10 * time.Millisecond,
		Breaker:      BreakerOptions{FailureThreshold: 1, Cooldown: time.Hour},
	})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	if _, err := emb.EmbedDocuments(context.Background(), []string{"one", "two"}); !errors.Is(err, ErrBatchTooLarge) {
		t.Fatalf("expected batch bound failure, got %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := emb.EmbedQuery(cancelled, "safe query"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}

	if _, err := emb.EmbedDocuments(context.Background(), []string{"cacheable page"}); err != nil {
		t.Fatalf("prime document cache: %v", err)
	}
	backend.setDelay(250 * time.Millisecond)
	if _, err := emb.EmbedQuery(context.Background(), "query that times out"); !errors.Is(err, ErrDegraded) {
		t.Fatalf("expected degraded timeout, got %v", err)
	}
	health := emb.Health(context.Background())
	if !health.Degraded || health.BreakerState != BreakerOpen || health.Reason != "timeout_or_cancelled" {
		t.Fatalf("health after timeout = %#v", health)
	}
	if _, err := emb.EmbedQuery(context.Background(), "another query"); !errors.Is(err, ErrCircuitOpen) || !errors.Is(err, ErrDegraded) {
		t.Fatalf("expected circuit-open degraded error, got %v", err)
	}
	if batch, err := emb.EmbedDocuments(context.Background(), []string{"cacheable page"}); err != nil {
		t.Fatalf("cache-only document batch should survive open circuit: %v", err)
	} else if batch.CacheHits != 1 || batch.CacheMisses != 0 {
		t.Fatalf("cache-only stats = hits %d misses %d", batch.CacheHits, batch.CacheMisses)
	}
}

func TestNamespaceChangeDoesNotReuseCache(t *testing.T) {
	profile := testProfile()
	ns1 := MustNamespace(profile)
	profile.RedactionVersion = "redaction/v0.2.0"
	ns2 := MustNamespace(profile)
	cache := NewDocumentCache()
	b1 := newTestingBackend(ns1.Profile.Dimensions)
	e1, err := New(Options{Namespace: ns1, Backend: b1, Cache: cache})
	if err != nil {
		t.Fatalf("new embedder 1: %v", err)
	}
	if _, err := e1.EmbedDocuments(context.Background(), []string{"same canonical text"}); err != nil {
		t.Fatalf("embed namespace 1: %v", err)
	}
	b2 := newTestingBackend(ns2.Profile.Dimensions)
	e2, err := New(Options{Namespace: ns2, Backend: b2, Cache: cache})
	if err != nil {
		t.Fatalf("new embedder 2: %v", err)
	}
	batch, err := e2.EmbedDocuments(context.Background(), []string{"same canonical text"})
	if err != nil {
		t.Fatalf("embed namespace 2: %v", err)
	}
	if batch.CacheHits != 0 || batch.CacheMisses != 1 {
		t.Fatalf("namespace changed but cache reused: hits %d misses %d", batch.CacheHits, batch.CacheMisses)
	}
	if got := b2.docCalls(); got != 1 {
		t.Fatalf("backend 2 calls = %d, want 1", got)
	}
	if got := len(cache.Snapshot()); got != 2 {
		t.Fatalf("cache entries = %d, want one per namespace", got)
	}
}

func TestConcurrentEmbedDocumentsQueryHealthAndCache(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	emb, err := New(Options{Namespace: ns, Backend: backend})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			text := fmt.Sprintf("safe working-set page %d", i%4)
			if _, err := emb.EmbedDocuments(context.Background(), []string{text, text}); err != nil {
				t.Errorf("embed documents: %v", err)
			}
			if _, err := emb.EmbedQuery(context.Background(), fmt.Sprintf("safe query %d", i)); err != nil {
				t.Errorf("embed query: %v", err)
			}
			_ = emb.Health(context.Background())
			_ = emb.cache.Snapshot()
		}(i)
	}
	wg.Wait()
	if got := emb.Health(context.Background()).CacheEntries; got != 4 {
		t.Fatalf("cache entries = %d, want 4 canonical documents", got)
	}
}

func testProfile() NamespaceProfile {
	return NamespaceProfile{
		Provider:          "local",
		ModelRepository:   "test/model",
		ExactRevision:     "rev-1",
		Dimensions:        3,
		DistanceMetric:    MetricCosine,
		Normalization:     NormalizationL2,
		QueryInstruction:  "query instruction",
		DocumentTemplate:  "doc<{{.SearchText}}>",
		TokenizerRevision: "tok-1",
		PageSchemaVersion: "pages/v-test",
		RedactionVersion:  CurrentRedactionVersion,
	}
}

type testingBackend struct {
	mu              sync.Mutex
	dims            int
	documentCallCnt int
	queryCallCnt    int
	documentPayload []string
	queryPayload    []string
	documentVectors [][]float32
	queryVector     []float32
	err             error
	delay           time.Duration
}

func newTestingBackend(dims int) *testingBackend {
	return &testingBackend{dims: dims}
}

func (b *testingBackend) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	b.documentCallCnt++
	b.documentPayload = append(b.documentPayload, texts...)
	if b.documentVectors != nil {
		out := make([][]float32, len(b.documentVectors))
		for i := range b.documentVectors {
			out[i] = copyVector(b.documentVectors[i])
		}
		return out, nil
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = vectorFromText(text, b.dims)
	}
	return out, nil
}

func (b *testingBackend) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	b.queryCallCnt++
	b.queryPayload = append(b.queryPayload, text)
	if b.queryVector != nil {
		return copyVector(b.queryVector), nil
	}
	return vectorFromText(text, b.dims), nil
}

func (b *testingBackend) wait(ctx context.Context) error {
	b.mu.Lock()
	delay := b.delay
	b.mu.Unlock()
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *testingBackend) setDocumentVectors(vectors [][]float32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.documentVectors = vectors
}

func (b *testingBackend) setDelay(delay time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.delay = delay
}

func (b *testingBackend) docCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.documentCallCnt
}

func (b *testingBackend) queryCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.queryCallCnt
}

func (b *testingBackend) lastDocumentPayload() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.documentPayload) == 0 {
		return ""
	}
	return b.documentPayload[len(b.documentPayload)-1]
}

func (b *testingBackend) lastQueryPayload() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.queryPayload) == 0 {
		return ""
	}
	return b.queryPayload[len(b.queryPayload)-1]
}

func vectorFromText(text string, dims int) []float32 {
	sum := sha256.Sum256([]byte(text))
	out := make([]float32, dims)
	for i := range out {
		out[i] = float32(int(sum[i%len(sum)])+1) / 255
	}
	return out
}
