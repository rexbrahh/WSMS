package harness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"wsms/internal/config"
	wsmserrors "wsms/internal/errors"
	"wsms/internal/faults"
	"wsms/internal/ledger"
	"wsms/internal/memory"
	"wsms/internal/observers"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

func TestOpenSessionUsesDefaultForZeroResidencyPolicy(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "zero-residency-policy")
	cfg.SessionID = "zero-residency-policy"
	cfg.ResidencyPolicy = memory.Policy{}

	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	if got, want := s.Hierarchy.Snapshot().Policy, memory.DefaultPolicy(); !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved residency policy = %#v, want %#v", got, want)
	}
}

func TestOpenSessionRejectsInvalidResidencyPolicyBeforeCreatingDataDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "must-not-be-created")
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.SessionID = "invalid-residency-policy"
	cfg.ResidencyPolicy.MaxResidentPages = 0

	_, err := OpenSession(cfg)
	if !errors.Is(err, memory.ErrInvalidPolicy) {
		t.Fatalf("OpenSession error = %v, want ErrInvalidPolicy", err)
	}
	if _, statErr := os.Stat(dataDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid policy mutated data dir: stat error = %v", statErr)
	}
}

type failOnceConstraintObserver struct {
	ids   *observers.SeqIDGen
	calls int
}

func (o *failOnceConstraintObserver) Name() string { return "fail_once_constraint" }

func (o *failOnceConstraintObserver) Handle(context.Context, ledger.Event) ([]wsl.Update, error) {
	o.calls++
	updates := []wsl.Update{{
		Op: "upsert",
		Record: &wsl.ConstraintRecord{
			IDValue:  o.ids.Next("C"),
			Strength: types.StrengthHard,
			Source:   types.SourceUser,
			Text:     "do not reuse a rolled-back virtual id",
			Scope:    types.ScopeTask,
		},
	}}
	if o.calls == 1 {
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

func TestSessionSmokeEventsToCapsule(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "smoke"
	cfg.CapsuleTokenBudget = 512

	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })

	ctx := context.Background()
	if err := s.IngestUser(ctx, "do not rewrite transport layer"); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx,
		"go test ./runtime -run TestCancelStream",
		1,
		"error: stream goroutine still blocked\nsrc/runtime/stream.go:118: fail",
	); err != nil {
		t.Fatal(err)
	}

	// Seed a task + next for a richer capsule (observers don't create tasks yet).
	// Capsule should still include hard constraint + failure from observers.
	cap, err := s.BeforeTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cap, "do not rewrite transport layer") {
		t.Fatalf("missing constraint:\n%s", cap)
	}
	if !strings.Contains(cap, "LAST FAILURE") && !strings.Contains(cap, "stream goroutine still blocked") {
		// Failure detail might use LAST FAILURE header
		t.Fatalf("missing failure evidence:\n%s", cap)
	}
	if !strings.Contains(cap, "request a page by ID") {
		t.Fatalf("missing fault instruction:\n%s", cap)
	}

	// Find failure id from state
	f := s.State.LastFailure()
	if f == nil {
		t.Fatal("expected failure in state")
	}
	page, err := s.PageFault(ctx, f.IDValue)
	if err != nil {
		t.Fatal(err)
	}
	if page == faults.PageMiss {
		t.Fatal("page miss")
	}
	if !strings.Contains(page, "stream goroutine still blocked") {
		t.Fatalf("page body: %q", page)
	}
}

func TestSessionCloseReleasesLedgerAndArtifactStore(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "close-resources"
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ledger.ListBySession(context.Background(), cfg.SessionID); err == nil {
		t.Fatal("session close left ledger usable")
	}
	if _, err := s.Artifacts.Put([]byte("after close"), "text/plain"); err == nil {
		t.Fatal("session close left artifact store usable")
	}
}

