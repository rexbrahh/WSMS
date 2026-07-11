# WSMS L3 Warm Memory and Semantic Paging

**Status:** Normative architecture; implementation is complete through the explicit Phase 7E/L3-4 hybrid-fault mechanism
**Version:** 0.1  
**Decision date:** 2026-07-10  
**Owner:** Go runtime, with optional local inference and ANN sidecars

## 1. Decision summary

WSMS should use vector retrieval, but it must use it in the same disciplined
way that Unix uses predictive memory-management heuristics: to estimate the
next useful working set, never to replace durable storage, stable addressing,
or correctness checks.

The complete L3 design is:

1. The append-only ledger and content-addressed artifacts remain L4 truth.
2. Deterministic observers compile ledger evidence into small, typed semantic
   pages. Pages contain searchable summaries and exact references, not a second
   transcript.
3. A separate, disposable L3 index combines SQLite FTS5 lexical search with
   dense-vector search. A complete descriptor-derived exact tuple allowlist and
   the remaining metadata filters run before either channel chooses top-k.
4. Hybrid retrieval returns page references. It does not return authoritative
   prose. The resolver follows those references to exact L4 evidence before a
   page enters the fault response or any future L2/L1 state.
5. Known identifiers always use direct lookup and bypass embeddings. Vector
   retrieval exists for semantic faults, such as “what did we learn about this
   deadlock?”, when the caller does not know an identifier.
6. The default backend is a local, separate SQLite database using FTS5 and the
   `sqlite-vec` extension distributed with the pinned `modernc.org/sqlite`
   dependency. The pinned v0.1.9 compatibility slice is implemented, remains
   config-gated, and still requires target-scale resource measurement because
   `sqlite-vec` is pre-1.0.
7. An exact, brute-force cosine backend is retained as a small reference
   implementation for tests and evaluation.
8. Qdrant is the optional ANN scale-out backend only after measured corpus or
   latency pressure justifies another process. It implements the same Go
   interface and remains reconstructible from L4.
9. The default private embedding profile is a local
   `Qwen/Qwen3-Embedding-0.6B` adapter, namespaced with its complete model and
   preprocessing identity. Hosted embedding providers are explicit opt-in.
10. The explicit hybrid semantic-fault mechanism is implemented with a
    provisional safety profile. It earns real-model enablement, prefetch, or L1
    promotion only through forced-reset evaluation against no-L3, FTS-only,
    dense-only, and strong structured-memory baselines.

The strongest invariant is:

> Deleting every vector, lexical index row, access statistic, and cached page
> may reduce recall and increase latency, but it must not lose session truth or
> make an exact identifier unresolvable.

## 2. Why this is the Unix-inspired answer

A vector database is not the semantic equivalent of swap, a page table, or a
filesystem. Treating it as any of those would make approximate similarity a
correctness boundary. Instead, WSMS assigns it the narrower role of
working-set estimator and readahead index.

| Unix memory-management concept | WSMS mechanism | Vector role |
|---|---|---|
| Durable executable/file/swap backing | L4 ledger and artifacts | None; vectors cannot replace backing evidence |
| Virtual address | Stable record, event, artifact, or page ID | None for known addresses |
| Page-table lookup | Direct resolver by stable ID | Vector search is bypassed |
| Major page fault | Needed evidence is not resident | Semantic search may discover the address when it is unknown |
| Page cache | L2 decoded, validated pages | Search result may nominate a page for admission |
| Resident set | L1 rendered working-state capsule | Vectors cannot inject content directly |
| Working-set estimation | Scope, access, recency, frequency, salience | Similarity is one estimator among several |
| Readahead | Prefetch likely-nearby page references | Dense neighbors may be prefetched, with usefulness measured |
| Page replacement | CLOCK-Pro/2Q-inspired hot/cold policy | Retrieval score is not an eviction score |
| Dirty-page writeback | Not used for truth | Derived pages never write approximate content back as evidence |
| TLB/cache invalidation | Scope epoch and page invalidation | Stale candidates are rejected even if highly similar |

This preserves the useful parts of the analogy: bounded residency, stable
addresses, demand paging, locality, prefetch, invalidation, and a strict split
between mechanism and policy.

## 3. Goals and non-goals

### 3.1 Goals

- Find relevant older operational knowledge when neither the agent nor harness
  knows its stable ID.
- Preserve exact commands, errors, paths, decisions, constraints, and
  provenance after retrieval.
- Prefer the active session, repo, task, branch, commit lineage, and file scope
  before considering semantic similarity.
- Degrade cleanly to lexical search or direct faults when embedding or ANN
  services are unavailable.
- Make the entire index reproducible, inspectable, versioned, and disposable.
- Measure negative transfer, stale revival, useful prefetch, latency, and token
  cost rather than reporting retrieval hit rate alone.
- Keep provider-facing memory compact: retrieve references first, then page in
  only the minimum exact evidence needed for the current turn.

### 3.2 Non-goals

- Storing the authoritative event ledger in a vector database.
- Embedding raw transcripts, entire repositories, secrets, or unrestricted raw
  command logs by default.
- Using nearest-neighbor output as a hard constraint, decision, or verified
  fact without provenance and deterministic validation.
- Replacing direct page faults by ID.
- Adding a remote service to the first mechanism demo.
- Claiming that embeddings improve task outcomes before controlled evaluation.
- Making a Python process the owner of ledger truth, WSL, or capsule rendering.
- Building a general-purpose personal knowledge base or cross-user memory
  product in this phase.

## 4. Safety and correctness invariants

The following are mandatory across every L3 backend:

### L3-I1 - Derivative-only storage

Every indexed row is reproducible from a specific ledger sequence, observer
version, page-compiler version, and embedding namespace. L3 never contains the
only copy of evidence.

### L3-I2 - Direct addressing wins

If a request contains a valid record, page, event, artifact, or file-slice
address, the runtime resolves it directly. It must not substitute a “similar”
target.

### L3-I3 - Scope before score

Session isolation, tenant/ACL policy, repository identity, active task policy,
branch compatibility, trust requirements, and invalidation state are hard
gates. Dense or lexical relevance cannot override them. Phase 7E additionally
requires a complete per-attempt allowlist of exact page tuples derived from
current descriptors, paths, and transitive logical refs before either channel
may choose top-k.

### L3-I4 - Reference-first retrieval

Search returns bounded candidate metadata and exact references. A candidate's
content reaches the model only after page materialization and normal WSL
validation.

### L3-I5 - No trust promotion

Retrieval does not change source trust. Model-authored or tool-authored text
remains untrusted data even if it is the nearest vector match.

### L3-I6 - Namespace isolation

Embeddings with different model revisions, dimensions, distance metrics,
normalization, query instructions, or page schemas are never compared in one
namespace.

### L3-I7 - Visible abstention

If no candidate clears the active relevance and validity thresholds, the
retriever returns `SEMANTIC_PAGE_MISS`. The Phase 7E thresholds are named and
provisional, not calibrated. An unavailable/failed operational channel plus no
surviving candidate is an index error rather than a semantic miss. Deliberately
dense-disabled lexical-only mode is not an operational failure. A complete
empty authority allowlist is likewise a valid empty universe; an unavailable or
incomplete allowlist is operational failure.

