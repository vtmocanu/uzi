import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { test, type TestContext } from "node:test";
import {
  acquireWorkerLifetime, type AuthorityTransaction, withAuthorityTransaction, type WorkerLifetime,
} from "../src/authority-lock.js";
import {
  type AuthorityFsOps, type AuthorityStore, installWorkerLifetime, openAuthorityStore, requireWorkerLifetime, SourceUseLease,
} from "../src/completion-authority.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");

interface Fixture { root: string; lifetime: WorkerLifetime; token: string }
async function fixture(t: TestContext): Promise<Fixture> {
  const root = makeTmp(t);
  return { root, lifetime: await acquireWorkerLifetime(root, testLockOptions()), token: TOKEN };
}
const authorityDir = (f: Fixture): string => path.join(f.root, ".uzi-authority");
const slotsDir = (f: Fixture): string => path.join(authorityDir(f), "slots");
const enrollmentFile = (f: Fixture): string => path.join(authorityDir(f), "enrollment.json");

function tx<T>(f: Fixture, fn: (tx: AuthorityTransaction) => Promise<T>): Promise<T> {
  return withAuthorityTransaction(f.lifetime, { timeoutMs: 5000 }, fn);
}
function store(
  f: Fixture, extra: { workerIncarnation?: string; ops?: AuthorityFsOps; token?: string; maxHeadBytes?: number } = {},
): Promise<AuthorityStore> {
  return tx(f, (t) => openAuthorityStore(t, { workerToken: extra.token ?? f.token, ...extra }));
}
function source(f: Fixture, name = "runner/repo-a"): string {
  const full = path.join(f.root, name);
  fs.mkdirSync(full, { recursive: true });
  return full;
}
const shardFile = (f: Fixture, key: string): string => path.join(authorityDir(f), "heads", `${key.slice(0, 2)}.json`);
function slotFiles(f: Fixture): string[] {
  return fs.existsSync(slotsDir(f)) ? fs.readdirSync(slotsDir(f)) : [];
}

test("enrollment is created once, authenticated, and stores no device or inode identity", async (t) => {
  const f = await fixture(t);
  const first = await store(f);
  const text = fs.readFileSync(enrollmentFile(f), "utf8");
  const parsed = JSON.parse(text) as Record<string, unknown>;
  assert.deepEqual(Object.keys(parsed).sort(), ["dataDir", "layout", "mac", "storageId", "version"]);
  assert.equal(parsed.dataDir, f.root);
  assert.equal(parsed.layout, "uzi-authority-layout/1");
  assert.doesNotMatch(text, /"(?:st_)?(?:dev|ino)"/i);
  assert.equal(fs.statSync(enrollmentFile(f)).mode & 0o7777, 0o600);
  assert.equal(fs.statSync(authorityDir(f)).mode & 0o7777, 0o700);
  const second = await store(f);
  assert.equal(second.storageId, first.storageId);
  assert.equal(fs.readFileSync(enrollmentFile(f), "utf8"), text, "an existing store is never re-enrolled");
});

test("enrollment survives a device or inode change of the same file", async (t) => {
  const f = await fixture(t);
  const first = await store(f);
  const bytes = fs.readFileSync(enrollmentFile(f));
  fs.rmSync(enrollmentFile(f));
  fs.writeFileSync(enrollmentFile(f), bytes, { mode: 0o600 });
  assert.equal((await store(f)).storageId, first.storageId);
});

test("enrollment authentication: tamper, missing MAC, wrong token and replacement all refuse", async (t) => {
  const f = await fixture(t);
  await store(f);
  const original = fs.readFileSync(enrollmentFile(f), "utf8");
  const parsed = JSON.parse(original) as Record<string, string>;

  fs.writeFileSync(enrollmentFile(f), JSON.stringify({ ...parsed, storageId: randomUUID() }) + "\n", { mode: 0o600 });
  await assert.rejects(store(f), /authentication/);

  const { mac: _mac, ...noMac } = parsed;
  fs.writeFileSync(enrollmentFile(f), JSON.stringify(noMac) + "\n", { mode: 0o600 });
  await assert.rejects(store(f), /not authenticated|malformed/);

  fs.writeFileSync(enrollmentFile(f), original, { mode: 0o600 });
  await assert.rejects(store(f, { token: TOKEN + "-other" }), /authentication/);
  await store(f);

  fs.chmodSync(enrollmentFile(f), 0o644);
  await assert.rejects(store(f), /unsafe authority file metadata/);
  fs.chmodSync(enrollmentFile(f), 0o600);

  fs.rmSync(enrollmentFile(f));
  fs.symlinkSync("/etc/hostname", enrollmentFile(f));
  await assert.rejects(store(f), /unreadable authority file/);
});

