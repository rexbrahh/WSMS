# WSMS Implementation Plan

**Status:** Active plan  
**Date:** 2026-07-10  
**Target:** Verified local vertical-slice demo plus independently gated productization through Phase 7E

## 1. Definition of done for this plan slice

This immediate plan slice is done when the repository contains:

1. A detailed normative product specification.
2. A detailed architecture with explicit current/target boundaries.
3. A complete normative L3 warm-memory design that decides the vector-store
   role, local/scale-out backends, embedding profile, hybrid retrieval,
   rebuild/consistency, security, evaluation, and rollout gates.
4. Durable session replay and collision-free multi-session event identity.
5. Store-owned append ordering and ledger-handle session isolation.
6. Atomic event-level WSL application with a replayable record-to-event
   provenance index.
7. Validated, containment-safe artifacts whose digest is checked on read, plus
   visible corruption/ledger decode errors.
8. Correct WSL exact-evidence and canonicalization behavior for identified MVP
   invariants.
9. Explicit task, decision/avoidance, and next-action derivation sufficient for
   a representative capsule.
10. A one-command, no-API-key `wsms demo` that executes the full local mechanism,
   closes/reopens the session, retrieves structured and raw evidence, crosses the
   `harness.Client` boundary, and prints `DEMO PASS` only after assertions pass.
11. Focused tests plus repository-wide test, race, vet, build, and live demo
   verification.
12. README/demo instructions that match the verified command.
13. A disposable Phase 7E semantic-fault mechanism that can combine bounded
    lexical and dense candidate ranks without making either channel evidence.
14. A complete per-attempt exact page-tuple allowlist—derived from active page
    descriptors plus current path/transitive-ref coherence—runs before both
    channel limits and is rechecked against exact L4 before return.
15. Operational index/query failures remain distinct from a valid semantic
    miss, including freshness races around resolution.
16. Every selected semantic candidate is exact-materialized under cumulative
    budgets without changing L1 or claiming Phase 7F residency.
17. A bounded Phase 7F L2 mechanism adds cold/hot/pinned bodies, bodyless exact
    ghosts, deterministic real-use accounting, transactional pin quotas,
    invalidation shootdown, and bounded residency snapshots/traces.
18. Semantic readahead remains metadata-only: selected exact evidence may be
    demand-admitted after final freshness, while non-selected/non-suppressed
    exact tuples can only create bodyless usefulness episodes.

The original demo milestone did **not** require an L3 index. This extended plan
slice now includes the Phase 7A-7F in-repo mechanisms through metadata-only
shadow accounting, while still not requiring a production chat UI, real
provider credentials, a real Qwen weight/process run, comparative benchmark
results, calibrated retrieval quality, actual speculative prefetch, or
automatic L1 admission.

It also does not claim the entire product specification is implemented. In
particular, authorized model-facing file slices, calibrated automatic
residency, async maintenance, provider adapters, and comparative evaluation are
post-demo phases.

## 2. Phase 0 - Documentation and API discovery (complete)

### Findings

- The thesis and core loop are consistent across `README.md:3-7`,
  `README.md:45-50`, `docs/early_report.md:65-87`, and
  `docs/deep-research-report (6).md:192-225`.
- WSL v0's record family and invariants are specified in
  `docs/wsl/v0.md:14-52`.
- The current in-process path already works through
  `internal/harness/session_test.go:13`.
- `cmd/wsms/main.go` exposes only parse/lint/capsule helpers; there is no runtime
  demo.
- `OpenSession` creates empty derived state and does not replay the ledger.
- `events.id` is globally unique while allocation is per session, causing
  cross-session `E0001` collisions.
- Only constraint and tool-digest observers emit updates; decisions and
  staleness are stubs, and task/next records are not event-derived.
- WSL probes found canonical-spacing, escaping, exact-field erasure, event-ref,
  and polite-negation contradiction defects.
- Baseline `go test ./...`, `go test -race ./...`, `go vet ./...`, and CLI build
  passed before implementation.

### External research validation

- OpenAI and Anthropic both expose compaction for long-running contexts, but
  this does not provide WSMS's inspectable exact-evidence layer.
- “Lost in the Middle” supports the premise that nominal context capacity is
  not reliable utilization.
- MemGPT supports the virtual-context/page-fault analogy.
- Anthropic's long-running-agent harness report supports explicit progress
  artifacts and end-to-end verification.

These validate the problem framing, not WSMS's untested performance claim.

L3 design research also established a conservative implementation path:

- SQLite FTS5 provides mature lexical/BM25 search for paths, commands, symbols,
  identifiers, and error strings.
- The selected `modernc.org/sqlite` ecosystem distributes a pure-Go-transpiled
  `sqlite-vec` extension, but its pre-1.0 status requires a pinned compatibility
  spike before it can be a supported backend.
- Qdrant supports filtered hybrid/ANN retrieval and an official Go client, so
  it is the preferred scale-out adapter only after measurement justifies an
  additional process.
- Qwen3-Embedding-0.6B is the reference local evaluation profile; it remains
  replaceable and must beat FTS-only on WSMS fixtures before default enablement.

These are capability findings and design choices, not retrieval-quality claims.
Primary links and caveats are recorded in `docs/l3-warm-memory.md`.

## 3. Allowed APIs and source patterns

Implementers must copy and extend these existing contracts rather than invent
parallel abstractions.

| Need | Allowed API/pattern | Source |
|---|---|---|
| Open runtime | `harness.OpenSession(config.Config) (*Session, error)` | `internal/harness/session.go:31` |
| Append durable event | `(*Session).Append(context.Context, ledger.Event)` | `internal/harness/session.go:82` |
| List replay events | `(*AppendOnlyLedger).ListBySession(ctx, sessionID)` | `internal/ledger/store.go:145` |
| Apply event derivation | `(*Scheduler).AfterEvent(ctx, event)` | `internal/scheduler/scheduler.go:50` |
| Render capsule | `(*Session).BeforeTurn(ctx)` | `internal/harness/session.go:104` |
| Page fault | `(*Session).PageFault(ctx, id)` | `internal/harness/session.go:109` |
| Raw evidence fault | `(*faults.Tools).ReadRawLog(ctx, id, budget)` | `internal/faults/tools.go:28` |
| Foreground provider boundary | `(*harness.Loop).Turn(ctx, userText)` | `internal/harness/loop.go:12` |
| Observer contract | `Observer.Handle(ctx, event) ([]wsl.Update, error)` | `internal/observers/observer.go:11` |
| Apply typed record | `(*wsl.WorkingState).Apply/ApplyUpdate` | `internal/wsl/store.go:31` |
| CLI structure | subcommand switch with explicit exit status | `cmd/wsms/main.go:17` |
| Existing end-to-end test | event -> capsule -> page fault | `internal/harness/session_test.go:13` |
| Artifact offload | threshold, `Store.Put`, preview, raw ref | `internal/harness/session.go:84-93` |
| WSL fixture | T42/C7/F18/A4/next | `testdata/sample_session.wsl` and `docs/wsl/v0.md:54-81` |
| Explicit semantic fault | `(*Session).SemanticSearch(ctx, text)` | `internal/harness/session.go` |
| Hybrid retrieval | `(*retrieval.HybridRetriever).ResolveSemantic(ctx, intent)` | `internal/retrieval/hybrid.go` |
| Filtered lexical/dense search | `(*indexer.Index).SearchLexical/SearchDense` | `internal/indexer/search.go`, `dense.go` |

### APIs that do not exist and must not be assumed

- No general async observer queue runner exists; the Phase 7D dense writeback
  worker is a narrower supervised lifecycle owned by the harness.
- No production provider adapter exists; only `harness.Client` and
  `NoopClient` exist.
