import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import type { RunRunner, ExecutorFactory } from "../src/runner.js";
import type { Logger } from "../src/log.js";
import type { StateRequest } from "../src/protocol.js";
import { type CacheCapConfig, DiskGovernor, DiskParkSignal } from "../src/cache-cap.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { SteeringChannel } from "../src/steering.js";
import { dataVolumeUsedFraction } from "../src/stats.js";
import { testLockOptions } from "./authority-test-support.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, input, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// Issue #3916: a mid-run data_volume_full disk park (the hard cache-cap layer) takes a NON-held
// restore-point capture. With an authority runtime the capture's fetch-back must acquire A, and the
// disk stop has already aborted the run's own lifecycle with its PauseNowSignal for good. The capture
// must not wait on that signal: it has to verify the tracking ref instead of reporting "unverified"
// on every retry. Driven through RunRunner.execute() with the production AuthorityRuntime.
//
// Milestone 2: a mid-run disk park (counted or preventive) also bounds its unverified captures like the
// terminal-disk park does, and a persistently incomplete settlement ends in the blocked bound.

const FEATURE = "recovery_cause_data_volume_full";
const FENCE = "claim_generation_fence";
const TOKEN = "unit-join-disk-park-3916";
const GIB = 1024 ** 3;
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const IID = 3916;
const DEADLINE_MS = 25_000;
const DEGRADED = "unverified work and session are retained";
const HAS_FD_DIR = fs.existsSync(["", "proc", "self", "fd"].join("/"));

const CONFIG: CacheCapConfig = {
  capEnabled: true,
  capFraction: 0.5,
  lowWater: 0.6,
  maxConcurrentRuns: 1,
  hardStopEnabled: true,
  hardMargin: 0.03,
};

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

function governor(): DiskGovernor {
  return new DiskGovernor({
    config: CONFIG,
    log: nullLogger(),
    volumeTotalBytes: () => 10 * GIB,
    thresholdOf: () => undefined,
    reclaim: async () => undefined,
    measure: async () => ({ cacheBytes: 3 * GIB, entries: 1, truncated: false }),
    trim: async (_home, target) => ({
      goBuildBytes: 0, goBuildUnits: 0, npmBytes: 0, npmBuckets: 0, npmContents: 0, kept: 0,
      moduleCacheRemoved: false, outOfTime: false, estimatedBytesAfter: target.totalBytes,
    }),
  });
}

/** A logger recording every warn so the test can read what the capture reported. */
function recordingLogger(): { logger: Logger; warns: Array<{ msg: string; fields?: Record<string, unknown> }> } {
  const warns: Array<{ msg: string; fields?: Record<string, unknown> }> = [];
  const self: Logger = {
    debug() {}, info() {}, error() {},
    warn: (msg, fields) => { warns.push({ msg, fields }); },
    addSecret() {}, removeSecret() {},
    child: () => self,
  };
  return { logger: self, warns };
}

interface Drive {
  /** Preventive park (a DiskParkSignal(true) from the cache-cap soft layer) instead of the hard stop. */
  preventive?: boolean;
  /** Run with the production AuthorityRuntime (the m1 scenario). */
  authority?: boolean;
  /** Stub runner methods before execute(). */
  tweak?: (runner: Any) => void;
  /** Called before the n-th (1-based) capture attempt, with the flight. */
  onCapture?: (n: number, flight: Any, runner: Any) => void | Promise<void>;
}

interface Driven {
  runId: string;
  outcome: "done" | "timeout";
  clone: string;
  headBeforeStop: string;
  trackingBeforeStop: string | null;
  captures: unknown[];
  captureCalls: () => number;
  warns: Array<{ msg: string; fields?: Record<string, unknown> }>;
  parks: StateRequest[];
  failed: StateRequest[];
  flight: Any;
  feed: () => string[];
}

/** One mid-run disk park driven through RunRunner.execute(): the executor commits work, then the hard
 *  stop (or a preventive DiskParkSignal) parks it and the recovery capture loop runs. */
