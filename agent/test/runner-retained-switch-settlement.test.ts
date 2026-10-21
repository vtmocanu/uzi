import { it, describe, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { ExecutorFactory, RunRunner } from "../src/runner.js";
import { Worker } from "../src/worker.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { Outbox } from "../src/outbox.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { RunContext } from "../src/executor.js";
import { MaintenanceBudget, RetainedSwitchSettlement } from "../src/maintenance-budget.js";
import type { RunProcessReap } from "../src/run-procs.js";
import { SdkExecutor } from "../src/sdk-executor.js";
import { LiveAttemptRegistry, type QuiesceRunOutcome, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { noProofReseed, nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

// issue #3181 m3: a retained successor's credential switch must prove its own run processes
// stopped (settle + quiesce) BEFORE it reports the switch, or HOLD when it cannot, and a held
// flight must join the operations it still owns before its slots are released.

installHarness();

const TIMEOUT = { timeout: 90_000 };
const NONCE = "n".repeat(43);
const PID = 777_001;
const HOLD = "credential_switch_held";
const SETTLED = "credential_switch_hold_settled";

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}

async function seed(iid: number) {
  const claim = gitlabClaim(iid, { claim_generation: 2, session_id: "predecessor-session" });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone };
}

function readJournal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}

interface Gate { promise: Promise<void>; open(): void; opened: boolean }
function gate(): Gate {
  let open!: () => void;
  const g: Gate = { opened: false, open: () => { g.opened = true; open(); }, promise: new Promise<void>(resolve => { open = resolve; }) };
  return g;
}

