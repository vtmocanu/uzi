import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHmac, randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { test, type TestContext } from "node:test";
import {
  acquireWorkerLifetime, AuthorityRefusal, type AuthorityTransaction, withAuthorityTransaction, type WorkerLifetime,
} from "../src/authority-lock.js";
import {
  type AuthorityFsOps, AuthorityStore, installWorkerLifetime, leaseEpoch, leaseHarness, leaseSlotKey, openAuthorityStore,
  requireWorkerLifetime, SourceUseLease,
} from "../src/completion-authority.js";
import { buildClaudeSourceCore, buildCodexSourceCore, type CompletionSourceV2, sourceCoreDigest } from "../src/completion-source.js";
import { ExecutorLifetime } from "../src/codex/executor-lifetime.js";
import { agentRoot, makeTmp, testLockOptions } from "./authority-test-support.js";

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
const CAPS = ["11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"];

// Publication needs a certified source for the lease, so the tests remember each lease's path (by
// wrapping admission once) and, for codex leases, the lifetime that sealed it.
const leasePaths = new WeakMap<SourceUseLease, string>();
const leaseSeals = new WeakMap<SourceUseLease, { lifetime: ExecutorLifetime; seal: unknown }>();
const leaseSources = new WeakMap<SourceUseLease, CompletionSourceV2>();
{
  const realAdmit = AuthorityStore.prototype.admitSourceUse;
  AuthorityStore.prototype.admitSourceUse = async function (this: AuthorityStore, ...args: Parameters<typeof realAdmit>) {
    const lease = await realAdmit.apply(this, args);
    leasePaths.set(lease, args[1]);
    return lease;
  };
}
/** Admit a codex slot: the executor id is the lifetime's, and the lease is sealed for it. */
async function admitCodex(s: AuthorityStore, t2: AuthorityTransaction, src: string): Promise<SourceUseLease> {
  const lifetime = new ExecutorLifetime();
  const lease = await s.admitSourceUse(t2, src, { executor: (lifetime as unknown as { executorIncarnation: string }).executorIncarnation });
  const identity = {};
  lifetime.inventory(identity, () => true);
  lifetime.bindFacade(identity, {}, () => true);
  const sealed = await lifetime.seal(lease, async () => undefined, () => true);
  assert.equal(sealed.kind, "sealed");
  leaseSeals.set(lease, { lifetime, seal: sealed.kind === "sealed" ? sealed.seal : undefined });
  return lease;
}
/** Build and certify a source for the lease's own harness (cached per lease), without transitioning. */
async function certifiedFor(f: Fixture, s: AuthorityStore, t2: AuthorityTransaction, lease: SourceUseLease): Promise<CompletionSourceV2> {
  const cached = leaseSources.get(lease);
  if (cached) return cached;
  const clonePath = leasePaths.get(lease) as string;
  const sha = "a".repeat(40);
  const context = {
    runId: "run-1", branch: "agent/issue-1", kind: "issue" as const, barePath: path.join(f.root, "bare"), generation: 2,
    defaultIdentity: { ref: "refs/heads/main", sha },
  };
  const base = { context, clonePath, expectedHead: sha, roots: [{ sha: "b".repeat(40), contexts: [context] }] };
  const seals = leaseSeals.get(lease);
  const core = leaseHarness(lease) === "codex" && seals
    ? buildCodexSourceCore({ store: s, base, lease, seal: seals.seal, accepts: (seal, use) => seals.lifetime.accepts(seal, use) })
    : buildClaudeSourceCore({ store: s, base, lease });
  const certified = await s.certifySource(t2, lease, core);
  leaseSources.set(lease, certified);
  return certified;
}
/** Certify (once) and publish; returns the certified source that was published. */
async function publish(
  f: Fixture, s: AuthorityStore, t2: AuthorityTransaction, lease: SourceUseLease, to: "claude_ready" | "sealed", captureIds: string[] = CAPS,
): Promise<CompletionSourceV2> {
  const source = await certifiedFor(f, s, t2, lease);
  await s.transition(t2, lease, to, { source, captureIds });
  return source;
}
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
  assert.equal(await tx(f, (t2) => s.admitSourceUse(t2, src, { lease, executor: "exec-1" })), lease);
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src, { lease, executor: "exec-other" })), /another executor/);
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.epoch, slot?.epoch);

  const next = await tx(f, (t2) => s.admitSourceUse(t2, src));
  assert.notEqual((await tx(f, (t2) => s.readSlot(t2, key)))?.epoch, slot?.epoch);
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, lease, "claude_ready")), /stale/);
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, next, "claude_ready")), /admitted as codex/, "the slot was admitted with an executor once");
  const plain = await tx(f, (t2) => s.admitSourceUse(t2, source(f, "runner/plain")));
  await tx(f, (t2) => publish(f, s, t2, plain, "claude_ready"));
  assert.throws(() => new SourceUseLease(Symbol("x"), s, key, randomUUID(), "claude"), /cannot be constructed/);
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
  await tx(f, (t2) => publish(f, two, t2, taken, "claude_ready"));
});

