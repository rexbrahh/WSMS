// Package operator implements the WSMS operator-facing commands: read-only
// inspection, durable export, logical delete (memory invalidation), and offline
// purge. It is the CLI counterpart to the in-process serve/demo surfaces.
//
// The invariants that shape this package: the L4 ledger is truth and is
// append-only, so "delete" is a logical invalidation event (kept in L4, honored
// by every derived cache) rather than a row removal; only the offline `purge`
// removes durable bytes, and it never touches an operator-supplied data
// directory itself — only that session's rows plus the disposable L3 index.
package operator

import (
	"fmt"
	"io"
	"net/url"
	"path/filepath"

	"wsms/internal/config"
)

// Options names the data directory and session every operator command scopes to.
type Options struct {
	DataDir   string
	SessionID string
}

// cfgFor mirrors serve/demo: start from the defaults, override only what the
// operator supplied, so a session opens identically across every entry point.
func cfgFor(opts Options) config.Config {
	cfg := config.Default()
	if opts.DataDir != "" {
		cfg.DataDir = opts.DataDir
	}
	if opts.SessionID != "" {
		cfg.SessionID = opts.SessionID
	}
	return cfg
}

// ledgerPath / indexDir locate the on-disk stores under a data directory,
// matching harness.OpenSession's layout.
func ledgerPath(dataDir string) string { return filepath.Join(dataDir, "ledger.db") }
func indexDir(dataDir string) string   { return filepath.Join(dataDir, "index") }

// sqliteDSN reproduces the ledger/indexer DSN so a direct (non-ledger-API)
// connection uses the same locking and busy-timeout behavior. Purge is the only
// caller that bypasses the append-only API and thus needs this.
func sqliteDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout=5000")
	u.RawQuery = q.Encode()
	return u.String()
}

// reportf writes a human-readable operator line, surfacing short writes.
func reportf(w io.Writer, format string, args ...any) error {
	_, err := io.WriteString(w, fmt.Sprintf(format, args...))
	return err
}
