# WSMS Forced-Reset Benchmark

Phase 10 design. This document specifies the evaluation system that decides
whether WSMS's memory mechanisms actually work: matched conditions resumed from
byte-identical frozen pre-reset state, programmatic oracles over nonce-bearing
synthetic scenarios, pre-registered decision gates, and a hard split between an
offline keyless tier (buildable and CI-tested now) and real-model tranches
(gated on served models, budget, and owner ratification).

Status: **design**. No benchmark API exists yet; measurement is gated per the
owner's Phase 10 hold. Nothing in this document is a result.

## 1. Decision summary

The benchmark exists to execute three owner-ratified decisions, not to produce
comparative marketing numbers:

- **D1 — WSL keep/simplify/reject.** Keep WSL only if it beats the strongest
  structured (YAML/Markdown) baseline on held-out continuation success at
  equal-or-lower measured token cost. Only the **format layer** is on trial:
  the ledger, scheduler, coherence gating, and fault tools are not. A null or
  negative result mandates renderer replacement atop the unchanged mechanism
  stack, and the report must state this scoping so a null D1 is not misread as
  whole-system failure.
- **D2 — semantic ladder promotion.** Explicit fault tool (shipped) →
  L2-only prefetch → automatic L1 admission. Each rung promotes only on
  held-out end-to-end improvement, or equal success at lower token cost, plus
  zero-tolerance safety suites.
- **D3 — instrument validity.** Decide whether the local open-weights stratum
  is a valid co-primary decision instrument or dev-only.

Method in one paragraph: every condition resumes from the identical frozen
pre-reset ledger and working tree; the resume artifact is the only channel
that differs. Scenarios are synthetic, nonce-bearing, and oracle-checked
programmatically, so continuation success is a substring/execution fact, not a
judge opinion. All confirmatory statistics are paired and cluster-aware. The
entire runner — scenarios, conditions, scorers, statistics — is proven keyless
in CI on a deterministic scripted actor before a single hosted token is spent.
Until held-out results exist, the only permitted register for any claim is
"hypothesis"; afterwards, the only sanctioned headline is the pre-registered
claim template with measured values and confidence intervals, negative results
at equal prominence.

Acceptance criteria for this document itself:

- Every gate below is mechanically evaluable from frozen artifacts.
- Every metric names a measurement path in the current codebase or a named new
  seam (section 10).
- Every owner-only decision appears in the ratification checklist (section 14)
  or the open questions (section 18).

## 2. Benchmark invariants

### B-I1 — One differing channel

