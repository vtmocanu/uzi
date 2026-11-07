// issue #4046 M2 (agent): a retained-recovery window whose every witnessed reservation ended in a data volume capacity
// refusal parks with `retained_capacity_only: true`, but only against an api that advertised
// `retained_recovery_capacity_hold_v1`. Real git on the real retained runner, a real DiskGovernor (only for its
// hard threshold) and a DataVolumeGuard whose statfs/stat/reclaim seams are injected, as in
// runner-retained-seed-admission.test.ts. Clock, backoff and reclaim waits are injected, never slept on.
import { it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { DiskGovernor, type CacheCapConfig } from "../src/cache-cap.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import { RequestError } from "../src/client.js";
import { RecoveryBudgetExhaustedError, RecoveryDeadlineExhaustedError } from "../src/git.js";
import type { ExecutorFactory } from "../src/runner.js";
import type { StateRequest } from "../src/protocol.js";
import { codexRig } from "./runner-codex-sinks-fixture.js";
import { noProofReseed, nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const BSIZE = 4096;
const BLOCKS = 10_000_000;
const FILES = 10_000_000;
const HOLD = "retained_recovery_capacity_hold_v1";
const BASE_FEATURES = ["claim_generation_fence", "recovery_cause_data_volume_full", "recovery_cause_retained_recovery_exhausted"];
const WITH_HOLD = [...BASE_FEATURES, HOLD];
const GOVERNOR_ON: CacheCapConfig = {
  capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 2, hardStopEnabled: true, hardMargin: 0.03,
};
const HELD_TEXT = "retained recovery held for owner: every recovery attempt in this window was refused for data volume capacity; owner Resume required; predecessor work and custody retained";
const UNMARKED = { status: "recovery_wait", recovery_cause: "retained_recovery_exhausted", claim_generation: 2 };
const MARKED = { ...UNMARKED, retained_capacity_only: true };

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
async function seed() {
  const claim = gitlabClaim(2512, { claim_generation: 2, session_id: "predecessor-session" });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, 2512, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone };
}
function journal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}
const sleep = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms));

/** A controllable Date.now: real time until pinned. Timers stay real, which is what lets a pinned clock sit a few
 *  milliseconds before a deadline and let the operation's own timer fire. */
function clockOf(t: TestContext) {
  const real = Date.now.bind(Date);
  let pinned: number | undefined;
  t.mock.method(Date, "now", () => pinned ?? real());
  return { pin: (v: number) => { pinned = v; } };
}

type Ack = Awaited<ReturnType<typeof client.reportState>>;
interface Ctx {
  claim: ReturnType<typeof gitlabClaim>;
  bare: string;
  clone: { path: string; branch: string };
  deadline: () => number;
  set: (used: number) => void;
}
interface Opts {
  used: number;
  features?: string[];
  /** Answer the n-th (1-based) exhausted-park report; default confirms it. Throwing simulates a lost ACK or a 4xx. */
  respond?: (body: StateRequest, n: number, runId: string) => Ack | Promise<Ack>;
  onReclaim?: (n: number, ctx: Ctx) => void | Promise<void>;
  onWait?: (n: number, flight: { steering: { markCancelled(): void } }, ctx: Ctx) => void;
  onMeasure?: (ctx: Ctx) => void;
  measureError?: Error;
  /** Runs after the n-th retry backoff (1-based), before the next reservation. */
  afterBackoff?: (n: number, ctx: Ctx) => void;
  /** Wraps the n-th (1-based) call of the real reservation. */
  reserveWrap?: (n: number, orig: () => Promise<any>, ctx: Ctx) => Promise<any>;
  claimOverrides?: Record<string, unknown>;
  executor?: Record<string, unknown>;
  /** Reuse an already seeded fixture (a second execute on the same journal). */
  prior?: Awaited<ReturnType<typeof seed>>;
  runner?: any;
}

const confirmed = (runId: string, body: StateRequest): Ack => {
  api.setOwnershipStatus(runId, "recovery_wait", body.claim_generation as number);
  return { applied: true, status: "recovery_wait", recoveryCause: "retained_recovery_exhausted" } as Ack;
};

