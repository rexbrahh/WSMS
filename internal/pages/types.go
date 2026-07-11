// Package pages defines the deterministic, disposable semantic-page layer.
//
// Pages are compiled from durable ledger evidence and current WSL state. They
// are search inputs and page-table metadata, never a replacement for either
// source of truth.
package pages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"wsms/internal/ledger"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

const (
	// MaxSearchTokens is the Phase 7A upper bound for canonical search text.
	// The compiler targets 100-400 tokens when the evidence supports it, but it
	// never pads sparse evidence with invented prose.
	MaxSearchTokens = 400
	// MaxSummaryTokens keeps the inspectable synopsis smaller than its search
	// representation.
	MaxSummaryTokens = 100
	// MaxPageRefs prevents a broad event from becoming an unbounded transcript.
	MaxPageRefs = 16
	// MaxFileSliceLines bounds exact source materialization independently of
	// semantic text budgets.
	MaxFileSliceLines = 512
	// MaxFileSliceBytes bounds a capability implementation that returns a
	// pathological slice despite the line limit.
	MaxFileSliceBytes = 256 * 1024
	// MaxSearchBytes and MaxSummaryBytes close the unbroken-token bypass around
	// token-only limits.
	MaxSearchBytes  = 16 * 1024
	MaxSummaryBytes = 4 * 1024
	MaxReasonBytes  = 512
)

var (
	// ErrInvalidPage reports malformed or internally inconsistent page data.
	ErrInvalidPage = errors.New("invalid warm page")
	// ErrUnmaterializableRef reports evidence that cannot be resolved from L4 or
	// the current WSL mapping.
	ErrUnmaterializableRef = errors.New("unmaterializable page reference")
	// ErrInvalidVector reports malformed exact-oracle input.
	ErrInvalidVector = errors.New("invalid vector")
)

