package memory

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"wsms/internal/types"
)

func TestPutL2CopiesPageAndRefsWithoutMutatingCaller(t *testing.T) {
	caller := &Page{
		ID:   "P1",
		Tier: types.TierL3Warm,
		Refs: []string{"E0001", "F1"},
	}
	h := NewHierarchy()
	h.PutL2(caller)

	if caller.Tier != types.TierL3Warm {
		t.Fatalf("PutL2 mutated caller tier to %q", caller.Tier)
	}
	caller.Refs[0] = "tampered-caller"

	got, ok := h.GetPage("P1")
	if !ok {
		t.Fatal("stored page not found")
	}
	if got.Tier != types.TierL2Hot {
		t.Fatalf("stored tier=%q, want %q", got.Tier, types.TierL2Hot)
	}
	if got.Refs[0] != "E0001" {
		t.Fatalf("caller Refs mutation leaked into hierarchy: %#v", got.Refs)
	}

	got.Refs[1] = "tampered-result"
	again, ok := h.GetPage("P1")
	if !ok {
		t.Fatal("stored page disappeared")
	}
	if again.Refs[1] != "F1" {
		t.Fatalf("GetPage Refs mutation leaked into hierarchy: %#v", again.Refs)
	}
}

func TestGetPageCopiesL3Refs(t *testing.T) {
	h := NewHierarchy()
	caller := &Page{ID: "P3", Tier: types.TierL2Hot, Refs: []string{"E0003"}}
	h.PutL3(caller)
	if caller.Tier != types.TierL2Hot {
		t.Fatalf("PutL3 mutated caller tier to %q", caller.Tier)
	}
	caller.Refs[0] = "tampered-caller"

	got, ok := h.GetPage("P3")
	if !ok {
		t.Fatal("L3 page not found")
	}
	if got.Tier != types.TierL3Warm {
		t.Fatalf("stored tier=%q, want %q", got.Tier, types.TierL3Warm)
	}
	if got.Refs[0] != "E0003" {
		t.Fatalf("caller Refs mutation leaked into L3: %#v", got.Refs)
	}
	got.Refs[0] = "tampered-result"

	again, ok := h.GetPage("P3")
	if !ok {
		t.Fatal("L3 page disappeared")
	}
	if again.Refs[0] != "E0003" {
		t.Fatalf("GetPage L3 Refs mutation leaked: %#v", again.Refs)
	}
}

func TestPageMovesExclusivelyBetweenResidentTiers(t *testing.T) {
	h := NewHierarchy()
	h.PutL3(&Page{ID: "P1", AccessCount: 3})
	if _, ok := h.l3["P1"]; !ok {
		t.Fatal("PutL3 did not populate L3")
	}

	h.PutL2(&Page{ID: "P1", AccessCount: 7})
	if _, ok := h.l3["P1"]; ok {
		t.Fatal("PutL2 retained an old L3 copy")
	}
	if _, ok := h.l2["P1"]; !ok {
		t.Fatal("PutL2 did not populate L2")
	}
	h.RecordAccess("P1")
	got, ok := h.GetPage("P1")
	if !ok || got.Tier != types.TierL2Hot || got.AccessCount != 8 {
		t.Fatalf("hot page after access=%#v, found=%v", got, ok)
	}

	h.PutL3(&Page{ID: "P1", AccessCount: 11})
	if _, ok := h.l2["P1"]; ok {
		t.Fatal("PutL3 retained an old L2 copy")
	}
	if _, ok := h.l3["P1"]; !ok {
		t.Fatal("PutL3 did not repopulate L3")
	}
	h.RecordAccess("P1")
	got, ok = h.GetPage("P1")
	if !ok || got.Tier != types.TierL3Warm || got.AccessCount != 12 {
		t.Fatalf("warm page after access=%#v, found=%v", got, ok)
	}
}

func TestPutResidentPageIsNilSafe(t *testing.T) {
	h := NewHierarchy()
	h.PutL2(nil)
	h.PutL3(nil)
	if _, ok := h.GetPage(""); ok {
		t.Fatal("nil insertion created a resident page")
	}
}

