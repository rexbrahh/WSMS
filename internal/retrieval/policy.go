package retrieval

import (
	"fmt"
	"math"
	"strings"
	"unicode"

	"wsms/internal/pages"
)

// HybridPolicyWeights names every nonzero deterministic policy contribution.
// Trust classes are separate weights so the profile fully describes source
// preference instead of hiding it in code.
type HybridPolicyWeights struct {
	RepoAffinity   float64
	TaskAffinity   float64
	BranchAffinity float64
	CommitAffinity float64
	PathAffinity   float64
	TrustUser      float64
	TrustRepo      float64
	TrustSystem    float64
	TrustTool      float64
	TrustModel     float64
	TrustMixed     float64
	Salience       float64
	Verification   float64
	FailureOverlap float64
}

// HybridPolicyProfile is a complete, named deterministic rerank/diversity and
// abstention profile. The checked-in default is explicitly provisional: it has
// not been calibrated on a real Qwen sidecar or held-out forced-reset corpus.
type HybridPolicyProfile struct {
	Name                   string
	Version                string
	Provisional            bool
	RRFK                   float64
	MaxCandidates          int
	MaxSelected            int
	MaxPerKind             int
	MaxPerSource           int
	MaxDenseDistance       float64
	MinScore               float64
	NearDuplicateThreshold float64
	Weights                HybridPolicyWeights
}

// DefaultHybridPolicyProfile returns the checked-in provisional Phase 7E
// profile. Its values are safety defaults, not Qwen-quality claims.
func DefaultHybridPolicyProfile() HybridPolicyProfile {
	return HybridPolicyProfile{
		Name: "working-set", Version: "v1-provisional", Provisional: true,
		RRFK: defaultRRFK, MaxCandidates: 50, MaxSelected: 3,
		MaxPerKind: 2, MaxPerSource: 1, MaxDenseDistance: 0.35,
		MinScore: 0.018, NearDuplicateThreshold: 0.86,
		Weights: HybridPolicyWeights{
			RepoAffinity: 0.004, TaskAffinity: 0.006, BranchAffinity: 0.005,
			CommitAffinity: 0.004, PathAffinity: 0.006,
			TrustUser: 0.006, TrustRepo: 0.005, TrustSystem: 0.005,
			TrustTool: 0.004, TrustModel: 0.001, TrustMixed: 0.002,
			Salience: 0.006, Verification: 0.004, FailureOverlap: 0.010,
		},
	}
}

func (p HybridPolicyProfile) validate() error {
	if !safeProfileToken(p.Name) || !safeProfileToken(p.Version) {
		return fmt.Errorf("invalid hybrid policy identity")
	}
	if !finiteRange(p.RRFK, 1, 10_000) || p.MaxCandidates <= 0 || p.MaxCandidates > maxIntentCandidateLimit ||
		p.MaxSelected <= 0 || p.MaxSelected > maxIntentMaterializeLimit || p.MaxPerKind <= 0 || p.MaxPerKind > p.MaxSelected ||
		p.MaxPerSource <= 0 || p.MaxPerSource > p.MaxSelected || !finiteRange(p.MaxDenseDistance, 0, 2) ||
		!finiteRange(p.MinScore, 0, 2) || !finiteRange(p.NearDuplicateThreshold, 0, 1) {
		return fmt.Errorf("invalid hybrid policy bounds")
	}
	for _, weight := range p.weightValues() {
		if !finiteRange(weight, 0, 1) {
			return fmt.Errorf("invalid hybrid policy weight")
		}
	}
	return nil
}

func safeProfileToken(value string) bool {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' || r == '/' {
			continue
		}
		return false
	}
	return true
}

func (p HybridPolicyProfile) weightValues() []float64 {
	w := p.Weights
	return []float64{w.RepoAffinity, w.TaskAffinity, w.BranchAffinity, w.CommitAffinity, w.PathAffinity,
		w.TrustUser, w.TrustRepo, w.TrustSystem, w.TrustTool, w.TrustModel, w.TrustMixed,
		w.Salience, w.Verification, w.FailureOverlap}
}

func finiteRange(value, min, max float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= min && value <= max
}

