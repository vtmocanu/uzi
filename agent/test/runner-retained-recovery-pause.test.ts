import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { Readable } from "node:stream";
import type { ExecutorFactory } from "../src/runner.js";
import type { RunContext } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { SummaryRunner } from "../src/summary-runner.js";
import { noProofReseed, nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

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

async function seed(iid = 3995, overrides: Record<string, unknown> = {}) {
  const claim = gitlabClaim(iid, { claim_generation: 2, session_id: "predecessor-session", ...overrides });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
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

type Hook = "pre" | "post" | "reserve" | "successor" | "evidence" | "none";
interface Rig {
  models: number;
  sdkCalls: number;
  summaryCalls: number;
  reserves: number;
  publishes: number;
  pauseParks: number;
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
  failPublish?: boolean;
  claim?: Record<string, unknown>;
  /** Route an owner `now` pause inside the recovery window (first recovery reservation). */
  windowPause?: boolean;
  /** The harness executor refuses command settlement, so recovery fails with a quiescence blocker. */
  refuseSettlement?: boolean;
  /** Use the production SdkExecutor over a counted fake SDK query and a counted fake SummaryRunner. */
  production?: boolean;
  protocolFeatures?: string[];
  /** Wrap the worker client's state report. */
  report?: (body: any, original: () => Promise<any>) => Promise<any>;
}

/** Build a runner over a seeded retained clone. `deliver` runs once, at the chosen seam, with the live flight. */
async function rig(hook: Hook, deliver: (flight: any, runner: any) => void, opts: RigOptions = {}): Promise<Rig> {
  const { claim, bare, clone } = await seed(opts.iid ?? 3995, opts.claim);
  client.protocolFeatures = opts.protocolFeatures ?? ["claim_generation_fence"];
  const retainedHome = path.join(homeDir, claim.run_id);
  fs.mkdirSync(retainedHome, { recursive: true });
  fs.writeFileSync(path.join(retainedHome, "session"), "predecessor session");
  const state = { models: 0, sdkCalls: 0, summaryCalls: 0, reserves: 0, publishes: 0, pauseParks: 0, fired: 0 } as Rig;
  const logs = recordingLogger();
  const makeExecutor: ExecutorFactory = runId => {
    const homeForRun = path.join(homeDir, runId);
    if (opts.production) {
      const summaryRunner = {
        generateIntentSummary: async () => { state.summaryCalls++; return null; },
        generatePlanSummary: async () => { state.summaryCalls++; return { summary: "unused", deltas: [] }; },
      } as unknown as SummaryRunner;
      const queryFn: SdkQueryFn = () => { state.sdkCalls++; throw new Error("the model must not be reached"); };
      return { homeDir: homeForRun, executor: new SdkExecutor(nullLogger(), homeForRun, { queryFn, client, summaryRunner }) };
    }
    return { homeDir: homeForRun, executor: {
      run: async (_ctx: RunContext) => { state.models++; throw new Error("fixture ends the model run"); },
      ...(opts.refuseSettlement ? { settleWorkerCommands: async () => ({ confirmed: false }) } : {}),
    } as any };
  };
  const r = runnerWith(makeExecutor, fakeGitlab().gitlab, undefined, logs.logger, { recoveryRetryMs: 1 });
  const once = () => { if (state.fired++ === 0) deliver(state.flight, r); };
  const phaseClone = (r as any).phaseClone.bind(r);
  (r as any).phaseClone = async (...args: any[]) => { state.flight = args[1]; return phaseClone(...args); };
  const park = (r as any).handlePausePark.bind(r);
  (r as any).handlePausePark = async (...args: any[]) => { state.pauseParks++; return park(...args); };
  if (hook === "pre") {
    const recover = (r as any).recoverRetainedClaim.bind(r);
    (r as any).recoverRetainedClaim = async (...args: any[]) => { once(); return recover(...args); };
  }
  if (hook === "post") {
    const recover = (r as any).recoverRetainedClaim.bind(r);
    (r as any).recoverRetainedClaim = async (...args: any[]) => { const out = await recover(...args); once(); return out; };
  }
  const reserve = git.reserveRecoveryIteration.bind(git);
  let windowRouted = false;
  git.reserveRecoveryIteration = async (...args) => {
    state.reserves++;
    if (opts.windowPause && !windowRouted) { windowRouted = true; route(state.flight, "pause", "now"); }
    if (hook === "reserve") once();
    return reserve(...args);
  };
  const adopt = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => {
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
  if (opts.report) {
    const report = client.reportState.bind(client);
    client.reportState = (async (...args: any[]) =>
      opts.report!(args[1], () => (report as any)(...args))) as typeof client.reportState;
  }
  Object.assign(state, { runner: r, logs, claim, bare, clone });
  return state;
}

const statuses = (rg: Rig) => api.states.filter(s => s.runId === rg.claim.run_id).map(s => s.body.status);
const route = (flight: any, kind: string, body?: string) => flight.steering.route(kind, body, 1, false, undefined);

const logged = (rg: Rig, re: RegExp) => rg.logs.lines.some((l: any) => re.test(String(l.msg)));
const pausedReports = (rg: Rig) => statuses(rg).filter(s => s === "paused").length;

it("control: a run with no pause reaches the model after one recovery reservation", async () => {
  const rg = await rig("none", () => undefined);
  await rg.runner.execute(rg.claim);
  assert.equal(rg.models, 1);
  assert.equal(rg.reserves, 1);
  assert.equal(pausedReports(rg), 0);
});

for (const seam of ["reserve", "successor"] as const) {
  it(`a delivered now pause during retained recovery (${seam === "reserve" ? "before capture" : "after the successor handoff"}) parks with one paused report and no model call`, async () => {
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

for (const control of ["cancel", "wall", "switch", "fence", "shutdown"] as const) {
  it(`a stronger control inside the window keeps its own outcome: ${control}`, async () => {
    const rg = await rig("reserve", (flight, runner) => {
      route(flight, "pause", "now");
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
    assert.ok(fs.existsSync(readJournal(rg.bare, rg.clone.branch).clonePath), "custody kept");
    if (control === "wall" || control === "cancel" || control === "shutdown")
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
  assert.ok(fs.existsSync(readJournal(rg.bare, rg.clone.branch).clonePath), "custody kept");
});

it("a claim-seeded now pause keeps its existing behaviour (characterization)", async () => {
  const rg = await rig("none", () => undefined, { claim: { pause_pending: true, pause_mode: "now" } });
  await rg.runner.execute(rg.claim);
  assert.equal(rg.flight.steering.deferredPauseNowPending(), false, "a seeded pause never sets the deferred flag");
  // The runner does not settle a seeded pause itself: the executor honours it at its first loop boundary.
  assert.equal(rg.models, 1, "a seeded pause still reaches the executor, which owns its handling");
  assert.equal(pausedReports(rg), 0, "the runner settlement arm is not used for a seeded pause");
});

const custodyKept = (rg: Rig) => fs.existsSync(readJournal(rg.bare, rg.clone.branch).clonePath);

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
it("a fence after the successor handoff leaves the same journal with or without a deferred pause", async () => {
  const fence = (flight: any) => { flight.steering.claimFence = () => "superseded"; };
  const normalized = (rg: Rig) => {
    const journal = readJournal(rg.bare, rg.clone.branch);
    const text = JSON.stringify(journal)
      .replaceAll(rg.claim.run_id, "RUN").replace(/issue-399[56]/g, "issue-N")
      .replace(/[0-9]{8}T[0-9]{6}Z-g[0-9]+-[0-9a-f]+/g, "ATTEMPT").replace(/[0-9a-f]{40}/g, "SHA")
      .replace(/"(startedAt|deadline|createdAt|updatedAt|at)":\d+/g, '"$1":0');
    return text;
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
    assert.equal(statuses(rg).includes("pause_failed" as any), false);
  }
  assert.equal(normalized(deferred), normalized(plain));
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
