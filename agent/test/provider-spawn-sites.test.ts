import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { ChatExecutor, type ChatContext, type ChatScheduler, type ChatTimerHandle, type ChatUserMessage } from "../src/chat-executor.js";
import { ClaudeAdviceHarness } from "../src/claude-advice-harness.js";
import { runReadOnlyModelPass } from "../src/model-pass.js";
import { ClaudeCrossCheck } from "../src/claude-cross-check.js";
import { preflightReason } from "../src/isolated-runner.js";
import { JudgeRunner } from "../src/judge-runner.js";
import { JobRunner } from "../src/job-runner.js";
import { LimitReachedError } from "../src/limit.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import type { AgentTemplate, ClaimClaudeProviderBinding, ClaimResponse } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import type { AdviceUsageSnapshot } from "../src/harness.js";
import { createLogger } from "../src/log.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";
import { testOwnedPassHomeRoot } from "./teardown-fixtures.js";
import { secretRecordingLogger } from "./runner-harness.js";
import { withAppliedSettings } from "./phase-settings-fake.js";

// PRD #4320 module 6/7 (agent half): every converted spawn builder's env and model in provider mode,
// the gateway-limit and switch refusals, and the second-layer lane refusals. The gateway key is
// assembled at runtime: no provider-token-shaped literal lives in source.
const KEY = ["gw", "key", "do-not-scan", "6666", "abcdefgh"].join("-");
const BASE_URL = "https://gw.example.test/anthropic";
const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const nonexistentWorktree = nonexistentWorktreeFactory("uzi-provider-sites");

function binding(over: Partial<ClaimClaudeProviderBinding> = {}): ClaimClaudeProviderBinding {
  return {
    provider_id: "p-1", revision: 2, engine: "claude", protocol: "anthropic_messages", base_url: BASE_URL, auth_style: "bearer",
    credential: KEY,
    model_map: { opus: { model: "corp-opus" }, sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku", context_window: 64_000 } },
    ...over,
  };
}

/** What every provider-mode spawn must hand the CLI. */
function assertProviderEnv(env: Record<string, string | undefined> | undefined, label: string): void {
  assert.ok(env, `${label}: env`);
  assert.equal(env.ANTHROPIC_BASE_URL, BASE_URL, `${label}: base url`);
  assert.equal(env.ANTHROPIC_AUTH_TOKEN, KEY, `${label}: bearer key`);
  assert.equal(env.ANTHROPIC_API_KEY, undefined, `${label}: no x-api-key`);
  assert.equal(env.CLAUDE_CODE_OAUTH_TOKEN, undefined, `${label}: no OAuth token`);
  assert.equal(env.ANTHROPIC_DEFAULT_SONNET_MODEL, "corp-sonnet", `${label}: sonnet map`);
  assert.equal(env.ANTHROPIC_SMALL_FAST_MODEL, "corp-haiku", `${label}: small/fast slot`);
  assert.equal(env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC, "1", `${label}: nonessential traffic off`);
  assert.equal(env.CLAUDE_CODE_MAX_CONTEXT_TOKENS, "64000", `${label}: smallest context window`);
  assert.ok(!Object.values(env).includes(OAUTH), `${label}: the OAuth token is nowhere`);
}

function sessionResult(over: Record<string, unknown> = {}, sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId, ...over } as unknown as SDKMessage;
}
const gatewayError = (status: number): SDKMessage => sessionResult({
  is_error: true, terminal_reason: "api_error", api_error_status: status, result: `API Error: ${status} gateway says no`,
});
function submitPlan(plan: string): SDKMessage {
  return { type: "assistant", session_id: "sess-1", message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] } } as unknown as SDKMessage;
}
function signalDone(): SDKMessage {
  return { type: "assistant", session_id: "sess-1", message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }] } } as unknown as SDKMessage;
}

