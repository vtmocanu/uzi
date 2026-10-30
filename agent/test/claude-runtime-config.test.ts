import test from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import path from "node:path";

import { ClaudeHarness, type ClaudeTurnConfig, type SettingsProbeTimers } from "../src/claude-harness.js";
import type { RunTurnRequest, RuntimeConfigObservation } from "../src/harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

// PRD #3244 M2: the Claude init decode and the single getSettings probe, driven through the real
// ClaudeHarness with a fake query function.

const config: ClaudeTurnConfig = {
  cwd: "/tmp/uzi-rc-test", env: {}, skillsPluginPath: "/tmp/uzi-rc-plugin",
  skills: [], systemPrompt: "sys", agents: {}, mcpServers: {}, preToolUse: [],
};

const result = { type: "result", subtype: "success", result: "ok", is_error: false };
const init = (extra: Record<string, unknown> = {}) => ({
  type: "system", subtype: "init", session_id: "s1", model: "claude-opus-4-8", ...extra,
});

/** A fake timer set whose scheduled callbacks fire only when the test calls `fire`. */
function manualTimers() {
  const pending = new Map<number, () => void>();
  let next = 1;
  const timers: SettingsProbeTimers = {
    setTimeout: (fn) => { const id = next++; pending.set(id, fn); return id; },
    clearTimeout: (h) => { pending.delete(h as number); },
  };
  return { timers, fire: () => { for (const fn of pending.values()) fn(); }, pending };
}

async function run(opts: {
  frames: Record<string, unknown>[];
  getSettings?: () => Promise<unknown>;
  timers?: SettingsProbeTimers;
  noCallback?: boolean;
  throwInCallback?: boolean;
  afterFrames?: () => Promise<void>;
}): Promise<{ obs: RuntimeConfigObservation[]; settingsCalls: number }> {
  let settingsCalls = 0;
  const queryFn = (() => {
    const gen = (async function* () {
      for (const f of opts.frames) yield f;
      await opts.afterFrames?.();
    })();
    if (opts.getSettings) {
      (gen as unknown as Record<string, unknown>)["getSettings"] = () => { settingsCalls++; return opts.getSettings!(); };
    }
    return gen;
  }) as unknown as SdkQueryFn;
  const harness = new ClaudeHarness({
    queryFn, spawn: () => ({ pid: undefined }), kill: () => true, log: nullLogger(),
    contextUsageTimeoutMs: 2000, spawnedPids: new Set<number>(),
    homeDir: path.join(os.tmpdir(), "uzi-rc-home-does-not-exist"),
    settingsProbe: opts.timers ? { timers: opts.timers } : undefined,
  });
  harness.prepareTurn(config, { pid: undefined });
  const obs: RuntimeConfigObservation[] = [];
  const request: RunTurnRequest = {
    prompt: "p", systemPrompt: "sys", signal: new AbortController().signal,
    phase: "implement", agents: {}, leadSkills: [],
    ...(opts.noCallback ? {} : {
      onRuntimeConfig: (o) => { obs.push(o); if (opts.throwInCallback) throw new Error("boom"); },
    }),
  };
  for await (const _ of harness.startTurn(request).events) { /* drain */ }
  // The probe is detached from the event stream: let its continuation settle.
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
  return { obs, settingsCalls };
}

test("init with a known effort reports it as claude_init without probing", async () => {
  const { obs, settingsCalls } = await run({
    frames: [init({ effort: "high" }), result],
    getSettings: async () => { throw new Error("must not be called"); },
  });
  assert.deepEqual(obs, [{ model: "claude-opus-4-8", modelEvidence: "claude_init", effort: "high", effortEvidence: "claude_init" }]);
  assert.equal(settingsCalls, 0);
});

test("init with effort null reports not sent, with real evidence", async () => {
  const { obs } = await run({ frames: [init({ effort: null }), result] });
  assert.deepEqual(obs, [{ model: "claude-opus-4-8", modelEvidence: "claude_init", effort: null, effortEvidence: "claude_init" }]);
});

test("init with a custom effort string reports other; a non-string shape reports unknown", async () => {
  const custom = await run({ frames: [init({ effort: "turbo" }), result] });
  assert.equal(custom.obs.length, 1);
  assert.equal(custom.obs[0]!.effort, "other");
  assert.equal(custom.obs[0]!.effortEvidence, "claude_init");
  for (const effort of [7, {}, ""]) {
    const { obs } = await run({ frames: [init({ effort }), result] });
    assert.equal(obs.length, 1);
    assert.equal(obs[0]!.effort, undefined);
    assert.equal(obs[0]!.effortEvidence, "unknown");
  }
});

test("getSettings reporting a custom effort string maps to other", async () => {
  const { obs } = await run({ frames: [init(), result], getSettings: async () => ({ applied: { effort: "turbo" } }) });
  assert.equal(obs[0]!.effort, "other");
  assert.equal(obs[0]!.effortEvidence, "claude_get_settings");
});

test("absent effort key: getSettings answering supplies applied.effort as claude_get_settings", async () => {
  const { obs, settingsCalls } = await run({
    frames: [init(), result],
    getSettings: async () => ({ effective: { effortLevel: "low" }, applied: { effort: "xhigh", model: "x" } }),
  });
  assert.equal(settingsCalls, 1);
  assert.deepEqual(obs, [{ model: "claude-opus-4-8", modelEvidence: "claude_init", effort: "xhigh", effortEvidence: "claude_get_settings" }]);
});

