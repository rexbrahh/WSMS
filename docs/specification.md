# WSMS Product Specification

**Status:** Normative product specification; section 11 defines the first demo milestone  
**Version:** 0.1  
**Date:** 2026-07-10  
**Product:** Working State Management System (WSMS)  
**Internal state language:** WSL v0

## 1. Purpose

WSMS is a local runtime that keeps a coding agent's operational working state
sharp across long sessions, compaction, and process restarts. It treats model
context as a small resident working set, not as the durable source of truth.

The runtime records exact session evidence in an append-only ledger and a
content-addressed artifact store. Deterministic observers derive typed WSL
records. A scheduler selects a budgeted L1 working-state capsule for the next
model turn. Missing evidence is retrieved on demand through page-fault tools.

The core product claim is intentionally narrow and testable:

> Under the same or a smaller active-token budget, a typed, inspectable working
> state backed by exact evidence should preserve coding-session continuity
> better than transcript truncation or summary-only continuation.

This specification turns the research drafts into an implementable product
contract. The brainstorm reports remain research inputs; this file, the WSL v0
grammar, and the architecture document are the authority for MVP behavior.

The first demo is a complete vertical slice of the core mechanism, not the
entire product specification. Requirements explicitly assigned to later
reliability, residency, adapter, and evaluation phases remain normative product
work but are not prerequisites for the section 11 demo milestone.

## 2. Source authority

When sources disagree, use this order:

1. `docs/specification.md` for product behavior and acceptance criteria.
2. `docs/wsl/v0.md` for WSL v0 syntax and record semantics.
3. `docs/architecture.md` for component ownership and runtime invariants.
4. `docs/l3-warm-memory.md` for post-demo semantic page, index, embedding,
   retrieval, residency, and L3 rollout contracts.
5. Current tests for behavior already enforced by executable checks.
6. `docs/early_report.md`, `docs/deep-research-report (6).md`, and the PDF as
   research rationale, not API authority.

The checked-in language remains **WSL v0**. Later research drafts sometimes call
it v1, but no promotion or migration has been specified. The attached PDF is 13
pages, despite stale prose claiming 21 pages.

## 3. Users and jobs

### 3.1 Primary user

A developer operating a local coding agent during a long, synchronous,
human-supervised session.

Primary jobs:

- Resume after compaction, reset, or process restart without re-explaining the
  active task.
- Preserve hard user constraints and prevent them from disappearing into a
  lossy summary.
- Retain exact failed commands, exit codes, error strings, paths, and raw logs.
- Avoid retrying an approach that already failed or was explicitly rejected.
- Keep branch-, task-, repo-, and file-scoped state from contaminating other
  work.
- Inspect the exact state capsule presented to the model.
- Retrieve missing evidence by stable identifier instead of guessing.

### 3.2 Secondary users

- **Harness implementer:** integrates WSMS at model/tool lifecycle boundaries.
- **Evaluation researcher:** compares WSMS with transcript, summary, YAML,
  Markdown, retrieval-only, and provider-compaction baselines.

## 4. Problem definition

WSMS targets **working-state degradation**, meaning loss, corruption,
misprioritization, or stale revival of task-relevant operational state during a
long agent session.

Observable failure modes are:

1. Repeating a failed command, patch, or approach.
2. Losing a user correction or hard constraint.
3. Remembering that a test failed while losing its exact command and error.
4. Preserving the story of the session but losing the next executable action.
5. Allowing large tool output to crowd out more important state.
6. Reviving an assumption after later evidence invalidated it.
7. Reusing state from the wrong branch, task, repo, or file.
8. Forcing the human to restate information already present in durable evidence.

## 5. Product outcomes

### 5.1 MVP outcomes

- A complete local event-to-capsule-to-page-fault path runs without an API key.
- Operational state survives closing and reopening a session.
- Exact large command output survives outside the capsule and is recoverable
  byte-for-byte.
- Hard constraints remain present under a constrained capsule budget.
- The runtime exposes a deterministic demo that proves the above properties.

### 5.2 Research outcomes

- Lower repeated-failure rate after forced resets.
- Lower hard-constraint loss rate.
- Higher exact-error recall.
- Lower stale-assumption recurrence.
- Fewer human reminders.
- Equal or lower active-token use at comparable task success.

