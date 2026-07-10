package ledger

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	wsmserrors "wsms/internal/errors"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// AppendOnlyLedger is an append-only SQLite event store.
type AppendOnlyLedger struct {
	db        *sql.DB
	sessionID string
}

// Open opens or creates a ledger at path (SQLite file).
func Open(path, sessionID string) (*AppendOnlyLedger, error) {
	if sessionID == "" {
		return nil, &wsmserrors.LedgerError{Op: "open", Err: fmt.Errorf("session id is required")}
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, &wsmserrors.LedgerError{Op: "open", Err: err}
	}
	if path == ":memory:" {
		// A database/sql pool would otherwise create one unrelated in-memory
		// database per connection.
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		_ = db.Close()
		return nil, &wsmserrors.LedgerError{Op: "pragma", Err: err}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, &wsmserrors.LedgerError{Op: "schema", Err: err}
	}
	return &AppendOnlyLedger{db: db, sessionID: sessionID}, nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (l *AppendOnlyLedger) loadSeq(ctx context.Context, q queryRower) (int64, error) {
	var seq int64
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(append_seq), 0) FROM events WHERE session_id = ?`,
		l.sessionID,
	).Scan(&seq)
	if err != nil {
		return 0, &wsmserrors.LedgerError{Op: "load_seq", Err: err}
	}
	return seq, nil
}

// Close closes the underlying database.
func (l *AppendOnlyLedger) Close() error {
	return l.db.Close()
}

// Append atomically allocates and inserts an event. IDs and append sequences are
// assigned by the store; callers cannot import preassigned identities here.
func (l *AppendOnlyLedger) Append(ctx context.Context, ev Event) (Event, error) {
	if ev.ID != "" {
		return Event{}, &wsmserrors.LedgerError{Op: "append", Err: fmt.Errorf("caller-supplied event id is not allowed")}
	}
	if ev.Seq != 0 {
		return Event{}, &wsmserrors.LedgerError{Op: "append", Err: fmt.Errorf("caller-supplied append sequence is not allowed")}
	}
	if ev.SessionID != "" && ev.SessionID != l.sessionID {
		return Event{}, &wsmserrors.LedgerError{Op: "append", Err: fmt.Errorf("session %q is outside ledger session %q", ev.SessionID, l.sessionID)}
	}
	if err := ValidateEvent(ev); err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "validate_event", Err: err}
	}
	ev.SessionID = l.sessionID
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	if ev.Scope == nil {
		ev.Scope = map[string]any{}
	}

	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "marshal_payload", Err: err}
	}
	scope, err := json.Marshal(ev.Scope)
	if err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "marshal_scope", Err: err}
	}

	// The DSN starts transactions with BEGIN IMMEDIATE, so independent handles
	// serialize on SQLite's writer lock before either reads the durable maximum.
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "begin_append", Err: err}
	}
	defer func() { _ = tx.Rollback() }()

	lastSeq, err := l.loadSeq(ctx, tx)
	if err != nil {
		return Event{}, err
	}
	ev.Seq = lastSeq + 1
	ev.ID = fmt.Sprintf("E%04d", ev.Seq)

	_, err = tx.ExecContext(ctx, `
			INSERT INTO events (
				id, append_seq, ts, type, session_id, task_id, repo, branch, commit_sha,
				payload_json, artifact_hash, scope_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID,
		ev.Seq,
		ev.TS.UTC().Format(time.RFC3339Nano),
		string(ev.Type),
		ev.SessionID,
		nullStr(ev.TaskID),
		nullStr(ev.Repo),
		nullStr(ev.Branch),
		nullStr(ev.Commit),
		string(payload),
		nullStr(ev.ArtifactHash),
		string(scope),
	)
	if err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "insert", Err: err}
	}
	if err := tx.Commit(); err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "commit_append", Err: err}
	}
	return ev, nil
}

// Get returns an event by id within this ledger's session.
func (l *AppendOnlyLedger) Get(ctx context.Context, id string) (Event, error) {
	row := l.db.QueryRowContext(ctx, `
			SELECT id, append_seq, ts, type, session_id, task_id, repo, branch, commit_sha,
			       payload_json, artifact_hash, scope_json
			FROM events WHERE session_id = ? AND id = ?`, l.sessionID, id)
	return scanEvent(row)
}

// ListBySession returns events for a session in append order.
func (l *AppendOnlyLedger) ListBySession(ctx context.Context, sessionID string) ([]Event, error) {
	if sessionID != l.sessionID {
		return nil, &wsmserrors.LedgerError{Op: "list", Err: fmt.Errorf("session %q is outside ledger session %q", sessionID, l.sessionID)}
	}
	rows, err := l.db.QueryContext(ctx, `
			SELECT id, append_seq, ts, type, session_id, task_id, repo, branch, commit_sha,
			       payload_json, artifact_hash, scope_json
			FROM events WHERE session_id = ? ORDER BY append_seq ASC`, l.sessionID)
	if err != nil {
		return nil, &wsmserrors.LedgerError{Op: "list", Err: err}
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		ev, err := scanEventRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, &wsmserrors.LedgerError{Op: "list", Err: err}
	}
	return out, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanEvent(row scannable) (Event, error) {
	var (
		ev                                 Event
		ts, typ                            string
		taskID, repo, branch, commit, hash sql.NullString
		payloadJSON, scopeJSON             string
	)
	err := row.Scan(
		&ev.ID, &ev.Seq, &ts, &typ, &ev.SessionID,
		&taskID, &repo, &branch, &commit,
		&payloadJSON, &hash, &scopeJSON,
	)
	if err == sql.ErrNoRows {
		return Event{}, wsmserrors.ErrNotFound
	}
	if err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "scan", Err: err}
	}
	ev.TS, err = time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "decode_timestamp", Err: err}
	}
	ev.Type = EventType(typ)
	ev.TaskID = taskID.String
	ev.Repo = repo.String
	ev.Branch = branch.String
	ev.Commit = commit.String
	ev.ArtifactHash = hash.String
	if err := json.Unmarshal([]byte(payloadJSON), &ev.Payload); err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "decode_payload", Err: err}
	}
	if ev.Payload == nil {
		return Event{}, &wsmserrors.LedgerError{Op: "decode_payload", Err: fmt.Errorf("payload must be a JSON object")}
	}
	if err := json.Unmarshal([]byte(scopeJSON), &ev.Scope); err != nil {
		return Event{}, &wsmserrors.LedgerError{Op: "decode_scope", Err: err}
	}
	if ev.Scope == nil {
		return Event{}, &wsmserrors.LedgerError{Op: "decode_scope", Err: fmt.Errorf("scope must be a JSON object")}
	}
	return ev, nil
}

func scanEventRows(rows *sql.Rows) (Event, error) {
	return scanEvent(rows)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func sqliteDSN(path string) string {
	if path == ":memory:" {
		return ":memory:?_txlock=immediate&_pragma=busy_timeout%3D5000"
	}
	u := &url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(path),
	}
	query := u.Query()
	query.Set("_txlock", "immediate")
	query.Add("_pragma", "busy_timeout=5000")
	u.RawQuery = query.Encode()
	return u.String()
}
