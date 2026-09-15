import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, RunContext } from "../src/executor.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  assistant,
  fakeGitlab,
  git,
  gitlabClaim,
  homeDir,
  input,
  installHarness,
  resultOk,
  runner,
  runnerWith,
  simulateCommittedWork,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// PRD #1247 M5b — the Runner's enterCredentialSwitch release state machine, driven end-to-end
// through the real runner (clone seed → executor throws CredentialSwitchSignal → executeClaim's
// catch arm → enterCredentialSwitch → finally). Modelled on runner-completion-hold.test.ts: the
// injected executor stubs the capture git on the shared GitCache (AFTER the runner has seeded the
// clone) so the capture verdict is deterministic without a live checkpoint endpoint, then throws a
// CredentialSwitchSignal exactly as a live turn's abort would. The FakeApi answers the
// credential_switch state report with `queued` (via overrideStateStatus) so the positive-ack
// release contract is provable.

/** Verdicts for the stubbed capture git — mirrors captureRecoveryRestorePoint's own branches:
 *  - "verified":   clean tree, tracking ref covers HEAD, publish a no-op ⇒ verified + published:false
 *  - "unpublished": clean + verified, but the publish THROWS ⇒ verified (publish best-effort) ⇒ released
 *  - "dirty_fail": dirty tree whose WIP commit FAILS ⇒ never verified ⇒ give up */
type CaptureVerdict = "verified" | "unpublished" | "dirty_fail";

interface SwitchProbe {
  executor: Executor;
  captureAttempts: { count: number };
}

