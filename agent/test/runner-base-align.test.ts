import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { type Executor, type RunContext, type ExecutorResult } from "../src/executor.js";
import { RunRunner, composeBaseAlignConflictReason, type RunnerOptions } from "../src/runner.js";
import { Outbox } from "../src/outbox.js";
import { CheckpointSoftDeadlineError, RunnerCloneImportError, isNonFastForwardRejection } from "../src/git.js";
import { GitHubClient } from "../src/forge.js";
import {
  api,
  client,
  fakeGitHub,
  fx,
  git,
  gitlabClaim,
  installHarness,
} from "./runner-harness.js";

installHarness();

// PRD #456 M1/M2: the finalize base-align. A GitHub run merely BEHIND main on
// .github/workflows/** (main advanced those files after the clone base) must not lose its
// work at the finalize push. These drive the REAL runner over a REAL on-disk fixture origin
// carrying a real workflow file; "main advanced" is a genuine commit to the fixture origin's
// main during the executor turn, so the merge/rebase are exercised for real (no pure stubs).

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const CI_V1 = "name: ci\non: [push]\njobs: {}\n";
const CI_V2 = "name: ci\non: [pull_request]\njobs: {}\n";

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

/** Commit `files` onto the fixture origin's checked-out main. */
function commitToOriginMain(files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(fx.originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", msg]);
}

/** Seed the fixture origin with a real workflow file (plus optional extras) BEFORE the run,
 *  so the clone base carries it and a later origin commit models "main moved ahead". */
function seedWorkflowsOnOrigin(extra: Record<string, string> = {}): void {
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V1, ...extra }, "seed workflows");
}

/** Publish a legitimate prior align, then leave main one workflow revision ahead. */
function publishPriorAlign(iid: number, advanceAgain = true): void {
  seedWorkflowsOnOrigin();
  gitIn(fx.originPath, ["branch", `agent/issue-${iid}`]);
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "default D1");
  const d1 = gitIn(fx.originPath, ["rev-parse", "main"]);
  gitIn(fx.originPath, ["checkout", `agent/issue-${iid}`]);
  fs.writeFileSync(path.join(fx.originPath, ".github/workflows/ci.yml"), CI_V2);
  gitIn(fx.originPath, ["add", ".github/workflows/ci.yml"]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", `chore: align .github/workflows with ${d1}`]);
  gitIn(fx.originPath, ["checkout", "main"]);
  if (advanceAgain) commitToOriginMain({ ".github/workflows/ci.yml": "name: ci\non: [schedule]\njobs: {}\n" }, "default D2");
}

function githubRunner(github: GitHubClient, executor: Executor): RunRunner {
  return new RunRunner(
    client,
    git,
    () => ({ executor }),
    nullLogger(),
    20,
    undefined,
    { pollMs: 5, planApprovalTimeoutMs: 0, github },
  );
}

/** A github runner with extra RunnerOptions (e.g. a wired outbox), for the M3b write-ahead tests. */
function githubRunnerWith(github: GitHubClient, executor: Executor, extra: Partial<RunnerOptions>): RunRunner {
  return new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    github,
    ...extra,
  });
}

const githubClaim = (iid: number, overrides = {}) =>
  gitlabClaim(iid, {
    repo: {
      id: "r1",
      url: "https://github.com/org/repo",
      clone_url: fx.originPath,
      forge_type: "github",
    },
    ...overrides,
  });

/** An executor that commits `branchFiles` as the agent's work in the runner clone, then (if
 *  `mainFiles` given) advances the fixture origin's main to model main moving ahead. */
function committingExecutor(
  branchFiles: Record<string, string>,
  mainFiles?: Record<string, string>,
): Executor {
  return {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      for (const [rel, content] of Object.entries(branchFiles)) {
        const target = path.join(ctx.worktreePath, rel);
        fs.mkdirSync(path.dirname(target), { recursive: true });
        fs.writeFileSync(target, content);
      }
      execFileSync("git", ["-C", ctx.worktreePath, "add", "."], { env: ENV });
      execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "agent work"], {
        env: ENV,
      });
      if (mainFiles) commitToOriginMain(mainFiles, "main advances");
      return { branch: ctx.branch };
    },
  };
}

/** Record every strategy `alignBranchWithDefault` is invoked with, delegating to the real one. */
function spyAlign(): string[] {
  const strategies: string[] = [];
  const orig = git.alignBranchWithDefault.bind(git);
  git.alignBranchWithDefault = (async (
    clonePath: string,
    branch: string,
    baseTip: string,
    defaultTip: string,
    strategy: "merge" | "rebase" | "workflow-subtree",
  ) => {
    strategies.push(strategy);
    return orig(clonePath, branch, baseTip, defaultTip, strategy);
  }) as typeof git.alignBranchWithDefault;
  return strategies;
}

/** Count fetchDefaultTip invocations, delegating to the real one. */
function spyFetchDefaultTip(): { count: () => number } {
  let n = 0;
  const orig = git.fetchDefaultTip.bind(git);
  git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
    n++;
    return orig(...args);
  }) as typeof git.fetchDefaultTip;
  return { count: () => n };
}

