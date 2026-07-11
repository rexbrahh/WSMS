package embedder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrDegraded reports that dense embedding is unavailable but the caller can
	// continue on lexical/direct-fault paths.
	ErrDegraded = errors.New("embedder degraded")
	// ErrCircuitOpen reports a circuit-open backend suppression.
	ErrCircuitOpen = errors.New("embedder circuit open")
)

const (
	DefaultTimeout          = 2 * time.Second
	selfCheckDocumentText   = "wsms self check document page cache locality"
	selfCheckQueryText      = "wsms self check query page fault"
	selfCheckFailureReason  = "self_check_failed"
	selfCheckRequiredReason = "self_check_required"
)

// Backend is the supervised local inference seam. It is intentionally narrower
// than Embedder and receives already-admitted, role-specific payload text.
type Backend interface {
	EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// SelfChecker is an optional readiness extension. It intentionally does not
// change the documented three-method Embedder ABI.
type SelfChecker interface {
	SelfCheck(ctx context.Context) error
}

type backendSelfChecker interface {
	SelfCheck(ctx context.Context, namespace EmbeddingNamespace) error
}

// Options configures a Supervised embedder.
type Options struct {
	Namespace     EmbeddingNamespace
	Backend       Backend
	Cache         *DocumentCache
	Policy        AdmissionPolicy
	MaxBatchSize  int
	MaxDimensions int
	Timeout       time.Duration
	Breaker       BreakerOptions
	Now           func() time.Time
}

// BreakerOptions configures the local circuit breaker.
type BreakerOptions struct {
	FailureThreshold int
	Cooldown         time.Duration
}

// BreakerState is the inspectable circuit state.
type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half_open"
)

// Health is a redacted, inspectable embedder status snapshot.
type Health struct {
	Ready               bool
	Degraded            bool
	Reason              string
	Namespace           string
	Dimensions          int
	BreakerState        BreakerState
	ConsecutiveFailures int
	LastFailureAt       time.Time
	LastSuccessAt       time.Time
	Checked             bool
	LastCheckedAt       time.Time
	CacheEntries        int
	MaxBatchSize        int
	MaxDocumentBytes    int
	MaxQueryBytes       int
}

// Supervised is a production-shaped embedder wrapper around a local backend.
type Supervised struct {
	namespace     EmbeddingNamespace
	backend       Backend
	cache         *DocumentCache
	policy        AdmissionPolicy
	maxBatchSize  int
	maxDimensions int
	timeout       time.Duration
	now           func() time.Time

	mu                  sync.Mutex
	breaker             BreakerOptions
	breakerState        BreakerState
	halfOpenProbe       bool
	consecutiveFailures int
	lastFailureAt       time.Time
	lastSuccessAt       time.Time
	checked             bool
	lastCheckedAt       time.Time
	reason              string
}

var _ Embedder = (*Supervised)(nil)

