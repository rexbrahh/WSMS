Below is a **canonical draft v0.1** of the paper. I’m writing it as a serious research/position paper rather than a product manifesto.

---

# Working-State Virtual Memory for LLM Agents

## A local runtime for context decay, compaction failure, and long-session agent cognition

**Author:** Rex Liu **Status:** Draft v0.1 / research proposal **Primary domain:** local coding agents, synchronous human-agent work, hosted-model-compatible harnesses

---

## Abstract

Long-horizon LLM agents degrade during extended interactive work. The failure often appears after many tool calls, large command outputs, file reads, failed attempts, and eventual context compaction. Existing systems mitigate context overflow through larger context windows, summarization, tool-result clearing, prompt compression, retrieval, and persistent memory. These approaches help, yet they do not reliably preserve the operational working state of an agent: exact failures, rejected approaches, current branch state, user corrections, hard constraints, next actions, and stale assumptions.

This paper proposes **Working-State Virtual Memory**, a local runtime architecture for LLM agents that treats the model context window as a managed working set rather than as the session’s source of truth. The system consists of an append-only event ledger, asynchronous observer workers, a working-state intermediate representation called **WSL**, a scheduler-managed memory hierarchy, page-fault tools, and provider-compatible context renderers. WSL is designed as a readable, typed, validated operational notation. It does not require hosted providers to train on a new language. Instead, the runtime uses WSL internally and renders small structured-text capsules into the main agent’s prompt.

The central claim is narrow: for local coding sessions, a harness-managed working-state runtime can reduce context-induced cognitive degradation better than append-only transcripts and emergency summarization. The goal is not general artificial memory. The goal is to keep a coding agent sharp during serious local work by continuously maintaining, scheduling, and rehydrating the agent’s active working state.

---

## 1. Introduction

Modern coding agents are increasingly useful, yet daily use reveals a persistent failure mode: the agent starts a long session coherently, accumulates transcript sludge, encounters context pressure, undergoes compaction or summary-based continuation, and then resumes with a subtly damaged understanding of the task. The agent may repeat failed attempts, forget exact user corrections, preserve vague narrative while losing exact errors, or revive stale assumptions from earlier in the session.

The field has made real progress. Larger context windows help. Prompt compression helps. Tool-result clearing helps. Persistent memory helps. Provider compaction helps. First-party harnesses have improved substantially. Yet the core pathology remains: **the model’s active context is being asked to behave like durable working memory, long-term memory, scratch space, tool log, planning notebook, and execution transcript all at once**.

The analogy to operating systems is natural. A process does not treat physical memory as the entire memory system. It benefits from caches, page tables, working-set policies, virtual memory, storage, scheduling, and fault handling. Classical working-set theory already framed memory demand in terms of the pages a process actively needs during a time window. Denning’s working-set model remains foundational for understanding locality and paging behavior in virtual memory systems. ([ACM Digital Library][1])

LLM agents today often lack the equivalent of a memory-management unit. The context window receives whatever the harness appends, summarizes, retrieves, or forgets. This causes avoidable cognitive degradation.

This paper proposes a runtime architecture for **harness-managed working memory**. The foreground interaction remains synchronous and operator-led: the human sits down at a local workstation, gives instructions, reviews results, and commands the agent. Behind the foreground loop, an asynchronous memory scheduler maintains a compact operational representation of the active task.

The resulting system aims to preserve the feeling of a minimal harness while reducing the pain of context babysitting. The human should work. The harness should remember the operational state. The model should stay within a sharp working range.

---

## 2. Motivation: Context windows are active working sets

The context window of a transformer is frequently treated as if it were memory. This is an architectural mistake. Long-context models can receive large inputs, but they do not necessarily use all positions with equal reliability. “Lost in the Middle” showed that language models can degrade when relevant information appears in the middle of long contexts, even when those models nominally support long inputs. ([arXiv][2])

