-- Disposable L3 warm index. Never store this schema in ledger.db.
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS index_meta (
  key   TEXT PRIMARY KEY NOT NULL,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS index_watermarks (
  session_id        TEXT PRIMARY KEY NOT NULL,
  last_source_seq   INTEGER NOT NULL,
  last_page_version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS warm_pages (
  page_id           TEXT PRIMARY KEY NOT NULL,
  page_version      INTEGER NOT NULL,
  session_id        TEXT NOT NULL,
  repo_id           TEXT NOT NULL DEFAULT '',
  task_id           TEXT NOT NULL DEFAULT '',
  branch            TEXT NOT NULL DEFAULT '',
  commit_id         TEXT NOT NULL DEFAULT '',
  path_scope_json   TEXT NOT NULL DEFAULT '[]',
  scope             TEXT NOT NULL,
  kind              TEXT NOT NULL,
  trust             TEXT NOT NULL,
  status            TEXT NOT NULL,
  salience          REAL NOT NULL,
  salience_reason   TEXT NOT NULL,
  search_text       TEXT NOT NULL,
  summary           TEXT NOT NULL,
  refs_json         TEXT NOT NULL,
  source_digest     TEXT NOT NULL,
  source_seq_min    INTEGER NOT NULL,
  source_seq_max    INTEGER NOT NULL,
  compiler_version  TEXT NOT NULL,
  scope_epoch       INTEGER NOT NULL,
  created_at        TEXT NOT NULL,
  last_verified_at  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS warm_pages_session_status
  ON warm_pages(session_id, status);

CREATE INDEX IF NOT EXISTS warm_pages_session_branch
  ON warm_pages(session_id, branch);

CREATE VIRTUAL TABLE IF NOT EXISTS warm_pages_fts USING fts5(
  page_id UNINDEXED,
  search_text,
  summary,
  tokenize = 'unicode61'
);
