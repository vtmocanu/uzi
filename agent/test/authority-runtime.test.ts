import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { describe, it, type TestContext } from "node:test";
import fsp from "node:fs/promises";
import {
  acquireWorkerLifetime, AuthorityLockBusy, AuthorityRefusal, withAuthorityTransaction, type WorkerLifetime,
} from "../src/authority-lock.js";
import { AuthorityRuntime, AuthorityShutdownRefusal, type AuthorityRuntimeOptions, type AuthorityScope } from "../src/authority-runtime.js";
import { leaseEpoch, leaseHarness, openAuthorityStore, type AuthorityFsOps, type SlotSnapshot } from "../src/completion-authority.js";
import type { AuthoritySettlement } from "../src/git-authority-hooks.js";
import { armShutdownResultDeadline, finishWorkerShutdown, type ShutdownPhase } from "../src/shutdown-barrier.js";
import { buildClaudeSourceCore } from "../src/completion-source.js";
import { loadConfig } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { Executor } from "../src/executor.js";
import { GitCache } from "../src/git.js";
import { buildWorkerGitCache } from "../src/main.js";
import { RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryBundleProducer } from "../src/recovery.js";
import { RunRunner } from "../src/runner.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";
import { makeFixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger } from "./helpers.js";

// issue #3404 M2 U3 — the authority runtime behind GitCache's hooks, and the entry points that open A
// before the locks beneath it. Real kernel flock (authority-test-support), real GitCache where a hooked
// effect must (not) happen, instrumented scopes where an ordering is the claim.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");

interface Rig {
  root: string;
  lifetime: WorkerLifetime;
  rt: AuthorityRuntime;
  bare: string;
  clone: string;
  skills: string;
}

/** A second worker process over the same data root: its own incarnation, the same W capability (one process). */
function successorOf(rig: Rig): AuthorityRuntime {
  return new AuthorityRuntime({ lifetime: rig.lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });
}

async function rig(t: TestContext): Promise<Rig> {
  const root = makeTmp(t);
  const lifetime = await acquireWorkerLifetime(root, testLockOptions());
  const bare = path.join(root, "repos", "repo.git");
  const clone = path.join(root, "runner", "repo", "issue-1");
  const skills = path.join(root, "runner", "repo", ".uzi-skills-issue-1");
  for (const dir of [bare, clone, skills]) fs.mkdirSync(dir, { recursive: true });
  return { root, lifetime, rt: new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 }), bare, clone, skills };
}

/** The parts of a RunFlight the authority wait reads (its cancel and lifecycle signals). */
function fakeFlight(executor: Executor): { executor: Executor; sourceLease?: unknown; cancel: AbortController; steering: { lifecycleSignal(): AbortSignal } } {
  const lifecycle = new AbortController();
  return { executor, cancel: new AbortController(), steering: { lifecycleSignal: () => lifecycle.signal } };
}

async function slotOf(rt: AuthorityRuntime, p: string): Promise<SlotSnapshot | null> {
  return rt.withTransaction(async (tx) => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, p));
  });
}

describe("hooks require the outer transaction", () => {
  it("every hook refuses outside a transaction and writes no slot", async (t) => {
    const r = await rig(t);
    const calls: Array<() => Promise<unknown>> = [
      () => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"),
      () => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]),
      () => r.rt.hooks.beginDestructive([r.clone], "runner-uid-delete"),
    ];
    for (const call of calls) {
      await assert.rejects(call(), (e: unknown) => e instanceof AuthorityRefusal && /requires an active authority transaction/.test(e.message));
    }
    assert.equal(fs.existsSync(path.join(r.root, ".uzi-authority", "slots")), false, "nothing was written");
    assert.equal(fs.existsSync(path.join(r.root, ".uzi-authority", "enrollment.json")), false);
  });

  it("withTransaction borrows the active transaction instead of acquiring A again", async (t) => {
    const r = await rig(t);
    const seen = await r.rt.withTransaction(async (outer) => {
      assert.equal(r.rt.active(), outer);
      return r.rt.withTransaction(async (inner) => inner === outer);
    });
    assert.equal(seen, true);
    assert.equal(r.rt.active(), undefined, "no transaction outlives its callback");
  });
});

describe("source-use admission", () => {
  it("records the flight-visible lease; normalization keeps the epoch and reseeding mints a fresh one", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const first = await slotOf(r.rt, r.clone);
    assert.equal(first?.state, "active");
    const lease = r.rt.leaseFor(r.clone);
    assert.ok(lease);
    assert.equal(leaseEpoch(lease), first?.epoch);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "normalize"));
    assert.equal((await slotOf(r.rt, r.clone))?.epoch, first?.epoch, "same open lease keeps its epoch");
    assert.equal(r.rt.leaseFor(r.clone), lease);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const again = await slotOf(r.rt, r.clone);
    assert.notEqual(again?.epoch, first?.epoch, "a reseed is a new use");
    assert.equal(leaseEpoch(r.rt.leaseFor(r.clone)!), again?.epoch);
  });

  it("binds a codex executor's incarnation before it runs; a claude flight keeps the clone's lease", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-attempt"));
    const claude = r.rt.leaseFor(r.clone)!;
    assert.equal(await r.rt.bindSource(r.clone), claude, "claude keeps the lease clone admission minted");
    assert.equal(leaseHarness(claude), "claude");

    const codex = await r.rt.bindSource(r.clone, "executor-incarnation-1");
    assert.notEqual(codex, claude);
    assert.equal(leaseHarness(codex), "codex");
    const slot = await slotOf(r.rt, r.clone);
    assert.equal(slot?.owner.executor, "executor-incarnation-1");
    assert.equal(slot?.codexMarked, true);
    assert.equal(leaseEpoch(codex), slot?.epoch);
    assert.equal(r.rt.leaseFor(r.clone), codex, "the flight-visible lease is the final one");
    assert.equal(await r.rt.bindSource(r.clone, "executor-incarnation-1"), codex, "binding twice mints no new epoch");
    const other = await r.rt.bindSource(r.clone, "executor-incarnation-2");
    assert.notEqual(leaseEpoch(other), leaseEpoch(codex), "another executor gets its own epoch");
  });
});

describe("shared-bare mutation", () => {
  it("a running record blocks assertBareClear and a successor incarnation; the owner's settlement releases both", async (t) => {
    const r = await rig(t);
    const successor = successorOf(r);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/uzi-owed/run/sha"]));
    const slot = await slotOf(r.rt, r.bare);
    assert.equal(slot?.pending?.status, "running");
    assert.equal(slot?.pending?.kind, "bare_mutation");
    assert.match(slot?.pending?.footprint ?? "", /recovery-cleanup-delete-ref: refs\/uzi-owed\/run\/sha/);

    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.assertBareClear(r.bare, "owed-pin")), /unresolved bare_mutation/);
    // The owner's death leaves the record: a successor incarnation refuses both the check and a new mutation.
    await assert.rejects(successor.withTransaction(() => successor.hooks.assertBareClear(r.bare, "owed-pin")), /unresolved bare_mutation/);
    await assert.rejects(successor.withTransaction(() => successor.hooks.beginBareMutation(r.bare, "owed-reconcile-delete-ref", ["refs/y"])),
      /unresolved pending footprint/);
    // Another bare is unaffected.
    const other = path.join(r.root, "repos", "other.git");
    fs.mkdirSync(other, { recursive: true });
    await successor.withTransaction(() => successor.hooks.assertBareClear(other, "owed-pin"));

    await r.rt.withTransaction(() => settlement.settle({ ok: true }));
    assert.equal((await slotOf(r.rt, r.bare))?.pending, null);
    await r.rt.withTransaction(() => r.rt.hooks.assertBareClear(r.bare, "owed-pin"));
    // The settled slot left active, so the successor is not wedged by a slot with nothing pending.
    const next = await successor.withTransaction(() => successor.hooks.beginBareMutation(r.bare, "owed-reconcile-delete-ref", ["refs/y"]));
    await successor.withTransaction(() => next.settle({ ok: true }));
  });

  it("a joined failed bare record releases only the running exclusion: later mutations are not wedged", async (t) => {
    const r = await rig(t);
    const successor = successorOf(r);
    const failed = await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-pin-delete-ref", ["refs/pin"]));
    await r.rt.withTransaction(() => failed.settle({ ok: false, residue: ["refs/pin"] }));
    const slot = await slotOf(r.rt, r.bare);
    assert.equal(slot?.pending?.status, "settled_failed");
    assert.deepEqual(slot?.pending?.residue, ["refs/pin"]);

    await r.rt.withTransaction(() => r.rt.hooks.assertBareClear(r.bare, "recovery-pin-anchor"));
    const again = await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-pin-delete-ref", ["refs/pin"]));
    assert.equal((await slotOf(r.rt, r.bare))?.pending?.status, "running", "the failed record was replaced");
    await r.rt.withTransaction(() => again.settle({ ok: false, residue: ["refs/pin"] }));
    const viaSuccessor = await successor.withTransaction(() => successor.hooks.beginBareMutation(r.bare, "recovery-pin-delete-ref", ["refs/pin"]));
    await successor.withTransaction(() => viaSuccessor.settle({ ok: true }));
  });

  it("a record that cannot be decoded blocks the bare instead of being skipped", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    const slots = path.join(r.root, ".uzi-authority", "slots");
    const [file] = fs.readdirSync(slots).filter((n) => n.endsWith(".json"));
    fs.writeFileSync(path.join(slots, file!), "{}\n", { mode: 0o600 });
    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.assertBareClear(r.bare, "owed-pin")), AuthorityRefusal);
  });

  it("settling twice is a no-op", async (t) => {
    const r = await rig(t);
    const s = await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    await r.rt.withTransaction(() => s.settle({ ok: true }));
    await r.rt.withTransaction(() => s.settle({ ok: true }));
    assert.equal((await slotOf(r.rt, r.bare))?.pending, null);
  });
});

