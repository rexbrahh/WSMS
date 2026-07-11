package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The body mirrors serve's /viz/state: lowercase top-level keys, PascalCase
// nested fields (the domain structs carry no json tags).
const vizBody = `{
	"capsule": "<working_state>\nTASK T1: demo.\n</working_state>",
	"residency": {"ResidentPages": 12, "HotPages": 8, "ColdPages": 4, "PinnedPages": 1, "GhostPages": 2, "Policy": {"MaxResidentPages": 64}},
	"maintenance": {"Category": "repair", "Degraded": true, "Pending": 3},
	"embedding": {"Degraded": false, "Category": ""}
}`

func TestFetchVizDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/viz/state" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vizBody))
	}))
	defer srv.Close()

	st, err := newCoreClient(srv.URL, "").fetchViz(context.Background())
	if err != nil {
		t.Fatalf("fetchViz: %v", err)
	}
	if !st.reachable {
		t.Fatal("reachable should be true")
	}
	if st.residentPages != 12 || st.maxPages != 64 || st.hotPages != 8 || st.pinnedPages != 1 {
		t.Fatalf("residency decode wrong: %+v", st)
	}
	if !st.maintDegraded || st.maintCategory != "repair" || st.maintPending != 3 {
		t.Fatalf("maintenance decode wrong: %+v", st)
	}
	if !strings.Contains(st.capsule, "TASK T1") {
		t.Fatalf("capsule decode wrong: %q", st.capsule)
	}
}

func TestFetchVizErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := newCoreClient(srv.URL, "").fetchViz(context.Background()); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestViewRendersPanels(t *testing.T) {
	m := newModel(nil, newCoreClient("http://127.0.0.1:1", ""))
	m.width, m.height = 100, 24
	m.viz = vizState{reachable: true, capsule: "TASK T1: demo", residentPages: 12, maxPages: 64}

	out := m.View()
	for _, want := range []string{"WSMS", "memory", "CAPSULE", "RESIDENCY", "STATUS", "resident 12/64"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q\n%s", want, out)
		}
	}
}
