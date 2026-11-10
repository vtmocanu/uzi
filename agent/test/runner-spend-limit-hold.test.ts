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

const FEATURE = "recovery_cause_anthropic_spend_limit";
const TEXT = "You have reached your specified API usage limits. You will regain access on 2026-12-01 at 00:00 UTC.";
const RAW = `API Error: 400 ${TEXT}`;
const MESSAGE = providerErrorMessage(400, RAW);

function rig(thrown: () => Error, homeRoot: string): { runner: RunRunner; custody: () => number; home: string } {
  const { gitlab } = fakeGitlab();
  const { coord } = enabledRecovery();
  const home = path.join(homeRoot, "home");
  const behavior = async (ctx: RunContext): Promise<ExecutorResult> => {
    fs.mkdirSync(home, { recursive: true });
    fs.writeFileSync(path.join(home, "session"), "transcript", "utf8");
    commitInTree(ctx.worktreePath, "WORK.txt", "work before the spend limit\n");
    throw thrown();
  };
  const executor: Executor = { run: behavior };
  const runner = runnerWith(() => ({ homeDir: home, executor }), gitlab, undefined, nullLogger(), {
    recovery: coord, recoveryRetryMs: 5,
  });
  return { runner, custody: spyCustodySettle(runner), home };
}

async function withHome(fn: (homeRoot: string) => Promise<void>): Promise<void> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-spend-park-"));
  try { await fn(homeRoot); } finally { fs.rmSync(homeRoot, { recursive: true, force: true }); }
}

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
      assert.match(feed[0]!, /Resume retries under the same credential choice/);
      assert.match(feed[0]!, /uzi run set-token/);
      assert.match(feed[0]!, /Run with another token/);
      assert.match(feed[0]!, /Cancel ends the run/);
      assert.doesNotMatch(feed[0]!.replace(TEXT.slice(0, 200).trim(), ""), /automatically|\byour\b/i, "owner-neutral, no automatic retry wording");
    });
  });

  it("feature absent: fails with the byte-identical provider message and never parks", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence"];
      const claim = gitlabClaim(3768, { claim_generation: 3 });
      await rig(spend, homeRoot).runner.execute(claim);
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
      }, { httpStatus: 400, error: "unknown recovery_cause" });
      await rig(spend, homeRoot).runner.execute(claim);
      assert.equal(sends, 1, "one typed park send, no retry loop");
      assert.equal(parks(claim.run_id).length, 0, "no untyped park was sent either");
      const failed = failures(claim.run_id);
      assert.equal(failed.length, 1);
      assert.equal(failed[0]!.failure_reason, MESSAGE);
      assert.equal(client.protocolFeatures.includes(FEATURE), false, "the refused feature is dropped");
    });
  });
});