Provider-side compaction mitigates context overflow, yet public APIs expose the limitation clearly. OpenAI’s compaction documentation states that the compacted item carries forward prior state using fewer tokens, while also saying that the item is opaque and not intended to be human-interpretable. ([OpenAI Developers][3]) Anthropic describes context engineering as a combination of compaction, memory, and tool-result clearing, acknowledging that long-running agents require active management of what remains in context. ([Claude Platform][4])

Existing context engineering usually begins from the transcript. The transcript grows. The system compresses, clears, retrieves, or summarizes. This helps, but it preserves history only indirectly. A coding session’s critical state is rarely the whole transcript. It is the active operational state:

```text
current objective
current branch
dirty files
known-good commands
last failed command
exact error string
user corrections
hard constraints
rejected approaches
stale assumptions
next plausible action
```

The thesis here is simple: **long-session agent reliability depends on maintaining this operational state as a first-class runtime object**.

---

## 3. Problem statement: working-state degradation

We define **working-state degradation** as the loss, corruption, misprioritization, or stale revival of task-relevant operational state during an extended agent session.

It manifests as:

1. **Repeated failed attempts** The agent retries a command, patch, or approach that already failed.

2. **Lost user corrections** The human says “do not do X,” and the agent later does X after compaction or context pressure.

3. **Exact-evidence loss** The summary says “the test failed,” while dropping the exact command, test name, error message, file path, or exit condition.

4. **Narrative preservation with operational loss** The compactor preserves the story of the work but loses the actionable state needed to continue.

5. **Tool-output pollution** Large logs, file reads, diffs, and package-manager output crowd out more important task state.

6. **Stale assumption recurrence** Earlier hypotheses remain influential after later evidence invalidates them.

7. **Cross-task contamination** Memory from an old branch, old issue, or old architectural interpretation bleeds into the current task.

8. **Human reminder burden** The human repeatedly re-explains state the harness should have retained.

This degradation differs from simple context overflow. A session may fit inside the context window and still degrade due to attention dilution, stale context, badly ranked retrieval, and overlong tool outputs. Larger windows defer failure, but they do not create a managed memory hierarchy. The Pichay paper states this in OS terms: the context window is better viewed as L1 cache, with missing L2, virtual memory, demand paging, and persistent storage abstractions. ([arXiv][5])

---

## 4. Design principles

The proposed system follows seven principles.

### 4.1 The foreground loop remains synchronous

Most human-agent coding work is turn-based:

```text
human instruction
→ agent action
→ tool result
→ human review or next instruction
```

A full async agent operating system is excessive for the core use case. The main coding loop should remain simple and human-commanded.

### 4.2 Memory management runs asynchronously

Although the foreground loop is synchronous, memory work is naturally asynchronous. The system should digest tool outputs, update task state, consolidate pages, rank memory, and maintain indexes while the human and main agent continue working.

### 4.3 The context window is resident working memory

The context window should contain only the current resident working set, not the whole session. Old history remains recoverable through page faults and local storage.

### 4.4 Compaction should become routine maintenance, not emergency surgery

Emergency transcript summarization after context bloat is fragile. The system should continuously maintain compact operational state before the transcript becomes obese.

### 4.5 WSL is internal and readable

The working-state language should be compact and validated, but readable by ordinary hosted models after a short primer. It should not require OpenAI, Anthropic, or any provider to train models on a private language.

### 4.6 Exact evidence lives outside summaries

Summaries may describe state. Exact evidence must remain available through pointers: raw command outputs, diffs, files, logs, user corrections, and decision records.

### 4.7 The system optimizes sharpness, not maximum context

The objective is not to use the largest possible context window. The objective is to maximize task-relevant working state per token.

---

## 5. System overview

The proposed runtime has six major components:

```text
Foreground Agent Loop
  ↓ emits events
Append-only Event Ledger
  ↓ consumed by
Observer Workers
  ↓ update
WSL Working State
  ↓ managed by
Memory Scheduler
  ↓ selects pages for
Context Renderer
  ↓ sends structured capsule to
Main Agent
```

### 5.1 Foreground agent loop

