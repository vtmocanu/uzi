import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { type Executor } from "../src/executor.js";
import {
  api,
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
});
