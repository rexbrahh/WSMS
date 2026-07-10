package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"wsms/internal/pages"
)

// Apply commits page mutations transactionally and syncs the FTS projection.
// Mutations are idempotent by page_id + version + source_digest. A lower
// version for an existing page_id is ignored (no downgrade).
func (idx *Index) Apply(ctx context.Context, mutations []pages.PageMutation) error {
	return idx.ApplyWithWatermark(ctx, mutations, "", 0, 0)
}

// ApplyWithWatermark applies mutations and optionally advances a session watermark.
func (idx *Index) ApplyWithWatermark(ctx context.Context, mutations []pages.PageMutation, sessionID string, sourceSeq, pageVersion int64) error {
	if err := idx.guard(ctx); err != nil {
		return err
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.db == nil {
		return ErrClosed
	}
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, mut := range mutations {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := mut.Validate(); err != nil {
			return fmt.Errorf("apply mutation: %w", err)
		}
		switch mut.Op {
		case pages.MutationUpsert:
			if err := upsertPage(ctx, tx, mut.Page); err != nil {
				return err
			}
		case pages.MutationInvalidate:
			if err := invalidatePage(ctx, tx, mut.Page); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown mutation op %q", mut.Op)
		}
	}
	if sessionID != "" && sourceSeq > 0 {
		if err := setWatermarkTx(ctx, tx, sessionID, sourceSeq, pageVersion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Watermark returns the last applied source sequence for sessionID.
func (idx *Index) Watermark(ctx context.Context, sessionID string) (sourceSeq, pageVersion int64, err error) {
	if err := idx.guard(ctx); err != nil {
		return 0, 0, err
	}
	if sessionID == "" {
		return 0, 0, fmt.Errorf("session id is required")
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	err = idx.db.QueryRowContext(ctx,
		`SELECT last_source_seq, last_page_version FROM index_watermarks WHERE session_id = ?`,
		sessionID,
	).Scan(&sourceSeq, &pageVersion)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	return sourceSeq, pageVersion, err
}

// SetWatermark records catch-up progress for sessionID.
func (idx *Index) SetWatermark(ctx context.Context, sessionID string, sourceSeq, pageVersion int64) error {
	if err := idx.guard(ctx); err != nil {
		return err
	}
	if sessionID == "" || sourceSeq <= 0 {
		return fmt.Errorf("session id and positive source seq are required")
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := setWatermarkTx(ctx, tx, sessionID, sourceSeq, pageVersion); err != nil {
		return err
	}
	return tx.Commit()
}

func setWatermarkTx(ctx context.Context, tx *sql.Tx, sessionID string, sourceSeq, pageVersion int64) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO index_watermarks(session_id, last_source_seq, last_page_version)
VALUES(?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
  last_source_seq = excluded.last_source_seq,
  last_page_version = excluded.last_page_version
WHERE excluded.last_source_seq >= index_watermarks.last_source_seq
`, sessionID, sourceSeq, pageVersion)
	return err
}

func upsertPage(ctx context.Context, tx *sql.Tx, page pages.WarmPage) error {
	var existingVersion int64
	var existingDigest string
	err := tx.QueryRowContext(ctx,
		`SELECT page_version, source_digest FROM warm_pages WHERE page_id = ?`,
		string(page.ID),
	).Scan(&existingVersion, &existingDigest)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if int64(page.Version) < existingVersion {
			return nil // no downgrade
		}
		if int64(page.Version) == existingVersion && string(page.SourceDigest) == existingDigest {
			return nil // idempotent
		}
	}

	refsJSON, err := json.Marshal(page.Refs)
	if err != nil {
		return err
	}
	pathsJSON, err := json.Marshal(page.PathScope)
	if err != nil {
		return err
	}
	created := page.CreatedAt.UTC().Format(time.RFC3339Nano)
	verified := ""
	if !page.LastVerifiedAt.IsZero() {
		verified = page.LastVerifiedAt.UTC().Format(time.RFC3339Nano)
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO warm_pages(
  page_id, page_version, session_id, repo_id, task_id, branch, commit_id,
  path_scope_json, scope, kind, trust, status, salience, salience_reason,
  search_text, summary, refs_json, source_digest, source_seq_min, source_seq_max,
  compiler_version, scope_epoch, created_at, last_verified_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(page_id) DO UPDATE SET
  page_version=excluded.page_version,
  session_id=excluded.session_id,
  repo_id=excluded.repo_id,
  task_id=excluded.task_id,
  branch=excluded.branch,
  commit_id=excluded.commit_id,
  path_scope_json=excluded.path_scope_json,
  scope=excluded.scope,
  kind=excluded.kind,
  trust=excluded.trust,
  status=excluded.status,
  salience=excluded.salience,
  salience_reason=excluded.salience_reason,
  search_text=excluded.search_text,
  summary=excluded.summary,
  refs_json=excluded.refs_json,
  source_digest=excluded.source_digest,
  source_seq_min=excluded.source_seq_min,
  source_seq_max=excluded.source_seq_max,
  compiler_version=excluded.compiler_version,
  scope_epoch=excluded.scope_epoch,
  created_at=excluded.created_at,
  last_verified_at=excluded.last_verified_at
`,
		string(page.ID), int64(page.Version), page.SessionID, page.RepoID, page.TaskID, page.Branch, page.Commit,
		string(pathsJSON), string(page.Scope), string(page.Kind), string(page.Trust), string(page.Status),
		page.Salience, page.SalienceReason, page.SearchText, page.Summary, string(refsJSON),
		string(page.SourceDigest), page.SourceSeqMin, page.SourceSeqMax, string(page.CompilerVersion),
		int64(page.ScopeEpoch), created, verified,
	)
	if err != nil {
		return err
	}
	return syncFTS(ctx, tx, page)
}

func invalidatePage(ctx context.Context, tx *sql.Tx, page pages.WarmPage) error {
	res, err := tx.ExecContext(ctx, `
UPDATE warm_pages SET status = ?, page_version = ?, source_digest = ?
WHERE page_id = ? AND session_id = ?
`, string(pages.StatusInvalidated), int64(page.Version), string(page.SourceDigest), string(page.ID), page.SessionID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Insert invalidated stub if unknown so future searches can exclude it.
		if err := upsertPage(ctx, tx, page); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM warm_pages_fts WHERE page_id = ?`, string(page.ID)); err != nil {
		return err
	}
	// Best-effort dense cleanup; map table may not exist when dense is off.
	return deleteVectorTx(ctx, tx, page.ID)
}

func syncFTS(ctx context.Context, tx *sql.Tx, page pages.WarmPage) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM warm_pages_fts WHERE page_id = ?`, string(page.ID))
	if err != nil {
		return err
	}
	if page.Status != pages.StatusActive {
		return nil
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO warm_pages_fts(page_id, search_text, summary) VALUES(?, ?, ?)`,
		string(page.ID), page.SearchText, page.Summary,
	)
	return err
}
