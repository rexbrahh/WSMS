# WSMS pi bridge

A [pi](https://github.com/earendil-works/pi) harness extension that gives the
agent loop durable, evidence-grounded working memory backed by the WSMS core
service (`wsms serve`). It rides pi's public extension seams, so **pi's source is
not modified** — the fork is pinned only for control.

## Architecture

Three processes (Phase 9 "Config B"):

```
Go bubbletea TUI ──drives──▶ pi (--mode rpc) ──loads──▶ this extension
       │                                                      │
       └───────────── HTTP ──▶ wsms serve (core) ◀── HTTP ────┘
```

This extension and the TUI are two independent clients of the loopback
`wsms serve` HTTP/JSON API.

## What it wires

| pi seam | WSMS core call | Effect |
|---|---|---|
| `context` | `POST /before_turn` | Prepend the freshly compiled capsule before every model call (ephemeral) |
| `message_end` (user/assistant) | `POST /ingest/{user,assistant}` | Record finalized messages in the durable ledger |
| `tool_result` | `POST /ingest/command` | Record command executions (real command + output) as evidence |
| `wsms_read_page` tool | `POST /page` | Demand-fetch a page's exact body by ID |
| `wsms_recall` tool | `POST /semantic` | Semantic recall over durable memory, or abstain |

Every core call is best-effort: if `wsms serve` is slow or down, the handler
degrades to a no-op so the agent loop never blocks or crashes.

## Load it

```bash
wsms serve --data-dir .wsms &          # start the core (loopback:7673)
pi -e ./pi-bridge/src/index.ts          # or symlink into .pi/extensions/
```

TypeScript loads directly via jiti — no build step.

## Offline end-to-end demo (no API key)

`WSMS_MOCK_MODEL=1` registers a keyless echo provider (`wsms-mock/wsms-echo`)
that drives pi's real streaming path with canned tokens — so the whole
three-process pipeline runs with no credential and no network, which is the
default demo path. It is strictly opt-in and never masquerades as a real model.

```bash
export WSMS_MOCK_MODEL=1
wsms serve --data-dir .wsms &
wsms-tui \
  --pi "$(pwd)/third_party/pi/pi-test.sh" \
  --extension "$(pwd)/pi-bridge/src/index.ts" \
  --provider wsms-mock --model wsms-echo
```

The streamed `message_update` envelopes this produces are shape-identical to a
real model's, which is how the TUI's event extraction was verified against live
pi output (`internal/tui`: incremental text is read from
`assistantMessageEvent.delta`, gated on `type == "text_delta"`).

## TUI keys

The frontend is keyboard-first, with a few mouse affordances layered on. The
resting view stays scannable — verbose detail is one keystroke or one click away,
never crammed in by default.

| Input | Effect |
|---|---|
| type + `enter` | send a message to pi |
| `tab` | switch focus between the chat and memory panes |
| `^O` | expand/collapse every verbose tool-result block at once |
| `↑`/`↓` (or `k`/`j`) | move the selection in the memory pane |
| `space` | expand/collapse the selected memory section |
| **click** a `↳` tool-result block | expand/collapse just that block |
| **click** a memory section header | focus the pane and toggle that section |
| `esc` | back out of the memory pane / quit from chat |
| `^C` | quit |

Tool results render as their own `↳ toolName` block holding the exact evidence pi
returned — collapsed to a short preview (the head lines plus a `+N more` marker)
until expanded, so the model's prose and the raw tool output stay visually
distinct.

## Interaction contract

How the bridge behaves under pi's runtime seams. Grounded in pi's actual
implementation (packages `agent`, `ai`, `coding-agent`), not assumed.

**Streaming.** Incremental assistant text is the provider's
`assistantMessageEvent.delta`, gated on `type == "text_delta"`; the authoritative
final text is adopted at `message_end`. Tool calls stream as
`toolcall_start → toolcall_delta* → toolcall_end` with a terminal `stopReason`
of `toolUse`. The bridge ingests only finalized `message_end` user/assistant
text (skipping empty, i.e. tool-call-only, turns) — never partial deltas — so a
retried or aborted stream never writes partial evidence to L4.

**Cancellation.** pi threads the run's `AbortSignal` into both the provider
stream and each tool's `execute(id, params, signal)`. The `wsms_read_page` /
`wsms_recall` tools forward that signal into the core HTTP request, so a user
abort cancels an in-flight page/recall fetch. An aborted turn ends with
`stopReason: "aborted"` and is treated as no durable evidence. Every core call
is wrapped in `safe()`: a cancelled or failed call degrades the handler to a
no-op and never propagates into pi's failure path.

**Timeout.** pi imposes no wall-clock deadline on a model call or a tool
execution — only cancellation. The bridge adds its own bound: each core request
aborts on whichever fires first, the run signal or a 5 s per-request timeout, so
a hung or missing core can never stall the agent loop. Ingestion timing out is
silently dropped (best-effort); a page/recall timing out surfaces to the model
as a normal tool error, and the model may retry or proceed.

**Provider-compaction.** pi compacts its own conversation history
(summarize-then-drop) between runs when it nears the context window. This never
threatens WSMS memory, by construction:

- The capsule is injected through the ephemeral `context` hook — recomputed
  fresh every turn from the live ledger, never written back into pi's transcript,
  so it is never itself compacted or summarized away.
- The durable L4 ledger is the real memory and is built incrementally from
  `message_end` / `tool_result` as the turn happens — *before* any compaction —
  so whatever compaction later drops is already recorded exactly.
- Any verbatim detail compaction discards remains page-faultable via
  `wsms_read_page` / `wsms_recall`.

Net: pi's compaction summary is a pi-internal context-management artifact with no
authority here; WSMS treats only real user/assistant/tool events as evidence, and
the capsule plus page-fault recover anything compaction removes. WSMS makes pi's
lossy compaction non-destructive.

## Configuration (env only)

| Var | Purpose | Default |
|---|---|---|
| `WSMS_CORE_URL` | Core service base URL | `http://127.0.0.1:7673` |
| `WSMS_SERVE_TOKEN` | Bearer token, if the core requires one | unset |
| `WSMS_MOCK_MODEL` | Register the keyless offline echo model (`=1`) | unset (no mock) |
| `WSMS_LLAMA_BASE_URL` | Register a local llama-server provider | unset (offline default registers none) |
| `WSMS_LLAMA_MODEL` / `WSMS_LLAMA_CONTEXT` | Local model id / context window | `local-model` / `32768` |
| `OPENAI_API_KEY` | Enable the hosted OpenAI provider | unset (hosted off) |
| `WSMS_OPENAI_MODEL` | Hosted model id | `gpt-4o-mini` |

Credential policy: env-var-only, no secret is inlined, hosted providers stay off
unless a key is present, and the offline no-key path is the default. See
`src/providers.ts`.

## Test

```bash
go build -o /tmp/wsms ./cmd/wsms
WSMS_BIN=/tmp/wsms node pi-bridge/test/client.smoke.mjs
```

Exercises the `WsmsClient` ↔ live core seam (including the core's CSRF
hardening).

The pi-facing handlers are verified end-to-end against a real pi runtime: with
`WSMS_MOCK_MODEL=1` the bridge loads, injects the capsule (the model observes
`<working_state>` in its context), and ingests the finalized user/assistant
turn into the durable ledger — all with no API key.
