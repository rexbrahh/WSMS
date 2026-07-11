package operator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wsms/internal/config"
	"wsms/internal/harness"
	"wsms/internal/ledger"
	"wsms/internal/operator"
	"wsms/internal/types"
	"wsms/internal/wsl"
)

// seed writes a deterministic session into dataDir through the real harness, so
// the operator commands run against the same ledger/derived-cache layout the CLI
// produces. It mirrors the demo shape: a task, a failed command (→ @failure F1),
// a failure-grounded decision (→ @decision D1 + @avoid A1), and a next action.
func seed(t *testing.T, dataDir, sessionID string) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.SessionID = sessionID

	s, err := harness.OpenSession(cfg)
	if err != nil {
		t.Fatalf("open session %q: %v", sessionID, err)
	}
	ctx := context.Background()

	must := func(op string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	must("StartTask", s.StartTask(ctx, harness.TaskStart{
		Goal:     "preserve exact failure evidence across restart",
		TaskID:   "T1",
		Phase:    "debugging",
		Priority: types.PriorityHot,
		Branch:   "main",
		Commit:   "seed-commit",
		Dirty:    "src/runtime/stream.go",
	}))
	must("IngestUser", s.IngestUser(ctx, "keep the exact blocked-waiter failure"))
	must("IngestCommandOutput", s.IngestCommandOutput(ctx,
		"go test ./runtime -run TestCancelStream", 1,
		"error: stream goroutine still blocked\n  at src/runtime/stream.go:118"))
	// Refs/AvoidRef ground the decision in the failure ingested above (the sole
	// failure, so the observer assigns it F1); avoid_ref is required whenever
	// avoid_text is present.
	must("RecordDecision", s.RecordDecision(ctx, harness.DecisionInput{
		Chosen:    "patch cancellation cleanup within the existing boundary",
		Because:   "the failure preserves the exact blocked-waiter evidence",
		Refs:      "F1",
		Scope:     types.ScopeTask,
		AvoidText: "retrying the failed transport rewrite",
		AvoidRef:  "F1",
	}))
	must("SetNext", s.SetNext(ctx, harness.NextAction{
		Action:   "inspect",
		Target:   "src/runtime/stream.go:118-176",
		Question: "does the waiter exit after cancellation?",
	}))
	must("Close", s.Close())
}

// recordID opens the session and returns the id of the first record of the given
// kind, so tests target real (observer-assigned) ids rather than hard-coding them.
func recordID(t *testing.T, dataDir, sessionID string, kind wsl.Kind) string {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.SessionID = sessionID
	s, err := harness.OpenSession(cfg)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() { _ = s.Close() }()
	for _, r := range s.State.Records() {
		if r.Kind() == kind {
			return r.ID()
		}
	}
	t.Fatalf("no %s record in session %q", kind, sessionID)
	return ""
}

func exportEvents(t *testing.T, dataDir, sessionID string) []ledger.Event {
	t.Helper()
	var buf bytes.Buffer
	n, err := operator.Export(context.Background(), operator.Options{DataDir: dataDir, SessionID: sessionID}, &buf)
	if err != nil {
		t.Fatalf("export %q: %v", sessionID, err)
	}
	var events []ledger.Event
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var ev ledger.Event
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("decode exported event: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) != n {
		t.Fatalf("export returned count %d but wrote %d JSONL lines", n, len(events))
	}
	return events
}

func inspect(t *testing.T, dataDir, sessionID, view, arg string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := operator.Inspect(context.Background(),
		operator.Options{DataDir: dataDir, SessionID: sessionID}, view, arg, &buf); err != nil {
		t.Fatalf("inspect %s %q: %v", view, arg, err)
	}
	return buf.String()
}

func TestExportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")

	events := exportEvents(t, dir, "s1")
	if len(events) == 0 {
		t.Fatal("export produced no events")
	}
	// Append order must be preserved and ids stable.
	if events[0].Type != ledger.EventTaskStarted {
		t.Errorf("first event = %q, want %q", events[0].Type, ledger.EventTaskStarted)
	}
	if events[0].ID != "E0001" {
		t.Errorf("first event id = %q, want E0001", events[0].ID)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Errorf("append_seq not monotonic at %d: %d <= %d", i, events[i].Seq, events[i-1].Seq)
		}
	}
}