The foreground loop is the main harness experience: TUI, local repo, tools, shell commands, diffs, and user messages. It may wrap any main model: hosted API, OpenRouter-style API, local model, Codex-compatible adapter, Claude-compatible adapter, or a Pi-like execution loop.

The foreground loop emits events but does not own durable memory.

### 5.2 Append-only event ledger

The ledger is the source of truth. It records:

```text
user_instruction
assistant_message
tool_call
tool_result
command_run
command_output
file_read
file_write
git_diff
test_result
human_correction
decision
assumption
failure
source_added
memory_created
memory_invalidated
```

Each event includes IDs, timestamps, repo metadata, branch, commit, dirty-state hash, and links to artifacts.

### 5.3 Observer workers

Observers are small local processes that consume events and update working state. They should be conservative and mostly deterministic. Many observer jobs require no LLM:

```text
command failed
exit code nonzero
test name detected
file modified
branch changed
same error reappeared
user used a hard constraint phrase
agent repeated a rejected command
```

LLMs or embedding models may help classify salience, cluster similar failures, and extract decision records, but the system should avoid using a second hallucinating agent as its memory authority.

### 5.4 WSL working state

WSL is a typed, line-oriented operational notation used internally by the scheduler. It represents tasks, constraints, failures, decisions, pages, stale assumptions, and next actions.

Example:

```text
@task T42 phase=debugging priority=hot
goal: fix(stream_cancel_hang)
branch: solver-stream-cancel
dirty: src/runtime/stream.go, src/solver/cancel.rs

@constraint C7 hard source=user
text: "do not rewrite transport layer"

@failure F18
cmd: `go test ./runtime -run TestCancelStream`
exit: 1
err: "stream goroutine still blocked"
file_hint: src/runtime/stream.go:118-176

@avoid A4 reason=failed_attempt ref=F16
text: "previous goroutine cleanup patch"

@next
inspect: src/runtime/stream.go:118-176
question: "is cancellation propagated before stream recv blocks?"
```

WSL is not meant to be a model-native secret language. Its usefulness comes from being structured, compact, validated, pointer-rich, and readable enough for ordinary models.

### 5.5 Memory scheduler

The scheduler decides what becomes resident in the active model context. It maintains memory tiers, schedules background jobs, handles page faults, and prevents stale state from entering L1.

### 5.6 Context renderer

The renderer converts WSL into model-facing structured text. Hosted APIs see ordinary text, not private latent state.

Example rendered capsule:

```text
<working_state>
TASK T42: Fix stream cancellation hang.
NEXT: Inspect cancellation propagation in src/runtime/stream.go:118-176.
HARD USER CONSTRAINT C7: Do not rewrite the transport layer.
LAST FAILURE F18: `go test ./runtime -run TestCancelStream` failed with:
"stream goroutine still blocked"
AVOID A4: Previous goroutine cleanup patch failed in F16.
</working_state>
```

This preserves provider compatibility while allowing the runtime to maintain a stricter internal representation.

---

## 6. Memory hierarchy

The memory hierarchy adapts operating-system concepts to LLM agent context.

| Layer | Name                  | Contents                                            | Policy                      |
| ----- | --------------------- | --------------------------------------------------- | --------------------------- |
| L0    | Turn scratch          | current user message, immediate tool result         | ephemeral                   |
| L1    | Active capsule        | task, hard constraints, last failure, next step     | always resident             |
| L2    | Hot pages             | recent failures, active branch decisions, hot files | ready to inject             |
| L3    | Warm project memory   | repo conventions, stable commands, prior decisions  | indexed retrieval           |
| L4    | Cold ledger/artifacts | full logs, transcripts, diffs, raw evidence         | pointer-only unless faulted |

This resembles recent virtual-memory approaches to LLM context. MemGPT introduced virtual context management with memory tiers and control-flow interrupts for long conversations and document analysis. ([arXiv][6]) Pichay frames context management even more directly as demand paging for LLM context windows, with L1 eviction, L2 fault-driven pinning, and L3 compaction. ([arXiv][5])

