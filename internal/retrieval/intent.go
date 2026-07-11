package retrieval

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"wsms/internal/indexer"
	"wsms/internal/pages"
)

const (
	maxIntentIDBytes          = 256
	maxIntentContextBytes     = 4 * 1024
	maxIntentPathBytes        = 1024
	maxIntentListEntries      = 64
	maxIntentEligibilityPages = indexer.MaxAuthoritySnapshotPages
	maxIntentCandidateLimit   = 100
	maxIntentMaterializeLimit = 3
	maxIntentTokenBudget      = 64 * 1024
)

var defaultSemanticTrust = []pages.Trust{
	pages.TrustUser,
	pages.TrustRepo,
	pages.TrustSystem,
	pages.TrustTool,
	pages.TrustModel,
	pages.TrustMixed,
}

// Mode selects how retrieval results will be consumed.
type Mode string

const (
	ModeSemanticFault Mode = "semantic_fault"
	ModePrefetch      Mode = "prefetch"
	ModeInspection    Mode = "inspection"
)

// QueryIntent is a typed semantic-search request. Session isolation and
// validity are hard filters, never free-text embedding content.
type QueryIntent struct {
	Mode      Mode
	SessionID string
	RepoID    string
	TaskID    string
	Branch    string
	Commit    string
	// ScopeEpochs is the bounded set of authority generations currently
	// admissible to hybrid semantic-fault or prefetch retrieval. Epoch zero is a
	// valid initial generation; an empty set is reserved for inspection and
	// legacy lexical compatibility.
	ScopeEpochs []pages.ScopeEpoch
	// EligibilityComplete distinguishes an authoritative, point-in-time page
	// table snapshot from an unavailable snapshot. For semantic-fault and
	// prefetch modes it must be true even when EligiblePageTuples is empty; a
	// complete empty snapshot is a healthy empty search universe.
	EligibilityComplete bool
	// EligiblePageTuples is the bounded exact page-table universe admitted by
	// the current authority snapshot. Hybrid retrieval carries these tuples to
	// every search channel and accepts no candidate outside this set.
	EligiblePageTuples []indexer.PageTuple

	PathHints        []string
	AllowedKinds     []pages.PageKind
	RequiredTrust    []pages.Trust
	UserText         string
	ActiveGoal       string
	LastFailure      string
	NextAction       string
	Exclusions       []string // page IDs or WSL record IDs to drop
	CandidateLimit   int
	MaterializeLimit int
	TokenBudget      int
}

func (q QueryIntent) validate() error {
	if strings.TrimSpace(q.SessionID) == "" {
		return fmt.Errorf("session_id is required")
	}
	switch q.Mode {
	case "", ModeSemanticFault, ModePrefetch, ModeInspection:
	default:
		return fmt.Errorf("unknown retrieval mode %q", q.Mode)
	}
	text := strings.TrimSpace(q.UserText)
	if text == "" && strings.TrimSpace(q.LastFailure) == "" && strings.TrimSpace(q.ActiveGoal) == "" {
		return fmt.Errorf("query text is required")
	}
	if q.CandidateLimit < 0 || q.CandidateLimit > maxIntentCandidateLimit ||
		q.MaterializeLimit < 0 || q.MaterializeLimit > maxIntentMaterializeLimit {
		return fmt.Errorf("retrieval limits exceed bounds")
	}
	if q.TokenBudget < 0 || q.TokenBudget > maxIntentTokenBudget {
		return fmt.Errorf("token budget exceeds bound")
	}
	for _, field := range []struct{ name, value string }{
		{"session_id", q.SessionID},
		{"repo_id", q.RepoID},
		{"task_id", q.TaskID},
		{"branch", q.Branch},
		{"commit", q.Commit},
	} {
		if err := validateIntentString(field.name, field.value, maxIntentIDBytes); err != nil {
			return err
		}
	}
	for _, field := range []struct{ name, value string }{
		{"user text", q.UserText},
		{"active goal", q.ActiveGoal},
		{"last failure", q.LastFailure},
		{"next action", q.NextAction},
	} {
		if err := validateIntentString(field.name, field.value, maxIntentContextBytes); err != nil {
			return err
		}
	}
	if len(q.ScopeEpochs) > maxIntentListEntries || len(q.PathHints) > maxIntentListEntries || len(q.Exclusions) > maxIntentListEntries ||
		len(q.AllowedKinds) > maxIntentListEntries || len(q.RequiredTrust) > maxIntentListEntries {
		return fmt.Errorf("intent list exceeds bound")
	}
	for _, hint := range q.PathHints {
		if err := validateIntentString("path hint", hint, maxIntentPathBytes); err != nil {
			return err
		}
	}
	for _, epoch := range q.ScopeEpochs {
		if uint64(epoch) > math.MaxInt64 {
			return fmt.Errorf("scope epoch exceeds bound")
		}
	}
	if _, err := q.effectiveEligiblePageTuples(); err != nil {
		return err
	}
	for _, exclusion := range q.Exclusions {
		if err := validateIntentString("exclusion", exclusion, maxIntentIDBytes); err != nil {
			return err
		}
	}
	for _, kind := range q.AllowedKinds {
		if !validIntentKind(kind) {
			return fmt.Errorf("unknown page kind %q", kind)
		}
	}
	for _, trust := range q.effectiveTrust() {
		if !validIntentTrust(trust) {
			return fmt.Errorf("unknown trust %q", trust)
		}
	}
	return nil
}

