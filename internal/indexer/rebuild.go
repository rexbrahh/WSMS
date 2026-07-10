package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	if err := idx.guard(ctx); err != nil {
		return err
	}
	if source == nil {
		return fmt.Errorf("page source is required")
	}
	lockPath := filepath.Join(idx.dir, rebuildLockName)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return ErrRebuildInProgress
		}
		return err
	}
	defer func() {
		_ = lock.Close()
		_ = os.Remove(lockPath)
	}()
	if _, err := lock.WriteString(time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}

	rebuildPath := filepath.Join(idx.dir, rebuildDBName)
	_ = os.Remove(rebuildPath)
	_ = os.Remove(rebuildPath + "-wal")
	_ = os.Remove(rebuildPath + "-shm")

	rebuildDB, err := openDB(rebuildPath)
	if err != nil {
		return err
	}
	// Ensure meta on rebuild DB.
	tmp := &Index{dir: idx.dir, path: rebuildPath, db: rebuildDB}
	if err := tmp.ensureMeta(ctx); err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return err
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
	if err := validateGeneration(ctx, tmp); err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return fmt.Errorf("rebuild validation: %w", err)
	}

	// Bump generation on rebuild DB before cutover.
	var genStr string
	_ = idx.db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaGeneration).Scan(&genStr)
	gen, _ := strconv.ParseInt(genStr, 10, 64)
	nextGen := strconv.FormatInt(gen+1, 10)
	if _, err := rebuildDB.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaGeneration, nextGen); err != nil {
		_ = rebuildDB.Close()
		_ = os.Remove(rebuildPath)
		return err
	}

	if err := rebuildDB.Close(); err != nil {
		_ = os.Remove(rebuildPath)
		return err
	}

	// Atomic cutover: close serving handle, rename files, reopen.
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.db == nil {
		return ErrClosed
	}
	if err := idx.db.Close(); err != nil {
		return err
	}
	idx.db = nil

	oldPath := idx.path + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(idx.path, oldPath); err != nil && !os.IsNotExist(err) {
		// Reopen previous if rename failed.
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
	_ = os.Remove(oldPath)
	_ = os.Remove(oldPath + "-wal")
	_ = os.Remove(oldPath + "-shm")

	db, err := openDB(idx.path)
	if err != nil {
		return err
	}
	idx.db = db
	return nil
}

func validateGeneration(ctx context.Context, idx *Index) error {
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
	return nil
}
