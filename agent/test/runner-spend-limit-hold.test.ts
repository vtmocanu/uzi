import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import type { RunRunner } from "../src/runner.js";
import { SpendLimitHoldError } from "../src/limit.js";
import { providerErrorMessage } from "../src/sdk-messages.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";
import { commitInTree, enabledRecovery, feedTexts, installRecoveryRootCleanup, spyCustodySettle } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

// Issue #3767: a Claude lead whose terminal is an Anthropic spend-limit 400 parks recovery_wait
// typed `anthropic_spend_limit` (owner-held) when the api advertised the cause; otherwise it fails
// with the unchanged provider message and never loops.

// The api's generic invalid-state 400 body (api/internal/handler/worker_protocol.go), which is what an
// api that does not know a recovery_cause answers; the client drops the feature only on this exact body.
const GENERIC_INVALID_STATE_ERROR =
  "state must be one of running, awaiting_approval, awaiting_input, awaiting_followup, limit_wait, recovery_wait, paused, pause_failed, credential_switch, credential_switch_failed, credential_switch_held, credential_switch_hold_settled, clarification_settled, completed, failed";

const FEATURE = "recovery_cause_anthropic_spend_limit";
const TEXT = "You have reached your specified API usage limits. You will regain access on 2026-12-01 at 00:00 UTC.";
const RAW = `API Error: 400 ${TEXT}`;
const MESSAGE = providerErrorMessage(400, RAW);

function rig(thrown: () => Error, homeRoot: string, pre?: (ctx: RunContext) => void): { runner: RunRunner; custody: () => number; home: string; turns: () => number } {
  let turnCount = 0;
  const { gitlab } = fakeGitlab();
  const { coord } = enabledRecovery();
  const home = path.join(homeRoot, "home");
  const behavior = async (ctx: RunContext): Promise<ExecutorResult> => {
    turnCount++;
    fs.mkdirSync(home, { recursive: true });
    fs.writeFileSync(path.join(home, "session"), "transcript", "utf8");
    commitInTree(ctx.worktreePath, "WORK.txt", "work before the spend limit\n");
    pre?.(ctx);
    throw thrown();
  };
  const executor: Executor = { run: behavior };
  const runner = runnerWith(() => ({ homeDir: home, executor }), gitlab, undefined, nullLogger(), {
    recovery: coord, recoveryRetryMs: 5,
  });
  return { runner, custody: spyCustodySettle(runner), home, turns: () => turnCount };
}

async function withHome(fn: (homeRoot: string) => Promise<void>): Promise<void> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-spend-park-"));
  try { await fn(homeRoot); } finally { fs.rmSync(homeRoot, { recursive: true, force: true }); }
}

const RESUME_SENTENCE = "Resume uses this run's current credential selection. If it still selects the capped token, raise the limit first.";
const SWITCH_SENTENCE = "To choose another token now, use Switch token or `uzi run set-token`.";

const spend = () => new SpendLimitHoldError(MESSAGE, TEXT.slice(0, 200).trim());
const statesOf = (runId: string) => api.states.filter((s) => s.runId === runId).map((s) => s.body);
const parks = (runId: string) => statesOf(runId).filter((b) => b.status === "recovery_wait");
const failures = (runId: string) => statesOf(runId).filter((b) => b.status === "failed");

