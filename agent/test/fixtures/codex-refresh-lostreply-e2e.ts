// Launched by TestCodexRefreshLostReplyE2E. No fake API, state, permit, or recovery client.
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { WorkerClient } from "../../src/client.js";
import { GitCache } from "../../src/git.js";
import { GitLabClient } from "../../src/forge.js";
import { RunRunner } from "../../src/runner.js";
import type { Executor, ExecutorResult, RunContext } from "../../src/executor.js";
import type { Logger } from "../../src/log.js";
import { createCodexExecutionSafety } from "../../src/codex/safety.js";
import { buildRunLaneReconcile } from "../../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../../src/codex/registry.js";
import { selectCodexBinding } from "../../src/codex/select.js";
import { CodexSessionStore } from "../../src/codex/session-state.js";
import { scopedRealView } from "../fake-proc.js";
import { setQuiescenceViewForTests } from "../../src/run-quiescence.js";

const cfg = JSON.parse(process.env.UZI_LOSTREPLY_FIXTURE ?? "") as {
  url: string; token: string; run: string; variant: string; origin: string; scratch: string;
};
assert.ok(path.isAbsolute(cfg.scratch));
assert.ok(path.isAbsolute(cfg.origin));
const local = fs.mkdtempSync(path.join(cfg.scratch, "worker-"));
setQuiescenceViewForTests(scopedRealView());
const lines: string[] = [];
const log: Logger = {
  debug: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  info: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  warn: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  error: (message, fields) => { lines.push(JSON.stringify({ message, fields })); },
  addSecret: () => {}, removeSecret: () => {}, child: () => log,
};
const gitEnv = {
  ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
  GIT_CONFIG_COUNT: "3",
  GIT_CONFIG_KEY_0: "maintenance.auto", GIT_CONFIG_VALUE_0: "false",
  GIT_CONFIG_KEY_1: "gc.auto", GIT_CONFIG_VALUE_1: "0",
  GIT_CONFIG_KEY_2: "core.fsmonitor", GIT_CONFIG_VALUE_2: "false",
};
function gitAt(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: gitEnv, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
}
const originSeed = path.join(local, "seed");
execFileSync("git", ["init", "-b", "main", originSeed], { env: gitEnv, stdio: "pipe" });
fs.writeFileSync(path.join(originSeed, "README.md"), "# combined proof\n");
gitAt(originSeed, ["add", "README.md"]);
const ident = ["-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false"];
gitAt(originSeed, [...ident, "commit", "-m", "initial"]);
execFileSync("git", ["clone", "--bare", originSeed, cfg.origin], { env: gitEnv, stdio: "pipe" });

// Offline forge transport: rewrite only this fixture's HTTPS remote to its bare origin.
const forgeGitConfig = path.join(local, "forge-git-config");
fs.writeFileSync(forgeGitConfig, `[url "${cfg.origin}"]\n\tinsteadOf = https://forge.example/fixture.git\n`);
process.env.GIT_CONFIG_GLOBAL = forgeGitConfig;
gitEnv.GIT_CONFIG_GLOBAL = forgeGitConfig;