describe("settlement outside a transaction", () => {
  it("settle outside a transaction never takes A: it returns while A is held, the record stays running, the next transaction applies it", async (t) => {
    const r = await rig(t);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let acquired!: () => void;
    const holding = new Promise<void>((resolve) => { acquired = resolve; });
    // Another holder owns A (not through the runtime, so no drain can run under it).
    const holder = withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async () => { acquired(); await held; });
    await holding;
    // Were settle to wait for A it would not return before the holder released; it returns at once.
    const outcome = await Promise.race([
      settlement.settle({ ok: true }).then(() => "returned"),
      new Promise<string>((resolve) => setTimeout(() => resolve("waited-for-A"), 1500)),
    ]);
    assert.equal(outcome, "returned");
    // While another holder owns A the durable record stays running (fail-closed): the settle only queued it.
    const during = await withAuthorityTransaction(r.lifetime, { timeoutMs: 100 }, async () => undefined).then(() => "got-A", () => "A-held");
    assert.equal(during, "A-held");
    release();
    await holder;
    // The idle drain the settle itself scheduled applies it once A is free: no further runtime transaction is needed.
    const read = () => withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async (tx) => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN });
      return store.readSlot(tx, await store.slotKey(tx, r.bare));
    });
    let raw = await read();
    for (let i = 0; i < 100 && raw?.pending !== null; i++) { await new Promise((resolve) => setTimeout(resolve, 20)); raw = await read(); }
    assert.equal(raw?.pending, null, "applied by the drain scheduled from settle");
  });

  it("a settle outside a transaction is applied with no other runtime transaction ever opened", async (t) => {
    const r = await rig(t);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    // The transaction above has fully exited (and its own exit drain found nothing). Settle with none active.
    await settlement.settle({ ok: true });
    let slot = await slotOf(new AuthorityRuntime({ lifetime: r.lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 }), r.bare);
    for (let i = 0; i < 100 && slot?.pending !== null; i++) {
      await new Promise((resolve) => setTimeout(resolve, 20));
      slot = await slotOf(new AuthorityRuntime({ lifetime: r.lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 }), r.bare);
    }
    assert.equal(slot?.pending, null);
  });

  it("a failed apply is re-queued a bounded number of times, then logged and left running", async (t) => {
    const r = await rig(t);
    const warnings: string[] = [];
    const fail = { on: false, syncs: 0 };
    const rt = new AuthorityRuntime({
      lifetime: r.lifetime, workerToken: TOKEN, timeoutMs: 5000, retryMs: 30,
      log: { warn: (message) => { warnings.push(message); } },
      ops: {
        syncFile: async (h) => { if (fail.on) { fail.syncs++; throw new Error("injected ENOSPC"); } await h.sync(); },
        syncDir: (h) => h.sync(),
        rename: async (a, b) => { fs.renameSync(a, b); },
      },
    });
    const settlement = await rt.withTransaction(() => rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    fail.on = true;
    await settlement.settle({ ok: true });
    for (let i = 0; i < 200 && !warnings.some((w) => /stays open/.test(w)); i++) await new Promise((resolve) => setTimeout(resolve, 20));
    assert.equal(warnings.filter((w) => /queued again/.test(w)).length, 2, "re-queued after the first two failures");
    assert.equal(warnings.filter((w) => /its record stays open/.test(w)).length, 1, "then given up on, loudly");
    const attempts = fail.syncs;
    await new Promise((resolve) => setTimeout(resolve, 150));
    assert.equal(fail.syncs, attempts, "no further attempt after the bound");
    fail.on = false;
    const slot = await slotOf(successorOf(r), r.bare);
    assert.equal(slot?.pending?.status, "running", "fail closed: the record is still open");
  });

  it("a failed apply that succeeds on a re-queue settles the record", async (t) => {
    const r = await rig(t);
    const fail = { on: false };
    const rt = new AuthorityRuntime({
      lifetime: r.lifetime, workerToken: TOKEN, timeoutMs: 5000, retryMs: 30,
      ops: {
        syncFile: async (h) => { if (fail.on) { fail.on = false; throw new Error("injected once"); } await h.sync(); },
        syncDir: (h) => h.sync(),
        rename: async (a, b) => { fs.renameSync(a, b); },
      },
    });
    const settlement = await rt.withTransaction(() => rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    fail.on = true;
    await settlement.settle({ ok: true });
    let slot = await slotOf(successorOf(r), r.bare);
    for (let i = 0; i < 200 && slot?.pending !== null; i++) { await new Promise((resolve) => setTimeout(resolve, 20)); slot = await slotOf(successorOf(r), r.bare); }
    assert.equal(slot?.pending, null);
  });

  it("a queued settlement is also applied right after the outer transaction that was running releases", async (t) => {
    const r = await rig(t);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    let open!: () => void;
    const gate = new Promise<void>((resolve) => { open = resolve; });
    // Registered outside any transaction, so it runs with none active even while one is open.
    const late = gate.then(() => settlement.settle({ ok: true }));
    await r.rt.withTransaction(async () => { open(); await late; });
    // That transaction's exit scheduled the drain (a microtask that acquires A once more); wait for it.
    for (let i = 0; i < 100; i++) {
      const raw = await withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async (tx) => {
        const store = await openAuthorityStore(tx, { workerToken: TOKEN });
        return store.readSlot(tx, await store.slotKey(tx, r.bare));
      });
      if (raw?.pending === null) break;
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    const raw = await withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async (tx) => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN });
      return store.readSlot(tx, await store.slotKey(tx, r.bare));
    });
    assert.equal(raw?.pending, null);
  });

  it("a bare mutation admits its epoch and records the running footprint in one slot write", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    const slot = await slotOf(r.rt, r.bare);
    assert.equal(slot?.seq, 1);
    assert.equal(slot?.state, "active");
    assert.equal(slot?.pending?.status, "running");
  });

  it("a callback that inherited a finished transaction's async context still settles", async (t) => {
    const r = await rig(t);
    let proceed!: () => void;
    const gate = new Promise<void>((resolve) => { proceed = resolve; });
    let lateSettle!: Promise<void>;
    await r.rt.withTransaction(async () => {
      const settlement = await r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]);
      // Runs after this transaction ended, in the async context it created (a child's late close).
      lateSettle = gate.then(() => settlement.settle({ ok: true }));
    });
    proceed();
    await lateSettle;
    assert.equal((await slotOf(r.rt, r.bare))?.pending, null);
  });
});

describe("destructive footprints", () => {
  it("maps a skills dir to its owner clone and keeps one record per source slot", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-attempt"));
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.skills], "runner-uid-delete"));
    const slot = await slotOf(r.rt, r.clone);
    assert.equal(slot?.pending?.kind, "destructive");
    assert.equal(slot?.pending?.footprint, r.skills);
    assert.equal(await slotOf(r.rt, r.skills), null, "no slot of its own for the skills dir");

    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone], "runner-uid-delete")), /already recorded/);
    const successor = successorOf(r);
    await assert.rejects(successor.withTransaction(() => successor.hooks.admitSourceUse(r.clone, "seed-attempt")), /unresolved pending footprint/);

    await r.rt.withTransaction(() => settlement.settle({ ok: false, residue: [r.skills] }));
    const failed = await slotOf(r.rt, r.clone);
    assert.equal(failed?.pending?.status, "settled_failed");
    assert.deepEqual(failed?.pending?.residue, [r.skills]);
    // Unlike a bare record, a failed destructive record keeps its residue authority: it still blocks.
    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical")), /unresolved pending footprint/);
    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone], "runner-uid-delete")), /unresolved pending footprint/);
  });

  it("registers a clone and its skills dir as one record and clears it on success", async (t) => {
    const r = await rig(t);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone, r.skills], "runner-uid-delete"));
    const slot = await slotOf(r.rt, r.clone);
    assert.equal(slot?.pending?.footprint, `${r.clone}\n${r.skills}`);
    await r.rt.withTransaction(() => settlement.settle({ ok: true }));
    assert.equal((await slotOf(r.rt, r.clone))?.pending, null);
  });

  it("closes the records it opened when a later source is refused, so nothing is left running", async (t) => {
    const r = await rig(t);
    const other = path.join(r.root, "runner", "repo", "issue-2");
    fs.mkdirSync(other, { recursive: true });
    await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([other], "runner-uid-delete"));
    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone, other], "runner-uid-delete")), /already recorded/);
    assert.equal((await slotOf(r.rt, r.clone))?.pending, null, "the first source's record was closed ok");
  });
});

describe("the production GitCache", () => {
  it("refuses a hooked effect outside a transaction and performs none; inside one it runs and admits the clone", async (t) => {
    const fx = makeFixture();
    t.after(() => fx.cleanup());
    const dataDir = fs.realpathSync(fx.dataDir);
    const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
    const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
    const config = loadConfig({ UZI_API_URL: "http://api:8080", UZI_WORKER_TOKEN: TOKEN, UZI_DATA_DIR: dataDir });
    const cache = buildWorkerGitCache(config, nullLogger(), async () => false, rt);
    const bare = await cache.ensureClone(fx.originPath);
    fs.chmodSync(path.dirname(bare), 0o700);
    fs.chmodSync(bare, 0o700);
    const target = cache.runnerClonePath(bare, "issue-31");

    await assert.rejects(cache.createOrAttachRunnerClone(bare, 31, noProofReseed, "run-31"),
      (e: unknown) => e instanceof AuthorityRefusal && /requires an active authority transaction/.test(e.message));
    assert.equal(fs.existsSync(target), false, "no effect ran");
    assert.equal(fs.existsSync(path.join(dataDir, ".uzi-authority", "slots")), false, "nothing was admitted");

    const clone = await rt.withTransaction(() => cache.createOrAttachRunnerClone(bare, 31, noProofReseed, "run-31"));
    assert.equal(clone.path, target);
    assert.equal(fs.existsSync(path.join(target, ".git")), true);
    const slot = await slotOf(rt, target);
    assert.equal(slot?.state, "active");
    assert.equal(leaseEpoch(rt.leaseFor(target)!), slot?.epoch);
  });
});

