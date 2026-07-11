package operator

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"wsms/internal/harness"
	"wsms/internal/ledger"
)

// DeleteSpec identifies one logical target to invalidate and why.
type DeleteSpec struct {
	Kind   string // record | event | page | path
	Target string // the target id/path
	Reason string // superseded | user_rejected | source_deleted | policy_changed | security_revoked
}

// validKinds / validReasons are the closed policy sets from the ledger. The
// reason matters beyond bookkeeping: security_revoked and policy_changed also
// withdraw raw L4 evidence access, while the others suppress reuse but keep
// diagnostic L4 readable — so the operator must choose deliberately.
var validKinds = map[string]ledger.MemoryTargetKind{
	"record": ledger.TargetRecord,
	"event":  ledger.TargetEvent,
	"page":   ledger.TargetPage,
	"path":   ledger.TargetPath,
}

var validReasons = map[string]ledger.InvalidationReason{
	"superseded":       ledger.ReasonSuperseded,
	"user_rejected":    ledger.ReasonUserRejected,
	"source_deleted":   ledger.ReasonSourceDeleted,
	"policy_changed":   ledger.ReasonPolicyChanged,
	"security_revoked": ledger.ReasonSecurityRevoked,
}

// Delete logically deletes a target by appending a memory_invalidated event.
// The bytes stay in L4; every derived cache (L1 capsule, L2 residency, L3 warm
// index) stops serving the target, and — for security/policy reasons — raw L4
// access is withdrawn too. It is reversible only for stale (not invalidated)
// targets via revalidation, by design.
func Delete(ctx context.Context, opts Options, spec DeleteSpec, w io.Writer) error {
	kind, ok := validKinds[spec.Kind]
	if !ok {
		return fmt.Errorf("invalid --kind %q (want one of %s)", spec.Kind, keys(validKinds))
	}
	reason, ok := validReasons[spec.Reason]
	if !ok {
		return fmt.Errorf("invalid --reason %q (want one of %s)", spec.Reason, keys(validReasons))
	}
	if strings.TrimSpace(spec.Target) == "" {
		return fmt.Errorf("--target is required")
	}

	session, err := harness.OpenSession(cfgFor(opts))
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = session.Close() }()

	if err := session.InvalidateMemory(ctx, harness.MemoryInvalidation{
		Kind:   kind,
		Target: spec.Target,
		Reason: reason,
	}); err != nil {
		return fmt.Errorf("invalidate %s %q: %w", spec.Kind, spec.Target, err)
	}

	return reportf(w, "invalidated %s %q (reason: %s); L4 evidence retained\n", spec.Kind, spec.Target, spec.Reason)
}

func keys[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
