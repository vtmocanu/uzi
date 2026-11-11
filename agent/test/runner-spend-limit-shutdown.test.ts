import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import type { RunRunner } from "../src/runner.js";
import { SpendLimitHoldError } from "../src/limit.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { providerErrorMessage } from "../src/sdk-messages.js";
import { recordingLogger } from "./helpers.js";
import { api, client, deferred, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { commitInTree, enabledRecovery, feedTexts, installRecoveryRootCleanup } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

// Issue #3767: a worker shutdown that races the spend-limit park takes the shutdown interruption
// (checkpoint, feed diagnostic, no terminal report) at every point it can meet the park, instead of
// being dropped silently or reported as `failed`.

const FEATURE = "recovery_cause_anthropic_spend_limit";
const TEXT = "You have reached your specified API usage limits. You will regain access on 2026-12-01 at 00:00 UTC.";
const MESSAGE = providerErrorMessage(400, `API Error: 400 ${TEXT}`);
const spend = () => new SpendLimitHoldError(MESSAGE, TEXT.slice(0, 200).trim());

const statesOf = (runId: string) => api.states.filter((s) => s.runId === runId).map((s) => s.body);
const parks = (runId: string) => statesOf(runId).filter((b) => b.status === "recovery_wait");
const failures = (runId: string) => statesOf(runId).filter((b) => b.status === "failed");
const checkpointLines = (runId: string) => feedTexts(runId).filter((t) => t.startsWith("shutdown checkpoint"));

async function until(pred: () => boolean, what: string, ms = 15000): Promise<void> {
  const end = Date.now() + ms;
  while (!pred()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 5));
  }
}

async function bounded<T>(p: Promise<T>, what: string, ms = 30000): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([
      p,
      new Promise<never>((_, reject) => { timer = setTimeout(() => reject(new Error(`${what} did not finish`)), ms); }),
    ]);
  } finally { clearTimeout(timer); }
}

interface Rig {
  runner: RunRunner;
  /** Resolves once the executor has committed work and is about to throw (or wait for the abort). */
  thrown: Promise<void>;
  exits: () => string[];
}

function rig(homeRoot: string, opts: { waitForAbort: boolean }): Rig {
  const { gitlab } = fakeGitlab();
  const { coord } = enabledRecovery();
  const home = path.join(homeRoot, "home");
  const thrown = deferred();
  const { logger, lines } = recordingLogger();
  const executor: Executor = {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      fs.mkdirSync(home, { recursive: true });
      fs.writeFileSync(path.join(home, "session"), "transcript", "utf8");
      commitInTree(ctx.worktreePath, "WORK.txt", "work before the spend limit\n");
      thrown.resolve();
      if (opts.waitForAbort) {
        await new Promise<void>((resolve) => {
          if (ctx.signal!.aborted) return resolve();
          ctx.signal!.addEventListener("abort", () => resolve(), { once: true });
        });
      }
      throw spend();
    },
  };
  const runner = runnerWith(() => ({ homeDir: home, executor }), gitlab, undefined, logger, {
    recovery: coord, recoveryRetryMs: 5,
  });
  const exits = (): string[] => (lines as Array<Record<string, unknown>>)
    .filter((l) => l.msg === "spend-limit park interrupted by worker shutdown")
    .map((l) => String(l.exit));
  return { runner, thrown: thrown.promise, exits };
}

async function withHome(fn: (homeRoot: string) => Promise<void>): Promise<void> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-spend-shutdown-"));
  try { await fn(homeRoot); } finally { fs.rmSync(homeRoot, { recursive: true, force: true }); }
}

