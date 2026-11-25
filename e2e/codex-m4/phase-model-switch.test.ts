import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { pathToFileURL } from "node:url";
import { FakeProvider, dummyCredential } from "./fake-provider.js";
import { message } from "../codex-m0/harness.mjs";
import { loadPackagedConfig, loadPackagedSessionState } from "./packaged-modules.js";
import type { CodexUsageBreakdown } from "../../agent/src/codex/transport.js";
import type { CodexModelUsageEntry } from "../../agent/src/codex/token-accounting.js";
import { assertBinaryVersion, resolveCodexBin } from "./provision.js";
import { PhaseCodex, phaseSettings } from "./phase-model-driver.js";
import { P_LAYER_SKIP } from "./p-platform.js";
import { PHASE_MODEL_TITLES } from "./phase-model-titles.js";
import { recordEvidence } from "./evidence.js";

const A = "gpt-6-astra", B = "gpt-6.1-sol", nonce = `phase-${randomUUID()}`;
let root: string, fake: FakeProvider, warm: PhaseCodex, cold: PhaseCodex, adopted: PhaseCodex;

// Small complete vectors: raw input, cache read, cache write, output, reasoning, total, uncached.
function usageVector(b: CodexUsageBreakdown): number[] {
  return [b.inputTokens, b.cachedInputTokens, b.cacheWriteInputTokens, b.outputTokens,
    b.reasoningOutputTokens, b.totalTokens, b.inputTokens - b.cachedInputTokens - b.cacheWriteInputTokens];
}
function modelVector(b: CodexModelUsageEntry): number[] {
  const raw = b.inputTokens + b.cacheReadInputTokens + b.cacheCreationInputTokens;
  return [raw, b.cacheReadInputTokens, b.cacheCreationInputTokens, b.outputTokens,
    b.reasoningOutputTokens, raw + b.outputTokens, b.inputTokens];
}
function lastUsage(turn: Awaited<ReturnType<PhaseCodex["turn"]>>, threadId: string): CodexUsageBreakdown {
  const notes = turn.notes.filter(n => n.kind === "token_usage_updated" && n.threadId === threadId && n.turnId === turn.turnId);
  const note = notes.at(-1);
  assert.ok(note?.kind === "token_usage_updated", "the matching turn reports cumulative usage");
  return note.usage.total;
}
const observed: Record<string, unknown> = {};
before(async () => {
  if (P_LAYER_SKIP) return;
  const config = await loadPackagedConfig();
  const bin = resolveCodexBin().codexBin;
  assertBinaryVersion(bin, "0.171.0");
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
  await warm.turn(s.thread.id, A, `Remember ${nonce}.`, "high");
  const resumed = await warm.thread(B, s.thread.id);
  const countAtResume = fake.requests.length;
  const switched = await warm.turn(s.thread.id, B, "Repeat the nonce.", "medium");
  assert.equal(switched.text, nonce, "the provider response control reaches the caller");
  const switchWire = fake.requests.at(-1)!;
  observed.switch = { resumed, wire: switchWire, priorRequests: countAtResume };
  // Warm B/A requests characterize runtime behavior only; they are not planning billing.
  await warm.thread(A, s.thread.id);
  await warm.turn(s.thread.id, A, "Repeat it again.", "high");
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
  // A separate pure planning history supplies both the billing replay and the adoption snapshot.
  const billing = await warm.thread(A);
  const billingId = billing.thread.id;
  const billingNonce = `${nonce}-billing`;
  const accountant = new accounting.CodexUsageAccountant();
  accountant.registerThread(billingId, A, false);
  const assertPlanningSettings = (turn: Awaited<ReturnType<PhaseCodex["turn"]>>, requireObservation = true) => {
    const wire = fake.requests.at(-1)!;
    assert.equal(wire.model, A);
    assert.equal((wire.reasoning as { effort: string }).effort, "high");
    const settings = phaseSettings(turn.notes, billingId, turn.turnId).at(-1);
    // An unchanged second turn need not emit a settings update; every provider request
    // still proves the model/effort pair, and the first turn establishes runtime evidence.
    if (!requireObservation && settings === undefined) return;
    assert.ok(settings);
    assert.equal(settings.model, A);
    assert.equal(settings.effortPresent, true);
    assert.equal(settings.effort, "high");
  };
  const firstPlan = await warm.turn(billingId, A, `Remember ${billingNonce}.`, "high");
  assertPlanningSettings(firstPlan);
  for (const note of firstPlan.notes) {
    if (note.kind === "token_usage_updated" && note.threadId === billingId && note.turnId === firstPlan.turnId)
      accountant.record(billingId, note.usage);
  }
  const firstTotal = usageVector(lastUsage(firstPlan, billingId));
  assert.deepEqual(modelVector(accountant.aggregateByModel()![A]!), firstTotal, "the accountant observes only the first planning turn");
  const secondPlan = await warm.turn(billingId, A, "Repeat the billing nonce.", "high");
  assertPlanningSettings(secondPlan, false);
  const fullPlan = usageVector(lastUsage(secondPlan, billingId));
  // Deliberately drop the second planning usage: cold historical replay must recover it for A.
  assert.ok(fullPlan[5]! > firstTotal[5]!);
  await warm.close();
  const { CodexSessionStore } = await loadPackagedSessionState();
  const snapshot = path.join(root, "snapshot");
  const persisted = await CodexSessionStore.persist(home, snapshot);
  assert.ok(persisted.files > 0, "production persist captured actual warm session files");
  assert.equal(await CodexSessionStore.inspectSession(snapshot, billingId), "present");
  cold = await PhaseCodex.open(bin, home, cwd, credential, config.CODEX_M3B_LOOPBACK_PROVIDER_NAME);
  const count = fake.requests.length;
  const reclaimed = await cold.thread(B, s.thread.id);
  assert.equal(fake.requests.length, count, "resume itself sends no Responses request");
  const next = await cold.turn(s.thread.id, undefined, "Repeat the nonce.", "medium");
  observed.cold = { reclaimed, wire: fake.requests.at(-1), before: count, after: fake.requests.length };
  assert.equal(next.text, nonce);
  for (const group of groups) {
    const reply = await cold.thread(B, group.id);
    assert.equal(reply.thread.id, group.id);
    const result = await cold.turn(group.id, B, "Repeat it.", group.omitted);
    effortReports.push(phaseSettings(result.notes, group.id, result.turnId).at(-1));
    assert.match(JSON.stringify(fake.requests.at(-1)!.input), new RegExp(group.nonce), "cold effort reset retains its own group's history");
    effortWire.push(fake.requests.at(-1)!.reasoning);
  }
  const beforeBillingResume = fake.requests.length;
  const billingResume = await cold.thread(B, billingId);
  assert.equal(billingResume.thread.id, billingId);
  assert.equal(billingResume.model, B);
  assert.equal(fake.requests.length, beforeBillingResume, "billing resume sends no provider request");
  const implementation = await cold.turn(billingId, B, "Repeat the billing nonce.", "medium");
  assert.equal(implementation.text, nonce);
  const billingWire = fake.requests.at(-1)!;
  assert.equal(billingWire.model, B);
  assert.equal((billingWire.reasoning as { effort: string }).effort, "medium");
  assert.match(JSON.stringify(billingWire.input), new RegExp(billingNonce));
  // Characterization feeder only; production two-harness boundary coverage lives in agent tests.
  // The driver bounds notes to 512; this synchronous fold cuts once at the matching root boundary.
  let cuts = 0;
  let replayTotal: number[] | undefined;
  const liveModels: string[] = [];
  for (const note of implementation.notes) {
    if (note.kind === "turn_started" && note.threadId === billingId && note.turnId === implementation.turnId) {
      assert.equal(cuts, 0);
      assert.ok(replayTotal, "historical usage must precede the matching root turn boundary");
      assert.deepEqual(replayTotal, fullPlan, "all historical planning usage is recovered before cutting");
      assert.deepEqual(modelVector(accountant.aggregateByModel()![A]!), fullPlan);
      assert.equal(accountant.aggregateByModel()![B], undefined, "no historical A usage is charged to B");
      accountant.beginModelSegment(billingId, B);
      cuts++;
    }
    if (note.kind !== "token_usage_updated" || note.threadId !== billingId) continue;
    const replay = cuts === 0;
    if (replay) {
      assert.notEqual(note.turnId, implementation.turnId, "replay belongs to a historical turn");
      replayTotal = usageVector(note.usage.total);
    } else assert.equal(note.turnId, implementation.turnId, "live usage belongs to the implementation turn");
    const measurement = accountant.record(billingId, note.usage, replay);
    if (measurement) liveModels.push(measurement.model);
  }
  assert.equal(cuts, 1);
  assert.ok(replayTotal);
  assert.ok(replayTotal[5]! > firstTotal[5]!, "replay advances beyond the last observed plan cumulative");
  assert.ok(liveModels.length > 0);
  assert.ok(liveModels.every(model => model === B));
  const liveTotal = usageVector(lastUsage(implementation, billingId));
  assert.ok(liveTotal[5]! > fullPlan[5]!, "implementation adds live usage after the replay boundary");
  const entries = accountant.aggregateByModel()!;
  assert.deepEqual(Object.keys(entries).sort(), [A, B].sort());
  const a = modelVector(entries[A]!), b = modelVector(entries[B]!);
  assert.deepEqual(a, fullPlan, "A retains the full replay cumulative");
  assert.deepEqual(b, liveTotal.map((value, i) => value - fullPlan[i]!), "B receives only the post-boundary delta");
  assert.deepEqual(a.map((value, i) => value + b[i]!), liveTotal, "every well-formed column conserves usage");
  observed.coldSegmentedAccounting = { firstTotal, replayTotal, liveTotal, a, b, liveModels, cuts };
  await cold.close();

  const freshHome = path.join(root, "adopted-home");
  fs.mkdirSync(freshHome);
  const adoption = await CodexSessionStore.adopt(snapshot, freshHome);
  assert.ok(adoption.files > 0, "production adopt copied the saved planning session");
  assert.equal(fs.existsSync(path.join(freshHome, "auth.json")), false, "adoption copies no authentication before setup");
  fs.writeFileSync(path.join(freshHome, "config.toml"), config.buildCodexLoopbackTestConfigToml({ model: A, projectPath: cwd, authMode: "api_key" }, fake.baseUrl));
  adopted = await PhaseCodex.open(bin, freshHome, cwd, credential, config.CODEX_M3B_LOOPBACK_PROVIDER_NAME);
  const beforeAdoptedResume = fake.requests.length;
  const adoptedResume = await adopted.thread(B, billingId);
  assert.equal(adoptedResume.thread.id, billingId);
  assert.equal(adoptedResume.model, B);
  const resumeRequests = fake.requests.length - beforeAdoptedResume;
  assert.equal(resumeRequests, 0, "fresh-HOME resume performs no provider request");
  const adoptedTurn = await adopted.turn(billingId, undefined, "Repeat the billing nonce without overrides.", undefined);
  const adoptedWire = fake.requests.at(-1)!;
  assert.equal(fake.requests.length, beforeAdoptedResume + 1);
  assert.equal(adoptedWire.model, B);
  assert.match(JSON.stringify(adoptedWire.input), new RegExp(billingNonce));
  const adoptedSettings = phaseSettings(adoptedTurn.notes, billingId, adoptedTurn.turnId).at(-1);
  assert.ok(adoptedSettings);
  assert.equal(adoptedSettings.model, B);
  assert.equal(adoptedSettings.effortPresent, true);
  assert.equal(adoptedSettings.effort, null, "fresh-HOME cold resume clears historical high effort");
  observed.adoption = { threadId: adoptedResume.thread.id, savedThreadId: billingId, model: adoptedResume.model,
    wireModel: adoptedWire.model, settings: adoptedSettings, persistedFiles: persisted.files, adoptedFiles: adoption.files,
    resumeRequests, turnRequests: fake.requests.length - beforeAdoptedResume };
  // The forked snapshot is a separate runtime control and is never fed to the billing accountant.
  observed.efforts = effortWire;
  observed.effortReports = effortReports;
  observed.requests = fake.requests.length;
  assert.deepEqual(fake.errors, []);
}, { timeout: 110_000 });
after(async () => { await adopted?.close(); await cold?.close(); await warm?.close(); await fake?.close(); if (root) fs.rmSync(root, { recursive: true, force: true }); });

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
test("Codex phase credential-free adoption: fresh HOME resumes the saved planning thread without turn overrides", { skip: P_LAYER_SKIP }, () => {
  const result = observed.adoption as { threadId: string; savedThreadId: string; model: string; wireModel: string;
    settings: { effortPresent: boolean; effort: string | null }; persistedFiles: number; adoptedFiles: number;
    resumeRequests: number; turnRequests: number };
  assert.ok(result);
  assert.equal(result.threadId, result.savedThreadId);
  assert.equal(result.model, B);
  assert.equal(result.wireModel, B);
  assert.ok(result.persistedFiles > 0);
  assert.ok(result.adoptedFiles > 0);
  assert.equal(result.resumeRequests, 0);
  assert.equal(result.turnRequests, 1);
  assert.equal(result.settings.effortPresent, true);
  assert.equal(result.settings.effort, null);
});
test("Codex phase case 6: cold segmented accounting recovers larger historical replay before implementation", { skip: P_LAYER_SKIP }, () => {
  assert.ok(observed.coldSegmentedAccounting);
  const warmResult = observed.switch as { resumed: { model: string }; wire: { model?: unknown } };
  const coldResult = observed.cold as { reclaimed: { model: string }; wire: { model?: unknown } };
  console.log(`PHASE_CODEX_EVIDENCE ${JSON.stringify({ pin: "0.171.0", efforts: observed.efforts, effortReports: observed.effortReports, requests: observed.requests,
    warmResumeModel: warmResult.resumed.model, warmRequestModel: warmResult.wire.model,
    coldResumeModel: coldResult.reclaimed.model, coldRequestModelWithoutTurnOverride: coldResult.wire.model, warmControl: observed.warmControl,
    coldSegmentedAccounting: observed.coldSegmentedAccounting, credentialFreeAdoption: observed.adoption })}`);
});
