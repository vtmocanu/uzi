import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { Probe, callOutput, message, tool } from "./harness.mjs";

assert.equal(process.env.M0_MANAGED_ONESHOT, "1", "managed tests require the explicit M0_MANAGED_ONESHOT=1 container opt-in");
assert.equal(process.platform, "linux", "managed requirements fixture is container-only; never change host /etc");
assert.equal(
  await readFile("/etc/codex/requirements.toml", "utf8"),
  await readFile(new URL("./requirements.toml", import.meta.url), "utf8"),
  "mount the exact tracked fixture read-only at /etc/codex/requirements.toml",
);

function assertOneShotSchema(body) {
  const named = [];
  const visit = (value) => {
    if (!value || typeof value !== "object") return;
    if (typeof value.name === "string") named.push(value);
    for (const child of Object.values(value)) {
      if (Array.isArray(child)) child.forEach(visit);
      else visit(child);
    }
  };
  visit(body.tools);
  const exec = named.find((value) => value.name === "exec_command");
  assert.ok(exec, "real model request advertises exec_command");
  assert.ok(exec.parameters.properties.timeout_ms, "managed one-shot schema exposes timeout_ms");
  assert.equal(exec.parameters.properties.tty, undefined);
  assert.equal(exec.parameters.properties.yield_time_ms, undefined);
  assert.equal(named.some((value) => value.name === "write_stdin"), false);
}

for (const decision of ["accept", "decline", "malformed"]) {
  test(`managed one-shot shell approval ${decision}`, async (t) => {
    const p = await Probe.create(t, {
      unifiedExec: false, approval: () => ({ decision }),
      respond: (body) => {
        assertOneShotSchema(body);
        return callOutput(body, "marker") ? [message()] : [tool("marker", "exec_command", {
          cmd: "printf 'M0 marker\\n' > shell-marker", shell: "/bin/sh", login: false, timeout_ms: 1000,
        })];
      },
    });
    const threadId = await p.thread("untrusted");
    const turnId = await p.turn(threadId);
    const terminal = await p.completed(threadId, turnId);
    assert.equal(terminal.params.turn.status, "completed");
    assert.equal(p.requests.length, 2);
    assert.equal(p.approvals.length, 1);
    assert.equal(await p.marker(), decision === "accept" ? "M0 marker\n" : null);
    t.diagnostic(JSON.stringify({ decision, approvals: p.approvals.length, marker: await p.marker() }));
  });
}

test("managed one-shot ignores forced tty and rejects forced write_stdin", async (t) => {
  const p = await Probe.create(t, {
    unifiedExec: false, approval: () => ({ decision: "accept" }),
    respond: (body) => {
      assertOneShotSchema(body);
      if (callOutput(body, "input")) return [message()];
      if (callOutput(body, "shell")) return [tool("input", "write_stdin", {
        session_id: 12345, chars: "printf 'M0 forbidden late input\\n' > stdin-marker\n",
      })];
      return [tool("shell", "exec_command", {
        cmd: "/bin/sh", shell: "/bin/sh", login: false, tty: true, yield_time_ms: 1, timeout_ms: 1000,
      })];
    },
  });
  const threadId = await p.thread("untrusted");
  const turnId = await p.turn(threadId);
  const terminal = await p.completed(threadId, turnId);
  assert.equal(terminal.params.turn.status, "completed");
  assert.equal(p.requests.length, 3);
  const shell = callOutput(p.requests[1], "shell");
  assert.match(shell.output, /Process exited with code 0/);
  assert.doesNotMatch(shell.output, /Process running with session ID/);
  const input = callOutput(p.requests[2], "input");
  assert.equal(input.output, "unsupported call: write_stdin", "dispatcher has no write_stdin handler; this is not an unknown-session rejection");
  assert.equal(p.approvals.length, 1);
  assert.equal(await p.marker("stdin-marker"), null);
  assert.equal((await p.rpc("thread/backgroundTerminals/list", { threadId })).data.length, 0);
  t.diagnostic(JSON.stringify({ approvals: p.approvals.length, shell: shell.output, forcedInput: input.output }));
});

for (const timeoutMs of [4000, 150]) {
test(`managed one-shot delayed marker; timeout_ms=${timeoutMs}`, async (t) => {
  const p = await Probe.create(t, {
    unifiedExec: false, approval: () => ({ decision: "accept" }),
    respond: (body) => {
      assertOneShotSchema(body);
      return callOutput(body, "delayed") ? [message()] : [tool("delayed", "exec_command", {
        cmd: "printf 'M0 ready\\n' > child-ready; sleep 2; printf 'M0 late marker\\n' > child-late-marker",
        shell: "/bin/sh", login: false, timeout_ms: timeoutMs,
      })];
    },
  });
  const threadId = await p.thread("untrusted");
  const turnId = await p.turn(threadId);
  const terminal = await p.completed(threadId, turnId);
  assert.equal(terminal.params.turn.status, "completed");
  assert.equal(p.requests.length, 2);
  assert.equal(p.approvals.length, 1);
  const output = callOutput(p.requests[1], "delayed");
  assert.match(output.output, timeoutMs === 150 ? /Process exited with code 124/ : /Process exited with code 0/);
  assert.doesNotMatch(output.output, /Process running with session ID/);
  assert.equal(await p.marker("child-ready"), "M0 ready\n");
  if (timeoutMs === 150) await new Promise((resolve) => setTimeout(resolve, 2500));
  assert.equal(await p.marker("child-late-marker"), timeoutMs === 150 ? null : "M0 late marker\n");
  assert.equal((await p.rpc("thread/backgroundTerminals/list", { threadId })).data.length, 0);
  t.diagnostic(JSON.stringify({ timeoutMs, approvals: p.approvals.length, readyMarker: await p.marker("child-ready"),
    lateMarker: await p.marker("child-late-marker"), output: output.output }));
});
}
