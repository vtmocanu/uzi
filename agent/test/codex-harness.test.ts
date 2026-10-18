import { describe, it } from "node:test";
import { RunRunner } from "../src/runner.js";
import { FakeApi } from "./fake-api.js";
import { makeFixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import type { Executor } from "../src/executor.js";
import { PassThrough } from "node:stream";
import { createCodexTransport } from "../src/codex/transport.js";
import assert from "node:assert/strict";

import {
  CodexHarness,
  CodexHarnessError,
  CodexResumeError,
  type CodexLaunchRootResult,
  type CodexProviderConfig,
  type LaunchRootSeam,
  type SessionInspectSeam,
} from "../src/codex/codex-harness.js";
import { ExecutionRegistry, newLocalExecutionEpoch, type RegisteredRoot } from "../src/codex/registry.js";
import {
  CodexCallbackBroker,
  type CallbackOrigin,
  type CallbackResult,
  type CallbackRuntimeId,
} from "../src/codex/broker.js";
import { CodexUsageAccountant } from "../src/codex/token-accounting.js";
import { renderCodexRun } from "../src/codex/render.js";
import { CodexTurnFailedError } from "../src/codex/terminal-normalize.js";
import { classifyLimitEvidence, LimitReachedError } from "../src/limit.js";
import { evidencesModelProcessing, type HarnessEvent, type HarnessItem, type HarnessTerminal, type RunTurnRequest } from "../src/harness.js";
import type { CodexNotification, CodexTransport, CodexUsageBreakdown } from "../src/codex/transport.js";
import type { Logger } from "../src/log.js";
import {
  createCodexAppServerAuth,
  type CodexAppServerAuthMode,
  type CodexAppServerAuthSession,
} from "../src/codex/appserver-auth.js";

// PRD #1171 (M3, milestone 3) — the Codex run-harness core, driven with an in-memory
// transport and scripted app-server frames (NO real Codex process). Every external
// dependency is an injected seam; the ExecutionRegistry is the REAL one (pure logic) so
// launch reservation / callback admission are exercised end to end.

const WORKSPACE = "/work/repo";

const provider: CodexProviderConfig = {
  name: "openai",
  baseUrl: "http://127.0.0.1:9/v1",
  envKey: "OPENAI_API_KEY",
  model: "gpt-6-astra",
};

const noopLog: Logger = {
  debug() {},
  info() {},
  warn() {},
  error() {},
  addSecret() {},
  removeSecret() {},
  child() {
    return noopLog;
  },
};

const fakeRoot: RegisteredRoot = {
  kind: "provider",
  reap: async () => ({ ok: true }),
  dispose: async () => {},
};

function rec(v: unknown): Record<string, unknown> {
  return (v ?? {}) as Record<string, unknown>;
}

function defaultResponder(method: string): unknown {
  if (method === "thread/start") return { thread: { id: "th-1" } };
  if (method === "thread/resume") return { thread: { id: "resumed-1" } };
  if (method === "turn/start") return { turn: { id: "tn-1" } };
  return {};
}

function authResponder(method: string, params: unknown): unknown {
  if (method === "initialize") {
    return { userAgent: "codex/0.153.2", codexHome: "/owned/codex", platformFamily: "unix", platformOs: "linux" };
  }
  if (method === "account/login/start") return { type: rec(params).type };
  return defaultResponder(method);
}

/** A scriptable in-memory {@link CodexTransport}: request/respond/notify calls are
 *  captured, requests answer from an injected responder, and `notifications()` drains a
 *  queue the test pre-loads via {@link FakeTransport.push} / {@link FakeTransport.end}. */
class FakeTransport implements CodexTransport {
  requests: { method: string; params: unknown }[] = [];
  responses: { requestId: number | string; response: unknown }[] = [];
  respondAttempts = 0;
  notifies: { method: string; params: unknown }[] = [];
  closes = 0;

  private readonly queue: CodexNotification[] = [];
  private ended = false;
  private failure: Error | undefined;
  private waiter: ((r: IteratorResult<CodexNotification>) => void) | undefined;
  private failWaiter: ((error: Error) => void) | undefined;
  private consumed = false;
  private closedFlag = false;
  private interceptor?: (note: CodexNotification, frameBytes: number) => boolean;
  onRespond?: (requestId: number | string, response: unknown) => void;
  onClose?: () => void;

  constructor(private readonly responder: (method: string, params: unknown) => unknown = defaultResponder) {}

  push(note: CodexNotification): this {
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = undefined;
      w({ value: note, done: false });
    } else {
      this.queue.push(note);
    }
    return this;
  }

  emitServerRequest(note: CodexNotification): boolean {
    const requestId = note.kind === "activity" ? note.requestId : undefined;
    const frameBytes = Buffer.byteLength(JSON.stringify({ id: requestId, method: note.method, params: note.params }), "utf8");
    if (this.interceptor?.(note, frameBytes)) return true;
    this.push(note);
    return false;
  }

  /** Make the notification iterator's pending (or next) `next()` REJECT with `error`, as a
   *  transport read failure would. */
  fail(error: Error): this {
    this.failure = error;
    if (this.failWaiter) {
      const f = this.failWaiter;
      this.waiter = undefined;
      this.failWaiter = undefined;
      f(error);
    }
    return this;
  }

  end(): this {
    this.ended = true;
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = undefined;
      w({ value: undefined, done: true });
    }
    return this;
  }

  request<T = unknown>(method: string, params?: unknown): Promise<T> {
    this.requests.push({ method, params });
    return Promise.resolve(this.responder(method, params) as T);
  }

  notify(method: string, params?: unknown): void {
    this.notifies.push({ method, params });
  }

  respond(
    requestId: number | string,
    response: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } },
  ): void {
    this.respondAttempts += 1;
    // Mirror the real transport's closed-guard: a respond after close throws, so a broker
    // reply that settles after the turn is torn down exercises routeToolCall's guard.
    if (this.closedFlag) throw new Error("codex transport is closed");
    this.responses.push({ requestId, response });
    this.onRespond?.(requestId, response);
  }

  installServerRequestInterceptor(interceptor: (note: CodexNotification, frameBytes: number) => boolean): () => void {
    if (this.interceptor !== undefined) throw new Error("interceptor already installed");
    this.interceptor = interceptor;
    return () => {
      if (this.interceptor === interceptor) this.interceptor = undefined;
    };
  }

  notifications(): AsyncIterableIterator<CodexNotification> {
    if (this.consumed) throw new Error("notifications() is single-consumer");
    this.consumed = true;
    const next = (): Promise<IteratorResult<CodexNotification>> =>
      new Promise((resolve, reject) => {
        const q = this.queue.shift();
        if (q !== undefined) {
          resolve({ value: q, done: false });
          return;
        }
        if (this.failure !== undefined) {
          reject(this.failure);
          return;
        }
        if (this.ended) {
          resolve({ value: undefined, done: true });
          return;
        }
        this.waiter = resolve;
        this.failWaiter = reject;
      });
    return {
      next,
      return: () => Promise.resolve({ value: undefined, done: true }),
      [Symbol.asyncIterator]() {
        return this;
      },
    };
  }

  close(): Promise<void> {
    this.closes += 1;
    this.closedFlag = true;
    this.onClose?.();
    return Promise.resolve();
  }
}

// --- notification builders (the app-server wire vocabulary, per the M0 fixtures) -----

function threadStarted(threadId = "th-1"): CodexNotification {
  return { kind: "thread_started", method: "thread/started", threadId, params: { thread: { id: threadId } } };
}

function turnStarted(threadId = "th-1", turnId = "tn-1"): CodexNotification {
  return { kind: "turn_started", method: "turn/started", threadId, turnId, params: { threadId, turn: { id: turnId } } };
}

function turnCompleted(status = "completed", usage?: unknown, threadId = "th-1", turnId = "tn-1"): CodexNotification {
  const turn: Record<string, unknown> = { id: turnId, status };
  if (usage !== undefined) turn.usage = usage;
  return { kind: "turn_completed", method: "turn/completed", threadId, turnId, status, params: { threadId, turn } };
}

function agentMessage(text: string, usage?: unknown, threadId = "th-1"): CodexNotification {
  const item: Record<string, unknown> = { type: "agentMessage", text };
  if (usage !== undefined) item.usage = usage;
  return { kind: "activity", method: "item/completed", params: { threadId, item } };
}

/** PRD #1534: a decoded provider `ErrorNotification` (method "error"), bound to a turn. The
 *  FakeTransport pushes decoded notes directly, so this builds the typed shape the transport
 *  would hand the harness. `params.error` mirrors the raw provider payload (codexErrorInfo +
 *  any extra raw fields), so the harness reads codexErrorInfo through the same path as prod. */
function codexError(
  codexErrorInfo: unknown,
  opts: { willRetry?: boolean; extraError?: Record<string, unknown>; threadId?: string; turnId?: string } = {},
): CodexNotification {
  const willRetry = opts.willRetry ?? false;
  const threadId = opts.threadId ?? "th-1";
  const turnId = opts.turnId ?? "tn-1";
  const error: Record<string, unknown> = { codexErrorInfo, ...opts.extraError };
  return { kind: "codex_error", method: "error", threadId, turnId, willRetry, params: { error, willRetry, threadId, turnId } };
}

/** A failed `turn/completed` carrying a terminal `turn.error.codexErrorInfo` FALLBACK — the
 *  base `turnCompleted` builder never sets `turn.error`, so this hand-builds the note. */
function turnCompletedWithError(codexErrorInfo: unknown, threadId = "th-1", turnId = "tn-1"): CodexNotification {
  return {
    kind: "turn_completed",
    method: "turn/completed",
    threadId,
    turnId,
    status: "failed",
    params: { threadId, turn: { id: turnId, status: "failed", error: { codexErrorInfo } } },
  };
}

function delta(): CodexNotification {
  return { kind: "activity", method: "item/agent_message_delta", params: { delta: "h" } };
}

function bd(overrides: Partial<CodexUsageBreakdown> = {}): CodexUsageBreakdown {
  return {
    inputTokens: 0,
    cachedInputTokens: 0,
    cacheWriteInputTokens: 0,
    outputTokens: 0,
    reasoningOutputTokens: 0,
    totalTokens: 0,
    ...overrides,
  };
}

/** A decoded `thread/tokenUsage/updated` notification (PRD #1332 C4a), as the transport would
 *  hand it to the harness. The FakeTransport pushes decoded notes directly, so this builds the
 *  typed shape rather than a raw wire frame. */
function tokenUsage(
  total: Partial<CodexUsageBreakdown>,
  last: Partial<CodexUsageBreakdown>,
  threadId = "th-1",
  turnId = "tn-1",
): Extract<CodexNotification, { kind: "token_usage_updated" }> {
  return {
    kind: "token_usage_updated",
    method: "thread/tokenUsage/updated",
    threadId,
    turnId,
    usage: { total: bd(total), last: bd(last) },
    params: { threadId, turnId, tokenUsage: { total: bd(total), last: bd(last) } },
  };
}

function toolCall(
  requestId: number,
  tool: string,
  args: unknown,
  threadId = "th-1",
  turnId = "tn-1",
  callId = "c1",
): CodexNotification {
  return { kind: "activity", method: "item/tool/call", requestId, params: { threadId, turnId, callId, tool, arguments: args, namespace: null } };
}

function serverRequest(requestId: number, method = "unknown/request"): CodexNotification {
  return { kind: "activity", method, requestId, params: { value: true } };
}

/** An item/tool/call whose params OMIT threadId entirely (an absent/non-string id can never
 *  match the active root turn, so it must be denied fail-closed and never reach the broker). */
function toolCallNoThread(requestId: number, tool: string, args: unknown, turnId = "tn-1", callId = "c1"): CodexNotification {
  return { kind: "activity", method: "item/tool/call", requestId, params: { turnId, callId, tool, arguments: args, namespace: null } };
}

function completedItem(item: Record<string, unknown>, threadId = "th-1"): CodexNotification {
  return { kind: "activity", method: "item/completed", params: { threadId, item } };
}


function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

describe("m1 response usage", () => {
  const records = (events: HarnessEvent[]) => events.filter((e): e is Extract<HarnessEvent, { kind: "usage_record" }> => e.kind === "usage_record");
  const last = { inputTokens: 100, cachedInputTokens: 30, cacheWriteInputTokens: 20, outputTokens: 40, totalTokens: 140 };
  const doubled = { inputTokens: 200, cachedInputTokens: 60, cacheWriteInputTokens: 40, outputTokens: 80, totalTokens: 280 };

  it("measures RPC-inclusive duration with sequential and resumed dispatch resets", async () => {
    for (const resumed of [false, true]) {
      let now = 1000;
      let turn = 0;
      const transport = new FakeTransport(method => {
        if (method === "turn/start") { now += 17.8; return { turn: { id: `tn-${++turn}` } }; }
        return defaultResponder(method);
      });
      const { harness } = makeHarness({ transport, nowMs: () => now });
      const root = resumed ? "resumed-1" : "th-1";
      for (let i = 1; i <= 2; i++) {
        now = i * 1000;
        transport.push(turnCompleted("completed", undefined, root, `tn-${i}`));
        const events = await collect(harness.startTurn(makeRequest(resumed ? { resumeSessionId: root } : {})).events);
        const terminal = events.find(e => e.kind === "turn_finished");
        assert.ok(terminal?.kind === "turn_finished");
        assert.equal(terminal.terminal.metrics.durationMs, 17);
        assert.deepEqual(terminal.terminal.metrics.wire, { duration_ms: 17 });
      }
    }
  });

  it("persists two identical root buckets with distinct adoption IDs without assistant text or lifecycle evidence", async () => {
    const { harness, transport } = makeHarness();
    transport.push(tokenUsage(last, last)).push(tokenUsage(last, last))
      .push(tokenUsage({}, last)).push(tokenUsage(last, last, "unknown"))
      .push(tokenUsage(doubled, last)).push(turnCompleted("completed"));
    const events = await collect(harness.startTurn(makeRequest()).events);
    const adopted = records(events);
    assert.equal(adopted.length, 2);
    assert.notEqual(adopted[0]!.responseId, adopted[1]!.responseId);
    const { RunTurnReducerImpl } = await import("../src/harness-reducer.js");
    let requests = 0;
    const reducer = new RunTurnReducerImpl({ request() { requests++; }, async get() { return undefined; } });
    reducer.beginTurn();
    for (const record of adopted) {
      assert.match(record.responseId, /^[0-9a-f-]{36}$/);
      assert.equal(evidencesModelProcessing(record), false);
      assert.deepEqual(record.usage, { input_tokens: 50, cache_read_input_tokens: 30, cache_creation_input_tokens: 20, output_tokens: 40 });
      const reduction = await reducer.accept(record);
      assert.deepEqual(reduction.messages, [{
        kind: "status", agent: "lead",
        payload: { event: "codex_response_usage", usage_response_id: record.responseId, usage: record.usage, model: provider.model },
      }]);
      assert.deepEqual((await reducer.accept(record)).messages, reduction.messages, "replay preserves worker identity");
    }
    assert.equal(requests, 0);
    const completion = await reducer.finish({ kind: "exhausted" });
    assert.equal(completion.result.sawModelActivity, undefined);
    assert.equal(completion.result.subagentActivity, undefined);
  });

  it("omits incomplete and impossible splits, accepts equality, and retains aggregate accounting", async () => {
    const accountant = new CodexUsageAccountant();
    const { harness, transport } = makeHarness({ accountant });
    const complete = tokenUsage(last, last);
    const incomplete = { ...complete, usage: { ...complete.usage, pricingEvidenceComplete: false } };
    const impossible = { inputTokens: 100, cachedInputTokens: 80, cacheWriteInputTokens: 30, outputTokens: 40, totalTokens: 140 };
    const equality = { inputTokens: 100, cachedInputTokens: 70, cacheWriteInputTokens: 30, outputTokens: 40, totalTokens: 140 };
    // doubled + equality keeps cumulative splits consistent and uncached input nondecreasing at 100.
    transport.push(incomplete).push(tokenUsage(doubled, impossible))
      .push(tokenUsage({ inputTokens: 300, cachedInputTokens: 130, cacheWriteInputTokens: 70, outputTokens: 120, totalTokens: 420 }, equality))
      .push(turnCompleted("completed"));
    const events = await collect(harness.startTurn(makeRequest()).events);
    assert.deepEqual(records(events).map(e => e.usage), [{ input_tokens: 0, cache_read_input_tokens: 70, cache_creation_input_tokens: 30, output_tokens: 40 }]);
    const terminal = events.find(e => e.kind === "turn_finished");
    assert.ok(terminal?.kind === "turn_finished");
    assert.deepEqual(terminal.terminal.usage?.wire?.modelUsage, { [provider.model]: {
      inputTokens: 100, outputTokens: 120, cacheReadInputTokens: 130, cacheCreationInputTokens: 70, reasoningOutputTokens: 0, costStatus: "unreported",
    } });
    assert.equal(accountant.usageIncomplete, false);
  });

  it("proved historical replay and unproved resume emit none; current resumed response emits once", async () => {
    for (const proved of [false, true]) {
      const { harness, transport } = makeHarness();
      if (proved) transport.push(tokenUsage(last, last, "resumed-1", "old"));
      transport.push(turnStarted("resumed-1")).push(tokenUsage(doubled, last, "resumed-1"))
        .push(tokenUsage(doubled, last, "resumed-1")).push(tokenUsage(last, last, "resumed-1", "old"))
        .push(turnCompleted("completed", undefined, "resumed-1"));
      const events = await collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events);
      assert.equal(records(events).length, proved ? 1 : 0);
    }
  });

  for (const bindAfter of [false, true]) {
    it(`attributes child usage with bind ${bindAfter ? "after" : "before"} adoption, display cap and normal close; drains before first terminal`, async () => {
      let h!: CodexHarness;
      let bind!: () => void;
      let release!: () => void;
      const gate = new Promise<void>(r => { release = r; });
      const broker = stubBroker(async (rt, name) => {
        if (name !== "spawn_agent") return { ok: true, output: {} };
        h.recordChildThreadModel("child", "gpt-6-sol");
        h.registerChildSink("child", { push(note) {
          if (note.kind === "token_usage_updated") {
            if (bindAfter) bind();
            h.unregisterChildSink("child");
            release();
          }
        } });
        bind = () => {
          h.bindChildDispatch("child", rt, "coder");
          h.emitChildFrame("child", [{ kind: "text", text: "lane" }, ...Array.from({ length: 2001 }, () => ({ kind: "text" as const, text: "cap" }))]);
        };
        if (!bindAfter) bind();
        await gate;
        return { ok: true, output: {} };
      });
      const { harness, transport } = makeHarness({ broker });
      h = harness;
      transport.push(toolCall(1, "spawn_agent", { subagent_type: "coder", description: "[usage lane]" }))
        .push(tokenUsage(last, last)).push(tokenUsage(doubled, last))
        .push(tokenUsage(last, last, "child")).push(tokenUsage(doubled, last, "child"))
        .push(turnCompleted("completed"));
      const events: HarnessEvent[] = [];
      for await (const event of h.startTurn(makeRequest()).events) {
        events.push(event);
        if (event.kind === "turn_finished") break; // CodexExecutor's consumer seam.
      }
      const adopted = records(events);
      assert.equal(adopted.length, 4);
      const child = adopted.filter(e => e.model === "gpt-6-sol");
      const display = events.find(e => e.kind === "frame" && e.origin.kind === "subagent");
      assert.ok(display?.kind === "frame");
      assert.equal(child.length, 2);
      for (const record of child) assert.deepEqual(record.attribution, display.attribution);
      assert.equal(events.at(-1)?.kind, "turn_finished");
    });
  }

  for (const failure of ["failed start", "rejected binding", "ambiguous binding", "cancel", "stream failure"] as const) {
    it(`discards unprovable child usage on ${failure} without leaking into the next turn`, async () => {
      let h!: CodexHarness;
      let turn = 0;
      let admitted = 0;
      let ready!: () => void;
      const entered = new Promise<void>(resolve => { ready = resolve; });
      let release!: () => void;
      const pending = new Promise<void>(resolve => { release = resolve; });
      const controller = new AbortController();
      const streamError = new Error("m1 stream failed");
      let parent!: CallbackRuntimeId;
      const transport = new FakeTransport(method => {
        if (method === "turn/start") return { turn: { id: `tn-${++turn}` } };
        return defaultResponder(method);
      });
      const broker = stubBroker(async rt => {
        parent = rt;
        h.recordChildThreadModel("child", "gpt-6-sol");
        h.registerChildSink("child", { push(note) {
          if (note.kind !== "token_usage_updated") return;
          if (turn === 2) {
            h.unregisterChildSink("child");
            return;
          }
          if (failure === "rejected binding") h.bindChildDispatch("child", { ...rt, callId: "foreign" }, "coder");
          if (failure === "ambiguous binding") h.bindChildDispatch("child", rt, "coder");
          if (failure === "cancel") controller.abort();
          else if (failure === "stream failure") transport.fail(streamError);
          else {
            h.unregisterChildSink("child");
            release();
          }
        } });
        if (turn === 2) {
          h.bindChildDispatch("child", rt, "coder");
          return { ok: true, output: {} };
        }
        if (++admitted === (failure === "ambiguous binding" ? 2 : 1)) ready();
        await pending;
        // Startup may settle after stream cancellation/failure. It cannot reopen the lane.
        h.bindChildDispatch("child", rt, "coder");
        return { ok: false, code: "startup_failed", message: "child did not start" };
      });
      h = makeHarness({ transport, broker }).harness;
      transport.push(toolCall(1, "spawn_agent", { subagent_type: "coder" }));
      if (failure === "ambiguous binding") transport.push(toolCall(2, "spawn_agent", { subagent_type: "coder" }));
      const events: HarnessEvent[] = [];
      const first = (async () => {
        for await (const event of h.startTurn(makeRequest({ signal: controller.signal })).events) events.push(event);
      })();
      await withTimeout(entered, 2000, "child callbacks admitted");
      transport.push(tokenUsage(last, last, "child"));
      if (failure !== "cancel" && failure !== "stream failure") transport.push(turnCompleted("completed"));
      if (failure === "stream failure") await assert.rejects(withTimeout(first, 2000, failure), error => error === streamError);
      else await withTimeout(first, 2000, failure);
      assert.deepEqual(records(events), [], "unprovable child usage never falls back to lead");
      if (failure === "cancel" || failure === "stream failure") {
        assert.ok(!events.some(e => e.kind === "turn_finished"));
        release();
        await tick();
        h.bindChildDispatch("child", parent, "coder");
        h.unregisterChildSink("child");
      }
      // A failed transport cannot serve another turn; the other paths reuse the same
      // harness/thread to prove abandoned measurements do not cross a turn boundary.
      if (failure === "stream failure") return;
      transport.push(toolCall(3, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-2", "next"))
        .push(tokenUsage(doubled, last, "child", "tn-2"))
        .push(turnCompleted("completed", undefined, "th-1", "tn-2"));
      const next = await collect(h.startTurn(makeRequest()).events);
      assert.equal(records(next).length, 1);
      assert.equal(records(next)[0]!.attribution.agent, "coder");
      assert.deepEqual(records(next)[0]!.usage, { input_tokens: 50, cache_read_input_tokens: 30, cache_creation_input_tokens: 20, output_tokens: 40 });
    });
  }

  it("callback-queued binding flushes adopted usage before a consumer stops at terminal", async () => {
    let h!: CodexHarness;
    let parent!: CallbackRuntimeId;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      parent = rt;
      h.recordChildThreadModel("child", "gpt-6-sol");
      h.registerChildSink("child", { push() {} });
      await new Promise<void>(resolve => setImmediate(resolve));
      h.bindChildDispatch("child", parent, "coder");
      h.unregisterChildSink("child");
      return { ok: true, output: {} };
    });
    const { harness, transport } = makeHarness({ broker }); h = harness;
    transport.push(toolCall(1, "spawn_agent", { subagent_type: "coder" }))
      .push(tokenUsage(last, last, "child")).push(turnCompleted("completed"));
    const events: HarnessEvent[] = [];
    for await (const event of h.startTurn(makeRequest()).events) { events.push(event); if (event.kind === "turn_finished") break; }
    assert.equal(records(events).length, 1);
  });
});

// --- request + harness builders -----------------------------------------------------

function makeRequest(overrides: Partial<RunTurnRequest> = {}): RunTurnRequest {
  return {
    prompt: "do the thing",
    systemPrompt: "you are the lead",
    signal: new AbortController().signal,
    model: "gpt-6-astra",
    phase: "implement",
    agents: {},
    leadSkills: [],
    ...overrides,
  };
}

function stubBroker(
  handle: (rt: CallbackRuntimeId, name: unknown, args: unknown, origin: CallbackOrigin) => Promise<CallbackResult> = async () => ({
    ok: true,
    output: {},
  }),
): CodexCallbackBroker {
  return { handleToolCall: handle } as unknown as CodexCallbackBroker;
}

interface HarnessBits {
  harness: CodexHarness;
  transport: FakeTransport;
  registry: ExecutionRegistry;
}

function makeHarness(
  opts: {
    transport?: FakeTransport;
    registry?: ExecutionRegistry;
    broker?: CodexCallbackBroker;
    launchRoot?: LaunchRootSeam;
    sessionInspect?: SessionInspectSeam;
    appServerAuth?: CodexAppServerAuthSession;
    credentialValue?: string;
    authMode?: CodexAppServerAuthMode;
    accountant?: CodexUsageAccountant;
    onTokenUsageIncomplete?: (text: string) => void | Promise<void>;
    scrubProjected?: (s: string) => string;
    idNonce?: string;
    nowMs?: () => number;
    log?: Logger;
  } = {},
): HarnessBits {
  const transport = opts.transport ?? new FakeTransport();
  const registry = opts.registry ?? new ExecutionRegistry(newLocalExecutionEpoch(1));
  const launchRoot: LaunchRootSeam =
    opts.launchRoot ?? (async (): Promise<CodexLaunchRootResult> => ({ root: fakeRoot, transport, supervisorPid: 4321 }));
  const harness = new CodexHarness({
    registry,
    launchRoot,
    broker: opts.broker ?? stubBroker(),
    provider,
    workspace: WORKSPACE,
    homeDir: "/work/.codex-state",
    log: opts.log ?? noopLog,
    sessionInspect: opts.sessionInspect ?? (async () => "unknown"),
    appServerAuth: opts.appServerAuth,
    credentialValue: opts.credentialValue,
    authMode: opts.authMode,
    accountant: opts.accountant,
    onTokenUsageIncomplete: opts.onTokenUsageIncomplete,
    scrubProjected: opts.scrubProjected,
    idNonce: opts.idNonce,
    nowMs: opts.nowMs,
  });
  return { harness, transport, registry };
}

async function collect(events: AsyncIterable<HarnessEvent>): Promise<HarnessEvent[]> {
  const out: HarnessEvent[] = [];
  for await (const ev of events) out.push(ev);
  return out;
}

function tick(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}

/** Reject if `p` has not settled within `ms`, so a wedged (never-ending) stream fails the
 *  test with a bounded timeout instead of hanging the run. Clears its timer on settle. */
async function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      p,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`timed out waiting for ${label}`)), ms);
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

/** A broker whose handleToolCall NEVER settles on its own: `entered` resolves the moment a
 *  callback is dispatched to it, and `release` settles the pending callback on demand. This
 *  models a model-selected long-running seam (a shell that never returns) so a test can prove
 *  the turn stream still ends on the owner abort / requestStop rather than wedging. */
function wedgeBroker(): { broker: CodexCallbackBroker; entered: Promise<void>; release: (r: CallbackResult) => void } {
  let resolveEntered!: () => void;
  const entered = new Promise<void>((r) => {
    resolveEntered = r;
  });
  let release!: (r: CallbackResult) => void;
  const result = new Promise<CallbackResult>((r) => {
    release = r;
  });
  const broker = stubBroker(() => {
    resolveEntered();
    return result;
  });
  return { broker, entered, release };
}

describe("#2878 M2 harness terminal", () => {
  const entry = (inputTokens: number, outputTokens: number) => ({
    inputTokens, outputTokens, cacheReadInputTokens: 0, cacheCreationInputTokens: 0,
    reasoningOutputTokens: 0, costStatus: "unreported",
  });
  const note = (inputTokens: number, outputTokens: number, threadId = "resumed-1", turnId = "tn-1") =>
    tokenUsage({ inputTokens, outputTokens, totalTokens: inputTokens + outputTokens },
      { inputTokens, outputTokens, totalTokens: inputTokens + outputTokens }, threadId, turnId);

  for (const sameModel of [false, true]) {
    it(`regression retains earlier child in terminal after root incomplete: ${sameModel ? "same model" : "distinct model"}`, async () => {
      const { harness, transport } = makeHarness({ authMode: "api_key" });
      const childModel = sameModel ? "gpt-6-astra" : "gpt-6.1-sol";
      harness.recordChildThreadModel("child", childModel);
      harness.registerChildSink("child", { push() {} });
      transport.push(note(200, 100, "child", "child-turn"))
        .push(turnStarted("resumed-1"))
        .push(note(600, 180))
        .push(turnCompleted("completed", undefined, "resumed-1")).end();
      try {
        const events = await collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events);
        const result = events.find(e => e.kind === "turn_finished");
        assert.ok(result?.kind === "turn_finished");
        assert.deepEqual(result.terminal.usage?.wire?.modelUsage, sameModel
          ? { "gpt-6-astra": entry(200, 100) }
          : { "gpt-6-astra": entry(0, 0), [childModel]: {
            ...entry(200, 100), costStatus: "metered", costUSD: 0.0014,
          } }, "terminal retains proved child work even with a zero incomplete root");
        assert.deepEqual(result.terminal.metrics?.cost, { kind: "unreported" });
      } finally { await harness.close(); }
    });
  }

  for (const invalid of ["raw bucket", "unknown identity", "missing turn", "activity"] as const) {
    it(`regression baselined resumed root retains pre/post counters after invalid live ${invalid}`, async () => {
      const accountant = new CodexUsageAccountant();
      let notices = 0;
      const { harness, transport } = makeHarness({ authMode: "api_key", accountant,
        onTokenUsageIncomplete: () => { notices++; } });
      let bad = note(9000, 900);
      if (invalid === "raw bucket") {
        bad = { ...bad, params: { threadId: "resumed-1", turnId: "tn-1", tokenUsage: {
          total: { ...bd(), inputTokens: "invalid" }, last: bd(),
        } } };
      } else if (invalid === "unknown identity") {
        bad = { ...bad, threadId: "", params: { ...bad.params as object, threadId: "" } };
      } else if (invalid === "missing turn") {
        bad = { ...bad, turnId: "", params: { ...bad.params as object, turnId: "" } };
      }
      const invalidNote: CodexNotification = invalid === "activity"
        ? { kind: "activity", method: bad.method, params: { threadId: "resumed-1", turnId: "tn-1", tokenUsage: {} } }
        : bad;
      transport.push(note(1000, 200, "resumed-1", "historical"))
        .push(turnStarted("resumed-1"))
        .push(tokenUsage({ inputTokens: 1100, outputTokens: 220, totalTokens: 1320 },
          { inputTokens: 100, outputTokens: 20, totalTokens: 120 }, "resumed-1"))
        .push(invalidNote)
        .push(tokenUsage({ inputTokens: 1130, outputTokens: 230, totalTokens: 1360 },
          { inputTokens: 30, outputTokens: 10, totalTokens: 40 }, "resumed-1"))
        .push(turnCompleted("completed", undefined, "resumed-1")).end();
      try {
        const events = await collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events);
        const result = events.find(e => e.kind === "turn_finished");
        assert.ok(result?.kind === "turn_finished");
        assert.equal(notices, 1);
        assert.deepEqual(result.terminal.usage?.wire?.modelUsage, { "gpt-6-astra": entry(130, 30) },
          "retain validated strictly-newer usage on both sides of the latch");
      } finally { await harness.close(); }
    });
  }

  for (const invalid of ["raw bucket", "activity", "unknown identity", "missing turn"] as const) {
    it(`regression invalid ${invalid} live note before baseline quarantines later replay`, async () => {
      const accountant = new CodexUsageAccountant();
      const notices: string[] = [];
      const { harness, transport } = makeHarness({ authMode: "api_key", accountant,
        onTokenUsageIncomplete: text => { notices.push(text); } });
      const bad = note(9000, 900);
      const invalidNote: CodexNotification = invalid === "activity"
        ? { kind: "activity", method: bad.method,
          params: { threadId: "resumed-1", turnId: "tn-1", tokenUsage: {} } }
        : invalid === "raw bucket"
          ? { ...bad, params: { threadId: "resumed-1", turnId: "tn-1",
            tokenUsage: { total: { ...bd(), inputTokens: "invalid" }, last: bd() } } }
          : invalid === "unknown identity"
            ? { ...bad, threadId: "", params: { ...bad.params as object, threadId: "" } }
            : { ...bad, turnId: "", params: { ...bad.params as object, turnId: "" } };
      transport.push(invalidNote)
        .push(note(1000, 200, "resumed-1", "historical"))
        .push(turnStarted("resumed-1"))
        .push(tokenUsage({ inputTokens: 1100, outputTokens: 220, totalTokens: 1320 },
          { inputTokens: 100, outputTokens: 20, totalTokens: 120 }, "resumed-1"))
        .push(turnCompleted("completed", { inputTokens: 9999 }, "resumed-1")).end();
      try {
        const events = await collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events);
        const result = events.find(e => e.kind === "turn_finished");
        assert.ok(result?.kind === "turn_finished");
        assert.equal(accountant.hasBaseline("resumed-1"), false, "late history cannot erase attempted live work");
        assert.deepEqual(result.terminal.usage?.wire?.modelUsage, { "gpt-6-astra": entry(0, 0) });
        assert.deepEqual(result.terminal.metrics?.cost, { kind: "unreported" });
        assert.equal(events.filter(e => e.kind === "usage_record").length, 0);
        assert.equal(notices.length, 1);
        assert.match(notices[0]!, invalid === "raw bucket" || invalid === "activity"
          ? /invalid raw token usage/ : /invalid token usage identity/);
        assert.match(notices[0]!, /baseline is unavailable/);
      } finally { await harness.close(); }
    });
  }

  for (const historicalOnly of [true, false]) {
    it(`regression ${historicalOnly ? "recoverable historical malformation" : "live missing baseline quarantine"} terminal`, async () => {
      const { harness, transport } = makeHarness({ authMode: "api_key" });
      if (historicalOnly) transport.push({ kind: "activity", method: "thread/tokenUsage/updated",
        params: { threadId: "resumed-1", turnId: "historical", tokenUsage: {} } });
      else transport.push(turnStarted("resumed-1")).push(note(600, 180));
      transport.push(note(1000, 200, "resumed-1", "historical")).push(turnStarted("resumed-1"))
        .push(tokenUsage({ inputTokens: 1100, outputTokens: 220, totalTokens: 1320 },
          { inputTokens: 100, outputTokens: 20, totalTokens: 120 }, "resumed-1"))
        .push(turnCompleted("completed", undefined, "resumed-1")).end();
      try {
        const events = await collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events);
        const result = events.find(e => e.kind === "turn_finished");
        assert.ok(result?.kind === "turn_finished");
        assert.deepEqual(result.terminal.usage?.wire?.modelUsage,
          { "gpt-6-astra": historicalOnly ? entry(100, 20) : entry(0, 0) });
      } finally { await harness.close(); }
    });
  }

  for (const shape of ["equal", "reset", "advancing regression"] as const) {
    it(`${shape === "advancing regression" ? "regression" : "compatibility"} real transport active ${shape}`, async () => {
      // Decode NDJSON through the production transport, including deliberate split frames.
      // The pinned resume fixtures prove replay ordering, not a reset/compaction incident cause.
      const inbound = new PassThrough();
      const outbound = new PassThrough();
      const transport = createCodexTransport({ inbound, outbound });
      const accountant = new CodexUsageAccountant();
      const { harness } = makeHarness({ authMode: "api_key", accountant,
        launchRoot: async () => ({ root: fakeRoot, transport, supervisorPid: 4321 }) });
      let buffered = "";
      outbound.setEncoding("utf8");
      outbound.on("data", (chunk: string) => {
        buffered += chunk;
        let end: number;
        while ((end = buffered.indexOf("\n")) !== -1) {
          const frame = rec(JSON.parse(buffered.slice(0, end)));
          buffered = buffered.slice(end + 1);
          if (frame.id === undefined || typeof frame.method !== "string") continue;
          inbound.write(JSON.stringify({ id: frame.id, result: defaultResponder(frame.method) }) + "\n");
          if (frame.method !== "turn/start") continue;
          const totals = shape === "equal" ? [1110, 210] : shape === "reset" ? [10, 2] : [1120, 210];
          const sequence = [
            note(1000, 200, "resumed-1", "historical"), turnStarted("resumed-1"),
            tokenUsage({ inputTokens: 1100, outputTokens: 220, totalTokens: 1320 },
              { inputTokens: 100, outputTokens: 20, totalTokens: 120 }, "resumed-1"),
            tokenUsage({ inputTokens: totals[0], outputTokens: totals[1], totalTokens: totals[0]! + totals[1]! },
              { inputTokens: 10, outputTokens: 2, totalTokens: 12 }, "resumed-1"),
            turnCompleted("completed", undefined, "resumed-1"),
          ];
          for (const n of sequence) {
            const line = JSON.stringify({ method: n.method, params: n.params }) + "\n";
            inbound.write(line.slice(0, 17)); inbound.write(line.slice(17));
          }
        }
      });
      try {
        const events = await withTimeout(collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events), 5000, shape);
        const result = events.find(e => e.kind === "turn_finished");
        assert.ok(result?.kind === "turn_finished");
        assert.equal(accountant.usageIncomplete, shape === "advancing regression");
        assert.deepEqual(result.terminal.usage?.wire?.modelUsage, { "gpt-6-astra":
          shape === "advancing regression" ? entry(100, 20)
            : { ...entry(100, 20), costStatus: "metered", costUSD: 0.002 } });
      } finally { await harness.close(); inbound.destroy(); outbound.destroy(); }
    });
  }
});

