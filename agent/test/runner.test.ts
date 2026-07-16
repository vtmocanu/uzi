import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage, SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import { FakeApi } from "./fake-api.js";
import { spawnDetached } from "../src/sdk-spawn.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { StubExecutor, PlanRejectedError, STUB_FAIL_SENTINEL, type Executor, type RunContext, type ExecutorResult } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { GitLabClient, type FetchFn } from "../src/gitlab.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import type { Logger } from "../src/log.js";
import type { UserInput } from "../src/protocol.js";

const TOKEN = "tkn-runner-123456";

let api: FakeApi;
let baseUrl: string;
let fx: Fixture;
let git: GitCache;
let client: WorkerClient;
let homeDir: string;

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  baseUrl = await api.listen();
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger());
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-runnerhome-"));
  client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    terminalRetrySchedule: [1, 1],
  });
});

afterEach(async () => {
  await api.close();
  fx.cleanup();
  fs.rmSync(homeDir, { recursive: true, force: true });
});

function worktreeDirFor(iid: number): string {
  const repoDir = path.basename(git.barePathFor(fx.originPath)).replace(/\.git$/, "");
  return path.join(fx.dataDir, "worktrees", repoDir, `issue-${iid}`);
}

function isAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function waitDead(pid: number, timeoutMs = 2000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (isAlive(pid) && Date.now() < deadline) await new Promise((r) => setTimeout(r, 10));
}

interface MrCall {
  url: string;
  method: string;
  headers: Record<string, string>;
  body?: string;
}

/** A GitLab client whose transport is captured; opens MR !42 with no network. */
function fakeGitlab(): { gitlab: GitLabClient; calls: MrCall[] } {
  const calls: MrCall[] = [];
  const fetchFn: FetchFn = async (url, init) => {
    calls.push({ url, method: init.method, headers: init.headers, body: init.body });
    return {
      status: 201,
      text: async () => JSON.stringify({ iid: 42, web_url: "https://gitlab.example.test/org/repo/-/merge_requests/42" }),
    };
  };
  return { gitlab: new GitLabClient({ fetchFn }), calls };
}

function runner(executor: Executor, gitlab: GitLabClient, joinToken?: string): RunRunner {
  // Wrap the single executor as a factory (PRD #42): each execute() gets it back.
  // The per-run-executor tests below inject a real per-run factory instead.
  return runnerWith(() => ({ executor }), gitlab, joinToken);
}

function runnerWith(makeExecutor: ExecutorFactory, gitlab: GitLabClient, joinToken?: string, log: Logger = nullLogger()): RunRunner {
  return new RunRunner(client, git, makeExecutor, log, 20, joinToken, {
    pollMs: 5,
    planApprovalTimeoutMs: 0, // disabled — the gate resolves from injected inputs
    gitlab,
  });
}

const gitlabClaim = (iid: number, overrides = {}) =>
  makeClaim({
    issue_iid: iid,
    issue_title: `Fix thing ${iid}`,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
    ...overrides,
  });

