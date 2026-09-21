import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CodexHarness,
  CodexHarnessError,
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
import { renderCodexRun } from "../src/codex/render.js";
import type { HarnessEvent, HarnessTerminal, RunTurnRequest } from "../src/harness.js";
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
  private waiter: ((r: IteratorResult<CodexNotification>) => void) | undefined;
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
      new Promise((resolve) => {
        const q = this.queue.shift();
        if (q !== undefined) {
          resolve({ value: q, done: false });
          return;
        }
        if (this.ended) {
          resolve({ value: undefined, done: true });
          return;
        }
        this.waiter = resolve;
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
  const error: Record<string, unknown> = { codexErrorInfo, ...(opts.extraError ?? {}) };
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
): CodexNotification {
  return {
    kind: "token_usage_updated",
    method: "thread/tokenUsage/updated",
    threadId,
    turnId,
    usage: { total: bd(total), last: bd(last) },
    params: { threadId, turnId },
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
    log: noopLog,
    sessionInspect: opts.sessionInspect ?? (async () => "unknown"),
    appServerAuth: opts.appServerAuth,
    credentialValue: opts.credentialValue,
    authMode: opts.authMode,
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
      (err: unknown) => err instanceof CodexHarnessError && /thread\/resume returned no thread id/.test(err.message),
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
    assert.equal(params.modelReasoningEffort, "high");
    assert.deepEqual(params.input, [{ type: "text", text: "do the thing" }]);
  });
});

describe("CodexHarness: frame → neutral event decode", () => {
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

  it("a subscription run marks the entry subscription (no costUSD) and the run cost subscription", async () => {
    const { harness, transport } = makeHarness({ authMode: "subscription" });
    transport
      .push(threadStarted())
      .push(turnStarted())
      .push(tokenUsage({ inputTokens: 300, outputTokens: 200, totalTokens: 500 }, { inputTokens: 300, outputTokens: 200, totalTokens: 500 }))
      .push(turnCompleted("completed"))
      .end();
    const term = terminal(await collect(harness.startTurn(makeRequest()).events));
    const mu = entryOf(term, "gpt-6-astra");
    assert.equal(mu.costStatus, "subscription");
    assert.ok(!("costUSD" in mu));
    assert.deepEqual(term.metrics.cost, { kind: "subscription" });
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

    // The tool-call is intercepted (activity), never a frame.
    assert.equal(events.filter((e) => e.kind === "frame").length, 0);
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

    await collect(harness.startTurn(request).events);
    const result = rec(rec(transport.responses[0]!.response).result);
    assert.equal(result.success, false);
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

  it("refuses a non-tool server request and never calls the broker", async () => {
    let brokerCalls = 0;
    const broker = stubBroker(async () => {
      brokerCalls += 1;
      return { ok: true, output: {} };
    });
    const { harness, transport } = makeHarness({ broker });
    transport
      .push(threadStarted())
      .push(serverRequest(17, "account/login/refresh"))
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

  it("a non-signal tool call still yields activity and NO signals frame (byte-identical path)", async () => {
    const request = makeRequest();
    const { harness, transport } = makeHarness({ broker: signalBroker(request) });
    transport
      .push(threadStarted())
      .push(toolCall(7, "Bash", { command: "echo hi" }, "th-1", "tn-1", "c1"))
      .push(turnCompleted("completed"))
      .end();

    const events = await collect(harness.startTurn(request).events);
    assert.equal(events.filter((e) => e.kind === "frame").length, 0, "a shell effect emits no signals frame");
    // The tool call was still intercepted + replied, surfaced only as activity.
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

    const pending = iter.next(); // drives the tool-call → wedges in the broker callback
    await withTimeout(entered, 1000, "the broker callback to be entered");

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
    const pending = iter.next(); // wedges in the broker callback
    await withTimeout(entered, 1000, "the broker callback to be entered");

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
    const pending = iter.next(); // wedges
    await withTimeout(entered, 1000, "the broker callback to be entered");

    ac.abort();
    await withTimeout(pending, 1000, "the stream to end on abort");
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

  it("a launch failure cancels the reservation (no poison) and re-throws the setup error", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const boom = new Error("launch failed");
    const { harness } = makeHarness({ registry, launchRoot: async () => { throw boom; } });

    await assert.rejects(collect(harness.startTurn(makeRequest()).events), (err: unknown) => err === boom);
    assert.equal(registry.pendingLaunchCount(), 0, "the reservation was settled (cancelled)");
    assert.equal(registry.isPoisoned(), false, "a launch abort cancels, it does not poison");
  });

  it("aborts the launch and tears the root down when registry admission fails (finding 6)", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    let disposed = 0;
    // A root whose declared kind MISMATCHES the reserved "provider" kind: the REAL registry
    // poisons and registerRoot returns { ok: false }, so ensureRoot must not proceed on it.
    const mismatchedRoot: RegisteredRoot = {
      kind: "command",
      reap: async () => ({ ok: true }),
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

    await assert.rejects(collect(harness.startTurn(makeRequest()).events), (err: unknown) => {
      assert.ok(err instanceof CodexHarnessError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /failed registry admission/);
      return true;
    });
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
