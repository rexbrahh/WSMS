package pages

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"wsms/internal/artifacts"
	wsmserrors "wsms/internal/errors"
	"wsms/internal/ledger"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

// DeterministicCompiler derives bounded logical pages from one durable event
// and its post-event WSL view. It has no clock, model, network, or ambient
// filesystem dependency.
type DeterministicCompiler struct{}

// NewDeterministicCompiler constructs the Phase 7A reference compiler.
func NewDeterministicCompiler() *DeterministicCompiler { return &DeterministicCompiler{} }

// Version returns the canonicalization contract used by this compiler.
func (*DeterministicCompiler) Version() CompilerVersion { return CurrentCompilerVersion }

// Compile emits deterministic, evidence-materializable mutations for change.
func (c *DeterministicCompiler) Compile(ctx context.Context, change LedgerChange) ([]PageMutation, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: nil compiler", ErrInvalidPage)
	}
	if err := validateLedgerChange(ctx, change); err != nil {
		return nil, err
	}
	authority := authorityFrom(change)
	derived := recordsDerivedFrom(change.State, change.Event.ID)
	var (
		mutations []PageMutation
		err       error
	)
	appendMutation := func(mutation *PageMutation, compileErr error) {
		if err != nil {
			return
		}
		if compileErr != nil {
			err = compileErr
			return
		}
		if mutation != nil {
			mutations = append(mutations, *mutation)
		}
	}

	switch change.Event.Type {
	case ledger.EventTaskStarted, ledger.EventNextAction:
		mutation, compileErr := c.compileCheckpoint(ctx, change, authority)
		appendMutation(mutation, compileErr)
	case ledger.EventUserInstruction, ledger.EventHumanCorrection:
		for _, record := range derived {
			if constraint, ok := record.(*wsl.ConstraintRecord); ok {
				mutation, compileErr := c.compileConstraint(ctx, change, authority, constraint)
				appendMutation(mutation, compileErr)
			}
		}
	case ledger.EventCommandOutput, ledger.EventTestResult, ledger.EventToolResult:
		if change.Event.PayloadInt("exit", 0) == 0 {
			mutation, compileErr := c.compileKnownGood(ctx, change, authority)
			appendMutation(mutation, compileErr)
		} else {
			for _, record := range derived {
				if failure, ok := record.(*wsl.FailureRecord); ok {
					mutation, compileErr := c.compileFailure(ctx, change, authority, failure)
					appendMutation(mutation, compileErr)
				}
			}
		}
	case ledger.EventDecision:
		for _, record := range derived {
			if decision, ok := record.(*wsl.DecisionRecord); ok {
				mutation, compileErr := c.compileDecision(ctx, change, authority, decision, derived)
				appendMutation(mutation, compileErr)
			}
		}
	case ledger.EventFileSnapshot:
		for _, kind := range []PageKind{KindFileContext, KindRepoFact} {
			mutation, compileErr := c.compileFileEvidence(ctx, change, authority, kind)
			appendMutation(mutation, compileErr)
		}
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(mutations, func(i, j int) bool {
		if mutations[i].Page.ID == mutations[j].Page.ID {
			return mutations[i].Page.Version < mutations[j].Page.Version
		}
		return mutations[i].Page.ID < mutations[j].Page.ID
	})
	for i := range mutations {
		if i > 0 && mutations[i-1].Page.ID == mutations[i].Page.ID && mutations[i-1].Page.Version == mutations[i].Page.Version {
			return nil, fmt.Errorf("%w: duplicate mutation %s version %d", ErrInvalidPage, mutations[i].Page.ID, mutations[i].Page.Version)
		}
		if validateErr := mutations[i].Validate(); validateErr != nil {
			return nil, validateErr
		}
	}
	return mutations, nil
}

