import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { Readable } from "node:stream";
import type { ExecutorFactory } from "../src/runner.js";
import type { RunContext } from "../src/executor.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

// Multi-claim proof for the owner `now` pause deferred during retained recovery (issue #3995).
// The production RunRunner is driven claim after claim against a small server model that owns the
// facts the single-claim tests cannot see: the run row (status, pause columns, claim generation,
// requeue_count), the server writers a worker report reaches (SetRunPaused, ClearPauseRequest,
// ParkRunRetainedRecovery) and the missing-snapshot automatic requeue (Wr) that follows a
// nonterminal departure without a confirmed park. Explicit owner Resume is a separate action the
// driver never calls on its own.

installHarness();

const REQUEUE_CAP = 3;
const RECOVERY_PARKS = new Set(["recovery_wait", "limit_wait", "pool_wait"]);
const TERMINAL = new Set(["failed", "completed", "cancelled"]);

/** The server's view of one run. */
class ServerModel {
  status = "running";
  claimGeneration: number;
  requeueCount = 0;
  readonly baseline = 0;
  pauseRequestedAt: number | undefined;
  pauseMode: string | undefined;
  claimReleased = false;
  recoveryCause: string | undefined;
  pausedApplied = 0;
  pauseFailedApplied = 0;
  requeues = 0;
  exhausted = false;
  claimsOffered = 1;
  readonly reports: string[] = [];

  constructor(readonly runId: string, readonly iid: number, firstGeneration: number) {
    this.claimGeneration = firstGeneration;
  }

  /** The owner's pause request: the durable pause columns (api layer), not a worker-side event. */
  requestPause(mode: "now" | "milestone"): void {
    this.pauseRequestedAt = Date.now();
    this.pauseMode = mode;
  }
  private clearPause(): void { this.pauseRequestedAt = undefined; this.pauseMode = undefined; }
  get pausePending(): boolean { return this.pauseRequestedAt !== undefined; }

  /** Apply one worker report and answer whether the server applied it. */
  apply(body: any): boolean {
    this.reports.push(String(body.status));
    if (TERMINAL.has(this.status) || this.status === "paused" || this.status === "recovery_wait") return false;
    const status = String(body.status);
    if (status === "paused") {
      // SetRunPaused: status='running' AND pause_requested_at IS NOT NULL AND pause_mode IN (milestone, now, reserve).
      if (this.status !== "running" || !this.pausePending ||
          !["milestone", "now", "reserve"].includes(this.pauseMode ?? "")) return false;
      this.status = "paused";
      this.clearPause();
      this.pausedApplied++;
      return true;
    }
    if (status === "pause_failed") {
      // ClearPauseRequest: fenced on the live claim and generation; the columns go, the row stays running.
      if (this.claimReleased || (body.claim_generation !== undefined && body.claim_generation !== this.claimGeneration)) return false;
      this.clearPause();
      this.pauseFailedApplied++;
      return true;
    }
    if (status === "recovery_wait") {
      // ParkRunRetainedRecovery: the park releases the claim and leaves the pause columns alone.
      if (typeof body.recovery_cause !== "string" || this.status !== "running" || this.claimReleased ||
          (body.claim_generation !== undefined && body.claim_generation !== this.claimGeneration)) return false;
      this.status = "recovery_wait";
      this.recoveryCause = body.recovery_cause;
      this.claimReleased = true;
      return true;
    }
    if (RECOVERY_PARKS.has(status)) { this.status = status; return true; }
    // Terminal transitions also clear the pause columns.
    if (TERMINAL.has(status)) { this.status = status; this.clearPause(); return true; }
    if (status === "running" || status.startsWith("awaiting_")) { this.status = status; return true; }
    return false;
  }

  /** Nonterminal departure without a confirmed park: the flight ended with the row still running. */
  get departed(): boolean {
    return (this.status === "running" || this.status.startsWith("awaiting_")) && !this.claimReleased;
  }

  /** After a flight ended: Wr (requeue and re-offer), Wf (cap reached) or nothing. */
  afterFlight(offer: (generation: number, pause: boolean, mode: string | undefined) => any): any {
    if (!this.departed) return undefined;
    if (this.requeueCount - this.baseline >= REQUEUE_CAP) {
      this.status = "recovery_wait";
      this.recoveryCause = "worker_requeue_exhausted";
      this.claimReleased = true;
      this.clearPause();
      this.exhausted = true;
      return undefined;
    }
    this.requeueCount++;
    this.requeues++;
    this.claimGeneration++;
    this.claimsOffered++;
    return offer(this.claimGeneration, this.pausePending, this.pauseMode);
  }

  /** The explicit owner Resume action: never invoked automatically. */
  ownerResume(offer: (generation: number, pause: boolean, mode: string | undefined) => any): any {
    assert.ok(this.status === "paused" || this.status === "recovery_wait", "Resume applies to a held run");
    this.status = "running";
    this.claimReleased = false;
    this.recoveryCause = undefined;
    this.claimGeneration++;
    this.claimsOffered++;
    return offer(this.claimGeneration, this.pausePending, this.pauseMode);
  }
}

