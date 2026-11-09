import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { Readable } from "node:stream";
import type { ExecutorFactory } from "../src/runner.js";
import type { RunContext } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { SummaryRunner } from "../src/summary-runner.js";
import { RunRunner } from "../src/runner.js";
import { GitCache } from "../src/git.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { testLockOptions } from "./authority-test-support.js";
import { noProofReseed, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, TOKEN } from "./runner-harness.js";

// An owner `now` pause delivered while retained re-claim recovery runs (before the model starts) is
// recorded, not acted on, and settled afterwards through the ordinary checkpointed pause park.
// Everything below uses the production RunRunner, real Git retained seeding, the real steering
// dispatch (flight.steering.route) fired from a hook on a recovery Git operation, and the real
// handlePausePark. The harness executor factory stands in for the model: every case counts its calls.

installHarness();

function readJournal(bare: string, branch: string): any {
  return JSON.parse(execFileSync("git", ["-C", bare, "config", `uzi-recovery.${branch}.clone`], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" }, encoding: "utf8",
  }).trim());
}

async function seed(iid = 3995, overrides: Record<string, unknown> = {}, prompt = false) {
  const claim = gitlabClaim(iid, { claim_generation: 2, session_id: "predecessor-session",
    ...(prompt ? { kind: "prompt", issue_iid: null } : {}), ...overrides });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = prompt
    ? await git.runnerCloneForBranch(bare, `uzi/prompt-${claim.run_id}`, `uzi-prompt-${claim.run_id}`, noProofReseed, claim.run_id)
    : await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone };
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

type Hook = "pre" | "post" | "reserve" | "capture" | "successor" | "evidence" | "none";
interface Rig {
  models: number;
  sdkCalls: number;
  summaryCalls: number;
  reserves: number;
  publishes: number;
  pauseParks: number;
  /** The server's SetRunPaused model: a pause request is pending, and how many writers applied. */
  serverPending: boolean;
  serverPaused: number;
  serverPauseFailed: number;
  flight: any;
  runner: ReturnType<typeof runnerWith>;
  logs: ReturnType<typeof recordingLogger>;
  claim: Awaited<ReturnType<typeof seed>>["claim"];
  bare: string;
  clone: Awaited<ReturnType<typeof seed>>["clone"];
  fired: number;
}
interface RigOptions {
  iid?: number;
  /** Seed and claim a `prompt`-kind run (branch uzi/prompt-<run-id>, no issue iid). */
  prompt?: boolean;
  failPublish?: boolean;
  claim?: Record<string, unknown>;
  /** Route an owner `now` pause inside the recovery window (first recovery reservation). */
  windowPause?: boolean;
  /** The harness executor refuses command settlement, so recovery fails with a quiescence blocker. */
  refuseSettlement?: boolean;
  /** Use the production SdkExecutor over a counted fake SDK query and a counted fake SummaryRunner. */
  production?: boolean;
  protocolFeatures?: string[];
  /** Wrap the worker client's state report (outside the server model: a throw here is a lost ACK). */
  report?: (body: any, original: () => Promise<any>) => Promise<any>;
  /** Run under a test-owned production AuthorityRuntime and an authority-required GitCache. */
  authority?: boolean;
  /** The pause-park quiescence proof reports survivors (the clone is not provably quiescent). */
  quiesceBlocked?: boolean;
  /** Run once, with the live flight, inside the first retained-recovery `ensureClone`, before it resolves. */
  duringEnsureClone?: (flight: any, runner: any) => void;
  /** Run once, with the live flight, immediately before the executor's run (after the preflight handoff). */
  beforeExecutorRun?: (flight: any, runner: any) => void;
  /** Run after each pause-park attempt (1-based count) has returned, with the live flight. */
  afterParkAttempt?: (attempt: number, flight: any) => void;
}