test("transition table is enforced", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, source(f)));
  await tx(f, (t2) => publish(f, s, t2, lease, "claude_ready"));
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, lease, "sealed")), /illegal transition/);
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, lease, "claude_ready")), /illegal transition/);
  await tx(f, (t2) => s.transition(t2, lease, "ineligible"));
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, lease, "sealed")), /illegal transition/);

  const codexLease = await tx(f, (t2) => admitCodex(s, t2, source(f, "runner/repo-codex")));
  await tx(f, (t2) => publish(f, s, t2, codexLease, "sealed"));
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, codexLease, "claude_ready")), /illegal transition/);
  await tx(f, (t2) => s.transition(t2, codexLease, "ineligible"));
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
  // A sealed state without publication data is refused by the decoder before the MAC is even checked.
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /authentication|malformed/);
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, lease, "claude_ready")), /authentication|malformed/);

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
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, lease, "claude_ready")), /pending/);
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
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, lease, "claude_ready")), /stale/);
  await tx(f, (t2) => publish(f, s, t2, fresh, "claude_ready"));
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

  const lease = await tx(f, (t2) => admitCodex(s, t2, src));
  await tx(f, (t2) => publish(f, s, t2, lease, "sealed"));

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
  await tx(f, (t2) => publish(f, s, t2, other, "claude_ready"));
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
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, lease, "claude_ready")), /replay/);
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
  assert.deepEqual(head(), {}, "every shard exists, empty, from enrollment on");

  flags.head = true;
  await assert.rejects(tx(f, (t2) => s.admitSourceUse(t2, src)), /injected head failure/);
  assert.deepEqual(head(), {}, "slot renamed, head not yet updated");
  assert.equal(slotFiles(f).length, 1);
  flags.head = false;

  const restarted = await store(f);
  assert.equal((await tx(f, (t2) => restarted.readSlot(t2, key)))?.seq, 1);
  assert.deepEqual(head(), { [key]: 1 }, "completed under the transaction");
  const lease = await tx(f, (t2) => restarted.admitSourceUse(t2, src));
  await tx(f, (t2) => publish(f, restarted, t2, lease, "claude_ready"));
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
  await tx(f, (t2) => publish(f, s, t2, lease, "claude_ready"));
  await tx(f, (t2) => s.transition(t2, lease, "ineligible"));
  const newHead = fs.readFileSync(headFile);

  fs.writeFileSync(headFile, oldHead, { mode: 0o600 });
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /ahead of the recorded head/);

  fs.writeFileSync(headFile, JSON.stringify({ ...JSON.parse(newHead.toString()) as object, slots: {} }) + "\n", { mode: 0o600 });
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /authentication/);

  fs.rmSync(headFile);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /authority head missing/);

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

