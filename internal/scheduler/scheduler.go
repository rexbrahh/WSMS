// Package scheduler manages residency and safe injection boundaries.
package scheduler

import (
	"context"
	"fmt"
	"sync"

	"wsms/internal/coherence"
	"wsms/internal/config"
	"wsms/internal/faults"
	"wsms/internal/ledger"
	"wsms/internal/memory"
	"wsms/internal/observers"
	"wsms/internal/renderer"
	"wsms/internal/wsl"
)

// Scheduler is the memory-management unit for a session.
// Public surface: BeforeTurn, AfterEvent, PageFault only.
type Scheduler struct {
	derivationMu sync.Mutex
	Cfg          config.Config
	State        *wsl.WorkingState
	Hierarchy    *memory.Hierarchy
	Dispatcher   *observers.Dispatcher
	Resolver     *faults.Resolver
	Coherence    *coherence.State
}

// New constructs a scheduler.
func New(cfg config.Config, st *wsl.WorkingState, h *memory.Hierarchy, d *observers.Dispatcher, r *faults.Resolver, states ...*coherence.State) *Scheduler {
	coherent := coherence.NewState()
	if len(states) > 0 && states[0] != nil {
		coherent = states[0]
	}
	scheduler := &Scheduler{
		Cfg:        cfg,
		State:      st,
		Hierarchy:  h,
		Dispatcher: d,
		Resolver:   r,
		Coherence:  coherent,
	}
	if r != nil {
		r.Coherence = coherent
	}
	return scheduler
}

// BeforeTurn builds the L1 capsule at a safe injection boundary.
func (s *Scheduler) BeforeTurn(ctx context.Context) (string, error) {
	s.derivationMu.Lock()
	defer s.derivationMu.Unlock()
	_ = ctx
	view := s.State.Filter(func(rec wsl.Record) bool {
		return s.Coherence == nil || s.Coherence.RecordEligible(rec.ID())
	})
	_ = SelectL1(view)
	budget := s.Cfg.CapsuleTokenBudget
	if budget <= 0 {
		budget = 512
	}
	cap := renderer.RenderCapsule(view, budget)
	s.Hierarchy.SetL1Capsule(cap)
	return cap, nil
}

// AfterEvent digests an event into WSL via observers (tool-digest / constraints queues).
func (s *Scheduler) AfterEvent(ctx context.Context, ev ledger.Event) error {
	s.derivationMu.Lock()
	defer s.derivationMu.Unlock()

	if ev.ID == "" {
		return fmt.Errorf("event id is required for derivation provenance")
	}
	if err := ledger.ValidateEvent(ev); err != nil {
		return fmt.Errorf("validate durable event %s type %q: %w", ev.ID, ev.Type, err)
	}
	checkpoint := s.Dispatcher.CheckpointAllocator()
	var ups []wsl.Update
	err := s.Coherence.WithCandidate(ev, func(candidate *coherence.Candidate) error {
		var observerErr error
		ups, observerErr = s.Dispatcher.OnEvent(ctx, ev)
		if observerErr != nil {
			return fmt.Errorf("with observers: %w", observerErr)
		}
		for i := range ups {
			ups[i].EvidenceID = ev.ID
		}
		if err := candidate.BindUpdates(ups); err != nil {
			return fmt.Errorf("bind derived addresses: %w", err)
		}
		if err := s.State.ApplyEventUpdates(ev.ID, ups); err != nil {
			return fmt.Errorf("into WSL: %w", err)
		}
		return nil
	})
	if err != nil {
		checkpoint.Restore()
		return fmt.Errorf("derive durable event %s type %q: %w", ev.ID, ev.Type, err)
	}
	checkpoint.Commit()
	s.reconcileHierarchy()
	if coherence.ChangesResidency(ev.Type) {
		s.Hierarchy.ClearL1Capsule()
	}
	for _, u := range ups {
		// Promote failures into L2 only after the full state batch commits.
		if f, ok := u.Record.(*wsl.FailureRecord); ok {
			if !s.Coherence.RecordEligible(f.IDValue) {
				continue
			}
			page := &memory.Page{
				ID:      f.IDValue,
				Summary: f.Err,
				Refs:    []string{f.IDValue},
				Body:    formatFailurePage(f),
			}
			if binding, found := s.Coherence.BindingFor(ledger.TargetRecord, f.IDValue); found {
				page.Scope = binding.Scope
				page.Branch = binding.Branch
				page.Commit = binding.Commit
				page.Paths = append([]string(nil), binding.Paths...)
				page.SourceDigest = binding.SourceDigest
				page.ScopeEpoch = binding.Generation()
			}
			s.Hierarchy.PutL2(page)
		}
	}
	return nil
}

// PageFault handles demand retrieval.
func (s *Scheduler) PageFault(ctx context.Context, req faults.Request) (string, error) {
	s.derivationMu.Lock()
	defer s.derivationMu.Unlock()
	return s.Resolver.Resolve(ctx, req)
}

func (s *Scheduler) reconcileHierarchy() {
	if s.Hierarchy == nil || s.Coherence == nil {
		return
	}
	s.Hierarchy.Reconcile(func(page *memory.Page) memory.PageCoherence {
		status, revision := s.Coherence.PageStatus(
			page.ID, page.Refs, page.Scope, page.Branch, page.Commit, page.Paths, page.SourceDigest, page.ScopeEpoch,
		)
		result := memory.PageCoherence{
			Stale: status != coherence.StatusActive, Invalidated: status == coherence.StatusInvalidated,
			StaleRevision: revision, Branch: page.Branch, Commit: page.Commit,
			Paths: append([]string(nil), page.Paths...), ScopeEpoch: page.ScopeEpoch,
		}
		return result
	})
}

func formatFailurePage(f *wsl.FailureRecord) string {
	return renderer.RenderFailureDetail(f)
}