// New returns a supervised embedder for one canonical namespace.
func New(opts Options) (*Supervised, error) {
	if err := opts.Namespace.Validate(); err != nil {
		return nil, err
	}
	if opts.Backend == nil {
		return nil, fmt.Errorf("embedder backend is required")
	}
	maxBatch := opts.MaxBatchSize
	if maxBatch <= 0 {
		maxBatch = DefaultMaxBatchSize
	}
	maxDims := opts.MaxDimensions
	if maxDims <= 0 {
		maxDims = DefaultMaxDimensions
	}
	if opts.Namespace.Profile.Dimensions > maxDims {
		return nil, fmt.Errorf("%w: dimensions %d exceeds max %d", ErrInvalidNamespace, opts.Namespace.Profile.Dimensions, maxDims)
	}
	policy := opts.Policy.withDefaults()
	cache := opts.Cache
	if cache == nil {
		cache = NewDocumentCache()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	breaker := opts.Breaker
	if breaker.FailureThreshold <= 0 {
		breaker.FailureThreshold = 3
	}
	if breaker.Cooldown <= 0 {
		breaker.Cooldown = 30 * time.Second
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Supervised{
		namespace:     opts.Namespace,
		backend:       opts.Backend,
		cache:         cache,
		policy:        policy,
		maxBatchSize:  maxBatch,
		maxDimensions: maxDims,
		timeout:       timeout,
		now:           now,
		breaker:       breaker,
		breakerState:  BreakerClosed,
	}, nil
}

// Namespace returns the canonical embedding namespace.
func (e *Supervised) Namespace() EmbeddingNamespace {
	if e == nil {
		return EmbeddingNamespace{}
	}
	return e.namespace
}

// EmbedDocuments embeds an ordered batch of document search text.
func (e *Supervised) EmbedDocuments(ctx context.Context, texts []string) (EmbeddingBatch, error) {
	if e == nil {
		return EmbeddingBatch{}, fmt.Errorf("%w: nil embedder", ErrDegraded)
	}
	if err := ctx.Err(); err != nil {
		return EmbeddingBatch{}, err
	}
	if len(texts) > e.maxBatchSize {
		return EmbeddingBatch{}, fmt.Errorf("%w: got %d max %d", ErrBatchTooLarge, len(texts), e.maxBatchSize)
	}
	result := EmbeddingBatch{
		Namespace:      e.namespace,
		Role:           RoleDocument,
		Embeddings:     make([]Embedding, len(texts)),
		CanonicalTexts: make([]string, len(texts)),
	}
	type miss struct {
		key       string
		canonical string
		payload   string
		indexes   []int
		flight    *cacheFlight
	}
	missesByKey := make(map[string]*miss)
	var misses []*miss
	for i, text := range texts {
		canonical, err := e.policy.canonicalDocument(text)
		if err != nil {
			return EmbeddingBatch{}, err
		}
		result.CanonicalTexts[i] = canonical
		key := cacheKey(e.namespace, canonical)
		if cached, ok := e.cache.get(key); ok {
			if err := cached.Validate(e.namespace, RoleDocument); err != nil {
				return EmbeddingBatch{}, err
			}
			result.Embeddings[i] = cached
			result.CacheHits++
			continue
		}
		if existing := missesByKey[key]; existing != nil {
			existing.indexes = append(existing.indexes, i)
			continue
		}
		payload := documentPayload(e.namespace.Profile, canonical)
		if err := e.policy.validatePayload(payload, RoleDocument); err != nil {
			return EmbeddingBatch{}, err
		}
		m := &miss{
			key:       key,
			canonical: canonical,
			payload:   payload,
			indexes:   []int{i},
		}
		missesByKey[key] = m
		misses = append(misses, m)
	}
	leaders := make([]*miss, 0, len(misses))
	for _, miss := range misses {
		cached, ok, flight, leader := e.cache.getOrBegin(miss.key)
		switch {
		case ok:
			if err := cached.Validate(e.namespace, RoleDocument); err != nil {
				return EmbeddingBatch{}, err
			}
			for _, index := range miss.indexes {
				result.Embeddings[index] = cached
			}
			result.CacheHits += len(miss.indexes)
		case !leader:
			embedding, err := e.cache.waitFlight(ctx, flight)
			if err != nil {
				return EmbeddingBatch{}, err
			}
			if err := embedding.Validate(e.namespace, RoleDocument); err != nil {
				return EmbeddingBatch{}, err
			}
			for _, index := range miss.indexes {
				resultEmbedding := embedding
				resultEmbedding.Vector = copyVector(embedding.Vector)
				result.Embeddings[index] = resultEmbedding
			}
			result.CacheHits += len(miss.indexes)
		default:
			miss.indexes = append([]int(nil), miss.indexes...)
			miss.flight = flight
			leaders = append(leaders, miss)
		}
	}
	if len(leaders) == 0 {
		return result, result.Validate(e.namespace, len(texts))
	}
	result.CacheMisses = len(leaders)
	if err := e.allowBackend(); err != nil {
		for _, miss := range leaders {
			e.cache.finishFlight(miss.key, miss.flight, Embedding{}, err)
		}
		return EmbeddingBatch{}, err
	}
	backendTexts := make([]string, len(leaders))
	for i, miss := range leaders {
		backendTexts[i] = miss.payload
	}
	callCtx, cancel := e.callContext(ctx)
	defer cancel()
	vectors, err := e.backend.EmbedDocuments(callCtx, backendTexts)
	if err != nil {
		e.recordFailure(classifyFailure(callCtx, err))
		err = degradeError(callCtx, err)
		for _, miss := range leaders {
			e.cache.finishFlight(miss.key, miss.flight, Embedding{}, err)
		}
		return EmbeddingBatch{}, err
	}
	if err := callCtx.Err(); err != nil {
		e.recordFailure(classifyFailure(callCtx, err))
		err = degradeError(callCtx, err)
		for _, miss := range leaders {
			e.cache.finishFlight(miss.key, miss.flight, Embedding{}, err)
		}
		return EmbeddingBatch{}, err
	}
	if len(vectors) != len(leaders) {
		e.recordFailure("malformed_batch")
		err := fmt.Errorf("%w: backend returned %d vectors for %d documents", ErrInvalidEmbedding, len(vectors), len(leaders))
		for _, miss := range leaders {
			e.cache.finishFlight(miss.key, miss.flight, Embedding{}, err)
		}
		return EmbeddingBatch{}, err
	}
	embeddings := make([]Embedding, len(leaders))
	for i, vector := range vectors {
		embedding := Embedding{
			Namespace:     e.namespace,
			Role:          RoleDocument,
			Vector:        copyVector(vector),
			CanonicalText: leaders[i].canonical,
			CacheKey:      leaders[i].key,
		}
		if err := embedding.Validate(e.namespace, RoleDocument); err != nil {
			e.recordFailure("malformed_vector")
			for _, miss := range leaders {
				e.cache.finishFlight(miss.key, miss.flight, Embedding{}, err)
			}
			return EmbeddingBatch{}, err
		}
		embeddings[i] = embedding
	}
	for i, embedding := range embeddings {
		miss := leaders[i]
		e.cache.finishFlight(miss.key, miss.flight, embedding, nil)
		for _, index := range miss.indexes {
			resultEmbedding := embedding
			resultEmbedding.Vector = copyVector(embedding.Vector)
			result.Embeddings[index] = resultEmbedding
		}
	}
	e.recordSuccess()
	return result, result.Validate(e.namespace, len(texts))
}

// EmbedQuery embeds one query using the query instruction from the namespace.
func (e *Supervised) EmbedQuery(ctx context.Context, text string) (Embedding, error) {
	if e == nil {
		return Embedding{}, fmt.Errorf("%w: nil embedder", ErrDegraded)
	}
	if err := ctx.Err(); err != nil {
		return Embedding{}, err
	}
	canonical, err := e.policy.canonicalQuery(text)
	if err != nil {
		return Embedding{}, err
	}
	payload := queryPayload(e.namespace.Profile, canonical)
	if err := e.policy.validatePayload(payload, RoleQuery); err != nil {
		return Embedding{}, err
	}
	if err := e.allowBackend(); err != nil {
		return Embedding{}, err
	}
	callCtx, cancel := e.callContext(ctx)
	defer cancel()
	vector, err := e.backend.EmbedQuery(callCtx, payload)
	if err != nil {
		e.recordFailure(classifyFailure(callCtx, err))
		return Embedding{}, degradeError(callCtx, err)
	}
	if err := callCtx.Err(); err != nil {
		e.recordFailure(classifyFailure(callCtx, err))
		return Embedding{}, degradeError(callCtx, err)
	}
	embedding := Embedding{
		Namespace:     e.namespace,
		Role:          RoleQuery,
		Vector:        copyVector(vector),
		CanonicalText: canonical,
	}
	if err := embedding.Validate(e.namespace, RoleQuery); err != nil {
		e.recordFailure("malformed_vector")
		return Embedding{}, err
	}
	e.recordSuccess()
	return embedding, nil
}

// Health returns a redacted health snapshot.
func (e *Supervised) Health(ctx context.Context) Health {
	if e == nil {
		return Health{Degraded: true, Reason: "nil_embedder", BreakerState: BreakerOpen}
	}
	_ = ctx
	e.mu.Lock()
	defer e.mu.Unlock()
	h := Health{
		Namespace:           e.namespace.ID,
		Dimensions:          e.namespace.Profile.Dimensions,
		BreakerState:        e.breakerState,
		ConsecutiveFailures: e.consecutiveFailures,
		LastFailureAt:       e.lastFailureAt,
		LastSuccessAt:       e.lastSuccessAt,
		Checked:             e.checked,
		LastCheckedAt:       e.lastCheckedAt,
		Reason:              e.reason,
		CacheEntries:        e.cache.Len(),
		MaxBatchSize:        e.maxBatchSize,
		MaxDocumentBytes:    e.policy.withDefaults().MaxDocumentBytes,
		MaxQueryBytes:       e.policy.withDefaults().MaxQueryBytes,
	}
	if !h.Checked && h.Reason == "" {
		h.Reason = selfCheckRequiredReason
	}
	h.Degraded = h.BreakerState == BreakerOpen || h.Reason != "" || !h.Checked
	h.Ready = h.Checked && !h.Degraded
	return h
}

// SelfCheck performs the supervised readiness probe without changing the
// public embedding ABI. It optionally asks the backend to verify its loaded
// profile, then runs fixed document and query probes through the role-specific
// payload contracts.
func (e *Supervised) SelfCheck(ctx context.Context) error {
	if e == nil {
		return fmt.Errorf("%w: nil embedder", ErrDegraded)
	}
	if err := ctx.Err(); err != nil {
		e.recordSelfCheckFailure(classifyFailure(ctx, err))
		return err
	}
	documentProbe := documentPayload(e.namespace.Profile, selfCheckDocumentText)
	if err := e.policy.validatePayload(documentProbe, RoleDocument); err != nil {
		e.recordSelfCheckFailure(selfCheckFailureReason)
		return err
	}
	queryProbe := queryPayload(e.namespace.Profile, selfCheckQueryText)
	if err := e.policy.validatePayload(queryProbe, RoleQuery); err != nil {
		e.recordSelfCheckFailure(selfCheckFailureReason)
		return err
	}
	if err := e.allowBackend(); err != nil {
		return err
	}
	callCtx, cancel := e.callContext(ctx)
	defer cancel()
	if checker, ok := e.backend.(backendSelfChecker); ok {
		if err := checker.SelfCheck(callCtx, e.namespace); err != nil {
			reason := selfCheckFailureReason
			if callCtx.Err() != nil {
				reason = classifyFailure(callCtx, err)
				err = degradeError(callCtx, err)
			} else {
				err = fmt.Errorf("%w: backend profile self-check failed", ErrDegraded)
			}
			e.recordSelfCheckFailure(reason)
			return err
		}
	}
	documentVectors, err := e.backend.EmbedDocuments(callCtx, []string{documentProbe})
	if err != nil {
		e.recordSelfCheckFailure(classifyFailure(callCtx, err))
		return degradeError(callCtx, err)
	}
	if err := callCtx.Err(); err != nil {
		e.recordSelfCheckFailure(classifyFailure(callCtx, err))
		return degradeError(callCtx, err)
	}
	if len(documentVectors) != 1 {
		err := fmt.Errorf("%w: self-check document batch returned %d vectors", ErrInvalidEmbedding, len(documentVectors))
		e.recordSelfCheckFailure("malformed_batch")
		return err
	}
	document := Embedding{
		Namespace:     e.namespace,
		Role:          RoleDocument,
		Vector:        copyVector(documentVectors[0]),
		CanonicalText: selfCheckDocumentText,
		CacheKey:      cacheKey(e.namespace, selfCheckDocumentText),
	}
	if err := document.Validate(e.namespace, RoleDocument); err != nil {
		e.recordSelfCheckFailure("malformed_vector")
		return err
	}
	queryVector, err := e.backend.EmbedQuery(callCtx, queryProbe)
	if err != nil {
		e.recordSelfCheckFailure(classifyFailure(callCtx, err))
		return degradeError(callCtx, err)
	}
	if err := callCtx.Err(); err != nil {
		e.recordSelfCheckFailure(classifyFailure(callCtx, err))
		return degradeError(callCtx, err)
	}
	query := Embedding{
		Namespace:     e.namespace,
		Role:          RoleQuery,
		Vector:        copyVector(queryVector),
		CanonicalText: selfCheckQueryText,
	}
	if err := query.Validate(e.namespace, RoleQuery); err != nil {
		e.recordSelfCheckFailure("malformed_vector")
		return err
	}
	e.recordSelfCheckSuccess()
	return nil
}

func (e *Supervised) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if e.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, e.timeout)
}

