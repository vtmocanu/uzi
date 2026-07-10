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
import { StubExecutor, PlanRejectedError, STUB_FAIL_SENTINEL, type Executor } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { GitLabClient, type FetchFn } from "../src/gitlab.js";
import { RunRunner } from "../src/runner.js";
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
  return new RunRunner(client, git, executor, nullLogger(), 20, joinToken, {
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
      const repoRunner = new RunRunner(client, new GitCache(repoFx.dataDir, nullLogger()), new StubExecutor(nullLogger()), nullLogger(), 20, undefined, {
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
        new StubExecutor(nullLogger()),
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
});