func TestSessionCloseIsIdempotentAndConcurrencySafe(t *testing.T) {
	t.Run("concurrent success", func(t *testing.T) {
		cfg := config.Default()
		cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
		cfg.SessionID = "concurrent-close"
		s, err := OpenSession(cfg)
		if err != nil {
			t.Fatal(err)
		}

		const callers = 16
		start := make(chan struct{})
		errs := make(chan error, callers)
		var wg sync.WaitGroup
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- s.Close()
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent Close: %v", err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatalf("repeated Close: %v", err)
		}
		if _, err := s.Append(context.Background(), ledger.Event{Type: ledger.EventAssistantMessage}); !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("Append after Close error=%v, want ErrSessionClosed", err)
		}
		if _, err := s.BeforeTurn(context.Background()); !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("BeforeTurn after Close error=%v, want ErrSessionClosed", err)
		}
	})

}

func TestSessionFailStopsAfterDurableDerivationFailure(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "fail-stop"
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })

	ids := observers.NewSeqIDGen()
	observer := &failOnceConstraintObserver{ids: ids}
	s.Dispatcher.Allocator = ids
	s.Dispatcher.Observers = []observers.Observer{observer}

	ctx := context.Background()
	stored, err := s.Append(ctx, ledger.Event{
		Type:    ledger.EventUserInstruction,
		Payload: map[string]any{"text": "trigger rejected derivation"},
	})
	if !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("first Append error=%v, want ErrSessionUnavailable", err)
	}
	if stored.ID != "E0001" || stored.Seq != 1 {
		t.Fatalf("durable failed event=(%q,%d), want (E0001,1)", stored.ID, stored.Seq)
	}
	if snapshot := ids.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("failed derivation consumed ids: %#v", snapshot)
	}
	if _, ok := s.State.Get("C1"); ok {
		t.Fatal("failed derivation committed C1")
	}

	later, err := s.Append(ctx, ledger.Event{
		Type:    ledger.EventUserInstruction,
		Payload: map[string]any{"text": "would otherwise reuse C1"},
	})
	if !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("later Append error=%v, want ErrSessionUnavailable", err)
	}
	if later.ID != "" || later.Seq != 0 {
		t.Fatalf("rejected later Append returned stored identity (%q,%d)", later.ID, later.Seq)
	}
	if observer.calls != 1 {
		t.Fatalf("observer calls=%d, want 1", observer.calls)
	}
	if snapshot := ids.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("later Append reused a restored id: %#v", snapshot)
	}
	events, listErr := s.Ledger.ListBySession(ctx, cfg.SessionID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(events) != 1 || events[0].ID != stored.ID {
		t.Fatalf("durable events=%#v, want only failed E0001", events)
	}
	if _, err := s.BeforeTurn(ctx); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("BeforeTurn error=%v, want ErrSessionUnavailable", err)
	}
	if page, err := s.PageFault(ctx, "C1"); err != nil || page != faults.PageMiss {
		t.Fatalf("diagnostic PageFault=(%q,%v), want PAGE_MISS without error", page, err)
	}
}