describe("outermost entries take A before the locks beneath it", () => {
  /** A scope that records when A is entered and left, around a body that records its own events. */
  function scopeOf(events: string[]): AuthorityScope {
    return {
      withTransaction: async (fn) => {
        events.push("A:enter");
        try { return await fn(undefined as never); } finally { events.push("A:exit"); }
      },
    };
  }

  it("forgetGeneration opens A before the capture-cycle key and the journal lock", async (t) => {
    const events: string[] = [];
    const root = makeTmp(t, "authority-recovery-");
    const coord = new RecoveryCoordinator({
      authority: scopeOf(events),
      client: {} as RecoveryArchiveClient,
      git: {} as RecoveryBundleProducer,
      log: nullLogger(),
      recoveryRoot: path.join(root, "recovery"),
      workerToken: TOKEN,
    });
    const internals = coord as unknown as {
      cycleKey: (...a: unknown[]) => Promise<string>;
      withJournalLock: <T>(runId: string, fn: () => Promise<T>) => Promise<T>;
    };
    const cycleKey = internals.cycleKey.bind(coord);
    internals.cycleKey = async (...a) => { events.push("C"); return cycleKey(...a); };
    const journalLock = internals.withJournalLock.bind(coord);
    internals.withJournalLock = (runId, fn) => { events.push("J"); return journalLock(runId, fn); };
    await coord.forgetGeneration("run-1", 7);
    assert.deepEqual(events, ["A:enter", "C", "J", "A:exit"]);
  });
});

describe("the runner's clone path", () => {
  it("admits the clone inside A and binds the executor's incarnation before the flight runs", async (t) => {
    const fx = makeFixture();
    t.after(() => fx.cleanup());
    const dataDir = fs.realpathSync(fx.dataDir);
    const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
    const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
    const config = loadConfig({ UZI_API_URL: "http://api:8080", UZI_WORKER_TOKEN: TOKEN, UZI_DATA_DIR: dataDir });
    const git: GitCache = buildWorkerGitCache(config, nullLogger(), async () => false, rt);
    const bare = await git.ensureClone(fx.originPath);
    fs.chmodSync(path.dirname(bare), 0o700);
    fs.chmodSync(bare, 0o700);
    const runner = new RunRunner({} as unknown as WorkerClient, git, () => ({ executor: {} as never }), nullLogger(), 20, undefined, { authority: rt });
    const internals = runner as unknown as {
      runnerCloneForClaim(bare: string, claim: unknown, reseed: unknown, executor: Executor): Promise<{ path: string }>;
      bindFlightSource(flight: ReturnType<typeof fakeFlight>, clonePath: string): Promise<void>;
    };

    const codexExecutor = { executorIncarnation: "exec-inc-77" } as unknown as Executor;
    const claim = { run_id: "run-77", kind: "issue", issue_iid: 77, session_id: null, secrets: {}, repo: { id: "repo-1" } };
    const clone = await internals.runnerCloneForClaim(bare, claim, noProofReseed, codexExecutor);
    const flight = fakeFlight(codexExecutor);
    await internals.bindFlightSource(flight, clone.path);
    const slot = await slotOf(rt, clone.path);
    assert.equal(slot?.state, "active");
    assert.equal(slot?.owner.executor, "exec-inc-77");
    assert.equal(flight.sourceLease, rt.leaseFor(clone.path), "the flight keeps the final lease");
    assert.equal(leaseHarness(flight.sourceLease as never), "codex");

    const claudeFlight = fakeFlight({} as Executor);
    const claudeClone = await internals.runnerCloneForClaim(bare, { ...claim, run_id: "run-78", issue_iid: 78 }, noProofReseed, claudeFlight.executor);
    await internals.bindFlightSource(claudeFlight, claudeClone.path);
    assert.equal(leaseHarness(claudeFlight.sourceLease as never), "claude");
    assert.equal((await slotOf(rt, claudeClone.path))?.owner.executor, null);
  });
});

describe("assertBareClear needs no transaction", () => {
  it("passes on a data root with no authority state and creates none", async (t) => {
    const r = await rig(t);
    await r.rt.hooks.assertBareClear(r.bare, "owed-pin");
    for (const name of ["enrollment.json", "slots", "heads"]) {
      assert.equal(fs.existsSync(path.join(r.root, ".uzi-authority", name)), false, `a read creates no ${name}`);
    }
    // Also when A is held by someone else: the read does not queue behind it.
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let acquired!: () => void;
    const holding = new Promise<void>((resolve) => { acquired = resolve; });
    const holder = withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async () => { acquired(); await held; });
    await holding;
    const outcome = await Promise.race([
      r.rt.hooks.assertBareClear(r.bare, "owed-pin").then(() => "returned"),
      new Promise<string>((resolve) => setTimeout(() => resolve("waited-for-A"), 1500)),
    ]);
    release();
    await holder;
    assert.equal(outcome, "returned");
  });

  it("refuses an unresolved record from any incarnation with no transaction open, and passes once it is settled", async (t) => {
    const r = await rig(t);
    const successor = successorOf(r);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    assert.equal(r.rt.active(), undefined);
    await assert.rejects(r.rt.hooks.assertBareClear(r.bare, "owed-pin"), /unresolved bare_mutation/);
    await assert.rejects(successor.hooks.assertBareClear(r.bare, "owed-pin"), /unresolved bare_mutation/);
    const other = path.join(r.root, "repos", "other.git");
    fs.mkdirSync(other, { recursive: true });
    await successor.hooks.assertBareClear(other, "owed-pin");
    await r.rt.withTransaction(() => settlement.settle({ ok: true }));
    await r.rt.hooks.assertBareClear(r.bare, "owed-pin");
    await successor.hooks.assertBareClear(r.bare, "owed-pin");
  });

  it("a slot caught in the head crash window is refused and nothing is completed by the read", async (t) => {
    const r = await rig(t);
    const flags = { head: false };
    const rt = new AuthorityRuntime({
      lifetime: r.lifetime, workerToken: TOKEN, timeoutMs: 5000,
      ops: {
        syncFile: (h) => h.sync(), syncDir: (h) => h.sync(),
        rename: async (a, b) => {
          if (flags.head && path.basename(path.dirname(b)) === "heads") throw new Error("injected head failure");
          fs.renameSync(a, b);
        },
      },
    });
    const settlement = await rt.withTransaction(() => rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    flags.head = true;
    await assert.rejects(rt.withTransaction(() => settlement.settle({ ok: true })), /injected head failure/);
    flags.head = false;
    const heads = path.join(r.root, ".uzi-authority", "heads");
    const snapshot = (): string => fs.readdirSync(heads).sort().map((n) => `${n}:${fs.readFileSync(path.join(heads, n), "utf8")}`).join("\n");
    const before = snapshot();
    await assert.rejects(rt.hooks.assertBareClear(r.bare, "owed-pin"), /crash window/);
    assert.equal(snapshot(), before, "a read-only load completes nothing");
    // The next transaction finishes the window, after which the read passes (the record was settled).
    assert.equal((await slotOf(rt, r.bare))?.pending, null);
    assert.notEqual(snapshot(), before);
    await rt.hooks.assertBareClear(r.bare, "owed-pin");
  });

  it("refuses a slot that cannot be decoded", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    const slots = path.join(r.root, ".uzi-authority", "slots");
    const [file] = fs.readdirSync(slots).filter((n) => n.endsWith(".json"));
    fs.writeFileSync(path.join(slots, file!), "{}\n", { mode: 0o600 });
    await assert.rejects(r.rt.hooks.assertBareClear(r.bare, "owed-pin"), AuthorityRefusal);
  });
});

/** The end outcome without the additive receipts, whose epochs are random. */
const endKept = (o: { ended: string[]; kept: string[] }): { ended: string[]; kept: string[] } => ({ ended: o.ended, kept: o.kept });

