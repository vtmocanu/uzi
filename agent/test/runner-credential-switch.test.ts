import { mockCommittedTracking } from "./runner-tracking-fixture.js";
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import type { PlanSummaryInput, SummaryRunner } from "../src/summary-runner.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  assistant,
  client,
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
  planWithMilestonesThenDoneQuery,
} from "./runner-harness.js";
import { freshClaim, newApi, row, send, until } from "./gate-revision-harness.js";

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
      mockCommittedTracking(git, "cafef00dcafef00dcafef00dcafef00dcafef00d");
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

// In-place-switch verdicts (the ctx.attemptCredentialSwitch path) model the WIP-MARKER lifecycle so the
// data-integrity fix is observable — a give-up that continues in place MUST undo a marker it committed
// (else the throwaway commit rides into the MR), while a release LEAVES it for the reclaim's reseed:
//  - "verified":       CLEAN tree, NO marker committed (status.length===0 skips commitWipMarker),
//                      verified ⇒ RELEASED — the switchReleased finalize short-circuit.
//  - "marker_giveup":  DIRTY tree, marker committed (commitWipMarker→true, so HEAD IS a marker), never
//                      verified ⇒ GIVE UP; the marker is at HEAD and MUST be undone in place.
//  - "marker_release": DIRTY tree, marker committed, verified ⇒ RELEASED; the marker is LEFT at HEAD (the
//                      reclaim reseeds and reset-softs it) — undoWipMarker must NOT fire.
type InPlaceVerdict = "verified" | "marker_giveup" | "marker_release";

interface InPlaceSwitchProbe {
  executor: Executor;
  captureAttempts: { count: number };
  // Observable WIP-marker state the git double tracks: whether HEAD is currently a marker, and whether
  // the runner undid it (undoWipMarker called on the continue-in-place path).
  marker: { headIsMarker: boolean; undoCalled: boolean };
}

