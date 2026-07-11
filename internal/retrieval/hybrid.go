package retrieval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"wsms/internal/indexer"
	"wsms/internal/pages"
)

const (
	// RRFFusionVersion names the checked-in reciprocal-rank-fusion contract.
	RRFFusionVersion   = "rrf/v1"
	defaultRRFK        = 60.0
	defaultTokenBudget = 4096
)

// HybridIndex is the filtered, provenance-bearing search contract consumed by
// Phase 7E. Implementations must apply every SearchQuery hard filter before
// their channel limit and populate Candidate tuple/snapshot metadata.
type HybridIndex interface {
	SearchLexical(context.Context, indexer.SearchQuery) ([]indexer.Candidate, error)
	SearchDense(context.Context, indexer.SearchQuery, []float64) ([]indexer.Candidate, error)
	DenseEnabled() bool
}

// DenseUnavailableReason is a safe categorical reason the harness could not
// produce a usable dense query. It never carries backend text.
type DenseUnavailableReason string

const (
	DenseUnavailableEmbedder    DenseUnavailableReason = "embedder"
	DenseUnavailableNamespace   DenseUnavailableReason = "namespace"
	DenseUnavailableQueryVector DenseUnavailableReason = "query-vector"
	DenseUnavailableAdmission   DenseUnavailableReason = "admission"
	DenseUnavailableSearch      DenseUnavailableReason = "search"
)

// DenseQuery is either a prevalidated query vector plus its complete embedding
// ABI namespace, or an explicit operational-unavailable category. ResolveSemantic
// copies it before starting concurrent searches.
type DenseQuery struct {
	Namespace         string
	Vector            []float64
	UnavailableReason DenseUnavailableReason
}

// RecheckResult is the detailed current-authority decision for one candidate.
// TokensUsed is charged against QueryIntent.TokenBudget cumulatively.
type RecheckResult struct {
	Eligible   bool
	Reason     string
	TokensUsed int
}

// DetailedRecheckFunc distinguishes a current-authority suppression from an
// operational validation/materialization error and reports cumulative cost.
type DetailedRecheckFunc func(context.Context, pages.WarmPage, int) (RecheckResult, error)

// HybridRetriever resolves one intent through concurrent lexical and optional
// dense channels. Dense is per-call state; callers should construct a retriever
// after validating the query embedding.
type HybridRetriever struct {
	Index           HybridIndex
	Dense           *DenseQuery
	Recheck         RecheckFunc
	DetailedRecheck DetailedRecheckFunc
	Policy          HybridPolicyProfile
}

// PolicyFeatureTrace records one nonzero, named deterministic policy feature.
// Values and weights are numeric only; query and indexed prose are never copied.
type PolicyFeatureTrace struct {
	Name         string
	Value        float64
	Weight       float64
	Contribution float64
}

// CandidateTrace is the bounded, text-free ranking explanation for one page.
type CandidateTrace struct {
	PageID          pages.PageID
	LexicalPosition int
	LexicalRank     float64
	DensePosition   int
	DenseDistance   float64
	RRFScore        float64
	PolicyScore     float64
	FinalScore      float64
	Features        []PolicyFeatureTrace
	Selected        bool
}

// SuppressionTrace is one categorical reason a page did not survive.
type SuppressionTrace struct {
	PageID pages.PageID
	Reason string
}

// RetrievalTrace is a bounded, deterministic, text-free hybrid-search trace.
type RetrievalTrace struct {
	FusionVersion              string
	RRFK                       float64
	PolicyName                 string
	PolicyVersion              string
	PolicyProvisional          bool
	Filters                    []string
	LexicalCandidates          int
	DenseCandidates            int
	Candidates                 []CandidateTrace
	Suppressions               []SuppressionTrace
	SuppressionCount           int
	Selected                   []pages.PageID
	Degraded                   []string
	Abstention                 string
	MaterializationTokenBudget int
	MaterializationTokensUsed  int
}

