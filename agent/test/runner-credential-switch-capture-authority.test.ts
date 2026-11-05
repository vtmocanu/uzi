import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { CredentialSwitchSignal, SteeringChannel } from "../src/steering.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// Issue #3953: a credential switch aborts the run's cancel signal and its lifecycle with its own
// "run released for a credential switch" signal. Its non-held restore-point capture must acquire source
// authority (the fetch-back) without waiting on those aborted signals, or every attempt reports unverified
// and the switch gives up. Driven through RunRunner.execute() with a test-owned AuthorityRuntime (the
// shared harness git has no source-authority hooks), like runner-disk-park-capture-authority.test.ts.

const TOKEN = "unit-join-credential-switch-3953";
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const IID = 3953;
const GENERATION = 4;
const DEADLINE_MS = 25_000;

type Any = any;

const gitOut = (tree: string, args: string[]): string =>
  execFileSync("git", ["-C", tree, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();

function trackingSha(iid: number): string | null {
  try {
    return gitOut(git.barePathFor(fx.originPath), ["rev-parse", "--verify", `refs/uzi-runner/agent/issue-${iid}`]);
  } catch {
    return null;
  }
}

interface Drive {
  /** Called before the n-th (1-based) capture attempt. `defer` registers teardown work (run in finally). */
  /** Called after the n-th capture attempt returned or threw. */
  afterCapture?: (n: number, runner: Any) => void;
  onCapture?: (n: number, flight: Any, runner: Any, rt: AuthorityRuntime, defer: (fn: () => void) => void) => void | Promise<void>;
}

async function drive(o: Drive = {}) {
  const lifetime = await acquireWorkerLifetime(fs.realpathSync(fx.dataDir), testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3953-"));
  const cleanups: Array<() => void> = [];
  let clone = "";
  let headBeforeSwitch = "";
  let trackingBefore: string | null = null;
  let executorRuns = 0;
  const factory: ExecutorFactory = (id) => {
    const home = path.join(homeRoot, id);
    return {
      homeDir: home,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          executorRuns++;
          fs.mkdirSync(home, { recursive: true });
          clone = ctx.worktreePath;
          fs.writeFileSync(path.join(clone, "WORK.txt"), "committed before the switch\n");
          gitOut(clone, ["add", "WORK.txt"]);
          gitOut(clone, [...IDENT, "commit", "-m", "work"]);
          headBeforeSwitch = gitOut(clone, ["rev-parse", "HEAD"]);
          trackingBefore = trackingSha(IID);
          api.requestCredentialSwitch(claim.run_id, GENERATION);
          await new Promise<void>((resolve) => {
            if (ctx.signal?.aborted) return resolve();
            ctx.signal?.addEventListener("abort", () => resolve(), { once: true });
            setTimeout(resolve, 10_000).unref(); // bound a missing switch without hanging the flight
          });
          throw new CredentialSwitchSignal();
        },
      },
    };
  };
  const { gitlab } = fakeGitlab();
  const authorityGit = new GitCache(fs.realpathSync(fx.dataDir), nullLogger(), undefined, testGitCacheOptions({
    authority: rt.hooks, authorityRequired: true, gitleaksBin: defaultGitleaksShim(),
  }));
  const runner = new RunRunner(client, authorityGit, factory, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    prDescriptionHeadLagMs: 0,
    gitlab,
    teardownTestDeps: portableTeardownTestDeps,
    authority: rt,
    recoveryRetryMs: 5,
    checkpointIntervalMs: 0,
    checkpointTestHooks: { retryBackoffMs: [0, 0] },
  });
  Object.assign(runner, { attemptPaths: true });
  const claim = gitlabClaim(IID, { claim_generation: GENERATION });
  api.overrideStateStatus(claim.run_id, "queued"); // a release requeues the run
  // The capture's OWN authority acquisitions: was the wait signal already aborted when it was requested?
  // Only acquisitions made while captureRecoveryRestorePoint is running count.
  const acquisitions: boolean[] = [];
  let inCapture = false;
  const withTransaction = rt.withTransaction.bind(rt) as Any;
  (rt as Any).withTransaction = (fn: unknown, wait?: { signal?: AbortSignal }) => {
    if (inCapture) acquisitions.push(wait?.signal?.aborted === true);
    return withTransaction(fn, wait);
  };
  let calls = 0;
  let flightSeen: Any;
  const inner = runner as Any;
  const capture = inner.captureRecoveryRestorePoint.bind(runner);
  inner.captureRecoveryRestorePoint = async (...a: unknown[]) => {
    flightSeen = a[1];
    calls++;
    await o.onCapture?.(calls, a[1], runner, rt, (fn) => cleanups.push(fn));
    inCapture = true;
    try { return await capture(...a); } finally { inCapture = false; o.afterCapture?.(calls, runner); }
  };
  let timer: NodeJS.Timeout | undefined;
  let outcome: "done" | "timeout";
  try {
    const done = runner.execute(claim);
    const guard = new Promise<"timeout">((resolve) => {
      timer = setTimeout(() => { runner.shutdown(); resolve("timeout"); }, DEADLINE_MS);
      timer.unref();
    });
    outcome = await Promise.race([done.then(() => "done" as const), guard]);
  } finally {
    clearTimeout(timer);
    for (const fn of cleanups) fn();
    runner.shutdown();
    fs.rmSync(homeRoot, { recursive: true, force: true });
  }
  const runId = claim.run_id;
  const statuses = api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
  return {
    runId, outcome, clone, headBeforeSwitch, trackingBefore, statuses, acquisitions, executorRuns,
    captureCalls: () => calls, flight: flightSeen,
    failed: api.states.filter((s) => s.runId === runId && s.body.status === "failed").map((s) => s.body),
  };
}