- No Qdrant adapter or production ANN service exists.
- No speculative L2 prefetch worker exists. Phase 7F provides bounded
  cold/hot/pinned residency, bodyless ghosts, and metadata-only semantic-shadow
  accounting, but candidate observation cannot admit a speculative body.
- No bundled real-Qwen serving process or model weights exist.
- No durable WSL snapshot loader exists.
- No forced-reset benchmark API exists.
- No WSL override syntax exists.

### Implemented L3 boundaries and remaining target APIs

Phases 7A-7E provide the page compiler, disposable index, embedding, retrieval,
and exact materialization boundaries described in `docs/l3-warm-memory.md`.
Phase 7F adds bounded L2 mechanism policy behind `internal/memory` plus explicit
scheduler/fault/harness integration, subject to the root final verification
matrix. Backend-specific vector clients remain isolated from `scheduler`,
`faults`, `wsl`, and residency policy. Actual speculative prefetch remains
disabled; Phase 7G may add Qdrant behind the existing index boundary only after
its measurement gate.

## 4. Phase 1 - Durable identity and replay

**Implementation status:** Core replay/composite-key work is complete and its
focused tests pass. Independent verification found additional session-isolation,
append-order, and durable-decode requirements; those are Phase 2B gates below.

### Objective

Make ledger truth sufficient to reconstruct session state after process restart
and allow multiple sessions to share one database safely.

### Implementation tasks

1. Change the fresh-schema event primary key to `(session_id, id)`.
2. Scope `AppendOnlyLedger.Get` by the ledger's `sessionID`.
3. Keep per-session monotonic allocation in `loadSeq`; do not replace readable
   IDs with random UUIDs for this slice.
4. Add a private replay method in `harness` that lists ordered events and calls
   `Scheduler.AfterEvent` without appending.
5. Invoke replay after the session composition root is fully wired and before
   returning from `OpenSession`.
6. Close the ledger and return the replay error if reconstruction fails.

Documentation references:

- `docs/specification.md` FR-001 and FR-003.
- `docs/architecture.md` A2, A3, and restart flow.
- Existing composition pattern: `internal/harness/session.go:31-78`.
- Existing ordered list: `internal/ledger/store.go:145-164`.

### Focused tests

- Two ledger handles with different session IDs append their own `E0001` into
  the same DB and retrieve only their own events.
- A session containing a hard constraint and failure is closed/reopened; WSL,
  page fault, and raw-log fault remain available.
- A new event after replay receives the next event/record IDs.
- Replay does not increase ledger event count.

### Anti-pattern guards

- Do not append events again during replay.
- Do not load a WSL snapshot lacking an event watermark/version.
- Do not make event IDs globally random merely to avoid fixing SQL scope.
- Do not hide replay errors by returning an empty session.
- Do not claim compatibility with pre-0.1 database files; no release schema
  contract exists yet.

### Verification checklist

- [x] Focused ledger identity tests pass.
- [x] Focused harness reopen tests pass.
- [x] Existing ledger/harness tests still pass.
- [x] `go test -race ./internal/ledger ./internal/harness` passes.
- [x] Phase 2B closes receiver isolation, append ordering, and decode failures.

## 5. Phase 2 - WSL correctness hardening

**Implementation status:** Complete. Focused, race, repository-wide, CLI, and
independent verification checks pass. A pre-existing `@page branch` grammar
drift is reconciled in Phase 2B.

### Objective

Protect the exact-evidence and canonical-format invariants on which the demo
depends.

### Implementation tasks

1. Emit exactly one blank line between serialized records.
2. Parse quoted values with correct unescaping compatible with serializer
   quoting; reject malformed escapes rather than silently multiplying them.
3. Reject changes **or erasure** of immutable hard-constraint and failure exact
   fields.
4. Require event references to have been registered with `NoteEvent`; remove
   syntactic `E*` acceptance.
5. Normalize contradiction checks so polite prefixes do not bypass hard
   negation detection.
6. Add focused regression tests and refresh the capsule golden fixture only if
   the actual renderer output is the intended contract.

Documentation references:

- `docs/wsl/v0.md:29-52`.
- `docs/specification.md` FR-005 and FR-006.
- Existing serializer: `internal/wsl/serializer.go`.
- Existing parser body handling: `internal/wsl/parser.go:118`.
- Existing lint application checks: `internal/wsl/lint.go:81`.

### Focused tests

- Two-record serialization has one blank separator and is idempotent.
- Windows-style backslashes survive repeated parse/serialize cycles.
- Empty replacement values cannot erase exact evidence.
- `E999` fails unless the state noted that exact event.
- “please do not rewrite transport layer” contradicts “rewrite transport
  layer.”
- Existing sample session still parses, lints, and round-trips semantically.

### Anti-pattern guards

- Do not special-case only the sample fixture.
- Do not broaden reference acceptance by prefix.
- Do not implement a WSL override syntax in this phase.
- Do not change field ordering or record grammar without updating the v0 spec.
- Do not rely on Go `%q` unless the parser performs the corresponding unquote.

### Verification checklist

- [x] New WSL regression tests failed before and pass after the fix.
- [x] `go test ./internal/wsl ./internal/renderer` passes.
- [x] `wsms parse` output is canonically stable on the sample fixture.
- [x] `wsms lint` rejects the known invalid probes.

## 5B. Phase 2B - Pre-demo integrity and provenance

### Objective

Close the independent review findings that would otherwise make restart,
session isolation, provenance, and exact raw evidence weaker than the demo's
claims.

### Implementation tasks

#### Ledger identity, ordering, and decoding

1. Add a store-assigned `append_seq` column with
   `UNIQUE(session_id, append_seq)` and order replay/listing by it.
2. Recover the next sequence from `MAX(append_seq)`, not row count.
3. Reject an `Event.SessionID` that differs from the open ledger and reject a
   `ListBySession` argument outside that receiver's session.
4. Preserve caller timestamps only as metadata; add out-of-order/equal timestamp
   tests proving append order.
5. Reject caller-supplied live event IDs; a future import path must be separate
   from append.
6. Allocate IDs/sequences with database-level serialization so two handles for
   the same session cannot both reserve `E0001`.
7. Return typed ledger errors for malformed persisted timestamp, payload JSON,
   or scope JSON instead of silently substituting zero/empty values.

#### Atomic derivation and provenance

8. Add an atomic `WorkingState.ApplyUpdates`/batch path that validates all
   records against a cloned candidate and commits them together.
9. Store a derived-record-to-event provenance mapping in `WorkingState`; clone,
   replay, and lookup must preserve/reconstruct it.
10. Change `Scheduler.AfterEvent` to apply the full observer batch atomically and
   materialize L2 pages only after the batch commits.
11. Reject replacement of an existing record ID with a different WSL kind.
12. Reconcile `PageRecord.Branch` with `docs/wsl/v0.md` and `FieldOrder` rather
   than leaving an undocumented canonical field.

#### Artifact and fault integrity

13. Require exact hexadecimal SHA-256 refs/hashes and containment-safe derived
    paths.
14. Recompute SHA-256 on artifact read and report corruption.
15. In raw-log resolution, return `PAGE_MISS` only for an absent logical target;
    propagate malformed, missing referenced, corrupt, ledger, and I/O errors.

Documentation references:

- `docs/specification.md` FR-001 through FR-004 and NFR-004/NFR-007.
- `docs/architecture.md` A2, A3.1, A4.1, durable data, and error model.
- Current ledger allocation/scan: `internal/ledger/store.go`.
- Current update loop: `internal/scheduler/scheduler.go:51-70`.
- Current state clone/apply: `internal/wsl/store.go`.
- Current artifact parsing/path/read: `internal/artifacts/store.go:74-111`.
- Current raw resolution: `internal/faults/resolver.go:76-102`.

### Focused tests

