import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory, RunRunner } from "../src/runner.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import type { RecoveryCoordinator } from "../src/recovery.js";
import type { ProcessQuiescenceState, QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import type { StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

// Cross-feature regression (issue #1783 x PRD #1809/#1826): a MID-RUN data_volume_full park takes
// handleRecoveryExhausted's disk branch, which runs #1826's attributed-process reap
// (executor.reapAttributedProcesses) and drops the run caches BEFORE the capture. That reap is a
// best-effort kill, not a proof: captureRecoveryRestorePoint still opens with the #1783 quiescence
// proof, and a `survivors`/`unverified` proof must skip the whole capture (no wip marker, no
// fetch-back, no PAT-bearing overlay, no checkpoint publish) and keep the clone and the custody hold,
// however "successful" the reap was. A `quiescent` control proves the negative assertions are live.
// The quiescence primitive and the recovery coordinator are injected; the disk signal is thrown by a
// fake executor, never a real full disk.

installHarness();

const FEATURE = "recovery_cause_data_volume_full";
const FENCE = "claim_generation_fence";
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const RUNNER_OPTS = { checkpointIntervalMs: 0, recoveryRetryMs: 1 };
/** How many blocked capture attempts the handler's retry loop makes before the test ends it. */
const BLOCKED_ATTEMPTS = 3;

function gitOut(tree: string, args: string[]): string {
  return execFileSync("git", ["-C", tree, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
}

function trackingSha(iid: number): string | null {
  try {
    return gitOut(git.barePathFor(fx.originPath), ["rev-parse", "--verify", `refs/uzi-runner/agent/issue-${iid}`]);
  } catch {
    return null;
  }
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

/** Spy client.publishCheckpoint: every call is a landed publish, counted. */
function spyPublish(): { calls: () => number } {
  let n = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, _tip: string, pack: Readable) => {
    await drain(pack);
    n += 1;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return { calls: () => n };
}

/** An enabled recovery coordinator stand-in counting the credentialed capture and the release. */
function fakeRecovery(): { recovery: RecoveryCoordinator; captures: () => number; releases: () => number } {
  let captures = 0;
  let releases = 0;
  const recovery = {
    enabled: true,
    pin: async (input: { runId: string; sourceSha: string; branch: string; generation?: number }) => ({
      version: 1,
      runId: input.runId,
      captureId: "cap-1826",
      sourceSha: input.sourceSha,
      kind: "issue",
      branch: input.branch,
      generation: input.generation,
    }),
    captureAndUpload: async () => {
      captures += 1;
      return { state: "needs_action", captureId: "cap-1826", reason: "test" };
    },
    release: async () => {
      releases += 1;
    },
    inventoryHolds: async () => [],
    inspect: async () => [],
    resumePending: async () => undefined,
  };
  return { recovery: recovery as unknown as RecoveryCoordinator, captures: () => captures, releases: () => releases };
}

/**
 * A quiescer answering `state` at every proof. A blocked recovery capture retries forever while
 * the run's ownership reads `running` (the retain-and-retry posture); after BLOCKED_ATTEMPTS
 * capture proofs it flips the ownership probe to `paused` (an owner action while the worker
 * retries), which ends handleRecoveryExhausted's loop non-parked with the preserve flags still set.
 */
function quiescer(state: ProcessQuiescenceState, runId: string, order: string[]) {
  const calls: QuiesceRunRequest[] = [];
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    calls.push(req);
    order.push(`proof:${req.site ?? "?"}`);
    if (state !== "quiescent" && calls.filter((c) => c.site === "recovery_capture").length >= BLOCKED_ATTEMPTS) {
      api.setOwnershipStatus(runId, "paused");
    }
    return {
      process: {
        state,
        processes:
          state === "quiescent" ? [] : [{ pid: 4242, uid: 10002, comm: "go", cwd: req.targetPaths[0]!, reason: "own_attempt:kill_unconfirmed" }],
        killed: [],
        detail: `scripted ${state}`,
      },
      docker: { state: "not_wired", removed: [], detail: "" },
    };
  };
  return { calls, quiesceRun };
}

interface Seen {
  clone: string;
}

/** An executor that commits work, leaves an UNCOMMITTED file (so a capture would commit a
 *  `wip(park):` marker), then throws the cache cap's preventive disk park mid-run. */
function diskParkFactory(seen: Seen, order: string[]): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      killAgentTree: () => {
        order.push("group-reap");
      },
      reapAttributedProcesses: async () => {
        order.push("attributed-reap");
      },
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        seen.clone = ctx.worktreePath;
        fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "work\n");
        gitOut(ctx.worktreePath, ["add", "WORK.txt"]);
        gitOut(ctx.worktreePath, [...IDENT, "commit", "-m", "work"]);
        fs.writeFileSync(path.join(ctx.worktreePath, "UNCOMMITTED.txt"), "in flight\n");
        throw new DiskParkSignal(true);
      },
    },
  });
}

