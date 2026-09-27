import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, rmSync, statSync } from "node:fs";
import { homedir, tmpdir } from "node:os";

import {
  DIND_PRUNE_ARGV,
  DOCKER_BIN,
  DindPruneController,
  DindPruneGate,
  assertDockerArgvAllowed,
  createDindPrune,
  type DockerExec,
} from "../src/dind-prune.js";
import { readDindMeterSample } from "../src/dind-meter.js";
import { loadConfig } from "../src/config.js";
import { recordingLogger } from "./helpers.js";

// issue #1759 M3: the DinD prune controller, driven with a fake clock (the injected
// sleep advances it), a fake meter file read through the REAL parser, a recording exec
// spy and a real claim gate. Each test states the pressure/custody/idle inputs and
// asserts exactly which docker argv ran.

const FORBIDDEN = ["volume", "volumes", "--volumes", "system", "container", "rm"];
/** DOCKER_CONFIG directories the controllers created, removed after the file. */
const configDirs = new Set<string>();
after(() => {
  for (const d of configDirs) rmSync(d, { recursive: true, force: true });
});

/** Every argv any controller in this file executed, checked by the last test. */
const ALL_EXECUTED: string[][] = [];

const HB_MS = 15_000;
const TICK = 30_000;

interface Meter {
  bytesPct: number;
  inodesPct: number;
  /** Fixed epoch (seconds); undefined = "sampled now". */
  frozenEpoch?: number;
  /** Raw override (e.g. malformed); null = file missing. */
  raw?: string | null;
}

function harness(opts: { workerName?: string } = {}) {
  let nowMs = 1_700_000_000_000;
  const clock = {
    now: () => nowMs,
    advance: (ms: number) => {
      nowMs += ms;
    },
  };
  const meter: Meter = { bytesPct: 10, inodesPct: 10 };
  const line = (): string | null => {
    if (meter.raw !== undefined) return meter.raw;
    const epoch = meter.frozenEpoch ?? Math.floor(nowMs / 1000);
    const bfree = 1000 - Math.round(meter.bytesPct * 10);
    const ffree = 1000 - Math.round(meter.inodesPct * 10);
    return `v1 ${epoch} 4096 1000 ${bfree} 1000 ${ffree}\n`;
  };
  const calls: string[][] = [];
  const envs: NodeJS.ProcessEnv[] = [];
  const files: string[] = [];
  const timeouts: number[] = [];
  const state = {
    psOut: "",
    idle: true,
    /** Called after each exec is recorded; may throw to fail the command. */
    onExec: undefined as ((argv: readonly string[]) => void | Promise<void>) | undefined,
    /** Called at each injected sleep (after the clock advanced). */
    onSleep: undefined as ((ms: number) => void) | undefined,
  };
  const exec: DockerExec = async (argv, o) => {
    calls.push([...argv]);
    ALL_EXECUTED.push([...argv]);
    envs.push(o.env);
    files.push(o.file);
    timeouts.push(o.timeoutMs);
    await state.onExec?.(argv);
    return argv[0] === "ps" ? state.psOut : "";
  };
  const gate = new DindPruneGate();
  const { logger, lines } = recordingLogger();
  const ctl = new DindPruneController({
    dockerHost: "tcp://127.0.0.1:2375",
    heartbeatIntervalMs: HB_MS,
    workerName: opts.workerName ?? "w-name",
    gate,
    isIdle: () => state.idle,
    log: logger,
    readSample: () => readDindMeterSample({ readFile: () => line(), nowMs: clock.now }),
    exec,
    now: clock.now,
    sleep: async (ms) => {
      clock.advance(ms);
      state.onSleep?.(ms);
    },
  });
  /** One heartbeat-then-evaluate step: advance a tick, record custody (as the heartbeat
   *  would), then tick the controller. `custody: "skip"` records nothing. */
  const step = async (...args: [] | [boolean | undefined | "skip"]) => {
    // Not a default parameter: an explicit `undefined` must reach recordCustody.
    const custody = args.length === 0 ? false : args[0];
    clock.advance(TICK);
    if (custody !== "skip") ctl.recordCustody(custody);
    return ctl.tick();
  };
  const msgs = () => (lines as Array<{ msg: string }>).map((l) => l.msg);
  const records = () => lines as Array<{ level: string; msg: string } & Record<string, unknown>>;
  return { clock, meter, calls, envs, files, timeouts, state, gate, ctl, step, msgs, records, lines };
}

