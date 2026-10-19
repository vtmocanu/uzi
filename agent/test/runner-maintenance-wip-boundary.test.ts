import test, { describe } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import type { GitCache } from "../src/git.js";
import type { RunRunner, RunnerOptions } from "../src/runner.js";
import type { RecoveryCoordinatorOptions } from "../src/recovery.js";
import { TickSpawner } from "../src/tick-spawner.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import { recordingLogger } from "./helpers.js";
import { installHarness, runnerWith, fakeGitlab, git, deferred } from "./runner-harness.js";

installHarness();
type Capture = GitCache["captureMaintenanceWip"];
type Callbacks = Parameters<Capture>[2];
type Context = Parameters<Parameters<RunRunner["withRetainedSourceBoundary"]>[2]>[0];
type View = {
  executionTails: Map<string, Promise<unknown>>;
  inventoryReservations: Set<string>;
  liveAttempts: { isLivePath: (p: string) => boolean };
  recovery: { isExecuting: (run: string) => boolean;
    withTerminalSourceBoundary: NonNullable<RecoveryCoordinatorOptions["withTerminalSourceBoundary"]> };
};
const refused = /maintenance_wip_boundary_refused/;
const callbacks: Callbacks = { beforeStep: async () => {}, afterRunnerGit: async () => {} };

async function fixture(t: test.TestContext, extra: Partial<RunnerOptions> = {}) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/runner-wip-"));
  const root = path.join(dir, "source");
  await fs.mkdir(root);
  const gitIn = (...args: string[]) => execFileSync("/usr/bin/git", ["-C", root, ...args],
    { encoding: "utf8", stdio: "pipe", env: { PATH: process.env.PATH, HOME: dir,
      GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } });
  gitIn("init");
  for (const [key, value] of [["user.name", "fixture"], ["user.email", "fixture@example.com"],
    ["commit.gpgsign", "false"], ["maintenance.auto", "false"], ["gc.auto", "0"], ["core.fsmonitor", "false"]])
    gitIn("config", key!, value!);
  await fs.writeFile(path.join(root, "tracked"), "original");
  gitIn("add", "-A"); gitIn("commit", "-m", "base");
  const old = gitIn("rev-parse", "HEAD").trim();
  let now = 1_000_000;
  const slotAbort = new AbortController();
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"),
    workerToken: "fixture", now: () => now });
  const admission = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  }, { signal: slotAbort.signal });
  assert.ok(admission.ok);
  const log = recordingLogger();
  const pids: number[] = [], commands: string[] = [], families: string[][] = [];
  let quiet = true, pressure = false;
  let onQuiet: (() => void) | undefined;
  const guard = new DataVolumeGuard({ dataDir: dir });
  t.mock.method(guard, "preflight", () => pressure ? "data_volume_full" : "not_disk_full");
  const runner = runnerWith(() => { throw new Error("model forbidden"); }, fakeGitlab().gitlab, undefined, log.logger, {
    dataVolume: guard,
    quiesceRun: async request => {
      families.push([...request.targetPaths]);
      onQuiet?.();
      return { process: { state: quiet ? "quiescent" : "unverified", processes: [], killed: [], detail: "" },
        docker: { state: "not_wired", removed: [], detail: "" } };
    },
    ...extra,
    checkpointTestHooks: { tickKillGraceMs: 0, ...extra.checkpointTestHooks,
      tickSpawn: { ...extra.checkpointTestHooks?.tickSpawn, onSpawn: (pid, argv) => {
        pids.push(pid);
        commands.push(argv.find(a => ["status", "add", "commit"].includes(a)) ?? "preparation");
        extra.checkpointTestHooks?.tickSpawn?.onSpawn?.(pid, argv);
      } },
    },
  });
  const view = runner as unknown as View;
  const run = <T>(action: () => T) => MaintenanceResources.run(admission.value, action);
  const boundary = (action: (ctx: Context) => Promise<unknown>, paths = [root], signal?: AbortSignal) =>
    runner.withRetainedSourceBoundary("run", paths, action, signal);
  const capture = (cb = callbacks, signal?: AbortSignal) => run(() => boundary(ctx => ctx.captureWip(root, old, cb), [root], signal));
  t.after(async () => {
    runner.shutdown();
    await runner.shutdownRecoveries();
    await ledger.finish(admission.value, "transient");
    for (const pid of pids) assert.throws(() => process.kill(pid, 0), { code: "ESRCH" }, "owned leader joined");
    await fs.rm(dir, { recursive: true, force: true });
  });
  return { root, old, runner, view, run, boundary, capture, gitIn, pids, commands, families, log,
    dirty: () => fs.writeFile(path.join(root, "tracked"), "changed"),
    quiet: (value: boolean) => { quiet = value; },
    pressure: () => { pressure = true; }, slotAbort,
    expire: () => { now += 120_000; },
    onQuiet: (fn: () => void) => { onQuiet = fn; },
  };
}