async function until(label: string, cond: () => boolean, ms = 30_000): Promise<void> {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${label}`);
    await new Promise(resolve => setTimeout(resolve, 5));
  }
}

/** Wait for an already-running promise, giving up (false) once the ambient budget has no time left. */
async function within(pending: Promise<unknown>): Promise<boolean> {
  const budget = MaintenanceBudget.current();
  if (!budget) { await pending; return true; }
  let timer: ReturnType<typeof setTimeout> | undefined;
  const expired = new Promise<false>(resolve => { timer = setTimeout(() => resolve(false), budget.remainingMs()); });
  try { return await Promise.race([pending.then(() => true as const), expired]); } finally { clearTimeout(timer); }
}

let callbackFailures: unknown[] = [];
const cleanups: Array<() => void> = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
  const failures = callbackFailures;
  callbackFailures = [];
  assert.deepEqual(failures, [], "runner must not swallow model callback assertions");
});

/** A worker-level model-command survivor shared by every executor on this worker. A later
 *  generation's reaper sees it as `left` (a worker_child, which the real reaper never kills). */
class WorkerSurvivors {
  readonly pids = new Set<number>();
  /** Six survivors while any is live: five worker children, then a sixth of another class. */
  mixed = false;
  reap = async (): Promise<RunProcessReap> => {
    if (this.mixed && this.pids.size > 0) {
      const left = [777001, 777002, 777003, 777004, 777005, 777006];
      return {
        killed: [], left, complete: true,
        leftReasons: Object.fromEntries(left.map((pid, i) => [pid, i < 5 ? "worker_child" : "kill_failed"])),
      } as RunProcessReap;
    }
    return {
      killed: [], left: [...this.pids], complete: true,
      leftReasons: Object.fromEntries([...this.pids].map(pid => [pid, "worker_child"])),
    } as RunProcessReap;
  };
}

interface Options {
  iid: number;
  /** Gen 2 leaves a model-command survivor: `settled-by-settle` is removed by gen 2's own settle,
   *  `joined-then-reaped` once gen 2's owning operation finishes (the reap joins it), `permanent`
   *  never. `none` leaves nothing. */
  survivor: "none" | "settled-by-settle" | "joined-then-reaped" | "permanent";
  settleConfirmed?: boolean;
  settleDelayMs?: number;
  /** Gen 2 owns a pending operation (the deps fixture): its reap waits on it bounded by the ambient
   *  budget, and joinOwnedOperations waits on it without bound. */
  pendingOperation?: boolean;
  settlementMs?: number;
  /** The retained switch's evidence-verification allowance (RunnerOptions.retainedSwitchVerificationMs). */
  verificationMs?: number;
  /** Gen 2 trips the switch and drives it through ctx.attemptCredentialSwitch (the executor's
   *  in-place path), with a taken approve that a pending switch refused. */
  inPlace?: boolean;
  /** The turn completes (successor completed) before the switch trips. */
  completedTurn?: boolean;
  holdFeature?: boolean;
  /** How the hold report misbehaves at the transport. */
  holdTransport?: "throw-once" | "throw-twice-applied" | "throw-twice-unapplied";
  settledTransport?: "throw-first";
  refuseHold?: boolean;
  /** Runs inside gen 2's settle (cancel / generation change during settlement). */
  duringSettle?: (flight: any, r: RunRunner) => void;
  /** The reap names six survivors, the sixth of a different class than the five the detail lists. */
  mixedSurvivors?: boolean;
  /** Runs after gen 2's own-mode quiescence proof returned clean (cancel / fence at the last check). */
  afterProof?: (flight: any, r: RunRunner) => void;
  /** An injected, deliberately large codexBoundaryDeadlineMs, to tell it from the verification allowance. */
  codexBoundaryDeadlineMs?: number;
  /** Block the first ownership read after the switch until it is aborted (verification deadline). */
  blockOwnership?: boolean;
}

async function scenario(o: Options) {
  const { claim, bare, clone } = await seed(o.iid);
  client.protocolFeatures = ["claim_generation_fence", ...(o.holdFeature === false ? [] : ["credential_switch_hold"])];
  (client as any).registerNonce = NONCE;
  client.publishCheckpoint = async () => { throw new Error("publication unknown"); };
  const events: string[] = [];
  const retainedHome = path.join(homeDir, claim.run_id);
  fs.mkdirSync(retainedHome, { recursive: true });
  fs.writeFileSync(path.join(retainedHome, "session"), "predecessor session");
  const survivors = new WorkerSurvivors();
  survivors.mixed = o.mixedSurvivors === true;
  const marks: { proofEnd?: number; cut?: number } = {};
  const pending = gate();
  if (!o.pendingOperation) pending.open();
  const ownOperation = gate();
  cleanups.push(pending.open, ownOperation.open);
  const counts = { models2: 0, models3: 0, joins: 0, holdAttempts: 0, settledAttempts: 0 };
  let flight: any;
  let before: any;
  let generation = 0;
  let blocked = false;
  let switched = false;
  const registry = new ActiveRunRegistry();
  const removeEntry = registry.remove.bind(registry);
  registry.remove = (runId: string) => { events.push("snapshot-remove"); return removeEntry(runId); };

  const factory: ExecutorFactory = runId => {
    // The first executor built is the retained generation-2 flight; the next is generation 3.
    const gen = ++generation + 1;
    const ex: any = {
      killAgentTree: () => { events.push(`kill:g${gen}`); },
      reapAttributedProcesses: async (): Promise<RunProcessReap> => {
        events.push(`reap:g${gen}`);
        // Only the flight's own post-switch proof is under test; earlier proofs (capture) are clean.
        if (gen === 2 && switched) {
          if (!await within(pending.promise)) return { killed: [], left: [], complete: false };
          if (o.survivor === "joined-then-reaped") {
            if (!await within(ownOperation.promise)) return { killed: [], left: [], complete: false };
            survivors.pids.clear();
          }
        }
        return survivors.reap();
      },
      settleWorkerCommands: async () => {
        events.push(`settle:g${gen}`);
        if (gen !== 2 || !switched) return { confirmed: true };
        o.duringSettle?.(flight, r);
        if (o.survivor === "joined-then-reaped") setTimeout(() => ownOperation.open(), 30);
        if (o.settleDelayMs) await new Promise(resolve => setTimeout(resolve, o.settleDelayMs));
        const confirmed = o.settleConfirmed !== false;
        if (confirmed && o.survivor === "settled-by-settle") survivors.pids.clear();
        return { confirmed };
      },
      joinOwnedOperations: async () => {
        events.push("join:start");
        counts.joins++;
        await pending.promise;
        events.push("join:end");
      },
      run: async (ctx: RunContext) => {
        if (gen > 2) { counts.models3++; events.push("model:g3"); throw new Error("fixture ends gen 3"); }
        counts.models2++;
        events.push("model:g2");
        try {
          if (o.survivor !== "none") survivors.pids.add(PID);
          if (o.completedTurn) await ctx.onModelTurnSettled!(1);
          before = readJournal(bare, clone.branch);
          events.push("switch");
          switched = true;
          flight.steering.tripCredentialSwitch(2);
          if (o.inPlace) {
            // The state a switch tripped by a refused approve leaves: every report but the
            // switch's own would otherwise throw the switch signal.
            flight.steering.approveRefusedBySwitchPending = () => true;
            await ctx.attemptCredentialSwitch!();
            throw new Error("in-place switch expected to stop the flight");
          }
          if (o.completedTurn) throw new CredentialSwitchSignal();
          await ctx.onModelTurnSettled!(1);
          throw new Error("switch signal expected");
        } finally { ex.killAgentTree(); }
      },
    };
    return { homeDir: path.join(homeDir, runId), executor: ex };
  };
  const { gitlab } = fakeGitlab();
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    events.push(`quiesce:${req.site}`);
    if (req.site === "retained_credential_switch") { marks.proofEnd = Date.now(); o.afterProof?.(flight, r); }
    return { process: { state: "quiescent", processes: [], killed: [], detail: "" }, docker: { state: "not_wired", removed: [], detail: "" } };
  };
  const log = recordingLogger();
  const r = runnerWith(factory, gitlab, undefined, log.logger, {
    recoveryRetryMs: 1, quiesceRun, dockerHost: "unix:///nonexistent-docker.sock", liveAttempts: new LiveAttemptRegistry(),
    activeRuns: registry,
    ...(o.settlementMs ? { retainedSwitchSettlementMs: o.settlementMs } : {}),
    ...(o.codexBoundaryDeadlineMs ? { codexBoundaryDeadlineMs: o.codexBoundaryDeadlineMs } : {}),
    ...(o.verificationMs ? { retainedSwitchVerificationMs: o.verificationMs } : {}),
  });
  const enter = (r as any).enterCredentialSwitch.bind(r);
  (r as any).enterCredentialSwitch = async (...args: any[]) => {
    const outcome = await enter(...args);
    events.push(`enter:${outcome}`);
    return outcome;
  };
  const phaseClone = (r as any).phaseClone.bind(r);
  (r as any).phaseClone = async (...args: any[]) => { flight = args[1]; return phaseClone(...args); };
  // Credentialed operations, recorded so a test can prove none ran after the switch tripped.
  const ensure = git.ensureClone.bind(git);
  git.ensureClone = (async (...args: Parameters<typeof ensure>) => { events.push("ensureClone"); return ensure(...args); }) as typeof git.ensureClone;
  const publish = client.publishCheckpoint.bind(client);
  client.publishCheckpoint = (async (...args: Parameters<typeof publish>) => { events.push("publishCheckpoint"); return publish(...args); }) as typeof client.publishCheckpoint;
  const capture = (r as any).captureRecoveryRestorePoint.bind(r);
  (r as any).captureRecoveryRestorePoint = async (...args: any[]) => { events.push("capture"); return capture(...args); };
  const real = client.reportState.bind(client);
  client.reportState = async (runId, body, signal) => {
    const status = body.status as string;
    if (status.startsWith("credential_switch")) events.push(`report:${status}`);
    if (status === HOLD) {
      counts.holdAttempts++;
      if (o.holdTransport === "throw-twice-unapplied") throw new Error("transport down");
      const ack = await real(runId, body, signal);
      if (o.holdTransport === "throw-once" && counts.holdAttempts === 1) throw new Error("ACK lost");
      if (o.holdTransport === "throw-twice-applied") throw new Error("ACK lost");
      return ack;
    }
    if (status === SETTLED) {
      counts.settledAttempts++;
      if (o.settledTransport === "throw-first" && counts.settledAttempts === 1) throw new Error("transport down");
    }
    return real(runId, body, signal);
  };
  const owner = client.getRunOwnership.bind(client);
  client.getRunOwnership = async (...args) => {
    if (o.blockOwnership && switched && !blocked) {
      blocked = true;
      const signal = args[1] instanceof AbortSignal ? args[1] : args[1]?.signal;
      assert.ok(signal, "verification passes its bounded signal");
      return new Promise<Awaited<ReturnType<typeof owner>>>((_resolve, reject) => {
        const aborted = () => { marks.cut = Date.now(); reject(signal.reason); };
        if (signal.aborted) aborted(); else signal.addEventListener("abort", aborted, { once: true });
      });
    }
    return owner(...args);
  };
  if (o.refuseHold) api.refuseHold(HOLD);
  const run = r.execute(claim);
  return {
    claim, bare, clone, r, run, events, marks, survivors, pending, ownOperation, counts, log, registry, retainedHome,
    flight: () => flight, before: () => before, wasBlocked: () => blocked,
    async gen3() {
      api.setOwnershipStatus(claim.run_id, "running", 3);
      await r.execute(gitlabClaim(o.iid, { run_id: claim.run_id, claim_generation: 3 }));
    },
  };
}
type Env = Awaited<ReturnType<typeof scenario>>;

const reports = (runId: string, ...statuses: string[]) =>
  api.states.filter(s => s.runId === runId && statuses.includes(s.body.status));
const lastFailed = (runId: string) => api.states.filter(s => s.runId === runId && s.body.status === "failed").at(-1)?.body;
const feedText = (runId: string) => JSON.stringify(api.messages(runId));

/** Source, HOME and journal custody is exactly what it was when the switch tripped. */
function assertCustodyKept(env: Env, journalUnchanged = true): void {
  const journal = readJournal(env.bare, env.clone.branch);
  assert.equal(fs.readFileSync(path.join(env.clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  assert.equal(fs.readFileSync(path.join(journal.clonePath, "retained.txt"), "utf8"), "only local dirty work\n");
  assert.equal(fs.readFileSync(path.join(env.retainedHome, "session"), "utf8"), "predecessor session");
  if (journalUnchanged) assert.deepEqual(journal, env.before(), "the recovery journal is unchanged");
}

/** No credentialed operation ran once the switch tripped; the settle and the quiescence proof
 *  precede the first switch report (or none was sent). */
function assertNoCredentialedOps(env: Env, proven = true): void {
  const after = env.events.slice(env.events.indexOf("switch") + 1);
  for (const op of ["ensureClone", "publishCheckpoint", "capture", "model:g2", "model:g3"])
    assert.ok(!after.includes(op), `${op} ran after the switch tripped: ${after.join(",")}`);
  const firstReport = env.events.findIndex(e => e.startsWith("report:"));
  if (firstReport >= 0) {
    const settle = env.events.indexOf("settle:g2");
    const proof = env.events.indexOf("quiesce:retained_credential_switch");
    assert.ok(settle >= 0 && settle < firstReport, `settle precedes the first report: ${env.events.join(",")}`);
    if (proven) assert.ok(proof >= 0 && proof < firstReport, "the quiescence proof precedes the first report");
  }
}

describe("issue #3181 m3: retained credential switch settles before it reports", { skip: process.platform !== "linux" }, () => {
  // Outcome 1 (the regression that is red on the base): the survivor only goes when gen 2 drives
  // its own settle, so gen 3's predecessor proof sees it unless the switch settled first.
  for (const survivor of ["none", "settled-by-settle"] as const) {
    it(`1. ${survivor === "none" ? "control: with no survivor" : "a settled gen-2 switch leaves no worker survivor for"} gen 3's predecessor proof`, TIMEOUT, async () => {
      const env = await scenario({ iid: survivor === "none" ? 31811 : 31810, survivor });
      await env.run;
      assert.equal(reports(env.claim.run_id, "credential_switch").length, 1, "gen 2 released the switch");
      assert.equal(reports(env.claim.run_id, HOLD).length, 0);
      assertNoCredentialedOps(env);
      assertCustodyKept(env);
      assert.equal(env.counts.joins, 0, "a settled flight joins nothing");
      await env.gen3();
      assert.equal(lastFailed(env.claim.run_id)?.fail_origin, undefined, JSON.stringify(lastFailed(env.claim.run_id)));
      assert.equal(env.counts.models3, 1, "gen 3 reached the model");
    });
  }

  it("2. a verification deadline after a proven settlement stops as today, with no hold", TIMEOUT, async () => {
    // The settlement outlasts the verification allowance (60ms) yet fits its own (30s): the
    // verification clock starts only after settlement returns. Only the verification allowance is
    // shortened, so the unrelated recovery discovery never depends on wall-clock under load.
    const env = await scenario({ iid: 31820, survivor: "settled-by-settle", settleDelayMs: 200, verificationMs: 60, settlementMs: 30_000, blockOwnership: true, codexBoundaryDeadlineMs: 600_000 });
    await env.run;
    // The cut comes from retainedSwitchVerificationMs, not the (injected, 10 minute) codex boundary deadline.
    assert.ok(env.marks.proofEnd !== undefined && env.marks.cut !== undefined, "both marks recorded");
    assert.ok(env.marks.cut - env.marks.proofEnd < 30_000, `cut ${env.marks.cut - env.marks.proofEnd}ms after settlement`);
    assert.equal(env.wasBlocked(), true, "the verification ownership read ran and was cut by the deadline");
    assert.deepEqual(reports(env.claim.run_id, "credential_switch", "credential_switch_failed", HOLD, SETTLED), []);
    assert.equal(env.flight().retainedSwitchSettlement, undefined);
    assert.ok(env.events.includes("settle:g2") && env.events.includes("quiesce:retained_credential_switch"));
    assertNoCredentialedOps(env);
    assertCustodyKept(env);
  });

  it("3. an incomplete settlement holds: no credential_switch report, parked, nothing reclaimed", TIMEOUT, async () => {
    const env = await scenario({ iid: 31830, survivor: "permanent", settleConfirmed: false });
    await env.run;
    assert.deepEqual(reports(env.claim.run_id, "credential_switch", "credential_switch_failed"), []);
    const held = api.holdRequests.filter(h => h.status === HOLD);
    assert.deepEqual(held.map(h => [h.claim_generation, h.register_nonce]), [[2, NONCE]]);
    assert.equal(env.flight().parked, true);
    assert.equal(env.flight().retainedSwitchSettlement, "held");
    for (const flag of ["preserveRecoveryClone", "preserveSession", "retainedLocalCustody", "retainedEpisodeCustody"])
      assert.equal(env.flight()[flag], true, flag);
    assert.match(feedText(env.claim.run_id), /credential switch held: run processes could not be proven stopped \(settle unconfirmed\); work and custody kept on this worker\. Resume or Cancel/);
    assert.equal(api.holdState(env.claim.run_id), "unsettled", "the flight ended and acknowledged the hold settled");
    assert.equal(env.counts.joins, 1);
    assert.equal(api.states.some(s => s.body.status === "failed"), false, "no terminal report");
    assertNoCredentialedOps(env, false);
    assertCustodyKept(env);
  });

  it("3c. the in-place path: a switch tripped by a refused approve still delivers the hold and stops held", TIMEOUT, async () => {
    // ctx.attemptCredentialSwitch (not the safety-net arm): enterCredentialSwitch returns
    // retained_held and the flight stops with CredentialSwitchRetainedStop(held). The pending
    // refused approve must not abort the hold report with the switch signal.
    const env = await scenario({ iid: 31832, survivor: "permanent", settleConfirmed: false, inPlace: true });
    await env.run;
    assert.ok(env.events.includes("enter:retained_held"), `in-place outcome: ${env.events.join(",")}`);
    assert.equal(reports(env.claim.run_id, HOLD).length, 1, "the hold report reached the server");
    assert.deepEqual(reports(env.claim.run_id, "credential_switch", "credential_switch_failed"), []);
    assert.equal(api.holdState(env.claim.run_id), "unsettled");
    assert.equal(env.flight().retainedSwitchSettlement, "held");
    assert.equal(env.flight().parked, true);
    assert.ok(env.log.lines.some(l => (l as { msg: string }).msg === "credential switch held; run processes unproven, work and custody kept for an owner Resume or Cancel"));
    assert.equal(api.states.some(s => s.body.status === "failed"), false, "no terminal report");
    assertNoCredentialedOps(env, false);
    assertCustodyKept(env);
  });

  it("3d. an unconfirmed hold says so in the feed and never claims a hold", TIMEOUT, async () => {
    const env = await scenario({ iid: 31833, survivor: "permanent", settleConfirmed: false, refuseHold: true });
    await env.run;
    assert.doesNotMatch(feedText(env.claim.run_id), /credential switch held/);
    assert.match(feedText(env.claim.run_id), /hold not confirmed, work and custody kept/);
    assert.doesNotMatch(feedText(env.claim.run_id), /777001/);
  });

  it("3b. a quiescence proof that names a survivor holds and names its class in the feed", TIMEOUT, async () => {
    const env = await scenario({ iid: 31831, survivor: "permanent" });
    await env.run;
    assert.deepEqual(reports(env.claim.run_id, "credential_switch", "credential_switch_failed"), []);
    assert.equal(env.flight().retainedSwitchSettlement, "held");
    assert.match(feedText(env.claim.run_id), /\(survivors: worker_child\); work and custody kept/);
    assert.doesNotMatch(feedText(env.claim.run_id), /777001|HOME-attributed/, "no pid or raw detail in the feed");
    assertCustodyKept(env);
  });

  it("3e. a sixth survivor's class still reaches the feed, without any pid", TIMEOUT, async () => {
    const env = await scenario({ iid: 31834, survivor: "permanent", mixedSurvivors: true });
    await env.run;
    assert.equal(env.flight().retainedSwitchSettlement, "held");
    assert.match(feedText(env.claim.run_id), /\(survivors: worker_child, kill_failed\); work and custody kept/);
    assert.doesNotMatch(feedText(env.claim.run_id), /77700\d|HOME-attributed/, "no pid or raw detail in the feed");
  });

  it("4. a blocked owning-operation join holds, keeps the tail reserved and acknowledges only after the join", TIMEOUT, async () => {
    const env = await scenario({ iid: 31840, survivor: "none", pendingOperation: true, settlementMs: 80 });
    const id = env.claim.run_id;
    await until("the hold report", () => api.holdState(id) === "settling").catch(e => { throw new Error(`${e.message}: ${env.events.join(",")} :: ${JSON.stringify(env.log.lines.slice(-6))}`); });
    await until("the join to start", () => env.events.includes("join:start"));
    // The join is outstanding: the flight is still executing and nothing has been released.
    assert.equal(env.r.isExecuting(id), true, "the execution tail stays reserved");
    assert.equal(env.registry.has(id), true, "the snapshot entry stays");
    assert.equal(api.holdRequests.filter(h => h.status === SETTLED).length, 0, "no settled acknowledgement yet");
    assert.equal(api.ownerResume(id, 3), "refused", "an owner Resume is refused while settling");
    assert.deepEqual(reports(id, "credential_switch", "credential_switch_failed"), []);
    assertCustodyKept(env);
    // A second same-worker claim cannot start next to the unjoined operation.
    const second = env.gen3();
    await new Promise(resolve => setTimeout(resolve, 100));
    assert.equal(env.counts.models3, 0, "gen 3 does not start while the join is outstanding");
    env.pending.open();
    await env.run;
    const joinEnd = env.events.indexOf("join:end");
    assert.ok(joinEnd > 0 && env.events.indexOf(`report:${SETTLED}`) > joinEnd, "the acknowledgement follows the join");
    assert.ok(env.events.indexOf("snapshot-remove") > joinEnd, "the snapshot entry is removed after the join");
    assert.equal(api.holdState(id), "unsettled");
    await second.catch(() => undefined);
    assertCustodyKept(env, false);
  });

  it("4v. a settled acknowledgement the transport lost is delivered by the next heartbeat retry", TIMEOUT, async () => {
    const env = await scenario({ iid: 31841, survivor: "none", pendingOperation: true, settlementMs: 80, settledTransport: "throw-first" });
    const id = env.claim.run_id;
    await until("the join to start", () => env.events.includes("join:start"));
    env.pending.open();
    await env.run;
    assert.equal(env.counts.settledAttempts, 1, "the first delivery failed");
    assert.equal(api.holdState(id), "settling", "the hold is still settling");
    assert.equal(api.ownerResume(id, 3), "refused");
    await env.r.retryPendingHoldSettled();
    assert.equal(env.counts.settledAttempts, 2);
    assert.equal(api.holdState(id), "unsettled");
    await env.r.retryPendingHoldSettled();
    assert.equal(env.counts.settledAttempts, 2, "an applied acknowledgement is not retried");
    assert.equal(api.ownerResume(id, 3), "resumed");
  });

  it("4w. a definitively refused settled acknowledgement is dropped, not retried", TIMEOUT, async () => {
    const env = await scenario({ iid: 31842, survivor: "none", pendingOperation: true, settlementMs: 80, settledTransport: "throw-first" });
    await until("the join to start", () => env.events.includes("join:start"));
    api.refuseHold(SETTLED);
    env.pending.open();
    await env.run;
    await env.r.retryPendingHoldSettled();
    assert.equal(env.counts.settledAttempts, 2);
    await env.r.retryPendingHoldSettled();
    assert.equal(env.counts.settledAttempts, 2, "a refusal drops the pending acknowledgement");
  });

  describe("4b. ordinary flights never wait for owned operations", () => {
    it("a settled and released retained flight does not join an operation", TIMEOUT, async () => {
      const env = await scenario({ iid: 31850, survivor: "settled-by-settle" });
      await env.run;
      assert.equal(env.counts.joins, 0);
      assert.ok(env.events.includes("snapshot-remove"));
      assert.equal(env.r.isExecuting(env.claim.run_id), false);
      assert.equal(env.events.includes(`report:${SETTLED}`), false);
    });

    it("an ordinary failing run completes while its executor has a pending operation", TIMEOUT, async () => {
      const claim = gitlabClaim(31851);
      let joins = 0;
      const pending = gate();
      const registry = new ActiveRunRegistry();
      const events: string[] = [];
      const removeEntry = registry.remove.bind(registry);
      registry.remove = (runId: string) => { events.push("snapshot-remove"); return removeEntry(runId); };
      const factory: ExecutorFactory = runId => ({ homeDir: path.join(homeDir, runId), executor: {
        joinOwnedOperations: async () => { joins++; await pending.promise; },
        run: async () => { throw new Error("fixture agent failure"); },
      } });
      const { gitlab } = fakeGitlab();
      const r = runnerWith(factory, gitlab, undefined, nullLogger(), { activeRuns: registry });
      await r.execute(claim);
      assert.equal(pending.opened, false, "the operation is still pending");
      assert.equal(joins, 0, "an ordinary flight never calls joinOwnedOperations");
      assert.deepEqual(events, ["snapshot-remove"]);
      assert.equal(r.isExecuting(claim.run_id), false);
      pending.open();
    });
  });

  describe("5. hold acknowledgement convergence", () => {
    it("a lost hold ACK converges on the identical retry", TIMEOUT, async () => {
      const env = await scenario({ iid: 31860, survivor: "permanent", settleConfirmed: false, holdTransport: "throw-once" });
      await env.run;
      assert.equal(env.counts.holdAttempts, 2);
      assert.equal(env.flight().retainedSwitchSettlement, "held");
      assert.equal(api.holdRequests.filter(h => h.status === HOLD).length, 2, "one identical retry");
      assertCustodyKept(env);
    });

    it("two lost hold ACKs converge on an ownership read showing the settling hold", TIMEOUT, async () => {
      const env = await scenario({ iid: 31861, survivor: "permanent", settleConfirmed: false, holdTransport: "throw-twice-applied" });
      await env.run;
      assert.equal(env.counts.holdAttempts, 2);
      assert.equal(env.flight().retainedSwitchSettlement, "held");
      assert.equal(env.flight().parked, true);
      assertCustodyKept(env);
    });

    it("an unapplied hold the ownership read cannot confirm is a residual stop, not a hold", TIMEOUT, async () => {
      const env = await scenario({ iid: 31862, survivor: "permanent", settleConfirmed: false, holdTransport: "throw-twice-unapplied" });
      await env.run;
      assert.equal(env.flight().retainedSwitchSettlement, "unproven");
      assert.equal(env.flight().parked, false);
      assert.ok(env.log.lines.some(l => (l as { msg: string }).msg === "retained credential switch hold unconfirmed"));
      assert.equal(api.holdState(env.claim.run_id), undefined);
      assert.deepEqual(reports(env.claim.run_id, "credential_switch", "credential_switch_failed", SETTLED), []);
      assert.equal(env.counts.joins, 1, "an unproven flight still joins before releasing its slots");
      assertCustodyKept(env);
    });

    it("a refused hold is a residual non-terminal stop with no reclaim", TIMEOUT, async () => {
      const env = await scenario({ iid: 31863, survivor: "permanent", settleConfirmed: false, refuseHold: true });
      await env.run;
      assert.equal(env.counts.holdAttempts, 1, "a refusal is not retried");
      assert.equal(env.flight().retainedSwitchSettlement, "unproven");
      assert.equal(env.flight().parked, false);
      assert.ok(env.log.lines.some(l => (l as { msg: string }).msg === "retained credential switch hold unconfirmed"));
      assert.deepEqual(reports(env.claim.run_id, "credential_switch", "credential_switch_failed", SETTLED), []);
      assert.equal(api.states.some(s => s.body.status === "failed"), false);
      assertCustodyKept(env);
    });

    it("an api without the credential_switch_hold feature gets no hold report", TIMEOUT, async () => {
      const env = await scenario({ iid: 31864, survivor: "permanent", settleConfirmed: false, holdFeature: false });
      await env.run;
      assert.equal(env.counts.holdAttempts, 0);
      assert.equal(env.flight().retainedSwitchSettlement, "unproven");
      assert.ok(env.log.lines.some(l => (l as { msg: string }).msg === "retained credential switch hold unconfirmed"));
      assert.doesNotMatch(feedText(env.claim.run_id), /credential switch held/);
      assertCustodyKept(env);
    });
  });

  describe("6. lifecycle precedence during settlement", () => {
    it("an owner cancel during settlement keeps cancel precedence and reports no hold", TIMEOUT, async () => {
      const env = await scenario({ iid: 31870, survivor: "permanent", settleConfirmed: false,
        duringSettle: flight => { flight.steering.isCancelled = () => true; } });
      await env.run;
      assert.deepEqual(reports(env.claim.run_id, HOLD, SETTLED, "credential_switch", "credential_switch_failed"), []);
      assert.equal(env.flight().retainedSwitchSettlement, "unproven", "an unsettled flight is joined even though no hold was reported");
      assert.equal(env.counts.joins, 1);
      assert.equal(api.holdState(env.claim.run_id), undefined);
      assertCustodyKept(env);
    });

    it("a claim-fence change during settlement reports no hold", TIMEOUT, async () => {
      const env = await scenario({ iid: 31871, survivor: "permanent", settleConfirmed: false,
        duringSettle: flight => { flight.steering.claimFence = () => ({ kind: "superseded" }); } });
      await env.run;
      assert.deepEqual(reports(env.claim.run_id, HOLD, SETTLED, "credential_switch", "credential_switch_failed"), []);
      assert.equal(env.flight().retainedSwitchSettlement, "unproven", "an unsettled flight is joined even though no hold was reported");
      assert.equal(env.counts.joins, 1);
      assertCustodyKept(env);
    });

    it("a cancel at the last lifecycle check after a proven settlement does not join or hold", TIMEOUT, async () => {
      const env = await scenario({ iid: 31874, survivor: "settled-by-settle",
        afterProof: flight => { flight.steering.isCancelled = () => true; } });
      await env.run;
      assert.deepEqual(reports(env.claim.run_id, HOLD, SETTLED, "credential_switch", "credential_switch_failed"), []);
      assert.notEqual(env.flight().retainedSwitchSettlement, "unproven", "a proven settlement is never relabelled");
      assert.equal(env.counts.joins, 0, "a settled flight joins nothing");
      assert.doesNotMatch(JSON.stringify(env.log.lines), /settlement did not finish/);
      assertCustodyKept(env);
    });

    it("a claim-fence change at the last check after a proven settlement does not join or hold", TIMEOUT, async () => {
      const env = await scenario({ iid: 31875, survivor: "settled-by-settle",
        afterProof: flight => { flight.steering.claimFence = () => ({ kind: "superseded" }); } });
      await env.run;
      assert.deepEqual(reports(env.claim.run_id, HOLD, SETTLED, "credential_switch", "credential_switch_failed"), []);
      assert.notEqual(env.flight().retainedSwitchSettlement, "unproven");
      assert.equal(env.counts.joins, 0);
      assertCustodyKept(env);
    });

    it("a generation change during settlement reports no hold", TIMEOUT, async () => {
      const env = await scenario({ iid: 31872, survivor: "permanent", settleConfirmed: false,
        duringSettle: flight => { flight.steering.pendingCredentialSwitch = () => 3; } });
      await env.run;
      assert.deepEqual(reports(env.claim.run_id, HOLD, SETTLED, "credential_switch", "credential_switch_failed"), []);
      assert.equal(env.flight().retainedSwitchSettlement, "unproven", "an unsettled flight is joined even though no hold was reported");
      assert.equal(env.counts.joins, 1);
      assertCustodyKept(env);
    });

    it("a worker shutdown during settlement reports no hold", TIMEOUT, async () => {
      const env = await scenario({ iid: 31873, survivor: "permanent", settleConfirmed: false,
        duringSettle: (_flight, r) => { (r as any).shuttingDownGlobal = true; } });
      await env.run;
      assert.deepEqual(reports(env.claim.run_id, HOLD, SETTLED, "credential_switch", "credential_switch_failed"), []);
      assertCustodyKept(env);
    });
  });

  it("7. a completed retained successor settles before its switch report", TIMEOUT, async () => {
    const env = await scenario({ iid: 31880, survivor: "settled-by-settle", completedTurn: true });
    await env.run;
    assert.ok(env.flight().completedRetainedSuccessor, "the successor completed its model turn");
    assert.equal(reports(env.claim.run_id, HOLD).length, 0);
    assert.equal(reports(env.claim.run_id, "credential_switch", "credential_switch_failed").length, 1);
    assertNoCredentialedOps(env);
    assert.equal(fs.readFileSync(path.join(env.clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
    assert.equal(fs.readFileSync(path.join(env.retainedHome, "session"), "utf8"), "predecessor session");
  });

  it("8. a survivor that is only this flight's own worker child is joined, reaped, and released", TIMEOUT, async () => {
    const env = await scenario({ iid: 31890, survivor: "joined-then-reaped" });
    await env.run;
    assert.equal(reports(env.claim.run_id, "credential_switch").length, 1, "released");
    assert.equal(reports(env.claim.run_id, HOLD).length, 0);
    assert.equal(env.survivors.pids.size, 0);
    assertNoCredentialedOps(env);
    assertCustodyKept(env);
    await env.gen3();
    assert.equal(env.counts.models3, 1);
  });

  it("9. a permanently unverified survivor holds; after Resume gen 3's predecessor proof blocks with custody kept", TIMEOUT, async () => {
    const env = await scenario({ iid: 31900, survivor: "permanent" });
    await env.run;
    const id = env.claim.run_id;
    assert.equal(api.holdState(id), "unsettled");
    assert.equal(api.ownerResume(id, 3), "resumed");
    await env.r.execute(gitlabClaim(31900, { run_id: id, claim_generation: 3 }));
    const failed = lastFailed(id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked", JSON.stringify(failed));
    assert.equal(env.counts.models3, 0, "gen 3 never reaches the model");
    assertCustodyKept(env, false);
  });
});

describe("issue #3181 m3: the wire contract", () => {
  it("a hold report carries the registration nonce and parses the held disposition", TIMEOUT, async () => {
    const runId = "00000000-0000-4000-8000-000000031910";
    client.protocolFeatures = ["credential_switch_hold"];
    (client as any).registerNonce = NONCE;
    api.setOwnershipStatus(runId, "running", 2);
    const ack = await client.reportState(runId, { status: HOLD, claim_generation: 2 });
    assert.equal(ack.credentialSwitchHeld, true);
    assert.deepEqual(api.holdRequests.map(h => [h.status, h.claim_generation, h.register_nonce]), [[HOLD, 2, NONCE]]);
    const settled = await client.reportState(runId, { status: SETTLED, claim_generation: 2 });
    assert.equal(settled.credentialSwitchHoldSettled, true);
    const refused = await client.reportState(runId, { status: SETTLED, claim_generation: 9 });
    assert.equal(refused.applied, false);
    assert.equal(refused.credentialSwitchHoldSettled, undefined);
  });

  it("a hold report needs the feature, a registration nonce and a positive generation", TIMEOUT, async () => {
    const runId = "00000000-0000-4000-8000-000000031911";
    client.protocolFeatures = [];
    (client as any).registerNonce = NONCE;
    await assert.rejects(client.reportState(runId, { status: HOLD, claim_generation: 2 }), /credential_switch_hold feature/);
    client.protocolFeatures = ["credential_switch_hold"];
    (client as any).registerNonce = undefined;
    await assert.rejects(client.reportState(runId, { status: HOLD, claim_generation: 2 }), /registration nonce/);
    (client as any).registerNonce = NONCE;
    await assert.rejects(client.reportState(runId, { status: HOLD }), /positive claim_generation/);
    assert.equal(api.holdRequests.length, 0, "nothing reached the wire");
  });
});

describe("issue #3181 m3: SdkExecutor bounded reap and join", () => {
  const scratch = () => fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3181-sdk-"));
  const executor = (reap: () => Promise<RunProcessReap>) => new SdkExecutor(nullLogger(), scratch(), {
    runProcesses: { scan: async () => ({ pids: [], complete: true }), reap },
  });
  const withOperations = (exec: SdkExecutor, ops: { lane?: Promise<void>; deps?: Promise<unknown> }) => {
    const aborted = { count: 0 };
    const abort = new AbortController();
    abort.signal.addEventListener("abort", () => { aborted.count++; });
    if (ops.lane) (exec as any).laneDisposal = ops.lane;
    if (ops.deps) { (exec as any).activeDeps = { abort, install: ops.deps }; (exec as any).depsJoined = false; }
    return aborted;
  };

  for (const which of ["lane", "deps"] as const) {
    it(`under a spent budget a pending ${which} operation reads as an incomplete reap and keeps running`, TIMEOUT, async () => {
      let reaps = 0;
      const exec = executor(async () => { reaps++; return { killed: [], left: [], complete: true }; });
      const op = gate();
      withOperations(exec, which === "lane" ? { lane: op.promise } : { deps: op.promise });
      const started = Date.now();
      const budget = new MaintenanceBudget(60);
      try {
        const reap = await MaintenanceBudget.run(budget, () => RetainedSwitchSettlement.run(() => exec.reapAttributedProcesses()));
        assert.deepEqual(reap, { killed: [], left: [], complete: false });
      } finally { budget.dispose(); }
      assert.ok(Date.now() - started < 5_000, "the wait was bounded by the budget");
      assert.equal(reaps, 0, "the process reap never ran over an unjoined operation");
      assert.equal((exec as any).depsJoined, which === "lane", "an unjoined deps install is not marked joined");
      op.open();
      // The operation was neither cancelled nor lost: with no budget the same reap now completes.
      assert.deepEqual(await exec.reapAttributedProcesses(), { killed: [], left: [], complete: true });
      assert.equal(reaps, 1);
    });
  }

  it("a budgeted reap outside the retained-switch settlement (cancellation maintenance) still waits unbounded", TIMEOUT, async () => {
    const exec = executor(async () => ({ killed: [], left: [], complete: true }));
    const op = gate();
    withOperations(exec, { lane: op.promise });
    const budget = new MaintenanceBudget(40);
    try {
      let done = false;
      const reap = MaintenanceBudget.run(budget, () => exec.reapAttributedProcesses()).then(r => { done = true; return r; });
      await new Promise(resolve => setTimeout(resolve, 150));
      assert.equal(done, false, "the budget alone does not bound the join");
      op.open();
      assert.deepEqual(await reap, { killed: [], left: [], complete: true });
    } finally { budget.dispose(); }
  });

  it("without a budget the reap waits for the pending operation, as before", TIMEOUT, async () => {
    const exec = executor(async () => ({ killed: [], left: [], complete: true }));
    const op = gate();
    withOperations(exec, { lane: op.promise });
    let done = false;
    const reap = exec.reapAttributedProcesses().then(r => { done = true; return r; });
    await new Promise(resolve => setTimeout(resolve, 80));
    assert.equal(done, false, "no budget, no give-up");
    op.open();
    assert.deepEqual(await reap, { killed: [], left: [], complete: true });
  });

  it("joinOwnedOperations waits for the lane and deps without aborting or reaping anything", TIMEOUT, async () => {
    let reaps = 0;
    const exec = executor(async () => { reaps++; return { killed: [], left: [], complete: true }; });
    const lane = gate();
    const deps = gate();
    const aborted = withOperations(exec, { lane: lane.promise, deps: deps.promise });
    let joined = false;
    const join = exec.joinOwnedOperations().then(() => { joined = true; });
    await new Promise(resolve => setTimeout(resolve, 60));
    lane.open();
    await new Promise(resolve => setTimeout(resolve, 60));
    assert.equal(joined, false, "still waiting for the deps install");
    deps.open();
    await join;
    assert.equal(aborted.count, 0, "the join aborts nothing");
    assert.equal(reaps, 0, "the join kills nothing");
    assert.equal((exec as any).depsJoined, true);
  });
});

describe("issue #3181 m3: the heartbeat retries pending hold acknowledgements", () => {
  it("calls the runner's retry on a successful tick", TIMEOUT, async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3181-hb-"));
    const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    let retries = 0;
    const runner = {
      resumePendingRecoveries: async () => {},
      resumeLiveRecoveries: async () => {},
      retryPendingHoldSettled: async () => { retries++; },
      execute: async () => {},
    } as unknown as RunRunner;
    const wclient = {
      register: async () => ({}), heartbeat: async () => {}, reportState: async () => ({ applied: true, status: "completed" }),
      claimRun: async (): Promise<ClaimResponse | null> => null, claimChat: async () => null,
      hasFeature: () => false, getMessageGaps: async () => ({ gaps: [] }), postMessages: async () => {},
    } as unknown as WorkerClient;
    const noop = { execute: async () => {} };
    const worker = new Worker(
      { workerName: "w1", workerTemplate: "base", pollIntervalMs: 1, heartbeatIntervalMs: 5, chatPollMs: 1, chatSessions: 1,
        maxConcurrentRuns: 1, dockerWiring: {}, dataDir: root, gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20 } as unknown as Config,
      wclient, runner, noop as unknown as ChatRunner, noop as unknown as JudgeRunner, noop as unknown as ReviewRunner,
      nullLogger(), () => ({ ok: true, missing: [] }), outbox, new Map(),
      new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32),
    );
    const controller = new AbortController();
    const done = worker.run(controller.signal);
    try { await until("two heartbeat retries", () => retries >= 2); }
    finally { controller.abort(); await done.catch(() => undefined); fs.rmSync(root, { recursive: true, force: true }); }
    assert.ok(retries >= 2);
  });
});