/** Build a runner over a seeded retained clone. `deliver` runs once, at the chosen seam, with the live flight. */
async function rig(hook: Hook, deliver: (flight: any, runner: any) => void, opts: RigOptions = {}): Promise<Rig> {
  const { claim, bare, clone } = await seed(opts.iid ?? 3995, opts.claim, opts.prompt === true);
  client.protocolFeatures = opts.protocolFeatures ?? ["claim_generation_fence"];
  const retainedHome = path.join(homeDir, claim.run_id);
  fs.mkdirSync(retainedHome, { recursive: true });
  fs.writeFileSync(path.join(retainedHome, "session"), "predecessor session");
  const state = {
    models: 0, sdkCalls: 0, summaryCalls: 0, reserves: 0, publishes: 0, pauseParks: 0, fired: 0,
    serverPending: (opts.claim as any)?.pause_pending === true, serverPaused: 0, serverPauseFailed: 0,
  } as Rig;
  const logs = recordingLogger();
  let executorRuns = 0;
  const beforeRun = (r: any) => { if (executorRuns++ === 0) opts.beforeExecutorRun?.(state.flight, r); };
  let runner: RunRunner;
  const makeExecutor: ExecutorFactory = runId => {
    const homeForRun = path.join(homeDir, runId);
    if (opts.production) {
      // The production SummaryRunner over a counted fake summary query: any summary pass is a counted call.
      const summaryQuery: SdkQueryFn = () => { state.summaryCalls++; throw new Error("the summary model must not be reached"); };
      const summaryRunner = new SummaryRunner(nullLogger(), { queryFn: summaryQuery, homeRoot: homeForRun });
      const queryFn: SdkQueryFn = () => { state.sdkCalls++; throw new Error("the model must not be reached"); };
      const executor = new SdkExecutor(nullLogger(), homeForRun, { queryFn, client, summaryRunner });
      const run = executor.run.bind(executor);
      executor.run = async ctx => { beforeRun(runner); return run(ctx); };
      return { homeDir: homeForRun, executor };
    }
    return { homeDir: homeForRun, executor: {
      run: async (_ctx: RunContext) => { beforeRun(runner); state.models++; throw new Error("fixture ends the model run"); },
      ...(opts.refuseSettlement ? { settleWorkerCommands: async () => ({ confirmed: false }) } : {}),
    } as any };
  };
  // The cache the runner uses: the shared harness cache, or an authority-required one under a
  // test-owned production AuthorityRuntime (same data dir, so the seeded retained clone is found).
  let g: GitCache = git;
  if (opts.authority) {
    const lifetime = await acquireWorkerLifetime(fs.realpathSync(fx.dataDir), testLockOptions());
    const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
    g = new GitCache(fs.realpathSync(fx.dataDir), nullLogger(), undefined, testGitCacheOptions({
      authority: rt.hooks, authorityRequired: true, gitleaksBin: defaultGitleaksShim(),
    }));
    runner = new RunRunner(client, g, makeExecutor, logs.logger, 20, undefined, {
      pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, prDescriptionHeadLagMs: 0,
      gitlab: fakeGitlab().gitlab, teardownTestDeps: portableTeardownTestDeps,
      authority: rt, recoveryRetryMs: 1, checkpointTestHooks: { retryBackoffMs: [0, 0] },
    });
  } else {
    runner = runnerWith(makeExecutor, fakeGitlab().gitlab, undefined, logs.logger, { recoveryRetryMs: 1 });
  }
  const r = runner;
  const once = () => { if (state.fired++ === 0) deliver(state.flight, r); };
  const phaseClone = (r as any).phaseClone.bind(r);
  (r as any).phaseClone = async (...args: any[]) => { state.flight = args[1]; return phaseClone(...args); };
  const park = (r as any).handlePausePark.bind(r);
  (r as any).handlePausePark = async (...args: any[]) => {
    const attempt = ++state.pauseParks;
    try { return await park(...args); } finally { opts.afterParkAttempt?.(attempt, state.flight); }
  };
  if (opts.quiesceBlocked) {
    const quiesce = (r as any).quiesceRun.bind(r);
    (r as any).quiesceRun = async (...args: any[]) => args[2]?.site === "pause_park"
      ? { blocked: true, outcome: {
        process: { state: "survivors", processes: [], killed: [], detail: "fixture survivor" },
        docker: { state: "not_wired", removed: [], detail: "" },
      } }
      : quiesce(...args);
  }
  if (hook === "pre") {
    const recover = (r as any).recoverRetainedClaim.bind(r);
    (r as any).recoverRetainedClaim = async (...args: any[]) => { once(); return recover(...args); };
  }
  if (hook === "post") {
    const recover = (r as any).recoverRetainedClaim.bind(r);
    (r as any).recoverRetainedClaim = async (...args: any[]) => { const out = await recover(...args); once(); return out; };
  }
  if (hook === "capture") {
    // Inside the predecessor's recovery capture, between the reservation and the successor handoff.
    const capture = (r as any).captureRecoveryRestorePoint.bind(r);
    (r as any).captureRecoveryRestorePoint = async (...args: any[]) => { once(); return capture(...args); };
  }
  const reserve = g.reserveRecoveryIteration.bind(g);
  let windowRouted = false;
  g.reserveRecoveryIteration = async (...args) => {
    state.reserves++;
    if (opts.windowPause && !windowRouted) { windowRouted = true; route(state.flight, "pause", "now"); }
    if (hook === "reserve") once();
    return reserve(...args);
  };
  if (opts.duringEnsureClone) {
    const ensure = g.ensureClone.bind(g);
    let injected = false;
    g.ensureClone = (async (...args: Parameters<typeof ensure>) => {
      const pending = ensure(...args);
      if (!injected && state.flight?.retainedEpisode) { injected = true; opts.duringEnsureClone!(state.flight, r); }
      return pending;
    }) as typeof g.ensureClone;
  }
  const adopt = g.prepareRecoverySuccessor.bind(g);
  g.prepareRecoverySuccessor = async (...args) => {
    const result = await adopt(...args);
    if (hook === "successor") once();
    return result;
  };
  if (hook === "evidence") {
    const record = (r as any).recordRecoveryGenerationEvidence.bind(r);
    (r as any).recordRecoveryGenerationEvidence = async (...args: any[]) => {
      once();
      return record(...args);
    };
  }
  client.publishCheckpoint = async (_run, _tip, pack) => {
    await drain(pack);
    state.publishes++;
    if (opts.failPublish) return { ok: false, httpStatus: 500 } as any;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } } as any;
  };
  // The fake API acks any status verbatim. Model the server's SetRunPaused: `paused` is applied only
  // while an owner pause request is pending (pause_requested_at set); otherwise the ACK answers the
  // run's own status (running). `pause_failed` clears the request. `wall` rides its own writer and
  // is accepted. The model applies only after the underlying report resolved.
  const baseReport = client.reportState.bind(client);
  const pending = () => state.serverPending || state.flight?.pauseRequestedModel === true;
  const settle = () => { state.serverPending = false; if (state.flight) state.flight.pauseRequestedModel = false; };
  client.reportState = (async (...args: any[]) => {
    const ack = await (baseReport as any)(...args);
    const body = args[1];
    if (args[0] !== claim.run_id) return ack;
    if (body.status === "paused") {
      if (pending()) { settle(); state.serverPaused++; return ack; }
      return { ...ack, status: "running" };
    }
    if (body.status === "pause_failed") { settle(); state.serverPauseFailed++; }
    return ack;
  }) as typeof client.reportState;
  if (opts.report) {
    const report = client.reportState.bind(client);
    client.reportState = (async (...args: any[]) =>
      opts.report!(args[1], () => (report as any)(...args))) as typeof client.reportState;
  }
  Object.assign(state, { runner: r, logs, claim, bare, clone });
  return state;
}

