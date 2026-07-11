// Package serve exposes a running WSMS session over a localhost HTTP/JSON API.
//
// It is the Phase 9 "WSMS core" process: the authoritative memory service that
// the forked pi harness (via a bridge extension) and the bubbletea TUI both
// call as independent clients. The API only wraps existing Session methods —
// the durable ledger (L4) remains truth and the HTTP layer never becomes an
// authority of its own. Binding is loopback-only; an optional bearer token
// guards against other local processes when set.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"wsms/internal/config"
	"wsms/internal/harness"
	"wsms/internal/ledger"
	"wsms/internal/retrieval"
	"wsms/internal/types"
)

// Options configures a serve run. The zero value is not valid; use Run with an
// explicitly populated Options (cmd/wsms wires the flags).
type Options struct {
	Addr             string // loopback host:port; ":0" or "127.0.0.1:0" picks a free port
	DataDir          string
	SessionID        string
	Token            string // optional bearer token; empty disables the guard
	AsyncMaintenance bool
	// Ready, when non-nil, receives the actual listen address once bound. It lets
	// callers (and tests) learn the chosen port when Addr requested port 0.
	Ready chan<- string
}

type server struct {
	session *harness.Session
	cfg     config.Config
	token   string
}

// Run opens a session, serves the API until ctx is cancelled, then closes the
// session. It blocks until shutdown completes.
func Run(ctx context.Context, opts Options) error {
	cfg := config.Default()
	if opts.DataDir != "" {
		cfg.DataDir = opts.DataDir
	}
	if opts.SessionID != "" {
		cfg.SessionID = opts.SessionID
	}
	cfg.AsyncMaintenance = opts.AsyncMaintenance

	session, err := harness.OpenSession(cfg)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = session.Close() }()

	srv := &server{session: session, cfg: cfg, token: opts.Token}

	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	if opts.Ready != nil {
		opts.Ready <- ln.Addr().String()
	}

	httpSrv := &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/before_turn", s.guard(s.handleBeforeTurn))
	mux.HandleFunc("/ingest/user", s.guard(s.handleIngestUser))
	mux.HandleFunc("/ingest/assistant", s.guard(s.handleIngestAssistant))
	mux.HandleFunc("/ingest/command", s.guard(s.handleIngestCommand))
	mux.HandleFunc("/task/start", s.guard(s.handleTaskStart))
	mux.HandleFunc("/decision", s.guard(s.handleDecision))
	mux.HandleFunc("/next", s.guard(s.handleNext))
	mux.HandleFunc("/page", s.guard(s.handlePage))
	mux.HandleFunc("/semantic", s.guard(s.handleSemantic))
	mux.HandleFunc("/viz/state", s.guard(s.handleVizState))
	return mux
}

// guard enforces POST + the optional bearer token. /health stays open so a
// client can probe readiness before authenticating.
func (s *server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			writeErr(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		next(w, r)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": s.cfg.SessionID})
}

func (s *server) handleBeforeTurn(w http.ResponseWriter, r *http.Request) {
	capsule, err := s.session.BeforeTurn(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capsule": capsule})
}

func (s *server) handleIngestUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.session.IngestUser(r.Context(), req.Text); err != nil {
		writeSessionErr(w, err)
		return
	}
	writeOK(w)
}

func (s *server) handleIngestAssistant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.session.IngestAssistant(r.Context(), req.Text); err != nil {
		writeSessionErr(w, err)
		return
	}
	writeOK(w)
}

