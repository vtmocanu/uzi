import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { makeClaim } from "./helpers.js";
import { type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { api, fx, fakeGitlab, git, installHarness, runner } from "./runner-harness.js";

installHarness();

// PRD #1416 M2 — the MID-RUN checkpoint divergence detection + safety steer. When the branch's
// history is rewritten at/below the published floor P, the runner emits EXACTLY ONE status line
// and arms EXACTLY ONE worker-authoritative steer per distinct fetched tip, and never blocks a
// git command. These drive a real runner clone through a fake executor that rewrites history, then
// calls ctx.checkpoint (the mid-run fetch-back seam maybeSteerOnDivergence hangs off) and records
// what ctx.pullSafetySteer hands back. Real bare + clone from the runner-harness fixture.

const ISO_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ISO_ENV, encoding: "utf8" }).trim();
}

/** Publish a branch on the fixture origin with a commit of its own, so its tip P is a real forge
 *  tip distinct from `main`. Restores origin's HEAD to `main`. (Mirrors the M1 test's helper.) */
function publishBranch(name: string): string {
  gitIn(fx.originPath, ["checkout", "-b", name]);
  fs.writeFileSync(path.join(fx.originPath, "PUBLISHED.md"), `# published work on ${name}\n`);
  gitIn(fx.originPath, ["add", "PUBLISHED.md"]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", `published commit on ${name}`]);
  const sha = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  gitIn(fx.originPath, ["checkout", "main"]);
  return sha;
}

/** Rewrite the tip commit in place (amend) — a rebase/amend of the commit AT the published tip P,
 *  so the new tip shares P's parent but P is NOT its ancestor: history rewritten BELOW P. */
function rewriteAtTip(treePath: string): string {
  fs.writeFileSync(path.join(treePath, "REWRITE.md"), `rewrite ${randomUUID()}\n`);
  gitIn(treePath, ["add", "REWRITE.md"]);
  gitIn(treePath, [...IDENT, "commit", "--amend", "--no-edit"]);
  return gitIn(treePath, ["rev-parse", "HEAD"]);
}

/** Add a commit ON TOP of the current tip — a fast-forward advance (nothing rewritten). */
function addOnTop(treePath: string, name: string): string {
  fs.writeFileSync(path.join(treePath, name), `new work ${randomUUID()}\n`);
  gitIn(treePath, ["add", name]);
  gitIn(treePath, [...IDENT, "commit", "-m", `add ${name}`]);
  return gitIn(treePath, ["rev-parse", "HEAD"]);
}

interface Obs {
  steers: (string | undefined)[];
}

/** An executor that runs a caller-supplied step callback (which does git ops + checkpoints and
 *  records the drained steer), then returns without a real finalize expectation. */
function stepExecutor(steps: (ctx: RunContext, obs: Obs) => Promise<void>): { executor: Executor; obs: Obs } {
  const obs: Obs = { steers: [] };
  const executor: Executor = {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      await steps(ctx, obs);
      return { branch: ctx.branch };
    },
  };
  return { executor, obs };
}

function taskClaim(branch: string, overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return makeClaim({
    run_id: (overrides.run_id as string | undefined) ?? randomUUID(),
    kind: "task",
    issue_iid: null,
    issue_title: "Handoff: published-branch work",
    issue_description: "Continue the work on the already-published branch.",
    branch,
    open_mr: false,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    secrets: {
      forge_pat: "fixture-forge-pat-000000",
      anthropic_oauth_token: "dummy-oauth-do-not-scan",
    },
    ...overrides,
  });
}

/** The divergence status lines the worker emitted for a run. */
const divergenceStatuses = (runId: string): string[] =>
  api
    .messages(runId)
    .filter(
      (m) =>
        m.kind === "status" &&
        String(m.payload.text).includes("history was rewritten below its published tip"),
    )
    .map((m) => String(m.payload.text));

