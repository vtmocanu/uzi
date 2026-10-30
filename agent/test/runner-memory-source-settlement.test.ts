import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
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
  return { memory, claim, workerId, reservations, pressure: (bytes: number) => { usage = bytes; } };
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
async function assertSettledRecord(claim: ClaimResponse, workerId: string, rt: AuthorityRuntime, clone: string, slot: SlotSnapshot,
  interventionIds: string[] = []): Promise<void> {
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
  assert.deepEqual(park.settlement?.evidence.interventionIds, interventionIds, "the settlement names the intervention that produced the hold");
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

    assert.equal(f.reservations.length, 1, "exactly one pressure intervention was reserved");
    await assertSettledRecord(f.claim, f.workerId, r.rt, clone, slot!, [f.reservations[0]!.intervention_id]);

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
    // A deadline far above any plausible load: the park body must reach the server before preservation can stop on time.
    try {
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 120_000, authority: r.rt }).execute(f.claim);
    } finally { await f.memory.stop(); }

    // Gate every expectation on the premise: exactly one park body was sent (and never confirmed).
    assert.equal(parks().length, 1, "exactly one park body was sent to the server");
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

// T1: the positive end of the story. After an accepted, settled park the owner's Resume is a SUCCESSOR claim on a fresh
// authority incarnation: it recovers the retained clone through the real retained recovery, is admitted (the slot is
// ineligible) and reaches the successor executor, with the predecessor's work still in the worktree it is handed.
describe("a settled memory park lets the successor claim continue on the retained work (issue #3638)", () => {
  const MARKER = "predecessor-work.txt";
  for (const producer of ["pressure", "exhausted allowance"] as const) {
    it(`${producer}: the successor claim on a fresh incarnation reaches its executor with the predecessor's work preserved`, async () => {
      const f = await setup(producer === "pressure" ? 1 : 2);
      const r = await authorityRig();
      fs.writeFileSync(homeDir + "/session-proof", "original session");
      git.worktreeStatus = async () => [];
      const monitoring = producer === "pressure" ? f.memory.run() : Promise.resolve();
      const executor: Executor = {
        run: async ctx => {
          fs.writeFileSync(path.join(ctx.worktreePath, MARKER), "predecessor wrote this");
          execFileSync("git", ["-C", ctx.worktreePath, "add", MARKER], { stdio: "pipe" });
          execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.name=predecessor", "-c", "user.email=p@example.test",
            "commit", "-m", "predecessor work"], { stdio: "pipe" });
          if (producer === "pressure") {
            const command = enroll(ctx);
            f.pressure(950);
            const result = await command.captured.commands.watchOutcome(command.entry);
            ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
          } else {
            api.setOwnershipStatus(f.claim.run_id, "running", 7);
            assert.throws(() => ctx.capturedMemory!(), /retained recovery/i);
          }
          throw new Error("model abort is not a test failure");
        },
      };
      try {
        await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
          { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, authority: r.rt }).execute(f.claim);
      } finally { await f.memory.stop(); await monitoring; }
      const clone = worktreeDirFor(IID);
      assert.equal(parks().length, 1, "the predecessor parked");
      assert.equal(activeJournal()?.memoryPark?.settlement?.confirmed, true, "with a settled record");
      assert.equal((await slotOf(r.rt, clone))?.state, "ineligible");
      assert.equal(fs.readFileSync(path.join(clone, MARKER), "utf8"), "predecessor wrote this", "the predecessor's work is retained");

      // The owner's Resume: a later generation and episode, a fresh registration, a fresh authority incarnation.
      api.memoryRegisterNonce = "resumed-memory-registration";
      await client.register("memory-fixture", "base");
      const resumed: ClaimResponse = { ...f.claim, claim_generation: 8, memory_episode: 4, memory_policy: null, memory_intervention_count: 0 };
      api.setOwnershipStatus(resumed.run_id, "running", 8);
      const seen: Array<{ worktree: string; marker: string | null }> = [];
      const successor: Executor = {
        run: async ctx => {
          seen.push({ worktree: ctx.worktreePath,
            marker: fs.existsSync(path.join(ctx.worktreePath, MARKER)) ? fs.readFileSync(path.join(ctx.worktreePath, MARKER), "utf8") : null });
          throw new Error("the successor stops here");
        },
      };
      const next = r.successor();
      await runnerWith(() => ({ executor: successor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { codexBoundaryDeadlineMs: 5000, authority: next }).execute(resumed).catch(() => {});

      assert.equal(seen.length, 1, "the successor executor ran: the retained continuation was not refused");
      assert.equal(seen[0]!.marker, "predecessor wrote this", "and it works on the predecessor's retained work");
      assert.ok(path.resolve(seen[0]!.worktree).startsWith(`${path.resolve(clone)}.attempt-`),
        `in a fresh attempt clone restored from the retained one: ${seen[0]!.worktree}`);
      assert.equal(api.states.filter(s => s.body.status === "failed" && s.body.failure_reason?.includes("preservation")).length,
        0, "no preservation_failed");
    });
  }
});

