import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { rmSync } from "node:fs";
import { DindMaintenanceController } from "../src/dind-maintenance.js";
import { DindPruneGate, assertDockerArgvAllowed, dockerExecEnvironment, type DockerExec } from "../src/dind-prune.js";
import {
  DindVolumePruneController,
  assertDindVolumeArgvAllowed,
  createDindVolumePrune,
} from "../src/dind-volume-prune.js";
import {
  DIND_VOLUME_PRUNE_ARGV,
  DIND_VOLUME_SERVER_VERSION_ARGV,
  apiVersionAtLeast142,
} from "../src/dind-volume-argv.js";
import type { DindMaintenance } from "../src/protocol.js";
import type { DindMeterSample } from "../src/dind-meter.js";
import { recordingLogger } from "./helpers.js";

// issue #3891: the pressure-only anonymous-volume prune leg, driven through its public
// seams (tick/loop/createDindVolumePrune) with a fake exec, sample source and clock.

const configDirs = new Set<string>();
after(() => {
  for (const dir of configDirs) rmSync(dir, { recursive: true, force: true });
});

const HOST = "tcp://dind:2375";
const VERSION = ["version", "--format", "{{.Server.APIVersion}}"];
const PRUNE = ["volume", "prune", "-f"];
const isPrune = (argv: readonly string[]) => argv[0] === "volume";

type Call = { argv: string[]; opts: Parameters<DockerExec>[1] };

function execError(fields: Record<string, unknown>, message = "Command failed"): Error {
  return Object.assign(new Error(message), fields);
}

function harness(overrides: { gate?: DindPruneGate } = {}) {
  let time = 1_700_000_000_000;
  const gate = overrides.gate ?? new DindPruneGate();
  const { logger, lines } = recordingLogger();
  const calls: Call[] = [];
  const state = {
    bytes: 95,
    inodes: 1,
    missing: false,
    sampleOverride: undefined as DindMeterSample | null | undefined,
    version: "1.43",
    pruneOut: "Total reclaimed space: 0B\n",
    onExec: undefined as ((argv: readonly string[], opts: Parameters<DockerExec>[1]) => Promise<string | undefined> | string | undefined) | undefined,
    readThrows: false,
  };
  const sample = (): DindMeterSample | null => {
    if (state.readThrows) throw new Error("meter exploded");
    if (state.sampleOverride !== undefined) return state.sampleOverride;
    if (state.missing) return null;
    return { epochS: Math.floor(time / 1000), bytesUsed: state.bytes, bytesTotal: 100, inodesUsed: state.inodes, inodesTotal: 100 };
  };
  const exec: DockerExec = async (argv, opts) => {
    if (opts.env.DOCKER_CONFIG) configDirs.add(opts.env.DOCKER_CONFIG);
    calls.push({ argv: [...argv], opts });
    const custom = state.onExec ? await state.onExec(argv, opts) : undefined;
    if (custom !== undefined) return custom;
    return argv[0] === "version" ? state.version : state.pruneOut;
  };
  const sleep = async (ms: number) => {
    time += ms;
    // Yield a macrotask so a spinning loop() cannot starve timers.
    await new Promise((r) => setImmediate(r));
  };
  const leg = new DindVolumePruneController({
    dockerHost: HOST,
    workerName: "w1",
    gate,
    log: logger,
    readSample: sample,
    exec,
    now: () => time,
    sleep,
  });
  /** Advance one meter interval, then evaluate. */
  const step = async (signal?: AbortSignal) => {
    time += 30_000;
    return leg.tick(signal);
  };
  return { leg, gate, state, calls, lines, step, now: () => time, advance: (ms: number) => { time += ms; }, logger };
}

const pruneCalls = (calls: Call[]) => calls.filter((c) => isPrune(c.argv));
const msgs = (lines: unknown[]) => (lines as Array<{ msg: string }>).map((l) => l.msg);