The proposed system differs in emphasis: it targets local synchronous coding sessions, uses asynchronous observer-managed WSL state, and treats provider-compatible structured text as the main interface.

---

## 7. Scheduler design

The scheduler should use queues rather than a monolithic “memory update” step.

### 7.1 Real-time injection queue

Runs before each main-agent turn.

Contains:

```text
active task
hard user constraints
last failure
current next step
known stale assumptions
critical file pointers
```

This queue produces L1.

### 7.2 Tool-result digestion queue

Runs after shell commands, file reads, test runs, and tool calls.

Extracts:

```text
exit code
failing test
exact error
changed files
new file hints
new dependency/version facts
log artifact pointer
```

Anthropic’s context-engineering guidance treats tool-result clearing as distinct from compaction, since large tool outputs are a major source of context pressure. ([Claude Platform][4])

### 7.3 Consolidation queue

Runs during idle moments, after task boundaries, or when the active state changes.

Creates:

```text
task checkpoints
failure pages
decision pages
known-good command pages
repo fact pages
stale assumption records
```

### 7.4 Retrieval and page-fault queue

Runs when the main agent requests missing context.

Example page-fault requests:

```text
READ_PAGE F18
READ_DECISION D12
READ_FILE_SLICE src/runtime/stream.go:118-176
READ_RAW_LOG E9044
```

The runtime returns only the requested page or evidence, then updates page-access history.

### 7.5 Cold-storage maintenance queue

Runs slowly.

Handles:

```text
embedding updates
FTS index updates
artifact compression
duplicate memory merging
branch validity checks
stale page demotion
old session archival
```

---

## 8. WSL: Working-State Language

WSL is the internal representation used by observers and the scheduler. It should satisfy six constraints.

### 8.1 Human-readable

A model and a human should infer the meaning from the syntax. The protocol must be learnable from a one-page local spec.

### 8.2 Typed

Records have stable types:

```text
@task
@constraint
@failure
@decision
@avoid
@assumption
@invalidated
@next
@page
@fault
```

### 8.3 Pointer-rich

WSL does not copy all evidence. It points to exact events and artifacts.

```text
ref=E9182
raw=artifact:sha256:...
file_hint=src/runtime/stream.go:118-176
```

### 8.4 Validated

The runtime should lint WSL updates.

Example:

```text
@constraint C7 hard source=user
text: "do not rewrite transport layer"

@decision D9
chosen: "rewrite transport layer"
```

This update conflicts with a hard constraint and should be rejected or flagged.

### 8.5 Compact without becoming cryptic

Token savings should come primarily from selection and structure, not unreadable abbreviations. Dense symbolic formats risk becoming brittle for untrained hosted models.

### 8.6 Renderable

WSL must render into ordinary structured text for models that do not know the protocol.

---

## 9. Why WSL over YAML, XML, or Markdown?

YAML, XML, and Markdown are strong baselines. A paper or prototype must beat them before WSL deserves attention.

WSL’s hypothesis is that a domain-specific operational notation can improve over general structured text in four ways:

1. **Stable semantics** `@failure`, `@avoid`, and `@constraint` have defined meanings.

2. **State transitions** The system can represent updates, invalidations, promotions, and demotions.

3. **Validation** WSL can be linted for contradiction, missing evidence, invalid references, and stale scope.

4. **Scheduler integration** WSL records map directly to cache residency, priority, scope, and page-fault behavior.

The syntax itself is not the breakthrough. The breakthrough is the combination of syntax, validation, scheduling, and memory hierarchy.

---

## 10. Provider compatibility

Most users will use hosted providers. Therefore, the system cannot depend on hidden-state injection, KV-cache surgery, or provider training.

The runtime should support three modes:

### 10.1 Controlled raw-API mode

The harness assembles the full message array and sends a rendered working-state capsule. Provider compaction may be disabled or avoided when possible.

### 10.2 Hosted compatibility mode