func TestExportMissingLedger(t *testing.T) {
	_, err := operator.Export(context.Background(),
		operator.Options{DataDir: t.TempDir(), SessionID: "nope"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("export against an empty data dir should error, got nil")
	}
	if !strings.Contains(err.Error(), "no ledger") {
		t.Errorf("error = %q, want it to mention the missing ledger", err)
	}
}

func TestInspectSessions(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "alpha")
	seed(t, dir, "beta")

	// The session used for the sessions scan does not matter — it scans all.
	out := inspect(t, dir, "alpha", "sessions", "")
	for _, id := range []string{"alpha", "beta"} {
		if !strings.Contains(out, id) {
			t.Errorf("sessions output missing %q:\n%s", id, out)
		}
	}
}

func TestInspectEventsAndEvent(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")

	list := inspect(t, dir, "s1", "events", "")
	if !strings.Contains(list, "E0001") || !strings.Contains(list, string(ledger.EventTaskStarted)) {
		t.Errorf("events listing missing E0001/task_started:\n%s", list)
	}

	one := inspect(t, dir, "s1", "event", "E0001")
	if !strings.Contains(one, `"type": "task_started"`) {
		t.Errorf("event E0001 detail unexpected:\n%s", one)
	}
}

func TestInspectEventErrors(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")

	err := operator.Inspect(context.Background(),
		operator.Options{DataDir: dir, SessionID: "s1"}, "event", "", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "requires an event id") {
		t.Errorf("empty event id = %v, want an id-required error", err)
	}

	err = operator.Inspect(context.Background(),
		operator.Options{DataDir: dir, SessionID: "s1"}, "event", "E9999", &bytes.Buffer{})
	if err == nil {
		t.Error("nonexistent event id should error")
	}

	err = operator.Inspect(context.Background(),
		operator.Options{DataDir: dir, SessionID: "s1"}, "bogusview", "", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown view") {
		t.Errorf("unknown view = %v, want an unknown-view error", err)
	}
}

func TestInspectStateAndCapsule(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")

	state := inspect(t, dir, "s1", "state", "")
	if !strings.Contains(state, "@task T1") || !strings.Contains(state, "@failure") {
		t.Errorf("state missing expected records:\n%s", state)
	}

	capsule := inspect(t, dir, "s1", "capsule", "")
	if !strings.Contains(capsule, "<working_state>") || !strings.Contains(capsule, "LAST FAILURE") {
		t.Errorf("capsule missing working state / failure:\n%s", capsule)
	}
}

func TestInspectPage(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")

	failureID := recordID(t, dir, "s1", wsl.KindFailure)
	page := inspect(t, dir, "s1", "page", failureID)
	if strings.Contains(page, "PAGE_MISS") || !strings.Contains(page, failureID) {
		t.Errorf("page %s should render failure content, got:\n%s", failureID, page)
	}

	miss := inspect(t, dir, "s1", "page", "NOSUCHPAGE")
	if !strings.Contains(miss, "PAGE_MISS") {
		t.Errorf("bogus page id should report PAGE_MISS, got:\n%s", miss)
	}
}

func TestInspectResidency(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")

	out := inspect(t, dir, "s1", "residency", "")
	var snap map[string]any
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("residency output is not valid JSON: %v\n%s", err, out)
	}
	if _, ok := snap["ResidentPages"]; !ok {
		t.Errorf("residency snapshot missing ResidentPages:\n%s", out)
	}
}

func TestDeleteSuppressesFromCapsule(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")

	failureID := recordID(t, dir, "s1", wsl.KindFailure)
	before := exportEvents(t, dir, "s1")
	if !strings.Contains(inspect(t, dir, "s1", "capsule", ""), "LAST FAILURE") {
		t.Fatal("precondition: capsule should show the failure before delete")
	}

	var report bytes.Buffer
	if err := operator.Delete(context.Background(),
		operator.Options{DataDir: dir, SessionID: "s1"},
		operator.DeleteSpec{Kind: "record", Target: failureID, Reason: "superseded"},
		&report); err != nil {
		t.Fatalf("delete %s: %v", failureID, err)
	}
	if !strings.Contains(report.String(), "invalidated") {
		t.Errorf("delete report = %q, want it to confirm invalidation", report.String())
	}

	// L4 grew by exactly one memory_invalidated event (append-only: nothing removed).
	after := exportEvents(t, dir, "s1")
	if len(after) != len(before)+1 {
		t.Fatalf("event count %d -> %d, want +1 (append-only invalidation)", len(before), len(after))
	}
	if last := after[len(after)-1]; last.Type != ledger.EventMemoryInvalidated {
		t.Errorf("last event = %q, want %q", last.Type, ledger.EventMemoryInvalidated)
	}

	// The derived L1 capsule now honors the tombstone; the raw L4 state retains it.
	if strings.Contains(inspect(t, dir, "s1", "capsule", ""), "LAST FAILURE") {
		t.Error("capsule still shows the failure after logical delete")
	}
	if !strings.Contains(inspect(t, dir, "s1", "state", ""), "@invalidated") {
		t.Error("raw state should retain an @invalidated tombstone")
	}
}