// ValidateMaterializable rechecks a compiled page against current authority
// and exact evidence. L3 callers must run this after candidate selection and
// before admitting anything to L2; indexed text is never served as truth.
func ValidateMaterializable(ctx context.Context, page WarmPage, change LedgerChange) error {
	op := MutationUpsert
	if page.Status == StatusInvalidated {
		op = MutationInvalidate
	}
	if err := (PageMutation{Op: op, Page: page}).Validate(); err != nil {
		return err
	}
	if page.Status != StatusActive {
		return fmt.Errorf("%w: page %s is %s", ErrUnmaterializableRef, page.ID, page.Status)
	}
	if err := validateLedgerChange(ctx, change); err != nil {
		return err
	}
	if page.SessionID != change.Event.SessionID {
		return fmt.Errorf("%w: page %s crossed session", ErrUnmaterializableRef, page.ID)
	}
	if page.CompilerVersion != CurrentCompilerVersion {
		return fmt.Errorf("%w: page %s compiler %q is not current %q", ErrUnmaterializableRef, page.ID, page.CompilerVersion, CurrentCompilerVersion)
	}
	if change.Coherence == nil {
		return fmt.Errorf("%w: page %s requires authoritative coherence", ErrUnmaterializableRef, page.ID)
	}
	currentEpoch := ScopeEpoch(change.Coherence.DescriptorGeneration(page.Scope, page.Branch, page.Commit, page.PathScope))
	if page.ScopeEpoch != currentEpoch {
		return fmt.Errorf("%w: page %s scope epoch %d is not current %d", ErrUnmaterializableRef, page.ID, page.ScopeEpoch, currentEpoch)
	}
	if uint64(change.Event.Seq) < uint64(page.Version) {
		return fmt.Errorf("%w: page %s version is from the future", ErrUnmaterializableRef, page.ID)
	}
	if !authorityMatchesPage(page, authorityFrom(change)) {
		return fmt.Errorf("%w: page %s is outside current authority", ErrUnmaterializableRef, page.ID)
	}
	if !change.Coherence.PageDescriptorEligible(
		string(page.ID), logicalCoherenceRefs(page.Refs), page.Scope, page.Branch,
		page.Commit, page.PathScope, string(page.SourceDigest), uint64(page.ScopeEpoch),
	) {
		return fmt.Errorf("%w: page %s or one of its refs is not eligible", ErrUnmaterializableRef, page.ID)
	}
	digest, sourceMin, sourceMax, createdAt, err := sourceDigest(ctx, change, page)
	if err != nil {
		return err
	}
	if digest != page.SourceDigest || sourceMin != page.SourceSeqMin || sourceMax != page.SourceSeqMax || !createdAt.Equal(page.CreatedAt) {
		return fmt.Errorf("%w: page %s evidence digest or replay range changed", ErrUnmaterializableRef, page.ID)
	}
	return nil
}

type compilerAuthority struct {
	repo   string
	task   string
	branch string
	commit string
}

type pageSpec struct {
	kind           PageKind
	anchor         string
	scope          types.Scope
	trust          Trust
	pathScope      []string
	refs           []PageRef
	search         []textField
	summary        []textField
	salience       float64
	salienceReason string
	verified       bool
}

type textField struct {
	name  string
	value string
}

func (c *DeterministicCompiler) compileCheckpoint(ctx context.Context, change LedgerChange, authority compilerAuthority) (*PageMutation, error) {
	task := change.State.ActiveTask()
	if task == nil || task.IDValue == "" {
		return nil, nil
	}
	refs := []PageRef{{Kind: RefWSLRecord, ID: task.IDValue}}
	search := []textField{
		{name: "kind", value: string(KindTaskCheckpoint)},
		{name: "goal", value: task.Goal},
		{name: "phase", value: task.Phase},
		{name: "priority", value: string(task.Priority)},
		{name: "branch", value: task.Branch},
		{name: "commit", value: task.Commit},
		{name: "dirty", value: task.Dirty},
	}
	if next := change.State.Next(); next != nil {
		refs = append(refs, PageRef{Kind: RefWSLRecord, ID: next.ID()})
		search = append(search,
			textField{name: "next_action", value: next.Action},
			textField{name: "next_target", value: next.Target},
			textField{name: "next_question", value: next.Question},
		)
	}
	return c.build(ctx, change, authority, pageSpec{
		kind: KindTaskCheckpoint, anchor: task.IDValue, scope: types.ScopeTask,
		trust: TrustMixed, refs: refs, search: search,
		summary:  []textField{{name: "task", value: task.Goal}, {name: "next", value: nextSummary(change.State.Next())}},
		salience: 0.85, salienceReason: "active task checkpoint",
	})
}