describe("CodexHarness: closed usage diagnostics", () => {
  const total = bd({ inputTokens: 100, cachedInputTokens: 20, cacheWriteInputTokens: 5,
    outputTokens: 20, reasoningOutputTokens: 5, totalTokens: 120 });
  const last = bd({ inputTokens: 40, cachedInputTokens: 5, cacheWriteInputTokens: 2,
    outputTokens: 10, reasoningOutputTokens: 2, totalTokens: 50 });
  const prefix = "Codex token accounting is incomplete: invalid raw token usage. The proved baseline is retained; proven usage remains available. Cost is unreported for the affected model. Execution continues.";
  type Note = Extract<CodexNotification, { kind: "token_usage_updated" }>;
  const fresh = () => tokenUsage(total, last, "resumed-1");
  const raw = (note: Note) => rec(rec(note.params).tokenUsage);
  const cases: { label: string; detail: string; mutate: (note: Note) => CodexNotification; real?: boolean }[] = [];
  for (const shape of [undefined, null, [], "provider-object-sentinel"]) {
    cases.push({ label: `params ${String(shape)}`, detail: "missing container",
      mutate: n => ({ ...n, params: shape }) });
    cases.push({ label: `container ${String(shape)}`, detail: "missing container",
      mutate: n => ({ ...n, params: { ...rec(n.params), tokenUsage: shape } }) });
  }
  for (const name of ["total", "last"] as const) {
    for (const shape of [undefined, null, [], "provider-object-sentinel"]) {
      cases.push({ label: `${name} breakdown ${String(shape)}`, detail: `missing breakdown (${name})`,
        mutate: n => { raw(n)[name] = shape; return n; } });
    }
    for (const key of ["inputTokens", "cachedInputTokens", "cacheWriteInputTokens",
      "outputTokens", "reasoningOutputTokens", "totalTokens"] as const) {
      cases.push({ label: `${name} missing ${key}`, detail: `missing bucket (${name}.${key})`,
        real: ["inputTokens", "cachedInputTokens", "cacheWriteInputTokens", "outputTokens"].includes(key),
        mutate: n => { delete rec(raw(n)[name])[key]; return n; } });
      for (const value of [undefined, null, "provider-value-sentinel", { secret: "provider-object-sentinel" }]) {
        cases.push({ label: `${name} non-number ${key} ${value === null ? "null" : typeof value}`, detail: `non-number (${name}.${key})`,
          real: value !== undefined && ["inputTokens", "cachedInputTokens", "cacheWriteInputTokens", "outputTokens"].includes(key),
          mutate: n => { rec(raw(n)[name])[key] = value; return n; } });
      }
      for (const value of [-87654321, Number.MAX_SAFE_INTEGER + 1, 1.5, NaN, Infinity]) {
        cases.push({ label: `${name} unsafe ${key} ${value}`, detail: `unsafe or negative (${name}.${key})`,
          mutate: n => { rec(raw(n)[name])[key] = value; return n; } });
      }
      cases.push({ label: `${name} mismatch ${key}`, detail: `raw-vs-decoded mismatch (${name}.${key})`,
        mutate: n => { rec(raw(n)[name])[key] = n.usage[name][key] + 1; return n; } });
    }
    for (const [key, value] of [["cachedInputTokens", 99], ["reasoningOutputTokens", 99], ["totalTokens", 999]] as const) {
      cases.push({ label: `${name} relationship ${key}`, detail: `bucket relationship (${name}.${key})`,
        mutate: n => {
          const changed = { ...n.usage[name], [key]: value };
          raw(n)[name] = changed;
          return { ...n, usage: { ...n.usage, [name]: changed } };
        } });
    }
  }
  const exceeds: [keyof CodexUsageBreakdown, Partial<CodexUsageBreakdown>][] = [
    ["inputTokens", { inputTokens: 101, totalTokens: 111 }],
    ["cachedInputTokens", { cachedInputTokens: 21 }],
    ["cacheWriteInputTokens", { cacheWriteInputTokens: 6 }],
    ["outputTokens", { outputTokens: 21, totalTokens: 61 }],
    ["reasoningOutputTokens", { reasoningOutputTokens: 6 }],
  ];
  for (const [key, overrides] of exceeds) {
    cases.push({ label: `last exceeds ${key}`, detail: `last > total (last.${key})`,
      mutate: n => {
        return tokenUsage(total, { ...last, ...overrides }, n.threadId, n.turnId);
      } });
  }
  cases.push({ label: "total precedes last and fixed bucket order", detail: "missing bucket (total.inputTokens)",
    mutate: n => {
      delete rec(raw(n).total).inputTokens;
      delete rec(raw(n).total).cachedInputTokens;
      raw(n).last = null;
      return n;
    } });
  cases.push({ label: "untyped skips raw usage inspection", detail: "untyped usage notification",
    mutate: n => ({ kind: "activity", method: n.method, params: {
      threadId: n.threadId, turnId: n.turnId,
      get tokenUsage() { throw new Error("untyped raw usage must not be inspected"); },
    } }) });
  cases.push({ label: "pricing fallback", detail: "pricing evidence incomplete",
    mutate: n => ({ ...n, usage: { ...n.usage, pricingEvidenceComplete: false } }) });
  cases.push({ label: "untyped", detail: "untyped usage notification",
    mutate: n => ({ kind: "activity", method: n.method,
      params: { ...rec(n.params), tokenUsage: { "provider-key-sentinel": "provider-value-sentinel" },
        id: "provider-id-sentinel" } }) });

  for (const c of cases) {
    it(c.label, async () => {
      const accountant = new CodexUsageAccountant();
      const notices: string[] = [];
      // Pricing false deliberately accompanies raw failures to prove diagnostic precedence.
      let bad = c.mutate(fresh());
      if (bad.kind === "token_usage_updated")
        bad = { ...bad, usage: { ...bad.usage, pricingEvidenceComplete: false } };
      const inbound = new PassThrough();
      const outbound = new PassThrough();
      const transport = createCodexTransport({ inbound, outbound });
      const bits = makeHarness({ accountant, onTokenUsageIncomplete: text => { notices.push(text); },
        ...(c.real ? { launchRoot: async () => ({ root: fakeRoot, transport, supervisorPid: 4321 }) } : {}) });
      const sequence = [
        tokenUsage(last, last, "resumed-1", "historical"),
        turnStarted("resumed-1"), bad, turnCompleted("completed", undefined, "resumed-1"),
      ];
      let buffer = "";
      outbound.setEncoding("utf8");
      outbound.on("data", (chunk: string) => {
        buffer += chunk;
        let end: number;
        while ((end = buffer.indexOf("\n")) !== -1) {
          const frame = rec(JSON.parse(buffer.slice(0, end)));
          buffer = buffer.slice(end + 1);
          if (typeof frame.method !== "string" || frame.id === undefined) continue;
          inbound.write(JSON.stringify({ id: frame.id, result: defaultResponder(frame.method) }) + "\n");
          if (frame.method === "turn/start") {
            // Finite ordered frames; a write failure stops this test's delivery.
            for (const n of sequence)
              inbound.write(JSON.stringify({ method: n.method, params: n.params }) + "\n");
          }
        }
      });
      try {
        if (!c.real) {
          for (const n of sequence) bits.transport.push(n);
          bits.transport.end();
        }
        const events = await withTimeout(collect(bits.harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events), 5000, c.label);
        assert.ok(events.some(e => e.kind === "turn_finished" && e.terminal.outcome === "success"));
        assert.equal(accountant.hasBaseline("resumed-1"), true);
        assert.deepEqual(notices, [prefix + " Diagnostic: " + c.detail + "."], "exact closed diagnostic");
        for (const sentinel of ["provider-value-sentinel", "provider-object-sentinel", "provider-key-sentinel",
          "provider-id-sentinel", "-87654321", "9007199254740992", "1.5"])
          assert.equal(notices[0]!.includes(sentinel), false);
      } finally {
        await bits.harness.close();
        transport.close();
        inbound.destroy(); outbound.destroy();
      }
    });
  }
  it("accepts explicit undefined pricing evidence", async () => {
    const accountant = new CodexUsageAccountant();
    const notices: string[] = [];
    const { harness, transport } = makeHarness({ accountant, onTokenUsageIncomplete: text => { notices.push(text); } });
    const accepted = fresh();
    const note = { ...accepted, usage: { ...accepted.usage, pricingEvidenceComplete: undefined } };
    transport.push(tokenUsage(last, last, "resumed-1", "historical"))
      .push(turnStarted("resumed-1")).push(note)
      .push(turnCompleted("completed", undefined, "resumed-1")).end();
    try {
      await collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events);
      assert.deepEqual(notices, []);
      assert.equal(accountant.usageIncomplete, false);
      assert.equal(accountant.aggregateByModel()?.["gpt-6-astra"]?.inputTokens, 42);
    } finally { await harness.close(); }
  });
});

// U1 resumed-claim accounting through completed turns, both credential modes.
describe("CodexHarness: resumed usage evidence", () => {
  const incompleteModels = (retained = false) => ({
    "gpt-6-astra": {
      inputTokens: retained ? 560 : 0, cacheReadInputTokens: retained ? 140 : 0,
      cacheCreationInputTokens: 0, outputTokens: retained ? 210 : 0,
      reasoningOutputTokens: retained ? 70 : 0, costStatus: "unreported",
    },
  });
  const assertIncompleteUsage = (result: HarnessTerminal, retained = false) => {
    assert.deepEqual(result.usage, {
      basis: "turn", tokens: {},
      wire: { usage: undefined, modelUsage: incompleteModels(retained) },
    }, "terminal retains the exact proved lower bound and suppresses ambiguous provider usage");
  };
  const historical = { inputTokens: 300, cachedInputTokens: 60, outputTokens: 90, reasoningOutputTokens: 30, totalTokens: 390 };
  const last = { inputTokens: 200, cachedInputTokens: 40, outputTokens: 60, reasoningOutputTokens: 20, totalTokens: 260 };
  const next = { inputTokens: 600, cachedInputTokens: 120, outputTokens: 180, reasoningOutputTokens: 60, totalTokens: 780 };
  const response = { inputTokens: 300, cachedInputTokens: 60, outputTokens: 90, reasoningOutputTokens: 30, totalTokens: 390 };
  const replay = () => tokenUsage(historical, last, "resumed-1", "historical");
  const terminal = (events: HarnessEvent[]) => {
    const event = events.find(e => e.kind === "turn_finished");
    assert.ok(event?.kind === "turn_finished");
    return event.terminal;
  };
  for (const authMode of ["api_key", "subscription"] as const) {
    it(`${authMode}: delayed replay after outbound turn/start is historical; child/stale boundaries cannot close replay`, async () => {
      const accountant = new CodexUsageAccountant();
      let notices = 0;
      const { harness, transport } = makeHarness({ authMode, accountant, onTokenUsageIncomplete: () => { notices++; } });
      const running = collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events);
      await tick();
      assert.ok(transport.requests.some(r => r.method === "turn/start"));
      transport.push(turnStarted("child", "tn-1")).push(turnStarted("resumed-1", "stale"));
      await tick(); // separate inbound delivery after the outbound RPC
      transport.push(replay()).push(replay());
      await tick();
      transport.push(turnStarted("resumed-1", "tn-1"))
        .push(tokenUsage(next, response, "resumed-1"))
        .push(replay()) // stale historical usage after boundary is ignored
        .push(turnCompleted("completed", { inputTokens: 9999 }, "resumed-1")).end();
      const result = terminal(await running);
      assert.equal(result.outcome, "success");
      assert.equal(notices, 0);
      assert.deepEqual(accountant.aggregateByModel({ authMode, now: new Date("2026-10-05") })?.["gpt-6-astra"], {
        inputTokens: 240, cacheReadInputTokens: 60, cacheCreationInputTokens: 0,
        outputTokens: 90, reasoningOutputTokens: 30,
        costStatus: "metered",
        costUSD: 0.00696,
      });
      await harness.close();
    });

    it(`${authMode}: real transport accepts delayed replay in split chunks after outbound start`, async () => {
      const inbound = new PassThrough();
      const outbound = new PassThrough();
      const transport = createCodexTransport({ inbound, outbound });
      const accountant = new CodexUsageAccountant();
      let notices = 0;
      const { harness } = makeHarness({
        authMode, accountant, onTokenUsageIncomplete: () => { notices++; },
        launchRoot: async () => ({ root: fakeRoot, transport, supervisorPid: 4321 }),
      });
      const childNotes: CodexNotification[] = [];
      harness.registerChildSink("child", { push: note => { childNotes.push(note); } });
      const unrelatedUsage = ["foreign", "child"].flatMap(threadId => [
        ...["", " \t "].map(turnId => {
          const note = tokenUsage(historical, last, threadId, turnId);
          return { method: note.method, params: note.params };
        }),
        ...[undefined, "", " \t "].map(turnId => ({
          method: "thread/tokenUsage/updated",
          params: { threadId, ...(turnId === undefined ? {} : { turnId }), tokenUsage: {} },
        })),
      ]);
      const write = (frame: unknown) => { inbound.write(JSON.stringify(frame) + "\n"); };
      let buffered = "";
      let delivery: Promise<void> | undefined;
      outbound.setEncoding("utf8");
      outbound.on("data", (chunk: string) => {
        buffered += chunk;
        let end: number;
        while ((end = buffered.indexOf("\n")) !== -1) {
          const frame = rec(JSON.parse(buffered.slice(0, end)));
          buffered = buffered.slice(end + 1);
          if (typeof frame.method !== "string" || frame.id === undefined) continue;
          write({ id: frame.id, result: defaultResponder(frame.method) });
          if (frame.method === "turn/start") delivery = (async () => {
            await tick();
            // Foreign lifecycle and usage cannot establish the resumed root baseline.
            const sequence = [
              turnStarted("foreign"), tokenUsage(historical, last, "foreign", "historical"),
              tokenUsage(historical, last, "child", "historical"),
              tokenUsage(historical, last, " resumed-1 ", "historical"),
              turnStarted("resumed-1", "stale"), replay(), replay(),
              ...unrelatedUsage,
              turnStarted("resumed-1"), tokenUsage(next, response, "resumed-1"),
              ...unrelatedUsage,
              tokenUsage(next, response, "resumed-1"), replay(),
              tokenUsage(historical, last, "foreign"), tokenUsage(historical, last, "child"),
              tokenUsage(historical, last, " resumed-1 "),
              tokenUsage(historical, last, "resumed-1", " tn-1 "),
              turnCompleted("completed", undefined, "resumed-1"),
            ];
            for (const note of sequence) {
              const line = JSON.stringify({ method: note.method, params: note.params }) + "\n";
              const split = Math.floor(line.length / 2);
              inbound.write(line.slice(0, split));
              await tick();
              inbound.write(line.slice(split));
            }
          })();
        }
      });
      try {
        const result = terminal(await withTimeout(collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events), 5000, "split replay"));
        await delivery;
        assert.equal(childNotes.filter(note => note.kind === "token_usage_updated").length, 6);
        assert.equal(childNotes.filter(note => note.kind === "activity").length, 6);
        assert.equal(result.outcome, "success");
        assert.equal(notices, 0);
        assert.deepEqual(accountant.aggregateByModel({ authMode, now: new Date("2026-10-05") })?.["gpt-6-astra"], {
          inputTokens: 240, cacheReadInputTokens: 60, cacheCreationInputTokens: 0,
          outputTokens: 90, reasoningOutputTokens: 30,
          costStatus: "metered",
          costUSD: 0.00696,
        });
      } finally {
        await delivery;
        await harness.close();
        inbound.destroy(); outbound.destroy();
      }
    });

    for (const position of ["before", "after"] as const) {
      for (const shape of ["typed", "activity"] as const) {
        for (const field of ["threadId", "turnId"] as const) {
          for (const identity of ["", " \t "] as const) {
            it(`${authMode}: real transport rejects ${shape} ${field} ${identity === "" ? "empty" : "whitespace"} identity ${position} boundary`, async () => {
              const inbound = new PassThrough();
              const outbound = new PassThrough();
              const transport = createCodexTransport({ inbound, outbound });
              const accountant = new CodexUsageAccountant();
              let notices = 0;
              const onTokenUsageIncomplete = () => { notices++; };
              const { harness } = makeHarness({
                authMode, accountant, onTokenUsageIncomplete,
                launchRoot: async () => ({ root: fakeRoot, transport, supervisorPid: 4321 }),
              });
              const finalTotal = { inputTokens: 1000, cachedInputTokens: 200, outputTokens: 300, reasoningOutputTokens: 100, totalTokens: 1300 };
              const finalResponse = { inputTokens: 400, cachedInputTokens: 80, outputTokens: 120, reasoningOutputTokens: 40, totalTokens: 520 };
              const malformed = {
                ...rec(tokenUsage(next, response, "resumed-1").params),
                [field]: identity,
                ...(shape === "activity" ? { tokenUsage: {} } : {}),
              };
              let buffered = "";
              let delivery: Promise<void> | undefined;
              const write = (frame: unknown) => { inbound.write(JSON.stringify(frame) + "\n"); };
              outbound.setEncoding("utf8");
              outbound.on("data", (chunk: string) => {
                buffered += chunk;
                let end: number;
                while ((end = buffered.indexOf("\n")) !== -1) {
                  const frame = rec(JSON.parse(buffered.slice(0, end)));
                  buffered = buffered.slice(end + 1);
                  if (typeof frame.method !== "string" || frame.id === undefined) continue;
                  write({ id: frame.id, result: defaultResponder(frame.method) });
                  if (frame.method === "turn/start") delivery = (async () => {
                    // Two replay pages arrive independently after the outbound start.
                    // Delivery is finite and ordered; a write failure rejects the sequence.
                    for (const note of [
                      tokenUsage(last, last, "resumed-1", "older"), replay(),
                    ]) {
                      await tick();
                      write({ method: note.method, params: note.params });
                    }
                    if (position === "before") write({ method: "thread/tokenUsage/updated", params: malformed });
                    const boundary = turnStarted("resumed-1");
                    write({ method: boundary.method, params: boundary.params });
                    if (position === "after") write({ method: "thread/tokenUsage/updated", params: malformed });
                    for (const note of [
                      tokenUsage(finalTotal, finalResponse, "resumed-1"),
                      turnCompleted("completed", { inputTokens: 9999 }, "resumed-1"),
                    ]) {
                      await tick();
                      write({ method: note.method, params: note.params });
                    }
                  })();
                }
              });
              try {
                const result = terminal(await withTimeout(
                  collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events),
                  5000, "malformed usage identity",
                ));
                await delivery;
                assert.equal(result.outcome, "success");
                assert.equal(notices, 1);
                assert.equal(accountant.usageIncomplete, true);
                assertIncompleteUsage(result, position === "after");
                assert.deepEqual(accountant.aggregateByModel(), incompleteModels(position === "after"));
                assert.deepEqual(result.metrics?.cost, { kind: "unreported" });

                const retry = makeHarness({ authMode, accountant, onTokenUsageIncomplete });
                try {
                  retry.transport.push(replay()).push(turnStarted("resumed-1"))
                    .push(tokenUsage(finalTotal, finalResponse, "resumed-1"))
                    .push(turnCompleted("completed", { inputTokens: 9999 }, "resumed-1")).end();
                  const retried = terminal(await collect(retry.harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events));
                  assert.equal(retried.outcome, "success");
                  assertIncompleteUsage(retried, position === "after");
                  assert.equal(notices, 1);
                } finally {
                  await retry.harness.close();
                }
              } finally {
                await delivery;
                await harness.close();
                inbound.destroy(); outbound.destroy();
              }
            });
          }
        }
      }
    }

    for (const hasReplay of [true, false]) {
      it(`${authMode}: metadata-only resume avoids an oversized history frame with ${hasReplay ? "replay" : "no replay"} and multiple new updates`, async () => {
        const inbound = new PassThrough();
        const outbound = new PassThrough();
        const transport = createCodexTransport({ inbound, outbound });
        const accountant = new CodexUsageAccountant();
        let notices = 0;
        const { harness } = makeHarness({
          authMode, accountant, onTokenUsageIncomplete: () => { notices++; },
          launchRoot: async () => ({ root: fakeRoot, transport, supervisorPid: 4321 }),
        });
        const finalTotal = { inputTokens: 1000, cachedInputTokens: 200, outputTokens: 300, reasoningOutputTokens: 100, totalTokens: 1300 };
        const finalResponse = { inputTokens: 400, cachedInputTokens: 80, outputTokens: 120, reasoningOutputTokens: 40, totalTokens: 520 };
        let buffered = "";
        let resumeParams: Record<string, unknown> | undefined;
        let historyBytes = 0;
        let responseBytes = 0;
        let delivery: Promise<void> | undefined;
        const write = (frame: unknown) => { inbound.write(JSON.stringify(frame) + "\n"); };
        outbound.setEncoding("utf8");
        outbound.on("data", (chunk: string) => {
          buffered += chunk;
          let end: number;
          while ((end = buffered.indexOf("\n")) !== -1) {
            const frame = rec(JSON.parse(buffered.slice(0, end)));
            buffered = buffered.slice(end + 1);
            if (typeof frame.method !== "string" || frame.id === undefined) continue;
            if (frame.method === "thread/resume") {
              resumeParams = rec(frame.params);
              const history = JSON.stringify({
                id: frame.id,
                result: { thread: { id: "resumed-1", turns: [{ items: [{ text: "z".repeat(4 * 1024 * 1024) }] }] } },
              });
              historyBytes = Buffer.byteLength(history, "utf8");
              const metadata = JSON.stringify({ id: frame.id, result: { thread: { id: "resumed-1" } } });
              const responseLine = resumeParams.excludeTurns === true ? metadata : history;
              responseBytes = Buffer.byteLength(responseLine, "utf8");
              inbound.write(responseLine + "\n");
            } else {
              write({ id: frame.id, result: defaultResponder(frame.method) });
            }
            if (frame.method === "turn/start") delivery = (async () => {
              await tick();
              const sequence = [
                ...(hasReplay ? [replay(), replay()] : []),
                turnStarted("resumed-1"),
                tokenUsage(next, response, "resumed-1"),
                tokenUsage(next, response, "resumed-1"),
                tokenUsage(finalTotal, finalResponse, "resumed-1"),
                turnCompleted("completed", { inputTokens: 9999 }, "resumed-1"),
              ];
              // Finite, ordered delivery: a write failure rejects the sequence.
              for (const note of sequence) {
                write({ method: note.method, params: note.params });
                await tick();
              }
            })();
          }
        });
        try {
          // Await the real transport first: omitting excludeTurns must fail on
          // oversized framing before any assertion about the outbound flag.
          const result = terminal(await withTimeout(
            collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events),
            5000, "metadata-only resume",
          ));
          await delivery;
          assert.equal(result.outcome, "success");
          assert.ok(historyBytes > 4 * 1024 * 1024);
          assert.ok(responseBytes < 4 * 1024 * 1024);
          assert.equal(resumeParams?.threadId, "resumed-1");
          assert.equal(resumeParams?.excludeTurns, true);
          assert.equal(accountant.usageIncomplete, !hasReplay);
          assert.equal(notices, hasReplay ? 0 : 1);
          if (hasReplay) {
            assert.deepEqual(accountant.aggregateByModel({ authMode, now: new Date("2026-10-05") })?.["gpt-6-astra"], {
              inputTokens: 560, cacheReadInputTokens: 140, cacheCreationInputTokens: 0,
              outputTokens: 210, reasoningOutputTokens: 70,
              costStatus: "metered",
              costUSD: 0.01624,
            });
          } else {
            assertIncompleteUsage(result);
            assert.deepEqual(accountant.aggregateByModel(), incompleteModels());
            assert.deepEqual(result.metrics?.cost, { kind: "unreported" });
          }
        } finally {
          await delivery;
          await harness.close();
          inbound.destroy();
          outbound.destroy();
        }
      });
    }

    for (const abortAt of ["prelaunch", "preturn"] as const) {
      it(`${authMode}: ${abortAt} abort does not taint an unstarted resumed turn`, async () => {
        const accountant = new CodexUsageAccountant();
        const controller = new AbortController();
        const transport = new FakeTransport(method => {
          if (method === "thread/resume") {
            controller.abort();
            throw new Error("aborted before turn");
          }
          return defaultResponder(method);
        });
        const { harness } = makeHarness({ authMode, accountant, transport });
        if (abortAt === "prelaunch") controller.abort();
        const running = collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1", signal: controller.signal })).events);
        if (abortAt === "preturn") await assert.rejects(running, /aborted before turn/);
        else assert.deepEqual(await running, []);
        assert.equal(transport.requests.some(r => r.method === "turn/start"), false);
        assert.equal(accountant.usageIncomplete, false);
        await harness.close();
      });
    }

    for (const stopKind of ["abort", "stop"] as const) {
      it(`${authMode}: ${stopKind} after thread resume prevents sending turn/start`, async () => {
        const accountant = new CodexUsageAccountant();
        const controller = new AbortController();
        let stopTurn!: () => void;
        const transport = new FakeTransport(method => {
          if (method === "thread/resume") {
            if (stopKind === "abort") controller.abort();
            else stopTurn();
          }
          return defaultResponder(method);
        });
        const { harness } = makeHarness({ authMode, accountant, transport });
        try {
          const turn = harness.startTurn(makeRequest({
            resumeSessionId: "resumed-1", signal: controller.signal,
          }));
          stopTurn = () => turn.requestStop("cancel");
          assert.deepEqual(await collect(turn.events), []);
          assert.equal(transport.requests.some(r => r.method === "turn/start"), false);
          assert.equal(accountant.usageIncomplete, false);
        } finally { await harness.close(); }
      });
    }

    it(`${authMode}: real transport cancellation with an accepted turn/start reply pending cannot erase spend on retry`, async () => {
      const accountant = new CodexUsageAccountant();
      const controller = new AbortController();
      const inbound = new PassThrough();
      const outbound = new PassThrough();
      const transport = createCodexTransport({ inbound, outbound });
      let notices = 0;
      const onTokenUsageIncomplete = async () => { notices++; throw new Error("diagnostic sink failed"); };
      let receivedStart = false;
      const write = (frame: unknown) => { inbound.write(JSON.stringify(frame) + "\n"); };
      let buffered = "";
      outbound.setEncoding("utf8");
      outbound.on("data", (chunk: string) => {
        buffered += chunk;
        let end: number;
        while ((end = buffered.indexOf("\n")) !== -1) {
          const frame = rec(JSON.parse(buffered.slice(0, end)));
          buffered = buffered.slice(end + 1);
          if (typeof frame.method !== "string" || frame.id === undefined) continue;
          if (frame.method === "turn/start") {
            receivedStart = true;
            // The peer persists A but withholds the RPC reply; cancellation rejects the pending request.
            for (const note of [
              turnStarted("resumed-1", "accepted-aborted"),
              tokenUsage(next, response, "resumed-1", "accepted-aborted"),
            ]) write({ method: note.method, params: note.params });
            controller.abort();
          } else write({ id: frame.id, result: defaultResponder(frame.method) });
        }
      });
      const first = makeHarness({ authMode, accountant, onTokenUsageIncomplete,
        launchRoot: async () => ({ root: fakeRoot, transport, supervisorPid: 4321 }) });
      try {
        await assert.rejects(withTimeout(collect(first.harness.startTurn(makeRequest({
          resumeSessionId: "resumed-1", signal: controller.signal,
        })).events), 5000, "pending start cancellation"), /request aborted/);
        assert.equal(receivedStart, true);
        assert.equal(accountant.usageIncomplete, true);
        assert.equal(notices, 1, "cleanup delivers without another notification");
      } finally {
        await first.harness.close();
        inbound.destroy();
        outbound.destroy();
      }

      const retry = makeHarness({ authMode, accountant, onTokenUsageIncomplete });
      // Retry restores H+A (600/180), then spends new B (400/120).
      retry.transport.push(tokenUsage(next, response, "resumed-1", "accepted-aborted"))
        .push(turnStarted("resumed-1"))
        .push(tokenUsage(
          { inputTokens: 1000, cachedInputTokens: 200, outputTokens: 300, reasoningOutputTokens: 100, totalTokens: 1300 },
          { inputTokens: 400, cachedInputTokens: 80, outputTokens: 120, reasoningOutputTokens: 40, totalTokens: 520 },
          "resumed-1"))
        .push(turnCompleted("completed", { inputTokens: 9999 }, "resumed-1")).end();
      try {
        const result = terminal(await collect(retry.harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events));
        assert.equal(result.outcome, "success");
        assertIncompleteUsage(result);
        assert.deepEqual(result.metrics?.cost, { kind: "unreported" });
        assert.deepEqual(accountant.aggregateByModel({ authMode, now: new Date("2026-10-05") }), incompleteModels());
        assert.equal(accountant.usageIncomplete, true);
        assert.equal(notices, 1, "retry cannot redeliver a failed notice callback");
      } finally { await retry.harness.close(); }
    });

    for (const stagedReplay of [true, false]) {
      it(`${authMode}: abort before replay boundary keeps retry totals incomplete (${stagedReplay})`, async () => {
        const accountant = new CodexUsageAccountant();
        const controller = new AbortController();
        let notices = 0;
        const onTokenUsageIncomplete = async () => { notices++; throw new Error("diagnostic sink failed"); };
        const first = makeHarness({ authMode, accountant, onTokenUsageIncomplete });
        const running = collect(first.harness.startTurn(makeRequest({ resumeSessionId: "resumed-1", signal: controller.signal })).events);
        await tick();
        assert.ok(first.transport.requests.some(r => r.method === "turn/start"));
        if (stagedReplay) first.transport.push(replay());
        await tick();
        controller.abort();
        await running;
        assert.equal(accountant.hasBaseline("resumed-1"), false);
        assert.equal(accountant.usageIncomplete, true);
        assert.equal(notices, 1, "cleanup delivers the notice in the cancelled epoch");
        await first.harness.close();
        const retry = makeHarness({ authMode, accountant, onTokenUsageIncomplete });
        // The aborted turn persisted A: the retry restores H+A rather than the original H.
        retry.transport.push(tokenUsage(next, response, "resumed-1", "aborted-turn")).push(turnStarted("resumed-1"))
          .push(tokenUsage(next, response, "resumed-1"))
          .push(turnCompleted("completed", undefined, "resumed-1")).end();
        const result = terminal(await collect(retry.harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events));
        assert.equal(result.outcome, "success");
        assertIncompleteUsage(result);
        assert.deepEqual(result.metrics?.cost, { kind: "unreported" });
        assert.equal(accountant.usageIncomplete, true);
        assert.deepEqual(accountant.aggregateByModel(), incompleteModels());
        assert.equal(notices, 1, "retry shares the once-per-claim diagnostic latch");
        await retry.harness.close();
      });
    }

    for (const firstEpochUsage of [true, false]) {
      it(`${authMode}: no-replay recreation recovers all fresh-root tokens from zero (${firstEpochUsage})`, async () => {
        const accountant = new CodexUsageAccountant();
        let notices = 0;
        const onTokenUsageIncomplete = async () => { notices++; throw new Error("diagnostic rejected"); };
        const first = makeHarness({
          authMode, accountant, onTokenUsageIncomplete,
          transport: new FakeTransport((method) => method === "thread/start" ? { thread: { id: "resumed-1" } } : defaultResponder(method)),
        });
        first.transport.push(turnStarted("resumed-1"));
        if (firstEpochUsage) first.transport.push(tokenUsage(historical, historical, "resumed-1"));
        first.transport.push(turnCompleted("completed", undefined, "resumed-1")).end();
        assert.equal(terminal(await collect(first.harness.startTurn(makeRequest()).events)).outcome, "success");
        const before = accountant.aggregateByModel({ authMode, now: new Date("2026-10-05") });
        assert.equal(accountant.hasBaseline("resumed-1"), true);
        await first.harness.close();

        const second = makeHarness({ authMode, accountant, onTokenUsageIncomplete });
        second.transport.push(turnStarted("resumed-1"))
          .push(tokenUsage(next, response, "resumed-1"))
          .push(tokenUsage(next, response, "resumed-1"))
          .push(tokenUsage({ ...next, inputTokens: 1000, cachedInputTokens: 200, outputTokens: 300, reasoningOutputTokens: 100, totalTokens: 1300 },
            { inputTokens: 400, cachedInputTokens: 80, outputTokens: 120, reasoningOutputTokens: 40, totalTokens: 520 }, "resumed-1"))
          .push(turnCompleted("completed", { inputTokens: 9999 }, "resumed-1")).end();
        const result = terminal(await collect(second.harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events));
        assert.equal(result.outcome, "success");
        if (firstEpochUsage) {
          const entry = accountant.aggregateByModel({ authMode, now: new Date("2026-10-05") })?.["gpt-6-astra"];
          assert.deepEqual(entry, {
            inputTokens: 800, cacheReadInputTokens: 200, cacheCreationInputTokens: 0,
            outputTokens: 300, reasoningOutputTokens: 100,
            costStatus: "metered",
            costUSD: 0.0232,
          });
          assert.ok(before?.["gpt-6-astra"]);
          assert.equal(notices, 0);
        } else {
          assert.deepEqual(accountant.aggregateByModel({ authMode, now: new Date("2026-10-05") })?.["gpt-6-astra"], {
            inputTokens: 800, cacheReadInputTokens: 200, cacheCreationInputTokens: 0,
            outputTokens: 300, reasoningOutputTokens: 100,
            costStatus: "unreported",
          });
          assert.ok(result.usage);
          assert.deepEqual(result.metrics?.cost, { kind: "unreported" });
          assert.equal(accountant.usageIncomplete, false);
          assert.equal(notices, 0);
        }
        await second.harness.close();
      });
    }

    for (const problem of ["absent", "malformed", "typed-malformed", "coerced", "child-only"] as const) {
      it(`${authMode}: ${problem} replay completes with sticky incomplete totals and one nonfatal notice`, async () => {
        const accountant = new CodexUsageAccountant();
        let notices = 0;
        const onTokenUsageIncomplete = () => { notices++; throw new Error("diagnostic sink failed"); };
        const { harness, transport } = makeHarness({ authMode, accountant, onTokenUsageIncomplete });
        if (problem === "malformed")
          transport.push({ kind: "activity", method: "thread/tokenUsage/updated", params: { threadId: "resumed-1", tokenUsage: {} } });
        if (problem === "typed-malformed") {
          const note = replay();
          transport.push({ ...note, usage: { ...note.usage, pricingEvidenceComplete: false } });
        }
        if (problem === "coerced") {
          const note = replay();
          const raw = rec(rec(note.params).tokenUsage);
          rec(raw.total).reasoningOutputTokens = "30";
          transport.push(note);
        }
        if (problem === "child-only")
          transport.push(tokenUsage(historical, last, "child", "historical"));
        transport.push(turnStarted("resumed-1"))
          .push(tokenUsage(next, response, "resumed-1"))
          .push(tokenUsage(next, response, "resumed-1"))
          .push(turnCompleted("completed", { inputTokens: 9999 }, "resumed-1")).end();
        const result = terminal(await collect(harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events));
        assert.equal(result.outcome, "success");
        assertIncompleteUsage(result);
        assert.deepEqual(result.metrics?.cost, { kind: "unreported" });
        assert.equal(notices, 1);
        assert.deepEqual(accountant.aggregateByModel(), incompleteModels());
        await harness.close();

        const retry = makeHarness({ authMode, accountant, onTokenUsageIncomplete });
        retry.transport.push(replay()).push(turnStarted("resumed-1"))
          .push(tokenUsage(next, response, "resumed-1"))
          .push(turnCompleted("completed", undefined, "resumed-1")).end();
        assertIncompleteUsage(terminal(await collect(retry.harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" })).events)));
        assert.equal(notices, 1);
        await retry.harness.close();
      });
    }
  }
});

