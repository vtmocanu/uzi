import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { type Executor } from "../src/executor.js";
import type { Logger } from "../src/log.js";
import type { PublishResult } from "../src/protocol.js";
import {
  api,
  client,
  fakeGitlab,
  git,
  gitlabClaim,
  installHarness,
  runner,
  runnerWith,
} from "./runner-harness.js";

installHarness();

// The checkpoint callback (PRD #122 M6) is constructed inside RunRunner.execute and handed
// to the executor as ctx.checkpoint. These tests drive it through a CUSTOM executor that
// commits in the (real) runner clone, then calls ctx.checkpoint, so the whole path —
// Decision 6 no-op rejection, reap-before-git ordering, and the milestone report — is
// exercised against a real fixture bare + clone with no live SDK.

const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

/** Commit a new file in the runner clone so its branch tip advances. */
function commitInClone(dir: string, file: string): void {
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  fs.writeFileSync(path.join(dir, file), `${file}\n`);
  execFileSync("git", ["-C", dir, "add", file], { env });
  execFileSync("git", ["-C", dir, ...IDENT, "commit", "-m", `work ${file}`], { env });
}

/** Spy on git.fetchAgentBranch, recording a "fetch" event and calling through. Returns a
 *  restore fn (call it in a finally). */
function spyFetch(events: string[]): () => void {
  const orig = git.fetchAgentBranch.bind(git);
  (git as unknown as { fetchAgentBranch: unknown }).fetchAgentBranch = async (
    ...args: unknown[]
  ) => {
    events.push("fetch");
    return (orig as (...a: unknown[]) => Promise<string>)(...args);
  };
  return () => {
    (git as unknown as { fetchAgentBranch: unknown }).fetchAgentBranch = orig;
  };
}

/** Spy on client.publishCheckpoint (PRD #122 M8), recording a "publish" event. Drains the
 *  real pack stream so pack-objects can exit, and optionally throws to prove a publish
 *  failure never fails the run. `nullFirst` returns a non-2xx PublishResult (the client's
 *  `{ ok: false, httpStatus }` contract for an UNCONFIRMED publish, issue #1030) for the
 *  first N calls before succeeding, so a test can drive a fail-once-then-succeed broker
 *  without throwing (PRD #267 Fix 1). Returns a restore fn (call it in a finally). */
function spyPublish(
  events: string[],
  opts: { throws?: boolean; nullFirst?: number } = {},
): () => void {
  const orig = client.publishCheckpoint.bind(client);
  let calls = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    ...args: unknown[]
  ) => {
    events.push("publish");
    (args[2] as Readable | undefined)?.resume(); // drain the pack so the git child can exit
    if (opts.throws) throw new Error("boom publish");
    calls += 1;
    // A non-2xx result models an UNCONFIRMED publish: it did NOT confirmably land.
    if (opts.nullFirst && calls <= opts.nullFirst) return { ok: false, httpStatus: 500 };
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return () => {
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
  };
}

/** A Logger that records every `info` message string (across children, which share the
 *  same list). Used to prove the PRD #267 M3 time-based-publish signal is emitted. */
function capturingLogger(): { logger: Logger; infos: string[] } {
  const infos: string[] = [];
  const self: Logger = {
    debug() {},
    info: (msg) => {
      infos.push(msg);
    },
    warn() {},
    error() {},
    addSecret() {},
    removeSecret() {},
    child: () => self,
  };
  return { logger: self, infos };
}

const TIME_BASED_MSG = "checkpoint published to origin (time-based)";

