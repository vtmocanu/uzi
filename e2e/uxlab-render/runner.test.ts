import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { ChildProcess } from "node:child_process";
import { once } from "node:events";
import { Socket } from "node:net";
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
      if (error.code === "ENOENT" || error.code === "ESRCH") return null;
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

for (const permissionDenied of [false, true]) {
  test(`runner false cleanup distinguishes ${permissionDenied ? "EPERM from absence" : "an absent group"}`, async (t) => {
    let ownedPgid: number | undefined;
    const probes: number[] = [];
    const realKill = process.kill.bind(process);
    t.mock.method(process, "kill", (pid: number, signal?: NodeJS.Signals | number) => {
      assert.equal(signal, 0, "runner may only probe through process.kill");
      probes.push(pid);
      if (permissionDenied) throw Object.assign(new Error("probe denied"), { code: "EPERM" });
      return realKill(pid, signal);
    });
    const opts = { ...options(), killRunnerGroupOnly: (pgid: number) => {
      ownedPgid = pgid;
      return false;
    } };
    const result = runRunner(localRunner, process.execPath, ["-e", 'console.log("done")'], opts);
    if (permissionDenied) {
      await assert.rejects(result, { code: "ERR_RUNNER_GROUP_CLEANUP" });
      assert(!opts.events.some((event) => event.kind === "runner-complete"));
    } else {
      assert.equal((await result).stdout, "done\n");
    }
    assert(ownedPgid !== undefined);
    assert.deepEqual(probes, [-ownedPgid]);
    assert.equal(opts.events.find((event) => event.kind === "runner-cleanup")?.data.groupAbsent,
      !permissionDenied);
  });
}

for (const stream of ["stdout", "stderr"] as const) {
  for (const cleanup of ["returns false", "throws"] as const) {
    for (const endless of [false, true]) {
      test(`runner stops both pipes on ${stream} overflow when cleanup ${cleanup} and output is ${endless ? "endless" : "32 MiB"}`,
        { timeout: 8_000 }, async (t) => {
          const received = new Map<Socket, { bytes: number; largest: number }>();
          const realEmit = Socket.prototype.emit;
          t.mock.method(Socket.prototype, "emit", function (this: Socket, event: string | symbol, ...args: unknown[]) {
            if (event === "data" && Buffer.isBuffer(args[0])) {
              const stats = received.get(this) ?? { bytes: 0, largest: 0 };
              stats.bytes += args[0].length;
              stats.largest = Math.max(stats.largest, args[0].length);
              received.set(this, stats);
            }
            return Reflect.apply(realEmit, this, [event, ...args]);
          });
          const unreferenced: ChildProcess[] = [];
          const realUnref = ChildProcess.prototype.unref;
          t.mock.method(ChildProcess.prototype, "unref", function (this: ChildProcess) {
            unreferenced.push(this);
            return realUnref.call(this);
          });
          let ownedPgid: number | undefined;
          const opts = { ...options(), timeout: 3_000,
            killRunnerGroupOnly: (pgid: number) => {
              ownedPgid = pgid;
              if (cleanup === "throws") throw new Error("signal denied");
              return false;
            } };
          t.after(async () => {
            // Only runRunner's own detached PGID is signalled. Await its parent's
            // exit when still alive; the fixture's self deadline is a second bound.
            const detached = unreferenced[0];
            const exited = detached && detached.exitCode === null && detached.signalCode === null
              ? once(detached, "exit") : undefined;
            if (ownedPgid !== undefined) killRunnerGroupOnly(ownedPgid);
            await exited;
          });
          const fixture = `
            process.stdout.on("error", () => {});
            process.stderr.on("error", () => {});
            process.stdout.write("x");
            process.stderr.write("y");
            setTimeout(() => process.exit(0), 5000);
            setTimeout(() => {
              let sent = 0;
              const chunk = Buffer.alloc(64 * 1024, "z");
              const send = () => {
                // One chunk per turn bounds each attempt; a self deadline bounds
                // endless output even when the regression runs against old code.
                process.${stream}.write(chunk);
                sent += chunk.length;
                if (${endless} || sent < 32 * 1024 * 1024) setImmediate(send);
              };
              send();
            }, 30);
          `;
          const begun = performance.now();
          await assert.rejects(runRunner(localRunner, process.execPath, ["-e", fixture], opts),
            (error: unknown) => {
              const failure = error as { code: string; killed: boolean; stdout: string; stderr: string };
              assert.equal(failure.code, "ERR_CHILD_PROCESS_STDIO_MAXBUFFER");
              assert.equal(failure.killed, false);
              assert.equal(Buffer.byteLength(failure[stream]), 4 * 1024 * 1024);
              return true;
            });
          assert(performance.now() - begun < 1_500, "overflow must settle before the runner deadline");
          assert.equal(received.size, 2, "both child pipes were observed");
          const total = [...received.values()].reduce((sum, stats) => sum + stats.bytes, 0);
          assert(total < 32 * 1024 * 1024, "must stop reading before the full finite payload");
          for (const [pipe, stats] of received) {
            assert(pipe.destroyed, "both child pipes must be destroyed");
            assert(stats.bytes <= 4 * 1024 * 1024 + stats.largest, "only the current overflow chunk may exceed the cap");
          }
          await new Promise((resolve) => setTimeout(resolve, 30));
          assert.equal([...received.values()].reduce((sum, stats) => sum + stats.bytes, 0), total,
            "no further data events after overflow");
          assert.equal(unreferenced.length, 1, "surviving child must be unreferenced");
          const cleanupEvents = opts.events.filter((event) => event.kind === "runner-cleanup");
          assert.equal(cleanupEvents.length, 1);
          assert.equal(cleanupEvents[0]?.data.reason, "overflow");
          assert.equal(cleanupEvents[0]?.data.dispatched, false);
          assert.equal(cleanupEvents[0]?.data.groupAbsent, false);
          assert.equal(cleanupEvents[0]?.data.error, cleanup === "throws" ? "Error: signal denied" : null);
          assert(opts.events.some((event) => event.kind === "runner-error" && event.data.killed === false));
          assert(!opts.events.some((event) => event.kind === "runner-complete"));
        });
    }
  }
}