All conditions resume from byte-identical frozen pre-reset state. The resume
artifact (and the arm's registered memory tools) is the only input that may
differ across conditions. Injection role and position are identical.

### B-I2 — Ledger truth survives every condition

The frozen ledger and content-addressed artifacts always exist on disk as the
scoring record. Under non-WSMS conditions the core still ingests resume turns
(for uniform metric extraction) but injects nothing and registers no memory
tools unless the condition table says otherwise — no covert channel.

### B-I3 — Programmatic oracles gate; judges never do

Every gate path is computed from deterministic oracles. An LLM judge may score
judge-eligible probes for reporting, but every gate must also pass on the
programmatic-only variant of its metric. A judge can never flip a gate.

### B-I4 — Contamination-proof by construction

Every identifier, error string, and path in a scenario carries a
generator-random 8-hex nonce. Success requires recalling session-specific
strings that cannot exist in any model's training data. No real OSS repos in
the primary endpoint.

### B-I5 — Paired or it does not count

Confirmatory contrasts are within-scenario, on the same frozen pre-reset
state. Unpaired comparisons are descriptive only.

### B-I6 — Pre-registration before held-out

Gate rules, margins, sample sizes, expansion rules, model pins, judge hashes,
and the owner's ratified WSL pre-commitment are committed and git-tagged
before the held-out corpus is generated; `HELDOUT.sha256` is committed in a
sealed-manifest addendum tag immediately after generation; nothing held-out
executes before ratification (G1). Deviations require a new tag and are
reported. Post-hoc analyses are labeled exploratory.

### B-I7 — Mock evidence is never decision evidence

Runs on the scripted actor or echo mock are stamped `evidence:false` in their
manifests, and the gate evaluator mechanically refuses them. Attestation is
per-message (section 9), not per-flag.

### B-I8 — Replay equivalence is a hard oracle

Before the first resume turn of every arm, the reopened session must
reproduce the frozen serialized state, provenance map, capsule, and event
count byte-identically. A mismatch marks the episode INVALID (runner defect,
never agent failure) and halts the arm.

### B-I9 — Equal measured budgets

Bounded resume artifacts and per-turn injections are budget-enforced in
measured tokens from one pinned, vendored, offline tokenizer — identical
across strata and CI — with post-hoc validation against each stratum's true
tokenizer. The byte cap is an anti-abuse backstop set high enough that the
token cap is always the binding constraint, and the linter asserts it was.

### B-I10 — Leakage is linted in both polarities

Solution-class strings (anything that satisfies a continuation oracle) may
appear nowhere: not in pre-reset turns, not in any resume artifact, not in
probe text, not in reminder templates, not in the frozen fixture tree.
Recall-class strings (nonced evidence that legitimately occurred pre-reset)
must appear in pre-reset events, and the scorer records, for every bounded
gate-relevant arm (C1 reference/sensitivity arms exempt; §4.4), whether the
string was present in that arm's injected artifact or required retrieval.

### B-I11 — Fail-stop is an incident, not a data point

A poisoned ledger or runner defect aborts the arm as INVALID with a
pre-registered handling rule. INVALID rates are reported per arm and per pair;
differential invalidity triggers a worst-case sensitivity analysis that can
override the primary result (§8.7).

### B-I12 — Credential and egress policy

Credentials env-var-only; no secret in any committed or frozen file (provider
config records env-var names only). Hosted traffic flows through a
deny-by-default redaction/egress-allowlist proxy enforced at the network
layer, with an isolated agent config dir so no ambient credential store can
bypass it. The offline no-key path is the default.

## 3. Conditions and baselines

All bounded artifacts obey `B_resume` (proposed: 1,024 measured tokens;
owner-ratified) plus a 16 KiB anti-abuse byte backstop, and mirror the FR-008
hard-constraint overflow exception in every renderer. The resume artifact is
installed as the first system message in every arm (the pi bridge gains
`WSMS_INJECT_ROLE=system` to close the current system-vs-user asymmetry).

Every arm receives the identical **coding toolset** (read/write/edit/bash in a
no-network sandbox over the arm's copied fixture tree). The "memory tools"
column below governs `wsms_read_page`/`wsms_recall` only.

| id | resume substrate | memory tools | budget-matched | role |
|---|---|---|---|---|
| C1-FULL | verbatim transcript reconstructed from the ledger **with every artifact ref dereferenced** to its full stored body | none | no (lossless upper anchor) | reference only |
| C1-TRIM | the dereferenced transcript, tail-trimmed newest-first to `B_resume` | none | yes | sensitivity floor |
| C2-COMPACT | pi `compact` output (preserve-verbatim instructions) | none | no (provider-controlled) | descriptive only, real-model-gated, "where available" |
| C3-SUM | frontier best-of-N summary of the dereferenced transcript (extraction-only prompt; probe-blind selection) | none | yes | ladder rung |
| C4-YAML-det / C5-MD-det | deterministic render of the same coherence-filtered selection the WSL renderer consumes (formalized as `L1Selection`; same fields, greedy drop order, FR-008 exception) | per-arm (see STRUCT*) | yes | STRUCT candidates |
| C4-YAML-eng / C5-MD-eng | frontier best-of-N engineered checkpoint from the dereferenced transcript, human anti-sandbag audited | per-arm (see STRUCT*) | yes | STRUCT candidates |
| STRUCT* | the pre-named strongest of the four STRUCT candidates (§8.8) | **with** read_page + recall | yes | **D1 opponent** |
| C6-RETR | pointer line only; memory via tools against the replayed session | tools only (metered) | yes | diagnostic |
| W-NOSCHED | capsule from raw unfiltered WorkingState (`SchedulerMode=off`: no coherence gating, pinning, or invalidation shootdown) | none | yes | ablation, train split only |
| W-SCHED | full BeforeTurn capsule | none | yes | G2a baseline |
| W-FAULT | full capsule + read_page + recall | tools | yes | **primary WSL arm** |
| W-PREFETCH | W-FAULT + L2-only speculative body admission (new mode; L1 untouched) | tools | yes | ladder rung 2, gated |
| W-L1AUTO | W-PREFETCH + auto-admitted semantic block, placed last in the greedy drop order inside the unchanged budget | tools | yes | ladder rung 3, gated |

Normative condition rules:

1. **Artifact dereferencing (blocking).** The session's append path truncates
   oversized output payloads above `ArtifactThresholdBytes` to a 200-byte stub
   plus `ArtifactHash`/`raw` artifact ref (`Session.Append` in
   `internal/harness/session.go`). The transcript
   reconstructor MUST dereference every artifact ref to the full
   content-addressed body when building C1-FULL and the generation inputs for
   C3-SUM and the engineered C4/C5 variants; C1-TRIM trims the dereferenced
   stream. Freeze-blocking acceptance test: every oracle nonce present
   anywhere in the frozen artifacts dir appears byte-verbatim in the C1-FULL
   reconstruction. Without this, every transcript-derived baseline is blinded
   to exactly the evidence W-FAULT can page-fault back, rigging D1.
2. **Fault-instruction parity.** The page-fault instruction currently appended
   unconditionally to every WSL capsule becomes a renderer parameter
   conditioned on the arm's tool registration: emitted with verbatim-identical
   text by the WSL, YAML-det, and MD-det renderers when memory tools are
   registered; omitted everywhere when not. W-SCHED therefore does not
   instruct the model to use tools it lacks (which would corrupt the
   fault-tool-value contrast), and STRUCT* carries the same nudge and the same
   page IDs (avoids+refs, failure IDs) as WSL. The golden
   information-equivalence test asserts field content, instruction text, and
   page-ID presence are identical across the three deterministic formats under
   the same tool config. Engineered variants receive the same instruction plus
   a budget-counted ID index.
3. **STRUCT candidate calibration parity.** The four STRUCT candidates are
   calibrated WITH read_page + recall registered — identical to the held-out
   STRUCT* arm — so the opponent is selected under the condition it is tested
   in. The `argmax` metric is pinned in the prereg: continuation success on
   the calibration split, ties broken by lower measured tokens.
4. **Engineered-variant cost honesty.** Best-of-N generation tokens are
   reported alongside, and applied as a sensitivity to, the D1 token
   comparison. Selection among the N candidates is probe-blind: a fixed
   extraction-completeness rubric that never sees or executes probe text.
   Every generated artifact passes the solution-class leakage lint plus a
   novel-content check (checkpoint lines must have a supporting span in the
   source transcript) before any run consumes it; failures are regenerated
   with a tightened extraction-only prompt, all regenerations logged. If an
   engineered variant is STRUCT* and wins D1, the pre-registered outcome is
   "reject the format layer, adopt the LLM-checkpoint condition" with its
   generation cost included in the shipped-config accounting — only the
   deterministic variants define an adoptable renderer for SIMPLIFY.
5. **C2 mechanism.** Phase A is scripted through the Session appenders with no
   pi involvement, so a live pi session has nothing to compact at reset time.
   C2 exists only if a session-seeding seam is built (replaying the scripted
   pre-reset turns through the scripted-actor provider into a live pi session
   before issuing `compact`); otherwise it is dropped to a footnote. Either
   way it is descriptive-only: provider compaction cannot be token-matched.
6. **L3 sub-variants** (config-only switches under W-FAULT): (a) no-L3
   (read_page by ID only), (b) FTS-only (`Embedder` unset — note
   `DenseDimensions=0` with an embedder configured is auto-upgraded to the
   embedder's namespace dimensions by `configureEmbedder` in
   `internal/harness/session.go`, so zero does not disable dense), (c)
   dense-only, (d) hybrid RRF, (e) hybrid + policy rerank + abstention
   (default). Variants c–e are gated on the pinned real embedder run
   (GATED-9).
7. **Model axis.** Hosted frontier (pinned ID, behind the egress proxy) and
   local open-weights coder (pinned weights, greedy) run the gate-relevant
   arms. The scripted mock actor runs the entire matrix in CI, permanently
   `evidence:false`.
8. **Budget-parity audit (blocks held-out).** On the training split, the
   per-turn measured injected-token distributions of the gate-relevant format
   arms (W-*, C4-YAML-det, C5-MD-det) must agree within a pre-registered
   tolerance (median within 5%), computed from the dual counts frozen in
   `capsules.jsonl`. On failure, `CapsuleTokenBudget` is re-expressed in
   measured tokens (section 14 item 2) under a new prereg tag before any
   held-out run.

## 4. Scenario suite, injection protocol, and splits

### 4.1 Families

Ten seeded template families × 16 seeds = 160 frozen scenarios, each targeting
named spec pathologies (specification.md §4):

| family | trap | pathology |
|---|---|---|
| F1 flaky-test-fix | repeated-failure | repeating a failed command/patch |
| F2 hard-constraint-refactor | "do not X" correction | losing a user correction |
| F3 exact-error-chase | verbatim recall | remembering *that* it failed, losing the exact error |
| F4 branch-switch | wrong-scope lookalike | reusing state from the wrong branch |
| F5 invalidated-assumption | stale revival | reviving an invalidated assumption |
| F6 big-output-crowding | artifact offload | large output crowding out state; recall must hit the offloaded artifact |
| F7 multi-failure-triage | interleaved failures | losing the next executable action |
| F8 decision-with-avoid | avoid-list discipline | retrying a rejected approach |
| F9 rename-churn | path exactness | stale paths after renames |
| F10 no-answer/adversarial | must-abstain + poisoning probes | fabrication under absence; malicious imperatives in tool text |

The initial build MAY start with six families (F1, F2, F3, F5, F9, F10) to
control schedule, but all ten must exist before the held-out corpus is sealed.

### 4.2 Scenario format

`bench/scenarios/<family>/<family>-<seed>.episode.jsonl`: ordered typed
records mapping 1:1 onto the Session ingest API (`task_start`, `ingest_user`,
`ingest_assistant` — the pre-reset assistant side is scripted too —
`ingest_command{cmd,exit,output}`, `decision`, `next_action`, `branch_change`,
`invalidate`, file events), every event with pinned RFC3339 timestamps;
exactly one reset marker; then 6–8 probe records, each with a programmatic
oracle spec (`success_regexes`, `forbidden_commands`,
`required_nonce_substrings`, `stale_patterns`, `expected_page_ids`,
`must_abstain`, `reminder_trigger`, and a named fixture test where the family
defines one), plus a small fixture `repo.tar.zst`.

Injection invariants, linted per scenario: ≥8 pre-reset turns (target 20–40
events), ≥2 failed commands/patches, ≥1 explicit user correction, ≥1 branch or
file change, ≥1 oversized output forcing artifact offload, ≥1
`memory_invalidated`. Distractor noise is generated at two pre-registered
levels (standard, heavy) to give the negative-transfer contrast.

Exact-recall oracle targets must be facts **absent from the final frozen
capsule** (an earlier failure, not the LAST FAILURE block) — enforced as a
generator lint — so recall genuinely requires memory beyond the resting
injected artifact.

### 4.3 Labeled L3 query set

Each scenario emits 6–15 labeled queries (per `l3-warm-memory.md` §18.2):
exact identifier/error/path, paraphrase, multi-hop decision, wrong-branch
lookalike, superseded/invalidated evidence, malicious-imperative-in-tool-text,
and true no-answer must-abstain — with gold page-ID/scope labels emitted by
the generator itself (exact by construction; 20% hand-audited on the training
split).

### 4.4 Corpus admission gates

All three block scenario freeze:

1. **Oracle sanity.** A scripted perfect-agent must PASS and a scripted
   amnesiac-agent must FAIL every scenario in the offline tier.
2. **Disk-grepper.** A third scripted agent — amnesiac plus exhaustive tree
   grep — must also FAIL, proving oracles are not satisfiable from the fixture
   tree. The generator routes failed-command output to ephemeral paths
   excluded from the freeze, and the linter greps the frozen tree (including
   `.git` objects, caches, and files written by scripted commands) for both
   oracle classes.
3. **Leakage linter** (`wsms bench lint`), two polarities per B-I10:
   - *solution-class* strings forbidden in every pre-reset turn, every resume
     artifact of every condition, every probe user-message, every reminder
     template, and the fallback policy text;
   - *recall-class* strings asserted present in pre-reset events; bounded
     gate-relevant artifacts (STRUCT candidates, C3-SUM, W-* capsules) are
     scored with a per-arm flag for whether the string was already in the
     injected artifact; C1-FULL/C1-TRIM are exempt by design (reference and
     sensitivity arms, never gate opponents).
   Probes must require composing pre-reset facts, never echoing artifact text.

### 4.5 Splits

Two families are held-out-only (designated at seal time; owner ratifies); the
remaining eight split seeds 8 train / 8 held-out. Training/calibration = 64
episodes; held-out = 8×8 + 2×16 = 96 base episodes. Because seeds within a
family are near-clones, statistical power scales with families, not seeds
(§8.2): the pre-registered expansion rule adds **families** (blocks of 2
new families × 16 seeds), never more seeds inside existing families.

All tuning — prompts, thresholds, judge, distractor sweeps, STRUCT* selection
— uses the training split only. Held-out episodes are generated, SHA-256
manifested (`bench/scenarios/HELDOUT.sha256` committed), stored **out of
repo** in a sealed archive (bodies never enter CI logs or golden trees), and
never executed against any real model until ratification. A held-out
oracle-sanity failure fixes the generator and regenerates the entire affected
seed block — never a single scenario — with a new manifest and a logged
deviation entry.

## 5. Reset protocol

**Phase A (pre-reset; once per scenario; condition-independent).** The runner
plays the scripted episode — both user and assistant sides — through the typed
Session appenders with runner-pinned timestamps, so the frozen pre-reset
ledger is byte-identical across all conditions, models, and replicates. Zero
model tokens are spent pre-reset. At the reset marker the runner drains
maintenance (`WaitForMaintenance` barrier), freezes the ledger (`wsms export`
JSONL), the content-addressed artifacts dir, the final capsule, the residency
snapshot, and the fixture working tree, then closes the session.

**Phase B (resume; per condition × replicate).**

1. Copy the frozen DataDir into a fresh per-arm directory, placed strictly
   outside the agent's sandbox root and denied by the bash tool's path policy
   (asserted per episode) — the ledger and raw artifacts must not be
   grep-recoverable "memory".
2. Model context is destroyed unconditionally: pi `new_session` for pi-driven
   arms, a fresh message list for the in-process driver. C2-COMPACT alone
   issues `compact` instead.
3. `harness.OpenSession` on the copy performs deterministic replay; the replay
   equivalence oracle (B-I8) must pass.
4. The condition's resume artifact is installed as the first system message.
5. Up to 8 scripted probe turns run with the condition's tool registration,
   ending early on oracle success or exhaustion of the 3-reminder budget.

Capsule/checkpoint arms re-render per turn (BeforeTurn semantics);
transcript/summary arms keep their frozen artifact plus accumulating turns.

**Scripted-user reminder policy.** Deterministic: a reminder fires iff a
probe's `reminder_trigger` predicate matches (the agent asks for durable info,
violates a constraint, or proposes a forbidden command). Reminder templates
are pointer-style — "you appear to be violating a recorded constraint;
re-check your working state" — with zero oracle-string content (linted, per
B-I10), so a reminder can prompt recall but never supply the answer. A bounded
fallback ("proceed with your best judgment") handles unanticipated questions.
The policy is identical across conditions, making `human_reminder_count` a
pure function of agent behavior.

