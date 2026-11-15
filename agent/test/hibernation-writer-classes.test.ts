import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { rmSync } from "node:fs";
import { DindMaintenanceController } from "../src/dind-maintenance.js";
import { DindPruneGate, dockerExecEnvironment } from "../src/dind-prune.js";
import { DindVolumePruneController } from "../src/dind-volume-prune.js";
import { DiskPressureController } from "../src/disk-reclaim.js";
import { Worker } from "../src/worker.js";
import { hibernationWriters } from "../src/hibernation-inventory.js";
import { nullLogger } from "./helpers.js";

// A prune leg under test builds its child env once per process, which creates the shared
// empty DOCKER_CONFIG dir in tmpdir; remove it so tmpdir-leak-guard stays clean.
after(() => { rmSync(dockerExecEnvironment("unix:///unused").DOCKER_CONFIG as string, { recursive: true, force: true }); });

// PRD #4208 M1: production entry points of writer classes the wiring test does not drive (dind_prune is in
// hibernation-wiring, recovery_pass in hibernation-recovery-writers, live_settle in runner-recovery-settlement).
// Each runs through its real entry with the process-wide registry latched and must not start its write pass.
// Not driven here (their entries need the full boot, terminal-rejection or retained-source harness): recovery_resume,
// boot_terminals, terminal_rejections and retained_ops.

const HOST = "tcp://127.0.0.1:2375";

