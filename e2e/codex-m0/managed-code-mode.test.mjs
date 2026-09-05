import assert from "node:assert/strict";
import { appendFile, readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { Probe, callOutput, message } from "./harness.mjs";

assert.equal(process.env.M0_MANAGED_ONESHOT, "1", "managed tests require the explicit M0_MANAGED_ONESHOT=1 container opt-in");
assert.equal(process.platform, "linux", "managed requirements fixture is container-only; never change host /etc");
assert.equal(
  await readFile("/etc/codex/requirements.toml", "utf8"),
  await readFile(new URL("./requirements.toml", import.meta.url), "utf8"),
  "mount the exact tracked fixture read-only at /etc/codex/requirements.toml",
);

const exec = (id, input) => ({ type: "custom_tool_call", call_id: id, name: "exec", input });

function assertCodeModeSchema(body, model) {
  assert.equal(body.model, model, "fixture receives the intended model, not the baseline fallback");
  const specs = [];
  const visit = (value) => {
    if (!value || typeof value !== "object") return;
    if (typeof value.name === "string") specs.push(value);
    for (const child of Object.values(value)) {
      if (Array.isArray(child)) child.forEach(visit);
      else visit(child);
    }
  };
  visit(body.tools);
  for (const item of body.input ?? []) if (item.type === "additional_tools") visit(item.tools);
  const cell = specs.find((value) => value.name === "exec");
  assert.equal(cell?.type, "custom", "real request advertises the code-mode custom exec tool");
  assert.equal(cell.format?.type, "grammar");
  assert.match(cell.description, /exec_command/);
  assert.match(cell.description, /timeout_ms\??: number/);
  assert.doesNotMatch(cell.description, /tty\??: boolean/);
  assert.doesNotMatch(cell.description, /declare const tools: \{ write_stdin\(/);
  assert.equal(specs.some((value) => value.name === "exec_command"), false,
    "code-mode-only exposes exec_command inside the cell declaration rather than a direct function spec");
}

function cellText(body, id) {
  const item = callOutput(body, id);
  assert.equal(item?.type, "custom_tool_call_output", "real code-mode cell produced a custom tool result");
  if (typeof item.output === "string") return item.output;
  assert.ok(Array.isArray(item.output), "cell output contains text items");
  return item.output.filter((value) => value.type === "input_text").map((value) => value.text).join("\n");
}

for (const model of ["gpt-6-astra", "gpt-5.6-sol"]) {
  for (const decision of ["accept", "decline", "malformed"]) {
    test(`${model} managed code-mode shell approval ${decision}`, async (t) => {
      const p = await Probe.create(t, {
        model, unifiedExec: false, codeMode: true, codeModeOnly: true, codeModeHost: true,
        approval: () => ({ decision }),
        respond: (body) => {
          assertCodeModeSchema(body, model);
          return callOutput(body, "marker") ? [message()] : [exec("marker", `
text(await tools.exec_command({
  cmd: "printf 'M0 marker\\n' > shell-marker", shell: "/bin/sh", login: false, timeout_ms: 1000,
}));`)];
        },
      });
      const threadId = await p.thread("untrusted");
      const turnId = await p.turn(threadId);
      const terminal = await p.completed(threadId, turnId);
      assert.equal(terminal.params.turn.status, "completed", JSON.stringify({ terminal, stderr: p.stderr, errors: p.errors }));
      assert.equal(p.requests.length, 2);
      const output = cellText(p.requests[1], "marker");
      assert.equal(p.approvals.length, 1, output);
      assert.equal(await p.marker(), decision === "accept" ? "M0 marker\n" : null);
      if (decision === "accept") {
        assert.match(output, /^Script completed/);
        assert.match(output, /exit_code.*0/);
      } else {
        assert.match(output, /^Script failed/);
        assert.match(output, decision === "decline" ? /rejected by user/ : /approval request failed/);
      }
      t.diagnostic(JSON.stringify({ model, decision, approvals: p.approvals.length, output }));
    });
  }

  test(`${model} metadata selects code-mode with both mode flags disabled`, async (t) => {
    const p = await Probe.create(t, {
      model, unifiedExec: false, codeMode: false, codeModeOnly: false, codeModeHost: true,
      respond: (body) => {
        assertCodeModeSchema(body, model);
        return callOutput(body, "metadata") ? [message()] : [exec("metadata", 'text("M0 real code-mode cell");')];
      },
    });
    const threadId = await p.thread("never");
    const turnId = await p.turn(threadId);
    assert.equal((await p.completed(threadId, turnId)).params.turn.status, "completed");
    assert.equal(p.requests.length, 2);
    const output = cellText(p.requests[1], "metadata");
    assert.match(output, /^Script completed/);
    assert.match(output, /M0 real code-mode cell/);
    t.diagnostic(JSON.stringify({ model, codeMode: false, codeModeOnly: false, codeModeHost: true, output }));
  });

  test(`${model} managed code-mode forced tty has no session and write_stdin is unavailable`, async (t) => {
    const p = await Probe.create(t, {
      model, unifiedExec: false, codeMode: true, codeModeOnly: true, codeModeHost: true,
      approval: () => ({ decision: "accept" }),
      respond: (body) => {
        assertCodeModeSchema(body, model);
        return callOutput(body, "input") ? [message()] : [exec("input", `
text({ shell: await tools.exec_command({
  cmd: "/bin/sh", shell: "/bin/sh", login: false, tty: true, yield_time_ms: 1, timeout_ms: 1000,
}) });
try {
  text({ forcedInput: await tools.write_stdin({ session_id: 12345, chars: "printf 'M0 forbidden late input\\n' > stdin-marker\\n" }) });
} catch (error) { text({ forcedInputError: error.message }); }`)];
      },
    });
    const threadId = await p.thread("untrusted");
    const turnId = await p.turn(threadId);
    assert.equal((await p.completed(threadId, turnId)).params.turn.status, "completed");
    assert.equal(p.requests.length, 2);
    const output = cellText(p.requests[1], "input");
    assert.match(output, /exit_code.*0/);
    assert.doesNotMatch(output, /session_id/);
    assert.match(output, /write_stdin is not a function/);
    assert.equal(p.approvals.length, 1);
    assert.equal(await p.marker("stdin-marker"), null);
    assert.deepEqual((await p.rpc("thread/backgroundTerminals/list", { threadId })).data, []);
    t.diagnostic(JSON.stringify({ model, approvals: p.approvals.length, output }));
  });

  test(`${model} managed code-mode dynamic callback uses runtime thread and turn identity`, async (t) => {
    let rootId;
    let accepted = 0;
    const dynamicTools = [{ name: "m0_mark", description: "M0 fixed harmless marker fixture",
      inputSchema: { type: "object", properties: { claimedThreadId: { type: "string" } }, additionalProperties: false } }];
    const result = (success, text) => ({ result: { success, contentItems: [{ type: "inputText", text }] } });
    const p = await Probe.create(t, {
      model, unifiedExec: false, codeMode: true, codeModeOnly: true, codeModeHost: true,
      dynamicTool: async (request, probe) => {
        assert.equal(request.params.tool, "m0_mark");
        assert.equal(request.params.namespace, null);
        assert.ok(request.params.callId.length > 0);
        if (request.params.threadId !== rootId) return result(false, "M0 scope denied");
        await appendFile(path.join(probe.workspace, "dynamic-marker"), "M0 authorized root\n");
        accepted++;
        return result(true, "M0 marker recorded");
      },
      respond: (body) => {
        assertCodeModeSchema(body, model);
        return callOutput(body, "mark") ? [message()] : [exec("mark",
          `text(await tools.m0_mark({ claimedThreadId: ${JSON.stringify(rootId)} }));`)];
      },
    });
    rootId = await p.thread("never", dynamicTools);
    const otherId = await p.thread("never", dynamicTools);
    for (const threadId of [rootId, otherId]) {
      const turnId = await p.turn(threadId);
      assert.equal((await p.completed(threadId, turnId)).params.turn.status, "completed");
      const callback = p.dynamicCalls.find((value) => value.params.threadId === threadId);
      assert.ok(callback, "nested code-mode call reached the app-server dynamic callback");
      assert.equal(callback.params.turnId, turnId);
      assert.equal(callback.params.arguments.claimedThreadId, rootId);
    }
    assert.equal(p.dynamicCalls.length, 2);
    assert.equal(p.requests.length, 4);
    assert.equal(accepted, 1);
    assert.equal(await p.marker("dynamic-marker"), "M0 authorized root\n");
    assert.match(cellText(p.requests[1], "mark"), /M0 marker recorded/);
    assert.match(cellText(p.requests[3], "mark"), /M0 scope denied/);
    const denied = p.messages.find((value) => value.method === "item/completed"
      && value.params.threadId === otherId && value.params.item.type === "dynamicToolCall");
    assert.equal(denied?.params.item.status, "failed");
    t.diagnostic(JSON.stringify({ model, callbacks: p.dynamicCalls.length, accepted, actualOtherThreadDenied: true }));
  });
}
