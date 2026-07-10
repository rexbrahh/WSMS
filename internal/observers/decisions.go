package observers

import (
	"context"

	"wsms/internal/ledger"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

// Decisions derives explicit decisions and grounded avoidance records.
type Decisions struct {
	IDs IDGen
}

func (o *Decisions) Name() string { return "decisions" }

func (o *Decisions) Handle(ctx context.Context, ev ledger.Event) ([]wsl.Update, error) {
	_ = ctx
	if ev.Type != ledger.EventDecision {
		return nil, nil
	}

	decision := &wsl.DecisionRecord{
		IDValue: o.IDs.Next("D"),
		Chosen:  ev.PayloadString("chosen"),
		Because: ev.PayloadString("because"),
		Refs:    ev.PayloadString("refs"),
		Scope:   types.Scope(ev.PayloadString("scope")),
	}
	avoidText := ev.PayloadString("avoid_text")
	if avoidText == "" {
		return []wsl.Update{{Op: "upsert", Record: decision}}, nil
	}

	// Apply the avoid first so its reference must resolve against pre-event state;
	// the WorkingState batch still commits the avoid and decision atomically.
	avoid := &wsl.AvoidRecord{
		IDValue: o.IDs.Next("A"),
		Reason:  "failed_attempt",
		Text:    avoidText,
		Ref:     ev.PayloadString("avoid_ref"),
	}
	return []wsl.Update{
		{Op: "upsert", Record: avoid},
		{Op: "upsert", Record: decision},
	}, nil
}
