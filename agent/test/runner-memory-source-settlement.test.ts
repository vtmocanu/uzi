import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import { describe, it } from "node:test";
import { performance } from "node:perf_hooks";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import type { MemoryParkRecord } from "../src/git.js";
import { execFileSync } from "node:child_process";
import type { Executor, RunContext } from "../src/executor.js";
import type { Logger } from "../src/log.js";
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
  const workerId = client.memoryIncarnation!.worker_id;
  return { memory, claim, workerId, pressure: (bytes: number) => { usage = bytes; } };
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

/** The ACTIVE recovery-journal key of this run's branch, read straight from the worker bare. */
const BRANCH = `agent/issue-${IID}`;
function activeJournal(): { runId: string; clonePath: string; memoryPark?: MemoryParkRecord } | undefined {
  const bare = git.barePathFor(fx.originPath);
  const env: NodeJS.ProcessEnv = { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", PATH: process.env.PATH };
  let raw = "";
  try {
    raw = execFileSync("git", ["-C", bare, "config", "--local", "--get", `uzi-recovery.${BRANCH}.clone`],
      { env, encoding: "utf8", stdio: "pipe" }).trim();
  } catch { return undefined; }
  return raw ? JSON.parse(raw) as { runId: string; clonePath: string; memoryPark?: MemoryParkRecord } : undefined;
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

/** T1: the confirmed, settled cooperative record on the active key, durable and verifiable. */
async function assertSettledRecord(claim: ClaimResponse, workerId: string, rt: AuthorityRuntime, clone: string, slot: SlotSnapshot): Promise<void> {
  const journal = activeJournal();
  assert.ok(journal, "the clone's active journal key survives the park");
  const park = journal.memoryPark;
  assert.ok(park, "the producer recorded the cooperative memory park on the active key");
  assert.equal(park.version, 1);
  assert.equal(park.ack, "confirmed");
  assert.deepEqual(park.binding, { runId: claim.run_id, claimGeneration: 7,
    workerId, registerNonce: "original-memory-registration", memoryEpisode: 3 });
  assert.equal(park.origin.authorityIncarnation, rt.authorityIncarnation());
  assert.equal(park.settlement?.confirmed, true, "phase 2 confirmed the settlement");
  assert.deepEqual(park.settlement?.sources, [{ path: clone, authorityKey: slot.key, attemptId: null, epoch: slot.epoch, seq: slot.seq }]);
  assert.deepEqual(park.settlement?.evidence.parkAck, park.binding);
  assert.deepEqual(await git.readConfirmedMemoryPark(git.barePathFor(fx.originPath), BRANCH,
    { runId: claim.run_id, clonePath: journal.clonePath, record: park }), park, "the lock-held reader confirms it");
  assert.equal(await rt.verifyEndedSlots(park.settlement!.sources, park.origin.authorityIncarnation), true,
    "the authority still shows exactly the recorded ended slot");
  assert.equal(await rt.verifyEndedSlots(park.settlement!.sources, randomUUID()), false, "another incarnation does not verify");
}

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

    await assertSettledRecord(f.claim, f.workerId, r.rt, clone, slot!);

    const next = r.successor();
    const sources = activeJournal()!.memoryPark!.settlement!.sources;
    await next.withTransaction(() => next.hooks.admitSourceUse(clone, "seed-canonical"));
    assert.equal((await slotOf(next, clone))?.state, "active", "the successor admitted the path");
    assert.equal(await r.rt.verifyEndedSlots(sources, activeJournal()!.memoryPark!.origin.authorityIncarnation), false,
      "once a successor minted its own epoch the recorded end no longer verifies");
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
    await assertSettledRecord(f.claim, f.workerId, r.rt, clone, (await slotOf(r.rt, clone))!);
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
    const park = activeJournal()?.memoryPark;
    assert.equal(park?.ack, "sent_unconfirmed", "a park body was sent but never confirmed: recorded as such");
    assert.equal(park?.settlement, undefined);
    assert.equal(park?.binding.claimGeneration, 7);
    const next = r.successor();
    await assert.rejects(next.withTransaction(() => next.hooks.admitSourceUse(clone, "seed-canonical")),
      /active under another worker incarnation/);
  });
});

