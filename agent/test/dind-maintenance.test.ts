import { after, describe, it, type TestContext } from "node:test";
import childProcess, { type ExecFileOptionsWithStringEncoding, type ExecFileException } from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import assert from "node:assert/strict";
import { statSync, readdirSync, rmSync } from "node:fs";
import { DindMaintenanceController } from "../src/dind-maintenance.js";
import { DindPruneController, DindPruneGate, assertDockerArgvAllowed, execDocker, type DockerExec } from "../src/dind-prune.js";
import type { DindMaintenance } from "../src/protocol.js";
import type { DindMeterSample } from "../src/dind-meter.js";
import { recordingLogger } from "./helpers.js";

const configDirs = new Set<string>();
after(() => {
  for (const dir of configDirs) rmSync(dir, { recursive: true, force: true });
});

const operation: DindMaintenance = {
  id: "op", nonce: "op-nonce", deployment_uid: "deployment", pvc_uid: "pvc",
  register_nonce: "registration", phase: "ready", fenced: true, ready_ack: false,
};

function harness() {
  let time = 1_700_000_000_000;
  const gate = new DindPruneGate();
  const { logger } = recordingLogger();
  const calls: { argv: readonly string[]; opts: Parameters<DockerExec>[1] }[] = [];
  const state = {
    idle: true, version: "1.42", missing: false, frozen: undefined as number | undefined,
    used: 90, threshold: 0.85 as number | undefined,
    exec: undefined as DockerExec | undefined,
    sleeping: undefined as (() => void) | undefined,
  };
  const sample = (): DindMeterSample | null => state.missing ? null : ({
    epochS: state.frozen ?? Math.floor(time / 1000), bytesUsed: state.used, bytesTotal: 100,
    inodesUsed: state.used, inodesTotal: 100,
  });
  const exec: DockerExec = async (argv, opts) => {
    if (opts.env.DOCKER_CONFIG) configDirs.add(opts.env.DOCKER_CONFIG);
    calls.push({ argv: [...argv], opts });
    return state.exec ? state.exec(argv, opts) : argv[0] === "version" ? state.version : "";
  };
  const sleep = async (ms: number, signal?: AbortSignal) => {
    if (signal?.aborted) throw new Error("aborted");
    time += ms;
    state.sleeping?.();
  };
  const ctl = new DindMaintenanceController({
    gate, dockerHost: "tcp://dind:2375", heartbeatIntervalMs: 15_000,
    isIdle: () => state.idle, log: logger, threshold: () => state.threshold,
    readSample: sample, exec, now: () => time, sleep,
  });
  ctl.register("registration", true);
  const observe = (op: DindMaintenance | null | undefined = operation, retaining: boolean | undefined = false, stamp = time) =>
    ctl.observe(op, retaining, stamp);
  observe();
  time += 1000;
  observe();
  return { ctl, gate, state, calls, sample, exec, sleep, logger, now: () => time,
    advance: (ms: number) => { time += ms; }, observe };
}

// Capture only children launched by this test, delegating to Node's real transport.
function ownedExec(t: TestContext) {
  const original = childProcess.execFile;
  let child: ReturnType<typeof original> | undefined;
  let closed = false;
  let abortCallbackBeforeClose = false;
  let started!: () => void;
  let close!: () => void;
  const spawned = new Promise<void>((resolve) => { started = resolve; });
  const closure = new Promise<void>((resolve) => { close = resolve; });
  const spy = t.mock.method(childProcess, "execFile", (
    file: string, argv: string[], opts: ExecFileOptionsWithStringEncoding,
    callback: (err: ExecFileException | null, stdout: string, stderr: string) => void,
  ) => {
    assert.equal(child, undefined, "each test owns exactly one child");
    child = original(file, argv, opts, (err, stdout, stderr) => {
      if (err?.name === "AbortError") abortCallbackBeforeClose = !closed;
      callback(err, stdout, stderr);
    });
    child.once("spawn", started);
    child.once("close", () => { closed = true; close(); });
    return child;
  });
  syncBuiltinESMExports();
  return {
    spawned, closure, closed: () => closed,
    abortCallbackBeforeClose: () => abortCallbackBeforeClose,
    async cleanup() {
      try {
        if (child) {
          if (!closed) child.kill("SIGKILL");
          await closure;
        }
      } finally {
        spy.mock.restore();
        syncBuiltinESMExports();
      }
    },
  };
}

const INERT_CHILD_ARGV = ["-e", "setInterval(() => {}, 1000)"];