func (s *server) handleIngestCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
		Exit    int    `json:"exit"`
		Output  string `json:"output"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.session.IngestCommandOutput(r.Context(), req.Command, req.Exit, req.Output); err != nil {
		writeSessionErr(w, err)
		return
	}
	writeOK(w)
}

func (s *server) handleTaskStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Goal     string `json:"goal"`
		TaskID   string `json:"task_id"`
		Repo     string `json:"repo"`
		Phase    string `json:"phase"`
		Priority string `json:"priority"`
		Branch   string `json:"branch"`
		Commit   string `json:"commit"`
		Dirty    string `json:"dirty"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.session.StartTask(r.Context(), harness.TaskStart{
		Goal: req.Goal, TaskID: req.TaskID, Repo: req.Repo, Phase: req.Phase,
		Priority: types.Priority(req.Priority), Branch: req.Branch, Commit: req.Commit, Dirty: req.Dirty,
	})
	if err != nil {
		writeSessionErr(w, err)
		return
	}
	writeOK(w)
}

func (s *server) handleDecision(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chosen    string `json:"chosen"`
		Because   string `json:"because"`
		Refs      string `json:"refs"`
		Scope     string `json:"scope"`
		AvoidText string `json:"avoid_text"`
		AvoidRef  string `json:"avoid_ref"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.session.RecordDecision(r.Context(), harness.DecisionInput{
		Chosen: req.Chosen, Because: req.Because, Refs: req.Refs,
		Scope: types.Scope(req.Scope), AvoidText: req.AvoidText, AvoidRef: req.AvoidRef,
	})
	if err != nil {
		writeSessionErr(w, err)
		return
	}
	writeOK(w)
}

func (s *server) handleNext(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action   string `json:"action"`
		Target   string `json:"target"`
		Question string `json:"question"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.session.SetNext(r.Context(), harness.NextAction{
		Action: req.Action, Target: req.Target, Question: req.Question,
	})
	if err != nil {
		writeSessionErr(w, err)
		return
	}
	writeOK(w)
}

func (s *server) handlePage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Budget int    `json:"budget"`
	}
	if !decode(w, r, &req) {
		return
	}
	budget := req.Budget
	if budget <= 0 {
		budget = s.cfg.PageFaultTokenBudget
	}
	body, err := s.session.Tools.ReadPage(r.Context(), req.ID, budget)
	if err != nil {
		// A miss is not an operational failure; report it as a bounded, typed
		// absence so the agent tool can say "no such page" rather than crash.
		writeJSON(w, http.StatusOK, map[string]any{"found": false, "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": true, "body": body})
}

type materializedPageDTO struct {
	PageID   string   `json:"page_id"`
	Evidence []string `json:"evidence"`
}

func (s *server) handleSemantic(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
	}
	if !decode(w, r, &req) {
		return
	}
	result, err := s.session.SemanticSearch(r.Context(), req.Query)
	if err != nil {
		// Abstention (lagging/unavailable index or no eligible page) is a valid
		// "no memory found" answer, not an error: the cache must never fabricate.
		if errors.Is(err, retrieval.ErrIndexUnavailable) || errors.Is(err, retrieval.ErrSemanticPageMiss) {
			writeJSON(w, http.StatusOK, map[string]any{
				"abstained": true, "reason": err.Error(), "materialized": []materializedPageDTO{},
			})
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pages := make([]materializedPageDTO, 0, len(result.Materialized))
	for _, m := range result.Materialized {
		pages = append(pages, materializedPageDTO{PageID: string(m.PageID), Evidence: m.Evidence})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"abstained":    false,
		"materialized": pages,
		"explanation":  result.Explanation,
		"degraded":     result.Degraded,
	})
}

func (s *server) handleVizState(w http.ResponseWriter, r *http.Request) {
	capsule, err := s.session.BeforeTurn(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	maint := s.session.MaintenanceStatus()
	embed := s.session.EmbeddingStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"capsule":     capsule,
		"residency":   s.session.ResidencySnapshot(),
		"maintenance": maint,
		"embedding":   embed,
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST required"))
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return false
	}
	return true
}

func writeOK(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]any{"ok": true}) }

// writeSessionErr classifies an append-path error for the HTTP boundary: a
// payload the ledger rejects as malformed is the caller's fault (400), while
// anything else is a genuine core failure (500). The distinction lets the
// bridge and TUI react correctly — fix the request vs. surface an outage.
func writeSessionErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ledger.ErrInvalidEvent) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
