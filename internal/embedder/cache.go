package embedder

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// DocumentCache is a deterministic, content-addressed cache for document
// embeddings. Query embeddings are intentionally not cached by this core.
type DocumentCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
	now     func() time.Time
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

// NewDocumentCache returns an empty in-memory document cache.
func NewDocumentCache() *DocumentCache {
	return &DocumentCache{entries: make(map[string]cacheEntry), now: time.Now}
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

func (c *DocumentCache) put(key string, embedding Embedding) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
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
	return out
}

func cacheKey(ns EmbeddingNamespace, canonicalText string) string {
	sum := sha256.Sum256([]byte(ns.ID + "\x00" + canonicalText))
	return "doc_" + hex.EncodeToString(sum[:])
}
