import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { loadConfig } from "../src/config.js";
import { DindMaintenanceController } from "../src/dind-maintenance.js";
import {
  DIND_BUILDER_PRUNE_ARGV,
  DindBuilderPruneController,
  assertDindBuilderArgvAllowed,
  createDindBuilderPrune,
  type BuilderPruneExec,
  type BuilderPruneExecOptions,
  type BuilderPruneExecResult,
} from "../src/dind-builder-prune.js";
import { DindPruneGate, assertDockerArgvAllowed, dockerExecEnvironment, DOCKER_BIN } from "../src/dind-prune.js";
import { assertDindVolumeArgvAllowed } from "../src/dind-volume-prune.js";
import type { DindMaintenance } from "../src/protocol.js";
import type { DindMeterSample } from "../src/dind-meter.js";
import { recordingLogger } from "./helpers.js";

// issue #4291: the pressure-only BuildKit regular-cache prune leg, driven through its public
// seams (tick/loop/createDindBuilderPrune). Most tests inject a fake exec; the real-child
// tests spawn node fixtures through the leg's real spawn wrapper.

const HOST = "tcp://dind:2375";
const FROZEN = ["builder", "prune", "-f", "--filter", "type=regular", "--filter", "until=24h", "--max-used-space", "4GiB"];
const CHILD = fileURLToPath(new URL("./fixtures/dind-builder-prune-child.mjs", import.meta.url));
const RUNNER = fileURLToPath(new URL("./fixtures/dind-builder-prune-exit-runner.ts", import.meta.url));
const AGENT_DIR = fileURLToPath(new URL("..", import.meta.url));

const scratchDirs: string[] = [];
const configDirs = new Set<string>();
after(() => {
  for (const d of [...scratchDirs, ...configDirs]) rmSync(d, { recursive: true, force: true });
});

function scratch(): string {
  const d = mkdtempSync(join(tmpdir(), "uzi-builder-prune-test-"));
  scratchDirs.push(d);
  return d;
}

const PLUGIN = (() => {
  const p = join(scratch(), "docker-buildx");
  writeFileSync(p, "");
  return p;
})();

class SpyGate extends DindPruneGate {
  calls: string[] = [];
  override close(owner?: Parameters<DindPruneGate["close"]>[0]): void {
    this.calls.push("close");
    super.close(owner);
  }
  override open(owner?: Parameters<DindPruneGate["open"]>[0]): void {
    this.calls.push("open");
    super.open(owner);
  }
  override tryEnterClaim(): boolean {
    this.calls.push("tryEnterClaim");
    return super.tryEnterClaim();
  }
  override exitClaim(): void {
    this.calls.push("exitClaim");
    super.exitClaim();
  }
}

const exited = (code: number, stdout = "", stderr = "", signal: string | null = null): BuilderPruneExecResult => ({
  kind: "exit",
  code,
  signal,
  stdout,
  stderr,
});

type Call = { argv: string[]; opts: BuilderPruneExecOptions };

function harness(overrides: { gate?: DindPruneGate; pluginPath?: string; real?: Partial<ConstructorParameters<typeof DindBuilderPruneController>[0]> } = {}) {
  let time = 1_700_000_000_000;
  const gate = overrides.gate ?? new DindPruneGate();
  const { logger, lines } = recordingLogger();
  const calls: Call[] = [];
  const state = {
    bytes: 95,
    inodes: 1,
    missing: false,
    sampleOverride: undefined as DindMeterSample | null | undefined,
    result: exited(0, "Deleted build cache records\nTotal:\t1.081MB\n") as BuilderPruneExecResult,
    onExec: undefined as ((argv: readonly string[], opts: BuilderPruneExecOptions) => Promise<BuilderPruneExecResult | undefined> | undefined) | undefined,
    readThrows: false,
  };
  const sample = (): DindMeterSample | null => {
    if (state.readThrows) throw new Error("meter exploded");
    if (state.sampleOverride !== undefined) return state.sampleOverride;
    if (state.missing) return null;
    return { epochS: Math.floor(time / 1000), bytesUsed: state.bytes, bytesTotal: 100, inodesUsed: state.inodes, inodesTotal: 100 };
  };
  const exec: BuilderPruneExec = async (argv, opts) => {
    if (opts.env.DOCKER_CONFIG) configDirs.add(opts.env.DOCKER_CONFIG);
    calls.push({ argv: [...argv], opts });
    const custom = state.onExec ? await state.onExec(argv, opts) : undefined;
    return custom ?? state.result;
  };
  const sleep = async (ms: number) => {
    time += ms;
    // Yield a macrotask so a spinning loop() cannot starve timers.
    await new Promise((r) => setImmediate(r));
  };
  const real = overrides.real;
  const leg = new DindBuilderPruneController({
    dockerHost: HOST,
    workerName: "w1",
    gate,
    log: logger,
    readSample: sample,
    ...(real ? {} : { exec }),
    now: () => time,
    sleep,
    pluginPath: overrides.pluginPath ?? PLUGIN,
    ...real,
  });
  /** Advance one meter interval, then evaluate. */
  const step = async (signal?: AbortSignal) => {
    time += 30_000;
    return leg.tick(signal);
  };
  return { leg, gate, state, calls, lines, step, now: () => time, advance: (ms: number) => { time += ms; }, logger };
}