### L3-I8 - Eventual index consistency cannot weaken truth

Index lag must not block ledger appends, alter replay, or make a stale page
authoritative. The current explicit semantic-fault path checks `IndexErr`, index
health/generation, both source/page watermark components, event high-water, and
coherence revision around authority-snapshot construction and again after
resolution. Known lag or projection change fails operationally; query-time
exact post-validation still rejects stale candidates.

### L3-I9 - Bounded materialization

Search, page-in, and rendering each accept explicit candidate, byte, and token
budgets. Large artifacts remain outside the capsule.

### L3-I10 - Rebuild equivalence

Given the same ledger, artifact store, compiler version, and embedding profile,
a rebuild produces the same logical page identities and searchable text. PageID
stabilizes equal-distance order only within a backend's returned bounded set.
For sqlite-vec, more than `k` eligible rows tied at the KNN boundary have
backend-defined membership; Phase 7E does not claim otherwise.

### L3-I11 - Authority snapshots are complete or unavailable

The local Phase 7E snapshot supports at most
`indexer.MaxAuthoritySnapshotPages` (4,096) active page descriptors and 4 MiB
for descriptor or encoded-eligibility payload. Crossing either bound is a
visible capability/SLO failure. WSMS never truncates the allowlist, paginates it
across changing authority, or falls back to an unfiltered semantic search.
Malformed active descriptor metadata is likewise typed operational corruption,
not a page that may be silently suppressed into a miss.

## 5. What gets indexed

WSMS indexes semantic memory pages, not arbitrary event chunks. A page is a
small, single-topic unit that answers one likely future operational question
and points back to exact evidence.

### 5.1 Initial page kinds

| Kind | Question answered | Typical exact refs |
|---|---|---|
| `failure_episode` | What failed, with what signature, and what was tried? | failure, command event, raw artifact, avoid |
| `decision` | What was chosen and why? | decision, constraints, rejected alternative |
| `constraint` | What requirement governs this scope? | exact user event and constraint record |
| `task_checkpoint` | Where did this task stop and what is next? | task, next action, last verified state |
| `known_good_command` | Which exact command worked in this environment? | tool event, exit code, commit/scope |
| `repo_fact` | What stable operational fact was established? | file slice, test event, explicit observation |
| `file_context` | What prior work is relevant to this file or symbol? | file/symbol refs and supporting events |

Pages are not created for every conversational turn. Low-salience acknowledgments,
unverified speculation, repeated output, and raw logs stay in L4 unless later
evidence makes them operationally useful.

### 5.2 Page size and composition

The initial target is 100-400 searchable tokens per page, with one topic and a
bounded reference set. This is an engineering default to evaluate, not a claim
that one size is universally optimal.

Each page has three distinct representations:

1. **Identity and control metadata:** exact IDs, scope, trust, validity, compiler
   version, and evidence hashes.
2. **Search text:** compact, normalized text designed for FTS and embedding.
3. **Materialized view:** exact WSL records and bounded evidence returned only
   after a hit is selected.

Search text may combine:

- a typed heading such as `failure_episode`;
- the exact failure signature or constraint text;
- a concise deterministic summary;
- stable file paths, symbol names, commands, test names, and error codes;
- the decision/avoidance relationship;
- explicit scope terms.

It must not contain secret-bearing raw artifacts, unrestricted source files, or
model-written prose that lacks evidence links.

### 5.3 Logical page schema

```text
WarmPage
  page_id                 stable logical ID
  page_version            monotonic version for this logical page
  session_id              source session
  repo_id                 canonical repository identity
  task_id                 optional task identity
  branch                  optional branch scope
  commit                  optional evidence commit
  path_scope[]            optional normalized paths/symbols
  kind                    controlled page kind
  trust                   user | repo | system | tool | model | mixed
  status                  active | stale | invalidated
  salience                derived bounded value with explanation
  search_text             inspectable embedding/FTS input
  summary                 bounded human-readable synopsis
  refs[]                  exact WSL/event/artifact/file refs
  source_seq_min/max      ledger replay range
  source_digest           digest of canonical evidence inputs
  compiler_version        deterministic compiler identity
  scope_epoch             active-scope/invalidation epoch
  created_at              source-derived time
  last_verified_at        evidence verification time, if known
```

The embedding, access, and backend fields are derivative and live beside this
logical record:

```text
IndexedPage
  page_id + page_version
  embedding_namespace
  embedding_dimension
  vector
  indexed_source_digest
  indexed_at
  lexical_document
  access_count
  last_accessed_at
  prefetched_count
  useful_prefetch_count
```

Access counters affect residency policy, not evidence truth. They may be lost
without violating correctness.

## 6. Query types

### 6.1 Address fault

Input contains a stable ID such as `F18`, `P7`, `E0042`, or an artifact hash.

Path:

```text
stable ID -> direct resolver -> L2 materialization -> bounded fault response
```

No embedding call and no vector search occur.

### 6.2 Semantic fault

The agent or scheduler knows the need but not the address, for example:

- “Find the previous failure involving stream cancellation.”
- “What decision constrained changes to the transport layer?”
- “Which command last verified this package on this branch?”

Path:

```text
typed query intent
  -> hard scope/trust/validity filters
  -> parallel lexical + dense candidate generation
  -> tuple/snapshot checks + deterministic fusion and provisional policy rerank
  -> abstain or page refs
  -> exact resolver
  -> bounded exact fault response
```

Phase 7E stops at the bounded exact fault response. It neither mutates L1 nor
claims Phase 7F L2 residency.

### 6.3 Working-set prefetch

Phase 7F first runs semantic readahead in metadata-only shadow mode. After a
successful semantic fault and final freshness check, bounded non-selected,
non-suppressed candidates with exact current tuples may be observed without
materializing or retaining their bodies. A selected page whose exact evidence
was actually returned may be admitted separately as demand; observing it as
speculative would make usefulness tautological and is forbidden.

Each pending shadow episode records the exact authoritative page tuple,
embedding namespace, deterministic final fused position, and first
observation-use sequence. It contains no page body, summary, refs, query text,
or vector.
Embedding namespace attributes the estimator that made the prediction; it is
not part of authoritative page identity. A same-tuple/same-namespace duplicate
does not extend the original 64-real-use horizon. Different namespaces retain
separate bounded estimator-attribution episodes while aggregate page
usefulness is counted once.

Only a later real use or exact demand that actually serves the same current
tuple may resolve an episode as useful. Pinning, replay, cold admission,
compatibility cache maintenance, and reconciliation cannot do so. Capacity or
horizon expiry records unused shadow estimation; invalidation is censored and
shoots the observation down rather than scoring it.

Actual speculative L2 body admission is **disabled**. It requires a pinned real
Qwen run plus held-out usefulness and negative-transfer evidence. Similarity
alone never injects L1, and automatic L1 admission remains disabled through
Phase 10.

## 7. Query construction

The runtime never embeds the entire live prompt. It builds a typed query intent:

```text
QueryIntent
  mode              semantic_fault | prefetch | inspection
  session_id
  repo_id
  task_id
  branch
  commit
  scope_epochs[]    coarse current generations; defense in depth
  eligibility_complete
  eligible_page_tuples[]
                     exact session/id/version/digest/compiler/epoch allowlist
  path_hints[]
  allowed_kinds[]
  required_trust[]
  user_text         bounded current request
  active_goal       canonical task goal
  last_failure      exact bounded signature
  next_action       canonical action/target
  exclusions[]      invalidated IDs and rejected scopes
  candidate_limit
  materialize_limit
  token_budget
```