describe("RunRunner #3767 spend-limit park meets a worker shutdown", () => {
  it("shutdown already in progress when the spend terminal arrives: shutdown interruption, no park request (pre_catch)", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3790, { claim_generation: 3 });
      const r = rig(homeRoot, { waitForAbort: true });
      let sends = 0;
      api.failStateWhen(claim.run_id, (b) => {
        if (b.recovery_cause === "anthropic_spend_limit") sends++;
        return false;
      });
      const done = r.runner.execute(claim);
      await r.thrown;
      r.runner.shutdown();
      await bounded(done, "execute");
      assert.deepEqual(r.exits(), ["pre_catch"]);
      assert.equal(sends, 0, "no spend park request was sent");
      assert.equal(parks(claim.run_id).length, 0);
      assert.equal(failures(claim.run_id).length, 0, "the run is left for requeue, not failed");
      assert.equal(checkpointLines(claim.run_id).length, 1, `a delivered shutdown checkpoint line: ${JSON.stringify(feedTexts(claim.run_id))}`);
    });
  });

  it("shutdown during the ownership probe / retry wait: shutdown interruption at the loop top (loop_top)", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3791, { claim_generation: 3 });
      // Every ownership read fails transiently, so the park loop sits in its probe / retry wait.
      api.failOwnership(claim.run_id, 503);
      const r = rig(homeRoot, { waitForAbort: false });
      const done = r.runner.execute(claim);
      await r.thrown;
      const baseline = api.ownershipRequests.length;
      await until(() => api.ownershipRequests.length > baseline, "the park loop's ownership probe");
      r.runner.shutdown();
      await bounded(done, "execute");
      assert.deepEqual(r.exits(), ["loop_top"]);
      assert.equal(parks(claim.run_id).length, 0);
      assert.equal(failures(claim.run_id).length, 0);
      assert.equal(checkpointLines(claim.run_id).length, 1, `a delivered shutdown checkpoint line: ${JSON.stringify(feedTexts(claim.run_id))}`);
    });
  });

  it("shutdown while the park request is in flight, then a definite 4xx: shutdown interruption instead of failed (terminal_arm)", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3792, { claim_generation: 3 });
      const r = rig(homeRoot, { waitForAbort: false });
      let sends = 0;
      api.failStateWhen(claim.run_id, (b) => {
        if (b.recovery_cause !== "anthropic_spend_limit") return false;
        sends++;
        r.runner.shutdown();
        return true;
      }, { httpStatus: 403, error: "forbidden" });
      await bounded(r.runner.execute(claim), "execute");
      assert.ok(sends >= 1, "the park request was sent");
      assert.deepEqual(r.exits(), ["terminal_arm"]);
      assert.equal(failures(claim.run_id).length, 0, "no failed report");
      assert.equal(checkpointLines(claim.run_id).length, 1, `a delivered shutdown checkpoint line: ${JSON.stringify(feedTexts(claim.run_id))}`);
    });
  });

  it("an interruption thrown out of the park capture during shutdown is handled inside the handler (propagated)", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3793, { claim_generation: 3 });
      const r = rig(homeRoot, { waitForAbort: false });
      let sends = 0;
      api.failStateWhen(claim.run_id, (b) => {
        if (b.recovery_cause === "anthropic_spend_limit") sends++;
        return false;
      });
      (r.runner as unknown as { captureRecoveryRestorePoint: unknown }).captureRecoveryRestorePoint = async () => {
        r.runner.shutdown();
        throw new TransientRecoveryError("capture interrupted by shutdown");
      };
      await bounded(r.runner.execute(claim), "execute");
      assert.deepEqual(r.exits(), ["propagated"]);
      assert.equal(sends, 0, "no spend park request was sent");
      assert.equal(failures(claim.run_id).length, 0);
      assert.equal(checkpointLines(claim.run_id).length, 1, `a delivered shutdown checkpoint line: ${JSON.stringify(feedTexts(claim.run_id))}`);
    });
  });

  it("control: shutdown during the request, then an accepted spend ACK, still parks", async () => {
    await withHome(async (homeRoot) => {
      client.protocolFeatures = ["claim_generation_fence", FEATURE];
      const claim = gitlabClaim(3794, { claim_generation: 3 });
      const r = rig(homeRoot, { waitForAbort: false });
      api.failStateWhen(claim.run_id, (b) => {
        if (b.recovery_cause !== "anthropic_spend_limit") return false;
        r.runner.shutdown();
        return false;
      });
      await bounded(r.runner.execute(claim), "execute");
      assert.equal(parks(claim.run_id).length, 1, "the run is parked");
      assert.equal(parks(claim.run_id)[0]!.recovery_cause, "anthropic_spend_limit");
      assert.equal(failures(claim.run_id).length, 0);
      assert.deepEqual(r.exits(), [], "an accepted ACK is not a shutdown interruption");
    });
  });
});