// makeInPlaceSwitchExecutor models the DATA-INTEGRITY fix (PRD #1247 M5b): the executor handles the
// switch IN PLACE by CALLING ctx.attemptCredentialSwitch (which the runner wires to
// enterCredentialSwitch) instead of throwing to the outer catch, then acts on the outcome exactly as the
// real loop does: "released" → return switchReleased so phasePublish skips finalize; "gave_up" → the run
// CONTINUES on the old token, here modelled as a later NORMAL (report-only) completion that cleans up as
// usual. Unlike makeSwitchExecutor's "dirty_fail" (where the WIP commit FAILS so no marker exists), the
// DIRTY verdicts here let the WIP commit SUCCEED — so headIsWipMarker sees a real marker at HEAD and the
// give-up's undo (and the release's non-undo) can be asserted.
function makeInPlaceSwitchExecutor(verdict: InPlaceVerdict): InPlaceSwitchProbe {
  const captureAttempts = { count: 0 };
  const marker = { headIsMarker: false, undoCalled: false };
  const dirty = verdict === "marker_giveup" || verdict === "marker_release";
  const verifies = verdict === "verified" || verdict === "marker_release";
  const executor: Executor = {
    run: async (ctx: RunContext) => {
      git.worktreeStatus = (async () => (dirty ? ["M src/impl.ts"] : [])) as typeof git.worktreeStatus;
      // A DIRTY tree's WIP commit SUCCEEDS here — the marker IS committed, so headIsWipMarker sees it at
      // HEAD. A CLEAN tree skips commitWipMarker entirely (captureRecoveryRestorePoint's status.length===0
      // branch), so no marker is ever planted.
      git.commitWipMarker = (async () => {
        marker.headIsMarker = true;
        return true;
      }) as typeof git.commitWipMarker;
      mockCommittedTracking(git, "cafef00dcafef00dcafef00dcafef00dcafef00d");
      git.verifyRunnerTrackingCovers = (async () => {
        captureAttempts.count++;
        return verifies;
      }) as typeof git.verifyRunnerTrackingCovers;
      git.trackingTip = (async () => "cafef00dcafef00dcafef00dcafef00dcafef00d") as typeof git.trackingTip;
      git.checkpointPack = (async () => null) as typeof git.checkpointPack;
      // The marker helpers the give-up-continue path drives: headIsWipMarker reflects the current HEAD;
      // undoWipMarker (the blind reset --mixed HEAD^) clears the marker and records that it fired.
      git.headIsWipMarker = (async () => marker.headIsMarker) as typeof git.headIsWipMarker;
      git.undoWipMarker = (async () => {
        marker.undoCalled = true;
        marker.headIsMarker = false;
      }) as typeof git.undoWipMarker;
      const outcome = await ctx.attemptCredentialSwitch!();
      if (outcome === "released") {
        // The loop broke with switchReleased latched; the runner skips finalize.
        return { branch: ctx.branch, switchReleased: true };
      }
      // "gave_up" — the run CONTINUES on the old token. Model a later NORMAL completion (report-only,
      // so the test needs no push/MR plumbing) that must then clean up the clone + HOME as usual.
      return {
        branch: ctx.branch,
        reportOnly: true,
        summary: "continued on the old token after the credential-switch give-up",
      };
    },
  };
  return { executor, captureAttempts, marker };
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
  it("verified capture + queued ack → RELEASED: reports credential_switch, keeps the clone for recapture, preserves HOME", async () => {
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
      // Released ⇒ the clone is KEPT for recapture (work is durable on the tracking ref + best-effort origin),
      // and the HOME is PRESERVED (parked) for a same-worker resume.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1260)), true, "the clone is kept for recapture on release");
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
      assert.strictEqual(fs.existsSync(worktreeDirFor(1261)), true, "the clone is kept for recapture on release");
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

  // --- PRD #1247 M5b data-integrity fix: the IN-PLACE ctx.attemptCredentialSwitch path -----------
  // The executor now handles the switch via ctx.attemptCredentialSwitch (wired to enterCredentialSwitch)
  // instead of throwing to the outer catch. These pin the RUNNER side of that wiring: the flag-clear on
  // give-up (so the continuing run cleans up normally) and the phasePublish switchReleased short-circuit.

  it("give-up via ctx.attemptCredentialSwitch CONTINUES: reports credential_switch_failed, CLEARS the preserve flags, UNDOES the wip(park) marker, and a later normal completion cleans up the clone + HOME", async () => {
    const { gitlab, calls } = fakeGitlab();
    // DIRTY tree whose WIP commit SUCCEEDS (a marker IS at HEAD) but never verifies ⇒ give up: the
    // continue-in-place path must undo that marker or it rides into the MR.
    const { executor, captureAttempts, marker } = makeInPlaceSwitchExecutor("marker_giveup");
    const home = seedHome();
    const claim = gitlabClaim(1264);
    try {
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      const s = statuses(claim.run_id);
      assert.ok(s.includes("credential_switch_failed"), "give-up reports credential_switch_failed (server clears the stamp, keeps the run)");
      assert.ok(!s.includes("credential_switch"), "the release is NEVER reported on a give-up");
      assert.ok(!s.includes("failed"), "a running give-up never fails the run");
      assert.ok(s.includes("completed"), "the run CONTINUED on the old token and reached a NORMAL completion");
      assert.strictEqual(calls.length, 0, "the report-only completion opens no MR");
      assert.ok(captureAttempts.count > 0, "the marker WAS committed and the capture reached the positive-verify step (which never covered) ⇒ give up");
      // The DATA-INTEGRITY fix: the give-up-continue UNDID the wip(park) marker it committed, so HEAD is
      // no longer a throwaway commit that would ride into the MR. Removing the undo in the runner reddens
      // exactly these two assertions (the marker would stay at HEAD).
      assert.ok(marker.undoCalled, "the give-up-continue UNDID the wip(park) marker (undoWipMarker fired)");
      assert.strictEqual(marker.headIsMarker, false, "HEAD is no longer a wip(park) marker after the in-place undo");
      // The give-up CLEARED the preserve flags (the run continued normally), so the NORMAL completion
      // cleans up as usual — the clone AND the HOME are removed, NOT preserved.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1264)), false, "the clone is cleaned up on the normal completion (flags cleared)");
      assert.strictEqual(fs.existsSync(home.sentinel), false, "the HOME is cleaned up on the normal completion (flags cleared)");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("BLOCKING-2: an idempotent release AFTER a reclaim (applied, status 'running', disposition 'released') is accepted as RELEASED, not a give-up", async () => {
    const { gitlab, calls } = fakeGitlab();
    const { executor } = makeInPlaceSwitchExecutor("verified");
    const home = seedHome();
    const claim = gitlabClaim(1270);
    try {
      // The release is a REDELIVERY the server already applied via a reclaim: it answers 200 with the
      // reclaimed run's status 'running' (NOT 'queued') plus disposition:'released'. The pre-fix worker
      // keyed release off status === 'queued' and so GAVE UP on this server-confirmed success, leaving
      // the old flight to keep working a claim the reclaim already owns; the fix keys off the released
      // disposition, so `status: 'running'` is still accepted as released.
      api.overrideStateStatus(claim.run_id, "running");
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      const s = statuses(claim.run_id);
      assert.ok(s.includes("credential_switch"), "the release reported credential_switch");
      assert.ok(!s.includes("credential_switch_failed"), "an idempotent-after-reclaim release is RELEASED, not a give-up");
      assert.ok(!s.includes("completed"), "a released run skips finalize");
      assert.ok(!s.includes("failed"), "a released run is not failed");
      assert.strictEqual(calls.length, 0, "a released run opens no MR");
      assert.strictEqual(fs.existsSync(worktreeDirFor(1270)), true, "the clone is kept for recapture on the accepted release");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the HOME is preserved for the reclaim");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("BLOCKING-3: a give-up whose credential_switch_failed clear is NOT confirmed RETAINS work and STOPS (no continue, no terminal report, marker kept)", async () => {
    const { gitlab, calls } = fakeGitlab();
    // DIRTY tree, marker committed, never verifies ⇒ GIVE UP. The clear report is REFUSED (409, not
    // applied), so the switch stamp may still be pending — continuing in place would let the server's
    // consume-nothing rule strand every buffered answer/follow-up for this claim. So the run RETAINS
    // everything and STOPS for requeue instead of continuing.
    const { executor, marker } = makeInPlaceSwitchExecutor("marker_giveup");
    const home = seedHome();
    const claim = gitlabClaim(1271);
    try {
      api.failStateWhen(claim.run_id, (b) => b.status === "credential_switch_failed", { httpStatus: 409 });
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      const s = statuses(claim.run_id);
      // The credential_switch_failed clear WAS attempted but the fake refused it (409) before
      // recording, matching the real server — so it does not appear in `statuses`; its refusal is
      // exactly what makes the clear "not confirmed" and drives the retain-and-stop below.
      assert.ok(!s.includes("credential_switch"), "the release is never reported on a give-up");
      // Clear NOT confirmed ⇒ retain-and-stop. On the pre-fix code the run CONTINUED regardless (a
      // normal completion was reported and the marker was undone); the fix STOPS: no completion, no
      // terminal report, the wip marker KEPT, and the clone + HOME retained for the reclaim.
      assert.ok(!s.includes("completed"), "an unconfirmed give-up STOPS — it does NOT continue to a completion");
      assert.ok(!s.includes("failed"), "a switch never fails the run");
      assert.strictEqual(calls.length, 0, "no MR is opened");
      assert.strictEqual(marker.undoCalled, false, "retain-and-stop KEEPS the wip(park) marker (no in-place undo)");
      assert.strictEqual(fs.existsSync(worktreeDirFor(1271)), true, "the clone is RETAINED for the reclaim");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the HOME is RETAINED for the reclaim");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("release via ctx.attemptCredentialSwitch ENDS: switchReleased skips finalize (no completed/MR), clone kept for recapture, HOME preserved", async () => {
    const { gitlab, calls } = fakeGitlab();
    const { executor } = makeInPlaceSwitchExecutor("verified");
    const home = seedHome();
    const claim = gitlabClaim(1265);
    try {
      api.overrideStateStatus(claim.run_id, "queued"); // the release requeues the run
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      const s = statuses(claim.run_id);
      assert.ok(s.includes("credential_switch"), "the release reported credential_switch");
      assert.ok(!s.includes("completed"), "phasePublish SKIPS finalize on switchReleased — no completed report");
      assert.ok(!s.includes("failed"), "a released run is not failed");
      assert.strictEqual(calls.length, 0, "a released run opens no MR");
      assert.strictEqual(fs.existsSync(worktreeDirFor(1265)), true, "the clone is kept for recapture on release");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the HOME is preserved for the reclaim");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("release via ctx.attemptCredentialSwitch LEAVES a wip(park) marker at HEAD: the reclaim's reseed reset-softs it, so the runner must NOT undo it in place", async () => {
    const { gitlab, calls } = fakeGitlab();
    // DIRTY tree whose WIP commit succeeds AND verifies ⇒ RELEASED with a marker at HEAD. On release the
    // run requeues and a reclaim reseeds (reset-softing the marker), so the give-up-only undo must NOT
    // fire here — this guards against the undo being wrongly moved out of the "gave_up" branch onto the
    // release (or into the shared enterCredentialSwitch).
    const { executor, marker } = makeInPlaceSwitchExecutor("marker_release");
    const home = seedHome();
    const claim = gitlabClaim(1266);
    try {
      api.overrideStateStatus(claim.run_id, "queued"); // the release requeues the run
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      const s = statuses(claim.run_id);
      assert.ok(s.includes("credential_switch"), "the release reported credential_switch");
      assert.ok(!s.includes("credential_switch_failed"), "a released run did NOT give up");
      assert.ok(!s.includes("completed"), "phasePublish SKIPS finalize on switchReleased — no completed report");
      assert.strictEqual(calls.length, 0, "a released run opens no MR");
      // The marker is LEFT at HEAD on release — the reclaim reseeds and reset-softs it.
      assert.ok(!marker.undoCalled, "the marker is NOT undone on release (undoWipMarker never fired)");
      assert.strictEqual(marker.headIsMarker, true, "the wip(park) marker is LEFT at HEAD for the reclaim's reseed");
      assert.strictEqual(fs.existsSync(worktreeDirFor(1266)), true, "the clone is kept for recapture on release");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the HOME is preserved for the reclaim");
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
      // Issue #1604: the persisted plan's frame instant; without it the worker fails closed and a
      // replayed approve is stale.
      resume_plan_at: "2026-01-01T00:00:00.000000Z",
    });
    // Sent after that plan was shown (created_at past resume_plan_at), so it is judged current.
    api.setInputs(claim.run_id, [{ ...input("approve_plan"), created_at: "2026-01-02T00:00:00.000000Z" }]);
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

// Issue #2945: a plan accepted on the storage retry that follows a credential-switch give-up still
// gets its summary. Real runner + real SdkExecutor; only the summary model and POST are faked.
describe("plan summary after a credential-switch give-up (issue #2945)", () => {
  type Mode = "storage-throw" | "ack" | "second-switch" | "pause-now" | "usage-in-settle"
    | "pause-now-during-switch" | "pause-wall-during-switch";

  async function scenario(mode: Mode) {
    newApi();
    const claim = freshClaim(1, { summary_intent_present: true, summary_model: "fixture-summary-model" });
    const runId = claim.run_id;
    const calls: PlanSummaryInput[] = [];
    const posts: string[] = [];
    let abortedPromptly: boolean | undefined;
    let ctxRef: RunContext | undefined;
    let thrown = false;
    let result: ExecutorResult | undefined;
    const blocks = mode === "second-switch" || mode === "pause-now" || mode === "usage-in-settle";
    const summaryRunner = {
      generateIntentSummary: async () => null,
      generatePlanSummary: async (input: PlanSummaryInput) => {
        calls.push(input);
        if (!blocks) return { summary: "Plan summary", deltas: [] };
        const aborted = new Promise<boolean>(resolve => {
          if (input.signal!.aborted) resolve(true);
          input.signal!.addEventListener("abort", () => resolve(true), { once: true });
          setTimeout(() => resolve(false), 3_000);
        });
        if (mode === "second-switch") api.requestCredentialSwitch(runId, 1);
        else if (mode === "pause-now") api.setInputs(runId, [{ id: 7_001, kind: "pause", body: "now" }]);
        else api.requestCredentialSwitch(runId, 1);
        abortedPromptly = await aborted;
        if (mode === "usage-in-settle") { input.onUsage?.({}); input.onUsage?.({}); }
        return { summary: "Late plan summary", deltas: [] };
      },
    } as unknown as SummaryRunner;
    const postPlanSummary = client.postPlanSummary;
    const reportState = client.reportState;
    client.postPlanSummary = async (_id, body) => { posts.push(body.plan_md); };
    const pauseDuringSwitch = mode === "pause-now-during-switch" ? "now"
      : mode === "pause-wall-during-switch" ? "wall" : undefined;
    if (mode === "storage-throw" || mode === "second-switch" || mode === "pause-now" || pauseDuringSwitch) {
      client.reportState = async (id, body, ...rest) => {
        if (id === runId && body.status === "awaiting_approval" && !thrown) {
          thrown = true;
          // The switch is requested, so the real ctx.signal aborts with a CredentialSwitchSignal; the
          // plan-storage report then fails with that signal before its callback can run.
          api.requestCredentialSwitch(runId, 1);
          assert.ok(await until(() => ctxRef!.signal!.aborted, 3_000), "the switch aborted ctx.signal");
          throw new CredentialSwitchSignal();
        }
        return reportState.call(client, id, body, ...rest);
      };
    }
    if (mode === "ack") api.armStateAckCredentialSwitchOnce(runId, 1, "awaiting_approval");
    const approveAt = mode === "storage-throw" || pauseDuringSwitch ? 1 : 2;
    let gates = 0;
    api.onState(runId, (b) => {
      if (b.status !== "awaiting_approval") return;
      if (++gates === approveAt) send(runId, row("approve_plan"));
    });
    const sdk = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: planWithMilestonesThenDoneQuery([]), client, summaryRunner, intentSettleBoundMs: 100,
    });
    const exec: Executor = {
      run: async (ctx) => {
        ctxRef = ctx;
        // A capture that never verifies: the switch gives up and the run continues on the old token.
        git.worktreeStatus = (async () => ["M src/impl.ts"]) as typeof git.worktreeStatus;
        let pausedDuringSwitch = false;
        git.commitWipMarker = (async () => {
          // An immediate pause routed while the give-up's capture is still in flight.
          if (pauseDuringSwitch && !pausedDuringSwitch) {
            pausedDuringSwitch = true;
            api.setInputs(runId, [{ id: 7_002, kind: "pause", body: pauseDuringSwitch }]);
            assert.ok(await until(() => ctx.pauseModeRequested?.() === pauseDuringSwitch, 3_000), "pause routed during the switch");
          }
          return false;
        }) as typeof git.commitWipMarker;
        return (result = await sdk.run(ctx));
      },
      closeAndSettlePlanSummary: (d) => sdk.closeAndSettlePlanSummary(d),
      killAgentTree: () => sdk.killAgentTree(),
      reapAttributedProcesses: () => sdk.reapAttributedProcesses(),
    };
    const { gitlab } = fakeGitlab();
    try {
      await runner(exec, gitlab, undefined, { recoveryRetryMs: 1, planApprovalTimeoutMs: 6_000 }).execute(claim);
    } finally {
      client.postPlanSummary = postPlanSummary;
      client.reportState = reportState;
    }
    const usage = api.messages(runId).filter(m => m.kind === "summary_usage");
    const statuses = api.states.filter(s => s.runId === runId).map(s => s.body.status);
    return { calls, posts, abortedPromptly, usage, statuses, result };
  }

  it("storage retry after a give-up generates and posts exactly one summary", async () => {
    const r = await scenario("storage-throw");
    assert.ok(r.statuses.includes("credential_switch_failed"), "the switch gave up");
    assert.equal(r.calls.length, 1);
    assert.deepEqual(r.posts, ["# PLAN\n- do it"]);
  });

  it("a switch riding the awaiting_approval ACK, then a give-up, still yields exactly one summary", async () => {
    const r = await scenario("ack");
    assert.ok(r.statuses.includes("credential_switch_failed"), "the switch gave up");
    assert.equal(r.calls.length, 1);
    assert.equal(r.posts.length, 1);
  });

  it("a second credential switch aborts the reopened generator promptly: no post, no usage, no latch", async () => {
    const r = await scenario("second-switch");
    assert.equal(r.calls.length, 1);
    assert.equal(r.abortedPromptly, true);
    assert.equal(r.posts.length, 0);
    assert.equal(r.usage.length, 0);
    assert.equal(r.result?.summaryPlan, undefined);
  });

  it("a pause-now aborts the reopened generator promptly: no post, no usage, no latch", async () => {
    const r = await scenario("pause-now");
    assert.equal(r.calls.length, 1);
    assert.equal(r.abortedPromptly, true);
    assert.equal(r.posts.length, 0);
    assert.equal(r.usage.length, 0);
    assert.equal(r.result?.summaryPlan, undefined);
  });

  for (const mode of ["pause-now-during-switch", "pause-wall-during-switch"] as const) {
    it(`a ${mode === "pause-now-during-switch" ? "now" : "wall"} pause routed during the give-up never reopens the summary`, async () => {
      const r = await scenario(mode);
      assert.ok(r.statuses.includes("credential_switch_failed"), "the switch gave up");
      assert.equal(r.calls.length, 0, "a pending immediate pause must not start a summary");
      assert.equal(r.posts.length, 0);
      assert.equal(r.result?.summaryPlan, undefined);
    });
  }

  it("usage reported during the bounded settlement of a switch-closed flight is emitted exactly once", async () => {
    const r = await scenario("usage-in-settle");
    assert.equal(r.calls.length, 1, "a started flight is not revived by the retry");
    assert.equal(r.posts.length, 0);
    assert.equal(r.usage.length, 1);
  });
});