**Failure handling.** A poisoned ledger (derivation failure) aborts the arm as
an INVALID incident (arm failure, never agent failure). More than 10% INVALID
in any arm voids the run; differential invalidity between paired arms triggers
the sensitivity rule in §8.7.

## 6. Metric definitions and measurement procedures

Every metric names its data source in the frozen artifacts (section 9). No
metric is defined in terms of WSMS-internal quality signals: everything scores
agent behavior against scenario ground truth.

### 6.1 Primary endpoint

`continuation_success` (binary per episode-replicate): all scenario oracles
satisfied within 8 post-reset turns and ≤3 reminders. Measured
programmatically — nonce substring/regex evaluation over frozen assistant
messages, tool-call arguments, and command events — plus actual execution of
the fixture's named test against the arm's final repo tree where the family
defines one. Judge fallback only for probes marked judge-eligible at authoring
time, logged separately, and never gate-deciding (B-I3).

### 6.2 Primary suite (all programmatic)

- `repeated_failed_attempt_rate` — normalized command-argv + patch-hunk
  fingerprints matched against pre-reset failed attempts.
- `lost_hard_constraint_rate` — violation predicates over commands, diffs, and
  tool-call args; verbal compliance with behavioral violation scores as
  violation.
- `exact_error_recall` — verbatim nonce substring of command AND error;
  paraphrase scores 0; the SHA-256-verified raw artifact is the reference.
- `stale_assumption_recurrence` — invalidated-nonce match in assistant output
  or commands.
- `human_reminder_count` — deterministic runner counter.
- `next_action_fidelity` — first substantive action matches the recorded
  `next_action` within 2 turns.
- `tokens_per_successful_continuation` — real usage decoded from pi
  `message_end` / `get_session_stats`; resume-artifact tokens reported as a
  separate component; mock runs record estimator tokens and are never
  gate-eligible.
- `page_fault_precision_recall` — the runner fault log and durable
  `page_access` ledger appends joined against `expected_page_ids`; PAGE_MISS
  counted separately.