func TestDeleteValidation(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")
	opts := operator.Options{DataDir: dir, SessionID: "s1"}

	cases := []struct {
		name string
		spec operator.DeleteSpec
		want string
	}{
		{"bad kind", operator.DeleteSpec{Kind: "widget", Target: "F1", Reason: "superseded"}, "invalid --kind"},
		{"bad reason", operator.DeleteSpec{Kind: "record", Target: "F1", Reason: "meh"}, "invalid --reason"},
		{"empty target", operator.DeleteSpec{Kind: "record", Target: "  ", Reason: "superseded"}, "target is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := operator.Delete(context.Background(), opts, tc.spec, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Delete(%+v) = %v, want error containing %q", tc.spec, err, tc.want)
			}
		})
	}
}

func TestDeleteUnknownTarget(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")

	// Coherence rejects invalidating a target that does not exist.
	err := operator.Delete(context.Background(),
		operator.Options{DataDir: dir, SessionID: "s1"},
		operator.DeleteSpec{Kind: "record", Target: "ZZ9", Reason: "superseded"},
		&bytes.Buffer{})
	if err == nil {
		t.Fatal("deleting a nonexistent target should error")
	}
}

func TestPurgeDryRunKeepsBytes(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")
	before := len(exportEvents(t, dir, "s1"))

	var out bytes.Buffer
	if err := operator.Purge(context.Background(),
		operator.Options{DataDir: dir, SessionID: "s1"}, false, &out); err != nil {
		t.Fatalf("purge dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "dry run") || !strings.Contains(out.String(), "--yes") {
		t.Errorf("dry-run report should describe intent and require --yes:\n%s", out.String())
	}
	// Nothing removed.
	if after := len(exportEvents(t, dir, "s1")); after != before {
		t.Errorf("dry-run changed event count %d -> %d", before, after)
	}
}

func TestPurgeErasesOneSessionOnly(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "victim")
	seed(t, dir, "keeper")

	var out bytes.Buffer
	if err := operator.Purge(context.Background(),
		operator.Options{DataDir: dir, SessionID: "victim"}, true, &out); err != nil {
		t.Fatalf("purge victim: %v", err)
	}
	if !strings.Contains(out.String(), "purged session") {
		t.Errorf("purge report unexpected:\n%s", out.String())
	}

	// Victim's durable rows are gone; the shared ledger and other session survive.
	if _, err := os.Stat(filepath.Join(dir, "ledger.db")); err != nil {
		t.Errorf("ledger.db should survive purge: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("data dir should survive purge: %v", err)
	}
	if keeper := len(exportEvents(t, dir, "keeper")); keeper == 0 {
		t.Error("keeper session lost its events during victim purge")
	}
	if victim := len(exportEvents(t, dir, "victim")); victim != 0 {
		t.Errorf("victim session still has %d events after purge", victim)
	}
	// The disposable L3 index is dropped (rebuildable on next open).
	if _, err := os.Stat(filepath.Join(dir, "index")); !os.IsNotExist(err) {
		t.Errorf("index dir should be removed by purge, stat err = %v", err)
	}
}

func TestPurgeNothingToDo(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "s1")

	var out bytes.Buffer
	if err := operator.Purge(context.Background(),
		operator.Options{DataDir: dir, SessionID: "ghost"}, true, &out); err != nil {
		t.Fatalf("purge unknown session: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to purge") {
		t.Errorf("purging a rowless session should be a no-op, got:\n%s", out.String())
	}
}

func TestPurgeMissingLedger(t *testing.T) {
	err := operator.Purge(context.Background(),
		operator.Options{DataDir: t.TempDir(), SessionID: "s1"}, true, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no ledger") {
		t.Errorf("purge against empty dir = %v, want a missing-ledger error", err)
	}
}
