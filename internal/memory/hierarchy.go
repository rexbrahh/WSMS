// Package memory implements L0–L4 residency containers.
package memory

import (
	"errors"
	"fmt"
	"sync"

	"wsms/internal/types"
)

const (
	defaultResidentPages    = 64
	defaultResidentBytes    = 512 * 1024
	defaultPinnedPages      = 16
	defaultPinnedBytes      = 128 * 1024
	defaultPageBytes        = 64 * 1024
	defaultGhostEntries     = 64
	defaultGhostBytes       = 32 * 1024
	defaultShadowEntries    = 256
	defaultShadowBytes      = 64 * 1024
	defaultShadowUseHorizon = 64
	traceLimit              = 512
)

var (
	ErrInvalidPolicy  = errors.New("invalid memory policy")
	ErrInvalidPage    = errors.New("invalid memory page")
	ErrPageTooLarge   = errors.New("memory page retained bytes exceed page limit")
	ErrCapacity       = errors.New("memory hierarchy capacity exceeded")
	ErrPinnedCapacity = errors.New("memory hierarchy pinned capacity exceeded")
	ErrTupleMismatch  = errors.New("memory page tuple mismatch")
	ErrNotResident    = errors.New("memory page is not resident")
)

// Policy bounds the derived residency layers. Pinned pages are included in the
// global resident page/byte caps; their limits are stricter sub-caps, not extra
// memory.
type Policy struct {
	MaxResidentPages int
	MaxResidentBytes int
	MaxPinnedPages   int
	MaxPinnedBytes   int
	MaxPageBytes     int
	MaxGhostEntries  int
	MaxGhostBytes    int
	MaxShadowEntries int
	MaxShadowBytes   int
	ShadowUseHorizon uint64
}

// DefaultPolicy returns the fixed Phase 7F limits.
func DefaultPolicy() Policy {
	return Policy{
		MaxResidentPages: defaultResidentPages,
		MaxResidentBytes: defaultResidentBytes,
		MaxPinnedPages:   defaultPinnedPages,
		MaxPinnedBytes:   defaultPinnedBytes,
		MaxPageBytes:     defaultPageBytes,
		MaxGhostEntries:  defaultGhostEntries,
		MaxGhostBytes:    defaultGhostBytes,
		MaxShadowEntries: defaultShadowEntries,
		MaxShadowBytes:   defaultShadowBytes,
		ShadowUseHorizon: defaultShadowUseHorizon,
	}
}

// Validate rejects policies that cannot preserve the resident/pinned invariant.
func (p Policy) Validate() error {
	if p.MaxResidentPages <= 0 || p.MaxResidentBytes <= 0 || p.MaxPinnedPages < 0 ||
		p.MaxPinnedBytes < 0 || p.MaxPageBytes <= 0 || p.MaxGhostEntries < 0 ||
		p.MaxGhostBytes < 0 || p.MaxShadowEntries < 0 || p.MaxShadowBytes < 0 {
		return fmt.Errorf("%w: non-positive limit", ErrInvalidPolicy)
	}
	if p.MaxPinnedPages > p.MaxResidentPages {
		return fmt.Errorf("%w: pinned pages exceed resident pages", ErrInvalidPolicy)
	}
	if p.MaxPinnedBytes > p.MaxResidentBytes {
		return fmt.Errorf("%w: pinned bytes exceed resident bytes", ErrInvalidPolicy)
	}
	if p.MaxPageBytes > p.MaxResidentBytes {
		return fmt.Errorf("%w: page retained bytes exceed resident bytes", ErrInvalidPolicy)
	}
	return nil
}

func (p Policy) withDefaults() Policy {
	if p == (Policy{}) {
		return DefaultPolicy()
	}
	return p
}

type residentState string

const (
	stateCold   residentState = "cold"
	stateHot    residentState = "hot"
	statePinned residentState = "pinned"
)

type residentEntry struct {
	page         *Page
	state        residentState
	ref          bool
	realUses     int
	materialized bool
}

type ghostEntry struct {
	tuple           PageTuple
	page            *Page
	evictionTick    uint64
	evictionUseTick uint64
	evictionAge     uint64
	priorState      string
	retainedBytes   int
}

type shadowEntry struct {
	tuple              PageTuple
	embeddingNamespace string
	candidateOrdinal   int
	page               *Page
	observedUseTick    uint64
	useful             bool
}

type shadowKey struct {
	Tuple     PageTuple
	Namespace string
}

// TraceEvent is a redacted deterministic policy event. It intentionally carries
// tuple metadata but never resident body text.
type TraceEvent struct {
	Tick               uint64
	Operation          string
	PageID             string
	PageVersion        uint64
	SessionID          string
	SourceDigest       string
	CompilerVersion    string
	ScopeEpoch         uint64
	EmbeddingNamespace string
	State              string
	Reason             string
	RetainedBytes      int
}

// ResidentSnapshot is an exact copy of resident policy state without body text.
type ResidentSnapshot struct {
	Tuple         PageTuple
	State         string
	Ref           bool
	RealUses      int
	RetainedBytes int
	Stale         bool
	Materialized  bool
}

// GhostSnapshot describes a bodyless refault hint.
type GhostSnapshot struct {
	Tuple           PageTuple
	EvictionTick    uint64
	EvictionUseTick uint64
	EvictionAge     uint64
	PriorState      string
	RetainedBytes   int
}

// ShadowSnapshot describes a bodyless semantic observation.
type ShadowSnapshot struct {
	Tuple              PageTuple
	EmbeddingNamespace string
	CandidateOrdinal   int
	ObservedUseTick    uint64
	Useful             bool
}

// SemanticObservation is projection metadata for a semantic candidate. The
// embedding namespace is attribution for the rebuildable estimator, not part of
// the authoritative L4 page tuple.
type SemanticObservation struct {
	Tuple              PageTuple
	EmbeddingNamespace string
	CandidateOrdinal   int
}

// Metrics are bounded redacted policy counters for residency audits.
type Metrics struct {
	DemandAdmissions          uint64
	DemandPageIns             uint64
	ColdAdmissions            uint64
	PinAdmissions             uint64
	Uses                      uint64
	Hits                      uint64
	Promotions                uint64
	Demotions                 uint64
	Evictions                 uint64
	UnusedEvictions           uint64
	CapacityRejections        uint64
	PageTooLargeRejections    uint64
	PinCapacityRejections     uint64
	TupleRejections           uint64
	GhostHits                 uint64
	GhostMisses               uint64
	GhostMismatches           uint64
	GhostPurged               uint64
	GhostRefaultDistance      uint64
	GhostThrash               uint64
	ShadowObservations        uint64
	ShadowDuplicates          uint64
	ShadowNamespaceConflicts  uint64
	ShadowUseful              uint64
	ShadowUnused              uint64
	ShadowCensored            uint64
	AuthoritativeReplacements uint64
}

