import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mapSdkMessage, isErrorResult, isResult, sessionIdOf, assistantModelOf, assistantUsageOf, orphanInstanceKind } from "../src/sdk-messages.js";

// SDK stream events → run_messages (kinds + agent attribution). Built with
// hand-rolled SDK-shaped objects; no live session.

describe("mapSdkMessage", () => {
  it("maps assistant text/thinking/tool_use blocks with lead attribution", () => {
    const out = mapSdkMessage({
      type: "assistant",
      session_id: "s1",
      message: {
        content: [
          { type: "text", text: "hello" },
          { type: "thinking", thinking: "pondering" },
          { type: "tool_use", id: "tu1", name: "Bash", input: { command: "ls" } },
        ],
      },
    });
    assert.deepStrictEqual(out, [
      { kind: "text", agent: "lead", payload: { text: "hello" } },
      { kind: "thinking", agent: "lead", payload: { text: "pondering" } },
      { kind: "tool_use", agent: "lead", payload: { id: "tu1", name: "Bash", input: { command: "ls" } } },
    ]);
  });

  it("attributes a subagent frame to its subagent_type", () => {
    const out = mapSdkMessage({
      type: "assistant",
      subagent_type: "reviewer",
      message: { content: [{ type: "text", text: "looks good" }] },
    });
    assert.deepStrictEqual(out, [{ kind: "text", agent: "reviewer", payload: { text: "looks good" } }]);
  });

  it("maps a user tool_result but ignores plain user text (prompt echoes)", () => {
    const out = mapSdkMessage({
      type: "user",
      message: {
        content: [
          { type: "text", text: "the original prompt echo" },
          { type: "tool_result", tool_use_id: "tu1", content: "ok", is_error: false },
        ],
      },
    });
    assert.deepStrictEqual(out, [
      { kind: "tool_result", agent: "lead", payload: { tool_use_id: "tu1", content: "ok", is_error: false } },
    ]);
  });

  it("maps a success result to a status message (unguarded passthrough, absent fields are undefined)", () => {
    const out = mapSdkMessage({ type: "result", subtype: "success", is_error: false, num_turns: 3 });
    assert.deepStrictEqual(out, [
      {
        kind: "status",
        agent: "lead",
        payload: {
          event: "result",
          subtype: "success",
          num_turns: 3,
          duration_ms: undefined,
          total_cost_usd: undefined,
          usage: undefined,
          modelUsage: undefined,
        },
      },
    ]);
  });

  it("forwards duration_ms/total_cost_usd + usage/modelUsage on a success result (PRD #11, #40 M1)", () => {
    const usage = { input_tokens: 1200, output_tokens: 800, cache_read_input_tokens: 400, cache_creation_input_tokens: 50 };
    const modelUsage = { "claude-fable-5": { inputTokens: 1200, outputTokens: 800, costUSD: 0.0731 } };
    const out = mapSdkMessage({
      type: "result",
      subtype: "success",
      is_error: false,
      num_turns: 3,
      duration_ms: 12400,
      total_cost_usd: 0.0731,
      usage,
      modelUsage,
    });
    assert.deepStrictEqual(out, [
      {
        kind: "status",
        agent: "lead",
        payload: {
          event: "result",
          subtype: "success",
          num_turns: 3,
          duration_ms: 12400,
          total_cost_usd: 0.0731,
          usage,
          modelUsage,
        },
      },
    ]);
  });

  it("maps an error result to an error message, forwarding usage/cost/turns/duration (PRD #40 Decision 4)", () => {
    const usage = { input_tokens: 500, output_tokens: 120, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 };
    const modelUsage = { "claude-fable-5": { inputTokens: 500, outputTokens: 120, costUSD: 0.031 } };
    const out = mapSdkMessage({
      type: "result",
      subtype: "error_max_turns",
      is_error: true,
      errors: ["hit the cap"],
      usage,
      modelUsage,
      total_cost_usd: 0.031,
      num_turns: 7,
      duration_ms: 44000,
    });
    assert.deepStrictEqual(out, [
      {
        kind: "error",
        agent: "lead",
        payload: {
          event: "result",
          subtype: "error_max_turns",
          errors: ["hit the cap"],
          usage,
          modelUsage,
          total_cost_usd: 0.031,
          num_turns: 7,
          duration_ms: 44000,
        },
      },
    ]);
  });

  it("an error result with no usage yields undefined accounting keys (absent → drops on the wire)", () => {
    const out = mapSdkMessage({ type: "result", subtype: "error_during_execution", is_error: true });
    assert.deepStrictEqual(out, [
      {
        kind: "error",
        agent: "lead",
        payload: {
          event: "result",
          subtype: "error_during_execution",
          errors: [],
          usage: undefined,
          modelUsage: undefined,
          total_cost_usd: undefined,
          num_turns: undefined,
          duration_ms: undefined,
        },
      },
    ]);
  });

  it("maps a system init frame to a status heartbeat", () => {
    const out = mapSdkMessage({ type: "system", subtype: "init", model: "claude-fable-5" });
    assert.deepStrictEqual(out, [{ kind: "status", agent: "lead", payload: { event: "init", model: "claude-fable-5" } }]);
  });

  it("skips partial stream events and unknown frames", () => {
    assert.deepStrictEqual(mapSdkMessage({ type: "stream_event", event: {} }), []);
    assert.deepStrictEqual(mapSdkMessage({ type: "system", subtype: "task_started" }), []);
    assert.deepStrictEqual(mapSdkMessage({ type: "totally_unknown" }), []);
    assert.deepStrictEqual(mapSdkMessage(null), []);
    assert.deepStrictEqual(mapSdkMessage(42), []);
  });
});

