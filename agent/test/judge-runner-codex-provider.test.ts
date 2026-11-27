import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { JudgeRunner } from "../src/judge-runner.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import type { Logger } from "../src/log.js";
import { runReadOnlyModelPass, type ModelPassSettlement } from "../src/model-pass.js";
import { providerKeyForms } from "../src/redact.js";
import { PROVIDER_LANE_UNSUPPORTED } from "../src/provider-binding.js";
import { resolvePendingTerminal } from "../src/terminal-resolve.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type {
  ClaimCodexProviderBinding,
  ClaimProviderBinding,
  ClaimResponse,
  JudgeTraceResponse,
  OutgoingMessage,
  ReviewRequest,
  StateRequest,
} from "../src/protocol.js";
import type {
  AdviceRequest,
  AdviceResult,
  AdviceResultPolicy,
  AdviceUsageSnapshot,
  HarnessErrorCategory,
  HarnessTerminal,
} from "../src/harness.js";
import type { CodexAdviceHarnessFactory } from "../src/codex/codex-executor.js";
import { AdviceCallTimeoutError, CodexAdviceHarness, type CodexAdviceLaunchResult } from "../src/codex/codex-advice-harness.js";
import type { CodexProviderConfig } from "../src/codex/codex-harness.js";
import type { CodexAppServerAuthSession } from "../src/codex/appserver-auth.js";
import type { CodexNotification, CodexTransport } from "../src/codex/transport.js";
import { createCodexProviderExecutionAuthority } from "../src/codex/select.js";
import { isCodexProviderGatewayLimit } from "../src/codex/terminal-normalize.js";
import { nullLogger } from "./helpers.js";
import { testOwnedPassHomeRoot } from "./teardown-fixtures.js";

// Issue #4574 M1: the Codex provider-bound JUDGE on the worker. Every case drives the real
// JudgeRunner.execute with an injected Codex advice-harness factory (a scripted fake harness, or a real
// CodexAdviceHarness over an in-memory transport for the classified-terminal rows) and a fake api client,
// and asserts only what the api would observe: the posted state/review/usage and the captured log text.
// The gateway key is assembled at runtime: no provider-token-shaped literal in source.

const PASS_HOME_ROOT = testOwnedPassHomeRoot();
const KEY = ["gw", "key/do-not-scan", "4444<abcdefgh&z"].join("-");
const REDACTED = "***REDACTED***";
const JUDGE_MODEL = "gpt-6-sol";

// Journal directories created by the outbox-backed row.
const journalTmp: string[] = [];
afterEach(async () => {
  for (const dir of journalTmp.splice(0)) await fsp.rm(dir, { recursive: true, force: true });
});

const trace: JudgeTraceResponse = {
  target: {
    id: "target-1", kind: "issue", status: "completed", issue_title: "Do X", issue_description: "d", branch: null,
    mr_iid: 7, failure_reason: null, fix_verdict: null, plan_md: "# plan", iteration_count: 2, repo_agents: null, stop_kind: null,
  },
  messages: [],
  inputs: [],
};

function codexBinding(credential = KEY): ClaimProviderBinding {
  return {
    provider_id: "p-codex", revision: 3, engine: "codex", protocol: "openai_responses", base_url: "https://gw.example.test/v1",
    auth_style: "bearer", credential,
    model_map: { default: { model: "gw-default" }, [JUDGE_MODEL]: { model: "gw-sol" } },
  } as ClaimProviderBinding;
}

function codexClaim(over: Partial<ClaimResponse> = {}, credential = KEY): ClaimResponse {
  return {
    run_id: "judge-1", kind: "judge", target_run_id: "target-1", claim_generation: 5, issue_iid: null, issue_title: "Judge: Do X",
    issue_description: "", repo: {} as never, last_seq: 0, agents: [], judge_model: JUDGE_MODEL,
    judge_signal: { missing_tools: [{ command: "jq", evidence: "jq: command not found" }] },
    secrets: { forge_pat: "", anthropic_oauth_token: "", provider: codexBinding(credential) } as never,
    ...over,
  } as ClaimResponse;
}

interface Calls {
  review?: ReviewRequest;
  states: StateRequest[];
  messages: OutgoingMessage[][];
  order: string[];
}

interface ClientOverrides {
  getTrace?: () => Promise<JudgeTraceResponse>;
  postReview?: () => Promise<void>;
  postMessages?: () => Promise<void>;
  reportState?: (body: StateRequest) => Promise<unknown>;
}

function fakeClient(over: ClientOverrides = {}) {
  const calls: Calls = { states: [], messages: [], order: [] };
  const client = {
    getTrace: async () => { calls.order.push("getTrace"); return over.getTrace ? over.getTrace() : trace; },
    postReview: async (_id: string, review: ReviewRequest) => {
      if (over.postReview) await over.postReview();
      calls.review = review;
    },
    reportState: async (_id: string, body: StateRequest) => {
      calls.order.push(`state:${body.status}`);
      if (over.reportState) {
        const out = await over.reportState(body);
        if (out !== undefined) return out as never;
      }
      calls.states.push(body);
      return { applied: true, status: body.status } as never;
    },
    postMessages: async (_id: string, messages: OutgoingMessage[]) => {
      if (over.postMessages) await over.postMessages();
      calls.messages.push(messages);
    },
  } as unknown as WorkerClient;
  return { client, calls };
}

// --- fake advice harness -------------------------------------------------------

type Classification = { classification: string; category: HarnessErrorCategory; httpStatus?: number };
type Script = (request: AdviceRequest, policy: AdviceResultPolicy) => Promise<AdviceResult>;

function term(outcome: "success" | "failed", classification?: Classification): HarnessTerminal {
  return {
    outcome, subtype: outcome === "success" ? "completed" : "failed", errors: [], metrics: { cost: { kind: "unreported" } },
    ...(classification ? { codexClassification: classification } : {}),
  };
}

