// Package pirpc is a Go client for the pi harness RPC protocol (pi --mode rpc).
//
// The wire format is strict LF-framed JSONL over the child's stdin/stdout
// (see pi's packages/coding-agent/src/modes/rpc/jsonl.ts). Outbound commands
// carry an "id"; an inbound line with type "response" and a matching id
// resolves that request, and every other line is a streamed session event.
// This mirrors pi's own reference client (rpc-client.ts) so the two stay
// protocol-compatible.
//
// The transport is injected (stdin io.Writer, stdout io.Reader) so the client
// is testable against an in-memory mock; Spawn wires it to a real child.
package pirpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// requestTimeout bounds how long a command waits for its response line.
const requestTimeout = 30 * time.Second

// maxLine caps a single JSONL record; streamed message events can be large.
const maxLine = 8 << 20

// Event is a streamed session event (any inbound line that is not a response).
// Raw is the full JSON line so callers can decode whichever event types matter.
type Event struct {
	Type string
	Raw  json.RawMessage
}

type response struct {
	success bool
	errMsg  string
	data    json.RawMessage
}

type pendingResult struct {
	resp response
	err  error
}

// Client speaks the pi RPC protocol over an injected transport.
type Client struct {
	w      io.Writer
	events chan Event

	mu      sync.Mutex
	nextID  int
	pending map[string]chan pendingResult
	err     error // terminal transport error, once set

	closeOnce sync.Once
	closeFn   func() error
}

// New starts a client over the given transport. stdout is read on a background
// goroutine until EOF. closeFn (may be nil) is invoked by Close to tear down the
// underlying process/pipes.
func New(stdin io.Writer, stdout io.Reader, closeFn func() error) *Client {
	c := &Client{
		w:       stdin,
		events:  make(chan Event, 256),
		pending: make(map[string]chan pendingResult),
		closeFn: closeFn,
	}
	go c.readLoop(stdout)
	return c
}

// Spawn launches `name args...` and returns a client bound to its stdio. The
// caller is expected to have appended pi's "--mode rpc" (and any provider/model
// flags) to args. Child stderr is forwarded to the parent's stderr.
func Spawn(ctx context.Context, name string, args ...string) (*Client, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return New(stdin, stdout, func() error {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		return cmd.Wait()
	}), nil
}

// Events returns the stream of session events. It is closed when the transport
// reaches EOF.
func (c *Client) Events() <-chan Event { return c.events }

// Prompt sends a user prompt; streaming output arrives as events.
func (c *Client) Prompt(message string) error {
	return c.ack(map[string]any{"type": "prompt", "message": message})
}

// Steer queues a message that interrupts the agent mid-run.
func (c *Client) Steer(message string) error {
	return c.ack(map[string]any{"type": "steer", "message": message})
}

// Abort cancels the current operation.
func (c *Client) Abort() error {
	return c.ack(map[string]any{"type": "abort"})
}

// GetState returns the raw session-state payload.
func (c *Client) GetState() (json.RawMessage, error) {
	resp, err := c.send(map[string]any{"type": "get_state"})
	if err != nil {
		return nil, err
	}
	return resp.data, nil
}

// SetModel selects a provider/model.
func (c *Client) SetModel(provider, modelID string) error {
	return c.ack(map[string]any{"type": "set_model", "provider": provider, "modelId": modelID})
}

// Close tears down the transport and fails any in-flight requests.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.closeFn != nil {
			err = c.closeFn()
		}
	})
	return err
}

// ack sends a command and requires a successful response with no payload.
func (c *Client) ack(cmd map[string]any) error {
	_, err := c.send(cmd)
	return err
}

func (c *Client) send(cmd map[string]any) (response, error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return response{}, c.err
	}
	c.nextID++
	id := fmt.Sprintf("req_%d", c.nextID)
	cmd["id"] = id
	line, err := json.Marshal(cmd)
	if err != nil {
		c.mu.Unlock()
		return response{}, err
	}
	ch := make(chan pendingResult, 1)
	c.pending[id] = ch
	_, werr := c.w.Write(append(line, '\n'))
	c.mu.Unlock()

	if werr != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return response{}, werr
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return response{}, res.err
		}
		if !res.resp.success {
			return response{}, errors.New(res.resp.errMsg)
		}
		return res.resp, nil
	case <-time.After(requestTimeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return response{}, fmt.Errorf("pirpc: timeout awaiting response to %v", cmd["type"])
	}
}

func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)

	for scanner.Scan() {
		line := scanner.Bytes()
		var head struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Success bool   `json:"success"`
			Error   string `json:"error"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			continue // ignore non-JSON lines, as pi's own client does
		}
		if head.Type == "response" && head.ID != "" && c.deliver(head.ID, response{
			success: head.Success, errMsg: head.Error, data: head.Data,
		}) {
			continue
		}
		// Copy: scanner reuses its buffer across Scan calls.
		raw := make(json.RawMessage, len(line))
		copy(raw, line)
		c.events <- Event{Type: head.Type, Raw: raw}
	}

	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.fail(err)
}

// deliver routes a response to its waiting request; returns false if no request
// is pending for the id (then it is treated as an event).
func (c *Client) deliver(id string, resp response) bool {
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	ch <- pendingResult{resp: resp}
	return true
}

// fail records the terminal error, rejects pending requests, and closes events.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	pending := c.pending
	c.pending = make(map[string]chan pendingResult)
	c.mu.Unlock()

	for _, ch := range pending {
		ch <- pendingResult{err: err}
	}
	close(c.events)
}
