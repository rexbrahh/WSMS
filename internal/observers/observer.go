// Package observers extracts WSL updates from ledger events.
package observers

import (
	"context"
	"sync"

	"wsms/internal/ledger"
	"wsms/internal/wsl"
)

// Observer converts events into WSL updates.
type Observer interface {
	Name() string
	Handle(ctx context.Context, ev ledger.Event) ([]wsl.Update, error)
}

// IDGen allocates new WSL ids (C1, F1, …).
type IDGen interface {
	Next(prefix string) string
}

// IDSnapshot is a point-in-time copy of a sequence allocator's counters.
// Restoring a snapshot lets scheduler derivation roll ID allocation back when
// an observer or atomic WSL batch rejects an event.
type IDSnapshot map[string]int

// IDAllocator is a sequence generator that supports transactional checkpoints.
type IDAllocator interface {
	IDGen
	Snapshot() IDSnapshot
	Restore(IDSnapshot)
}

// SeqIDGen is a simple monotonic id generator.
type SeqIDGen struct {
	mu sync.Mutex
	n  map[string]int
}

// NewSeqIDGen creates a generator.
func NewSeqIDGen() *SeqIDGen {
	return &SeqIDGen{n: map[string]int{}}
}

// Next returns PREFIX + number (e.g. F1, C1).
func (g *SeqIDGen) Next(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n == nil {
		g.n = map[string]int{}
	}
	g.n[prefix]++
	return prefix + itoa(g.n[prefix])
}

// Snapshot returns an independent copy of the current prefix counters.
func (g *SeqIDGen) Snapshot() IDSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(IDSnapshot, len(g.n))
	for prefix, n := range g.n {
		out[prefix] = n
	}
	return out
}

// Restore replaces the allocator counters with an independent snapshot copy.
func (g *SeqIDGen) Restore(snapshot IDSnapshot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n = make(map[string]int, len(snapshot))
	for prefix, n := range snapshot {
		g.n[prefix] = n
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