describe("RunRunner — finalize base-align (PRD #456)", () => {
  it("duplicate merged workflows advancing after claim use a real subtree overlay and nonforced push", async () => {
    seedWorkflowsOnOrigin();
    gitIn(fx.originPath, ["checkout", "-b", "agent/issue-1869"]);
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "maintainer workflow");
    const maintainer = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    gitIn(fx.originPath, ["checkout", "main"]);
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "independent default workflow");
    const independent = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    assert.notStrictEqual(independent, maintainer);
    gitIn(fx.originPath, ["checkout", "agent/issue-1869"]);
    gitIn(fx.originPath, [...IDENT, "merge", "--no-ff", "-m", "merge duplicate default", "main"]);
    const published = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    gitIn(fx.originPath, ["checkout", "main"]);
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const claim = githubClaim(1869);
    await githubRunner(github, committingExecutor({ "impl.ts": "task content\n" },
      { ".github/workflows/ci.yml": "name: after claim\n" })).execute(claim);
    assert.strictEqual(api.states.filter((s) => s.runId === claim.run_id).at(-1)?.body.status, "completed", "duplicate published workflow must pass precheck");
    assert.deepStrictEqual(strategies, ["workflow-subtree"], "use the actual workflow subtree overlay");
    assert.strictEqual(calls.length, 1);
    const tip = gitIn(fx.originPath, ["rev-parse", "agent/issue-1869"]);
    assert.notStrictEqual(tip, published, "origin really advanced");
    assert.strictEqual(gitIn(fx.originPath, ["merge-base", "--is-ancestor", published, tip]), "");
    assert.strictEqual(gitIn(fx.originPath, ["show", `${tip}:impl.ts`]), "task content");
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", `${tip}:.github/workflows`]), gitIn(fx.originPath, ["rev-parse", "main:.github/workflows"]));
  });

  it("newly edited published workflow content is refused and preserves original work", async () => {
    publishPriorAlign(1872, true);
    const { github, calls } = fakeGitHub();
    const claim = githubClaim(1872);
    await githubRunner(github, committingExecutor({
      ".github/workflows/ci.yml": "name: unpublished edit\n", "impl.ts": "task content\n",
    })).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.strictEqual(failed.length, 1);
    assert.strictEqual(failed[0]?.body.fail_origin, "workflow_scope_missing");
    assert.match(failed[0]?.body.preserved_patch ?? "", /unpublished edit/);
    assert.match(failed[0]?.body.preserved_patch ?? "", /task content/);
    assert.strictEqual(calls.length, 0);
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-1872:.github/workflows/ci.yml"]), CI_V2.trim());
  });

  it("a final mismatch after overlay reports once with the original patch and no fallback", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const align = git.alignBranchWithDefault.bind(git);
    git.alignBranchWithDefault = (async (...args: Parameters<typeof git.alignBranchWithDefault>) => {
      const res = await align(...args);
      commitToOriginMain({ ".github/workflows/ci.yml": "name: later default\n" }, "default advances after overlay");
      return res;
    }) as typeof git.alignBranchWithDefault;
    let pushes = 0;
    git.pushBranch = (async () => { pushes++; }) as typeof git.pushBranch;
    const claim = githubClaim(1873);
    await githubRunner(github, committingExecutor({ "impl.ts": "original task\n" },
      { ".github/workflows/ci.yml": CI_V2 })).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.strictEqual(failed.length, 1);
    assert.strictEqual(failed[0]?.body.fail_origin, "workflow_scope_missing");
    assert.match(failed[0]?.body.preserved_patch ?? "", /original task/);
    assert.doesNotMatch(failed[0]?.body.preserved_patch ?? "", /diff --git a\/\.github\/workflows/);
    assert.deepStrictEqual(strategies, ["workflow-subtree"]);
    assert.strictEqual(pushes, 0);
    assert.strictEqual(calls.length, 0);
  });

  for (const [index, failure] of ["unavailable", "ordinary error"].entries()) {
    it(`recovers from first precheck target lookup ${failure} with fresh alignment and final reads`, async () => {
      seedWorkflowsOnOrigin();
      const initialDefault = gitIn(fx.originPath, ["rev-parse", "main"]);
      const { github, calls } = fakeGitHub();
      const strategies = spyAlign();
      const alignedDefaults: string[] = [];
      const align = git.alignBranchWithDefault.bind(git);
      git.alignBranchWithDefault = (async (...args: Parameters<typeof git.alignBranchWithDefault>) => {
        alignedDefaults.push(args[3]);
        return align(...args);
      }) as typeof git.alignBranchWithDefault;

      const defaultTips: string[] = [];
      const fetch = git.fetchDefaultTip.bind(git);
      git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
        if (defaultTips.length === 1) {
          commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "advance after unavailable precheck");
        }
        const sha = await fetch(...args);
        defaultTips.push(sha);
        return sha;
      }) as typeof git.fetchDefaultTip;
      let targetReads = 0;
      const target = git.fetchWorkflowTargetTip.bind(git);
      git.fetchWorkflowTargetTip = (async (...args: Parameters<typeof git.fetchWorkflowTargetTip>) => {
        if (++targetReads === 1) {
          if (failure === "ordinary error") throw new Error("precheck target lookup failed");
          return { kind: "unavailable" };
        }
        return target(...args);
      }) as typeof git.fetchWorkflowTargetTip;
      const snapshots: { defaultTip: string; files: string[] | null }[] = [];
      const classify = git.branchWorkflowFiles.bind(git);
      git.branchWorkflowFiles = (async (...args: Parameters<typeof git.branchWorkflowFiles>) => {
        const files = await classify(...args);
        snapshots.push({ defaultTip: args[1], files });
        return files;
      }) as typeof git.branchWorkflowFiles;

      let pushes = 0;
      const push = git.pushBranch.bind(git);
      git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
        pushes++;
        return push(...args);
      }) as typeof git.pushBranch;
      const pushArgs: string[][] = [];
      const commands = git as unknown as {
        runGit: (cwd: string | undefined, args: string[], pat?: string, scope?: string, username?: string) => Promise<string>;
      };
      const runGit = commands.runGit.bind(git);
      commands.runGit = async (...args: Parameters<typeof commands.runGit>) => {
        if (args[1][0] === "push") pushArgs.push([...args[1]]);
        return runGit(...args);
      };

      let implementationSha = "";
      const executor = committingExecutor({ "impl.ts": "recovered implementation\n" });
      const claim = githubClaim(1876 + index);
      await githubRunner(github, {
        run: async (ctx: RunContext) => {
          const result = await executor.run(ctx);
          implementationSha = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
          return result;
        },
      }).execute(claim);

      const freshDefault = gitIn(fx.originPath, ["rev-parse", "main"]);
      assert.notStrictEqual(freshDefault, initialDefault, "main advanced after the failed precheck");
      assert.strictEqual(targetReads, 3, "precheck failure must not skip fresh eligibility or the final read");
      assert.deepStrictEqual(defaultTips, [initialDefault, freshDefault, freshDefault]);
      assert.deepStrictEqual(snapshots, [
        { defaultTip: freshDefault, files: [".github/workflows/ci.yml"] },
        { defaultTip: freshDefault, files: [] },
      ], "real fresh eligibility uses the fetched align SHA and final classification sees aligned workflows");
      assert.deepStrictEqual(alignedDefaults, [defaultTips[1]], "align uses the exact eligibility snapshot");
      assert.deepStrictEqual(strategies, ["workflow-subtree"], "real workflow-subtree alignment ran");
      const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
      assert.deepStrictEqual(states.map((s) => s.status), ["running", "running", "completed"]);
      assert.ok(states.every((s) => s.fail_origin !== "workflow_scope_missing"));
      assert.strictEqual(pushes, 1, "one real pushBranch call");
      assert.strictEqual(pushArgs.length, 1, "one real git push");
      assert.ok(pushArgs[0]!.every((arg) => !arg.startsWith("--force") && arg !== "-f" && !arg.startsWith("+")),
        "no force flags or forced refspec");
      const published = gitIn(fx.originPath, ["rev-parse", `agent/issue-${1876 + index}`]);
      assert.ok(implementationSha, "captured executor commit before alignment");
      assert.strictEqual(gitIn(fx.originPath, ["merge-base", "--is-ancestor", implementationSha, published]), "");
      assert.strictEqual(gitIn(fx.originPath, ["show", `${published}:impl.ts`]), "recovered implementation");
      assert.strictEqual(gitIn(fx.originPath, ["rev-parse", `${published}:.github/workflows`]),
        gitIn(fx.originPath, ["rev-parse", `${freshDefault}:.github/workflows`]), "published workflows match fresh main");
      assert.strictEqual(calls.length, 1);
    });
  }

  for (const [index, failure] of [
    "unavailable target", "null classifier", "thrown target", "thrown classifier",
  ].entries()) {
    it(`fresh alignment ${failure} latches normal nonforced publication after a permitted precheck`, async () => {
      seedWorkflowsOnOrigin();
      const initialDefault = gitIn(fx.originPath, ["rev-parse", "main"]);
      const { github, calls } = fakeGitHub();
      const strategies = spyAlign();
      // Main advances only after the real precheck has permitted the original workflow.
      const fetch = git.fetchDefaultTip.bind(git);
      let fetches = 0;
      git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
        if (++fetches === 2) commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "advance before unavailable eligibility");
        return fetch(...args);
      }) as typeof git.fetchDefaultTip;
      const target = git.fetchWorkflowTargetTip.bind(git);
      let targetReads = 0;
      git.fetchWorkflowTargetTip = (async (...args: Parameters<typeof git.fetchWorkflowTargetTip>) => {
        if (++targetReads === 2) {
          if (failure === "unavailable target") return { kind: "unavailable" };
          if (failure === "thrown target") throw new Error("target lookup failed");
        }
        return target(...args);
      }) as typeof git.fetchWorkflowTargetTip;
      const classify = git.branchWorkflowFiles.bind(git);
      const snapshots: { defaultTip: string; hits: string[] | null }[] = [];
      let classifierReads = 0;
      git.branchWorkflowFiles = (async (...args: Parameters<typeof git.branchWorkflowFiles>) => {
        if (++classifierReads === 2) {
          if (failure === "null classifier") return null;
          if (failure === "thrown classifier") throw new Error("classification failed");
        }
        const hits = await classify(...args);
        snapshots.push({ defaultTip: args[1], hits });
        return hits;
      }) as typeof git.branchWorkflowFiles;
      git.changedFiles = (async () => null) as typeof git.changedFiles;
      let pushes = 0;
      const push = git.pushBranch.bind(git);
      git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
        pushes++;
        return push(...args);
      }) as typeof git.pushBranch;
      const pushArgs: string[][] = [];
      const commands = git as unknown as {
        runGit: (cwd: string | undefined, args: string[], pat?: string, scope?: string, username?: string) => Promise<string>;
      };
      const runGit = commands.runGit.bind(git);
      commands.runGit = async (...args: Parameters<typeof commands.runGit>) => {
        if (args[1][0] === "push") pushArgs.push([...args[1]]);
        return runGit(...args);
      };
      const claim = githubClaim(1900 + index);
      await githubRunner(github, committingExecutor({ "impl.ts": "preserved implementation\n" })).execute(claim);

      assert.deepStrictEqual(snapshots[0], { defaultTip: initialDefault, hits: [] }, "real precheck permitted publication");
      assert.notStrictEqual(gitIn(fx.originPath, ["rev-parse", "main"]), initialDefault, "workflow trees genuinely diverged");
      assert.deepStrictEqual(api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status),
        ["running", "running", "completed"]);
      assert.deepStrictEqual(strategies, [], "unknown fresh eligibility skips every align strategy");
      assert.strictEqual(fetches, 2, "fresh SHA reused; unavailable latch skips final local veto");
      assert.strictEqual(targetReads, 2, "target refreshed once at alignment, never at final push");
      assert.strictEqual(classifierReads, failure.includes("target") ? 1 : 2);
      assert.strictEqual(pushes, 1);
      assert.strictEqual(pushArgs.length, 1, "one real git push");
      assert.ok(pushArgs[0]!.every((arg) => !arg.startsWith("--force") && arg !== "-f" && !arg.startsWith("+")));
      const published = gitIn(fx.originPath, ["rev-parse", `agent/issue-${1900 + index}`]);
      assert.strictEqual(gitIn(fx.originPath, ["merge-base", "--is-ancestor", initialDefault, published]), "");
      assert.strictEqual(gitIn(fx.originPath, ["show", `${published}:impl.ts`]), "preserved implementation");
      assert.strictEqual(gitIn(fx.originPath, ["show", `${published}:.github/workflows/ci.yml`]), CI_V1.trim(),
        "normal push retains the original workflow rather than aligning");
      assert.strictEqual(calls.length, 1);
    });
  }

  for (const [siteIndex, site] of ["target", "classifier"].entries()) {
    for (const [errorIndex, kind] of ["abort", "deadline"].entries()) {
      it(`fresh alignment ${site} ${kind} prohibits publication`, async () => {
        seedWorkflowsOnOrigin();
        const { github, calls } = fakeGitHub();
        const strategies = spyAlign();
        const fetch = git.fetchDefaultTip.bind(git);
        let fetches = 0;
        git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
          if (++fetches === 2) commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "advance before boundary stop");
          return fetch(...args);
        }) as typeof git.fetchDefaultTip;
        const stop = () => {
          if (kind === "deadline") throw new CheckpointSoftDeadlineError();
          const error = new Error("fixture boundary cancelled");
          error.name = "AbortError";
          throw error;
        };
        const target = git.fetchWorkflowTargetTip.bind(git);
        let targetReads = 0;
        git.fetchWorkflowTargetTip = (async (...args: Parameters<typeof git.fetchWorkflowTargetTip>) => {
          if (++targetReads === 2 && site === "target") stop();
          return target(...args);
        }) as typeof git.fetchWorkflowTargetTip;
        const classify = git.branchWorkflowFiles.bind(git);
        let classifierReads = 0;
        git.branchWorkflowFiles = (async (...args: Parameters<typeof git.branchWorkflowFiles>) => {
          if (++classifierReads === 2 && site === "classifier") stop();
          return classify(...args);
        }) as typeof git.branchWorkflowFiles;
        let pushes = 0;
        git.pushBranch = (async () => { pushes++; }) as typeof git.pushBranch;
        const claim = githubClaim(1910 + siteIndex * 2 + errorIndex);
        await githubRunner(github, committingExecutor({ "impl.ts": "boundary task\n" })).execute(claim);
        assert.strictEqual(fetches, 2);
        assert.strictEqual(targetReads, 2, "abort occurs after successful precheck at the fresh seam");
        assert.strictEqual(classifierReads, site === "target" ? 1 : 2);
        assert.deepStrictEqual(strategies, []);
        assert.strictEqual(pushes, 0);
        assert.strictEqual(calls.length, 0);
      });
    }
  }

  it("plain remote workflow rejection preserves work with a typed outcome", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    let pushes = 0;
    git.pushBranch = (async () => {
      pushes++;
      throw new Error("refusing to update workflow without workflow scope");
    }) as typeof git.pushBranch;
    const claim = githubClaim(1874);
    await githubRunner(github, committingExecutor({ "impl.ts": "remote rejection task\n" })).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.strictEqual(failed.length, 1);
    assert.strictEqual(failed[0]?.body.fail_origin, "workflow_scope_missing");
    assert.match(failed[0]?.body.preserved_patch ?? "", /remote rejection task/);
    assert.strictEqual(pushes, 1);
    assert.strictEqual(calls.length, 0);
  });

  it("a retry refreshes refs after the first push attempt changes the default", async (t) => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    let pushes = 0;
    let skipBackoff = false;
    const realTimeout = globalThis.setTimeout;
    t.mock.method(globalThis, "setTimeout", ((...args: Parameters<typeof setTimeout>) => {
      if (skipBackoff && args[1] === 1_000) {
        skipBackoff = false;
        queueMicrotask(() => (args[0] as (...params: unknown[]) => void)(...args.slice(2)));
        return { ref() { return this; }, unref() { return this; } } as ReturnType<typeof setTimeout>;
      }
      return realTimeout(...args);
    }) as typeof setTimeout);
    git.pushBranch = (async () => {
      pushes++;
      commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "default changes during first attempt");
      skipBackoff = true;
      throw new Error("connection reset");
    }) as typeof git.pushBranch;
    const claim = githubClaim(1875);
    await githubRunner(github, committingExecutor({ "impl.ts": "retry task\n" })).execute(claim);
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.strictEqual(failed.length, 1);
    assert.strictEqual(failed[0]?.body.fail_origin, "workflow_scope_missing");
    assert.match(failed[0]?.body.preserved_patch ?? "", /retry task/);
    assert.strictEqual(pushes, 1, "the second attempt checks fresh refs before pushing");
    assert.strictEqual(calls.length, 0);
  });

  for (const site of ["precheck", "align", "final"] as const) {
    it(`boundary abort at ${site} prohibits publication`, async () => {
      seedWorkflowsOnOrigin();
      const { github, calls } = fakeGitHub();
      const fetch = git.fetchDefaultTip.bind(git);
      let fetches = 0;
      let pushes = 0;
      const failAt = site === "precheck" ? 1 : site === "align" ? 2 : 3;
      git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
        if (++fetches === failAt) {
          const error = new Error("fixture boundary cancelled");
          error.name = "AbortError";
          throw error;
        }
        return fetch(...args);
      }) as typeof git.fetchDefaultTip;
      git.pushBranch = (async () => { pushes++; }) as typeof git.pushBranch;
      const claim = githubClaim(1880 + failAt);
      await githubRunner(github, committingExecutor({ "impl.ts": "abort task\n" })).execute(claim);
      assert.strictEqual(pushes, 0);
      assert.strictEqual(calls.length, 0);
    });
  }

  for (const [iid, advanceAgain] of [[165, true], [166, false]] as const) {
    it(`prior published align ${advanceAgain ? "behind D2 bypasses unsafe alignment" : "at current D1 survives rework and opens a PR"}`, async () => {
      publishPriorAlign(iid, advanceAgain);
      const { github, calls } = fakeGitHub();
      const strategies = spyAlign();
      const claim = githubClaim(iid);
      await githubRunner(github, committingExecutor({ "impl.ts": "export const result = 1;\n" })).execute(claim);
      const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
      assert.deepStrictEqual(states.map((s) => s.status), ["running", "running", "completed"]);
      assert.strictEqual(calls.length, 1);
      assert.strictEqual(gitIn(fx.originPath, ["show", `agent/issue-${iid}:impl.ts`]), "export const result = 1;");
      assert.strictEqual(gitIn(fx.originPath, ["show", `agent/issue-${iid}:.github/workflows/ci.yml`]), CI_V2.trim());
      assert.deepStrictEqual(strategies, []);
    });
  }

  it("workflow resolution in a published merge is permitted for implementation-only rework", async () => {
    seedWorkflowsOnOrigin();
    const base = gitIn(fx.originPath, ["rev-parse", "main"]);
    gitIn(fx.originPath, ["checkout", "-b", "agent/issue-157"]);
    commitToOriginMain({ "branch.txt": "branch work\n" }, "branch nonworkflow work");
    const branchParent = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    assert.strictEqual(gitIn(fx.originPath, ["diff", "--name-only", base, branchParent]), "branch.txt");
    gitIn(fx.originPath, ["checkout", "main"]);
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "default workflow");
    const fresh = gitIn(fx.originPath, ["rev-parse", "main"]);
    gitIn(fx.originPath, ["checkout", "agent/issue-157"]);
    gitIn(fx.originPath, ["merge", "--no-ff", "--no-commit", fresh]);
    // Only this merge resolution changes the workflow relative to both parents.
    fs.writeFileSync(path.join(fx.originPath, ".github/workflows/ci.yml"), "name: resolved\n");
    gitIn(fx.originPath, ["add", ".github/workflows/ci.yml"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "resolve workflow"]);
    const merge = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    assert.strictEqual(gitIn(fx.originPath, ["rev-list", "--parents", "-n", "1", merge]), `${merge} ${branchParent} ${fresh}`);
    for (const parent of [branchParent, fresh]) {
      assert.strictEqual(gitIn(fx.originPath, ["diff", "--name-only", parent, merge, "--", ".github/workflows"]), ".github/workflows/ci.yml");
    }
    gitIn(fx.originPath, ["checkout", "main"]);
    const { github, calls } = fakeGitHub();
    const claim = githubClaim(157);
    const checked: Array<string[] | null> = [];
    const original = git.branchWorkflowFiles.bind(git);
    git.branchWorkflowFiles = (async (...args: Parameters<typeof git.branchWorkflowFiles>) => {
      const result = await original(...args);
      checked.push(result);
      return result;
    }) as typeof git.branchWorkflowFiles;
    await githubRunner(github, committingExecutor({ "impl.ts": "export const x = 1;\n" })).execute(claim);
    assert.ok(checked.length >= 2);
    assert.ok(checked.every((paths) => paths?.length === 0));
    assert.strictEqual(api.states.filter((s) => s.runId === claim.run_id).at(-1)?.body.status, "completed");
    assert.strictEqual(calls.length, 1);
  });

  it("published clean merge followed by MR rework completes without a workflow-scope failure", async () => {
    seedWorkflowsOnOrigin();
    gitIn(fx.originPath, ["checkout", "-b", "agent/issue-158"]);
    commitToOriginMain({ "branch.txt": "prior work\n" }, "branch work");
    gitIn(fx.originPath, ["checkout", "main"]);
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "default workflow");
    const fresh = gitIn(fx.originPath, ["rev-parse", "main"]);
    gitIn(fx.originPath, ["checkout", "agent/issue-158"]);
    gitIn(fx.originPath, [...IDENT, "merge", "--no-ff", "-m", "merge default workflows", fresh]);
    const merge = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    assert.strictEqual(gitIn(fx.originPath, ["show", `${merge}:.github/workflows/ci.yml`]), CI_V2.trim());
    gitIn(fx.originPath, ["checkout", "main"]);
    const { github, calls } = fakeGitHub();
    const claim = githubClaim(158, { kind: "mr_rework", branch: "agent/issue-158", issue_iid: null });
    const checked: Array<string[] | null> = [];
    const original = git.branchWorkflowFiles.bind(git);
    git.branchWorkflowFiles = (async (...args: Parameters<typeof git.branchWorkflowFiles>) => {
      const result = await original(...args);
      checked.push(result);
      return result;
    }) as typeof git.branchWorkflowFiles;
    await githubRunner(github, committingExecutor({ "impl.ts": "export const reworked = true;\n" })).execute(claim);
    assert.deepStrictEqual(checked, [[], []]);
    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    assert.deepStrictEqual(states.map((s) => s.status), ["running", "running", "completed"]);
    assert.strictEqual(calls.length, 1);
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-158:impl.ts"]), "export const reworked = true;");
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-158:.github/workflows/ci.yml"]), CI_V2.trim());
    assert.strictEqual(gitIn(fx.originPath, ["merge-base", "--is-ancestor", merge, "agent/issue-158"]), "");
  });

  // (a) behind-on-workflows: main advanced a workflow file the branch never touched → the
  // merge aligns the tree and the push proceeds, landing BOTH the agent's work and the fresh
  // workflow content on origin.
  it("(a) behind-on-workflows → merge aligns → push proceeds and lands the merged tree", async () => {
    seedWorkflowsOnOrigin();
    // A null diff (D6 fail-open) makes canOverlay false, so the overlay is skipped and this
    // exercises the UNCHANGED merge fallback — the primary overlay path has its own tests below.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushCalls++;
      return realPush(...args);
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(50);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.deepStrictEqual(strategies, ["merge"], "the merge path aligns; no rebase fallback");
    // Pins the exactly-one-push `if (!alignPushed)` guard: the align path pushed, so the
    // normal push must be skipped — one push total, never two.
    assert.strictEqual(pushCalls, 1, "the aligned branch is pushed exactly once");
    assert.strictEqual(calls.length, 1, "the PR was opened once");

    // The branch really landed on origin, carrying the agent's file AND the fresh workflow.
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-50:impl.ts"]), "export const x = 1;");
    assert.strictEqual(
      gitIn(fx.originPath, ["show", "agent/issue-50:.github/workflows/ci.yml"]).trim(),
      CI_V2.trim(),
      "the pushed branch's workflow file matches the current default (aligned)",
    );
  });

  // (b) the merged push is STILL workflow-scope-rejected → rebase fallback → push proceeds.
  // pushBranch is stubbed to reject once (as GitHub would) then succeed, so both strategies run.
  it("(b) merge push rejected for workflow scope → rebase fallback → push proceeds", async () => {
    seedWorkflowsOnOrigin();
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase fallback.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      if (pushCalls === 1) {
        throw new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-51 -> agent/issue-51 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope)",
        );
      }
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(51);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "merge rejected → rebase fallback");
    assert.strictEqual(pushCalls, 2, "pushed once (rejected), then once after the rebase");
    assert.strictEqual(calls.length, 1, "the PR was opened once after the successful align-push");
  });

  // (c) the branch and default edited the SAME non-workflow file divergently (plus the
  // workflow divergence that triggers the align) → merge AND rebase conflict → the run fails
  // typed with the diff preserved, and NO push happens.
  it("(c) merge and rebase both conflict → failed finalize_base_align_conflict + preserved_patch, no push", async () => {
    seedWorkflowsOnOrigin({ "conflict.txt": "base\n" });
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase→preserve chain.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    const exec = committingExecutor(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(52);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "both strategies attempted before failing");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.match(failed.failure_reason ?? "", /docs\/github-bot-setup\.md/);
    assert.match(failed.failure_reason ?? "", /workflows/);
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved for a human to land");
    assert.match(failed.preserved_patch!, /conflict\.txt/, "the preserved patch carries the agent's work");
    assert.strictEqual(pushed, false, "the doomed push was skipped");
    assert.strictEqual(calls.length, 0, "no PR opened on the conflict fail");
  });

  // PRD #1391 Run B M3 (N1): the failBaseAlignConflict FAILED must be journalled WRITE-AHEAD, so an
  // api outage at the finalize report does NOT degrade the typed outcome (fail_origin +
  // preserved_patch, the diff a human needs to land) into a generic agent_failure. Drives the REAL
  // conflict path (test (c)'s fixture) with an outbox wired and the failed report refused 409
  // running (the outage/lost-ack shape), then proves the exact typed body is on disk as the journal.
  it("(c') the base-align conflict FAILED is journalled write-ahead — fail_origin + preserved_patch survive an outage", async () => {
    seedWorkflowsOnOrigin({ "conflict.txt": "base\n" });
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    const exec = committingExecutor(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    );
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "base-align-journal-"));
    const outbox = new Outbox({
      root: path.join(dir, "outbox"),
      log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024,
      retentionMs: 7 * 86_400_000,
    });
    await outbox.init();
    const claim = githubClaim(70, { claim_generation: 9 });
    // Refuse the failed report with a benign 409 running (nothing applied), so the write-ahead
    // journal STAYS on disk and we can prove the typed conflict body was journalled before the send.
    api.failStateWhen(claim.run_id, (b) => b.status === "failed", { httpStatus: 409, runStatus: "running" });

    await githubRunnerWith(github, exec, { outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 }).execute(claim);

    assert.strictEqual(pushed, false, "the doomed push was skipped");
    assert.strictEqual(calls.length, 0, "no PR opened on the conflict fail");
    assert.equal(outbox.hasPendingTerminal(claim.run_id, 9), true, "the base-align-conflict failure is journalled write-ahead and kept on the 409");
    const j = await outbox.readTerminalJournal(claim.run_id, 9);
    assert.equal(j?.body.status, "failed", "the journal holds the failed outcome");
    assert.equal(
      j?.body.fail_origin,
      "finalize_base_align_conflict",
      "the TYPED fail_origin survived the outage, not a generic agent_failure",
    );
    const patch = j?.body.preserved_patch;
    assert.ok(typeof patch === "string" && patch.includes("conflict.txt"), "the preserved diff a human needs to land was journalled");
    await fsp.rm(dir, { recursive: true, force: true });
  });

  // (d) already aligned: the branch's workflow tree already matches the fresh default.
  it("(d) already aligned → no align work, single push, three fetchDefaultTip calls", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const fetchSpy = spyFetchDefaultTip();
    let pushCalls = 0;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushCalls++;
      return realPush(...args);
    }) as typeof git.pushBranch;
    // The agent changes a NON-workflow file; main does NOT move → workflow trees stay equal.
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" });

    const claim = githubClaim(53);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.deepStrictEqual(strategies, [], "no align was performed");
    assert.strictEqual(fetchSpy.count(), 3, "precheck, alignment and final push each fetch the default tip");
    assert.strictEqual(pushCalls, 1, "exactly one push");
    assert.strictEqual(calls.length, 1, "the PR was opened once");
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-53:impl.ts"]), "export const x = 1;");
  });

  it("uses a default tip that advances after precheck and before alignment", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const fetch = git.fetchDefaultTip.bind(git);
    let fetches = 0;
    git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
      fetches++;
      if (fetches === 2) commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "advance after precheck");
      return fetch(...args);
    }) as typeof git.fetchDefaultTip;
    const claim = githubClaim(154);
    await githubRunner(github, committingExecutor({ "impl.ts": "export const x = 1;\n" })).execute(claim);
    assert.deepStrictEqual(api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status),
      ["running", "running", "completed"]);
    assert.strictEqual(fetches, 3);
    assert.deepStrictEqual(strategies, ["workflow-subtree"]);
    assert.strictEqual(calls.length, 1);
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-154:.github/workflows/ci.yml"]), CI_V2.trim());
  });

  it("an absent implementation-only branch with a null diff merges the fresh default after a permitted precheck", async () => {
    seedWorkflowsOnOrigin();
    const initialDefault = gitIn(fx.originPath, ["rev-parse", "main"]);
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const alignedDefaults: string[] = [];
    const align = git.alignBranchWithDefault.bind(git);
    git.alignBranchWithDefault = (async (...args: Parameters<typeof git.alignBranchWithDefault>) => {
      alignedDefaults.push(args[3]);
      return align(...args);
    }) as typeof git.alignBranchWithDefault;

    const workflowSnapshots: { defaultTip: string; files: string[] | null }[] = [];
    let initialWorkflowArgs: Parameters<typeof git.branchWorkflowFiles> | undefined;
    const classify = git.branchWorkflowFiles.bind(git);
    git.branchWorkflowFiles = (async (...args: Parameters<typeof git.branchWorkflowFiles>) => {
      initialWorkflowArgs ??= args;
      const files = await classify(...args);
      workflowSnapshots.push({ defaultTip: args[1], files });
      return files;
    }) as typeof git.branchWorkflowFiles;
    const targets: Awaited<ReturnType<typeof git.fetchWorkflowTargetTip>>[] = [];
    const target = git.fetchWorkflowTargetTip.bind(git);
    git.fetchWorkflowTargetTip = (async (...args: Parameters<typeof git.fetchWorkflowTargetTip>) => {
      const result = await target(...args);
      targets.push(result);
      return result;
    }) as typeof git.fetchWorkflowTargetTip;

    const defaultTips: string[] = [];
    let fetches = 0;
    const fetch = git.fetchDefaultTip.bind(git);
    git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
      fetches++;
      if (fetches === 2) commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "advance after permitted precheck");
      const sha = await fetch(...args);
      defaultTips.push(sha);
      if (fetches === 2) {
        assert.ok(initialWorkflowArgs, "the real initial workflow comparison ran before main advanced");
        const [barePath, , trackingRef, targetTip] = initialWorkflowArgs;
        // Observe the same unaligned branch against the freshly fetched default.
        // This probe delegates to the real classifier without changing the runner's decision.
        await git.branchWorkflowFiles(barePath, sha, trackingRef, targetTip);
      }
      return sha;
    }) as typeof git.fetchDefaultTip;

    let pushCalls = 0;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushCalls++;
      return realPush(...args);
    }) as typeof git.pushBranch;
    const pushArgs: string[][] = [];
    const gitCommands = git as unknown as {
      runGit: (cwd: string | undefined, args: string[], pat?: string, scope?: string, username?: string) => Promise<string>;
    };
    const runGit = gitCommands.runGit.bind(git);
    gitCommands.runGit = async (...args: Parameters<typeof gitCommands.runGit>) => {
      if (args[1][0] === "push") pushArgs.push([...args[1]]);
      return runGit(...args);
    };

    const claim = githubClaim(1877);
    await githubRunner(github, committingExecutor({ "impl.ts": "export const x = 1;\n" })).execute(claim);

    const freshDefault = gitIn(fx.originPath, ["rev-parse", "main"]);
    assert.notStrictEqual(freshDefault, initialDefault, "main really advanced between precheck and alignment");
    assert.strictEqual(defaultTips[0], initialDefault);
    assert.strictEqual(defaultTips[1], freshDefault);
    assert.deepStrictEqual(targets[0], { kind: "absent" }, "the implementation branch was initially unpublished");
    assert.deepStrictEqual(workflowSnapshots[0], { defaultTip: initialDefault, files: [] }, "the initial snapshot permitted publication");
    assert.deepStrictEqual(workflowSnapshots[1], { defaultTip: freshDefault, files: [".github/workflows/ci.yml"] }, "the fresh snapshot requires alignment");
    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    assert.deepStrictEqual(states.map((s) => s.status), ["running", "running", "completed"],
      `the permitted precheck must allow later merge alignment, not workflow_scope_missing: ${JSON.stringify(states)}; strategies=${JSON.stringify(strategies)}`);
    assert.deepStrictEqual(strategies, ["merge"], "a null diff uses the real merge fallback");
    assert.deepStrictEqual(alignedDefaults, [freshDefault], "alignment uses the exact freshly fetched SHA");
    assert.deepStrictEqual(defaultTips, [initialDefault, freshDefault, freshDefault]);
    assert.strictEqual(pushCalls, 1, "exactly one real pushBranch call");
    assert.strictEqual(pushArgs.length, 1, "exactly one real git push");
    assert.ok(pushArgs[0]!.every((arg) => !arg.startsWith("--force") && arg !== "-f" && !arg.startsWith("+")),
      "the observed push has no force flag or forced refspec");
    const published = gitIn(fx.originPath, ["rev-parse", "agent/issue-1877"]);
    assert.notStrictEqual(published, initialDefault, "the real push published a branch update");
    assert.strictEqual(gitIn(fx.originPath, ["merge-base", "--is-ancestor", freshDefault, published]), "");
    assert.strictEqual(gitIn(fx.originPath, ["show", `${published}:impl.ts`]), "export const x = 1;");
    assert.strictEqual(gitIn(fx.originPath, ["show", `${published}:.github/workflows/ci.yml`]), CI_V2.trim());
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", `${published}:.github/workflows`]),
      gitIn(fx.originPath, ["rev-parse", `${freshDefault}:.github/workflows`]), "published workflows match fresh main");
    assert.strictEqual(calls.length, 1, "the PR was opened once");
  });

  it("second default-tip fetch failure falls through to the normal push", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const fetch = git.fetchDefaultTip.bind(git);
    let fetches = 0;
    git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
      fetches++;
      if (fetches === 2) throw new Error("second fetch unavailable");
      return fetch(...args);
    }) as typeof git.fetchDefaultTip;
    const claim = githubClaim(155);
    await githubRunner(github, committingExecutor(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    )).execute(claim);
    assert.deepStrictEqual(api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status),
      ["running", "running", "completed"]);
    assert.strictEqual(fetches, 2);
    assert.deepStrictEqual(strategies, []);
    assert.strictEqual(calls.length, 1);
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-155:impl.ts"]), "export const x = 1;");
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-155:.github/workflows/ci.yml"]), CI_V1.trim());
  });

  it("default-tip fetch failure falls through to the normal push", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    git.fetchDefaultTip = (async () => { throw new Error("fetch unavailable"); }) as typeof git.fetchDefaultTip;
    const claim = githubClaim(153);
    await githubRunner(github, committingExecutor({ "impl.ts": "export const x = 1;\n" })).execute(claim);
    assert.deepStrictEqual(api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status),
      ["running", "running", "completed"]);
    assert.deepStrictEqual(strategies, []);
    assert.strictEqual(calls.length, 1);
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-153:impl.ts"]), "export const x = 1;");
  });

  // An UNEXPECTED throw from alignBranchWithDefault (e.g. the S3 count-mismatch guard, or any
  // git error) must NOT escape to the generic catch — which would report `failed` with a raw
  // internal message and NO preserved_patch. It must route to the typed conflict-fail path so
  // the agent's work is still preserved. This is the whole point of the feature on failure.
  it("an unexpected align error still fails typed with preserved_patch, never the raw generic catch", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // Both align attempts throw an unexpected internal error (models the count-mismatch guard).
    git.alignBranchWithDefault = (async () => {
      throw new Error("alignBranchWithDefault: rebase dropped commits (2 → 1) — refusing to push truncated work");
    }) as typeof git.alignBranchWithDefault;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(54);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    // The typed reason, NOT the raw internal error the generic catch would surface.
    assert.match(failed.failure_reason ?? "", /docs\/github-bot-setup\.md/);
    assert.ok(
      !(failed.failure_reason ?? "").includes("dropped commits"),
      "the raw internal error must not reach the failure_reason (that would mean the generic catch)",
    );
    assert.ok(failed.preserved_patch, "the diff is preserved even on an unexpected align error");
    assert.match(failed.preserved_patch!, /impl\.ts/);
    assert.strictEqual(pushed, false, "no push on an align error");
    assert.strictEqual(calls.length, 0, "no PR opened on an align error");
  });

  // (e) DOUBLE-TOCTOU: the merge push is workflow-scope-rejected → rebase fallback aligns →
  // but the POST-REBASE push is ITSELF workflow-scope-rejected (main's workflow files moved
  // AGAIN during our align). This must NOT escape to the generic catch (which would report a
  // raw message, a defaulted fail_origin, and NO preserved_patch — the very data-loss bug).
  // It must route to the typed conflict-fail path: failed + finalize_base_align_conflict +
  // preserved_patch, no PR, and the raw reject text absent from failure_reason.
  it("(e) rebase-aligned push STILL workflow-scope-rejected → typed fail + preserved_patch, no raw catch", async () => {
    seedWorkflowsOnOrigin();
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase→preserve chain.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const rejectMsg =
      "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-55 -> agent/issue-55 " +
      "(refusing to allow a Personal Access Token to create or update workflow " +
      "`.github/workflows/ci.yml` without workflow scope)";
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      // BOTH pushes are workflow-scope-rejected: the first drives the rebase fallback, the
      // second models main's workflow files moving again DURING the align.
      throw new Error(rejectMsg);
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(55);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "merge rejected → rebase, then the aligned push is rejected again");
    assert.strictEqual(pushCalls, 2, "the merge push and the post-rebase push were both attempted and rejected");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict", "routed to the typed fail, not the generic catch");
    assert.match(failed.failure_reason ?? "", /docs\/github-bot-setup\.md/);
    assert.ok(
      !(failed.failure_reason ?? "").includes("Personal Access Token"),
      "the raw reject text must not reach failure_reason (that would mean the generic catch)",
    );
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved even on a double-TOCTOU rejection");
    assert.match(failed.preserved_patch!, /impl\.ts/, "the preserved patch carries the agent's work");
    assert.strictEqual(calls.length, 0, "no PR opened when the aligned push was rejected");
  });

  // (f) mislabel guard: a NON-workflow-scope push error on the rebase-fallback path must still
  // rethrow to the generic catch — a genuine auth/transient/protected-branch failure must NOT
  // be mislabelled as a base-align conflict. It surfaces the raw message with a defaulted
  // fail_origin and no preserved_patch.
  it("(f) non-workflow-scope error on the rebase path still surfaces via the generic catch", async () => {
    seedWorkflowsOnOrigin();
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase fallback.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      if (pushCalls === 1) {
        throw new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-56 -> agent/issue-56 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope)",
        );
      }
      // The post-rebase push fails for an unrelated reason (transient/network) — NOT a
      // workflow-scope rejection, so it must rethrow rather than be preserved-and-typed.
      throw new Error("boom: transient push failure over the network");
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(56);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "merge rejected → rebase, then the non-scope error");
    assert.strictEqual(pushCalls, 2, "the merge push and the post-rebase push were both attempted");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.notStrictEqual(
      failed.fail_origin,
      "finalize_base_align_conflict",
      "a non-workflow-scope error must NOT be mislabelled as a base-align conflict",
    );
    assert.match(failed.failure_reason ?? "", /transient push failure/, "the raw error surfaces via the generic catch");
    assert.ok(!failed.preserved_patch, "no preserved_patch on the generic-catch path");
    assert.strictEqual(calls.length, 0, "no PR opened on the failed push");
  });

  // (g) NB2 — resumed / rewritten-history edge. The merge push is workflow-scope-rejected →
  // rebase fallback aligns → but the POST-REBASE push is rejected NON-FAST-FORWARD, because the
  // rebase rewound to the original agent tip and replayed the commits, rewriting SHAs that were
  // already published at origin (a resume, or the self_improve fixed branch). Force-push is
  // denied by the guardrails, so this must NOT escape to the generic catch (raw message,
  // defaulted fail_origin, no preserved_patch); it must route to the SAME typed conflict-fail
  // path a repeat workflow-scope rejection takes: failed + finalize_base_align_conflict +
  // preserved_patch, no PR, and the raw non-fast-forward text absent from failure_reason.
  it("(g) rebase-aligned push rejected non-fast-forward (resumed branch) → typed fail + preserved_patch, no raw catch", async () => {
    seedWorkflowsOnOrigin();
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase→preserve chain.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const nonFfMsg =
      "git push origin ... failed: ! [rejected] agent/issue-57 -> agent/issue-57 (non-fast-forward)\n" +
      "error: failed to push some refs to '...'\n" +
      "hint: Updates were rejected because the tip of your current branch is behind\n" +
      "hint: its remote counterpart. Integrate the remote changes (e.g.\n" +
      "hint: 'git pull ...') before pushing again.";
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      if (pushCalls === 1) {
        // The merge push is workflow-scope-rejected → drives the rebase fallback.
        throw new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-57 -> agent/issue-57 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope)",
        );
      }
      // The post-rebase push cannot fast-forward — the rebase rewrote already-published history.
      throw new Error(nonFfMsg);
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(57);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "merge rejected → rebase, then the aligned push is non-fast-forward rejected");
    assert.strictEqual(pushCalls, 2, "the merge push and the post-rebase push were both attempted and rejected");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict", "routed to the typed fail, not the generic catch");
    assert.match(failed.failure_reason ?? "", /docs\/github-bot-setup\.md/);
    assert.ok(
      !(failed.failure_reason ?? "").includes("non-fast-forward"),
      "the raw non-fast-forward text must not reach failure_reason (that would mean the generic catch)",
    );
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved even on a non-fast-forward rejection");
    assert.match(failed.preserved_patch!, /impl\.ts/, "the preserved patch carries the agent's work");
    assert.strictEqual(calls.length, 0, "no PR opened when the aligned push was rejected");
  });

  // (h) issue #627 PRIMARY: base-staleness on workflows PLUS an unrelated divergence on a
  // non-workflow file. A whole-tree merge/rebase WOULD conflict (main and the branch both
  // touch the same non-workflow file), but the narrow overlay only replaces the workflow
  // subtree, so it sails through: strategies is EXACTLY ["workflow-subtree"] (merge/rebase
  // never invoked), one push, one PR, and the agent's non-workflow work survives.
  it("(h) overlay primary: unrelated non-workflow divergence → workflow-subtree overlay aligns and keeps agent work", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushCalls++;
      return realPush(...args);
    }) as typeof git.pushBranch;
    // The branch commits a non-workflow file; main advances BOTH a workflow file AND the SAME
    // non-workflow file DIVERGENTLY — a whole-tree merge/rebase would conflict on it.
    const exec = committingExecutor(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(58);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    // The guard against a vacuous pass: the overlay handled it and NO merge/rebase ran.
    assert.deepStrictEqual(strategies, ["workflow-subtree"], "overlay is primary; merge/rebase not invoked");
    assert.strictEqual(pushCalls, 1, "the overlaid branch is pushed exactly once");
    assert.strictEqual(calls.length, 1, "the PR was opened once");
    // On origin: the agent's non-workflow content survives AND main's workflow content landed.
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-58:conflict.txt"]), "branch side");
    assert.strictEqual(
      gitIn(fx.originPath, ["show", "agent/issue-58:.github/workflows/ci.yml"]).trim(),
      CI_V2.trim(),
      "the pushed branch's workflow file matches the current default (overlaid)",
    );
  });

  // (i) issue #627 clobber-safety: the branch DID modify a workflow file, but changedFiles
  // returns null for the whole run. #377 fails OPEN (so execution reaches base-align even for a
  // workflow-editing branch), and the overlay gate canOverlay is false — so the overlay is NOT
  // attempted (it would clobber the agent's workflow edit). The fallback merge/rebase then
  // conflicts on the shared workflow file, and the run fails typed with the diff preserved.
  it("(i) clobber-safety: null diff + branch edited a workflow → overlay NOT taken, fallback conflicts, work preserved", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // Unknown precheck eligibility stays open; known fresh eligibility reaches the real fallback.
    let workflowReads = 0;
    git.branchWorkflowFiles = (async () => ++workflowReads === 1 ? null : [".github/workflows/ci.yml"]) as typeof git.branchWorkflowFiles;
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    // The branch edits the SAME workflow file main diverges, forcing the whole-tree conflict.
    const exec = committingExecutor(
      { ".github/workflows/ci.yml": "name: ci\non: [branch-edit]\njobs: {}\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(59);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.ok(!strategies.includes("workflow-subtree"), "the overlay must NOT be attempted for a workflow-editing branch");
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "the fallback merge→rebase ran instead");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.ok(failed.preserved_patch, "the agent's own workflow edit is preserved (never clobbered)");
    assert.match(failed.preserved_patch!, /workflows\/ci\.yml/, "the preserved patch carries the branch's workflow edit");
    assert.strictEqual(pushed, false, "no clobbering push");
    assert.strictEqual(calls.length, 0, "no PR opened on the conflict fail");
  });

  it("newline workflow edit is seen by the real overlay guard and cannot be clobbered", async () => {
    const workflow = ".github/workflows/line\nfeed.yml";
    seedWorkflowsOnOrigin({ [workflow]: "name: original\n" });
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    // Unknown precheck, then known fresh eligibility; retain the real newline overlay guard.
    let workflowReads = 0;
    git.branchWorkflowFiles = (async () => ++workflowReads === 1 ? null : [workflow]) as typeof git.branchWorkflowFiles;
    const realChanged = git.changedFiles.bind(git);
    let sawNewline = false;
    git.changedFiles = (async (...args: Parameters<typeof git.changedFiles>) => {
      const changed = await realChanged(...args);
      if (changed?.includes(workflow)) sawNewline = true;
      return changed;
    }) as typeof git.changedFiles;
    const claim = githubClaim(167);
    await githubRunner(github, committingExecutor(
      { [workflow]: "name: branch\n" },
      { [workflow]: "name: default\n" },
    )).execute(claim);
    assert.strictEqual(sawNewline, true, "the real diff retains the newline path");
    assert.ok(!strategies.includes("workflow-subtree"), "overlay cannot overwrite the branch edit");
    assert.strictEqual(api.states.at(-1)?.body.fail_origin, "finalize_base_align_conflict");
    assert.strictEqual(calls.length, 0);
  });

  it("a genuine workflow edit fails typed and preserves its exact content", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const branchContent = "name: ci\non: [branch-edit]\njobs: {}\n";
    const claim = githubClaim(159);
    await githubRunner(github, committingExecutor(
      { ".github/workflows/ci.yml": branchContent },
      { ".github/workflows/ci.yml": CI_V2 },
    )).execute(claim);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.strictEqual(failed?.fail_origin, "workflow_scope_missing");
    assert.ok(failed?.preserved_patch?.includes("+on: [branch-edit]"));
    assert.strictEqual(calls.length, 0);
  });

  it("newline workflow edit fails the precheck before any overlay", async () => {
    const workflow = ".github/workflows/line\nfeed.yml";
    seedWorkflowsOnOrigin({ [workflow]: "name: original\n" });
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const claim = githubClaim(168);
    await githubRunner(github, committingExecutor({ [workflow]: "name: branch\n" })).execute(claim);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.strictEqual(failed?.fail_origin, "workflow_scope_missing");
    assert.ok(failed?.preserved_patch?.includes("+name: branch"));
    assert.deepStrictEqual(strategies, []);
    assert.strictEqual(calls.length, 0);
  });

  // (j) issue #627 post-overlay push rejection → fallback. The overlay aligns, but its push is
  // workflow-scope-rejected (main's workflow files moved again during the align). This routes
  // to the merge/rebase fallback (NOT preserve-and-fail), which then pushes successfully.
  it("(j) overlay push still workflow-scope-rejected → falls back to merge/rebase and completes", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      if (pushCalls === 1) {
        // The overlay's push is workflow-scope-rejected → must fall back to merge/rebase.
        throw new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-60 -> agent/issue-60 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope)",
        );
      }
      // The merge fallback push succeeds.
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(60);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.strictEqual(strategies[0], "workflow-subtree", "the overlay was tried FIRST");
    assert.ok(
      strategies.slice(1).includes("merge") || strategies.slice(1).includes("rebase"),
      "a merge/rebase fallback was entered AFTER the overlay push rejection",
    );
    assert.strictEqual(pushCalls, 2, "overlay push (rejected) then the fallback push (succeeds)");
    assert.strictEqual(calls.length, 1, "the PR was opened once after the successful fallback push");
  });

  // (k) issue #631 (Item 1 regression) DOUBLE-FAULT: the overlay ALIGNS then its push is
  // workflow-scope-rejected → the merge/rebase fallback both CONFLICT on an unrelated
  // non-workflow file → failBaseAlignConflict preserves the diff. Pre-fix the preserved diff
  // was taken from `trackingRef`, which the overlay's fetchAndPush had already advanced to the
  // ALIGNED tip — so it carried the workflow-subtree change as a SUPERSET. Post-fix it is the
  // pre-align agent tip's diff: exactly the agent's human-landable work, with NO workflow file.
  it("(k) double-fault: overlay push rejected then merge+rebase conflict → preserved patch is the agent's work only, NOT a workflow superset", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      // The overlay's push is workflow-scope-rejected → falls back to merge/rebase; the
      // fallback then conflicts, so there is no second push.
      throw new Error(
        "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-61 -> agent/issue-61 " +
          "(refusing to allow a Personal Access Token to create or update workflow " +
          "`.github/workflows/ci.yml` without workflow scope)",
      );
    }) as typeof git.pushBranch;
    // Branch edits a non-workflow file; main advances BOTH the workflow file AND the SAME
    // non-workflow file divergently → the overlay aligns the workflow subtree (the branch
    // touched no workflow, so canOverlay is true) but a whole-tree merge AND rebase both
    // conflict on conflict.txt.
    const exec = committingExecutor(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(61);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.strictEqual(strategies[0], "workflow-subtree", "the overlay was tried FIRST");
    assert.ok(
      strategies.includes("merge") && strategies.includes("rebase"),
      "the fallback merge+rebase ran after the overlay push rejection",
    );
    assert.strictEqual(pushCalls, 1, "only the overlay push (rejected); the conflicting fallback pushes nothing");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved for a human to land");
    assert.match(failed.preserved_patch!, /conflict\.txt/, "the preserved patch carries the agent's work");
    // The regression guard: pre-fix this diff came from the aligned overlay tip and carried the
    // workflow-subtree change; post-fix it is the original agent tip's diff, so it must NOT.
    assert.ok(
      !failed.preserved_patch!.includes(".github/workflows/ci.yml"),
      "the preserved patch must not carry the workflow-subtree superset",
    );
    assert.strictEqual(calls.length, 0, "no PR opened on the conflict fail");
  });

  // (l) issue #631 (Item 3): the merge is PRIMARY (null #377 diff → overlay skipped) and its
  // push is rejected NON-FAST-FORWARD (an already-published branch — a resume, or the
  // self_improve fixed branch — whose merge push cannot fast-forward). The rebase fallback also
  // cannot force-push, so this must route to the typed preserve-and-fail — NOT rethrow to the
  // generic catch (raw message, no preserved_patch), and NOT attempt a rebase.
  it("(l) merge-fallback push rejected non-fast-forward → typed fail + preserved_patch, no rebase, no raw catch", async () => {
    seedWorkflowsOnOrigin();
    // null diff → overlay skipped → the merge is the primary align strategy.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const nonFfMsg =
      "git push origin ... failed: ! [rejected] agent/issue-62 -> agent/issue-62 (non-fast-forward)\n" +
      "error: failed to push some refs to '...'\n" +
      "hint: Updates were rejected because the tip of your current branch is behind\n" +
      "hint: its remote counterpart. Integrate the remote changes (e.g.\n" +
      "hint: 'git pull ...') before pushing again.";
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      // The merge push cannot fast-forward an already-published branch.
      throw new Error(nonFfMsg);
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(62);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge"], "non-ff must not trigger a rebase attempt");
    assert.strictEqual(pushCalls, 1, "only the merge push (rejected non-ff); no rebase push");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict", "typed fail, not the generic catch");
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved for a human to land");
    assert.match(failed.preserved_patch!, /impl\.ts/, "the preserved patch carries the agent's work");
    assert.ok(
      !(failed.failure_reason ?? "").includes("non-fast-forward"),
      "the raw non-fast-forward text must not reach failure_reason (that would mean the generic catch)",
    );
    assert.strictEqual(calls.length, 0, "no PR opened");
  });

  // The tip-blob precheck also uses changedFiles to distinguish branch edits from staleness. The zero-diff guard and the
  // overlay's independent clobber guard each call it once.
  it("(m) overlay checks the branch tree independently of tip-blob eligibility", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    let changedCalls = 0;
    const realChanged = git.changedFiles.bind(git);
    git.changedFiles = (async (...args: Parameters<typeof git.changedFiles>) => {
      changedCalls++;
      return realChanged(...args);
    }) as typeof git.changedFiles;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) =>
      realPush(...args)) as typeof git.pushBranch;
    // Real, non-null diff: the branch edits a non-workflow file, main advances the workflow
    // file → the overlay aligns and pushes (as in (h)).
    const exec = committingExecutor(
      { "conflict.txt": "branch side\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(63);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.strictEqual(calls.length, 1, "the overlaid branch was pushed and a PR opened");
    assert.strictEqual(
      changedCalls,
      4,
      "changedFiles: zero-diff guard, workflow precheck, fresh alignment eligibility and independent overlay clobber guard",
    );
  });

  // A null overlay diff must disable the overlay and preserve the workflow edit.
  it("(n) null overlay diff falls back without clobbering workflow work", async () => {
    seedWorkflowsOnOrigin();
    const { github } = fakeGitHub();
    const strategies = spyAlign();
    let changedCalls = 0;
    git.changedFiles = (async () => {
      changedCalls++;
      return null;
    }) as typeof git.changedFiles;
    // Precheck null does not latch; fresh nonempty eligibility plus null diff must use fallback.
    let workflowReads = 0;
    git.branchWorkflowFiles = (async () => ++workflowReads === 1 ? null : [".github/workflows/ci.yml"]) as typeof git.branchWorkflowFiles;
    git.pushBranch = (async () => {}) as typeof git.pushBranch;
    // The branch edits the SAME workflow file main diverges → the fallback merge/rebase
    // conflicts (as in (i)), proving the run reached the fallback after the recompute.
    const exec = committingExecutor(
      { ".github/workflows/ci.yml": "name: ci\non: [branch-edit]\njobs: {}\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(64);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "the run reached the fallback merge→rebase");
    assert.strictEqual(
      changedCalls,
      3,
      "zero-diff guard, fresh alignment eligibility and independent overlay guard; null precheck skips its diff",
    );
  });
});

