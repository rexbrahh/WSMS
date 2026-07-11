package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"wsms/internal/pages"
	"wsms/internal/types"
)

// SearchQuery is the shared hard-filter request for lexical and dense search.
type SearchQuery struct {
	SessionID string
	RepoID    string
	TaskID    string
	Branch    string
	Commit    string
	// EmbeddingNamespace is used only by dense search. Empty selects the
	// explicit manual/test namespace used before Phase 7D supplies an embedder.
	EmbeddingNamespace string
	Kinds              []pages.PageKind
	Trust              []pages.Trust
	Statuses           []pages.Status
	// ScopeEpochs is the caller's currently admissible authority generation set.
	// Empty preserves the legacy no-epoch-filter behavior; non-empty values are
	// hard-filtered before either lexical or dense channel limits are applied.
	ScopeEpochs []pages.ScopeEpoch
	// EligibilityComplete declares that EligiblePageTuples is the complete
	// caller-authorized page table for this search snapshot. A complete empty
	// set intentionally matches no rows. When false, search retains the legacy
	// inspection behavior and does not apply tuple eligibility.
	EligibilityComplete bool
	// EligiblePageTuples is matched exactly before either lexical LIMIT or dense
	// KNN selection. It is valid only when EligibilityComplete is true.
	EligiblePageTuples []PageTuple
	PathHints          []string
	ExcludedPageIDs    []pages.PageID
	ExcludedRefIDs     []string
	Text               string
	Limit              int
	ActiveOnly         bool
}

// ScoreKind identifies how Candidate.Rank should be interpreted.
type ScoreKind string

const (
	ScoreKindFTS5BM25       ScoreKind = "fts5_bm25"
	ScoreKindCosineDistance ScoreKind = "cosine_distance"
)

// PageTuple is the exact page-table tuple a candidate was indexed against.
type PageTuple struct {
	PageID          pages.PageID          `json:"page_id"`
	PageVersion     pages.PageVersion     `json:"page_version"`
	SessionID       string                `json:"session_id"`
	SourceDigest    pages.SourceDigest    `json:"source_digest"`
	CompilerVersion pages.CompilerVersion `json:"compiler_version"`
	ScopeEpoch      pages.ScopeEpoch      `json:"scope_epoch"`
}

// Validate checks that an exact page identity is complete and representable by
// SQLite. ScopeEpoch zero is a valid initial authority generation.
func (t PageTuple) Validate() error {
	pageID := string(t.PageID)
	if len(pageID) != len("wp_")+32 || !strings.HasPrefix(pageID, "wp_") || !isLowerHexString(pageID[len("wp_"):]) {
		return fmt.Errorf("%w: malformed page id", ErrInvalidPageTuple)
	}
	if t.PageVersion == 0 || strings.TrimSpace(t.SessionID) == "" ||
		t.SourceDigest == "" || t.CompilerVersion == "" {
		return fmt.Errorf("%w: required identity field is empty", ErrInvalidPageTuple)
	}
	if uint64(t.PageVersion) > math.MaxInt64 || uint64(t.ScopeEpoch) > math.MaxInt64 {
		return fmt.Errorf("%w: integer field exceeds SQLite range", ErrInvalidPageTuple)
	}
	if !utf8.ValidString(t.SessionID) || t.SessionID != strings.TrimSpace(t.SessionID) || len(t.SessionID) > 256 {
		return fmt.Errorf("%w: malformed session id", ErrInvalidPageTuple)
	}
	digest := string(t.SourceDigest)
	if len(digest) != 64 || !isLowerHexString(digest) {
		return fmt.Errorf("%w: malformed source digest", ErrInvalidPageTuple)
	}
	if t.CompilerVersion != pages.CurrentCompilerVersion {
		return fmt.Errorf("%w: compiler version is not current", ErrInvalidPageTuple)
	}
	if len(t.CompilerVersion) > 256 {
		return fmt.Errorf("%w: identity field exceeds bound", ErrInvalidPageTuple)
	}
	return nil
}