interface Turn { options: SdkOptions }
type Script = SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>);
function fakeTurns(scripts: Script[]): { queryFn: SdkQueryFn; turns: Turn[] } {
  const turns: Turn[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    turns.push({ options: params.options });
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      const s = typeof script === "function" ? script(params.options.abortController!.signal) : script;
      if (Array.isArray(s)) for (const m of s) yield m;
      else yield* s as AsyncIterable<SDKMessage>;
    })();
  };
  return { queryFn, turns };
}

let homeDir: string;
beforeEach(() => { homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-provider-sites-")); });
afterEach(() => { fs.rmSync(homeDir, { recursive: true, force: true }); });

const lead: AgentTemplate = { name: "lead", description: "leads", prompt_body: "LEAD" };
const coder: AgentTemplate = { name: "coder", description: "writes", prompt_body: "CODE", tools: ["Read", "Edit"] };

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; emits: EmittedMessage[] } {
  const emits: EmittedMessage[] = [];
  const verdict: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  const ctx: RunContext = {
    runId: "r-prov", issueIid: 5, issueTitle: "Fix", issueDescription: "do it", worktreePath: nonexistentWorktree(), branch: "agent/issue-5",
    emit: (m) => emits.push(m), oauthToken: "", provider: binding(), agents: [lead, coder], config: null, sessionId: null,
    gatePlan: async () => verdict, pullFollowUps: () => [], checkpoint: async () => {}, reportIteration: async () => undefined,
    ...overrides,
  };
  return { ctx, emits };
}

describe("SdkExecutor in provider mode (issue run lead and subagents)", () => {
  it("the lead's env is the provider env with no OAuth token, and an unset model is the explicit sonnet alias", async () => {
    const { queryFn, turns } = fakeTurns([[submitPlan("plan"), sessionResult()], [signalDone(), sessionResult()]]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx().ctx);
    assertProviderEnv(turns[0]!.options.env, "plan turn");
    assertProviderEnv(turns[1]!.options.env, "implement turn");
    assert.equal(turns[0]!.options.model, "sonnet");
    assert.equal(turns[1]!.options.model, "sonnet");
  });

  it("PRD #3252: a planning pair resolves through the map per phase, the plan turn on the planning model and the implement turn on the implement model", async () => {
    const fake = fakeTurns([[submitPlan("plan"), sessionResult()], [signalDone(), sessionResult()]]);
    const { turns } = fake;
    // The gateway map resolves the alias the CLI is given; the runtime reports the mapped ids.
    const queryFn = withAppliedSettings(fake.queryFn, (o) => ({ model: o.model === "opus" ? "corp-opus" : o.model, effort: "medium" }));
    const { ctx } = makeCtx({
      config: {
        default_model: "corp-sonnet", default_model_source: "user_default",
        default_effort: "medium", default_effort_source: "user_default",
        plan_phase: { model: "opus", model_source: "user_planning_default", effort: "medium", effort_source: "user_planning_default" },
      },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assertProviderEnv(turns[0]!.options.env, "plan turn");
    assertProviderEnv(turns[1]!.options.env, "implement turn");
    assert.equal(turns[0]!.options.model, "opus", "plan turn: the planning alias, mapped by the provider");
    assert.equal(turns[1]!.options.model, "corp-sonnet", "implement turn: the implement model, not the planning one");
  });

  it("a vendor run is unchanged: OAuth token, no base URL, no explicit model", async () => {
    const { queryFn, turns } = fakeTurns([[submitPlan("plan"), sessionResult()], [signalDone(), sessionResult()]]);
    const { ctx } = makeCtx({ oauthToken: OAUTH });
    delete ctx.provider;
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    const env = turns[0]!.options.env!;
    assert.equal(env.CLAUDE_CODE_OAUTH_TOKEN, OAUTH);
    assert.ok(!("ANTHROPIC_BASE_URL" in env));
    assert.ok(!("model" in turns[0]!.options));
  });

  it("a mapped alias or a mapped target model is spawned as given", async () => {
    for (const model of ["opus", "corp-sonnet"]) {
      const { queryFn, turns } = fakeTurns([[submitPlan("plan"), sessionResult()], [signalDone(), sessionResult()]]);
      await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ config: { default_model: model } }).ctx);
      assert.equal(turns[0]!.options.model, model);
    }
  });

  it("an unmapped lead model fails the run with provider_model_unmapped before any content is sent", async () => {
    for (const model of ["claude-opus-4-1", "fable", "opus[1m]"]) {
      const { queryFn, turns } = fakeTurns([[submitPlan("plan"), sessionResult()]]);
      await assert.rejects(
        new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ config: { default_model: model } }).ctx),
        (e: Error) => e.message.startsWith("provider_model_unmapped") && e.message.includes(model),
      );
      assert.equal(turns.length, 0, `${model}: no turn was spawned`);
    }
  });

  it("an unmapped template subagent model fails the run before any turn", async () => {
    const { queryFn, turns } = fakeTurns([[submitPlan("plan"), sessionResult()]]);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ agents: [lead, { ...coder, model: "claude-sonnet-4-5" }] }).ctx),
      /provider_model_unmapped.*claude-sonnet-4-5/s,
    );
    assert.equal(turns.length, 0);
  });

  it("a repo-sourced agent with an unmapped model fails the run before any turn; a mapped one runs", async () => {
    const repoAgent = (model: string): AgentTemplate => ({ name: "auditor", description: "audits", prompt_body: "AUDIT", model });
    const { queryFn, turns } = fakeTurns([[submitPlan("plan"), sessionResult()]]);
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ repoAgents: [repoAgent("claude-opus-4-1")], requestedAgentSource: "repo" }).ctx),
      /provider_model_unmapped.*claude-opus-4-1/s,
    );
    assert.equal(turns.length, 0);
    const ok = fakeTurns([[submitPlan("plan"), sessionResult()], [signalDone(), sessionResult()]]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn: ok.queryFn }).run(makeCtx({ repoAgents: [repoAgent("haiku")], requestedAgentSource: "repo" }).ctx);
    assert.equal(ok.turns[0]!.options.agents?.auditor?.model, "haiku");
  });

  for (const status of [429, 529]) {
    it(`a terminal gateway ${status} parks as a limit with provider_gateway_limit and NO reset time`, async () => {
      const { queryFn } = fakeTurns([[gatewayError(status)]]);
      await assert.rejects(
        new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx().ctx),
        (e: unknown) => e instanceof LimitReachedError && e.rateLimitType === "provider_gateway_limit" && e.resetsAtMs === undefined,
      );
    });
  }

  it("the result frame's vendor-priced cost is not reported on a provider run; tokens stay", async () => {
    const result = sessionResult({
      total_cost_usd: 1.23, usage: { input_tokens: 10, output_tokens: 5 },
      modelUsage: { "corp-sonnet": { inputTokens: 10, outputTokens: 5, costUSD: 1.23 } },
    });
    const { queryFn } = fakeTurns([[submitPlan("plan"), result], [signalDone(), result]]);
    const provider = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(provider.ctx);
    const frames = provider.emits.filter((m) => (m.payload as { event?: string }).event === "result");
    assert.ok(frames.length > 0, "result frames were emitted");
    for (const f of frames) {
      assert.ok(!("total_cost_usd" in f.payload), "no total_cost_usd");
      assert.deepStrictEqual(f.payload.modelUsage, { "corp-sonnet": { inputTokens: 10, outputTokens: 5 } });
    }
    const vendor = makeCtx({ oauthToken: OAUTH });
    delete vendor.ctx.provider;
    const again = fakeTurns([[submitPlan("plan"), result], [signalDone(), result]]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn: again.queryFn }).run(vendor.ctx);
    const vframes = vendor.emits.filter((m) => (m.payload as { event?: string }).event === "result");
    assert.ok(vframes.some((f) => f.payload.total_cost_usd === 1.23), "a vendor run keeps its cost");
  });

  it("an in-run credential switch is refused on a provider run: the run fails, nothing is applied, no OAuth token is injected", async () => {
    const hang = (signal: AbortSignal): AsyncIterable<unknown> => ({
      // eslint-disable-next-line require-yield
      async *[Symbol.asyncIterator]() {
        await new Promise<void>((resolve) => {
          if (signal.aborted) return resolve();
          const keepAlive = setInterval(() => {}, 1_000);
          signal.addEventListener("abort", () => { clearInterval(keepAlive); resolve(); }, { once: true });
        });
      },
    });
    const { queryFn, turns } = fakeTurns([[submitPlan("# Plan"), sessionResult()], hang]);
    const cancel = new AbortController();
    let attempts = 0;
    const { ctx } = makeCtx({
      signal: cancel.signal,
      reportIteration: async (n) => { if (n === 1) cancel.abort(new CredentialSwitchSignal()); return undefined; },
      attemptCredentialSwitch: async () => { attempts++; return "applied"; },
      credential: () => ({ token: OAUTH, ordinal: 1 }),
    });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /provider_credential_switch_refused/);
    assert.equal(attempts, 0, "the switch machinery never ran");
    assert.equal(ctx.oauthToken, "", "the OAuth token was never injected into the context");
    for (const t of turns) assert.equal(t.options.env?.CLAUDE_CODE_OAUTH_TOKEN, undefined);
  });
});