describe("isNonFastForwardRejection", () => {
  it("matches the stable non-ff git phrases and rejects unrelated errors", () => {
    assert.ok(
      isNonFastForwardRejection(new Error("! [rejected] agent/issue-57 -> agent/issue-57 (non-fast-forward)")),
      "matches a non-fast-forward rejection",
    );
    assert.ok(
      isNonFastForwardRejection(new Error("Updates were rejected; fetch first before pushing")),
      "matches a fetch-first rejection",
    );
    assert.ok(
      !isNonFastForwardRejection(new Error("boom: transient push failure over the network")),
      "does not match an unrelated transient error",
    );
    assert.ok(
      !isNonFastForwardRejection(
        new Error(
          "refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope",
        ),
      ),
      "does not match a pure workflow-scope rejection",
    );
  });
});

describe("composeBaseAlignConflictReason", () => {
  it("keeps the doc link and preserved-diff pointer within the cap, even for a long branch name", () => {
    const short = composeBaseAlignConflictReason("main");
    assert.ok(short.length <= 512, `reason must be capped at 512 (got ${short.length})`);
    assert.match(short, /docs\/github-bot-setup\.md/);
    assert.match(short, /\.github\/workflows/);
    assert.match(short, /main/, "names the default branch");

    const long = composeBaseAlignConflictReason("x".repeat(300));
    assert.ok(long.length <= 512, `reason must be capped at 512 (got ${long.length})`);
    assert.match(long, /docs\/github-bot-setup\.md/, "doc link survives a long branch name");
    assert.match(long, /Your diff is preserved below\./, "preserved-diff pointer survives a long branch name");
    assert.match(long, /\.github\/workflows/);

    // The issue's measured regression: a 64-char branch name pushed the assembled reason to
    // 599 chars, so a blind slice(0, 512) dropped the doc link and preserved pointer.
    const sixtyFour = composeBaseAlignConflictReason("a".repeat(64));
    assert.ok(sixtyFour.length <= 512, `reason must be capped at 512 (got ${sixtyFour.length})`);
    assert.match(sixtyFour, /docs\/github-bot-setup\.md/, "doc link survives a 64-char branch name");
    assert.match(sixtyFour, /Your diff is preserved below\./, "preserved-diff pointer survives a 64-char branch name");
    assert.match(sixtyFour, /\.github\/workflows/);
  });
});

