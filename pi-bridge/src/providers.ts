/**
 * Optional model-provider registration for the WSMS PoC.
 *
 * This encodes the Phase 9 credential policy: credentials come from env vars
 * only, no secret is ever inlined here, hosted providers stay OFF unless a key
 * is present, and the offline no-key path is the default — if nothing is
 * configured this registers nothing and pi falls back to its own model config.
 *
 * Both providers target the OpenAI-compatible `/v1/chat/completions` ABI via
 * `api: "openai-completions"`: the local path is llama.cpp `llama-server`, the
 * hosted path is OpenAI itself.
 */

import type { ExtensionAPI, ProviderModelConfig } from "@earendil-works/pi-coding-agent";

export function maybeRegisterProviders(pi: ExtensionAPI): void {
	registerLocal(pi);
	registerHosted(pi);
}

function registerLocal(pi: ExtensionAPI): void {
	const baseUrl = process.env.WSMS_LLAMA_BASE_URL;
	if (!baseUrl) return; // offline default: leave model selection to pi's own config

	pi.registerProvider("wsms-local", {
		baseUrl,
		// llama-server has no auth, but pi's openai-completions client rejects an
		// empty key. A placeholder satisfies it without introducing a real secret.
		apiKey: process.env.WSMS_LLAMA_API_KEY ?? "unused",
		api: "openai-completions",
		models: [localModel(process.env.WSMS_LLAMA_MODEL ?? "local-model")],
	});
}

function registerHosted(pi: ExtensionAPI): void {
	// Hosted stays off unless the key is present. The key is passed by reference
	// (`$OPENAI_API_KEY`), resolved by pi from the environment — never inlined.
	if (!process.env.OPENAI_API_KEY) return;

	pi.registerProvider("wsms-openai", {
		baseUrl: "https://api.openai.com/v1",
		apiKey: "$OPENAI_API_KEY",
		api: "openai-completions",
		models: [hostedModel(process.env.WSMS_OPENAI_MODEL ?? "gpt-4o-mini")],
	});
}

// TODO(owner decision): these descriptors are the one place domain knowledge
// matters — the model IDs you will actually serve, their real context windows,
// and (for the hosted path) truthful cost so pi's budgeting is honest. The
// defaults below are safe placeholders; adjust them to the models you run.

function localModel(id: string): ProviderModelConfig {
	const contextWindow = Number(process.env.WSMS_LLAMA_CONTEXT ?? 32768);
	return {
		id,
		name: id,
		reasoning: false,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow,
		maxTokens: 4096,
	};
}

function hostedModel(id: string): ProviderModelConfig {
	return {
		id,
		name: id,
		reasoning: false,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 128000,
		maxTokens: 16384,
	};
}
