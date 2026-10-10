// PRD #2603 M1: the RunRunner wiring of the Now summary controller, end to end through a real
// claim against FakeApi: the setting rides the /inputs poll, the run's own emit/reportProgress
// feed the controller, the note lands on the run's message stream, and a run without the setting,
// the collaborators or the Claude harness makes no model call at all.

import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import type { Executor, RunContext } from "../src/executor.js";
import { GitLabClient, type FetchFn } from "../src/forge.js";
import { RunRunner } from "../src/runner.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";

const TOKEN = "tkn-nowsummary-123456";
const HAIKU = "claude-haiku-4-5-20251001";

let api: FakeApi;
let fx: Fixture;
let git: GitCache;
let client: WorkerClient;

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  const baseUrl = await api.listen();
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
  client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
});

afterEach(async () => {
  try {
    await api.close();
  } finally {
    fx.cleanup();
  }
});

function fakeGitlab(): GitLabClient {
  const fetchFn: FetchFn = async () => ({
    status: 201,
    text: async () => JSON.stringify({ iid: 42, web_url: "https://gitlab.example.test/org/repo/-/merge_requests/42" }),
  });
  return new GitLabClient({ fetchFn });
}

function summaryQuery(calls: { options: unknown; prompt: unknown }[]): SdkQueryFn {
  return ((params: { options: unknown; prompt: unknown }) => {
    calls.push(params);
    return (async function* () {
      yield { type: "assistant", message: { id: "msg1", model: HAIKU, role: "assistant", content: [{ type: "text", text: "Running the api gate for the docs milestone" }], usage: { input_tokens: 21, output_tokens: 8 } } };
      yield { type: "result", subtype: "success", is_error: false, modelUsage: { [HAIKU]: { inputTokens: 21, outputTokens: 8, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.0003 } } };
    })();
  }) as unknown as SdkQueryFn;
}

function runnerFor(executor: Executor, nowSummary?: { homeRoot: string; queryFn: SdkQueryFn }): RunRunner {
  return new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    gitlab: fakeGitlab(),
    ...(nowSummary ? { nowSummary } : {}),
  });
}

const claimFor = (iid: number, overrides = {}) =>
  makeClaim({
    issue_iid: iid,
    issue_title: `Fix thing ${iid}`,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "oauth-fixture-token" },
    ...overrides,
  });

function commitMarker(worktreePath: string): void {
  fs.writeFileSync(path.join(worktreePath, "UZI_RUN.md"), "# now summary\n");
  execFileSync("git", ["add", "UZI_RUN.md"], { cwd: worktreePath });
  execFileSync("git", ["-c", "user.name=uzi-agent", "-c", "user.email=uzi-agent@uzi.local", "-c", "commit.gpgsign=false", "commit", "-m", "uzi test"], { cwd: worktreePath });
}

const notesOf = (runId: string) => api.messages(runId).filter((m) => m.kind === "progress_note");

/** An executor that works milestone m1: it emits tool frames and reports progress until `done()`
 *  or `maxMs`, the way a live lead does, then commits. */
function workingExecutor(done: () => boolean, maxMs = 4000): Executor {
  return {
    async run(ctx: RunContext) {
      const frozen = [{ id: "m1", title: "Write the docs" }] as never;
      await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, frozen);
      const until = Date.now() + maxMs;
      while (!done() && Date.now() < until) {
        ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "task gate:api", description: "Run the api gate" } } });
        await new Promise((r) => setTimeout(r, 25));
      }
      commitMarker(ctx.worktreePath);
      return { branch: ctx.branch };
    },
  };
}

describe("RunRunner Now summary wiring (PRD #2603)", () => {
  it("posts one progress_note with the usage on the run's stream when the poll says the setting is on", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2603);
    api.nowSummary.set(claim.run_id, true);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => notesOf(claim.run_id).length > 0), { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    const notes = notesOf(claim.run_id);
    assert.equal(notes.length, 1, "one call per 5 minute window");
    const p = notes[0]!.payload as Record<string, unknown>;
    assert.equal(p.text, "Running the api gate for the docs milestone");
    assert.equal(p.milestone_id, "m1");
    assert.deepEqual(Object.keys(p).sort(), ["milestone_id", "model_usage", "text"]);
    assert.deepEqual((p.model_usage as Record<string, unknown>)[HAIKU], { inputTokens: 21, outputTokens: 8, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.0003 });
    assert.equal(notes[0]!.agent, "worker");
    assert.equal(calls.length, 1);
    const opts = calls[0]!.options as { model?: string; env?: Record<string, string>; thinking?: unknown };
    assert.equal(opts.model, "haiku");
    assert.equal(opts.env?.CLAUDE_CODE_MAX_OUTPUT_TOKENS, "256");
    assert.deepEqual(opts.thinking, { type: "disabled" });
    assert.equal(opts.env?.CLAUDE_CODE_OAUTH_TOKEN, "oauth-fixture-token", "the run's own credential");
  });

  it("makes no call when the poll never says on (an absent now_summary is off)", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2604);
    let frames = 0;
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => ++frames > 40, 3000), { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.equal(calls.length, 0);
    assert.equal(notesOf(claim.run_id).length, 0);
  });

  it("makes no call when the setting is on but the runner has no collaborators (stub executor, tests)", async () => {
    const claim = claimFor(2605);
    api.nowSummary.set(claim.run_id, true);
    let frames = 0;
    await runnerFor(workingExecutor(() => ++frames > 40, 3000)).execute(claim);
    assert.equal(notesOf(claim.run_id).length, 0);
  });

  it("makes no call for a Codex-harness claim (M1 is Claude only)", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2606, {
      secrets: { forge_pat: "fixture-forge-pat-000000", codex: { auth_mode: "subscription", access_token: "claim-key", capability: "cap", generation: 3, chatgpt_account_id: "verified-account", chatgpt_plan_type: null } },
    });
    api.nowSummary.set(claim.run_id, true);
    let frames = 0;
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => ++frames > 40, 3000), { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.equal(calls.length, 0);
  });
});