describe("result helpers", () => {
  it("isResult / isErrorResult classify result frames", () => {
    assert.strictEqual(isResult({ type: "result", subtype: "success" }), true);
    assert.strictEqual(isResult({ type: "assistant" }), false);
    assert.strictEqual(isErrorResult({ type: "result", subtype: "success", is_error: false }), false);
    assert.strictEqual(isErrorResult({ type: "result", subtype: "error_during_execution", is_error: true }), true);
    assert.strictEqual(isErrorResult({ type: "result", subtype: "success", is_error: true }), true);
    assert.strictEqual(isErrorResult({ type: "assistant" }), false);
  });
});

describe("sessionIdOf", () => {
  it("extracts a string session_id", () => {
    assert.strictEqual(sessionIdOf({ type: "system", session_id: "abc" }), "abc");
    assert.strictEqual(sessionIdOf({ type: "system" }), undefined);
    assert.strictEqual(sessionIdOf(null), undefined);
  });
});

describe("assistantUsageOf (PRD #40 Decision 11)", () => {
  it("returns the assistant frame's per-call message.usage", () => {
    const usage = { input_tokens: 900, output_tokens: 300, cache_read_input_tokens: 120, cache_creation_input_tokens: 0 };
    assert.deepStrictEqual(
      assistantUsageOf({ type: "assistant", message: { usage, content: [{ type: "text", text: "hi" }] } }),
      usage,
    );
  });

  it("keeps working for subagent frames (usage rides alongside subagent_type)", () => {
    const usage = { input_tokens: 10, output_tokens: 5 };
    assert.deepStrictEqual(
      assistantUsageOf({ type: "assistant", subagent_type: "reviewer", message: { usage, content: [] } }),
      usage,
    );
  });

  it("returns undefined for non-assistant frames and missing/malformed usage", () => {
    assert.strictEqual(assistantUsageOf({ type: "result", subtype: "success", usage: { input_tokens: 1 } }), undefined);
    assert.strictEqual(assistantUsageOf({ type: "user", message: { usage: { input_tokens: 1 } } }), undefined);
    assert.strictEqual(assistantUsageOf({ type: "assistant", message: { content: [] } }), undefined);
    assert.strictEqual(assistantUsageOf({ type: "assistant", message: { usage: 42, content: [] } }), undefined);
    assert.strictEqual(assistantUsageOf(null), undefined);
  });
});

