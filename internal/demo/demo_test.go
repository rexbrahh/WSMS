package demo

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"=== RUNTIME MEMORY DROPPED / PROCESS REOPEN ===",
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
}

func TestRunRejectsReusedDemoSessionWithoutSuccessMarker(t *testing.T) {
	dataDir := t.TempDir()
	var first bytes.Buffer
	if err := Run(context.Background(), &first, Options{DataDir: dataDir}); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	var second bytes.Buffer
	err := Run(context.Background(), &second, Options{DataDir: dataDir})
	if err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("second Run error=%v, want occupied-session rejection", err)
	}
	if strings.Contains(second.String(), "DEMO PASS") {
		t.Fatalf("failed run printed success marker:\n%s", second.String())
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
