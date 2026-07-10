package pages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"wsms/internal/ledger"
)

const (
	// CurrentCorpusVersion identifies the hand-labeled semantic replay contract.
	CurrentCorpusVersion = "wsms-semantic-corpus/v1"
	maxCorpusBytes       = 4 * 1024 * 1024
	maxCorpusStreams     = 64
	maxCorpusEvents      = 10_000
	maxCorpusQueries     = 10_000
)

var ErrInvalidCorpus = errors.New("invalid frozen semantic corpus")

// CorpusLabel names the failure mode or positive behavior a hand-labeled
// query exists to exercise.
type CorpusLabel string

const (
	CorpusLabelPositive         CorpusLabel = "positive"
	CorpusLabelWrongRepo        CorpusLabel = "wrong_repo"
	CorpusLabelWrongTask        CorpusLabel = "wrong_task"
	CorpusLabelWrongBranch      CorpusLabel = "wrong_branch"
	CorpusLabelWrongCommit      CorpusLabel = "wrong_commit"
	CorpusLabelTrustMismatch    CorpusLabel = "trust_mismatch"
	CorpusLabelInvalidated      CorpusLabel = "invalidated"
	CorpusLabelPoisoned         CorpusLabel = "poisoned"
	CorpusLabelTrueNoAnswer     CorpusLabel = "true_no_answer"
	CorpusLabelNegativeTransfer CorpusLabel = "negative_transfer"
)

// FrozenCorpus is replayable L4 input plus semantic relevance judgments. It
// contains no generated embeddings and therefore remains backend-neutral.
type FrozenCorpus struct {
	Version string         `json:"version"`
	Streams []CorpusStream `json:"streams"`
}

// CorpusStream is one append-ordered session-local ledger stream.
type CorpusStream struct {
	ID        string         `json:"id"`
	SessionID string         `json:"session_id"`
	Events    []ledger.Event `json:"events"`
	Queries   []CorpusQuery  `json:"queries"`
}

// CorpusQuery is one hand-labeled semantic fault at a precise replay point.
// Expected targets describe relevant pages; forbidden and negative-transfer
// targets make false admission observable even before a production retriever
// exists.
type CorpusQuery struct {
	ID               string             `json:"id"`
	AtEventID        string             `json:"at_event_id"`
	Text             string             `json:"text"`
	Scope            CorpusQueryScope   `json:"scope"`
	MustAbstain      bool               `json:"must_abstain"`
	Expected         []CorpusPageTarget `json:"expected,omitempty"`
	Forbidden        []CorpusPageTarget `json:"forbidden,omitempty"`
	NegativeTransfer []CorpusPageTarget `json:"negative_transfer,omitempty"`
	Labels           []CorpusLabel      `json:"labels"`
}