The Phase 7E `Session.SemanticSearch` embeds only its bounded text argument;
scope, identity, trust, epochs, and validity remain metadata filters. A future
prefetch serializer may add canonical relevance-bearing goal/failure/next-action
fields, but it must never embed ACL or authority metadata. The serializer and
query instruction are part of the embedding namespace.

For asymmetric embedding models, document and query preprocessing are distinct.
The reference Qwen profile uses its documented retrieval instruction for
queries; stored page documents are encoded as documents. Tests must catch an
accidental query/document inversion.

## 8. Retrieval pipeline

### 8.1 Stage A - authoritative eligibility

The Phase 7E planner resolves one complete candidate universe per attempt:

1. Under the session boundary, capture the current coherence revision, L4
   source sequence, coarse epoch set, index handle, and `IndexErr` state.
2. Read index health/generation plus the session source/page watermark.
3. In one index read transaction, list every `status = active` page descriptor
   for the session in PageID order. A descriptor contains only its exact
   `PageTuple`, logical WSL/event ref IDs, scope, branch, commit, and path scope;
   search text and summaries are not copied into the authority snapshot. Decode
   refs and paths as strict single JSON values and reuse
   `pages.ValidateAuthorityDescriptor` to validate scope, repo/task authority,
   branch/commit, paths, refs, and kind/trust compatibility.
4. Ask current coherence whether each descriptor's path-associated scope and
   transitive logical refs remain eligible. Suppressed descriptors never enter
   the search allowlist.
5. Re-read generation/source+page watermark and session revision/source
   sequence/`IndexErr`. Any change makes the attempt retry or fail
   operationally rather than treating a partial view as current.
6. Mark the snapshot complete and sort the surviving exact tuples. The tuple is
   `(session, page ID, page version, source digest, compiler version, scope
   epoch)`.
7. Send that same complete allowlist to FTS5 and dense search, alongside repo,
   task, branch, commit, trust, kind, path, exclusion, status, and coarse-epoch
   filters.

Shared/team ACLs remain a required future hard filter before multi-user
deployment; they are not implied by the current single-user query contract.
Historical inspection does not weaken Phase 7E's active-status filter.

FTS joins every tuple field through `json_each` before `LIMIT`. The sqlite-vec
adapter applies the same tuple join while building the ordinary-table rowid set
passed to vec0 through `rowid IN (...)` before KNN chooses `k`. Go validates the
same universe, exact tuple, serving generation, and source/page watermark again.
Cross-session or cross-repository leakage is a channel-contract failure, not a
low retrieval score.

The flat scope-epoch set remains a coarse, cheap defense but is not authority:
different paths can share one epoch value, and an active page can depend on a
transitively invalidated WSL/event ref without changing that scalar. The exact
descriptor check plus tuple allowlist closes both starvation cases before the
candidate limit.

A complete empty allowlist is represented explicitly and matches no rows in
either channel; this can produce a genuine semantic miss. Snapshot capture and
eligibility encoding are each bounded to
`indexer.MaxAuthoritySnapshotPages` (4,096) pages and 4 MiB. Unavailable,
malformed, changing, or over-bound authority returns `ErrIndexUnavailable` for
the semantic fault. It never truncates, paginates, or invokes the legacy
unfiltered inspection behavior.

Malformed active metadata in any covered descriptor class—scope/authority,
path, refs, kind, or trust—first produces the typed index sentinel
`ErrAuthorityDescriptorCorrupt`. It is not an ordinary coherence rejection.
This distinction is verified with high-ranked malformed chaff: even when those
rows would consume legacy top-k, semantic search returns an operational error
with no partial allowlist or materialized evidence.

### 8.2 Stage B - candidate generation

Run both channels concurrently against the same eligible corpus:

- **Lexical:** FTS5 bounded quoted-token AND matching with BM25 rank. This
  catches exact test names, paths, symbols, commands, error strings, and
  identifiers.
- **Dense:** cosine nearest-neighbor search over the active embedding namespace.
  Stored and query vectors are canonicalized to finite, unit-length,
  float32-safe directions before the indexer compares or sends them to
  sqlite-vec. This channel is intended to catch paraphrases and conceptually
  similar episodes.

The harness currently requests 20 candidates per channel, while the provisional
policy caps either request at 50. These are implementation bounds, not a quality
claim or ABI promise.

When a requested query embedding or dense search is unavailable, the dense
channel reports a safe category and lexical search continues. When FTS fails,
dense search may continue. A surviving candidate can therefore produce a
degraded hit. If an operational channel failure leaves no candidate, the call
returns `ErrIndexUnavailable`; only a healthy/valid search that finds or selects
nothing returns `SEMANTIC_PAGE_MISS`. An index opened without dense support is
an intentional lexical-only mode, not a failed request. Exact identifiers
always bypass both channels.

### 8.3 Stage C - reciprocal-rank fusion

Scores from different systems are not directly comparable. The default fusion
uses reciprocal rank fusion (RRF):

```text
rrf(page) = sum over channels 1 / (rrf_k + rank_channel(page))
```

The implemented `rrf/v1` uses `rrf_k = 60`. The checked-in profile and PageID
tie-breaker make fusion deterministic for the candidates each channel returns.
sqlite-vec selects no more than `k` rows before Go sees them, so PageID cannot
stabilize which rows belong to an overfull equal-distance tie at that boundary.

### 8.4 Stage D - deterministic policy rerank

Fusion establishes topical relevance. The implemented
`working-set/v1-provisional` policy then accounts for:

- exact repo/task/branch/commit/path affinity;
- current-failure signature overlap;
- source trust;
- whether the source has a verification timestamp;
- page salience;
- duplicate evidence and topic diversity.

Hard scope and validity rules remain gates, never weights. The initial weights
and thresholds are named, bounded, and included in the safe trace. They are
safety defaults, not values calibrated against real Qwen output. User-pinned
priority, access/recency, invalidation-risk scores, and negative-transfer
history remain Phase 7F/evaluation inputs rather than hidden Phase 7E features.

### 8.5 Stage E - diversity and caps

Phase 7E uses a deterministic equivalent: per-kind and per-exact-source caps
plus bounded token-set Jaccard suppression for near duplicates. A normal
semantic fault returns at most 1-3 materialization candidates, not a dump of
the nearest channel results.

### 8.6 Stage F - threshold and abstention

The retriever applies a named maximum dense distance and minimum fused/policy
score. In Phase 7E both are provisional safety cutoffs. Held-out forced-reset
work must calibrate them before enablement or quality claims. Below them, an
otherwise healthy search returns `SEMANTIC_PAGE_MISS`; precision is more
valuable than reviving a rejected approach or wrong-branch assumption.

### 8.7 Stage G - exact materialization

For each selected page reference:

1. load current logical page metadata;
2. re-check scope epoch, status, trust, and source digest;
3. follow bounded WSL-record refs, with an event-ref fallback, through the exact
   resolver; raw artifacts and file slices are not auto-faulted by semantic
   search;