describe("ending a source use", () => {
  it("moves the slots this worker admitted to ineligible, so another incarnation can use the path", async (t) => {
    const r = await rig(t);
    const successor = successorOf(r);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.skills], "runner-uid-delete"));
    await r.rt.withTransaction(() => settlement.settle({ ok: true }));
    await assert.rejects(successor.withTransaction(() => successor.hooks.admitSourceUse(r.clone, "seed-canonical")),
      /active under another worker incarnation/);

    const outcome = await r.rt.endSourceUse([r.clone, r.clone]);
    assert.deepEqual(endKept(outcome), { ended: [r.clone], kept: [] });
    assert.equal(outcome.endedSlots.length, 1, "the confirmed end names the slot it wrote");
    assert.equal(outcome.endedSlots[0]!.path, r.clone);
    assert.equal((await slotOf(r.rt, r.clone))?.state, "ineligible");
    assert.equal(r.rt.leaseFor(r.clone), undefined, "the lease is dropped");
    await successor.withTransaction(() => successor.hooks.admitSourceUse(r.clone, "seed-canonical"));
    assert.equal((await slotOf(r.rt, r.clone))?.state, "active");
    assert.deepEqual(endKept(await r.rt.endSourceUse([r.clone])), { ended: [], kept: [r.clone] }, "nothing is held here any more: nothing to end");
    assert.equal((await slotOf(r.rt, r.clone))?.state, "active", "the successor's epoch is untouched");
  });

  it("keeps a slot active while an operation on it is unresolved, and says so", async (t) => {
    const warnings: string[] = [];
    const r = await rig(t);
    const rt = new AuthorityRuntime({
      lifetime: r.lifetime, workerToken: TOKEN, timeoutMs: 5000,
      log: { warn: (message) => { warnings.push(message); } },
    });
    await rt.withTransaction(() => rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    await rt.withTransaction(() => rt.hooks.beginDestructive([r.clone], "runner-uid-delete"));
    assert.deepEqual(endKept(await rt.endSourceUse([r.clone])), { ended: [], kept: [r.clone] });
    assert.equal((await slotOf(rt, r.clone))?.state, "active");
    assert.match(warnings.join("\n"), /unresolved/);
  });

  it("an unavailable A keeps everything active and never throws", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let acquired!: () => void;
    const holding = new Promise<void>((resolve) => { acquired = resolve; });
    const holder = withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async () => { acquired(); await held; });
    await holding;
    const outcome = await r.rt.endSourceUse([r.clone], { timeoutMs: 200 });
    release();
    await holder;
    assert.deepEqual(endKept(outcome), { ended: [], kept: [r.clone] });
    assert.deepEqual(outcome.endedSlots, [], "a kept slot has no confirmed end");
    // Kept, not forgotten: the end is queued and the drain applies it once A is free (see "an end that could not
    // be applied is retried by the drain"), so the slot is never left active forever by a slow A.
    let slot = await slotOf(successorOf(r), r.clone);
    for (let i = 0; i < 200 && slot?.state !== "ineligible"; i++) { await new Promise((resolve) => setTimeout(resolve, 20)); slot = await slotOf(successorOf(r), r.clone); }
    assert.equal(slot?.state, "ineligible");
  });

  it("a lease with a queued end is not re-adopted by the next flight, so the end cannot retire the next flight's epoch", async (t) => {
    const r = await rig(t);
    const l1 = await r.rt.bindSource(r.clone);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone], "runner-uid-delete"));
    assert.deepEqual(endKept(await r.rt.endSourceUse([r.clone])), { ended: [], kept: [r.clone] }, "flight 1's end is queued behind the running record");
    // Flight 2 binds the same clone while flight 1's end is still queued behind the running record: it must not
    // be handed flight 1's lease (whose queued end would later retire flight 2's slot).
    await assert.rejects(r.rt.bindSource(r.clone), /unresolved pending footprint/);
    await r.rt.withTransaction(() => settlement.settle({ ok: true }));
    await r.rt.withTransaction(async () => undefined); // acquiring A applies the queued end
    assert.equal((await slotOf(r.rt, r.clone))?.state, "ineligible", "flight 1's end applied to flight 1's epoch");
    const l2 = await r.rt.bindSource(r.clone);
    assert.notEqual(leaseEpoch(l2), leaseEpoch(l1), "a fresh epoch, never flight 1's");
    await r.rt.withTransaction(async () => undefined);
    const slot = await slotOf(r.rt, r.clone);
    assert.equal(slot?.state, "active", "flight 2's slot stays active");
    assert.equal(slot?.epoch, leaseEpoch(l2));
    await r.rt.assertLaunchable(r.clone, l2, undefined);
  });

  it("recordAdmissions names every path an admission touched, including one that was refused", async (t) => {
    const r = await rig(t);
    const sink = new Set<string>();
    await r.rt.recordAdmissions(sink, () => r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical")));
    assert.deepEqual([...sink], [r.clone]);
    const successor = successorOf(r);
    const refused = new Set<string>();
    await assert.rejects(successor.recordAdmissions(refused, () => successor.withTransaction(() => successor.hooks.admitSourceUse(r.clone, "seed-canonical"))),
      /active under another worker incarnation/);
    assert.deepEqual([...refused], [r.clone]);
    const outside = new Set<string>();
    // The owning sink's end is confirmed first: an admission outside any sink is otherwise refused (see the ownership tests).
    await r.rt.endSourceUse([r.clone]);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    assert.equal(outside.size, 0, "nothing is recorded outside a recordAdmissions scope");
  });

  it("a predecessor admission carries the capturing executor and a foreign active epoch refuses it", async (t) => {
    const r = await rig(t);
    const successor = successorOf(r);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-attempt"));
    await assert.rejects(successor.bindSource(r.clone, "exec-pred"), /active under another worker incarnation/);
    assert.equal((await slotOf(r.rt, r.clone))?.owner.executor, null, "the refused admission wrote nothing");
    await r.rt.endSourceUse([r.clone]);
    const lease = await successor.bindSource(r.clone, "exec-pred");
    assert.equal(leaseHarness(lease), "codex");
    assert.equal((await slotOf(r.rt, r.clone))?.owner.executor, "exec-pred");
    // The retained normalize that follows keeps this very epoch.
    await successor.withTransaction(() => successor.hooks.admitSourceUse(r.clone, "normalize"));
    assert.equal((await slotOf(r.rt, r.clone))?.epoch, leaseEpoch(lease));
  });
});

describe("the executor-launch gate", () => {
  it("accepts only the current lease of the clone, carrying this executor's incarnation", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const claude = await r.rt.bindSource(r.clone);
    await r.rt.assertLaunchable(r.clone, claude, undefined);
    await assert.rejects(r.rt.assertLaunchable(r.clone, claude, "exec-1"), /launch refused/, "a claude epoch does not carry an executor");
    await assert.rejects(r.rt.assertLaunchable(r.clone, undefined, undefined), /launch refused/, "no lease");

    const codex = await r.rt.bindSource(r.clone, "exec-1");
    await r.rt.assertLaunchable(r.clone, codex, "exec-1");
    await assert.rejects(r.rt.assertLaunchable(r.clone, codex, "exec-2"), /launch refused/, "another executor's incarnation");
    await assert.rejects(r.rt.assertLaunchable(r.clone, codex, undefined), /launch refused/);
    await assert.rejects(r.rt.assertLaunchable(r.clone, claude, undefined), /launch refused/, "a replaced lease is stale");

    await r.rt.endSourceUse([r.clone]);
    await assert.rejects(r.rt.assertLaunchable(r.clone, codex, "exec-1"), /launch refused/, "an ended use cannot launch");
  });
});

/** Publish `p` as claude_ready through an independent store of the same worker incarnation (the runtime holds no lease for it). */
async function publishOutOfBand(r: Rig, p: string): Promise<void> {
  await r.rt.withTransaction(async (tx) => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    const lease = await store.admitSourceUse(tx, p);
    const sha = "a".repeat(40);
    const context = { runId: "run-1", branch: "agent/issue-1", kind: "issue" as const, barePath: r.bare, generation: 2, defaultIdentity: { ref: "refs/heads/main", sha } };
    const base = { context, clonePath: p, expectedHead: sha, roots: [{ sha: "b".repeat(40), contexts: [context] }] };
    const certified = await store.certifySource(tx, lease, buildClaudeSourceCore({ store, base, lease }));
    await store.transition(tx, lease, "claude_ready", {
      source: certified, captureIds: ["11111111-1111-4111-8111-111111111111"],
    });
  });
}

describe("a destructive operation over a published source", () => {
  it("is refused without that slot's retiring lease, and the publication survives", async (t) => {
    const r = await rig(t);
    await publishOutOfBand(r, r.clone);
    const before = await slotOf(r.rt, r.clone);
    assert.equal(before?.state, "claude_ready");
    assert.ok(before?.publication);
    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone], "runner-uid-delete")),
      /published source needs its retiring lease/);
    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.skills], "runner-uid-delete")),
      /published source needs its retiring lease/, "the paired skills dir maps to the same slot");
    const after = await slotOf(r.rt, r.clone);
    assert.equal(after?.state, "claude_ready", "no new epoch was minted");
    assert.equal(after?.epoch, before?.epoch);
    assert.deepEqual(after?.publication, before?.publication);
    assert.equal(after?.seq, before?.seq, "nothing was written");
  });

  it("an unpublished slot is still admitted and recorded by a destructive operation", async (t) => {
    const r = await rig(t);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone], "runner-uid-delete"));
    assert.equal((await slotOf(r.rt, r.clone))?.pending?.kind, "destructive");
    await r.rt.withTransaction(() => settlement.settle({ ok: true }));
  });
});

describe("a reseed over a published canonical source", () => {
  it("is stopped by the journal guards before any admission or deletion: the published slot and the clone survive", async (t) => {
    const fx2 = makeFixture();
    t.after(() => fx2.cleanup());
    const dataDir = fs.realpathSync(fx2.dataDir);
    const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
    const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
    const config = loadConfig({ UZI_API_URL: "http://api:8080", UZI_WORKER_TOKEN: TOKEN, UZI_DATA_DIR: dataDir });
    const cache = buildWorkerGitCache(config, nullLogger(), async () => false, rt);
    const bare = await cache.ensureClone(fx2.originPath);
    fs.chmodSync(path.dirname(bare), 0o700);
    fs.chmodSync(bare, 0o700);
    const clone = await rt.withTransaction(() => cache.createOrAttachRunnerClone(bare, 3405, noProofReseed, "run-3405"));
    // A published source keeps the journal that names it (the completion-source fence lives in that journal).
    await cache.markRecoveryCapture(bare, clone.path, "agent/issue-3405", "run-3405");
    await publishOutOfBand({ rt, bare } as Rig, clone.path);
    const published = await slotOf(rt, clone.path);
    assert.equal(published?.state, "claude_ready");

    for (const runId of ["run-3405", "run-other"]) {
      await assert.rejects(rt.withTransaction(() => cache.createOrAttachRunnerClone(bare, 3405, noProofReseed, runId)),
        (e: unknown) => /PendingRecoveryCaptureError|ForeignCaptureBlockedError/.test((e as Error).name) && !(e instanceof AuthorityRefusal));
    }
    assert.equal(fs.existsSync(path.join(clone.path, ".git")), true, "the published clone was not freed");
    const after = await slotOf(rt, clone.path);
    assert.equal(after?.seq, published?.seq, "no epoch was minted and nothing was written");
    assert.deepEqual(after?.publication, published?.publication);
  });
});

