package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	paneBorder  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
	focusBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("250"))
	youStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("153")) // user — pale pastel blue
	piStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("231")) // model output — white
	mutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245")) // tool/system output — grey
	sysStyle    = lipgloss.NewStyle().Faint(true)
	labelStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	dotStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	selStyle    = lipgloss.NewStyle().Reverse(true)
	headStyle   = lipgloss.NewStyle().Faint(true)
	wordmark    = lipgloss.NewStyle().Bold(true)
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

	innerH := m.height - 2 - 2 // header + footer, then pane borders
	if innerH < 1 {
		innerH = 1
	}

	lb, rb := paneBorder, paneBorder
	if m.focus == focusMemory {
		rb = focusBorder
	} else {
		lb = focusBorder
	}
	chat, chatOwn := m.renderChat(lw-2, innerH)
	mem, memHdr := m.renderMemory(rw-2, innerH)
	left := lb.Width(lw - 2).Height(innerH).Render(chat)
	right := rb.Width(rw - 2).Height(innerH).Render(mem)
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	// Record where clickable things landed for the mouse handler. A pane content
	// row k sits at screen row 2+k (header bar + the pane's top border), and the
	// memory pane starts at screen column lw (the left box spans columns 0..lw-1).
	reg := hitTest{leftWidth: lw}
	for s := range reg.memHeaders {
		reg.memHeaders[s] = -1
	}
	for s, row := range memHdr {
		if row >= 0 {
			reg.memHeaders[s] = 2 + row
		}
	}
	for row, idx := range chatOwn {
		if idx >= 0 {
			reg.toolBlocks = append(reg.toolBlocks, toolHit{y: 2 + row, idx: idx})
		}
	}
	*m.regions = reg

	return strings.Join([]string{m.renderHeader(m.width), body, m.renderFooter(m.width)}, "\n")
}

// renderHeader is the top status bar: identity on the left, core reachability on
// the right. The core dot is the only element that takes a non-neutral color, and
// only when the core is unreachable — colour marks trouble, not the resting state.
func (m Model) renderHeader(w int) string {
	reachable := m.viz.reachable && !m.vizErr

	meta := ""
	if m.viz.session != "" {
		meta += " · " + m.viz.session
	}
	if m.modelName != "" {
		meta += " · " + m.modelName
	}
	coreText := " core"
	if !reachable {
		coreText = " core unreachable"
	}

	leftPlain := " WSMS" + meta
	rightPlain := "●" + coreText + " "
	pad := w - len([]rune(leftPlain)) - len([]rune(rightPlain))
	if pad < 0 {
		pad = 0
	}

	coreDot := dotStyle
	if !reachable {
		coreDot = warnStyle
	}
	left := wordmark.Render(" WSMS") + headStyle.Render(meta)
	right := coreDot.Render("●") + headStyle.Render(coreText+" ")
	return left + strings.Repeat(" ", pad) + right
}

// renderFooter is the bottom keybar; its contents track the focused pane so the
// available keys are always the ones that do something here.
func (m Model) renderFooter(w int) string {
	keys := " tab switch pane · enter send · ^O / click ↳ expand · read_page:<id> · recall:<q> · ^C quit"
	if m.focus == focusMemory {
		keys = " ↑↓ move · space expand · click section · tab switch pane · esc back · ^C quit"
	}
	return headStyle.Render(truncate(keys, w))
}

// renderChat lays out the transcript and returns, alongside the rendered pane, an
// owner map parallel to its content rows: owner[row] is the m.lines index of the
// tool-result block that drew that row (so a click there can toggle it), or -1 for
// any other row. View lifts these into screen coordinates for the mouse handler.
func (m Model) renderChat(w, h int) (string, []int) {
	body := make([]string, 0, len(m.lines)+1)
	owns := make([]int, 0, len(m.lines)+1)
	for idx, ln := range m.lines {
		rendered := renderLine(ln, w, m.expandOutput || ln.expanded)
		owner := -1
		if ln.who == "toolresult" {
			owner = idx // the whole block is one click target
		}
		for range rendered {
			owns = append(owns, owner)
		}
		body = append(body, rendered...)
	}
	if m.streaming && m.stream.Len() > 0 {
		streamed := renderLine(line{who: "pi", text: m.stream.String() + "▍"}, w, m.expandOutput)
		body = append(body, streamed...)
		for range streamed {
			owns = append(owns, -1)
		}
	}

	rows := []string{labelStyle.Render("chat"), ""}
	rowOwn := []int{-1, -1}
	transcriptH := h - len(rows) - 1 // reserve the input line
	if transcriptH < 1 {
		transcriptH = 1
	}

	switch {
	case len(body) == 0:
		for _, hint := range []string{
			"type to chat — memory stays live on the right.",
			"try  read_page:F1  to page-fault the exact failure evidence.",
		} {
			for _, wl := range wordWrap(hint, w) {
				rows = append(rows, sysStyle.Render(wl))
				rowOwn = append(rowOwn, -1)
			}
		}
	case len(body) > transcriptH:
		// Tailing hides older turns; mark how many so the history isn't silently lost.
		hidden := len(body) - (transcriptH - 1)
		rows = append(rows, sysStyle.Render(fmt.Sprintf("⌃ %d earlier", hidden)))
		rowOwn = append(rowOwn, -1)
		rows = append(rows, tail(body, transcriptH-1)...)
		rowOwn = append(rowOwn, tail(owns, transcriptH-1)...)
	default:
		rows = append(rows, body...)
		rowOwn = append(rowOwn, owns...)
	}

	for len(rows) < h-1 {
		rows = append(rows, "")
		rowOwn = append(rowOwn, -1)
	}
	rows = append(rows, m.input.View())
	rowOwn = append(rowOwn, -1)
	return strings.Join(rows, "\n"), rowOwn
}

