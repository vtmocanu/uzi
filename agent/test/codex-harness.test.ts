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
import type { HarnessEvent, RunTurnRequest } from "../src/harness.js";
import type { CodexNotification, CodexTransport } from "../src/codex/transport.js";
import type { Logger } from "../src/log.js";

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

function delta(): CodexNotification {
  return { kind: "activity", method: "item/agent_message_delta", params: { delta: "h" } };
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
    credentialValue?: string;
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
    credentialValue: opts.credentialValue,
  });
  return { harness, transport, registry };
}

async function collect(events: AsyncIterable<HarnessEvent>): Promise<HarnessEvent[]> {
  const out: HarnessEvent[] = [];
  for await (const ev of events) out.push(ev);
  return out;
}

// --- tests --------------------------------------------------------------------------

describe("CodexHarness: kind + thread configuration", () => {
  it("advertises kind codex", () => {
    const { harness } = makeHarness();
    assert.equal(harness.kind, "codex");
  });

  it("thread/start carries an EXPLICIT untrusted project + doc_max_bytes 0 and NEVER a hook-trust bypass", async () => {
    const { harness, transport } = makeHarness();
    transport.push(threadStarted()).end();
    const turn = harness.startTurn(makeRequest());
    const it = turn.events[Symbol.asyncIterator]();
    await it.next(); // drives setup (thread/start + turn/start) then yields `initialized`

    const start = transport.requests.find((r) => r.method === "thread/start");
    assert.ok(start, "thread/start was sent");
    const params = rec(start.params);
    const config = rec(params.config);
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
    const config = rec(params.config);
    assert.equal(config.project_doc_max_bytes, 0);
    assert.deepEqual(rec(config.projects)[WORKSPACE], { trust_level: "untrusted" });
    assert.doesNotMatch(JSON.stringify(params), /bypass_hook_trust/);
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

  it("routes with the RUNTIME thread identity, not a spoofed argument (origin=root only for the real root thread)", async () => {
    const seen: { rt: CallbackRuntimeId; origin: CallbackOrigin }[] = [];
    const broker = stubBroker(async (rt, _name, _args, origin) => {
      seen.push({ rt, origin });
      return { ok: true, output: {} };
    });
    const { harness, transport } = makeHarness({ broker });
    // A tool-call whose params SPOOF a different threadId is treated as non-root (fail-closed).
    transport
      .push(threadStarted())
      .push(toolCall(1, "Bash", { command: "echo" }, "spoofed-thread"))
      .push(turnCompleted("completed"))
      .end();

    await collect(harness.startTurn(makeRequest()).events);
    assert.equal(seen.length, 1);
    assert.equal(seen[0]!.rt.threadId, "spoofed-thread");
    assert.equal(seen[0]!.origin, "unknown");
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

  it("close() is idempotent and does NOT reap / dispose / quiesce the registry", async () => {
    const { harness, transport, registry } = makeHarness();
    transport.push(threadStarted()).push(turnCompleted("completed")).end();
    const turn = harness.startTurn(makeRequest());
    await collect(turn.events); // run a turn so the root/transport exist

    await turn.close();
    await turn.close(); // idempotent — no throw
    assert.equal(transport.closes, 2, "close closes the transport idempotently");
    // The registry was never quiesced/reaped/disposed by close(): it stays open with the
    // one registered provider root.
    assert.equal(registry.state(), "open");
    assert.equal(registry.rootCount(), 1);
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
