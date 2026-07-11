package operator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"

	_ "modernc.org/sqlite"

	"wsms/internal/faults"
	"wsms/internal/harness"
	"wsms/internal/ledger"
	"wsms/internal/wsl"
)

// InspectViews lists the supported read-only views, for help text.
var InspectViews = []string{"sessions", "events", "event", "state", "capsule", "page", "residency"}

// Inspect renders one read-only operator view. "sessions" scans the ledger
// across sessions; "events"/"event" read the append-only ledger directly; the
// rest reconstruct derived state through a full session open.
func Inspect(ctx context.Context, opts Options, view, arg string, w io.Writer) error {
	switch view {
	case "sessions":
		return inspectSessions(ctx, opts, w)
	case "events":
		return inspectEvents(ctx, opts, w)
	case "event":
		return inspectEvent(ctx, opts, arg, w)
	case "state":
		return inspectState(opts, w)
	case "capsule":
		return inspectCapsule(ctx, opts, w)
	case "page":
		return inspectPage(ctx, opts, arg, w)
	case "residency":
		return inspectResidency(opts, w)
	default:
		return fmt.Errorf("unknown view %q (want one of %v)", view, InspectViews)
	}
}

func openLedgerRO(dataDir, sessionID string) (*ledger.AppendOnlyLedger, error) {
	path := ledgerPath(dataDir)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no ledger at %s: %w", path, err)
	}
	return ledger.Open(path, sessionID)
}

func inspectSessions(ctx context.Context, opts Options, w io.Writer) error {
	cfg := cfgFor(opts)
	path := ledgerPath(cfg.DataDir)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no ledger at %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(ctx,
		`SELECT session_id, COUNT(*) FROM events GROUP BY session_id ORDER BY session_id`)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	any := false
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		any = true
		if err := reportf(w, "%-24s %d events\n", id, n); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !any {
		return reportf(w, "(no sessions)\n")
	}
	return nil
}

func inspectEvents(ctx context.Context, opts Options, w io.Writer) error {
	cfg := cfgFor(opts)
	l, err := openLedgerRO(cfg.DataDir, cfg.SessionID)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()

	events, err := l.ListBySession(ctx, cfg.SessionID)
	if err != nil {
		return fmt.Errorf("list events: %w", err)
	}
	if len(events) == 0 {
		return reportf(w, "(no events for session %q)\n", cfg.SessionID)
	}
	for _, ev := range events {
		task := ev.TaskID
		if task == "" {
			task = "-"
		}
		if err := reportf(w, "%-6s %-19s %-10s %s\n", ev.ID, ev.Type, task, ev.TS.Format("2006-01-02T15:04:05")); err != nil {
			return err
		}
	}
	return nil
}

func inspectEvent(ctx context.Context, opts Options, id string, w io.Writer) error {
	if id == "" {
		return fmt.Errorf("inspect event requires an event id")
	}
	cfg := cfgFor(opts)
	l, err := openLedgerRO(cfg.DataDir, cfg.SessionID)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()

	ev, err := l.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get event %s: %w", id, err)
	}
	body, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	return reportf(w, "%s\n", body)
}

func inspectState(opts Options, w io.Writer) error {
	session, err := harness.OpenSession(cfgFor(opts))
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = session.Close() }()

	// The raw WSL serialization already renders logically deleted records as
	// @invalidated entries, so this is a faithful view: an active @decision D1
	// alongside its @invalidated marker means D1 is retained in L4 but excluded
	// from the model-facing capsule.
	text := wsl.Serialize(session.State.Records())
	if text == "" {
		return reportf(w, "(empty working state)\n")
	}
	return reportf(w, "%s\n", text)
}

func inspectCapsule(ctx context.Context, opts Options, w io.Writer) error {
	session, err := harness.OpenSession(cfgFor(opts))
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = session.Close() }()

	capsule, err := session.BeforeTurn(ctx)
	if err != nil {
		return fmt.Errorf("render capsule: %w", err)
	}
	return reportf(w, "%s\n", capsule)
}

func inspectPage(ctx context.Context, opts Options, id string, w io.Writer) error {
	if id == "" {
		return fmt.Errorf("inspect page requires a page id")
	}
	session, err := harness.OpenSession(cfgFor(opts))
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = session.Close() }()

	body, err := session.PageFault(ctx, id)
	if err != nil {
		return fmt.Errorf("read page %s: %w", id, err)
	}
	if body == faults.PageMiss {
		return reportf(w, "(no page %q — PAGE_MISS)\n", id)
	}
	return reportf(w, "%s\n", body)
}

func inspectResidency(opts Options, w io.Writer) error {
	session, err := harness.OpenSession(cfgFor(opts))
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = session.Close() }()

	body, err := json.MarshalIndent(session.ResidencySnapshot(), "", "  ")
	if err != nil {
		return err
	}
	return reportf(w, "%s\n", body)
}
