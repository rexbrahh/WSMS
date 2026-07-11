// Integration smoke test: the bridge's WsmsClient against a live `wsms serve`.
//
// Validates the exact HTTP seam the extension rides — including the core's
// CSRF hardening (JSON content-type, loopback Host) — with no pi involved.
// Run: WSMS_BIN=/path/to/wsms node pi-bridge/test/client.smoke.mjs

import { spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import assert from "node:assert/strict";

const bin = process.env.WSMS_BIN;
assert.ok(bin, "set WSMS_BIN to the built wsms binary");

const dataDir = mkdtempSync(join(tmpdir(), "wsms-bridge-"));
const proc = spawn(bin, ["serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir], { stdio: ["ignore", "ignore", "pipe"] });

function waitForAddr() {
	return new Promise((resolve, reject) => {
		const timer = setTimeout(() => reject(new Error("timed out waiting for listen line")), 10000);
		let buf = "";
		proc.stderr.on("data", (chunk) => {
			buf += chunk.toString();
			const m = buf.match(/listening on http:\/\/(\S+)/);
			if (m) {
				clearTimeout(timer);
				resolve(m[1]);
			}
		});
		proc.on("exit", (code) => reject(new Error(`serve exited early: ${code}`)));
	});
}

let failed = false;
try {
	const addr = await waitForAddr();
	process.env.WSMS_CORE_URL = `http://${addr}`;

	const { WsmsClient } = await import("../src/wsms-client.ts");
	const client = new WsmsClient();

	const capsule = await client.beforeTurn();
	assert.equal(typeof capsule, "string", "beforeTurn returns a string capsule");

	await client.ingestUser("bridge client smoke test");
	await client.ingestAssistant("acknowledged");
	await client.ingestCommand("echo hi", 0, "hi");

	const page = await client.readPage("nonexistent-page-id");
	assert.equal(page.found, false, "unknown page reports found:false, not an error");

	const recall = await client.semantic("anything at all");
	assert.equal(recall.abstained, true, "fresh session abstains rather than fabricating");
	assert.ok(Array.isArray(recall.materialized), "materialized is present");

	console.log("PASS: bridge WsmsClient ↔ live wsms core");
} catch (err) {
	failed = true;
	console.error("FAIL:", err?.message ?? err);
} finally {
	proc.kill("SIGTERM");
}
process.exit(failed ? 1 : 0);
