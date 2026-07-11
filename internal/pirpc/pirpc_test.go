package pirpc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// mockPi speaks enough of the pi RPC protocol to drive the client: it reads
// command lines and writes back a response (correlated by id) plus, for a
// prompt, a couple of streamed events.
func mockPi(t *testing.T) (*Client, func()) {
	t.Helper()
	cmdR, cmdW := io.Pipe() // client → mock (commands)
	outR, outW := io.Pipe() // mock → client (responses/events)

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(cmdR)
		for scanner.Scan() {
			var cmd struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
				continue
			}
			switch cmd.Type {
			case "prompt":
				writeLine(outW, map[string]any{"type": "response", "command": "prompt", "success": true, "id": cmd.ID})
				writeLine(outW, map[string]any{"type": "message_update", "delta": "hi"})
				writeLine(outW, map[string]any{"type": "agent_settled"})
			case "steer":
				writeLine(outW, map[string]any{"type": "response", "command": "steer", "success": false, "id": cmd.ID, "error": "boom"})
			case "get_state":
				writeLine(outW, map[string]any{"type": "response", "command": "get_state", "success": true, "id": cmd.ID, "data": map[string]any{"model": "mock-model"}})
			default:
				writeLine(outW, map[string]any{"type": "response", "command": cmd.Type, "success": true, "id": cmd.ID})
			}
		}
	}()

	client := New(cmdW, outR, func() error {
		_ = cmdW.Close()
		_ = outW.Close()
		return nil
	})
	cleanup := func() {
		_ = cmdW.Close()
		_ = outW.Close()
		_ = cmdR.Close()
		_ = outR.Close()
		<-done
	}
	return client, cleanup
}

func writeLine(w io.Writer, v any) {
	line, _ := json.Marshal(v)
	_, _ = w.Write(append(line, '\n'))
}

// TestPirpcMockHelper is not a real test: when PIRPC_BE_MOCK=1 the process acts
// as a mock pi (used by TestSpawnRealSubprocess via re-exec).
func TestPirpcMockHelper(t *testing.T) {
	if os.Getenv("PIRPC_BE_MOCK") != "1" {
		t.Skip("helper process only")
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var cmd struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(scanner.Bytes(), &cmd) != nil {
			continue
		}
		if cmd.Type == "prompt" {
			writeLine(os.Stdout, map[string]any{"type": "response", "command": "prompt", "success": true, "id": cmd.ID})
			writeLine(os.Stdout, map[string]any{"type": "message_update", "delta": "ok"})
			writeLine(os.Stdout, map[string]any{"type": "agent_settled"})
			continue
		}
		writeLine(os.Stdout, map[string]any{"type": "response", "command": cmd.Type, "success": true, "id": cmd.ID})
	}
}

// TestSpawnRealSubprocess exercises the real exec/pipe path by re-executing this
// test binary as a mock pi — the seam the in-memory mock cannot cover.
func TestSpawnRealSubprocess(t *testing.T) {
	if os.Getenv("PIRPC_BE_MOCK") == "1" {
		return // we are the child; the helper test does the work
	}
	t.Setenv("PIRPC_BE_MOCK", "1") // inherited by the spawned child

	client, err := Spawn(context.Background(), os.Args[0], "-test.run=TestPirpcMockHelper")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer client.Close()

	if err := client.Prompt("hi"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-client.Events():
			if !ok {
				t.Fatal("events closed before agent_settled")
			}
			if ev.Type == "agent_settled" {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for agent_settled from real subprocess")
		}
	}
}

func TestPromptResponseAndEvents(t *testing.T) {
	client, cleanup := mockPi(t)
	defer cleanup()

	if err := client.Prompt("hello"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	sawUpdate, sawSettled := false, false
	timeout := time.After(2 * time.Second)
	for !sawSettled {
		select {
		case ev := <-client.Events():
			switch ev.Type {
			case "message_update":
				sawUpdate = true
			case "agent_settled":
				sawSettled = true
			}
		case <-timeout:
			t.Fatal("timed out waiting for agent_settled")
		}
	}
	if !sawUpdate {
		t.Fatal("expected a message_update event before settling")
	}
}

func TestErrorResponseSurfaces(t *testing.T) {
	client, cleanup := mockPi(t)
	defer cleanup()

	err := client.Steer("x")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected steer error 'boom', got %v", err)
	}
}

func TestGetStateData(t *testing.T) {
	client, cleanup := mockPi(t)
	defer cleanup()

	data, err := client.GetState()
	if err != nil {
		t.Fatalf("get_state: %v", err)
	}
	var state struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.Model != "mock-model" {
		t.Fatalf("state model = %q", state.Model)
	}
}

// When the transport dies, in-flight and subsequent commands must fail rather
// than hang, and the event stream must close.
func TestTransportEOFFailsRequests(t *testing.T) {
	client, cleanup := mockPi(t)
	cleanup() // close both pipes → client sees EOF

	if _, err := client.GetState(); err == nil {
		t.Fatal("expected error after transport EOF")
	}
	select {
	case _, open := <-client.Events():
		if open {
			// drain until closed
			for range client.Events() {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events channel was not closed after EOF")
	}
}
