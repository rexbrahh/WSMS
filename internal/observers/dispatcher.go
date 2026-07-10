package observers

import (
	"context"

	"wsms/internal/ledger"
	"wsms/internal/wsl"
)

// Dispatcher routes events to observers and collects updates.
type Dispatcher struct {
	Observers []Observer
	Allocator IDAllocator
}

// Default builds the scaffold observer set.
func Default(ids IDAllocator, states ...*wsl.WorkingState) *Dispatcher {
	var state *wsl.WorkingState
	if len(states) > 0 {
		state = states[0]
	}
	return &Dispatcher{
		Allocator: ids,
		Observers: []Observer{
			&Operational{IDs: ids},
			&Constraints{IDs: ids},
			&ToolDigest{IDs: ids},
			&Decisions{IDs: ids},
			&Staleness{IDs: ids, State: state},
		},
	}
}

// AllocatorCheckpoint represents one tentative observer-allocation transaction.
// A scheduler must either Commit it after its WSL batch commits or Restore it
// on any observer/batch failure.
type AllocatorCheckpoint struct {
	allocator IDAllocator
	snapshot  IDSnapshot
	settled   bool
}

// CheckpointAllocator captures the dispatcher allocator, if it has one.
func (d *Dispatcher) CheckpointAllocator() *AllocatorCheckpoint {
	checkpoint := &AllocatorCheckpoint{}
	if d == nil || d.Allocator == nil {
		return checkpoint
	}
	checkpoint.allocator = d.Allocator
	checkpoint.snapshot = d.Allocator.Snapshot()
	return checkpoint
}

// Commit keeps allocations made since the checkpoint.
func (c *AllocatorCheckpoint) Commit() {
	if c != nil {
		c.settled = true
	}
}

// Restore rolls allocations back to the checkpoint exactly once.
func (c *AllocatorCheckpoint) Restore() {
	if c == nil || c.settled {
		return
	}
	c.settled = true
	if c.allocator != nil {
		c.allocator.Restore(c.snapshot)
	}
}

// OnEvent runs all observers and returns ordered updates.
func (d *Dispatcher) OnEvent(ctx context.Context, ev ledger.Event) ([]wsl.Update, error) {
	var out []wsl.Update
	for _, o := range d.Observers {
		ups, err := o.Handle(ctx, ev)
		if err != nil {
			return out, err
		}
		out = append(out, ups...)
	}
	return out, nil
}
