import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHash } from "node:crypto";
import { deflateSync } from "node:zlib";
import { spawn } from "node:child_process";
import { PassThrough, Transform } from "node:stream";
import { GitCache } from "../src/git.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { nullLogger } from "./helpers.js";

const cap = 256 * 1024 ** 2;
const tick = () => new Promise<void>(resolve => setImmediate(resolve));
const digest = (b: Buffer) => createHash("sha1").update(b).digest();
function pack(count = 1) {
  const bodies = Array.from({ length: count }, (_, i) => Buffer.from(`bounded index fixture ${i}\n`));
  const oids = bodies.map(body =>
    digest(Buffer.concat([Buffer.from(`blob ${body.length}\0`), body])).toString("hex"));
  const header = Buffer.alloc(12);
  header.write("PACK"); header.writeUInt32BE(2, 4); header.writeUInt32BE(count, 8);
  const bytes = Buffer.concat([header, ...bodies.map(body =>
    Buffer.concat([Buffer.from([0xb0 | (body.length & 15), body.length >> 4]), deflateSync(body)]))]);
  return { bytes: Buffer.concat([bytes, digest(bytes)]), oid: oids[0]!, oids };
}
async function fixture(t: test.TestContext, count = 1) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/pack-index-"));
  const git = new GitCache(dir, nullLogger());
  await fs.mkdir(git.recoveryRoot, { mode: 0o700 });
  // Model deployment ownership: checkout ancestors are not a deployed worker hierarchy.
  // Preserve real permissions for recoveryRoot and descendants, including descriptor stats.
  const ancestors = new Set<string>();
  for (let p = dir;; p = path.dirname(p)) {
    ancestors.add(p);
    if (path.dirname(p) === p) break;
  }
  const lstat = fs.lstat.bind(fs);
  t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
    const stat = await lstat(p);
    if (typeof p === "string" && ancestors.has(p))
      Object.assign(stat, { uid: process.getuid!(), mode: stat.mode & ~0o022 });
    return stat;
  });
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture" });
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
  const input = path.join(git.recoveryRoot, "stage.pack"), output = path.join(git.recoveryRoot, "stage.idx");
  const { bytes, oid, oids } = pack(count);
  await fs.writeFile(input, bytes, { mode: 0o600 });
  const staged = { path: input, byteSize: bytes.length, objectCount: count };
  const requests: BoundaryProcessRequest[] = [];
  let joined = 0;
  const owner: { afterClose?: () => Promise<void>; beforeCancel?: () => Promise<void>;
    holdStderr?: boolean; stderr?: PassThrough; status?: (bytes: Buffer) => Buffer } = {};
  const ownedSpawn = async (request: BoundaryProcessRequest) => {
    requests.push(request);
    const child = spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env });
    const completed = new Promise<{ code: number }>(resolve => {
      child.once("error", () => {});
      child.once("close", code => {
        joined++;
        void (async () => { await owner.afterClose?.(); resolve({ code: code ?? -1 }); })();
      });
    });
    const stderr = owner.holdStderr ? (owner.stderr = new PassThrough()) : child.stderr;
    if (owner.stderr) child.stderr.pipe(owner.stderr, { end: false });
    const stdout = owner.status ? child.stdout.pipe(new Transform({
      transform(bytes: Buffer, _encoding, callback) { callback(null, owner.status!(bytes)); },
    })) : child.stdout;
    return { stdin: child.stdin, stdout, stderr, completed,
      cancel: async () => { child.kill("SIGKILL"); await owner.beforeCancel?.(); await completed; } };
  };
  MaintenanceResources.run(admitted.value, () => MaintenanceResources.consume("closureEntries", count));
  const run = <T>(fn: () => Promise<T>) => MaintenanceResources.run(admitted.value, () =>
    git.withBoundaryProcessSpawner(ownedSpawn, controller.signal, fn));
  const index = (expected = oids, before = async () => {}) =>
    git["indexMaintenancePack"](staged, output, expected, before);
  return { dir, git, input, output, bytes, oid, staged, controller, run, index, requests, owner,
    joined: () => joined, charge: (resource: "artifactBytes" | "closureEntries", amount: number) =>
      MaintenanceResources.run(admitted.value, () => MaintenanceResources.consume(resource, amount)) };
}
test("real trusted Git FILEINPUT indexes exact OIDs with isolated environment and joined exit", async t => {
  const f = await fixture(t);
  const saved = process.env.GIT_CONFIG_GLOBAL;
  process.env.GIT_CONFIG_GLOBAL = "/hostile/config";
  try {
    assert.deepEqual(await f.run(() => f.index()), { path: f.output, byteSize: 1100, objectCount: 1 });
  } finally {
    if (saved === undefined) delete process.env.GIT_CONFIG_GLOBAL;
    else process.env.GIT_CONFIG_GLOBAL = saved;
  }
  assert.equal(f.joined(), 1);
  assert.deepEqual(await fs.readFile(f.input), f.bytes);
  assert.equal(f.requests.length, 1);
  const request = f.requests[0]!;
  assert.equal(request.argv[0], "/usr/bin/git");
  assert.deepEqual(request.argv.slice(-7), ["index-pack", "--strict", "--no-rev-index", "--index-version=2", "-o", f.output, f.input].slice(-8));
  assert.equal(request.env.GIT_CONFIG_GLOBAL, "/dev/null");
  assert.equal(request.env.GIT_ALLOW_PROTOCOL, "none");
  assert.equal(request.env.GIT_NO_LAZY_FETCH, "1");
  assert.equal(request.env.GIT_DIR, "/dev/null");
  assert.equal(request.env.LD_PRELOAD, undefined);
  f.charge("artifactBytes", cap - 1108);
  assert.throws(() => f.charge("artifactBytes", 1), { message: "maintenance_resource_exhausted" });
});
test("wrong expected OID refuses and removes only owned index", async t => {
  const f = await fixture(t);
  await assert.rejects(f.run(() => f.index(["0".repeat(40)])), { message: "maintenance_pack_index_refused" });
  assert.equal(f.joined(), 1);
  await assert.rejects(fs.lstat(f.output), { code: "ENOENT" });
  assert.deepEqual(await fs.readFile(f.input), f.bytes);
  f.charge("artifactBytes", cap - 1108);
  assert.throws(() => f.charge("artifactBytes", 1), { message: "maintenance_resource_exhausted" });
});
test("full envelope is precharged; quota refusal launches nothing", async t => {
  const f = await fixture(t);
  f.charge("artifactBytes", cap - 1107);
  await assert.rejects(f.run(() => f.index()), { message: "maintenance_resource_exhausted" });
  assert.equal(f.requests.length, 0);
  await assert.rejects(fs.lstat(f.output), { code: "ENOENT" });
});
test("empty and multiple-object fileinput packs validate exact sets", async t => {
  for (const count of [0, 2]) {
    await t.test(String(count), async t => {
      const f = await fixture(t, count);
      assert.equal((await f.run(() => f.index())).byteSize, 1072 + 28 * count);
    });
  }
});
test("all repeated OID visits share the stage's closureEntries allowance", async t => {
  const f = await fixture(t);
  f.charge("closureEntries", 65536 - 3);
  await assert.rejects(f.run(() => f.index()), { message: "maintenance_resource_exhausted" });
  assert.equal(f.joined(), 1);
});
for (const kind of ["signature", "version", "count", "fanout", "oid", "offset", "large offset",
  "crc", "pack checksum", "index checksum", "truncated", "tail"] as const) {
  test(`index rejects ${kind} even with recomputed index checksum`, async t => {
    const f = await fixture(t), lstat = fs.lstat.bind(fs), open = fs.open.bind(fs);
    let changed = false, readReached = false;
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args);
      if (args[0] === f.output) {
        const read = file.read.bind(file);
        t.mock.method(file, "read", async (buffer: Buffer, offset: number, length: number, position: number) => {
          readReached = true;
          return read(buffer, offset, length, position);
        });
      }
      return file;
    });
    t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
      if (p === f.output && f.joined() && !changed) {
        changed = true;
        let bytes = await fs.readFile(f.output);
        if (kind === "signature") bytes[0] = 0;
        if (kind === "version") bytes.writeUInt32BE(3, 4);
        if (kind === "count") bytes.writeUInt32BE(2, 1028);
        if (kind === "fanout") bytes.writeUInt32BE(2, 8);
        if (kind === "oid") bytes[1032] = bytes[1032]! ^ 1;
        if (kind === "offset") bytes.writeUInt32BE(11, 1056);
        if (kind === "large offset") bytes.writeUInt32BE(0x80000000, 1056);
        if (kind === "crc") bytes[1052] = bytes[1052]! ^ 1;
        if (kind === "pack checksum") bytes[1060] = bytes[1060]! ^ 1;
        if (kind === "index checksum") bytes[1099] = bytes[1099]! ^ 1;
        else if (kind === "truncated") bytes = bytes.subarray(0, bytes.length - 1);
        else if (kind === "tail") bytes = Buffer.concat([bytes, Buffer.alloc(8)]);
        if (kind !== "index checksum" && kind !== "truncated")
          digest(bytes.subarray(0, -20)).copy(bytes, bytes.length - 20);
        await fs.chmod(f.output, 0o600);
        await fs.writeFile(f.output, bytes);
      }
      return lstat(p);
    });
    await assert.rejects(f.run(() => f.index()), { message: "maintenance_pack_index_refused" });
    assert.equal(changed, true);
    assert.equal(readReached, true, "must reach index parser, not fail identity admission");
    assert.equal(f.joined(), 1);
    await assert.rejects(fs.lstat(f.output), { code: "ENOENT" });
  });
}
for (const kind of ["duplicate oid", "unsorted oid", "duplicate offset"] as const) {
  test(`index rejects ${kind} with a valid checksum`, async t => {
    const f = await fixture(t, 2), lstat = fs.lstat.bind(fs);
    let changed = false;
    t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
      if (p === f.output && f.joined() && !changed) {
        changed = true;
        const bytes = await fs.readFile(f.output);
        if (kind === "duplicate oid") bytes.copy(bytes, 1052, 1032, 1052);
        if (kind === "unsorted oid") {
          const first = Buffer.from(bytes.subarray(1032, 1052));
          bytes.copy(bytes, 1032, 1052, 1072); first.copy(bytes, 1052);
        }
        if (kind === "duplicate offset") bytes.copy(bytes, 1084, 1080, 1084);
        digest(bytes.subarray(0, -20)).copy(bytes, bytes.length - 20);
        await fs.chmod(f.output, 0o600);
        await fs.writeFile(f.output, bytes);
      }
      return lstat(p);
    });
    await assert.rejects(f.run(() => f.index()), { message: "maintenance_pack_index_refused" });
  });
}
for (const kind of ["pack checksum", "pack count", "pack version", "pack trailing junk", "strict invalid object"] as const) {
  test(`completed input refuses ${kind}`, async t => {
    const f = await fixture(t);
    let bytes = Buffer.from(f.bytes);
    if (kind === "pack checksum") bytes[bytes.length - 1] = bytes[bytes.length - 1]! ^ 1;
    if (kind === "pack count") bytes.writeUInt32BE(2, 8);
    if (kind === "pack version") bytes.writeUInt32BE(4, 4);
    if (kind === "pack trailing junk") {
      bytes = Buffer.concat([bytes.subarray(0, -20), Buffer.from("junk"), Buffer.alloc(20)]);
      f.staged.byteSize = bytes.length;
    }
    if (kind === "strict invalid object") bytes[14] = 0;
    if (kind !== "pack checksum") digest(bytes.subarray(0, -20)).copy(bytes, bytes.length - 20);
    await fs.writeFile(f.input, bytes);
    await assert.rejects(f.run(() => f.index()), { message: "maintenance_pack_index_refused" });
    if (kind === "strict invalid object" || kind === "pack trailing junk") assert.equal(f.joined(), 1);
    else assert.equal(f.requests.length, 0);
    assert.deepEqual(await fs.readFile(f.input), bytes);
  });
}
for (const kind of ["existing output", "output symlink", "input symlink", "input hardlink", "input mode",
  "foreign input owner", "recovery group write", "descendant group write", "world ancestor"] as const) {
  test(`admission protects ${kind}`, async t => {
    const f = await fixture(t), lstat = fs.lstat.bind(fs);
    if (kind === "existing output") await fs.writeFile(f.output, "foreign");
    if (kind === "output symlink") await fs.symlink(f.input, f.output);
    if (kind === "input symlink") {
      await fs.rename(f.input, f.input + ".original"); await fs.symlink(f.input + ".original", f.input);
    }
    if (kind === "input hardlink") await fs.link(f.input, f.input + ".link");
    if (kind === "input mode") await fs.chmod(f.input, 0o644);
    if (kind === "foreign input owner") {
      const open = fs.open.bind(fs);
      t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
        const file = await open(...args);
        if (args[0] === f.input) {
          const stat = file.stat.bind(file);
          t.mock.method(file, "stat", async () => Object.assign(await stat(), { uid: process.getuid!() + 1 }));
        }
        return file;
      });
    }
    if (kind === "descendant group write") {
      const child = path.join(f.git.recoveryRoot, "child");
      await fs.mkdir(child, { mode: 0o700 });
      f.staged.path = path.join(child, "stage.pack");
      await fs.rename(f.input, f.staged.path);
      await fs.chmod(child, 0o770);
      await assert.rejects(f.run(() => f.git["indexMaintenancePack"](f.staged,
        path.join(child, "stage.idx"), [f.oid], async () => {})));
      assert.equal(f.requests.length, 0);
      return;
    }
    t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
      const stat = await lstat(p);
      if (kind === "recovery group write" && p === f.git.recoveryRoot) stat.mode |= 0o020;
      if (kind === "world ancestor" && p === f.dir) stat.mode |= 0o002;
      return stat;
    });
    await assert.rejects(f.run(() => f.index()), { message: "maintenance_pack_index_refused" });
    assert.equal(f.requests.length, 0);
    if (kind === "existing output") assert.equal(await fs.readFile(f.output, "utf8"), "foreign");
    if (kind === "output symlink") assert.equal(await fs.readlink(f.output), f.input);
  });
}
test("fresh output is rechecked after custody callback at launch", async t => {
  const f = await fixture(t);
  await assert.rejects(f.run(() => f.index(undefined, async () => {
    await fs.writeFile(f.output, "foreign");
  })), { message: "maintenance_pack_index_refused" });
  assert.equal(f.requests.length, 0);
  assert.equal(await fs.readFile(f.output, "utf8"), "foreign");
});
test("foreign output substituted during final custody check is preserved", async t => {
  const f = await fixture(t);
  let callbacks = 0;
  await assert.rejects(f.run(() => f.index(undefined, async () => {
    if (++callbacks === 2) {
      await fs.rename(f.output, f.output + ".owned");
      await fs.writeFile(f.output, "foreign");
    }
  })), { message: "maintenance_pack_index_refused" });
  assert.equal(f.joined(), 1);
  assert.equal(await fs.readFile(f.output, "utf8"), "foreign");
});
test("abort during admitted filesystem read joins it before closing or returning", async t => {
  const f = await fixture(t), open = fs.open.bind(fs);
  let release!: () => void, entered!: () => void;
  const blocked = new Promise<void>(r => { release = r; }), reading = new Promise<void>(r => { entered = r; });
  let settled = false, closed = false;
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    if (args[0] !== f.output) return file;
    const read = file.read.bind(file), close = file.close.bind(file);
    t.mock.method(file, "read", async (buffer: Buffer, offset: number, length: number, position: number) => {
      entered(); await blocked;
      return read(buffer, offset, length, position);
    });
    t.mock.method(file, "close", async () => { closed = true; return close(); });
    return file;
  });
  const result = f.run(() => f.index()).then(v => { settled = true; return v; }, e => { settled = true; return e; });
  try {
    await Promise.race([reading, result.then(() => { throw Error("settled before read seam"); })]);
    f.controller.abort();
    await tick();
    assert.equal(settled, false);
    assert.equal(closed, false);
  } finally { release(); await result; }
  assert.equal((await result).message, "maintenance_token_invalid");
  assert.equal(closed, true);
  assert.equal(f.joined(), 1);
  await assert.rejects(fs.lstat(f.output), { code: "ENOENT" });
});
test("abort joins process ownership and cancellation before returning", async t => {
  const f = await fixture(t);
  let release!: () => void, entered!: () => void, cancelEntered!: () => void;
  const blocked = new Promise<void>(r => { release = r; });
  const closing = new Promise<void>(r => { entered = r; });
  const cancelling = new Promise<void>(r => { cancelEntered = r; });
  f.owner.afterClose = async () => { entered(); await blocked; };
  f.owner.beforeCancel = async () => { cancelEntered(); await blocked; };
  let settled = false;
  const result = f.run(() => f.index()).then(v => { settled = true; return v; }, e => { settled = true; return e; });
  try {
    await Promise.race([closing, result.then(() => { throw Error("settled before owner seam"); })]);
    f.controller.abort();
    await Promise.race([cancelling, result.then(() => { throw Error("settled before cancel join"); })]);
    await tick();
    assert.equal(settled, false);
    assert.equal(f.joined(), 1); // Real child closed; owner settlement remains blocked.
  } finally { release(); await result; }
  assert.equal((await result).message, "maintenance_token_invalid");
  assert.deepEqual(await fs.readFile(f.input), f.bytes);
});
test("successful owned exit still waits for stderr EOF", async t => {
  const f = await fixture(t);
  let closed!: () => void;
  const closing = new Promise<void>(r => { closed = r; });
  f.owner.holdStderr = true;
  let ownedExit = false, validationBeforeEof = 0, validationAfterEof = 0;
  const lstat = fs.lstat.bind(fs);
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    // Observe the FIRST validation dispatch, before native filesystem latency.
    // A disabled join must not hide behind later asynchronous readback checks.
    if (ownedExit) {
      if (f.owner.stderr?.readableEnded) validationAfterEof++;
      else validationBeforeEof++;
    }
    return lstat(...args);
  });
  f.owner.afterClose = async () => { ownedExit = true; closed(); };
  let settled = false;
  const result = f.run(() => f.index()).then(v => { settled = true; return v; }, e => { settled = true; return e; });
  try {
    await Promise.race([closing, result.then(() => { throw Error("settled before child exit"); })]);
    await tick();
    assert.equal(validationBeforeEof, 0, "no validation dispatch before owned stderr EOF");
    assert.equal(settled, false);
  } finally { f.owner.stderr?.end(); await result; }
  assert.ok(validationAfterEof > 0, "the observation seam must see real validation");
  assert.equal((await result).path, f.output);
});
for (const kind of ["wrong", "high bit", "extra", "short"] as const) {
  test(`bounded exact status rejects ${kind} with successful Git exit`, async t => {
    const f = await fixture(t);
    f.owner.status = bytes => {
      const changed = Buffer.from(bytes);
      if (kind === "wrong") changed[0] = 0x7a;
      if (kind === "high bit") changed[0] = changed[0]! | 0x80;
      return kind === "extra" ? Buffer.concat([changed, Buffer.from("extra")])
        : kind === "short" ? changed.subarray(0, -1) : changed;
    };
    await assert.rejects(f.run(() => f.index()), { message: "maintenance_pack_index_refused" });
    assert.equal(f.joined(), 1);
  });
}
test("callback errors expose fixed diagnostics only", async t => {
  const f = await fixture(t);
  await assert.rejects(f.run(() => f.index(undefined, async () => {
    throw Object.assign(new Error("private stderr", { cause: "private cause" }), { stderr: "private" });
  })), error => {
    assert.ok(error instanceof Error);
    assert.equal(error.message, "maintenance_pack_index_refused");
    assert.equal(error.cause, undefined);
    assert.deepEqual(Object.keys(error), []);
    return true;
  });
  assert.equal(f.requests.length, 0);
});
for (const kind of ["valid", "unsafe integer", "outside pack", "unreferenced slot"] as const) {
  test(`64-bit offset table ${kind}`, async t => {
    const f = await fixture(t), lstat = fs.lstat.bind(fs);
    let changed = false;
    t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
      if (p === f.output && f.joined() && !changed) {
        changed = true;
        const original = await fs.readFile(f.output);
        const bytes = Buffer.concat([original.subarray(0, 1060), Buffer.alloc(8), original.subarray(1060)]);
        if (kind !== "unreferenced slot") bytes.writeUInt32BE(0x80000000, 1056);
        bytes.writeBigUInt64BE(kind === "unsafe integer" ? 9007199254740992n :
          kind === "outside pack" ? BigInt(f.bytes.length - 20) : 12n, 1060);
        digest(bytes.subarray(0, -20)).copy(bytes, bytes.length - 20);
        await fs.chmod(f.output, 0o600);
        await fs.writeFile(f.output, bytes);
      }
      return lstat(p);
    });
    if (kind === "valid") assert.equal((await f.run(() => f.index())).byteSize, 1108);
    else await assert.rejects(f.run(() => f.index()), { message: "maintenance_pack_index_refused" });
    assert.equal(changed, true);
  });
}
test("foreign symlink substituted after owned exit is never removed", async t => {
  const f = await fixture(t);
  f.owner.afterClose = async () => {
    await fs.unlink(f.output);
    await fs.symlink(f.input, f.output);
  };
  await assert.rejects(f.run(() => f.index()), { message: "maintenance_pack_index_refused" });
  assert.equal(await fs.readlink(f.output), f.input);
  assert.deepEqual(await fs.readFile(f.input), f.bytes);
});
test("input mutation after indexing is refused without removing input", async t => {
  const f = await fixture(t);
  f.owner.afterClose = async () => { await fs.writeFile(f.input, Buffer.alloc(f.bytes.length)); };
  await assert.rejects(f.run(() => f.index()), { message: "maintenance_pack_index_refused" });
  assert.deepEqual(await fs.readFile(f.input), Buffer.alloc(f.bytes.length));
});
for (const kind of ["supported", "foreign gid", "missing split", "private root"] as const) {
  test(`modeled split worker 2775 ancestor ${kind}`, async t => {
    const f = await fixture(t), lstat = fs.lstat.bind(fs), open = fs.open.bind(fs);
    const saved = process.env.UZI_UID_SPLIT;
    if (kind === "missing split") delete process.env.UZI_UID_SPLIT;
    else process.env.UZI_UID_SPLIT = "1";
    t.after(() => {
      if (saved === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = saved;
    });
    t.mock.method(process as NodeJS.Process & { getuid: () => number }, "getuid", () => 10001);
    t.mock.method(process as NodeJS.Process & { getgid: () => number }, "getgid", () => 10001);
    t.mock.method(fs, "lstat", async (p: Parameters<typeof fs.lstat>[0]) => {
      const stat = await lstat(p);
      if (typeof p === "string" && (p === f.dir || p.startsWith(f.dir + path.sep))) stat.uid = 10001;
      if (p === f.dir || (kind === "private root" && p === f.git.recoveryRoot)) {
        stat.gid = kind === "foreign gid" ? 10002 : 10001;
        stat.mode = (stat.mode & ~0o7777) | 0o2775;
      }
      return stat;
    });
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args);
      if (typeof args[0] === "string" && args[0].startsWith(f.git.recoveryRoot + path.sep)) {
        const stat = file.stat.bind(file);
        t.mock.method(file, "stat", async () => Object.assign(await stat(), { uid: 10001 }));
      }
      return file;
    });
    if (kind === "supported") assert.equal((await f.run(() => f.index())).path, f.output);
    else {
      await assert.rejects(f.run(() => f.index()), { message: "maintenance_pack_index_refused" });
      assert.equal(f.requests.length, 0);
    }
  });
}
test("custody callback only runs at actual launch and descriptor release effects", async t => {
  const f = await fixture(t);
  let calls = 0;
  const result = await f.run(() => f.index(undefined, async () => {
    calls++;
    MaintenanceResources.consume("rpcAttempts", 2);
  }));
  assert.equal(calls, 2);
  assert.equal(result.path, f.output);
});