const msgs = (lines: unknown[]) => (lines as Array<{ msg: string }>).map((l) => l.msg);
const rec = (lines: unknown[], prefix: string) =>
  (lines as Array<Record<string, unknown>>).find((l) => typeof l.msg === "string" && l.msg.startsWith(prefix));

describe("createDindBuilderPrune activation", () => {
  const deps = { gate: new DindPruneGate(), log: recordingLogger().logger };
  const wired = { dockerHost: HOST };
  const base = { UZI_API_URL: "http://api:8080", UZI_WORKER_TOKEN: "worker-join-token-0123456789" };

  it("requires the master switch, the leg switch and a wired docker host", () => {
    const cfg = (over: Record<string, unknown>) => ({ dindPruneEnabled: true, dindBuilderPruneEnabled: true, dockerWiring: wired, workerName: "w", ...over }) as never;
    assert.ok(createDindBuilderPrune(cfg({}), deps));
    assert.equal(createDindBuilderPrune(cfg({ dindPruneEnabled: false }), deps), undefined);
    assert.equal(createDindBuilderPrune(cfg({ dindBuilderPruneEnabled: false }), deps), undefined);
    assert.equal(createDindBuilderPrune(cfg({ dindPruneEnabled: "true" }), deps), undefined);
    assert.equal(createDindBuilderPrune(cfg({ dindBuilderPruneEnabled: "true" }), deps), undefined);
    assert.equal(createDindBuilderPrune(cfg({ dockerWiring: {} }), deps), undefined);
    assert.equal(createDindBuilderPrune(cfg({ dockerWiring: { dockerHost: "" } }), deps), undefined);
    assert.equal(createDindBuilderPrune(cfg({ dockerWiring: undefined }), deps), undefined);
  });

  it("matrix: master switch x UZI_DIND_BUILDER_PRUNE_ENABLED {absent,true,false,empty,garbage} x wiring", () => {
    const switches: Array<[string | undefined, boolean]> = [
      [undefined, true],
      ["true", true],
      ["false", false],
      ["", false],
      ["TRUE", false],
      ["1", false],
      ["garbage", false],
    ];
    for (const master of [undefined, "true", "false"]) {
      for (const [sw, legOn] of switches) {
        const env: NodeJS.ProcessEnv = {
          ...base,
          ...(master === undefined ? {} : { UZI_DIND_PRUNE_ENABLED: master }),
          ...(sw === undefined ? {} : { UZI_DIND_BUILDER_PRUNE_ENABLED: sw }),
        };
        const cfg = loadConfig(env);
        assert.equal(cfg.dindBuilderPruneEnabled, legOn, `parse ${JSON.stringify(sw)}`);
        for (const dockerWiring of [wired, {} as { dockerHost?: string }]) {
          const built = createDindBuilderPrune({ ...cfg, dockerWiring }, deps);
          const expected = master === "true" && legOn && dockerWiring.dockerHost !== undefined;
          assert.equal(built !== undefined, expected, `master=${master} switch=${JSON.stringify(sw)} wired=${dockerWiring.dockerHost !== undefined}`);
        }
      }
    }
  });
});

