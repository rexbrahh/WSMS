package observers

import (
	"context"
	"strings"

	"wsms/internal/ledger"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

// Constraints extracts hard/soft user constraints from instructions.
type Constraints struct {
	IDs IDGen
}

func (o *Constraints) Name() string { return "constraints" }

func (o *Constraints) Handle(ctx context.Context, ev ledger.Event) ([]wsl.Update, error) {
	_ = ctx
	switch ev.Type {
	case ledger.EventUserInstruction, ledger.EventHumanCorrection:
	default:
		return nil, nil
	}
	text := strings.TrimSpace(ev.PayloadString("text"))
	if text == "" {
		return nil, nil
	}
	strength := types.StrengthSoft
	if looksHard(text) {
		strength = types.StrengthHard
	}
	// Only emit when it looks like a constraint, not free-form chatter.
	if strength != types.StrengthHard && !looksConstraint(text) {
		return nil, nil
	}
	id := o.IDs.Next("C")
	rec := &wsl.ConstraintRecord{
		IDValue:  id,
		Strength: strength,
		Source:   types.SourceUser,
		Text:     text,
		Scope:    types.ScopeTask,
	}
	return []wsl.Update{{Op: "upsert", Record: rec}}, nil
}

func looksHard(text string) bool {
	lower := strings.ToLower(text)
	needles := []string{
		"do not ", "don't ", "dont ", "never ", "must not ",
		"hard constraint", "required:",
	}
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func looksConstraint(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "must ") ||
		strings.Contains(lower, "should not") ||
		strings.Contains(lower, "prefer ") ||
		strings.HasPrefix(lower, "constraint:")
}
