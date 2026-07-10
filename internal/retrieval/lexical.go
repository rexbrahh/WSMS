// Package retrieval owns typed query intent, hard filters, lexical ranking,
// abstention, and explanations. It does not own L4 bytes or provider clients.
package retrieval

import (
	"context"
	"fmt"
	"strings"

	"wsms/internal/indexer"
	"wsms/internal/pages"
)

// RecheckFunc optionally re-validates a candidate against current authority
// (coherence epoch, invalidation, etc.). Return false to suppress.
type RecheckFunc func(context.Context, pages.WarmPage) (bool, string)

// LexicalRetriever resolves semantic intents using FTS only.
type LexicalRetriever struct {
	Index   *indexer.Index
	Recheck RecheckFunc
}

// Result is a successful lexical retrieval.
type Result struct {
	Candidates   []indexer.Candidate
	Materialized []MaterializedPage
	Explanation  string
	Degraded     []string // e.g. dense unavailable
}

// MaterializedPage is bounded exact evidence admitted only after the indexed
// candidate passes current L4/coherence validation.
type MaterializedPage struct {
	PageID   pages.PageID
	Evidence []string
}

// ResolveSemantic runs hard filters → FTS → post-filters → abstention.
func (r *LexicalRetriever) ResolveSemantic(ctx context.Context, intent QueryIntent) (Result, error) {
	if r == nil || r.Index == nil {
		return Result{}, ErrIndexUnavailable
	}
	if err := intent.validate(); err != nil {
		return Result{}, err
	}
	if ctx == nil {
		return Result{}, fmt.Errorf("nil context")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	limit := intent.CandidateLimit
	if limit <= 0 {
		limit = 20
	}
	materialize := intent.MaterializeLimit
	if materialize <= 0 {
		materialize = 3
	}

	mode := intent.Mode
	if mode == "" {
		mode = ModeSemanticFault
	}

	q := indexer.SearchQuery{
		SessionID:  intent.SessionID,
		RepoID:     intent.RepoID,
		TaskID:     intent.TaskID,
		Branch:     intent.Branch,
		Commit:     intent.Commit,
		Kinds:      intent.AllowedKinds,
		Trust:      intent.RequiredTrust,
		Text:       intent.searchText(),
		Limit:      limit,
		ActiveOnly: true,
	}
	candidates, err := r.Index.SearchLexical(ctx, q)
	if err != nil {
		if err == indexer.ErrEmptyQuery {
			return Result{}, ErrSemanticPageMiss
		}
		return Result{}, fmt.Errorf("%w: %v", ErrIndexUnavailable, err)
	}

	exclude := map[string]bool{}
	for _, id := range intent.Exclusions {
		exclude[id] = true
	}
	pathHints := normalizeHints(intent.PathHints)

	var kept []indexer.Candidate
	var suppressions []string
	for _, cand := range candidates {
		if exclude[string(cand.Page.ID)] {
			suppressions = append(suppressions, "excluded:"+string(cand.Page.ID))
			continue
		}
		// Drop candidates whose WSL refs are explicitly excluded.
		skip := false
		for _, ref := range cand.Page.Refs {
			if ref.Kind == pages.RefWSLRecord && exclude[ref.ID] {
				suppressions = append(suppressions, "excluded_ref:"+ref.ID)
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if len(pathHints) > 0 && !pathAffinity(cand.Page, pathHints) {
			// Soft preference: do not hard-drop when page has no path scope.
			if len(cand.Page.PathScope) > 0 {
				suppressions = append(suppressions, "path_mismatch:"+string(cand.Page.ID))
				continue
			}
		}
		if cand.Page.Status != pages.StatusActive {
			suppressions = append(suppressions, "status:"+string(cand.Page.ID))
			continue
		}
		if r.Recheck != nil {
			ok, reason := r.Recheck(ctx, cand.Page)
			if !ok {
				if reason == "" {
					reason = "recheck"
				}
				suppressions = append(suppressions, reason+":"+string(cand.Page.ID))
				continue
			}
		}
		// Session isolation belt-and-suspenders.
		if cand.Page.SessionID != intent.SessionID {
			suppressions = append(suppressions, "session_mismatch:"+string(cand.Page.ID))
			continue
		}
		kept = append(kept, cand)
		if len(kept) >= materialize {
			break
		}
	}

	if len(kept) == 0 {
		return Result{}, ErrSemanticPageMiss
	}

	explain := fmt.Sprintf("mode=%s channel=fts5 candidates=%d selected=%d suppressions=%d",
		mode, len(candidates), len(kept), len(suppressions))
	if len(suppressions) > 0 && len(suppressions) <= 8 {
		explain += " reasons=[" + strings.Join(suppressions, "; ") + "]"
	}
	return Result{
		Candidates:  kept,
		Explanation: explain,
		Degraded:    []string{"dense=unavailable"},
	}, nil
}

func normalizeHints(hints []string) []string {
	var out []string
	for _, h := range hints {
		h = strings.TrimSpace(h)
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

func pathAffinity(page pages.WarmPage, hints []string) bool {
	for _, path := range page.PathScope {
		for _, hint := range hints {
			if path == hint || strings.HasPrefix(path, hint) || strings.Contains(path, hint) {
				return true
			}
		}
	}
	return false
}
