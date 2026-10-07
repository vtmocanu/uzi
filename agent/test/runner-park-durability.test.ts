import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
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
import { api, client, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
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

/**
 * Fail every fetch-back after the first `ok` ones, as a full data volume does. With
 * `objectsLand`, a failing fetch-back first copies the clone's objects into the bare (to
 * FETCH_HEAD only) and then fails before the tracking ref moves: the ref-update half of the
 * fetch ran out of space. The run's newest commit is then present in the bare but NOT under the
 * tracking ref, so only the positive tracking-ref verify tells the two apart.
 */
function failFetchBackAfter(ok: number, opts: { objectsLand?: boolean } = {}): void {
  const real = git.fetchAgentBranch.bind(git);
  let calls = 0;
  git.fetchAgentBranch = (async (...args: Parameters<typeof real>) => {
    calls += 1;
    if (calls > ok) {
      if (opts.objectsLand) {
        const [barePath, clonePath, branch] = args;
        execFileSync(
          "git",
          ["--git-dir", barePath, "-c", "protocol.file.allow=user", "fetch", "--no-tags", `file://${clonePath}`, `refs/heads/${branch}`],
          { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" }, stdio: "pipe" },
        );
      }
      throw new Error("fatal: unable to write loose object file: No space left on device");
    }
    return await real(...args);
  }) as typeof git.fetchAgentBranch;
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

  it("the #1798 shape: the fetch-back fails, the stale checkpoint is published; reports false and keeps the custody hold", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      const tips = stubPublish();
      // The mid-run checkpoint's fetch-back lands (milestone 1); every later one fails after
      // milestone 2's objects reached the bare but before the tracking ref moved.
      failFetchBackAfter(1, { objectsLand: true });
      const archive = new FakeRecoveryClient();
      const recoveryGit = new FakeRecoveryGit();
      // The REAL coordinator, pin included: the settle runs its full transfer-and-verify.
      const { coord, root } = makeRecoveryCoordinator(archive, recoveryGit);
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
          feed.includes(
            "park checkpoint published to origin, but the latest committed work is not in that checkpoint; the worker keeps it until it is recovered",
          ),
          `the feed names the stale checkpoint, got ${JSON.stringify(feed)}`,
        );
        // The settle's transfer fetch-back failed too: it must not trust the bare (whose objects
        // include milestone 2, but whose tracking ref does not) and must keep the hold open.
        assert.deepStrictEqual(archive.releaseCalls, [], "custody is never released while the latest work is on the worker only");
        assert.deepStrictEqual(archive.reserveCalls, [], "nothing archived from an unverified transfer");
        assert.strictEqual(recoveryGit.produceCalls, 0, "no bundle produced from an unverified head");
      } finally {
        fs.rmSync(root, { recursive: true, force: true });
      }
    });
  });

  it("only the recovery pin fails: the checkpoint holds the latest work, so it reports true and keeps the hold", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      const tips = stubPublish();
      const archive = new FakeRecoveryClient();
      const { coord, root } = makeRecoveryCoordinator(archive, new FakeRecoveryGit());
      // The journal write fails (ENOSPC): recovery.pin's failure result. It does not change what
      // the published checkpoint holds.
      (coord as unknown as { pin: () => Promise<undefined> }).pin = async () => undefined;
      try {
        const { gitlab } = fakeGitlab();
        const claim = gitlabClaim(18090, { wait_on_limit: true, claim_generation: 4 });
        await runnerWith(twoMilestoneFactory(homeRoot, limitEnd), gitlab, undefined, nullLogger(), {
          checkpointIntervalMs: 0,
          recovery: coord,
        }).execute(claim);
        const [park] = reports(claim.run_id, "limit_wait");
        assert.ok(park, "parked");
        assert.strictEqual(tips.length, 1, "the park published a checkpoint");
        assert.strictEqual(park.checkpoint_contains_latest, true, "the fetch-back landed: the checkpoint holds milestone 2");
        assert.ok(statusTexts(claim.run_id).includes("park checkpoint published to origin"));
        assert.deepStrictEqual(archive.releaseCalls, [], "a failed pin never releases custody");
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

  it("a milestone overlay publish after a failed fetch-back does not let the next pause claim the latest work", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      const tips = stubPublish();
      failFetchBackAfter(1);
      // The overlay's default-tip wrapper needs a forge fetch; pack the plain tracking ref instead.
      // The runner still took the overlay (unpinned) publish path.
      const realPack = git.checkpointPack.bind(git);
      git.checkpointPack = async (barePath, branch, _overlay, pinned) => await realPack(barePath, branch, undefined, pinned);
      const { github } = fakeGitHub();
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(18088, {
        repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
      });
      const milestoneThenPause = async (ctx: RunContext): Promise<ExecutorResult> => {
        // The milestone checkpoint (reap:true, overlay) publishes the OLDER tracking tip: its
        // fetch-back of milestone 2 failed.
        await ctx.checkpoint?.({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        const at = { completedCount: 1, total: 2 };
        const parked = await ctx.parkForPause?.(at);
        return parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      };
      await runnerWith(twoMilestoneFactory(homeRoot, milestoneThenPause), gitlab, undefined, nullLogger(), {
        github,
        checkpointIntervalMs: 0,
      }).execute(claim);
      assert.strictEqual(tips.length, 2, "the milestone published, and the pause published again");
      assert.strictEqual(tips[0], tips[1], "both packed the older tracking tip");
      const [park] = reports(claim.run_id, "paused");
      assert.ok(park, "paused");
      assert.strictEqual(park.checkpoint_contains_latest, false);
    });
  });

  it("a second pause with no new commit after a stale publish still reports false", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE];
      const tips = stubPublish();
      failFetchBackAfter(1);
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(18089);
      // The server declines the first pause (acks `running`), so the run keeps going and pauses
      // again with no commit in between; the second one parks.
      let pauses = 0;
      api.onState(claim.run_id, (body) => {
        if (body.status === "paused") pauses += 1;
        api.overrideStateStatus(claim.run_id, body.status === "paused" && pauses === 1 ? "running" : body.status);
      });
      const twicePausing = async (ctx: RunContext): Promise<ExecutorResult> => {
        const at = { completedCount: 1, total: 2 };
        const first = await ctx.parkForPause?.(at);
        assert.strictEqual(first, false, "the first pause was declined");
        const parked = await ctx.parkForPause?.(at);
        return parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      };
      await runnerWith(twoMilestoneFactory(homeRoot, twicePausing), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
      }).execute(claim);
      const parks = reports(claim.run_id, "paused");
      assert.strictEqual(parks.length, 2, "two pause reports");
      assert.deepStrictEqual(
        parks.map((p) => p.checkpoint_contains_latest),
        [false, false],
        "the second pause does not take the already-published shortcut on a tip that was never published",
      );
      assert.strictEqual(tips.length, 2, "the second pause published again (still the older tracking tip)");
      assert.strictEqual(tips[0], tips[1]);
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