4. apply cumulative attempt/page/ref/byte/token budgets;
5. return only the bounded exact WSL/event evidence in the fault response.

If any current check fails, suppress that candidate, record the reason, and try
the next candidate within budget. Do not serve the stale indexed copy, indexed
search text, or derivative summary. L2 admission and any later L1 choice are
separate Phase 7F/scheduler policy.

## 9. Residency policy after retrieval

Retrieval and residency are different policies.

The implemented Phase 7F L2 mechanism uses a CLOCK-Pro/2Q-inspired
approximation:

- **Cold:** a first exact demand starts cold with reference bit 1; derived
  future-only material may start cold/ref0.
- **Hot:** a later actual demand/use promotes a cold page; an exact-authority
  ghost refault may promote directly. Hot pages demote before eviction.
- **Test/ghost:** recently evicted exact identities are retained without bodies,
  summaries, refs, query text, or vectors to detect recurring demand.
- **Pinned:** hard constraints and active task anchors; these are scheduled by
  explicit policy, not similarity, and count within the resident budget.

Default bounds are 64 resident pages/512 KiB logical retained bytes, including
a 16-page/128-KiB pinned subset, with at most 64 KiB per page. Ghost metadata is
bounded to 64 entries/32 KiB; semantic-shadow episodes to 256 entries/64 KiB
over 64 later real uses; categorical residency trace to 512 entries. Logical
size counts every retained page string/list value. These are provisional safety
bounds rather than calibrated working-set sizes.

Each real access sets a reference bit. The deterministic hand never selects by
map order, wall time, retrieval score, or similarity. It skips pinned pages,
grants referenced pages one chance, demotes hot pages before eviction, and
retains only a bounded bodyless ghost. Pin overflow is transactional: it cannot
silently evict an existing pin. Direct page hits copy and record use atomically.
Bodies remain reconstructible from L4.

Ghost hits require the exact `(session, page ID, version, source digest,
compiler, scope epoch)` tuple. Reuse distance/thrash telemetry is derived from
deterministic reclaim/reference progression, never timestamps. A tuple change
purges the matching resident/estimator identity. Because ghosts and shadows
omit dependency refs, a broader residency-changing coherence event
conservatively purges every ghost and censors every pending shadow rather than
letting unverifiable metadata guide promotion or usefulness.

The L1 scheduler then chooses from pinned records plus validated L2 pages under
the capsule budget. A page's admission to L2 does not guarantee L1 residency,
and the Phase 7F mechanism itself never performs automatic L1 admission.

## 10. Go interfaces and ownership

The contracts below are target shapes, not claims about current packages:

```go
type PageCompiler interface {
    Compile(ctx context.Context, change LedgerChange) ([]WarmPageMutation, error)
    Version() string
}

type Embedder interface {
    EmbedDocuments(ctx context.Context, texts []string) (EmbeddingBatch, error)
    EmbedQuery(ctx context.Context, text string) (Embedding, error)
    Namespace() EmbeddingNamespace
}

type WarmIndex interface {
    Apply(ctx context.Context, mutations []IndexedPageMutation) error
    ActivePageSnapshot(ctx context.Context, sessionID string) (PageAuthoritySnapshot, error)
    SearchLexical(ctx context.Context, q SearchQuery) ([]Candidate, error)
    SearchDense(ctx context.Context, q SearchQuery, v Embedding) ([]Candidate, error)
    Watermark(ctx context.Context, stream StreamKey) (uint64, error)
    Rebuild(ctx context.Context, source PageSource) error
    Health(ctx context.Context) IndexHealth
    Close() error
}

type Retriever interface {
    ResolveSemantic(ctx context.Context, intent QueryIntent) (RetrievalResult, error)
}

type PageMaterializer interface {
    Materialize(ctx context.Context, ref PageRef, budget Budget) (ResolvedPage, error)
}
```

Ownership rules:

- `ledger` owns immutable events and sequence order.
- `artifacts` owns exact large bytes.
- `observers` and the page compiler own reproducible logical page mutations.
- `embedder` owns namespace/profile validation, role-specific payloads,
  admission, supervision, and the local backend client.
- `indexer` owns lexical/vector rows, descriptor snapshots, exact tuple joins,
  tuple CAS, generations, and watermarks; it never calls an embedding backend.
- `retrieval` owns filters, fusion, reranking, abstention, and explanations.
- `memory` owns L2 residency/access state.
- `scheduler` owns L1 admission and eviction policy.
- `faults` owns exact materialization and the external fault ABI.
- `harness` owns lifecycle integration, current-coherence reduction of the
  descriptor snapshot into a complete tuple allowlist, asynchronous embedding
  calls/writeback, and cancellation.

No backend client type crosses the `WarmIndex` boundary.

## 11. Default embedded backend

### 11.1 Storage layout

Use a second database so cache deletion is mechanically distinct from truth:

```text
<data-dir>/ledger.db                 authoritative L4 events
<data-dir>/artifacts/...             authoritative L4 bytes
<data-dir>/index/warm.db             disposable L3 metadata + FTS + vectors
<data-dir>/index/rebuild.lock        local rebuild coordination
```

`ledger.db` must not acquire vector extension tables. A user can stop WSMS,
remove `index/`, and rebuild without data loss.

### 11.2 Logical SQLite tables

```text
index_meta
  schema_version, compiler_version, embedding_namespace, created_at

index_watermarks
  session_id, last_source_seq, last_page_version

warm_pages
  page_id, page_version, session_id, repo_id, task_id, branch,
  kind, trust, status, scope_epoch, search_text, summary,
  refs_json, source_digest, source_seq_min, source_seq_max,
  compiler_version, created_at, last_verified_at

warm_pages_fts
  FTS5 projection of selected search fields

warm_pages_vec
  sqlite-vec vec0 KNN projection with session/namespace partition keys

warm_page_vec_map
  page_id, vec0 rowid, session_id, page_version, source_digest,
  compiler_version, embedding_namespace

warm_page_vec_rows
  ordinary rowid/page/session/namespace mirror used to construct the bounded
  eligibility bitmap; serving code never performs an ordinary scan of vec0

warm_page_vec_shadow
  current tuple, embedding_namespace, canonical unit-float32 vector JSON used
  for rebuild/residency validation without ordinary vec0 scans

warm_page_vec_suppressed
  current page tuple, embedding_namespace, fixed lexical-only reason

access_stats
  page_id, access_count, last_accessed_at,
  prefetched_count, useful_prefetch_count
```

The SQL `access_stats` sketch remains unimplemented and is not required for the
first Phase 7F slice. Phase 7F keeps deterministic reference/use, ghost, pin,
and semantic-shadow counters in bounded in-memory residency state; losing them
does not affect truth. The vector map, rowid mirror, canonical-vector shadow,
and suppression table are implemented Phase 7C/7D state. They are all
disposable; none belongs in `ledger.db`.

The Phase 7E exact eligibility allowlist is per-attempt query state, not a new
durable table. Its JSON tuple set is passed to SQLite as data and joined on all
six tuple fields before FTS `LIMIT` and before vec0 rowid eligibility/KNN.

The actual migration must reflect the APIs available in the pinned SQLite and
`sqlite-vec` versions. It must use prepared statements, explicit transactions,
foreign/logical integrity checks, and a migration version.

