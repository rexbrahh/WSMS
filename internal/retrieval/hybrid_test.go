package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"wsms/internal/indexer"
	"wsms/internal/pages"
	"wsms/internal/types"
)

func TestHybridFusionUsesVersionedRRFAndStablePageIDTie(t *testing.T) {
	a := hybridPage("1", pages.KindConstraint, "alpha unique", "E1")
	b := hybridPage("2", pages.KindFailureEpisode, "beta unique", "E2")
	idx := &fakeHybridIndex{denseEnabled: true}
	idx.lexical = []indexer.Candidate{
		hybridCandidate(a, indexer.ScoreKindFTS5BM25, 1, -9),
		hybridCandidate(b, indexer.ScoreKindFTS5BM25, 2, -0.01),
	}
	idx.dense = []indexer.Candidate{
		hybridCandidate(b, indexer.ScoreKindCosineDistance, 1, 0.01),
		hybridCandidate(a, indexer.ScoreKindCosineDistance, 2, 0.30),
	}
	policy := zeroHybridPolicy()
	ret := hybridRetriever(idx, policy)
	result, err := ret.ResolveSemantic(context.Background(), hybridIntent(2))
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(result); !reflect.DeepEqual(got, []pages.PageID{a.ID, b.ID}) {
		t.Fatalf("stable RRF tie order = %v, want [%s %s]", got, a.ID, b.ID)
	}
	if result.Trace.FusionVersion != RRFFusionVersion || result.Trace.RRFK != 60 {
		t.Fatalf("fusion trace = %#v", result.Trace)
	}
	traceA := traceForPage(t, result.Trace, a.ID)
	if traceA.LexicalPosition != 1 || traceA.DensePosition != 2 || traceA.LexicalRank != -9 || traceA.DenseDistance != 0.30 {
		t.Fatalf("page A trace = %#v", traceA)
	}
	if math.Abs(traceA.RRFScore-(1.0/61.0+1.0/62.0)) > 1e-15 {
		t.Fatalf("page A RRF = %.12f", traceA.RRFScore)
	}
}

func TestHybridDenseOnlyConceptualHitAndFTSOnlyFallback(t *testing.T) {
	t.Run("dense_only", func(t *testing.T) {
		page := hybridPage("3", pages.KindRepoFact, "conceptual allocator locality", "E3")
		idx := &fakeHybridIndex{denseEnabled: true, dense: []indexer.Candidate{
			hybridCandidate(page, indexer.ScoreKindCosineDistance, 1, 0.08),
		}}
		rechecked := 0
		ret := hybridRetriever(idx, zeroHybridPolicy())
		ret.DetailedRecheck = func(context.Context, pages.WarmPage, int) (RecheckResult, error) {
			rechecked++
			return RecheckResult{Eligible: true}, nil
		}
		result, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
		if err != nil {
			t.Fatal(err)
		}
		if got := candidateIDs(result); !reflect.DeepEqual(got, []pages.PageID{page.ID}) {
			t.Fatalf("dense-only ids=%v", got)
		}
		if result.Trace.DenseCandidates != 1 || result.Trace.LexicalCandidates != 0 {
			t.Fatalf("trace=%#v", result.Trace)
		}
		if rechecked != 1 {
			t.Fatalf("dense-only candidate crossed the empty lexical snapshot boundary without exact recheck: calls=%d", rechecked)
		}
	})

	t.Run("fts_only_disabled_dense", func(t *testing.T) {
		page := hybridPage("4", pages.KindKnownGood, "exact TestCancelStream", "E4")
		idx := &fakeHybridIndex{denseEnabled: false, lexical: []indexer.Candidate{
			hybridCandidate(page, indexer.ScoreKindFTS5BM25, 1, -4),
		}}
		ret := hybridRetriever(idx, zeroHybridPolicy())
		result, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
		if err != nil {
			t.Fatal(err)
		}
		if !containsString(result.Degraded, "dense=unavailable") {
			t.Fatalf("degraded=%v", result.Degraded)
		}
		if idx.denseCalls() != 0 {
			t.Fatalf("disabled dense calls=%d", idx.denseCalls())
		}
	})
}

func TestHybridRequiresAuthoritativeEligibilityBeforeChannelCalls(t *testing.T) {
	for _, mode := range []Mode{"", ModeSemanticFault, ModePrefetch} {
		name := string(mode)
		if name == "" {
			name = "default_semantic_fault"
		}
		t.Run(name, func(t *testing.T) {
			idx := &fakeHybridIndex{denseEnabled: true}
			ret := hybridRetriever(idx, zeroHybridPolicy())
			intent := hybridIntent(1)
			intent.Mode = mode
			intent.EligibilityComplete = false
			intent.EligiblePageTuples = nil
			_, err := ret.ResolveSemantic(context.Background(), intent)
			if !errors.Is(err, ErrAuthorityUnavailable) || !errors.Is(err, ErrIndexUnavailable) || errors.Is(err, ErrSemanticPageMiss) {
				t.Fatalf("err=%v want safe operational authority failure", err)
			}
			if idx.lexicalCalls() != 0 || idx.denseCalls() != 0 {
				t.Fatalf("hybrid request without authority reached channels: lexical=%d dense=%d", idx.lexicalCalls(), idx.denseCalls())
			}
		})
	}

	t.Run("inspection_may_examine_historical_epochs", func(t *testing.T) {
		page := hybridPage("4", pages.KindConstraint, "inspection history", "EI")
		idx := &fakeHybridIndex{lexical: []indexer.Candidate{hybridCandidate(page, indexer.ScoreKindFTS5BM25, 1, -1)}}
		ret := hybridRetriever(idx, zeroHybridPolicy())
		ret.Dense = nil
		intent := hybridIntent(1)
		intent.Mode = ModeInspection
		intent.ScopeEpochs = nil
		intent.EligibilityComplete = false
		intent.EligiblePageTuples = nil
		if _, err := ret.ResolveSemantic(context.Background(), intent); err != nil {
			t.Fatal(err)
		}
		if idx.lexicalCalls() != 1 {
			t.Fatalf("inspection lexical calls=%d want 1", idx.lexicalCalls())
		}
	})
}