### 5.3 Non-goals for MVP

- A general-purpose personal-memory product.
- A production chat UI or autonomous coding agent.
- A new model, private latent protocol, or provider retraining requirement.
- KV-cache or hidden-state interchange across providers.
- Embedding/ANN retrieval, a temporal knowledge graph, or an L3 ranking service
  in the **first mechanism demo**. Hybrid L3 retrieval is normative post-demo
  product work under `docs/l3-warm-memory.md`; it is not required to prove the
  initial durable paging slice.
- A full forced-reset benchmark runner.
- Model-authored truth without deterministic provenance.
- Multi-process observer workers or distributed scheduling.

## 6. Product principles

1. **Ledger truth, derived caches.** L1-L3 may be rebuilt; L4 evidence must not
   depend on them.
2. **Exact evidence stays exact.** Commands, errors, paths, constraint text, and
   artifact bytes are never replaced by paraphrase.
3. **Conservative derivation.** Observers emit only facts supported by an event.
4. **Boring external ABI.** Hosted/local models receive structured text and
   stable tools, not a private latent representation.
5. **Predictable boundaries.** Context changes only at `AfterEvent`,
   `BeforeTurn`, or `PageFault`.
6. **Scope before relevance.** Out-of-scope or stale state cannot become
   resident merely because it scores highly.
7. **Inspectability before cleverness.** A human can inspect capsules, records,
   pages, and evidence pointers.
8. **Strong baselines.** WSL must eventually beat carefully engineered YAML or
   Markdown, not only weak transcript truncation.
9. **Unix mechanism/policy split.** Stable evidence addressing and page-fault
   resolution are mechanisms; promotion, pinning, eviction, and revalidation are
   scheduler policy.
10. **Approximation discovers addresses, never truth.** Lexical/vector retrieval
    may nominate pages for inspection, but current scope checks and exact L4
    references determine what can be materialized.

## 7. Domain model

| Concept | Meaning | Authority |
|---|---|---|
| Event | Immutable observation from the user, model, tool, repo, or runtime | SQLite ledger |
| Artifact | Large immutable byte payload addressed by SHA-256 | Artifact store |
| WSL record | Typed operational fact derived from events or explicit input | Rebuildable working state |
| Page | Bounded, retrievable evidence or detail with scope/residency metadata | L2/L3 cache plus L4 refs |
| Capsule | Budgeted provider-facing rendering of the current L1 working set | Rebuilt before a turn |
| Fault | Request for non-resident evidence by known address or semantic intent | Resolver/retriever response |
| Session | Ordered event stream and derived state for one agent work session | Ledger `session_id` |

## 8. Functional requirements

### FR-001 - Append-only event ledger (MUST)

The runtime must append immutable events with an ID, timestamp, event type,
session ID, optional task/repo/branch/commit scope, JSON payload, and optional
artifact hash.

Acceptance:

- Appending returns the stored event with a stable ID and timestamp.
- No runtime API updates or deletes an event.
- Every event receives a store-assigned, per-session append sequence. Listing
  and replay use that sequence, not caller timestamps.
- Event identity is unique within a session and cannot collide with another
  session sharing the same database.
- A ledger handle cannot append into, get from, or list a different session.
- One `Session` serializes its append-and-derivation boundary so concurrent
  callers observe the same record order live and after replay.

### FR-002 - Content-addressed artifacts (MUST)

Payloads above the configured threshold must be stored by SHA-256 and replaced
in the ledger payload with a bounded preview plus an artifact reference.

Acceptance:

- Equal bytes produce the same content hash.
- Artifact refs and lookup hashes are exactly 64 hexadecimal SHA-256 digits.
- `Get(hash)` recomputes the digest and returns the exact stored bytes only when
  it matches; malformed, missing, or corrupted referenced artifacts are errors.
- A failed command's full output can be recovered through its failure or event
  ID after a session restart.

### FR-003 - Deterministic session replay (MUST)

Opening an existing session must reconstruct its derived WSL state and hot
pages by replaying durable events through the same observer and scheduler path
used for live events.

Acceptance:

- Closing and reopening a session produces a semantically equivalent WSL state.
- The pre-close and post-reopen capsules preserve the same exact hard
  constraint and failure evidence.
