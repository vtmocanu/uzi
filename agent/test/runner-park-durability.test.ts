import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { LimitReachedError } from "../src/limit.js";
import type { StateRequest } from "../src/protocol.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { commitInTree, FakeRecoveryClient, FakeRecoveryGit, makeRecoveryCoordinator } from "./codex-reap-fixture.js";

installHarness();

// PRD #1809 D8: a park report says whether the checkpoint it published contains the run's latest
// committed work (`checkpoint_contains_latest`), gated on the api's `run_checkpoint_durability`
// feature. The #1798 shape: the park's fetch-back of the clone into the worker bare fails
// (ENOSPC), so the publish packs the OLDER tracking tip; the park used to say only "checkpoint
// published". Custody handling is unchanged: such a park keeps its hold.

const FEATURE = "run_checkpoint_durability";

function drain(stream: Readable): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on("data", (c: Buffer) => chunks.push(Buffer.from(c)));
    stream.on("end", () => resolve(Buffer.concat(chunks)));
    stream.on("error", reject);
  });
}

/** Make every checkpoint publish land (or fail with `httpStatus`), recording the tips. */
function stubPublish(httpStatus?: number): string[] {
  const tips: string[] = [];
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, tipOid: string, pack: Readable) => {
    await drain(pack);
    tips.push(tipOid);
    return httpStatus === undefined
      ? { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } }
      : { ok: false, httpStatus };
  };
  return tips;
}

/** Fail every fetch-back after the first `ok` ones, as a full data volume does. */
function failFetchBackAfter(ok: number): void {
  const real = git.fetchAgentBranch.bind(git);
  let calls = 0;
  git.fetchAgentBranch = async (...args: Parameters<typeof real>) => {
    calls += 1;
    if (calls > ok) throw new Error("fatal: unable to write loose object file: No space left on device");
    return await real(...args);
  };
}

/** Commit milestone 1, fetch it back to the tracking ref (the mid-run checkpoint's job), commit
 *  milestone 2 without fetching it back, then end the turn with `end`. */
function twoMilestoneFactory(homeRoot: string, end: (ctx: RunContext) => Promise<ExecutorResult>): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        commitInTree(ctx.worktreePath, "M1.txt", "milestone 1\n");
        await ctx.checkpoint?.({ reap: false });
        commitInTree(ctx.worktreePath, "M2.txt", "milestone 2, committed, not yet fetched back\n");
        return await end(ctx);
      },
    },
  });
}

const limitEnd = async (): Promise<ExecutorResult> => {
  throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
};

const reports = (runId: string, status: string): StateRequest[] =>
  api.states.filter((s) => s.runId === runId && s.body.status === status).map((s) => s.body);

const statusTexts = (runId: string): string[] =>
  api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String(m.payload.text));

async function withHomeRoot(fn: (homeRoot: string) => Promise<void>): Promise<void> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-d8-"));
  try {
    await fn(homeRoot);
  } finally {
    fs.rmSync(homeRoot, { recursive: true, force: true });
  }
}

describe("RunRunner — PRD #1809 D8: the usage-limit park reports what its checkpoint holds", () => {
  it("a clean park reports checkpoint_contains_latest: true and says the checkpoint is published", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      const tips = stubPublish();
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(18091, { wait_on_limit: true });
      await runnerWith(twoMilestoneFactory(homeRoot, limitEnd), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
      }).execute(claim);
      const [park, ...more] = reports(claim.run_id, "limit_wait");
      assert.deepStrictEqual(more, []);
      assert.ok(tips.length > 0, "the park published a checkpoint");
      assert.strictEqual(park?.checkpoint_contains_latest, true);
      assert.ok(statusTexts(claim.run_id).includes("park checkpoint published to origin"));
    });
  });

  it("an api without the feature gets no checkpoint_contains_latest field", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [];
      stubPublish();
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(18092, { wait_on_limit: true });
      await runnerWith(twoMilestoneFactory(homeRoot, limitEnd), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
      }).execute(claim);
      const [park] = reports(claim.run_id, "limit_wait");
      assert.ok(park, "parked");
      assert.ok(!("checkpoint_contains_latest" in park), `field omitted, got ${JSON.stringify(park)}`);
    });
  });

  it("no checkpoint published (the publish failed): the field is omitted", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      stubPublish(500);
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(18093, { wait_on_limit: true });
      await runnerWith(twoMilestoneFactory(homeRoot, limitEnd), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
      }).execute(claim);
      const [park] = reports(claim.run_id, "limit_wait");
      assert.ok(park, "parked");
      assert.strictEqual(park.checkpoint_contains_latest, undefined);
    });
  });

  it("the #1798 shape: fetch-back and recovery pin fail, the stale checkpoint is published; reports false and keeps the custody hold", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      const tips = stubPublish();
      // The mid-run checkpoint's fetch-back lands (milestone 1); every later one fails.
      failFetchBackAfter(1);
      const archive = new FakeRecoveryClient();
      const { coord, root } = makeRecoveryCoordinator(archive, new FakeRecoveryGit());
      // The journal write fails too (ENOSPC): recovery.pin's failure result.
      (coord as unknown as { pin: () => Promise<undefined> }).pin = async () => undefined;
      try {
        const { gitlab } = fakeGitlab();
        const claim = gitlabClaim(18094, { wait_on_limit: true, claim_generation: 4 });
        await runnerWith(twoMilestoneFactory(homeRoot, limitEnd), gitlab, undefined, nullLogger(), {
          checkpointIntervalMs: 0,
          recovery: coord,
        }).execute(claim);
        const [park] = reports(claim.run_id, "limit_wait");
        assert.ok(park, "the run still parks: a park that fails is worse than one that loses work");
        assert.strictEqual(tips.length, 1, "the older tracking tip was published");
        assert.strictEqual(park.checkpoint_contains_latest, false, "the checkpoint lacks milestone 2");
        const feed = statusTexts(claim.run_id);
        assert.ok(
          feed.some((t) => t.startsWith("park checkpoint published to origin, but it does not contain the latest committed work")),
          `the feed names the stale checkpoint, got ${JSON.stringify(feed)}`,
        );
        assert.deepStrictEqual(archive.releaseCalls, [], "custody is never released while the latest work is on the worker only");
        assert.deepStrictEqual(archive.reserveCalls, [], "nothing to archive: the head never reached the bare");
      } finally {
        fs.rmSync(root, { recursive: true, force: true });
      }
    });
  });
});