const DANGLING = [...DIND_PRUNE_ARGV.pruneDangling];
const BUILDER = [...DIND_PRUNE_ARGV.pruneBuildCache];
const UNUSED = [...DIND_PRUNE_ARGV.pruneUnusedImages];
const PS = [...DIND_PRUNE_ARGV.listRunning];

/** Make the meter drop to `pct` (both) once the build-cache prune runs. */
function relieveAfterBuilder(h: ReturnType<typeof harness>, pct: number): void {
  h.state.onExec = (argv) => {
    if (argv[0] === "builder") {
      h.meter.bytesPct = pct;
      h.meter.inodesPct = pct;
    }
  };
}

describe("DinD prune — pressure trigger and debounce", () => {
  it("inode-only pressure triggers after two fresh over samples (bytes 10%, inodes 95%)", async () => {
    const h = harness();
    h.meter.bytesPct = 10;
    h.meter.inodesPct = 95;
    relieveAfterBuilder(h, 10);
    assert.strictEqual(await h.step(), "pending", "one over sample is not enough");
    assert.deepStrictEqual(h.calls, []);
    assert.strictEqual(await h.step(), "completed");
    assert.deepStrictEqual(h.calls, [PS, DANGLING, BUILDER], "ps, then the two first-line prunes, no third step");
  });

  it("byte pressure alone triggers too", async () => {
    const h = harness();
    h.meter.bytesPct = 90;
    relieveAfterBuilder(h, 10);
    await h.step();
    assert.strictEqual(await h.step(), "completed");
  });

  it("an over sample followed by a below sample resets the streak", async () => {
    const h = harness();
    h.meter.bytesPct = 90;
    assert.strictEqual(await h.step(), "pending");
    h.meter.bytesPct = 50;
    assert.strictEqual(await h.step(), "below");
    h.meter.bytesPct = 90;
    assert.strictEqual(await h.step(), "pending", "the streak restarted at one");
    assert.deepStrictEqual(h.calls, []);
  });

  it("a missing, invalid, stale or future sample never triggers and resets the streak", async () => {
    const variants: Array<[string, (h: ReturnType<typeof harness>) => void]> = [
      ["missing", (h) => (h.meter.raw = null)],
      ["invalid", (h) => (h.meter.raw = "v1 garbage\n")],
      ["bfree>blocks", (h) => (h.meter.raw = `v1 ${Math.floor(h.clock.now() / 1000)} 4096 1000 1001 1000 10\n`)],
      ["stale", (h) => (h.meter.frozenEpoch = Math.floor(h.clock.now() / 1000) - 91)],
      ["future", (h) => (h.meter.frozenEpoch = Math.floor(h.clock.now() / 1000) + 60)],
    ];
    for (const [name, spoil] of variants) {
      const h = harness();
      h.meter.inodesPct = 95;
      assert.strictEqual(await h.step(), "pending", name);
      h.clock.advance(TICK);
      spoil(h);
      h.ctl.recordCustody(false);
      assert.strictEqual(await h.ctl.tick(), "no-sample", `${name}: rejected`);
      h.meter.raw = undefined;
      h.meter.frozenEpoch = undefined;
      assert.strictEqual(await h.step(), "pending", `${name}: the streak was reset, one fresh over sample is not enough`);
      assert.deepStrictEqual(h.calls, [], `${name}: nothing executed`);
    }
  });

  it("the same sample epoch read twice counts once", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    h.meter.frozenEpoch = Math.floor(h.clock.now() / 1000) + 30;
    assert.strictEqual(await h.step(), "pending");
    // Re-read the SAME file 5s later (still fresh): must not count as a second sample.
    h.clock.advance(5_000);
    h.ctl.recordCustody(false);
    assert.strictEqual(await h.ctl.tick(), "pending");
    h.clock.advance(5_000);
    h.ctl.recordCustody(false);
    assert.strictEqual(await h.ctl.tick(), "pending");
    assert.deepStrictEqual(h.calls, []);
    // A genuinely new sample is the second one.
    h.meter.frozenEpoch = undefined;
    relieveAfterBuilder(h, 10);
    assert.strictEqual(await h.step(), "completed");
  });
});