test("getSettings reporting applied.effort null means not sent", async () => {
  const { obs } = await run({ frames: [init(), result], getSettings: async () => ({ applied: { effort: null } }) });
  assert.equal(obs[0]!.effort, null);
  assert.equal(obs[0]!.effortEvidence, "claude_get_settings");
});

test("absent effort key and no getSettings method gives unknown effort (the pinned SDK case)", async () => {
  const { obs } = await run({ frames: [init(), result] });
  assert.deepEqual(obs, [{ model: "claude-opus-4-8", modelEvidence: "claude_init", effortEvidence: "unknown" }]);
});

test("a throwing getSettings gives unknown effort", async () => {
  const { obs, settingsCalls } = await run({ frames: [init(), result], getSettings: async () => { throw new Error("nope"); } });
  assert.equal(settingsCalls, 1);
  assert.deepEqual(obs, [{ model: "claude-opus-4-8", modelEvidence: "claude_init", effortEvidence: "unknown" }]);
});

test("a getSettings that never answers times out through the injected timer", async () => {
  const t = manualTimers();
  const hang = new Promise<never>(() => {});
  const running = run({ frames: [init(), result], getSettings: () => hang, timers: t.timers });
  for (let i = 0; i < 20 && t.pending.size === 0; i++) await new Promise((r) => setImmediate(r));
  assert.equal(t.pending.size, 1, "the probe armed one timer");
  t.fire();
  const { obs } = await running;
  assert.deepEqual(obs, [{ model: "claude-opus-4-8", modelEvidence: "claude_init", effortEvidence: "unknown" }]);
});

test("a malformed getSettings response gives unknown effort", async () => {
  for (const response of [undefined, null, "str", {}, { applied: null }, { applied: {} }, { applied: { effort: 3 } }, { effective: { effort: "high" } }]) {
    const { obs } = await run({ frames: [init(), result], getSettings: async () => response });
    assert.equal(obs.length, 1, JSON.stringify(response));
    assert.equal(obs[0]!.effort, undefined, JSON.stringify(response));
    assert.equal(obs[0]!.effortEvidence, "unknown", JSON.stringify(response));
  }
});

test("the probe never blocks the event stream: events finish while getSettings is still pending", async () => {
  let answer: (v: unknown) => void = () => {};
  const pending = new Promise((r) => { answer = r; });
  const t = manualTimers();
  const events: string[] = [];
  const queryFn = (() => {
    const gen = (async function* () { yield init(); yield result; })();
    (gen as unknown as Record<string, unknown>)["getSettings"] = () => pending;
    return gen;
  }) as unknown as SdkQueryFn;
  const harness = new ClaudeHarness({
    queryFn, spawn: () => ({ pid: undefined }), kill: () => true, log: nullLogger(),
    contextUsageTimeoutMs: 2000, spawnedPids: new Set<number>(),
    homeDir: path.join(os.tmpdir(), "uzi-rc-home-does-not-exist"), settingsProbe: { timers: t.timers },
  });
  harness.prepareTurn(config, { pid: undefined });
  const obs: RuntimeConfigObservation[] = [];
  for await (const e of harness.startTurn({
    prompt: "p", systemPrompt: "s", signal: new AbortController().signal, phase: "plan", agents: {}, leadSkills: [],
    onRuntimeConfig: (o) => obs.push(o),
  }).events) events.push(e.kind);
  assert.ok(events.includes("turn_finished"), "the stream ended");
  assert.equal(obs.length, 0, "no observation yet");
  answer({ applied: { effort: "max" } });
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
  assert.equal(obs[0]!.effort, "max");
});

test("a model failing the stored-model rules is reported unknown", async () => {
  for (const model of ["bad model", "claude\uD800x", "claude\uDC00", "claude\uFFFDx"]) {
    const { obs } = await run({ frames: [init({ model, effort: "low" }), result] });
    assert.equal(obs[0]!.model, undefined, JSON.stringify(model));
    assert.equal(obs[0]!.modelEvidence, "unknown");
    assert.equal(obs[0]!.effort, "low");
  }
});

test("at most once per turn, and a subagent init is ignored", async () => {
  const { obs } = await run({
    frames: [init({ parent_tool_use_id: "toolu_x", model: "child-model", effort: "low" }), init({ effort: "high" }), init({ effort: "max" }), result],
  });
  assert.equal(obs.length, 1);
  assert.equal(obs[0]!.effort, "high");
});

test("no callback means no probe", async () => {
  const { obs, settingsCalls } = await run({
    frames: [init(), result], noCallback: true, getSettings: async () => ({ applied: { effort: "low" } }),
  });
  assert.equal(obs.length, 0);
  assert.equal(settingsCalls, 0);
});

test("a throwing callback never reaches the turn", async () => {
  const withEffort = await run({ frames: [init({ effort: "high" }), result], throwInCallback: true });
  assert.equal(withEffort.obs.length, 1);
  const probed = await run({ frames: [init(), result], throwInCallback: true, getSettings: async () => ({ applied: { effort: "low" } }) });
  assert.equal(probed.obs.length, 1);
});