describe("writer classes refuse to start while the registry is latched", () => {
  it("dind_maintenance: the loop skips its tick", async (t) => {
    t.after(() => hibernationWriters.unlatch());
    let execs = 0;
    let time = 1_700_000_000_000;
    let current = new AbortController();
    const ctl = new DindMaintenanceController({
      gate: new DindPruneGate(), dockerHost: HOST, heartbeatIntervalMs: 15_000, isIdle: () => true, log: nullLogger(),
      threshold: () => 0.5, readSample: () => null, exec: async () => { execs++; return "1.43"; },
      now: () => time, sleep: async () => { current.abort(); },
    });
    // A fenced, ready operation bound to this registration: the one state in which a tick would run docker.
    ctl.register("n", true);
    ctl.observe({ id: "i", nonce: "o", deployment_uid: "d", pvc_uid: "p", register_nonce: "n", phase: "ready", fenced: true, ready_ack: false } as never,
      false, time + 5); // custody sampled strictly after the fence closed
    time += 10;
    hibernationWriters.latch();
    await ctl.loop(current.signal);
    assert.equal(execs, 0, "no tick while latched");
    hibernationWriters.unlatch();
    current = new AbortController();
    await ctl.loop(current.signal);
    assert.ok(execs > 0, "the tick runs once unlatched");
  });

  it("dind_volume_prune: the loop skips its tick", async (t) => {
    t.after(() => hibernationWriters.unlatch());
    let reads = 0;
    let current = new AbortController();
    const ctl = new DindVolumePruneController({
      dockerHost: HOST, workerName: "w", gate: new DindPruneGate(), log: nullLogger(),
      readSample: () => { reads++; return null; }, exec: async () => "",
      sleep: async () => { current.abort(); },
    });
    hibernationWriters.latch();
    await ctl.loop(current.signal);
    assert.equal(reads, 0, "no tick while latched");
    hibernationWriters.unlatch();
    current = new AbortController();
    await ctl.loop(current.signal);
    assert.ok(reads > 0, "the tick runs once unlatched");
  });

  it("disk_reclaim: a requested pass does not run its reclaim while latched", async (t) => {
    t.after(() => hibernationWriters.unlatch());
    let passes = 0;
    const ctl = new DiskPressureController({
      softMargin: 0.1, thresholdOf: () => 0.9, reclaim: async () => { passes++; }, admission: false,
      admissionMaxWaitMs: 1000, intervalMs: 1000, log: nullLogger(),
    });
    hibernationWriters.latch();
    await ctl.requestReclaim("periodic");
    assert.equal(passes, 0);
    hibernationWriters.unlatch();
    await ctl.requestReclaim("periodic");
    assert.equal(passes, 1);
  });

  it("outbox_drain: the worker's drain skips the outbox while latched", async (t) => {
    t.after(() => hibernationWriters.unlatch());
    let listed = 0;
    const outbox = { runsWithPending: () => { listed++; return []; } };
    const w = new Worker({ workerName: "w" } as never, {} as never, {} as never, {} as never, {} as never, {} as never,
      nullLogger(), () => ({ ok: true, missing: [] }));
    (w as unknown as { outbox: unknown }).outbox = outbox;
    const drain = (w as unknown as { drainOutbox(s?: AbortSignal): Promise<void> });
    hibernationWriters.latch();
    await drain.drainOutbox();
    assert.equal(listed, 0);
    hibernationWriters.unlatch();
    await drain.drainOutbox();
    assert.equal(listed, 1);
  });

  function bareWorker(runner: Record<string, unknown>, client: Record<string, unknown> = {}) {
    const w = new Worker({ workerName: "w", heartbeatIntervalMs: 1, maxConcurrentRuns: 1, chatSessions: 1 } as never,
      { register: async () => ({ worker_id: "worker", register_nonce: "n" }), hasFeature: () => false,
        heartbeat: async () => undefined, ...client } as never,
      runner as never, {} as never, {} as never, {} as never, nullLogger(), () => ({ ok: true, missing: [] }));
    return w as unknown as {
      settlementLoop(s: AbortSignal): Promise<void>;
      heartbeatLoop(s: AbortSignal): Promise<void>;
      registerWithRetry(s: AbortSignal): Promise<void>;
      sweepPendingTerminals: (s: AbortSignal) => Promise<void>;
      sweepPendingFinalizes: (s: AbortSignal) => Promise<void>;
    };
  }

  it("settlement_sweep: the settlement loop skips the predecessor sweep", async (t) => {
    t.after(() => hibernationWriters.unlatch());
    let sweeps = 0;
    const w = bareWorker({ settlePendingPredecessors: async () => { sweeps++; } });
    (w as unknown as { settlementSweepMs: number }).settlementSweepMs = 1;
    hibernationWriters.latch();
    let stop = new AbortController();
    setTimeout(() => stop.abort(), 30);
    await w.settlementLoop(stop.signal);
    assert.equal(sweeps, 0);
    hibernationWriters.unlatch();
    stop = new AbortController();
    setTimeout(() => stop.abort(), 30);
    await w.settlementLoop(stop.signal);
    assert.ok(sweeps > 0);
  });

  it("terminal_sweep, finalize_sweep, hold_settled_retry, recovery_redrive: a heartbeat tick starts none while latched", async (t) => {
    t.after(() => hibernationWriters.unlatch());
    const seen: string[] = [];
    const runner = {
      retryPendingHoldSettled: async () => { seen.push("hold_settled_retry"); },
      resumeLiveRecoveries: async () => { seen.push("recovery_redrive"); },
    };
    const tick = async () => {
      seen.length = 0;
      const w = bareWorker(runner);
      w.sweepPendingTerminals = async () => { seen.push("terminal_sweep"); };
      w.sweepPendingFinalizes = async () => { seen.push("finalize_sweep"); };
      await w.registerWithRetry(new AbortController().signal);
      const stop = new AbortController();
      setTimeout(() => stop.abort(), 20);
      await w.heartbeatLoop(stop.signal);
      return [...new Set(seen)].sort();
    };
    hibernationWriters.latch();
    assert.deepEqual(await tick(), []);
    hibernationWriters.unlatch();
    assert.deepEqual(await tick(), ["finalize_sweep", "hold_settled_retry", "recovery_redrive", "terminal_sweep"]);
  });
});
