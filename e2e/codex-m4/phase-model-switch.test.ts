import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { pathToFileURL } from "node:url";
import { FakeProvider, dummyCredential } from "./fake-provider.js";
import { message } from "../codex-m0/harness.mjs";
import { loadPackagedConfig } from "./packaged-modules.js";
import { assertBinaryVersion, resolveCodexBin } from "./provision.js";
import { PhaseCodex, phaseSettings } from "./phase-model-driver.js";
import { P_LAYER_SKIP } from "./p-platform.js";
import { PHASE_MODEL_TITLES } from "./phase-model-titles.js";
import { recordEvidence } from "./evidence.js";

const A = "gpt-6-astra", B = "gpt-6.1-sol", nonce = `phase-${randomUUID()}`;
let root: string, fake: FakeProvider, warm: PhaseCodex, cold: PhaseCodex;
const observed: Record<string, unknown> = {};
before(async () => {
  if (P_LAYER_SKIP) return;
  const config = await loadPackagedConfig();
  const bin = resolveCodexBin().codexBin;
  assertBinaryVersion(bin, "0.169.0");
  fs.mkdirSync("../.uzi/scratch", { recursive: true });
  root = fs.mkdtempSync(path.resolve("../.uzi/scratch/phase-model-"));
  const home = path.join(root, "home"), cwd = path.join(root, "cwd");
  fs.mkdirSync(home); fs.mkdirSync(cwd);
  const credential = dummyCredential();
  fake = await FakeProvider.start({ credential, respond: () => [message(nonce) as Record<string, unknown>],
    usage: i => ({ input_tokens: i * 100, output_tokens: i * 10, total_tokens: i * 110,
      input_tokens_details: { cached_tokens: i * 20 }, output_tokens_details: { reasoning_tokens: i * 5 } }) });
  fs.writeFileSync(path.join(home, "config.toml"), config.buildCodexLoopbackTestConfigToml({ model: A, projectPath: cwd, authMode: "api_key" }, fake.baseUrl));
  warm = await PhaseCodex.open(bin, home, cwd, credential, config.CODEX_M3B_LOOPBACK_PROVIDER_NAME);
  const s = await warm.thread(A);
  observed.thread = s.thread.id;
  const accounting = await import(pathToFileURL(path.resolve(process.env.CODEX_M4_SRC ?? "src", "codex/token-accounting.ts")).href) as typeof import("../../agent/src/codex/token-accounting.js");
  const accountant = new accounting.CodexUsageAccountant();
  accountant.registerThread(s.thread.id, A, false);
  const feed = (turn: Awaited<ReturnType<PhaseCodex["turn"]>>, model: string) => {
    let replay = true;
    const models: string[] = [];
    accountant.registerThread(s.thread.id, model, true);
    for (const note of turn.notes) {
      if (note.kind === "turn_started") replay = false;
      if (note.kind === "token_usage_updated" && note.threadId === s.thread.id) {
        const adopted = accountant.record(s.thread.id, note.usage, replay);
        if (adopted) models.push(adopted.model);
      }
    }
    return models;
  };
  feed(await warm.turn(s.thread.id, A, `Remember ${nonce}.`, "high"), A);
  const resumed = await warm.thread(B, s.thread.id);
  const countAtResume = fake.requests.length;
  const switched = await warm.turn(s.thread.id, B, "Repeat the nonce.", "medium");
  assert.equal(switched.text, nonce, "the provider response control reaches the caller");
  const switchWire = fake.requests.at(-1)!;
  observed.switch = { resumed, wire: switchWire, priorRequests: countAtResume };
  observed.accounted = feed(switched, B);
  await warm.thread(A, s.thread.id);
  feed(await warm.turn(s.thread.id, A, "Repeat it again.", "high"), A);
  observed.replan = fake.requests.at(-1)!.model;
  // Separate control: a model supplied again on turn/start would mask an ignored resume override.
  const control = await warm.thread(A);
  await warm.turn(control.thread.id, A, "Warm resume effectiveness control.", "high");
  const controlReport = await warm.thread(B, control.thread.id);
  await warm.turn(control.thread.id, undefined, "Use the resumed model without another model override.", "medium");
  observed.warmControl = { report: controlReport.model, wire: fake.requests.at(-1)!.model };
  const groups: Array<{ id: string; omitted: undefined | null; nonce: string }> = [];
  const effortWire: unknown[] = [];
  const effortReports: unknown[] = [];
  for (const omitted of [undefined, null]) {
    const groupNonce = `${nonce}-${omitted === null ? "null" : "omitted"}`;
    const thread = await warm.thread(A);
    await warm.turn(thread.thread.id, A, groupNonce, "high");
    await warm.thread(B, thread.thread.id);
    const result = await warm.turn(thread.thread.id, B, "Repeat it.", omitted);
    effortReports.push(phaseSettings(result.notes, thread.thread.id, result.turnId).at(-1));
    effortWire.push(fake.requests.at(-1)!.reasoning);
    await warm.turn(thread.thread.id, A, "Repeat it.", "high");
    groups.push({ id: thread.thread.id, omitted, nonce: groupNonce });
  }
  await warm.close();
  cold = await PhaseCodex.open(bin, home, cwd, credential, config.CODEX_M3B_LOOPBACK_PROVIDER_NAME);
  const count = fake.requests.length;
  const reclaimed = await cold.thread(B, s.thread.id);
  assert.equal(fake.requests.length, count, "resume itself sends no Responses request");
  const next = await cold.turn(s.thread.id, undefined, "Repeat the nonce.", "medium");
  observed.cold = { reclaimed, wire: fake.requests.at(-1), before: count, after: fake.requests.length };
  observed.accountedCold = feed(next, B);
  for (const group of groups) {
    const reply = await cold.thread(B, group.id);
    assert.equal(reply.thread.id, group.id);
    const result = await cold.turn(group.id, B, "Repeat it.", group.omitted);
    effortReports.push(phaseSettings(result.notes, group.id, result.turnId).at(-1));
    assert.match(JSON.stringify(fake.requests.at(-1)!.input), new RegExp(group.nonce), "cold effort reset retains its own group's history");
    effortWire.push(fake.requests.at(-1)!.reasoning);
  }
  observed.efforts = effortWire;
  observed.effortReports = effortReports;
  observed.requests = fake.requests.length;
  assert.deepEqual(fake.errors, []);
}, { timeout: 110_000 });
after(async () => { await cold?.close(); await warm?.close(); await fake?.close(); if (root) fs.rmSync(root, { recursive: true, force: true }); });