test("sealed and claude_ready publish the digest and cohort, and require the matching harness", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const cSrc = source(f, "runner/claude");
  const xSrc = source(f, "runner/codex");
  const cKey = await tx(f, (t2) => s.slotKey(t2, cSrc));
  const cLease = await tx(f, (t2) => s.admitSourceUse(t2, cSrc));
  const xLease = await tx(f, (t2) => admitCodex(s, t2, xSrc));
  assert.equal(leaseSlotKey(cLease), cKey);
  assert.equal(leaseEpoch(cLease), (await tx(f, (t2) => s.readSlot(t2, cKey)))?.epoch);
  assert.equal(leaseHarness(cLease), "claude");
  assert.equal(leaseHarness(xLease), "codex");
  assert.equal(await tx(f, (t2) => s.readPublication(t2, cSrc)), null, "an active slot publishes nothing");

  await assert.rejects(tx(f, (t2) => publish(f, s, t2, cLease, "sealed")), /requires harness codex/, "a claude lease cannot seal");
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, xLease, "claude_ready")), /requires harness claude/, "a codex lease cannot become claude_ready");
  const loose = s.transition.bind(s) as (...a: unknown[]) => Promise<void>;
  await assert.rejects(tx(f, (t2) => loose(t2, cLease, "claude_ready")), /publication data required/);
  await assert.rejects(tx(f, (t2) => loose(t2, xLease, "sealed")), /publication data required/);
  await assert.rejects(tx(f, (t2) => loose(t2, cLease, "ineligible", { source: {}, captureIds: [] })), /no publication/);
  assert.equal((await tx(f, (t2) => s.readSlot(t2, cKey)))?.state, "active", "no refused call wrote anything");

  const claudeSource = await tx(f, (t2) => publish(f, s, t2, cLease, "claude_ready"));
  const codexSource = await tx(f, (t2) => publish(f, s, t2, xLease, "sealed"));
  const cohort = { runId: "run-1", generation: 2, captureIds: CAPS };
  assert.deepEqual(await tx(f, (t2) => s.readPublication(t2, cSrc)), {
    harness: "claude", sourceCoreDigest: sourceCoreDigest(claudeSource), cohort, state: "claude_ready", epoch: leaseEpoch(cLease),
  });
  assert.deepEqual(await tx(f, (t2) => s.readPublication(t2, xSrc)), {
    harness: "codex", sourceCoreDigest: sourceCoreDigest(codexSource), cohort, state: "sealed", epoch: leaseEpoch(xLease),
  });
  assert.equal(await tx(f, (t2) => s.readPublication(t2, source(f, "runner/never"))), null);
});

