# WSMS — Working State Management System

Local runtime for LLM coding agents: append-only event ledger, deterministic observers, typed **WSL** working state, L0–L4 memory hierarchy, scheduler-managed L1 capsules, and page-fault tools.

The model context is a **resident working set**, not the session source of truth. Exact evidence lives in the ledger and content-addressed artifacts; WSL is the internal operational IR; the renderer emits ordinary structured-text capsules for hosted or local models.

The research framing in `docs/` describes this as *working-state virtual memory*; **WSMS** is the project and runtime that implements it.

## Language policy

| Role | Language |
|------|----------|
| Runtime (ledger, WSL, observers, scheduler, renderer, harness) | **Go** (default) |
| Later hotspots / ANN / Rust-only inference glue | **Rust** sidecar (not in scaffold) |
| Eval stats, plots, HF/torch research | **Python** under `research/` only |

Python must never own ledger truth, WSL authority, or capsule rendering.

## Docs

- [Architecture](docs/architecture.md) — package map and design boundaries
- [WSL v0](docs/wsl/v0.md) — grammar, records, lint rules
- Research drafts under `docs/`

## Layout

```text
cmd/wsms/          thin CLI
internal/          runtime packages (ledger, wsl, observers, …)
testdata/          fixtures
research/          analysis only (no runtime code yet)
docs/              design + WSL spec
```

## Develop

```bash
go test ./...
go test -race ./...
go build -o bin/wsms ./cmd/wsms
```

No API keys required for unit tests.

## Smoke path

```text
event → ledger.append → observers → WSL (lint) → scheduler → <working_state> capsule
                                                              ↘ page fault → exact evidence
```
