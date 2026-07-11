// Package tui is the WSMS demo frontend: a bubbletea two-pane terminal UI that
// drives the pi harness (left: chat) and renders the live WSMS memory hierarchy
// read from `wsms serve` (right: capsule / residency / status).
package tui

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"wsms/internal/pirpc"
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
	who  string // "you" | "pi" | "sys"
	text string
}

// Model is the bubbletea model. Zero panels render until the first viz poll and
// window-size message arrive.
type Model struct {
	width, height int

	input     textinput.Model
	lines     []line
	stream    strings.Builder // in-progress assistant text for the current turn
	streaming bool

	viz       vizState
	vizErr    bool
	agent     agent
	core      *coreClient
	vizCtx    context.Context
}

// message types
type (
	agentEventMsg  struct{ ev pirpc.Event }
	agentClosedMsg struct{}
	vizMsg         struct{ state vizState }
	vizErrMsg      struct{}
	vizTickMsg     struct{}
	promptResultMsg struct{ err error }
)

func newModel(a agent, core *coreClient) Model {
	in := textinput.New()
	in.Placeholder = "message pi… (Ctrl+C to quit)"
	in.Prompt = "> "
	in.Focus()
	return Model{input: in, agent: a, core: core, vizCtx: context.Background()}
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
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			if m.agent != nil {
				_ = m.agent.Close()
			}
			return m, tea.Quit
		case tea.KeyEnter:
			return m.submit()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case agentEventMsg:
		m.applyEvent(msg.ev)
		return m, waitForEvent(m.agent)

	case agentClosedMsg:
		m.lines = append(m.lines, line{"sys", "(agent exited)"})
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
			m.lines = append(m.lines, line{"sys", "prompt failed: " + msg.err.Error()})
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
	m.lines = append(m.lines, line{"you", text})
	if m.agent == nil {
		m.lines = append(m.lines, line{"sys", "(no agent configured — launch with --pi)"})
		return m, nil
	}
	m.streaming = true
	m.stream.Reset()
	return m, promptCmd(m.agent, text)
}

// applyEvent folds a streamed pi event into the chat. Field extraction is
// tolerant across event shapes; the flush points (assistant message_end,
// agent_settled) are what commit a turn's text to the transcript.
func (m *Model) applyEvent(ev pirpc.Event) {
	switch ev.Type {
	case "message_update":
		if txt := assistantText(ev.Raw); txt != "" {
			m.stream.Reset()
			m.stream.WriteString(txt)
		}
	case "message_end":
		if role(ev.Raw) == "assistant" {
			m.flushStream()
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
	m.lines = append(m.lines, line{"pi", m.stream.String()})
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

// --- tolerant event field extraction ---

// assistantText pulls the assistant's text out of a streamed event. pi's
// message events carry the whole message under "message"; fall back to flatter
// shapes so a schema drift degrades to less text rather than none.
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
