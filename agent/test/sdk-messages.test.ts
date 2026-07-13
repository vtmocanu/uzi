import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mapSdkMessage, isErrorResult, isResult, sessionIdOf, assistantUsageOf } from "../src/sdk-messages.js";

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
