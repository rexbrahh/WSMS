# WSMS L3 Warm Memory and Semantic Paging

**Status:** Normative target architecture; implementation follows the first local mechanism demo  
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
   dense-vector search. Metadata filters run before relevance scoring.
4. Hybrid retrieval returns page references. It does not return authoritative
   prose. The resolver follows those references to exact L4 evidence before a
   page becomes L2 or L1 state.
5. Known identifiers always use direct lookup and bypass embeddings. Vector
   retrieval exists for semantic faults, such as “what did we learn about this
   deadlock?”, when the caller does not know an identifier.
6. The default backend is a local, separate SQLite database using FTS5 and the
   `sqlite-vec` extension distributed with the pinned `modernc.org/sqlite`
   dependency. This path requires a compatibility spike because `sqlite-vec`
   is still pre-1.0.
7. An exact, brute-force cosine backend is retained as a small reference
   implementation for tests and evaluation.
8. Qdrant is the optional ANN scale-out backend only after measured corpus or
   latency pressure justifies another process. It implements the same Go
   interface and remains reconstructible from L4.
9. The default private embedding profile is a local
   `Qwen/Qwen3-Embedding-0.6B` adapter, namespaced with its complete model and
   preprocessing identity. Hosted embedding providers are explicit opt-in.
10. A hybrid retriever earns promotion through forced-reset evaluation against
    no-L3, FTS-only, dense-only, and strong structured-memory baselines.

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
gates. Dense or lexical relevance cannot override them.

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

If no candidate clears the calibrated relevance and validity threshold, the
retriever returns `SEMANTIC_PAGE_MISS`. It does not fill the gap with a weak
match.

### L3-I8 - Eventual index consistency cannot weaken truth

Index lag may cause a temporary miss. It must not block ledger appends, alter
replay, or make a stale page authoritative. Query-time post-validation uses the
current ledger-derived invalidation state.

### L3-I9 - Bounded materialization

Search, page-in, and rendering each accept explicit candidate, byte, and token
budgets. Large artifacts remain outside the capsule.

### L3-I10 - Rebuild equivalence

Given the same ledger, artifact store, compiler version, and embedding profile,
a rebuild produces the same logical page identities and searchable text. Minor
floating-point ranking ties must have a stable ID tie-breaker.

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
  -> lexical + dense candidate generation
  -> deterministic fusion and policy rerank
  -> abstain or page refs
  -> exact resolver
  -> validated L2 page
  -> bounded L1/fault rendering