describe("argv guard", () => {
  it("accepts exactly the frozen argv, frozen", () => {
    assertDindBuilderArgvAllowed(DIND_BUILDER_PRUNE_ARGV);
    assert.deepEqual([...DIND_BUILDER_PRUNE_ARGV], FROZEN);
    assert.ok(Object.isFrozen(DIND_BUILDER_PRUNE_ARGV));
  });

  it("rejects every other argv", () => {
    const without = (flag: string, value: string) => FROZEN.filter((v, i) => !(v === value || (v === flag && FROZEN[i + 1] === value)));
    for (const bad of [
      ["builder", "prune", "-f"],
      [...FROZEN, "-a"],
      [...FROZEN, "--all"],
      without("--filter", "type=regular"),
      without("--filter", "until=24h"),
      FROZEN.map((v) => (v === "until=24h" ? "until=12h" : v)),
      FROZEN.map((v) => (v === "4GiB" ? "8GiB" : v)),
      FROZEN.slice(0, -2),
      ["volume", "prune", "-f"],
      ["system", "prune", "-f"],
      ["image", "prune", "-a", "-f"],
      ["ps", "-q"],
      [],
    ]) {
      assert.throws(() => assertDindBuilderArgvAllowed(bad), /frozen command/, bad.join(" "));
    }
  });

  it("the other legs' guards reject the new argv", () => {
    assert.throws(() => assertDockerArgvAllowed(FROZEN), /frozen allowlist/);
    assert.throws(() => assertDindVolumeArgvAllowed(FROZEN), /frozen pair/);
    // dind-maintenance's allowlist is a private exact-match inside its docker() wrapper, so it
    // is not reachable here; the coexistence test below shows a real maintenance controller
    // executes only its own version/prune pair.
  });

  it("the new leg's guard rejects the other legs' argv", () => {
    for (const other of [["builder", "prune", "-f"], ["volume", "prune", "-f"], ["image", "prune", "-f"]]) {
      assert.throws(() => assertDindBuilderArgvAllowed(other), /frozen command/);
    }
  });
});