describe("DinD prune — debounce gap", () => {
  it("an over sample more than 45s after the previous counted one restarts the streak at one", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    assert.strictEqual(await h.step(), "pending");
    // The meter stalled: the next distinct sample is 60s newer, not one interval.
    h.clock.advance(30_000);
    assert.strictEqual(await h.step(), "pending", "a 60s gap is not consecutive");
    assert.deepStrictEqual(h.calls, []);
    relieveAfterBuilder(h, 10);
    assert.strictEqual(await h.step(), "completed", "the next 30s sample completes the new streak");
  });

  it("a sample 45s after the previous one still counts as consecutive", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    assert.strictEqual(await h.step(), "pending");
    h.clock.advance(15_000);
    relieveAfterBuilder(h, 10);
    assert.strictEqual(await h.step(), "completed");
  });
});

describe("DinD prune — hysteresis and backoff", () => {
  it("does not re-trigger while above the low watermark, re-arms below 75%", async () => {
    const h = harness();
    h.meter.bytesPct = 90;
    relieveAfterBuilder(h, 80); // relieved below 85 but not below 75
    await h.step();
    assert.strictEqual(await h.step(), "completed");
    h.state.onExec = undefined;
    const executed = h.calls.length;
    h.meter.bytesPct = 90;
    h.meter.inodesPct = 90;
    for (let i = 0; i < 10; i++) assert.strictEqual(await h.step(), "backoff");
    assert.strictEqual(h.calls.length, executed, "no command during backoff");
    // Below 75 on bytes only is not enough: inodes still 80.
    h.meter.bytesPct = 70;
    h.meter.inodesPct = 80;
    assert.strictEqual(await h.step(), "backoff");
    h.meter.inodesPct = 70;
    assert.strictEqual(await h.step(), "below", "re-armed below the low watermark on both");
    h.meter.bytesPct = 90;
    relieveAfterBuilder(h, 10);
    assert.strictEqual(await h.step(), "pending");
    assert.strictEqual(await h.step(), "completed");
  });

  it("re-arms after the 1h backoff even while still over", async () => {
    const h = harness();
    h.meter.bytesPct = 90;
    relieveAfterBuilder(h, 80);
    await h.step();
    assert.strictEqual(await h.step(), "completed");
    h.state.onExec = undefined;
    h.meter.bytesPct = 90;
    const executed = h.calls.length;
    let outcome = "";
    let ticks = 0;
    // 30s ticks: the first 119 (< 1h) are all backoff.
    while ((outcome = await h.step()) === "backoff") ticks++;
    assert.ok(ticks >= 118 && ticks <= 120, `backoff lasted ~1h of ticks (got ${ticks})`);
    assert.strictEqual(outcome, "pending", "re-armed, and needs a fresh streak");
    assert.strictEqual(h.calls.length, executed);
    assert.notStrictEqual(await h.step(), "backoff");
  });
});

