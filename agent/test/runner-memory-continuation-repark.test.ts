import path from "node:path";
import { Outbox } from "../src/outbox.js";
import type { PolicyHoldRequest } from "../src/protocol.js";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import { describe, it } from "node:test";
import { performance } from "node:perf_hooks";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import type { MemoryParkRecord } from "../src/git.js";
import type { Executor } from "../src/executor.js";
import type { Logger } from "../src/log.js";
import type { ClaimResponse, MemoryReservationRequest, StateRequest } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

installHarness();

// issue #3638 D4 - a retained claim whose predecessor's source use was never positively ended, but whose journal proves
// that the predecessor parked the run for worker memory pressure, re-parks for the owner at `worker_memory_pressure`
// instead of failing the run as `preservation_failed`. Driven through the real RunRunner.execute() with the production
// AuthorityRuntime, the memory runtime of runner-worker-memory.test.ts and the fake API. The first claim produces the
// predecessor (an active slot and a `memoryPark` record); the second claim is the owner's Resume.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");
const IID = 2396;
const BRANCH = `agent/issue-${IID}`;
const NONCE = "original-memory-registration";
const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };

function memoryRuntime(): WorkerMemoryRuntime {
  let sampleId = 0;
  return new WorkerMemoryRuntime(settings, client, nullLogger(), {
    reader: { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: ++sampleId, startedAtMs: time, completedAtMs: time,
        currentBytes: 0, limitBytes: 1000,
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0, inactive_file: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
          full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
}

async function setup(count = 0, policy = false) {
  api.memoryRegisterNonce = NONCE;
  api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence",
    ...(policy ? ["provider_policy_hold_v1"] : [])]);
  await client.register("memory-fixture", "base", policy ? 1 : undefined, undefined, policy ? ["provider_policy_hold_v1"] : undefined,
    undefined, policy ? 0 : undefined);
  const claim: ClaimResponse = { ...gitlabClaim(IID), claim_generation: 7,
    memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: count };
  api.setOwnershipStatus(claim.run_id, "running", 7);
  let used = count;
  const accepted = new Map<string, number>();
  client.reserveMemoryIntervention = async (request: MemoryReservationRequest) => {
    accepted.set(request.intervention_id, ++used);
    return { ...request, admitted: true, authorizing: true, allowance: { limit: 2, used, remaining: 2 - used } };
  };
  client.reportMemoryInterventionOutcome = async request => {
    const spent = accepted.get(request.intervention_id)!;
    return { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: spent, remaining: 2 - spent } };
  };
  return { memory: memoryRuntime(), claim, workerId: client.memoryIncarnation!.worker_id };
}

async function authorityRig() {
  const dataDir = fs.realpathSync(fx.dataDir);
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
  return {
    rt,
    successor: () => new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 }),
  };
}

async function slotOf(rt: AuthorityRuntime, p: string): Promise<SlotSnapshot | null> {
  return rt.withTransaction(async (tx) => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, p));
  });
}