describe("pressure detection", () => {
  it("needs two distinct fresh over samples; one is pending", async () => {
    const h = harness();
    assert.equal(await h.step(), "pending");
    assert.equal(h.calls.length, 0);
    assert.equal(await h.step(), "completed");
    assert.deepEqual(h.calls.map((c) => c.argv), [FROZEN]);
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

describe("attempt contract", () => {
  it("runs the frozen argv with exactly the minimal env plus DOCKER_BUILDKIT=1, ignoring ambient env", async () => {
    const keys = ["DOCKER_API_VERSION", "DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_BUILDKIT", "UZI_BUILDER_PRUNE_SECRET"] as const;
    const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
    process.env.DOCKER_API_VERSION = "1.41";
    process.env.DOCKER_CONTEXT = "evil";
    process.env.DOCKER_HOST = "tcp://evil:1";
    process.env.DOCKER_BUILDKIT = "0";
    process.env.UZI_BUILDER_PRUNE_SECRET = "s3cret";
    try {
      const h = harness();
      await h.step();
      await h.step();
      assert.equal(h.calls.length, 1);
      const c = h.calls[0]!;
      assert.deepEqual(c.argv, FROZEN);
      assert.equal(c.opts.file, DOCKER_BIN);
      assert.equal(c.opts.file, "/usr/bin/docker");
      assert.deepEqual(c.opts.argvPrefix, []);
      assert.equal(c.opts.timeoutMs, 5 * 60_000);
      assert.equal(c.opts.settleTimeoutMs, 10_000);
      assert.deepEqual(c.opts.env, { ...dockerExecEnvironment(HOST), DOCKER_BUILDKIT: "1" });
      assert.deepEqual(Object.keys(c.opts.env).sort(), ["DOCKER_BUILDKIT", "DOCKER_CONFIG", "DOCKER_HOST", "HOME", "PATH"]);
      assert.equal("DOCKER_API_VERSION" in c.opts.env, false);
    } finally {
      for (const k of keys) {
        if (saved[k] === undefined) delete process.env[k];
        else process.env[k] = saved[k];
      }
    }
  });

  it("never touches the gate: zero close/open/tryEnterClaim/exitClaim calls across a completed attempt", async () => {
    const gate = new SpyGate();
    const h = harness({ gate });
    await h.step();
    await h.step();
    assert.deepEqual(gate.calls, []);
    assert.equal(gate.generation(), 0);
    assert.equal(gate.claimsClosed(), false);
  });

  it("preflight: a missing plugin path is refused with zero spawns, logs the reason, consumes the backoff", async () => {
    const h = harness({ pluginPath: join(scratch(), "no-such-plugin") });
    await h.step();
    assert.equal(await h.step(), "refused");
    assert.equal(h.calls.length, 0);
    assert.equal(rec(h.lines, "dind-builder-prune-refused worker=w1")?.reason, "buildx-plugin-missing");
    assert.equal(await h.step(), "backoff");
    assert.equal(h.calls.length, 0);
  });

  it("preflight: a directory is not a regular file", async () => {
    const dir = join(scratch(), "dir");
    mkdirSync(dir);
    const h = harness({ pluginPath: dir });
    await h.step();
    assert.equal(await h.step(), "refused");
    assert.equal(h.calls.length, 0);
  });

  const failures: Array<[string, BuilderPruneExecResult, string]> = [
    ["spawn error", { kind: "spawn-error", error: "spawn /usr/bin/docker ENOENT" }, "refused"],
    ["timeout kill", { kind: "killed", reason: "timeout" }, "unknown"],
    ["abort kill", { kind: "killed", reason: "abort" }, "unknown"],
    ["overflow kill", { kind: "killed", reason: "overflow" }, "unknown"],
    ["external signal", exited(0, "", "", "SIGTERM"), "unknown"],
    ["external signal after daemon text", exited(1, "", "Error response from daemon: boom", "SIGKILL"), "unknown"],
    ["buildx missing", exited(1, "", "ERROR: BuildKit is enabled but the buildx component is missing or broken."), "refused"],
    ["buildx missing beats daemon text", exited(1, "", "Error response from daemon: x\nthe buildx component is missing or broken"), "refused"],
    ["daemon rejection", exited(1, "", "Error response from daemon: boom"), "rejected"],
    ["rpc error", exited(1, "", "rpc error: code = Unavailable desc = closing transport"), "rejected"],
    ["prune conflict", exited(1, "", "Error response from daemon: a prune operation is already running"), "conflict"],
    ["already running without a daemon error is not a conflict", exited(1, "", "something already running"), "unknown"],
    ["lost response (exit 1, no daemon error)", exited(1, "", "error during connect"), "unknown"],
  ];
  for (const [name, result, expected] of failures) {
    it(`failure: ${name} -> ${expected}, exactly one exec, no retry, backoff consumed`, async () => {
      const h = harness();
      h.state.result = result;
      await h.step();
      assert.equal(await h.step(), expected);
      assert.equal(h.calls.length, 1);
      for (let i = 0; i < 3; i++) assert.equal(await h.step(), "backoff");
      assert.equal(h.calls.length, 1, "no retry");
      assert.ok(msgs(h.lines).includes(`dind-builder-prune-${expected} worker=w1`), msgs(h.lines).join("|"));
      assert.equal(msgs(h.lines).some((m) => m.startsWith("dind-builder-prune-completed")), false);
    });
  }

  it("unknown and rejected disclaim reclamation and preservation", async () => {
    for (const [result, expected] of [
      [{ kind: "killed", reason: "timeout" }, "unknown"],
      [exited(1, "", "Error response from daemon: nope"), "rejected"],
    ] as const) {
      const h = harness();
      h.state.result = result;
      await h.step();
      await h.step();
      const r = rec(h.lines, `dind-builder-prune-${expected}`);
      assert.match(String(r?.note ?? ""), /no reclamation or preservation claimed/);
    }
  });

  it("bounds the error text it logs", async () => {
    const h = harness();
    h.state.result = exited(1, "", `Error response from daemon: ${"x".repeat(5000)}\nsecond line`);
    await h.step();
    await h.step();
    const r = rec(h.lines, "dind-builder-prune-rejected");
    assert.ok(typeof r?.error === "string");
    assert.ok(r.error.length <= 310, `error length ${r.error.length}`);
    assert.equal(r.error.includes("\n"), false);
  });

  it("completed logs before ratios, the parsed Total value only, then the after reading", async () => {
    const h = harness();
    h.state.result = exited(0, "Deleted build cache:\nsecret-record-id-123\n\nTotal:\t1.081MB\n");
    await h.step();
    h.state.onExec = async () => {
      h.state.bytes = 40;
      return undefined;
    };
    assert.equal(await h.step(), "completed");
    const all = JSON.stringify(h.lines);
    assert.equal(all.includes("secret-record-id-123"), false, "raw output is never logged");
    assert.equal(all.includes("Deleted build cache"), false);
    const done = rec(h.lines, "dind-builder-prune-completed worker=w1");
    assert.ok(done);
    assert.equal(done.reclaimed, "1.081MB");
    assert.equal(done.before_bytes_pct, 95);
    const after = rec(h.lines, "dind-builder-prune-after worker=w1");
    assert.equal(after?.bytes_pct, 40);
  });

  it("completed without a Total line logs no reclaimed field", async () => {
    const h = harness();
    h.state.result = exited(0, "garbage\nTotal reclaimed: nothing useful here\n");
    await h.step();
    await h.step();
    const done = rec(h.lines, "dind-builder-prune-completed worker=w1");
    assert.ok(done);
    assert.equal("reclaimed" in done, false);
  });

  it("the after reading is unavailable when no newer sample arrives within the wait", async () => {
    const h = harness();
    await h.step();
    h.state.sampleOverride = { epochS: Math.floor(h.now() / 1000) + 30, bytesUsed: 95, bytesTotal: 100, inodesUsed: 1, inodesTotal: 100 };
    h.advance(30_000);
    assert.equal(await h.leg.tick(), "completed");
    assert.deepEqual(rec(h.lines, "dind-builder-prune-after worker=w1")?.sample, "unavailable");
  });

  it("uses the api worker id in log tokens once known", async () => {
    const h = harness();
    h.leg.setWorkerId("wid-9");
    await h.step();
    await h.step();
    assert.ok(msgs(h.lines).includes("dind-builder-prune-completed worker=wid-9"));
  });

  it("is single-flight: a concurrent tick returns in-flight and execs nothing", async () => {
    const h = harness();
    let release!: () => void;
    const open = new Promise<void>((r) => (release = r));
    h.state.onExec = async () => {
      await open;
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
    assert.equal(h.calls.length, 1);
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
    assert.equal(h.calls.length, 2);
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
    h.state.result = exited(1, "", "error during connect");
    await h.step();
    assert.equal(await h.step(), "unknown");
    h.state.bytes = 10;
    assert.equal(await h.step(), "backoff");
    h.state.bytes = 95;
    for (let i = 0; i < 5; i++) assert.equal(await h.step(), "backoff");
    assert.equal(h.calls.length, 1, "no replacement prune within the hour under continued pressure");
    h.advance(60 * 60_000);
    assert.equal(await h.leg.tick(), "pending");
    assert.equal(await h.step(), "unknown");
    assert.equal(h.calls.length, 2);
  });
});

describe("maintenance fence", () => {
  it("defers with maintenance-fenced while maintenance holds the gate: no exec, streak kept, no backoff, one log", async () => {
    const gate = new SpyGate();
    const h = harness({ gate });
    await h.step();
    gate.close("maintenance");
    const generation = gate.generation();
    gate.calls.length = 0;
    for (let i = 0; i < 4; i++) assert.equal(await h.step(), "maintenance-fenced");
    assert.equal(h.calls.length, 0);
    assert.equal(gate.generation(), generation, "the leg only reads the gate");
    assert.deepEqual(gate.calls, []);
    assert.deepEqual(msgs(h.lines).filter((m) => m.includes("reason=maintenance-fenced")), ["dind-builder-prune-deferred worker=w1 reason=maintenance-fenced"]);
    gate.open("maintenance");
    assert.equal(await h.step(), "completed", "streak survived and no backoff was consumed");
    assert.equal(h.calls.length, 1);
  });

  it("a fence that appears after the first check still stops the exec (second, immediate check)", async () => {
    class LateFenceGate extends DindPruneGate {
      reads = 0;
      override heldBy(owner: Parameters<DindPruneGate["heldBy"]>[0]): boolean {
        if (owner !== "maintenance") return super.heldBy(owner);
        return ++this.reads > 1;
      }
    }
    const gate = new LateFenceGate();
    const h = harness({ gate });
    await h.step();
    assert.equal(await h.step(), "maintenance-fenced");
    assert.equal(h.calls.length, 0);
    assert.ok(gate.reads >= 2);
    // Streak kept, no backoff: with the fence gone the next evaluation runs the prune.
    gate.reads = -1_000_000;
    assert.equal(await h.step(), "completed");
    assert.equal(h.calls.length, 1);
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
    const mCalls: string[][] = [];
    const maintenance = new DindMaintenanceController({
      gate,
      dockerHost: HOST,
      heartbeatIntervalMs: 15_000,
      isIdle: () => true,
      log: h.logger,
      threshold: () => 0.85,
      readSample: () => null,
      exec: async (argv) => {
        mCalls.push([...argv]);
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

    maintenance.observe(operation, false, h.now());
    await maintenance.tick();
    assert.deepEqual(mCalls, [["version", "--format", "{{.Server.APIVersion}}"], ["volume", "prune", "-f"]]);
    assert.equal(h.calls.length, 0);

    maintenance.observe({ ...operation, phase: "complete" }, false, h.now());
    assert.equal(gate.heldBy("maintenance"), false);
    assert.equal(await h.step(), "pending");
    assert.equal(await h.step(), "completed");
    assert.deepEqual(h.calls.map((c) => c.argv), [FROZEN]);
  });
});

describe("loop (fake exec)", () => {
  it("never rejects when the meter reader throws", async () => {
    const h = harness();
    h.state.readThrows = true;
    const controller = new AbortController();
    const done = h.leg.loop(controller.signal);
    await new Promise((r) => setTimeout(r, 10));
    controller.abort();
    await done;
    assert.equal(h.calls.length, 0);
    assert.ok(msgs(h.lines).includes("dind-builder-prune: evaluation failed"));
  });

  it("never rejects when the logger throws", async () => {
    const h = harness();
    h.state.readThrows = true;
    const leg = new DindBuilderPruneController({
      dockerHost: HOST,
      workerName: "w1",
      gate: h.gate,
      log: { warn: () => { throw new Error("logger broke"); }, info: () => { throw new Error("logger broke"); } } as never,
      readSample: () => { throw new Error("meter exploded"); },
      sleep: async () => { await new Promise((r) => setImmediate(r)); },
      pluginPath: PLUGIN,
    });
    const controller = new AbortController();
    const done = leg.loop(controller.signal);
    await new Promise((r) => setTimeout(r, 10));
    controller.abort();
    await done;
  });
});

// ---- real-child cases: the leg's real spawn wrapper against node fixtures -------------------

function isAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
  } catch {
    return false;
  }
  try {
    // A zombie that nothing reaps still answers signal 0; it is dead for our purposes.
    return !/^\d+ \(.*\) Z/.test(readFileSync(`/proc/${pid}/stat`, "utf8"));
  } catch {
    return true;
  }
}

function reap(pid: number | undefined): void {
  if (pid === undefined) return;
  try {
    process.kill(pid, "SIGKILL");
  } catch {
    // ESRCH: already gone.
  }
}

async function waitFor(cond: () => boolean, what: string, maxMs = 8000): Promise<void> {
  const start = Date.now();
  while (!cond()) {
    if (Date.now() - start > maxMs) assert.fail(`timed out waiting for: ${what}`);
    await new Promise((r) => setTimeout(r, 10));
  }
}

function pids(stateFile: string): { child: number; grandchild: number } {
  return JSON.parse(readFileSync(stateFile, "utf8")) as { child: number; grandchild: number };
}

const spawnCount = (stateFile: string) => (existsSync(`${stateFile}.spawns`) ? readFileSync(`${stateFile}.spawns`, "utf8").split("\n").filter(Boolean).length : 0);

function realHarness(mode: string, extra: Partial<ConstructorParameters<typeof DindBuilderPruneController>[0]> = {}, gate?: DindPruneGate) {
  const stateFile = join(scratch(), "state.json");
  const prefix = [CHILD, mode, stateFile];
  const settled = { count: 0, waiters: [] as Array<() => void> };
  const h = harness({
    ...(gate ? { gate } : {}),
    real: {
      spawnFile: process.execPath,
      spawnArgvPrefix: prefix,
      pruneTimeoutMs: 60_000,
      settleTimeoutMs: 2000,
      onSettled: () => {
        settled.count++;
        for (const w of settled.waiters.splice(0)) w();
      },
      ...extra,
    },
  });
  const nextSettled = () => new Promise<void>((r) => settled.waiters.push(r));
  return { ...h, stateFile, prefix, nextSettled, settled };
}

describe("real child", () => {
  it("runs the real wrapper: exit 0 with a Total line is completed and the env is exactly the contract", async () => {
    const saved = process.env.UZI_BUILDER_PRUNE_SECRET;
    process.env.UZI_BUILDER_PRUNE_SECRET = "s3cret";
    try {
      const h = realHarness("env");
      await h.step();
      assert.equal(await h.step(), "completed");
      const env = JSON.parse(readFileSync(h.stateFile, "utf8")) as Record<string, string>;
      assert.deepEqual(env, { ...dockerExecEnvironment(HOST), DOCKER_BUILDKIT: "1" });
      configDirs.add(dockerExecEnvironment(HOST).DOCKER_CONFIG as string);
    } finally {
      if (saved === undefined) delete process.env.UZI_BUILDER_PRUNE_SECRET;
      else process.env.UZI_BUILDER_PRUNE_SECRET = saved;
    }
  });

  it("exit 0 prints Total: completed with the reclaimed value", async () => {
    const h = realHarness("exit0");
    await h.step();
    assert.equal(await h.step(), "completed");
    assert.equal(rec(h.lines, "dind-builder-prune-completed")?.reclaimed, "1.081MB");
  });

  for (const [mode, expected] of [
    ["daemon-error", "rejected"],
    ["daemon-conflict", "conflict"],
    ["buildx-missing", "refused"],
    ["plain-fail", "unknown"],
  ] as const) {
    it(`child output ${mode} -> ${expected}`, async () => {
      const h = realHarness(mode);
      await h.step();
      assert.equal(await h.step(), expected);
      assert.equal(spawnCount(h.stateFile), 1);
    });
  }

  it("a missing executable is a spawn error: refused", async () => {
    const h = realHarness("exit0", { spawnFile: join(scratch(), "no-such-docker"), spawnArgvPrefix: [] });
    await h.step();
    assert.equal(await h.step(), "refused");
    assert.equal(rec(h.lines, "dind-builder-prune-refused")?.reason, "spawn-failed");
  });

  it("abort: the group kill ends the child and its in-group grandchild; unknown; a positive control re-arms after 1h", async () => {
    const h = realHarness("sleep");
    const ac = new AbortController();
    await h.step(ac.signal);
    const attempt = h.step(ac.signal);
    await waitFor(() => existsSync(h.stateFile), "child started");
    const { child, grandchild } = pids(h.stateFile);
    try {
      ac.abort();
      assert.equal(await attempt, "unknown");
      await waitFor(() => !isAlive(child) && !isAlive(grandchild), "group dead");
      assert.equal(rec(h.lines, "dind-builder-prune-unknown")?.kill_reason, "abort");
      assert.equal(rec(h.lines, "dind-builder-prune-unsettled"), undefined);
      // Positive control: after the full hour with over samples exactly one more spawn happens.
      h.prefix[1] = "exit0";
      assert.equal(await h.step(), "backoff");
      h.advance(60 * 60_000);
      assert.equal(await h.leg.tick(), "pending");
      assert.equal(await h.step(), "completed");
      assert.equal(spawnCount(h.stateFile), 2);
    } finally {
      reap(child);
      reap(grandchild);
    }
  });

  it("timeout: the group kill ends child and grandchild; unknown with kill_reason timeout", async () => {
    const h = realHarness("sleep", { pruneTimeoutMs: 1000 });
    await h.step();
    const outcome = await h.step();
    const { child, grandchild } = pids(h.stateFile);
    try {
      assert.equal(outcome, "unknown");
      await waitFor(() => !isAlive(child) && !isAlive(grandchild), "group dead");
      assert.equal(rec(h.lines, "dind-builder-prune-unknown")?.kill_reason, "timeout");
      h.prefix[1] = "exit0";
      h.advance(60 * 60_000);
      assert.equal(await h.leg.tick(), "pending");
      assert.equal(await h.step(), "completed");
      assert.equal(spawnCount(h.stateFile), 2);
    } finally {
      reap(child);
      reap(grandchild);
    }
  });

  it("a killed client stays unknown even when its partial stderr carries daemon text", async () => {
    const h = realHarness("daemon-then-sleep", { pruneTimeoutMs: 1000 });
    await h.step();
    assert.equal(await h.step(), "unknown");
    const { child, grandchild } = pids(h.stateFile);
    reap(child);
    reap(grandchild);
    assert.equal(rec(h.lines, "dind-builder-prune-rejected"), undefined);
    // Full backoff: a below-75% sample does not re-arm.
    h.state.bytes = 10;
    assert.equal(await h.step(), "backoff");
  });

  it("latch: an escaped pipe holder leaves the leg unsettled, spawning nothing, until close arrives", async () => {
    const gate = new SpyGate();
    const h = realHarness("escape", { settleTimeoutMs: 200 }, gate);
    const ac = new AbortController();
    let escapee: number | undefined;
    try {
      await h.step();
      const attempt = h.step(ac.signal);
      await waitFor(() => existsSync(h.stateFile), "child started");
      escapee = pids(h.stateFile).grandchild;
      ac.abort();
      assert.equal(await attempt, "unknown");
      assert.equal(spawnCount(h.stateFile), 1);
      assert.ok(isAlive(escapee), "the escapee survived the group kill");
      assert.equal((h.lines as Array<{ msg: string }>).filter((l) => l.msg.startsWith("dind-builder-prune-unsettled")).length, 1);
      assert.equal(rec(h.lines, "dind-builder-prune-unknown")?.reason, "settlement-unconfirmed");

      for (const jump of [0, 61 * 60_000, 61 * 60_000]) {
        h.advance(jump);
        assert.equal(await h.step(), "unsettled");
        assert.equal(await h.step(), "unsettled");
      }
      assert.equal(spawnCount(h.stateFile), 1, "no further spawn while unsettled");
      assert.deepEqual(gate.calls, []);
      assert.equal(gate.generation(), 0);
      assert.equal((h.lines as Array<{ msg: string }>).filter((l) => l.msg.startsWith("dind-builder-prune-unsettled")).length, 1, "logged once");

      const settled = h.nextSettled();
      process.kill(escapee, "SIGKILL");
      await settled;
      const done = rec(h.lines, "dind-builder-prune-settled worker=w1");
      assert.ok(done);
      assert.equal(typeof done.after_ms, "number");
      // Latch cleared: two fresh over samples trigger exactly one new spawn.
      h.prefix[1] = "exit0";
      assert.equal(await h.step(), "pending");
      assert.equal(await h.step(), "completed");
      assert.equal(spawnCount(h.stateFile), 2);
      assert.deepEqual(gate.calls, []);
    } finally {
      reap(escapee);
    }
  });

  it("worker exit: a process holding an unsettled child still exits by itself", async () => {
    const stateFile = join(scratch(), "state.json");
    const child = spawn(process.execPath, ["--import", "tsx", RUNNER, stateFile], {
      cwd: AGENT_DIR,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    child.stdout.on("data", (c: Buffer) => (stdout += c.toString()));
    let escapee: number | undefined;
    try {
      const code = await new Promise<number | null | "hung">((resolve) => {
        const t = setTimeout(() => resolve("hung"), 5000);
        child.once("exit", (c) => {
          clearTimeout(t);
          resolve(c);
        });
      });
      if (existsSync(stateFile)) escapee = pids(stateFile).grandchild;
      assert.equal(code, 0, `runner did not exit by itself: ${String(code)}`);
      const result = JSON.parse(stdout.trim().split("\n").pop()!) as { outcome: string; unsettled: boolean };
      assert.deepEqual(result, { outcome: "unknown", unsettled: true });
    } finally {
      reap(escapee);
      reap(child.pid);
    }
  });

  it("loop: abort with an in-flight real child returns within the settle bound plus slack", async () => {
    const h = realHarness("sleep", { settleTimeoutMs: 300 });
    const ac = new AbortController();
    const done = h.leg.loop(ac.signal);
    let pidsSeen: { child: number; grandchild: number } | undefined;
    try {
      await waitFor(() => existsSync(h.stateFile), "child started");
      pidsSeen = pids(h.stateFile);
      const t0 = Date.now();
      ac.abort();
      await done;
      assert.ok(Date.now() - t0 < 300 + 2000, `loop took ${Date.now() - t0}ms`);
      await waitFor(() => !isAlive(pidsSeen!.child) && !isAlive(pidsSeen!.grandchild), "group dead");
      assert.equal(spawnCount(h.stateFile), 1);
    } finally {
      ac.abort();
      await done;
      reap(pidsSeen?.child);
      reap(pidsSeen?.grandchild);
    }
  });
});