// Each conjunct of the settlement predicate is independently necessary: the park really happened (a witness exists,
// so a "no witness" trap would pass for the wrong reason), then ONE fact is falsified at the instant teardown asks.
describe("a settled memory park ends the source use only when every fact of the witness holds (issue #3638)", () => {
  type Hold = { context: Record<string, unknown>; origin: Record<string, unknown> } & Record<string, unknown>;
  type Memory = { hold?: Hold; parkWitness?: { origin: unknown; binding: Record<string, unknown> } & Record<string, unknown>;
    ownershipRejected?: boolean };
  type Flight = { workerMemory?: Memory; parked?: boolean; claimGeneration: number };
  const cases: Array<[string, (flight: Flight, memory: Memory) => void]> = [
    ["the executor that wrote the hold is no longer the tearing-down executor", (_flight, memory) => {
      const origin = Object.freeze({ ...memory.hold!.origin, executor: { run: async () => { throw new Error("substitute"); } } });
      memory.hold = Object.freeze({ ...memory.hold!, origin }) as Hold;
      memory.parkWitness = Object.freeze({ ...memory.parkWitness!, origin });
    }],
    ["the hold was written by another flight (wrong cohort)", (_flight, memory) => {
      const origin = Object.freeze({ ...memory.hold!.origin, flightId: randomUUID() });
      memory.hold = Object.freeze({ ...memory.hold!, origin }) as Hold;
      memory.parkWitness = Object.freeze({ ...memory.parkWitness!, origin });
    }],
    ["the flight is not parked", (flight) => { flight.parked = false; }],
    ["the ownership probe was rejected", (_flight, memory) => { memory.ownershipRejected = true; }],
    ["the accepted binding is for another claim generation", (_flight, memory) => {
      memory.hold = Object.freeze({ ...memory.hold!, context: Object.freeze({ ...memory.hold!.context, claim_generation: 99 }) }) as Hold;
      memory.parkWitness = Object.freeze({ ...memory.parkWitness!, binding: Object.freeze({ ...memory.parkWitness!.binding, claim_generation: 99 }) });
    }],
  ];
  for (const [name, tamper] of cases) {
    it(`no settlement when ${name}`, async () => {
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
      const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, authority: r.rt });
      const internals = runner as unknown as { memoryHoldSettled(flight: Flight, executor: Executor): boolean };
      const settled = internals.memoryHoldSettled.bind(runner);
      let tampered = false;
      let verdict: boolean | undefined;
      internals.memoryHoldSettled = (flight, ex) => {
        // Only the first consultation (the teardown's predicate) is tampered; the producer's later one sees the same flight.
        if (tampered) return settled(flight, ex);
        const memory = flight.workerMemory!;
        assert.ok(memory.parkWitness, "the park produced its witness before the fact is falsified");
        assert.equal(flight.parked, true);
        tamper(flight, memory);
        tampered = true;
        return verdict = settled(flight, ex);
      };
      try { await runner.execute(f.claim); } finally { await f.memory.stop(); }

      assert.equal(tampered, true, "the predicate was consulted with a witness in hand");
      assert.equal(verdict, false);
      const clone = worktreeDirFor(IID);
      assert.equal((await slotOf(r.rt, clone))?.state, "active", "fail closed");
      assert.deepEqual(r.outcomes.flatMap(o => o.endedSlots), []);
      const park = activeJournal()?.memoryPark;
      assert.ok(park, "the park is still recorded");
      assert.equal(park.settlement, undefined, "but no settlement is written for a fact that does not hold");
    });
  }
});