describe("assistantModelOf (PRD #93 Decision 2)", () => {
  it("returns the assistant frame's message.model verbatim (no aliasing)", () => {
    assert.strictEqual(
      assistantModelOf({ type: "assistant", message: { model: "claude-opus-4-8", content: [{ type: "text", text: "hi" }] } }),
      "claude-opus-4-8",
    );
  });

  it("keeps working for subagent frames (model rides alongside subagent_type)", () => {
    // The agent→model mapping only exists on this path, and subagents are exactly
    // the case that makes the column worth showing (PRD #93 Decision 1).
    assert.strictEqual(
      assistantModelOf({ type: "assistant", subagent_type: "coder", message: { model: "claude-sonnet-5", content: [] } }),
      "claude-sonnet-5",
    );
  });

  it("returns undefined for non-assistant frames", () => {
    // The result frame's own model info lives in its `modelUsage` map, a different
    // (per-model, not per-agent) data path this helper must never touch.
    assert.strictEqual(assistantModelOf({ type: "result", subtype: "success", model: "claude-opus-4-8" }), undefined);
    assert.strictEqual(assistantModelOf({ type: "user", message: { model: "claude-opus-4-8" } }), undefined);
  });

  it("returns undefined for a missing or non-string model, and for null", () => {
    assert.strictEqual(assistantModelOf({ type: "assistant", message: { content: [] } }), undefined);
    assert.strictEqual(assistantModelOf({ type: "assistant", message: { model: 42, content: [] } }), undefined);
    assert.strictEqual(assistantModelOf(null), undefined);
  });
});

// PRD #99 M2: per-frame instance + label capture. All three attribution fields
// (subagent_type / parent_tool_use_id / task_description) are read off the frame
// with no correlation state, so these assertions are about ONE frame at a time.
describe("mapSdkMessage instance + label capture (PRD #99)", () => {
  it("carries parent_tool_use_id + task_description onto every block of a subagent frame", () => {
    const out = mapSdkMessage({
      type: "assistant",
      subagent_type: "coder",
      parent_tool_use_id: "toolu_A",
      task_description: "web gate UX",
      message: {
        content: [
          { type: "text", text: "editing" },
          { type: "tool_use", id: "tu1", name: "Edit", input: { file: "a.ts" } },
        ],
      },
    });
    // One frame explodes into N messages; EVERY one must carry the attribution,
    // otherwise half a subagent's contribution lands in the wrong lane.
    assert.equal(out.length, 2);
    for (const m of out) {
      assert.equal(m.agent, "coder");
      assert.equal(m.agentInstance, "toolu_A");
      assert.equal(m.agentLabel, "web gate UX");
    }
  });

  it("omits both keys entirely for a lead frame (null parent, no description)", () => {
    const out = mapSdkMessage({
      type: "assistant",
      parent_tool_use_id: null,
      message: { content: [{ type: "text", text: "delegating" }] },
    });
    assert.deepStrictEqual(out, [{ kind: "text", agent: "lead", payload: { text: "delegating" } }]);
    // Key ABSENCE, not just an undefined value: the batcher copies on
    // `!== undefined`, and the API turns an absent field into SQL NULL. An
    // explicit "" would persist as a lane key that swallows every lead message.
    const rec = out[0] as unknown as Record<string, unknown>;
    assert.ok(!("agentInstance" in rec), "a lead frame must not carry an agentInstance key");
    assert.ok(!("agentLabel" in rec), "a lead frame must not carry an agentLabel key");
  });

  it("keeps two parallel same-role invocations distinct", () => {
    const frame = (instance: string, label: string, text: string) =>
      mapSdkMessage({
        type: "assistant",
        subagent_type: "coder",
        parent_tool_use_id: instance,
        task_description: label,
        message: { content: [{ type: "text", text }] },
      })[0];
    const a = frame("toolu_A", "API wiring", "unit A");
    const b = frame("toolu_B", "web gate UX", "unit B");
    assert.equal(a?.agent, b?.agent, "both are the coder role — the role alone cannot tell them apart");
    assert.notEqual(a?.agentInstance, b?.agentInstance, "the instance id is what keeps them in separate lanes");
    assert.equal(a?.agentLabel, "API wiring");
    assert.equal(b?.agentLabel, "web gate UX");
  });

  it("carries the attribution onto user tool_result frames too", () => {
    const out = mapSdkMessage({
      type: "user",
      subagent_type: "reviewer",
      parent_tool_use_id: "toolu_C",
      task_description: "audit unit A",
      message: { content: [{ type: "tool_result", tool_use_id: "tu1", content: "ok", is_error: false }] },
    });
    assert.equal(out.length, 1);
    assert.equal(out[0]?.agentInstance, "toolu_C");
    assert.equal(out[0]?.agentLabel, "audit unit A");
  });

  it("carries an instance with no task_description (label absent, id present)", () => {
    const out = mapSdkMessage({
      type: "assistant",
      subagent_type: "tester",
      parent_tool_use_id: "toolu_D",
      message: { content: [{ type: "text", text: "running" }] },
    });
    assert.equal(out[0]?.agentInstance, "toolu_D");
    const rec = out[0] as unknown as Record<string, unknown>;
    assert.ok(!("agentLabel" in rec), "a frame with no task_description must not carry an agentLabel key");
  });

  it("degrades to absent (never throws) when the SDK reshapes the fields", () => {
    // The defensive-probe posture: a non-string lands as undefined -> SQL NULL ->
    // the web's role-name fallback, which is today's behaviour, not a crash.
    const out = mapSdkMessage({
      type: "assistant",
      subagent_type: "coder",
      parent_tool_use_id: { nested: "shape" },
      task_description: 42,
      message: { content: [{ type: "text", text: "x" }] },
    });
    assert.deepStrictEqual(out, [{ kind: "text", agent: "coder", payload: { text: "x" } }]);
  });

  it("pins the replay-frame gap: instance set while the role falls back to lead", () => {
    // SDKUserMessageReplay (sdk.d.ts:4334) has type 'user' and parent_tool_use_id
    // but NO subagent_type and NO task_description. This asserts the DOCUMENTED
    // M2 behaviour (see the decision note in sdk-messages.ts): the worker does not
    // guess a role, so the lane's role/label must be healed client-side in M3.
    // If someone later adds a replay guard, this test should fail and be revisited.
    const out = mapSdkMessage({
      type: "user",
      parent_tool_use_id: "toolu_A",
      uuid: "u-1",
      session_id: "s-1",
      message: { content: [{ type: "tool_result", tool_use_id: "tu1", content: "ok", is_error: false }] },
    });
    assert.equal(out.length, 1);
    assert.equal(out[0]?.agent, "lead", "no subagent_type on a replay frame -> the role falls back");
    assert.equal(out[0]?.agentInstance, "toolu_A", "...but the instance id IS present, so the lane key is a subagent's");
    const rec = out[0] as unknown as Record<string, unknown>;
    assert.ok(!("agentLabel" in rec), "a replay frame carries no task_description");
  });
});