for (const [key, title] of Object.entries(PHASE_MODEL_TITLES)) test(title, { skip: P_LAYER_SKIP }, async () => {
  recordEvidence(title, "fail");
  const switched = observed.switch as { resumed: { model: string }; wire: Record<string, unknown>; priorRequests: number };
  const coldResult = observed.cold as { reclaimed: { model: string; thread: { id: string } }; wire: Record<string, unknown>; before: number; after: number };
  if (key === "switch") {
    assert.equal(switched.resumed.model, A, "warm resume reports the cached model, not the new turn model");
    assert.equal(switched.wire.model, B);
    assert.equal((switched.wire.reasoning as { effort: string }).effort, "medium");
    assert.match(JSON.stringify(switched.wire.input), new RegExp(nonce));
  } else if (key === "reclaim") {
    assert.equal(coldResult.reclaimed.thread.id, observed.thread);
    assert.equal(coldResult.reclaimed.model, B); assert.equal(coldResult.wire.model, B);
    assert.match(JSON.stringify(coldResult.wire.input), new RegExp(nonce));
  } else if (key === "replan") assert.equal(observed.replan, A);
  else if (key === "modelOmitted") {
    const { resolveLeadRequest } = await import("../../agent/src/lead-config-report.js");
    assert.equal(resolveLeadRequest({ harness: "codex", phase: "implement", config: undefined, providerDefaultModel: A }).model, A);
  } else if (key === "effortOmitted") {
    assert.deepEqual((observed.efforts as Array<{ effort: string }>).map(r => r.effort), ["high", "high", "low", "low"]);
    assert.deepEqual((observed.effortReports as Array<{ model: string; effortPresent: boolean; effort: string | null }>).map(r => ({ model: r.model, effortPresent: r.effortPresent, effort: r.effort })),
      ["high", "high", null, null].map(effort => ({ model: B, effortPresent: true, effort })));
  }
  else {
    assert.equal(switched.priorRequests, 1);
    assert.equal(coldResult.after, coldResult.before + 1);
    assert.deepEqual(observed.warmControl, { report: A, wire: A });
  }
  recordEvidence(title, "pass");
});
test("Codex phase case 6: today's shared accountant attributes switched usage to model A (Q-codex must invert)", { skip: P_LAYER_SKIP }, () => {
  assert.deepEqual(observed.accounted, [A]);
  assert.deepEqual(observed.accountedCold, [A]);
  const warmResult = observed.switch as { resumed: { model: string }; wire: { model?: unknown } };
  const coldResult = observed.cold as { reclaimed: { model: string }; wire: { model?: unknown } };
  console.log(`PHASE_CODEX_EVIDENCE ${JSON.stringify({ pin: "0.169.0", efforts: observed.efforts, effortReports: observed.effortReports, requests: observed.requests,
    warmResumeModel: warmResult.resumed.model, warmRequestModel: warmResult.wire.model,
    coldResumeModel: coldResult.reclaimed.model, coldRequestModelWithoutTurnOverride: coldResult.wire.model, warmControl: observed.warmControl,
    switchedModelAccounting: observed.accounted })}`);
});
