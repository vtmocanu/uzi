import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { ClaudeHarness, type ClaudeTurnConfig } from "../src/claude-harness.js";
import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import { projectInit, projectResult } from "../src/harness-messages.js";
import type { HarnessEvent, RunTurnRequest } from "../src/harness.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import {
  UsageRecorder,
  resetUsageRouteStateForTests,
  type UsageClient,
  type UsageWireRequest,
} from "../src/usage-recorder.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

// Issue #2014 (ADR-2014 D1/D2): the run lane's usage capture. Driven through the real ClaudeHarness
// decode, the reducer and the SdkExecutor emit, with raw SDK frames (the repo holds no recorded SDK
// transcript, so the equivalence input below is BUILT from the frame shapes sdk-executor.test.ts
// uses, plus the documented stream_event sequence).

const nonexistentWorktree = nonexistentWorktreeFactory("uzi-usage-capture");

class FakeUsageClient implements UsageClient {
  readonly calls: UsageWireRequest[] = [];
  async postUsage(_runId: string, body: UsageWireRequest): Promise<void> {
    this.calls.push(structuredClone(body));
  }
  get records() {
    return this.calls.flatMap((c) => c.messages);
  }
  get legs() {
    return this.calls.flatMap((c) => c.legs);
  }
}

function newRecorder(client: FakeUsageClient): UsageRecorder {
  return new UsageRecorder({ client, runId: "r1", claimGeneration: 0, log: nullLogger(), debounceMs: 60_000, backoffBaseMs: 1 });
}

const MODEL = "claude-sonnet-5-5";
const asMsg = (m: Record<string, unknown>): SDKMessage => m as unknown as SDKMessage;

function init(sessionId = "sess-1"): SDKMessage {
  return asMsg({ type: "system", subtype: "init", session_id: sessionId, model: MODEL });
}
function usageOf(n: number): Record<string, unknown> {
  return { input_tokens: n, output_tokens: 1, cache_read_input_tokens: 5, cache_creation_input_tokens: 0 };
}
function asst(id: string, content: unknown[], extra: Record<string, unknown> = {}, n = 10, sessionId = "sess-1"): SDKMessage {
  return asMsg({ type: "assistant", session_id: sessionId, message: { id, model: MODEL, content, usage: usageOf(n) }, ...extra });
}
function text(t: string): Record<string, unknown> {
  return { type: "text", text: t };
}
function thinking(t: string): Record<string, unknown> {
  return { type: "thinking", thinking: t };
}
function toolUse(id: string, name: string, input: Record<string, unknown> = {}): Record<string, unknown> {
  return { type: "tool_use", id, name, input };
}
function toolResultFrame(toolUseId: string, extra: Record<string, unknown> = {}): SDKMessage {
  return asMsg({ type: "user", session_id: "sess-1", message: { content: [{ type: "tool_result", tool_use_id: toolUseId, content: "ok" }] }, ...extra });
}
function result(opts: { subtype?: string; isError?: boolean; sessionId?: string } = {}): SDKMessage {
  return asMsg({
    type: "result",
    subtype: opts.subtype ?? "success",
    is_error: opts.isError ?? false,
    num_turns: 3,
    duration_ms: 50,
    total_cost_usd: 0.01,
    session_id: opts.sessionId ?? "sess-1",
    usage: usageOf(100),
    modelUsage: { [MODEL]: { inputTokens: 100, outputTokens: 5, cacheReadInputTokens: 50, cacheCreationInputTokens: 0, costUSD: 0.01 } },
    ...(opts.isError ? { errors: ["boom"] } : {}),
  });
}
const SUBAGENT = { parent_tool_use_id: "toolu_T", subagent_type: "coder" };

// --- the documented stream_event sequence, wrapped around an assistant frame ----------------
function ev(event: Record<string, unknown>, parent: string | null = null): SDKMessage {
  return asMsg({ type: "stream_event", session_id: "sess-1", parent_tool_use_id: parent, uuid: "u", event });
}
function partialsBefore(id: string, n: number, parent: string | null = null): SDKMessage[] {
  return [
    ev({ type: "message_start", message: { id, model: MODEL, usage: { ...usageOf(n), output_tokens: 1 } } }, parent),
    ev({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } }, parent),
    ev({ type: "ping" }, parent),
    ev({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "x" } }, parent),
    ev({ type: "content_block_stop", index: 0 }, parent),
  ];
}
function partialsAfter(parent: string | null = null, out = 77): SDKMessage[] {
  return [
    ev({ type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: out } }, parent),
    ev({ type: "ping" }, parent),
    ev({ type: "message_stop" }, parent),
  ];
}

