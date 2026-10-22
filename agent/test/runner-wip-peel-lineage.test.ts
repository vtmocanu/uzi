import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { LimitReachedError } from "../src/limit.js";
import { fakeGitlab, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// issue #3239: the runner journals the `wip(park):` marker a resumed seed peeled (adoptedSource) as the
// attempt's lineage, but only for the plain `reset --soft` leg: not when nothing was recovered as WIP,
// and not on the diverged cherry-pick leg, which has no single adopted marker.

const SID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };

function parkFactory(homeRoot: string, commit: boolean): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.writeFileSync(path.join(ctx.worktreePath, "WIP.txt"), "in-progress, uncommitted\n");
        if (commit) {
          const g = (...a: string[]) => execFileSync("git", ["-C", ctx.worktreePath, ...a], { env: GIT_ENV, stdio: "pipe" });
          g("add", "WIP.txt");
          g("-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-m", "milestone");
        }
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
      },
    },
  });
}
const noopFactory = (homeRoot: string): ExecutorFactory => (runId) => ({
  homeDir: path.join(homeRoot, runId),
  executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch }) },
});

/** Park (dirty or committed), then resume; returns every markRecoveryCapture call of the RESUME. */
async function parkThenResume(iid: number, runId: string, opts: { commit: boolean; cherryPicked?: boolean }) {
  const { gitlab } = fakeGitlab();
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3239-lineage-"));
  try {
    await runnerWith(parkFactory(homeRoot, opts.commit), gitlab).execute(gitlabClaim(iid, { run_id: runId, wait_on_limit: true }));
    const calls: Array<{ barePath: string; peeledMarker: string | undefined }> = [];
    const mark = git.markRecoveryCapture.bind(git);
    git.markRecoveryCapture = async (...args: Parameters<typeof mark>) => {
      calls.push({ barePath: args[0], peeledMarker: args[5] });
      return mark(...args);
    };
    if (opts.cherryPicked) {
      const orig = git.runnerCloneForBranch.bind(git) as unknown as (...a: unknown[]) => Promise<Record<string, unknown>>;
      (git as unknown as { runnerCloneForBranch: unknown }).runnerCloneForBranch = async (...a: unknown[]) =>
        ({ ...(await orig(...a)), wipCherryPicked: true });
    }
    await runnerWith(noopFactory(homeRoot), gitlab).execute(
      gitlabClaim(iid, { run_id: runId, wait_on_limit: true, session_id: SID, last_seq: 1000 }));
    return calls;
  } finally {
    fs.rmSync(homeRoot, { recursive: true, force: true });
  }
}
const subjectOf = (bare: string, sha: string) =>
  execFileSync("git", ["-C", bare, "log", "-1", "--format=%s", sha], { env: GIT_ENV, encoding: "utf8" }).trim();

describe("RunRunner journals the peeled WIP marker as lineage (issue #3239)", () => {
  it("a WIP resume (reset --soft leg) passes the adopted marker to markRecoveryCapture", async () => {
    const calls = await parkThenResume(3239, "32390000-0000-4000-8000-000000003239", { commit: false });
    assert.equal(calls.length, 1);
    const marker = calls[0]!.peeledMarker;
    assert.match(marker ?? "", /^[0-9a-f]{40}$/);
    assert.ok(subjectOf(calls[0]!.barePath, marker!).startsWith("wip(park):"), "the passed commit is the park's WIP marker");
  });

  it("a resume that recovered a committed milestone and no WIP passes no marker", async () => {
    const calls = await parkThenResume(3240, "32400000-0000-4000-8000-000000003240", { commit: true });
    assert.equal(calls.length, 1);
    assert.equal(calls[0]!.peeledMarker, undefined);
  });

  it("a WIP resume that took the cherry-pick leg passes no marker", async () => {
    const calls = await parkThenResume(3241, "32410000-0000-4000-8000-000000003241", { commit: false, cherryPicked: true });
    assert.equal(calls.length, 1);
    assert.equal(calls[0]!.peeledMarker, undefined);
  });
});