test("a codex slot can never become claude_ready: the harness is bound at admission and the marker is sticky", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  // (a) admitted with an executor, then a claude_ready attempt.
  const aSrc = source(f, "runner/a");
  const aLease = await tx(f, (t2) => s.admitSourceUse(t2, aSrc, { executor: "exec-1" }));
  const aKey = await tx(f, (t2) => s.slotKey(t2, aSrc));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, aKey)))?.codexMarked, true);
  await assert.rejects(tx(f, (t2) => s.transition(t2, aLease, "claude_ready", { source: {}, captureIds: [] })), /requires harness claude/);

  // (b) a sealed codex slot re-admitted by a successor (publication wiped) is still not claude_ready-able.
  const bSrc = source(f, "runner/b");
  const bKey = await tx(f, (t2) => s.slotKey(t2, bSrc));
  const bLease = await tx(f, (t2) => admitCodex(s, t2, bSrc));
  await tx(f, (t2) => publish(f, s, t2, bLease, "sealed"));
  const successor = await tx(f, (t2) => s.admitSourceUse(t2, bSrc));
  const reslot = await tx(f, (t2) => s.readSlot(t2, bKey));
  assert.equal(reslot?.publication, null, "re-admission wipes the publication");
  assert.equal(reslot?.owner.executor, null);
  assert.equal(reslot?.codexMarked, true, "the marker outlives the wipe");
  assert.equal(leaseHarness(successor), "claude");
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, successor, "claude_ready")), /admitted as codex/);
  await assert.rejects(tx(f, (t2) => s.transition(t2, successor, "sealed", { source: {}, captureIds: [] })), /requires harness codex/, "a claude lease cannot seal either");

  // The marker survives ineligible, retiring and the tombstone, so a reused path is still codex.
  await tx(f, (t2) => s.transition(t2, successor, "ineligible"));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, bKey)))?.codexMarked, true);
  await tx(f, (t2) => s.markRetiring(t2, successor, { prior: "a".repeat(64), manifest: [] }));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, bKey)))?.codexMarked, true);
  await tx(f, (t2) => s.markRetired(t2, successor));
  const tomb = await tx(f, (t2) => s.readSlot(t2, bKey));
  assert.equal(tomb?.state, "retired");
  assert.equal(tomb?.codexMarked, true, "even the tombstone keeps it");
  const reused = await tx(f, (t2) => s.admitSourceUse(t2, bSrc));
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, reused, "claude_ready")), /admitted as codex/);
  assert.equal((await tx(f, (t2) => s.readSlot(t2, bKey)))?.state, "active");

  // A claude-only slot is never marked, and a codex re-admission marks it for good.
  const cSrc = source(f, "runner/c");
  const cKey = await tx(f, (t2) => s.slotKey(t2, cSrc));
  await tx(f, (t2) => s.admitSourceUse(t2, cSrc));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, cKey)))?.codexMarked, false);
  await tx(f, (t2) => s.admitSourceUse(t2, cSrc, { executor: "exec-2" }));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, cKey)))?.codexMarked, true);
  const next = await tx(f, (t2) => s.admitSourceUse(t2, cSrc));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, cKey)))?.codexMarked, true);
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, next, "claude_ready")), /admitted as codex/);
});

test("the decoder refuses an executor or codex publication without the marker, claude_ready with it, and a retired slot with a publication", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const lease = await tx(f, (t2) => admitCodex(s, t2, src));
  const file = path.join(slotsDir(f), `${key}.json`);
  const active = fs.readFileSync(file, "utf8");
  const forge = (mutate: (o: Record<string, any>) => void, base: string): void => {
    const o = JSON.parse(base) as Record<string, any>;
    mutate(o);
    fs.writeFileSync(file, JSON.stringify(o) + "\n", { mode: 0o600 });
  };
  forge((o) => { o.codexMarked = false; }, active);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /malformed/, "an executor without the marker");
  forge((o) => { delete o.codexMarked; }, active);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /malformed/);
  fs.writeFileSync(file, active, { mode: 0o600 });

  await tx(f, (t2) => publish(f, s, t2, lease, "sealed"));
  const sealed = fs.readFileSync(file, "utf8");
  forge((o) => { o.state = "retired"; }, sealed);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /malformed/, "a retired slot cannot carry a publication");
  forge((o) => { o.codexMarked = false; o.owner.executor = null; }, sealed);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /malformed/, "a codex publication without the marker");
  forge((o) => { o.state = "claude_ready"; o.publication.harness = "claude"; }, sealed);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, key)), /malformed/, "claude_ready in a marked slot");
  fs.writeFileSync(file, sealed, { mode: 0o600 });
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.state, "sealed");
});