func TestSessionReplayRestoresStatePagesAndRawEvidence(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "replay"
	cfg.ArtifactThresholdBytes = 64
	cfg.CapsuleTokenBudget = 512
	cfg.PageFaultTokenBudget = 512

	ctx := context.Background()
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}

	const constraint = "do not rewrite transport layer"
	const failure = "error: stream goroutine still blocked"
	const rawSentinel = "RAW-EVIDENCE-AFTER-PREVIEW"
	if err := s.IngestUser(ctx, constraint); err != nil {
		t.Fatal(err)
	}
	output := failure + "\n" + strings.Repeat("diagnostic context ", 32) + rawSentinel
	if err := s.IngestCommandOutput(ctx,
		"go test ./runtime -run TestCancelStream",
		1,
		output,
	); err != nil {
		t.Fatal(err)
	}

	beforeEvents, err := s.Ledger.ListBySession(ctx, cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeEvents) != 2 {
		t.Fatalf("pre-close event count=%d", len(beforeEvents))
	}
	beforeCapsule, err := s.BeforeTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(beforeCapsule, constraint) || !strings.Contains(beforeCapsule, failure) {
		t.Fatalf("pre-close capsule missing exact evidence:\n%s", beforeCapsule)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, reopened) })

	afterEvents, err := reopened.Ledger.ListBySession(ctx, cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("replay wrote events: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}

	constraints := reopened.State.HardConstraints()
	if len(constraints) != 1 || constraints[0].IDValue != "C1" || constraints[0].Text != constraint {
		t.Fatalf("replayed constraints=%#v", constraints)
	}
	replayedFailure := reopened.State.LastFailure()
	if replayedFailure == nil {
		t.Fatal("replay did not restore failure")
	}
	if replayedFailure.IDValue != "F1" || replayedFailure.Err != failure {
		t.Fatalf("replayed failure=%#v", replayedFailure)
	}
	if replayedFailure.Raw == "" {
		t.Fatal("replayed failure lost raw evidence reference")
	}

	afterCapsule, err := reopened.BeforeTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(afterCapsule, constraint) || !strings.Contains(afterCapsule, failure) {
		t.Fatalf("reopened capsule missing exact evidence:\n%s", afterCapsule)
	}
	page, err := reopened.PageFault(ctx, replayedFailure.IDValue)
	if err != nil {
		t.Fatal(err)
	}
	if page == faults.PageMiss || !strings.Contains(page, failure) {
		t.Fatalf("replayed failure page=%q", page)
	}
	raw, err := reopened.Tools.ReadRawLog(ctx, replayedFailure.IDValue, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, rawSentinel) {
		t.Fatalf("replayed raw evidence missing sentinel: %q", raw)
	}

	nextConstraintEvent, err := reopened.Append(ctx, ledger.Event{
		Type:    ledger.EventHumanCorrection,
		Payload: map[string]any{"text": "never discard exact evidence"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if nextConstraintEvent.ID != "E0003" {
		t.Fatalf("next event id=%q, want E0003", nextConstraintEvent.ID)
	}
	record, ok := reopened.State.Get("C2")
	if !ok {
		t.Fatal("next constraint record C2 not found")
	}
	if constraintRecord, ok := record.(*wsl.ConstraintRecord); !ok || constraintRecord.Text != "never discard exact evidence" {
		t.Fatalf("next constraint record=%#v", record)
	}

	nextFailureEvent, err := reopened.Append(ctx, ledger.Event{
		Type: ledger.EventCommandOutput,
		Payload: map[string]any{
			"cmd":    "go test ./runtime -run TestSecondFailure",
			"exit":   1,
			"output": "error: second failure",
			"err":    "error: second failure",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if nextFailureEvent.ID != "E0004" {
		t.Fatalf("next failure event id=%q, want E0004", nextFailureEvent.ID)
	}
	if got := reopened.State.LastFailure(); got == nil || got.IDValue != "F2" {
		t.Fatalf("next failure record=%#v", got)
	}
}

func TestSessionReplayUsesDurableAppendOrder(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "append-order"

	ctx := context.Background()
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	first, err := s.Append(ctx, ledger.Event{
		TS:      base.Add(time.Hour),
		Type:    ledger.EventUserInstruction,
		Payload: map[string]any{"text": "do not discard the first event"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Append(ctx, ledger.Event{
		TS:      base.Add(-time.Hour),
		Type:    ledger.EventUserInstruction,
		Payload: map[string]any{"text": "do not discard the second event"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("live sequences=(%d,%d)", first.Seq, second.Seq)
	}
	closeTestSession(t, s)

	reopened, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, reopened) })
	constraints := reopened.State.HardConstraints()
	if len(constraints) != 2 {
		t.Fatalf("constraint count=%d", len(constraints))
	}
	if constraints[0].IDValue != "C1" || constraints[0].Text != "do not discard the first event" {
		t.Fatalf("first replayed constraint=%#v", constraints[0])
	}
	if constraints[1].IDValue != "C2" || constraints[1].Text != "do not discard the second event" {
		t.Fatalf("second replayed constraint=%#v", constraints[1])
	}
}

func TestConcurrentSessionAppendPreservesLiveReplayOrderAndProvenance(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "concurrent-append"

	ctx := context.Background()
	live, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	liveClosed := false
	t.Cleanup(func() {
		if !liveClosed {
			closeTestSession(t, live)
		}
	})

	const eventCount = 24
	start := make(chan struct{})
	eventsCh := make(chan ledger.Event, eventCount)
	errs := make(chan error, eventCount)
	var wg sync.WaitGroup
	for i := 0; i < eventCount; i++ {
		text := fmt.Sprintf("must preserve concurrent instruction %02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			stored, err := live.Append(ctx, ledger.Event{
				Type:    ledger.EventUserInstruction,
				Payload: map[string]any{"text": text},
			})
			if err != nil {
				errs <- err
				return
			}
			eventsCh <- stored
		}()
	}
	close(start)
	wg.Wait()
	close(eventsCh)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}

	events := make([]ledger.Event, 0, eventCount)
	for ev := range eventsCh {
		events = append(events, ev)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	assertConcurrentConstraintOrder(t, live.State, events)

	liveClosed = true
	closeTestSession(t, live)
	replayed, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, replayed) })
	assertConcurrentConstraintOrder(t, replayed.State, events)
}

func TestOpenSessionRejectsCorruptReplayEvent(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "corrupt-replay"

	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(context.Background(), "do not discard durable errors"); err != nil {
		t.Fatal(err)
	}
	closeTestSession(t, s)

	execLedgerTestSQL(t, filepath.Join(cfg.DataDir, "ledger.db"),
		`UPDATE events SET payload_json = '{broken' WHERE session_id = ?`, cfg.SessionID)

	reopened, err := OpenSession(cfg)
	if reopened != nil {
		closeTestSession(t, reopened)
		t.Fatal("OpenSession returned a session for corrupt replay input")
	}
	var ledgerErr *wsmserrors.LedgerError
	if !errors.As(err, &ledgerErr) || ledgerErr.Op != "decode_payload" {
		t.Fatalf("error=%v, want decode_payload LedgerError", err)
	}
}

func TestOperationalEventsReplayWithStableIDsAndProvenance(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "operational-replay"
	cfg.CapsuleTokenBudget = 512
	ctx := context.Background()

	live, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.StartTask(ctx, TaskStart{Goal: "first task", Phase: "planning"}); err != nil {
		t.Fatal(err)
	}
	if err := live.StartTask(ctx, TaskStart{Goal: "current task", Phase: "debugging", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := live.IngestUser(ctx, "do not rewrite transport layer"); err != nil {
		t.Fatal(err)
	}
	if err := live.IngestCommandOutput(ctx, "go test ./runtime", 1, "error: waiter blocked"); err != nil {
		t.Fatal(err)
	}
	if err := live.RecordDecision(ctx, DecisionInput{
		Chosen:    "inspect cancellation cleanup",
		Because:   "F1 is exact evidence",
		Refs:      "F1",
		AvoidText: "retry the failed cleanup",
		AvoidRef:  "F1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := live.SetNext(ctx, NextAction{Action: "inspect", Target: "first.go", Question: "old question"}); err != nil {
		t.Fatal(err)
	}
	if err := live.SetNext(ctx, NextAction{Action: "patch", Target: "stream.go"}); err != nil {
		t.Fatal(err)
	}

	events, err := live.Ledger.ListBySession(ctx, cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("event count=%d, want 7", len(events))
	}
	expectedProvenance := map[string]string{
		"T1": "E0001", "T2": "E0002", "C1": "E0003", "F1": "E0004",
		"D1": "E0005", "A1": "E0005", "next": "E0007",
	}
	if got := live.State.Provenance(); !reflect.DeepEqual(got, expectedProvenance) {
		t.Fatalf("live provenance=%v, want %v", got, expectedProvenance)
	}
	if first, ok := live.State.Get("T1"); !ok || first.(*wsl.TaskRecord).Goal != "first task" {
		t.Fatalf("first task=%#v ok=%v", first, ok)
	}
	active := live.State.ActiveTask()
	if active == nil || active.IDValue != "T2" || active.Goal != "current task" || active.Priority != types.PriorityHot {
		t.Fatalf("active task=%#v", active)
	}
	if next := live.State.Next(); next == nil || next.Action != "patch" || next.Target != "stream.go" || next.Question != "" {
		t.Fatalf("replacement next=%#v", next)
	}
	decisions := live.State.Decisions()
	if len(decisions) != 1 || decisions[0].Scope != types.ScopeTask {
		t.Fatalf("default decision scope=%#v", decisions)
	}
	beforeState := wsl.Serialize(live.State.Records())
	beforeCapsule, err := live.BeforeTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(beforeCapsule, "TASK T2: Current task") || !strings.Contains(beforeCapsule, "Patch stream.go") {
		t.Fatalf("capsule did not use latest task/next:\n%s", beforeCapsule)
	}
	closeTestSession(t, live)

	replayed, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, replayed) })
	if got := wsl.Serialize(replayed.State.Records()); got != beforeState {
		t.Fatalf("replayed state differs:\n%s\nwant:\n%s", got, beforeState)
	}
	if got := replayed.State.Provenance(); !reflect.DeepEqual(got, expectedProvenance) {
		t.Fatalf("replayed provenance=%v, want %v", got, expectedProvenance)
	}
	afterCapsule, err := replayed.BeforeTurn(ctx)
	if err != nil || afterCapsule != beforeCapsule {
		t.Fatalf("replayed capsule mismatch err=%v\n%s", err, afterCapsule)
	}
	if err := replayed.StartTask(ctx, TaskStart{Goal: "continuation task"}); err != nil {
		t.Fatal(err)
	}
	if got := replayed.State.ActiveTask(); got == nil || got.IDValue != "T3" {
		t.Fatalf("allocator did not continue after replay: %#v", got)
	}
}

func TestSessionPrevalidationLeavesNoEventOrArtifactAndRemainsUsable(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "prevalidation"
	cfg.ArtifactThresholdBytes = 1
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	ctx := context.Background()
	large := strings.Repeat("raw evidence ", 100)

	if _, err := s.Append(ctx, ledger.Event{
		Type:      ledger.EventCommandOutput,
		SessionID: "foreign",
		Payload:   map[string]any{"cmd": "go test", "exit": 1, "output": large},
	}); err == nil {
		t.Fatal("foreign event was accepted")
	}
	if _, err := s.Append(ctx, ledger.Event{
		Type:    ledger.EventCommandOutput,
		Payload: map[string]any{"exit": 1, "output": large},
	}); !errors.Is(err, ledger.ErrInvalidEvent) {
		t.Fatalf("malformed command error=%v, want ErrInvalidEvent", err)
	}
	events, err := s.Ledger.ListBySession(ctx, cfg.SessionID)
	if err != nil || len(events) != 0 {
		t.Fatalf("prevalidation persisted events=%#v err=%v", events, err)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.DataDir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("prevalidation left artifact entries: %#v", entries)
	}
	if err := s.StartTask(ctx, TaskStart{Goal: "first valid task"}); err != nil {
		t.Fatal(err)
	}
	events, err = s.Ledger.ListBySession(ctx, cfg.SessionID)
	if err != nil || len(events) != 1 || events[0].ID != "E0001" {
		t.Fatalf("first valid event=%#v err=%v", events, err)
	}
	if got := s.State.ActiveTask(); got == nil || got.IDValue != "T1" {
		t.Fatalf("first valid task=%#v", got)
	}
}

func TestDecisionWithMissingGroundingFailStopsAtomicallyAndFailsReplayWithContext(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "bad-decision"
	ctx := context.Background()
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.Append(ctx, ledger.Event{
		Type: ledger.EventDecision,
		Payload: map[string]any{
			"chosen":     "inspect a missing failure",
			"avoid_text": "retry missing evidence",
			"avoid_ref":  "F404",
		},
	})
	if !errors.Is(err, ErrSessionUnavailable) || stored.ID != "E0001" {
		t.Fatalf("Append=(%#v,%v), want durable E0001 plus ErrSessionUnavailable", stored, err)
	}
	if len(s.State.Records()) != 0 || len(s.State.Provenance()) != 0 {
		t.Fatalf("failed decision leaked state=%#v provenance=%v", s.State.Records(), s.State.Provenance())
	}
	if err := s.SetNext(ctx, NextAction{Action: "continue", Target: "later"}); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("fail-stopped helper error=%v", err)
	}
	events, listErr := s.Ledger.ListBySession(ctx, cfg.SessionID)
	if listErr != nil || len(events) != 1 {
		t.Fatalf("durable events=%#v err=%v", events, listErr)
	}
	closeTestSession(t, s)

	replayed, err := OpenSession(cfg)
	if replayed != nil {
		closeTestSession(t, replayed)
		t.Fatal("replay unexpectedly accepted missing avoidance grounding")
	}
	if err == nil || !strings.Contains(err.Error(), "E0001") || !strings.Contains(err.Error(), "decision") || !strings.Contains(err.Error(), "dangling_avoid_ref") {
		t.Fatalf("replay error lacks event/type/lint context: %v", err)
	}
}

func TestOpenSessionRejectsSchemaInvalidPersistedKnownEventWithContext(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "wsms-data")
	cfg.SessionID = "schema-invalid-replay"
	s, err := OpenSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IngestAssistant(context.Background(), "valid before corruption"); err != nil {
		t.Fatal(err)
	}
	closeTestSession(t, s)
	execLedgerTestSQL(t, filepath.Join(cfg.DataDir, "ledger.db"),
		`UPDATE events SET payload_json = '{}' WHERE session_id = ? AND id = 'E0001'`, cfg.SessionID)

	replayed, err := OpenSession(cfg)
	if replayed != nil {
		closeTestSession(t, replayed)
		t.Fatal("OpenSession returned a session for schema-invalid replay input")
	}
	if err == nil || !strings.Contains(err.Error(), "E0001") || !strings.Contains(err.Error(), "assistant_message") || !strings.Contains(err.Error(), `"text"`) {
		t.Fatalf("replay validation error lacks context: %v", err)
	}
}

func closeTestSession(t *testing.T, s *Session) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Errorf("close session: %v", err)
	}
}

func assertConcurrentConstraintOrder(t *testing.T, st *wsl.WorkingState, events []ledger.Event) {
	t.Helper()
	constraints := st.SoftConstraints()
	if len(constraints) != len(events) {
		t.Fatalf("constraint count=%d, want %d", len(constraints), len(events))
	}
	for i, ev := range events {
		wantID := fmt.Sprintf("C%d", i+1)
		wantText := ev.PayloadString("text")
		if constraints[i].IDValue != wantID || constraints[i].Text != wantText {
			t.Fatalf("constraint %d=(%q,%q), want (%q,%q)", i, constraints[i].IDValue, constraints[i].Text, wantID, wantText)
		}
		evidenceID, ok := st.EvidenceID(wantID)
		if !ok || evidenceID != ev.ID {
			t.Fatalf("constraint %s evidence=(%q,%v), want (%q,true)", wantID, evidenceID, ok, ev.ID)
		}
	}
}

func execLedgerTestSQL(t *testing.T, path, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close independent test database: %v", err)
		}
	}()
	if _, err := db.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}