// makeSwitchExecutor returns an executor whose run() stubs the capture git to the given verdict,
// optionally emits a run message (for the drain-ordering test), and then THROWS a
// CredentialSwitchSignal — the exact signal a live turn's abort or a gate-waiter rejection raises,
// which the runner's catch chain routes into enterCredentialSwitch.
function makeSwitchExecutor(
  verdict: CaptureVerdict,
  opts: { emitBeforeThrow?: boolean } = {},
): SwitchProbe {
  const captureAttempts = { count: 0 };
  const executor: Executor = {
    run: async (ctx: RunContext) => {
      // Dirty ⇒ worktreeStatus returns changes and the WIP commit fails (never verified). Clean ⇒
      // empty status and the tracking ref covers HEAD (verified). The runner already seeded the
      // real clone, so these stubs only govern enterCredentialSwitch's capture.
      const dirty = verdict === "dirty_fail";
      git.worktreeStatus = (async () => (dirty ? ["M src/impl.ts"] : [])) as typeof git.worktreeStatus;
      git.commitWipMarker = (async () => !dirty) as typeof git.commitWipMarker; // a dirty commit FAILS
      git.fetchAgentBranch = (async () =>
        `refs/uzi-runner/${ctx.branch}`) as typeof git.fetchAgentBranch;
      git.verifyRunnerTrackingCovers = (async () => {
        captureAttempts.count++;
        return !dirty; // clean ⇒ verified; dirty never reaches here (WIP commit already failed)
      }) as typeof git.verifyRunnerTrackingCovers;
      git.trackingTip = (async () => "cafef00dcafef00dcafef00dcafef00dcafef00d") as typeof git.trackingTip;
      // "unpublished" models a verified-locally-but-publish-FAILED capture: the pack build throws,
      // publishCheckpointBestEffort swallows it (published:false), and the verified capture still
      // releases. "verified" makes the publish a plain no-op (null pack).
      git.checkpointPack = (async () => {
        if (verdict === "unpublished") throw new Error("origin publish failed (best-effort)");
        return null;
      }) as typeof git.checkpointPack;

      if (opts.emitBeforeThrow) {
        // A pending run message the batcher must DRAIN before the release report (the fenced append
        // only persists while claim_released_at IS NULL).
        ctx.emit({ kind: "text", agent: "lead", payload: { text: "work in flight before the switch" } });
      }
      throw new CredentialSwitchSignal();
    },
  };
  return { executor, captureAttempts };
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

/** A pre-created run HOME with a sentinel; if the run preserves HOME the sentinel survives. */
function seedHome(): { homeDir: string; sentinel: string; root: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-switch-home-"));
  const homeDir = path.join(root, "h");
  fs.mkdirSync(homeDir, { recursive: true });
  const sentinel = path.join(homeDir, "SESSION_TRANSCRIPT");
  fs.writeFileSync(sentinel, "resumable session state\n");
  return { homeDir, sentinel, root };
}

describe("RunRunner — held-state credential switch (PRD #1247 M5b)", () => {
  it("verified capture + queued ack → RELEASED: reports credential_switch, retires the clone, preserves HOME", async () => {
    const { gitlab, calls } = fakeGitlab();
    const { executor } = makeSwitchExecutor("verified");
    const home = seedHome();
    const claim = gitlabClaim(1260);
    try {
      // The release requeues the run, so its ack status is `queued` (the positive-ack contract).
      api.overrideStateStatus(claim.run_id, "queued");
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.ok(statuses(claim.run_id).includes("credential_switch"), "the release reported credential_switch");
      assert.ok(!statuses(claim.run_id).includes("completed"), "a released run is not completed");
      assert.ok(!statuses(claim.run_id).includes("failed"), "a released run is not failed");
      assert.ok(!statuses(claim.run_id).includes("credential_switch_failed"), "a released run did NOT give up");
      assert.strictEqual(calls.length, 0, "a released run opens no MR");
      // Released ⇒ the clone is RETIRED (work is durable on the tracking ref + best-effort origin),
      // and the HOME is PRESERVED (parked) for a same-worker resume.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1260)), false, "the clone is retired on release");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the HOME (SDK session) is preserved for resume");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("clean-but-unpublished (verified locally, origin publish failed) → still RELEASED (verified is the gate; publish is best-effort)", async () => {
    const { gitlab } = fakeGitlab();
    const { executor } = makeSwitchExecutor("unpublished");
    const home = seedHome();
    const claim = gitlabClaim(1261);
    try {
      api.overrideStateStatus(claim.run_id, "queued");
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.ok(statuses(claim.run_id).includes("credential_switch"), "a verified-but-unpublished capture still releases");
      assert.ok(!statuses(claim.run_id).includes("credential_switch_failed"), "it did NOT give up on a failed publish");
      assert.strictEqual(fs.existsSync(worktreeDirFor(1261)), false, "the clone is retired on release");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the HOME is preserved for resume");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("dirty capture that never verifies → GIVES UP: reports credential_switch_failed, KEEPS the clone + HOME, makes NO terminal report", async () => {
    const { gitlab } = fakeGitlab();
    const { executor, captureAttempts } = makeSwitchExecutor("dirty_fail");
    const home = seedHome();
    const claim = gitlabClaim(1262);
    try {
      // Would grant `queued`, but the release report must NEVER be reached on a give-up.
      api.overrideStateStatus(claim.run_id, "queued");
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.ok(
        statuses(claim.run_id).includes("credential_switch_failed"),
        "give-up reports credential_switch_failed (server clears the switch stamp, keeps the run)",
      );
      assert.ok(!statuses(claim.run_id).includes("credential_switch"), "the release is NEVER reported before a verified capture");
      assert.ok(!statuses(claim.run_id).includes("completed"), "give-up makes no terminal completed report");
      assert.ok(!statuses(claim.run_id).includes("failed"), "give-up makes no terminal failed report — the run is left for requeue");
      // A dirty WIP commit fails on every attempt, so the capture never reaches the positive verify.
      assert.strictEqual(captureAttempts.count, 0, "the dirty WIP commit fails before the positive-verify step is ever reached");
      // Give-up KEEPS both preserve flags ⇒ the clone AND the HOME survive for the requeue.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1262)), true, "the clone is retained on give-up (no work loss)");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the HOME is retained on give-up");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("drains the batcher BEFORE the credential_switch report (the fenced append persists only while the claim is unreleased)", async () => {
    const { gitlab } = fakeGitlab();
    const { executor } = makeSwitchExecutor("verified", { emitBeforeThrow: true });
    const home = seedHome();
    const claim = gitlabClaim(1263);
    try {
      api.overrideStateStatus(claim.run_id, "queued");
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      const msgAt = api.requestLog.indexOf("messages");
      const releaseAt = api.requestLog.indexOf("state:credential_switch");
      assert.ok(msgAt >= 0, "the pending message batch was delivered");
      assert.ok(releaseAt >= 0, "the credential_switch release was reported");
      assert.ok(msgAt < releaseAt, "the message batch DRAINED before the credential_switch release report");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });
});

// PRD #1247 M5b (D13) — phase restore on a reclaim after a credential switch, driven end-to-end
// through the REAL SdkExecutor (a scripted queryFn), so the phasePlanGate branches are exercised,
// not a stub's mirror. `doneOnlyQuery` yields ONLY an implement/done turn and counts its calls: a
// planning turn (if one wrongly ran) would consume a call and, getting a plan-less done turn, fail
// the run with REASON_NO_PLAN — so `calls === 1` plus a `completed` run proves NO planning turn ran.
function doneOnlyQuery(counter: { calls: number }): SdkQueryFn {
  return (params) => {
    counter.calls++;
    return (async function* () {
      for await (const _ of params.prompt) {
        /* drain */
      }
      yield assistant([
        { type: "text", text: "done implementing" },
        { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} },
      ]);
      yield resultOk();
    })();
  };
}

describe("RunRunner — resume phase after a credential switch (PRD #1247 M5b D13)", () => {
  it('resume_phase="awaiting_approval" re-presents the SAME submitted plan at the gate WITHOUT re-planning', async () => {
    const { gitlab } = fakeGitlab();
    simulateCommittedWork(); // the scripted query commits nothing; model committed work (issue #279)
    const counter = { calls: 0 };
    const resumedPlan = "# RESUMED PLAN\n- finish this on the newly chosen token";
    // A gate resume: the plan was SUBMITTED (plan_md) but NOT approved (plan_approved omitted), so
    // preApproved is false and resumeAtGate governs — the gate is re-presented, no planning turn.
    const claim = gitlabClaim(1270, {
      resume_phase: "awaiting_approval",
      plan_md: resumedPlan,
    });
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: doneOnlyQuery(counter) }),
      gitlab,
    ).execute(claim);

    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    const gate = states.find((s) => s.status === "awaiting_approval");
    assert.ok(gate, "the run re-presented the plan gate (awaiting_approval)");
    assert.strictEqual(
      gate!.plan_md,
      resumedPlan,
      "the gate re-presented the SUBMITTED plan verbatim (from the claim, not a fresh planning turn)",
    );
    assert.strictEqual(counter.calls, 1, "exactly ONE SDK turn ran (the implement turn) — NO planning turn");
    assert.ok(
      states.map((s) => s.status).includes("completed"),
      "the run implemented and completed after the gate approval on the new token",
    );
  });

  it('resume_phase="implementing" skips the gate entirely (the existing preApproved path)', async () => {
    const { gitlab } = fakeGitlab();
    simulateCommittedWork();
    const counter = { calls: 0 };
    // An approved, seeded resume in the implementing phase: planApproved is derived true (seeded),
    // so preApproved skips the gate — resume_phase="implementing" rides alongside and needs no new
    // executor code (this pins that it does NOT accidentally re-gate).
    const claim = gitlabClaim(1271, {
      resume_phase: "implementing",
      plan_approved: true,
      plan_source: "seeded",
      plan_md: "# APPROVED PLAN\n- implement directly",
      session_id: null,
    });
    // No approve_plan input is set: a run that (wrongly) entered the gate would hang/timeout, so a
    // clean completion is itself evidence the gate was skipped.
    await runner(
      new SdkExecutor(nullLogger(), homeDir, { queryFn: doneOnlyQuery(counter) }),
      gitlab,
    ).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.ok(!statuses.includes("awaiting_approval"), "resume_phase=implementing never parks at the gate");
    assert.ok(statuses.includes("completed"), "it implements the approved plan directly and completes");
    assert.strictEqual(counter.calls, 1, "exactly ONE SDK turn ran (the implement turn) — no planning turn");
  });
});
