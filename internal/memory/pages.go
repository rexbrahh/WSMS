package memory

import (
	"time"

	"wsms/internal/types"
)

// Page is a reusable memory chunk with residency metadata.
type Page struct {
	ID                 string
	PageVersion        uint64
	SessionID          string
	CompilerVersion    string
	EmbeddingNamespace string
	Tier               types.MemoryTier
	Summary            string
	Refs               []string
	Scope              types.Scope
	Branch             string
	Commit             string
	Paths              []string
	SourceDigest       string
	ScopeEpoch         uint64
	StaleRevision      uint64
	Salience           float64
	AccessCount        int
	LastAccess         time.Time
	CreatedAt          time.Time
	Stale              bool
	Invalidated        bool
	UserPrio           float64 // 0..1
	Body               string  // optional exact content for L2 faults
}

// PageTuple identifies the exact authoritative L4 page instance represented by
// a resident, ghost, or shadow entry. Page IDs alone are not enough after an L4
// rewrite because vectors and residency metadata are rebuildable estimates.
type PageTuple struct {
	ID              string
	PageVersion     uint64
	SessionID       string
	SourceDigest    string
	CompilerVersion string
	ScopeEpoch      uint64
}

// Tuple returns the exact authoritative identity for p.
func (p *Page) Tuple() PageTuple {
	if p == nil {
		return PageTuple{}
	}
	return PageTuple{
		ID:              p.ID,
		PageVersion:     p.PageVersion,
		SessionID:       p.SessionID,
		SourceDigest:    p.SourceDigest,
		CompilerVersion: p.CompilerVersion,
		ScopeEpoch:      p.ScopeEpoch,
	}
}

func (t PageTuple) matches(p *Page) bool {
	return p != nil && t == p.Tuple()
}