### 11.3 Why SQLite + FTS5 + sqlite-vec

- It matches the local-first, no-service deployment model.
- The Go runtime already uses the pure-Go `modernc.org/sqlite` stack.
- FTS5 is mature and strong for code identifiers and error strings.
- `modernc.org/sqlite` now distributes a transpiled `sqlite-vec` package, so an
  embedded path need not make Python authoritative or require CGO.
- `vec0` supports cosine KNN, and its rowid-IN constraint lets WSMS compute the
  authoritative eligible set in ordinary SQL before the KNN limit.
- A separate DB keeps all search state disposable.

The caveat is important: `sqlite-vec` documentation and releases remain
pre-1.0. Phase L3-2 proved extension registration, persistence, filtering,
dimension handling, cancellation, restart, and clean rebuild on the development
pure-Go platform using the exact pinned dependency. Supported-platform breadth,
corruption behavior beyond current recovery tests, and production resource
limits still require explicit verification.

The repo pins `modernc.org/sqlite v1.53.0`, whose
module carries `sqlite-vec v0.1.9` and auto-registers it on supported targets
when `_ "modernc.org/sqlite/vec"` is imported. The disposable indexer owns that
blank import; the ledger and vector-free demo do not depend on it.

The current v0.1.9 vec0 path performs brute-force KNN. WSMS must measure CPU,
memory, latency, and concurrency at the intended corpus before treating it as a
production envelope. sqlite-vec chooses at most `k` rows before Go can sort
ties. PageID therefore stabilizes equal-distance order only inside the returned
set; when more than `k` eligible rows tie at the boundary, membership is
backend-defined unless a future bounded tie-completion mechanism is added.

Avoid excessive vector partitions. Repository partitioning is appropriate only
when partitions contain enough pages to search efficiently; otherwise use
metadata filters and query-time validation.

### 11.4 Reference exact backend

Keep a deterministic in-process backend that stores vectors in ordinary rows
or memory and computes exact cosine similarity. It is not for production scale.
It provides:

- a correctness oracle for ANN results;
- tiny-fixture tests without extension coupling;
- deterministic evaluation of fusion and policy code;
- a fallback during migrations and compatibility diagnosis.

## 12. Optional Qdrant backend

Qdrant is the preferred dense ANN scale-out component when measurement shows
the embedded backend cannot meet the target corpus, concurrent-query,
filtering, or latency SLO. It offers dense/sparse hybrid queries, payload
filtering, HNSW indexing, on-disk options, snapshots, and an official Go client.
The lowest-risk first scale-out composition keeps SQLite FTS5 as the lexical
channel and replaces only dense `sqlite-vec` search with Qdrant. A later Qdrant
sparse/BM25 channel must prove lexical and abstention parity before replacing
FTS5.

One collection represents one embedding namespace. Payload contains only the
derivative page metadata needed for hard filtering and candidate explanation:

```text
point ID       deterministic UUID/hash of page_id + page_version
vector         dense document embedding
payload        repo/task/branch/kind/trust/status/scope_epoch/refs/source_digest
```

The Go runtime supervises a local Qdrant process or connects to an explicitly
configured service. It does not assume Qdrant Edge because its embedded client
support is not currently Go-native. L4 remains local authority; Qdrant snapshots
are an optimization, not the disaster-recovery source.

Move to Qdrant only when a checked-in benchmark, repeated on supported target
machines, demonstrates that SQLite misses an agreed SLO or resource budget.
The initial design target is p95 under 75 ms for retrieval after a query vector
exists and under 350 ms for the complete local semantic lookup. These are
product targets to validate, not reported measurements.

Migration is dual-write/dual-build:

1. build a new namespace from L4 while the old backend serves queries;
2. compare recall, filters, abstention, and latency on the frozen corpus;
3. shadow production-like queries without changing L1;
4. cut over by configuration after parity gates pass;
5. retain a rollback window, then delete the old derivative index.

LanceDB remains a reasonable future adapter, especially for Arrow-native
offline analysis, but it is not the default. The newer Go surface and additional
Arrow/Rust integration offer less immediate leverage than the existing SQLite
runtime plus a clearly isolated Qdrant scale-out path.

## 13. Embedding profile

### 13.1 Namespace

The namespace is a canonical digest of:

```text
provider + model repository + exact revision + dimensions + distance metric
+ normalization + query instruction + document template + tokenizer revision
+ page schema version + redaction version
```

Every stored vector records this identity. A change creates a new namespace and
requires a rebuild; the runtime never silently mixes old and new vectors.

### 13.2 Default private profile

The reference profile is `Qwen/Qwen3-Embedding-0.6B` with its documented
1024-dimensional output and retrieval-query instruction. It is Apache-2.0,
supports code and multilingual retrieval, and is small enough to evaluate as a
local sidecar while still being a serious retrieval model.

The reference serializer follows the model card's asymmetric retrieval shape:
documents remain unprefixed, while queries are rendered as
`Instruct: <WSMS retrieval task>\nQuery: <query>`. The complete literal prefix,
normalization, model/tokenizer revisions, page schema, and redaction version are
part of the namespace digest. The Hub revision observed during the 2026-07-10
research pass was `97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3`; it is a candidate
for a reproducibility run, not a mutable default or a claim that those weights
were downloaded and executed.

The model runtime is behind `Embedder`; model ownership never leaks into ledger
or retrieval interfaces. A Rust or local inference sidecar may expose a bounded
Unix-domain-socket or loopback API. Startup performs a known-vector/profile
self-check. The Go runtime sets deadlines, batch limits, and circuit breaking.

This profile is a reference choice, not an immutable product dependency. It
must beat FTS-only and smaller/cheaper alternatives on WSMS's own corpus before
becoming enabled by default.

### 13.3 Hosted providers

Hosted embeddings are disabled by default. Enabling one requires:

- an explicit provider configuration and consent boundary;
- documented data retention and region policy;
- secret and high-risk-data redaction before transmission;
- a visible list/export of the exact text eligible to leave the machine;
- per-request timeouts, rate-limit handling, and cost telemetry;
- a distinct namespace from every local model.

Provider failure falls back to FTS and does not block ledger persistence.

### 13.4 Embedding cache and idempotency

Document embeddings are keyed by:

```text
SHA256(embedding_namespace || canonical_search_text)
```

Identical content reuses a vector. Page-vector mutations compare-and-swap
`(page_id, page_version, source_digest, compiler_version, namespace)`. Query
embeddings may use a small bounded TTL cache only after stripping
session-specific identifiers from the cache key where safe.

## 14. Indexing and consistency

### 14.1 Write-through truth, asynchronous cache

The foreground path is:

```text
append event to L4
  -> validate/apply deterministic WSL update
  -> return foreground result
```

The deterministic derivative commit path currently is:

```text
current committed event or startup catch-up
  -> compile logical page mutation
  -> redact/normalize searchable text
  -> transactionally write page + FTS projection + contiguous source watermark
  -> non-blocking wake of the dense writeback worker
```

Page compilation always runs synchronously on the append thread against
as-of-event state, so replay stays equivalent (A3). The transactional write is
synchronous by default; behind the default-off `AsyncMaintenance` flag (Phase 8)
it is deferred to a bounded per-session worker under the same watermark/idempotency
contract. Deferral never blocks the durable append (NFR-010): a full queue or a
gap flags a ledger-watermark reconciliation, and the freshness gate abstains
while the index trails the ledger, so an asynchronous cache still never beats
exact evidence.

