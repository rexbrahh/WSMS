CREATE TABLE IF NOT EXISTS events (
    id TEXT NOT NULL,
    append_seq INTEGER NOT NULL CHECK (append_seq > 0),
    ts TEXT NOT NULL,
    type TEXT NOT NULL,
    session_id TEXT NOT NULL,
    task_id TEXT,
    repo TEXT,
    branch TEXT,
    commit_sha TEXT,
    payload_json TEXT NOT NULL DEFAULT '{}',
    artifact_hash TEXT,
    scope_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY (session_id, id),
    UNIQUE (session_id, append_seq)
);

CREATE INDEX IF NOT EXISTS idx_events_session_append_seq ON events(session_id, append_seq);

CREATE TABLE IF NOT EXISTS artifacts_meta (
    sha256 TEXT PRIMARY KEY,
    path TEXT NOT NULL,
    size INTEGER NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS wsl_snapshots (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    ts TEXT NOT NULL,
    wsl_text TEXT NOT NULL
);