// PRD #1809 D8: the overlay (unpinned) publish after a FAILED fetch-back packs the OLDER tracking
// tip, so the flight bookkeeping must name that tip, not the clone HEAD that never landed:
//   - the checkpoint floor C is the tracking tip (a floor at the unpublished clone HEAD would let a
//     later bridge or #1416 steer treat never-published work as durable);
//   - lastPublishedTip stays unset (hasNewWork keeps retrying the clone HEAD);
//   - but a checkpoint DID land, so a report_only completion must still refuse to orphan it (N1).
describe("RunRunner — PRD #1809 D8: an overlay publish after a failed fetch-back", () => {
  /** A GitHub claim (so the milestone checkpoint takes the overlay path) whose overlay pack is
   *  the plain tracking ref: the overlay's default-tip wrapper would need a forge fetch. */
  function overlayClaim(n: number) {
    const realPack = git.checkpointPack.bind(git);
    git.checkpointPack = async (barePath, branch, _overlay, pinned) => await realPack(barePath, branch, undefined, pinned);
    return gitlabClaim(n, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
  }

  /** Milestone 2 is committed but its fetch-back fails; the milestone checkpoint then publishes
   *  the older tracking tip (milestone 1) through the overlay path, then the turn ends with `end`. */
  const milestoneThen =
    (end: (ctx: RunContext) => ExecutorResult) =>
    async (ctx: RunContext): Promise<ExecutorResult> => {
      await ctx.checkpoint?.({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
      return end(ctx);
    };

  it("sets the checkpoint floor to the older tracking tip, not the clone HEAD", async () => {
    await withHomeRoot(async (homeRoot) => {
      const tips = stubPublish();
      failFetchBackAfter(1);
      const { github } = fakeGitHub();
      const { gitlab } = fakeGitlab();
      const claim = overlayClaim(18097);
      const seen: Array<{ cloneTip: string | null; fetchedTip: string | null; lastPublishedTip?: string; checkpointFloor?: string }> =
        [];
      await runnerWith(
        twoMilestoneFactory(
          homeRoot,
          milestoneThen((ctx) => ({ branch: ctx.branch })),
        ),
        gitlab,
        undefined,
        nullLogger(),
        { github, checkpointIntervalMs: 0, checkpointTestHooks: { afterUnpinnedPublish: (s) => seen.push(s) } },
      ).execute(claim);
      assert.strictEqual(tips.length, 1, "the milestone checkpoint published once");
      assert.strictEqual(seen.length, 1, "one unpinned (overlay) publish");
      const s = seen[0];
      assert.ok(s?.fetchedTip && s.cloneTip, "both tips resolved");
      assert.notStrictEqual(s.fetchedTip, s.cloneTip, "the fetch-back failed: the tracking ref is behind the clone HEAD");
      assert.strictEqual(tips[0], s.fetchedTip, "the publish packed the older tracking tip");
      assert.strictEqual(s.checkpointFloor, s.fetchedTip, "the floor is the tip that was published");
      assert.strictEqual(s.lastPublishedTip, undefined, "the clone HEAD was never published");
    });
  });

  it("a report_only completion after that publish is refused, not left to orphan the checkpoint", async () => {
    await withHomeRoot(async (homeRoot) => {
      const tips = stubPublish();
      failFetchBackAfter(1);
      const { github } = fakeGitHub();
      const { gitlab, calls } = fakeGitlab();
      const claim = overlayClaim(18098);
      await runnerWith(
        twoMilestoneFactory(
          homeRoot,
          milestoneThen((ctx) => ({ branch: ctx.branch, reportOnly: true, summary: "verified after milestone m1" })),
        ),
        gitlab,
        undefined,
        nullLogger(),
        { github, checkpointIntervalMs: 0 },
      ).execute(claim);
      assert.strictEqual(tips.length, 1, "the milestone checkpoint landed (the older tracking tip)");
      const [failed] = reports(claim.run_id, "failed");
      assert.ok(failed, "the run failed");
      assert.match(failed.failure_reason ?? "", /report_only/);
      assert.match(failed.failure_reason ?? "", /checkpoint/);
      assert.strictEqual(reports(claim.run_id, "completed").length, 0, "it did not complete report-only");
      assert.strictEqual(calls.length, 0, "no MR opened");
    });
  });

  it("a declined pause that published the older tip also blocks a later report_only completion", async () => {
    await withHomeRoot(async (homeRoot) => {
      const tips = stubPublish();
      failFetchBackAfter(1);
      const { gitlab, calls } = fakeGitlab();
      const claim = gitlabClaim(18099);
      // The server declines the pause (acks `running`), so the run continues past its publish.
      api.onState(claim.run_id, (body) => {
        api.overrideStateStatus(claim.run_id, body.status === "paused" ? "running" : body.status);
      });
      const pauseThenReport = async (ctx: RunContext): Promise<ExecutorResult> => {
        const parked = await ctx.parkForPause?.({ completedCount: 1, total: 2 });
        assert.strictEqual(parked, false, "the pause was declined");
        return { branch: ctx.branch, reportOnly: true, summary: "verified after milestone m1" };
      };
      await runnerWith(twoMilestoneFactory(homeRoot, pauseThenReport), gitlab, undefined, nullLogger(), {
        checkpointIntervalMs: 0,
      }).execute(claim);
      assert.strictEqual(tips.length, 1, "the pause published the older tracking tip");
      const [failed] = reports(claim.run_id, "failed");
      assert.ok(failed, "the run failed");
      assert.match(failed.failure_reason ?? "", /report_only/);
      assert.strictEqual(reports(claim.run_id, "completed").length, 0, "it did not complete report-only");
      assert.strictEqual(calls.length, 0, "no MR opened");
    });
  });
});