const statuses = (rg: Rig) => api.states.filter(s => s.runId === rg.claim.run_id).map(s => s.body.status);
const route = (flight: any, kind: string, body?: string) => {
  // The owner's pause request is a durable server-side column; the model reads it when `paused` is reported.
  if (kind === "pause") flight.pauseRequestedModel = true;
  if (kind === "pause_cancel") flight.pauseRequestedModel = false;
  return flight.steering.route(kind, body, 1, false, undefined);
};

/** Custody kept: the journal's clone exists with the seeded dirty file's exact bytes, and the run HOME survives. */
const custodyKept = (rg: Rig) => {
  const clonePath = readJournal(rg.bare, rg.clone.branch).clonePath;
  const file = path.join(clonePath, "retained.txt");
  return fs.existsSync(clonePath) && fs.existsSync(file) && fs.readFileSync(file, "utf8") === "only local dirty work\n" &&
    fs.existsSync(path.join(homeDir, rg.claim.run_id));
};

const logged = (rg: Rig, re: RegExp) => rg.logs.lines.some((l: any) => re.test(String(l.msg)));
const pausedReports = (rg: Rig) => statuses(rg).filter(s => s === "paused").length;

it("control: a run with no pause reaches the model after one recovery reservation", async () => {
  const rg = await rig("none", () => undefined);
  await rg.runner.execute(rg.claim);
  assert.equal(rg.models, 1);
  assert.equal(rg.reserves, 1);
  assert.equal(pausedReports(rg), 0);
});

// Issue #4116 (characterization: expected to pass on base too). A prompt run's checkpoint key is
// uzi/prompt-<run-id>; the deferred owner `now` pause must behave exactly as for an issue run.
it("prompt kind: a deferred now pause waits for retained recovery, then parks once with a published checkpoint and no model call", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), { prompt: true });
  assert.equal(rg.clone.branch, `uzi/prompt-${rg.claim.run_id}`);
  await rg.runner.execute(rg.claim);
  assert.equal(rg.fired, 1, "the pause was delivered inside recovery");
  assert.equal(pausedReports(rg), 1, "exactly one paused report");
  assert.equal(rg.flight.parked, true);
  assert.equal(rg.models, 0, "no model call after the owner pause");
  assert.equal(rg.reserves, 1, "recovery completed (one reservation) before the park");
  assert.equal(rg.publishes >= 1, true, "the pause park published a checkpoint first");
  assert.equal(statuses(rg).includes("pause_failed" as any), false);
  assert.equal(statuses(rg).includes("failed"), false);
});

it("prompt kind: a refused paused ACK is not a parked run and keeps custody", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
    prompt: true,
    report: async (body, original) => body.status === "paused" ? ({ status: "running" } as any) : original(),
  });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.pauseParks, 1);
  assert.equal(rg.flight.parked === true, false);
  assert.equal(rg.models, 0);
  assert.ok(custodyKept(rg));
});

const SEAM_LABEL = { reserve: "before capture", capture: "during the predecessor capture", successor: "after the successor handoff" } as const;
for (const seam of ["reserve", "capture", "successor"] as const) {
  it(`a delivered now pause during retained recovery (${SEAM_LABEL[seam]}) parks with one paused report and no model call`, async () => {
    const rg = await rig(seam, flight => route(flight, "pause", "now"));
    await rg.runner.execute(rg.claim);
    assert.equal(rg.fired, 1, "the pause was delivered inside recovery");
    assert.equal(pausedReports(rg), 1, "exactly one paused report");
    assert.equal(rg.flight.parked, true);
    assert.equal(rg.models, 0, "no model call after the owner pause");
    assert.equal(rg.reserves, 1, "recovery spent the same single reservation as the no-pause control");
    assert.equal(rg.publishes >= 1, true, "the pause park published a checkpoint first");
    assert.equal(statuses(rg).includes("pause_failed" as any), false);
    assert.equal(statuses(rg).includes("failed"), false);
    assert.equal(logged(rg, /retained recovery stopped/), false, "recovery was not stopped");
  });
}

