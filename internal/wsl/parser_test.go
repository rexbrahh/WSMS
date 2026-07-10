package wsl

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func testdata(name string) string {
	_, file, _, _ := runtime.Caller(0)
	// internal/wsl -> repo root
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	return filepath.Join(root, "testdata", name)
}

func TestParseSampleSession(t *testing.T) {
	data, err := os.ReadFile(testdata("sample_session.wsl"))
	if err != nil {
		t.Fatal(err)
	}
	recs, err := Parse(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 {
		t.Fatalf("len=%d", len(recs))
	}
	task := recs[0].(*TaskRecord)
	if task.IDValue != "T42" || task.Goal != "fix(stream_cancel_hang)" {
		t.Fatalf("task: %#v", task)
	}
	c := recs[1].(*ConstraintRecord)
	if c.Strength != "hard" || c.Text != "do not rewrite transport layer" {
		t.Fatalf("constraint: %#v", c)
	}
	f := recs[2].(*FailureRecord)
	if f.Exit != 1 || f.Cmd != "go test ./runtime -run TestCancelStream" {
		t.Fatalf("failure: %#v", f)
	}
	if f.Err != "stream goroutine still blocked" {
		t.Fatalf("err=%q", f.Err)
	}
}
