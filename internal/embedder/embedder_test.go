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

func TestQwen3ProfileUsesOfficialInstructedQuerySerialization(t *testing.T) {
	profile := Qwen3Embedding06BProfile("model-rev", "tokenizer-rev", "pages/v1", CurrentRedactionVersion)
	const wantPrefix = "Instruct: Represent this query for retrieving relevant WSMS working-state pages.\nQuery:"
	if profile.QueryInstruction != wantPrefix {
		t.Fatalf("Qwen query prefix = %q, want %q", profile.QueryInstruction, wantPrefix)
	}
	if got, want := queryPayload(profile, "working set eviction"), wantPrefix+" working set eviction"; got != want {
		t.Fatalf("Qwen query payload = %q, want %q", got, want)
	}
	if got := documentPayload(profile, "working set page"); got != "working set page" {
		t.Fatalf("Qwen document payload = %q, want unprefixed document", got)
	}

	base := MustNamespace(profile)
	changed := profile
	changed.QueryInstruction = strings.Replace(changed.QueryInstruction, "working-state", "resident-set", 1)
	if other := MustNamespace(changed); other.ID == base.ID {
		t.Fatalf("serialized Qwen query prefix did not change namespace id %q", base.ID)
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

func TestDocumentPayloadAdmissionPrecedesFlightCreation(t *testing.T) {
	profile := testProfile()
	profile.DocumentTemplate = "password={{.SearchText}}"
	ns := MustNamespace(profile)
	backend := newTestingBackend(ns.Profile.Dimensions)
	cache := NewDocumentCache()
	emb, err := New(Options{Namespace: ns, Backend: backend, Cache: cache})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}

	if _, err := emb.EmbedDocuments(context.Background(), []string{"short", "longvalue"}); !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("mixed payload admission error = %v, want admission denied", err)
	}
	if got := backend.docCalls(); got != 0 {
		t.Fatalf("backend document calls = %d, want 0", got)
	}
	if got := flightCount(cache); got != 0 {
		t.Fatalf("cache flights = %d, want 0 after admission rejection", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	batch, err := emb.EmbedDocuments(ctx, []string{"short"})
	if err != nil {
		t.Fatalf("subsequent safe first key blocked by leaked flight: %v", err)
	}
	if batch.CacheHits != 0 || batch.CacheMisses != 1 {
		t.Fatalf("subsequent stats = hits %d misses %d, want 0/1", batch.CacheHits, batch.CacheMisses)
	}
	if got := flightCount(cache); got != 0 {
		t.Fatalf("cache flights = %d, want 0 after recovery", got)
	}
}

func TestRenderedQueryPayloadIsAdmittedBeforeBackend(t *testing.T) {
	profile := testProfile()
	profile.QueryInstruction = "sensitive-marker"
	ns := MustNamespace(profile)
	backend := newTestingBackend(ns.Profile.Dimensions)
	emb, err := New(Options{Namespace: ns, Backend: backend})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}

	if _, err := emb.EmbedQuery(context.Background(), "safe query text"); !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("rendered query admission error = %v, want admission denied", err)
	}
	if got := backend.queryCalls(); got != 0 {
		t.Fatalf("backend query calls = %d, want 0", got)
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

func TestDocumentBatchUsesLeadersWhenMissBecomesWaiter(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	cache := NewDocumentCache()
	emb, err := New(Options{Namespace: ns, Backend: backend, Cache: cache})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}

	externalCanonical, err := emb.policy.canonicalDocument("external leader")
	if err != nil {
		t.Fatalf("canonical external: %v", err)
	}
	externalKey := cacheKey(ns, externalCanonical)
	_, _, externalFlight, leader := cache.getOrBegin(externalKey)
	if !leader {
		t.Fatalf("expected to own external flight")
	}

	done := make(chan struct {
		batch EmbeddingBatch
		err   error
	}, 1)
	go func() {
		batch, err := emb.EmbedDocuments(context.Background(), []string{"external leader", "local leader"})
		done <- struct {
			batch EmbeddingBatch
			err   error
		}{batch: batch, err: err}
	}()

	waitForFlightCount(t, cache, 1)
	external := Embedding{
		Namespace:     ns,
		Role:          RoleDocument,
		Vector:        vectorFromText("external", ns.Profile.Dimensions),
		CanonicalText: externalCanonical,
		CacheKey:      externalKey,
	}
	cache.finishFlight(externalKey, externalFlight, external, nil)

	got := waitBatchResult(t, done)
	if got.err != nil {
		t.Fatalf("embed documents: %v", got.err)
	}
	if err := got.batch.Validate(ns, 2); err != nil {
		t.Fatalf("validate batch: %v", err)
	}
	if got.batch.CacheHits != 1 || got.batch.CacheMisses != 1 {
		t.Fatalf("stats = hits %d misses %d, want 1/1", got.batch.CacheHits, got.batch.CacheMisses)
	}
	if got := backend.docCalls(); got != 1 {
		t.Fatalf("backend document calls = %d, want only the local leader", got)
	}
	if got.batch.Embeddings[0].CanonicalText != externalCanonical {
		t.Fatalf("external waiter not placed at index 0: %#v", got.batch.Embeddings[0])
	}
	if got := flightCount(cache); got != 0 {
		t.Fatalf("cache flights = %d, want 0", got)
	}
}

func TestDuplicateFollowerCacheAccountingIsExact(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	cache := NewDocumentCache()
	emb, err := New(Options{Namespace: ns, Backend: backend, Cache: cache})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	canonical, err := emb.policy.canonicalDocument("shared follower")
	if err != nil {
		t.Fatalf("canonical follower: %v", err)
	}
	key := cacheKey(ns, canonical)
	_, _, externalFlight, leader := cache.getOrBegin(key)
	if !leader {
		t.Fatalf("expected to own external flight")
	}
	claimed := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFollower := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseFollower()
	emb.flightClaimed = func(claimedKey string) {
		if claimedKey == key {
			claimed <- struct{}{}
			<-release
		}
	}
	done := make(chan struct {
		batch EmbeddingBatch
		err   error
	}, 1)
	go func() {
		batch, err := emb.EmbedDocuments(context.Background(), []string{"shared follower", "shared follower"})
		done <- struct {
			batch EmbeddingBatch
			err   error
		}{batch: batch, err: err}
	}()
	waitSignal(t, claimed, "duplicate follower claim")
	external := Embedding{
		Namespace:     ns,
		Role:          RoleDocument,
		Vector:        vectorFromText("shared", ns.Profile.Dimensions),
		CanonicalText: canonical,
		CacheKey:      key,
	}
	cache.finishFlight(key, externalFlight, external, nil)
	releaseFollower()
	got := waitBatchResult(t, done)
	if got.err != nil {
		t.Fatalf("duplicate follower batch: %v", got.err)
	}
	if got.batch.CacheHits != 2 || got.batch.CacheMisses != 0 {
		t.Fatalf("duplicate follower stats = hits %d misses %d, want exact 2/0", got.batch.CacheHits, got.batch.CacheMisses)
	}
	if got := backend.docCalls(); got != 0 {
		t.Fatalf("backend document calls = %d, want 0 for followers", got)
	}
	if got := flightCount(cache); got != 0 {
		t.Fatalf("cache flights = %d, want 0", got)
	}
}

func TestReversedDocumentBatchesCompleteLeadersBeforeWaiting(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	cache := NewDocumentCache()
	emb, err := New(Options{Namespace: ns, Backend: backend, Cache: cache})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	canonicalA, err := emb.policy.canonicalDocument("page alpha")
	if err != nil {
		t.Fatalf("canonical alpha: %v", err)
	}
	canonicalB, err := emb.policy.canonicalDocument("page beta")
	if err != nil {
		t.Fatalf("canonical beta: %v", err)
	}
	keyA := cacheKey(ns, canonicalA)
	keyB := cacheKey(ns, canonicalB)

	claimed := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseClaims := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseClaims()
	var onceA sync.Once
	var onceB sync.Once
	emb.flightClaimed = func(key string) {
		var once *sync.Once
		switch key {
		case keyA:
			once = &onceA
		case keyB:
			once = &onceB
		default:
			return
		}
		once.Do(func() {
			claimed <- key
			<-release
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	doneAB := make(chan struct {
		batch EmbeddingBatch
		err   error
	}, 1)
	doneBA := make(chan struct {
		batch EmbeddingBatch
		err   error
	}, 1)
	go func() {
		batch, err := emb.EmbedDocuments(ctx, []string{"page alpha", "page beta"})
		doneAB <- struct {
			batch EmbeddingBatch
			err   error
		}{batch: batch, err: err}
	}()
	go func() {
		batch, err := emb.EmbedDocuments(ctx, []string{"page beta", "page alpha"})
		doneBA <- struct {
			batch EmbeddingBatch
			err   error
		}{batch: batch, err: err}
	}()

	firstClaims := make(map[string]bool, 2)
	for len(firstClaims) < 2 {
		select {
		case key := <-claimed:
			firstClaims[key] = true
		case <-time.After(750 * time.Millisecond):
			t.Fatalf("timed out waiting for reversed first-key claims: %#v", firstClaims)
		}
	}
	if !firstClaims[keyA] || !firstClaims[keyB] {
		t.Fatalf("first claims = %#v, want alpha and beta", firstClaims)
	}
	releaseClaims()

	for name, got := range map[string]struct {
		batch EmbeddingBatch
		err   error
	}{
		"alpha_beta": waitBatchResult(t, doneAB),
		"beta_alpha": waitBatchResult(t, doneBA),
	} {
		if got.err != nil {
			t.Fatalf("%s batch deadlocked or failed: %v", name, got.err)
		}
		if err := got.batch.Validate(ns, 2); err != nil {
			t.Fatalf("%s batch validation: %v", name, err)
		}
		if got.batch.CacheHits != 1 || got.batch.CacheMisses != 1 {
			t.Fatalf("%s stats = hits %d misses %d, want exact 1/1", name, got.batch.CacheHits, got.batch.CacheMisses)
		}
	}
	if got := backend.docCalls(); got != 2 {
		t.Fatalf("backend document calls = %d, want one owned-leader call per batch", got)
	}
	if got := flightCount(cache); got != 0 {
		t.Fatalf("cache flights = %d, want 0", got)
	}
}

func TestPostClaimCacheValidationFailureFinishesOwnedLeaders(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	cache := NewDocumentCache()
	emb, err := New(Options{Namespace: ns, Backend: backend, Cache: cache})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	leaderCanonical, err := emb.policy.canonicalDocument("owned leader")
	if err != nil {
		t.Fatalf("canonical leader: %v", err)
	}
	invalidCanonical, err := emb.policy.canonicalDocument("racing cache hit")
	if err != nil {
		t.Fatalf("canonical invalid hit: %v", err)
	}
	leaderKey := cacheKey(ns, leaderCanonical)
	invalidKey := cacheKey(ns, invalidCanonical)
	var inject sync.Once
	emb.flightClaimed = func(key string) {
		if key != leaderKey {
			return
		}
		inject.Do(func() {
			cache.put(invalidKey, Embedding{
				Namespace:     ns,
				Role:          RoleDocument,
				Vector:        []float32{1, 2},
				CanonicalText: invalidCanonical,
				CacheKey:      invalidKey,
			})
		})
	}

	if _, err := emb.EmbedDocuments(context.Background(), []string{"owned leader", "racing cache hit"}); !errors.Is(err, ErrInvalidEmbedding) {
		t.Fatalf("post-claim cache validation error = %v, want invalid embedding", err)
	}
	if got := backend.docCalls(); got != 0 {
		t.Fatalf("backend document calls = %d, want 0 before cache validation", got)
	}
	if got := flightCount(cache); got != 0 {
		t.Fatalf("cache flights = %d, want owned leader completed on validation exit", got)
	}

	emb.flightClaimed = nil
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := emb.EmbedDocuments(ctx, []string{"owned leader"}); err != nil {
		t.Fatalf("owned leader remained blocked after validation exit: %v", err)
	}
}

func TestDocumentBatchFinishesLeadersBeforeWaitingOnFollowers(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	cache := NewDocumentCache()
	emb, err := New(Options{Namespace: ns, Backend: backend, Cache: cache})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}

	externalCanonical, err := emb.policy.canonicalDocument("external follower")
	if err != nil {
		t.Fatalf("canonical external: %v", err)
	}
	externalKey := cacheKey(ns, externalCanonical)
	_, _, externalFlight, leader := cache.getOrBegin(externalKey)
	if !leader {
		t.Fatalf("expected to own external flight")
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	backend.setDocumentGate(entered, release)

	done := make(chan struct {
		batch EmbeddingBatch
		err   error
	}, 1)
	go func() {
		batch, err := emb.EmbedDocuments(context.Background(), []string{"local leader", "external follower"})
		done <- struct {
			batch EmbeddingBatch
			err   error
		}{batch: batch, err: err}
	}()

	waitSignal(t, entered, "local leader backend call before external follower resolves")
	close(release)
	external := Embedding{
		Namespace:     ns,
		Role:          RoleDocument,
		Vector:        vectorFromText("external", ns.Profile.Dimensions),
		CanonicalText: externalCanonical,
		CacheKey:      externalKey,
	}
	cache.finishFlight(externalKey, externalFlight, external, nil)
	got := waitBatchResult(t, done)
	if got.err != nil {
		t.Fatalf("embed documents: %v", got.err)
	}
	if err := got.batch.Validate(ns, 2); err != nil {
		t.Fatalf("validate batch: %v", err)
	}
	if got.batch.CacheHits != 1 || got.batch.CacheMisses != 1 {
		t.Fatalf("stats = hits %d misses %d, want 1/1", got.batch.CacheHits, got.batch.CacheMisses)
	}
	if got := backend.docCalls(); got != 1 {
		t.Fatalf("backend document calls = %d, want one local leader", got)
	}
	if got := flightCount(cache); got != 0 {
		t.Fatalf("cache flights = %d, want 0", got)
	}
}

func TestDocumentFlightsCompleteForMixedHitsWaitersAndAllLeaderExitBranches(t *testing.T) {
	ns := MustNamespace(testProfile())
	tests := []struct {
		name      string
		configure func(*testingBackend)
		timeout   time.Duration
		release   bool
		wantErr   error
	}{
		{
			name:    "success",
			release: true,
		},
		{
			name:    "backend_error",
			release: true,
			configure: func(b *testingBackend) {
				b.setErr(errors.New("backend down"))
			},
			wantErr: ErrDegraded,
		},
		{
			name:    "batch_mismatch",
			release: true,
			configure: func(b *testingBackend) {
				b.setDocumentVectors([][]float32{})
			},
			wantErr: ErrInvalidEmbedding,
		},
		{
			name:    "vector_validation",
			release: true,
			configure: func(b *testingBackend) {
				b.setDocumentVectors([][]float32{
					vectorFromText("one", b.dims),
					{1, 2},
				})
			},
			wantErr: ErrInvalidEmbedding,
		},
		{
			name:    "context_timeout",
			timeout: 20 * time.Millisecond,
			wantErr: ErrDegraded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newTestingBackend(ns.Profile.Dimensions)
			if tt.configure != nil {
				tt.configure(backend)
			}
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			backend.setDocumentGate(entered, release)
			cache := NewDocumentCache()
			opts := Options{Namespace: ns, Backend: backend, Cache: cache}
			if tt.timeout > 0 {
				opts.Timeout = tt.timeout
			}
			emb, err := New(opts)
			if err != nil {
				t.Fatalf("new embedder: %v", err)
			}
			putCachedDocument(t, emb, cache, "cached page")

			leaderDone := make(chan struct {
				batch EmbeddingBatch
				err   error
			}, 1)
			go func() {
				batch, err := emb.EmbedDocuments(context.Background(), []string{"cached page", "leader one", "leader two"})
				leaderDone <- struct {
					batch EmbeddingBatch
					err   error
				}{batch: batch, err: err}
			}()

			waitSignal(t, entered, "document backend entry")
			waitForFlightCount(t, cache, 2)

			waiterStarted := make(chan struct{})
			waiterDone := make(chan error, 1)
			go func() {
				close(waiterStarted)
				_, err := emb.EmbedDocuments(context.Background(), []string{"leader one"})
				waiterDone <- err
			}()
			waitSignal(t, waiterStarted, "waiter start")
			if tt.release {
				close(release)
			}

			leader := waitBatchResult(t, leaderDone)
			waiterErr := waitErrorResult(t, waiterDone)
			if tt.wantErr == nil {
				if leader.err != nil {
					t.Fatalf("leader error: %v", leader.err)
				}
				if waiterErr != nil {
					t.Fatalf("waiter error: %v", waiterErr)
				}
				if leader.batch.CacheHits != 1 || leader.batch.CacheMisses != 2 {
					t.Fatalf("leader stats = hits %d misses %d, want 1/2", leader.batch.CacheHits, leader.batch.CacheMisses)
				}
				if got := len(cache.Snapshot()); got != 3 {
					t.Fatalf("cache entries = %d, want cached hit plus two leaders", got)
				}
			} else {
				if !errors.Is(leader.err, tt.wantErr) {
					t.Fatalf("leader error = %v, want %v", leader.err, tt.wantErr)
				}
				if !errors.Is(waiterErr, tt.wantErr) {
					t.Fatalf("waiter error = %v, want %v", waiterErr, tt.wantErr)
				}
				if got := len(cache.Snapshot()); got != 1 {
					t.Fatalf("cache entries = %d, want only pre-existing hit", got)
				}
			}
			if !tt.release {
				close(release)
			}
			if got := flightCount(cache); got != 0 {
				t.Fatalf("cache flights = %d, want 0", got)
			}
		})
	}
}

func TestDocumentFlightsCompleteWhenBreakerRejectsLeaders(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	emb, err := New(Options{
		Namespace: ns,
		Backend:   backend,
		Breaker:   BreakerOptions{FailureThreshold: 1, Cooldown: time.Hour},
	})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	backend.setErr(errors.New("backend down"))
	if _, err := emb.EmbedQuery(context.Background(), "open circuit"); !errors.Is(err, ErrDegraded) {
		t.Fatalf("open circuit query error = %v, want degraded", err)
	}
	backend.setErr(nil)
	if _, err := emb.EmbedDocuments(context.Background(), []string{"uncached rejected page"}); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("breaker rejected document error = %v, want circuit open", err)
	}
	if got := flightCount(emb.cache); got != 0 {
		t.Fatalf("cache flights = %d, want 0 after breaker rejection", got)
	}
}

func TestSelfCheckSuccessMarksHealthReady(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	emb, err := New(Options{Namespace: ns, Backend: backend})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	if emb.timeout <= 0 {
		t.Fatalf("default timeout = %v, want non-zero", emb.timeout)
	}
	initial := emb.Health(context.Background())
	if initial.Ready || !initial.Degraded || initial.Checked || initial.Reason != selfCheckRequiredReason {
		t.Fatalf("initial health = %#v, want self-check-required degraded", initial)
	}
	if err := emb.SelfCheck(context.Background()); err != nil {
		t.Fatalf("self check: %v", err)
	}
	health := emb.Health(context.Background())
	if !health.Ready || health.Degraded || !health.Checked || health.LastCheckedAt.IsZero() || health.Reason != "" {
		t.Fatalf("health after self-check = %#v, want ready", health)
	}
	if got := backend.selfCheckCalls(); got != 1 {
		t.Fatalf("backend profile checks = %d, want 1", got)
	}
	if got := backend.docCalls(); got != 1 {
		t.Fatalf("document probes = %d, want 1", got)
	}
	if got := backend.queryCalls(); got != 1 {
		t.Fatalf("query probes = %d, want 1", got)
	}
	if got, want := backend.lastDocumentPayload(), documentPayload(ns.Profile, selfCheckDocumentText); got != want {
		t.Fatalf("document self-check payload = %q, want %q", got, want)
	}
	if got, want := backend.lastQueryPayload(), queryPayload(ns.Profile, selfCheckQueryText); got != want {
		t.Fatalf("query self-check payload = %q, want %q", got, want)
	}
}

func TestSelfCheckAdmitsAllRenderedPayloadsBeforeBackend(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*NamespaceProfile)
	}{
		{
			name: "document_template",
			configure: func(profile *NamespaceProfile) {
				profile.DocumentTemplate = "sensitive-marker {{.SearchText}}"
			},
		},
		{
			name: "query_instruction",
			configure: func(profile *NamespaceProfile) {
				profile.QueryInstruction = "sensitive-marker"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := testProfile()
			tt.configure(&profile)
			ns := MustNamespace(profile)
			backend := newTestingBackend(ns.Profile.Dimensions)
			emb, err := New(Options{Namespace: ns, Backend: backend})
			if err != nil {
				t.Fatalf("new embedder: %v", err)
			}

			if err := emb.SelfCheck(context.Background()); !errors.Is(err, ErrAdmissionDenied) {
				t.Fatalf("self-check admission error = %v, want admission denied", err)
			}
			if got := backend.selfCheckCalls(); got != 0 {
				t.Fatalf("backend profile checks = %d, want 0", got)
			}
			if got := backend.docCalls(); got != 0 {
				t.Fatalf("backend document probes = %d, want 0", got)
			}
			if got := backend.queryCalls(); got != 0 {
				t.Fatalf("backend query probes = %d, want 0", got)
			}
			health := emb.Health(context.Background())
			if health.Ready || !health.Degraded || !health.Checked || health.Reason != selfCheckFailureReason {
				t.Fatalf("health after rejected self-check = %#v", health)
			}
		})
	}
}

