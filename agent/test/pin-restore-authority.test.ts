import assert from "node:assert/strict";
import { createHash, randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { syncBuiltinESMExports } from "node:module";
import { it, type TestContext } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { leaseEpoch, peekAuthoritySlot, peekAuthoritySlotMetadata, type AuthorityFsOps } from "../src/completion-authority.js";
import type { PinRestoreLink, RetainedPinEnvelope } from "../src/pin-restore-journal.js";
import { execFileSync } from "node:child_process";
import { GitCache } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";

const TOKEN = "pin-authority-" + randomBytes(12).toString("hex");

// Two runtimes share this process's real W only to prove store primitives, not an M3 crash handoff.
async function rig(t: TestContext, ops?: AuthorityFsOps) {
  const root = makeTmp(t);
  const lifetime = await acquireWorkerLifetime(root, testLockOptions());
  const old = new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: randomUUID() });
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), ops });
  const predecessor = path.join(root, "runner", "repo", "old");
  const fresh = path.join(root, "runner", "repo", "new");
  const bare = path.join(root, "repos", "repo.git");
  for (const p of [predecessor, fresh, bare]) fs.mkdirSync(p, { recursive: true });
  const oldLease = await old.bindSource(predecessor);
  const sink = new Set<string>();
  const sourceLease = await rt.recordAdmissions(sink, () => rt.bindSource(fresh, "final-executor"));
  const link: PinRestoreLink = {
    version: 1, intentId: "primitive", predecessorJournalDigest: "a".repeat(64),
    predecessorGeneration: 1, predecessorEpoch: leaseEpoch(oldLease),
    successor: {
      runId: "run", claimGeneration: 2, workerId: "worker", registerNonce: "register",
      authorityIncarnation: rt.authorityIncarnation()!, executorIncarnation: "final-executor",
      sourceEpoch: leaseEpoch(sourceLease), attemptId: "20261008T171401Z-g2-0123456789abcdef", clonePath: fresh,
    },
    restore: {
      captureId: "capture", authenticatedRecordDigest: "b".repeat(64), predecessorGeneration: 1,
      bareDir: "repo.git", pinRef: "refs/uzi-recovery-pin/run/1", tip: "c".repeat(40),
      contentSource: "c".repeat(40), expectedHead: "c".repeat(40), expectedTree: "d".repeat(40),
    },
  };
  const within = <T>(fn: () => Promise<T>) => rt.recordAdmissions(sink, () => rt.withTransaction(fn));
  const peek = (p: string) => peekAuthoritySlot({ dataDir: root, workerToken: TOKEN }, p);
  const validate = (value = link, p = predecessor) => rt.hooks.validatePinRestoreSources!(value, p);
  return { root, rt, old, predecessor, fresh, bare, sink, link, within, peek, validate };
}

it("validates actual final executor lease without rewriting the foreign predecessor", async t => {
  const r = await rig(t);
  const slots = path.join(r.root, "completion-authority");
  // Capture all authority bytes, including the head shards, through the real filesystem.
  const bytes = (dir: string): Array<[string, string]> => fs.readdirSync(dir, { withFileTypes: true })
    .flatMap(e => e.isDirectory() ? bytes(path.join(dir, e.name))
      : [[path.join(dir, e.name), fs.readFileSync(path.join(dir, e.name)).toString("hex")]] as Array<[string, string]>);
  // Locate the private authority root by its actual files rather than guessing slot names.
  const before = bytes(fs.existsSync(slots) ? slots : r.root);
  await r.within(() => r.validate());
  assert.deepEqual(bytes(fs.existsSync(slots) ? slots : r.root), before);
  assert.equal((await r.peek(r.predecessor))?.state, "active");
  await assert.rejects(r.validate(), /active authority transaction/);
  for (const change of [
    { sourceEpoch: randomUUID() }, { authorityIncarnation: randomUUID() },
    { executorIncarnation: "stale-executor" }, { executorIncarnation: null },
  ]) {
    await assert.rejects(r.within(() => r.validate({ ...r.link, successor: { ...r.link.successor, ...change } })), /lease differs/);
  }
  await assert.rejects(r.within(() => r.validate({ ...r.link, predecessorEpoch: randomUUID() })), /predecessor/);
  await assert.rejects(r.within(() => r.validate(r.link, r.fresh)), /coincide/);
  await assert.rejects(r.rt.recordAdmissions(new Set(), () => r.rt.withTransaction(() => r.validate())), /earlier flight/);
  r.rt.retireSink(r.sink);
  await assert.rejects(r.within(() => r.validate()), /ended its source use/);
});

