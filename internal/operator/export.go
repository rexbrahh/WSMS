package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"wsms/internal/ledger"
)

// Export writes the session's durable L4 events to w as JSON Lines (one event
// per line, append order). This is the authoritative, replay-complete artifact:
// it reads the append-only ledger directly rather than any derived cache, so it
// is valid even when derived state cannot be reconstructed. Returns the count
// written.
func Export(ctx context.Context, opts Options, w io.Writer) (int, error) {
	cfg := cfgFor(opts)
	path := ledgerPath(cfg.DataDir)

	// Open (not create): exporting a nonexistent ledger is an operator error,
	// not an empty success — ledger.Open would otherwise create a fresh DB.
	if _, err := os.Stat(path); err != nil {
		return 0, fmt.Errorf("no ledger at %s: %w", path, err)
	}

	l, err := ledger.Open(path, cfg.SessionID)
	if err != nil {
		return 0, fmt.Errorf("open ledger: %w", err)
	}
	defer func() { _ = l.Close() }()

	events, err := l.ListBySession(ctx, cfg.SessionID)
	if err != nil {
		return 0, fmt.Errorf("list events: %w", err)
	}

	enc := json.NewEncoder(w) // Encode appends '\n', giving JSONL framing.
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return 0, fmt.Errorf("encode event %s: %w", ev.ID, err)
		}
	}
	return len(events), nil
}
