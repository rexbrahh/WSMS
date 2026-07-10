package scheduler

import (
	"math"
	"time"

	"wsms/internal/memory"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

// ScoreInputs are normalized 0..1 features for residency scoring.
type ScoreInputs struct {
	ScopeMatch      float64
	Recency         float64
	Salience        float64
	AccessFrequency float64
	UserPriority    float64
	Staleness       float64
	Invalidation    float64
}

// Score combines residency-policy features into a candidate priority.
func Score(in ScoreInputs) float64 {
	return 0.30*in.ScopeMatch +
		0.20*in.Recency +
		0.20*in.Salience +
		0.15*in.AccessFrequency +
		0.15*in.UserPriority -
		0.40*in.Staleness -
		0.60*in.Invalidation
}

// ScorePage scores a mechanism-owned page against the active task.
func ScorePage(p *memory.Page, task *wsl.TaskRecord, now time.Time) float64 {
	if p == nil {
		return 0
	}
	in := ScoreInputs{
		Salience:     clamp01(p.Salience),
		UserPriority: clamp01(p.UserPrio),
	}
	if p.Stale {
		in.Staleness = 1
	}
	if p.Invalidated {
		in.Invalidation = 1
	}
	if task != nil {
		if p.Scope == types.ScopeTask || p.Scope == types.ScopeGlobal {
			in.ScopeMatch = 1
		}
		if p.Branch != "" && task.Branch != "" && p.Branch == task.Branch {
			in.ScopeMatch = math.Max(in.ScopeMatch, 0.9)
		}
	}
	if !p.LastAccess.IsZero() {
		age := now.Sub(p.LastAccess).Hours()
		in.Recency = clamp01(1.0 - age/72.0)
	} else if !p.CreatedAt.IsZero() {
		age := now.Sub(p.CreatedAt).Hours()
		in.Recency = clamp01(1.0 - age/72.0)
	}
	in.AccessFrequency = clamp01(float64(p.AccessCount) / 10.0)
	return Score(in)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
