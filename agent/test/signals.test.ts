import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { buildSignalMcpServer, isSignalToolName, scanSignals } from "../src/signals.js";

// The plan/done signals are observed from the SDK message stream, so a scripted
// tool_use proves them with no live session.

function toolUse(name: string, input: unknown): unknown {
  return { type: "assistant", session_id: "s", message: { content: [{ type: "tool_use", id: "t", name, input }] } };
}

/** Same as toolUse but tagged as a REAL subagent frame (PRD #43 M2): the SDK
 *  stamps subagent-produced frames with BOTH a top-level `subagent_type` label
 *  (sibling of type/message/session_id — sdk.d.ts:2762-2777, the same field
 *  sdk-messages.ts:28-30 attributes by) AND a non-null `parent_tool_use_id` (the
 *  spawning Agent tool_use id). The default carries both, like the wire does;
 *  pass `extra` to isolate a single discriminator. */
function subagentToolUse(
  name: string,
  input: unknown,
  extra: Record<string, unknown> = { subagent_type: "coder", parent_tool_use_id: "toolu_parent" },
): unknown {
  return {
    type: "assistant",
    session_id: "s",
    ...extra,
    message: { content: [{ type: "tool_use", id: "t", name, input }] },
  };
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

  // PRD #43 M2 / Decision 3: the workflow signals gate the run and end the loop —
  // only the lead's MAIN-THREAD frames may carry them. A subagent frame reaching
  // signal_done/submit_plan (prompt-injected, buggy, or via a future tool leak)
  // must NOT latch done or the plan, or the loop ends on a partial, unreviewed
  // tree. This worker-side scan is the LOAD-BEARING guarantee for that success
  // criterion: it holds no matter what the SDK does with the server-level
  // `mcp__uzi` disallowedTools denial (whether that denial wins over a custom
  // template's explicit `tools` allowlist is unproven from the SDK types).
  it("ignores signal_done from a real subagent frame (subagent_type + parent_tool_use_id)", () => {
    const r = scanSignals(subagentToolUse("mcp__uzi__signal_done", {}));
    assert.deepStrictEqual(r, {}, "a subagent must not be able to end the run");
    assert.notStrictEqual(r.done, true);
  });

  it("ignores submit_plan from a real subagent frame (subagent_type + parent_tool_use_id)", () => {
    const r = scanSignals(subagentToolUse("mcp__uzi__submit_plan", { plan_md: "# injected" }));
    assert.deepStrictEqual(r, {}, "a subagent must not be able to submit the plan");
    assert.strictEqual(r.plan, undefined);
  });

  it("ignores signals when tagged ONLY by subagent_type (no parent_tool_use_id)", () => {
    // Each discriminator alone is sufficient — a subagent frame that omitted its
    // parent id still can't latch a signal.
    const r = scanSignals(subagentToolUse("mcp__uzi__signal_done", {}, { subagent_type: "reviewer" }));
    assert.deepStrictEqual(r, {});
  });

  it("ignores signals when tagged ONLY by parent_tool_use_id (no subagent_type)", () => {
    // The other half: a subagent frame is also identifiable by its non-null
    // parent_tool_use_id (sdk.d.ts:2765, required string|null — null only on the
    // main thread), so a frame that dropped its subagent_type label still can't
    // latch a signal.
    const r = scanSignals(subagentToolUse("mcp__uzi__signal_done", {}, { parent_tool_use_id: "toolu_abc" }));
    assert.deepStrictEqual(r, {});
  });

  it("still honors signals on a main-thread frame (parent_tool_use_id: null, no subagent_type)", () => {
    // Guard against over-rejection: a real lead frame carries parent_tool_use_id:
    // null and no subagent_type, and its signals must still latch.
    const r = scanSignals(subagentToolUse("mcp__uzi__signal_done", {}, { parent_tool_use_id: null }));
    assert.deepStrictEqual(r, { done: true });
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