test("slot files without an enrollment refuse rather than re-enroll", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  await tx(f, (t2) => s.admitSourceUse(t2, source(f)));
  fs.rmSync(enrollmentFile(f));
  await assert.rejects(store(f), /without an enrollment/);
  assert.equal(fs.existsSync(enrollmentFile(f)), false);
});

test("an alternate logical root spelling refuses; a copied enrollment at another root refuses", async (t) => {
  const base = makeTmp(t);
  fs.mkdirSync(path.join(base, "real", "data"), { recursive: true });
  fs.symlinkSync(path.join(base, "real"), path.join(base, "alias"));
  // A symlinked root itself cannot even take W; a symlinked ancestor takes W but never enrolls.
  await assert.rejects(acquireWorkerLifetime(path.join(base, "alias"), testLockOptions()), /not a real directory/);
  const viaAncestor = path.join(base, "alias", "data");
  const aliased: Fixture = { root: viaAncestor, lifetime: await acquireWorkerLifetime(viaAncestor, testLockOptions()), token: TOKEN };
  await assert.rejects(store(aliased), /canonical spelling/);

  const f = await fixture(t);
  await store(f);
  const other = path.join(makeTmp(t), "copy");
  fs.mkdirSync(path.join(other, ".uzi-authority"), { recursive: true, mode: 0o700 });
  fs.chmodSync(path.join(other, ".uzi-authority"), 0o700);
  fs.copyFileSync(enrollmentFile(f), path.join(other, ".uzi-authority", "enrollment.json"));
  fs.chmodSync(path.join(other, ".uzi-authority", "enrollment.json"), 0o600);
  const moved: Fixture = { root: other, lifetime: await acquireWorkerLifetime(other, testLockOptions()), token: TOKEN };
  await assert.rejects(store(moved), /different data root/);
});

test("unsupported authority storage refuses enrollment", async (t) => {
  const root = makeTmp(t);
  let supported = true;
  const lifetime = await acquireWorkerLifetime(root, testLockOptions({ supportedFs: async () => supported }));
  supported = false;
  await assert.rejects(
    withAuthorityTransaction(lifetime, {}, (t2) => openAuthorityStore(t2, { workerToken: TOKEN })), /ext4 or XFS/);
});

test("admit mints an epoch, re-admit with the open lease retains it, a plain re-admit invalidates the old lease", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src, { executor: "exec-1" }));
  const slot = await tx(f, (t2) => s.readSlot(t2, key));
  assert.equal(slot?.state, "active");
  assert.equal(slot?.owner.executor, "exec-1");
  assert.equal(fs.statSync(path.join(slotsDir(f), `${key}.json`)).mode & 0o7777, 0o600);
  assert.doesNotMatch(fs.readFileSync(path.join(slotsDir(f), `${key}.json`), "utf8"), /"(?:st_)?(?:dev|ino)"/i);
  assert.equal(await tx(f, (t2) => s.admitSourceUse(t2, src, { lease })), lease);
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.epoch, slot?.epoch);

  const next = await tx(f, (t2) => s.admitSourceUse(t2, src));
  assert.notEqual((await tx(f, (t2) => s.readSlot(t2, key)))?.epoch, slot?.epoch);
  await assert.rejects(tx(f, (t2) => s.transition(t2, lease, "claude_ready")), /stale/);
  await tx(f, (t2) => s.transition(t2, next, "claude_ready"));
  assert.throws(() => new SourceUseLease(Symbol("x"), s, key, randomUUID()), /cannot be constructed/);
});

