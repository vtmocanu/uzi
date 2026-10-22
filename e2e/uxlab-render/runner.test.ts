import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs/promises";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { runnerCommand } from "../../agent/src/runner-uid.js";
import { runRunner } from "./runner.js";

// The Alpine test host's /usr/bin/timeout may be BusyBox. An explicit test-only
// override exercises installed GNU timeout without modifying the host or command env.
const timeoutBinary = process.env.UXLAB_TEST_TIMEOUT_BINARY ?? "/usr/bin/timeout";
const localRunner = (command: string, args: string[]) => {
  assert.equal(command, "/usr/bin/timeout");
  assert.equal(args[0], "--kill-after=5s");
  assert(!args.includes("--foreground"));
  return runnerCommand(timeoutBinary, args, false);
};
function receipts() {
  const events: { kind: string; data: Record<string, unknown> }[] = [];
  return { events, record: (kind: string, data: Record<string, unknown>) => {
    events.push({ kind, data });
  } };
}
const options = () => ({ cwd: process.cwd(), env: { PATH: process.env.PATH },
  timeout: 2_000, ...receipts() });

test("runner success retains output and records launch before completion", async () => {
  const probe = spawnSync(timeoutBinary, ["--version"], { encoding: "utf8", timeout: 2000 });
  assert.ifError(probe.error);
  assert.equal(probe.status, 0, "tests require GNU timeout; set UXLAB_TEST_TIMEOUT_BINARY if needed");
  assert.match(probe.stdout, /GNU coreutils/);
  const opts = options();
  const output = await runRunner(localRunner, process.execPath,
    ["-e", 'console.log("ok"); console.error("warning")'], opts);
  assert.equal(output.stdout, "ok\n");
  assert.equal(output.stderr, "warning\n");
  assert.deepEqual(opts.events.map((event) => event.kind), ["runner-launch", "runner-complete"]);
  assert.equal(opts.events[0]?.data.outerTimeoutMs, 12_000);
});

test("runner launch failure reports bounded fields and preserves original error", async () => {
  const opts = options();
  await assert.rejects(runRunner(() => ({ command: "/nonexistent/uxlab-runner", args: [] }),
    "unused", [], opts), { code: "ENOENT" });
  const failure = opts.events[1]?.data;
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
  const failure = opts.events[1]?.data;
  assert.equal(String(failure?.stdout).length, 4096);
  assert.equal(String(failure?.stderr).length, 4096);
  assert.equal(failure?.code, "7");
  const wrap = options(), error = new Error("wrap failed");
  await assert.rejects(runRunner(() => { throw error; }, "unused", [], wrap),
    (actual) => actual === error);
  assert.equal(wrap.events[1]?.data.stage, "wrap");
});

test("runner timeout kills a TERM-resistant descendant within deadline plus grace", async (t) => {
  const dir = await fs.mkdtemp(fileURLToPath(new URL("../../.uzi/scratch/uxlab-runner-", import.meta.url)));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const heartbeat = fileURLToPath(new URL("heartbeat", "file://" + dir + "/"));
  const child = `
    const fs = require("node:fs");
    process.on("SIGTERM", () => {});
    console.log("CHILD=" + process.pid);
    console.error("child-ready");
    fs.writeFileSync(process.argv[1], "ready");
    setInterval(() => fs.appendFileSync(process.argv[1], "."), 25);
    setTimeout(() => process.exit(0), 20000);
  `;
  // Both processes resist TERM, requiring timeout's finite KILL escalation.
  // The child has a 20s self deadline too; a failed assertion cannot leave it forever.
  const parent = `
    process.on("SIGTERM", () => {});
    require("node:child_process").spawn(process.execPath, ["-e", process.argv[1], process.argv[2]],
      { stdio: ["ignore", "inherit", "inherit"] });
    setTimeout(() => process.exit(0), 20000);
  `;
  const opts = { ...options(), timeout: 1_000 };
  const begun = performance.now();
  await assert.rejects(runRunner(localRunner, process.execPath,
    ["-e", parent, child, heartbeat], opts));
  const elapsed = performance.now() - begun;
  const failure = opts.events[1]?.data;
  assert.equal(failure?.signal, "SIGKILL");
  assert.equal(failure?.killed, false, "inner timeout must finish before execFile's fallback");
  assert.match(String(failure?.stderr), /child-ready/);
  const pid = /CHILD=(\d+)/.exec(String(failure?.stdout))?.[1];
  assert(pid, "descendant started");
  assert(elapsed >= 5_000 && elapsed < 10_000, `unexpected deadline: ${elapsed}ms`);
  const before = await fs.readFile(heartbeat, "utf8");
  // One bounded observation after exit: no retry and no sibling process signalling.
  await new Promise((resolve) => setTimeout(resolve, 150));
  assert.equal(await fs.readFile(heartbeat, "utf8"), before, "descendant still writes after timeout");
  const stat = await fs.readFile(`/proc/${pid}/stat`, "utf8").catch((error) => {
    if (error.code === "ENOENT") return null;
    throw error;
  });
  assert(stat === null || /^\d+ \(.+\) Z /.test(stat), "descendant must be gone or a stopped zombie");
});