describe("RunRunner — worker-performed push + MR", () => {
  it("pushes the branch and opens an MR after the executor returns done", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(7);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    // Two `running` reports: the claim→running transition, then the post-checkout
    // one carrying the repo's detected agent roster (PRD #37).
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    const completed = api.states.find((s) => s.body.status === "completed")!.body;
    assert.strictEqual(completed.branch, "agent/issue-7");
    assert.strictEqual(completed.mr_iid, 42);

    // The MR was opened with the PAT in the PRIVATE-TOKEN header — never the URL
    // or the body (primary directive: the credential stays off argv/URL/logs).
    assert.strictEqual(calls.length, 1);
    const call = calls[0]!;
    assert.strictEqual(call.method, "POST");
    assert.match(call.url, /\/api\/v4\/projects\/org%2Frepo\/merge_requests$/);
    assert.strictEqual(call.headers["PRIVATE-TOKEN"], "fixture-forge-pat-000000");
    assert.ok(!call.url.includes("fixture-forge-pat"), "PAT must not be in the URL");
    assert.ok(!(call.body ?? "").includes("fixture-forge-pat"), "PAT must not be in the body");
    const body = JSON.parse(call.body ?? "{}");
    assert.strictEqual(body.source_branch, "agent/issue-7");
    assert.strictEqual(body.target_branch, "main");
    assert.match(body.description, /Closes #7/);
    assert.strictEqual(body.remove_source_branch, false);

    // The branch really landed on origin, and the worktree was torn down.
    const log = execFileSync("git", ["-C", fx.originPath, "log", "--oneline", "agent/issue-7"], { encoding: "utf8" });
    assert.ok(log.includes("uzi stub: work on issue #7"));
    assert.strictEqual(fs.existsSync(worktreeDirFor(7)), false);
  });

  it("tears down the sibling skills plugin dir with the worktree (PRD #16 M6 follow-up)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(11);
    const pluginDir = skillsPluginDir(worktreeDirFor(11));
    // An executor that materializes a plugin dir the way SdkExecutor does — the
    // runner's finally must remove it (it is OUTSIDE the worktree, so the worktree
    // teardown does not reach it).
    const exec: Executor = {
      run: async (ctx) => {
        fs.mkdirSync(pluginDir, { recursive: true });
        fs.writeFileSync(path.join(pluginDir, "marker"), "x");
        return { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab).execute(claim);
    assert.strictEqual(fs.existsSync(pluginDir), false, "skills plugin dir must be cleaned up");
    assert.strictEqual(fs.existsSync(worktreeDirFor(11)), false, "worktree still cleaned up");
  });

  it("reports failed and opens NO merge request when the executor throws", async () => {
    const { gitlab, calls } = fakeGitlab();
    const boom: Executor = { run: async () => { throw new Error("kaboom"); } };
    const claim = gitlabClaim(9);
    await runner(boom, gitlab).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    // The second `running` is the post-checkout roster report; the executor throws
    // after it.
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.ok(api.states.find((s) => s.body.status === "failed")?.body.failure_reason?.includes("kaboom"));
    assert.strictEqual(calls.length, 0, "no MR on failure");
    assert.strictEqual(fs.existsSync(worktreeDirFor(9)), false);
  });

  it("redacts and caps the failure_reason before it reaches reportState", async () => {
    // failure_reason bypasses the batcher's payload redactor (it goes straight to
    // reportState), so the runner scrubs it directly. A raw SDK error can carry a
    // live secret and run long — this is the defense-in-depth gap PRD #12 widened.
    const OAUTH = "dummy-oauth-secret-fail-000000";
    const PAT = "fixture-forge-pat-000000";
    const { gitlab, calls } = fakeGitlab();
    const boom: Executor = {
      run: async () => {
        throw new Error(`clone failed: oauth=${OAUTH} pat=${PAT} ${"x".repeat(1000)}`);
      },
    };
    const claim = gitlabClaim(41, { secrets: { forge_pat: PAT, anthropic_oauth_token: OAUTH } });
    await runner(boom, gitlab).execute(claim);

    const reason =
      api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body.failure_reason ?? "";
    assert.ok(!reason.includes(OAUTH), "the OAuth token must not reach reportState");
    assert.ok(!reason.includes(PAT), "the forge PAT must not reach reportState");
    assert.ok(reason.includes("***REDACTED***"), "the secret should be redacted in place");
    assert.ok(reason.length <= 512, `failure_reason must be capped at 512 (got ${reason.length})`);
    assert.strictEqual(calls.length, 0, "no MR on failure");
  });

  it("redacts the OAuth token AND the worker join token from emitted messages", async () => {
    const OAUTH = "dummy-oauth-token-runner-000000";
    const JOIN_TOKEN = "dummy-join-token-runner-111111";
    const { gitlab } = fakeGitlab();
    const leaky: Executor = {
      run: async (ctx) => {
        ctx.emit({ kind: "tool_result", agent: "coder", payload: { content: `oauth=${OAUTH} join=${JOIN_TOKEN}` } });
        return { branch: ctx.branch };
      },
    };
    const claim = gitlabClaim(11, { secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: OAUTH } });
    await runner(leaky, gitlab, JOIN_TOKEN).execute(claim);

    const tr = api.messages(claim.run_id).find((m) => m.kind === "tool_result")!;
    const serialized = JSON.stringify(tr.payload);
    assert.ok(!serialized.includes(OAUTH), "the OAuth token must not reach the API");
    assert.ok(!serialized.includes(JOIN_TOKEN), "the join token must not reach the API");
    assert.ok(serialized.includes("***REDACTED***"), "secrets should be redacted in place");
  });
});

// --- end-to-end: the real SdkExecutor driven through the runner --------------

function assistant(content: unknown[], sessionId = "sess-e2e"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content } } as unknown as SDKMessage;
}
function resultOk(sessionId = "sess-e2e"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}
/** A queryFn that yields the plan turn, then the done turn. */
function planThenDoneQuery(): SdkQueryFn {
  const scripts: SDKMessage[][] = [
    [assistant([{ type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: "# PLAN\n- do it" } }]), resultOk()],
    [assistant([{ type: "text", text: "done implementing" }, { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} }]), resultOk()],
  ];
  let i = 0;
  return (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      for (const m of script) yield m;
    })();
  };
}