```

### 6.3 Working-set prefetch

Before a turn, the scheduler may search from the active task, last failure,
next action, touched paths, and current user intent. Results are admitted only
as prefetched L2 references. They are not injected into L1 solely because they
are similar.

An explicit use or fault sets a reference bit and counts as useful prefetch.
Unused prefetched pages decay quickly. The scheduler records precision so that
over-eager readahead is observable.

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

The embedding query text is a stable serialization of only relevance-bearing
fields. Session IDs, ACLs, epochs, and validity are metadata filters rather than
semantic text. The serializer and query instruction are part of the embedding
namespace.

For asymmetric embedding models, document and query preprocessing are distinct.
The reference Qwen profile uses its documented retrieval instruction for
queries; stored page documents are encoded as documents. Tests must catch an
accidental query/document inversion.

## 8. Retrieval pipeline

### 8.1 Stage A - authoritative eligibility

The planner resolves the allowed candidate universe before ranking:

1. current session and explicitly allowed project-memory scope;
2. canonical repository identity;
3. task and branch compatibility policy;
4. ACL and trust requirements;
5. active scope epoch;
6. `status = active`, unless the caller explicitly requests stale inspection;
7. requested page kinds and path boundaries.

For backends whose vector filter is not authoritative, the runtime over-fetches
and applies the same gates again in Go. Cross-session and cross-repository
leakage is a correctness failure, not a low retrieval score.

### 8.2 Stage B - candidate generation

Run both channels against the same eligible corpus:

- **Lexical:** FTS5 phrase/prefix/boolean matching with BM25 rank. This catches
  exact test names, paths, symbols, commands, error strings, and identifiers.
- **Dense:** cosine nearest-neighbor search over the active embedding namespace.
  This catches paraphrases and conceptually similar episodes.

Initial evaluation defaults are `k_lexical = 50` and `k_dense = 50`. These are
configuration values, not ABI promises.

When the embedder is unavailable, the dense channel reports degraded status
and lexical search continues. When FTS is unavailable, dense search may
continue, but exact identifiers still use direct resolution.

### 8.3 Stage C - reciprocal-rank fusion

Scores from different systems are not directly comparable. The default fusion
uses reciprocal rank fusion (RRF):

```text
rrf(page) = sum over channels 1 / (rrf_k + rank_channel(page))
```

The initial `rrf_k` is 60. Evaluation may tune it, but a checked-in profile and
stable ID tie-breaker make each run reproducible.

### 8.4 Stage D - deterministic policy rerank

Fusion establishes topical relevance. A separate policy layer then accounts
for:

- exact repo/task/branch/path affinity;
- current-failure signature overlap;
- user-pinned priority;
- source trust;
- last verification and invalidation risk;
- access frequency and recency;
- page salience;
- duplicate evidence and topic diversity;
- negative-transfer history for the page in similar scopes.

Hard scope and validity rules remain gates, never weights. The initial weights
must be named, logged, and evaluated; they must not be hidden in backend-specific
ranking expressions.

### 8.5 Stage E - diversity and caps

Use maximal marginal relevance or a deterministic equivalent to avoid returning
five pages for the same failure. Apply per-kind and per-source-event caps. A
normal semantic fault should return 1-3 materialization candidates, not a dump
of the nearest 50 pages.

### 8.6 Stage F - threshold and abstention

The retriever calibrates a minimum score/margin on held-out forced-reset tasks.
Below it, the system returns `SEMANTIC_PAGE_MISS`. Precision is more valuable
than recall when a false memory could revive a rejected approach or wrong-branch
assumption.

### 8.7 Stage G - exact materialization

For each selected page reference:

1. load current logical page metadata;
2. re-check scope epoch, status, trust, and source digest;
3. follow exact refs into WSL, ledger, artifacts, or authorized file slices;
4. apply normal validation and byte/token budgets;
5. admit the decoded page to L2;
6. render only the needed summary and identifiers into L1 or a fault response.

If any current check fails, suppress that candidate, record the reason, and try
the next candidate within budget. Do not serve the stale indexed copy.

## 9. Residency policy after retrieval

Retrieval and residency are different policies.

The initial L2 policy should use a CLOCK-Pro/2Q-inspired approximation:

- **Hot:** pages explicitly faulted or repeatedly used in the active task.
- **Cold:** newly materialized or prefetched pages with one access.
- **Test/ghost:** recently evicted identities retained without bodies to detect
  recurring demand and tune the hot/cold target.
- **Pinned:** hard constraints and active task anchors; these are scheduled by
  explicit policy, not similarity.

Each access sets a reference bit. The eviction hand skips pinned pages, clears
referenced pages once, demotes cold unused prefetch, and retains a bounded ghost
identity. Bodies can be reconstructed from L4. This captures recency and
frequency without letting an embedding score masquerade as a page lifetime.

The L1 scheduler then chooses from pinned records plus validated L2 pages under
the capsule budget. A page's admission to L2 does not guarantee L1 residency.

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
- `indexer` owns embedding calls, lexical/vector rows, and watermarks.
- `retrieval` owns filters, fusion, reranking, abstention, and explanations.
- `memory` owns L2 residency/access state.
- `scheduler` owns L1 admission and eviction policy.
- `faults` owns exact materialization and the external fault ABI.
- `harness` owns lifecycle integration and cancellation.

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
  sqlite-vec vec0 projection keyed to page version and embedding namespace

access_stats
  page_id, access_count, last_accessed_at,
  prefetched_count, useful_prefetch_count
```

The actual migration must reflect the APIs available in the pinned SQLite and
`sqlite-vec` versions. It must use prepared statements, explicit transactions,
foreign/logical integrity checks, and a migration version.

### 11.3 Why SQLite + FTS5 + sqlite-vec

- It matches the local-first, no-service deployment model.
- The Go runtime already uses the pure-Go `modernc.org/sqlite` stack.
- FTS5 is mature and strong for code identifiers and error strings.
- `modernc.org/sqlite` now distributes a transpiled `sqlite-vec` package, so an
  embedded path need not make Python authoritative or require CGO.
- `vec0` supports KNN and metadata-aware filtering suitable for the first L3
  corpus.
- A separate DB keeps all search state disposable.

The caveat is important: `sqlite-vec` documentation and releases remain
pre-1.0. Phase L3-2 must prove extension registration, persistence, filtering,
dimension handling, supported platforms, cancellation, corruption behavior,
and clean rebuild using the exact pinned dependency. Until that spike passes,
the reference backend plus FTS5 is the supported behavior.

At this decision point the scaffold pins `modernc.org/sqlite v1.53.0`, whose
module carries `sqlite-vec v0.1.9` and auto-registers it on supported targets
when `_ "modernc.org/sqlite/vec"` is imported. That makes the spike concrete;
it does not remove the gate or authorize adding the blank import to the first
demo.

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

Identical content reuses a vector. Page mutations are idempotent on
`(page_id, page_version, source_digest, namespace)`. Query embeddings may use a
small bounded TTL cache only after stripping session-specific identifiers from
the cache key where safe.

## 14. Indexing and consistency

### 14.1 Write-through truth, asynchronous cache

The foreground path is:

```text
append event to L4
  -> validate/apply deterministic WSL update
  -> return foreground result
```

The derivative path is:

```text
tail ledger sequence
  -> compile logical page mutation
  -> redact/normalize searchable text
  -> write FTS projection
  -> embed and write vector projection
  -> advance index watermark
```

An index error never rolls back or disguises a successful ledger append. It
leaves the watermark behind and exposes degraded health.

### 14.2 Crash recovery

On startup, the indexer compares each session's L4 high-water sequence with the
L3 watermark. It replays missing ranges through the deterministic compiler.
Page version and source digest make retries idempotent.