const verdict = (summary = "fine") => JSON.stringify({ verdict: "ok", summary, recommendations: [] });
const result = (text: string): AdviceResult => ({ text, end: { kind: "terminal", terminal: term("success") } }) as unknown as AdviceResult;
const usageSnapshot = (): AdviceUsageSnapshot => ({ "gw-model": { inputTokens: 1, outputTokens: 2, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costStatus: "unreported" } }) as unknown as AdviceUsageSnapshot;
/** A usage snapshot whose member read throws: usage.flush's redaction throws inside its try, so the
 *  pass logs `judge usage callback failed` (the "onUsage throws" case). */
const hostileUsage = (): AdviceUsageSnapshot => ({ get "gw-model"(): never { throw new Error("hostile usage member"); } }) as unknown as AdviceUsageSnapshot;

const RATE: Classification = { classification: "rateLimitExceeded", category: "rate_limit" };
const OVERLOADED: Classification = { classification: "serverOverloaded", category: "transport" };
const INTERNAL: Classification = { classification: "internalServerError", category: "transport" };

const okScript: Script = async (_request, policy) => {
  policy.onTerminal(term("success"), { isError: false, latest: undefined });
  return result(verdict());
};
const failScript = (cls?: Classification): Script => async (_request, policy) => {
  policy.onTerminal(term("failed", cls), { isError: true, latest: undefined });
  throw new Error("unreachable: the pass policy throws on an error terminal");
};
const withUsage = (inner: Script, snapshot: () => AdviceUsageSnapshot = usageSnapshot): Script => async (request, policy) => {
  request.usageObserver?.(snapshot());
  return inner(request, policy);
};

interface Rec { builds: Parameters<CodexAdviceHarnessFactory>[0][]; requests: AdviceRequest[] }
const newRec = (): Rec => ({ builds: [], requests: [] });

function scriptedFactory(script: Script, rec: Rec = newRec()): CodexAdviceHarnessFactory {
  return (async (params: Parameters<CodexAdviceHarnessFactory>[0]) => {
    rec.builds.push(params);
    return {
      kind: "codex",
      async run(request: AdviceRequest, policy: AdviceResultPolicy): Promise<AdviceResult> {
        rec.requests.push(request);
        return script(request, policy);
      },
    };
  }) as unknown as CodexAdviceHarnessFactory;
}

// --- real advice harness over an in-memory transport ----------------------------

const PROVIDER_CFG: CodexProviderConfig = { name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: "gpt-6-astra" };

class ScriptedTransport implements CodexTransport {
  private readonly queue: CodexNotification[];
  constructor(notes: CodexNotification[], private readonly closeThrows = false) { this.queue = [...notes]; }
  request<T = unknown>(method: string): Promise<T> {
    if (method === "thread/start") return Promise.resolve({ thread: { id: "th-1" } } as T);
    if (method === "turn/start") return Promise.resolve({ turn: { id: "tn-1" } } as T);
    return Promise.resolve({} as T);
  }
  notify(): void {}
  respond(): void {}
  notifications(): AsyncIterableIterator<CodexNotification> {
    const queue = this.queue;
    return {
      next: async () => {
        const value = queue.shift();
        return value === undefined ? { value: undefined, done: true } : { value, done: false };
      },
      return: async () => ({ value: undefined, done: true }),
      [Symbol.asyncIterator]() { return this; },
    } as AsyncIterableIterator<CodexNotification>;
  }
  async close(): Promise<void> {
    if (this.closeThrows) throw new Error("transport close boom");
  }
}

function agentMessage(text: string): CodexNotification {
  return { kind: "activity", method: "item/completed", params: { threadId: "th-1", item: { type: "agentMessage", text } } };
}
function completed(): CodexNotification {
  return { kind: "turn_completed", method: "turn/completed", threadId: "th-1", turnId: "tn-1", status: "completed",
    params: { threadId: "th-1", turn: { id: "tn-1", status: "completed" } } };
}
/** A failed terminal; `info` undefined leaves turn.error absent (no classification at all). */
function failedWith(info?: unknown): CodexNotification {
  const turn: Record<string, unknown> = { id: "tn-1", status: "failed" };
  if (info !== undefined) turn.error = { codexErrorInfo: info };
  return { kind: "turn_completed", method: "turn/completed", threadId: "th-1", turnId: "tn-1", status: "failed",
    params: { threadId: "th-1", turn } };
}

function realFactory(opts: { notes: CodexNotification[]; closeThrows?: boolean; appServerAuth?: CodexAppServerAuthSession }, rec: Rec = newRec()): CodexAdviceHarnessFactory {
  return (async (params: Parameters<CodexAdviceHarnessFactory>[0]) => {
    rec.builds.push(params);
    const transport = new ScriptedTransport(opts.notes, opts.closeThrows);
    const launchRoot = async (): Promise<CodexAdviceLaunchResult> => ({ transport, cwd: "/isolated/advice-home/work", dispose: async () => {} });
    return new CodexAdviceHarness({ launchRoot, provider: PROVIDER_CFG, authMode: "provider", appServerAuth: opts.appServerAuth, log: nullLogger() });
  }) as unknown as CodexAdviceHarnessFactory;
}

/** An app-server auth session whose closeAdmissionAndCancel throws (the harness's cleanup path). */
function throwingAuth(): CodexAppServerAuthSession {
  return {
    mode: "provider",
    authenticate: async () => {},
    handleServerRequest: async () => false,
    drainInterceptedRequests: async () => {},
    suspendRefreshAdmission: () => { throw new Error("not used"); },
    settleReapedAuth: async () => {},
    closeAdmissionAndCancel: () => { throw new Error("close admission boom"); },
  } as unknown as CodexAppServerAuthSession;
}

// --- driver ----------------------------------------------------------------------

const forbiddenQuery = (() => assert.fail("the Claude query path must not run for a Codex provider judge")) as unknown as SdkQueryFn;