func (c *DeterministicCompiler) compileConstraint(ctx context.Context, change LedgerChange, authority compilerAuthority, record *wsl.ConstraintRecord) (*PageMutation, error) {
	if record == nil || record.Text == "" {
		return nil, nil
	}
	trust, ok := constraintTrust(record.Source)
	if !ok {
		return nil, fmt.Errorf("%w: constraint %s has unsupported source %q", ErrInvalidPage, record.IDValue, record.Source)
	}
	salience := 0.75
	if record.Strength == types.StrengthHard {
		salience = 0.98
	}
	return c.build(ctx, change, authority, pageSpec{
		kind: KindConstraint, anchor: record.IDValue, scope: defaultScope(record.Scope), trust: trust,
		refs: []PageRef{{Kind: RefWSLRecord, ID: record.IDValue}},
		search: []textField{
			{name: "kind", value: string(KindConstraint)}, {name: "strength", value: string(record.Strength)},
			{name: "requirement", value: record.Text}, {name: "source", value: string(record.Source)},
		},
		summary:  []textField{{name: string(record.Strength) + " constraint", value: record.Text}},
		salience: salience, salienceReason: "explicit scoped constraint",
	})
}

func (c *DeterministicCompiler) compileFailure(ctx context.Context, change LedgerChange, authority compilerAuthority, record *wsl.FailureRecord) (*PageMutation, error) {
	if record == nil {
		return nil, nil
	}
	paths := pathFromFileHint(record.FileHint)
	scope := types.ScopeBranch
	if len(paths) > 0 && authority.repo != "" && authority.branch != "" && authority.commit != "" {
		scope = types.ScopeFile
	}
	return c.build(ctx, change, authority, pageSpec{
		kind: KindFailureEpisode, anchor: record.IDValue, scope: scope, trust: TrustTool,
		pathScope: paths, refs: []PageRef{{Kind: RefWSLRecord, ID: record.IDValue}},
		search: []textField{
			{name: "kind", value: string(KindFailureEpisode)}, {name: "command", value: record.Cmd},
			{name: "exit", value: strconv.Itoa(record.Exit)}, {name: "error", value: record.Err},
			{name: "file", value: record.FileHint},
		},
		summary:  []textField{{name: "failure", value: record.Err}, {name: "command", value: record.Cmd}},
		salience: 0.92, salienceReason: "non-zero verified tool result",
	})
}

func (c *DeterministicCompiler) compileKnownGood(ctx context.Context, change LedgerChange, authority compilerAuthority) (*PageMutation, error) {
	command := firstNonEmpty(change.Event.PayloadString("cmd"), change.Event.PayloadString("command"))
	if command == "" {
		return nil, nil
	}
	scope := types.ScopeBranch
	if authority.branch == "" {
		if authority.task == "" {
			return nil, nil
		}
		scope = types.ScopeTask
	}
	anchor := authority.repo + "\x00" + authority.branch + "\x00" + command
	return c.build(ctx, change, authority, pageSpec{
		kind: KindKnownGood, anchor: anchor, scope: scope, trust: TrustTool,
		search: []textField{
			{name: "kind", value: string(KindKnownGood)}, {name: "command", value: command},
			{name: "exit", value: "0"}, {name: "file", value: change.Event.PayloadString("file_hint")},
		},
		summary:  []textField{{name: "verified command", value: command}},
		salience: 0.82, salienceReason: "zero-exit verified command", verified: true,
	})
}

