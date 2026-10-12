import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { followSuccessfulPush } from "./publication-fixture.js";
import { nullLogger } from "./helpers.js";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { api, fakeGitlab as rawFakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

// Issue #2779 — a declared non-closing issue run must not publish a commit message that closes its
// issue: the refusal happens before ANY push or forge call, names the offending commit SHA(s), and
// leaves the run failed. Driven through the REAL RunRunner over the real fixture origin.

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-2172";

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

function fakeGitlab() {
  const forge = rawFakeGitlab({});
  followSuccessfulPush(git, forge.pr, true);
  return forge;
}

const statuses = (runId: string): string[] => api.states.filter(s => s.runId === runId).map(s => s.body.status);
const failureReason = (runId: string): string | undefined =>
  api.states.find(s => s.runId === runId && s.body.status === "failed")?.body.failure_reason;

function originHasBranch(): boolean {
  try {
    gitIn(fx.originPath, ["rev-parse", "--verify", "--quiet", `refs/heads/${BRANCH}`]);
    return true;
  } catch {
    return false;
  }
}

/** StubExecutor plus one extra commit carrying `message`; records that commit's SHA. */
function committing(message: string, obs: { sha?: string }, declare: boolean): Executor {
  return new class extends StubExecutor {
    override async run(ctx: RunContext): Promise<ExecutorResult> {
      const result = await super.run(ctx);
      fs.writeFileSync(path.join(ctx.worktreePath, "closing.txt"), "closing work\n");
      gitIn(ctx.worktreePath, ["add", "closing.txt"]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", message]);
      obs.sha = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      return { ...result, ...(declare ? { nonClosing: { reason: "Approved follow-up remains" } } : {}) };
    }
  }(nullLogger());
}

describe("declared non-closing run with a closing commit message (issue #2779)", () => {
  it("refuses before any push or forge call and names the offending commit", async () => {
    const { gitlab, all } = fakeGitlab();
    const obs: { sha?: string } = {};
    const claim = gitlabClaim(2172);
    await runner(committing("Fixes #2172\n\nbody", obs, true), gitlab).execute(claim);
    assert.ok(obs.sha);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.ok(!statuses(claim.run_id).includes("completed"));
    const reason = failureReason(claim.run_id) ?? "";
    assert.ok(reason.includes(obs.sha!), `failure_reason names the full commit SHA: ${reason}`);
    assert.ok(!reason.includes("Fixes"), "the reason carries no commit text");
    assert.equal(all.length, 0, "no forge request of any kind");
    assert.equal(originHasBranch(), false, "nothing was pushed");
    assert.equal(api.completionHoldRequests.length, 0);
  });

  it("without a declaration the same commit delivers and the PR closes the issue", async () => {
    const { gitlab, pr } = fakeGitlab();
    const obs: { sha?: string } = {};
    const claim = gitlabClaim(2172);
    await runner(committing("Fixes #2172", obs, false), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.match(pr.description, /Closes #2172/);
    assert.equal(originHasBranch(), true);
  });

  it("a declared run whose commits do not close the issue still completes", async () => {
    const { gitlab } = fakeGitlab();
    const obs: { sha?: string } = {};
    const claim = gitlabClaim(2172);
    await runner(committing("Refs #2172", obs, true), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
  });

  it("the in-push check refuses a bridged candidate whose published floor commit closes the issue", async () => {
    // P: a published tip on the branch whose commit closes the issue. The agent rewrites it (the
    // reworded H carries no directive, so the early check passes); the finalize bridge then merges
    // P back in as a floor and the bridged candidate would publish P's closing message.
    gitIn(fx.originPath, ["checkout", "-b", BRANCH]);
    fs.writeFileSync(path.join(fx.originPath, "PUBLISHED.md"), "published\n");
    gitIn(fx.originPath, ["add", "PUBLISHED.md"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "Fixes #2172"]);
    const floor = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    gitIn(fx.originPath, ["checkout", "main"]);
    const { gitlab, all } = fakeGitlab();
    const executor: Executor = {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.writeFileSync(path.join(ctx.worktreePath, "REWRITE.md"), "rewrite\n");
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "--amend", "-m", "reworded work"]);
        return { branch: ctx.branch, nonClosing: { reason: "Approved follow-up remains" } };
      },
    };
    const claim = gitlabClaim(2172);
    await runner(executor, gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"), JSON.stringify(statuses(claim.run_id)));
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.ok((failureReason(claim.run_id) ?? "").includes(floor), "names the floor commit");
    assert.equal(gitIn(fx.originPath, ["rev-parse", `refs/heads/${BRANCH}`]), floor, "origin branch untouched");
    assert.equal(all.length, 0);
  });
});