// ── chat ──────────────────────────────────────────────────────────────────────────────────────

class FakeScheduler implements ChatScheduler {
  set(): ChatTimerHandle { return { clear: () => {} }; }
}

function chatCtx(over: Partial<ChatContext> = {}, queue: (string | undefined)[] = ["hello", undefined]): { ctx: ChatContext; emits: EmittedMessage[] } {
  const emits: EmittedMessage[] = [];
  const pending = [...queue];
  const ctx: ChatContext = {
    runId: "chat-1", oauthToken: "", provider: binding(), sessionId: null, emit: (m) => emits.push(m), onSessionId: () => {},
    maxTurns: 50, turnTimeoutMs: 5000,
    nextUserMessage: () => Promise.resolve<ChatUserMessage | undefined>(pending.length === 0 ? undefined : (pending.shift() === undefined ? undefined : { text: "hello", inputId: 1 })),
    followUpIncluded: () => {},
    ...over,
  };
  return { ctx, emits };
}

describe("ChatExecutor in provider mode", () => {
  it("builds the provider env, resolves an unset model to sonnet, and never carries an OAuth token", async () => {
    const { queryFn, turns } = fakeTurns([[sessionResult()]]);
    await new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: new FakeScheduler() }).run(chatCtx().ctx);
    assertProviderEnv(turns[0]!.options.env, "chat");
    assert.equal(turns[0]!.options.model, "sonnet");
  });

  it("an unmapped chat model fails the chat with provider_model_unmapped and spawns nothing", async () => {
    const { queryFn, turns } = fakeTurns([[sessionResult()]]);
    await assert.rejects(
      new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: new FakeScheduler() }).run(chatCtx({ model: "claude-sonnet-4-5" }).ctx),
      /provider_model_unmapped.*claude-sonnet-4-5/s,
    );
    assert.equal(turns.length, 0);
  });

  for (const status of [429, 529]) {
    it(`a gateway ${status} ends the turn with a provider_gateway_limit reason, not a subscription reset, and never retries`, async () => {
      const { queryFn, turns } = fakeTurns([[gatewayError(status)]]);
      const { ctx, emits } = chatCtx({}, ["hello", "again", undefined]);
      const result = await new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: new FakeScheduler() }).run(ctx);
      const errors = emits.filter((m) => m.kind === "error").map((m) => String(m.payload.text));
      const limited = errors.filter((t) => t.includes("provider_gateway_limit"));
      assert.equal(limited.length, 2, "one ended turn per user message; the turn is never retried");
      for (const text of limited) assert.match(text, new RegExp(`HTTP ${status}`));
      for (const text of errors) assert.ok(!/Anthropic usage limit|resets at|five_hour/i.test(text), "no subscription-reset wording");
      assert.equal(turns.length, 2, "exactly one query per user message");
      assert.equal(result.turns, 2);
    });
  }

  it("a vendor chat that hits a usage limit keeps the subscription wording (unchanged)", async () => {
    const limit = sessionResult({ is_error: true, terminal_reason: "blocking_limit", result: "limit" });
    const { queryFn } = fakeTurns([[limit]]);
    const vendor = chatCtx({ oauthToken: OAUTH });
    delete vendor.ctx.provider;
    await new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: new FakeScheduler() }).run(vendor.ctx);
    const texts = vendor.emits.filter((m) => m.kind === "error").map((m) => String(m.payload.text));
    assert.ok(texts.some((t) => /Anthropic usage limit reached/.test(t)), texts.join(" | "));
    assert.ok(!texts.some((t) => t.includes("provider_gateway_limit")));
  });
});

