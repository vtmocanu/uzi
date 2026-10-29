import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import { describe, it } from "node:test";
import { performance } from "node:perf_hooks";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimResponse, MemoryReservationRequest } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

installHarness();

// issue #3638 D1 — an ACCEPTED worker-memory park is positive evidence that settles the memory hold, so the
// flight's source use ends (`ineligible`) and a fresh worker incarnation may admit the path. A preservation that
// failed or was swallowed carries no such evidence and leaves the slot active. Driven through the real
// RunRunner.execute() with the production AuthorityRuntime and the memory runtime of runner-worker-memory.test.ts.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");
const IID = 2396;

const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };

/** The memory fixture of runner-worker-memory.test.ts, trimmed to what these cases need. */
async function setup(count = 0) {
  api.memoryRegisterNonce = "original-memory-registration";
  api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence"]);
  await client.register("memory-fixture", "base");
  const claim: ClaimResponse = { ...gitlabClaim(IID), claim_generation: 7,
    memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 },
    memory_intervention_count: count };
  // A Claude claim (no Codex secret): its process half is joined by the ordinary quiescence proof, so the memory
  // hold is the only tail that can keep the source use open.
  assert.equal(claim.secrets.codex, undefined);
  api.setOwnershipStatus(claim.run_id, "running", 7);
  let usage = 0, sampleId = 0, used = count;
  const reservations: MemoryReservationRequest[] = [];
  const accepted = new Map<string, number>();
  client.reserveMemoryIntervention = async request => {
    reservations.push(request);
    accepted.set(request.intervention_id, ++used);
    return { ...request, admitted: true, authorizing: true, allowance: { limit: 2, used, remaining: 2 - used } };
  };
  client.reportMemoryInterventionOutcome = async request => {
    const spent = accepted.get(request.intervention_id)!;
    return { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: spent, remaining: 2 - spent } };
  };
  const memory = new WorkerMemoryRuntime(settings, client, nullLogger(), {
    reader: { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: ++sampleId, startedAtMs: time, completedAtMs: time,
        currentBytes: usage, limitBytes: 1000,
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0, inactive_file: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
          full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
  return { memory, claim, pressure: (bytes: number) => { usage = bytes; } };
}

async function authorityRig() {
  const dataDir = fs.realpathSync(fx.dataDir);
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
  const outcomes: Array<Awaited<ReturnType<AuthorityRuntime["endSourceUse"]>>> = [];
  const endSourceUse = rt.endSourceUse.bind(rt);
  rt.endSourceUse = async (...args) => { const outcome = await endSourceUse(...args); outcomes.push(outcome); return outcome; };
  return {
    rt, outcomes,
    successor: () => new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 }),
  };
}

async function slotOf(rt: AuthorityRuntime, p: string): Promise<SlotSnapshot | null> {
  return rt.withTransaction(async (tx) => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, p));
  });
}

const parks = () => api.states.filter(s => s.body.recovery_cause === "worker_memory_pressure");
const enroll = (ctx: RunContext) => {
  const captured = ctx.capturedMemory!()!;
  const entry = captured.commands.register(captured.context, {
    source: "codex-model-command", toolId: "original-tool", attemptId: "original-attempt",
    command: "inert fixture", root: { pid: 10, startTime: "10" },
    observe: async () => {
      const time = performance.now();
      return { available: true, complete: true, startedAtMs: time, completedAtMs: time,
        rows: [{ pid: 10, startTime: "10", rssBytes: 100 }] };
    },
    cancel: async () => {},
    drain: async () => ({ confirmed: true }),
  });
  return { captured, entry };
};

