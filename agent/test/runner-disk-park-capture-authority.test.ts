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
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// Issue #3916: a mid-run data_volume_full disk park (the hard cache-cap layer) takes a NON-held
// restore-point capture. With an authority runtime the capture's fetch-back must acquire A, and the
// disk stop has already aborted the run's own lifecycle with its PauseNowSignal for good. The capture
// must not wait on that signal: it has to verify the tracking ref instead of reporting "unverified"
// on every retry. Driven through RunRunner.execute() with the production AuthorityRuntime.

const FEATURE = "recovery_cause_data_volume_full";
const FENCE = "claim_generation_fence";
const TOKEN = "unit-join-disk-park-3916";
const GIB = 1024 ** 3;
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const IID = 3916;
const DEADLINE_MS = 25_000;
const DEGRADED = "unverified work and session are retained";

const CONFIG: CacheCapConfig = {
  capEnabled: true,
  capFraction: 0.5,
  lowWater: 0.6,
  maxConcurrentRuns: 1,
  hardStopEnabled: true,
  hardMargin: 0.03,
};

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

describe("RunRunner - issue #3916: a disk-park capture acquires A despite the aborted run lifecycle", () => {
  it("verifies the tracking ref at the pre-stop clone HEAD and parks data_volume_full without the degraded notice", async (t) => {
    if (!fs.existsSync("/proc/self/fd")) return t.skip("no /proc/self/fd on this host: the cache drop refuses here by design");
    client.protocolFeatures = [FEATURE, FENCE];
    const lifetime = await acquireWorkerLifetime(fs.realpathSync(fx.dataDir), testLockOptions());
    const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
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
            await new Promise<void>((resolve) => {
              if (ctx.signal?.aborted) return resolve();
              ctx.signal?.addEventListener("abort", () => resolve(), { once: true });
              setTimeout(resolve, 10_000); // bound a missing stop without hanging the flight
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
      diskGovernor: gov, authority: rt, recoveryRetryMs: 5, checkpointIntervalMs: 0,
    });
    Object.assign(runner, { attemptPaths: true });
    const captures: unknown[] = [];
    const inner = runner as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
    const capture = inner.captureRecoveryRestorePoint!.bind(runner);
    inner.captureRecoveryRestorePoint = async (...a) => {
      try {
        const result = await capture(...a);
        captures.push(result);
        return result;
      } catch (e) {
        captures.push({ threw: String(e) });
        throw e;
      }
    };
    const claim = gitlabClaim(IID, { claim_generation: 4 });
    try {
      const done = runner.execute(claim);
      let timer: NodeJS.Timeout | undefined;
      const guard = new Promise<"timeout">((resolve) => {
        timer = setTimeout(() => { runner.shutdown(); resolve("timeout"); }, DEADLINE_MS);
      });
      await Promise.race([turnStarted, guard]);
      assert.ok(headBeforeStop, "the executor committed its work before the stop");
      assert.notStrictEqual(trackingBeforeStop, headBeforeStop, "the tracking ref does not yet cover the clone HEAD");
      const sample = {
        mem_bytes: 1, mem_limit_bytes: null, source: "process" as const,
        disk_data_bytes: 90, disk_data_total_bytes: 100, disk_data_inodes: 20, disk_data_total_inodes: 100,
      };
      gov.observe(dataVolumeUsedFraction(sample)); // hard stop: over 0.90 - 0.03
      const outcome = await Promise.race([done.then(() => "done" as const), guard]);
      clearTimeout(timer);
      assert.strictEqual(outcome, "done", "the park completed under the deadline instead of retrying an unverified capture");
    } finally {
      runner.shutdown();
    }

    const runId = claim.run_id;
    assert.strictEqual(captures.length, 1, "exactly one capture attempt");
    const result = captures[0] as { verified: boolean };
    assert.strictEqual(result.verified, true, "the capture verified");
    assert.strictEqual(trackingSha(IID), headBeforeStop, "the tracking ref equals the pre-stop clone HEAD");
    const fetchBackFailures = warns.filter((w) => w.msg.includes("recovery capture: fetch-back failed"));
    assert.deepStrictEqual(fetchBackFailures, [], "no fetch-back failure was logged");
    assert.ok(
      !warns.some((w) => JSON.stringify(w.fields ?? {}).includes("run paused (now)")),
      "nothing reported the run's own PauseNowSignal as a capture error",
    );
    const parkStates: StateRequest[] = api.states.filter((s) => s.runId === runId && s.body.status === "recovery_wait").map((s) => s.body);
    assert.strictEqual(parkStates.length, 1, "one park");
    assert.strictEqual(parkStates[0]!.recovery_cause, "data_volume_full");
    assert.strictEqual("disk_park_preventive" in parkStates[0]!, false, "the hard stop is a counted park");
    assert.ok(
      !api.messages(runId).some((m) => m.kind === "status" && String(m.payload.text).includes(DEGRADED)),
      "the feed carries no degraded capture notice",
    );
  });
});

describe("RunRunner.diskParkCaptureWait - issue #3916", () => {
  const channel = () => new SteeringChannel({ getInputs: async () => ({ inputs: [] }) } as never, "run-1", 1, nullLogger(), new AbortController());
  const waitOf = (runner: RunRunner, ch: SteeringChannel) =>
    (runner as unknown as { diskParkCaptureWait(f: unknown): { signal: AbortSignal; timeoutMs: number } | undefined })
      .diskParkCaptureWait({ steering: ch });
  const newRunner = () => runnerWith(() => ({ executor: {} as never }), fakeGitlab().gitlab);

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

  it("aborts on a real cancel (terminal lifecycle abort)", () => {
    const ch = channel();
    ch.requestDiskStop();
    const wait = waitOf(newRunner(), ch)!;
    ch.abortLifecycle(new Error("cancelled"));
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
