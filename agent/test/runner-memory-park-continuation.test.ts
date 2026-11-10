import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { describe, it } from "node:test";
import type { Logger } from "../src/log.js";
import { performance } from "node:perf_hooks";
import { acquireWorkerLifetime, withAuthorityTransaction } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import { RequestError, WorkerClient, isTransient } from "../src/client.js";
import {
  ForeignRetainedRecoveryError, MemoryReceiptSourceError, RetainedRecoveryBlockedError, parseMemoryPark, serializeMemoryPark,
  type MemoryParkRecord,
} from "../src/git.js";
import type { Executor } from "../src/executor.js";
import type { ClaimResponse, MemoryParkReference, MemoryReservationRequest, StateRequest } from "../src/protocol.js";
import type { RunRunner } from "../src/runner.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

installHarness();

// issue #3639 - the worker side of the settled memory-park receipt: the producer posts the confirmed journal bytes, the
// run's own-park entry records what was posted, and a later claim of the same worker incarnation that carries the
// server's reference passes a gate and a handoff before it continues on the retained source. Everything else (no
// reference, a different one, a source that changed, a guard that never sampled) re-parks for the owner, nonterminally.
// Driven through the real RunRunner.execute() with the production AuthorityRuntime and the memory runtime; ONE runner
// executes both the parked claim and the owner's Resume, because the own-park entry lives in the runner.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");
const IID = 2396;
const BRANCH = `agent/issue-${IID}`;
const NONCE = "original-memory-registration";
const FEATURES = ["worker_memory_pressure_v1", "claim_generation_fence", "memory_park_settlement_v1"];
const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };
const timing = { rearmMs: settings.rearmMs, sampleMs: settings.sampleMs, responseBudgetMs: settings.responseBudgetMs };

