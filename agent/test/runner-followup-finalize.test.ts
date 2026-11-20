// Issue #4479 (M2): an interactive task park whose follow-up wait ends idle or stopped moves the
// server's awaiting_followup row back to running (`followup_finalize`) before the worker finalizes,
// or the publication intent refuses it and the #3846 enrollment loop waits forever.
//
// A small server model answers the park ACK (with followup_park_seq), the admission and the
// publication intent the way the api does. Every test ends at an assertion deadline, never a hang:
// a run that does not finish is cancelled and the test fails with the observed states.
import { execFileSync } from "node:child_process";
import { AsyncLocalStorage } from "node:async_hooks";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import type { Executor, FollowUpOutcome, RunContext } from "../src/executor.js";
import { FOLLOWUP_FINALIZE_ADMISSION_FEATURE } from "../src/client.js";
import { ReportPreconditionError } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import { REASON_FOLLOWUP_NOT_PARKED, RunRunner, StaleClaimError } from "../src/runner.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { CredentialSwitchSignal, PauseNowSignal, SteeringChannel } from "../src/steering.js";
import type { ClaimResponse, StateAck, StateRequest } from "../src/protocol.js";
import { api, client, fakeGitlab, fx, git, installHarness } from "./runner-harness.js";
import { makeClaim, nullLogger } from "./helpers.js";

installHarness({}, cleanupTest);

const GEN = 4;
const REPO_ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
const BASE_FEATURES = ["publication_resume_v1", "recovery_park_cause"];
const FEATURES = [...BASE_FEATURES, FOLLOWUP_FINALIZE_ADMISSION_FEATURE];
const GENERIC_400 = "state must be one of running, awaiting_approval, awaiting_input, awaiting_followup, limit_wait, recovery_wait, paused, pause_failed, credential_switch, credential_switch_failed, credential_switch_held, credential_switch_hold_settled, credential_switch_uncertain, clarification_settled, followup_finalize, completed, failed";
/** One client send is 1 attempt plus the harness's two transient retries. */
const ATTEMPTS = 3;

const roots: string[] = [];
afterEach(async () => {
  for (const r of roots.splice(0)) await fsp.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

// Issue #4536: no outcome here depends on real timer ordering. The run's SteeringChannel reads a
// frozen FakeClock (RunnerOptions.steeringNow), the follow-up idle window is crossed by advancing
// that clock at arm time, and the admission loop's recovery backoff waits are fake waits the test
// advances. Real-time waits below (until/finish/sleep) are only readiness polling and failure caps.
interface FakeWait { deadline: number; check(): void; wake(): void }

class FakeClock {
  private t = Date.now();
  private readonly waits = new Set<FakeWait>();
  private fake = false;
  private released = false;
  readonly now = (): number => this.t;
  get engaged(): boolean { return this.fake && !this.released; }
  engage(): void { this.fake = true; }
  get pending(): number { return this.waits.size; }
  nextDeadline(): number | undefined {
    let min: number | undefined;
    for (const w of this.waits) min = min === undefined ? w.deadline : Math.min(min, w.deadline);
    return min;
  }
  advance(ms: number): void {
    this.t += ms;
    for (const w of this.waits) w.check();
  }
  /** Resolves at the fake deadline, or early when `signal` aborts or `shouldWake()` holds on an advance. */
  wait(ms: number, signal: AbortSignal | undefined, shouldWake: () => boolean): Promise<void> {
    return new Promise<void>(resolve => {
      const w: FakeWait = {
        deadline: this.t + ms,
        check: () => { if (this.t >= w.deadline || shouldWake()) w.wake(); },
        wake: () => {
          if (!this.waits.delete(w)) return;
          signal?.removeEventListener("abort", w.wake);
          resolve();
        },
      };
      this.waits.add(w);
      signal?.addEventListener("abort", w.wake, { once: true });
      w.check();
    });
  }
  /** Resolves every pending wait; later waits delegate to the real implementation. */
  release(): void {
    this.released = true;
    for (const w of this.waits) w.wake();
  }
}

const RECOVERY_RETRY_MS = 5;
/** The follow-up idle bound the parks arm; only the FakeClock crosses it. */
const IDLE_MS = 40;
const realAwaitFollowUp = SteeringChannel.prototype.awaitFollowUp;
let clock = new FakeClock();
let armPolicy: (armIndex: number) => boolean = () => true;
let armCount = 0;
let currentSteering: SteeringChannel | undefined;
const executions: Array<{ runner: RunRunner; promise: Promise<void> }> = [];

/** Join before installHarness removes the API and clone paths, including after a thrown assertion. */
async function cleanupTest(): Promise<void> {
  try {
    clock.release();
    for (const { runner } of executions) runner.shutdown();
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      await Promise.race([
        Promise.allSettled(executions.map(e => e.promise)),
        new Promise<never>((_, reject) => {
          timer = setTimeout(() => reject(new Error("followup-finalize execution did not settle during fixture cleanup")), 5_000);
        }),
      ]);
    } finally {
      if (timer) clearTimeout(timer);
    }
    assert.equal(clock.pending, 0, "fixture cleanup left fake waits pending");
  } finally {
    SteeringChannel.prototype.awaitFollowUp = realAwaitFollowUp;
    executions.length = 0;
  }
}

function startExecution(runner: RunRunner, claim: ClaimResponse): Promise<void> {
  const promise = runner.execute(claim);
  executions.push({ runner, promise });
  // Teardown observes failures without replacing the original promise's rejection for its caller.
  void promise.catch(() => undefined);
  return promise;
}

beforeEach(() => {
  const c = clock = new FakeClock();
  armPolicy = () => true;
  armCount = 0;
  currentSteering = undefined;
  SteeringChannel.prototype.awaitFollowUp = function (this: SteeringChannel, idleMs: number) {
    const parked = realAwaitFollowUp.call(this, idleMs); // throws synchronously on a double arm
    // eslint-disable-next-line typescript/no-this-alias
    currentSteering = this;
    const armIndex = ++armCount;
    if (armPolicy(armIndex)) c.advance(idleMs);
    return parked;
  };
});

/** Stop and cancel cases: the park must end by that input, never by the idle window. */
const neverIdle = (): void => { armPolicy = () => false; };

it("fixture cleanup releases a fake wait abandoned by a failing operation", async t => {
  const c = clock;
  c.engage();
  const pending = c.wait(5, undefined, () => false);
  const original = new Error("injected failure before the explicit clock release");
  await assert.rejects(async () => { throw original; }, error => error === original);
  // t.after runs after the harness's afterEach, so this observes the shared teardown.
  t.after(async () => {
    const remaining = c.pending;
    c.release(); // The regression must also clean up when run against the unfixed hooks.
    await pending;
    assert.equal(remaining, 0, "fixture cleanup left a fake wait pending");
    assert.equal(SteeringChannel.prototype.awaitFollowUp, realAwaitFollowUp, "fixture cleanup did not restore the prototype");
  });
});

async function mkOutbox(): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "runner-followup-finalize-"));
  roots.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return outbox;
}