test("a successor worker cannot advance a foreign active epoch; it may supersede a settled one", async (t) => {
  const f = await fixture(t);
  const one = await store(f);
  const two = await store(f, { workerIncarnation: randomUUID() });
  const src = source(f);
  const lease = await tx(f, (t2) => one.admitSourceUse(t2, src));
  await assert.rejects(tx(f, (t2) => two.admitSourceUse(t2, src)), /another worker incarnation/);
  await assert.rejects(tx(f, (t2) => two.transition(t2, lease, "ineligible")), /does not belong/);
  await tx(f, (t2) => one.transition(t2, lease, "ineligible"));
  const taken = await tx(f, (t2) => two.admitSourceUse(t2, src));
  await assert.rejects(tx(f, (t2) => one.transition(t2, lease, "ineligible")), /stale/);
  await tx(f, (t2) => two.transition(t2, taken, "claude_ready"));
});

test("transition table is enforced", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, source(f)));
  await assert.rejects(tx(f, (t2) => s.transition(t2, lease, "sealed")), /illegal transition/);
  await tx(f, (t2) => s.transition(t2, lease, "claude_ready"));
  await tx(f, (t2) => s.transition(t2, lease, "sealed"));
  await assert.rejects(tx(f, (t2) => s.transition(t2, lease, "claude_ready")), /illegal transition/);
  await tx(f, (t2) => s.transition(t2, lease, "ineligible"));
});

test("tampered or malformed slots refuse and block admission", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const file = path.join(slotsDir(f), `${key}.json`);
  const original = fs.readFileSync(file, "utf8");

  const forged = (JSON.parse(original) as Record<string, unknown>);
  forged.state = "sealed";
  fs.writeFileSync(file, JSON.stringify(forged) + "\n", { mode: 0o600 });
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /authentication/);
  await assert.rejects(tx(f, (t2) => s.transition(t2, lease, "claude_ready")), /authentication/);

  fs.writeFileSync(file, '{"version":', { mode: 0o600 });
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /malformed/);

  fs.writeFileSync(file, JSON.stringify({ ...JSON.parse(original) as object, extra: 1 }) + "\n", { mode: 0o600 });
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /malformed/);

  fs.writeFileSync(file, original, { mode: 0o600 });
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.state, "active");
});

test("source paths with symlinks, escapes or odd spelling refuse", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const real = source(f, "real/dir");
  fs.symlinkSync(path.join(f.root, "real"), path.join(f.root, "link"));
  fs.symlinkSync(real, path.join(f.root, "leaf"));
  const refuse = (p: string, re: RegExp): Promise<void> => assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, p)), re);
  await refuse(path.join(f.root, "link", "dir"), /symlink/);
  await refuse(path.join(f.root, "leaf"), /symlink/);
  await refuse(path.join(path.dirname(f.root), "elsewhere"), /outside the data root/);
  await refuse(f.root, /outside the data root/);
  await refuse(`${f.root}/real/../real/dir`, /normalized/);
  await refuse(real + "/", /normalized/);
  await refuse("real/dir", /normalized/);
  await refuse(path.join(f.root, ".uzi-authority", "slots"), /authority storage/);
  await refuse(path.join(f.root, "absent", "child"), /not resolvable/);
  assert.deepEqual(slotFiles(f), []);
  // A final component that does not exist yet is admissible.
  await tx(f, (t2) => s.admitSourceUse(t2, path.join(f.root, "real", "future")));
});

test("pending footprints: running blocks admission, settle clears, a failed settle leaves an ineligible slot that blocks admission", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const op = await tx(f, (t2) => s.beginPending(t2, lease, { kind: "destructive", footprint: "rm -r build" }));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.pending?.status, "running");
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /pending/);
  await assert.rejects(tx(f, (t2) => s.beginPending(t2, lease, { kind: "bare_mutation", footprint: "x" })), /already recorded/);
  await assert.rejects(tx(f, (t2) => s.transition(t2, lease, "claude_ready")), /pending/);
  await assert.rejects(tx(f, (t2) => s.markRetiring(t2, lease, { prior: "a".repeat(64), manifest: [] })), /pending/);
  await tx(f, (t2) => s.settlePending(t2, lease, op, { ok: true }));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.pending, null);
  const again = await tx(f, (t2) => s.admitSourceUse(t2, src));

  const op2 = await tx(f, (t2) => s.beginPending(t2, again, { kind: "bare_mutation", footprint: "chmod" }));
  await assert.rejects(tx(f, (t2) => s.settlePending(t2, again, randomUUID(), { ok: true })), /no matching/);
  await tx(f, (t2) => s.settlePending(t2, again, op2, { ok: false, residue: ["a/b"] }));
  const failed = await tx(f, (t2) => s.readSlot(t2, key));
  assert.equal(failed?.state, "ineligible");
  assert.deepEqual(failed?.pending?.residue, ["a/b"]);
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /pending/);
});