test("publication takes the certified source: any foreign epoch, slot, worker, executor, certificate or cohort is refused", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const aSrc = source(f, "runner/pa");
  const bSrc = source(f, "runner/pb");
  const aLease = await tx(f, (t2) => s.admitSourceUse(t2, aSrc));
  const bLease = await tx(f, (t2) => s.admitSourceUse(t2, bSrc));
  const aSource = await tx(f, (t2) => certifiedFor(f, s, t2, aLease));
  const bSource = await tx(f, (t2) => certifiedFor(f, s, t2, bLease));
  const put = (lease: SourceUseLease, source: unknown, captureIds: unknown = CAPS): Promise<void> =>
    tx(f, (t2) => s.transition(t2, lease, "claude_ready", { source, captureIds: captureIds as string[] }));

  await assert.rejects(put(aLease, bSource), /another source-use epoch/, "another slot's source");
  await assert.rejects(put(aLease, { ...aSource, certificate: undefined }), /malformed/);
  const { certificate: _c, ...uncertified } = aSource;
  await assert.rejects(put(aLease, uncertified), /certificate does not verify/);
  await assert.rejects(put(aLease, { ...aSource, certificate: bSource.certificate }), /certificate does not verify/);
  await assert.rejects(put(aLease, { ...aSource, expectedHead: "c".repeat(40) }), /certificate does not verify/, "a digest the certificate does not cover");
  await assert.rejects(put(aLease, { ...aSource, extra: 1 }), /malformed/);
  await assert.rejects(put(aLease, { ...aSource, workerIncarnation: randomUUID() }), /another worker incarnation/);
  // A certificate-bearing source for another slot, rebound to this lease's epoch, still names the wrong slot.
  const wrongSlot = { ...bSource, sourceUseEpoch: leaseEpoch(aLease) };
  await assert.rejects(put(aLease, wrongSlot), /another slot|certificate does not verify/);
  await assert.rejects(put(aLease, aSource, "not-an-array"), /malformed/);
  assert.equal((await tx(f, (t2) => s.readPublication(t2, aSrc))), null, "nothing was written");
  await put(aLease, aSource);
  const read = await tx(f, (t2) => s.readPublication(t2, aSrc));
  assert.equal(read?.cohort.runId, aSource.context.runId, "the cohort run and generation come from the source");
  assert.equal(read?.cohort.generation, aSource.context.generation);

  // Codex: the source's executor must be the slot owner's.
  const xSrc = source(f, "runner/px");
  const xLease = await tx(f, (t2) => admitCodex(s, t2, xSrc));
  const xSource = await tx(f, (t2) => certifiedFor(f, s, t2, xLease));
  const wrongExecutor = await tx(f, (t2) => s.certifySource(t2, xLease, { ...xSource, executorIncarnation: "exec-forged" }).catch((e: unknown) => e));
  assert.match(String((wrongExecutor as Error).message), /executor does not match/);
  await tx(f, (t2) => s.transition(t2, xLease, "sealed", { source: xSource, captureIds: CAPS }));
});

test("publication data is validated before anything is written", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const bad = (captureIds: unknown): Promise<void> => assert.rejects(tx(f, async (t2) => {
    const source = await certifiedFor(f, s, t2, lease);
    await s.transition(t2, lease, "claude_ready", { source, captureIds: captureIds as string[] });
  }), /malformed/);
  const [a, b] = CAPS as [string, string];
  await bad([b, a]);
  await bad([a, a]);
  await bad([""]);
  await bad(["cap-a"]);
  await bad(["AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"]);
  await bad([`maintenance-v2-${a}`]);
  await bad([`${a}\n`]);
  await bad([5]);
  await bad(Array.from({ length: 257 }, (_, i) => `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`));
  const slot = await tx(f, (t2) => s.readSlot(t2, key));
  assert.equal(slot?.state, "active");
  assert.equal(slot?.publication, null);
  await tx(f, (t2) => publish(f, s, t2, lease, "claude_ready", [a, `maintenance-v1-${b}`].sort()));
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.state, "claude_ready");
});