- `invalid_wsl_update_rate` — a new non-fatal lint-rejection counter per 100
  events; fail-stop behavior preserved and counted as incidents.

### 6.3 L3 suite (programmatic, over the retrieval trace vs generator gold labels, per variant)

Recall@1/3/10, MRR, nDCG@10 (candidate trace with final positions);
`exact_reference_precision` (materialized evidence refs vs gold event IDs);
`abstention_quality` (true-abstain on must-abstain, false-abstain on
answerable; operational `ErrIndexUnavailable` strictly excluded per the FR-016
taxonomy); `wrong_scope_stale_revival` (selected-candidate scope/status vs
labels; any invalidated materialization is a gate-blocking incident);
`negative_transfer`, measured two pre-registered ways — (a) paired incident
counting: the with-recall arm fails where its no-L3 twin passes and a
wrong-scope/stale page preceded the diverging action; (b) noise contrast:
success drop under heavy distractors with L3 minus without;
`useful_prefetch_ratio` (shadow-useful / shadow-observations pre-promotion;
prefetched-pages-later-hit post-promotion); `tokens_per_useful_page`
(materialization tokens / pages later referenced by a successful oracle).

### 6.4 Secondary and descriptive

No gate consumes these except the two SLOs:

- Latency p50/p95/p99 for BeforeTurn, PageFault, SemanticSearch, Append via
  new timing middleware. SLO checks: p95 < 75 ms post-embedding retrieval;
  p95 < 350 ms full local semantic fault.
- `context_token_savings` vs C1-FULL.
- Index/rebuild cost (timers around index rebuild + replay; health counts).
- Cache thrash (ghost-thrash and refault-distance counter deltas).
- Poisoning incidents (planted canary imperatives grepped in frozen capsules
  and actions).
- Observer CPU (descriptive only).
- Background scheduling cost — per-maintenance-cycle durations from the
  `WaitForMaintenance` drain barrier plus observer-queue timings, emitted into
  `timings.jsonl` (named here so the metric-coverage claim stays true).
- Wall time to next verified action — elapsed time from the first resume turn
  to the first probe-oracle-passing action, computed from `timings.jsonl` and
  `transcript.jsonl` timestamps; descriptive only (model/API latency
  dominates; no gate consumes it).

## 7. Scoring and judging

**Programmatic-first, judge-last.** Deterministic oracles cover 100% of every
gate path; target ≥80% of scenarios with fully pattern/execution-reducible
continuation contracts. Judge-required scenarios need written justification at
authoring time.

**Dual-scorer cross-check (abort gate).** The gate-path oracles —
continuation success, constraint/repeat/stale/exact-recall — are implemented
twice (Go `wsms bench score`, Python `research/scorer.py`) and must agree
exactly on the training split before any held-out scoring; disagreement halts
the run. Non-gate metrics may be single-implementation, with the Python side
consuming frozen per-check primitives.

**Judge protocol (narrow).** Different model family from every agent under
test; temperature 0; frozen rubric prompt hash-committed at prereg; 3-vote
self-consistency; blinded — sees only the probe question, the
format-normalized post-reset answer/actions, and gold facts; never the resume
artifact, condition name, or pre-reset transcript, with WSL/YAML/Markdown
style markers stripped by a normalizer.

**Blinding probe.** A classifier trained on judge inputs must fail to predict
condition better than chance (p > 0.1) before any judge score is reported.

**Calibration.** 50–100 training-split judge-eligible items double-labeled by
two humans (owner + one recruit; disagreements adjudicated); Cohen's κ ≥ 0.8
required per check type; below threshold, affected checks fall back to
programmatic-only (or full human scoring of held-out escalations) and the
limitation is reported. A 10% stratified human audit of held-out judge
verdicts is reported as the judge error rate.

**Anti-gaming guards.** Required substrings evaluated over the assistant's
final answer segment only (not tool echoes); forbidden-command matching
includes tool-call arguments; negation-window heuristics validated on the
training split; adversarial scorer fixtures (a verbose model mentioning a
forbidden command while refusing it; success only after a reminder, verifying
the oracle still requires recall the reminder did not supply) live in the
scorer test suite.

**Baseline anti-sandbag audit.** A 20-episode human audit of C3 and engineered
C4/C5 generations against a checklist (all hard constraints verbatim? last
failure exact? no oracle-string leakage? no novel solution content?). Failing
baselines are regenerated, never silently kept weak.

**Aggregation.** The episode-replicate is the scoring unit. The per-stratum
replicate policy (§8.3) defines the primary success bit.

## 8. Statistical protocol

### 8.1 Design

Fully paired: every condition resumes from the identical frozen Phase-A state
per scenario; all confirmatory contrasts are within-scenario. Variance is
confined to post-reset model sampling.

### 8.2 Clustering is first-class

Seeds within a family are near-clones, so episode outcomes are correlated
within family and the effective sample size is governed by families, not
episodes. Consequences, all normative:

- The **primary inference for every confirmatory contrast is the clustered
  paired bootstrap** (resample families, then seeds within family; 10k
  resamples). Exact McNemar is reported as sensitivity only.
- `research/power.py` computes power with a design effect estimated from
  training-split within-family ICC (`deff = 1 + (m−1)·ICC`).
- Sample expansion adds families, never seeds within existing families
  (section 4.5).

### 8.3 Replicates (per-stratum policy, pre-registered)

- Local stratum, greedy decoding: bit-deterministic ⇒ **1 replicate**, no
  majority vote, no escalation rule.
- Hosted stratum: the GATED-8 smoke run measures the actual run-to-run flip
  rate (~21 episode-arms: 7 training scenarios × 3 arms, each rerun once —
  the same run GATED-8 names); the replicate count is set from
  measured discordance. If discordance < 5%, run 1 replicate and reallocate
  the saved spend to held-out families. If replicates are used, the primary
  bit is majority-over-replicates, and power is computed on that aggregated
  bit — the same random variable that is analyzed.

### 8.4 Samples and power

Held-out ≈ 96–128 episodes across ≥10 families per gate-relevant arm per
stratum. Exact-binomial baseline (independence) plus the design-effect
adjustment are both committed with the prereg. If the training split predicts
effective power < 0.8 for a 12–15 pp paired difference, held-out expands by
pre-registered family blocks before ratification, never after unblinding.

### 8.5 Confirmatory family

Exactly three contrasts, Holm-Bonferroni at family α = 0.05:

- **H1 — WSL gate:** W-FAULT vs STRUCT*, continuation success. Two one-sided
  tests (superiority in each direction), each at the Holm-adjusted level, so a
  REJECT verdict is actually testable.
- **H2 — fault-tool value:** W-FAULT vs W-SCHED.
- **H3 — hybrid vs FTS-only:** confirmatory pair = exact-reference precision
  AND continuation success (the same pair G2-pre gates on; section 13).

Ladder gates G2b/G2c are sequentially gated (tested only if prerequisites
pass) and need no cross-family correction. Everything else is
estimation-only: clustered-bootstrap 95% CIs, Wilcoxon signed-rank +
Hodges-Lehmann shifts for counts, mixed-effects logistic/Poisson (family
random intercept) as sensitivity. No p-values outside the family.

### 8.6 Margins and the token rule (single definitions)

- Success non-inferiority margin: clustered-bootstrap CI lower bound of the
  paired difference > −5 pp.
