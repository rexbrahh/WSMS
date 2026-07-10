package wsl

// ValidKinds is the WSL v0 type registry.
var ValidKinds = map[Kind]bool{
	KindTask:        true,
	KindConstraint:  true,
	KindFailure:     true,
	KindDecision:    true,
	KindAvoid:       true,
	KindAssumption:  true,
	KindInvalidated: true,
	KindNext:        true,
	KindPage:        true,
	KindFault:       true,
}

// FieldOrder defines canonical serialization order per kind (body fields).
var FieldOrder = map[Kind][]string{
	KindTask:        {"goal", "branch", "commit", "dirty"},
	KindConstraint:  {"text", "scope"},
	KindFailure:     {"cmd", "exit", "err", "file_hint", "raw"},
	KindDecision:    {"chosen", "because", "refs", "scope"},
	KindAvoid:       {"text", "ref"},
	KindAssumption:  {"text", "evidence"},
	KindInvalidated: {"target", "reason"},
	KindNext:        {"action", "target", "question"},
	KindPage:        {"summary", "refs", "scope", "branch"},
	KindFault:       {"kind", "target"},
}
