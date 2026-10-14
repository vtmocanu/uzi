import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimConfig } from "../src/protocol.js";
import { client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, assertCustodyRetained, command, commit, failingWork, heldServer, skipCheckpoints,
  type Work,
} from "./runner-held-publication-fixture.js";

installHarness();

// Issue #2865: guarded (default-branch CI repair) runs check the exact candidate SHA against the
// protected-path globs in the worker before every checkpoint upload and every held upload. The
// server broker must skip subtrees absent from the pack, so it cannot see a merge whose second
// parent is an older default commit carrying a permissive workflow tree; the worker's merge-base
// diff does. Every repo is a throwaway temp repo; paths are string data.

const GUARD: ClaimConfig = { ci_config_guard: true, ci_config_paths: ["deploy/ci.yml"] };
const WORKFLOW = ".github/workflows/ci.yml";
const ID = ["-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"];

/** Origin history: an older commit with a permissive workflow, then a newer one with the strict one. */
function seedWorkflowHistory(): string {
  const old = commit(fx.originPath, WORKFLOW, "permissive\n");
  commit(fx.originPath, WORKFLOW, "strict\n");
  return old;
}

/** Author a merge whose second parent is `old` and whose tree restores the old workflow file. */
function mergeRestoringOld(clone: string, old: string): string {
  fs.writeFileSync(path.join(clone, WORKFLOW), "permissive\n");
  command(clone, ["add", WORKFLOW]);
  const tree = command(clone, ["write-tree"]);
  const merge = execFileSync("git", ["-C", clone, ...ID, "commit-tree", tree, "-p", "HEAD", "-p", old, "-m", "merge old"],
    { encoding: "utf8" }).trim();
  command(clone, ["update-ref", "HEAD", merge]);
  command(clone, ["reset", "-q", "--hard", "HEAD"]);
  return merge;
}

function spyPublish(): { uploads: number } {
  const seen = { uploads: 0 };
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk.length);
    seen.uploads++;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return seen;
}

let iid = 6500;
async function checkpointRun(config: ClaimConfig | undefined, work: (ctx: RunContext) => Promise<void> | void) {
  const seen = spyPublish();
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(iid++, config ? { config } : {});
  let continued = false;
  const exec: Executor = {
    run: async (ctx) => {
      await work(ctx);
      await ctx.checkpoint!({ reap: false });
      continued = true;
      return { branch: ctx.branch };
    },
  };
  await runner(exec, gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 }).execute(claim);
  return { seen, claim, continued: () => continued };
}

describe("CI-config guard: checkpoint publication", () => {
  it("does not upload a merge whose second parent restores an older workflow tree, and the run continues", async () => {
    const old = seedWorkflowHistory();
    const { seen, continued } = await checkpointRun(GUARD, ctx => { mergeRestoringOld(ctx.worktreePath, old); });
    assert.equal(seen.uploads, 0, "no checkpoint upload for a protected candidate");
    assert.equal(continued(), true, "the checkpoint skip does not stop the executor");
  });

  it("does not upload a checkpoint that touches a floor path directly", async () => {
    const { seen } = await checkpointRun(GUARD, ctx => { commit(ctx.worktreePath, ".forgejo/workflows/x.yml"); });
    assert.equal(seen.uploads, 0);
  });

  it("does not upload when the diff cannot be computed", async (t) => {
    const { seen } = await checkpointRun(GUARD, ctx => {
      commit(ctx.worktreePath, "src/ok.ts");
      t.mock.method(git, "changedFilesNoRenames", async () => null);
    });
    t.mock.restoreAll();
    assert.equal(seen.uploads, 0);
  });

  it("uploads a clean checkpoint of a guarded run", async () => {
    const { seen } = await checkpointRun(GUARD, ctx => { commit(ctx.worktreePath, "src/ok.ts"); });
    assert.equal(seen.uploads, 1);
  });

  it("an unguarded run touching a workflow path still uploads", async () => {
    const { seen } = await checkpointRun(undefined, ctx => { commit(ctx.worktreePath, ".forgejo/workflows/x.yml"); });
    assert.equal(seen.uploads, 1);
  });
});

describe("CI-config guard: held publication", () => {
  async function held(extra: (ctx: RunContext) => Promise<void> | void) {
    const claim = gitlabClaim(iid++, { claim_generation: 50, inventory_guarded: true, config: GUARD });
    const { gitlab } = fakeGitlab();
    const server = heldServer(claim);
    skipCheckpoints();
    const work: Partial<Work> = {};
    const r = runner(failingWork(work, extra), gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
    await r.execute(claim);
    return { claim, server, work };
  }

  it("a protected change is not uploaded and the work stays archived", { skip: HELD_LINUX_ONLY }, async () => {
    const { claim, server, work } = await held(ctx => { commit(ctx.worktreePath, ".forgejo/workflows/x.yml"); });
    assert.equal(server.heldCalls.length, 0);
    assert.equal(server.archiveUploads(), 1);
    await assertCustodyRetained(server, claim, work, "protected held change");
  });

  it("a merge restoring an older workflow tree is not uploaded", { skip: HELD_LINUX_ONLY }, async () => {
    const old = seedWorkflowHistory();
    const { server } = await held(ctx => { mergeRestoringOld(ctx.worktreePath, old); });
    assert.equal(server.heldCalls.length, 0);
    assert.equal(server.archiveUploads(), 1);
  });

  it("an uncomputable diff is not uploaded", { skip: HELD_LINUX_ONLY }, async () => {
    const real = git.changedFilesNoRenames.bind(git);
    let failed = false;
    git.changedFilesNoRenames = async (...a) => (failed ? null : real(...a));
    try {
      const { server } = await held(() => { failed = true; });
      assert.equal(server.heldCalls.length, 0);
    } finally {
      git.changedFilesNoRenames = real;
    }
  });

  it("a clean held change is uploaded", { skip: HELD_LINUX_ONLY }, async () => {
    const { server } = await held(() => {});
    assert.equal(server.heldCalls.length, 1);
  });
});
