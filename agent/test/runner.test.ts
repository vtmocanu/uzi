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
import { GitLabClient, ForgejoClient, type FetchFn } from "../src/forge.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import type { PlanVerdict } from "../src/steering.js";
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

// PRD #51 M3 (b): the run's working tree is the RUNNER CLONE under runner/, not a
// linked worktree under worktrees/.
function worktreeDirFor(iid: number): string {
  const repoDir = path.basename(git.barePathFor(fx.originPath)).replace(/\.git$/, "");
  return path.join(fx.dataDir, "runner", repoDir, `issue-${iid}`);
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

/** A Forgejo client whose transport is captured; opens PR #42 with no network. */
function fakeForgejo(): { forgejo: ForgejoClient; calls: MrCall[] } {
  const calls: MrCall[] = [];
  const fetchFn: FetchFn = async (url, init) => {
    calls.push({ url, method: init.method, headers: init.headers, body: init.body });
    return {
      status: 201,
      text: async () => JSON.stringify({ number: 42, html_url: "https://forgejo.example.test/org/repo/pulls/42" }),
    };
  };
  return { forgejo: new ForgejoClient({ fetchFn }), calls };
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
    // The forge-reported MR web URL is persisted on completion (PRD #65 D8), so the
    // web links it directly instead of reconstructing it.
    assert.strictEqual(completed.mr_web_url, "https://gitlab.example.test/org/repo/-/merge_requests/42");

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

  it("routes a forgejo claim to the Forgejo client and persists the PR web url (PRD #65 D9/D8)", async () => {
    const { forgejo, calls } = fakeForgejo();
    // A Forgejo run: same local clone_url (git is forge-agnostic), a Forgejo web url,
    // and forge_type=forgejo on the wire so the worker picks the Forgejo client.
    const claim = gitlabClaim(15, {
      repo: { id: "r1", url: "https://forgejo.example.test/org/repo", clone_url: fx.originPath, forge_type: "forgejo" },
    });
    const r = new RunRunner(client, git, () => ({ executor: new StubExecutor(nullLogger()) }), nullLogger(), 20, undefined, {
      pollMs: 5,
      planApprovalTimeoutMs: 0,
      forgejo,
    });
    await r.execute(claim);

    const completed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "completed")!.body;
    assert.strictEqual(completed.branch, "agent/issue-15");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(completed.mr_web_url, "https://forgejo.example.test/org/repo/pulls/42");

    // The PR was opened against Forgejo's /api/v1 pulls endpoint with the token header
    // — never the URL/body (primary directive).
    assert.strictEqual(calls.length, 1);
    const call = calls[0]!;
    assert.strictEqual(call.method, "POST");
    assert.match(call.url, /\/api\/v1\/repos\/org\/repo\/pulls$/);
    assert.strictEqual(call.headers["Authorization"], "token fixture-forge-pat-000000");
    assert.ok(!call.url.includes("fixture-forge-pat"), "PAT must not be in the URL");
    assert.ok(!(call.body ?? "").includes("fixture-forge-pat"), "PAT must not be in the body");
    const body = JSON.parse(call.body ?? "{}");
    assert.strictEqual(body.head, "agent/issue-15");
    assert.strictEqual(body.base, "main");
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

// --- PRD #41: plan revision at the approval gate -------------------------------
// The runner's gatePlan is called once per round; a `revise` verdict is returned to the
// executor (which runs a fresh plan turn), and the gate re-reports awaiting_approval,
// bumping the steering epoch AT the re-report (Decision 3) so a verdict written against
// the previous plan version goes stale. All rounds share ONE approval budget.
describe("RunRunner — plan revision at the gate (PRD #41)", () => {
  const pollTick = (ms = 5): Promise<void> => new Promise((r) => setTimeout(r, ms));

  /** Once the run has posted `n` awaiting_approval reports, submit `inputs`. The epoch is
   *  bumped right after each awaiting_approval RE-report (synchronously, before the gate
   *  awaits), so observing the n-th report proves the epoch has advanced to the round-n
   *  value — inputs set here are stamped at the new (current) epoch. */
  function onGateRound(runId: string, n: number, inputs: UserInput[]): Promise<void> {
    return (async () => {
      while (api.states.filter((s) => s.runId === runId && s.body.status === "awaiting_approval").length < n) {
        await pollTick();
      }
      api.setInputs(runId, inputs);
    })();
  }

  it("returns a revise verdict for a fresh round, then completes on the round-2 approve", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(80);
    const seen: PlanVerdictKind[] = [];
    const reviseExec: Executor = {
      run: async (ctx) => {
        const v1 = await ctx.gatePlan!("# PLAN v1");
        seen.push(v1.kind);
        assert.strictEqual(v1.kind, "revise");
        assert.strictEqual(v1.kind === "revise" ? v1.feedback : "", "tighten the scope");
        const v2 = await ctx.gatePlan!("# PLAN v2");
        seen.push(v2.kind);
        if (v2.kind !== "approve") throw new Error(`expected approve, got ${v2.kind}`);
        return { branch: ctx.branch };
      },
    };
    api.setInputs(claim.run_id, [input("revise_plan", "tighten the scope")]);
    const round2 = onGateRound(claim.run_id, 2, [input("approve_plan")]);
    await runner(reviseExec, gitlab).execute(claim);
    await round2;

    assert.deepStrictEqual(seen, ["revise", "approve"]);
    // The gate parked at awaiting_approval TWICE — once per plan version.
    const gates = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval").map((s) => s.body);
    assert.strictEqual(gates.length, 2);
    assert.match(gates[0]!.plan_md ?? "", /PLAN v1/);
    assert.match(gates[1]!.plan_md ?? "", /PLAN v2/);
    // The run completed with an MR after the round-2 approval.
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
    assert.strictEqual(calls.length, 1);
  });

  // The BLOCKING bug (PRD #41 Decision 3): the gate epoch must advance at the
  // awaiting_approval RE-report, NOT when a revise is taken. A revision planning turn
  // runs BETWEEN rounds; if the epoch is bumped when the revise is TAKEN, that whole
  // (minutes-long) window sits at the v2 epoch, so an approve clicked mid-revision is
  // stamped current and ACCEPTED at the v2 gate — implementing a plan v2 NO human saw.
  // This drives the REAL runner + REAL steering + REAL gatePlan (not a mocked gatePlan)
  // so it exercises the epoch machinery the sdk-executor mock bypasses. It FAILS against
  // the old settle-bump code (the mid-revision approve is accepted at v2, no stale
  // notice) and PASSES once the bump moves to the re-report.
  it("approve during the revision turn is discarded (mid-revision window)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(83);
    const seen: PlanVerdictKind[] = [];

    // Record when the steering poller actually CONSUMES the mid-revision approve, so the
    // executor waits for it before re-gating v2 — making the window deterministic (no
    // reliance on poll/HTTP timing). The approve is consumed while the epoch is still the
    // v1 epoch, so it must be stale at the v2 gate.
    const origGetInputs = client.getInputs.bind(client);
    let approveConsumed = false;
    (client as unknown as { getInputs: (r: string) => Promise<UserInput[]> }).getInputs = async (runId: string) => {
      const inputs = await origGetInputs(runId);
      if (inputs.some((i) => i.kind === "approve_plan")) approveConsumed = true;
      return inputs;
    };

    const reviseExec: Executor = {
      run: async (ctx) => {
        const v1 = await ctx.gatePlan!("# PLAN v1");
        seen.push(v1.kind);
        assert.strictEqual(v1.kind, "revise");
        // Mid-revision window: v1 returned revise, v2 is not yet re-reported. Inject an
        // approve of the SUPERSEDED plan now, and wait until the poller has CONSUMED it
        // (stamped at the still-current v1 epoch) before we re-gate v2.
        api.setInputs(ctx.runId, [input("approve_plan")]);
        while (!approveConsumed) await pollTick();
        // Re-gate v2: the re-report bumps the epoch, so the buffered approve is now stale.
        const v2 = await ctx.gatePlan!("# PLAN v2");
        seen.push(v2.kind);
        if (v2.kind !== "approve") throw new Error(`expected approve, got ${v2.kind}`);
        return { branch: ctx.branch };
      },
    };
    api.setInputs(claim.run_id, [input("revise_plan", "tighten the scope")]);
    // The FRESH v2 approve is injected only after the run RE-PARKS at the v2 gate (the 2nd
    // awaiting_approval report), so it is stamped at the new epoch and legitimately taken.
    const round2 = onGateRound(claim.run_id, 2, [input("approve_plan")]);
    try {
      await runner(reviseExec, gitlab).execute(claim);
      await round2;

      // The gate saw revise then approve — but the approve it acted on was the FRESH v2
      // one, not the discarded mid-revision one (asserted via the notice + re-park below).
      assert.deepStrictEqual(seen, ["revise", "approve"]);
      // The run RE-PARKED at v2 awaiting_approval (a 2nd report carrying the v2 plan_md) —
      // it did NOT proceed to implement on the strength of the mid-revision approve.
      const gates = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval").map((s) => s.body);
      assert.strictEqual(gates.length, 2, "the run re-parked at the v2 gate");
      assert.match(gates[1]!.plan_md ?? "", /PLAN v2/);
      // The mid-revision approve was DISCARDED with a stale feed notice (the bug's tell:
      // under the old code this notice is absent because the approve was accepted at v2).
      const texts = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text));
      assert.ok(texts.some((t) => t.includes("Approval ignored")), texts.join("\n"));
      // Only after the FRESH v2 approve did the run complete + open exactly one MR.
      assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
      assert.strictEqual(calls.length, 1);
    } finally {
      (client as unknown as { getInputs: unknown }).getInputs = origGetInputs;
    }
  });

  it("discards an approve batched with a revise (written against the pre-feedback plan) and notes it on the feed", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(81);
    const reviseExec: Executor = {
      run: async (ctx) => {
        const v1 = await ctx.gatePlan!("# PLAN v1");
        assert.strictEqual(v1.kind, "revise");
        const v2 = await ctx.gatePlan!("# PLAN v2");
        if (v2.kind !== "approve") throw new Error(`expected approve, got ${v2.kind}`);
        return { branch: ctx.branch };
      },
    };
    // The user's approve rides the SAME batch as the revise, so it is stamped against the
    // pre-feedback plan (epoch 1). Round 2 must NOT approve on it.
    api.setInputs(claim.run_id, [input("revise_plan", "rethink it"), input("approve_plan")]);
    const round2 = onGateRound(claim.run_id, 2, [input("approve_plan")]); // a fresh, round-2 approve
    await runner(reviseExec, gitlab).execute(claim);
    await round2;

    const texts = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text));
    assert.ok(texts.some((t) => t.includes("Approval ignored")), texts.join("\n"));
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
  });

  it("shares ONE approval budget across rounds: a revise does not reset the deadline", async () => {
    // With a tiny budget and a revision round that never approves, the run times out on
    // the SHARED deadline and fails with the timeout reason — the revise kept the clock
    // running rather than granting a fresh full budget.
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(82);
    const reviseExec: Executor = {
      run: async (ctx) => {
        const v1 = await ctx.gatePlan!("# PLAN v1");
        assert.strictEqual(v1.kind, "revise");
        const v2 = await ctx.gatePlan!("# PLAN v2"); // no approve ever comes → times out
        if (v2.kind === "reject") throw new PlanRejectedError(v2.reason);
        return { branch: ctx.branch };
      },
    };
    const r = new RunRunner(client, git, () => ({ executor: reviseExec }), nullLogger(), 20, undefined, {
      pollMs: 5,
      planApprovalTimeoutMs: 60, // small, shared across both rounds
      gitlab,
    });
    api.setInputs(claim.run_id, [input("revise_plan", "one more pass")]);
    await r.execute(claim);

    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failed, "run should fail on the shared-budget timeout");
    assert.match(failed!.body.failure_reason ?? "", /plan approval timed out/);
    assert.strictEqual(calls.length, 0, "no MR when the gate times out");
  });
});