func TestSelfCheckProfileFailureStaysCategorizedDegraded(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	backend.setSelfCheckErr(errors.New("wrong model revision TOP_SECRET_SELF_CHECK"))
	emb, err := New(Options{Namespace: ns, Backend: backend})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	if err := emb.SelfCheck(context.Background()); !errors.Is(err, ErrDegraded) {
		t.Fatalf("self-check error = %v, want degraded", err)
	} else if strings.Contains(err.Error(), "TOP_SECRET_SELF_CHECK") || strings.Contains(err.Error(), "wrong model revision") {
		t.Fatalf("self-check returned raw backend detail: %v", err)
	}
	health := emb.Health(context.Background())
	if health.Ready || !health.Degraded || !health.Checked || health.Reason != selfCheckFailureReason || health.LastCheckedAt.IsZero() {
		t.Fatalf("health after profile failure = %#v", health)
	}
	if got := backend.docCalls(); got != 0 {
		t.Fatalf("document probes = %d, want 0 after profile failure", got)
	}
	if got := backend.queryCalls(); got != 0 {
		t.Fatalf("query probes = %d, want 0 after profile failure", got)
	}
}

func TestBackendErrorsPreserveOnlySafeCategories(t *testing.T) {
	type operation struct {
		name string
		call func(*Supervised) error
	}
	operations := []operation{
		{
			name: "documents",
			call: func(emb *Supervised) error {
				_, err := emb.EmbedDocuments(context.Background(), []string{"safe document"})
				return err
			},
		},
		{
			name: "query",
			call: func(emb *Supervised) error {
				_, err := emb.EmbedQuery(context.Background(), "safe query")
				return err
			},
		},
		{
			name: "self_check",
			call: func(emb *Supervised) error {
				return emb.SelfCheck(context.Background())
			},
		},
	}
	tests := []struct {
		name       string
		backendErr error
		want       error
		wantReason string
	}{
		{
			name:       "invalid_embedding",
			backendErr: fmt.Errorf("%w: TOP_SECRET invalid vector detail", ErrInvalidEmbedding),
			want:       ErrInvalidEmbedding,
			wantReason: invalidEmbeddingReason,
		},
		{
			name:       "invalid_namespace",
			backendErr: fmt.Errorf("%w: TOP_SECRET namespace detail", ErrInvalidNamespace),
			want:       ErrInvalidNamespace,
			wantReason: invalidNamespaceReason,
		},
		{
			name:       "admission_denied",
			backendErr: fmt.Errorf("%w: TOP_SECRET policy detail", ErrAdmissionDenied),
			want:       ErrAdmissionDenied,
			wantReason: admissionDeniedReason,
		},
		{
			name:       "availability",
			backendErr: errors.New("TOP_SECRET backend availability detail"),
			wantReason: backendFailureReason,
		},
	}

	for _, op := range operations {
		for _, tt := range tests {
			t.Run(op.name+"/"+tt.name, func(t *testing.T) {
				ns := MustNamespace(testProfile())
				backend := newTestingBackend(ns.Profile.Dimensions)
				if op.name == "self_check" {
					backend.setSelfCheckErr(tt.backendErr)
				} else {
					backend.setErr(tt.backendErr)
				}
				emb, err := New(Options{Namespace: ns, Backend: backend})
				if err != nil {
					t.Fatalf("new embedder: %v", err)
				}

				err = op.call(emb)
				if !errors.Is(err, ErrDegraded) {
					t.Fatalf("error = %v, want degraded", err)
				}
				if tt.want != nil && !errors.Is(err, tt.want) {
					t.Fatalf("error = %v, want safe sentinel %v", err, tt.want)
				}
				for _, sentinel := range []error{ErrInvalidEmbedding, ErrInvalidNamespace, ErrAdmissionDenied} {
					if sentinel != tt.want && errors.Is(err, sentinel) {
						t.Fatalf("error = %v, unexpectedly retained sentinel %v", err, sentinel)
					}
				}
				if strings.Contains(err.Error(), "TOP_SECRET") || strings.Contains(err.Error(), "detail") {
					t.Fatalf("error exposed raw backend detail: %v", err)
				}
				wantReason := tt.wantReason
				if op.name == "self_check" && tt.want == nil {
					wantReason = selfCheckFailureReason
				}
				health := emb.Health(context.Background())
				if !health.Degraded || health.Reason != wantReason {
					t.Fatalf("health = %#v, want stable reason %q", health, wantReason)
				}
			})
		}
	}
}