The harness injects structured working-state capsules into existing agent loops. It cannot guarantee full control over provider-side session state, but it can reduce context bloat and improve foreground state.

### 10.3 Local model mode

The harness uses a local model through a runtime such as llama.cpp, Ollama, MLX, vLLM, or Hugging Face. Advanced experiments may explore prompt caching, prefix reuse, or learned soft prompts, but the first prototype should use structured text.

The design avoids dependence on model internals. vLLM’s PagedAttention shows that virtual-memory concepts can improve serving efficiency by paging KV-cache blocks, but serving-layer KV management solves a different problem from semantic working-state management. ([arXiv][7])

---

## 11. Algorithms

### 11.1 Event ingestion

```pseudo
on_event(event):
    ledger.append(event)
    classify_basic_event(event)

    if event.type in {command_output, test_result, tool_result}:
        enqueue(tool_result_digest, event)

    if event.type in {user_instruction, human_correction}:
        enqueue(real_time_state_update, event)

    if task_boundary_detected(event):
        enqueue(consolidation, current_task)
```

### 11.2 Observer update

```pseudo
digest_tool_result(event):
    facts = extract_deterministic_facts(event)
    salience = score_salience(facts, active_task)

    for fact in facts:
        if fact.salience > threshold:
            wsl.apply(fact_to_wsl_update(fact))

    if event.raw_size > budget:
        artifact_store.put(event.raw)
        wsl.link_artifact(event.id, artifact_hash)
```

### 11.3 Residency scoring

```pseudo
score(page, active_task):
    return (
        0.30 * scope_match(page, active_task)
      + 0.20 * recency(page)
      + 0.20 * salience(page)
      + 0.15 * access_frequency(page)
      + 0.15 * user_priority(page)
      - 0.40 * staleness(page)
      - 0.60 * invalidation(page)
    )
```

### 11.4 Context rendering

```pseudo
render_context(active_task, token_budget):
    capsule = []

    capsule.add(pinned_system_contract)
    capsule.add(render_L1(active_task))

    remaining = token_budget - capsule.tokens

    for page in ranked_L2_pages(active_task):
        if page.tokens <= remaining:
            capsule.add(render_page_summary(page))
            remaining -= page.tokens

    capsule.add(page_fault_instructions)
    return capsule
```

### 11.5 Page fault

```pseudo
on_page_fault(request):
    page = resolve(request.id)
    if page is None:
        return "PAGE_MISS"

    record_access(page)
    maybe_promote(page)

    return render_page(page, budget=request.budget)
```

---

## 12. Evaluation plan

The prototype must prove that the runtime improves long-session continuation. The evaluation should compare against strong baselines.

### 12.1 Baselines

1. Raw long context.
2. Provider compaction.
3. Natural-language summary.
4. YAML checkpoint.
5. Markdown `NOW.md` / task-state file.
6. Retrieval-only memory.
7. WSL without scheduler.
8. WSL with scheduler.
9. WSL with scheduler and page faults.

### 12.2 Task families

Use coding-agent tasks that require extended interaction, tool use, failure recovery, and context carryover. SWE-bench Verified is a widely used human-validated coding benchmark of 500 software-engineering issues, but it may be insufficient alone because our target pathology is long interactive continuity rather than single-issue patching. ([OpenAI][8]) SWE-Bench-CL is closer because it studies continual learning across chronologically ordered coding tasks and includes memory-enabled agent evaluation ideas. ([arXiv][9])

A custom benchmark should simulate the exact failure mode:

```text
long session
many tool calls
several failed attempts
explicit user corrections
branch/file changes
forced compaction or reset
continuation task
```

### 12.3 Metrics

Primary metrics:

```text
task success after reset
lost hard-constraint rate
repeated failed-attempt rate
exact error recall
stale assumption recurrence
human reminder count
tokens per successful continuation
page-fault precision
page-fault recall
invalid WSL update rate
```

Secondary metrics:

