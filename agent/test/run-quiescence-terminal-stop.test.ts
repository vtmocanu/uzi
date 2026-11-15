import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { RunProcessReap } from "../src/run-procs.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import { SinkGate } from "../src/sink-gate.js";
import { nullLogger } from "./helpers.js";
import { fakeGitlab, installHarness, runnerWith } from "./runner-harness.js";

// Terminal preservation lane stop: RunRunner.quiesceRun / quiesceOrBlockSink take an optional CheckpointStop whose
// signal the lane aborts (permit abort or window timer). An abort refuses or forfeits the sink-gate wait, starts no
// further step at the next boundary, and a stop that fires during the gated body reads as not quiescent. Without a stop
// nothing changes.

installHarness();

const quiescent = (): QuiesceRunOutcome => ({
  process: { state: "quiescent", processes: [], killed: [], detail: "scripted quiescent" },
  docker: { state: "not_wired", removed: [], detail: "" },
});
const sleep = (ms: number): Promise<void> => new Promise(res => setTimeout(res, ms));

interface Rig {
  runner: unknown;
  flight: { sinkGate: SinkGate; evidenceJoinFailed?: boolean };
  executor: Record<string, unknown>;
  counts: { impl: number; reap: number; settle: number; kill: number };
  quiesceRun: (opts: Record<string, unknown>) => Promise<{ outcome: QuiesceRunOutcome; blocked: boolean }>;
  quiesceOrBlockSink: (site: string, opts?: Record<string, unknown>) => Promise<void>;
}

function rig(opts: { settle?: () => Promise<void>; impl?: () => Promise<QuiesceRunOutcome> } = {}): Rig {
  const counts = { impl: 0, reap: 0, settle: 0, kill: 0 };
  const executor: Record<string, unknown> = {
    closeAndSettlePlanSummary: async () => { counts.settle++; await opts.settle?.(); },
    killAgentTree: () => { counts.kill++; },
    reapAttributedProcesses: async (): Promise<RunProcessReap> => { counts.reap++; return { killed: [], left: [], complete: true }; },
  };
  const runner = runnerWith(() => ({ executor }) as never, fakeGitlab().gitlab, undefined, nullLogger(), {
    quiesceRun: async (_req: QuiesceRunRequest) => { counts.impl++; return opts.impl ? opts.impl() : quiescent(); },
  });
  const flight = {
    runId: "run-stop-1", sinkGate: new SinkGate(), runLog: nullLogger(), executor, evidenceJoinFailed: false,
    worktreePath: "/data/runner/github.com+o+r/issue-1",
  };
  const r = runner as unknown as {
    quiesceRun: (f: unknown, e: unknown, o: unknown) => Promise<{ outcome: QuiesceRunOutcome; blocked: boolean }>;
    quiesceOrBlockSink: (f: unknown, e: unknown, s: string, o?: unknown) => Promise<void>;
  };
  return {
    runner, flight, executor, counts,
    quiesceRun: o => r.quiesceRun(flight, executor, { mode: "own", site: "terminal_preservation", ...o }),
    quiesceOrBlockSink: (s, o) => r.quiesceOrBlockSink(flight, executor, s, o),
  };
}

describe("quiesceRun with a lane stop", () => {
  it("an abort that lands asynchronously while a settle step is pending is noticed at the next boundary: no reap, no proof", async () => {
    const ctl = new AbortController();
    const t = rig({ settle: async () => { await sleep(20); ctl.abort(); } });
    const started = Date.now();
    const out = await t.quiesceRun({ deadlineAt: Date.now() + 60_000, stop: { signal: ctl.signal } });
    assert.equal(out.blocked, true);
    assert.notEqual(out.outcome.process?.state, "quiescent");
    assert.equal(t.counts.settle, 1, "the active settle step was awaited, never raced");
    assert.equal(t.counts.reap, 0, "the attributed reap never started");
    assert.equal(t.counts.impl, 0, "the quiescence proof never started");
    assert.ok(Date.now() - started < 5_000, "returned long before the deadline");
    assert.equal(t.flight.evidenceJoinFailed, true, "a non-safety executor's evidence join is failed closed");
  });

  it("a stop that is already aborted refuses the gate before any step", async () => {
    const ctl = new AbortController();
    ctl.abort();
    const t = rig();
    const out = await t.quiesceRun({ deadlineAt: Date.now() + 60_000, stop: { signal: ctl.signal } });
    assert.equal(out.blocked, true);
    assert.deepEqual(t.counts, { impl: 0, reap: 0, settle: 0, kill: 0 });
  });

  it("an abort while the sink gate is held forfeits the queued waiter, and no gated quiesce work runs after the holder releases", async () => {
    const ctl = new AbortController();
    const t = rig();
    let release!: () => void;
    const holder = t.flight.sinkGate.run(() => new Promise<void>(res => { release = res; }));
    await sleep(5);
    assert.equal(t.flight.sinkGate.isHeld(), true);
    const pending = t.quiesceRun({ deadlineAt: Date.now() + 60_000, stop: { signal: ctl.signal } });
    await sleep(10);
    assert.equal((t.flight.sinkGate as unknown as { waiters: unknown[] }).waiters.length, 1, "queued behind the holder");
    ctl.abort();
    const out = await pending;
    assert.equal(out.blocked, true);
    assert.equal((t.flight.sinkGate as unknown as { waiters: unknown[] }).waiters.length, 0, "the refused waiter was removed");
    release();
    await holder;
    await sleep(20);
    assert.deepEqual(t.counts, { impl: 0, reap: 0, settle: 0, kill: 0 }, "no late gated quiesce work");
    assert.equal(t.flight.sinkGate.isHeld(), false);
  });

  it("a stop that fires during the gated body's last step is conservatively blocked even though the proof was quiescent", async () => {
    const ctl = new AbortController();
    const t = rig({ impl: async () => { ctl.abort(); return quiescent(); } });
    const out = await t.quiesceRun({ deadlineAt: Date.now() + 60_000, stop: { signal: ctl.signal } });
    assert.equal(t.counts.impl, 1);
    assert.equal(out.blocked, true);
    assert.notEqual(out.outcome.process?.state, "quiescent");
  });

  it("quiesceOrBlockSink forwards the stop: an aborted stop throws the residue block instead of passing", async () => {
    const ctl = new AbortController();
    ctl.abort();
    const t = rig();
    await assert.rejects(t.quiesceOrBlockSink("terminal_preservation", { deadlineAt: Date.now() + 60_000, stop: { signal: ctl.signal } }),
      /terminal preservation stop|not quiescent/);
    assert.equal(t.counts.impl, 0);
  });

  it("characterization: without a stop the proof runs every step and reads quiescent, as before", async () => {
    const t = rig();
    const out = await t.quiesceRun({ deadlineAt: Date.now() + 60_000 });
    assert.equal(out.blocked, false);
    assert.equal(out.outcome.process?.state, "quiescent");
    assert.equal(t.counts.settle, 1);
    assert.equal(t.counts.impl, 1);
    await t.quiesceOrBlockSink("terminal_preservation");
    assert.equal(t.counts.impl, 2);
  });

  it("a live, never-aborted stop does not change the proof", async () => {
    const t = rig();
    const out = await t.quiesceRun({ deadlineAt: Date.now() + 60_000, stop: { signal: new AbortController().signal } });
    assert.equal(out.blocked, false);
    assert.equal(t.counts.impl, 1);
  });
});
