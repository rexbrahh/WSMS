// Package scheduler manages residency and safe injection boundaries.
package scheduler

import (
	"context"
	"fmt"
	"sync"

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
}

// New constructs a scheduler.
func New(cfg config.Config, st *wsl.WorkingState, h *memory.Hierarchy, d *observers.Dispatcher, r *faults.Resolver) *Scheduler {
	return &Scheduler{
		Cfg:        cfg,
		State:      st,
		Hierarchy:  h,
		Dispatcher: d,
		Resolver:   r,
	}
}

// BeforeTurn builds the L1 capsule at a safe injection boundary.
func (s *Scheduler) BeforeTurn(ctx context.Context) (string, error) {
	_ = ctx
	_ = SelectL1(s.State)
	budget := s.Cfg.CapsuleTokenBudget
	if budget <= 0 {
		budget = 512
	}
	cap := renderer.RenderCapsule(s.State, budget)
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
	s.State.NoteEvent(ev.ID)
	checkpoint := s.Dispatcher.CheckpointAllocator()
	ups, err := s.Dispatcher.OnEvent(ctx, ev)
	if err != nil {
		checkpoint.Restore()
		return fmt.Errorf("derive durable event %s type %q with observers: %w", ev.ID, ev.Type, err)
	}
	for i := range ups {
		ups[i].EvidenceID = ev.ID
	}
	if err := s.State.ApplyUpdates(ups); err != nil {
		checkpoint.Restore()
		return fmt.Errorf("derive durable event %s type %q into WSL: %w", ev.ID, ev.Type, err)
	}
	checkpoint.Commit()
	for _, u := range ups {
		// Promote failures into L2 only after the full state batch commits.
		if f, ok := u.Record.(*wsl.FailureRecord); ok {
			s.Hierarchy.PutL2(&memory.Page{
				ID:      f.IDValue,
				Summary: f.Err,
				Refs:    []string{f.IDValue},
				Body:    formatFailurePage(f),
			})
		}
	}
	return nil
}

// PageFault handles demand retrieval.
func (s *Scheduler) PageFault(ctx context.Context, req faults.Request) (string, error) {
	return s.Resolver.Resolve(ctx, req)
}

func formatFailurePage(f *wsl.FailureRecord) string {
	return renderer.RenderFailureDetail(f)
}