describe("a settled memory park ends the flight's source use (issue #3638)", () => {
  it("pressure path: an accepted park moves the slot to ineligible and a fresh incarnation admits the path", async () => {
    const f = await setup(1);
    const r = await authorityRig();
    const monitoring = f.memory.run();
    fs.writeFileSync(homeDir + "/session-proof", "original session");
    git.worktreeStatus = async () => [];
    const executor: Executor = {
      run: async ctx => {
        const command = enroll(ctx);
        f.pressure(950);
        const result = await command.captured.commands.watchOutcome(command.entry);
        ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
        throw new Error("model abort is not a test failure");
      },
    };
    try {
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, authority: r.rt }).execute(f.claim);
    } finally { await f.memory.stop(); await monitoring; }

    assert.equal(parks().length, 1, "the owner submitted exactly one accepted memory hold");
    assert.equal(api.states.some(s => s.body.status === "failed" || s.body.status === "completed"), false);
    assert.equal(fs.existsSync(worktreeDirFor(IID)), true, "the clone is retained");
    const clone = worktreeDirFor(IID);
    assert.equal((await slotOf(r.rt, clone))?.state, "ineligible", "the positive park evidence let the source use end");
    const ended = r.outcomes.flatMap(o => o.endedSlots).filter(e => e.path === clone);
    assert.equal(ended.length, 1, "endSourceUse reported the confirmed end of the clone");
    const slot = await slotOf(r.rt, clone);
    assert.deepEqual([ended[0]!.epoch, ended[0]!.seq, ended[0]!.key], [slot?.epoch, slot?.seq, slot?.key],
      "the receipt is the saved identity of the ended slot");

    const next = r.successor();
    await next.withTransaction(() => next.hooks.admitSourceUse(clone, "seed-canonical"));
    assert.equal((await slotOf(next, clone))?.state, "active", "the successor admitted the path");
  });

  it("exhausted allowance path: an accepted park ends the source use as well", async () => {
    const f = await setup(2);
    const r = await authorityRig();
    fs.writeFileSync(homeDir + "/session-proof", "original session");
    git.worktreeStatus = async () => [];
    const executor: Executor = {
      run: async ctx => {
        api.setOwnershipStatus(f.claim.run_id, "running", 7);
        assert.throws(() => ctx.capturedMemory!(), /retained recovery/i);
        throw new Error("fixture stops original model execution");
      },
    };
    try {
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, authority: r.rt }).execute(f.claim);
    } finally { await f.memory.stop(); }

    assert.equal(parks().length, 1);
    assert.equal(api.states.some(s => s.body.status === "failed" || s.body.status === "completed"), false);
    const clone = worktreeDirFor(IID);
    assert.equal((await slotOf(r.rt, clone))?.state, "ineligible");
    assert.equal(r.outcomes.flatMap(o => o.endedSlots).filter(e => e.path === clone).length, 1);
    const next = r.successor();
    await next.withTransaction(() => next.hooks.admitSourceUse(clone, "seed-canonical"));
    assert.equal((await slotOf(next, clone))?.state, "active");
  });

  it("a preservation that did not get an accepted park carries no evidence: the slot stays active", async () => {
    // The server never confirms the park (the ownership route answers "running" for another generation), so the
    // operation settles (its finally always sets settled) with no witness: the hold alone must not end the use.
    const f = await setup(2);
    const r = await authorityRig();
    git.worktreeStatus = async () => [];
    api.overrideStateStatus(f.claim.run_id, "running");
    const executor: Executor = {
      run: async ctx => {
        api.setOwnershipStatus(f.claim.run_id, "running", 7);
        assert.throws(() => ctx.capturedMemory!(), /retained recovery/i);
        throw new Error("fixture stops original model execution");
      },
    };
    try {
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, authority: r.rt }).execute(f.claim);
    } finally { await f.memory.stop(); }

    assert.equal(api.states.some(s => s.body.status === "failed"), false, "a quiet retained stop");
    const clone = worktreeDirFor(IID);
    assert.equal((await slotOf(r.rt, clone))?.state, "active", "no witness: fail closed");
    assert.deepEqual(r.outcomes.flatMap(o => o.endedSlots), [], "no confirmed end was produced");
    const next = r.successor();
    await assert.rejects(next.withTransaction(() => next.hooks.admitSourceUse(clone, "seed-canonical")),
      /active under another worker incarnation/);
  });
});