/** The cgroup usage the memory runtime's sampler reports; the default 0 never presses. */
let usage = 0;
function memoryRuntime(): WorkerMemoryRuntime {
  let sampleId = 0;
  return new WorkerMemoryRuntime(settings, client, nullLogger(), {
    reader: { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: ++sampleId, startedAtMs: time, completedAtMs: time,
        currentBytes: usage, limitBytes: 1000,
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0, inactive_file: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
          full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
}

async function setup(features = FEATURES, count = 2) {
  usage = 0;
  api.memoryRegisterNonce = NONCE;
  api.setRegisterProtocolFeatures(features);
  await client.register("memory-fixture", "base");
  const claim: ClaimResponse = { ...gitlabClaim(IID), claim_generation: 7,
    memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: count };
  api.setOwnershipStatus(claim.run_id, "running", 7);
  let used = count;
  const reservations: MemoryReservationRequest[] = [];
  const accepted = new Map<string, number>();
  client.reserveMemoryIntervention = async (request: MemoryReservationRequest) => {
    reservations.push(request);
    accepted.set(request.intervention_id, ++used);
    return { ...request, admitted: true, authorizing: true, allowance: { limit: 2, used, remaining: 2 - used } };
  };
  client.reportMemoryInterventionOutcome = async request => {
    const spent = accepted.get(request.intervention_id)!;
    return { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: spent, remaining: 2 - spent } };
  };
  return { memory: memoryRuntime(), claim, workerId: client.memoryIncarnation!.worker_id, reservations };
}

async function authorityRig(extra: Partial<ConstructorParameters<typeof AuthorityRuntime>[0]> = {}) {
  const dataDir = fs.realpathSync(fx.dataDir);
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000, ...extra });
  return {
    rt,
    lifetime,
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
function rawJournal(): string {
  try { return execFileSync("git", ["-C", bareOf(), "config", "--local", "--get", journalKey], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim(); }
  catch { return ""; }
}
const activeJournal = (): Journal | undefined => { const raw = rawJournal(); return raw ? JSON.parse(raw) as Journal : undefined; };
function dropJournal(): void {
  execFileSync("git", ["-C", bareOf(), "config", "--local", "--unset-all", journalKey], { env: GIT_ENV, stdio: "pipe" });
}
const sha256 = (text: string): string => createHash("sha256").update(text, "utf8").digest("hex");

const parkBody = (body: StateRequest): boolean => body.recovery_cause === "worker_memory_pressure";
const parks = () => api.states.filter(s => parkBody(s.body));
const terminals = () => api.states.filter(s => s.body.status === "failed" || s.body.status === "completed");
/** A status-only acknowledgement: the server answers 200 but carries no accepted binding. */
const declinePark = (): void => {
  api.stateAckWire = (body, run) => parkBody(body) ? { status: 200, body: { run: { ...run, status: "running" } } } : undefined;
};

type Post = { runId: string; bytes: string };
type PostBehaviour = (attempt: number, signal: AbortSignal | undefined) => Promise<void>;
const accept: PostBehaviour = async () => {};
const refusedWith = (status: number): PostBehaviour => async () => { throw new RequestError("POST", "/memory/settlement", status, ""); };
const lostAck: PostBehaviour = async () => { throw new TypeError("fetch failed"); };
const neverAnswers: PostBehaviour = (_attempt, signal) => new Promise<void>((_resolve, reject) => {
  if (signal?.aborted) { reject(new Error("aborted")); return; }
  signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
});

type Entry = { state: "pending" | "consumed"; binding: Record<string, unknown>; bytes?: string; sha?: string;
  clonePath?: string; attemptId?: string | null };
type Internals = {
  parkMemoryGateRefused(...args: unknown[]): Promise<never>;
  captureRecoveryRestorePoint(...args: unknown[]): Promise<unknown>;
  captureWorkerMemory(...args: unknown[]): unknown;
  admitPredecessorSource(...args: unknown[]): Promise<unknown>;
  preserveWorkerMemory(...args: unknown[]): Promise<void>;
  memoryHoldSettled(...args: unknown[]): boolean;
  runCancellationMaintenance(flight: unknown, fn: () => Promise<unknown>, ...rest: unknown[]): Promise<unknown>;
  ownMemoryParks: Map<string, Entry>;
  createOwnMemoryPark(runId: string, binding: Record<string, unknown>): Entry | undefined;
  memoryReceiptDecision(claim: unknown): { kind: string; reason?: string };
  memoryReceiptGate(claim: unknown, flight: unknown): Promise<void>;
};

type Counters = { refusals: Array<{ kind: string; reason: string }>; reserve: number; block: number; capture: number;
  captureMemory: number; admit: number; preserve: number; ran: number; clone: number; owed: number };

interface Parked {
  f: Awaited<ReturnType<typeof setup>>;
  r: Awaited<ReturnType<typeof authorityRig>>;
  runner: RunRunner;
  inner: Internals;
  posts: Post[];
  clone: string;
  counters: Counters;
  /** The active journal key and the clone's slot exactly as the parked claim left them. */
  journal0: string;
  slot: SlotSnapshot;
  successor: Executor;
  setExecutor(executor: Executor): void;
  /** The reference the server would send: it names the PARKED generation and the sha256 of the posted bytes. */
  ref(patch?: Partial<MemoryParkReference>): MemoryParkReference;
  done(): Promise<void>;
}

/** The parked claim: generation 7, episode 3, exhausted allowance. Its accepted, settled park posts its receipt. */
async function parked(o: {
  post?: PostBehaviour; features?: string[]; boundaryMs?: number; endKept?: boolean; attempt?: boolean;
  tweak?: (inner: Internals, r: Awaited<ReturnType<typeof authorityRig>>) => void;
  authority?: Parameters<typeof authorityRig>[0];
  count?: number;
  /** The predecessor's model turn, built from the fixture's memory runtime; default stops at the exhausted allowance. */
  predecessor?: (f: Awaited<ReturnType<typeof setup>>) => Executor;
} = {}): Promise<Parked> {
  const f = await setup(o.features, o.count);
  const r = await authorityRig(o.authority);
  fs.writeFileSync(homeDir + "/session-proof", "original session");
  git.worktreeStatus = async () => [];
  const posts: Post[] = [];
  let attempts = 0;
  client.reportMemoryParkSettlement = async (runId: string, bytes: string, signal?: AbortSignal) => {
    posts.push({ runId, bytes });
    await (o.post ?? accept)(attempts++, signal);
  };
  if (o.endKept) r.rt.endSourceUse = async (paths) => ({ ended: [], kept: [...paths], endedSlots: [] });
  const monitoring = f.memory.run();
  let current: Executor = o.predecessor?.(f) ?? {
    run: async ctx => {
      api.setOwnershipStatus(f.claim.run_id, "running", 7);
      assert.throws(() => ctx.capturedMemory!(), /retained recovery/i);
      throw new Error("fixture stops original model execution");
    },
  };
  const runner = runnerWith(() => ({ executor: current, homeDir }), fakeGitlab().gitlab, undefined, undefined,
    { memoryRuntime: f.memory, memoryGuardTiming: timing, codexBoundaryDeadlineMs: o.boundaryMs ?? 5000, authority: r.rt });
  // An attempt-path worker (Docker-wired) seeds an attempt clone, so the receipt names an attempt id.
  if (o.attempt) Object.assign(runner, { attemptPaths: true });
  const inner = runner as unknown as Internals;
  o.tweak?.(inner, r);
  try { await runner.execute(f.claim); } catch { /* the fixture's stop is not a test failure */ }
  const clone = worktreeDirFor(IID);
  const counters: Counters = { refusals: [], reserve: 0, block: 0, capture: 0, captureMemory: 0, admit: 0, preserve: 0, ran: 0, clone: 0, owed: 0 };
  const spy = <T extends object>(target: T, name: string, bump: () => void): void => {
    const bag = target as unknown as Record<string, (...args: unknown[]) => unknown>;
    const original = bag[name]!.bind(target);
    bag[name] = (...args: unknown[]) => { bump(); return original(...args); };
  };
  // Counted from here on: the Resume's work. The parked claim's own is not part of any assertion.
  const refuse = inner.parkMemoryGateRefused.bind(runner) as (...a: unknown[]) => Promise<never>;
  inner.parkMemoryGateRefused = (claim, flight, kind, reason) => {
    counters.refusals.push({ kind: String(kind), reason: String(reason) });
    return refuse(claim, flight, kind, reason);
  };
  spy(git, "reserveRecoveryIteration", () => counters.reserve++);
  spy(git, "blockRecoveryEpisode", () => counters.block++);
  spy(git, "ensureClone", () => counters.clone++);
  spy(git, "retainCurrentOwedCandidate", () => counters.owed++);
  spy(inner, "captureRecoveryRestorePoint", () => counters.capture++);
  spy(inner, "captureWorkerMemory", () => counters.captureMemory++);
  spy(inner, "admitPredecessorSource", () => counters.admit++);
  spy(inner, "preserveWorkerMemory", () => counters.preserve++);
  const successor: Executor = { run: async () => { counters.ran++; throw new Error("the successor stops here"); } };
  const p: Parked = {
    f, r, runner, inner, posts, clone, counters, successor,
    journal0: rawJournal(), slot: (await slotOf(r.rt, clone))!,
    setExecutor: executor => { current = executor; },
    ref: patch => ({ claim_generation: 7, memory_episode: 3, worker_id: f.workerId, register_nonce: NONCE,
      settlement_sha256: sha256(posts[0]?.bytes ?? ""), ...patch }),
    done: async () => { await f.memory.stop(); await monitoring; },
  };
  return p;
}

/** The owner's Resume on the SAME worker incarnation: a later generation and the next memory episode, carrying `ref`. */
async function resume(p: Parked, o: { generation?: number; episode?: number; ref?: MemoryParkReference | null;
  policy?: { version: 1; max_interventions: number } } = {}) {
  const generation = o.generation ?? 8;
  const ref = o.ref === undefined ? p.ref() : o.ref;
  const claim: ClaimResponse = { ...p.f.claim, claim_generation: generation, memory_episode: o.episode ?? 4,
    memory_policy: o.policy ?? null, memory_intervention_count: 0, ...(ref ? { memory_park_reference: ref } : {}) };
  api.setOwnershipStatus(claim.run_id, "running", generation);
  p.setExecutor(p.successor);
  const before = parks().length;
  try { await p.runner.execute(claim); } catch { /* a stopped flight may reject; the assertions read its effects */ }
  return { claim, newParks: () => parks().slice(before) };
}

const entryOf = (p: Parked): Entry | undefined => p.inner.ownMemoryParks.get(p.f.claim.run_id);

/** The slot and journal the parked claim left behind are exactly as they were: nothing admitted, ended or rewritten. */
async function assertUntouched(p: Parked): Promise<void> {
  assert.deepEqual(await slotOf(p.r.rt, p.clone), p.slot, "the settled slot is unchanged");
  assert.equal(rawJournal(), p.journal0, "the journal is byte-identical");
  assert.equal(fs.existsSync(p.clone), true, "the clone is retained");
}

/** A refused receipt claim did no work of any kind and failed nothing. */
function assertNothingDone(p: Parked): void {
  const { refusals: _refusals, ...work } = p.counters;
  void _refusals;
  assert.deepEqual(work, { reserve: 0, block: 0, capture: 0, captureMemory: 0, admit: 0, preserve: 0, ran: 0, clone: 0, owed: 0 },
    "no reservation, recovery-episode charge, capture, memory context, admission, preservation, fresh clone, owed candidate or model turn");
  assert.equal(terminals().length, 0, "no terminal report");
}

function assertRefusal(p: Parked, kind: "F2" | "F3", reason: RegExp): void {
  assert.equal(p.counters.refusals.length, 1, `exactly one receipt refusal: ${JSON.stringify(p.counters.refusals)}`);
  assert.equal(p.counters.refusals[0]!.kind, kind);
  assert.match(p.counters.refusals[0]!.reason, reason);
}

/** Replace the guard's observation so that no fresh valid sample ever arrives; `during` runs inside the wait. */
function starveGuard(memory: WorkerMemoryRuntime, outcome: "timeout" | "aborted", during?: () => Promise<void>): void {
  memory.guardObservation = () => ({ seq: 1, validSeq: 0, armed: true, pendingIntervention: false });
  memory.waitGuardSample = async () => { await during?.(); return outcome; };
}

describe("the producer posts the settled park's exact journal bytes (issue #3639)", () => {
  it("posts serializeMemoryPark of the journal's confirmed record exactly once, then keeps the entry pending", async () => {
    const p = await parked();
    try {
      const journal = activeJournal()!;
      assert.equal(journal.memoryPark?.settlement?.confirmed, true, "premise: the park settled");
      assert.equal(p.posts.length, 1, "one request");
      assert.equal(p.posts[0]!.runId, p.f.claim.run_id);
      assert.equal(p.posts[0]!.bytes, serializeMemoryPark(journal.memoryPark!), "the exact bytes of the confirmed journal record");
      assert.equal(JSON.parse(p.posts[0]!.bytes).settlement.confirmed, true);
      const entry = entryOf(p)!;
      assert.equal(entry.state, "pending", "no response consumes it");
      assert.deepEqual([entry.bytes, entry.sha, entry.clonePath, entry.attemptId],
        [p.posts[0]!.bytes, sha256(p.posts[0]!.bytes), p.clone, journal.attemptId ?? null],
        "bytes, S, clone path and attempt id are stored with it");
      assert.equal(parks().length, 1, "the parked claim parked once");
    } finally { await p.done(); }
  });

  for (const fixture of ["settlement-typical.json", "settlement-max-shape.json"]) {
    it(`serializeMemoryPark(parseMemoryPark(${fixture})) is the fixture's bytes, and its sha256 file matches`, () => {
      const dir = path.resolve(import.meta.dirname, "../../fixtures/memory-park");
      const text = fs.readFileSync(path.join(dir, fixture), "utf8");
      assert.equal(serializeMemoryPark(parseMemoryPark(JSON.parse(text))), text);
      assert.equal(fs.readFileSync(path.join(dir, `${fixture}.sha256`), "utf8").trim(), sha256(text));
      assert.ok(Buffer.byteLength(text) <= 524288, "within the server's cap");
    });
  }

  // Each trap leaves the park unsettled; none may post, and none may leave an entry behind.
  const traps: Array<[string, Parameters<typeof parked>[0]]> = [
    ["settled set in a finally (the preservation threw)", { tweak: inner => {
      inner.preserveWorkerMemory = async () => { throw new Error("injected preservation failure"); };
    } }],
    ["the hold cleared by hand with a required join unresolved", { tweak: inner => {
      let tampered = false;
      const maintenance = inner.runCancellationMaintenance.bind(inner);
      inner.runCancellationMaintenance = (flight, fn, ...rest) => {
        const memory = (flight as { workerMemory?: { hold?: unknown; parkWitness?: unknown; feedback: Map<string, unknown> }; parked?: boolean }).workerMemory;
        if (!tampered && memory?.parkWitness && (flight as { parked?: boolean }).parked) {
          tampered = true;
          memory.hold = undefined;
          memory.feedback.set("outstanding-feedback", {});
        }
        return maintenance(flight, fn, ...rest);
      };
    } }],
    ["endSourceUse kept the slots (not ended under its lock)", { endKept: true }],
    ["sourceTailsJoined alone: the hold is not settled", { tweak: inner => { inner.memoryHoldSettled = () => false; } }],
  ];
  for (const [name, options] of traps) {
    it(`no POST and no entry: ${name}`, async () => {
      const p = await parked(options);
      try {
        assert.equal(p.posts.length, 0);
        assert.equal(activeJournal()?.memoryPark?.settlement?.confirmed === true, false, "no settlement was confirmed");
        assert.equal(entryOf(p), undefined, "an entry that never reached a POST is removed");
      } finally { await p.done(); }
    });
  }

  // A settlement IS built and the journal really holds it, but the producer's own write call did not report `settled`
  // for this hold: the POST is gated on that return value alone. (The `hold &&` term of the delivery condition is
  // redundant: `record.settlement` is only ever assigned in the branch that has a hold, so with no hold the condition is
  // already false at `record.settlement`; a cleared hold cannot reach it, and the re-park test below pins the no-POST.)
  for (const outcome of ["unconfirmed", "recorded"] as const) {
    it(`no POST and no entry: the journal write returned ${outcome} although a settlement was built`, async () => {
      const p = await parked({ tweak: () => {
        const record = git.recordMemoryPark.bind(git);
        git.recordMemoryPark = async (...args: Parameters<typeof record>) => { await record(...args); return outcome; };
      } });
      try {
        assert.equal(activeJournal()?.memoryPark?.settlement?.confirmed, true, "premise: the settlement was built and written");
        assert.equal(p.posts.length, 0);
        assert.equal(entryOf(p), undefined, "an entry that never reached a POST is removed");
      } finally { await p.done(); }
    });
  }

  it("a feature the server did not negotiate: no POST, entry removed, the journal still settles", async () => {
    const p = await parked({ features: ["worker_memory_pressure_v1", "claim_generation_fence"] });
    try {
      assert.equal(activeJournal()?.memoryPark?.settlement?.confirmed, true);
      assert.equal(p.posts.length, 0);
      assert.equal(entryOf(p), undefined);
    } finally { await p.done(); }
  });

  it("bytes over the server's cap: no POST, entry removed", async () => {
    const realByteLength = Buffer.byteLength;
    const hasFeature = client.hasFeature.bind(client);
    // The delivery serializes and measures synchronously right after it asks for the feature: inflate that one measurement.
    client.hasFeature = (feature: string) => {
      if (feature === "memory_park_settlement_v1") {
        Buffer.byteLength = ((value: unknown, encoding?: BufferEncoding) =>
          typeof value === "string" && value.startsWith('{"version":1,"binding"') ? 600_000 : realByteLength(value as string, encoding)) as typeof Buffer.byteLength;
        setImmediate(() => { Buffer.byteLength = realByteLength; });
      }
      return hasFeature(feature);
    };
    let p: Parked | undefined;
    try {
      p = await parked();
      assert.equal(activeJournal()?.memoryPark?.settlement?.confirmed, true, "premise: the park settled");
      assert.equal(p.posts.length, 0);
      assert.equal(entryOf(p), undefined);
    } finally { Buffer.byteLength = realByteLength; client.hasFeature = hasFeature; await p?.done(); }
  });

  // Delivery bounds: transient failures earn the second attempt, definite ones do not, and no outcome touches the entry.
  const outcomes: Array<[string, PostBehaviour, number]> = [
    ["400", refusedWith(400), 1], ["404", refusedWith(404), 1], ["409", refusedWith(409), 1], ["413", refusedWith(413), 1],
    ["500 (transient, twice)", refusedWith(500), 2], ["a lost answer (transport error, twice)", lostAck, 2],
  ];
  for (const [name, post, attempts] of outcomes) {
    it(`${name}: ${attempts} attempt(s) at most, never throws, the entry stays pending`, async () => {
      const p = await parked({ post });
      try {
        assert.equal(p.posts.length, attempts);
        assert.equal(entryOf(p)?.state, "pending");
        assert.equal(entryOf(p)?.sha, sha256(p.posts[0]!.bytes));
        assert.equal(terminals().length, 0, "the parked claim still ends nonterminally");
      } finally { await p.done(); }
    });
  }

  it("a transient failure then success: delivered on the second attempt", async () => {
    const p = await parked({ post: async attempt => { if (attempt === 0) throw new TypeError("fetch failed"); } });
    try {
      assert.equal(p.posts.length, 2);
      assert.equal(p.posts[1]!.bytes, p.posts[0]!.bytes, "the identical bytes are replayed");
      assert.equal(entryOf(p)?.state, "pending", "even a delivered 200 consumes nothing");
    } finally { await p.done(); }
  });

  it("an unanswered request is bounded by the fresh boundary deadline", async () => {
    // Exercise delivery independently of the park and journal deadlines. Under load the
    // 1500 ms park can expire before it writes a settlement, leaving no POST to bound.
    const f = await setup();
    const runner = runnerWith(() => { throw new Error("delivery test starts no executor"); },
      fakeGitlab().gitlab, undefined, undefined, { codexBoundaryDeadlineMs: 1500 });
    const inner = runner as unknown as Internals & {
      deliverMemoryParkSettlement(flight: unknown, log: Logger, entry: Entry, record: MemoryParkRecord,
        clonePath: string, attemptId: string | undefined): Promise<boolean>;
    };
    const binding = { runId: f.claim.run_id, claimGeneration: 7, workerId: f.workerId,
      registerNonce: NONCE, memoryEpisode: 3 };
    const entry: Entry = { state: "pending", binding };
    inner.ownMemoryParks.set(binding.runId, entry);
    const clonePath = worktreeDirFor(IID);
    const confirmed: MemoryParkRecord = {
      version: 1, binding, ack: "confirmed",
      origin: { flightId: randomUUID(), attemptId: null, executorIncarnation: null,
        authorityIncarnation: randomUUID(), sourceEpoch: randomUUID() },
      settlement: { version: 1, confirmed: true, sources: [{ path: clonePath,
        authorityKey: sha256(clonePath), attemptId: null, epoch: randomUUID(), seq: 1 }],
        evidence: { parkAck: binding, interventionIds: [], processJoin: "executor",
          commands: "not_required", steering: "joined", auxiliaries: "none",
          feedback: "drained", sinkReceipts: "joined", parkReap: "not_applicable" } },
    };
    const posts: Post[] = [];
    client.reportMemoryParkSettlement = async (runId, bytes, signal) => {
      posts.push({ runId, bytes });
      await neverAnswers(posts.length - 1, signal);
    };
    const started = Date.now();
    assert.equal(await inner.deliverMemoryParkSettlement({ runId: binding.runId }, nullLogger(),
      entry, confirmed, clonePath, undefined), true, "delivery attempts the request");
    assert.equal(posts.length, 1, "the abort ends the loop: no second attempt after the deadline");
    assert.ok(Date.now() - started < 60_000);
    assert.equal(entry.state, "pending");
    assert.equal(entry.sha, sha256(posts[0]!.bytes), "the attempted bytes remain pinned");
  });
});

describe("the receipt gate and handoff (issue #3639)", () => {
  it("pending entry + the matching reference: gate and handoff pass, the retained work continues, the entry is consumed", async () => {
    const p = await parked();
    try {
      assert.equal(entryOf(p)?.state, "pending");
      const c = await resume(p);
      assert.equal(p.counters.refusals.length, 0, "no refusal");
      assert.equal(c.newParks().length, 0, "no fallback park");
      assert.equal(p.counters.ran, 1, "the successor executor ran on the retained source");
      assert.equal(entryOf(p)?.state, "consumed", "consumed only by the complete handoff");
    } finally { await p.done(); }
  });

  it("an attempt-path receipt names its attempt id: discovery is bound to it and the handoff passes", async () => {
    const p = await parked({ attempt: true });
    try {
      const attemptId = activeJournal()!.attemptId;
      assert.match(attemptId ?? "", /^\d{8}T\d{6}Z-g7-[0-9a-f]{16}$/, "premise: the parked claim seeded an attempt clone");
      assert.equal(entryOf(p)?.attemptId, attemptId);
      const calls: unknown[][] = [];
      const discover = git.discoverRetainedRecovery.bind(git);
      git.discoverRetainedRecovery = async (...args: Parameters<typeof discover>) => { calls.push(args); return discover(...args); };
      await resume(p);
      assert.equal((calls[0]![4] as { attemptId: string }).attemptId, attemptId);
      assert.equal(p.counters.refusals.length, 0);
      assert.equal(p.counters.ran, 1);
      assert.equal(entryOf(p)?.state, "consumed");
    } finally { await p.done(); }
  });

  it("a canonical-clone receipt names no attempt id: discovery expects none and the gate and handoff pass to CONSUMED", async () => {
    const p = await parked();
    try {
      assert.equal(activeJournal()!.attemptId, undefined, "premise: the parked clone is canonical");
      assert.equal(entryOf(p)?.attemptId, null);
      const calls: unknown[][] = [];
      const discover = git.discoverRetainedRecovery.bind(git);
      git.discoverRetainedRecovery = async (...args: Parameters<typeof discover>) => { calls.push(args); return discover(...args); };
      await resume(p);
      assert.equal("attemptId" in (calls[0]![4] as object), false, "the expected binding carries no attemptId");
      assert.equal(p.counters.refusals.length, 0);
      assert.equal(p.counters.ran, 1);
      assert.equal(entryOf(p)?.state, "consumed");
    } finally { await p.done(); }
  });

  it("R8: consumed entry: a claim with no reference is ordinary and keeps the entry; repeating the reference is ordinary too", async () => {
    const p = await parked();
    try {
      await resume(p);
      assert.equal(entryOf(p)?.state, "consumed");
      const before = p.counters.refusals.length;
      await resume(p, { generation: 9, episode: 5, ref: null });
      assert.equal(entryOf(p)?.state, "consumed", "kept");
      await resume(p, { generation: 10, episode: 6 });
      assert.equal(p.counters.refusals.length, before, "neither claim was refused");
      assert.equal(entryOf(p)?.state, "consumed");
      await resume(p, { generation: 11, episode: 7, ref: p.ref({ settlement_sha256: sha256("another park") }) });
      assert.equal(p.counters.refusals.at(-1)?.kind, "F2", "a different reference of a consumed entry is refused");
    } finally { await p.done(); }
  });

  it("pending entry + no reference: F2, nothing admitted, nothing written, the entry stays pending until a park is accepted", async () => {
    const p = await parked();
    try {
      declinePark();
      const c = await resume(p, { ref: null });
      assertRefusal(p, "F2", /no reference/);
      assert.equal(c.newParks().length >= 1, true, "a park report was sent");
      assertNothingDone(p);
      await assertUntouched(p);
      assert.equal(entryOf(p)?.state, "pending", "an unaccepted fallback park consumes nothing");
    } finally { await p.done(); }
  });

  it("an accepted fallback park at the claim's own binding removes the entry", async () => {
    const p = await parked();
    try {
      const c = await resume(p, { ref: null });
      assertRefusal(p, "F2", /no reference/);
      assert.equal(c.newParks().length, 1);
      assert.deepEqual([c.newParks()[0]!.body.claim_generation, c.newParks()[0]!.body.memory_episode], [8, 4]);
      assert.equal(entryOf(p), undefined, "the newer park supersedes the receipt");
      assertNothingDone(p);
      await assertUntouched(p);
      assert.equal(activeJournal()?.memoryPark?.binding.claimGeneration, 7, "no provenance record: F2 writes no journal");
    } finally { await p.done(); }
  });

  for (const [name, patch] of [
    ["a different settlement sha", { settlement_sha256: sha256("not the posted bytes") }],
    ["another worker id", { worker_id: randomUUID() }],
    ["another registration nonce", { register_nonce: "foreign-registration" }],
    ["another parked generation", { claim_generation: 6 }],
    ["another parked episode", { memory_episode: 2 }],
  ] as const) {
    it(`pending entry + a different reference (${name}): F2`, async () => {
      const p = await parked();
      try {
        declinePark();
        await resume(p, { ref: p.ref(patch) });
        assertRefusal(p, "F2", /differs/);
        assertNothingDone(p);
        await assertUntouched(p);
        assert.equal(entryOf(p)?.state, "pending");
      } finally { await p.done(); }
    });
  }

  it("no entry + a reference: F2", async () => {
    const f = await setup();
    const r = await authorityRig();
    git.worktreeStatus = async () => [];
    let ran = 0;
    const runner = runnerWith(() => ({ executor: { run: async () => { ran++; throw new Error("no model"); } }, homeDir }),
      fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory, memoryGuardTiming: timing, codexBoundaryDeadlineMs: 5000, authority: r.rt });
    const refusals: string[] = [];
    const inner = runner as unknown as Internals;
    const refuse = inner.parkMemoryGateRefused.bind(runner) as (...a: unknown[]) => Promise<never>;
    inner.parkMemoryGateRefused = (claim, flight, kind, reason) => { refusals.push(`${String(kind)}: ${String(reason)}`); return refuse(claim, flight, kind, reason); };
    const claim: ClaimResponse = { ...f.claim, claim_generation: 8, memory_episode: 4, memory_policy: null, memory_intervention_count: 0,
      memory_park_reference: { claim_generation: 7, memory_episode: 3, worker_id: f.workerId, register_nonce: NONCE, settlement_sha256: sha256("x") } };
    api.setOwnershipStatus(claim.run_id, "running", 8);
    const monitoring = f.memory.run();
    try { await runner.execute(claim); } catch { /* nonterminal stop */ }
    await f.memory.stop(); await monitoring;
    assert.equal(refusals.length, 1);
    assert.match(refusals[0]!, /^F2: .*no record/);
    assert.equal(ran, 0);
    assert.equal(parks().length, 1, "it re-parked for the owner");
    assert.equal(terminals().length, 0);
  });

  it("a same-nonce old-generation reference is refused once a newer park replaced the entry", async () => {
    const p = await parked();
    try {
      const old = p.ref();
      const newer = { ...(entryOf(p)!.binding), claim_generation: 9, memory_episode: 5 };
      p.inner.createOwnMemoryPark(p.f.claim.run_id, newer);
      assert.equal(p.inner.ownMemoryParks.size, 1, "the newer accepted park replaced the entry");
      const decision = p.inner.memoryReceiptDecision({ run_id: p.f.claim.run_id, memory_park_reference: old });
      assert.equal(decision.kind, "refuse");
    } finally { await p.done(); }
  });
});

describe("gate steps (a) to (d) (issue #3639)", () => {
  for (const [name, generation, episode] of [
    ["the claim is not a later generation than the parked one", 7, 4],
    ["the claim's episode is not the parked episode + 1 (same)", 8, 3],
    ["the claim's episode is not the parked episode + 1 (skipped)", 8, 5],
  ] as const) {
    it(`(a) ${name}: F2`, async () => {
      const p = await parked();
      try {
        declinePark();
        await resume(p, { generation, episode });
        assertRefusal(p, "F2", /next episode/);
        assertNothingDone(p);
        await assertUntouched(p);
        assert.equal(entryOf(p)?.state, "pending");
      } finally { await p.done(); }
    });
  }

  it("(a) the receipt's authority incarnation is not this process's: F2", async () => {
    const p = await parked();
    try {
      declinePark();
      p.r.rt.authorityIncarnation = () => randomUUID();
      await resume(p);
      assertRefusal(p, "F2", /another authority incarnation/);
      assertNothingDone(p);
      await assertUntouched(p);
    } finally { await p.done(); }
  });

  it("(a) the journal no longer confirms the receipt (absent): F2", async () => {
    const p = await parked();
    try {
      declinePark();
      dropJournal();
      await resume(p);
      assertRefusal(p, "F2", /no longer confirms/);
      assertNothingDone(p);
      assert.deepEqual(await slotOf(p.r.rt, p.clone), p.slot);
      assert.equal(rawJournal(), "", "nothing was written back");
      assert.equal(entryOf(p)?.state, "pending");
    } finally { await p.done(); }
  });

  it("(a) the journal's record differs from the receipt (changed): F2", async () => {
    const p = await parked();
    try {
      declinePark();
      const journal = activeJournal()!;
      journal.memoryPark!.settlement!.evidence.interventionIds = ["tampered"];
      execFileSync("git", ["-C", bareOf(), "config", "--local", journalKey, JSON.stringify(journal)], { env: GIT_ENV, stdio: "pipe" });
      const tampered = rawJournal();
      await resume(p);
      assertRefusal(p, "F2", /no longer confirms/);
      assertNothingDone(p);
      assert.equal(rawJournal(), tampered, "the tampered journal is left exactly as found");
    } finally { await p.done(); }
  });

  for (const [name, attempt, stored] of [
    ["an attempt-path receipt whose entry names no attempt", true, null],
    ["an attempt-path receipt whose entry names another attempt", true, "20200101T000000Z-g1-0000000000000000"],
    ["a canonical receipt whose entry names an attempt", false, "20200101T000000Z-g1-0000000000000000"],
  ] as const) {
    it(`(a) ${name}: F2`, async () => {
      const p = await parked({ attempt });
      try {
        declinePark();
        assert.notEqual(entryOf(p)!.attemptId, stored, "premise: the tampered value differs from the record's");
        entryOf(p)!.attemptId = stored;
        await resume(p);
        assertRefusal(p, "F2", /another attempt/);
        assertNothingDone(p);
        assert.equal(rawJournal(), p.journal0, "the journal is byte-identical");
        assert.equal(entryOf(p)?.state, "pending");
      } finally { await p.done(); }
    });
  }

  it("(a) no memory runtime on the claimant: F2", async () => {
    const p = await parked();
    try {
      declinePark();
      // A flight with no worker-memory binding cannot prove a receipt.
      (p.runner as unknown as { memoryRuntime: unknown }).memoryRuntime = undefined;
      await resume(p);
      assertRefusal(p, "F2", /no memory runtime/);
      assertNothingDone(p);
    } finally { await p.done(); }
  });

  it("(b) a settled slot changes while the guard is waited on: F3, journal and authority bytes identical, no owed candidate", async () => {
    const p = await parked();
    try {
      declinePark();
      const intruder = p.r.successor();
      starveGuard(p.f.memory, "timeout", async () => {
        await intruder.withTransaction(() => intruder.hooks.admitSourceUse(p.clone, "seed-canonical"));
      });
      await resume(p);
      assertRefusal(p, "F3", /changed while the guard was sampled/);
      assertNothingDone(p);
      assert.equal(rawJournal(), p.journal0, "the journal is byte-identical");
      assert.equal((await slotOf(p.r.rt, p.clone))?.state, "active", "the intruder's own admission is untouched");
      assert.equal(p.counters.owed, 0);
      assert.equal(entryOf(p)?.state, "pending");
    } finally { await p.done(); }
  });

  it("(b) a slot that no longer verifies before the wait: F3", async () => {
    const p = await parked();
    try {
      declinePark();
      p.r.rt.verifyReceiptSlots = async () => false;
      await resume(p);
      assertRefusal(p, "F3", /no longer verifies/);
      assertNothingDone(p);
      await assertUntouched(p);
    } finally { await p.done(); }
  });

  it("(c) unchanged slots and a guard that never samples in time: F1, a re-park through the D4 body with provenance", async () => {
    const p = await parked();
    try {
      starveGuard(p.f.memory, "timeout");
      const c = await resume(p);
      assert.equal(p.counters.refusals.length, 0, "not a receipt refusal");
      assert.equal(c.newParks().length, 1, "one authenticated park report");
      assert.equal(p.posts.length, 1, "the re-park (no hold) posts nothing: only the original park's receipt was sent");
      assert.deepEqual([c.newParks()[0]!.body.claim_generation, c.newParks()[0]!.body.memory_episode], [8, 4]);
      assert.deepEqual({ ...p.counters, refusals: 0 }, { refusals: 0, reserve: 0, block: 0, capture: 0, captureMemory: 0, admit: 0,
        preserve: 0, ran: 0, clone: 0, owed: p.counters.owed }, "no reservation, capture, admission or model turn");
      assert.equal(terminals().length, 0);
      const park = activeJournal()?.memoryPark;
      assert.equal(park?.binding.claimGeneration, 8, "provenance of the re-park");
      assert.equal(park?.ack, "confirmed");
      assert.equal(park?.settlement, undefined, "a re-park settles nothing");
      assert.equal(park?.origin.sourceEpoch, p.slot.epoch, "the slot owner and epoch of the receipt are preserved");
      assert.equal(entryOf(p), undefined, "the accepted re-park superseded the receipt");
      assert.deepEqual(await slotOf(p.r.rt, p.clone), p.slot);
    } finally { await p.done(); }
  });

  it("(c) the latest sample invalid: an earlier valid one after the start does not count (current-sample validity)", async () => {
    const p = await parked();
    try {
      let reads = 0;
      p.f.memory.guardObservation = () => reads++ === 0
        ? { seq: 4, validSeq: 4, armed: true, pendingIntervention: false }
        : { seq: 6, validSeq: 5, armed: true, pendingIntervention: false };
      let waits = 0;
      p.f.memory.waitGuardSample = async () => (waits++ === 0 ? "sample" : "timeout");
      const c = await resume(p);
      assert.equal(p.counters.refusals.length, 0);
      assert.equal(c.newParks().length, 1, "F1: the receipt never counted as freshly sampled");
      assert.equal(p.counters.ran, 0);
    } finally { await p.done(); }
  });

  for (const patch of [{ armed: false }, { pendingIntervention: true }]) {
    it(`(c) a fresh valid sample with ${JSON.stringify(patch)} is not a pass: F1`, async () => {
      const p = await parked();
      try {
        let reads = 0;
        p.f.memory.guardObservation = () => reads++ === 0
          ? { seq: 4, validSeq: 4, armed: true, pendingIntervention: false }
          : { seq: 6, validSeq: 6, armed: true, pendingIntervention: false, ...patch };
        let waits = 0;
        p.f.memory.waitGuardSample = async () => (waits++ === 0 ? "sample" : "timeout");
        const c = await resume(p);
        assert.equal(c.newParks().length, 1);
        assert.equal(p.counters.refusals.length, 0, "F1 re-park, not an F3 refusal");
        assert.equal(p.counters.ran, 0);
      } finally { await p.done(); }
    });
  }

  it("(c) a wait stub that reports aborted ends the claim nonterminally and keeps the entry pending", async () => {
    const p = await parked();
    try {
      starveGuard(p.f.memory, "aborted");
      const c = await resume(p);
      assert.equal(c.newParks().length, 0, "no park report and no fallback");
      assert.equal(p.counters.refusals.length, 0);
      assertNothingDone(p);
      await assertUntouched(p);
      assert.equal(entryOf(p)?.state, "pending", "PENDING kept");
    } finally { await p.done(); }
  });

  // The guard wait is a stand-in that honours the lifecycle signal it is handed exactly as the monitor's wait does
  // (it returns `aborted` only once that signal aborts, and a stall surfaces as `timeout`, which would F1 and fail the
  // test). The abort itself is raised by the REAL mechanism: a steering input polled from the server, or runner.shutdown().
  const custodyOf = (flight: Record<string, unknown>) => ({
    preserveRecoveryClone: flight.preserveRecoveryClone, preserveSession: flight.preserveSession,
    retainedLocalCustody: flight.retainedLocalCustody, keepGuardedInventoryOpen: flight.keepGuardedInventoryOpen,
  });
  const RETAINED = { preserveRecoveryClone: true, preserveSession: true, retainedLocalCustody: true, keepGuardedInventoryOpen: true };
  function lifecycleWait(p: Parked, trigger: () => void) {
    let flight: Record<string, unknown> | undefined;
    const seen: { atWait?: ReturnType<typeof custodyOf>; signalAborted?: boolean } = {};
    const gate = p.inner.memoryReceiptGate.bind(p.runner);
    p.inner.memoryReceiptGate = (claim, f) => { flight = f as Record<string, unknown>; return gate(claim, f); };
    p.f.memory.guardObservation = () => ({ seq: 1, validSeq: 0, armed: true, pendingIntervention: false });
    p.f.memory.waitGuardSample = async (_seq, _deadline, signal) => {
      seen.atWait = custodyOf(flight!);
      trigger();
      await new Promise<void>(resolve => {
        if (signal?.aborted) return resolve();
        const stall = setTimeout(resolve, 20_000);
        signal?.addEventListener("abort", () => { clearTimeout(stall); resolve(); }, { once: true });
      });
      seen.signalAborted = signal?.aborted === true;
      return seen.signalAborted ? "aborted" : "timeout";
    };
    return { seen, flight: () => flight! };
  }
  // A real cancel is the owner's terminal decision: its existing arm reports the run `failed` ("run cancelled"); the gate
  // adds nothing to that (no park report, no refusal, no fallback). Pause and shutdown report nothing terminal.
  const lifecycleCases: Array<[string, (p: Parked) => () => void, string[]]> = [
    ["a real server cancel", p => () => api.setInputs(p.f.claim.run_id, [{ id: 1, kind: "cancel" }]), ["run cancelled"]],
    ["a real steering pause now", p => () => api.setInputs(p.f.claim.run_id, [{ id: 1, kind: "pause", body: "now" }]), []],
    ["a real worker shutdown", p => () => p.runner.shutdown(), []],
  ];
  for (const [name, trigger, cancelReports] of lifecycleCases) {
    it(`(c) ${name} during the wait ends the gate without a refusal or fallback, retains custody from its start, keeps the entry pending`, async () => {
      const p = await parked();
      try {
        const w = lifecycleWait(p, trigger(p));
        const c = await resume(p);
        assert.equal(w.seen.signalAborted, true, "the wait ended because the real lifecycle signal aborted");
        assert.deepEqual(w.seen.atWait, RETAINED, "custody was already retained when the wait began (before the gate's first await)");
        assert.deepEqual(custodyOf(w.flight()), RETAINED, "and still is at the end");
        assert.equal(c.newParks().length, 0, "no park report and no fallback");
        assert.equal(p.counters.refusals.length, 0);
        assert.deepEqual(terminals().map(s => s.body.failure_reason).filter(Boolean), cancelReports);
        const { refusals: _refusals, ...work } = p.counters;
        void _refusals;
        assert.deepEqual(work, { reserve: 0, block: 0, capture: 0, captureMemory: 0, admit: 0, preserve: 0, ran: 0, clone: 0, owed: 0 },
          "no reservation, capture, admission, preservation, fresh clone or model turn");
        await assertUntouched(p);
        assert.equal(entryOf(p)?.state, "pending", "PENDING kept");
      } finally { await p.done(); }
    });
  }

  it("a gate that passes admits nothing before the handoff: no memory context, reservation or capture precedes discovery", async () => {
    const p = await parked();
    try {
      let atDiscovery: Counters | undefined;
      const discover = git.discoverRetainedRecovery.bind(git);
      git.discoverRetainedRecovery = async (...args: Parameters<typeof discover>) => {
        atDiscovery = { ...p.counters };
        return discover(...args);
      };
      await resume(p);
      assert.ok(atDiscovery, "discovery ran");
      assert.deepEqual({ ...atDiscovery, refusals: [] }, { refusals: [], reserve: 0, block: 0, capture: 0, captureMemory: 0, admit: 0,
        preserve: 0, ran: 0, clone: 0, owed: 0 });
    } finally { await p.done(); }
  });
});

describe("expected-bound discovery and the handoff (issue #3639)", () => {
  it("discovery returning nothing is F2, never a fresh clone", async () => {
    const p = await parked();
    try {
      declinePark();
      git.discoverRetainedRecovery = async () => undefined;
      await resume(p);
      assertRefusal(p, "F2", /journal is absent/);
      assertNothingDone(p);
      await assertUntouched(p);
      assert.equal(entryOf(p)?.state, "pending");
    } finally { await p.done(); }
  });

  const errors: Array<[string, () => Error, "F2" | "F3"]> = [
    ["MemoryReceiptSourceError absent", () => new MemoryReceiptSourceError("absent"), "F2"],
    ["MemoryReceiptSourceError foreign", () => new MemoryReceiptSourceError("foreign"), "F2"],
    ["MemoryReceiptSourceError changed", () => new MemoryReceiptSourceError("changed"), "F3"],
    ["ForeignRetainedRecoveryError", () => new ForeignRetainedRecoveryError(), "F2"],
    ["RetainedRecoveryBlockedError", () => new RetainedRecoveryBlockedError("/bare", BRANCH, "issue-2396", { runId: "r", clonePath: "/c" }), "F3"],
    ["a journal that would need a repair", () => new Error("recovery source does not belong to this key"), "F3"],
    ["a corrupt journal", () => new SyntaxError("Unexpected token"), "F2"],
    ["an unreadable store", () => Object.assign(new Error("EIO: i/o error"), { code: "EIO", syscall: "read" }), "F2"],
  ];
  for (const [name, make, kind] of errors) {
    it(`${name}: ${kind}, no blocker, no generic failure, no coordinate`, async () => {
      const p = await parked();
      try {
        declinePark();
        git.discoverRetainedRecovery = async () => { throw make(); };
        await resume(p);
        assertRefusal(p, kind, /./);
        assertNothingDone(p);
        await assertUntouched(p);
        assert.equal(entryOf(p)?.state, "pending");
      } finally { await p.done(); }
    });
  }

  it("discovery is bound to the receipt: it receives the expected source and runs read-only", async () => {
    const p = await parked();
    try {
      const calls: unknown[][] = [];
      assert.equal(activeJournal()!.attemptId, undefined, "premise: the parked clone is canonical, so the binding names no attempt");
      const discover = git.discoverRetainedRecovery.bind(git);
      git.discoverRetainedRecovery = async (...args: Parameters<typeof discover>) => { calls.push(args); return discover(...args); };
      await resume(p);
      assert.equal(calls.length, 1);
      const expected = calls[0]![4] as { runId: string; clonePath: string; attemptId?: string; memoryPark: string };
      assert.deepEqual(expected, { runId: p.f.claim.run_id, clonePath: p.clone, memoryPark: p.posts[0]!.bytes });
    } finally { await p.done(); }
  });

  it("revision item 1: gate passes, the handoff mismatches, the fallback park is unaccepted; the repeated reference is F3 again and never consumes", async () => {
    const p = await parked();
    try {
      declinePark();
      const classify = p.r.rt.classifySourceAdmission.bind(p.r.rt);
      p.r.rt.classifySourceAdmission = async (...args: Parameters<typeof classify>) => ({ ...(await classify(...args)), kind: "other" as const });
      await resume(p);
      assertRefusal(p, "F3", /not admissible at handoff/);
      assertNothingDone(p);
      await assertUntouched(p);
      assert.equal(entryOf(p)?.state, "pending", "the mismatch consumed nothing");
      // The owner's next Resume repeats the reference (the server never saw an accepted newer park).
      p.counters.refusals.length = 0;
      await resume(p, { generation: 9, episode: 4 });
      assertRefusal(p, "F3", /not admissible at handoff/);
      assertNothingDone(p);
      assert.equal(entryOf(p)?.state, "pending", "never CONSUMED");
      // And without a reference it is the unproven-pending refusal, not an ordinary flow.
      p.counters.refusals.length = 0;
      await resume(p, { generation: 10, episode: 5, ref: null });
      assertRefusal(p, "F2", /no reference/);
      assertNothingDone(p);
      await assertUntouched(p);
    } finally { await p.done(); }
  });

  it("revision item 1: the gate passes, then the journal vanishes before discovery; a later claim with no reference is still F2", async () => {
    const p = await parked();
    try {
      declinePark();
      const confirm = git.readConfirmedMemoryPark.bind(git);
      git.readConfirmedMemoryPark = async (...args: Parameters<typeof confirm>) => {
        const out = await confirm(...args);
        dropJournal();
        return out;
      };
      await resume(p);
      assertRefusal(p, "F2", /journal is absent|is absent/);
      assertNothingDone(p);
      assert.equal(entryOf(p)?.state, "pending");
      p.counters.refusals.length = 0;
      await resume(p, { generation: 9, episode: 4, ref: null });
      assertRefusal(p, "F2", /no reference/);
      assertNothingDone(p);
    } finally { await p.done(); }
  });

  it("R7: a lost ACK then a 400 leave the entry pending; with the journal missing the reference claim is F2 and nothing is admitted", async () => {
    const p = await parked({ post: async attempt => { if (attempt === 0) throw new TypeError("fetch failed"); throw new RequestError("POST", "/memory/settlement", 400, ""); } });
    try {
      assert.equal(p.posts.length, 2);
      assert.equal(entryOf(p)?.state, "pending");
      declinePark();
      dropJournal();
      await resume(p);
      assertRefusal(p, "F2", /no longer confirms/);
      assertNothingDone(p);
      assert.equal(entryOf(p)?.state, "pending");
    } finally { await p.done(); }
  });
});

/** A registered inert command on the flight's captured memory context, as a model command would be. */
function enrollCommand(ctx: Parameters<Executor["run"]>[0], counts: { cancelled: number }) {
  const captured = ctx.capturedMemory!()!;
  const entry = captured.commands.register(captured.context, {
    source: "codex-model-command", toolId: "tool", attemptId: "attempt", command: "inert fixture", root: { pid: 10, startTime: "10" },
    observe: async () => {
      const time = performance.now();
      return { available: true, complete: true, startedAtMs: time, completedAtMs: time, rows: [{ pid: 10, startTime: "10", rssBytes: 100 }] };
    },
    cancel: async () => { counts.cancelled++; },
    drain: async () => ({ confirmed: true }),
  });
  return { captured, entry };
}

describe("the same memory runtime and nonce guard the continuation (issue #3639)", () => {
  it("predecessor intervention (genuine drain and outcome) -> rearm after the quiet dwell -> gate passes at G+1 -> a later high-pressure sample intervenes with G+1 / E+1", async () => {
    const cancels = { cancelled: 0 };
    const p = await parked({ count: 1, predecessor: () => ({ run: async ctx => {
      const command = enrollCommand(ctx, cancels);
      usage = 950;
      const result = await command.captured.commands.watchOutcome(command.entry);
      ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
      throw new Error("model abort is not a test failure");
    } }) });
    try {
      assert.equal(p.f.reservations.length, 1, "premise: the predecessor really intervened");
      assert.deepEqual([p.f.reservations[0]!.claim_generation, p.f.reservations[0]!.memory_episode], [7, 3]);
      assert.equal(cancels.cancelled, 1, "premise: the predecessor's command was signalled");
      assert.equal(activeJournal()?.memoryPark?.settlement?.confirmed, true, "premise: a complete settlement (its joins really happened)");
      assert.equal(p.posts.length, 1);
      assert.equal(p.f.memory.guardObservation().armed, false, "premise: the intervention disarmed the guard (pressure still high)");
      const nonce = client.memoryIncarnation!.register_nonce;
      usage = 0; // the load drains: the guard rearms only inside its own evaluate, after the ticket resolved and a quiet dwell
      const nextCancels = { cancelled: 0 };
      // The owner's Resume starts the new episode's allowance afresh (the claim carries count 0).
      const spent = new Map<string, number>();
      client.reserveMemoryIntervention = async (request: MemoryReservationRequest) => {
        p.f.reservations.push(request);
        spent.set(request.intervention_id, spent.size + 1);
        const used = spent.size;
        return { ...request, admitted: true, authorizing: true, allowance: { limit: 2, used, remaining: 2 - used } };
      };
      client.reportMemoryInterventionOutcome = async request => {
        const used = spent.get(request.intervention_id)!;
        return { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
          allowance: { limit: 2, used, remaining: 2 - used } };
      };
      let ran = 0;
      let armedAtAdmission: boolean | undefined;
      p.setExecutor({ run: async ctx => {
        ran++;
        armedAtAdmission = p.f.memory.guardObservation().armed;
        const command = enrollCommand(ctx, nextCancels);
        usage = 950; // a later high-pressure event
        await command.captured.commands.watchOutcome(command.entry);
        throw new Error("the successor stops after its intervention");
      } });
      const claim: ClaimResponse = { ...p.f.claim, claim_generation: 8, memory_episode: 4, memory_policy: { version: 1, max_interventions: 2 },
        memory_intervention_count: 0, memory_park_reference: p.ref() };
      api.setOwnershipStatus(claim.run_id, "running", 8);
      try { await p.runner.execute(claim); } catch { /* the successor's stop */ }
      assert.equal(p.counters.refusals.length, 0, `the gate passed: ${JSON.stringify(p.counters.refusals)}`);
      assert.equal(ran, 1);
      assert.equal(armedAtAdmission, true, "the SAME guard rearmed (no reset, no fabricated ticket) before the continuation admitted anything");
      assert.equal(client.memoryIncarnation!.register_nonce, nonce, "the same registration nonce throughout");
      assert.equal(p.f.reservations.length, 2, "the later high-pressure sample intervened");
      assert.deepEqual([p.f.reservations[1]!.claim_generation, p.f.reservations[1]!.memory_episode], [8, 4],
        "its reserve request names the continuation's generation and episode");
      assert.equal(p.f.reservations[1]!.register_nonce, NONCE);
      assert.equal(nextCancels.cancelled, 1, "the continuation's command was actually signalled");
    } finally { await p.done(); }
  });
});

describe("the guard stays armed through the handoff (issue #3639)", () => {
  const DISARMED = () => ({ seq: 9, validSeq: 9, armed: false, pendingIntervention: true });

  for (const [label, observed] of [["another flight's intervention disarms it", DISARMED],
    ["its current sample turns invalid", () => ({ seq: 10, validSeq: 9, armed: true, pendingIntervention: false })]] as const) {
  it(`armed at gate step (c), then ${label} before the handoff ends: F3, nothing admitted, PENDING kept`, async () => {
    const p = await parked();
    try {
      declinePark();
      let armedReads = 0;
      const observe = p.f.memory.guardObservation.bind(p.f.memory);
      p.f.memory.guardObservation = () => { armedReads++; return observe(); };
      const classify = p.r.rt.classifySourceAdmission.bind(p.r.rt);
      p.r.rt.classifySourceAdmission = async (...args: Parameters<typeof classify>) => {
        const out = await classify(...args);
        assert.ok(armedReads >= 2, "premise: the gate had already observed the armed guard");
        p.f.memory.guardObservation = observed; // the concurrent change, after the gate and before the handoff's end
        return out;
      };
      await resume(p);
      assertRefusal(p, "F3", /no longer armed at handoff/);
      assertNothingDone(p);
      await assertUntouched(p);
      assert.equal(entryOf(p)?.state, "pending", "PENDING kept");
    } finally { await p.done(); }
  });
  }

  it("after the handoff the flight is admitted like any claim: a later disarm stops neither a renewal begin nor resets the consumed entry", async () => {
    const p = await parked();
    try {
      let began = 0;
      const begin = p.f.memory.begin.bind(p.f.memory);
      p.f.memory.begin = (...args: Parameters<typeof begin>) => { began++; return begin(...args); };
      let flight: { workerMemory?: { epochReady?: boolean; resumeReady?: boolean } } | undefined;
      const capture = p.inner.captureWorkerMemory.bind(p.runner) as (f: unknown) => unknown;
      p.inner.captureWorkerMemory = (f: unknown) => { flight = f as typeof flight; return capture(f); };
      const seen: { first?: boolean; second?: boolean; threw?: boolean; stateAfter?: string } = {};
      p.setExecutor({ run: async ctx => {
        p.counters.ran++;
        seen.first = ctx.capturedMemory!() !== undefined;
        p.f.memory.guardObservation = DISARMED; // another flight intervened after this flight was admitted
        flight!.workerMemory!.epochReady = true; // the epoch renewal path retires the context and begins again
        flight!.workerMemory!.resumeReady = true;
        try { seen.second = ctx.capturedMemory!() !== undefined; } catch { seen.threw = true; }
        seen.stateAfter = entryOf(p)?.state;
        throw new Error("the successor stops here");
      } });
      const claim: ClaimResponse = { ...p.f.claim, claim_generation: 8, memory_episode: 4, memory_policy: { version: 1, max_interventions: 2 },
        memory_intervention_count: 0, memory_park_reference: p.ref() };
      api.setOwnershipStatus(claim.run_id, "running", 8);
      try { await p.runner.execute(claim); } catch { /* the executor stops */ }
      assert.equal(seen.first, true, "the first begin had the guard armed");
      assert.equal(seen.threw, undefined, "no RetainedRecoveryStop on the renewal");
      assert.equal(seen.second, true);
      assert.equal(began, 2, "begin ran again for the renewal");
      assert.equal(seen.stateAfter, "consumed", "the entry stays consumed");
    } finally { await p.done(); }
  });

  it("armed with no intervention ticket at all is the positive control: the real guard passes the gate and the first context begins", async () => {
    const p = await parked();
    try {
      let began = 0;
      const begin = p.f.memory.begin.bind(p.f.memory);
      p.f.memory.begin = (...args: Parameters<typeof begin>) => { began++; return begin(...args); };
      let captured = false;
      p.setExecutor({ run: async ctx => {
        p.counters.ran++;
        captured = ctx.capturedMemory!() !== undefined;
        throw new Error("the successor stops here");
      } });
      const claim: ClaimResponse = { ...p.f.claim, claim_generation: 8, memory_episode: 4, memory_policy: { version: 1, max_interventions: 2 },
        memory_intervention_count: 0, memory_park_reference: p.ref() };
      api.setOwnershipStatus(claim.run_id, "running", 8);
      try { await p.runner.execute(claim); } catch { /* the executor stops */ }
      assert.equal(p.counters.refusals.length, 0);
      assert.equal(captured, true);
      assert.equal(began, 1);
      assert.equal(entryOf(p)?.state, "consumed");
    } finally { await p.done(); }
  });
});

describe("the live producer's durable source end is genuinely delayed or failed (issue #3639)", () => {
  // The REAL AuthorityRuntime.endSourceUse runs in both cases; only the condition around it is injected.
  const settledProbe = (inner: Internals, seen: boolean[]) => {
    const real = inner.memoryHoldSettled.bind(inner);
    inner.memoryHoldSettled = (...args: unknown[]) => { const out = real(...args); seen.push(out); return out; };
  };
  async function drained(p: Parked): Promise<SlotSnapshot | null> {
    let slot = await slotOf(p.r.rt, p.clone);
    for (let i = 0; i < 300 && slot?.state !== "ineligible"; i++) {
      await new Promise(resolve => setTimeout(resolve, 20));
      slot = await slotOf(p.r.rt, p.clone);
    }
    return slot;
  }

  it("end paused under the authority lock: the end is only queued, no POST, no qualifying settlement, and the queued end cannot act on a successor lease", async () => {
    const settled: boolean[] = [];
    const ends: Array<{ kept: number; endedSlots: number }> = [];
    const p = await parked({ authority: { retryMs: 20 }, tweak: (inner, r) => {
      (inner as unknown as { sourceUseEndWaitMs: number }).sourceUseEndWaitMs = 150;
      settledProbe(inner, settled);
      const real = r.rt.endSourceUse.bind(r.rt);
      // The authority lock A is held by another party for the whole of the producer's bounded end wait, and freed right
      // after it gave up: the end is then only QUEUED, and the drain applies it later.
      r.rt.endSourceUse = async (paths, opts) => {
        let release!: () => void;
        const held = new Promise<void>(resolve => { release = resolve; });
        let acquired!: () => void;
        const holding = new Promise<void>(resolve => { acquired = resolve; });
        const holder = withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async () => { acquired(); await held; });
        await holding;
        const out = await real(paths, opts);
        ends.push({ kept: out.kept.length, endedSlots: out.endedSlots.length });
        release();
        await holder;
        return out;
      };
    } });
    try {
      assert.ok(settled.length >= 1 && settled.every(Boolean), "premise: the hold is settled and the tails were joined; only the durable end is missing");
      assert.deepEqual(ends, [{ kept: 1, endedSlots: 0 }], "premise: the real end was kept, with no end receipt");
      assert.equal(p.posts.length, 0, "no settlement POST");
      assert.notEqual(activeJournal()?.memoryPark?.settlement?.confirmed, true, "no settlement was confirmed");
      assert.equal(entryOf(p), undefined, "the park does not qualify");
      // The queued end lands on ITS OWN epoch only.
      const old = await drained(p);
      assert.equal(old?.state, "ineligible");
      assert.equal(old?.epoch, p.slot.epoch);
      const lease = await p.r.rt.bindSource(p.clone);
      await p.r.rt.withTransaction(async () => undefined);
      const next = await slotOf(p.r.rt, p.clone);
      assert.equal(next?.state, "active", "the successor's slot stays active");
      assert.notEqual(next?.epoch, p.slot.epoch, "a fresh epoch");
      assert.equal(p.r.rt.leaseFor(p.clone), lease);
    } finally { await p.done(); }
  });

  it("end write failing: the key stays active and owned, no POST, no qualifying settlement", async () => {
    const settled: boolean[] = [];
    const flags = { head: false };
    const ends: Array<{ kept: number; endedSlots: number }> = [];
    const p = await parked({ authority: {
      retryMs: 3_600_000,
      ops: { syncFile: h => h.sync(), syncDir: h => h.sync(), rename: async (a, b) => {
        if (flags.head && path.basename(path.dirname(b)) === "heads") throw new Error("injected head failure");
        fs.renameSync(a, b);
      } },
    }, tweak: (inner, r) => {
      settledProbe(inner, settled);
      const real = r.rt.endSourceUse.bind(r.rt);
      r.rt.endSourceUse = async (paths, opts) => {
        flags.head = true;
        const out = await real(paths, opts);
        ends.push({ kept: out.kept.length, endedSlots: out.endedSlots.length });
        return out;
      };
    } });
    try {
      assert.ok(settled.length >= 1 && settled.every(Boolean), "premise: the hold is settled and the tails were joined; only the durable end failed");
      assert.equal(p.posts.length, 0, "no settlement POST");
      assert.notEqual(activeJournal()?.memoryPark?.settlement?.confirmed, true);
      assert.equal(entryOf(p), undefined);
      assert.deepEqual(ends, [{ kept: 1, endedSlots: 0 }], "the real end reported no end receipt");
      assert.equal(p.r.rt.heldByOtherSink(p.clone, new Set()), true, "the key stays owned");
    } finally { await p.done(); }
  });
});

describe("old-API claims after a lost receipt ACK (issue #3639)", () => {
  it("stored receipt, lost ACK, retry 400, Resume, old-API claim with the reference AND journal missing: F2, nothing admitted, PENDING kept for the next claim", async () => {
    const p = await parked({ post: async attempt => { if (attempt === 0) throw new TypeError("fetch failed"); throw new RequestError("POST", "/memory/settlement", 400, ""); } });
    try {
      assert.equal(p.posts.length, 2, "the lost ACK was retried and the retry was refused with 400");
      assert.equal(entryOf(p)?.state, "pending", "a bare 400 never consumes PENDING");
      declinePark();
      dropJournal();
      const c = await resume(p, { ref: null });
      assertRefusal(p, "F2", /no reference/);
      assert.equal(c.newParks().length >= 1, true, "the fallback park was reported, not accepted");
      assertNothingDone(p);
      assert.equal(rawJournal(), "", "no provenance was invented for the missing journal");
      assert.equal(entryOf(p)?.state, "pending", "PENDING kept");
      // A later requeue claim (the reference is advertised again) still finds the pending entry: it reaches the gate.
      assert.equal(p.inner.memoryReceiptDecision({ run_id: p.f.claim.run_id, memory_park_reference: p.ref() }).kind, "gate");
      p.counters.refusals.length = 0;
      await resume(p, { generation: 9, episode: 4 });
      assertRefusal(p, "F2", /no longer confirms/);
      assertNothingDone(p);
      assert.equal(entryOf(p)?.state, "pending", "still PENDING after the requeue claim");
    } finally { await p.done(); }
  });
});

describe("the own-park table (issue #3639)", () => {
  async function bareRunner() {
    api.memoryRegisterNonce = NONCE;
    api.setRegisterProtocolFeatures(FEATURES);
    await client.register("memory-fixture", "base");
    return runnerWith(() => ({ executor: { run: async () => { throw new Error("none"); } }, homeDir }), fakeGitlab().gitlab) as unknown as Internals;
  }
  const binding = (runId: string, nonce = NONCE, generation = 7) => ({ run_id: runId, worker_id: client.memoryIncarnation!.worker_id,
    register_nonce: nonce, claim_generation: generation, memory_episode: 3 });

  it("holds at most 1024 entries: a consumed one is evicted oldest first, a pending one never, and a full pending table creates none", async () => {
    const runner = await bareRunner();
    const ids = Array.from({ length: 1024 }, () => randomUUID());
    for (const id of ids) assert.ok(runner.createOwnMemoryPark(id, binding(id)));
    assert.equal(runner.ownMemoryParks.size, 1024);
    const overflow = randomUUID();
    assert.equal(runner.createOwnMemoryPark(overflow, binding(overflow)), undefined, "full of pending: no entry, so no receipt is ever sent");
    assert.equal(runner.ownMemoryParks.size, 1024);
    runner.ownMemoryParks.get(ids[5]!)!.state = "consumed";
    runner.ownMemoryParks.get(ids[9]!)!.state = "consumed";
    const first = randomUUID();
    assert.ok(runner.createOwnMemoryPark(first, binding(first)));
    assert.equal(runner.ownMemoryParks.has(ids[5]!), false, "the oldest consumed entry was evicted");
    assert.equal(runner.ownMemoryParks.has(ids[9]!), true);
    assert.equal(runner.ownMemoryParks.size, 1024);
    // A consumed entry that is used again is the newest, so the other one goes next.
    const entry = runner.ownMemoryParks.get(ids[9]!)!;
    entry.sha = sha256("x");
    runner.memoryReceiptDecision({ run_id: ids[9]!, memory_park_reference: { claim_generation: 7, memory_episode: 3,
      worker_id: client.memoryIncarnation!.worker_id, register_nonce: NONCE, settlement_sha256: entry.sha } });
    runner.ownMemoryParks.get(ids[7]!)!.state = "consumed";
    const second = randomUUID();
    assert.ok(runner.createOwnMemoryPark(second, binding(second)));
    assert.equal(runner.ownMemoryParks.has(ids[7]!), false, "LRU: the touched entry outlived the older one");
    assert.equal(runner.ownMemoryParks.has(ids[9]!), true);
  });

  it("a newer accepted park of the run replaces the entry", async () => {
    const runner = await bareRunner();
    const id = randomUUID();
    const older = runner.createOwnMemoryPark(id, binding(id, NONCE, 7))!;
    older.state = "consumed";
    const newer = runner.createOwnMemoryPark(id, binding(id, NONCE, 9))!;
    assert.equal(runner.ownMemoryParks.size, 1);
    assert.equal(runner.ownMemoryParks.get(id), newer);
    assert.equal(newer.state, "pending");
    assert.equal(newer.binding.claim_generation, 9);
  });

  it("a registration nonce change drops the old nonce's entries", async () => {
    const runner = await bareRunner();
    const id = randomUUID();
    runner.createOwnMemoryPark(id, binding(id));
    assert.equal(runner.ownMemoryParks.size, 1);
    api.memoryRegisterNonce = "a-new-registration";
    await client.register("memory-fixture", "base");
    const decision = runner.memoryReceiptDecision({ run_id: id });
    assert.equal(decision.kind, "ordinary", "no entry survives for the old registration");
    assert.equal(runner.ownMemoryParks.size, 0);
  });

  it("the decision table: pending needs the equal reference, consumed tolerates none or equal, no entry refuses any reference", async () => {
    const runner = await bareRunner();
    const id = randomUUID();
    const entry = runner.createOwnMemoryPark(id, binding(id))!;
    entry.sha = sha256("posted");
    const ref = (sha = entry.sha!): MemoryParkReference => ({ claim_generation: 7, memory_episode: 3,
      worker_id: client.memoryIncarnation!.worker_id, register_nonce: NONCE, settlement_sha256: sha });
    assert.equal(runner.memoryReceiptDecision({ run_id: id, memory_park_reference: ref() }).kind, "gate");
    assert.equal(runner.memoryReceiptDecision({ run_id: id }).kind, "refuse");
    assert.equal(runner.memoryReceiptDecision({ run_id: id, memory_park_reference: ref(sha256("other")) }).kind, "refuse");
    entry.state = "consumed";
    assert.equal(runner.memoryReceiptDecision({ run_id: id }).kind, "ordinary");
    assert.equal(runner.memoryReceiptDecision({ run_id: id, memory_park_reference: ref() }).kind, "ordinary");
    assert.equal(runner.memoryReceiptDecision({ run_id: id, memory_park_reference: ref(sha256("other")) }).kind, "refuse");
    const other = randomUUID();
    assert.equal(runner.memoryReceiptDecision({ run_id: other }).kind, "ordinary");
    assert.equal(runner.memoryReceiptDecision({ run_id: other, memory_park_reference: ref() }).kind, "refuse");
    // An entry whose bytes were never posted (no sha) can equal no reference.
    const unposted = randomUUID();
    runner.createOwnMemoryPark(unposted, binding(unposted));
    assert.equal(runner.memoryReceiptDecision({ run_id: unposted, memory_park_reference: ref() }).kind, "refuse");
  });
});

describe("the client side of the receipt (issue #3639)", () => {
  const run = "11111111-1111-4111-8111-111111111111";
  const worker = "22222222-2222-4222-8222-222222222222";
  const log: Logger = { debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {}, child() { return this; } };
  const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
  const registration = (features: string[]) => ({ worker_id: worker, register_nonce: "nonce", protocol_features: features });
  const newClient = () => new WorkerClient("http://example.invalid", "fixture", "test", log, { terminalRetrySchedule: [0], sleep: async () => {} });
  const BYTES = '{"version":1,"binding":{"runId":"x"}}';

  it("posts the exact bytes, unquoted, to the settlement route and accepts only an echo of their sha256", async t => {
    const c = newClient();
    const seen: Array<{ url: string; body: unknown; type: string | null }> = [];
    t.mock.method(globalThis, "fetch", async (url: string, init: RequestInit) => {
      if (url.endsWith("/register")) return json(registration(["worker_memory_pressure_v1", "memory_park_settlement_v1"]));
      seen.push({ url, body: init.body, type: new Headers(init.headers).get("content-type") });
      return json({ settlement_sha256: sha256(BYTES) });
    });
    await c.register("fixture");
    await c.reportMemoryParkSettlement(run, BYTES);
    assert.deepEqual(seen, [{ url: `http://example.invalid/api/worker/runs/${run}/memory/settlement`, body: BYTES, type: "application/json" }]);
  });

  // The producer cannot be driven to the fixture's exact record (its run id, incarnations and epochs are fixed values), so
  // this pins the two halves at their seams: the client posts whatever bytes it is given verbatim and accepts only the
  // echo of their sha256, and the runner hands it serializeMemoryPark of the journal record (first producer test above).
  for (const fixture of ["settlement-typical.json", "settlement-max-shape.json"]) {
    it(`the client posts ${fixture}'s bytes verbatim and accepts exactly the fixture's .sha256 as the echo`, async t => {
      const dir = path.resolve(import.meta.dirname, "../../fixtures/memory-park");
      const text = fs.readFileSync(path.join(dir, fixture), "utf8");
      const expected = fs.readFileSync(path.join(dir, `${fixture}.sha256`), "utf8").trim();
      const c = newClient();
      const bodies: unknown[] = [];
      t.mock.method(globalThis, "fetch", async (url: string, init: RequestInit) => {
        if (url.endsWith("/register")) return json(registration(["memory_park_settlement_v1"]));
        bodies.push(init.body);
        return json({ settlement_sha256: expected });
      });
      await c.register("fixture");
      await c.reportMemoryParkSettlement(run, text);
      assert.equal(bodies.length, 1);
      assert.equal(bodies[0], text, "the POST body is the fixture's bytes exactly");
      assert.equal(sha256(bodies[0] as string), expected, "and S is the fixture's .sha256");
    });
  }

  it("a 200 that does not echo the posted bytes' sha256 is a definite failure, never a retry", async t => {
    const c = newClient();
    t.mock.method(globalThis, "fetch", async (url: string) => url.endsWith("/register")
      ? json(registration(["memory_park_settlement_v1"])) : json({ settlement_sha256: sha256("other") }));
    await c.register("fixture");
    await assert.rejects(c.reportMemoryParkSettlement(run, BYTES), (error: unknown) =>
      error instanceof Error && error.name === "MemorySettlementResponseError" && !isTransient(error));
  });

  it("an HTTP refusal is a RequestError carrying its status, so the caller can tell a transient one", async t => {
    const c = newClient();
    let status = 409;
    t.mock.method(globalThis, "fetch", async (url: string) => url.endsWith("/register")
      ? json(registration(["memory_park_settlement_v1"])) : json({ error: "memory_binding_conflict" }, status));
    await c.register("fixture");
    for (status of [400, 404, 409, 413, 500]) {
      await assert.rejects(c.reportMemoryParkSettlement(run, BYTES), (error: unknown) =>
        error instanceof RequestError && error.status === status && isTransient(error) === (status === 500));
    }
  });

  it("without the negotiated feature (or with a malformed run id) nothing is sent", async t => {
    const c = newClient();
    let calls = 0;
    t.mock.method(globalThis, "fetch", async (url: string) => {
      if (url.endsWith("/register")) return json(registration(["worker_memory_pressure_v1"]));
      calls++;
      return json({});
    });
    await c.register("fixture");
    await assert.rejects(c.reportMemoryParkSettlement(run, BYTES));
    c.protocolFeatures = ["memory_park_settlement_v1"];
    await assert.rejects(c.reportMemoryParkSettlement("not-a-run-id", BYTES));
    assert.equal(calls, 0);
  });

  const reference = (patch: Record<string, unknown> = {}) => ({ claim_generation: 7, memory_episode: 3, worker_id: worker,
    register_nonce: "nonce", settlement_sha256: sha256("posted"), ...patch });
  it("a claim's memory_park_reference survives when well formed and is removed when malformed", async t => {
    const c = newClient();
    let body: unknown;
    t.mock.method(globalThis, "fetch", async (url: string) => url.endsWith("/register")
      ? json(registration(["worker_memory_pressure_v1"])) : json(body));
    await c.register("fixture");
    const claim = (memory_park_reference?: unknown) => ({ run_id: run, claim_generation: 8, memory_episode: 4,
      memory_intervention_count: 0, memory_policy: null, ...(memory_park_reference === undefined ? {} : { memory_park_reference }) });
    body = claim(reference());
    assert.deepEqual((await c.claimRun())?.memory_park_reference, reference());
    for (const bad of [reference({ settlement_sha256: "ABC" }), reference({ claim_generation: 0 }), reference({ worker_id: "nope" }),
      reference({ register_nonce: "" }), reference({ extra: true }), "a string", null, []]) {
      body = claim(bad);
      const decoded = await c.claimRun();
      assert.equal(decoded?.run_id, run, "the claim itself is delivered");
      assert.equal("memory_park_reference" in decoded!, false, `removed: ${JSON.stringify(bad)}`);
    }
    body = claim();
    assert.equal("memory_park_reference" in (await c.claimRun())!, false);
  });
});
