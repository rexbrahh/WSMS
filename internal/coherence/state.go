// Package coherence owns the authoritative, replay-derived scope and
// invalidation sidecar for one session. L4 evidence remains immutable; this
// package only decides whether an address is eligible for L1-L3 reuse.
package coherence

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"wsms/internal/ledger"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

var (
	// ErrTransition reports a coherence event that does not match the current
	// session-local scope or attempts an invalid state transition.
	ErrTransition = errors.New("invalid coherence transition")
	// ErrStaleRevision reports a failed compare-and-swap revalidation.
	ErrStaleRevision = errors.New("stale revision mismatch")
)

// Status is the authoritative eligibility state of a derived address.
type Status string

const (
	StatusActive      Status = "active"
	StatusStale       Status = "stale"
	StatusInvalidated Status = "invalidated"
)

// Scope is the current post-transition repository scope.
type Scope struct {
	Repo   string
	TaskID string
	Branch string
	Commit string
}

// Binding is the immutable-address sidecar used by residency gates.
type Binding struct {
	Kind            ledger.MemoryTargetKind
	ID              string
	Scope           types.Scope
	Repo            string
	TaskID          string
	Branch          string
	Commit          string
	Paths           []string
	SourceDigest    string
	Status          Status
	StaleRevision   uint64
	InvalidReason   ledger.InvalidationReason
	EvidenceEventID string
	Refs            []string
	RepoEpoch       uint64
	BranchEpoch     uint64
	CommitEpoch     uint64
	PathEpochs      map[string]uint64
}

// PathBinding is the current status of one canonical repository path.
type PathBinding struct {
	Path          string
	Repo          string
	Branch        string
	Commit        string
	SourceDigest  string
	Status        Status
	StaleRevision uint64
	InvalidReason ledger.InvalidationReason
	Epoch         uint64
	EvidenceID    string
}

// Snapshot is an independent read-only value suitable for diagnostics/tests.
type Snapshot struct {
	Revision     uint64
	EpochClock   uint64
	Current      Scope
	RepoEpochs   map[string]uint64
	BranchEpochs map[string]uint64
	CommitEpochs map[string]uint64
	PathEpochs   map[string]uint64
	Bindings     map[string]Binding
	Paths        map[string]PathBinding
}

// State serializes committed coherence snapshots. Prepare is pure; Commit is
// the only mutation point and uses Revision as a compare-and-swap guard.
type State struct {
	mu       sync.RWMutex
	snapshot Snapshot
}

// Candidate is one uncommitted transition. Scheduler binds observer outputs to
// this candidate, commits WSL, then installs the candidate.
type Candidate struct {
	baseRevision uint64
	event        ledger.Event
	next         Snapshot
}

// NewState creates an empty per-session coherence state.
func NewState() *State {
	return &State{snapshot: emptySnapshot()}
}

func emptySnapshot() Snapshot {
	return Snapshot{
		RepoEpochs:   map[string]uint64{},
		BranchEpochs: map[string]uint64{},
		CommitEpochs: map[string]uint64{},
		PathEpochs:   map[string]uint64{},
		Bindings:     map[string]Binding{},
		Paths:        map[string]PathBinding{},
	}
}

// Snapshot returns a deep copy of the committed state.
func (s *State) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSnapshot(s.snapshot)
}

// Prepare validates ev against a clone and returns an uncommitted transition.
func (s *State) Prepare(ev ledger.Event) (*Candidate, error) {
	s.mu.RLock()
	base := cloneSnapshot(s.snapshot)
	s.mu.RUnlock()

	if ev.ID == "" {
		return nil, fmt.Errorf("%w: durable event id is required", ErrTransition)
	}
	c := &Candidate{baseRevision: base.Revision, event: ev, next: base}
	if err := c.applyEvent(); err != nil {
		return nil, err
	}
	// Every durable event is addressable, but only after the whole candidate
	// commits. Contextual transitions therefore cannot cite themselves.
	c.bindEvent(ev)
	return c, nil
}

// BindUpdates adds observer-derived addresses to the uncommitted candidate.
func (c *Candidate) BindUpdates(updates []wsl.Update) error {
	if c == nil {
		return fmt.Errorf("%w: nil candidate", ErrTransition)
	}
	// The common harness path lets the observer allocate T1. Establish that
	// logical task identity before binding any same-event records.
	if c.event.Type == ledger.EventTaskStarted && c.next.Current.TaskID == "" {
		for _, update := range updates {
			if task, ok := update.Record.(*wsl.TaskRecord); ok {
				c.next.Current.TaskID = task.IDValue
				eventKey := bindingKey(ledger.TargetEvent, c.event.ID)
				if eventBinding, found := c.next.Bindings[eventKey]; found {
					eventBinding.TaskID = task.IDValue
					c.next.Bindings[eventKey] = eventBinding
				}
				break
			}
		}
	}
	for _, update := range updates {
		if update.Record == nil {
			continue
		}
		binding, ok, err := bindingForRecord(c.event, c.next, update.Record)
		if err != nil {
			return err
		}
		if ok {
			c.next.Bindings[bindingKey(binding.Kind, binding.ID)] = cloneBinding(binding)
		}
	}
	return nil
}

