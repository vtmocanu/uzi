import { it } from "node:test";
import assert from "node:assert/strict";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { CLAUDE_PROVIDER_CAPABILITY, CLAUDE_PROVIDER_JUDGE_CAPABILITY, CODEX_PROVIDER_CAPABILITY } from "../src/provider-binding.js";

// PRD #4320 / #4505: the pinned Claude CLI follows gateway redirects (see the PRD Decision Log), and
// that risk was accepted on 2026-11-19 (ADR-4320), so the worker advertises claude_provider_v1 on
// registration and the api may hand it provider-bound runs. The relay (#4496) is the future mitigation.
async function registeredCaps(advertise: boolean): Promise<string[]> {
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
    codexHarness: { advertise },
  } as Config, client, {} as RunRunner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
  nullLogger(), () => ({ ok: true, missing: [] }), undefined, undefined, new ActiveRunRegistry());
  await (worker as unknown as { registerWithRetry(s: AbortSignal): Promise<void> }).registerWithRetry(new AbortController().signal);
  const reg = requests.find(r => r.path.endsWith("/register"));
  assert.ok(reg, "the worker registered");
  const caps = reg.body.protocol_capabilities as string[];
  assert.ok(Array.isArray(caps) && caps.length > 0, "the registration carries a protocol capability list");
  return caps;
}

it("the worker advertises claude_provider_v1 on registration", async () => {
  const caps = await registeredCaps(false);
  assert.equal(caps.includes(CLAUDE_PROVIDER_CAPABILITY), true);
  assert.equal(caps.includes("claude_provider_v1"), true);
});

// PRD #4322 M1a: the Claude provider judge path ships in this image; the api places a provider judge
// only on a worker advertising claude_provider_judge_v1, with or without the Codex harness.
it("the worker advertises claude_provider_judge_v1 on registration", async () => {
  for (const advertise of [false, true]) {
    const caps = await registeredCaps(advertise);
    assert.equal(caps.includes(CLAUDE_PROVIDER_JUDGE_CAPABILITY), true);
    assert.equal(caps.includes("claude_provider_judge_v1"), true);
  }
});

// PRD #4323: codex_provider_v1 rides only with the Codex harness, and implies codex_custom_model_v1.
it("a Codex-harness worker advertises codex_provider_v1 together with the Codex runtime capabilities", async () => {
  const caps = await registeredCaps(true);
  for (const c of [CODEX_PROVIDER_CAPABILITY, "codex_provider_v1", "codex_custom_model_v1", "codex_phase_models_v1", "codex_harness_v1", "codex_runtime_v2"]) {
    assert.equal(caps.includes(c), true, c);
  }
});

it("a worker without the Codex harness does not advertise codex_provider_v1", async () => {
  const caps = await registeredCaps(false);
  assert.equal(caps.includes("codex_provider_v1"), false);
  assert.equal(caps.includes("codex_custom_model_v1"), false);
  assert.equal(caps.includes("codex_harness_v1"), false);
});