type PlanVerdictKind = PlanVerdict["kind"];

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

  // PRD #108 M6. The Go module cache writes its package directories mode 0555, and
  // `fs.rm(force: true)` suppresses ENOENT, not EACCES — so before this fix the
  // runner's terminal cleanup rejected on the first unlink inside such a directory
  // and stranded the module cache (measured: 167.3 MB for one run,
  // "EACCES: permission denied, unlink '<home>/go/pkg/mod/…/benchmark_test.go'").
  //
  // RED against the unfixed runner: with `fs.rm` restored at runner.ts's cleanup
  // this test fails on `HOME … must be removed` while the run itself still
  // completes — which is the other half of the contract, below.
  it("removes a run HOME containing read-only (0555) directories, and cleanup failure never fails the run", async (t) => {
    // Root ignores the permission bits, so the fixture would not be hostile and
    // the assertion would hold against the UNFIXED runner too. Say so; do not
    // pass quietly.
    if (process.getuid?.() === 0) {
      t.skip("running as uid 0 — root bypasses the 0555 fixture, so it proves nothing here");
      return;
    }
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-runhome-ro-"));
    let runHome = "";
    let modDir = "";
    // The mode as the FILESYSTEM reported it, read back after the chmod and
    // immediately before the run returns — so the fixture is proven hostile at
    // the moment the runner's cleanup sees it, not merely requested to be.
    let observedMode = "";
    const factory: ExecutorFactory = (runId) => {
      runHome = path.join(homeRoot, runId);
      modDir = path.join(runHome, "go", "pkg", "mod", "gopkg.in", "inf.v0@v0.9.1");
      const executor: Executor = {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          // Mirror what a `go build` inside the run leaves behind.
          fs.mkdirSync(modDir, { recursive: true });
          fs.writeFileSync(path.join(modDir, "benchmark_test.go"), "package inf\n");
          fs.chmodSync(modDir, 0o555);
          observedMode = (fs.lstatSync(modDir).mode & 0o777).toString(8);
          return { branch: ctx.branch };
        },
      };
      return { executor, homeDir: runHome };
    };
    const claim = gitlabClaim(57);
    try {
      await runnerWith(factory, gitlab).execute(claim);
      assert.strictEqual(observedMode, "555", "fixture directory was not actually read-only when the run ended");
      assert.strictEqual(fs.existsSync(runHome), false, `HOME ${runHome} must be removed even with a 0555 dir inside`);
      // Cleanup is best-effort and lives in a `finally`; it must never turn a
      // completed run into a failed one.
      const state = api.states.filter((s) => s.runId === claim.run_id).at(-1);
      assert.strictEqual(state?.body.status, "completed");
    } finally {
      // Only reachable when the fix regressed and the tree survived.
      if (fs.existsSync(modDir)) fs.chmodSync(modDir, 0o755);
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

// ── Resume preflight (issue #105) ────────────────────────────────────────────
//
// The run lane pins a per-run HOME (`agent-home/<runId>`) that lives on the CLAIMING
// worker's data volume. A requeued run whose affinity grace lapsed can be claimed by a
// DIFFERENT worker, where that HOME never existed — the claim still carries the session
// id, but the transcript it names does not. Because the SDK resolves a resume LOCALLY
// (verified against the shipped CLI: exit 1, "No conversation found with session ID",
// duration_api_ms 0), passing that id through did not start a fresh run — it killed the
// run on its first turn with `error_during_execution`.
describe("RunRunner — resume preflight (issue #105)", () => {
  const SID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

  /** A factory with a per-run HOME under `homeRoot`, capturing the RunContext. */
  function capturingFactory(homeRoot: string, seen: RunContext[]): ExecutorFactory {
    return (runId) => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          seen.push(ctx);
          return { branch: ctx.branch };
        },
      },
      homeDir: path.join(homeRoot, runId),
    });
  }

  /** Plant a transcript under this run's HOME. The preflight globs the HOME's project
   *  dirs (sdk-session.ts), so the exact dir name does not matter — a per-run HOME holds
   *  only this run's own, so any project dir stands in for it. */
  function plantTranscript(runHome: string, sessionId: string): void {
    const dir = path.join(runHome, ".claude", "projects", "-data-runner-repo-issue-x");
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, `${sessionId}.jsonl`), "{}\n");
  }

  it("drops the resume and says so when the transcript is not on this worker", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-resume-"));
    const seen: RunContext[] = [];
    const claim = gitlabClaim(70, { session_id: SID });
    try {
      await runnerWith(capturingFactory(homeRoot, seen), gitlab).execute(claim);
      assert.strictEqual(seen[0]?.sessionId, undefined, "the dead session id must not reach the executor");
      const texts = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text));
      const notice = texts.find((t) => /earlier session could not be found/.test(t));
      assert.ok(notice, `expected an honest resume notice, got ${JSON.stringify(texts)}`);
      // Both facts the user needs to make sense of the feed: context is gone, AND that
      // is why the agent may re-tread ground. A bare "session not found" is not enough.
      assert.match(notice, /WITHOUT its earlier context/);
      assert.match(notice, /work may be repeated/);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("keeps the resume, silently, when the transcript IS on this worker", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-resume-"));
    const seen: RunContext[] = [];
    const claim = gitlabClaim(71, { session_id: SID });
    try {
      plantTranscript(path.join(homeRoot, claim.run_id), SID);
      await runnerWith(capturingFactory(homeRoot, seen), gitlab).execute(claim);
      assert.strictEqual(seen[0]?.sessionId, SID, "a resolvable session must still resume");
      const texts = api.messages(claim.run_id).filter((m) => m.kind === "status").map((m) => String(m.payload.text));
      assert.ok(!texts.some((t) => /earlier session could not be found/.test(t)), "must not cry wolf");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("does not preflight a run with no per-run HOME (the stub lane is untouched)", async () => {
    // The stub executor persists no SDK session, so main.ts gives it no per-run HOME.
    // That absence is the discriminator — no executor-kind check lives in the runner.
    const { gitlab } = fakeGitlab();
    const seen: RunContext[] = [];
    const claim = gitlabClaim(72, { session_id: SID });
    await runnerWith(
      () => ({ executor: { run: async (ctx: RunContext) => { seen.push(ctx); return { branch: ctx.branch }; } } }),
      gitlab,
    ).execute(claim);
    assert.strictEqual(seen[0]?.sessionId, SID, "no HOME to check ⇒ the claim's id passes through unchanged");
  });

  it("warns the amnesiac lead about prior commits on the branch — but ONLY when the resume was dropped", async () => {
    // The honest degradation must not become silently redone work: if the branch already
    // carries pushed work and the lead can no longer remember it, say so in the prompt.
    const { gitlab } = fakeGitlab();
    const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
    const gitFx = (args: string[]): void => {
      execFileSync("git", ["-C", fx.originPath, ...args], { env, stdio: "pipe" });
    };
    // A previous COMPLETED run's pushed work: two commits on the issue branch. (An
    // attempt requeued mid-flight leaves nothing — a run pushes once, at the end.)
    gitFx(["checkout", "-b", "agent/issue-73"]);
    fs.writeFileSync(path.join(fx.originPath, "a.txt"), "a\n");
    gitFx(["add", "."]);
    gitFx(["commit", "-m", "prior work 1"]);
    fs.writeFileSync(path.join(fx.originPath, "b.txt"), "b\n");
    gitFx(["add", "."]);
    gitFx(["commit", "-m", "prior work 2"]);
    gitFx(["checkout", "main"]);

    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-resume-"));
    try {
      const dropped: RunContext[] = [];
      const droppedClaim = gitlabClaim(73, { session_id: SID });
      await runnerWith(capturingFactory(homeRoot, dropped), gitlab).execute(droppedClaim);
      assert.deepStrictEqual(dropped[0]?.priorWork, { commits: 2 }, "counted against the default branch");

      // Same branch, same prior commits — but a resolvable session, so the lead
      // remembers its own work and needs no warning.
      const kept: RunContext[] = [];
      const keptClaim = gitlabClaim(73, { session_id: SID });
      plantTranscript(path.join(homeRoot, keptClaim.run_id), SID);
      await runnerWith(capturingFactory(homeRoot, kept), gitlab).execute(keptClaim);
      assert.strictEqual(kept[0]?.priorWork, undefined, "a live resume needs no prior-work warning");

      // And a fresh run (no session at all) on a branch with no prior work gets none.
      const fresh: RunContext[] = [];
      await runnerWith(capturingFactory(homeRoot, fresh), gitlab).execute(gitlabClaim(74));
      assert.strictEqual(fresh[0]?.priorWork, undefined);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