describe("createDindVolumePrune", () => {
  const deps = { gate: new DindPruneGate(), log: recordingLogger().logger };
  it("uses the cache prune's predicate: flag exactly true and a wired docker host", () => {
    const wired = { dockerHost: HOST };
    assert.ok(createDindVolumePrune({ dindPruneEnabled: true, dockerWiring: wired, workerName: "w" } as never, deps));
    assert.equal(createDindVolumePrune({ dindPruneEnabled: false, dockerWiring: wired, workerName: "w" } as never, deps), undefined);
    assert.equal(createDindVolumePrune({ dindPruneEnabled: true, dockerWiring: {}, workerName: "w" } as never, deps), undefined);
    assert.equal(createDindVolumePrune({ dindPruneEnabled: true, dockerWiring: { dockerHost: "" }, workerName: "w" } as never, deps), undefined);
    assert.equal(createDindVolumePrune({ dindPruneEnabled: true, dockerWiring: undefined, workerName: "w" } as never, deps), undefined);
    assert.equal(createDindVolumePrune({ dindPruneEnabled: "true", dockerWiring: wired, workerName: "w" } as never, deps), undefined);
  });
});

describe("pressure detection", () => {
  it("needs two distinct fresh over samples; one is pending", async () => {
    const h = harness();
    assert.equal(await h.step(), "pending");
    assert.equal(h.calls.length, 0);
    assert.equal(await h.step(), "completed");
    assert.deepEqual(h.calls.map((c) => c.argv), [VERSION, PRUNE]);
  });

  it("an inode-only over sample triggers", async () => {
    const h = harness();
    h.state.bytes = 10;
    h.state.inodes = 90;
    await h.step();
    assert.equal(await h.step(), "completed");
  });

  it("a sample below 85% on both resets the streak", async () => {
    const h = harness();
    await h.step();
    h.state.bytes = 50;
    assert.equal(await h.step(), "below");
    h.state.bytes = 95;
    assert.equal(await h.step(), "pending");
    assert.equal(h.calls.length, 0);
  });

  it("a missing sample resets the streak", async () => {
    const h = harness();
    await h.step();
    h.state.missing = true;
    assert.equal(await h.step(), "no-sample");
    h.state.missing = false;
    assert.equal(await h.step(), "pending");
  });

  it("stale and invalid samples count as missing", async () => {
    const h = harness();
    const base = { bytesUsed: 95, bytesTotal: 100, inodesUsed: 1, inodesTotal: 100 };
    for (const bad of [
      { ...base, epochS: Math.floor(h.now() / 1000) - 3600 },
      { ...base, epochS: Math.floor(h.now() / 1000), bytesTotal: 0 },
      { ...base, epochS: Math.floor(h.now() / 1000), inodesTotal: -1 },
      { ...base, epochS: Number.NaN },
      { ...base, epochS: Math.floor(h.now() / 1000), bytesUsed: -5 },
    ]) {
      h.state.sampleOverride = bad;
      assert.equal(await h.leg.tick(), "no-sample");
    }
    assert.equal(h.calls.length, 0);
  });

  it("a re-read of the same epoch never counts twice", async () => {
    const h = harness();
    h.advance(30_000);
    assert.equal(await h.leg.tick(), "pending");
    assert.equal(await h.leg.tick(), "pending");
    assert.equal(await h.leg.tick(), "pending");
    assert.equal(h.calls.length, 0);
  });

  it("an epoch gap over 45s restarts the streak at one", async () => {
    const h = harness();
    h.advance(30_000);
    assert.equal(await h.leg.tick(), "pending");
    h.advance(46_000);
    assert.equal(await h.leg.tick(), "pending");
    assert.equal(h.calls.length, 0);
    h.advance(30_000);
    assert.equal(await h.leg.tick(), "completed");
  });
});