async function drive(t: { skip(msg?: string): void }, o: Drive = {}): Promise<Driven | undefined> {
  if (!HAS_FD_DIR) { t.skip("no fd directory on this host: the cache drop refuses here by design"); return undefined; }
  client.protocolFeatures = [FEATURE, FENCE];
  const lifetime = o.authority ? await acquireWorkerLifetime(fs.realpathSync(fx.dataDir), testLockOptions()) : undefined;
  const rt = lifetime ? new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 }) : undefined;
  const gov = governor();
  const { logger, warns } = recordingLogger();
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3916-"));
  let clone = "";
  let headBeforeStop = "";
  let trackingBeforeStop: string | null = null;
  let inTurn: () => void = () => {};
  const turnStarted = new Promise<void>((r) => (inTurn = r));
  const factory: ExecutorFactory = (id) => {
    const home = path.join(homeRoot, id);
    return {
      homeDir: home,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.mkdirSync(home, { recursive: true });
          clone = ctx.worktreePath;
          assert.strictEqual(await ctx.cacheCapBoundary?.(async () => false), "continue");
          fs.writeFileSync(path.join(clone, "WORK.txt"), "committed before the stop\n");
          gitOut(clone, ["add", "WORK.txt"]);
          gitOut(clone, [...IDENT, "commit", "-m", "work"]);
          headBeforeStop = gitOut(clone, ["rev-parse", "HEAD"]);
          trackingBeforeStop = trackingSha(IID);
          if (o.preventive) {
            inTurn();
            throw new DiskParkSignal(true);
          }
          await new Promise<void>((resolve) => {
            if (ctx.signal?.aborted) return resolve();
            ctx.signal?.addEventListener("abort", () => resolve(), { once: true });
            setTimeout(resolve, 10_000).unref(); // bound a missing stop without hanging the flight
            inTurn();
          });
          if (ctx.pauseModeRequested?.() === "disk") throw new DiskParkSignal(false);
          throw new Error("the hard stop never arrived");
        },
      },
    };
  };
  const { gitlab } = fakeGitlab();
  const runner = runnerWith(factory, gitlab, undefined, logger, {
    diskGovernor: gov, ...(rt ? { authority: rt } : {}), recoveryRetryMs: 5, checkpointIntervalMs: 0,
  });
  Object.assign(runner, { attemptPaths: true });
  const captures: unknown[] = [];
  let calls = 0;
  let flightSeen: Any;
  const inner = runner as Any;
  const capture = inner.captureRecoveryRestorePoint.bind(runner);
  inner.captureRecoveryRestorePoint = async (...a: unknown[]) => {
    flightSeen = a[1];
    calls++;
    await o.onCapture?.(calls, a[1], runner);
    try {
      const result = await capture(...a);
      captures.push(result);
      return result;
    } catch (e) {
      captures.push({ threw: String(e) });
      throw e;
    }
  };
  o.tweak?.(runner);
  const claim = gitlabClaim(IID, { claim_generation: 4 });
  let outcome: "done" | "timeout";
  let timer: NodeJS.Timeout | undefined;
  try {
    const done = runner.execute(claim);
    const guard = new Promise<"timeout">((resolve) => {
      timer = setTimeout(() => { runner.shutdown(); resolve("timeout"); }, DEADLINE_MS);
      timer.unref();
    });
    if (!o.preventive) {
      await Promise.race([turnStarted, guard]);
      assert.ok(headBeforeStop, "the executor committed its work before the stop");
      const sample = {
        mem_bytes: 1, mem_limit_bytes: null, source: "process" as const,
        disk_data_bytes: 90, disk_data_total_bytes: 100, disk_data_inodes: 20, disk_data_total_inodes: 100,
      };
      gov.observe(dataVolumeUsedFraction(sample)); // hard stop: over 0.90 - 0.03
    }
    outcome = await Promise.race([done.then(() => "done" as const), guard]);
  } finally {
    clearTimeout(timer);
    runner.shutdown();
    fs.rmSync(homeRoot, { recursive: true, force: true });
  }
  const runId = claim.run_id;
  return {
    runId, outcome, clone, headBeforeStop, trackingBeforeStop, captures, captureCalls: () => calls, warns,
    parks: api.states.filter((s) => s.runId === runId && s.body.status === "recovery_wait").map((s) => s.body),
    failed: api.states.filter((s) => s.runId === runId && s.body.status === "failed").map((s) => s.body),
    flight: flightSeen,
    feed: () => api.messages(runId).filter((m) => m.kind === "status").map((m) => String(m.payload.text)),
  };
}

