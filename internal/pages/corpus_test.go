package pages_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wsms/internal/pages"
)

// corpusRoot resolves testdata/pages/corpus relative to this source file so
// tests work regardless of the package under test's working directory.
func corpusRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/pages/corpus_test.go → repo root → testdata/pages/corpus
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "testdata", "pages", "corpus"))
}

type corpusExpected struct {
	CompilerVersion string             `json:"compiler_version"`
	SessionID       string             `json:"session_id"`
	Pages           []corpusPageExpect `json:"pages"`
}

type corpusPageExpect struct {
	Kind         pages.PageKind `json:"kind"`
	PageID       pages.PageID   `json:"page_id"`
	Version      uint64         `json:"page_version"`
	SearchText   string         `json:"search_text"`
	Summary      string         `json:"summary"`
	Trust        pages.Trust    `json:"trust"`
	SourceSeqMin int64          `json:"source_seq_min"`
	SourceSeqMax int64          `json:"source_seq_max"`
}

type corpusQuery struct {
	ID             string   `json:"id"`
	Label          string   `json:"label"` // positive | wrong_branch | invalidated | poisoned | true_no_answer
	Text           string   `json:"text"`
	ExpectedKinds  []string `json:"expected_kinds"`
	ForbiddenKinds []string `json:"forbidden_kinds"`
	Notes          string   `json:"notes,omitempty"`
}

func TestFrozenTransportFixCorpus(t *testing.T) {
	root := filepath.Join(corpusRoot(t), "transport_fix")
	expectedPath := filepath.Join(root, "expected_pages.json")
	queriesPath := filepath.Join(root, "queries.json")

	s := openPageSession(t, "corpus-transport")
	muts := driveTransportFixStream(t, s)

	got := corpusExpected{
		CompilerVersion: string(pages.CurrentCompilerVersion),
		SessionID:       "corpus-transport",
		Pages:           make([]corpusPageExpect, 0, len(muts)),
	}
	for _, mut := range muts {
		p := mut.Page
		got.Pages = append(got.Pages, corpusPageExpect{
			Kind: p.Kind, PageID: p.ID, Version: uint64(p.Version),
			SearchText: p.SearchText, Summary: p.Summary, Trust: p.Trust,
			SourceSeqMin: p.SourceSeqMin, SourceSeqMax: p.SourceSeqMax,
		})
	}

	if os.Getenv("WSMS_UPDATE_CORPUS") == "1" {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(expectedPath, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", expectedPath)
	}

	raw, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run WSMS_UPDATE_CORPUS=1 go test ./internal/pages -run TestFrozenTransportFixCorpus)", expectedPath, err)
	}
	var want corpusExpected
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if want.CompilerVersion != got.CompilerVersion {
		t.Fatalf("compiler version golden=%q got=%q", want.CompilerVersion, got.CompilerVersion)
	}
	if len(want.Pages) != len(got.Pages) {
		t.Fatalf("page count golden=%d got=%d", len(want.Pages), len(got.Pages))
	}
	for i := range want.Pages {
		w, g := want.Pages[i], got.Pages[i]
		if w != g {
			t.Fatalf("page[%d] golden=%+v got=%+v", i, w, g)
		}
	}

	qraw, err := os.ReadFile(queriesPath)
	if err != nil {
		t.Fatal(err)
	}
	var queries []corpusQuery
	if err := json.Unmarshal(qraw, &queries); err != nil {
		t.Fatal(err)
	}
	if len(queries) < 5 {
		t.Fatalf("expected at least 5 labeled queries, got %d", len(queries))
	}

	labels := map[string]int{}
	for _, q := range queries {
		labels[q.Label]++
		switch q.Label {
		case "positive":
			if len(q.ExpectedKinds) == 0 {
				t.Fatalf("positive query %s needs expected_kinds", q.ID)
			}
			if !corpusHasKindMatch(muts, q.ExpectedKinds, q.Text) {
				t.Fatalf("positive query %s text %q did not match expected kinds %v in compiled pages", q.ID, q.Text, q.ExpectedKinds)
			}
		case "true_no_answer":
			if len(q.ExpectedKinds) != 0 {
				t.Fatalf("true_no_answer query %s must have empty expected_kinds", q.ID)
			}
			if corpusTextMatchesAny(muts, q.Text) {
				t.Fatalf("true_no_answer query %s unexpectedly matched a compiled page", q.ID)
			}
		case "poisoned":
			// Poisoned queries document that model prose must not become a
			// constraint page; the stream already proved assistant emits zero
			// pages. Label must still be present for later retrievers.
			if len(q.ForbiddenKinds) == 0 {
				t.Fatalf("poisoned query %s needs forbidden_kinds", q.ID)
			}
		case "wrong_branch", "invalidated":
			// Labels for Phase 7B+ hard filters; require well-formed entries.
			if strings.TrimSpace(q.Text) == "" {
				t.Fatalf("query %s empty text", q.ID)
			}
		default:
			t.Fatalf("unknown query label %q on %s", q.Label, q.ID)
		}
	}
	for _, required := range []string{"positive", "wrong_branch", "invalidated", "poisoned", "true_no_answer"} {
		if labels[required] == 0 {
			t.Fatalf("corpus missing label %s", required)
		}
	}
}

func corpusHasKindMatch(muts []pages.PageMutation, kinds []string, text string) bool {
	needle := strings.ToLower(text)
	kindSet := map[string]bool{}
	for _, k := range kinds {
		kindSet[k] = true
	}
	for _, mut := range muts {
		if !kindSet[string(mut.Page.Kind)] {
			continue
		}
		hay := strings.ToLower(mut.Page.SearchText + " " + mut.Page.Summary)
		if strings.Contains(hay, needle) {
			return true
		}
		// Allow multi-word partial: every significant token appears.
		tokens := strings.Fields(needle)
		ok := len(tokens) > 0
		for _, tok := range tokens {
			if len(tok) < 4 {
				continue
			}
			if !strings.Contains(hay, tok) {
				ok = false
				break
			}
		}
		if ok && len(tokens) > 0 {
			return true
		}
	}
	return false
}

func corpusTextMatchesAny(muts []pages.PageMutation, text string) bool {
	needle := strings.ToLower(text)
	for _, mut := range muts {
		hay := strings.ToLower(mut.Page.SearchText + " " + mut.Page.Summary)
		if strings.Contains(hay, needle) {
			return true
		}
	}
	return false
}
