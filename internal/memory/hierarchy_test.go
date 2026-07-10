package memory

import (
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
	h.PutL2(&Page{ID: "F1", Branch: "main", Commit: "a", Paths: []string{"src/a.go"}})
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
}
