import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { describe, it, type TestContext } from "node:test";
import { acquireWorkerLifetime, AuthorityRefusal, withAuthorityTransaction, type WorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime, type AuthorityScope } from "../src/authority-runtime.js";
import { leaseEpoch, leaseHarness, openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
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
      () => r.rt.hooks.assertBareClear(r.bare, "owed-pin"),
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
  it("a settlement after its transaction ended acquires A (it waits for a holder)", async (t) => {
    const r = await rig(t);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginBareMutation(r.bare, "recovery-cleanup-delete-ref", ["refs/x"]));
    const events: string[] = [];
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let acquired!: () => void;
    const holding = new Promise<void>((resolve) => { acquired = resolve; });
    const holder = withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async () => {
      acquired();
      await held;
      events.push("holder-released");
    });
    await holding;
    const late = settlement.settle({ ok: true }).then(() => events.push("settled"));
    release();
    await holder;
    await late;
    assert.deepEqual(events, ["holder-released", "settled"], "the settlement queued behind A");
    assert.equal((await slotOf(r.rt, r.bare))?.pending, null);
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
      bindFlightSource(flight: { executor: Executor; sourceLease?: unknown }, clonePath: string): Promise<void>;
    };

    const codexExecutor = { executorIncarnation: "exec-inc-77" } as unknown as Executor;
    const claim = { run_id: "run-77", kind: "issue", issue_iid: 77, session_id: null, secrets: {}, repo: { id: "repo-1" } };
    const clone = await internals.runnerCloneForClaim(bare, claim, noProofReseed, codexExecutor);
    const flight: { executor: Executor; sourceLease?: unknown } = { executor: codexExecutor };
    await internals.bindFlightSource(flight, clone.path);
    const slot = await slotOf(rt, clone.path);
    assert.equal(slot?.state, "active");
    assert.equal(slot?.owner.executor, "exec-inc-77");
    assert.equal(flight.sourceLease, rt.leaseFor(clone.path), "the flight keeps the final lease");
    assert.equal(leaseHarness(flight.sourceLease as never), "codex");

    const claudeFlight: { executor: Executor; sourceLease?: unknown } = { executor: {} as Executor };
    const claudeClone = await internals.runnerCloneForClaim(bare, { ...claim, run_id: "run-78", issue_iid: 78 }, noProofReseed, claudeFlight.executor);
    await internals.bindFlightSource(claudeFlight, claudeClone.path);
    assert.equal(leaseHarness(claudeFlight.sourceLease as never), "claude");
    assert.equal((await slotOf(rt, claudeClone.path))?.owner.executor, null);
  });
});