// --- harness-level plumbing ----------------------------------------------------------------
const config: ClaudeTurnConfig = {
  cwd: "/tmp/uzi-usage-capture", env: {}, skillsPluginPath: "/tmp/uzi-usage-capture-plugin",
  skills: [], systemPrompt: "sys", agents: {}, mcpServers: {}, preToolUse: [],
};
const request: RunTurnRequest = {
  prompt: "p", systemPrompt: "sys", signal: new AbortController().signal,
  phase: "implement", agents: {}, leadSkills: [],
};

function harnessOver(frames: SDKMessage[] | (() => AsyncIterable<unknown>), recorder: UsageRecorder | undefined): ClaudeHarness {
  const queryFn = (() =>
    typeof frames === "function" ? frames() : (async function* () { for (const f of frames) yield f; })()) as unknown as SdkQueryFn;
  const harness = new ClaudeHarness({
    queryFn, spawn: () => ({ pid: undefined }), kill: () => true, log: nullLogger(),
    contextUsageTimeoutMs: 2000, spawnedPids: new Set<number>(),
    homeDir: path.join(os.tmpdir(), "uzi-usage-capture-home-does-not-exist"),
  });
  harness.setUsageSink(recorder);
  harness.prepareTurn(config, { pid: undefined });
  return harness;
}

async function collect(harness: ClaudeHarness): Promise<{ events: HarnessEvent[]; messages: EmittedMessage[]; firstSessionIds: string[] }> {
  const reducer = new RunTurnReducerImpl({ request: () => {}, get: async () => undefined });
  reducer.beginTurn();
  const events: HarnessEvent[] = [];
  const messages: EmittedMessage[] = [];
  const firstSessionIds: string[] = [];
  for await (const event of harness.startTurn(request).events) {
    events.push(event);
    const reduction = await reducer.accept(event);
    messages.push(...reduction.messages);
    if (reduction.firstSessionId !== undefined) firstSessionIds.push(reduction.firstSessionId);
  }
  return { events, messages, firstSessionIds };
}

beforeEach(() => resetUsageRouteStateForTests());
afterEach(() => resetUsageRouteStateForTests());