const liveSignal = (s: unknown): boolean => s instanceof AbortSignal && !s.aborted;
const failFetch = (runner: Any): void => {
  runner.fetchTracking = async () => { throw new Error("persistent fetch-back failure"); };
};
const degradedFeed = (d: Driven): boolean => d.feed().some((x) => x.includes(DEGRADED));

describe("RunRunner - issue #3916: a disk-park capture acquires A despite the aborted run lifecycle", () => {
  it("verifies the tracking ref at the pre-stop clone HEAD and parks data_volume_full without the degraded notice", async (t) => {
    // The body-signal half: the capture's bootstrap and publish run under the live (never aborted) wait
    // signal instead of the disk stop's aborted lifecycle, or with no signal at all.
    // Each entry: the signal was passed and was live at the call (it aborts later, at teardown).
    const bootstrapSignals: boolean[] = [];
    const publishSignals: boolean[] = [];
    let inCapture = false;
    const d = await drive(t, {
      authority: true,
      tweak: (runner) => {
        const boot = runner.bootstrapRemediationRestrictions.bind(runner);
        runner.bootstrapRemediationRestrictions = (...a: unknown[]) => {
          // The capture body's own call passes no bare path; bridge and overlay callers pass theirs.
          if (inCapture && a[1] === undefined) bootstrapSignals.push(liveSignal(a[3]));
          return boot(...a);
        };
        const publish = runner.publishCheckpointBestEffort.bind(runner);
        runner.publishCheckpointBestEffort = (...a: unknown[]) => {
          if (inCapture) publishSignals.push(liveSignal(a[4]));
          return publish(...a);
        };
        const cap = runner.captureRecoveryRestorePoint;
        runner.captureRecoveryRestorePoint = async (...a: unknown[]) => {
          inCapture = true;
          try { return await cap(...a); } finally { inCapture = false; }
        };
      },
    });
    if (!d) return;
    assert.notStrictEqual(d.trackingBeforeStop, d.headBeforeStop, "the tracking ref does not yet cover the clone HEAD");
    assert.strictEqual(d.outcome, "done", "the park completed under the deadline instead of retrying an unverified capture");
    // Verified, non-degraded and tracking-advanced come first: with the root fix reverted and the bound
    // kept, the run still parks (degraded), so these are the assertions that must go red.
    assert.strictEqual((d.captures[0] as { verified: boolean }).verified, true, "the capture verified");
    assert.strictEqual(trackingSha(IID), d.headBeforeStop, "the tracking ref equals the pre-stop clone HEAD");
    assert.strictEqual(degradedFeed(d), false, "the feed carries no degraded capture notice");
    assert.strictEqual(d.captures.length, 1, "exactly one capture attempt");
    assert.deepStrictEqual(d.warns.filter((w) => w.msg.includes("recovery capture: fetch-back failed")), [], "no fetch-back failure was logged");
    assert.ok(
      !d.warns.some((w) => JSON.stringify(w.fields ?? {}).includes("run paused (now)")),
      "nothing reported the run's own PauseNowSignal as a capture error",
    );
    assert.strictEqual(d.parks.length, 1, "one park");
    assert.strictEqual(d.parks[0]!.recovery_cause, "data_volume_full");
    assert.strictEqual("disk_park_preventive" in d.parks[0]!, false, "the hard stop is a counted park");
    assert.ok(bootstrapSignals.length >= 1, "the capture bootstrapped the remediation restrictions");
    assert.deepStrictEqual(bootstrapSignals.filter((live) => !live), [], "bootstrapRemediationRestrictions gets a live signal");
    assert.ok(publishSignals.length >= 1, "the capture published its checkpoint");
    assert.deepStrictEqual(publishSignals.filter((live) => !live), [], "publishCheckpointBestEffort gets a live signal");
  });
});