it("accepts an actual Claude lease and refuses a foreign predecessor with pending", async t => {
  const r = await rig(t);
  const claude = path.join(path.dirname(r.fresh), "claude");
  const lease = await r.within(() => r.rt.bindSource(claude));
  const link = { ...r.link, successor: { ...r.link.successor, clonePath: claude,
    sourceEpoch: leaseEpoch(lease), executorIncarnation: null } };
  await r.within(() => r.validate(link));
  await r.old.withTransaction(() => r.old.hooks.beginDestructive([r.predecessor], "old-pending"));
  await assert.rejects(r.within(() => r.validate(link)), /predecessor/);
});

it("refuses a source lease superseded by a fresh actual epoch", async t => {
  const r = await rig(t);
  await r.within(() => r.rt.hooks.admitSourceUse(r.fresh, "seed-attempt"));
  await assert.rejects(r.within(() => r.validate()), /lease differs/);
});

it("refuses pending and ended source leases and same-worker predecessors", async t => {
  const r = await rig(t);
  await assert.rejects(r.within(() => r.validate(r.link, r.fresh + "-absent")));
  const local = path.join(path.dirname(r.fresh), "local");
  await r.within(() => r.rt.bindSource(local));
  await assert.rejects(r.within(() => r.validate(r.link, local)), /predecessor/);
  await r.within(() => r.rt.hooks.beginDestructive([r.fresh], "pending"));
  await assert.rejects(r.within(() => r.validate()), /lease differs/);
  // A running pending record prevents the end: its queued end also refuses validation.
  await r.rt.endSourceUse([r.fresh], { sink: r.sink });
  await assert.rejects(r.within(() => r.validate()), /lease differs/);
});

it("refuses foreign active bare slots even without pending, and any running pending", async t => {
  const r = await rig(t);
  await r.old.bindSource(r.bare);
  await assert.rejects(r.within(() => r.rt.hooks.beginPinRestoreBareMutation!(r.bare, "restore", ["ref"])), /another worker/);
  const other = path.join(path.dirname(r.bare), "other.git");
  await r.old.withTransaction(() => r.old.hooks.beginBareMutation(other, "old", ["ref"]));
  await assert.rejects(r.within(() => r.rt.hooks.beginPinRestoreBareMutation!(other, "restore", ["ref"])), /pending/);
});

it("owns the actual operation, refuses second tokens and requires durable successful settlement", async t => {
  const r = await rig(t);
  await assert.rejects(r.rt.hooks.beginPinRestoreBareMutation!(r.bare, "restore", ["ref"]), /active authority/);
  const handle = await r.within(() => r.rt.hooks.beginPinRestoreBareMutation!(r.bare, "restore", ["ref"]));
  const before = await r.peek(r.bare);
  await r.within(() => handle.assertCurrent());
  await assert.rejects(r.within(() => handle.assertSettled()), /not durably settled/);
  await assert.rejects(r.within(() => r.rt.hooks.beginPinRestoreBareMutation!(r.bare, "second", ["ref"])), /pending/);
  assert.deepEqual(await r.peek(r.bare), before);
  await assert.rejects(handle.assertCurrent(), /active authority/);
  await assert.rejects(handle.assertSettled(), /active authority/);
  await assert.rejects(handle.settle({ ok: true }), /active authority/);
  await assert.rejects(r.rt.recordAdmissions(new Set(), () => r.rt.withTransaction(() => handle.assertCurrent())), /sink differs/);
  // The test's inert effect has completed; no child or background work remains to join.
  await r.within(() => handle.settle({ ok: true }));
  await r.within(() => handle.assertSettled());
  await assert.rejects(r.within(() => handle.assertCurrent()), /operation differs/);
  const next = await r.within(() => r.rt.hooks.beginPinRestoreBareMutation!(r.bare, "next", ["ref"]));
  await assert.rejects(r.within(() => handle.assertSettled()), /lease differs/);
  await r.within(() => next.settle({ ok: false, residue: ["ref"] }));
  await assert.rejects(r.within(() => next.assertSettled()), /not durably settled/);
});

