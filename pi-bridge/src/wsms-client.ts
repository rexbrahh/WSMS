/**
 * HTTP client for the WSMS core service (`wsms serve`).
 *
 * The bridge extension is one of two independent clients of the core (the Go
 * TUI is the other), so the transport is loopback HTTP/JSON. Every request
 * carries `Content-Type: application/json` — the core requires it as part of
 * its CSRF defense — and the optional bearer token. A per-request timeout keeps
 * a hung core from stalling the agent loop.
 */

export interface MaterializedPage {
	page_id: string;
	evidence: string[];
}

export interface SemanticResult {
	abstained: boolean;
	materialized: MaterializedPage[];
	explanation?: string;
	degraded?: string[];
	reason?: string;
}

export interface PageResult {
	found: boolean;
	body?: string;
	detail?: string;
}

export class WsmsClient {
	private readonly baseUrl: string;
	private readonly token: string;
	private readonly timeoutMs: number;

	constructor(opts?: { baseUrl?: string; token?: string; timeoutMs?: number }) {
		this.baseUrl = (opts?.baseUrl ?? process.env.WSMS_CORE_URL ?? "http://127.0.0.1:7673").replace(/\/$/, "");
		this.token = opts?.token ?? process.env.WSMS_SERVE_TOKEN ?? "";
		this.timeoutMs = opts?.timeoutMs ?? 5000;
	}

	async beforeTurn(): Promise<string> {
		const data = await this.post<{ capsule: string }>("/before_turn");
		return data.capsule ?? "";
	}

	ingestUser(text: string): Promise<void> {
		return this.postVoid("/ingest/user", { text });
	}

	ingestAssistant(text: string): Promise<void> {
		return this.postVoid("/ingest/assistant", { text });
	}

	ingestCommand(command: string, exit: number, output: string): Promise<void> {
		return this.postVoid("/ingest/command", { command, exit, output });
	}

	readPage(id: string): Promise<PageResult> {
		return this.post<PageResult>("/page", { id });
	}

	semantic(query: string): Promise<SemanticResult> {
		return this.post<SemanticResult>("/semantic", { query });
	}

	private async postVoid(path: string, body?: unknown): Promise<void> {
		await this.post<unknown>(path, body);
	}

	private async post<T>(path: string, body?: unknown): Promise<T> {
		const headers: Record<string, string> = { "Content-Type": "application/json" };
		if (this.token) headers.Authorization = `Bearer ${this.token}`;
		const resp = await fetch(this.baseUrl + path, {
			method: "POST",
			headers,
			body: JSON.stringify(body ?? {}),
			signal: AbortSignal.timeout(this.timeoutMs),
		});
		if (!resp.ok) {
			const detail = await resp.text().catch(() => "");
			throw new Error(`wsms ${path} → ${resp.status}: ${detail.slice(0, 200)}`);
		}
		return (await resp.json()) as T;
	}
}
