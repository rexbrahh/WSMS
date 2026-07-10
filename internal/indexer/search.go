package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"wsms/internal/pages"
	"wsms/internal/types"
)

// SearchQuery is a filtered lexical search request.
type SearchQuery struct {
	SessionID  string
	RepoID     string
	TaskID     string
	Branch     string
	Kinds      []pages.PageKind
	Trust      []pages.Trust
	Text       string
	Limit      int
	ActiveOnly bool
}

// Candidate is one ranked warm page from the index.
type Candidate struct {
	Page        pages.WarmPage
	Rank        float64
	Explanation string
}

// SearchLexical runs FTS5 BM25 over hard-filtered active pages.
func (idx *Index) SearchLexical(ctx context.Context, q SearchQuery) ([]Candidate, error) {
	if err := idx.guard(ctx); err != nil {
		return nil, err
	}
	if q.SessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	match, err := buildFTSMatch(q.Text)
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
	// Phase 7B lexical search only surfaces active pages. Stale/invalidated rows
	// remain in warm_pages for diagnostics but are excluded from FTS joins.

	args := []any{match, q.SessionID}
	var b strings.Builder
	b.WriteString(`
SELECT p.page_id, p.page_version, p.session_id, p.repo_id, p.task_id, p.branch, p.commit_id,
       p.path_scope_json, p.scope, p.kind, p.trust, p.status, p.salience, p.salience_reason,
       p.search_text, p.summary, p.refs_json, p.source_digest, p.source_seq_min, p.source_seq_max,
       p.compiler_version, p.scope_epoch, p.created_at, p.last_verified_at,
       bm25(warm_pages_fts) AS rank
FROM warm_pages_fts
JOIN warm_pages p ON p.page_id = warm_pages_fts.page_id
WHERE warm_pages_fts MATCH ?
  AND p.session_id = ?
  AND p.status = 'active'
`)
	if q.RepoID != "" {
		b.WriteString(` AND p.repo_id = ?`)
		args = append(args, q.RepoID)
	}
	if q.TaskID != "" {
		b.WriteString(` AND (p.task_id = '' OR p.task_id = ?)`)
		args = append(args, q.TaskID)
	}
	if q.Branch != "" {
		b.WriteString(` AND (p.branch = '' OR p.branch = ?)`)
		args = append(args, q.Branch)
	}
	if len(q.Kinds) > 0 {
		b.WriteString(` AND p.kind IN (`)
		for i, kind := range q.Kinds {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('?')
			args = append(args, string(kind))
		}
		b.WriteByte(')')
	}
	if len(q.Trust) > 0 {
		b.WriteString(` AND p.trust IN (`)
		for i, trust := range q.Trust {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('?')
			args = append(args, string(trust))
		}
		b.WriteByte(')')
	}
	b.WriteString(` ORDER BY rank ASC, p.page_id ASC LIMIT ?`)
	args = append(args, limit)

	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.db == nil {
		return nil, ErrClosed
	}

	rows, err := idx.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		page, rank, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		filters := []string{"session", "status=active"}
		if q.RepoID != "" {
			filters = append(filters, "repo")
		}
		if q.Branch != "" {
			filters = append(filters, "branch")
		}
		if q.TaskID != "" {
			filters = append(filters, "task")
		}
		out = append(out, Candidate{
			Page: page,
			Rank: rank,
			Explanation: fmt.Sprintf("channel=fts5 bm25=%.6f filters=%s page=%s",
				rank, strings.Join(filters, ","), page.ID),
		})
	}
	return out, rows.Err()
}

func scanPage(rows *sql.Rows) (pages.WarmPage, float64, error) {
	var (
		pageID, sessionID, repoID, taskID, branch, commitID string
		pathScopeJSON, scope, kind, trust, status           string
		salienceReason, searchText, summary, refsJSON       string
		sourceDigest, compilerVersion, createdAt, verified  string
		pageVersion, sourceSeqMin, sourceSeqMax, scopeEpoch int64
		salience, rank                                      float64
	)
	if err := rows.Scan(
		&pageID, &pageVersion, &sessionID, &repoID, &taskID, &branch, &commitID,
		&pathScopeJSON, &scope, &kind, &trust, &status, &salience, &salienceReason,
		&searchText, &summary, &refsJSON, &sourceDigest, &sourceSeqMin, &sourceSeqMax,
		&compilerVersion, &scopeEpoch, &createdAt, &verified, &rank,
	); err != nil {
		return pages.WarmPage{}, 0, err
	}
	var refs []pages.PageRef
	if err := json.Unmarshal([]byte(refsJSON), &refs); err != nil {
		return pages.WarmPage{}, 0, fmt.Errorf("decode refs: %w", err)
	}
	var paths []string
	if pathScopeJSON != "" {
		if err := json.Unmarshal([]byte(pathScopeJSON), &paths); err != nil {
			return pages.WarmPage{}, 0, fmt.Errorf("decode path scope: %w", err)
		}
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		created, err = time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return pages.WarmPage{}, 0, fmt.Errorf("decode created_at: %w", err)
		}
	}
	var lastVerified time.Time
	if verified != "" {
		lastVerified, err = time.Parse(time.RFC3339Nano, verified)
		if err != nil {
			lastVerified, _ = time.Parse(time.RFC3339, verified)
		}
	}
	return pages.WarmPage{
		ID: pages.PageID(pageID), Version: pages.PageVersion(pageVersion), SessionID: sessionID,
		RepoID: repoID, TaskID: taskID, Branch: branch, Commit: commitID, PathScope: paths,
		Scope: types.Scope(scope), Kind: pages.PageKind(kind), Trust: pages.Trust(trust),
		Status: pages.Status(status), Salience: salience, SalienceReason: salienceReason,
		SearchText: searchText, Summary: summary, Refs: refs,
		SourceSeqMin: sourceSeqMin, SourceSeqMax: sourceSeqMax,
		SourceDigest: pages.SourceDigest(sourceDigest), CompilerVersion: pages.CompilerVersion(compilerVersion),
		ScopeEpoch: pages.ScopeEpoch(scopeEpoch), CreatedAt: created.UTC(), LastVerifiedAt: lastVerified.UTC(),
	}, rank, nil
}

// buildFTSMatch tokenizes user text into a safe FTS5 MATCH expression.
// Tokens are AND-combined; each token is double-quoted after escaping.
func buildFTSMatch(text string) (string, error) {
	tokens := tokenize(text)
	if len(tokens) == 0 {
		return "", ErrEmptyQuery
	}
	parts := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		parts = append(parts, `"`+strings.ReplaceAll(tok, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " AND "), nil
}

func tokenize(text string) []string {
	var tokens []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		tok := strings.ToLower(b.String())
		b.Reset()
		if len(tok) < 2 {
			return
		}
		tokens = append(tokens, tok)
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == '/' {
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	// Cap token count to keep MATCH small.
	if len(tokens) > 12 {
		tokens = tokens[:12]
	}
	return tokens
}