test("a published slot refuses bare mutation until a fresh epoch is admitted", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const cSrc = source(f, "runner/pc");
  const xSrc = source(f, "runner/pd");
  const cLease = await tx(f, (t2) => s.admitSourceUse(t2, cSrc));
  const xLease = await tx(f, (t2) => admitCodex(s, t2, xSrc));
  for (const [lease, to, src] of [[cLease, "claude_ready", cSrc], [xLease, "sealed", xSrc]] as const) {
    await tx(f, (t2) => publish(f, s, t2, lease, to));
    const key = await tx(f, (t2) => s.slotKey(t2, src));
    for (const kind of ["bare_mutation", "destructive"] as const) {
      await assert.rejects(tx(f, (t2) => s.beginPending(t2, lease, { kind, footprint: "x" })), /published/);
    }
    const slot = await tx(f, (t2) => s.readSlot(t2, key));
    assert.equal(slot?.pending, null, "nothing was recorded");
    assert.equal(slot?.state, to);
  }
  const fresh = await tx(f, (t2) => s.admitSourceUse(t2, cSrc));
  await tx(f, (t2) => s.beginPending(t2, fresh, { kind: "bare_mutation", footprint: "x" }));
});

test("a stale lease cannot publish, and a pending footprint blocks publication and reads", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const stale = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, stale, "claude_ready")), /stale/);
  assert.equal(await tx(f, (t2) => s.readPublication(t2, src)), null);

  await tx(f, (t2) => s.beginPending(t2, lease, { kind: "bare_mutation", footprint: "x" }));
  await assert.rejects(tx(f, (t2) => publish(f, s, t2, lease, "claude_ready")), /pending/);
  await assert.rejects(tx(f, (t2) => s.readPublication(t2, src)), /pending/);
  assert.equal((await tx(f, (t2) => s.readSlot(t2, key)))?.publication, null);
});

test("publication survives ineligible and retiring, is cleared by retire, and is authenticated", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  const lease = await tx(f, (t2) => admitCodex(s, t2, src));
  await tx(f, (t2) => publish(f, s, t2, lease, "sealed"));
  const file = path.join(slotsDir(f), `${key}.json`);
  const original = fs.readFileSync(file, "utf8");

  const forged = JSON.parse(original) as { publication: { harness: string; sourceCoreDigest: string } };
  forged.publication.harness = "claude";
  fs.writeFileSync(file, JSON.stringify(forged) + "\n", { mode: 0o600 });
  await assert.rejects(tx(f, (t2) => s.readPublication(t2, src)), /malformed/, "harness must match the state");
  forged.publication.harness = "codex";
  forged.publication.sourceCoreDigest = "e".repeat(64);
  fs.writeFileSync(file, JSON.stringify(forged) + "\n", { mode: 0o600 });
  await assert.rejects(tx(f, (t2) => s.readPublication(t2, src)), /authentication/);
  fs.writeFileSync(file, original, { mode: 0o600 });

  await tx(f, (t2) => s.transition(t2, lease, "ineligible"));
  assert.equal((await tx(f, (t2) => s.readPublication(t2, src)))?.state, "ineligible");
  await tx(f, (t2) => s.markRetiring(t2, lease, { prior: "a".repeat(64), manifest: [] }));
  assert.equal((await tx(f, (t2) => s.readPublication(t2, src)))?.state, "retiring");
  await tx(f, (t2) => s.markRetired(t2, lease));
  assert.equal(await tx(f, (t2) => s.readPublication(t2, src)), null);
});

test("source certificates are minted for a live lease and checked under a live transaction with the store key", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const lease = await tx(f, (t2) => s.admitSourceUse(t2, src));
  const certified = await tx(f, (t2) => certifiedFor(f, s, t2, lease));
  const digest = sourceCoreDigest(certified);
  const cert = certified.certificate as string;
  assert.match(cert, /^[0-9a-f]{64}$/);
  assert.equal(await tx(f, async (t2) => s.verifySourceCertificate(t2, digest, cert)), true);
  assert.equal(await tx(f, async (t2) => s.verifySourceCertificate(t2, "b".repeat(64), cert)), false);
  assert.equal(await tx(f, async (t2) => s.verifySourceCertificate(t2, digest, cert.replace(/^./, "0"))), cert[0] === "0");
  assert.equal(await tx(f, async (t2) => s.verifySourceCertificate(t2, digest, 5)), false);
  let captured: AuthorityTransaction | undefined;
  await tx(f, async (t2) => { captured = t2; });
  assert.throws(() => s.verifySourceCertificate(captured!, digest, cert), /active authority transaction/);
  await assert.rejects(s.certifySource(captured!, lease, certified), /active authority transaction/);
  await assert.rejects(s.certifySource({} as AuthorityTransaction, lease, certified), /active authority transaction/);
  // A lease from another store, or one the slot has moved past, cannot certify.
  const other = await store(f, { workerIncarnation: randomUUID() });
  await assert.rejects(tx(f, (t2) => other.certifySource(t2, lease, certified)), /does not belong/);
  await tx(f, (t2) => s.admitSourceUse(t2, src));
  await assert.rejects(tx(f, (t2) => s.certifySource(t2, lease, certified)), /stale/);
});