```text
latency overhead
observer CPU usage
background scheduling cost
context-token savings
memory poisoning incidents
cache thrashing incidents
```

### 12.4 Forced-reset benchmark

The most important benchmark:

```text
Run a coding task for N turns.
Inject tool noise and failed attempts.
Force context reset or compaction.
Resume using each baseline memory method.
Measure continuation quality and repeated mistakes.
```

The headline result should be stated only after data exists:

```text
WSL+scheduler reduces repeated failed attempts by X%
and lost hard constraints by Y%
relative to natural-language compaction
under equal active-token budgets.
```

Until then, this remains a hypothesis.

---

## 13. Related work

### 13.1 Long-context degradation

“Lost in the Middle” demonstrated that long-context models do not robustly use information across all positions, with degraded performance when relevant information is buried in the middle. ([arXiv][2]) This supports the view that larger context windows alone cannot solve working-state degradation.

### 13.2 Prompt compression

LLMLingua compresses prompts through budget-controlled token reduction while attempting to preserve semantic integrity. ([arXiv][10]) Prompt-compression surveys divide methods into hard and soft prompt compression, showing this is an active research area. ([arXiv][11]) WSL differs by representing operational state and memory residency rather than compressing arbitrary prompt text.

### 13.3 Provider compaction

OpenAI exposes compaction through its Responses API and documents opaque compacted items that carry prior state forward with fewer tokens. ([OpenAI Developers][3]) Anthropic exposes server-side compaction for long conversations and also describes broader context-engineering strategies. ([Claude Platform][12]) These are important mitigations, but hosted compaction usually remains provider-controlled.

### 13.4 Agent memory systems

Codex memories store local memory files under `~/.codex/memories/`, including summaries, durable entries, recent inputs, and supporting evidence. ([OpenAI Developers][13]) Cloudflare’s Agent Memory ingests conversation history at compaction time and extracts facts, events, instructions, and tasks for later retrieval. ([The Cloudflare Blog][14]) These systems show industry convergence around external memory, but WSL emphasizes local operational state and scheduling.

### 13.5 Virtual-memory-inspired agents

MemGPT introduced virtual context management using memory tiers inspired by operating systems. ([arXiv][6]) AIOS proposes an LLM-agent operating-system kernel with scheduling, context management, memory management, storage management, and access control. ([arXiv][15]) Pichay directly frames the context window as L1 cache and implements demand paging for LLM context windows. ([arXiv][5])

The proposed architecture inherits this framing but narrows the target: local coding harnesses, synchronous foreground use, asynchronous memory scheduling, and a typed operational protocol.

### 13.6 Human memory inspiration

Human memory is not a perfect database, but its architecture suggests useful abstractions: limited working memory, episodic memory, semantic memory, and indexing. Baddeley’s episodic buffer adds a limited-capacity binding space within working memory. ([PubMed][16]) Tulving’s episodic/semantic distinction remains central in cognitive neuroscience. ([PMC][17]) Hippocampal indexing theory proposes that the hippocampus forms an index over distributed neocortical activity patterns. ([PubMed][18])

These ideas motivate separating active working state, episodic session evidence, stable project knowledge, and indexes into evidence.

---

## 14. Failure modes and risks

### 14.1 WSL ceremony risk

If WSL performs no better than clean YAML or XML under equal budgets, it is unnecessary. The benchmark must include strong structured-text baselines.

### 14.2 Observer poisoning

A bad observer can inject stale or false state into the main agent. The observer should be conservative, evidence-linked, and validated.

### 14.3 Scheduler unpredictability

Async memory management can make the foreground loop feel unstable if context changes unexpectedly. Injections should occur at safe boundaries: before agent turns, after tools, or on explicit page faults.

### 14.4 Overfitting to coding

The architecture targets coding sessions. General agents, legal research, scientific workflows, or customer-support agents may require different record types and policies.

### 14.5 Provider opacity

Hosted models may still apply hidden transformations, safety filters, or provider-managed context behavior. The runtime can control what it sends, but it cannot guarantee the provider’s internal handling.

