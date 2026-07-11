# WSMS Architecture

**Status:** Normative product architecture; mechanism demo and Phase 7E hybrid semantic faults are implemented
**Version:** 0.1  
**Date:** 2026-07-10

## 1. Architectural goal

WSMS keeps a coding agent's operational working state outside the transcript
while preserving exact evidence. The model context is a bounded resident set;
the append-only ledger and content-addressed artifacts are durable truth.

The architecture optimizes four properties:

1. **Continuity:** active task state survives compaction and process restart.
2. **Fidelity:** exact commands, errors, constraints, paths, and raw logs remain
   recoverable without paraphrase.
3. **Sharpness:** only the most useful state is resident in the next model turn.
4. **Inspectability:** humans can see why a fact is resident and retrieve its
   evidence.

## 1.1 Unix virtual-memory correspondence

Unix memory-management design is the organizing model for WSMS, not merely a
naming metaphor. WSMS borrows the separation between a process's active working
set and its larger durable address space, plus the separation of paging
mechanism from residency policy.

| Unix / VM concept | WSMS analogue | Architectural consequence |
|---|---|---|
| Process virtual address space | Complete session event/evidence space | The agent can address more history than fits in context |
| Resident working set | L1 `<working_state>` capsule | Context contains the currently useful subset, not all history |
| Page / virtual address | Stable WSL record, page, event, or artifact ID | Missing detail is requested by identity rather than guessed |
| Page table / mapping metadata | `WorkingState`, hierarchy metadata, provenance index | IDs resolve to current typed state and backing evidence |
| Physical frame budget | Capsule token budget | Residency is explicitly bounded |
| Pinned / unevictable page | Hard user constraint | Governance-critical state survives ordinary reclamation |
| Backing store / swap | Append-only ledger plus content-addressed artifacts | Exact truth remains outside the resident prompt |
| Page fault | `ReadPage` / `ReadRawLog` request | Non-resident evidence is loaded on demand |
| Page-in | Fault result rendered into the current interaction | Detail becomes temporarily available without bloating every turn |
| Eviction / reclamation | Dropping optional capsule blocks or demoting pages | Low-value state leaves L1 while remaining recoverable |
| Working-set locality | Scope, recency, salience, access, and staleness policy | Scheduler favors state needed by the current task/branch |
| Write-through cache | Event append before derived-state update | Durable truth precedes and can rebuild caches |

### Mechanism versus policy

- **Mechanism:** ledger/artifact persistence, typed IDs, page lookup, bounded
  rendering, and fault delivery.
- **Policy:** which records are pinned, promoted, demoted, invalidated, or
  evicted under the active token budget.

L3 semantic search is a policy aid, not address translation. It estimates which
stable page addresses may belong to the next working set. The direct resolver
and L4 provenance path remain the paging mechanism.

The MVP proves the mechanism with a deliberately simple fixed residency policy.
Later work may improve working-set estimation without changing durable identity
or the page-fault ABI.

### Limits of the analogy

WSMS pages are semantic and variable-sized; model attention is not physical RAM;
and a page fault is an explicit harness operation rather than a CPU trap. The
architecture borrows locality, identity, backing-store, pinning, invalidation,
and demand-paging principles without pretending the model is a literal process.

## 2. Scope and honesty boundary

The MVP is a local Go runtime. It implements deterministic event ingestion,
SQLite persistence, content-addressed artifacts, typed WSL state, safe lifecycle
boundaries, a structured-text renderer, and page-fault tools.

The first executable demo is a **mechanism proof**, not a benchmark result. It
may prove that state survives restart and that exact evidence is retrievable. It
must not claim that WSMS beats summaries, YAML, retrieval, or provider-native
compaction until matched forced-reset evaluation exists.

The MVP processes observers synchronously during `AfterEvent`. “Asynchronous
memory scheduler” describes the target separation of foreground and maintenance
work, not a current goroutine/queue implementation. Safe boundaries and replay
semantics are established first so later workers can be introduced without
changing the provider ABI.

## 3. System context

```mermaid
flowchart LR
    U["Developer"] --> H["Agent harness"]
    H --> M["Hosted or local model"]
    H --> T["Shell, repo, and tools"]
    H --> W["WSMS runtime"]
    T -->|"tool evidence"| W
    W -->|"working-state capsule"| H
    H -->|"page fault"| W
    W --> D[("Local SQLite ledger")]
    W --> A[("Content-addressed artifacts")]
```

WSMS does not own the model or tool sandbox. It receives events from the harness
and returns provider-compatible text/tools. Harness policy remains responsible
for authorizing file, shell, network, and external-system access.

## 4. Container and component view

```mermaid
flowchart TB
    subgraph Foreground["Foreground agent loop"]
        L["harness.Loop"]
        C["harness.Client"]
    end

    subgraph Runtime["WSMS session runtime"]
        S["harness.Session\ncomposition root"]
        E["ledger.AppendOnlyLedger"]
        O["observers.Dispatcher"]
        WSL["wsl.WorkingState"]
        SCH["scheduler.Scheduler"]
        MEM["memory.Hierarchy"]
        REN["renderer capsule ABI"]
        F["faults.Resolver / Tools"]
        ART["artifacts.Store"]
    end

    L --> S
    L --> C
    S --> E
    S --> ART
    S --> SCH
    SCH --> O
    O --> WSL
    SCH --> WSL
    SCH --> MEM
    SCH --> REN
    F --> WSL
    F --> MEM
    F --> E
    F --> ART
    S --> F
```

## 5. Package ownership