for (const failure of ["settle-fsync", "transition", "transition-directory"] as const) {
  it(`refuses settled proof after ${failure} failure`, async t => {
    let armed = false;
    let writes = 0;
    let directories = 0;
    const r = await rig(t, {
      syncFile: async h => {
        if (armed && ++writes === (failure === "transition" ? 3 : failure === "settle-fsync" ? 1 : -1)) throw new Error("injected fsync failure");
        await h.sync();
      },
      syncDir: async h => {
        if (armed && ++directories === 3 && failure === "transition-directory") throw new Error("injected directory failure");
        await h.sync();
      },
      rename: (a, b) => fsp.rename(a, b),
    });
    const handle = await r.within(() => r.rt.hooks.beginPinRestoreBareMutation!(r.bare, "restore", ["ref"]));
    await r.within(async () => {
      armed = true;
      if (failure === "settle-fsync") await assert.rejects(handle.settle({ ok: true }), /injected/);
      else await handle.settle({ ok: true }); // Existing settlement catches the failed transition.
      armed = false;
      await assert.rejects(handle.assertSettled(), /not durably settled/);
    });
    assert.ok(writes > 0);
    if (failure === "transition-directory") assert.equal((await r.peek(r.bare))?.state, "ineligible");
  });
}

function authorityBytes(root: string): string {
  const walk = (dir: string): Array<[string, string]> => fs.readdirSync(dir, { withFileTypes: true })
    .sort((a, b) => a.name.localeCompare(b.name)).flatMap(e => e.isDirectory() ? walk(path.join(dir, e.name))
      : [[path.relative(root, path.join(dir, e.name)), fs.readFileSync(path.join(dir, e.name)).toString("hex")]]);
  return JSON.stringify(walk(path.join(root, ".uzi-authority")));
}

for (const state of ["removed", "symlink"] as const) {
  it(`classifies and validates foreign metadata with zero old-source calls after source is ${state}`, async t => {
    const r = await rig(t);
    const owner = { worker: r.old.authorityIncarnation()!, epoch: r.link.predecessorEpoch };
    fs.rmSync(r.predecessor, { recursive: true });
    if (state === "symlink") fs.symlinkSync(r.fresh, r.predecessor);
    const before = authorityBytes(r.root);
    let calls = 0;
    for (const name of ["lstat", "stat", "realpath", "open", "readFile", "readdir", "opendir", "access", "readlink"] as const) {
      const original = fsp[name] as (...args: unknown[]) => Promise<unknown>;
      t.mock.method(fsp, name, async (...args: unknown[]) => {
        const p = String(args[0]);
        if (p === r.predecessor || p.startsWith(r.predecessor + path.sep)) {
          calls++;
          throw new Error(`old-source tripwire ${name}`);
        }
        return original(...args);
      });
    }
    syncBuiltinESMExports();
    t.after(() => { t.mock.restoreAll(); syncBuiltinESMExports(); });
    assert.deepEqual(await r.rt.classifyPinRestorePredecessor(r.predecessor), { kind: "foreign_active", owner });
    await r.within(async () => {
      assert.deepEqual(await r.rt.classifyPinRestorePredecessor(r.predecessor), { kind: "foreign_active", owner });
      await r.validate();
    });
    assert.equal((await peekAuthoritySlotMetadata({ dataDir: r.root, workerToken: TOKEN }, r.predecessor))?.epoch, owner.epoch);
    assert.equal(calls, 0);
    assert.equal(authorityBytes(r.root), before);
    await assert.rejects(r.peek(r.predecessor), /tripwire|resolvable/);
    assert.ok(calls > 0, "ordinary peek retains the physical path walk");
    t.mock.restoreAll();
    syncBuiltinESMExports();
    if (state === "symlink") {
      await assert.rejects(r.peek(r.predecessor), /symlink/);
      await assert.rejects(r.rt.bindSource(r.predecessor), /symlink/);
    }
    assert.equal(authorityBytes(r.root), before);
  });
}

it("predecessor classification never grants admission for missing, local, pending or ended sources", async t => {
  const r = await rig(t);
  assert.deepEqual(await r.rt.classifyPinRestorePredecessor(r.fresh + "-missing"), { kind: "other" });
  assert.equal((await r.rt.classifyPinRestorePredecessor(r.fresh)).kind, "other");
  assert.equal((await r.rt.classifyPinRestorePredecessor(r.predecessor)).kind, "foreign_active");
  const pending = await r.old.withTransaction(() => r.old.hooks.beginDestructive([r.predecessor], "pending"));
  assert.equal((await r.rt.classifyPinRestorePredecessor(r.predecessor)).kind, "other");
  await r.old.withTransaction(() => pending.settle({ ok: true }));
  await r.old.endSourceUse([r.predecessor]);
  const before = authorityBytes(r.root);
  assert.equal((await r.rt.classifyPinRestorePredecessor(r.predecessor)).kind, "other");
  assert.equal((await r.rt.classifySourceAdmission(r.predecessor, undefined)).kind, "admissible", "generic classifier is unchanged");
  assert.equal(authorityBytes(r.root), before);
  for (const p of [r.root, path.join(r.root, ".uzi-authority", "slots"), r.predecessor + "/..", "/outside"]) {
    assert.equal((await r.rt.classifyPinRestorePredecessor(p)).kind, "other");
  }
});