test("retiring blocks admission until retired; a retired tombstone mints a new epoch", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  await tx(f, (t2) => s.markRetiring(t2, lease, { prior: "b".repeat(64), manifest: ["x", "y"] }));
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /retired|retiring/);
  await assert.rejects(tx(f, (t2) => s.markRetired(t2, lease)), /incomplete/);
  await assert.rejects(tx(f, (t2) => s.advanceRetiring(t2, lease, 3)), /invalid retire progress/);
  await tx(f, (t2) => s.advanceRetiring(t2, lease, 1));
  await assert.rejects(tx(f, (t2) => s.advanceRetiring(t2, lease, 0)), /invalid retire progress/);
  await tx(f, (t2) => s.advanceRetiring(t2, lease, 2));
  await tx(f, (t2) => s.markRetired(t2, lease));
  const retired = await tx(f, (t2) => s.readSlot(t2, key));
  assert.equal(retired?.state, "retired");
  const fresh = await tx(f, (t2) => s.admitSourceUse(t2, src));
  assert.notEqual((await tx(f, (t2) => s.readSlot(t2, key)))?.epoch, retired?.epoch);
  await assert.rejects(tx(f, (t2) => s.transition(t2, lease, "claude_ready")), /stale/);
  await tx(f, (t2) => s.transition(t2, fresh, "claude_ready"));
});

function failingOps(flags: { file: boolean; dir: boolean; rename: boolean }): AuthorityFsOps {
  return {
    syncFile: async (h) => { if (flags.file) throw new Error("injected file sync failure"); await h.sync(); },
    syncDir: async (h) => { if (flags.dir) throw new Error("injected dir sync failure"); await h.sync(); },
    rename: async (a, b) => { if (flags.rename) throw new Error("injected rename failure"); fs.renameSync(a, b); },
  };
}

test("a failed durable write never returns a lease or leaves an eligible state", async (t) => {
  const f = await fixture(t);
  const flags = { file: false, dir: false, rename: false };
  const s = await store(f, { ops: failingOps(flags) });
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));

  flags.file = true;
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /injected file sync/);
  assert.equal(await tx(f, (t2) => s.readSlot(t2, key)), null);
  assert.deepEqual(slotFiles(f), [], "no slot and no temporary file remains");
  flags.file = false;

  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  await tx(f, (t2) => s.transition(t2, lease, "claude_ready"));
  await tx(f, (t2) => s.transition(t2, lease, "sealed"));

  flags.rename = true;
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /injected rename/);
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.state, "sealed", "old state intact when nothing was replaced");
  assert.deepEqual(slotFiles(f), [`${key}.json`]);
  flags.rename = false;

  flags.dir = true;
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /injected dir sync/);
  flags.dir = false;
  // The rename may or may not be durable; either way the state is never one that releases.
  // (Reading finishes the head update for a slot renamed just before the failure.)
  const after = await tx(f, (t2) => s.readSlot(t2, key));
  assert.ok(after?.state === "sealed" || after?.state === "active");
  await assert.rejects(tx(f, (t2) => s.transition(t2, lease, "ineligible")), /stale/);
});

