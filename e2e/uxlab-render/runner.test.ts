import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { runnerCommand } from "../../agent/src/runner-uid.js";
import { runRunner } from "./runner.js";

const localRunner = (command: string, args: string[]) => runnerCommand(command, args, false);
// runRunner supplies only the pgid of its own detached child; never fall back to pid.
const killRunnerGroupOnly = (pgid: number) => {
  try { process.kill(-pgid, "SIGKILL"); return true; }
  catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ESRCH") return false;
    throw error;
  }
};
function receipts() {
  const events: { kind: string; data: Record<string, unknown> }[] = [];
  return { events, record: (kind: string, data: Record<string, unknown>) => {
    events.push({ kind, data });
  } };
}
const options = () => ({ cwd: process.cwd(), env: { PATH: process.env.PATH },
  timeout: 2_000, killRunnerGroupOnly, ...receipts() });

test("runner success retains output and records launch before completion", async () => {
  const opts = options();
  const output = await runRunner(localRunner, process.execPath,
    ["-e", 'console.log("ok"); console.error("warning")'], opts);
  assert.equal(output.stdout, "ok\n");
  assert.equal(output.stderr, "warning\n");
  assert.deepEqual(opts.events.map((event) => event.kind), ["runner-launch", "runner-cleanup", "runner-complete"]);
  assert.equal(opts.events[0]?.data.timeoutMs, 2_000);
});

test("runner launch failure reports bounded fields and preserves original error", async () => {
  const opts = options();
  await assert.rejects(runRunner(() => ({ command: "/nonexistent/uxlab-runner", args: [] }),
    "unused", [], opts), { code: "ENOENT" });
  const failure = opts.events.find((event) => event.kind === "runner-error")?.data;
  assert.equal(failure?.stage, "exec");
  assert.equal(failure?.code, "ENOENT");
  assert.match(String(failure?.syscall), /spawn/);
  assert.equal(failure?.killed, false);
  assert.equal(failure?.signal, null);
  assert.equal(failure?.stdout, "");
  assert.equal(failure?.stderr, "");
});

test("runner error output is bounded and wrap failures identify their stage", async () => {
  const opts = options();
  await assert.rejects(runRunner(localRunner, process.execPath,
    ["-e", 'process.stdout.write("x".repeat(9000)); process.stderr.write("y".repeat(9000)); process.exitCode = 7'],
    opts), { code: 7 });
  const failure = opts.events.find((event) => event.kind === "runner-error")?.data;
  assert.equal(String(failure?.stdout).length, 4096);
  assert.equal(String(failure?.stderr).length, 4096);
  assert.equal(failure?.code, "7");
  const wrap = options(), error = new Error("wrap failed");
  await assert.rejects(runRunner(() => { throw error; }, "unused", [], wrap),
    (actual) => actual === error);
  assert.equal(wrap.events[1]?.data.stage, "wrap");
});

test("runner retains exactly 4 MiB on each output stream", async () => {
  const opts = { ...options(), timeout: 5_000 };
  const output = await runRunner(localRunner, process.execPath, ["-e", `
    process.stdout.write(Buffer.alloc(4 * 1024 * 1024, "x"));
    process.stderr.write(Buffer.alloc(4 * 1024 * 1024, "y"));
  `], opts);
  assert.equal(output.stdout.length, 4 * 1024 * 1024);
  assert.equal(output.stderr.length, 4 * 1024 * 1024);
  assert.equal(output.stdout.slice(-1), "x");
  assert.equal(output.stderr.slice(-1), "y");
});

for (const stream of ["stdout", "stderr"] as const) {
  test(`runner bounds ${stream} by bytes and kills its group on overflow`, async () => {
    const opts = { ...options(), timeout: 5_000 };
    await assert.rejects(runRunner(localRunner, process.execPath, ["-e", `
      process.${stream}.write("€".repeat(2 * 1024 * 1024));
      setInterval(() => {}, 1000);
    `], opts), (error: unknown) => {
      const failure = error as { code: string; stdout: string; stderr: string };
      assert.equal(failure.code, "ERR_CHILD_PROCESS_STDIO_MAXBUFFER");
      // UTF-8 decoding may replace one final split character; the retained raw
      // byte count is 4 MiB, not 4 MiB of JavaScript characters.
      assert(failure[stream].length <= Math.ceil(4 * 1024 * 1024 / 3));
      return true;
    });
    assert(opts.events.some((event) => event.kind === "runner-cleanup" &&
      event.data.reason === "overflow" && event.data.dispatched === true));
    const failure = opts.events.find((event) => event.kind === "runner-error")?.data;
    assert(Buffer.byteLength(String(failure?.[stream])) <= 4096);
  });
}

