// issue #4046 M2 (agent): the capacity-only owner hold composed with the #3995 deferred owner `now` pause.
// The retained-recovery window defers an owner pause (deferOwnerPauseNow around recoverRetainedClaimBody); an
// exhaustion whose every witnessed reservation was a data volume capacity refusal parks as an owner hold
// (`retained_capacity_only`) while the pause stays pending server-side. A window that recovers settles the pause
// through the ordinary checkpointed pause park, after required-base validation. Real git on the real retained
// runner, the real steering dispatch (flight.steering.route), the production SdkExecutor and SummaryRunner over
// counted fakes, and a DataVolumeGuard whose statfs/stat/reclaim seams are injected (as in
// runner-retained-capacity-hold.test.ts). The clock and backoff are injected, never slept on.
import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { Readable } from "node:stream";
import { DiskGovernor, type CacheCapConfig } from "../src/cache-cap.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import type { ExecutorFactory } from "../src/runner.js";
import type { StateRequest } from "../src/protocol.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { SummaryRunner } from "../src/summary-runner.js";
import { noProofReseed, nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const BSIZE = 4096;
const BLOCKS = 10_000_000;
const FILES = 10_000_000;
const HOLD = "retained_recovery_capacity_hold_v1";
const FEATURES = ["claim_generation_fence", "recovery_cause_data_volume_full", "recovery_cause_retained_recovery_exhausted", HOLD];
const GOVERNOR_ON: CacheCapConfig = {
  capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 2, hardStopEnabled: true, hardMargin: 0.03,
};
const HELD_TEXT = "retained recovery held for owner: every recovery attempt in this window was refused for data volume capacity; owner Resume required; predecessor work and custody retained";
const UNMARKED = { status: "recovery_wait", recovery_cause: "retained_recovery_exhausted", claim_generation: 2 };
const MARKED = { ...UNMARKED, retained_capacity_only: true };
const SEEDED_PLAN = { plan_md: "# Plan\n- change the thing", plan_approved: true, plan_source: "seeded",
  summary_model: "fixture-summary-model", summary_intent_present: false };

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
const journal = (bare: string, branch: string): any => JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

type Ack = Awaited<ReturnType<typeof client.reportState>>;
interface Rig {
  claim: Awaited<ReturnType<typeof seed>>["claim"];
  bare: string;
  clone: Awaited<ReturnType<typeof seed>>["clone"];
  flight: any;
  runner: any;
  logs: ReturnType<typeof recordingLogger>;
  state: { used: number };
  counts: { sdk: number; summary: number; prepares: number; reclaims: number; reserves: number; publishes: number; releases: number };
  /** Chronological milestones: "validate" (required-base planned-commit resolution), "publish", "paused". */
  events: string[];
  /** Every exhausted-park body, in order. */
  parks: StateRequest[];
  /** Every `paused` body the worker attempted (whether or not the fake server saw it). */
  pausedAttempts: number;
  serverPaused: number;
  serverPauseFailed: number;
}
interface Opts {
  used: number;
  claim?: Record<string, unknown>;
  /** Runs in the n-th (1-based) retained seed-admission reclaim wait, with the live flight. */
  onWait?: (n: number, rig: Rig) => void;
  onReclaim?: (n: number, rig: Rig) => void;
  /** Runs as the n-th (1-based) recovery reservation starts: headroom returning between iterations. */
  onReserve?: (n: number, rig: Rig) => void;
  /** Answer the n-th (1-based) exhausted-park report; default confirms it. */
  respond?: (body: StateRequest, n: number, runId: string) => Ack | Promise<Ack>;
  /** Replaces the worker's answer to a `paused` report (the server never sees it). */
  pausedAnswer?: () => Ack | Promise<Ack>;
  /** Runs before recoverRetainedClaim opens the deferral window. */
  beforeWindow?: (rig: Rig) => void;
  /** The checkpoint attests the adopted retained source, so required-base validation passes (as after a prior published checkpoint). */
  attestBase?: boolean;
}

const route = (flight: any, kind: string, body?: string) => {
  // The owner's pause request is a durable server-side column; the server model reads it when `paused` is reported.
  if (kind === "pause") flight.pauseRequestedModel = true;
  if (kind === "pause_cancel") flight.pauseRequestedModel = false;
  return flight.steering.route(kind, body, 1, false, undefined);
};

async function build(o: Opts): Promise<Rig> {
  const { claim, bare, clone } = await seed();
  Object.assign(claim, SEEDED_PLAN, o.claim ?? {});
  client.protocolFeatures = FEATURES;
  const retainedHome = path.join(homeDir, claim.run_id);
  fs.mkdirSync(retainedHome, { recursive: true });
  fs.writeFileSync(path.join(retainedHome, "session"), "predecessor session");
  const rig = {
    claim, bare, clone, flight: undefined, logs: recordingLogger(), state: { used: o.used },
    counts: { sdk: 0, summary: 0, prepares: 0, reclaims: 0, reserves: 0, publishes: 0, releases: 0 },
    events: [] as string[], parks: [] as StateRequest[], pausedAttempts: 0, serverPaused: 0, serverPauseFailed: 0,
  } as unknown as Rig;
  const prep = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => { rig.counts.prepares++; return prep(...args); };
  const reserve = git.reserveRecoveryIteration.bind(git);
  git.reserveRecoveryIteration = async (...args) => { const n = ++rig.counts.reserves; o.onReserve?.(n, rig); return reserve(...args); };
  const planned = claim.planned_base_commit as string | undefined;
  const resolveUnique = git.resolveCommitUnique.bind(git);
  git.resolveCommitUnique = (async (...args: Parameters<typeof resolveUnique>) => {
    if (planned !== undefined && args[1] === planned) rig.events.push("validate");
    return resolveUnique(...args);
  }) as typeof git.resolveCommitUnique;
  client.publishCheckpoint = async (_run, _tip, pack) => {
    await drain(pack);
    rig.counts.publishes++;
    rig.events.push("publish");
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } } as any;
  };
  client.releaseRecoveryCustody = async () => { rig.counts.releases++; throw new Error("must retain custody"); };
  // The fake API acks any status verbatim. Model the server's SetRunPaused: `paused` is applied only while an
  // owner pause request is pending; otherwise the ACK answers the run's own status (running).
  const baseReport = client.reportState.bind(client);
  client.reportState = (async (runId: string, body: StateRequest, signal?: AbortSignal) => {
    if (runId === claim.run_id && body.status === "recovery_wait" && body.recovery_cause === "retained_recovery_exhausted") {
      rig.parks.push(body);
      if (o.respond) return o.respond(body, rig.parks.length, runId);
      api.setOwnershipStatus(runId, "recovery_wait", body.claim_generation as number);
      return { applied: true, status: "recovery_wait", recoveryCause: "retained_recovery_exhausted" } as Ack;
    }
    if (runId === claim.run_id && body.status === "paused") {
      rig.pausedAttempts++;
      if (o.pausedAnswer) return o.pausedAnswer();
    }
    const ack = await baseReport(runId, body, signal);
    if (runId !== claim.run_id) return ack;
    if (body.status === "paused") {
      if (rig.flight?.pauseRequestedModel === true) {
        rig.flight.pauseRequestedModel = false;
        rig.serverPaused++;
        rig.events.push("paused");
        return ack;
      }
      return { ...ack, status: "running" } as Ack;
    }
    if (body.status === "pause_failed") { if (rig.flight) rig.flight.pauseRequestedModel = false; rig.serverPauseFailed++; }
    return ack;
  }) as typeof client.reportState;

  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs: () => {
      const free = Math.round(BLOCKS * (1 - rig.state.used));
      return { bsize: BSIZE, blocks: BLOCKS, bfree: free, bavail: free, files: FILES, ffree: Math.round(FILES * 0.8) };
    },
    stat: async () => ({ dev: 7 }),
    reclaim: async () => { rig.counts.reclaims++; o.onReclaim?.(rig.counts.reclaims, rig); },
  });
  const diskGovernor = new DiskGovernor({
    config: GOVERNOR_ON, log: nullLogger(), volumeTotalBytes: () => BLOCKS * BSIZE, thresholdOf: () => 0.9,
  });
  const factory: ExecutorFactory = runId => {
    const homeForRun = path.join(homeDir, runId);
    const summaryQuery: SdkQueryFn = () => { rig.counts.summary++; throw new Error("the summary model must not be reached"); };
    const summaryRunner = new SummaryRunner(nullLogger(), { queryFn: summaryQuery, homeRoot: homeForRun });
    const queryFn: SdkQueryFn = () => { rig.counts.sdk++; throw new Error("the model must not be reached"); };
    return { homeDir: homeForRun, executor: new SdkExecutor(nullLogger(), homeForRun, { queryFn, client, summaryRunner }) };
  };
  const runner: any = runnerWith(factory, fakeGitlab().gitlab, undefined, rig.logs.logger,
    { recoveryRetryMs: 1, dataVolume: guard, diskGovernor });
  rig.runner = runner;
  const phaseClone = runner.phaseClone.bind(runner);
  runner.phaseClone = async (...args: any[]) => { rig.flight = args[1]; return phaseClone(...args); };
  let waits = 0;
  const wait = runner.awaitDataVolumeReclaim.bind(runner);
  runner.awaitDataVolumeReclaim = (g: unknown, flight: any, op: string, signal?: AbortSignal) => {
    rig.flight = flight;
    if (op === "retained_seed_admission") o.onWait?.(++waits, rig);
    return wait(g, flight, op, signal);
  };
  if (o.attestBase) {
    const handoff = runner.phasePreflightHandoff.bind(runner);
    runner.phasePreflightHandoff = async (...args: any[]) => {
      const clone = args[1].runnerClone;
      args[1].checkpointClaimEvidence = Object.freeze({ attestedSource: clone.adoptedContentSource ?? clone.adoptedSource });
      return handoff(...args);
    };
  }
  if (o.beforeWindow) {
    const recover = runner.recoverRetainedClaim.bind(runner);
    runner.recoverRetainedClaim = async (...args: any[]) => { o.beforeWindow!(rig); return recover(...args); };
  }
  return rig;
}
async function drive(o: Opts): Promise<Rig> {
  const rig = await build(o);
  await rig.runner.execute(rig.claim);
  return rig;
}

