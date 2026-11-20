import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import type { SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";
import { FakeAnthropicApi } from "./fake-anthropic-api.js";
import { PhaseClaude } from "./phase-model-claude.js";

const nonce = `phase-${randomUUID()}`;
const wire: Array<Record<string, unknown>> = [];
let runtime: PhaseClaude;
type Turn = Awaited<ReturnType<PhaseClaude["turn"]>>;
let a: Turn, b: Turn, back: Turn, omitted: Turn;
let barrier: { beforePrompt: boolean; requestsAfterAbort: number; events: string[] };
const fake = new FakeAnthropicApi(body => {
  runtime.events.push("request");
  wire.push(body as unknown as Record<string, unknown>);
  return { kind: "text", text: nonce };
});
before(async () => {
  runtime = new PhaseClaude({ ANTHROPIC_BASE_URL: await fake.listen(), ANTHROPIC_API_KEY: "offline-phase-fixture" });
  a = await runtime.turn({ model: "opus", effort: "high", prompt: `Remember ${nonce}. Say it.` });
  b = await runtime.turn({ model: "sonnet", effort: "medium", resume: a.session, prompt: "Repeat the nonce." });
  back = await runtime.turn({ model: "opus", effort: "high", resume: a.session, prompt: "Repeat it again." });
  omitted = await runtime.turn({ resume: a.session, prompt: "Repeat it once more." });
  for (const turn of [a, b, back, omitted]) assert.equal(turn.failure, undefined);
  const count = wire.length;
  const aborted = await runtime.turn({ model: "sonnet", effort: "medium", prompt: "Barrier abort control", abortOnInit: true });
  assert.equal(aborted.aborted, true);
  const requestsAfterAbort = wire.length - count;
  let release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  let consumed = false, beforePrompt = false;
  async function* prompt(): AsyncGenerator<SDKUserMessage> {
    await gate;
    consumed = true;
    runtime.events.push("prompt");
    yield { type: "user", session_id: "", parent_tool_use_id: null, message: { role: "user", content: "Gated prompt control" } };
  }
  const start = runtime.events.length;
  // A successful settings reply proves CLI readiness while the prompt is still withheld.
  // The timeout is failure protection, never evidence that a barrier was established.
  const fallback = setTimeout(release, 5_000);
  try {
    await runtime.turn({ model: "sonnet", prompt: prompt(), onInit: () => { beforePrompt = !consumed; release(); },
      onReady: () => { runtime.events.push("settings-ready"); release(); } });
  } finally { clearTimeout(fallback); }
  barrier = { beforePrompt, requestsAfterAbort, events: runtime.events.slice(start) };
}, { timeout: 110_000 });
after(async () => { await runtime?.close(); await fake.close(); });

test("Claude phase case 1: resumed provider request switches model and effort with nonce history", () => {
  assert.match(a.model, /opus/);
  assert.match(b.model, /sonnet/);
  assert.equal(wire[1]?.model, b.model);
  assert.deepEqual(wire[1]?.output_config, { effort: "medium" });
  assert.equal(wire[1]?.max_tokens, 256, "the probe's provider output cap is applied");
  assert.equal(b.effort, "medium");
  assert.match(JSON.stringify(wire[1]), new RegExp(nonce));
});
test("Claude phase case 2: a fresh bundled CLI process resumes the same session", () => {
  assert.equal(b.session, a.session);
  assert.ok(a.pid > 0 && b.pid > 0 && a.pid !== b.pid);
});
test("Claude phase case 3: re-plan switches back on the same session", () => {
  assert.equal(back.session, a.session);
  assert.equal(back.model, a.model);
  assert.equal(wire[2]?.model, a.model);
  assert.equal(back.effort, "high");
});
test("Claude phase case 4: omitted model retains the explicit planning model", () => {
  assert.equal(omitted.model, a.model);
  assert.equal(wire[3]?.model, a.model);
});
test("Claude phase case 5: omitted effort reverts from high to the runtime medium default", () => {
  assert.equal(back.effort, "high");
  assert.equal(omitted.effort, "medium");
  assert.deepEqual(wire[3]?.output_config, { effort: "medium" });
});
test("Claude phase case 6: result modelUsage retains both producing models after a switch", () => {
  assert.deepEqual(Object.keys(b.usage as object).sort(), [a.model, b.model].sort());
});
test("Claude phase case 9: init precedes requests but is not a prompt or abort admission barrier", () => {
  assert.ok(runtime.events.indexOf("init") < runtime.events.indexOf("request"));
  assert.equal(barrier.beforePrompt, false);
  assert.deepEqual(barrier.events, ["settings-ready", "prompt", "init", "request"]);
  assert.equal(barrier.requestsAfterAbort, 1);
  console.log(`PHASE_CLAUDE_EVIDENCE ${JSON.stringify({ sdk: "0.3.319", cli: a.cli,
    models: [a.model, b.model, back.model, omitted.model], efforts: [a.effort, b.effort, back.effort, omitted.effort],
    switchedUsageModels: Object.keys(b.usage as object), barrier })}`);
});