test("input the decoder would refuse is rejected before anything is written, so no slot is wedged", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));

  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src, { executor: "" })), /invalid executor/);
  assert.deepEqual(slotFiles(f), []);
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));

  await tx(f, (t2) => s.markRetiring(t2, lease, { prior: "a".repeat(64), manifest: ["x", "y"] }));
  await assert.rejects(tx(f, (t2) => s.advanceRetiring(t2, lease, 0.5)), /invalid retire progress/);
  await assert.rejects(tx(f, (t2) => s.advanceRetiring(t2, lease, Number.NaN)), /invalid retire progress/);
  const retiring = await tx(f, (t2) => s.readSlot(t2, key));
  assert.equal(retiring?.retiring?.deleted, 0);
  await tx(f, (t2) => s.advanceRetiring(t2, lease, 1));

  // 1024 manifest items of 512 three-byte characters pass the per-item limits yet exceed the file bound.
  const other = await tx(f, (t2) => s.admitSourceUse(t2, source(f, "runner/repo-b")));
  const otherKey = await tx(f, (t2) => s.slotKey(t2, source(f, "runner/repo-b")));
  const big = Array.from({ length: 1024 }, (_, i) => String(i).padStart(4, "0") + "€".repeat(508));
  await assert.rejects(tx(f, (t2) => s.markRetiring(t2, other, { prior: "c".repeat(64), manifest: big })), /exceeds bound/);
  assert.equal((await tx(f, (t2) => s.readSlot(t2, otherKey)))?.state, "active", "slot still readable and unchanged");
  await tx(f, (t2) => s.transition(t2, other, "claude_ready"));
});

test("replaying an older authentic slot file refuses and keeps refusing", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const file = path.join(slotsDir(f), `${key}.json`);
  const old = fs.readFileSync(file);
  const op = await tx(f, (t2) => s.beginPending(t2, lease, { kind: "destructive", footprint: "rm -r build" }));
  await tx(f, (t2) => s.settlePending(t2, lease, op, { ok: false, residue: ["build"] }));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.state, "ineligible");

  fs.writeFileSync(file, old, { mode: 0o600 });
  const restarted = await store(f); // a fresh store has no in-process memory: the head alone must catch it
  for (const who of [s, restarted]) {
    await assert.rejects(tx(f, (t2) => who.readSlot(t2, key)), /replay/);
    await assert.rejects(tx(f, (t2) => who.admitSourceUse(t2, src)), /replay/);
  }
  await assert.rejects(tx(f, (t2) => s.transition(t2, lease, "claude_ready")), /replay/);
});

test("the in-process high-water mark refuses a replay of the slot and head together", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const file = path.join(slotsDir(f), `${key}.json`);
  const headFile = shardFile(f, key);
  const oldSlot = fs.readFileSync(file);
  const oldHead = fs.readFileSync(headFile);
  await tx(f, (t2) => s.transition(t2, lease, "ineligible"));
  fs.writeFileSync(file, oldSlot, { mode: 0o600 });
  fs.writeFileSync(headFile, oldHead, { mode: 0o600 });
  // A whole-store rollback is outside the guarantee across a restart; within the process it is caught,
  // even by a store reopened after the write (the mark is per storage id, not per store instance).
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /replay/);
  const reopened = await store(f);
  await assert.rejects(tx(f, (t2) => reopened.readSlot(t2, key)), /replay/);
});

test("the head tracks every slot write and survives its own crash window", async (t) => {
  const f = await fixture(t);
  const src = source(f);
  const flags = { head: false };
  const s = await store(f, {
    ops: {
      syncFile: (h) => h.sync(), syncDir: (h) => h.sync(),
      rename: async (a, b) => {
        if (flags.head && path.basename(path.dirname(b)) === "heads") throw new Error("injected head failure");
        fs.renameSync(a, b);
      },
    },
  });
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const headFile = shardFile(f, key);
  const head = (): Record<string, number> => (JSON.parse(fs.readFileSync(headFile, "utf8")) as { slots: Record<string, number> }).slots;
  assert.equal(fs.existsSync(headFile), false, "a shard exists only once a slot in it was written");

  flags.head = true;
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /injected head failure/);
  assert.equal(fs.existsSync(headFile), false, "slot renamed, head not yet updated");
  assert.equal(slotFiles(f).length, 1);
  flags.head = false;

  const restarted = await store(f);
  assert.equal((await tx(f, (t2) => restarted.readSlot(t2, key)))?.seq, 1);
  assert.deepEqual(head(), { [key]: 1 }, "completed under the transaction");
  const lease = await tx(f, (t2) => restarted.admitSourceUse(t2, src));
  await tx(f, (t2) => restarted.transition(t2, lease, "claude_ready"));
  assert.deepEqual(head(), { [key]: 3 });
});

