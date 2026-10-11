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
import type { CodexAdviceHarnessFactory } from "../src/codex/codex-executor.js";
import type { AdviceRequest, AdviceResult, AdviceResultPolicy } from "../src/harness.js";

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

/** summaryQuery that also drains the streamed prompt into `texts` (the model pass sends an async iterable). */
function promptCapturingQuery(texts: string[]): SdkQueryFn {
  const inner = summaryQuery([]);
  return ((params: { options: unknown; prompt: unknown }) => {
    return (async function* () {
      let text = "";
      if (typeof params.prompt === "string") text = params.prompt;
      else for await (const m of params.prompt as AsyncIterable<unknown>) text += JSON.stringify(m);
      texts.push(text);
      yield* (inner as unknown as (p: unknown) => AsyncIterable<unknown>)(params);
    })();
  }) as unknown as SdkQueryFn;
}

function runnerFor(executor: Executor, nowSummary?: { homeRoot: string; queryFn: SdkQueryFn; codexAdviceHarnessFactory?: CodexAdviceHarnessFactory }): RunRunner {
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

  it("a poll that turns the setting off clears the shown note at once", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2603);
    api.nowSummary.set(claim.run_id, true);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    const executor: Executor = {
      async run(ctx: RunContext) {
        const frozen = [{ id: "m1", title: "Write the docs" }] as never;
        await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, frozen);
        const emitFrame = () => ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "Run the api gate" } } });
        const until = Date.now() + 4000;
        while (!notesOf(claim.run_id).some((m) => (m.payload as { text?: string }).text) && Date.now() < until) {
          emitFrame();
          await new Promise((r) => setTimeout(r, 25));
        }
        api.nowSummary.set(claim.run_id, false);
        // Quiet: no frames, so only the poll can clear the note.
        const quietUntil = Date.now() + 3000;
        while (!notesOf(claim.run_id).some((m) => (m.payload as { text?: string }).text === "" && !(m.payload as { model_usage?: unknown }).model_usage) && Date.now() < quietUntil) {
          await new Promise((r) => setTimeout(r, 25));
        }
        commitMarker(ctx.worktreePath);
        return { branch: ctx.branch };
      },
    };
    try {
      await runnerFor(executor, { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    const notes = notesOf(claim.run_id).map((m) => m.payload as Record<string, unknown>);
    assert.equal(notes[0]!.text, "Running the api gate for the docs milestone");
    const clears = notes.filter((p) => p.text === "" && p.model_usage === undefined);
    assert.equal(clears.length, 1);
    assert.equal(clears[0]!.milestone_id, "m1");
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

  const CODEX_SECRETS = {
    forge_pat: "fixture-forge-pat-000000",
    // Carries an Anthropic token too, so ONLY the Codex term of the claim gate decides.
    anthropic_oauth_token: "oauth-fixture-token",
    codex: { auth_mode: "subscription", access_token: "claim-key", capability: "cap", generation: 3, chatgpt_account_id: "verified-account", chatgpt_plan_type: null },
  };

  /** A fake production factory: records the build params and each request, answers like luna. */
  function lunaFactory(rec: { builds: unknown[]; requests: AdviceRequest[] }, text = "Running the api gate for the docs milestone"): CodexAdviceHarnessFactory {
    return (async (params: unknown) => {
      rec.builds.push(params);
      return {
        kind: "codex",
        async run(request: AdviceRequest, _policy: AdviceResultPolicy): Promise<AdviceResult> {
          rec.requests.push(request);
          request.usageObserver?.({ "gpt-6-luna": { inputTokens: 90, outputTokens: 12, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costStatus: "metered", costUSD: 0.000015 } });
          return { text, end: { kind: "terminal", terminal: { outcome: "success" } } } as unknown as AdviceResult;
        },
      };
    }) as unknown as CodexAdviceHarnessFactory;
  }

  it("a Codex-harness claim with a binding posts a luna note with its usage through the injected factory", async () => {
    const claim = claimFor(2606, { secrets: CODEX_SECRETS });
    api.nowSummary.set(claim.run_id, true);
    const rec = { builds: [] as unknown[], requests: [] as AdviceRequest[] };
    const sdkCalls: { options: unknown; prompt: unknown }[] = [];
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => notesOf(claim.run_id).length > 0), { homeRoot, queryFn: summaryQuery(sdkCalls), codexAdviceHarnessFactory: lunaFactory(rec) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.equal(sdkCalls.length, 0, "a Codex run never reaches the Claude SDK");
    const notes = notesOf(claim.run_id);
    assert.equal(notes.length, 1);
    const p = notes[0]!.payload as Record<string, unknown>;
    assert.equal(p.text, "Running the api gate for the docs milestone");
    assert.deepEqual((p.model_usage as Record<string, unknown>)["gpt-6-luna"], { inputTokens: 90, outputTokens: 12, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costStatus: "metered", costUSD: 0.000015 });
    assert.equal(rec.requests.length, 1);
    assert.equal(rec.requests[0]!.model, "gpt-6-luna");
    assert.equal(rec.requests[0]!.effort, "low");
    assert.equal(rec.requests[0]!.timeoutMs, 30_000);
    assert.equal(rec.requests[0]!.label, "now");
    assert.match(rec.requests[0]!.prompt, /Run the api gate/);
    const built = rec.builds[0] as { runId: string; refresh?: string; binding: { authMode: string; capability: string } };
    assert.equal(built.runId, claim.run_id);
    assert.equal(built.refresh, "deny");
    assert.equal(built.binding.authMode, "subscription");
  });

  it("a Codex account that refuses the model leaves the run unaffected and shows no summary", async () => {
    const claim = claimFor(2608, { secrets: CODEX_SECRETS });
    api.nowSummary.set(claim.run_id, true);
    const statuses: string[] = [];
    api.onState(claim.run_id, (body) => void statuses.push(body.status));
    let builds = 0;
    const refusing = (async () => {
      builds++;
      throw new Error("model gpt-6-luna is not available to this account");
    }) as unknown as CodexAdviceHarnessFactory;
    let frames = 0;
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(workingExecutor(() => ++frames > 40, 3000), { homeRoot, queryFn: summaryQuery([]), codexAdviceHarnessFactory: refusing }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.equal(builds, 1, "tried once; the failed call holds the 5 minute window");
    assert.equal(notesOf(claim.run_id).length, 0);
    assert.ok(statuses.includes("completed") && !statuses.includes("failed"), `the run is unaffected: ${statuses.join(",")}`);
  });

  it("makes no call for a Codex-harness claim when no advice factory is wired", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2609, { secrets: CODEX_SECRETS });
    api.nowSummary.set(claim.run_id, true);
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

  it("makes no call while the executor is parked on askUser, and calls once the answer ends the hold", async () => {
    const calls: { options: unknown; prompt: unknown }[] = [];
    const claim = claimFor(2607);
    api.nowSummary.set(claim.run_id, true);
    let questionId = "";
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id) questionId = body.open_question_id;
    });
    let callsDuringHold = -1;
    let heldFrames = 0;
    const executor: Executor = {
      async run(ctx: RunContext) {
        await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, [{ id: "m1", title: "Write the docs" }] as never);
        const asked = ctx.askUser!([{ question: "which docs?", header: "Docs" }]);
        const parkBy = Date.now() + 3000;
        while (questionId === "" && Date.now() < parkBy) await new Promise((r) => setTimeout(r, 10));
        // Frames arrive while the run is parked; a controller that is not held would call at once.
        const holdUntil = Date.now() + 300;
        while (Date.now() < holdUntil) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "Run the api gate" } } });
          heldFrames++;
          await new Promise((r) => setTimeout(r, 25));
        }
        callsDuringHold = calls.length;
        api.setInputs(claim.run_id, [{ id: 1, kind: "answer", body: JSON.stringify({ question_id: questionId, answers: ["the api docs"] }) }]);
        await asked;
        const until = Date.now() + 4000;
        while (notesOf(claim.run_id).length === 0 && Date.now() < until) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "Run the api gate" } } });
          await new Promise((r) => setTimeout(r, 25));
        }
        commitMarker(ctx.worktreePath);
        return { branch: ctx.branch };
      },
    };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(executor, { homeRoot, queryFn: summaryQuery(calls) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.ok(questionId !== "", "the run really parked on the question");
    assert.ok(heldFrames > 3, "frames arrived during the hold");
    assert.equal(callsDuringHold, 0, "no model call while the run is parked on askUser");
    assert.ok(calls.length >= 1, "the call is made once the hold ends (positive control)");
  });

  it("redacts the run's claim secrets in the model prompt: a secret straddling the 200 rune cut leaves no prefix", async () => {
    const texts: string[] = [];
    // Assembled at runtime so no secret-shaped literal sits in source.
    const secret = ["fixture", "forge", "pat", "Zq9Xw8Vu7Ts6"].join("-");
    const claim = claimFor(2608, { secrets: { forge_pat: secret, anthropic_oauth_token: "oauth-fixture-token" } });
    api.nowSummary.set(claim.run_id, true);
    const executor: Executor = {
      async run(ctx: RunContext) {
        await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, [{ id: "m1", title: "Write the docs" }] as never);
        const until = Date.now() + 4000;
        while (notesOf(claim.run_id).length === 0 && Date.now() < until) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "y".repeat(192) + secret } } });
          await new Promise((r) => setTimeout(r, 25));
        }
        commitMarker(ctx.worktreePath);
        return { branch: ctx.branch };
      },
    };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(executor, { homeRoot, queryFn: promptCapturingQuery(texts) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.ok(texts.length >= 1, "a call was made");
    const prompt = texts[0]!;
    assert.ok(prompt.includes("yyyyyyyy"), "the frame reached the prompt (positive control)");
    assert.ok(!prompt.includes(secret.slice(0, 6)), "no prefix of the claim secret reaches the model");
  });

  it("the turn-boundary claim milestone list is only a fallback: it never replaces the list reportProgress gave", async () => {
    const texts: string[] = [];
    const claim = claimFor(2609, { milestones: [{ id: "m1", title: "CLAIM_LIST_TITLE" }] });
    api.nowSummary.set(claim.run_id, true);
    const executor: Executor = {
      async run(ctx: RunContext) {
        await ctx.reportProgress!({ completed: [], in_progress: ["m1"] }, [{ id: "m1", title: "LIVE_LIST_TITLE" }] as never);
        await ctx.reportIteration!(1, { completed: [], in_progress: ["m1"] });
        const until = Date.now() + 4000;
        while (notesOf(claim.run_id).length === 0 && Date.now() < until) {
          ctx.emit({ kind: "tool_use", agent: "lead", payload: { id: "t", name: "Bash", input: { command: "x", description: "Run the api gate" } } });
          await new Promise((r) => setTimeout(r, 25));
        }
        commitMarker(ctx.worktreePath);
        return { branch: ctx.branch };
      },
    };
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "now-wiring-"));
    try {
      await runnerFor(executor, { homeRoot, queryFn: promptCapturingQuery(texts) }).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    assert.ok(texts.length >= 1);
    const prompt = texts[0]!;
    assert.ok(prompt.includes("LIVE_LIST_TITLE"));
    assert.ok(!prompt.includes("CLAIM_LIST_TITLE"));
  });
});