describe("owned maintenance WIP boundary", { skip: process.platform !== "linux" && "Linux /proc source proof required" }, () => {
for (const dirty of [false, true]) test("real owned boundary captures " + (dirty ? "tracked/untracked/binary" : "clean"), async t => {
  const f = await fixture(t);
  if (dirty) {
    await f.dirty();
    await fs.writeFile(path.join(f.root, "untracked"), "new");
    await fs.writeFile(path.join(f.root, "binary"), Buffer.from([0, 255, 1]));
  }
  const phases: string[] = [];
  const after: string[] = [];
  const result = await f.capture({ beforeStep: async (step, phase) => { phases.push(step + ":" + phase); }, afterRunnerGit: async step => {
    after.push(step);
    for (const pid of f.pids) assert.throws(() => process.kill(pid, 0), { code: "ESRCH" });
  } });
  assert.ok(result.entered);
  const value = result.value as Awaited<ReturnType<Capture>>;
  assert.equal(value.kind, dirty ? "committed" : "clean");
  assert.equal(value.parent, dirty ? f.old : null);
  assert.deepEqual(after, dirty ? ["status", "add", "commit"] : ["status"]);
  assert.deepEqual(phases, after.flatMap(step => [step + ":prepare", step + ":dispatch"]));
  assert.equal(f.families.length, dirty ? 4 : 2, "one family pass initially and after each attempted Git");
  assert.ok(f.families.every(paths => paths.length === 1 && paths[0] === f.root));
  assert.equal(f.runner.isExecuting("run"), false);
  if (dirty) {
    assert.equal(f.gitIn("show", "HEAD:tracked"), "changed");
    assert.equal(f.gitIn("show", "HEAD:untracked"), "new");
    assert.deepEqual(execFileSync("/usr/bin/git", ["-C", f.root, "show", "HEAD:binary"]), Buffer.from([0, 255, 1]));
    assert.equal(f.gitIn("show", "-s", "--format=%P").trim(), f.old);
  } else assert.equal(value.head, f.old);
});

test("unawaited capture joins deferred cleanup before releasing reservation", async t => {
  const f = await fixture(t);
  const entered = deferred(), release = deferred();
  const settled = TickSpawner.prototype.settled;
  t.mock.method(TickSpawner.prototype, "settled", async function (this: TickSpawner) {
    await settled.call(this);
    entered.resolve();
    await release.promise;
  });
  let completed = false;
  const pending = f.run(() => f.boundary(async ctx => {
    void ctx.captureWip(f.root, f.old, callbacks);
  })).finally(() => { completed = true; });
  try {
    await entered.promise;
    assert.equal(f.runner.isExecuting("run"), true);
    assert.equal(completed, false);
    assert.deepEqual(await f.boundary(async () => assert.fail("second boundary")), { entered: false });
  } finally { release.resolve(); }
  await assert.rejects(pending, refused);
  assert.equal(f.runner.isExecuting("run"), false, "proven join releases even failed/unawaited capture");
});

test("synchronous quiescence reentry and concurrent capture have only one owner", async t => {
  const f = await fixture(t);
  let ctx!: Context, reentered: Promise<unknown> | undefined;
  let tried = false;
  f.onQuiet(() => {
    if (tried) return;
    tried = true;
    reentered = assert.rejects(ctx.captureWip(f.root, f.old, callbacks), refused);
  });
  await f.run(() => f.boundary(async current => {
    ctx = current;
    const first = ctx.captureWip(f.root, f.old, callbacks);
    await assert.rejects(ctx.captureWip(f.root, f.old, callbacks), refused);
    await first;
    await reentered;
  }));
  assert.equal(f.families.length, 2);
  assert.equal(f.commands.filter(c => c === "status").length, 1);
});

for (const mode of ["probe-unknown", "survivor-confirmed", "survivor-unconfirmed", "settled-throws"] as const)
test("uncertain settlement retains blocking tail: " + mode, async t => {
  // Advance only the modeled group's clock: no surviving real process or product timer sleep.
  let clock = 0;
  const signals: string[] = [];
  const f = await fixture(t, { checkpointTestHooks: { tickSpawn: {
    groupAlive: () => {
      if (mode === "probe-unknown") throw new Error("private probe details");
      if (mode.startsWith("survivor")) { clock += 3_000; return true; }
      return undefined;
    },
    signalGroup: (_pid, sig) => { signals.push(sig); return mode === "survivor-confirmed"; },
  } } });
  if (mode.startsWith("survivor")) t.mock.method(Date, "now", () => clock);
  if (mode === "settled-throws") t.mock.method(TickSpawner.prototype, "settled", async () => {
    throw new Error("private settlement details");
  });
  let stillOpen: boolean | undefined, stillQuiet: boolean | undefined;
  await assert.rejects(f.run(() => f.boundary(async ctx => {
    try { return await ctx.captureWip(f.root, f.old, callbacks); }
    catch (error) {
      stillOpen = ctx.open();
      stillQuiet = await ctx.quiesce([f.root]);
      throw error;
    }
  })), /maintenance_wip_settlement_unconfirmed/);
  assert.equal(stillOpen, false, "uncertain ownership closes an outstanding context immediately");
  assert.equal(stillQuiet, false, "uncertain ownership cannot dispatch another family proof");
  assert.equal(f.runner.isExecuting("run"), true);
  assert.equal(f.view.recovery.isExecuting("run"), true, "ordinary coordinator sees busy; exemption removed");
  assert.equal(f.view.inventoryReservations.has("run"), false);
  let released = false;
  void f.view.executionTails.get("run")!.then(() => { released = true; });
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(released, false);
  assert.deepEqual(await f.boundary(async () => assert.fail("new owner forbidden")), { entered: false });
  assert.ok(JSON.stringify(f.log.lines).includes("maintenance_wip_settlement_unconfirmed"));
  assert.ok(!JSON.stringify(f.log.lines).includes("private "));
  if (mode.startsWith("survivor")) assert.deepEqual(signals, ["SIGKILL"]);
});

for (const mode of ["before-status", "after-status", "before-add", "after-add", "before-commit", "after-commit",
  "pressure-before", "pressure-after", "caller-abort", "slot-abort", "deadline", "ambient-deadline", "shutdown", "live", "closed"] as const)
test("fresh callbacks stop commands and reject late answers: " + mode, async t => {
  const f = await fixture(t);
  await f.dirty();
  const caller = new AbortController();
  let budgetNow = 0;
  const budget = new MaintenanceBudget(120_000, () => budgetNow);
  t.after(() => budget.dispose());
  const cb: Callbacks = {
    beforeStep: async step => {
      if (mode === "before-" + step) throw new Error("callback failed");
      if (mode === "pressure-before") f.pressure();
    },
    afterRunnerGit: async step => {
      if (mode === "after-" + step) throw new Error("callback failed");
      if (step !== "status") return;
      if (mode === "pressure-after") f.pressure();
      if (mode === "caller-abort") caller.abort();
      if (mode === "slot-abort") f.slotAbort.abort();
      if (mode === "deadline") f.expire();
      if (mode === "ambient-deadline") budgetNow = 120_000;
      if (mode === "shutdown") f.runner.shutdown();
      if (mode === "live") t.mock.method(f.view.liveAttempts, "isLivePath", () => true);
      if (mode === "closed") f.view.executionTails.delete("run");
    },
  };
  await assert.rejects(MaintenanceBudget.run(budget, () => f.capture(cb, caller.signal)), refused);
  const last = mode.endsWith("commit") ? "commit" : mode.endsWith("add") ? "add" : "status";
  if (mode.startsWith("before-")) assert.ok(!f.commands.includes(last));
  else if (last !== "commit") assert.ok(!f.commands.includes(last === "status" ? "add" : "commit"));
  assert.equal(f.runner.isExecuting("run"), false);
});

for (const mode of ["no-slot", "discovery", "foreign", "relative", "empty-family", "foreign-family", "missing-before",
  "missing-after", "aborted", "shutdown", "live", "exhausted", "initial-unquiet"] as const)
test("refuses before child: " + mode, async t => {
  const f = await fixture(t);
  const abort = new AbortController();
  if (mode === "aborted") abort.abort();
  if (mode === "shutdown") f.runner.shutdown();
  if (mode === "live") t.mock.method(f.view.liveAttempts, "isLivePath", () => true);
  if (mode === "initial-unquiet") f.quiet(false);
  const cb = mode === "missing-before" ? { ...callbacks, beforeStep: undefined } :
    mode === "missing-after" ? { ...callbacks, afterRunnerGit: false } : callbacks;
  const paths = mode === "empty-family" ? [] : mode === "foreign-family" ? [f.root, path.join(f.root, "../foreign")] : [f.root];
  const invoke = () => f.boundary(async ctx => {
    if (mode === "exhausted") assert.throws(() => MaintenanceResources.consume("launches", 513));
    return ctx.captureWip(mode === "foreign" ? path.join(f.root, "../foreign") : mode === "relative" ? "source" : f.root,
      f.old, cb as Callbacks);
  }, paths, abort.signal);
  if (["aborted", "shutdown", "live"].includes(mode)) assert.deepEqual(await f.run(invoke), { entered: false });
  else if (mode === "no-slot") await assert.rejects(invoke(), refused);
  else if (mode === "discovery") await assert.rejects(MaintenanceResources.runDiscovery(invoke), refused);
  else await assert.rejects(f.run(invoke), /maintenance_/);
  assert.equal(f.pids.length, 0);
  assert.equal(f.runner.isExecuting("run"), false);
});

test("slot abort after Git prevents a new quiescence dispatch", async t => {
  const f = await fixture(t);
  t.mock.method(git, "captureMaintenanceWip", async (...[_path, _head, cb]: Parameters<Capture>) => {
    await cb.beforeStep("status", "prepare");
    f.slotAbort.abort();
    await cb.afterRunnerGit("status");
    return { kind: "clean" as const, head: f.old, parent: null };
  });
  await assert.rejects(f.capture(), refused);
  assert.equal(f.families.length, 1, "only initial proof runs; expired scope grants no fresh helper");
  assert.equal(f.pids.length, 0);
});

test("invalid newline head refuses before initial quiescence", async t => {
  const f = await fixture(t);
  await assert.rejects(f.run(() => f.boundary(ctx => ctx.captureWip(f.root, f.old + "\n", callbacks))), refused);
  assert.equal(f.families.length, 0);
  assert.equal(f.pids.length, 0);
});

test("boundary supervisor signal inherits ambient deadline before any later helper check", async t => {
  const f = await fixture(t);
  let now = 0, ownerSignal: AbortSignal | undefined, abortedInsideCallback = false;
  const budget = new MaintenanceBudget(120_000, () => now);
  t.after(() => budget.dispose());
  const install = git.withBoundaryProcessSpawner.bind(git);
  t.mock.method(git, "withBoundaryProcessSpawner", (...args: Parameters<typeof install>) => {
    ownerSignal = args[1];
    return install(...args);
  });
  await assert.rejects(MaintenanceBudget.run(budget, () => f.capture({
    ...callbacks,
    beforeStep: async () => {
      now = 120_000;
      assert.throws(() => budget.check());
      abortedInsideCallback = ownerSignal?.aborted === true;
    },
  })), refused);
  assert.equal(abortedInsideCallback, true, "boundary owner aborts synchronously with tighter ambient budget");
  assert.equal(f.pids.length, 0);
});

for (const mode of ["ambient", "caller", "slot"] as const)
test("active child abort is joined under the original " + mode + " signal", async t => {
  const spawned = deferred(), cleanupEntered = deferred(), releaseCleanup = deferred();
  const signalled: string[] = [];
  const caller = new AbortController();
  let now = 0;
  const budget = new MaintenanceBudget(120_000, () => now);
  t.after(() => budget.dispose());
  const f = await fixture(t, { checkpointTestHooks: { tickSpawn: {
    rewrite: request => ({ ...request, argv: [process.execPath, "-e", "setInterval(() => {}, 1000)"] }),
    onSpawn: () => spawned.resolve(),
    signalGroup: (_pid, sig) => { signalled.push(sig); return undefined; },
  } } });
  const settled = TickSpawner.prototype.settled;
  t.mock.method(TickSpawner.prototype, "settled", async function (this: TickSpawner) {
    await settled.call(this);
    cleanupEntered.resolve();
    await releaseCleanup.promise;
  });
  const pending = MaintenanceBudget.run(budget, () => f.capture(callbacks, caller.signal));
  // Attach before any abort can reject the action.
  const rejected = assert.rejects(pending, refused);
  try {
    await spawned.promise;
    if (mode === "ambient") {
      now = 120_000;
      assert.throws(() => budget.check());
    } else if (mode === "caller") caller.abort();
    else f.slotAbort.abort();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.ok(signalled.includes("SIGTERM"), "owner signal reaches the active child without its command timeout");
    await cleanupEntered.promise;
    assert.equal(f.runner.isExecuting("run"), true, "cleanup join still holds execution reservation");
    assert.deepEqual(await f.boundary(async () => assert.fail()), { entered: false });
    assert.equal(f.pids.length, 1, "abort prevents any later preparation or Git command");
  } finally {
    caller.abort();
    releaseCleanup.resolve();
    await rejected;
  }
  assert.equal(f.runner.isExecuting("run"), false);
});

test("plain legacy boundary remains available without capture authority, including pressure", async t => {
  const f = await fixture(t);
  f.pressure();
  const result = await f.boundary(async ctx => {
    assert.equal(ctx.open(), true);
    assert.equal(await ctx.quiesce([f.root]), true);
    return "legacy";
  });
  assert.deepEqual(result, { entered: true, value: "legacy" });
  assert.equal(f.pids.length, 0);
});

test("reserved paths are snapshotted and all family paths share each pass", async t => {
  const f = await fixture(t);
  const skills = path.join(path.dirname(f.root), ".uzi-skills-source");
  const paths = [f.root, skills];
  await f.run(() => f.boundary(async ctx => {
    paths.push(path.join(f.root, "../foreign"));
    const result = await ctx.captureWip(f.root, f.old, callbacks);
    assert.equal(result.kind, "clean");
  }, paths));
  assert.deepEqual(f.families, [[f.root, skills], [f.root, skills]]);
});

test("escaped capture and aborted legacy open cannot grant authority", async t => {
  const f = await fixture(t);
  let escaped!: Context;
  const abort = new AbortController();
  await f.boundary(async ctx => {
    escaped = ctx;
    abort.abort();
    assert.equal(ctx.open(), false);
    assert.equal(await ctx.quiesce([f.root]), false);
  }, [f.root], abort.signal);
  await assert.rejects(f.run(() => escaped.captureWip(f.root, f.old, callbacks)), refused);
  assert.equal(f.pids.length, 0);
});

test("production wrapper exposes owned capture and refreshes pressure", async t => {
  const f = await fixture(t);
  await assert.rejects(f.view.recovery.withTerminalSourceBoundary("run", [f.root], async ctx =>
    ctx.captureWip!(f.root, f.old, callbacks)), refused);
  assert.equal(f.pids.length, 0, "wrapper cannot bypass the underlying charged-slot requirement");
  await assert.rejects(f.run(() => f.view.recovery.withTerminalSourceBoundary("run", [f.root], async ctx => {
    assert.equal(typeof ctx.captureWip, "function");
    return ctx.captureWip!(f.root, f.old, { ...callbacks, afterRunnerGit: async () => f.pressure() });
  })), refused);
  assert.ok(!f.commands.includes("add"));
  assert.equal(f.runner.isExecuting("run"), false);
  assert.deepEqual(await f.view.recovery.withTerminalSourceBoundary("run", [f.root], async () => assert.fail()), { entered: false });
});


test("transfer after real dirty WIP uses a fresh child owner under the same tail and charged slot", async t => {
  const f = await fixture(t);
  await f.dirty();
  const owners: TickSpawner[] = [];
  const settled = TickSpawner.prototype.settled;
  t.mock.method(TickSpawner.prototype, "settled", async function (this: TickSpawner) {
    owners.push(this);
    return settled.call(this);
  });
  let lateCheck!: () => void, escaped!: Context;
  await f.run(() => f.boundary(async ctx => {
    escaped = ctx;
    const tail = f.view.executionTails.get("run");
    const slot = MaintenanceResources.signalIfActive();
    const wip = await ctx.captureWip(f.root, f.old, callbacks);
    assert.equal(wip.kind, "committed");
    assert.equal(owners.length, 1, "capture has actually settled before transfer");
    const count = f.pids.length;
    assert.equal(await ctx.withMaintenanceTransfer(async check => {
      lateCheck = check;
      check();
      assert.equal(f.view.executionTails.get("run"), tail);
      assert.equal(f.view.inventoryReservations.has("run"), true);
      assert.equal(MaintenanceResources.signalIfActive(), slot);
      await assert.rejects(ctx.withMaintenanceTransfer(async () => assert.fail()), refused);
      assert.equal(await git.worktreeHead(f.root), wip.head, "existing Git spawner launches the real transfer child");
      assert.equal(f.pids.length, count + 1);
      check();
      return "transferred";
    }), "transferred");
    assert.throws(lateCheck, refused, "callback check expires when the phase returns");
    await assert.rejects(ctx.withMaintenanceTransfer(async () => assert.fail()), refused);
    assert.equal(ctx.open(), true, "transfer disposal does not dispose the outer reservation");
  }));
  assert.equal(owners.length, 2);
  assert.notEqual(owners[0], owners[1]);
  assert.equal(f.families.length, 6, "four dirty WIP passes plus two whole-family transfer passes");
  await assert.rejects(f.run(() => escaped.withMaintenanceTransfer(async () => assert.fail())), refused);
  assert.throws(lateCheck, refused);
  assert.equal(f.runner.isExecuting("run"), false);
});

for (const mode of ["before", "during", "failed", "settling"] as const)
test("transfer never waits for or bypasses incomplete capture: " + mode, async t => {
  const f = await fixture(t);
  const invoke = () => f.run(() => f.boundary(async ctx => {
    if (mode === "before") {
      await assert.rejects(ctx.withMaintenanceTransfer(async () => assert.fail()), refused);
      return;
    }
    if (mode === "settling") {
      const settled = TickSpawner.prototype.settled;
      t.mock.method(TickSpawner.prototype, "settled", async function (this: TickSpawner) {
        await settled.call(this);
        await assert.rejects(ctx.withMaintenanceTransfer(async () => assert.fail()), refused);
      });
    }
    const capture = ctx.captureWip(f.root, f.old, { ...callbacks, beforeStep: async () => {
      await assert.rejects(ctx.withMaintenanceTransfer(async () => assert.fail()), refused);
      if (mode === "failed") throw new Error("capture failed");
    } });
    if (mode === "failed") {
      await assert.rejects(capture, refused);
      await assert.rejects(ctx.withMaintenanceTransfer(async () => assert.fail()), refused);
    } else await capture;
  }));
  if (mode === "failed") await assert.rejects(invoke(), refused);
  else await invoke();
  assert.ok(f.families.length <= 2);
});

for (const mode of ["no-charge", "discovery"] as const)
test("a completed capture cannot lend its charge to a transfer: " + mode, async t => {
  const f = await fixture(t);
  await assert.rejects(f.boundary(async ctx => {
    await f.run(() => ctx.captureWip(f.root, f.old, callbacks));
    const count = f.pids.length;
    const invoke = () => ctx.withMaintenanceTransfer(async () => assert.fail());
    await assert.rejects(mode === "discovery" ? MaintenanceResources.runDiscovery(invoke) : invoke(), /maintenance_/);
    assert.equal(f.pids.length, count);
  }), /maintenance_/);
});

test("a different valid slot cannot continue an earlier capture", async t => {
  const f = await fixture(t);
  const otherDir = path.join(path.dirname(f.root), "other-charge");
  await fs.mkdir(otherDir);
  const ledger = new RecoveryMaintenance({ root: path.join(otherDir, "recovery-maintenance"), workerToken: "fixture" });
  const admitted = await ledger.admit({
    identity: { run: "other-run", generation: 1, hold: "other-hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(admitted.ok);
  try {
    await assert.rejects(f.boundary(async ctx => {
      await f.run(() => ctx.captureWip(f.root, f.old, callbacks));
      const count = f.pids.length;
      await assert.rejects(MaintenanceResources.run(admitted.value,
        () => ctx.withMaintenanceTransfer(async () => assert.fail("foreign slot callback"))), refused);
      assert.equal(f.pids.length, count);
    }), refused);
    MaintenanceResources.run(admitted.value, () => MaintenanceResources.check());
  } finally { await ledger.finish(admitted.value, "transient"); }
});

for (const mode of ["expiry", "pressure", "shutdown", "slot-abort", "caller-abort", "ambient", "live", "closed", "unquiet", "exhausted"] as const)
for (const at of ["entry", "spawn"] as const)
test("transfer checks local authority at " + at + ": " + mode, async t => {
  const f = await fixture(t);
  const caller = new AbortController();
  let now = 0;
  const budget = new MaintenanceBudget(120_000, () => now);
  t.after(() => budget.dispose());
  const revoke = () => {
    if (mode === "expiry") f.expire();
    if (mode === "pressure") f.pressure();
    if (mode === "shutdown") f.runner.shutdown();
    if (mode === "slot-abort") f.slotAbort.abort();
    if (mode === "caller-abort") caller.abort();
    if (mode === "ambient") now = 120_000;
    if (mode === "live") t.mock.method(f.view.liveAttempts, "isLivePath", () => true);
    if (mode === "closed") f.view.executionTails.delete("run");
    if (mode === "unquiet") f.quiet(false);
    if (mode === "exhausted") assert.throws(() => MaintenanceResources.consume("launches", 513));
  };
  await assert.rejects(MaintenanceBudget.run(budget, () => f.run(() => f.boundary(async ctx => {
    await ctx.captureWip(f.root, f.old, callbacks);
    const count = f.pids.length;
    if (at === "entry") revoke();
    await assert.rejects(ctx.withMaintenanceTransfer(async check => {
      if (at === "entry") assert.fail("refused before callback");
      revoke();
      if (mode === "unquiet") { check(); return; } // Positive post-transfer proof is separately required.
      assert.throws(check);
      assert.equal(await git.worktreeHead(f.root), null, "late Git launch is denied by the supervisor");
    }), /maintenance_|abort/i);
    assert.equal(f.pids.length, count);
  }, [f.root], caller.signal))), /maintenance_|abort/i);
});

test("transfer quiescence reentry is denied synchronously and uses one family envelope", async t => {
  const f = await fixture(t);
  const skills = path.join(path.dirname(f.root), ".uzi-skills-source");
  let reentry: Promise<unknown> | undefined;
  await f.run(() => f.boundary(async ctx => {
    await ctx.captureWip(f.root, f.old, callbacks);
    f.onQuiet(() => { reentry = assert.rejects(ctx.withMaintenanceTransfer(async () => assert.fail()), refused); });
    await ctx.withMaintenanceTransfer(async check => { check(); });
    await reentry;
  }, [f.root, skills]));
  assert.deepEqual(f.families, Array.from({ length: 4 }, () => [f.root, skills]));
});

test("unawaited transfer joins delayed filesystem callback before releasing its reservation", async t => {
  const f = await fixture(t);
  const ioEntered = deferred(), releaseIO = deferred();
  const target = path.join(f.root, "transfer-output");
  const write = fs.writeFile.bind(fs);
  t.mock.method(fs, "writeFile", async (...args: Parameters<typeof fs.writeFile>) => {
    if (args[0] === target) { ioEntered.resolve(); await releaseIO.promise; }
    return write(...args);
  });
  let completed = false, lateCheck!: () => void;
  const pending = f.run(() => f.boundary(async ctx => {
    await ctx.captureWip(f.root, f.old, callbacks);
    void ctx.withMaintenanceTransfer(async check => {
      lateCheck = check;
      await fs.writeFile(target, "joined");
    });
    await ioEntered.promise;
  })).finally(() => { completed = true; });
  const rejected = assert.rejects(pending, refused);
  try {
    await ioEntered.promise;
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(completed, false);
    assert.equal(f.view.inventoryReservations.has("run"), true);
    assert.equal(f.runner.isExecuting("run"), true);
    assert.throws(lateCheck, refused);
    assert.deepEqual(await f.boundary(async () => assert.fail()), { entered: false });
  } finally { releaseIO.resolve(); }
  await rejected;
  assert.equal(await fs.readFile(target, "utf8"), "joined");
  assert.equal(f.runner.isExecuting("run"), false);
});

for (const mode of ["abort", "throw", "settlement-failure"] as const)
test("transfer callback and real owned child join before release: " + mode, async t => {
  const f = await fixture(t);
  const callbackEntered = deferred(), releaseCallback = deferred(), cleanupEntered = deferred(), releaseCleanup = deferred();
  const caller = new AbortController();
  let completed = false;
  const pending = f.run(() => f.boundary(async ctx => {
    await ctx.captureWip(f.root, f.old, callbacks);
    const settled = TickSpawner.prototype.settled;
    t.mock.method(TickSpawner.prototype, "settled", async function (this: TickSpawner) {
      await settled.call(this);
      cleanupEntered.resolve();
      await releaseCleanup.promise;
      if (mode === "settlement-failure") throw new Error("private join failure");
    });
    await ctx.withMaintenanceTransfer(async check => {
      check();
      assert.equal(await git.worktreeHead(f.root), f.old);
      callbackEntered.resolve();
      await releaseCallback.promise;
      if (mode === "throw") throw new Error("private callback failure");
    });
  }, [f.root], caller.signal)).finally(() => { completed = true; });
  const rejected = assert.rejects(pending, mode === "settlement-failure" ? /maintenance_wip_settlement_unconfirmed/ : refused);
  try {
    await callbackEntered.promise;
    if (mode === "abort") caller.abort();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(completed, false);
    assert.equal(f.view.inventoryReservations.has("run"), true);
    releaseCallback.resolve();
    await cleanupEntered.promise;
    assert.equal(completed, false);
    assert.equal(f.runner.isExecuting("run"), true);
    assert.equal(f.view.inventoryReservations.has("run"), true);
  } finally { releaseCallback.resolve(); releaseCleanup.resolve(); }
  await rejected;
  assert.equal(f.runner.isExecuting("run"), mode === "settlement-failure");
  if (mode === "settlement-failure") {
    assert.equal(f.view.recovery.isExecuting("run"), true);
    assert.equal(f.view.inventoryReservations.has("run"), false);
  }
});

test("escaped asynchronous Git callback cannot launch after transfer return", async t => {
  const f = await fixture(t);
  const release = deferred();
  let late!: Promise<string | null>, check!: () => void;
  await f.run(() => f.boundary(async ctx => {
    await ctx.captureWip(f.root, f.old, callbacks);
    const count = f.pids.length;
    await ctx.withMaintenanceTransfer(async current => {
      check = current;
      late = (async () => { await release.promise; return git.worktreeHead(f.root); })();
    });
    release.resolve();
    assert.equal(await late, null);
    assert.throws(check, refused);
    assert.equal(f.pids.length, count);
  }));
});


for (const mode of ["caller", "slot", "ambient"] as const)
test("transfer signal aborts and joins an active real Git-spawned child: " + mode, async t => {
  let armed = false, now = 0;
  const spawned = deferred(), callbackIO = deferred(), releaseIO = deferred();
  const caller = new AbortController();
  const budget = new MaintenanceBudget(120_000, () => now);
  t.after(() => budget.dispose());
  const signals: string[] = [];
  const f = await fixture(t, { checkpointTestHooks: { tickSpawn: {
    rewrite: request => armed ? { ...request, argv: [process.execPath, "-e", "setInterval(() => {}, 1000)"] } : request,
    onSpawn: () => { if (armed) spawned.resolve(); },
    signalGroup: (_pid, sig) => { if (armed) signals.push(sig); return undefined; },
  } } });
  let completed = false;
  const pending = MaintenanceBudget.run(budget, () => f.run(() => f.boundary(async ctx => {
    await ctx.captureWip(f.root, f.old, callbacks);
    armed = true;
    await ctx.withMaintenanceTransfer(async check => {
      check();
      assert.equal(await git.worktreeHead(f.root), null, "aborted child cannot provide a Git answer");
      callbackIO.resolve();
      await releaseIO.promise;
    });
  }, [f.root], caller.signal))).finally(() => { completed = true; });
  const rejected = assert.rejects(pending, refused);
  try {
    await spawned.promise;
    if (mode === "caller") caller.abort();
    if (mode === "slot") f.slotAbort.abort();
    if (mode === "ambient") { now = 120_000; assert.throws(() => budget.check()); }
    await callbackIO.promise;
    assert.ok(signals.includes("SIGTERM"));
    assert.equal(completed, false);
    assert.equal(f.view.inventoryReservations.has("run"), true);
    assert.equal(f.runner.isExecuting("run"), true);
  } finally { caller.abort(); releaseIO.resolve(); }
  await rejected;
  for (const pid of f.pids) assert.throws(() => process.kill(pid, 0), { code: "ESRCH" });
  assert.equal(f.runner.isExecuting("run"), false);
});

for (const mode of ["unknown-probe", "survivor"] as const)
test("transfer uncertainty retains the original unresolved tail: " + mode, async t => {
  let armed = false, clock = 0;
  const f = await fixture(t, { checkpointTestHooks: { tickSpawn: {
    groupAlive: () => {
      if (!armed) return undefined;
      if (mode === "unknown-probe") throw new Error("private transfer probe");
      clock += 3_000;
      return true;
    },
    signalGroup: () => armed ? false : undefined,
  } } });
  if (mode === "survivor") t.mock.method(Date, "now", () => clock);
  let tail: Promise<unknown> | undefined;
  await assert.rejects(f.run(() => f.boundary(async ctx => {
    await ctx.captureWip(f.root, f.old, callbacks);
    tail = f.view.executionTails.get("run");
    armed = true;
    await ctx.withMaintenanceTransfer(async check => { check(); await git.worktreeHead(f.root); });
  })), /maintenance_wip_settlement_unconfirmed/);
  assert.equal(f.view.executionTails.get("run"), tail);
  let released = false;
  void tail!.then(() => { released = true; });
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(released, false);
  assert.equal(f.view.recovery.isExecuting("run"), true);
  assert.equal(f.view.inventoryReservations.has("run"), false);
  assert.deepEqual(await f.boundary(async () => assert.fail()), { entered: false });
});


for (const mode of ["return", "throw"] as const)
test("transfer joins a real unawaited child after callback " + mode, async t => {
  let armed = false;
  const spawned = deferred(), cleanupEntered = deferred(), releaseCleanup = deferred();
  const f = await fixture(t, { checkpointTestHooks: { tickSpawn: {
    rewrite: request => armed ? { ...request, argv: [process.execPath, "-e", "setInterval(() => {}, 1000)"] } : request,
    onSpawn: () => { if (armed) spawned.resolve(); },
  } } });
  let completed = false, lateCheck!: () => void, child!: Promise<string | null>;
  const pending = f.run(() => f.boundary(async ctx => {
    await ctx.captureWip(f.root, f.old, callbacks);
    armed = true;
    const settled = TickSpawner.prototype.settled;
    t.mock.method(TickSpawner.prototype, "settled", async function (this: TickSpawner) {
      await settled.call(this);
      for (const pid of this.pids()) assert.throws(() => process.kill(pid, 0), { code: "ESRCH" });
      cleanupEntered.resolve();
      await releaseCleanup.promise;
    });
    await ctx.withMaintenanceTransfer(async check => {
      lateCheck = check;
      child = git.worktreeHead(f.root);
      await spawned.promise;
      if (mode === "throw") throw new Error("private callback failure");
    });
  })).finally(() => { completed = true; });
  const outcome = mode === "throw" ? assert.rejects(pending, refused) : pending;
  try {
    await cleanupEntered.promise;
    assert.equal(completed, false);
    assert.equal(f.runner.isExecuting("run"), true);
    assert.equal(f.view.inventoryReservations.has("run"), true);
    assert.throws(lateCheck, refused, "callback scope closes before child settlement completes");
    assert.equal(await child, null);
  } finally { releaseCleanup.resolve(); }
  await outcome;
  assert.equal(f.runner.isExecuting("run"), false);
});

test("failed capture settlement never permits a transfer", async t => {
  const f = await fixture(t);
  const settled = TickSpawner.prototype.settled;
  t.mock.method(TickSpawner.prototype, "settled", async function (this: TickSpawner) {
    await settled.call(this);
    throw new Error("private capture settlement");
  });
  await assert.rejects(f.run(() => f.boundary(async ctx => {
    await assert.rejects(ctx.captureWip(f.root, f.old, callbacks), /maintenance_wip_settlement_unconfirmed/);
    await assert.rejects(ctx.withMaintenanceTransfer(async () => assert.fail()), refused);
  })), /maintenance_wip_settlement_unconfirmed/);
  assert.equal(f.runner.isExecuting("run"), true);
});

});