const models = new Map<string, ServerModel>();

function installServerModel(): void {
  models.clear();
  const report = client.reportState.bind(client);
  client.reportState = (async (...args: any[]) => {
    const [runId, body] = args as [string, any];
    const model = models.get(runId);
    if (!model) return (report as any)(...args);
    // The model applies only after the underlying report resolves, so a transport throw records nothing.
    const ack = await (report as any)(...args);
    const wouldApply = model.apply(body);
    if (!wouldApply && body.status === "paused") return { ...ack, status: model.status };
    return ack;
  }) as typeof client.reportState;
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

function readJournal(bare: string, branch: string): any {
  return JSON.parse(execFileSync("git", ["-C", bare, "config", `uzi-recovery.${branch}.clone`], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" }, encoding: "utf8",
  }).trim());
}

interface Run {
  model: ServerModel;
  runner: ReturnType<typeof runnerWith>;
  bare: string;
  claims: any[];
  models: number;
  reserves: number;
  publishes: number;
  pauseParks: number;
  fired: number;
  flight: any;
  logs: ReturnType<typeof recordingLogger>;
  offer: (generation: number, pause: boolean, mode: string | undefined) => any;
}

interface RunOptions {
  iid: number;
  /** Create the owner pause on the model and deliver it to the first claim's live flight. */
  pause?: boolean;
  failPublish?: boolean;
  protocolFeatures?: string[];
  /** Wrap the worker's state report; throwing means the report never reached the server. */
  report?: (body: any, original: () => Promise<any>) => Promise<any>;
  adoptionFailure?: boolean;
}

/** Seed a retained clone for a claim at generation 2 and build the runner over it. */
async function setup(opts: RunOptions): Promise<Run> {
  const first = gitlabClaim(opts.iid, { claim_generation: 2, session_id: "predecessor-session" });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, opts.iid, noProofReseed, first.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, first.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(first.run_id, "running", 2);
  client.protocolFeatures = opts.protocolFeatures ?? ["claim_generation_fence"];
  const retainedHome = path.join(homeDir, first.run_id);
  fs.mkdirSync(retainedHome, { recursive: true });
  fs.writeFileSync(path.join(retainedHome, "session"), "predecessor session");

  const model = new ServerModel(first.run_id, opts.iid, 2);
  models.set(first.run_id, model);
  if (opts.pause) model.requestPause("now");

  const run = { model, bare, claims: [first], models: 0, reserves: 0, publishes: 0, pauseParks: 0, fired: 0 } as unknown as Run;
  const logs = recordingLogger();
  const makeExecutor: ExecutorFactory = runId => ({
    homeDir: path.join(homeDir, runId),
    executor: { run: async (_ctx: RunContext) => { run.models++; throw new Error("fixture ends the model run"); } } as any,
  });
  const r = runnerWith(makeExecutor, fakeGitlab().gitlab, undefined, logs.logger, { recoveryRetryMs: 1 });
  const phaseClone = (r as any).phaseClone.bind(r);
  (r as any).phaseClone = async (...args: any[]) => { run.flight = args[1]; return phaseClone(...args); };
  const park = (r as any).handlePausePark.bind(r);
  (r as any).handlePausePark = async (...args: any[]) => { run.pauseParks++; return park(...args); };
  const reserve = git.reserveRecoveryIteration.bind(git);
  git.reserveRecoveryIteration = async (...args) => {
    // Count only this run's reservations (a prior run in the same test leaves its wrapper in the chain).
    if (args[3] && (args[3] as any).runId === first.run_id) run.reserves++;
    if (opts.pause && run.fired === 0 && args[3] && (args[3] as any).runId === first.run_id) {
      run.fired++;
      // The owner pause reaches the live flight through the real steering dispatch, inside recovery.
      run.flight.steering.route("pause", "now", 1, false, undefined);
    }
    return reserve(...args);
  };
  if (opts.adoptionFailure) git.prepareRecoverySuccessor = async () => { throw new Error("successor adoption refused"); };
  client.publishCheckpoint = async (_run, _tip, pack) => {
    await drain(pack);
    run.publishes++;
    if (opts.failPublish) return { ok: false, httpStatus: 500 } as any;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } } as any;
  };
  if (opts.report) {
    const wrapped = client.reportState.bind(client);
    client.reportState = (async (...args: any[]) => {
      const [runId, body] = args as [string, any];
      if (runId !== first.run_id) return (wrapped as any)(...args);
      return opts.report!(body, () => (wrapped as any)(...args));
    }) as typeof client.reportState;
  }
  run.runner = r;
  run.logs = logs;
  run.offer = (generation, pause, mode) => {
    api.setOwnershipStatus(first.run_id, "running", generation);
    const claim = gitlabClaim(opts.iid, {
      run_id: first.run_id, claim_generation: generation, session_id: `session-${generation}`,
      ...(pause ? { pause_pending: true, pause_mode: mode } : {}),
    });
    run.claims.push(claim);
    return claim;
  };
  return run;
}

