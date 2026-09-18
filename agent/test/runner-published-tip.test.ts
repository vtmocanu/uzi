import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { makeClaim } from "./helpers.js";
import { type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { api, fx, fakeGitlab, installHarness, runner } from "./runner-harness.js";

installHarness();

// PRD #1416 M1: at claim, after the clone, the runner records the branch's PUBLISHED FLOOR P
// (`originBranchTip(bare, branch)`, fact 2) as runner-level flight state and threads it into the
// RunContext, so both prompt builders can name it on a run whose branch is already published on
// the forge. P is deliberately NOT `RunnerClone.baseCommit` (which can point at unpublished
// recovered work) and is re-established at each claim from the restart-surviving origin mirror —
// so an executor/session restart (the run picked up again) never leaves the prompt without it.
//
// These mirror the base-commit-across-resume pattern in runner-usage-limit-park.test.ts: a
// capturing executor records the ctx it is handed, and the assertions read what reached it. P is
// the only flight field threaded to ctx in M1 (checkpointFloor C is seeded to P on the same
// phaseClone line and becomes observable when M2/M3 thread it), so these assert P via ctx and the
// underlying `originBranchTip` source both floors share.

const ISO_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};

/** Publish a branch on the fixture origin with a commit of its own, so its tip is a real
 *  forge tip distinct from `main` and `originBranchTip` has something to read after the clone.
 *  Restores origin's HEAD to `main` so default-branch resolution is unperturbed. */
function publishBranch(name: string): string {
  const g = (args: string[]): string =>
    execFileSync("git", ["-C", fx.originPath, ...args], { env: ISO_ENV, encoding: "utf8" });
  g(["checkout", "-b", name]);
  fs.writeFileSync(path.join(fx.originPath, "PUBLISHED.md"), `# published work on ${name}\n`);
  g(["add", "PUBLISHED.md"]);
  g(["commit", "-m", `published commit on ${name}`]);
  const sha = g(["rev-parse", "HEAD"]).trim();
  g(["checkout", "main"]);
  return sha;
}

/** An executor that records every ctx handed to it and returns without committing — enough to
 *  observe what the runner threaded, whatever finalize then does. */
function capturingExecutor(seen: RunContext[]): Executor {
  return {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      seen.push(ctx);
      return { branch: ctx.branch };
    },
  };
}

function taskClaim(branch: string, overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  const runId = (overrides.run_id as string | undefined) ?? randomUUID();
  return makeClaim({
    run_id: runId,
    kind: "task",
    issue_iid: null,
    issue_title: "Handoff: published-branch work",
    issue_description: "Continue the work on the already-published branch.",
    branch,
    open_mr: false,
    repo: {
      id: "r1",
      url: "https://gitlab.example.test/org/repo",
      clone_url: fx.originPath,
    },
    last_seq: 0,
    secrets: {
      forge_pat: "fixture-forge-pat-000000",
      anthropic_oauth_token: "dummy-oauth-do-not-scan",
    },
    ...overrides,
  });
}

describe("RunRunner — published floor P recorded at claim (PRD #1416 M1)", () => {
  it("records P from originBranchTip and threads it to the executor's ctx for a published branch", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/published";
    const pubSha = publishBranch(branch);
    const seen: RunContext[] = [];
    const claim = taskClaim(branch);
    await runner(capturingExecutor(seen), gitlab).execute(claim).catch(() => undefined);

    assert.equal(seen.length, 1, "the executor ran once and captured its ctx");
    assert.equal(
      seen[0]!.publishedTip,
      pubSha,
      "ctx.publishedTip is the branch's forge tip at claim (P), read from originBranchTip",
    );
    // The run reported at least once (the flight/report path ran with P recorded on it).
    assert.ok(api.states.some((s) => s.runId === claim.run_id));
  });

  it("leaves publishedTip undefined for a branch that is NOT published (a fresh run)", async () => {
    const { gitlab } = fakeGitlab();
    const seen: RunContext[] = [];
    // A task branch that never existed on the forge; runnerCloneForBranch creates it from the
    // default, so baseCommit is set but there is no published floor.
    const claim = taskClaim(`uzi/task/${randomUUID()}`);
    await runner(capturingExecutor(seen), gitlab).execute(claim).catch(() => undefined);

    assert.equal(seen.length, 1);
    // undefined here means originBranchTip returned null at claim (P = originBranchTip ?? undefined).
    assert.equal(seen[0]!.publishedTip, undefined, "a fresh branch has no published floor");
    assert.ok(
      seen[0]!.baseCommit,
      "baseCommit is still set — proving publishedTip is a DISTINCT floor, not baseCommit",
    );
  });

  it("re-establishes P on a second pickup, so an executor/session restart never drops it", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/republished";
    const pubSha = publishBranch(branch);
    const seen: RunContext[] = [];
    const runId = randomUUID();
    // First claim (fresh), then a resume of the SAME run (session_id set) — the run picked up
    // again, a fresh flight built at claim. Both must carry P.
    await runner(capturingExecutor(seen), gitlab)
      .execute(taskClaim(branch, { run_id: runId }))
      .catch(() => undefined);
    await runner(capturingExecutor(seen), gitlab)
      .execute(taskClaim(branch, { run_id: runId, session_id: "sess-restart", last_seq: 100 }))
      .catch(() => undefined);

    assert.equal(seen.length, 2, "the executor ran on both the first attempt and the resume");
    assert.equal(seen[0]!.publishedTip, pubSha, "P recorded on the first attempt");
    assert.equal(
      seen[1]!.publishedTip,
      pubSha,
      "P re-recorded on the resume — restart-surviving, not lost with the executor",
    );
  });
});