func (q QueryIntent) effectiveEligiblePageTuples() ([]indexer.PageTuple, error) {
	if len(q.EligiblePageTuples) > maxIntentEligibilityPages {
		return nil, fmt.Errorf("eligible page tuple list exceeds bound")
	}
	if !q.EligibilityComplete {
		if len(q.EligiblePageTuples) != 0 {
			return nil, fmt.Errorf("eligible page tuples require a complete snapshot")
		}
		return nil, nil
	}

	byID := make(map[pages.PageID]indexer.PageTuple, len(q.EligiblePageTuples))
	for _, tuple := range q.EligiblePageTuples {
		if err := validateEligiblePageTuple(tuple, q.SessionID); err != nil {
			return nil, err
		}
		if prior, exists := byID[tuple.PageID]; exists {
			if prior != tuple {
				return nil, fmt.Errorf("eligible page tuple list contains conflicting page versions")
			}
			continue
		}
		byID[tuple.PageID] = tuple
	}
	out := make([]indexer.PageTuple, 0, len(byID))
	for _, tuple := range byID {
		out = append(out, tuple)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PageID < out[j].PageID })
	return out, nil
}

func validateEligiblePageTuple(tuple indexer.PageTuple, sessionID string) error {
	pageID := string(tuple.PageID)
	if len(pageID) != len("wp_")+32 || !strings.HasPrefix(pageID, "wp_") || !isLowerHex(pageID[len("wp_"):]) {
		return fmt.Errorf("eligible page tuple has invalid page id")
	}
	if tuple.PageVersion == 0 || uint64(tuple.PageVersion) > math.MaxInt64 {
		return fmt.Errorf("eligible page tuple has invalid page version")
	}
	if tuple.SessionID == "" || tuple.SessionID != sessionID {
		return fmt.Errorf("eligible page tuple crossed session")
	}
	digest := string(tuple.SourceDigest)
	if len(digest) != 64 || !isLowerHex(digest) {
		return fmt.Errorf("eligible page tuple has invalid source digest")
	}
	if tuple.CompilerVersion != pages.CurrentCompilerVersion {
		return fmt.Errorf("eligible page tuple has non-current compiler version")
	}
	if uint64(tuple.ScopeEpoch) > math.MaxInt64 {
		return fmt.Errorf("eligible page tuple has invalid scope epoch")
	}
	return nil
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validateIntentString(name, value string, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid utf-8", name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds bound", name)
	}
	return nil
}

func validIntentKind(kind pages.PageKind) bool {
	switch kind {
	case pages.KindFailureEpisode, pages.KindDecision, pages.KindConstraint,
		pages.KindTaskCheckpoint, pages.KindKnownGood, pages.KindRepoFact,
		pages.KindFileContext:
		return true
	default:
		return false
	}
}

func validIntentTrust(trust pages.Trust) bool {
	switch trust {
	case pages.TrustUser, pages.TrustRepo, pages.TrustSystem, pages.TrustTool,
		pages.TrustModel, pages.TrustMixed:
		return true
	default:
		return false
	}
}

func (q QueryIntent) effectiveTrust() []pages.Trust {
	if len(q.RequiredTrust) > 0 {
		return append([]pages.Trust(nil), q.RequiredTrust...)
	}
	return append([]pages.Trust(nil), defaultSemanticTrust...)
}

func (q QueryIntent) effectiveScopeEpochs() []pages.ScopeEpoch {
	out := make([]pages.ScopeEpoch, 0, len(q.ScopeEpochs))
	seen := make(map[pages.ScopeEpoch]struct{}, len(q.ScopeEpochs))
	for _, epoch := range q.ScopeEpochs {
		if _, exists := seen[epoch]; exists {
			continue
		}
		seen[epoch] = struct{}{}
		out = append(out, epoch)
	}
	return out
}

func (q QueryIntent) searchText() string {
	parts := []string{q.UserText, q.LastFailure, q.ActiveGoal, q.NextAction}
	var b strings.Builder
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p)
	}
	return b.String()
}
