import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import type { BigIntStats, Stats } from "node:fs";
import { GitCache } from "../src/git.js";
import { MaintenanceResources, RecoveryMaintenance, type MaintenanceResource } from "../src/recovery-maintenance.js";
import { nullLogger } from "./helpers.js";

const nativeExec = promisify(execFile);
const exec = (command: string, args: string[]) => {
  const result = nativeExec(command, args, { env: { PATH: "/usr/bin:/bin",
    TMPDIR: process.env.TMPDIR, LANG: "C", LC_ALL: "C", GIT_CONFIG_NOSYSTEM: "1",
    GIT_CONFIG_GLOBAL: "/dev/null", GIT_TERMINAL_PROMPT: "0" } });
  result.child.stdin?.end();
  return result;
};
const refused = { message: "maintenance_object_snapshot_refused" };
const exhausted = { message: "maintenance_resource_exhausted" };
const cap = 256 * 1024 ** 2;
type Handle = Awaited<ReturnType<typeof fs.open>>;
const latch = () => {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
};
async function fixture(t: test.TestContext, profile: { split?: boolean; mode?: number; gid?: number } = {}) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/object-snapshot-"));
  const git = new GitCache(dir, nullLogger());
  const clone = path.join(dir, "runner", "repo", "branch.attempt-fixture");
  const objects = path.join(clone, ".git", "objects");
  await fs.mkdir(objects, { recursive: true });
  await fs.writeFile(path.join(objects, "leaf"), "abc");
  await fs.mkdir(git.recoveryRoot, { mode: 0o700 });
  const out = path.join(git.recoveryRoot, "snapshot");
  const controller = new AbortController();
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture" });
  const sourceStat = await fs.lstat(clone, { bigint: true });
  const source = { clonePath: clone, identity: sourceStat.dev + ":" + sourceStat.ino };
  const ancestors = new Set<string>();
  for (let p = dir;; p = path.dirname(p)) {
    ancestors.add(p);
    if (path.dirname(p) === p) break;
  }
  // Model trusted deployment ancestors, NOT actual admission on a deployed PVC.
  // Both descriptor and named stats retain actual bigint identities/timestamps.
  // Ledger and test cleanup paths are never projected or fault-injected.
  const workerUid = 10001;
  t.mock.method(process as NodeJS.Process & { getuid: () => number }, "getuid", () => workerUid);
  t.mock.method(process as NodeJS.Process & { getgid: () => number }, "getgid", () => workerUid);
  const savedSplit = process.env.UZI_UID_SPLIT;
  if (profile.split) process.env.UZI_UID_SPLIT = "1"; else delete process.env.UZI_UID_SPLIT;
  t.after(() => {
    if (savedSplit === undefined) delete process.env.UZI_UID_SPLIT;
    else process.env.UZI_UID_SPLIT = savedSplit;
  });
  const fdPaths = new Map<number, string>();
  const physical = (p: unknown): string => {
    if (typeof p !== "string") return "";
    const match = /^\/proc\/self\/fd\/([0-9]+)(?:\/(.*))?$/.exec(p);
    return match && fdPaths.has(Number(match[1]))
      ? path.join(fdPaths.get(Number(match[1]))!, match[2] ?? "") : p;
  };
  const owned = (p: string) => ancestors.has(p) || p === git.recoveryRoot ||
    p.startsWith(git.recoveryRoot + "/") || p === path.join(dir, "runner") || p.startsWith(path.join(dir, "runner") + "/");
  const project = <T extends Stats | BigIntStats>(p: string, s: T): T => {
    if (!owned(p)) return s;
    const big = typeof s.mode === "bigint";
    Object.assign(s, { uid: big ? BigInt(workerUid) : workerUid, gid: big ? BigInt(workerUid) : workerUid });
    if (ancestors.has(p)) {
      const mode = Number(s.mode) & ~0o022;
      Object.assign(s, { mode: big ? BigInt(mode) : mode });
    }
    if (p === path.dirname(dir) && profile.mode !== undefined) {
      const mode = (Number(s.mode) & ~0o7777) | profile.mode;
      Object.assign(s, { mode: big ? BigInt(mode) : mode,
        gid: big ? BigInt(profile.gid ?? workerUid) : profile.gid ?? workerUid });
    }
    return s;
  };
  const hooks: {
    open?: (file: Handle, p: string, flags: number | string) => void | Promise<void>;
    stat?: (p: string, s: BigIntStats | Stats) => void | Promise<void>;
    raw?: () => void | Promise<void>;
  } = {};
  const lstat = fs.lstat.bind(fs), open = fs.open.bind(fs), opendir = fs.opendir.bind(fs);
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    const p = physical(args[0]);
    const s = project(p, await lstat(...args));
    if (owned(p)) await hooks.stat?.(p, s);
    return s;
  });
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args), p = physical(args[0]);
    if (!owned(p)) return file;
    fdPaths.set(file.fd, p);
    const stat = file.stat.bind(file);
    t.mock.method(file, "stat", async (...args: Parameters<typeof file.stat>) => project(p, (await stat(...args))!));
    await hooks.open?.(file, p, args[1]!);
    return file;
  });
  t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    const stream = await opendir(...args), p = physical(args[0]);
    if (p === objects || p.startsWith(objects + "/")) {
      const read = stream.read.bind(stream);
      t.mock.method(stream, "read", async () => { await hooks.raw?.(); return read(); });
    }
    return stream;
  });
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
  const charge = (resource: MaintenanceResource, amount: number) =>
    MaintenanceResources.run(admitted.value, () => MaintenanceResources.consume(resource, amount));
  let launches = 0, callbacks = 0;
  const run = <T>(fn: () => Promise<T>) => MaintenanceResources.run(admitted.value, () =>
    git.withBoundaryProcessSpawner(async () => { launches++; throw Error("unexpected child"); }, controller.signal, fn));
  const snapshot = (before = async () => {}, input = source, output = out) =>
    git["snapshotMaintenanceObjects"](input, output, async () => { callbacks++; await before(); });
  return { git, dir, clone, objects, out, source, hooks, physical, controller, run, snapshot, charge,
    callbacks: () => callbacks, launches: () => launches };
}
function longFixturePath(root: string, length: number): string {
  while (length - Buffer.byteLength(root) - 1 > 200) root += "/" + "p".repeat(200);
  return root + "/" + "q".repeat(length - Buffer.byteLength(root) - 1);
}
async function removeLongFixtureOutput(out: string, children: ReadonlyArray<readonly [string, boolean]>) {
  let directory: Handle | undefined;
  try {
    directory = await fs.open(out, 0);
    for (const [name, isDirectory] of children) {
      try {
        const p = "/proc/self/fd/" + directory.fd + "/" + name;
        if (isDirectory) await fs.rmdir(p); else await fs.unlink(p);
      } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
    }
  } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
  finally { await directory?.close(); }
  try { await fs.rmdir(out); }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
}
for (const allowed of [true, false]) {
  test("derived source directory paths enforce 4096 bytes: " + allowed, async t => {
    const f = await fixture(t), clone = longFixturePath(path.join(f.dir, "runner"), allowed ? 4070 : 4078);
    const objects = path.join(clone, ".git", "objects");
    await fs.mkdir(objects, { recursive: true });
    const directory = await fs.open(objects, 0);
    try {
      await fs.mkdir("/proc/self/fd/" + directory.fd + "/nested-long");
      const stat = await fs.lstat(clone, { bigint: true });
      const capture = () => f.run(() => f.snapshot(undefined, {
        clonePath: clone, identity: stat.dev + ":" + stat.ino,
      }));
      if (allowed) {
        await capture();
        assert.ok((await fs.lstat(path.join(f.out, "nested-long"))).isDirectory());
      } else await assert.rejects(capture(), refused);
    } finally {
      try { await fs.rmdir("/proc/self/fd/" + directory.fd + "/nested-long"); }
      finally { await directory.close(); }
    }
  });
  test("derived destination directory paths enforce 4096 bytes: " + allowed, async t => {
    const f = await fixture(t);
    await fs.mkdir(path.join(f.objects, "nested-long"));
    const parent = longFixturePath(f.git.recoveryRoot, allowed ? 4066 : 4076);
    await fs.mkdir(parent, { recursive: true, mode: 0o700 });
    const out = parent + "/snapshot-long";
    const capture = () => f.run(() => f.snapshot(undefined, f.source, out));
    try {
      if (allowed) {
        await capture();
        assert.equal(await fs.readFile(path.join(out, "leaf"), "utf8"), "abc");
        assert.ok((await fs.lstat(path.join(out, "nested-long"))).isDirectory());
      } else await assert.rejects(capture(), refused);
    } finally {
      // Descriptor-relative cleanup also handles the original buggy overlong tree.
      // Only these known fixture-created children can exist in the exclusive output.
      await removeLongFixtureOutput(out, [["nested-long", true], ["leaf", false]]);
    }
  });
}
test("derived regular source file paths cannot exceed 4096 bytes", async t => {
  const f = await fixture(t), clone = longFixturePath(path.join(f.dir, "runner"), 4078);
  const objects = path.join(clone, ".git", "objects");
  await fs.mkdir(objects, { recursive: true });
  const directory = await fs.open(objects, 0);
  const file = "/proc/self/fd/" + directory.fd + "/payload-long";
  try {
    await fs.writeFile(file, "abc");
    const stat = await fs.lstat(clone, { bigint: true });
    await assert.rejects(f.run(() => f.snapshot(undefined, {
      clonePath: clone, identity: stat.dev + ":" + stat.ino,
    })), refused);
    assert.equal(await fs.readFile(file, "utf8"), "abc");
  } finally { try { await fs.unlink(file); } finally { await directory.close(); } }
});
test("derived regular destination file paths cannot exceed 4096 bytes", async t => {
  const f = await fixture(t);
  await fs.writeFile(path.join(f.objects, "payload-long"), "abc");
  const parent = longFixturePath(f.git.recoveryRoot, 4076);
  await fs.mkdir(parent, { recursive: true, mode: 0o700 });
  const out = parent + "/snapshot-long";
  try {
    await assert.rejects(f.run(() => f.snapshot(undefined, f.source, out)), refused);
    assert.equal(await fs.readFile(path.join(f.objects, "payload-long"), "utf8"), "abc");
  } finally {
    await removeLongFixtureOutput(out, [["payload-long", false], ["leaf", false]]);
  }
});
test("detaches actual loose objects, pack/index and nested encoded bytes without source writes or launches", async t => {
  const f = await fixture(t);
  await exec("/usr/bin/git", ["init", "--quiet", "--template=", f.clone]);
  await fs.writeFile(path.join(f.clone, "body"), "snapshot fixture\n");
  const { stdout } = await exec("/usr/bin/git", ["-C", f.clone, "hash-object", "-w", "body"]);
  await exec("/usr/bin/git", ["-C", f.clone, "update-ref", "refs/fixture", stdout.trim()]);
  await exec("/usr/bin/git", ["-C", f.clone, "pack-objects", "--all", path.join(f.objects, "pack", "fixture")]);
  await fs.mkdir(path.join(f.objects, "nested"));
  await fs.writeFile(path.join(f.objects, "nested", "bytes"), Buffer.from([0, 255, 1]));
  const before = new Map<string, { bytes: Buffer; stat: BigIntStats }>();
  async function inventory(p: string, prefix = "") {
    for (const ent of await fs.readdir(p, { withFileTypes: true })) {
      const rel = prefix + ent.name;
      if (ent.isDirectory()) await inventory(path.join(p, ent.name), rel + "/");
      else before.set(rel, { bytes: await fs.readFile(path.join(p, ent.name)),
        stat: await fs.lstat(path.join(p, ent.name), { bigint: true }) });
    }
  }
  await inventory(f.objects);
  const result = await f.run(() => f.snapshot());
  assert.deepEqual(result, { path: f.out, byteSize: [...before.values()].reduce((n, r) => n + r.bytes.length, 0),
    fileCount: before.size });
  for (const [rel, r] of before) {
    assert.deepEqual(await fs.readFile(path.join(f.out, rel)), r.bytes);
    const source = await fs.lstat(path.join(f.objects, rel), { bigint: true });
    const dest = await fs.lstat(path.join(f.out, rel), { bigint: true });
    assert.equal(source.ino, r.stat.ino);
    assert.equal(source.mtimeNs, r.stat.mtimeNs);
    assert.equal(source.ctimeNs, r.stat.ctimeNs);
    assert.notEqual(dest.ino, source.ino);
    assert.equal(dest.mode & 0o7777n, 0o600n);
    assert.equal(dest.nlink, 1n);
  }
  await fs.writeFile(path.join(f.objects, "leaf"), "changed");
  assert.equal(await fs.readFile(path.join(f.out, "leaf"), "utf8"), "abc");
  assert.equal(f.launches(), 0);
  assert.ok(f.callbacks() >= before.size * 2 + 2);
});
test("accepts bounded caller-family paths beyond canonical two-level shape", async t => {
  const f = await fixture(t), deep = path.join(f.dir, "runner", "family", "generation", "attempt");
  await fs.mkdir(path.dirname(deep), { recursive: true });
  await fs.rename(f.clone, deep);
  const s = await fs.lstat(deep, { bigint: true });
  const input = { clonePath: deep, identity: s.dev + ":" + s.ino };
  assert.equal((await f.run(() => f.snapshot(undefined, input))).fileCount, 1);
});
for (const kind of ["wrong identity", "rounded identity", "relative source", "outside source", "unnormalized source",
  "relative output", "outside output", "root output", "existing output", "unsafe name"] as const) {
  test("refuses " + kind, async t => {
    const f = await fixture(t), input = { ...f.source };
    let out = f.out;
    if (kind === "wrong identity") input.identity = "0:0";
    if (kind === "rounded identity") input.identity = input.identity + "\n";
    if (kind === "relative source") input.clonePath = "runner/repo/branch";
    if (kind === "outside source") input.clonePath = f.dir;
    if (kind === "unnormalized source") input.clonePath += "/../branch";
    if (kind === "relative output") out = "snapshot";
    if (kind === "outside output") out = path.join(f.dir, "foreign");
    if (kind === "root output") out = f.git.recoveryRoot;
    if (kind === "existing output") await fs.mkdir(f.out);
    if (kind === "unsafe name") await fs.writeFile(path.join(f.objects, "bad\\name"), "x");
    await assert.rejects(f.run(() => f.snapshot(undefined, input, out)), refused);
    assert.equal(f.launches(), 0);
  });
}
for (const kind of ["source ancestor", "clone", ".git", "objects", "leaf", "target parent", "hardlink", "FIFO", "raw invalid UTF8"]) {
  test("rejects " + kind, async t => {
    const f = await fixture(t);
    const linkPath = kind === "source ancestor" ? path.dirname(f.clone) : kind === "clone" ? f.clone :
      kind === ".git" ? path.join(f.clone, ".git") : kind === "objects" ? f.objects :
      kind === "target parent" ? f.git.recoveryRoot : path.join(f.objects, "leaf");
    if (kind === "hardlink") await fs.link(linkPath, path.join(f.objects, "alias"));
    else if (kind === "FIFO") {
      await fs.unlink(linkPath); await exec("/usr/bin/mkfifo", [linkPath]);
    } else if (kind === "raw invalid UTF8") {
      await fs.writeFile(Buffer.concat([Buffer.from(f.objects + "/"), Buffer.from([255])]), "x");
    } else {
      await fs.rename(linkPath, linkPath + "-real"); await fs.symlink(linkPath + "-real", linkPath);
    }
    await assert.rejects(f.run(() => f.snapshot()), refused);
  });
}
for (const control of ["alternates", "http-alternates"]) {
  for (const kind of ["empty", "link", "directory", "late insertion", "handoff insertion"]) {
    test(control + " refuses " + kind + " without reading control bytes", async t => {
      const f = await fixture(t), info = path.join(f.objects, "info");
      await fs.mkdir(info);
      const p = path.join(info, control);
      let reads = 0, inserted = false;
      f.hooks.open = (file, named) => {
        if (named === p) t.mock.method(file, "read", async () => { reads++; throw Error("control read"); });
        if (kind === "late insertion" && named === path.join(f.out, "leaf") && !inserted) {
          inserted = true; return fs.writeFile(p, "");
        }
      };
      if (kind === "empty") await fs.writeFile(p, "");
      if (kind === "link") await fs.symlink(path.join(f.objects, "leaf"), p);
      if (kind === "directory") await fs.mkdir(p);
      await assert.rejects(f.run(() => f.snapshot(async () => {
        // Empty fixture has two mkdirs (root + info), file create/write, handoff.
        if (kind === "handoff insertion" && f.callbacks() === 5 && !inserted) {
          inserted = true; await fs.writeFile(p, "");
        }
      })), refused);
      assert.equal(reads, 0);
      if (kind.includes("insertion")) assert.equal(inserted, true);
    });
  }
}
test("no producer token or discovery scope can authorize a snapshot", async t => {
  const f = await fixture(t);
  await assert.rejects(f.snapshot(), { message: "maintenance_token_invalid" });
  await assert.rejects(MaintenanceResources.runDiscovery(() => f.snapshot()), { message: "maintenance_token_invalid" });
  assert.equal(f.callbacks(), 0);
});
for (const kind of ["mutation", "replacement", "late earlier leaf", "late directory replacement", "growth"]) {
  test("rejects changed source: " + kind, async t => {
    const f = await fixture(t);
    await fs.mkdir(path.join(f.objects, "later"));
    await fs.writeFile(path.join(f.objects, "later", "second"), "later");
    let changed = false;
    f.hooks.open = (file, p) => {
      if (p !== path.join(f.objects, "leaf")) return;
      const read = file.read.bind(file);
      t.mock.method(file, "read", async (buffer: Buffer, offset: number, length: number, position: number) => {
        const result = await read(buffer, offset, length, position);
        if (!changed && !kind.startsWith("late")) {
          changed = true;
          if (kind === "replacement") {
            await fs.rename(p, p + "-old"); await fs.writeFile(p, "abc");
          } else await fs.writeFile(p, kind === "growth" ? "abcdefgh" : "xyz");
        }
        return result;
      });
    };
    await assert.rejects(f.run(() => f.snapshot(async () => {
      if (!changed && kind.startsWith("late") && f.callbacks() >= 6) {
        changed = true;
        if (kind === "late directory replacement") {
          await fs.rename(path.join(f.objects, "later"), path.join(f.objects, "old"));
          await fs.mkdir(path.join(f.objects, "later"));
        } else await fs.writeFile(path.join(f.objects, "leaf"), "xyz");
      }
    })), refused);
    assert.equal(changed, true);
  });
}
for (const resource of ["sourceBytes", "sourceEntries", "adminEntries", "metadataBytes", "artifactBytes"] as const) {
  test("original shared quota precharges " + resource, async t => {
    const f = await fixture(t);
    const limit = resource === "sourceEntries" ? 65536 : resource === "adminEntries" ? 4096 : cap;
    f.charge(resource, limit - (resource === "artifactBytes" ? 2 : 0));
    let writes = 0;
    f.hooks.open = (file, p) => {
      if (p.startsWith(f.out + "/")) t.mock.method(file, "write", async () => { writes++; throw Error("write"); });
    };
    await assert.rejects(f.run(() => f.snapshot()), exhausted);
    assert.equal(writes, 0);
  });
}
test("a different output path cannot renew the original artifact quota", async t => {
  const f = await fixture(t);
  f.charge("artifactBytes", cap - 5);
  await f.run(() => f.snapshot());
  await assert.rejects(f.run(() => f.snapshot(undefined, f.source, f.out + "-second")), exhausted);
  assert.equal(await fs.readFile(path.join(f.out, "leaf"), "utf8"), "abc");
  assert.equal(await fs.readFile(path.join(f.objects, "leaf"), "utf8"), "abc");
  await assert.rejects(fs.lstat(f.out + "-second"), { code: "ENOENT" });
});
test("source DAC refusal never repairs permissions or launches a privileged reader", async t => {
  const f = await fixture(t), open = fs.open.bind(fs), chmod = fs.chmod.bind(fs);
  let permissionChanges = 0;
  t.mock.method(fs, "chmod", async (...args: Parameters<typeof fs.chmod>) => {
    permissionChanges++;
    return chmod(...args);
  });
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    if (f.physical(args[0]) === path.join(f.objects, "leaf"))
      throw Object.assign(new Error("fixture DAC refusal"), { code: "EACCES" });
    return open(...args);
  });
  await assert.rejects(f.run(() => f.snapshot()), refused);
  assert.equal(permissionChanges, 0);
  assert.equal(f.launches(), 0);
  assert.equal(await fs.readFile(path.join(f.objects, "leaf"), "utf8"), "abc");
});
test("source request accounting includes raw names, EOF and short read capacity", async t => {
  const f = await fixture(t);
  let requested = 0, rawReads = 0;
  f.hooks.raw = () => { rawReads++; };
  f.hooks.open = (file, p) => {
    if (p !== path.join(f.objects, "leaf")) return;
    const read = file.read.bind(file);
    t.mock.method(file, "read", async (b: Buffer, o: number, n: number, at: number) => {
      requested += n; return read(b, o, Math.min(n, 1), at);
    });
  };
  await f.run(() => f.snapshot());
  assert.equal(rawReads, 2);
  assert.equal(requested, 4 + 3 + 2 + 1);
  f.charge("sourceBytes", cap - requested - rawReads * 255);
  assert.throws(() => f.charge("sourceBytes", 1), exhausted);
});
test("partial writes retry only unwritten tail and charge the chunk once", async t => {
  const f = await fixture(t), attempts: Array<[number, number, number]> = [];
  f.hooks.open = (file, p) => {
    if (p !== path.join(f.out, "leaf")) return;
    const write = file.write.bind(file);
    t.mock.method(file, "write", async (b: Buffer, o: number, n: number, at: number) => {
      attempts.push([o, n, at]); return write(b, o, 1, at);
    });
  };
  await f.run(() => f.snapshot());
  assert.deepEqual(attempts, [[0, 3, 0], [1, 2, 1], [2, 1, 2]]);
  assert.equal(f.callbacks(), 6); // mkdir + create + three attempts + handoff.
  assert.equal(await fs.readFile(path.join(f.out, "leaf"), "utf8"), "abc");
  f.charge("artifactBytes", cap - 3);
  assert.throws(() => f.charge("artifactBytes", 1), exhausted);
});
for (const kind of ["zero write", "read fault", "write fault", "read cancel", "write cancel"]) {
  test("settles serial filesystem I/O and cleans owned output: " + kind, async t => {
    const f = await fixture(t), entered = latch(), release = latch();
    let settled = false, reached = false, released = false, earlyCloses = 0;
    f.hooks.open = (file, p) => {
      if (p === path.join(f.objects, "leaf") || p === path.join(f.out, "leaf")) {
        const close = file.close.bind(file);
        t.mock.method(file, "close", async () => {
          if (reached && !released) earlyCloses++;
          await close();
        });
      }
      const reading = kind.startsWith("read");
      if (p !== path.join(reading ? f.objects : f.out, "leaf")) return;
      if (reading) {
        const read = file.read.bind(file);
        t.mock.method(file, "read", async (b: Buffer, o: number, n: number, at: number) => {
          reached = true; entered.release(); await release.promise;
          if (kind === "read fault") throw Error("private read evidence");
          return read(b, o, n, at);
        });
      } else {
        const write = file.write.bind(file);
        t.mock.method(file, "write", async (b: Buffer, o: number, n: number, at: number) => {
          reached = true; entered.release(); await release.promise;
          if (kind === "zero write") return { bytesWritten: 0, buffer: b };
          if (kind === "write fault") throw Error("private write evidence");
          return write(b, o, n, at);
        });
      }
    };
    const result = f.run(() => f.snapshot()).then(value => { settled = true; return value; },
      error => { settled = true; return error; });
    try {
      await Promise.race([entered.promise, result.then(() => { throw Error("missed I/O seam"); })]);
      if (kind.endsWith("cancel")) f.controller.abort();
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(earlyCloses, 0, "no descriptor close while admitted I/O is pending");
      assert.equal(settled, false);
    } finally { released = true; release.release(); await result; }
    assert.equal(reached, true);
    assert.ok((await result) instanceof Error);
    assert.equal((await result).message, kind.endsWith("cancel") ? "maintenance_token_invalid" : "maintenance_object_snapshot_refused");
    await assert.rejects(fs.lstat(f.out), { code: "ENOENT" });
  });
}
test("cleanup preserves a foreign replacement name", async t => {
  const f = await fixture(t);
  f.hooks.open = (file, p) => {
    if (p !== path.join(f.out, "leaf")) return;
    t.mock.method(file, "write", async () => {
      await fs.rename(p, path.join(f.out, "owned-renamed"));
      await fs.writeFile(p, "foreign");
      throw Error("private failure");
    });
  };
  await assert.rejects(f.run(() => f.snapshot()), refused);
  assert.equal(await fs.readFile(path.join(f.out, "leaf"), "utf8"), "foreign");
});
test("modeled supported split-worker 2775 strict ancestor keeps private output", async t => {
  const f = await fixture(t, { split: true, mode: 0o2775, gid: 10001 });
  assert.equal((await f.run(() => f.snapshot())).fileCount, 1);
  assert.equal((await fs.lstat(f.out)).mode & 0o5777, 0o700);
});
for (const profile of [{ split: false, mode: 0o2775 }, { split: true, mode: 0o2775, gid: 10002 },
  { split: true, mode: 0o2777 }]) {
  test("refuses unsupported modeled parent profile " + JSON.stringify(profile), async t => {
    const f = await fixture(t, profile);
    await assert.rejects(f.run(() => f.snapshot()), refused);
    await assert.rejects(fs.lstat(f.out), { code: "ENOENT" });
  });
}
test("refuses a source info change during the last control probe", async t => {
  const f = await fixture(t);
  await fs.mkdir(path.join(f.objects, "info"));
  const lstat = fs.lstat.bind(fs);
  let probes = 0;
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    try { return await lstat(...args); }
    catch (error) {
      if (f.physical(args[0]) === path.join(f.objects, "info", "http-alternates") && ++probes === 3)
        await fs.writeFile(path.join(f.objects, "info", "late"), "retained source");
      throw error;
    }
  });
  await assert.rejects(f.run(() => f.snapshot()), refused);
  assert.equal(probes, 3);
  assert.equal(await fs.readFile(path.join(f.objects, "info", "late"), "utf8"), "retained source");
  await assert.rejects(fs.lstat(f.out), { code: "ENOENT" });
});
test("new private directories retain harmless inherited setgid", async t => {
  const f = await fixture(t);
  await fs.chmod(f.git.recoveryRoot, 0o2700);
  await f.run(() => f.snapshot());
  assert.equal((await fs.lstat(f.out)).mode & 0o7777, 0o2700);
  assert.equal((await fs.lstat(path.join(f.out, "leaf"))).mode & 0o7777, 0o600);
});
for (const mode of [0o4700, 0o1700]) {
  test("refuses unexpected bits on a newly created directory: " + mode.toString(8), async t => {
    const f = await fixture(t), mkdir = fs.mkdir.bind(fs);
    t.mock.method(fs, "mkdir", async (...args: Parameters<typeof fs.mkdir>) => {
      const result = await mkdir(...args);
      if (f.physical(args[0]) === f.out) await fs.chmod(f.out, mode);
      return result;
    });
    await assert.rejects(f.run(() => f.snapshot()), refused);
    assert.equal(await fs.readFile(path.join(f.objects, "leaf"), "utf8"), "abc");
  });
}
test("private root never inherits the group-writable ancestor exception", async t => {
  const f = await fixture(t);
  await fs.chmod(f.git.recoveryRoot, 0o770);
  await assert.rejects(f.run(() => f.snapshot()), refused);
});
for (const message of ["maintenance_resource_exhausted", "maintenance_token_invalid", "maintenance_clock_invalid",
  "maintenance_resource_exhausted private", "private diagnostic"]) {
  test("fixed refusal strips cause and properties: " + message, async t => {
    const f = await fixture(t);
    await assert.rejects(f.run(() => f.snapshot(async () => {
      throw Object.assign(new Error(message, { cause: "private" }), { stderr: "private" });
    })), error => {
      assert.ok(error instanceof Error);
      assert.equal(error.message, ["maintenance_resource_exhausted", "maintenance_token_invalid",
        "maintenance_clock_invalid"].includes(message) ? message : refused.message);
      assert.equal(error.cause, undefined);
      assert.deepEqual(Object.keys(error), []);
      return true;
    });
  });
}