func renderLine(ln line, w int, expand bool) []string {
	if ln.who == "toolresult" {
		return renderToolResult(ln, w, expand)
	}
	var style lipgloss.Style
	var tag string
	switch ln.who {
	case "you":
		style, tag = youStyle, "you> "
	case "pi":
		style, tag = piStyle, "pi>  "
	case "tool":
		style, tag = mutedStyle, "⚙ "
	default:
		style, tag = mutedStyle, ""
	}
	wrapped := wordWrap(tag+ln.text, w)
	out := make([]string, len(wrapped))
	for i, s := range wrapped {
		out[i] = style.Render(s)
	}
	return out
}

// toolPreview is how many body lines a collapsed tool-result block shows before
// eliding the rest — enough to read the evidence's head (which failure, which
// file) without letting a long dump dominate the transcript.
const toolPreview = 3

// renderToolResult renders a tool-result entry (the exact fetched evidence) under
// a "↳ toolName" header. It is verbose system output, so when collapsed it shows
// the first toolPreview body lines and a "⋯ +N more" marker; expanding it (^O, a
// click, or the block's own flag) reveals the full indented body. Collapsing only
// kicks in past toolPreview+1 lines — below that the marker would cost the row it
// saves.
func renderToolResult(ln line, w int, expand bool) []string {
	bodyLines := strings.Split(strings.TrimRight(ln.text, "\n"), "\n")
	rows := []string{mutedStyle.Render(truncate("↳ "+ln.head, w))}

	shown, hidden := bodyLines, 0
	if !expand && len(bodyLines) > toolPreview+1 {
		shown, hidden = bodyLines[:toolPreview], len(bodyLines)-toolPreview
	}
	for _, bl := range shown {
		for _, wl := range wordWrap(bl, w-2) {
			rows = append(rows, mutedStyle.Render("  "+wl))
		}
	}
	if hidden > 0 {
		rows = append(rows, mutedStyle.Render(fmt.Sprintf("  ⋯ +%d more", hidden)))
	}
	return rows
}

// renderMemory lays out the three collapsible sections. The capsule sits on top
// and flexes to fill the pane (shown from its HEAD with an ellipsis when it
// overflows — the full body is always a page-fault away); residency and status
// are compact and render below it. Details stay hidden behind a collapsed summary
// until the pane is focused (tab) and the section expanded (space), so the
// resting view is scannable rather than dense.
func (m Model) renderMemory(w, h int) (string, [nSections]int) {
	noHeaders := [nSections]int{-1, -1, -1}
	title := labelStyle.Render("memory")
	if !m.viz.reachable {
		note := "waiting for core…"
		if m.vizErr {
			note = "core unreachable (retrying)"
		}
		return fitHeight([]string{title, "", sysStyle.Render(note)}, h), noHeaders
	}

	v := m.viz
	sel := func(i int) bool { return m.focus == focusMemory && m.memSel == i }
	sw := max0(w - 14) // width budget for a collapsed summary after its label

	// Collapsed shows just the ratio — a near-empty mini-gauge is muddy and adds no
	// information the "3/64" doesn't; the real gauge lives in the expanded detail.
	resPlain := truncate(fmt.Sprintf("%d/%d", v.residentPages, v.maxPages), sw)
	resDetail := []string{
		fmt.Sprintf("resident %d/%d", v.residentPages, v.maxPages),
		gauge(v.residentPages, v.maxPages, w-2),
	}
	// Pre-wrap the breakdown so a narrow pane can't hard-wrap it behind our back
	// (an unaccounted wrap desyncs the row count and skews the pane height).
	resDetail = append(resDetail, wordWrap(fmt.Sprintf("hot %d · cold %d · pin %d · ghost %d",
		v.hotPages, v.coldPages, v.pinnedPages, v.ghostPages), w-2)...)
	resRows := renderSection("RESIDENCY", sysStyle.Render(resPlain), resPlain, resDetail,
		m.memOpen[secResidency], sel(secResidency), w, -1)

	statSummary := dot(v.maintDegraded) + " index · " + dot(v.embedDegraded) + " embed"
	statDetail := []string{
		"index " + statusWord(v.maintDegraded, v.maintCategory, v.maintPending),
		"embed " + statusWord(v.embedDegraded, v.embedCategory, 0),
	}
	statRows := renderSection("STATUS", statSummary, "● index · ● embed", statDetail,
		m.memOpen[secStatus], sel(secStatus), w, -1)

	// title + top spacer + a blank separator before each of the two fixed blocks.
	used := 2 + 2 + len(resRows) + len(statRows)
	capMax := max0(h - used - 1) // remaining rows for the capsule's expanded body
	anchors := strings.Join(capsuleAnchors(v.capsule), " · ")
	if anchors == "" {
		anchors = "(capsule)"
	}
	anchors = truncate(anchors, sw)
	capRows := renderSection("CAPSULE", sysStyle.Render(anchors), anchors, wordWrap(capsuleBody(v.capsule), w-2),
		m.memOpen[secCapsule], sel(secCapsule), w, capMax)

	rows := []string{title, ""}
	rows = append(rows, capRows...)
	rows = append(rows, "")
	rows = append(rows, resRows...)
	rows = append(rows, "")
	rows = append(rows, statRows...)

	// Content-row of each section header, for click hit-testing. The layout is
	// title, blank, capRows, blank, resRows, blank, statRows — so headers sit at the
	// first row of their block. These stay valid because capMax caps the capsule so
	// the total never exceeds h (fitHeight only pads, never top-chops).
	headers := [nSections]int{
		secCapsule:   2,
		secResidency: 3 + len(capRows),
		secStatus:    4 + len(capRows) + len(resRows),
	}
	return fitHeight(rows, h), headers
}

