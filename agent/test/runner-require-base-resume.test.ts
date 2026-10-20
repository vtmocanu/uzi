import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { type RunContext, type ExecutorResult } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { api, fakeGitlab, fx, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.test"];
const gitIn = (dir: string, args: string[]): string =>
  execFileSync("git", ["-C", dir, ...args], { encoding: "utf8" }).trim();

describe("--require-base on a same-run continuation", () => {
  it("a reclaim adopting this run's own leg-1 commit on the planned base implements (regression: 0124ab39)", async () => {
    const { gitlab } = fakeGitlab();
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const legs: RunContext[] = [];
    let leg = 0;
    const factory: ExecutorFactory = () => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          legs.push(ctx);
          leg++;
          fs.writeFileSync(path.join(ctx.worktreePath, `leg${leg}.txt`), `leg ${leg}\n`);
          gitIn(ctx.worktreePath, ["add", "."]);
          gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", `leg ${leg}`]);
          if (leg === 1) {
            await ctx.checkpoint?.({ reap: false });
            throw new Error("simulated budget pause");
          }
          return { branch: ctx.branch };
        },
      },
    });
    const claim = gitlabClaim(31031, {
      plan_approved: true,
      plan_source: "seeded",
      plan_md: "# Seeded plan\n- do it",
      session_id: null,
      planned_base_commit: planned,
      require_base_match: true,
      claim_generation: 1,
    });
    const r = runnerWith(factory, gitlab);
    await r.execute(claim);
    const resumed = gitlabClaim(31031, { ...claim, claim_generation: 2 });
    await r.execute(resumed).catch(() => undefined);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.equal(legs[0]?.baseCommit, planned, "sanity: leg 1 was cut from the planned commit");
    assert.equal(legs.length, 2, "leg 2 reached the executor");
    assert.notEqual(legs[1]?.baseCommit, planned, "sanity: leg 2 adopted leg 1's work, not the planned commit");
    const refusals = failed.filter((f) => (f.body.failure_reason ?? "").includes("--require-base"));
    assert.deepEqual(refusals, [], "the continuation is not refused as a diverged base");
  });
});
