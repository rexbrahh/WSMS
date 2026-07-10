package observers

import (
	"context"

	"wsms/internal/ledger"
	"wsms/internal/wsl"
)

// Staleness is a scaffold stub for branch/file scope invalidation.
type Staleness struct{}

func (o *Staleness) Name() string { return "staleness" }

func (o *Staleness) Handle(ctx context.Context, ev ledger.Event) ([]wsl.Update, error) {
	_ = ctx
	_ = ev
	return nil, nil
}