// Snapshot is a deterministic view of the derived residency tiers.
type Snapshot struct {
	Policy         Policy
	Metrics        Metrics
	Tick           uint64
	UseTick        uint64
	ResidentPages  int
	ResidentBytes  int
	HotPages       int
	ColdPages      int
	PinnedPages    int
	PinnedBytes    int
	GhostPages     int
	GhostBytes     int
	ShadowPages    int
	ShadowBytes    int
	NonresidentAge uint64
	Resident       []ResidentSnapshot
	Ghosts         []GhostSnapshot
	Shadows        []ShadowSnapshot
}

// UseResult describes the policy effects of an atomic resident lookup+use.
type UseResult struct {
	Tuple    PageTuple
	State    string
	Promoted bool
	Stale    bool
}

// Hierarchy holds semantic memory tiers.
// Coherence: write-through for truth; L1–L3 are derived caches.
type Hierarchy struct {
	mu     sync.RWMutex
	l0     map[string]any // turn scratch
	L1Text string         // last rendered capsule
	l2     map[string]*Page
	l3     map[string]*Page // interface-only in scaffold
	// L4 is external: ledger + artifacts

	policy Policy

	resident      map[string]*residentEntry
	residentOrder []string
	clock         int
	residentBytes int
	pinnedBytes   int
	pinnedPages   int

	ghosts     map[string]ghostEntry
	ghostOrder []string
	ghostBytes int

	shadows     map[shadowKey]shadowEntry
	shadowOrder []shadowKey
	shadowBytes int

	tick           uint64
	useTick        uint64
	nonresidentAge uint64
	trace          []TraceEvent
	metrics        Metrics
}

// NewHierarchy creates empty tiers with the fixed default Phase 7F policy.
func NewHierarchy() *Hierarchy {
	h, err := NewHierarchyWithPolicy(DefaultPolicy())
	if err != nil {
		panic(err)
	}
	return h
}

// NewHierarchyWithPolicy creates empty tiers with an explicit validated policy.
func NewHierarchyWithPolicy(policy Policy) (*Hierarchy, error) {
	policy = policy.withDefaults()
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Hierarchy{
		l0:       map[string]any{},
		l2:       map[string]*Page{},
		l3:       map[string]*Page{},
		policy:   policy,
		resident: map[string]*residentEntry{},
		ghosts:   map[string]ghostEntry{},
		shadows:  map[shadowKey]shadowEntry{},
	}, nil
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

// AdmitDemand admits an exact L4 materialization after a real demand fault.
// The first demand is resident cold/ref1. An exact ghost refault is promoted hot.
func (h *Hierarchy) AdmitDemand(p *Page) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	h.recordDemandEventLocked(p, "demand")
	if err := h.validatePage(p); err != nil {
		h.appendRejectLocked("admit_rejected", p, err)
		return err
	}
	if err := h.admitLocked(p, admitModeDemand, false); err != nil {
		h.appendRejectLocked("admit_rejected", p, err)
		return err
	}
	return nil
}

// AdmitCold admits a future-only page. It never consumes semantic shadow
// usefulness and starts cold/ref0.
func (h *Hierarchy) AdmitCold(p *Page) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	if err := h.validatePage(p); err != nil {
		h.mu.Lock()
		h.appendRejectLocked("admit_cold_rejected", p, err)
		h.mu.Unlock()
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	if err := h.admitLocked(p, admitModeCold, false); err != nil {
		h.appendRejectLocked("admit_cold_rejected", p, err)
		return err
	}
	return nil
}

// Pin admits or updates p as pinned. Pinned pages count against global resident
// limits and their own stricter sub-caps.
func (h *Hierarchy) Pin(p *Page) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	if err := h.validatePage(p); err != nil {
		h.mu.Lock()
		h.appendRejectLocked("pin_rejected", p, err)
		h.mu.Unlock()
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	if err := h.admitLocked(p, admitModePinned, true); err != nil {
		h.appendRejectLocked("pin_rejected", p, err)
		return err
	}
	return nil
}

// PinResident pins an already resident exact tuple.
func (h *Hierarchy) PinResident(p *Page) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	e, ok := h.resident[p.ID]
	if !ok {
		h.appendRejectLocked("pin_resident_rejected", p, ErrNotResident)
		return ErrNotResident
	}
	if !e.page.Tuple().matches(p) {
		h.appendRejectLocked("pin_resident_rejected", p, ErrTupleMismatch)
		return ErrTupleMismatch
	}
	if e.state == statePinned {
		h.appendTraceLocked("pin_resident", e.page, string(statePinned), "already_pinned")
		return nil
	}
	addBytes := pageSize(e.page)
	if h.pinnedPages+1 > h.policy.MaxPinnedPages || h.pinnedBytes+addBytes > h.policy.MaxPinnedBytes {
		h.appendRejectLocked("pin_resident_rejected", p, ErrPinnedCapacity)
		return ErrPinnedCapacity
	}
	h.pinnedPages++
	h.pinnedBytes += addBytes
	e.state = statePinned
	e.ref = true
	h.syncResidentTierLocked(e)
	h.appendTraceLocked("pin_resident", e.page, string(statePinned), "pinned")
	return nil
}

// Unpin converts an exact pinned resident tuple back to hot/ref1.
func (h *Hierarchy) Unpin(p *Page) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	e, ok := h.resident[p.ID]
	if !ok {
		h.appendRejectLocked("unpin_rejected", p, ErrNotResident)
		return ErrNotResident
	}
	if !e.page.Tuple().matches(p) {
		h.appendRejectLocked("unpin_rejected", p, ErrTupleMismatch)
		return ErrTupleMismatch
	}
	if e.state != statePinned {
		h.appendTraceLocked("unpin", e.page, string(e.state), "not_pinned")
		return nil
	}
	h.pinnedPages--
	h.pinnedBytes -= pageSize(e.page)
	e.state = stateHot
	e.ref = true
	h.syncResidentTierLocked(e)
	h.appendTraceLocked("unpin", e.page, string(stateHot), "unpinned")
	return nil
}