const bodies = (rig: Rig) => api.states.filter(s => s.runId === rig.claim.run_id).map(s => s.body.status);
const count = (rig: Rig, status: string) => bodies(rig).filter(s => s === status).length;
const feed = (rig: Rig) => api.messages(rig.claim.run_id).filter(m => m.kind === "status").map(m => String((m.payload as { text?: unknown }).text));
const logged = (rig: Rig, re: RegExp) => rig.logs.lines.some((l: any) => re.test(String(l.msg)));
const retainedFileIntact = (rig: Rig) =>
  fs.readFileSync(path.join(journal(rig.bare, rig.clone.branch).clonePath, "retained.txt"), "utf8") === "only local dirty work\n";
/** The only /state bodies a refused window may send for the run: running reports, never failed/paused/pause_failed. */
function assertNoFailureOrPause(rig: Rig): void {
  for (const bad of ["failed", "paused", "pause_failed", "completed"]) assert.equal(count(rig, bad), 0, `no ${bad} body`);
}
function assertNoModel(rig: Rig): void {
  assert.equal(rig.counts.sdk, 0, "no provider call");
  assert.equal(rig.counts.summary, 0, "no summary pass");
}

it("1. a deferred now pause + all-capacity exhaustion holds for the owner; the pause stays pending and nothing is seeded", async () => {
  const rig = await drive({ used: 0.89, onWait: (n, r) => { if (n === 1) route(r.flight, "pause", "now"); } });
  assert.equal(rig.flight.steering.getPauseMode(), "now", "the pause was recorded in the window");
  assert.deepEqual(rig.parks, [MARKED], "exactly one marked exhausted park");
  assert.equal(rig.flight.parked, true, "the marked park was confirmed");
  assert.equal(feed(rig).filter(t => t === HELD_TEXT).length, 1);
  assert.equal(rig.pausedAttempts, 0, "no paused report was even attempted");
  assert.equal(rig.serverPaused, 0);
  assert.equal(rig.serverPauseFailed, 0);
  assert.equal(rig.flight.pauseRequestedModel, true, "the owner pause is still pending server-side");
  assertNoFailureOrPause(rig);
  assertNoModel(rig);
  assert.equal(rig.counts.publishes, 0, "no pause checkpoint was published");
  assert.equal(rig.counts.prepares, 0, "no successor seeded");
  assert.equal(rig.counts.releases, 0, "custody retained");
  assert.equal(rig.counts.reserves, 3);
  const j = journal(rig.bare, rig.clone.branch);
  assert.equal(j.clonePath, rig.clone.path, "no successor replaces the predecessor");
  assert.equal(j.recovery.stage, "blocked");
  assert.equal(j.recovery.attempts, 3);
  assert.ok(retainedFileIntact(rig));
  assert.equal(logged(rig, /^retained recovery stopped; source and accounting kept$/), true, "recovery ended through the retained stop");
});