func scorePolicy(profile HybridPolicyProfile, page pages.WarmPage, intent QueryIntent) (float64, []PolicyFeatureTrace) {
	features := make([]PolicyFeatureTrace, 0, 9)
	add := func(name string, value, weight float64) {
		if value <= 0 || weight <= 0 {
			return
		}
		features = append(features, PolicyFeatureTrace{Name: name, Value: value, Weight: weight, Contribution: value * weight})
	}
	if intent.RepoID != "" && page.RepoID == intent.RepoID {
		add("repo_affinity", 1, profile.Weights.RepoAffinity)
	}
	if intent.TaskID != "" && page.TaskID == intent.TaskID {
		add("task_affinity", 1, profile.Weights.TaskAffinity)
	}
	if intent.Branch != "" && page.Branch == intent.Branch {
		add("branch_affinity", 1, profile.Weights.BranchAffinity)
	}
	if intent.Commit != "" && page.Commit == intent.Commit {
		add("commit_affinity", 1, profile.Weights.CommitAffinity)
	}
	if len(intent.PathHints) > 0 && pathAffinity(page, normalizeHints(intent.PathHints)) {
		add("path_affinity", 1, profile.Weights.PathAffinity)
	}
	switch page.Trust {
	case pages.TrustUser:
		add("trust_user", 1, profile.Weights.TrustUser)
	case pages.TrustRepo:
		add("trust_repo", 1, profile.Weights.TrustRepo)
	case pages.TrustSystem:
		add("trust_system", 1, profile.Weights.TrustSystem)
	case pages.TrustTool:
		add("trust_tool", 1, profile.Weights.TrustTool)
	case pages.TrustModel:
		add("trust_model", 1, profile.Weights.TrustModel)
	case pages.TrustMixed:
		add("trust_mixed", 1, profile.Weights.TrustMixed)
	}
	add("salience", clamp01(page.Salience), profile.Weights.Salience)
	if !page.LastVerifiedAt.IsZero() {
		add("verification", 1, profile.Weights.Verification)
	}
	add("failure_overlap", failureOverlap(intent.LastFailure, page.SearchText+" "+page.Summary), profile.Weights.FailureOverlap)
	var score float64
	for _, feature := range features {
		score += feature.Contribution
	}
	return score, features
}

func clamp01(value float64) float64 {
	if value < 0 || math.IsNaN(value) {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func failureOverlap(failure, text string) float64 {
	left, right := semanticTokenSet(failure), semanticTokenSet(text)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	var overlap int
	for token := range left {
		if _, ok := right[token]; ok {
			overlap++
		}
	}
	return float64(overlap) / float64(len(left))
}

func nearDuplicate(page pages.WarmPage, selected []pages.WarmPage, threshold float64) bool {
	if threshold <= 0 {
		return false
	}
	want := semanticTokenSet(page.SearchText + " " + page.Summary)
	if len(want) == 0 {
		return false
	}
	for _, other := range selected {
		if jaccard(want, semanticTokenSet(other.SearchText+" "+other.Summary)) >= threshold {
			return true
		}
	}
	return false
}

func semanticTokenSet(text string) map[string]struct{} {
	set := make(map[string]struct{})
	var b strings.Builder
	flush := func() {
		if b.Len() < 2 {
			b.Reset()
			return
		}
		set[strings.ToLower(b.String())] = struct{}{}
		b.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == '/' {
			b.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
		if len(set) >= 128 {
			break
		}
	}
	flush()
	return set
}

func jaccard(left, right map[string]struct{}) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	intersection := 0
	for token := range left {
		if _, ok := right[token]; ok {
			intersection++
		}
	}
	union := len(left) + len(right) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func sourceCapReached(page pages.WarmPage, counts map[string]int, cap int) bool {
	for _, key := range sourceKeys(page) {
		if counts[key] >= cap {
			return true
		}
	}
	return false
}

func incrementSourceCounts(page pages.WarmPage, counts map[string]int) {
	for _, key := range sourceKeys(page) {
		counts[key]++
	}
}

func sourceKeys(page pages.WarmPage) []string {
	set := make(map[string]struct{})
	for _, ref := range page.Refs {
		set[ref.Address()] = struct{}{}
	}
	if len(set) == 0 {
		return []string{"page:" + string(page.ID)}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	return keys
}
