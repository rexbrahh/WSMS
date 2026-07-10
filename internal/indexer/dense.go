package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"

	"wsms/internal/pages"
)

const (
	metaDenseEnabled  = "dense_enabled"
	metaDenseDims     = "dense_dimensions"
	metaDenseMetric   = "dense_metric"
	denseMetricCosine = "cosine"
)

// VectorRecord is one page embedding for the optional dense projection.
// Phase 7C accepts fixture vectors; a real embedder arrives in 7D.
type VectorRecord struct {
	PageID    pages.PageID
	SessionID string
	Vector    []float64
}

// DenseEnabled reports whether this index has a vec0 projection.
func (idx *Index) DenseEnabled() bool {
	return idx != nil && idx.denseDims > 0
}

// DenseDimensions returns the configured embedding width, or 0.
func (idx *Index) DenseDimensions() int {
	if idx == nil {
		return 0
	}
	return idx.denseDims
}

func (idx *Index) initDense(ctx context.Context, requested int) error {
	if requested < 0 {
		return fmt.Errorf("dense dimensions must be non-negative")
	}
	if requested > pages.MaxVectorDimensions {
		return fmt.Errorf("dense dimensions %d exceed max %d", requested, pages.MaxVectorDimensions)
	}

	var stored string
	err := idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaDenseDims).Scan(&stored)
	switch {
	case err == sql.ErrNoRows:
		if requested == 0 {
			idx.denseDims = 0
			return nil
		}
		return idx.enableDense(ctx, requested)
	case err != nil:
		return err
	default:
		n, convErr := strconv.Atoi(stored)
		if convErr != nil || n <= 0 {
			return fmt.Errorf("corrupt dense_dimensions meta %q", stored)
		}
		if requested > 0 && requested != n {
			return fmt.Errorf("dense dimension mismatch: open requested %d, index has %d", requested, n)
		}
		idx.denseDims = n
		return idx.ensureDenseTables(ctx)
	}
}

func (idx *Index) enableDense(ctx context.Context, dims int) error {
	if dims <= 0 {
		return fmt.Errorf("dense dimensions must be positive")
	}
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, pair := range [][2]string{
		{metaDenseEnabled, "1"},
		{metaDenseDims, strconv.Itoa(dims)},
		{metaDenseMetric, denseMetricCosine},
	} {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO index_meta(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, pair[0], pair[1]); err != nil {
			return err
		}
	}
	if err := ensureDenseTablesTx(ctx, tx, dims); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	idx.denseDims = dims
	return nil
}

func (idx *Index) ensureDenseTables(ctx context.Context) error {
	if idx.denseDims <= 0 {
		return nil
	}
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureDenseTablesTx(ctx, tx, idx.denseDims); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureDenseTablesTx(ctx context.Context, tx *sql.Tx, dims int) error {
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS warm_page_vec_map (
  page_id    TEXT PRIMARY KEY NOT NULL,
  rowid      INTEGER NOT NULL UNIQUE,
  session_id TEXT NOT NULL
)`); err != nil {
		return err
	}
	// vec0 cannot use IF NOT EXISTS reliably across recreates; check first.
	var name string
	err := tx.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='warm_pages_vec'`).Scan(&name)
	if err == sql.ErrNoRows {
		ddl := fmt.Sprintf(`CREATE VIRTUAL TABLE warm_pages_vec USING vec0(
  session_id TEXT PARTITION KEY,
  embedding float[%d] distance_metric=cosine
)`, dims)
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("create warm_pages_vec: %w", err)
		}
		return nil
	}
	return err
}