func (c *DeterministicCompiler) compileDecision(ctx context.Context, change LedgerChange, authority compilerAuthority, record *wsl.DecisionRecord, derived []wsl.Record) (*PageMutation, error) {
	if record == nil || record.Chosen == "" {
		return nil, nil
	}
	refs := []PageRef{{Kind: RefWSLRecord, ID: record.IDValue}}
	for _, logical := range parseLogicalAddresses(record.Refs) {
		refs = append(refs, logicalRef(change.State, logical))
	}
	search := []textField{
		{name: "kind", value: string(KindDecision)}, {name: "chosen", value: record.Chosen},
		{name: "because", value: record.Because}, {name: "scope", value: string(defaultScope(record.Scope))},
	}
	for _, candidate := range derived {
		avoid, ok := candidate.(*wsl.AvoidRecord)
		if !ok {
			continue
		}
		refs = append(refs, PageRef{Kind: RefWSLRecord, ID: avoid.IDValue})
		if avoid.Ref != "" {
			refs = append(refs, logicalRef(change.State, avoid.Ref))
		}
		search = append(search, textField{name: "avoid", value: avoid.Text})
	}
	scope := defaultScope(record.Scope)
	if scope == types.ScopeFile {
		return nil, nil // no exact path address exists in WSL v0 decision records
	}
	return c.build(ctx, change, authority, pageSpec{
		kind: KindDecision, anchor: record.IDValue, scope: scope, trust: TrustModel,
		refs: refs, search: search,
		summary:  []textField{{name: "decision", value: record.Chosen}, {name: "because", value: record.Because}},
		salience: 0.86, salienceReason: "explicit operational decision",
	})
}

func (c *DeterministicCompiler) compileFileEvidence(ctx context.Context, change LedgerChange, authority compilerAuthority, kind PageKind) (*PageMutation, error) {
	path := change.Event.PayloadString("path")
	digest := change.Event.PayloadString("content_digest")
	if path == "" || digest == "" || authority.repo == "" || authority.branch == "" || authority.commit == "" {
		return nil, nil
	}
	label := "file context"
	salience := 0.62
	if kind == KindRepoFact {
		label, salience = "repository fact", 0.68
	}
	return c.build(ctx, change, authority, pageSpec{
		kind: kind, anchor: authority.repo + "\x00" + path, scope: types.ScopeFile, trust: TrustRepo,
		pathScope: []string{path},
		search: []textField{
			{name: "kind", value: string(kind)}, {name: "path", value: path},
			{name: "commit", value: authority.commit}, {name: "content_digest", value: digest},
		},
		summary:  []textField{{name: label, value: path + " at " + authority.commit}},
		salience: salience, salienceReason: "commit-pinned file snapshot", verified: true,
	})
}

func (c *DeterministicCompiler) build(ctx context.Context, change LedgerChange, authority compilerAuthority, spec pageSpec) (*PageMutation, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if spec.anchor == "" {
		return nil, fmt.Errorf("%w: empty %s anchor", ErrInvalidPage, spec.kind)
	}
	paths, err := canonicalPaths(spec.pathScope)
	if err != nil {
		return nil, err
	}
	refs, err := expandRefs(change, spec.refs)
	if err != nil {
		return nil, err
	}
	page := WarmPage{
		ID: stablePageID(change.Event.SessionID, spec.kind, spec.anchor), Version: PageVersion(change.Event.Seq),
		SessionID: change.Event.SessionID, Scope: spec.scope, Kind: spec.kind, Trust: spec.trust, Status: StatusActive,
		PathScope: paths, Salience: spec.salience, SalienceReason: spec.salienceReason,
		SearchText: boundedText(spec.search, MaxSearchTokens, MaxSearchBytes),
		Summary:    boundedText(spec.summary, MaxSummaryTokens, MaxSummaryBytes), Refs: refs,
		CompilerVersion: c.Version(), ScopeEpoch: change.ScopeEpoch,
	}
	if !applyAuthority(&page, authority) {
		return nil, nil
	}
	if change.Coherence != nil {
		page.ScopeEpoch = ScopeEpoch(change.Coherence.DescriptorGeneration(page.Scope, page.Branch, page.Commit, page.PathScope))
	}
	if spec.verified {
		page.LastVerifiedAt = change.Event.TS.UTC()
	}
	digest, sourceMin, sourceMax, createdAt, err := sourceDigest(ctx, change, page)
	if err != nil {
		return nil, err
	}
	page.SourceSeqMin, page.SourceSeqMax = sourceMin, sourceMax
	page.SourceDigest, page.CreatedAt = digest, createdAt
	mutation := &PageMutation{Op: MutationUpsert, Page: page}
	if err := mutation.Validate(); err != nil {
		return nil, err
	}
	return mutation, nil
}