| Package | Owns | Does not own |
|---|---|---|
| `cmd/wsms` | Argument parsing, operator output, exit status | Runtime truth or business logic |
| `internal/config` | Local paths and budgets | Global mutable configuration |
| `internal/ledger` | Immutable event persistence and ordering | Derived WSL state |
| `internal/artifacts` | SHA-256 addressed immutable bytes | Semantic interpretation |
| `internal/observers` | Event-to-WSL deterministic derivation | Durable truth or arbitrary model inference |
| `internal/wsl` | Grammar, records, canonical serialization, lint, working-state store | Provider formatting |
| `internal/memory` | L0-L3 containers and residency metadata | L4 durability |
| `internal/scheduler` | Safe boundaries, residency scoring/policy, atomic update application, L1 selection, page materialization | Tool authorization |
| `internal/renderer` | Stable provider-facing capsule ABI and budgets | Event ingestion |
| `internal/faults` | Bounded page/raw-log/file-slice resolution | Access-control policy |
| `internal/harness` | Session composition, replay, foreground loop | Provider-specific SDK logic |
| `internal/demo` | Deterministic vertical-slice orchestration and assertions | Production runtime behavior |
| `research` | Frozen-data analysis and benchmark statistics | Authoritative ledger/WSL writes |

## 6. Architectural invariants

### A1 - Durable truth is append-only

Events and artifact bytes are immutable. L1-L3 state is a cache and may be
discarded. No capsule, WSL snapshot, or observer output is allowed to become the
only copy of exact evidence.

### A2 - Event identity is session-scoped

The durable event key is `(session_id, id)`. IDs such as `E0001` are readable
and monotonic within a session. Every lookup through an open ledger is scoped to
that ledger's session. A ledger handle rejects attempts to append or list a
different session rather than acting as a database-wide capability.

This resolves the scaffold mismatch where IDs were allocated per session but
`events.id` was globally unique.

### A3 - Replay equals live derivation

`OpenSession` replays existing events through the same ordered observer and
scheduler path as live events, except it performs no append. For a fixed event
stream and observer/WSL version:

```text
Replay(events) semantically equals LiveApply(events)
```

Replay reconstructs WSL state, L2 pages, known event refs, and observer ID
counters before new events may be appended.

### A3.1 - Append order is store-owned

Every event receives a durable session-local `append_seq` at commit. Replay and
listing order by that sequence. Caller-supplied timestamps remain useful event
metadata but cannot reorder history. Sequence recovery uses the durable maximum
append sequence, not row count or a parsed caller ID.

Within one open `Session`, durable append and derived mapping commit share one
serialization boundary. Observer ID allocation is checkpointed and restored if
the mapping batch is rejected, keeping live addresses identical to replay.

### A4 - Exact fields are immutable

Once non-empty, hard-constraint text and failure command/error fields cannot be
changed or erased by an update with the same record ID. A future correction
must create a new record plus an explicit invalidation/override relation.

### A4.1 - One event applies atomically

All WSL updates emitted for one event validate against a candidate state and
commit together. Derived pages are materialized only after the batch commits.
If any update fails, the pre-event derived state and hierarchy remain unchanged;
the durable event remains available for diagnosis and replay repair.

### A5 - Boundary-only injection

Context-visible changes occur only at:

- `AfterEvent`: digest one durable event into derived state.
- `BeforeTurn`: select and render the L1 capsule.
- `PageFault`: resolve explicitly requested evidence.

Background workers may prepare candidates between boundaries later, but cannot
mutate the prompt mid-turn.

### A6 - Scope gates residency

A record/page must match the active session/repo/task/branch/file scope before
ranking. Invalidated or all-stale pages cannot become resident without explicit
revalidation.

`internal/coherence` is the authoritative, per-session sidecar for this gate.
It replays the same durable events as WSL and binds each derived record/page to
its independently composable repo/task/branch/commit gates, paths, refs, status,
and keyed scope epochs. Repo, branch, commit, and path epochs draw from one
monotonic session clock and advance only at the narrowest affected scope;
returning to an older branch/commit therefore does not revive an old cache entry.
Reference eligibility is recursive and cycle-safe, so a page, decision, or avoid
record cannot outlive ineligible grounding evidence.

The flat set of current scope-epoch values is only a coarse early filter. A
path-specific page can share its scalar epoch with an ineligible page from a
different path, and an active index row can still depend transitively on a
revoked WSL/event address. Phase 7E therefore evaluates every active page
descriptor against `PageDescriptorEligible` and carries only the resulting
complete exact tuple allowlist into ranking.

Transitions use a candidate/commit protocol. The scheduler prepares a cloned
coherence candidate, derives observer updates, atomically notes the event and
applies the WSL batch, and only then installs the candidate. A rejected WSL
batch cannot advance scope epochs, stale revisions, allocator IDs, hierarchy
flags, or the known-event set. Page faults and capsule rendering serialize on
the same scheduler boundary.

`branch_change`, `commit_change`, and `file_renamed` make affected bindings
stale. `memory_revalidated` may restore only a stale target and must compare the
expected stale revision while citing preexisting eligible evidence. A terminal
`memory_invalidated` target cannot be revalidated. Its reason is one of
`superseded`, `user_rejected`, `source_deleted`, `policy_changed`, or
`security_revoked`.

Path invalidation is terminal for its known subtree and blocks later snapshots
or renames that overlap the revoked namespace. A materialized page carries the
maximum relevant monotonic scope generation. If its descriptor is older than an
eligible logical address, the resolver discards the resident body and
rematerializes from current WSL/L4 evidence instead of refreshing metadata in
place.

Logical invalidation suppresses L1-L3 but never deletes WSL/L4. Raw diagnostics
remain readable for ordinary staleness and the first three invalidation reasons.
`policy_changed` and `security_revoked` fail closed for raw access as well.
The resolver applies this authority check before looking in L2 or falling back
to WSL, so a stale cache body cannot bypass coherence.

### A7 - Misses are not guesses