- Token comparison (used by every gate that mentions tokens): per-episode
  paired token ratio on episodes where **both** arms succeed (sensitivity:
  all episodes), summarized as the median with a clustered-bootstrap 95% CI.
  "Equal-or-lower" ⇔ CI upper bound ≤ 1.05. "Token superiority" ⇔ ≥10% lower
  median with CI excluding 1.0.
- Every non-regression side condition names its estimator, margin, and CI
  rule (e.g. stale-revival: CI upper bound of the delta < +1 pp). Zero-count
  safety criteria are reported with the rule-of-three upper bound at the
  achieved n, so "zero incidents" is never read as "rate = 0".

### 8.7 INVALID episodes

Pairwise exclusion with counts reported per arm and per pair, plus a mandatory
worst-case sensitivity scoring each INVALID as failure for the arm that caused
it. If the gate verdict flips between exclusion and worst-case imputation, the
verdict is "no decision, investigate". If arm INVALID rates within a contrast
differ by more than 3 pp, the worst-case sensitivity becomes primary.

### 8.8 STRUCT* robustness (winner's-curse guard)

STRUCT* is selected on the calibration split via family-blocked
cross-validation, pre-named before held-out runs, and remains the inferential
opponent. All four STRUCT variants also run on held-out (estimation-only, no
new confirmatory contrasts). KEEP additionally requires W-FAULT's held-out
paired CI vs **every** structured variant to exclude a loss; if any variant
beats W-FAULT descriptively, the verdict downgrades to SIMPLIFY and the report
names that variant as the strongest baseline. The selection margin is recorded
in the prereg addendum.

### 8.9 Pre-registration

`PREREGISTRATION.md` — hypotheses and directions, exact gate rules (verbatim
from section 13), the 3-contrast family, margins, per-stratum replicate
policy, sample sizes and family-expansion rules, split seeds, the STRUCT*
selection rule, judge model+prompt hashes, scorer SHAs, model pins, tranche
cost table, budget-degradation ordering, and the owner's formally ratified WSL
pre-commitment — committed and git-tagged (`bench-prereg-v1`) before held-out
generation. `HELDOUT.sha256` is committed in a sealed-manifest addendum tag
immediately after generation (B-I6); execution stays blocked until G1.

## 9. Artifact freezing and replay

**Content-addressed run store.** Runs keyed by
`sha256(scenario-hash × condition-hash × driver × model-pin × seed)`; results
stored separately under `results/<run-id>/<scorer-version>/` so re-scoring is
additive and never re-runs episodes; a judge-model change forces a visible
scorer-version bump.

**Frozen per episode-arm:** `episode.jsonl` (resolved scenario, SHA-256);
Phase-A frozen state (ledger export JSONL + content-addressed artifacts dir +
SHA manifest; one canonical copy shared across arms by hash reference);
`resume_artifact.txt` with pinned-tokenizer AND per-stratum measured token
counts; `capsules.jsonl` (every per-turn injected artifact, with both token
counts per turn — runner-side capture, since the core keeps only the latest
capsule); `transcript.jsonl` (pi `get_messages` / driver log incl. tool
calls); `usage.jsonl` (per-message tokens AND the provider-attested model id
and request id); `faults.jsonl` (every page/semantic call with the full
retrieval trace, budgets, PAGE_MISS/abstention reasons); `residency.jsonl`
(per-turn snapshot diffs, redaction-asserted body-free); `timings.jsonl`;
`outcome.json` (oracle verdicts + judge votes).

**Run-level `manifest.json`:** git SHA, wsms binary hash, pi lockfile hash,
exact model pins (hosted ID + local weights SHA), prereg tag, seeds, condition
matrix, host fingerprint, the environment attestation (item 1 of the evidence
attestation below), and `evidence:{true,false}`.

**Evidence attestation (three mechanical checks, per B-I7):**

1. For any `evidence:true` run, the runner scrubs and asserts the absence of
   `WSMS_MOCK_MODEL` and `WSMS_BENCH_ACTOR` in the exact environment passed to
   pi and the in-process driver, recording the assertion in the manifest.
2. `usage.jsonl` records the provider-attested model id per message; the gate
   evaluator hard-rejects any run where any per-message id differs from the
   manifest pin.
3. The scripted actor is forbidden by construction from registering under a
   non-mock model id (asserted in the bridge, tested in CI).

**Replay/verification.** `wsms bench verify <run-id>` re-imports the frozen
ledger via a new `wsms import` (inverse of export), opens the session, asserts
the equivalence oracle, SHA-256 re-verifies raw artifacts, and re-derives
outcome bytes with both scorers asserting exact agreement — turning the
deterministic-replay invariant into a CI check rather than a claim.

**Storage policy.** In-repo: frozen training corpus, one full mock-run golden
results tree (diffed in CI), `HELDOUT.sha256`, `PREREGISTRATION.md`,
`RATIFIED.sig`. Out-of-repo: held-out scenario bodies and large results trees,
archived with committed manifests. The repo stays private until the
reproducibility bundle is published (GATED-13). No secrets serialize anywhere:
provider config records env-var names only.

## 10. Runner architecture

New Go packages:

- `internal/bench/scenario` — episode schema, loader, seeded generators with
  the nonce engine, leakage linter (both polarities, incl. fixture-tree and
  scripted-user channels), fixture materializer, oracle sanity harness
  (perfect / amnesiac / disk-grepper agents).
- `internal/bench/conditions` — condition-as-data from
  `bench/conditions/*.yaml`: substrate renderer, memory-tool registration,
  config overrides, budgets; the dereferencing transcript reconstructor;
  summary/compaction loaders.
- `internal/bench/runner` — Phase-A/B executor, reset orchestration + replay
  oracle, scripted-user reminder engine, per-turn capture, token ledger,
  timing middleware, per-episode sandbox management.
- `internal/bench/agentloop` — the in-process agentic loop (a major new
  component, not a wiring job): a typed tool-calling interface extending the
  current one-shot `harness.Client`, a tool registry shared with the pi
  bridge's tool definitions, multi-step tool dispatch, per-turn capsule
  re-injection, transcript/usage capture. Driver parity (below) is the
  acceptance test.
- `internal/bench/score` — programmatic oracles, IR metrics over the retrieval
  trace, judge client (env-key, default-off).
- `internal/bench/egress` — redaction + deny-by-default allowlist proxy for
  the hosted path, enforced at the network layer (container netns or macOS pf
  rules), with an isolated agent HOME/config dir so pi's own credential store
  cannot bypass it, and a CI canary asserting a benchmark pi run with a
  planted decoy credential file makes zero non-allowlisted connections.
- `research/` — Python: `scorer.py`, `stats.py`, `power.py`, `report.py`;
  frozen-data-only per the architecture boundary, banned from ledger writes.

**Two drivers, one pipeline.** (A) In-process Go driver —
`harness.OpenSession` + the bench agent loop with a `ScriptedActor` client
(default; fastest; direct retrieval-trace/residency access; runs the sanity
agents and the whole CI tier). (B) pi driver — pi `--mode rpc` + bridge via
extended `internal/pirpc`, hosting the WSMS core in-process per arm via
`serve.Run(Options{Addr: ":0", Ready})` to sidestep port races and the
one-session-per-process limit. A driver-parity test on a reference scenario
asserts identical tool-call sequences and injected artifacts before any arm is
trusted on driver A; every real-model tranche opens with a pi-path canary
subset.

