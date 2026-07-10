# WSMS — Working State Management System

Local runtime for LLM coding agents: append-only event ledger, deterministic observers, typed **WSL** working state, L0–L4 memory hierarchy, scheduler-managed L1 capsules, and page-fault tools.

The model context is a **resident working set**, not the session source of truth. Exact evidence lives in the ledger and content-addressed artifacts; WSL is the internal operational IR; the renderer emits ordinary structured-text capsules for hosted or local models.

The research framing in `docs/` describes this as *working-state virtual memory*; **WSMS** is the project and runtime that implements it.

## Language policy

| Role | Language |
|------|----------|
| Runtime (ledger, WSL, observers, scheduler, renderer, harness) | **Go** (default) |
| Optional measured ANN scale-out / local inference glue | **Rust** sidecar or supervised service (not in scaffold) |
| Eval stats, plots, HF/torch research | **Python** under `research/` only |

Python must never own ledger truth, WSL authority, or capsule rendering.

## Docs

- [Product specification](docs/specification.md) — normative behavior and acceptance criteria
- [Architecture](docs/architecture.md) — package map, Unix VM correspondence, and design boundaries
- [L3 warm memory](docs/l3-warm-memory.md) — hybrid lexical/vector semantic paging, backends, safety, and rollout gates
- [Implementation plan](docs/implementation-plan.md) — verified demo slice and staged productization
- [WSL v0](docs/wsl/v0.md) — grammar, records, lint rules
- Phase 7A frozen corpus: `testdata/pages/corpus/` (page compiler goldens + labeled queries)
- Research drafts under `docs/`

The L3 design uses vector retrieval as a disposable working-set estimator, not
as the source of truth: known IDs always page directly from exact evidence;
semantic queries use FTS plus dense retrieval to discover candidate page refs,
then rejoin the same validated L4-to-L2 page-in path. The first demo remains a
deterministic vector-free proof of the paging mechanism. Phase 7A adds an
offline deterministic page compiler and exact cosine oracle. Phase 7B adds a
disposable FTS5 warm index under `<data-dir>/index/` (safe to delete). Phase 7C
adds an optional sqlite-vec dense projection (config `DenseDimensions`; off by
default). The demo path remains a vector-free mechanism proof and does not
require the index or dense search.

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

## Run the mechanism demo

```bash
make demo
```

Equivalently, run `go run ./cmd/wsms demo`. The command uses a fresh temporary
data directory and proves the complete vector-free path: ledger/artifact
backing storage, event-derived WSL mappings, a bounded resident capsule, a real
close/reopen reconstruction, direct `F1` page-in, independently verified raw
artifact bytes, the foreground client boundary, and same-database session
isolation. It prints `DEMO PASS` only after every assertion and resource close
succeeds.

To keep the SQLite ledger and content-addressed artifacts for inspection, give
the demo a fresh persistent directory:

```bash
go run ./cmd/wsms demo --data-dir /tmp/wsms-demo-inspect
```

The command never removes an explicitly supplied directory or unrelated files
inside it. For safety it refuses the reserved paths `ledger.db`,
`ledger.db-journal`, `ledger.db-wal`, `ledger.db-shm`, or `artifacts` when any
already exists, so use a fresh directory for each persistent run. This prevents
old or unrelated storage from being mutated or mistaken for fresh evidence.

## Smoke path

```text
event → ledger.append → observers → WSL (lint) → scheduler → <working_state> capsule
                                                              ↘ page fault → exact evidence
```