test("a rolled-back or deleted head, or a deleted slot, refuses", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const headFile = shardFile(f, key);
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const oldHead = fs.readFileSync(headFile);
  await tx(f, (t2) => s.transition(t2, lease, "claude_ready"));
  await tx(f, (t2) => s.transition(t2, lease, "ineligible"));
  const newHead = fs.readFileSync(headFile);

  fs.writeFileSync(headFile, oldHead, { mode: 0o600 });
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /ahead of the recorded head/);

  fs.writeFileSync(headFile, JSON.stringify({ ...JSON.parse(newHead.toString()) as object, slots: {} }) + "\n", { mode: 0o600 });
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /authentication/);

  fs.rmSync(headFile);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /ahead of the recorded head/);

  fs.rmSync(path.join(authorityDir(f), "heads"), { recursive: true });
  await assert.rejects(store(f), /without a head/);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /head missing/);

  const g = await fixture(t);
  const gs = await store(g);
  const gsrc = source(g);
  const gkey = await tx(g, (t2) => gs.slotKey(t2, gsrc));
  await tx(g, (t2) => gs.admitSourceUse(t2, gsrc));
  fs.rmSync(path.join(slotsDir(g), `${gkey}.json`));
  await assert.rejects(tx(g, (t2) => gs.admitSourceUse(t2, gsrc)), /slot missing/);
});

test("a directory swapped under a write is refused before anything is renamed into the store", async (t) => {
  const f = await fixture(t);
  let swap = false;
  const s = await store(f, {
    ops: {
      syncFile: async (h) => {
        await h.sync();
        if (!swap) return;
        swap = false;
        fs.renameSync(slotsDir(f), slotsDir(f) + ".old");
        fs.mkdirSync(slotsDir(f), { mode: 0o700 });
      },
      syncDir: (h) => h.sync(),
      rename: async (a, b) => { fs.renameSync(a, b); },
    },
  });
  const src = source(f);
  await tx(f, (t2) => s.slotKey(t2, src));
  fs.mkdirSync(slotsDir(f), { recursive: true, mode: 0o700 });
  swap = true;
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /identity changed/);
  assert.deepEqual(slotFiles(f), [], "nothing renamed into the replacement directory");
  assert.deepEqual(fs.readdirSync(slotsDir(f) + ".old"), [], "nor into the original, and the synced tmp file is not leaked there");
});

/** Slot paths whose keys share one head shard, found by brute force over the 256 shards. */
async function sameShardSources(f: Fixture, s: AuthorityStore, count: number): Promise<{ src: string; key: string }[]> {
  const byShard = new Map<string, { src: string; key: string }[]>();
  for (let i = 0; i < 4000; i++) {
    const src = source(f, `runner/probe-${i}`);
    const key = await tx(f, (t2) => s.slotKey(t2, src));
    const list = byShard.get(key.slice(0, 2)) ?? [];
    list.push({ src, key });
    byShard.set(key.slice(0, 2), list);
    if (list.length === count) return list;
  }
  throw new Error("no shard collision found");
}

test("the per-shard head bound is checked before the slot is written, and other shards keep working", async (t) => {
  const f = await fixture(t);
  const probe = await store(f);
  const [a, b] = await sameShardSources(f, probe, 2) as [{ src: string; key: string }, { src: string; key: string }];
  await tx(f, (t2) => probe.admitSourceUse(t2, a.src));
  const oneEntry = fs.statSync(shardFile(f, a.key)).size;

  // Room for the shard with one key, not two.
  const small = await store(f, { maxHeadBytes: oneEntry + 20 });
  await assert.rejects(tx(f, (t2) => small.admitSourceUse(t2, b.src)), /head exceeds bound/);
  assert.equal(fs.existsSync(path.join(slotsDir(f), `${b.key}.json`)), false, "no slot is written that its head cannot record");
  assert.equal(await tx(f, (t2) => small.readSlot(t2, b.key)), null);

  // The full-shard condition is local to that shard: a key in another shard is still admitted.
  let other: { src: string; key: string } | undefined;
  for (let i = 0; !other; i++) {
    const src = source(f, `runner/other-${i}`);
    const key = await tx(f, (t2) => small.slotKey(t2, src));
    if (key.slice(0, 2) !== a.key.slice(0, 2)) other = { src, key };
  }
  await tx(f, (t2) => small.admitSourceUse(t2, other.src));
  assert.equal((await tx(f, (t2) => small.readSlot(t2, other.key)))?.seq, 1);
});