func TestReconcilePersistsAndCanRestorePageCoherence(t *testing.T) {
	h := NewHierarchy()
	h.PutL2(&Page{ID: "F1", Branch: "main", Commit: "a", Paths: []string{"src/a.go"}, Body: "poisoned stale body"})
	h.Reconcile(func(*Page) PageCoherence {
		return PageCoherence{
			Stale: true, StaleRevision: 2, Branch: "feature", Commit: "b",
			Paths: []string{"src/a.go"}, ScopeEpoch: 7,
		}
	})
	page, ok := h.GetPage("F1")
	if !ok || !page.Stale || page.Invalidated || page.StaleRevision != 2 || page.Branch != "feature" || page.Commit != "b" || page.ScopeEpoch != 7 {
		t.Fatalf("stale reconciliation=%#v found=%v", page, ok)
	}
	h.Reconcile(func(*Page) PageCoherence {
		return PageCoherence{Branch: "feature", Commit: "b", Paths: []string{"src/a.go"}, ScopeEpoch: 7}
	})
	page, ok = h.GetPage("F1")
	if !ok || page.Stale || page.Invalidated || page.StaleRevision != 0 {
		t.Fatalf("active reconciliation=%#v found=%v", page, ok)
	}
	if page.Body != "" || page.Summary != "" || len(page.Refs) != 0 {
		t.Fatalf("active reconciliation retained poisoned materialization: %#v", page)
	}
}

func TestReconcileOversizedMetadataRefreshFailsClosed(t *testing.T) {
	policy := testPolicy(4)
	policy.MaxPageBytes = 256
	h, err := NewHierarchyWithPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	p := testPage("P-reconcile-oversized", 1)
	p.Body = "small"
	p.Summary = "small"
	p.Paths = []string{"src/small.go"}
	if err := h.AdmitDemand(p); err != nil {
		t.Fatalf("admit: %v", err)
	}
	h.Reconcile(func(*Page) PageCoherence {
		return PageCoherence{
			Branch: "main", Commit: "commit-a", ScopeEpoch: p.ScopeEpoch,
			Paths: []string{strings.Repeat("oversized-path/", 32)},
		}
	})
	if got, ok := h.GetPage(p.ID); ok || got != nil {
		t.Fatalf("oversized reconcile retained resident got=%#v ok=%v", got, ok)
	}
	snap := h.Snapshot()
	if snap.ResidentPages != 0 || snap.ResidentBytes > policy.MaxResidentBytes || snap.Metrics.PageTooLargeRejections != 1 {
		t.Fatalf("oversized reconcile snapshot=%#v metrics=%#v", snap, snap.Metrics)
	}
}

func TestAdmitDemandColdThenUsePromotesAndStaleLookupMisses(t *testing.T) {
	h := NewHierarchy()
	p := testPage("P-demand", 1)
	if err := h.AdmitDemand(p); err != nil {
		t.Fatalf("AdmitDemand: %v", err)
	}
	snap := h.Snapshot()
	res := requireResident(t, snap, p.ID)
	if res.State != string(stateCold) || !res.Ref || res.RealUses != 1 {
		t.Fatalf("first demand state=%#v, want cold ref1/use1", res)
	}
	if got, result, ok := h.Use(p.ID); !ok || got.ID != p.ID || !result.Promoted || result.State != string(stateHot) {
		t.Fatalf("Use result page=%#v result=%#v ok=%v", got, result, ok)
	}
	res = requireResident(t, h.Snapshot(), p.ID)
	if res.State != string(stateHot) || res.RealUses != 2 {
		t.Fatalf("second use state=%#v, want hot/use2", res)
	}
	h.Reconcile(func(*Page) PageCoherence { return PageCoherence{Stale: true, StaleRevision: 9} })
	if got, ok := h.LookupAndUse(p.ID); ok || got != nil {
		t.Fatalf("LookupAndUse served stale page=%#v ok=%v", got, ok)
	}
	res = requireResident(t, h.Snapshot(), p.ID)
	if res.RealUses != 2 {
		t.Fatalf("stale lookup mutated use count: %#v", res)
	}
}