**Sandboxing.** All agent bash executes inside a no-network sandbox per
episode-arm over the copied fixture tree; the per-arm DataDir lives outside
the sandbox root and is denied by path policy (asserted per episode).

**Core seams to build** (each behind default-off config with
replay-equivalence tests):

- A `Renderer` seam replacing the hardcoded capsule render call in
  `internal/scheduler` — note `SelectL1`'s result is currently computed and
  discarded in `BeforeTurn` while `renderer.RenderCapsule` consumes the
  coherence-filtered `WorkingState` directly; the seam adopts `L1Selection`
  (field-equivalent to what `RenderCapsule` reads today) as the shared input
  for all three deterministic renderers, including the WSL one — with the
  tool-conditional fault instruction (section 3 rule 2).
- `Config.SchedulerMode {full|static|off}` for W-NOSCHED.
- `Session.WaitForMaintenance` drain barrier.
- Durable `page_access` appends wiring the existing unused ledger event kind.
- The non-fatal WSL lint-rejection counter.
- Per-turn capsule capture.
- Retrieval-trace exposure for HTTP runners (bench-only field on `/semantic`).
- Timing middleware.
- Budget enforcement via one pinned, vendored offline tokenizer (B-I9);
  render-time enforcement is deterministic and keyless, with post-hoc
  validation against each stratum's true tokenizer at a pre-registered ±5%
  tolerance; a violation fails the artifact at freeze, never mid-run.

