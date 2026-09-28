import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { CodexCredentialDeferredError } from "../src/codex/codex-executor.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import type { StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

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
        detail: `runner-uid pid ${PID} "${COMM}" could not be attributed (env/cwd unreadable)`,
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

/** The runner's credentialed custody calls (the pre-report provider reap and every custody settle),
 *  spied by name: a vault-lock park must reach none of them. */
function spyCustody(runner: object): string[] {
  const calls: string[] = [];
  const r = runner as Record<string, (...args: unknown[]) => Promise<unknown>>;
  for (const name of ["reapRecoveryProviderForSettle", "reapThenSettleRecoveryGeneration", "settleRecoveryGeneration"] as const) {
    const orig = r[name]!.bind(runner);
    r[name] = async (...args: unknown[]) => {
      calls.push(name);
      return orig(...args);
    };
  }
  return calls;
}

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
      assert.ok(reason.includes(`pid ${PID} "${COMM}"`), `the reason names the pid and comm: ${reason}`);
      assert.ok(reason.includes("could not be attributed"), reason);
      assert.ok(!states.some((s) => s.status === "recovery_wait"), "never parked over an uncaptured clone");
      assert.equal(mrCalls.length, 0);
      assert.ok(fs.existsSync(path.join(seen.clone, ".git")), "the clone is kept");
      assert.ok(fs.existsSync(path.join(seen.clone, "ONLY_COPY.txt")), "the uncommitted work is kept");
      assert.ok(fs.existsSync(path.join(seen.home, "session")), "the session is kept");
    });
  }

  it("control: the count is of CONSECUTIVE blocks: 4 blocked, 1 non-blocked failure, 4 blocked, then a pass parks", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1783_03);
    // Capture proofs 1-4 and 6-9 block; proof 5 passes but its fetch-back fails (an unverified
    // capture that is NOT a blocked proof, so it resets the count); proof 10 passes and verifies.
    // A cumulative count would reach the cap at proof 6 and fail the run.
    const RESET_AT = CAP;
    const PASS_AT = 2 * CAP;
    let captureProofs = 0;
    const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
      if (req.site === "recovery_capture") captureProofs++;
      const blocked = req.site === "recovery_capture" && captureProofs !== RESET_AT && captureProofs < PASS_AT;
      return {
        process: blocked
          ? { state: "unverified", processes: [{ pid: PID, uid: 10002, comm: COMM, cwd: "unreadable", reason: "unreadable_unattributed" }], killed: [], detail: "blocked" }
          : { state: "quiescent", processes: [], killed: [], detail: "" },
        docker: { state: "not_wired", removed: [], detail: "" },
      };
    };
    const realFetch = git.fetchAgentBranch.bind(git);
    let failedFetches = 0;
    git.fetchAgentBranch = (async (...a: Parameters<typeof git.fetchAgentBranch>) => {
      if (captureProofs === RESET_AT) {
        failedFetches++;
        throw new Error("injected recovery-capture fetch-back failure");
      }
      return realFetch(...a);
    }) as typeof git.fetchAgentBranch;
    try {
      const seen = { clone: "", home: "" };
      await runnerWith(factory(() => new TransientRecoveryError(), seen), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
        recoveryRetryMs: 1,
        quiesceRun,
      }).execute(claim);
    } finally {
      git.fetchAgentBranch = realFetch;
    }
    assert.equal(failedFetches, 1, "the non-blocked capture failure ran once, at proof 5");
    assert.equal(captureProofs, PASS_AT, "4 blocked, 1 non-blocked failure, 4 blocked, then a passing proof");
    const states = statesOf(claim.run_id).map((s) => s.status);
    assert.ok(states.includes("recovery_wait"), `parked once the proof passed: ${states.join(",")}`);
    assert.ok(!states.includes("failed"), states.join(","));
  });

  it("a shutdown arriving during the cap-reaching blocked capture wins: no failed report, clone and session kept", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1783_04);
    const q = blockedQuiescer(claim.run_id);
    let runner: ReturnType<typeof runnerWith> | undefined;
    const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
      const out = await q.quiesceRun(req);
      if (req.site === "recovery_capture" && q.captureProofs() === CAP) runner!.shutdown();
      return out;
    };
    const seen = { clone: "", home: "" };
    runner = runnerWith(factory(() => new TransientRecoveryError(), seen), gitlab, undefined, nullLogger(), {
      checkpointIntervalMs: 0,
      recoveryRetryMs: 1,
      quiesceRun,
    });
    await runner.execute(claim);
    assert.equal(q.captureProofs(), CAP, "the shutdown landed during the cap-reaching capture proof");
    const states = statesOf(claim.run_id);
    assert.ok(!states.some((s) => s.status === "failed"), `no terminal failure on shutdown: ${states.map((s) => s.status).join(",")}`);
    assert.ok(!states.some((s) => s.fail_origin === "worker_residue_blocked"));
    assert.ok(fs.existsSync(path.join(seen.clone, "ONLY_COPY.txt")), "the uncommitted work is kept");
    assert.ok(fs.existsSync(path.join(seen.home, "session")), "the session is kept");
  });

  it("a cancel arriving during the cap-reaching blocked capture takes the cancel path, not worker_residue_blocked", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1783_05);
    const q = blockedQuiescer(claim.run_id);
    let runSignal: AbortSignal | undefined;
    const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
      const out = await q.quiesceRun(req);
      if (req.site === "recovery_capture" && q.captureProofs() === CAP) {
        api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
        // The steering poller sets its sticky cancel BEFORE it aborts the run's signal.
        const until = Date.now() + 10_000;
        while (!runSignal?.aborted && Date.now() < until) await new Promise((r) => setTimeout(r, 5));
        assert.ok(runSignal?.aborted, "the cancel reached the run's steering");
      }
      return out;
    };
    const seen = { clone: "", home: "" };
    const inner = factory(() => new TransientRecoveryError(), seen);
    const withSignal: ExecutorFactory = (runId, ...rest) => {
      const made = inner(runId, ...rest);
      const run = made.executor.run.bind(made.executor);
      return {
        ...made,
        executor: {
          ...made.executor,
          run: async (ctx: RunContext) => {
            runSignal = ctx.signal;
            return run(ctx);
          },
        },
      };
    };
    await runnerWith(withSignal, gitlab, undefined, nullLogger(), {
      checkpointIntervalMs: 0,
      recoveryRetryMs: 1,
      quiesceRun,
    }).execute(claim);
    assert.equal(q.captureProofs(), CAP, "the cancel landed during the cap-reaching capture proof");
    const failed = statesOf(claim.run_id).filter((s) => s.status === "failed");
    assert.equal(failed.length, 1, `one terminal report: ${JSON.stringify(failed)}`);
    assert.equal(failed[0]!.failure_reason, "run cancelled");
    assert.notEqual(failed[0]!.fail_origin, "worker_residue_blocked");
  });

  it("vault_locked park: fails worker_residue_blocked after the cap, custody kept (no credentialed reap or settle), clone and session kept", TIMEOUT, async () => {
    client.protocolFeatures = ["recovery_cause_vault_locked"];
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const claim = gitlabClaim(1783_06, { claim_generation: 3 });
    const q = blockedQuiescer(claim.run_id);
    const seen = { clone: "", home: "" };
    const runner = runnerWith(factory(() => new CodexCredentialDeferredError(), seen), gitlab, undefined, nullLogger(), {
      checkpointIntervalMs: 0,
      recoveryRetryMs: 1,
      quiesceRun: q.quiesceRun,
    });
    const custodyCalls = spyCustody(runner);
    await runner.execute(claim);

    assert.ok(q.captureProofs() < SAFETY_VALVE, `the safety valve fired (${q.captureProofs()} proofs)`);
    assert.equal(q.captureProofs(), CAP, "exactly the cap of blocked capture proofs ran");
    const states = statesOf(claim.run_id);
    const failed = states.filter((s) => s.status === "failed");
    assert.equal(failed.length, 1, `one failure report: ${states.map((s) => s.status).join(",")}`);
    assert.equal(failed[0]!.fail_origin, "worker_residue_blocked");
    assert.ok(String(failed[0]!.failure_reason).includes(`pid ${PID} "${COMM}"`), String(failed[0]!.failure_reason));
    assert.ok(!states.some((s) => s.status === "recovery_wait"), "never parked over an uncaptured clone");
    assert.deepEqual(custodyCalls, [], "custody kept: no credentialed pre-report reap and no custody settle");
    assert.equal(mrCalls.length, 0);
    assert.ok(fs.existsSync(path.join(seen.clone, ".git")), "the clone is kept");
    assert.ok(fs.existsSync(path.join(seen.clone, "ONLY_COPY.txt")), "the uncommitted work is kept");
    assert.ok(fs.existsSync(path.join(seen.home, "session")), "the session is kept");
  });

  it("vault_locked park: the bound's report keeps custody even when a terminal outcome already resolved", TIMEOUT, async () => {
    // On the ordinary path a RunResidueBlockedError already skips reportGenericFailure's reap and
    // settle, so `keepCustody` matters only where the outcome was already journaled or resolved
    // (the permanent-failure hook's write-ahead `failed`): that arm runs a credentialed
    // reap-then-settle unless custody is kept. Seam: mark the flight's terminal as resolved as the
    // bound calls reportGenericFailure, and require that no custody call follows.
    client.protocolFeatures = ["recovery_cause_vault_locked"];
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1783_07, { claim_generation: 3 });
    const q = blockedQuiescer(claim.run_id);
    const seen = { clone: "", home: "" };
    const runner = runnerWith(factory(() => new CodexCredentialDeferredError(), seen), gitlab, undefined, nullLogger(), {
      checkpointIntervalMs: 0,
      recoveryRetryMs: 1,
      quiesceRun: q.quiesceRun,
    });
    const r = runner as unknown as Record<string, (...args: unknown[]) => Promise<unknown>>;
    const report = r.reportGenericFailure!.bind(runner);
    let bounded = 0;
    r.reportGenericFailure = async (...args: unknown[]) => {
      if (args[2] instanceof Error && args[2].name === "RunResidueBlockedError") {
        bounded += 1;
        (args[1] as { terminalResolved?: boolean }).terminalResolved = true;
      }
      return report(...args);
    };
    const custodyCalls = spyCustody(runner);
    await runner.execute(claim);

    assert.equal(q.captureProofs(), CAP, "exactly the cap of blocked capture proofs ran");
    assert.equal(bounded, 1, "the bound reported the residue-blocked failure once");
    assert.deepEqual(custodyCalls, [], "custody kept: no credentialed reap or settle after the resolved terminal");
    assert.ok(fs.existsSync(path.join(seen.clone, "ONLY_COPY.txt")), "the uncommitted work is kept");
    assert.ok(fs.existsSync(path.join(seen.home, "session")), "the session is kept");
  });
});
