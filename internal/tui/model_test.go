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
