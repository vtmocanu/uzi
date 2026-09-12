import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, RunContext } from "../src/executor.js";
import {
  api,
  fakeGitlab,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// PRD #1226 M4 (D3/D6): the Runner's enterCompletionHold seam, driven end-to-end through the real
// runner (clone seed → executor → phasePublish → finally). The injected executor calls
// ctx.enterCompletionHold and reports the boolean it returned; the git CAPTURE calls
// (worktreeStatus / fetchAgentBranch / verifyRunnerTrackingCovers / trackingTip / checkpointPack)
// are stubbed on the shared GitCache AFTER the runner has already seeded the clone, so the capture
// verdict is deterministic without a live checkpoint endpoint. The FakeApi completion/hold route
// answers a configurable {run:{status}} so the paused/refused ACK contract is provable.

const CAPTURED_HEAD = "cafef00dcafef00dcafef00dcafef00dcafef00d";

interface HoldProbe {
  executor: Executor;
  held: { value: boolean | undefined };
  /** PRD #1225 (CodeRabbit !1254): number of verifyRunnerTrackingCovers calls = capture attempts the
   *  bounded retry loop actually ran. Lets a test pin the retry count against a "shrink attempts to 1"
   *  or "drop the try/catch" mutant. */
  captureAttempts: { count: number };
}

// makeHoldExecutor returns an executor whose run() stubs the capture git to the given verdict, calls
// ctx.enterCompletionHold, and — modelling the sdk-executor call sites — latches completionHeld on a
// true return (the hold was entered) or THROWS the legacy terminal error on a false return (kept
// live, falls back to the legacy throw). It records the boolean for the test.
//
// `verdict` is either a single constant verdict (used by every attempt) or a SCRIPT of per-attempt
// verdicts (`"throw"` makes verifyRunnerTrackingCovers throw that attempt, a boolean makes it
// return that value). The script drives enterCompletionHold's bounded capture-retry loop across a
// throw, a false, and a true so the loop — not a single constant verdict — is exercised.
function makeHoldExecutor(verdict: boolean | Array<"throw" | boolean>): HoldProbe {
  const held: { value: boolean | undefined } = { value: undefined };
  const captureAttempts = { count: 0 };
  const executor: Executor = {
    run: async (ctx: RunContext) => {
      // Stub the capture git deterministically (the runner already seeded the real clone).
      git.worktreeStatus = (async () => []) as typeof git.worktreeStatus; // clean tree
      git.fetchAgentBranch = (async () =>
        `refs/uzi-runner/${ctx.branch}`) as typeof git.fetchAgentBranch;
      git.verifyRunnerTrackingCovers = (async () => {
        // Count every attempt that reaches the positive-verify step (one per capture attempt).
        const i = captureAttempts.count++;
        const step = Array.isArray(verdict)
          ? verdict[Math.min(i, verdict.length - 1)]!
          : verdict;
        if (step === "throw") throw new Error("capture attempt failed (scripted throw)");
        return step;
      }) as typeof git.verifyRunnerTrackingCovers;
      git.trackingTip = (async () => CAPTURED_HEAD) as typeof git.trackingTip;
      git.checkpointPack = (async () => null) as typeof git.checkpointPack; // publish is a no-op

      const entered = await ctx.enterCompletionHold!("test completion hold reason");
      held.value = entered;
      if (entered) return { branch: ctx.branch, completionHeld: { reason: "test completion hold reason" } };
      // A false return means keep-live-then-legacy-throw at the real call sites.
      throw new Error("legacy terminal (kept live, could not hold)");
    },
  };
  return { executor, held, captureAttempts };
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

/** A pre-created run HOME with a sentinel; if the run preserves HOME the sentinel survives. */
function seedHome(): { homeDir: string; sentinel: string; root: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-hold-home-"));
  const homeDir = path.join(root, "h");
  fs.mkdirSync(homeDir, { recursive: true });
  const sentinel = path.join(homeDir, "SESSION_TRANSCRIPT");
  fs.writeFileSync(sentinel, "resumable session state\n");
  return { homeDir, sentinel, root };
}

describe("RunRunner — recoverable completion hold (PRD #1226 M4 D6)", () => {
  it("verified capture + paused ACK → held: requests the hold with the captured head, preserves the clone and HOME", async () => {
    const { gitlab, calls } = fakeGitlab();
    const { executor, held } = makeHoldExecutor(true);
    const home = seedHome();
    const claim = gitlabClaim(1250);
    try {
      api.setCompletionHoldResponse("paused", 200);
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.strictEqual(held.value, true, "enterCompletionHold entered the verified hold");
      // The hold was requested with the captured tracking tip H.
      assert.strictEqual(api.completionHoldRequests.length, 1, "the hold was requested exactly once");
      assert.strictEqual(api.completionHoldRequests[0]!.runId, claim.run_id);
      assert.strictEqual(api.completionHoldRequests[0]!.body.head, CAPTURED_HEAD, "the captured head H rode the hold request");
      // A hold is non-terminal: no completed/failed report, and NO push/MR.
      assert.ok(!statuses(claim.run_id).includes("completed"), "a held run is not completed");
      assert.ok(!statuses(claim.run_id).includes("failed"), "a held run is not failed");
      assert.strictEqual(calls.length, 0, "a held run opens no MR");
      // The destructive cleanup did NOT run: the clone and the HOME are preserved for resume.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1250)), true, "the runner clone is preserved");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the run HOME (SDK session) is preserved");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("capture retries across throw→false→true: holds on the third attempt, preserving the clone and HOME (bounded retry loop)", async () => {
    // The scripted verdict drives enterCompletionHold's bounded capture-retry loop: attempt 0 throws
    // (the try/catch retains and retries), attempt 1 returns false (unverified ⇒ retry), attempt 2
    // returns true (verified ⇒ capture, then request the hold). A "shrink attempts to 1" or "drop the
    // try/catch" mutant would abort on the throw/false attempts before ever reaching the true verdict,
    // so held.value would be false and no hold would be requested.
    const { gitlab, calls } = fakeGitlab();
    const { executor, held, captureAttempts } = makeHoldExecutor(["throw", false, true]);
    const home = seedHome();
    const claim = gitlabClaim(1253);
    try {
      api.setCompletionHoldResponse("paused", 200);
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.strictEqual(captureAttempts.count, 3, "the bounded retry loop ran all three capture attempts (throw, false, true)");
      assert.strictEqual(held.value, true, "the third, verified attempt entered the hold");
      // The hold was requested exactly once, only after the retries produced a verified capture.
      assert.strictEqual(api.completionHoldRequests.length, 1, "the hold was requested exactly once, after the retries");
      assert.strictEqual(api.completionHoldRequests[0]!.runId, claim.run_id);
      assert.strictEqual(api.completionHoldRequests[0]!.body.head, CAPTURED_HEAD, "the captured head H rode the hold request");
      // A hold is non-terminal: no completed/failed report, and NO push/MR.
      assert.ok(!statuses(claim.run_id).includes("completed"), "a held run is not completed");
      assert.ok(!statuses(claim.run_id).includes("failed"), "a held run is not failed");
      assert.strictEqual(calls.length, 0, "a held run opens no MR");
      // The destructive cleanup did NOT run: the clone and the HOME are preserved for resume.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1253)), true, "the runner clone is preserved");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the run HOME (SDK session) is preserved");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("refused hold ACK (non-paused) → false: does NOT park, and clears the preserve flags so the run's normal terminal cleanup removes the clone and HOME", async () => {
    const { gitlab } = fakeGitlab();
    const { executor, held } = makeHoldExecutor(true);
    const home = seedHome();
    const claim = gitlabClaim(1251);
    try {
      // The server refuses the hold: it answers running (a 409-style refusal reads its status off the
      // body too). enterCompletionHold must return false. The retain-everything flags are held ONLY
      // while the hold is being attempted; on a refusal it is NOT parked, so it clears them and the
      // failing run (the executor's legacy throw) follows normal terminal cleanup — otherwise a run
      // that attempts a hold, is kept live, then FAILS would leak its clone + HOME.
      api.setCompletionHoldResponse("running", 409);
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.strictEqual(held.value, false, "a non-paused ACK ⇒ enterCompletionHold returns false");
      assert.strictEqual(api.completionHoldRequests.length, 1, "the hold WAS requested (capture verified)");
      // Not parked ⇒ the flags were cleared ⇒ the terminal (failed) run cleaned up.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1251)), false, "the clone is cleaned up on a refused ACK (not parked)");
      assert.strictEqual(fs.existsSync(home.sentinel), false, "the HOME is cleaned up on a refused ACK (not parked)");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("unverified capture → false: never requests the hold, and clears the preserve flags so the run's normal terminal cleanup removes the clone and HOME", async () => {
    const { gitlab } = fakeGitlab();
    const { executor, held } = makeHoldExecutor(false); // verifyRunnerTrackingCovers → false
    const home = seedHome();
    const claim = gitlabClaim(1252);
    try {
      api.setCompletionHoldResponse("paused", 200); // would grant, but must never be reached
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.strictEqual(held.value, false, "an unverified capture ⇒ enterCompletionHold returns false");
      assert.strictEqual(
        api.completionHoldRequests.length,
        0,
        "the hold is NEVER requested before a positively-verified capture",
      );
      // Giving up on an unverified capture is a NOT-parked outcome: the flags are cleared and the
      // failing run cleans up (the AC's no-clean-while-uncertain protection covers the in-flight
      // retries, not the terminal outcome once the bounded attempts are exhausted).
      assert.strictEqual(fs.existsSync(worktreeDirFor(1252)), false, "the clone is cleaned up on an unverified capture (not parked)");
      assert.strictEqual(fs.existsSync(home.sentinel), false, "the HOME is cleaned up on an unverified capture (not parked)");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });
});
