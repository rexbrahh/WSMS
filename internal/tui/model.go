// Package tui is the WSMS demo frontend: a bubbletea two-pane terminal UI that
// drives the pi harness (left: chat) and renders the live WSMS memory hierarchy
// read from `wsms serve` (right: capsule / residency / status).
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"wsms/internal/pirpc"
)

// focusMode is which pane keystrokes drive: the chat input, or the memory pane's
// collapsible sections.
type focusMode int

const (
	focusChat focusMode = iota
	focusMemory
)

// Memory-pane section indices, top to bottom.
const (
	secCapsule = iota
	secResidency
	secStatus
	nSections
)

// agent is the slice of the pi RPC client the TUI needs. An interface so tests
// can drive the model with a fake in place of a real child process.
type agent interface {
	Prompt(string) error
	Steer(string) error
	Abort() error
	Events() <-chan pirpc.Event
	Close() error
}

type line struct {
	who      string // "you" | "pi" | "tool" | "toolresult" | "sys"
	text     string
	head     string // optional label (e.g. the tool name for a "toolresult" line)
	expanded bool   // per-block override for a verbose "toolresult" line
}

// toolHit maps a clicked screen row back to the "toolresult" line that drew it.
type toolHit struct {
	y   int
	idx int
}

// hitTest is the clickable geometry of the last render, populated by View and
// consulted by the mouse handler (View has a value receiver, so it writes through
// this pointer; the single Update→View loop keeps it current).
type hitTest struct {
	leftWidth  int            // screen column where the memory pane begins
	memHeaders [nSections]int // screen row of each section header (-1 if absent)
	toolBlocks []toolHit
}

// Model is the bubbletea model. Zero panels render until the first viz poll and
// window-size message arrive.
type Model struct {
	width, height int

	input     textinput.Model
	lines     []line
	stream    strings.Builder // in-progress assistant text for the current turn
	streaming bool

	modelName string // pi model id, shown in the header (empty ⇒ omitted)

	focus        focusMode
	memSel       int             // selected memory section when focus == focusMemory
	memOpen      [nSections]bool // per-section expanded state
	expandOutput bool            // when set, verbose tool-result blocks render in full
	regions      *hitTest        // clickable geometry from the last render

	viz    vizState
	vizErr bool
	agent  agent
	core   *coreClient
	vizCtx context.Context
}

// message types
type (
	agentEventMsg   struct{ ev pirpc.Event }
	agentClosedMsg  struct{}
	vizMsg          struct{ state vizState }
	vizErrMsg       struct{}
	vizTickMsg      struct{}
	promptResultMsg struct{ err error }
)