for (const fault of ["mac", "missing-head", "head-window", "replay"] as const) {
  it(`metadata lookup shares authenticated read-only refusal for ${fault}`, async t => {
    const r = await rig(t);
    const slot = (await r.peek(r.predecessor))!;
    const slots = path.join(r.root, ".uzi-authority", "slots");
    const slotFile = path.join(slots, slot.key + ".json");
    const headFile = path.join(r.root, ".uzi-authority", "heads", slot.key.slice(0, 2) + ".json");
    if (fault === "mac") {
      const envelope = JSON.parse(fs.readFileSync(slotFile, "utf8"));
      envelope.mac = "0".repeat(64);
      fs.writeFileSync(slotFile, JSON.stringify(envelope));
    } else if (fault === "missing-head") fs.unlinkSync(headFile);
    else {
      const oldSlot = fs.readFileSync(slotFile);
      const pending = await r.old.withTransaction(() => r.old.hooks.beginDestructive([r.predecessor], "advance"));
      await r.old.withTransaction(() => pending.settle({ ok: true }));
      if (fault === "head-window") {
        // Revert only the head to one sequence behind the current, authentic slot.
        const oldHead = fs.readFileSync(headFile);
        await r.old.withTransaction(() => r.old.hooks.beginDestructive([r.predecessor], "advance-again"));
        // Any older authenticated head is refused; the shared loader never heals it.
        fs.writeFileSync(headFile, oldHead);
      } else fs.writeFileSync(slotFile, oldSlot);
    }
    const before = authorityBytes(r.root);
    assert.equal((await r.rt.classifyPinRestorePredecessor(r.predecessor)).kind, "other");
    await assert.rejects(r.within(() => r.validate()));
    assert.equal(authorityBytes(r.root), before);
  });
}

async function gitRig(t: TestContext) {
  const fx = makeFixture();
  t.after(() => fx.cleanup());
  const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const run = "pin-authority-generation";
  const branch = "agent/issue-2512";
  const key = "issue-2512";
  const clone = (await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, run)).path;
  const git = (args: string[]) => execFileSync("git", ["-C", bare, ...args], { encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } });
  return { cache, bare, run, branch, key, clone, git };
}

it("epoch discovery counts immutable and active raw source occurrences before identity deduplication", async t => {
  const r = await gitRig(t);
  const source = { runId: r.run, clonePath: r.clone };
  const raw = JSON.stringify({ ...source, sourceGeneration: 1, retainedSources: Array(40).fill(source) });
  const epoch = randomUUID();
  const attemptId = "20261008T171401Z-g2-0123456789abcdef";
  const tip = r.git(["rev-parse", "HEAD"]).trim();
  const tree = r.git(["rev-parse", "HEAD^{tree}"]).trim();
  const digest = createHash("sha256").update(raw).digest("hex");
  const envelope: RetainedPinEnvelope = {
    version: 2, runId: r.run, branch: r.branch, key: r.key,
    predecessors: [{ rawJournal: raw, journalDigest: digest, generation: 1, sourceEpoch: epoch }],
    intent: { version: 1, phase: "prepared", link: {
      version: 1, intentId: "count-occurrences", predecessorJournalDigest: digest,
      predecessorGeneration: 1, predecessorEpoch: epoch,
      successor: { runId: r.run, claimGeneration: 2, workerId: "worker", registerNonce: "register",
        authorityIncarnation: randomUUID(), executorIncarnation: null, sourceEpoch: randomUUID(), attemptId,
        clonePath: r.clone + ".attempt-" + attemptId },
      restore: { captureId: "capture", authenticatedRecordDigest: "a".repeat(64), predecessorGeneration: 1,
        bareDir: path.basename(r.bare), pinRef: `refs/uzi-recovery-pin/${r.run}/1`, tip, contentSource: tip,
        expectedHead: tip, expectedTree: tree },
    } },
  };
  r.git(["config", "--local", `uzi-recovery.${r.branch}.clone`, raw]);
  r.git(["config", "--local", `uzi-retained.${r.run}.journal`, JSON.stringify(envelope)]);
  const before = fs.readFileSync(path.join(r.bare, "config"));
  const capped = await r.cache.discoverEpochRetainedSources({ limits: { maxMetadataRecords: 50 } });
  assert.equal(capped.inventoryComplete, false, "82 raw occurrences exceed 50 despite naming only one path");
  assert.ok(capped.unreadable > 0);
  const allowed = await r.cache.discoverEpochRetainedSources({ limits: { maxMetadataRecords: 200 } });
  assert.equal(allowed.inventoryComplete, true, "the same valid metadata fits the larger occurrence budget");
  assert.equal(allowed.physicalPaths, 1, "identity deduplication happens after raw occurrence charging");
  assert.deepEqual(fs.readFileSync(path.join(r.bare, "config")), before);
});