func validateLedgerChange(ctx context.Context, change LedgerChange) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	ev := change.Event
	if !logicalIDRE.MatchString(ev.ID) || ev.Seq <= 0 || !validToken(ev.SessionID, 256) || ev.TS.IsZero() {
		return fmt.Errorf("%w: durable event identity, sequence, session, and timestamp are required", ErrInvalidPage)
	}
	if err := ledger.ValidateEvent(ev); err != nil {
		return fmt.Errorf("%w: source event %s: %v", ErrInvalidPage, ev.ID, err)
	}
	if change.State == nil {
		return fmt.Errorf("%w: post-event WSL state is required", ErrInvalidPage)
	}
	for name, value := range map[string]string{
		"event repo": ev.Repo, "event task": ev.TaskID, "event branch": ev.Branch, "event commit": ev.Commit,
		"current repo": change.RepoID, "current task": change.TaskID, "current branch": change.Branch, "current commit": change.Commit,
	} {
		if value != "" && !validToken(value, 256) {
			return fmt.Errorf("%w: %s is malformed", ErrInvalidPage, name)
		}
	}
	for name, pair := range map[string][2]string{
		"repo": {change.RepoID, ev.Repo}, "task": {change.TaskID, ev.TaskID},
		"branch": {change.Branch, ev.Branch}, "commit": {change.Commit, ev.Commit},
	} {
		if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
			return fmt.Errorf("%w: current %s %q conflicts with event %s %q", ErrInvalidPage, name, pair[0], name, pair[1])
		}
	}
	return nil
}

func authorityFrom(change LedgerChange) compilerAuthority {
	authority := compilerAuthority{
		repo:   firstNonEmpty(change.RepoID, change.Event.Repo),
		task:   firstNonEmpty(change.TaskID, change.Event.TaskID),
		branch: firstNonEmpty(change.Branch, change.Event.Branch),
		commit: firstNonEmpty(change.Commit, change.Event.Commit),
	}
	if task := change.State.ActiveTask(); task != nil {
		authority.task = firstNonEmpty(authority.task, task.IDValue)
		if change.Event.Type == ledger.EventTaskStarted {
			authority.branch = firstNonEmpty(authority.branch, task.Branch)
			authority.commit = firstNonEmpty(authority.commit, task.Commit)
		}
	}
	return authority
}

func applyAuthority(page *WarmPage, authority compilerAuthority) bool {
	if page == nil {
		return false
	}
	switch page.Scope {
	case types.ScopeGlobal:
		return true
	case types.ScopeRepo:
		if authority.repo == "" {
			return false
		}
		page.RepoID = authority.repo
	case types.ScopeTask:
		if authority.repo == "" || authority.task == "" {
			return false
		}
		page.RepoID, page.TaskID = authority.repo, authority.task
	case types.ScopeBranch:
		if authority.repo == "" || authority.branch == "" {
			return false
		}
		page.RepoID, page.TaskID, page.Branch, page.Commit = authority.repo, authority.task, authority.branch, authority.commit
	case types.ScopeFile:
		if authority.repo == "" || authority.branch == "" || authority.commit == "" || len(page.PathScope) == 0 {
			return false
		}
		page.RepoID, page.TaskID, page.Branch, page.Commit = authority.repo, authority.task, authority.branch, authority.commit
	default:
		return false
	}
	return true
}

func authorityMatchesPage(page WarmPage, authority compilerAuthority) bool {
	if page.Scope == types.ScopeGlobal {
		return true
	}
	if page.RepoID == "" || page.RepoID != authority.repo {
		return false
	}
	if page.TaskID != "" && page.TaskID != authority.task {
		return false
	}
	switch page.Scope {
	case types.ScopeRepo:
		return true
	case types.ScopeTask:
		return authority.task != "" && page.TaskID == authority.task
	case types.ScopeBranch:
		return authority.branch != "" && page.Branch == authority.branch && (page.Commit == "" || page.Commit == authority.commit)
	case types.ScopeFile:
		return authority.branch != "" && authority.commit != "" && page.Branch == authority.branch && page.Commit == authority.commit
	default:
		return false
	}
}

func recordsDerivedFrom(state *wsl.WorkingState, eventID string) []wsl.Record {
	provenance := state.Provenance()
	var records []wsl.Record
	for _, record := range state.Records() {
		if provenance[record.ID()] == eventID {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].ID() == records[j].ID() {
			return records[i].Kind() < records[j].Kind()
		}
		return records[i].ID() < records[j].ID()
	})
	return records
}

