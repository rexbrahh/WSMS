package operator

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"

	_ "modernc.org/sqlite"
)

// Purge irreversibly erases one session's durable bytes: its append-only ledger
// rows (events + wsl snapshots) and its L3 warm index. This is the only command
// that bypasses the append-only ledger, so it is offline (stop `wsms serve`
// first) and gated on confirm. It never removes the operator-supplied data
// directory itself, and it never deletes content-addressed artifact blobs
// (shared across sessions); those are reported and retained.
//
// Without confirm it is a dry run: it reports exactly what would be removed.
func Purge(ctx context.Context, opts Options, confirm bool, w io.Writer) error {
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

	events, snaps, artifacts, err := purgeCounts(ctx, db, cfg.SessionID)
	if err != nil {
		return err
	}
	if events == 0 && snaps == 0 {
		return reportf(w, "nothing to purge: session %q has no rows in %s\n", cfg.SessionID, path)
	}

	if !confirm {
		return reportf(w,
			"purge (dry run) would remove for session %q:\n"+
				"  %d ledger events\n"+
				"  %d wsl snapshots\n"+
				"  the disposable L3 index (other sessions rebuild lazily)\n"+
				"%d content-addressed artifact blobs are retained (may be shared)\n"+
				"re-run with --yes to erase these durable bytes\n",
			cfg.SessionID, events, snaps, artifacts)
	}

	// Drop the disposable index first: it is safe to remove at any time and
	// rebuilds from surviving sessions' ledgers. Doing it before the durable
	// delete means a mid-way failure can never leave L3 serving pages whose L4
	// evidence is already gone.
	if err := os.RemoveAll(indexDir(cfg.DataDir)); err != nil {
		return fmt.Errorf("drop L3 index: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin purge: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE session_id = ?`, cfg.SessionID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM wsl_snapshots WHERE session_id = ?`, cfg.SessionID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete wsl snapshots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit purge: %w", err)
	}

	return reportf(w,
		"purged session %q: removed %d events, %d wsl snapshots; L3 index dropped\n"+
			"%d artifact blobs retained (content-addressed; delete the data dir yourself to reclaim)\n",
		cfg.SessionID, events, snaps, artifacts)
}

func purgeCounts(ctx context.Context, db *sql.DB, sessionID string) (events, snaps, artifacts int, err error) {
	if err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE session_id = ?`, sessionID).Scan(&events); err != nil {
		return 0, 0, 0, fmt.Errorf("count events: %w", err)
	}
	if err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wsl_snapshots WHERE session_id = ?`, sessionID).Scan(&snaps); err != nil {
		return 0, 0, 0, fmt.Errorf("count snapshots: %w", err)
	}
	if err = db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT artifact_hash) FROM events WHERE session_id = ? AND artifact_hash != ''`,
		sessionID).Scan(&artifacts); err != nil {
		return 0, 0, 0, fmt.Errorf("count artifacts: %w", err)
	}
	return events, snaps, artifacts, nil
}