func TestAdmitDemandExistingExactIsUseHitAndPromotes(t *testing.T) {
	h := NewHierarchy()
	p := testPage("P-demand-hit", 1)
	if err := h.AdmitDemand(p); err != nil {
		t.Fatalf("first demand: %v", err)
	}
	first := h.Snapshot()
	if first.Metrics.DemandAdmissions != 1 || first.Metrics.DemandPageIns != 1 || first.Metrics.Uses != 1 || first.Metrics.Hits != 0 {
		t.Fatalf("first demand metrics=%#v", first.Metrics)
	}
	refreshed := *p
	refreshed.Body = "refreshed exact body"
	if err := h.AdmitDemand(&refreshed); err != nil {
		t.Fatalf("resident exact demand: %v", err)
	}
	snap := h.Snapshot()
	res := requireResident(t, snap, p.ID)
	if res.State != string(stateHot) || res.RealUses != 2 || !res.Materialized {
		t.Fatalf("resident exact demand state=%#v", res)
	}
	if snap.Metrics.DemandAdmissions != 1 || snap.Metrics.DemandPageIns != 2 || snap.Metrics.Uses != 2 || snap.Metrics.Hits != 0 || snap.Metrics.Promotions != 1 {
		t.Fatalf("resident exact demand metrics=%#v", snap.Metrics)
	}
	got, ok := h.GetPage(p.ID)
	if !ok || got.Body != refreshed.Body {
		t.Fatalf("resident exact demand did not refresh body: %#v ok=%v", got, ok)
	}
}

func TestOverboundDemandStillRecordsServedUseAndSettlesShadow(t *testing.T) {
	h := NewHierarchy()
	p := testPage("P-overbound-demand", 1)
	p.EmbeddingNamespace = "dense/overbound"
	if err := h.ObserveSemantic(p); err != nil {
		t.Fatalf("observe shadow: %v", err)
	}
	p.Body = strings.Repeat("x", h.policy.MaxPageBytes+1)
	if err := h.AdmitDemand(p); !errors.Is(err, ErrPageTooLarge) {
		t.Fatalf("overbound demand err=%v, want ErrPageTooLarge", err)
	}
	if got, ok := h.GetPage(p.ID); ok || got != nil {
		t.Fatalf("overbound demand became resident got=%#v ok=%v", got, ok)
	}
	snap := h.Snapshot()
	if snap.UseTick != 1 || snap.Metrics.Uses != 1 || snap.Metrics.DemandPageIns != 1 ||
		snap.Metrics.ShadowUseful != 1 || snap.Metrics.PageTooLargeRejections != 1 {
		t.Fatalf("overbound demand snapshot=%#v metrics=%#v", snap, snap.Metrics)
	}
	if snap.ShadowPages != 1 || !snap.Shadows[0].Useful {
		t.Fatalf("overbound demand did not settle exact shadow: %#v", snap.Shadows)
	}
}

func TestLookupAndUseIfCurrentRequiresExactCurrentRepresentation(t *testing.T) {
	h := NewHierarchy()
	current := testPage("P-current", 1)
	if err := h.AdmitDemand(current); err != nil {
		t.Fatalf("admit: %v", err)
	}
	mismatch := *current
	mismatch.Body = "new body from current WSL"
	if got, result, ok := h.LookupAndUseIfCurrent(&mismatch); ok || got != nil || result != (UseResult{}) {
		t.Fatalf("mismatched current representation counted hit page=%#v result=%#v ok=%v", got, result, ok)
	}
	if metrics := h.Snapshot().Metrics; metrics.Hits != 0 || metrics.Uses != 1 {
		t.Fatalf("mismatch mutated metrics: %#v", metrics)
	}
	if err := h.AdmitDemand(&mismatch); err != nil {
		t.Fatalf("refresh demand: %v", err)
	}
	if got, result, ok := h.LookupAndUseIfCurrent(&mismatch); !ok || got.Body != mismatch.Body || result.State != string(stateHot) {
		t.Fatalf("validated lookup page=%#v result=%#v ok=%v", got, result, ok)
	}
	if metrics := h.Snapshot().Metrics; metrics.Hits != 1 {
		t.Fatalf("validated lookup did not count hit: %#v", metrics)
	}
}

func TestPinAndLegacyHotDoNotFabricateRealUse(t *testing.T) {
	h := NewHierarchy()
	pinned := testPage("P-policy-pin", 1)
	if err := h.Pin(pinned); err != nil {
		t.Fatalf("pin: %v", err)
	}
	snap := h.Snapshot()
	res := requireResident(t, snap, pinned.ID)
	if res.RealUses != 0 || snap.Metrics.PinAdmissions != 1 || snap.Metrics.Uses != 0 || snap.Metrics.Hits != 0 {
		t.Fatalf("pin fabricated use res=%#v metrics=%#v", res, snap.Metrics)
	}
	if err := h.Pin(pinned); err != nil {
		t.Fatalf("repin: %v", err)
	}
	if metrics := h.Snapshot().Metrics; metrics.PinAdmissions != 1 {
		t.Fatalf("repin counted new admission: %#v", metrics)
	}
	legacy := testPage("P-legacy-hot", 1)
	h.PutL2(legacy)
	snap = h.Snapshot()
	res = requireResident(t, snap, legacy.ID)
	if res.RealUses != 0 || snap.Metrics.Uses != 0 || snap.Metrics.Hits != 0 {
		t.Fatalf("legacy hot fabricated use res=%#v metrics=%#v", res, snap.Metrics)
	}
}