An unresolved fault returns `PAGE_MISS`. Operational errors propagate as
errors. The resolver cannot synthesize plausible evidence.

### A8 - L3 is a disposable, reference-first cache

Lexical/vector search may return only eligible page references plus an
explanation. The runtime revalidates each reference and materializes its current
evidence from L4 before returning the fault response or permitting any future
L2/L1 admission. Deleting the entire L3 index cannot lose durable evidence or
break a known-ID fault.

Embedding namespaces include every representation-affecting input. Different
model revisions, dimensions, distance metrics, normalization, query
instructions, page schemas, or redaction versions are never searched together.

The Phase 7E dense ABI canonicalizes both stored and query vectors to finite,
unit-length, exactly float32-representable components before exact comparison or
sqlite-vec use inside the indexer. Search candidates carry an exact page tuple
covering session, ID, version, source digest, compiler version, and scope epoch,
plus the serving generation and source/page watermark. Those derivative
snapshots never replace the current L4/coherence authority check.

## 7. Live event flow

```mermaid
sequenceDiagram
    participant User
    participant Loop as harness.Loop
    participant Session as harness.Session
    participant Ledger
    participant Artifacts
    participant Scheduler
    participant Observers
    participant WSL
    participant Renderer
    participant Model as harness.Client

    User->>Loop: instruction
    Loop->>Session: IngestUser
    Session->>Artifacts: Put large payload if needed
    Session->>Ledger: Append event
    Ledger-->>Session: stored event with ID
    Session->>Scheduler: AfterEvent(stored event)
    Scheduler->>WSL: NoteEvent(event ID)
    Scheduler->>Observers: OnEvent
    Observers-->>Scheduler: ordered typed updates
    Scheduler->>WSL: lint then apply each update
    Loop->>Session: BeforeTurn
    Session->>Scheduler: BeforeTurn
    Scheduler->>Renderer: RenderCapsule(state, budget)
    Renderer-->>Loop: working-state capsule
    Loop->>Model: system capsule + user message
    Model-->>Loop: assistant response
    Loop->>Session: IngestAssistant
```

### Ordering

Within one session, append and update application are serial. `append_seq`, not
timestamp, defines the stream. An event becomes visible to a later turn only
after ledger append and its atomic observer-update batch succeeds. There is no
transaction spanning SQLite append and in-memory derivation. If derivation
fails, the event remains durable and reopen/replay must surface the same failure
rather than skip it.

A later production design should persist observer/version checkpoints and a
dead-letter state so one malformed event cannot permanently prevent inspection.

## 8. Restart and reset flow

```mermaid
sequenceDiagram
    participant CLI
    participant Session as OpenSession
    participant Ledger
    participant Scheduler
    participant WSL
    participant Renderer

    CLI->>Session: open(dataDir, sessionID)
    Session->>Ledger: Open database and load sequence
    Session->>Ledger: ListBySession(sessionID)
    loop ordered durable events
        Session->>Scheduler: AfterEvent(event) without append
        Scheduler->>Scheduler: prepare cloned coherence candidate
        Scheduler->>WSL: atomically note event, derive, lint, apply
        Scheduler->>Scheduler: commit coherence candidate and reconcile L1-L3
    end
    CLI->>Session: BeforeTurn
    Session->>Renderer: render reconstructed state
    Renderer-->>CLI: equivalent capsule
```

A **model context reset** discards the transcript but may leave the process
running. A **runtime-state restart** closes the session resources and discards
in-memory WSL and L0-L3 before a new composition root replays only durable
state. This does not require launching a second OS process; the proof boundary
is that no original runtime object survives. The demo exercises this stronger
state-reconstruction case rather than a capsule-only or transcript-only reset.

## 9. Page-fault flow

```mermaid
flowchart LR
    R["Fault request"] --> K{"known address?"}
    K -->|"yes"| D["Direct resolver"]
    K -->|"no, semantic intent"| H["L3 hybrid retriever"]
    H -->|"eligible page refs"| V["Current scope / validity check"]
    H -->|"no qualified candidate"| SM["SEMANTIC_PAGE_MISS"]
    V --> D
    D --> T{"target kind"}
    T -->|"page / WSL record"| L2["L2 lookup or materialization"]
    T -->|"raw_log"| RR["Failure/event raw ref"]
    RR --> A["Artifact lookup"]
    RR -->|"inline failure"| PE["Provenance event payload"]
    T -->|"file_slice"| FS["Authorized bounded file read"]
    L2 --> B["Budget trim"]
    A --> B
    PE --> B
    FS --> B
    B --> O["Plain-text response + exact IDs"]
```

`ReadPage(F1)` is valid even though `F1` is a WSL failure ID rather than a `P`
page ID: the resolver materializes record detail into L2. Future APIs should
distinguish record faults, page faults, and event faults in metadata while
retaining this convenient lookup behavior.

The first demo implements only the known-address side. The post-demo semantic
path discovers a possible address, then rejoins this same resolver. It never
defines a vector-only evidence path.

## 10. Durable data design

### 10.1 SQLite ledger

`events` stores the immutable envelope. Its primary key is composite:

```sql
PRIMARY KEY (session_id, id)
UNIQUE (session_id, append_seq)
```

Indexes support `(session_id, append_seq)` ordering. `Get`, list, and append
always bind the open ledger's session ID. A pre-0.1 scaffold database with the
old global primary key has no compatibility promise; the first tagged release
must introduce explicit schema versioning before further incompatible changes.

`wsl_snapshots` is reserved for replay acceleration. A snapshot is never
authoritative without an event watermark and observer/WSL version. Until those
fields exist, MVP replay uses the ledger.

### 10.2 Artifact store

Large immutable bytes live under a SHA-256-derived path. Ledger events retain
the hash, a bounded preview, and an `artifact:sha256:<hash>` reference.

