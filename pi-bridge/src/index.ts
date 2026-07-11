/**
 * WSMS bridge extension for the pi harness.
 *
 * Wires pi's public extension seams to the WSMS core service so the agent loop
 * gains durable, evidence-grounded working memory without pi's source being
 * modified:
 *
 *   - `context`             → inject the freshly compiled WSMS capsule before
 *                             every model call (ephemeral, recomputed per turn).
 *   - `message_end`         → ingest finalized user / assistant messages.
 *   - `tool_result`         → ingest command executions (the actual command +
 *                             its output) as durable evidence.
 *   - `wsms_read_page` tool → demand-fetch a page's exact body by ID.
 *   - `wsms_recall` tool    → semantic recall over durable memory (or abstain).
 *
 * Every core call is best-effort: WSMS being slow or down must never block or
 * crash the agent loop (an extension handler that throws propagates to pi's
 * failure path), so each is wrapped and degrades to a no-op.
 */

import type { AgentMessage, ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { WsmsClient } from "./wsms-client.ts";
import { maybeRegisterProviders } from "./providers.ts";
import { maybeRegisterMockModel } from "./mock-provider.ts";

const OWN_TOOLS = new Set(["wsms_read_page", "wsms_recall"]);

export default function (pi: ExtensionAPI): void {
	const client = new WsmsClient();

	maybeRegisterProviders(pi);
	maybeRegisterMockModel(pi);

	// Inject the WSMS capsule ahead of the real conversation on every model call.
	pi.on("context", async (event) => {
		const capsule = await safe(() => client.beforeTurn());
		if (!capsule) return; // core down or empty capsule → inject nothing
		const message: AgentMessage = {
			role: "user",
			content: [{ type: "text", text: capsule }],
			timestamp: Date.now(),
		} as AgentMessage;
		return { messages: [message, ...event.messages] };
	});

	// Observe finalized messages and record them in the durable ledger.
	pi.on("message_end", async (event) => {
		const message = event.message;
		if (message.role === "user") {
			await safe(() => client.ingestUser(textOf(message.content)));
		} else if (message.role === "assistant") {
			await safe(() => client.ingestAssistant(textOf(message.content)));
		}
	});

	// Record tool executions as command evidence. tool_result (not
	// tool_execution_end) carries the input args, so we recover the real command.
	pi.on("tool_result", async (event) => {
		if (OWN_TOOLS.has(event.toolName)) return; // don't ingest our own memory tools
		const command =
			event.toolName === "bash" && typeof (event.input as { command?: unknown })?.command === "string"
				? ((event.input as { command: string }).command)
				: `${event.toolName} ${JSON.stringify(event.input ?? {})}`;
		await safe(() => client.ingestCommand(command, event.isError ? 1 : 0, textOf(event.content)));
	});

	pi.registerTool({
		name: "wsms_read_page",
		label: "WSMS Read Page",
		description:
			"Fetch the exact body of a WSMS memory page by its page ID. Use when the working-state capsule references a page ID whose details you need verbatim, instead of guessing.",
		promptSnippet: "Fetch a WSMS memory page's exact body by ID",
		parameters: Type.Object({
			id: Type.String({ description: "The WSMS page ID to fetch" }),
		}),
		async execute(_toolCallId, params) {
			const res = await client.readPage(params.id);
			const text = res.found ? (res.body ?? "") : `No page found for ${params.id}: ${res.detail ?? "unknown"}`;
			return { content: [{ type: "text", text }], details: res };
		},
	});

	pi.registerTool({
		name: "wsms_recall",
		label: "WSMS Recall",
		description:
			"Semantic search over durable WSMS memory. Returns exact evidence pages when a match clears authority checks, or abstains when nothing eligible is found. Absence is a valid answer — do not fabricate memory it does not return.",
		promptSnippet: "Recall durable memory by meaning; abstains when nothing matches",
		parameters: Type.Object({
			query: Type.String({ description: "What to recall from durable memory" }),
		}),
		async execute(_toolCallId, params) {
			const res = await client.semantic(params.query);
			if (res.abstained) {
				return {
					content: [{ type: "text", text: `No durable memory found (${res.reason ?? "abstained"}).` }],
					details: res,
				};
			}
			const rendered = res.materialized
				.map((page) => `# ${page.page_id}\n${(page.evidence ?? []).join("\n")}`)
				.join("\n\n");
			return {
				content: [{ type: "text", text: rendered || "Matched pages carried no evidence." }],
				details: res,
			};
		},
	});
}

/** Extract plain text from a message/content payload across its several shapes. */
function textOf(content: unknown): string {
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";
	return content
		.filter((c): c is { type: "text"; text: string } => !!c && (c as { type?: string }).type === "text")
		.map((c) => c.text)
		.join("");
}

/** Run a best-effort core call; on any failure degrade to undefined. */
async function safe<T>(fn: () => Promise<T>): Promise<T | undefined> {
	try {
		return await fn();
	} catch {
		return undefined;
	}
}