it("pause_cancel inside the recovery window withdraws the deferred pause and the run proceeds to the model", async () => {
  const rg = await rig("reserve", flight => { route(flight, "pause", "now"); route(flight, "pause_cancel"); });
  await rg.runner.execute(rg.claim);
  assert.equal(pausedReports(rg), 0);
  assert.equal(rg.flight.parked, false);
  assert.equal(rg.models, 1);
});

for (const control of ["cancel", "wall", "switch", "fence", "shutdown", "shutdownReal", "disk", "abort"] as const) {
  it(`a stronger control inside the window keeps its own outcome: ${control}`, async () => {
    const rg = await rig("reserve", (flight, runner) => {
      route(flight, "pause", "now");
      if (control === "shutdownReal") runner.shutdown();
      if (control === "disk") flight.steering.requestDiskStop();
      if (control === "abort") flight.cancel.abort(new Error("x"));
      if (control === "cancel") route(flight, "cancel");
      if (control === "wall") route(flight, "pause", "wall");
      if (control === "switch") flight.steering.tripCredentialSwitch(2);
      if (control === "fence") flight.steering.claimFence = () => "superseded";
      if (control === "shutdown") runner.shuttingDownGlobal = true;
    });
    await rg.runner.execute(rg.claim);
    assert.equal(rg.models, 0, "stopped before the model");
    assert.equal(pausedReports(rg), 0, "no paused report");
    assert.equal(rg.flight.parked === true, false);
    if (control === "cancel") assert.equal(statuses(rg).includes("failed"), true, "cancellation failure reported");
    else assert.equal(statuses(rg).includes("failed"), false);
    assert.ok(custodyKept(rg), "custody kept");
    if (["wall", "cancel", "shutdown", "shutdownReal", "disk", "abort"].includes(control))
      assert.ok(logged(rg, /^retained recovery stopped; source and accounting kept$/), "retained stop arm");
    if (control === "switch") assert.ok(logged(rg, /retained credential switch stopped/), "switch arm");
  });
}

it("publication refusal reports pause_failed, no paused, and leaves the run for requeue with custody", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), { failPublish: true });
  await rg.runner.execute(rg.claim);
  assert.equal(pausedReports(rg), 0);
  assert.equal(statuses(rg).includes("pause_failed" as any), true);
  assert.equal(rg.flight.parked, false);
  assert.equal(rg.models, 0);
  assert.ok(custodyKept(rg), "custody kept");
});

it("a claim-seeded now pause keeps its existing behaviour (characterization)", async () => {
  const rg = await rig("none", () => undefined, { claim: { pause_pending: true, pause_mode: "now" } });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.flight.steering.deferredPauseNowPending(), false, "a seeded pause never sets the deferred flag");
  // The runner does not settle a seeded pause itself: the executor honours it at its first loop boundary.
  assert.equal(rg.models, 1, "a seeded pause still reaches the executor, which owns its handling");
  assert.equal(pausedReports(rg), 0, "the runner settlement arm is not used for a seeded pause");
});


// A. The production executor: the model and summary seams are counted fakes and must stay untouched.
it("production SdkExecutor: a now pause delivered before capture makes no SDK and no summary call", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
    production: true, claim: { summary_model: "fixture-summary-model", summary_intent_present: false },
  });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.fired, 1);
  assert.equal(pausedReports(rg), 1);
  assert.equal(rg.flight.parked, true);
  assert.equal(rg.sdkCalls, 0, "no SDK query");
  assert.equal(rg.summaryCalls, 0, "no summary pass");
  assert.equal(rg.reserves, 1);
});

// B. Recovery failure beats a deferred pause.
it("a recovery failure with a deferred pause pending reports failed with custody kept, never paused", async () => {
  const rg = await rig("none", () => undefined, { windowPause: true, refuseSettlement: true });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.flight.steering.getPauseMode(), "now", "the pause was recorded");
  assert.equal(pausedReports(rg), 0);
  assert.equal(rg.pauseParks, 0);
  assert.equal(rg.models, 0);
  assert.ok(api.states.some(s => s.runId === rg.claim.run_id && s.body.status === "failed" &&
    String(s.body.failure_reason).includes("quiescence failed")));
  assert.ok(custodyKept(rg));
});

it("a successor-adoption failure with a deferred pause pending never parks the run", async () => {
  const rg = await rig("none", () => undefined, { windowPause: true });
  git.prepareRecoverySuccessor = async () => { throw new Error("successor adoption refused"); };
  await rg.runner.execute(rg.claim);
  assert.equal(pausedReports(rg), 0);
  assert.equal(rg.pauseParks, 0);
  assert.equal(rg.models, 0);
  assert.ok(custodyKept(rg));
});

