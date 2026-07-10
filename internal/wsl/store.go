package wsl

import (
	"fmt"
	"reflect"
	"sync"

	wsmserrors "wsms/internal/errors"
	"wsms/internal/types"
)

// WorkingState is the in-memory WSL store.
type WorkingState struct {
	mu           sync.RWMutex
	byID         map[string]Record
	order        []string // insertion order for stable serialize
	eventOK      map[string]bool
	evidenceByID map[string]string // derived record id -> durable event id
}

// NewWorkingState creates an empty state.
func NewWorkingState() *WorkingState {
	return &WorkingState{
		byID:         map[string]Record{},
		eventOK:      map[string]bool{},
		evidenceByID: map[string]string{},
	}
}

// NoteEvent marks an event id as a valid ref target for lint.
func (s *WorkingState) NoteEvent(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventOK[id] = true
}

// Apply upserts a record after lint. Rejects on error-severity issues.
func (s *WorkingState) Apply(rec Record) error {
	return s.applyUpdates([]Update{{Op: "upsert", Record: rec}}, false)
}

// ApplyAll atomically applies records in order.
func (s *WorkingState) ApplyAll(recs []Record) error {
	updates := make([]Update, 0, len(recs))
	for _, rec := range recs {
		updates = append(updates, Update{Op: "upsert", Record: rec})
	}
	return s.applyUpdates(updates, false)
}

// ApplyUpdate applies one observer-derived update. Derived updates must identify
// a noted durable event as their evidence.
func (s *WorkingState) ApplyUpdate(u Update) error {
	return s.ApplyDerivedUpdates([]Update{u})
}

// ApplyUpdates applies observer-derived updates. It is retained as the batch
// observer API and delegates to the provenance-enforcing path.
func (s *WorkingState) ApplyUpdates(updates []Update) error {
	return s.ApplyDerivedUpdates(updates)
}

// ApplyDerivedUpdates validates an observer batch against a cloned candidate
// and commits it only when every update has a non-empty, noted evidence event.
func (s *WorkingState) ApplyDerivedUpdates(updates []Update) error {
	return s.applyUpdates(updates, true)
}

func (s *WorkingState) applyUpdates(updates []Update, requireEvidence bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if requireEvidence {
		knownBefore := s.knownIDsUnlocked()
		for _, update := range updates {
			if isNilRecord(update.Record) {
				continue
			}
			avoid, ok := update.Record.(*AvoidRecord)
			if !ok || avoid.Ref == "" || refExists(avoid.Ref, knownBefore) {
				continue
			}
			return &wsmserrors.LintError{Issues: []wsmserrors.LintIssue{{
				Severity: "error",
				Code:     "dangling_avoid_ref",
				Message:  fmt.Sprintf("avoid %s refs non-preexisting %s", avoid.IDValue, avoid.Ref),
				RecordID: avoid.IDValue,
			}}}
		}
	}

	candidate := s.cloneUnlocked()
	for _, update := range updates {
		if err := candidate.applyUpdateUnlocked(update, requireEvidence); err != nil {
			return err
		}
	}
	s.commitCandidateUnlocked(candidate)
	return nil
}

func (s *WorkingState) applyUpdateUnlocked(update Update, requireEvidence bool) error {
	if update.Op != "upsert" {
		return fmt.Errorf("unsupported update operation %q", update.Op)
	}
	if isNilRecord(update.Record) {
		return fmt.Errorf("nil record in update")
	}
	if requireEvidence && update.EvidenceID == "" {
		return fmt.Errorf("derived update evidence event is required")
	}
	if update.EvidenceID != "" && !s.eventOK[update.EvidenceID] {
		return fmt.Errorf("evidence event %s was not noted", update.EvidenceID)
	}
	issues := lintApplyUnlocked(s, update.Record)
	if HasError(issues) {
		return &wsmserrors.LintError{Issues: issues}
	}
	s.upsertUnchecked(update.Record)
	if update.EvidenceID != "" {
		s.evidenceByID[update.Record.ID()] = update.EvidenceID
	} else {
		// Trusted static replacement is deliberately provenance-free. Do not
		// retain evidence that described an older derived value at this ID.
		delete(s.evidenceByID, update.Record.ID())
	}
	return nil
}

func isNilRecord(rec Record) bool {
	if rec == nil {
		return true
	}
	v := reflect.ValueOf(rec)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (s *WorkingState) commitCandidateUnlocked(candidate *WorkingState) {
	s.byID = candidate.byID
	s.order = candidate.order
	s.eventOK = candidate.eventOK
	s.evidenceByID = candidate.evidenceByID
}

func (s *WorkingState) upsertUnchecked(rec Record) {
	id := rec.ID()
	if _, ok := s.byID[id]; !ok {
		s.order = append(s.order, id)
	}
	s.byID[id] = rec.Clone()
}

// Get returns a record by id.
func (s *WorkingState) Get(id string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	return r.Clone(), true
}

// Records returns all records in insertion order.
func (s *WorkingState) Records() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.order))
	for _, id := range s.order {
		if r, ok := s.byID[id]; ok {
			out = append(out, r.Clone())
		}
	}
	return out
}