async function exec(
  factory: CodexAdviceHarnessFactory | undefined,
  claim: ClaimResponse = codexClaim(),
  extra: { log?: Logger; client?: ReturnType<typeof fakeClient>; modelTimeoutMs?: number; signal?: AbortSignal; outbox?: Outbox } = {},
): Promise<Calls> {
  const api = extra.client ?? fakeClient();
  const runner = new JudgeRunner(api.client, extra.log ?? nullLogger(), {
    homeRoot: PASS_HOME_ROOT, queryFn: forbiddenQuery, modelTimeoutMs: extra.modelTimeoutMs,
    ...(factory ? { codexAdviceHarnessFactory: factory } : {}),
    ...(extra.outbox ? { outbox: extra.outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 } : {}),
  });
  await runner.execute(claim, extra.signal);
  return api.calls;
}

function assertTerminalFailure(calls: Calls, reason?: RegExp | string): StateRequest {
  const last = calls.states.at(-1)!;
  assert.equal(last.status, "failed");
  assert.notEqual(last.failure_reason, PROVIDER_LANE_UNSUPPORTED, "not the pre-#4574 Claude-only refusal");
  assert.equal(last.rate_limit_type, undefined, "a terminal failure carries no limit facts");
  assert.equal(calls.review, undefined, "no review is posted");
  assert.equal(calls.states.some(s => s.status === "completed"), false, "never completed");
  if (typeof reason === "string") assert.equal(last.failure_reason, reason);
  else if (reason) assert.match(last.failure_reason ?? "", reason);
  return last;
}

function assertLimit(calls: Calls): void {
  const last = calls.states.at(-1)!;
  assert.equal(last.status, "failed");
  assert.equal(last.rate_limit_type, "provider_gateway_limit");
  assert.equal(last.limit_resets_at, undefined);
  assert.equal(last.failure_reason, undefined, "enums only: no free text on a limit");
  assert.equal(last.claim_generation, 5);
  assert.equal(calls.review, undefined, "no review is posted");
  assert.equal(calls.states.some(s => s.status === "completed"), false, "never completed");
}

function assertFallbackReview(calls: Calls): void {
  assert.equal(calls.review?.recommendations[0]?.target, "jq", "the deterministic fallback review posted");
  assert.equal(calls.states.at(-1)?.status, "completed");
  assert.equal(calls.states.some(s => s.status === "failed"), false);
}

function assertNoKey(text: string, what = "output"): void {
  // The recording logger JSON-stringifies its fields, which doubles a form's backslashes: check both spellings.
  for (const form of providerKeyForms(KEY)) {
    assert.ok(!text.includes(form), `a key form reached the ${what}: ${form.slice(0, 8)}`);
    assert.ok(!text.includes(JSON.stringify(form).slice(1, -1)), `a JSON-escaped key form reached the ${what}: ${form.slice(0, 8)}`);
  }
  assert.ok(!text.replace(/[\p{Cc}\p{Cf}]/gu, "").includes(KEY), `an invisible-separated key reached the ${what}`);
}

