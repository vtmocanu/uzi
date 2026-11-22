import test from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import path from "node:path";

import { ClaudeHarness, type ClaudeTurnConfig, type SettingsProbeTimers } from "../src/claude-harness.js";
import type { RunTurnRequest } from "../src/harness.js";
import { PhaseSwitchUnverifiedError, type PhaseSwitchCause } from "../src/phase-switch-unverified.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

// PRD #3252 D8 (m4): the pre-prompt settings barrier of the Claude adapter, driven through the real
// ClaudeHarness with a fake query whose "CLI" pulls the prompt eagerly, as the pinned SDK does.

const config: ClaudeTurnConfig = {
  cwd: "/tmp/uzi-pb-test", env: {}, skillsPluginPath: "/tmp/uzi-pb-plugin",
  skills: [], systemPrompt: "sys", agents: {}, mcpServers: {}, preToolUse: [],
};
const result = { type: "result", subtype: "success", result: "ok", is_error: false };
const init = (extra: Record<string, unknown> = {}) => ({ type: "system", subtype: "init", session_id: "s1", ...extra });

function manualTimers() {
  const pending = new Map<number, () => void>();
  let next = 1;
  const timers: SettingsProbeTimers = {
    setTimeout: (fn) => { const id = next++; pending.set(id, fn); return id; },
    clearTimeout: (h) => { pending.delete(h as number); },
  };
  return { timers, fire: () => { for (const fn of pending.values()) fn(); }, pending };
}
const tick = async (n = 10) => { for (let i = 0; i < n; i++) await new Promise((r) => setImmediate(r)); };

interface Rig {
  messages: number;
  settingsCalls: number;
  aborted: () => boolean;
  verified: number;
  drive(): Promise<{ error?: unknown }>;
}

function rig(opts: {
  getSettings?: () => Promise<unknown>;
  frames?: Record<string, unknown>[];
  verify?: RunTurnRequest["verifyApplied"] | null;
  timers?: SettingsProbeTimers;
  signal?: AbortSignal;
}): Rig {
  const r = { messages: 0, settingsCalls: 0, verified: 0 } as Rig;
  let abort: AbortController | undefined;
  const queryFn = ((params: { prompt: AsyncIterable<unknown>; options: { abortController?: AbortController } }) => {
    abort = params.options.abortController;
    const started = (async () => {
      const it = params.prompt[Symbol.asyncIterator]();
      const first = await it.next();
      if (!first.done) r.messages++;
    })();
    const gen = (async function* () {
      await started;
      if (r.messages === 0) return;
      for (const f of opts.frames ?? [init(), result]) yield f;
    })();
    if (opts.getSettings) {
      (gen as unknown as Record<string, unknown>)["getSettings"] = () => { r.settingsCalls++; return opts.getSettings!(); };
    }
    return gen;
  }) as unknown as SdkQueryFn;
  const harness = new ClaudeHarness({
    queryFn, spawn: () => ({ pid: undefined }), kill: () => true, log: nullLogger(),
    contextUsageTimeoutMs: 2000, spawnedPids: new Set<number>(),
    homeDir: path.join(os.tmpdir(), "uzi-pb-home-does-not-exist"),
    settingsProbe: opts.timers ? { timers: opts.timers } : undefined,
  });
  harness.prepareTurn(config, { pid: undefined });
  r.aborted = () => abort?.signal.aborted === true;
  r.drive = async () => {
    const request: RunTurnRequest = {
      prompt: "p", systemPrompt: "sys", signal: opts.signal ?? new AbortController().signal,
      phase: "implement", agents: {}, leadSkills: [],
      ...(opts.verify === null ? {} : { verifyApplied: opts.verify ?? { model: "opus", effort: "high" } }),
      onPhaseVerified: () => { r.verified++; },
    };
    try {
      for await (const _ of harness.startTurn(request).events) { /* drain */ }
      return {};
    } catch (error) {
      return { error };
    }
  };
  return r;
}

const applied = (model: string | undefined, effort: string | undefined) =>
  async () => ({ applied: { ...(model !== undefined ? { model } : {}), ...(effort !== undefined ? { effort } : {}) } });

async function expectHold(r: Rig, cause: PhaseSwitchCause): Promise<PhaseSwitchUnverifiedError> {
  const { error } = await r.drive();
  assert.ok(error instanceof PhaseSwitchUnverifiedError, `expected PhaseSwitchUnverifiedError, got ${String(error)}`);
  assert.equal(error.cause, cause);
  await tick();
  assert.equal(r.messages, 0, "the prompt was never consumed");
  assert.equal(r.aborted(), true, "the query was aborted");
  assert.equal(r.verified, 0);
  return error;
}

test("the prompt is withheld until the verdict, then released exactly once", async () => {
  let answer!: (v: unknown) => void;
  const r = rig({ getSettings: () => new Promise((resolve) => { answer = resolve; }) });
  const done = r.drive();
  await tick();
  assert.equal(r.messages, 0, "no prompt consumed before the verdict");
  assert.equal(r.settingsCalls, 1);
  answer({ applied: { model: "claude-opus-5-5", effort: "high" } });
  assert.deepEqual(await done, {});
  assert.equal(r.messages, 1);
  assert.equal(r.verified, 1);
  assert.equal(r.settingsCalls, 1);
});