// --- tests --------------------------------------------------------------------------

describe("CodexHarness: kind + thread configuration", () => {
  it("advertises kind codex", () => {
    const { harness } = makeHarness();
    assert.equal(harness.kind, "codex");
  });

  it("authenticates before thread/model work and routes refresh outside the model broker", async () => {
    let brokerCalls = 0;
    const appServerAuth = createCodexAppServerAuth({
      mode: "subscription",
      initial: { accessToken: "initial-access", accountId: "server-account" },
      bridge: {
        refresh: async () => ({ accessToken: "next-access", accountId: "server-account" }),
      },
    });
    const transport = new FakeTransport(authResponder);
    const { harness } = makeHarness({
      transport,
      appServerAuth,
      broker: stubBroker(async () => {
        brokerCalls += 1;
        return { ok: true, output: {} };
      }),
    });
    transport
      .push(threadStarted())
      .push({
        kind: "activity",
        method: "account/chatgptAuthTokens/refresh",
        requestId: 91,
        params: { reason: "unauthorized", previousAccountId: "hint-only" },
      })
      .push(turnCompleted())
      .end();

    await collect(harness.startTurn(makeRequest()).events);

    assert.deepEqual(transport.requests.map((request) => request.method), [
      "initialize",
      "account/login/start",
      "thread/start",
      "turn/start",
    ]);
    assert.deepEqual(transport.notifies, [{ method: "initialized", params: undefined }]);
    assert.equal(brokerCalls, 0, "the auth callback never enters the model callback broker");
    assert.deepEqual(transport.responses, [{
      requestId: 91,
      response: {
        result: { accessToken: "next-access", chatgptAccountId: "server-account", chatgptPlanType: null },
      },
    }]);
  });

  it("pumps auth while thread/start is pending, so setup cannot deadlock behind notifications", async () => {
    let brokerCalls = 0;
    let resolveThread!: (value: unknown) => void;
    let transport!: FakeTransport;
    transport = new FakeTransport((method, params) => {
      if (method === "thread/start") {
        const pending = new Promise<unknown>((resolve) => {
          resolveThread = resolve;
        });
        // This request is emitted while thread/start is unresolved. Without the read-path
        // auth pump it sits behind notifications(), which the harness cannot consume until
        // thread/start returns: the exact setup deadlock this regression pins.
        transport.emitServerRequest({
          kind: "activity",
          method: "account/chatgptAuthTokens/refresh",
          requestId: 92,
          params: { reason: "unauthorized", previousAccountId: "hint-only" },
        });
        return pending;
      }
      return authResponder(method, params);
    });
    transport.onRespond = (requestId) => {
      if (requestId === 92) resolveThread({ thread: { id: "th-1" } });
    };
    const appServerAuth = createCodexAppServerAuth({
      mode: "subscription",
      initial: { accessToken: "initial-access", accountId: "server-account" },
      bridge: {
        refresh: async () => ({ accessToken: "next-access", accountId: "server-account" }),
      },
    });
    const { harness } = makeHarness({
      transport,
      appServerAuth,
      broker: stubBroker(async () => {
        brokerCalls += 1;
        return { ok: true, output: {} };
      }),
    });
    transport.push(threadStarted()).push(turnCompleted()).end();

    const events = await withTimeout(collect(harness.startTurn(makeRequest()).events), 500, "setup auth refresh");

    assert.equal(events.at(-1)?.kind, "turn_finished");
    assert.equal(brokerCalls, 0, "setup auth never enters the model callback broker");
    assert.equal(transport.responses.some((response) => response.requestId === 92), true);
  });

  it("does not publish a same-chunk terminal while an intercepted refresh is held", async () => {
    let releaseRefresh!: (value: { accessToken: string; accountId: string }) => void;
    const heldRefresh = new Promise<{ accessToken: string; accountId: string }>((resolve) => {
      releaseRefresh = resolve;
    });
    let brokerCalls = 0;
    let transport!: FakeTransport;
    transport = new FakeTransport((method, params) => {
      if (method === "turn/start") {
        queueMicrotask(() => {
          // Same decoder batch ordering: the interceptor owns refresh before terminal is
          // visible to the consumer. Terminal must drain that owner before publication.
          transport.emitServerRequest({
            kind: "activity",
            method: "account/chatgptAuthTokens/refresh",
            requestId: 93,
            params: { reason: "unauthorized", previousAccountId: "hint-only" },
          });
          transport.push(turnCompleted());
        });
      }
      return authResponder(method, params);
    });
    const appServerAuth = createCodexAppServerAuth({
      mode: "subscription",
      initial: { accessToken: "initial-access", accountId: "server-account" },
      bridge: { refresh: () => heldRefresh },
    });
    const { harness } = makeHarness({
      transport,
      appServerAuth,
      broker: stubBroker(async () => {
        brokerCalls += 1;
        return { ok: true, output: {} };
      }),
    });
    transport.push(threadStarted());

    let settled = false;
    const result = collect(harness.startTurn(makeRequest()).events).finally(() => {
      settled = true;
    });
    await tick();
    await tick();
    assert.equal(settled, false, "terminal remains withheld while refresh is held");
    assert.equal(brokerCalls, 0);

    releaseRefresh({ accessToken: "next-access", accountId: "server-account" });
    const events = await withTimeout(result, 500, "held auth terminal drain");
    assert.equal(events.at(-1)?.kind, "turn_finished");
    assert.equal(transport.responses.some((response) => response.requestId === 93), true);
  });

  it("close cancels and drains accepted intercepted auth before transport teardown", async () => {
    let bridgeEntered!: () => void;
    const entered = new Promise<void>((resolve) => {
      bridgeEntered = resolve;
    });
    let bridgeSettled = false;
    const appServerAuth = createCodexAppServerAuth({
      mode: "subscription",
      initial: { accessToken: "initial-access", accountId: "server-account" },
      bridge: {
        refresh: ({ signal }) => new Promise((_resolve, reject) => {
          bridgeEntered();
          signal.addEventListener("abort", () => {
            setTimeout(() => {
              bridgeSettled = true;
              reject(new Error("cancelled"));
            }, 20);
          }, { once: true });
        }),
      },
    });
    const transport = new FakeTransport(authResponder);
    const { harness } = makeHarness({ transport, appServerAuth });
    transport.push(threadStarted());
    const iterator = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
    await iterator.next();

    transport.emitServerRequest({
      kind: "activity",
      method: "account/chatgptAuthTokens/refresh",
      requestId: 94,
      params: { reason: "unauthorized", previousAccountId: null },
    });
    await entered;

    await harness.close();

    assert.equal(bridgeSettled, true, "close waited for the cancelled bridge to settle");
    assert.equal(
      transport.responses.some((response) => response.requestId === 94),
      false,
      "transport closes before drain, so shutdown sends no late auth reply",
    );
    assert.ok(transport.closes >= 1);
  });

  it("close breaks a pending-login plus intercepted-refresh ownership cycle", async () => {
    let rejectLogin!: (error: Error) => void;
    let markRefreshEmitted!: () => void;
    const refreshEmitted = new Promise<void>((resolve) => {
      markRefreshEmitted = resolve;
    });
    let bridgeCalls = 0;
    let brokerCalls = 0;
    let transport!: FakeTransport;
    transport = new FakeTransport((method, params) => {
      if (method === "account/login/start") {
        queueMicrotask(() => {
          transport.emitServerRequest({
            kind: "activity",
            method: "account/chatgptAuthTokens/refresh",
            requestId: 95,
            params: { reason: "unauthorized", previousAccountId: null },
          });
          markRefreshEmitted();
        });
        return new Promise<never>((_resolve, reject) => {
          rejectLogin = reject;
        });
      }
      return authResponder(method, params);
    });
    transport.onClose = () => rejectLogin(new Error("transport closed"));
    const appServerAuth = createCodexAppServerAuth({
      mode: "subscription",
      initial: { accessToken: "initial-access", accountId: "server-account" },
      bridge: {
        refresh: async () => {
          bridgeCalls += 1;
          return { accessToken: "unused", accountId: "unused" };
        },
      },
    });
    const { harness } = makeHarness({
      transport,
      appServerAuth,
      broker: stubBroker(async () => {
        brokerCalls += 1;
        return { ok: true, output: {} };
      }),
    });
    const iterator = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
    const pendingSetup = iterator.next();
    void pendingSetup.catch(() => {});
    await refreshEmitted;

    await withTimeout(harness.close(), 500, "pending-login auth close");
    await assert.rejects(pendingSetup, /authentication startup failed/);
    await withTimeout(appServerAuth.drainInterceptedRequests(), 50, "post-close auth drain");

    assert.equal(bridgeCalls, 0, "refresh never crosses the unvalidated login boundary");
    assert.equal(brokerCalls, 0);
    assert.ok(transport.closes >= 1);
  });

  it("rejects simultaneous env credential and app-server auth instead of choosing a fallback", () => {
    const appServerAuth = createCodexAppServerAuth({ mode: "api_key", apiKey: "test-key" });
    assert.throws(
      () => makeHarness({ appServerAuth, credentialValue: "other-key" }),
      /conflicting authentication inputs/,
    );
  });

  it("thread/start carries worker dynamic tools, no native environment, explicit instructions/trust, and no hook bypass", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).end();
    const turn = harness.startTurn(makeRequest());
    const it = turn.events[Symbol.asyncIterator]();
    await it.next(); // drives setup (thread/start + turn/start) then yields `initialized`

    const start = transport.requests.find((r) => r.method === "thread/start");
    assert.ok(start, "thread/start was sent");
    const params = rec(start.params);
    assert.equal(params.ephemeral, false, "run roots persist the rollout required by new-root resume");
    const config = rec(params.config);
    assert.deepEqual(params.environments, [], "an empty environment list disables native shell and patch tools");
    const dynamicTools = params.dynamicTools as Array<Record<string, unknown>>;
    assert.ok(dynamicTools.some((tool) => tool.name === "signal_done"), "the root callback vocabulary is registered");
    assert.ok(dynamicTools.some((tool) => tool.name === "uzi_bash"), "the screened shell callback uses a collision-free wire alias");
    assert.equal(dynamicTools.some((tool) => tool.name === "SubagentStart"), false, "code-mode lifecycle names are not model-visible");
    assert.equal(params.developerInstructions, "you are the lead");
    assert.equal(params.instructions, undefined, "the ignored legacy field is never sent");
    const startTurn = transport.requests.find((r) => r.method === "turn/start");
    assert.ok(startTurn, "turn/start was sent after thread/start");
    assert.deepEqual(rec(startTurn.params).environments, [], "every root turn disables native environments");
    assert.equal(config.project_doc_max_bytes, 0);
    const projects = rec(config.projects);
    assert.deepEqual(projects[WORKSPACE], { trust_level: "untrusted" });
    // No hook-trust bypass anywhere in the start params — the one fixture-only affordance
    // that must never reach production (config.ts header; harness.mjs:362).
    assert.doesNotMatch(JSON.stringify(params), /bypass_hook_trust/);
    assert.doesNotMatch(JSON.stringify(params), /dangerously-bypass-hook-trust/);
  });

  it("thread/resume ALSO carries untrusted + doc_max 0 and never a hook-trust bypass", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted("resumed-1")).end();
    const turn = harness.startTurn(makeRequest({ resumeSessionId: "resumed-1" }));
    const it = turn.events[Symbol.asyncIterator]();
    await it.next();

    assert.equal(transport.requests.some((r) => r.method === "thread/start"), false, "resume does not start a fresh thread");
    const resume = transport.requests.find((r) => r.method === "thread/resume");
    assert.ok(resume, "thread/resume was sent");
    const params = rec(resume.params);
    assert.equal(params.threadId, "resumed-1");
    assert.equal(params.dynamicTools, undefined, "pinned thread/resume restores dynamic tools from persisted history");
    assert.equal(params.environments, undefined, "thread/resume has no environments field in the pinned protocol");
    assert.equal(params.developerInstructions, "you are the lead");
    assert.equal(params.instructions, undefined);
    const config = rec(params.config);
    assert.equal(config.project_doc_max_bytes, 0);
    assert.deepEqual(rec(config.projects)[WORKSPACE], { trust_level: "untrusted" });
    assert.doesNotMatch(JSON.stringify(params), /bypass_hook_trust/);
    const startTurn = transport.requests.find((r) => r.method === "turn/start");
    assert.ok(startTurn, "turn/start was sent after resume");
    assert.deepEqual(
      rec(startTurn.params).environments,
      [],
      "the resumed turn explicitly disables native environments instead of accepting provider defaults",
    );
  });

  it("rejects a thread/resume response without a non-empty thread id", async () => {
    const transport = new FakeTransport((method) => {
      if (method === "thread/resume") return { thread: {} };
      return defaultResponder(method);
    });
    const { harness } = makeHarness({ transport });
    await assert.rejects(
      collect(harness.startTurn(makeRequest({ resumeSessionId: "requested-session" })).events),
      (err: unknown) => err instanceof CodexResumeError && err.message === "codex thread/resume failed",
    );
    assert.equal(
      transport.requests.some((r) => r.method === "turn/start"),
      false,
      "a malformed resume response never starts model work",
    );
  });

  it("starts the turn with the rendered prompt + model + effort", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).end();
    const turn = harness.startTurn(makeRequest({ effort: "high" }));
    await turn.events[Symbol.asyncIterator]().next();

    const start = transport.requests.find((r) => r.method === "turn/start");
    assert.ok(start);
    const params = rec(start.params);
    assert.equal(params.model, "gpt-6-astra");
    assert.equal(params.effort, "high");
    assert.equal(params.modelReasoningEffort, undefined, "only the actual app-server wire key is sent");
    assert.deepEqual(params.input, [{ type: "text", text: "do the thing" }]);
  });

  // PRD #1551 (D4/D5): a validated custom worker-default root model reaches the turn/start +
  // initialized event unchanged (render passes it through on modelSource:"worker_default"),
  // never the provider fallback gpt-6-astra. Discriminating: the passthrough revert would make
  // render drop it and the harness fall back to provider.model.
  it("carries a custom worker-default root model to turn/start + initialized, not the provider fallback", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnCompleted("completed")).end();
    const events = await collect(
      harness.startTurn(makeRequest({ model: "gpt-7-custom-preview", modelSource: "worker_default" })).events,
    );
    const start = transport.requests.find((r) => r.method === "turn/start");
    assert.ok(start);
    assert.equal(rec(start.params).model, "gpt-7-custom-preview", "turn/start carries the custom model");
    const init = events.find((e) => e.kind === "initialized");
    assert.ok(init && init.kind === "initialized");
    if (init.kind === "initialized") assert.equal(init.model, "gpt-7-custom-preview", "initialized reports the custom model");
  });

  it("carries a custom worker-default root model through a RESUME turn too", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted("resumed-1")).end();
    const turn = harness.startTurn(
      makeRequest({ model: "gpt-7-custom-preview", modelSource: "worker_default", resumeSessionId: "resumed-1" }),
    );
    await turn.events[Symbol.asyncIterator]().next();
    const start = transport.requests.find((r) => r.method === "turn/start");
    assert.ok(start, "turn/start was sent after resume");
    assert.equal(rec(start.params).model, "gpt-7-custom-preview", "the resumed turn carries the custom model, not gpt-6-astra");
  });
});

describe("CodexHarness: frame → neutral event decode", () => {
  it("#1566 projects async agent text as ordinary text, without a questions signal", async () => {
    let brokerCalls = 0;
    const { harness, transport } = makeHarness({ broker: stubBroker(async () => {
      brokerCalls += 1;
      return { ok: true, output: {} };
    }) });
    transport.push(threadStarted()).push({
      kind: "activity", method: "item/completed",
      params: { threadId: "th-1", item: { type: "agentMessage", text: "Choose validation",
        delivery: "async", questions: [{ title: "Which validation?", options: ["Focused", "Defer"] }] } },
    }).push(agentMessage("finished")).push(turnCompleted("completed")).end();
    const events = await collect(harness.startTurn(makeRequest()).events);
    const frames = events.filter(e => e.kind === "frame");
    assert.deepEqual(frames.map(e => e.items), [[{ kind: "text", text: "Choose validation" }], [{ kind: "text", text: "finished" }]]);
    assert.equal(JSON.stringify(events).includes('"questions"'), false);
    assert.equal(brokerCalls, 0);
    assert.deepEqual(transport.responses, []);
    assert.equal(events.at(-1)?.kind, "turn_finished");
  });

  it("decodes the full turn: initialized(model) → activity → frame(items/usage/model) → turn_finished", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(delta()) // an item update is activity only
      .push(agentMessage("hello world", { input_tokens: 5, output_tokens: 3 }))
      .push(turnCompleted("completed", { total_tokens: 12 }))
      .end();

    const events = await collect(harness.startTurn(makeRequest()).events);
    assert.deepEqual(
      events.map((e) => e.kind),
      ["initialized", "activity", "activity", "frame", "turn_finished"],
    );

    // init → initialized with the configured model.
    const init = events[0]!;
    assert.equal(init.kind, "initialized");
    if (init.kind === "initialized") {
      assert.equal(init.model, "gpt-6-astra");
      assert.equal(init.sessionId, "th-1");
    }

    // assistant → frame with items + call-basis usage + model.
    const frame = events[3]!;
    assert.equal(frame.kind, "frame");
    if (frame.kind === "frame") {
      assert.deepEqual(frame.items, [{ kind: "text", text: "hello world" }]);
      assert.deepEqual(frame.origin, { kind: "main" });
      assert.equal(frame.model, "gpt-6-astra");
      assert.deepEqual(frame.usage, { basis: "call", tokens: {}, wire: { usage: { input_tokens: 5, output_tokens: 3 } } });
    }
  });

  it("turn/completed → turn_finished with the decoded terminal (outcome/subtype/usage)", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnCompleted("completed", { total_tokens: 10 })).end();

    const events = await collect(harness.startTurn(makeRequest()).events);
    const last = events.at(-1)!;
    assert.equal(last.kind, "turn_finished");
    if (last.kind === "turn_finished") {
      assert.equal(last.terminal.outcome, "success");
      assert.equal(last.terminal.subtype, "completed");
      assert.deepEqual(last.terminal.usage, { basis: "turn", tokens: {}, wire: { usage: { total_tokens: 10 } } });
      assert.deepEqual(last.terminal.metrics.cost, { kind: "unreported" });
    }
  });

  it("a FAILED terminal is neutral data (turn_finished), never a thrown failure", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnCompleted("failed")).end();

    const events = await collect(harness.startTurn(makeRequest()).events); // does NOT throw
    const last = events.at(-1)!;
    assert.equal(last.kind, "turn_finished");
    if (last.kind === "turn_finished") {
      assert.equal(last.terminal.outcome, "failed");
      assert.equal(last.terminal.subtype, "failed");
      // The deferred failure materializes the generic terminal exception, no invented category.
      const thrown = last.terminal.failure!.materialize();
      assert.equal(thrown.failure.category, "unknown");
      assert.match(thrown.failure.message, /codex turn failed: failed/);
    }
  });

  it("closes the iterator after the terminal (a trailing note is not decoded)", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(turnCompleted("completed"))
      .push(agentMessage("after the end")) // must never be decoded
      .end();

    const events = await collect(harness.startTurn(makeRequest()).events);
    assert.equal(events.at(-1)!.kind, "turn_finished");
    assert.equal(events.filter((e) => e.kind === "frame").length, 0);
  });
});

describe("CodexHarness: token accounting → result-frame modelUsage (PRD #1332 C4a)", () => {
  function terminalModelUsage(events: HarnessEvent[]): Record<string, unknown> | undefined {
    const last = events.at(-1)!;
    assert.equal(last.kind, "turn_finished");
    if (last.kind !== "turn_finished") return undefined;
    return last.terminal.usage?.wire?.modelUsage as Record<string, unknown> | undefined;
  }

  it("aggregates root token-usage into modelUsage keyed by the configured model, unreported", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(tokenUsage(
        { inputTokens: 500, cachedInputTokens: 100, cacheWriteInputTokens: 20, outputTokens: 300, reasoningOutputTokens: 40, totalTokens: 800 },
        { inputTokens: 500, cachedInputTokens: 100, cacheWriteInputTokens: 20, outputTokens: 300, reasoningOutputTokens: 40, totalTokens: 800 },
      ))
      .push(turnCompleted("completed"))
      .end();

    const mu = terminalModelUsage(await collect(harness.startTurn(makeRequest()).events));
    // uncached input = 500 - 100 - 20 = 380. Every entry is unreported with NO costUSD.
    assert.deepEqual(mu, {
      "gpt-6-astra": {
        inputTokens: 380,
        outputTokens: 300,
        cacheReadInputTokens: 100,
        cacheCreationInputTokens: 20,
        reasoningOutputTokens: 40,
        costStatus: "unreported",
      },
    });
  });

  it("a turn with NO token-usage notes leaves the terminal usage byte-identical (no modelUsage)", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnCompleted("completed", { total_tokens: 10 })).end();
    const events = await collect(harness.startTurn(makeRequest()).events);
    const last = events.at(-1)!;
    assert.equal(last.kind, "turn_finished");
    if (last.kind === "turn_finished") {
      // Exactly the pre-C4a shape: bounded turn.usage, and NO modelUsage key on the wire.
      assert.deepEqual(last.terminal.usage, { basis: "turn", tokens: {}, wire: { usage: { total_tokens: 10 } } });
    }
  });

  it("a duplicate root token-usage note does not inflate the emitted modelUsage", async () => {
    const { harness, transport } = makeHarness();
    const note = tokenUsage({ inputTokens: 200, outputTokens: 120, totalTokens: 320 }, { inputTokens: 200, outputTokens: 120, totalTokens: 320 });
    transport.push(threadStarted()).push(turnStarted()).push(note).push(note).push(turnCompleted("completed")).end();
    const mu = terminalModelUsage(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(mu, {
      "gpt-6-astra": { inputTokens: 200, outputTokens: 120, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "unreported" },
    });
  });

  it("a mixed-model child's usage aggregates under its OWN model, not the root's", async () => {
    const { harness, transport } = makeHarness();
    // Mirror exactly what the executor's child-turn seam does: record the child model and
    // register its sink before the child's frames arrive.
    harness.recordChildThreadModel("th-child", "gpt-5.6-sol");
    harness.registerChildSink("th-child", { push: () => {} });
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(tokenUsage({ inputTokens: 300, outputTokens: 200, totalTokens: 500 }, { inputTokens: 300, outputTokens: 200, totalTokens: 500 })) // root (th-1)
      .push(tokenUsage({ inputTokens: 80, outputTokens: 40, totalTokens: 120 }, { inputTokens: 80, outputTokens: 40, totalTokens: 120 }, "th-child", "ctn-1")) // child
      .push(turnCompleted("completed"))
      .end();

    const mu = terminalModelUsage(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(mu, {
      "gpt-6-astra": { inputTokens: 300, outputTokens: 200, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "unreported" },
      "gpt-5.6-sol": { inputTokens: 80, outputTokens: 40, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "unreported" },
    });
  });

  it("an unknown child thread's usage is NOT attributed to the root model", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(tokenUsage({ inputTokens: 300, outputTokens: 200, totalTokens: 500 }, { inputTokens: 300, outputTokens: 200, totalTokens: 500 })) // root (th-1)
      // A usage note for a thread never registered as root or child (no sink either): dropped.
      .push(tokenUsage({ inputTokens: 9999, outputTokens: 9999, totalTokens: 19998 }, { inputTokens: 9999, outputTokens: 9999, totalTokens: 19998 }, "th-stray", "stn-1"))
      .push(turnCompleted("completed"))
      .end();

    const mu = terminalModelUsage(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(mu, {
      "gpt-6-astra": { inputTokens: 300, outputTokens: 200, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, reasoningOutputTokens: 0, costStatus: "unreported" },
    });
  });
});

describe("CodexHarness: cost projection from the run auth mode (PRD #1332 C4b / D5)", () => {
  function terminal(events: HarnessEvent[]): HarnessTerminal {
    const last = events.at(-1)!;
    assert.equal(last.kind, "turn_finished");
    if (last.kind !== "turn_finished") throw new Error("no terminal");
    return last.terminal;
  }

  interface WireEntry {
    costStatus: string;
    costUSD?: number;
    inputTokens: number;
    outputTokens: number;
  }

  /** The named model's `modelUsage` entry off a terminal, extracted safely (no optional-chain
   *  indexing) so a missing `modelUsage` fails the assertion rather than throwing. */
  function entryOf(term: HarnessTerminal, model: string): WireEntry {
    const wire = term.usage?.wire;
    const modelUsage = wire?.modelUsage as Record<string, WireEntry> | undefined;
    assert.ok(modelUsage, "the terminal carries per-model usage");
    const entry = modelUsage[model];
    assert.ok(entry, `an entry for ${model}`);
    return entry;
  }

  it("an api_key run prices the reconciled response as metered (costUSD) and folds the run cost", async () => {
    const { harness, transport } = makeHarness({ authMode: "api_key" });
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(tokenUsage(
        { inputTokens: 500, cachedInputTokens: 100, cacheWriteInputTokens: 20, outputTokens: 300, reasoningOutputTokens: 40, totalTokens: 800 },
        { inputTokens: 500, cachedInputTokens: 100, cacheWriteInputTokens: 20, outputTokens: 300, reasoningOutputTokens: 40, totalTokens: 800 },
      ))
      .push(turnCompleted("completed"))
      .end();
    const term = terminal(await collect(harness.startTurn(makeRequest()).events));
    const mu = entryOf(term, "gpt-6-astra");
    assert.equal(mu.costStatus, "metered");
    assert.ok(mu.costUSD !== undefined, "a metered entry carries costUSD");
    // uncached 380: 380*10 + 100*1 + 20*12.5 + 300*50 = 19150 µ$.
    assert.equal(Math.round(mu.costUSD * 1e6), 19150);
    // The run-level HarnessCost mirrors it: metered, price_table sourced, same dollars.
    assert.deepEqual(term.metrics.cost, { kind: "metered", usd: mu.costUSD, source: "price_table" });
  });

  it("a subscription run emits a PRIMARY metered model estimate and SECONDARY run cost", async () => {
    const { harness, transport } = makeHarness({ authMode: "subscription" });
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(tokenUsage({ inputTokens: 300, outputTokens: 200, totalTokens: 500 }, { inputTokens: 300, outputTokens: 200, totalTokens: 500 }))
      .push(turnCompleted("completed"))
      .end();
    const term = terminal(await collect(harness.startTurn(makeRequest()).events));
    const mu = entryOf(term, "gpt-6-astra");
    assert.equal(mu.costStatus, "metered");
    assert.equal(mu.costUSD, 0.013);
    assert.deepEqual(term.metrics.cost, { kind: "metered", usd: 0.013, source: "price_table" });
  });

  it("missing auth keeps PRIMARY model usage and SECONDARY run cost unreported", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(tokenUsage({ inputTokens: 300, outputTokens: 200, totalTokens: 500 }, { inputTokens: 300, outputTokens: 200, totalTokens: 500 }))
      .push(turnCompleted("completed"))
      .end();
    const term = terminal(await collect(harness.startTurn(makeRequest()).events));
    const mu = entryOf(term, "gpt-6-astra");
    assert.equal(mu.costStatus, "unreported");
    assert.ok(!("costUSD" in mu));
    assert.deepEqual(term.metrics.cost, { kind: "unreported" });
  });

  it("an api_key run with an unreconciled gap keeps tokens but reports the run cost unreported", async () => {
    const { harness, transport } = makeHarness({ authMode: "api_key" });
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(tokenUsage({ inputTokens: 100, outputTokens: 60, totalTokens: 160 }, { inputTokens: 100, outputTokens: 60, totalTokens: 160 }))
      // A jump the observed responses cannot account for (a dropped intermediate note).
      .push(tokenUsage({ inputTokens: 700, outputTokens: 500, totalTokens: 1200 }, { inputTokens: 400, outputTokens: 300, totalTokens: 700 }))
      .push(turnCompleted("completed"))
      .end();
    const term = terminal(await collect(harness.startTurn(makeRequest()).events));
    const mu = entryOf(term, "gpt-6-astra");
    assert.equal(mu.costStatus, "unreported");
    assert.ok(!("costUSD" in mu));
    // Tokens retained from the cumulative delta.
    assert.equal(mu.inputTokens, 700);
    assert.equal(mu.outputTokens, 500);
    assert.deepEqual(term.metrics.cost, { kind: "unreported" });
  });
});