// ---- head shards: creation, deletion, restart rollback --------------------------------------

/**
 * Run authority setup in a separate node process, so the module-level high-water marks of this
 * process never see it: the parent then behaves like a restarted worker. The child holds W only
 * while it runs. Prints the slot key.
 */
function setupInChild(root: string, src: string, backupDir: string, mode: "admit" | "settle_failed"): string {
  const code = `(async () => {
    const [lockUrl, authUrl, supportUrl, root, token, src, backupDir, mode] = process.argv.slice(1);
    const fs = require("node:fs");
    const path = require("node:path");
    const lock = await import(lockUrl);
    const auth = await import(authUrl);
    const support = await import(supportUrl);
    const w = await lock.acquireWorkerLifetime(root, support.testLockOptions());
    const run = (fn) => lock.withAuthorityTransaction(w, { timeoutMs: 5000 }, fn);
    const store = await run((t) => auth.openAuthorityStore(t, { workerToken: token }));
    const key = await run((t) => store.slotKey(t, src));
    const lease = await run((t) => store.admitSourceUse(t, src));
    fs.copyFileSync(path.join(root, ".uzi-authority", "slots", key + ".json"), path.join(backupDir, "slot.json"));
    if (mode === "settle_failed") {
      const op = await run((t) => store.beginPending(t, lease, { kind: "destructive", footprint: "rm -r build" }));
      await run((t) => store.settlePending(t, lease, op, { ok: false, residue: ["a"] }));
    }
    process.stdout.write(key + "\\n");
  })().catch((error) => { process.stderr.write(String(error && error.stack || error) + "\\n"); process.exit(1); });`;
  const result = spawnSync(process.execPath, [
    "--import", "tsx", "-e", code,
    new URL("../src/authority-lock.ts", import.meta.url).href, new URL("../src/completion-authority.ts", import.meta.url).href,
    new URL("./authority-test-support.ts", import.meta.url).href, root, TOKEN, src, backupDir, mode,
  ], { cwd: agentRoot, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  return result.stdout.trim();
}

async function restarted(root: string): Promise<Fixture> {
  return { root, lifetime: await acquireWorkerLifetime(root, testLockOptions()), token: TOKEN };
}

test("a deleted head shard cannot roll a key back across a restart (restore of the crash-window slot)", async (t) => {
  const root = makeTmp(t);
  const backup = makeTmp(t, "authority-backup-");
  const src = path.join(root, "runner", "repo-a");
  fs.mkdirSync(src, { recursive: true });
  const key = setupInChild(root, src, backup, "settle_failed");
  const f = await restarted(root);
  fs.rmSync(shardFile(f, key));
  fs.copyFileSync(path.join(backup, "slot.json"), path.join(slotsDir(f), `${key}.json`));
  fs.chmodSync(path.join(slotsDir(f), `${key}.json`), 0o600);
  await assert.rejects(async () => {
    const s = await store(f);
    return tx(f, (t2) => s.readSlot(t2, key));
  }, /authority head missing/);
});

test("a deleted head shard and slot cannot make an admitted key read as never admitted across a restart", async (t) => {
  const root = makeTmp(t);
  const backup = makeTmp(t, "authority-backup-");
  const src = path.join(root, "runner", "repo-a");
  fs.mkdirSync(src, { recursive: true });
  const key = setupInChild(root, src, backup, "admit");
  const f = await restarted(root);
  fs.rmSync(shardFile(f, key));
  fs.rmSync(path.join(slotsDir(f), `${key}.json`));
  await assert.rejects(async () => {
    const s = await store(f);
    return tx(f, (t2) => s.admitSourceUse(t2, src));
  }, /authority head missing/);
});

test("a fresh store gets all 256 empty shards at once, and a failed creation leaves no heads or staging directory", async (t) => {
  const f = await fixture(t);
  const headsPath = path.join(authorityDir(f), "heads");
  let fail = true;
  const ops: AuthorityFsOps = {
    syncFile: (h) => h.sync(), syncDir: (h) => h.sync(),
    rename: async (a, b) => {
      if (fail && path.basename(b) === "heads") throw new Error("injected heads rename failure");
      fs.renameSync(a, b);
    },
  };
  await assert.rejects(store(f, { ops }), /injected heads rename failure/);
  assert.equal(fs.existsSync(headsPath), false);
  assert.deepEqual(fs.readdirSync(authorityDir(f)).filter((name) => name.includes("heads")), []);
  fail = false;
  await store(f, { ops });
  const names = fs.readdirSync(headsPath).sort();
  assert.equal(names.length, 256);
  assert.deepEqual(names, Array.from({ length: 256 }, (_, i) => `${i.toString(16).padStart(2, "0")}.json`));
  assert.equal(fs.statSync(headsPath).mode & 0o7777, 0o700);
  assert.ok(names.every((name) => (fs.statSync(path.join(headsPath, name)).mode & 0o7777) === 0o600));
});

test("an existing heads directory with any shard missing refuses at open", async (t) => {
  const f = await fixture(t);
  await store(f);
  fs.rmSync(path.join(authorityDir(f), "heads", "7f.json"));
  await assert.rejects(store(f), /authority head missing/);
});

test("heads swapped for a symlink refuses with an authority refusal, not a raw error", async (t) => {
  const f = await fixture(t);
  const s = await store(f);
  const src = source(f);
  const key = await tx(f, (t2) => s.slotKey(t2, src));
  await tx(f, (t2) => s.admitSourceUse(t2, src));
  const headsPath = path.join(authorityDir(f), "heads");
  fs.renameSync(headsPath, `${headsPath}.real`);
  fs.symlinkSync(`${headsPath}.real`, headsPath);
  const refusal = await tx(f, (t2) => s.readSlot(t2, key)).then(() => undefined, (error: unknown) => error);
  assert.ok(refusal instanceof AuthorityRefusal, String(refusal));
  assert.match((refusal as Error).message, /unsafe authority directory/);
  const reopened = await store(f).then(() => undefined, (error: unknown) => error);
  assert.ok(reopened instanceof AuthorityRefusal, String(reopened));
});

test("a MAC-valid head shard holding a key of another shard is refused", async (t) => {
  const f = await fixture(t);
  await store(f);
  const enrolled = JSON.parse(fs.readFileSync(enrollmentFile(f), "utf8")) as { storageId: string };
  // Forge with the join token's derived key: the only way to reach the prefix check past the MAC.
  const macKey = createHmac("sha256", TOKEN).update("uzi completion authority v1").digest();
  const shard = "ab";
  const body = {
    version: "uzi-authority-head/2", storageId: enrolled.storageId, shard, slots: { ["cd" + "1".repeat(62)]: 1 },
  };
  const mac = createHmac("sha256", macKey).update(`uzi-authority-record\0head\0${JSON.stringify(body)}`).digest("hex");
  fs.writeFileSync(path.join(authorityDir(f), "heads", `${shard}.json`), JSON.stringify({ ...body, mac }) + "\n", { mode: 0o600 });
  const s = await store(f);
  await assert.rejects(tx(f, (t2) => s.readSlot(t2, shard + "2".repeat(62))), /malformed authority record/);
});