for (const hold of [false, true]) {
  it(`recovery budget exhaustion with a deferred pause pending ${hold ? "holds at retained_recovery_exhausted" : "reports failed"}, never paused`, async () => {
    const rg = await rig("none", () => undefined, {
      windowPause: true,
      protocolFeatures: hold ? ["claim_generation_fence", "recovery_cause_retained_recovery_exhausted"] : ["claim_generation_fence"],
    });
    client.getRunOwnership = async () => { throw new Error("ownership read connection reset"); };
    // The window pause rides the first reservation; ownership outages then consume the attempts.
    const reserve = git.reserveRecoveryIteration.bind(git);
    git.reserveRecoveryIteration = async (...args) => { route(rg.flight, "pause", "now"); return reserve(...args); };
    await rg.runner.execute(rg.claim);
    assert.equal(rg.flight.steering.getPauseMode(), "now", "the pause was recorded");
    assert.equal(pausedReports(rg), 0);
    assert.equal(rg.pauseParks, 0);
    assert.equal(rg.models, 0);
    const own = api.states.filter(s => s.runId === rg.claim.run_id);
    if (hold) {
      assert.ok(own.some(s => s.body.status === "recovery_wait" &&
        (s.body as any).recovery_cause === "retained_recovery_exhausted"), "exhaustion hold reported");
      assert.equal(own.some(s => s.body.status === "failed"), false);
    } else assert.ok(own.some(s => s.body.status === "failed"));
    assert.ok(custodyKept(rg));
  });
}

// C. Integrity refusals of the pause park itself.
it("P2: a paused report that throws is not a parked run and keeps custody", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
    report: async (body, original) => { if (body.status === "paused") throw new Error("report transport failed"); return original(); },
  });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.pauseParks, 1, "the settlement attempted exactly one pause park");
  assert.equal(pausedReports(rg), 0);
  assert.equal(rg.flight.parked === true, false);
  assert.equal(rg.models, 0);
  assert.ok(custodyKept(rg));
});

for (const status of ["running", "cancelled"]) {
  it(`P3: a paused ACK answering ${status} is not a parked run`, async () => {
    const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
      report: async (body, original) => body.status === "paused" ? ({ status } as any) : original(),
    });
    await rg.runner.execute(rg.claim);
    assert.equal(rg.pauseParks, 1, "the settlement attempted exactly one pause park");
    assert.equal(pausedReports(rg), 0);
    assert.equal(rg.flight.parked === true, false);
    assert.equal(rg.models, 0);
    assert.ok(custodyKept(rg));
  });
}

it("P1b: a pause_failed report that throws still yields no paused and keeps custody", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
    failPublish: true,
    report: async (body, original) => { if (body.status === "pause_failed") throw new Error("report transport failed"); return original(); },
  });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.pauseParks, 1, "the settlement attempted exactly one pause park");
  assert.equal(pausedReports(rg), 0);
  assert.equal(rg.flight.parked === true, false);
  assert.equal(rg.models, 0);
  assert.ok(custodyKept(rg));
});

// D. A stronger control arriving after the window but before settlement behaves exactly as it
// does without a deferred pause, and no pause park is synthesized.
for (const control of ["switch", "wall", "cancel", "fence", "shutdown"] as const) {
  it(`differential: ${control} after the window but before settlement matches the same control without a deferred pause`, async () => {
    const deliver = (flight: any, runner: any) => {
      if (control === "fence") flight.steering.claimFence = () => "superseded";
      if (control === "shutdown") runner.shuttingDownGlobal = true;
      if (control === "switch") flight.steering.tripCredentialSwitch(2);
      if (control === "wall") route(flight, "pause", "wall");
      if (control === "cancel") route(flight, "cancel");
    };
    const observe = async (rg: Rig) => {
      await rg.runner.execute(rg.claim);
      return {
        statuses: statuses(rg).filter(x => x !== "running"), models: rg.models, parks: rg.pauseParks,
        parked: rg.flight.parked === true,
        arms: rg.logs.lines.map((l: any) => String(l.msg)).filter(m => /retained credential switch|retained recovery stopped|cancel/i.test(m))
          .map(m => m.replace(" with an owner pause pending", "")),
      };
    };
    const plain = await observe(await rig("evidence", deliver, { iid: 3996 }));
    const deferred = await observe(await rig("evidence", deliver, { iid: 3995, windowPause: true }));
    // A wall routed after the window aborts the shared controller itself, so its own pause-signal arm
    // legitimately parks in both runs; the differential equality below is what proves nothing extra
    // was synthesized for the deferred pause.
    if (control !== "wall") assert.equal(deferred.parks, 0, "no pause park synthesized");
    assert.deepEqual(deferred, plain);
  });
}

// E. A milestone pause is not a deferred `now`.
it("a milestone pause delivered during recovery is not deferred-settled and the run reaches the executor", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "milestone"));
  await rg.runner.execute(rg.claim);
  assert.equal(rg.flight.steering.getPauseMode(), "milestone");
  assert.equal(rg.flight.steering.deferredPauseNowPending(), false);
  assert.equal(rg.pauseParks, 0);
  assert.equal(pausedReports(rg), 0);
  assert.equal(rg.models, 1);
});

it("a claim-seeded milestone pause keeps its existing behaviour (characterization)", async () => {
  const rg = await rig("none", () => undefined, { claim: { pause_pending: true, pause_mode: "milestone" } });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.flight.steering.deferredPauseNowPending(), false);
  assert.equal(rg.pauseParks, 0);
  assert.equal(rg.models, 1);
});

