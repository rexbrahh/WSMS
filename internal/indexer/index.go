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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wsms/internal/pages"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

const (
	schemaVersion     = "2"
	servingDBName     = "warm.db"
	rebuildDBName     = "warm.rebuild.db"
	rebuildLockName   = "rebuild.lock"
	metaSchemaVersion = "schema_version"
	metaCompiler      = "compiler_version"
	metaGeneration    = "generation"
	metaCreatedAt     = "created_at"
	metaEmbeddingNS   = "embedding_namespace"
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
	// ErrInvalidCompiler reports derivative rows built by an incompatible page
	// compiler. The index may be deleted and replayed from L4.
	ErrInvalidCompiler = errors.New("unsupported warm index compiler")
	// ErrWatermarkGap reports an attempt to advance past an unapplied source
	// sequence. L3 must catch up from L4 rather than hide the hole.
	ErrWatermarkGap = errors.New("warm index watermark gap")
	// ErrEmbeddingNamespaceMismatch reports an index generation whose stored
	// vector ABI does not match the configured embedder. The generation is
	// disposable and must be replaced before it can serve.
	ErrEmbeddingNamespaceMismatch = errors.New("warm index embedding namespace mismatch")
)

const (
	// EmbeddingStatusOK means the optional dense projection is healthy.
	EmbeddingStatusOK = ""
	// EmbeddingStatusDenseUnavailable means dense tables are not active.
	EmbeddingStatusDenseUnavailable = "dense-unavailable"
	// EmbeddingStatusNamespace means the configured namespace/dimensions do not
	// match the index generation or returned vectors.
	EmbeddingStatusNamespace = "namespace"
	// EmbeddingStatusSelfCheck means the optional embedder readiness probe failed.
	EmbeddingStatusSelfCheck = "self-check"
	// EmbeddingStatusEmbedder means document/query inference failed.
	EmbeddingStatusEmbedder = "embedder"
	// EmbeddingStatusVector means returned vector shape or role validation failed.
	EmbeddingStatusVector = "vector"
	// EmbeddingStatusIndexer means persisting dense rows failed.
	EmbeddingStatusIndexer = "indexer"
	// EmbeddingStatusSearch means the non-authoritative dense shadow search failed.
	EmbeddingStatusSearch = "search"
	// EmbeddingStatusInternal is a redacted catch-all for unexpected callers.
	EmbeddingStatusInternal = "internal"
)

// ValidEmbeddingStatus reports whether category is safe to expose in health.
func ValidEmbeddingStatus(category string) bool {
	switch category {
	case EmbeddingStatusOK,
		EmbeddingStatusDenseUnavailable,
		EmbeddingStatusNamespace,
		EmbeddingStatusSelfCheck,
		EmbeddingStatusEmbedder,
		EmbeddingStatusVector,
		EmbeddingStatusIndexer,
		EmbeddingStatusSearch,
		EmbeddingStatusInternal:
		return true
	default:
		return false
	}
}

// Options configures optional index capabilities.
type Options struct {
	// DenseDimensions enables the sqlite-vec projection when > 0.
	// Zero leaves dense disabled unless the on-disk index already has dense meta.
	DenseDimensions int
	// EmbeddingNamespace is the complete vector ABI identity for this index
	// generation. Empty is an explicit vector-free/manual namespace.
	EmbeddingNamespace string
}

// Index is a generation of disposable warm-page search state.
type Index struct {
	mu         sync.RWMutex
	writeMu    sync.Mutex
	rebuildMu  sync.Mutex
	dir        string
	path       string
	db         *sql.DB
	denseDims  int
	generation int64
	lease      *indexLease

	embeddingNamespace      string
	embeddingDegradedReason string
}

type indexLease struct {
	mu         sync.RWMutex
	rebuildMu  sync.Mutex
	generation atomic.Int64
}

