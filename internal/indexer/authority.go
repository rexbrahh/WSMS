package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"wsms/internal/pages"
	"wsms/internal/types"
)

const (
	// MaxAuthoritySnapshotPages bounds one exact page-table projection. Crossing
	// it is an operational fault: serving must not truncate and accidentally
	// treat an incomplete tuple list as complete authority.
	MaxAuthoritySnapshotPages = 4096
	maxAuthoritySnapshotBytes = 4 << 20
)

// PageAuthority contains only the descriptor fields needed to ask the current
// coherence table whether an indexed page remains eligible. Search text,
// summaries, and other derivative prose are intentionally excluded.
type PageAuthority struct {
	Tuple     PageTuple
	RefIDs    []string
	Scope     types.Scope
	Branch    string
	Commit    string
	PathScope []string
}

// PageAuthoritySnapshot is one read-transaction view of active page
// descriptors and the index generation/watermark that accompanied them.
type PageAuthoritySnapshot struct {
	ServingGeneration int64
	Watermark         SearchWatermark
	Pages             []PageAuthority
}

// ActivePageSnapshot lists every active indexed page for sessionID in one
// bounded read snapshot. An empty Pages slice is a healthy empty page table;
// an over-bound table returns ErrAuthoritySnapshotTooLarge without truncation.
func (idx *Index) ActivePageSnapshot(ctx context.Context, sessionID string) (PageAuthoritySnapshot, error) {
	release, err := idx.beginOperation(ctx)
	if err != nil {
		return PageAuthoritySnapshot{}, err
	}
	defer release()
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return PageAuthoritySnapshot{}, fmt.Errorf("session id is required")
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.db == nil {
		return PageAuthoritySnapshot{}, ErrClosed
	}
	tx, err := idx.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PageAuthoritySnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()

	generation, watermark, err := searchSnapshotTx(ctx, tx, sessionID)
	if err != nil {
		return PageAuthoritySnapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT page_id, page_version, session_id, source_digest, compiler_version, scope_epoch,
       refs_json, kind, trust, scope, repo_id, task_id, branch, commit_id, path_scope_json
FROM warm_pages
WHERE session_id = ? AND status = ?
ORDER BY page_id ASC
LIMIT ?`, sessionID, string(pages.StatusActive), MaxAuthoritySnapshotPages+1)
	if err != nil {
		return PageAuthoritySnapshot{}, fmt.Errorf("active page snapshot: %w", err)
	}
	defer rows.Close()

	out := PageAuthoritySnapshot{
		ServingGeneration: generation,
		Watermark:         watermark,
		Pages:             make([]PageAuthority, 0),
	}
	totalDescriptorBytes := 0
	for rows.Next() {
		if err := contextErr(ctx); err != nil {
			return PageAuthoritySnapshot{}, err
		}
		if len(out.Pages) == MaxAuthoritySnapshotPages {
			return PageAuthoritySnapshot{}, fmt.Errorf("%w: session has more than %d active pages", ErrAuthoritySnapshotTooLarge, MaxAuthoritySnapshotPages)
		}
		var (
			authority                                    PageAuthority
			pageVersion, scopeEpoch                      int64
			pageID, tupleSession                         string
			sourceDigest, compilerVersion                string
			refsJSON, kind, trust, scope, repoID, taskID string
			branch, commitID, pathsJSON                  string
		)
		if err := rows.Scan(
			&pageID, &pageVersion, &tupleSession, &sourceDigest, &compilerVersion, &scopeEpoch,
			&refsJSON, &kind, &trust, &scope, &repoID, &taskID, &branch, &commitID, &pathsJSON,
		); err != nil {
			return PageAuthoritySnapshot{}, authorityDescriptorCorrupt(err)
		}
		if pageVersion <= 0 || scopeEpoch < 0 {
			return PageAuthoritySnapshot{}, authorityDescriptorCorrupt(fmt.Errorf("%w: stored tuple integer is invalid", ErrInvalidPageTuple))
		}
		authority.Tuple = PageTuple{
			PageID:          pages.PageID(pageID),
			PageVersion:     pages.PageVersion(pageVersion),
			SessionID:       tupleSession,
			SourceDigest:    pages.SourceDigest(sourceDigest),
			CompilerVersion: pages.CompilerVersion(compilerVersion),
			ScopeEpoch:      pages.ScopeEpoch(scopeEpoch),
		}
		if authority.Tuple.SessionID != sessionID {
			return PageAuthoritySnapshot{}, authorityDescriptorCorrupt(fmt.Errorf("%w: stored tuple crossed session", ErrInvalidPageTuple))
		}
		if err := authority.Tuple.Validate(); err != nil {
			return PageAuthoritySnapshot{}, authorityDescriptorCorrupt(err)
		}
		totalDescriptorBytes += len(pageID) + len(tupleSession) + len(sourceDigest) + len(compilerVersion) +
			len(refsJSON) + len(kind) + len(trust) + len(scope) + len(repoID) + len(taskID) + len(branch) + len(commitID) + len(pathsJSON)
		if totalDescriptorBytes > maxAuthoritySnapshotBytes {
			return PageAuthoritySnapshot{}, fmt.Errorf("%w: descriptor payload exceeds %d bytes", ErrAuthoritySnapshotTooLarge, maxAuthoritySnapshotBytes)
		}
		var refs []pages.PageRef
		if err := decodeAuthorityJSON(refsJSON, &refs); err != nil {
			return PageAuthoritySnapshot{}, authorityDescriptorCorrupt(fmt.Errorf("decode refs: %w", err))
		}
		if err := decodeAuthorityJSON(pathsJSON, &authority.PathScope); err != nil {
			return PageAuthoritySnapshot{}, authorityDescriptorCorrupt(fmt.Errorf("decode paths: %w", err))
		}
		authority.Scope = types.Scope(scope)
		authority.Branch = branch
		authority.Commit = commitID
		if err := pages.ValidateAuthorityDescriptor(
			pages.PageKind(kind), pages.Trust(trust), authority.Scope,
			repoID, taskID, authority.Branch, authority.Commit,
			authority.PathScope, refs,
		); err != nil {
			return PageAuthoritySnapshot{}, authorityDescriptorCorrupt(err)
		}
		authority.RefIDs = logicalAuthorityRefIDs(refs)
		out.Pages = append(out.Pages, authority)
	}
	if err := rows.Err(); err != nil {
		return PageAuthoritySnapshot{}, err
	}
	return out, nil
}

func decodeAuthorityJSON(raw string, dst any) error {
	if raw == "" || !utf8.ValidString(raw) {
		return fmt.Errorf("invalid JSON encoding")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func authorityDescriptorCorrupt(err error) error {
	return fmt.Errorf("%w: %w", ErrAuthorityDescriptorCorrupt, err)
}

func logicalAuthorityRefIDs(refs []pages.PageRef) []string {
	result := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if (ref.Kind != pages.RefWSLRecord && ref.Kind != pages.RefEvent) || ref.ID == "" {
			continue
		}
		if _, ok := seen[ref.ID]; ok {
			continue
		}
		seen[ref.ID] = struct{}{}
		result = append(result, ref.ID)
	}
	sort.Strings(result)
	return result
}