func TestHybridCompleteEmptyEligibilityIsHealthyMiss(t *testing.T) {
	idx := &fakeHybridIndex{denseEnabled: true}
	ret := hybridRetriever(idx, zeroHybridPolicy())
	intent := hybridIntent(1)
	intent.EligiblePageTuples = nil
	_, err := ret.ResolveSemantic(context.Background(), intent)
	if !errors.Is(err, ErrSemanticPageMiss) || errors.Is(err, ErrIndexUnavailable) {
		t.Fatalf("complete empty authority err=%v, want semantic miss", err)
	}
	if idx.lexicalCalls() != 1 || idx.denseCalls() != 1 {
		t.Fatalf("complete empty authority did not run healthy channels: lexical=%d dense=%d", idx.lexicalCalls(), idx.denseCalls())
	}
	lexical, dense := idx.queries()
	if !lexical.EligibilityComplete || !dense.EligibilityComplete || len(lexical.EligiblePageTuples) != 0 || len(dense.EligiblePageTuples) != 0 {
		t.Fatalf("complete empty authority was not preserved: lexical=%#v dense=%#v", lexical, dense)
	}
}

func TestHybridRealIndexDenseHitUsesPreLimitHardFiltersAndExactRecheck(t *testing.T) {
	ctx := context.Background()
	idx, err := indexer.Open(filepath.Join(t.TempDir(), "index"), indexer.Options{DenseDimensions: 2, EmbeddingNamespace: "embed/test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	eligible := hybridPage("a", pages.KindRepoFact, "unix resident cache mechanics", "IR1")
	eligible.Trust = pages.TrustRepo
	eligible.PathScope = []string{"src/runtime"}
	eligible.Scope = types.ScopeFile
	wrongRepo := hybridPage("b", pages.KindRepoFact, "wrong repository vector", "IR2")
	wrongRepo.Trust = pages.TrustRepo
	wrongRepo.RepoID = "other"
	wrongRepo.Scope = types.ScopeBranch
	excluded := hybridPage("c", pages.KindRepoFact, "excluded closer vector", "IR3")
	excluded.Trust = pages.TrustRepo
	excluded.Scope = types.ScopeBranch
	mutations := []pages.PageMutation{
		{Op: pages.MutationUpsert, Page: eligible},
		{Op: pages.MutationUpsert, Page: wrongRepo},
		{Op: pages.MutationUpsert, Page: excluded},
	}
	if err := idx.ApplyWithWatermark(ctx, mutations, "s", 1, 1); err != nil {
		t.Fatal(err)
	}
	records := make([]indexer.VectorRecord, 0, len(mutations))
	for _, item := range []struct {
		page   pages.WarmPage
		vector []float64
	}{{eligible, []float64{0.99, 0.14}}, {wrongRepo, []float64{1, 0}}, {excluded, []float64{1, 0}}} {
		records = append(records, indexer.VectorRecord{
			PageID: item.page.ID, SessionID: item.page.SessionID, PageVersion: item.page.Version,
			SourceDigest: item.page.SourceDigest, CompilerVersion: item.page.CompilerVersion,
			EmbeddingNamespace: "embed/test", Vector: item.vector,
		})
	}
	if err := idx.UpsertVectors(ctx, records); err != nil {
		t.Fatal(err)
	}

	rechecked := 0
	ret := &HybridRetriever{
		Index: idx, Dense: &DenseQuery{Namespace: "embed/test", Vector: []float64{1, 0}}, Policy: zeroHybridPolicy(),
		DetailedRecheck: func(_ context.Context, page pages.WarmPage, _ int) (RecheckResult, error) {
			rechecked++
			if page.ID != eligible.ID {
				t.Fatalf("hard-filtered page reached authority recheck: %s", page.ID)
			}
			return RecheckResult{Eligible: true, TokensUsed: 2}, nil
		},
	}
	intent := hybridIntent(1)
	intent.UserText = "conceptual allocator proximity"
	intent.RepoID = "repo"
	intent.PathHints = []string{"src/runtime/worker.go"}
	intent.Exclusions = []string{string(excluded.ID)}
	result, err := ret.ResolveSemantic(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidateIDs(result), []pages.PageID{eligible.ID}) || result.Trace.LexicalCandidates != 0 || result.Trace.DenseCandidates != 1 || rechecked != 1 {
		t.Fatalf("real-index result=%#v rechecked=%d", result, rechecked)
	}
	if result.Candidates[0].Page.SearchText != "" || len(result.Candidates[0].Page.Refs) != 0 {
		t.Fatalf("real-index candidate leaked indexed content: %#v", result.Candidates[0])
	}
}

func TestHybridChannelFailuresAreCategoricalAndNotMisses(t *testing.T) {
	page := hybridPage("5", pages.KindConstraint, "safe survivor", "E5")
	tests := []struct {
		name        string
		lexical     []indexer.Candidate
		dense       []indexer.Candidate
		lexicalErr  error
		denseErr    error
		wantMarker  string
		wantSuccess bool
	}{
		{name: "dense_failure_lexical_survives", lexical: []indexer.Candidate{hybridCandidate(page, indexer.ScoreKindFTS5BM25, 1, -1)}, denseErr: errors.New("TOP_SECRET dense failure"), wantMarker: "dense=search", wantSuccess: true},
		{name: "lexical_failure_dense_survives", dense: []indexer.Candidate{hybridCandidate(page, indexer.ScoreKindCosineDistance, 1, 0.1)}, lexicalErr: errors.New("TOP_SECRET fts failure"), wantMarker: "fts=search", wantSuccess: true},
		{name: "both_fail", lexicalErr: errors.New("TOP_SECRET fts failure"), denseErr: errors.New("TOP_SECRET dense failure")},
		{name: "failed_plus_empty", lexicalErr: errors.New("TOP_SECRET fts failure")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := &fakeHybridIndex{denseEnabled: true, lexical: tt.lexical, dense: tt.dense, lexicalErr: tt.lexicalErr, denseErr: tt.denseErr}
			ret := hybridRetriever(idx, zeroHybridPolicy())
			result, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
			if tt.wantSuccess {
				if err != nil {
					t.Fatal(err)
				}
				if !containsString(result.Degraded, tt.wantMarker) {
					t.Fatalf("degraded=%v want %q", result.Degraded, tt.wantMarker)
				}
				if strings.Contains(result.Explanation, "TOP_SECRET") {
					t.Fatalf("raw backend text in explanation: %s", result.Explanation)
				}
				return
			}
			if !errors.Is(err, ErrIndexUnavailable) || errors.Is(err, ErrSemanticPageMiss) {
				t.Fatalf("err=%v want operational failure", err)
			}
			if len(result.Degraded) == 0 || result.Trace.FusionVersion != RRFFusionVersion {
				t.Fatalf("operational result lost categorical trace: %#v", result)
			}
			if strings.Contains(err.Error(), "TOP_SECRET") {
				t.Fatalf("raw backend text in error: %v", err)
			}
		})
	}
}

func TestHybridExplicitDenseUnavailabilityIsOperationalWhenFTSEmpty(t *testing.T) {
	idx := &fakeHybridIndex{denseEnabled: true}
	ret := hybridRetriever(idx, zeroHybridPolicy())
	ret.Dense = &DenseQuery{UnavailableReason: DenseUnavailableEmbedder}
	result, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
	if !errors.Is(err, ErrIndexUnavailable) || errors.Is(err, ErrSemanticPageMiss) {
		t.Fatalf("err=%v want operational failure", err)
	}
	if !containsString(result.Degraded, "dense=embedder") || result.Trace.Abstention != "no_candidates" {
		t.Fatalf("operational result=%#v", result)
	}
	if idx.denseCalls() != 0 {
		t.Fatalf("explicitly unavailable dense channel was called %d times", idx.denseCalls())
	}
}

func TestHybridUsesIdenticalBoundedHardFilterUniverse(t *testing.T) {
	page := hybridPage("6", pages.KindFailureEpisode, "stream blocked", "E6")
	page.PathScope = []string{"src/runtime"}
	page.ScopeEpoch = 7
	idx := &fakeHybridIndex{denseEnabled: true}
	idx.lexicalFn = func(_ context.Context, query indexer.SearchQuery) ([]indexer.Candidate, error) {
		return []indexer.Candidate{hybridCandidate(page, indexer.ScoreKindFTS5BM25, 1, -1)}, nil
	}
	idx.denseFn = func(_ context.Context, query indexer.SearchQuery, _ []float64) ([]indexer.Candidate, error) {
		return []indexer.Candidate{hybridCandidate(page, indexer.ScoreKindCosineDistance, 1, 0.1)}, nil
	}
	intent := hybridIntent(1)
	intent.RepoID, intent.TaskID, intent.Branch, intent.Commit = "repo", "T1", "main", "abc"
	intent.PathHints = []string{"src/runtime/stream.go"}
	intent.ScopeEpochs = []pages.ScopeEpoch{7, 7}
	intent.EligiblePageTuples = []indexer.PageTuple{pageTuple(page), pageTuple(page)}
	intent.AllowedKinds = []pages.PageKind{pages.KindFailureEpisode}
	intent.RequiredTrust = nil
	intent.Exclusions = []string{"wp_" + strings.Repeat("f", 32), "F9"}
	ret := hybridRetriever(idx, zeroHybridPolicy())
	if _, err := ret.ResolveSemantic(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	lexicalQuery, denseQuery := idx.queries()
	if !reflect.DeepEqual(lexicalQuery, denseQuery) {
		t.Fatalf("channel queries differ:\nlex=%#v\ndense=%#v", lexicalQuery, denseQuery)
	}
	if len(lexicalQuery.Trust) != len(defaultSemanticTrust) || len(lexicalQuery.Statuses) != 1 || lexicalQuery.Statuses[0] != pages.StatusActive {
		t.Fatalf("explicit trust/status policy missing: %#v", lexicalQuery)
	}
	if !reflect.DeepEqual(lexicalQuery.ScopeEpochs, []pages.ScopeEpoch{7}) {
		t.Fatalf("scope epochs were not bounded/deduped: %#v", lexicalQuery.ScopeEpochs)
	}
	if !lexicalQuery.EligibilityComplete || !reflect.DeepEqual(lexicalQuery.EligiblePageTuples, []indexer.PageTuple{pageTuple(page)}) {
		t.Fatalf("exact eligibility was not bounded/deduped: %#v", lexicalQuery.EligiblePageTuples)
	}
	if !reflect.DeepEqual(lexicalQuery.PathHints, []string{"src/runtime/stream.go"}) || len(lexicalQuery.ExcludedPageIDs) != 1 || !reflect.DeepEqual(lexicalQuery.ExcludedRefIDs, []string{"F9"}) {
		t.Fatalf("path/exclusion filters=%#v", lexicalQuery)
	}
}

func TestHybridExactEligibilityPrecedesLimitWhenScopeEpochAliasesAcrossPaths(t *testing.T) {
	const epoch pages.ScopeEpoch = 42
	const candidateLimit = 3
	current := epochHybridPage(1000, epoch)
	current.PathScope = []string{"path/b/current.go"}
	universe := make([]pages.WarmPage, 0, candidateLimit+7)
	for i := 1; i <= candidateLimit+6; i++ {
		stale := epochHybridPage(i, epoch)
		stale.PathScope = []string{"path/a/stale.go"}
		universe = append(universe, stale)
	}
	universe = append(universe, current)
	search := func(kind indexer.ScoreKind, query indexer.SearchQuery) []indexer.Candidate {
		out := make([]indexer.Candidate, 0, query.Limit)
		for _, page := range universe {
			tuple := pageTuple(page)
			if !matchesHardUniverse(page, tuple, query) {
				continue
			}
			out = append(out, hybridCandidate(page, kind, len(out)+1, float64(len(out))))
			if len(out) == query.Limit {
				break
			}
		}
		return out
	}
	idx := &fakeHybridIndex{denseEnabled: true}
	idx.lexicalFn = func(_ context.Context, query indexer.SearchQuery) ([]indexer.Candidate, error) {
		return search(indexer.ScoreKindFTS5BM25, query), nil
	}
	idx.denseFn = func(_ context.Context, query indexer.SearchQuery, _ []float64) ([]indexer.Candidate, error) {
		return search(indexer.ScoreKindCosineDistance, query), nil
	}
	intent := hybridIntent(1)
	intent.CandidateLimit = candidateLimit
	intent.ScopeEpochs = []pages.ScopeEpoch{epoch}
	intent.EligiblePageTuples = []indexer.PageTuple{pageTuple(current)}
	result, err := hybridRetriever(idx, zeroHybridPolicy()).ResolveSemantic(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(result); !reflect.DeepEqual(got, []pages.PageID{current.ID}) {
		t.Fatalf("same numeric epoch from path A admitted outside exact path-B tuple: %v", got)
	}
	if result.Trace.LexicalCandidates != 1 || result.Trace.DenseCandidates != 1 || !containsString(result.Trace.Filters, "eligibility") {
		t.Fatalf("exact pre-limit filter trace=%#v", result.Trace)
	}
}

func TestHybridScopeEpochFilterPrecedesFakeChannelLimit(t *testing.T) {
	const currentEpoch pages.ScopeEpoch = 9
	const candidateLimit = 3
	current := epochHybridPage(1000, currentEpoch)
	search := func(scoreKind indexer.ScoreKind) func(indexer.SearchQuery) []indexer.Candidate {
		return func(query indexer.SearchQuery) []indexer.Candidate {
			universe := make([]pages.WarmPage, 0, candidateLimit+7)
			for i := 0; i < candidateLimit+6; i++ {
				universe = append(universe, epochHybridPage(i+1, 1))
			}
			universe = append(universe, current)
			out := make([]indexer.Candidate, 0, query.Limit)
			for _, page := range universe {
				if len(query.ScopeEpochs) > 0 && !containsScopeEpoch(query.ScopeEpochs, page.ScopeEpoch) {
					continue
				}
				out = append(out, hybridCandidate(page, scoreKind, len(out)+1, float64(len(out))))
				if len(out) == query.Limit {
					break
				}
			}
			return out
		}
	}
	lexicalSearch := search(indexer.ScoreKindFTS5BM25)
	denseSearch := search(indexer.ScoreKindCosineDistance)
	idx := &fakeHybridIndex{denseEnabled: true}
	idx.lexicalFn = func(_ context.Context, query indexer.SearchQuery) ([]indexer.Candidate, error) {
		return lexicalSearch(query), nil
	}
	idx.denseFn = func(_ context.Context, query indexer.SearchQuery, _ []float64) ([]indexer.Candidate, error) {
		return denseSearch(query), nil
	}
	ret := hybridRetriever(idx, zeroHybridPolicy())
	intent := hybridIntent(1)
	intent.CandidateLimit = candidateLimit
	intent.ScopeEpochs = []pages.ScopeEpoch{currentEpoch, currentEpoch}
	intent.EligiblePageTuples = []indexer.PageTuple{pageTuple(current), pageTuple(current)}
	result, err := ret.ResolveSemantic(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidateIDs(result), []pages.PageID{current.ID}) || !containsString(result.Trace.Filters, "scope_epoch") {
		t.Fatalf("stale epochs starved current fake candidate: %#v", result)
	}
}

func TestHybridScopeEpochFilterPrecedesRealIndexChannelLimits(t *testing.T) {
	ctx := context.Background()
	idx, err := indexer.Open(filepath.Join(t.TempDir(), "index"), indexer.Options{DenseDimensions: 2, EmbeddingNamespace: "embed/test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	const currentEpoch pages.ScopeEpoch = 11
	const candidateLimit = 3
	current := epochHybridPage(1000, currentEpoch)
	all := make([]pages.WarmPage, 0, candidateLimit+7)
	for i := 0; i < candidateLimit+6; i++ {
		all = append(all, epochHybridPage(i+1, 1))
	}
	all = append(all, current)
	mutations := make([]pages.PageMutation, 0, len(all))
	records := make([]indexer.VectorRecord, 0, len(all))
	for _, page := range all {
		mutations = append(mutations, pages.PageMutation{Op: pages.MutationUpsert, Page: page})
		records = append(records, indexer.VectorRecord{
			PageID: page.ID, SessionID: page.SessionID, PageVersion: page.Version,
			SourceDigest: page.SourceDigest, CompilerVersion: page.CompilerVersion,
			EmbeddingNamespace: "embed/test", Vector: []float64{1, 0},
		})
	}
	if err := idx.ApplyWithWatermark(ctx, mutations, "s", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertVectors(ctx, records); err != nil {
		t.Fatal(err)
	}
	ret := hybridRetriever(idx, zeroHybridPolicy())
	intent := hybridIntent(1)
	intent.UserText = "scope epoch starvation needle"
	intent.CandidateLimit = candidateLimit
	intent.ScopeEpochs = []pages.ScopeEpoch{currentEpoch, currentEpoch}
	intent.EligiblePageTuples = []indexer.PageTuple{pageTuple(current), pageTuple(current)}
	result, err := ret.ResolveSemantic(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidateIDs(result), []pages.PageID{current.ID}) || result.Trace.LexicalCandidates != 1 || result.Trace.DenseCandidates != 1 {
		t.Fatalf("stale epochs starved current real-index candidate: %#v", result)
	}
}

func TestHybridFailsClosedOnHardFilterContractViolation(t *testing.T) {
	page := hybridPage("7", pages.KindConstraint, "wrong session", "E7")
	page.SessionID = "other"
	page = withDigest(page, "7")
	idx := &fakeHybridIndex{lexical: []indexer.Candidate{hybridCandidate(page, indexer.ScoreKindFTS5BM25, 1, -1)}}
	ret := hybridRetriever(idx, zeroHybridPolicy())
	ret.Dense = nil
	_, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
	if !errors.Is(err, ErrIndexUnavailable) || errors.Is(err, ErrSemanticPageMiss) {
		t.Fatalf("err=%v", err)
	}
}

func TestHybridSuppresssTupleMismatchAndSnapshotMismatch(t *testing.T) {
	t.Run("tuple_mismatch", func(t *testing.T) {
		v1 := hybridPage("8", pages.KindRepoFact, "tuple v1", "E8")
		v2 := v1
		v2.Version = 2
		v2.SourceDigest = pages.SourceDigest(strings.Repeat("9", 64))
		idx := &fakeHybridIndex{denseEnabled: true,
			lexical: []indexer.Candidate{hybridCandidate(v1, indexer.ScoreKindFTS5BM25, 1, -1)},
			dense:   []indexer.Candidate{hybridCandidate(v2, indexer.ScoreKindCosineDistance, 1, 0.1)},
		}
		ret := hybridRetriever(idx, zeroHybridPolicy())
		result, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
		if !errors.Is(err, ErrIndexUnavailable) || errors.Is(err, ErrSemanticPageMiss) {
			t.Fatalf("err=%v want operational tuple-race failure", err)
		}
		if !hasSuppression(result.Trace, v1.ID, "channel_contract") {
			t.Fatalf("trace=%#v", result.Trace)
		}
	})

	t.Run("snapshot_mismatch_falls_back_to_fts", func(t *testing.T) {
		page := hybridPage("9", pages.KindConstraint, "snapshot", "E9")
		lex := hybridCandidate(page, indexer.ScoreKindFTS5BM25, 1, -1)
		dense := hybridCandidate(page, indexer.ScoreKindCosineDistance, 1, 0.1)
		dense.ServingGeneration = 2
		idx := &fakeHybridIndex{denseEnabled: true, lexical: []indexer.Candidate{lex}, dense: []indexer.Candidate{dense}}
		ret := hybridRetriever(idx, zeroHybridPolicy())
		result, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
		if err != nil {
			t.Fatal(err)
		}
		if !containsString(result.Degraded, "dense=snapshot") || len(result.Candidates) != 1 {
			t.Fatalf("result=%#v", result)
		}
	})
}

func TestHybridExclusionsDiversityAndRecheckContinuation(t *testing.T) {
	excluded := hybridPage("a", pages.KindFailureEpisode, "excluded unique", "EA")
	first := hybridPage("b", pages.KindFailureEpisode, "first unique", "shared")
	duplicateSource := hybridPage("c", pages.KindFailureEpisode, "second unique", "shared")
	continuation := hybridPage("d", pages.KindConstraint, "continuation unique", "ED")
	idx := &fakeHybridIndex{lexicalFn: func(_ context.Context, query indexer.SearchQuery) ([]indexer.Candidate, error) {
		all := []pages.WarmPage{excluded, first, duplicateSource, continuation}
		var out []indexer.Candidate
		for _, page := range all {
			excludedPage := false
			for _, id := range query.ExcludedPageIDs {
				if id == page.ID {
					excludedPage = true
				}
			}
			if excludedPage {
				continue
			}
			out = append(out, hybridCandidate(page, indexer.ScoreKindFTS5BM25, len(out)+1, float64(len(out))))
		}
		return out, nil
	}}
	policy := zeroHybridPolicy()
	policy.MaxSelected = 2
	policy.MaxPerKind = 2
	policy.MaxPerSource = 1
	var checked []pages.PageID
	ret := &HybridRetriever{Index: idx, Policy: policy, DetailedRecheck: func(_ context.Context, page pages.WarmPage, _ int) (RecheckResult, error) {
		checked = append(checked, page.ID)
		if page.ID == first.ID {
			return RecheckResult{Reason: "TOP_SECRET stale detail"}, nil
		}
		return RecheckResult{Eligible: true, TokensUsed: 1}, nil
	}}
	intent := hybridIntent(2)
	intent.Exclusions = []string{string(excluded.ID)}
	result, err := ret.ResolveSemantic(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if containsPage(result.Candidates, excluded.ID) || containsPage(result.Candidates, first.ID) {
		t.Fatalf("suppressed candidates leaked: %v", candidateIDs(result))
	}
	if !reflect.DeepEqual(candidateIDs(result), []pages.PageID{duplicateSource.ID, continuation.ID}) {
		t.Fatalf("continuation ids=%v checked=%v", candidateIDs(result), checked)
	}
	if strings.Contains(result.Explanation, "TOP_SECRET") || traceContains(result.Trace, "TOP_SECRET") {
		t.Fatalf("raw recheck detail leaked: %#v", result.Trace)
	}
	if !hasSuppression(result.Trace, first.ID, "recheck") {
		t.Fatalf("trace=%#v", result.Trace)
	}
}

func TestHybridDiversityCapsAndDenseAbstention(t *testing.T) {
	t.Run("kind_source_and_near_duplicate_caps", func(t *testing.T) {
		one := hybridPage("a", pages.KindFailureEpisode, "alpha beta gamma", "shared")
		two := hybridPage("b", pages.KindFailureEpisode, "different episode", "shared")
		near := hybridPage("c", pages.KindConstraint, "alpha beta gamma", "other")
		last := hybridPage("d", pages.KindConstraint, "delta epsilon", "last")
		idx := &fakeHybridIndex{lexical: []indexer.Candidate{
			hybridCandidate(one, indexer.ScoreKindFTS5BM25, 1, -4),
			hybridCandidate(two, indexer.ScoreKindFTS5BM25, 2, -3),
			hybridCandidate(near, indexer.ScoreKindFTS5BM25, 3, -2),
			hybridCandidate(last, indexer.ScoreKindFTS5BM25, 4, -1),
		}}
		policy := zeroHybridPolicy()
		policy.MaxSelected, policy.MaxPerKind, policy.MaxPerSource, policy.NearDuplicateThreshold = 2, 1, 1, 0.8
		ret := hybridRetriever(idx, policy)
		ret.Dense = nil
		result, err := ret.ResolveSemantic(context.Background(), hybridIntent(2))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(candidateIDs(result), []pages.PageID{one.ID, last.ID}) {
			t.Fatalf("ids=%v suppressions=%v", candidateIDs(result), result.Trace.Suppressions)
		}
		if !hasSuppression(result.Trace, two.ID, "kind_cap") && !hasSuppression(result.Trace, two.ID, "source_cap") {
			t.Fatalf("missing cap suppression: %#v", result.Trace)
		}
		if !hasSuppression(result.Trace, near.ID, "near_duplicate") {
			t.Fatalf("missing duplicate suppression: %#v", result.Trace)
		}
	})

	t.Run("dense_distance_abstains", func(t *testing.T) {
		page := hybridPage("e", pages.KindRepoFact, "weak dense", "EE")
		idx := &fakeHybridIndex{denseEnabled: true, dense: []indexer.Candidate{hybridCandidate(page, indexer.ScoreKindCosineDistance, 1, 0.8)}}
		policy := zeroHybridPolicy()
		policy.MaxDenseDistance = 0.2
		ret := hybridRetriever(idx, policy)
		_, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
		var miss *SemanticMissError
		if !errors.As(err, &miss) || miss.Trace.Abstention != "threshold" || !hasSuppression(miss.Trace, page.ID, "dense_distance") {
			t.Fatalf("err=%v miss=%#v", err, miss)
		}
	})

	t.Run("minimum_score_abstains", func(t *testing.T) {
		page := hybridPage("f", pages.KindRepoFact, "low score", "EF")
		idx := &fakeHybridIndex{lexical: []indexer.Candidate{hybridCandidate(page, indexer.ScoreKindFTS5BM25, 1, -1)}}
		policy := zeroHybridPolicy()
		policy.MinScore = 1
		ret := hybridRetriever(idx, policy)
		ret.Dense = nil
		_, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
		if !errors.Is(err, ErrSemanticPageMiss) || errors.Is(err, ErrIndexUnavailable) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestHybridPolicyFeaturesCanRerankWithoutWeakeningHardFilters(t *testing.T) {
	generic := hybridPage("1", pages.KindRepoFact, "generic memory", "P1")
	generic.TaskID, generic.Branch, generic.Commit = "", "", ""
	generic.Trust, generic.Salience = pages.TrustModel, 0
	affine := hybridPage("2", pages.KindRepoFact, "panic stream blocked", "P2")
	affine.Salience = 1
	affine.LastVerifiedAt = time.Unix(10, 0).UTC()
	idx := &fakeHybridIndex{lexical: []indexer.Candidate{
		hybridCandidate(generic, indexer.ScoreKindFTS5BM25, 1, -10),
		hybridCandidate(affine, indexer.ScoreKindFTS5BM25, 2, -1),
	}}
	ret := hybridRetriever(idx, DefaultHybridPolicyProfile())
	ret.Dense = nil
	intent := hybridIntent(1)
	intent.RepoID, intent.TaskID, intent.Branch, intent.Commit = "repo", "T1", "main", "abc"
	intent.LastFailure = "panic stream blocked"
	result, err := ret.ResolveSemantic(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if result.Candidates[0].Page.ID != affine.ID {
		t.Fatalf("policy did not rerank: %v", candidateIDs(result))
	}
	trace := traceForPage(t, result.Trace, affine.ID)
	for _, name := range []string{"repo_affinity", "task_affinity", "branch_affinity", "commit_affinity", "trust_user", "salience", "verification", "failure_overlap"} {
		if !hasFeature(trace, name) {
			t.Fatalf("missing feature %q in %#v", name, trace.Features)
		}
	}
}

func TestHybridDetailedRecheckEnforcesCumulativeBudgetAndOperationalErrors(t *testing.T) {
	pagesInRank := []pages.WarmPage{
		hybridPage("1", pages.KindConstraint, "one", "B1"),
		hybridPage("2", pages.KindFailureEpisode, "two", "B2"),
		hybridPage("3", pages.KindRepoFact, "three", "B3"),
	}
	idx := &fakeHybridIndex{}
	for i, page := range pagesInRank {
		idx.lexical = append(idx.lexical, hybridCandidate(page, indexer.ScoreKindFTS5BM25, i+1, float64(i)))
	}
	policy := zeroHybridPolicy()
	policy.MaxSelected, policy.MaxPerKind, policy.MaxPerSource = 3, 3, 3
	var remaining []int
	ret := &HybridRetriever{Index: idx, Policy: policy, DetailedRecheck: func(_ context.Context, _ pages.WarmPage, budget int) (RecheckResult, error) {
		remaining = append(remaining, budget)
		return RecheckResult{Eligible: true, TokensUsed: 3}, nil
	}}
	intent := hybridIntent(3)
	intent.TokenBudget = 6
	result, err := ret.ResolveSemantic(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(remaining, []int{6, 3}) || len(result.Candidates) != 2 || result.Trace.MaterializationTokensUsed != 6 {
		t.Fatalf("remaining=%v result=%#v", remaining, result)
	}
	if !hasSuppression(result.Trace, pagesInRank[2].ID, "budget") {
		t.Fatalf("missing budget suppression: %#v", result.Trace)
	}

	remaining = nil
	ret.DetailedRecheck = func(_ context.Context, page pages.WarmPage, budget int) (RecheckResult, error) {
		remaining = append(remaining, budget)
		if page.ID == pagesInRank[0].ID {
			return RecheckResult{Reason: "stale", TokensUsed: 2}, nil
		}
		return RecheckResult{Eligible: true, TokensUsed: 4}, nil
	}
	result, err = ret.ResolveSemantic(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(remaining, []int{6, 4}) || !reflect.DeepEqual(candidateIDs(result), []pages.PageID{pagesInRank[1].ID}) || result.Trace.MaterializationTokensUsed != 6 {
		t.Fatalf("suppressed materialization was not charged: remaining=%v result=%#v", remaining, result)
	}

	ret.DetailedRecheck = func(context.Context, pages.WarmPage, int) (RecheckResult, error) {
		return RecheckResult{}, errors.New("TOP_SECRET materializer failure")
	}
	_, err = ret.ResolveSemantic(context.Background(), intent)
	if !errors.Is(err, ErrIndexUnavailable) || strings.Contains(err.Error(), "TOP_SECRET") {
		t.Fatalf("err=%v", err)
	}
}

func TestHybridCopiesDenseQueryAndHonorsCancellation(t *testing.T) {
	lexicalEntered, denseEntered := make(chan struct{}, 1), make(chan struct{}, 1)
	idx := &fakeHybridIndex{denseEnabled: true}
	idx.lexicalFn = func(ctx context.Context, _ indexer.SearchQuery) ([]indexer.Candidate, error) {
		lexicalEntered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	idx.denseFn = func(ctx context.Context, _ indexer.SearchQuery, vector []float64) ([]indexer.Candidate, error) {
		denseEntered <- struct{}{}
		<-ctx.Done()
		if !reflect.DeepEqual(vector, []float64{1, 0}) {
			return nil, errors.New("vector was not copied")
		}
		return nil, ctx.Err()
	}
	vector := []float64{1, 0}
	ret := hybridRetriever(idx, zeroHybridPolicy())
	ret.Dense = &DenseQuery{Namespace: "embed/test", Vector: vector}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := ret.ResolveSemantic(ctx, hybridIntent(1)); done <- err }()
	waitTestSignal(t, lexicalEntered)
	waitTestSignal(t, denseEntered)
	vector[0], vector[1] = 0, 1
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("hybrid cancellation did not return")
	}
}

func TestHybridScrubsIndexedProseAndValidatesProfileAndIntentBounds(t *testing.T) {
	page := hybridPage("1", pages.KindConstraint, "TOP_SECRET indexed search text", "SECRET_REF")
	page.Summary = "TOP_SECRET summary"
	page.SalienceReason = "TOP_SECRET derivative salience reason"
	candidate := hybridCandidate(page, indexer.ScoreKindFTS5BM25, 1, -1)
	candidate.Explanation = "TOP_SECRET backend explanation"
	idx := &fakeHybridIndex{lexical: []indexer.Candidate{candidate}}
	ret := hybridRetriever(idx, zeroHybridPolicy())
	ret.Dense = nil
	result, err := ret.ResolveSemantic(context.Background(), hybridIntent(1))
	if err != nil {
		t.Fatal(err)
	}
	got := result.Candidates[0]
	if got.Page.SearchText != "" || got.Page.Summary != "" || got.Page.SalienceReason != "" || len(got.Page.Refs) != 0 {
		t.Fatalf("indexed prose leaked: %#v", got.Page)
	}
	if got.Rank != 0 || got.ScoreKind != "" || !strings.Contains(got.Explanation, "policy=working-set/v1-provisional") {
		t.Fatalf("public fused score semantics are ambiguous: %#v", got)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "TOP_SECRET") || strings.Contains(string(encoded), "SECRET_REF") {
		t.Fatalf("public explanation leaked prose: %s", encoded)
	}
	traceJSON, err := json.Marshal(result.Trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(traceJSON), string(page.SourceDigest)) || strings.Contains(string(traceJSON), string(page.CompilerVersion)) {
		t.Fatalf("public trace exposed eligibility tuple contents: %s", traceJSON)
	}

	badPolicy := zeroHybridPolicy()
	badPolicy.Weights.Salience = math.NaN()
	ret.Policy = badPolicy
	if _, err := ret.ResolveSemantic(context.Background(), hybridIntent(1)); err == nil {
		t.Fatal("NaN policy accepted")
	}

	ret.Policy = zeroHybridPolicy()
	badIntent := hybridIntent(1)
	badIntent.PathHints = make([]string, maxIntentListEntries+1)
	if _, err := ret.ResolveSemantic(context.Background(), badIntent); err == nil {
		t.Fatal("unbounded intent list accepted")
	}
	badIntent = hybridIntent(1)
	badIntent.ScopeEpochs = make([]pages.ScopeEpoch, maxIntentListEntries+1)
	if _, err := ret.ResolveSemantic(context.Background(), badIntent); err == nil {
		t.Fatal("unbounded scope epoch list accepted")
	}
	badIntent = hybridIntent(1)
	badIntent.ScopeEpochs = []pages.ScopeEpoch{pages.ScopeEpoch(math.MaxInt64) + 1}
	if _, err := ret.ResolveSemantic(context.Background(), badIntent); err == nil {
		t.Fatal("out-of-range scope epoch accepted")
	}
	badIntent = hybridIntent(1)
	badIntent.EligiblePageTuples = make([]indexer.PageTuple, maxIntentEligibilityPages+1)
	if _, err := ret.ResolveSemantic(context.Background(), badIntent); err == nil {
		t.Fatal("unbounded eligibility snapshot accepted")
	}
	badIntent = hybridIntent(1)
	conflict := badIntent.EligiblePageTuples[0]
	conflict.PageVersion++
	badIntent.EligiblePageTuples = append(badIntent.EligiblePageTuples, conflict)
	if _, err := ret.ResolveSemantic(context.Background(), badIntent); err == nil {
		t.Fatal("conflicting same-page authority tuples accepted")
	}
	badIntent = hybridIntent(1)
	badIntent.EligibilityComplete = false
	if _, err := ret.ResolveSemantic(context.Background(), badIntent); err == nil {
		t.Fatal("tuple payload without complete snapshot accepted")
	}
}

type fakeHybridIndex struct {
	mu             sync.Mutex
	denseEnabled   bool
	lexical        []indexer.Candidate
	dense          []indexer.Candidate
	lexicalErr     error
	denseErr       error
	lexicalFn      func(context.Context, indexer.SearchQuery) ([]indexer.Candidate, error)
	denseFn        func(context.Context, indexer.SearchQuery, []float64) ([]indexer.Candidate, error)
	lexicalQuery   indexer.SearchQuery
	denseQuery     indexer.SearchQuery
	lexicalCallCnt int
	denseCallCnt   int
}

func (f *fakeHybridIndex) DenseEnabled() bool { return f.denseEnabled }

func (f *fakeHybridIndex) SearchLexical(ctx context.Context, query indexer.SearchQuery) ([]indexer.Candidate, error) {
	f.mu.Lock()
	f.lexicalCallCnt++
	f.lexicalQuery = cloneSearchQuery(query)
	fn, candidates, err := f.lexicalFn, append([]indexer.Candidate(nil), f.lexical...), f.lexicalErr
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, query)
	}
	return candidates, err
}

func (f *fakeHybridIndex) SearchDense(ctx context.Context, query indexer.SearchQuery, vector []float64) ([]indexer.Candidate, error) {
	f.mu.Lock()
	f.denseCallCnt++
	f.denseQuery = cloneSearchQuery(query)
	fn, candidates, err := f.denseFn, append([]indexer.Candidate(nil), f.dense...), f.denseErr
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, query, vector)
	}
	return candidates, err
}

func (f *fakeHybridIndex) denseCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.denseCallCnt }
func (f *fakeHybridIndex) lexicalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lexicalCallCnt
}
func (f *fakeHybridIndex) queries() (indexer.SearchQuery, indexer.SearchQuery) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lexicalQuery, f.denseQuery
}

func cloneSearchQuery(query indexer.SearchQuery) indexer.SearchQuery {
	query.Kinds = append([]pages.PageKind(nil), query.Kinds...)
	query.Trust = append([]pages.Trust(nil), query.Trust...)
	query.Statuses = append([]pages.Status(nil), query.Statuses...)
	query.ScopeEpochs = append([]pages.ScopeEpoch(nil), query.ScopeEpochs...)
	query.EligiblePageTuples = append([]indexer.PageTuple(nil), query.EligiblePageTuples...)
	query.PathHints = append([]string(nil), query.PathHints...)
	query.ExcludedPageIDs = append([]pages.PageID(nil), query.ExcludedPageIDs...)
	query.ExcludedRefIDs = append([]string(nil), query.ExcludedRefIDs...)
	return query
}

func hybridRetriever(index HybridIndex, policy HybridPolicyProfile) *HybridRetriever {
	return &HybridRetriever{
		Index: index, Dense: &DenseQuery{Namespace: "embed/test", Vector: []float64{1, 0}}, Policy: policy,
		DetailedRecheck: func(context.Context, pages.WarmPage, int) (RecheckResult, error) {
			return RecheckResult{Eligible: true}, nil
		},
	}
}

func hybridIntent(materialize int) QueryIntent {
	eligible := make([]indexer.PageTuple, 0, 15)
	for _, digit := range "123456789abcdef" {
		page := hybridPage(string(digit), pages.KindConstraint, "", "")
		eligible = append(eligible, pageTuple(page))
	}
	return QueryIntent{
		Mode: ModeSemanticFault, SessionID: "s", ScopeEpochs: []pages.ScopeEpoch{0},
		EligibilityComplete: true, EligiblePageTuples: eligible,
		UserText: "working set query", CandidateLimit: 20, MaterializeLimit: materialize,
	}
}

func zeroHybridPolicy() HybridPolicyProfile {
	policy := DefaultHybridPolicyProfile()
	policy.MinScore = 0
	policy.MaxDenseDistance = 2
	policy.NearDuplicateThreshold = 1
	policy.MaxPerKind = 3
	policy.MaxPerSource = 3
	policy.Weights = HybridPolicyWeights{}
	return policy
}

func hybridPage(hex string, kind pages.PageKind, text, source string) pages.WarmPage {
	id := pages.PageID("wp_" + strings.Repeat(hex, 32))
	return pages.WarmPage{
		ID: id, Version: 1, SessionID: "s", RepoID: "repo", TaskID: "T1", Branch: "main", Commit: "abc",
		Scope: types.ScopeTask, Kind: kind, Trust: pages.TrustUser, Status: pages.StatusActive,
		Salience: 0.8, SalienceReason: "test", SearchText: text, Summary: text,
		Refs: []pages.PageRef{{Kind: pages.RefEvent, ID: source}}, SourceSeqMin: 1, SourceSeqMax: 1,
		SourceDigest: pages.SourceDigest(strings.Repeat(hex, 64)), CompilerVersion: pages.CurrentCompilerVersion,
		CreatedAt: time.Unix(1, 0).UTC(),
	}
}

func epochHybridPage(number int, epoch pages.ScopeEpoch) pages.WarmPage {
	page := hybridPage("a", pages.KindConstraint, "scope epoch starvation needle", fmt.Sprintf("E%d", number))
	page.ID = pages.PageID(fmt.Sprintf("wp_%032x", number))
	page.SourceDigest = pages.SourceDigest(fmt.Sprintf("%064x", number))
	page.Scope = types.ScopeBranch
	page.ScopeEpoch = epoch
	return page
}

func withDigest(page pages.WarmPage, hex string) pages.WarmPage {
	page.SourceDigest = pages.SourceDigest(strings.Repeat(hex, 64))
	return page
}

func hybridCandidate(page pages.WarmPage, scoreKind indexer.ScoreKind, ordinal int, rank float64) indexer.Candidate {
	return indexer.Candidate{
		Page: page, Rank: rank, Explanation: "TOP_SECRET backend explanation",
		Tuple:             pageTuple(page),
		ServingGeneration: 1, Watermark: indexer.SearchWatermark{SessionID: page.SessionID, LastSourceSeq: 10, LastPageVersion: 10},
		ChannelOrdinal: ordinal, ScoreKind: scoreKind,
	}
}

func pageTuple(page pages.WarmPage) indexer.PageTuple {
	return indexer.PageTuple{
		PageID: page.ID, PageVersion: page.Version, SessionID: page.SessionID,
		SourceDigest: page.SourceDigest, CompilerVersion: page.CompilerVersion, ScopeEpoch: page.ScopeEpoch,
	}
}

func candidateIDs(result Result) []pages.PageID {
	out := make([]pages.PageID, 0, len(result.Candidates))
	for _, candidate := range result.Candidates {
		out = append(out, candidate.Page.ID)
	}
	return out
}

func traceForPage(t *testing.T, trace RetrievalTrace, id pages.PageID) CandidateTrace {
	t.Helper()
	for _, candidate := range trace.Candidates {
		if candidate.PageID == id {
			return candidate
		}
	}
	t.Fatalf("trace missing page %s", id)
	return CandidateTrace{}
}

func hasSuppression(trace RetrievalTrace, id pages.PageID, reason string) bool {
	for _, suppression := range trace.Suppressions {
		if suppression.PageID == id && suppression.Reason == reason {
			return true
		}
	}
	return false
}

func hasFeature(trace CandidateTrace, name string) bool {
	for _, feature := range trace.Features {
		if feature.Name == name {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func containsPage(candidates []indexer.Candidate, want pages.PageID) bool {
	for _, candidate := range candidates {
		if candidate.Page.ID == want {
			return true
		}
	}
	return false
}

func traceContains(trace RetrievalTrace, text string) bool {
	encoded, _ := json.Marshal(trace)
	return strings.Contains(string(encoded), text)
}

func waitTestSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel start")
	}
}