The demo-slice store validates exact 64-digit hexadecimal hashes, derives paths
only from validated hashes, and recomputes SHA-256 on read. A referenced missing
or corrupt artifact is an error, not a page miss. Writes use unique temporary
files in the target directory and root-confined filesystem operations so
concurrent writers and symlink tricks cannot escape the backing store.

Future hardening:

- atomic temp-write plus rename;
- record size/content type;
- retention and garbage-collection policy based on ledger reachability;
- optional compression and encryption at rest.

### 10.3 Derived IDs

Observer-generated IDs (`T`, `C`, `F`, `D`, `A`, `P`) are deterministic under
ordered replay in MVP. This is sufficient for the demo but not a long-term
storage contract. Before observer algorithms evolve, persist derivation version
and stable event-to-record identity or store derived WSL updates as events.

For the first demo, `WorkingState` also holds a deterministic derivation index
from record ID to source event ID. Observer batches populate it, clones preserve
it, and replay reconstructs it. This supplies an inspectable L4 provenance path
without changing the WSL v0 text grammar.

## 11. WSL state architecture

WSL is an internal typed operational IR, not the external prompt format. Record
types are defined in `docs/wsl/v0.md`.

The store follows copy-on-read/copy-on-write behavior so callers cannot mutate
records behind the linter. `Apply` runs semantic validation before replacing a
record. `ApplyUpdates` validates an event's records against a cloned candidate
and swaps state only after the full batch succeeds. Canonical serialization is
stable and round-trips escaped values.

Required MVP correctness properties:

- exactly one blank line between serialized records;
- quoted strings unescape and re-escape without growth;
- hard constraint and failure exact fields cannot be changed or erased;
- an event ref exists only if `NoteEvent` recorded it;
- contradiction detection handles ordinary polite negation such as
  “please do not rewrite transport layer.”

## 12. Scheduler and memory tiers

| Tier | Name | MVP representation | Policy |
|---|---|---|---|
| L0 | Turn scratch | map | Ephemeral, cleared per turn |
| L1 | Active capsule | rendered string | Always resident; strict budget; hard constraints pinned |
| L2 | Hot pages | in-memory map | Immediate fault hits; failure details materialized here |
| L3 | Warm memory | separate disposable `index/warm.db` | Phase 7E hybrid FTS/vector address discovery; derivative and rebuildable |
| L4 | Cold truth | SQLite + artifacts | Durable, exact, never preloaded wholesale into prompts |

The scheduler owns the preparatory score function because scoring is residency
policy; `internal/memory` owns only page/tier storage mechanism. Until scope
filtering, candidate enumeration, eviction, and selection actually use that
score, the runtime must not claim ranked residency. MVP `BeforeTurn` selects
directly from typed WSL categories in a fixed priority order.

### L3/L2 component view through the Phase 7F mechanism boundary

The normative target is detailed in `docs/l3-warm-memory.md`.

```mermaid
flowchart TB
    subgraph Truth["Authoritative L4"]
        LED["Append-only ledger"]
        ART["Content-addressed artifacts"]
    end

    subgraph Derivation["Deterministic derivation"]
        PC["Versioned page compiler"]
        PG["Typed semantic pages + exact refs"]
    end

    subgraph Index["Disposable L3 generation"]
        FTS["SQLite FTS5 / BM25"]
        VEC["sqlite-vec cosine KNN"]
        META["Active descriptors + exact tuple authority"]
    end

    subgraph Paging["Semantic paging"]
        Q["Typed QueryIntent"]
        RET["Filter, RRF, rerank, diversify, abstain"]
        MAT["Exact page materializer"]
    end

    subgraph Residency["Bounded Phase 7F residency"]
        L2["L2 cold / hot / pinned bodies"]
        GHOST["Bodyless exact-tuple ghosts"]
        SHADOW["Bodyless semantic-shadow episodes"]
        RSTAT["Bounded estimator metrics"]
        L1["Independent L1 scheduler"]
    end

    LED --> PC
    ART --> PC
    PC --> PG
    PG --> FTS
    PG --> VEC
    PG --> META
    Q --> FTS
    Q --> VEC
    META --> RET
    FTS --> RET
    VEC --> RET
    RET -->|"selected page refs"| MAT
    LED --> MAT
    ART --> MAT
    MAT -->|"exact demand after final freshness"| L2
    RET -.->|"non-selected, non-suppressed exact tuples"| SHADOW
    L2 -->|"reclaim body; retain identity"| GHOST
    SHADOW -.->|"episode outcome"| RSTAT
    L2 -.->|"later exact real use"| RSTAT
    L2 -.->|"separate scheduler choice"| L1
```

The dotted shadow path is telemetry, not admission: it cannot materialize a
body, set a reference bit, pin, or change L1. Actual speculative L2 admission
is disabled. A selected semantic page follows the solid path only after exact
L4 materialization and the final attempt freshness checks.

Target package boundaries, added only when their phase starts:

| Package | Owns | Must not own |
|---|---|---|
| `internal/pages` | Logical page schema, compiler, versions, source digests | Ledger persistence or vector client |
| `internal/embedder` | Namespace ABI, role-specific payloads, admission, supervision, local client | Page truth, vector residency, or capsule admission |
| `internal/indexer` | Page/FTS/vector projections, descriptor snapshots, exact tuple joins, generations, watermarks, rebuild | Embedding calls, capsule admission, or evidence authority |
| `internal/harness` | Best-effort page compilation, current-coherence allowlist construction, asynchronous embedding writeback | Making L3 failure roll back L4 truth |
| `internal/retrieval` | Query intent, hard filters, hybrid fusion, rerank, diversity, abstention, explanations | L4 bytes or provider-specific clients |
| `internal/memory` | Bounded L2 cold/hot/pinned bodies, bodyless ghosts/shadows, reference/use accounting, snapshots/traces | Search ranking, L4 authority, or L1 admission policy |
| `internal/faults` | Current-ref validation and exact materialization | Nearest-neighbor ranking |

