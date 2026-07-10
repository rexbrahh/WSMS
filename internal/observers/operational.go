package observers

import (
	"context"

	"wsms/internal/ledger"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

// Operational derives explicit task and next-action state from durable events.
type Operational struct {
	IDs IDGen
}

func (o *Operational) Name() string { return "operational" }

func (o *Operational) Handle(ctx context.Context, ev ledger.Event) ([]wsl.Update, error) {
	_ = ctx
	switch ev.Type {
	case ledger.EventTaskStarted:
		branch := ev.PayloadString("branch")
		if branch == "" {
			branch = ev.Branch
		}
		return []wsl.Update{{
			Op: "upsert",
			Record: &wsl.TaskRecord{
				IDValue:  o.IDs.Next("T"),
				Phase:    ev.PayloadString("phase"),
				Priority: types.Priority(ev.PayloadString("priority")),
				Goal:     ev.PayloadString("goal"),
				Branch:   branch,
				Commit:   ev.Commit,
				Dirty:    ev.PayloadString("dirty"),
			},
		}}, nil
	case ledger.EventNextAction:
		return []wsl.Update{{
			Op: "upsert",
			Record: &wsl.NextRecord{
				Action:   ev.PayloadString("action"),
				Target:   ev.PayloadString("target"),
				Question: ev.PayloadString("question"),
			},
		}}, nil
	default:
		return nil, nil
	}
}