// SemanticMissError preserves a safe abstention trace while remaining
// compatible with errors.Is(err, ErrSemanticPageMiss).
type SemanticMissError struct {
	Explanation string
	Trace       RetrievalTrace
}

func (e *SemanticMissError) Error() string { return ErrSemanticPageMiss.Error() }
func (e *SemanticMissError) Unwrap() error { return ErrSemanticPageMiss }

type searchChannel string

const (
	channelLexical searchChannel = "fts"
	channelDense   searchChannel = "dense"
)

type channelResult struct {
	channel    searchChannel
	candidates []indexer.Candidate
	err        error
}

type channelSnapshot struct {
	generation int64
	watermark  indexer.SearchWatermark
	set        bool
}

type fusedCandidate struct {
	page          pages.WarmPage
	lexical       *indexer.Candidate
	dense         *indexer.Candidate
	denseAccepted bool
	rrfScore      float64
	policyScore   float64
	finalScore    float64
	features      []PolicyFeatureTrace
}

// ResolveSemantic runs hard filters, concurrent candidate generation,
// tuple-aware RRF, policy rerank, diversity, abstention, and serial authority
// rechecks. Semantic-fault and prefetch requests require a complete current
// eligibility snapshot; inspection may intentionally examine historical rows
// without one. Operational failure is never reported as a semantic miss.
func (r *HybridRetriever) ResolveSemantic(ctx context.Context, intent QueryIntent) (Result, error) {
	if r == nil || r.Index == nil {
		return Result{}, ErrIndexUnavailable
	}
	if ctx == nil {
		return Result{}, fmt.Errorf("nil context")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := intent.validate(); err != nil {
		return Result{}, err
	}
	eligibleTuples, err := intent.effectiveEligiblePageTuples()
	if err != nil {
		return Result{}, err
	}
	if hybridModeRequiresEligibility(intent.Mode) && !intent.EligibilityComplete {
		return Result{}, authorityUnavailableError()
	}
	if r.DetailedRecheck == nil && r.Recheck == nil {
		return Result{}, fmt.Errorf("hybrid authority recheck is required")
	}

	policy := r.Policy
	if policy.Name == "" && policy.Version == "" {
		policy = DefaultHybridPolicyProfile()
	}
	if err := policy.validate(); err != nil {
		return Result{}, err
	}

	limit := intent.CandidateLimit
	if limit <= 0 {
		limit = 20
	}
	if limit > policy.MaxCandidates {
		limit = policy.MaxCandidates
	}
	selectedLimit := intent.MaterializeLimit
	if selectedLimit <= 0 {
		selectedLimit = policy.MaxSelected
	}
	if selectedLimit > policy.MaxSelected {
		selectedLimit = policy.MaxSelected
	}
	tokenBudget := intent.TokenBudget
	if tokenBudget <= 0 {
		tokenBudget = defaultTokenBudget
	}

	dense, denseRequested, denseInputOK, denseInputMarker := copyDenseQuery(r.Dense)
	query := buildHybridSearchQuery(intent, dense.Namespace, limit, eligibleTuples)
	trace := RetrievalTrace{
		FusionVersion: RRFFusionVersion, RRFK: policy.RRFK,
		PolicyName: policy.Name, PolicyVersion: policy.Version,
		PolicyProvisional:          policy.Provisional,
		Filters:                    hybridFilterNames(query),
		MaterializationTokenBudget: tokenBudget,
	}
	degraded := make([]string, 0, 2)
	operationalFailure := false

	results := make(chan channelResult, 2)
	expected := 1
	go func() {
		candidates, err := r.Index.SearchLexical(ctx, query)
		results <- channelResult{channel: channelLexical, candidates: candidates, err: err}
	}()
	denseActive := denseRequested && denseInputOK && r.Index.DenseEnabled()
	if denseActive {
		expected++
		go func() {
			candidates, err := r.Index.SearchDense(ctx, query, dense.Vector)
			results <- channelResult{channel: channelDense, candidates: candidates, err: err}
		}()
	} else {
		switch {
		case denseRequested && !denseInputOK:
			degraded = append(degraded, denseInputMarker)
			operationalFailure = true
		default:
			degraded = append(degraded, "dense=unavailable")
		}
	}

	var lexical, denseCandidates []indexer.Candidate
	var lexicalSnapshot, denseSnapshot channelSnapshot
	contractBlocked := make(map[pages.PageID]struct{})
	for i := 0; i < expected; i++ {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case result := <-results:
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded) {
					if err := ctx.Err(); err != nil {
						return Result{}, err
					}
				}
				if result.channel == channelLexical && errors.Is(result.err, indexer.ErrEmptyQuery) {
					continue
				}
				operationalFailure = true
				if result.channel == channelLexical {
					degraded = append(degraded, "fts=search")
				} else {
					degraded = append(degraded, denseFailureMarker(result.err))
				}
				continue
			}

			validated, snapshot, badPage, ok := validateChannelCandidates(result.candidates, query, result.channel)
			if !ok {
				trace.Suppressions = append(trace.Suppressions, SuppressionTrace{PageID: badPage, Reason: "channel_contract"})
				if badPage != "" {
					contractBlocked[badPage] = struct{}{}
				}
				operationalFailure = true
				if result.channel == channelLexical {
					degraded = append(degraded, "fts=contract")
				} else {
					degraded = append(degraded, "dense=contract")
				}
				continue
			}
			if result.channel == channelLexical {
				lexical, lexicalSnapshot = validated, snapshot
			} else {
				denseCandidates, denseSnapshot = validated, snapshot
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if len(contractBlocked) > 0 {
		lexical = withoutBlocked(lexical, contractBlocked)
		denseCandidates = withoutBlocked(denseCandidates, contractBlocked)
	}
	lexical, denseCandidates = suppressTupleDisagreements(lexical, denseCandidates, &trace)
	if lexicalSnapshot.set && denseSnapshot.set && lexicalSnapshot != denseSnapshot {
		denseCandidates = nil
		operationalFailure = true
		degraded = append(degraded, "dense=snapshot")
	}
	trace.LexicalCandidates = len(lexical)
	trace.DenseCandidates = len(denseCandidates)

	fused := fuseCandidates(lexical, denseCandidates, policy, intent, &trace)
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].finalScore == fused[j].finalScore {
			return string(fused[i].page.ID) < string(fused[j].page.ID)
		}
		return fused[i].finalScore > fused[j].finalScore
	})

	remaining := tokenBudget
	selectedPages := make([]pages.WarmPage, 0, selectedLimit)
	selected := make([]indexer.Candidate, 0, selectedLimit)
	kindCounts := make(map[pages.PageKind]int)
	sourceCounts := make(map[string]int)
	traceIndex := make(map[pages.PageID]int, len(trace.Candidates))
	for i := range trace.Candidates {
		traceIndex[trace.Candidates[i].PageID] = i
	}
	for _, candidate := range fused {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if len(selected) >= selectedLimit {
			break
		}
		if kindCounts[candidate.page.Kind] >= policy.MaxPerKind {
			addSuppression(&trace, candidate.page.ID, "kind_cap")
			continue
		}
		if sourceCapReached(candidate.page, sourceCounts, policy.MaxPerSource) {
			addSuppression(&trace, candidate.page.ID, "source_cap")
			continue
		}
		if nearDuplicate(candidate.page, selectedPages, policy.NearDuplicateThreshold) {
			addSuppression(&trace, candidate.page.ID, "near_duplicate")
			continue
		}
		if remaining <= 0 {
			addSuppression(&trace, candidate.page.ID, "budget")
			continue
		}
		eligible, used, reason, err := r.recheck(ctx, candidate.page, remaining)
		if err != nil {
			addSuppression(&trace, candidate.page.ID, "recheck_error")
			trace.Degraded = stableMarkers(append(degraded, "authority=recheck"))
			trace.SuppressionCount = len(trace.Suppressions)
			trace.Abstention = "operational"
			return operationalResult(intent, trace), operationalError("authority recheck failed")
		}
		remaining -= used
		trace.MaterializationTokensUsed += used
		if !eligible {
			addSuppression(&trace, candidate.page.ID, reason)
			continue
		}
		kindCounts[candidate.page.Kind]++
		incrementSourceCounts(candidate.page, sourceCounts)
		selectedPages = append(selectedPages, candidate.page)
		selected = append(selected, publicCandidate(candidate, policy))
		trace.Selected = append(trace.Selected, candidate.page.ID)
		if i, ok := traceIndex[candidate.page.ID]; ok {
			trace.Candidates[i].Selected = true
		}
	}

	degraded = stableMarkers(degraded)
	trace.Degraded = append([]string(nil), degraded...)
	trace.SuppressionCount = len(trace.Suppressions)
	if len(selected) == 0 {
		trace.Abstention = abstentionReason(trace)
		explanation := hybridExplanation(intent, trace, 0)
		if operationalFailure {
			return operationalResult(intent, trace), operationalError("hybrid search incomplete")
		}
		return Result{}, &SemanticMissError{Explanation: explanation, Trace: trace}
	}
	return Result{
		Candidates:  selected,
		Explanation: hybridExplanation(intent, trace, len(selected)),
		Degraded:    degraded,
		Trace:       trace,
	}, nil
}