/** What the model answers one followup_finalize HTTP request with. `apply:false` keeps the row parked. */
type Reply =
  | { kind: "admitted"; apply?: boolean }
  | { kind: "lost" }
  | { kind: "refuse"; reason: string; apply?: boolean }
  | { kind: "stale" }
  | { kind: "raw"; status: number; body: unknown; apply?: boolean };

interface Model {
  status: string;
  parkSeq: number;
  /** What the park ACK echoes: a number, or any raw value for the malformed cases, or `absent`. */
  parkAck: "seq" | "absent" | { raw: unknown };
  script: Reply[];
  finalizeRequests: number;
  events: string[];
  intentStatuses: string[];
  onPark?: (n: number) => void;
  onFinalize?: (index: number) => void;
  forceOwnershipRunning: boolean;
  lastReply?: Reply;
}

function model(over: Partial<Model> = {}): Model {
  return {
    status: "running", parkSeq: 0, parkAck: "seq", script: [{ kind: "admitted" }], finalizeRequests: 0,
    events: [], intentStatuses: [], forceOwnershipRunning: false, ...over,
  };
}

function install(m: Model, runId: string): void {
  api.onState(runId, (body: StateRequest) => {
    if (body.status === "awaiting_followup") {
      m.status = "awaiting_followup";
      m.parkSeq++;
      m.events.push("park");
      m.onPark?.(m.parkSeq);
    } else if (body.status === "running") {
      m.status = "running";
    } else if (body.status === "followup_finalize") {
      const index = m.finalizeRequests++;
      const reply = m.script[Math.min(index, m.script.length - 1)]!;
      m.lastReply = reply;
      const applies = reply.kind === "lost" || reply.kind === "stale" ? true : (reply.apply ?? reply.kind === "admitted");
      if (applies && m.status === "awaiting_followup") m.status = "running";
      m.onFinalize?.(index);
    } else if (body.status === "completed" || body.status === "failed") {
      m.status = body.status;
    }
  });
  api.stateAckWire = (body, run) => {
    if (body.status === "awaiting_followup") {
      const wire = m.parkAck === "seq" ? { followup_park_seq: m.parkSeq }
        : m.parkAck === "absent" ? {} : { followup_park_seq: m.parkAck.raw };
      return { status: 200, body: { ...wire, run: { ...run, status: "awaiting_followup" } } };
    }
    if (body.status !== "followup_finalize") return undefined;
    const reply = m.lastReply!;
    switch (reply.kind) {
      case "admitted":
        m.events.push("finalize:admitted");
        return { status: 200, body: { applied: true, disposition: "followup_finalize_admitted", run: { ...run, status: "running" } } };
      case "lost":
        m.events.push("finalize:lost");
        return { status: 503, body: { error: "unavailable" } };
      case "refuse":
        m.events.push(`finalize:${reason(reply)}`);
        return { status: 409, body: { applied: false, reason: reply.reason, run: { ...run, status: "awaiting_followup" } } };
      case "stale":
        m.events.push("finalize:stale");
        return { status: 409, body: { disposition: "stale_claim", run: { ...run, status: "running" } } };
      case "raw":
        m.events.push("finalize:raw");
        return { status: reply.status, body: reply.body };
    }
  };
  api.ownershipWire.set(runId, res => {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ status: m.forceOwnershipRunning ? "running" : m.status, claim_generation: GEN, claim_released: false }));
  });
  api.publicationIntentWire = req => {
    m.events.push("intent");
    m.intentStatuses.push(m.status);
    // The api refuses an intent for a run that is not running with the exact stale_claim disposition.
    if (m.status !== "running") return { status: 409, body: { disposition: "stale_claim" } };
    return { status: 200, body: {
      enrollment_seq: (req.body.enrollment_seq as number | undefined) ?? 1,
      candidate_seq: (req.body.candidate_seq as number | undefined) ?? 1,
      state: "active",
    } };
  };
}
const reason = (r: Extract<Reply, { kind: "refuse" }>): string => r.reason;

