package tui

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func plain(s string) string { return ansi.ReplaceAllString(s, "") }

// A capsule long enough to overflow the memory pane, so the truncation policy is
// actually exercised.
const longCapsule = "<working_state>\n" +
	"TASK T1: Preserve exact failure evidence across restart.\n" +
	"PHASE: Debugging.\nBRANCH: main.\nDIRTY FILES: src/runtime/stream.go.\n\n" +
	"HARD CONSTRAINT C1:\ndo not rewrite transport layer\n\n" +
	"LAST FAILURE F1:\nCommand: `go test ./runtime -run TestCancelStream`\n" +
	"Exit: 1\nError: \"error: stream goroutine still blocked\"\n" +
	"Likely file area: src/runtime/stream.go:118\n\n" +
	"AVOID A1:\nretrying the failed transport rewrite; see F1\n\n" +
	"NEXT:\nInspect src/runtime/stream.go:118-176\n" +
	"Question: does the waiter exit after cancellation?\n\n" +
	"If details are missing, request a page by ID instead of guessing.\n</working_state>\n"

func memoryFrame(w, h int) string {
	m := newModel(nil, nil)
	m.width, m.height = w, h
	m.input.Width = leftWidth(w) - 4
	m.viz = vizState{
		reachable: true, capsule: longCapsule,
		residentPages: 3, maxPages: 64, hotPages: 0, coldPages: 1, pinnedPages: 2, ghostPages: 0,
	}
	return plain(m.View())
}

// When the capsule overflows, the pane must still show its title, the CAPSULE
// label, the capsule HEAD (not the tail), and the pinned residency/status block.
// This guards against the regression where fitHeight tailed the whole pane and
// cut the title + head off the top.
func TestMemoryPaneKeepsHeadAndPinnedBlock(t *testing.T) {
	out := memoryFrame(110, 30)

	mustContain := []string{
		"memory",    // pane title
		"CAPSULE",   // section label
		"TASK T1",   // capsule HEAD — the most important context
		"RESIDENCY", // pinned block, always visible
		"resident 3/64",
		"STATUS",
		"index ok",
		"embed ok",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("memory pane missing %q; overflow truncation hid it:\n%s", want, out)
		}
	}

	// The capsule tail should be elided (its last lines dropped for the pinned
	// block), so the whole thing does not fit — sanity-check the ellipsis.
	if !strings.Contains(out, "…") {
		t.Errorf("expected an ellipsis marking the elided capsule tail:\n%s", out)
	}
}

func TestMemoryPaneUnreachable(t *testing.T) {
	m := newModel(nil, nil)
	m.width, m.height = 100, 24
	m.input.Width = leftWidth(100) - 4
	m.viz = vizState{reachable: false}
	out := plain(m.View())
	if !strings.Contains(out, "waiting for core") {
		t.Errorf("unreachable core should show a waiting note:\n%s", out)
	}
}

// TestRenderDump prints frames for eyeballing the layout without a TTY.
// Gated behind WSMS_TUI_DUMP so it stays out of the normal suite.
func TestRenderDump(t *testing.T) {
	if os.Getenv("WSMS_TUI_DUMP") == "" {
		t.Skip("set WSMS_TUI_DUMP=1 to print TUI frames")
	}
	for _, sz := range []struct{ w, h int }{{110, 30}, {80, 24}} {
		fmt.Printf("\n========== %dx%d ==========\n%s\n", sz.w, sz.h, memoryFrame(sz.w, sz.h))
	}
}