/** Every encoding a gateway echo might use: raw, JSON-escaped, and split by zero-width spaces. */
const SLASH_ESCAPED = KEY.replaceAll("/", "\\/");
const HTML_SAFE = KEY.replace(/[<>&]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, "0")}`);
const INVISIBLE = KEY.split("").join("\u200b");
for (const form of [SLASH_ESCAPED, HTML_SAFE, INVISIBLE]) assert.notEqual(form, KEY, "each echoed form is a distinct encoding of the key");
it("assertNoKey catches a JSON-escaped echo of the key (negative control)", () => {
  assert.throws(() => assertNoKey(JSON.stringify(SLASH_ESCAPED)), /key form/);
  assert.throws(() => assertNoKey(JSON.stringify(HTML_SAFE)), /key form/);
});
const ECHO = (): string => [KEY, SLASH_ESCAPED, HTML_SAFE, INVISIBLE].join(" | ");

function capturingLog(lines: string[]): Logger {
  const base = nullLogger();
  const push = (level: string) => (msg: string, fields?: Record<string, unknown>): void => {
    lines.push(JSON.stringify([level, msg, fields ?? {}], (_k, v: unknown) => (v instanceof Error ? v.message : v)));
  };
  const log: Logger = { ...base, debug: push("debug"), info: push("info"), warn: push("warn"), error: push("error"), child: () => log };
  return log;
}

function warnThrows(match: RegExp, thrown: () => unknown): Logger {
  const base = nullLogger();
  return { ...base, warn: (msg: string) => { if (match.test(msg)) throw thrown(); } };
}

// --- tests -------------------------------------------------------------------------

describe("codex provider judge: routing", () => {
  it("builds the harness with the frozen provider authority, enters harness.run, and posts the review (red on the Claude-only route)", async () => {
    const rec = newRec();
    const calls = await exec(scriptedFactory(okScript, rec));
    assert.equal(rec.builds.length, 1, "the factory was invoked");
    const params = rec.builds[0]!;
    assert.equal(params.binding.authMode, "provider");
    if (params.binding.authMode !== "provider") return;
    assert.equal(params.binding.providerBinding.credential, KEY);
    assert.equal(params.binding.providerBinding.engine, "codex");
    assert.ok(Object.isFrozen(params.binding.providerBinding), "the authority snapshot is frozen");
    assert.equal(params.runId, "judge-1");
    assert.equal(params.claimGeneration, 5);
    assert.equal(rec.requests.length, 1, "harness.run was entered");
    assert.equal(rec.requests[0]!.model, JUDGE_MODEL, "the server-chosen model reaches the request unchanged");
    assert.equal(rec.requests[0]!.label, "judge");
    assert.equal(calls.review?.status, "complete");
    assert.equal(calls.states.at(-1)?.status, "completed");
  });

  it("a provider key split by the 180-character missing-tool clip never reaches the model prompt", async () => {
    // The key starts at offset 160, so clip(command, 180) would keep only its first 20 characters:
    // a partial key the exact-form redactor cannot match. The signal must be scrubbed before clipping.
    const command = `${"x".repeat(160)}${KEY} --tail`;
    const claim = codexClaim({ judge_signal: { missing_tools: [{ command, evidence: "not found" }] } });
    const rec = newRec();
    await exec(scriptedFactory(okScript, rec), claim);
    assert.equal(rec.requests.length, 1, "harness.run was entered");
    const prompt = rec.requests[0]!.prompt;
    assert.ok(prompt.includes("x".repeat(160)), "the missing-tool command reached the prompt");
    for (let n = 8; n <= KEY.length; n++) {
      assert.ok(!prompt.includes(KEY.slice(0, n)), `the prompt carries a ${n}-character key prefix`);
    }
  });

  it("a missing factory is a terminal failure, not a fallback review", async () => {
    const calls = await exec(undefined);
    assertTerminalFailure(calls, /no Codex advice-harness factory/);
  });

  it("an unparseable success text keeps the scrubbed fallback review and completes", async () => {
    const script: Script = async (_r, policy) => { policy.onTerminal(term("success"), { isError: false, latest: undefined }); return result("not json at all"); };
    const calls = await exec(scriptedFactory(script));
    assertFallbackReview(calls);
  });

  it("a normal return with no recorded terminal is a terminal failure", async () => {
    const calls = await exec(scriptedFactory(async () => result(verdict())));
    assertTerminalFailure(calls, "provider judge model returned no usable result");
  });

  it("the usage evidence posts as a judge usage frame with unknown cost", async () => {
    const calls = await exec(scriptedFactory(withUsage(okScript)));
    assert.equal(calls.messages.length, 1);
    const frame = calls.messages[0]![0]!;
    assert.equal(frame.agent, "judge");
    const payload = frame.payload as { event: string; modelUsage: Record<string, { costUSD?: number }> };
    assert.equal(payload.event, "result");
    for (const u of Object.values(payload.modelUsage)) assert.equal(u.costUSD, undefined);
  });

  it("a prompt-build failure posts the scrubbed fallback with a scrubbed model and never invokes the factory", async () => {
    const malformed = { ...trace, target: { ...trace.target, plan_md: { length: 7000 } } } as unknown as JudgeTraceResponse;
    const rec = newRec();
    const claim = codexClaim({ judge_model: `m-${KEY}`, judge_signal: { missing_tools: [{ command: "jq", evidence: `${KEY}: command not found` }] } });
    const calls = await exec(scriptedFactory(okScript, rec), claim, { client: fakeClient({ getTrace: async () => malformed }) });
    assert.equal(rec.builds.length, 0, "the factory is never invoked");
    assertFallbackReview(calls);
    assertNoKey(JSON.stringify(calls.review), "fallback review");
    assert.ok(calls.review!.model.includes(REDACTED), "the model is scrubbed");
  });

  it("a Claude provider binding still takes the Claude provider path (the Codex factory is not built)", async () => {
    const rec = newRec();
    const claude = { ...codexClaim(), secrets: { forge_pat: "", anthropic_oauth_token: "", provider: {
      provider_id: "p-1", revision: 3, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/anthropic",
      auth_style: "bearer", credential: KEY, model_map: { haiku: { model: "h" }, sonnet: { model: "s" } } } } as never, judge_model: "sonnet" };
    let queried = 0;
    const query = ((): AsyncGenerator<unknown> => { queried++; return (async function* () { yield { type: "result", subtype: "success", is_error: false }; })(); }) as unknown as SdkQueryFn;
    const api = fakeClient();
    await new JudgeRunner(api.client, nullLogger(), { homeRoot: PASS_HOME_ROOT, queryFn: query, codexAdviceHarnessFactory: scriptedFactory(okScript, rec) }).execute(claude as ClaimResponse);
    assert.equal(rec.builds.length, 0);
    assert.equal(queried, 1);
  });
});

describe("codex provider judge: settlement of a classified terminal (real advice harness)", () => {
  const rows: Array<[string, unknown, "limit" | "terminal" | "fallback"]> = [
    ["rateLimitExceeded", "rateLimitExceeded", "limit"],
    ["serverOverloaded", "serverOverloaded", "limit"],
    ["responseStreamDisconnected 429", { responseStreamDisconnected: { httpStatusCode: 429 } }, "limit"],
    ["responseStreamConnectionFailed 529", { responseStreamConnectionFailed: { httpStatusCode: 529 } }, "limit"],
    ["httpConnectionFailed 429", { httpConnectionFailed: { httpStatusCode: 429 } }, "limit"],
    ["responseTooManyFailedAttempts 529", { responseTooManyFailedAttempts: { httpStatusCode: 529 } }, "limit"],
    ["httpConnectionFailed with no status", { httpConnectionFailed: {} }, "terminal"],
    ["responseStreamDisconnected with no status", { responseStreamDisconnected: {} }, "terminal"],
    ["responseStreamConnectionFailed with no status", { responseStreamConnectionFailed: {} }, "terminal"],
    ["responseTooManyFailedAttempts with no status", { responseTooManyFailedAttempts: {} }, "terminal"],
    ["no classification at all", undefined, "terminal"],
    ["other", "other", "terminal"],
    ["an unknown tag", "someFutureTag", "terminal"],
    ["httpConnectionFailed 500 (a status that is not a limit)", { httpConnectionFailed: { httpStatusCode: 500 } }, "fallback"],
    ["scalar internalServerError", "internalServerError", "fallback"],
    ["scalar badRequest", "badRequest", "fallback"],
    ["unauthorized", "unauthorized", "fallback"],
    ["contextWindowExceeded", "contextWindowExceeded", "fallback"],
  ];
  for (const [name, info, expected] of rows) {
    it(`${name} settles as ${expected}`, async () => {
      const rec = newRec();
      const calls = await exec(realFactory({ notes: [failedWith(info)] }, rec));
      assert.equal(rec.builds.length, 1, "the provider factory was invoked");
      assert.equal(rec.builds[0]!.binding.authMode, "provider");
      if (expected === "limit") assertLimit(calls);
      else if (expected === "terminal") assertTerminalFailure(calls);
      else assertFallbackReview(calls);
    });
  }

  it("a success terminal through the real harness parses and completes", async () => {
    const calls = await exec(realFactory({ notes: [agentMessage(verdict("real")), completed()] }));
    assert.equal(calls.review?.summary, "real");
    assert.equal(calls.states.at(-1)?.status, "completed");
  });

  it("a stream that ends before any terminal is a terminal failure", async () => {
    const calls = await exec(realFactory({ notes: [agentMessage(verdict())] }));
    assertTerminalFailure(calls, /stream ended before turn completion/);
  });
});

describe("codex provider judge: a cleanup failure never settles as the classified outcome", () => {
  for (const [terminalName, info] of [["a gateway-limit terminal", "rateLimitExceeded"], ["a fallback-class terminal", "internalServerError"]] as const) {
    it(`transport.close throwing after ${terminalName} is a terminal failure`, async () => {
      const calls = await exec(realFactory({ notes: [failedWith(info)], closeThrows: true }));
      assertTerminalFailure(calls, "transport close boom");
    });
    it(`closeAdmissionAndCancel throwing after ${terminalName} is a terminal failure`, async () => {
      const calls = await exec(realFactory({ notes: [failedWith(info)], appServerAuth: throwingAuth() }));
      assertTerminalFailure(calls, "close admission boom");
    });
  }
  it("transport.close throwing after a success terminal is a terminal failure, not a review", async () => {
    const calls = await exec(realFactory({ notes: [agentMessage(verdict()), completed()], closeThrows: true }));
    assertTerminalFailure(calls, "transport close boom");
  });
});

describe("codex provider judge: a recorded stop outranks everything", () => {
  const waitForAbort = (request: AdviceRequest): Promise<void> => new Promise<void>(resolve => {
    if (request.signal.aborted) resolve();
    else request.signal.addEventListener("abort", () => resolve(), { once: true });
  });
  const recordThenWait = (cls: Classification): Script => async (request, policy) => {
    try { policy.onTerminal(term("failed", cls), { isError: true, latest: undefined }); } catch (error) {
      await waitForAbort(request);
      throw error;
    }
    throw new Error("unreachable");
  };

  it("the outer timer firing after a recorded limit terminal is a terminal timeout failure", async () => {
    const calls = await exec(scriptedFactory(recordThenWait(RATE)), codexClaim(), { modelTimeoutMs: 40 });
    assertTerminalFailure(calls, /judge model call exceeded 40ms/);
  });

  it("a terminal arriving after the outer timer records nothing and fails terminal", async () => {
    const late: Script = async (request, policy) => {
      await waitForAbort(request);
      policy.onTerminal(term("failed", RATE), { isError: true, latest: undefined });
      throw new Error("unreachable");
    };
    const rec = newRec();
    const calls = await exec(scriptedFactory(late, rec), codexClaim(), { modelTimeoutMs: 40 });
    assert.equal(rec.requests.length, 1, "harness.run was entered");
    assertTerminalFailure(calls, /judge model call exceeded 40ms/);
  });

  it("an inner AdviceCallTimeoutError after a recorded limit terminal is a terminal failure", async () => {
    const script: Script = async (_request, policy) => {
      try { policy.onTerminal(term("failed", RATE), { isError: true, latest: undefined }); } catch { /* the harness timer won */ }
      throw new AdviceCallTimeoutError("judge model call exceeded 5ms");
    };
    const calls = await exec(scriptedFactory(script));
    assertTerminalFailure(calls, "judge model call exceeded 5ms");
  });

  it("a bare inner AdviceCallTimeoutError is a terminal failure", async () => {
    const calls = await exec(scriptedFactory(async () => { throw new AdviceCallTimeoutError("judge model call exceeded 5ms"); }));
    assertTerminalFailure(calls, "judge model call exceeded 5ms");
  });

  it("an external abort after a recorded limit terminal is a terminal failure", async () => {
    const controller = new AbortController();
    setTimeout(() => controller.abort(), 30);
    const calls = await exec(scriptedFactory(recordThenWait(RATE)), codexClaim(), { signal: controller.signal });
    assertTerminalFailure(calls, /judge model call aborted/);
  });

  it("an external abort that arrives first, then a terminal, is a terminal failure", async () => {
    const controller = new AbortController();
    setTimeout(() => controller.abort(), 30);
    const late: Script = async (request, policy) => {
      await waitForAbort(request);
      policy.onTerminal(term("failed", RATE), { isError: true, latest: undefined });
      throw new Error("unreachable");
    };
    const calls = await exec(scriptedFactory(late), codexClaim(), { signal: controller.signal });
    assertTerminalFailure(calls, /judge model call aborted/);
  });
});

describe("codex provider judge: a throw from the pass finally is never the classified outcome", () => {
  const usageBoom = (): Logger => warnThrows(/usage callback failed/, () => new Error(`warn boom ${KEY}`));
  const cases: Array<[string, Script]> = [
    ["a success terminal", okScript],
    ["a fallback-class terminal", failScript(INTERNAL)],
    ["a gateway-limit terminal", failScript(RATE)],
  ];
  for (const [name, inner] of cases) {
    it(`onUsage failing plus a throwing pass logger after ${name} is a terminal failure with the key scrubbed`, async () => {
      const calls = await exec(scriptedFactory(withUsage(inner, hostileUsage)), codexClaim(), { log: usageBoom() });
      const last = assertTerminalFailure(calls, /warn boom/);
      assert.ok((last.failure_reason ?? "").includes(REDACTED));
      assertNoKey(last.failure_reason ?? "", "failure_reason");
      assert.equal(calls.messages.length, 0, "no usage frame posted");
    });
  }

  it("control: a failing onUsage whose warn does not throw leaves the review intact", async () => {
    const calls = await exec(scriptedFactory(withUsage(okScript, hostileUsage)));
    assert.equal(calls.review?.status, "complete");
    assert.equal(calls.states.at(-1)?.status, "completed");
  });

  it("control: a failing onUsage whose warn does not throw leaves a limit a limit", async () => {
    assertLimit(await exec(scriptedFactory(withUsage(failScript(RATE), hostileUsage))));
  });

  it("a finally that throws undefined after success is a terminal failure (the presence flag, not the value)", async () => {
    const sentinel: unknown = undefined;
    const rec = newRec();
    const calls = await exec(scriptedFactory(withUsage(okScript, hostileUsage), rec), codexClaim(), {
      log: warnThrows(/usage callback failed/, () => sentinel),
    });
    assert.equal(rec.requests.length, 1, "harness.run was entered and a success terminal scripted");
    assertTerminalFailure(calls);
  });
});

describe("codex provider judge: other rejections", () => {
  it("a factory-build rejection is a terminal failure with the key scrubbed", async () => {
    const factory = (async () => { throw new Error(`build failed ${ECHO()}`); }) as unknown as CodexAdviceHarnessFactory;
    const calls = await exec(factory);
    const last = assertTerminalFailure(calls, /build failed/);
    assertNoKey(last.failure_reason ?? "", "failure_reason");
  });

  it("a harness rejection that is not the policy error is a terminal failure", async () => {
    const calls = await exec(scriptedFactory(async () => { throw new Error("app-server exploded"); }));
    assertTerminalFailure(calls, "app-server exploded");
  });

  it("a plain policy error whose rejection was replaced by the harness is a terminal failure", async () => {
    const script: Script = async (_r, policy) => {
      try { policy.onTerminal(term("failed", RATE), { isError: true, latest: undefined }); } catch { /* replaced */ }
      throw new Error("cleanup replaced the policy error");
    };
    assertTerminalFailure(await exec(scriptedFactory(script)), "cleanup replaced the policy error");
  });

  it("a harness that swallows the policy error and returns text is a terminal failure", async () => {
    const script: Script = async (_r, policy) => {
      try { policy.onTerminal(term("failed", RATE), { isError: true, latest: undefined }); } catch { /* swallowed */ }
      return result(verdict());
    };
    assertTerminalFailure(await exec(scriptedFactory(script)));
  });
});

describe("codex provider judge: key redaction", () => {
  it("a gateway echo (raw, escaped and invisible-separated) in a thrown error is scrubbed from the report and the logs", async () => {
    const lines: string[] = [];
    const calls = await exec(scriptedFactory(async () => { throw new Error(`gateway said ${ECHO()}`); }), codexClaim(), { log: capturingLog(lines) });
    const last = assertTerminalFailure(calls, /gateway said/);
    assertNoKey(last.failure_reason ?? "", "failure_reason");
    assertNoKey(lines.join("\n"), "log");
    assert.ok(lines.some(l => /provider judge failed terminally/.test(l)), "the terminal branch logged");
  });

  it("a gateway-limit settlement logs the limit branch with no key form", async () => {
    const lines: string[] = [];
    const calls = await exec(scriptedFactory(failScript(RATE)), codexClaim(), { log: capturingLog(lines) });
    assertLimit(calls);
    assert.ok(lines.some(l => /judge model call hit a usage limit/.test(l)), "the limit branch logged");
    assertNoKey(lines.join("\n"), "log");
    assertNoKey(JSON.stringify(calls.states), "wire");
  });

  it("a getTrace error echoing the key keeps the fallback review and logs scrubbed", async () => {
    const lines: string[] = [];
    const api = fakeClient({ getTrace: async () => { throw new RequestError("GET", "/trace", 502, `bad gateway ${ECHO()}`); } });
    const calls = await exec(scriptedFactory(okScript), codexClaim(), { client: api, log: capturingLog(lines) });
    assertFallbackReview(calls);
    assertNoKey(lines.join("\n"), "log");
    assertNoKey(JSON.stringify([calls.review, calls.states]), "wire");
    assert.ok(lines.some(l => /judge trace\/prep failed/.test(l)));
  });

  it("a postReview error echoing the key reports a scrubbed failure and logs scrubbed", async () => {
    const lines: string[] = [];
    const api = fakeClient({ postReview: async () => { throw new RequestError("POST", "/review", 500, `upstream said ${ECHO()}`); } });
    const calls = await exec(scriptedFactory(okScript), codexClaim(), { client: api, log: capturingLog(lines) });
    const last = calls.states.at(-1)!;
    assert.equal(last.status, "failed");
    assertNoKey(last.failure_reason ?? "", "failure_reason");
    assertNoKey(lines.join("\n"), "log");
    assert.ok(lines.some(l => /judge post\/complete failed/.test(l)));
  });

  it("a postMessages error echoing the key is logged scrubbed and the review still posts", async () => {
    const lines: string[] = [];
    const api = fakeClient({ postMessages: async () => { throw new RequestError("POST", "/messages", 500, `usage said ${ECHO()}`); } });
    const calls = await exec(scriptedFactory(withUsage(okScript)), codexClaim(), { client: api, log: capturingLog(lines) });
    assert.equal(calls.review?.status, "complete");
    assertNoKey(lines.join("\n"), "log");
    assert.ok(lines.some(l => /judge usage post failed/.test(l)));
  });

  it("a reportState error echoing the key inside safeReportFailed is logged scrubbed", async () => {
    const lines: string[] = [];
    const api = fakeClient({ reportState: async (body) => {
      if (body.status === "failed") throw new RequestError("POST", "/state", 500, `state said ${ECHO()}`);
      return undefined;
    } });
    await exec(scriptedFactory(async () => { throw new Error("boom"); }), codexClaim(), { client: api, log: capturingLog(lines) });
    assertNoKey(lines.join("\n"), "log");
    assert.ok(lines.some(l => /failed-state report failed/.test(l)), "the report failure was logged");
  });

  it("an outbox-backed terminal: the journaled body and the outbox log lines are key-free", async () => {
    const dir = await fsp.mkdtemp(path.join(os.tmpdir(), "codex-judge-journal-"));
    journalTmp.push(dir);
    const outbox = new Outbox({ root: path.join(dir, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    const lines: string[] = [];
    const api = fakeClient({ reportState: async (body) => {
      if (body.status === "failed") throw new RequestError("POST", "/state", 500, `state said ${ECHO()}`);
      return undefined;
    } });
    Object.assign(api.client, { hasFeature: () => false, getMessageGaps: async () => ({ gaps: [] }) });
    await exec(scriptedFactory(async () => { throw new Error(`gateway ${ECHO()}`); }), codexClaim(), { client: api, log: capturingLog(lines), outbox });
    assertNoKey(lines.join("\n"), "log");
    assert.ok(lines.some(l => /outbox: terminal report send failed/.test(l)), "the outbox logged the failed send");
    const journal = await outbox.readTerminalJournal("judge-1", 5);
    assert.ok(journal, "the terminal outcome is journaled");
    assert.equal(journal.body.status, "failed");
    assertNoKey(JSON.stringify(journal.body), "journaled body");

    // A later resolve by the worker (sweep, drain or boot) after JudgeRunner exited: plain worker logger,
    // no per-run redactor. A key-echoing failure response must still never reach that log.
    const replayLines: string[] = [];
    await resolvePendingTerminal({ outbox, client: api.client, gapFillMax: 100, terminalMaxBytes: 1 << 20, log: capturingLog(replayLines) }, {
      runId: "judge-1", claimGeneration: 5,
      send: async () => { throw new RequestError("POST", "/state", 502, `replay said ${ECHO()}`); },
    });
    assert.ok(replayLines.some(l => /outbox: terminal report send failed/.test(l)), "the replay logged the failed send");
    assertNoKey(replayLines.join("\n"), "replay log");
    assert.ok(replayLines.some(l => l.includes("POST /state returned 502")), "the bounded diagnostic names the request and status");
    assert.ok(await outbox.readTerminalJournal("judge-1", 5), "the journal stays for a later resolve");
  });

  it("a key echoed in the model text, summary, model name and usage never reaches the wire", async () => {
    const echoed = JSON.stringify({ verdict: "issues", summary: `the key ${ECHO()} leaked`,
      recommendations: [{ category: "improve_uzi", target: `t-${KEY}`, rationale: `rationale ${KEY}`, confidence: "high" }] });
    const script: Script = async (request, policy) => {
      request.usageObserver?.({ [`m-${KEY}`]: { inputTokens: 1, outputTokens: 2, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costStatus: "unreported" } } as unknown as AdviceUsageSnapshot);
      policy.onTerminal(term("success"), { isError: false, latest: undefined });
      return result(echoed);
    };
    const calls = await exec(scriptedFactory(script), codexClaim({ judge_model: `m-${KEY}` }));
    assert.equal(calls.review?.status, "complete");
    assertNoKey(JSON.stringify([calls.review, calls.messages, calls.states]), "wire");
    assert.ok(JSON.stringify(calls.review).includes(REDACTED));
  });
});

describe("codex provider judge: call surface", () => {
  it("calls only getTrace / postMessages / postReview / reportState on the client", async () => {
    const api = fakeClient();
    const accessed = new Set<string>();
    const spy = new Proxy(api.client as unknown as object, {
      get(target, prop, receiver) {
        if (typeof prop === "string") accessed.add(prop);
        return Reflect.get(target, prop, receiver) as unknown;
      },
    }) as unknown as WorkerClient;
    await exec(scriptedFactory(withUsage(okScript)), codexClaim(), { client: { client: spy, calls: api.calls } });
    assert.ok(accessed.size > 0);
    for (const name of accessed) assert.ok(["getTrace", "postMessages", "postReview", "reportState"].includes(name), `unexpected client call: ${name}`);
    for (const name of ["getTrace", "postReview", "reportState"]) assert.ok(accessed.has(name), `${name} was used`);
  });

  it("the judge runner source imports no git or forge module", () => {
    const source = fs.readFileSync(fileURLToPath(new URL("../src/judge-runner.ts", import.meta.url)), "utf8");
    const imports = source.split("\n").filter(line => /^\s*import\b/.test(line) || /^\s*}\s*from\b/.test(line));
    assert.ok(imports.length > 5);
    // Every module specifier in any form (static, side-effect, re-export, dynamic import(), require()).
    const specifiers = [...source.matchAll(/(?:\bfrom\s*|\bimport\s*\(?\s*|\brequire\s*\(\s*)["'`]([^"'`]+)["'`]/g)].map(m => m[1]!);
    assert.ok(specifiers.length > 5, "the specifier scan found the imports");
    for (const spec of specifiers) {
      assert.ok(!/(^|[/.])(git|forge|gitlab|github|forgejo|gitea)([-./]|$)/i.test(spec), `forbidden import: ${spec}`);
      assert.ok(!/^(node:)?child_process$/.test(spec), `forbidden import: ${spec}`);
    }
  });
});