**pirpc extensions:** wrappers for `new_session`, `compact`, `get_messages`,
`get_entries`, `get_session_stats`, `bash`; per-request timeout override
(compact/bash can exceed the client's fixed response wait); usage + attested
model-id decoding from `message_end`; `extension_ui_request` auto-responder
for headless safety; a session-seeding seam only if C2 survives (section 3
rule 5).

**Bridge/mock extensions:** `WSMS_BENCH_CONDITION` switching the context hook
between capsule-fetch / static-artifact-file / none and gating memory-tool
registration; `WSMS_INJECT_ROLE=system`; `WSMS_BENCH_ACTOR=<planfile>`
scripted-actor mode at the existing provider seam (planned turns,
deterministic tool calls, synthetic usage, mock-only model id); capsule
detection generalized beyond the `<working_state>` substring so baseline
substrates register.

**CLI:** `wsms bench gen|lint|run|freeze|verify|score|score-retrieval|report|
ratify-check`. `run` takes `--split {train|heldout} --conditions --driver
{go|pi} --model {mock|local|hosted} --replicates --jobs --out`; it refuses
`--split heldout` unless the working-tree `PREREGISTRATION.md` hash matches
`RATIFIED.sig` and the held-out manifest matches the sealed archive — except
in `--verify-only` mode, which requires only the sealed-manifest match: it
exercises held-out Phase-A/B plumbing under the mock for G0 but emits
hash-only reports (no transcripts, no golden trees), so the G0 plumbing proof
can run before ratification exists. All other modes refuse `--model mock` for
held-out. Episode-arms are hermetic and embarrassingly parallel.

**Determinism.** Scripted Phase-A actor + pinned event timestamps +
seq-derived IDs + synchronous-or-drained maintenance at freeze ⇒
byte-identical frozen state; post-reset nondeterminism is confined to the
model; the mock full-matrix run is 100% deterministic and gates CI via golden
diffs.

## 11. Offline tier vs model-gated tier

**OFFLINE-NOW (keyless, CI-tested, $0):** the scenario generator + training
corpus + labeled query sets + adversarial fixtures; all deterministic
condition renderers and core seams; both drivers including the bench agent
loop; the scripted-actor provider; freeze/verify/import; both scorers + stats
+ power code; the full condition-matrix mock dry run as CI regression; the
leakage linter and all three sanity agents; `PREREGISTRATION.md` drafting.
Mock results are permanently `evidence:false` and mechanically gate-ineligible.

**REAL-MODEL-GATED (blocked on served model + budget + ratification):** the
egress proxy + hosted smoke run; pinned local-embedder runs (vector parity,
cold start, CPU/GPU memory, throughput, cancellation behavior, query latency;
vec0 brute-force resource benchmark at corpus scale, with the equal-distance
top-k boundary limitation documented) unblocking dense/hybrid sub-variants and
the SLO measurements;
generation + audit of C2/C3/engineered artifacts; judge calibration;
training-split calibration (STRUCT* selection, retrieval thresholds, G3
analysis, variance/power re-check, hosted nondeterminism probe); held-out
confirmatory runs.

**G0 — harness proof (blocks any spend):** full offline matrix green in CI —
every scenario × every arm under both drivers, byte-identical replay
verification (two independent Phase-A executions per scenario), zero golden
drift, oracle sanity 100% including the disk-grepper — before the first
real-model token.

The boundary is enforced in artifacts (evidence attestation), in the CLI
(`--split`/`--model` refusals), and in the gate evaluator (hard rejection) —
not by convention.

## 12. Cost model

Pre-reset phases are scripted: zero model tokens. Post-reset economics
dominate: ~8 probe turns × ~4–5k input (heavily cache-read; shared prefixes
within an episode) + ~0.5k output per turn.

The tranche cost table in `PREREGISTRATION.md` is parameterized by the
candidate model pin, so ratifying the pin ratifies the price. Indicative
ranges (mid-tier hosted pricing; roughly 3–4× at frontier-tier pricing):

- D1 tranche (W-FAULT + STRUCT* + the three non-selected STRUCT variants,
  held-out, per section 8.8): the separable WSL decision, ~$300–650.
- Training calibration (6–8 arms × 64 episodes × 1–2 replicates): ~$150–400.
- Ladder tranches (G2a/G2-pre/G2b/G2c): ~$250–450.
- Judge inference ~$30–50; baseline best-of-N generation ~$50–100.
- Component sum ~$780–1,650; with a 2× retry/escalation margin the worst-case
  envelope is ~$1,600–3,300 (the ladder tranches only run if their
  prerequisites pass, so the expected total sits well below the worst case).

The hosted replicate count follows the measured nondeterminism probe
(section 8.3); if discordance < 5%, replicate savings are reallocated to
held-out families. Pre-committed budget-degradation ordering (in the prereg):
drop C1-FULL replicates first, then local-stratum replicates, then non-gate
diagnostic arms — never held-out family count for the H1 contrast.

Local stratum: $0 marginal. Wall clock is GPU-bound, not process-bound:
single-GPU ≈ the serial 3–5 days for ~1,000–1,600 episode-arms; any speedup
claim must come from the measured tokens/sec of the GATED-9 throughput
benchmark, not assumed parallelism. Embedding runs are trivial (<1 GPU-hour).

Human labor: judge calibration double-labeling ~7–10 person-hours; baseline
audits + 10% held-out judge audit + 20% L3-label audit ≈ 15–25 person-hours.

Engineering: the offline tier is realistically **8–14 single-engineer weeks**
at full scope (the bench agent loop, ten fixture families, dual scorers, and
the linter/sanity harness are the dominant items). The descoped start — six
families — targets the low end. Gated steps: ~1.5–3 weeks once model access
lands. Storage: ~5–15 GB compressed for the full frozen matrix.

## 13. Decision gates

All gates are mechanical given frozen artifacts and the prereg. Statistical
terms are defined in section 8 and referenced here by name — each rule exists
in exactly one place.

- **G0 — harness proof** (blocks spend): section 11.
- **G1 — ratification** (blocks held-out): `PREREGISTRATION.md` owner-ratified;
  `RATIFIED.sig` hash matches the working tree; the held-out manifest matches
  the sealed archive; the runner refuses otherwise.
- **G2 — WSL keep/simplify/reject** (executes D1; trial = format layer only).
  On held-out, authoritative stratum per G3, per-stratum replicate policy:
  - **KEEP** iff W-FAULT beats STRUCT* on continuation success (one-sided
    superiority at the Holm-adjusted level, clustered-bootstrap primary) AND
    the token rule's "equal-or-lower" holds (§8.6) AND the robustness guard
    holds against every STRUCT variant (§8.8).
  - **REJECT** the format layer iff STRUCT* beats W-FAULT (the opposite
    one-sided test at the Holm-adjusted level).
  - **SIMPLIFY otherwise** — the explicit residual branch, including
    inconclusive/wide-CI outcomes and KEEP-quality wins at higher token cost.
    An under-powered true edge therefore reads as SIMPLIFY; the owner ratifies
    that semantics knowingly.
  - Remediation: SIMPLIFY adopts the winning deterministic structured renderer
    atop the unchanged ledger/scheduler/coherence/fault stack (WSL becomes an
    internal encoding or is deleted). If an engineered variant is STRUCT* and
    wins, the outcome is "reject the format layer, adopt the LLM-checkpoint
    condition" with generation cost accounted (§3 rule 4). Reported vs
    deterministic and vs engineered variants separately: a win over
    deterministic STRUCT evidences syntax; over engineered STRUCT, mechanism.
- **G2a — fault-tool retention:** keep tools default-on iff W-FAULT ≥ W-SCHED
  (superiority, or non-inferior within 5 pp at token superiority per §8.6) AND
  page-fault precision on labeled probes ≥ 0.5. **On failure:** fault tools
  ship default-off (retained as opt-in) and G2b/G2c are not evaluated.
- **G2-pre — hybrid vs FTS-only:** hybrid ships as default iff the H3
  confirmatory pair passes — exact-reference precision improves AND
  continuation success does not regress (non-inferiority per §8.6) — with
  mandatory side conditions: Recall@10 improves; zero wrong-scope selections
  (rule-of-three bound reported); stale-revival delta CI upper bound < +1 pp;
  no negative-transfer regression on either operationalization; both SLOs
  met. Else ship FTS-only (the documented fallback).
- **G2b — L2-only prefetch promotion** (requires the pinned real embedder run):
  useful-prefetch ratio ≥ the training-calibrated pre-registered threshold;
  both negative-transfer operationalizations within margin (incident CI
  excluding harm worse than −2 pp; noise-contrast CI upper bound < +2 pp);
  zero wrong-scope/invalidated materializations in the adversarial fixture
  suite; and the ladder's end-to-end clause: W-PREFETCH vs W-FAULT improves,
  or is non-inferior at token superiority (§8.6).
- **G2c — automatic L1 admission** (only if G2b promotes): its own paired
  held-out contrast — **W-L1AUTO vs W-PREFETCH** improves, or is non-inferior
  at token superiority, with the same paired machinery — PLUS zero
  invalidated-page materializations in the coherence suite, zero
  cross-session/scope leaks, hard-constraint loss not worsened (margin 0,
  exact test), direct-ID read_page and no-L3 behavior unchanged versus the
  W-FAULT no-L3 sub-variant (golden-diff on the offline tier plus
  non-regression on held-out), index deletion + full rebuild passing the
  checked-in crash/interruption test suite (offline tier), and every admitted
  page carrying an inspectable explanation with exact L4 refs.
- **G3 — instrument validity:** on the training split, run {STRUCT*, W-FAULT,
  C3-SUM, C1-TRIM} under both strata. Local is co-primary iff the **sign** of
  the W-FAULT − STRUCT* paired difference matches across strata AND the
  between-strata difference CI includes 0. If co-primary, held-out agreement
  = sign concordance plus a pooled stratified test at the family α (joint
  power pre-registered), not two independent significance requirements.
  Disagreement ⇒ "no decision, investigate", with the pre-registered
  post-investigation rule: hosted becomes sole authority under a new prereg
  tag, or the run is voided and re-designed.
- **G4 — evidence rule:** the gate evaluator hard-rejects `evidence:false`
  manifests, any per-message attested model id differing from the pin, and
  scorer-version mismatches.
- **G5 — claim gate:** the report generator emits only the pre-registered
  claim template with measured values and CIs; negative results at equal
  prominence; no comparative wording anywhere until the held-out stats report
  artifact exists.
- **G6 — abort gates:** replay-equivalence failure, Go/Python scorer
  disagreement on gate-path metrics, judge κ < 0.8 without programmatic
  fallback, >10% INVALID episodes in any arm, or a differential-invalidity
  flip (§8.7) ⇒ the affected arm/metric is invalidated and the run halts for
  diagnosis.

## 14. Ratification checklist

The owner signs before any held-out execution (G1). Each item is a decision
only the owner can make:

1. WSL pre-commitment wording as operationalized here: the exhaustive
   KEEP/REJECT/**SIMPLIFY-otherwise** trichotomy, the 5 pp non-inferiority
   margin, the token rule (§8.6), and the STRUCT* robustness guard (§8.8).
2. `B_resume` = 1,024 measured tokens (or adjusted), and whether
   `CapsuleTokenBudget` is re-expressed in measured tokens for the benchmark
   (the per-turn budget-parity audit, §3 rule 8, makes this visible either
   way).
3. Model pins: hosted frontier ID, local agent weights (size/quant), local
   embedder weights, judge family (≠ every agent under test) — pinning also
   fixes the tranche cost table.
4. Hosted budget envelope and the budget-degradation ordering.
5. Held-out-only family designation (2 of 10).
6. G3 authority rule including the post-investigation resolution, and the
   fallback if the hosted budget is denied or exhausted (local-only verdict
   acceptable for D1, or D1 waits).
7. Per-stratum replicate policy (local 1; hosted from the measured
   nondeterminism probe).
8. INVALID-handling rules: >10% voiding, differential-invalidity trigger,
   worst-case-sensitivity override.
9. Second human labeler recruited or waived (waiver weakens the κ protocol
   and is recorded in the prereg).
10. C2-COMPACT: build the session-seeding seam, or drop to a footnote.
11. External-validity appendix arm (non-nonce, descriptive, never gating): in
    scope for Phase 10 or deferred.
12. Out-of-repo archive location for held-out bodies and large results trees.
13. Formal signature: `RATIFIED.sig` commit — the runner mechanically blocks
    all held-out execution until it exists.

## 15. Build plan

Offline (keyless; ordered; realistically 8–14 weeks at full scope, low end
with the descoped start):

- **OFF-1** — episode schema + scenario generator (nonce engine, oracle
  specs, labeled L3 queries), leakage linter (both polarities, fixture-tree
  and scripted-user channels), perfect/amnesiac/disk-grepper sanity harness;
  author the training corpus (six families first); unit-test oracles against
  hand-written transcripts.
- **OFF-2** — core seams: Renderer interface with tool-conditional fault
  instruction, deterministic YAML/MD renderers over `L1Selection` with
  budget/drop-order/FR-008 parity + golden information-equivalence tests
  (fields, instruction text, page IDs), `SchedulerMode`, `WaitForMaintenance`,
  durable page-access appends, lint counter, per-turn capsule capture,
  retrieval-trace exposure, timing middleware, pinned offline tokenizer
  budget enforcement.
- **OFF-3** — pirpc extensions (new_session/compact/get_messages/get_entries/
  get_session_stats/bash, timeout override, usage + attested-model-id
  decoder, ui auto-responder) + bridge condition switch + `WSMS_INJECT_ROLE`
  + scripted-actor provider with generalized capsule detection.
- **OFF-4** — `internal/bench`: the agent loop (typed tool-calling interface,
  shared tool registry, per-turn re-injection — budgeted as its own 1–2 week
  item), runner (Phase-A/B, replay oracle, reminder engine, sandbox,
  freeze/verify/import, content-addressed store), pi driver, dereferencing
  transcript reconstructor; driver-parity test (identical tool-call sequences
  and injected artifacts).
- **OFF-5** — scorers: Go + Python with exact-agreement on gate-path metrics,
  IR metrics, anti-gaming fixtures; `research/` stats (clustered paired
  bootstrap primary, McNemar sensitivity, Wilcoxon, Holm) + `power.py` with
  design-effect handling and golden fixtures.
- **OFF-6** — full-matrix mock dry run wired into CI (golden results tree,
  race/stress on the runner); two independent Phase-A executions per scenario
  byte-identical ⇒ satisfies G0.
- **OFF-7** — remaining four families; draft `PREREGISTRATION.md` (gate rules
  verbatim, margins, samples, STRUCT* rule, judge protocol, claim template,
  cost table) and tag `bench-prereg-v1`; then generate + seal the held-out
  corpus out-of-repo and commit `HELDOUT.sha256` in the sealed-manifest
  addendum tag (B-I6); surface the ratification checklist.

Model-gated (each opens with its own smoke/canary):

- **GATED-8** — egress/redaction proxy with network-layer enforcement,
  isolated agent config dir, decoy-credential CI canary; hosted smoke run
  (7 training scenarios × 3 arms, each rerun once — the §8.3 nondeterminism
  probe) validating usage capture, attested model ids, cost projection, and
  setting the hosted replicate count.
- **GATED-9** — pinned local-embedder runs (vector parity, cold start,
  CPU/GPU memory, throughput, cancellation behavior, query latency; vec0
  brute-force resource benchmark at corpus scale, with the equal-distance
  top-k boundary limitation documented in the run report) unblocking
  dense/hybrid arms and SLOs; local-agent smoke fixing the wall-clock model.
- **GATED-10** — generate + human-audit C3/engineered artifacts (probe-blind
  selection, admission lint, novel-content check); C2 only if its seam was
  ratified; judge calibration vs double-labeled items (κ ≥ 0.8 + blinding
  probe) or programmatic-only fallback.
- **GATED-11** — training-split calibration on both strata: STRUCT* selection
  (family-blocked CV), retrieval/prefetch thresholds, G3 analysis, ICC/design
  effect, variance/power re-check; freeze all choices into a prereg addendum
  (any threshold move ⇒ new tag) before unsealing.
- **GATED-12** — held-out D1 tranche first (W-FAULT, STRUCT*, and the three
  robustness variants — the WSL decision lands before any prefetch code
  exists); then G2a and G2-pre; build the prefetch mode only if warranted, run
  G2b; build L1-auto only if G2b promotes, run G2c.
- **GATED-13** — freeze results trees, run stats, fire gates, emit the gate
  report via the claim-template generator, execute the D1 outcome, publish
  the reproducibility bundle (frozen runs + prereg + results).

## 16. Risks

- **Scenario authorship bias toward WSMS's ontology.** Mitigated by
  information-equivalent baselines rendered from the same filtered view,
  transcript-observable oracles, pathology-driven templates taken from the
  spec, three sanity agents, and published generator seeds. Residual
  task-distribution favoritism is acknowledged; the external-validity
  appendix is the owner's call.
- **Interpretation hazard on D1.** A WSL win over deterministic STRUCT
  evidences syntax; over engineered STRUCT, mechanism. The report must keep
  these separate or it overclaims.
- **Mock-to-real gap.** G0 proves plumbing, not effect sizes; real-model
  resume behavior may break scripted-user assumptions — mitigated by the
  smoke/calibration tranches, the bounded fallback policy, and the pi-path
  canary. Mock numbers are mechanically gate-ineligible.
- **Token-matching contestability.** Estimator-vs-tokenizer divergence is
  format-correlated (nonce identifiers explode under BPE; WSL/YAML/Markdown
  have different punctuation densities). Handled by pinned-tokenizer
  enforcement on every per-turn injection, both counts frozen, the
  budget-parity audit gate (§3 rule 8), and the FR-008 overflow mirrored and
  audited from `capsules.jsonl`.
- **Judge residual circularity.** Blinding + normalization + the probe
  classifier + a cross-family judge + programmatic-only gate passage reduce
  but do not eliminate style-condition correlation; κ failure demotes checks
  rather than shipping an unvalidated judge.
- **Statistical fragility.** Ceiling/floor baseline success or low
  discordance collapses power; family clustering shrinks effective n well
  below episode count. Pressure valves: the dev-split ICC/discordance check,
  family-block expansion, and the measured-replicate policy. The design is
  deliberately conservative: an under-powered true 5–8 pp WSL edge reads as
  SIMPLIFY, and the owner ratifies that semantics.
- **Fail-stop poisoned ledgers.** Systematic fail-stops make conditions
  unscoreable and, worse, can differ by arm; the INVALID machinery (§8.7)
  makes that visible and decision-safe. Frequent incidents force a core fix
  and a new prereg tag.
- **Hosted egress is security-critical greenfield.** Deny-by-default at the
  network layer, isolated config dir, audit-log hashes in manifests,
  nonce-only synthetic fixtures (no real user code), and the local stratum as
  a fallback publication path.
- **C2 compaction is operationally fragile** and has no mechanism without a
  seeding seam — descriptive only, "where available", or dropped.
- **New-mechanism schedule risk.** The prefetch mode and L1-auto admission
  are new code, deferred behind their gates so the highest-value decision is
  never blocked by them.
- **Replay-ID drift across observer versions.** Manifests pin the wsms
  commit; `verify` runs against the pinned commit; longitudinal corpus reuse
  needs the versioned derivation-identity work already flagged in the plan.
- **Hand-labor concentration.** L3 labels and adversarial fixtures are the
  largest human item — bounded by generator-emitted labels (exact by
  construction) with a 20% audit.

## 17. Non-goals

- Absolute agent capability (SWE-bench-style pass rates): orthogonal to
  memory; at most a descriptive external-validity appendix, never a gate
  input.
- Observer CPU and background scheduling cost as decision inputs: recorded
  descriptively; no gate consumes them.
- Human-preference prose quality, multi-repo and multi-agent scenarios,
  cross-session long-horizon memory: out of scope until D1/D2 resolve.
- Provider compaction as a gate opponent: cannot be token-matched.
- Marketing comparisons: the outputs are gate verdicts and the pre-registered
  claim template — nothing else.
- Mock-tier performance numbers: plumbing regression signal only.

## 18. Open questions

The ratification checklist (section 14) enumerates the owner decisions. Items
needing an answer before OFF-7 (everything else can wait for G1): the
trichotomy wording and margins (item 1), `B_resume` (item 2), the descoped
six-family start vs full ten (schedule), and whether C2 gets its seam
(item 10).