// Use atomically copies a resident page by ID and records a real use while the
// hierarchy lock is held. The hierarchy owns the exact tuple at cache-hit time,
// avoiding a GetPage -> RecordAccess TOCTOU window for known-address faults.
func (h *Hierarchy) Use(id string) (*Page, UseResult, bool) {
	if id == "" {
		return nil, UseResult{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	return h.useLocked(id, "use")
}

// LookupAndUse is the cache-hit primitive for legacy resolver naming.
func (h *Hierarchy) LookupAndUse(id string) (*Page, bool) {
	page, _, ok := h.Use(id)
	return page, ok
}

// LookupAndUseIfMaterialization atomically validates that the resident body and
// summary still match the caller's current exact representation before counting
// a cache hit. Mismatches are misses and do not mutate policy state.
func (h *Hierarchy) LookupAndUseIfMaterialization(id, expectedBody, expectedSummary string) (*Page, UseResult, bool) {
	if id == "" {
		return nil, UseResult{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	e, ok := h.resident[id]
	if !ok || e.page.Stale || e.page.Invalidated || !e.materialized {
		return nil, UseResult{}, false
	}
	if e.page.Body != expectedBody || e.page.Summary != expectedSummary {
		return nil, UseResult{}, false
	}
	return h.useResidentLocked(e, "lookup_use_validated")
}

// LookupAndUseIfCurrent validates the authoritative tuple plus current
// serveable representation fields before recording an L2 use. Mismatches are
// misses and do not mutate policy state.
func (h *Hierarchy) LookupAndUseIfCurrent(expected *Page) (*Page, UseResult, bool) {
	if expected == nil || expected.ID == "" {
		return nil, UseResult{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	e, ok := h.resident[expected.ID]
	if !ok || e.page.Stale || e.page.Invalidated || !e.materialized {
		return nil, UseResult{}, false
	}
	if !pageCurrentMatches(e.page, expected) {
		return nil, UseResult{}, false
	}
	return h.useResidentLocked(e, "lookup_use_validated")
}

func (h *Hierarchy) useLocked(id, op string) (*Page, UseResult, bool) {
	e, ok := h.resident[id]
	if !ok {
		return nil, UseResult{}, false
	}
	if e.page.Stale || e.page.Invalidated || !e.materialized {
		return nil, UseResult{}, false
	}
	return h.useResidentLocked(e, op)
}

func (h *Hierarchy) useResidentLocked(e *residentEntry, op string) (*Page, UseResult, bool) {
	cp := clonePage(e.page)
	h.useTick++
	h.markShadowUsefulLocked(e.page, op)
	e.realUses++
	e.ref = true
	e.page.AccessCount++
	h.metrics.Uses++
	h.metrics.Hits++
	result := UseResult{
		Tuple: e.page.Tuple(),
		State: string(e.state),
		Stale: e.page.Stale || e.page.Invalidated,
	}
	if e.state == stateCold && e.realUses >= 2 {
		h.nonresidentAge++
		e.state = stateHot
		h.syncResidentTierLocked(e)
		result.State = string(stateHot)
		result.Promoted = true
		h.metrics.Promotions++
		h.appendTraceLocked(op, e.page, string(stateHot), "second_use_promoted")
		return cp, result, true
	}
	h.appendTraceLocked(op, e.page, string(e.state), "accessed")
	return cp, result, true
}

func (h *Hierarchy) recordDemandEventLocked(p *Page, reason string) {
	h.useTick++
	h.metrics.Uses++
	h.metrics.DemandPageIns++
	h.markShadowUsefulLocked(p, reason)
}

// Invalidate removes an exact resident/ghost/shadow tuple. It is a shootdown,
// not a ghostable eviction, because invalid authority must not guide refaults.
func (h *Hierarchy) Invalidate(p *Page) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	if e, ok := h.resident[p.ID]; ok {
		if !e.page.Tuple().matches(p) {
			h.appendRejectLocked("invalidate_rejected", p, ErrTupleMismatch)
			return ErrTupleMismatch
		}
		h.removeResidentLocked(p.ID)
	}
	h.deleteGhostLocked(p.ID)
	h.censorShadowsForIDLocked(p.ID)
	h.appendTraceLocked("invalidate", p, "", "removed")
	return nil
}

// Shootdown removes every derived metadata tier for a logical ID without
// creating a ghost. It is used when authority learns an ID was invalidated but
// only ghost/shadow metadata may remain.
func (h *Hierarchy) Shootdown(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	var tracePage *Page
	if entry := h.resident[id]; entry != nil {
		tracePage = cloneMetadataPage(entry.page)
		h.removeResidentLocked(id)
	}
	if tracePage == nil {
		if ghost, ok := h.ghosts[id]; ok {
			tracePage = cloneMetadataPage(ghost.page)
		} else {
			for _, key := range h.shadowOrder {
				if shadow, ok := h.shadows[key]; ok && shadow.tuple.ID == id {
					tracePage = cloneMetadataPage(shadow.page)
					break
				}
			}
			if tracePage == nil {
				tracePage = &Page{ID: id}
			}
		}
	}
	h.deleteGhostLocked(id)
	h.censorShadowsForIDLocked(id)
	h.appendTraceLocked("shootdown", tracePage, "", "id_invalidated")
}

// PurgeAllGhosts conservatively removes every nonresident refault hint after a
// broad coherence transition. Residents remain precisely reconciled; ghosts are
// disposable estimators and fail closed.
func (h *Hierarchy) PurgeAllGhosts(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	if reason == "" {
		reason = "coherence_transition"
	}
	for _, id := range append([]string(nil), h.ghostOrder...) {
		ghost, ok := h.ghosts[id]
		if !ok {
			continue
		}
		h.metrics.GhostPurged++
		h.appendTraceLocked("ghost_purge_all", ghost.page, "", reason)
		h.deleteGhostLocked(id)
	}
}

// CensorAllShadows conservatively removes every semantic observation after a
// broad coherence transition where dependency-specific reverse maps are not yet
// available. Pending shadows count as censored; already useful shadows keep
// their observed outcome and are simply removed.
func (h *Hierarchy) CensorAllShadows(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	if reason == "" {
		reason = "coherence_transition"
	}
	for _, key := range append([]shadowKey(nil), h.shadowOrder...) {
		sh, ok := h.shadows[key]
		if !ok {
			continue
		}
		if !sh.useful {
			h.metrics.ShadowCensored++
		}
		h.appendTraceLocked("shadow_censor", sh.page, "", reason)
		h.deleteShadowKeyLocked(key)
	}
}

// AdmitAuthoritativeDemand replaces any old tuple for p.ID before admitting p
// as a sealed L4 page-in. This is reserved for callers that have already
// committed final authoritative page state; normal AdmitDemand remains strict.
func (h *Hierarchy) AdmitAuthoritativeDemand(p *Page) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	h.recordDemandEventLocked(p, "authoritative_demand")
	if old := h.resident[p.ID]; old != nil && !old.page.Tuple().matches(p) {
		h.removeResidentLocked(p.ID)
		h.metrics.AuthoritativeReplacements++
		h.appendTraceLocked("authoritative_replace", p, "", "resident_tuple_replaced")
	}
	if ghost, ok := h.ghosts[p.ID]; ok && !ghost.tuple.matches(p) {
		h.deleteGhostLocked(p.ID)
		h.metrics.AuthoritativeReplacements++
		h.appendTraceLocked("authoritative_replace", p, "", "ghost_tuple_replaced")
	}
	for _, key := range append([]shadowKey(nil), h.shadowOrder...) {
		if sh, ok := h.shadows[key]; ok && sh.tuple.ID == p.ID && !sh.tuple.matches(p) {
			h.censorShadowKeyLocked(key)
			h.appendTraceLocked("authoritative_replace", p, "", "shadow_tuple_replaced")
		}
	}
	if err := h.validatePage(p); err != nil {
		h.appendRejectLocked("authoritative_admit_rejected", p, err)
		return err
	}
	if err := h.admitLocked(p, admitModeDemand, false); err != nil {
		h.appendRejectLocked("authoritative_admit_rejected", p, err)
		return err
	}
	return nil
}

// Revalidate clears stale/invalidated flags for an exact resident tuple.
func (h *Hierarchy) Revalidate(p *Page) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	e, ok := h.resident[p.ID]
	if !ok {
		h.appendRejectLocked("revalidate_rejected", p, ErrNotResident)
		return ErrNotResident
	}
	if !e.page.Tuple().matches(p) {
		h.appendRejectLocked("revalidate_rejected", p, ErrTupleMismatch)
		return ErrTupleMismatch
	}
	e.page.Stale = false
	e.page.Invalidated = false
	e.page.StaleRevision = 0
	oldSize := pageSize(e.page)
	e.page.Body = ""
	e.page.Summary = ""
	e.page.Refs = nil
	e.materialized = false
	newSize := pageSize(e.page)
	h.residentBytes += newSize - oldSize
	if e.state == statePinned {
		h.pinnedBytes += newSize - oldSize
	}
	h.appendTraceLocked("revalidate", e.page, string(e.state), "active")
	return nil
}

// ObserveSemantic records bodyless tuple metadata for a semantic candidate.
// It does not change residency or usefulness until a later exact demand/use.
func (h *Hierarchy) ObserveSemantic(p *Page) error {
	return h.observeSemantic(p, 0)
}

func (h *Hierarchy) observeSemantic(p *Page, candidateOrdinal int) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	h.expireShadowsLocked()
	cp := cloneMetadataPage(p)
	if err := h.putShadowLocked(cp, candidateOrdinal); err != nil {
		h.appendRejectLocked("observe_semantic_rejected", cp, err)
		return err
	}
	h.appendTraceLocked("observe_semantic", cp, "", "shadow")
	return nil
}

// ObserveSemanticObservation records bodyless semantic metadata when the caller
// already has a tuple and projection namespace without a resident Page value.
func (h *Hierarchy) ObserveSemanticObservation(obs SemanticObservation) error {
	if obs.Tuple.ID == "" {
		return ErrInvalidPage
	}
	return h.observeSemantic(&Page{
		ID:                 obs.Tuple.ID,
		PageVersion:        obs.Tuple.PageVersion,
		SessionID:          obs.Tuple.SessionID,
		SourceDigest:       obs.Tuple.SourceDigest,
		CompilerVersion:    obs.Tuple.CompilerVersion,
		ScopeEpoch:         obs.Tuple.ScopeEpoch,
		EmbeddingNamespace: obs.EmbeddingNamespace,
	}, obs.CandidateOrdinal)
}

// PutL2 inserts/updates a hot page. This legacy API preserves its historical
// behavior; new demand faults should use AdmitDemand directly.
func (h *Hierarchy) PutL2(p *Page) {
	if p == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	if err := h.admitLocked(p, admitModeLegacyHot, false); err != nil {
		h.appendRejectLocked("put_l2_rejected", p, err)
	}
}

// PutL3 inserts/updates a warm page and removes any hot copy of the same ID.
func (h *Hierarchy) PutL3(p *Page) {
	if p == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	if err := h.admitLocked(p, admitModeLegacyWarm, false); err != nil {
		h.appendRejectLocked("put_l3_rejected", p, err)
	}
}

// GetPage looks up resident L2 then L3.
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

// Snapshot returns a deterministic copy of current residency state.
func (h *Hierarchy) Snapshot() Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	snap := Snapshot{
		Policy:         h.policy,
		Metrics:        h.metrics,
		Tick:           h.tick,
		UseTick:        h.useTick,
		ResidentPages:  len(h.resident),
		ResidentBytes:  h.residentBytes,
		PinnedPages:    h.pinnedPages,
		PinnedBytes:    h.pinnedBytes,
		GhostPages:     len(h.ghosts),
		GhostBytes:     h.ghostBytes,
		ShadowPages:    len(h.shadows),
		ShadowBytes:    h.shadowBytes,
		NonresidentAge: h.nonresidentAge,
		Resident:       make([]ResidentSnapshot, 0, len(h.residentOrder)),
		Ghosts:         make([]GhostSnapshot, 0, len(h.ghostOrder)),
		Shadows:        make([]ShadowSnapshot, 0, len(h.shadowOrder)),
	}
	for _, id := range h.residentOrder {
		e := h.resident[id]
		if e == nil {
			continue
		}
		switch e.state {
		case stateHot:
			snap.HotPages++
		case stateCold:
			snap.ColdPages++
		case statePinned:
			snap.PinnedPages = h.pinnedPages
		}
		snap.Resident = append(snap.Resident, ResidentSnapshot{
			Tuple:         e.page.Tuple(),
			State:         string(e.state),
			Ref:           e.ref,
			RealUses:      e.realUses,
			RetainedBytes: pageSize(e.page),
			Stale:         e.page.Stale || e.page.Invalidated,
			Materialized:  e.materialized,
		})
	}
	for _, id := range h.ghostOrder {
		g, ok := h.ghosts[id]
		if !ok {
			continue
		}
		snap.Ghosts = append(snap.Ghosts, GhostSnapshot{
			Tuple:           g.tuple,
			EvictionTick:    g.evictionTick,
			EvictionUseTick: g.evictionUseTick,
			EvictionAge:     g.evictionAge,
			PriorState:      g.priorState,
			RetainedBytes:   g.retainedBytes,
		})
	}
	for _, key := range h.shadowOrder {
		sh, ok := h.shadows[key]
		if !ok {
			continue
		}
		snap.Shadows = append(snap.Shadows, ShadowSnapshot{
			Tuple: sh.tuple, EmbeddingNamespace: sh.embeddingNamespace, CandidateOrdinal: sh.candidateOrdinal,
			ObservedUseTick: sh.observedUseTick, Useful: sh.useful,
		})
	}
	return snap
}

// Trace returns a deterministic redacted copy of policy events.
func (h *Hierarchy) Trace() []TraceEvent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]TraceEvent, len(h.trace))
	copy(out, h.trace)
	return out
}

