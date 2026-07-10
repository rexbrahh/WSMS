package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
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
	PageID             pages.PageID
	SessionID          string
	EmbeddingNamespace string
	Vector             []float64
}

const DefaultVectorNamespace = "wsms/manual-vector/v1"

// DenseEnabled reports whether this index has a vec0 projection.
func (idx *Index) DenseEnabled() bool {
	if idx == nil {
		return false
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.db != nil && idx.denseDims > 0
}

// DenseDimensions returns the configured embedding width, or 0.
func (idx *Index) DenseDimensions() int {
	if idx == nil {
		return 0
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
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
	dropProjection := false
	if exists, err := tableExistsTx(ctx, tx, "warm_page_vec_map"); err != nil {
		return err
	} else if exists {
		hasNamespace, err := tableHasColumnTx(ctx, tx, "warm_page_vec_map", "embedding_namespace")
		if err != nil {
			return err
		}
		if !hasNamespace {
			dropProjection = true
		}
	}
	if exists, err := tableExistsTx(ctx, tx, "warm_pages_vec"); err != nil {
		return err
	} else if exists {
		hasNamespace, err := tableHasColumnTx(ctx, tx, "warm_pages_vec", "embedding_namespace")
		if err != nil {
			return err
		}
		if !hasNamespace {
			dropProjection = true
		}
	}
	if dropProjection {
		if err := dropDenseProjectionTx(ctx, tx); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS warm_page_vec_map (
  page_id             TEXT PRIMARY KEY NOT NULL,
  rowid               INTEGER NOT NULL UNIQUE,
  session_id          TEXT NOT NULL,
  page_version        INTEGER NOT NULL,
  source_digest       TEXT NOT NULL,
  compiler_version    TEXT NOT NULL,
  embedding_namespace TEXT NOT NULL
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
  embedding_namespace TEXT PARTITION KEY,
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
	release, err := idx.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.db == nil {
		return ErrClosed
	}
	if idx.denseDims <= 0 {
		return ErrDenseUnavailable
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
	release, err := idx.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.db == nil {
		return ErrClosed
	}
	if idx.denseDims <= 0 {
		return ErrDenseUnavailable
	}
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
	namespace, err := normalizeVectorNamespace(rec.EmbeddingNamespace)
	if err != nil {
		return err
	}
	var pageVersion int64
	var pageSession, sourceDigest, compilerVersion, status string
	if err := tx.QueryRowContext(ctx, `SELECT page_version, session_id, source_digest, compiler_version, status FROM warm_pages WHERE page_id = ?`, string(rec.PageID)).Scan(
		&pageVersion, &pageSession, &sourceDigest, &compilerVersion, &status,
	); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("page %s is not in warm_pages", rec.PageID)
		}
		return err
	}
	if pageSession != rec.SessionID {
		return fmt.Errorf("page %s session %q does not match vector session %q", rec.PageID, pageSession, rec.SessionID)
	}
	if status != string(pages.StatusActive) {
		return fmt.Errorf("page %s is not active", rec.PageID)
	}

	var rowID int64
	err = tx.QueryRowContext(ctx, `SELECT rowid FROM warm_page_vec_map WHERE page_id = ?`, string(rec.PageID)).Scan(&rowID)
	switch {
	case err == sql.ErrNoRows:
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) + 1 FROM warm_page_vec_map`).Scan(&rowID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO warm_page_vec_map(page_id, rowid, session_id, page_version, source_digest, compiler_version, embedding_namespace) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			string(rec.PageID), rowID, rec.SessionID, pageVersion, sourceDigest, compilerVersion, namespace,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO warm_pages_vec(rowid, session_id, embedding_namespace, embedding) VALUES(?, ?, ?, ?)`,
			rowID, rec.SessionID, namespace, vectorJSON(rec.Vector),
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
			`UPDATE warm_page_vec_map SET session_id = ?, page_version = ?, source_digest = ?, compiler_version = ?, embedding_namespace = ? WHERE page_id = ?`,
			rec.SessionID, pageVersion, sourceDigest, compilerVersion, namespace, string(rec.PageID),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO warm_pages_vec(rowid, session_id, embedding_namespace, embedding) VALUES(?, ?, ?, ?)`,
			rowID, rec.SessionID, namespace, vectorJSON(rec.Vector),
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

func tableExistsTx(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var found string
	err := tx.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, name,
	).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func tableHasColumnTx(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func dropDenseProjectionTx(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS warm_pages_vec`,
		`DROP TABLE IF EXISTS warm_page_vec_map`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// SearchDense runs cosine KNN over the optional vec0 projection.
// Candidate.Rank is cosine distance (lower is better). Dense is unavailable
// when the index was opened without DenseDimensions.
func (idx *Index) SearchDense(ctx context.Context, q SearchQuery, vector []float64) ([]Candidate, error) {
	release, err := idx.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if q.SessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	namespace, err := normalizeVectorNamespace(q.EmbeddingNamespace)
	if err != nil {
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
	if idx.denseDims <= 0 {
		return nil, ErrDenseUnavailable
	}
	if err := validateVector(vector, idx.denseDims); err != nil {
		return nil, err
	}

	rows, err := idx.db.QueryContext(ctx, `
SELECT m.page_id, v.distance, m.page_version, m.source_digest, m.compiler_version
FROM warm_pages_vec v
JOIN warm_page_vec_map m ON m.rowid = v.rowid
WHERE v.embedding MATCH ?
  AND v.k = ?
  AND v.session_id = ?
  AND v.embedding_namespace = ?
  AND m.embedding_namespace = ?
ORDER BY v.distance ASC, m.page_id ASC
`, vectorJSON(vector), fetch, q.SessionID, namespace, namespace)
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
		var pageVersion int64
		var sourceDigest, compilerVersion string
		if err := rows.Scan(&pageID, &distance, &pageVersion, &sourceDigest, &compilerVersion); err != nil {
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
		if int64(page.Version) != pageVersion || string(page.SourceDigest) != sourceDigest || string(page.CompilerVersion) != compilerVersion {
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
		if q.Commit != "" && page.Commit != "" && page.Commit != q.Commit {
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
			Explanation: fmt.Sprintf("channel=vec0 metric=cosine namespace=%s distance=%.6f filters=session,namespace,status=active,page-version page=%s",
				namespace, distance, page.ID),
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

func normalizeVectorNamespace(value string) (string, error) {
	if value == "" {
		return DefaultVectorNamespace, nil
	}
	if len(value) > 512 || strings.TrimSpace(value) != value {
		return "", fmt.Errorf("invalid embedding namespace")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("invalid embedding namespace")
		}
	}
	return value, nil
}

func copyCompatibleVectors(ctx context.Context, from *sql.DB, to *Index) (int, error) {
	rows, err := from.QueryContext(ctx, `
SELECT m.page_id, m.session_id, m.page_version, m.source_digest,
       m.compiler_version, m.embedding_namespace, vec_to_json(v.embedding)
FROM warm_page_vec_map m
JOIN warm_pages_vec v ON v.rowid = m.rowid
ORDER BY m.page_id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var compatible []VectorRecord
	for rows.Next() {
		if err := contextErr(ctx); err != nil {
			return 0, err
		}
		var pageID, sessionID, sourceDigest, compilerVersion, namespace, encoded string
		var pageVersion int64
		if err := rows.Scan(&pageID, &sessionID, &pageVersion, &sourceDigest, &compilerVersion, &namespace, &encoded); err != nil {
			return 0, err
		}
		var currentVersion int64
		var currentDigest, currentCompiler, status string
		err := to.db.QueryRowContext(ctx,
			`SELECT page_version, source_digest, compiler_version, status FROM warm_pages WHERE page_id = ? AND session_id = ?`,
			pageID, sessionID,
		).Scan(&currentVersion, &currentDigest, &currentCompiler, &status)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, err
		}
		if currentVersion != pageVersion || currentDigest != sourceDigest || currentCompiler != compilerVersion || status != string(pages.StatusActive) {
			continue
		}
		var vector []float64
		if err := json.Unmarshal([]byte(encoded), &vector); err != nil {
			return 0, err
		}
		compatible = append(compatible, VectorRecord{
			PageID: pages.PageID(pageID), SessionID: sessionID,
			EmbeddingNamespace: namespace, Vector: vector,
		})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(compatible) > 0 {
		if err := to.UpsertVectors(ctx, compatible); err != nil {
			return 0, err
		}
	}
	return len(compatible), nil
}