func TestBodyClearedResidentMetadataMissesAtomicUse(t *testing.T) {
	h := NewHierarchy()
	p := testPage("P-body-clear", 1)
	if err := h.AdmitDemand(p); err != nil {
		t.Fatalf("admit: %v", err)
	}
	h.Reconcile(func(*Page) PageCoherence { return PageCoherence{Stale: true, StaleRevision: 1} })
	h.Reconcile(func(*Page) PageCoherence { return PageCoherence{} })
	if got, ok := h.GetPage(p.ID); !ok || got.Body != "" || got.Summary != "" {
		t.Fatalf("metadata compatibility page=%#v ok=%v", got, ok)
	}
	if got, ok := h.LookupAndUse(p.ID); ok || got != nil {
		t.Fatalf("LookupAndUse served body-cleared metadata: %#v ok=%v", got, ok)
	}
	res := requireResident(t, h.Snapshot(), p.ID)
	if res.Materialized {
		t.Fatalf("body-cleared resident marked materialized: %#v", res)
	}

	refsOnly := testPage("P-refs-only", 1)
	refsOnly.Body = ""
	refsOnly.Summary = ""
	if err := h.AdmitDemand(refsOnly); err != nil {
		t.Fatalf("refs-only admit: %v", err)
	}
	if got, ok := h.LookupAndUse(refsOnly.ID); ok || got != nil {
		t.Fatalf("LookupAndUse served refs-only metadata: %#v ok=%v", got, ok)
	}
}

func TestGhostRefaultIsExactAndBodyless(t *testing.T) {
	h, err := NewHierarchyWithPolicy(testPolicy(1))
	if err != nil {
		t.Fatal(err)
	}
	p1 := testPage("P-ghost-1", 1)
	p2 := testPage("P-ghost-2", 1)
	if err := h.AdmitDemand(p1); err != nil {
		t.Fatalf("admit p1: %v", err)
	}
	if err := h.AdmitDemand(p2); err != nil {
		t.Fatalf("admit p2: %v", err)
	}
	snap := h.Snapshot()
	if snap.GhostPages != 1 || snap.Ghosts[0].Tuple != p1.Tuple() || snap.GhostBytes <= 0 {
		t.Fatalf("ghost snapshot=%#v, want bodyless p1 ghost", snap)
	}
	if err := h.AdmitDemand(p1); err != nil {
		t.Fatalf("exact ghost refault: %v", err)
	}
	res := requireResident(t, h.Snapshot(), p1.ID)
	if res.State != string(stateHot) {
		t.Fatalf("exact ghost refault state=%#v, want hot", res)
	}
	if metrics := h.Snapshot().Metrics; metrics.GhostHits != 1 || metrics.GhostRefaultDistance != 1 || metrics.GhostThrash != 0 {
		t.Fatalf("zero-hot ghost refault metrics=%#v, want hit without thrash", metrics)
	}
	if err := h.AdmitDemand(p2); err != nil {
		t.Fatalf("re-evict p1 with p2: %v", err)
	}
	mismatch := testPage("P-ghost-1", 2)
	if err := h.AdmitDemand(mismatch); err != nil {
		t.Fatalf("mismatched ghost demand should admit cold, got %v", err)
	}
	res = requireResident(t, h.Snapshot(), mismatch.ID)
	if res.State != string(stateCold) {
		t.Fatalf("mismatched ghost refault state=%#v, want cold", res)
	}
}