describe("DinD prune — custody (fail-closed)", () => {
  for (const [name, custody] of [
    ["true", true],
    ["undefined", undefined],
  ] as const) {
    it(`retaining_unpublished_work ${name} → no prune`, async () => {
      const h = harness();
      h.meter.bytesPct = 95;
      await h.step(custody);
      assert.strictEqual(await h.step(custody), "custody");
      assert.strictEqual(await h.step(custody), "custody");
      assert.deepStrictEqual(h.calls, []);
      const skips = h.msgs().filter((m) => m.includes("custody is not provably clear"));
      assert.strictEqual(skips.length, 1, "logged once per episode");
    });
  }

  it("a stale custody flag (older than two heartbeat intervals) → no prune", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    h.ctl.recordCustody(false);
    await h.step("skip"); // 30s after the only heartbeat: exactly 2 intervals, still fresh
    h.clock.advance(1);
    h.meter.frozenEpoch = Math.floor(h.clock.now() / 1000) + 1;
    assert.strictEqual(await h.ctl.tick(), "custody");
    assert.deepStrictEqual(h.calls, []);
  });

  it("a custody flag sent before the last run ended is not trusted; one sent after is", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    await h.step();
    // A run ends at t; the only custody sample was sent at t-5s (fresh, false).
    h.clock.advance(TICK);
    const t = h.clock.now();
    h.ctl.recordCustody(false, t - 5_000);
    h.ctl.noteActivityEnded();
    assert.strictEqual(await h.ctl.tick(), "custody", "a pre-run-end flag may predate the hold the run opened");
    assert.deepStrictEqual(h.calls, []);
    // A heartbeat sent at t+5s says false: now the prune may go.
    h.clock.advance(5_000);
    h.ctl.recordCustody(false, t + 5_000);
    h.clock.advance(TICK - 5_000);
    relieveAfterBuilder(h, 10);
    assert.strictEqual(await h.ctl.tick(), "completed");
  });

  it("isIdle observed false makes an earlier custody flag untrusted", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    h.state.idle = false;
    await h.step();
    assert.strictEqual(await h.step(), "busy");
    // The run ends between ticks; no heartbeat since the busy observation.
    h.state.idle = true;
    h.clock.advance(TICK);
    assert.strictEqual(await h.ctl.tick(), "custody");
    assert.deepStrictEqual(h.calls, []);
    relieveAfterBuilder(h, 10);
    assert.strictEqual(await h.step(), "completed", "a heartbeat after the observation clears it");
  });

  it("no heartbeat ever → no prune", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    await h.step("skip");
    assert.strictEqual(await h.step("skip"), "custody");
    assert.deepStrictEqual(h.calls, []);
  });

  it("the re-check under the held gate catches a flip to true", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    await h.step();
    // A claim is in flight when the gate closes; while the prune waits for it, a
    // heartbeat lands saying the worker now retains work, then the claim ends empty.
    assert.ok(h.gate.tryEnterClaim());
    h.state.onSleep = () => {
      if (h.gate.inFlightClaims() > 0) {
        h.ctl.recordCustody(true);
        h.gate.exitClaim();
      }
    };
    assert.strictEqual(await h.step(), "busy");
    assert.deepStrictEqual(h.calls, [], "not even docker ps ran");
    assert.strictEqual(h.gate.claimsClosed(), false, "gate released");
  });
});

