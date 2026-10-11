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

it("advice diagnostic uses the test-owned sink before rethrowing, and passing bodies stay silent", async () => {
  const printed: string[] = [];
  const diagnostic = new AdviceTeardownDiagnostic((line) => printed.push(line));
  await diagnostic.run(async () => {});
  assert.equal(printed.length, 0);
  const original = new assert.AssertionError({ message: "controlled failure" });
  await assert.rejects(diagnostic.run(async () => { throw original; }), (error) => {
    assert.equal(printed.length, 1, "snapshot emitted while the failing body is still owned");
    return error === original;
  });
  assert.ok(printed[0]!.startsWith("advice-teardown-diagnostic "));
});

it("advice diagnostic bounds the producer sink and preserves assertions when the sink fails", async (t) => {
  const printed: string[] = [];
  const diagnostic = new AdviceTeardownDiagnostic((line) => printed.push(line));
  t.mock.method(diagnostic, "snapshot", async () => ({ data: "private-" + "é".repeat(20000) }));
  const original = new assert.AssertionError({ message: "controlled failure" });
  await assert.rejects(diagnostic.run(async () => { throw original; }), (error) => error === original);
  assert.deepEqual(printed, ['advice-teardown-diagnostic {"snapshot":"oversized"}']);
  assert.ok(Buffer.byteLength(printed[0]!) <= 16384);
  const broken = new AdviceTeardownDiagnostic(() => { throw new Error("sink failure"); });
  await assert.rejects(broken.run(async () => { throw original; }), (error) => error === original);
});

function fakeSupervisor(t: import("node:test").TestContext) {
  const child = new EventEmitter() as import("node:child_process").ChildProcess;
  const stdin = new PassThrough(), stdout = new PassThrough(), stderr = new PassThrough();
  const evidence = new PassThrough();
  Object.assign(child, { stdin, stdout, stderr, stdio: [stdin, stdout, stderr, new PassThrough(), evidence] });
  const spawn = t.mock.method(cp, "spawn", () => child);
  syncBuiltinESMExports();
  const cleanup = (): void => {
    spawn.mock.restore(); syncBuiltinESMExports();
    for (const stream of child.stdio) stream?.destroy();
  };
  return { child, evidence, cleanup };
}

it("advice diagnostic records each valid phase with its ordinal and ignores unknown phases", () => {
  const diagnostic = new AdviceTeardownDiagnostic();
  const phases = ["mutation_start", "mutation_end", "provider_ready", "provider_not_ready", "dispose_start", "dispose_end"] as const;
  for (const phase of phases) diagnostic.mark(phase);
  assert.doesNotThrow(() => diagnostic.mark("private-unknown-phase" as never));
  const events = (diagnostic as unknown as { events: Record<string, unknown>[] }).events;
  assert.deepEqual(events, phases.map((phase, i) => ({ ordinal: i + 1, event: "phase", phase })));
});

it("advice diagnostic counts phase marks toward the 64 event cap", async () => {
  const diagnostic = new AdviceTeardownDiagnostic();
  for (let i = 0; i < 70; i++) diagnostic.mark("provider_ready");
  const snapshot = await diagnostic.snapshot();
  assert.equal((snapshot.events as unknown[]).length, 64);
  assert.equal(snapshot.truncated, true);
  assert.deepEqual(snapshot.failure_signature, {
    child_exit_before_dispose: "unknown", exit_status_before_dispose: "unknown", warning: "none",
  });
});

