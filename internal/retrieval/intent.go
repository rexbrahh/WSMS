package retrieval

import (
	"fmt"
	"strings"

	"wsms/internal/pages"
)

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
	Mode             Mode
	SessionID        string
	RepoID           string
	TaskID           string
	Branch           string
	Commit           string
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
	if q.SessionID == "" {
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
	if q.CandidateLimit < 0 || q.MaterializeLimit < 0 {
		return fmt.Errorf("limits must be non-negative")
	}
	if len(q.UserText) > 4*1024 {
		return fmt.Errorf("user text exceeds bound")
	}
	return nil
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
