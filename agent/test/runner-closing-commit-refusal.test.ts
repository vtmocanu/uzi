import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { followSuccessfulPush } from "./publication-fixture.js";
import { nullLogger } from "./helpers.js";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { api, fakeGitHub, fakeGitlab as rawFakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

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

const githubClaim = (iid: number) =>
  gitlabClaim(iid, { repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" } });

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

/** StubExecutor plus one extra commit carrying `message`; records that commit's SHA. `file` is the
 *  path the commit writes (a `.github/workflows/` path models a disallowed workflow blob). */
function committing(message: string, obs: { sha?: string }, declare: boolean, file = "closing.txt", extra: Partial<ExecutorResult> = {}): Executor {
  return new class extends StubExecutor {
    override async run(ctx: RunContext): Promise<ExecutorResult> {
      const result = await super.run(ctx);
      fs.mkdirSync(path.dirname(path.join(ctx.worktreePath, file)), { recursive: true });
      fs.writeFileSync(path.join(ctx.worktreePath, file), "closing work\n");
      gitIn(ctx.worktreePath, ["add", file]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", message]);
      obs.sha = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      return { ...result, ...(declare ? { nonClosing: { reason: "Approved follow-up remains" } } : {}), ...extra };
    }
  }(nullLogger());
}

describe("declared non-closing run with a closing commit message (issue #2779)", () => {
  it("M2 stored claim declaration refuses a closing commit after resume without redeclaration", async () => {
    const { gitlab, all } = fakeGitlab();
    const obs: { sha?: string } = {};
    const claim = gitlabClaim(2172, { config: { non_closing: { reason: "Human acceptance pending" } } });
    await runner(committing("Fixes #2172", obs, false), gitlab).execute(claim);
    assert.ok(obs.sha);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.ok((failureReason(claim.run_id) ?? "").includes(obs.sha!));
    assert.equal(all.length, 0);
    assert.equal(originHasBranch(), false);
  });

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
    // A lost push ACK can retry after a publish, so the reason must not claim nothing was pushed.
    assert.ok(reason.includes("publication stopped") && !reason.includes("nothing was pushed"), reason);
    assert.equal(all.length, 0, "no forge request of any kind");
    assert.equal(originHasBranch(), false, "nothing was pushed");
    assert.equal(api.completionHoldRequests.length, 0);
  });

  it("refuses when the commit range cannot be read (null never passes), pushing nothing", async () => {
    const { gitlab, all } = fakeGitlab();
    const obs: { sha?: string } = {};
    const claim = gitlabClaim(2172);
    const original = git.publishedCommitMessages;
    git.publishedCommitMessages = async () => null;
    try {
      await runner(committing("Refs #2172", obs, true), gitlab).execute(claim);
    } finally {
      git.publishedCommitMessages = original;
    }
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.match(failureReason(claim.run_id) ?? "", /could not read the commit messages/);
    assert.equal(all.length, 0);
    assert.equal(originHasBranch(), false, "nothing was pushed");
  });

  it("a GitHub declared run with a closing commit is refused by the early check (no push, no forge call)", async () => {
    const { github, all } = fakeGitHub();
    const obs: { sha?: string } = {};
    const claim = githubClaim(2172);
    // Only the early check runs before the publication preflight, so zero preflight calls pins it.
    let preflights = 0;
    const original = git.scratchPublicationPreflight;
    git.scratchPublicationPreflight = async function (this: unknown, ...args: Parameters<typeof original>) {
      preflights++;
      return original.apply(this as typeof git, args);
    };
    try {
      await runner(committing("Fixes #2172", obs, true), rawFakeGitlab({}).gitlab, undefined, { github }).execute(claim);
    } finally {
      git.scratchPublicationPreflight = original;
    }
    assert.equal(preflights, 0, "refused before any publication preflight");
    const failed = api.states.find(s => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failed);
    assert.ok((failed.body.failure_reason ?? "").includes(obs.sha!));
    assert.notEqual(failed.body.fail_origin, "workflow_scope_missing");
    assert.equal(all.length, 0);
    assert.equal(originHasBranch(), false);
  });

  it("a GitHub run with both a disallowed workflow blob and a closing commit reports workflow_scope_missing", async () => {
    const { github, all } = fakeGitHub();
    const obs: { sha?: string } = {};
    const claim = githubClaim(2172);
    await runner(committing("Fixes #2172", obs, true, ".github/workflows/ci.yml"), rawFakeGitlab({}).gitlab, undefined, { github })
      .execute(claim);
    const failed = api.states.filter(s => s.runId === claim.run_id && s.body.status === "failed");
    assert.equal(failed.length, 1);
    assert.equal(failed[0]!.body.fail_origin, "workflow_scope_missing");
    assert.equal(all.length, 0);
    assert.equal(originHasBranch(), false);
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
  // Legacy APIs retain the #2815 behavior: undeclared partial/capped runs still publish.
  // Interlocked publication uses the fresh projection and is covered by the M3 guard suite.
  it("an owner-partial run (no declaration) with a closing commit message is not refused", async () => {
    const { gitlab } = fakeGitlab();
    const obs: { sha?: string } = {};
    const claim = gitlabClaim(2172, {
      config: { completion_scope: { deferred: [{ milestone_id: "m2", title: "Later", reason: "owner deferred" }] } },
    });
    await runner(committing("Fixes #2172", obs, false), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"), JSON.stringify(statuses(claim.run_id)));
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(originHasBranch(), true);
  });

  it("a scope-capped run (no declaration) with a closing commit message is not refused", async () => {
    const { gitlab } = fakeGitlab();
    const obs: { sha?: string } = {};
    const claim = gitlabClaim(2172);
    await runner(committing("Fixes #2172", obs, false, "closing.txt", { scopeCapped: { completedCount: 1, total: 2 } }), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"), JSON.stringify(statuses(claim.run_id)));
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(originHasBranch(), true);
  });
});