it("2. capacity refusal then headroom: required-base validation, then the checkpoint, then the confirmed paused report", async () => {
  const planned = command(fx.originPath, "rev-parse", "HEAD");
  const rig = await drive({
    used: 0.89,
    claim: { planned_base_commit: planned, require_base_match: true },
    attestBase: true,
    onWait: (n, r) => { if (n === 1) route(r.flight, "pause", "now"); },
    onReserve: (n, r) => { if (n === 2) r.state.used = 0.30; },
  });
  assert.equal(rig.counts.reserves, 2, "one refused iteration, then one that seeded");
  assert.equal(rig.counts.prepares, 1, "the successor was seeded");
  assert.deepEqual(rig.parks, [], "no exhausted park");
  assert.equal(rig.counts.reclaims, 1, "only the first iteration needed a reclaim");
  // The leading publish is the predecessor recovery publication; the validation is then followed by the pause checkpoint and the report.
  assert.deepEqual(rig.events, ["publish", "validate", "publish", "paused"]);
  assert.equal(count(rig, "paused"), 1, "exactly one paused body");
  assert.equal(rig.serverPaused, 1);
  assert.equal(rig.flight.parked, true);
  assert.equal(count(rig, "failed"), 0);
  assert.equal(count(rig, "pause_failed"), 0);
  assert.equal(feed(rig).includes(HELD_TEXT), false);
  assertNoModel(rig);
});

