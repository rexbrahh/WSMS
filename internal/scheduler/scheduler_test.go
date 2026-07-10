package scheduler

import (
	"context"
	"errors"
	"testing"

	"wsms/internal/config"
	"wsms/internal/ledger"
	"wsms/internal/memory"
	"wsms/internal/observers"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

type staticObserver struct {
	updates []wsl.Update
}

type checkpointObserver struct {
	ids          *observers.SeqIDGen
	rejectBatch  bool
	observerFail bool
}

func (o *checkpointObserver) Name() string { return "checkpoint" }

func (o *checkpointObserver) Handle(context.Context, ledger.Event) ([]wsl.Update, error) {
	if o.observerFail {
		o.observerFail = false
		_ = o.ids.Next("F")
		return nil, errors.New("observer failed after allocating an id")
	}
	updates := []wsl.Update{
		{
			Op: "upsert",
			Record: &wsl.ConstraintRecord{
				IDValue:  o.ids.Next("C"),
				Strength: types.StrengthHard,
				Source:   types.SourceUser,
				Text:     "do not consume rejected ids",
				Scope:    types.ScopeTask,
			},
		},
		{
			Op: "upsert",
			Record: &wsl.FailureRecord{
				IDValue: o.ids.Next("F"),
				Cmd:     "go test ./...",
				Exit:    1,
				Err:     "failed",
			},
		},
	}
	if o.rejectBatch {
		o.rejectBatch = false
		updates = append(updates, wsl.Update{
			Op: "upsert",
			Record: &wsl.AvoidRecord{
				IDValue: "A1",
				Reason:  "failed_attempt",
				Text:    "invalid dangling reference",
				Ref:     "F404",
			},
		})
	}
	return updates, nil
}

func (o *staticObserver) Name() string { return "static" }

func (o *staticObserver) Handle(context.Context, ledger.Event) ([]wsl.Update, error) {
	return append([]wsl.Update(nil), o.updates...), nil
}

func TestAfterEventRejectsBatchWithoutStateProvenanceOrPageLeaks(t *testing.T) {
	st := wsl.NewWorkingState()
	hierarchy := memory.NewHierarchy()
	sched := newTestScheduler(st, hierarchy, []wsl.Update{
		{
			Op: "upsert",
			Record: &wsl.FailureRecord{
				IDValue: "F1",
				Cmd:     "go test ./...",
				Exit:    1,
				Err:     "failed",
			},
			EvidenceID: "wrong-event",
		},
		{
			Op: "upsert",
			Record: &wsl.AvoidRecord{
				IDValue: "A1",
				Reason:  "failed_attempt",
				Text:    "bad patch",
				Ref:     "F404",
			},
		},
	})

	err := sched.AfterEvent(context.Background(), ledger.Event{ID: "E0001", Type: ledger.EventType("test_event")})
	if err == nil {
		t.Fatal("expected invalid second update to reject event derivation")
	}
	if _, ok := st.Get("F1"); ok {
		t.Fatal("failure record leaked from rejected event batch")
	}
	if _, ok := st.EvidenceID("F1"); ok {
		t.Fatal("failure provenance leaked from rejected event batch")
	}
	if _, ok := hierarchy.GetPage("F1"); ok {
		t.Fatal("failure L2 page leaked from rejected event batch")
	}
}

func TestAfterEventAssignsDeterministicLiveReplayProvenance(t *testing.T) {
	event := ledger.Event{ID: "E0042", Type: ledger.EventType("test_event")}
	updates := []wsl.Update{
		{
			Op: "upsert",
			Record: &wsl.ConstraintRecord{
				IDValue:  "C1",
				Strength: types.StrengthHard,
				Source:   types.SourceUser,
				Text:     "do not rewrite transport layer",
				Scope:    types.ScopeTask,
			},
		},
		{
			Op: "upsert",
			Record: &wsl.FailureRecord{
				IDValue: "F1",
				Cmd:     "go test ./...",
				Exit:    1,
				Err:     "failed",
			},
		},
	}

	liveState := wsl.NewWorkingState()
	liveHierarchy := memory.NewHierarchy()
	if err := newTestScheduler(liveState, liveHierarchy, updates).AfterEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}

	replayState := wsl.NewWorkingState()
	replayHierarchy := memory.NewHierarchy()
	if err := newTestScheduler(replayState, replayHierarchy, updates).AfterEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"C1", "F1"} {
		liveEvidence, liveOK := liveState.EvidenceID(id)
		replayEvidence, replayOK := replayState.EvidenceID(id)
		if !liveOK || !replayOK || liveEvidence != event.ID || replayEvidence != event.ID {
			t.Fatalf("%s live=(%q,%v) replay=(%q,%v)", id, liveEvidence, liveOK, replayEvidence, replayOK)
		}
	}
	if _, ok := liveHierarchy.GetPage("F1"); !ok {
		t.Fatal("successful live derivation did not materialize failure page")
	}
	if _, ok := replayHierarchy.GetPage("F1"); !ok {
		t.Fatal("successful replay derivation did not materialize failure page")
	}
}