function taskClaim(): ClaimResponse {
  const runId = randomUUID();
  return makeClaim({
    run_id: runId, kind: "task", interactive: true, issue_iid: null, issue_title: "Handoff: interactive session",
    issue_description: "Work with me interactively.", branch: `uzi/task/${runId}`, base_branch: "develop",
    open_mr: false, claim_generation: GEN,
    repo: { id: REPO_ID, url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
  });
}

function commitWork(ctx: RunContext, note: string): void {
  fs.writeFileSync(path.join(ctx.worktreePath, "UZI_RUN.md"), `# ${note}\n`);
  execFileSync("git", ["add", "UZI_RUN.md"], { cwd: ctx.worktreePath });
  execFileSync("git", ["-c", "user.name=uzi-agent", "-c", "user.email=uzi-agent@uzi.local", "-c", "commit.gpgsign=false",
    "commit", "-m", `uzi test: ${note}`], { cwd: ctx.worktreePath });
}

/** Parks until the park ENDS (idle, stop or cancel), reporting a turn after each follow-up as the
 *  SDK executor does, then commits and returns. A cancelled park raises the cancel signal as it does. */
function parkingExecutor(log: { outcomes: FollowUpOutcome[] }, idleMs = IDLE_MS): Executor {
  return {
    async run(ctx: RunContext): Promise<{ branch: string }> {
      await ctx.checkpoint?.({ reap: true });
      for (let turn = 1; ; turn++) {
        const outcome = await ctx.awaitFollowUp!(idleMs);
        log.outcomes.push(outcome);
        if (outcome.kind === "ended") {
          if (outcome.reason === "cancelled") throw new Error("run cancelled");
          break;
        }
        await ctx.reportIteration?.(turn);
      }
      commitWork(ctx, "interactive finalize");
      return { branch: ctx.branch };
    },
  };
}

function build(executor: Executor, outbox: Outbox | undefined, gitlab = fakeGitlab().gitlab, extra: Record<string, unknown> = {}, homeDir?: string): RunRunner {
  const r = new RunRunner(client, git, () => ({ executor, homeDir }), nullLogger(), 20, undefined, {
    pollMs: 5, planApprovalTimeoutMs: 0, prDescriptionHeadLagMs: 0, recoveryRetryMs: RECOVERY_RETRY_MS,
    checkpointTestHooks: { retryBackoffMs: [0, 0] }, gitlab, steeringNow: clock.now,
    ...(outbox ? { outbox } : {}), ...extra,
  });
  const seams = r as unknown as { forgeRetrySleep: (ms: number) => Promise<void>; publicationRetry: unknown };
  seams.forgeRetrySleep = async () => {};
  seams.publicationRetry = { retryDelaysMs: [0, 0, 0], sleep: async () => {} };
  installBackoffSeams(r);
  return r;
}

interface WaitFlight { cancel: { signal: AbortSignal }; active?: { shuttingDown?: boolean }; steering: { isCancelled(): boolean } }

/** Routes the admission loop's own recovery backoff (not the send closure's) to the FakeClock while
 *  fake mode is engaged; every other caller, and every call once the clock is released, stays real. */
function installBackoffSeams(r: RunRunner): void {
  const c = clock;
  const where = new AsyncLocalStorage<"admit" | "send">();
  const seams = r as unknown as {
    admitFollowUpFinalize(flight: WaitFlight, first: unknown, idleMs: number, parkSeq: number, send: (b: StateRequest) => Promise<StateAck>): Promise<unknown>;
    waitRecoveryRetry(flight: WaitFlight, cancelStopsWait?: boolean, waitMs?: number, signal?: AbortSignal): Promise<void>;
    shuttingDownGlobal: boolean;
  };
  const realAdmit = seams.admitFollowUpFinalize.bind(r);
  const realWait = seams.waitRecoveryRetry.bind(r);
  seams.admitFollowUpFinalize = (flight, first, idleMs, parkSeq, send) =>
    where.run("admit", () => realAdmit(flight, first, idleMs, parkSeq, b => where.run("send", () => send(b))));
  seams.waitRecoveryRetry = async (flight, cancelStopsWait = true, waitMs, signal) => {
    if (!c.engaged || where.getStore() !== "admit") return realWait(flight, cancelStopsWait, waitMs, signal);
    signal?.throwIfAborted();
    const cancelSignal = flight.cancel.signal.aborted ? undefined : flight.cancel.signal;
    const signals = [signal, cancelSignal].filter((s): s is AbortSignal => s !== undefined);
    await c.wait(waitMs ?? RECOVERY_RETRY_MS, signals.length ? AbortSignal.any(signals) : undefined,
      () => flight.active?.shuttingDown === true || seams.shuttingDownGlobal === true
        || (cancelStopsWait && flight.steering.isCancelled()));
    signal?.throwIfAborted();
  };
}

/** The flight the runner hands the admission loop, so a test can reach its steering and flags. */
interface Captured { flight?: { steering: { abortLifecycle(reason?: unknown, terminal?: boolean): void; markCancelled(): void; isCancelled(): boolean; approveRefusedBySwitchPending(): boolean }; preserveRecoveryClone?: boolean; preserveSession?: boolean; stoppedSourceKept?: boolean; followUpPendingRearms?: number } }
function captureFlight(r: RunRunner, onEnter?: (flight: NonNullable<Captured["flight"]>) => void): Captured {
  const cap: Captured = {};
  const seam = r as unknown as { admitFollowUpFinalize: (flight: NonNullable<Captured["flight"]>, ...rest: unknown[]) => Promise<unknown> };
  const real = seam.admitFollowUpFinalize.bind(r);
  seam.admitFollowUpFinalize = (flight, ...rest) => { cap.flight = flight; onEnter?.(flight); return real(flight, ...rest); };
  return cap;
}

/** Counts the followup_finalize reports the runner hands the client, whether or not a request follows. */
function spyFinalizeSends(): { count: number } {
  const seen = { count: 0 };
  const real = client.reportState.bind(client);
  client.reportState = (async (...a: Parameters<typeof real>) => {
    if (a[1].status === "followup_finalize") seen.count++;
    return real(...a);
  }) as typeof client.reportState;
  return seen;
}

/** Records every snapshot phase change into the model's event list. */
function trackPhases(m: Model): ActiveRunRegistry {
  const registry = new ActiveRunRegistry();
  const real = registry.setPhase.bind(registry);
  registry.setPhase = (runId, phase) => { m.events.push(`phase:${phase}`); real(runId, phase); };
  return registry;
}

const states = (runId: string) => api.states.filter(s => s.runId === runId).map(s => s.body);
const statuses = (runId: string) => states(runId).map(b => b.status);
const finalizeBodies = (runId: string) => states(runId).filter(b => b.status === "followup_finalize");
const sleep = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms));

async function until(predicate: () => boolean, ms: number): Promise<boolean> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (predicate()) return true;
    await sleep(10);
  }
  return predicate();
}

/** Run to its end or fail at the deadline: a hung run is cancelled so the harness can tear down. */
async function finish(r: RunRunner, claim: ClaimResponse, ms = 10_000): Promise<void> {
  const execution = startExecution(r, claim).then(() => "done" as const);
  const winner = await Promise.race([execution, sleep(ms).then(() => "deadline" as const)]);
  if (winner === "deadline") {
    api.setInputs(claim.run_id, [{ id: 9_000, kind: "cancel", body: null }]);
    await Promise.race([execution.catch(() => undefined), sleep(5_000)]);
    assert.fail(`the run did not finish within ${ms}ms; statuses: ${JSON.stringify(statuses(claim.run_id))}`);
  }
}

/** For the cases whose correct outcome is to NOT proceed: observe for a window, then cancel. */
async function observeThenCancel(r: RunRunner, claim: ClaimResponse, log: { outcomes: FollowUpOutcome[] }, windowMs: number, ms = 10_000): Promise<void> {
  clock.engage();
  let settled = false;
  const execution = startExecution(r, claim).then(() => { settled = true; return "done" as const; });
  void execution.catch(() => { settled = true; });
  // The window is FAKE time measured from the first backoff wait: every pending admission backoff is
  // advanced to its deadline until the window has elapsed, so the resend count never depends on the host.
  // A real-time cap that expires is a failure, never a completed window; on any failure the run is
  // still cancelled and the clock released, so the file never hangs.
  let cancelSent = false;
  try {
    assert.ok(await until(() => statuses(claim.run_id).includes("awaiting_followup"), 8_000), "the run parked");
    let firstBackoffAt: number | undefined;
    for (;;) {
      assert.ok(await until(() => clock.pending > 0 || log.outcomes.length > 0 || settled, 8_000),
        "the admission reached a backoff, an outcome or the end of the run");
      const deadline = clock.nextDeadline();
      if (deadline === undefined) break;
      firstBackoffAt ??= clock.now();
      if (clock.now() - firstBackoffAt >= windowMs) break;
      clock.advance(Math.max(0, deadline - clock.now()));
    }
    // An outcome that reached the executor proceeds to publication (or completes); let that settle.
    if (log.outcomes.length > 0)
      assert.ok(await until(() => api.publicationIntentRequests.length >= 1 || settled, 8_000), "the outcome reached publication");
    observed.push({ statuses: statuses(claim.run_id), intents: api.publicationIntentRequests.length, finished: settled });
    api.setInputs(claim.run_id, [{ id: 9_001, kind: "cancel", body: null }]);
    cancelSent = true;
    assert.ok(await until(() => currentSteering?.isCancelled() === true || settled, 8_000), "the cancel was observed");
  } finally {
    if (!cancelSent) api.setInputs(claim.run_id, [{ id: 9_001, kind: "cancel", body: null }]);
    clock.release();
  }
  const winner = await Promise.race([execution, sleep(ms).then(() => "deadline" as const)]);
  assert.equal(winner, "done", "the cancel ends the run");
}
const observed: Array<{ statuses: string[]; intents: number; finished: boolean }> = [];