// UpsertVectors writes or replaces dense vectors for existing warm pages.
func (idx *Index) UpsertVectors(ctx context.Context, records []VectorRecord) error {
	if err := idx.guard(ctx); err != nil {
		return err
	}
	if !idx.DenseEnabled() {
		return ErrDenseUnavailable
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
	for _, rec := range records {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := idx.upsertVectorTx(ctx, tx, rec); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteVector removes one page's dense projection.
func (idx *Index) DeleteVector(ctx context.Context, pageID pages.PageID) error {
	if err := idx.guard(ctx); err != nil {
		return err
	}
	if !idx.DenseEnabled() {
		return ErrDenseUnavailable
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteVectorTx(ctx, tx, pageID); err != nil {
		return err
	}
	return tx.Commit()
}

func (idx *Index) upsertVectorTx(ctx context.Context, tx *sql.Tx, rec VectorRecord) error {
	if rec.PageID == "" || rec.SessionID == "" {
		return fmt.Errorf("page id and session id are required")
	}
	if err := validateVector(rec.Vector, idx.denseDims); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM warm_pages WHERE page_id = ?`, string(rec.PageID)).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("page %s is not in warm_pages", rec.PageID)
		}
		return err
	}

	var rowID int64
	err := tx.QueryRowContext(ctx, `SELECT rowid FROM warm_page_vec_map WHERE page_id = ?`, string(rec.PageID)).Scan(&rowID)
	switch {
	case err == sql.ErrNoRows:
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) + 1 FROM warm_page_vec_map`).Scan(&rowID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO warm_page_vec_map(page_id, rowid, session_id) VALUES(?, ?, ?)`,
			string(rec.PageID), rowID, rec.SessionID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO warm_pages_vec(rowid, session_id, embedding) VALUES(?, ?, ?)`,
			rowID, rec.SessionID, vectorJSON(rec.Vector),
		); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		// Replace: delete old vec row then insert (vec0 update semantics vary).
		if _, err := tx.ExecContext(ctx, `DELETE FROM warm_pages_vec WHERE rowid = ?`, rowID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE warm_page_vec_map SET session_id = ? WHERE page_id = ?`,
			rec.SessionID, string(rec.PageID),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO warm_pages_vec(rowid, session_id, embedding) VALUES(?, ?, ?)`,
			rowID, rec.SessionID, vectorJSON(rec.Vector),
		); err != nil {
			return err
		}
	}
	return nil
}

func deleteVectorTx(ctx context.Context, tx *sql.Tx, pageID pages.PageID) error {
	if pageID == "" {
		return nil
	}
	var rowID int64
	err := tx.QueryRowContext(ctx, `SELECT rowid FROM warm_page_vec_map WHERE page_id = ?`, string(pageID)).Scan(&rowID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		// Dense tables are optional; missing map is not an invalidate failure.
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM warm_pages_vec WHERE rowid = ?`, rowID); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM warm_page_vec_map WHERE page_id = ?`, string(pageID))
	return err
}

// SearchDense runs cosine KNN over the optional vec0 projection.
// Candidate.Rank is cosine distance (lower is better). Dense is unavailable
// when the index was opened without DenseDimensions.
func (idx *Index) SearchDense(ctx context.Context, q SearchQuery, vector []float64) ([]Candidate, error) {
	if err := idx.guard(ctx); err != nil {
		return nil, err
	}
	if !idx.DenseEnabled() {
		return nil, ErrDenseUnavailable
	}
	if q.SessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if err := validateVector(vector, idx.denseDims); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	// Over-fetch so Go post-filters can still fill the limit.
	fetch := limit * 4
	if fetch < 20 {
		fetch = 20
	}
	if fetch > 200 {
		fetch = 200
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.db == nil {
		return nil, ErrClosed
	}

	rows, err := idx.db.QueryContext(ctx, `
SELECT m.page_id, v.distance
FROM warm_pages_vec v
JOIN warm_page_vec_map m ON m.rowid = v.rowid
WHERE v.embedding MATCH ?
  AND v.k = ?
  AND v.session_id = ?
ORDER BY v.distance ASC, m.page_id ASC
`, vectorJSON(vector), fetch, q.SessionID)
	if err != nil {
		return nil, fmt.Errorf("dense search: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		var pageID string
		var distance float64
		if err := rows.Scan(&pageID, &distance); err != nil {
			return nil, err
		}
		page, err := loadPageByID(ctx, idx.db, pageID)
		if err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return nil, err
		}
		if page.Status != pages.StatusActive {
			continue
		}
		if page.SessionID != q.SessionID {
			continue
		}
		if q.RepoID != "" && page.RepoID != "" && page.RepoID != q.RepoID {
			continue
		}
		if q.TaskID != "" && page.TaskID != "" && page.TaskID != q.TaskID {
			continue
		}
		if q.Branch != "" && page.Branch != "" && page.Branch != q.Branch {
			continue
		}
		if len(q.Kinds) > 0 && !kindAllowed(page.Kind, q.Kinds) {
			continue
		}
		if len(q.Trust) > 0 && !trustAllowed(page.Trust, q.Trust) {
			continue
		}
		out = append(out, Candidate{
			Page: page,
			Rank: distance,
			Explanation: fmt.Sprintf("channel=vec0 metric=cosine distance=%.6f filters=session,status=active page=%s",
				distance, page.ID),
		})
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func loadPageByID(ctx context.Context, db *sql.DB, pageID string) (pages.WarmPage, error) {
	row := db.QueryRowContext(ctx, `
SELECT page_id, page_version, session_id, repo_id, task_id, branch, commit_id,
       path_scope_json, scope, kind, trust, status, salience, salience_reason,
       search_text, summary, refs_json, source_digest, source_seq_min, source_seq_max,
       compiler_version, scope_epoch, created_at, last_verified_at
FROM warm_pages WHERE page_id = ?`, pageID)
	return scanPageRow(row)
}

type scannable interface {
	Scan(dest ...any) error
}

func scanPageRow(row scannable) (pages.WarmPage, error) {
	// Reuse the same column layout as SearchLexical without rank.
	// Implemented via a tiny synthetic wrapper in search helpers.
	return scanPageFromScanner(row)
}

func kindAllowed(kind pages.PageKind, allowed []pages.PageKind) bool {
	for _, a := range allowed {
		if a == kind {
			return true
		}
	}
	return false
}

func trustAllowed(trust pages.Trust, allowed []pages.Trust) bool {
	for _, a := range allowed {
		if a == trust {
			return true
		}
	}
	return false
}

func validateVector(vector []float64, dims int) error {
	if dims <= 0 {
		return ErrDenseUnavailable
	}
	if len(vector) != dims {
		return fmt.Errorf("%w: vector dimension %d != %d", pages.ErrInvalidVector, len(vector), dims)
	}
	if len(vector) == 0 {
		return fmt.Errorf("%w: empty vector", pages.ErrInvalidVector)
	}
	var norm float64
	for _, v := range vector {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%w: non-finite component", pages.ErrInvalidVector)
		}
		norm = math.Hypot(norm, v)
	}
	if norm == 0 || math.IsInf(norm, 0) {
		return fmt.Errorf("%w: zero or overflowing norm", pages.ErrInvalidVector)
	}
	return nil
}

func vectorJSON(vector []float64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vector {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	}
	b.WriteByte(']')
	return b.String()
}