async function drive(o: Opts) {
  const { claim, bare, clone } = o.prior ?? await seed();
  Object.assign(claim, o.claimOverrides ?? {});
  client.protocolFeatures = o.features ?? WITH_HOLD;
  const state = { used: o.used };
  const r = {
    models: 0, prepares: 0, reclaims: 0, releases: 0, backoffs: 0, reserves: 0,
    parks: [] as StateRequest[], reserveErrors: [] as unknown[], waitErrors: [] as unknown[],
  };
  const prep = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => { r.prepares++; return prep(...args); };
  const measure = git.measureRetainedSeedFootprint.bind(git);
  git.measureRetainedSeedFootprint = async (p, opts) => {
    o.onMeasure?.(ctx);
    if (o.measureError) throw o.measureError;
    return measure(p, opts);
  };
  const ctx: Ctx = {
    claim, bare, clone,
    deadline: () => journal(bare, clone.branch).recovery.deadline as number,
    set: used => { state.used = used; },
  };
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs: () => {
      const free = Math.round(BLOCKS * (1 - state.used));
      return { bsize: BSIZE, blocks: BLOCKS, bfree: free, bavail: free, files: FILES, ffree: Math.round(FILES * 0.8) };
    },
    stat: async () => ({ dev: 7 }),
    reclaim: async () => { r.reclaims++; await o.onReclaim?.(r.reclaims, ctx); },
  });
  client.releaseRecoveryCustody = async () => { r.releases++; throw new Error("must retain custody"); };
  const report = client.reportState.bind(client);
  client.reportState = async (runId, body, signal) => {
    if (body.status === "recovery_wait" && body.recovery_cause === "retained_recovery_exhausted") {
      r.parks.push(body);
      return o.respond ? o.respond(body, r.parks.length, runId) : confirmed(runId, body);
    }
    return report(runId, body, signal);
  };
  const reserve = git.reserveRecoveryIteration.bind(git);
  git.reserveRecoveryIteration = async (...args) => {
    const n = ++r.reserves;
    const orig = () => reserve(...args);
    try { return await (o.reserveWrap ? o.reserveWrap(n, orig, ctx) : orig()); }
    catch (error) { r.reserveErrors.push(error); throw error; }
  };
  const diskGovernor = new DiskGovernor({
    config: GOVERNOR_ON, log: nullLogger(), volumeTotalBytes: () => BLOCKS * BSIZE, thresholdOf: () => 0.9,
  });
  const factory: ExecutorFactory = runId => ({
    homeDir: path.join(homeDir, runId),
    executor: { ...o.executor, run: async () => { r.models++; throw new Error("model interrupted before settled completion"); } } as any,
  });
  const runner = o.runner ?? runnerWith(factory, fakeGitlab().gitlab, undefined, nullLogger(),
    { recoveryRetryMs: 1, dataVolume: guard, diskGovernor });
  let waits = 0;
  const wait = (runner as any).awaitDataVolumeReclaim.bind(runner);
  (runner as any).awaitDataVolumeReclaim = (g: unknown, flight: any, op: string, signal?: AbortSignal) => {
    if (op === "retained_seed_admission") o.onWait?.(++waits, flight, ctx);
    return wait(g, flight, op, signal).catch((error: unknown) => { r.waitErrors.push(error); throw error; });
  };
  const backoff = (runner as any).waitRecoveryRetry.bind(runner);
  (runner as any).waitRecoveryRetry = async (...args: unknown[]) => {
    await backoff(...args);
    o.afterBackoff?.(++r.backoffs, ctx);
  };
  await runner.execute(claim);
  return { ...r, claim, bare, clone, state, runner, ctx, prior: { claim, bare, clone } };
}
type Result = Awaited<ReturnType<typeof drive>>;