func expandRefs(change LedgerChange, base []PageRef) ([]PageRef, error) {
	refs := append([]PageRef{{Kind: RefEvent, ID: change.Event.ID}}, base...)
	if change.Event.ArtifactHash != "" {
		refs = append(refs, PageRef{Kind: RefArtifact, ID: change.Event.ArtifactHash})
	}
	for _, ref := range append([]PageRef(nil), refs...) {
		if ref.Kind != RefWSLRecord {
			continue
		}
		evidenceID, ok := change.State.EvidenceID(ref.ID)
		if !ok || !logicalIDRE.MatchString(evidenceID) {
			return nil, fmt.Errorf("%w: WSL record %s has no durable provenance", ErrUnmaterializableRef, ref.ID)
		}
		refs = append(refs, PageRef{Kind: RefEvent, ID: evidenceID})
	}
	seen := map[string]bool{}
	out := make([]PageRef, 0, len(refs))
	for _, ref := range refs {
		if err := ref.validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnmaterializableRef, err)
		}
		address := ref.Address()
		if !seen[address] {
			seen[address] = true
			out = append(out, ref)
		}
	}
	sortRefs(out)
	if len(out) == 0 || len(out) > MaxPageRefs {
		return nil, fmt.Errorf("%w: expanded reference count %d", ErrUnmaterializableRef, len(out))
	}
	return out, nil
}

type resolvedEvidence struct {
	canonical []byte
	seq       int64
	ts        time.Time
}

func sourceDigest(ctx context.Context, change LedgerChange, page WarmPage) (SourceDigest, int64, int64, time.Time, error) {
	h := sha256.New()
	writeDigestPart(h, []byte("wsms-page-source-v1"))
	for _, field := range []string{
		string(page.CompilerVersion), string(page.ID), strconv.FormatUint(uint64(page.Version), 10),
		page.SessionID, string(page.Kind), string(page.Trust), string(page.Scope),
		page.RepoID, page.TaskID, page.Branch, page.Commit, strconv.FormatUint(uint64(page.ScopeEpoch), 10),
		page.SearchText, page.Summary, strconv.FormatFloat(page.Salience, 'g', -1, 64), page.SalienceReason,
		page.LastVerifiedAt.UTC().Format(time.RFC3339Nano),
	} {
		writeDigestPart(h, []byte(field))
	}
	for _, path := range page.PathScope {
		writeDigestPart(h, []byte(path))
	}
	var minSeq, maxSeq int64
	var created time.Time
	for _, ref := range page.Refs {
		if err := contextError(ctx); err != nil {
			return "", 0, 0, time.Time{}, err
		}
		resolved, err := resolveRef(ctx, change, ref)
		if err != nil {
			return "", 0, 0, time.Time{}, err
		}
		writeDigestPart(h, []byte(ref.Address()))
		writeDigestPart(h, resolved.canonical)
		if resolved.seq > 0 {
			if minSeq == 0 || resolved.seq < minSeq {
				minSeq = resolved.seq
			}
			if resolved.seq > maxSeq {
				maxSeq = resolved.seq
			}
		}
		if !resolved.ts.IsZero() && (created.IsZero() || resolved.ts.Before(created)) {
			created = resolved.ts.UTC()
		}
	}
	if minSeq == 0 || maxSeq == 0 || created.IsZero() {
		return "", 0, 0, time.Time{}, fmt.Errorf("%w: page source has no durable event", ErrUnmaterializableRef)
	}
	return SourceDigest(hex.EncodeToString(h.Sum(nil))), minSeq, maxSeq, created, nil
}