for (const mode of ["accepts TERM", "resists TERM", "normal exit"] as const) {
  test(`runner cleans TERM-resistant child when parent ${mode}`, { timeout: 25_000 }, async (t) => {
    const dir = await fs.mkdtemp(fileURLToPath(new URL("../../.uzi/scratch/uxlab-runner-", import.meta.url)));
    const heartbeat = dir + "/heartbeat";
    const child = `
      const fs = require("node:fs");
      process.on("SIGTERM", () => {});
      fs.writeFileSync(process.argv[1], "ready");
      setInterval(() => fs.appendFileSync(process.argv[1], "."), 25);
      // Final self deadline also bounds leaks when exercising the old implementation.
      setTimeout(() => process.exit(0), 15000);
      console.error("child-ready");
      console.log("CHILD=" + process.pid);
    `;
    const parent = `
      if (process.argv[3] === "resists TERM") process.on("SIGTERM", () => {});
      if (process.argv[3] === "accepts TERM") process.on("SIGTERM", () => process.exit(0));
      const child = require("node:child_process").spawn(process.execPath,
        ["-e", process.argv[1], process.argv[2]],
        { stdio: ["ignore", "pipe", "inherit"] });
      child.stdout.on("data", (chunk) => {
        process.stdout.write(chunk);
        if (process.argv[3] === "normal exit") process.exit(0);
      });
      setTimeout(() => process.exit(0), 15000);
    `;
    const opts = { ...options(), timeout: 1_000 };
    // On old behavior the child may survive the assertion. Wait only until its
    // finite self deadline before deleting its heartbeat directory; never signal
    // a group whose id runRunner did not supply to our callback.
    const begun = performance.now();
    let verified = false;
    t.after(async () => {
      if (!verified) {
        const remaining = Math.max(0, 15_500 - (performance.now() - begun));
        await new Promise((resolve) => setTimeout(resolve, remaining));
      }
      await fs.rm(dir, { recursive: true, force: true });
    });
    let output: { stdout: string; stderr: string };
    if (mode === "normal exit") {
      output = await runRunner(localRunner, process.execPath, ["-e", parent, child, heartbeat, mode], opts);
    } else {
      await assert.rejects(runRunner(localRunner, process.execPath,
        ["-e", parent, child, heartbeat, mode], opts));
      const failure = opts.events.find((event) => event.kind === "runner-error")?.data;
      output = { stdout: String(failure?.stdout), stderr: String(failure?.stderr) };
    }
    const elapsed = performance.now() - begun;
    assert.match(output.stderr, /child-ready/);
    const pid = /CHILD=(\d+)/.exec(output.stdout)?.[1];
    assert(pid, "descendant started");
    const before = await fs.readFile(heartbeat, "utf8");
    await new Promise((resolve) => setTimeout(resolve, 150));
    assert.equal(await fs.readFile(heartbeat, "utf8"), before, "descendant still writes after runner returns");
    const stat = await fs.readFile(`/proc/${pid}/stat`, "utf8").catch((error) => {
      if (error.code === "ENOENT") return null;
      throw error;
    });
    assert(stat === null || /^\d+ \(.+\) Z /.test(stat), "descendant must be gone or a stopped zombie");
    assert(elapsed < 2_000, `unexpected deadline: ${elapsed}ms`);
    if (mode !== "normal exit") assert(elapsed >= 1_000);
    assert(opts.events.some((event) => event.kind === "runner-cleanup" &&
      event.data.reason === (mode === "normal exit" ? "exit" : "deadline") &&
      event.data.dispatched === true), "known detached group was signalled");
    verified = true;
  });
}
