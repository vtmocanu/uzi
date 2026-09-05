import assert from "node:assert/strict";
import { appendFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { Probe, callOutput, message, tool } from "./harness.mjs";

const dynamicTools = [{
  name: "m0_mark", description: "M0 fixed harmless marker fixture",
  inputSchema: { type: "object", properties: { claimedThreadId: { type: "string" } }, additionalProperties: false },
}];
const result = (success, text) => ({ result: { success, contentItems: [{ type: "inputText", text }] } });

test("dynamic tool scope uses actual callback thread identity, not spoofed arguments", async (t) => {
  let rootId;
  let accepted = 0;
  const p = await Probe.create(t, {
    dynamicTool: async (request, probe) => {
      const params = request.params;
      assert.equal(params.tool, "m0_mark");
      assert.equal(params.namespace, null);
      assert.equal(params.callId, "mark");
      assert.ok(typeof params.turnId === "string" && params.turnId.length > 0);
      if (params.threadId !== rootId) return result(false, "M0 scope denied");
      await appendFile(path.join(probe.workspace, "dynamic-marker"), "M0 authorized root\n");
      accepted++;
      return result(true, "M0 marker recorded");
    },
    respond: (body) => callOutput(body, "mark") ? [message()] :
      [tool("mark", "m0_mark", { claimedThreadId: rootId })],
  });
  rootId = await p.thread("never", dynamicTools);
  const otherId = await p.thread("never", dynamicTools);
  for (const threadId of [rootId, otherId]) {
    const turnId = await p.turn(threadId);
    const terminal = await p.completed(threadId, turnId);
    assert.equal(terminal.params.turn.status, "completed");
    const callback = p.dynamicCalls.find((request) => request.params.threadId === threadId);
    assert.ok(callback, "actual app-server callback reached the client");
    assert.equal(callback.params.turnId, turnId);
    assert.equal(callback.params.arguments.claimedThreadId, rootId);
  }
  assert.equal(p.dynamicCalls.length, 2);
  assert.equal(p.requests.length, 4);
  assert.equal(accepted, 1);
  assert.equal(await p.marker("dynamic-marker"), "M0 authorized root\n");
  const outputs = p.requests.map((body) => callOutput(body, "mark")).filter(Boolean);
  assert.match(JSON.stringify(outputs[0]), /M0 marker recorded/);
  assert.match(JSON.stringify(outputs[1]), /M0 scope denied/);
  const denied = p.messages.find((value) => value.method === "item/completed"
    && value.params.threadId === otherId && value.params.item.type === "dynamicToolCall");
  assert.equal(denied?.params.item.status, "failed");
  t.diagnostic(JSON.stringify({ callbacks: p.dynamicCalls.length, accepted,
    actualOtherThreadDenied: true, claimedRootIdIgnored: true }));
});

for (const failure of ["malformed", "error"]) {
  test(`dynamic tool callback ${failure} becomes a failed tool result`, async (t) => {
    const p = await Probe.create(t, {
      dynamicTool: () => failure === "error"
        ? { error: { code: -32000, message: "M0 deterministic callback error" } }
        : { result: { success: "invalid", contentItems: "invalid" } },
      respond: (body) => callOutput(body, "mark") ? [message()] : [tool("mark", "m0_mark", {})],
    });
    const threadId = await p.thread("never", dynamicTools);
    const turnId = await p.turn(threadId);
    const terminal = await p.completed(threadId, turnId);
    assert.equal(terminal.params.turn.status, "completed");
    assert.equal(p.dynamicCalls.length, 1);
    assert.equal(p.dynamicCalls[0].params.threadId, threadId);
    assert.equal(p.dynamicCalls[0].params.turnId, turnId);
    assert.equal(p.requests.length, 2);
    assert.ok(callOutput(p.requests[1], "mark"));
    const completed = p.messages.find((value) => value.method === "item/completed"
      && value.params.item.type === "dynamicToolCall");
    assert.equal(completed?.params.item.status, "failed");
    assert.equal(await p.marker("dynamic-marker"), null);
    t.diagnostic(JSON.stringify({ failure, status: completed.params.item.status,
      output: callOutput(p.requests[1], "mark") }));
  });
}
