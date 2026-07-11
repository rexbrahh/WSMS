package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"wsms/internal/pages"
)

// PageSource streams mutations for a full rebuild.
type PageSource interface {
	// ForEach yields ordered mutations. Implementations may stop on error.
	ForEach(context.Context, func(pages.PageMutation) error) error
}

// MutationList is an in-memory PageSource.
type MutationList []pages.PageMutation

// ForEach implements PageSource.
func (m MutationList) ForEach(ctx context.Context, fn func(pages.PageMutation) error) error {
	for _, mut := range m {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := fn(mut); err != nil {
			return err
		}
	}
	return nil
}

// Rebuild builds a new generation into warm.rebuild.db, validates it, then
// atomically cuts over the serving warm.db. Concurrent rebuilds fail with
// ErrRebuildInProgress. An incomplete rebuild file is never served.
func (idx *Index) Rebuild(ctx context.Context, source PageSource) error {
	if idx == nil {
		return ErrClosed
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if source == nil {
		return fmt.Errorf("page source is required")
	}
	if !idx.rebuildMu.TryLock() {
		return ErrRebuildInProgress
	}
	defer idx.rebuildMu.Unlock()
	if idx.lease != nil {
		if !idx.lease.rebuildMu.TryLock() {
			return ErrRebuildInProgress
		}
		defer idx.lease.rebuildMu.Unlock()
		idx.lease.mu.Lock()
		defer idx.lease.mu.Unlock()
		if err := idx.rebindIfStale(ctx); err != nil {
			return err
		}
	}
	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()
	idx.mu.RLock()
	closed := idx.db == nil
	idx.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	lockPath := filepath.Join(idx.dir, rebuildLockName)
	lock, err := acquireRebuildLock(lockPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = lock.Close()
		_ = os.Remove(lockPath)
	}()
	rebuildPath := filepath.Join(idx.dir, rebuildDBName)
	_ = os.Remove(rebuildPath)
	_ = os.Remove(rebuildPath + "-wal")
	_ = os.Remove(rebuildPath + "-shm")

	rebuildDB, err := openDB(rebuildPath)
	if err != nil {
		return err
	}
	// Ensure meta on rebuild DB; preserve dense configuration when enabled.
	tmp := &Index{
		dir:                idx.dir,
		path:               rebuildPath,
		db:                 rebuildDB,
		denseDims:          idx.denseDims,
		embeddingNamespace: idx.embeddingNamespace,
	}
	if err := tmp.ensureMeta(ctx); err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return err
	}
	if idx.denseDims > 0 {
		if err := tmp.enableDense(ctx, idx.denseDims); err != nil {
			_ = rebuildDB.Close()
			_ = os.Remove(rebuildPath)
			return err
		}
	}

	var mutations []pages.PageMutation
	if err := source.ForEach(ctx, func(mut pages.PageMutation) error {
		mutations = append(mutations, mut)
		return nil
	}); err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return err
	}
	if err := tmp.Apply(ctx, mutations); err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return err
	}
	expectedVectors := 0
	if idx.denseDims > 0 {
		expectedVectors, err = copyCompatibleVectors(ctx, idx.db, tmp)
		if err != nil {
			_ = rebuildDB.Close()
			_ = os.Remove(rebuildPath)
			return err
		}
	}
	if err := validateGeneration(ctx, tmp, expectedVectors); err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return fmt.Errorf("rebuild validation: %w", err)
	}

	// Bump generation on rebuild DB before cutover.
	gen, err := readGeneration(ctx, idx.db)
	if err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return err
	}
	nextGen := strconv.FormatInt(gen+1, 10)
	if _, err := rebuildDB.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaGeneration, nextGen); err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return err
	}

	if err := checkpointDB(ctx, rebuildDB); err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return err
	}
	if err := rebuildDB.Close(); err != nil {
		_ = os.Remove(rebuildPath)
		return err
	}
	if err := removeDBSidecars(rebuildPath); err != nil {
		_ = os.Remove(rebuildPath)
		return err
	}

	// Atomic cutover: close serving handle, rename files, reopen.
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.db == nil {
		return ErrClosed
	}
	if err := checkpointDB(ctx, idx.db); err != nil {
		return err
	}
	if err := idx.db.Close(); err != nil {
		return err
	}
	idx.db = nil

	oldPath := idx.path + ".old"
	if err := removeDBFiles(oldPath); err != nil {
		return err
	}
	if err := os.Rename(idx.path, oldPath); err != nil && !os.IsNotExist(err) {
		// Reopen previous if rename failed.
		db, openErr := openDB(idx.path)
		if openErr == nil {
			idx.db = db
		}
		return err
	}
	if err := removeDBSidecars(idx.path); err != nil {
		_ = os.Rename(oldPath, idx.path)
		db, openErr := openDB(idx.path)
		if openErr == nil {
			idx.db = db
		}
		return err
	}
	if err := os.Rename(rebuildPath, idx.path); err != nil {
		// Attempt restore.
		_ = os.Rename(oldPath, idx.path)
		db, openErr := openDB(idx.path)
		if openErr == nil {
			idx.db = db
		}
		return err
	}
	db, err := openDB(idx.path)
	if err != nil {
		_ = os.Remove(idx.path)
		_ = os.Rename(oldPath, idx.path)
		if previous, openErr := openDB(idx.path); openErr == nil {
			idx.db = previous
		}
		return err
	}
	idx.db = db
	idx.generation = gen + 1
	// Re-bind dense dims from the new generation meta.
	if err := idx.initDense(ctx, idx.denseDims); err != nil {
		_ = db.Close()
		idx.db = nil
		_ = os.Remove(idx.path)
		_ = os.Rename(oldPath, idx.path)
		if previous, openErr := openDB(idx.path); openErr == nil {
			idx.db = previous
		}
		return err
	}
	if idx.lease != nil {
		idx.lease.noteGeneration(idx.generation)
	}
	if err := removeDBFiles(oldPath); err != nil {
		return err
	}
	return nil
}