func newModel(a agent, core *coreClient) Model {
	in := textinput.New()
	in.Placeholder = "message pi…"
	in.Prompt = "› "
	in.Focus()
	m := Model{input: in, agent: a, core: core, vizCtx: context.Background(), regions: &hitTest{}}
	m.memOpen[secCapsule] = true // the capsule is the point of the pane; the rest opens on demand
	return m
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, fetchVizCmd(m.core, m.vizCtx), scheduleViz()}
	if m.agent != nil {
		cmds = append(cmds, waitForEvent(m.agent))
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = leftWidth(msg.Width) - 4
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			if m.agent != nil {
				_ = m.agent.Close()
			}
			return m, tea.Quit
		}
		if msg.String() == "ctrl+o" { // inspect/collapse verbose tool output, from either pane
			m.expandOutput = !m.expandOutput
			return m, nil
		}
		if m.focus == focusMemory {
			return m.handleMemoryKey(msg)
		}
		switch msg.Type {
		case tea.KeyEsc:
			if m.agent != nil {
				_ = m.agent.Close()
			}
			return m, tea.Quit
		case tea.KeyTab:
			m.focus = focusMemory
			m.input.Blur()
			return m, nil
		case tea.KeyEnter:
			return m.submit()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case agentEventMsg:
		m.applyEvent(msg.ev)
		return m, waitForEvent(m.agent)

	case agentClosedMsg:
		m.lines = append(m.lines, line{who: "sys", text: "(agent exited)"})
		m.agent = nil
		return m, nil

	case vizMsg:
		m.viz, m.vizErr = msg.state, false
		return m, nil

	case vizErrMsg:
		m.vizErr = true
		return m, nil

	case vizTickMsg:
		return m, tea.Batch(fetchVizCmd(m.core, m.vizCtx), scheduleViz())

	case promptResultMsg:
		if msg.err != nil {
			m.lines = append(m.lines, line{who: "sys", text: "prompt failed: " + msg.err.Error()})
			m.streaming = false
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	m.input.Reset()
	m.lines = append(m.lines, line{who: "you", text: text})
	if m.agent == nil {
		m.lines = append(m.lines, line{who: "sys", text: "(no agent configured — launch with --pi)"})
		return m, nil
	}
	m.streaming = true
	m.stream.Reset()
	return m, promptCmd(m.agent, text)
}

// handleMemoryKey drives the memory pane while it holds focus: move the selection,
// expand/collapse the selected section, or hand focus back to the chat input.
// Keys are matched by name so a rune ('j'/'k') and its arrow equivalent share a
// path and the space key is caught however the terminal reports it.
func (m Model) handleMemoryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "tab":
		m.focus = focusChat
		m.input.Focus()
		return m, textinput.Blink
	case "up", "k":
		if m.memSel > 0 {
			m.memSel--
		}
	case "down", "j":
		if m.memSel < nSections-1 {
			m.memSel++
		}
	case " ", "space", "enter":
		m.memOpen[m.memSel] = !m.memOpen[m.memSel]
	}
	return m, nil
}

// handleMouse gives the keyboard-centric UI a few GUI niceties: a left-click
// picks the pane under the cursor, and lands on a live target it switches focus
// there — a memory section header toggles open/closed, a collapsed tool-result
// block expands in place. The clickable geometry comes from the last render via
// m.regions (populated by View); coordinates outside any target just move focus.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if msg.X >= m.regions.leftWidth {
		m.focus = focusMemory
		m.input.Blur()
		for s, y := range m.regions.memHeaders {
			if y >= 0 && msg.Y == y {
				m.memSel = s
				m.memOpen[s] = !m.memOpen[s]
				break
			}
		}
		return m, nil
	}
	m.focus = focusChat
	for _, t := range m.regions.toolBlocks {
		if msg.Y == t.y && t.idx < len(m.lines) {
			m.lines[t.idx].expanded = !m.lines[t.idx].expanded
			break
		}
	}
	m.input.Focus()
	return m, textinput.Blink
}

// applyEvent folds a streamed pi event into the chat.
//
// Streaming text is accumulated from message_update deltas
// (assistantMessageEvent.delta, gated on type "text_delta") — the only reliable
// incremental signal, since pi's in-place-mutated `message.content` snapshot is
// timing-dependent on the wire. message_end carries the finalized message, whose
// content is authoritative: we adopt it (repairing any deltas lost to
// backpressure) before flushing the turn. agent_settled is the final safety net.
func (m *Model) applyEvent(ev pirpc.Event) {
	switch ev.Type {
	case "message_start":
		if role(ev.Raw) == "assistant" {
			m.stream.Reset()
			m.streaming = true
		}
	case "message_update":
		m.stream.WriteString(deltaText(ev.Raw))
	case "message_end":
		switch role(ev.Raw) {
		case "assistant":
			for _, call := range toolCalls(ev.Raw) {
				m.lines = append(m.lines, line{who: "tool", text: call})
			}
			if full := assistantText(ev.Raw); full != "" {
				m.stream.Reset()
				m.stream.WriteString(full)
			}
			m.flushStream()
		case "toolResult":
			// The exact evidence pi returned for a tool call — rendered as its own
			// collapsible block, distinct from the model's prose about it.
			if name, body := toolResultMessage(ev.Raw); body != "" {
				m.lines = append(m.lines, line{who: "toolresult", head: name, text: body})
			}
		}
	case "agent_settled":
		m.streaming = false
		m.flushStream()
	}
}