describe("server-fenced maintenance", () => {
  for (const command of ["version", "prune"]) {
    it(`real abort during ${command} holds claims and tick until owned child close`, async (t) => {
      const owned = ownedExec(t);
      const h = harness();
      let helperClosed: boolean | undefined;
      h.state.exec = (argv, opts) => {
        if (command === "prune" && argv[0] === "version") return Promise.resolve("1.42");
        return execDocker(INERT_CHILD_ARGV, { ...opts, file: process.execPath, timeoutMs: 2000 })
          .catch((err: unknown) => { helperClosed = owned.closed(); throw err; });
      };
      let tickClosed: boolean | undefined;
      const work = h.ctl.tick().then(() => { tickClosed = owned.closed(); });
      try {
        await owned.spawned;
        h.observe({ ...operation, phase: "cancelled" });
        assert.equal(owned.closed(), false);
        assert.equal(h.gate.tryEnterClaim(), false);
        assert.equal(h.ctl.acknowledgement(), undefined);
        await work;
        assert.equal(owned.abortCallbackBeforeClose(), true, "exercise Node's early abort callback");
        assert.equal(helperClosed, true, "helper must reject only after child close");
        assert.equal(tickClosed, true, "maintenance must finish only after child close");
        assert.equal(h.gate.claimsClosed(), false);
      } finally {
        await owned.cleanup();
        await work;
      }
    });
  }

  it("real short command timeout closes owned child before rejection and permits fresh ready ACK", async (t) => {
    const owned = ownedExec(t);
    const h = harness();
    let helperClosed: boolean | undefined;
    let failure: unknown;
    h.state.exec = (argv, opts) => {
      if (argv[0] === "version") return Promise.resolve("1.42");
      return execDocker(INERT_CHILD_ARGV, { ...opts, file: process.execPath, timeoutMs: 25 })
        .catch((err: unknown) => {
          helperClosed = owned.closed();
          failure = err;
          throw err;
        });
    };
    const start = performance.now();
    const work = h.ctl.tick();
    try {
      await work;
      assert.equal(helperClosed, true, "timeout rejection must follow child close");
      assert.ok(failure instanceof Error);
      assert.equal((failure as ExecFileException).signal, "SIGKILL");
      assert.ok(performance.now() - start < 2000, "short timeout must return within two seconds");
      h.advance(1);
      h.observe();
      assert.equal(h.ctl.acknowledgement()?.phase, "ready");
      assert.equal(h.ctl.acknowledgement()?.pruned, false);
      assert.equal(h.gate.claimsClosed(), true);
    } finally {
      await owned.cleanup();
      await work;
    }
  });

  it("requested drain allows resumes; ready fences every local claim lane", async () => {
    const h = harness();
    h.observe({ ...operation, phase: "cancelled" });
    h.observe({ ...operation, phase: "requested", fenced: false });
    assert.equal(h.gate.tryEnterClaim(), true);
    h.observe(operation);
    assert.equal(h.gate.tryEnterClaim(), false);
    await h.ctl.tick();
    assert.equal(h.calls.length, 0);
    h.gate.exitClaim();
    h.advance(1);
    h.observe();
    await h.ctl.tick();
    assert.ok(h.ctl.acknowledgement());
  });

  for (const busy of ["execution", "registry", "live-wait"]) {
    it(`blocks prune and ACK while ${busy} makes isIdle false`, async () => {
      const h = harness();
      h.state.idle = false;
      await h.ctl.tick();
      assert.equal(h.calls.length, 0);
      assert.equal(h.ctl.acknowledgement(), undefined);
      h.state.idle = true;
      await h.ctl.tick();
      assert.equal(h.calls.length, 0);
      h.advance(1);
      h.observe();
      await h.ctl.tick();
      assert.ok(h.ctl.acknowledgement());
    });
  }

  it("unknown, null, malformed, stale binding and custody fail closed", async () => {
    const h = harness();
    for (const op of [undefined, null, { ...operation, id: "" }, { ...operation, register_nonce: "old" }]) {
      h.ctl.observe(op, false, h.now());
      await h.ctl.tick();
      assert.equal(h.calls.length, 0);
      assert.equal(h.ctl.acknowledgement(), undefined);
      assert.equal(h.gate.claimsClosed(), true);
    }
    for (const retaining of [true, undefined]) {
      h.ctl.observe(operation, retaining, h.now());
      await h.ctl.tick();
      assert.equal(h.calls.length, 0);
    }
    h.observe();
    h.advance(30_001);
    await h.ctl.tick();
    assert.equal(h.calls.length, 0);
    h.observe(operation, false, h.now() + 1);
    await h.ctl.tick();
    assert.equal(h.calls.length, 0);
    h.ctl.noteActivityEnded();
    h.observe();
    await h.ctl.tick();
    assert.equal(h.calls.length, 0);
    h.advance(1);
    h.observe();
    await h.ctl.tick();
    assert.ok(h.ctl.acknowledgement());
  });

  it("cache finally cannot open maintenance; cache-owned attempt serializes commands", async () => {
    const h = harness();
    h.gate.close();
    await h.ctl.tick();
    assert.equal(h.calls.length, 0);
    h.gate.open();
    assert.equal(h.gate.tryEnterClaim(), false);
    await h.ctl.tick();
    assert.deepEqual(h.calls.map((c) => c.argv), [
      ["version", "--format", "{{.Server.APIVersion}}"], ["volume", "prune", "-f"],
    ]);
    const cache = new DindPruneController({
      gate: h.gate, dockerHost: "tcp://dind:2375", heartbeatIntervalMs: 15_000,
      workerName: "worker", isIdle: () => true, log: h.logger,
      readSample: h.sample, exec: h.exec, now: h.now, sleep: h.sleep,
    });
    h.advance(30_000);
    cache.recordCustody(false, h.now());
    await cache.tick();
    h.advance(30_000);
    cache.recordCustody(false, h.now());
    await cache.tick();
    assert.equal(h.calls.length, 2);
    assert.equal(h.gate.claimsClosed(), true);
  });

  for (const cacheMode of ["running", "failure", "backoff"]) {
    it(`cache ${cacheMode} is not a maintenance prerequisite`, async () => {
      const h = harness();
      h.observe({ ...operation, phase: "cancelled" });
      const cache = new DindPruneController({
        gate: h.gate, dockerHost: "tcp://dind:2375", heartbeatIntervalMs: 15_000,
        workerName: "worker", isIdle: () => true, log: h.logger,
        readSample: h.sample, exec: h.exec, now: h.now, sleep: h.sleep,
      });
      h.state.exec = async (argv) => {
        if (argv[0] === "version") return "1.42";
        if (argv[0] === "ps" && cacheMode === "running") return "leaked-container";
        if (cacheMode === "failure" || (cacheMode === "backoff" && argv[0] !== "ps")) throw new Error("daemon");
        return "";
      };
      for (let i = 0; i < 4; i++) {
        h.advance(30_000);
        cache.recordCustody(false, h.now());
        await cache.tick();
      }
      h.state.exec = undefined;
      h.observe();
      h.advance(1);
      h.observe();
      const before = h.calls.length;
      await h.ctl.tick();
      assert.deepEqual(h.calls.slice(before).map((c) => c.argv[0]), ["version", "volume"]);
      assert.ok(h.ctl.acknowledgement());
    });
  }

  for (const version of ["1.42", "1.100", "2.0", "1.9", "1.41", "bad", "1.42.0", "01.42"]) {
    it(`numeric API version ${version}`, async () => {
      const h = harness();
      h.state.version = version;
      await h.ctl.tick();
      const supported = ["1.42", "1.100", "2.0"].includes(version);
      assert.equal(h.calls.length, supported ? 2 : 1);
      assert.ok(h.ctl.acknowledgement());
      await h.ctl.tick();
      assert.equal(h.calls.length, supported ? 2 : 1);
    });
  }

  it("exact command and stripped exec environment share the cache transport", async () => {
    const h = harness();
    await h.ctl.tick();
    for (const { argv, opts } of h.calls) {
      assert.equal(opts.file, "/usr/bin/docker");
      assert.ok(opts.timeoutMs > 0 && opts.timeoutMs <= 300_000);
      assert.deepEqual(Object.keys(opts.env).sort(), ["DOCKER_CONFIG", "DOCKER_HOST", "HOME", "PATH"]);
      assert.equal(opts.env.DOCKER_HOST, "tcp://dind:2375");
      assert.equal(opts.env.HOME, opts.env.DOCKER_CONFIG);
      assert.equal(statSync(opts.env.HOME!).mode & 0o777, 0o700);
      assert.deepEqual(readdirSync(opts.env.HOME!), []);
      assert.equal(opts.env.PATH, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin");
      assert.throws(() => assertDockerArgvAllowed(argv));
    }
  });

  for (const failure of ["version", "prune"]) {
    it(`optional ${failure} failure still permits fresh ready ACK`, async () => {
      const h = harness();
      h.state.exec = async (argv) => {
        if (failure === "version" || argv[0] === "volume") throw new Error(failure);
        return "1.42";
      };
      await h.ctl.tick();
      assert.equal(h.ctl.acknowledgement()?.pruned, false);
      assert.equal(h.ctl.acknowledgement()?.phase, "ready");
    });
  }

  it("below-threshold cancellation needs a new fresh sample and known API threshold", async () => {
    const h = harness();
    h.state.sleeping = () => { h.state.used = 20; };
    await h.ctl.tick();
    assert.equal(h.ctl.acknowledgement()?.phase, "cancelled");
    assert.equal(h.ctl.acknowledgement()?.reason, "below_threshold");
    h.state.threshold = undefined;
    assert.equal(h.ctl.acknowledgement()?.phase, "ready");
    h.state.threshold = 0.85;
    h.state.used = 90;
    assert.equal(h.ctl.acknowledgement()?.phase, "ready");
    h.state.used = 20;
    h.state.frozen = Math.floor(h.now() / 1000) + 1;
    assert.equal(h.ctl.acknowledgement()?.phase, "ready");
  });

  it("sample timeout is bounded; missing, stale or preregistration samples cannot cancel", async () => {
    const h = harness();
    h.state.missing = true;
    h.state.sleeping = () => h.observe(); // parallel heartbeat preserves custody during the wait
    const start = h.now();
    await h.ctl.tick();
    assert.equal(h.now() - start, 60_000);
    assert.ok(h.ctl.acknowledgement());
    h.advance(30_001);
    assert.equal(h.ctl.acknowledgement(), undefined);
    h.observe();
    assert.equal(h.ctl.acknowledgement()?.phase, "ready");
    assert.equal(h.ctl.acknowledgement()?.resampled_epoch, 0);
    h.state.missing = false;
    h.state.used = 0;
    h.state.frozen = Math.floor(start / 1000) - 1;
    assert.equal(h.ctl.acknowledgement()?.phase, "ready");
  });

  it("unknown or stale registration observations cannot repeat an existing disposition", async () => {
    const h = harness();
    await h.ctl.tick();
    h.ctl.observe(undefined, false, h.now());
    assert.equal(h.ctl.acknowledgement(), undefined);
    h.observe({ ...operation, register_nonce: "stale" });
    await h.ctl.tick();
    assert.equal(h.ctl.acknowledgement(), undefined);
    h.observe();
    h.advance(1);
    h.observe();
    await h.ctl.tick();
    assert.equal(h.calls.length, 2);
    assert.ok(h.ctl.acknowledgement());
  });

  it("registration invalidates ACK and disposition while retaining the fence", async () => {
    const h = harness();
    await h.ctl.tick();
    assert.ok(h.ctl.acknowledgement());
    h.ctl.register("new-registration", true);
    assert.equal(h.ctl.acknowledgement(), undefined);
    h.observe();
    await h.ctl.tick();
    assert.equal(h.calls.length, 2);
    assert.equal(h.gate.claimsClosed(), true);
    const replacement = { ...operation, register_nonce: "new-registration", nonce: "new-op" };
    h.observe(replacement);
    h.advance(1);
    h.observe(replacement);
    await h.ctl.tick();
    assert.equal(h.calls.length, 4);
    assert.equal(h.ctl.acknowledgement()?.nonce, "new-op");
  });

  it("cancel during an asynchronous command aborts but holds claims until exec settles", async () => {
    const h = harness();
    let settle!: () => void;
    let entered!: () => void;
    const started = new Promise<void>((resolve) => { entered = resolve; });
    h.state.exec = async (argv, opts) => {
      if (argv[0] === "version") return "1.42";
      entered();
      await new Promise<void>((resolve) => { settle = resolve; });
      assert.equal(opts.signal?.aborted, true);
      throw new Error("killed");
    };
    const work = h.ctl.tick();
    await started;
    h.observe({ ...operation, phase: "cancelled" });
    assert.equal(h.gate.tryEnterClaim(), false);
    assert.equal(h.ctl.acknowledgement(), undefined);
    settle();
    await work;
    assert.equal(h.gate.tryEnterClaim(), true);
    h.gate.exitClaim();
  });

  it("operation changes after version cannot authorize a volume command", async () => {
    const h = harness();
    h.state.exec = async () => {
      h.observe({ ...operation, nonce: "replacement" });
      return "1.42";
    };
    await h.ctl.tick();
    assert.equal(h.calls.length, 1);
    assert.equal(h.ctl.acknowledgement(), undefined);
    assert.equal(h.gate.claimsClosed(), true);
  });

  it("disabled registration cannot actuate; stopping cannot be locally cancelled", async () => {
    const h = harness();
    h.ctl.register("registration", false);
    h.advance(1);
    h.observe();
    await h.ctl.tick();
    assert.equal(h.calls.length, 0);
    h.observe({ ...operation, phase: "stopping" });
    h.ctl.register("replacement", true);
    h.observe({ ...operation, phase: "cancelled" });
    assert.equal(h.gate.claimsClosed(), true);
    h.observe({ ...operation, phase: "complete" });
    assert.equal(h.gate.claimsClosed(), false);
  });
});
