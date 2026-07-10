package observers

import (
	"context"
	"testing"

	"wsms/internal/ledger"
	"wsms/internal/wsl"
)

func TestToolDigestFailure(t *testing.T) {
	o := &ToolDigest{IDs: NewSeqIDGen()}
	ups, err := o.Handle(context.Background(), ledger.Event{
		Type: ledger.EventCommandOutput,
		Payload: map[string]any{
			"cmd":    "go test ./runtime -run TestCancelStream",
			"exit":   1,
			"output": "error: stream goroutine still blocked\nsrc/runtime/stream.go:118: boom",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 {
		t.Fatalf("len=%d", len(ups))
	}
	f := ups[0].Record.(*wsl.FailureRecord)
	if f.Cmd != "go test ./runtime -run TestCancelStream" {
		t.Fatalf("cmd=%q", f.Cmd)
	}
	if f.Exit != 1 {
		t.Fatalf("exit=%d", f.Exit)
	}
	if f.Err == "" {
		t.Fatal("empty err")
	}
	if f.FileHint == "" {
		t.Fatal("expected file hint")
	}
}

func TestToolDigestSuccessNoUpdate(t *testing.T) {
	o := &ToolDigest{IDs: NewSeqIDGen()}
	ups, err := o.Handle(context.Background(), ledger.Event{
		Type: ledger.EventCommandOutput,
		Payload: map[string]any{
			"cmd":  "go test",
			"exit": 0,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 0 {
		t.Fatalf("expected none, got %d", len(ups))
	}
}