function input(kind: UserInput["kind"], body?: string): UserInput {
  return { id: 1, kind, body: body ?? null };
}

describe("RunRunner — plan gate + steering end to end", () => {
  it("halts at awaiting_approval, resumes on approve, then completes with an MR", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(21);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }), gitlab).execute(claim);

    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    const statuses = states.map((s) => s.status);
    assert.ok(statuses.includes("awaiting_approval"), "run halted at the plan gate");
    assert.ok(statuses.includes("completed"), "run completed after approval");
    const gate = states.find((s) => s.status === "awaiting_approval")!;
    assert.match(gate.plan_md ?? "", /# PLAN/);
    // The plan was surfaced to the run stream as a `plan` message, once.
    const planMsgs = api.messages(claim.run_id).filter((m) => m.kind === "plan");
    assert.strictEqual(planMsgs.length, 1);
    // Completed with an MR on the agent branch (never main).
    const completed = states.find((s) => s.status === "completed")!;
    assert.strictEqual(completed.branch, "agent/issue-21");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(JSON.parse(calls[0]!.body ?? "{}").source_branch, "agent/issue-21");
    // An iteration heartbeat was reported.
    assert.ok(states.some((s) => s.status === "running" && s.iteration_count === 1));
  });

  it("stub executor with planGate halts at awaiting_approval, then completes on approve", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(24);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "awaiting_approval", "running", "completed"]);
    const gate = api.states.find((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval")!.body;
    assert.match(gate.plan_md ?? "", /Stub plan for issue #24/);
    const completed = api.states.find((s) => s.body.status === "completed")!.body;
    assert.strictEqual(completed.branch, "agent/issue-24");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(calls.length, 1, "one MR opened after approval");
    // The plan reached the run stream exactly once.
    assert.strictEqual(api.messages(claim.run_id).filter((m) => m.kind === "plan").length, 1);
  });

  it("auto-approves the plan gate for an autopilot claim: never awaiting_approval, plan still recorded", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(26, { auto_approve: true });
    // No inputs are set: an autopilot run must resolve the gate itself. If it
    // parked at awaiting_approval it would hang until the (disabled) timeout.
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.ok(!statuses.includes("awaiting_approval"), "autopilot run never enters awaiting_approval");
    assert.strictEqual(statuses.at(-1), "completed", "autopilot run runs to completion");
    // The plan is still recorded as an audit message even though no human saw it.
    assert.strictEqual(api.messages(claim.run_id).filter((m) => m.kind === "plan").length, 1);
    const completed = api.states.find((s) => s.body.status === "completed")!.body;
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(calls.length, 1, "one MR opened after auto-approval");
  });

  it("throws on the UZI_STUB_FAIL sentinel after the gate (drives the E2E failure path)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(28, {
      auto_approve: true,
      issue_description: `implement prds/x.md then ${STUB_FAIL_SENTINEL}`,
    });
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.ok(!statuses.includes("awaiting_approval"), "auto-approved before it fails");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failed, "sentinel run should fail");
    assert.match(failed!.body.failure_reason ?? "", /UZI_STUB_FAIL/);
    assert.strictEqual(calls.length, 0, "no MR when the run fails");
    // The plan is recorded before the failure (the throw is AFTER the gate).
    assert.strictEqual(api.messages(claim.run_id).filter((m) => m.kind === "plan").length, 1);
  });

  it("still halts a NON-autopilot claim at the gate (auto_approve absent)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(27); // no auto_approve
    api.setInputs(claim.run_id, [input("approve_plan")]);
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "awaiting_approval", "running", "completed"]);
  });

  it("stub executor with planGate fails verbatim when the plan is rejected", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(25);
    api.setInputs(claim.run_id, [input("reject_plan", "not this way")]);
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.strictEqual(failed!.body.failure_reason, "not this way");
    assert.strictEqual(calls.length, 0, "no MR on rejection");
  });

  it("fails with the rejection reason (verbatim) when the plan is rejected", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(22);
    api.setInputs(claim.run_id, [input("reject_plan", "please rethink the approach")]);
    await runner(new SdkExecutor(nullLogger(), homeDir, { queryFn: planThenDoneQuery() }), gitlab).execute(claim);

    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failed, "run should have failed");
    assert.strictEqual(failed!.body.failure_reason, "please rethink the approach");
    assert.strictEqual(calls.length, 0, "no MR on rejection");
  });

  it("propagates a PlanRejectedError as a verbatim failure through a custom executor", async () => {
    const { gitlab } = fakeGitlab();
    const rejectExec: Executor = {
      run: async (ctx) => {
        const v = await ctx.gatePlan!("PLAN");
        if (v.kind === "reject") throw new PlanRejectedError(v.reason);
        return { branch: ctx.branch };
      },
    };
    const claim = gitlabClaim(23);
    api.setInputs(claim.run_id, [input("reject_plan", "no")]);
    await runner(rejectExec, gitlab).execute(claim);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.strictEqual(failed!.body.failure_reason, "no");
  });

  it("reaps the agent tree BEFORE the PAT-bearing push (B1 ordering)", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const exec: Executor = {
      run: async (ctx) => ({ branch: ctx.branch }),
      killAgentTree: () => events.push("kill"),
    };
    const claim = gitlabClaim(31);
    const origPush = git.pushBranch.bind(git);
    (git as unknown as { pushBranch: unknown }).pushBranch = async (...args: unknown[]) => {
      events.push("push");
      return (origPush as (...a: unknown[]) => Promise<void>)(...args);
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      (git as unknown as { pushBranch: unknown }).pushBranch = origPush;
    }
    assert.ok(events.includes("kill") && events.includes("push"));
    assert.ok(events.indexOf("kill") < events.indexOf("push"), "kill must precede push");
  });

  it("kills a real agent-backgrounded survivor before the run completes (B1)", async () => {
    const { gitlab } = fakeGitlab();
    const survivors: number[] = [];
    // Injected spawn stands in for the SDK CLI spawn, launching a real detached
    // `sleep` in its own group — the kind of survivor a `nohup … &` leaves behind.
    const spawn = (_opts: SpawnOptions): { pid?: number } => {
      const p = spawnDetached({ command: "sleep", args: ["30"] } as SpawnOptions);
      if (p.pid) survivors.push(p.pid);
      return p;
    };
    // Real plan→done query that also triggers the spawn hook each turn.
    const scripts: SDKMessage[][] = [
      [assistant([{ type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: "# P" } }]), resultOk()],
      [assistant([{ type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} }]), resultOk()],
    ];
    let turn = 0;
    const queryFn: SdkQueryFn = (params) => {
      const script = scripts[Math.min(turn, scripts.length - 1)]!;
      turn++;
      return (async function* () {
        params.options.spawnClaudeCodeProcess?.({ command: "x", args: [] } as never);
        for await (const _ of params.prompt) { /* drain */ }
        for (const m of script) yield m;
      })();
    };
    const claim = gitlabClaim(32);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    const exec = new SdkExecutor(nullLogger(), homeDir, { queryFn, spawn });
    try {
      await runner(exec, gitlab).execute(claim);
      assert.ok(survivors.length >= 1, "at least one survivor was spawned");
      for (const pid of survivors) {
        await waitDead(pid);
        assert.strictEqual(isAlive(pid), false, `survivor ${pid} must be dead after the run`);
      }
      // The run still completed with an MR (the reap did not disturb the happy path).
      assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
    } finally {
      for (const pid of survivors) try { process.kill(-pid, "SIGKILL"); } catch { /* already gone */ }
    }
  });

  it("cancels a live run: a cancel input aborts the executor's signal and fails the run", async () => {
    const { gitlab, calls } = fakeGitlab();
    // An executor that runs until cancelled through ctx.signal.
    const waiter: Executor = {
      run: (ctx) =>
        new Promise((_resolve, reject) => {
          const onAbort = (): void => reject(new Error("run cancelled"));
          if (ctx.signal?.aborted) return onAbort();
          ctx.signal?.addEventListener("abort", onAbort, { once: true });
        }),
    };
    const claim = gitlabClaim(24);
    api.setInputs(claim.run_id, [input("cancel")]);
    await runner(waiter, gitlab).execute(claim);

    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failed, "cancelled run reports failed");
    assert.match(failed!.body.failure_reason ?? "", /run cancelled/);
    assert.strictEqual(calls.length, 0, "no MR on cancel");
  });
});