describe("RunRunner - issue #3916: a mid-run disk park bounds its unverified captures", () => {
  it("a counted park with a persistent fetch-back failure degrades after three captures and keeps the clone", async (t) => {
    const d = await drive(t, { tweak: failFetch });
    if (!d) return;
    assert.strictEqual(d.outcome, "done", "the park completed under the deadline");
    assert.strictEqual(d.captureCalls(), 3, "exactly three capture attempts");
    assert.ok(d.captures.every((c) => (c as { verified?: boolean }).verified !== true), "no capture ever verified");
    assert.strictEqual(d.parks.length, 1, "one park");
    assert.strictEqual(d.parks[0]!.recovery_cause, "data_volume_full");
    assert.strictEqual("disk_park_preventive" in d.parks[0]!, false, "the hard stop stays a counted park");
    assert.deepStrictEqual(d.failed, [], "never a failure");
    assert.ok(degradedFeed(d), "the feed says the unverified work is retained on this worker");
    assert.ok(fs.existsSync(d.clone), "the clone is retained");
    assert.strictEqual(d.flight.preserveRecoveryClone, true);
    assert.strictEqual(d.flight.preserveSession, true);
  });

  it("a preventive park degrades the same way and keeps disk_park_preventive", async (t) => {
    const d = await drive(t, { preventive: true, tweak: failFetch });
    if (!d) return;
    assert.strictEqual(d.outcome, "done", "the park completed under the deadline");
    assert.strictEqual(d.captureCalls(), 3, "exactly three capture attempts");
    assert.strictEqual(d.parks.length, 1, "one park");
    assert.strictEqual(d.parks[0]!.recovery_cause, "data_volume_full");
    assert.strictEqual(d.parks[0]!.disk_park_preventive, true, "the preventive flag is unchanged");
    assert.deepStrictEqual(d.failed, []);
    assert.ok(degradedFeed(d), "the degraded notice is on the feed");
    assert.ok(fs.existsSync(d.clone), "the clone is retained");
  });

  it("an unproven final quiescence fails worker_residue_blocked and keeps the clone and custody", async (t) => {
    let releases = 0;
    const d = await drive(t, {
      tweak: (runner) => {
        failFetch(runner);
        const quiesce = runner.quiesceRun.bind(runner);
        runner.quiesceRun = (flight: unknown, executor: unknown, opts: { site: string }) =>
          opts.site === "terminal_disk:after_runner_git"
            ? Promise.resolve({
              outcome: { process: { state: "unverified", processes: [], killed: [], detail: "stub: not provable" }, docker: { state: "not_wired", removed: [], detail: "" } },
              blocked: true,
            })
            : quiesce(flight, executor, opts);
        const settle = runner.settleRecoveryGeneration.bind(runner);
        runner.settleRecoveryGeneration = (...a: unknown[]) => { releases++; return settle(...a); };
      },
    });
    if (!d) return;
    assert.strictEqual(d.outcome, "done");
    assert.strictEqual(d.captureCalls(), 3, "the bound ends the captures");
    assert.deepStrictEqual(d.parks, [], "an unproven quiescence never parks");
    assert.strictEqual(d.failed.length, 1);
    assert.strictEqual(d.failed[0]!.fail_origin, "worker_residue_blocked");
    assert.ok(String(d.failed[0]!.failure_reason).includes("final quiescence unproven"), "the existing reason text");
    assert.strictEqual(releases, 0, "custody was not settled or released");
    assert.ok(fs.existsSync(d.clone), "the clone is kept");
  });

  // A typed error is a canonical recovery interruption, so this also pins the `held &&` guard: only a
  // held capture rethrows from the mid-run proof.
  it("a final quiescence proof that throws a typed error fails worker_residue_blocked and never escapes", async (t) => {
    let proofs = 0;
    const d = await drive(t, {
      tweak: (runner) => {
        failFetch(runner);
        const quiesce = runner.quiesceRun.bind(runner);
        runner.quiesceRun = (flight: unknown, executor: unknown, opts: { site: string }) => {
          if (opts.site !== "terminal_disk:after_runner_git") return quiesce(flight, executor, opts);
          proofs++;
          return Promise.reject(new TypeError("stub: proof probe failed"));
        };
      },
    });
    if (!d) return;
    assert.strictEqual(d.outcome, "done", "the error did not escape execute()");
    assert.strictEqual(proofs, 5, "every bounded proof attempt threw");
    assert.deepStrictEqual(d.parks, [], "an unproven quiescence never parks");
    assert.strictEqual(d.failed.length, 1);
    assert.strictEqual(d.failed[0]!.fail_origin, "worker_residue_blocked");
    assert.ok(String(d.failed[0]!.failure_reason).includes("final quiescence unproven"), "the existing reason text");
    assert.ok(fs.existsSync(d.clone), "the clone is kept");
  });

  it("a persistent incomplete settlement fails worker_residue_blocked after five blocked attempts without any capture", async (t) => {
    let settlements = 0;
    let captureWork = 0;
    const d = await drive(t, {
      tweak: (runner) => {
        runner.settleForCredentialCapture = async () => { settlements++; return { kind: "settlement_incomplete", errors: [{ category: "stub" }] }; };
        runner.captureRecoveryRestorePointBody = async () => { captureWork++; return { verified: true, published: true }; };
      },
    });
    if (!d) return;
    assert.strictEqual(d.outcome, "done");
    assert.strictEqual(settlements, 5, "the blocked bound ends the retries");
    assert.strictEqual(captureWork, 0, "no capture ran: the settlement gave no source permission");
    assert.deepStrictEqual(d.parks, []);
    assert.strictEqual(d.failed.length, 1);
    assert.strictEqual(d.failed[0]!.fail_origin, "worker_residue_blocked");
    assert.ok(String(d.failed[0]!.failure_reason).includes("authenticated settlement incomplete"));
    // No custody observable here: keepCustody is inert for RunResidueBlockedError (reportGenericFailure
    // forces reaped=false for that error type), so custody retention is not asserted for this bound.
    assert.ok(fs.existsSync(d.clone), "the clone is kept");
  });
});