// Every preserved_patch is stored and rendered on the run page, and redactText knows only the run's
// OWN secrets. So a patch is attached only after a trusted gitleaks scan of the exact redacted text
// finds nothing: a foreign credential in the diff, or a scanner that cannot vouch for the text,
// keeps the typed failure and omits the patch. Scan findings and scanner failure are separate
// mechanisms, so each gets its own case per preserve site.
describe("RunRunner — preserved_patch is attached only after a clean exact-text secret scan", () => {
  // GitHub-PAT-shaped (what the test gitleaks shim detects), assembled at runtime so no complete
  // token literal is committed (check:token-literals).
  const foreignToken = () => ["gh", "p_", "x7Rq".repeat(9)].join("");
  const failedFor = (runId: string) =>
    api.states.find((s) => s.runId === runId && s.body.status === "failed")?.body;
  const untrustedScanner = () => {
    let calls = 0;
    git.scanPatchForSecrets = (async () => {
      calls++;
      return { trusted: false, findings: [] };
    }) as typeof git.scanPatchForSecrets;
    return () => calls;
  };

  it("workflow_scope_missing: a foreign secret in the diff withholds the patch", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const claim = githubClaim(1701);
    await githubRunner(github, committingExecutor(
      { ".github/workflows/ci.yml": "name: ci\non: [branch-edit]\njobs: {}\n", "leak.ts": `export const k = "${foreignToken()}";\n` },
      { ".github/workflows/ci.yml": CI_V2 },
    )).execute(claim);
    const failed = failedFor(claim.run_id);
    assert.strictEqual(failed?.fail_origin, "workflow_scope_missing", "the typed failure still lands");
    assert.strictEqual(failed?.preserved_patch, undefined, "the patch carrying the secret is not persisted");
    assert.doesNotMatch(failed?.failure_reason ?? "", /preserved below/, "the reason does not promise a withheld patch");
    assert.doesNotMatch(JSON.stringify(api.states.filter((s) => s.runId === claim.run_id)), new RegExp(foreignToken()));
    assert.strictEqual(calls.length, 0);
  });

  it("workflow_scope_missing: an untrustworthy scan withholds the patch", async () => {
    seedWorkflowsOnOrigin();
    const scans = untrustedScanner();
    const { github } = fakeGitHub();
    const claim = githubClaim(1702);
    await githubRunner(github, committingExecutor(
      { ".github/workflows/ci.yml": "name: ci\non: [branch-edit]\njobs: {}\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    )).execute(claim);
    const failed = failedFor(claim.run_id);
    assert.strictEqual(failed?.fail_origin, "workflow_scope_missing");
    assert.strictEqual(failed?.preserved_patch, undefined);
    assert.strictEqual(scans(), 1, "the exact patch was offered to the scanner once");
  });

  it("finalize_base_align_conflict: the patch gate itself withholds a foreign secret the range scan missed", async () => {
    seedWorkflowsOnOrigin({ "conflict.txt": "base\n" });
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    // The range scan reports trusted-and-clean (it missed the secret); only the exact-text patch
    // scan stands between the diff and runs.preserved_patch.
    git.secretScanRange = (async () => ({ trusted: true, findings: [] })) as typeof git.secretScanRange;
    const { github } = fakeGitHub();
    git.pushBranch = (async () => undefined) as typeof git.pushBranch;
    const claim = githubClaim(1704);
    await githubRunner(github, committingExecutor(
      { "conflict.txt": `branch side ${foreignToken()}\n` },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    )).execute(claim);
    const failed = failedFor(claim.run_id)!;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.strictEqual(failed.preserved_patch, undefined);
    assert.doesNotMatch(failed.failure_reason ?? "", /preserved below/);
  });

  it("finalize_base_align_conflict: an untrustworthy scan withholds the patch", async () => {
    seedWorkflowsOnOrigin({ "conflict.txt": "base\n" });
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const scans = untrustedScanner();
    const { github } = fakeGitHub();
    git.pushBranch = (async () => undefined) as typeof git.pushBranch;
    const claim = githubClaim(1705);
    await githubRunner(github, committingExecutor(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    )).execute(claim);
    const failed = failedFor(claim.run_id)!;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.strictEqual(failed.preserved_patch, undefined);
    assert.strictEqual(scans(), 1);
  });
});

// issue #1769 m2: a self-contained (Codex) runner clone has no alternate into the worker bare, so
// the fresh default tip fetchDefaultTip brought into the bare is imported into the clone
// (ensureRunnerCloneObjects) before any align strategy anchors it. An import failure happens
// BEFORE any merge/rebase, so it fails typed with an import-stage reason that never claims one ran.
describe("RunRunner — finalize base-align in a self-contained clone (issue #1769 m2)", () => {
  const IMPORT_TEXT = /could not import the default branch's new objects into the runner clone/;
  const failedFor = (runId: string) =>
    api.states.find((s) => s.runId === runId && s.body.status === "failed")?.body;
  const statusTexts = (runId: string): string[] =>
    api
      .messages(runId)
      .filter((m) => m.kind === "status")
      .map((m) => String((m.payload as { text?: unknown }).text ?? ""));
  const sandboxed = (exec: Executor): Executor => ({ ...exec, sandboxesCommands: true });
  const failImport = (): { count: () => number } => {
    let n = 0;
    git.ensureRunnerCloneObjects = (async (_bare: string, clonePath: string, tip: string) => {
      n++;
      throw new RunnerCloneImportError(clonePath, tip, "index-pack exited 128: simulated");
    }) as typeof git.ensureRunnerCloneObjects;
    return { count: () => n };
  };

  it("imports the fresh default tip into the self-contained clone, then the overlay aligns and pushes", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const imports: { tipInCloneBefore: boolean; haves: string[] }[] = [];
    const orig = git.ensureRunnerCloneObjects.bind(git);
    git.ensureRunnerCloneObjects = (async (bare: string, clonePath: string, tip: string, haves: string[]) => {
      let tipInCloneBefore = true;
      try {
        execFileSync("git", ["-C", clonePath, "cat-file", "-e", `${tip}^{commit}`], { env: ENV, stdio: "pipe" });
      } catch {
        tipInCloneBefore = false;
      }
      imports.push({ tipInCloneBefore, haves });
      return orig(bare, clonePath, tip, haves);
    }) as typeof git.ensureRunnerCloneObjects;
    const claim = githubClaim(1769);
    await githubRunner(github, sandboxed(committingExecutor(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    ))).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.strictEqual(imports.length, 1, "imported once, before the strategies");
    assert.strictEqual(imports[0]!.tipInCloneBefore, false, "the self-contained clone could not read the fresh tip before the import");
    assert.ok(imports[0]!.haves.length >= 1);
    assert.deepStrictEqual(strategies, ["workflow-subtree"]);
    assert.strictEqual(calls.length, 1, "the PR was opened");
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-1769:impl.ts"]), "export const x = 1;");
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-1769:.github/workflows/ci.yml"]), CI_V2.trim());
  });

  it("an import failure fails typed with the import-stage reason and status, preserving the patch, before any strategy", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const imports = failImport();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    const claim = githubClaim(1780);
    await githubRunner(github, sandboxed(committingExecutor(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    ))).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.strictEqual(imports.count(), 1);
    assert.deepStrictEqual(strategies, [], "no overlay, merge or rebase was attempted");
    assert.strictEqual(pushed, false, "nothing was pushed");
    assert.strictEqual(calls.length, 0, "no PR");
    const failed = failedFor(claim.run_id)!;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.strictEqual(failed.failure_reason, composeBaseAlignConflictReason("main", true, "import"));
    assert.match(failed.failure_reason ?? "", IMPORT_TEXT);
    assert.doesNotMatch(failed.failure_reason ?? "", /merge then rebase|tried to merge/);
    assert.match(failed.failure_reason ?? "", /Your diff is preserved below\./);
    assert.ok(failed.preserved_patch, "the agent's diff is preserved");
    assert.match(failed.preserved_patch!, /impl\.ts/);
    const texts = statusTexts(claim.run_id);
    const importStatus = texts.find((t) => IMPORT_TEXT.test(t));
    assert.ok(importStatus, `an import-stage status was emitted (got ${JSON.stringify(texts)})`);
    assert.match(importStatus!, /failing and preserving the diff for a human to land$/);
    assert.ok(!texts.some((t) => /merge and rebase conflicted/.test(t)), "no status claims a merge/rebase ran");
  });

  it("an import failure with an untrustworthy patch scan withholds the patch and says so", async () => {
    seedWorkflowsOnOrigin();
    const { github } = fakeGitHub();
    failImport();
    git.scanPatchForSecrets = (async () => ({ trusted: false, findings: [] })) as typeof git.scanPatchForSecrets;
    git.pushBranch = (async () => undefined) as typeof git.pushBranch;
    const claim = githubClaim(1781);
    await githubRunner(github, sandboxed(committingExecutor(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    ))).execute(claim);

    const failed = failedFor(claim.run_id)!;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.strictEqual(failed.preserved_patch, undefined, "the patch is withheld");
    assert.strictEqual(failed.failure_reason, composeBaseAlignConflictReason("main", false, "import"));
    assert.match(failed.failure_reason ?? "", IMPORT_TEXT);
    assert.doesNotMatch(failed.failure_reason ?? "", /preserved below/);
    assert.match(failed.failure_reason ?? "", /recoverable \(export it with `uzi run export`\)\.$/);
    const importStatus = statusTexts(claim.run_id).find((t) => IMPORT_TEXT.test(t));
    assert.match(importStatus ?? "", /failing \(the diff is withheld: it could not be preserved or did not scan clean\)$/);
  });

  it("the align-stage reason and statuses are unchanged by the stage parameter", async () => {
    // The pre-#1769 align reason, byte for byte (default stage and explicit "align").
    const legacy = (tail: string, branch = "main") =>
      `This run's branch is behind the default branch (${branch}) on .github/workflows files, which uzi's ` +
      "GitHub bot token cannot push while they differ from the default (its scope is `repo`, without " +
      "`workflow`, by design). uzi tried to merge then rebase the current default into the branch to " +
      "realign those files, but could not realign and safely push it, so the run failed without " +
      "pushing. The work is valid; a human can rebase and land it. See docs/github-bot-setup.md." + tail;
    const preservedTail = " Your diff is preserved below.";
    const withheldTail = " The committed work is on the run's branch and is recoverable (export it with `uzi run export`).";
    assert.strictEqual(composeBaseAlignConflictReason("main"), legacy(preservedTail));
    assert.strictEqual(composeBaseAlignConflictReason("main", true, "align"), legacy(preservedTail));
    // The align withheld variant drops the token-scope parenthetical so its recovery tail fits
    // the cap with the branch name intact (it used to overflow: the branch collapsed to "…" and
    // the final slice cut the tail).
    const withheld = composeBaseAlignConflictReason("main", false, "align");
    assert.ok(withheld.length <= 512, `capped at 512 (got ${withheld.length})`);
    assert.ok(withheld.includes("(main)"), withheld);
    assert.ok(withheld.endsWith(withheldTail), withheld);
    assert.ok(withheld.endsWith("`uzi run export`)."), withheld);
    assert.ok(!withheld.includes("its scope is `repo`"), withheld);
    assert.match(withheld, /docs\/github-bot-setup\.md/);
    const longWithheld = composeBaseAlignConflictReason("y".repeat(600), false, "align");
    assert.ok(longWithheld.length <= 512, `capped at 512 (got ${longWithheld.length})`);
    assert.match(longWithheld, /\(y+…\) on \.github\/workflows/);
    assert.ok(longWithheld.endsWith(withheldTail), longWithheld);

    // A real align conflict (test (c)'s fixture) keeps its status text.
    seedWorkflowsOnOrigin({ "conflict.txt": "base\n" });
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github } = fakeGitHub();
    git.pushBranch = (async () => undefined) as typeof git.pushBranch;
    const claim = githubClaim(1782);
    await githubRunner(github, committingExecutor(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    )).execute(claim);
    const failed = failedFor(claim.run_id)!;
    assert.strictEqual(failed.failure_reason, legacy(preservedTail));
    assert.ok(statusTexts(claim.run_id).includes(
      "could not realign the branch with the updated default branch and safely push it (merge and rebase conflicted, or the aligned branch could not be fast-forwarded); failing and preserving the diff for a human to land",
    ));
  });

  it("the import-stage reason keeps the doc link and tail within the cap for a long branch name", () => {
    for (const preserved of [true, false]) {
      const long = composeBaseAlignConflictReason("x".repeat(300), preserved, "import");
      assert.ok(long.length <= 512, `capped at 512 (got ${long.length})`);
      assert.match(long, /docs\/github-bot-setup\.md/);
      assert.match(long, IMPORT_TEXT);
      assert.match(long, preserved ? /Your diff is preserved below\.$/ : /`uzi run export`\)\.$/);
    }
  });
});
