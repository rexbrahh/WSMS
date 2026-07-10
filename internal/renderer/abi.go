// Package renderer converts WSL into provider-facing structured text capsules.
package renderer

// PageFaultInstruction is always appended to capsules.
const PageFaultInstruction = "If details are missing, request a page by ID instead of guessing."

// EstimateTokens is a whitespace-split token estimator for v0 budgets.
func EstimateTokens(s string) int {
	n := 0
	in := false
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			in = false
			continue
		}
		if !in {
			n++
			in = true
		}
	}
	return n
}
