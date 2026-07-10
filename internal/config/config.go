// Package config holds runtime budgets and paths for a WSMS session.
package config

import "time"

// Config is the scaffold configuration for a session runtime.
type Config struct {
	// DataDir is the root for SQLite DB and artifacts (default .wsms).
	DataDir string

	// ArtifactThresholdBytes offloads larger payloads to the artifact store.
	ArtifactThresholdBytes int

	// CapsuleTokenBudget is the L1 structured-text budget (whitespace tokens).
	CapsuleTokenBudget int

	// PageFaultTokenBudget is the default budget for demand-fetched pages.
	PageFaultTokenBudget int

	// SessionID scopes ledger events.
	SessionID string

	// DenseDimensions enables the optional sqlite-vec projection on the warm
	// index when > 0. Zero (default) keeps dense search unavailable; FTS still
	// works. Real embeddings arrive in Phase 7D.
	DenseDimensions int
}

// Default returns scaffold defaults.
func Default() Config {
	return Config{
		DataDir:                ".wsms",
		ArtifactThresholdBytes: 4 * 1024, // 4 KiB
		CapsuleTokenBudget:     512,
		PageFaultTokenBudget:   256,
		SessionID:              "session-default",
		DenseDimensions:        0,
	}
}

// NowRFC3339 is a small helper for event timestamps.
func NowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