test("head shards are bound to their name and cannot be swapped", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const [a, b] = await (async () => {
    const found: { src: string; key: string }[] = [];
    for (let i = 0; found.length < 2; i++) {
      const src = source(f, `runner/swap-${i}`);
      const key = await tx(f, (t2) => s.slotKey(t2, src));
      if (found.every((x) => x.key.slice(0, 2) !== key.slice(0, 2))) found.push({ src, key });
    }
    return found as [{ src: string; key: string }, { src: string; key: string }];
  })();
  await tx(f, (t2) => s.admitSourceUse(t2, a.src));
  await tx(f, (t2) => s.admitSourceUse(t2, b.src));
  fs.copyFileSync(shardFile(f, a.key), shardFile(f, b.key));
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, b.key)), /does not match its name/);
});

test("a head shard stays far below the file bound with many retired tombstones kept", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  for (let i = 0; i < 40; i++) {
    const src = source(f, `runner/many-${i}`);
    const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
    await tx(f, (t2) => s.markRetiring(t2, lease, { prior: "e".repeat(64), manifest: [] }));
    await tx(f, (t2) => s.markRetired(t2, lease));
  }
  const heads = fs.readdirSync(path.join(authorityDir(f), "heads"));
  assert.ok(heads.length > 1, "writes spread over several shards");
  for (const name of heads) assert.ok(fs.statSync(path.join(authorityDir(f), "heads", name)).size < 4096);
});

test("a stale or foreign lease passed to admit refuses instead of minting a new epoch", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const stale = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const current = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const before = await tx(f, (t2) => s.readSlot(t2, key));
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src, { lease: stale })), /stale/);
  assert.deepEqual(await tx(f, (t2) => s.readSlot(t2, key)), before, "nothing was written");
  assert.equal(await tx(f, (t2) => s.admitSourceUse(t2, src, { lease: current })), current);

  const foreign = await tx(f, (t2) => s.admitSourceUse(t2, source(f, "runner/repo-b")));
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src, { lease: foreign })), /does not belong/);

  await tx(f, (t2) => s.transition(t2, current, "ineligible"));
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src, { lease: current })), /stale/);
});

test("a failed footprint's residue cannot be discarded by a retire that does not cover it", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const op = await tx(f, (t2) => s.beginPending(t2, lease, { kind: "destructive", footprint: "rm -r build" }));
  await tx(f, (t2) => s.settlePending(t2, lease, op, { ok: false, residue: ["a/b", "c"] }));
  await assert.rejects(tx(f, (t2) => s.markRetiring(t2, lease, { prior: "d".repeat(64), manifest: ["a/b"] })), /residue/);
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.state, "ineligible");
  await tx(f, (t2) => s.markRetiring(t2, lease, { prior: "d".repeat(64), manifest: ["c", "a/b"] }));
  await tx(f, (t2) => s.advanceRetiring(t2, lease, 2));
  await tx(f, (t2) => s.markRetired(t2, lease));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.state, "retired");
});

test("authority operations require an active transaction of the same root", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  let captured: AuthorityTransaction | undefined;
  await tx(f, async (t2) => { captured = t2; });
  await assert.rejects(s.admitSourceUse(captured!, src), /active authority transaction/);
  await assert.rejects(s.readSlot({} as AuthorityTransaction, "a".repeat(64)), /active authority transaction/);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, "not-a-key")), /invalid slot key/);
});

test("the worker lifetime accessor refuses before install and exposes only the installed capability", async (t) => {
  const f = await fixture(t);
  assert.throws(() => requireWorkerLifetime(), /not acquired/);
  installWorkerLifetime(f.lifetime);
  assert.equal(requireWorkerLifetime(), f.lifetime);
  assert.throws(() => installWorkerLifetime(f.lifetime), /already installed/);
});
