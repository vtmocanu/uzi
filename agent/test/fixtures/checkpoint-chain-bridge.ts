// Launched by TestAgentCheckpointChainBridge (api/internal/pushbroker). Issue #2733 binding bridge:
// drives a REAL RunRunner + GitCache through a GitHub-forge turn that publishes a workflow overlay
// checkpoint O, then a raw tick or iteration checkpoint N, and sends every publish through the REAL
// WorkerClient.publishCheckpoint to the Go test's loopback server, which runs the real
// pushbroker.Publish against the Go-owned bare origin. Every other worker RPC goes to the in-process
// FakeApi. Only APIs that already existed before the chain fix are used, so the same file runs
// against an older agent/src to show the failure there.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import "../setup/hermetic-proc.js";
import { WorkerClient } from "../../src/client.js";
import { GitCache } from "../../src/git.js";
import { RunRunner, type RunnerOptions } from "../../src/runner.js";
import type { RunContext } from "../../src/executor.js";
import { FakeApi } from "../fake-api.js";
import { defaultGitleaksShim } from "../gitleaks-shim.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "../helpers.js";
import { fakeGitHub } from "../runner-harness.js";

const cfg = JSON.parse(process.env.UZI_CHAIN_BRIDGE_FIXTURE ?? "") as {
  url: string; token: string; origin: string; iid: number; boundary: "tick" | "iteration"; scratch: string;
  /** Absent: the #2733 overlay-then-chain turn. Present (issue #3242): the turn starts on main's tip,
   *  origin's main advances by `advance` ("none", a plain file, a workflow change, or deleting files the
   *  base has), optionally the run commits (`commit`), and a tick publishes. With `descendant` the run
   *  then commits once more and a second tick publishes the next descendant. */
  stale?: { advance: "none" | "plain" | "workflow" | "delete"; commit: boolean; descendant: boolean; pause?: boolean };
  /** A seeded claim (plan supplied at create time, approved, no planning turn). */
  seeded?: boolean;
  /** Issue #3284: origin already carries refs/heads/<branch> = seed and no checkpoint ref; the claim is
   *  seeded with planned_base_commit = seed and require_base_match = true. Zero new work throughout.
   *  "tick": main advances ("none", a plain file or a workflow change), then one tick publishes.
   *  "pause": a first tick (the Go side answers it superseded) fetches the tracking ref back, main
   *  advances, then the run pauses through the real pause publish. */
  originSeeded?: { seed: string; advance: "none" | "plain" | "workflow"; mode: "tick" | "pause" };
};
assert.ok(path.isAbsolute(cfg.scratch));
assert.ok(path.isAbsolute(cfg.origin));
process.umask(0o077);
const local = fs.mkdtempSync(path.join(cfg.scratch, "worker-"));
const INTERVAL_MS = 1_000;
const TICK_MS = 424_242;
const workflow = ".github/workflows/ci.yml";

const gitEnv = {
  ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const ident = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: gitEnv, encoding: "utf8" }).trim();
}
function commitIn(tree: string, file: string, content: string): string {
  fs.mkdirSync(path.dirname(path.join(tree, file)), { recursive: true });
  fs.writeFileSync(path.join(tree, file), content);
  gitIn(tree, ["add", "-A"]);
  gitIn(tree, [...ident, "commit", "-m", `work ${file}`]);
  return gitIn(tree, ["rev-parse", "HEAD"]);
}
/** A ref's OID in a repo, or "" when absent. */
function tryRef(dir: string, ref: string): string {
  try { return gitIn(dir, ["rev-parse", "--verify", "--quiet", ref]); } catch { return ""; }
}
/** Advance main's workflow in the Go-owned bare origin from a scratch clone, so the agent branch
 *  is behind on .github/workflows and the overlay path runs. */