/** Count the private PAT-bearing overlay build and the credentialed custody settles. */
function instrument(runner: RunRunner): { overlays: () => number; settles: () => number } {
  const r = runner as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  let overlays = 0;
  let settles = 0;
  const overlay = r.buildCheckpointOverlay!.bind(runner);
  r.buildCheckpointOverlay = async (...a) => {
    overlays++;
    return await overlay(...a);
  };
  for (const name of ["settleRecoveryGeneration", "reapThenSettleRecoveryGeneration"]) {
    const orig = r[name]!.bind(runner);
    r[name] = async (...a) => {
      settles++;
      return await orig(...a);
    };
  }
  return { overlays: () => overlays, settles: () => settles };
}

const statesOf = (runId: string): StateRequest[] => api.states.filter((s) => s.runId === runId).map((s) => s.body);

async function runDiskPark(iid: number, state: ProcessQuiescenceState) {
  client.protocolFeatures = [FEATURE, FENCE];
  const { gitlab, calls: mrCalls } = fakeGitlab();
  const claim = gitlabClaim(iid, { claim_generation: 3 });
  const order: string[] = [];
  const seen: Seen = { clone: "" };
  const pub = spyPublish();
  const { recovery, captures, releases } = fakeRecovery();
  const { calls, quiesceRun } = quiescer(state, claim.run_id, order);
  const runner = runnerWith(diskParkFactory(seen, order), gitlab, undefined, nullLogger(), { ...RUNNER_OPTS, quiesceRun, recovery });
  const spy = instrument(runner);
  await runner.execute(claim);
  return { claim, order, seen, pub, captures, releases, calls, spy, mrCalls };
}