var (
	warmPageIDRE = regexp.MustCompile(`^wp_[0-9a-f]{32}$`)
	logicalIDRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,127}$`)
)

// PageID is a stable logical page address.
type PageID string

// PageVersion is the monotonic version of one logical page. The deterministic
// compiler uses the source ledger sequence; versions may have gaps.
type PageVersion uint64

// PageKind is the controlled retrieval unit taxonomy.
type PageKind string

const (
	KindFailureEpisode PageKind = "failure_episode"
	KindDecision       PageKind = "decision"
	KindConstraint     PageKind = "constraint"
	KindTaskCheckpoint PageKind = "task_checkpoint"
	KindKnownGood      PageKind = "known_good_command"
	KindRepoFact       PageKind = "repo_fact"
	KindFileContext    PageKind = "file_context"
)

// Trust preserves the authority of the evidence that produced a page.
type Trust string

const (
	TrustUser   Trust = "user"
	TrustRepo   Trust = "repo"
	TrustSystem Trust = "system"
	TrustTool   Trust = "tool"
	TrustModel  Trust = "model"
	TrustMixed  Trust = "mixed"
)

// Status is the current coherence state of a logical page.
type Status string

const (
	StatusActive      Status = "active"
	StatusStale       Status = "stale"
	StatusInvalidated Status = "invalidated"
)

// MutationOp describes an idempotent logical-page change.
type MutationOp string

const (
	MutationUpsert     MutationOp = "upsert"
	MutationInvalidate MutationOp = "invalidate"
)

// CompilerVersion identifies all canonicalization and extraction rules.
type CompilerVersion string

// CurrentCompilerVersion is changed whenever the logical page representation
// or deterministic extraction rules change incompatibly.
const CurrentCompilerVersion CompilerVersion = "pages/v0.2.0"

// SourceDigest is a lowercase SHA-256 digest of canonical evidence inputs.
type SourceDigest string

// ScopeEpoch invalidates an otherwise matching page when its authority scope
// changes. Epoch zero is the initial scope generation.
type ScopeEpoch uint64

// RefKind distinguishes exact address spaces; similarity never rewrites one
// kind into another.
type RefKind string

const (
	RefWSLRecord RefKind = "wsl_record"
	RefEvent     RefKind = "event"
	RefArtifact  RefKind = "artifact"
	RefFileSlice RefKind = "file_slice"
)

// PageRef is an exact, typed address into current WSL or L4 evidence.
type PageRef struct {
	Kind      RefKind `json:"kind"`
	ID        string  `json:"id,omitempty"`
	Path      string  `json:"path,omitempty"`
	Commit    string  `json:"commit,omitempty"`
	StartLine int     `json:"start_line,omitempty"`
	EndLine   int     `json:"end_line,omitempty"`
}

// Address returns a stable inspectable form for sorting and diagnostics.
func (r PageRef) Address() string {
	switch r.Kind {
	case RefWSLRecord:
		return "wsl:" + r.ID
	case RefEvent:
		return "event:" + r.ID
	case RefArtifact:
		return "artifact:sha256:" + r.ID
	case RefFileSlice:
		return fmt.Sprintf("file:%s@%s:%d-%d", r.Path, r.Commit, r.StartLine, r.EndLine)
	default:
		return string(r.Kind) + ":" + r.ID
	}
}

// WarmPage is the logical, backend-independent semantic page.
type WarmPage struct {
	ID              PageID          `json:"page_id"`
	Version         PageVersion     `json:"page_version"`
	SessionID       string          `json:"session_id"`
	RepoID          string          `json:"repo_id,omitempty"`
	TaskID          string          `json:"task_id,omitempty"`
	Branch          string          `json:"branch,omitempty"`
	Commit          string          `json:"commit,omitempty"`
	PathScope       []string        `json:"path_scope,omitempty"`
	Scope           types.Scope     `json:"scope"`
	Kind            PageKind        `json:"kind"`
	Trust           Trust           `json:"trust"`
	Status          Status          `json:"status"`
	Salience        float64         `json:"salience"`
	SalienceReason  string          `json:"salience_reason"`
	SearchText      string          `json:"search_text"`
	Summary         string          `json:"summary"`
	Refs            []PageRef       `json:"refs"`
	SourceSeqMin    int64           `json:"source_seq_min"`
	SourceSeqMax    int64           `json:"source_seq_max"`
	SourceDigest    SourceDigest    `json:"source_digest"`
	CompilerVersion CompilerVersion `json:"compiler_version"`
	ScopeEpoch      ScopeEpoch      `json:"scope_epoch"`
	CreatedAt       time.Time       `json:"created_at"`
	LastVerifiedAt  time.Time       `json:"last_verified_at,omitempty"`
}

// PageMutation is the atomic unit consumed by later index generations.
type PageMutation struct {
	Op   MutationOp `json:"op"`
	Page WarmPage   `json:"page"`
}

// EventReader is satisfied by ledger.AppendOnlyLedger.
type EventReader interface {
	Get(context.Context, string) (ledger.Event, error)
}

// ArtifactReader is satisfied by artifacts.Store. Verification is streaming:
// raw artifact bytes never enter a WarmPage or the compiler's semantic text.
type ArtifactReader interface {
	VerifyArtifact(context.Context, string) error
}

// FileSliceReader resolves a commit-pinned repository slice without granting
// the compiler ambient filesystem access.
type FileSliceReader interface {
	ReadFileSlice(context.Context, string, string, int, int) ([]byte, error)
}

// CoherenceReader supplies the authoritative page-table generation and
// transitive ref eligibility without importing the coherence implementation.
// Production materialization requires it; offline compilation may omit it and
// use the explicit ScopeEpoch fixture value.
type CoherenceReader interface {
	DescriptorGeneration(types.Scope, string, string, []string) uint64
	PageDescriptorEligible(string, []string, types.Scope, string, string, []string, string, uint64) bool
}

// LedgerChange is one committed ledger event plus the fully derived WSL view
// after that event. Events and Artifacts are exact-evidence validation seams.
type LedgerChange struct {
	Event      ledger.Event
	State      *wsl.WorkingState
	Events     EventReader
	Artifacts  ArtifactReader
	Files      FileSliceReader
	Coherence  CoherenceReader
	RepoID     string
	TaskID     string
	Branch     string
	Commit     string
	ScopeEpoch ScopeEpoch
}

// PageCompiler is the deterministic logical-page compiler contract.
type PageCompiler interface {
	Compile(context.Context, LedgerChange) ([]PageMutation, error)
	Version() CompilerVersion
}

// Validate checks schema, bounds, and mutation/status consistency. Exact
// evidence availability is checked separately by the compiler.
func (m PageMutation) Validate() error {
	p := m.Page
	if !validMutation(m.Op) {
		return fmt.Errorf("%w: unknown mutation %q", ErrInvalidPage, m.Op)
	}
	if p.ID == "" || p.Version == 0 || p.SessionID == "" {
		return fmt.Errorf("%w: page id, positive version, and session are required", ErrInvalidPage)
	}
	if !warmPageIDRE.MatchString(string(p.ID)) || !validToken(p.SessionID, 256) {
		return fmt.Errorf("%w: malformed page or session id", ErrInvalidPage)
	}
	if !validStatus(p.Status) {
		return fmt.Errorf("%w: invalid status on %s", ErrInvalidPage, p.ID)
	}
	if m.Op == MutationInvalidate && p.Status != StatusInvalidated {
		return fmt.Errorf("%w: invalidate mutation %s must have invalidated status", ErrInvalidPage, p.ID)
	}
	if m.Op == MutationUpsert && p.Status == StatusInvalidated {
		return fmt.Errorf("%w: invalidated page %s requires invalidate mutation", ErrInvalidPage, p.ID)
	}
	if p.CompilerVersion == "" || p.Version > PageVersion(^uint64(0)>>1) || p.SourceSeqMin <= 0 || p.SourceSeqMax < p.SourceSeqMin || uint64(p.SourceSeqMax) > uint64(p.Version) {
		return fmt.Errorf("%w: compiler and valid source sequence range are required", ErrInvalidPage)
	}
	if !validDigest(string(p.SourceDigest)) {
		return fmt.Errorf("%w: malformed source digest on %s", ErrInvalidPage, p.ID)
	}
	if p.CreatedAt.IsZero() {
		return fmt.Errorf("%w: source-derived creation time is required", ErrInvalidPage)
	}
	if !p.LastVerifiedAt.IsZero() && p.LastVerifiedAt.Before(p.CreatedAt) {
		return fmt.Errorf("%w: verification predates creation on %s", ErrInvalidPage, p.ID)
	}
	if math.IsNaN(p.Salience) || math.IsInf(p.Salience, 0) || p.Salience < 0 || p.Salience > 1 {
		return fmt.Errorf("%w: salience outside [0,1] on %s", ErrInvalidPage, p.ID)
	}
	if strings.TrimSpace(p.SalienceReason) == "" || len(p.SalienceReason) > MaxReasonBytes || hasUnsafeText(p.SalienceReason) {
		return fmt.Errorf("%w: page %s requires a salience reason", ErrInvalidPage, p.ID)
	}
	if err := ValidateAuthorityDescriptor(p.Kind, p.Trust, p.Scope, p.RepoID, p.TaskID, p.Branch, p.Commit, p.PathScope, p.Refs); err != nil {
		return fmt.Errorf("%w: page %s: %v", ErrInvalidPage, p.ID, err)
	}
	if m.Op == MutationUpsert {
		if strings.TrimSpace(p.SearchText) == "" || strings.TrimSpace(p.Summary) == "" {
			return fmt.Errorf("%w: active/stale page %s requires search text and summary", ErrInvalidPage, p.ID)
		}
		if len(p.SearchText) > MaxSearchBytes || len(p.Summary) > MaxSummaryBytes || tokenCount(p.SearchText) > MaxSearchTokens || tokenCount(p.Summary) > MaxSummaryTokens {
			return fmt.Errorf("%w: page %s exceeds text bounds", ErrInvalidPage, p.ID)
		}
		if strings.Contains(p.SearchText, "artifact:sha256:") {
			return fmt.Errorf("%w: page %s leaks artifact addresses into semantic text", ErrInvalidPage, p.ID)
		}
		if hasUnsafeText(p.SearchText) || hasUnsafeText(p.Summary) {
			return fmt.Errorf("%w: page %s contains control text", ErrInvalidPage, p.ID)
		}
	}
	return nil
}

// ValidateAuthorityDescriptor validates the non-prose page-table fields used
// to decide whether a compiled page remains coherent. It is shared by page
// mutation admission and disposable index snapshot reconstruction so their
// authority rules cannot drift.
func ValidateAuthorityDescriptor(
	kind PageKind,
	trust Trust,
	scope types.Scope,
	repoID, taskID, branch, commit string,
	pathScope []string,
	refs []PageRef,
) error {
	if !validKind(kind) {
		return fmt.Errorf("unknown page kind %q", kind)
	}
	if !validTrust(trust) {
		return fmt.Errorf("unknown page trust %q", trust)
	}
	if !validTrustForKind(kind, trust) {
		return fmt.Errorf("trust %q cannot authorize kind %q", trust, kind)
	}
	if !validScope(scope) {
		return fmt.Errorf("unknown scope %q", scope)
	}
	page := WarmPage{
		Scope: scope, RepoID: repoID, TaskID: taskID, Branch: branch, Commit: commit,
		PathScope: pathScope,
	}
	if err := validatePageScope(page); err != nil {
		return err
	}
	if len(refs) == 0 || len(refs) > MaxPageRefs {
		return fmt.Errorf("descriptor has %d refs, expected 1..%d", len(refs), MaxPageRefs)
	}
	seen := make(map[string]bool, len(refs))
	previous := ""
	for i, ref := range refs {
		if err := ref.validate(); err != nil {
			return err
		}
		address := ref.Address()
		if seen[address] {
			return fmt.Errorf("duplicate ref %s", address)
		}
		if i > 0 && address < previous {
			return fmt.Errorf("refs are not canonically sorted")
		}
		seen[address] = true
		previous = address
	}
	return nil
}

func (r PageRef) validate() error {
	switch r.Kind {
	case RefWSLRecord, RefEvent:
		if !logicalIDRE.MatchString(r.ID) || r.Path != "" || r.Commit != "" || r.StartLine != 0 || r.EndLine != 0 {
			return fmt.Errorf("malformed %s ref", r.Kind)
		}
	case RefArtifact:
		if !validDigest(r.ID) || r.Path != "" || r.Commit != "" || r.StartLine != 0 || r.EndLine != 0 {
			return fmt.Errorf("malformed artifact ref")
		}
	case RefFileSlice:
		cleaned, ok := normalizeRepoPath(r.Path)
		if !ok || cleaned != r.Path || !validToken(r.Commit, 256) || r.StartLine <= 0 || r.EndLine < r.StartLine || r.EndLine-r.StartLine+1 > MaxFileSliceLines || r.ID != "" {
			return fmt.Errorf("malformed file-slice ref")
		}
	default:
		return fmt.Errorf("unknown ref kind %q", r.Kind)
	}
	return nil
}

func stablePageID(sessionID string, kind PageKind, anchor string) PageID {
	sum := sha256.Sum256([]byte("wsms-page-v1\x00" + sessionID + "\x00" + string(kind) + "\x00" + anchor))
	return PageID("wp_" + hex.EncodeToString(sum[:16]))
}

func sortRefs(refs []PageRef) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Address() < refs[j].Address() })
}

func validDigest(digest string) bool {
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func normalizeRepoPath(value string) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	cleaned, err := ledger.NormalizeRepoPath(value)
	if err != nil {
		return "", false
	}
	return cleaned, true
}

func validKind(value PageKind) bool {
	switch value {
	case KindFailureEpisode, KindDecision, KindConstraint, KindTaskCheckpoint, KindKnownGood, KindRepoFact, KindFileContext:
		return true
	default:
		return false
	}
}

func validTrust(value Trust) bool {
	switch value {
	case TrustUser, TrustRepo, TrustSystem, TrustTool, TrustModel, TrustMixed:
		return true
	default:
		return false
	}
}

func validTrustForKind(kind PageKind, trust Trust) bool {
	switch kind {
	case KindFailureEpisode, KindKnownGood:
		return trust == TrustTool
	case KindConstraint:
		return trust == TrustUser || trust == TrustRepo || trust == TrustSystem
	case KindDecision:
		return trust == TrustUser || trust == TrustSystem || trust == TrustModel || trust == TrustMixed
	case KindTaskCheckpoint:
		return trust == TrustUser || trust == TrustSystem || trust == TrustMixed
	case KindRepoFact, KindFileContext:
		return trust == TrustRepo || trust == TrustSystem || trust == TrustTool
	default:
		return false
	}
}

func validStatus(value Status) bool {
	switch value {
	case StatusActive, StatusStale, StatusInvalidated:
		return true
	default:
		return false
	}
}

func validMutation(value MutationOp) bool {
	return value == MutationUpsert || value == MutationInvalidate
}

func validScope(value types.Scope) bool {
	switch value {
	case types.ScopeGlobal, types.ScopeRepo, types.ScopeBranch, types.ScopeTask, types.ScopeFile:
		return true
	default:
		return false
	}
}

func tokenCount(text string) int { return len(strings.Fields(text)) }

func validatePageScope(p WarmPage) error {
	for _, value := range []string{p.RepoID, p.TaskID, p.Branch, p.Commit} {
		if value != "" && !validToken(value, 256) {
			return fmt.Errorf("scope token is malformed")
		}
	}
	if len(p.PathScope) > MaxPageRefs {
		return fmt.Errorf("too many path-scope entries")
	}
	seen := map[string]bool{}
	for _, candidate := range p.PathScope {
		cleaned, ok := normalizeRepoPath(candidate)
		if !ok || cleaned != candidate || seen[candidate] {
			return fmt.Errorf("path scope is noncanonical or duplicated")
		}
		seen[candidate] = true
	}
	switch p.Scope {
	case types.ScopeGlobal:
		if p.RepoID != "" || p.TaskID != "" || p.Branch != "" || p.Commit != "" || len(p.PathScope) != 0 {
			return fmt.Errorf("global scope must not carry narrower authority")
		}
	case types.ScopeRepo:
		if p.RepoID == "" || p.TaskID != "" || p.Branch != "" || p.Commit != "" || len(p.PathScope) != 0 {
			return fmt.Errorf("repo scope requires only repo identity")
		}
	case types.ScopeTask:
		if p.RepoID == "" || p.TaskID == "" || p.Branch != "" || p.Commit != "" || len(p.PathScope) != 0 {
			return fmt.Errorf("task scope requires repo/task identity without branch/path authority")
		}
	case types.ScopeBranch:
		if p.RepoID == "" || p.Branch == "" || len(p.PathScope) != 0 {
			return fmt.Errorf("branch scope requires repo/branch identity and no path authority")
		}
	case types.ScopeFile:
		if p.RepoID == "" || p.Branch == "" || p.Commit == "" || len(p.PathScope) == 0 {
			return fmt.Errorf("file scope requires repo, branch, commit, and paths")
		}
	}
	return nil
}

func validToken(value string, limit int) bool {
	if value == "" || len(value) > limit || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func hasUnsafeText(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