describe("CodexHarness: notifications are bound to the active (thread, turn)", () => {
  it("a turn_completed with a STALE turnId is liveness only; the REAL active terminal ends the turn", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      // A stale-turn terminal (right thread, wrong turn) must NOT end the root turn — it is
      // surfaced as `activity`, so the loop keeps waiting for the real active-turn terminal.
      .push(turnCompleted("completed", undefined, "th-1", "stale-turn"))
      // The REAL active turn's terminal (th-1/tn-1) is the one that ends the turn.
      .push(turnCompleted("completed", { total_tokens: 3 }))
      .end();

    const events = await collect(harness.startTurn(makeRequest()).events);
    // The stale terminal shows up as an activity, never as an early turn_finished.
    assert.deepEqual(
      events.map((e) => e.kind),
      ["initialized", "activity", "turn_finished"],
    );
    assert.equal(events.filter((e) => e.kind === "turn_finished").length, 1);
    const last = events.at(-1)!;
    assert.equal(last.kind, "turn_finished");
    // The surviving terminal is the REAL one (its usage), not the stale (usage-less) one.
    if (last.kind === "turn_finished") {
      assert.deepEqual(last.terminal.usage, { basis: "turn", tokens: {}, wire: { usage: { total_tokens: 3 } } });
    }
  });

  it("a child/foreign-thread turn_completed is ignored; a clean EOF then protocol-throws (no fabricated terminal)", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      // A foreign thread's terminal must never end the root turn.
      .push(turnCompleted("completed", undefined, "foreign-thread", "tn-1"))
      .end(); // clean EOF WITHOUT the real active terminal

    await assert.rejects(collect(harness.startTurn(makeRequest()).events), (err: unknown) => {
      assert.ok(err instanceof CodexHarnessError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /ended before turn completion/);
      return true;
    });
  });

  it("a child/foreign-thread item/completed yields activity, never a main frame", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      // An assistant item on a foreign thread must not become a {origin:{kind:"main"}} frame.
      .push(completedItem({ type: "agentMessage", text: "from a child thread" }, "foreign-thread"))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(makeRequest()).events);
    assert.equal(events.filter((e) => e.kind === "frame").length, 0, "no main frame for a foreign thread");
    assert.deepEqual(
      events.map((e) => e.kind),
      ["initialized", "activity", "turn_finished"],
    );
  });
});

describe("CodexHarness: bounded Read transport serialization (#296)", () => {
  for (const { name, raw, args, expected, offset, lines, partial, truncated } of [
    { name: "middle excerpt", raw: "a\nb\nc\n", args: { file_path: "src/x.ts", offset: 2, limit: 1 }, expected: "b\n", offset: 2, lines: 1, partial: false, truncated: true },
    { name: "near 1 MiB U+0001", raw: "\u0001".repeat(1024 * 1024 - 2), args: { path: "src/x.ts" }, expected: "\u0001".repeat(64 * 1024), offset: 1, lines: 1, partial: true, truncated: true },
    { name: "near 1 MiB quotes and backslashes", raw: '"\\'.repeat((1024 * 1024 - 2) / 2), args: { path: "src/x.ts" }, expected: '"\\'.repeat(32 * 1024), offset: 1, lines: 1, partial: true, truncated: true },
  ]) {
    it("delivers and settles " + name + " through emitted transport frames", async () => {
      const inbound = new PassThrough();
      const outbound = new PassThrough();
      const transport = createCodexTransport({ inbound, outbound });
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
      const request = makeRequest();
      let calls = 0;
      const broker = new CodexCallbackBroker({
        registry,
        spawnCommand: async () => ({ code: 0, stdout: "", stderr: "" }),
        fileop: { op: async () => { calls++; return { ok: true, size: Buffer.byteLength(raw), data: Buffer.from(raw).toString("base64") }; } },
        worktreePath: WORKSPACE,
        grants: renderCodexRun(request).leadGrants,
        delegate: async () => ({ ok: false, code: "x", message: "no" }),
        allowedRoles: new Set<string>(),
      });
      const replies: { frame: Record<string, unknown>; bytes: number }[] = [];
      const write = (frame: unknown): void => { inbound.write(JSON.stringify(frame) + "\n"); };
      let buffered = "";
      outbound.setEncoding("utf8");
      outbound.on("data", (chunk: string) => {
        buffered += chunk;
        let nl = buffered.indexOf("\n");
        while (nl !== -1) {
          const line = buffered.slice(0, nl);
          buffered = buffered.slice(nl + 1);
          const frame = rec(JSON.parse(line));
          if (typeof frame.method === "string" && frame.id !== undefined) {
            write({ id: frame.id, result: defaultResponder(frame.method) });
            if (frame.method === "turn/start") {
              write({ method: "thread/started", params: { thread: { id: "th-1" } } });
              write({ id: 296, method: "item/tool/call", params: { threadId: "th-1", turnId: "tn-1", callId: "read-296", tool: "uzi_read", arguments: args } });
            }
          } else if (frame.id === 296) {
            replies.push({ frame, bytes: Buffer.byteLength(line) + 1 });
            // Complete only once the real response has arrived at the peer.
            write({ method: "turn/completed", params: { threadId: "th-1", turn: { id: "tn-1", status: "completed" } } });
          }
          nl = buffered.indexOf("\n");
        }
      });
      const { harness } = makeHarness({
        registry, broker,
        launchRoot: async () => ({ root: fakeRoot, transport, supervisorPid: 4321 }),
      });
      try {
        const events = await withTimeout(collect(harness.startTurn(request).events), 5000, "Read response delivery");
        assert.ok(events.some(e => e.kind === "turn_finished"));
        assert.equal(replies.length, 1);
        assert.ok(replies[0]!.bytes < 4 * 1024 * 1024);
        const reply = rec(replies[0]!.frame.result);
        assert.equal(reply.success, true);
        const items = reply.contentItems as { type: string; text: string }[];
        assert.equal(items.length, 1);
        assert.equal(items[0]!.type, "inputText");
        assert.deepEqual(JSON.parse(items[0]!.text), {
          size: Buffer.byteLength(raw), content: expected, offset, linesReturned: lines, partialLastLine: partial, truncated,
        });
        assert.equal(calls, 1);
        assert.equal(registry.inFlightCallbackCount(), 0);
        assert.equal(registry.isPoisoned(), false);
        const replay = await broker.handleToolCall({ threadId: "th-1", turnId: "tn-1", callId: "read-296" }, "uzi_read", args, "root");
        assert.equal(replay.ok, true);
        if (replay.ok) assert.deepEqual(replay.output, { replay: true });
        assert.equal(calls, 1);
      } finally {
        transport.close();
        inbound.destroy();
        outbound.destroy();
      }
    });
  }
});

describe("CodexHarness: server→client tool-call routing", () => {
  it("routes an item/tool/call to the broker, admits via the registry, and replies via respond", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const request = makeRequest();
    const spawnCalls: { argv: readonly string[] }[] = [];
    const broker = new CodexCallbackBroker({
      registry,
      spawnCommand: async (argv, _opts) => {
        spawnCalls.push({ argv });
        return { code: 0, stdout: "ok", stderr: "" };
      },
      fileop: { op: async () => ({ ok: true, size: 0 }) },
      worktreePath: WORKSPACE,
      grants: renderCodexRun(request).leadGrants,
      delegate: async () => ({ ok: false, code: "x", message: "no" }),
      allowedRoles: new Set<string>(),
    });
    const { harness, transport } = makeHarness({ registry, broker });
    transport
      .push(threadStarted())
      .push(toolCall(7, "Bash", { command: "echo hello" }))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(request).events);

    // The tool-call is intercepted (activity). Issue #1583: it is ALSO projected as exactly one
    // lead tool started/finished frame pair, neither of which carries `signals`.
    const frames = events.filter((e) => e.kind === "frame");
    assert.equal(frames.length, 2);
    assert.deepEqual(
      frames.map((f) => (f.kind === "frame" ? f.items.map((i) => (i.kind === "tool" ? i.phase : i.kind)) : [])),
      [["started"], ["finished"]],
    );
    for (const f of frames) if (f.kind === "frame") assert.equal(f.signals, undefined);
    assert.equal(spawnCalls.length, 1);
    assert.deepEqual(spawnCalls[0]!.argv, ["/bin/sh", "-c", "echo hello"]);

    // Answered via transport.respond as a successful tool RESULT (not a JSON-RPC error).
    assert.equal(transport.responses.length, 1);
    const reply = transport.responses[0]!;
    assert.equal(reply.requestId, 7);
    const result = rec(rec(reply.response).result);
    assert.equal(result.success, true);
    assert.deepEqual(result.contentItems, [{ type: "inputText", text: JSON.stringify({ code: 0, stdout: "ok", stderr: "" }) }]);

    // The registry actually admitted + settled the callback: an identical replay of the
    // same (thread,turn,call) is served from the cached terminal, running NO second effect.
    const replay = await broker.handleToolCall(
      { threadId: "th-1", turnId: "tn-1", callId: "c1" },
      "Bash",
      { command: "echo hello" },
      "root",
    );
    assert.equal(replay.ok, true);
    if (replay.ok) assert.deepEqual(replay.output, { replay: true });
    assert.equal(spawnCalls.length, 1, "the replay ran no second shell effect");
  });

  it("a broker DENY is replied as a failed tool result (success:false), not a thrown error", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const request = makeRequest();
    const broker = new CodexCallbackBroker({
      registry,
      spawnCommand: async () => ({ code: 0, stdout: "", stderr: "" }),
      fileop: { op: async () => ({ ok: true }) },
      worktreePath: WORKSPACE,
      grants: renderCodexRun(request).leadGrants,
      delegate: async () => ({ ok: false, code: "x", message: "no" }),
      allowedRoles: new Set<string>(),
    });
    const { harness, transport } = makeHarness({ registry, broker });
    transport.push(threadStarted()).push(toolCall(9, "DangerTool", {})).push(turnCompleted("completed")).end();

    const events = await collect(harness.startTurn(request).events);
    const result = rec(rec(transport.responses[0]!.response).result);
    assert.equal(result.success, false);
    // Issue #1583: the denied call's projected finished frame is an error result.
    const finished = events.flatMap((e) => (e.kind === "frame" ? e.items : [])).find((i) => i.kind === "tool" && i.phase === "finished");
    assert.ok(finished && finished.kind === "tool" && finished.phase === "finished");
    assert.equal(finished.isError, true);
  });

  it("a tool-call SPOOFING a different threadId is denied fail-closed: the broker is never invoked", async () => {
    const seen: CallbackRuntimeId[] = [];
    const broker = stubBroker(async (rt) => {
      seen.push(rt);
      return { ok: true, output: {} };
    });
    const { harness, transport } = makeHarness({ broker });
    // A tool-call whose params SPOOF a different threadId is NOT the active root turn's, so
    // the broker is never asked to run an effect for it (finding 1: bind to the active turn).
    transport
      .push(threadStarted())
      .push(toolCall(1, "Bash", { command: "echo" }, "spoofed-thread"))
      .push(turnCompleted("completed"))
      .end();

    await collect(harness.startTurn(makeRequest()).events);
    assert.equal(seen.length, 0, "no effect ran for a spoofed thread");
    // The transport still receives a bounded FAILED tool result (never a JSON-RPC error).
    assert.equal(transport.responses.length, 1);
    assert.equal(rec(rec(transport.responses[0]!.response).result).success, false);
  });

  for (const method of ["account/login/refresh", "currentTime/read"]) {
  it(`refuses ${method} and never calls the broker`, async () => {
    let brokerCalls = 0;
    const broker = stubBroker(async () => {
      brokerCalls += 1;
      return { ok: true, output: {} };
    });
    const { harness, transport } = makeHarness({ broker });
    transport
      .push(threadStarted())
      .push(serverRequest(17, method))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(makeRequest()).events);
    assert.equal(brokerCalls, 0);
    assert.ok(events.some((event) => event.kind === "activity"));
    assert.equal(transport.respondAttempts, 1);
    assert.equal(transport.responses.length, 1);
    assert.deepEqual(transport.responses[0], {
      requestId: 17,
      response: { error: { code: -32601, message: "unsupported request" } },
    });
  });
  }
});

describe("CodexHarness: tool-call is bound fail-closed to the active (thread, turn)", () => {
  function spyBroker(): { broker: CodexCallbackBroker; calls: { rt: CallbackRuntimeId; origin: CallbackOrigin }[] } {
    const calls: { rt: CallbackRuntimeId; origin: CallbackOrigin }[] = [];
    const broker = stubBroker(async (rt, _name, _args, origin) => {
      calls.push({ rt, origin });
      return { ok: true, output: {} };
    });
    return { broker, calls };
  }

  /** Drive a single tool-call note through a full turn and return what the broker saw and
   *  what the transport was told to reply. */
  async function driveToolCall(
    note: CodexNotification,
  ): Promise<{ calls: { rt: CallbackRuntimeId; origin: CallbackOrigin }[]; transport: FakeTransport }> {
    const { broker, calls } = spyBroker();
    const { harness, transport } = makeHarness({ broker });
    transport.push(threadStarted()).push(note).push(turnCompleted("completed")).end();
    await collect(harness.startTurn(makeRequest()).events);
    return { calls, transport };
  }

  it("an ABSENT threadId is denied fail-closed: broker never invoked, success:false reply", async () => {
    const { calls, transport } = await driveToolCall(toolCallNoThread(1, "Bash", { command: "echo" }));
    assert.equal(calls.length, 0, "no effect ran for an absent thread id");
    assert.equal(transport.responses.length, 1);
    assert.equal(rec(rec(transport.responses[0]!.response).result).success, false);
  });

  it("a present-but-DIFFERENT threadId is denied fail-closed: broker never invoked", async () => {
    const { calls, transport } = await driveToolCall(toolCall(1, "Bash", { command: "echo" }, "different-thread"));
    assert.equal(calls.length, 0);
    assert.equal(rec(rec(transport.responses[0]!.response).result).success, false);
  });

  it("a STALE turnId (right thread, wrong turn) is denied fail-closed: broker never invoked", async () => {
    // The stale-turn root-signal latch: same root thread, but a turn that is no longer
    // active. It must run NO effect and reply failed (finding 1).
    const { calls, transport } = await driveToolCall(toolCall(1, "Bash", { command: "echo" }, "th-1", "stale-turn"));
    assert.equal(calls.length, 0, "a stale-turn callback runs no effect");
    assert.equal(rec(rec(transport.responses[0]!.response).result).success, false);
  });

  it("the ACTIVE (root thread, active turn) is served with origin=root", async () => {
    const { calls, transport } = await driveToolCall(toolCall(1, "Bash", { command: "echo" }, "th-1", "tn-1"));
    assert.equal(calls.length, 1);
    assert.equal(calls[0]!.origin, "root");
    assert.equal(calls[0]!.rt.threadId, "th-1");
    assert.equal(calls[0]!.rt.turnId, "tn-1");
    // A matched, successful callback replies success:true.
    assert.equal(rec(rec(transport.responses[0]!.response).result).success, true);
  });
});

describe("CodexHarness: trusted ROOT signal callbacks route scanned signals into a main frame (m2)", () => {
  // A REAL broker so dispatchSignal actually scans the signal (root grants include the signal
  // tools + isRoot:true), returning `{ok:true, output: scanned}` — the input the harness surfaces.
  function signalBroker(request: RunTurnRequest): CodexCallbackBroker {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    return new CodexCallbackBroker({
      registry,
      spawnCommand: async () => ({ code: 0, stdout: "", stderr: "" }),
      fileop: { op: async () => ({ ok: true }) },
      worktreePath: WORKSPACE,
      grants: renderCodexRun(request).leadGrants,
      delegate: async () => ({ ok: false, code: "x", message: "no" }),
      allowedRoles: new Set<string>(),
    });
  }

  it("a root submit_plan tool call yields a main-origin frame carrying the scanned signals (AND still replies)", async () => {
    const request = makeRequest({ phase: "plan" });
    const { harness, transport } = makeHarness({ broker: signalBroker(request) });
    transport
      .push(threadStarted())
      .push(toolCall(5, "submit_plan", { plan_md: "the plan body" }, "th-1", "tn-1", "c1"))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(request).events);
    const frames = events.filter((e) => e.kind === "frame");
    assert.equal(frames.length, 1, "exactly one signals frame for the accepted root signal");
    const frame = frames[0]!;
    assert.equal(frame.kind, "frame");
    if (frame.kind === "frame") {
      assert.deepEqual(frame.origin, { kind: "main" }, "the signals frame is main-origin (reducer folds only main)");
      assert.deepEqual(frame.items, [], "the signals frame carries no persisted output items");
      assert.equal(frame.signals?.plan, "the plan body", "the scanned plan rides the frame's signals");
      assert.equal(frame.sessionId, "th-1");
    }
    // The model reply still went out (success:true) IN ADDITION to the frame.
    assert.equal(transport.responses.length, 1);
    assert.equal(rec(rec(transport.responses[0]!.response).result).success, true);
  });

  it("a root signal_done tool call yields a main-origin frame carrying done:true", async () => {
    const request = makeRequest();
    const { harness, transport } = makeHarness({ broker: signalBroker(request) });
    transport
      .push(threadStarted())
      .push(toolCall(6, "signal_done", {}, "th-1", "tn-1", "c1"))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(request).events);
    const frame = events.find((e) => e.kind === "frame");
    assert.ok(frame, "a signals frame was emitted for a root signal_done");
    if (frame && frame.kind === "frame") {
      assert.deepEqual(frame.origin, { kind: "main" });
      assert.equal(frame.signals?.done, true, "signal_done folds to done:true");
    }
  });

  it("a non-signal tool call still yields activity, NO signals frame, and only its projected tool pair", async () => {
    const request = makeRequest();
    const { harness, transport } = makeHarness({ broker: signalBroker(request) });
    transport
      .push(threadStarted())
      .push(toolCall(7, "Bash", { command: "echo hi" }, "th-1", "tn-1", "c1"))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(request).events);
    const frames = events.filter((e) => e.kind === "frame");
    for (const f of frames) if (f.kind === "frame") assert.equal(f.signals, undefined, "a shell effect emits no signals frame");
    // Issue #1583: the only frames are the call's projected tool started/finished pair.
    assert.deepEqual(
      frames.map((f) => (f.kind === "frame" ? f.items.map((i) => (i.kind === "tool" ? i.phase : i.kind)) : [])),
      [["started"], ["finished"]],
    );
    // The tool call was still intercepted + replied once.
    assert.equal(transport.responses.length, 1);
    assert.equal(rec(rec(transport.responses[0]!.response).result).success, true);
  });

  it("a stale-turn OR foreign-thread signal is denied fail-closed: broker never reached, no main+signals frame", async () => {
    const request = makeRequest();
    const { harness, transport } = makeHarness({ broker: signalBroker(request) });
    transport
      .push(threadStarted())
      // right root thread, WRONG turn — denied before the broker, so no signals frame.
      .push(toolCall(8, "submit_plan", { plan_md: "sneaky stale plan" }, "th-1", "stale-turn", "c1"))
      // foreign thread — likewise denied before the broker.
      .push(toolCall(9, "signal_done", {}, "foreign-thread", "tn-1", "c2"))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(request).events);
    assert.equal(events.filter((e) => e.kind === "frame").length, 0, "no signals frame for a stale/foreign signal");
    // Both were denied with a FAILED tool result (not_active_turn), never reaching the broker.
    assert.equal(transport.responses.length, 2);
    for (const r of transport.responses) assert.equal(rec(rec(r.response).result).success, false);
  });
});

describe("CodexHarness: terminal provider fields are bounded + redacted", () => {
  it("a terminal never leaks a raw turn.error, a raw usage blob, and closes an unknown status", async () => {
    const { harness, transport } = makeHarness();
    // Assembled at runtime so no secret-shaped literal sits in the source (scanner-safe).
    const secret = ["sk", "redact", "PRETENDSECRET0123456789"].join("-");
    const hugeNested = { blob: "x".repeat(4096), inner: { deep: [1, 2, 3] } };
    const rawUsage = {
      input_tokens: 7, // finite, >= 0 → kept
      note: secret, // string → dropped
      breakdown: hugeNested, // nested object → dropped
      negative: -5, // negative → dropped
      naughty: Number.NaN, // NaN → dropped
      infinite: Number.POSITIVE_INFINITY, // Infinity → dropped
    };
    // A raw status OUTSIDE the closed allowlist, plus a secret-bearing raw turn.error.
    const note: CodexNotification = {
      kind: "turn_completed",
      method: "turn/completed",
      threadId: "th-1",
      turnId: "tn-1",
      status: "mystery-provider-status",
      params: {
        threadId: "th-1",
        turn: { id: "tn-1", status: "mystery-provider-status", error: secret, usage: rawUsage },
      },
    };
    transport.push(threadStarted()).push(note).end();

    const events = await collect(harness.startTurn(makeRequest()).events);
    const last = events.at(-1)!;
    assert.equal(last.kind, "turn_finished");
    if (last.kind !== "turn_finished") return;
    const terminal = last.terminal;

    // subtype collapses to the closed token; an unknown raw status is failed.
    assert.equal(terminal.outcome, "failed");
    assert.equal(terminal.subtype, "unknown");

    // errors are provider-text-free: derived only from the closed subtype, no secret.
    assert.deepEqual(terminal.errors, ["codex turn ended with status: unknown"]);
    assert.ok(!terminal.errors.join(" ").includes(secret), "no secret leaked into errors");

    // usage is a bounded numeric-only subset — never the raw object.
    assert.deepEqual(terminal.usage, { basis: "turn", tokens: {}, wire: { usage: { input_tokens: 7 } } });
    const usageJson = JSON.stringify(terminal.usage);
    assert.ok(!usageJson.includes(secret), "no secret leaked into usage");
    assert.ok(!usageJson.includes("blob"), "no raw nested blob retained in usage");

    // The deferred failure message is based on the CLOSED subtype, not the raw status/secret.
    const thrown = terminal.failure!.materialize();
    assert.match(thrown.failure.message, /codex turn failed: unknown/);
    assert.ok(!thrown.failure.message.includes("mystery-provider-status"), "no raw status in the failure message");
  });
});

describe("approved root planning policy refusal through RunRunner (#2321)", () => {
  it("reports the typed provider origin after successful worker setup", async () => {
    const api = new FakeApi("policy-test-worker");
    const url = await api.listen();
    const fx = makeFixture();
    let runs = 0;
    const log = nullLogger();
    const client = new WorkerClient(url, "policy-test-worker", "test", log, {
      sleep: async () => {}, terminalRetrySchedule: [1, 1],
    });
    const git = new GitCache(fx.dataDir, log, undefined, testGitCacheOptions());
    const fakeCodexExecutor: Executor = {
      async run(ctx) {
        runs++;
        assert.ok(ctx.worktreePath, "worker setup and clone succeeded before planning");
        const { harness, transport } = makeHarness();
        transport.push(threadStarted()).push(turnCompletedWithError("cyberPolicy")).end();
        const events = await collect(harness.startTurn(makeRequest({ phase: "plan" })).events);
        const last = events.at(-1)!;
        assert.equal(last.kind, "turn_finished", "provider turn reached its behavior seam");
        if (last.kind !== "turn_finished") throw new Error("missing terminal");
        throw last.terminal.failure!.materialize().original;
      },
    };
    const claim = makeClaim({
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    });
    try {
      await new RunRunner(client, git, () => ({ executor: fakeCodexExecutor }), log, 20, undefined, { pollMs: 5 }).execute(claim);
      assert.equal(runs, 1, "worker setup succeeded and planning ran once");
      const failed = api.states.filter(s => s.body.status === "failed").at(-1)?.body;
      assert.ok(failed);
      assert.equal(failed.fail_origin, "provider_policy_refusal");
      assert.equal(failed.failure_reason, "Codex provider safety-policy refusal (cyberPolicy)");
    } finally {
      try { await api.close(); } finally { fx.cleanup(); }
    }
  });
});

describe("CodexHarness: provider error classification folds into the terminal (PRD #1534)", () => {
  function terminalOf(events: HarnessEvent[]): HarnessTerminal {
    const last = events.at(-1)!;
    assert.equal(last.kind, "turn_finished");
    if (last.kind !== "turn_finished") throw new Error("expected a turn_finished terminal");
    return last.terminal;
  }

  it("captures a non-retrying scalar codex_error and folds it into the terminal (errors + message + category)", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(codexError("unauthorized"))
      .push(turnCompleted("failed"))
      .end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(terminal.errors, ["codex turn ended with status: failed (unauthorized)"]);
    const thrown = terminal.failure!.materialize();
    // `failure.message` is `original.message` verbatim; `original` is typed `unknown`.
    assert.match(thrown.failure.message, /codex turn failed: failed \(unauthorized\)/);
    assert.equal(thrown.failure.category, "authentication");
  });

  it("carries a TAGGED classification with a bounded http status into the message/errors/category", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(codexError({ httpConnectionFailed: { httpStatusCode: 503 } }))
      .push(turnCompleted("failed"))
      .end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(terminal.errors, ["codex turn ended with status: failed (httpConnectionFailed; http 503)"]);
    const thrown = terminal.failure!.materialize();
    assert.match(thrown.failure.message, /codex turn failed: failed \(httpConnectionFailed; http 503\)/);
    assert.equal(thrown.failure.category, "transport");
  });

  it("IGNORES a willRetry:true codex_error (byte-identical to no notification)", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(codexError("unauthorized", { willRetry: true }))
      .push(turnCompleted("failed"))
      .end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(terminal.errors, ["codex turn ended with status: failed"]);
    const thrown = terminal.failure!.materialize();
    assert.match(thrown.failure.message, /codex turn failed: failed$/);
    assert.equal(thrown.failure.category, "unknown");
  });

  it("IGNORES a codex_error bound to a STALE turn id (the identity gate is load-bearing)", async () => {
    // A codex_error carrying the active thread but a STALE turnId must NOT latch onto the
    // active turn — else a prior/foreign turn's error mislabels this one's failure_reason.
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(codexError("unauthorized", { turnId: "stale-turn" }))
      .push(turnCompleted("failed"))
      .end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(terminal.errors, ["codex turn ended with status: failed"]);
    const thrown = terminal.failure!.materialize();
    assert.match(thrown.failure.message, /codex turn failed: failed$/);
    assert.equal(thrown.failure.category, "unknown");
  });

  it("IGNORES a codex_error bound to a FOREIGN thread id (the identity gate is load-bearing)", async () => {
    // A codex_error for an unregistered foreign thread reaches mapNote (only registered child
    // sinks are demuxed away) and must NOT be captured onto the active root turn.
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(codexError("unauthorized", { threadId: "foreign-thread" }))
      .push(turnCompleted("failed"))
      .end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(terminal.errors, ["codex turn ended with status: failed"]);
    const thrown = terminal.failure!.materialize();
    assert.match(thrown.failure.message, /codex turn failed: failed$/);
    assert.equal(thrown.failure.category, "unknown");
  });

  it("FALLS BACK to the terminal turn.error.codexErrorInfo when there is no codex_error notification", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnCompletedWithError("usageLimitExceeded")).end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(terminal.errors, ["codex turn ended with status: failed (usageLimitExceeded)"]);
    const thrown = terminal.failure!.materialize();
    assert.match(thrown.failure.message, /codex turn failed: failed \(usageLimitExceeded\)/);
    assert.equal(thrown.failure.category, "rate_limit");
    assert.ok(thrown.original instanceof CodexTurnFailedError);
    assert.equal(thrown.original.failOrigin, "rate_limited");
  });

  it("#2360: only final usageLimitExceeded consumes subscription window evidence", async () => {
    for (const tag of ["usageLimitExceeded", "rateLimitExceeded", "sessionBudgetExceeded"]) {
      const { harness, transport } = makeHarness({ authMode: "subscription" });
      transport.push(threadStarted()).push({
        kind: "rate_limits_updated", method: "account/rateLimits/updated",
        rateLimits: { primary: { usedPercent: 100, windowDurationMins: 300, resetsAt: 2000000000 } }, params: {},
      }).push(turnCompletedWithError(tag)).end();
      const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
      const limit = classifyLimitEvidence(terminal.limitEvidence ?? { explicitExhaustion: false }, 1900000000000);
      const thrown = terminal.failure!.materialize(limit);
      if (tag === "usageLimitExceeded") {
        assert.deepEqual(terminal.limitEvidence, { explicitExhaustion: true,
          latest: { status: "rejected", resetsAtMs: 2000000000000, window: "five_hour" } });
        assert.ok(thrown.original instanceof LimitReachedError);
        assert.equal(thrown.original.message, "usage limit reached");
        assert.deepEqual(thrown.failure.limit, { resetsAtMs: 2000000000000, window: "five_hour" });
      } else {
        assert.equal(terminal.limitEvidence, undefined);
        assert.ok(thrown.original instanceof CodexTurnFailedError);
        assert.equal(thrown.original.failOrigin, undefined);
        assert.equal(thrown.failure.limit, undefined);
      }
    }
  });

  it("#2360: a new turn cannot reuse a previous turn snapshot", async () => {
    const { harness, transport } = makeHarness({ authMode: "subscription" });
    transport.push(threadStarted()).push({
      kind: "rate_limits_updated", method: "account/rateLimits/updated",
      rateLimits: { primary: { usedPercent: 100, windowDurationMins: 300, resetsAt: 2000000000 } }, params: {},
    }).push(turnCompleted("completed"));
    const first = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.equal(first.outcome, "success");
    assert.equal(first.limitEvidence, undefined);
    transport.push(turnCompletedWithError("usageLimitExceeded")).end();
    const second = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.equal(second.limitEvidence, undefined);
    const thrown = second.failure!.materialize();
    assert.ok(thrown.original instanceof CodexTurnFailedError);
    assert.equal(thrown.original.failOrigin, "rate_limited");
  });

  it("#2360: undefined auth and api key never admit subscription evidence", async () => {
    for (const authMode of [undefined, "api_key"] as const) {
      const { harness, transport } = makeHarness({ authMode });
      transport.push(threadStarted()).push({
        kind: "rate_limits_updated", method: "account/rateLimits/updated",
        rateLimits: { primary: { usedPercent: 100, windowDurationMins: 300, resetsAt: 2000000000 } }, params: {},
      }).push(turnCompletedWithError("usageLimitExceeded")).end();
      const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
      assert.equal(terminal.limitEvidence, undefined);
      const thrown = terminal.failure!.materialize();
      assert.ok(thrown.original instanceof CodexTurnFailedError);
      assert.equal(thrown.original.failOrigin, "rate_limited");
      assert.equal(thrown.failure.limit, undefined);
    }
  });

  it("issue #2099: the materialized failure is a CodexTurnFailedError carrying the closed classification", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnCompletedWithError("serverOverloaded")).end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    const thrown = terminal.failure!.materialize();
    assert.ok(thrown.original instanceof CodexTurnFailedError);
    assert.equal(thrown.original.message, "codex turn failed: failed (serverOverloaded)");
    assert.deepEqual(thrown.original.classification, { classification: "serverOverloaded", category: "transport" });
  });

  it("issue #2099: a failure with no classification carries none", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnCompleted("failed")).end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    const thrown = terminal.failure!.materialize();
    assert.ok(thrown.original instanceof CodexTurnFailedError);
    assert.equal(thrown.original.classification, undefined);
  });

  it("PRECEDENCE: a recognized terminal turn.error WINS over an UNRECOGNIZED notification classification", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      // The notification's codexErrorInfo is unrecognized → classification "unknown", which
      // must NOT mask the terminal's recognized "unauthorized".
      .push(codexError("totallyMadeUp"))
      .push(turnCompletedWithError("unauthorized"))
      .end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(terminal.errors, ["codex turn ended with status: failed (unauthorized)"]);
    const thrown = terminal.failure!.materialize();
    assert.match(thrown.failure.message, /codex turn failed: failed \(unauthorized\)/);
    assert.ok(!thrown.failure.message.includes("unknown"), "the notification's 'unknown' never surfaces");
    assert.equal(thrown.failure.category, "authentication");
  });

  it("NO-LEAK: a secret-shaped message/additionalDetails on the error frame never reaches the terminal", async () => {
    const { harness, transport } = makeHarness();
    // Assembled at runtime so no secret-shaped literal sits in the source (scanner-safe).
    const secretMsg = ["sk", "codexerr", "MUSTNOTLEAK7777"].join("-");
    const secretDetails = ["sk", "detail", "ALSOHIDDEN4242"].join("-");
    transport
      .push(threadStarted())
      .push(
        codexError(["madeUp", "provider", "tag"].join("-"), {
          extraError: { message: secretMsg, additionalDetails: secretDetails },
        }),
      )
      .push(turnCompleted("failed"))
      .end();

    const terminal = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    // The unrecognized codexErrorInfo collapses to the fixed "unknown" token; no raw text.
    assert.deepEqual(terminal.errors, ["codex turn ended with status: failed (unknown)"]);
    assert.equal(terminal.subtype, "failed");
    const thrown = terminal.failure!.materialize();
    for (const s of [secretMsg, secretDetails]) {
      assert.ok(!terminal.errors.join(" ").includes(s), "no secret leaked into errors");
      assert.ok(!terminal.subtype.includes(s), "no secret leaked into subtype");
      // `failure.message` is `original.message` verbatim; `original` is typed `unknown`.
      assert.ok(!thrown.failure.message.includes(s), "no secret leaked into the failure message");
    }
  });

  it("RESETS the captured classification across turns (a later turn with no codex_error is clean)", async () => {
    const { harness, transport } = makeHarness();
    // Turn 1 captures a classification from a non-retrying codex_error (queue NOT ended, so
    // the same single-consumer transport serves turn 2).
    transport.push(threadStarted()).push(codexError("unauthorized")).push(turnCompleted("failed"));
    const terminal1 = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(terminal1.errors, ["codex turn ended with status: failed (unauthorized)"]);

    // Turn 2 has NO codex_error — the prior classification must not leak into it.
    transport.push(turnCompleted("failed")).end();
    const terminal2 = terminalOf(await collect(harness.startTurn(makeRequest()).events));
    assert.deepEqual(terminal2.errors, ["codex turn ended with status: failed"]);
    const thrown2 = terminal2.failure!.materialize();
    assert.match(thrown2.failure.message, /codex turn failed: failed$/);
    assert.equal(thrown2.failure.category, "unknown");
  });
});

