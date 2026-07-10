package faults

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"wsms/internal/artifacts"
	"wsms/internal/coherence"
	wsmserrors "wsms/internal/errors"
	"wsms/internal/ledger"
	"wsms/internal/memory"
	"wsms/internal/renderer"
	"wsms/internal/wsl"
)

// PageMiss is the string returned when a page cannot be resolved.
const PageMiss = "PAGE_MISS"

var (
	// ErrFileSliceUnauthorized reports that production file-slice faults are
	// disabled until the resolver receives a workspace-root capability.
	ErrFileSliceUnauthorized = errors.New("file_slice fault requires an authorized root capability")
	// ErrMissingRawEvidence reports an existing failure whose backing evidence
	// cannot be resolved because it has neither raw content nor provenance.
	ErrMissingRawEvidence = errors.New("failure has no raw evidence or provenance")
	// ErrRawEvidenceRevoked reports a policy/security invalidation that also
	// withdraws diagnostic byte access, not merely residency eligibility.
	ErrRawEvidenceRevoked = errors.New("raw evidence access revoked")
)

// Resolver resolves fault requests against hierarchy, WSL, ledger, and artifacts.
type Resolver struct {
	State     *wsl.WorkingState
	Hierarchy *memory.Hierarchy
	Ledger    *ledger.AppendOnlyLedger
	Artifacts *artifacts.Store
	Coherence *coherence.State
}

// Resolve handles a page fault request.
func (r *Resolver) Resolve(ctx context.Context, req Request) (string, error) {
	budget := req.Budget
	if budget <= 0 {
		budget = 256
	}

	switch req.Kind {
	case "", "page":
		return r.resolvePage(ctx, req.ID, budget)
	case "raw_log":
		return r.resolveRaw(ctx, req.ID, budget)
	case "file_slice":
		return "", ErrFileSliceUnauthorized
	default:
		return "", fmt.Errorf("unknown fault kind %q", req.Kind)
	}
}

func (r *Resolver) resolvePage(ctx context.Context, id string, budget int) (string, error) {
	_ = ctx
	if id == "" {
		return PageMiss, nil
	}
	// Authoritative coherence is checked before every cache and WSL lookup.
	// A stale L2 hit must never win and fallback must never resurrect it.
	if r.Coherence != nil && !r.Coherence.RecordEligible(id) {
		return PageMiss, nil
	}
	if r.Hierarchy != nil {
		if p, ok := r.Hierarchy.GetPage(id); ok {
			if !p.Stale && !p.Invalidated {
				r.Hierarchy.RecordAccess(id)
				body := p.Body
				if body == "" {
					body = p.Summary
				}
				return trimBudget(body, budget), nil
			}
			// An eligible logical address with an old resident generation is a
			// cache miss, not an authority miss. Fall through to WSL/L4 and
			// rematerialize instead of refreshing stale body metadata in place.
		}
	}
	if r.State == nil {
		return PageMiss, nil
	}
	if f := r.State.FailureByID(id); f != nil {
		// materialize into L2 when a hierarchy is configured
		body := renderer.RenderFailureDetail(f)
		if r.Hierarchy != nil {
			page := &memory.Page{ID: id, Summary: f.Err, Refs: []string{id}, Body: body}
			if r.Coherence != nil {
				if binding, ok := r.Coherence.BindingFor(ledger.TargetRecord, id); ok {
					page.Scope = binding.Scope
					page.Branch = binding.Branch
					page.Commit = binding.Commit
					page.Paths = append([]string(nil), binding.Paths...)
					page.SourceDigest = binding.SourceDigest
					page.ScopeEpoch = binding.Generation()
				}
			}
			r.Hierarchy.PutL2(page)
			r.Hierarchy.RecordAccess(id)
		}
		return trimBudget(body, budget), nil
	}
	if rec, ok := r.State.Get(id); ok {
		text := wsl.Serialize([]wsl.Record{rec})
		return trimBudget(text, budget), nil
	}
	return PageMiss, nil
}

func (r *Resolver) resolveRaw(ctx context.Context, id string, budget int) (string, error) {
	if r.Coherence != nil && !r.Coherence.RawAllowed(id) {
		return "", fmt.Errorf("resolve raw log %s: %w", id, ErrRawEvidenceRevoked)
	}
	if r.State != nil {
		if f := r.State.FailureByID(id); f != nil {
			if f.Raw == "" {
				evidenceID, ok := r.State.EvidenceID(id)
				if !ok {
					return "", fmt.Errorf("resolve raw log %s: %w", id, ErrMissingRawEvidence)
				}
				return r.resolveEvidenceEvent(ctx, id, evidenceID, budget)
			}
			if !strings.HasPrefix(f.Raw, artifacts.RefPrefix) {
				return trimBudget(f.Raw, budget), nil
			}
			hash, ok := artifacts.ParseRef(f.Raw)
			if !ok {
				return "", fmt.Errorf("%w: %q", artifacts.ErrInvalidRef, f.Raw)
			}
			if r.Artifacts == nil {
				return "", fmt.Errorf("resolve raw log %s: artifact store unavailable", id)
			}
			data, err := r.Artifacts.Get(hash)
			if err != nil {
				return "", err
			}
			return trimBudget(string(data), budget), nil
		}
	}
	if strings.HasPrefix(id, "E") {
		return r.resolveEvent(ctx, id, budget)
	}
	return PageMiss, nil
}

func (r *Resolver) resolveEvidenceEvent(ctx context.Context, failureID, eventID string, budget int) (string, error) {
	if r.Ledger == nil {
		return "", fmt.Errorf("resolve raw log %s: ledger unavailable for provenance event %s", failureID, eventID)
	}
	ev, err := r.Ledger.Get(ctx, eventID)
	if err != nil {
		return "", fmt.Errorf("resolve raw log %s: provenance event %s: %w", failureID, eventID, err)
	}
	return r.rawEventPayload(failureID, ev, budget)
}

func (r *Resolver) resolveEvent(ctx context.Context, eventID string, budget int) (string, error) {
	if r.Ledger == nil {
		return "", fmt.Errorf("resolve raw log %s: ledger unavailable", eventID)
	}
	ev, err := r.Ledger.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, wsmserrors.ErrNotFound) {
			return PageMiss, nil
		}
		return "", err
	}
	return r.rawEventPayload(eventID, ev, budget)
}

func (r *Resolver) rawEventPayload(requestID string, ev ledger.Event, budget int) (string, error) {
	if ev.ArtifactHash != "" {
		if r.Artifacts == nil {
			return "", fmt.Errorf("resolve raw log %s: artifact store unavailable", requestID)
		}
		data, err := r.Artifacts.Get(ev.ArtifactHash)
		if err != nil {
			return "", err
		}
		return trimBudget(string(data), budget), nil
	}
	if output, ok := ev.Payload["output"].(string); ok {
		return trimBudget(output, budget), nil
	}
	return trimBudget(fmt.Sprintf("%v", ev.Payload), budget), nil
}

func trimBudget(s string, budget int) string {
	if renderer.EstimateTokens(s) <= budget {
		return s
	}
	// crude: keep leading runes until token budget roughly met
	fields := strings.Fields(s)
	if len(fields) <= budget {
		return s
	}
	return strings.Join(fields[:budget], " ") + " …"
}