- A session handle cannot append, list, or get another session's events.
- Out-of-order timestamps replay in append order; equal timestamps remain stable.
- Caller-supplied live event IDs are rejected.
- Two concurrently open handles for the same session allocate distinct ordered
  IDs without a uniqueness failure.
- Corrupt timestamp/payload/scope rows fail open/replay visibly.
- A two-record observer batch with an invalid second record leaves no first
  record, provenance entry, or L2 page.
- Live and replayed records report the same source event IDs.
- Malformed/traversal artifact refs fail without reading outside the store.
- Corrupted or missing referenced artifacts return errors; absent logical IDs
  return exactly `PAGE_MISS`.

### Anti-pattern guards

- Do not use timestamps or lexicographic IDs as append order.
- Do not let a ledger receiver act as a database-wide session capability.
- Do not rely on a per-Go-object mutex for cross-handle sequence allocation.
- Do not add provenance fields to WSL v0 solely for the demo; use the derivation
  index unless the grammar is deliberately versioned.
- Do not commit page/cache side effects before a record batch validates.
- Do not label byte equality as hash verification without recomputing the digest.
- Do not collapse corruption or I/O failure into `PAGE_MISS`.
- Do not broaden `file_slice` exposure; authorized workspace policy is a later
  harness phase.

### Verification checklist

- [x] Focused ledger ordering/isolation/decode tests pass under `-race`.
- [x] Focused WSL atomic/provenance and scheduler tests pass under `-race`.
- [x] Focused artifact/fault tests pass under `-race`.
- [x] Existing Phase 1/2 tests remain green.
- [x] Independent verification and anti-pattern review report no P0/P1 finding.

## 5C. Phase 2C - Unix VM integrity review fixes

**Implementation status:** Complete. Full repository tests, full race suite, and
`go vet ./...` pass. Review-closure work added fail-stop session semantics,
capability-gated file slices, private one-tier page containers, idempotent
resource ownership, and inline-evidence page-in.

### Objective

Close the remaining review findings that affect working-set ordering, virtual
identity, page-fault reachability, safe backing-store writes, or the Unix
mechanism/policy split. Do not expand into unrelated event-sourcing features.

### Implementation tasks

1. Serialize append plus derivation inside one `Session` so concurrent calls
   cannot assign WSL IDs in a different order from durable `append_seq`.
2. Snapshot/restore the observer ID allocator around event derivation; rejected
   mapping batches must not consume virtual record IDs.
3. Remove the raw ledger `DB()` capability and keep corruption injection inside
   tests using an independently opened SQLite connection.
4. Construct an escaped `file:` SQLite URI so valid filesystem `?` characters
   cannot be interpreted as DSN query delimiters.
5. Add `ApplyDerivedUpdates`, which requires noted non-empty provenance; retain
   a separate trusted static WSL import path.
6. Clear stale provenance when trusted provenance-free replacement occurs;
   reject typed-nil records and unsupported update operations.
7. Resolve `ReadRawLog(F...)` through the failure's provenance event when the
   exact output is inline rather than artifact-backed.
8. Make artifact writes concurrent-safe with unique same-directory temporary
   files and prevent symlink escape from the artifact root.
9. Deep-copy page slice metadata at hierarchy boundaries and avoid mutating
   caller-owned pages.
10. Move residency scoring/weights from `internal/memory` into
    `internal/scheduler`; memory owns page/tier mechanism, scheduler owns policy.

### Focused tests

- Concurrent appends on one Session derive records in durable append order and
  are race-free.
- A rejected observer batch does not advance the next derived record ID.
- No production API exposes raw cross-session SQL access.
- A data directory containing `?` opens and reopens the intended database.
- Derived updates without evidence fail; static replacement cannot retain stale
  evidence; typed nil and unknown ops fail without panic.
- A sub-threshold failure raw-log fault returns its exact inline output.
- Concurrent identical artifact puts are idempotent; symlink escape attempts
  fail without touching an outside sentinel.
- Hierarchy copies isolate `Refs`; score tests live under scheduler.

### Anti-pattern guards

- Do not add a generic transaction/event framework.
- Do not make WSL IDs random; preserve deterministic Unix-like addressability.
- Do not weaken `PAGE_MISS` into a catch-all error.
- Do not expose unrestricted file/SQL capabilities to make tests convenient.
- Do not implement an elaborate eviction policy before the fixed-policy demo.

## 6. Phase 3 - Explicit operational-state events and observers

### Objective

Create the active task, decision/avoidance, and next action from durable events
instead of seeding in-memory WSL directly.

### Implementation tasks

1. Add stable `task_started` and `next_action` event types.
2. Add event-envelope validation for required payload fields. Validate known
   events before append and again during replay with the event ID in errors.
3. Add `Session.StartTask` and `Session.SetNext` helpers that append those event
   envelopes.
4. Add a deterministic task/next observer using existing WSL `TaskRecord` and
   `NextRecord` types.
5. Implement the existing `Decisions` observer for explicit `decision` payloads.
   It may emit a `DecisionRecord` plus `AvoidRecord` only when both are grounded
   in the event and the avoid ref already exists.
6. Add helpers for recording a decision/avoidance without exposing WSL store
   mutation to the CLI.

Documentation references:

- Event payloads in `docs/specification.md` section 10.2.
- WSL records in `docs/wsl/v0.md:14-27`.
- Observer ordering in `internal/observers/dispatcher.go:14-25`.
- Clone/record structures in `internal/wsl/records.go`.

### Focused tests

- Task-start event yields the exact task record and capsule block.
- Next-action replacement uses stable `next` identity.
- Decision plus valid failure ref yields decision and avoid records.
- Invalid avoid ref fails visibly and leaves state unchanged for that update.
- Replay produces the same record IDs and state as live application.
- Missing required fields fail before durable append; a malformed persisted
  known event fails replay with event context.

### Anti-pattern guards

- Do not infer a full task object from arbitrary free-form user prose.
- Do not let model-generated text bypass explicit event provenance.
- Do not write WSL records directly from the CLI/demo.
- Do not implement staleness behavior under the existing stub without the
  branch/file scope design and tests.

### Verification checklist

- [x] Observer unit tests pass.
- [x] Harness live/replay tests include task, avoid, and next state.
- [x] Existing constraint/tool-digest behavior remains unchanged.

## 7. Phase 4 - One-command vertical-slice demo

### Objective

Expose the complete local runtime mechanism in a deterministic command that a
user can run and audit.

### Implementation tasks

1. Add `internal/demo` with a reusable `Run(ctx, writer, options) error` entry
   point. Business assertions live here, not in CLI argument parsing.
2. Default to a fresh temporary data directory. Allow an explicit data directory
   only without deleting or overwriting unrelated content.
3. Use a low artifact threshold and include a sentinel beyond the bounded ledger
   preview.
4. Drive task, constraint, failure, decision/avoid, and next events through
   `harness.Session` APIs.
5. Render and validate the pre-restart capsule as the bounded resident working
   set; report the ledger/artifact layer as backing store.
6. Close and reopen the session, then validate the reconstructed capsule,
   `ReadPage`, and `ReadRawLog` results.
7. Exercise `harness.Loop` with a deterministic local client that verifies the
   system capsule before returning a continuation response.
8. Print concise stage evidence and `DEMO PASS` only after every assertion.
9. Add the `wsms demo` subcommand, `make demo`, README instructions, and an
   end-to-end test.

Documentation references:

- Demo contract in `docs/specification.md` FR-012 and section 11.
- Runtime sequence in `docs/architecture.md` sections 7-9.
- Existing `Session` smoke flow in `internal/harness/session_test.go:13-69`.
- Existing CLI switch in `cmd/wsms/main.go:17-82`.
- Existing client boundary in `internal/harness/loop.go` and `client.go`.

### Expected human-readable evidence