### 14.6 False confidence

A clean working-state capsule may make the main agent sound more confident. The system should encourage page faults when evidence is missing.

---

## 15. Research roadmap

### Phase 1: Structured-state baseline

Build:

```text
ledger
WSL parser
simple observer
L1 capsule renderer
forced-reset benchmark
```

Compare:

```text
natural summary
YAML checkpoint
WSL capsule
```

### Phase 2: Scheduler and memory tiers

Add:

```text
L2 hot pages
L3 warm project memory
cold artifact store
residency scoring
page access history
```

Compare:

```text
WSL capsule
WSL+scheduler
WSL+scheduler+page faults
```

### Phase 3: Strong observer

Add:

```text
failure clustering
user-correction detection
stale assumption tracking
branch/commit invalidation
known-good command extraction
```

### Phase 4: Cross-harness adapter

Support:

```text
raw OpenAI-compatible API
Anthropic API
OpenRouter-style routing
local model endpoint
Pi-like local harness adapter
Codex/Claude compatibility mode
```

### Phase 5: Optional model training

Only after strong structured-text baselines:

```text
SFT an open-weight model on WSL reading/updating
RL for page-fault behavior and hard-constraint preservation
compare against untrained hosted models
```

Recent work such as CompactionRL suggests that training agents on compacted long-horizon trajectories may improve compaction-aware task execution, but the first prototype should avoid depending on provider retraining. ([arXiv][19])

---

## 16. Core thesis

The strongest version of the claim:

> LLM agent failure during long local coding sessions is often a working-set management failure. A harness can reduce this failure by maintaining a local asynchronous memory scheduler that represents operational state in a typed intermediate language, keeps only a compact resident working set in the model context, and uses page faults to recover exact evidence on demand.

The narrowness is important. This paper does not claim to solve all memory, all compaction, or all agent autonomy. It proposes a practical runtime architecture for keeping a main coding agent sharp across long synchronous work sessions.

---

## 17. Conclusion

The industry has treated LLM context as transcript, memory, scratchpad, execution log, and control surface. That design works surprisingly well for short tasks. It degrades during serious long-session work.

Working-State Virtual Memory reframes the problem. The active context is resident working memory. The ledger is durable truth. WSL is the internal operational state. The scheduler is the memory-management unit. Page faults recover exact evidence. The renderer speaks ordinary structured text to hosted models.

The result is a research agenda and prototype path that avoids asking providers to train on a new language. It works with today’s hosted models, while leaving room for future open-weight experiments.

The benchmark is brutal and simple: after hours of noisy work, compaction, resets, failed attempts, and user corrections, does the agent keep going intelligently?

That is the signal worth sending to the industry.

---

# Appendix A: WSL v0 sketch

```text
@task <id> phase=<phase> priority=<hot|warm|cold>
goal: <short symbolic or natural-language goal>
branch: <git branch>
commit: <sha or unknown>
dirty: <file list>

@constraint <id> <hard|soft> source=<user|repo|system|test>
text: "<exact constraint>"
scope: <global|repo|branch|task|file>

@failure <id>
cmd: `<command>`
exit: <integer>
err: "<exact error signature>"
file_hint: <path[:line-range]>
raw: <artifact pointer>

@decision <id>
chosen: "<decision>"
because: "<short reason>"
refs: <event ids>
scope: <repo|branch|task|file>

@avoid <id> reason=<failed_attempt|user_rejected|invalidated>
text: "<approach to avoid>"
ref: <failure|decision|event id>

@assumption <id> status=<active|invalidated|uncertain>
text: "<assumption>"
evidence: <event ids>

@next
action: <inspect|edit|test|ask|plan>
target: <file, command, or page>
question: "<question to resolve>"
```

---

# Appendix B: Example rendered capsule

