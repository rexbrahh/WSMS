package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"wsms/internal/pirpc"
)

type fakeAgent struct {
	prompts []string
	events  chan pirpc.Event
}

func newFakeAgent() *fakeAgent { return &fakeAgent{events: make(chan pirpc.Event, 8)} }

func (f *fakeAgent) Prompt(s string) error      { f.prompts = append(f.prompts, s); return nil }
func (f *fakeAgent) Steer(string) error         { return nil }
func (f *fakeAgent) Abort() error               { return nil }
func (f *fakeAgent) Events() <-chan pirpc.Event { return f.events }
func (f *fakeAgent) Close() error               { return nil }

func step(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

func TestSubmitAppendsAndPrompts(t *testing.T) {
	fake := newFakeAgent()
	m := newModel(fake, newCoreClient("http://127.0.0.1:1", ""))
	m.input.SetValue("hello pi")

	m, cmd := step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.lines) != 1 || m.lines[0].who != "you" || m.lines[0].text != "hello pi" {
		t.Fatalf("expected a 'you' line, got %+v", m.lines)
	}
	if !m.streaming {
		t.Fatal("expected streaming to start")
	}
	if cmd == nil {
		t.Fatal("expected a prompt command")
	}
	if _, ok := cmd().(promptResultMsg); !ok {
		t.Fatal("command should produce a promptResultMsg")
	}
	if len(fake.prompts) != 1 || fake.prompts[0] != "hello pi" {
		t.Fatalf("agent should have received the prompt, got %v", fake.prompts)
	}
}

func TestSubmitWithoutAgent(t *testing.T) {
	m := newModel(nil, newCoreClient("http://127.0.0.1:1", ""))
	m.input.SetValue("hi")
	m, _ = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.lines) != 2 || m.lines[1].who != "sys" {
		t.Fatalf("expected a sys notice about no agent, got %+v", m.lines)
	}
}

// Envelopes below mirror live pi RPC output captured against the real harness:
// incremental text arrives as message_update.assistantMessageEvent deltas, and
// message_end carries the authoritative finalized message.
func TestApplyEventStreamsAndFlushes(t *testing.T) {
	m := newModel(newFakeAgent(), newCoreClient("http://127.0.0.1:1", ""))

	start := json.RawMessage(`{"type":"message_start","message":{"role":"assistant","content":[]}}`)
	m, _ = step(m, agentEventMsg{ev: pirpc.Event{Type: "message_start", Raw: start}})
	if !m.streaming {
		t.Fatal("assistant message_start should begin streaming")
	}

	for _, delta := range []string{"Hello ", "world"} {
		raw := json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"` + delta + `"}}`)
		m, _ = step(m, agentEventMsg{ev: pirpc.Event{Type: "message_update", Raw: raw}})
	}
	if m.stream.String() != "Hello world" {
		t.Fatalf("stream = %q, want accumulated deltas", m.stream.String())
	}

	// text_start / text_end updates carry no delta and must not corrupt the buffer.
	nonDelta := json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"text_end","content":"Hello world"}}`)
	m, _ = step(m, agentEventMsg{ev: pirpc.Event{Type: "message_update", Raw: nonDelta}})
	if m.stream.String() != "Hello world" {
		t.Fatalf("non-delta update changed stream: %q", m.stream.String())
	}

	end := json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Hello world"}]}}`)
	m, _ = step(m, agentEventMsg{ev: pirpc.Event{Type: "message_end", Raw: end}})
	if len(m.lines) != 1 || m.lines[0].who != "pi" || m.lines[0].text != "Hello world" {
		t.Fatalf("expected flushed pi line, got %+v", m.lines)
	}

	settled := json.RawMessage(`{"type":"agent_settled"}`)
	m, _ = step(m, agentEventMsg{ev: pirpc.Event{Type: "agent_settled", Raw: settled}})
	if m.streaming {
		t.Fatal("agent_settled should stop streaming")
	}
	if len(m.lines) != 1 {
		t.Fatalf("agent_settled must not duplicate the flushed line, got %+v", m.lines)
	}
}