// PRD #99: the orphan-instance DETECTOR. It exists so the "replay frames do not
// arrive in practice" reasoning stays checkable against reality instead of being
// assumed forever. It must key on the PRESENCE of subagent_type, never on
// agentOf's output — the `??` in agentOf collapses "field absent" and
// "field == 'lead'" into the same string, and this repo legitimately produces
// both (a repo roster may ship an agent NAMED lead).
describe("orphanInstanceKind (PRD #99)", () => {
  it("fires on a frame with parent_tool_use_id but no subagent_type", () => {
    assert.equal(
      orphanInstanceKind({ type: "user", parent_tool_use_id: "toolu_A", message: { content: [] } }),
      "user",
    );
  });

  it("does NOT fire for a repo-authored subagent NAMED lead", () => {
    // The case that broke the value-based version: subagent_type is PRESENT and
    // its value is "lead". This is a healthy subagent frame, and a detector that
    // fired here would mean "working as intended" and "replay artifact" at once.
    assert.equal(
      orphanInstanceKind({
        type: "assistant",
        subagent_type: "lead",
        parent_tool_use_id: "toolu_A",
        message: { content: [] },
      }),
      undefined,
    );
    // ...and the instance id is still captured for that frame, so two parallel
    // invocations of a repo `lead` stay in separate lanes.
    const out = mapSdkMessage({
      type: "assistant",
      subagent_type: "lead",
      parent_tool_use_id: "toolu_A",
      message: { content: [{ type: "text", text: "x" }] },
    });
    assert.equal(out[0]?.agentInstance, "toolu_A");
    assert.equal(out[0]?.agent, "lead");
  });

  it("does not fire for the parentless orchestrator or an ordinary subagent", () => {
    assert.equal(orphanInstanceKind({ type: "assistant", parent_tool_use_id: null, message: {} }), undefined);
    assert.equal(
      orphanInstanceKind({ type: "assistant", subagent_type: "coder", parent_tool_use_id: "toolu_B", message: {} }),
      undefined,
    );
  });

  it("is inert on junk", () => {
    assert.equal(orphanInstanceKind(null), undefined);
    assert.equal(orphanInstanceKind({ type: "result" }), undefined);
  });
});