describe("CodexHarness: item content decode", () => {
  it("decodes a reasoning item to a thinking frame", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(completedItem({ type: "reasoning", text: "let me think" }))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(makeRequest()).events);
    const frame = events.find((e) => e.kind === "frame");
    assert.ok(frame && frame.kind === "frame", "the reasoning item decoded to a frame");
    assert.deepEqual(frame.items, [{ kind: "thinking", text: "let me think" }]);
  });

  it("decodes an unknown completed-item type to activity, never a frame (documents current behavior)", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(completedItem({ type: "mysteryItem", text: "???" }))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(makeRequest()).events);
    assert.equal(events.filter((e) => e.kind === "frame").length, 0);
    assert.deepEqual(
      events.map((e) => e.kind),
      ["initialized", "activity", "turn_finished"],
    );
  });
});

describe("CodexHarness: modelInitiated inclusion evidence (issue #1800)", () => {
  const modelInitiated = (events: HarnessEvent[]): number =>
    events.filter((e) => e.kind === "activity" && e.modelInitiated === true).length;
  const itemNote = (method: string, type: string, threadId: string): CodexNotification => ({
    kind: "activity",
    method,
    params: { threadId, item: { type } },
  });

  it("a root-thread item/started of a tool item type is marked modelInitiated; its item/completed is not", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(itemNote("item/started", "commandExecution", "th-1"))
      .push(itemNote("item/completed", "commandExecution", "th-1"))
      .push(turnCompleted("completed"))
      .end();
    assert.equal(modelInitiated(await collect(harness.startTurn(makeRequest()).events)), 1);
  });

  it("a root-thread item/completed alone (no start this turn) is not marked modelInitiated", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(itemNote("item/completed", "commandExecution", "th-1"))
      .push(itemNote("item/completed", "mcpToolCall", "th-1"))
      .push(turnCompleted("completed"))
      .end();
    assert.equal(modelInitiated(await collect(harness.startTurn(makeRequest()).events)), 0);
  });

  it("a child/foreign-thread commandExecution item is not marked modelInitiated", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(itemNote("item/started", "commandExecution", "child-thread"))
      .push(itemNote("item/completed", "commandExecution", "child-thread"))
      .push(turnCompleted("completed"))
      .end();
    assert.equal(modelInitiated(await collect(harness.startTurn(makeRequest()).events)), 0);
  });

  it("userMessage items and lifecycle / token-usage notifications stay unmarked", async () => {
    const { harness, transport } = makeHarness();
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(itemNote("item/started", "userMessage", "th-1"))
      .push(itemNote("item/completed", "userMessage", "th-1"))
      .push(tokenUsage({}, {}))
      .push(turnCompleted("completed"))
      .end();
    assert.equal(modelInitiated(await collect(harness.startTurn(makeRequest()).events)), 0);
  });
});

describe("CodexHarness: owner abort cancels a turn wedged in a broker callback", () => {
  it("an owner abort ENDS a turn wedged in a never-settling broker callback (no hang)", async () => {
    const ac = new AbortController();
    const { broker, entered } = wedgeBroker();
    const { harness, transport } = makeHarness({ broker });
    transport.push(threadStarted()).push(toolCall(1, "Bash", { command: "sleep 999" }));
    const iter = harness.startTurn(makeRequest({ signal: ac.signal })).events[Symbol.asyncIterator]();

    const first = await iter.next(); // setup → initialized
    assert.equal(first.done, false);
    assert.equal((first.value as HarnessEvent).kind, "initialized");

    // Drives the tool-call. Issue #1583: its projected `started` tool frame is yielded while
    // the broker callback is still pending; the NEXT pull wedges in that callback.
    const started = await withTimeout(iter.next(), 1000, "the projected started frame");
    assert.equal((started.value as HarnessEvent).kind, "frame");
    await withTimeout(entered, 1000, "the broker callback to be entered");
    const pending = iter.next(); // wedges in the broker callback

    ac.abort(); // owner abort WHILE the callback is pending
    const ended = await withTimeout(pending, 1000, "the stream to end on abort");
    assert.equal(ended.done, true, "the stream ends promptly instead of wedging");
  });

  it("requestStop ENDS a turn wedged in a never-settling broker callback (no hang)", async () => {
    const { broker, entered } = wedgeBroker();
    const { harness, transport } = makeHarness({ broker });
    transport.push(threadStarted()).push(toolCall(1, "Bash", { command: "sleep 999" }));
    const turn = harness.startTurn(makeRequest());
    const iter = turn.events[Symbol.asyncIterator]();

    await iter.next(); // initialized
    // Issue #1583: the projected `started` tool frame comes first; the next pull wedges.
    const started = await withTimeout(iter.next(), 1000, "the projected started frame");
    assert.equal((started.value as HarnessEvent).kind, "frame");
    await withTimeout(entered, 1000, "the broker callback to be entered");
    const pending = iter.next(); // wedges in the broker callback

    turn.requestStop("cancel");
    const ended = await withTimeout(pending, 1000, "the stream to end on requestStop");
    assert.equal(ended.done, true);
    // A best-effort interrupt (a cancellation REQUEST, not a kill) was still issued.
    assert.ok(transport.requests.some((r) => r.method === "turn/interrupt"), "an interrupt was requested");
  });

  it("a broker reply that settles AFTER abort+close is dropped, never thrown into the closed transport", async () => {
    const ac = new AbortController();
    const { broker, entered, release } = wedgeBroker();
    const { harness, transport } = makeHarness({ broker });
    transport.push(threadStarted()).push(toolCall(5, "Bash", { command: "echo" }));
    const turn = harness.startTurn(makeRequest({ signal: ac.signal }));
    const iter = turn.events[Symbol.asyncIterator]();

    await iter.next(); // initialized
    // Issue #1583: the projected `started` tool frame comes first; the next pull wedges.
    const started = await withTimeout(iter.next(), 1000, "the projected started frame");
    assert.equal((started.value as HarnessEvent).kind, "frame");
    await withTimeout(entered, 1000, "the broker callback to be entered");
    const pending = iter.next(); // wedges

    ac.abort();
    const ended = await withTimeout(pending, 1000, "the stream to end on abort");
    assert.equal(ended.done, true);
    await turn.close(); // tears the transport down under the still-pending callback

    // The broker finally settles AFTER close: routeToolCall's guard must swallow the
    // closed-transport throw so no reply is delivered and nothing throws.
    release({ ok: true, output: {} });
    await tick();
    await tick();
    assert.equal(transport.respondAttempts, 1, "the broker reply reached the guarded respond seam once");
    assert.equal(transport.responses.length, 0, "the undeliverable reply was dropped");
  });
});

const label = (e: HarnessEvent): string => e.kind !== "frame" ? e.kind :
  e.items.some(i => i.kind === "tool") ? e.items.filter(i => i.kind === "tool").map(i => i.phase).join(",") :
    JSON.stringify(e).includes("commentary") ? "commentary" : "signal";

async function pullUntil(iter: AsyncIterator<HarnessEvent>, wanted: string): Promise<HarnessEvent[]> {
  const events: HarnessEvent[] = [];
  for (let n = 0; n < 20; n++) {
    const step = await withTimeout(iter.next(), 1000, wanted);
    assert.equal(step.done, false);
    events.push(step.value!);
    if (label(step.value!) === wanted) return events;
  }
  assert.fail(`missing ${wanted}`);
}

describe("CodexHarness: commentary during callbacks (issue #2283)", () => {
  it("deep queued B preserves dispatch and exactly one reply", async () => {
    const { broker: wedged, release } = wedgeBroker();
    let expectedArgs: unknown;
    let receivedArgs: unknown;
    const calls: string[] = [];
    const broker = stubBroker((rt, name, args, origin) => {
      calls.push(String(rec(args).command));
      if (calls.length === 2) receivedArgs = args;
      return calls.length === 1 ? wedged.handleToolCall(rt, name, args, origin) :
        Promise.resolve({ ok: true, output: {} });
    });
    const { harness, transport } = makeHarness({ broker });
    const ac = new AbortController();
    transport.push(toolCall(1, "Bash", { command: "A" }, "th-1", "tn-1", "A"));
    const iter = harness.startTurn(makeRequest({ signal: ac.signal })).events[Symbol.asyncIterator]();
    let observed: unknown;
    try {
      await iter.next();
      await pullUntil(iter, "started");
      let deep: unknown = null;
      for (let i = 0; i < 20000; i++) deep = { a: deep };
      expectedArgs = { command: "B", deep };
      transport.push(toolCall(2, "Bash", expectedArgs, "th-1", "tn-1", "B"))
        .push(agentMessage("commentary C"));
      try {
        await pullUntil(iter, "commentary");
        assert.deepEqual(calls, ["A"]);
        release({ ok: true, output: {} });
        transport.push(turnCompleted());
        await collect({ [Symbol.asyncIterator]: () => iter });
      } catch (error) { observed = error; }
      release({ ok: true, output: {} });
      await tick();

      assert.equal(observed, undefined, "queued deep JSON must not crash or lose request B reply");
      assert.deepEqual(calls, ["A", "B"]);
      assert.deepEqual(transport.responses.map(r => r.requestId), [1, 2]);
      assert.equal(transport.respondAttempts, 2);
      assert.equal(receivedArgs, expectedArgs, "broker receives the unchanged raw arguments");
    } finally {
      ac.abort(); release({ ok: true, output: {} });
      await iter.return?.(); await harness.close();
    }
  });
  for (const failure of ["EOF", "read failure"] as const) {
    it(failure + " at A activity yield refuses B without brokerage", async () => {
      const { broker: wedged, release } = wedgeBroker();
      const calls: string[] = [];
      const broker = stubBroker((rt, name, args, origin) => {
        calls.push(String(rec(args).command));
        return calls.length === 1 ? wedged.handleToolCall(rt, name, args, origin) :
          Promise.resolve({ ok: true, output: {} });
      });
      const { harness, transport } = makeHarness({ broker });
      transport.push(toolCall(1, "Bash", { command: "A" }, "th-1", "tn-1", "A"));
      const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
      try {
        await iter.next(); await pullUntil(iter, "started");
        transport.push(toolCall(2, "Bash", { command: "B" }, "th-1", "tn-1", "B"))
          .push(agentMessage("commentary C"));
        await pullUntil(iter, "commentary");
        release({ ok: true, output: {} });
        await pullUntil(iter, "finished"); await pullUntil(iter, "activity");
        assert.deepEqual(calls, ["A"], "B remains deferred at the activity suspension");
        if (failure === "EOF") transport.end(); else transport.fail(new Error("tester-1 notification failure"));
        await tick();
        const events: string[] = [];
        let observed: unknown;
        try {
          for (let n = 0; n < 20; n++) {
            const step = await withTimeout(iter.next(), 1000, "read termination");
            if (step.done) break;
            events.push(label(step.value!));
          }
        } catch (error) { observed = error; }
        await tick();

        assert.ok(observed instanceof Error);
        assert.match(observed.message, failure === "EOF" ? /stream ended before turn completion/ : /tester-1 notification failure/);
        assert.deepEqual(calls, ["A"], "B must not be brokered after ready EOF/read failure");
        assert.equal(transport.responses.filter(r => r.requestId === 2).length, 1);
        assert.deepEqual(transport.responses.find(r => r.requestId === 2)?.response,
          { result: { success: false, contentItems: [{ type: "inputText", text: "callback does not match the active turn" }] } });
      } finally {
        release({ ok: true, output: {} });
        await iter.return?.(); await harness.close();
      }
    });
  }

  for (const ordering of ["notification-first", "completion-first", "co-ready notification-first", "co-ready completion-first"] as const) {
    it(`${ordering}: retained read and finished projection follow readiness order`, async () => {
      const ac = new AbortController();
      const { broker, release } = wedgeBroker();
      const { harness, transport } = makeHarness({ broker });
      transport.push(toolCall(1, "Bash", { command: "echo blocked" }));
      const iter = harness.startTurn(makeRequest({ signal: ac.signal })).events[Symbol.asyncIterator]();
      try {
        await iter.next();
        await pullUntil(iter, "started");
        let first: HarnessEvent[];
        if (ordering.includes("notification-first")) {
          transport.push(agentMessage("commentary"));
          if (ordering.startsWith("co-ready")) {
            await tick();
            release({ ok: true, output: {} });
            await tick();
          }
          first = await pullUntil(iter, "commentary");
          assert.ok(!first.some(e => label(e) === "finished"));
          if (!ordering.startsWith("co-ready")) {
            assert.equal(transport.responses.length, 0);
            release({ ok: true, output: {} });
          }
          await pullUntil(iter, "finished");
          await pullUntil(iter, "activity");
        } else {
          release({ ok: true, output: {} });
          await tick();
          if (ordering.startsWith("co-ready")) transport.push(agentMessage("commentary"));
          first = await pullUntil(iter, "finished");
          assert.ok(!first.some(e => label(e) === "commentary"));
          if (!ordering.startsWith("co-ready")) {
            await pullUntil(iter, "activity");
            transport.push(agentMessage("commentary"));
          }
          await pullUntil(iter, "commentary");
        }
        transport.push(turnCompleted());
        const rest = await collect({ [Symbol.asyncIterator]: () => iter });
        assert.equal(rest.at(-1)?.kind, "turn_finished");
        assert.equal(transport.responses.length, 1);
      } finally {
        ac.abort();
        release({ ok: true, output: {} });
        await iter.return?.();
        await harness.close();
      }
    });
  }

  for (const longRunning of [false, true]) {
  for (const readBehindEffects of [false, true]) {
  it(`#2903 M2: real broker observes read ${readBehindEffects ? "behind" : "before"} queued pwd checkpoint during held Bash (long_running=${longRunning})`, async () => {
    const ac = new AbortController();
    const request = makeRequest({ signal: ac.signal });
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    let release!: () => void;
    let markEntered!: () => void;
    const held = new Promise<void>(resolve => { release = resolve; });
    const entered = new Promise<void>(resolve => { markEntered = resolve; });
    let raw = "gate progress fixture\n";
    const order: string[] = [];
    let fileops = 0;
    const broker = new CodexCallbackBroker({
      registry,
      spawnCommand: async (argv, opts) => {
        assert.deepEqual(argv.slice(0, 2), ["/bin/sh", "-c"]);
        assert.equal(opts.cwd, WORKSPACE);
        order.push(`spawn:${argv[2]}`);
        if (argv[2] === "echo held-2903") {
          assert.equal(opts.long_running, longRunning);
          markEntered();
          await held;
          return { code: 0, stdout: "held effect complete\n", stderr: "" };
        }
        assert.equal(argv[2], "pwd");
        return { code: 0, stdout: WORKSPACE + "\n", stderr: "" };
      },
      fileop: { op: async operation => {
        assert.deepEqual(operation, { op: "read", path: "progress.txt" });
        fileops++;
        order.push("read");
        return { ok: true, size: Buffer.byteLength(raw), data: Buffer.from(raw).toString("base64") };
      } },
      worktreePath: WORKSPACE,
      grants: renderCodexRun(request).leadGrants,
      delegate: async () => ({ ok: false, code: "unused", message: "no delegation" }),
      allowedRoles: new Set<string>(),
    });
    const { harness, transport } = makeHarness({ registry, broker });
    transport.onRespond = id => { order.push(`reply:${id}`); };
    transport.push(toolCall(29030, "uzi_bash", { command: "echo held-2903", long_running: longRunning }, "th-1", "tn-1", "held-2903"));
    const iter = harness.startTurn(request).events[Symbol.asyncIterator]();
    const events: HarnessEvent[] = [];
    try {
      events.push((await iter.next()).value!);
      events.push(...await pullUntil(iter, "started"));
      await withTimeout(entered, 1000, "held command entry");
      if (longRunning) raw += "gate progress grew\n";
      if (!readBehindEffects) transport.push(toolCall(29031, "uzi_read", { path: "progress.txt" }, "th-1", "tn-1", "read-2903"));
      transport.push(toolCall(29032, "uzi_bash", { command: "pwd" }, "th-1", "tn-1", "pwd-2903"))
        .push(toolCall(29033, "checkpoint", {}, "th-1", "tn-1", "checkpoint-2903"));
      if (readBehindEffects) transport.push(toolCall(29031, "uzi_read", { path: "progress.txt" }, "th-1", "tn-1", "read-2903"));
      transport
        // Consuming this frame proves the reader encountered every queued request.
        .push(agentMessage("commentary after all #2903 requests"));
      events.push(...await pullUntil(iter, "commentary"));
      await tick();
      assert.deepEqual(order, ["spawn:echo held-2903", "read", "reply:29031"]);
      assert.equal(transport.responses.length, 1);
      assert.equal(fileops, 1);
      assert.equal(events.some(e => e.kind === "frame" && e.signals?.checkpoint), false);
      assert.equal(registry.inFlightCallbackCount(), 1);

      release();
      transport.push(turnCompleted());
      events.push(...await withTimeout(collect({ [Symbol.asyncIterator]: () => iter }), 5000, "#2903 FIFO completion"));
      assert.deepEqual(order, [
        "spawn:echo held-2903", "read", "reply:29031", "reply:29030",
        "spawn:pwd", "reply:29032", "reply:29033",
      ]);
      assert.deepEqual(transport.responses.map(r => r.requestId), [29031, 29030, 29032, 29033]);
      const outputs = transport.responses.map(({ response }) => {
        const result = rec(rec(response).result);
        assert.equal(result.success, true);
        const items = result.contentItems as { type: string; text: string }[];
        assert.equal(items.length, 1);
        assert.equal(items[0]!.type, "inputText");
        return rec(JSON.parse(items[0]!.text));
      });
      assert.deepEqual(outputs[0], {
        size: Buffer.byteLength(raw), content: raw, offset: 1,
        linesReturned: longRunning ? 2 : 1, partialLastLine: false, truncated: false,
      });
      assert.deepEqual(outputs[2], { code: 0, stdout: WORKSPACE + "\n", stderr: "" });
      assert.deepEqual(outputs[3], { checkpoint: true });
      const checkpointFrames = events.filter(e => e.kind === "frame" && e.signals?.checkpoint);
      assert.equal(checkpointFrames.length, 1);
      const terminal = events.at(-1);
      assert.equal(terminal?.kind, "turn_finished");
      assert.ok(terminal?.kind === "turn_finished");
      assert.equal(terminal.terminal.outcome, "success");
      assert.equal(fileops, 1);
      assert.equal(registry.inFlightCallbackCount(), 0);
      assert.equal(registry.isPoisoned(), false);
    } finally {
      release();
      ac.abort();
      await tick();
      await iter.return?.();
      await harness.close();
    }
  });

  }
  }

  it("A blocked, B queued, C commentary: effects stay serial and terminal waits without reading beyond it", async () => {
    const ac = new AbortController();
    let releaseA!: (r: CallbackResult) => void;
    let releaseB!: (r: CallbackResult) => void;
    const a = new Promise<CallbackResult>(r => { releaseA = r; });
    const b = new Promise<CallbackResult>(r => { releaseB = r; });
    const calls: unknown[] = [];
    const broker = stubBroker((_rt, _tool, args) => {
      calls.push(args);
      return calls.length === 1 ? a : b;
    });
    const { harness, transport } = makeHarness({ broker });
    let reads = 0;
    let reading = false;
    const notifications = transport.notifications.bind(transport);
    transport.notifications = () => {
      const notes = notifications();
      return {
        next: () => {
          assert.equal(reading, false, "notification reads never overlap");
          reading = true;
          reads++;
          const pending = notes.next();
          void pending.then(() => { reading = false; }, () => { reading = false; });
          return pending;
        },
        return: notes.return?.bind(notes),
        [Symbol.asyncIterator]() { return this; },
      };
    };
    transport.push(toolCall(1, "Bash", { command: "A" }, "th-1", "tn-1", "A"));
    const iter = harness.startTurn(makeRequest({ signal: ac.signal })).events[Symbol.asyncIterator]();
    let pending: Promise<IteratorResult<HarnessEvent>> | undefined;
    try {
      await iter.next();
      await pullUntil(iter, "started");
      transport.push(toolCall(2, "Bash", { command: "B" }, "th-1", "tn-1", "B"))
        .push(agentMessage("commentary C"))
        .push(turnCompleted("completed", undefined, "th-1", "stale"))
        .push(turnCompleted("completed", undefined, "foreign", "tn-1"))
        .push(agentMessage("commentary after stale"))
        .push(turnCompleted())
        .push(toolCall(3, "Bash", { command: "beyond terminal" }));
      await pullUntil(iter, "commentary");
      assert.deepEqual(calls, [{ command: "A" }]);
      await pullUntil(iter, "commentary");
      pending = iter.next();
      await tick();
      assert.equal(transport.responses.length, 0);
      releaseA({ ok: true, output: {} });
      const finished = await withTimeout(pending, 1000, "A finished");
      assert.equal(label(finished.value!), "finished");
      await pullUntil(iter, "started");
      assert.deepEqual(calls, [{ command: "A" }, { command: "B" }]);
      pending = iter.next();
      await tick();
      assert.equal(transport.responses.length, 1);
      releaseB({ ok: true, output: {} });
      assert.equal(label((await pending).value!), "finished");
      const rest = await collect({ [Symbol.asyncIterator]: () => iter });
      assert.deepEqual(rest.map(label), ["activity", "turn_finished"]);
      assert.deepEqual(transport.responses.map(r => r.requestId), [1, 2]);
      assert.equal(reads, 7, "the read stops at the active terminal, leaving the next request unread");
    } finally {
      ac.abort();
      releaseA({ ok: true, output: {} });
      releaseB({ ok: true, output: {} });
      await pending?.catch(() => {});
      await iter.return?.();
      await harness.close();
    }
  });

  it("throwing callback completes its pair and reply before dispatching a queued sibling", async () => {
    let rejectA!: (error: Error) => void;
    const a = new Promise<CallbackResult>((_resolve, reject) => { rejectA = reject; });
    let calls = 0;
    const broker = stubBroker(() => ++calls === 1 ? a : Promise.resolve({ ok: true, output: {} }));
    const { harness, transport } = makeHarness({ broker });
    transport.push(toolCall(1, "Bash", {}, "th-1", "tn-1", "A"));
    const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
    try {
      await iter.next();
      await pullUntil(iter, "started");
      transport.push(toolCall(2, "Bash", {}, "th-1", "tn-1", "B")).push(agentMessage("commentary"));
      await pullUntil(iter, "commentary");
      assert.equal(calls, 1);
      rejectA(new Error("broker threw"));
      transport.push(turnCompleted());
      const events = await collect({ [Symbol.asyncIterator]: () => iter });
      assert.deepEqual(events.map(label), ["finished", "activity", "started", "finished", "activity", "turn_finished"]);
      const error = events[0];
      assert.ok(error?.kind === "frame");
      assert.deepEqual(error.items.map(i => i.kind === "tool" && i.phase === "finished" ? [i.phase, i.isError, i.output] : []),
        [["finished", true, "the callback failed"]]);
      assert.equal(calls, 2);
      assert.deepEqual(transport.responses.map(r => r.requestId), [1, 2]);
    } finally {
      rejectA(new Error("cleanup"));
      await iter.return?.();
      await harness.close();
    }
  });

  for (const stop of ["abort", "requestStop"] as const) {
    for (const barrier of [false, true]) {
      it(`${stop} while ${barrier ? "terminal waits" : "reading"} refuses queued requests and isolates the next turn`, async () => {
        let turns = 0;
        const transport = new FakeTransport(method => method === "turn/start" ? { turn: { id: `tn-${++turns}` } } : defaultResponder(method));
        const ac = new AbortController();
        const { broker, release } = wedgeBroker();
        const { harness } = makeHarness({ broker, transport });
        transport.push(toolCall(1, "Bash", {}));
        const turn = harness.startTurn(makeRequest({ signal: ac.signal }));
        const iter = turn.events[Symbol.asyncIterator]();
        let pending: Promise<IteratorResult<HarnessEvent>> | undefined;
        try {
          await iter.next();
          await pullUntil(iter, "started");
          transport.push(toolCall(2, "Bash", {})).push(agentMessage("commentary queued"));
          await pullUntil(iter, "commentary");
          if (barrier) transport.push(turnCompleted());
          pending = iter.next();
          await tick();
          if (stop === "abort") ac.abort(); else await turn.requestStop("cancel");
          assert.equal((await withTimeout(pending, 1000, "stop")).done, true);
          assert.deepEqual(transport.responses.map(r => r.requestId), [2]);
          assert.deepEqual(transport.responses[0]?.response, { result: { success: false, contentItems: [{ type: "inputText", text: "callback does not match the active turn" }] } });
          const next = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
          const nextPull = next.next();
          await tick();
          release({ ok: true, output: {} });
          await tick();
          transport.push(turnCompleted("completed", undefined, "th-1", "tn-2"));
          const nextEvents: HarnessEvent[] = [];
          for (let step = await nextPull; !step.done; step = await next.next()) nextEvents.push(step.value);
          assert.ok(!nextEvents.some(e => e.kind === "frame"));
          assert.deepEqual(transport.responses.map(r => r.requestId), [2, 1]);
        } finally {
          ac.abort();
          release({ ok: true, output: {} });
          await pending?.catch(() => {});
          await iter.return?.();
          await harness.close();
        }
      });
    }
  }

  for (const failure of ["EOF", "read failure", "iterator teardown"] as const) {
    it(`${failure} refuses the FIFO and suppresses late callback output`, async () => {
      const { broker, release } = wedgeBroker();
      const { harness, transport } = makeHarness({ broker });
      transport.push(toolCall(1, "Bash", {}));
      const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
      try {
        await iter.next();
        await pullUntil(iter, "started");
        transport.push(toolCall(2, "Bash", {})).push(agentMessage("commentary"));
        await pullUntil(iter, "commentary");
        if (failure === "iterator teardown") {
          await iter.return?.();
        } else {
          const pending = iter.next();
          if (failure === "EOF") transport.end(); else transport.fail(new Error("test notification failure"));
          await assert.rejects(withTimeout(pending, 1000, "prompt read failure"),
            failure === "EOF" ? /stream ended before turn completion/ : /test notification failure/);
        }
        assert.deepEqual(transport.responses.map(r => r.requestId), [2]);
        assert.deepEqual(transport.responses[0]?.response, { result: { success: false, contentItems: [{ type: "inputText", text: "callback does not match the active turn" }] } });
        release({ ok: true, output: {} });
        await tick();
        assert.deepEqual(transport.responses.map(r => r.requestId), [2, 1]);
        assert.equal((await iter.next()).done, true);
      } finally {
        release({ ok: true, output: {} });
        await iter.return?.();
        await harness.close();
      }
    });
  }

  for (const encoding of ["UTF8", "escaped"] as const) {
    for (const extra of [0, 1]) {
      it(`${encoding} serialized queue bytes at 64MiB + ${extra} respect the exact boundary`, async () => {
        const ac = new AbortController();
        const { broker: wedged, release } = wedgeBroker();
        let calls = 0;
        const broker = stubBroker((rt, name, args, origin) => {
          calls++;
          return wedged.handleToolCall(rt, name, args, origin);
        });
        const { harness, transport } = makeHarness({ broker });
        transport.push(toolCall(1, "Bash", {}));
        const iter = harness.startTurn(makeRequest({ signal: ac.signal })).events[Symbol.asyncIterator]();
        try {
          await iter.next();
          await pullUntil(iter, "started");
          const pattern = encoding === "UTF8" ? "é漢😀" : "\"\\\n\t\u0001\ud800";
          const first = toolCall(2, "Bash", {
            data: pattern.repeat(1024 * 1024),
            omitted: undefined,
            nested: { omitted: undefined, "é\"": [undefined, null, true, false, 1.5, -0, 1e30] },
          });
          const second = toolCall(3, "Bash", { data: "", omitted: undefined });
          // The oracle uses the actual normalized notification, including omitted fields,
          // escaped keys, surrogate handling, array nulls and notification envelope.
          const overhead = Buffer.byteLength(JSON.stringify(first), "utf8") + Buffer.byteLength(JSON.stringify(second), "utf8");
          rec(rec(second.params).arguments).data = "x".repeat(64 * 1024 * 1024 - overhead + extra);
          assert.equal(Buffer.byteLength(JSON.stringify(first), "utf8") + Buffer.byteLength(JSON.stringify(second), "utf8"),
            64 * 1024 * 1024 + extra);
          transport.push(first).push(second).push(agentMessage("commentary"));
          if (extra) {
            await assert.rejects(withTimeout(iter.next(), 10000, "byte boundary refusal"), /deferred request queue overflow/);
          } else {
            await pullUntil(iter, "commentary");
            assert.equal(transport.responses.length, 0, "both requests fit exactly");
            ac.abort();
            await iter.next();
          }
          assert.equal(calls, 1);
          assert.deepEqual(transport.responses.map(r => r.requestId).sort(), [2, 3]);
          assert.equal(transport.respondAttempts, 2);
          for (const response of transport.responses) assert.deepEqual(response.response,
            { result: { success: false, contentItems: [{ type: "inputText", text: "callback does not match the active turn" }] } });
        } finally {
          ac.abort();
          release({ ok: true, output: {} });
          await iter.return?.();
          await harness.close();
        }
      });
    }
  }

  it("accounting failure refuses the current request and queued siblings once", async () => {
    const { broker: wedged, release } = wedgeBroker();
    let calls = 0;
    const broker = stubBroker((rt, name, args, origin) => {
      calls++;
      return wedged.handleToolCall(rt, name, args, origin);
    });
    const { harness, transport } = makeHarness({ broker });
    transport.push(toolCall(1, "Bash", {}));
    const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
    try {
      await iter.next();
      await pullUntil(iter, "started");
      const cyclic: Record<string, unknown> = {};
      cyclic.self = cyclic;
      transport.push(toolCall(2, "Bash", {})).push(toolCall(3, "Bash", cyclic));
      await assert.rejects(withTimeout(iter.next(), 1000, "accounting failure"), /deferred request accounting failed/);
      assert.equal(calls, 1);
      assert.deepEqual(transport.responses.map(r => r.requestId).sort(), [2, 3]);
      assert.equal(transport.respondAttempts, 2);
      for (const response of transport.responses) assert.deepEqual(response.response,
        { result: { success: false, contentItems: [{ type: "inputText", text: "callback does not match the active turn" }] } });
      release({ ok: true, output: {} });
      await tick();
      assert.equal(transport.respondAttempts, 3);
    } finally {
      release({ ok: true, output: {} });
      await iter.return?.();
      await harness.close();
    }
  });

  for (const limit of ["count", "bytes"] as const) {
    it(`${limit} overflow refuses overflow and every undispatched request exactly once`, async () => {
      let calls = 0;
      const { broker: wedged, release } = wedgeBroker();
      const broker = stubBroker((rt, name, args, origin) => {
        calls++;
        return wedged.handleToolCall(rt, name, args, origin);
      });
      const { harness, transport } = makeHarness({ broker });
      transport.push(toolCall(1, "Bash", {}));
      const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
      try {
        await iter.next();
        await pullUntil(iter, "started");
        const count = limit === "count" ? 4097 : 2;
        for (let n = 0; n < count; n++)
          transport.push(toolCall(n + 2, "Bash", limit === "bytes" ? { data: "x".repeat(32 * 1024 * 1024) } : {}));
        await assert.rejects(withTimeout(iter.next(), 10000, "queue overflow"), /deferred request queue overflow/);
        assert.equal(calls, 1);
        assert.equal(transport.responses.length, count);
        assert.equal(new Set(transport.responses.map(r => r.requestId)).size, count);
        for (const response of transport.responses) assert.deepEqual(response.response, { result: { success: false, contentItems: [{ type: "inputText", text: "callback does not match the active turn" }] } });
        release({ ok: true, output: {} });
        await tick();
        assert.equal(transport.responses.length, count + 1);
      } finally {
        release({ ok: true, output: {} });
        await iter.return?.();
        await harness.close();
      }
    });
  }

  for (const accepted of [true, false]) {
    it(`blocked signal with queued request permits commentary; ${accepted ? "accepted" : "denied"} signal folds once`, async () => {
      const { broker: wedged, entered, release } = wedgeBroker();
      let calls = 0;
      const broker = stubBroker((rt, name, args, origin) => ++calls === 1 ?
        wedged.handleToolCall(rt, name, args, origin) : Promise.resolve({ ok: true, output: {} }));
      const { harness, transport } = makeHarness({ broker });
      transport.push(toolCall(1, "signal_done", {}));
      const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
      let pending: Promise<IteratorResult<HarnessEvent>> | undefined;
      try {
        await iter.next();
        pending = iter.next();
        await entered;
        transport.push(toolCall(2, "Bash", {}, "th-1", "tn-1", "B")).push(agentMessage("commentary during signal"));
        assert.equal(label((await withTimeout(pending, 1000, "signal commentary")).value!), "commentary");
        assert.equal(calls, 1);
        assert.equal(transport.responses.length, 0);
        release(accepted ? { ok: true, output: { done: true } } : { ok: false, code: "denied", message: "denied" });
        transport.push(turnCompleted());
        const events = await collect({ [Symbol.asyncIterator]: () => iter });
        assert.equal(events.filter(e => e.kind === "frame" && e.signals !== undefined).length, accepted ? 1 : 0);
        assert.equal(events.filter(e => label(e) === "started").length, 1);
        assert.equal(events.filter(e => label(e) === "finished").length, 1);
        assert.deepEqual(transport.responses.map(r => r.requestId), [1, 2]);
      } finally {
        release({ ok: false, code: "denied", message: "cleanup" });
        await pending?.catch(() => {});
        await iter.return?.();
        await harness.close();
      }
    });
  }

  it("direct commentary emits while Bash remains blocked", async () => {
    const ac = new AbortController();
    const { broker, entered, release } = wedgeBroker();
    const { harness, transport } = makeHarness({ broker });
    transport.push(toolCall(2283, "Bash", { command: "echo blocked" }));
    const iter = harness.startTurn(makeRequest({ signal: ac.signal })).events[Symbol.asyncIterator]();
    let pull: Promise<IteratorResult<HarnessEvent>> | undefined;
    try {
      await iter.next();
      const started = await withTimeout(iter.next(), 1000, "started projection");
      assert.ok(started.value?.kind === "frame");
      await entered;
      pull = iter.next();
      transport.push(agentMessage("commentary while blocked"));
      const commentary = await withTimeout(pull, 300, "issue #2283 commentary while callback is unreleased");
      assert.ok(commentary.value?.kind === "frame");
      assert.match(JSON.stringify(commentary.value), /commentary while blocked/);
      assert.equal(transport.responses.length, 0);
    } finally {
      ac.abort();
      release({ ok: true, output: {} });
      await pull?.catch(() => {});
      await iter.return?.();
      await harness.close();
    }
  });
});