func resolveRef(ctx context.Context, change LedgerChange, ref PageRef) (resolvedEvidence, error) {
	if err := contextError(ctx); err != nil {
		return resolvedEvidence{}, err
	}
	switch ref.Kind {
	case RefWSLRecord:
		record, ok := change.State.Get(ref.ID)
		if !ok {
			return resolvedEvidence{}, fmt.Errorf("%w: missing WSL record %s", ErrUnmaterializableRef, ref.ID)
		}
		return resolvedEvidence{canonical: []byte(wsl.Serialize([]wsl.Record{record}))}, nil
	case RefEvent:
		ev, err := eventByID(ctx, change, ref.ID)
		if err != nil {
			return resolvedEvidence{}, err
		}
		canonical, err := canonicalEvent(ev)
		if err != nil {
			return resolvedEvidence{}, fmt.Errorf("%w: canonical event %s: %v", ErrUnmaterializableRef, ref.ID, err)
		}
		return resolvedEvidence{canonical: canonical, seq: ev.Seq, ts: ev.TS}, nil
	case RefArtifact:
		if change.Artifacts == nil {
			return resolvedEvidence{}, fmt.Errorf("artifact reader unavailable for %s", ref.ID)
		}
		if err := change.Artifacts.VerifyArtifact(ctx, ref.ID); err != nil {
			if missingEvidence(err) {
				return resolvedEvidence{}, fmt.Errorf("%w: missing artifact %s", ErrUnmaterializableRef, ref.ID)
			}
			return resolvedEvidence{}, fmt.Errorf("verify artifact %s: %w", ref.ID, err)
		}
		// The source digest commits to the verified address, never raw bytes.
		return resolvedEvidence{canonical: []byte("artifact-sha256:" + ref.ID)}, nil
	case RefFileSlice:
		if change.Files == nil {
			return resolvedEvidence{}, fmt.Errorf("file-slice reader unavailable for %s", ref.Address())
		}
		data, err := change.Files.ReadFileSlice(ctx, ref.Path, ref.Commit, ref.StartLine, ref.EndLine)
		if err != nil {
			if missingEvidence(err) {
				return resolvedEvidence{}, fmt.Errorf("%w: missing file slice %s", ErrUnmaterializableRef, ref.Address())
			}
			return resolvedEvidence{}, fmt.Errorf("read file slice %s: %w", ref.Address(), err)
		}
		if len(data) > MaxFileSliceBytes {
			return resolvedEvidence{}, fmt.Errorf("%w: file slice %s exceeds %d bytes", ErrUnmaterializableRef, ref.Address(), MaxFileSliceBytes)
		}
		digest := sha256.Sum256(data)
		return resolvedEvidence{canonical: []byte(ref.Address() + "\x00sha256:" + hex.EncodeToString(digest[:]))}, nil
	default:
		return resolvedEvidence{}, fmt.Errorf("%w: unknown ref kind %q", ErrUnmaterializableRef, ref.Kind)
	}
}

func eventByID(ctx context.Context, change LedgerChange, id string) (ledger.Event, error) {
	if change.Event.ID == id {
		return change.Event, nil
	}
	if change.Events == nil {
		return ledger.Event{}, fmt.Errorf("event reader unavailable for %s", id)
	}
	ev, err := change.Events.Get(ctx, id)
	if err != nil {
		if missingEvidence(err) {
			return ledger.Event{}, fmt.Errorf("%w: missing event %s", ErrUnmaterializableRef, id)
		}
		return ledger.Event{}, fmt.Errorf("read event %s: %w", id, err)
	}
	if ev.ID != id || ev.SessionID != change.Event.SessionID || ev.Seq <= 0 || ev.TS.IsZero() {
		return ledger.Event{}, fmt.Errorf("%w: event %s crossed session or lacks durable identity", ErrUnmaterializableRef, id)
	}
	if !logicalIDRE.MatchString(ev.ID) || ev.Seq > change.Event.Seq {
		return ledger.Event{}, fmt.Errorf("%w: event %s is malformed or from the future", ErrUnmaterializableRef, id)
	}
	if err := ledger.ValidateEvent(ev); err != nil {
		return ledger.Event{}, fmt.Errorf("%w: event %s failed validation: %v", ErrUnmaterializableRef, id, err)
	}
	return ev, nil
}

func missingEvidence(err error) bool {
	return errors.Is(err, wsmserrors.ErrNotFound) ||
		errors.Is(err, artifacts.ErrArtifactNotFound) ||
		errors.Is(err, fs.ErrNotExist)
}