func isLowerHexString(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// SearchWatermark captures the current session replay watermark used to serve a
// candidate list. It is comparable so callers can reject mixed snapshots.
type SearchWatermark struct {
	SessionID       string
	LastSourceSeq   int64
	LastPageVersion pages.PageVersion
}

// Candidate is one ranked warm page from the index.
type Candidate struct {
	Page              pages.WarmPage
	Rank              float64
	Explanation       string
	Tuple             PageTuple
	ServingGeneration int64
	Watermark         SearchWatermark
	ChannelOrdinal    int
	ScoreKind         ScoreKind
}

// SearchLexical runs FTS5 BM25 over hard-filtered active pages.
func (idx *Index) SearchLexical(ctx context.Context, q SearchQuery) ([]Candidate, error) {
	release, err := idx.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if q.SessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	plan, err := newSearchPlan(q)
	if err != nil {
		return nil, err
	}
	match, err := buildFTSMatch(q.Text)
	if err != nil {
		return nil, err
	}
	// Phase 7B lexical search only surfaces active pages. Stale/invalidated rows
	// remain in warm_pages for diagnostics but are excluded from FTS joins.

	args := []any{match, plan.sessionID}
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
`)
	plan.appendSQL(&b, &args)
	b.WriteString(` ORDER BY rank ASC, p.page_id ASC LIMIT ?`)
	args = append(args, plan.limit)

	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.db == nil {
		return nil, ErrClosed
	}
	tx, err := idx.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	generation, watermark, err := searchSnapshotTx(ctx, tx, plan.sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		if err := contextErr(ctx); err != nil {
			return nil, err
		}
		page, rank, err := scanPageWithRank(rows)
		if err != nil {
			return nil, err
		}
		if !isFinite(rank) {
			return nil, fmt.Errorf("lexical search: non-finite rank")
		}
		ordinal := len(out) + 1
		out = append(out, Candidate{
			Page: page, Rank: rank,
			Tuple:             tupleForPage(page),
			ServingGeneration: generation,
			Watermark:         watermark,
			ChannelOrdinal:    ordinal,
			ScoreKind:         ScoreKindFTS5BM25,
			Explanation: fmt.Sprintf("channel=fts5 score_kind=%s filters=%s page=%s",
				ScoreKindFTS5BM25, strings.Join(plan.filterLabels(), ","), page.ID),
		})
	}
	return out, rows.Err()
}

type searchPlan struct {
	sessionID           string
	repoID              string
	taskID              string
	branch              string
	commit              string
	kinds               []pages.PageKind
	trust               []pages.Trust
	statuses            []pages.Status
	scopeEpochs         []pages.ScopeEpoch
	eligibilityComplete bool
	eligibilityJSON     string
	pathHints           []string
	excludedPageIDs     []pages.PageID
	excludedRefIDs      []string
	limit               int
}

const (
	maxSearchLimit       = 100
	maxSearchFilterTerms = 512
	maxPathHintTerms     = 64
	maxEligibilityTuples = MaxAuthoritySnapshotPages
	maxEligibilityBytes  = 4 << 20
)

func newSearchPlan(q SearchQuery) (searchPlan, error) {
	if strings.TrimSpace(q.SessionID) == "" {
		return searchPlan{}, fmt.Errorf("session id is required")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	statuses := compactStatuses(q.Statuses)
	if q.ActiveOnly || len(statuses) == 0 {
		statuses = []pages.Status{pages.StatusActive}
	}
	scopeEpochs, err := compactScopeEpochs(q.ScopeEpochs)
	if err != nil {
		return searchPlan{}, err
	}
	eligibilityJSON, err := encodeEligibility(q.SessionID, q.EligibilityComplete, q.EligiblePageTuples)
	if err != nil {
		return searchPlan{}, err
	}
	plan := searchPlan{
		sessionID:           strings.TrimSpace(q.SessionID),
		repoID:              strings.TrimSpace(q.RepoID),
		taskID:              strings.TrimSpace(q.TaskID),
		branch:              strings.TrimSpace(q.Branch),
		commit:              strings.TrimSpace(q.Commit),
		kinds:               compactKinds(q.Kinds),
		trust:               compactTrust(q.Trust),
		statuses:            statuses,
		scopeEpochs:         scopeEpochs,
		eligibilityComplete: q.EligibilityComplete,
		eligibilityJSON:     eligibilityJSON,
		pathHints:           compactStrings(q.PathHints, maxPathHintTerms),
		excludedPageIDs:     compactPageIDs(q.ExcludedPageIDs, maxSearchFilterTerms),
		excludedRefIDs:      compactStrings(q.ExcludedRefIDs, maxSearchFilterTerms),
		limit:               limit,
	}
	if len(q.Kinds) > maxSearchFilterTerms || len(q.Trust) > maxSearchFilterTerms || len(q.Statuses) > maxSearchFilterTerms || len(q.ScopeEpochs) > maxSearchFilterTerms ||
		len(q.ExcludedPageIDs) > maxSearchFilterTerms || len(q.ExcludedRefIDs) > maxSearchFilterTerms || len(q.PathHints) > maxPathHintTerms {
		return searchPlan{}, fmt.Errorf("search filters exceed bound")
	}
	return plan, nil
}

func (p searchPlan) appendSQL(b *strings.Builder, args *[]any) {
	if p.repoID != "" {
		b.WriteString(` AND p.repo_id = ?`)
		*args = append(*args, p.repoID)
	}
	if p.taskID != "" {
		b.WriteString(` AND (p.task_id = '' OR p.task_id = ?)`)
		*args = append(*args, p.taskID)
	}
	if p.branch != "" {
		b.WriteString(` AND (p.branch = '' OR p.branch = ?)`)
		*args = append(*args, p.branch)
	}
	if p.commit != "" {
		b.WriteString(` AND (p.commit_id = '' OR p.commit_id = ?)`)
		*args = append(*args, p.commit)
	}
	appendInClause(b, args, `p.kind`, pageKindsToStrings(p.kinds))
	appendInClause(b, args, `p.trust`, trustsToStrings(p.trust))
	appendInClause(b, args, `p.status`, statusesToStrings(p.statuses))
	appendScopeEpochInClause(b, args, p.scopeEpochs)
	p.appendEligibilitySQL(b, args)
	appendNotInClause(b, args, `p.page_id`, pageIDsToStrings(p.excludedPageIDs))
	if len(p.excludedRefIDs) > 0 {
		b.WriteString(` AND NOT EXISTS (
    SELECT 1 FROM json_each(p.refs_json) AS ref
    WHERE json_extract(ref.value, '$.id') IN (`)
		for i, refID := range p.excludedRefIDs {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('?')
			*args = append(*args, refID)
		}
		b.WriteString(`)
  )`)
	}
	if len(p.pathHints) > 0 {
		b.WriteString(` AND (
    p.path_scope_json IN ('[]', 'null')
    OR EXISTS (
      SELECT 1 FROM json_each(p.path_scope_json) AS path_scope
      WHERE `)
		for i, hint := range p.pathHints {
			if i > 0 {
				b.WriteString(` OR `)
			}
			b.WriteString(`(
        CAST(path_scope.value AS TEXT) = ?
        OR CAST(path_scope.value AS TEXT) LIKE ? ESCAPE '\'
        OR ? LIKE replace(replace(replace(CAST(path_scope.value AS TEXT), '\', '\\'), '%', '\%'), '_', '\_') || '/%' ESCAPE '\'
      )`)
			*args = append(*args, hint, pathPrefixPattern(hint), hint)
		}
		b.WriteString(`
    )
  )`)
	}
}

func (p searchPlan) filterLabels() []string {
	labels := []string{"session", "status"}
	if p.repoID != "" {
		labels = append(labels, "repo")
	}
	if p.taskID != "" {
		labels = append(labels, "task")
	}
	if p.branch != "" {
		labels = append(labels, "branch")
	}
	if p.commit != "" {
		labels = append(labels, "commit")
	}
	if len(p.kinds) > 0 {
		labels = append(labels, "kind")
	}
	if len(p.trust) > 0 {
		labels = append(labels, "trust")
	}
	if len(p.pathHints) > 0 {
		labels = append(labels, "path")
	}
	if len(p.scopeEpochs) > 0 {
		labels = append(labels, "scope-epoch")
	}
	if p.eligibilityComplete {
		labels = append(labels, "page-authority")
	}
	if len(p.excludedPageIDs) > 0 {
		labels = append(labels, "excluded-page")
	}
	if len(p.excludedRefIDs) > 0 {
		labels = append(labels, "excluded-ref")
	}
	return labels
}

func appendInClause(b *strings.Builder, args *[]any, column string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString(` AND `)
	b.WriteString(column)
	b.WriteString(` IN (`)
	for i, value := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		*args = append(*args, value)
	}
	b.WriteByte(')')
}

func appendNotInClause(b *strings.Builder, args *[]any, column string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString(` AND `)
	b.WriteString(column)
	b.WriteString(` NOT IN (`)
	for i, value := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		*args = append(*args, value)
	}
	b.WriteByte(')')
}

func appendScopeEpochInClause(b *strings.Builder, args *[]any, values []pages.ScopeEpoch) {
	if len(values) == 0 {
		return
	}
	b.WriteString(` AND p.scope_epoch IN (`)
	for i, value := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		*args = append(*args, int64(value))
	}
	b.WriteByte(')')
}

func (p searchPlan) appendEligibilitySQL(b *strings.Builder, args *[]any) {
	if !p.eligibilityComplete {
		return
	}
	if p.eligibilityJSON == "" {
		b.WriteString(` AND 0`)
		return
	}
	b.WriteString(` AND EXISTS (
    SELECT 1
    FROM json_each(?) AS eligible
    WHERE p.page_id = json_extract(eligible.value, '$.page_id')
      AND p.page_version = json_extract(eligible.value, '$.page_version')
      AND p.session_id = json_extract(eligible.value, '$.session_id')
      AND p.source_digest = json_extract(eligible.value, '$.source_digest')
      AND p.compiler_version = json_extract(eligible.value, '$.compiler_version')
      AND p.scope_epoch = json_extract(eligible.value, '$.scope_epoch')
  )`)
	*args = append(*args, p.eligibilityJSON)
}

func encodeEligibility(sessionID string, complete bool, tuples []PageTuple) (string, error) {
	if !complete {
		if len(tuples) != 0 {
			return "", fmt.Errorf("%w: tuple set requires EligibilityComplete", ErrInvalidPageTuple)
		}
		return "", nil
	}
	if len(tuples) == 0 {
		return "", nil
	}
	if len(tuples) > maxEligibilityTuples {
		return "", fmt.Errorf("%w: %d tuples exceeds %d", ErrEligibilitySetTooLarge, len(tuples), maxEligibilityTuples)
	}
	sessionID = strings.TrimSpace(sessionID)
	deduped := make([]PageTuple, 0, len(tuples))
	seen := make(map[PageTuple]struct{}, len(tuples))
	for _, tuple := range tuples {
		if err := tuple.Validate(); err != nil {
			return "", err
		}
		if tuple.SessionID != sessionID {
			return "", fmt.Errorf("%w: tuple session does not match query", ErrInvalidPageTuple)
		}
		if _, ok := seen[tuple]; ok {
			continue
		}
		seen[tuple] = struct{}{}
		deduped = append(deduped, tuple)
	}
	encoded, err := json.Marshal(deduped)
	if err != nil {
		return "", fmt.Errorf("encode eligibility: %w", err)
	}
	if len(encoded) > maxEligibilityBytes {
		return "", fmt.Errorf("%w: encoded tuple set is %d bytes", ErrEligibilitySetTooLarge, len(encoded))
	}
	return string(encoded), nil
}

func compactKinds(values []pages.PageKind) []pages.PageKind {
	out := make([]pages.PageKind, 0, len(values))
	seen := map[pages.PageKind]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func compactTrust(values []pages.Trust) []pages.Trust {
	out := make([]pages.Trust, 0, len(values))
	seen := map[pages.Trust]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func compactScopeEpochs(values []pages.ScopeEpoch) ([]pages.ScopeEpoch, error) {
	out := make([]pages.ScopeEpoch, 0, len(values))
	seen := make(map[pages.ScopeEpoch]bool, len(values))
	for _, value := range values {
		if uint64(value) > math.MaxInt64 {
			return nil, fmt.Errorf("scope epoch %d exceeds SQLite integer range", value)
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}

func compactStatuses(values []pages.Status) []pages.Status {
	out := make([]pages.Status, 0, len(values))
	seen := map[pages.Status]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func compactPageIDs(values []pages.PageID, max int) []pages.PageID {
	out := make([]pages.PageID, 0, len(values))
	seen := map[pages.PageID]bool{}
	for _, value := range values {
		if len(out) >= max {
			break
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func compactStrings(values []string, max int) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if len(out) >= max {
			break
		}
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func pageKindsToStrings(values []pages.PageKind) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func trustsToStrings(values []pages.Trust) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func statusesToStrings(values []pages.Status) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func pageIDsToStrings(values []pages.PageID) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}

func pathPrefixPattern(value string) string {
	value = strings.TrimSuffix(escapeLike(value), "/")
	return value + "/%"
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func tupleForPage(page pages.WarmPage) PageTuple {
	return PageTuple{
		PageID: page.ID, PageVersion: page.Version, SessionID: page.SessionID,
		SourceDigest: page.SourceDigest, CompilerVersion: page.CompilerVersion,
		ScopeEpoch: page.ScopeEpoch,
	}
}

func searchSnapshotTx(ctx context.Context, tx *sql.Tx, sessionID string) (int64, SearchWatermark, error) {
	var genStr string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key = ?`, metaGeneration).Scan(&genStr); err != nil {
		return 0, SearchWatermark{}, err
	}
	var generation int64
	if _, err := fmt.Sscan(genStr, &generation); err != nil || generation <= 0 {
		return 0, SearchWatermark{}, fmt.Errorf("invalid warm index generation %q", genStr)
	}
	watermark := SearchWatermark{SessionID: sessionID}
	var lastVersion int64
	err := tx.QueryRowContext(ctx,
		`SELECT last_source_seq, last_page_version FROM index_watermarks WHERE session_id = ?`,
		sessionID,
	).Scan(&watermark.LastSourceSeq, &lastVersion)
	if err != nil && err != sql.ErrNoRows {
		return 0, SearchWatermark{}, err
	}
	if lastVersion > 0 {
		watermark.LastPageVersion = pages.PageVersion(lastVersion)
	}
	return generation, watermark, nil
}

func scanPageWithRank(rows *sql.Rows) (pages.WarmPage, float64, error) {
	var rank float64
	page, err := scanPageFromScanner(rankScanner{rows: rows, rank: &rank})
	return page, rank, err
}

// rankScanner scans the warm_pages columns plus a trailing bm25 rank.
type rankScanner struct {
	rows *sql.Rows
	rank *float64
}

func (r rankScanner) Scan(dest ...any) error {
	args := append(dest, r.rank)
	return r.rows.Scan(args...)
}

func scanPageFromScanner(row scannable) (pages.WarmPage, error) {
	var (
		pageID, sessionID, repoID, taskID, branch, commitID string
		pathScopeJSON, scope, kind, trust, status           string
		salienceReason, searchText, summary, refsJSON       string
		sourceDigest, compilerVersion, createdAt, verified  string
		pageVersion, sourceSeqMin, sourceSeqMax, scopeEpoch int64
		salience                                            float64
	)
	if err := row.Scan(
		&pageID, &pageVersion, &sessionID, &repoID, &taskID, &branch, &commitID,
		&pathScopeJSON, &scope, &kind, &trust, &status, &salience, &salienceReason,
		&searchText, &summary, &refsJSON, &sourceDigest, &sourceSeqMin, &sourceSeqMax,
		&compilerVersion, &scopeEpoch, &createdAt, &verified,
	); err != nil {
		return pages.WarmPage{}, err
	}
	var refs []pages.PageRef
	if err := json.Unmarshal([]byte(refsJSON), &refs); err != nil {
		return pages.WarmPage{}, fmt.Errorf("decode refs: %w", err)
	}
	var paths []string
	if pathScopeJSON != "" {
		if err := json.Unmarshal([]byte(pathScopeJSON), &paths); err != nil {
			return pages.WarmPage{}, fmt.Errorf("decode path scope: %w", err)
		}
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		created, err = time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return pages.WarmPage{}, fmt.Errorf("decode created_at: %w", err)
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
	}, nil
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