const onFirst = (act: (flight: Any, runner: Any, rt: AuthorityRuntime, defer: (fn: () => void) => void) => void | Promise<void>) =>
  async (n: number, flight: Any, runner: Any, rt: AuthorityRuntime, defer: (fn: () => void) => void) => {
    if (n === 1) await act(flight, runner, rt, defer);
  };

describe("RunRunner - issue #3953: a credential-switch capture acquires A despite the switch's own abort", () => {
  it("verifies the tracking ref and releases the run, never waiting on an aborted signal", async () => {
    const d = await drive();
    assert.strictEqual(d.outcome, "done", "the switch completed under the deadline");
    assert.ok(d.acquisitions.length >= 1, "the capture itself consulted the authority wait");
    assert.deepStrictEqual(d.acquisitions.filter((aborted) => aborted), [], "no wait signal was aborted at acquisition");
    assert.strictEqual(trackingSha(IID), d.headBeforeSwitch, "the tracking ref equals the pre-switch clone HEAD");
    assert.ok(d.statuses.includes("credential_switch"), "the release reported credential_switch");
    assert.ok(!d.statuses.includes("credential_switch_failed"), "the switch did not give up");
    assert.strictEqual(d.captureCalls(), 1, "exactly one capture attempt");
  });
});

// The fetch-back is NOT stubbed below: each stop must reach the capture's real authority acquisition through
// the credential-switch capture wait, so a wait that ignored the stop would verify and release.
// The switch loop still reports credential_switch_failed after such a stop; that give-up report is
// pre-existing loop behaviour and is not asserted here. What matters is that nothing was verified,
// published or released, and the run did not continue (one executor turn, then a terminal end).
describe("RunRunner - issue #3953: every stop reaches the capture's own authority wait", () => {
  const assertStopped = (d: Awaited<ReturnType<typeof drive>>, expectAborted: boolean) => {
    assert.strictEqual(d.outcome, "done", `execute() resolved under the deadline (statuses ${d.statuses.join(",")}; captures ${d.captureCalls()}; acquisitions ${d.acquisitions.join(",")})`);
    assert.ok(d.acquisitions.length >= 1, "the capture itself consulted the authority wait");
    if (expectAborted) assert.ok(d.acquisitions.every((aborted) => aborted), "every capture acquisition saw an aborted signal");
    assert.strictEqual(trackingSha(IID), d.trackingBefore, "the tracking ref did not advance: nothing was verified");
    assert.notStrictEqual(trackingSha(IID), d.headBeforeSwitch, "the tracking ref does not cover the pre-switch HEAD");
    assert.ok(!d.statuses.includes("credential_switch"), "no release was reported");
    assert.ok(!d.statuses.includes("completed"), "the run did not complete");
    assert.strictEqual(d.executorRuns, 1, "no further executor turn after the capture");
  };

  it("a real cancel stops the capture", async () => {
    const d = await drive({
      onCapture: onFirst((flight) => {
        flight.steering.route("cancel", null, 991, false, undefined);
        assert.strictEqual(flight.steering.isCancelled(), true, "the cancel input was routed");
      }),
    });
    assertStopped(d, true);
  });

  it("a claim fence stops the capture", async () => {
    const d = await drive({ onCapture: onFirst((flight) => flight.steering.endFencedFlight("stale")) });
    assertStopped(d, true);
    assert.deepStrictEqual(d.failed, [], "a fenced claim never fails the run");
  });

  it("an input-receipt failure stops the capture", async () => {
    const d = await drive({ onCapture: onFirst((flight) => flight.steering.failReceipts(new Error("receipt gave up"))) });
    assertStopped(d, true);
  });

  it("a worker shutdown stops the capture", async () => {
    const d = await drive({ onCapture: onFirst((_flight, runner) => runner.shutdown()) });
    assertStopped(d, true);
    assert.deepStrictEqual(d.failed, [], "a shutdown never reports a failure");
  });

  it("an expired boundary deadline stops the capture while another holder owns A", async () => {
    let releaseHold: () => void = () => {};
    const d = await drive({
      // The bound is three attempts, all expiring. Once the last has failed, free A (so the give-up and the
      // shutdown cleanup are not themselves starved of it) and end the otherwise idle run.
      afterCapture: (n, runner) => { if (n === 3) { releaseHold(); runner.shutdown(); } },
      onCapture: onFirst(async (_flight, runner, rt, defer) => {
        runner.codexBoundaryDeadlineMs = 300;
        let entered!: () => void;
        const inside = new Promise<void>((r) => (entered = r));
        let release!: () => void;
        const hold = new Promise<void>((r) => (release = r));
        const holder = rt.withTransaction(async () => { entered(); await hold; });
        releaseHold = release; // released after the last attempt, or in teardown; never on a timer
        defer(release);
        defer(() => { holder.catch(() => undefined); });
        await inside;
      }),
    });
    // The wait signal never aborts here: the bounded acquisition itself expires.
    assertStopped(d, false);
  });
});