The independent dense path is:

```text
scan active page tuples missing the configured namespace
  -> supervised self-check and bounded document embedding
  -> canonicalize to a finite unit float32-safe direction
  -> compare-and-swap page version + digest + compiler + namespace
  -> transactionally upsert vec0/map/residency shadow
  -> or atomically evict dense residency and record tuple-scoped lexical-only
     suppression when final payload admission denies the page
```

The source watermark intentionally does not claim vector completion. A slow
v1 embedding cannot be stamped onto v2: the tuple CAS rejects it, and the new
page remains in the missing-vector working set. Transient service faults retry
with bounded cancelable backoff. Terminal namespace/ABI/vector faults park
until a new wake or reopen. An L3 error never rolls back or disguises a
successful ledger append.

### 14.2 Crash recovery

On startup, the indexer compares each session's L4 high-water sequence with the
L3 watermark. It replays missing ranges through the deterministic compiler.
Page version and source digest make retries idempotent.

If schema, compiler, redaction, or embedding identity is incompatible, WSMS
creates a new index generation and rebuilds. It does not mutate an incompatible
generation in place.

### 14.3 Update and invalidation

New evidence never overwrites the old ledger event. It emits a new logical page
version or invalidation mutation. Page/FTS application is transactional per
batch. Vector map, vec0 row, and ordinary residency shadow are transactional per
write, but may lag the page watermark. A page update/invalidation removes its
old dense row and lexical-only suppression; only an exact current-tuple CAS may
install either state again.

At query time, current invalidation and scope state is checked again outside the
index. For explicit semantic faults the harness additionally checks the
documented `IndexErr`, serving generation, source watermark, current L4 event
sequence, and coherence revision before and after resolution. Known lag or a
projection change is operational failure, not a miss. These checks close the
window where an asynchronous index has not yet processed an invalidation or a
concurrent rebuild/index fault would otherwise be hidden by an empty result.

### 14.4 Full rebuild

`wsms index rebuild` should eventually:

1. acquire a local generation lock;
2. create a new temporary generation beside the serving index;
3. replay L4 in sequence with bounded batches;
4. validate row counts, digests, namespaces, filters, and sample searches;
5. fsync/close and atomically switch the active-generation pointer;
6. keep the prior generation for a bounded rollback period;
7. remove it only after healthy queries.

Queries continue on the previous generation during rebuild. Interrupted
temporary generations are safe to delete.

## 15. Security, privacy, and poisoning resistance

### 15.1 Data minimization

- Embed typed summaries and exact bounded signatures, not complete raw logs.
- Do not embed artifact bytes by default.
- Do not embed `.env`, credential stores, private keys, tokens, or paths denied
  by workspace policy.
- Persist the inspectable `search_text` that produced each document vector so a
  user can audit what was indexed.

### 15.2 Trust and prompt injection

Pages preserve source labels. Tool output and repository text may contain
instructions but remain quoted evidence. Retrieval never changes their role or
places them in the system-policy portion of the capsule.

A page compiler may extract exact strings deterministically. Any model-assisted
summary is labeled, linked to evidence, and excluded from hard constraints or
decisions until a trusted deterministic rule or user action establishes it.

### 15.3 Scope and access

The first implementation is single-user and local, but the interface includes
an authorization filter so a later shared backend does not retrofit ACLs after
ranking. Every query and materialization carries the caller/session scope.

Filters are applied before similarity where the backend supports it, then
rechecked by the Go runtime. Index telemetry must not log page text or vectors
at normal verbosity.

### 15.4 Deletion and retention

Deleting an index generation is always safe. Deleting authoritative events or
artifacts is a separate retention operation and must trigger page invalidation
and rebuild. A future multi-user product needs explicit right-to-delete,
encryption, ACL, and export policy before remote vector storage is enabled.

## 16. Degraded modes and failures

| Failure | Required behavior |
|---|---|
| Embedder timeout/unavailable | Continue FTS-only and expose a safe category; if FTS selects nothing, report operational unavailability rather than a semantic miss |
| FTS failure but vector healthy | Dense candidate generation may continue and return a degraded hit; no surviving hit is operational failure; direct IDs are unaffected |
| Entire L3 unavailable | L1/L2 current state and all direct L4 faults continue |
| `IndexErr`, lagging watermark, or projection change | Explicit semantic fault fails operationally before/after resolve; never disguise known stale projection as a miss |
| Complete authority snapshot is empty | Search an intentionally empty universe; return a genuine semantic miss if no independent operational fault exists |
| Authority snapshot unavailable, changing, malformed, over 4,096 pages, or over 4 MiB | Return operational unavailability; never truncate or fall back to unfiltered ranking |
| Active descriptor violates scope/authority/path/ref/kind/trust schema or strict JSON | Return typed descriptor corruption, surfaced as operational unavailability; never suppress into a miss |
| Namespace mismatch | Refuse dense comparison; schedule/recommend rebuild |
| Malformed vector/dimension | Reject mutation or generation; never truncate/pad silently |
| Candidate is no longer materializable | Suppress it categorically and try next within cumulative budget |
| Exact resolver/materializer operation fails | Return operational error, not a miss |
| No candidate above threshold after an operationally valid search | Return `SEMANTIC_PAGE_MISS` |
| Qdrant process exits | Circuit-break to embedded/FTS path if configured; L4 remains healthy |
| Index corruption | Quarantine generation and rebuild from L4 |

Search errors and semantic misses are different result types. Neither may be
presented as exact evidence.

## 17. Observability

The implemented Phase 7E lookup produces a bounded, inspectable, text-free
explanation without copying query text, indexed prose, or raw backend errors:

- query mode, active filters, policy identity, and candidate/materialization
  budgets;
- lexical and dense channel availability categories and candidate counts;
- candidate IDs and per-channel ranks;
- fusion score and named rerank features;
- filter/suppression/abstention reasons;
- selected page IDs and materialization token use.

Each candidate also carries its exact six-field page tuple, serving generation,
and source/page watermark for validation. The harness checks `IndexErr`,
coherence revision, source sequence, generation, and watermark freshness around
snapshot construction and after resolution. Exact refs remain inside the
materializer, and derivative page prose is stripped before result return.

Future observability work adds per-channel and total latency, explicit
generation/watermark-lag reporting, and exact-ref debug views under policy.
Phase 7F now adds a separate bounded, body-free residency snapshot/trace with
admission/use, cold/hot/pinned transitions, ghost/refault/thrash, invalidation,
rejection, and semantic-shadow useful/unused counters. This mechanism status is
subject to the root final verification matrix; it is not evidence that actual
prefetch improves outcomes.

Aggregate metrics include:

- index pages/bytes by kind, repo, status, and namespace;
- compiler/index lag and failed mutations;
- semantic fault count, hit, miss, error, and abstention rates;
- Recall@k, MRR, nDCG, and exact-reference precision on labeled queries;
- stale-page suppression and wrong-scope candidate rates;
- negative-transfer and repeated-failure rate after retrieval;
- metadata-only shadow useful/unused ratio now; actual useful-prefetch ratio
  and pages evicted unused only after speculative L2 admission is enabled;