describe("DinD prune — exclusive idle acquisition", () => {
  it("a claim still in flight after 60s aborts the attempt and releases the gate", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    await h.step();
    assert.ok(h.gate.tryEnterClaim());
    let closedWhileWaiting = false;
    h.state.onSleep = () => {
      closedWhileWaiting ||= h.gate.claimsClosed();
    };
    const t0 = h.clock.now();
    assert.strictEqual(await h.step(), "busy");
    assert.ok(closedWhileWaiting, "claims were closed while waiting");
    assert.ok(h.clock.now() - t0 - TICK >= 60_000, "waited the full bound");
    assert.ok(h.clock.now() - t0 - TICK < 61_000, "and no longer");
    assert.strictEqual(h.gate.claimsClosed(), false);
    assert.deepStrictEqual(h.calls, []);
    // Not a consumed attempt: once the claim ends, the prune goes without a backoff. The
    // 60s drain left a 90s gap since the last counted sample, so the streak restarts first.
    h.gate.exitClaim();
    relieveAfterBuilder(h, 10);
    assert.strictEqual(await h.step(), "pending", "a 90s sample gap is not consecutive");
    assert.strictEqual(await h.step(), "completed");
  });

  it("an in-flight claim that turns into a run aborts the prune: nothing executes while a run is active", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    await h.step();
    assert.ok(h.gate.tryEnterClaim());
    h.state.onSleep = () => {
      if (h.gate.inFlightClaims() > 0) {
        h.state.idle = false; // the claimed run is now in the active set…
        h.gate.exitClaim(); // …and only then does the claim exit
      }
    };
    assert.strictEqual(await h.step(), "busy");
    assert.deepStrictEqual(h.calls, []);
    // While the run stays active, no tick executes anything and the gate stays open.
    h.state.onSleep = undefined;
    for (let i = 0; i < 5; i++) {
      assert.strictEqual(await h.step(), "busy");
      assert.strictEqual(h.gate.claimsClosed(), false);
    }
    assert.deepStrictEqual(h.calls, []);
  });

  it("an active chat or run (isIdle false) → no prune", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    h.state.idle = false;
    await h.step();
    assert.strictEqual(await h.step(), "busy");
    assert.deepStrictEqual(h.calls, []);
  });

  it("a running container (docker ps -q prints an id) → no prune, retried next tick", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    h.state.psOut = "3f2a9c1b7d4e\n";
    await h.step();
    assert.strictEqual(await h.step(), "busy");
    assert.strictEqual(await h.step(), "busy");
    assert.deepStrictEqual(h.calls, [PS, PS], "only docker ps ran");
    assert.strictEqual(h.gate.claimsClosed(), false);
    h.state.psOut = "";
    relieveAfterBuilder(h, 10);
    assert.strictEqual(await h.step(), "completed", "no backoff was consumed");
  });

  it("a running-container deferral is logged at INFO once per pressure episode", async () => {
    const h = harness({ workerName: "w-name" });
    h.meter.bytesPct = 95;
    h.state.psOut = "3f2a9c1b7d4e\n";
    await h.step();
    for (let i = 0; i < 5; i++) assert.strictEqual(await h.step(), "busy");
    const token = "dind-prune-deferred worker=w-name reason=running-container";
    const logged = () => h.records().filter((l) => l.msg === token);
    assert.strictEqual(logged().length, 1, "once, not per tick");
    assert.strictEqual(logged()[0]?.level, "info");
    // The episode ends (below), a new one starts: logged again, once.
    h.meter.bytesPct = 50;
    assert.strictEqual(await h.step(), "below");
    h.meter.bytesPct = 95;
    await h.step();
    for (let i = 0; i < 3; i++) assert.strictEqual(await h.step(), "busy");
    assert.strictEqual(logged().length, 2);
  });

  it("docker ps runs with a 30s timeout", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    relieveAfterBuilder(h, 10);
    await h.step();
    assert.strictEqual(await h.step(), "completed");
    assert.deepStrictEqual(h.calls[0], PS);
    assert.strictEqual(h.timeouts[0], 30_000);
  });

  it("three consecutive docker ps failures enter the 1h backoff and log dind-prune-failed once", async () => {
    const h = harness({ workerName: "w-name" });
    h.meter.bytesPct = 95;
    h.state.onExec = (argv) => {
      if (argv[0] === "ps") throw new Error("Cannot connect to the Docker daemon");
    };
    await h.step();
    assert.strictEqual(await h.step(), "busy");
    assert.strictEqual(await h.step(), "busy");
    assert.strictEqual(await h.step(), "failed");
    assert.deepStrictEqual(h.calls, [PS, PS, PS]);
    assert.strictEqual(h.gate.claimsClosed(), false);
    for (let i = 0; i < 20; i++) assert.strictEqual(await h.step(), "backoff");
    assert.strictEqual(h.calls.length, 3, "the claim gate is no longer closed every tick");
    const failed = h.records().filter((l) => l.msg === "dind-prune-failed worker=w-name");
    assert.strictEqual(failed.length, 1);
    assert.strictEqual(failed[0]?.reason, "docker-ps-failed");
    assert.strictEqual(
      h.msgs().filter((m) => m === "dind-prune-deferred worker=w-name reason=docker-ps-failed").length,
      1,
    );
  });

  it("a successful docker ps resets the consecutive-failure count", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    let fail = true;
    h.state.onExec = (argv) => {
      if (argv[0] === "ps" && fail) throw new Error("timeout");
    };
    await h.step();
    assert.strictEqual(await h.step(), "busy");
    assert.strictEqual(await h.step(), "busy");
    fail = false;
    h.state.psOut = "3f2a9c1b7d4e\n";
    assert.strictEqual(await h.step(), "busy", "ps succeeded (a running container)");
    fail = true;
    h.state.psOut = "";
    assert.strictEqual(await h.step(), "busy");
    assert.strictEqual(await h.step(), "busy", "the count restarted after the success");
    assert.strictEqual(await h.step(), "failed");
  });

  it("holds the gate through the prune commands", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    const closedAt: boolean[] = [];
    h.state.onExec = (argv) => {
      closedAt.push(h.gate.claimsClosed());
      if (argv[0] === "builder") h.meter.bytesPct = 10;
    };
    await h.step();
    assert.strictEqual(await h.step(), "completed");
    assert.deepStrictEqual(closedAt, [true, true, true]);
    assert.strictEqual(h.gate.claimsClosed(), false);
  });
});