func canonicalEvent(ev ledger.Event) ([]byte, error) {
	type eventWire struct {
		ID           string           `json:"id"`
		Seq          int64            `json:"append_seq"`
		TS           string           `json:"ts"`
		Type         ledger.EventType `json:"type"`
		SessionID    string           `json:"session_id"`
		TaskID       string           `json:"task_id,omitempty"`
		Repo         string           `json:"repo,omitempty"`
		Branch       string           `json:"branch,omitempty"`
		Commit       string           `json:"commit,omitempty"`
		Payload      map[string]any   `json:"payload,omitempty"`
		ArtifactHash string           `json:"artifact_hash,omitempty"`
		Scope        map[string]any   `json:"scope,omitempty"`
	}
	return json.Marshal(eventWire{
		ID: ev.ID, Seq: ev.Seq, TS: ev.TS.UTC().Format(time.RFC3339Nano), Type: ev.Type,
		SessionID: ev.SessionID, TaskID: ev.TaskID, Repo: ev.Repo, Branch: ev.Branch, Commit: ev.Commit,
		Payload: ev.Payload, ArtifactHash: ev.ArtifactHash, Scope: ev.Scope,
	})
}

func writeDigestPart(h interface{ Write([]byte) (int, error) }, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

func canonicalPaths(paths []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, candidate := range paths {
		cleaned, ok := normalizeRepoPath(candidate)
		if !ok || cleaned != candidate {
			return nil, fmt.Errorf("%w: noncanonical page path %q", ErrInvalidPage, candidate)
		}
		if !seen[candidate] {
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	sort.Strings(out)
	return out, nil
}

func boundedText(fields []textField, maxTokens, maxBytes int) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		value := normalizeText(field.value)
		if value == "" {
			continue
		}
		name := normalizeText(field.name)
		if name == "" {
			continue
		}
		parts = append(parts, name+"="+value)
	}
	tokens := strings.Fields(strings.Join(parts, "\n"))
	if len(tokens) > maxTokens {
		tokens = tokens[:maxTokens]
	}
	text := strings.Join(tokens, " ")
	if len(text) <= maxBytes {
		return text
	}
	return strings.TrimSpace(cutUTF8(text, maxBytes))
}

func normalizeText(value string) string {
	value = strings.ReplaceAll(value, "artifact:sha256:", "artifact_ref:")
	return strings.Join(strings.Fields(value), " ")
}

func cutUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func pathFromFileHint(hint string) []string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return nil
	}
	for i := len(hint) - 1; i >= 0; i-- {
		if hint[i] != ':' {
			continue
		}
		line := hint[i+1:]
		if _, err := strconv.Atoi(strings.SplitN(line, "-", 2)[0]); err == nil {
			hint = hint[:i]
		}
		break
	}
	cleaned, ok := normalizeRepoPath(hint)
	if !ok || cleaned != hint {
		return nil
	}
	return []string{cleaned}
}

func parseLogicalAddresses(value string) []string {
	var out []string
	seen := map[string]bool{}
	for _, field := range strings.Fields(value) {
		field = strings.Trim(field, ",")
		if field != "" && !seen[field] {
			seen[field] = true
			out = append(out, field)
		}
	}
	sort.Strings(out)
	return out
}

func logicalRef(state *wsl.WorkingState, id string) PageRef {
	if state != nil {
		if _, ok := state.Get(id); ok {
			return PageRef{Kind: RefWSLRecord, ID: id}
		}
	}
	return PageRef{Kind: RefEvent, ID: id}
}

func constraintTrust(source types.Source) (Trust, bool) {
	switch source {
	case types.SourceUser:
		return TrustUser, true
	case types.SourceRepo:
		return TrustRepo, true
	case types.SourceSystem:
		return TrustSystem, true
	default:
		return "", false
	}
}

func defaultScope(scope types.Scope) types.Scope {
	if scope == "" {
		return types.ScopeTask
	}
	return scope
}

func nextSummary(next *wsl.NextRecord) string {
	if next == nil {
		return ""
	}
	return strings.TrimSpace(next.Action + " " + next.Target + " " + next.Question)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func logicalCoherenceRefs(refs []PageRef) []string {
	out := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		if ref.Kind != RefWSLRecord && ref.Kind != RefEvent {
			continue
		}
		if !seen[ref.ID] {
			seen[ref.ID] = true
			out = append(out, ref.ID)
		}
	}
	sort.Strings(out)
	return out
}