type Journal = { runId: string; clonePath: string; attemptId?: string; memoryPark?: MemoryParkRecord } & Record<string, unknown>;
const GIT_ENV: NodeJS.ProcessEnv = { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", PATH: process.env.PATH };
const journalKey = `uzi-recovery.${BRANCH}.clone`;
const bareOf = () => git.barePathFor(fx.originPath);
function activeJournal(): Journal | undefined {
  let raw = "";
  try { raw = execFileSync("git", ["-C", bareOf(), "config", "--local", "--get", journalKey], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim(); }
  catch { return undefined; }
  return raw ? JSON.parse(raw) as Journal : undefined;
}
function rewriteJournal(mutate: (journal: Journal) => void): void {
  const journal = activeJournal()!;
  mutate(journal);
  execFileSync("git", ["-C", bareOf(), "config", "--local", journalKey, JSON.stringify(journal)], { env: GIT_ENV, stdio: "pipe" });
}
const withoutPark = (journal: Journal | undefined): string => {
  const { memoryPark: _park, ...rest } = journal!;
  void _park;
  return JSON.stringify(rest);
};

const parkBody2 = (body: StateRequest): boolean => body.recovery_cause === "worker_memory_pressure";
const parkBody = (s: { body: StateRequest }) => parkBody2(s.body);
const parks = () => api.states.filter(parkBody);
const terminals = () => api.states.filter(s => s.body.status === "failed" || s.body.status === "completed");

type Kind = "sent_unconfirmed" | "not_sent" | "confirmed";

/** The predecessor: a first claim (generation 7, memory episode 3) whose memory park left its slot ACTIVE. */
async function predecessor(kind: Kind, policy = false) {
  const f = await setup(2, policy);
  const r = await authorityRig();
  fs.writeFileSync(homeDir + "/session-proof", "original session");
  git.worktreeStatus = async () => [];
  if (kind === "sent_unconfirmed") {
    // The park body reaches the server but its answer carries no accepted binding.
    api.stateAckWire = (body, run) => parkBody({ body }) ? { status: 200, body: { run: { ...run, status: "running" } } } : undefined;
  }
  if (kind === "confirmed") {
    // The park is accepted, but the source use end never confirms: the slot stays active under a confirmed record.
    r.rt.endSourceUse = async (paths) => ({ ended: [], kept: [...paths], endedSlots: [] });
  }
  const executor: Executor = {
    run: async ctx => {
      api.setOwnershipStatus(f.claim.run_id, kind === "not_sent" ? "paused" : "running", 7);
      assert.throws(() => ctx.capturedMemory!(), /retained recovery/i);
      throw new Error("fixture stops original model execution");
    },
  };
  try {
    await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: kind === "confirmed" ? 5000 : 120_000, authority: r.rt }).execute(f.claim);
  } finally { await f.memory.stop(); api.stateAckWire = undefined; }
  const clone = worktreeDirFor(IID);
  const journal = activeJournal();
  assert.equal(journal?.memoryPark?.ack, kind, "the predecessor recorded its park outcome");
  assert.equal(journal?.memoryPark?.settlement, undefined, "and no settlement");
  assert.equal((await slotOf(r.rt, clone))?.state, "active", "the predecessor's source use was never ended");
  assert.equal(terminals().length, 0, "the predecessor failed nothing");
  return { f, r, clone, baseline: withoutPark(journal), slot: (await slotOf(r.rt, clone))! };
}
type Predecessor = Awaited<ReturnType<typeof predecessor>>;

let identity = { workerId: "", nonce: NONCE };

type Counters = { reports: number; reserve: number; block: number; capture: number; admit: number; preserve: number; ran: number };