- L2 hit ratio, ghost hit ratio, promotion/demotion rate, and thrash;
- p50/p95/p99 embedding, retrieval, and page-in latency;
- tokens introduced per useful retrieved page.

Logs use IDs and reasons by default. A deliberate debug mode may show redacted
search text locally.

## 18. Evaluation and release gates

The vector path is successful only if it improves continuation behavior, not
merely offline similarity.

### 18.1 Frozen variants

Run the same event streams and forced-reset checkpoints against:

1. no L3; direct IDs and current L1/L2 only;
2. FTS5 only;
3. dense only;
4. FTS5 + dense RRF;
5. hybrid + policy rerank + abstention;
6. strong YAML/Markdown working-state baseline;
7. provider compaction/summary baseline where available.

### 18.2 Retrieval labels

Create hand-reviewed queries for each page kind, including:

- exact identifier/error/path queries;
- paraphrases;
- multi-hop decision/failure questions;
- same words in the wrong branch/task/repo;
- superseded and invalidated evidence;
- malicious imperative text inside tool/repo evidence;
- true no-answer queries that must abstain.

### 18.3 Metrics

Offline:

- Recall@1/3/10, MRR, nDCG;
- exact evidence-reference precision;
- scope leak, stale revival, and false-positive abstention rates;
- latency, index bytes, rebuild time, and embedding cost.

End-to-end after forced reset:

- task continuation success;
- repeated failed approaches;
- hard-constraint violations;
- exact command/error recall;
- stale-assumption recurrence;
- human reminders;
- active tokens and page faults;
- wall time to next verified action.

### 18.4 Enablement gates

Before semantic retrieval can affect L1 automatically:

- direct-ID behavior and no-L3 operation remain unchanged;
- zero cross-session/repo/ACL leaks in adversarial fixtures;
- zero invalidated-page materializations in the coherence suite;
- hybrid beats FTS-only on held-out exact-reference precision/recall without a
  material increase in negative transfer;
- end-to-end forced-reset outcomes improve or token cost falls at equal success;
- p95 latency meets the configured local SLO;
- index deletion and full rebuild pass crash/interruption tests;
- every selected page has an inspectable explanation and exact L4 refs.

If those gates fail, ship FTS-only or explicit semantic-search tooling and keep
automatic prefetch disabled.

## 19. Implementation sequence

### L3-0 - Evaluation corpus and exact oracle

**Status:** complete (Phase 7A).

- Strict replay corpus: `internal/pages/testdata/frozen_corpus.json`; legacy
  page compiler goldens remain under `testdata/pages/corpus/transport_fix/`.
- Page schema, `DeterministicCompiler`, and
  `ExactCosineSearch` / `ExactCosineSearchContext` live in `internal/pages`.
- Materialization gate: `ValidateMaterializable` (session, current coherence
  generation, transitive ref eligibility, full authority, compiler, and
  projection-bound source digest).
- No-L3 baseline remains the direct-ID demo. FTS-only baseline starts in L3-1.

### L3-1 - Interfaces and lexical path

**Status:** complete (Phase 7B). Dense materializer remains partial (no dense).

- `internal/indexer`: disposable FTS5 `warm.db`, Apply, SearchLexical, Rebuild,
  watermarks.
- `internal/retrieval`: `QueryIntent`, lexical `ResolveSemantic`,
  `SEMANTIC_PAGE_MISS`.
- Harness best-effort contiguous index apply/catch-up after events;
  `SemanticSearch` revalidates then materializes exact L4 evidence.
- Delete `index/`, WAL-safe rebuild cutover, process-local multi-handle/symlink
  coordination, cross-session isolation, and corpus FTS labels are covered by
  package tests. Multi-process writers remain out of MVP scope and require a
  filesystem-wide operation lock before that boundary expands.

### L3-2 - sqlite-vec compatibility spike

**Status:** complete (Phase 7C) on the development pure-Go path; dense remains
config-gated (`DenseDimensions`, default 0).

- `modernc.org/sqlite v1.53.0` + blank `_ "modernc.org/sqlite/vec"`.
- Optional `vec0` cosine projection with session + embedding-namespace
  partition keys and a page-version/source-digest/compiler-bound map.
- `SearchDense`, vector upsert/delete, invalidate cleanup, legacy projection
  recreation, compatible-vector rebuild copy, and restart meta restore.
- Exact cosine oracle parity tests in `internal/indexer/parity_test.go`.
- Real document/query embeddings are supplied by L3-3 / Phase 7D when a local
  embedder is configured; FTS remains the default fallback and truth path.

### L3-3 - Local embedding adapter

**Status:** in-repo ABI/client/lifecycle implemented and verified with
deterministic backends and adversarial local HTTP/Unix test servers. Optional,
local-first, and non-authoritative. Phase 7E can consume the adapter's dense
query ranks. An actual Qwen serving process/model-weight run is an open
operational gate and is not claimed here.

- Namespace/profile types and document/query separation are implemented in
  `internal/embedder`.
- The reference Qwen3 profile is represented as a complete namespace ABI and
  is accepted only through the supervised WSMS-owned local protocol. The client
  disables proxies, rejects redirects, bounds responses, and revalidates a
  literal loopback target at connection time (or uses an explicit Unix socket).
- Redaction/admission, batching, deadlines, circuit breaking, self-checks,
  document-cache singleflight, and bounded response handling are covered by
  tests.
- Dense writes CAS the page tuple and generation namespace. Admission-denied
  pages become tuple-scoped lexical-only entries; transient failures retry and
  terminal ABI faults park without starving later safe pages.
- FTS fallback remains visible and deterministic; dense embedding/backfill is
  asynchronous and never blocks L4 append or direct page faults. Phase 7E owns
  the separate decision to let dense ranks affect candidate discovery.

### L3-4 - Hybrid semantic faults

**Status:** mechanism complete and verified with deterministic/synthetic vectors
and local test embedders. Real-Qwen execution and retrieval-quality calibration
remain open.

- Parallel FTS/dense candidate generation uses identical pre-limit authority
  filters. A descriptor-only active-page snapshot is reduced through current
  path/transitive-ref coherence into a complete exact tuple allowlist; FTS joins
  it before `LIMIT`, and sqlite-vec receives its rowid set before KNN.
- Active descriptors reuse the page-admission authority validator and strict
  JSON decoding. Malformed scope/authority/path/ref/kind/trust metadata is typed
  operational corruption before allowlisting, including high-ranked chaff.
- Canonical unit-float32 vectors, exact tuple/generation/watermark checks,
  `rrf/v1`, and the named provisional policy cover fusion, deterministic
  rerank, diversity, distance/min-score abstention, and safe explanations.
- `Session.SemanticSearch` rechecks `IndexErr` and projection freshness before
  and after authority-snapshot construction and resolution, then materializes
  exact evidence through the existing resolver under cumulative budgets.
- Complete-empty authority is a valid miss. Unavailable/changing/malformed,
  over-4,096-page, or over-4-MiB authority is operational failure without
  truncation or unfiltered fallback.
- Operational channel/materializer faults stay distinct from valid misses.
  Dense/query failure may return a degraded lexical hit but cannot convert an
  empty operational failure into `SEMANTIC_PAGE_MISS`.