// Clone deep-copies state.
func (s *WorkingState) Clone() *WorkingState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cloneUnlocked()
}

func (s *WorkingState) cloneUnlocked() *WorkingState {
	c := NewWorkingState()
	for _, id := range s.order {
		if r, ok := s.byID[id]; ok {
			c.upsertUnchecked(r)
		}
	}
	for k, v := range s.eventOK {
		c.eventOK[k] = v
	}
	for recordID, evidenceID := range s.evidenceByID {
		c.evidenceByID[recordID] = evidenceID
	}
	return c
}

// EvidenceID returns the durable event that most recently derived recordID.
func (s *WorkingState) EvidenceID(recordID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	evidenceID, ok := s.evidenceByID[recordID]
	return evidenceID, ok
}

// Provenance returns a copy of the derived-record-to-event index.
func (s *WorkingState) Provenance() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.evidenceByID))
	for recordID, evidenceID := range s.evidenceByID {
		out[recordID] = evidenceID
	}
	return out
}

// KnownIDs returns record ids (+ noted events).
func (s *WorkingState) KnownIDs() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.knownIDsUnlocked()
}

func (s *WorkingState) knownIDsUnlocked() map[string]bool {
	m := map[string]bool{}
	for id := range s.byID {
		m[id] = true
	}
	for id := range s.eventOK {
		m[id] = true
	}
	return m
}

// ActiveTask returns the most recently inserted task or nil.
func (s *WorkingState) ActiveTask() *TaskRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeTaskUnlocked()
}

func (s *WorkingState) activeTaskUnlocked() *TaskRecord {
	for i := len(s.order) - 1; i >= 0; i-- {
		id := s.order[i]
		if r, ok := s.byID[id]; ok {
			if t, ok := r.(*TaskRecord); ok {
				return t.Clone().(*TaskRecord)
			}
		}
	}
	return nil
}

// HardConstraints returns hard constraints.
func (s *WorkingState) HardConstraints() []*ConstraintRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.constraintsUnlocked(types.StrengthHard)
}

// SoftConstraints returns soft constraints.
func (s *WorkingState) SoftConstraints() []*ConstraintRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.constraintsUnlocked(types.StrengthSoft)
}

func (s *WorkingState) constraintsUnlocked(str types.Strength) []*ConstraintRecord {
	var out []*ConstraintRecord
	for _, id := range s.order {
		if r, ok := s.byID[id]; ok {
			if c, ok := r.(*ConstraintRecord); ok && c.Strength == str {
				out = append(out, c.Clone().(*ConstraintRecord))
			}
		}
	}
	return out
}

// LastFailure returns the last failure record in order.
func (s *WorkingState) LastFailure() *FailureRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var last *FailureRecord
	for _, id := range s.order {
		if r, ok := s.byID[id]; ok {
			if f, ok := r.(*FailureRecord); ok {
				last = f.Clone().(*FailureRecord)
			}
		}
	}
	return last
}

// Avoids returns avoid records.
func (s *WorkingState) Avoids() []*AvoidRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.avoidsUnlocked()
}

func (s *WorkingState) avoidsUnlocked() []*AvoidRecord {
	var out []*AvoidRecord
	for _, id := range s.order {
		if r, ok := s.byID[id]; ok {
			if a, ok := r.(*AvoidRecord); ok {
				out = append(out, a.Clone().(*AvoidRecord))
			}
		}
	}
	return out
}

// Decisions returns decision records.
func (s *WorkingState) Decisions() []*DecisionRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.decisionsUnlocked()
}

func (s *WorkingState) decisionsUnlocked() []*DecisionRecord {
	var out []*DecisionRecord
	for _, id := range s.order {
		if r, ok := s.byID[id]; ok {
			if d, ok := r.(*DecisionRecord); ok {
				out = append(out, d.Clone().(*DecisionRecord))
			}
		}
	}
	return out
}

// Next returns the next-step record if any.
func (s *WorkingState) Next() *NextRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.byID["next"]; ok {
		return r.Clone().(*NextRecord)
	}
	return nil
}

// Pages returns page records.
func (s *WorkingState) Pages() []*PageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pagesUnlocked()
}

func (s *WorkingState) pagesUnlocked() []*PageRecord {
	var out []*PageRecord
	for _, id := range s.order {
		if r, ok := s.byID[id]; ok {
			if p, ok := r.(*PageRecord); ok {
				out = append(out, p.Clone().(*PageRecord))
			}
		}
	}
	return out
}

// Invalidated returns invalidated records.
func (s *WorkingState) Invalidated() []*InvalidatedRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.invalidatedUnlocked()
}

func (s *WorkingState) invalidatedUnlocked() []*InvalidatedRecord {
	var out []*InvalidatedRecord
	for _, id := range s.order {
		if r, ok := s.byID[id]; ok {
			if inv, ok := r.(*InvalidatedRecord); ok {
				out = append(out, inv.Clone().(*InvalidatedRecord))
			}
		}
	}
	return out
}

// FailureByID looks up a failure.
func (s *WorkingState) FailureByID(id string) *FailureRecord {
	r, ok := s.Get(id)
	if !ok {
		return nil
	}
	f, _ := r.(*FailureRecord)
	return f
}