```text
BACKING STORE: EVENT ... task_started -> T1
BACKING STORE: EVENT ... user_instruction -> C1 hard
BACKING STORE: EVENT ... command_output exit=1 -> F1
BACKING STORE: ARTIFACT sha256:...
RESIDENT WORKING SET: CAPSULE ...
=== SESSION RUNTIME CLOSED / MEMORY DROPPED / REOPENED ===
PAGE TABLE: DERIVED MAPPINGS RECONSTRUCTED
PAGE FAULT F1: PAGE-IN HIT
BACKING STORE F1: SHA256 VERIFIED
CLIENT: CAPSULE RECEIVED
DEMO PASS
```

Exact wording may differ; semantic markers and exit behavior are tested.

### Anti-pattern guards

- Do not fake restart by keeping the original `WorkingState` alive.
- Do not seed WSL directly.
- Do not call a hosted provider or require a secret.
- Do not delete an operator-supplied data directory.
- Do not print `PASS` before page/raw/client assertions finish.
- Do not claim comparative quality or async scheduling.

### Verification checklist

- [x] Demo end-to-end test passes with a temporary directory.
- [x] Live `go run ./cmd/wsms demo` exits 0 and prints all evidence markers.
- [x] A forced bad invariant causes nonzero exit and no success marker.
- [x] README and Makefile commands reproduce the tested path.

## 8. Phase 5 - Independent verification and quality review

### Verification agent checklist

1. Re-read the demo acceptance contract in `docs/specification.md` section 11.
2. Run focused tests for every modified package.
3. Run:

   ```bash
   go test ./...
   go test -race ./...
   go vet ./...
   go build -o /private/tmp/wsms ./cmd/wsms
   /private/tmp/wsms demo
   git diff --check
   ```

4. Inspect the actual demo output for exact hard constraint, failure signature,
   restart boundary, page hit, raw-log verification, client boundary, and final
   success marker.
5. Confirm no runtime data, DB files, binaries, caches, or PDF renders appeared
   as untracked repository artifacts.

### Anti-pattern review

- Search for direct WSL mutation in `cmd/wsms` or `internal/demo`.
- Search for success-shaped ignored errors.
- Confirm SQL event reads include session scope.
- Confirm replay never calls ledger append.
- Confirm no observer or scheduler is described as async in runtime output.
- Confirm no network/provider dependency was introduced.
- Confirm docs do not say the comparative research hypothesis was proven.

### Code quality review

- Review error cleanup and ownership around open/replay/close.
- Review exact-evidence immutability and quoting.
- Review deterministic ID behavior live versus replay.
- Review test isolation and race-safety.
- Review CLI exit codes and stable operator output.

## 9. Post-demo roadmap

### Phase 6 - Coherence and invalidation

**Status:** implemented and verified as the prerequisite for Phase 7.

Implementation:

1. `internal/coherence` owns a replay-derived per-session scope snapshot and
   cloned sidecar bindings for records, events, pages, and canonical paths.
2. Durable post-scope events define `branch_change`, `commit_change`,
   `file_renamed`, `memory_invalidated`, and `memory_revalidated`. The strict
   `file_snapshot` path/digest event is additive; legacy `file_read` rows retain
   their pre-Phase-6 compatibility contract.
3. Repo/branch/commit/path epochs are keyed by scope and allocated by one
   monotonic session clock. Independent repo/task/branch/commit requirements can
   be intersected without widening an address; branch/commit/rename transitions
   mark only affected bindings stale.
4. Revalidation is stale-only and CAS-like: it requires a positive
   `expected_stale_revision`, a preexisting eligible `evidence_ref`, and a
   matching source digest for page/path targets. It never retargets an old path.
5. Scheduler candidate/commit ordering makes WSL event noting, observer updates,
   allocator checkpoints, coherence transitions, and hierarchy reconciliation
   one foreground transaction boundary.
6. Capsule selection and page faults share the same recursive, cycle-safe
   eligibility oracle. Decision/page/avoid refs inherit dependency status, and
   pages without current refs fail closed.
7. Path invalidation is terminal across known descendants and rejects future
   overlapping snapshots/renames. L4 raw evidence stays diagnostic except for
   policy/security revocation, including aliases beneath a revoked path.
8. An old resident generation is rematerialized from current WSL/L4 evidence;
   its body is never made current by metadata refresh alone.
9. Harness helpers expose validated branch, commit, rename, snapshot,
   invalidation, and revalidation operations without direct WSL mutation.

Verification gates:

- Live and close/reopen replay produce identical WSL, keyed epochs, bindings,
  stale revisions, capsule output, hierarchy status, and fault behavior.
- Branch round trips and commit changes cannot reactivate an older epoch.
- Rename matching uses path-component boundaries and never silently retargets
  old refs.
- Terminal path ancestors suppress known/future descendants, raw aliases, and
  overlapping rename/snapshot attempts.
- Task/branch/repo scope intersections and transitive decision/page/avoid refs
  cannot widen eligibility.
- A rejected WSL batch leaves coherence, hierarchy, provenance, known events,
  and allocator counters unchanged.
- Malformed/noncanonical/cross-scope inputs fail before append and leave the
  session usable.
- After invalidation commits, serialized concurrent page faults cannot return a
  stale L2/WSL hit; cross-session IDs remain isolated under the race detector.

### Phase 7 - Real residency scheduling

This phase is expanded into independently gated slices. Each slice must leave
the direct-ID demo and no-L3 runtime operational.

#### Phase 7A - Frozen semantic corpus and page compiler

**Status:** implemented, runtime-wired, and independently verified. The page
compiler remains a pure L4/WSL derivative; Phase 7B owns its best-effort index
wiring.

**Objective:** establish the unit of retrieval and a correctness oracle before
selecting a production vector implementation.

Implementation:

1. `internal/pages` defines `WarmPage`, `PageRef`, `PageMutation`, source
   digests, compiler version, trust/status, scope epoch, and controlled kinds.
2. `DeterministicCompiler` emits evidence-backed `failure_episode`, `decision`,
   `constraint`, `task_checkpoint`, `known_good_command`, `repo_fact`, and
   `file_context` mutations from one durable event + post-event WSL view.
3. Canonical search text is token/byte-bounded; raw artifact bodies stay in L4
   and are verified via streaming `artifacts.Store.VerifyArtifact`.
4. The strict replay stream and hand labels live at
   `internal/pages/testdata/frozen_corpus.json`; legacy compiler goldens remain
   under `testdata/pages/corpus/transport_fix/`. The strict corpus covers all
   seven page kinds plus isolated wrong-repo/task/branch/commit, trust,
   invalidation, poison, true-no-answer, and negative-transfer judgments.
5. `ExactCosineSearch` / `ExactCosineSearchContext` is the tiny-fixture exact
   oracle (cancel-aware, dimension-bounded). Evaluation-only, not production ANN.
6. `ValidateMaterializable` is the post-candidate gate: session, current
   coherence generation, transitive ref eligibility, repo/task/branch/commit,
   compiler version, and projection-bound source digest must still match before
   L2 admission.
7. Ledger validation rejects malformed UTF-8 before persistence, preventing
   live observer/compiler bytes from diverging from reopen replay. The compiler
   uses mixed trust for task checkpoints and cannot promote tool prose into
   user/system authority.

Verification/gates:

- [x] Same ledger change recompiles to byte-identical mutations, including
      after JSON persistence/reopen and adversarial UTF-8 fuzzing.
- [x] Independent sessions match structural identity (IDs/text/refs); digests
      differ only by durable event timestamps.
- [x] Every compiled active page passes `ValidateMaterializable`; stale,
      cross-session, wrong-epoch, and digest mismatch pages fail closed.