function advanceOriginWorkflow(): void {
  advanceOrigin(workflow, `name: ci\non: push\njobs: {}\n# advanced ${cfg.iid}\n`);
}
/** Advance main in the Go-owned bare origin by one commit removing paths (files or directories). */
function removeOnOrigin(paths: string[]): string {
  const work = path.join(local, "origin-work");
  if (!fs.existsSync(work)) execFileSync("git", ["clone", "-q", cfg.origin, work], { env: gitEnv });
  gitIn(work, ["rm", "-r", "-q", "--", ...paths]);
  gitIn(work, [...ident, "commit", "-m", "remove " + paths.join(" ")]);
  gitIn(work, ["push", "-q", "origin", "HEAD:refs/heads/main"]);
  return gitIn(work, ["rev-parse", "HEAD"]);
}
/** Advance main in the Go-owned bare origin by one commit to file, from a scratch clone. */
function advanceOrigin(file: string, content: string): string {
  const work = path.join(local, "origin-work");
  if (!fs.existsSync(work)) execFileSync("git", ["clone", "-q", cfg.origin, work], { env: gitEnv });
  const tip = commitIn(work, file, content);
  gitIn(work, ["push", "-q", "origin", "HEAD:refs/heads/main"]);
  return tip;
}

// The runner's worker client talks to the in-process FakeApi; only publishCheckpoint is delegated to
// a second REAL WorkerClient aimed at the Go server.
const api = new FakeApi(cfg.token);
const apiUrl = await api.listen();
const client = new WorkerClient(apiUrl, cfg.token, "0.1.0-test", nullLogger(), {
  sleep: async () => {}, terminalRetrySchedule: [1, 1],
});
const goClient = new WorkerClient(cfg.url, cfg.token, "0.1.0-test", nullLogger(), {
  sleep: async () => {}, terminalRetrySchedule: [1, 1],
});
const tips: string[] = [];
const results: unknown[] = [];
(client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
  ...args: Parameters<WorkerClient["publishCheckpoint"]>
) => {
  const [, tipOid] = args;
  tips.push(tipOid);
  // Every argument is forwarded (claim generation, attested source); only this RPC reaches Go.
  const res = await goClient.publishCheckpoint(...args);
  results.push(res);
  return res;
};

// Fake clock + tick timer, as control() in test/runner-midturn-checkpoint.test.ts.
let fakeNow = 0;
let tickCb: (() => void) | undefined;
let outcomeWaiter: ((o: string) => void) | undefined;
const hooks: NonNullable<RunnerOptions["checkpointTestHooks"]> = {
  tickKillGraceMs: 300,
  onTickOutcome: (o) => { outcomeWaiter?.(o); },
};
async function fireTick(): Promise<string> {
  assert.ok(tickCb, "the tick timer is armed");
  const done = new Promise<string>((resolve) => { outcomeWaiter = resolve; });
  tickCb();
  return await done;
}