```text
<working_state>
TASK T42: Fix stream cancellation hang.
PHASE: Debugging.
BRANCH: solver-stream-cancel.
DIRTY FILES: src/runtime/stream.go, src/solver/cancel.rs.

HARD CONSTRAINT C7:
Do not rewrite the transport layer.

LAST FAILURE F18:
Command: `go test ./runtime -run TestCancelStream`
Exit: 1
Error: "stream goroutine still blocked"
Likely file area: src/runtime/stream.go:118-176

AVOID A4:
Do not retry the previous goroutine cleanup patch; it failed in F16.

NEXT:
Inspect whether cancellation is propagated before stream recv blocks in src/runtime/stream.go:118-176.

When missing details, request a page by ID instead of guessing.
</working_state>
```

---

# Appendix C: One-sentence version

**A coding agent should not carry a transcript; it should operate from a managed working set.**

---

## Confidence level

**0.86**

## Key caveats

This is a strong research framing, but the novelty is architectural rather than magical. MemGPT, AIOS, Pichay, provider compaction, prompt compression, and agent-memory systems already occupy nearby territory. The demo has to prove that WSL plus an async scheduler beats strong YAML/Markdown checkpoints and provider compaction on forced-reset long-session coding tasks. If it cannot beat those baselines, the idea collapses into nice terminology.

[1]: https://dl.acm.org/doi/pdf/10.1145/363095.363141?utm_source=chatgpt.com "The working set model for program behavior"
[2]: https://arxiv.org/abs/2307.03172?utm_source=chatgpt.com "Lost in the Middle: How Language Models Use Long Contexts"
[3]: https://developers.openai.com/api/docs/guides/compaction?utm_source=chatgpt.com "Compaction | OpenAI API"
[4]: https://platform.claude.com/cookbook/tool-use-context-engineering-context-engineering-tools?utm_source=chatgpt.com "Context engineering: memory, compaction, and tool clearing"
[5]: https://arxiv.org/abs/2603.09023?utm_source=chatgpt.com "The Missing Memory Hierarchy: Demand Paging for LLM Context Windows"
[6]: https://arxiv.org/abs/2310.08560?utm_source=chatgpt.com "MemGPT: Towards LLMs as Operating Systems"
[7]: https://arxiv.org/abs/2309.06180?utm_source=chatgpt.com "Efficient Memory Management for Large Language Model Serving with PagedAttention"
[8]: https://openai.com/index/introducing-swe-bench-verified/?utm_source=chatgpt.com "Introducing SWE-bench Verified"
[9]: https://arxiv.org/abs/2507.00014?utm_source=chatgpt.com "SWE-Bench-CL: Continual Learning for Coding Agents"
[10]: https://arxiv.org/html/2310.05736v2?utm_source=chatgpt.com "LLMLingua: Compressing Prompts for Accelerated ..."
[11]: https://arxiv.org/html/2410.12388v2?utm_source=chatgpt.com "Prompt Compression for Large Language Models: A Survey"
[12]: https://platform.claude.com/docs/en/build-with-claude/compaction?utm_source=chatgpt.com "Compaction - Claude Platform Docs"
[13]: https://developers.openai.com/codex/memories?utm_source=chatgpt.com "Memories – Codex"
[14]: https://blog.cloudflare.com/introducing-agent-memory/?utm_source=chatgpt.com "Agents that remember: introducing Agent Memory"
[15]: https://arxiv.org/html/2403.16971v5?utm_source=chatgpt.com "AIOS: LLM Agent Operating System"
[16]: https://pubmed.ncbi.nlm.nih.gov/11058819/?utm_source=chatgpt.com "The episodic buffer: a new component of working memory?"
[17]: https://pmc.ncbi.nlm.nih.gov/articles/PMC2952732/?utm_source=chatgpt.com "Interdependence of episodic and semantic memory - PMC - NIH"
[18]: https://pubmed.ncbi.nlm.nih.gov/3008780/?utm_source=chatgpt.com "The hippocampal memory indexing theory - PubMed - NIH"
[19]: https://arxiv.org/html/2607.05378v1?utm_source=chatgpt.com "Reinforcement Learning with Context Compaction for Long ..."