Backend-native types remain behind `WarmIndex`; embedding runtime types remain
behind `Embedder`. This lets SQLite, the exact oracle, and optional Qdrant share
one behavior contract.

### Semantic-fault sequence (Phase 7E implemented mechanism)

```mermaid
sequenceDiagram
    participant Caller
    participant Harness
    participant Embedder
    participant State as Current scope/invalidation state
    participant Retrieval
    participant Index as Disposable WarmIndex
    participant Faults as Exact materializer
    participant L4 as Exact WSL/ledger evidence

    Caller->>Harness: SemanticSearch(text)
    opt dense is configured
        Harness->>Embedder: EmbedQuery(canonical query)
        Embedder-->>Harness: namespaced vector or safe unavailable category
    end
    Harness->>State: capture revision, source sequence, coarse epochs
    Harness->>Index: check IndexErr, health, generation, source/page watermark
    Harness->>Index: ActivePageSnapshot(session)
    Index->>Index: strict JSON + shared descriptor validation
    Index-->>Harness: every active descriptor, no search prose
    loop each descriptor
        Harness->>State: check scope/path + transitive WSL/event refs
        State-->>Harness: admit or suppress exact PageTuple
    end
    Harness->>Index: recheck generation + source/page watermark
    Harness->>State: recheck revision + source sequence + IndexErr
    Harness->>Harness: seal complete exact tuple allowlist
    Harness->>Retrieval: ResolveSemantic(QueryIntent, vector, budgets)
    par Lexical channel
        Retrieval->>Index: tuple join, then SearchLexical LIMIT
    and Dense channel
        Retrieval->>Index: tuple rowid-IN, then vec0 KNN
    end
    Index-->>Retrieval: ranks + tuple/generation/watermark snapshot
    Retrieval->>Retrieval: tuple checks + RRF + provisional policy/diversity/thresholds
    alt no qualified candidate
        Retrieval-->>Harness: semantic miss or operational error
    else candidate selected
        Retrieval->>State: recheck current status/scope/source digest
        Retrieval->>Faults: materialize exact refs under cumulative budget
        Faults->>L4: resolve exact refs
        L4-->>Faults: current evidence
        Faults-->>Retrieval: bounded exact evidence
        Retrieval-->>Harness: selected refs + exact evidence + safe trace
    end
    Harness->>Index: recheck IndexErr, generation, source/page watermark
    Harness->>State: recheck coherence revision + source sequence
    Harness-->>Caller: result without L1 mutation
```

The query embedding is prepared outside the append lock. If it fails, lexical
search may still return a categorically degraded hit. If an operational channel
failure leaves no surviving candidate, the call returns `ErrIndexUnavailable`,
not `SEMANTIC_PAGE_MISS`; a miss is reserved for an operationally valid search
that abstains. A coherence change is retried once and then becomes a typed scope
abstention. A known `IndexErr`, lagging/changing source watermark, generation
change, or materializer error fails operationally before a weak miss can hide
it. Current L1 state and all known-ID L4 faults remain independent.

The per-attempt page table is complete or unavailable. A healthy complete-empty
allowlist is a genuine semantic miss. An unavailable snapshot, more than
`indexer.MaxAuthoritySnapshotPages` (4,096) active descriptors, or more than
4 MiB of descriptor/encoded-eligibility payload is an explicit local
capability/SLO failure reported as `ErrIndexUnavailable`. There is no truncated
allowlist, pagination across inconsistent authority snapshots, or unfiltered
fallback.

Snapshot validation reuses `pages.ValidateAuthorityDescriptor`, the same
non-prose contract used when page mutations are admitted. It checks kind/trust
compatibility plus scope, repo/task authority, branch/commit, paths, and refs;
refs/path arrays are decoded as strict single JSON values. Malformed active
metadata returns the typed `ErrAuthorityDescriptorCorrupt`, which the semantic
fault exposes as operational index failure before allowlisting. It cannot be
silently filtered into a miss, even when malformed rows rank ahead of valid
evidence.

Phase 7E materializes a bounded fault response and still does not inject L1.
The Phase 7F layer may demand-admit selected exact evidence only after the final
freshness check, and may observe non-selected/non-suppressed exact tuples as
bodyless shadow metadata. The direct `PageFault` path for known WSL records is
unchanged, bypasses both retrieval channels, and updates a byte/authority-exact
L2 hit atomically.

Compiler-derived `wp_*` IDs do not use that ID-only fallback. Their authority
requires the descriptor tuple plus transitive refs, which the semantic attempt
already seals and rechecks. Repeated semantic selection therefore revalidates
and rematerializes L4, then uses the privileged authoritative-demand operation
to replace an older resident tuple. Direct `ReadPage(wp_*)` remains disabled
until a descriptor-backed direct page table can preserve those same checks.

### Phase 7F residency overlay

```mermaid
stateDiagram-v2
    [*] --> Cold: first exact demand / ref=1
    Cold --> Hot: later actual use
    Cold --> Ghost: unreferenced reclaim
    Hot --> Cold: unreferenced demotion
    Ghost --> Hot: exact-tuple refault
    Cold --> Pinned: explicit anchor policy
    Hot --> Pinned: explicit anchor policy
    Pinned --> Hot: explicit unpin
    Cold --> [*]: invalidation shootdown
    Hot --> [*]: invalidation shootdown
    Pinned --> [*]: invalidation shootdown
    Ghost --> [*]: invalidation or bounded expiry
```

