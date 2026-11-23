import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import type { Server } from "node:http";
import type { SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";
import { FakeAnthropicApi } from "./fake-anthropic-api.js";
import { CLAUDE_PHASE_SDK, PhaseClaude } from "./phase-model-claude.js";

const nonce = `phase-${randomUUID()}`;
const wire: Array<Record<string, unknown>> = [];
let runtime: PhaseClaude;
let arrivals = 0;
type Turn = Awaited<ReturnType<PhaseClaude["turn"]>>;
let a: Turn, b: Turn, back: Turn, omitted: Turn, runtimeDefault: Turn, runtimeDefaultAfter: Turn, explicitNondefault: Turn, omittedModel: Turn;
let barrier: { beforePrompt: boolean; requestsAfterAbort: number; events: string[]; timeline: Array<{ event: string; atMs: number }> };
const fake = new FakeAnthropicApi(body => {
  wire.push(body as unknown as Record<string, unknown>);
  return { kind: "text", text: nonce };
});
// Instrument HTTP header arrival, not respond(), which runs only after the body is complete.
const server = (fake as unknown as { server: Server }).server;
server.prependListener("request", req => {
  if (req.method === "POST" && req.url?.split("?")[0] === "/v1/messages") {
    arrivals++;
    runtime.mark("request");
  }
});
before(async () => {
  runtime = new PhaseClaude({ ANTHROPIC_BASE_URL: await fake.listen(), ANTHROPIC_API_KEY: "offline-phase-fixture" });
  a = await runtime.turn({ model: "opus", effort: "high", prompt: `Remember ${nonce}. Say it.` });
  b = await runtime.turn({ model: "sonnet", effort: "medium", resume: a.session, prompt: "Repeat the nonce." });
  back = await runtime.turn({ model: "opus", effort: "high", resume: a.session, prompt: "Repeat it again." });
  omitted = await runtime.turn({ model: "opus", resume: a.session, prompt: "Repeat it once more." });
  const clean = new PhaseClaude(runtime.providerEnv);
  try {
    runtimeDefault = await clean.turn({ prompt: "Default model control." });
    explicitNondefault = await clean.turn({ model: "sonnet", effort: "high", prompt: `Remember ${nonce}.` });
    omittedModel = await clean.turn({ resume: explicitNondefault.session, effort: "medium", prompt: "Repeat the nonce." });
    runtimeDefaultAfter = await clean.turn({ prompt: "Fresh default control after the explicit model." });
  } finally { await clean.close(); }
  for (const turn of [a, b, back, omitted]) assert.equal(turn.failure, undefined);
  const abortSeed = await runtime.turn({ model: "opus", effort: "high", prompt: "Plan-session seed for the abort barrier." });
  const count = arrivals;
  const aborted = await runtime.turn({ model: "sonnet", effort: "medium", resume: abortSeed.session, prompt: "Barrier abort control", abortOnInit: true });
  assert.equal(aborted.aborted, true);
  const requestsAfterAbort = arrivals - count;
  const gateSeed = await runtime.turn({ model: "opus", effort: "high", prompt: "Plan-session seed for the gated prompt." });
  let release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  let consumed = false, beforePrompt = false;
  async function* prompt(): AsyncGenerator<SDKUserMessage> {
    await gate;
    consumed = true;
    runtime.mark("prompt");
    yield { type: "user", session_id: "", parent_tool_use_id: null, message: { role: "user", content: "Gated prompt control" } };
  }
  const start = runtime.events.length;
  // A successful settings reply proves CLI readiness while the prompt is still withheld.
  // The timeout is failure protection, never evidence that a barrier was established.
  const fallback = setTimeout(release, 5_000);
  let readyRelease: ReturnType<typeof setTimeout> | undefined;
  try {
    await runtime.turn({ model: "sonnet", resume: gateSeed.session, prompt: prompt(), onInit: () => { beforePrompt = !consumed; release(); },
      onReady: () => { runtime.mark("settings-ready"); readyRelease = setTimeout(() => { runtime.mark("gate-release"); release(); }, 1_000); } });
  } finally { clearTimeout(fallback); if (readyRelease) clearTimeout(readyRelease); }
  barrier = { beforePrompt, requestsAfterAbort, events: runtime.events.slice(start), timeline: runtime.timeline.slice(start) };
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
  assert.notEqual(explicitNondefault.model, runtimeDefault.model, "the control distinguishes retention from vendor default");
  assert.equal(omittedModel.session, explicitNondefault.session);
  assert.equal(omittedModel.model, explicitNondefault.model);
  assert.equal(wire[6]?.model, explicitNondefault.model);
  assert.equal(runtimeDefaultAfter.model, runtimeDefault.model, "the explicit query did not change the fresh-session default");
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
  assert.deepEqual(barrier.events, ["settings-ready", "gate-release", "prompt", "init", "request"]);
  assert.equal(barrier.requestsAfterAbort, 1);
  console.log(`PHASE_CLAUDE_EVIDENCE ${JSON.stringify({ sdk: CLAUDE_PHASE_SDK, cli: a.cli,
    models: [a.model, b.model, back.model, omitted.model], efforts: [a.effort, b.effort, back.effort, omitted.effort],
    runtimeDefaultModel: runtimeDefault.model, explicitNondefaultModel: explicitNondefault.model,
    omittedModel: omittedModel.model, switchedUsageModels: Object.keys(b.usage as object),
    barrierShape: "new CLI resuming an explicit plan session", withheldAfterReadyMs: 1_000, barrier })}`);
});

// PRD #3252 m4 (D8): the pin-sensitivity characterization behind claude-canonical-models.ts. Each alias
// is driven as a RESUMED turn with the prompt withheld, the way the production barrier runs it.
// A non-default explicit effort, so an alias that honours an explicit effort is told apart from one that
// merely applies its own default.
const EXPLICIT_EFFORT = "low" as const;
const CHARACTERIZED_ALIASES = ["opus", "sonnet", "haiku", "fable"] as const;
interface Characterization {
  hasGetSettings: boolean;
  settings: unknown;
  wireModel: unknown;
  wireEffort: unknown;
  arrivalsWhileWithheld: number;
  failure: string | undefined;
}
async function characterize(model: string, effort: "low" | "high" | undefined, resume: string): Promise<Characterization> {
  let release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  async function* prompt(): AsyncGenerator<SDKUserMessage> {
    await gate;
    yield { type: "user", session_id: "", parent_tool_use_id: null, message: { role: "user", content: "Characterize." } };
  }
  const wireBefore = wire.length;
  const arrivalsBefore = arrivals;
  let settings: unknown, arrivalsWhileWithheld = -1;
  const fallback = setTimeout(release, 8_000);
  try {
    const turn = await runtime.turn({ model, ...(effort ? { effort } : {}), resume, prompt: prompt(),
      onReady: applied => { settings = applied; arrivalsWhileWithheld = arrivals - arrivalsBefore; release(); } });
    const first = wire[wireBefore];
    return { hasGetSettings: settings !== undefined, settings, wireModel: first?.model,
      wireEffort: (first?.output_config as { effort?: unknown } | undefined)?.effort,
      arrivalsWhileWithheld, failure: turn.failure };
  } finally { clearTimeout(fallback); }
}
test("Claude phase characterization: canonical ids and omitted-effort defaults match the table", { timeout: 115_000 }, async () => {
  const { CLAUDE_CANONICAL_MODELS, CLAUDE_OMITTED_EFFORT } = await import("../src/claude-canonical-models.js");
  const seed = await runtime.turn({ model: "opus", effort: "high", prompt: "Plan-session seed for the characterization." });
  const observed: Record<string, { model: unknown; effort: unknown; omittedEffort: unknown; wireModel: unknown; wireEffort: unknown }> = {};
  const characterized = new Set<string>();
  for (const alias of CHARACTERIZED_ALIASES) {
    const withEffort = await characterize(alias, EXPLICIT_EFFORT, seed.session);
    const omitted = await characterize(alias, undefined, seed.session);
    const applied = (withEffort.settings as { applied?: { model?: unknown; effort?: unknown } } | undefined)?.applied;
    const appliedOmitted = (omitted.settings as { applied?: { model?: unknown; effort?: unknown } } | undefined)?.applied;
    observed[alias] = { model: applied?.model, effort: applied?.effort, omittedEffort: appliedOmitted?.effort,
      wireModel: withEffort.wireModel, wireEffort: withEffort.wireEffort };
    assert.equal(withEffort.hasGetSettings, true, `${alias}: getSettings answers while the prompt is withheld`);
    assert.equal(withEffort.arrivalsWhileWithheld, 0, `${alias}: no provider request while the prompt is withheld`);
    assert.equal(omitted.arrivalsWhileWithheld, 0, `${alias}: no provider request while the prompt is withheld (omitted effort)`);
    assert.equal(typeof omitted.wireEffort, "string", "the omitted-effort wire request carries an effort");
    assert.equal(omitted.wireEffort, appliedOmitted?.effort,
      `${alias}: the first wire request's effort equals the applied omitted-effort default`);
    if (typeof applied?.model === "string" && withEffort.failure === undefined && applied.model === withEffort.wireModel
        && applied.effort === EXPLICIT_EFFORT && withEffort.wireEffort === EXPLICIT_EFFORT) characterized.add(alias);
  }
  console.log(`PHASE_CLAUDE_CANONICAL ${JSON.stringify(observed)}`);
  assert.deepEqual([...characterized].sort(), Object.keys(CLAUDE_CANONICAL_MODELS).sort(),
    "the table holds exactly the aliases this pin characterizes");
  for (const [alias, id] of Object.entries(CLAUDE_CANONICAL_MODELS)) {
    assert.equal(observed[alias]?.model, id, `${alias}: table id equals the observed applied model`);
  }
  for (const [id, effort] of Object.entries(CLAUDE_OMITTED_EFFORT)) {
    const alias = Object.entries(CLAUDE_CANONICAL_MODELS).find(([, v]) => v === id)?.[0];
    assert.ok(alias, `omitted-effort entry ${id} belongs to a characterized model`);
    assert.equal(observed[alias!]?.omittedEffort, effort, `${id}: omitted-effort entry equals the observed applied effort`);
  }
});
