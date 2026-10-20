import assert from "node:assert/strict";
import { test } from "node:test";
import { performance } from "node:perf_hooks";
import type { Executor } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// Issue #2471 M3: a guard-bound runner enrols Claude claims (no Codex secret) too.

const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };

async function setup() {
  api.memoryRegisterNonce = "claude-memory-registration";
  api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence"]);
  await client.register("memory-claude-fixture", "base");
  // A Claude claim: the Codex secret is absent, the memory fields are the guard's.
  const claim: ClaimResponse = { ...gitlabClaim(2471), claim_generation: 7,
    memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: 0 };
  api.setOwnershipStatus(claim.run_id, "running", 7);
  const memory = new WorkerMemoryRuntime(settings, client, nullLogger(), {
    reader: { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: 1, startedAtMs: time, completedAtMs: time,
        currentBytes: 0, limitBytes: 1000,
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
          full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
  return { memory, claim };
}

test("a Claude claim on a guard-bound runner is enrolled; a missing memory field is refused before the executor", async () => {
  const f = await setup();
  let calls = 0;
  let accessor: unknown, secrets: unknown, subscribe: unknown;
  const executor: Executor = { run: async ctx => {
    calls++;
    accessor = ctx.capturedMemory;
    secrets = ctx.guardedCommandSecrets;
    subscribe = ctx.onMemoryAuthorityLost;
    throw new Error("fixture end");
  } };
  const r = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  try {
    assert.equal(f.claim.secrets.codex, undefined, "the fixture is a Claude claim");
    // Red: the pre-change gate returned early for a Claude claim and ran it unenrolled; an
    // incomplete memory field must now stop it before executor admission.
    for (const bad of [{ memory_policy: undefined }, { memory_policy: { version: 1 as const, max_interventions: 0 } },
      { memory_episode: undefined }]) {
      await r.execute({ ...f.claim, ...bad });
      assert.equal(calls, 0, "an incomplete Claude claim must stop before executor admission");
    }
    // Green: the complete claim reaches the executor with the guard's seams.
    await r.execute(f.claim);
    assert.equal(calls, 1);
    assert.equal(typeof accessor, "function");
    assert.equal(typeof subscribe, "function");
    assert.ok(Array.isArray(secrets) && secrets.includes(f.claim.secrets.forge_pat), "the run's redaction secrets ride the ctx");
  } finally { await f.memory.stop(); }
});

test("a runner without a memory runtime leaves a Claude claim unenrolled", async () => {
  const f = await setup();
  let accessor: unknown = "unset", subscribe: unknown = "unset", secrets: unknown = "unset";
  const executor: Executor = { run: async ctx => {
    accessor = ctx.capturedMemory;
    subscribe = ctx.onMemoryAuthorityLost;
    secrets = ctx.guardedCommandSecrets;
    throw new Error("fixture end");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab).execute(f.claim);
    assert.equal(accessor, undefined);
    assert.equal(subscribe, undefined);
    assert.equal(secrets, undefined);
  } finally { await f.memory.stop(); }
});

test("only the memory invalidation tells the executor; cancellation retires memory without telling it", async () => {
  const f = await setup();
  const monitoring = f.memory.run();
  const told: Error[] = [];
  let afterRetire = -1;
  let afterUnsubscribe = -1;
  const executor: Executor = { run: async ctx => {
    const off = ctx.onMemoryAuthorityLost!(stop => { told.push(stop); });
    const captured = ctx.capturedMemory!()!;
    // A flight cancel retires the memory context through its own listener: no invalidation.
    (captured.flight as { cancel: AbortController }).cancel.abort();
    await new Promise(resolve => setTimeout(resolve, 10));
    afterRetire = told.length;
    client.revokeMemoryAuthority();
    const afterRevoke = told.length;
    off();
    client.revokeMemoryAuthority();
    afterUnsubscribe = told.length;
    assert.equal(afterRevoke, 1);
    throw new Error("fixture end");
  } };
  const r = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  try {
    await r.execute(f.claim);
    assert.equal(afterRetire, 0, "retirement alone must not tell the executor");
    assert.equal(told.length, 1, "the invalidation told it exactly once");
    assert.equal(afterUnsubscribe, 1, "an unsubscribed listener hears nothing more");
    assert.equal(told[0]!.constructor.name, "RetainedRecoveryStop");
    assert.match(told[0]!.message, /retained recovery stopped/);
  } finally { await f.memory.stop(); await monitoring.catch(() => undefined); }
});

test("credential capture settles guarded commands first and goes incomplete while they are unconfirmed", async () => {
  const f = await setup();
  const r = runnerWith(() => ({ executor: {} as Executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  const settle = (executor: Executor) => (r as unknown as {
    settleForCredentialCapture(executor: Executor, deadlineMs: number): Promise<{ kind: string }>;
  }).settleForCredentialCapture(executor, 500);
  try {
    let deadlines: number[] = [];
    let freeCalls = 0;
    const free = async () => { freeCalls++; return { kind: "observed_empty" as const }; };
    const unconfirmed: Executor = { run: async () => { throw new Error("unused"); },
      settleWorkerCommands: async (deadline) => { deadlines.push(deadline); return { confirmed: false }; },
      settleForCredentialFreeCapture: free };
    assert.equal((await settle(unconfirmed)).kind, "incomplete");
    assert.equal(freeCalls, 0, "an unconfirmed lane stops the capture before any other settle");
    assert.equal(deadlines.length, 1);
    assert.ok(deadlines[0]! > 0 && deadlines[0]! <= 500);
    const throwing: Executor = { run: async () => { throw new Error("unused"); },
      settleWorkerCommands: async () => { throw new Error("boom"); } };
    assert.equal((await settle(throwing)).kind, "incomplete");
    deadlines = [];
    const confirmed: Executor = { run: async () => { throw new Error("unused"); },
      settleWorkerCommands: async (deadline) => { deadlines.push(deadline); return { confirmed: true }; },
      settleForCredentialFreeCapture: free };
    assert.equal((await settle(confirmed)).kind, "observed_empty");
    assert.equal(freeCalls, 1);
    // A guard-disabled executor (no settleWorkerCommands) keeps the existing order untouched.
    assert.equal((await settle({ run: async () => { throw new Error("unused"); } })).kind, "observed_empty");
  } finally { await f.memory.stop(); }
});

/** Run a Claude claim whose executor is told the memory invalidation and ends with `ending(stop)`. */
async function invalidatedRun(ending: (stop: Error) => Error) {
  const f = await setup();
  const monitoring = f.memory.run();
  let told: Error | undefined;
  const executor: Executor = { run: async ctx => {
    ctx.onMemoryAuthorityLost!(stop => { told = stop; });
    ctx.capturedMemory!();
    client.revokeMemoryAuthority();
    // Asserted outside the run: an assertion thrown here would itself report `failed`, making the control vacuous.
    throw ending(told ?? new Error("invalidation not delivered"));
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory }).execute(f.claim);
    return { failed: api.states.some(s => s.body.status === "failed"), told: told !== undefined };
  } finally { await f.memory.stop(); await monitoring.catch(() => undefined); }
}

test("a Claude executor that rejects with the carried retained stop lands in the retained-custody arm, not a failure", async () => {
  // The carrier rethrows exactly the stop the runner delivered.
  const r = await invalidatedRun(stop => stop);
  assert.ok(r.told, "the invalidation reached the executor");
  assert.equal(r.failed, false, "the retained stop reports no model failure");
});

test("a Claude executor that rebuilds the error after the invalidation is reported as a failure (the control)", async () => {
  const r = await invalidatedRun(() => new Error("rebuilt after the invalidation"));
  assert.ok(r.told, "the invalidation reached the executor");
  assert.equal(r.failed, true);
});
