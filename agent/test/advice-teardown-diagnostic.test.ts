import assert from "node:assert/strict";
import cp from "node:child_process";
import { EventEmitter } from "node:events";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { PassThrough } from "node:stream";
import { it } from "node:test";
import { syncBuiltinESMExports } from "node:module";
import { AdviceTeardownDiagnostic } from "./advice-teardown-diagnostic.js";
import { recordingLogger } from "./helpers.js";

it("advice diagnostic prints before cleanup and rethrows the original assertion without private fields", async (t) => {
  const base = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "advice-diagnostic-"));
  const { logger, lines } = recordingLogger();
  const diagnostic = new AdviceTeardownDiagnostic();
  const original = new assert.AssertionError({ message: "private-assertion-sentinel" });
  const printed: string[] = [];
  t.mock.method(console, "error", (line: string) => printed.push(line));
  try {
    await diagnostic.watch(base, base + "/absent", lines);
    logger.warn("Codex advice data retained: disposal was not confirmed clean", {
      classification: "not_clean", reason: "supervisor_exit_nonzero", state: "drained",
      authority: "ECHILD+__WALL", cleanup_attempted: false, error: "private-error-sentinel",
      config: "private-config-sentinel",
    });
    let cleanup = false;
    await assert.rejects(async () => {
      try {
        await diagnostic.run(async () => { throw original; });
      } finally {
        assert.equal(printed.length, 1);
        assert.equal(cleanup, false);
        await fs.rm(base, { recursive: true, force: true });
        cleanup = true;
      }
    }, (error) => error === original);
    const output = printed[0]!;
    assert.ok(output.startsWith("advice-teardown-diagnostic "));
    assert.equal(output.includes("private-"), false);
    assert.equal(output.includes(base), false);
    const snapshot = JSON.parse(output.slice("advice-teardown-diagnostic ".length));
    assert.equal(snapshot.node_version, process.version);
    assert.equal(snapshot.warnings[0].reason, "supervisor_exit_nonzero");
    assert.equal(snapshot.roots[0].current.existence, "present");
    assert.equal(snapshot.roots[1].current.existence, "absent");
    assert.equal(snapshot.warnings[0].classification, "not_clean");
    assert.equal(snapshot.warnings[0].cleanup_attempted, false);
    await diagnostic.failure(original);
    assert.equal(printed.length, 1);
    assert.throws(() => diagnostic.rethrow(new Error("cleanup failure")), (error) => error === original);
  } finally { await fs.rm(base, { recursive: true, force: true }); }
});

it("advice diagnostic bounds evidence and observes pipe events without reading transport contents", async (t) => {
  const child = new EventEmitter() as import("node:child_process").ChildProcess;
  const stdin = new PassThrough(), stdout = new PassThrough(), stderr = new PassThrough();
  const evidence = new PassThrough();
  Object.assign(child, { stdin, stdout, stderr, stdio: [stdin, stdout, stderr, new PassThrough(), evidence] });
  const spawn = t.mock.method(cp, "spawn", () => child);
  syncBuiltinESMExports();
  const diagnostic = new AdviceTeardownDiagnostic();
  try {
    await diagnostic.run(async () => {
      cp.spawn("/bin/setpriv", ["/fixture/uzi-codex-supervisor"]);
      assert.equal(stdout.listenerCount("data"), 0);
      assert.equal(stderr.listenerCount("data"), 0);
      child.emit("exit", 0, null);
      evidence.write(JSON.stringify({ event: "dispose", state: "drained", authority: "ECHILD+__WALL",
        reason: "private-reason", config: "private-config" }) + "\n");
      for (let i = 0; i < 100; i++) evidence.write('{"event":"snapshot","private":"private-content"}\n');
      evidence.write("private-oversized-" + "x".repeat(9000));
      evidence.write('{"event":"abnormal"}\n');
      const snapshot = await diagnostic.snapshot();
      const events = snapshot.events as Record<string, unknown>[];
      assert.equal(events.length, 64);
      assert.equal(snapshot.truncated, true);
      assert.equal(events[1]?.event, "exit");
      assert.equal(events[2]?.category, "dispose");
      assert.equal(events[2]?.authority, "ECHILD+__WALL");
      assert.equal(JSON.stringify(snapshot).includes("private-"), false);
      assert.equal(stdout.listenerCount("data"), 0);
      assert.equal(stderr.listenerCount("data"), 0);
    });
    assert.equal(evidence.listenerCount("data"), 0);
    assert.equal(child.listenerCount("exit"), 0);
  } finally {
    spawn.mock.restore(); syncBuiltinESMExports();
    for (const stream of child.stdio) stream?.destroy();
  }
});