describe("shared gateway-limit predicate", () => {
  const table: Array<[string, "success" | "failed", string | undefined, Classification | undefined, boolean]> = [
    ["rateLimitExceeded under a provider", "failed", "provider", RATE, true],
    ["serverOverloaded under a provider", "failed", "provider", OVERLOADED, true],
    ["transport 429", "failed", "provider", { classification: "httpConnectionFailed", category: "transport", httpStatus: 429 }, true],
    ["transport 529", "failed", "provider", { classification: "responseStreamDisconnected", category: "transport", httpStatus: 529 }, true],
    ["transport 500", "failed", "provider", { classification: "httpConnectionFailed", category: "transport", httpStatus: 500 }, false],
    ["transport with no status", "failed", "provider", { classification: "httpConnectionFailed", category: "transport" }, false],
    ["a non-transport 429", "failed", "provider", { classification: "other", category: "unknown", httpStatus: 429 }, false],
    ["usageLimitExceeded (a vendor limit)", "failed", "provider", { classification: "usageLimitExceeded", category: "rate_limit" }, false],
    ["a success outcome", "success", "provider", RATE, false],
    ["a vendor api_key credential", "failed", "api_key", RATE, false],
    ["a vendor subscription credential", "failed", "subscription", RATE, false],
    ["no auth mode", "failed", undefined, RATE, false],
    ["no classification", "failed", "provider", undefined, false],
  ];
  for (const [name, outcome, authMode, classification, expected] of table) {
    it(`${name}: ${expected}`, () => {
      assert.equal(isCodexProviderGatewayLimit(outcome, authMode, classification), expected);
    });
  }

  it("the run lane decodes its gateway limit through the same exported predicate", () => {
    const source = fs.readFileSync(fileURLToPath(new URL("../src/codex/codex-harness.ts", import.meta.url)), "utf8");
    assert.ok(source.includes("isCodexProviderGatewayLimit(outcome, this.authMode, classification)"));
    assert.ok(!source.includes("isGatewayLimitStatus"), "no second copy of the status test in the run lane");
  });

  it("AdviceCallTimeoutError keeps the plain Error name and message shape", () => {
    const e = new AdviceCallTimeoutError("judge model call exceeded 5ms");
    assert.equal(e.name, "Error");
    assert.equal(String(e), "Error: judge model call exceeded 5ms");
    assert.ok(e instanceof Error);
  });
});

