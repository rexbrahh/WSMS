package scheduler

import (
	"math"
	"testing"
	"time"

	"wsms/internal/memory"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

func TestScoreFormula(t *testing.T) {
	s := Score(ScoreInputs{
		ScopeMatch: 1, Recency: 1, Salience: 1,
		AccessFrequency: 1, UserPriority: 1,
	})
	want := 0.30 + 0.20 + 0.20 + 0.15 + 0.15
	if s != want {
		t.Fatalf("got %v want %v", s, want)
	}

	if got := Score(ScoreInputs{Invalidation: 1}); got != -0.60 {
		t.Fatalf("invalidation score=%v, want -0.60", got)
	}
}

func TestScorePagePolicyLivesWithScheduler(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	page := &memory.Page{
		Scope:       types.ScopeTask,
		Branch:      "main",
		Salience:    2,
		UserPrio:    -1,
		AccessCount: 20,
		LastAccess:  now.Add(-36 * time.Hour),
	}
	task := &wsl.TaskRecord{Branch: "main"}

	// scope=1, recency=.5, clamped salience=1, clamped access=1,
	// clamped user priority=0.
	want := 0.30 + 0.20*0.5 + 0.20 + 0.15
	if got := ScorePage(page, task, now); math.Abs(got-want) > 1e-12 {
		t.Fatalf("ScorePage=%v, want %v", got, want)
	}
}

func TestScorePageNilIsZero(t *testing.T) {
	if got := ScorePage(nil, &wsl.TaskRecord{Branch: "main"}, time.Now()); got != 0 {
		t.Fatalf("ScorePage(nil)=%v, want 0", got)
	}
}