// renderSection renders one collapsible section. summary is the pre-styled
// collapsed detail shown on the header when closed; summaryPlain is its unstyled
// form, used when the section is selected (rendered in reverse video, which must
// wrap plain text). A maxDetail >= 0 caps the expanded body, appending an
// ellipsis; a negative maxDetail means unbounded.
func renderSection(label, summary, summaryPlain string, detail []string, open, selected bool, w, maxDetail int) []string {
	caret := "▸"
	if open {
		caret = "▾"
	}
	prefix := caret + " " + label

	var head string
	switch {
	case selected:
		line := prefix
		if !open && summaryPlain != "" {
			line += "  " + summaryPlain
		}
		head = selStyle.Render(truncate(line, w))
	case !open && summary != "":
		head = labelStyle.Render(prefix) + "  " + summary
	default:
		head = labelStyle.Render(prefix)
	}

	rows := []string{head}
	if open {
		d := detail
		if maxDetail >= 0 && len(d) > maxDetail {
			if maxDetail <= 1 {
				d = []string{sysStyle.Render("…")}
			} else {
				d = append(d[:maxDetail-1:maxDetail-1], sysStyle.Render("…"))
			}
		}
		for _, ln := range d {
			rows = append(rows, "  "+ln)
		}
	}
	return rows
}

// capsuleAnchors distils the compiled capsule into its record IDs (TASK T1, C1,
// F1, A1, NEXT …) for the one-line collapsed summary. It is a display convenience,
// not a parse of authority: an unrecognised capsule simply yields no anchors and
// the summary falls back to a placeholder.
func capsuleAnchors(capsule string) []string {
	var out []string
	for _, raw := range strings.Split(capsule, "\n") {
		ln := strings.TrimSpace(raw)
		f := strings.Fields(ln)
		switch {
		case strings.HasPrefix(ln, "TASK ") && len(f) >= 2:
			out = append(out, "TASK "+strings.TrimSuffix(f[1], ":"))
		case strings.HasPrefix(ln, "HARD CONSTRAINT ") && len(f) >= 3:
			out = append(out, strings.TrimSuffix(f[2], ":"))
		case strings.HasPrefix(ln, "CONSTRAINT ") && len(f) >= 2:
			out = append(out, strings.TrimSuffix(f[1], ":"))
		case strings.HasPrefix(ln, "LAST FAILURE ") && len(f) >= 3:
			out = append(out, strings.TrimSuffix(f[2], ":"))
		case strings.HasPrefix(ln, "FAILURE ") && len(f) >= 2:
			out = append(out, strings.TrimSuffix(f[1], ":"))
		case strings.HasPrefix(ln, "AVOID ") && len(f) >= 2:
			out = append(out, strings.TrimSuffix(f[1], ":"))
		case ln == "NEXT" || strings.HasPrefix(ln, "NEXT:") || strings.HasPrefix(ln, "NEXT "):
			out = append(out, "NEXT")
		}
	}
	seen := make(map[string]bool, len(out))
	uniq := out[:0]
	for _, s := range out {
		if !seen[s] {
			seen[s] = true
			uniq = append(uniq, s)
		}
	}
	return uniq
}

func capsuleBody(c string) string {
	if c = strings.TrimSpace(c); c != "" {
		return c
	}
	return "(empty)"
}

func dot(degraded bool) string {
	if degraded {
		return warnStyle.Render("●")
	}
	return dotStyle.Render("●")
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
	return dotStyle.Render(label)
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

func truncate(s string, w int) string {
	if w < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

func max0(x int) int {
	if x < 0 {
		return 0
	}
	return x
}

func tail[T any](rows []T, n int) []T {
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