describe("#2903 M2: observation scheduling", () => {
  function latch() {
    let release!: () => void;
    const promise = new Promise<void>(resolve => { release = resolve; });
    return { promise, release };
  }

  async function fixture(effect: "Bash" | "Search" = "Bash", denyRead = false) {
    const shell = latch();
    const read = latch();
    const readEntered = latch();
    const effectEntered = latch();
    const controller = new AbortController();
    const request = makeRequest({ signal: controller.signal });
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const order: string[] = [];
    const renderedGrants = renderCodexRun(request).leadGrants;
    const grants = { ...renderedGrants, allowedTools: new Set([...renderedGrants.allowedTools, "Search"]) };
    const broker = new CodexCallbackBroker({
      registry, worktreePath: WORKSPACE,
      grants: denyRead ? { ...grants, allowedTools: new Set([...grants.allowedTools].filter(t => t !== "Read")) } : grants,
      spawnCommand: async argv => {
        order.push(`shell:${argv[2]}`);
        if (argv[2] === "echo held") { effectEntered.release(); await shell.promise; }
        return { code: 0, stdout: "done\n", stderr: "" };
      },
      fileop: { op: async operation => {
        if (operation.op === "list") {
          assert.deepEqual(operation, { op: "list", path: "." });
          order.push("Search");
          effectEntered.release();
          await shell.promise;
          return { ok: true, entries: [] };
        }
        assert.equal(operation.op, "read");
        order.push(`read:${operation.path}`);
        if (operation.path === "first.txt") {
          readEntered.release();
          await read.promise;
        }
        return { ok: true, size: 2, data: Buffer.from("x\n").toString("base64") };
      } },
      delegate: async () => ({ ok: false, code: "unused", message: "unused" }),
      allowedRoles: new Set<string>(),
    });
    let turns = 0;
    const transport = new FakeTransport(method => method === "turn/start"
      ? { turn: { id: `tn-${++turns}` } } : defaultResponder(method));
    const { harness } = makeHarness({ registry, broker, transport });
    let reads = 0;
    let reading = false;
    const notifications = transport.notifications.bind(transport);
    transport.notifications = () => {
      const notes = notifications();
      return {
        next: () => {
          assert.equal(reading, false, "sole notification reader");
          reading = true;
          reads++;
          const pending = notes.next();
          void pending.then(() => { reading = false; }, () => { reading = false; });
          return pending;
        },
        return: notes.return?.bind(notes),
        [Symbol.asyncIterator]() { return this; },
      };
    };
    transport.push(toolCall(1, effect === "Bash" ? "uzi_bash" : "Search",
      effect === "Bash" ? { command: "echo held" } : { query: "gate" }, "th-1", "tn-1", "effect"));
    const turn = harness.startTurn(request);
    const iter = turn.events[Symbol.asyncIterator]();
    await iter.next();
    await pullUntil(iter, "started");
    try {
      await withTimeout(effectEntered.promise, 1000, "effect admission");
    } catch (error) {
      controller.abort(); shell.release(); read.release();
      await harness.close(); await iter.return?.();
      throw new Error(`effect admission failed: ${JSON.stringify({ order, responses: transport.responses })}`, { cause: error });
    }
    return {
      harness, transport, registry, broker, order, iter, turn, controller, shell, read, readEntered,
      reads: () => reads,
      async cleanup() {
        controller.abort(); shell.release(); read.release();
        await tick(); await iter.return?.(); await harness.close();
      },
    };
  }

  async function driveUntilReply(f: Awaited<ReturnType<typeof fixture>>, id: number) {
    for (let n = 0; n < 20 && !f.transport.responses.some(r => r.requestId === id); n++) {
      f.transport.push(agentMessage(`commentary driving reply ${id}`));
      assert.equal((await withTimeout(f.iter.next(), 1000, `reply ${id}`)).done, false);
    }
    assert.ok(f.transport.responses.some(r => r.requestId === id), `missing reply ${id}`);
  }

  it("Search held at list fileop keeps Read in the serial FIFO", async () => {
    const f = await fixture("Search");
    try {
      f.transport.push(toolCall(2, "uzi_read", { path: "second.txt" }, "th-1", "tn-1", "read"))
        .push(agentMessage("commentary Search control"));
      await pullUntil(f.iter, "commentary");
      assert.deepEqual(f.order, ["Search"]);
      assert.equal(f.registry.inFlightCallbackCount(), 1);
      assert.equal(f.transport.responses.length, 0);
      f.shell.release();
      f.transport.push(turnCompleted());
      await collect({ [Symbol.asyncIterator]: () => f.iter });
      assert.deepEqual(f.order, ["Search", "read:second.txt"]);
      assert.deepEqual(f.transport.responses.map(r => r.requestId), [1, 2]);
    } finally { await f.cleanup(); }
  });

  for (const firstTool of ["Read", "uzi_read"]) {
    it(`${firstTool}: one held observation, read FIFO, Bash readiness closes bypass, terminal barrier`, async () => {
      const f = await fixture();
      try {
        f.transport.push(toolCall(2, "Bash", { command: "pwd" }, "th-1", "tn-1", "pwd"))
          .push(toolCall(3, firstTool, { path: "first.txt" }, "th-1", "tn-1", "first"))
          .push(toolCall(4, "uzi_read", { path: "second.txt" }, "th-1", "tn-1", "second"))
          .push(agentMessage("commentary read FIFO barrier"));
        await pullUntil(f.iter, "commentary");
        await withTimeout(f.readEntered.promise, 1000, "observation admission");
        assert.deepEqual(f.order, ["shell:echo held", "read:first.txt"]);
        assert.equal(f.registry.inFlightCallbackCount(), 2);
        f.shell.release();
        await tick();
        assert.equal(f.registry.inFlightCallbackCount(), 1, "only the admitted read remains");
        f.transport.push(toolCall(5, "Read", { path: "third.txt" }, "th-1", "tn-1", "third"))
          .push(agentMessage("commentary Bash completed, read still held"));
        await pullUntil(f.iter, "commentary");
        assert.deepEqual(f.order, ["shell:echo held", "read:first.txt"]);
        f.transport.push(turnCompleted()).push(toolCall(6, "Bash", { command: "beyond" }));
        const events: HarnessEvent[] = [];
        let settled = false;
        const pending = collect({ [Symbol.asyncIterator]: () => f.iter }).then(e => { events.push(...e); settled = true; });
        await tick();
        const readsAtTerminal = f.reads();
        assert.equal(settled, false);
        f.read.release();
        await withTimeout(pending, 1000, "terminal after observation");
        assert.deepEqual(f.order, ["shell:echo held", "read:first.txt", "shell:pwd", "read:second.txt", "read:third.txt"]);
        assert.equal(f.reads(), readsAtTerminal, "no notification read beyond active terminal");
        assert.deepEqual(f.transport.responses.map(r => r.requestId), [1, 3, 2, 4, 5]);
        assert.equal(events.at(-1)?.kind, "turn_finished");
        assert.equal(f.registry.inFlightCallbackCount(), 0);
      } finally { await f.cleanup(); }
    });
  }

  for (const completion of ["Read first", "Bash first", "co-ready"] as const) {
    it(`${completion} while suspended at yield preserves the observation barrier`, async () => {
      const f = await fixture();
      try {
        f.transport.push(toolCall(2, "Read", { path: "first.txt" }, "th-1", "tn-1", "first"))
          .push(toolCall(3, "Bash", { command: "pwd" }, "th-1", "tn-1", "pwd"))
          .push(agentMessage("commentary suspended completion"));
        await pullUntil(f.iter, "commentary");
        await withTimeout(f.readEntered.promise, 1000, "held observation");
        if (completion === "Read first") {
          f.read.release(); await tick();
          assert.equal(f.registry.inFlightCallbackCount(), 1);
          assert.deepEqual(f.transport.responses.map(r => r.requestId), [2]);
          f.shell.release();
        } else if (completion === "Bash first") {
          f.shell.release(); await tick();
          assert.equal(f.registry.inFlightCallbackCount(), 1);
          assert.deepEqual(f.transport.responses.map(r => r.requestId), [1]);
          f.read.release();
        } else {
          f.shell.release(); f.read.release();
        }
        await tick();
        assert.deepEqual(f.order, ["shell:echo held", "read:first.txt"], "yield suspension cannot dispatch pwd");
        // Bash is already completion-ready: a newly encountered Read must stay
        // behind pwd even if its notification and both mappings are co-ready.
        f.transport.push(toolCall(4, "Read", { path: "second.txt" }, "th-1", "tn-1", "second"))
          .push(turnCompleted());
        const events = await collect({ [Symbol.asyncIterator]: () => f.iter });
        assert.deepEqual(f.order, ["shell:echo held", "read:first.txt", "shell:pwd", "read:second.txt"]);
        assert.deepEqual(f.transport.responses.map(r => r.requestId),
          completion === "Read first" ? [2, 1, 3, 4] : [1, 2, 3, 4]);
        assert.equal(events.at(-1)?.kind, "turn_finished");
      } finally { await f.cleanup(); }
    });
  }

  it("ready EOF while suspended with an observation stops queued admissions", async () => {
    const f = await fixture();
    try {
      f.transport.push(toolCall(2, "Read", { path: "first.txt" }, "th-1", "tn-1", "first"))
        .push(toolCall(3, "Bash", { command: "pwd" }, "th-1", "tn-1", "pwd"))
        .push(toolCall(4, "Read", { path: "second.txt" }, "th-1", "tn-1", "second"))
        .push(agentMessage("commentary held before EOF"));
      await pullUntil(f.iter, "commentary");
      f.read.release();
      await pullUntil(f.iter, "finished");
      await pullUntil(f.iter, "activity"); // observation removed; retained read is pending
      f.shell.release(); f.transport.end(); await tick();
      await assert.rejects(collect({ [Symbol.asyncIterator]: () => f.iter }), /stream ended before turn completion/);
      assert.deepEqual(f.order, ["shell:echo held", "read:first.txt"]);
      assert.deepEqual(f.transport.responses.map(r => r.requestId).sort(), [1, 2, 3, 4]);
      assert.equal(new Set(f.transport.responses.map(r => r.requestId)).size, 4);
    } finally { await f.cleanup(); }
  });

  for (const limit of ["count", "bytes"] as const) {
    for (const extra of [0, 1]) {
      it(`shared ${limit} budget with held observation at boundary + ${extra} and slot recovery`, async () => {
        const f = await fixture();
        try {
          const observed = toolCall(2, "Read", { path: "first.txt" }, "th-1", "tn-1", "first");
          f.transport.push(observed).push(agentMessage("commentary observation charged"));
          await pullUntil(f.iter, "commentary");
          await withTimeout(f.readEntered.promise, 1000, "budget observation");
          const queued = limit === "count" ? 4095 + extra : 1;
          for (let n = 0; n < queued; n++) {
            const note = toolCall(n + 10, "Bash", { command: "pwd", data: "" }, "th-1", "tn-1", `pwd-${n}`);
            if (limit === "bytes") {
              const overhead = Buffer.byteLength(JSON.stringify(observed)) + Buffer.byteLength(JSON.stringify(note));
              rec(rec(note.params).arguments).data = "x".repeat(64 * 1024 * 1024 - overhead + extra);
              assert.equal(Buffer.byteLength(JSON.stringify(observed)) + Buffer.byteLength(JSON.stringify(note)),
                64 * 1024 * 1024 + extra);
            }
            f.transport.push(note);
          }
          f.transport.push(agentMessage("commentary budget boundary"));
          if (extra) {
            await assert.rejects(collect({ [Symbol.asyncIterator]: () => f.iter }), /deferred request queue overflow/);
            assert.equal(f.transport.responses.length, queued);
            assert.ok(f.transport.responses.every(r => rec(rec(r.response).result).success === false));
            assert.equal(f.registry.inFlightCallbackCount(), 2, "overflow leaves admitted callbacks broker-owned");
          } else {
            await pullUntil(f.iter, "commentary");
            assert.equal(f.transport.responses.length, 0, "observation plus queue fits exactly");
            f.read.release();
            await pullUntil(f.iter, "finished"); await pullUntil(f.iter, "activity");
            const recovered = toolCall(5, "Read", { path: "first.txt" }, "th-1", "tn-1", "again");
            assert.equal(Buffer.byteLength(JSON.stringify(recovered)), Buffer.byteLength(JSON.stringify(observed)));
            f.transport.push(recovered);
            await driveUntilReply(f, 5);
            assert.equal(rec(rec(f.transport.responses.find(r => r.requestId === 5)!.response).result).success, true);
            assert.deepEqual(f.order, ["shell:echo held", "read:first.txt", "read:first.txt"]);
            f.controller.abort();
            await collect({ [Symbol.asyncIterator]: () => f.iter });
          }
          f.shell.release(); f.read.release(); await tick();
          assert.equal(new Set(f.transport.responses.map(r => r.requestId)).size, f.transport.responses.length);
          assert.equal(f.transport.responses.length, queued + (extra ? 2 : 3));
          assert.equal(f.registry.inFlightCallbackCount(), 0);
        } finally { await f.cleanup(); }
      });
    }
  }

  it("a settled observation frees its slot for the oldest queued Read behind effects", async () => {
    const f = await fixture();
    try {
      f.transport.push(toolCall(2, "Bash", { command: "pwd" }, "th-1", "tn-1", "pwd"))
        .push(toolCall(3, "Read", { path: "first.txt" }, "th-1", "tn-1", "first"))
        .push(toolCall(4, "Read", { path: "second.txt" }, "th-1", "tn-1", "second"))
        .push(agentMessage("commentary first held"));
      await pullUntil(f.iter, "commentary");
      f.read.release();
      await tick();
      f.transport.push(agentMessage("commentary second observed"));
      await pullUntil(f.iter, "commentary");
      await driveUntilReply(f, 4);
      assert.deepEqual(f.order, ["shell:echo held", "read:first.txt", "read:second.txt"]);
      assert.deepEqual(f.transport.responses.map(r => r.requestId), [3, 4]);
      f.shell.release(); f.transport.push(turnCompleted());
      await collect({ [Symbol.asyncIterator]: () => f.iter });
      assert.deepEqual(f.transport.responses.map(r => r.requestId), [3, 4, 1, 2]);
    } finally { await f.cleanup(); }
  });

  it("only the admitted held observation prevents a credentialed sink after Bash settles", async () => {
    const f = await fixture();
    try {
      f.transport.push(toolCall(2, "Read", { path: "first.txt" }, "th-1", "tn-1", "first"))
        .push(agentMessage("commentary read admitted"));
      await pullUntil(f.iter, "commentary");
      await f.readEntered.promise;
      f.shell.release(); await tick();
      assert.equal(f.registry.inFlightCallbackCount(), 1);
      assert.deepEqual(f.transport.responses.map(r => r.requestId), [1]);
      const { createCodexExecutionSafety } = await import("../src/codex/safety.js");
      const safety = createCodexExecutionSafety(f.registry, async () => { throw new Error("no boundary spawn"); });
      let sink = 0;
      await assert.rejects(safety.withBoundary({ boundary: "checkpoint", deadlineMs: 30 },
        async () => { sink++; }), /boundary failed at quiesce/);
      assert.equal(sink, 0);
      assert.equal(f.registry.isPoisoned(), true);
      f.read.release(); await tick();
      assert.equal(f.registry.inFlightCallbackCount(), 0);
    } finally { await f.cleanup(); }
  });

  for (const identity of ["stale", "foreign", "absent", "denied"]) {
    it(`${identity} Read gets no observation authority`, async () => {
      const f = await fixture("Bash", identity === "denied");
      try {
        const note = toolCall(2, "uzi_read", { path: "second.txt" },
          identity === "foreign" ? "other" : "th-1", identity === "stale" ? "old" : "tn-1", "read");
        if (identity === "absent") delete rec(note.params).threadId;
        f.transport.push(note).push(agentMessage("commentary identity barrier"));
        await pullUntil(f.iter, "commentary"); await tick();
        assert.deepEqual(f.order, ["shell:echo held"]);
        assert.equal(f.transport.responses.length, identity === "denied" ? 1 : 0);
        f.shell.release(); f.transport.push(turnCompleted());
        await collect({ [Symbol.asyncIterator]: () => f.iter });
        const reply = f.transport.responses.find(r => r.requestId === 2)!;
        assert.equal(rec(rec(reply.response).result).success, false);
        assert.equal(f.transport.responses.filter(r => r.requestId === 2).length, 1);
        assert.equal(f.registry.isPoisoned(), false);
      } finally { await f.cleanup(); }
    });
  }

  it("observed Read replay is effect-free; changed reuse is denied and poisons", async () => {
    const f = await fixture();
    try {
      f.transport.push(toolCall(2, "uzi_read", { path: "second.txt" }, "th-1", "tn-1", "read"))
        .push(toolCall(3, "uzi_read", { path: "second.txt" }, "th-1", "tn-1", "read"))
        .push(toolCall(4, "uzi_read", { path: "third.txt" }, "th-1", "tn-1", "read"))
        .push(agentMessage("commentary replay barrier"));
      await pullUntil(f.iter, "commentary"); await tick();
      f.transport.push(agentMessage("commentary replay settled"));
      await pullUntil(f.iter, "commentary");
      await driveUntilReply(f, 4);
      assert.deepEqual(f.order, ["shell:echo held", "read:second.txt"]);
      assert.deepEqual(f.transport.responses.map(r => r.requestId), [2, 3, 4]);
      const result = (id: number) => rec(rec(f.transport.responses.find(r => r.requestId === id)!.response).result);
      assert.equal(result(2).success, true);
      assert.deepEqual(JSON.parse((result(3).contentItems as { text: string }[])[0]!.text), { replay: true });
      assert.equal(result(4).success, false);
      assert.equal(f.registry.isPoisoned(), true);
    } finally { await f.cleanup(); }
  });

  for (const end of ["abort", "requestStop", "EOF", "failure", "close"] as const) {
    it(`${end} with admitted observation refuses only undispatched work and isolates late completion`, async () => {
      const f = await fixture();
      let pending: Promise<IteratorResult<HarnessEvent>> | undefined;
      try {
        f.transport.push(toolCall(2, "Read", { path: "first.txt" }, "th-1", "tn-1", "first"))
          .push(toolCall(3, "Bash", { command: "pwd" }, "th-1", "tn-1", "pwd"))
          .push(toolCall(4, "Read", { path: "second.txt" }, "th-1", "tn-1", "second"))
          .push(agentMessage("commentary cleanup barrier"));
        await pullUntil(f.iter, "commentary"); await f.readEntered.promise;
        f.shell.release(); await tick();
        assert.equal(f.registry.inFlightCallbackCount(), 1);
        // Drain the shell projection, then suspend with the genuinely held Read.
        await pullUntil(f.iter, "activity");
        pending = f.iter.next(); await tick();
        if (end === "EOF") f.transport.end();
        else if (end === "failure") f.transport.fail(new Error("observation transport failure"));
        else if (end === "abort") f.controller.abort();
        else if (end === "requestStop") await f.turn.requestStop("cancel");
        else await f.harness.close();
        if (end === "EOF" || end === "failure")
          await assert.rejects(withTimeout(pending, 1000, "observation failure"),
            end === "EOF" ? /stream ended before turn completion/ : /observation transport failure/);
        else {
          let step = await withTimeout(pending, 1000, "observation stop");
          for (let n = 0; !step.done && n < 10; n++) step = await withTimeout(f.iter.next(), 1000, "stop projection drain");
          assert.equal(step.done, true);
        }
        assert.deepEqual(f.transport.responses.map(r => r.requestId).sort(), end === "close" ? [1] : [1, 3, 4]);
        assert.equal(f.registry.inFlightCallbackCount(), 1, "harness refusal cannot settle broker-owned Read");
        if (end === "abort" || end === "requestStop") {
          const next = f.harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
          // A reused turn has no initialized event: drive setup with an explicit
          // note before releasing the old callback or delivering its terminal.
          f.transport.push(agentMessage("commentary next turn ready"));
          const events = await pullUntil(next, "commentary");
          f.read.release(); await tick();
          f.transport.push(turnCompleted("completed", undefined, "th-1", "tn-2"));
          events.push(...await collect({ [Symbol.asyncIterator]: () => next }));
          assert.equal(events.some(e => e.kind === "frame" && e.items.some(i => i.kind === "tool")), false, "no late projection in next turn");
        } else {
          f.read.release(); await tick();
          assert.equal((await f.iter.next()).done, true);
        }
        assert.equal(f.registry.inFlightCallbackCount(), 0);
        assert.equal(f.transport.responses.filter(r => r.requestId === 2).length, end === "close" ? 0 : 1);
        assert.equal(f.transport.respondAttempts, 4, "each callback has exactly one reply owner");
      } finally {
        f.controller.abort(); f.shell.release(); f.read.release();
        await f.harness.close();
        await pending?.catch(() => {});
        await f.cleanup();
      }
    });
  }
});