describe("ClaudeHarness usage capture", () => {
  it("text-only, thinking-only, signal-only and subagent text-only frames each produce a record", async () => {
    const client = new FakeUsageClient();
    const recorder = newRecorder(client);
    const frames = [
      init(),
      asst("m-text", [text("only text")]),
      asst("m-think", [thinking("only thinking")]),
      // every item of this frame is filtered out of the projection (a signal tool_use)
      asst("m-signal", [toolUse("t1", "mcp__uzi__submit_plan", { plan_md: "p" })]),
      // a text-only message by a subagent, seen only because forwardSubagentText forwards it
      asst("m-sub-text", [text("sub says")], SUBAGENT),
      asst("m-sub-think", [thinking("sub thinks")], SUBAGENT),
      result(),
    ];
    const { messages } = await collect(harnessOver(frames, recorder));
    await recorder.drain();

    assert.deepEqual(
      client.records.map((r) => [r.message_id, r.ordinal, r.subagent]),
      [
        ["m-text", 1, false],
        ["m-think", 2, false],
        ["m-signal", 3, false],
        ["m-sub-text", 4, true],
        ["m-sub-think", 5, true],
      ],
    );
    // the projection never shows the subagent's text/thinking, and the signal tool_use stays filtered
    assert.deepEqual(messages.filter((m) => m.kind === "text" || m.kind === "thinking").map((m) => [m.kind, m.payload["text"]]), [
      ["text", "only text"],
      ["thinking", "only thinking"],
    ]);
    assert.equal(messages.some((m) => m.kind === "tool_use"), false);
  });

  it("without a usage sink a subagent text frame is projected exactly as before (nothing recorded, no flag)", async () => {
    const { messages } = await collect(harnessOver([init(), asst("m-sub-text", [text("sub says")], SUBAGENT), result()], undefined));
    assert.deepEqual(messages.filter((m) => m.kind === "text").map((m) => m.payload["text"]), ["sub says"]);
  });

  it("duplicate frames sharing one message.id are one record; message_delta sets output_final and the final output", async () => {
    const client = new FakeUsageClient();
    const recorder = newRecorder(client);
    const frames = [
      init(),
      ...partialsBefore("m1", 10),
      asst("m1", [thinking("t")], {}, 10),
      asst("m1", [text("a")], {}, 10),
      asst("m1", [toolUse("t9", "Bash")], {}, 10),
      ...partialsAfter(null, 321),
      ...partialsBefore("m2", 11),
      asst("m2", [text("b")], {}, 11),
      result(),
    ];
    await collect(harnessOver(frames, recorder));
    await recorder.drain();
    assert.deepEqual(
      client.records.map((r) => [r.message_id, r.ordinal, r.output_tokens, r.output_final]),
      [
        ["m1", 1, 321, true],
        ["m2", 2, 1, false],
      ],
    );
  });

  it("a message seen only through stream events is recorded; a subagent lane's stream events are attributed to it", async () => {
    const client = new FakeUsageClient();
    const recorder = newRecorder(client);
    const frames = [init(), ...partialsBefore("ghost", 3), ...partialsAfter(null, 9), ...partialsBefore("sub-ghost", 4, "toolu_T"), ...partialsAfter("toolu_T", 12), result()];
    await collect(harnessOver(frames, recorder));
    await recorder.drain();
    assert.deepEqual(
      client.records.map((r) => [r.message_id, r.subagent, r.output_tokens, r.output_final]),
      [
        ["ghost", false, 9, true],
        ["sub-ghost", true, 12, true],
      ],
    );
  });

  it("stream_events yield NO harness event: no liveness, no session latch, no orphan diagnostic", async () => {
    const client = new FakeUsageClient();
    const frames = [
      ...partialsBefore("m1", 1),
      // a stream_event carrying a session id and a parent_tool_use_id (and no subagent_type)
      asMsg({ type: "stream_event", session_id: "only-on-a-partial", parent_tool_use_id: "toolu_X", event: { type: "ping" } }),
      ...partialsAfter(),
    ];
    const { events, firstSessionIds } = await collect(harnessOver(frames, newRecorder(client)));
    assert.deepEqual(events, [], "a stream-event-only turn yields nothing to the executor");
    assert.deepEqual(firstSessionIds, [], "a session id seen only on a partial never latches");
  });

  it("the close marker is enqueued when the events iterator ends, including by a throw", async () => {
    const client = new FakeUsageClient();
    const recorder = newRecorder(client);
    const harness = harnessOver(
      () =>
        (async function* () {
          yield asst("m1", [text("a")]);
          yield asst("m2", [text("b")]);
          throw new Error("transport died");
        })(),
      recorder,
    );
    await assert.rejects(collect(harness), /transport died/);
    await recorder.drain();
    assert.equal(client.records.length, 2);
    assert.equal(client.legs.length, 1);
    assert.equal(client.legs[0]!.closed_through, 2);
    assert.equal(client.legs[0]!.leg_id, client.records[0]!.leg_id);
  });

  it("each query() is its own leg with its own dense ordinals", async () => {
    const client = new FakeUsageClient();
    const recorder = newRecorder(client);
    const harness = harnessOver([init(), asst("a1", [text("1")]), asst("a2", [text("2")]), result()], recorder);
    await collect(harness);
    harness.prepareTurn(config, { pid: undefined });
    await collect(harness);
    await recorder.drain();
    const legIds = [...new Set(client.records.map((r) => r.leg_id))];
    assert.equal(legIds.length, 2);
    assert.deepEqual(client.records.map((r) => r.ordinal), [1, 2, 1, 2]);
  });
});

