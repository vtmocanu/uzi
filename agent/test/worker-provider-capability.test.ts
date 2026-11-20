import { it } from "node:test";
import assert from "node:assert/strict";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { CLAUDE_PROVIDER_CAPABILITY } from "../src/provider-binding.js";

// PRD #4320 Decision Log, "M2 finding: the pinned Claude CLI follows gateway redirects". Until a
// redirect-refusing relay lands, a provider-bound claim would let the CLI re-send the gateway key and
// request body to a Location the gateway names, so the worker must NOT advertise claude_provider_v1.
// The api's claim clauses then never hand it a provider-bound run. This test flips (to an assertion
// that the capability IS advertised) in the change that lands the relay.
it("the worker does not advertise claude_provider_v1 on registration", async () => {
  const { Worker } = await import("../src/worker.js");
  const { WorkerClient } = await import("../src/client.js");
  const { ActiveRunRegistry } = await import("../src/active-run-registry.js");
  const { nullLogger } = await import("./helpers.js");
  const requests: { path: string; body: Record<string, unknown> }[] = [];
  const client = new WorkerClient("http://example.test", "join-fixture", "prov", nullLogger(), {
    fetch: async (input, init) => {
      const path = String(input);
      requests.push({ path, body: JSON.parse(String(init?.body)) });
      return new Response(JSON.stringify({ worker_id: "worker", register_nonce: Buffer.alloc(32, 3).toString("base64url") }));
    },
  });
  const worker = new Worker({
    workerName: "prov", workerTemplate: "base", maxConcurrentRuns: 1, crossCheckSlots: 0,
    heartbeatIntervalMs: 15_000, pollIntervalMs: 1000, dataDir: ".",
    codexHarness: { advertise: false },
  } as Config, client, {} as RunRunner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
  nullLogger(), () => ({ ok: true, missing: [] }), undefined, undefined, new ActiveRunRegistry());
  await (worker as unknown as { registerWithRetry(s: AbortSignal): Promise<void> }).registerWithRetry(new AbortController().signal);
  const reg = requests.find(r => r.path.endsWith("/register"));
  assert.ok(reg, "the worker registered");
  const caps = reg.body.protocol_capabilities as string[];
  assert.ok(Array.isArray(caps) && caps.length > 0, "the registration carries a protocol capability list");
  assert.equal(caps.includes(CLAUDE_PROVIDER_CAPABILITY), false);
  assert.equal(caps.includes("claude_provider_v1"), false);
});