describe("RunRunner — milestone checkpoint (PRD #122 M6)", () => {
  it("reap:true reaps the agent tree BEFORE the fetch-back (reap-before-git, B1 order)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(60);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    const restore = spyFetch(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    // The cooperative checkpoint reaps, then fetches — in that order.
    assert.deepStrictEqual(afterCheckpoint, ["kill", "fetch"]);
  });

  it("reap:false fetches back WITHOUT reaping (the iteration-boundary fallback)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(61);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    const restore = spyFetch(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: false, progress: { completed: ["m1"], in_progress: [] } });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    // Fallback: a fetch, but NO reap — a backgrounded dev server must survive.
    assert.deepStrictEqual(afterCheckpoint, ["fetch"]);
  });

  it("skips the fetch when the branch tip is unmoved since the last checkpoint (Decision 6 no-op)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(62);
    const events: string[] = [];
    let afterSecond: string[] = [];
    const restore = spyFetch(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // First checkpoint fetches and writes the tracking ref.
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        const beforeSecond = events.length;
        // Second checkpoint with NO new commit: the tip is unmoved, so it is a no-op.
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        afterSecond = events.slice(beforeSecond);
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    // The no-op check runs BEFORE the reap, so an unmoved tip does neither: no kill, no fetch.
    assert.deepStrictEqual(afterSecond, []);
  });

  it("reports the checkpointed milestone as a running report (no iteration_count)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(63);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({
          reap: true,
          progress: { completed: ["m1"], in_progress: ["m2"] },
        });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    await runner(exec, gitlab).execute(claim);

    const report = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((b) => b.status === "running" && Array.isArray(b.milestones_completed));
    assert.ok(report, "a running report carried the checkpointed milestone progress");
    assert.deepStrictEqual(report!.milestones_completed, ["m1"]);
    assert.deepStrictEqual(report!.milestones_in_progress, ["m2"]);
    assert.ok(
      !("iteration_count" in report!),
      "the checkpoint report omits iteration_count so it cannot regress the GREATEST-merged counter",
    );
  });

  it("reap:true brokers the checkpoint pack to origin, strictly AFTER the reap and fetch-back (PRD #122 M8)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(64);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // Reap, then fetch-back, then the brokered publish — publish is credentialed-class and
    // must stay after the reap and the tracking-ref update the fetch-back writes.
    assert.deepStrictEqual(afterCheckpoint, ["kill", "fetch", "publish"]);
  });

  it("reap:false does NOT publish (the iteration-boundary fallback fetches only)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(65);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: false, progress: { completed: ["m1"], in_progress: [] } });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.deepStrictEqual(afterCheckpoint, ["fetch"]);
  });

  it("a thrown publish error is swallowed — the run still completes and reports state (best-effort)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(66);
    const events: string[] = [];
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events, { throws: true });
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      // Must NOT reject even though publishCheckpoint throws.
      await runner(exec, gitlab).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.ok(events.includes("publish"), "the publish was attempted");
    const completed = api.states.some(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    );
    assert.ok(completed, "the run completed despite the publish failure");
  });
});

