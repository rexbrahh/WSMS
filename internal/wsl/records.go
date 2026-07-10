package wsl

import "wsms/internal/types"

// Kind identifies a WSL record type.
type Kind string

const (
	KindTask        Kind = "task"
	KindConstraint  Kind = "constraint"
	KindFailure     Kind = "failure"
	KindDecision    Kind = "decision"
	KindAvoid       Kind = "avoid"
	KindAssumption  Kind = "assumption"
	KindInvalidated Kind = "invalidated"
	KindNext        Kind = "next"
	KindPage        Kind = "page"
	KindFault       Kind = "fault"
)

// Record is any WSL record.
type Record interface {
	Kind() Kind
	ID() string
	Clone() Record
}

// TaskRecord is @task.
type TaskRecord struct {
	IDValue  string
	Phase    string
	Priority types.Priority
	Goal     string
	Branch   string
	Commit   string
	Dirty    string
}

func (r *TaskRecord) Kind() Kind { return KindTask }
func (r *TaskRecord) ID() string { return r.IDValue }
func (r *TaskRecord) Clone() Record {
	c := *r
	return &c
}

// ConstraintRecord is @constraint.
type ConstraintRecord struct {
	IDValue  string
	Strength types.Strength
	Source   types.Source
	Text     string
	Scope    types.Scope
}

func (r *ConstraintRecord) Kind() Kind { return KindConstraint }
func (r *ConstraintRecord) ID() string { return r.IDValue }
func (r *ConstraintRecord) Clone() Record {
	c := *r
	return &c
}

// FailureRecord is @failure.
type FailureRecord struct {
	IDValue  string
	Cmd      string
	Exit     int
	Err      string
	FileHint string
	Raw      string
}

func (r *FailureRecord) Kind() Kind { return KindFailure }
func (r *FailureRecord) ID() string { return r.IDValue }
func (r *FailureRecord) Clone() Record {
	c := *r
	return &c
}

// DecisionRecord is @decision.
type DecisionRecord struct {
	IDValue string
	Chosen  string
	Because string
	Refs    string
	Scope   types.Scope
}

func (r *DecisionRecord) Kind() Kind { return KindDecision }
func (r *DecisionRecord) ID() string { return r.IDValue }
func (r *DecisionRecord) Clone() Record {
	c := *r
	return &c
}

// AvoidRecord is @avoid.
type AvoidRecord struct {
	IDValue string
	Reason  string
	Text    string
	Ref     string
}

func (r *AvoidRecord) Kind() Kind { return KindAvoid }
func (r *AvoidRecord) ID() string { return r.IDValue }
func (r *AvoidRecord) Clone() Record {
	c := *r
	return &c
}

// AssumptionRecord is @assumption.
type AssumptionRecord struct {
	IDValue  string
	Status   string
	Text     string
	Evidence string
}

func (r *AssumptionRecord) Kind() Kind { return KindAssumption }
func (r *AssumptionRecord) ID() string { return r.IDValue }
func (r *AssumptionRecord) Clone() Record {
	c := *r
	return &c
}

// InvalidatedRecord is @invalidated.
type InvalidatedRecord struct {
	IDValue string
	Target  string
	Reason  string
}

func (r *InvalidatedRecord) Kind() Kind { return KindInvalidated }
func (r *InvalidatedRecord) ID() string { return r.IDValue }
func (r *InvalidatedRecord) Clone() Record {
	c := *r
	return &c
}

// NextRecord is @next (singleton-ish; id optional / empty).
type NextRecord struct {
	Action   string
	Target   string
	Question string
}

func (r *NextRecord) Kind() Kind { return KindNext }
func (r *NextRecord) ID() string { return "next" }
func (r *NextRecord) Clone() Record {
	c := *r
	return &c
}

// PageRecord is @page.
type PageRecord struct {
	IDValue string
	KindStr string
	Tier    string
	Summary string
	Refs    string
	Scope   types.Scope
	Branch  string
	Stale   bool
}

func (r *PageRecord) Kind() Kind { return KindPage }
func (r *PageRecord) ID() string { return r.IDValue }
func (r *PageRecord) Clone() Record {
	c := *r
	return &c
}

// FaultRecord is @fault.
type FaultRecord struct {
	IDValue string
	KindStr string
	Target  string
}

func (r *FaultRecord) Kind() Kind { return KindFault }
func (r *FaultRecord) ID() string {
	if r.IDValue != "" {
		return r.IDValue
	}
	return "fault"
}
func (r *FaultRecord) Clone() Record {
	c := *r
	return &c
}

// Update is an observer/scheduler mutation intent.
type Update struct {
	Op         string // must be "upsert"
	Record     Record
	EvidenceID string // durable event that produced this update
}