it("advice failure_signature is true when a child_exit evidence event precedes dispose_start", async (t) => {
  const { child, evidence, cleanup } = fakeSupervisor(t);
  const diagnostic = new AdviceTeardownDiagnostic();
  try {
    await diagnostic.run(async () => {
      cp.spawn("/bin/setpriv", ["/fixture/uzi-codex-supervisor"]);
      diagnostic.mark("mutation_start");
      evidence.write('{"event":"child_exit","private":"private-content"}\n');
      child.emit("exit", null, "SIGKILL");
      await new Promise((resolve) => setImmediate(resolve));
      diagnostic.mark("dispose_start");
      const snapshot = await diagnostic.snapshot();
      assert.deepEqual(snapshot.failure_signature, {
        child_exit_before_dispose: true, exit_status_before_dispose: "signalled", warning: "none",
      });
      assert.equal(JSON.stringify(snapshot).includes("private-"), false);
    });
  } finally { cleanup(); }
});

it("advice failure_signature is false when child_exit only arrives after dispose_start", async (t) => {
  const { child, evidence, cleanup } = fakeSupervisor(t);
  const diagnostic = new AdviceTeardownDiagnostic();
  try {
    await diagnostic.run(async () => {
      cp.spawn("/bin/setpriv", ["/fixture/uzi-codex-supervisor"]);
      diagnostic.mark("dispose_start");
      evidence.write('{"event":"child_exit"}\n');
      child.emit("exit", 0, null);
      await new Promise((resolve) => setImmediate(resolve));
      const snapshot = await diagnostic.snapshot();
      assert.deepEqual(snapshot.failure_signature, {
        child_exit_before_dispose: false, exit_status_before_dispose: "none", warning: "none",
      });
    });
  } finally { cleanup(); }
});

it("advice providerChildExitObserved is false without child_exit evidence", async (t) => {
  const { evidence, cleanup } = fakeSupervisor(t);
  const diagnostic = new AdviceTeardownDiagnostic();
  try {
    await diagnostic.run(async () => {
      cp.spawn("/bin/setpriv", ["/fixture/uzi-codex-supervisor"]);
      evidence.write('{"event":"started"}\n');
      await new Promise((resolve) => setImmediate(resolve));
      assert.equal(diagnostic.providerChildExitObserved(), false);
    });
  } finally { cleanup(); }
});

it("advice providerChildExitObserved is true when child_exit precedes dispose_start", async (t) => {
  const { evidence, cleanup } = fakeSupervisor(t);
  const diagnostic = new AdviceTeardownDiagnostic();
  try {
    await diagnostic.run(async () => {
      cp.spawn("/bin/setpriv", ["/fixture/uzi-codex-supervisor"]);
      evidence.write('{"event":"child_exit"}\n');
      await new Promise((resolve) => setImmediate(resolve));
      diagnostic.mark("dispose_start");
      assert.equal(diagnostic.providerChildExitObserved(), true);
    });
  } finally { cleanup(); }
});

it("advice providerChildExitObserved is true when child_exit follows dispose_start", async (t) => {
  const { evidence, cleanup } = fakeSupervisor(t);
  const diagnostic = new AdviceTeardownDiagnostic();
  try {
    await diagnostic.run(async () => {
      cp.spawn("/bin/setpriv", ["/fixture/uzi-codex-supervisor"]);
      diagnostic.mark("dispose_start");
      evidence.write('{"event":"child_exit"}\n');
      await new Promise((resolve) => setImmediate(resolve));
      assert.equal(diagnostic.providerChildExitObserved(), true);
    });
  } finally { cleanup(); }
});

it("advice failure_signature is unknown without dispose_start and reports the first warning category", async () => {
  const { logger, lines } = recordingLogger();
  const diagnostic = new AdviceTeardownDiagnostic();
  await diagnostic.watch("/nonexistent/advice-data", "/nonexistent/advice-cwd", lines);
  logger.warn("Codex advice data cleanup failed", { error: "private-error" });
  diagnostic.mark("mutation_start");
  const snapshot = await diagnostic.snapshot();
  assert.deepEqual(snapshot.failure_signature, {
    child_exit_before_dispose: "unknown", exit_status_before_dispose: "unknown", warning: "cleanup_failed",
  });
  assert.equal(JSON.stringify(snapshot.failure_signature).includes("private-"), false);
});