The default policy bounds resident pages to 64/512 KiB logical retained bytes,
including a 16-page/128-KiB pinned subset, with 64 KiB per page. Bodyless ghosts
are bounded to 64/32 KiB; semantic-shadow episodes to 256/64 KiB over 64 later
real uses; categorical residency trace to 512 entries. The bounds count all
retained strings/list values. Pin overflow is transactional and pins remain
subject to authority invalidation.

Active-task and hard-constraint anchors are pinned explicitly. Similarity
cannot pin or choose a reclaim victim. Embedding namespace attributes shadow
estimator provenance; the exact six-field L4 tuple remains page identity.
Direct invalidation synchronously removes the matching resident identity.
Because ghosts and semantic shadows deliberately retain no dependency refs,
every broader residency-changing coherence event conservatively purges all
ghosts and censors all pending shadows. L1 construction remains a separate
safe-boundary scheduler decision, and automatic L1 admission remains disabled
through Phase 10.

### Indexing and consistency sequence

```mermaid
sequenceDiagram
    participant Foreground
    participant Ledger
    participant Compiler as Page compiler
    participant Index as Disposable L3
    participant Worker as Dense writeback worker
    participant Embedder

    Foreground->>Ledger: append event and commit L4 truth
    Ledger-->>Foreground: durable append sequence
    Foreground->>Compiler: best-effort current event or contiguous catch-up
    Compiler->>Compiler: deterministic page mutations + source digests
    Compiler->>Index: transactionally write pages + FTS + source watermark
    Compiler-->>Foreground: never overturn committed L4 on L3 error
    Compiler->>Worker: non-blocking wake
    Worker->>Index: read missing current page tuples
    Worker->>Embedder: self-check + bounded document batch
    Embedder-->>Worker: namespaced document vectors or categorized fault
    alt admitted current tuple
        Worker->>Index: canonicalize unit-float32 vector
        Worker->>Index: CAS(version,digest,compiler,namespace) + upsert vector
    else permanently denied payload
        Worker->>Index: CAS tuple + evict vector + mark lexical-only
    else transient service fault
        Worker->>Worker: bounded cancelable backoff and retry
    end
```

The source watermark covers deterministic page and FTS application, not vector
completion. Dense residency is tracked separately as a page tuple plus
embedding namespace; missing tuples form the writeback backlog. Startup
reconciliation replays page/FTS state from the L4 high water, then the worker
re-derives missing vectors. Page mutations are idempotent by page ID/version,
source digest, and compiler version. Vector writes compare-and-swap that tuple
and the generation namespace, so slow inference cannot be relabeled as a newer
page. Query-time validation still suppresses a page whose invalidation has
committed to L4 but not yet reached L3.

Admission denial is not a transient service failure. L3 records a disposable,
tuple-scoped lexical-only suppression, atomically removes any vector for that
page, and retries only after a new page tuple invalidates the suppression.
Transient backend/self-check faults use bounded cancelable backoff. Terminal
ABI, namespace, or malformed-vector faults park until a new wake or reopen.

### Hybrid policy and residency

Candidate generation runs FTS5 BM25 and dense cosine search concurrently over
one `SearchQuery` filter universe. Session, status, trust, repo, task, branch,
commit, kind, current scope epoch, path, and exclusions remain coarse filters.
The complete exact tuple allowlist is additionally joined before the FTS5
`LIMIT` and inside sqlite-vec's `rowid IN` eligibility subquery before KNN.
This closes path-epoch alias and transitive-ref starvation that the flat epoch
set cannot detect. Both channels must report coherent page tuples, serving
generation, and source/page watermark snapshots before their ranks can be
fused.

Reciprocal rank fusion (`rrf/v1`, default `k = 60`) combines channel order
without comparing BM25 values with cosine distances. The checked-in
`working-set/v1-provisional` policy adds named, bounded contributions for
repo/task/branch/commit/path affinity, source trust, salience, verification,
and last-failure overlap. It then applies per-kind and per-source caps, token-set
Jaccard near-duplicate suppression, maximum dense distance, and a minimum final
score. Hard scope/validity conditions remain gates, never weights. All values
are safety defaults: real Qwen execution and held-out calibration have not run,
so no retrieval-quality improvement is claimed.

The normal semantic fault materializes at most 1-3 candidates under cumulative
attempt/page/ref/byte/token budgets. Phase 7F now adds bounded
CLOCK-Pro/2Q-inspired cold/hot/pinned residency, exact bodyless ghosts, and
metadata-only shadow accounting subject to the root final verification matrix.
Similarity still neither pins a page nor changes L1. Actual speculative L2
body admission remains disabled pending a pinned real-Qwen run and held-out
usefulness/negative-transfer evidence.

