package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"wsms/internal/config"
	"wsms/internal/retrieval"
	"wsms/internal/types"
)

// maintenanceConfig builds a session config with async maintenance either on or
// off. Callers pass a fresh dir but may reuse a session id so page identities
// match across a synchronous reference session and an async one.
func maintenanceConfig(dir, sessionID string, async bool, depth int) config.Config {
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.SessionID = sessionID
	cfg.PageFaultTokenBudget = 4096
	cfg.AsyncMaintenance = async
	cfg.MaintenanceQueueDepth = depth
	return cfg
}

func buildMaintenanceCorpus(t *testing.T, s *Session, marker string) {
	t.Helper()
	ctx := context.Background()
	if err := s.StartTask(ctx, TaskStart{Goal: marker + " goal", Repo: "repo", Branch: "main", Commit: "aaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestUser(ctx, "do not drop "+marker+" constraint"); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, marker+" failing", 1, marker+" failure signature"); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCommandOutput(ctx, marker+" verified", 0, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDecision(ctx, DecisionInput{Chosen: marker + " decision", Because: "keep a measured working set", Scope: types.ScopeTask}); err != nil {
		t.Fatal(err)
	}
}

// waitForMaintenanceDrained blocks until the async worker has applied every
// durable event to the index and reports no pending or parked work.
func waitForMaintenanceDrained(t *testing.T, s *Session) {
	t.Helper()
	if s.Index == nil {
		t.Fatal("session has no warm index")
	}
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.appendMu.Lock()
		head := s.lastEvent.Seq
		indexErr := s.IndexErr
		s.appendMu.Unlock()
		wm, _, err := s.Index.Watermark(ctx, s.Cfg.SessionID)
		if err != nil {
			t.Fatalf("watermark: %v", err)
		}
		status := s.MaintenanceStatus()
		if wm >= head && status.Pending == 0 && !status.Parked && !status.Reconciling {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("async maintenance did not drain: head=%d watermark=%d status=%#v indexErr=%v", head, wm, status, indexErr)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// materializedSet maps each materialized page id to its exact evidence body.
// With a shared session id it is comparable across a sync and an async session.
func materializedSet(result retrieval.Result) map[string]string {
	out := make(map[string]string, len(result.Materialized))
	for _, m := range result.Materialized {
		out[string(m.PageID)] = strings.Join(m.Evidence, "\n\n")
	}
	return out
}

// Async maintenance must return the same exact-evidence semantic result as the
// synchronous path once the queue has drained (quiescent A3 equivalence).
func TestAsyncMaintenanceMatchesSynchronousSemanticResult(t *testing.T) {
	ctx := context.Background()
	const marker = "asyncparity"

	syncSession, err := OpenSession(maintenanceConfig(t.TempDir(), marker, false, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, syncSession) })
	buildMaintenanceCorpus(t, syncSession, marker)
	syncResult, err := syncSession.SemanticSearch(ctx, marker)
	if err != nil {
		t.Fatalf("sync semantic: %v", err)
	}
	if len(syncResult.Materialized) == 0 {
		t.Fatalf("sync reference produced no materialization: %#v", syncResult)
	}

	asyncSession, err := OpenSession(maintenanceConfig(t.TempDir(), marker, true, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, asyncSession) })
	buildMaintenanceCorpus(t, asyncSession, marker)
	waitForMaintenanceDrained(t, asyncSession)
	asyncResult, err := asyncSession.SemanticSearch(ctx, marker)
	if err != nil {
		t.Fatalf("async semantic: %v", err)
	}
	if got, want := materializedSet(asyncResult), materializedSet(syncResult); !reflect.DeepEqual(got, want) {
		t.Fatalf("async materialization != sync:\n async=%#v\n  sync=%#v", got, want)
	}
}

// A bounded queue must never block the durable append, and overflow must
// reconcile from the ledger watermark to the same result as the sync path.
func TestAsyncMaintenanceAppendNeverBlocksAndReconciles(t *testing.T) {
	ctx := context.Background()
	const marker = "asyncbackpressure"

	ref, err := OpenSession(maintenanceConfig(t.TempDir(), marker, false, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, ref) })
	buildMaintenanceCorpus(t, ref, marker)
	for i := 0; i < 40; i++ {
		if err := ref.IngestUser(ctx, fmt.Sprintf("%s filler %d", marker, i)); err != nil {
			t.Fatal(err)
		}
	}
	refResult, err := ref.SemanticSearch(ctx, marker)
	if err != nil {
		t.Fatalf("ref semantic: %v", err)
	}
	if len(refResult.Materialized) == 0 {
		t.Fatal("ref produced no materialization")
	}

	// Depth-1 queue: bursts overflow, but appends must still succeed and the
	// index must reconcile from the ledger watermark.
	s, err := OpenSession(maintenanceConfig(t.TempDir(), marker, true, 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	buildMaintenanceCorpus(t, s, marker)
	for i := 0; i < 40; i++ {
		if err := s.IngestUser(ctx, fmt.Sprintf("%s filler %d", marker, i)); err != nil {
			t.Fatalf("append %d failed under bounded queue: %v", i, err)
		}
	}
	waitForMaintenanceDrained(t, s)
	got, err := s.SemanticSearch(ctx, marker)
	if err != nil {
		t.Fatalf("async semantic after reconcile: %v", err)
	}
	if !reflect.DeepEqual(materializedSet(refResult), materializedSet(got)) {
		t.Fatalf("reconciled result != reference:\n  got=%#v\n  ref=%#v", materializedSet(got), materializedSet(refResult))
	}

	// Force an explicit ledger reconciliation and confirm the index stays correct.
	s.markMaintenanceRepair()
	s.wakeIndexMaintenance()
	waitForMaintenanceDrained(t, s)
	forced, err := s.SemanticSearch(ctx, marker)
	if err != nil {
		t.Fatalf("async semantic after forced repair: %v", err)
	}
	if !reflect.DeepEqual(materializedSet(refResult), materializedSet(forced)) {
		t.Fatalf("forced-repair result != reference")
	}

	// After a real overflow reconciliation drains, the inspection surface must
	// settle back to caught-up rather than leaving stale degraded/reconciling.
	if st := s.MaintenanceStatus(); st.Degraded || st.Reconciling || st.Parked || st.Category != "ok" {
		t.Fatalf("post-reconcile status = %#v, want caught-up ok", st)
	}
}

// Close cancels-and-discards pending applies; reopen must catch the index up
// from the durable ledger watermark to the full synchronous result.
func TestAsyncMaintenanceCloseDiscardsThenReopenCatchesUp(t *testing.T) {
	ctx := context.Background()
	const marker = "asyncreopen"
	dir := t.TempDir()

	ref, err := OpenSession(maintenanceConfig(t.TempDir(), marker, false, 0))
	if err != nil {
		t.Fatal(err)
	}
	buildMaintenanceCorpus(t, ref, marker)
	refResult, err := ref.SemanticSearch(ctx, marker)
	if err != nil {
		t.Fatalf("ref semantic: %v", err)
	}
	if len(refResult.Materialized) == 0 {
		t.Fatal("ref produced no materialization")
	}
	closeTestSession(t, ref)

	s, err := OpenSession(maintenanceConfig(dir, marker, true, 0))
	if err != nil {
		t.Fatal(err)
	}
	buildMaintenanceCorpus(t, s, marker)
	// Close immediately; pending index applies may be discarded.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenSession(maintenanceConfig(dir, marker, true, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, reopened) })
	waitForMaintenanceDrained(t, reopened)
	got, err := reopened.SemanticSearch(ctx, marker)
	if err != nil {
		t.Fatalf("reopened semantic: %v", err)
	}
	if !reflect.DeepEqual(materializedSet(refResult), materializedSet(got)) {
		t.Fatalf("reopened result != reference:\n  got=%#v\n  ref=%#v", materializedSet(got), materializedSet(refResult))
	}
}

func TestAsyncMaintenanceStatusTracksAsyncFlag(t *testing.T) {
	async, err := OpenSession(maintenanceConfig(t.TempDir(), "asyncstatus", true, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, async) })
	if st := async.MaintenanceStatus(); !st.Async {
		t.Fatalf("async session reports Async=false: %#v", st)
	}
	buildMaintenanceCorpus(t, async, "asyncstatus")
	waitForMaintenanceDrained(t, async)
	if st := async.MaintenanceStatus(); st.Degraded || st.Parked || st.Pending != 0 || st.Category != "ok" {
		t.Fatalf("drained async maintenance status = %#v, want clean ok", st)
	}

	syncSession, err := OpenSession(maintenanceConfig(t.TempDir(), "syncstatus", false, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, syncSession) })
	buildMaintenanceCorpus(t, syncSession, "syncstatus")
	if st := syncSession.MaintenanceStatus(); st.Async {
		t.Fatalf("sync session reports Async=true: %#v", st)
	}
}