/** The owner's Resume: a fresh claim of the same run at a later generation and memory episode. */
async function resume(p: Predecessor, o: {
  policy?: boolean; generation?: number; episode?: number; authority?: AuthorityRuntime | "same"; noMemory?: boolean; log?: Logger;
  extra?: Parameters<typeof runnerWith>[4]; arm?: () => void; wrap?: (rt: AuthorityRuntime) => void;
} = {}) {
  const generation = o.generation ?? 8;
  // The owner's Resume reaches a worker registration of its own (a new nonce), as a restarted worker would.
  api.memoryRegisterNonce = `resumed-memory-registration-${generation}`;
  await client.register("memory-fixture", "base", o.policy ? 1 : undefined, undefined, o.policy ? ["provider_policy_hold_v1"] : undefined,
    undefined, o.policy ? 0 : undefined);
  if (o.policy) assert.equal(client.supportsPolicyHold, true, "policy reconciliation is negotiated before execution");
  identity = { workerId: client.memoryIncarnation!.worker_id, nonce: api.memoryRegisterNonce };
  const claim: ClaimResponse = { ...p.f.claim, claim_generation: generation, memory_episode: o.episode ?? 4,
    memory_policy: null, memory_intervention_count: 0 };
  api.setOwnershipStatus(claim.run_id, "running", generation);
  const authority = o.authority === "same" ? p.r.rt : o.authority ?? p.r.successor();
  o.wrap?.(authority);
  const counters: Counters = { reports: 0, reserve: 0, block: 0, capture: 0, admit: 0, preserve: 0, ran: 0 };
  const executor: Executor = { run: async () => { counters.ran++; throw new Error("a refused continuation runs no model"); } };
  const memory = o.noMemory ? undefined : memoryRuntime();
  const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, o.log,
    { ...(memory ? { memoryRuntime: memory } : {}), codexBoundaryDeadlineMs: 5000, authority, ...o.extra });
  const spy = <T extends object>(target: T, name: string, counter: keyof Counters): void => {
    const bag = target as unknown as Record<string, (...args: unknown[]) => unknown>;
    const inner = bag[name]!.bind(target);
    bag[name] = (...args: unknown[]) => { counters[counter]++; return inner(...args); };
  };
  spy(git, "reserveRecoveryIteration", "reserve");
  spy(git, "blockRecoveryEpisode", "block");
  spy(runner, "captureRecoveryRestorePoint", "capture");
  spy(runner, "admitPredecessorSource", "admit");
  spy(runner, "preserveWorkerMemory", "preserve");
  let teardown: { policyHold: boolean; policyAdmission: boolean; memoryHold: boolean; repark?: unknown; parked: boolean } | undefined;
  const observer = runner as unknown as { recordMemoryParkOutcome: (flight: {
    policyHold?: unknown; policyAdmission?: boolean; workerMemory?: { hold?: unknown };
    memoryRepark?: unknown; parked: boolean;
  }, ...args: unknown[]) => Promise<void> };
  const record = observer.recordMemoryParkOutcome.bind(runner);
  observer.recordMemoryParkOutcome = async (flight, ...args) => {
    teardown = { policyHold: !!flight.policyHold, policyAdmission: !!flight.policyAdmission,
      memoryHold: !!flight.workerMemory?.hold, repark: structuredClone(flight.memoryRepark), parked: flight.parked };
    await record(flight, ...args);
  };
  o.arm?.();
  const before = parks().length;
  // Runner-level park reports, including those the server declines without recording.
  const reportState = client.reportState.bind(client);
  client.reportState = (runId: string, body: StateRequest, signal?: AbortSignal) => {
    if (parkBody2(body)) counters.reports++;
    return reportState(runId, body, signal);
  };
  try { await runner.execute(claim); } finally { client.reportState = reportState; await memory?.stop(); }
  return { claim, counters, authority, teardown, newParks: () => parks().slice(before) };
}

/** The slot the predecessor left behind is exactly as it was: nothing admitted, ended or stolen. */
async function assertUntouched(p: Predecessor, journalBaseline = p.baseline): Promise<void> {
  assert.deepEqual(await slotOf(p.r.rt, p.clone), p.slot, "the predecessor's active slot is unchanged");
  assert.equal(withoutPark(activeJournal()), journalBaseline, "the journal is unchanged apart from the memory park record");
  assert.equal(fs.existsSync(p.clone), true, "the clone is retained");
}

function assertRecord(p: Predecessor, expected: { generation: number; episode: number; ack: "confirmed" | "sent_unconfirmed" }): void {
  const park = activeJournal()?.memoryPark;
  assert.ok(park, "a memory park record is on the active key");
  assert.deepEqual(park.binding, { runId: p.f.claim.run_id, claimGeneration: expected.generation, workerId: identity.workerId,
    registerNonce: identity.nonce, memoryEpisode: expected.episode });
  assert.equal(park.ack, expected.ack);
  assert.equal(park.settlement, undefined, "provenance only: a re-park settles nothing");
  assert.equal(park.origin.authorityIncarnation, p.r.rt.authorityIncarnation(), "the slot owner of the predecessor is preserved");
  assert.equal(park.origin.sourceEpoch, p.slot.epoch, "the predecessor's source epoch is preserved");
}

function assertNothingDone(c: Counters): void {
  assert.deepEqual({ ...c, reports: 0 }, { reports: 0, reserve: 0, block: 0, capture: 0, admit: 0, preserve: 0, ran: 0 },
    "no reservation, recovery-episode charge, capture, admission, second preservation or model turn");
}

/** A refused continuation that fell through to today's terminal path. */
function assertToday(c: ReturnType<typeof resume> extends Promise<infer T> ? T : never): void {
  assert.equal(c.newParks().length, 0, "no memory re-park");
  assert.ok(api.states.some(s => s.body.status === "failed"), "today's path: the run is failed");
}