func TestShootdownPurgesGhostOnlyAndShadowOnlyMetadata(t *testing.T) {
	h, err := NewHierarchyWithPolicy(testPolicy(1))
	if err != nil {
		t.Fatal(err)
	}
	ghosted := testPage("P-shoot-ghost", 1)
	if err := h.AdmitDemand(ghosted); err != nil {
		t.Fatalf("admit ghosted: %v", err)
	}
	if err := h.AdmitDemand(testPage("P-shoot-other", 1)); err != nil {
		t.Fatalf("evict ghosted: %v", err)
	}
	if h.Snapshot().GhostPages == 0 {
		t.Fatal("expected ghost before shootdown")
	}
	h.Shootdown(ghosted.ID)
	if snap := h.Snapshot(); snap.GhostPages != 0 || residencyContainsSnapshot(snap, ghosted.ID) {
		t.Fatalf("ghost shootdown snapshot=%#v", snap)
	}

	shadow := testPage("P-shoot-shadow", 1)
	if err := h.ObserveSemantic(shadow); err != nil {
		t.Fatalf("observe shadow: %v", err)
	}
	h.Shootdown(shadow.ID)
	snap := h.Snapshot()
	if snap.ShadowPages != 0 || snap.Metrics.ShadowCensored != 1 {
		t.Fatalf("shadow shootdown snapshot=%#v metrics=%#v", snap, snap.Metrics)
	}
}