// F. A now that spent the cancel before the window is today's retained stop.
it("a now pause that spent the cancel before the recovery window stops recovery as before", async () => {
  const rg = await rig("pre", flight => route(flight, "pause", "now"));
  await rg.runner.execute(rg.claim);
  assert.equal(rg.fired, 1);
  assert.equal(pausedReports(rg), 0);
  assert.equal(rg.pauseParks, 0);
  assert.equal(rg.models, 0);
  assert.ok(logged(rg, /^retained recovery stopped; source and accounting kept$/));
  assert.ok(logged(rg, /^retained recovery stopped with an owner pause pending; preserving/), "truthful retention cause");
  assert.ok(custodyKept(rg));
});

// G. A stop injected after the handoff leaves the same durable state with or without a deferred pause.
// Only the run id, branch, attempt id and times are normalized (they identify the run, not its state).
// Commit hashes are compared exactly: the capture commit's own hash legitimately differs between two
// runs (it embeds per-run metadata), so each run's tip is checked against independent git facts
// (the bare tracking ref, the seeded retained file's blob, the shared tree) and only that checked tip
// is replaced by a token; any other 40-hex value in the journal fails the comparison.
const gitOut = (cwd: string, ...args: string[]) => execFileSync("git", ["-C", cwd, ...args], {
  env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" }, encoding: "utf8",
}).trim();
it("a fence after the successor handoff leaves the same journal with or without a deferred pause", async () => {
  const fence = (flight: any) => { flight.steering.claimFence = () => "superseded"; };
  const facts = (rg: Rig) => {
    const journal = readJournal(rg.bare, rg.clone.branch);
    const tip = gitOut(rg.bare, "rev-parse", `refs/uzi-runner/${rg.clone.branch}`);
    assert.equal(journal.restoreTip, tip, "the journal's restore tip is the tracking ref's tip");
    assert.equal(gitOut(rg.bare, "rev-parse", `${tip}:retained.txt`),
      execFileSync("git", ["hash-object", "--stdin"], { input: "only local dirty work\n", encoding: "utf8" }).trim(),
      "the restore tip holds the seeded dirty file's exact bytes");
    const hashes = new Set(JSON.stringify(journal).match(/[0-9a-f]{40}/g));
    assert.deepEqual([...hashes], [tip], "the only commit hash the journal carries is the checked tip");
    const text = JSON.stringify(journal)
      .replaceAll(rg.claim.run_id, "RUN").replace(/issue-399[56]/g, "issue-N")
      .replace(/[0-9]{8}T[0-9]{6}Z-g[0-9]+-[0-9a-f]+/g, "ATTEMPT").replaceAll(tip, "TIP")
      .replace(/"(startedAt|deadline|createdAt|updatedAt|at)":\d+/g, '"$1":0');
    return { text, tree: gitOut(rg.bare, "rev-parse", `${tip}^{tree}`), parent: gitOut(rg.bare, "rev-parse", `${tip}^`) };
  };
  const plain = await rig("successor", fence, { iid: 3996 });
  await plain.runner.execute(plain.claim);
  const deferred = await rig("successor", fence, { iid: 3995, windowPause: true });
  await deferred.runner.execute(deferred.claim);
  assert.equal(deferred.flight.steering.getPauseMode(), "now", "the pause was recorded");
  for (const rg of [plain, deferred]) {
    assert.equal(rg.models, 0);
    assert.equal(rg.pauseParks, 0);
    assert.equal(pausedReports(rg), 0);
    assert.equal(statuses(rg).includes("pause_failed" as any), false, "no pause-related /state body was sent");
    assert.equal(rg.serverPaused + rg.serverPauseFailed, 0);
  }
  const [p, d] = [facts(plain), facts(deferred)];
  assert.equal(d.tree, p.tree, "identical content: the same tree");
  assert.equal(d.parent, p.parent, "identical base: the same parent");
  assert.equal(d.text, p.text);
});

// H. The window is released when recovery returns: a `now` routed afterwards is today's immediate path.
it("a now pause routed right after the recovery window is not deferred and takes the immediate path", async () => {
  const rg = await rig("post", flight => route(flight, "pause", "now"));
  await rg.runner.execute(rg.claim);
  assert.equal(rg.fired, 1);
  assert.equal(rg.flight.steering.deferredPauseNowPending(), false, "released window defers nothing");
  assert.equal(rg.flight.steering.getPauseMode(), "now");
  assert.equal(rg.models, 0, "stopped before the model");
  assert.equal(rg.flight.cancel.signal.aborted, true);
  assert.equal(rg.flight.cancel.signal.reason?.constructor?.name, "PauseNowSignal", "the immediate path aborted the lifecycle");
  assert.equal(rg.pauseParks, 1, "the ordinary outer pause arm settled it");
  assert.equal(pausedReports(rg), 1);
});