describe("RunRunner - issue #3916: precedence while a disk park retries its capture", () => {
  const onSecond = (act: (flight: Any, runner: Any) => void | Promise<void>) =>
    async (n: number, flight: Any, runner: Any) => { if (n === 2) await act(flight, runner); };
  const until = async (ready: () => boolean): Promise<void> => {
    for (let i = 0; i < 2000 && !ready(); i++) await new Promise((r) => setTimeout(r, 5));
  };

  it("a real cancel input delivered during the retries reports cancelled and never parks", async (t) => {
    const d = await drive(t, {
      tweak: failFetch,
      onCapture: onSecond(async (flight) => {
        api.appendInputs(flight.runId, [input("cancel")]);
        await until(() => flight.steering.isCancelled());
        assert.strictEqual(flight.steering.isCancelled(), true, "the polled cancel input was routed");
      }),
    });
    if (!d) return;
    assert.strictEqual(d.outcome, "done");
    assert.deepStrictEqual(d.parks, []);
    assert.strictEqual(d.failed.length, 1);
    assert.strictEqual(d.failed[0]!.failure_reason, "run cancelled");
  });

  it("a pending-only cancellation (markCancelled) reports cancelled and never parks", async (t) => {
    const d = await drive(t, { tweak: failFetch, onCapture: onSecond((flight) => flight.steering.markCancelled()) });
    if (!d) return;
    assert.strictEqual(d.outcome, "done");
    assert.deepStrictEqual(d.parks, []);
    assert.strictEqual(d.failed.length, 1);
    assert.strictEqual(d.failed[0]!.failure_reason, "run cancelled");
  });

  it("a claim fence during the retries reports nothing: no park and no failure", async (t) => {
    const d = await drive(t, { tweak: failFetch, onCapture: onSecond((flight) => flight.steering.endFencedFlight("stale")) });
    if (!d) return;
    assert.strictEqual(d.outcome, "done");
    assert.deepStrictEqual(d.parks, [], "a fenced claim never parks");
    assert.deepStrictEqual(d.failed, [], "and never fails the run");
    assert.ok(fs.existsSync(d.clone), "the clone is kept");
  });

  it("a worker shutdown during the retries retains the clone and reports no park", async (t) => {
    const d = await drive(t, { tweak: failFetch, onCapture: onSecond((_flight, runner) => runner.shutdown()) });
    if (!d) return;
    assert.strictEqual(d.outcome, "done");
    assert.deepStrictEqual(d.parks, []);
    assert.deepStrictEqual(d.failed, [], "a shutdown never reports a failure");
    assert.ok(fs.existsSync(d.clone), "the clone is kept for the requeue");
  });

  for (const mode of ["milestone", "now", "wall"]) {
    it(`a preventive park with an owner ${mode} pause pending still ends in the bounded degraded park`, async (t) => {
      // The park already ends the flight, so the pending owner pause is not an interruption of its proof.
      const d = await drive(t, {
        preventive: true,
        tweak: failFetch,
        onCapture: (n, flight) => { if (n === 1) flight.steering.route("pause", mode, 991, false, undefined); },
      });
      if (!d) return;
      assert.strictEqual(d.outcome, "done", "execute() resolved instead of rejecting");
      assert.strictEqual(d.parks.length, 1, "the degraded park was reported");
      assert.strictEqual(d.parks[0]!.recovery_cause, "data_volume_full");
      assert.strictEqual(d.parks[0]!.disk_park_preventive, true);
      assert.deepStrictEqual(d.failed, []);
      assert.ok(degradedFeed(d), "the degraded notice is on the feed");
      assert.ok(fs.existsSync(d.clone), "the clone is retained");
    });
  }

  const unprovenProof = (during: (flight: Any, runner: Any) => void) => (runner: Any): void => {
    failFetch(runner);
    const quiesce = runner.quiesceRun.bind(runner);
    let first = true;
    runner.quiesceRun = (flight: Any, executor: unknown, opts: { site: string }) => {
      if (opts.site !== "terminal_disk:after_runner_git") return quiesce(flight, executor, opts);
      if (first) { first = false; during(flight, runner); }
      return Promise.resolve({
        outcome: { process: { state: "unverified", processes: [], killed: [], detail: "stub: not provable" }, docker: { state: "not_wired", removed: [], detail: "" } },
        blocked: true,
      });
    };
  };

  it("a worker shutdown during the final proof resolves with no park and no failed report, keeping the clone", async (t) => {
    let proofs = 0;
    const d = await drive(t, { tweak: unprovenProof((_f, r) => { proofs++; r.shutdown(); }) });
    if (!d) return;
    assert.strictEqual(proofs, 1, "the proof ran once");
    assert.strictEqual(d.outcome, "done", "execute() resolved instead of rejecting");
    assert.strictEqual(d.captureCalls(), 3, "no capture reran after the proof");
    assert.deepStrictEqual(d.parks, []);
    assert.deepStrictEqual(d.failed, []);
    assert.ok(fs.existsSync(d.clone), "the clone is kept for the requeue");
  });

  it("a cancel during the final proof reports cancelled and never parks", async (t) => {
    const d = await drive(t, { tweak: unprovenProof((flight) => flight.steering.markCancelled()) });
    if (!d) return;
    assert.strictEqual(d.outcome, "done");
    assert.strictEqual(d.captureCalls(), 3, "no capture reran after the proof");
    assert.deepStrictEqual(d.parks, []);
    assert.strictEqual(d.failed.length, 1);
    assert.strictEqual(d.failed[0]!.failure_reason, "run cancelled");
  });

  it("a credential switch after the disk stop does not stop the disk park from completing", async (t) => {
    // Pinned behaviour: the disk stop already spent the flight's cancel controller, so the switch only
    // records its pending generation; the park still lands.
    const d = await drive(t, { onCapture: onSecond((flight) => flight.steering.maybeTripCredentialSwitch(flight.claimGeneration)) });
    if (!d) return;
    assert.strictEqual(d.outcome, "done");
    assert.strictEqual(d.parks.length, 1);
    assert.strictEqual(d.parks[0]!.recovery_cause, "data_volume_full");
    assert.deepStrictEqual(d.failed, []);
  });
});