const client = new WorkerClient(cfg.url, cfg.token, "0.1.0-test", log, {
  terminalRetrySchedule: [5, 5], codexHTTPTimeoutMs: 2000,
});
const cache = new GitCache(path.join(local, "data"), log);
let mrCreates = 0;
let description = "";
let finalHead = "";
const forge = new GitLabClient({
  fetchFn: async (url, init) => {
    if (init.method === "POST" && url.includes("/merge_requests")) {
      mrCreates++;
      finalHead = gitAt(cfg.origin, ["rev-parse", "refs/heads/agent/issue-1770"]);
      description = String((JSON.parse(init.body ?? "{}") as { description?: string }).description ?? "");
      return { status: 201, text: async () => JSON.stringify({ iid: 42, web_url: "https://forge.example/fixture/merge_requests/42" }) };
    }
    if (init.method === "GET") return { status: 200, text: async () => JSON.stringify({
      sha: finalHead, target_branch: "main", description, state: "opened",
    }) };
    if (init.method === "PUT") {
      description = String((JSON.parse(init.body ?? "{}") as { description?: string }).description ?? "");
      return { status: 200, text: async () => "{}" };
    }
    throw new Error("unexpected forge edge");
  },
});
type Snapshot = {
  status: string; cause: string | null; session: string | null; slot: boolean;
  coord: string; generation: number; holds: number; captures: number;
  issued: number; consumed: number; permit_requests: number; inputs: string[]; discover: number;
};
async function control(action: string): Promise<Snapshot> {
  const response = await fetch(cfg.url + "/fixture/" + action, {
    method: "POST", headers: { Authorization: "Bearer " + cfg.token },
    signal: AbortSignal.timeout(10_000),
  });
  if (!response.ok) throw new Error("fixture control " + action + ": " + await response.text());
  return await response.json() as Snapshot;
}
const session = randomUUID();
const home = path.join(local, "home");
fs.mkdirSync(home);
const epoch = path.join(local, "model-session");
fs.mkdirSync(path.join(epoch, "sessions"), { recursive: true });
fs.writeFileSync(path.join(epoch, "sessions", "rollout-" + session + ".jsonl"),
  JSON.stringify({ type: "session_meta", payload: { id: session } }) + "\n");
