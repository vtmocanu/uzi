import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import type { StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

// issue #1783 M3 — the recovery-exhausted loop must not retry a BLOCKED capture proof forever.
// Before the fix, handleRecoveryExhausted retried captureRecoveryRestorePoint for as long as the
// run's ownership read `running`: a quiescence proof blocked by an unreadable, unattributed
// runner-uid process never clears by waiting, so the claim was held forever and a test driving it
// never ended (CI ran for hours). Now RECOVERY_CAPTURE_BLOCKED_ATTEMPTS (5) consecutive blocked
// proofs fail the run with fail_origin worker_residue_blocked, naming the pid and comm, with the
// clone and session kept.
//
// Safety valve: on UNFIXED code the loop would never end, so the quiescer flips the run's ownership
// to `paused` after SAFETY_VALVE capture proofs, which ends the unfixed loop non-parked; the test
// then FAILS on the call-count and fail-origin assertions instead of hanging. The explicit per-test
// timeout is a second guard.

installHarness();

const CAP = 5;
const SAFETY_VALVE = 50;
const PID = 4242;
const COMM = "ssh-agent";
const TIMEOUT = { timeout: 60_000 };

/** A quiesceRun that always answers `unverified` with one unreadable, unattributed process. */
function blockedQuiescer(runId: string) {
  const sites: string[] = [];
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    sites.push(req.site ?? "?");
    if (sites.filter((s) => s === "recovery_capture").length >= SAFETY_VALVE) api.setOwnershipStatus(runId, "paused");
    return {
      process: {
        state: "unverified",
        processes: [{ pid: PID, uid: 10002, comm: COMM, cwd: "unreadable", reason: "unreadable_unattributed" }],
        killed: [],
        detail: `runner-uid pid ${PID} (${COMM}) could not be attributed (env/cwd unreadable)`,
      },
      docker: { state: "not_wired", removed: [], detail: "" },
    };
  };
  return { quiesceRun, captureProofs: () => sites.filter((s) => s === "recovery_capture").length };
}

function factory(signal: () => Error, seen: { clone: string; home: string }): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        seen.clone = ctx.worktreePath;
        seen.home = path.join(homeDir, runId);
        fs.mkdirSync(seen.home, { recursive: true });
        fs.writeFileSync(path.join(seen.home, "session"), "transcript");
        fs.writeFileSync(path.join(ctx.worktreePath, "ONLY_COPY.txt"), "uncommitted work\n");
        throw signal();
      },
    },
  });
}

const statesOf = (runId: string): StateRequest[] => api.states.filter((s) => s.runId === runId).map((s) => s.body);

const CASES: Array<{ name: string; iid: number; signal: () => Error; features?: string[] }> = [
  { name: "transient recovery", iid: 1783_01, signal: () => new TransientRecoveryError() },
  {
    name: "mid-run data_volume_full park",
    iid: 1783_02,
    signal: () => new DiskParkSignal(true),
    features: ["recovery_cause_data_volume_full", "claim_generation_fence"],
  },
];

describe("issue #1783 M3: a recovery capture whose proof keeps blocking is bounded", () => {
  for (const c of CASES) {
    it(`${c.name}: fails worker_residue_blocked after ${CAP} blocked proofs, clone and session kept`, TIMEOUT, async () => {
      if (c.features) client.protocolFeatures = c.features;
      const { gitlab, calls: mrCalls } = fakeGitlab();
      const claim = gitlabClaim(c.iid, { claim_generation: 3 });
      const q = blockedQuiescer(claim.run_id);
      const seen = { clone: "", home: "" };
      await runnerWith(factory(c.signal, seen), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
        recoveryRetryMs: 1,
        quiesceRun: q.quiesceRun,
      }).execute(claim);

      assert.ok(q.captureProofs() < SAFETY_VALVE, `the safety valve fired: the loop never stopped by itself (${q.captureProofs()} proofs)`);
      assert.equal(q.captureProofs(), CAP, "exactly the cap of blocked capture proofs ran");
      const states = statesOf(claim.run_id);
      const failed = states.filter((s) => s.status === "failed");
      assert.equal(failed.length, 1, `one failure report: ${states.map((s) => s.status).join(",")}`);
      assert.equal(failed[0]!.fail_origin, "worker_residue_blocked");
      const reason = String(failed[0]!.failure_reason);
      assert.match(reason, /^worker_residue_blocked: /);
      assert.ok(reason.includes(`pid ${PID} (${COMM})`), `the reason names the pid and comm: ${reason}`);
      assert.ok(reason.includes("could not be attributed"), reason);
      assert.ok(!states.some((s) => s.status === "recovery_wait"), "never parked over an uncaptured clone");
      assert.equal(mrCalls.length, 0);
      assert.ok(fs.existsSync(path.join(seen.clone, ".git")), "the clone is kept");
      assert.ok(fs.existsSync(path.join(seen.clone, "ONLY_COPY.txt")), "the uncommitted work is kept");
      assert.ok(fs.existsSync(path.join(seen.home, "session")), "the session is kept");
    });
  }

  it("control: a blocked proof that clears before the cap still parks (the count is of CONSECUTIVE blocks)", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1783_03);
    let captureProofs = 0;
    const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
      if (req.site === "recovery_capture") captureProofs++;
      const blocked = req.site === "recovery_capture" && captureProofs < CAP;
      return {
        process: blocked
          ? { state: "unverified", processes: [{ pid: PID, uid: 10002, comm: COMM, cwd: "unreadable", reason: "unreadable_unattributed" }], killed: [], detail: "blocked" }
          : { state: "quiescent", processes: [], killed: [], detail: "" },
        docker: { state: "not_wired", removed: [], detail: "" },
      };
    };
    const seen = { clone: "", home: "" };
    await runnerWith(factory(() => new TransientRecoveryError(), seen), gitlab, undefined, nullLogger(), {
      checkpointIntervalMs: 0,
      recoveryRetryMs: 1,
      quiesceRun,
    }).execute(claim);
    assert.equal(captureProofs, CAP, "four blocked proofs, then a passing one");
    const states = statesOf(claim.run_id).map((s) => s.status);
    assert.ok(states.includes("recovery_wait"), `parked once the proof passed: ${states.join(",")}`);
    assert.ok(!states.includes("failed"), states.join(","));
  });
});
