package renderer

import (
	"fmt"
	"strings"

	"wsms/internal/wsl"
)

// RenderCapsule builds a <working_state> capsule under a token budget.
// Hard constraints are pinned; lower-priority blocks drop first.
func RenderCapsule(st *wsl.WorkingState, budgetTokens int) string {
	if budgetTokens <= 0 {
		budgetTokens = 512
	}

	var blocks []string

	if task := st.ActiveTask(); task != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "TASK %s: %s.", task.IDValue, humanizeGoal(task.Goal))
		if task.Phase != "" {
			fmt.Fprintf(&b, "\nPHASE: %s.", title(task.Phase))
		}
		if task.Branch != "" {
			fmt.Fprintf(&b, "\nBRANCH: %s.", task.Branch)
		}
		if task.Dirty != "" {
			fmt.Fprintf(&b, "\nDIRTY FILES: %s.", task.Dirty)
		}
		blocks = append(blocks, b.String())
	}

	// Pinned hard constraints
	var hardBlocks []string
	for _, c := range st.HardConstraints() {
		hardBlocks = append(hardBlocks, fmt.Sprintf("HARD CONSTRAINT %s:\n%s", c.IDValue, c.Text))
	}

	var softBlocks []string
	for _, c := range st.SoftConstraints() {
		softBlocks = append(softBlocks, fmt.Sprintf("SOFT CONSTRAINT %s:\n%s", c.IDValue, c.Text))
	}

	var failBlock string
	if f := st.LastFailure(); f != nil {
		failBlock = RenderFailureDetail(f)
	}

	var avoidBlocks []string
	for _, a := range st.Avoids() {
		line := fmt.Sprintf("AVOID %s:\n%s", a.IDValue, a.Text)
		if a.Ref != "" {
			line += fmt.Sprintf("; see %s", a.Ref)
		}
		avoidBlocks = append(avoidBlocks, line)
	}

	var nextBlock string
	if n := st.Next(); n != nil {
		var b strings.Builder
		b.WriteString("NEXT:\n")
		if n.Action != "" && n.Target != "" {
			fmt.Fprintf(&b, "%s %s", title(n.Action), n.Target)
		} else if n.Target != "" {
			b.WriteString(n.Target)
		}
		if n.Question != "" {
			fmt.Fprintf(&b, "\nQuestion: %s", n.Question)
		}
		nextBlock = b.String()
	}

	// Assemble with budget: always try to include hard constraints + instruction.
	fault := PageFaultInstruction
	core := []string{}
	core = append(core, blocks...)
	core = append(core, hardBlocks...)

	optional := []string{}
	if failBlock != "" {
		optional = append(optional, failBlock)
	}
	optional = append(optional, avoidBlocks...)
	if nextBlock != "" {
		optional = append(optional, nextBlock)
	}
	optional = append(optional, softBlocks...)

	selected := append([]string{}, core...)
	for _, o := range optional {
		candidate := joinCapsule(append(append([]string{}, selected...), o), fault)
		if EstimateTokens(candidate) <= budgetTokens {
			selected = append(selected, o)
		}
	}

	// If still over budget, drop optional already not added; if core alone over, keep hard+fault only.
	out := joinCapsule(selected, fault)
	if EstimateTokens(out) > budgetTokens {
		// pin hard only
		minimal := append([]string{}, hardBlocks...)
		if len(blocks) > 0 {
			minimal = append(blocks[:1], hardBlocks...)
		}
		out = joinCapsule(minimal, fault)
	}
	return out
}

// RenderFailureDetail formats a failure for capsule or page fault.
func RenderFailureDetail(f *wsl.FailureRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LAST FAILURE %s:\n", f.IDValue)
	if f.Cmd != "" {
		fmt.Fprintf(&b, "Command: `%s`\n", f.Cmd)
	}
	fmt.Fprintf(&b, "Exit: %d\n", f.Exit)
	if f.Err != "" {
		fmt.Fprintf(&b, "Error: %q", f.Err)
	}
	if f.FileHint != "" {
		fmt.Fprintf(&b, "\nLikely file area: %s", f.FileHint)
	}
	return b.String()
}

func joinCapsule(blocks []string, fault string) string {
	var b strings.Builder
	b.WriteString("<working_state>\n")
	for i, blk := range blocks {
		if blk == "" {
			continue
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(blk)
	}
	if len(blocks) > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(fault)
	b.WriteString("\n</working_state>\n")
	return b.String()
}

func humanizeGoal(g string) string {
	g = strings.TrimSpace(g)
	g = strings.TrimPrefix(g, "fix(")
	g = strings.TrimSuffix(g, ")")
	g = strings.ReplaceAll(g, "_", " ")
	if g == "" {
		return "(no goal)"
	}
	// Capitalize first rune simply
	return strings.ToUpper(g[:1]) + g[1:]
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
