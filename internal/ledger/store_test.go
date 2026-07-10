package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	wsmserrors "wsms/internal/errors"
)

func TestAppendOnlyAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	ctx := context.Background()
	ev1, err := l.Append(ctx, Event{
		Type: EventUserInstruction,
		Payload: map[string]any{
			"text": "do not rewrite transport layer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev1.ID != "E0001" {
		t.Fatalf("id=%s", ev1.ID)
	}
	if ev1.Seq != 1 {
		t.Fatalf("seq=%d", ev1.Seq)
	}

	ev2, err := l.Append(ctx, Event{
		Type: EventCommandOutput,
		Payload: map[string]any{
			"cmd":    "go test ./runtime -run TestCancelStream",
			"exit":   1,
			"output": "stream goroutine still blocked",
			"err":    "stream goroutine still blocked",
		},
		TS: time.Now().UTC().Add(time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev2.ID != "E0002" {
		t.Fatalf("id=%s", ev2.ID)
	}
	if ev2.Seq != 2 {
		t.Fatalf("seq=%d", ev2.Seq)
	}

	got, err := l.Get(ctx, "E0001")
	if err != nil {
		t.Fatal(err)
	}
	if got.PayloadString("text") != "do not rewrite transport layer" {
		t.Fatalf("payload: %#v", got.Payload)
	}

	list, err := l.ListBySession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("len=%d", len(list))
	}
}

func TestSessionScopedIdentityAndLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	first, err := Open(path, "session-one")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := Open(path, "session-two")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	ctx := context.Background()
	if _, err := first.Append(ctx, Event{
		SessionID: "session-two",
		Type:      EventUserInstruction,
	}); err == nil {
		t.Fatal("foreign-session append succeeded")
	}
	if _, err := first.ListBySession(ctx, "session-two"); err == nil {
		t.Fatal("foreign-session list succeeded")
	}
	firstEvent, err := first.Append(ctx, Event{
		Type:    EventUserInstruction,
		Payload: map[string]any{"text": "first session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondEvent, err := second.Append(ctx, Event{
		Type:    EventUserInstruction,
		Payload: map[string]any{"text": "second session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstEvent.ID != "E0001" || secondEvent.ID != "E0001" {
		t.Fatalf("first id=%q second id=%q", firstEvent.ID, secondEvent.ID)
	}

	gotFirst, err := first.Get(ctx, "E0001")
	if err != nil {
		t.Fatal(err)
	}
	gotSecond, err := second.Get(ctx, "E0001")
	if err != nil {
		t.Fatal(err)
	}
	if gotFirst.SessionID != "session-one" || gotFirst.PayloadString("text") != "first session" {
		t.Fatalf("first lookup returned %#v", gotFirst)
	}
	if gotSecond.SessionID != "session-two" || gotSecond.PayloadString("text") != "second session" {
		t.Fatalf("second lookup returned %#v", gotSecond)
	}

	secondOnly, err := second.Append(ctx, Event{
		Type:    EventAssistantMessage,
		Payload: map[string]any{"text": "only in second session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondOnly.ID != "E0002" {
		t.Fatalf("second-only id=%q", secondOnly.ID)
	}
	if _, err := first.Get(ctx, "E0002"); !errors.Is(err, wsmserrors.ErrNotFound) {
		t.Fatalf("first ledger Get(E0002) error=%v, want not found", err)
	}

	firstEvents, err := first.ListBySession(ctx, "session-one")
	if err != nil {
		t.Fatal(err)
	}
	secondEvents, err := second.ListBySession(ctx, "session-two")
	if err != nil {
		t.Fatal(err)
	}
	if len(firstEvents) != 1 || len(secondEvents) != 2 {
		t.Fatalf("first count=%d second count=%d", len(firstEvents), len(secondEvents))
	}
}

func TestAppendRejectsCallerAssignedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path, "identity")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestLedger(t, l) })

	ctx := context.Background()
	for _, ev := range []Event{
		{ID: "E0042", Type: EventAssistantMessage, Payload: map[string]any{"text": "caller id"}},
		{Seq: 42, Type: EventAssistantMessage, Payload: map[string]any{"text": "caller seq"}},
	} {
		if _, err := l.Append(ctx, ev); err == nil {
			t.Fatalf("caller identity accepted: %#v", ev)
		}
	}
	stored, err := l.Append(ctx, Event{
		Type:    EventAssistantMessage,
		Payload: map[string]any{"text": "server-assigned identity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != "E0001" || stored.Seq != 1 {
		t.Fatalf("stored identity=(%q,%d), want (E0001,1)", stored.ID, stored.Seq)
	}
}

func TestAppendOrderIgnoresCallerTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(path, "order")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestLedger(t, l) })

	base := time.Now().UTC()
	inputs := []struct {
		name string
		ts   time.Time
	}{
		{name: "appended-first", ts: base.Add(time.Hour)},
		{name: "appended-second", ts: base.Add(-time.Hour)},
		{name: "equal-time-first", ts: base},
		{name: "equal-time-second", ts: base},
	}
	ctx := context.Background()
	for i, input := range inputs {
		stored, err := l.Append(ctx, Event{
			TS:      input.ts,
			Type:    EventAssistantMessage,
			Payload: map[string]any{"text": input.name, "name": input.name},
		})
		if err != nil {
			t.Fatal(err)
		}
		wantSeq := int64(i + 1)
		if stored.Seq != wantSeq || stored.ID != fmt.Sprintf("E%04d", wantSeq) {
			t.Fatalf("stored identity=(%q,%d), want sequence %d", stored.ID, stored.Seq, wantSeq)
		}
	}

	events, err := l.ListBySession(ctx, "order")
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range events {
		if got := ev.PayloadString("name"); got != inputs[i].name {
			t.Fatalf("event %d name=%q, want %q", i, got, inputs[i].name)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d seq=%d", i, ev.Seq)
		}
	}
}

func TestConcurrentHandlesSerializeSessionSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	first, err := Open(path, "shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestLedger(t, first) })
	second, err := Open(path, "shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestLedger(t, second) })

	const perHandle = 12
	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	appendMany := func(l *AppendOnlyLedger, source string) {
		defer wg.Done()
		<-start
		for i := 0; i < perHandle; i++ {
			_, err := l.Append(context.Background(), Event{
				Type:    EventAssistantMessage,
				Payload: map[string]any{"text": fmt.Sprintf("%s-%d", source, i), "source": source, "index": i},
			})
			if err != nil {
				errCh <- err
				return
			}
		}
	}
	wg.Add(2)
	go appendMany(first, "first")
	go appendMany(second, "second")
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent append: %v", err)
	}

	events, err := first.ListBySession(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2*perHandle {
		t.Fatalf("event count=%d, want %d", len(events), 2*perHandle)
	}
	for i, ev := range events {
		wantSeq := int64(i + 1)
		wantID := fmt.Sprintf("E%04d", wantSeq)
		if ev.Seq != wantSeq || ev.ID != wantID {
			t.Fatalf("event %d identity=(%q,%d), want (%q,%d)", i, ev.ID, ev.Seq, wantID, wantSeq)
		}
	}
}

func TestSequenceRecoversFromDurableMaximum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	ctx := context.Background()
	l, err := Open(path, "durable-max")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := l.Append(ctx, Event{
			Type:    EventAssistantMessage,
			Payload: map[string]any{"text": fmt.Sprintf("seed-%d", i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a durable gap. Allocation must follow MAX(append_seq), not COUNT(*).
	if _, err := l.db.ExecContext(ctx,
		`DELETE FROM events WHERE session_id = ? AND append_seq = ?`, "durable-max", 2,
	); err != nil {
		t.Fatal(err)
	}
	closeTestLedger(t, l)

	reopened, err := Open(path, "durable-max")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestLedger(t, reopened) })
	stored, err := reopened.Append(ctx, Event{
		Type:    EventAssistantMessage,
		Payload: map[string]any{"text": "after-gap"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != "E0004" || stored.Seq != 4 {
		t.Fatalf("stored identity=(%q,%d), want (E0004,4)", stored.ID, stored.Seq)
	}
}

func TestQuestionMarkPathUsesEscapedFileURIAndReopens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger?data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events?.db")
	ctx := context.Background()

	first, err := Open(path, "escaped-path")
	if err != nil {
		t.Fatal(err)
	}
	firstClosed := false
	t.Cleanup(func() {
		if !firstClosed {
			closeTestLedger(t, first)
		}
	})
	stored, err := first.Append(ctx, Event{
		Type:    EventAssistantMessage,
		Payload: map[string]any{"text": "persist at the requested path"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != "E0001" {
		t.Fatalf("first event id=%q", stored.ID)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("requested database path was not created: %v", err)
	}
	firstClosed = true
	closeTestLedger(t, first)

	reopened, err := Open(path, "escaped-path")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestLedger(t, reopened) })
	events, err := reopened.ListBySession(ctx, "escaped-path")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].PayloadString("text") != "persist at the requested path" {
		t.Fatalf("reopened events=%#v", events)
	}
}

func TestDurableDecodeErrorsAreTyped(t *testing.T) {
	tests := []struct {
		name      string
		updateSQL string
		wantOp    string
	}{
		{name: "timestamp", updateSQL: `UPDATE events SET ts = 'not-a-timestamp'`, wantOp: "decode_timestamp"},
		{name: "payload", updateSQL: `UPDATE events SET payload_json = '{broken'`, wantOp: "decode_payload"},
		{name: "scope", updateSQL: `UPDATE events SET scope_json = '{broken'`, wantOp: "decode_scope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.db")
			l, err := Open(path, "corrupt")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeTestLedger(t, l) })
			if _, err := l.Append(context.Background(), Event{
				Type:    EventAssistantMessage,
				Payload: map[string]any{"text": "seed"},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := l.db.Exec(tt.updateSQL); err != nil {
				t.Fatal(err)
			}

			_, err = l.ListBySession(context.Background(), "corrupt")
			var ledgerErr *wsmserrors.LedgerError
			if !errors.As(err, &ledgerErr) {
				t.Fatalf("error=%v, want LedgerError", err)
			}
			if ledgerErr.Op != tt.wantOp {
				t.Fatalf("op=%q, want %q", ledgerErr.Op, tt.wantOp)
			}
		})
	}
}

func closeTestLedger(t *testing.T, l *AppendOnlyLedger) {
	t.Helper()
	if err := l.Close(); err != nil {
		t.Errorf("close ledger: %v", err)
	}
}
