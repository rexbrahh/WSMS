# WSMS — Working State Management System

Memory management runtime for agents: append-only event ledger, deterministic observers, typed **WSL** working state, L0–L4 memory hierarchy, scheduler-managed L1 capsules, and page-fault tools.

The model context is a **resident working set**, not the session source of truth. Exact evidence lives in the ledger and content-addressed artifacts; WSL is the internal operational IR; the renderer emits ordinary structured-text capsules for hosted or local models.

The research framing in `docs/` describes this as _working-state virtual memory_; **WSMS** is the project and runtime that implements it.

## Language policy

| Role                                                           | Language                                                                                       |
| -------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Runtime (ledger, WSL, observers, scheduler, renderer, harness) | **Go** (default)                                                                               |
| Optional measured ANN scale-out / local inference service      | **Rust** sidecar or supervised service (client boundary exists; service binary is not bundled) |
| Eval stats, plots, HF/torch research                           | **Python** under `research/` only                                                              |

Python must never own ledger truth, WSL authority, or capsule rendering.

## Docs

- [Product specification](docs/specification.md) — normative behavior and acceptance criteria
- [Architecture](docs/architecture.md) — package map, Unix VM correspondence, and design boundaries
- [L3 warm memory](docs/l3-warm-memory.md) — hybrid lexical/vector semantic paging, backends, safety, and rollout gates
- [Implementation plan](docs/implementation-plan.md) — verified demo slice and staged productization
- [WSL v0](docs/wsl/v0.md) — grammar, records, lint rules
- Phase 7A strict replay corpus: `internal/pages/testdata/frozen_corpus.json` (legacy compiler goldens remain under `testdata/pages/corpus/`)
- Research drafts under `docs/`

The L3 design uses vector retrieval as a disposable working-set estimator, not as the source of truth: known WSL/event IDs page directly from exact evidence; Phase 7E uses lexical and dense channels only to discover candidate page refs, then rejoins the validated L4 materialization path. The first demo remains a deterministic vector-free proof of the paging mechanism. Phase 7A adds the deterministic page compiler and exact cosine oracle; Phase 7B adds a disposable FTS5 warm index under `<data-dir>/index/`; Phase 7C adds the optional sqlite-vec dense projection (`DenseDimensions`, off by default); and Phase 7D adds the supervised private embedder ABI plus asynchronous tuple-validated vector backfill.

The implemented Phase 7E semantic-fault path prepares a query vector outside the append lock and builds a complete per-attempt authority snapshot before ranking. The index exposes descriptor-only active pages; current coherence then checks each page's path-associated scope and transitive WSL/event refs. The resulting allowlist contains exact `(session, page ID, version, source digest,
compiler, scope epoch)` tuples and is joined before both the FTS5 limit and sqlite-vec KNN. The flat current-epoch set remains a coarse filter; the tuple allowlist prevents a colliding path epoch or invalidated dependency from starving the bounded candidate set.

Active descriptor metadata is validated with the same `pages.ValidateAuthorityDescriptor` contract used at page admission, including scope/authority/path/ref structure and kind/trust compatibility. Refs and paths use strict JSON decoding. Malformed covered metadata is typed operational index corruption before allowlisting—never suppression or a semantic miss; regression tests include malformed high-ranked chaff that would otherwise fill top-k.

Snapshot capture is complete or it fails operationally. A complete empty allowlist is a genuine semantic miss, while an unavailable snapshot, more than `indexer.MaxAuthoritySnapshotPages` (4,096) active pages, or more than 4 MiB of descriptor/eligibility payload returns an operational error—never truncation or an unfiltered fallback. Generation, source/page watermark, coherence revision, source sequence, and `IndexErr` checks surround snapshot construction and resolution. Within that exact universe, FTS5 and dense search run in parallel; canonical unit-float32 vectors feed sqlite-vec's rowid-IN KNN, and versioned RRF plus a named **provisional** policy applies deterministic affinity, trust, salience, verification, failure-overlap, diversity, distance, and minimum-score rules. Selected refs are materialized from exact L4 under cumulative budgets without injecting L1. Dense-disabled lexical-only mode may return a normal miss; a requested operational channel failure with no survivor remains an error.

The Phase 7F mechanism adds a bounded, disposable L2 working-set estimator on top of that exact materialization path. Its default policy admits at most 64 resident pages and 512 KiB of logical retained bytes, including at most 16 pinned pages and 128 KiB of pinned bytes; one page may retain at most 64 KiB. The policy also bounds bodyless exact-tuple ghosts to 64 entries/32 KiB, bodyless semantic-shadow episodes to 256 entries/64 KiB over a 64-real-use horizon, and categorical residency trace history to 512 entries. Pinned active task and hard-constraint anchors count inside the resident budget and cannot be chosen by similarity.

A first exact demand is cold with its reference bit set; a later actual demand/use or an exact ghost refault promotes it. Direct faults update residency atomically. A selected semantic page is demand-admitted only after exact L4 materialization and the final coherence/index freshness checks. Bounded non-selected, non-suppressed candidates may create metadata-only shadow observations; they cannot admit a body, set a reference bit, pin a page, or change L1. Embedding namespace records which estimator made an observation and is not part of authoritative page identity. Direct invalidation synchronously shoots down the matching resident identity. Because bodyless ghosts and shadows retain no dependency refs, broader residency-changing coherence transitions conservatively purge all ghosts and censor all pending shadow episodes.

Compiler-derived `wp_*` IDs are intentionally not served by the ID-only direct fault path: their authority requires the exact descriptor tuple and transitive refs. Repeated semantic selection revalidates and rematerializes L4 before an authority-only resident tuple replacement. A selected compiler body is thus working-set policy state, never independent truth or a weaker fault shortcut.

This mechanism is implemented in the current worktree subject to the final correctness/race/demo verification matrix. The repo still does not bundle or claim execution of real Qwen model weights, calibrated retrieval quality, measured sqlite-vec production resource limits, actual speculative L2 prefetch, or automatic L1 admission. Speculative L2 admission remains disabled until a real pinned-Qwen run and held-out usefulness/negative-transfer evaluation; automatic L1 admission remains disabled through Phase 10. The demo path requires neither an index nor embeddings.

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

Equivalently, run `go run ./cmd/wsms demo`. The command uses a fresh temporary data directory and proves the complete vector-free path: ledger/artifact backing storage, event-derived WSL mappings, a bounded resident capsule, a real close/reopen reconstruction, direct `F1` page-in, independently verified raw artifact bytes, the foreground client boundary, and same-database session isolation. It prints `DEMO PASS` only after every assertion and resource close succeeds.

To keep the SQLite ledger and content-addressed artifacts for inspection, give the demo a fresh persistent directory:

```bash
go run ./cmd/wsms demo --data-dir /tmp/wsms-demo-inspect
```

The command never removes an explicitly supplied directory or unrelated files inside it. For safety it refuses the reserved paths `ledger.db`, `ledger.db-journal`, `ledger.db-wal`, `ledger.db-shm`, or `artifacts` when any already exists, so use a fresh directory for each persistent run. This prevents old or unrelated storage from being mutated or mistaken for fresh evidence.

## Smoke path

```text
event → ledger.append → observers → WSL (lint) → scheduler → <working_state> capsule
                                                              ↘ page fault → exact evidence
```