describe("the production GitCache builder", () => {
  it("is authorityRequired: a completion retirement without its expected fence refuses before any effect", async (t) => {
    const fx2 = makeFixture();
    t.after(() => fx2.cleanup());
    const dataDir = fs.realpathSync(fx2.dataDir);
    const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
    const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
    const config = loadConfig({ UZI_API_URL: "http://api:8080", UZI_WORKER_TOKEN: TOKEN, UZI_DATA_DIR: dataDir });
    const cache = buildWorkerGitCache(config, nullLogger(), async () => false, rt);
    const bare = await cache.ensureClone(fx2.originPath);
    fs.chmodSync(path.dirname(bare), 0o700);
    fs.chmodSync(bare, 0o700);
    const clone = await rt.withTransaction(() => cache.createOrAttachRunnerClone(bare, 3404, noProofReseed, "run-3404"));
    await cache.markRecoveryCapture(bare, clone.path, "agent/issue-3404", "run-3404");
    const head = (await cache.worktreeHead(clone.path)) as string;
    await assert.rejects(rt.withTransaction(() => cache.retireRunnerClone(bare, clone.path, "agent/issue-3404", "run-3404", {
      discard: true, completionSource: { expectedHead: head, canDelete: () => true },
    })), /fence missing or changed/);
    assert.equal(fs.existsSync(path.join(clone.path, ".git")), true, "the clone is untouched");
  });
});

describe("seed after a failed head write", () => {
  it("assertBareClear inside A completes the crash window, so a seed is not blocked, across a fresh incarnation too", async (t) => {
    const r = await rig(t);
    const flags = { head: false };
    const rt = new AuthorityRuntime({
      lifetime: r.lifetime, workerToken: TOKEN, timeoutMs: 5000,
      ops: {
        syncFile: (h) => h.sync(), syncDir: (h) => h.sync(),
        rename: async (a, b) => {
          if (flags.head && path.basename(path.dirname(b)) === "heads") throw new Error("injected ENOSPC on the head write");
          fs.renameSync(a, b);
        },
      },
    });
    // A bare settle whose slot write lands but whose head write fails: the slot is one ahead of its head.
    const settlement = await rt.withTransaction(() => rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    flags.head = true;
    await assert.rejects(rt.withTransaction(() => settlement.settle({ ok: true })), /injected ENOSPC/);
    flags.head = false;
    // No-transaction readers still refuse (they never complete the window)...
    await assert.rejects(rt.hooks.assertBareClear(r.bare, "owed-pin"), /crash window/);
    // ...but a seed holds A, and its assertBareClear completes the window instead of blocking the repository.
    const fresh = successorOf(r);
    await fresh.withTransaction(async () => {
      await fresh.hooks.assertBareClear(r.bare, "seed-canonical");
      await fresh.hooks.admitSourceUse(r.clone, "seed-canonical");
    });
    assert.equal((await slotOf(fresh, r.clone))?.state, "active");
    assert.equal((await slotOf(fresh, r.bare))?.pending, null);
    // And the repair is durable: a later incarnation's plain read passes too.
    await successorOf(r).hooks.assertBareClear(r.bare, "owed-pin");
  });

  it("inside A an unresolved record still refuses the seed", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    const next = successorOf(r);
    await assert.rejects(next.withTransaction(() => next.hooks.assertBareClear(r.bare, "seed-canonical")), /unresolved bare_mutation/);
  });
});

describe("a published source is never re-admitted without an explicit consuming purpose", () => {
  async function published(t: TestContext): Promise<{ r: Rig; before: SlotSnapshot | null }> {
    const r = await rig(t);
    await publishOutOfBand(r, r.clone);
    const before = await slotOf(r.rt, r.clone);
    assert.equal(before?.state, "claude_ready");
    return { r, before };
  }
  const untouched = async (r: Rig, before: SlotSnapshot | null): Promise<void> => {
    const after = await slotOf(r.rt, r.clone);
    assert.equal(after?.seq, before?.seq, "nothing was written");
    assert.equal(after?.state, "claude_ready");
    assert.deepEqual(after?.publication, before?.publication);
  };

  it("bindSource (claude and codex) refuses", async (t) => {
    const { r, before } = await published(t);
    await assert.rejects(r.rt.bindSource(r.clone), /published/);
    await assert.rejects(r.rt.bindSource(r.clone, "exec-1"), /published/);
    await untouched(r, before);
  });

  it("a predecessor admission (bindSource with the capturing executor) refuses", async (t) => {
    const { r, before } = await published(t);
    await assert.rejects(successorOf(r).bindSource(r.clone, "exec-pred"), /published/);
    await untouched(r, before);
  });

  // U5: "discard" is no longer in this list. The guarded operator discard (git.ts discardRetainedSource) has
  // already authorized its footprint, so its admission is the one explicit operator-discard takeover
  // (AuthorityStore.admitOperatorDiscard; see completion-retirement.test.ts) instead of a refusal.
  it("normalize and replace admissions refuse", async (t) => {
    const { r, before } = await published(t);
    for (const op of ["normalize", "replace"]) {
      await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, op)), /published/, op);
    }
    await untouched(r, before);
  });

  it("a destructive operation refuses (it needs the retiring lease)", async (t) => {
    const { r, before } = await published(t);
    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone], "runner-uid-delete")), /published/);
    await untouched(r, before);
  });

  it("the canonical and attempt seeds, which run after git.ts's journal guards, may consume it", async (t) => {
    for (const op of ["seed-canonical", "seed-attempt"]) {
      const { r, before } = await published(t);
      await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, op));
      const after = await slotOf(r.rt, r.clone);
      assert.equal(after?.state, "active", op);
      assert.equal(after?.publication, null, op);
      assert.notEqual(after?.epoch, before?.epoch, `${op} minted a fresh epoch`);
    }
  });

  it("an unpublished slot is admitted by every caller as before", async (t) => {
    const r = await rig(t);
    for (const op of ["seed-canonical", "replace"]) {
      await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, op));
      assert.equal((await slotOf(r.rt, r.clone))?.state, "active", op);
    }
    await r.rt.bindSource(r.clone);
  });
});

describe("a claude flight never keeps a codex lease", () => {
  it("bindSource mints a fresh claude epoch that assertLaunchable accepts", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const codex = await r.rt.bindSource(r.clone, "exec-1");
    assert.equal(leaseHarness(codex), "codex");
    const claude = await r.rt.bindSource(r.clone);
    assert.notEqual(claude, codex);
    assert.equal(leaseHarness(claude), "claude");
    assert.equal((await slotOf(r.rt, r.clone))?.owner.executor, null);
    await r.rt.assertLaunchable(r.clone, claude, undefined);
    assert.equal(await r.rt.bindSource(r.clone), claude, "a claude lease is then kept");
  });
});

describe("the launch gate needs no A for a normal launch", () => {
  it("passes while another holder owns A, and a refusal still waits for the transactional check", async (t) => {
    const r = await rig(t);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const claude = await r.rt.bindSource(r.clone);
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let acquired!: () => void;
    const holding = new Promise<void>((resolve) => { acquired = resolve; });
    const holder = withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async () => { acquired(); await held; });
    await holding;
    try {
      const started = Date.now();
      await r.rt.assertLaunchable(r.clone, claude, undefined, { timeoutMs: 300 });
      assert.ok(Date.now() - started < 250, "did not wait for A");
      // A mismatch is not decided without A: it falls back to the transactional check, which times out here.
      const refused = await r.rt.assertLaunchable(r.clone, claude, "exec-1", { timeoutMs: 300 }).then(() => "passed", (e: unknown) => String(e));
      assert.notEqual(refused, "passed");
    } finally {
      release();
      await holder;
    }
    await assert.rejects(r.rt.assertLaunchable(r.clone, claude, "exec-1"), /launch refused/);
  });
});

describe("an end that could not be applied is retried by the drain", () => {
  it("a wait for A that times out queues the end; once A frees, the slot ends and the lease is dropped", async (t) => {
    const r = await rig(t);
    const rt = new AuthorityRuntime({ lifetime: r.lifetime, workerToken: TOKEN, timeoutMs: 5000, retryMs: 30 });
    await rt.withTransaction(() => rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let acquired!: () => void;
    const holding = new Promise<void>((resolve) => { acquired = resolve; });
    const holder = withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async () => { acquired(); await held; });
    await holding;
    const outcome = await rt.endSourceUse([r.clone], { timeoutMs: 100 });
    assert.deepEqual(endKept(outcome), { ended: [], kept: [r.clone] });
    assert.deepEqual(outcome.endedSlots, [], "a kept slot has no confirmed end");
    release();
    await holder;
    let slot = await slotOf(successorOf(r), r.clone);
    for (let i = 0; i < 200 && slot?.state !== "ineligible"; i++) { await new Promise((resolve) => setTimeout(resolve, 20)); slot = await slotOf(successorOf(r), r.clone); }
    assert.equal(slot?.state, "ineligible", "the queued end was applied with no further caller");
    assert.equal(rt.leaseFor(r.clone), undefined);
  });

  it("a running pending record defers the queued end until it is settled, and not before", async (t) => {
    const r = await rig(t);
    const rt = new AuthorityRuntime({ lifetime: r.lifetime, workerToken: TOKEN, timeoutMs: 5000, retryMs: 30 });
    await rt.withTransaction(() => rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    const settlement = await rt.withTransaction(() => rt.hooks.beginDestructive([r.clone], "runner-uid-delete"));
    assert.deepEqual(endKept(await rt.endSourceUse([r.clone])), { ended: [], kept: [r.clone] });
    await new Promise((resolve) => setTimeout(resolve, 200));
    assert.equal((await slotOf(successorOf(r), r.clone))?.state, "active", "several drains ran; the running record held the end back");
    await rt.withTransaction(() => settlement.settle({ ok: true }));
    let slot = await slotOf(successorOf(r), r.clone);
    for (let i = 0; i < 200 && slot?.state !== "ineligible"; i++) { await new Promise((resolve) => setTimeout(resolve, 20)); slot = await slotOf(successorOf(r), r.clone); }
    assert.equal(slot?.state, "ineligible");
  });
});


describe("M1 remediation authenticates the bound source lease", () => {
  for (const executor of [undefined, "codex-remediation-executor"]) {
    it(`${executor ? "Codex" : "Claude"} keeps its lease, epoch and launch authority`, async t => {
      const r = await rig(t);
      const lease = await r.rt.bindSource(r.clone, executor);
      const before = await slotOf(r.rt, r.clone);
      await r.rt.assertLaunchable(r.clone, lease, executor);
      await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "remediation-recovery"));
      assert.equal(r.rt.leaseFor(r.clone), lease);
      const after = await slotOf(r.rt, r.clone);
      assert.equal(after?.epoch, before?.epoch);
      assert.deepEqual(after?.owner, before?.owner);
      await r.rt.assertLaunchable(r.clone, lease, executor);
      await r.rt.endSourceUse([r.clone]);
      const ended = await slotOf(r.rt, r.clone);
      await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "remediation-recovery")),
        /current active source lease/);
      assert.deepEqual(await slotOf(r.rt, r.clone), ended, "refusal creates no replacement epoch");
    });
  }
  it("missing lease cannot mint a source epoch", async t => {
    const r = await rig(t);
    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "remediation-recovery")),
      /current active source lease/);
    assert.equal(await slotOf(r.rt, r.clone), null);
  });
});


