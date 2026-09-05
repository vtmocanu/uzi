import assert from "node:assert/strict";
import test from "node:test";
import { Probe, callOutput, message, patchTool, tool } from "./harness.mjs";

const markerCommand = "printf 'M0 marker\\n' > shell-marker";
// Redirection makes this command fall back to the complete shell argv for
// execpolicy matching. This deliberately rules that wrapper, not every command.
const promptRule = 'prefix_rule(pattern=["/bin/sh"], decision="prompt")';

for (const decision of ["decline", "accept", "malformed"]) {
  test(`ruled shell approval ${decision}`, async (t) => {
    const p = await Probe.create(t, {
      rules: promptRule,
      approval: () => ({ decision }),
      respond: (body) => callOutput(body, "shell") ? [message()] :
        [tool("shell", "exec_command", { cmd: markerCommand, shell: "/bin/sh", login: false })],
    });
    const threadId = await p.thread();
    const turnId = await p.turn(threadId);
    const terminal = await p.completed(threadId, turnId);
    assert.equal(terminal.params.turn.status, "completed");
    assert.equal(p.approvals.length, 1);
    assert.equal(p.approvals[0].method, "item/commandExecution/requestApproval");
    assert.equal(p.requests.length, 2);
    assert.ok(callOutput(p.requests[1], "shell"), "the model receives the actual tool result");
    assert.equal(await p.marker(), decision === "accept" ? "M0 marker\n" : null);
    t.diagnostic(JSON.stringify({ decision, approvals: p.approvals.length, marker: await p.marker() }));
  });
}

for (const policy of ["on-request", "untrusted"]) {
  for (const operation of ["safe-shell", "patch"]) {
    test(`${policy} ${operation} coverage characterization`, async (t) => {
      const p = await Probe.create(t, {
        approval: () => ({ decision: operation === "safe-shell" ? "accept" : "decline" }),
        respond: (body) => callOutput(body, "action") ? [message()] : [operation === "patch"
          ? patchTool("action")
          : tool("action", "exec_command", { cmd: "pwd", shell: "/bin/sh", login: false })],
      });
      const threadId = await p.thread(policy);
      const turnId = await p.turn(threadId);
      const terminal = await p.completed(threadId, turnId);
      assert.equal(terminal.params.turn.status, "completed");
      assert.equal(p.requests.length, 2);
      const output = callOutput(p.requests[1], "action");
      assert.ok(output, "the model receives the action result");
      const expectedApprovals = policy === "untrusted" ? 1 : 0;
      assert.equal(p.approvals.length, expectedApprovals);
      if (operation === "safe-shell") assert.ok(output.output.includes(p.workspace));
      else assert.equal(await p.marker("patch-marker"), policy === "untrusted" ? null : "M0 marker\n");
      t.diagnostic(JSON.stringify({ policy, operation, approvals: p.approvals.length, output }));
    });
  }
}

test("pending ruled approval has no effect before bounded owned shutdown", async (t) => {
  const p = await Probe.create(t, {
    rules: promptRule, approval: () => undefined,
    respond: () => [tool("shell", "exec_command", { cmd: markerCommand, shell: "/bin/sh", login: false })],
  });
  const threadId = await p.thread();
  await p.turn(threadId);
  await p.until(() => p.approvals.length === 1, "pending approval");
  assert.equal(await p.marker(), null);
  assert.equal((await p.rpc("thread/backgroundTerminals/list", { threadId })).data.length, 0);
  // This case intentionally closes an unanswered approval with no terminals.
  // Other unexpected app-server exits make cleanup fail and retain the fixture.
  p.expectedDisconnectedExit = true;
  p.child.stdin.end();
  await p.until(() => p.exited, "disconnected app-server exit", 5000);
  assert.equal(await p.marker(), null);
  assert.equal(p.approvals.length, 1);
  t.diagnostic("Client EOF and owned app-server exit produced no marker; this is not immediate denial evidence.");
});