If schema, compiler, redaction, or embedding identity is incompatible, WSMS
creates a new index generation and rebuilds. It does not mutate an incompatible
generation in place.

### 14.3 Update and invalidation

New evidence never overwrites the old ledger event. It emits a new logical page
version or invalidation mutation. Index application is transactional per batch:
metadata, FTS, and vector projections must expose the same active page version
after commit.

At query time, current invalidation and scope state is checked again outside the
index. This closes the window where an asynchronous index has not yet processed
an invalidation.

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
| Embedder timeout/unavailable | Continue FTS-only; expose degraded reason |
| FTS failure but vector healthy | Dense candidate generation may continue; direct IDs unaffected |
| Entire L3 unavailable | L1/L2 current state and all direct L4 faults continue |
| Index watermark behind | Search old eligible pages, post-filter current invalidations, expose lag |
| Namespace mismatch | Refuse dense comparison; schedule/recommend rebuild |
| Malformed vector/dimension | Reject mutation or generation; never truncate/pad silently |
| Candidate fails materialization | Suppress it, record reason, try next within budget |
| No candidate above threshold | Return `SEMANTIC_PAGE_MISS` |
| Qdrant process exits | Circuit-break to embedded/FTS path if configured; L4 remains healthy |
| Index corruption | Quarantine generation and rebuild from L4 |

Search errors and semantic misses are different result types. Neither may be
presented as exact evidence.

## 17. Observability

Every semantic lookup produces an inspectable explanation without exposing
secret text:

- query mode, namespace, active filters, and candidate budgets;
- lexical and dense channel availability/latency;
- candidate IDs and per-channel ranks;
- fusion score and named rerank features;
- filter/suppression/abstention reasons;
- selected/materialized page IDs and exact refs;
- index generation and watermark lag;
- total embedding, search, materialization, and rendering latency;
- resulting L1 tokens and L2 residency transitions.

Aggregate metrics include:

- index pages/bytes by kind, repo, status, and namespace;
- compiler/index lag and failed mutations;
- semantic fault count, hit, miss, error, and abstention rates;
- Recall@k, MRR, nDCG, and exact-reference precision on labeled queries;
- stale-page suppression and wrong-scope candidate rates;
- negative-transfer and repeated-failure rate after retrieval;
- useful-prefetch ratio and pages evicted unused;
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

- Frozen stream and labeled queries:
  `testdata/pages/corpus/transport_fix/`.
- Page schema, `DeterministicCompiler`, and
  `ExactCosineSearch` / `ExactCosineSearchContext` live in `internal/pages`.
- Materialization gate: `ValidateMaterializable` (session, epoch, authority,
  source digest).
- No-L3 baseline remains the direct-ID demo. FTS-only baseline starts in L3-1.

### L3-1 - Interfaces and lexical path

**Status:** complete (Phase 7B). Dense materializer remains partial (no dense).

- `internal/indexer`: disposable FTS5 `warm.db`, Apply, SearchLexical, Rebuild,
  watermarks.
- `internal/retrieval`: `QueryIntent`, lexical `ResolveSemantic`,
  `SEMANTIC_PAGE_MISS`.
- Harness best-effort index apply after events; `SemanticSearch` for tests.
- Delete `index/`, rebuild cutover, cross-session isolation, and corpus FTS
  labels are covered by package tests.

### L3-2 - sqlite-vec compatibility spike

- Pin exact modernc/sqlite and sqlite-vec versions.
- Prove extension initialization on every supported platform.
- Test KNN, cosine distance, metadata filters, cancellation, concurrent reads,
  batch replacement, malformed dimensions, restart, and corruption handling.
- Compare results with the exact oracle and record recall/latency.
- Keep this behind a feature/config gate until the spike passes.

### L3-3 - Local embedding adapter

- Implement namespace/profile types and document/query separation.
- Add the local Qwen3 profile through a supervised adapter.
- Add redaction, batching, deadlines, circuit breaking, self-checks, and caches.
- Make FTS fallback visible and deterministic.

### L3-4 - Hybrid semantic faults

- Implement parallel lexical/dense candidate generation, RRF, policy rerank,
  diversity, thresholding, and explanations.
- Expose an explicit semantic-fault tool first.
- Materialize exact evidence through the existing resolver.
- Do not enable automatic L1 injection yet.

### L3-5 - Unix-style residency and prefetch

- Add hot/cold/ghost residency metadata and bounded CLOCK-Pro/2Q behavior.
- Add reference bits, useful-prefetch accounting, thrash telemetry, and L1
  admission separation.
- Run prefetch in shadow mode before it affects L2/L1.

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
- [LanceDB hybrid search](https://docs.lancedb.com/search/hybrid-search) and its
  [Go package](https://pkg.go.dev/github.com/lancedb/lancedb-go/pkg/lancedb)
  support its status as a viable but non-default future adapter.

## 22. Final architectural rule

L3 helps WSMS discover *which address may matter*. L4 proves *what happened*.
L2 holds the validated page. L1 receives only the bounded part needed now.

That division is the reason vector retrieval strengthens the Unix memory model
instead of replacing it with an opaque similarity database.