// I. A publication-only claim never reaches phasePreflightHandoff, so the window must stay closed for it.
for (const publicationOnly of [true, false]) {
  it(`recoverRetainedClaim ${publicationOnly ? "does not open" : "opens"} the owner-pause deferral window ${publicationOnly ? "for a publication-only claim" : "for an ordinary claim"}`, async () => {
    // Building a valid publication-only retained claim is impractical here; assert at the wrapper seam.
    const rg = await rig("none", () => undefined);
    let opened = 0, released = 0, bodies = 0;
    const steering = { deferOwnerPauseNow: () => { opened++; return () => { released++; }; } };
    (rg.runner as any).recoverRetainedClaimBody = async () => { bodies++; };
    const claim = publicationOnly ? { ...rg.claim, publication_resume: {} } : rg.claim;
    await (rg.runner as any).recoverRetainedClaim(claim, { steering }, "key", {});
    assert.equal(bodies, 1);
    assert.equal(opened, publicationOnly ? 0 : 1);
    assert.equal(released, publicationOnly ? 0 : 1);
  });
}

// J. Adoption hold (row 4a): a deferred pause never turns the integrity hold into a pause.
it("an adoption failure with a deferred pause pending holds at retained_recovery_adoption_failed, never paused", async () => {
  const rg = await rig("none", () => undefined, {
    windowPause: true, protocolFeatures: ["claim_generation_fence", "recovery_cause_retained_recovery_adoption_failed"],
  });
  git.prepareRecoverySuccessor = async () => { throw new Error("successor adoption refused"); };
  await rg.runner.execute(rg.claim);
  assert.equal(rg.flight.steering.getPauseMode(), "now", "the pause was recorded");
  const own = api.states.filter(s => s.runId === rg.claim.run_id);
  assert.ok(own.some(s => s.body.status === "recovery_wait" &&
    (s.body as any).recovery_cause === "retained_recovery_adoption_failed"), "adoption hold reported");
  assert.equal(pausedReports(rg), 0);
  assert.equal(rg.pauseParks, 0);
  assert.equal(rg.models, 0);
  assert.ok(custodyKept(rg));
});

// K. The main before-capture regression under a test-owned production AuthorityRuntime and an
// authority-required GitCache (the shape production retained recovery runs in).
it("authority-backed: a now pause delivered before capture parks with one confirmed paused report and no model call", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), { authority: true });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.fired, 1, "the pause was delivered inside recovery");
  assert.equal(pausedReports(rg), 1);
  assert.equal(rg.serverPaused, 1, "the server model applied the pause");
  assert.equal(rg.flight.parked, true);
  assert.equal(rg.models, 0);
  assert.equal(rg.reserves, 1, "one reservation");
  assert.equal(statuses(rg).includes("failed"), false);
  assert.equal(statuses(rg).includes("pause_failed" as any), false);
});

// L. A control injected while an async recovery step is still outstanding (inside the hooked
// ensureClone, before it resolves): the stop is prompt, owned work joins, nothing is left pending.
const timeoutCount = () => process.getActiveResourcesInfo().filter(r => r === "Timeout").length;
for (const control of ["cancel", "shutdown"] as const) {
  it(`a ${control} injected during an outstanding recovery git operation stops promptly with owned work joined`, async () => {
    const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
      duringEnsureClone: (flight, runner) => {
        if (control === "cancel") route(flight, "cancel");
        else runner.shutdown();
      },
    });
    const timersBefore = timeoutCount();
    const started = Date.now();
    await rg.runner.execute(rg.claim); // resolves only once owned recovery work has joined
    assert.ok(Date.now() - started < 20_000, "the stop was prompt");
    assert.equal(rg.models, 0);
    assert.equal(pausedReports(rg), 0, "the stronger control, not the deferred pause, decided");
    assert.equal(rg.pauseParks, 0);
    assert.equal(rg.flight.parked === true, false);
    assert.ok(logged(rg, /^retained recovery stopped; source and accounting kept$/), "retained stop arm");
    assert.ok(custodyKept(rg));
    assert.ok(timeoutCount() <= timersBefore, "no timer left pending by the stopped recovery");
  });
}

// M. Pre-approval is irrelevant to the settlement: not-approved and pre-approved claims both park
// before any planning, provider or summary call.
it("a claim whose plan is not approved parks on the deferred pause with no planning call", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
    claim: { plan_md: "# Plan\n- change the thing", plan_approved: false },
  });
  await rg.runner.execute(rg.claim);
  assert.equal(pausedReports(rg), 1);
  assert.equal(rg.serverPaused, 1);
  assert.equal(rg.flight.parked, true);
  assert.equal(rg.models, 0, "no executor (planning) call");
});

it("production SdkExecutor: a pre-approved claim parks on the deferred pause with no SDK and no summary call", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
    production: true, claim: { plan_md: "# Plan\n- change the thing", plan_approved: true, plan_source: "seeded" },
  });
  await rg.runner.execute(rg.claim);
  assert.equal(pausedReports(rg), 1);
  assert.equal(rg.serverPaused, 1);
  assert.equal(rg.flight.parked, true);
  assert.equal(rg.sdkCalls, 0);
  assert.equal(rg.summaryCalls, 0);
});