// T2: the hold alone is no evidence in either direction. A failed preservation settles its operation (finally) without a
// witness, and clearing the hold by hand must not let an unresolved join through.
describe("the memory settlement bookkeeping cannot be tricked into an end (issue #3638)", () => {
  type Flight = { workerMemory?: { hold?: unknown; settled?: boolean; operation?: unknown; parkWitness?: unknown;
    feedback: Map<string, unknown> }; parked?: boolean; sourceTailsJoined?: boolean };
  type Internals = {
    memoryHoldSettled(flight: Flight, executor: Executor): boolean;
    runCancellationMaintenance(flight: Flight, fn: () => Promise<unknown>, ...rest: unknown[]): Promise<unknown>;
    preserveWorkerMemory(...args: unknown[]): Promise<void>;
  };
  async function exhausted(tweak: (runner: Internals) => void) {
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
    tweak(runner as unknown as Internals);
    try { await runner.execute(f.claim); } finally { await f.memory.stop(); }
    return { f, r, clone: worktreeDirFor(IID) };
  }

  it("a preservation that throws settles its operation but leaves no witness: the slot stays active and the hold is never settled", async () => {
    const verdicts: boolean[] = [];
    let seen: Flight | undefined;
    const { r, clone } = await exhausted(runner => {
      runner.preserveWorkerMemory = async () => { throw new Error("injected preservation failure"); };
      const settled = runner.memoryHoldSettled.bind(runner);
      runner.memoryHoldSettled = (flight, ex) => { seen = flight; const v = settled(flight, ex); verdicts.push(v); return v; };
    });
    assert.ok(seen, "the settlement predicate was consulted for a flight holding a memory hold");
    assert.equal(seen.workerMemory?.settled, true, "the operation settled through its finally despite the failure");
    assert.equal(seen.workerMemory?.parkWitness, undefined, "and produced no witness");
    assert.equal(parks().length, 0, "no park was accepted");
    assert.equal(verdicts.length > 0 && verdicts.every(v => !v), true, `memoryHoldSettled is false every time: ${verdicts.join(",")}`);
    assert.equal(seen.sourceTailsJoined, false);
    assert.equal((await slotOf(r.rt, clone))?.state, "active", "fail closed: the source use did not end");
    assert.deepEqual(r.outcomes.flatMap(o => o.endedSlots), []);
    assert.equal(activeJournal()?.memoryPark?.settlement, undefined);
  });

  it("a park witness with the hold cleared by hand does not end the source use while a required join is unresolved", async () => {
    let flightSeen: Flight | undefined;
    let tampered = false;
    const { r, clone } = await exhausted(runner => {
      const inner = runner.runCancellationMaintenance.bind(runner);
      runner.runCancellationMaintenance = (flight, fn, ...rest) => {
        const memory = flight.workerMemory;
        if (!tampered && memory?.parkWitness && flight.parked) {
          tampered = true; flightSeen = flight;
          memory.hold = undefined; // cleared by hand, as nothing in production does
          memory.feedback.set("outstanding-feedback", {}); // a required join (undelivered feedback) stays unresolved
        }
        return inner(flight, fn, ...rest);
      };
    });
    assert.equal(tampered, true, "the witness existed before the hold was cleared");
    assert.equal(flightSeen?.workerMemory?.hold, undefined);
    assert.equal(flightSeen?.sourceTailsJoined, false, "the unresolved join keeps the tails unjoined");
    assert.equal((await slotOf(r.rt, clone))?.state, "active", "no end");
    assert.deepEqual(r.outcomes.flatMap(o => o.endedSlots), []);
    assert.equal(activeJournal()?.memoryPark?.settlement, undefined, "and no settlement");
  });

  // The crash/companion boundary: the process dies after the park was accepted but before teardown's producer ran, so no
  // end and no record exist for this attempt. The next claimant's real disposition is today's path.
  it("crash/companion boundary: a process that died before endFlightSourceUse and the record write leaves today's preservation failure for a foreign-active slot", async () => {
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
    const first = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, authority: r.rt });
    // Process death: neither the end nor the record producer ever runs (the old runtime is then discarded).
    const dead = first as unknown as { endFlightSourceUse(): Promise<unknown>; recordMemoryParkOutcome(): Promise<void> };
    dead.endFlightSourceUse = async () => ({ paths: [], endedSlots: [], published: false, ownedEpochs: {} });
    dead.recordMemoryParkOutcome = async () => {};
    try { await first.execute(f.claim); } finally { await f.memory.stop(); }
    const clone = worktreeDirFor(IID);
    assert.equal(parks().length, 1, "premise: the park was accepted by the server");
    assert.equal((await slotOf(r.rt, clone))?.state, "active", "premise: the slot was never ended");
    assert.equal(activeJournal()?.memoryPark, undefined, "premise: no record was written for this attempt");

    api.memoryRegisterNonce = "resumed-memory-registration";
    await client.register("memory-fixture", "base");
    const resumed: ClaimResponse = { ...f.claim, claim_generation: 8, memory_episode: 4, memory_policy: null, memory_intervention_count: 0 };
    api.setOwnershipStatus(resumed.run_id, "running", 8);
    let ran = 0;
    const successor: Executor = { run: async () => { ran++; throw new Error("a refused continuation runs no model"); } };
    const before = parks().length;
    await runnerWith(() => ({ executor: successor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
      { codexBoundaryDeadlineMs: 5000, authority: r.successor() }).execute(resumed).catch(() => {});

    assert.equal(ran, 0, "the successor executor never ran");
    assert.equal(parks().length, before, "no re-park without provenance");
    const failed = api.states.filter(s => s.body.status === "failed");
    assert.equal(failed.length, 1, "today's path: the run is failed");
    assert.match(failed[0]!.body.failure_reason ?? "", /Retained recovery blocked: preservation failed/);
    assert.equal((await slotOf(r.rt, clone))?.state, "active", "the predecessor's slot is untouched");
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
  for (const scenario of ["shutdown pending", "shutdown during write", "original deadline first"] as const) {
    it(`memory-park write respects the earliest deadline: ${scenario}`, async t => {
      const r = await authorityRig();
      const runner = runnerWith(() => ({ executor: { run: async () => { throw new Error("unused"); } }, homeDir }),
        fakeGitlab().gitlab, undefined, undefined,
        { authority: r.rt, codexBoundaryDeadlineMs: 1000, shutdownPublishTimeoutMs: 100 });
      const wire = { run_id: randomUUID(), worker_id: randomUUID(), register_nonce: "nonce", claim_generation: 7, memory_episode: 3 };
      const flight = { runId: wire.run_id, claimGeneration: 7, flightId: randomUUID(), workerMemory: {}, executor: {},
        barePath: "bare", branch: "agent/issue-2396", worktreePath: "/clone",
        memoryRepark: { authorityIncarnation: randomUUID(), sourceEpoch: randomUUID(), attemptId: null, sent: wire } };
      let writerSignal: AbortSignal | undefined;
      t.mock.method(git, "recordMemoryPark", async (...args: Parameters<typeof git.recordMemoryPark>) => {
        writerSignal = args[4]?.signal;
        await new Promise<void>(resolve => writerSignal!.addEventListener("abort", () => resolve(), { once: true }));
        return "unconfirmed" as const;
      });
      const internal = runner as unknown as { recordMemoryParkOutcome(...args: unknown[]): Promise<void> };
      t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: 1000 });
      if (scenario === "shutdown pending") runner.shutdown();
      let finished = false;
      const writing = internal.recordMemoryParkOutcome(flight, nullLogger(), {}, {}).then(() => { finished = true; });
      try {
        assert.ok(writerSignal, "the producer started the pending writer");
        if (scenario !== "shutdown pending") {
          t.mock.timers.tick(scenario === "original deadline first" ? 950 : 50);
          runner.shutdown();
        }
        const remaining = scenario === "original deadline first" ? 50 : 100;
        t.mock.timers.tick(remaining - 1);
        assert.equal(writerSignal.aborted, false, "shutdown never cuts off the remaining grace early");
        assert.equal(finished, false);
        t.mock.timers.tick(1);
        assert.equal(writerSignal.aborted, true, "the pending writer is aborted at the earliest deadline");
        await writing;
        assert.equal(finished, true);
      } finally {
        t.mock.timers.tick(5000);
        await writing;
      }
    });
  }

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

  it("a flight that never owned a slot for its clone path records no memory park (source epoch unknown)", async () => {
    const messages: string[] = [];
    const log = { ...nullLogger(), warn: (m: string) => { messages.push(m); } };
    log.child = () => log;
    await exhaustedPark(rig => {
      // Neither the owned epoch nor an ended-slot receipt names the clone: the epoch is not determinable.
      rig.rt.ownedSourceEpoch = () => undefined;
      const inner = rig.rt.endSourceUse.bind(rig.rt);
      rig.rt.endSourceUse = async (...args) => ({ ...(await inner(...args)), endedSlots: [] });
    }, log);
    assert.equal(activeJournal()?.memoryPark, undefined, "no record without a source epoch");
    assert.ok(messages.includes("memory park not recorded: the flight's source epoch is unknown"), messages.join("; "));
  });

  it("teardown retires the sink, takes the admission barrier, and only then ends the source use", async () => {
    const order: string[] = [];
    await exhaustedPark(rig => {
      for (const name of ["retireSink", "awaitSinkQuiescent", "endSourceUse"] as const) {
        const bag = rig.rt as unknown as Record<string, (...args: unknown[]) => unknown>;
        const inner = bag[name]!.bind(rig.rt);
        bag[name] = (...args: unknown[]) => { order.push(name); return inner(...args); };
      }
    });
    assert.deepEqual(order, ["retireSink", "awaitSinkQuiescent", "endSourceUse"]);
  });

  it("a published end writes no settlement even though the park is confirmed and the slot ended", async () => {
    const { r } = await exhaustedPark((_rig, runner) => {
      const internals = runner as { endFlightSourceUse(...args: unknown[]): Promise<Record<string, unknown>> };
      const inner = internals.endFlightSourceUse.bind(runner);
      internals.endFlightSourceUse = async (...args) => ({ ...(await inner(...args)), published: true });
    });
    const park = activeJournal()?.memoryPark;
    assert.equal(park?.ack, "confirmed", "the park itself is still recorded");
    assert.equal(park?.settlement, undefined, "a published end is not a settlement");
    assert.equal(r.outcomes.flatMap(o => o.endedSlots).length, 1, "the source use did end, so only the published conjunct refused");
  });

  it("an ended slot owned by another authority incarnation writes no settlement", async () => {
    const { r } = await exhaustedPark(rig => {
      const inner = rig.rt.endSourceUse.bind(rig.rt);
      rig.rt.endSourceUse = async (...args) => {
        const out = await inner(...args);
        return { ...out, endedSlots: out.endedSlots.map(e => ({ ...e, worker: randomUUID() })) };
      };
    });
    const park = activeJournal()?.memoryPark;
    assert.equal(park?.ack, "confirmed");
    assert.equal(park?.settlement, undefined, "a receipt of another incarnation is no witness for this one");
    assert.equal(r.outcomes.flatMap(o => o.endedSlots).length, 1);
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
    assert.equal(await r.rt.verifyEndedSlots([{ path: worktreeDirFor(IID), authorityKey: slot.key, epoch: slot.epoch, seq: slot.seq }], slot.owner.worker), false);
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