- Replay performs no new ledger writes.
- Replay follows durable append sequence even when event timestamps are
  out-of-order or equal.
- A subsequent live event receives the next event and derived-record IDs rather
  than overwriting replayed state.

### FR-004 - Observer dispatch (MUST)

Observers must consume one event at a time in stable configured order and emit
zero or more typed WSL updates.

All updates emitted by one event must validate and commit atomically. A failed
second update cannot leave the first update or its L2 page visible.

MVP observers:

- Task lifecycle: explicit task start/update to `@task`.
- Constraint extraction: explicit hard/soft user language to `@constraint`.
- Tool digest: failed command/test/tool output to `@failure`.
- Decision/avoidance: explicit decision evidence to `@decision` and `@avoid`.
- Next action: explicit operational next step to `@next`.

Branch/file staleness is specified for the post-demo reliability phase. It is a
product requirement, not part of the first vertical demo, and a stub must not be
represented as working.

### FR-005 - WSL v0 parsing and canonical serialization (MUST)

The parser and serializer must support the record family in `docs/wsl/v0.md`.
Canonical serialization must be stable, and parsing serialized records must be
semantically equivalent to the source records.

### FR-006 - WSL semantic validation (MUST)

The store must reject error-severity lint violations before mutating state.

Required invariants:

- A decision cannot contradict a hard constraint without a future explicit
  override mechanism.
- `@avoid` and `@invalidated` refs point to known record/event IDs.
- Exact failure and hard-constraint evidence is immutable once established.
- Branch-scoped pages cannot be promoted into a different branch.
- A page backed only by stale refs cannot be promoted without revalidation.

The first demo milestone directly proves exact-field immutability, known
references, and hard-constraint contradiction handling. Branch/page scope and
stale-only promotion are completed in the later coherence phase.

### FR-007 - Memory hierarchy (MUST)

The runtime must expose semantic tiers:

- L0: ephemeral turn scratch.
- L1: current rendered capsule.
- L2: hot pages ready for fault resolution.
- L3: warm indexed pages; interface-only is acceptable in MVP.
- L4: ledger and artifact truth.

L1-L3 must be safe to rebuild from durable truth. L3's target design is a
separate, disposable hybrid lexical/vector index; deleting it may reduce recall
or increase latency but must not lose evidence or break direct faults.

### FR-008 - Budgeted L1 scheduling (MUST)

`BeforeTurn` must render an L1 capsule under the configured approximate token
budget. Hard constraints are pinned. Optional blocks are dropped in documented
priority order.

For the MVP renderer, priority is:

1. Active task.
2. Hard constraints.
3. Last failure.
4. Avoids.
5. Next action.
6. Soft constraints.
7. Page-fault instruction (always present).

If the active task plus hard constraints exceed the budget, the capsule may
exceed the nominal budget rather than silently drop a hard constraint. This
exception must be observable.

### FR-009 - Renderer ABI (MUST)

The provider-facing capsule must:

- Start with `<working_state>` and end with `</working_state>`.
- Use stable block labels and record IDs.
- Preserve exact commands, exits, error strings, constraints, and paths.
- End with a clear instruction to request a page rather than guess.
- Avoid provider-specific or latent-only features.

### FR-010 - Page-fault tools (MUST)

The fault layer must expose:

- `ReadPage(pageID, budget)` for WSL/page detail.
- `ReadRawLog(id, budget)` for exact artifact/event evidence.
- `ReadFileSlice(path, start, end)` for bounded local file evidence.

Unknown or unavailable targets return the stable sentinel `PAGE_MISS`; they do
not fabricate fallback text.

For a failure with inline output, `ReadRawLog(F...)` follows the WSL provenance
mapping to its source event. Artifact-backed and inline failures therefore share
one fault ABI.

`file_slice` is a low-level primitive. A production harness must bind it to an
authorized workspace root before exposing it to a model. The first demo uses
page and raw-log faults only and does not claim an authorized file-slice surface.

### FR-011 - Foreground harness boundary (MUST)

The foreground loop must append the user event, build the capsule at
`BeforeTurn`, place it in a system message, invoke a `Client`, and append the
assistant result. A client error must not be disguised as an assistant success.

