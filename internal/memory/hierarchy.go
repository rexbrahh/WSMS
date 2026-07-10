// Package memory implements L0–L4 residency containers.
package memory

import (
	"sync"

	"wsms/internal/types"
)

// Hierarchy holds semantic memory tiers.
// Coherence: write-through for truth; L1–L3 are derived caches.
type Hierarchy struct {
	mu     sync.RWMutex
	l0     map[string]any // turn scratch
	L1Text string         // last rendered capsule
	l2     map[string]*Page
	l3     map[string]*Page // interface-only in scaffold
	// L4 is external: ledger + artifacts
}

// NewHierarchy creates empty tiers.
func NewHierarchy() *Hierarchy {
	return &Hierarchy{
		l0: map[string]any{},
		l2: map[string]*Page{},
		l3: map[string]*Page{},
	}
}

// ClearL0 resets turn scratch.
func (h *Hierarchy) ClearL0() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.l0 = map[string]any{}
}

// SetL0 sets a scratch key.
func (h *Hierarchy) SetL0(k string, v any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.l0[k] = v
}

// SetL1Capsule stores the active rendered capsule.
func (h *Hierarchy) SetL1Capsule(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.L1Text = text
}

// L1Capsule returns the active capsule.
func (h *Hierarchy) L1Capsule() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.L1Text
}

// PutL2 inserts/updates a hot page.
func (h *Hierarchy) PutL2(p *Page) {
	if p == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := clonePage(p)
	cp.Tier = types.TierL2Hot
	delete(h.l3, cp.ID)
	h.l2[cp.ID] = cp
}

// PutL3 inserts/updates a warm page and removes any hot copy of the same ID.
func (h *Hierarchy) PutL3(p *Page) {
	if p == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := clonePage(p)
	cp.Tier = types.TierL3Warm
	delete(h.l2, cp.ID)
	h.l3[cp.ID] = cp
}

// GetPage looks up L2 then L3.
func (h *Hierarchy) GetPage(id string) (*Page, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if p, ok := h.l2[id]; ok {
		return clonePage(p), true
	}
	if p, ok := h.l3[id]; ok {
		return clonePage(p), true
	}
	return nil, false
}

func clonePage(p *Page) *Page {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Refs != nil {
		cp.Refs = append([]string{}, p.Refs...)
	}
	return &cp
}

// RecordAccess bumps access stats if page is resident.
func (h *Hierarchy) RecordAccess(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.l2[id]; ok {
		p.AccessCount++
		return
	}
	if p, ok := h.l3[id]; ok {
		p.AccessCount++
	}
}