const NO_PAUSE_STATUSES = ["running"];
it("3a. control: with no pause the all-capacity window parks once, marked", async () => {
  const rig = await drive({ used: 0.89 });
  assert.deepEqual(rig.parks, [MARKED]);
  assert.deepEqual(bodies(rig), NO_PAUSE_STATUSES);
  assert.equal(feed(rig).filter(t => t === HELD_TEXT).length, 1);
});

it("3a. a pause withdrawn during the window leaves the all-capacity outcome exactly as without a pause", async () => {
  const rig = await drive({
    used: 0.89,
    onWait: (n, r) => { if (n === 1) { route(r.flight, "pause", "now"); route(r.flight, "pause_cancel"); } },
  });
  assert.deepEqual(rig.parks, [MARKED]);
  assert.deepEqual(bodies(rig), NO_PAUSE_STATUSES, "the same /state statuses as the no-pause control");
  assert.equal(feed(rig).filter(t => t === HELD_TEXT).length, 1);
  assert.equal(rig.pausedAttempts, 0);
  assert.equal(rig.flight.steering.deferredPauseNowPending(), false, "the withdrawn pause is no longer deferred");
  assertNoFailureOrPause(rig);
  assertNoModel(rig);
});

it("3b. a pause withdrawn during a window that then recovers sends no paused report and reaches the model", async () => {
  const rig = await drive({
    used: 0.89,
    onWait: (n, r) => { if (n === 1) { route(r.flight, "pause", "now"); route(r.flight, "pause_cancel"); } },
    onReserve: (n, r) => { if (n === 2) r.state.used = 0.30; },
  });
  assert.deepEqual(rig.parks, []);
  assert.equal(rig.pausedAttempts, 0);
  assert.equal(count(rig, "paused"), 0);
  assert.equal(rig.flight.parked === true, false);
  assert.ok(rig.counts.sdk >= 1, "the run proceeds to the provider, as it would without a pause");
  assert.equal(rig.counts.prepares, 1);
});

it("4. an immediate (non-deferred) pause stop in the window behaves as today: no marked park, no paused report", async () => {
  // Routed before recoverRetainedClaim opens its deferral window: the same immediate-stop path a
  // publication-only claim takes, because that claim never opens the window (see runner-retained-recovery-pause).
  const rig = await drive({ used: 0.89, beforeWindow: r => route(r.flight, "pause", "now") });
  assert.deepEqual(rig.parks, [], "no exhausted park of any kind");
  assert.equal(feed(rig).includes(HELD_TEXT), false);
  assert.equal(rig.pausedAttempts, 0);
  assert.equal(count(rig, "paused"), 0);
  assert.equal(rig.flight.parked === true, false);
  assert.equal(rig.counts.prepares, 0);
  assert.equal(rig.counts.releases, 0);
  assertNoModel(rig);
  assert.ok(logged(rig, /^retained recovery stopped; source and accounting kept$/), "today's retained stop arm");
  assert.ok(logged(rig, /^retained recovery stopped with an owner pause pending; preserving/), "truthful retention cause");
  assert.ok(retainedFileIntact(rig));
});

