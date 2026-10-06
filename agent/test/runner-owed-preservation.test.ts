import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { Executor } from "../src/executor.js";
import type { FetchAgentBranchOptions, TrackingUpdateResult } from "../src/git.js";
import { LimitReachedError } from "../src/limit.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runner, deferred } from "./runner-harness.js";

installHarness();

function commit(dir: string): void {
  fs.writeFileSync(path.join(dir, "OWED.txt"), "owed work\n");
  execFileSync("git", ["-C", dir, "add", "OWED.txt"]);
  execFileSync("git", ["-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com",
    "-c", "commit.gpgsign=false", "commit", "-m", "owed work"]);
}

function forbidPublication(): { packs: () => number; brokers: () => number } {
  let packs = 0;
  let brokers = 0;
  git.checkpointPack = async () => { packs++; throw new Error("unexpected pack"); };
  client.publishCheckpoint = async () => { brokers++; throw new Error("unexpected broker"); };
  return { packs: () => packs, brokers: () => brokers };
}

for (const reason of ["preservation_failed", "owner_stamp_failed", "receipt_commit_failed", "ownership_unknown"] as const) {
  it(`checkpoint refusal ${reason} stops publication and retries an unchanged tip`, async () => {
    const claim = gitlabClaim(1924);
    claim.claim_generation = 7;
    const { gitlab } = fakeGitlab();
    const publication = forbidPublication();
    let fetches = 0;
    let clone = "";
    let options: FetchAgentBranchOptions | undefined;
    git.fetchAgentBranch = (async (_bare, _clone, _branch, _runId, opts) => {
      fetches++;
      options = opts;
      return { kind: "not_updated", reason };
    }) as typeof git.fetchAgentBranch;
    const exec: Executor = {
      run: async (ctx) => {
        clone = ctx.worktreePath;
        commit(clone);
        await ctx.checkpoint!({ reap: false });
        await ctx.checkpoint!({ reap: false });
        assert.equal(fetches, 2, "refusal must not satisfy the unchanged-tip shortcut");
        assert.equal(publication.packs(), 0);
        assert.equal(publication.brokers(), 0);
        assert.equal(api.states.some(s => s.body.status === "running" && s.body.checkpoint_contains_latest === true), false);
        throw new Error("stop after checkpoint assertions");
      },
    };
    await runner(exec, gitlab).execute(claim);
    assert.equal(fs.existsSync(clone), true, "refused preservation retains the source clone");
    assert.equal(options?.context.generation, 7);
    assert.equal(options?.context.kind, "issue");
    assert.equal(options?.context.defaultIdentity.ref, "refs/remotes/origin/main");
    assert.match(options?.context.defaultIdentity.sha ?? "", /^[0-9a-f]{40}$/);
    assert.equal(options?.remotelyConfirmedSha, undefined);
  });
}

it("same SHA with a lost owner stamp fetches again and never publishes an unowned head", async () => {
  const claim = gitlabClaim(1925);
  claim.claim_generation = 8;
  const { gitlab } = fakeGitlab();
  const publication = forbidPublication();
  const realFetch = git.fetchAgentBranch.bind(git);
  let fetches = 0;
  git.fetchAgentBranch = (async (...args: Parameters<typeof realFetch>): Promise<TrackingUpdateResult> => {
    fetches++;
    if (fetches === 1) {
      const result = await realFetch(...args);
      assert.equal(result.kind, "updated");
      execFileSync("git", ["-C", args[0], "config", "--local", "--unset-all", `uzi-trackowner.${args[2]}.owner`]);
      return { kind: "not_updated", reason: "owner_stamp_failed" };
    }
    return { kind: "not_updated", reason: "ownership_unknown" };
  }) as typeof git.fetchAgentBranch;
  await runner({
    run: async ctx => {
      commit(ctx.worktreePath);
      await ctx.checkpoint!({ reap: false });
      await ctx.checkpoint!({ reap: false });
      assert.equal(fetches, 2, "equal tracking and clone SHAs cannot bypass the missing committed owner");
      assert.equal(publication.packs(), 0);
      assert.equal(publication.brokers(), 0);
      throw new Error("stop");
    },
  }, gitlab).execute(claim);
});

for (const sink of ["finalize", "limit-park", "shutdown"] as const) {
  it(`${sink} refuses preservation without packing stale tracking work`, async () => {
    const claim = gitlabClaim(1926, { wait_on_limit: true });
    const { gitlab } = fakeGitlab();
    const publication = forbidPublication();
    git.fetchAgentBranch = (async (): Promise<TrackingUpdateResult> => ({ kind: "not_updated", reason: "preservation_failed" })) as unknown as typeof git.fetchAgentBranch;
    const started = deferred();
    let clone = "";
    const r = runner({
      run: async ctx => {
        clone = ctx.worktreePath;
        commit(clone);
        if (sink === "limit-park") throw new LimitReachedError({ resetsAtMs: Date.now() + 3600_000 });
        if (sink === "shutdown") {
          started.resolve();
          await new Promise<void>(resolve => ctx.signal!.addEventListener("abort", () => resolve(), { once: true }));
          throw new Error("shutdown");
        }
        return { branch: ctx.branch };
      },
    }, gitlab);
    const execution = r.execute(claim);
    if (sink === "shutdown") {
      await started.promise;
      r.shutdown();
    }
    await execution;
    assert.equal(publication.packs(), 0);
    assert.equal(publication.brokers(), 0);
    assert.equal(fs.existsSync(clone), true);
    assert.equal(api.states.some(s => s.body.status === "completed"), false);
    assert.equal(api.states.some(s => s.body.checkpoint_contains_latest === true), false);
    if (sink === "limit-park") assert.equal(api.states.some(s => s.body.status === "limit_wait"), true);
  });
}

it("ownership becoming unknown after a successful fetch skips the checkpoint and continues the run", async () => {
  const { gitlab } = fakeGitlab();
  const publication = forbidPublication();
  const originalOwnership = git.committedTrackingOwnership.bind(git);
  let proved = 0;
  git.committedTrackingOwnership = async (...args) => {
    const owned = await originalOwnership(...args);
    if (owned.kind === "owned" && ++proved > 1) return { kind: "not_owned" };
    return owned;
  };
  let returned = false;
  await runner({
    run: async ctx => {
      commit(ctx.worktreePath);
      await ctx.checkpoint!({ reap: false });
      returned = true;
      throw new Error("stop");
    },
  }, gitlab).execute(gitlabClaim(1928));
  assert.equal(proved, 2);
  assert.equal(returned, true);
  assert.equal(publication.packs(), 0);
  assert.equal(publication.brokers(), 0);
});

it("an ordinary fetch error remains best effort at a checkpoint", async () => {
  const { gitlab } = fakeGitlab();
  const publication = forbidPublication();
  git.fetchAgentBranch = (async () => { throw new Error("ordinary fetch failure"); }) as typeof git.fetchAgentBranch;
  let returned = false;
  await runner({
    run: async ctx => {
      commit(ctx.worktreePath);
      await ctx.checkpoint!({ reap: false });
      returned = true;
      throw new Error("stop");
    },
  }, gitlab).execute(gitlabClaim(1927));
  assert.equal(returned, true);
  assert.equal(publication.packs(), 0);
  assert.equal(publication.brokers(), 0);
});