describe("RunRunner #3767 spend-limit hold", () => {
  it("advertised + fenced: parks recovery_wait anthropic_spend_limit, keeps clone and session, owner-neutral feed", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const r = rig(spend, homeRoot);
      const claim = gitlabClaim(3767, { claim_generation: 3 });
      await r.runner.execute(claim);
      const sent = parks(claim.run_id);
      assert.equal(sent.length, 1);
      assert.equal(sent[0]!.recovery_cause, "anthropic_spend_limit");
      assert.equal(sent[0]!.claim_generation, 3);
      assert.equal(sent[0]!.provider_progress_turns, undefined);
      assert.equal(sent[0]!.productive_turns, undefined);
      assert.equal(failures(claim.run_id).length, 0, "the run is parked, not failed");
      assert.equal(r.custody(), 0, "no settlement: custody is kept");
      assert.equal(fs.existsSync(worktreeDirFor(3767)), true, "the clone is kept");
      assert.equal(fs.existsSync(r.home), true, "the session HOME is kept");
      const feed = feedTexts(claim.run_id).filter((t) => /spend limit/.test(t));
      assert.equal(feed.length, 1);
      assert.match(feed[0]!, /You have reached your specified API usage limits\./);
      assert.ok(feed[0]!.includes(RESUME_SENTENCE));
      assert.ok(feed[0]!.includes(SWITCH_SENTENCE));
      assert.match(feed[0]!, /Cancel ends the run/);
      assert.doesNotMatch(feed[0]!.replace(TEXT.slice(0, 200).trim(), ""), /automatically|\byour\b/i, "owner-neutral, no automatic retry wording");
    });
  });

  it("every in-scope kind parks with work and custody retained; the switch sentence is omitted only on self_improve", async () => {
    const kinds = [
      [3780, "issue", true], [3781, "task", true], [3782, "prompt", true],
      [3783, "ci_fix", true], [3784, "mr_rework", true], [3785, "self_improve", false],
    ] as const;
    for (const [iid, kind, switchable] of kinds) {
      await withHome(async (homeRoot) => {
        client.protocolFeatures = ["claim_generation_fence", FEATURE];
        const base = gitlabClaim(iid, { claim_generation: 3 });
        const claim = kind === "issue" ? base
          : kind === "task" ? { ...base, kind, branch: `uzi/task/${base.run_id}` }
          : kind === "mr_rework" ? { ...base, kind, issue_iid: null, branch: `agent/issue-${iid}`, base_branch: "main" }
          : { ...base, kind };
        const r = rig(spend, homeRoot);
        await r.runner.execute(claim);
        assert.equal(parks(claim.run_id).length, 1, `${kind} parks; states: ${JSON.stringify(statesOf(claim.run_id).map((s) => [s.status, s.failure_reason]))}`);
        assert.equal(parks(claim.run_id)[0]!.recovery_cause, "anthropic_spend_limit", kind);
        assert.equal(failures(claim.run_id).length, 0, `${kind} is parked, not failed`);
        assert.equal(r.custody(), 0, `${kind}: custody is kept`);
        assert.equal(fs.existsSync(r.home), true, `${kind}: the session HOME is kept`);
        const feed = feedTexts(claim.run_id).filter((t) => /spend limit/.test(t));
        assert.equal(feed.length, 1, kind);
        assert.ok(feed[0]!.includes(RESUME_SENTENCE), kind);
        assert.equal(feed[0]!.includes(SWITCH_SENTENCE), switchable, kind);
        assert.equal(/To choose another token now/.test(feed[0]!), switchable, kind);
        assert.match(feed[0]!, /Cancel ends the run\.$/);
      });
    }
  });

  it("feature absent: fails with the byte-identical provider message and never parks", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence"];
      const claim = gitlabClaim(3768, { claim_generation: 3 });
      const r = rig(spend, homeRoot);
      await r.runner.execute(claim);
      assert.equal(r.turns(), 1, "exactly one executor turn: no retry and no second claim");
      assert.equal(parks(claim.run_id).length, 0);
      const failed = failures(claim.run_id);
      assert.equal(failed.length, 1);
      assert.equal(failed[0]!.failure_reason, MESSAGE);
      assert.match(failed[0]!.failure_reason!, /^provider error: 400 You have reached your specified API usage limits\./);
    });
  });

  it("an unfenced (generation 0) claim fails like an absent feature", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3769, { claim_generation: 0 });
      await rig(spend, homeRoot).runner.execute(claim);
      assert.equal(parks(claim.run_id).length, 0);
      assert.equal(failures(claim.run_id)[0]?.failure_reason, MESSAGE);
    });
  });

  it("another 400 text is an ordinary failure, never a park", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const other = "provider error: 400 messages: text content blocks must be non-empty";
      const claim = gitlabClaim(3770, { claim_generation: 3 });
      await rig(() => new Error(other), homeRoot).runner.execute(claim);
      assert.equal(parks(claim.run_id).length, 0);
      assert.equal(failures(claim.run_id)[0]?.failure_reason, other);
    });
  });

  it("an api that refuses the typed park with a 400 gets exactly one send, then the run fails with the original message", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3771, { claim_generation: 3 });
      let sends = 0;
      api.failStateWhen(claim.run_id, (b) => {
        if (b.recovery_cause !== "anthropic_spend_limit") return false;
        sends++;
        return true;
      }, { httpStatus: 400, error: GENERIC_INVALID_STATE_ERROR });
      await rig(spend, homeRoot).runner.execute(claim);
      assert.equal(sends, 1, "one typed park send, no retry loop");
      assert.equal(parks(claim.run_id).length, 0, "no untyped park was sent either");
      const failed = failures(claim.run_id);
      assert.equal(failed.length, 1);
      assert.equal(failed[0]!.failure_reason, MESSAGE);
      assert.equal(client.protocolFeatures.includes(FEATURE), false, "the refused feature is dropped");
    });
  });

  it("a feature dropped between park retries fails the run with the original message, no loop and no untyped send", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3772, { claim_generation: 3 });
      let sends = 0;
      const arm = (): void => api.failStateWhen(claim.run_id, (b) => {
        if (b.recovery_cause !== "anthropic_spend_limit") return false;
        sends++;
        // Every typed send in the first runner attempt hits a 5xx (the client's own bounded transport
        // retries included, hence the re-arm); the feature set then shrinks (heartbeat replacement or
        // a strict-decode rollback), so the runner's next park retry is refused locally.
        client.protocolFeatures = ["claim_generation_fence"];
        if (sends < 20) arm();
        return true;
      }, { httpStatus: 503, error: "unavailable" });
      arm();
      const r = rig(spend, homeRoot);
      const outcome = await Promise.race([
        r.runner.execute(claim).then(() => "done"),
        new Promise<string>((resolve) => setTimeout(() => resolve("timeout"), 20000).unref()),
      ]);
      assert.equal(outcome, "done", "the run must end instead of retrying forever");
      assert.ok(sends >= 1 && sends < 20, `bounded typed sends, got ${sends}`);
      assert.equal(parks(claim.run_id).filter((b) => b.recovery_cause === undefined).length, 0, "no untyped park was sent");
      const failed = failures(claim.run_id);
      assert.equal(failed.length, 1);
      assert.equal(failed[0]!.failure_reason, MESSAGE);
    });
  });
  it("the park report carries the failing turn's session id (B), not the one observed earlier (A)", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3773, { claim_generation: 3 });
      const r = rig(() => new SpendLimitHoldError(MESSAGE, TEXT.slice(0, 200).trim(), "session-B"), homeRoot,
        (ctx) => ctx.onSessionId?.("session-A"));
      await r.runner.execute(claim);
      const sent = parks(claim.run_id);
      assert.equal(sent.length, 1);
      assert.equal(sent[0]!.recovery_cause, "anthropic_spend_limit");
      assert.equal(sent[0]!.session_id, "session-B");
    });
  });

  it("an ownership probe that shows a newer claim generation stops the run: no park send, no failed, no retry loop", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3774, { claim_generation: 3 });
      let sends = 0;
      api.failStateWhen(claim.run_id, (b) => {
        if (b.recovery_cause === "anthropic_spend_limit") sends++;
        return false;
      });
      const r = rig(spend, homeRoot, () => api.setOwnershipStatus(claim.run_id, "running", 4));
      const before = api.ownershipRequests.length;
      const outcome = await Promise.race([
        r.runner.execute(claim).then(() => "done"),
        new Promise<string>((resolve) => setTimeout(() => resolve("timeout"), 20000).unref()),
      ]);
      assert.equal(outcome, "done", "the run must stop instead of retrying");
      assert.equal(sends, 0, "no park was sent through the superseded claim");
      assert.equal(parks(claim.run_id).length, 0);
      assert.equal(failures(claim.run_id).length, 0, "no failed report through a superseded claim");
      assert.ok(api.ownershipRequests.length - before <= 5, `bounded ownership reads, got ${api.ownershipRequests.length - before}`);
      assert.equal(r.turns(), 1);
    });
  });

  it("a park report refused as a stale claim stops the run: no failed, no second park send, no retry loop", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3775, { claim_generation: 3 });
      let sends = 0;
      api.failStateWhen(claim.run_id, (b) => {
        if (b.recovery_cause !== "anthropic_spend_limit") return false;
        sends++;
        return true;
      }, { httpStatus: 409, runStatus: "running", disposition: "stale_claim" });
      const r = rig(spend, homeRoot);
      const outcome = await Promise.race([
        r.runner.execute(claim).then(() => "done"),
        new Promise<string>((resolve) => setTimeout(() => resolve("timeout"), 20000).unref()),
      ]);
      assert.equal(outcome, "done", "the run must stop instead of retrying");
      assert.equal(sends, 1, "exactly one park send");
      assert.equal(parks(claim.run_id).length, 0, "the park is not re-sent through a superseded claim");
      assert.equal(failures(claim.run_id).length, 0, "no failed report through a superseded claim");
    });
  });

  it("an ambiguous first send followed by a 401 retries; the ownership probe then shows recovery_wait and the run counts as parked", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3776, { claim_generation: 3 });
      const isSpend = (b: { recovery_cause?: string }): boolean => b.recovery_cause === "anthropic_spend_limit";
      let dropped = 0;
      let refused = 0;
      api.dropStatesWhen(claim.run_id, (b) => isSpend(b) && dropped++ === 0);
      api.failStateWhen(claim.run_id, (b) => {
        if (!isSpend(b)) return false;
        refused++;
        // The first send's outcome is unknown (connection dropped) and this definite refusal followed it:
        // the park may have landed, so the api's view is what decides, and it says the run is parked.
        api.setOwnershipStatus(claim.run_id, "recovery_wait", 3);
        return true;
      }, { httpStatus: 401, error: "unauthorized" });
      const r = rig(spend, homeRoot);
      const outcome = await Promise.race([
        r.runner.execute(claim).then(() => "done"),
        new Promise<string>((resolve) => setTimeout(() => resolve("timeout"), 20000).unref()),
      ]);
      assert.equal(outcome, "done");
      assert.equal(dropped >= 1 && refused === 1, true, `one dropped send then one 401, got dropped=${dropped} refused=${refused}`);
      assert.equal(failures(claim.run_id).length, 0, "an uncertain park outcome is never reported as failed");
      assert.equal(r.custody(), 0, "custody is kept: the run counts as parked");
      assert.equal(fs.existsSync(worktreeDirFor(3776)), true, "the clone is kept");
    });
  });
});