- [x] Assistant/untrusted prose cannot mint constraint or other hard pages.
- [x] Corpus labels cover every page kind and each hard authority axis in
      isolation; unlabeled or multi-axis drift fails corpus validation.
- [x] Projection corruption, transitive durable invalidation, task confusion,
      and trust mismatch fail closed at materialization.
- [x] Direct-ID `wsms demo` remains vector-free and does not depend on pages.

#### Phase 7B - Separate FTS5 warm index

**Status:** implemented and verified. Dense/hybrid channels remain deferred.

**Objective:** deliver useful semantic-by-text faults without an embedding
dependency and establish index lifecycle semantics.

Implementation:

1. `internal/indexer` owns disposable `<data-dir>/index/warm.db` (FTS5); never
   writes search tables into `ledger.db`.
2. Versioned generations, metadata/FTS projections, contiguous per-session
   watermarks, idempotent Apply, rebuild lock, and validated atomic cutover via
   `warm.rebuild.db`.
3. `internal/retrieval` implements typed `QueryIntent`, hard filters, FTS5 BM25
   candidates, stable `page_id` tie-break, explanations, and
   `SEMANTIC_PAGE_MISS`. `SearchDense` returns `ErrDenseUnavailable` until 7C.
4. Harness best-effort compile+Apply after each durable event (live and replay);
   gaps trigger ordered reconstruction from L4 and index errors never fail the
   durable append. A watermark cannot jump over a missing source sequence.
5. `Session.SemanticSearch` rechecks the current coherence/authority/digest,
   then faults bounded WSL/event refs through the exact resolver. Raw artifact
   bodies are never auto-faulted by semantic search.
6. Rebuild validates active-page/FTS counts, checkpoints WAL state, and keeps a
   restorable prior generation through cutover. A process-local physical-dir
   lease serializes Open/recovery/rebuild against all handles, including
   symlink aliases, and stale handles rebind before their next operation.

Verification/gates:

- [x] Deleting `index/` leaves replay, capsules, and direct faults unchanged.
- [x] Incomplete rebuild artifacts are not served; open cleans orphans.
- [x] Concurrent schema replacement, Apply/invalidation during rebuild, stale
      handles, and Close/read races pass under the race detector.
- [x] Failed indexing cannot forge progress: gap repair resumes at the first
      unapplied L4 event across reopen.
- [x] Invalidated pages and recheck failures suppress hits (`SEMANTIC_PAGE_MISS`).
- [x] Cross-session queries cannot materialize another session's pages.
- [x] Frozen corpus positive labels hit expected kinds; true-no-answer abstains.
- [x] `wsms demo` still PASSes (vector-free; index optional).

Deployment boundary: the operation lease is process-local because the MVP is
one local process. Before adding multi-process workers or a daemon with external
writers, put Apply/vector writes and rebuild behind one filesystem-wide
advisory operation lock; `rebuild.lock` alone serializes only rebuild owners.

#### Phase 7C - sqlite-vec compatibility and exact-parity spike

**Status:** implemented and verified on the development platform (darwin,
pure-Go `modernc.org/sqlite` + `modernc.org/sqlite/vec`). Dense remains
**opt-in** (`DenseDimensions > 0`); default open keeps `SearchDense`
unavailable. Phase 7D supplies the optional embedding boundary and Phase 7E
consumes dense ranks.

**Objective:** prove the preferred embedded dense backend against the pinned Go
and platform matrix before product code depends on it.

Implementation:

1. Blank-import `modernc.org/sqlite/vec` only from `internal/indexer/vecregister.go`
   (process-wide registration; ledger never creates `vec0` tables).
2. Optional dense projection on disposable `warm.db`: `warm_pages_vec` (cosine
   `vec0` with `session_id` and `embedding_namespace` partition keys) and a
   version-bound `warm_page_vec_map` rowid map.
3. `UpsertVectors` / `DeleteVector` / invalidate hook; `SearchDense` builds a
   namespace-constrained eligible rowid set with status/repo/task/branch/commit,
   kind/trust, current scope epoch, path, and page/ref-exclusion filters before
   sqlite-vec KNN. Each vector is bound to page version, source digest,
   compiler version, session, and embedding namespace.
4. Exact-oracle parity tests against `pages.ExactCosineSearchContext` on
   well-separated unit vectors (top-k ID order + distance ≈ 1 − similarity).
5. Config `DenseDimensions` (default 0); harness passes option through.
6. Restart restores dense dims from `index_meta`; legacy session-only vec0
   layouts are discarded/rebuilt, and rebuild copies only tuple-compatible
   vectors into the new generation.
7. Stored/query vectors are canonical finite unit directions with exactly
   float32-representable components. The shadow, write path, and sqlite-vec
   MATCH path share this ABI; oracle fixtures compare equivalent directions.

Verification/gates:

- [x] Extension initializes without CGO on the verified platform.
- [x] Default open: dense off, FTS and demo unchanged.
- [x] Dense KNN, filters, invalidate, batch replace, cancel, concurrent, restart.
- [x] More than the maximum over-fetch of closer wrong-namespace vectors cannot
      starve a valid namespace hit.
- [x] Page updates drop stale vectors; rebuild preserves only compatible vectors.
- [x] Oracle parity on synthetic fixtures.
- [x] Pre-limit scope/path/exclusion filtering prevents ineligible chaff from
      consuming bounded KNN slots.
- [x] Extreme finite inputs canonicalize without overflow/underflow and use the
      same float32-safe direction across backends.
- [x] Pre-1.0 churn isolated to indexer; no ledger/WSL format changes.

Known limitation: vec0 selects at most `k` rows before Go applies PageID tie
ordering. Equal-distance order is stable inside the returned set, but when more
than `k` eligible rows tie at the boundary, boundary membership is
backend-defined. sqlite-vec v0.1.9 also uses a brute-force KNN path; target-scale
CPU, memory, latency, and concurrency measurements remain open.

Stop conditions (not hit on verified platform):

- If the extension cannot initialize without weakening pure-Go portability,
  keep FTS plus the exact backend and do not leak extension handling upward.
- If filtering or restart behavior is unreliable, do not enable dense search;
  evaluate Qdrant only after documenting the measured blocker.

#### Phase 7D - Namespaced local embedder

**Status:** in-repo runtime boundary implemented and verified with deterministic
backends and an adversarial test sidecar. The embedder remains optional,
local-first, and non-authoritative. Phase 7E can consume its dense query ranks,
but a real Qwen process, downloaded weights, and an exact-revision
latency/resource run have not been executed in this repo and remain an
operational gate rather than an implied code result.

**Objective:** add reproducible private query/document embeddings without
placing inference in the truth path.

Implementation:

1. [x] Define `EmbeddingNamespace` over exact model revision, dimensions,
   metric, normalization, tokenizer, query instruction, document template, page
   schema, and redaction version.
2. [x] Add `Embedder` with distinct `EmbedDocuments` and `EmbedQuery` methods.
3. [x] Implement the namespaced Qwen3-Embedding-0.6B profile and WSMS-owned
   client protocol behind a supervised adapter with bounded Unix-socket or
   loopback transport. The official query `Instruct:`/`Query:` serialization is
   distinct from the unprefixed document path.
4. [x] Add startup self-check, batch limits, deadlines, cancellation, circuit
   breaker, content-addressed document embedding cache, and health reporting.
5. [x] Exclude secrets, denied paths, unrestricted artifacts, and raw
   transcripts; retain inspectable canonical search text locally.
6. [x] Keep hosted providers disabled unless explicitly configured with
   redaction, payload inspection, cost/error telemetry, and a distinct
   namespace.
7. [ ] Run a real local serving stack at an explicitly pinned model/tokenizer
   revision through a small WSMS protocol bridge; record cold start, memory,
   throughput, cancellation, and normalized-vector parity. This requires model
   weights/service setup and is not claimed by the deterministic test adapter.

