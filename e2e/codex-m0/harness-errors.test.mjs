import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { PassThrough } from "node:stream";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { Probe, deadline, poll } from "./harness.mjs";

for (const failure of ["group signal", "final exit wait", "version signal", "version exit wait"]) {
  test(`cleanup continues after ${failure} failure and retains evidence`, async (t) => {
    const root = await mkdtemp(path.join(tmpdir(), "cdr-cleanup-error-"));
    const p = new Probe(root);
    const first = new Error(`injected ${failure} failure`);
    const input = new PassThrough();
    p.lines = createInterface({ input });
    let closed = false;
    p.lines.once("close", () => { closed = true; });
    p.server = createServer((_, response) => response.end("owned fixture"));
    p.server.listen(0, "127.0.0.1");
    await once(p.server, "listening");
    const signals = [];
    let versionKilled = false;
    // No real process uses this synthetic identity. The mock refuses any other
    // target, and test-finally owns the real listener/readline even on a red.
    t.mock.method(process, "kill", (pid, signal) => {
      assert.equal(pid, -123);
      signals.push(signal);
      if (failure === "group signal") throw first;
    });
    const exitError = failure === "final exit wait" ? first : new Error("injected graceful wait failure");
    p.exit = Promise.reject(exitError);
    p.exit.catch(() => {});
    if (failure.startsWith("group") || failure === "final exit wait") {
      p.child = { pid: 123, stdin: new PassThrough() };
    }
    p.versionChild = { exitCode: null, signalCode: null, kill(signal) {
      assert.equal(signal, "SIGKILL");
      versionKilled = true;
      if (failure === "version signal") throw first;
    } };
    p.versionExit = failure === "version exit wait" ? Promise.reject(first) : Promise.resolve();
    p.versionExit.catch(() => {});
    await writeFile(path.join(root, "evidence"), "keep this fixture\n");
    try {
      const error = await p.close().then(() => assert.fail("incomplete cleanup was reported as success"), (error) => error);
      assert.equal(closed, true, "readline closes despite earlier cleanup failure");
      assert.equal(p.server.listening, false, "fake server closes despite earlier cleanup failure");
      assert.equal(versionKilled, true, "version child cleanup is attempted independently");
      if (p.child) assert.deepEqual(signals, ["SIGTERM", "SIGKILL"], "remaining owned signal cleanup is attempted");
      assert.equal(error.message, `M0 cleanup failed; retained ${root}`);
      assert.equal(error.cause, first, "the first terminal cleanup failure remains the cause");
      assert.equal(await readFile(path.join(root, "evidence"), "utf8"), "keep this fixture\n");
    } finally {
      p.lines.close();
      input.destroy();
      p.child?.stdin.destroy();
      p.server.closeAllConnections();
      if (p.server.listening) await new Promise((resolve) => p.server.close(resolve));
      await rm(root, { recursive: true, force: true });
    }
  });
}

test("supervisor callback send failure reaches dispose without an unhandled derived rejection", async () => {
  // Isolate Node's strict unhandled-rejection behavior from the test runner.
  // This reaches the real base callback catch path: both reply writes throw.
  const source = `
import assert from "node:assert/strict";
import { SupervisorProbe } from ${JSON.stringify(new URL("./supervisor-probe.mjs", import.meta.url).href)};
const p = new SupervisorProbe("unused-fixture-root");
p.callbackReplies = new Set();
p.child = { stdin: { destroyed: false, writableEnded: false } };
const callback = Promise.withResolvers();
p.dynamicTool = () => callback.promise;
const original = new Error("injected callback error-response send failure");
let sends = 0;
p.send = () => { sends++; throw original; };
p.releaseCallback = () => callback.resolve({ result: {} });
p.supervisorCommand = () => assert.fail("rejected callback must not authorize drain");
const answer = p.answerDynamic({ id: 1 });
const rejectedAnswer = assert.rejects(answer, (error) => error === original);
await assert.rejects(p.dispose(), (error) => error === original);
await rejectedAnswer;
assert.equal(sends, 2);
assert.deepEqual(p.errors, [original.message]);
assert.equal(p.callbackReplies.size, 0);
assert.equal(p.drained, undefined);
await new Promise((resolve) => setImmediate(resolve));
console.log("original callback and dispose rejection preserved");
`;
  const child = spawn(process.execPath, ["--unhandled-rejections=strict", "--input-type=module", "-e", source],
    { env: { PATH: process.env.PATH }, stdio: ["ignore", "pipe", "pipe"] });
  let stdout = "", stderr = "";
  child.stdout.on("data", (chunk) => { stdout += chunk; });
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  const exit = once(child, "close");
  try {
    const [code, signal] = await deadline(exit, "strict callback rejection child", 5000);
    assert.equal(code, 0, stderr);
    assert.equal(signal, null);
    assert.match(stdout, /original callback and dispose rejection preserved/);
    assert.equal(stderr, "");
  } finally {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    await exit;
  }
});

for (const target of ["missing", "non-executable", "working"]) {
  test(`supervisor ${target} exec target stays in child branch and is reaped`, async () => {
    assert.equal(process.platform, "linux", "supervisor startup requires the isolated Linux profile");
    const root = await mkdtemp(path.join(tmpdir(), "cdr-supervisor-startup-"));
    const executable = target === "working" ? "/bin/sh" : path.join(root, "target");
    if (target === "non-executable") await writeFile(executable, "#!/bin/sh\nexit 0\n", { mode: 0o600 });
    const child = spawn("/usr/bin/python3", [fileURLToPath(new URL("./supervisor.py", import.meta.url)),
      executable, "-c", "exit 0"], { stdio: ["ignore", "ignore", "pipe", "pipe", "pipe"] });
    let stderr = "";
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    const messages = [];
    const lines = createInterface({ input: child.stdio[4] });
    lines.on("line", (line) => messages.push(JSON.parse(line)));
    const exit = once(child, "close");
    try {
      const started = await poll(() => messages.find((value) => value.event === "started"), "parent started evidence");
      assert.equal(started.pid, child.pid);
      assert.equal(started.uid, 10002);
      const stat = await poll(async () => {
        const value = (await readFile(`/proc/${started.appServerPid}/stat`, "utf8")).split(") ")[1].split(" ");
        return value[0] === "Z" && value;
      }, "owned exec child reached zombie state");
      assert.equal(Number(stat[1]), child.pid, "parent retains ownership before reaping");
      assert.equal(Number(stat[49]) >> 8, target === "working" ? 0 : 127, "exec failure has deterministic child exit 127");
      child.stdio[3].write(`${JSON.stringify({ id: 1, op: "dispose", noSignal: true, timeoutMs: 1000 })}\n`);
      const disposed = await poll(() => messages.find((value) => value.id === 1), "parent disposal evidence");
      assert.equal(disposed.state, "drained");
      assert.deepEqual(disposed.reaped, [started.appServerPid]);
      assert.deepEqual(await deadline(exit, "normal supervisor exit", 2000), [0, null]);
      assert.deepEqual(messages.map((value) => value.event), ["started", "dispose"]);
      if (target === "working") assert.equal(stderr, "");
      else assert.match(stderr, /^M0 child startup failed \(errno \d+\)\n$/);
    } finally {
      // EOF asks this exact owned helper to drain even when an assertion failed.
      child.stdio[3].end();
      try { await deadline(exit, "test-owned supervisor cleanup", 3000); }
      finally {
        lines.close();
        if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
        await exit;
        await rm(root, { recursive: true, force: true });
      }
    }
  });
}