for (const kind of ["cancel", "pause-and-cancel"] as const) {
  it(`5. an owner cancel before classification (${kind}) parks nothing and carries no marker`, async () => {
    const rig = await drive({
      used: 0.89,
      onWait: (n, r) => {
        if (n !== 1) return;
        if (kind === "pause-and-cancel") route(r.flight, "pause", "now");
        route(r.flight, "cancel");
      },
    });
    assert.deepEqual(rig.parks, [], "no exhausted park, so no marker");
    assert.equal(feed(rig).includes(HELD_TEXT), false);
    assert.equal(rig.pausedAttempts, 0);
    assert.equal(rig.flight.parked === true, false);
    assert.equal(rig.counts.reserves, 1, "no further iteration was charged after the cancel");
    assert.equal(rig.counts.prepares, 0);
    assert.equal(rig.counts.releases, 0);
    assertNoModel(rig);
    assert.ok(retainedFileIntact(rig));
    assert.ok(logged(rig, /^retained recovery stopped; source and accounting kept$/));
  });
}

for (const answer of ["throw", "running", "cancelled"] as const) {
  it(`6a. a paused report that ${answer === "throw" ? "is lost (transport error)" : `is answered ${answer}`} is never a confirmed pause`, async () => {
    const rig = await drive({
      used: 0.89,
      onWait: (n, r) => { if (n === 1) route(r.flight, "pause", "now"); },
      onReserve: (n, r) => { if (n === 2) r.state.used = 0.30; },
      pausedAnswer: () => {
        if (answer === "throw") throw new Error("report transport failed");
        return { applied: true, status: answer } as Ack;
      },
    });
    assert.deepEqual(rig.parks, [], "headroom returned, so no exhausted park");
    assert.ok(rig.pausedAttempts >= 1, "the pause park was attempted");
    assert.equal(count(rig, "paused"), 0, "no paused body reached the server");
    assert.equal(rig.serverPaused, 0);
    assert.equal(rig.flight.parked === true, false, "the worker is not parked");
    assert.equal(rig.flight.pauseRequestedModel, true, "the pause is still pending server-side");
    assertNoModel(rig);
    assert.equal(feed(rig).includes(HELD_TEXT), false);
    assert.ok(retainedFileIntact(rig));
  });
}

it("6b. a lost ACK on the marked hold is replayed byte-identically and the pending pause is never reported as paused", async () => {
  const rig = await drive({
    used: 0.89,
    onWait: (n, r) => { if (n === 1) route(r.flight, "pause", "now"); },
    respond: (body, n, runId) => {
      if (n === 1) throw new Error("connection reset after send");
      api.setOwnershipStatus(runId, "recovery_wait", body.claim_generation as number);
      return { applied: true, status: "recovery_wait", recoveryCause: "retained_recovery_exhausted" } as Ack;
    },
  });
  assert.equal(rig.parks.length, 2);
  assert.deepEqual(rig.parks[0], MARKED);
  assert.equal(JSON.stringify(rig.parks[1]), JSON.stringify(rig.parks[0]));
  assert.equal(feed(rig).filter(t => t === HELD_TEXT).length, 1, "the hold is announced once");
  assert.equal(rig.pausedAttempts, 0);
  assert.equal(rig.flight.pauseRequestedModel, true, "the pause is still pending server-side");
  assertNoFailureOrPause(rig);
  assertNoModel(rig);
});

it("6c. a refused marked hold (stale-claim ACK) with a pending pause neither pauses nor fails the run", async () => {
  const rig = await drive({
    used: 0.89,
    onWait: (n, r) => { if (n === 1) route(r.flight, "pause", "now"); },
    respond: () => ({ applied: false, staleClaim: true, reason: "stale_claim" }) as Ack,
  });
  assert.deepEqual(rig.parks, [MARKED]);
  assert.equal(rig.flight.parked === true, false);
  assert.equal(rig.pausedAttempts, 0);
  assert.equal(feed(rig).includes(HELD_TEXT), false);
  assertNoFailureOrPause(rig);
  assertNoModel(rig);
});