// --- PRD #42 M1: per-run executor + HOME isolation + secret eviction ----------

interface Deferred {
  promise: Promise<void>;
  resolve: () => void;
}
function deferred(): Deferred {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => (resolve = () => r()));
  return { promise, resolve };
}

/** Resolves once `n` participants have arrived (idempotent past n). */
function barrier(n: number): { arrive: () => void; ready: Promise<void> } {
  let count = 0;
  const d = deferred();
  return { arrive: () => { if (++count >= n) d.resolve(); }, ready: d.promise };
}

/**
 * A fake executor modelling the B1 reap: run() "spawns" its injected pids into a
 * PRIVATE set (mirroring SdkExecutor.spawnedPids), signals it is mid-run, then
 * blocks on a gate so the test can hold two runs live at once; killAgentTree reaps
 * ITS set into a shared log and clears it. One instance per run (from the factory)
 * is the whole point — a single shared instance would let one run's reap clear
 * another's set (the pre-#42 hazard).
 */
class FakeReapExecutor implements Executor {
  private readonly live = new Set<number>();
  constructor(
    private readonly runId: string,
    private readonly injected: readonly number[],
    private readonly killLog: Array<{ runId: string; pid: number }>,
    private readonly onSpawned: () => void,
    private readonly gate: Promise<void>,
  ) {}
  async run(ctx: RunContext): Promise<ExecutorResult> {
    for (const p of this.injected) this.live.add(p);
    this.onSpawned();
    await this.gate;
    return { branch: ctx.branch };
  }
  killAgentTree(): void {
    for (const pid of this.live) this.killLog.push({ runId: this.runId, pid });
    this.live.clear();
  }
  livePids(): number[] {
    return [...this.live].sort((a, b) => a - b);
  }
}