// PRD #267: the reap:false iteration-boundary checkpoint may now publish to origin on a
// time budget, WITHOUT reaping the agent tree. These tests drive the checkpoint callback
// through a custom executor that commits then calls ctx.checkpoint({reap:false}), with an
// injected fake clock and a short (1000ms) interval so the time-gate is deterministic.
describe("RunRunner — time-based checkpoint (PRD #267)", () => {
  const progress = { completed: [], in_progress: ["m1"] };

  it("reap:false past the interval with a moved tip publishes AND does zero killAgentTree", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(70);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 1001; // cross the 1000ms interval since run start (lastPublish=0)
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // A moved tip fetches, then the open time-gate publishes — and reap:false never kills.
    assert.ok(!afterCheckpoint.includes("kill"), "reap:false did NOT kill the agent tree");
    assert.deepStrictEqual(afterCheckpoint, ["fetch", "publish"]);
  });

  it("reap:false before the interval does not publish", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(71);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 500; // still inside the interval
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // Tip moved so it fetches, but the gate is closed — no publish.
    assert.deepStrictEqual(afterCheckpoint, ["fetch"]);
  });

  it("idle-commit regression (Decision 9): a commit that goes idle for >= the interval still publishes exactly once", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(72);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // Interval closed at fakeNow=0: fetch happens, no publish yet.
        await ctx.checkpoint!({ reap: false, progress });
        assert.ok(!events.includes("publish"), "first checkpoint did not publish (gate closed)");
        // No new commit; the tip is now unmoved since the last fetch. Advancing past the
        // interval must STILL publish — the gate keys on lastPublishedTip, not the fetch-skip.
        fakeNow = 1001;
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    const publishes = afterCheckpoint.filter((e) => e === "publish").length;
    const fetches = afterCheckpoint.filter((e) => e === "fetch").length;
    assert.equal(publishes, 1, "the idle commit shipped exactly once");
    assert.equal(fetches, 1, "the second checkpoint skipped the fetch (tip unmoved) but still published");
  });

  it("nothing new since last publish publishes nothing", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(73);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 1001;
        await ctx.checkpoint!({ reap: false, progress }); // publishes tip
        // No new commit; a later checkpoint sees cloneTip === lastPublishedTip.
        fakeNow = 3000;
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    const publishes = afterCheckpoint.filter((e) => e === "publish").length;
    assert.equal(publishes, 1, "an already-published tip is not re-published");
  });

  it("CHECKPOINT_INTERVAL=0 disables the time path", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(74);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 999_999; // arbitrarily far past any interval
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 0, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // interval 0 => the time path never publishes, however much time elapsed.
    assert.deepStrictEqual(afterCheckpoint, ["fetch"]);
  });

  it("a milestone (reap:true) publish resets the gate so a following reap:false under the interval does not publish", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(75);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "A.txt");
        // Advance the clock to a NONZERO time BEFORE the milestone so the gate reset moves
        // lastPublish to a distinct value (5000). If the milestone did NOT reset the gate,
        // the reap:false at 5500 below would see 5500 - 0 >= 1000 (gate open) and publish B —
        // so the publishes==1 assertion genuinely exercises the reset, not "never moved".
        fakeNow = 5000;
        await ctx.checkpoint!({ reap: true, progress });
        commitInClone(ctx.worktreePath, "B.txt");
        fakeNow = 5500; // < 1000 after the milestone reset at t=5000
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    const publishes = afterCheckpoint.filter((e) => e === "publish").length;
    assert.equal(publishes, 1, "only the milestone published; the gate reset blocks the next one");
    assert.ok(afterCheckpoint.includes("kill"), "the milestone (reap:true) reaped the agent tree");
  });

  it("publishes at most once per interval even with new commits each iteration", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(76);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "A.txt");
        fakeNow = 1001; // crosses the interval => publishes, lastPublish := 1001
        await ctx.checkpoint!({ reap: false, progress });
        commitInClone(ctx.worktreePath, "B.txt");
        fakeNow = 1400; // 399 since the last publish => gated
        await ctx.checkpoint!({ reap: false, progress });
        commitInClone(ctx.worktreePath, "C.txt");
        fakeNow = 1900; // 899 since the last publish => still gated
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    const publishes = afterCheckpoint.filter((e) => e === "publish").length;
    assert.equal(publishes, 1, "at most one origin publish per interval per run");
  });

  it("logs the time-based-publish signal when a reap:false checkpoint publishes past the interval (PRD #267 M3)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(77);
    const events: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const { logger, infos } = capturingLogger();
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const executor: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 1001; // cross the 1000ms interval since run start
        await ctx.checkpoint!({ reap: false, progress });
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runnerWith(() => ({ executor }), gitlab, undefined, logger, {
        checkpointIntervalMs: 1000,
        now,
      }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.ok(events.includes("publish"), "the time-based checkpoint published");
    assert.ok(
      infos.some((m) => m.includes(TIME_BASED_MSG)),
      "a reap:false publish past the interval logs the time-based signal",
    );
  });

  it("a milestone (reap:true) publish does NOT log the time-based signal (PRD #267 M3)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(78);
    const events: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const { logger, infos } = capturingLogger();
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const executor: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress });
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runnerWith(() => ({ executor }), gitlab, undefined, logger, {
        checkpointIntervalMs: 1000,
        now,
      }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.ok(events.includes("publish"), "the milestone checkpoint published");
    assert.ok(
      !infos.some((m) => m.includes(TIME_BASED_MSG)),
      "the milestone publish is visible via its running report, not the time-based line",
    );
  });

  it("a FAILED time-based publish does NOT strand the tip — the idle commit ships on retry (PRD #267 Fix 1)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(79);
    const events: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    // The FIRST publish returns a non-2xx PublishResult (the client's { ok: false,
    // httpStatus } contract — an UNCONFIRMED publish); every later publish succeeds. A
    // failed publish must NOT advance
    // lastPublishedTip, so the SAME tip stays publish-eligible next interval.
    const restorePublish = spyPublish(events, { nullFirst: 1 });
    const publishesAtEachStage: number[] = [];
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // t=1001: gate open, tip moved → publishes, but the broker fails (null). lastPublish
        // advances to 1001 (bounds retry cadence) yet lastPublishedTip does NOT.
        fakeNow = 1001;
        await ctx.checkpoint!({ reap: false, progress });
        publishesAtEachStage.push(events.filter((e) => e === "publish").length);
        // NO new commit. One full interval later the same idle tip must be retried and this
        // time land — the "worst-case loss ~one interval" guarantee (Decision 9).
        fakeNow = 2002;
        await ctx.checkpoint!({ reap: false, progress });
        publishesAtEachStage.push(events.filter((e) => e === "publish").length);
        // A THIRD interval later, with the tip now CONFIRMED published, there is nothing new
        // to ship — proving the second publish actually advanced lastPublishedTip.
        fakeNow = 3003;
        await ctx.checkpoint!({ reap: false, progress });
        publishesAtEachStage.push(events.filter((e) => e === "publish").length);
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // 1 (failed) → 2 (retry lands) → 2 (nothing new): the failed publish did not strand the
    // tip, and the successful retry then stopped it re-publishing.
    assert.deepStrictEqual(
      publishesAtEachStage,
      [1, 2, 2],
      "a failed publish leaves the tip publish-eligible; the retry lands then stops re-publishing",
    );
  });

  it("no running report on a pure-idle checkpoint — tip unmoved AND nothing published (PRD #267 Fix 2)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(80);
    const events: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    let idleReportDelta = -1;
    let idleEvents: string[] = [];
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // First checkpoint at a CLOSED gate (t=0): the tip moved, so it fetches and emits a
        // running report — but does not publish.
        await ctx.checkpoint!({ reap: false, progress });
        const beforeStates = api.states.filter((s) => s.runId === claim.run_id).length;
        const beforeEvents = events.length;
        // Second checkpoint: NO new commit and the gate still closed → pure idle. It must do
        // nothing: no fetch, no publish, and — restoring the pre-M1 early-return — no report.
        await ctx.checkpoint!({ reap: false, progress });
        idleReportDelta =
          api.states.filter((s) => s.runId === claim.run_id).length - beforeStates;
        idleEvents = events.slice(beforeEvents);
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.deepStrictEqual(idleEvents, [], "a pure-idle checkpoint neither fetches nor publishes");
    assert.equal(idleReportDelta, 0, "a pure-idle checkpoint emits no running report");
  });
});