func TestSelfCheckTimeoutStaysCategorizedDegraded(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	backend.setDelay(100 * time.Millisecond)
	emb, err := New(Options{
		Namespace: ns,
		Backend:   backend,
		Timeout:   10 * time.Millisecond,
		Breaker:   BreakerOptions{FailureThreshold: 1, Cooldown: time.Hour},
	})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	if err := emb.SelfCheck(context.Background()); !errors.Is(err, ErrDegraded) {
		t.Fatalf("self-check error = %v, want degraded timeout", err)
	}
	health := emb.Health(context.Background())
	if health.Ready || !health.Degraded || !health.Checked || health.Reason != "timeout_or_cancelled" || health.BreakerState != BreakerOpen {
		t.Fatalf("health after timeout = %#v", health)
	}
}

func TestHalfOpenAllowsExactlyOneConcurrentProbe(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	now := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
	emb, err := New(Options{
		Namespace: ns,
		Backend:   backend,
		Breaker:   BreakerOptions{FailureThreshold: 1, Cooldown: time.Second},
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	backend.setErr(errors.New("backend down"))
	if _, err := emb.EmbedQuery(context.Background(), "open circuit"); !errors.Is(err, ErrDegraded) {
		t.Fatalf("open circuit query error = %v, want degraded", err)
	}
	backend.setErr(nil)
	now = now.Add(2 * time.Second)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	backend.setQueryGate(entered, release)

	probeDone := make(chan error, 1)
	go func() {
		_, err := emb.EmbedQuery(context.Background(), "half open probe")
		probeDone <- err
	}()
	waitSignal(t, entered, "half-open backend probe")

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := emb.EmbedQuery(context.Background(), fmt.Sprintf("concurrent half-open %d", i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrCircuitOpen) || !errors.Is(err, ErrDegraded) {
			t.Fatalf("concurrent half-open error = %v, want circuit-open degraded", err)
		}
	}
	close(release)
	if err := waitErrorResult(t, probeDone); err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if got := backend.queryCalls(); got != 1 {
		t.Fatalf("backend query probes = %d, want 1", got)
	}
}

func TestHalfOpenProbeSlotIsReleasedAfterFailure(t *testing.T) {
	ns := MustNamespace(testProfile())
	backend := newTestingBackend(ns.Profile.Dimensions)
	now := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
	emb, err := New(Options{
		Namespace: ns,
		Backend:   backend,
		Breaker:   BreakerOptions{FailureThreshold: 1, Cooldown: time.Second},
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new embedder: %v", err)
	}
	backend.setErr(errors.New("backend down"))
	if _, err := emb.EmbedQuery(context.Background(), "open circuit"); !errors.Is(err, ErrDegraded) {
		t.Fatalf("open circuit query error = %v, want degraded", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := emb.EmbedQuery(context.Background(), "failed half-open"); !errors.Is(err, ErrDegraded) {
		t.Fatalf("failed half-open error = %v, want degraded", err)
	}
	backend.setErr(nil)
	now = now.Add(2 * time.Second)
	if _, err := emb.EmbedQuery(context.Background(), "second half-open"); err != nil {
		t.Fatalf("second half-open should acquire released probe slot: %v", err)
	}
}

func putCachedDocument(t *testing.T, emb *Supervised, cache *DocumentCache, text string) {
	t.Helper()
	canonical, err := emb.policy.canonicalDocument(text)
	if err != nil {
		t.Fatalf("canonical cached document: %v", err)
	}
	key := cacheKey(emb.namespace, canonical)
	cache.put(key, Embedding{
		Namespace:     emb.namespace,
		Role:          RoleDocument,
		Vector:        vectorFromText("cached:"+canonical, emb.namespace.Profile.Dimensions),
		CanonicalText: canonical,
		CacheKey:      key,
	})
}

func flightCount(cache *DocumentCache) int {
	if cache == nil {
		return 0
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return len(cache.flights)
}

func waitForFlightCount(t *testing.T, cache *DocumentCache, want int) {
	t.Helper()
	deadline := time.After(500 * time.Millisecond)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if got := flightCount(cache); got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("cache flights did not reach %d; got %d", want, flightCount(cache))
		case <-tick.C:
		}
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitBatchResult(t *testing.T, ch <-chan struct {
	batch EmbeddingBatch
	err   error
}) struct {
	batch EmbeddingBatch
	err   error
} {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(750 * time.Millisecond):
		t.Fatalf("timed out waiting for embedding batch")
		return struct {
			batch EmbeddingBatch
			err   error
		}{}
	}
}

func waitErrorResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(750 * time.Millisecond):
		t.Fatalf("timed out waiting for error result")
		return nil
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
	mu               sync.Mutex
	dims             int
	documentCallCnt  int
	queryCallCnt     int
	selfCheckCallCnt int
	documentPayload  []string
	queryPayload     []string
	documentVectors  [][]float32
	queryVector      []float32
	err              error
	selfCheckErr     error
	delay            time.Duration
	selfCheckDelay   time.Duration
	documentEntered  chan struct{}
	documentGate     <-chan struct{}
	queryEntered     chan struct{}
	queryGate        <-chan struct{}
}

func newTestingBackend(dims int) *testingBackend {
	return &testingBackend{dims: dims}
}

func (b *testingBackend) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	if err := b.waitDocumentGate(ctx); err != nil {
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
	if err := b.waitQueryGate(ctx); err != nil {
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

func (b *testingBackend) SelfCheck(ctx context.Context, namespace EmbeddingNamespace) error {
	b.mu.Lock()
	delay := b.selfCheckDelay
	b.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.selfCheckCallCnt++
	if b.selfCheckErr != nil {
		return b.selfCheckErr
	}
	if namespace.ID == "" {
		return errors.New("empty namespace")
	}
	return nil
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

func (b *testingBackend) setErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

func (b *testingBackend) setSelfCheckErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.selfCheckErr = err
}

func (b *testingBackend) setDelay(delay time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.delay = delay
}

func (b *testingBackend) setDocumentGate(entered chan struct{}, gate <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.documentEntered = entered
	b.documentGate = gate
}

func (b *testingBackend) setQueryGate(entered chan struct{}, gate <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queryEntered = entered
	b.queryGate = gate
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

func (b *testingBackend) selfCheckCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.selfCheckCallCnt
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

func (b *testingBackend) waitDocumentGate(ctx context.Context) error {
	b.mu.Lock()
	entered := b.documentEntered
	gate := b.documentGate
	b.mu.Unlock()
	return waitBackendGate(ctx, entered, gate)
}

func (b *testingBackend) waitQueryGate(ctx context.Context) error {
	b.mu.Lock()
	entered := b.queryEntered
	gate := b.queryGate
	b.mu.Unlock()
	return waitBackendGate(ctx, entered, gate)
}

func waitBackendGate(ctx context.Context, entered chan struct{}, gate <-chan struct{}) error {
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate == nil {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gate:
		return nil
	}
}

func vectorFromText(text string, dims int) []float32 {
	sum := sha256.Sum256([]byte(text))
	out := make([]float32, dims)
	var norm float64
	for i := range out {
		out[i] = float32(int(sum[i%len(sum)])+1) / 255
		norm += float64(out[i] * out[i])
	}
	if norm == 0 {
		return out
	}
	scale := float32(math.Sqrt(norm))
	for i := range out {
		out[i] /= scale
	}
	return out
}