function feed(r: Result): string[] {
  return api.messages(r.claim.run_id).filter(m => m.kind === "status").map(m => String((m.payload as { text?: unknown }).text));
}
const failedReports = () => api.states.filter(s => s.body.status === "failed");
/** The clean marked park of a fully refused window of `attempts` iterations. */
function assertMarkedHold(r: Result, attempts: number): void {
  assert.equal(r.models, 0, "no model call");
  assert.equal(r.prepares, 0, "no successor seeded");
  assert.equal(r.releases, 0, "custody retained");
  assert.deepEqual(failedReports(), [], "the park replaces the failed report");
  assert.equal(r.parks.length, 1, JSON.stringify(r.parks));
  assert.deepEqual(r.parks[0], MARKED);
  const j = journal(r.bare, r.clone.branch);
  assert.equal(j.clonePath, r.clone.path, "no successor replaces the predecessor");
  assert.equal(j.recovery.stage, "blocked");
  assert.equal(j.recovery.blocker, "budget_exhausted");
  assert.equal(j.recovery.attempts, attempts);
  assert.equal(feed(r).filter(t => t === HELD_TEXT).length, 1, JSON.stringify(feed(r)));
  assert.equal(fs.readFileSync(path.join(r.clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
}
function assertUnmarkedPark(r: Result): void {
  assert.equal(r.models, 0);
  assert.equal(r.prepares, 0);
  assert.deepEqual(failedReports(), []);
  assert.deepEqual(r.parks, [UNMARKED]);
  assert.equal(journal(r.bare, r.clone.branch).recovery.blocker, "budget_exhausted");
  assert.equal(feed(r).includes(HELD_TEXT), false, "the owner-hold notice is only for a confirmed marked park");
}
const badRequest = () => new RequestError("POST", "/api/worker/runs/x/state", 400, "bad request");

it("three positive capacity refusals park once with retained_capacity_only and seed nothing", async () => {
  const r = await drive({ used: 0.89 });
  assertMarkedHold(r, 3);
  assert.equal(r.reclaims, 3, "one bounded reclaim per refused iteration");
});

for (const k of [1, 2]) {
  it(`a deadline after ${k} refusal(s) in the iteration-catch deadline branch is still capacity-only`, async t => {
    const clock = clockOf(t);
    const r = await drive({ used: 0.89, onReclaim: (n, c) => { if (n === k) clock.pin(c.deadline() + 1); } });
    assertMarkedHold(r, k);
  });
  it(`a deadline after ${k} refusal(s) where the reservation's own timer fires is still capacity-only`, async t => {
    const clock = clockOf(t);
    const r = await drive({
      used: 0.89,
      afterBackoff: (n, c) => { if (n === k) clock.pin(c.deadline() - 5); },
      reserveWrap: async (n, orig, c) => {
        if (n !== k + 1) return orig();
        await sleep(60);
        clock.pin(c.deadline() + 1);
        return orig();
      },
    });
    assertMarkedHold(r, k);
    assert.equal(r.reserveErrors.length, 1);
    assert.ok(r.reserveErrors[0] instanceof RecoveryDeadlineExhaustedError, "the timer's abort reason surfaces typed from the real reservation");
  });
  it(`a deadline after ${k} refusal(s) already expired when the reservation starts is still capacity-only`, async t => {
    const clock = clockOf(t);
    const thrown: unknown[] = [];
    const wrapped = git.withRecoveryOperation.bind(git);
    t.mock.method(git, "withRecoveryOperation", async (...args: Parameters<typeof wrapped>) => {
      try { return await wrapped(...args); } catch (error) { thrown.push(error); throw error; }
    });
    const r = await drive({ used: 0.89, afterBackoff: (n, c) => { if (n === k) clock.pin(c.deadline() + 1); } });
    assertMarkedHold(r, k);
    assert.ok(thrown.some(e => e instanceof RecoveryDeadlineExhaustedError), "the entry-time expiry is the typed deadline error");
    assert.ok(thrown.some(e => e instanceof Error && e.message === "recovery deadline exhausted"));
  });
  it(`a deadline after ${k} refusal(s) reached through the remaining-budget check is still capacity-only`, async t => {
    const clock = clockOf(t);
    const r = await drive({
      used: 0.89,
      afterBackoff: (n, c) => { if (n === k) clock.pin(c.deadline() - 60_000); },
      reserveWrap: async (n, orig, c) => {
        if (n !== k + 1) return orig();
        clock.pin(c.deadline() + 1);
        return orig();
      },
    });
    assertMarkedHold(r, k);
    assert.equal(r.reserveErrors.length, 1);
    const error = r.reserveErrors[0];
    assert.ok(error instanceof RecoveryDeadlineExhaustedError || error instanceof RecoveryBudgetExhaustedError, String(error));
  });
}

it("a deadline abort inside a consumed iteration (the reclaim wait) is not capacity-only", async t => {
  const clock = clockOf(t);
  const r = await drive({
    used: 0.89,
    reserveWrap: async (n, orig) => {
      const reservation = await orig();
      // The third iteration runs against a clock pinned just before its deadline: its own timer aborts the wait.
      if (n === 3) clock.pin(reservation.deadline - 250);
      return reservation;
    },
    onReclaim: async (n: number) => { if (n === 3) await sleep(1500); },
  });
  assert.equal(r.reclaims, 3, "the third iteration reached its reclaim wait");
  assert.equal(r.waitErrors.length, 1);
  assert.ok(r.waitErrors[0] instanceof RecoveryDeadlineExhaustedError, "the wait was aborted by the iteration's own deadline");
  assertUnmarkedPark(r);
  assert.equal(journal(r.bare, r.clone.branch).recovery.attempts, 3);
});

it("a reservation persisted but never returned leaves no marked park and falls back to today's failure", async () => {
  const r = await drive({
    used: 0.89,
    reserveWrap: async (n, orig) => {
      const reservation = await orig();
      // The journal now holds attempt 3, but this flight never witnessed it.
      if (n === 3) throw new RecoveryBudgetExhaustedError();
      return reservation;
    },
  });
  assert.equal(r.parks.length, 0, "no park of any kind");
  assert.equal(journal(r.bare, r.clone.branch).recovery.attempts, 3);
  assert.equal(journal(r.bare, r.clone.branch).recovery.stage, "capturing", "the fence refused to block an unwitnessed episode");
  assert.equal(failedReports().length, 1, "today's generic failure");
  assert.equal(feed(r).includes(HELD_TEXT), false);
});

it("one capacity refusal followed by a non-capacity iteration failure is not capacity-only", async () => {
  const r = await drive({
    used: 0.89,
    measureError: new Error("not a filesystem error"),
    onReclaim: (n, c) => { if (n === 2) c.set(0.80); },
  });
  assertUnmarkedPark(r);
  assert.equal(journal(r.bare, r.clone.branch).recovery.attempts, 3);
});

it("a deadline with no capacity refusal at all is not capacity-only", async t => {
  const clock = clockOf(t);
  const r = await drive({
    used: 0.30,
    measureError: new Error("not a filesystem error"),
    onMeasure: c => clock.pin(c.deadline() + 1),
  });
  assertUnmarkedPark(r);
});

it("a window already charged by an earlier flight of the same claim generation is never capacity-only", async () => {
  // Flight 1 is fenced out after one refused iteration (attempts 1 stays in the journal for generation 2).
  const first = await drive({ used: 0.89, onReclaim: (n, c) => { if (n === 1) api.setOwnershipStatus(c.claim.run_id, "running", 3); } });
  assert.equal(first.parks.length, 0);
  assert.equal(journal(first.bare, first.clone.branch).recovery.attempts, 1);
  api.setOwnershipStatus(first.claim.run_id, "running", 2);
  const second = await drive({ used: 0.89, prior: first.prior });
  assertUnmarkedPark(second);
  assert.equal(journal(second.bare, second.clone.branch).recovery.attempts, 3);
});

it("a blocked-journal re-hold (no iteration) is never capacity-only", async () => {
  const first = await drive({ used: 0.89 });
  assertMarkedHold(first, 3);
  api.setOwnershipStatus(first.claim.run_id, "running", 2);
  const second = await drive({ used: 0.89, prior: first.prior });
  assert.deepEqual(second.parks, [UNMARKED]);
  assert.equal(second.reserves, 0, "the re-hold reserves no iteration");
});

it("an api that did not advertise the hold gets today's unmarked park", async () => {
  const r = await drive({ used: 0.89, features: BASE_FEATURES });
  assertUnmarkedPark(r);
  assert.equal(r.parks.length, 1);
});

it("a first-attempt 400 on the marked report is followed by exactly one confirmed unmarked report", async () => {
  const r = await drive({
    used: 0.89,
    respond: (body, n, runId) => {
      if (n === 1) throw badRequest();
      return confirmed(runId, body);
    },
  });
  assert.deepEqual(r.parks, [MARKED, UNMARKED]);
  assert.deepEqual(failedReports(), []);
  assert.equal(feed(r).includes(HELD_TEXT), false, "a fallback to the unmarked park never claims an owner hold");
});

it("an unresolved marked report followed by a marked 400 gets one unmarked attempt, whose 400 only stops", async () => {
  const r = await drive({
    used: 0.89,
    respond: (_body, n) => {
      if (n === 1) throw new Error("connection reset after send");
      throw badRequest();
    },
  });
  assert.deepEqual(r.parks, [MARKED, MARKED, UNMARKED]);
  assert.deepEqual(failedReports(), [], "an uncertain park is never turned into a failure");
  assert.equal(feed(r).includes(HELD_TEXT), false);
});

it("a 400 on both the marked and the unmarked report (no uncertainty) is today's generic failure", async () => {
  const r = await drive({ used: 0.89, respond: () => { throw badRequest(); } });
  assert.deepEqual(r.parks, [MARKED, UNMARKED]);
  assert.equal(failedReports().length, 1, "today's generic failure");
  assert.equal(feed(r).includes(HELD_TEXT), false);
});

it("a lost ACK is replayed with a byte-identical marked body and the hold is announced once", async () => {
  const r = await drive({
    used: 0.89,
    respond: (body, n, runId) => {
      if (n === 1) throw new Error("connection reset after send");
      return confirmed(runId, body);
    },
  });
  assert.equal(r.parks.length, 2);
  assert.deepEqual(r.parks[0], MARKED);
  assert.deepEqual(r.parks[1], r.parks[0]);
  assert.equal(JSON.stringify(r.parks[1]), JSON.stringify(r.parks[0]));
  assert.deepEqual(failedReports(), []);
  assert.equal(feed(r).filter(t => t === HELD_TEXT).length, 1);
});

it("a stale-claim ack on the marked report stops without a fallback report or a failure", async () => {
  const r = await drive({ used: 0.89, respond: () => ({ applied: false, staleClaim: true, reason: "stale_claim" }) as Ack });
  assert.deepEqual(r.parks, [MARKED]);
  assert.deepEqual(failedReports(), []);
  assert.equal(feed(r).includes(HELD_TEXT), false);
});

for (const [name, hook] of [
  ["a stale claim during the refusal", (c: Ctx) => api.setOwnershipStatus(c.claim.run_id, "running", 3)],
  ["an ownership loss during the refusal", (c: Ctx) => api.setOwnershipStatus(c.claim.run_id, "cancelled", 2)],
] as const) {
  it(`${name} parks nothing`, async () => {
    const r = await drive({ used: 0.89, onReclaim: (n, c) => { if (n === 1) hook(c); } });
    assert.deepEqual(r.parks, []);
    assert.equal(r.models, 0);
    assert.equal(r.releases, 0);
    assert.equal(journal(r.bare, r.clone.branch).recovery.attempts, 1, "no further iteration was charged");
    assert.equal(feed(r).includes(HELD_TEXT), false);
  });
}

it("an owner cancel during the reclaim wait is stopped by the lifecycle guard before classification", async () => {
  const r = await drive({ used: 0.89, onWait: (_n, flight) => flight.steering.markCancelled() });
  assert.deepEqual(r.parks, []);
  assert.equal(r.models, 0);
  assert.equal(journal(r.bare, r.clone.branch).recovery.attempts, 1);
  assert.equal(feed(r).includes(HELD_TEXT), false);
});

it("after an owner Resume (higher episode, next generation) with headroom a successor is seeded and nothing parks", async () => {
  const first = await drive({ used: 0.89 });
  assertMarkedHold(first, 3);
  api.setOwnershipStatus(first.claim.run_id, "running", 3);
  const second = await drive({
    used: 0.30, prior: first.prior,
    claimOverrides: { claim_generation: 3, worker_recovery_episode: 1 },
  });
  assert.deepEqual(second.parks, [], "no park after the resume");
  assert.equal(second.models, 1, "the seed proceeds to the model");
  assert.equal(second.prepares, 1);
});

it("a Codex-shaped claim (secrets.codex, self-contained seed) with all-capacity refusals is marked too", async t => {
  // Hermetic seam only, as in runner-codex-capture-proof-guarded-retained: real UID readiness needs a root fixture.
  t.mock.method(git, "normalizeRetainedSourceObjects", async () => {});
  const rig = codexRig();
  const r = await drive({
    used: 0.89,
    executor: { sandboxesCommands: true, safety: rig.safety, settleForCredentialFreeCapture: rig.settle, withCredentialFreeCapture: rig.capture },
    claimOverrides: { plan_cross_check_required: true, secrets: { forge_pat: "fixture-forge-pat-000000", codex: { auth_json: "{}" } } },
  });
  assertMarkedHold(r, 3);
});