func copyDenseQuery(input *DenseQuery) (DenseQuery, bool, bool, string) {
	if input == nil {
		return DenseQuery{}, false, true, ""
	}
	out := DenseQuery{Namespace: strings.TrimSpace(input.Namespace), Vector: append([]float64(nil), input.Vector...), UnavailableReason: input.UnavailableReason}
	if out.UnavailableReason != "" {
		return out, true, false, denseUnavailableMarker(out.UnavailableReason)
	}
	if out.Namespace == "" || len(out.Namespace) > maxIntentContextBytes || len(out.Vector) == 0 || len(out.Vector) > pages.MaxVectorDimensions {
		return out, true, false, "dense=query-vector"
	}
	var norm float64
	for _, value := range out.Vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return out, true, false, "dense=query-vector"
		}
		norm = math.Hypot(norm, value)
	}
	if norm == 0 || math.IsInf(norm, 0) {
		return out, true, false, "dense=query-vector"
	}
	return out, true, true, ""
}

func denseUnavailableMarker(reason DenseUnavailableReason) string {
	switch reason {
	case DenseUnavailableEmbedder:
		return "dense=embedder"
	case DenseUnavailableNamespace:
		return "dense=namespace"
	case DenseUnavailableQueryVector:
		return "dense=query-vector"
	case DenseUnavailableAdmission:
		return "dense=admission"
	case DenseUnavailableSearch:
		return "dense=search"
	default:
		return "dense=query-vector"
	}
}