### FR-012 - Local vertical-slice demo (MUST)

`wsms demo` must execute a deterministic, no-network scenario covering:

1. Session/task start.
2. Hard user constraint.
3. Failed command with an artifact-sized raw log.
4. Decision/avoidance and next action.
5. L1 capsule rendering.
6. Session close and reopen.
7. Equivalent state reconstruction from the ledger.
8. Page fault for structured failure detail.
9. Raw-log fault for exact artifact bytes.
10. Provider-boundary turn through a deterministic local `Client`.

The operator output must narrate the Unix-style lifecycle using recognizable
stages: backing-store append, resident working-set render, runtime-memory loss,
page fault/page-in, and reconstructed mappings after reopen.

The command exits nonzero on an unmet invariant and prints a final unambiguous
success marker only after all checks pass.

### FR-013 - Operator inspection (SHOULD)

The CLI should later expose session event, WSL, capsule, page, and artifact
inspection without requiring SQLite knowledge. The demo's printed evidence is
the MVP inspection surface.

### FR-014 - Forced-reset evaluation (SHOULD)

A later benchmark runner should execute matched tasks across transcript,
summary, YAML/Markdown, retrieval-only, WSL-only, and WSMS-with-faults
conditions. It is not part of the first demo.

### FR-015 - Semantic memory pages (MUST after the demo)

Deterministic observers and a versioned page compiler must derive bounded,
single-topic warm pages from ledger evidence. Initial page kinds are failure
episodes, decisions, constraints, task checkpoints, known-good commands, repo
facts, and file context.

Acceptance:

- Every page has a stable ID/version, typed kind, current status, scope/trust
  metadata, inspectable search text, and exact L4 references.
- A page records its source sequence range/digest and compiler version.
- Raw artifacts and transcripts are not embedded wholesale by default.
- Replaying the same ledger through the same compiler produces the same logical
  page identities and searchable text.
- A model-assisted synopsis, if enabled later, remains labeled derivative data
  and cannot establish a hard constraint or verified fact.

### FR-016 - Hybrid L3 retrieval (MUST after the demo)

The runtime must support semantic faults when a caller knows the information
need but not a stable address. Candidate generation combines FTS5 lexical/BM25
search with dense cosine search inside one embedding namespace, followed by
deterministic fusion, policy reranking, diversity control, and abstention.

Acceptance:

- Requests containing stable IDs bypass vector/lexical search and use the
  direct resolver.
- Session/ACL, repo, task/branch compatibility, validity, trust, page kind, and
  scope epoch are applied as hard gates before scoring and rechecked before
  materialization.
- Search returns bounded page references and explanations, not authoritative
  prose.
- Selected candidates are materialized from current L4 refs through the normal
  validation and budget path.
- No qualifying result returns `SEMANTIC_PAGE_MISS`; operational index failure
  is distinguishable from a semantic miss.
- Embedder failure visibly degrades to lexical search without blocking event
  persistence or known-ID faults.

### FR-017 - Rebuildable L3 backend (MUST after the demo)

The default L3 backend must use a separate local index database and be
reconstructible from L4. The reference architecture uses SQLite FTS5 plus a
compatibility-gated `sqlite-vec` projection. A brute-force exact cosine backend
serves as the test oracle. Qdrant is an optional measured scale-out adapter, not
a second truth store.

Acceptance:

- Removing `<data-dir>/index` does not remove ledger events, artifacts, or WSL
  replay capability.
- Per-session index watermarks expose lag and drive idempotent catch-up.
- Schema, compiler, redaction, or embedding incompatibility creates a new index
  generation rather than silently mixing representations.
- Metadata/FTS/vector projections expose the same active page version after a
  batch commits.
- A rebuild is validated before atomic generation cutover and can be
  interrupted without corrupting the serving generation.
- The optional ANN backend passes the same filter, explanation, abstention, and
  materialization contract as the embedded backend.

### FR-018 - Embedding profile and privacy (MUST after the demo)

Every vector must belong to a namespace that covers provider, exact model
revision, dimensions, distance metric, normalization, query instruction,
document template, tokenizer, page schema, and redaction version. Local
embedding is the reference mode; hosted embedding is explicit opt-in.

