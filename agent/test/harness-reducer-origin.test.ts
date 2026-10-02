import test from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import path from "node:path";

import { ClaudeHarness, type ClaudeTurnConfig } from "../src/claude-harness.js";
import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import type { RunTurnRequest } from "../src/harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

const config: ClaudeTurnConfig = {
  cwd: "/tmp/uzi-origin-test", env: {}, skillsPluginPath: "/tmp/uzi-origin-plugin",
  skills: [], systemPrompt: "sys", agents: {}, mcpServers: {}, preToolUse: [],
};
const request: RunTurnRequest = {
  prompt: "p", systemPrompt: "sys", signal: new AbortController().signal,
  phase: "implement", agents: {}, leadSkills: [],
};

function assistant(text: string, markers: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    type: "assistant", message: {
      role: "assistant", content: [{ type: "text", text }],
      usage: { input_tokens: 10, output_tokens: 2 }, model: "claude-test",
    }, ...markers,
  };
}

async function reduceRaw(frames: Record<string, unknown>[]) {
  const queryFn = (() => (async function* () { for (const frame of frames) yield frame; })()) as unknown as SdkQueryFn;
  const harness = new ClaudeHarness({
    queryFn, spawn: () => ({ pid: undefined }), kill: () => true, log: nullLogger(),
    contextUsageTimeoutMs: 2000, spawnedPids: new Set<number>(),
    homeDir: path.join(os.tmpdir(), "uzi-origin-home-does-not-exist"),
  });
  harness.prepareTurn(config, { pid: undefined });
  const reducer = new RunTurnReducerImpl({
    request: () => { contextRequests++; }, get: async () => undefined,
  });
  let contextRequests = 0;
  reducer.beginTurn();
  const messages = [];
  for await (const event of harness.startTurn(request).events) {
    messages.push(...(await reducer.accept(event)).messages);
  }
  return { result: reducer.finish({ kind: "exhausted" }).result, messages, contextRequests };
}

test("Claude main assistant text and usage retain lead finalText and request context once", async () => {
  const out = await reduceRaw([assistant("main first"), assistant("main second")]);
  assert.equal(out.result.finalText, "main first\nmain second");
  assert.equal(out.contextRequests, 1);
  assert.equal(out.result.subagentActivity, undefined);
  assert.deepEqual(out.messages.filter((m) => m.kind === "text").map((m) => m.agent), ["lead", "lead"]);
});

for (const [label, markers] of [
  ["parent id only", { parent_tool_use_id: "toolu_parent" }],
  ["parent id and lead role", { parent_tool_use_id: "toolu_parent", subagent_type: "lead" }],
] as const) {
  for (const check of ["finalText", "context", "activity"] as const) {
    test(`Claude ${label} child origin controls ${check} despite displayed agent lead`, async () => {
      const out = await reduceRaw([assistant("child text", markers)]);
      assert.equal(out.messages.find((m) => m.kind === "text")?.agent, "lead");
      if (check === "finalText") assert.equal(out.result.finalText, undefined, "child text cannot become lead finalText");
      if (check === "context") assert.equal(out.contextRequests, 0, "child usage cannot request lead context");
      if (check === "activity") assert.equal(out.result.subagentActivity, true, "child origin counts as subagent activity");
    });
  }
}

test("Claude neutral subagent frame without instance id remains child", async () => {
  const out = await reduceRaw([assistant("unkeyed child", { subagent_type: "lead" })]);
  assert.equal(out.messages.find((m) => m.kind === "text")?.agent, "lead");
  assert.equal(out.result.finalText, undefined);
  assert.equal(out.contextRequests, 0);
  assert.equal(out.result.subagentActivity, true);
});