for (const fault of ["none", "config", "directory", "readback"] as const) {
  it(`known-generation legacy successor requires durable exact readback: ${fault}`, async t => {
    const r = await gitRig(t);
    const source = { runId: r.run, clonePath: r.clone };
    await r.cache.markRecoveryCapture(r.bare, r.clone, r.branch, r.run);
    const reservation = await r.cache.reserveRecoveryIteration(r.bare, r.branch, r.key, source, 1, 2, 0, 0);
    await fixtureFetchTracking(r.cache, r.bare, r.clone, r.branch, r.run);
    const tip = r.git(["rev-parse", "HEAD"]).trim();
    await r.cache.recordRecoveryCapture(r.bare, r.branch, r.key, source, reservation.attempts, tip);
    const events: string[] = [];
    const open = fsp.open;
    t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
      const h = await Reflect.apply(open, fsp, args);
      const p = String(args[0]);
      const what = p === path.join(r.bare, "config") ? "config" : p === r.bare ? "directory" : undefined;
      if (what) {
        const sync = h.sync.bind(h);
        t.mock.method(h, "sync", async () => {
          events.push(what);
          if (fault === what) throw new Error("injected successor durability failure");
          await sync();
          if (fault === "readback" && what === "directory") {
            const k = `uzi-recovery.${r.branch}.clone`;
            const raw = r.git(["config", "--local", "--get", k]).slice(0, -1);
            r.git(["config", "--local", k, raw + " "]);
          }
        });
      }
      return h;
    });
    const successor = r.cache.prepareRecoverySuccessor(r.bare, r.branch, r.key, source, reservation.attempts,
      "20261008T171401Z-g2-0123456789abcdef");
    if (fault === "none") {
      const result = await successor;
      const journal = JSON.parse(r.git(["config", "--local", "--get", `uzi-recovery.${r.branch}.clone`]));
      assert.equal(journal.clonePath, result.path);
      assert.equal(journal.sourceGeneration, 2);
    } else await assert.rejects(successor, fault === "readback" ? /read-back differs/ : /injected successor durability/);
    assert.deepEqual(events, fault === "config" ? ["config"] : ["config", "directory"]);
  });
}

it("predecessor classification outside A never drains a queued local end or repairs its head", async t => {
  let armed = false;
  let attempts = 0;
  const r = await rig(t, {
    syncFile: h => h.sync(), syncDir: h => h.sync(),
    rename: async (a, b) => {
      if (armed && path.basename(path.dirname(b)) === "heads") {
        attempts++;
        throw new Error("injected queued-end head failure");
      }
      await fsp.rename(a, b);
    },
  });
  r.rt.retireSink(r.sink);
  armed = true;
  const ended = await r.rt.endSourceUse([r.fresh], { sink: r.sink });
  assert.deepEqual(ended.endedSlots, []);
  assert.equal(await r.rt.awaitSinkQuiescent({ timeoutMs: 5000 }), true);
  const before = authorityBytes(r.root);
  const writes = attempts;
  assert.ok(writes > 0);
  assert.equal(r.rt.heldByOtherSink(r.fresh, new Set()), true);
  assert.equal((await r.rt.classifyPinRestorePredecessor(r.fresh)).kind, "other");
  assert.equal((await r.rt.classifyPinRestorePredecessor(r.predecessor)).kind, "foreign_active");
  assert.equal(attempts, writes);
  assert.equal(authorityBytes(r.root), before);
  assert.equal(r.rt.heldByOtherSink(r.fresh, new Set()), true, "classification did not apply the queued end");
  armed = false;
  await r.rt.withTransaction(async () => undefined);
  assert.equal(r.rt.heldByOtherSink(r.fresh, new Set()), false, "normal transaction still drains");
});

it("predecessor metadata never bypasses the successor's physical lease path checks", async t => {
  const r = await rig(t);
  fs.rmSync(r.fresh, { recursive: true });
  fs.symlinkSync(r.predecessor, r.fresh);
  const before = authorityBytes(r.root);
  assert.equal((await r.rt.classifyPinRestorePredecessor(r.predecessor)).kind, "foreign_active");
  await assert.rejects(r.within(() => r.validate()), /symlink/);
  assert.equal(authorityBytes(r.root), before);
});