/** Drive the runner claim after claim until the model stops offering one (bounded by maxClaims). */
async function drive(run: Run, maxClaims = REQUEUE_CAP + 3): Promise<void> {
  let claim: any = run.claims[0];
  for (let n = 0; n < maxClaims; n++) {
    try { await run.runner.execute(claim); } catch { /* a thrown flight is still a departure */ }
    claim = run.model.afterFlight(run.offer);
    if (!claim) return;
  }
  assert.fail(`the server kept offering claims past ${maxClaims}`);
}

/** The recovery accounting the first claim left durable, with identities and times normalised. */
function allowance(run: Run, branch: string): string {
  const journal = readJournal(run.bare, branch);
  const rec = journal.recovery ?? {};
  assert.equal(typeof rec.attempts, "number", "the journal carries the recovery attempt count");
  return JSON.stringify({
    stage: rec.stage, attempts: rec.attempts, claimGeneration: rec.claimGeneration, backoffMs: rec.backoffMs,
  });
}

const branchFor = (iid: number) => `agent/issue-${iid}`;

it("control: a run with no pause reaches the model once with one recovery reservation", async () => {
  installServerModel();
  const run = await setup({ iid: 3996 });
  await drive(run);
  assert.equal(run.models, 1);
  assert.equal(run.reserves, 1);
  assert.equal(run.model.pausedApplied, 0);
});

it("a deferred owner now pause settles as one claim, no automatic requeue and a paused row, spending what the control spends", async () => {
  installServerModel();
  const control = await setup({ iid: 3996 });
  await drive(control);
  assert.equal(control.models, 1, "the control reaches the model");

  const run = await setup({ iid: 3995, pause: true });
  await drive(run);
  assert.equal(run.claims.length, 1, "exactly one claim");
  assert.equal(run.model.requeues, 0, "no automatic requeue");
  assert.equal(run.fired, 1, "the pause was delivered inside recovery, on the first claim only");
  assert.equal(run.model.requeueCount, 0);
  assert.equal(run.model.status, "paused");
  assert.equal(run.model.pausedApplied, 1);
  assert.equal(run.model.pausePending, false, "SetRunPaused cleared the pause columns");
  assert.equal(run.models, 0, "no model call in the paused run");
  assert.equal(run.pauseParks, 1);
  assert.equal(run.publishes >= 1, true, "checkpoint first");
  assert.equal(run.reserves, control.reserves, "reservations equal the no-pause control");
  assert.equal(allowance(run, branchFor(3995)), allowance(control, branchFor(3996)), "allowance consumption equals the control");
});

it("characterization (a): a refused park clears the pause columns, departs, and the single requeue seeds no pause", async () => {
  installServerModel();
  const run = await setup({ iid: 3995, pause: true, failPublish: true });
  await drive(run);
  assert.equal(run.model.pauseFailedApplied >= 1, true, "pause_failed applied");
  assert.equal(run.model.pausedApplied, 0);
  assert.equal(run.model.requeues, 1, "one automatic requeue after the departure");
  assert.equal(run.claims.length, 2);
  assert.equal(run.claims[1].pause_pending, undefined, "the next claim carries no pause");
  assert.equal(run.claims[1].claim_generation, 3);
});

it("characterization (b): an unconfirmed paused report leaves the row running with the pause columns, so the requeue seeds the pause", async () => {
  installServerModel();
  let throwOnce = true;
  const run = await setup({
    iid: 3995, pause: true,
    report: async (body, original) => {
      if (body.status === "paused" && throwOnce) { throwOnce = false; throw new Error("report transport failed"); }
      return original();
    },
  });
  await drive(run);
  assert.equal(run.model.requeues >= 1, true, "the departure is requeued");
  assert.equal(run.claims[1].pause_pending, true, "the pause columns survived and are re-seeded");
  assert.equal(run.claims[1].pause_mode, "now");
});

it("characterization (c): a confirmed adoption hold is never requeued; only an explicit owner Resume offers a claim", async () => {
  installServerModel();
  const run = await setup({
    iid: 3995, pause: true, adoptionFailure: true,
    protocolFeatures: ["claim_generation_fence", "recovery_cause_retained_recovery_adoption_failed"],
  });
  await drive(run);
  assert.equal(run.model.status, "recovery_wait");
  assert.equal(run.model.recoveryCause, "retained_recovery_adoption_failed");
  assert.equal(run.model.requeues, 0, "no automatic requeue");
  assert.equal(run.claims.length, 1, "no further claim without an owner action");
  assert.equal(run.model.pausePending, true, "the hold leaves the pause columns alone");
  assert.equal(run.models, 0);
  // Explicit owner Resume, a separate action: now a claim is offered.
  const next = run.model.ownerResume(run.offer);
  assert.equal(run.claims.length, 2);
  assert.equal(next.claim_generation, 3);
  assert.equal(next.pause_pending, true);
});