describe("CodexHarness: root tool projection outbox (issue #1583)", () => {
  function toolItems(events: HarnessEvent[]): Extract<HarnessEvent, { kind: "frame" }>["items"][number][] {
    return events.flatMap((e) => (e.kind === "frame" ? e.items : [])).filter((i) => i.kind === "tool");
  }

  it("yields the started tool frame BEFORE a slow inline broker settles, then finished < turn_finished, one reply", async () => {
    let brokerCalls = 0;
    let settled = false;
    let release!: (r: CallbackResult) => void;
    const gate = new Promise<CallbackResult>((r) => {
      release = r;
    });
    const broker = stubBroker(async () => {
      brokerCalls += 1;
      const r = await gate;
      settled = true;
      return r;
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    transport
      .push(threadStarted())
      .push(toolCall(7, "Bash", { command: "echo hi" }, "th-1", "tn-1", "call_7"))
      .push(turnCompleted("completed"))
      .end();
    const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();

    const init = await iter.next();
    assert.equal((init.value as HarnessEvent).kind, "initialized");
    const startedStep = await withTimeout(iter.next(), 1000, "the started frame before the broker settles");
    const started = startedStep.value as HarnessEvent;
    assert.equal(settled, false, "the broker has NOT settled when the started frame is yielded");
    assert.equal(brokerCalls, 1);
    assert.equal(transport.responses.length, 0, "no reply before the broker settles");
    assert.ok(started.kind === "frame");
    assert.deepEqual(started.origin, { kind: "main" });
    assert.deepEqual(started.attribution, { agent: "lead" });
    assert.equal(started.signals, undefined);
    assert.equal(started.model, provider.model);
    assert.equal(started.sessionId, "th-1");
    assert.deepEqual(started.items, [
      { kind: "tool", phase: "started", id: "cx-abcdef012345-t1-call_7", name: "Bash", input: { command: "echo hi" } },
    ]);

    release({ ok: true, output: { code: 0, stdout: "hi", stderr: "" } });
    const rest: HarnessEvent[] = [];
    for (let step = await iter.next(); !step.done; step = await iter.next()) rest.push(step.value);

    const kinds = rest.map((e) => (e.kind === "frame" ? `frame:${e.items[0]?.kind === "tool" ? e.items[0].phase : "?"}` : e.kind));
    assert.deepEqual(kinds, ["frame:finished", "activity", "turn_finished"], "started < finished < turn_finished");
    const finished = rest[0]!;
    assert.ok(finished.kind === "frame");
    assert.deepEqual(finished.attribution, { agent: "lead" });
    assert.deepEqual(finished.items, [
      {
        kind: "tool",
        phase: "finished",
        id: "cx-abcdef012345-t1-call_7",
        name: "Bash",
        output: JSON.stringify({ code: 0, stdout: "hi", stderr: "" }),
        isError: false,
      },
    ]);
    // Exactly one reply for the request, and the broker ran exactly once.
    assert.equal(transport.responses.filter((r) => r.requestId === 7).length, 1);
    assert.equal(transport.responses.length, 1);
    assert.equal(brokerCalls, 1);
  });

  it("a broker THROW projects a finished error frame (broker_error) before the single reply", async () => {
    const broker = stubBroker(async () => {
      throw new Error("boom");
    });
    const { harness, transport } = makeHarness({ broker });
    transport.push(threadStarted()).push(toolCall(3, "Bash", { command: "x" })).push(turnCompleted("completed")).end();
    const events = await collect(harness.startTurn(makeRequest()).events);
    const tools = toolItems(events);
    assert.equal(tools.length, 2);
    const fin = tools[1]!;
    assert.ok(fin.kind === "tool" && fin.phase === "finished");
    assert.equal(fin.isError, true);
    assert.equal(fin.output, "the callback failed");
    assert.equal(transport.responses.length, 1);
  });

  it("scrubs projected input and output with scrubProjected (the model reply is untouched)", async () => {
    const secret = ["tok", "RUNTIME", "released", "0123456789"].join("-");
    const broker = stubBroker(async () => ({ ok: true, output: { code: 0, stdout: `value=${secret}`, stderr: "" } }));
    const { harness, transport } = makeHarness({
      broker,
      scrubProjected: (s) => s.split(secret).join("***REDACTED***"),
    });
    transport
      .push(threadStarted())
      .push(toolCall(4, "Bash", { command: `echo ${secret}` }))
      .push(turnCompleted("completed"))
      .end();
    const events = await collect(harness.startTurn(makeRequest()).events);
    const projected = JSON.stringify(toolItems(events));
    assert.ok(!projected.includes(secret), "no raw secret in any projected item");
    assert.equal((projected.match(/\*\*\*REDACTED\*\*\*/g) ?? []).length, 2, "input and output both scrubbed");
    assert.equal(transport.responses.length, 1);
  });

  it("a denied stale-turn call is not projected; a reused call id is namespaced per turn", async () => {
    let turnNumber = 0;
    const transport = new FakeTransport((method) => {
      if (method === "turn/start") {
        turnNumber += 1;
        return { turn: { id: `tn-${turnNumber}` } };
      }
      return defaultResponder(method);
    });
    const { harness } = makeHarness({ transport, idNonce: "0123456789ab" });
    transport
      .push(threadStarted())
      .push(toolCall(1, "Bash", { command: "a" }, "th-1", "stale-turn", "same"))
      .push(toolCall(2, "Bash", { command: "b" }, "th-1", "tn-1", "same"))
      .push(turnCompleted("completed", undefined, "th-1", "tn-1"));
    const first = toolItems(await collect(harness.startTurn(makeRequest()).events));
    transport.push(toolCall(3, "Bash", { command: "c" }, "th-1", "tn-2", "same")).push(turnCompleted("completed", undefined, "th-1", "tn-2"));
    const second = toolItems(await collect(harness.startTurn(makeRequest()).events));
    assert.equal(first.length, 2, "the stale-turn call is not projected");
    assert.equal(second.length, 2);
    const idOf = (i: (typeof first)[number]): unknown => (i.kind === "tool" ? i.id : undefined);
    assert.equal(idOf(first[0]!), "cx-0123456789ab-t1-same");
    assert.equal(idOf(second[0]!), "cx-0123456789ab-t2-same", "a reused call id is namespaced per turn");
  });

  it("a callback settling after its turn ended projects nothing into the next turn", async () => {
    let turnNumber = 0;
    const transport = new FakeTransport((method) => {
      if (method === "turn/start") {
        turnNumber += 1;
        return { turn: { id: `tn-${turnNumber}` } };
      }
      return defaultResponder(method);
    });
    const ac = new AbortController();
    const { broker, entered, release } = wedgeBroker();
    const { harness } = makeHarness({ transport, broker });
    transport.push(threadStarted()).push(toolCall(1, "Bash", { command: "sleep 999" }, "th-1", "tn-1"));
    const iter = harness.startTurn(makeRequest({ signal: ac.signal })).events[Symbol.asyncIterator]();
    await iter.next(); // initialized
    await withTimeout(iter.next(), 1000, "the started frame"); // started
    await withTimeout(entered, 1000, "the broker callback to be entered");
    const pending = iter.next();
    ac.abort();
    assert.equal((await withTimeout(pending, 1000, "the stream to end")).done, true);

    const second = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
    const turn2Events: HarnessEvent[] = [];
    // Make turn 2 LIVE first: its first next() runs runTurn's body (which marks turn 2 as the
    // live projection turn) and parks awaiting the next notification. Only THEN settle the OLD
    // turn's wedged callback, so the cross-turn guard in emitProjected is what drops its frame.
    const firstStep = second.next();
    await tick();
    release({ ok: true, output: {} });
    await tick();
    await tick();
    transport.push(turnCompleted("completed", undefined, "th-1", "tn-2")).end();
    for (let step = await withTimeout(firstStep, 1000, "turn 2's first event"); !step.done; step = await second.next()) {
      turn2Events.push(step.value);
    }
    assert.equal(turn2Events.filter((e) => e.kind === "frame").length, 0, "the late finished frame is dropped");
    assert.equal(turn2Events.at(-1)?.kind, "turn_finished");
  });
});

describe("CodexHarness: delegation dispatch binding + child frames (issue #1583 m2)", () => {
  const scrubProjected = (s: string): string => s.split("SECRET").join("***");
  const frames = (events: HarnessEvent[]): Extract<HarnessEvent, { kind: "frame" }>[] =>
    events.flatMap((e) => (e.kind === "frame" ? [e] : []));

  async function waitUntil(cond: () => boolean, label: string): Promise<void> {
    for (let i = 0; i < 2000 && !cond(); i += 1) await tick();
    if (!cond()) throw new Error(`timed out waiting for ${label}`);
  }

  for (const key of ["prompt", "task", "input", "message", "description"] as const) {
    it(`Codex runtime discovery projects a scrubbed bounded ${key} assignment without changing execution`, async () => {
      let harnessRef!: CodexHarness;
      const assignment = `Review SECRET\u001b ${"🧪".repeat(200)}`;
      const broker = stubBroker(async (rt, _name, args) => {
        assert.equal((args as Record<string, unknown>)[key], assignment, "the delegate receives its original assignment");
        harnessRef.registerChildSink("th-c", { push: () => {} });
        harnessRef.bindChildDispatch("th-c", rt, "coder");
        harnessRef.unregisterChildSink("th-c");
        return { ok: true, output: {} };
      });
      const { harness, transport } = makeHarness({ broker, scrubProjected });
      harnessRef = harness;
      transport.push(threadStarted()).push(toolCall(1, "spawn_agent", {
        subagent_type: "coder", description: "[m1] review", [key]: assignment,
      }, "th-1", "tn-1", "c-1"));
      const pending = collect(harness.startTurn(makeRequest()).events);
      await waitUntil(() => transport.responses.length === 1, "assignment reply");
      transport.push(turnCompleted("completed")).end();
      const events = await withTimeout(pending, 2000, "assignment projection");
      const item = frames(events)[0]!.items[0]!;
      assert.ok(item.kind === "tool" && item.phase === "started");
      const input = item.input as { prompt: string; description: string };
      assert.match(input.prompt, /^Review \*\*\*/);
      assert.ok(Buffer.byteLength(input.prompt, "utf8") <= 256, "the excerpt shares the label's UTF-8 byte bound");
      assert.ok(!input.prompt.includes("SECRET") && !input.prompt.includes("\u001b") && !input.prompt.includes("\ufffd"));
      if (key !== "description") assert.equal(input.description, "[m1] review", "milestone labels remain unchanged");
    });
  }

  it("Codex runtime discovery omits assignment text if scrubbing fails", async () => {
    let harnessRef!: CodexHarness;
    const broker = stubBroker(async rt => {
      harnessRef.registerChildSink("th-c", { push: () => {} });
      harnessRef.bindChildDispatch("th-c", rt, "coder");
      harnessRef.unregisterChildSink("th-c");
      return { ok: true, output: {} };
    });
    const { harness, transport } = makeHarness({ broker, scrubProjected: (text) => {
      if (text.includes("SECRET")) throw new Error("scrub failed");
      return text;
    } });
    harnessRef = harness;
    transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder", description: "[m1] review", prompt: "SECRET" }, "th-1", "tn-1", "c-1"));
    const pending = collect(harness.startTurn(makeRequest()).events);
    await waitUntil(() => transport.responses.length === 1, "scrub failure reply");
    transport.push(turnCompleted("completed")).end();
    const events = await withTimeout(pending, 2000, "scrub failure turn");
    const item = frames(events)[0]?.items[0];
    assert.ok(item?.kind === "tool" && item.phase === "started", "Agent started frame survives assignment scrubbing failure");
    assert.equal(item.name, "Agent");
    const input = item.input as Record<string, unknown>;
    assert.equal(input.subagent_type, "coder");
    assert.equal(input.description, "[m1] review");
    assert.ok(!Object.hasOwn(input, "prompt"));
    assert.ok(!JSON.stringify(events).includes("SECRET"));
    assert.equal(events.at(-1)?.kind, "turn_finished");
  });

  it("binds nothing when the parent key is ambiguous (two in-flight calls share an empty call id)", async () => {
    let harnessRef!: CodexHarness;
    let entered = 0;
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      entered += 1;
      const child = `th-child-${entered}`;
      await gate;
      harnessRef.registerChildSink(child, { push: () => {} });
      harnessRef.bindChildDispatch(child, rt, "coder");
      harnessRef.emitChildFrame(child, [{ kind: "text", text: "child says" }]);
      harnessRef.unregisterChildSink(child);
      return { ok: true, output: { text: "done" } };
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    transport
      .push(threadStarted())
      .push(toolCall(1, "spawn_agent", { subagent_type: "coder", description: "[a]" }, "th-1", "tn-1", ""))
      .push(toolCall(2, "spawn_agent", { subagent_type: "coder", description: "[b]" }, "th-1", "tn-1", ""));
    const eventsP = collect(harness.startTurn(makeRequest()).events);
    await waitUntil(() => entered === 2, "both delegations in flight");
    release();
    await waitUntil(() => transport.responses.length === 2, "both replies");
    transport.push(turnCompleted("completed")).end();
    const events = await withTimeout(eventsP, 2000, "ambiguous turn");
    assert.deepEqual(frames(events), [], "no dispatch, child or completion frame for an ambiguous key");
    assert.equal(events[events.length - 1]!.kind, "turn_finished");
  });

  it("issue #1800: a child-projected frame is not model-processing evidence; a root agentMessage frame is", async () => {
    let harnessRef!: CodexHarness;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      harnessRef.registerChildSink("th-c", { push: () => {} });
      harnessRef.bindChildDispatch("th-c", rt, "coder");
      harnessRef.emitChildFrame("th-c", [{ kind: "text", text: "child says hi" }]);
      harnessRef.unregisterChildSink("th-c");
      return { ok: true, output: { text: "done" } };
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    transport
      .push(threadStarted())
      .push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"))
      .push(agentMessage("root answer"));
    const eventsP = collect(harness.startTurn(makeRequest()).events);
    await waitUntil(() => transport.responses.length === 1, "the parent reply");
    transport.push(turnCompleted("completed")).end();
    const events = await withTimeout(eventsP, 2000, "evidence turn");

    const child = frames(events).filter((f) => f.origin.kind === "subagent");
    assert.equal(child.length, 1, "one child frame");
    assert.equal(evidencesModelProcessing(child[0]!), false, "a subagent frame never evidences the lead");
    const root = frames(events).find((f) => f.origin.kind === "main" && f.items.some((i) => i.kind === "text" && i.text === "root answer"));
    assert.ok(root, "the root agentMessage frame was projected");
    assert.equal(evidencesModelProcessing(root), true);
    const toolFrames = frames(events).filter((f) => f.origin.kind === "main" && f.items.some((i) => i.kind === "tool"));
    const start = toolFrames.find((f) => f.items.some((i) => i.kind === "tool" && i.phase === "started"));
    const finish = toolFrames.find((f) => f.items.some((i) => i.kind === "tool" && i.phase === "finished"));
    assert.ok(start && finish, "the lead dispatch start and completion frames were projected");
    assert.equal(evidencesModelProcessing(start), true, "the model-issued dispatch start counts");
    assert.equal(evidencesModelProcessing(finish), false, "a completion (result) frame never counts");
  });

  it("emitChildFrame projects only for a registered AND bound thread, scrubbed, namespaced and signal-free", async () => {
    let harnessRef!: CodexHarness;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      const h = harnessRef;
      h.emitChildFrame("th-c", [{ kind: "text", text: "unregistered" }]);
      h.bindChildDispatch("th-c", rt, "coder"); // not registered yet: binds nothing
      h.registerChildSink("th-c", { push: () => {} });
      h.emitChildFrame("th-c", [{ kind: "text", text: "unbound" }]);
      h.bindChildDispatch("th-c", { ...rt, callId: "other" }, "coder"); // no pending dispatch for that key
      h.bindChildDispatch("th-c", rt, "co\u001bder");
      h.bindChildDispatch("th-c", rt, "coder"); // already bound: no second dispatch frame
      h.emitChildFrame("th-c", [
        { kind: "text", text: "tok SECRET" },
        { kind: "tool", phase: "started", id: "call 1", name: "Bash", input: { command: "cat SECRET" }, signal: "submit_plan" },
        { kind: "tool", phase: "finished", id: "call 1", name: "Bash", output: "SECRET out", isError: false },
        { kind: "tool", phase: "finished", id: "never-started", name: "Bash", output: "x" },
      ]);
      h.unregisterChildSink("th-c");
      h.emitChildFrame("th-c", [{ kind: "text", text: "late" }]);
      return { ok: true, output: { text: "done" } };
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345", scrubProjected });
    harnessRef = harness;
    transport
      .push(threadStarted())
      .push(toolCall(1, "spawn_agent", { subagent_type: "coder", description: "[m1] SECRET label", prompt: "Review SECRET changes", task: "ignored fallback" }, "th-1", "tn-1", "c-1"));
    const eventsP = collect(harness.startTurn(makeRequest()).events);
    await waitUntil(() => transport.responses.length === 1, "the parent reply");
    transport.push(turnCompleted("completed")).end();
    const events = await withTimeout(eventsP, 2000, "bound turn");

    const dispatchId = "cx-abcdef012345-t1-c-1";
    const fs = frames(events);
    assert.equal(fs.length, 3, "dispatch, one child frame, completion");
    const [dispatch, child, completion] = fs as [(typeof fs)[number], (typeof fs)[number], (typeof fs)[number]];
    assert.deepEqual(dispatch.origin, { kind: "main" });
    assert.deepEqual(dispatch.attribution, { agent: "lead" });
    assert.deepEqual(dispatch.items, [
      { kind: "tool", phase: "started", id: dispatchId, name: "Agent", input: { subagent_type: "coder", description: "[m1] *** label", prompt: "Review *** changes" } },
    ]);
    assert.deepEqual(child.origin, { kind: "subagent", role: "coder", instanceId: dispatchId });
    assert.deepEqual(child.attribution, { agent: "coder", agentInstance: dispatchId, agentLabel: "[m1] *** label" });
    assert.equal(child.signals, undefined);
    assert.deepEqual(child.items, [
      { kind: "text", text: "tok ***" },
      { kind: "tool", phase: "started", id: `${dispatchId}/call1`, name: "Bash", input: { command: "cat ***" } },
      { kind: "tool", phase: "finished", id: `${dispatchId}/call1`, name: "Bash", output: "*** out", isError: false },
    ]);
    assert.deepEqual(completion.items, [
      { kind: "tool", phase: "finished", id: dispatchId, name: "Agent", output: JSON.stringify({ text: "done" }), isError: false },
    ]);
    assert.ok(events.indexOf(completion) < events.findIndex((e) => e.kind === "turn_finished"));
    assert.equal(transport.responses.length, 1, "exactly one reply");
  });

  it("a bound dispatch still open at turn end gets ONE unconfirmed completion before turn_finished; its late settlement adds none", async () => {
    let harnessRef!: CodexHarness;
    let release!: (r: CallbackResult) => void;
    const settle = new Promise<CallbackResult>((r) => {
      release = r;
    });
    let bound = false;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      harnessRef.registerChildSink("th-c", { push: () => {} });
      harnessRef.bindChildDispatch("th-c", rt, "coder");
      // Issue #1864: a child tool left open (its args and output carry child text the warn
      // must never repeat), under a name that needs sanitizing.
      harnessRef.emitChildFrame("th-c", [
        { kind: "tool", phase: "started", id: "ct-1", name: "shell exec/run", input: { cmd: "child-secret-arg" } },
      ]);
      bound = true;
      return settle;
    });
    const warns: { msg: string; fields: Record<string, unknown> | undefined }[] = [];
    const log: Logger = {
      ...noopLog,
      warn(msg: string, fields?: Record<string, unknown>) {
        warns.push({ msg, fields });
      },
    };
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345", log });
    harnessRef = harness;
    transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"));
    const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
    const seen: HarnessEvent[] = [];
    // Pull up to (and including) turn_finished in the background: the generator is lazy.
    const upToTerminal = (async (): Promise<void> => {
      for (;;) {
        const step = await iter.next();
        assert.ok(!step.done, "the stream ended before turn_finished");
        seen.push(step.value);
        if (step.value.kind === "turn_finished") return;
      }
    })();
    await waitUntil(() => bound, "the dispatch bound");
    transport.push(turnCompleted("completed")).end();
    await withTimeout(upToTerminal, 2000, "turn events");
    const tail = iter.next();
    release({ ok: true, output: { text: "late success" } });
    for (let step = await withTimeout(tail, 2000, "the late settle"); !step.done; step = await iter.next()) seen.push(step.value);

    const results = frames(seen).flatMap((f) => f.items).filter((i) => i.kind === "tool" && i.phase === "finished" && i.name === "Agent");
    assert.equal(results.length, 1, "exactly one completion");
    const synthesized = frames(seen).find((f) => f.items.some((i) => i.kind === "tool" && i.phase === "finished" && i.name === "Agent"));
    assert.equal(evidencesModelProcessing(synthesized!), false, "a worker-synthesized completion is not model evidence (issue #1800)");
    // Issue #1864: one warn names the open delegation, its role, age and open child tools only.
    const openWarns = warns.filter((w) => w.msg === "codex delegation open at turn end");
    assert.equal(openWarns.length, 1, "one open-at-turn-end warn");
    const fields = openWarns[0]!.fields!;
    assert.deepEqual(Object.keys(fields).sort(), ["age_ms", "dispatch_id", "open_child_tool_names", "open_child_tools", "role", "state"]);
    assert.equal(fields.dispatch_id, "cx-abcdef012345-t1-c-1");
    assert.equal(fields.state, "bound");
    assert.equal(fields.role, "coder");
    assert.equal(typeof fields.age_ms, "number");
    assert.ok((fields.age_ms as number) >= 0);
    assert.equal(fields.open_child_tools, 1);
    assert.deepEqual(fields.open_child_tool_names, ["shell_exec_run"]);
    assert.ok(!JSON.stringify(warns).includes("child-secret-arg"), "no child args in the warn");
    assert.ok(!JSON.stringify(warns).includes("late success"), "no child text in the warn");
    assert.deepEqual(results[0], {
      kind: "tool",
      phase: "finished",
      id: "cx-abcdef012345-t1-c-1",
      name: "Agent",
      output: "delegation still open at turn end; child settlement not confirmed",
      isError: true,
    });
    const completionAt = seen.findIndex((e) => e.kind === "frame" && e.items.some((i) => i.kind === "tool" && i.phase === "finished"));
    assert.ok(completionAt < seen.findIndex((e) => e.kind === "turn_finished"), "the completion precedes turn_finished");
    assert.equal(transport.responses.filter((r) => r.requestId === 1).length, 1, "the parent still replied exactly once");
  });

  it("a PENDING dispatch still open at turn end (reserved, child never bound) is warned with state pending and projects nothing", async () => {
    let entered = false;
    const broker = stubBroker(async (_rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      entered = true;
      return new Promise<CallbackResult>(() => {}); // never binds a child, never settles
    });
    const warns: { msg: string; fields: Record<string, unknown> | undefined }[] = [];
    const log: Logger = {
      ...noopLog,
      warn(msg: string, fields?: Record<string, unknown>) {
        warns.push({ msg, fields });
      },
    };
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345", log });
    transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"));
    const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
    const seen: HarnessEvent[] = [];
    const upToTerminal = (async (): Promise<void> => {
      for (;;) {
        const step = await iter.next();
        assert.ok(!step.done, "the stream ended before turn_finished");
        seen.push(step.value);
        if (step.value.kind === "turn_finished") return;
      }
    })();
    await waitUntil(() => entered, "the delegation reserved");
    transport.push(turnCompleted("completed")).end();
    await withTimeout(upToTerminal, 2000, "turn events");

    const openWarns = warns.filter((w) => w.msg === "codex delegation open at turn end");
    assert.equal(openWarns.length, 1, "one open-at-turn-end warn for the pending dispatch");
    const fields = openWarns[0]!.fields!;
    assert.equal(fields.dispatch_id, "cx-abcdef012345-t1-c-1");
    assert.equal(fields.state, "pending");
    assert.equal(fields.role, "");
    assert.equal(fields.open_child_tools, 0);
    assert.deepEqual(fields.open_child_tool_names, []);
    assert.deepEqual(frames(seen), [], "a pending dispatch projects no dispatch, child or completion frame");
  });

  it("requestStop with a bound open dispatch yields a synthesized stopped completion before the stream ends", async () => {
    let harnessRef!: CodexHarness;
    let bound = false;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      harnessRef.registerChildSink("th-c", { push: () => {} });
      harnessRef.bindChildDispatch("th-c", rt, "coder");
      bound = true;
      return new Promise<CallbackResult>(() => {});
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"));
    const turn = harness.startTurn(makeRequest());
    const eventsP = collect(turn.events);
    await waitUntil(() => bound, "the dispatch bound");
    turn.requestStop("cancel");
    const events = await withTimeout(eventsP, 2000, "stopped turn");
    const items = frames(events).flatMap((f) => f.items);
    assert.equal(items.length, 2, "the dispatch and its synthesized completion");
    assert.deepEqual(items[1], {
      kind: "tool",
      phase: "finished",
      id: "cx-abcdef012345-t1-c-1",
      name: "Agent",
      output: "delegation stopped by the run; child settlement not confirmed",
      isError: true,
    });
  });

  it("an unexpected EOF with a bound open dispatch yields a synthesized error completion BEFORE the protocol throw", async () => {
    let harnessRef!: CodexHarness;
    let bound = false;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      harnessRef.registerChildSink("th-c", { push: () => {} });
      harnessRef.bindChildDispatch("th-c", rt, "coder");
      bound = true;
      return new Promise<CallbackResult>(() => {});
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"));
    const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
    const seen: HarnessEvent[] = [];
    let thrown: unknown;
    const drive = (async (): Promise<void> => {
      try {
        for (let step = await iter.next(); !step.done; step = await iter.next()) seen.push(step.value);
      } catch (error) {
        thrown = error;
      }
    })();
    await waitUntil(() => bound, "the dispatch bound");
    transport.end(); // the provider stream ends with no turn/completed
    await withTimeout(drive, 2000, "the EOF turn");

    assert.ok(thrown instanceof CodexHarnessError, "the protocol throw still surfaces");
    assert.equal(thrown.failure.category, "protocol");
    assert.match(thrown.message, /ended before turn completion/);
    const items = frames(seen).flatMap((f) => f.items);
    assert.equal(items.length, 2, "the dispatch and its synthesized completion");
    assert.deepEqual(items[1], {
      kind: "tool",
      phase: "finished",
      id: "cx-abcdef012345-t1-c-1",
      name: "Agent",
      output: "delegation ended by a provider stream failure; child settlement not confirmed",
      isError: true,
    });
  });

  /** Drive a turn with one bound, never-settling dispatch; once bound, `fail` makes the stream
   *  fail. Returns the events seen before the throw, and the throw itself. */
  async function runBoundDispatchThenFail(
    fail: (transport: FakeTransport) => void,
    opts: { appServerAuth?: CodexAppServerAuthSession } = {},
  ): Promise<{ seen: HarnessEvent[]; thrown: unknown }> {
    let harnessRef!: CodexHarness;
    let bound = false;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      harnessRef.registerChildSink("th-c", { push: () => {} });
      harnessRef.bindChildDispatch("th-c", rt, "coder");
      bound = true;
      return new Promise<CallbackResult>(() => {});
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345", appServerAuth: opts.appServerAuth });
    harnessRef = harness;
    transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"));
    const iter = harness.startTurn(makeRequest()).events[Symbol.asyncIterator]();
    const seen: HarnessEvent[] = [];
    let thrown: unknown;
    const drive = (async (): Promise<void> => {
      try {
        for (let step = await iter.next(); !step.done; step = await iter.next()) seen.push(step.value);
      } catch (error) {
        thrown = error;
      }
    })();
    await waitUntil(() => bound, "the dispatch bound");
    fail(transport);
    await withTimeout(drive, 2000, "the failing turn");
    return { seen, thrown };
  }

  const streamFailedCompletion = {
    kind: "tool",
    phase: "finished",
    id: "cx-abcdef012345-t1-c-1",
    name: "Agent",
    output: "delegation ended by a provider stream failure; child settlement not confirmed",
    isError: true,
  };

  it("a REJECTED notification read with a bound open dispatch yields the stream-failure completion BEFORE the rethrow", async () => {
    const boom = new Error("transport read failed");
    const { seen, thrown } = await runBoundDispatchThenFail((transport) => transport.fail(boom));
    assert.equal(thrown, boom, "the read failure is rethrown as-is");
    const items = frames(seen).flatMap((f) => f.items);
    assert.equal(items.length, 2, "the dispatch and its synthesized completion");
    assert.deepEqual(items[1], streamFailedCompletion);
  });

  it("a REJECTED mapNote (auth drain throws at turn completion) with a bound open dispatch yields the completion BEFORE the rethrow", async () => {
    const boom = new Error("auth drain poisoned");
    const appServerAuth: CodexAppServerAuthSession = {
      mode: "api_key",
      authenticate: async () => {},
      handleServerRequest: async () => false,
      drainInterceptedRequests: () => Promise.reject(boom),
      closeAdmissionAndCancel: () => {},
    };
    const { seen, thrown } = await runBoundDispatchThenFail((transport) => transport.push(turnCompleted("completed")), {
      appServerAuth,
    });
    assert.equal(thrown, boom, "the mapNote failure is rethrown as-is");
    assert.ok(!seen.some((e) => e.kind === "turn_finished"), "no terminal was published");
    const items = frames(seen).flatMap((f) => f.items);
    assert.equal(items.length, 2, "the dispatch and its synthesized completion");
    assert.deepEqual(items[1], streamFailedCompletion);
  });

  it("a stop closes a PENDING dispatch: a child start completing after the stop binds nothing and projects nothing", async () => {
    // Dispatch c-a is bound (so the stop path yields its synthesized completion and the stream is
    // suspended mid-drain, still live); dispatch c-b is still PENDING (its child turn/start has
    // not completed). The child of c-b "starts" only after the stop: it must not bind.
    let harnessRef!: CodexHarness;
    let aBound = false;
    let bEntered = false;
    let bAttempted = false;
    let releaseB!: () => void;
    const gateB = new Promise<void>((r) => {
      releaseB = r;
    });
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      if (rt.callId === "c-a") {
        harnessRef.registerChildSink("th-a", { push: () => {} });
        harnessRef.bindChildDispatch("th-a", rt, "coder");
        aBound = true;
        return new Promise<CallbackResult>(() => {});
      }
      bEntered = true;
      await gateB;
      harnessRef.registerChildSink("th-b", { push: () => {} });
      harnessRef.bindChildDispatch("th-b", rt, "tester");
      harnessRef.emitChildFrame("th-b", [{ kind: "text", text: "late child text" }]);
      bAttempted = true;
      return { ok: true, output: { text: "late done" } };
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    transport
      .push(threadStarted())
      .push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-a"))
      .push(toolCall(2, "spawn_agent", { subagent_type: "tester" }, "th-1", "tn-1", "c-b"));
    const turn = harness.startTurn(makeRequest());
    const iter = turn.events[Symbol.asyncIterator]();
    const seen: HarnessEvent[] = [];
    // Pull (one outstanding next() at a time) until c-a is bound and c-b is in flight; the last
    // pull stays outstanding while the stream waits for a provider note.
    let pending = iter.next();
    for (;;) {
      const step = await Promise.race([pending, tick().then(() => undefined)]);
      if (step === undefined) {
        if (aBound && bEntered) break;
        continue;
      }
      assert.ok(!step.done, "the stream ended early");
      seen.push(step.value);
      pending = iter.next();
    }
    turn.requestStop("cancel");
    // Pull up to c-a's synthesized completion: the generator is then suspended mid-drain, live.
    for (;;) {
      const step = await withTimeout(pending, 2000, "the stop completion");
      assert.ok(!step.done, "the stream ended before the stop completion");
      seen.push(step.value);
      if (step.value.kind === "frame" && step.value.items.some((i) => i.kind === "tool" && i.phase === "finished")) break;
      pending = iter.next();
    }
    releaseB();
    await waitUntil(() => bAttempted, "the late c-b child start");
    for (let step = await iter.next(); !step.done; step = await iter.next()) seen.push(step.value);

    const items = frames(seen).flatMap((f) => f.items);
    assert.deepEqual(
      items.map((i) => (i.kind === "tool" ? `${i.phase}:${i.id}` : i.kind)),
      ["started:cx-abcdef012345-t1-c-a", "finished:cx-abcdef012345-t1-c-a"],
      "no dispatch, child frame or completion for the pending c-b",
    );
    assert.ok(!frames(seen).some((f) => f.origin.kind === "subagent"), "no child frame");
  });

  it("caps projected child items per dispatch: one 'not shown' marker, later items dropped, the completion still emitted", async () => {
    let harnessRef!: CodexHarness;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      const h = harnessRef;
      h.registerChildSink("th-c", { push: () => {} });
      h.bindChildDispatch("th-c", rt, "coder");
      // One frame with far more parts than the per-dispatch item budget.
      h.emitChildFrame("th-c", Array.from({ length: 2500 }, (_, i) => ({ kind: "text" as const, text: `part ${i}` })));
      h.emitChildFrame("th-c", [{ kind: "text", text: "after the cap" }]);
      h.unregisterChildSink("th-c");
      return { ok: true, output: { text: "done" } };
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"));
    const eventsP = collect(harness.startTurn(makeRequest()).events);
    await waitUntil(() => transport.responses.length === 1, "the parent reply");
    transport.push(turnCompleted("completed")).end();
    const events = await withTimeout(eventsP, 5000, "capped turn");

    const childFrames = frames(events).filter((f) => f.origin.kind === "subagent");
    assert.equal(childFrames.length, 1, "nothing projects after the cap");
    const items = childFrames[0]!.items;
    assert.equal(items.length, 2001, "2000 items plus the marker");
    assert.deepEqual(items[1999], { kind: "text", text: "part 1999" });
    assert.deepEqual(items[2000], { kind: "text", text: "[further subagent output not shown]" });
    assert.deepEqual(childFrames[0]!.attribution, { agent: "coder", agentInstance: "cx-abcdef012345-t1-c-1" });
    const completion = frames(events).flatMap((f) => f.items).find((i) => i.kind === "tool" && i.phase === "finished");
    assert.deepEqual(completion, {
      kind: "tool",
      phase: "finished",
      id: "cx-abcdef012345-t1-c-1",
      name: "Agent",
      output: JSON.stringify({ text: "done" }),
      isError: false,
    });
  });

  it("caps projected child BYTES per dispatch across frames, then emits the marker once", async () => {
    let harnessRef!: CodexHarness;
    const big = "a".repeat(60 * 1024);
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      const h = harnessRef;
      h.registerChildSink("th-c", { push: () => {} });
      h.bindChildDispatch("th-c", rt, "coder");
      for (let i = 0; i < 100; i += 1) h.emitChildFrame("th-c", [{ kind: "text", text: big }]);
      h.unregisterChildSink("th-c");
      return { ok: true, output: { text: "done" } };
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"));
    const eventsP = collect(harness.startTurn(makeRequest()).events);
    await waitUntil(() => transport.responses.length === 1, "the parent reply");
    transport.push(turnCompleted("completed")).end();
    const events = await withTimeout(eventsP, 5000, "byte-capped turn");

    const childItems = frames(events)
      .filter((f) => f.origin.kind === "subagent")
      .flatMap((f) => f.items);
    const markers = childItems.filter((i) => i.kind === "text" && i.text === "[further subagent output not shown]");
    assert.equal(markers.length, 1, "exactly one marker");
    assert.equal(childItems[childItems.length - 1], markers[0], "the marker is last");
    assert.ok(childItems.length < 100, `the byte budget cut the stream (${childItems.length} items)`);
    const projectedBytes = childItems.slice(0, -1).reduce((n, i) => n + Buffer.byteLength(JSON.stringify(i), "utf8"), 0);
    assert.ok(projectedBytes <= 4 * 1024 * 1024, `${projectedBytes} bytes`);
    assert.ok(
      frames(events).some((f) => f.items.some((i) => i.kind === "tool" && i.phase === "finished" && !i.isError)),
      "the lead completion is still emitted",
    );
  });

  it("a cap tripping between a child tool's started and finished still passes that finished (output omitted), paired; a started past the cap never goes out", async () => {
    let harnessRef!: CodexHarness;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      const h = harnessRef;
      h.registerChildSink("th-c", { push: () => {} });
      h.bindChildDispatch("th-c", rt, "coder");
      h.emitChildFrame("th-c", [{ kind: "tool", phase: "started", id: "t1", name: "Bash", input: { command: "ls" } }]);
      // 1 + 1999 = exactly the per-dispatch item budget.
      h.emitChildFrame("th-c", Array.from({ length: 1999 }, (_, i) => ({ kind: "text" as const, text: `part ${i}` })));
      h.emitChildFrame("th-c", [
        { kind: "tool", phase: "started", id: "t2", name: "Read", input: {} }, // trips the cap: never projected
        { kind: "tool", phase: "finished", id: "t1", name: "Bash", output: "ok", isError: false }, // passes
        { kind: "tool", phase: "finished", id: "t2", name: "Read", output: "x" }, // its started never went out
      ]);
      h.emitChildFrame("th-c", [
        { kind: "text", text: "after the cap" },
        { kind: "tool", phase: "finished", id: "t1", name: "Bash", output: "again" }, // already closed
      ]);
      h.unregisterChildSink("th-c");
      return { ok: true, output: { text: "done" } };
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"));
    const eventsP = collect(harness.startTurn(makeRequest()).events);
    await waitUntil(() => transport.responses.length === 1, "the parent reply");
    transport.push(turnCompleted("completed")).end();
    const events = await withTimeout(eventsP, 5000, "capped turn");

    const dispatchId = "cx-abcdef012345-t1-c-1";
    const childItems = frames(events)
      .filter((f) => f.origin.kind === "subagent")
      .flatMap((f) => f.items);
    assert.equal(childItems.length, 2002, "started + 1999 texts + marker + the paired finished");
    assert.deepEqual(childItems[0], { kind: "tool", phase: "started", id: `${dispatchId}/t1`, name: "Bash", input: { command: "ls" } });
    assert.deepEqual(childItems[2000], { kind: "text", text: "[further subagent output not shown]" });
    // Past the cap the closer still goes out, but with its output replaced by the constant.
    assert.deepEqual(childItems[2001], {
      kind: "tool",
      phase: "finished",
      id: `${dispatchId}/t1`,
      name: "Bash",
      output: "[output not shown: subagent output cap reached]",
      isError: false,
    });
    const tools = childItems.flatMap((i) => (i.kind === "tool" ? [`${i.phase}:${i.id}`] : []));
    assert.deepEqual(tools, [`started:${dispatchId}/t1`, `finished:${dispatchId}/t1`], "every projected started is paired");
  });

  it("caps projected child output per TURN across several dispatches; each later dispatch gets one marker; completions all go out", async () => {
    let harnessRef!: CodexHarness;
    // 64 x 60 KiB of text per dispatch stays under the 4 MiB per-dispatch budget, so only the
    // 16 MiB per-turn budget can cut: it runs out during the 5th dispatch.
    const big = "a".repeat(60 * 1024);
    const perDispatch = 64;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      const h = harnessRef;
      const child = `th-${rt.callId}`;
      h.registerChildSink(child, { push: () => {} });
      h.bindChildDispatch(child, rt, "coder");
      h.emitChildFrame(child, [{ kind: "tool", phase: "started", id: "t", name: "Bash", input: {} }]);
      for (let i = 0; i < perDispatch; i += 1) h.emitChildFrame(child, [{ kind: "text", text: big }]);
      h.emitChildFrame(child, [{ kind: "tool", phase: "finished", id: "t", name: "Bash", output: "ok" }]);
      h.emitChildFrame(child, [{ kind: "text", text: "tail" }]);
      h.unregisterChildSink(child);
      return { ok: true, output: { text: `done ${rt.callId}` } };
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    const calls = ["c-1", "c-2", "c-3", "c-4", "c-5", "c-6"];
    transport.push(threadStarted());
    calls.forEach((c, i) => transport.push(toolCall(i + 1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", c)));
    const eventsP = collect(harness.startTurn(makeRequest()).events);
    await waitUntil(() => transport.responses.length === calls.length, "every parent reply");
    transport.push(turnCompleted("completed")).end();
    const events = await withTimeout(eventsP, 10000, "turn-capped turn");

    const marker = "[further subagent output not shown]";
    let total = 0;
    for (const c of calls) {
      const dispatchId = `cx-abcdef012345-t1-${c}`;
      const items = frames(events)
        .filter((f) => f.origin.kind === "subagent" && f.origin.instanceId === dispatchId)
        .flatMap((f) => f.items);
      total += items.filter((i) => !(i.kind === "text" && i.text === marker)).reduce((n, i) => n + Buffer.byteLength(JSON.stringify(i), "utf8"), 0);
      const markers = items.filter((i) => i.kind === "text" && i.text === marker).length;
      const tools = items.flatMap((i) => (i.kind === "tool" ? [i.phase] : []));
      if (c <= "c-4") {
        assert.equal(markers, 0, `${c} is within the turn budget`);
        assert.equal(items.length, perDispatch + 3, `${c} projects everything`);
      } else {
        assert.equal(markers, 1, `${c} gets exactly one marker`);
        assert.ok(!items.some((i) => i.kind === "text" && i.text === "tail"), `${c} drops items after the marker`);
      }
      assert.equal(tools.filter((p) => p === "started").length, tools.filter((p) => p === "finished").length, `${c} tools are paired`);
      const completion = frames(events).flatMap((f) => f.items).find((i) => i.kind === "tool" && i.phase === "finished" && i.id === dispatchId);
      assert.deepEqual(completion, {
        kind: "tool",
        phase: "finished",
        id: dispatchId,
        name: "Agent",
        output: JSON.stringify({ text: `done ${c}` }),
        isError: false,
      });
    }
    assert.ok(total <= 16 * 1024 * 1024 + 64 * 1024, `the turn total stays bounded (${total} bytes)`);
  });

  const MARKER = "[further subagent output not shown]";
  const OMITTED = "[output not shown: subagent output cap reached]";
  const jsonBytes = (i: unknown): number => Buffer.byteLength(JSON.stringify(i), "utf8");
  const isMarker = (i: HarnessItem): boolean => i.kind === "text" && i.text === MARKER;

  /** Run ONE turn (on an un-ended transport, so a later turn can follow) whose spawn_agent calls
   *  `calls` each bind child `th-<call>` and run `body` on it synchronously, then settle. */
  async function runDispatchTurn(
    harness: CodexHarness,
    transport: ReturnType<typeof makeHarness>["transport"],
    calls: readonly string[],
    opts: { first: boolean; end: boolean },
  ): Promise<HarnessEvent[]> {
    const before = transport.responses.length;
    if (opts.first) transport.push(threadStarted());
    calls.forEach((c, i) => transport.push(toolCall(before + i + 1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", c)));
    const eventsP = collect(harness.startTurn(makeRequest()).events);
    await waitUntil(() => transport.responses.length === before + calls.length, "every parent reply");
    transport.push(turnCompleted("completed"));
    if (opts.end) transport.end();
    return withTimeout(eventsP, 20000, "dispatch turn");
  }

  function dispatchBroker(harnessRef: () => CodexHarness, body: (h: CodexHarness, child: string) => void): CodexCallbackBroker {
    return stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      const h = harnessRef();
      const child = `th-${rt.callId}`;
      h.registerChildSink(child, { push: () => {} });
      h.bindChildDispatch(child, rt, "coder");
      body(h, child);
      h.unregisterChildSink(child);
      return { ok: true, output: { text: `done ${rt.callId}` } };
    });
  }

  const childItemsOf = (events: HarnessEvent[], dispatchId: string): HarnessItem[] =>
    frames(events)
      .filter((f) => f.origin.kind === "subagent" && f.origin.instanceId === dispatchId)
      .flatMap((f) => f.items);

  it("a closing finished is COUNTED against the budgets; past them it closes its pair with the constant output (1999 starteds then 1999 16 KB finisheds)", async () => {
    let harnessRef!: CodexHarness;
    const n = 1999;
    const out = "o".repeat(16_000);
    const broker = dispatchBroker(
      () => harnessRef,
      (h, child) => {
        h.emitChildFrame(child, Array.from({ length: n }, (_, i) => ({ kind: "tool" as const, phase: "started" as const, id: `t${i}`, name: "Bash", input: {} })));
        h.emitChildFrame(
          child,
          Array.from({ length: n }, (_, i) => ({ kind: "tool" as const, phase: "finished" as const, id: `t${i}`, name: "Bash", output: out })),
        );
      },
    );
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    const calls = ["c-1", "c-2", "c-3", "c-4"];
    const events = await runDispatchTurn(harness, transport, calls, { first: true, end: true });

    const SMALL = 256; // an upper bound on one started or one constant-output closer, in JSON bytes
    let turnBytes = 0;
    let turnCount = 0;
    for (const c of calls) {
      const dispatchId = `cx-abcdef012345-t1-${c}`;
      const items = childItemsOf(events, dispatchId);
      const real = items.filter((i) => !isMarker(i));
      const bytes = real.reduce((b, i) => b + jsonBytes(i), 0);
      turnBytes += bytes;
      turnCount += real.length;
      assert.equal(items.filter(isMarker).length, 1, `${c} is capped with exactly one marker`);
      assert.ok(bytes <= 4 * 1024 * 1024 + real.length * SMALL, `${c} stays within its dispatch budget (${bytes} bytes)`);
      const started = real.flatMap((i) => (i.kind === "tool" && i.phase === "started" ? [i.id] : []));
      const finished = real.flatMap((i) => (i.kind === "tool" && i.phase === "finished" ? [i.id] : []));
      assert.deepEqual(finished, started, `${c}: every projected started has exactly one finished, in order`);
      const full = real.filter((i) => i.kind === "tool" && i.phase === "finished" && i.output !== OMITTED);
      assert.ok(full.length < 300, `${c}: only the closers within budget carry their output (${full.length})`);
      for (const i of real) if (i.kind === "tool" && i.phase === "finished" && i.output === OMITTED) assert.ok(jsonBytes(i) <= SMALL);
      const completion = frames(events).flatMap((f) => f.items).find((i) => i.kind === "tool" && i.phase === "finished" && i.id === dispatchId);
      assert.ok(completion?.kind === "tool" && completion.phase === "finished" && completion.isError === false, `${c} still completes`);
    }
    assert.ok(turnBytes <= 16 * 1024 * 1024 + turnCount * SMALL, `the turn stays within its budget (${turnBytes} bytes)`);
    assert.ok(turnBytes < 4 * 1024 * 1024, `the 16 KB outputs of over-budget closers were not projected (${turnBytes} bytes)`);
  });

  it("caps projected child ITEMS per turn when every dispatch stays under its own item cap", async () => {
    let harnessRef!: CodexHarness;
    const perDispatch = 1500; // under the 2000 per-dispatch cap; 8 x 1500 = 12000 > the 10000 per-turn cap
    const broker = dispatchBroker(
      () => harnessRef,
      (h, child) => {
        h.emitChildFrame(child, Array.from({ length: perDispatch }, (_, i) => ({ kind: "text" as const, text: `p${i}` })));
        h.emitChildFrame(child, [{ kind: "text", text: "tail" }]);
      },
    );
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    const calls = ["c-1", "c-2", "c-3", "c-4", "c-5", "c-6", "c-7", "c-8"];
    const events = await runDispatchTurn(harness, transport, calls, { first: true, end: true });

    let total = 0;
    const markers: number[] = [];
    for (const c of calls) {
      const items = childItemsOf(events, `cx-abcdef012345-t1-${c}`);
      total += items.filter((i) => !isMarker(i)).length;
      markers.push(items.filter(isMarker).length);
    }
    assert.equal(total, 10_000, "exactly the per-turn item budget is projected");
    assert.deepEqual(markers, [0, 0, 0, 0, 0, 0, 1, 1], "the dispatches past the turn item budget get one marker each");
  });

  it("the per-turn child budget resets in startTurn: turn 2 projects fully after a turn 1 near both turn budgets", async () => {
    let harnessRef!: CodexHarness;
    // 1999 x ~1.6 KB per dispatch: under the per-dispatch item and byte caps. Five dispatches reach
    // 9995 items and ~16.2 MB, just under both per-turn budgets; one more dispatch would exceed both.
    const text = "x".repeat(1600);
    const broker = dispatchBroker(
      () => harnessRef,
      (h, child) => h.emitChildFrame(child, Array.from({ length: 1999 }, () => ({ kind: "text" as const, text }))),
    );
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    const turn1Calls = ["c-1", "c-2", "c-3", "c-4", "c-5"];
    const events1 = await runDispatchTurn(harness, transport, turn1Calls, { first: true, end: false });
    const turn1 = turn1Calls.flatMap((c) => childItemsOf(events1, `cx-abcdef012345-t1-${c}`));
    assert.equal(turn1.filter(isMarker).length, 0, "turn 1 stays under the turn budget");
    assert.equal(turn1.length, 5 * 1999);

    const events2 = await runDispatchTurn(harness, transport, ["c-6"], { first: false, end: true });
    const turn2 = childItemsOf(events2, "cx-abcdef012345-t2-c-6");
    assert.equal(turn2.filter(isMarker).length, 0, "turn 2 starts with a fresh turn budget");
    assert.equal(turn2.length, 1999, "turn 2 projects fully");
  });

  it("closing a bound dispatch with child tools still open synthesizes their finisheds BEFORE the lead completion (also after the child sink unregistered)", async () => {
    let harnessRef!: CodexHarness;
    let bound = 0;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      const h = harnessRef;
      const child = `th-${rt.callId}`;
      h.registerChildSink(child, { push: () => {} });
      h.bindChildDispatch(child, rt, "coder");
      h.emitChildFrame(child, [
        { kind: "tool", phase: "started", id: "a", name: "Bash", input: {} },
        { kind: "tool", phase: "started", id: "b", name: "Read", input: {} },
        { kind: "tool", phase: "finished", id: "b", name: "Read", output: "ok" },
        { kind: "tool", phase: "started", id: "a", name: "Bash", input: {} },
      ]);
      if (rt.callId === "c-2") h.unregisterChildSink(child);
      bound += 1;
      return new Promise<CallbackResult>(() => {});
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    transport
      .push(threadStarted())
      .push(toolCall(1, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-1"))
      .push(toolCall(2, "spawn_agent", { subagent_type: "coder" }, "th-1", "tn-1", "c-2"));
    const turn = harness.startTurn(makeRequest());
    const eventsP = collect(turn.events);
    await waitUntil(() => bound === 2, "both dispatches bound");
    turn.requestStop("cancel");
    const events = await withTimeout(eventsP, 2000, "stopped turn");

    for (const c of ["c-1", "c-2"]) {
      const dispatchId = `cx-abcdef012345-t1-${c}`;
      const items = childItemsOf(events, dispatchId);
      const started = items.flatMap((i) => (i.kind === "tool" && i.phase === "started" ? [i.id] : []));
      const finished = items.flatMap((i) => (i.kind === "tool" && i.phase === "finished" ? [i.id] : []));
      assert.equal(started.length, 3);
      assert.deepEqual([...finished].sort(), [...started].sort(), `${c}: every child started is finished`);
      const synthesized = items.slice(-2);
      assert.deepEqual(synthesized, [
        { kind: "tool", phase: "finished", id: started[0], name: "Bash", output: "tool result not confirmed: delegation closed", isError: true },
        { kind: "tool", phase: "finished", id: started[2], name: "Bash", output: "tool result not confirmed: delegation closed", isError: true },
      ]);
      const lastChildAt = events.findLastIndex((e) => e.kind === "frame" && e.origin.kind === "subagent" && e.origin.instanceId === dispatchId);
      const completionAt = events.findIndex(
        (e) => e.kind === "frame" && e.origin.kind === "main" && e.items.some((i) => i.kind === "tool" && i.phase === "finished" && i.id === dispatchId),
      );
      assert.ok(lastChildAt >= 0 && completionAt > lastChildAt, `${c}: the synthesized child finisheds precede the lead completion`);
    }
  });

  it("a dispatch that SETTLES (child_timeout) with a child tool still open synthesizes its finished BEFORE the lead completion", async () => {
    let harnessRef!: CodexHarness;
    const broker = stubBroker(async (rt, name) => {
      if (name !== "spawn_agent") return { ok: true, output: {} };
      const h = harnessRef;
      h.registerChildSink("th-c", { push: () => {} });
      h.bindChildDispatch("th-c", rt, "coder");
      // The child starts a tool and is wedged in it; the per-child deadline then settles the delegate.
      h.emitChildFrame("th-c", [
        { kind: "tool", phase: "started", id: "w", name: "Bash", input: { command: "sleep 1d" } },
        { kind: "tool", phase: "started", id: "d", name: "Read", input: {} },
        { kind: "tool", phase: "finished", id: "d", name: "Read", output: "ok" },
      ]);
      h.unregisterChildSink("th-c");
      return { ok: false, code: "child_timeout", message: "child turn exceeded its deadline" };
    });
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    const events = await runDispatchTurn(harness, transport, ["c-1"], { first: true, end: true });

    const dispatchId = "cx-abcdef012345-t1-c-1";
    const items = childItemsOf(events, dispatchId);
    assert.deepEqual(items.at(-1), {
      kind: "tool",
      phase: "finished",
      id: `${dispatchId}/w`,
      name: "Bash",
      output: "tool result not confirmed: delegation closed",
      isError: true,
    });
    const started = items.flatMap((i) => (i.kind === "tool" && i.phase === "started" ? [i.id] : []));
    const finished = items.flatMap((i) => (i.kind === "tool" && i.phase === "finished" ? [i.id] : []));
    assert.deepEqual([...finished].sort(), [...started].sort(), "no child tool is left running");
    const lastChildAt = events.findLastIndex((e) => e.kind === "frame" && e.origin.kind === "subagent");
    const completionAt = events.findIndex(
      (e) => e.kind === "frame" && e.origin.kind === "main" && e.items.some((i) => i.kind === "tool" && i.phase === "finished" && i.id === dispatchId),
    );
    assert.ok(lastChildAt >= 0 && completionAt > lastChildAt, "the synthesized child finished precedes the lead completion");
    const completionFrame = events[completionAt];
    const completion = completionFrame?.kind === "frame" ? completionFrame.items[0] : undefined;
    assert.ok(completion?.kind === "tool" && completion.phase === "finished" && completion.isError === true, "the lead completion reports the timeout");
  });

  it("two child starteds sharing a raw call id each get their own finished, first finished closing the first started", async () => {
    let harnessRef!: CodexHarness;
    const broker = dispatchBroker(
      () => harnessRef,
      (h, child) => {
        h.emitChildFrame(child, [
          { kind: "tool", phase: "started", id: "dup", name: "Bash", input: { n: 1 } },
          { kind: "tool", phase: "started", id: "dup", name: "Bash", input: { n: 2 } },
        ]);
        h.emitChildFrame(child, [
          { kind: "tool", phase: "finished", id: "dup", name: "Bash", output: "first" },
          { kind: "tool", phase: "finished", id: "dup", name: "Bash", output: "second" },
          { kind: "tool", phase: "finished", id: "dup", name: "Bash", output: "third: nothing open" },
        ]);
      },
    );
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    harnessRef = harness;
    const events = await runDispatchTurn(harness, transport, ["c-1"], { first: true, end: true });
    const items = childItemsOf(events, "cx-abcdef012345-t1-c-1");
    assert.equal(items.length, 4);
    const [s1, s2, f1, f2] = items as [HarnessItem, HarnessItem, HarnessItem, HarnessItem];
    assert.ok(s1.kind === "tool" && s2.kind === "tool" && f1.kind === "tool" && f2.kind === "tool");
    assert.notEqual(s1.id, s2.id, "distinct projected ids");
    assert.deepEqual([f1.id, f1.phase === "finished" ? f1.output : undefined], [s1.id, "first"]);
    assert.deepEqual([f2.id, f2.phase === "finished" ? f2.output : undefined], [s2.id, "second"]);
  });
});

describe("CodexHarness: projection is fail-safe (issue #1583)", () => {
  function toolItems(events: HarnessEvent[]): Extract<HarnessEvent, { kind: "frame" }>["items"][number][] {
    return events.flatMap((e) => (e.kind === "frame" ? e.items : [])).filter((i) => i.kind === "tool");
  }

  it("a Bash call with 20000-deep nested args still reaches the broker once and gets exactly one reply", async () => {
    let deep: unknown = [];
    for (let i = 0; i < 20_000; i++) deep = [deep];
    let brokerCalls = 0;
    const broker = stubBroker(async () => {
      brokerCalls += 1;
      return { ok: true, output: { code: 0, stdout: "hi", stderr: "" } };
    });
    const { harness, transport } = makeHarness({ broker });
    transport
      .push(threadStarted())
      .push(toolCall(9, "Bash", { command: "echo hi", x: deep }, "th-1", "tn-1", "deep"))
      .push(turnCompleted("completed"))
      .end();
    const events = await withTimeout(collect(harness.startTurn(makeRequest()).events), 5000, "the turn to complete");
    assert.equal(brokerCalls, 1, "the broker ran exactly once");
    assert.equal(transport.responses.filter((r) => r.requestId === 9).length, 1, "exactly one reply");
    assert.equal(transport.responses.length, 1);
    assert.equal(events.at(-1)?.kind, "turn_finished");
    const tools = toolItems(events);
    assert.equal(tools.length, 2, "a started/finished pair");
    const [started, finished] = tools;
    assert.ok(started?.kind === "tool" && started.phase === "started");
    assert.ok(finished?.kind === "tool" && finished.phase === "finished");
    assert.equal(started.id, finished.id);
    assert.equal(finished.isError, false);
  });

  it("a projection that throws falls back to a minimal frame pair and never blocks the broker or the reply", async () => {
    let brokerCalls = 0;
    const broker = stubBroker(async () => {
      brokerCalls += 1;
      return { ok: true, output: { code: 0 } };
    });
    const { harness, transport } = makeHarness({
      broker,
      scrubProjected: () => {
        throw new Error("redactor exploded");
      },
    });
    transport.push(threadStarted()).push(toolCall(5, "Bash", { command: "echo hi" })).push(turnCompleted("completed")).end();
    const events = await withTimeout(collect(harness.startTurn(makeRequest()).events), 5000, "the turn to complete");
    assert.equal(brokerCalls, 1);
    assert.equal(transport.responses.length, 1, "exactly one reply");
    assert.equal(events.at(-1)?.kind, "turn_finished");
    const tools = toolItems(events);
    assert.equal(tools.length, 2);
    const [started, finished] = tools;
    assert.ok(started?.kind === "tool" && started.phase === "started");
    assert.equal(started.name, "unknown");
    assert.deepEqual(started.input, { truncated: true });
    assert.ok(finished?.kind === "tool" && finished.phase === "finished");
    assert.equal(finished.output, "[projection failed]");
  });

  it("two root calls in one turn with the SAME call id get distinct ids, each finished paired with its own started", async () => {
    const broker = stubBroker(async (_rt, _name, args) => ({ ok: true, output: { echoed: (args as { command: string }).command } }));
    const { harness, transport } = makeHarness({ broker, idNonce: "abcdef012345" });
    transport
      .push(threadStarted())
      .push(toolCall(1, "Bash", { command: "first" }, "th-1", "tn-1", "dup"))
      .push(toolCall(2, "Bash", { command: "second" }, "th-1", "tn-1", "dup"))
      // Two call ids that sanitise to the same part collide too.
      .push(toolCall(3, "Bash", { command: "third" }, "th-1", "tn-1", "d<u>p"))
      .push(turnCompleted("completed"))
      .end();
    const tools = toolItems(await collect(harness.startTurn(makeRequest()).events));
    assert.equal(tools.length, 6);
    const started = tools.filter((t) => t.kind === "tool" && t.phase === "started");
    const finished = tools.filter((t) => t.kind === "tool" && t.phase === "finished");
    const ids = started.map((t) => (t.kind === "tool" ? t.id : ""));
    assert.equal(new Set(ids).size, 3, `distinct ids: ${ids.join(", ")}`);
    assert.equal(ids[0], "cx-abcdef012345-t1-dup");
    for (const [i, cmd] of ["first", "second", "third"].entries()) {
      const s = started[i]!;
      assert.ok(s.kind === "tool");
      assert.deepEqual(s.input, { command: cmd });
      const f = finished.find((t) => t.kind === "tool" && t.id === s.id);
      assert.ok(f?.kind === "tool", `a finished frame for ${s.id}`);
      assert.equal(f.output, JSON.stringify({ echoed: cmd }), "the finished frame pairs with its own started");
    }
  });
});

describe("CodexHarness: sequential turn lifecycle", () => {
  it("reuses the thread and transport across two clean turns", async () => {
    let turnNumber = 0;
    const transport = new FakeTransport((method) => {
      if (method === "turn/start") {
        turnNumber += 1;
        return { turn: { id: `tn-${turnNumber}` } };
      }
      return defaultResponder(method);
    });
    const { harness } = makeHarness({ transport });

    transport.push(threadStarted()).push(turnCompleted("completed", undefined, "th-1", "tn-1"));
    await collect(harness.startTurn(makeRequest()).events);
    transport.push(turnCompleted("completed", undefined, "th-1", "tn-2"));
    await collect(harness.startTurn(makeRequest()).events);

    assert.equal(transport.requests.filter((r) => r.method === "thread/start").length, 1);
    assert.equal(transport.requests.filter((r) => r.method === "turn/start").length, 2);
    assert.equal(transport.closes, 0);
  });

  it("retains an abandoned notification read for the next turn after owner abort", async () => {
    let turnNumber = 0;
    const transport = new FakeTransport((method) => {
      if (method === "turn/start") {
        turnNumber += 1;
        return { turn: { id: `tn-${turnNumber}` } };
      }
      return defaultResponder(method);
    });
    const { harness } = makeHarness({ transport });
    const firstAbort = new AbortController();
    transport.push(threadStarted());
    const first = harness.startTurn(makeRequest({ signal: firstAbort.signal })).events[Symbol.asyncIterator]();
    assert.equal((await first.next()).done, false);
    const abandonedRead = first.next();
    await tick();
    firstAbort.abort();
    assert.equal((await abandonedRead).done, true);

    const secondEvents = collect(harness.startTurn(makeRequest()).events);
    await tick();
    // The retained read receives a stale terminal and maps it to liveness; the real
    // second-turn terminal is then consumed normally instead of being lost.
    transport.push(turnCompleted("completed", undefined, "th-1", "tn-1"));
    await tick();
    transport.push(turnCompleted("completed", undefined, "th-1", "tn-2"));
    const events = await secondEvents;
    assert.deepEqual(events.map((event) => event.kind), ["activity", "turn_finished"]);
  });
});

describe("CodexHarness: error precedence + lifecycle", () => {
  it("unexpected EOF mid-turn (no terminal) throws a protocol error, not a fabricated success", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnStarted()).end(); // stream ends before turn/completed

    await assert.rejects(collect(harness.startTurn(makeRequest()).events), (err: unknown) => {
      assert.ok(err instanceof CodexHarnessError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /ended before turn completion/);
      return true;
    });
  });

  it("unknown failed startup retains uncertainty after reservation cancellation and terminal disposal", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    let reject!: (error: Error) => void;
    const pending = new Promise<never>((_, fail) => { reject = fail; });
    const boom = new Error("unknown native startup");
    const { harness } = makeHarness({ registry, launchRoot: () => pending });
    const turn = collect(harness.startTurn(makeRequest()).events);
    await tick();
    assert.equal(registry.pendingLaunchCount(), 1);
    reject(boom);
    await assert.rejects(turn, error => error === boom);
    assert.equal(registry.pendingLaunchCount(), 0);
    assert.equal((await registry.settleForCapture(1000)).kind, "incomplete");
    assert.equal((await registry.disposeTools(1000)).kind, "incomplete");
    assert.equal((await registry.settleForCapture(1000)).kind, "incomplete");
  });

  it("an unknown launch failure settles its reservation but retains uncertainty and the setup error", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const boom = new Error("launch failed");
    const { harness } = makeHarness({ registry, launchRoot: async () => { throw boom; } });

    await assert.rejects(collect(harness.startTurn(makeRequest()).events), (err: unknown) => err === boom);
    assert.equal(registry.pendingLaunchCount(), 0, "the reservation was settled (cancelled)");
    assert.equal(registry.isPoisoned(), true, "canceling a reservation cannot prove failed-start cleanup");
    assert.equal((await registry.settleForCapture(1_000)).kind, "incomplete");
  });

  it("aborts the launch and tears the root down when registry admission fails (finding 6)", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    let disposed = 0;
    let reaps = 0;
    let releaseReap!: (value: { ok: true }) => void;
    const reaping = new Promise<{ ok: true }>(resolve => { releaseReap = resolve; });
    // A root whose declared kind MISMATCHES the reserved "provider" kind: the REAL registry
    // poisons and registerRoot returns { ok: false }, so ensureRoot must not proceed on it.
    const mismatchedRoot: RegisteredRoot = {
      kind: "command",
      reap: () => { reaps++; return reaping; },
      dispose: async () => {
        disposed += 1;
      },
    };
    const transport = new FakeTransport();
    // Frames are pre-loaded so that CURRENT (pre-fix) code — which ignores the admission
    // result and proceeds — would finish the turn WITHOUT throwing, making this a true
    // regression test: it only rejects once ensureRoot honours the failed admission.
    transport.push(threadStarted()).push(turnCompleted("completed")).end();
    const { harness } = makeHarness({
      registry,
      transport,
      launchRoot: async () => ({ root: mismatchedRoot, transport, supervisorPid: 1 }),
    });

    let finished = false;
    const turn = collect(harness.startTurn(makeRequest()).events);
    const rejection = assert.rejects(turn, (err: unknown) => {
      assert.ok(err instanceof CodexHarnessError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /failed registry admission/);
      return true;
    });
    void turn.then(() => { finished = true; }, () => { finished = true; });
    try {
      await tick();
      assert.equal(registry.rootCount(), 1, "rejected registration retains the launched root");
      assert.equal(registry.pendingLaunchCount(), 0, "retained root settles its reservation");
      assert.equal(reaps, 1, "startup joins the launched root's reap");
      assert.equal(finished, false, "startup cannot reject before owned reaping settles");
    } finally {
      releaseReap({ ok: true });
      await rejection;
    }
    assert.equal((await registry.settleForCapture(1000)).kind, "incomplete");
    // The registry poisoned on the kind mismatch, and the just-launched root was torn down:
    // its transport was closed and it was disposed rather than left to a (dead) reap.
    assert.equal(registry.isPoisoned(), true, "the admission failure poisoned the epoch");
    assert.equal(transport.closes, 1, "the just-launched transport was closed");
    assert.equal(disposed, 1, "the just-launched root was disposed");

    // this.transport was NEVER assigned: a second turn re-enters ensureRoot (rather than
    // short-circuiting on a set transport) and is refused by the now-poisoned registry.
    await assert.rejects(collect(harness.startTurn(makeRequest()).events), (err: unknown) => {
      assert.ok(err instanceof CodexHarnessError);
      assert.match(err.message, /launch admission is closed/);
      return true;
    });
  });

  it("requestStop interrupts the in-flight turn (a cancellation request, not a process kill)", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()); // no end(): the turn stays in flight
    const turn = harness.startTurn(makeRequest());
    await turn.events[Symbol.asyncIterator]().next(); // setup done → activeTurnId known

    turn.requestStop("cancel");
    const interrupt = transport.requests.find((r) => r.method === "turn/interrupt");
    assert.ok(interrupt, "an interrupt was requested");
    assert.deepEqual(interrupt.params, { threadId: "th-1", turnId: "tn-1" });
  });

  it("honors requestStop between startTurn and the iterable's first next", async () => {
    const { harness, transport } = makeHarness();
    const turn = harness.startTurn(makeRequest());
    turn.requestStop("cancel");
    assert.deepEqual(await collect(turn.events), []);
    assert.equal(transport.requests.length, 0, "the stopped lazy turn launches no model work");
  });

  it("reentrant transport close observes the shared pending completion and sticky rejection", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnCompleted("completed")).end();
    await collect(harness.startTurn(makeRequest()).events);
    const cleanup = deferred<void>();
    const failure = new Error("reentrant transport close failed");
    let reentered: Promise<void> | undefined;
    let closes = 0;
    transport.close = () => {
      closes++;
      reentered = harness.close();
      return cleanup.promise;
    };
    const first = harness.close();
    assert.ok(reentered);
    const settled: unknown[] = [];
    const observe = (work: Promise<void>) => work.then(
      () => { settled.push("fulfilled"); }, error => { settled.push(error); });
    const joined = Promise.all([observe(first), observe(reentered)]);
    try {
      await tick();
      assert.deepEqual(settled, [], "a reentrant close cannot finish before owned cleanup");
      cleanup.reject(failure);
      await joined;
      assert.deepEqual(settled, [failure, failure]);
      await assert.rejects(harness.close(), error => error === failure);
      assert.equal(closes, 1);
    } finally { cleanup.reject(failure); await joined; }
  });

  it("concurrent and repeated close callers share pending completion and rejection", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).push(turnCompleted("completed")).end();
    await collect(harness.startTurn(makeRequest()).events);
    const cleanup = deferred<void>();
    const failure = new Error("transport close failed");
    let closes = 0;
    transport.close = () => { closes++; return cleanup.promise; };
    const first = harness.close();
    const second = harness.close();
    const settled: unknown[] = [];
    const observe = (work: Promise<void>) => work.then(
      () => { settled.push("fulfilled"); }, error => { settled.push(error); });
    const joined = Promise.all([observe(first), observe(second)]);
    try {
      await tick();
      assert.deepEqual(settled, [], "every close caller waits for owned cleanup");
      cleanup.reject(failure);
      await joined;
      assert.deepEqual(settled, [failure, failure]);
      await assert.rejects(harness.close(), error => error === failure);
      assert.equal(closes, 1);
    } finally { cleanup.reject(failure); await joined; }
  });

  it("close() is idempotent and does NOT reap / dispose / quiesce the registry", async () => {
    const { harness, transport, registry } = makeHarness();
    transport.push(threadStarted()).push(turnCompleted("completed")).end();
    const turn = harness.startTurn(makeRequest());
    await collect(turn.events); // run a turn so the root/transport exist

    await turn.close();
    await turn.close(); // idempotent — no throw
    assert.equal(transport.closes, 1, "close closes the run-scoped transport exactly once");
    // The registry was never quiesced/reaped/disposed by close(): it stays open with the
    // one registered provider root.
    assert.equal(registry.state(), "open");
    assert.equal(registry.rootCount(), 1);
    assert.throws(
      () => harness.startTurn(makeRequest()),
      (err: unknown) => err instanceof CodexHarnessError && /harness is closed/.test(err.message),
      "a closed cached transport can never be reused by a later turn",
    );
  });

  it("readContext is bounded and returns undefined on absence", async () => {
    const { harness } = makeHarness();
    const turn = harness.startTurn(makeRequest());
    assert.equal(await turn.readContext(2000), undefined);
  });
});

