/**
 * Keyless mock model provider for the WSMS PoC.
 *
 * This exists so the full three-process pipeline (TUI → pi → bridge → core) can
 * be exercised end-to-end with NO API key and NO network — honoring the Phase 9
 * credential policy where the offline path is the default. It is strictly opt-in
 * (`WSMS_MOCK_MODEL=1`): it never masquerades as a real model, it just echoes.
 *
 * It drives pi's own `AssistantMessageEventStream` exactly as a real provider
 * would (start → text_start → text_delta* → text_end → done), so the streamed
 * `message_update` envelopes the frontend receives are byte-identical in shape
 * to a genuine model's — which is the point: it validates the plumbing, not the
 * intelligence.
 *
 * It can also drive the tool round-trip on demand so the `wsms_read_page` /
 * `wsms_recall` seam is exercisable keyless: a user prompt of `read_page:<id>`
 * or `recall:<query>` makes it emit that tool call (start → toolcall_start →
 * toolcall_delta* → toolcall_end → done, stopReason "toolUse"); on the resume
 * turn it detects the returned `toolResult` in context and acknowledges it
 * tersely (citing the result's head), the way a real model would — the exact
 * fetched body is surfaced by the frontend as its own tool-result block, not
 * parroted back verbatim in the assistant text.
 */

import {
	type Api,
	type AssistantMessage,
	type AssistantMessageEventStream,
	type Context,
	createAssistantMessageEventStream,
	type Message,
	type Model,
	type SimpleStreamOptions,
	type ToolCall,
	type ToolResultMessage,
} from "@earendil-works/pi-ai";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const MOCK_PROVIDER = "wsms-mock";
const MOCK_API = "wsms-mock-api";
const MOCK_MODEL_ID = "wsms-echo";

export function maybeRegisterMockModel(pi: ExtensionAPI): void {
	if (process.env.WSMS_MOCK_MODEL !== "1") return;

	pi.registerProvider(MOCK_PROVIDER, {
		// baseUrl/apiKey are unused by streamSimple but the config type wants them;
		// no secret is involved because no request ever leaves the process.
		baseUrl: "http://mock.invalid",
		apiKey: "unused",
		api: MOCK_API as Api,
		streamSimple: streamEcho,
		models: [
			{
				id: MOCK_MODEL_ID,
				name: "WSMS Echo (offline mock)",
				reasoning: false,
				input: ["text"],
				cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
				contextWindow: 32768,
				maxTokens: 4096,
			},
		],
	});
}

/** Extract the newest user-authored text from the model context. */
function lastUserText(messages: Message[]): string {
	for (let i = messages.length - 1; i >= 0; i--) {
		const msg = messages[i];
		if (msg.role !== "user") continue;
		if (typeof msg.content === "string") return msg.content;
		if (Array.isArray(msg.content)) {
			return msg.content
				.filter((c): c is { type: "text"; text: string } => !!c && (c as { type?: string }).type === "text")
				.map((c) => c.text)
				.join("");
		}
	}
	return "";
}

/** True if the injected WSMS working-state capsule is present in context. */
function sawCapsule(messages: Message[]): boolean {
	return messages.some((m) => {
		const text = typeof m.content === "string" ? m.content : JSON.stringify(m.content ?? "");
		return text.includes("<working_state>");
	});
}

/** Newest tool result in context, marking a turn that resumes after a tool call. */
function lastToolResult(messages: Message[]): ToolResultMessage | undefined {
	for (let i = messages.length - 1; i >= 0; i--) {
		if (messages[i].role === "toolResult") return messages[i] as ToolResultMessage;
	}
	return undefined;
}

/** Join the text parts of a content array (tool results, message content). */
function textParts(content: unknown): string {
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";
	return content
		.filter((c): c is { type: "text"; text: string } => !!c && (c as { type?: string }).type === "text")
		.map((c) => c.text)
		.join("");
}

/**
 * Parse an explicit tool-call directive from the user text so the round-trip is
 * deterministically drivable keyless. `read_page:<id>` and `recall:<query>` map
 * to the bridge's two tools; anything else is a plain echo.
 */
function parseToolDirective(user: string): { name: string; arguments: Record<string, unknown> } | undefined {
	const page = user.match(/^read_page:\s*(\S+)/);
	if (page) return { name: "wsms_read_page", arguments: { id: page[1] } };
	const recall = user.match(/^recall:\s*(.+)/);
	if (recall) return { name: "wsms_recall", arguments: { query: recall[1].trim() } };
	return undefined;
}