describe("a refused memory continuation re-parks for the owner instead of failing (issue #3638 D4)", () => {
  it("lost-ACK regression E -> E+1: the server applied the park at E, every ACK was lost, the owner Resumed, and the fresh claimant re-parks", async () => {
    const p = await predecessor("sent_unconfirmed");
    const c = await resume(p, { generation: 8, episode: 4 });
    const sent = c.newParks();
    assert.equal(sent.length, 1, "exactly one park report");
    assert.deepEqual([sent[0]!.body.claim_generation, sent[0]!.body.memory_episode, sent[0]!.body.register_nonce],
      [8, 4, identity.nonce], "at the claim's OWN episode, no increment");
    assert.equal(terminals().length, 0, "zero terminal reports");
    assertNothingDone(c.counters);
    await assertUntouched(p);
    assertRecord(p, { generation: 8, episode: 4, ack: "confirmed" });
  });

  it("the park the server never applied and requeued at the same episode (C === E) re-parks too", async () => {
    const p = await predecessor("sent_unconfirmed");
    const c = await resume(p, { generation: 8, episode: 3 });
    assert.equal(c.newParks().length, 1);
    assert.equal(c.newParks()[0]!.body.memory_episode, 3);
    assert.equal(terminals().length, 0);
    assertNothingDone(c.counters);
    await assertUntouched(p);
    assertRecord(p, { generation: 8, episode: 3, ack: "confirmed" });
  });

  it("a predecessor held by an earlier flight of this very process (held_unended) re-parks as well", async () => {
    const p = await predecessor("sent_unconfirmed");
    const c = await resume(p, { authority: "same" });
    assert.equal(c.newParks().length, 1);
    assert.equal(terminals().length, 0);
    assertNothingDone(c.counters);
    await assertUntouched(p);
    assertRecord(p, { generation: 8, episode: 4, ack: "confirmed" });
  });

  it("journal attempts and everything else stay unchanged across four Resume cycles", async () => {
    const p = await predecessor("sent_unconfirmed");
    for (let cycle = 0; cycle < 4; cycle++) {
      const c = await resume(p, { generation: 8 + cycle, episode: 4 + cycle });
      assert.equal(c.newParks().length, 1, `cycle ${cycle}: one park report`);
      assert.equal(c.newParks()[0]!.body.memory_episode, 4 + cycle);
      assert.equal(terminals().length, 0);
      assertNothingDone(c.counters);
      await assertUntouched(p);
      assertRecord(p, { generation: 8 + cycle, episode: 4 + cycle, ack: "confirmed" });
      assert.equal(activeJournal()?.recovery, undefined, "no recovery iteration was ever charged");
    }
  });

  it("an exact 409 replay of the park counts as accepted", async () => {
    const p = await predecessor("sent_unconfirmed");
    const c = await resume(p, { arm: () => {
      // An exact replay: the run is already held at this episode by this registration.
      api.stateAckWire = (body, run) => parkBody({ body }) ? { status: 409, body: { run: { ...run, worker_id: identity.workerId,
        recovery_wait_cause: "worker_memory_pressure", memory_hold: { episode: body.memory_episode } } } } : undefined;
    } });
    assert.equal(c.newParks().length, 1, "accepted on the first report");
    assert.equal(terminals().length, 0);
    assertRecord(p, { generation: 8, episode: 4, ack: "confirmed" });
  });

  it("a lost ACK on the re-park itself: the transport retry's replay is accepted", async () => {
    const p = await predecessor("sent_unconfirmed");
    const c = await resume(p, { arm: () => api.afterPersistState(p.f.claim.run_id, parkBody2, "drop") });
    assert.equal(c.counters.reports, 1, "one runner-level report");
    assert.equal(c.newParks().length, 2, "the server saw the report and the transport retry's replay");
    assert.equal(terminals().length, 0);
    assertRecord(p, { generation: 8, episode: 4, ack: "confirmed" });
  });

  it("a crash between persistence and ACK: the report throws after the server applied it; nonterminal, recorded sent_unconfirmed, and the next Resume re-parks again", async () => {
    const p = await predecessor("sent_unconfirmed");
    const original = client.reportState.bind(client);
    client.reportState = async (runId: string, body: StateRequest, signal?: AbortSignal) => {
      const ack = await original(runId, body, signal);
      if (body.recovery_cause === "worker_memory_pressure") throw new Error("the process lost the ACK after the server applied the park");
      return ack;
    };
    try {
      const c = await resume(p);
      assert.equal(c.newParks().length, 2, "two reports at most");
      assert.equal(terminals().length, 0, "no failed report");
      assertNothingDone(c.counters);
    } finally { client.reportState = original; }
    await assertUntouched(p);
    assertRecord(p, { generation: 8, episode: 4, ack: "sent_unconfirmed" });
    // The server applied it at episode 4, the owner Resumed to 5: the chain continues.
    const next = await resume(p, { generation: 9, episode: 5 });
    assert.equal(next.newParks().length, 1);
    assert.equal(terminals().length, 0);
    assertRecord(p, { generation: 9, episode: 5, ack: "confirmed" });
  });

  it("a status-only acknowledgement (no accepted binding) is retried once and then stops: two reports, no failed report", async () => {
    const p = await predecessor("sent_unconfirmed");
    const c = await resume(p, { arm: () => {
      api.stateAckWire = (body, run) => parkBody({ body }) ? { status: 200, body: { run: { ...run, status: "running" } } } : undefined;
    } });
    assert.equal(c.newParks().length, 2);
    assert.equal(terminals().length, 0);
    assertNothingDone(c.counters);
    await assertUntouched(p);
    assertRecord(p, { generation: 8, episode: 4, ack: "sent_unconfirmed" });
  });

  for (const [name, runStatus] of [["a stale owner or generation", "running"], ["a checker-parent change (a parent-ineligible stale_claim)", "recovery_wait"]] as const) {
    it(`${name} stops nonterminal`, async () => {
      const p = await predecessor("sent_unconfirmed");
      const c = await resume(p, { arm: () => api.failStateWhen(p.f.claim.run_id, body => body.recovery_cause === "worker_memory_pressure",
        { disposition: "stale_claim", runStatus }) });
      assert.equal(c.counters.reports, 1, "one report: the server declined it without recording it");
      assert.equal(terminals().length, 0);
      assertNothingDone(c.counters);
      await assertUntouched(p);
      assertRecord(p, { generation: 8, episode: 4, ack: "sent_unconfirmed" });
    });
  }

  it("an exhausted wall budget (a wall-parked response) is retained nonterminal, with no failed report", async () => {
    const p = await predecessor("sent_unconfirmed");
    const c = await resume(p, { arm: () => api.failStateWhen(p.f.claim.run_id, body => body.recovery_cause === "worker_memory_pressure",
      { disposition: "stale_claim", runStatus: "paused", holdReason: "budget_exhausted" }) });
    assert.equal(c.counters.reports, 1, "one report: the server declined it without recording it");
    assert.equal(terminals().length, 0);
    assertNothingDone(c.counters);
    await assertUntouched(p);
    assertRecord(p, { generation: 8, episode: 4, ack: "sent_unconfirmed" });
  });

  for (const [name, status, generation] of [["owner Cancel", "cancelled", 8], ["owner Resume to a newer generation", "running", 9]] as const) {
    it(`${name} during the settlement stops nonterminal`, async () => {
      const p = await predecessor("sent_unconfirmed");
      const c = await resume(p, { arm: () => {
        api.stateAckWire = (body, run) => parkBody({ body }) ? { status: 200, body: { run: { ...run, status: "running" } } } : undefined;
        api.onState(p.f.claim.run_id, body => { if (parkBody({ body })) api.setOwnershipStatus(p.f.claim.run_id, status, generation); });
      } });
      assert.equal(c.newParks().length, 1, "no duplicate report once ownership moved");
      assert.equal(terminals().length, 0);
      assertNothingDone(c.counters);
      await assertUntouched(p);
      assertRecord(p, { generation: 8, episode: 4, ack: "sent_unconfirmed" });
    });
  }

  it("a credential switch signalled on the ACK stops without a failed report", async () => {
    const p = await predecessor("sent_unconfirmed");
    const c = await resume(p, { arm: () => api.armStateAckCredentialSwitchOnce(p.f.claim.run_id, 8, "recovery_wait") });
    assert.equal(c.counters.reports, 1, "the park report went out once");
    assert.equal(api.states.some(s => s.body.status === "failed"), false);
    const switches = api.states.filter(s => s.body.status === "credential_switch" || s.body.status === "credential_switch_failed");
    assert.equal(switches.length, 1, "the existing retained credential-switch protocol reported the switch");
    assert.equal(api.states.at(-1)!.body.status, switches[0]!.body.status, "and nothing else followed it");
    assertNothingDone(c.counters);
    await assertUntouched(p);
  });

  for (const [name, status, generation] of [["owner pause", "paused", 8], ["a newer claim fence", "running", 9]] as const) {
    // The pause/fence is armed before the resume, but the report is still sent: it is seen through the ownership read
    // after the report. This pins that outcome (nonterminal, no failed report), not a suppressed report.
    it(`${name} seen during the re-park is nonterminal with no failed report`, async () => {
      const p = await predecessor("sent_unconfirmed");
      const c = await resume(p, { arm: () => api.setOwnershipStatus(p.f.claim.run_id, status, generation) });
      assert.equal(c.counters.reports, 1, "the park report was sent before the ownership change was seen");
      assert.equal(terminals().length, 0);
      assertNothingDone(c.counters);
      await assertUntouched(p);
    });
  }

  it("the boundary deadline stops a park whose answer never arrives, nonterminal and recorded sent_unconfirmed", async () => {
    const p = await predecessor("sent_unconfirmed");
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    try {
      const c = await resume(p, { extra: { codexBoundaryDeadlineMs: 400 }, arm: () => api.afterPersistState(p.f.claim.run_id, parkBody2, gate) });
      assert.equal(c.newParks().length, 1);
      assert.equal(terminals().length, 0);
      assertNothingDone(c.counters);
    } finally { release(); }
    await assertUntouched(p);
    assertRecord(p, { generation: 8, episode: 4, ack: "sent_unconfirmed" });
  });
});