var indexLeases sync.Map // map[absoluteIndexDir]*indexLease

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

	EmbeddingNamespace      string
	EmbeddingDegraded       bool
	EmbeddingDegradedReason string
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
	if err := validateConfiguredEmbeddingNamespace(opt.EmbeddingNamespace); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lease := leaseForDir(dir)
	// Open may recover cutover artifacts or replace an incompatible disposable
	// generation, so it is a directory mutation rather than a read operation.
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if err := recoverIndexFiles(dir); err != nil {
		return nil, err
	}

	path := filepath.Join(dir, servingDBName)
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	idx := &Index{
		dir: dir, path: path, db: db, lease: lease,
		embeddingNamespace: opt.EmbeddingNamespace,
	}
	ctx := context.Background()
	if err := idx.ensureMeta(ctx); err != nil {
		if errors.Is(err, ErrEmbeddingNamespaceMismatch) {
			gen, genErr := readGeneration(ctx, db)
			if genErr != nil {
				_ = db.Close()
				return nil, errors.Join(err, genErr)
			}
			_ = db.Close()
			if err := removeDBFiles(path); err != nil {
				return nil, err
			}
			db, err = openDB(path)
			if err != nil {
				return nil, err
			}
			idx.db = db
			if err := idx.ensureMeta(ctx); err != nil {
				_ = db.Close()
				return nil, err
			}
			if err := writeGeneration(ctx, db, gen+1); err != nil {
				_ = db.Close()
				return nil, err
			}
		} else {
			if !errors.Is(err, ErrInvalidSchema) && !errors.Is(err, ErrInvalidCompiler) {
				_ = db.Close()
				return nil, err
			}
			_ = db.Close()
			if err := removeDBFiles(path); err != nil {
				return nil, err
			}
			db, err = openDB(path)
			if err != nil {
				return nil, err
			}
			idx.db = db
			if err := idx.ensureMeta(ctx); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
	}
	if err := idx.initDense(ctx, opt.DenseDimensions); err != nil {
		_ = db.Close()
		return nil, err
	}
	gen, err := readGeneration(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	idx.generation = gen
	// The serving file is authoritative while the exclusive lease is held. An
	// operator may have safely deleted a fully closed disposable index between
	// opens, in which case the new generation legitimately resets to one.
	lease.generation.Store(gen)
	return idx, nil
}

func leaseForDir(dir string) *indexLease {
	key, err := filepath.Abs(dir)
	if err != nil {
		key = filepath.Clean(dir)
	}
	// MkdirAll runs before this lookup, so EvalSymlinks can collapse aliases to
	// the same physical directory and prevent parallel leases over one WAL set.
	if physical, err := filepath.EvalSymlinks(key); err == nil {
		key = physical
	}
	actual, _ := indexLeases.LoadOrStore(key, &indexLease{})
	return actual.(*indexLease)
}

func (lease *indexLease) noteGeneration(gen int64) {
	for {
		current := lease.generation.Load()
		if gen <= current {
			return
		}
		if lease.generation.CompareAndSwap(current, gen) {
			return
		}
	}
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
			{metaEmbeddingNS, idx.embeddingNamespace},
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
	var compiler string
	if err := idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaCompiler).Scan(&compiler); err != nil {
		return err
	}
	if compiler != string(pages.CurrentCompilerVersion) {
		return fmt.Errorf("%w: got %q want %q", ErrInvalidCompiler, compiler, pages.CurrentCompilerVersion)
	}
	var storedNamespace string
	err = idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaEmbeddingNS).Scan(&storedNamespace)
	if errors.Is(err, sql.ErrNoRows) {
		// Vector-free legacy generations may be adopted in place. Configuring an
		// embedder against a legacy generation is an ABI transition and must bump
		// the generation so replay reconstructs pages and FTS from L4.
		if idx.embeddingNamespace != "" {
			return fmt.Errorf("%w: stored legacy namespace is empty, configured %q", ErrEmbeddingNamespaceMismatch, idx.embeddingNamespace)
		}
		_, err = idx.db.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES(?, ?)`, metaEmbeddingNS, "")
		return err
	}
	if err != nil {
		return err
	}
	if storedNamespace != idx.embeddingNamespace {
		return fmt.Errorf("%w: stored %q configured %q", ErrEmbeddingNamespaceMismatch, storedNamespace, idx.embeddingNamespace)
	}
	return nil
}

func recoverIndexFiles(dir string) error {
	if err := removeStaleRebuildLock(filepath.Join(dir, rebuildLockName)); err != nil {
		return err
	}
	serving := filepath.Join(dir, servingDBName)
	backup := serving + ".old"
	rebuild := filepath.Join(dir, rebuildDBName)
	if _, err := os.Stat(serving); os.IsNotExist(err) {
		switch {
		case fileExists(backup):
			if err := os.Rename(backup, serving); err != nil {
				return fmt.Errorf("restore previous warm index: %w", err)
			}
		case fileExists(rebuild):
			if err := os.Rename(rebuild, serving); err != nil {
				return fmt.Errorf("promote rebuilt warm index: %w", err)
			}
		}
	} else if err != nil {
		return err
	}
	if fileExists(serving) {
		for _, path := range []string{rebuild, rebuild + "-wal", rebuild + "-shm", backup, backup + "-wal", backup + "-shm"} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func removeDBFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func removeDBSidecars(path string) error {
	for _, candidate := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()
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
	release, err := idx.beginOperation(ctx)
	if err != nil {
		return Health{}, err
	}
	defer release()
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.db == nil {
		return Health{}, ErrClosed
	}
	h := Health{
		Ready: true, Path: idx.path,
		DenseEnabled: idx.denseDims > 0, DenseDimensions: idx.denseDims,
		EmbeddingNamespace:      idx.embeddingNamespace,
		EmbeddingDegraded:       idx.embeddingDegradedReason != "",
		EmbeddingDegradedReason: idx.embeddingDegradedReason,
	}
	if h.DenseEnabled {
		h.DenseMetric = denseMetricCosine
	}
	_ = idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaSchemaVersion).Scan(&h.SchemaVersion)
	_ = idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaCompiler).Scan(&h.CompilerVersion)
	_ = idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaEmbeddingNS).Scan(&h.EmbeddingNamespace)
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

// RecordEmbeddingStatus updates non-authoritative dense-embedding health. The
// reason must be one of the caller's fixed redacted categories. It does not
// touch SQLite and must never affect page-table or watermark commits.
func (idx *Index) RecordEmbeddingStatus(reason string) {
	if idx == nil {
		return
	}
	if !ValidEmbeddingStatus(reason) {
		reason = EmbeddingStatusInternal
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.embeddingDegradedReason = reason
}

func validateConfiguredEmbeddingNamespace(namespace string) error {
	if namespace == "" {
		return nil
	}
	if len(namespace) > 512 || strings.TrimSpace(namespace) != namespace {
		return fmt.Errorf("invalid embedding namespace")
	}
	for _, r := range namespace {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid embedding namespace")
		}
	}
	return nil
}

func readGeneration(ctx context.Context, db *sql.DB) (int64, error) {
	var genStr string
	if err := db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaGeneration).Scan(&genStr); err != nil {
		return 0, err
	}
	var gen int64
	if _, err := fmt.Sscan(genStr, &gen); err != nil || gen <= 0 {
		return 0, fmt.Errorf("invalid warm index generation %q", genStr)
	}
	return gen, nil
}

func writeGeneration(ctx context.Context, db *sql.DB, generation int64) error {
	if generation <= 0 {
		return fmt.Errorf("invalid warm index generation %d", generation)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaGeneration, fmt.Sprint(generation))
	return err
}

func (idx *Index) beginOperation(ctx context.Context) (func(), error) {
	if idx == nil {
		return nil, ErrClosed
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if idx.lease == nil {
		return func() {}, nil
	}
	idx.lease.mu.RLock()
	release := func() { idx.lease.mu.RUnlock() }
	if err := idx.rebindIfStale(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
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

func (idx *Index) rebindIfStale(ctx context.Context) error {
	if idx == nil {
		return ErrClosed
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if idx.lease == nil {
		return nil
	}
	target := idx.lease.generation.Load()
	if target == 0 {
		return nil
	}
	idx.mu.RLock()
	closed := idx.db == nil
	current := idx.generation
	idx.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if current >= target {
		return nil
	}

	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.db == nil {
		return ErrClosed
	}
	target = idx.lease.generation.Load()
	if idx.generation >= target {
		return nil
	}
	db, err := openDB(idx.path)
	if err != nil {
		return err
	}
	requestedDenseDims := idx.denseDims
	old := idx.db
	idx.db = db
	if err := idx.ensureMeta(ctx); err != nil {
		idx.db = old
		_ = db.Close()
		return err
	}
	if err := idx.initDense(ctx, requestedDenseDims); err != nil {
		idx.db = old
		_ = db.Close()
		return err
	}
	gen, err := readGeneration(ctx, db)
	if err != nil {
		idx.db = old
		_ = db.Close()
		return err
	}
	idx.generation = gen
	idx.lease.noteGeneration(gen)
	return old.Close()
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