// Snapshot returns a deep copy of the uncommitted candidate for hierarchy
// reconciliation after commit.
func (c *Candidate) Snapshot() Snapshot {
	if c == nil {
		return emptySnapshot()
	}
	return cloneSnapshot(c.next)
}

// Commit installs a candidate if no other transition has intervened.
func (s *State) Commit(c *Candidate) error {
	if c == nil {
		return fmt.Errorf("%w: nil candidate", ErrTransition)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Revision != c.baseRevision {
		return fmt.Errorf("%w: candidate base %d, current %d", ErrTransition, c.baseRevision, s.snapshot.Revision)
	}
	next := cloneSnapshot(c.next)
	next.Revision = s.snapshot.Revision + 1
	s.snapshot = next
	return nil
}

// WithCandidate serializes the only foreground transition path. fn may update
// WSL using the candidate; returning an error leaves coherence unchanged. Once
// fn succeeds, installation cannot lose a CAS race, so WSL and coherence cannot
// diverge through an externally interleaved Prepare/Commit.
func (s *State) WithCandidate(ev ledger.Event, fn func(*Candidate) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := cloneSnapshot(s.snapshot)
	if ev.ID == "" {
		return fmt.Errorf("%w: durable event id is required", ErrTransition)
	}
	c := &Candidate{baseRevision: base.Revision, event: ev, next: base}
	if err := c.applyEvent(); err != nil {
		return err
	}
	c.bindEvent(ev)
	if err := fn(c); err != nil {
		return err
	}
	next := cloneSnapshot(c.next)
	next.Revision = s.snapshot.Revision + 1
	s.snapshot = next
	return nil
}

// ChangesResidency reports whether this event changes active scope/status and
// therefore invalidates the previously rendered L1 capsule.
func ChangesResidency(eventType ledger.EventType) bool {
	switch eventType {
	case ledger.EventTaskStarted, ledger.EventBranchChange, ledger.EventCommitChange,
		ledger.EventFileSnapshot, ledger.EventFileRenamed,
		ledger.EventMemoryInvalidated, ledger.EventMemoryRevalidated:
		return true
	default:
		return false
	}
}

func (c *Candidate) applyEvent() error {
	snap := &c.next
	ev := c.event
	switch ev.Type {
	case ledger.EventTaskStarted:
		if snap.Current.Repo != "" && ev.Repo != "" && snap.Current.Repo != ev.Repo {
			markMatchingStale(snap, func(b Binding) bool { return b.Scope != types.ScopeGlobal })
		}
		if snap.Current.TaskID != "" && (ev.TaskID == "" || snap.Current.TaskID != ev.TaskID) {
			markMatchingStale(snap, func(b Binding) bool { return b.Scope == types.ScopeTask || b.Scope == types.ScopeFile })
		}
		snap.Current = Scope{Repo: ev.Repo, TaskID: ev.TaskID, Branch: effectiveTaskBranch(ev), Commit: ev.Commit}
		enterCurrentScope(snap)
		markMatchingStale(snap, func(b Binding) bool {
			return branchScoped(b) && !bindingMatchesCurrent(b, *snap)
		})
	case ledger.EventBranchChange:
		from := ev.PayloadString("from_branch")
		if err := requireCurrent(snap.Current.Repo, ev.Repo, "repo"); err != nil {
			return err
		}
		if err := requireCurrent(snap.Current.Branch, from, "from_branch"); err != nil {
			return err
		}
		if fromCommit, ok := ev.Payload["from_commit"]; ok {
			if err := requireCurrent(snap.Current.Commit, fromCommit.(string), "from_commit"); err != nil {
				return err
			}
		}
		snap.Current.Repo, snap.Current.Branch, snap.Current.Commit = ev.Repo, ev.Branch, ev.Commit
		enterBranchScope(snap)
		markMatchingStale(snap, func(b Binding) bool {
			return branchScoped(b) && b.Repo == ev.Repo && !bindingMatchesCurrent(b, *snap)
		})
	case ledger.EventCommitChange:
		if err := requireCurrent(snap.Current.Repo, ev.Repo, "repo"); err != nil {
			return err
		}
		if err := requireCurrent(snap.Current.Branch, ev.Branch, "branch"); err != nil {
			return err
		}
		if err := requireCurrent(snap.Current.Commit, ev.PayloadString("from_commit"), "from_commit"); err != nil {
			return err
		}
		snap.Current.Commit = ev.Commit
		enterCommitScope(snap)
		for key, p := range snap.Paths {
			if p.Status == StatusActive && p.Repo == ev.Repo && p.Branch == ev.Branch && p.Commit != ev.Commit {
				p.Status = StatusStale
				p.StaleRevision = nextStaleRevision(p.StaleRevision)
				snap.Paths[key] = p
			}
		}
		markMatchingStale(snap, func(b Binding) bool {
			return branchScoped(b) && b.Repo == ev.Repo && b.Branch == ev.Branch && !bindingMatchesCurrent(b, *snap)
		})
	case ledger.EventFileSnapshot:
		if err := requireEnvelopeCurrent(*snap, ev); err != nil {
			return err
		}
		p := ev.PayloadString("path")
		if existing, ok := snap.Paths[pathKey(ev.Repo, ev.Branch, p)]; ok && existing.Status == StatusInvalidated {
			return fmt.Errorf("%w: path %s is terminally invalidated", ErrTransition, p)
		}
		existing, existed := snap.Paths[pathKey(ev.Repo, ev.Branch, p)]
		previousEpoch := snap.PathEpochs[pathKey(ev.Repo, ev.Branch, p)]
		epoch := ensurePathEpoch(snap, ev.Repo, ev.Branch, p)
		if existed && existing.SourceDigest != ev.PayloadString("content_digest") {
			epoch = bumpPathEpoch(snap, ev.Repo, ev.Branch, p)
		}
		// A first authoritative snapshot also advances bindings captured before
		// the path had a generation. Never leave such a binding status=active but
		// ineligible, because that state cannot be CAS-revalidated.
		if epoch != previousEpoch {
			markMatchingStale(snap, func(b Binding) bool {
				if b.Repo != ev.Repo || b.Branch != ev.Branch {
					return false
				}
				for _, boundPath := range b.Paths {
					if pathWithin(boundPath, p) || pathWithin(p, boundPath) {
						return true
					}
				}
				return false
			})
		}
		snap.Paths[pathKey(ev.Repo, ev.Branch, p)] = PathBinding{
			Path: p, Repo: ev.Repo, Branch: ev.Branch, Commit: ev.Commit,
			SourceDigest: ev.PayloadString("content_digest"), Status: StatusActive,
			Epoch: epoch, EvidenceID: ev.ID,
		}
	case ledger.EventFileRenamed:
		if err := requireRepoBranchCurrent(*snap, ev); err != nil {
			return err
		}
		from, to := ev.PayloadString("from_path"), ev.PayloadString("to_path")
		fromKey, toKey := pathKey(ev.Repo, ev.Branch, from), pathKey(ev.Repo, ev.Branch, to)
		fromState, fromExists := snap.Paths[fromKey]
		if fromExists && fromState.Status == StatusInvalidated {
			return fmt.Errorf("%w: renamed source %s is terminally invalidated", ErrTransition, from)
		}
		toState, toExists := snap.Paths[toKey]
		if toExists && toState.Status == StatusInvalidated {
			return fmt.Errorf("%w: rename destination %s is terminally invalidated", ErrTransition, to)
		}
		// A rename replaces both address ranges. Advance every known descendant
		// once, preserving terminal descendants, then install only the exact new
		// destination as active. Old addresses are never silently retargeted.
		affected := map[string]string{fromKey: from, toKey: to}
		for key, pathState := range snap.Paths {
			if pathState.Repo == ev.Repo && pathState.Branch == ev.Branch &&
				(pathsOverlap(pathState.Path, from) || pathsOverlap(pathState.Path, to)) {
				affected[key] = pathState.Path
			}
		}
		affectedKeys := make([]string, 0, len(affected))
		for key := range affected {
			affectedKeys = append(affectedKeys, key)
		}
		sort.Strings(affectedKeys)
		for _, key := range affectedKeys {
			affectedPath := affected[key]
			pathState, exists := snap.Paths[key]
			if exists && pathState.Status == StatusInvalidated {
				continue
			}
			epoch := bumpPathEpoch(snap, ev.Repo, ev.Branch, affectedPath)
			pathState.Path, pathState.Repo, pathState.Branch = affectedPath, ev.Repo, ev.Branch
			pathState.Status, pathState.Epoch = StatusStale, epoch
			pathState.StaleRevision = nextStaleRevision(pathState.StaleRevision)
			pathState.EvidenceID = ev.ID
			snap.Paths[key] = pathState
		}
		toEpoch := snap.PathEpochs[toKey]
		snap.Paths[toKey] = PathBinding{
			Path: to, Repo: ev.Repo, Branch: ev.Branch, Commit: snap.Current.Commit,
			SourceDigest: ev.PayloadString("content_digest"), Status: StatusActive,
			Epoch: toEpoch, EvidenceID: ev.ID,
		}
		markMatchingStale(snap, func(b Binding) bool {
			if b.Repo != ev.Repo || b.Branch != ev.Branch {
				return false
			}
			for _, p := range b.Paths {
				if pathsOverlap(p, from) || pathsOverlap(p, to) {
					return true
				}
			}
			return false
		})
	case ledger.EventMemoryInvalidated:
		kind := ledger.MemoryTargetKind(ev.PayloadString("target_kind"))
		target := ev.PayloadString("target")
		reason := ledger.InvalidationReason(ev.PayloadString("reason"))
		if kind == ledger.TargetPath {
			key := pathKey(snap.Current.Repo, snap.Current.Branch, target)
			p, ok := snap.Paths[key]
			if !ok {
				return fmt.Errorf("%w: unknown path target %s", ErrTransition, target)
			}
			if p.Status == StatusInvalidated {
				return fmt.Errorf("%w: path target %s is terminally invalidated", ErrTransition, target)
			}
			p.Status, p.InvalidReason = StatusInvalidated, reason
			p.StaleRevision = nextStaleRevision(p.StaleRevision)
			p.EvidenceID = ev.ID
			snap.Paths[key] = p
			markMatchingInvalidated(snap, reason, func(b Binding) bool {
				for _, p := range b.Paths {
					if pathWithin(p, target) {
						return true
					}
				}
				return false
			})
			break
		}
		key := bindingKey(kind, target)
		b, ok := snap.Bindings[key]
		if !ok {
			return fmt.Errorf("%w: unknown %s target %s", ErrTransition, kind, target)
		}
		if b.Status == StatusInvalidated {
			return fmt.Errorf("%w: %s target %s is terminally invalidated", ErrTransition, kind, target)
		}
		b.Status, b.InvalidReason = StatusInvalidated, reason
		b.StaleRevision = nextStaleRevision(b.StaleRevision)
		snap.Bindings[key] = b
		cascadeInvalidation(snap, target, reason)
	case ledger.EventMemoryRevalidated:
		if err := c.applyRevalidation(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Candidate) applyRevalidation() error {
	snap, ev := &c.next, c.event
	kind := ledger.MemoryTargetKind(ev.PayloadString("target_kind"))
	target := ev.PayloadString("target")
	expected := uint64(ev.PayloadInt("expected_stale_revision", 0))
	evidenceRef := ev.PayloadString("evidence_ref")
	if !eligibleEvidence(*snap, evidenceRef) {
		return fmt.Errorf("%w: evidence_ref %s is not a preexisting eligible address", ErrTransition, evidenceRef)
	}
	if kind == ledger.TargetPath {
		key := pathKey(snap.Current.Repo, snap.Current.Branch, target)
		p, ok := snap.Paths[key]
		if !ok || p.Status != StatusStale {
			return fmt.Errorf("%w: path %s is not stale", ErrTransition, target)
		}
		if p.StaleRevision != expected {
			return fmt.Errorf("%w: target %s expected %d, current %d", ErrStaleRevision, target, expected, p.StaleRevision)
		}
		if p.SourceDigest != "" && p.SourceDigest != ev.PayloadString("source_digest") {
			return fmt.Errorf("%w: source digest changed for %s", ErrTransition, target)
		}
		p.Status, p.InvalidReason, p.EvidenceID = StatusActive, "", ev.ID
		p.Commit = snap.Current.Commit
		p.Epoch = ensurePathEpoch(snap, snap.Current.Repo, snap.Current.Branch, target)
		snap.Paths[key] = p
		return nil
	}
	key := bindingKey(kind, target)
	b, ok := snap.Bindings[key]
	if !ok || b.Status != StatusStale {
		return fmt.Errorf("%w: %s %s is not stale", ErrTransition, kind, target)
	}
	if b.StaleRevision != expected {
		return fmt.Errorf("%w: target %s expected %d, current %d", ErrStaleRevision, target, expected, b.StaleRevision)
	}
	if kind == ledger.TargetPage && b.SourceDigest != "" && b.SourceDigest != ev.PayloadString("source_digest") {
		return fmt.Errorf("%w: source digest changed for %s", ErrTransition, target)
	}
	// Revalidation never rewrites path addresses. A renamed/deleted path must be
	// represented by a new version rather than silently retargeted.
	for _, p := range b.Paths {
		pathState, ok := snap.Paths[pathKey(snap.Current.Repo, snap.Current.Branch, p)]
		if !ok || pathState.Status != StatusActive {
			return fmt.Errorf("%w: target %s still depends on stale path %s", ErrTransition, target, p)
		}
	}
	// Revalidation changes eligibility, not immutable derivation provenance.
	// Raw faults still resolve through the original evidence event.
	b.Status, b.InvalidReason = StatusActive, ""
	commitBound := b.Commit != ""
	b.Repo, b.TaskID = snap.Current.Repo, snap.Current.TaskID
	switch b.Scope {
	case types.ScopeGlobal:
		b.Repo, b.TaskID, b.Branch, b.Commit = "", "", "", ""
	case types.ScopeRepo:
		b.Branch, b.Commit = "", ""
	case types.ScopeBranch:
		b.Branch = snap.Current.Branch
		if commitBound {
			b.Commit = snap.Current.Commit
		} else {
			b.Commit = ""
		}
	default:
		b.Branch, b.Commit = snap.Current.Branch, snap.Current.Commit
	}
	captureEpochs(&b, *snap)
	snap.Bindings[key] = b
	return nil
}

// RecordEligible applies the authoritative gate used by capsule rendering and
// WSL/page-fault fallback. Unbound derived addresses fail closed.
func (s *State) RecordEligible(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, kind := range []ledger.MemoryTargetKind{ledger.TargetRecord, ledger.TargetPage} {
		if b, ok := s.snapshot.Bindings[bindingKey(kind, id)]; ok {
			return bindingEligible(b, s.snapshot)
		}
	}
	return false
}

// AddressStatus returns the current status and stale CAS revision.
func (s *State) AddressStatus(kind ledger.MemoryTargetKind, target string) (Status, uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if kind == ledger.TargetPath {
		p, ok := s.snapshot.Paths[pathKey(s.snapshot.Current.Repo, s.snapshot.Current.Branch, target)]
		return p.Status, p.StaleRevision, ok
	}
	b, ok := s.snapshot.Bindings[bindingKey(kind, target)]
	return b.Status, b.StaleRevision, ok
}

// HasTarget reports whether target is already bound in this session.
func (s *State) HasTarget(kind ledger.MemoryTargetKind, target string) bool {
	_, _, ok := s.AddressStatus(kind, target)
	return ok
}

// RawAllowed enforces only revocations that can withdraw diagnostic access.
// Ordinary stale/superseded evidence remains readable from L4.
func (s *State) RawAllowed(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	deny := func(reason ledger.InvalidationReason) bool {
		return reason == ledger.ReasonSecurityRevoked || reason == ledger.ReasonPolicyChanged
	}
	for _, kind := range []ledger.MemoryTargetKind{ledger.TargetRecord, ledger.TargetEvent, ledger.TargetPage} {
		if b, ok := s.snapshot.Bindings[bindingKey(kind, id)]; ok {
			if deny(b.InvalidReason) {
				return false
			}
			if source, found := s.snapshot.Bindings[bindingKey(ledger.TargetEvent, b.EvidenceEventID)]; found && deny(source.InvalidReason) {
				return false
			}
		}
	}
	// A failure's raw bytes can also be requested through its evidence event.
	// Revocation therefore follows the provenance edge in both directions.
	for _, b := range s.snapshot.Bindings {
		if b.EvidenceEventID == id && deny(b.InvalidReason) {
			return false
		}
	}
	return true
}

// PageStatus evaluates an L2/L3 descriptor without importing memory package
// types into the coherence boundary.
func (s *State) PageStatus(id string, refs []string, scope types.Scope, branch, commit string, paths []string, sourceDigest string, epoch uint64) (Status, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status, revision := pageStatus(s.snapshot, id, refs, scope, branch, commit, paths, sourceDigest, epoch)
	return status, revision
}

// BindingFor returns a cloned current sidecar binding.
func (s *State) BindingFor(kind ledger.MemoryTargetKind, id string) (Binding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.snapshot.Bindings[bindingKey(kind, id)]
	return cloneBinding(b), ok
}

func (c *Candidate) bindEvent(ev ledger.Event) {
	b := Binding{
		Kind: ledger.TargetEvent, ID: ev.ID, Scope: eventScope(ev),
		Repo: firstNonEmpty(ev.Repo, c.next.Current.Repo), TaskID: firstNonEmpty(ev.TaskID, c.next.Current.TaskID),
		Branch: firstNonEmpty(ev.Branch, c.next.Current.Branch), Commit: firstNonEmpty(ev.Commit, c.next.Current.Commit),
		Status: StatusActive, EvidenceEventID: ev.ID,
	}
	if ev.Type == ledger.EventFileSnapshot {
		b.Paths = []string{ev.PayloadString("path")}
		b.SourceDigest = ev.PayloadString("content_digest")
	}
	captureEpochs(&b, c.next)
	c.next.Bindings[bindingKey(b.Kind, b.ID)] = b
}

func bindingForRecord(ev ledger.Event, snap Snapshot, rec wsl.Record) (Binding, bool, error) {
	b := Binding{
		Kind: ledger.TargetRecord, ID: rec.ID(), Scope: types.ScopeTask,
		Repo: firstNonEmpty(ev.Repo, snap.Current.Repo), TaskID: firstNonEmpty(ev.TaskID, snap.Current.TaskID),
		Branch: firstNonEmpty(ev.Branch, snap.Current.Branch), Commit: firstNonEmpty(ev.Commit, snap.Current.Commit),
		Status: StatusActive, EvidenceEventID: ev.ID,
	}
	switch r := rec.(type) {
	case *wsl.ConstraintRecord:
		b.Scope = r.Scope
	case *wsl.FailureRecord:
		b.Scope = types.ScopeBranch
		if p := normalizeFileHint(r.FileHint); p != "" {
			b.Paths = []string{p}
		}
	case *wsl.DecisionRecord:
		b.Scope = r.Scope
	case *wsl.PageRecord:
		b.Kind, b.Scope = ledger.TargetPage, r.Scope
		b.Branch = firstNonEmpty(r.Branch, b.Branch)
	case *wsl.TaskRecord:
		b.Scope, b.Branch, b.Commit = types.ScopeTask, r.Branch, r.Commit
	case *wsl.AvoidRecord:
		b.Scope = types.ScopeTask
		if ref, found := snap.Bindings[bindingKey(ledger.TargetRecord, r.Ref)]; found {
			grounding := cloneBinding(ref)
			grounding.Kind, grounding.ID = ledger.TargetRecord, r.IDValue
			grounding.EvidenceEventID = ev.ID
			grounding.Refs = []string{r.Ref}
			b = grounding
		}
	case *wsl.InvalidatedRecord, *wsl.NextRecord, *wsl.AssumptionRecord:
		b.Scope = types.ScopeTask
	default:
		return Binding{}, false, nil
	}
	if b.Scope == "" {
		b.Scope = types.ScopeTask
	}
	// A true branch/repo fact survives commit changes. Commit-bound operational
	// evidence (failures and file scope) deliberately retains Commit.
	switch rec.(type) {
	case *wsl.ConstraintRecord, *wsl.DecisionRecord, *wsl.PageRecord:
		if b.Scope == types.ScopeBranch || b.Scope == types.ScopeRepo || b.Scope == types.ScopeGlobal {
			b.Commit = ""
		}
	}
	if b.Scope == types.ScopeGlobal {
		b.Repo, b.TaskID, b.Branch, b.Commit = "", "", "", ""
		b.Paths = nil
	}
	captureEpochs(&b, snap)
	return b, true, nil
}

func eventScope(ev ledger.Event) types.Scope {
	switch ev.Type {
	case ledger.EventFileSnapshot, ledger.EventFileRead, ledger.EventFileWrite, ledger.EventFileRenamed:
		return types.ScopeFile
	case ledger.EventCommandRun, ledger.EventCommandOutput, ledger.EventTestResult, ledger.EventToolCall, ledger.EventToolResult, ledger.EventGitDiff:
		return types.ScopeBranch
	default:
		return types.ScopeTask
	}
}

func pageStatus(snap Snapshot, id string, refs []string, scope types.Scope, branch, commit string, paths []string, sourceDigest string, epoch uint64) (Status, uint64) {
	var direct *Binding
	for _, kind := range []ledger.MemoryTargetKind{ledger.TargetRecord, ledger.TargetPage} {
		if b, ok := snap.Bindings[bindingKey(kind, id)]; ok {
			copy := b
			direct = &copy
			if !bindingEligible(b, snap) {
				return ineligibleStatus(b)
			}
			break
		}
	}
	for _, ref := range refs {
		found := false
		for _, kind := range []ledger.MemoryTargetKind{ledger.TargetRecord, ledger.TargetEvent, ledger.TargetPage} {
			if b, ok := snap.Bindings[bindingKey(kind, ref)]; ok {
				found = true
				if !bindingEligible(b, snap) {
					return ineligibleStatus(b)
				}
			}
		}
		if !found {
			return StatusStale, 1
		}
	}
	if direct != nil {
		if !pageDescriptorMatchesBinding(*direct, scope, branch, commit, paths, sourceDigest, epoch) {
			return StatusStale, nextStaleRevision(direct.StaleRevision)
		}
		return StatusActive, 0
	}
	if len(refs) == 0 || !pageDescriptorMatchesCurrent(snap, scope, branch, commit, paths, epoch) {
		return StatusStale, 1
	}
	return StatusActive, 0
}

func bindingEligible(b Binding, snap Snapshot) bool {
	if b.Status != StatusActive {
		return false
	}
	if b.Scope != types.ScopeGlobal && b.Repo != snap.Current.Repo {
		return false
	}
	if (b.Scope == types.ScopeTask || b.Scope == types.ScopeFile) && b.TaskID != snap.Current.TaskID {
		return false
	}
	if branchScoped(b) && !bindingMatchesCurrent(b, snap) {
		return false
	}
	for _, p := range b.Paths {
		key := pathKey(b.Repo, b.Branch, p)
		ps, ok := snap.Paths[key]
		if ok && ps.Status != StatusActive {
			return false
		}
		if b.PathEpochs[p] != snap.PathEpochs[key] {
			return false
		}
	}
	return true
}

func bindingMatchesCurrent(b Binding, snap Snapshot) bool {
	if b.Branch != snap.Current.Branch {
		return false
	}
	if b.BranchEpoch != 0 && b.BranchEpoch != snap.BranchEpochs[branchKey(b.Repo, b.Branch)] {
		return false
	}
	if b.Commit != "" && b.Commit != snap.Current.Commit {
		return false
	}
	if b.CommitEpoch != 0 && b.CommitEpoch != snap.CommitEpochs[commitKey(b.Repo, b.Branch, b.Commit)] {
		return false
	}
	return true
}

func branchScoped(b Binding) bool {
	return b.Scope == types.ScopeBranch || b.Scope == types.ScopeFile
}

// Generation is the scalar scope generation captured by a materialized page.
// Epoch values come from one session-local monotonic clock, so the newest
// relevant component is a collision-free summary until uint64 exhaustion.
func (b Binding) Generation() uint64 {
	var generation uint64
	if branchScoped(b) {
		generation = maxEpoch(generation, b.BranchEpoch)
	}
	if b.Commit != "" {
		generation = maxEpoch(generation, b.CommitEpoch)
	}
	for _, epoch := range b.PathEpochs {
		generation = maxEpoch(generation, epoch)
	}
	return generation
}

func ineligibleStatus(b Binding) (Status, uint64) {
	if b.Status == StatusInvalidated {
		return StatusInvalidated, b.StaleRevision
	}
	revision := b.StaleRevision
	if revision == 0 {
		revision = 1
	}
	return StatusStale, revision
}

func pageDescriptorMatchesBinding(b Binding, scope types.Scope, branch, commit string, paths []string, sourceDigest string, epoch uint64) bool {
	if scope != b.Scope || !slices.Equal(paths, b.Paths) || sourceDigest != b.SourceDigest || epoch != b.Generation() {
		return false
	}
	if branchScoped(b) && branch != b.Branch {
		return false
	}
	if b.Commit != "" && commit != b.Commit {
		return false
	}
	return true
}

func pageDescriptorMatchesCurrent(snap Snapshot, scope types.Scope, branch, commit string, paths []string, epoch uint64) bool {
	if scope == "" {
		return false
	}
	if (scope == types.ScopeBranch || scope == types.ScopeFile) && branch != snap.Current.Branch {
		return false
	}
	if commit != "" && commit != snap.Current.Commit {
		return false
	}
	for _, p := range paths {
		if ps, ok := snap.Paths[pathKey(snap.Current.Repo, snap.Current.Branch, p)]; ok && ps.Status != StatusActive {
			return false
		}
	}
	return epoch == currentDescriptorGeneration(snap, scope, branch, commit, paths)
}

func currentDescriptorGeneration(snap Snapshot, scope types.Scope, branch, commit string, paths []string) uint64 {
	var generation uint64
	if scope == types.ScopeBranch || scope == types.ScopeFile {
		generation = maxEpoch(generation, snap.BranchEpochs[branchKey(snap.Current.Repo, branch)])
	}
	if commit != "" {
		generation = maxEpoch(generation, snap.CommitEpochs[commitKey(snap.Current.Repo, branch, commit)])
	}
	for _, p := range paths {
		generation = maxEpoch(generation, snap.PathEpochs[pathKey(snap.Current.Repo, branch, p)])
	}
	return generation
}

func maxEpoch(left, right uint64) uint64 {
	if right > left {
		return right
	}
	return left
}

func eligibleEvidence(snap Snapshot, id string) bool {
	for _, kind := range []ledger.MemoryTargetKind{ledger.TargetRecord, ledger.TargetEvent, ledger.TargetPage} {
		if b, ok := snap.Bindings[bindingKey(kind, id)]; ok {
			return bindingEligible(b, snap) && b.InvalidReason != ledger.ReasonSecurityRevoked && b.InvalidReason != ledger.ReasonPolicyChanged
		}
	}
	return false
}

func markMatchingStale(snap *Snapshot, match func(Binding) bool) {
	for key, b := range snap.Bindings {
		if b.Status == StatusActive && match(b) {
			b.Status = StatusStale
			b.StaleRevision = nextStaleRevision(b.StaleRevision)
			snap.Bindings[key] = b
		}
	}
}

func markMatchingInvalidated(snap *Snapshot, reason ledger.InvalidationReason, match func(Binding) bool) {
	for key, b := range snap.Bindings {
		if b.Status != StatusInvalidated && match(b) {
			b.Status, b.InvalidReason = StatusInvalidated, reason
			b.StaleRevision = nextStaleRevision(b.StaleRevision)
			snap.Bindings[key] = b
		}
	}
}

func cascadeInvalidation(snap *Snapshot, target string, reason ledger.InvalidationReason) {
	queue := []string{target}
	seen := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		for key, b := range snap.Bindings {
			depends := b.EvidenceEventID == current || slices.Contains(b.Refs, current)
			if b.Status == StatusInvalidated || !depends {
				continue
			}
			b.Status, b.InvalidReason = StatusInvalidated, reason
			b.StaleRevision = nextStaleRevision(b.StaleRevision)
			snap.Bindings[key] = b
			queue = append(queue, b.ID)
		}
	}
}

func enterCurrentScope(snap *Snapshot) {
	if snap.Current.Repo != "" {
		snap.RepoEpochs[snap.Current.Repo] = nextScopeEpoch(snap)
	}
	enterBranchScope(snap)
}

func enterBranchScope(snap *Snapshot) {
	if snap.Current.Repo != "" && snap.Current.Branch != "" {
		snap.BranchEpochs[branchKey(snap.Current.Repo, snap.Current.Branch)] = nextScopeEpoch(snap)
	}
	enterCommitScope(snap)
}

func enterCommitScope(snap *Snapshot) {
	if snap.Current.Repo != "" && snap.Current.Branch != "" && snap.Current.Commit != "" {
		snap.CommitEpochs[commitKey(snap.Current.Repo, snap.Current.Branch, snap.Current.Commit)] = nextScopeEpoch(snap)
	}
}

func captureEpochs(b *Binding, snap Snapshot) {
	b.RepoEpoch = snap.RepoEpochs[b.Repo]
	b.BranchEpoch = snap.BranchEpochs[branchKey(b.Repo, b.Branch)]
	b.CommitEpoch = snap.CommitEpochs[commitKey(b.Repo, b.Branch, b.Commit)]
	b.PathEpochs = map[string]uint64{}
	for _, p := range b.Paths {
		b.PathEpochs[p] = snap.PathEpochs[pathKey(b.Repo, b.Branch, p)]
	}
}

func ensurePathEpoch(snap *Snapshot, repo, branch, p string) uint64 {
	key := pathKey(repo, branch, p)
	if snap.PathEpochs[key] == 0 {
		snap.PathEpochs[key] = nextScopeEpoch(snap)
	}
	return snap.PathEpochs[key]
}

func bumpPathEpoch(snap *Snapshot, repo, branch, p string) uint64 {
	key := pathKey(repo, branch, p)
	snap.PathEpochs[key] = nextScopeEpoch(snap)
	return snap.PathEpochs[key]
}

func nextScopeEpoch(snap *Snapshot) uint64 {
	if snap.EpochClock == ^uint64(0) {
		// Exhaustion is practically unreachable, but retaining the terminal value
		// fails closed through stale status instead of wrapping to an old token.
		return snap.EpochClock
	}
	snap.EpochClock++
	return snap.EpochClock
}

func requireEnvelopeCurrent(snap Snapshot, ev ledger.Event) error {
	if err := requireRepoBranchCurrent(snap, ev); err != nil {
		return err
	}
	return requireCurrent(snap.Current.Commit, ev.Commit, "commit")
}

func requireRepoBranchCurrent(snap Snapshot, ev ledger.Event) error {
	if err := requireCurrent(snap.Current.Repo, ev.Repo, "repo"); err != nil {
		return err
	}
	return requireCurrent(snap.Current.Branch, ev.Branch, "branch")
}

func requireCurrent(current, supplied, name string) error {
	if current == "" || current != supplied {
		return fmt.Errorf("%w: %s %q does not match current %q", ErrTransition, name, supplied, current)
	}
	return nil
}

func effectiveTaskBranch(ev ledger.Event) string {
	return firstNonEmpty(ev.Branch, ev.PayloadString("branch"))
}

func normalizeFileHint(hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return ""
	}
	if m := fileLocationRE.FindStringSubmatch(hint); m != nil {
		hint = m[1]
	}
	cleaned, err := ledger.NormalizeRepoPath(hint)
	if err != nil || cleaned != hint {
		return ""
	}
	return cleaned
}

var fileLocationRE = regexp.MustCompile(`^(.+):[0-9]+(?:-[0-9]+)?$`)

func pathWithin(candidate, root string) bool {
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

func pathsOverlap(left, right string) bool {
	return pathWithin(left, right) || pathWithin(right, left)
}

func bindingKey(kind ledger.MemoryTargetKind, id string) string {
	return string(kind) + "\x00" + id
}

func branchKey(repo, branch string) string { return repo + "\x00" + branch }
func commitKey(repo, branch, commit string) string {
	return repo + "\x00" + branch + "\x00" + commit
}
func pathKey(repo, branch, p string) string {
	return repo + "\x00" + branch + "\x00" + path.Clean(p)
}

func nextStaleRevision(current uint64) uint64 {
	if current == ^uint64(0) {
		return current
	}
	return current + 1
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func cloneSnapshot(in Snapshot) Snapshot {
	out := in
	out.RepoEpochs = cloneEpochMap(in.RepoEpochs)
	out.BranchEpochs = cloneEpochMap(in.BranchEpochs)
	out.CommitEpochs = cloneEpochMap(in.CommitEpochs)
	out.PathEpochs = cloneEpochMap(in.PathEpochs)
	out.Bindings = make(map[string]Binding, len(in.Bindings))
	for key, b := range in.Bindings {
		out.Bindings[key] = cloneBinding(b)
	}
	out.Paths = make(map[string]PathBinding, len(in.Paths))
	for key, p := range in.Paths {
		out.Paths[key] = p
	}
	return out
}

func cloneBinding(in Binding) Binding {
	out := in
	out.Paths = append([]string(nil), in.Paths...)
	out.Refs = append([]string(nil), in.Refs...)
	out.PathEpochs = cloneEpochMap(in.PathEpochs)
	return out
}

func cloneEpochMap(in map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