// ---- issue #4066: the shutdown ledger, the owner join of inline applies and the barrier ------------------------------
//
// In-process units over the real AuthorityRuntime and the real durable store. Filesystem faults and pauses are injected
// through AuthorityFsOps.rename (the one place a slot becomes durable): every slot save is two renames, the slot and
// then its head shard. Nothing here sleeps for a shutdown-scale time; budgets are tens to hundreds of milliseconds.

interface Gate<T = void> { promise: Promise<T>; resolve(value: T): void }
function gate<T = void>(): Gate<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}

interface FaultControl {
  renames: number;
  failWhen: ((nth: number) => boolean) | undefined;
  pause: { hit: () => void; release: Promise<void> } | undefined;
  /** Runs after the real rename of the nth counted rename (to mutate what landed). */
  afterRename?: ((nth: number, to: string) => Promise<void>) | undefined;
}

interface FaultRig extends Rig {
  ctl: FaultControl;
  /** Creates the store and one slot so later counted renames start from a known point. */
  warm(): Promise<void>;
  warns: string[];
}

async function faultRig(t: TestContext, extra: Partial<AuthorityRuntimeOptions> = {}): Promise<FaultRig> {
  const base = await rig(t);
  const ctl: FaultControl = { renames: 0, failWhen: undefined, pause: undefined };
  const ops: AuthorityFsOps = {
    syncFile: (handle) => handle.sync(),
    syncDir: (handle) => handle.sync(),
    rename: async (from, to) => {
      ctl.renames += 1;
      if (ctl.failWhen?.(ctl.renames)) throw new Error("injected authority rename failure");
      if (ctl.pause) {
        const pause = ctl.pause;
        ctl.pause = undefined;
        pause.hit();
        await pause.release;
      }
      await fsp.rename(from, to);
      await ctl.afterRename?.(ctl.renames, to);
    },
  };
  const warns: string[] = [];
  const rt = new AuthorityRuntime({
    lifetime: base.lifetime, workerToken: TOKEN, timeoutMs: 5000, ops,
    log: { warn: (message: string) => { warns.push(message); } }, ...extra,
  });
  const warm = async (): Promise<void> => {
    await rt.withTransaction(() => rt.hooks.admitSourceUse(base.clone, "seed-canonical"));
  };
  return { ...base, rt, ctl, warm, warns };
}

const settle = <T>(promise: Promise<T>, ms = 1_500): Promise<T | "pending"> =>
  Promise.race([promise, new Promise<"pending">((resolve) => { setTimeout(() => resolve("pending"), ms).unref(); })]);

/** A raw holder of A that is not AuthorityRuntime.withTransaction: nothing in the runtime knows about it. */
async function holdA(lifetime: WorkerLifetime): Promise<{ release(): void; done: Promise<void> }> {
  const held = gate();
  const release = gate();
  const done = withAuthorityTransaction(lifetime, { timeoutMs: 5000 }, async () => { held.resolve(); await release.promise; });
  await held.promise;
  return { release: () => release.resolve(), done };
}

const tick = (): Promise<void> => new Promise((resolve) => setImmediate(resolve));

describe("issue #4066: the owner joins inline settlement applies", () => {
  it("an apply started by code detached from the callback keeps the transaction active and A held until it is durable", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    const written = gate();
    const release = gate();
    const callbackReturned = gate();
    let detached: Promise<void> | undefined;
    const done = f.rt.withTransaction(async () => {
      const settlement = await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete");
      f.ctl.pause = { hit: () => written.resolve(), release: release.promise };
      // Detached from this callback (it is not awaited here) but inside its async context, like a child's 'close'.
      detached = Promise.resolve().then(() => settlement.settle({ ok: true }));
      await written.promise;
      callbackReturned.resolve();
    });
    await callbackReturned.promise;
    await tick();
    assert.equal(f.rt.snapshot().counts.inline_applies, 1, "the paused apply is in the ledger");
    assert.equal(f.rt.snapshot().counts.outer, 1, "the transaction is still counted after its callback returned");
    assert.equal(await settle(done, 50), "pending", "withTransaction does not resolve while the apply is paused");
    await assert.rejects(withAuthorityTransaction(f.lifetime, { timeoutMs: 50 }, async () => undefined), AuthorityLockBusy,
      "A is still held by the paused apply");

    release.resolve();
    await done;
    await detached;
    assert.equal(f.rt.snapshot().counts.inline_applies, 0);
    assert.equal(f.rt.snapshot().counts.outer, 0, "decremented only once the transaction finished");
    assert.equal((await slotOf(f.rt, f.clone))?.pending, null, "the settlement is durable once withTransaction resolved");
    assert.deepEqual(f.rt.snapshot().reasons, []);
  });

  it("a settle that finds no active transaction is queued, stays in the ledger and is applied by the final drain", async (t) => {
    const f = await faultRig(t, { lateWaitMs: 100 });
    await f.warm();
    let settlement!: AuthoritySettlement;
    await f.rt.withTransaction(async () => { settlement = await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); });
    assert.equal(f.rt.snapshot().counts.open_settlements, 1, "opened, not yet settled");
    const holder = await holdA(f.lifetime);
    await settlement.settle({ ok: true });
    assert.equal(f.rt.snapshot().counts.queued_settlements, 1);
    assert.ok(f.rt.snapshot().reasons.includes("queued_settlement"));
    assert.equal(f.rt.snapshot().counts.open_settlements, 0, "queued is not open");
    assert.equal(f.rt.snapshot().operations.length, 1);
    await until(() => f.warns.some((w) => w.includes("stays queued until the next transaction")));
    holder.release();
    await holder.done;
    const outcome = await f.rt.drainForShutdown({ deadlineAt: Date.now() + 5000 });
    assert.equal(outcome.result, "drained");
    assert.equal((await slotOf(f.rt, f.clone))?.pending, null);
  });

  it("an outermost transaction is counted from entry, before it has A", async (t) => {
    const f = await faultRig(t);
    const holder = await holdA(f.lifetime);
    const waiting = f.rt.withTransaction(async () => "ran");
    assert.equal(f.rt.snapshot().counts.outer, 1, "counted while still waiting for A");
    assert.equal(f.rt.prerequisitesPending(), true);
    holder.release();
    await holder.done;
    assert.equal(await waiting, "ran");
    assert.equal(f.rt.snapshot().counts.outer, 0);
    assert.equal(f.rt.prerequisitesPending(), false);
  });
});

/** The durable slot `target` as the runtime's own store reads it, without a transaction of the runtime. */
async function peek(f: FaultRig, target: string): Promise<SlotSnapshot | null> {
  return withAuthorityTransaction(f.lifetime, { timeoutMs: 5000 }, async (tx) => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, target));
  });
}

async function until(condition: () => boolean, ms = 5_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!condition()) {
    if (Date.now() > deadline) assert.fail("condition not reached");
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

describe("issue #4066: the seal", () => {
  it("refuses a NEW outermost transaction only; a borrowed one, the runtime's drain and the read-only raw reads go on", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    await f.rt.withTransaction(async () => {
      f.rt.sealForShutdown();
      assert.equal(await f.rt.withTransaction(async () => "borrowed"), "borrowed", "a borrowed transaction is not new");
    });
    const refused = await f.rt.withTransaction(async () => "new").then(() => undefined, (e: unknown) => e);
    assert.ok(refused instanceof AuthorityShutdownRefusal);
    assert.ok(refused instanceof AuthorityRefusal, "it IS a refusal for every existing catch");
    assert.equal((refused as AuthorityShutdownRefusal).kind, undefined, "and carries no kind a mapping could key on");
    assert.equal(f.rt.snapshot().counts.outer, 0, "a refused entry is never counted");
    assert.equal(await f.rt.awaitSinkQuiescent({ timeoutMs: 1000 }), true, "read-only raw methods are excluded");
  });

  it("the drain still applies a queued settlement after the seal", async (t) => {
    const f = await faultRig(t, { lateWaitMs: 100 });
    await f.warm();
    let settlement!: AuthoritySettlement;
    await f.rt.withTransaction(async () => { settlement = await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); });
    const holder = await holdA(f.lifetime);
    await settlement.settle({ ok: false, residue: [f.clone] });
    f.rt.sealForShutdown();
    holder.release();
    await holder.done;
    const outcome = await f.rt.drainForShutdown({ deadlineAt: Date.now() + 5000 });
    assert.equal(outcome.result, "drained", outcome.reasons.join());
    assert.equal((await peek(f, f.clone))?.pending?.status, "settled_failed");
    await assert.rejects(f.rt.withTransaction(async () => undefined), AuthorityShutdownRefusal);
  });
});