func checkpointDB(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE);`)
	return err
}

type rebuildLockInfo struct {
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"created_at"`
}

func acquireRebuildLock(path string) (*os.File, error) {
	for attempt := 0; attempt < 2; attempt++ {
		lock, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			info := rebuildLockInfo{PID: os.Getpid(), CreatedAt: time.Now().UTC()}
			if err := json.NewEncoder(lock).Encode(info); err != nil {
				_ = lock.Close()
				_ = os.Remove(path)
				return nil, err
			}
			if err := lock.Sync(); err != nil {
				_ = lock.Close()
				_ = os.Remove(path)
				return nil, err
			}
			return lock, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if err := removeStaleRebuildLock(path); err != nil {
			if errors.Is(err, ErrRebuildInProgress) {
				return nil, err
			}
			return nil, err
		}
	}
	return nil, ErrRebuildInProgress
}

func removeStaleRebuildLock(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var info rebuildLockInfo
	if json.Unmarshal(data, &info) == nil && info.PID > 0 && processAlive(info.PID) {
		return ErrRebuildInProgress
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || !errors.Is(err, os.ErrProcessDone)
}

func validateGeneration(ctx context.Context, idx *Index, expectedVectors int) error {
	var version string
	if err := idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaSchemaVersion).Scan(&version); err != nil {
		return err
	}
	if version != schemaVersion {
		return fmt.Errorf("%w: %q", ErrInvalidSchema, version)
	}
	var pagesCount, ftsCount int64
	if err := idx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM warm_pages WHERE status = 'active'`).Scan(&pagesCount); err != nil {
		return err
	}
	if err := idx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM warm_pages_fts`).Scan(&ftsCount); err != nil {
		return err
	}
	if pagesCount != ftsCount {
		return fmt.Errorf("fts count %d != active pages %d", ftsCount, pagesCount)
	}
	if idx.denseDims > 0 {
		var mapCount, rowCount, missingRows, orphanRows int
		if err := idx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM warm_page_vec_map`).Scan(&mapCount); err != nil {
			return err
		}
		if err := idx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM warm_page_vec_rows`).Scan(&rowCount); err != nil {
			return err
		}
		if mapCount != expectedVectors {
			return fmt.Errorf("vector map count %d != compatible source vectors %d", mapCount, expectedVectors)
		}
		if rowCount != expectedVectors {
			return fmt.Errorf("vector shadow row count %d != compatible source vectors %d", rowCount, expectedVectors)
		}
		if err := idx.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM warm_page_vec_map m
LEFT JOIN warm_page_vec_rows r
  ON r.rowid = m.rowid
 AND r.page_id = m.page_id
 AND r.session_id = m.session_id
 AND r.embedding_namespace = m.embedding_namespace
WHERE r.rowid IS NULL`).Scan(&missingRows); err != nil {
			return err
		}
		if missingRows != 0 {
			return fmt.Errorf("vector shadow missing %d map rows", missingRows)
		}
		if err := idx.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM warm_page_vec_rows r
LEFT JOIN warm_page_vec_map m
  ON m.rowid = r.rowid
 AND m.page_id = r.page_id
 AND m.session_id = r.session_id
 AND m.embedding_namespace = r.embedding_namespace
WHERE m.rowid IS NULL`).Scan(&orphanRows); err != nil {
			return err
		}
		if orphanRows != 0 {
			return fmt.Errorf("vector shadow has %d orphan rows", orphanRows)
		}
	}
	return nil
}