test("a model mismatch holds with the observed model and never releases the prompt", async () => {
  const err = await expectHold(rig({ getSettings: applied("claude-sonnet-5-5", "high") }), "model_mismatch");
  assert.deepEqual(err.requested, { model: "opus", effort: "high" });
  assert.deepEqual(err.observed, { model: "claude-sonnet-5-5", effort: "high" });
});

test("a third model is a model mismatch too", async () => {
  const err = await expectHold(rig({ getSettings: applied("claude-haiku-5-5", "high") }), "model_mismatch");
  assert.equal(err.observed.model, "claude-haiku-5-5");
});

test("an effort mismatch holds", async () => {
  const err = await expectHold(rig({ getSettings: applied("claude-opus-5-5", "medium") }), "effort_mismatch");
  assert.equal(err.observed.effort, "medium");
});

test("a runtime without getSettings is method_missing", async () => {
  await expectHold(rig({}), "method_missing");
});

test("a getSettings that never answers times out through the injected timer", async () => {
  const t = manualTimers();
  const r = rig({ getSettings: () => new Promise(() => {}), timers: t.timers });
  const done = r.drive();
  await tick();
  assert.equal(t.pending.size, 1);
  t.fire();
  const { error } = await done;
  assert.ok(error instanceof PhaseSwitchUnverifiedError);
  assert.equal(error.cause, "timeout");
  assert.equal(r.messages, 0);
});

test("malformed answers hold: no applied object, a rejected call, a missing field", async () => {
  await expectHold(rig({ getSettings: async () => ({}) }), "malformed");
  await expectHold(rig({ getSettings: async () => { throw new Error("boom"); } }), "malformed");
  await expectHold(rig({ getSettings: applied("claude-opus-5-5", undefined) }), "malformed");
  await expectHold(rig({ getSettings: applied(undefined, "high") }), "malformed");
});

test("a known alias absent from the characterized table has no expectation", async () => {
  const err = await expectHold(
    rig({ getSettings: applied("claude-opus-5-5", "high"), verify: { model: "opusplan", effort: "high" } }),
    "no_characterized_expectation",
  );
  assert.equal(err.requested.model, "opusplan");
});

test("a full custom id is expected verbatim", async () => {
  const ok = rig({ getSettings: applied("acme-model-1", "high"), verify: { model: "acme-model-1", effort: "high" } });
  assert.deepEqual(await ok.drive(), {});
  assert.equal(ok.messages, 1);
  await expectHold(rig({ getSettings: applied("acme-model-2", "high"), verify: { model: "acme-model-1", effort: "high" } }), "model_mismatch");
});

test("an omitted model is not compared, an omitted effort uses the per-model default", async () => {
  const noModel = rig({ getSettings: applied("anything-the-session-kept", "high"), verify: { effort: "high" } });
  assert.deepEqual(await noModel.drive(), {});
  const omitted = rig({ getSettings: applied("claude-opus-5-5", "medium"), verify: { model: "opus" } });
  assert.deepEqual(await omitted.drive(), {});
  await expectHold(rig({ getSettings: applied("claude-opus-5-5", "high"), verify: { model: "opus" } }), "effort_mismatch");
  await expectHold(rig({ getSettings: applied("acme-model-1", "medium"), verify: { model: "acme-model-1" } }), "no_characterized_expectation");
});

test("an init contradicting the admitted pair after release stops the query", async () => {
  const wrongModel = rig({
    getSettings: applied("claude-opus-5-5", "high"),
    frames: [init({ model: "claude-sonnet-5-5" }), result],
  });
  const { error } = await wrongModel.drive();
  assert.ok(error instanceof PhaseSwitchUnverifiedError);
  assert.equal(error.cause, "model_mismatch");
  assert.equal(wrongModel.aborted(), true);
  const wrongEffort = rig({
    getSettings: applied("claude-opus-5-5", "high"),
    frames: [init({ model: "claude-opus-5-5", effort: "low" }), result],
  });
  const second = await wrongEffort.drive();
  assert.ok(second.error instanceof PhaseSwitchUnverifiedError);
  assert.equal((second.error as PhaseSwitchUnverifiedError).cause, "effort_mismatch");
});

test("missing optional init fields are not a contradiction", async () => {
  const r = rig({ getSettings: applied("claude-opus-5-5", "high"), frames: [init(), result] });
  assert.deepEqual(await r.drive(), {});
  assert.equal(r.aborted(), false);
});

test("without verifyApplied the prompt is delivered at once and getSettings is never consulted", async () => {
  const r = rig({ getSettings: () => new Promise(() => {}), verify: null });
  assert.deepEqual(await r.drive(), {});
  assert.equal(r.messages, 1);
  assert.equal(r.settingsCalls, 0);
  assert.equal(r.verified, 0);
});

test("an owner abort during the barrier ends the turn without a hold or a prompt", async () => {
  const owner = new AbortController();
  const r = rig({ getSettings: () => new Promise(() => {}), signal: owner.signal });
  const done = r.drive();
  await tick();
  owner.abort();
  const { error } = await done;
  assert.equal(error instanceof PhaseSwitchUnverifiedError, false);
  assert.equal(r.messages, 0);
});
