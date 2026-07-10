package demo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wsms/internal/harness"
)

func TestRunEndToEndPreservesExplicitDataDir(t *testing.T) {
	dataDir := t.TempDir()
	marker := filepath.Join(dataDir, "operator-owned.txt")
	if err := os.WriteFile(marker, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := Run(context.Background(), &output, Options{DataDir: dataDir}); err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, output.String())
	}
	transcript := output.String()
	required := []string{
		"BACKING STORE: EVENT E0001 task_started -> T1",
		"BACKING STORE: EVENT E0002 user_instruction -> C1 hard",
		"BACKING STORE: EVENT E0003 command_output exit=1 -> F1",
		"BACKING STORE: ARTIFACT sha256:",
		"BACKING STORE: EVENT E0004 decision -> D1,A1",
		"BACKING STORE: EVENT E0005 next_action -> next",
		"PAGE TABLE: T1<-E0001 C1<-E0002 F1<-E0003 D1<-E0004 A1<-E0004 next<-E0005",
		"RESIDENT WORKING SET: CAPSULE BEFORE REOPEN",
		"=== SESSION RUNTIME CLOSED / MEMORY DROPPED / REOPENED ===",
		"PAGE TABLE: DERIVED MAPPINGS RECONSTRUCTED",
		"PAGE FAULT F1: PAGE-IN HIT",
		"BACKING STORE F1: SHA256 VERIFIED",
		"BACKING STORE F1: RAW SENTINEL VERIFIED",
		"SESSION ISOLATION: VERIFIED",
		"CLIENT: CAPSULE RECEIVED through harness.Loop",
		"CLIENT: CONTINUATION RECEIVED",
	}
	for _, marker := range required {
		if !strings.Contains(transcript, marker) {
			t.Errorf("transcript missing %q:\n%s", marker, transcript)
		}
	}
	if !strings.HasSuffix(transcript, "DEMO PASS\n") {
		t.Fatalf("success marker is not the final line:\n%s", transcript)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve me" {
		t.Fatalf("operator file changed: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "ledger.db")); err != nil {
		t.Fatalf("persistent ledger missing: %v", err)
	}

	primary, err := harness.OpenSession(demoConfig(dataDir, primarySessionID))
	if err != nil {
		t.Fatalf("reopen persistent primary proof: %v", err)
	}
	defer func() {
		if err := primary.Close(); err != nil {
			t.Errorf("close persistent primary proof: %v", err)
		}
	}()
	events, err := primary.Ledger.ListBySession(context.Background(), primarySessionID)
	if err != nil || len(events) != 7 {
		t.Fatalf("persistent primary events=%d err=%v, want 7", len(events), err)
	}
	if err := requireProvenance(primary.State, map[string]string{
		"T1": "E0001", "C1": "E0002", "F1": "E0003", "D1": "E0004", "A1": "E0004", "next": "E0005",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := primary.Tools.ReadRawLog(context.Background(), "F1", 4096)
	if err != nil || raw != demoRawOutput() {
		t.Fatalf("persistent raw proof mismatch: err=%v", err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(raw))); got != events[2].ArtifactHash {
		t.Fatalf("persistent artifact hash=%s, want %s", got, events[2].ArtifactHash)
	}

	secondary, err := harness.OpenSession(demoConfig(dataDir, secondarySessionID))
	if err != nil {
		t.Fatalf("reopen persistent secondary proof: %v", err)
	}
	defer func() {
		if err := secondary.Close(); err != nil {
			t.Errorf("close persistent secondary proof: %v", err)
		}
	}()
	secondaryEvents, err := secondary.Ledger.ListBySession(context.Background(), secondarySessionID)
	if err != nil || len(secondaryEvents) != 1 || secondaryEvents[0].ID != "E0001" {
		t.Fatalf("persistent secondary events=%#v err=%v", secondaryEvents, err)
	}
	if err := requireProvenance(secondary.State, map[string]string{"T1": "E0001"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunLateWriterFailureCleansTemporaryDataWithoutSuccessMarker(t *testing.T) {
	writer := &failOnMarkerWriter{marker: "CLIENT: CAPSULE RECEIVED"}
	err := Run(context.Background(), writer, Options{})
	if err == nil || !strings.Contains(err.Error(), "injected writer failure") {
		t.Fatalf("Run error=%v, want injected late writer failure", err)
	}
	transcript := writer.buf.String()
	if !strings.Contains(transcript, "SESSION ISOLATION: VERIFIED") {
		t.Fatalf("writer did not fail late enough to exercise populated sessions:\n%s", transcript)
	}
	if strings.Contains(transcript, "DEMO PASS") {
		t.Fatalf("failed run printed success marker:\n%s", transcript)
	}
	firstLine, _, ok := strings.Cut(transcript, "\n")
	if !ok {
		t.Fatalf("missing data-directory line:\n%s", transcript)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(firstLine, "DEMO DATA: "), " (temporary)")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("temporary data survived late failure: stat error=%v", statErr)
	}
}

func TestRunRejectsShortWriteAndCleansTemporaryData(t *testing.T) {
	writer := &shortWriter{}
	err := Run(context.Background(), writer, Options{})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Run error=%v, want io.ErrShortWrite", err)
	}
	transcript := writer.buf.String()
	if strings.Contains(transcript, "DEMO PASS") {
		t.Fatalf("short write emitted success marker: %q", transcript)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(transcript, "DEMO DATA: "), " (temporary)")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("temporary data survived short write: stat error=%v", statErr)
	}
}

type failOnMarkerWriter struct {
	buf    bytes.Buffer
	marker string
}

func (w *failOnMarkerWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(w.marker)) {
		return 0, errors.New("injected writer failure")
	}
	return w.buf.Write(p)
}

type shortWriter struct {
	buf bytes.Buffer
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return w.buf.Write(p[:len(p)-1])
}

func TestRunRejectsReusedDemoSessionWithoutSuccessMarker(t *testing.T) {
	dataDir := t.TempDir()
	var first bytes.Buffer
	if err := Run(context.Background(), &first, Options{DataDir: dataDir}); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	var second bytes.Buffer
	err := Run(context.Background(), &second, Options{DataDir: dataDir})
	if err == nil || !strings.Contains(err.Error(), "reserved demo path already exists") {
		t.Fatalf("second Run error=%v, want occupied-session rejection", err)
	}
	if strings.Contains(second.String(), "DEMO PASS") {
		t.Fatalf("failed run printed success marker:\n%s", second.String())
	}
}

func TestRunRefusesReservedOperatorPathWithoutModification(t *testing.T) {
	dataDir := t.TempDir()
	reserved := filepath.Join(dataDir, "ledger.db")
	want := []byte("operator-owned database placeholder")
	if err := os.WriteFile(reserved, want, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := Run(context.Background(), &output, Options{DataDir: dataDir})
	if err == nil || !strings.Contains(err.Error(), "reserved demo path already exists") {
		t.Fatalf("Run error=%v, want reserved-path rejection", err)
	}
	got, readErr := os.ReadFile(reserved)
	if readErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("reserved operator file changed: got=%q err=%v", got, readErr)
	}
	if output.Len() != 0 || strings.Contains(output.String(), "DEMO PASS") {
		t.Fatalf("preflight rejection emitted demo evidence: %q", output.String())
	}
	for _, name := range []string{"ledger.db-journal", "ledger.db-wal", "ledger.db-shm", "artifacts"} {
		if _, statErr := os.Lstat(filepath.Join(dataDir, name)); !os.IsNotExist(statErr) {
			t.Fatalf("preflight created %s: stat error=%v", name, statErr)
		}
	}
}

func TestRunRefusesOperatorOwnedRollbackJournalWithoutModification(t *testing.T) {
	dataDir := t.TempDir()
	reserved := filepath.Join(dataDir, "ledger.db-journal")
	want := []byte("operator-owned rollback journal")
	if err := os.WriteFile(reserved, want, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := Run(context.Background(), &output, Options{DataDir: dataDir})
	if err == nil || !strings.Contains(err.Error(), "ledger.db-journal") {
		t.Fatalf("Run error=%v, want rollback-journal preflight rejection", err)
	}
	got, readErr := os.ReadFile(reserved)
	if readErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("operator rollback journal changed: got=%q err=%v", got, readErr)
	}
	if output.Len() != 0 {
		t.Fatalf("preflight rejection emitted output: %q", output.String())
	}
	for _, name := range []string{"ledger.db", "ledger.db-wal", "ledger.db-shm", "artifacts"} {
		if _, statErr := os.Lstat(filepath.Join(dataDir, name)); !os.IsNotExist(statErr) {
			t.Fatalf("preflight created %s: stat error=%v", name, statErr)
		}
	}
}

func TestRunDefaultTemporaryDataIsRemovedBeforeSuccess(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), &output, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	firstLine, _, ok := strings.Cut(output.String(), "\n")
	if !ok || !strings.HasPrefix(firstLine, "DEMO DATA: ") || !strings.HasSuffix(firstLine, " (temporary)") {
		t.Fatalf("unexpected data-directory line %q", firstLine)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(firstLine, "DEMO DATA: "), " (temporary)")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temporary demo directory still exists: stat error=%v", err)
	}
	if !strings.HasSuffix(output.String(), "DEMO PASS\n") {
		t.Fatalf("success marker missing or not final:\n%s", output.String())
	}
}
