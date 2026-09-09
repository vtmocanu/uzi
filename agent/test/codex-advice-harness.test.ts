import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CodexAdviceHarness,
  CodexAdviceError,
  type CodexAdviceHarnessOptions,
  type CodexAdviceLaunchResult,
  type CodexAdviceLaunchSpec,
  type LaunchAdviceRootSeam,
} from "../src/codex/codex-advice-harness.js";
import type { CodexProviderConfig } from "../src/codex/codex-harness.js";
import type { AdviceRequest, AdviceResultPolicy } from "../src/harness.js";
import type { CodexNotification, CodexTransport } from "../src/codex/transport.js";
import type { Logger } from "../src/log.js";
import {
  createCodexAppServerAuth,
  type CodexAppServerAuthSession,
} from "../src/codex/appserver-auth.js";

// PRD #1171 (M3, milestone 3) — the Codex TOOL-LESS, isolated advice pass, driven with an
// in-memory transport and scripted app-server frames (NO real Codex process). Every
// external effect is an injected seam; there is NO registry and NO broker in this lane, so
// the model has NO tool surface (the advice ceiling).

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

function defaultResponder(method: string): unknown {
  if (method === "thread/start") return { thread: { id: "th-1" } };
  if (method === "turn/start") return { turn: { id: "tn-1" } };
  return {};
}

function authResponder(method: string, params: unknown): unknown {
  if (method === "initialize") {
    return { userAgent: "codex/0.153.2", codexHome: "/owned/codex", platformFamily: "unix", platformOs: "linux" };
  }
  if (method === "account/login/start") {
    return { type: (params as { type?: unknown } | undefined)?.type };
  }
  return defaultResponder(method);
}

/** A scriptable in-memory {@link CodexTransport}: request/respond/notify calls are
 *  captured, requests answer from an injected responder, and `notifications()` drains a
 *  queue the test pre-loads via {@link FakeTransport.push} / {@link FakeTransport.end}. */