describe("RunRunner — mid-run divergence detection + safety steer (PRD #1416 M2)", () => {
  it("a rewrite below P triggers EXACTLY ONE status and ONE armed steer on the next checkpoint tick", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/rewritten";
    const P = publishBranch(branch);
    const { executor, obs } = stepExecutor(async (ctx, o) => {
      const tip = rewriteAtTip(ctx.worktreePath);
      assert.notEqual(tip, P, "the amend rewrote the published tip");
      await ctx.checkpoint?.({ reap: false });
      o.steers.push(ctx.pullSafetySteer?.());
    });
    const claim = taskClaim(branch);
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    const statuses = divergenceStatuses(claim.run_id);
    assert.equal(statuses.length, 1, "exactly one divergence status line");
    assert.match(statuses[0]!, new RegExp(P.slice(0, 12)), "the status names the published tip's short sha");
    assert.match(statuses[0]!, /fast-forward only/);
    const armed = obs.steers.filter((s) => s !== undefined);
    assert.equal(armed.length, 1, "exactly one steer was armed");
    assert.match(armed[0]!, /git merge -s ours/, "the steer carries the restore recipe");
    assert.match(armed[0]!, new RegExp(P), "the steer names the published tip P");
  });

  it("a fast-forward advance above P triggers NO status and NO steer", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/fastforward";
    publishBranch(branch);
    const { executor, obs } = stepExecutor(async (ctx, o) => {
      addOnTop(ctx.worktreePath, "ONTOP.md");
      await ctx.checkpoint?.({ reap: false });
      o.steers.push(ctx.pullSafetySteer?.());
    });
    const claim = taskClaim(branch);
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    assert.equal(divergenceStatuses(claim.run_id).length, 0, "no divergence status");
    assert.deepEqual(obs.steers, [undefined], "no steer armed on a fast-forward");
  });

  it("a rewrite WHOLLY ABOVE P (P still an ancestor) triggers nothing", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/above-p";
    publishBranch(branch);
    const { executor, obs } = stepExecutor(async (ctx, o) => {
      addOnTop(ctx.worktreePath, "X.md"); // commit X above P
      rewriteAtTip(ctx.worktreePath); // amend X → X' (P is still an ancestor of X')
      await ctx.checkpoint?.({ reap: false });
      o.steers.push(ctx.pullSafetySteer?.());
    });
    const claim = taskClaim(branch);
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    assert.equal(divergenceStatuses(claim.run_id).length, 0, "a rewrite above P is not divergent");
    assert.deepEqual(obs.steers, [undefined]);
  });

  it("null P (a fresh, never-published branch) is skipped entirely", async () => {
    const { gitlab } = fakeGitlab();
    const { executor, obs } = stepExecutor(async (ctx, o) => {
      rewriteAtTip(ctx.worktreePath); // rewrite the fresh branch's tip
      await ctx.checkpoint?.({ reap: false });
      o.steers.push(ctx.pullSafetySteer?.());
    });
    const claim = taskClaim(`uzi/task/${randomUUID()}`); // never on the forge → P is null
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    assert.equal(divergenceStatuses(claim.run_id).length, 0, "no published floor → nothing to detect");
    assert.deepEqual(obs.steers, [undefined]);
  });

  it("an ancestry 'unknown' (a git error / missing ref) never steers — not coerced to divergent", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/ancestry-unknown";
    publishBranch(branch);
    // Force the tri-state helper to answer "unknown" for this run, exactly as a transient git
    // error would. The rewrite WOULD be divergent, so a coerce-to-divergent bug would steer.
    const orig = git.ancestry.bind(git);
    (git as unknown as { ancestry: unknown }).ancestry = async () => "unknown";
    try {
      const { executor, obs } = stepExecutor(async (ctx, o) => {
        rewriteAtTip(ctx.worktreePath);
        await ctx.checkpoint?.({ reap: false });
        o.steers.push(ctx.pullSafetySteer?.());
      });
      const claim = taskClaim(branch);
      await runner(executor, gitlab).execute(claim).catch(() => undefined);

      assert.equal(divergenceStatuses(claim.run_id).length, 0, "'unknown' is not divergent → no status");
      assert.deepEqual(obs.steers, [undefined]);
    } finally {
      (git as unknown as { ancestry: unknown }).ancestry = orig;
    }
  });

  it("a repeated tick on the SAME diverged tip emits nothing; a NEW rewrite (new tip) emits again", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/repeat-then-new";
    publishBranch(branch);
    const { executor, obs } = stepExecutor(async (ctx, o) => {
      // Tick 1: rewrite → tip T1 → detected.
      rewriteAtTip(ctx.worktreePath);
      await ctx.checkpoint?.({ reap: false });
      o.steers.push(ctx.pullSafetySteer?.());
      // Tick 2: NO rewrite, same tip T1 → deduped, nothing new.
      await ctx.checkpoint?.({ reap: false });
      o.steers.push(ctx.pullSafetySteer?.());
      // Tick 3: a NEW rewrite → tip T2 → detected again.
      rewriteAtTip(ctx.worktreePath);
      await ctx.checkpoint?.({ reap: false });
      o.steers.push(ctx.pullSafetySteer?.());
    });
    const claim = taskClaim(branch);
    await runner(executor, gitlab).execute(claim).catch(() => undefined);

    assert.equal(divergenceStatuses(claim.run_id).length, 2, "one status per distinct diverged tip (T1, T2)");
    assert.equal(obs.steers[0] !== undefined, true, "T1 armed a steer");
    assert.equal(obs.steers[1], undefined, "the repeated tick on T1 armed nothing");
    assert.equal(obs.steers[2] !== undefined, true, "the new tip T2 armed a steer again");
  });
});