Verification/gates:

- [x] Query/document inversion, dimension mismatch, namespace mismatch, and
      malformed vectors fail visibly.
- [x] Embedder timeout degrades to FTS-only while ledger writes and direct
      faults continue.
- [x] Re-embedding identical canonical content reuses the namespaced cache.
- [x] A namespace change builds a new generation; mixed-vector search is
      impossible.
- [x] Failed embedding backfills do not advance truth: page watermarks continue
      and missing-vector pages are retried in-session and after reopen.
- [x] Embedding inference runs out of the append/direct-fault path; a blocked or
      failed embedder cannot delay ledger writes or exact page faults.
- [x] HTTP transport ignores ambient proxies, rejects every redirect, and
      revalidates a literal loopback target at dial time; Unix-socket transport
      uses the same no-redirect policy.
- [x] Rendered query/document payloads pass final admission before backend use;
      denied pages become tuple-scoped lexical-only entries without starving
      later vector backlog.
- [x] Vector and suppression writes compare-and-swap the exact page
      version/source digest/compiler tuple; a configured generation rejects
      foreign embedding namespaces.
- [x] Transient service faults retry with bounded backoff; terminal ABI,
      namespace, and vector faults park until a new wake/reopen instead of
      polling indefinitely.
- [ ] Real Qwen sidecar/model execution passes the exact-revision operational
      gate above. This does not block the vector-free mechanism demo or FTS.

#### Phase 7E - Hybrid semantic faults

**Status:** mechanism implemented and verified with deterministic vectors,
synthetic/local test embedders, adversarial index fixtures, and exact L4
materialization. The checked-in policy is provisional; real-Qwen execution,
held-out quality calibration, production resource measurement, and automatic
residency/admission remain open gates.

**Objective:** combine exact lexical and conceptual retrieval while keeping
approximation outside the evidence boundary.

Implementation:

1. [x] Prepare the optional query embedding outside the append lock, then run
   bounded FTS and dense candidate generation concurrently over one
   `SearchQuery` eligibility universe.
2. [x] Capture every active page's descriptor for the session in one index read
   snapshot without search text/summary. The descriptor contains its exact
   tuple, logical WSL/event ref IDs, scope, branch, commit, and path scope. Use
   strict JSON for refs/paths and the shared `pages.ValidateAuthorityDescriptor`
   contract for scope, repo/task authority, branch/commit, paths, refs, and
   kind/trust compatibility; malformed covered metadata is typed corruption.
3. [x] Evaluate every descriptor with current path-associated coherence and
   transitive-ref eligibility. Seal the survivors as a complete allowlist of
   `(session, page ID, version, source digest, compiler, scope epoch)` tuples.
4. [x] Join that allowlist before FTS `LIMIT` and inside sqlite-vec's rowid-IN
   eligibility subquery before KNN. Session/status/trust/repo/task/branch/
   commit/kind/coarse-epoch/path/exclusion filters remain additional gates.
5. [x] Treat the coarse flat epoch set only as defense in depth; exact tuple
   authority prevents cross-path epoch aliases and transitively invalidated
   active rows from starving bounded search.
6. [x] Require the exact page tuple, serving generation, and source/page
   watermark metadata from each channel; suppress cross-channel tuple
   disagreement and degrade/fail on projection snapshot disagreement.
7. [x] Bound authority snapshots at `indexer.MaxAuthoritySnapshotPages` (4,096)
   active pages and 4 MiB of descriptor/encoded-eligibility payload. A complete
   empty allowlist is valid; unavailable or over-bound state is operational and
   never truncates or falls back to unfiltered search.
8. [x] Fuse ranks with `rrf/v1` (`k = 60`) and stable PageID order within each
   returned channel set. Do not compare raw BM25 and cosine values.
9. [x] Apply the named `working-set/v1-provisional` policy: repo/task/branch/
   commit/path affinity, trust, salience, verification, last-failure overlap,
   per-kind/per-source caps, token-Jaccard near-duplicate suppression, maximum
   dense distance, and minimum final score.
10. [x] Recheck current coherence and exact refs serially, continue after
   suppressible candidates, and enforce cumulative attempt/page/ref/byte/token
   budgets while selecting no more than three pages (and only bounded refs from
   each page).
11. [x] Return a bounded, text-free trace containing filters, channel positions,
   RRF/policy contributions, categorical degradation/suppression/abstention,
   selected page IDs, and token use. Candidate metadata retains generation and
   watermark for freshness validation; latency telemetry remains future work.
12. [x] Keep `Session.SemanticSearch` explicit and reference-first. Known
    WSL/event IDs continue through direct faults; compiler `wp_*` IDs remain
    behind descriptor/refs validation rather than an ID-only cache shortcut.
    Semantic results do not mutate L1.

Verification/gates:

- [x] Fake and real-index tests prove the complete exact tuple allowlist reaches
      both channels before FTS top-k and dense KNN.
- [x] Descriptor snapshots omit derivative prose, expose only logical WSL/event
      refs for coherence, and reject cancellation, malformed tuples, concurrent
      tuple updates, and over-4,096 page tables without returning a partial
      allowlist. Both descriptor capture and eligibility encoding also enforce
      the documented 4-MiB bound in their implementation paths.
- [x] Strict-JSON and shared descriptor-validation fixtures cover malformed
      scope/authority/path/ref metadata, unknown/trailing JSON, and invalid
      kind/trust pairings. High-ranked malformed chaff that fills the legacy
      top-k produces typed operational corruption and no partial authority or
      exact evidence, never `SEMANTIC_PAGE_MISS`.
- [x] A complete-empty allowlist produces a genuine semantic miss; incomplete,
      unavailable, or over-bound authority produces `ErrIndexUnavailable`.
- [x] Security-revoked transitive refs and stale pages with a colliding current
      path epoch are removed before either channel can spend its candidate
      budget; the still-current page remains discoverable.
- [x] Tuple/snapshot races, stale/ineligible chaff, exclusions, cancellation,
      and cross-session/scope cases fail closed.
- [x] RRF, provisional policy contributions, deterministic diversity, dense
      distance, minimum-score abstention, and within-result PageID ties have
      exhaustive deterministic tests.
- [x] Search results expose no derivative prose/refs and are returned only
      after current L4 validation and exact cumulative-budget materialization.
- [x] Dense/query failure with a lexical survivor returns a categorical
      degraded hit; operational channel failure with no survivor returns
      `ErrIndexUnavailable`, not `SEMANTIC_PAGE_MISS`.
- [x] `IndexErr`, coherence revision, source sequence, health/generation, and
      source/page watermark are checked around snapshot construction and again
      after resolution, including otherwise valid hits. Coherence change
      receives one bounded retry and then a typed scope abstention.
- [x] Query inference does not hold the append lock; cancellation and concurrent
      close are bounded. The vector-free demo and direct-ID path remain
      unchanged.
- [ ] Run the real pinned Qwen model/tokenizer through the WSMS bridge and
      measure normalized-vector parity, cold start, CPU/GPU memory, throughput,
      cancellation, and query latency.
- [ ] Compare no-L3, FTS-only, dense-only, and hybrid variants on a held-out
      corpus; calibrate thresholds/weights on training data only.
- [ ] Hybrid must improve exact-reference retrieval over FTS-only without
      material wrong-scope/stale revival or negative transfer before any
      quality or enablement claim.
- [ ] Measure vec0 v0.1.9 brute-force CPU/memory/latency/concurrency at the
      target corpus and document the equal-distance top-k boundary limitation.

#### Phase 7F - Unix-style L2 residency and shadow prefetch