// issue #3638 D3 - the producer. The record is complete or it is absent: a settlement is written only when the
// flight's every source path has a confirmed end and the hold is settled by the flight and executor that wrote it.
describe("the cooperative memory-park record is complete or absent (issue #3638)", () => {
  /** Park through the exhausted-allowance path with an optional tweak of the rig before the run. */
  async function exhaustedPark(tweak: (r: Awaited<ReturnType<typeof authorityRig>>, runner: unknown) => void = () => {},
    log = nullLogger()) {
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
    const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, log,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, authority: r.rt });
    tweak(r, runner);
    try { await runner.execute(f.claim); } finally { await f.memory.stop(); }
    return { f, r, clone: worktreeDirFor(IID) };
  }
  const fakeReceipt = (rt: AuthorityRuntime, n: number) => ({
    path: `${fx.dataDir}/fake-source-${String(n).padStart(2, "0")}`, key: n.toString(16).padStart(64, "e"),
    epoch: randomUUID(), seq: 1, worker: rt.authorityIncarnation()! });

  it("a confirmed end the authority did not report for a source path writes no settlement (omitted source)", async () => {
    const { r } = await exhaustedPark(rig => {
      const inner = rig.rt.endSourceUse.bind(rig.rt);
      rig.rt.endSourceUse = async (...args) => ({ ...(await inner(...args)), endedSlots: [] });
    });
    const park = activeJournal()?.memoryPark;
    assert.equal(park?.ack, "confirmed", "the park itself is still recorded");
    assert.equal(park?.settlement, undefined, "no receipt for the clone: no settlement");
    assert.equal(r.outcomes.length > 0, true);
  });

  /** A park with `extra` additional source paths, each with a (fabricated) confirmed end, besides the clone. */
  const withExtraSources = (extra: number) => exhaustedPark((rig, runner) => {
    const internals = runner as { memoryHoldSettled(flight: { sourceUsePaths?: Set<string> }, executor: Executor): boolean };
    const settled = internals.memoryHoldSettled.bind(runner);
    internals.memoryHoldSettled = (flight, executor) => {
      for (let n = 0; n < extra; n++) flight.sourceUsePaths!.add(fakeReceipt(rig.rt, n).path);
      return settled(flight, executor);
    };
    const inner = rig.rt.endSourceUse.bind(rig.rt);
    rig.rt.endSourceUse = async (...args) => {
      const out = await inner(...args);
      return { ...out, endedSlots: [...out.endedSlots, ...Array.from({ length: extra }, (_, n) => fakeReceipt(rig.rt, n))] };
    };
  });

  it("the producer re-checks the hold's own flight and executor after the end: a substitute writes no settlement", async () => {
    // The teardown's own consultation passes and the source use really ends; only the producer's later consultation
    // sees a hold that no longer belongs to this flight (a different cohort).
    const { r, clone } = await exhaustedPark((_rig, runner) => {
      const internals = runner as { memoryHoldSettled(flight: { workerMemory?: { hold?: { origin: object } } }, executor: Executor): boolean };
      const settled = internals.memoryHoldSettled.bind(runner);
      let calls = 0;
      internals.memoryHoldSettled = (flight, executor) => {
        if (++calls === 2) {
          const memory = flight.workerMemory as unknown as { hold: { origin: object }; parkWitness: { origin: object } };
          const origin = Object.freeze({ ...memory.hold.origin, flightId: randomUUID() });
          memory.hold = Object.freeze({ ...memory.hold, origin });
          memory.parkWitness = Object.freeze({ ...memory.parkWitness, origin });
        }
        return settled(flight, executor);
      };
    });
    assert.equal((await slotOf(r.rt, clone))?.state, "ineligible", "the teardown's predicate passed and the end happened");
    const park = activeJournal()?.memoryPark;
    assert.equal(park?.ack, "confirmed");
    assert.equal(park?.settlement, undefined, "the producer's own check refuses a substituted cohort");
  });

  it("sixteen sources is the last settled set", async () => {
    await withExtraSources(15);
    const park = activeJournal()?.memoryPark;
    assert.equal(park?.ack, "confirmed");
    assert.equal(park?.settlement?.sources.length, 16, "exactly sixteen is accepted");
    const paths = park!.settlement!.sources.map(s => s.path);
    assert.deepEqual(paths, [...paths].sort(), "sorted ascending by path");
  });

  it("seventeen sources is never truncated: no settlement", async () => {
    await withExtraSources(16);
    const park = activeJournal()?.memoryPark;
    assert.equal(park?.ack, "confirmed");
    assert.equal(park?.settlement, undefined);
  });

  it("a preservation that stopped before sending the park body records not_sent from the hold context", async () => {
    const f = await setup(2);
    const r = await authorityRig();
    git.worktreeStatus = async () => [];
    // The run is no longer owned as `running` when the park is about to be sent: the preservation stops quietly.
    const executor: Executor = {
      run: async ctx => {
        api.setOwnershipStatus(f.claim.run_id, "paused", 7);
        assert.throws(() => ctx.capturedMemory!(), /retained recovery/i);
        throw new Error("fixture stops original model execution");
      },
    };
    try {
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, authority: r.rt }).execute(f.claim);
    } finally { await f.memory.stop(); }
    assert.equal(parks().length, 0, "no park body reached the server");
    const park = activeJournal()?.memoryPark;
    assert.equal(park?.ack, "not_sent");
    assert.equal(park?.settlement, undefined);
    assert.deepEqual(park?.binding, { runId: f.claim.run_id, claimGeneration: 7, workerId: f.workerId,
      registerNonce: "original-memory-registration", memoryEpisode: 3 });
    assert.equal((await slotOf(r.rt, worktreeDirFor(IID)))?.state, "active");
  });

  it("a failing record write never fails the park, its source use end or the teardown", async () => {
    const messages: string[] = [];
    const log = { ...nullLogger(), warn: (m: string) => { messages.push(m); } };
    log.child = () => log;
    git.recordMemoryPark = async () => { throw new Error("injected record failure"); };
    const { r, clone } = await exhaustedPark(undefined, log);
    assert.equal(api.states.some(s => s.body.status === "failed"), false);
    assert.equal(parks().length, 1);
    assert.equal(activeJournal()?.memoryPark, undefined, "nothing was recorded");
    assert.equal((await slotOf(r.rt, clone))?.state, "ineligible", "the source use end is independent of the record");
    assert.ok(messages.includes("memory park record not written; the park stays unqualified"), messages.join("; "));
  });

  it("the read-only verifier answers false for every difference from the recorded ended slots", async () => {
    const { r } = await exhaustedPark();
    const park = activeJournal()!.memoryPark!;
    const sources = park.settlement!.sources;
    const worker = park.origin.authorityIncarnation;
    assert.equal(await r.rt.verifyEndedSlots(sources, worker), true, "the unchanged record verifies");
    assert.equal(await r.rt.verifyEndedSlots(sources, randomUUID()), false, "another incarnation owns nothing here");
    assert.equal(await r.rt.verifyEndedSlots(sources.map(x => ({ ...x, epoch: randomUUID() })), worker), false, "a changed epoch");
    assert.equal(await r.rt.verifyEndedSlots(sources.map(x => ({ ...x, seq: x.seq + 1 })), worker), false, "a changed seq");
    assert.equal(await r.rt.verifyEndedSlots(sources.map(x => ({ ...x, authorityKey: "0".repeat(64) })), worker), false, "an absent slot");
    assert.equal(await r.rt.verifyEndedSlots([], worker), false, "no sources verify nothing");
    // A dropped lease: another epoch took the slot over.
    const next = r.successor();
    await next.withTransaction(() => next.hooks.admitSourceUse(worktreeDirFor(IID), "seed-canonical"));
    assert.equal(await r.rt.verifyEndedSlots(sources, worker), false, "a successor epoch");
  });

  it("a slot that is still active never verifies as an ended slot", async () => {
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
    const slot = (await slotOf(r.rt, worktreeDirFor(IID)))!;
    assert.equal(slot.state, "active");
    assert.equal(await r.rt.verifyEndedSlots([{ authorityKey: slot.key, epoch: slot.epoch, seq: slot.seq }], slot.owner.worker), false);
  });

  // The predecessor's record is read once by the next claim of the run, only to say whether it still verifies.
  for (const disturbed of [false, true]) {
    it(`a retained claim logs 'predecessor cooperative settlement ${disturbed ? "unverified" : "verified"}' when the next claim recovers the predecessor`, async () => {
      const first = await exhaustedPark();
      const record = activeJournal()!.memoryPark!;
      assert.equal(record.settlement?.confirmed, true);
      const clone = first.clone;
      const next = first.r.successor();
      if (disturbed) {
        // Another incarnation takes the slot over before the recovering claim reads the record.
        const other = first.r.successor();
        await other.withTransaction(() => other.hooks.admitSourceUse(clone, "seed-canonical"));
      }
      const messages: string[] = [];
      const log: Logger = { ...nullLogger(), info: (m: string) => { messages.push(m); } };
      log.child = () => log;
      const claim: ClaimResponse = { ...first.f.claim, claim_generation: 8, memory_episode: 4, memory_policy: undefined,
        memory_intervention_count: 0 };
      api.memoryRegisterNonce = "resumed-memory-registration";
      api.setOwnershipStatus(claim.run_id, "running", 8);
      const executor: Executor = { run: async () => { throw new Error("the recovering claim stops here"); } };
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, log, { authority: next })
        .execute(claim).catch(() => {});
      assert.ok(messages.includes(`predecessor cooperative settlement ${disturbed ? "unverified" : "verified"}`), messages.join("; "));
    });
  }
});
