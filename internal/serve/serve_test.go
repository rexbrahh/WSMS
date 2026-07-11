package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wsms/internal/config"
	"wsms/internal/harness"
)

func newTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.SessionID = "serve-test"
	sess, err := harness.OpenSession(cfg)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	srv := &server{session: sess, cfg: cfg, token: token}
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(func() {
		ts.Close()
		_ = sess.Close()
	})
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request: %v", err)
		}
	}
	req, err := http.NewRequest(method, ts.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s %s response: %v", method, path, err)
	}
	return resp.StatusCode, out
}

func TestHealthAndTurnLoop(t *testing.T) {
	ts := newTestServer(t, "")

	if code, body := do(t, ts, http.MethodGet, "/health", "", nil); code != 200 || body["ok"] != true {
		t.Fatalf("health = %d %v", code, body)
	}

	if code, _ := do(t, ts, http.MethodPost, "/task/start", "", map[string]any{
		"goal": "wire the serve endpoint", "task_id": "T-1", "phase": "9", "priority": "P1",
	}); code != 200 {
		t.Fatalf("task/start = %d", code)
	}

	if code, _ := do(t, ts, http.MethodPost, "/ingest/user", "", map[string]any{
		"text": "please expose the session over HTTP",
	}); code != 200 {
		t.Fatalf("ingest/user = %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/before_turn", "", nil)
	if code != 200 {
		t.Fatalf("before_turn = %d %v", code, body)
	}
	// The capsule is rendered through WSMS's working-state normalization
	// (capitalized, punctuated), so match the normalized substring rather than
	// the raw ingest text — this confirms the ingest→capsule path is live.
	capsule, _ := body["capsule"].(string)
	if !strings.Contains(strings.ToLower(capsule), "serve endpoint") {
		t.Fatalf("capsule missing the active goal: %q", capsule)
	}

	if code, _ := do(t, ts, http.MethodPost, "/decision", "", map[string]any{
		"chosen": "localhost HTTP/JSON", "because": "two clients need the core", "scope": "task",
	}); code != 200 {
		t.Fatalf("decision = %d", code)
	}
}

// A payload the ledger rejects as malformed is the caller's fault, so the HTTP
// boundary must answer 400 — not 500, which would read as a core outage.
func TestInvalidPayloadIs400(t *testing.T) {
	ts := newTestServer(t, "")
	code, body := do(t, ts, http.MethodPost, "/decision", "", map[string]any{
		"chosen": "x", "scope": "galaxy", // not one of global|repo|branch|task|file
	})
	if code != 400 {
		t.Fatalf("invalid scope should be 400, got %d %v", code, body)
	}
}

// A fresh session has no warm index, so semantic recall must abstain — a valid
// "no memory" answer, not a 500. This is the cache-never-fabricates invariant
// surfaced at the HTTP boundary.
func TestSemanticAbstainsCleanly(t *testing.T) {
	ts := newTestServer(t, "")
	code, body := do(t, ts, http.MethodPost, "/semantic", "", map[string]any{"query": "anything at all"})
	if code != 200 {
		t.Fatalf("semantic = %d %v", code, body)
	}
	if body["abstained"] != true {
		t.Fatalf("expected abstention on a fresh session, got %v", body)
	}
	if _, ok := body["materialized"].([]any); !ok {
		t.Fatalf("materialized should be present (possibly empty), got %v", body["materialized"])
	}
}

func TestVizStateShape(t *testing.T) {
	ts := newTestServer(t, "")
	code, body := do(t, ts, http.MethodGet, "/viz/state", "", nil)
	if code != 200 {
		t.Fatalf("viz/state = %d %v", code, body)
	}
	for _, key := range []string{"capsule", "residency", "maintenance", "embedding"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("viz/state missing %q: %v", key, body)
		}
	}
}

// A rebound DNS name resolving to 127.0.0.1 still carries the attacker's Host,
// so a non-loopback Host must be refused even though the socket is loopback.
func TestRejectsNonLoopbackHost(t *testing.T) {
	ts := newTestServer(t, "")
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/ingest/user", strings.NewReader(`{"text":"x"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-loopback Host should be 403, got %d", resp.StatusCode)
	}
}

// A form-encoded POST is a "simple request" that skips CORS preflight; refusing
// non-JSON forces the preflight this server never answers.
func TestRequiresJSONContentType(t *testing.T) {
	ts := newTestServer(t, "")
	resp, err := http.Post(ts.URL+"/ingest/user", "text/plain", strings.NewReader(`{"text":"x"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON content-type should be 415, got %d", resp.StatusCode)
	}
}

// A cross-site page always sends an Origin; our local clients never do.
func TestRejectsBrowserOrigin(t *testing.T) {
	ts := newTestServer(t, "")
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/ingest/user", strings.NewReader(`{"text":"x"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin request should be 403, got %d", resp.StatusCode)
	}
}

func TestRefusesNonLoopbackBind(t *testing.T) {
	err := Run(context.Background(), Options{Addr: "0.0.0.0:0", DataDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("expected non-loopback bind refusal, got %v", err)
	}
}

func TestTokenGuard(t *testing.T) {
	const token = "s3cr3t"
	ts := newTestServer(t, token)

	// /health stays open so clients can probe before authenticating.
	if code, _ := do(t, ts, http.MethodGet, "/health", "", nil); code != 200 {
		t.Fatalf("health should be open, got %d", code)
	}
	if code, _ := do(t, ts, http.MethodPost, "/ingest/user", "", map[string]any{"text": "x"}); code != 401 {
		t.Fatalf("missing token should be 401, got %d", code)
	}
	if code, _ := do(t, ts, http.MethodPost, "/ingest/user", token, map[string]any{"text": "x"}); code != 200 {
		t.Fatalf("valid token should be 200, got %d", code)
	}
}