- Known WSL/event IDs still bypass L3. Compiler-derived `wp_*` IDs require the
  descriptor tuple and transitive refs, so `ReadPage(wp_*)` remains disabled on
  the ID-only resolver; repeated semantic selection revalidates/rematerializes
  L4 before authoritative resident replacement. Phase 7E retrieval itself does
  not inject L1 or own residency; the Phase 7F layer consumes only finally
  fresh exact demand results and bodyless candidate observations. Actual
  speculative prefetch is still disabled.
- Open gates: run pinned real Qwen weights, measure vec0 brute-force resources,
  compare held-out no-L3/FTS/dense/hybrid variants, and calibrate the provisional
  weights/thresholds before promotion.

### L3-5 - Unix-style residency and prefetch

**Entry gate:** keep Phase 7E explicit-fault-only and freeze its provisional
profile while adding hot/cold/ghost mechanics. Shadow accounting may proceed;
L2-only prefetch requires real-model held-out usefulness and negative-transfer
evidence, and automatic L1 admission remains blocked through L3-6/Phase 10.

**Status:** bounded hot/cold/pinned bodies, bodyless exact ghosts, deterministic
reference/use accounting, selected-page demand admission, metadata-only
semantic-shadow accounting, invalidation shootdown, and bounded trace/metrics
are implemented in the current worktree subject to the root final
correctness/race/demo verification matrix.

- Default policy: 64 resident/512 KiB logical, including 16 pinned/128 KiB;
  64 KiB/page; 64 ghosts/32 KiB; 256 shadow episodes/64 KiB over 64 real uses;
  512 trace entries.
- First demand is cold/ref1; later actual demand/use or exact ghost refault may
  promote. Pinned active-task/hard-constraint anchors are explicit and bounded.
- Selected semantic evidence is demand-admitted only after exact L4
  materialization and final freshness. Non-selected, non-suppressed exact
  tuples remain body-free observations.
- Embedding namespace is estimator attribution, not authoritative page
  identity. Invalidation shoots down body, ghost, and matching observations.
- Actual speculative L2 body admission remains disabled pending pinned real
  Qwen plus held-out usefulness/negative-transfer evidence. Automatic L1
  admission remains disabled through Phase 10.

### L3-6 - Forced-reset evaluation

- Compare all frozen variants.
- Calibrate thresholds and policy weights only on training fixtures.
- Gate automatic prefetch/L1 admission on held-out end-to-end results.

### L3-7 - Optional Qdrant scale-out

- Benchmark the embedded backend at target scale first.
- Implement the backend adapter with identical filter and explanation semantics.
- Dual-build, shadow, parity-check, and cut over only after measured need.

## 20. Rejected designs

| Rejected design | Reason |
|---|---|
| Vector DB as primary memory | Approximate retrieval and model changes would become data-loss/correctness risks |
| Embed every raw event or transcript chunk | High duplication, poor semantics, secret exposure, and no operational page boundary |
| Dense-only search | Weak on exact code symbols, paths, commands, and errors |
| FTS-only forever | Misses paraphrases and conceptual recurrence that motivate semantic faults |
| Vector hit directly injected into system context | Bypasses provenance, trust, validation, budgets, and current invalidation |
| One giant shared collection without hard filters | Creates cross-task/repo leakage and negative transfer |
| One vector per repo/session partition regardless of size | Over-partitioning can damage search efficiency and complicate scope policy |
| Python-owned Chroma/FAISS service | Conflicts with runtime authority and adds a weakly isolated truth-like process |
| Qdrant required from day one | Adds process/operational cost before the corpus proves ANN is necessary |
| LanceDB as immediate default | Valuable technology, but less aligned than the existing SQLite Go path for the first local backend |
| Model-generated memory promoted as fact | Reintroduces hallucinating memory authority |

## 21. Primary-source basis

These sources describe current implementation capabilities; they do not prove
that the design improves WSMS task outcomes:

- The [CLOCK-Pro USENIX
  paper](https://www.usenix.org/conference/2005-usenix-annual-technical-conference/clock-pro-effective-improvement-clock-replacement)
  supplies the cold/hot clock, reference-bit, bounded nonresident-test, and
  adaptive-allocation precedent.
- The [2Q VLDB paper](https://www.vldb.org/conf/1994/P439.PDF) supplies the
  probationary resident, tag-only history, and recurrent/hot queue precedent.
- Linux [`mm/workingset.c`](https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/plain/mm/workingset.c)
  documents bodyless shadow entries, refault distance, and bounded shadow-node
  reclamation. WSMS's exact tuples and semantic shadows are a local adaptation.
- Linux [cgroup v2 `memory.stat`](https://docs.kernel.org/admin-guide/cgroup-v2.html)
  documents refault, activation, restore, scan, steal, promotion, and demotion
  counters that motivate the categorical Phase 7F telemetry.

- [SQLite FTS5](https://www.sqlite.org/fts5.html) documents phrase, prefix,
  boolean, NEAR, BM25/rank, and external-content search behavior.
- [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) documents the
  pure-Go SQLite dependency already selected by WSMS.
- [`modernc.org/sqlite/vec`](https://pkg.go.dev/modernc.org/sqlite/vec) documents
  the transpiled sqlite-vec extension distributed with modernc SQLite.
- [`sqlite-vec` vec0](https://alexgarcia.xyz/sqlite-vec/features/vec0.html) and
  [KNN search](https://alexgarcia.xyz/sqlite-vec/features/knn.html) document
  vector tables, metadata/partition columns, distance metrics, and KNN queries;
  the project currently labels parts of its documentation work in progress.
- [Qdrant hybrid queries](https://qdrant.tech/documentation/search/hybrid-queries/),
  [filtering](https://qdrant.tech/documentation/search/filtering/), and
  [payload](https://qdrant.tech/documentation/concepts/payload/) document the
  scale-out capabilities used by the optional adapter.
- [Qdrant's official Go client](https://github.com/qdrant/go-client) supports a
  Go-owned integration boundary.
- [Qwen3-Embedding-0.6B](https://huggingface.co/Qwen/Qwen3-Embedding-0.6B) and
  the [Qwen3 embedding paper](https://arxiv.org/abs/2506.05176) document the
  reference local embedding model, dimensions, license, and retrieval scope.
- Hugging Face's [TEI quick tour](https://huggingface.co/docs/text-embeddings-inference/en/quick_tour)
  and [supported-model matrix](https://huggingface.co/docs/text-embeddings-inference/supported_models)
  document local Qwen3-Embedding-0.6B serving and current CPU/GPU platform
  support. TEI is a candidate engine behind a WSMS protocol bridge, not a
  bundled dependency or verified runtime in this repo.
- [LanceDB hybrid search](https://docs.lancedb.com/search/hybrid-search) and its
  [Go package](https://pkg.go.dev/github.com/lancedb/lancedb-go/pkg/lancedb)
  support its status as a viable but non-default future adapter.

## 22. Final architectural rule

L3 helps WSMS discover *which address may matter*. L4 proves *what happened*.
L2 holds the validated page. L1 receives only the bounded part needed now.

That division is the reason vector retrieval strengthens the Unix memory model
instead of replacing it with an opaque similarity database.
