import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import type { Stats } from "node:fs";
import { PassThrough, Readable } from "node:stream";
import { GitCache } from "../src/git.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import { nullLogger } from "./helpers.js";

const cap = 256 * 1024 ** 2;
const tick = () => new Promise<void>(resolve => setImmediate(resolve));
const pack = (version = 2, count = 0, size = 32) => {
  const bytes = Buffer.alloc(size);
  bytes.write("PACK");
  bytes.writeUInt32BE(version, 4);
  bytes.writeUInt32BE(count, 8);
  return bytes;
};
async function fixture(t: test.TestContext, profile: number | {
  uid?: number; gid?: number; split?: boolean; dataGid?: number; dataMode?: number;
} = {}) {
  const options = typeof profile === "number" ? {} : profile;
  const actualUid = process.getuid!();
  const workerUid = options.uid ?? (actualUid === 0 ? 10001 : actualUid);
  if (actualUid !== workerUid)
    t.mock.method(process as NodeJS.Process & { getuid: () => number }, "getuid", () => workerUid);
  if (options.gid !== undefined) t.mock.method(process as NodeJS.Process & { getgid: () => number }, "getgid", () => options.gid!);
  if (options.split !== undefined) {
    const saved = process.env.UZI_UID_SPLIT;
    if (options.split) process.env.UZI_UID_SPLIT = "1";
    else delete process.env.UZI_UID_SPLIT;
    t.after(() => {
      if (saved === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = saved;
    });
  }
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/pack-stage-"));
  // The worker checkout has foreign/group-writable ancestors. Model a trusted
  // deployment hierarchy for these unit tests; root runners also project fixture
  // ownership to a nonroot UID, preserving all other stats inside dir.
  // This is an explicit model, not end-to-end admission on a real worker.
  const ancestors = new Set<string>();
  for (let p = path.dirname(dir);; p = path.dirname(p)) {
    ancestors.add(p);
    if (path.dirname(p) === p) break;
  }
  const lstat = fs.lstat.bind(fs);
  t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
    const stat = await lstat(p);
    if (typeof p === "string" && ancestors.has(p))
      return Object.assign(stat, { uid: typeof profile === "number" ? profile : workerUid,
        gid: p === path.dirname(dir) ? options.dataGid ?? stat.gid : stat.gid,
        mode: p === path.dirname(dir) && options.dataMode !== undefined
          ? (stat.mode & ~0o7777) | options.dataMode : stat.mode & ~0o022 });
    if (actualUid !== workerUid && typeof p === "string" && (p === dir || p.startsWith(dir + path.sep)))
      stat.uid = workerUid;
    return stat;
  });
  const git = new GitCache(dir, nullLogger());
  await fs.mkdir(git.recoveryRoot, { mode: 0o700 });
  let now = Date.now();
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture",
    now: () => now });
  const controller = new AbortController();
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  }, { signal: controller.signal });
  assert.ok(admitted.ok);
  t.after(async () => {
    await ledger.finish(admitted.value, "transient");
    await fs.rm(dir, { recursive: true, force: true });
  });
  const out = path.join(git.recoveryRoot, "temp.pack");
  if (actualUid !== workerUid) {
    // Project ownership only for this fixture output, including its fd stats.
    const open = fs.open.bind(fs);
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args);
      if (args[0] !== out) return file;
      const stat = file.stat.bind(file);
      t.mock.method(file, "stat", async () => Object.assign(await stat(), { uid: workerUid }));
      return file;
    });
  }
  const run = <T>(fn: () => T) => MaintenanceResources.run(admitted.value, fn);
  const stage = (p: { stdout: Readable; exited: Promise<{ code: number; stderr: string }>; abort: () => void },
    limit = 32, before = async () => {}) => git["stageMaintenancePack"](p, out, limit, before);
  return { git, out, dir, run, stage, controller, rewind: () => { now--; } };
}
test("tiny pack stages with custody RPCs in the remaining original slot", async t => {
  const f = await fixture(t);
  f.run(() => MaintenanceResources.consume("rpcAttempts", 16));
  assert.deepEqual(await f.run(() => f.stage(producer(), 32, async () => {
    MaintenanceResources.consume("rpcAttempts", 2);
  })), { path: f.out, byteSize: 32, objectCount: 0 });
  assert.deepEqual(await fs.readFile(f.out), pack());
});
test("modeled split worker admits data ancestor 10001:10001 mode 2775 with private recovery output", async t => {
  const f = await fixture(t, { uid: 10001, gid: 10001, split: true, dataGid: 10001, dataMode: 0o2775 });
  // path.dirname(f.dir) models /data; this is not an actual root/PVC proof.
  assert.deepEqual(await f.run(() => f.stage(producer())),
    { path: f.out, byteSize: 32, objectCount: 0 });
  assert.equal((await fs.lstat(f.git.recoveryRoot)).mode & 0o077, 0);
  assert.equal((await fs.stat(f.out)).mode & 0o7777, 0o600);
});
for (const kind of ["world write", "foreign group", "runner group", "command group", "provider group",
  "runner identity", "command identity", "non-worker uid with worker gid", "worker gid mismatch", "missing split",
  "recovery root group write", "descendant group write", "changed parent at recheck",
  "foreign owner", "symlink"] as const) {
  test(`modeled split data ancestor refuses ${kind}`, async t => {
    const uid = kind === "runner identity" || kind === "non-worker uid with worker gid" ? 10002
      : kind === "command identity" ? 10003 : 10001;
    const dataGid = kind === "foreign group" ? 12345 : kind === "runner group" ? 10002
      : kind === "command group" ? 10003 : kind === "provider group" ? 10004
      : kind === "non-worker uid with worker gid" ? 10001 : uid;
    const f = await fixture(t, { uid, gid: kind === "worker gid mismatch" ? 10002
      : kind === "non-worker uid with worker gid" ? 10001 : uid,
      split: kind !== "missing split", dataGid, dataMode: kind === "world write" ? 0o2777 : 0o2775 });
    const data = path.dirname(f.dir), lstat = fs.lstat.bind(fs);
    let changed = false;
    const child = path.join(f.git.recoveryRoot, "child");
    if (kind === "descendant group write") await fs.mkdir(child, { mode: 0o700 });
    t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
      const stat = await lstat(p);
      if ((p === f.git.recoveryRoot && kind === "recovery root group write") ||
          (p === child && kind === "descendant group write")) stat.mode |= 0o020;
      if (p === data) {
        if (kind === "changed parent at recheck" && changed) stat.mode |= 0o002;
        if (kind === "foreign owner") stat.uid = 12345;
        if (kind === "symlink") stat.isSymbolicLink = () => true;
      }
      return stat;
    });
    const out = kind === "descendant group write" ? path.join(child, "temp.pack") : f.out;
    await assert.rejects(f.run(() => f.git["stageMaintenancePack"](producer(), out, 32,
      async () => { changed = true; })), { message: "maintenance_pack_staging_refused" });
    await assert.rejects(fs.lstat(out), { code: "ENOENT" });
  });
}
test("secure root-owned ancestors admit a worker-owned recovery root", async t => {
  const f = await fixture(t, 0);
  assert.deepEqual(await f.run(() => f.stage(producer())),
    { path: f.out, byteSize: 32, objectCount: 0 });
});
for (const kind of ["foreign ancestor", "root-owned recovery root", "group-writable ancestor",
  "world-writable ancestor"] as const) {
  test(`ownership admission refuses ${kind}`, async t => {
    const workerUid = process.getuid!() || 10001;
    const f = await fixture(t, kind === "foreign ancestor" ? workerUid + 1 : 0);
    const lstat = fs.lstat.bind(fs);
    t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
      const stat = await lstat(p);
      if (kind === "root-owned recovery root" && p === f.git.recoveryRoot) stat.uid = 0;
      if (p === path.parse(f.out).root && kind.includes("writable"))
        stat.mode |= kind === "group-writable ancestor" ? 0o020 : 0o002;
      return stat;
    });
    await assert.rejects(f.run(() => f.stage(producer())),
      { message: "maintenance_pack_staging_refused" });
    assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
  });
}
for (const message of ["maintenance_resource_exhausted", "maintenance_token_invalid",
  "maintenance_clock_invalid", "maintenance_resource_exhausted private evidence", "private evidence"]) {
  test(`caught callback error exposes only fixed diagnostic: ${message}`, async t => {
    const f = await fixture(t);
    const original = new Error(message, { cause: "private cause" });
    Object.assign(original, { stderr: "private stderr" });
    const expected = ["maintenance_resource_exhausted", "maintenance_token_invalid",
      "maintenance_clock_invalid"].includes(message) ? message : "maintenance_pack_staging_refused";
    await assert.rejects(f.run(() => f.stage(producer(), 32, async () => { throw original; })), error => {
      assert.ok(error instanceof Error);
      assert.notEqual(error, original);
      assert.equal(error.message, expected);
      assert.equal(error.cause, undefined);
      assert.deepEqual(Object.keys(error), []);
      assert.ok(!error.stack?.includes("private"));
      return true;
    });
  });
}
test("clock rollback preserves the fixed clock refusal", async t => {
  const f = await fixture(t);
  await assert.rejects(f.run(() => f.stage(producer(), 32, async () => { f.rewind(); })),
    { message: "maintenance_clock_invalid" });
  assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
});
test("failed copy keeps its admitted artifact charge without refund", async t => {
  const f = await fixture(t), open = fs.open.bind(fs);
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    if (args[0] !== f.out) return file;
    t.mock.method(file, "write", async () => { throw Error("private write evidence"); });
    return file;
  });
  await assert.rejects(f.run(() => f.stage(producer())), { message: "maintenance_pack_staging_refused" });
  assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
  f.run(() => MaintenanceResources.consume("artifactBytes", cap - 12));
  assert.throws(() => f.run(() => MaintenanceResources.consume("artifactBytes", 1)),
    { message: "maintenance_resource_exhausted" });
});
test("abort during close joins late completion and never returns a descriptor", async t => {
  const f = await fixture(t), open = fs.open.bind(fs);
  let release!: () => void, entered!: () => void, settled = false;
  const closing = new Promise<void>(r => { entered = r; });
  const blocked = new Promise<void>(r => { release = r; });
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    if (args[0] !== f.out) return file;
    const close = file.close.bind(file);
    t.mock.method(file, "close", async () => { entered(); await blocked; await close(); });
    return file;
  });
  const result = f.run(() => f.stage(producer())).then(value => {
    settled = true;
    return value;
  }, error => { settled = true; return error; });
  try {
    await Promise.race([closing, result.then(() => { throw Error("operation settled before seam"); })]);
    f.controller.abort();
    await tick();
    assert.equal(settled, false);
  } finally {
    release();
    await result;
  }
  const error = await result;
  assert.ok(error instanceof Error);
  assert.equal(error.message, "maintenance_token_invalid");
  assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
});
function producer(chunks: Buffer[] = [pack()]) {
  let aborted = 0;
  const stdout = Readable.from(chunks);
  return { stdout, exited: Promise.resolve({ code: 0, stderr: "" }), abort: () => { aborted++; },
    aborted: () => aborted };
}

