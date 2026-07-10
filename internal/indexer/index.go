// Package indexer owns the disposable L3 warm index (FTS5 metadata store).
//
// It never owns ledger truth, WSL authority, or capsule admission. Deleting the
// index directory is always safe.
package indexer

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wsms/internal/pages"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

const (
	schemaVersion     = "1"
	servingDBName     = "warm.db"
	rebuildDBName     = "warm.rebuild.db"
	rebuildLockName   = "rebuild.lock"
	metaSchemaVersion = "schema_version"
	metaCompiler      = "compiler_version"
	metaGeneration    = "generation"
	metaCreatedAt     = "created_at"
)

var (
	// ErrClosed reports use after Close.
	ErrClosed = errors.New("warm index closed")
	// ErrEmptyQuery reports a text query with no searchable tokens.
	ErrEmptyQuery = errors.New("empty lexical query")
	// ErrDenseUnavailable reports dense search when the optional vec0 projection
	// is not enabled for this index.
	ErrDenseUnavailable = errors.New("dense search unavailable")
	// ErrRebuildInProgress reports a concurrent rebuild lock.
	ErrRebuildInProgress = errors.New("warm index rebuild in progress")
	// ErrInvalidSchema reports an unsupported on-disk schema.
	ErrInvalidSchema = errors.New("unsupported warm index schema")
)

// Options configures optional index capabilities.
type Options struct {
	// DenseDimensions enables the sqlite-vec projection when > 0.
	// Zero leaves dense disabled unless the on-disk index already has dense meta.
	DenseDimensions int
}

// Index is a generation of disposable warm-page search state.
type Index struct {
	mu        sync.RWMutex
	dir       string
	path      string
	db        *sql.DB
	denseDims int
}

// Health is inspectable index status.
type Health struct {
	Ready           bool
	SchemaVersion   string
	CompilerVersion string
	Generation      int64
	PageCount       int64
	Path            string
	DenseEnabled    bool
	DenseDimensions int
	DenseMetric     string
	VectorCount     int64
}

// Open opens or creates the warm index under dir (the index/ directory).
func Open(dir string, opts ...Options) (*Index, error) {
	if dir == "" {
		return nil, fmt.Errorf("index dir is required")
	}
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Orphan rebuild files are never authoritative.
	_ = os.Remove(filepath.Join(dir, rebuildDBName))
	_ = os.Remove(filepath.Join(dir, rebuildDBName+"-wal"))
	_ = os.Remove(filepath.Join(dir, rebuildDBName+"-shm"))

	path := filepath.Join(dir, servingDBName)
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	idx := &Index{dir: dir, path: path, db: db}
	ctx := context.Background()
	if err := idx.ensureMeta(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := idx.initDense(ctx, opt.DenseDimensions); err != nil {
		_ = db.Close()
		return nil, err
	}
	return idx, nil
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("warm index schema: %w", err)
	}
	return db, nil
}

func (idx *Index) ensureMeta(ctx context.Context) error {
	var version string
	err := idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaSchemaVersion).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		tx, err := idx.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, pair := range [][2]string{
			{metaSchemaVersion, schemaVersion},
			{metaCompiler, string(pages.CurrentCompilerVersion)},
			{metaGeneration, "1"},
			{metaCreatedAt, now},
		} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES(?, ?)`, pair[0], pair[1]); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if version != schemaVersion {
		return fmt.Errorf("%w: got %q want %q", ErrInvalidSchema, version, schemaVersion)
	}
	return nil
}

// Dir returns the index directory path.
func (idx *Index) Dir() string {
	if idx == nil {
		return ""
	}
	return idx.dir
}

// Close closes the underlying database.
func (idx *Index) Close() error {
	if idx == nil {
		return nil
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.db == nil {
		return nil
	}
	err := idx.db.Close()
	idx.db = nil
	return err
}

// Health reports generation metadata and page counts.
func (idx *Index) Health(ctx context.Context) (Health, error) {
	if err := idx.guard(ctx); err != nil {
		return Health{}, err
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	h := Health{
		Ready: true, Path: idx.path,
		DenseEnabled: idx.denseDims > 0, DenseDimensions: idx.denseDims,
	}
	if h.DenseEnabled {
		h.DenseMetric = denseMetricCosine
	}
	_ = idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaSchemaVersion).Scan(&h.SchemaVersion)
	_ = idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaCompiler).Scan(&h.CompilerVersion)
	var gen string
	if err := idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaGeneration).Scan(&gen); err == nil {
		_, _ = fmt.Sscan(gen, &h.Generation)
	}
	if err := idx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM warm_pages`).Scan(&h.PageCount); err != nil {
		return Health{}, err
	}
	if h.DenseEnabled {
		_ = idx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM warm_page_vec_map`).Scan(&h.VectorCount)
	}
	return h, nil
}

func (idx *Index) guard(ctx context.Context) error {
	if idx == nil || idx.db == nil {
		return ErrClosed
	}
	if ctx == nil {
		return fmt.Errorf("nil context")
	}
	return ctx.Err()
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

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("nil context")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
