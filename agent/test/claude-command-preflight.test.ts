import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { memoryLauncherPreflight, type MemoryLauncherProbeOptions } from "../src/claude-command/preflight.js";
import type { CodexRootHandle, ObservationOutcome, RunnerCommandChild, RunnerCommandRoot } from "../src/codex/launcher.js";

type DisposeResult = Awaited<ReturnType<CodexRootHandle["dispose"]>>;

function fakeChild(): RunnerCommandChild {
  return { pid: 4242, exitCode: null, signalCode: null } as unknown as RunnerCommandChild;
}

const complete = {
  event: "observe", id: 1, state: "complete", supervisor: { pid: 50, startTime: "1" },
  root: { pid: 51, startTime: "1" }, processes: [],
} as unknown as ObservationOutcome;

function fakeRoot(
  dispose: () => Promise<DisposeResult>,
  started = true,
  observe: () => Promise<ObservationOutcome> = async () => complete,
): RunnerCommandRoot {
  const handle = { started: started ? { event: "started" } : undefined, dispose, observe } as unknown as CodexRootHandle;
  return { handle, child: fakeChild() };
}

const drained = { clean: true, event: { event: "dispose", id: 1, state: "drained" } } as unknown as DisposeResult;

describe("memoryLauncherPreflight", () => {
  it("passes when the probe starts and disposes drained", async () => {
    let disposed = 0;
    let kills = 0;
    const result = await memoryLauncherPreflight({
      launch: async () => fakeRoot(async () => { disposed++; return drained; }),
      killUnreaped: () => { kills++; return true; },
    });
    assert.deepEqual(result, { ok: true });
    assert.equal(disposed, 1);
    assert.equal(kills, 0, "a clean probe is never signalled");
  });

  it("reports the profile field of an abnormal supervisor refusal buried in the cause chain", async () => {
    const refusal = new Error("supervisor abnormal: profile:capDrop");
    const wrapped = new Error("codex launch failed: protocol_evidence; cleanup unconfirmed", { cause: refusal });
    let killed: RunnerCommandChild | undefined;
    const result = await memoryLauncherPreflight({
      launch: async (options: MemoryLauncherProbeOptions) => {
        options.onSpawned({ pid: 4242, startTime: 1 }, fakeChild());
        throw wrapped;
      },
      killUnreaped: (child) => { killed = child; return true; },
    });
    assert.deepEqual(result, { ok: false, reason: "profile:capDrop" });
    assert.equal(killed?.pid, 4242, "an unreaped supervisor is killed after a failed probe");
  });

  it("fails when disposal is not clean and carries the reason", async () => {
    const result = await memoryLauncherPreflight({
      launch: async () => fakeRoot(async () => ({ clean: false, reason: "supervisor exited non-zero after a drained report" })),
      killUnreaped: () => true,
    });
    assert.deepEqual(result, { ok: false, reason: "supervisor exited non-zero after a drained report" });
  });

  it("fails when a clean disposal does not report the drained state", async () => {
    const result = await memoryLauncherPreflight({
      launch: async () => fakeRoot(async () => ({ clean: true, event: { state: "unconfirmed" } }) as unknown as DisposeResult),
      killUnreaped: () => true,
    });
    assert.equal(result.ok, false);
  });

  it("fails without started evidence", async () => {
    const result = await memoryLauncherPreflight({
      launch: async () => fakeRoot(async () => drained, false),
      killUnreaped: () => true,
    });
    assert.deepEqual(result, { ok: false, reason: "no started evidence" });
  });

  it("times out a launch that never resolves and kills the spawned supervisor", async () => {
    let killed = 0;
    const startedAt = Date.now();
    const result = await memoryLauncherPreflight({
      deadlineMs: 60,
      launch: (options: MemoryLauncherProbeOptions) => {
        options.onSpawned({ pid: 4242, startTime: 1 }, fakeChild());
        return new Promise<RunnerCommandRoot>(() => undefined);
      },
      killUnreaped: () => { killed++; return true; },
    });
    assert.equal(result.ok, false);
    assert.match((result as { reason: string }).reason, /deadline exceeded \(60ms\)/);
    assert.equal(killed, 1);
    assert.ok(Date.now() - startedAt < 2000, "the verdict does not wait past the deadline");
  });

  it("times out a disposal that never completes", async () => {
    const result = await memoryLauncherPreflight({
      deadlineMs: 60,
      launch: async () => fakeRoot(() => new Promise<DisposeResult>(() => undefined)),
      killUnreaped: () => true,
    });
    assert.equal(result.ok, false);
    assert.match((result as { reason: string }).reason, /deadline exceeded/);
  });

  it("never throws: a synchronous launch failure becomes a result", async () => {
    const result = await memoryLauncherPreflight({
      launch: () => { throw new Error("spawn exploded"); },
    });
    assert.deepEqual(result, { ok: false, reason: "spawn exploded" });
  });

  it("asks the launch seam for a started budget no larger than the deadline", async () => {
    let seen: number | undefined;
    await memoryLauncherPreflight({
      deadlineMs: 1234,
      launch: async (options: MemoryLauncherProbeOptions) => { seen = options.deadlines.started; return fakeRoot(async () => drained); },
    });
    assert.equal(seen, 1234);
  });

  it("asks the launch seam for the inert sleeping child and observes it once before disposing", async () => {
    const order: string[] = [];
    let child: string | undefined;
    const result = await memoryLauncherPreflight({
      launch: async (options: MemoryLauncherProbeOptions) => {
        child = options.child;
        return fakeRoot(async () => { order.push("dispose"); return drained; }, true,
          async () => { order.push("observe"); return complete; });
      },
    });
    assert.deepEqual(result, { ok: true });
    assert.equal(child, "sleep");
    assert.deepEqual(order, ["observe", "dispose"]);
  });

  it("fails closed when the supervisor lacks observe: a protocol failure is reported and the root is still disposed", async () => {
    for (const reason of ["transport", "stale", "oversize", "timeout"] as const) {
      let disposed = 0;
      const result = await memoryLauncherPreflight({
        launch: async () => fakeRoot(async () => { disposed++; return drained; }, true,
          async () => ({ event: "observe", id: 1, state: "unavailable", reason }) as ObservationOutcome),
        killUnreaped: () => true,
      });
      assert.deepEqual(result, { ok: false, reason: `supervisor lacks observe: unavailable (${reason})` }, reason);
      assert.equal(disposed, 1, "the sleeping probe child is still stopped");
    }
    const rejected = await memoryLauncherPreflight({
      launch: async () => fakeRoot(async () => drained, true, async () => { throw new Error("unknown control op"); }),
      killUnreaped: () => true,
    });
    assert.deepEqual(rejected, { ok: false, reason: "supervisor lacks observe: unknown control op" });
  });

  it("accepts an unavailable observation that says nothing about the control protocol", async () => {
    for (const reason of ["unreadable", "busy"] as const) {
      const result = await memoryLauncherPreflight({
        launch: async () => fakeRoot(async () => drained, true,
          async () => ({ event: "observe", id: 1, state: "unavailable", reason }) as ObservationOutcome),
      });
      assert.deepEqual(result, { ok: true }, reason);
    }
  });

  it("an observe that never answers is bounded by the preflight deadline", async () => {
    let killed = 0;
    const startedAt = Date.now();
    const result = await memoryLauncherPreflight({
      deadlineMs: 60,
      launch: async () => fakeRoot(async () => drained, true, () => new Promise<ObservationOutcome>(() => undefined)),
      killUnreaped: () => { killed++; return true; },
    });
    assert.equal(result.ok, false);
    assert.match((result as { reason: string }).reason, /deadline exceeded/);
    assert.ok(Date.now() - startedAt < 2000);
    assert.equal(killed, 1, "the hung probe supervisor is killed");
  });
});