let flight = 0;
let committedHead = "";
let resumeObserved = false;
let successfulBoundaries = 0;
const runner = new RunRunner(client, cache, (_runId, codex) => {
  flight++;
  assert.ok(codex);
  const selected = selectCodexBinding({ codex });
  assert.equal(selected.kind, "codex");
  if (selected.kind !== "codex") throw new Error("claim lacks Codex binding");
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(flight));
  const inner = createCodexExecutionSafety(
    registry,
    async () => { throw new Error("model spawn is replaced by fixture executor"); },
    buildRunLaneReconcile(cfg.run, client, selected.binding, () => {}),
    undefined,
    async (request) => {
      const [command, ...args] = request.argv;
      assert.ok(command);
      const child = spawn(command, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const terminal = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code, signal) => resolve({ code: code ?? (signal ? 128 : 1) }));
      });
      return {
        root: {
          kind: "boundary_action",
          reap: async () => { await terminal; return { ok: true }; },
          dispose: async () => { if (child.exitCode === null) child.kill("SIGKILL"); await terminal; },
        },
        stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
        waitChild: async () => terminal,
      };
    },
  );
  const safety = {
    kind: "codex" as const,
    withBoundary: ((request, action) => inner.withBoundary(request, async (permit) => {
      successfulBoundaries++;
      return await action(permit);
    })) as typeof inner.withBoundary,
    spawnBoundaryProcess: inner.spawnBoundaryProcess.bind(inner),
    dispose: inner.dispose.bind(inner),
  };
  const executor: Executor = {
    safety,
    settleForCredentialFreeCapture: (ms) => inner.settleForCredentialFreeCapture(ms),
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      if (flight === 1) {
        ctx.onSessionId?.(session);
        await CodexSessionStore.persist(epoch, path.join(home, "codex-session-store"));
        fs.writeFileSync(path.join(ctx.worktreePath, "COMMITTED.txt"), "committed before refresh\n");
        gitAt(ctx.worktreePath, ["add", "COMMITTED.txt"]);
        gitAt(ctx.worktreePath, [...ident, "commit", "-m", "committed work"]);
        committedHead = gitAt(ctx.worktreePath, ["rev-parse", "HEAD"]);
        fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "dirty before refresh\n");
      } else {
        assert.equal(ctx.sessionId, session, "real reclaim preserved the session");
        assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "COMMITTED.txt"), "utf8"), "committed before refresh\n");
        assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "utf8"), "dirty before refresh\n");
        assert.equal(gitAt(ctx.worktreePath, ["merge-base", "--is-ancestor", committedHead, "HEAD"]), "");
        assert.ok(gitAt(ctx.worktreePath, ["status", "--porcelain"]).includes("DIRTY.txt"), "dirty work restored as work, not a shipped WIP marker");
        resumeObserved = true;
        gitAt(ctx.worktreePath, ["add", "DIRTY.txt"]);
        gitAt(ctx.worktreePath, [...ident, "commit", "-m", "finish recovered work"]);
        assert.equal(ctx.completionInterlock, true);
        assert.ok(ctx.recordCompletionAttempt);
        const attempt = await ctx.recordCompletionAttempt({
          declared: ["M3"], head: gitAt(ctx.worktreePath, ["rev-parse", "HEAD"]), worktreeFingerprint: null,
        });
        assert.deepEqual(attempt.unmet, []);
        assert.ok(attempt.attemptCount > 0);
      }
      return { branch: ctx.branch };
    },
  };
  return { executor, homeDir: home };
}, log, 10, cfg.token, {
  gitlab: forge, recoveryRetryMs: 10, pollMs: 20, checkpointTickIntervalMs: 0,
  codexBoundaryDeadlineMs: 30_000, codexFinalizeBoundaryDeadlineMs: 30_000,
});
try {
  await client.register("lostreply", undefined, 1, [], [
    "codex_harness_v1", "codex_runtime_v2", "codex_completion_interlock_v1", "completion_interlock_v1",
    "recovery_archive_v1", "recovery_archive_v2", "credential_switch_v1", "codex_refresh_recovery_v1",
  ]);
  const first = await client.claimRun();
  assert.ok(first);
  assert.equal(first.run_id, cfg.run);
  assert.equal(first.config?.completion_contract_version, 1);
  await runner.execute(first);
  const parked = await control("inspect");
  assert.equal(parked.status, "recovery_wait");
  assert.equal(parked.cause, cfg.variant === "drop-first" ? "vault_locked" : null);
  assert.equal(parked.session, session);
  assert.deepEqual(parked.inputs, ["0"], "operation X spent the provider refresh exactly once");
  assert.equal(parked.discover, 0, "no sweep before pre-sweep assertions");
  assert.equal(parked.slot, cfg.variant !== "pending-retention");
  assert.equal(parked.issued, 0);
  assert.equal(parked.permit_requests, 0);
  assert.ok(parked.holds > 0);
  assert.equal(mrCreates, 0);
  assert.equal(successfulBoundaries, 0, "blocked reconcile minted no action authority");
  assert.equal(await CodexSessionStore.inspectSession(path.join(home, "codex-session-store"), session), "present");
  await control("recover");
  const resumed = await client.claimRun();
  assert.ok(resumed);
  assert.equal(resumed.run_id, first.run_id);
  assert.ok((resumed.claim_generation ?? 0) > (first.claim_generation ?? 0));
  assert.equal(resumed.secrets.codex?.auth_mode, "subscription");
  assert.ok(resumed.secrets.codex && "generation" in resumed.secrets.codex);
  assert.equal(resumed.secrets.codex.generation, 1);
  assert.ok(resumed.secrets.codex.capability !== first.secrets.codex?.capability, "reclaim minted a fresh capability");
  await runner.execute(resumed);
  assert.ok(resumeObserved);
  if (Number(mrCreates) !== 1) {
    const diagnostics = lines.join("\n").replaceAll(cfg.token, "[redacted]").replaceAll(first.secrets.codex?.capability ?? "absent", "[redacted]").replace(/(?:access|refresh)-canary-\d+/g, "[redacted]");
    throw new Error("MR create missing; worker diagnostics: " + diagnostics);
  }
  const completed = await control("inspect");
  assert.equal(completed.status, "completed");
  assert.equal(completed.consumed, 1, "real server permit consumed by terminal transaction");
  assert.equal(completed.holds, 0, "real custody settlement after publication");
  assert.ok(completed.inputs.length >= 2);
  assert.equal(completed.inputs[1], "1", "resume refreshed promoted lineage");
  assert.ok(description.includes("Closes #1770"), "exact-head verified full delivery");
  const logs = lines.join("\n");
  for (const canary of ["access-canary-", "refresh-canary-", cfg.token, first.secrets.codex?.capability ?? "", resumed.secrets.codex.capability]) {
    if (canary) assert.ok(!logs.includes(canary), "no credential canary in worker logs");
  }
  console.log("combined worker park, sweep, reclaim, completion and custody assertions passed: " + cfg.variant);
} finally {
  runner.shutdown();
  fs.rmSync(local, { recursive: true, force: true });
}