describe("stamps on the persisted init and result frames (ADR-2014 D4)", () => {
  it("init carries leg_id and sdk_session_id; both result sites carry leg_id and usage_through", async () => {
    for (const failed of [false, true]) {
      const recorder = newRecorder(new FakeUsageClient());
      const frames = [
        init("sdk-sess-7"),
        asst("m1", [text("a")], {}, 10, "sdk-sess-7"),
        asst("m2", [text("b")], {}, 10, "sdk-sess-7"),
        failed ? result({ subtype: "error_max_turns", isError: true, sessionId: "sdk-sess-7" }) : result({ sessionId: "sdk-sess-7" }),
      ];
      const { messages, events } = await collect(harnessOver(frames, recorder));
      const initMsg = messages.find((m) => m.kind === "status" && m.payload["event"] === "init")!;
      const resultMsg = messages.find((m) => m.payload["event"] === "result")!;
      const finished = events.find((e) => e.kind === "turn_finished");
      const legId = (events.find((e) => e.kind === "initialized") as { legId?: string }).legId;
      assert.ok(legId && /^[0-9a-f-]{36}$/.test(legId), "a UUID leg id");
      assert.equal(initMsg.payload["leg_id"], legId);
      assert.equal(initMsg.payload["sdk_session_id"], "sdk-sess-7");
      assert.equal(resultMsg.kind, failed ? "error" : "status", "the stamp rides BOTH result emit sites");
      assert.equal(resultMsg.payload["leg_id"], legId);
      assert.equal(resultMsg.payload["usage_through"], 2);
      assert.equal(resultMsg.payload["usage_basis"], "session_cumulative");
      assert.ok(finished);
    }
  });

  it("usage_through is the leg's highest ordinal when the result was decoded (0 with no messages)", async () => {
    const { messages } = await collect(harnessOver([init(), result()], newRecorder(new FakeUsageClient())));
    assert.equal(messages.find((m) => m.payload["event"] === "result")!.payload["usage_through"], 0);
  });

  it("no sink, no stamps: init and result are byte-identical to the pre-#2014 payloads", async () => {
    const { messages } = await collect(harnessOver([init("s"), asst("m1", [text("a")]), result()], undefined));
    const initMsg = messages.find((m) => m.payload["event"] === "init")!;
    const resultMsg = messages.find((m) => m.payload["event"] === "result")!;
    assert.deepEqual(Object.keys(initMsg.payload), ["event", "model", "fresh_session"]);
    for (const key of ["leg_id", "sdk_session_id", "usage_through"]) {
      assert.equal(key in initMsg.payload, false);
      assert.equal(key in resultMsg.payload, false);
    }
  });

  it("projectInit and projectResult leave other harnesses' payloads untouched when no stamp is given", () => {
    assert.deepEqual(projectInit("m").payload, { event: "init", model: "m" });
    assert.deepEqual(projectInit("m", true, 1).payload, { event: "init", model: "m", fresh_session: true, plugin_error_count: 1 });
    const wire = { usage: {}, modelUsage: {}, num_turns: 1, duration_ms: 2, total_cost_usd: 3 };
    for (const outcome of ["success", "failed"] as const) {
      const bare = projectResult({ outcome, subtype: "s", errors: [], wire });
      assert.equal("leg_id" in bare.payload, false);
      assert.equal("usage_through" in bare.payload, false);
      const stamped = projectResult({ outcome, subtype: "s", errors: [], usageBasis: "session_cumulative", usageStamp: { legId: "L", usageThrough: 4 }, wire });
      assert.equal(stamped.payload["leg_id"], "L");
      assert.equal(stamped.payload["usage_through"], 4);
      assert.equal(stamped.payload["usage_basis"], "session_cumulative");
    }
    // the init stamp omits sdk_session_id when the SDK session id is unknown
    assert.deepEqual(projectInit("m", undefined, undefined, { legId: "L" }).payload, { event: "init", model: "m", leg_id: "L" });
  });
});

// --- SdkExecutor-level: liveness and projection equivalence ---------------------------------