// A tool-call turn carries no assistant text — only a toolCall block in the
// finalized content. It must surface as a distinct "tool" line, not vanish.
func TestToolCallAffordance(t *testing.T) {
	m := newModel(newFakeAgent(), newCoreClient("http://127.0.0.1:1", ""))
	end := json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"wsms_read_page","arguments":{"id":"F1"}}]}}`)
	m, _ = step(m, agentEventMsg{ev: pirpc.Event{Type: "message_end", Raw: end}})
	if len(m.lines) != 1 || m.lines[0].who != "tool" || m.lines[0].text != "wsms_read_page(F1)" {
		t.Fatalf("expected a tool-call line, got %+v", m.lines)
	}
}

// A tool result is verbose evidence: it renders as its own "↳ toolName" block,
// showing the first few body lines and eliding the rest behind a "+N more" marker
// until inspected with ^O.
func TestToolResultCollapsesUntilInspected(t *testing.T) {
	m := newModel(nil, newCoreClient("http://127.0.0.1:1", ""))
	m.width, m.height = 100, 24
	// Five body lines, so the PREVIEW=3 policy shows the head and hides two.
	end := json.RawMessage(`{"type":"message_end","message":{"role":"toolResult","toolName":"wsms_read_page","content":[{"type":"text","text":"LAST FAILURE F1:\nCommand: go test ./runtime\nExit: 1\nError: stream goroutine still blocked\nLikely area: stream.go:118"}]}}`)
	m, _ = step(m, agentEventMsg{ev: pirpc.Event{Type: "message_end", Raw: end}})
	if len(m.lines) != 1 || m.lines[0].who != "toolresult" || m.lines[0].head != "wsms_read_page" {
		t.Fatalf("expected a toolresult line, got %+v", m.lines)
	}

	out := plain(m.View())
	if !strings.Contains(out, "LAST FAILURE F1") || !strings.Contains(out, "+2 more") {
		t.Fatalf("collapsed result should preview the head with a +N marker:\n%s", out)
	}
	if strings.Contains(out, "stream.go:118") {
		t.Fatalf("collapsed result must hide its tail until inspected:\n%s", out)
	}

	m, _ = step(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if !m.expandOutput {
		t.Fatal("ctrl+o should toggle output expansion on")
	}
	if out := plain(m.View()); !strings.Contains(out, "stream.go:118") {
		t.Fatalf("inspected result should show the full body:\n%s", out)
	}
}

func TestFocusToggleAndExpand(t *testing.T) {
	m := newModel(nil, newCoreClient("http://127.0.0.1:1", ""))
	m.width, m.height = 100, 24
	m.viz = vizState{reachable: true, capsule: "TASK T1", residentPages: 3, maxPages: 64, coldPages: 1, pinnedPages: 2}

	m, _ = step(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != focusMemory {
		t.Fatal("tab should move focus to the memory pane")
	}
	m, _ = step(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.memSel != secResidency {
		t.Fatalf("down should select residency, got section %d", m.memSel)
	}
	m, _ = step(m, tea.KeyMsg{Type: tea.KeySpace})
	if !m.memOpen[secResidency] {
		t.Fatal("space should expand the selected section")
	}
	if out := plain(m.View()); !strings.Contains(out, "hot 0") {
		t.Fatalf("expanded residency should show its breakdown:\n%s", out)
	}
	m, _ = step(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusChat {
		t.Fatal("esc should return focus to the chat input")
	}
}

// A left-click on a collapsed tool-result block expands just that block, without
// flipping the global expand-all toggle. The click geometry comes from regions,
// which View populates on each render.
func TestMouseTogglesToolBlock(t *testing.T) {
	m := newModel(nil, newCoreClient("http://127.0.0.1:1", ""))
	m.width, m.height = 100, 24
	end := json.RawMessage(`{"type":"message_end","message":{"role":"toolResult","toolName":"wsms_read_page","content":[{"type":"text","text":"L1\nL2\nL3\nL4\nsentinel:stream.go:118"}]}}`)
	m, _ = step(m, agentEventMsg{ev: pirpc.Event{Type: "message_end", Raw: end}})

	_ = m.View() // populate m.regions
	if len(m.regions.toolBlocks) == 0 {
		t.Fatal("expected the tool-result block to register a click target")
	}
	tb := m.regions.toolBlocks[0]
	if tb.idx != 0 {
		t.Fatalf("tool block should own line 0, got %d", tb.idx)
	}

	m, _ = step(m, tea.MouseMsg{X: 1, Y: tb.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.lines[0].expanded {
		t.Fatal("clicking the block should expand it")
	}
	if m.expandOutput {
		t.Fatal("a per-block click must not flip the global expand-all toggle")
	}
	if out := plain(m.View()); !strings.Contains(out, "stream.go:118") {
		t.Fatalf("expanded block should show its full body:\n%s", out)
	}

	// Clicking again collapses it back.
	m, _ = step(m, tea.MouseMsg{X: 1, Y: tb.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.lines[0].expanded {
		t.Fatal("a second click should collapse the block")
	}
}

// A left-click in the memory pane switches focus there; landing on a section
// header also toggles it open.
func TestMouseSwitchesPaneAndTogglesSection(t *testing.T) {
	m := newModel(nil, newCoreClient("http://127.0.0.1:1", ""))
	m.width, m.height = 100, 24
	m.viz = vizState{reachable: true, capsule: "TASK T1", residentPages: 3, maxPages: 64, coldPages: 1, pinnedPages: 2}

	_ = m.View() // populate m.regions
	y := m.regions.memHeaders[secResidency]
	if y < 0 {
		t.Fatal("residency header should have a click target when the core is reachable")
	}

	m, _ = step(m, tea.MouseMsg{X: m.regions.leftWidth + 1, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focus != focusMemory {
		t.Fatal("clicking the memory pane should move focus there")
	}
	if m.memSel != secResidency || !m.memOpen[secResidency] {
		t.Fatalf("clicking the residency header should select and open it, got sel=%d open=%v", m.memSel, m.memOpen)
	}

	// A click back in the chat pane returns focus to the input.
	m, _ = step(m, tea.MouseMsg{X: 1, Y: 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focus != focusChat {
		t.Fatal("clicking the chat pane should return focus to chat")
	}
}

// In chat focus, esc still quits (unchanged behaviour); ctrl+c always quits.
func TestChatEscQuits(t *testing.T) {
	m := newModel(newFakeAgent(), newCoreClient("http://127.0.0.1:1", ""))
	_, cmd := step(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in chat focus should return a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("esc in chat focus should quit")
	}
}

func TestDeltaText(t *testing.T) {
	cases := map[string]string{
		`{"assistantMessageEvent":{"type":"text_delta","delta":"hi"}}`:      "hi",
		`{"assistantMessageEvent":{"type":"thinking_delta","delta":"hmm"}}`: "", // reasoning, not chat
		`{"assistantMessageEvent":{"type":"text_start"}}`:                   "",
		`{"assistantMessageEvent":{"type":"text_end","content":"hi"}}`:      "",
		`{"type":"agent_settled"}`:                                          "",
	}
	for raw, want := range cases {
		if got := deltaText(json.RawMessage(raw)); got != want {
			t.Errorf("deltaText(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestVizMsgUpdatesPanels(t *testing.T) {
	m := newModel(nil, newCoreClient("http://127.0.0.1:1", ""))
	m, _ = step(m, vizMsg{state: vizState{reachable: true, capsule: "TASK T1", residentPages: 3, maxPages: 64}})
	if !m.viz.reachable || m.viz.residentPages != 3 {
		t.Fatalf("viz not applied: %+v", m.viz)
	}
	m, _ = step(m, vizErrMsg{})
	if !m.vizErr {
		t.Fatal("vizErr should be set")
	}
}

func TestAssistantTextShapes(t *testing.T) {
	cases := map[string]string{
		`{"message":{"content":[{"type":"text","text":"ab"},{"type":"thinking"},{"type":"text","text":"cd"}]}}`: "abcd",
		`{"message":{"content":"bare string"}}`: "bare string",
		`{"text":"flat"}`:                       "flat",
		`{"delta":"tok"}`:                       "tok",
		`{"type":"noise"}`:                      "",
	}
	for raw, want := range cases {
		if got := assistantText(json.RawMessage(raw)); got != want {
			t.Errorf("assistantText(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestRoleExtraction(t *testing.T) {
	if role(json.RawMessage(`{"message":{"role":"assistant"}}`)) != "assistant" {
		t.Fatal("nested role")
	}
	if role(json.RawMessage(`{"role":"user"}`)) != "user" {
		t.Fatal("flat role")
	}
}

func TestWordWrapAndGauge(t *testing.T) {
	wrapped := wordWrap("the quick brown fox", 9)
	for _, l := range wrapped {
		if len(l) > 9 {
			t.Fatalf("line exceeds width: %q", l)
		}
	}
	if strings.Join(wrapped, " ") != "the quick brown fox" {
		t.Fatalf("wrap changed content: %v", wrapped)
	}
	if g := gauge(32, 64, 24); !strings.Contains(g, "▓") || !strings.Contains(g, "░") {
		t.Fatalf("gauge should be half full: %q", g)
	}
	if gauge(0, 0, 24) != "" {
		t.Fatal("gauge with zero max should be empty")
	}
}

func TestWaitForEventClosed(t *testing.T) {
	fake := newFakeAgent()
	close(fake.events)
	if _, ok := waitForEvent(fake)().(agentClosedMsg); !ok {
		t.Fatal("closed event channel should yield agentClosedMsg")
	}
}