for (const prompt of [false, true]) it(`${prompt ? "prompt kind (issue #4116, characterization): " : ""}required-base validation holds an unattested retained successor before a deferred pause can publish`, async t => {
  const planned = gitOut(fx.originPath, "rev-parse", "HEAD");
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
    prompt,
    production: true,
    claim: { plan_md: "# Plan\n- preserve the planned base", plan_approved: true, plan_source: "seeded",
      planned_base_commit: planned, require_base_match: true },
  });
  let holds = 0;
  client.requestBaseAttestationHold = async () => { holds++; return { outcome: "held", status: "paused" }; };
  let publishesBeforeHandoff: number | undefined;
  const handoff = (rg.runner as any).phasePreflightHandoff.bind(rg.runner);
  (rg.runner as any).phasePreflightHandoff = async (...args: any[]) => {
    publishesBeforeHandoff = rg.publishes;
    return handoff(...args);
  };
  await rg.runner.execute(rg.claim);
  t.diagnostic(JSON.stringify({ holds, publishesBeforeHandoff, checkpointPublishes: rg.publishes, ownerPauseReports: pausedReports(rg) }));
  assert.equal(holds, 1, "required-base recovery hold must precede owner-pause publication");
  assert.equal(typeof publishesBeforeHandoff, "number", "the retained successor reached the production preflight handoff");
  assert.equal(rg.publishes, publishesBeforeHandoff, "the deferred pause must not publish another checkpoint before base validation");
  assert.equal(pausedReports(rg), 0, "the owner-pause writer must not replace the required-base hold");
  assert.equal(rg.pauseParks, 0);
  assert.equal(rg.flight.parked, true);
  assert.equal(rg.flight.preserveRecoveryClone, true);
  assert.equal(rg.sdkCalls, 0);
  assert.equal(rg.summaryCalls, 0);
});

// N. Integrity: the server applied `paused` but the ACK was lost on the way back.
it("P2 applied-ACK-lost: the server is paused, the worker is not parked, and no second pause report is sent", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), {
    report: async (body, original) => {
      const ack = await original(); // the server applied the report ...
      if (body.status === "paused") throw new Error("ack lost in transit"); // ... but the worker never saw it
      return ack;
    },
  });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.serverPaused, 1, "the server model is paused");
  assert.equal(rg.pauseParks, 1);
  assert.equal(pausedReports(rg), 1, "exactly one paused report reached the server");
  assert.equal(statuses(rg).includes("pause_failed" as any), false, "no pause_failed follows a server-applied pause");
  assert.equal(rg.flight.parked === true, false, "the worker did not park locally");
  assert.equal(rg.models, 0);
  assert.ok(custodyKept(rg));
});

// O. Residue refusal: a clone that is not provably quiescent never parks.
it("residue refusal: a non-quiescent clone reports pause_failed with no paused and keeps custody", async () => {
  const rg = await rig("reserve", flight => route(flight, "pause", "now"), { quiesceBlocked: true });
  await rg.runner.execute(rg.claim);
  assert.equal(pausedReports(rg), 0);
  assert.equal(statuses(rg).includes("pause_failed" as any), true);
  assert.equal(rg.serverPauseFailed, 1);
  assert.equal(rg.serverPaused, 0);
  assert.equal(rg.flight.parked === true, false);
  assert.equal(rg.models, 0);
  assert.ok(logged(rg, /clone not provably quiescent/), "the refusal names the residue block");
  assert.equal(logged(rg, /could not publish a pause checkpoint/), false, "the refusal is the residue block, not a failed publish");
  assert.ok(custodyKept(rg));
});

// P. Row (a) boundary: a now routed after the window (immediately before the production executor
// runs) reaches the SdkExecutor's first loop boundary and parks through the ordinary pause park.
const seededPlan = { plan_md: "# Plan\n- change the thing", plan_approved: true, plan_source: "seeded" };
it("characterization (row a): a now routed right before the production executor parks at its first boundary", async () => {
  const rg = await rig("none", () => undefined, {
    production: true, claim: seededPlan,
    beforeExecutorRun: flight => route(flight, "pause", "now"),
  });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.pauseParks, 1, "the first boundary parked through handlePausePark");
  assert.equal(pausedReports(rg), 1);
  assert.equal(rg.serverPaused, 1);
  assert.equal(rg.flight.parked, true);
  assert.equal(rg.sdkCalls, 0, "no provider turn was dispatched");
});

it("characterization (row a): a refused park is attempted twice, then a cancel delivered afterwards is still honored", async () => {
  const rg = await rig("none", () => undefined, {
    production: true, claim: seededPlan, failPublish: true,
    beforeExecutorRun: flight => route(flight, "pause", "now"),
    afterParkAttempt: (attempt, flight) => { if (attempt === 2) route(flight, "cancel"); },
  });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.pauseParks, 2, "the park was attempted twice (the loop re-arms after a declined park)");
  assert.equal(pausedReports(rg), 0);
  assert.equal(statuses(rg).filter(x => x === "pause_failed").length, 2);
  assert.equal(rg.flight.parked === true, false);
  assert.equal(rg.sdkCalls, 0, "no provider turn was dispatched");
  const failed = api.states.filter(x => x.runId === rg.claim.run_id && x.body.status === "failed");
  assert.equal(failed.length, 1, "the late cancel ended the run");
  assert.match(String(failed[0]!.body.failure_reason), /cancel/i);
});