const gitCache = new GitCache(path.join(local, "data"), nullLogger(), undefined,
  testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
const { github } = fakeGitHub();
const branch = `agent/issue-${cfg.iid}`;
const summary: Record<string, unknown> = { branch, boundary: cfg.boundary };
let turnError: unknown;

const runner = new RunRunner(client, gitCache, () => ({
  executor: {
    run: async (ctx: RunContext) => {
      try {
        const progress = { completed: ["m1"], in_progress: [] };
        if (cfg.originSeeded !== undefined) {
          const os = cfg.originSeeded;
          const ckpt = `refs/uzi-checkpoints/${branch}`;
          summary.baseTip = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
          summary.claimPlannedBase = claim.planned_base_commit;
          summary.claimRequireBase = claim.require_base_match;
          summary.claimCheckpointTip = claim.checkpoint_tip ?? null;
          summary.claimPlanSource = claim.plan_source;
          summary.checkpointRefBefore = tryRef(cfg.origin, ckpt);
          if (os.mode === "pause") {
            // The first tick establishes the tracking ref; the Go side answers its publish superseded.
            fakeNow += INTERVAL_MS + 1;
            summary.firstTickOutcome = await fireTick();
          }
          // The floor is already resolved (the runner's evaluateRequiredBase ran before executor.run).
          if (os.advance === "workflow") {
            summary.mainTip = advanceOrigin(workflow, `name: ci\non: push\njobs: {}\n# advanced ${cfg.iid}\n`);
          } else if (os.advance === "plain") {
            summary.mainTip = advanceOrigin("upstream.txt", `upstream ${cfg.iid}\n`);
          }
          if (os.mode === "tick") {
            fakeNow += INTERVAL_MS + 1;
            summary.tickOutcome = await fireTick();
          } else {
            const at = { completedCount: 0, total: 1 };
            summary.parked = await ctx.parkForPause!(at);
          }
          summary.checkpointRefAfter = tryRef(cfg.origin, ckpt);
          // Read inside the turn: the run's own finish may push the branch afterwards.
          summary.branchAfter = tryRef(cfg.origin, `refs/heads/${branch}`);
          summary.tips = tips;
          summary.results = results;
          return summary.parked ? { branch: ctx.branch, pausedAt: { completedCount: 0, total: 1 } } : { branch: ctx.branch };
        }
        if (cfg.stale !== undefined) {
          // Issue #3242: the run starts on main's tip; origin's main then moves past that base.
          const stale = cfg.stale;
          summary.baseTip = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
          if (stale.pause) {
            // A tick while main is still the base establishes the tracking ref (its publish is not
            // confirmed: the Go side answers it with a skip); then main moves and the run, still
            // without a commit, pauses through the real non-pinned pause publish.
            fakeNow += INTERVAL_MS + 1;
            summary.tickOutcome = await fireTick();
            summary.mainTip = advanceOrigin("upstream.txt", `upstream ${cfg.iid}\n`);
            const at = { completedCount: 0, total: 1 };
            summary.parked = await ctx.parkForPause!(at);
            summary.tips = tips;
            summary.results = results;
            return summary.parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
          }
          if (stale.advance === "workflow") {
            summary.mainTip = advanceOrigin(workflow, `name: ci\non: push\njobs: {}\n# advanced ${cfg.iid}\n`);
          } else if (stale.advance === "plain") {
            summary.mainTip = advanceOrigin("upstream.txt", `upstream ${cfg.iid}\n`);
          } else if (stale.advance === "delete") {
            summary.mainTip = removeOnOrigin(["README.md", "docs"]);
          }
          if (stale.commit) summary.workTip = commitIn(ctx.worktreePath, "w.txt", "W\n");
          fakeNow += INTERVAL_MS + 1;
          summary.tickOutcome = await fireTick();
          if (stale.descendant) {
            summary.descendantTip = commitIn(ctx.worktreePath, "d.txt", "D\n");
            fakeNow += INTERVAL_MS + 1;
            summary.descendantOutcome = await fireTick();
          }
          summary.tips = tips;
          summary.results = results;
          return { branch: ctx.branch };
        }
        summary.realTip = commitIn(ctx.worktreePath, "r.txt", "R\n");
        advanceOriginWorkflow();
        await ctx.checkpoint!({ reap: true, progress });
        summary.overlay = tips[0];
        summary.overlayRef = gitIn(cfg.origin, ["rev-parse", "--verify", "--quiet", `refs/uzi-checkpoints/${branch}`]);
        summary.rawTip = commitIn(ctx.worktreePath, "n.txt", "N\n");
        fakeNow += INTERVAL_MS + 1;
        if (cfg.boundary === "tick") summary.tickOutcome = await fireTick();
        else await ctx.checkpoint!({ reap: false, progress });
        summary.tips = tips;
        summary.results = results;
      } catch (e) {
        turnError = e;
        throw e;
      }
      return { branch: ctx.branch };
    },
  },
}), nullLogger(), 20, undefined, {
  pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, prDescriptionHeadLagMs: 0,
  github, checkpointIntervalMs: INTERVAL_MS, checkpointTickIntervalMs: TICK_MS,
  setTimer: (cb, ms) => { const t = setTimeout(cb, ms); t.unref?.(); return () => clearTimeout(t); },
  setTickTimer: (cb) => {
    tickCb = cb;
    return () => { if (tickCb === cb) tickCb = undefined; };
  },
  now: () => fakeNow,
  checkpointTestHooks: hooks,
});

const claim = makeClaim({
  issue_iid: cfg.iid,
  ...(cfg.originSeeded ? { planned_base_commit: cfg.originSeeded.seed, require_base_match: true } : {}),
  issue_title: `Bridge ${cfg.iid}`,
  repo: { id: "r1", url: "https://github.com/org/repo", clone_url: cfg.origin, forge_type: "github" },
  secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
  ...(cfg.seeded || cfg.originSeeded ? { plan_source: "seeded" as const, plan_approved: true, plan_md: `# Seeded plan ${cfg.iid}\n` } : {}),
});
let code = 0;
try {
  await runner.execute(claim);
  if (turnError !== undefined) throw turnError;
  console.log("BRIDGE_SUMMARY " + JSON.stringify(summary));
} catch (e) {
  console.error("bridge fixture failed:", e);
  console.log("BRIDGE_SUMMARY " + JSON.stringify({ ...summary, error: String(e) }));
  code = 1;
} finally {
  runner.shutdown();
  await api.close();
}
process.exit(code);
