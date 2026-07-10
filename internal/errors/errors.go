// Package errors defines sentinel and typed errors for WSMS packages.
package errors

import "fmt"

// Sentinel errors for common control-flow outcomes.
var (
	ErrNotFound      = fmt.Errorf("not found")
	ErrPageMiss      = fmt.Errorf("page miss")
	ErrInvalidWSL    = fmt.Errorf("invalid wsl")
	ErrLintFailed    = fmt.Errorf("wsl lint failed")
	ErrAppendOnly    = fmt.Errorf("ledger is append-only")
	ErrDuplicateID   = fmt.Errorf("duplicate id")
	ErrImmutableField = fmt.Errorf("immutable field changed")
)

// ParseError is a WSL parse failure with optional location.
type ParseError struct {
	Line    int
	Message string
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("wsl parse error at line %d: %s", e.Line, e.Message)
	}
	return fmt.Sprintf("wsl parse error: %s", e.Message)
}

func (e *ParseError) Unwrap() error { return ErrInvalidWSL }

// LintError aggregates lint issues that block an apply.
type LintError struct {
	Issues []LintIssue
}

func (e *LintError) Error() string {
	if len(e.Issues) == 0 {
		return ErrLintFailed.Error()
	}
	return fmt.Sprintf("%s: %s", ErrLintFailed.Error(), e.Issues[0].Message)
}

func (e *LintError) Unwrap() error { return ErrLintFailed }

// LintIssue is one lint finding.
type LintIssue struct {
	Severity  string // "error" or "warning"
	Code      string
	Message   string
	RecordID  string
}

// LedgerError wraps ledger operations.
type LedgerError struct {
	Op  string
	Err error
}

func (e *LedgerError) Error() string {
	return fmt.Sprintf("ledger %s: %v", e.Op, e.Err)
}

func (e *LedgerError) Unwrap() error { return e.Err }
