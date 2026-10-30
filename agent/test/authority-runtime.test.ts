import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { describe, it, type TestContext } from "node:test";
import { acquireWorkerLifetime, AuthorityRefusal, withAuthorityTransaction, type WorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime, type AuthorityScope } from "../src/authority-runtime.js";
import { leaseEpoch, leaseHarness, openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
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
  it("records the flight-visible lease; a normalize keeps the epoch, any other op mints a fresh one", async (t) => {
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