describe("DinD prune — commands and outcomes", () => {
  it("runs the third step only when a fresh post-prune sample is still over", async () => {
    const h = harness({ workerName: "w-name" });
    h.meter.bytesPct = 95;
    h.state.onExec = (argv) => {
      if (argv[0] === "image" && argv.includes("-a")) h.meter.bytesPct = 50;
    };
    await h.step();
    assert.strictEqual(await h.step(), "completed");
    assert.deepStrictEqual(h.calls, [PS, DANGLING, BUILDER, UNUSED]);
    assert.ok(h.msgs().includes("dind-prune-completed worker=w-name"));
  });

  it("does not run the third step when no fresh post-prune sample arrives", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    await h.step();
    // Freeze the meter at the pre-prune sample: nothing newer than the prune ever lands.
    h.meter.frozenEpoch = Math.floor(h.clock.now() / 1000) + TICK / 1000;
    assert.strictEqual(await h.step(), "completed");
    assert.deepStrictEqual(h.calls, [PS, DANGLING, BUILDER]);
  });

  it("still over after all three steps → dind-prune-insufficient, then backoff", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    h.ctl.setWorkerId("wid-7");
    await h.step();
    assert.strictEqual(await h.step(), "insufficient");
    assert.deepStrictEqual(h.calls, [PS, DANGLING, BUILDER, UNUSED]);
    assert.ok(h.msgs().includes("dind-prune-insufficient worker=wid-7"), "worker id in the token");
    assert.strictEqual(await h.step(), "backoff");
    assert.strictEqual(h.calls.length, 4);
  });

  it("a command failure releases the gate, logs dind-prune-failed once, and does not retry before backoff", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    h.state.onExec = (argv) => {
      if (argv[0] === "builder") throw new Error("Command failed: docker builder prune -f");
    };
    await h.step();
    assert.strictEqual(await h.step(), "failed");
    assert.deepStrictEqual(h.calls, [PS, DANGLING, BUILDER]);
    assert.strictEqual(h.gate.claimsClosed(), false);
    for (let i = 0; i < 20; i++) assert.strictEqual(await h.step(), "backoff");
    assert.strictEqual(h.calls.length, 3, "no retry loop");
    assert.strictEqual(h.msgs().filter((m) => m === "dind-prune-failed worker=w-name").length, 1);
  });

  it("a timed-out command is a failure too (per-command timeout ≤ 5 min)", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    h.state.onExec = (argv) => {
      if (argv[0] === "image") {
        const err = new Error("spawnSync docker ETIMEDOUT") as Error & { killed: boolean };
        err.killed = true;
        throw err;
      }
    };
    await h.step();
    assert.strictEqual(await h.step(), "failed");
    assert.ok(h.timeouts.every((t) => t > 0 && t <= 5 * 60_000));
    assert.strictEqual(h.gate.claimsClosed(), false);
    assert.strictEqual(await h.step(), "backoff");
  });

  it("an abort while waiting for the post-prune sample logs aborted, never completed", async () => {
    const h = harness({ workerName: "w-name" });
    h.meter.bytesPct = 95;
    await h.step();
    h.meter.frozenEpoch = Math.floor(h.clock.now() / 1000) + TICK / 1000;
    const ac = new AbortController();
    h.state.onSleep = () => ac.abort(); // shutdown lands inside waitFreshSample's wait
    h.clock.advance(TICK);
    h.ctl.recordCustody(false);
    assert.strictEqual(await h.ctl.tick(ac.signal), "aborted");
    assert.deepStrictEqual(h.calls, [PS, DANGLING, BUILDER]);
    assert.ok(!h.msgs().some((m) => m.startsWith("dind-prune-completed")), "no completed token on shutdown");
    assert.ok(h.msgs().includes("dind-prune: interrupted by shutdown"));
  });

  it("abandons the remaining steps once the 15 min budget is spent", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    h.state.onExec = (argv) => {
      if (argv[0] === "image") h.clock.advance(16 * 60_000); // the first prune ate the budget
    };
    await h.step();
    assert.strictEqual(await h.step(), "failed");
    assert.deepStrictEqual(h.calls, [PS, DANGLING], "the build-cache prune never started");
  });

  it("runs /usr/bin/docker with a fixed PATH, an empty 0700 DOCKER_CONFIG/HOME and the wired DOCKER_HOST", async () => {
    const h = harness();
    h.meter.bytesPct = 95;
    relieveAfterBuilder(h, 10);
    await h.step();
    await h.step();
    assert.strictEqual(DOCKER_BIN, "/usr/bin/docker");
    assert.ok(h.files.length > 0);
    for (const f of h.files) assert.strictEqual(f, "/usr/bin/docker", "never resolved through PATH");
    assert.ok(h.envs.length > 0);
    for (const env of h.envs) {
      assert.strictEqual(env.DOCKER_HOST, "tcp://127.0.0.1:2375");
      assert.deepStrictEqual(Object.keys(env).sort(), ["DOCKER_CONFIG", "DOCKER_HOST", "HOME", "PATH"]);
      assert.strictEqual(env.PATH, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin");
      const dir = env.DOCKER_CONFIG as string;
      assert.strictEqual(env.HOME, dir);
      assert.notStrictEqual(dir, homedir());
      assert.ok(dir.startsWith(tmpdir()), dir);
      const st = statSync(dir);
      assert.ok(st.isDirectory());
      assert.strictEqual(st.mode & 0o777, 0o700);
      assert.deepStrictEqual(readdirSync(dir), [], "empty: no config.json, no cli-plugins");
      configDirs.add(dir);
    }
    assert.strictEqual(new Set(h.envs.map((e) => e.DOCKER_CONFIG)).size, 1, "created once");
  });
});

