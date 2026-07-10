package wsl

import (
	"fmt"
	"strconv"
	"strings"
)

// Serialize renders records in canonical WSL form.
func Serialize(recs []Record) string {
	var b strings.Builder
	for i, r := range recs {
		if i > 0 {
			b.WriteByte('\n')
		}
		writeRecord(&b, r)
	}
	return b.String()
}

func writeRecord(b *strings.Builder, r Record) {
	switch rec := r.(type) {
	case *TaskRecord:
		fmt.Fprintf(b, "@task %s", rec.IDValue)
		writeAttr(b, "phase", rec.Phase)
		writeAttr(b, "priority", string(rec.Priority))
		b.WriteByte('\n')
		writeBody(b, "goal", rec.Goal, false)
		writeBody(b, "branch", rec.Branch, false)
		writeBody(b, "commit", rec.Commit, false)
		writeBody(b, "dirty", rec.Dirty, false)
	case *ConstraintRecord:
		fmt.Fprintf(b, "@constraint %s %s", rec.IDValue, rec.Strength)
		writeAttr(b, "source", string(rec.Source))
		b.WriteByte('\n')
		writeBody(b, "text", rec.Text, true)
		writeBody(b, "scope", string(rec.Scope), false)
	case *FailureRecord:
		fmt.Fprintf(b, "@failure %s\n", rec.IDValue)
		writeBody(b, "cmd", rec.Cmd, true)
		if rec.Cmd != "" || rec.Exit != 0 || rec.Err != "" {
			// always write exit when failure present
		}
		writeBody(b, "exit", strconv.Itoa(rec.Exit), false)
		writeBody(b, "err", rec.Err, true)
		writeBody(b, "file_hint", rec.FileHint, false)
		writeBody(b, "raw", rec.Raw, false)
	case *DecisionRecord:
		fmt.Fprintf(b, "@decision %s\n", rec.IDValue)
		writeBody(b, "chosen", rec.Chosen, true)
		writeBody(b, "because", rec.Because, true)
		writeBody(b, "refs", rec.Refs, false)
		writeBody(b, "scope", string(rec.Scope), false)
	case *AvoidRecord:
		fmt.Fprintf(b, "@avoid %s", rec.IDValue)
		writeAttr(b, "reason", rec.Reason)
		b.WriteByte('\n')
		writeBody(b, "text", rec.Text, true)
		writeBody(b, "ref", rec.Ref, false)
	case *AssumptionRecord:
		fmt.Fprintf(b, "@assumption %s", rec.IDValue)
		writeAttr(b, "status", rec.Status)
		b.WriteByte('\n')
		writeBody(b, "text", rec.Text, true)
		writeBody(b, "evidence", rec.Evidence, false)
	case *InvalidatedRecord:
		fmt.Fprintf(b, "@invalidated %s\n", rec.IDValue)
		writeBody(b, "target", rec.Target, false)
		writeBody(b, "reason", rec.Reason, true)
	case *NextRecord:
		b.WriteString("@next\n")
		writeBody(b, "action", rec.Action, false)
		writeBody(b, "target", rec.Target, false)
		writeBody(b, "question", rec.Question, true)
	case *PageRecord:
		fmt.Fprintf(b, "@page %s", rec.IDValue)
		writeAttr(b, "kind", rec.KindStr)
		writeAttr(b, "tier", rec.Tier)
		b.WriteByte('\n')
		writeBody(b, "summary", rec.Summary, true)
		writeBody(b, "refs", rec.Refs, false)
		writeBody(b, "scope", string(rec.Scope), false)
		writeBody(b, "branch", rec.Branch, false)
	case *FaultRecord:
		if rec.IDValue != "" {
			fmt.Fprintf(b, "@fault %s\n", rec.IDValue)
		} else {
			b.WriteString("@fault\n")
		}
		writeBody(b, "kind", rec.KindStr, false)
		writeBody(b, "target", rec.Target, false)
	default:
		fmt.Fprintf(b, "@%s %s\n", r.Kind(), r.ID())
	}
}

func writeAttr(b *strings.Builder, k, v string) {
	if v == "" {
		return
	}
	fmt.Fprintf(b, " %s=%s", k, v)
}

func writeBody(b *strings.Builder, k, v string, quotePrefer bool) {
	if v == "" {
		return
	}
	if quotePrefer || strings.ContainsAny(v, " \t\r\n\"'`") {
		// prefer double quotes unless backticks needed for nested quotes; use backticks for commands
		if (k == "cmd" || strings.Contains(v, `"`)) && !strings.ContainsAny(v, "`\r\n") {
			fmt.Fprintf(b, "%s: `%s`\n", k, v)
			return
		}
		fmt.Fprintf(b, "%s: %s\n", k, strconv.Quote(v))
		return
	}
	fmt.Fprintf(b, "%s: %s\n", k, v)
}
