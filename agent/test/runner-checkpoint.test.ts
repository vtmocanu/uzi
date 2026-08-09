import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { type Executor } from "../src/executor.js";
import {
  api,
  client,
  fakeGitlab,
  git,
  gitlabClaim,
  installHarness,
  runner,
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
 *  failure never fails the run. Returns a restore fn (call it in a finally). */
function spyPublish(events: string[], opts: { throws?: boolean } = {}): () => void {
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    ...args: unknown[]
  ) => {
    events.push("publish");
    (args[2] as Readable | undefined)?.resume(); // drain the pack so the git child can exit
    if (opts.throws) throw new Error("boom publish");
    return { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" };
  };
  return () => {
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
  };
}

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
        // Milestone publishes regardless of the gate and sets lastPublish = now() = 0.
        await ctx.checkpoint!({ reap: true, progress });
        commitInClone(ctx.worktreePath, "B.txt");
        fakeNow = 500; // < 1000 after the milestone at t=0
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
});