describe("RunRunner.diskParkCaptureWait - issue #3916", () => {
  const channel = () => new SteeringChannel({ getInputs: async () => ({ inputs: [] }) } as never, "run-1", 1, nullLogger(), new AbortController());
  const waitOf = (runner: RunRunner, ch: SteeringChannel) =>
    (runner as unknown as { diskParkCaptureWait(f: unknown): { signal: AbortSignal; timeoutMs: number } | undefined })
      .diskParkCaptureWait({ steering: ch });
  const newRunner = () => runnerWith(() => ({ executor: {} as never }), fakeGitlab().gitlab);
  const route = (ch: SteeringChannel, kind: string): void =>
    (ch as unknown as { route(k: string, b: null, id: number, receipted: boolean, at: undefined): void })
      .route(kind, null, 1, false, undefined);

  it("is undefined outside disk mode", () => {
    assert.strictEqual(waitOf(newRunner(), channel()), undefined);
  });

  it("stays live after the disk stop aborted the run's own lifecycle", () => {
    const ch = channel();
    ch.requestDiskStop();
    assert.strictEqual(ch.lifecycleSignal().aborted, true, "the disk stop aborted the lifecycle");
    const wait = waitOf(newRunner(), ch)!;
    assert.strictEqual(wait.signal.aborted, false);
    assert.ok(wait.timeoutMs >= 1);
  });

  it("aborts on a real cancel input routed through the steering channel", () => {
    const ch = channel();
    ch.requestDiskStop();
    const wait = waitOf(newRunner(), ch)!;
    route(ch, "cancel");
    assert.strictEqual(ch.isCancelled(), true);
    assert.strictEqual(wait.signal.aborted, true);
  });

  it("aborts on a claim fence (endFencedFlight)", () => {
    const ch = channel();
    ch.requestDiskStop();
    const wait = waitOf(newRunner(), ch)!;
    (ch as unknown as { endFencedFlight(r: string): void }).endFencedFlight("stale");
    assert.strictEqual(ch.claimFence(), "stale");
    assert.strictEqual(wait.signal.aborted, true);
  });

  it("aborts on an input-receipt failure", () => {
    const ch = channel();
    ch.requestDiskStop();
    const wait = waitOf(newRunner(), ch)!;
    (ch as unknown as { failReceipts(e: Error): void }).failReceipts(new Error("receipt gave up"));
    assert.strictEqual(wait.signal.aborted, true);
  });

  it("aborts on runner shutdown", () => {
    const ch = channel();
    ch.requestDiskStop();
    const runner = newRunner();
    const wait = waitOf(runner, ch)!;
    assert.strictEqual(wait.signal.aborted, false);
    runner.shutdown();
    assert.strictEqual(wait.signal.aborted, true);
  });
});