func TestPurgeAllGhostsRemovesDisposableRefaultHints(t *testing.T) {
	h, err := NewHierarchyWithPolicy(testPolicy(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.AdmitDemand(testPage("P-purge-ghost-1", 1)); err != nil {
		t.Fatalf("admit first: %v", err)
	}
	if err := h.AdmitDemand(testPage("P-purge-ghost-2", 1)); err != nil {
		t.Fatalf("admit second: %v", err)
	}
	if h.Snapshot().GhostPages == 0 {
		t.Fatal("expected ghost before purge")
	}
	h.PurgeAllGhosts("test_transition")
	snap := h.Snapshot()
	if snap.GhostPages != 0 || snap.Metrics.GhostPurged != 1 {
		t.Fatalf("purge ghosts snapshot=%#v metrics=%#v", snap, snap.Metrics)
	}
}

func TestCensorAllShadowsRemovesOnlyPendingOutcomesAsCensored(t *testing.T) {
	h := NewHierarchy()
	useful := testPage("P-censor-useful", 1)
	pending := testPage("P-censor-pending", 1)
	if err := h.ObserveSemantic(useful); err != nil {
		t.Fatalf("observe useful: %v", err)
	}
	if err := h.AdmitDemand(useful); err != nil {
		t.Fatalf("demand useful: %v", err)
	}
	if err := h.ObserveSemantic(pending); err != nil {
		t.Fatalf("observe pending: %v", err)
	}
	h.CensorAllShadows("test_transition")
	snap := h.Snapshot()
	if snap.ShadowPages != 0 || snap.Metrics.ShadowUseful != 1 || snap.Metrics.ShadowCensored != 1 {
		t.Fatalf("censor all snapshot=%#v metrics=%#v", snap, snap.Metrics)
	}
}

func TestAdmitAuthoritativeDemandReplacesOldTupleAndShootsDownOnReject(t *testing.T) {
	policy := testPolicy(2)
	policy.MaxPageBytes = 256
	h, err := NewHierarchyWithPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	old := testPage("P-authority", 1)
	next := testPage("P-authority", 2)
	if err := h.AdmitDemand(old); err != nil {
		t.Fatalf("admit old: %v", err)
	}
	if err := h.ObserveSemanticObservation(SemanticObservation{
		Tuple: old.Tuple(), EmbeddingNamespace: "dense/a", CandidateOrdinal: 1,
	}); err != nil {
		t.Fatalf("observe old: %v", err)
	}
	if err := h.AdmitAuthoritativeDemand(next); err != nil {
		t.Fatalf("authoritative replace: %v", err)
	}
	if got, ok := h.GetPage(next.ID); !ok || got.PageVersion != next.PageVersion {
		t.Fatalf("authoritative replacement got=%#v ok=%v", got, ok)
	}
	snap := h.Snapshot()
	if snap.ShadowPages != 0 || snap.Metrics.AuthoritativeReplacements == 0 || snap.Metrics.ShadowCensored != 1 {
		t.Fatalf("authoritative replacement metrics/snapshot=%#v", snap)
	}

	tooLarge := testPage("P-authority", 3)
	tooLarge.Body = strings.Repeat("x", policy.MaxPageBytes+1)
	if err := h.AdmitAuthoritativeDemand(tooLarge); !errors.Is(err, ErrPageTooLarge) {
		t.Fatalf("overbound authoritative err=%v", err)
	}
	if got, ok := h.GetPage(next.ID); ok || got != nil {
		t.Fatalf("overbound replacement retained old tuple got=%#v ok=%v", got, ok)
	}
}

func TestPinOverflowIsTransactionalAndTraced(t *testing.T) {
	policy := testPolicy(2)
	policy.MaxPinnedPages = 1
	policy.MaxPinnedBytes = 4096
	h, err := NewHierarchyWithPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	p1 := testPage("P-pin-1", 1)
	p2 := testPage("P-pin-2", 1)
	if err := h.Pin(p1); err != nil {
		t.Fatalf("pin p1: %v", err)
	}
	before := h.Snapshot()
	if err := h.Pin(p2); !errors.Is(err, ErrPinnedCapacity) {
		t.Fatalf("pin p2 err=%v, want ErrPinnedCapacity", err)
	}
	after := h.Snapshot()
	if after.ResidentPages != before.ResidentPages || after.PinnedPages != before.PinnedPages || after.ResidentBytes != before.ResidentBytes {
		t.Fatalf("pin overflow mutated snapshot before=%#v after=%#v", before, after)
	}
	trace := h.Trace()
	if len(trace) == 0 || trace[len(trace)-1].Reason != "pinned_capacity" {
		t.Fatalf("last trace=%#v, want pinned_capacity", trace)
	}
}

func TestTupleMismatchActiveFailsButStaleDemandReplaces(t *testing.T) {
	h := NewHierarchy()
	current := testPage("P-tuple", 1)
	next := testPage("P-tuple", 2)
	if err := h.AdmitDemand(current); err != nil {
		t.Fatalf("admit current: %v", err)
	}
	if err := h.AdmitDemand(next); !errors.Is(err, ErrTupleMismatch) {
		t.Fatalf("active tuple mismatch err=%v, want ErrTupleMismatch", err)
	}
	if got, ok := h.GetPage(current.ID); !ok || got.PageVersion != current.PageVersion {
		t.Fatalf("active mismatch changed resident got=%#v ok=%v", got, ok)
	}
	h.Reconcile(func(*Page) PageCoherence { return PageCoherence{Stale: true, StaleRevision: 3} })
	if err := h.AdmitDemand(next); err != nil {
		t.Fatalf("stale replacement demand: %v", err)
	}
	if got, ok := h.GetPage(next.ID); !ok || got.PageVersion != next.PageVersion || got.Body != next.Body {
		t.Fatalf("stale replacement got=%#v ok=%v", got, ok)
	}
}

func TestSemanticShadowUsefulnessExpiryAndNamespaceAttribution(t *testing.T) {
	policy := testPolicy(8)
	policy.ShadowUseHorizon = 1
	h, err := NewHierarchyWithPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	shadowed := testPage("P-shadow", 1)
	shadowed.Body = "semantic candidate body must not be retained in shadow"
	shadowed.Summary = "semantic candidate summary must not be retained in shadow"
	shadowed.EmbeddingNamespace = "dense/qwen3:v1"
	if err := h.ObserveSemantic(shadowed); err != nil {
		t.Fatalf("observe semantic: %v", err)
	}
	snap := h.Snapshot()
	if snap.ShadowPages != 1 || snap.Shadows[0].EmbeddingNamespace != shadowed.EmbeddingNamespace || snap.ShadowBytes <= 0 {
		t.Fatalf("shadow snapshot=%#v", snap)
	}
	if _, ok := h.GetPage(shadowed.ID); ok {
		t.Fatal("semantic observation created a resident page")
	}
	shadowed.EmbeddingNamespace = "dense/qwen3:v2"
	if err := h.ObserveSemantic(shadowed); err != nil {
		t.Fatalf("duplicate semantic observation: %v", err)
	}
	dup := h.Snapshot()
	if dup.ShadowPages != 2 {
		t.Fatalf("distinct namespaces did not coexist: %#v", dup.Shadows)
	}
	if dup.Shadows[0].ObservedUseTick != snap.Shadows[0].ObservedUseTick || dup.Shadows[1].ObservedUseTick != snap.Shadows[0].ObservedUseTick {
		t.Fatalf("namespace observations changed horizon: before=%#v after=%#v", snap.Shadows[0], dup.Shadows)
	}
	if dup.Shadows[0].EmbeddingNamespace != "dense/qwen3:v1" || dup.Shadows[1].EmbeddingNamespace != "dense/qwen3:v2" {
		t.Fatalf("namespace order/attribution mismatch: %#v", dup.Shadows)
	}
	if err := h.AdmitDemand(shadowed); err != nil {
		t.Fatalf("demand shadowed: %v", err)
	}
	snap = h.Snapshot()
	if snap.ShadowPages != 2 || !snap.Shadows[0].Useful || !snap.Shadows[1].Useful {
		t.Fatalf("exact demand did not mark shadow useful: %#v", snap)
	}
	for i := 0; i < 2; i++ {
		p := testPage(fmt.Sprintf("P-use-%d", i), 1)
		if err := h.AdmitDemand(p); err != nil {
			t.Fatalf("admit use page %d: %v", i, err)
		}
	}
	if snap = h.Snapshot(); snap.ShadowPages != 0 {
		t.Fatalf("shadow did not expire after actual-use horizon: %#v", snap)
	}
}

func TestSemanticObservationRetainsFirstOrdinalOnDuplicate(t *testing.T) {
	h := NewHierarchy()
	p := testPage("P-shadow-ordinal", 1)
	obs := SemanticObservation{Tuple: p.Tuple(), EmbeddingNamespace: "dense/a", CandidateOrdinal: 5}
	if err := h.ObserveSemanticObservation(obs); err != nil {
		t.Fatalf("observe ordinal: %v", err)
	}
	obs.CandidateOrdinal = 9
	if err := h.ObserveSemanticObservation(obs); err != nil {
		t.Fatalf("duplicate ordinal: %v", err)
	}
	snap := h.Snapshot()
	if snap.ShadowPages != 1 || snap.Shadows[0].CandidateOrdinal != 5 || snap.Metrics.ShadowDuplicates != 1 {
		t.Fatalf("duplicate changed first ordinal/tick: %#v metrics=%#v", snap.Shadows, snap.Metrics)
	}
}

func TestShadowUsefulTraceOrderIsDeterministicAndSettlesOnce(t *testing.T) {
	run := func() []string {
		h := NewHierarchy()
		p := testPage("P-shadow-trace", 1)
		observations := []SemanticObservation{
			{Tuple: p.Tuple(), EmbeddingNamespace: "dense/a", CandidateOrdinal: 1},
			{Tuple: p.Tuple(), EmbeddingNamespace: "dense/b", CandidateOrdinal: 2},
		}
		for _, obs := range observations {
			if err := h.ObserveSemanticObservation(obs); err != nil {
				t.Fatalf("observe %#v: %v", obs, err)
			}
		}
		if err := h.AdmitDemand(p); err != nil {
			t.Fatalf("demand: %v", err)
		}
		if err := h.AdmitDemand(p); err != nil {
			t.Fatalf("second demand: %v", err)
		}
		var got []string
		for _, ev := range h.Trace() {
			if ev.Operation == "shadow_useful" {
				got = append(got, ev.EmbeddingNamespace)
			}
		}
		return got
	}
	want := []string{"dense/a", "dense/b"}
	for i := 0; i < 5; i++ {
		if got := run(); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d shadow_useful trace=%v, want %v", i, got, want)
		}
	}
}

func TestZeroShadowUseHorizonDisablesShadowInsertion(t *testing.T) {
	policy := testPolicy(4)
	policy.ShadowUseHorizon = 0
	h, err := NewHierarchyWithPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ObserveSemantic(testPage("P-zero-shadow", 1)); err != nil {
		t.Fatalf("observe with zero horizon: %v", err)
	}
	if snap := h.Snapshot(); snap.ShadowPages != 0 || snap.Metrics.ShadowObservations != 0 {
		t.Fatalf("zero horizon inserted shadow: %#v", snap)
	}
}

func TestShadowOutcomeCountersConservePendingTrials(t *testing.T) {
	policy := testPolicy(8)
	policy.ShadowUseHorizon = 1
	h, err := NewHierarchyWithPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	useful := testPage("P-shadow-useful", 1)
	if err := h.ObserveSemantic(useful); err != nil {
		t.Fatalf("observe useful: %v", err)
	}
	if err := h.AdmitDemand(useful); err != nil {
		t.Fatalf("demand useful: %v", err)
	}
	h.Shootdown(useful.ID)
	if metrics := h.Snapshot().Metrics; metrics.ShadowUseful != 1 || metrics.ShadowCensored != 0 {
		t.Fatalf("useful shadow later counted as censored: %#v", metrics)
	}

	expired := testPage("P-shadow-expired", 1)
	if err := h.ObserveSemantic(expired); err != nil {
		t.Fatalf("observe expired: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := h.AdmitDemand(testPage(fmt.Sprintf("P-expire-trigger-%d", i), 1)); err != nil {
			t.Fatalf("expire trigger %d: %v", i, err)
		}
	}
	metrics := h.Snapshot().Metrics
	if metrics.ShadowUnused != 1 {
		t.Fatalf("pending expired shadow not counted unused: %#v", metrics)
	}

	censored := testPage("P-shadow-censored", 1)
	if err := h.ObserveSemantic(censored); err != nil {
		t.Fatalf("observe censored: %v", err)
	}
	h.Shootdown(censored.ID)
	metrics = h.Snapshot().Metrics
	if metrics.ShadowCensored != 1 {
		t.Fatalf("pending shootdown not counted censored: %#v", metrics)
	}
	if metrics.ShadowUseful+metrics.ShadowUnused+metrics.ShadowCensored != metrics.ShadowObservations {
		t.Fatalf("shadow outcome counters not conserved: %#v", metrics)
	}
}

func TestDeterministicEvictionOrder(t *testing.T) {
	run := func() []string {
		h, err := NewHierarchyWithPolicy(testPolicy(2))
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 4; i++ {
			if err := h.AdmitDemand(testPage(fmt.Sprintf("P-det-%d", i), 1)); err != nil {
				t.Fatalf("admit %d: %v", i, err)
			}
		}
		snap := h.Snapshot()
		ids := make([]string, 0, len(snap.Resident))
		for _, resident := range snap.Resident {
			ids = append(ids, resident.Tuple.ID)
		}
		return ids
	}
	first := run()
	second := run()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("eviction order varied first=%v second=%v", first, second)
	}
}