describe("CodexHarness: process-safety delegation + session inspection", () => {
  it("quiesce / reap / dispose delegate straight to the registry", async () => {
    const calls: { q?: number; r?: { dl: number; ep: number }; d?: number } = {};
    const spyRegistry = {
      quiesceChildren: (dl: number) => {
        calls.q = dl;
        return Promise.resolve({ kind: "quiescent", epoch: 7 });
      },
      reapProcesses: (dl: number, ep: number) => {
        calls.r = { dl, ep };
        return Promise.resolve({ kind: "observed_empty", evidence: "supervisor_echild", epoch: ep });
      },
      disposeTools: (dl: number) => {
        calls.d = dl;
        return Promise.resolve({ kind: "disposed" });
      },
    } as unknown as ExecutionRegistry;
    const { harness } = makeHarness({ registry: spyRegistry });

    const q = await harness.quiesceChildren({ boundary: "terminal", deadlineMs: 111 });
    assert.deepEqual(q, { kind: "quiescent", epoch: 7 });
    assert.equal(calls.q, 111);

    const r = await harness.reapProcesses({ boundary: "terminal", deadlineMs: 222 }, 7);
    assert.equal(r.kind, "observed_empty");
    assert.deepEqual(calls.r, { dl: 222, ep: 7 });

    const d = await harness.disposeTools({ boundary: "terminal", deadlineMs: 333 });
    assert.equal(d.kind, "disposed");
    assert.equal(calls.d, 333);
  });

  it("inspectSession routes through the injected seam", async () => {
    const { harness } = makeHarness({ sessionInspect: async (id) => (id === "abc" ? "present" : "absent") });
    assert.equal(await harness.inspectSession("abc"), "present");
    assert.equal(await harness.inspectSession("xyz"), "absent");
  });
});

for (const optedIn of [false, true]) {
  it(`U3 D9 actual first planning thread consent=${optedIn} retains registration on resume without new params`, async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).end();
    const access = { generation: 7, codeCrossCheckRepairActive: () => true, codeCrossCheckRepair: async () => undefined,
      reportCrossCheckDispositions: async () => ({ stage: "code" as const, result: "no_row" as const, repo_instructions_enabled: false }) };
    await harness.startTurn(makeRequest({ phase: "plan", ...(optedIn ? { codeCrossCheck: access } : {}) })).events[Symbol.asyncIterator]().next();
    const params = rec(transport.requests.find(r => r.method === "thread/start")!.params);
    assert.equal((params.dynamicTools as Array<{ name: string }>).some(t => t.name === "report_cross_check_dispositions"), optedIn);
    const resumed = makeHarness();
    resumed.transport.push(threadStarted("th-1")).end();
    await resumed.harness.startTurn(makeRequest({ phase: "implement", resumeSessionId: "th-1",
      ...(optedIn ? { codeCrossCheck: access } : {}) })).events[Symbol.asyncIterator]().next();
    assert.ok(resumed.transport.requests.some(r => r.method === "thread/resume"));
    for (const request of [...transport.requests, ...resumed.transport.requests]) {
      if (request.method !== "thread/start") {
        assert.equal(rec(request.params).dynamicTools, undefined, request.method);
        assert.equal(rec(request.params).dynamic_tools, undefined, request.method);
      }
    }
    await harness.close();
    await resumed.harness.close();
  });
}
