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
    assert.equal(features.code_mode, false);
    assert.equal(features.unified_exec, false);
    assert.equal(features.hooks, false);
    assert.equal(features.multi_agent, false);
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
    const bits = makeHarness({ credentialValue: CRED });
    bits.transport.push(threadStarted()).push(turnCompleted("completed")).end();
    await bits.harness.run(makeAdviceRequest(), noThrowPolicy);

    // The narrow provider-auth bridge reaches the launcher env (via the spec) ...
    assert.equal(bits.launchSpecs.length, 1);
    assert.equal(bits.launchSpecs[0]!.credentialValue, CRED);
    // ... but NO thread/start or turn/start param carries it (never model-visible).
    for (const r of bits.transport.requests) {
      assert.ok(!JSON.stringify(r.params).includes(CRED), `${r.method} params must not carry the credential`);
    }
  });
});

describe("CodexAdviceHarness: timeout + grace + fail-closed setup", () => {
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
    const launchRoot: LaunchAdviceRootSeam = async () => {
      throw boom;
    };
    const harness = new CodexAdviceHarness({ launchRoot, provider, log: noopLog });
    await assert.rejects(harness.run(makeAdviceRequest(), noThrowPolicy), (e: unknown) => e === boom);
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