The mechanism is inspired by the [CLOCK-Pro USENIX
paper](https://www.usenix.org/conference/2005-usenix-annual-technical-conference/clock-pro-effective-improvement-clock-replacement)
and the [2Q VLDB paper](https://www.vldb.org/conf/1994/P439.PDF). Linux's
[`workingset.c`](https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/plain/mm/workingset.c)
provides the refault-distance/shadow-entry precedent, while [cgroup v2
`memory.stat`](https://docs.kernel.org/admin-guide/cgroup-v2.html) provides the
refault/activation/promotion/demotion observability precedent. WSMS-specific
tuple authority, byte limits, pin quotas, and semantic estimator attribution
remain explicit local design choices rather than claims made by those sources.

### Backend decision

| Backend | Role | Decision |
|---|---|---|
| Exact cosine reference | Tiny deterministic fixtures and ANN oracle | Required |
| SQLite FTS5 | Exact lexical/code/error retrieval | Required first L3 backend |
| `sqlite-vec` via pinned modernc SQLite | Local dense KNN in separate `warm.db` | Verified development backend; config-gated and derivative |
| Qdrant with official Go client | High-scale/concurrent filtered dense ANN, initially paired with SQLite FTS5 | Optional only after measured SQLite SLO miss |
| LanceDB | Potential Arrow/offline adapter | Deferred, not default |

The embedded database is stored separately from `ledger.db`. Removing it is a
supported recovery action. Qdrant, if introduced, remains a derived process;
its snapshot is never the disaster-recovery source.

The current sqlite-vec adapter uses the v0.1.9 `vec0` brute-force KNN path; its
corpus-size, latency, CPU, and memory envelope still requires measurement on
target machines. PageID provides a stable tie-break only within the top-k set
returned by vec0. If more than `k` eligible rows have the same boundary
distance, boundary membership remains backend-defined because completing the
entire tie would violate the bounded-query contract.

### Future general observer async model

```mermaid
flowchart LR
    E["Committed event"] --> Q["Bounded per-session queue"]
    Q --> O["Observer workers"]
    O --> C["Ordered commit coordinator"]
    C --> W["Derived WSL/page candidates"]
    W --> B["Next safe boundary"]
```

Requirements before enabling this path:

- per-session ordering and idempotency;
- bounded queues and backpressure;
- cancellation and shutdown drain policy;
- observable lag/watermarks;
- deterministic ordered commit despite parallel extraction;
- replay from the last committed event watermark.

## 13. Provider boundary

`harness.Client` is the provider-neutral contract:

```go
type Client interface {
    Chat(ctx context.Context, messages []Message) (string, error)
}
```

The loop sends the WSMS capsule as a system message and the current instruction
as a user message. Provider adapters belong outside core memory packages and
must map cancellation, timeouts, tool calls, and provider compaction explicitly.

WSMS complements provider-native compaction. Provider compaction manages the
model's conversation window; WSMS maintains inspectable operational state and
exact local evidence. Neither is assumed to replace the other.

## 14. Error model

| Boundary | Failure behavior |
|---|---|
| Ledger open/append/list | Return typed ledger error; do not continue as if durable |
| Artifact put/get | Return error; do not store a fake ref |
| Observer derivation | Return error with event context; durable event remains |
| WSL lint/apply | Reject update atomically |
| Capsule render | Deterministic local operation; pin hard constraints |
| Page target absent | Return `PAGE_MISS` |
| Malformed/missing/corrupt referenced artifact | Return error, not `PAGE_MISS` |
| Malformed persisted timestamp/JSON | Return typed ledger/replay error |
| Client call | Return error and preserve capsule for diagnosis |

The demo-slice resolver distinguishes an absent logical target from operational
failure. The authorized workspace policy for `file_slice` remains an embedding-
harness responsibility and is outside the first page/raw-log demo.

## 15. Security boundaries

```mermaid
flowchart LR
    X["Untrusted user/tool/model text"] --> E["Quoted event data"]
    E --> O["Conservative observers"]
    O --> V["WSL validation + provenance"]
    V --> C["Inspectable capsule"]
    C --> M["Model"]
```

Key controls:

- No raw tool output is promoted to system authority solely because it contains
  imperative language.
- Every derived fact retains scope and evidence identity.
- Hard constraints come from recognized trusted sources.
- File-slice authorization is enforced by the embedding harness/sandbox.
- Session-scoped SQL lookups prevent cross-session leakage.
- Future shared/team memory requires an explicit ACL and deletion/export model.
- L3 stores typed, bounded search text and exact refs, not unrestricted raw
  artifacts or transcripts.
- Scope/ACL, trust, status, and invalidation are checked before ranking and
  again before exact page materialization.
- Retrieval preserves source trust and keeps imperative tool/repo text quoted
  as data; nearest-neighbor rank never promotes it to policy.
- Hosted embedding is off by default and requires redaction, explicit provider
  configuration, deadlines, cost/error telemetry, and a distinct namespace.

## 16. Observability

The MVP demo prints enough evidence for a human to audit the vertical slice:

- event IDs and derived record IDs;
- artifact SHA-256 and offload status;
- capsule approximate token count;
- explicit restart boundary;
- page and raw-log hit status;
- critical-evidence equality checks;
- final pass/fail marker and data directory.

Production metrics should include observer lag, invalid-update count, replay
duration, capsule size/budget overflow, resident-page count, fault hit/miss,
artifact bytes, stale-page suppression, and per-event derivation time.

Phase 7E currently emits a bounded, text-free trace containing active filter
labels, channel candidate counts/ranks, RRF and named policy contributions,
categorical degradation/suppression/abstention, selected page IDs, and cumulative
materialization tokens. Candidate metadata carries tuple, serving generation,
and source/page watermark for validation; the harness checks `IndexErr`,
coherence revision, source sequence, generation, and both watermark components
around snapshot construction and again after resolution.

Future L3 metrics additionally include compiler/index watermark lag,
pages/bytes by namespace and kind, lexical/dense channel latency, semantic
hit/miss/error/abstention, exact-reference precision, stale/wrong-scope
suppression, materialization latency, and tokens per useful retrieved page.
The separate Phase 7F residency snapshot/trace now reports bounded resident,
pinned, ghost, and shadow counts/bytes plus demand/use, promotion/demotion,
ghost/refault/actionable-thrash, rejection, shootdown, and shadow useful/unused
categories. These are mechanism counters pending root final verification, not
a measured useful-prefetch claim; actual prefetch counters remain disabled.

## 17. Deployment model

MVP is a single local process and local data directory:

```text
<data-dir>/ledger.db
<data-dir>/ledger.db-wal
<data-dir>/artifacts/<sha256-derived path>
```

The post-demo embedded L3 adds only disposable state:

```text
<data-dir>/index/warm.db
<data-dir>/index/rebuild.lock
```

No network service is required. The CLI and an embedding harness both call the
same Go packages. A future daemon must not create a second truth layer; it
should expose the same session/ledger contracts over a local authenticated IPC
boundary.

All `Index` handles in the MVP share a process-local lease keyed by the
evaluated physical index directory. Open/recovery/rebuild take the exclusive
side; reads and ordinary mutations take the shared side and stale handles
rebind by generation. `rebuild.lock` also rejects a second rebuild owner, but
it does not serialize ordinary writers in another OS process. A daemon or
multi-process deployment therefore requires one filesystem-wide advisory
operation lock before it is supported.

## 18. Architectural decision record

| Decision | Rationale | Consequence |
|---|---|---|
| Go owns runtime truth | Existing scaffold and predictable systems behavior | Python/Rust remain non-authoritative |
| SQLite + files | Local, inspectable, transactional enough for MVP | Schema migration must be introduced before releases |
| WSL internal, structured text external | Provider compatibility and inspectability | Renderer ABI needs compatibility tests |
| Synchronous observers first | Deterministic replay and smallest complete proof | Async claims deferred |
| Composite session/event identity | Matches per-session monotonic IDs | SQL lookups and schema must include session |
| Full ledger replay in MVP | Correctness before snapshot optimization | Open cost grows with event count |
| Deterministic no-key demo | Reproducible and aligned with repo policy | Does not measure real model quality |
| Strong-baseline evaluation later | Prevent format-driven conclusions | Product claim remains a hypothesis until benchmarked |
| L3 is a separate derivative index | Cache deletion/rebuild cannot damage truth | Search may lag or miss while L4 stays available |
| Hybrid FTS5 + dense retrieval | Code identifiers and paraphrases need different channels | Fusion/rerank/abstention require evaluation |
| SQLite first, Qdrant by measurement | Preserve local one-process simplicity until ANN scale is real | A compatibility spike gates pre-1.0 sqlite-vec |
| Reference local embedding profile | Private, reproducible evaluation without a hosted key | Model/profile remains replaceable and namespaced |
| WSMS-owned local embedding protocol | Verify the exact namespace, profile, input order, and digests instead of trusting a generic endpoint | A small bridge is required for TEI/other serving stacks; redirects, proxies, and non-loopback dials fail closed |
| Separate page watermark and vector residency | L4/FTS progress must not wait for inference | Missing tuples form a retryable derivative writeback queue |
| Vector tuple compare-and-swap | Slow inference must not complete into a newer logical page | Stale results are discarded and re-derived from the current tuple |
| Tuple-scoped lexical-only suppression | Secret/admission-denied pages must not leak or block later backlog | Suppression evicts dense residency and expires only when the page tuple changes |
| Reference-first semantic faults | Approximation discovers addresses; resolver proves evidence | Vector results never enter L1 directly |
| Canonical unit-float32 vector ABI | Exact oracle, stored shadow, and sqlite-vec must compare the same direction | Non-finite, zero, wrong-dimension, or non-representable vectors fail closed |
| Pre-limit eligibility for both channels | Stale or wrong-scope chaff must not consume bounded top-k slots | FTS SQL and vec0 rowid-IN KNN apply the same authority universe |
| Complete per-attempt page authority | Flat epochs can alias across paths and miss transitive invalidation | Descriptor eligibility produces an exact tuple allowlist before either channel limit |
| Bounded authority snapshot | An incomplete allowlist is not proof of authority | More than 4,096 active pages or 4 MiB of descriptor/eligibility payload fails operationally without truncation or fallback |
| Shared descriptor validation | Stored active metadata must obey the same authority schema as admitted pages | Malformed scope/authority/path/ref/kind/trust data is typed corruption before allowlisting, never a miss |
| Provisional Phase 7E policy | Mechanism safety can be tested before real-model quality is known | Thresholds and weights cannot be described as calibrated until held-out evaluation |
| Bounded vec0 tie semantics | sqlite-vec chooses top-k membership before Go can apply PageID order | PageID stabilizes returned ties, not equal-distance membership beyond the k boundary |
| Bounded Unix-style L2 mechanism | Exact bodies are reconstructible, so residency can use second chance, demotion, pins, and bodyless ghosts | All resident/metadata stores have entry and logical-byte caps; similarity is not lifetime policy |
| Semantic shadow before prefetch | Estimator utility must be measured without self-validating admission | Non-selected exact tuples record metadata only; selected demand and actual use are excluded from speculative prediction |
| Invalidation as shootdown | Stale metadata must not guide refault or usefulness | Matching residents are removed; dependency-free ghosts are purged and pending shadows censored on broad residency transitions, including pins |

## 19. Evolution path

1. **Durable vertical slice:** session identity, replay, exact evidence,
   deterministic demo.
2. **Correctness hardening:** WSL canonicalization, immutability, reference and
   contradiction validation, fault error taxonomy.
3. **Operational state:** task/decision/avoid/next observers and explicit scope.
4. **Coherence:** branch/file invalidation and revalidation.
5. **L3 foundations:** semantic page compiler, exact oracle, separate index
   generations/watermarks, and FTS-only semantic faults.
6. **Hybrid retrieval:** sqlite-vec compatibility proof, namespaced local
   embedder, RRF/rerank/diversity/abstention, and reference-first materialization.
7. **Residency:** bounded L2 cold/hot/pinned policy, bodyless ghosts, budget
   telemetry, and metadata-only semantic shadow; actual speculative prefetch
   remains gated.
8. **Async maintenance:** ordered, bounded background indexing/observer workers.
9. **Adapters:** hosted/local model integrations behind `harness.Client` and
   optional hosted embeddings behind `Embedder`.
10. **Evaluation:** forced-reset benchmark against strong baselines; enable
    automatic semantic admission only if held-out outcomes improve.
11. **Only if measured:** Qdrant/Rust sidecar for ANN scale or local latent/KV
    experiments. L3 details and gates are normative in
    `docs/l3-warm-memory.md`.