// ── advice / summary / now passes ────────────────────────────────────────────────────────────

function capturingQuery(frames: unknown[] = []): { queryFn: SdkQueryFn; seen: SdkOptions[] } {
  const seen: SdkOptions[] = [];
  const queryFn = ((params: { options: SdkOptions; prompt: unknown }) => {
    seen.push(params.options);
    return (async function* () { yield* frames; })();
  }) as unknown as SdkQueryFn;
  return { queryFn, seen };
}
const textFrames = (): unknown[] => [
  { type: "assistant", message: { id: "m1", model: "corp-haiku", role: "assistant", content: [{ type: "text", text: "ok" }], usage: { input_tokens: 3, output_tokens: 2 } } },
  { type: "result", subtype: "success", is_error: false, total_cost_usd: 0.5, modelUsage: { "corp-haiku": { inputTokens: 3, outputTokens: 2, costUSD: 0.5 } } },
];

describe("advice harness and model passes in provider mode", () => {
  const PASS_HOME_ROOT = testOwnedPassHomeRoot();

  it("ClaudeAdviceHarness builds the provider env and resolves the request model through the map", async () => {
    const { queryFn, seen } = capturingQuery(textFrames());
    const abort = new AbortController();
    const harness = new ClaudeAdviceHarness({
      credential: { kind: "provider", baseUrl: BASE_URL, authStyle: "bearer", key: KEY, modelMap: binding().model_map },
      homeDir: path.join(homeDir, "advice"), abort, queryFn, denyReason: "read-only", log: nullLogger(),
    });
    await harness.run({ label: "summary", systemPrompt: "s", prompt: "p", model: "haiku", output: { kind: "text" }, signal: abort.signal, timeoutMs: 5000 },
      { onTerminal: () => {} });
    assertProviderEnv(seen[0]!.env, "advice");
    assert.equal(seen[0]!.model, "haiku");
  });

  it("an unmapped model rejects before any spawn (the caller treats it as a skipped pass)", async () => {
    const { queryFn, seen } = capturingQuery(textFrames());
    const abort = new AbortController();
    const harness = new ClaudeAdviceHarness({
      credential: { kind: "provider", baseUrl: BASE_URL, authStyle: "bearer", key: KEY, modelMap: binding().model_map },
      homeDir: path.join(homeDir, "advice"), abort, queryFn, denyReason: "read-only", log: nullLogger(),
    });
    await assert.rejects(
      harness.run({ label: "summary", systemPrompt: "s", prompt: "p", model: "claude-haiku-4-5", output: { kind: "text" }, signal: abort.signal, timeoutMs: 5000 }, { onTerminal: () => {} }),
      /provider_model_unmapped.*claude-haiku-4-5/s,
    );
    assert.equal(seen.length, 0);
  });

  it("runReadOnlyModelPass with a provider needs no token, resolves an empty model to sonnet, and reports tokens without cost", async () => {
    const { queryFn, seen } = capturingQuery(textFrames());
    let usage: AdviceUsageSnapshot | undefined;
    const text = await runReadOnlyModelPass({
      provider: binding(), systemPrompt: "s", prompt: "p", homeRoot: PASS_HOME_ROOT, homePrefix: "uzi-summary-", label: "summary",
      timeoutMs: 5000, queryFn, denyReason: "read-only", log: nullLogger(), onUsage: (u) => { usage = u; },
    });
    assert.equal(text, "ok");
    assertProviderEnv(seen[0]!.env, "model pass");
    assert.equal(seen[0]!.model, "sonnet");
    assert.ok(usage, "usage was reported");
    assert.deepStrictEqual(Object.values(usage)[0]!.costUSD, undefined, "no vendor-priced cost");
    assert.equal(Object.values(usage)[0]!.inputTokens, 3);
  });

  it("an OAuth pass is unchanged: cost reported, OAuth env", async () => {
    const { queryFn, seen } = capturingQuery(textFrames());
    let usage: AdviceUsageSnapshot | undefined;
    await runReadOnlyModelPass({
      token: OAUTH, model: "haiku", systemPrompt: "s", prompt: "p", homeRoot: PASS_HOME_ROOT, homePrefix: "uzi-summary-", label: "summary",
      timeoutMs: 5000, queryFn, denyReason: "read-only", log: nullLogger(), onUsage: (u) => { usage = u; },
    });
    assert.equal(seen[0]!.env?.CLAUDE_CODE_OAUTH_TOKEN, OAUTH);
    assert.equal(Object.values(usage!)[0]!.costUSD, 0.5);
  });
});