describe("RunRunner.credentialSwitchCaptureWait - issue #3953", () => {
  const channel = () => new SteeringChannel({ getInputs: async () => ({ inputs: [] }) } as never, "run-1", 1, nullLogger(), new AbortController(), { claimGeneration: 1 });
  type Wait = { signal: AbortSignal; timeoutMs: number };
  const waitOf = (runner: RunRunner, ch: SteeringChannel, boundaryMs?: number): Wait =>
    (runner as unknown as { credentialSwitchCaptureWait(f: unknown, b?: number): Wait })
      .credentialSwitchCaptureWait({ steering: ch }, boundaryMs);
  const newRunner = () => runnerWith(() => ({ executor: {} as never }), fakeGitlab().gitlab);
  const trip = (ch: SteeringChannel): void => (ch as unknown as { maybeTripCredentialSwitch(g: number): void }).maybeTripCredentialSwitch(1);
  const route = (ch: SteeringChannel, kind: string): void =>
    (ch as unknown as { route(k: string, b: null, id: number, receipted: boolean, at: undefined): void })
      .route(kind, null, 1, false, undefined);

  it("stays live after the switch aborted the run's cancel and lifecycle signals", () => {
    const ch = channel();
    ch.tripCredentialSwitch(1);
    assert.strictEqual(ch.lifecycleSignal().aborted, true, "the switch aborted the lifecycle");
    const wait = waitOf(newRunner(), ch);
    assert.strictEqual(wait.signal.aborted, false);
    assert.ok(wait.timeoutMs >= 1);
  });

  it("aborts on a real cancel input routed through the steering channel", () => {
    const ch = channel();
    trip(ch);
    const wait = waitOf(newRunner(), ch);
    route(ch, "cancel");
    assert.strictEqual(ch.isCancelled(), true);
    assert.strictEqual(wait.signal.aborted, true);
  });

  it("aborts on a claim fence (endFencedFlight)", () => {
    const ch = channel();
    trip(ch);
    const wait = waitOf(newRunner(), ch);
    (ch as unknown as { endFencedFlight(r: string): void }).endFencedFlight("stale");
    assert.strictEqual(wait.signal.aborted, true);
  });

  it("aborts on an input-receipt failure", () => {
    const ch = channel();
    trip(ch);
    const wait = waitOf(newRunner(), ch);
    (ch as unknown as { failReceipts(e: Error): void }).failReceipts(new Error("receipt gave up"));
    assert.strictEqual(wait.signal.aborted, true);
  });

  it("aborts on runner shutdown", () => {
    const ch = channel();
    trip(ch);
    const runner = newRunner();
    const wait = waitOf(runner, ch);
    assert.strictEqual(wait.signal.aborted, false);
    runner.shutdown();
    assert.strictEqual(wait.signal.aborted, true);
  });

  it("bounds the wait by the boundary deadline, never below one millisecond", () => {
    const runner = newRunner();
    assert.strictEqual(waitOf(runner, channel(), 7).timeoutMs, 7);
    assert.strictEqual(waitOf(runner, channel(), 0).timeoutMs, 1);
  });
});