describe("the ack state decides which episodes may re-park (issue #3638 D4)", () => {
  it("not_sent re-parks only at the same episode (C === E)", async () => {
    const p = await predecessor("not_sent");
    const c = await resume(p, { generation: 8, episode: 3 });
    assert.equal(c.newParks().length, 1);
    assert.equal(terminals().length, 0);
    assertNothingDone(c.counters);
    assertRecord(p, { generation: 8, episode: 3, ack: "confirmed" });
  });

  it("not_sent at the next episode (C === E + 1) does not: today's path", async () => {
    const p = await predecessor("not_sent");
    assertToday(await resume(p, { generation: 8, episode: 4 }));
  });

  it("confirmed re-parks at the next episode and a later generation's record replaces the older one", async () => {
    const p = await predecessor("confirmed");
    const first = activeJournal()!.memoryPark!;
    const c = await resume(p, { generation: 8, episode: 4 });
    assert.equal(c.newParks().length, 1);
    assert.equal(terminals().length, 0);
    const second = activeJournal()!.memoryPark!;
    assert.notEqual(second.binding.claimGeneration, first.binding.claimGeneration, "the record was replaced");
    assertRecord(p, { generation: 8, episode: 4, ack: "confirmed" });
  });

  it("control: a confirmed ACK at the same episode is ineligible (the owner has not Resumed)", async () => {
    const p = await predecessor("confirmed");
    assertToday(await resume(p, { generation: 8, episode: 3 }));
  });
});