const idleSetup = async (over: Partial<Model> = {}) => {
  client.protocolFeatures = FEATURES;
  const claim = taskClaim();
  const m = model(over);
  install(m, claim.run_id);
  return { claim, m, outbox: await mkOutbox(), log: { outcomes: [] as FollowUpOutcome[] } };
};

describe("interactive follow-up finalize admission (issue #4479)", () => {
  it("idle: sends followup_finalize with the park's sequence, then the intent enrolls and the run completes", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.deepEqual(log.outcomes, [{ kind: "ended", reason: "idle" }]);
    const sent = finalizeBodies(claim.run_id);
    assert.equal(sent.length, 1);
    assert.deepEqual([sent[0]!.claim_generation, sent[0]!.followup_park_seq], [GEN, 1]);
    assert.ok(m.events.indexOf("finalize:admitted") < m.events.indexOf("intent"), `admission precedes the intent: ${m.events.join(",")}`);
    assert.deepEqual(m.intentStatuses, ["running"]);
    assert.deepEqual(statuses(claim.run_id).filter(s => ["awaiting_followup", "followup_finalize", "completed", "failed"].includes(s as string)),
      ["awaiting_followup", "followup_finalize", "completed"]);
  });

  it("stopped: a graceful stop is admitted the same way and completes", async () => {
    const { claim, m, outbox, log } = await idleSetup({ onPark: () => api.setInputs(claim.run_id, [{ id: 7, kind: "stop", body: null }]) });
    neverIdle();
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.deepEqual(log.outcomes, [{ kind: "ended", reason: "stopped" }]);
    assert.equal(finalizeBodies(claim.run_id).length, 1);
    assert.ok(m.events.indexOf("finalize:admitted") < m.events.indexOf("intent"));
    assert.equal(statuses(claim.run_id).at(-1), "completed");
  });

  it("a lost transition ACK resends, and neither the applied row nor ownership running authorizes the finalize", async () => {
    // The first round applies the admission and loses every reply; the run is already `running` on
    // the server, and the ownership probe says so, but nothing publishes until a reply admits it.
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "lost" }, { kind: "lost" }, { kind: "lost" }, { kind: "admitted" }], forceOwnershipRunning: true,
    });
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(m.finalizeRequests, ATTEMPTS + 1);
    assert.ok(m.events.indexOf("finalize:admitted") >= 0 && m.events.indexOf("finalize:admitted") < m.events.indexOf("intent"),
      `the intent waits for an admitted reply: ${m.events.join(",")}`);
    assert.equal(statuses(claim.run_id).at(-1), "completed");
  });

  for (const [name, body] of [
    ["the disposition", { applied: true, run: { status: "running" } }],
    ["applied", { disposition: "followup_finalize_admitted", run: { status: "running" } }],
    ["the running status", { applied: true, disposition: "followup_finalize_admitted", run: { status: "awaiting_followup" } }],
  ] as const) {
    it(`a 200 missing ${name} is unconfirmed: it reconciles, then the replay confirms`, async () => {
      const { claim, m, outbox, log } = await idleSetup({ script: [{ kind: "raw", status: 200, body, apply: true }, { kind: "admitted" }] });
      await finish(build(parkingExecutor(log), outbox), claim);

      assert.equal(m.finalizeRequests, 2, "one unconfirmed answer, one confirming replay");
      assert.ok(m.events.indexOf("finalize:admitted") < m.events.indexOf("intent"), m.events.join(","));
      assert.equal(statuses(claim.run_id).at(-1), "completed");
    });
  }

  it("a lost enrollment ACK replays the intent and the admission is not sent again", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    let drops = 0;
    const wire = api.publicationIntentWire!;
    api.publicationIntentWire = req => (drops++ < 4 ? "drop" : wire(req));
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(m.finalizeRequests, 1, "admitted once; the enrollment retries never re-run it");
    assert.ok(drops > 4);
    assert.equal(statuses(claim.run_id).at(-1), "completed");
  });

  it("an unconfirmed admission publishes nothing, and a cancel in the window ends the run cancelled", async () => {
    observed.length = 0;
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "raw", status: 200, body: {}, apply: false }], forceOwnershipRunning: true,
    });
    await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 300);

    assert.equal(observed[0]!.finished, false, "still waiting inside the window");
    assert.equal(observed[0]!.intents, 0, "no publication intent while the admission is unconfirmed");
    assert.ok(m.finalizeRequests > 1, "the admission is resent");
    assert.equal(api.publicationIntentRequests.length, 0);
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.match(String(states(claim.run_id).find(b => b.status === "failed")?.failure_reason), /run cancelled/);
  });

  it("a replay that answers cancel_pending after the admission ends cancelled", async () => {
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "lost" }, { kind: "lost" }, { kind: "lost" }, { kind: "refuse", reason: "cancel_pending", apply: false }],
      onFinalize: index => { if (index === ATTEMPTS) api.setInputs(claim.run_id, [{ id: 8, kind: "cancel", body: null }]); },
    });
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.deepEqual(log.outcomes.at(-1), { kind: "ended", reason: "cancelled" });
    assert.equal(api.publicationIntentRequests.length, 0, "nothing publishes");
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.match(String(states(claim.run_id).find(b => b.status === "failed")?.failure_reason), /run cancelled/);
    assert.ok(m.finalizeRequests >= ATTEMPTS + 1);
  });

  it("a cancel buffered before the park sends no transition and ends cancelled through the existing path", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    api.setInputs(claim.run_id, [{ id: 5, kind: "cancel", body: null }]);
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(m.finalizeRequests, 0);
    assert.ok(!statuses(claim.run_id).includes("awaiting_followup"));
    assert.match(String(states(claim.run_id).find(b => b.status === "failed")?.failure_reason), /run cancelled/);
  });

  it("a stop buffered before the park sends nothing: the run never parked", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    api.setInputs(claim.run_id, [{ id: 5, kind: "stop", body: null }]);
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.deepEqual(log.outcomes, [{ kind: "ended", reason: "stopped" }]);
    assert.equal(m.finalizeRequests, 0);
    assert.ok(!statuses(claim.run_id).includes("awaiting_followup"));
    assert.equal(statuses(claim.run_id).at(-1), "completed");
  });

  it("a follow-up buffered before the park sends nothing", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    api.setInputs(claim.run_id, [{ id: 5, kind: "follow_up", body: "keep going" }]);
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.deepEqual(log.outcomes[0], { kind: "followup", id: 5, body: "keep going" });
    assert.equal(m.parkSeq, 1, "only the later idle park parks");
    assert.equal(m.finalizeRequests, 1, "the one admission belongs to that later park");
    assert.equal(finalizeBodies(claim.run_id)[0]!.followup_park_seq, 1);
  });

  it("followup_pending after idle re-arms with a fresh window and the follow-up resumes; the next park finalizes with its own sequence", async () => {
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "refuse", reason: "followup_pending", apply: false }, { kind: "admitted" }],
      onFinalize: index => { if (index === 0) api.setInputs(claim.run_id, [{ id: 11, kind: "follow_up", body: "one more thing" }]); },
    });
    // Only arms 1 and 3 cross the idle window. The re-arm after the refusal (arm 2) must not idle out:
    // the poll loop delivers the follow-up to it.
    armPolicy = armIndex => armIndex === 1 || armIndex === 3;
    const r = build(parkingExecutor(log), outbox);
    const cap = captureFlight(r);
    await finish(r, claim, 20_000);

    assert.deepEqual(log.outcomes, [{ kind: "followup", id: 11, body: "one more thing" }, { kind: "ended", reason: "idle" }]);
    assert.deepEqual(finalizeBodies(claim.run_id).map(b => b.followup_park_seq), [1, 2], "each park binds its own sequence");
    assert.equal(statuses(claim.run_id).at(-1), "completed");
    assert.equal(m.parkSeq, 2);
    assert.equal(cap.flight?.followUpPendingRearms, 1, "the followup_pending re-arm is counted");
  });

  it("followup_pending after a stop is a protocol fault: the run fails and the stop is never re-armed", async () => {
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "refuse", reason: "followup_pending", apply: false }],
      onPark: () => api.setInputs(claim.run_id, [{ id: 7, kind: "stop", body: null }]),
    });
    neverIdle();
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(m.finalizeRequests, 1, "bounded: one refusal, no resend");
    assert.match(String(states(claim.run_id).find(b => b.status === "failed")?.failure_reason), new RegExp(REASON_FOLLOWUP_NOT_PARKED));
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });

  it("a cancel_pending refusal on a sticky stop stays bounded and ends cancelled", async () => {
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "refuse", reason: "cancel_pending", apply: false }],
      onPark: () => api.setInputs(claim.run_id, [{ id: 7, kind: "stop", body: null }]),
      onFinalize: index => { if (index === 0) api.setInputs(claim.run_id, [{ id: 8, kind: "cancel", body: null }]); },
    });
    neverIdle();
    // The clock never advances: the queued cancel ends the park (or wakes the fake backoff if one is pending).
    clock.engage();
    const r = build(parkingExecutor(log), outbox);
    const cap = captureFlight(r);
    await finish(r, claim);
    const cancelSeen = cap.flight?.steering.isCancelled() === true;

    assert.equal(cancelSeen, true);
    assert.deepEqual(log.outcomes.at(-1), { kind: "ended", reason: "cancelled" });
    assert.ok(m.finalizeRequests <= 20, `bounded resends: ${m.finalizeRequests}`);
    assert.match(String(states(claim.run_id).find(b => b.status === "failed")?.failure_reason), /run cancelled/);
  });

  it("followup_finalize_refused ends in the park refusal", async () => {
    const { claim, m, outbox, log } = await idleSetup({ script: [{ kind: "refuse", reason: "followup_finalize_refused", apply: false }] });
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(m.finalizeRequests, 1);
    assert.match(String(states(claim.run_id).find(b => b.status === "failed")?.failure_reason), new RegExp(`^${REASON_FOLLOWUP_NOT_PARKED}`));
  });

  it("a stale_claim answer stops the flight without a terminal report", async () => {
    const { claim, m, outbox, log } = await idleSetup({ script: [{ kind: "stale" }] });
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(m.finalizeRequests, 1);
    assert.deepEqual(log.outcomes, [], "the reportState closure threw StaleClaimError: the outcome never reached the executor");
    assert.ok(!statuses(claim.run_id).includes("completed") && !statuses(claim.run_id).includes("failed"));
    assert.equal(api.publicationIntentRequests.length, 0);
  });

  it("a newer same-worker wall park stops the old finalize flight and retains its source", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    m.script = [{ kind: "raw", status: 409,
      body: { disposition: "stale_claim", run: { id: claim.run_id, status: "paused", hold_reason: "budget_exhausted", claim_generation: GEN + 1 } } }];
    const r = build(parkingExecutor(log), outbox);
    const cap = captureFlight(r);
    await finish(r, claim);
    assert.equal(m.finalizeRequests, 1);
    assert.deepEqual(log.outcomes, []);
    assert.equal(cap.flight?.preserveRecoveryClone, true);
    assert.equal(cap.flight?.preserveSession, true);
    assert.ok(!statuses(claim.run_id).includes("completed") && !statuses(claim.run_id).includes("failed"));
    assert.equal(api.publicationIntentRequests.length, 0);
  });

  it("shutdown racing a wall-park ACK keeps the unpublished clone, HOME and wall-park retirement", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    m.script = [{ kind: "raw", status: 409, body: { disposition: "stale_claim",
      run: { id: claim.run_id, status: "paused", hold_reason: "budget_exhausted", claim_generation: GEN + 1 } } }];
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "followup-wall-shutdown-"));
    roots.push(root);
    const homeDir = path.join(root, "home");
    let worktree = "";
    const executor: Executor = { async run(ctx) {
      worktree = ctx.worktreePath;
      commitWork(ctx, "unpublished work before wall park");
      fs.mkdirSync(homeDir, { recursive: true });
      fs.writeFileSync(path.join(homeDir, "session"), "synthetic transcript");
      log.outcomes.push(await ctx.awaitFollowUp!(40));
      return { branch: ctx.branch };
    } };
    const r = build(executor, outbox, undefined, {}, homeDir);
    const cap = captureFlight(r);
    const internals = r as unknown as {
      admitFollowUpFinalize(flight: NonNullable<Captured["flight"]>, ...rest: unknown[]): Promise<unknown>;
      retireFinalizeRecord(flight: unknown, site: string): Promise<void>;
      publishCheckpointOutcome(...args: unknown[]): Promise<{ published: false; reason: "rejected"; httpStatus: number }>;
    };
    const admit = internals.admitFollowUpFinalize.bind(r);
    let wallSeen = false;
    internals.admitFollowUpFinalize = async (flight, ...rest) => {
      try { return await admit(flight, ...rest); }
      catch (error) {
        if (error instanceof Error && error.name === "ServerWallParkedError") {
          wallSeen = true;
          r.shutdown(); // ACK parsed; shutdown wins the race before dispatchFailure selects its arm.
        }
        throw error;
      }
    };
    // No checkpoint can supply an alternate durability proof for deleting the clone.
    internals.publishCheckpointOutcome = async () => ({ published: false, reason: "rejected", httpStatus: 403 });
    const retire = internals.retireFinalizeRecord.bind(r);
    const sites: string[] = [];
    internals.retireFinalizeRecord = async (flight, site) => { sites.push(site); await retire(flight, site); };
    await finish(r, claim);
    assert.equal(wallSeen, true);
    assert.equal(m.finalizeRequests, 1);
    assert.equal(cap.flight?.preserveRecoveryClone, true);
    assert.equal(cap.flight?.preserveSession, true);
    assert.equal(fs.readFileSync(path.join(worktree, "UZI_RUN.md"), "utf8"), "# unpublished work before wall park\n");
    assert.equal(fs.readFileSync(path.join(homeDir, "session"), "utf8"), "synthetic transcript");
    assert.deepEqual(sites, ["server_wall_park"]);
    assert.deepEqual(log.outcomes, []);
    assert.ok(!statuses(claim.run_id).includes("completed") && !statuses(claim.run_id).includes("failed"));
    assert.equal(api.publicationIntentRequests.length, 0);
  });

  it("without the negotiated token nothing new is sent", async () => {
    observed.length = 0;
    const { claim, m, outbox, log } = await idleSetup();
    client.protocolFeatures = BASE_FEATURES;
    const sends = spyFinalizeSends();
    await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 250);

    assert.equal(sends.count, 0, "the runner never even hands the client a followup_finalize report");
    assert.equal(m.finalizeRequests, 0);
    assert.equal(finalizeBodies(claim.run_id).length, 0);
  });

  it("without the publication resume the run finalizes ordinarily and sends no admission", async () => {
    const { claim, m, log } = await idleSetup();
    client.protocolFeatures = [FOLLOWUP_FINALIZE_ADMISSION_FEATURE, "recovery_park_cause"];
    await finish(build(parkingExecutor(log), undefined), claim);

    assert.equal(m.finalizeRequests, 0);
    assert.equal(statuses(claim.run_id).at(-1), "completed");
  });

  for (const [name, parkAck] of [
    ["absent", "absent"], ["zero", { raw: 0 }], ["negative", { raw: -3 }], ["a string", { raw: "7" }], ["a fraction", { raw: 1.5 }],
    ["unsafe", { raw: 2 ** 60 }],
  ] as const) {
    it(`a ${name} park sequence behaves as today: no admission is sent`, async () => {
      observed.length = 0;
      const { claim, m, outbox, log } = await idleSetup({ parkAck });
      await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 250);

      assert.equal(m.finalizeRequests, 0);
      assert.ok(statuses(claim.run_id).includes("awaiting_followup"), "it did park");
    });
  }

  it("a feature withdrawn after an ambiguous send stays unconfirmed and never publishes", async () => {
    observed.length = 0;
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "lost" }], onFinalize: () => { client.protocolFeatures = BASE_FEATURES; },
    });
    await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 300);

    assert.equal(m.finalizeRequests, ATTEMPTS, "only the first round reached the wire");
    assert.equal(api.publicationIntentRequests.length, 0);
    assert.equal(observed[0]!.finished, false);
  });

  for (const [name, body] of [["the generic invalid-state error", { error: GENERIC_400 }], ["the strict-decode error", { error: "invalid request body" }]] as const) {
    it(`a rolled-back api (${name}) on the first send keeps today's behaviour: no transition, no resend`, async () => {
      observed.length = 0;
      const { claim, m, outbox, log } = await idleSetup({ script: [{ kind: "raw", status: 400, body }] });
      await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 250);

      assert.equal(m.finalizeRequests, 1, "a 400 is neither retried nor resent");
      assert.ok(m.intentStatuses.every(status => status === "awaiting_followup"), "no transition: every intent met the parked row");
      assert.deepEqual(log.outcomes[0], { kind: "ended", reason: "idle" }, "the executor got the outcome as before");
    });
  }

  it("a generic 400 after an ambiguous send stays unconfirmed", async () => {
    observed.length = 0;
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "lost" }, { kind: "lost" }, { kind: "lost" }, { kind: "raw", status: 400, body: { error: GENERIC_400 } }],
    });
    await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 300);

    assert.ok(m.finalizeRequests > ATTEMPTS + 1, "it keeps resending");
    assert.equal(api.publicationIntentRequests.length, 0);
    assert.equal(log.outcomes.at(-1)?.kind === "ended" && (log.outcomes.at(-1) as { reason: string }).reason, "cancelled");
  });

  it("an idle admission followed by a forge outage takes the existing publication park", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    const seam = git as unknown as { execScoped(cmd: string, args: string[], ...rest: unknown[]): Promise<unknown> };
    const real = seam.execScoped.bind(git);
    seam.execScoped = async (cmd, args, ...rest) => {
      if (cmd === "git" && args.includes("push"))
        throw Object.assign(new Error(`Command failed: git ${args.join(" ")}`), { code: 128, stderr: "fatal: unable to access: connection reset by peer" });
      return real(cmd, args, ...rest);
    };
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(m.finalizeRequests, 1);
    const park = states(claim.run_id).filter(b => b.status === "recovery_wait");
    assert.equal(park.length, 1);
    assert.deepEqual([park[0]!.recovery_cause, park[0]!.recovery_phase, park[0]!.publication_surface, park[0]!.publication_step],
      ["forge_unreachable", "publication", "git", "push"]);
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.ok(statuses(claim.run_id).indexOf("followup_finalize") < statuses(claim.run_id).indexOf("recovery_wait"));
  });

  it("a stop admission followed by a forge outage reports the existing forge park once", async () => {
    const { claim, m, outbox, log } = await idleSetup({ onPark: () => api.setInputs(claim.run_id, [{ id: 7, kind: "stop", body: null }]) });
    const seam = git as unknown as { execScoped(cmd: string, args: string[], ...rest: unknown[]): Promise<unknown> };
    const real = seam.execScoped.bind(git);
    seam.execScoped = async (cmd, args, ...rest) => {
      if (cmd === "git" && args.includes("push"))
        throw Object.assign(new Error(`Command failed: git ${args.join(" ")}`), { code: 128, stderr: "fatal: unable to access: connection reset by peer" });
      return real(cmd, args, ...rest);
    };
    // The server answers a park for a stop-stamped run as the stamped stop's cancel.
    const wire = api.stateAckWire!;
    api.stateAckWire = (body, run) => body.status === "recovery_wait"
      ? { status: 200, body: { run: { ...run, status: "cancelled" } } } : wire(body, run);
    neverIdle();
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(m.finalizeRequests, 1);
    assert.equal(states(claim.run_id).filter(b => b.status === "recovery_wait").length, 1);
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });

  it("after a definite refusal a withdrawn feature returns the outcome and stops sending (no spin)", async () => {
    observed.length = 0;
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "refuse", reason: "followup_pending", apply: false }],
      onFinalize: () => { client.protocolFeatures = BASE_FEATURES; },
    });
    const sends = spyFinalizeSends();
    await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 400);

    assert.equal(sends.count, 1, "no further report is handed to the client once the feature is gone");
    assert.equal(m.finalizeRequests, 1);
    assert.deepEqual(log.outcomes[0], { kind: "ended", reason: "idle" }, "the outcome reached the executor as it would today");
  });

  it("a credential-switch signal thrown before the send is not an ambiguous send: it propagates and the re-park finalizes", async () => {
    const { claim, m, outbox } = await idleSetup();
    const caught: unknown[] = [];
    const executor: Executor = {
      async run(ctx: RunContext) {
        await ctx.checkpoint?.({ reap: true });
        for (;;) {
          try { await ctx.awaitFollowUp!(40); break; } catch (error) { caught.push(error); }
        }
        commitWork(ctx, "after switch");
        return { branch: ctx.branch };
      },
    };
    let armed = true;
    const r = build(executor, outbox);
    captureFlight(r, flight => {
      flight.steering.approveRefusedBySwitchPending = () => { const now = armed; armed = false; return now; };
    });
    await finish(r, claim);

    assert.equal(caught.length, 1);
    assert.ok(caught[0] instanceof CredentialSwitchSignal, "surfaced to the executor, not swallowed as unconfirmed");
    assert.equal(m.finalizeRequests, 1, "only the re-park's send reached the wire");
    assert.equal(finalizeBodies(claim.run_id)[0]!.followup_park_seq, 2);
    assert.equal(statuses(claim.run_id).at(-1), "completed");
  });

  it("the snapshot phase and the hold stay at awaiting_followup until an admitted ACK, never after an unconfirmed one", async () => {
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "raw", status: 200, body: {}, apply: false }, { kind: "raw", status: 200, body: {}, apply: false }, { kind: "admitted" }],
    });
    await finish(build(parkingExecutor(log), outbox, undefined, { activeRuns: trackPhases(m) }), claim);

    const parked = m.events.lastIndexOf("phase:awaiting_followup");
    const admitted = m.events.indexOf("finalize:admitted");
    assert.ok(parked >= 0 && admitted > parked, m.events.join(","));
    const between = m.events.slice(parked + 1, admitted);
    assert.ok(between.includes("finalize:raw"), "unconfirmed replies came first");
    assert.ok(!between.includes("phase:running"), `running only after the admitted ACK: ${m.events.join(",")}`);
    assert.ok(m.events.indexOf("phase:running", admitted) > admitted, "and then it does move to running");
  });

  it("an outbox without the publication_resume_v1 feature sends no followup_finalize and behaves as today", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    client.protocolFeatures = [FOLLOWUP_FINALIZE_ADMISSION_FEATURE, "recovery_park_cause"];
    const sends = spyFinalizeSends();
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(sends.count, 0);
    assert.equal(m.finalizeRequests, 0);
    assert.equal(statuses(claim.run_id).at(-1), "completed");
  });

  it("a cancel seen when the admitted ACK returns yields the cancelled outcome and publishes nothing", async () => {
    let releaseAck!: () => void;
    const hold = new Promise<void>(resolve => { releaseAck = resolve; });
    let cancelSeen = false;
    const { claim, outbox, log } = await idleSetup({
      onFinalize: index => {
        if (index !== 0) return;
        api.setInputs(claim.run_id, [{ id: 8, kind: "cancel", body: null }]);
        // Hold the ACK until the steering channel has actually seen the cancel (failure cap 8 s).
        void until(() => currentSteering?.isCancelled() === true, 8_000).then(ok => { cancelSeen = ok; releaseAck(); });
      },
    });
    api.afterPersistState(claim.run_id, b => b.status === "followup_finalize", hold);
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(cancelSeen, true, "the cancel was seen before the ACK was released");
    assert.deepEqual(log.outcomes.at(-1), { kind: "ended", reason: "cancelled" });
    assert.equal(api.publicationIntentRequests.length, 0);
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });

  it("cancel_pending on a sticky stop resends at the recovery backoff, not in a tight loop", async () => {
    observed.length = 0;
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "refuse", reason: "cancel_pending", apply: false }],
      onPark: () => api.setInputs(claim.run_id, [{ id: 7, kind: "stop", body: null }]),
    });
    neverIdle();
    await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 400);

    assert.ok(m.finalizeRequests >= 3, `it keeps resending: ${m.finalizeRequests}`);
    assert.ok(m.finalizeRequests <= 90, `bounded by the 5 ms backoff within a 400 ms window: ${m.finalizeRequests}`);
  });

  describe("interrupts after an unknown send", () => {
    const lostFirst = { script: [{ kind: "lost" }] satisfies Reply[] };
    const flags = (f: Captured["flight"]) => [f?.preserveRecoveryClone, f?.preserveSession, f?.stoppedSourceKept];

    it("a credential-switch lifecycle abort does not re-run the park and counts as unconfirmed", async () => {
      observed.length = 0;
      let cap!: Captured;
      const { claim, m, outbox, log } = await idleSetup({
        ...lostFirst, onFinalize: index => { if (index === 0) cap.flight!.steering.abortLifecycle(new CredentialSwitchSignal(), false); },
      });
      const r = build(parkingExecutor(log), outbox);
      cap = captureFlight(r);
      await observeThenCancel(r, claim, log, 400);

      assert.equal(m.parkSeq, 1, "the park was not re-run");
      assert.ok(m.finalizeRequests > ATTEMPTS, "it keeps resending");
      assert.ok(!observed[0]!.statuses.includes("failed"), "the flight neither failed nor threw the switch");
    });

    it("a claim fence stops with StaleClaimError and keeps the custody flags", async () => {
      let cap!: Captured;
      const { claim, outbox, log } = await idleSetup({
        ...lostFirst, onFinalize: index => { if (index === 0) (cap.flight!.steering as unknown as { fence: string }).fence = "stale"; },
      });
      const r = build(parkingExecutor(log), outbox);
      cap = captureFlight(r);
      await finish(r, claim);

      assert.deepEqual(flags(cap.flight), [true, true, true]);
      assert.deepEqual(log.outcomes, []);
      assert.ok(!statuses(claim.run_id).includes("completed"));
      assert.ok(new StaleClaimError() instanceof Error);
    });

    it("a shutdown throws and keeps the custody flags", async () => {
      let cap!: Captured;
      const { claim, outbox, log } = await idleSetup({
        ...lostFirst, onFinalize: index => { if (index === 0) (r as unknown as { shuttingDownGlobal: boolean }).shuttingDownGlobal = true; },
      });
      const r = build(parkingExecutor(log), outbox);
      cap = captureFlight(r);
      await finish(r, claim);

      assert.deepEqual(flags(cap.flight), [true, true, true]);
      assert.deepEqual(log.outcomes, []);
      assert.ok(!statuses(claim.run_id).includes("completed"));
    });

    it("a lifecycle abort throws and keeps the custody flags", async () => {
      let cap!: Captured;
      const { claim, outbox, log } = await idleSetup({
        ...lostFirst, onFinalize: index => { if (index === 0) cap.flight!.steering.abortLifecycle(new PauseNowSignal(), false); },
      });
      const r = build(parkingExecutor(log), outbox);
      cap = captureFlight(r);
      await finish(r, claim);

      assert.deepEqual(flags(cap.flight), [true, true, true]);
      assert.deepEqual(log.outcomes, []);
      assert.ok(!statuses(claim.run_id).includes("completed"));
    });
  });

  it("a definitive non-generic 4xx (run not owned) keeps the custody flags and stops the flight", async () => {
    const { claim, m, outbox, log } = await idleSetup({ script: [{ kind: "raw", status: 404, body: { error: "run not found for this worker" } }] });
    const r = build(parkingExecutor(log), outbox);
    const cap = captureFlight(r);
    await finish(r, claim);

    assert.equal(m.finalizeRequests, 1, "not retried");
    assert.deepEqual([cap.flight?.preserveRecoveryClone, cap.flight?.preserveSession, cap.flight?.stoppedSourceKept], [true, true, true]);
    assert.deepEqual(log.outcomes, []);
    assert.ok(!statuses(claim.run_id).includes("completed") && !statuses(claim.run_id).includes("failed"));
  });

  it("a malformed 409 is unconfirmed at the runner level: it resends until a reply confirms", async () => {
    const { claim, m, outbox, log } = await idleSetup({
      script: [
        { kind: "raw", status: 409, body: { applied: false }, apply: false },
        { kind: "raw", status: 409, body: { applied: false, reason: "something_else" }, apply: false },
        { kind: "admitted" },
      ],
    });
    await finish(build(parkingExecutor(log), outbox), claim);

    assert.equal(m.finalizeRequests, 3);
    assert.equal(statuses(claim.run_id).at(-1), "completed");
  });
  it("a cancel that arrives during the re-armed backoff beats a feature withdrawal and ends cancelled", async () => {
    const { claim, outbox, log } = await idleSetup({ script: [{ kind: "refuse", reason: "followup_pending", apply: false }] });
    const r = build(parkingExecutor(log), outbox);
    const cap = captureFlight(r);
    const seam = r as unknown as { waitRecoveryRetry: (...a: unknown[]) => Promise<void> };
    const real = seam.waitRecoveryRetry.bind(r);
    let first = true;
    seam.waitRecoveryRetry = async (...a) => {
      if (first) {
        first = false;
        cap.flight!.steering.markCancelled();
        client.protocolFeatures = BASE_FEATURES;
      }
      return real(...a);
    };
    await finish(r, claim);

    assert.deepEqual(log.outcomes[0], { kind: "ended", reason: "cancelled" }, "cancel wins over the withdrawal return");
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });

  it("an unknown send, then a followup_pending refusal, clears the ambiguity: a later withdrawn feature returns the outcome", async () => {
    observed.length = 0;
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "lost" }, { kind: "lost" }, { kind: "lost" }, { kind: "refuse", reason: "followup_pending", apply: false }],
      onFinalize: index => { if (index === ATTEMPTS) client.protocolFeatures = BASE_FEATURES; },
    });
    const sends = spyFinalizeSends();
    await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 400);

    assert.equal(sends.count, 2, "one unknown round, one refused round, then it stops sending");
    assert.equal(m.finalizeRequests, ATTEMPTS + 1);
    assert.deepEqual(log.outcomes[0], { kind: "ended", reason: "idle" });
  });

  it("a cancel set after the idle outcome but before the first send produces no followup_finalize request", async () => {
    const { claim, m, outbox, log } = await idleSetup();
    const r = build(parkingExecutor(log), outbox);
    captureFlight(r, flight => flight.steering.markCancelled());
    await finish(r, claim);

    assert.equal(m.finalizeRequests, 0);
    assert.deepEqual(log.outcomes.at(-1), { kind: "ended", reason: "cancelled" });
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });

  it("a ReportPreconditionError on the first send is not ambiguous: one attempt, the outcome is returned", async () => {
    observed.length = 0;
    const { claim, m, outbox, log } = await idleSetup();
    let attempts = 0;
    const real = client.reportState.bind(client);
    client.reportState = (async (...a: Parameters<typeof real>) => {
      if (a[1].status !== "followup_finalize") return real(...a);
      attempts++;
      throw new ReportPreconditionError("local guard");
    }) as typeof client.reportState;
    await observeThenCancel(build(parkingExecutor(log), outbox), claim, log, 400);

    assert.equal(attempts, 1, "never resent");
    assert.equal(m.finalizeRequests, 0);
    assert.deepEqual(log.outcomes[0], { kind: "ended", reason: "idle" });
  });
  it("a credential-switch signal thrown by the send after an unconfirmed attempt does not re-run the park and resends until admitted", async () => {
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "lost" }, { kind: "lost" }, { kind: "lost" }, { kind: "admitted" }],
    });
    let calls = 0;
    const r = build(parkingExecutor(log), outbox);
    captureFlight(r, flight => {
      // The closure consults this before every send: the second send is cut off by the switch signal.
      flight.steering.approveRefusedBySwitchPending = () => ++calls === 2;
    });
    await finish(r, claim);

    assert.equal(m.parkSeq, 1, "the park was not re-run");
    assert.equal(m.finalizeRequests, ATTEMPTS + 1, "the unknown round, the cut-off send (no request), then the admitting resend");
    assert.ok(calls >= 3, "the loop sent again after the signal");
    assert.deepEqual(log.outcomes, [{ kind: "ended", reason: "idle" }]);
    assert.equal(statuses(claim.run_id).at(-1), "completed");
  });

  it("a cancel that arrives during a rolled-back (generic 400) first send ends cancelled", async () => {
    const r0 = { cap: undefined as Captured | undefined };
    const { claim, m, outbox, log } = await idleSetup({
      script: [{ kind: "raw", status: 400, body: { error: GENERIC_400 } }],
      onFinalize: index => { if (index === 0) r0.cap!.flight!.steering.markCancelled(); },
    });
    const r = build(parkingExecutor(log), outbox);
    r0.cap = captureFlight(r);
    await finish(r, claim);

    assert.equal(m.finalizeRequests, 1);
    assert.deepEqual(log.outcomes.at(-1), { kind: "ended", reason: "cancelled" });
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });
  it("a cancel that arrives before a locally refused first send ends cancelled, not with the idle outcome", async () => {
    const { claim, outbox, log } = await idleSetup();
    const r = build(parkingExecutor(log), outbox);
    const cap = captureFlight(r);
    const real = client.reportState.bind(client);
    client.reportState = (async (...a: Parameters<typeof real>) => {
      if (a[1].status !== "followup_finalize") return real(...a);
      cap.flight!.steering.markCancelled();
      throw new ReportPreconditionError("local guard");
    }) as typeof client.reportState;
    await finish(r, claim);

    assert.deepEqual(log.outcomes.at(-1), { kind: "ended", reason: "cancelled" });
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });
});
