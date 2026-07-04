import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { buildSignalMcpServer, isSignalToolName, scanSignals } from "../src/signals.js";

// The plan/done signals are observed from the SDK message stream, so a scripted
// tool_use proves them with no live session.

function toolUse(name: string, input: unknown): unknown {
  return { type: "assistant", session_id: "s", message: { content: [{ type: "tool_use", id: "t", name, input }] } };
}

describe("scanSignals", () => {
  it("extracts plan_md from a submit_plan tool_use", () => {
    assert.deepStrictEqual(scanSignals(toolUse("mcp__uzi__submit_plan", { plan_md: "# Plan" })), { plan: "# Plan" });
  });

  it("treats a submit_plan with no body as a submitted (empty) plan, not 'no plan'", () => {
    const r = scanSignals(toolUse("mcp__uzi__submit_plan", {}));
    assert.strictEqual(r.plan, "");
  });

  it("detects signal_done", () => {
    assert.deepStrictEqual(scanSignals(toolUse("mcp__uzi__signal_done", {})), { done: true });
  });

  it("returns nothing for plain text, user frames, and unrelated tools", () => {
    assert.deepStrictEqual(scanSignals({ type: "assistant", message: { content: [{ type: "text", text: "hi" }] } }), {});
    assert.deepStrictEqual(scanSignals(toolUse("Bash", { command: "ls" })), {});
    assert.deepStrictEqual(scanSignals({ type: "user", message: { content: [] } }), {});
    assert.deepStrictEqual(scanSignals(undefined), {});
    assert.deepStrictEqual(scanSignals("garbage"), {});
  });

  it("captures both signals if a message carries them together", () => {
    const msg = {
      type: "assistant",
      message: {
        content: [
          { type: "tool_use", name: "mcp__uzi__submit_plan", input: { plan_md: "P" } },
          { type: "tool_use", name: "mcp__uzi__signal_done", input: {} },
        ],
      },
    };
    assert.deepStrictEqual(scanSignals(msg), { plan: "P", done: true });
  });
});

describe("isSignalToolName", () => {
  it("matches the qualified signal tools only", () => {
    assert.strictEqual(isSignalToolName("mcp__uzi__submit_plan"), true);
    assert.strictEqual(isSignalToolName("mcp__uzi__signal_done"), true);
    assert.strictEqual(isSignalToolName("submit_plan"), false);
    assert.strictEqual(isSignalToolName("Bash"), false);
    assert.strictEqual(isSignalToolName(undefined), false);
  });
});

describe("buildSignalMcpServer", () => {
  it("builds an in-process (sdk) MCP server named uzi", () => {
    const s = buildSignalMcpServer() as unknown as { type: string; name: string };
    assert.strictEqual(s.type, "sdk");
    assert.strictEqual(s.name, "uzi");
  });
});