describe("attempt", () => {
  it("runs the frozen pair with the pinned env and timeouts, ignoring ambient docker env", async () => {
    const saved = { v: process.env.DOCKER_API_VERSION, c: process.env.DOCKER_CONTEXT, h: process.env.DOCKER_HOST };
    process.env.DOCKER_API_VERSION = "1.41";
    process.env.DOCKER_CONTEXT = "evil";
    process.env.DOCKER_HOST = "tcp://evil:1";
    try {
      const h = harness();
      await h.step();
      await h.step();
      assert.equal(h.calls.length, 2);
      assert.equal(h.calls[0]!.opts.timeoutMs, 30_000);
      assert.equal(h.calls[1]!.opts.timeoutMs, 5 * 60_000);
      for (const c of h.calls) {
        assert.equal(c.opts.file, "/usr/bin/docker");
        assert.deepEqual(Object.keys(c.opts.env).sort(), ["DOCKER_API_VERSION", "DOCKER_CONFIG", "DOCKER_HOST", "HOME", "PATH"]);
        assert.equal(c.opts.env.DOCKER_API_VERSION, "1.42");
        assert.equal(c.opts.env.DOCKER_HOST, HOST);
      }
    } finally {
      for (const [k, v] of [["DOCKER_API_VERSION", saved.v], ["DOCKER_CONTEXT", saved.c], ["DOCKER_HOST", saved.h]] as const) {
        if (v === undefined) delete process.env[k];
        else process.env[k] = v;
      }
    }
  });

  it("the shared docker env helper carries no DOCKER_API_VERSION", () => {
    assert.equal("DOCKER_API_VERSION" in dockerExecEnvironment(HOST), false);
  });

  it("never touches the gate", async () => {
    const h = harness();
    const generation = h.gate.generation();
    await h.step();
    await h.step();
    assert.equal(h.gate.generation(), generation);
    assert.equal(h.gate.claimsClosed(), false);
  });

  for (const [name, version] of [["old", "1.41"], ["malformed", "v1.43"], ["empty", ""], ["three-part", "1.43.1"]] as const) {
    it(`a ${name} server API (${JSON.stringify(version)}) is refused without a prune and consumes the backoff`, async () => {
      const h = harness();
      h.state.version = version;
      await h.step();
      assert.equal(await h.step(), "refused");
      assert.equal(pruneCalls(h.calls).length, 0);
      assert.equal(await h.step(), "backoff");
      assert.equal(h.calls.length, 1);
    });
  }

  it("an unreadable version query is refused without a prune", async () => {
    const h = harness();
    h.state.onExec = async () => {
      throw execError({ code: 1 }, "Cannot connect to the Docker daemon");
    };
    await h.step();
    assert.equal(await h.step(), "refused");
    assert.equal(h.calls.length, 1);
  });

  const failures: Array<[string, Record<string, unknown>, string, string]> = [
    ["spawn ENOENT", { code: "ENOENT", syscall: "spawn /usr/bin/docker" }, "Command failed", "refused"],
    ["timeout kill", { killed: true, signal: "SIGKILL", code: null }, "Command failed", "unknown"],
    ["signal", { signal: "SIGTERM", code: null }, "Command failed", "unknown"],
    ["abort after spawn", { code: "ABORT_ERR", name: "AbortError" }, "The operation was aborted", "unknown"],
    ["stdout overflow", { code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER" }, "stdout maxBuffer length exceeded", "unknown"],
    ["lost response (exit 1, no daemon error)", { code: 1 }, "Command failed: docker volume prune -f\nerror during connect", "unknown"],
    ["daemon rejection", { code: 1 }, "Command failed\nError response from daemon: boom", "rejected"],
    ["daemon too old", { code: 1 }, "Error response from daemon: client version 1.42 is too old. Minimum supported API version is 1.50", "rejected"],
    ["daemon too new", { code: 1 }, "Error response from daemon: client version 1.42 is too new. Maximum supported API version is 1.41", "rejected"],
    ["prune conflict", { code: 1 }, "Error response from daemon: a prune operation is already running", "conflict"],
  ];
  for (const [name, fields, message, expected] of failures) {
    it(`prune failure: ${name} -> ${expected}, exactly one prune exec, no retry, backoff consumed`, async () => {
      const h = harness();
      h.state.onExec = async (argv) => {
        if (isPrune(argv)) throw execError(fields, message);
        return undefined;
      };
      await h.step();
      assert.equal(await h.step(), expected);
      assert.equal(pruneCalls(h.calls).length, 1);
      for (let i = 0; i < 3; i++) assert.equal(await h.step(), "backoff");
      assert.equal(pruneCalls(h.calls).length, 1, "no retry");
      assert.ok(msgs(h.lines).some((m) => m === `dind-volume-prune-${expected} worker=w1`), msgs(h.lines).join("|"));
    });
  }

  it("daemon error text arriving only on stderr is still classified as a daemon response", async () => {
    const h = harness();
    h.state.onExec = async (argv) => {
      if (isPrune(argv)) throw execError({ code: 1, stderr: "Error response from daemon: a prune operation is already running" });
      return undefined;
    };
    await h.step();
    assert.equal(await h.step(), "conflict");
  });

  it("unknown and rejected disclaim reclamation and preservation; unknown logs no success", async () => {
    for (const [fields, message, expected] of [
      [{ killed: true, signal: "SIGKILL" }, "timed out", "unknown"],
      [{ code: 1 }, "Error response from daemon: nope", "rejected"],
    ] as const) {
      const h = harness();
      h.state.onExec = async (argv) => {
        if (isPrune(argv)) throw execError({ ...fields }, message);
        return undefined;
      };
      await h.step();
      await h.step();
      const rec = (h.lines as Array<{ msg: string; note?: string }>).find((l) => l.msg.startsWith(`dind-volume-prune-${expected}`));
      assert.ok(rec, expected);
      assert.match(rec.note ?? "", /no reclamation or preservation claimed/);
      assert.equal(msgs(h.lines).some((m) => m.startsWith("dind-volume-prune-completed")), false);
      assert.equal(msgs(h.lines).some((m) => m.startsWith("dind-volume-prune-after")), false);
    }
  });

  it("bounds the error text it logs", async () => {
    const h = harness();
    h.state.onExec = async (argv) => {
      if (isPrune(argv)) throw execError({ code: 1 }, `Error response from daemon: ${"x".repeat(5000)}\nsecond line`);
      return undefined;
    };
    await h.step();
    await h.step();
    const rec = (h.lines as Array<{ msg: string; error?: string }>).find((l) => l.msg.startsWith("dind-volume-prune-rejected"));
    assert.ok(rec?.error);
    assert.ok(rec.error.length <= 310, `error length ${rec.error.length}`);
    assert.equal(rec.error.includes("\n"), false);
  });

  it("completed logs before ratios, the parsed reclaimed value only, then the after reading", async () => {
    const h = harness();
    h.state.pruneOut = "Deleted Volumes:\nsecret-volume-id-123\n\nTotal reclaimed space: 1.5GB\n";
    await h.step();
    // The prune lowers the meter's reading: the after sample is read once the prune settled.
    h.state.onExec = async (argv) => {
      if (isPrune(argv)) h.state.bytes = 40;
      return undefined;
    };
    assert.equal(await h.step(), "completed");
    const all = JSON.stringify(h.lines);
    assert.equal(all.includes("secret-volume-id-123"), false, "raw output is never logged");
    assert.equal(all.includes("Deleted Volumes"), false);
    const done = (h.lines as Array<Record<string, unknown>>).find((l) => l.msg === "dind-volume-prune-completed worker=w1");
    assert.ok(done);
    assert.equal(done.reclaimed, "1.5GB");
    assert.equal(done.before_bytes_pct, 95);
    const after = (h.lines as Array<Record<string, unknown>>).find((l) => l.msg === "dind-volume-prune-after worker=w1");
    assert.ok(after);
    assert.equal(after.bytes_pct, 40);
  });

  it("completed with no parsable reclaimed line logs no reclaimed field and still the after reading", async () => {
    const h = harness();
    h.state.pruneOut = "garbage";
    await h.step();
    await h.step();
    const done = (h.lines as Array<Record<string, unknown>>).find((l) => l.msg === "dind-volume-prune-completed worker=w1");
    assert.ok(done);
    assert.equal("reclaimed" in done, false);
  });

  it("the after reading is unavailable when no newer sample arrives within the wait", async () => {
    const h = harness();
    await h.step();
    // Freeze the meter on a single epoch so nothing newer than the prune ever appears.
    h.state.sampleOverride = { epochS: Math.floor(h.now() / 1000) + 30, bytesUsed: 95, bytesTotal: 100, inodesUsed: 1, inodesTotal: 100 };
    h.advance(30_000);
    assert.equal(await h.leg.tick(), "completed");
    const after = (h.lines as Array<Record<string, unknown>>).find((l) => l.msg === "dind-volume-prune-after worker=w1");
    assert.deepEqual(after?.sample, "unavailable");
  });

  it("uses the api worker id in log tokens once known", async () => {
    const h = harness();
    h.leg.setWorkerId("wid-9");
    await h.step();
    await h.step();
    assert.ok(msgs(h.lines).includes("dind-volume-prune-completed worker=wid-9"));
  });

  it("is single-flight: a concurrent tick returns in-flight and execs nothing", async () => {
    const h = harness();
    let release!: () => void;
    const gateOpen = new Promise<void>((r) => (release = r));
    h.state.onExec = async (argv) => {
      if (isPrune(argv)) await gateOpen;
      return undefined;
    };
    await h.step();
    const first = h.step();
    await new Promise((r) => setImmediate(r));
    const before = h.calls.length;
    assert.equal(await h.leg.tick(), "in-flight");
    assert.equal(h.calls.length, before);
    release();
    assert.equal(await first, "completed");
    assert.equal(pruneCalls(h.calls).length, 1);
  });
});

describe("hysteresis", () => {
  it("after a non-unknown outcome a sample below 75% re-arms, then two over samples trigger again", async () => {
    const h = harness();
    await h.step();
    assert.equal(await h.step(), "completed");
    h.state.bytes = 80;
    assert.equal(await h.step(), "backoff");
    h.state.bytes = 50;
    assert.equal(await h.step(), "below");
    h.state.bytes = 95;
    assert.equal(await h.step(), "pending");
    assert.equal(await h.step(), "completed");
    assert.equal(pruneCalls(h.calls).length, 2);
  });

  it("re-arms after an hour even under sustained pressure", async () => {
    const h = harness();
    await h.step();
    await h.step();
    assert.equal(await h.step(), "backoff");
    h.advance(60 * 60_000);
    assert.equal(await h.leg.tick(), "pending");
    assert.equal(await h.step(), "completed");
  });

  it("after unknown a below-75% sample does NOT re-arm; the full hour applies", async () => {
    const h = harness();
    h.state.onExec = async (argv) => {
      if (isPrune(argv)) throw execError({ code: 1 }, "Command failed");
      return undefined;
    };
    await h.step();
    assert.equal(await h.step(), "unknown");
    h.state.bytes = 10;
    assert.equal(await h.step(), "backoff");
    h.state.bytes = 95;
    for (let i = 0; i < 5; i++) assert.equal(await h.step(), "backoff");
    assert.equal(pruneCalls(h.calls).length, 1, "no replacement prune within the hour under continued pressure");
    assert.equal(msgs(h.lines).some((m) => m.startsWith("dind-volume-prune-completed")), false);
    h.advance(60 * 60_000);
    assert.equal(await h.leg.tick(), "pending");
    assert.equal(await h.step(), "unknown");
    assert.equal(pruneCalls(h.calls).length, 2);
  });
});

describe("maintenance fence", () => {
  it("defers with maintenance-fenced while maintenance holds the gate: no exec, streak kept, no backoff, one log", async () => {
    const h = harness();
    await h.step();
    h.gate.close("maintenance");
    const generation = h.gate.generation();
    for (let i = 0; i < 4; i++) assert.equal(await h.step(), "maintenance-fenced");
    assert.equal(h.calls.length, 0);
    assert.equal(h.gate.generation(), generation, "the leg only reads the gate");
    const deferrals = msgs(h.lines).filter((m) => m.includes("reason=maintenance-fenced"));
    assert.deepEqual(deferrals, ["dind-volume-prune-deferred worker=w1 reason=maintenance-fenced"]);
    h.gate.open("maintenance");
    assert.equal(await h.step(), "completed", "streak survived and no backoff was consumed");
    assert.equal(h.calls.length, 2);
  });

  it("a fence that appears between the version query and the prune stops the prune", async () => {
    const h = harness();
    let fencedOnce = false;
    h.state.onExec = async (argv) => {
      if (argv[0] === "version" && !fencedOnce) {
        fencedOnce = true;
        h.gate.close("maintenance");
      }
      return undefined;
    };
    await h.step();
    assert.equal(await h.step(), "maintenance-fenced");
    assert.equal(pruneCalls(h.calls).length, 0);
    h.gate.open("maintenance");
    assert.equal(await h.step(), "completed");
  });

  it("other gate owners (cache, retained_ops, terminal_maintenance) do not fence the leg", async () => {
    const h = harness();
    for (const owner of ["cache", "retained_ops", "terminal_maintenance"] as const) h.gate.close(owner);
    await h.step();
    assert.equal(await h.step(), "completed");
  });

  it("coexists with a real DindMaintenanceController on one gate", async () => {
    const gate = new DindPruneGate();
    const h = harness({ gate });
    const operation: DindMaintenance = {
      id: "op", nonce: "n", deployment_uid: "d", pvc_uid: "p", register_nonce: "reg",
      phase: "ready", fenced: true, ready_ack: false,
    };
    const mCalls: Call[] = [];
    const maintenance = new DindMaintenanceController({
      gate,
      dockerHost: HOST,
      heartbeatIntervalMs: 15_000,
      isIdle: () => true,
      log: h.logger,
      threshold: () => 0.85,
      readSample: () => null,
      exec: async (argv, opts) => {
        mCalls.push({ argv: [...argv], opts });
        return argv[0] === "version" ? "1.43" : "";
      },
      now: h.now,
      sleep: async (ms) => {
        h.advance(ms);
      },
    });
    maintenance.register("reg", true);
    maintenance.observe(operation, false, h.now());
    h.advance(1000);
    maintenance.observe(operation, false, h.now());
    assert.equal(gate.heldBy("maintenance"), true);

    await h.step();
    assert.equal(await h.step(), "maintenance-fenced");
    assert.equal(h.calls.length, 0, "the leg issues no exec while maintenance holds the gate");

    maintenance.observe(operation, false, h.now()); // a fresh heartbeat, as the worker sends
    await maintenance.tick();
    assert.deepEqual(mCalls.map((c) => c.argv), [VERSION, PRUNE]);
    for (const c of mCalls) assert.equal("DOCKER_API_VERSION" in c.opts.env, false, "maintenance env is unchanged");
    assert.equal(h.calls.length, 0);

    maintenance.observe({ ...operation, phase: "complete" }, false, h.now());
    assert.equal(gate.heldBy("maintenance"), false);
    // Maintenance's own waits advanced the clock past the 45s gap, so the streak restarts.
    assert.equal(await h.step(), "pending");
    assert.equal(await h.step(), "completed");
    assert.deepEqual(h.calls.map((c) => c.argv), [VERSION, PRUNE]);
  });
});

describe("loop", () => {
  it("returns promptly on abort and joins an in-flight client", async () => {
    const h = harness();
    const controller = new AbortController();
    h.state.onExec = (argv, opts) => {
      if (!isPrune(argv)) return undefined;
      return new Promise<string>((_resolve, reject) => {
        opts.signal?.addEventListener("abort", () => reject(execError({ code: "ABORT_ERR", name: "AbortError" }, "aborted")), { once: true });
      });
    };
    h.advance(30_000);
    await h.leg.tick(); // first over sample
    h.advance(30_000);
    const done = h.leg.loop(controller.signal);
    await new Promise((r) => setTimeout(r, 20));
    controller.abort();
    await done;
    assert.equal(pruneCalls(h.calls).length, 1);
    assert.ok(msgs(h.lines).includes("dind-volume-prune-unknown worker=w1"));
  });

  it("never rejects when the meter reader throws", async () => {
    const h = harness();
    h.state.readThrows = true;
    const controller = new AbortController();
    const done = h.leg.loop(controller.signal);
    await new Promise((r) => setTimeout(r, 10));
    controller.abort();
    await done;
    assert.equal(h.calls.length, 0);
  });
});

describe("argv guard", () => {
  it("accepts exactly the frozen pair", () => {
    assertDindVolumeArgvAllowed(DIND_VOLUME_SERVER_VERSION_ARGV);
    assertDindVolumeArgvAllowed(DIND_VOLUME_PRUNE_ARGV);
    assert.deepEqual([...DIND_VOLUME_SERVER_VERSION_ARGV], VERSION);
    assert.deepEqual([...DIND_VOLUME_PRUNE_ARGV], PRUNE);
    assert.ok(Object.isFrozen(DIND_VOLUME_PRUNE_ARGV) && Object.isFrozen(DIND_VOLUME_SERVER_VERSION_ARGV));
  });

  it("rejects every other argv", () => {
    for (const bad of [
      ["volume", "prune", "-a", "-f"],
      ["volume", "prune", "-f", "--filter", "all=true"],
      ["volume", "prune", "--all", "-f"],
      ["volume", "prune"],
      ["system", "prune", "-f"],
      ["volume", "rm", "abc"],
      ["image", "prune", "-f"],
      ["builder", "prune", "-f"],
      ["ps", "-q"],
      [],
    ]) {
      assert.throws(() => assertDindVolumeArgvAllowed(bad), /frozen pair/, bad.join(" "));
    }
  });

  it("leaves the cache prune's guard rejecting volume prune", () => {
    assert.throws(() => assertDockerArgvAllowed(["volume", "prune", "-f"]), /forbidden/);
  });
});

describe("apiVersionAtLeast142", () => {
  it("accepts 1.42 and above only in major.minor form", () => {
    for (const ok of ["1.42", "1.43", "1.100", "2.0", "10.1"]) assert.equal(apiVersionAtLeast142(ok), true, ok);
    for (const bad of ["1.41", "0.99", "1.4", "01.42", "1.042", "1.42.1", " 1.42", "", "abc", "1.", ".42"]) {
      assert.equal(apiVersionAtLeast142(bad), false, JSON.stringify(bad));
    }
  });
});