describe("SdkExecutor with the usage recorder", () => {
  let homeDir: string;
  beforeEach(() => {
    homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-usage-capture-home-"));
  });
  afterEach(() => {
    fs.rmSync(homeDir, { recursive: true, force: true });
  });

  function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; emits: EmittedMessage[] } {
    const emits: EmittedMessage[] = [];
    const ctx: RunContext = {
      runId: "r1",
      issueIid: 5,
      issueTitle: "Fix login",
      issueDescription: "please implement",
      worktreePath: nonexistentWorktree(),
      branch: "agent/issue-5",
      emit: (m) => emits.push(m),
      oauthToken: "dummy-oauth-token-do-not-scan-0000",
      agents: [],
      config: null,
      sessionId: null,
      onSessionId: () => {},
      gatePlan: async () => ({ kind: "approve", selection: { status: "absent" } }),
      pullFollowUp: () => undefined,
      reportIteration: () => {},
      ...overrides,
    };
    return { ctx, emits };
  }

  function fakeTurns(scripts: (SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>))[]): { queryFn: SdkQueryFn; options: SdkOptions[] } {
    const options: SdkOptions[] = [];
    let i = 0;
    const queryFn: SdkQueryFn = (params) => {
      const script = scripts[Math.min(i, scripts.length - 1)]!;
      i++;
      options.push(params.options);
      return (async function* () {
        for await (const p of params.prompt) void p;
        const s = typeof script === "function" ? script(params.options.abortController!.signal) : script;
        if (Array.isArray(s)) for (const m of s) yield m;
        else yield* s as AsyncIterable<SDKMessage>;
      })();
    };
    return { queryFn, options };
  }

  it("a ping-only stall still trips the idle watchdog (stream events are not liveness)", async () => {
    const recorder = newRecorder(new FakeUsageClient());
    const { queryFn } = fakeTurns([
      (signal) =>
        (async function* () {
          yield init();
          // pings (and a whole partial message) every 5 ms for as long as the executor lets it
          const keepAlive = setInterval(() => {}, 1_000);
          try {
            while (!signal.aborted) {
              for (const f of [...partialsBefore("m1", 1), ...partialsAfter()]) yield f;
              await new Promise((r) => setTimeout(r, 5));
            }
          } finally {
            clearInterval(keepAlive);
          }
        })(),
    ]);
    const { ctx } = makeCtx({ config: { idle_timeout_seconds: 0.08, run_timeout_seconds: 3 }, usage: recorder });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /idle timeout/);
  });

  it("the run lane asks for partial messages; subagent text is forwarded only with a usage sink", async () => {
    const script = [[asst("m1", [toolUse("t1", "mcp__uzi__submit_plan", { plan_md: "plan" })]), result()], [asst("m2", [toolUse("t2", "mcp__uzi__signal_done")]), result()]];
    const withSink = fakeTurns(script);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn: withSink.queryFn }).run(makeCtx({ usage: newRecorder(new FakeUsageClient()) }).ctx);
    assert.ok(withSink.options.length >= 2);
    for (const o of withSink.options) {
      assert.equal(o.includePartialMessages, true);
      assert.equal(o.forwardSubagentText, true);
    }
    const without = fakeTurns(script);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn: without.queryFn }).run(makeCtx().ctx);
    for (const o of without.options) {
      assert.equal(o.includePartialMessages, true);
      assert.equal("forwardSubagentText" in o, false);
    }
  });

  // The richest raw frame shapes sdk-executor.test.ts uses: a plan turn and an implement turn with
  // thinking-only, text, a Task tool_use, a subagent tool_use + result, a main tool_result, and a
  // signal-only frame, each carrying message.id and usage.
  function baseTurns(): SDKMessage[][] {
    return [
      [init("sess-1"), asst("p1", [text("planning")]), asst("p2", [toolUse("t1", "mcp__uzi__submit_plan", { plan_md: "# Plan\n- do it" })]), result()],
      [
        init("sess-1"),
        asst("i1", [thinking("hmm")]),
        asst("i2", [toolUse("task1", "Task", { description: "review" })]),
        asst("s1", [toolUse("sub-bash", "Bash", { command: "ls" })], SUBAGENT),
        toolResultFrame("sub-bash", SUBAGENT),
        toolResultFrame("task1"),
        asst("i3", [text("all reviewed")]),
        asst("i4", [toolUse("t2", "mcp__uzi__signal_done")]),
        result(),
      ],
    ];
  }

  /** The same turns as the CLI emits them with partial messages on: stream_events around every
   *  assistant message, a message seen only through its stream events, and the subagent text the
   *  forwardSubagentText flag adds (a text-only message and its user-frame prompt). */
  function partialTurns(): SDKMessage[][] {
    const withPartials = (frame: SDKMessage, n: number, parent: string | null = null): SDKMessage[] => {
      const id = ((frame as unknown as { message: { id: string } }).message).id;
      return [...partialsBefore(id, n, parent), frame, ...partialsAfter(parent)];
    };
    return baseTurns().map((turn) => {
      const out: SDKMessage[] = [];
      for (const frame of turn) {
        const rec = frame as unknown as { type: string; message?: { id?: string }; parent_tool_use_id?: string };
        if (rec.type === "assistant") {
          // the CLI never forwards a subagent's own stream_events: only the main lane is wrapped
          if (rec.parent_tool_use_id === undefined) out.push(...withPartials(frame, 10));
          else if (rec.message?.id === "s1") {
            // a MIXED subagent frame: forwardSubagentText adds the subagent's narration text next to
            // its tool_use; the projection must keep the tool_use and drop the added text.
            out.push(asst("s1", [text("subagent narration"), toolUse("sub-bash", "Bash", { command: "ls" })], SUBAGENT));
          } else out.push(frame);
          if (rec.message?.id === "s1") {
            out.push(
              asst("s2", [text("subagent text-only")], SUBAGENT),
              asMsg({ type: "user", session_id: "sess-1", ...SUBAGENT, message: { content: [{ type: "text", text: "the subagent prompt" }] } }),
              ...partialsBefore("ghost", 4),
              ...partialsAfter(null, 6),
            );
          }
        } else out.push(ev({ type: "ping" }), frame, ev({ type: "ping" }));
      }
      return out;
    });
  }

  const STAMPS = ["leg_id", "sdk_session_id", "usage_through"];
  function strip(m: EmittedMessage): EmittedMessage {
    const payload = { ...m.payload };
    for (const k of STAMPS) delete payload[k];
    return { ...m, payload };
  }

  it("PROJECTION EQUIVALENCE: partials on + usage sink persist the identical message sequence as partials off", async () => {
    const off = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn: fakeTurns(baseTurns()).queryFn }).run(off.ctx);

    const client = new FakeUsageClient();
    const recorder = newRecorder(client);
    const on = makeCtx({ usage: recorder });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn: fakeTurns(partialTurns()).queryFn }).run(on.ctx);

    assert.ok(off.emits.length > 8, "a non-trivial transcript");
    assert.equal(on.emits.length, off.emits.length, "same persisted message count");
    assert.deepEqual(on.emits.map(strip), off.emits.map(strip), "kinds and payloads deep-equal modulo the documented stamps");
    // and the stamps are the only difference
    const stamped = on.emits.filter((m, i) => JSON.stringify(m.payload) !== JSON.stringify(off.emits[i]!.payload));
    assert.deepEqual(
      stamped.map((m) => [m.payload["event"], Object.keys(m.payload).filter((k) => STAMPS.includes(k)).sort()]),
      [
        ["init", ["leg_id", "sdk_session_id"]],
        ["result", ["leg_id", "usage_through"]],
        ["init", ["leg_id", "sdk_session_id"]],
        ["result", ["leg_id", "usage_through"]],
      ],
    );

    // the recorder saw everything the projection hides: signal-only, text-only subagent, stream-only
    await recorder.drain();
    const byLeg = new Map<string, string[]>();
    for (const r of client.records) byLeg.set(r.leg_id, [...(byLeg.get(r.leg_id) ?? []), `${r.ordinal}:${r.message_id}${r.subagent ? ":sub" : ""}`]);
    assert.deepEqual([...byLeg.values()], [
      ["1:p1", "2:p2"],
      ["1:i1", "2:i2", "3:s1:sub", "4:s2:sub", "5:ghost", "6:i3", "7:i4"],
    ]);
    const results = on.emits.filter((m) => m.payload["event"] === "result");
    assert.deepEqual(results.map((m) => m.payload["usage_through"]), [2, 7]);
    assert.deepEqual(
      client.legs.map((l) => l.closed_through),
      [2, 7],
    );
    assert.deepEqual(
      results.map((m) => m.payload["leg_id"]),
      client.legs.map((l) => l.leg_id),
    );
  });
});