func clonePage(p *Page) *Page {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Refs != nil {
		cp.Refs = append([]string{}, p.Refs...)
	}
	if p.Paths != nil {
		cp.Paths = append([]string{}, p.Paths...)
	}
	return &cp
}

func cloneMetadataPage(p *Page) *Page {
	cp := clonePage(p)
	if cp == nil {
		return nil
	}
	cp.Body = ""
	cp.Summary = ""
	cp.Refs = nil
	return cp
}

// PageCoherence is an authoritative metadata refresh for a resident page.
type PageCoherence struct {
	Stale         bool
	Invalidated   bool
	StaleRevision uint64
	Branch        string
	Commit        string
	Paths         []string
	ScopeEpoch    uint64
}

// Reconcile atomically refreshes every resident page's authoritative
// coherence metadata. The callback receives a clone, so policy evaluation
// cannot mutate hierarchy-owned pages. Invalidated residents are eagerly
// removed and cannot become ghost refault hints.
func (h *Hierarchy) Reconcile(evaluate func(*Page) PageCoherence) {
	if evaluate == nil {
		return
	}
	type reconciliation struct {
		tuple  PageTuple
		status PageCoherence
	}
	h.mu.RLock()
	pages := make([]*Page, 0, len(h.residentOrder))
	for _, id := range h.residentOrder {
		if entry := h.resident[id]; entry != nil {
			pages = append(pages, clonePage(entry.page))
		}
	}
	h.mu.RUnlock()
	results := make([]reconciliation, 0, len(pages))
	for _, page := range pages {
		results = append(results, reconciliation{tuple: page.Tuple(), status: evaluate(clonePage(page))})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	for _, result := range results {
		entry := h.resident[result.tuple.ID]
		if entry == nil || entry.page.Tuple() != result.tuple {
			continue
		}
		page := entry.page
		status := result.status
		if status.Invalidated {
			h.removeResidentLocked(result.tuple.ID)
			h.deleteGhostLocked(result.tuple.ID)
			h.censorShadowsForIDLocked(result.tuple.ID)
			h.appendTraceLocked("reconcile", page, "", "invalidated_removed")
			continue
		}
		next := clonePage(page)
		reason := "refreshed"
		if page.Stale && !status.Stale {
			next.Body = ""
			next.Summary = ""
			next.Refs = nil
			next.Stale = false
			next.Invalidated = false
			next.StaleRevision = 0
			next.Branch = status.Branch
			next.Commit = status.Commit
			next.Paths = append([]string(nil), status.Paths...)
			next.ScopeEpoch = status.ScopeEpoch
			reason = "stale_active_body_cleared"
		} else {
			next.Stale = status.Stale
			next.Invalidated = false
			next.StaleRevision = status.StaleRevision
			next.Branch = status.Branch
			next.Commit = status.Commit
			next.Paths = append([]string(nil), status.Paths...)
			next.ScopeEpoch = status.ScopeEpoch
		}
		if err := h.validateResidentReplacementLocked(entry, next); err != nil {
			h.appendRejectLocked("reconcile_rejected", next, err)
			h.removeResidentLocked(result.tuple.ID)
			h.deleteGhostLocked(result.tuple.ID)
			h.appendTraceLocked("reconcile", next, "", "metadata_refresh_rejected")
			continue
		}
		if reason == "stale_active_body_cleared" {
			entry.materialized = false
			h.deleteGhostLocked(result.tuple.ID)
		}
		h.replaceResidentPageLocked(entry, next)
		h.appendTraceLocked("reconcile", entry.page, string(entry.state), reason)
	}
}

// ClearL1Capsule drops the previously rendered working set after a coherence
// transition. The next safe BeforeTurn boundary rebuilds it from eligible state.
func (h *Hierarchy) ClearL1Capsule() {
	h.SetL1Capsule("")
}

// RecordAccess bumps access stats if page is resident. Legacy callers only have
// an ID, so this intentionally does not enforce exact tuple matching.
func (h *Hierarchy) RecordAccess(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advanceLocked()
	if e, ok := h.resident[id]; ok {
		e.page.AccessCount++
		e.ref = true
		h.appendTraceLocked("record_access", e.page, string(e.state), "legacy")
		return
	}
	if p, ok := h.l2[id]; ok {
		p.AccessCount++
		return
	}
	if p, ok := h.l3[id]; ok {
		p.AccessCount++
	}
}

func (h *Hierarchy) validateResidentReplacementLocked(entry *residentEntry, next *Page) error {
	if entry == nil || entry.page == nil || next == nil {
		return ErrInvalidPage
	}
	nextSize := pageSize(next)
	if nextSize > h.policy.MaxPageBytes {
		return ErrPageTooLarge
	}
	diff := nextSize - pageSize(entry.page)
	if h.residentBytes+diff > h.policy.MaxResidentBytes {
		return ErrCapacity
	}
	if entry.state == statePinned && h.pinnedBytes+diff > h.policy.MaxPinnedBytes {
		return ErrPinnedCapacity
	}
	return nil
}

func (h *Hierarchy) replaceResidentPageLocked(entry *residentEntry, next *Page) {
	oldSize := pageSize(entry.page)
	newSize := pageSize(next)
	h.residentBytes += newSize - oldSize
	if entry.state == statePinned {
		h.pinnedBytes += newSize - oldSize
	}
	*entry.page = *clonePage(next)
	h.syncResidentTierLocked(entry)
}

type admitMode int

const (
	admitModeDemand admitMode = iota
	admitModeCold
	admitModePinned
	admitModeLegacyHot
	admitModeLegacyWarm
)

func (h *Hierarchy) admitLocked(p *Page, mode admitMode, pinned bool) error {
	cp := clonePage(p)
	if cp == nil || cp.ID == "" {
		return ErrInvalidPage
	}
	if err := h.validatePage(cp); err != nil {
		return err
	}
	newSize := pageSize(cp)
	old := h.resident[cp.ID]
	var oldSize int
	var wasPinned bool
	if old != nil {
		if !old.page.Tuple().matches(cp) {
			if mode != admitModeDemand || !old.page.Stale {
				return ErrTupleMismatch
			}
			oldSize = pageSize(old.page)
			wasPinned = old.state == statePinned
		} else {
			oldSize = pageSize(old.page)
			wasPinned = old.state == statePinned
		}
	}
	targetPinned := pinned || wasPinned || mode == admitModePinned
	if old != nil && !old.page.Tuple().matches(cp) && old.page.Stale && mode == admitModeDemand {
		targetPinned = false
	}
	if err := h.preflightCapacityLocked(newSize, oldSize, old != nil, targetPinned, wasPinned); err != nil {
		return err
	}
	if old != nil && !old.page.Tuple().matches(cp) && old.page.Stale && mode == admitModeDemand {
		h.removeResidentLocked(cp.ID)
		h.deleteGhostLocked(cp.ID)
		h.censorShadowsForIDLocked(cp.ID)
		h.appendTraceLocked("replace_stale", cp, "", "tuple_replaced")
		old = nil
	}
	if old == nil {
		h.purgeMismatchedMetadataLocked(cp)
		entry := &residentEntry{page: cp, materialized: hasMaterialization(cp)}
		switch mode {
		case admitModeDemand:
			h.metrics.DemandAdmissions++
			if ghost, ok := h.exactGhostLocked(cp); ok {
				entry.state = stateHot
				entry.ref = true
				entry.realUses = 1
				h.metrics.GhostHits++
				distance := h.nonresidentAge - ghost.evictionAge
				h.metrics.GhostRefaultDistance += distance
				hotPages := h.unpinnedHotPagesLocked()
				if hotPages > 0 && distance <= uint64(hotPages) {
					h.metrics.GhostThrash++
				}
				h.nonresidentAge++
				h.deleteGhostLocked(cp.ID)
			} else {
				h.metrics.GhostMisses++
				entry.state = stateCold
				entry.ref = true
				entry.realUses = 1
			}
		case admitModeCold:
			h.metrics.ColdAdmissions++
			entry.state = stateCold
			entry.ref = false
		case admitModeLegacyWarm:
			h.metrics.ColdAdmissions++
			entry.state = stateCold
			entry.ref = false
		case admitModePinned:
			h.metrics.PinAdmissions++
			entry.state = statePinned
			entry.ref = true
		case admitModeLegacyHot:
			entry.state = stateHot
			entry.ref = true
		}
		h.resident[cp.ID] = entry
		h.residentOrder = append(h.residentOrder, cp.ID)
		h.residentBytes += newSize
		if entry.state == statePinned {
			h.pinnedPages++
			h.pinnedBytes += newSize
		}
		h.syncResidentTierLocked(entry)
		if err := h.enforceResidentCapacityLocked(); err != nil {
			return err
		}
		h.appendTraceLocked("admit", cp, string(entry.state), admitReason(mode))
		return nil
	}
	h.residentBytes += newSize - oldSize
	if wasPinned {
		h.pinnedBytes += newSize - oldSize
	}
	old.page = cp
	old.materialized = hasMaterialization(cp)
	switch mode {
	case admitModeDemand:
		old.realUses++
		old.page.AccessCount++
		old.ref = true
		if old.state == stateCold && old.realUses >= 2 {
			h.nonresidentAge++
			old.state = stateHot
			h.metrics.Promotions++
		}
	case admitModeCold:
		if old.state != statePinned && old.state != stateHot {
			old.state = stateCold
		}
	case admitModeLegacyWarm:
		if old.state != statePinned {
			old.state = stateCold
			old.ref = false
		}
	case admitModePinned:
		if old.state != statePinned {
			old.state = statePinned
			h.pinnedPages++
			h.pinnedBytes += newSize
			h.metrics.PinAdmissions++
		}
		old.ref = true
	case admitModeLegacyHot:
		if old.state == statePinned {
			old.ref = true
		} else {
			old.state = stateHot
			old.ref = true
		}
	}
	h.syncResidentTierLocked(old)
	if err := h.enforceResidentCapacityLocked(); err != nil {
		return err
	}
	h.appendTraceLocked("admit", cp, string(old.state), admitReason(mode))
	return nil
}

func admitReason(mode admitMode) string {
	switch mode {
	case admitModeDemand:
		return "demand"
	case admitModeCold:
		return "cold"
	case admitModePinned:
		return "pinned"
	case admitModeLegacyHot:
		return "legacy_hot"
	case admitModeLegacyWarm:
		return "legacy_warm"
	default:
		return "unknown"
	}
}

func (h *Hierarchy) validatePage(p *Page) error {
	if p == nil || p.ID == "" {
		return ErrInvalidPage
	}
	if pageSize(p) > h.policy.MaxPageBytes {
		return ErrPageTooLarge
	}
	return nil
}

func (h *Hierarchy) preflightCapacityLocked(newSize, oldSize int, alreadyResident, targetPinned, wasPinned bool) error {
	addPages := 0
	if !alreadyResident {
		addPages = 1
	}
	addBytes := newSize - oldSize
	pinnedPages := h.pinnedPages
	pinnedBytes := h.pinnedBytes
	if wasPinned && !targetPinned {
		pinnedPages--
		pinnedBytes -= oldSize
	} else if targetPinned && !wasPinned {
		pinnedPages++
		pinnedBytes += newSize
	} else if targetPinned && wasPinned {
		pinnedBytes += addBytes
	}
	if pinnedPages > h.policy.MaxPinnedPages || pinnedBytes > h.policy.MaxPinnedBytes {
		return ErrPinnedCapacity
	}
	if pinnedPages > h.policy.MaxResidentPages || pinnedBytes > h.policy.MaxResidentBytes {
		return ErrCapacity
	}
	residentPagesAfter := len(h.resident) + addPages
	residentBytesAfter := h.residentBytes + addBytes
	if residentPagesAfter <= h.policy.MaxResidentPages && residentBytesAfter <= h.policy.MaxResidentBytes {
		return nil
	}
	// Over the global cap, admission is still feasible if eviction can drop
	// enough unpinned pages: the post-evict floor is the pinned set plus this
	// page when it is not itself pinned. Pinned pages are never eviction
	// victims, so if that floor already exceeds the cap the admission fails.
	minPagesAfterEvict := pinnedPages
	if !targetPinned {
		minPagesAfterEvict++
	}
	minBytesAfterEvict := pinnedBytes
	if !targetPinned {
		minBytesAfterEvict += newSize
	}
	if minPagesAfterEvict > h.policy.MaxResidentPages || minBytesAfterEvict > h.policy.MaxResidentBytes {
		return ErrCapacity
	}
	return nil
}

func (h *Hierarchy) enforceResidentCapacityLocked() error {
	for h.overCapacityLocked() {
		if len(h.residentOrder) == 0 {
			return ErrCapacity
		}
		evicted := false
		maxScans := len(h.residentOrder)*3 + 1
		for scans := 0; scans < maxScans && h.overCapacityLocked(); scans++ {
			if len(h.residentOrder) == 0 {
				return ErrCapacity
			}
			if h.clock >= len(h.residentOrder) {
				h.clock = 0
			}
			id := h.residentOrder[h.clock]
			entry := h.resident[id]
			if entry == nil {
				h.removeOrderAtLocked(h.clock)
				continue
			}
			switch entry.state {
			case statePinned:
				h.clock = (h.clock + 1) % len(h.residentOrder)
			case stateHot:
				if entry.ref {
					entry.ref = false
					h.appendTraceLocked("scan", entry.page, string(stateHot), "hot_ref_cleared")
				} else {
					entry.state = stateCold
					h.syncResidentTierLocked(entry)
					h.metrics.Demotions++
					h.appendTraceLocked("scan", entry.page, string(stateCold), "hot_demoted")
				}
				h.clock = (h.clock + 1) % len(h.residentOrder)
			case stateCold:
				if entry.ref {
					entry.ref = false
					h.appendTraceLocked("scan", entry.page, string(stateCold), "cold_ref_cleared")
					h.clock = (h.clock + 1) % len(h.residentOrder)
				} else {
					h.evictAtClockLocked()
					evicted = true
				}
			}
		}
		if !evicted && h.overCapacityLocked() {
			return ErrCapacity
		}
	}
	return nil
}

func (h *Hierarchy) overCapacityLocked() bool {
	return len(h.resident) > h.policy.MaxResidentPages || h.residentBytes > h.policy.MaxResidentBytes
}

func (h *Hierarchy) unpinnedHotPagesLocked() int {
	count := 0
	for _, entry := range h.resident {
		if entry != nil && entry.state == stateHot {
			count++
		}
	}
	return count
}

func (h *Hierarchy) evictAtClockLocked() {
	if len(h.residentOrder) == 0 {
		return
	}
	if h.clock >= len(h.residentOrder) {
		h.clock = 0
	}
	id := h.residentOrder[h.clock]
	entry := h.resident[id]
	if entry != nil {
		h.metrics.Evictions++
		if entry.realUses == 0 {
			h.metrics.UnusedEvictions++
		}
		h.addGhostLocked(entry.page, string(entry.state))
		if entry.state == stateCold {
			h.nonresidentAge++
		}
		h.appendTraceLocked("evict", entry.page, string(entry.state), "capacity")
	}
	h.removeResidentLocked(id)
}

func (h *Hierarchy) removeResidentLocked(id string) {
	entry := h.resident[id]
	if entry == nil {
		return
	}
	size := pageSize(entry.page)
	h.residentBytes -= size
	if entry.state == statePinned {
		h.pinnedPages--
		h.pinnedBytes -= size
	}
	delete(h.resident, id)
	delete(h.l2, id)
	delete(h.l3, id)
	for i, existing := range h.residentOrder {
		if existing == id {
			h.removeOrderAtLocked(i)
			if i < h.clock && h.clock > 0 {
				h.clock--
			}
			break
		}
	}
	if h.clock >= len(h.residentOrder) && len(h.residentOrder) > 0 {
		h.clock = 0
	}
}

func (h *Hierarchy) removeOrderAtLocked(i int) {
	if i < 0 || i >= len(h.residentOrder) {
		return
	}
	h.residentOrder = append(h.residentOrder[:i], h.residentOrder[i+1:]...)
	if h.clock >= len(h.residentOrder) && len(h.residentOrder) > 0 {
		h.clock = 0
	}
}

func (h *Hierarchy) syncResidentTierLocked(entry *residentEntry) {
	if entry == nil || entry.page == nil {
		return
	}
	switch entry.state {
	case stateCold:
		entry.page.Tier = types.TierL3Warm
		delete(h.l2, entry.page.ID)
		h.l3[entry.page.ID] = entry.page
	case stateHot, statePinned:
		entry.page.Tier = types.TierL2Hot
		delete(h.l3, entry.page.ID)
		h.l2[entry.page.ID] = entry.page
	}
}

func (h *Hierarchy) exactGhostLocked(p *Page) (ghostEntry, bool) {
	g, ok := h.ghosts[p.ID]
	return g, ok && g.tuple.matches(p)
}

func (h *Hierarchy) addGhostLocked(p *Page, priorState string) {
	if h.policy.MaxGhostEntries == 0 || h.policy.MaxGhostBytes == 0 || p == nil || p.ID == "" {
		return
	}
	cp := cloneMetadataPage(p)
	size := pageSize(cp)
	if size > h.policy.MaxGhostBytes {
		return
	}
	if old, ok := h.ghosts[cp.ID]; ok {
		h.ghostBytes -= pageSize(old.page)
	} else {
		h.ghostOrder = append(h.ghostOrder, cp.ID)
	}
	h.ghosts[cp.ID] = ghostEntry{
		tuple: cp.Tuple(), page: cp, evictionTick: h.tick, evictionUseTick: h.useTick,
		evictionAge: h.nonresidentAge, priorState: priorState, retainedBytes: size,
	}
	h.ghostBytes += size
	h.enforceGhostCapLocked()
}

func (h *Hierarchy) deleteGhostLocked(id string) {
	if old, ok := h.ghosts[id]; ok {
		h.ghostBytes -= pageSize(old.page)
	}
	delete(h.ghosts, id)
	for i, existing := range h.ghostOrder {
		if existing == id {
			h.ghostOrder = append(h.ghostOrder[:i], h.ghostOrder[i+1:]...)
			return
		}
	}
}

func (h *Hierarchy) enforceGhostCapLocked() {
	for len(h.ghostOrder) > h.policy.MaxGhostEntries || h.ghostBytes > h.policy.MaxGhostBytes {
		if len(h.ghostOrder) == 0 {
			h.ghostBytes = 0
			return
		}
		oldest := h.ghostOrder[0]
		h.deleteGhostLocked(oldest)
	}
}

func (h *Hierarchy) markShadowUsefulLocked(p *Page, reason string) {
	h.expireShadowsLocked()
	for _, key := range h.shadowOrder {
		sh, ok := h.shadows[key]
		if !ok {
			continue
		}
		if !sh.tuple.matches(p) {
			continue
		}
		if sh.useful {
			continue
		}
		h.metrics.ShadowUseful++
		sh.useful = true
		h.shadows[key] = sh
		h.appendTraceLocked("shadow_useful", sh.page, "", reason)
	}
}

func (h *Hierarchy) deleteShadowKeyLocked(key shadowKey) {
	if old, ok := h.shadows[key]; ok {
		h.shadowBytes -= pageSize(old.page)
	}
	delete(h.shadows, key)
	for i, existing := range h.shadowOrder {
		if existing == key {
			h.shadowOrder = append(h.shadowOrder[:i], h.shadowOrder[i+1:]...)
			return
		}
	}
}

func (h *Hierarchy) putShadowLocked(p *Page, candidateOrdinal int) error {
	if h.policy.MaxShadowEntries == 0 || h.policy.MaxShadowBytes == 0 || h.policy.ShadowUseHorizon == 0 {
		return nil
	}
	size := pageSize(p)
	if size > h.policy.MaxShadowBytes {
		return ErrCapacity
	}
	for key, old := range h.shadows {
		if old.tuple.ID == p.ID && old.tuple != p.Tuple() {
			h.censorShadowKeyLocked(key)
			h.appendTraceLocked("shadow_purge", p, "", "tuple_mismatch")
		}
	}
	key := makeShadowKey(p.Tuple(), p.EmbeddingNamespace)
	if old, ok := h.shadows[key]; ok {
		// A duplicate observation must not extend the working-set horizon or
		// replace the first deterministic candidate order.
		h.metrics.ShadowDuplicates++
		oldSize := pageSize(old.page)
		next := cloneMetadataPage(p)
		nextSize := pageSize(next)
		if h.shadowBytes-oldSize+nextSize > h.policy.MaxShadowBytes {
			return ErrCapacity
		}
		old.page = next
		h.shadowBytes += nextSize - oldSize
		h.shadows[key] = old
		return nil
	}
	h.shadowOrder = append(h.shadowOrder, key)
	h.shadows[key] = shadowEntry{
		tuple: p.Tuple(), embeddingNamespace: p.EmbeddingNamespace, candidateOrdinal: candidateOrdinal,
		page: cloneMetadataPage(p), observedUseTick: h.useTick,
	}
	h.shadowBytes += size
	h.metrics.ShadowObservations++
	h.enforceShadowCapLocked()
	return nil
}

func makeShadowKey(tuple PageTuple, namespace string) shadowKey {
	return shadowKey{Tuple: tuple, Namespace: namespace}
}

func (h *Hierarchy) censorShadowKeyLocked(key shadowKey) {
	if sh, ok := h.shadows[key]; ok {
		if !sh.useful {
			h.metrics.ShadowCensored++
		}
		h.deleteShadowKeyLocked(key)
	}
}

func (h *Hierarchy) censorShadowsForIDLocked(id string) {
	for _, key := range append([]shadowKey(nil), h.shadowOrder...) {
		if sh, ok := h.shadows[key]; ok && sh.tuple.ID == id {
			h.censorShadowKeyLocked(key)
		}
	}
}

func (h *Hierarchy) deleteShadowsForIDLocked(id string) {
	for _, key := range append([]shadowKey(nil), h.shadowOrder...) {
		if sh, ok := h.shadows[key]; ok && sh.tuple.ID == id {
			h.deleteShadowKeyLocked(key)
		}
	}
}

func (h *Hierarchy) enforceShadowCapLocked() {
	for len(h.shadowOrder) > h.policy.MaxShadowEntries || h.shadowBytes > h.policy.MaxShadowBytes {
		if len(h.shadowOrder) == 0 {
			h.shadowBytes = 0
			return
		}
		oldest := h.shadowOrder[0]
		if old, ok := h.shadows[oldest]; ok && !old.useful {
			h.metrics.ShadowUnused++
		}
		h.deleteShadowKeyLocked(oldest)
	}
}

func (h *Hierarchy) expireShadowsLocked() {
	if h.policy.ShadowUseHorizon == 0 {
		for key := range h.shadows {
			h.deleteShadowKeyLocked(key)
		}
		h.shadowOrder = nil
		h.shadowBytes = 0
		return
	}
	for _, id := range append([]shadowKey(nil), h.shadowOrder...) {
		sh, ok := h.shadows[id]
		if !ok {
			continue
		}
		if h.useTick-sh.observedUseTick > h.policy.ShadowUseHorizon {
			if !sh.useful {
				h.metrics.ShadowUnused++
			}
			h.deleteShadowKeyLocked(id)
			h.appendTraceLocked("shadow_expire", sh.page, "", "ttl")
		}
	}
}

func (h *Hierarchy) purgeMismatchedMetadataLocked(p *Page) {
	if p == nil {
		return
	}
	if g, ok := h.ghosts[p.ID]; ok && !g.tuple.matches(p) {
		h.metrics.GhostMismatches++
		h.deleteGhostLocked(p.ID)
		h.appendTraceLocked("ghost_purge", p, "", "tuple_mismatch")
	}
	for _, key := range append([]shadowKey(nil), h.shadowOrder...) {
		if sh, ok := h.shadows[key]; ok && sh.tuple.ID == p.ID && !sh.tuple.matches(p) {
			h.censorShadowKeyLocked(key)
			h.appendTraceLocked("shadow_purge", p, "", "tuple_mismatch")
		}
	}
}

func (h *Hierarchy) appendTraceLocked(op string, p *Page, state, reason string) {
	if p == nil {
		return
	}
	event := TraceEvent{
		Tick:               h.tick,
		Operation:          op,
		PageID:             p.ID,
		PageVersion:        p.PageVersion,
		SessionID:          p.SessionID,
		SourceDigest:       p.SourceDigest,
		CompilerVersion:    p.CompilerVersion,
		EmbeddingNamespace: p.EmbeddingNamespace,
		ScopeEpoch:         p.ScopeEpoch,
		State:              state,
		Reason:             reason,
		RetainedBytes:      pageSize(p),
	}
	h.trace = append(h.trace, event)
	if len(h.trace) > traceLimit {
		copy(h.trace, h.trace[len(h.trace)-traceLimit:])
		h.trace = h.trace[:traceLimit]
	}
}

func (h *Hierarchy) appendRejectLocked(op string, p *Page, err error) {
	switch {
	case errors.Is(err, ErrPageTooLarge):
		h.metrics.PageTooLargeRejections++
	case errors.Is(err, ErrPinnedCapacity):
		h.metrics.PinCapacityRejections++
	case errors.Is(err, ErrCapacity):
		h.metrics.CapacityRejections++
	case errors.Is(err, ErrTupleMismatch):
		h.metrics.TupleRejections++
	}
	h.appendTraceLocked(op, p, "", rejectionReason(err))
}

func rejectionReason(err error) string {
	switch {
	case errors.Is(err, ErrPageTooLarge):
		return "page_too_large"
	case errors.Is(err, ErrPinnedCapacity):
		return "pinned_capacity"
	case errors.Is(err, ErrCapacity):
		return "capacity"
	case errors.Is(err, ErrTupleMismatch):
		return "tuple_mismatch"
	case errors.Is(err, ErrNotResident):
		return "not_resident"
	case errors.Is(err, ErrInvalidPage):
		return "invalid_page"
	case errors.Is(err, ErrInvalidPolicy):
		return "invalid_policy"
	default:
		return "rejected"
	}
}

func (h *Hierarchy) advanceLocked() {
	h.tick++
}

func pageSize(p *Page) int {
	if p == nil {
		return 0
	}
	n := len(p.ID) + len(p.SessionID) + len(p.SourceDigest) + len(p.CompilerVersion) +
		len(p.EmbeddingNamespace) + len(p.Branch) + len(p.Commit) + len(string(p.Scope)) +
		len(p.Summary) + len(p.Body)
	for _, ref := range p.Refs {
		n += len(ref)
	}
	for _, path := range p.Paths {
		n += len(path)
	}
	return n
}

func hasMaterialization(p *Page) bool {
	return p != nil && (p.Body != "" || p.Summary != "")
}

func pageCurrentMatches(resident, expected *Page) bool {
	if resident == nil || expected == nil {
		return false
	}
	if expected.PageVersion != 0 || expected.SessionID != "" || expected.SourceDigest != "" ||
		expected.CompilerVersion != "" || expected.ScopeEpoch != 0 {
		if resident.Tuple() != expected.Tuple() {
			return false
		}
	}
	if resident.Body != expected.Body || resident.Summary != expected.Summary ||
		resident.Scope != expected.Scope || resident.Branch != expected.Branch ||
		resident.Commit != expected.Commit {
		return false
	}
	return stringSlicesEqual(resident.Refs, expected.Refs) && stringSlicesEqual(resident.Paths, expected.Paths)
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