func TestAfterEventRestoresAllocatedIDsWhenBatchIsRejected(t *testing.T) {
	ids := observers.NewSeqIDGen()
	observer := &checkpointObserver{ids: ids, rejectBatch: true}
	st := wsl.NewWorkingState()
	hierarchy := memory.NewHierarchy()
	dispatcher := &observers.Dispatcher{
		Allocator: ids,
		Observers: []observers.Observer{observer},
	}
	sched := New(config.Default(), st, hierarchy, dispatcher, nil)

	if err := sched.AfterEvent(context.Background(), ledger.Event{ID: "E0001", Type: ledger.EventType("test_event")}); err == nil {
		t.Fatal("expected rejected observer batch")
	}
	if snapshot := ids.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("rejected batch consumed ids: %#v", snapshot)
	}
	if records := st.Records(); len(records) != 0 {
		t.Fatalf("rejected batch committed records: %#v", records)
	}

	if err := sched.AfterEvent(context.Background(), ledger.Event{ID: "E0002", Type: ledger.EventType("test_event")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("C1"); !ok {
		t.Fatal("successful retry did not reuse C1")
	}
	if _, ok := st.Get("F1"); !ok {
		t.Fatal("successful retry did not reuse F1")
	}
	if _, ok := st.Get("C2"); ok {
		t.Fatal("rejected batch consumed C1")
	}
	if _, ok := st.Get("F2"); ok {
		t.Fatal("rejected batch consumed F1")
	}
	for _, id := range []string{"C1", "F1"} {
		evidenceID, ok := st.EvidenceID(id)
		if !ok || evidenceID != "E0002" {
			t.Fatalf("%s evidence=(%q,%v), want E0002", id, evidenceID, ok)
		}
	}
}

func TestAfterEventRestoresAllocatedIDsWhenObserverFails(t *testing.T) {
	ids := observers.NewSeqIDGen()
	observer := &checkpointObserver{ids: ids, observerFail: true}
	st := wsl.NewWorkingState()
	dispatcher := &observers.Dispatcher{
		Allocator: ids,
		Observers: []observers.Observer{observer},
	}
	sched := New(config.Default(), st, memory.NewHierarchy(), dispatcher, nil)

	if err := sched.AfterEvent(context.Background(), ledger.Event{ID: "E0001", Type: ledger.EventType("test_event")}); err == nil {
		t.Fatal("expected observer error")
	}
	if snapshot := ids.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("observer error consumed ids: %#v", snapshot)
	}
	if err := sched.AfterEvent(context.Background(), ledger.Event{ID: "E0002", Type: ledger.EventType("test_event")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("F1"); !ok {
		t.Fatal("successful retry did not reuse F1")
	}
}

func newTestScheduler(st *wsl.WorkingState, hierarchy *memory.Hierarchy, updates []wsl.Update) *Scheduler {
	dispatcher := &observers.Dispatcher{Observers: []observers.Observer{
		&staticObserver{updates: updates},
	}}
	return New(config.Default(), st, hierarchy, dispatcher, nil)
}