/** A logger that records every secret registered/evicted, for the eviction tests. */
function secretRecordingLogger(): { logger: Logger; added: string[]; removed: string[] } {
  const added: string[] = [];
  const removed: string[] = [];
  const self: Logger = {
    debug() {}, info() {}, warn() {}, error() {},
    addSecret: (s) => { added.push(s); },
    removeSecret: (s) => { removed.push(s); },
    child: () => self,
  };
  return { logger: self, added, removed };
}

describe("RunRunner — per-run executor isolation (PRD #42 Decision 4)", () => {
  it("two concurrent runs each reap ONLY their own subprocess set; a sibling is untouched", async () => {
    const { gitlab } = fakeGitlab();
    const claimA = gitlabClaim(51);
    const claimB = gitlabClaim(52);
    const pidsByRun: Record<string, number[]> = {
      [claimA.run_id]: [7001, 7002],
      [claimB.run_id]: [8001, 8002],
    };
    const killLog: Array<{ runId: string; pid: number }> = [];
    const bothSpawned = barrier(2);
    const gates: Record<string, Deferred> = {
      [claimA.run_id]: deferred(),
      [claimB.run_id]: deferred(),
    };
    const execs = new Map<string, FakeReapExecutor>();
    const factoryCalls: string[] = [];
    const factory: ExecutorFactory = (runId) => {
      factoryCalls.push(runId);
      const e = new FakeReapExecutor(runId, pidsByRun[runId]!, killLog, bothSpawned.arrive, gates[runId]!.promise);
      execs.set(runId, e);
      return { executor: e };
    };
    const rnr = runnerWith(factory, gitlab);

    const errs: unknown[] = [];
    // If an execute rejects before reaching the executor, still trip the barrier so
    // the test asserts (and fails clearly) rather than hanging on bothSpawned.ready.
    const pA = rnr.execute(claimA).catch((e) => { errs.push(e); bothSpawned.arrive(); });
    const pB = rnr.execute(claimB).catch((e) => { errs.push(e); bothSpawned.arrive(); });
    try {
      await bothSpawned.ready;
      assert.deepStrictEqual(errs, [], "both runs reached the executor without erroring");
      // The factory built a DISTINCT executor per run (per-run construction).
      assert.deepStrictEqual([...factoryCalls].sort(), [claimA.run_id, claimB.run_id].sort());
      assert.notStrictEqual(execs.get(claimA.run_id), execs.get(claimB.run_id));
      // Both runs are mid-run with their own sets populated.
      assert.deepStrictEqual(execs.get(claimA.run_id)!.livePids(), [7001, 7002]);
      assert.deepStrictEqual(execs.get(claimB.run_id)!.livePids(), [8001, 8002]);

      // Release run A → its pre-push reap kills EXACTLY A's tree and clears A's set.
      gates[claimA.run_id]!.resolve();
      await pA;
      assert.deepStrictEqual(killLog.map((k) => k.pid).sort((a, b) => a - b), [7001, 7002]);
      // The SIBLING (B) is untouched: its set is intact, none of its pids reaped.
      assert.deepStrictEqual(execs.get(claimB.run_id)!.livePids(), [8001, 8002]);
      assert.deepStrictEqual(execs.get(claimA.run_id)!.livePids(), []);

      // Release run B → it now reaps exactly its own set.
      gates[claimB.run_id]!.resolve();
      await pB;
    } finally {
      gates[claimA.run_id]!.resolve();
      gates[claimB.run_id]!.resolve();
      await Promise.allSettled([pA, pB]);
    }

    // End state: every reap targeted only its own run's pids — nothing crossed over.
    for (const k of killLog) {
      assert.ok(pidsByRun[k.runId]!.includes(k.pid), `run ${k.runId} reaped foreign pid ${k.pid}`);
    }
    const grouped = (id: string) => killLog.filter((k) => k.runId === id).map((k) => k.pid).sort((a, b) => a - b);
    assert.deepStrictEqual(grouped(claimA.run_id), [7001, 7002]);
    assert.deepStrictEqual(grouped(claimB.run_id), [8001, 8002]);
    // Both runs completed with an MR (the reaps did not disturb the happy path).
    for (const c of [claimA, claimB]) {
      assert.ok(
        api.states.some((s) => s.runId === c.run_id && s.body.status === "completed"),
        `run ${c.issue_iid} completed`,
      );
    }
  });

  it("gives each run its own HOME (agent-home/<runId>) and removes it on terminal (Decision 5)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-runhome-"));
    const created: string[] = [];
    const factory: ExecutorFactory = (runId) => {
      const runHome = path.join(homeRoot, runId);
      created.push(runHome);
      const executor: Executor = {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          // Mirror SdkExecutor: create the HOME and write a run-private file in it.
          await fs.promises.mkdir(runHome, { recursive: true });
          await fs.promises.writeFile(path.join(runHome, "session"), ctx.runId, "utf8");
          return { branch: ctx.branch };
        },
      };
      return { executor, homeDir: runHome };
    };
    const claimA = gitlabClaim(55);
    const claimB = gitlabClaim(56);
    try {
      await Promise.all([runnerWith(factory, gitlab).execute(claimA), runnerWith(factory, gitlab).execute(claimB)]);
      // Two distinct, run-id-scoped HOMEs were built (no shared dir).
      assert.strictEqual(created.length, 2);
      assert.deepStrictEqual(
        created.map((p) => path.basename(p)).sort(),
        [claimA.run_id, claimB.run_id].sort(),
      );
      // Each was removed on terminal (the runner's finally cleaned every run's HOME).
      for (const h of created) {
        assert.strictEqual(fs.existsSync(h), false, `HOME ${h} must be removed on terminal`);
      }
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("registers the run's secrets and evicts them all on terminal (Decision 7)", async () => {
    const { gitlab } = fakeGitlab();
    const { logger, added, removed } = secretRecordingLogger();
    const PAT = "fixture-forge-pat-evict-001";
    const OAUTH = "dummy-oauth-evict-000000";
    const claim = gitlabClaim(60, {
      secrets: { forge_pat: PAT, anthropic_oauth_token: OAUTH, forge_username: "bot" },
    });
    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, logger).execute(claim);

    assert.ok(added.includes(PAT), "PAT registered");
    assert.ok(added.includes(OAUTH), "OAuth token registered");
    // Every registered run-secret (PAT, OAuth token, and the derived git Basic
    // credential) is evicted on terminal — nothing lingers in the process set.
    for (const s of added) {
      assert.ok(removed.includes(s), `run secret ${JSON.stringify(s)} must be evicted on terminal`);
    }
  });

  it("still evicts the run's secrets when the run FAILS (Decision 7 — finally path)", async () => {
    const { gitlab } = fakeGitlab();
    const { logger, added, removed } = secretRecordingLogger();
    const boom: Executor = { run: async () => { throw new Error("kaboom"); } };
    const claim = gitlabClaim(61, {
      secrets: { forge_pat: "fixture-forge-pat-evict-002", anthropic_oauth_token: "dummy-oauth-evict-111111" },
    });
    await runnerWith(() => ({ executor: boom }), gitlab, undefined, logger).execute(claim);

    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"), "run failed");
    assert.ok(added.length > 0, "secrets were registered");
    for (const s of added) {
      assert.ok(removed.includes(s), `failed run's secret ${JSON.stringify(s)} must still be evicted`);
    }
  });

  it("rejects a run whose id is not a UUID BEFORE it becomes a path (defense in depth)", async () => {
    const { gitlab } = fakeGitlab();
    let factoryReached = false;
    const factory: ExecutorFactory = () => {
      factoryReached = true;
      return { executor: new StubExecutor(nullLogger()) };
    };
    // Empty (would collapse the per-run HOME to the shared root), path separators,
    // and traversal (would escape it) — all must be refused before makeExecutor.
    for (const badId of ["", "../../etc", "a/b", "not-a-uuid"]) {
      await assert.rejects(
        runnerWith(factory, gitlab).execute(gitlabClaim(70, { run_id: badId })),
        /invalid run id/,
      );
    }
    assert.strictEqual(factoryReached, false, "the executor factory must not be reached for an invalid run id");
  });
});

