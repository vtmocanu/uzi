import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Readable } from "node:stream";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { prepareFrozenHeld, type HeldGit } from "../src/frozen-held.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { nullLogger } from "./helpers.js";

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
it("unadopted empty-settle retry stops at the shared cancellation deadline", async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "budget-empty-retry-"));
  let now = 0, releases = 0;
  const budget = new MaintenanceBudget(600_000, () => now);
  const attempted = deferred();
  const coordinator = new RecoveryCoordinator({
    recoveryRoot: root, workerToken: "fixture", log: nullLogger(),
    client: {
      hasFeature: () => true,
      listRecoveryHolds: async () => ({ run_id: "run", holds: [{
        hold_id: "hold", generation: 1, inventory_guarded: true,
      }] }),
      releaseRecoveryCustody: async () => { releases++; attempted.resolve(); throw new Error("temporarily unavailable"); },
    } as never,
    git: {} as never, unadoptedSettleRetryDelaysMs: [200],
  });
  const pending = MaintenanceBudget.run(budget, () => coordinator.settleUnadoptedGuardedGeneration({
    runId: "run", generation: 1, barePath: path.join(root, "absent-bare"),
  })).then(() => "returned", () => "aborted");
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await attempted.promise;
    await new Promise<void>(resolve => setImmediate(resolve));
    now = 600_001;
    assert.throws(() => budget.check(), /maintenance deadline/);
    const result = await Promise.race([pending, new Promise<string>(resolve => {
      timer = setTimeout(() => resolve("retry still waiting"), 30);
    })]);
    assert.equal(result, "aborted", "retry delay must not extend the absolute cancellation deadline");
    assert.equal(releases, 1, "expiry cannot admit another release");
  } finally {
    if (timer) clearTimeout(timer);
    await pending;
    budget.dispose();
    await fs.rm(root, { recursive: true, force: true });
  }
});

const tip = "a".repeat(40);
const policy = { version: 1 as const, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] };

it("capture tail expiry preserves FIFO and never starts the expired action", async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "budget-tail-"));
  const entered = deferred(), release = deferred();
  let actions = 0, now = 0;
  const coordinator = new RecoveryCoordinator({ recoveryRoot: root, workerToken: "fixture", log: nullLogger(),
    client: { hasFeature: () => true, listRecoveryHolds: async () => ({ run_id: "run", holds: [{
      hold_id: "00000000-0000-4000-8000-000000000001", generation: 1, inventory_guarded: true,
    }] }) } as never, git: {} as never, testHooks: { afterPinObserve: async () => {
      actions++; if (actions === 1) { entered.resolve(); await release.promise; }
    } } });
  let observations = 0;
  const find = coordinator["findRecord"].bind(coordinator);
  Object.assign(coordinator, { findRecord: (...args: Parameters<typeof find>) => { observations++; return find(...args); } });
  const input = { runId: "run", inventoryGuarded: true, generation: 1, kind: "issue" as const, branch: "task", sourceSha: tip };
  const budget = new MaintenanceBudget(100, () => now);
  try {
    const holder = coordinator.pin(input);
    await entered.promise;
    const waiter = MaintenanceBudget.run(budget, () => coordinator.pin(input));
    // Wait until the waiter has installed its queue placeholder.
    await new Promise<void>(r => setImmediate(r));
    now = 100; assert.throws(() => budget.check());
    await assert.rejects(waiter, /maintenance deadline/);
    const later = coordinator.pin(input);
    await new Promise<void>(r => setImmediate(r));
    assert.equal(observations, 1, "later capture must not begin even its journal read before the holder releases");
    assert.equal(actions, 1);
    release.resolve(); await holder; await later;
    assert.equal(actions, 2);
    assert.equal(observations, 2);
  } finally { release.resolve(); budget.dispose(); await fs.rm(root, { recursive: true, force: true }); }
});