func (e *Supervised) allowBackend() error {
	now := e.now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.breakerState == BreakerOpen {
		if now.Sub(e.lastFailureAt) < e.breaker.Cooldown {
			e.reason = "circuit_open"
			return fmt.Errorf("%w: %w", ErrDegraded, ErrCircuitOpen)
		}
		if e.halfOpenProbe {
			e.reason = "half_open_probe"
			return fmt.Errorf("%w: %w", ErrDegraded, ErrCircuitOpen)
		}
		e.breakerState = BreakerHalfOpen
		e.halfOpenProbe = true
		e.reason = "half_open_probe"
		return nil
	}
	if e.breakerState == BreakerHalfOpen {
		if e.halfOpenProbe {
			e.reason = "half_open_probe"
			return fmt.Errorf("%w: %w", ErrDegraded, ErrCircuitOpen)
		}
		e.halfOpenProbe = true
		e.reason = "half_open_probe"
	}
	return nil
}

func (e *Supervised) recordSuccess() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consecutiveFailures = 0
	e.breakerState = BreakerClosed
	e.halfOpenProbe = false
	e.lastSuccessAt = e.now().UTC()
	e.reason = ""
}

func (e *Supervised) recordFailure(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consecutiveFailures++
	e.lastFailureAt = e.now().UTC()
	e.reason = reason
	e.halfOpenProbe = false
	if e.consecutiveFailures >= e.breaker.FailureThreshold {
		e.breakerState = BreakerOpen
	}
}

func (e *Supervised) recordSelfCheckSuccess() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checked = true
	e.lastCheckedAt = e.now().UTC()
	e.consecutiveFailures = 0
	e.breakerState = BreakerClosed
	e.halfOpenProbe = false
	e.lastSuccessAt = e.lastCheckedAt
	e.reason = ""
}

func (e *Supervised) recordSelfCheckFailure(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checked = true
	e.lastCheckedAt = e.now().UTC()
	e.consecutiveFailures++
	e.lastFailureAt = e.lastCheckedAt
	e.reason = reason
	e.halfOpenProbe = false
	if e.consecutiveFailures >= e.breaker.FailureThreshold {
		e.breakerState = BreakerOpen
	}
}

func classifyFailure(ctx context.Context, err error) string {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout_or_cancelled"
	}
	return "backend_error"
}

func degradeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", ErrDegraded, ctx.Err())
	}
	return fmt.Errorf("%w: backend unavailable", ErrDegraded)
}