**Entry gate:** Phase 7E remains explicit-fault-only with a frozen provisional
profile and clean correctness/race/demo verification. Phase 7F may first add
bounded hot/cold/ghost mechanics and shadow accounting without claiming useful
prefetch. Promotion from shadow observation to L2-only prefetch requires a real
embedding run plus held-out usefulness/negative-transfer evidence; automatic
L1 admission remains blocked until Phase 10.

**Objective:** turn retrieval into measured working-set estimation without
making similarity an eviction or pinning policy.

**Status:** implemented and verified on the development platform (darwin). The
bounded residency and metadata-only shadow mechanism below is complete, and the
root final verification matrix — `go test ./...`, `go test -race ./...`,
repeated/stress (`-count`) runs of the memory and residency paths, `go vet`,
build, live `wsms demo`, and `git diff --check` — passes. Independent
adversarial review found one commit-time semantic-retry recall regression (a
single materialization budget shared across coherence-retry attempts made the
retry abstain with a spurious `SEMANTIC_PAGE_MISS`); it is fixed and
regression-tested. The remaining confirmed findings were dead-code cleanups.
This is not an enablement claim for speculative prefetch: the real-model,
held-out usefulness, and negative-transfer gates below remain open.

Implementation:

1. [x] Add CLOCK-Pro/2Q-inspired cold/hot/pinned state. First exact demand is
   cold/ref1; later actual demand/use promotes it, while hot pages demote before
   eviction and pinned pages are not reclaim victims.
2. [x] Enforce default logical bounds: 64 resident pages/512 KiB total, a
   16-page/128-KiB pinned subset included in the total, 64 KiB per page, 64
   bodyless ghosts/32 KiB, 256 bodyless semantic-shadow episodes/64 KiB over a
   64-real-use horizon, and 512 categorical trace entries.
3. [x] Make direct resident hits atomic and admit a selected semantic page as
   demand only after exact L4 materialization and final attempt freshness.
4. [x] Record only non-selected, non-suppressed exact candidate tuples as
   bodyless semantic-shadow observations. Namespace attributes the estimator;
   it does not change authoritative page identity or extend duplicate horizons.
5. [x] Pin active-task and hard-constraint anchors explicitly with
   transactional overflow behavior; similarity cannot pin.
6. [x] Treat invalidation as a shootdown, not an eviction hint: matching
   resident identity is removed, while broad residency-changing transitions
   conservatively purge dependency-free ghosts and censor pending shadows.
7. [x] Expose bounded body-free residency snapshots/traces and deterministic
   fault/use, ghost/refault/thrash, promotion/demotion, pin rejection, and
   shadow useful/unused metrics.
8. [ ] Enable actual speculative L2 body admission only after a pinned real
   Qwen run plus held-out usefulness and negative-transfer evidence.
9. [ ] Consider automatic L1 admission only in Phase 10 after outcome gates.

Verification/gates:

- [x] Root final matrix passes normal, race, repeated/stress, build, demo, and
      diff checks for the integrated Phase 7F worktree.
- [x] Focused mechanism tests cover bounded residency/metadata, deterministic
      second chance and promotion, transactional pin overflow, bodyless exact
      ghosts, shadow useful/unused accounting, invalidation, copy isolation, and
      the commit-time freshness-retry recall path; final integrated verification
      passes.
- [x] Pinned critical state remains resident under ordinary churn within its
      explicit quota.
- [x] Semantic observations cannot admit a body or mutate L1; unused shadow
      episodes expire without masquerading as actual unused prefetch bodies.
- [x] Deterministic capacity and concurrent-churn fixtures keep resident,
      ghost, shadow, and trace work bounded.
- [x] Actual speculative L2 admission is disabled pending the real-model and
      held-out gates; automatic L1 admission stays disabled until Phase 10.

#### Phase 7G - Optional Qdrant scale-out

**Objective:** add ANN/process scale only if the embedded backend misses a
measured target.

Entry gate:

- A checked-in benchmark repeated on supported machines shows SQLite cannot
  meet the agreed corpus, concurrency, memory, or latency SLO. The initial
  target is retrieval p95 below 75 ms after query embedding and complete local
  semantic-fault p95 below 350 ms; these remain unverified until measured.

Implementation:

1. Add a Qdrant `WarmIndex` adapter using the official Go client, one collection
   per embedding namespace, and payload fields required for hard filters and
   explanations.
2. Initially replace only the dense sqlite-vec channel and retain SQLite FTS5
   for lexical retrieval. A native Qdrant sparse/BM25 channel needs a separate
   lexical-parity and abstention gate.
3. Supervise the local service or require explicit remote configuration. Do not
   treat service snapshots as L4 disaster recovery.
4. Dual-build a new backend, run filter/recall/abstention parity and shadow
   queries, then cut over by configuration with a rollback generation.

Verification/gates:

- Behavior matches the embedded backend contract for scope, invalidation,
  explanations, abstention, materialization, degraded mode, and rebuild.
- Scale-out produces a demonstrated SLO/resource benefit large enough to
  justify its operational cost.

### Phase 8 - Async maintenance

**Entry gate:** the synchronous slice through Phase 7F is verified, and async
stays behind a default-off flag (`config.AsyncMaintenance`) until a race+parity
suite is green. This phase does not make async the default and does not touch the
durable append's derivation order.

**Objective:** move best-effort L3 index I/O off the durable append path without
letting the disposable index ever beat exact evidence (A1) or diverge from replay
derivation (A3), and never block a ledger write on index maintenance (NFR-010).

**Status:** implemented and verified on the development platform (darwin), behind
a default-off flag. The bounded per-session index-apply worker below is complete,
and the root final verification matrix — `go test ./...`, `go test -race ./...`
(10x on the maintenance path plus the full harness suite), `go vet`, build, live
`wsms demo`, and `git diff --check` — passes; with the flag off the existing
harness behavior and tests are byte-identical. A multi-lens adversarial review
(replay-equivalence, non-blocking, concurrency, cache-vs-truth, inspection,
resource) with per-finding refutation confirmed two findings, both fixed and
regression-tested: (1) the inspection surface reported "caught up" while a repair
was pending or a rebuild-from-behind was running; (2) a persistent non-gap apply
error retried the identical unit forever instead of reconciling from the ledger
like the synchronous path, and left `IndexErr` unset. A follow-up adversarial
pass confirmed both fixes close their findings with no regression. This is not an
enablement claim: async remains opt-in, and the single-substrate and
observer-async items below stay deferred.

Implementation:

1. [x] Keep page compilation synchronous inside `appendMu`. The compiler reads
   live WSL and coherence state (descriptor generation, page eligibility,
   scope-epoch stamping) that cannot be reconstructed as-of-event off-thread, so
   deferring it would stamp a later epoch and break A3. Only the SQLite apply is
   deferred; compiled mutations are self-contained immutable data.
2. [x] Add a bounded per-session apply queue keyed by ledger `append_seq`,
   applied in sequence via the existing `ApplyWithWatermark` idempotency and
   watermark contiguity. A gap (`ErrWatermarkGap`) reconciles from the ledger
   rather than applying out of order.
3. [x] Never block the durable append (NFR-010): on a full queue or a compile
   failure, `Append` records lag and flags a ledger-watermark reconciliation
   instead of waiting. Overflow is counted; the worker rebuilds from the ledger.
4. [x] Supervise the worker like the embedding worker: coalescing wake, transient
   retry with bounded backoff, park on a terminal closed index. Any non-gap apply
   error reconciles from the ledger (mirroring the synchronous path) rather than
   re-applying the same unit, and documents the stall on `IndexErr`.
5. [x] Cancel-and-discard on `Close`: pending applies are dropped, the worker is
   joined before the index is closed, and the next `OpenSession` replays them
   from the ledger watermark. Replay itself stays synchronous.
6. [x] Expose a synchronized, redacted `MaintenanceStatus` (async/degraded/
   parked/reconciling/category/pending/dropped) for operator inspection. It never
   carries raw error text, is never authoritative, and never gates L4 or exact
   faults.
