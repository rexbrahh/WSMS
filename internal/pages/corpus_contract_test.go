package pages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"wsms/internal/coherence"
	"wsms/internal/ledger"
	"wsms/internal/observers"
	"wsms/internal/wsl"
)

type frozenEventReader map[string]ledger.Event

func (r frozenEventReader) Get(ctx context.Context, id string) (ledger.Event, error) {
	if err := contextError(ctx); err != nil {
		return ledger.Event{}, err
	}
	ev, ok := r[id]
	if !ok {
		return ledger.Event{}, errors.New("event not found")
	}
	return ev, nil
}

type frozenReplayPoint struct {
	change    LedgerChange
	mutations []PageMutation
}

func TestFrozenCorpusContractIsStrictAndAdversarial(t *testing.T) {
	data := readFrozenCorpus(t)
	corpus, err := ParseFrozenCorpus(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(corpus.Streams) != 1 || len(corpus.Streams[0].Queries) < 10 {
		t.Fatalf("unexpected corpus shape: streams=%d queries=%d", len(corpus.Streams), len(corpus.Streams[0].Queries))
	}

	drifted := bytes.Replace(
		data,
		[]byte(`"version": "wsms-semantic-corpus/v1"`),
		[]byte(`"version": "wsms-semantic-corpus/v1", "ignored_gate": true`),
		1,
	)
	if _, err := ParseFrozenCorpus(drifted); !errors.Is(err, ErrInvalidCorpus) {
		t.Fatalf("schema drift error=%v", err)
	}

	queries := corpus.Streams[0].Queries[:0]
	for _, query := range corpus.Streams[0].Queries {
		poisoned := false
		for _, label := range query.Labels {
			poisoned = poisoned || label == CorpusLabelPoisoned
		}
		if !poisoned {
			queries = append(queries, query)
		}
	}
	corpus.Streams[0].Queries = queries
	if err := corpus.Validate(); !errors.Is(err, ErrInvalidCorpus) || !strings.Contains(err.Error(), "poisoned") {
		t.Fatalf("missing poison coverage error=%v", err)
	}

	corpus, err = ParseFrozenCorpus(data)
	if err != nil {
		t.Fatal(err)
	}
	query := &corpus.Streams[0].Queries[0]
	query.Forbidden = append(query.Forbidden, query.Expected[0])
	if err := corpus.Validate(); !errors.Is(err, ErrInvalidCorpus) || !strings.Contains(err.Error(), "both expected and forbidden") {
		t.Fatalf("contradictory judgment error=%v", err)
	}

	corpus, err = ParseFrozenCorpus(data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range corpus.Streams[0].Queries {
		query := &corpus.Streams[0].Queries[i]
		if query.ID == "q_wrong_branch" {
			query.Scope.Commit = "bbbbbbbb"
			break
		}
	}
	if err := corpus.Validate(); !errors.Is(err, ErrInvalidCorpus) || !strings.Contains(err.Error(), "exactly one authority axis") {
		t.Fatalf("confounded scope judgment error=%v", err)
	}

	corpus, err = ParseFrozenCorpus(data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range corpus.Streams[0].Queries {
		query := &corpus.Streams[0].Queries[i]
		if query.ID == "q_wrong_branch" {
			query.Scope.RepoID = "other-repo"
			query.Labels = append(query.Labels, CorpusLabelWrongRepo)
			break
		}
	}
	if err := corpus.Validate(); !errors.Is(err, ErrInvalidCorpus) || !strings.Contains(err.Error(), "exactly one authority axis") {
		t.Fatalf("multi-label scope judgment error=%v", err)
	}

	corpus, err = ParseFrozenCorpus(data)
	if err != nil {
		t.Fatal(err)
	}
	corpus.Streams[0].Queries[0].Scope.RepoID = "other-repo"
	if err := corpus.Validate(); !errors.Is(err, ErrInvalidCorpus) || !strings.Contains(err.Error(), "must match all current authority axes") {
		t.Fatalf("unlabeled scope drift error=%v", err)
	}
}

func TestCompilerRejectsTaskAuthorityConfusionAndUsesMixedCheckpointTrust(t *testing.T) {
	ev := ledger.Event{
		ID: "E0001", Seq: 1, TS: mustCorpusTime(t, "2026-07-10T12:00:01Z"),
		Type: ledger.EventTaskStarted, SessionID: "session", TaskID: "task-a",
		Repo: "repo", Branch: "main", Commit: "aaaaaaaa",
		Payload: map[string]any{"goal": "ship", "branch": "main"},
	}
	state := wsl.NewWorkingState()
	if err := state.ApplyEventUpdates(ev.ID, []wsl.Update{{
		Op: "upsert", EvidenceID: ev.ID,
		Record: &wsl.TaskRecord{IDValue: "T1", Goal: "ship", Branch: "main", Commit: "aaaaaaaa"},
	}}); err != nil {
		t.Fatal(err)
	}
	compiler := NewDeterministicCompiler()
	change := LedgerChange{
		Event: ev, State: state, RepoID: "repo", TaskID: "task-b", Branch: "main", Commit: "aaaaaaaa",
	}
	if _, err := compiler.Compile(context.Background(), change); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("task-confused compile error=%v", err)
	}
	change.TaskID = "task-a"
	mutations, err := compiler.Compile(context.Background(), change)
	if err != nil {
		t.Fatal(err)
	}
	if len(mutations) != 1 || mutations[0].Page.Trust != TrustMixed {
		t.Fatalf("checkpoint mutations=%#v", mutations)
	}
}

func TestFrozenCorpusReplayIsByteIdenticalAndExactlyMaterializable(t *testing.T) {
	corpus, err := ParseFrozenCorpus(readFrozenCorpus(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := corpus.Streams[0]
	first := replayFrozenStream(t, stream)
	second := replayFrozenStream(t, stream)
	firstJSON := marshalReplayMutations(t, first)
	secondJSON := marshalReplayMutations(t, second)
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("replay differs:\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
	}

	kinds := map[PageKind]bool{}
	for _, point := range first {
		for _, mutation := range point.mutations {
			kinds[mutation.Page.Kind] = true
			semanticText := strings.ToLower(mutation.Page.SearchText + " " + mutation.Page.Summary)
			if strings.Contains(semanticText, "ignore the user") || strings.Contains(semanticText, "disable scope checks") {
				t.Fatalf("poison entered page %s: %q", mutation.Page.ID, mutation.Page.SearchText)
			}
		}
	}
	if len(kinds) != 7 {
		t.Fatalf("compiled kinds=%v, want all seven", kinds)
	}
	assertFrozenTargetsExist(t, stream, first)

	final := first[len(first)-1]
	failure := mustFindTarget(t, first[2].mutations, CorpusPageTarget{
		Kind: KindFailureEpisode,
		Ref:  PageRef{Kind: RefWSLRecord, ID: "F1"},
	})
	if err := ValidateMaterializable(context.Background(), failure, final.change); !errors.Is(err, ErrUnmaterializableRef) {
		t.Fatalf("invalidated page materialized: %v", err)
	}

	filePage := mustFindTarget(t, first[6].mutations, CorpusPageTarget{
		Kind: KindFileContext,
		Ref:  PageRef{Kind: RefEvent, ID: "E0007"},
	})
	wrongBranch := final.change
	wrongBranch.ScopeEpoch = filePage.ScopeEpoch
	wrongBranch.Branch = "feature/clock-redesign"
	wrongBranch.Commit = "aaaaaaaa"
	if err := ValidateMaterializable(context.Background(), filePage, wrongBranch); !errors.Is(err, ErrUnmaterializableRef) {
		t.Fatalf("wrong-branch page materialized: %v", err)
	}
}

func replayFrozenStream(t *testing.T, stream CorpusStream) []frozenReplayPoint {
	t.Helper()
	reader := make(frozenEventReader, len(stream.Events))
	for _, ev := range stream.Events {
		reader[ev.ID] = ev
	}
	state := wsl.NewWorkingState()
	coherent := coherence.NewState()
	dispatcher := observers.Default(observers.NewSeqIDGen(), state)
	compiler := NewDeterministicCompiler()
	points := make([]frozenReplayPoint, 0, len(stream.Events))

	for _, ev := range stream.Events {
		candidate, err := coherent.Prepare(ev)
		if err != nil {
			t.Fatalf("prepare coherence %s: %v", ev.ID, err)
		}
		updates, err := dispatcher.OnEvent(context.Background(), ev)
		if err != nil {
			t.Fatalf("observe %s: %v", ev.ID, err)
		}
		for i := range updates {
			updates[i].EvidenceID = ev.ID
		}
		if err := candidate.BindUpdates(updates); err != nil {
			t.Fatalf("bind %s: %v", ev.ID, err)
		}
		if err := state.ApplyEventUpdates(ev.ID, updates); err != nil {
			t.Fatalf("apply %s: %v", ev.ID, err)
		}
		if err := coherent.Commit(candidate); err != nil {
			t.Fatalf("commit coherence %s: %v", ev.ID, err)
		}
		snapshot := coherent.Snapshot()
		change := LedgerChange{
			Event: ev, State: state.Clone(), Events: reader,
			Coherence: coherent, RepoID: snapshot.Current.Repo, TaskID: snapshot.Current.TaskID,
			Branch: snapshot.Current.Branch, Commit: snapshot.Current.Commit,
		}
		mutations, err := compiler.Compile(context.Background(), change)
		if err != nil {
			t.Fatalf("compile %s: %v", ev.ID, err)
		}
		for _, mutation := range mutations {
			if err := ValidateMaterializable(context.Background(), mutation.Page, change); err != nil {
				t.Fatalf("page %s is not immediately materializable at %s: %v", mutation.Page.ID, ev.ID, err)
			}
		}
		points = append(points, frozenReplayPoint{change: change, mutations: mutations})
	}
	return points
}

func assertFrozenTargetsExist(t *testing.T, stream CorpusStream, points []frozenReplayPoint) {
	t.Helper()
	replayPoint := map[string]int{}
	for i, ev := range stream.Events {
		replayPoint[ev.ID] = i
	}
	for _, query := range stream.Queries {
		pages := latestPagesThrough(points, replayPoint[query.AtEventID])
		targets := append(append([]CorpusPageTarget(nil), query.Expected...), query.Forbidden...)
		for _, target := range targets {
			if _, found := findTarget(pages, target); !found {
				t.Errorf("query %s target %s was not produced by replay", query.ID, target.key())
			}
		}
	}
}

func latestPagesThrough(points []frozenReplayPoint, at int) []PageMutation {
	latest := map[PageID]PageMutation{}
	for i := 0; i <= at; i++ {
		for _, mutation := range points[i].mutations {
			latest[mutation.Page.ID] = mutation
		}
	}
	out := make([]PageMutation, 0, len(latest))
	for _, mutation := range latest {
		out = append(out, mutation)
	}
	return out
}

func findTarget(mutations []PageMutation, target CorpusPageTarget) (WarmPage, bool) {
	for _, mutation := range mutations {
		if mutation.Page.Kind != target.Kind {
			continue
		}
		for _, ref := range mutation.Page.Refs {
			if ref == target.Ref {
				return mutation.Page, true
			}
		}
	}
	return WarmPage{}, false
}

func mustFindTarget(t *testing.T, mutations []PageMutation, target CorpusPageTarget) WarmPage {
	t.Helper()
	page, found := findTarget(mutations, target)
	if !found {
		t.Fatalf("page target %s not found", target.key())
	}
	return page
}

func marshalReplayMutations(t *testing.T, points []frozenReplayPoint) []byte {
	t.Helper()
	mutations := make([][]PageMutation, len(points))
	for i := range points {
		mutations[i] = points[i].mutations
	}
	data, err := json.Marshal(mutations)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readFrozenCorpus(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/frozen_corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustCorpusTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