describe("every other refused continuation keeps today's behaviour (issue #3638 D4)", () => {
  it("control: a SIGKILLed active slot with no witness and no record", async () => {
    const p = await predecessor("sent_unconfirmed");
    rewriteJournal(j => { delete j.memoryPark; });
    assertToday(await resume(p));
  });

  it("no provenance in the claim's episode: an unrelated record of an older episode", async () => {
    const p = await predecessor("sent_unconfirmed");
    rewriteJournal(j => { j.memoryPark!.binding.memoryEpisode = 1; });
    assertToday(await resume(p));
  });

  it("a record of this or a later claim generation is no predecessor's", async () => {
    const p = await predecessor("sent_unconfirmed");
    rewriteJournal(j => { j.memoryPark!.binding.claimGeneration = 8; });
    assertToday(await resume(p, { generation: 8 }));
  });

  it("control: a post-Resume successor that minted its own epoch and then crashed, or any unrelated foreign writer (the slot owner is not the record's)", async () => {
    const p = await predecessor("sent_unconfirmed");
    rewriteJournal(j => { j.memoryPark!.origin.authorityIncarnation = randomUUID(); });
    assertToday(await resume(p));
  });

  it("a classification other than foreign_active or held_unended", async () => {
    const p = await predecessor("sent_unconfirmed");
    assertToday(await resume(p, { wrap: rt => { rt.classifySourceAdmission = async () => ({ kind: "other" }); } }));
  });

  it("auditor scenario: an unsettled park whose slot the same incarnation ended and re-admitted under a new epoch, then Resumed on another process, is not a re-park", async () => {
    const p = await predecessor("sent_unconfirmed");
    const parked = activeJournal()!.memoryPark!;
    assert.equal(parked.origin.sourceEpoch, p.slot.epoch, "the record binds the flight's own epoch");
    // A later flight of the same process ended the slot and admitted the clone again: same owner, a fresh epoch.
    const ended = await p.r.rt.endSourceUse([p.clone]);
    assert.equal(ended.ended.length, 1, "the earlier flight's source use ended");
    await p.r.rt.recordAdmissions(new Set(), () => p.r.rt.bindSource(p.clone));
    const slot = (await slotOf(p.r.rt, p.clone))!;
    assert.equal(slot.state, "active");
    assert.equal(slot.owner.worker, parked.origin.authorityIncarnation, "the same incarnation owns it");
    assert.notEqual(slot.epoch, parked.origin.sourceEpoch, "under a new epoch");
    // The record is carried by the later flight's writers, unchanged. The process then dies before adoption.
    assertToday(await resume({ ...p, slot }));
  });

  it("worker_memory_pressure_v1 not negotiated on the successor: today's path", async () => {
    const p = await predecessor("sent_unconfirmed");
    const hasFeature = client.hasFeature.bind(client);
    client.hasFeature = (feature: string) => feature !== "worker_memory_pressure_v1" && hasFeature(feature);
    try { assertToday(await resume(p)); } finally { client.hasFeature = hasFeature; }
  });

  it("the memory guard is absent on the successor", async () => {
    const p = await predecessor("sent_unconfirmed");
    assertToday(await resume(p, { noMemory: true }));
  });

  it("a settled record whose epoch is no longer the slot's: the slot was re-minted (the epoch rule)", async () => {
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
    const clone = worktreeDirFor(IID);
    assert.equal(activeJournal()?.memoryPark?.settlement?.confirmed, true, "a genuinely settled predecessor");
    // The same incarnation re-mints the slot afterwards (a new epoch that is active again).
    await r.rt.recordAdmissions(new Set(), () => r.rt.bindSource(clone));
    const slot = (await slotOf(r.rt, clone))!;
    assert.equal(slot.state, "active");
    const p: Predecessor = { f, r, clone, baseline: withoutPark(activeJournal()), slot };
    assertToday(await resume(p));
  });
});