// issue #1030: a checkpoint-publish failure or best-effort SKIP must be VISIBLE on the run
// feed rather than silently swallowed. These drive the mid-run (reap:true milestone) publish
// through a spy returning a FIXED PublishResult, then assert the emitted status feed lines,
// the deduping, and that a non-published outcome leaves the tip publish-eligible (so the next
// checkpoint retries — the interval-retry behaviour, exercised here by a second checkpoint).
describe("RunRunner — checkpoint-publish outcome is visible on the feed (issue #1030)", () => {
  /** Spy on client.publishCheckpoint returning a FIXED PublishResult, draining the pack so
   *  the git child exits and counting calls. Returns [restore, count]. */
  function spyPublishResult(result: PublishResult): { restore: () => void; count: () => number } {
    const orig = client.publishCheckpoint.bind(client);
    let calls = 0;
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      ...args: unknown[]
    ) => {
      calls += 1;
      (args[2] as Readable | undefined)?.resume(); // drain the pack so pack-objects can exit
      return result;
    };
    return {
      restore: () => {
        (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
      },
      count: () => calls,
    };
  }

  const statusTexts = (runId: string): string[] =>
    api
      .messages(runId)
      .filter((m) => m.kind === "status")
      .map((m) => String(m.payload.text));

  it("a 2xx {published:false, skipped:workflow_scope} does not advance the tip and emits ONE deduped skip line", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(90);
    const { restore, count } = spyPublishResult({
      ok: true,
      body: { published: false, ref: "", skipped: "workflow_scope" },
    });
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // First milestone checkpoint publishes; the server SKIPS (workflow_scope), so the
        // tip is NOT marked published.
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        // No new commit. Because the skip left lastPublishedTip un-advanced, the SAME tip is
        // still "new work", so the next checkpoint RE-ATTEMPTS the publish — the mid-run proxy
        // for the ~20-min interval retry.
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    assert.equal(count(), 2, "the skip left the tip publish-eligible, so the second checkpoint retried");
    const skips = statusTexts(claim.run_id).filter((t) =>
      t.includes("checkpoint publish skipped: workflow_scope"),
    );
    assert.equal(
      skips.length,
      1,
      `exactly one deduped skip feed line across two identical attempts, got ${JSON.stringify(statusTexts(claim.run_id))}`,
    );
  });

  it("a non-2xx (HTTP 500) does not advance the tip and emits ONE deduped feed line naming the status", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(91);
    const { restore, count } = spyPublishResult({ ok: false, httpStatus: 500 });
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // Two identical failing attempts on the same tip (the HTTP 500 does not advance it).
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    assert.equal(count(), 2, "the HTTP 500 left the tip publish-eligible, so the second checkpoint retried");
    const failures = statusTexts(claim.run_id).filter((t) =>
      t.includes("checkpoint publish failed: HTTP 500"),
    );
    assert.equal(
      failures.length,
      1,
      `two identical failing attempts produce exactly one feed line, got ${JSON.stringify(statusTexts(claim.run_id))}`,
    );
  });

  it("a successful publish emits NO failure/skip feed line", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(92);
    const { restore } = spyPublishResult({
      ok: true,
      body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" },
    });
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    assert.ok(
      !statusTexts(claim.run_id).some((t) => /checkpoint publish (failed|skipped)/.test(t)),
      "a confirmed publish is silent on the failure/skip channel",
    );
  });
});

describe("RunRunner — report_only after a checkpoint is refused (issue #299)", () => {
  // This pins the SAME-WORKER leg of the union guard: a run that publishes a checkpoint
  // mid-run (lastPublishedTip advances on the confirmed publish) and THEN declares
  // report_only must FAIL, because a report-only completion opens no branch/MR and would
  // orphan the published refs/uzi-checkpoints/<branch>. hasCommittedCheckpoint is NOT stubbed
  // here (the fixture bare carries no mirrored checkpoint ref — the publish went through
  // the client spy), so the guard trips on lastPublishedTip alone.
  it("fails a report_only completion after this worker published a checkpoint", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(67);
    const events: string[] = [];
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch, reportOnly: true, summary: "verified after milestone m1" };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.ok(events.includes("publish"), "the mid-run checkpoint published (sanity)");
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    )!.body;
    assert.match(failed.failure_reason ?? "", /report_only/);
    assert.match(failed.failure_reason ?? "", /checkpoint/);
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    );
    assert.strictEqual(completed, undefined, "the run FAILED, it did not complete report-only");
    assert.strictEqual(calls.length, 0, "no MR opened after the guarded report-only run");
  });
});