Acceptance:

- Vectors from different namespaces are never compared.
- Query/document preprocessing asymmetry is explicit and tested.
- Secrets, denied files, and unrestricted artifact bytes are excluded before
  any embedding call.
- The exact inspectable search text used for a document embedding is retained
  locally with its source digest.
- A hosted provider requires explicit configuration, bounded/redacted payloads,
  deadlines, cost/error telemetry, and a separate namespace.

### FR-019 - Working-set admission and readahead (SHOULD after hybrid faults)

Semantic relevance may nominate page references for L2 prefetch, but L2
residency and L1 admission use separate Unix-inspired hot/cold/pinned policy.
Prefetched pages do not enter L1 solely because they are vector neighbors.

Acceptance:

- Hard constraints and active task anchors are pinned explicitly, not by
  similarity score.
- L2 tracks reference/use state and distinguishes explicit faults from
  speculative prefetch.
- Useful-prefetch ratio, unused eviction, ghost hits, and thrash are observable.
- Automatic prefetch runs in shadow mode until forced-reset evaluation shows
  benefit without material negative transfer.
- All policy weights, thresholds, and caps are named, versioned, and included
  in retrieval explanations.

## 9. Non-functional requirements

### NFR-001 - Local-first and offline

Core runtime behavior and all repository tests must run without an API key or
network dependency.

### NFR-002 - Runtime ownership

Go owns ledger truth, WSL authority, scheduling, rendering, and page faults.
Python is limited to frozen-data evaluation. Rust is reserved for measured
hotspots behind a sidecar/process boundary.

### NFR-003 - Determinism

Given the same ordered event stream, observer version, configuration, and WSL
version, replay must produce semantically equivalent derived state.

### NFR-004 - Auditability

Every exact-evidence record must retain an event or artifact path back to L4.
The MVP may represent this as a deterministic event-to-record derivation index
reconstructed during replay rather than adding provenance fields to WSL v0.
Capsules and fault responses must be inspectable plain text.

### NFR-005 - Safety and provenance

Untrusted tool/user text remains data. It cannot silently become a system-level
instruction or global memory. Future model-assisted observers must emit
provenance and pass deterministic validation.

### NFR-006 - Compatibility

MVP file formats and public Go interfaces should remain backward compatible.
Any incompatible WSL, ledger, or capsule change requires an explicit version
and migration policy.

### NFR-007 - Reliability

The ledger and artifact store must fail visibly on write/open/corruption errors.
No broad catch may return success-shaped defaults. `PAGE_MISS` is reserved for
a valid lookup with no resolvable target.

### NFR-008 - Concurrency

The MVP may process observers synchronously at safe boundaries. Later async
workers must preserve per-session event order, idempotency, bounded queues, and
deterministic state application.

### NFR-009 - Performance budgets

- Capsule construction should be linear in resident record count for MVP.
- Large output must not be copied into WSL or the capsule.
- Page-fault output must respect its approximate token budget.
- L3 search must bound lexical/dense candidates, materialized pages, bytes, and
  tokens independently.
- The initial post-embedding retrieval target is p95 below 75 ms on supported
  local machines; the complete local semantic-fault target is p95 below 350 ms.
  These are acceptance targets to measure, not current performance claims.

### NFR-010 - L3 availability isolation

The L3 index and embedding adapter are optional derivative services. Their
timeout, lag, corruption, rebuild, or absence must not block ledger writes,
session replay, current L1/L2 state, or direct page/raw-log faults.

## 10. Normative data contracts

### 10.1 Event envelope

```text
id            store-assigned session-local monotonic ID, e.g. E0001
append_seq    store-assigned session-local append ordinal
ts            UTC RFC3339Nano timestamp
type          stable event type string
session_id    non-empty session scope
task_id       optional active task ID
repo          optional repository identity
branch        optional branch identity
commit_sha    optional commit identity
payload_json  event-specific JSON object
artifact_hash optional SHA-256 for offloaded payload
scope_json    additional structured scope metadata
```

The durable identity is `(session_id, id)`. Replay order is
`(session_id, append_seq)`. Caller-supplied timestamps are observation metadata,
not ordering authority. The live append API rejects caller-supplied event IDs;
imports, if added later, require a separate validated interface.