describe("R1 policy reconciliation precedes memory continuation classification", () => {
  for (const heldSource of [false, true]) for (const cleared of [false, true])
    it(`${cleared ? "cleared" : "unresolved"} policy with ${heldSource ? "held source" : "memory parked predecessor"}`, async () => {
      const p = await predecessor("sent_unconfirmed", true);
      if (heldSource) rewriteJournal(j => { delete j.memoryPark; });
      const baseline = activeJournal();
      const outbox = new Outbox({ root: path.join(fx.dataDir, "policy-memory"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 128 * 1024 * 1024, retentionMs: 86400000 });
      await outbox.init();
      const original: PolicyHoldRequest = {
        outcome_id: "22222222-2222-4222-8222-222222222222", source_generation: 7, claim_generation: 7,
        detail: "refused", continuation_mode: "same_conversation", published: false,
        tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "b".repeat(32) + "-1",
      };
      await outbox.journalPolicyHold(p.f.claim.run_id, original);
      const packets: PolicyHoldRequest[] = [];
      client.getRunOwnership = async () => ({ status: "running", claim_generation: 8, claim_released: false });
      client.reportPolicyHold = async (_id, body) => {
        packets.push(body);
        return cleared && packets.length > 1
          ? { disposition: "already_applied", outcome_id: body.outcome_id, source_generation: 7,
              observed_status: "running", observed_generation: 8 }
          : { disposition: "pending", outcome_id: body.outcome_id, source_generation: 7 };
      };
      const statesBefore = api.states.length;
      const c = await resume(p, { policy: true, authority: "same", extra: { outbox } });
      assert.ok(packets.length > 1, "policy reconciliation ran after establishing custody, even for a held source");
      assert.deepEqual(packets.at(-1), {
        outcome_id: original.outcome_id, source_generation: 7, claim_generation: 8,
        detail: "resume_rejected", continuation_mode: "fresh_from_saved_work", published: false,
      }, "retained custody reconciles the original outcome under the current claim");
      assertNothingDone(c.counters);
      assert.equal(c.counters.reports, cleared && !heldSource ? 1 : 0);
      assert.equal(terminals().length, 0);
      assert.deepEqual((await outbox.readTerminalJournal(p.f.claim.run_id, 7))?.body, original);
      assert.equal(await outbox.hasPendingPolicyOutcome(p.f.claim.run_id), !cleared);
      assert.ok(c.teardown, "observe the production teardown's selected latches");
      assert.equal(c.teardown.policyHold, !cleared);
      assert.equal(c.teardown.policyAdmission, !cleared);
      assert.equal(c.teardown.memoryHold, false, "no competing memory preservation hold");
      assert.equal(c.teardown.parked, cleared && !heldSource);
      assert.deepEqual(api.states.slice(statesBefore).map(s => [s.body.status, s.body.recovery_cause]),
        cleared && !heldSource ? [["recovery_wait", "worker_memory_pressure"]] : [],
        "the only allowed new state write is one memory park after policy settlement");
      if (!cleared || heldSource) {
        assert.equal(c.teardown.repark, undefined, "unresolved policy or held source never enters the memory writer/latch");
        assert.deepEqual(activeJournal(), baseline, "including memoryPark and any repark.sent evidence");
      } else {
        assert.ok(c.teardown.repark);
        assertRecord(p, { generation: 8, episode: 4, ack: "confirmed" });
        assert.equal(withoutPark(activeJournal()), withoutPark(baseline));
      }
      assert.deepEqual(await slotOf(p.r.rt, p.clone), p.slot);
    });
});


it("policy feature loss sends nothing and retains memory predecessor protection", async () => {
  const p = await predecessor("sent_unconfirmed", true);
  const baseline = activeJournal();
  const outbox = new Outbox({ root: path.join(fx.dataDir, "policy-feature-loss"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 128 * 1024 * 1024, retentionMs: 86400000 });
  await outbox.init();
  const original: PolicyHoldRequest = {
    outcome_id: "22222222-2222-4222-8222-222222222222", source_generation: 7, claim_generation: 7,
    detail: "refused", continuation_mode: "same_conversation", published: false,
    tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "b".repeat(32) + "-1",
  };
  await outbox.journalPolicyHold(p.f.claim.run_id, original);
  let sends = 0;
  client.reportPolicyHold = async () => { sends++; throw new Error("feature loss must suspend delivery"); };
  await resume(p, { policy: true, authority: "same", extra: { outbox }, arm: () => {
    client.protocolFeatures = client.protocolFeatures.filter(feature => feature !== "provider_policy_hold_v1");
    assert.equal(client.supportsPolicyHold, false);
  } });
  assert.equal(sends, 0);
  assert.equal(terminals().length, 0);
  assert.deepEqual((await outbox.readTerminalJournal(p.f.claim.run_id, 7))?.body, original);
  assert.equal(await outbox.hasPendingPolicyOutcome(p.f.claim.run_id), true);
  assert.deepEqual(activeJournal(), baseline);
  assert.deepEqual(await slotOf(p.r.rt, p.clone), p.slot);
});