func TestConcurrentAdmissionsAndUsesStayBounded(t *testing.T) {
	h, err := NewHierarchyWithPolicy(testPolicy(16))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				p := testPage(fmt.Sprintf("P-con-%d-%d", g, i), 1)
				if i%3 == 0 {
					_ = h.ObserveSemantic(p)
				}
				_ = h.AdmitDemand(p)
				h.LookupAndUse(p.ID)
			}
		}()
	}
	wg.Wait()
	snap := h.Snapshot()
	if snap.ResidentPages > h.policy.MaxResidentPages || snap.ResidentBytes > h.policy.MaxResidentBytes ||
		snap.GhostPages > h.policy.MaxGhostEntries || snap.ShadowPages > h.policy.MaxShadowEntries {
		t.Fatalf("snapshot exceeded bounds: %#v", snap)
	}
}

func testPolicy(residentPages int) Policy {
	p := DefaultPolicy()
	p.MaxResidentPages = residentPages
	if p.MaxPinnedPages > residentPages {
		p.MaxPinnedPages = residentPages
	}
	p.MaxResidentBytes = 64 * 1024
	p.MaxPinnedBytes = 16 * 1024
	p.MaxPageBytes = 8 * 1024
	p.MaxGhostBytes = 8 * 1024
	p.MaxShadowBytes = 8 * 1024
	return p
}

func testPage(id string, version uint64) *Page {
	return &Page{
		ID:              id,
		PageVersion:     version,
		SessionID:       "session-a",
		SourceDigest:    fmt.Sprintf("%064x", version),
		CompilerVersion: "compiler-v1",
		ScopeEpoch:      7,
		Scope:           types.ScopeTask,
		Branch:          "main",
		Commit:          "commit-a",
		Paths:           []string{"src/" + strings.ToLower(id) + ".go"},
		Refs:            []string{"E-" + id},
		Summary:         "summary " + id,
		Body:            "body " + id,
	}
}

func requireResident(t *testing.T, snap Snapshot, id string) ResidentSnapshot {
	t.Helper()
	for _, resident := range snap.Resident {
		if resident.Tuple.ID == id {
			return resident
		}
	}
	t.Fatalf("resident %q not found in %#v", id, snap)
	return ResidentSnapshot{}
}

func residencyContainsSnapshot(snap Snapshot, id string) bool {
	for _, resident := range snap.Resident {
		if resident.Tuple.ID == id {
			return true
		}
	}
	return false
}
