package observers

import (
	"context"
	"fmt"

	"wsms/internal/ledger"
	"wsms/internal/wsl"
)

// Staleness projects durable scope changes into the visible WSL page table.
// Authoritative eligibility lives in internal/coherence; this observer keeps
// the current task header accurate and records explicit terminal invalidations.
type Staleness struct {
	IDs   IDGen
	State *wsl.WorkingState
}

func (o *Staleness) Name() string { return "staleness" }

func (o *Staleness) Handle(ctx context.Context, ev ledger.Event) ([]wsl.Update, error) {
	_ = ctx
	switch ev.Type {
	case ledger.EventBranchChange:
		return o.updateTaskScope(ev.Branch, ev.Commit), nil
	case ledger.EventCommitChange:
		return o.updateTaskScope(ev.Branch, ev.Commit), nil
	case ledger.EventMemoryInvalidated:
		if o.IDs == nil {
			return nil, fmt.Errorf("staleness observer requires an id allocator")
		}
		target := ev.PayloadString("target")
		kind := ledger.MemoryTargetKind(ev.PayloadString("target_kind"))
		reason := ev.PayloadString("reason")
		known := map[string]bool{}
		if o.State != nil {
			known = o.State.KnownIDs()
		}
		if kind == ledger.TargetPath || !known[target] {
			// WSL v0 invalidation targets are known logical IDs. Rich page/path
			// addresses live in the coherence sidecar, so the durable mutation
			// event is the audit record's WSL address and retains the typed target.
			target = ev.ID
			reason += ": " + string(kind) + " " + ev.PayloadString("target")
		}
		return []wsl.Update{{
			Op: "upsert",
			Record: &wsl.InvalidatedRecord{
				IDValue: o.IDs.Next("I"),
				Target:  target,
				Reason:  reason,
			},
		}}, nil
	default:
		return nil, nil
	}
}

func (o *Staleness) updateTaskScope(branch, commit string) []wsl.Update {
	if o.State == nil {
		return nil
	}
	task := o.State.ActiveTask()
	if task == nil {
		return nil
	}
	task.Branch = branch
	task.Commit = commit
	return []wsl.Update{{Op: "upsert", Record: task}}
}