func (m *Model) flushStream() {
	if m.stream.Len() == 0 {
		return
	}
	m.lines = append(m.lines, line{who: "pi", text: m.stream.String()})
	m.stream.Reset()
	m.streaming = false
}

// --- commands ---

func waitForEvent(a agent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-a.Events()
		if !ok {
			return agentClosedMsg{}
		}
		return agentEventMsg{ev: ev}
	}
}

func promptCmd(a agent, text string) tea.Cmd {
	return func() tea.Msg { return promptResultMsg{err: a.Prompt(text)} }
}

func fetchVizCmd(core *coreClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		st, err := core.fetchViz(ctx)
		if err != nil {
			return vizErrMsg{}
		}
		return vizMsg{state: st}
	}
}

func scheduleViz() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return vizTickMsg{} })
}

// --- event field extraction, verified against live pi RPC output ---

// deltaText pulls one incremental text token from a message_update event. pi
// nests the streaming delta under assistantMessageEvent and gates it by type;
// only "text_delta" is chat text ("thinking_delta" is reasoning, deliberately
// not surfaced here). Anything else yields "".
func deltaText(raw json.RawMessage) string {
	var env struct {
		AME struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	if env.AME.Type != "text_delta" {
		return ""
	}
	return env.AME.Delta
}

// assistantText pulls the assistant's finalized text out of a message_end event.
// pi carries the whole message under "message"; fall back to flatter shapes so a
// schema drift degrades to less text rather than none.
func assistantText(raw json.RawMessage) string {
	var env struct {
		Message json.RawMessage `json:"message"`
		Delta   string          `json:"delta"`
		Text    string          `json:"text"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	if t := textFromMessage(env.Message); t != "" {
		return t
	}
	if env.Text != "" {
		return env.Text
	}
	return env.Delta
}

func textFromMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// content may be a bare string or an array of typed parts.
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var asString string
	if json.Unmarshal(msg.Content, &asString) == nil {
		return asString
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// toolCalls extracts a display string per tool call in a finalized assistant
// message ("name(arg, …)"). It reads the authoritative message.content — where a
// tool call is a {type:"toolCall", name, arguments} block — not the cosmetic
// streaming deltas, so the affordance reflects what pi actually dispatched.
func toolCalls(raw json.RawMessage) []string {
	var env struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil
	}
	var parts []struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(env.Message.Content, &parts) != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		if p.Type == "toolCall" {
			out = append(out, p.Name+"("+compactArgs(p.Arguments)+")")
		}
	}
	return out
}

// toolResultMessage extracts (toolName, body) from a finalized toolResult message
// — pi emits one message_end per tool result, carrying the tool's exact output as
// text content. The body is the authoritative evidence the model received.
func toolResultMessage(raw json.RawMessage) (name, body string) {
	var env struct {
		Message struct {
			ToolName string          `json:"toolName"`
			Content  json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return "", ""
	}
	var asString string
	if json.Unmarshal(env.Message.Content, &asString) == nil {
		return env.Message.ToolName, asString
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(env.Message.Content, &parts) != nil {
		return env.Message.ToolName, ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return env.Message.ToolName, b.String()
}

// compactArgs renders a tool call's arguments object as its values in key order,
// e.g. {"id":"F1"} ⇒ "F1". Key order is sorted so the display is deterministic.
func compactArgs(raw json.RawMessage) string {
	var obj map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]string, 0, len(keys))
	for _, k := range keys {
		vals = append(vals, fmt.Sprintf("%v", obj[k]))
	}
	return strings.Join(vals, ", ")
}

func role(raw json.RawMessage) string {
	var env struct {
		Message struct {
			Role string `json:"role"`
		} `json:"message"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	if env.Message.Role != "" {
		return env.Message.Role
	}
	return env.Role
}