describe("issue #4066: the shutdown budget", () => {
  it("an outermost entry that is not sealed keeps its own wait: the budget does not cut it", async (t) => {
    const f = await faultRig(t);
    f.rt.beginShutdown(Date.now() + 80);
    const holder = await holdA(f.lifetime);
    const waiting = f.rt.withTransaction(async () => "ok");
    await new Promise((resolve) => setTimeout(resolve, 250));
    holder.release();
    await holder.done;
    assert.equal(await waiting, "ok", "it waited past the 80ms budget for A");
  });

  it("an idle drain after beginShutdown waits for A at most the budget that is left, counting the time already spent", async (t) => {
    const f = await faultRig(t, { lateWaitMs: 20_000 });
    await f.warm();
    let settlement!: AuthoritySettlement;
    await f.rt.withTransaction(async () => { settlement = await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); });
    f.rt.beginShutdown(Date.now() + 500);
    await new Promise((resolve) => setTimeout(resolve, 300)); // most of the budget is already consumed
    const holder = await holdA(f.lifetime);
    const queuedAt = Date.now();
    await settlement.settle({ ok: true });
    const outcome = await settle(until(() => f.warns.some((w) => w.includes("stays queued until the next transaction"))).then(() => Date.now() - queuedAt), 4_000);
    assert.notEqual(outcome, "pending", "the drain gave up on A at the budget, not after LATE_WAIT_MS");
    assert.ok((outcome as number) < 2_500, `waited ${String(outcome)}ms`);
    holder.release();
    await holder.done;
  });

  it("no drain or retry starts once the budget is spent or the shutdown is decided", async (t) => {
    for (const how of ["deadline", "decided"] as const) {
      const f = await faultRig(t);
      await f.warm();
      let settlement!: AuthoritySettlement;
      await f.rt.withTransaction(async () => { settlement = await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); });
      if (how === "deadline") f.rt.beginShutdown(Date.now() - 1);
      else { f.rt.beginShutdown(Date.now() + 60_000); f.rt.markShutdownDecided(); }
      await settlement.settle({ ok: true });
      for (let i = 0; i < 5; i += 1) await tick();
      assert.equal(f.rt.snapshot().counts.queued_settlements, 1, `${how}: still queued, no drain ran`);
      assert.equal(f.rt.snapshot().counts.drain_in_flight, 0);
      assert.equal((await peek(f, f.clone))?.pending?.status, "running", `${how}: nothing was written`);
    }
  });

  it("the final drain never opens the store or takes A when nothing is queued", async (t) => {
    const f = await faultRig(t);
    const holder = await holdA(f.lifetime);
    const startedAt = Date.now();
    const outcome = await f.rt.drainForShutdown({ deadlineAt: Date.now() + 2_000 });
    assert.equal(outcome.result, "drained");
    assert.ok(Date.now() - startedAt < 1_000, "it did not wait for the held A");
    assert.equal(fs.existsSync(path.join(f.root, ".uzi-authority", "slots")), false, "the store was not opened");
    holder.release();
    await holder.done;
  });

  it("its waits are ref'd timers that are all cleared before it returns", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    await f.rt.withTransaction(async () => { await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); }); // open forever
    const created: Array<{ timer: NodeJS.Timeout; ref: boolean }> = [];
    const realSetTimeout = globalThis.setTimeout;
    const timersBefore = process.getActiveResourcesInfo().filter((r) => r === "Timeout").length;
    globalThis.setTimeout = ((handler: () => void, ms?: number, ...args: unknown[]) => {
      const timer = (realSetTimeout as unknown as (...a: unknown[]) => NodeJS.Timeout)(handler, ms, ...args);
      created.push({ timer, ref: timer.hasRef() });
      return timer;
    }) as typeof setTimeout;
    let outcome;
    try { outcome = await f.rt.drainForShutdown({ deadlineAt: Date.now() + 200 }); }
    finally { globalThis.setTimeout = realSetTimeout; }
    assert.equal(outcome.result, "incomplete");
    assert.ok(created.length > 0 && created.every((c) => c.ref), "every wait was a ref'd timer");
    assert.equal(process.getActiveResourcesInfo().filter((r) => r === "Timeout").length, timersBefore, "none outlived the call");
  });

  it("a drain already in flight is waited for, not duplicated", async (t) => {
    const hold = gate();
    const spliced = gate();
    const f = await faultRig(t, { lateWaitMs: 5_000, testHooks: { afterLateSplice: async () => { spliced.resolve(); await hold.promise; } } });
    await f.warm();
    let settlement!: AuthoritySettlement;
    await f.rt.withTransaction(async () => { settlement = await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); });
    await settlement.settle({ ok: true }); // queues; the idle drain splices it and pauses
    await spliced.promise;
    assert.equal(f.rt.snapshot().counts.queued_settlements, 0, "spliced out of the queue");
    assert.equal(f.rt.snapshot().counts.applying, 1, "but still in the ledger");
    assert.ok(f.rt.snapshot().reasons.includes("drain_in_flight"));
    const draining = f.rt.drainForShutdown({ deadlineAt: Date.now() + 10_000 });
    assert.equal(await settle(draining, 100), "pending");
    hold.resolve();
    const outcome = await draining;
    assert.equal(outcome.result, "drained", outcome.reasons.join());
    assert.equal((await peek(f, f.clone))?.pending, null, "the spliced settlement was applied");
  });
});

describe("issue #4066: what keeps a shutdown from being drained", () => {
  const incompleteReasons = async (f: FaultRig): Promise<string[]> =>
    (await f.rt.drainForShutdown({ deadlineAt: Date.now() + 250 })).reasons;

  it("an inline apply that rejects is owner-joined, recorded as settlement_failed and never retried", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    let detached: Promise<unknown> | undefined;
    await f.rt.withTransaction(async () => {
      const settlement = await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete");
      f.ctl.failWhen = () => true;
      detached = Promise.resolve().then(() => settlement.settle({ ok: true })).then(() => "settled", () => "rejected");
    });
    assert.equal(await detached, "rejected", "the caller of settle saw the failure");
    f.ctl.failWhen = undefined;
    assert.equal(f.rt.snapshot().counts.inline_applies, 0, "the owner joined it before withTransaction resolved");
    assert.deepEqual(await incompleteReasons(f), ["settlement_failed"]);
    assert.equal((await peek(f, f.clone))?.pending?.status, "running", "fail closed: the record stays running");
  });

  it("a settlement left open (no live child, no settle call) is open_settlement", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    await f.rt.withTransaction(async () => { await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); });
    const slot = await peek(f, f.clone);
    const reasons = await incompleteReasons(f);
    assert.deepEqual(reasons, ["open_settlement"]);
    assert.deepEqual(f.rt.snapshot().operations, [slot!.pending!.operation]);
  });

  it("a bare slot that settled but could not leave active is bare_slot_end_failed until a later transition succeeds", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    const bare = f.bare;
    let settlement!: AuthoritySettlement;
    await f.rt.withTransaction(async () => { settlement = await f.rt.hooks.beginBareMutation(bare, "recovery-cleanup-delete-ref", ["refs/x"]); });
    f.ctl.renames = 0;
    f.ctl.failWhen = (nth) => nth === 3; // the settle's slot and head renames succeed, the transition's slot rename fails
    await f.rt.withTransaction(async () => { await settlement.settle({ ok: true }); });
    f.ctl.failWhen = undefined;
    assert.equal((await peek(f, bare))?.pending, null, "the settlement itself is durable");
    assert.equal((await peek(f, bare))?.state, "active", "but the slot did not leave active");
    assert.deepEqual(await incompleteReasons(f), ["bare_slot_end_failed"]);
    const ended = await f.rt.endSourceUse([bare]);
    assert.deepEqual(ended.kept, []);
    assert.deepEqual(f.rt.snapshot().reasons, [], "a later successful transition of the same lease clears it");
  });

  it("an admission whose slot landed and whose head write failed is enrollment_uncertain", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    f.ctl.renames = 0;
    f.ctl.failWhen = (nth) => nth === 2;
    await assert.rejects(f.rt.withTransaction(() => f.rt.hooks.admitSourceUse(f.clone + "-b", "seed-attempt")), /injected authority rename failure/);
    f.ctl.failWhen = undefined;
    assert.deepEqual(await incompleteReasons(f), ["enrollment_uncertain"]);
  });

  it("an admission whose head did not read back after its durable write is enrollment_uncertain", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    f.ctl.renames = 0;
    f.ctl.afterRename = async (nth, to) => {
      if (nth === 2) await fsp.appendFile(to, " "); // the head landed, then differs on read-back
    };
    await assert.rejects(f.rt.withTransaction(() => f.rt.hooks.admitSourceUse(f.clone + "-b", "seed-attempt")), /authority head did not read back/);
    f.ctl.afterRename = undefined;
    assert.deepEqual(await incompleteReasons(f), ["enrollment_uncertain"]);
  });

  it("a plain refusal raised after the slot landed (heads directory made unsafe) is enrollment_uncertain and incomplete", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    f.ctl.renames = 0;
    const headsDir = path.join(f.root, ".uzi-authority", "heads");
    const mode = fs.statSync(headsDir).mode & 0o777;
    f.ctl.afterRename = async (nth) => {
      if (nth === 1) await fsp.chmod(headsDir, 0o755); // the slot landed; the head write now meets an unsafe directory
    };
    await assert.rejects(
      f.rt.withTransaction(() => f.rt.hooks.admitSourceUse(f.clone + "-b", "seed-attempt")),
      (error: unknown) => error instanceof AuthorityRefusal && /unsafe authority directory/.test(error.message),
    );
    f.ctl.afterRename = undefined;
    await fsp.chmod(headsDir, mode);
    assert.equal(f.rt.snapshot().counts.enrollment_uncertain, 1);
    assert.deepEqual(await incompleteReasons(f), ["enrollment_uncertain"]);
  });

  it("a slot that does not read back after its rename is enrollment_uncertain", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    f.ctl.renames = 0;
    f.ctl.afterRename = async (nth, to) => {
      if (nth === 1) await fsp.appendFile(to, " "); // the slot landed, then differs on read-back
    };
    await assert.rejects(f.rt.withTransaction(() => f.rt.hooks.admitSourceUse(f.clone + "-b", "seed-attempt")), /authority slot did not read back/);
    f.ctl.afterRename = undefined;
    assert.equal(f.rt.snapshot().counts.enrollment_uncertain, 1);
    assert.deepEqual(await incompleteReasons(f), ["enrollment_uncertain"]);
  });

  it("a clean refusal is not counted as an uncertain enrollment", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    await assert.rejects(f.rt.withTransaction(() => f.rt.hooks.admitSourceUse(f.clone + "-x", "remediation-recovery")), /current active source lease/);
    assert.equal(f.rt.snapshot().counts.enrollment_uncertain, 0);
  });

  it("a destructive rollback that could not close its records is rollback_failed", async (t) => {
    const f = await faultRig(t);
    await f.warm();
    const second = path.join(f.root, "runner", "repo", "issue-2");
    fs.mkdirSync(second, { recursive: true });
    f.ctl.renames = 0;
    // Group one: admission (2 renames) and its pending record (2). Group two fails at its admission, then every
    // later write fails too, so the rollback close of group one fails as well.
    f.ctl.failWhen = (nth) => nth >= 5;
    await assert.rejects(f.rt.withTransaction(() => f.rt.hooks.beginDestructive([f.clone, second], "runner-uid-delete")),
      /injected authority rename failure/);
    f.ctl.failWhen = undefined;
    assert.equal(f.rt.snapshot().counts.rollback_failed, 1);
    assert.ok((await incompleteReasons(f)).includes("rollback_failed"));
  });

  it("a settlement whose apply keeps failing is retried a bounded number of times, then apply_exhausted", async (t) => {
    const f = await faultRig(t, { lateWaitMs: 500, shutdownBackoffMs: 10 });
    await f.warm();
    let settlement!: AuthoritySettlement;
    await f.rt.withTransaction(async () => { settlement = await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); });
    f.ctl.failWhen = () => true;
    await settlement.settle({ ok: true });
    const outcome = await f.rt.drainForShutdown({ deadlineAt: Date.now() + 5_000 });
    f.ctl.failWhen = undefined;
    assert.equal(outcome.result, "incomplete");
    assert.deepEqual(outcome.reasons, ["apply_exhausted"]);
    assert.equal(outcome.counts.queued_settlements, 0);
    assert.equal(outcome.operations.length, 1);
    assert.equal((await peek(f, f.clone))?.pending?.status, "running");
  });
});