/** A fresh assistant-message skeleton stamped for this model. */
function baseMessage(model: Model<Api>): AssistantMessage {
	return {
		role: "assistant",
		content: [],
		api: model.api,
		provider: model.provider,
		model: model.id,
		usage: {
			input: 0,
			output: 0,
			cacheRead: 0,
			cacheWrite: 0,
			totalTokens: 0,
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
		},
		stopReason: "stop",
		timestamp: Date.now(),
	};
}

function streamEcho(model: Model<Api>, context: Context, options?: SimpleStreamOptions): AssistantMessageEventStream {
	const stream = createAssistantMessageEventStream();
	const base = baseMessage(model);

	// A returned tool result means we are resuming after a tool call — echo it
	// (do NOT re-emit the tool call, which would loop). Otherwise honor an
	// explicit tool directive, else plain-echo the user text.
	const toolResult = lastToolResult(context.messages);
	const directive = toolResult ? undefined : parseToolDirective(lastUserText(context.messages).trim());

	(async () => {
		try {
			if (directive) {
				emitToolCall(stream, base, directive, options);
			} else if (toolResult) {
				// Acknowledge the fetched evidence tersely — cite its head, not its
				// whole body. The frontend renders the exact result as its own
				// collapsible block, so echoing it in full here would duplicate it
				// (and a real model wouldn't parrot a page back verbatim either).
				const head = textParts(toolResult.content).split("\n")[0]?.trim() ?? "";
				emitText(stream, base, `echo: applied ${toolResult.toolName} → ${head || "(no content)"}`, options);
			} else {
				const capsuleNote = sawCapsule(context.messages) ? " [capsule seen]" : "";
				const user = lastUserText(context.messages).trim();
				emitText(stream, base, `echo:${capsuleNote} ${user || "(nothing to echo)"}`, options);
			}
		} catch (error) {
			base.stopReason = "error";
			base.errorMessage = error instanceof Error ? error.message : String(error);
			stream.push({ type: "error", reason: "error", error: base });
			stream.end();
		}
	})();

	return stream;
}

/** Stream one text message: start → text_start → text_delta* → text_end → done. */
function emitText(
	stream: AssistantMessageEventStream,
	base: AssistantMessage,
	reply: string,
	options?: SimpleStreamOptions,
): void {
	const partial: AssistantMessage = { ...base, content: [] };
	stream.push({ type: "start", partial: { ...partial } });

	partial.content = [{ type: "text", text: "" }];
	stream.push({ type: "text_start", contentIndex: 0, partial: { ...partial } });

	for (const piece of chunkText(reply)) {
		if (options?.signal?.aborted) break;
		(partial.content[0] as { type: "text"; text: string }).text += piece;
		stream.push({ type: "text_delta", contentIndex: 0, delta: piece, partial: { ...partial } });
	}

	stream.push({ type: "text_end", contentIndex: 0, content: reply, partial: { ...partial } });

	const message: AssistantMessage = { ...base, content: [{ type: "text", text: reply }] };
	stream.push({ type: "done", reason: "stop", message });
	stream.end();
}

/**
 * Stream one tool call the way a real provider does: start → toolcall_start →
 * toolcall_delta*(JSON-string fragments of the arguments) → toolcall_end →
 * done. The agent loop reads the authoritative arguments from the final
 * message's ToolCall block, so the deltas are cosmetic streaming; the terminal
 * stopReason must be "toolUse" for the loop to dispatch the tool.
 */
function emitToolCall(
	stream: AssistantMessageEventStream,
	base: AssistantMessage,
	directive: { name: string; arguments: Record<string, unknown> },
	options?: SimpleStreamOptions,
): void {
	const call: ToolCall = {
		type: "toolCall",
		id: `mock-${directive.name}-${Date.now()}`,
		name: directive.name,
		arguments: directive.arguments,
	};

	const partial: AssistantMessage = { ...base, content: [] };
	stream.push({ type: "start", partial: { ...partial } });

	partial.content = [{ ...call, arguments: {} }];
	stream.push({ type: "toolcall_start", contentIndex: 0, partial: { ...partial } });

	for (const piece of chunkText(JSON.stringify(call.arguments))) {
		if (options?.signal?.aborted) break;
		stream.push({ type: "toolcall_delta", contentIndex: 0, delta: piece, partial: { ...partial } });
	}

	partial.content = [call];
	stream.push({ type: "toolcall_end", contentIndex: 0, toolCall: call, partial: { ...partial } });

	const message: AssistantMessage = { ...base, content: [call], stopReason: "toolUse" };
	stream.push({ type: "done", reason: "toolUse", message });
	stream.end();
}

/** Split into fixed-size pieces so the frontend sees genuine token-by-token deltas. */
function chunkText(text: string): string[] {
	const out: string[] = [];
	for (let i = 0; i < text.length; i += 4) out.push(text.slice(i, i + 4));
	return out.length > 0 ? out : [""];
}
