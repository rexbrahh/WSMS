// Package types holds shared identifiers, scopes, and priorities for WSMS.
package types

// Scope bounds a record or page to a residency/coherence domain.
type Scope string

const (
	ScopeGlobal Scope = "global"
	ScopeRepo   Scope = "repo"
	ScopeBranch Scope = "branch"
	ScopeTask   Scope = "task"
	ScopeFile   Scope = "file"
)

// Priority is residency priority for tasks and pages.
type Priority string

const (
	PriorityHot  Priority = "hot"
	PriorityWarm Priority = "warm"
	PriorityCold Priority = "cold"
)

// Strength is constraint hardness.
type Strength string

const (
	StrengthHard Strength = "hard"
	StrengthSoft Strength = "soft"
)

// Source attributes provenance of a constraint or decision.
type Source string

const (
	SourceUser   Source = "user"
	SourceRepo   Source = "repo"
	SourceSystem Source = "system"
	SourceTest   Source = "test"
)

// MemoryTier is the L0–L4 hierarchy level.
type MemoryTier int

const (
	TierL0Scratch MemoryTier = 0
	TierL1Capsule MemoryTier = 1
	TierL2Hot     MemoryTier = 2
	TierL3Warm    MemoryTier = 3
	TierL4Cold    MemoryTier = 4
)

func (t MemoryTier) String() string {
	switch t {
	case TierL0Scratch:
		return "L0"
	case TierL1Capsule:
		return "L1"
	case TierL2Hot:
		return "L2"
	case TierL3Warm:
		return "L3"
	case TierL4Cold:
		return "L4"
	default:
		return "L?"
	}
}