describe("the pass records settlement provenance (direct pass, judge-only settlement)", () => {
  const binding = codexBinding() as ClaimCodexProviderBinding;
  const fresh = (): ModelPassSettlement => ({ terminalSeen: false, policyErrorSet: false, rejectionSet: false, escapedSet: false });
  const pass = (script: Script, settlement: ModelPassSettlement, rec: Rec, extra: { timeoutMs?: number; signal?: AbortSignal } = {}) => {
    const authority = createCodexProviderExecutionAuthority(binding);
    return runReadOnlyModelPass({
      provider: authority.providerBinding as ClaimProviderBinding, model: JUDGE_MODEL, systemPrompt: "s", prompt: "p",
      homeRoot: PASS_HOME_ROOT, homePrefix: "uzi-judge-", label: "judge", timeoutMs: extra.timeoutMs ?? 5000, queryFn: forbiddenQuery,
      denyReason: "read-only", log: nullLogger(), ...(extra.signal ? { signal: extra.signal } : {}),
      codex: { runId: "r1", binding: authority, buildHarness: scriptedFactory(script, rec), settlement },
    });
  };
  const waitAbort = (request: AdviceRequest): Promise<void> => new Promise<void>(resolve => {
    if (request.signal.aborted) resolve(); else request.signal.addEventListener("abort", () => resolve(), { once: true });
  });

  it("a recorded limit terminal: terminal, policy error, rejection identity and the escaped value are all recorded", async () => {
    const settlement = fresh(); const rec = newRec();
    let caught: unknown;
    await pass(failScript(RATE), settlement, rec).catch((e: unknown) => { caught = e; });
    assert.equal(rec.builds.length, 1); assert.equal(rec.requests.length, 1);
    assert.equal(settlement.terminalSeen, true);
    assert.deepEqual(settlement.terminal, { isError: true, classification: RATE });
    assert.equal(settlement.policyErrorSet, true); assert.equal(settlement.rejectionSet, true);
    assert.equal(settlement.rejection, settlement.policyError, "the run promise rejected with the policy error itself");
    assert.equal(settlement.escapedSet, true); assert.equal(caught, settlement.escaped, "the caller sees the recorded escaped value");
    assert.equal(settlement.stop, undefined);
  });

  it("a terminal arriving after the pass timer is never recorded (stop = timeout)", async () => {
    const settlement = fresh(); const rec = newRec();
    const late: Script = async (request, policy) => {
      await waitAbort(request);
      try { policy.onTerminal(term("failed", RATE), { isError: true, latest: undefined }); } catch { /* stopped */ }
      throw new Error("late");
    };
    await assert.rejects(pass(late, settlement, rec, { timeoutMs: 30 }));
    assert.equal(rec.requests.length, 1);
    assert.equal(settlement.stop, "timeout");
    assert.equal(settlement.terminalSeen, false, "a stopped pass records no terminal");
    assert.equal(settlement.policyErrorSet, false, "a stopped pass records no policy error");
  });

  it("a terminal arriving after an external abort is never recorded (stop = aborted)", async () => {
    const settlement = fresh(); const rec = newRec();
    const controller = new AbortController();
    setTimeout(() => controller.abort(), 20);
    const late: Script = async (request, policy) => {
      await waitAbort(request);
      try { policy.onTerminal(term("failed", RATE), { isError: true, latest: undefined }); } catch { /* stopped */ }
      throw new Error("late");
    };
    await assert.rejects(pass(late, settlement, rec, { signal: controller.signal }));
    assert.equal(settlement.stop, "aborted");
    assert.equal(settlement.terminalSeen, false);
    assert.equal(settlement.policyErrorSet, false);
  });

  it("a harness rejection that is not the policy error is recorded as a rejection without a policy error", async () => {
    const settlement = fresh(); const rec = newRec();
    await assert.rejects(pass(async () => { throw new Error("exploded"); }, settlement, rec));
    assert.equal(settlement.rejectionSet, true); assert.equal(settlement.policyErrorSet, false);
    assert.equal(settlement.escapedSet, true);
  });
});

describe("other advice lanes are unchanged on a gateway 429 (no settlement)", () => {
  const binding = codexBinding() as ClaimCodexProviderBinding;
  for (const label of ["summary", "review", "coverage"]) {
    it(`${label}: a provider 429 terminal is still the generic labeled error, never a LimitReachedError`, async () => {
      const authority = createCodexProviderExecutionAuthority(binding);
      const factory = scriptedFactory(failScript(RATE));
      await assert.rejects(
        runReadOnlyModelPass({
          provider: authority.providerBinding as ClaimProviderBinding, model: JUDGE_MODEL, systemPrompt: "s", prompt: "p",
          homeRoot: PASS_HOME_ROOT, homePrefix: `uzi-${label}-`, label, timeoutMs: 5000, queryFn: forbiddenQuery,
          denyReason: "read-only", log: nullLogger(),
          codex: { runId: "r1", binding: authority, buildHarness: factory },
        }),
        (err: unknown) => err instanceof Error && err.name === "Error" && err.message === `${label} model call returned an error result`,
      );
    });
  }
});