for (const cleanup of ["returns false", "throws"] as const) {
  for (const parentExits of [true, false]) {
    test(`runner deadline stays bounded when cleanup ${cleanup} and parent ${parentExits ? "exits" : "stays alive"}`,
      { timeout: 5_000 }, async (t) => {
        let ownedPgid: number | undefined;
        const opts = { ...options(), timeout: 1000,
          killRunnerGroupOnly: (pgid: number) => {
            ownedPgid = pgid;
            if (cleanup === "throws") throw new Error("signal denied");
            return false;
          } };
        // Teardown signals only the detached group supplied by runRunner itself.
        // Both fixture processes also exit within two seconds if an assertion fails.
        t.after(() => {
          if (ownedPgid !== undefined) killRunnerGroupOnly(ownedPgid);
        });
        const descendant = 'setTimeout(() => process.exit(0), 1800); console.log("ready");';
        const parent = `
          const child = require("node:child_process").spawn(process.execPath,
            ["-e", process.argv[1]], { stdio: ["ignore", "pipe", "inherit"] });
          child.stdout.on("data", (chunk) => {
            process.stdout.write(chunk);
            if (process.argv[2] === "exit") process.exit(0);
          });
          setTimeout(() => process.exit(0), 2000);
        `;
        const begun = performance.now();
        await assert.rejects(runRunner(localRunner, process.execPath,
          ["-e", parent, descendant, parentExits ? "exit" : "wait"], opts));
        const elapsed = performance.now() - begun;
        assert(elapsed < 1600, `cleanup exceeded completion bound: ${elapsed}ms`);
        const failure = opts.events.find((event) => event.kind === "runner-error");
        assert(failure);
        assert.equal(failure.data.killed, false);
        assert(opts.events.some((event) => event.kind === "runner-cleanup" &&
          event.data.dispatched === false && event.data.groupAbsent === false),
        "failed signal must not be recorded as absent or dispatched");
        assert(!opts.events.some((event) => event.kind === "runner-complete"));
        if (parentExits) {
          assert(opts.events.some((event) => event.kind === "runner-cleanup" &&
            event.data.reason === "exit"), "cleanup attempted before deadline");
        }
      });
  }
}
