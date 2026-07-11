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

function streamEcho(model: Model<Api>, context: Context, options?: SimpleStreamOptions): AssistantMessageEventStream {
	const stream = createAssistantMessageEventStream();

	const user = lastUserText(context.messages).trim();
	const capsuleNote = sawCapsule(context.messages) ? " [capsule seen]" : "";
	const reply = `echo:${capsuleNote} ${user || "(nothing to echo)"}`;

	const output: AssistantMessage = {
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

	(async () => {
		try {
			const partial: AssistantMessage = { ...output, content: [] };
			stream.push({ type: "start", partial: { ...partial } });

			partial.content = [{ type: "text", text: "" }];
			stream.push({ type: "text_start", contentIndex: 0, partial: { ...partial } });

			for (const piece of chunkText(reply)) {
				if (options?.signal?.aborted) break;
				(partial.content[0] as { type: "text"; text: string }).text += piece;
				stream.push({ type: "text_delta", contentIndex: 0, delta: piece, partial: { ...partial } });
			}

			stream.push({ type: "text_end", contentIndex: 0, content: reply, partial: { ...partial } });

			const message: AssistantMessage = { ...output, content: [{ type: "text", text: reply }] };
			stream.push({ type: "done", reason: "stop", message });
			stream.end();
		} catch (error) {
			output.stopReason = "error";
			output.errorMessage = error instanceof Error ? error.message : String(error);
			stream.push({ type: "error", reason: "error", error: output });
			stream.end();
		}
	})();

	return stream;
}

/** Split into fixed-size pieces so the frontend sees genuine token-by-token deltas. */
function chunkText(text: string): string[] {
	const out: string[] = [];
	for (let i = 0; i < text.length; i += 4) out.push(text.slice(i, i + 4));
	return out.length > 0 ? out : [""];
}