describe("issue #4066: finishWorkerShutdown", () => {
  const collect = (): { logs: Array<{ level: string; message: string; fields: Record<string, unknown> }>; log: Parameters<typeof finishWorkerShutdown>[0]["log"] } => {
    const logs: Array<{ level: string; message: string; fields: Record<string, unknown> }> = [];
    return { logs, log: {
      info: (message, fields) => { logs.push({ level: "info", message, fields: fields ?? {} }); },
      warn: (message, fields) => { logs.push({ level: "warn", message, fields: fields ?? {} }); },
    } };
  };
  const preserveExitCode = (t: TestContext): void => {
    const previous = process.exitCode;
    t.after(() => { process.exitCode = previous; });
  };

  it("joins a registered producer before it seals, so the producer's own transaction still succeeds", async (t) => {
    preserveExitCode(t);
    const f = await faultRig(t);
    await f.warm();
    const release = gate();
    const phases: string[] = [];
    let producerResult: string | undefined;
    const producer = (async () => {
      await release.promise;
      producerResult = await f.rt.withTransaction(async () => "committed");
    })();
    f.rt.trackProducer(producer, "retained_source_request");
    const { log, logs } = collect();
    const finishing = finishWorkerShutdown({ authority: f.rt, deadlineAt: Date.now() + 5_000, log,
      testHooks: { onShutdownPhase: (phase: ShutdownPhase) => { phases.push(phase); } } });
    assert.equal(await settle(finishing, 100), "pending", "the barrier is joining the producer");
    assert.deepEqual(phases, []);
    release.resolve();
    const result = await finishing;
    assert.equal(producerResult, "committed");
    assert.deepEqual(phases, ["prerequisites_joined", "sealed", "final_drain_started"]);
    assert.deepEqual(result, { result: "drained", exitCode: 0 });
    assert.equal(logs.filter((l) => l.message === "worker_shutdown_result").length, 1);
    assert.equal(logs[0]!.fields.result, "drained");
  });

  it("an open settlement with no live child ends incomplete (open_settlement, exit 1) with its operation id and no paths", async (t) => {
    preserveExitCode(t);
    const f = await faultRig(t);
    await f.warm();
    await f.rt.withTransaction(async () => { await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); });
    const slot = await peek(f, f.clone);
    const { log, logs } = collect();
    const result = await finishWorkerShutdown({ authority: f.rt, deadlineAt: Date.now() + 300, log });
    assert.deepEqual(result, { result: "incomplete", exitCode: 1 });
    assert.equal(process.exitCode, 1);
    const decision = logs.filter((l) => l.message === "worker_shutdown_result");
    assert.equal(decision.length, 1, "one decision");
    assert.deepEqual(decision[0]!.fields.reasons, ["deadline", "open_settlement"]);
    assert.deepEqual(decision[0]!.fields.operations, [slot!.pending!.operation]);
    assert.equal(decision[0]!.level, "warn");
    assert.ok(!JSON.stringify(decision[0]!.fields).includes(f.root), "no path in the decision");
    // Fail closed across the restart: the record is still running and a successor incarnation refuses the footprint.
    assert.equal((await peek(f, f.clone))?.pending?.status, "running");
    const successor = successorOf(f);
    await assert.rejects(successor.withTransaction(() => successor.hooks.admitSourceUse(f.clone, "seed-canonical")), /unresolved pending footprint/);
  });

  it("the deadline observer reports once while a prerequisite is pending, sets exit 1 and the barrier then logs nothing more", async (t) => {
    preserveExitCode(t);
    const f = await faultRig(t);
    await f.warm();
    const { log, logs } = collect();
    const deadlineAt = Date.now() + 80;
    const observer = armShutdownResultDeadline({ deadlineAt, log });
    const second = armShutdownResultDeadline({ deadlineAt: Date.now() + 60_000, log });
    const hold = gate();
    f.rt.trackProducer(hold.promise, "claude_shutdown_durability");
    const finishing = finishWorkerShutdown({ authority: f.rt, deadlineAt, log });
    await until(() => logs.some((l) => l.message === "worker_shutdown_result"));
    const decision = logs.find((l) => l.message === "worker_shutdown_result")!;
    assert.equal(decision.fields.result, "incomplete");
    assert.deepEqual(decision.fields.reasons, ["deadline", "prerequisite_pending", "producer_pending"]);
    assert.equal((decision.fields.counts as Record<string, number>).producers, 1);
    assert.equal(process.exitCode, 1);
    assert.equal(await settle(finishing, 100), "pending", "the observer cancels nothing: the join continues");
    hold.resolve();
    assert.deepEqual(await finishing, { result: "incomplete", exitCode: 1 });
    assert.equal(logs.filter((l) => l.message === "worker_shutdown_result").length, 1, "decided once");
    observer.clear();
    second.clear();
  });

  it("the observer stays live through the final drain: an apply paused inside it past the deadline is reported once at the deadline (exit 1) and the later drain logs nothing more", async (t) => {
    preserveExitCode(t);
    const f = await faultRig(t, { lateWaitMs: 100 });
    await f.warm();
    let settlement!: AuthoritySettlement;
    await f.rt.withTransaction(async () => { settlement = await f.rt.hooks.beginDestructive([f.clone], "runner-uid-delete"); });
    const holder = await holdA(f.lifetime);
    await settlement.settle({ ok: true }); // queued; the idle drain cannot take A and gives up
    const written = gate();
    const release = gate();
    const { log, logs } = collect();
    const deadlineAt = Date.now() + 600;
    const observer = armShutdownResultDeadline({ deadlineAt, log });
    const finishing = finishWorkerShutdown({ authority: f.rt, deadlineAt, log, testHooks: { onShutdownPhase: (phase: ShutdownPhase) => {
      if (phase !== "final_drain_started") return;
      // Only the FINAL drain can reach the write: A is freed now and the next rename pauses.
      f.ctl.pause = { hit: () => written.resolve(), release: release.promise };
      holder.release();
    } } });
    await written.promise;
    const decisions = (): typeof logs => logs.filter((l) => l.message === "worker_shutdown_result");
    assert.equal(decisions().length, 0, "nothing decided before the deadline");
    await until(() => decisions().length === 1);
    assert.equal(decisions()[0]!.fields.result, "incomplete");
    assert.ok((decisions()[0]!.fields.reasons as string[]).includes("deadline"));
    assert.equal(process.exitCode, 1);
    assert.equal(await settle(finishing, 100), "pending", "the write is not cancelled");
    release.resolve();
    assert.deepEqual(await finishing, { result: "incomplete", exitCode: 1 });
    assert.equal(decisions().length, 1, "no second result");
    assert.equal(process.exitCode, 1);
    assert.equal((await peek(f, f.clone))?.pending, null, "the paused write completed after release");
    observer.clear();
    await holder.done;
  });

  it("is idempotent per runtime: a second call returns the first call's result", async (t) => {
    preserveExitCode(t);
    const f = await faultRig(t);
    const { log, logs } = collect();
    const deadlineAt = Date.now() + 1_000;
    const first = finishWorkerShutdown({ authority: f.rt, deadlineAt, log });
    const again = finishWorkerShutdown({ authority: f.rt, deadlineAt, log });
    assert.equal(first, again);
    await first;
    assert.equal(logs.filter((l) => l.message === "worker_shutdown_result").length, 1);
  });
});