// ── lanes the server refuses (second layer) ──────────────────────────────────────────────────

function recordingClient(): { client: WorkerClient; states: { id: string; body: { status?: string; failure_reason?: string } }[]; calls: string[] } {
  const states: { id: string; body: { status?: string; failure_reason?: string } }[] = [];
  const calls: string[] = [];
  const client = new Proxy({}, {
    get: (_t, name: string) => {
      if (name === "reportState") return async (id: string, body: { status?: string; failure_reason?: string }) => { states.push({ id, body }); return { applied: true, status: body.status }; };
      if (name === "getTrace") return async () => { calls.push("getTrace"); return { target: { id: "t", kind: "issue", status: "completed", issue_title: "x", issue_description: "d", branch: null, mr_iid: 7, failure_reason: null, fix_verdict: null, plan_md: "", iteration_count: 1, repo_agents: null, stop_kind: null }, messages: [], inputs: [] }; };
      return async () => { calls.push(String(name)); return undefined; };
    },
  }) as unknown as WorkerClient;
  return { client, states, calls };
}

describe("lanes the server refuses reject a provider claim with provider_lane_unsupported", () => {
  const secrets = { forge_pat: "", anthropic_oauth_token: "", provider: binding() };

  it("judge: reports failed, makes no model call, posts no review", async () => {
    const { client, states, calls } = recordingClient();
    let modelCalls = 0;
    const queryFn = (() => { modelCalls++; return (async function* () { /* none */ })(); }) as unknown as SdkQueryFn;
    const { logger, added, removed } = secretRecordingLogger();
    await new JudgeRunner(client, logger, { homeRoot: testOwnedPassHomeRoot(), queryFn }).execute({
      run_id: "judge-1", kind: "judge", target_run_id: "t", issue_iid: null, issue_title: "J", issue_description: "", repo: {} as never,
      secrets: secrets as never, last_seq: 0, agents: [], judge_model: "haiku", claim_generation: 3,
    } as unknown as ClaimResponse);
    assert.equal(modelCalls, 0);
    assert.ok(!calls.includes("postReview"));
    const failed = states.find((s) => s.body.status === "failed");
    assert.ok(failed, "the judge is reported failed");
    assert.match(failed.body.failure_reason ?? "", /provider_lane_unsupported/);
    assert.ok(added.includes(KEY), "the gateway key was registered with the logger");
    assert.ok(removed.includes(KEY), "and evicted again on the refusal path");
  });

  it("job: fails terminally with the typed reason before any workspace or model work", async () => {
    const { client, states } = recordingClient();
    let modelCalls = 0;
    const queryFn = (() => { modelCalls++; return (async function* () { /* none */ })(); }) as unknown as SdkQueryFn;
    const jobsRoot = path.join(homeDir, "jobs");
    const { logger, added, removed } = secretRecordingLogger();
    await new JobRunner(client, logger, { queryFn, jobsRoot, batchMs: 5, cancelPollMs: 10 }).execute({
      run_id: "job-1", kind: "job", issue_iid: null, issue_title: "", issue_description: "", repo: undefined, secrets: secrets as never, last_seq: 0, agents: [],
      claim_generation: 4, budget_wall_seconds: 30, job: { type: "research", title: "t", prompt: "p", inputs: [] }, config: {},
    } as unknown as ClaimResponse);
    assert.equal(modelCalls, 0);
    assert.ok(!fs.existsSync(jobsRoot) || fs.readdirSync(jobsRoot).length === 0, "no workspace was created");
    const failed = states.find((s) => s.body.status === "failed");
    assert.ok(failed, "the job is reported failed");
    assert.match(failed.body.failure_reason ?? "", /provider_lane_unsupported/);
    assert.ok(added.includes(KEY), "the gateway key was registered with the logger");
    assert.ok(removed.includes(KEY), "and evicted again when the job ended");
  });

  it("job: a sibling holder of the same key keeps it scrubbed after the refusal (add/remove are symmetric)", async () => {
    const { client } = recordingClient();
    const logger = createLogger("error");
    logger.addSecret(KEY); // a concurrent run on the shared logger holds the same key
    await new JobRunner(client, logger, { queryFn: (() => (async function* () { /* none */ })()) as unknown as SdkQueryFn,
      jobsRoot: path.join(homeDir, "jobs"), batchMs: 5, cancelPollMs: 10 }).execute({
      run_id: "job-2", kind: "job", issue_iid: null, issue_title: "", issue_description: "", repo: undefined, secrets: secrets as never, last_seq: 0, agents: [],
      claim_generation: 4, budget_wall_seconds: 30, job: { type: "research", title: "t", prompt: "p", inputs: [] }, config: {},
    } as unknown as ClaimResponse);
    assert.ok(!logger.redactDiagnostic!(`leak ${KEY} here`).includes(KEY), "the holder's registration survives the refusal");
    logger.removeSecret(KEY);
    assert.ok(logger.redactDiagnostic!(`leak ${KEY} here`).includes(KEY), "released once its own holder lets go");
  });

  it("isolated: the preflight refuses with provider_lane_unsupported", () => {
    const reason = preflightReason(
      { run_id: "i-1", kind: "issue", secrets, isolated_fetch: { profile: "p", hosts: [], credential: "c".repeat(20) } } as unknown as ClaimResponse,
      { fetcherUrl: "https://fetch.test", fetcherCaFile: "/ca", splitActive: false },
    );
    assert.equal(reason, "provider_lane_unsupported");
  });

  it("cross-check Claude checker: refuses before any query", async () => {
    let queries = 0;
    const queryFn = (() => { queries++; return (async function* () { /* none */ })(); }) as unknown as SdkQueryFn;
    const checker = new ClaudeCrossCheck(nullLogger(), { secretPaths: ["/run/secrets/"], queryFn });
    await assert.rejects(
      checker.run({
        kind: "cross_check", run_id: "c-1", claim_generation: 1, last_seq: 0, agents: [], issue_iid: null, issue_title: "I", issue_description: "d",
        repo: { id: "r", full_name: "o/r", url: "https://example.test/r", clone_url: "https://example.test/r.git", default_branch: "main" },
        config: {}, secrets, cross_check: { stage: "plan", round: 1 },
      } as unknown as ClaimResponse, "/checkout", "/home", new AbortController().signal, async () => {}),
      /provider_lane_unsupported/,
    );
    assert.equal(queries, 0);
  });
});
