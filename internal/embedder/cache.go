package embedder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

const DefaultDocumentCacheCapacity = 4096

// DocumentCache is a deterministic, content-addressed cache for document
// embeddings. Query embeddings are intentionally not cached by this core.
type DocumentCache struct {
	mu       sync.RWMutex
	entries  map[string]cacheEntry
	flights  map[string]*cacheFlight
	capacity int
	now      func() time.Time
}

type cacheEntry struct {
	embedding Embedding
	createdAt time.Time
}

// CacheEntry is an inspectable, text-retaining cache row. Admission happens
// before entries are written, so this view should never contain denied text.
type CacheEntry struct {
	Key           string
	Namespace     string
	CanonicalText string
	Dimensions    int
	CreatedAt     time.Time
}

type cacheFlight struct {
	once      sync.Once
	done      chan struct{}
	embedding Embedding
	err       error
}

// NewDocumentCache returns an empty in-memory document cache.
func NewDocumentCache() *DocumentCache {
	return NewDocumentCacheWithCapacity(DefaultDocumentCacheCapacity)
}

// NewDocumentCacheWithCapacity returns a bounded in-memory document cache.
// A non-positive capacity disables storage but still permits miss singleflight.
func NewDocumentCacheWithCapacity(capacity int) *DocumentCache {
	return &DocumentCache{
		entries:  make(map[string]cacheEntry),
		flights:  make(map[string]*cacheFlight),
		capacity: capacity,
		now:      time.Now,
	}
}

func (c *DocumentCache) get(key string) (Embedding, bool) {
	if c == nil {
		return Embedding{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok {
		return Embedding{}, false
	}
	embedding := entry.embedding
	embedding.Vector = copyVector(embedding.Vector)
	return embedding, true
}

func (c *DocumentCache) getOrBegin(key string) (Embedding, bool, *cacheFlight, bool) {
	if c == nil {
		return Embedding{}, false, nil, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok {
		embedding := entry.embedding
		embedding.Vector = copyVector(embedding.Vector)
		return embedding, true, nil, false
	}
	if c.flights == nil {
		c.flights = make(map[string]*cacheFlight)
	}
	if flight, ok := c.flights[key]; ok {
		return Embedding{}, false, flight, false
	}
	flight := &cacheFlight{done: make(chan struct{})}
	c.flights[key] = flight
	return Embedding{}, false, flight, true
}

func (c *DocumentCache) finishFlight(key string, flight *cacheFlight, embedding Embedding, err error) {
	if c == nil || flight == nil {
		return
	}
	flight.once.Do(func() {
		c.mu.Lock()
		if current := c.flights[key]; current == flight {
			delete(c.flights, key)
		}
		if err == nil {
			c.putLocked(key, embedding)
		}
		flight.embedding = embedding
		flight.err = err
		close(flight.done)
		c.mu.Unlock()
	})
}

func (c *DocumentCache) waitFlight(ctx context.Context, flight *cacheFlight) (Embedding, error) {
	if flight == nil {
		return Embedding{}, nil
	}
	select {
	case <-flight.done:
		if flight.err != nil {
			return Embedding{}, flight.err
		}
		embedding := flight.embedding
		embedding.Vector = copyVector(embedding.Vector)
		return embedding, nil
	case <-ctx.Done():
		return Embedding{}, ctx.Err()
	}
}

func (c *DocumentCache) put(key string, embedding Embedding) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(key, embedding)
}

func (c *DocumentCache) putLocked(key string, embedding Embedding) {
	if c.capacity <= 0 {
		return
	}
	if c.entries == nil {
		c.entries = make(map[string]cacheEntry)
	}
	embedding.Vector = copyVector(embedding.Vector)
	embedding.CacheKey = key
	createdAt := c.now()
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	c.entries[key] = cacheEntry{embedding: embedding, createdAt: createdAt.UTC()}
	c.evictLocked()
}

func (c *DocumentCache) evictLocked() {
	if c.capacity <= 0 || len(c.entries) <= c.capacity {
		return
	}
	type candidate struct {
		key string
		at  time.Time
	}
	candidates := make([]candidate, 0, len(c.entries))
	for key, entry := range c.entries {
		candidates = append(candidates, candidate{key: key, at: entry.createdAt})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].at.Equal(candidates[j].at) {
			return candidates[i].key < candidates[j].key
		}
		return candidates[i].at.Before(candidates[j].at)
	})
	for len(c.entries) > c.capacity && len(candidates) > 0 {
		delete(c.entries, candidates[0].key)
		candidates = candidates[1:]
	}
}

// Len returns the number of cached document vectors.
func (c *DocumentCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Snapshot returns inspectable cache metadata and canonical text.
func (c *DocumentCache) Snapshot() []CacheEntry {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CacheEntry, 0, len(c.entries))
	for key, entry := range c.entries {
		out = append(out, CacheEntry{
			Key:           key,
			Namespace:     entry.embedding.Namespace.ID,
			CanonicalText: entry.embedding.CanonicalText,
			Dimensions:    len(entry.embedding.Vector),
			CreatedAt:     entry.createdAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out
}

func cacheKey(ns EmbeddingNamespace, canonicalText string) string {
	sum := sha256.Sum256([]byte(ns.ID + "\x00" + canonicalText))
	return "doc_" + hex.EncodeToString(sum[:])
}
