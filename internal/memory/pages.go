package memory

import (
	"time"

	"wsms/internal/types"
)

// Page is a reusable memory chunk with residency metadata.
type Page struct {
	ID          string
	Tier        types.MemoryTier
	Summary     string
	Refs        []string
	Scope       types.Scope
	Branch      string
	Salience    float64
	AccessCount int
	LastAccess  time.Time
	CreatedAt   time.Time
	Stale       bool
	Invalidated bool
	UserPrio    float64 // 0..1
	Body        string  // optional exact content for L2 faults
}