it("journal queued abort performs no mutation", async t => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "budget-journal-"));
  const entered = deferred(), release = deferred();
  const original = fs.writeFile;
  let writes = 0, now = 0;
  t.mock.method(fs, "writeFile", async (...args: Parameters<typeof fs.writeFile>) => {
    writes++; if (writes === 1) { entered.resolve(); await release.promise; }
    return original(...args);
  });
  const coordinator = new RecoveryCoordinator({ recoveryRoot: root, workerToken: "fixture", log: nullLogger(), client: { hasFeature: () => true, listRecoveryHolds: async () => ({ run_id: "run", holds: [1, 2].map(generation => ({ generation, inventory_guarded: true, hold_id: `hold-${generation}` })) }) } as never, git: {} as never });
  const input = { runId: "run", inventoryGuarded: true, kind: "issue" as const, branch: "task", sourceSha: tip };
  const budget = new MaintenanceBudget(100, () => now);
  try {
    const holder = coordinator.pin({ ...input, generation: 1 }); await entered.promise;
    const waiter = MaintenanceBudget.run(budget, () => coordinator.pin({ ...input, generation: 2 }));
    await new Promise<void>(r => setImmediate(r));
    now = 100; assert.throws(() => budget.check());
    await assert.rejects(waiter, /maintenance deadline/);
    release.resolve(); await holder;
    assert.equal(writes, 1);
  } finally { release.resolve(); budget.dispose(); await fs.rm(root, { recursive: true, force: true }); }
});

for (const late of ["range", "scan", "pack", "cumulative"] as const) {
  it(`frozen Git late ${late} result stops subsequent stages and joins pack`, async () => {
    let now = 0, cancelled = 0;
    const order: string[] = [], deadlines: number[] = [];
    const budget = new MaintenanceBudget(600_000, () => now);
    const stage = (name: string) => { order.push(name); assert.equal(MaintenanceBudget.current(), budget);
      if (name === late) now = 600_000;
      if (late === "cumulative") now += 300_000;
    };
    const git = {
      resolveCheckpointRange: async () => { stage("range"); return {}; },
      secretScanCheckpointRange: async (_b: string, _r: unknown, opts: { deadlineMs: number }) => {
        deadlines.push(opts.deadlineMs); stage("scan"); return { trusted: true, findings: [] };
      },
      rangeTouchesWorkflows: async () => { stage("workflow"); return "clean"; },
      checkpointPack: async () => { stage("pack"); return { tipOid: tip, pack: Readable.from(["PACK"]),
        cancel: () => { cancelled++; }, exited: Promise.resolve(0).then(code => { order.push("exited"); return code; }) }; },
    } as unknown as HeldGit;
    try {
      await assert.rejects(MaintenanceBudget.run(budget, () => prepareFrozenHeld({ git, barePath: "bare", branch: "task", sourceSha: tip, policy, deadlineMs: 999_999 })), /maintenance deadline/);
      if (late === "range") assert.deepEqual(order, ["range"]);
      if (late === "scan" || late === "cumulative") assert.deepEqual(order, ["range", "scan"]);
      if (late === "pack") { assert.equal(cancelled, 1); assert.equal(order.at(-1), "exited"); }
      if (late === "cumulative") assert.deepEqual(deadlines, [300_000]);
    } finally { budget.dispose(); }
  });
}

it("pack abort cancels the stream and awaits child exit before returning", async () => {
  let now = 0, cancelled = 0, returned = false;
  const entered = deferred(), exited = deferred();
  const stream = new Readable({ read() { entered.resolve(); } });
  const budget = new MaintenanceBudget(100, () => now);
  const git = {
    resolveCheckpointRange: async () => ({}),
    secretScanCheckpointRange: async () => ({ trusted: true, findings: [] }),
    rangeTouchesWorkflows: async () => "clean",
    checkpointPack: async () => ({ tipOid: tip, pack: stream, cancel: () => { cancelled++; }, exited: exited.promise.then(() => 0) }),
  } as unknown as HeldGit;
  try {
    const pending = MaintenanceBudget.run(budget, () => prepareFrozenHeld({ git, barePath: "bare", branch: "task", sourceSha: tip, policy, deadlineMs: 99_999 }));
    const rejected = assert.rejects(pending, /maintenance deadline/).then(() => { returned = true; });
    await entered.promise;
    now = 100; assert.throws(() => budget.check());
    await new Promise<void>(r => setImmediate(r));
    assert.equal(cancelled, 1); assert.equal(stream.destroyed, true); assert.equal(returned, false);
    exited.resolve(); await rejected;
  } finally { exited.resolve(); budget.dispose(); stream.destroy(); }
});
