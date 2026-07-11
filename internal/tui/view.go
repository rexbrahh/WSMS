package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	paneBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("62"))
	youStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	piStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("70"))
	sysStyle   = lipgloss.NewStyle().Faint(true)
	labelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("70"))
)

// leftWidth gives the chat pane ~60% of the terminal, clamped so both panes stay
// usable on narrow terminals.
func leftWidth(total int) int {
	w := total * 6 / 10
	if w < 30 {
		w = 30
	}
	if w > total-24 {
		w = total - 24
	}
	if w < 20 {
		w = 20
	}
	return w
}

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting WSMS TUI…"
	}
	lw := leftWidth(m.width)
	rw := m.width - lw

	innerH := m.height - 2 // pane borders
	left := paneBorder.Width(lw - 2).Height(innerH).Render(m.renderChat(lw-2, innerH))
	right := paneBorder.Width(rw - 2).Height(innerH).Render(m.renderMemory(rw-2, innerH))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (m Model) renderChat(w, h int) string {
	rows := []string{titleStyle.Render("WSMS · pi")}

	body := make([]string, 0, len(m.lines)+1)
	for _, ln := range m.lines {
		body = append(body, renderLine(ln, w)...)
	}
	if m.streaming && m.stream.Len() > 0 {
		body = append(body, renderLine(line{"pi", m.stream.String() + "▍"}, w)...)
	}

	// transcript occupies everything between the title and the input line.
	transcriptH := h - 2
	if transcriptH < 1 {
		transcriptH = 1
	}
	rows = append(rows, tail(body, transcriptH)...)
	for len(rows) < h-1 {
		rows = append(rows, "")
	}
	rows = append(rows, m.input.View())
	return strings.Join(rows, "\n")
}

func renderLine(ln line, w int) []string {
	var style lipgloss.Style
	var tag string
	switch ln.who {
	case "you":
		style, tag = youStyle, "you> "
	case "pi":
		style, tag = piStyle, "pi>  "
	default:
		style, tag = sysStyle, ""
	}
	wrapped := wordWrap(tag+ln.text, w)
	out := make([]string, len(wrapped))
	for i, s := range wrapped {
		out[i] = style.Render(s)
	}
	return out
}

func (m Model) renderMemory(w, h int) string {
	rows := []string{titleStyle.Render("memory")}

	if !m.viz.reachable {
		note := "core unreachable"
		if m.vizErr {
			note = "core unreachable (retrying)"
		} else {
			note = "waiting for core…"
		}
		rows = append(rows, "", sysStyle.Render(note))
		return fitHeight(rows, h)
	}

	rows = append(rows, labelStyle.Render("CAPSULE"))
	capsule := strings.TrimSpace(m.viz.capsule)
	if capsule == "" {
		capsule = "(empty)"
	}
	rows = append(rows, wordWrap(capsule, w)...)

	rows = append(rows, "", labelStyle.Render("RESIDENCY"))
	rows = append(rows, fmt.Sprintf("resident %d/%d", m.viz.residentPages, m.viz.maxPages))
	rows = append(rows, gauge(m.viz.residentPages, m.viz.maxPages, w))
	rows = append(rows, fmt.Sprintf("hot %d · cold %d · pin %d · ghost %d",
		m.viz.hotPages, m.viz.coldPages, m.viz.pinnedPages, m.viz.ghostPages))

	rows = append(rows, "", labelStyle.Render("STATUS"))
	rows = append(rows, "index "+statusWord(m.viz.maintDegraded, m.viz.maintCategory, m.viz.maintPending))
	rows = append(rows, "embed "+statusWord(m.viz.embedDegraded, m.viz.embedCategory, 0))

	return fitHeight(rows, h)
}

func statusWord(degraded bool, category string, pending int) string {
	if degraded {
		label := "degraded"
		if category != "" {
			label += " (" + category + ")"
		}
		if pending > 0 {
			label += fmt.Sprintf(" · %d pending", pending)
		}
		return warnStyle.Render(label)
	}
	label := "ok"
	if pending > 0 {
		label += fmt.Sprintf(" · %d pending", pending)
	}
	return okStyle.Render(label)
}

func gauge(n, max, w int) string {
	if max <= 0 || w <= 2 {
		return ""
	}
	width := w
	if width > 24 {
		width = 24
	}
	filled := 0
	if n > 0 {
		filled = n * width / max
		if filled > width {
			filled = width
		}
	}
	return strings.Repeat("▓", filled) + strings.Repeat("░", width-filled)
}

// wordWrap wraps s to width w, preserving explicit newlines.
func wordWrap(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case len(line)+1+len(word) <= w:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
			for len(line) > w { // a single word longer than the width
				out = append(out, line[:w])
				line = line[w:]
			}
		}
		out = append(out, line)
	}
	return out
}

func tail(rows []string, n int) []string {
	if len(rows) <= n {
		return rows
	}
	return rows[len(rows)-n:]
}

func fitHeight(rows []string, h int) string {
	rows = tail(rows, h)
	for len(rows) < h {
		rows = append(rows, "")
	}
	return strings.Join(rows, "\n")
}