describe("RunRunner — PRD #1809 D8: the owner pause park reports what its checkpoint holds", () => {
  const pauseEnd = async (ctx: RunContext): Promise<ExecutorResult> => {
    const at = { completedCount: 1, total: 2 };
    const parked = await ctx.parkForPause?.(at);
    return parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
  };

  it("a clean pause park reports checkpoint_contains_latest: true", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      stubPublish();
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(18095);
      await runnerWith(twoMilestoneFactory(homeRoot, pauseEnd), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
      }).execute(claim);
      const [park] = reports(claim.run_id, "paused");
      assert.ok(park, "paused");
      assert.strictEqual(park.checkpoint_contains_latest, true);
    });
  });

  it("a pause whose fetch-back fails publishes the older tip and reports false", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      const tips = stubPublish();
      failFetchBackAfter(1);
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(18096);
      await runnerWith(twoMilestoneFactory(homeRoot, pauseEnd), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
      }).execute(claim);
      const [park] = reports(claim.run_id, "paused");
      assert.ok(park, "paused");
      assert.strictEqual(tips.length, 1);
      assert.strictEqual(park.checkpoint_contains_latest, false);
    });
  });
});

describe("RunRunner — PRD #1809 D8: a recovery park reports what its checkpoint holds", () => {
  const transientEnd = async (): Promise<ExecutorResult> => {
    throw new TransientRecoveryError();
  };

  it("a verified, published capture reports checkpoint_contains_latest: true", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      stubPublish();
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(18097);
      await runnerWith(twoMilestoneFactory(homeRoot, transientEnd), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
        recoveryRetryMs: 5,
      }).execute(claim);
      const [park] = reports(claim.run_id, "recovery_wait");
      assert.ok(park, "parked");
      assert.strictEqual(park.checkpoint_contains_latest, true);
    });
  });

  it("a capture saved on the worker only (the publish failed) omits the field", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      stubPublish(503);
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(18098);
      await runnerWith(twoMilestoneFactory(homeRoot, transientEnd), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
        recoveryRetryMs: 5,
      }).execute(claim);
      const [park] = reports(claim.run_id, "recovery_wait");
      assert.ok(park, "parked");
      assert.ok(!("checkpoint_contains_latest" in park), `field omitted, got ${JSON.stringify(park)}`);
    });
  });
});

describe("RunRunner — PRD #1809 D8: the wall park reports what its checkpoint holds", () => {
  const wallEnd = async (ctx: RunContext): Promise<ExecutorResult> => {
    const outcome = await ctx.parkForWall?.({ completedCount: 1, total: 2 });
    return outcome === "parked" || outcome === "undeliverable"
      ? { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } }
      : { branch: ctx.branch };
  };

  for (const [name, features, httpStatus, expected] of [
    ["a verified, published capture reports true", [FEATURE], undefined, true],
    ["an api without the feature gets no field", [], undefined, undefined],
    ["a failed publish omits the field", [FEATURE], 500, undefined],
  ] as const) {
    it(name, async () => {
      await withHomeRoot(async (homeRoot) => {
        client.protocolFeatures = [...features];
        stubPublish(httpStatus);
        const { gitlab } = fakeGitlab();
        const claim = gitlabClaim(18099);
        await runnerWith(twoMilestoneFactory(homeRoot, wallEnd), gitlab, undefined, nullLogger(), {
          checkpointIntervalMs: 0,
        }).execute(claim);
        const [req, ...more] = api.wallParkRequests;
        assert.deepStrictEqual(more, []);
        assert.ok(req, "one wall park report");
        assert.strictEqual(req.body.checkpoint_contains_latest, expected);
        if (expected === undefined) assert.ok(!("checkpoint_contains_latest" in req.body));
      });
    });
  }
});