func buildHybridSearchQuery(intent QueryIntent, namespace string, limit int, eligibleTuples []indexer.PageTuple) indexer.SearchQuery {
	pageIDs, refIDs := splitExclusions(intent.Exclusions)
	return indexer.SearchQuery{
		SessionID: intent.SessionID, RepoID: intent.RepoID, TaskID: intent.TaskID,
		Branch: intent.Branch, Commit: intent.Commit, EmbeddingNamespace: namespace,
		Kinds: append([]pages.PageKind(nil), intent.AllowedKinds...),
		Trust: intent.effectiveTrust(), Text: intent.searchText(), Limit: limit,
		ActiveOnly: true, Statuses: []pages.Status{pages.StatusActive},
		ScopeEpochs:         hybridScopeEpochs(intent, eligibleTuples),
		EligibilityComplete: intent.EligibilityComplete,
		EligiblePageTuples:  append([]indexer.PageTuple(nil), eligibleTuples...),
		PathHints:           normalizeHints(intent.PathHints), ExcludedPageIDs: pageIDs,
		ExcludedRefIDs: refIDs,
	}
}

func hybridScopeEpochs(intent QueryIntent, eligibleTuples []indexer.PageTuple) []pages.ScopeEpoch {
	if !intent.EligibilityComplete {
		return intent.effectiveScopeEpochs()
	}
	seen := make(map[pages.ScopeEpoch]struct{}, len(eligibleTuples))
	for _, tuple := range eligibleTuples {
		seen[tuple.ScopeEpoch] = struct{}{}
	}
	out := make([]pages.ScopeEpoch, 0, len(seen))
	for epoch := range seen {
		out = append(out, epoch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func splitExclusions(exclusions []string) ([]pages.PageID, []string) {
	pageSet := make(map[pages.PageID]struct{})
	refSet := make(map[string]struct{})
	for _, raw := range exclusions {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if strings.HasPrefix(value, "wp_") {
			pageSet[pages.PageID(value)] = struct{}{}
		} else {
			refSet[value] = struct{}{}
		}
	}
	pageIDs := make([]pages.PageID, 0, len(pageSet))
	for id := range pageSet {
		pageIDs = append(pageIDs, id)
	}
	sort.Slice(pageIDs, func(i, j int) bool { return pageIDs[i] < pageIDs[j] })
	refIDs := make([]string, 0, len(refSet))
	for id := range refSet {
		refIDs = append(refIDs, id)
	}
	sort.Strings(refIDs)
	return pageIDs, refIDs
}

func hybridFilterNames(query indexer.SearchQuery) []string {
	filters := []string{"session", "status", "trust"}
	if query.RepoID != "" {
		filters = append(filters, "repo")
	}
	if query.TaskID != "" {
		filters = append(filters, "task")
	}
	if query.Branch != "" {
		filters = append(filters, "branch")
	}
	if query.Commit != "" {
		filters = append(filters, "commit")
	}
	if len(query.Kinds) > 0 {
		filters = append(filters, "kind")
	}
	if len(query.ScopeEpochs) > 0 {
		filters = append(filters, "scope_epoch")
	}
	if query.EligibilityComplete {
		filters = append(filters, "eligibility")
	}
	if len(query.PathHints) > 0 {
		filters = append(filters, "path")
	}
	if len(query.ExcludedPageIDs)+len(query.ExcludedRefIDs) > 0 {
		filters = append(filters, "exclusions")
	}
	return filters
}

func validateChannelCandidates(candidates []indexer.Candidate, query indexer.SearchQuery, channel searchChannel) ([]indexer.Candidate, channelSnapshot, pages.PageID, bool) {
	var snapshot channelSnapshot
	seen := make(map[pages.PageID]struct{}, len(candidates))
	wantScore := indexer.ScoreKindFTS5BM25
	if channel == channelDense {
		wantScore = indexer.ScoreKindCosineDistance
	}
	for i, candidate := range candidates {
		page := candidate.Page
		if page.ID == "" || candidate.ServingGeneration <= 0 || candidate.ScoreKind != wantScore ||
			candidate.ChannelOrdinal != i+1 || math.IsNaN(candidate.Rank) || math.IsInf(candidate.Rank, 0) ||
			(channel == channelDense && candidate.Rank < 0) || !tupleMatchesPage(candidate.Tuple, page) ||
			candidate.Watermark.SessionID != query.SessionID || !matchesHardUniverse(page, candidate.Tuple, query) {
			return nil, channelSnapshot{}, page.ID, false
		}
		if _, duplicate := seen[page.ID]; duplicate {
			return nil, channelSnapshot{}, page.ID, false
		}
		seen[page.ID] = struct{}{}
		current := channelSnapshot{generation: candidate.ServingGeneration, watermark: candidate.Watermark, set: true}
		if snapshot.set && snapshot != current {
			return nil, channelSnapshot{}, page.ID, false
		}
		snapshot = current
	}
	return append([]indexer.Candidate(nil), candidates...), snapshot, "", true
}

func tupleMatchesPage(tuple indexer.PageTuple, page pages.WarmPage) bool {
	return tuple.PageID == page.ID && tuple.PageVersion == page.Version && tuple.SessionID == page.SessionID &&
		tuple.SourceDigest == page.SourceDigest && tuple.CompilerVersion == page.CompilerVersion && tuple.ScopeEpoch == page.ScopeEpoch
}

func matchesHardUniverse(page pages.WarmPage, tuple indexer.PageTuple, query indexer.SearchQuery) bool {
	if page.SessionID != query.SessionID || page.Status != pages.StatusActive {
		return false
	}
	if query.RepoID != "" && page.RepoID != query.RepoID {
		return false
	}
	if query.TaskID != "" && page.TaskID != "" && page.TaskID != query.TaskID {
		return false
	}
	if query.Branch != "" && page.Branch != "" && page.Branch != query.Branch {
		return false
	}
	if query.Commit != "" && page.Commit != "" && page.Commit != query.Commit {
		return false
	}
	if len(query.Kinds) > 0 && !containsKind(query.Kinds, page.Kind) {
		return false
	}
	if len(query.Trust) > 0 && !containsTrust(query.Trust, page.Trust) {
		return false
	}
	if len(query.ScopeEpochs) > 0 && !containsScopeEpoch(query.ScopeEpochs, page.ScopeEpoch) {
		return false
	}
	if query.EligibilityComplete && !containsEligibleTuple(query.EligiblePageTuples, tuple) {
		return false
	}
	if len(query.PathHints) > 0 && len(page.PathScope) > 0 && !pathAffinity(page, query.PathHints) {
		return false
	}
	for _, id := range query.ExcludedPageIDs {
		if id == page.ID {
			return false
		}
	}
	for _, ref := range page.Refs {
		for _, id := range query.ExcludedRefIDs {
			if ref.ID == id {
				return false
			}
		}
	}
	return true
}

func containsEligibleTuple(tuples []indexer.PageTuple, want indexer.PageTuple) bool {
	for _, tuple := range tuples {
		if tuple == want {
			return true
		}
	}
	return false
}

func containsKind(kinds []pages.PageKind, want pages.PageKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

func containsTrust(trusts []pages.Trust, want pages.Trust) bool {
	for _, trust := range trusts {
		if trust == want {
			return true
		}
	}
	return false
}

func containsScopeEpoch(epochs []pages.ScopeEpoch, want pages.ScopeEpoch) bool {
	for _, epoch := range epochs {
		if epoch == want {
			return true
		}
	}
	return false
}

func suppressTupleDisagreements(lexical, dense []indexer.Candidate, trace *RetrievalTrace) ([]indexer.Candidate, []indexer.Candidate) {
	lexicalByID := make(map[pages.PageID]indexer.PageTuple, len(lexical))
	for _, candidate := range lexical {
		lexicalByID[candidate.Page.ID] = candidate.Tuple
	}
	blocked := make(map[pages.PageID]struct{})
	for _, candidate := range dense {
		if tuple, ok := lexicalByID[candidate.Page.ID]; ok && tuple != candidate.Tuple {
			blocked[candidate.Page.ID] = struct{}{}
			addSuppression(trace, candidate.Page.ID, "tuple_mismatch")
		}
	}
	if len(blocked) == 0 {
		return lexical, dense
	}
	return withoutBlocked(lexical, blocked), withoutBlocked(dense, blocked)
}

func withoutBlocked(candidates []indexer.Candidate, blocked map[pages.PageID]struct{}) []indexer.Candidate {
	out := make([]indexer.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, found := blocked[candidate.Page.ID]; !found {
			out = append(out, candidate)
		}
	}
	return out
}

func fuseCandidates(lexical, dense []indexer.Candidate, policy HybridPolicyProfile, intent QueryIntent, trace *RetrievalTrace) []fusedCandidate {
	byID := make(map[pages.PageID]*fusedCandidate, len(lexical)+len(dense))
	for i := range lexical {
		candidate := lexical[i]
		entry := &fusedCandidate{page: candidate.Page, lexical: &candidate}
		entry.rrfScore = 1 / (policy.RRFK + float64(candidate.ChannelOrdinal))
		byID[candidate.Page.ID] = entry
	}
	for i := range dense {
		candidate := dense[i]
		entry := byID[candidate.Page.ID]
		if entry == nil {
			entry = &fusedCandidate{page: candidate.Page}
			byID[candidate.Page.ID] = entry
		}
		entry.dense = &candidate
		if candidate.Rank <= policy.MaxDenseDistance {
			entry.denseAccepted = true
			entry.rrfScore += 1 / (policy.RRFK + float64(candidate.ChannelOrdinal))
		} else {
			addSuppression(trace, candidate.Page.ID, "dense_distance")
		}
	}

	ids := make([]pages.PageID, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]fusedCandidate, 0, len(ids))
	for _, id := range ids {
		entry := byID[id]
		entry.policyScore, entry.features = scorePolicy(policy, entry.page, intent)
		entry.finalScore = entry.rrfScore + entry.policyScore
		candidateTrace := CandidateTrace{PageID: id, RRFScore: entry.rrfScore, PolicyScore: entry.policyScore, FinalScore: entry.finalScore, Features: append([]PolicyFeatureTrace(nil), entry.features...)}
		if entry.lexical != nil {
			candidateTrace.LexicalPosition = entry.lexical.ChannelOrdinal
			candidateTrace.LexicalRank = entry.lexical.Rank
		}
		if entry.dense != nil {
			candidateTrace.DensePosition = entry.dense.ChannelOrdinal
			candidateTrace.DenseDistance = entry.dense.Rank
		}
		trace.Candidates = append(trace.Candidates, candidateTrace)
		if entry.lexical == nil && !entry.denseAccepted {
			continue
		}
		if entry.finalScore < policy.MinScore {
			addSuppression(trace, id, "min_score")
			continue
		}
		out = append(out, *entry)
	}
	return out
}

func (r *HybridRetriever) recheck(ctx context.Context, page pages.WarmPage, remaining int) (bool, int, string, error) {
	if r.DetailedRecheck != nil {
		result, err := r.DetailedRecheck(ctx, page, remaining)
		if err != nil {
			return false, 0, "recheck_error", err
		}
		if result.TokensUsed < 0 || result.TokensUsed > remaining {
			return false, 0, "recheck_error", fmt.Errorf("invalid recheck token accounting")
		}
		if !result.Eligible {
			return false, result.TokensUsed, safeSuppressionReason(result.Reason), nil
		}
		return true, result.TokensUsed, "", nil
	}
	ok, reason := r.Recheck(ctx, page)
	if !ok {
		return false, 0, safeSuppressionReason(reason), nil
	}
	return true, 0, "", nil
}

func publicCandidate(candidate fusedCandidate, policy HybridPolicyProfile) indexer.Candidate {
	page := candidate.page
	page.SearchText = ""
	page.Summary = ""
	page.SalienceReason = ""
	page.Refs = nil
	// A fused score is higher-is-better and therefore must not be placed in
	// indexer.Candidate.Rank, whose channel scores are lower-is-better. The
	// unambiguous fused value lives in Result.Trace.
	base := indexer.Candidate{Page: page, Tuple: candidateTuple(candidate), ServingGeneration: candidateGeneration(candidate), Watermark: candidateWatermark(candidate)}
	base.Explanation = candidateExplanation(candidate, policy)
	return base
}

func candidateTuple(candidate fusedCandidate) indexer.PageTuple {
	if candidate.lexical != nil {
		return candidate.lexical.Tuple
	}
	return candidate.dense.Tuple
}
func candidateGeneration(candidate fusedCandidate) int64 {
	if candidate.lexical != nil {
		return candidate.lexical.ServingGeneration
	}
	return candidate.dense.ServingGeneration
}
func candidateWatermark(candidate fusedCandidate) indexer.SearchWatermark {
	if candidate.lexical != nil {
		return candidate.lexical.Watermark
	}
	return candidate.dense.Watermark
}

func candidateExplanation(candidate fusedCandidate, policy HybridPolicyProfile) string {
	parts := []string{fmt.Sprintf("fusion=%s rrf=%.8f policy=%s/%s provisional=%t", RRFFusionVersion, candidate.rrfScore, policy.Name, policy.Version, policy.Provisional)}
	if candidate.lexical != nil {
		parts = append(parts, fmt.Sprintf("fts_pos=%d fts_rank=%.8f", candidate.lexical.ChannelOrdinal, candidate.lexical.Rank))
	}
	if candidate.dense != nil {
		parts = append(parts, fmt.Sprintf("dense_pos=%d dense_distance=%.8f", candidate.dense.ChannelOrdinal, candidate.dense.Rank))
	}
	parts = append(parts, fmt.Sprintf("policy=%.8f final=%.8f", candidate.policyScore, candidate.finalScore))
	for _, feature := range candidate.features {
		parts = append(parts, fmt.Sprintf("feature:%s=%.6f*%.6f", feature.Name, feature.Value, feature.Weight))
	}
	return strings.Join(parts, " ")
}

func addSuppression(trace *RetrievalTrace, pageID pages.PageID, reason string) {
	trace.Suppressions = append(trace.Suppressions, SuppressionTrace{PageID: pageID, Reason: safeSuppressionReason(reason)})
}

func safeSuppressionReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "authority", "scope", "branch", "status", "stale", "invalidation", "invalidated", "trust", "fault", "budget", "recheck", "recheck_error", "channel_contract", "tuple_mismatch", "kind_cap", "source_cap", "near_duplicate", "dense_distance", "min_score":
		return strings.ToLower(strings.TrimSpace(reason))
	default:
		return "recheck"
	}
}

func denseFailureMarker(err error) string {
	switch {
	case errors.Is(err, indexer.ErrDenseUnavailable):
		return "dense=unavailable"
	case errors.Is(err, indexer.ErrEmbeddingNamespaceMismatch):
		return "dense=namespace"
	case errors.Is(err, pages.ErrInvalidVector):
		return "dense=query-vector"
	default:
		return "dense=search"
	}
}

func stableMarkers(markers []string) []string {
	set := make(map[string]struct{}, len(markers))
	for _, marker := range markers {
		if marker != "" {
			set[marker] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for marker := range set {
		out = append(out, marker)
	}
	sort.Strings(out)
	return out
}

func hybridExplanation(intent QueryIntent, trace RetrievalTrace, selected int) string {
	mode := intent.Mode
	if mode == "" {
		mode = ModeSemanticFault
	}
	return fmt.Sprintf("mode=%s fusion=%s policy=%s/%s provisional=%t lexical=%d dense=%d selected=%d suppressions=%d degraded=%s abstention=%s",
		mode, trace.FusionVersion, trace.PolicyName, trace.PolicyVersion, trace.PolicyProvisional,
		trace.LexicalCandidates, trace.DenseCandidates, selected, trace.SuppressionCount,
		strings.Join(trace.Degraded, ","), trace.Abstention)
}

func operationalError(category string) error {
	return fmt.Errorf("%w: %s", ErrIndexUnavailable, category)
}

func authorityUnavailableError() error {
	return fmt.Errorf("%w: %w", ErrIndexUnavailable, ErrAuthorityUnavailable)
}

func hybridModeRequiresEligibility(mode Mode) bool {
	return mode == "" || mode == ModeSemanticFault || mode == ModePrefetch
}

func operationalResult(intent QueryIntent, trace RetrievalTrace) Result {
	return Result{
		Explanation: hybridExplanation(intent, trace, 0),
		Degraded:    append([]string(nil), trace.Degraded...),
		Trace:       trace,
	}
}

func abstentionReason(trace RetrievalTrace) string {
	if trace.LexicalCandidates+trace.DenseCandidates == 0 {
		return "no_candidates"
	}
	for _, suppression := range trace.Suppressions {
		switch suppression.Reason {
		case "min_score", "dense_distance":
			return "threshold"
		case "authority", "scope", "status", "stale", "invalidation", "invalidated", "trust", "fault", "recheck":
			return "authority"
		}
	}
	return "diversity"
}