class FakeTransport implements CodexTransport {
  requests: { method: string; params: unknown }[] = [];
  responses: { requestId: number | string; response: unknown }[] = [];
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
    // Mirror the real transport's closed-guard: a respond after close throws, so a refuse
    // that races a torn-down transport exercises refuseServerRequest's guard.
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

function agentMessage(text: string, threadId = "th-1"): CodexNotification {
  return { kind: "activity", method: "item/completed", params: { threadId, item: { type: "agentMessage", text } } };
}

function toolCall(requestId: number, tool: string, args: unknown, threadId = "th-1"): CodexNotification {
  return {
    kind: "activity",
    method: "item/tool/call",
    requestId,
    params: { threadId, turnId: "tn-1", callId: "c1", tool, arguments: args, namespace: null },
  };
}

// --- request + harness builders -----------------------------------------------------

function makeAdviceRequest(overrides: Partial<AdviceRequest> = {}): AdviceRequest {
  return {
    label: "review",
    systemPrompt: "you are the reviewer",
    prompt: "review this",
    model: "gpt-6-astra",
    output: { kind: "text" },
    signal: new AbortController().signal,
    timeoutMs: 5000,
    ...overrides,
  };
}

/** A policy that never throws — records the terminal context so a test can inspect it. */
function recordingPolicy(): { policy: AdviceResultPolicy; seen: { isError: boolean }[] } {
  const seen: { isError: boolean }[] = [];
  return { policy: { onTerminal: (_t, ctx) => seen.push({ isError: ctx.isError }) }, seen };
}

const noThrowPolicy: AdviceResultPolicy = { onTerminal: () => {} };

interface Bits {
  harness: CodexAdviceHarness;
  transport: FakeTransport;
  launchSpecs: CodexAdviceLaunchSpec[];
  disposeCalls: () => number;
}

function makeHarness(
  opts: {
    transport?: FakeTransport;
    credentialValue?: string;
    cwd?: string;
    provider?: CodexProviderConfig;
    appServerAuth?: CodexAppServerAuthSession;
    disposeThrows?: boolean;
    log?: Logger;
  } = {},
): Bits {
  const transport = opts.transport ?? new FakeTransport();
  const launchSpecs: CodexAdviceLaunchSpec[] = [];
  let disposeCalls = 0;
  const launchRoot: LaunchAdviceRootSeam = async (spec): Promise<CodexAdviceLaunchResult> => {
    launchSpecs.push(spec);
    return {
      transport,
      cwd: opts.cwd ?? "/isolated/advice-home/work",
      dispose: async () => {
        disposeCalls += 1;
        if (opts.disposeThrows) throw new Error("dispose boom");
      },
    };
  };
  const harness = new CodexAdviceHarness({
    launchRoot,
    provider: opts.provider ?? provider,
    appServerAuth: opts.appServerAuth,
    credentialValue: opts.credentialValue,
    log: opts.log ?? noopLog,
  });
  return { harness, transport, launchSpecs, disposeCalls: () => disposeCalls };
}

// --- tests --------------------------------------------------------------------------

describe("CodexAdviceHarness: kind + a text advice pass", () => {
  it("advertises kind codex", () => {
    assert.equal(makeHarness().harness.kind, "codex");
  });

  it("authenticates before thread/model work and admits only the narrow refresh bridge", async () => {
    const transport = new FakeTransport(authResponder);
    const appServerAuth = createCodexAppServerAuth({
      mode: "subscription",
      initial: { accessToken: "initial-access", accountId: "server-account" },
      bridge: {
        refresh: async () => ({ accessToken: "next-access", accountId: "server-account" }),
      },
    });
    const bits = makeHarness({ transport, appServerAuth });
    transport
      .push(threadStarted())
      .push({
        kind: "activity",
        method: "account/chatgptAuthTokens/refresh",
        requestId: 44,
        params: { reason: "unauthorized", previousAccountId: null },
      })
      .push(agentMessage("ok"))
      .push(turnCompleted("completed"))
      .end();

    const result = await bits.harness.run(makeAdviceRequest(), noThrowPolicy);

    assert.equal(result.text, "ok");
    assert.deepEqual(transport.requests.map((request) => request.method), [
      "initialize",
      "account/login/start",
      "thread/start",
      "turn/start",
    ]);
    assert.deepEqual(transport.notifies, [{ method: "initialized", params: undefined }]);
    assert.deepEqual(transport.responses, [{
      requestId: 44,
      response: {
        result: { accessToken: "next-access", chatgptAccountId: "server-account", chatgptPlanType: null },
      },
    }]);
  });

  it("pumps auth while turn/start is pending instead of deadlocking advice setup", async () => {
    let resolveTurn!: (value: unknown) => void;
    let transport!: FakeTransport;
    transport = new FakeTransport((method, params) => {
      if (method === "turn/start") {
        const pending = new Promise<unknown>((resolve) => {
          resolveTurn = resolve;
        });
        transport.emitServerRequest({
          kind: "activity",
          method: "account/chatgptAuthTokens/refresh",
          requestId: 45,
          params: { reason: "unauthorized", previousAccountId: null },
        });
        return pending;
      }
      return authResponder(method, params);
    });
    transport.onRespond = (requestId) => {
      if (requestId === 45) resolveTurn({ turn: { id: "tn-1" } });
    };
    const appServerAuth = createCodexAppServerAuth({
      mode: "subscription",
      initial: { accessToken: "initial-access", accountId: "server-account" },
      bridge: {
        refresh: async () => ({ accessToken: "next-access", accountId: "server-account" }),
      },
    });
    const bits = makeHarness({ transport, appServerAuth });
    transport.push(threadStarted()).push(agentMessage("ok")).push(turnCompleted("completed")).end();

    const result = await bits.harness.run(makeAdviceRequest({ timeoutMs: 500 }), noThrowPolicy);

    assert.equal(result.text, "ok");
    assert.equal(transport.responses.some((response) => response.requestId === 45), true);
  });

  it("withholds a same-chunk advice terminal until intercepted auth settles", async () => {
    let releaseRefresh!: (value: { accessToken: string; accountId: string }) => void;
    const heldRefresh = new Promise<{ accessToken: string; accountId: string }>((resolve) => {
      releaseRefresh = resolve;
    });
    let transport!: FakeTransport;
    transport = new FakeTransport((method, params) => {
      if (method === "turn/start") {
        queueMicrotask(() => {
          transport.emitServerRequest({
            kind: "activity",
            method: "account/chatgptAuthTokens/refresh",
            requestId: 46,
            params: { reason: "unauthorized", previousAccountId: null },
          });
          transport.push(turnCompleted("completed"));
        });
      }
      return authResponder(method, params);
    });
    const appServerAuth = createCodexAppServerAuth({
      mode: "subscription",
      initial: { accessToken: "initial-access", accountId: "server-account" },
      bridge: { refresh: () => heldRefresh },
    });
    const bits = makeHarness({ transport, appServerAuth });
    transport.push(threadStarted()).push(agentMessage("ok"));

    let settled = false;
    const resultPromise = bits.harness.run(makeAdviceRequest({ timeoutMs: 500 }), noThrowPolicy).finally(() => {
      settled = true;
    });
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.equal(settled, false, "advice terminal remains withheld while refresh is held");

    releaseRefresh({ accessToken: "next-access", accountId: "server-account" });
    const result = await resultPromise;
    assert.equal(result.text, "ok");
    assert.equal(transport.responses.some((response) => response.requestId === 46), true);
  });

  it("rejects simultaneous env credential and app-server auth", () => {
    const appServerAuth = createCodexAppServerAuth({ mode: "api_key", apiKey: "test-key" });
    assert.throws(
      () => makeHarness({ appServerAuth, credentialValue: "other-key" }),
      /conflicting authentication inputs/,
    );
  });

  it("accumulates assistant text and returns the terminal with turn-basis usage", async () => {
    const bits = makeHarness();
    bits.transport
      .push(threadStarted())
      .push(turnStarted())
      .push(agentMessage("hello "))
      .push(agentMessage("world"))
      .push(turnCompleted("completed", { total_tokens: 12 }))
      .end();

    const result = await bits.harness.run(makeAdviceRequest(), noThrowPolicy);
    assert.equal(result.text, "hello world");
    assert.equal(result.end.kind, "terminal");
    if (result.end.kind === "terminal") assert.equal(result.end.terminal.outcome, "success");
    // Usage basis "turn" (Codex declares turn usage; harness-contract.md §Accounting).
    assert.deepEqual(result.usage, { basis: "turn", tokens: {}, wire: { usage: { total_tokens: 12 } } });
    assert.equal(bits.disposeCalls(), 1, "the isolated HOME is disposed after settlement");
  });

  it("turn/start carries the rendered prompt + model + effort", async () => {
    const bits = makeHarness();
    bits.transport.push(threadStarted()).push(turnCompleted("completed")).end();
    await bits.harness.run(makeAdviceRequest({ effort: "high", prompt: "please review" }), noThrowPolicy);

    const start = bits.transport.requests.find((r) => r.method === "turn/start");
    assert.ok(start);
    const params = start.params as Record<string, unknown>;
    assert.deepEqual(params.input, [{ type: "text", text: "please review" }]);
    assert.equal(params.model, "gpt-6-astra");
    assert.equal(params.modelReasoningEffort, "high");
  });
});

describe("CodexAdviceHarness: policy timing", () => {
  it("invokes policy.onTerminal SYNCHRONOUSLY before iterator closure and HOME cleanup", async () => {
    const bits = makeHarness();
    bits.transport
      .push(threadStarted())
      .push(agentMessage("hi"))
      .push(turnCompleted("completed", { total_tokens: 4 }))
      .push(agentMessage("MUST NOT BE READ")) // a trailing note must NOT be consumed
      .end();

    const order: string[] = [];
    const policy: AdviceResultPolicy = {
      onTerminal: (terminal, ctx) => {
        order.push("policy");
        // Ordering: the policy runs BEFORE the iterator/transport closes and BEFORE the
        // isolated HOME is disposed (mirror ClaudeAdviceHarness).
        assert.equal(bits.transport.closes, 0, "policy runs before iterator/transport closure");
        assert.equal(bits.disposeCalls(), 0, "policy runs before HOME cleanup");
        assert.equal(ctx.isError, false);
        assert.equal(terminal.outcome, "success");
      },
    };

    const result = await bits.harness.run(makeAdviceRequest(), policy);
    assert.deepEqual(order, ["policy"]);
    assert.equal(result.text, "hi", "the trailing post-terminal message was NOT consumed");
    assert.ok(bits.transport.closes >= 1, "the iterator/transport is closed after the policy");
    assert.equal(bits.disposeCalls(), 1, "the HOME is disposed after the policy");
  });

  it("a FAILED terminal → the policy sees isError; a non-throwing policy returns end:terminal(failed)", async () => {
    const bits = makeHarness();
    bits.transport.push(threadStarted()).push(turnCompleted("failed")).end();
    const { policy, seen } = recordingPolicy();

    const result = await bits.harness.run(makeAdviceRequest(), policy);
    assert.deepEqual(seen, [{ isError: true }]);
    assert.equal(result.end.kind, "terminal");
    if (result.end.kind === "terminal") {
      assert.equal(result.end.terminal.outcome, "failed");
      // The deferred failure materializes the generic terminal exception, no invented category.
      const thrown = result.end.terminal.failure!.materialize();
      assert.equal(thrown.failure.category, "unknown");
      assert.match(thrown.failure.message, /codex advice turn failed: failed/);
    }
  });

  it("a THROWING policy (the default's shape) propagates out of run(), and the HOME is still disposed", async () => {
    const bits = makeHarness();
    bits.transport.push(threadStarted()).push(turnCompleted("failed")).end();
    const policy: AdviceResultPolicy = {
      onTerminal: (_t, ctx) => {
        if (ctx.isError) throw new Error("review model call returned an error result");
      },
    };

    await assert.rejects(bits.harness.run(makeAdviceRequest(), policy), /review model call returned an error result/);
    assert.equal(bits.disposeCalls(), 1, "a policy throw does not skip HOME disposal");
  });
});

describe("CodexAdviceHarness: the tool-less advice ceiling (by construction)", () => {
  it("offers NO tool surface: a server→client request is refused fail-closed and runs nothing", async () => {
    const bits = makeHarness();
    bits.transport
      .push(threadStarted())
      .push(toolCall(7, "Bash", { command: "rm -rf /" }))
      .push(turnCompleted("completed"))
      .end();

    const result = await bits.harness.run(makeAdviceRequest(), noThrowPolicy);
    // There is no broker to route to: the request is answered with a JSON-RPC method error
    // and NO effect runs. That is the ceiling — the harness has no callback registry at all.
    assert.equal(bits.transport.responses.length, 1);
    const reply = bits.transport.responses[0]!;
    assert.equal(reply.requestId, 7);
    const response = reply.response as { error?: { code?: number } };
    assert.ok(response.error, "the tool call was refused, never executed");
    assert.equal(response.error.code, -32601);
    assert.equal(result.end.kind, "terminal");
  });

  it("thread/start pins untrusted project + doc_max 0, disables native tools/agents/MCP, and never a hook-trust bypass", async () => {
    const bits = makeHarness({ cwd: "/isolated/advice/proj" });
    bits.transport.push(threadStarted()).push(turnCompleted("completed")).end();
    await bits.harness.run(makeAdviceRequest(), noThrowPolicy);

    const start = bits.transport.requests.find((r) => r.method === "thread/start");
    assert.ok(start);
    const params = start.params as Record<string, unknown>;
    const config = params.config as Record<string, unknown>;
    assert.equal(config.project_doc_max_bytes, 0);
    assert.equal(config.web_search, "disabled");
    assert.deepEqual((config.projects as Record<string, unknown>)["/isolated/advice/proj"], {
      trust_level: "untrusted",
    });
    const features = config.features as Record<string, boolean>;
    assert.deepEqual(
      Object.keys(features).sort(),
      [
        "apps",
        "code_mode",
        "code_mode_host",
        "code_mode_only",
        "code_mode_prewarm",
        "enable_request_compression",
        "hooks",
        "multi_agent",
        "multi_agent_v2",
        "plugins",
        "remote_models",
        "shell_snapshot",
        "shell_snapshot_v2",
        "unified_exec",
      ],
      "the complete characterized feature ceiling stays explicit",
    );
    assert.deepEqual(
      Object.entries(features).filter(([, enabled]) => enabled !== false),
      [],
      "every declared advice feature is disabled",
    );
    assert.equal((config.agents as Record<string, unknown>).enabled, false);
    assert.equal(params.approvalPolicy, "never");
    // No hook-trust bypass anywhere in the start params.
    assert.ok(!JSON.stringify(params).includes("bypass_hook_trust"));
    assert.ok(!JSON.stringify(params).includes("dangerously-bypass-hook-trust"));
  });

  it("REJECTS a request carrying run-tool surface (agents) — the ceiling, enforced at runtime", async () => {
    const bits = makeHarness();
    const req = { ...makeAdviceRequest(), agents: {} } as unknown as AdviceRequest;
    await assert.rejects(bits.harness.run(req, noThrowPolicy), /advice ceiling/);
    assert.equal(bits.launchSpecs.length, 0, "nothing was launched for a ceiling-violating request");
  });

  it("construction does NOT accept a broker / agents / registry / run workspace (type-level)", () => {
    // A compile-time guardrail: if CodexAdviceHarnessOptions ever grew one of these keys,
    // the corresponding alias below becomes `never` and this tuple assignment fails tsc.
    type NoBroker = "broker" extends keyof CodexAdviceHarnessOptions ? never : true;
    type NoAgents = "agents" extends keyof CodexAdviceHarnessOptions ? never : true;
    type NoRegistry = "registry" extends keyof CodexAdviceHarnessOptions ? never : true;
    type NoWorkspace = "workspace" extends keyof CodexAdviceHarnessOptions ? never : true;
    type NoTools = "tools" extends keyof CodexAdviceHarnessOptions ? never : true;
    const checks: [NoBroker, NoAgents, NoRegistry, NoWorkspace, NoTools] = [true, true, true, true, true];
    assert.deepEqual(checks, [true, true, true, true, true]);
  });
});

describe("CodexAdviceHarness: credential isolation", () => {
  it("forwards the credential to the launch spec ONLY, never into a thread/turn param", async () => {
    const CRED = "sk-super-secret-advice-credential-value";
    const captured: Array<{ message: string; fields?: Record<string, unknown> }> = [];
    const record = (message: string, fields?: Record<string, unknown>): void => {
      captured.push({ message, fields });
    };
    const log: Logger = {
      debug: record,
      info: record,
      warn: record,
      error: record,
      addSecret() {},
      removeSecret() {},
      child() {
        return this;
      },
    };
    const bits = makeHarness({ credentialValue: CRED, log });
    bits.transport.push(threadStarted()).push(turnCompleted("completed")).end();
    await bits.harness.run(makeAdviceRequest(), noThrowPolicy);

    // The narrow provider-auth bridge reaches the launcher env (via the spec) ...
    assert.equal(bits.launchSpecs.length, 1);
    assert.equal(bits.launchSpecs[0]!.credentialValue, CRED);
    // ... but NO thread/start or turn/start param carries it (never model-visible).
    for (const r of bits.transport.requests) {
      assert.ok(!JSON.stringify(r.params).includes(CRED), `${r.method} params must not carry the credential`);
    }
    assert.ok(
      !JSON.stringify(captured).includes(CRED),
      "no captured log message or metadata carries the credential",
    );
  });
});

describe("CodexAdviceHarness: timeout + grace + fail-closed setup", () => {
  it("timeout breaks a pending-login plus intercepted-refresh ownership cycle", async () => {
    let rejectLogin!: (error: Error) => void;
    let markRefreshEmitted!: () => void;
    const refreshEmitted = new Promise<void>((resolve) => {
      markRefreshEmitted = resolve;
    });
    let bridgeCalls = 0;
    let transport!: FakeTransport;
    transport = new FakeTransport((method, params) => {
      if (method === "account/login/start") {
        queueMicrotask(() => {
          transport.emitServerRequest({
            kind: "activity",
            method: "account/chatgptAuthTokens/refresh",
            requestId: 47,
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
    const bits = makeHarness({ transport, appServerAuth });
    const run = bits.harness.run(makeAdviceRequest({ timeoutMs: 50, graceMs: 100 }), noThrowPolicy);
    await refreshEmitted;

    await assert.rejects(run, /review model call exceeded 50ms/);
    await appServerAuth.drainInterceptedRequests();

    assert.equal(bridgeCalls, 0);
    assert.ok(transport.closes >= 1);
    assert.equal(bits.disposeCalls(), 1);
  });

  it("timeout ABORTS + rejects with the exact label/timeout message, returns NO partial text, and disposes HOME", async () => {
    const bits = makeHarness();
    // thread/start + turn/start answer, then a partial message, then the stream blocks with
    // NO terminal → the wall-clock timeout fires.
    bits.transport.push(threadStarted()).push(agentMessage("partial answer")); // no terminal, no end()

    const req = makeAdviceRequest({ label: "judge", timeoutMs: 25, graceMs: 10 });
    await assert.rejects(bits.harness.run(req, noThrowPolicy), (err: unknown) => {
      assert.ok(err instanceof Error);
      // The EXACT label/timeout message (mirror the Claude advice format).
      assert.equal((err as Error).message, "judge model call exceeded 25ms");
      return true;
    });
    assert.equal(bits.disposeCalls(), 1, "the isolated HOME is disposed after settlement/grace");
  });

  it("disposes the advice root when authentication draining fails", async () => {
    const appServerAuth: CodexAppServerAuthSession = {
      mode: "subscription",
      authenticate: async () => {},
      handleServerRequest: async () => false,
      drainInterceptedRequests: async () => { throw new Error("auth drain failed"); },
      closeAdmissionAndCancel() {},
    };
    const bits = makeHarness({ appServerAuth });
    bits.transport.push(threadStarted()).push(turnCompleted("completed")).end();

    await assert.rejects(bits.harness.run(makeAdviceRequest(), noThrowPolicy), /auth drain failed/);
    assert.equal(bits.disposeCalls(), 1, "auth drain failure cannot skip root cleanup");
  });

  it("does not let a cleanup-only auth drain failure replace a successful advice result", async () => {
    let drains = 0;
    const appServerAuth: CodexAppServerAuthSession = {
      mode: "subscription",
      authenticate: async () => {},
      handleServerRequest: async () => false,
      drainInterceptedRequests: async () => {
        drains += 1;
        if (drains > 1) throw new Error("cleanup drain failed");
      },
      closeAdmissionAndCancel() {},
    };
    const bits = makeHarness({ appServerAuth });
    bits.transport.push(threadStarted()).push(agentMessage("kept result")).push(turnCompleted("completed")).end();

    const result = await bits.harness.run(makeAdviceRequest(), noThrowPolicy);

    assert.equal(result.text, "kept result");
    assert.equal(drains, 2, "the terminal drain succeeded before the cleanup-only drain failed");
    assert.equal(bits.disposeCalls(), 1);
  });

  it("does not let an auth cleanup failure replace the exact timeout rejection", async () => {
    const appServerAuth: CodexAppServerAuthSession = {
      mode: "subscription",
      authenticate: async () => {},
      handleServerRequest: async () => false,
      drainInterceptedRequests: async () => { throw new Error("cleanup drain failed"); },
      closeAdmissionAndCancel() {},
    };
    const bits = makeHarness({ appServerAuth });
    bits.transport.push(threadStarted()).push(agentMessage("partial"));

    await assert.rejects(
      bits.harness.run(makeAdviceRequest({ label: "review", timeoutMs: 25, graceMs: 10 }), noThrowPolicy),
      /review model call exceeded 25ms/,
    );
    assert.equal(bits.disposeCalls(), 1);
  });

  it("an unexpected EOF (no terminal) rejects with a protocol error and still disposes the HOME", async () => {
    const bits = makeHarness();
    bits.transport.push(threadStarted()).push(turnStarted()).end(); // ends before turn/completed

    await assert.rejects(bits.harness.run(makeAdviceRequest(), noThrowPolicy), (err: unknown) => {
      assert.ok(err instanceof CodexAdviceError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /ended before turn completion/);
      return true;
    });
    assert.equal(bits.disposeCalls(), 1);
  });

  it("a thread/start with no id rejects (protocol) and disposes the HOME", async () => {
    const transport = new FakeTransport((method) => (method === "thread/start" ? {} : defaultResponder(method)));
    const bits = makeHarness({ transport });

    await assert.rejects(bits.harness.run(makeAdviceRequest(), noThrowPolicy), (err: unknown) => {
      assert.ok(err instanceof CodexAdviceError);
      assert.match(err.message, /thread\/start returned no thread id/);
      return true;
    });
    assert.equal(bits.disposeCalls(), 1);
  });

  it("a launch failure rejects and disposes NOTHING (the launcher owns its partial cleanup)", async () => {
    const boom = new Error("launch failed");
    let launchCalls = 0;
    let disposeAttempts = 0;
    const launchRoot: LaunchAdviceRootSeam = async () => {
      launchCalls += 1;
      throw boom;
    };
    const harness = new CodexAdviceHarness({ launchRoot, provider, log: noopLog });
    await assert.rejects(harness.run(makeAdviceRequest(), noThrowPolicy), (e: unknown) => e === boom);
    assert.equal(launchCalls, 1, "the failing launch seam was exercised");
    assert.equal(disposeAttempts, 0, "no disposer exists when launch never returns a root");
  });

  it("a HOME cleanup failure NEVER replaces the primary result (a dispose throw is swallowed + warned)", async () => {
    const warns: string[] = [];
    const log: Logger = { ...noopLog, warn: (m: string) => warns.push(m) };
    const bits = makeHarness({ disposeThrows: true, log });
    bits.transport.push(threadStarted()).push(agentMessage("ok")).push(turnCompleted("completed")).end();

    const result = await bits.harness.run(makeAdviceRequest(), noThrowPolicy);
    assert.equal(result.text, "ok", "the primary result stands despite the cleanup failure");
    assert.equal(result.end.kind, "terminal");
    assert.ok(warns.some((w) => /cleanup failed/.test(w)), "the cleanup failure is warned, not thrown");
  });
});

describe("CodexAdviceHarness: accumulated advice text is bounded (fail-closed)", () => {
  it("many large chunks with NO terminal REJECT with the bounded overflow error PROMPTLY, and the HOME is still disposed", async () => {
    const bits = makeHarness();
    // Feed near-cap assistant chunks with NO terminal and NO end(). The transport's
    // per-frame (4 MiB) and inbound-queue (64 MiB) ceilings bound only momentary buffering:
    // because consume() drains notes one at a time, the running `text` would otherwise grow
    // UNBOUNDED (100+ MiB) and OOM the worker. The byte cap must FAIL CLOSED long before the
    // wall-clock timeout. One 2 MiB string is shared across the notes (references, not
    // copies), so the test's own footprint stays small.
    const chunk = "x".repeat(2 * 1024 * 1024); // 2 MiB per frame
    bits.transport.push(threadStarted()).push(turnStarted());
    for (let i = 0; i < 6; i += 1) bits.transport.push(agentMessage(chunk)); // > 8 MiB total, no terminal

    // A wall-clock timeout well above the (near-instant) in-loop cap trip: if the cap
    // regressed, the harness would instead block on the drained queue until this timeout —
    // which the type/message + promptness assertions below both catch.
    const req = makeAdviceRequest({ label: "judge", timeoutMs: 5000, graceMs: 10 });
    const start = Date.now();
    await assert.rejects(bits.harness.run(req, noThrowPolicy), (err: unknown) => {
      assert.ok(err instanceof CodexAdviceError);
      // Mirrors the transport's own {category:"transport"} inbound overflow discipline.
      assert.equal(err.failure.category, "transport");
      assert.match(err.message, /exceeded the maximum size/);
      return true;
    });
    // PROMPTLY: the cap fired IN-LOOP, not at the 5000ms wall-clock timeout — proof it does
    // not accumulate toward OOM and does not wait out the timeout window.
    assert.ok(Date.now() - start < 2000, "the overflow rejects promptly, not at the wall-clock timeout");
    assert.equal(bits.disposeCalls(), 1, "the isolated HOME is disposed after the overflow");
  });

  it("accumulated text just UNDER the cap still returns normally (the cap does not fire on a genuine response)", async () => {
    const bits = makeHarness();
    // A single ~1 MiB assistant message is well under the 8 MiB ceiling: a normal advice
    // response must accumulate and return, proving the bound is a ceiling, not a throttle.
    const body = "y".repeat(1024 * 1024);
    bits.transport
      .push(threadStarted())
      .push(turnStarted())
      .push(agentMessage(body))
      .push(turnCompleted("completed", { total_tokens: 3 }))
      .end();

    const result = await bits.harness.run(makeAdviceRequest(), noThrowPolicy);
    assert.equal(result.text, body);
    assert.equal(result.end.kind, "terminal");
    assert.equal(bits.disposeCalls(), 1);
  });
});

describe("CodexAdviceHarness: output schema is fail-closed", () => {
  it("a {kind:'json'} output is REFUSED fail-closed (no silent unenforced text, nothing launched)", async () => {
    let launched = 0;
    const launchRoot: LaunchAdviceRootSeam = async () => {
      launched += 1;
      return { transport: new FakeTransport(), cwd: "/x", dispose: async () => {} };
    };
    const harness = new CodexAdviceHarness({ launchRoot, provider, log: noopLog });
    const req = makeAdviceRequest({ output: { kind: "json", schema: { type: "object" } } });

    await assert.rejects(harness.run(req, noThrowPolicy), (err: unknown) => {
      assert.ok(err instanceof CodexAdviceError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /native outputSchema enforcement is unmeasured/);
      return true;
    });
    assert.equal(launched, 0, "a refused json request launches nothing");
  });
});

describe("CodexAdviceHarness: terminal fields are normalized (no raw provider leakage)", () => {
  it("routes decodeTerminal through the shared normalizers: no secret error/usage, bounded numeric usage, closed subtype", async () => {
    // Assemble the secret at RUNTIME so no literal appears in source (the scanner sees none).
    const secret = ["sk", "live", "DEADBEEF", "advice", "TOKEN"].join("_");
    // A raw provider status OUTSIDE the closed allowlist → must collapse to the "unknown" token.
    const rawStatus = ["provider", "weird", "status"].join("-");
    // A big non-numeric nested blob to prove the WHOLE raw usage object is never retained.
    const blob = "z".repeat(256 * 1024);
    const usage = {
      total_tokens: 42, // numeric ≥ 0 → kept
      input_tokens: 7, // numeric ≥ 0 → kept
      api_key: secret, // secret-shaped STRING field → dropped
      nested: { blob }, // huge nested OBJECT → dropped
    };
    const terminalNote: CodexNotification = {
      kind: "turn_completed",
      method: "turn/completed",
      threadId: "th-1",
      turnId: "tn-1",
      status: rawStatus,
      params: { threadId: "th-1", turn: { id: "tn-1", status: rawStatus, error: secret, usage } },
    };

    const bits = makeHarness();
    bits.transport
      .push(threadStarted())
      .push(turnStarted())
      .push(agentMessage("verdict"))
      .push(terminalNote)
      .end();

    const result = await bits.harness.run(makeAdviceRequest(), noThrowPolicy);

    assert.equal(result.end.kind, "terminal");
    if (result.end.kind === "terminal") {
      const t = result.end.terminal;
      // subtype is a CLOSED token for an unknown raw status (never the raw provider string).
      assert.equal(t.subtype, "unknown");
      assert.equal(t.outcome, "failed");
      // errors are provider-text-free: the raw, secret-bearing turn.error is NEVER echoed.
      assert.deepEqual(t.errors, ["codex turn ended with status: unknown"]);
      // the materialized failure message is based on the CLOSED subtype, not the raw status.
      const thrown = t.failure!.materialize();
      assert.match(thrown.failure.message, /codex advice turn failed: unknown/);
      assert.ok(!thrown.failure.message.includes(rawStatus), "the raw status never rides the failure message");
    }
    // usage.wire.usage is a bounded numeric-only subset — the string + nested blob are dropped.
    assert.deepEqual(result.usage, {
      basis: "turn",
      tokens: {},
      wire: { usage: { total_tokens: 42, input_tokens: 7 } },
    });

    // Nothing secret-bearing or oversize survives ANYWHERE in the returned result.
    const serialized = JSON.stringify(result);
    assert.ok(!serialized.includes(secret), "the secret-bearing provider error/field never leaks out");
    assert.ok(!serialized.includes("zzzz"), "the huge nested usage blob is never retained");
    assert.ok(!serialized.includes(rawStatus), "the raw provider status is never echoed");
  });
});

describe("CodexAdviceHarness: launch is deadline-bound and disposed exactly once (finding 4)", () => {
  it("the injected launchRoot receives a signal that becomes aborted on the wall-clock timeout", async () => {
    const bits = makeHarness();
    // thread/start answers, a partial message, then the stream blocks with NO terminal and NO
    // end() → the wall-clock timeout fires and must abort the launch signal too.
    bits.transport.push(threadStarted()).push(agentMessage("partial"));

    const req = makeAdviceRequest({ label: "judge", timeoutMs: 25, graceMs: 10 });
    await assert.rejects(bits.harness.run(req, noThrowPolicy), /judge model call exceeded 25ms/);

    assert.equal(bits.launchSpecs.length, 1);
    const spec = bits.launchSpecs[0]!;
    assert.ok(spec.signal, "the launch spec carries an abort signal");
    assert.equal(spec.signal!.aborted, true, "the wall-clock timeout aborts the launch signal");
  });

  it("late-dispose: a launch that finishes AFTER the settlement grace is still disposed EXACTLY once", async () => {
    let disposeCalls = 0;
    // A transport that NEVER completes (no terminal, no end): once the slow launch resolves
    // past the timeout/grace, consume finds the signal already aborted and unwinds.
    const transport = new FakeTransport();
    const timeoutMs = 25;
    const graceMs = 10;
    const launchDelayMs = 120; // resolves well AFTER timeoutMs + graceMs

    const launchRoot: LaunchAdviceRootSeam = (_spec) =>
      new Promise<CodexAdviceLaunchResult>((resolve) => {
        const t = setTimeout(() => {
          resolve({
            transport,
            cwd: "/isolated/advice/late",
            dispose: async () => {
              disposeCalls += 1;
            },
          });
        }, launchDelayMs);
        t.unref?.();
      });
    const harness = new CodexAdviceHarness({ launchRoot, provider, log: noopLog });
    const req = makeAdviceRequest({ label: "judge", timeoutMs, graceMs });

    await assert.rejects(harness.run(req, noThrowPolicy), (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.equal((err as Error).message, `judge model call exceeded ${timeoutMs}ms`);
      return true;
    });
    // The finally already ran while the launch was still pending (disposeSeam undefined), so
    // nothing has been disposed yet — the old code would leak the root here forever.
    assert.equal(disposeCalls, 0, "nothing is disposed while the slow launch is still pending");

    // Wait past the launch delay for the late disposer (work.then → disposeOnce) to run.
    await new Promise((r) => setTimeout(r, launchDelayMs + 80));
    assert.equal(disposeCalls, 1, "the late-resolving launch root is disposed EXACTLY once");
  });
});