### 10.2 MVP event payloads

| Event type | Required payload | Optional payload |
|---|---|---|
| `task_started` | `goal` | `task_id`, `phase`, `priority`, `branch`, `dirty` |
| `user_instruction` | `text` | scope metadata |
| `command_output` | `cmd`, `exit`, `output` | `err`, `file_hint`, `raw` |
| `decision` | `chosen` | `because`, `refs`, `scope`, `avoid_text`, `avoid_ref` |
| `next_action` | `action`, `target` | `question` |
| `assistant_message` | `text` | none |

Unknown payload fields must be ignored unless a versioned event schema later
declares them invalid.

Known event types and their required fields are validated before durable append.
Replay validates persisted envelopes again and reports the event ID on failure.
Unknown future event types may remain inert for forward compatibility, but an
empty event type is invalid and observers may not invent fallback exact evidence
for a malformed known event.

### 10.3 WSL v0

The normative grammar and record fields are in `docs/wsl/v0.md`. The MVP does
not invent multiline values, unknown record kinds, or an override syntax.

### 10.4 Capsule ABI

The exact prose may evolve within v0, but tags, block labels, record IDs, exact
evidence, and the page-fault instruction are compatibility commitments.

### 10.5 Fault ABI

Fault kinds are `page`, `raw_log`, and `file_slice`. A successful response is
bounded plain text. A miss is exactly `PAGE_MISS`.

The post-demo semantic fault ABI accepts a typed query intent and returns either
bounded ranked page references with explanations, `SEMANTIC_PAGE_MISS`, or an
operational error. It then uses the existing direct fault/materialization path;
it does not define a second evidence ABI.

### 10.6 L3 index ABI

The normative page, query, namespace, backend, consistency, and degraded-mode
contracts are in `docs/l3-warm-memory.md`. Backend-native IDs, scores, clients,
and query expressions must not cross the Go `WarmIndex` boundary.

## 11. Demo acceptance contract

The demo is complete only when all of these are proven by current executable
evidence:

- [ ] `go run ./cmd/wsms demo` exits 0.
- [ ] Output shows an appended event stream and generated record IDs.
- [ ] Output labels the durable backing store and resident working set rather
      than presenting the capsule as the entire memory system.
- [ ] The capsule includes an active task, hard constraint, exact failed
      command/error, avoidance, next action, and page-fault instruction.
- [ ] The demo closes and reopens the same session before its final capsule.
- [ ] The reopened capsule retains the same critical evidence.
- [ ] `ReadPage(F...)` returns structured failure detail.
- [ ] The page-fault stage identifies the faulted ID and successful page-in.
- [ ] `ReadRawLog(F...)` returns the full offloaded log, including a sentinel
      placed beyond the ledger preview.
- [ ] The artifact ref is syntactically valid and the recovered bytes pass an
      independently recomputed SHA-256 check.
- [ ] Every demo-derived task/constraint/failure/decision/avoid/next record can
      report the ledger event from which it was derived.
- [ ] A deterministic local client receives the capsule through
      `harness.Loop` and returns a continuation response.
- [ ] A second session can use the same ledger database without event-ID
      collision or cross-session append/get/list access.
- [ ] `go test ./...`, `go test -race ./...`, `go vet ./...`, and a production
      CLI build pass.
- [ ] The documented one-command demo matches current output semantics.

## 12. Evaluation specification

### 12.1 Baselines

1. Full transcript until limit.
2. Transcript truncation.
3. Natural-language summary.
4. Carefully engineered YAML checkpoint.
5. Plain Markdown task/progress file.
6. Retrieval-only external memory.
7. WSL without scheduling.
8. WSL plus scheduling.
9. WSL plus scheduling and page faults.
10. WSL plus FTS-only semantic faults.
11. WSL plus dense-only semantic faults.
12. WSL plus hybrid semantic faults and policy reranking.
13. Provider-native compaction where available.

### 12.2 Forced-reset protocol

Each matched run should include realistic tool noise, at least one failed
attempt, at least one hard user correction, and a controlled reset boundary.
Resume with only the assigned continuation substrate and score downstream
behavior.

### 12.3 Primary metrics

