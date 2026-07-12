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

func frame(w, h int, tweak func(*Model)) string {
	m := newModel(nil, nil)
	m.width, m.height = w, h
	m.modelName = "wsms-echo"
	m.input.Width = leftWidth(w) - 4
	m.viz = vizState{
		reachable: true, session: "demo-primary", capsule: longCapsule,
		residentPages: 3, maxPages: 64, hotPages: 0, coldPages: 1, pinnedPages: 2, ghostPages: 0,
	}
	if tweak != nil {
		tweak(&m)
	}
	return plain(m.View())
}

// The resting view: header identity + core status, both pane titles, the capsule
// expanded from its HEAD (the task/constraint/failure that matter most) with the
// tail elided, and residency/status collapsed to one-line summaries. Guards the
// regression where the whole pane was tailed and cut the head off the top.
func TestMemoryPaneRestingView(t *testing.T) {
	out := frame(110, 30, nil)

	mustContain := []string{
		"WSMS",            // header wordmark
		"demo-primary",    // header session id
		"wsms-echo",       // header model id
		"core",            // header core-status
		"chat",            // left pane title
		"memory",          // right pane title
		"CAPSULE",         // section label
		"TASK T1",         // capsule HEAD — the most important context
		"RESIDENCY",       // collapsed section
		"3/64",            // residency summary
		"STATUS",          // collapsed section
		"tab switch pane", // footer keybar
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("resting memory view missing %q:\n%s", want, out)
		}
	}

	// The capsule overflows the pane, so its tail is elided with an ellipsis.
	if !strings.Contains(out, "…") {
		t.Errorf("expected an ellipsis marking the elided capsule tail:\n%s", out)
	}
	// Collapsed sections must NOT leak their expanded detail into the resting view.
	if strings.Contains(out, "hot 0 · cold 1") {
		t.Errorf("residency detail should stay hidden until expanded:\n%s", out)
	}
}

// Focusing the pane and expanding a section reveals its detail; the footer swaps
// to the in-pane keys.
func TestMemoryPaneExpandRevealsDetail(t *testing.T) {
	out := frame(110, 30, func(m *Model) {
		m.focus = focusMemory
		m.memSel = secResidency
		m.memOpen[secResidency] = true
		m.memOpen[secStatus] = true
	})

	for _, want := range []string{
		"hot 0 · cold 1 · pin 2 · ghost 0", // residency detail
		"index ok",                         // status detail
		"embed ok",
		"space expand", // focused footer
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expanded memory view missing %q:\n%s", want, out)
		}
	}
}

func TestHeaderShowsCoreUnreachable(t *testing.T) {
	m := newModel(nil, nil)
	m.width, m.height = 100, 24
	m.viz = vizState{reachable: false}
	m.vizErr = true
	out := plain(m.View())
	if !strings.Contains(out, "core unreachable") {
		t.Errorf("header should flag an unreachable core:\n%s", out)
	}
	if !strings.Contains(out, "waiting for core") && !strings.Contains(out, "retrying") {
		t.Errorf("memory pane should show a waiting note when core is down:\n%s", out)
	}
}

func TestCapsuleAnchors(t *testing.T) {
	got := strings.Join(capsuleAnchors(longCapsule), " · ")
	want := "TASK T1 · C1 · F1 · A1 · NEXT"
	if got != want {
		t.Fatalf("capsuleAnchors = %q, want %q", got, want)
	}
	if a := capsuleAnchors("no recognizable headers here"); len(a) != 0 {
		t.Fatalf("unrecognized capsule should yield no anchors, got %v", a)
	}
}

// TestRenderDump prints frames for eyeballing the layout without a TTY.
// Gated behind WSMS_TUI_DUMP so it stays out of the normal suite.
func TestRenderDump(t *testing.T) {
	if os.Getenv("WSMS_TUI_DUMP") == "" {
		t.Skip("set WSMS_TUI_DUMP=1 to print TUI frames")
	}
	dumps := []struct {
		name  string
		tweak func(*Model)
	}{
		{"resting", nil},
		{"focused+expanded", func(m *Model) {
			m.focus = focusMemory
			m.memSel = secResidency
			m.memOpen[secResidency] = true
			m.memOpen[secStatus] = true
		}},
	}
	for _, sz := range []struct{ w, h int }{{110, 30}, {80, 24}} {
		for _, d := range dumps {
			fmt.Printf("\n========== %dx%d · %s ==========\n%s\n", sz.w, sz.h, d.name, frame(sz.w, sz.h, d.tweak))
		}
	}
}