describe("issue #1783 x PRD #1826: a mid-run data_volume_full park whose clone is not provably quiescent", () => {
  for (const state of ["survivors", "unverified"] as const) {
    it(`${state}: the attributed reap ran, yet nothing is captured or published and custody plus the clone are kept`, async () => {
      const iid = 1826 + (state === "survivors" ? 0 : 1);
      const { claim, order, seen, pub, captures, releases, calls, spy, mrCalls } = await runDiskPark(iid, state);

      // #1826's reap ran (and "succeeded"), BEFORE the capture's proof: it does not substitute for it.
      const reap = order.indexOf("attributed-reap");
      const firstProof = order.indexOf("proof:recovery_capture");
      assert.ok(reap > 0 && order[reap - 1] === "group-reap", `attributed reap after the group reap: ${JSON.stringify(order)}`);
      assert.ok(firstProof > reap, `the capture's proof runs after the reap and still blocks: ${JSON.stringify(order)}`);
      const captureProofs = calls.filter((c) => c.site === "recovery_capture");
      assert.equal(captureProofs.length, BLOCKED_ATTEMPTS, "every capture attempt was proof-gated and blocked (retain-and-retry)");
      assert.ok(captureProofs.every((c) => c.mode === "own"));

      // No capture: no wip(park): marker, the uncommitted file still uncommitted, no fetch-back.
      assert.equal(gitOut(seen.clone, ["log", "-1", "--format=%s"]), "work", "no wip(park): marker was committed");
      assert.match(gitOut(seen.clone, ["status", "--porcelain"]), /UNCOMMITTED\.txt/, "the in-flight file is left uncommitted");
      assert.equal(trackingSha(iid), null, "no fetch-back: the tracking ref is untouched");
      // The capture stopped at its first proof: its post-marker re-proof never ran.
      assert.equal(
        calls.filter((c) => c.site !== "recovery_capture").length,
        0,
        `no re-proof ran: the capture stopped at its first proof (${calls.map((c) => c.site).join(",")})`,
      );
      // No publish and no PAT-bearing overlay.
      assert.equal(pub.calls(), 0, "no checkpoint published");
      assert.equal(spy.overlays(), 0, "the credentialed checkpoint overlay was never built");

      // Custody protected: no settle, no credentialed capture, no hold released, never parked or failed.
      assert.equal(spy.settles(), 0, "no custody settle ran");
      assert.equal(captures(), 0, "no credentialed recovery capture ran");
      assert.equal(releases(), 0, "no recovery hold was released");
      const st = statesOf(claim.run_id).map((s) => String(s.status));
      assert.ok(!st.includes("recovery_wait"), `no park was reported over an uncaptured clone: ${st.join(",")}`);
      assert.ok(!st.includes("failed") && !st.includes("completed"), `the run is left non-terminal: ${st.join(",")}`);
      assert.equal(mrCalls.length, 0, "no MR was opened");

      // The clone (preserveRecoveryClone) survives the flight's finally, work and all.
      assert.ok(fs.existsSync(path.join(seen.clone, ".git")), "the runner clone is kept");
      assert.ok(fs.existsSync(path.join(seen.clone, "WORK.txt")), "the committed work is on disk");
      assert.ok(fs.existsSync(path.join(seen.clone, "UNCOMMITTED.txt")), "the uncommitted work is on disk");
    });
  }

  it("quiescent (control): the same park captures the wip marker, fetches back, publishes and parks with custody kept", async () => {
    const iid = 1828;
    const { claim, order, seen, pub, captures, releases, calls, spy } = await runDiskPark(iid, "quiescent");

    assert.ok(order.includes("attributed-reap"), "the attributed reap ran");
    assert.ok(calls.some((c) => c.site === "recovery_capture"), "the capture's proof ran and passed");
    // The verified capture fetched a wip(park): marker (holding the uncommitted file) into the
    // tracking ref, which is what lets the finally retire the clone (the negative cases keep it).
    const bare = git.barePathFor(fx.originPath);
    const tip = trackingSha(iid);
    assert.ok(tip, "the capture fetched back into the tracking ref");
    assert.match(gitOut(bare, ["log", "-1", "--format=%s", tip]), /^wip\(park\):/, "a wip(park): marker was committed");
    assert.match(gitOut(bare, ["ls-tree", "--name-only", tip]), /UNCOMMITTED\.txt/, "the marker captured the uncommitted file");
    assert.equal(fs.existsSync(seen.clone), false, "a verified capture releases the clone to normal retirement");
    assert.ok(pub.calls() >= 1, "the checkpoint was published");
    assert.ok(spy.overlays() >= 1, "the checkpoint overlay was built");

    const parks = statesOf(claim.run_id).filter((s) => s.status === "recovery_wait");
    assert.equal(parks.length, 1, "parked once");
    assert.equal(parks[0]!.recovery_cause, "data_volume_full");
    assert.equal(parks[0]!.disk_park_preventive, true);
    assert.ok(!statesOf(claim.run_id).some((s) => s.status === "failed"), "never a failure");
    // The disk park keeps custody even when the capture ran (PRD #1809 N2).
    assert.equal(spy.settles(), 0);
    assert.equal(captures(), 0);
    assert.equal(releases(), 0);
  });
});