- Task success after reset.
- Repeated failed-attempt rate.
- Hard-constraint violation rate.
- Exact-error recall.
- Stale-assumption recurrence.
- Page-fault precision and recall.
- Semantic-fault Recall@k, MRR/nDCG, exact-reference precision, and abstention.
- Wrong-scope retrieval, stale revival, negative transfer, and useful-prefetch
  rate.
- Invalid WSL update rate.
- Active-token budget and latency overhead.
- Human reminder count.

## 13. Security and privacy

Threats include prompt-injection persistence, malicious tool output, scope
confusion, artifact leakage, path traversal, stale governance state, and
cross-session contamination.

MVP controls:

- Preserve provenance and scope on every event/record.
- Treat observed content as quoted data in the capsule.
- Restrict file-slice reads to caller-authorized workspace paths at the harness
  boundary; the low-level resolver alone is not a sandbox.
- Use content hashes and explicit artifact references.
- Never promote a hard constraint out of a lossy, model-authored summary.
- Keep session identity in every ledger lookup.
- Return visible errors or `PAGE_MISS`, never guessed evidence.

Post-demo L3 controls:

- Index only typed, bounded search text and exact refs; exclude unrestricted
  raw artifacts/transcripts and denied or secret-bearing content.
- Preserve source trust and quote imperative tool/repo text as evidence after
  retrieval.
- Apply scope/ACL, trust, validity, and invalidation before ranking and recheck
  them before L4 materialization.
- Keep hosted embedding disabled by default; enabling it requires explicit
  provider configuration, redaction, inspectable payload policy, deadlines,
  and cost/error telemetry.
- Keep lexical/vector rows in a disposable generation separate from L4 truth.

Deletion/export, retention, encryption at rest, and team permissions require a
later product policy before multi-user deployment.

## 14. Key decisions and open questions

### Decided for MVP

- WSL stays at v0.
- The runtime stays Go-only.
- Observer execution is synchronous but boundary-separated and replayable.
- SQLite plus content-addressed files are the durable local substrate.
- Event identity is `(session_id, id)`.
- The demo is deterministic and provider-independent.
- Restart replay, not transcript replay, is required.
- A full forced-reset benchmark is planned but not part of the demo.

### Open after MVP

- Durable observer/version metadata and snapshot migration.
- Formal event schema versioning and duplicate-ingestion keys.
- Explicit WSL override syntax.
- Normative multiline/escaping grammar.
- Calibrated L3 fusion/reranking weights, thresholds, and automatic-prefetch
  enablement. The mechanism and rollout gates are decided in
  `docs/l3-warm-memory.md`.
- Exact L2 hot/cold/ghost target tuning and branch/file revalidation algorithms.
- Branch/file staleness algorithms.
- Artifact retention, compression, corruption repair, and garbage collection.
- Provider adapter ownership and cancellation/timeout semantics.
- Operator CLI/TUI and export/delete UX.
- Whether WSL beats strong YAML; if not, simplify the representation.

## 15. Research validation

Primary-source checks completed on 2026-07-10 support the architecture's
premises without proving WSMS's product claim:

- [OpenAI compaction](https://developers.openai.com/api/docs/guides/compaction)
  supports long-running state reduction but returns an opaque encrypted
  compaction item. WSMS adds inspectable operational state; it does not replace
  provider compaction.
- [Anthropic compaction](https://platform.claude.com/docs/en/build-with-claude/compaction)
  explicitly summarizes older context to keep active context small. This
  reinforces the need to evaluate exact-evidence loss rather than assume it.
- [Lost in the Middle](https://aclanthology.org/2024.tacl-1.9/) shows that long
  context capacity does not guarantee robust use of information at every
  position.
- [MemGPT](https://arxiv.org/abs/2310.08560) validates virtual-context and page-
  fault framing, but WSMS narrows the target to inspectable coding-session
  operational state.
- [Anthropic's long-running-agent harness study](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents)
  reports that progress files, incremental work, and end-to-end verification
  improve continuity. WSMS makes those ideas a typed runtime rather than a
  manually maintained convention.

These sources motivate the experiment. They do not establish that WSL or the
WSMS scheduler outperforms strong structured-text baselines; only the planned
evaluation can do that.