for (const version of [2, 3]) {
  for (let split = 1; split < 12; split++) {
    test(`split prefix ${split}, version ${version}: empty pack, exact cap, no verification claim`, async t => {
      const f = await fixture(t), bytes = pack(version);
      const p = producer([bytes.subarray(0, split), bytes.subarray(split)]);
      assert.deepEqual(await f.run(() => f.stage(p)), { path: f.out, byteSize: 32, objectCount: 0 });
      assert.deepEqual(await fs.readFile(f.out), bytes);
      assert.equal((await fs.stat(f.out)).mode & 0o777, 0o600);
      f.run(() => MaintenanceResources.consume("artifactBytes", cap - 32));
      assert.throws(() => f.run(() => MaintenanceResources.consume("artifactBytes", 1)));
      assert.equal(p.aborted(), 0);
    });
  }
}
for (const kind of ["no scope", "discovery", "callback"] as const) {
  test(`${kind} refusal cancels and joins handed-off producer before return`, async t => {
    const f = await fixture(t);
    let reap!: (v: { code: number; stderr: string }) => void, cancelling!: () => void;
    const cancelled = new Promise<void>(r => { cancelling = r; });
    let aborted = 0, settled = false;
    const p = { stdout: new PassThrough(), exited: new Promise<{ code: number; stderr: string }>(r => { reap = r; }),
      abort: () => { aborted++; cancelling(); } };
    const call = () => f.stage(p, 32, async () => { if (kind === "callback") throw Error("private evidence"); });
    const result = (kind === "no scope" ? call() : kind === "discovery"
      ? MaintenanceResources.runDiscovery(call) : f.run(call)).catch(error => { settled = true; return error; });
    try {
      await Promise.race([cancelled, result.then(() => { throw Error("operation settled before cancellation"); })]);
      await tick();
      assert.ok(aborted > 0);
      assert.equal(settled, false);
      assert.equal(p.stdout.destroyed, true);
    } finally {
      p.stdout.destroy();
      reap({ code: -1, stderr: "private stderr" });
      await result;
    }
    assert.equal((await result).message, kind === "callback"
      ? "maintenance_pack_staging_refused" : "maintenance_token_invalid");
    assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
  });
}
for (const [name, bytes, limit] of [
  ["signature", Buffer.alloc(32), 32], ["high-bit signature", Buffer.concat([Buffer.from([0xd0]), pack().subarray(1)]), 32], ["version", pack(4), 32],
  ["short prefix", pack().subarray(0, 11), 32], ["short trailer", pack().subarray(0, 31), 32],
  ["cap plus one", pack(2, 0, 33), 32],
  ...[0, 31, -1, 32.5, NaN, Infinity, cap + 1, Number.MAX_SAFE_INTEGER].map(n => ["invalid cap " + n, pack(), n]),
] as Array<[string, Buffer, number]>) {
  test(name, async t => {
    const f = await fixture(t), p = producer([bytes]);
    await assert.rejects(f.run(() => f.stage(p, limit)), /maintenance_pack_staging_refused/);
    assert.ok(p.aborted() > 0);
    assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
  });
}
test("unsigned count quota is charged before any body or prefix write", async t => {
  const f = await fixture(t);
  let writes = 0;
  const open = fs.open.bind(fs);
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    if (args[0] !== f.out) return file;
    t.mock.method(file, "write", async () => { writes++; throw Error("must not write"); });
    return file;
  });
  await assert.rejects(f.run(() => f.stage(producer([pack(2, 0xffffffff)]))),
    { message: "maintenance_resource_exhausted" });
  assert.equal(writes, 0);
});
test("copy quota refuses before write", async t => {
  const f = await fixture(t);
  let writes = 0;
  const open = fs.open.bind(fs);
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    if (args[0] !== f.out) return file;
    t.mock.method(file, "write", async () => { writes++; throw Error(); });
    return file;
  });
  await assert.rejects(f.run(() => {
    MaintenanceResources.consume("artifactBytes", cap - 11);
    return f.stage(producer());
  }), { message: "maintenance_resource_exhausted" });
  assert.equal(writes, 0);
  assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
});
test("short writes charge once and bounded writes include header and trailer", async t => {
  const f = await fixture(t), bytes = pack(3, 1, 140000);
  const open = fs.open.bind(fs);
  let calls = 0;
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    if (args[0] !== f.out) return file;
    const write = file.write.bind(file);
    t.mock.method(file, "write", async (buffer: Buffer, offset: number, length: number, position: number) => {
      assert.ok(length <= 65536); calls++;
      return write(buffer, offset, Math.min(length, 7000), position);
    });
    return file;
  });
  assert.equal((await f.run(() => f.stage(producer([bytes]), bytes.length))).byteSize, bytes.length);
  assert.ok(calls > 20);
  assert.deepEqual(await fs.readFile(f.out), bytes);
  f.run(() => MaintenanceResources.consume("artifactBytes", cap - bytes.length));
  assert.throws(() => f.run(() => MaintenanceResources.consume("artifactBytes", 1)));
});
for (const rejection of [false, true]) {
  test(`defensive ${rejection ? "rejected" : "nonzero"} exit tears down stalled reader`, async t => {
    const f = await fixture(t);
    let settle!: () => void, aborted = 0;
    const stdout = new PassThrough();
    const exited = new Promise<{ code: number; stderr: string }>((resolve, reject) => {
      settle = () => rejection ? reject(Error("private")) : resolve({ code: 7, stderr: "private" });
    });
    const result = f.run(() => f.stage({ stdout, exited, abort: () => { aborted++; } }));
    stdout.write(pack());
    await tick();
    settle();
    await assert.rejects(result, /^Error: maintenance_pack_staging_refused$/);
    assert.ok(aborted > 0);
    assert.equal(stdout.destroyed, true);
    assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
  });
}
for (const fault of ["write", "zero", "sync", "read", "stat", "close"] as const) {
  test(`filesystem ${fault} failure cleans only owned file`, async t => {
    const f = await fixture(t), open = fs.open.bind(fs);
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args);
      if (args[0] !== f.out) return file;
      if (fault === "zero") t.mock.method(file, "write", async () => ({ bytesWritten: 0, buffer: Buffer.alloc(0) }));
      else {
        const method = fault === "stat" ? "stat" : fault;
        if (method === "close") {
          const close = file.close.bind(file);
          t.mock.method(file, "close", async () => { await close(); throw Error("private"); });
        } else t.mock.method(file, method, () => { throw Error("private"); });
      }
      return file;
    });
    await assert.rejects(f.run(() => f.stage(producer())));
    // A permanently failing fstat cannot prove inode ownership; preserving the
    // ambiguous created path is deliberate. Other injected failures allow cleanup.
    if (fault !== "stat") assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
  });
}
for (const kind of ["file", "symlink", "outside", "unsafe parent", "symlink parent", "replacement", "parent substitution"] as const) {
  test(`preserve foreign paths: ${kind}`, async t => {
    const f = await fixture(t), foreign = path.join(f.dir, "foreign");
    await fs.writeFile(foreign, "foreign");
    if (kind === "file") await fs.writeFile(f.out, "foreign");
    if (kind === "symlink") await fs.symlink(foreign, f.out);
    if (kind === "unsafe parent") await fs.chmod(f.git.recoveryRoot, 0o777);
    if (kind === "symlink parent") {
      await fs.rmdir(f.git.recoveryRoot);
      await fs.symlink(f.dir, f.git.recoveryRoot);
    }
    let replaced = false;
    const before = async () => {
      if (replaced || !["replacement", "parent substitution"].includes(kind)) return;
      try { await fs.access(f.out); } catch { return; }
      replaced = true;
      if (kind === "replacement") { await fs.unlink(f.out); await fs.writeFile(f.out, "foreign"); }
      else {
        await fs.rename(f.git.recoveryRoot, f.git.recoveryRoot + "-old");
        await fs.mkdir(f.git.recoveryRoot);
        await fs.writeFile(f.out, "foreign");
      }
      throw Error("refuse");
    };
    await assert.rejects(f.run(() => kind === "outside"
      ? f.git["stageMaintenancePack"](producer(), foreign, 32, async () => {})
      : f.stage(producer(), 32, before)));
    assert.equal(await fs.readFile(foreign, "utf8"), "foreign");
    if (["file", "symlink", "replacement", "parent substitution"].includes(kind))
      assert.equal(await fs.readFile(f.out, "utf8"), "foreign");
  });
}
test("scope cancellation tears stalled pipe down but waits for delayed actual reap", async t => {
  const f = await fixture(t), stdout = new PassThrough();
  let reap!: (v: { code: number; stderr: string }) => void;
  let aborted = 0, settled = false;
  const exited = new Promise<{ code: number; stderr: string }>(r => { reap = r; });
  const result = f.run(() => f.stage({ stdout, exited, abort: () => { aborted++; } }))
    .catch(error => { settled = true; return error; });
  try {
    stdout.write(pack());
    await tick();
    f.controller.abort();
    await tick();
    assert.ok(aborted > 0);
    assert.equal(stdout.destroyed, true);
    assert.equal(settled, false);
  } finally {
    stdout.destroy();
    reap({ code: -1, stderr: "private" });
    await result;
  }
  assert.equal((await result).message, "maintenance_token_invalid");
  assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
});
for (const parent of [false, true]) {
  test(`replacement during readback preserves foreign ${parent ? "parent" : "leaf"}`, async t => {
    const f = await fixture(t), open = fs.open.bind(fs);
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args);
      if (args[0] !== f.out) return file;
      const read = file.read.bind(file);
      t.mock.method(file, "read", async (buffer: Buffer, offset: number, length: number, position: number) => {
        const result = await read(buffer, offset, length, position);
        if (parent) {
          await fs.rename(f.git.recoveryRoot, f.git.recoveryRoot + "-old");
          await fs.mkdir(f.git.recoveryRoot);
        } else await fs.unlink(f.out);
        await fs.writeFile(f.out, "foreign");
        return result;
      });
      return file;
    });
    await assert.rejects(f.run(() => f.stage(producer())));
    assert.equal(await fs.readFile(f.out, "utf8"), "foreign");
  });
}
test("used or competing stdout and missing callback are refused before creation", async t => {
  const f = await fixture(t);
  const consumed = producer();
  consumed.stdout.read(1);
  await assert.rejects(f.run(() => f.stage(consumed)));
  const competing = producer();
  competing.stdout.on("data", () => {});
  await assert.rejects(f.run(() => f.stage(competing)));
  await assert.rejects(f.run(() => f.git["stageMaintenancePack"](producer(), f.out, 32,
    undefined as unknown as () => Promise<void>)));
  assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
});
test("cancellation joins in-flight filesystem write before cleanup and return", async t => {
  const f = await fixture(t), open = fs.open.bind(fs), lstat = fs.lstat.bind(fs), unlink = fs.unlink.bind(fs);
  let release!: () => void, entered!: () => void, settled = false, pending = false;
  let closeAttempts = 0, unlinkAttempts = 0;
  let actualWrite: Promise<unknown> | undefined, realClose: (() => Promise<void>) | undefined;
  const cached = new Map<string, Stats>();
  const parents = new Set<string>();
  for (let p = path.dirname(f.out);; p = path.dirname(p)) {
    parents.add(p);
    if (path.dirname(p) === p) break;
  }
  const writing = new Promise<void>(r => { entered = r; });
  const blocked = new Promise<void>(r => { release = r; });
  // Only staging paths use cached metadata during the probe. With no OS I/O in
  // cleanup, one event-loop turn drains an abandoned write's cleanup microtasks.
  t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
    if (typeof p !== "string" || (!parents.has(p) && p !== f.out)) return lstat(p);
    if (pending) {
      assert.ok(cached.has(p));
      return cached.get(p)!;
    }
    const stat = await lstat(p);
    cached.set(p, stat);
    return stat;
  });
  t.mock.method(fs, "unlink", async (p: Parameters<typeof fs.unlink>[0]) => {
    if (p === f.out && pending) { unlinkAttempts++; return; }
    return unlink(p);
  });
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    if (args[0] !== f.out) return file;
    cached.set(f.out, await file.stat());
    const write = file.write.bind(file);
    realClose = file.close.bind(file);
    t.mock.method(file, "close", async () => {
      if (pending) { closeAttempts++; return; }
      await realClose!();
    });
    t.mock.method(file, "write", (buffer: Buffer, offset: number, length: number, position: number) => {
      pending = true;
      const operation = blocked.then(() => write(buffer, offset, length, position)).finally(() => { pending = false; });
      actualWrite = operation;
      entered();
      return operation;
    });
    return file;
  });
  const result = f.run(() => f.stage(producer())).then(value => {
    settled = true; return value;
  }, error => { settled = true; return error; });
  try {
    await Promise.race([writing, result.then(() => { throw Error("operation settled before seam"); })]);
    f.controller.abort();
    await tick();
    assert.equal(closeAttempts, 0, "close attempted before admitted write completed");
    assert.equal(unlinkAttempts, 0, "unlink attempted before admitted write completed");
    assert.equal(settled, false, "staging returned before admitted write completed");
    assert.equal((await fs.stat(f.out)).size, 0);
  } finally {
    release();
    try { await actualWrite; } finally {
      await result;
      await realClose?.();
      if (unlinkAttempts) await unlink(f.out);
    }
  }
  assert.equal((await result).message, "maintenance_token_invalid");
  assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
});
for (const fault of ["size", "mode", "uid", "nlink", "identity", "prefix"] as const) {
  test(`readback rejects ${fault} mismatch`, async t => {
    const f = await fixture(t), open = fs.open.bind(fs);
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args);
      if (args[0] !== f.out) return file;
      const stat = file.stat.bind(file), read = file.read.bind(file);
      let stats = 0;
      t.mock.method(file, "stat", async () => {
        const result = await stat();
        if (++stats === 1) return result;
        if (fault === "size") result.size++;
        if (fault === "mode") result.mode |= 0o004;
        if (fault === "uid") result.uid++;
        if (fault === "nlink") result.nlink++;
        if (fault === "identity") result.ino++;
        return result;
      });
      if (fault === "prefix") t.mock.method(file, "read", async (buffer: Buffer, offset: number, length: number, position: number) => {
        const result = await read(buffer, offset, length, position);
        buffer[0] = 0;
        return result;
      });
      return file;
    });
    await assert.rejects(f.run(() => f.stage(producer())));
    assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
  });
}
test("same original slot: actual runner stream meters producer plus staging copy without launching an importer", async t => {
  const f = await fixture(t), raw = new PassThrough(), stderr = new PassThrough(), stdin = new PassThrough();
  let complete!: (v: { code: number }) => void;
  const completed = new Promise<{ code: number }>(r => { complete = r; });
  const requests: string[][] = [];
  const operation = f.run(() => f.git.withBoundaryProcessSpawner(async request => {
    requests.push([...request.argv]);
    return { stdin, stdout: raw, stderr, completed, cancel: async () => {} };
  }, new AbortController().signal, async () => {
    const p = await f.git["spawnGitAsRunnerWithStdin"]("/unused", ["pack-objects", "--stdout"], { streamStdout: true });
    assert.ok(p.stdout);
    p.stdin.end();
    const staged = f.stage({ ...p, stdout: p.stdout });
    raw.end(pack()); stderr.end(); complete({ code: 0 });
    return staged;
  }));
  assert.equal((await operation).byteSize, 32);
  f.run(() => MaintenanceResources.consume("artifactBytes", cap - 64));
  assert.throws(() => f.run(() => MaintenanceResources.consume("artifactBytes", 1)));
  assert.equal(requests.length, 1);
  assert.ok(requests[0]!.includes("pack-objects"));
  assert.ok(!requests.flat().includes("index-pack"));
});