describe("DinD prune — enablement", () => {
  const cfg = (over: Record<string, unknown>) =>
    ({ heartbeatIntervalMs: HB_MS, workerName: "w", dindPruneEnabled: true, dockerWiring: { dockerHost: "tcp://d:2375" }, ...over }) as never;
  const deps = () => ({ gate: new DindPruneGate(), isIdle: () => true, log: recordingLogger().logger });

  it("is built only when enabled AND a dockerHost is wired", () => {
    assert.ok(createDindPrune(cfg({}), deps()) instanceof DindPruneController);
    assert.strictEqual(createDindPrune(cfg({ dindPruneEnabled: false }), deps()), undefined);
    assert.strictEqual(createDindPrune(cfg({ dockerWiring: {} }), deps()), undefined);
    assert.strictEqual(createDindPrune(cfg({ dockerWiring: { dockerHost: "" } }), deps()), undefined);
  });

  it("UZI_DIND_PRUNE_ENABLED enables only on the exact string \"true\"", () => {
    const env = (v?: string): NodeJS.ProcessEnv => ({
      UZI_API_URL: "http://api:8080",
      UZI_WORKER_TOKEN: "worker-join-token-0123456789",
      ...(v === undefined ? {} : { UZI_DIND_PRUNE_ENABLED: v }),
    });
    assert.strictEqual(loadConfig(env("true")).dindPruneEnabled, true);
    for (const v of [undefined, "", "false", "TRUE", "1", "yes", " true"]) {
      assert.strictEqual(loadConfig(env(v)).dindPruneEnabled, false, `value ${JSON.stringify(v)}`);
    }
  });
});

describe("DinD prune — the argv allowlist", () => {
  it("the frozen table carries no forbidden token and cannot be mutated", () => {
    for (const argv of Object.values(DIND_PRUNE_ARGV)) {
      for (const t of argv) assert.ok(!FORBIDDEN.includes(t.toLowerCase()), `table token ${t}`);
      assert.ok(Object.isFrozen(argv));
    }
    assert.ok(Object.isFrozen(DIND_PRUNE_ARGV));
  });

  it("the runtime guard refuses forbidden or unlisted argv and admits the table", () => {
    for (const bad of [
      ["volume", "prune", "-f"],
      ["system", "prune", "-a", "-f"],
      ["image", "prune", "-f", "--volumes"],
      ["image", "prune", "-f", "--volumes=true"],
      ["container", "prune", "-f"],
      ["rm", "-f", "abc"],
      ["SYSTEM", "prune"],
      ["image", "ls"],
      ["image", "prune"],
    ]) {
      assert.throws(() => assertDockerArgvAllowed(bad), /refusing/, JSON.stringify(bad));
    }
    for (const ok of Object.values(DIND_PRUNE_ARGV)) assertDockerArgvAllowed(ok);
  });

  it("across every scenario above, no executed argv held a forbidden token", () => {
    assert.ok(ALL_EXECUTED.length > 0);
    for (const argv of ALL_EXECUTED) {
      for (const t of argv) assert.ok(!FORBIDDEN.includes(t.toLowerCase()), `executed ${JSON.stringify(argv)}`);
      assertDockerArgvAllowed(argv);
    }
  });
});