// A pending or in-flight ledger reconciliation means the index is provably
// behind the ledger, so the inspection surface must report it as degraded even
// with an empty queue and no recorded apply error. Otherwise an operator sees a
// false "caught up" while a rebuild-from-behind runs.
func TestMaintenanceStatusReflectsPendingRepair(t *testing.T) {
	s := &Session{asyncMaintenance: true}

	if st := s.MaintenanceStatus(); st.Degraded || st.Reconciling || st.Category != maintenanceStatusOK {
		t.Fatalf("clean async status = %#v, want caught-up ok", st)
	}

	// Overflow or a compile failure flags a pending reconciliation.
	s.markMaintenanceRepair()
	if st := s.MaintenanceStatus(); !st.Reconciling || !st.Degraded || st.Category != maintenanceStatusRepair {
		t.Fatalf("pending-repair status = %#v, want reconciling/degraded/repair", st)
	}

	// The rebuild clears needsRepair before it runs; the in-flight flag keeps the
	// status behind throughout the rebuild rather than briefly reading clean.
	s.maintenanceMu.Lock()
	s.maintenanceNeedsRepair = false
	s.maintenanceReconciling = true
	s.maintenanceMu.Unlock()
	if st := s.MaintenanceStatus(); !st.Reconciling || !st.Degraded || st.Category != maintenanceStatusRepair {
		t.Fatalf("in-flight rebuild status = %#v, want reconciling/degraded/repair", st)
	}

	s.clearReconciling()
	if st := s.MaintenanceStatus(); st.Reconciling || st.Degraded || st.Category != maintenanceStatusOK {
		t.Fatalf("post-repair status = %#v, want caught-up ok", st)
	}
}

// Concurrent appends and searches under async maintenance must never race or
// error unexpectedly. A lagging index legitimately abstains; only a real fault
// is a failure.
func TestAsyncMaintenanceConcurrentAppendsAndSearches(t *testing.T) {
	ctx := context.Background()
	const marker = "asyncconcurrent"
	s, err := OpenSession(maintenanceConfig(t.TempDir(), marker, true, 8))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestSession(t, s) })
	buildMaintenanceCorpus(t, s, marker)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			if err := s.IngestUser(ctx, fmt.Sprintf("%s probe %d", marker, i)); err != nil {
				t.Errorf("append during churn: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			_, err := s.SemanticSearch(ctx, marker)
			if err != nil && !errors.Is(err, retrieval.ErrSemanticPageMiss) && !errors.Is(err, retrieval.ErrIndexUnavailable) {
				t.Errorf("semantic during churn: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	waitForMaintenanceDrained(t, s)
	got, err := s.SemanticSearch(ctx, marker)
	if err != nil {
		t.Fatalf("final semantic: %v", err)
	}
	if len(got.Materialized) == 0 {
		t.Fatal("final async result empty after drain")
	}
}
