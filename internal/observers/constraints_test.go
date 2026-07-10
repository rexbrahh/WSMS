package observers

import (
	"context"
	"testing"

	"wsms/internal/ledger"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

func TestConstraintsHardVerbatim(t *testing.T) {
	o := &Constraints{IDs: NewSeqIDGen()}
	text := "do not rewrite transport layer"
	ups, err := o.Handle(context.Background(), ledger.Event{
		Type: ledger.EventUserInstruction,
		Payload: map[string]any{
			"text": text,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 {
		t.Fatalf("len=%d", len(ups))
	}
	c := ups[0].Record.(*wsl.ConstraintRecord)
	if c.Text != text {
		t.Fatalf("text mutated: %q", c.Text)
	}
	if c.Strength != types.StrengthHard {
		t.Fatalf("strength=%s", c.Strength)
	}
	if c.Source != types.SourceUser {
		t.Fatalf("source=%s", c.Source)
	}
}