7. [ ] Generalize the separate embedding and index-apply workers into one shared
   idempotency/watermark/retry/dead-letter substrate; today the index-apply
   worker wakes the existing embedding worker after applies rather than sharing
   its queue.
8. [ ] Add ordered-commit observer-async and a durable dead-letter store behind
   that substrate; the current inspection surface is park + `IndexErr` + status,
   not a persisted dead-letter queue.
9. [ ] Promote async to the default only after a broader real-workload bake.

Verification/gates:

- [x] Async lag never returns a stale semantic result: the existing
      watermark-freshness gate abstains (`ErrIndexUnavailable`) whenever the
      index watermark trails the live ledger head, and a documented failure sets
      `IndexErr` so the semantic path treats the index as unavailable; exact page
      faults read L4 directly and are unaffected.
- [x] Drained async maintenance produces the same exact-evidence materialization
      as the synchronous path (quiescent A3 equivalence), including after
      bounded-queue overflow reconciled from the ledger and after a
      close-discard/reopen catch-up.
- [x] The durable append never fails or blocks under a depth-1 queue driven to
      sustained overflow; concurrent appends and searches stay race-clean, with a
      lagging index legitimately abstaining rather than erroring.
- [x] Root final matrix passes normal, race (incl. repeated maintenance runs),
      build, demo, and diff checks; the flag-off path is unchanged.

### Phase 9 - Provider adapters and operator UX

- [x] Add one hosted and one local OpenAI-compatible adapter behind
  `harness.Client`. The pi provider seam realizes this: `wsms-local`
  (`WSMS_LLAMA_BASE_URL`) and hosted `wsms-openai` (gated on `OPENAI_API_KEY`,
  off unless configured), with a keyless `wsms-mock` echo model as the offline
  default. Credentials stay env-only; no secret is committed.
- [x] Define timeout, cancellation, streaming, tool-call, and provider-compaction
  interactions. Documented as a runtime contract in `pi-bridge/README.md`,
  grounded in pi's actual behavior. Streaming: incremental
  `assistantMessageEvent.delta` (`text_delta`-gated), authoritative content at
  `message_end`, only finalized non-empty turns ingested. Tool-call: the
  `wsms_read_page`/`wsms_recall` round-trip is verified live keyless (mock
  `toolUse` → pi dispatch → bridge → `serve /page` → `toolResult` → durable
  echo). Cancellation: pi threads the run `AbortSignal` into tool `execute`,
  now forwarded into the core request so an abort cancels an in-flight fetch.
  Timeout: pi has no built-in model/tool deadline, so the bridge bounds each core
  call by the run signal or a 5 s timeout. Provider-compaction: the ephemeral
  capsule (recomputed per turn, never compacted) plus the L4 ledger (built before
  compaction) plus page-fault recovery make pi's lossy compaction non-destructive.
- [x] Add session/event/state/page inspection and explicit export/delete
  commands. `internal/operator` + `wsms inspect|export|delete|purge`: seven
  read-only views, replay-complete JSONL export, logical delete via
  `memory_invalidated` (L4-retained, cache-honored), and an offline
  confirmation-gated purge that never touches the data dir or shared artifacts.

**Implementation status:** Mechanism-complete. Provider adapters (local + hosted
+ offline mock), the streamed-text contract, the tool-call round-trip, the
cancellation/timeout/compaction interaction contract, and the operator UX are
all done and committed. The operator commands ship with an end-to-end test suite
that asserts the invariants (e.g. deleting a failure record drops it from the L1
capsule while the raw L4 state keeps its `@invalidated` tombstone); the
`wsms_read_page` round-trip is verified live keyless (mock `toolUse` → pi
dispatch → bridge → `serve /page` → `toolResult` → durable echo). Two non-blocking
residuals remain, neither a mechanism gap: the non-mock provider model
descriptors in `providers.ts` are placeholder defaults pending an owner decision
on the actually-served models, and the bubbletea terminal render is only
pure-function tested (`View()`) since no TTY is available here.

### Phase 10 - Forced-reset benchmark

The full benchmark-system design is `docs/forced-reset-benchmark.md`: matched
conditions over byte-identical frozen pre-reset state, nonce-bearing synthetic
scenarios with programmatic oracles, cluster-aware paired statistics,
pre-registered decision gates (WSL keep/simplify/reject, the semantic ladder,
instrument validity), and a hard offline-keyless vs real-model-gated split.
The items below are the original scope; the design doc is normative where
they differ.

- Build matched baselines and a task/event injection protocol.
- Freeze event streams, capsules, and outcomes as reproducible artifacts.
- Measure continuation success, repeated failures, constraints, exact recall,
  stale assumptions, faults, token use, latency, and reminders.
- For L3 variants, additionally measure Recall@k, MRR/nDCG, exact-reference
  precision, abstention, wrong-scope/stale revival, negative transfer,
  useful-prefetch ratio, index/rebuild cost, and tokens per useful page.
- Gate semantic behavior in this order: explicit fault tool, L2-only prefetch,
  then automatic L1 admission. Each promotion requires held-out end-to-end
  improvement or equal success at lower token cost.
- Simplify or reject WSL if it does not beat strong YAML/Markdown baselines.

## 10. Risk register

| Risk | Early signal | Mitigation/gate |
|---|---|---|
| WSL is ceremony | Strong YAML matches it | Benchmark strong structured baselines; simplify if needed |
| Replay IDs drift | Observer changes alter refs | Version observers; persist stable derivation identity before evolving algorithms |
| Poisoned memory | Tool text becomes authority | Trusted-source rules, quoting, provenance, deterministic validation |
| Cross-scope leakage | Wrong branch/session state appears | Composite IDs, scoped lookups, scope gate before ranking |
| Incomplete authority starves valid pages | Invalid active rows consume every top-k slot | Complete descriptor-derived exact tuple allowlist before FTS/KNN; over-bound snapshots fail operationally |
| Async nondeterminism | Live and replay state differ | Ordered commit, watermarks, equivalence tests before enabling workers |
| Evidence corruption hidden as miss | `PAGE_MISS` on I/O failure | Separate absence from operational error |
| Demo overclaims research | “outperforms” without benchmark | Mechanism-proof wording and acceptance contract |
| Schema churn | Old DB opens incorrectly | Introduce schema version/migrations before first tagged compatibility promise |
| Vector index becomes shadow truth | Deleting/rebuilding L3 loses behavior or evidence | Separate `warm.db`; exact refs and L4 rebuild invariant |
| Semantically close but wrong memory | Similar page revives wrong task/branch assumption | Hard filters, post-validation, threshold/abstention, negative-transfer metric |
| Embedding drift | Model/profile update silently changes ranks | Complete namespaces, new generation, dual-build parity before cutover |
| Secret leakage to embeddings | Raw logs/source leave local machine | Typed pages, deny/redact before embed, local default, hosted opt-in |
| Pre-1.0 sqlite-vec instability | Platform/restart/filter tests diverge | Compatibility spike, exact oracle, FTS fallback, adapter isolation |
| ANN service complexity without value | Qdrant adds failure modes at small scale | Measurement entry gate and embedded backend retained |
| Readahead pollutes L1 | Similar but unused pages consume prompt | Shadow/L2-only prefetch, useful-prefetch metric, independent L1 scheduler |

## 11. Handoff protocol for future phases

Every later phase should begin by reading:

1. `docs/specification.md` requirements in scope.
2. The corresponding architecture section and invariant.
3. This plan phase's allowed APIs and anti-pattern guards.
4. Current `git status`, relevant tests, and actual package APIs.

Every phase should end with:

- files changed;
- exact commands and results;
- acceptance items proven;
- known gaps and residual risks;
- no claim beyond the executed evidence.
