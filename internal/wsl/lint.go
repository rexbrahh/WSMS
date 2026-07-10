package wsl

import (
	"fmt"
	"strings"

	wsmserrors "wsms/internal/errors"
	"wsms/internal/types"
)

// Lint checks a working state for policy violations (locks state).
func Lint(st *WorkingState) []wsmserrors.LintIssue {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return lintUnlocked(st)
}

func lintUnlocked(st *WorkingState) []wsmserrors.LintIssue {
	var issues []wsmserrors.LintIssue

	hard := st.constraintsUnlocked(types.StrengthHard)
	for _, d := range st.decisionsUnlocked() {
		for _, c := range hard {
			if contradicts(d.Chosen, c.Text) {
				issues = append(issues, wsmserrors.LintIssue{
					Severity: "error",
					Code:     "decision_vs_hard_constraint",
					Message:  fmt.Sprintf("decision %s contradicts hard constraint %s", d.IDValue, c.IDValue),
					RecordID: d.IDValue,
				})
			}
		}
	}

	known := st.knownIDsUnlocked()
	for _, a := range st.avoidsUnlocked() {
		if a.Ref != "" && !refExists(a.Ref, known) {
			issues = append(issues, wsmserrors.LintIssue{
				Severity: "error",
				Code:     "dangling_avoid_ref",
				Message:  fmt.Sprintf("avoid %s refs missing %s", a.IDValue, a.Ref),
				RecordID: a.IDValue,
			})
		}
	}
	for _, inv := range st.invalidatedUnlocked() {
		if inv.Target != "" && !refExists(inv.Target, known) {
			issues = append(issues, wsmserrors.LintIssue{
				Severity: "error",
				Code:     "dangling_invalidated_target",
				Message:  fmt.Sprintf("invalidated %s targets missing %s", inv.IDValue, inv.Target),
				RecordID: inv.IDValue,
			})
		}
	}

	for _, p := range st.pagesUnlocked() {
		if p.Stale {
			issues = append(issues, wsmserrors.LintIssue{
				Severity: "warning",
				Code:     "stale_page",
				Message:  fmt.Sprintf("page %s is stale", p.IDValue),
				RecordID: p.IDValue,
			})
		}
	}

	return issues
}

// LintApply validates inserting/updating rec against current state.
func LintApply(st *WorkingState, rec Record) []wsmserrors.LintIssue {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return lintApplyUnlocked(st, rec)
}

func lintApplyUnlocked(st *WorkingState, rec Record) []wsmserrors.LintIssue {
	var issues []wsmserrors.LintIssue

	if prev, ok := st.byID[rec.ID()]; ok {
		if prev.Kind() != rec.Kind() {
			issues = append(issues, wsmserrors.LintIssue{
				Severity: "error",
				Code:     "record_kind_changed",
				Message:  fmt.Sprintf("record %s kind %s cannot change to %s", rec.ID(), prev.Kind(), rec.Kind()),
				RecordID: rec.ID(),
			})
		}
		switch n := rec.(type) {
		case *FailureRecord:
			if p, ok := prev.(*FailureRecord); ok {
				if p.Cmd != "" && p.Cmd != n.Cmd {
					issues = append(issues, immutableIssue(n.IDValue, "cmd"))
				}
				if p.Err != "" && p.Err != n.Err {
					issues = append(issues, immutableIssue(n.IDValue, "err"))
				}
			}
		case *ConstraintRecord:
			if p, ok := prev.(*ConstraintRecord); ok {
				if p.Text != "" && p.Text != n.Text {
					issues = append(issues, immutableIssue(n.IDValue, "text"))
				}
			}
		}
	}

	if page, ok := rec.(*PageRecord); ok {
		if iss := branchScopeGuardUnlocked(st, page); iss != nil {
			issues = append(issues, *iss)
		}
	}

	clone := st.cloneUnlocked()
	clone.upsertUnchecked(rec)
	issues = append(issues, lintUnlocked(clone)...)
	return issues
}

func immutableIssue(id, field string) wsmserrors.LintIssue {
	return wsmserrors.LintIssue{
		Severity: "error",
		Code:     "immutable_field",
		Message:  fmt.Sprintf("record %s field %s is immutable", id, field),
		RecordID: id,
	}
}

func contradicts(chosen, constraintText string) bool {
	c := strings.ToLower(strings.TrimSpace(chosen))
	t := normalizeConstraintText(constraintText)
	if c == "" || t == "" {
		return false
	}
	for _, prefix := range []string{
		"do not ", "don't ", "never ", "must not ",
		"you must not ", "you should not ",
	} {
		if strings.HasPrefix(t, prefix) {
			banned := strings.TrimSpace(t[len(prefix):])
			banned = strings.Trim(banned, " \t\r\n.,;:!?\"'`")
			if banned != "" && strings.Contains(c, banned) {
				return true
			}
		}
	}
	return false
}

func normalizeConstraintText(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	for {
		trimmed := text
		for _, prefix := range []string{
			"could you please ", "would you please ",
			"please, ", "please: ", "please ",
			"kindly, ", "kindly: ", "kindly ",
		} {
			if strings.HasPrefix(text, prefix) {
				text = strings.TrimSpace(text[len(prefix):])
				break
			}
		}
		if text == trimmed {
			return text
		}
	}
}

func refExists(ref string, known map[string]bool) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return true
	}
	for _, part := range strings.Fields(ref) {
		part = strings.Trim(part, ",")
		if !known[part] {
			return false
		}
	}
	return true
}

// HasError reports whether any issue is severity error.
func HasError(issues []wsmserrors.LintIssue) bool {
	for _, i := range issues {
		if i.Severity == "error" {
			return true
		}
	}
	return false
}

func branchScopeGuardUnlocked(st *WorkingState, page *PageRecord) *wsmserrors.LintIssue {
	task := st.activeTaskUnlocked()
	if task == nil || page.Scope != types.ScopeBranch {
		return nil
	}
	if page.Branch != "" && task.Branch != "" && page.Branch != task.Branch {
		iss := wsmserrors.LintIssue{
			Severity: "error",
			Code:     "branch_scope_mismatch",
			Message:  fmt.Sprintf("page %s branch %s != task branch %s", page.IDValue, page.Branch, task.Branch),
			RecordID: page.IDValue,
		}
		return &iss
	}
	return nil
}