describe("RunRunner — repo agent detection (PRD #37)", () => {
  /** Run one claim against an origin carrying `files`, returning the state reports
   *  and the status messages that reached the stream. */
  async function runAgainst(files: Record<string, string>) {
    const repoFx = makeFixture(files);
    try {
      const { gitlab } = fakeGitlab();
      const claim = makeClaim({
        issue_iid: 31,
        issue_title: "detect the roster",
        repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: repoFx.originPath },
        last_seq: 0,
        secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
      });
      const repoRunner = new RunRunner(client, new GitCache(repoFx.dataDir, nullLogger()), () => ({ executor: new StubExecutor(nullLogger()) }), nullLogger(), 20, undefined, {
        pollMs: 5,
        planApprovalTimeoutMs: 0,
        gitlab,
      });
      await repoRunner.execute(claim);
      return {
        states: api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body),
        texts: api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text)),
      };
    } finally {
      repoFx.cleanup();
    }
  }

  it("reports the parsed roster on a running report, noting every drop", async () => {
    const { states, texts } = await runAgainst({
      ".claude/agents/coder.md": "---\nname: coder\ndescription: Implements changes.\nmodel: opus\n---\n\nImplement it.\n",
      ".claude/agents/reviewer.md": "---\nname: reviewer\ndescription: Reviews changes.\ntools: Read, WebFetch\n---\n\nReview it.\n",
      ".claude/agents/broken.md": "not an agent file\n",
      // Never loaded through this path: only .claude/agents/*.md is read.
      ".claude/settings.json": '{"permissions":{"allow":["Bash(rm -rf /)"]}}',
    });

    const report = states.find((s) => s.repo_agents !== undefined);
    assert.ok(report, "a state report carries the roster");
    assert.strictEqual(report!.status, "running", "the roster rides a running report, not the gate");
    assert.deepStrictEqual(report!.repo_agents, [
      { name: "coder", description: "Implements changes." },
      { name: "reviewer", description: "Reviews changes." },
    ]);
    // Prompt bodies stay worker-side; only names + descriptions travel.
    assert.ok(!JSON.stringify(report!.repo_agents).includes("Implement it."));

    assert.ok(texts.some((t) => t.includes('repo agent "broken" was skipped')), texts.join("\n"));
    // WebFetch is HONORED now — reviewer keeps it, so NO tools_filtered note fires.
    assert.ok(!texts.some((t) => t.includes("removed WebFetch")), texts.join("\n"));
    assert.ok(texts.some((t) => t.includes("detected 2 agent(s)")), texts.join("\n"));
  });

  it("reports an empty roster (not an absent one) for a repo with no .claude/agents", async () => {
    const { states, texts } = await runAgainst({});
    const report = states.find((s) => s.repo_agents !== undefined);
    // `[]` is "detection ran, found none" — distinct from a pre-feature run's NULL.
    assert.deepStrictEqual(report?.repo_agents, []);
    assert.ok(!texts.some((t) => t.includes("repo agent")), "no notes when there is nothing to detect");
    assert.ok(states.some((s) => s.status === "completed"), "the run still completes");
  });

  it("keeps a detection FAILURE distinguishable from an empty roster (no repo_agents reported)", async () => {
    // A detection throw (e.g. an unreadable dir) must NOT be reported as `[]` (which
    // means "scanned, found none"). The worker sends no repo_agents at all, so the
    // column stays NULL, and it says so on the feed. The run still completes.
    const repoFx = makeFixture({});
    try {
      const { gitlab } = fakeGitlab();
      const claim = makeClaim({
        issue_iid: 32,
        issue_title: "detection fails",
        repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: repoFx.originPath },
        last_seq: 0,
        secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
      });
      const runner = new RunRunner(
        client,
        new GitCache(repoFx.dataDir, nullLogger()),
        () => ({ executor: new StubExecutor(nullLogger()) }),
        nullLogger(),
        20,
        undefined,
        {
          pollMs: 5,
          planApprovalTimeoutMs: 0,
          gitlab,
          detectRepoAgents: async () => {
            throw new Error("enumeration failed");
          },
        },
      );
      await runner.execute(claim);

      const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
      const texts = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text));
      // NO report carries repo_agents — not even `[]`. The column stays "not reported".
      assert.ok(!states.some((s) => s.repo_agents !== undefined), "a detection failure reports no roster");
      assert.ok(texts.some((t) => t.includes("could not read the repo's .claude/agents/")), texts.join("\n"));
      assert.ok(states.some((s) => s.status === "completed"), "the run still completes");
    } finally {
      repoFx.cleanup();
    }
  });

  it("autopilot resolves + reports the repo-agent default selection with a feed note (PRD #37)", async () => {
    // A repo shipping agents + an autopilot claim: the self-approve path resolves
    // the default to the repo source, reports it on a running report (the only
    // channel a no-input run has), and states it on the feed — never parking at the
    // gate.
    const repoFx = makeFixture({
      ".claude/agents/coder.md": "---\nname: coder\ndescription: c.\n---\n\nbody\n",
      ".claude/agents/reviewer.md": "---\nname: reviewer\ndescription: r.\n---\n\nbody\n",
    });
    try {
      const { gitlab } = fakeGitlab();
      const claim = makeClaim({
        issue_iid: 33,
        issue_title: "autopilot with repo agents",
        repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: repoFx.originPath },
        last_seq: 0,
        secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
        auto_approve: true,
      });
      const r = new RunRunner(
        client,
        new GitCache(repoFx.dataDir, nullLogger()),
        () => ({ executor: new StubExecutor(nullLogger(), { planGate: true }) }),
        nullLogger(),
        20,
        undefined,
        { pollMs: 5, planApprovalTimeoutMs: 0, gitlab },
      );
      await r.execute(claim);

      const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
      const texts = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text));
      const selectionState = states.find((s) => s.agent_selection !== undefined);
      assert.deepStrictEqual(selectionState?.agent_selection, { source: "repo", exclusions: [] }, "autopilot persisted the repo default");
      // F1: the autopilot selection report is self-contained — it carries the roster
      // alongside the selection, so a failed fire-and-forget roster report above does
      // not cost the attribution. (The own-source case below carries NO repo_agents.)
      assert.deepStrictEqual(
        selectionState?.repo_agents?.map((a) => a.name).sort(),
        ["coder", "reviewer"],
        "the repo-source autopilot selection report carries repo_agents",
      );
      assert.ok(
        texts.some((t) => t.includes("autopilot: using the 2 agent(s) from the repo's .claude/agents/")),
        texts.join("\n"),
      );
      assert.ok(!states.some((s) => s.status === "awaiting_approval"), "autopilot never parks at the gate");
      assert.ok(states.some((s) => s.status === "completed"), "the run still completes");
    } finally {
      repoFx.cleanup();
    }
  });

  it("autopilot with no repo agents resolves + reports the OWN default", async () => {
    const { states, texts } = await (async () => {
      const repoFx = makeFixture({});
      try {
        const { gitlab } = fakeGitlab();
        const claim = makeClaim({
          issue_iid: 36,
          issue_title: "autopilot no repo agents",
          repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: repoFx.originPath },
          last_seq: 0,
          secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
          auto_approve: true,
        });
        const r = new RunRunner(
          client,
          new GitCache(repoFx.dataDir, nullLogger()),
          () => ({ executor: new StubExecutor(nullLogger(), { planGate: true }) }),
          nullLogger(),
          20,
          undefined,
          { pollMs: 5, planApprovalTimeoutMs: 0, gitlab },
        );
        await r.execute(claim);
        return {
          states: api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body),
          texts: api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text)),
        };
      } finally {
        repoFx.cleanup();
      }
    })();
    const ownSelectionState = states.find((s) => s.agent_selection !== undefined);
    assert.deepStrictEqual(ownSelectionState?.agent_selection, {
      source: "own",
      exclusions: [],
    });
    // F1 guard: on the own default (no repo agents detected) the report must NOT carry
    // repo_agents — sending [] would flip the column from NULL ("not reported") to []
    // ("detected none") and erase that distinction.
    assert.strictEqual(ownSelectionState?.repo_agents, undefined, "the own-source autopilot report carries no repo_agents");
    assert.ok(texts.some((t) => t.includes("autopilot: using your own agent templates")), texts.join("\n"));
  });

  it("the MR description carries the repo-agents marker only when the run used repo agents", async () => {
    // A fake executor that reports which roster the implement phase ran with — the
    // stub does not, so the marker is driven directly here (the SDK executor sets it).
    const repoExec: Executor = {
      run: async (ctx) => ({ branch: ctx.branch, agentSelection: { source: "repo", agents: ["coder", "auditor"] } }),
    };
    const ownExec: Executor = {
      run: async (ctx) => ({ branch: ctx.branch, agentSelection: { source: "own", agents: ["coder"] } }),
    };

    const repoGl = fakeGitlab();
    await runner(repoExec, repoGl.gitlab).execute(gitlabClaim(34));
    const repoBody = JSON.parse(repoGl.calls[0]!.body ?? "{}");
    assert.match(repoBody.description, /repository's own `\.claude\/agents\/`/, "repo-source MR carries the marker");
    assert.match(repoBody.description, /coder, auditor/, "the marker names the roster");

    const ownGl = fakeGitlab();
    await runner(ownExec, ownGl.gitlab).execute(gitlabClaim(35));
    const ownBody = JSON.parse(ownGl.calls[0]!.body ?? "{}");
    assert.ok(!/repository's own/.test(ownBody.description), "an own-source MR has no repo marker");
  });
});
