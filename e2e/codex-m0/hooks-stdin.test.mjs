import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { Probe, callOutput, message, shellQuote, tool } from "./harness.mjs";

const hookPath = fileURLToPath(new URL("./hook.mjs", import.meta.url));
const stdinMarkerCommand = "printf 'M0 stdin marker\\n' > stdin-marker";
function hookConfig(mode) {
  return (p) => {
    const command = mode === "missing" ? shellQuote(path.join(p.root, "missing-hook-executable"))
      : `${shellQuote(process.execPath)} ${shellQuote(hookPath)} ${shellQuote(mode)} ${shellQuote(path.join(p.root, "hooks.jsonl"))}`;
    return `
[[hooks.PreToolUse]]
matcher = ".*"
[[hooks.PreToolUse.hooks]]
type = "command"
command = ${JSON.stringify(command)}
timeout = 1
`;
  };
}

async function records(p) {
  try {
    return (await readFile(path.join(p.root, "hooks.jsonl"), "utf8")).trim().split("\n").map(JSON.parse);
  } catch (error) { if (error.code === "ENOENT") return []; throw error; }
}

for (const mode of ["allow", "deny", "missing", "malformed", "timeout"]) {
  test(`PreToolUse ${mode} characterization with actual shell marker`, async (t) => {
    const p = await Probe.create(t, {
      hookConfig: hookConfig(mode),
      respond: (body) => callOutput(body, "shell") ? [message()] :
        [tool("shell", "exec_command", {
          cmd: "printf 'M0 marker\\n' > shell-marker", shell: "/bin/sh", login: false,
        })],
    });
    const threadId = await p.thread("never");
    const turnId = await p.turn(threadId);
    const terminal = await p.completed(threadId, turnId);
    assert.equal(terminal.params.turn.status, "completed");
    assert.equal(p.requests.length, 2);
    assert.ok(callOutput(p.requests[1], "shell"));
    assert.equal(p.approvals.length, 0);
    const observed = await records(p);
    assert.deepEqual(observed.map((record) => record.tool), mode === "missing" ? [] : ["Bash"], p.stderr);
    assert.equal(await p.marker(), mode === "deny" ? null : "M0 marker\n");
    if (mode === "deny") assert.match(JSON.stringify(callOutput(p.requests[1], "shell")), /M0 deterministic hook denial/);
    t.diagnostic(JSON.stringify({ mode, hookCalls: observed.length, marker: await p.marker(),
      interpretation: ["missing", "malformed", "timeout"].includes(mode) ? "stock hook failure is fail-open" : "positive control" }));
  });
}

test("stdin marker policy denies the identical command through exec_command", async (t) => {
  const p = await Probe.create(t, {
    hookConfig: hookConfig("deny-marker"),
    respond: (body) => callOutput(body, "direct") ? [message()] :
      [tool("direct", "exec_command", { cmd: stdinMarkerCommand, shell: "/bin/sh", login: false })],
  });
  const threadId = await p.thread("never");
  const turnId = await p.turn(threadId);
  await p.completed(threadId, turnId);
  assert.equal(p.requests.length, 2);
  assert.match(callOutput(p.requests[1], "direct").output, /M0 deterministic hook denial/);
  assert.equal(await p.marker("stdin-marker"), null);
  assert.deepEqual((await records(p)).map((record) => record.tool), ["Bash"]);
});

for (const stdinApproval of [false, true]) {
  test(`PTY write_stdin bypass characterization; flag=${stdinApproval}`, async (t) => {
    const p = await Probe.create(t, {
      stdinApproval, hookConfig: hookConfig("deny-marker"),
      approval: () => ({ decision: "accept" }),
      respond: (body) => {
        if (callOutput(body, "input")) return [message()];
        const startup = callOutput(body, "pty");
        if (startup) {
          const match = /Process running with session ID (\d+)/.exec(startup.output);
          assert.ok(match, "actual PTY output must expose a running process session");
          return [tool("input", "write_stdin", {
            session_id: Number(match[1]),
            chars: `${stdinMarkerCommand}\nexit\n`, yield_time_ms: 1000,
          })];
        }
        return [tool("pty", "exec_command", {
          cmd: "/bin/sh", shell: "/bin/sh", login: false, tty: true, yield_time_ms: 100,
        })];
      },
    });
    const threadId = await p.thread("untrusted");
    const turnId = await p.turn(threadId);
    const terminal = await p.completed(threadId, turnId);
    assert.equal(terminal.params.turn.status, "completed");
    assert.equal(p.requests.length, 3);
    const input = callOutput(p.requests[2], "input");
    assert.ok(input);
    assert.match(input.output, /Process exited with code 0/, "fixture shell exits before cleanup");
    assert.equal(p.approvals.length, 1, "only initial interpreter execution is approved");
    assert.deepEqual((await records(p)).map((record) => record.tool), ["Bash"]);
    assert.equal(await p.marker("stdin-marker"), "M0 stdin marker\n");
    t.diagnostic(JSON.stringify({ stdinApproval, approvals: p.approvals.length,
      hookNames: (await records(p)).map((record) => record.tool), marker: await p.marker("stdin-marker"),
      interpretation: "subsequent matching UseDefault PTY input bypasses both gates" }));
  });
}
