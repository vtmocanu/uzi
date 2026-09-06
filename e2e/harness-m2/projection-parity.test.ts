// M2 neutral-harness-seam differential test: ONE projection core.
//
// PRD #1146 (#1106 M2) milestone m4 validation. Pins that the chat mapper
// (mapSdkMessage, sdk-messages.ts) and the run reducer's projection
// (projectItem/projectResult, harness-messages.ts) are the SAME core: for a
// representative frame of each kind, mapSdkMessage(rawSdkFrame) is DEEP-EQUAL to
// projectItem/projectResult over the equivalent HAND-BUILT neutral input — same
// key set and same undefined-key presence (deepStrictEqual is key-order-independent;
// exact key ORDER is pinned by the existing agent/test mapper fixtures, the D0
// oracle these differential tests complement). Pure and credential-free.

import test from "node:test";
import assert from "node:assert/strict";

import { mapSdkMessage } from "../../agent/src/sdk-messages.js";
import { projectItem, projectResult } from "../../agent/src/harness-messages.js";
import type { HarnessAttribution, HarnessItem } from "../../agent/src/harness.js";

/** The attribution a lead assistant/user frame with no subagent fields decodes to. */
const LEAD_AT: HarnessAttribution = { agent: "lead" };

test("assistant text frame: mapSdkMessage == projectItem(text)", () => {
  const raw = {
    type: "assistant",
    message: { role: "assistant", content: [{ type: "text", text: "hello world" }] },
  };
  const item: HarnessItem = { kind: "text", text: "hello world" };
  assert.deepStrictEqual(mapSdkMessage(raw), [projectItem(item, LEAD_AT)]);
});

test("assistant tool_use frame: mapSdkMessage == projectItem(started tool)", () => {
  const raw = {
    type: "assistant",
    message: {
      role: "assistant",
      content: [{ type: "tool_use", id: "tool-1", name: "Bash", input: { cmd: "ls -la" } }],
    },
  };
  const item: HarnessItem = {
    kind: "tool",
    phase: "started",
    id: "tool-1",
    name: "Bash",
    input: { cmd: "ls -la" },
  };
  assert.deepStrictEqual(mapSdkMessage(raw), [projectItem(item, LEAD_AT)]);
});

test("user tool_result frame: mapSdkMessage == projectItem(finished tool)", () => {
  const raw = {
    type: "user",
    message: {
      role: "user",
      content: [{ type: "tool_result", tool_use_id: "tool-1", content: "stdout", is_error: false }],
    },
  };
  const item: HarnessItem = {
    kind: "tool",
    phase: "finished",
    id: "tool-1",
    output: "stdout",
    isError: false,
  };
  assert.deepStrictEqual(mapSdkMessage(raw), [projectItem(item, LEAD_AT)]);
});

test("success result frame: mapSdkMessage == projectResult(success)", () => {
  const raw = {
    type: "result",
    subtype: "success",
    is_error: false,
    num_turns: 3,
    duration_ms: 1234,
    total_cost_usd: 0.5,
    usage: { input_tokens: 10 },
    modelUsage: { "claude-dummy": { input_tokens: 10 } },
  };
  const projected = projectResult({
    outcome: "success",
    subtype: "success",
    errors: [],
    wire: {
      usage: { input_tokens: 10 },
      modelUsage: { "claude-dummy": { input_tokens: 10 } },
      num_turns: 3,
      duration_ms: 1234,
      total_cost_usd: 0.5,
    },
  });
  assert.deepStrictEqual(mapSdkMessage(raw), [projected]);
});

test("failed result frame: mapSdkMessage == projectResult(failed)", () => {
  const raw = {
    type: "result",
    subtype: "error_max_turns",
    is_error: true,
    errors: ["boom"],
    num_turns: 1,
    duration_ms: 5,
    total_cost_usd: 0.1,
    usage: { input_tokens: 2 },
    modelUsage: { "claude-dummy": { input_tokens: 2 } },
  };
  const projected = projectResult({
    outcome: "failed",
    subtype: "error_max_turns",
    errors: ["boom"],
    wire: {
      usage: { input_tokens: 2 },
      modelUsage: { "claude-dummy": { input_tokens: 2 } },
      num_turns: 1,
      duration_ms: 5,
      total_cost_usd: 0.1,
    },
  });
  assert.deepStrictEqual(mapSdkMessage(raw), [projected]);
});