// CorpusQueryScope is the authoritative eligibility envelope for a query.
type CorpusQueryScope struct {
	RepoID    string   `json:"repo_id"`
	TaskID    string   `json:"task_id,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Commit    string   `json:"commit,omitempty"`
	PathHints []string `json:"path_hints,omitempty"`
	// RequiredTrust is a hard eligibility filter, never query text.
	RequiredTrust []Trust `json:"required_trust,omitempty"`
}

// CorpusPageTarget identifies a page by controlled kind plus an exact source
// ref. This remains stable when compiler implementation details add metadata.
type CorpusPageTarget struct {
	Kind PageKind `json:"kind"`
	Ref  PageRef  `json:"ref"`
}

// ParseFrozenCorpus strictly decodes and validates a frozen corpus. Unknown
// schema fields fail so an evaluator cannot silently ignore a mislabeled gate.
func ParseFrozenCorpus(data []byte) (FrozenCorpus, error) {
	if len(data) == 0 || len(data) > maxCorpusBytes {
		return FrozenCorpus{}, fmt.Errorf("%w: corpus size %d is outside 1..%d", ErrInvalidCorpus, len(data), maxCorpusBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var corpus FrozenCorpus
	if err := decoder.Decode(&corpus); err != nil {
		return FrozenCorpus{}, fmt.Errorf("%w: decode: %v", ErrInvalidCorpus, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return FrozenCorpus{}, fmt.Errorf("%w: trailing JSON value", ErrInvalidCorpus)
		}
		return FrozenCorpus{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidCorpus, err)
	}
	if err := corpus.Validate(); err != nil {
		return FrozenCorpus{}, err
	}
	return corpus, nil
}

// Validate checks replay order, exact targets, explicit abstention/negative
// labels, and coverage of every controlled page kind and adversarial gate.
func (c FrozenCorpus) Validate() error {
	if c.Version != CurrentCorpusVersion {
		return corpusError("version %q, want %q", c.Version, CurrentCorpusVersion)
	}
	if len(c.Streams) == 0 || len(c.Streams) > maxCorpusStreams {
		return corpusError("stream count %d is outside 1..%d", len(c.Streams), maxCorpusStreams)
	}
	streamIDs := map[string]bool{}
	kindCoverage := map[PageKind]bool{}
	labelCoverage := map[CorpusLabel]bool{}
	for i := range c.Streams {
		stream := c.Streams[i]
		if !logicalIDRE.MatchString(stream.ID) || !validToken(stream.SessionID, 256) {
			return corpusError("stream %d has malformed id or session", i)
		}
		if streamIDs[stream.ID] {
			return corpusError("duplicate stream id %s", stream.ID)
		}
		streamIDs[stream.ID] = true
		if len(stream.Events) == 0 || len(stream.Events) > maxCorpusEvents {
			return corpusError("stream %s event count %d is outside 1..%d", stream.ID, len(stream.Events), maxCorpusEvents)
		}
		eventSeq := make(map[string]int64, len(stream.Events))
		eventsByID := make(map[string]ledger.Event, len(stream.Events))
		for j := range stream.Events {
			ev := stream.Events[j]
			if !logicalIDRE.MatchString(ev.ID) || ev.SessionID != stream.SessionID || ev.Seq != int64(j+1) || ev.TS.IsZero() {
				return corpusError("stream %s event %d has noncanonical identity/order/session/time", stream.ID, j)
			}
			if _, duplicate := eventSeq[ev.ID]; duplicate {
				return corpusError("stream %s has duplicate event %s", stream.ID, ev.ID)
			}
			if err := ledger.ValidateEvent(ev); err != nil {
				return corpusError("stream %s event %s: %v", stream.ID, ev.ID, err)
			}
			eventSeq[ev.ID] = ev.Seq
			eventsByID[ev.ID] = ev
		}
		if len(stream.Queries) == 0 || len(stream.Queries) > maxCorpusQueries {
			return corpusError("stream %s query count %d is outside 1..%d", stream.ID, len(stream.Queries), maxCorpusQueries)
		}
		queryIDs := map[string]bool{}
		for j := range stream.Queries {
			query := stream.Queries[j]
			atSeq, found := eventSeq[query.AtEventID]
			if !logicalIDRE.MatchString(query.ID) || !found || queryIDs[query.ID] {
				return corpusError("stream %s query %d has malformed/duplicate id or unknown replay point", stream.ID, j)
			}
			queryIDs[query.ID] = true
			if strings.TrimSpace(query.Text) == "" || strings.TrimSpace(query.Text) != query.Text || len(query.Text) > MaxSearchBytes || hasUnsafeText(query.Text) {
				return corpusError("stream %s query %s has malformed text", stream.ID, query.ID)
			}
			if err := query.Scope.validate(); err != nil {
				return corpusError("stream %s query %s scope: %v", stream.ID, query.ID, err)
			}
			if query.MustAbstain && len(query.Expected) != 0 || !query.MustAbstain && len(query.Expected) == 0 {
				return corpusError("stream %s query %s must choose expected targets xor abstention", stream.ID, query.ID)
			}
			targetSets := map[string]map[string]bool{}
			for name, targets := range map[string][]CorpusPageTarget{
				"expected": query.Expected, "forbidden": query.Forbidden, "negative_transfer": query.NegativeTransfer,
			} {
				seen := map[string]bool{}
				for _, target := range targets {
					if err := target.validate(eventSeq, atSeq); err != nil {
						return corpusError("stream %s query %s %s: %v", stream.ID, query.ID, name, err)
					}
					key := target.key()
					if seen[key] {
						return corpusError("stream %s query %s duplicates %s target %s", stream.ID, query.ID, name, key)
					}
					seen[key] = true
					if name == "expected" {
						kindCoverage[target.Kind] = true
					}
				}
				targetSets[name] = seen
			}
			for key := range targetSets["expected"] {
				if targetSets["forbidden"][key] {
					return corpusError("stream %s query %s target %s is both expected and forbidden", stream.ID, query.ID, key)
				}
			}
			for _, target := range query.NegativeTransfer {
				if !containsTarget(query.Forbidden, target) {
					return corpusError("stream %s query %s negative-transfer target %s is not forbidden", stream.ID, query.ID, target.key())
				}
			}
			if len(query.Labels) == 0 {
				return corpusError("stream %s query %s has no labels", stream.ID, query.ID)
			}
			seenLabels := map[CorpusLabel]bool{}
			for _, label := range query.Labels {
				if !validCorpusLabel(label) || seenLabels[label] {
					return corpusError("stream %s query %s has invalid/duplicate label %q", stream.ID, query.ID, label)
				}
				seenLabels[label], labelCoverage[label] = true, true
			}
			if query.MustAbstain && seenLabels[CorpusLabelPositive] {
				return corpusError("stream %s query %s labels abstention as positive", stream.ID, query.ID)
			}
			if !query.MustAbstain && !seenLabels[CorpusLabelPositive] {
				return corpusError("stream %s query %s has expected targets without a positive label", stream.ID, query.ID)
			}
			if seenLabels[CorpusLabelPositive] && len(seenLabels) != 1 {
				return corpusError("stream %s query %s mixes positive and abstention labels", stream.ID, query.ID)
			}
			for _, label := range []CorpusLabel{
				CorpusLabelWrongRepo, CorpusLabelWrongTask, CorpusLabelWrongBranch, CorpusLabelWrongCommit,
				CorpusLabelTrustMismatch, CorpusLabelInvalidated, CorpusLabelPoisoned,
				CorpusLabelTrueNoAnswer, CorpusLabelNegativeTransfer,
			} {
				if seenLabels[label] && !query.MustAbstain {
					return corpusError("stream %s query %s label %s requires abstention", stream.ID, query.ID, label)
				}
			}
			if seenLabels[CorpusLabelNegativeTransfer] && len(query.NegativeTransfer) == 0 {
				return corpusError("stream %s query %s labels negative transfer without targets", stream.ID, query.ID)
			}
			if (seenLabels[CorpusLabelInvalidated] || seenLabels[CorpusLabelPoisoned]) && len(query.Forbidden) == 0 {
				return corpusError("stream %s query %s invalidated/poisoned label requires forbidden targets", stream.ID, query.ID)
			}
			if err := validateLabelMeaning(query, authorityAt(stream.Events, atSeq), eventsByID[query.AtEventID]); err != nil {
				return corpusError("stream %s query %s labels: %v", stream.ID, query.ID, err)
			}
		}
	}
	for _, kind := range []PageKind{KindFailureEpisode, KindDecision, KindConstraint, KindTaskCheckpoint, KindKnownGood, KindRepoFact, KindFileContext} {
		if !kindCoverage[kind] {
			return corpusError("page kind %s has no positive label", kind)
		}
	}
	for _, label := range []CorpusLabel{
		CorpusLabelPositive, CorpusLabelWrongRepo, CorpusLabelWrongTask, CorpusLabelWrongBranch,
		CorpusLabelWrongCommit, CorpusLabelTrustMismatch, CorpusLabelInvalidated,
		CorpusLabelPoisoned, CorpusLabelTrueNoAnswer, CorpusLabelNegativeTransfer,
	} {
		if !labelCoverage[label] {
			return corpusError("required label %s is absent", label)
		}
	}
	return nil
}

func (s CorpusQueryScope) validate() error {
	if !validToken(s.RepoID, 256) {
		return errors.New("repo id is required and must be canonical")
	}
	for name, value := range map[string]string{"task": s.TaskID, "branch": s.Branch, "commit": s.Commit} {
		if value != "" && !validToken(value, 256) {
			return fmt.Errorf("%s is malformed", name)
		}
	}
	if s.Commit != "" && s.Branch == "" {
		return errors.New("commit requires branch")
	}
	if len(s.PathHints) > MaxPageRefs {
		return fmt.Errorf("too many path hints")
	}
	seen := map[string]bool{}
	for _, candidate := range s.PathHints {
		cleaned, ok := normalizeRepoPath(candidate)
		if !ok || cleaned != candidate || seen[candidate] {
			return fmt.Errorf("path hint %q is noncanonical or duplicated", candidate)
		}
		seen[candidate] = true
	}
	if len(s.RequiredTrust) > 6 {
		return fmt.Errorf("too many required trust values")
	}
	seenTrust := map[Trust]bool{}
	for _, trust := range s.RequiredTrust {
		if !validTrust(trust) || seenTrust[trust] {
			return fmt.Errorf("required trust %q is invalid or duplicated", trust)
		}
		seenTrust[trust] = true
	}
	return nil
}

func (t CorpusPageTarget) validate(eventSeq map[string]int64, atSeq int64) error {
	if !validKind(t.Kind) {
		return fmt.Errorf("unknown page kind %q", t.Kind)
	}
	if err := t.Ref.validate(); err != nil {
		return err
	}
	if t.Ref.Kind == RefEvent {
		seq, found := eventSeq[t.Ref.ID]
		if !found || seq > atSeq {
			return fmt.Errorf("event ref %s does not exist at replay point", t.Ref.ID)
		}
	}
	return nil
}

func (t CorpusPageTarget) key() string { return string(t.Kind) + "\x00" + t.Ref.Address() }

func containsTarget(targets []CorpusPageTarget, want CorpusPageTarget) bool {
	for _, target := range targets {
		if target.key() == want.key() {
			return true
		}
	}
	return false
}

func validCorpusLabel(label CorpusLabel) bool {
	switch label {
	case CorpusLabelPositive, CorpusLabelWrongRepo, CorpusLabelWrongTask, CorpusLabelWrongBranch,
		CorpusLabelWrongCommit, CorpusLabelTrustMismatch, CorpusLabelInvalidated,
		CorpusLabelPoisoned, CorpusLabelTrueNoAnswer, CorpusLabelNegativeTransfer:
		return true
	default:
		return false
	}
}

type corpusAuthority struct {
	repo   string
	task   string
	branch string
	commit string
}

func authorityAt(events []ledger.Event, atSeq int64) corpusAuthority {
	var authority corpusAuthority
	for _, ev := range events {
		if ev.Seq > atSeq {
			break
		}
		switch ev.Type {
		case ledger.EventTaskStarted:
			authority = corpusAuthority{repo: ev.Repo, task: ev.TaskID, branch: ev.Branch, commit: ev.Commit}
		case ledger.EventBranchChange:
			authority.repo, authority.branch, authority.commit = ev.Repo, ev.Branch, ev.Commit
		case ledger.EventCommitChange:
			authority.repo, authority.branch, authority.commit = ev.Repo, ev.Branch, ev.Commit
		}
	}
	return authority
}

func validateLabelMeaning(query CorpusQuery, current corpusAuthority, atEvent ledger.Event) error {
	labels := map[CorpusLabel]bool{}
	for _, label := range query.Labels {
		labels[label] = true
	}
	if labels[CorpusLabelWrongRepo] && (current.repo == "" || query.Scope.RepoID == current.repo) {
		return errors.New("wrong_repo must name a different current repo")
	}
	if labels[CorpusLabelWrongTask] && (current.task == "" || query.Scope.TaskID == "" || query.Scope.TaskID == current.task) {
		return errors.New("wrong_task must name a different current task")
	}
	if labels[CorpusLabelWrongBranch] && (current.branch == "" || query.Scope.Branch == "" || query.Scope.Branch == current.branch) {
		return errors.New("wrong_branch must name a different current branch")
	}
	if labels[CorpusLabelWrongCommit] && (current.commit == "" || query.Scope.Commit == "" || query.Scope.Commit == current.commit) {
		return errors.New("wrong_commit must name a different current commit")
	}
	for _, label := range []CorpusLabel{CorpusLabelWrongRepo, CorpusLabelWrongTask, CorpusLabelWrongBranch, CorpusLabelWrongCommit} {
		if labels[label] && len(query.NegativeTransfer) == 0 {
			return fmt.Errorf("%s requires negative-transfer targets", label)
		}
	}
	wrongAxes := []struct {
		label   CorpusLabel
		differs bool
	}{
		{CorpusLabelWrongRepo, query.Scope.RepoID != current.repo},
		{CorpusLabelWrongTask, query.Scope.TaskID != current.task},
		{CorpusLabelWrongBranch, query.Scope.Branch != current.branch},
		{CorpusLabelWrongCommit, query.Scope.Commit != current.commit},
	}
	labelCount, differenceCount := 0, 0
	for _, axis := range wrongAxes {
		if labels[axis.label] {
			labelCount++
		}
		if axis.differs {
			differenceCount++
		}
	}
	if labelCount > 0 && (labelCount != 1 || differenceCount != 1) {
		return fmt.Errorf("wrong-scope judgment must differ on exactly one authority axis and carry exactly one matching label; labels=%d differences=%d", labelCount, differenceCount)
	}
	if labelCount == 0 && differenceCount != 0 {
		return fmt.Errorf("judgment without a wrong-scope label must match all current authority axes; differences=%d", differenceCount)
	}
	for _, axis := range wrongAxes {
		if labels[axis.label] != axis.differs && labelCount > 0 {
			return fmt.Errorf("wrong-scope judgment label %s does not match its sole differing authority axis", axis.label)
		}
	}
	if labels[CorpusLabelTrustMismatch] && len(query.Scope.RequiredTrust) == 0 {
		return errors.New("trust_mismatch requires a trust filter")
	}
	if labels[CorpusLabelInvalidated] && atEvent.Type != ledger.EventMemoryInvalidated {
		return errors.New("invalidated must be judged at an invalidation event")
	}
	if labels[CorpusLabelTrueNoAnswer] && (len(query.Forbidden) != 0 || len(query.NegativeTransfer) != 0) {
		return errors.New("true_no_answer must not disguise a forbidden candidate")
	}
	return nil
}

func corpusError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCorpus, fmt.Sprintf(format, args...))
}
