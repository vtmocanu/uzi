import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mapSdkMessage, isErrorResult, isResult, sessionIdOf } from "../src/sdk-messages.js";

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
        },
      },
    ]);
  });

  it("forwards duration_ms and total_cost_usd on a success result when present (PRD #11)", () => {
    const out = mapSdkMessage({
      type: "result",
      subtype: "success",
      is_error: false,
      num_turns: 3,
      duration_ms: 12400,
      total_cost_usd: 0.0731,
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
        },
      },
    ]);
  });

  it("maps an error result to an error message", () => {
    const out = mapSdkMessage({
      type: "result",
      subtype: "error_max_turns",
      is_error: true,
      errors: ["hit the cap"],
    });
    assert.deepStrictEqual(out, [
      { kind: "error", agent: "lead", payload: { event: "result", subtype: "error_max_turns", errors: ["hit the cap"] } },
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
