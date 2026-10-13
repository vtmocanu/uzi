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
/** Advance main's workflow in the Go-owned bare origin from a scratch clone, so the agent branch
 *  is behind on .github/workflows and the overlay path runs. */
function advanceOriginWorkflow(): void {
  const work = path.join(local, "origin-work");
  execFileSync("git", ["clone", "-q", cfg.origin, work], { env: gitEnv });
  commitIn(work, workflow, `name: ci\non: push\njobs: {}\n# advanced ${cfg.iid}\n`);
  gitIn(work, ["push", "-q", "origin", "HEAD:refs/heads/main"]);
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
  runId: string, tipOid: string, pack: Parameters<WorkerClient["publishCheckpoint"]>[2], signal?: AbortSignal,
) => {
  tips.push(tipOid);
  const res = await goClient.publishCheckpoint(runId, tipOid, pack, signal);
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
  issue_title: `Bridge ${cfg.iid}`,
  repo: { id: "r1", url: "https://github.com/org/repo", clone_url: cfg.origin, forge_type: "github" },
  secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
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
