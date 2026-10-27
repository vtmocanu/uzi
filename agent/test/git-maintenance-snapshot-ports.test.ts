import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHash } from "node:crypto";
import { Readable } from "node:stream";
import { MaintenanceResources, type MaintenanceResource } from "../src/recovery-maintenance.js";
import { execFileSync } from "node:child_process";
import { snapshotPortFixture, snapshotLinux, portLongPath, portEmptyObjects } from "./maintenance-snapshot-port-fixture.js";

type Fixture = Awaited<ReturnType<typeof snapshotPortFixture>>;
const layout = { message: "maintenance_source_layout_unsupported" };
async function sourceAt(f: Fixture, clonePath: string) {
  const stat = await fs.lstat(clonePath, { bigint: true });
  return { ...f.source, clonePath, identity: stat.dev + ":" + stat.ino };
}
async function bytesIdentity(root: string, labelRoot = root): Promise<string> {
  const hash = createHash("sha256");
  const walk = async (p: string): Promise<void> => {
    const stat = await fs.lstat(p, { bigint: true });
    hash.update(path.join(labelRoot, path.relative(root, p)) + ":" + stat.dev + ":" + stat.ino + ":" + stat.mode + ":" + stat.nlink);
    if (stat.isDirectory()) for (const name of (await fs.readdir(p)).sort()) await walk(path.join(p, name));
    else hash.update(await fs.readFile(p));
  };
  await walk(root);
  return hash.digest("hex");
}

async function refIdentity(bare: string): Promise<Array<[string, string | null]>> {
  const result: Array<[string, string | null]> = [];
  for (const name of ["HEAD", "refs", "packed-refs", "reftable"]) {
    try { result.push([name, await bytesIdentity(path.join(bare, name), name)]); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      result.push([name, null]);
    }
  }
  return result;
}

for (const allowed of [true, false]) {
  test("ported snapshot: derived source directory paths enforce 4096 bytes: " + allowed, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    const suffix = "/.git/objects/info/commit-graphs";
    const clone = portLongPath(path.join(f.dir, "runner"), (allowed ? 4095 : 4097) - Buffer.byteLength(suffix));
    await portEmptyObjects(clone);
    const info = path.join(clone, ".git", "objects", "info");
    await fs.mkdir(info);
    const parent = await fs.open(info, "r");
    const child = "/proc/self/fd/" + parent.fd + "/commit-graphs";
    await fs.mkdir(child);
    const target = path.join(info, "commit-graphs");
    assert.equal(Buffer.byteLength(target), allowed ? 4095 : 4097);
    let lookup = false;
    const lstat = fs.lstat.bind(fs);
    t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
      if (typeof args[0] === "string" && args[0].endsWith("/commit-graphs")) lookup = true;
      return lstat(...args);
    });
    try {
      const source = await sourceAt(f, clone);
      if (allowed) await f.walk({ source }, target);
      else await assert.rejects(f.walk({ source }), layout);
      assert.equal(lookup, allowed, "overlong derived directory refused before fd-relative lookup");
      await f.assertRetained();
    } finally { await fs.rmdir(child); await parent.close(); }
  });
  test("ported snapshot: derived destination directory paths enforce 4096 bytes: " + allowed, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    const outDir = portLongPath(f.cache.recoveryRoot, (allowed ? 4095 : 4097) - Buffer.byteLength("/read"));
    await fs.mkdir(path.dirname(outDir), { recursive: true, mode: 0o700 });
    const target = path.join(outDir, "read"), completed = new Error("fixture_directory_created");
    let attempted = false, created = false;
    const mkdir = fs.mkdir.bind(fs);
    t.mock.method(fs, "mkdir", async (...args: Parameters<typeof fs.mkdir>) => {
      if (args[0] !== target) return mkdir(...args);
      attempted = true;
      await mkdir(...args);
      created = true;
      throw completed;
    });
    if (allowed) {
      await assert.rejects(f.importTip({ outDir }), error => error === completed);
      assert.equal((await fs.lstat(target)).isDirectory(), true);
    } else await assert.rejects(f.importTip({ outDir }), layout);
    assert.equal(attempted, allowed, "derived directory bound checked before creation");
    assert.equal(created, allowed);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.assertRetained();
  });
}

test("ported snapshot: derived regular source file paths cannot exceed 4096 bytes", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), name = "tmp_" + "x".repeat(251);
  const clone = portLongPath(path.join(f.dir, "runner"), 4097 - Buffer.byteLength("/.git/objects/" + name));
  await portEmptyObjects(clone);
  const parent = await fs.open(path.join(clone, ".git", "objects"), "r");
  const leaf = "/proc/self/fd/" + parent.fd + "/" + name;
  await fs.writeFile(leaf, "abc");
  let attempted = false;
  const open = fs.open.bind(fs);
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    if (typeof args[0] === "string" && args[0].endsWith("/" + name)) attempted = true;
    return open(...args);
  });
  try {
    await assert.rejects(f.walk({ source: await sourceAt(f, clone) }), layout);
    assert.equal(attempted, false);
    assert.equal(await fs.readFile(leaf, "utf8"), "abc");
    await f.assertRetained();
  } finally { await fs.unlink(leaf); await parent.close(); }
});

test("ported snapshot: derived regular destination file paths cannot exceed 4096 bytes", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t);
  // The fixed final pack basename is longer than its staging name. All parent
  // directories and staging files fit; the final artifact must be bounded before rename.
  const outDir = portLongPath(f.cache.recoveryRoot,
    4097 - Buffer.byteLength("/proof/objects/pack/pack-" + "0".repeat(40) + ".pack"));
  await fs.mkdir(path.dirname(outDir), { recursive: true, mode: 0o700 });
  let overlongRename = false;
  const rename = fs.rename.bind(fs);
  t.mock.method(fs, "rename", async (...args: Parameters<typeof fs.rename>) => {
    overlongRename ||= Buffer.byteLength(String(args[1])) > 4096;
    return rename(...args);
  });
  await assert.rejects(f.importTip({ outDir }), layout);
  assert.equal(overlongRename, false, "final derived file refused before rename");
  assert.ok(f.requests.some(r => r.argv.includes("pack-objects")), "actual staging reached");
  assert.equal(f.requests.some(r => r.argv.includes("index-pack") && r.argv.includes("--stdin")), false);
  await f.assertRetained();
});

test("ported snapshot: real shared clone descendants traverse offline through completed private source and base", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t);
  const poison = "[include\npoisoned source config\n";
  await fs.writeFile(path.join(f.clone, ".git", "config"), poison);
  const original = await bytesIdentity(path.join(f.clone, ".git"));
  assert.deepEqual(await f.importTip(), { kind: "imported", sourceSha: f.tip });
  assert.equal(await f.native(f.bare, ["show", f.tip + ":new"]), "new selected blob");
  assert.equal(await f.native(f.bare, ["show", f.tip + ":base"]), "canonical prerequisite");
  assert.equal(await bytesIdentity(path.join(f.clone, ".git")), original);
  assert.equal(await fs.readFile(path.join(f.clone, ".git", "config"), "utf8"), poison);
  assert.ok(f.requests.every(r => !r.argv.some(a => a === "fetch" || a === "upload-pack")));
  await f.assertRetained();
});

for (const control of ["alternates", "http-alternates"]) for (const kind of ["file", "symlink", "FIFO"]) {
  test("ported snapshot: known base omits non-authoritative source " + control + " " + kind + " without opening", snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    const target = path.join(f.dir, "foreign-alternate");
    await fs.mkdir(target);
    const marker = path.join(target, "marker");
    await fs.writeFile(marker, "foreign encoded bytes");
    const name = path.join(f.clone, ".git", "objects", "info", control);
    await fs.unlink(name).catch(error => { if (error.code !== "ENOENT") throw error; });
    if (kind === "file") await fs.writeFile(name, target + "\n");
    else if (kind === "symlink") await fs.symlink(target, name);
    else execFileSync("/usr/bin/mkfifo", [name], { stdio: "pipe" });
    let followed = false;
    const open = fs.open.bind(fs);
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      followed ||= typeof args[0] === "string" && (args[0] === target || args[0].startsWith(target + "/"));
      return open(...args);
    });
    // Approved replacement: safely inspect a regular control itself if needed,
    // but reject this layout without following its referenced target or launching Git.
    await assert.rejects(f.walk(), layout);
    assert.equal(followed, false);
    assert.equal(await fs.readFile(marker, "utf8"), "foreign encoded bytes");
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.assertRetained();
  });
}


for (const kind of ["bad identity", "relative", "unnormalized", "outside", "nested", "basename",
  "repos symlink", "bare symlink", "objects symlink", "unsafe repos", "unsafe bare", "unsafe objects",
  "alternates", "http-alternates", "control symlink", "control FIFO", "control directory", "ordinary FIFO",
  "existing sibling", "sibling symlink"]) {
  test("ported snapshot: known base refuses " + kind + " and never hands off a partial source", snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    const original = { ...f.request.original };
    const objects = path.join(f.bare, "objects");
    const sourceObjects = path.join(f.clone, ".git", "objects");
    const sourceBefore = await bytesIdentity(sourceObjects);
    const canonicalRefs = await refIdentity(f.bare);
    let substituted = false;
    if (kind === "bad identity") {
      // knownBase.identity is no longer an input. Substitute the real canonical
      // directory after its initial dev/ino pin, before the walk revalidates it.
      f.cache.beforePhase = async phase => {
        if (phase !== "walk") return;
        await fs.rename(objects, objects + "-original");
        await fs.mkdir(objects, { mode: 0o700 });
        substituted = true;
      };
    }
    if (kind === "relative") original.barePath = "repos/fixture.git";
    if (kind === "unnormalized") original.barePath += "/../" + path.basename(f.bare);
    if (kind === "outside") original.barePath = f.clone;
    if (kind === "nested") original.barePath = path.join(f.bare, "nested.git");
    if (kind === "basename") {
      original.barePath += "-invalid";
      await fs.rename(f.bare, original.barePath);
    }
    if (["repos symlink", "bare symlink", "objects symlink"].includes(kind)) {
      const p = kind === "repos symlink" ? path.dirname(f.bare) : kind === "bare symlink" ? f.bare : objects;
      await fs.rename(p, p + "-real");
      await fs.symlink(p + "-real", p);
    }
    if (kind.startsWith("unsafe")) await fs.chmod(kind === "unsafe repos" ? path.dirname(f.bare) :
      kind === "unsafe bare" ? f.bare : objects, 0o777);
    let control: string | undefined;
    if (kind === "alternates" || kind === "http-alternates" || kind.startsWith("control")) {
      await fs.mkdir(path.join(objects, "info"), { recursive: true });
      control = path.join(objects, "info", kind === "http-alternates" ? kind : "alternates");
      if (kind === "control symlink") await fs.symlink(sourceObjects, control);
      else if (kind === "control FIFO") execFileSync("/usr/bin/mkfifo", [control], { stdio: "pipe" });
      else if (kind === "control directory") await fs.mkdir(control);
      else await fs.writeFile(control, sourceObjects + "\n");
    }
    const controlBefore = control ? await fs.lstat(control, { bigint: true }) : undefined;
    // The importer pins the required canonical pack directory; arbitrary base
    // object contents are outside its integrity contract. Keep the special-node
    // attack in the canonical object store, at that actually consumed entry.
    if (kind === "ordinary FIFO") {
      await fs.rmdir(path.join(objects, "pack"));
      execFileSync("/usr/bin/mkfifo", [path.join(objects, "pack")], { stdio: "pipe" });
    }
    const sibling = path.join(f.request.outDir, "proof");
    let siblingStat: Awaited<ReturnType<typeof fs.lstat>> | undefined;
    let siblingAttempted = false;
    const occupiedSibling = kind === "existing sibling" || kind === "sibling symlink";
    if (occupiedSibling) {
      // The actual private stores are read/proof, replacing the retired -base
      // sibling. Insert foreign proof immediately after the importer creates its
      // parent, so the refusal exercises proof creation rather than root creation.
      const mkdir = fs.mkdir.bind(fs);
      t.mock.method(fs, "mkdir", async (...args: Parameters<typeof fs.mkdir>) => {
        if (args[0] === sibling) siblingAttempted = true;
        const result = await mkdir(...args);
        if (args[0] === f.request.outDir) {
          if (kind === "existing sibling") await mkdir(sibling);
          else await fs.symlink(sourceObjects, sibling);
          siblingStat = await fs.lstat(sibling);
        }
        return result;
      });
    }
    let controlOpened = false;
    f.hooks.open = (_file, p) => { if (p === control) controlOpened = true; };
    await assert.rejects(f.importTip({ original }), occupiedSibling ? { code: "EEXIST" } : layout);
    assert.equal(siblingAttempted, occupiedSibling, "occupied private sibling is actually checked");
    assert.equal(substituted, kind === "bad identity");
    assert.equal(controlOpened, false, "canonical alternate controls are refused without opening");
    if (controlBefore) {
      const after = await fs.lstat(control!, { bigint: true });
      assert.deepEqual([after.dev, after.ino, after.mode], [controlBefore.dev, controlBefore.ino, controlBefore.mode],
        "refusal retains the alternate control's inode and mode");
    }
    assert.equal(f.requests.length, 0, "no Git launched or partial source handed off");
    assert.equal(f.pairs(), 0);
    assert.equal(await bytesIdentity(sourceObjects), sourceBefore, "all source bytes and identities retained");
    if (siblingStat) {
      const now = await fs.lstat(sibling);
      assert.equal(now.ino, siblingStat.ino);
      assert.equal(now.mode, siblingStat.mode);
      assert.deepEqual(await fs.readdir(f.request.outDir), ["proof"]);
      if (kind === "sibling symlink") assert.equal(await fs.readlink(sibling), sourceObjects);
    } else await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    // Compare ref bytes and identities without asking Git to open the poisoned
    // object store. Stable relative labels also allow the deliberate bare rename.
    const retainedBare = kind === "basename" ? original.barePath : f.bare;
    assert.deepEqual(await refIdentity(retainedBare), canonicalRefs, "refusal preserves canonical refs");
    assert.equal(f.joined(), 0);
  });
}

for (const store of ["source", "base"]) for (const kind of ["mutation", "replacement", "root replacement"]) {
  test("ported snapshot: both-store final revalidation: " + store + " " + kind, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    const objects = path.join(store === "source" ? path.join(f.clone, ".git") : f.bare, "objects");
    // Excluded base contents no longer have a full-copy/hash guarantee. The
    // canonical control file is pinned and is the approved base-content analogue.
    const leaf = store === "source" ? path.join(objects, f.tip.slice(0, 2), f.tip.slice(2)) : path.join(f.bare, "config");
    if (store === "source" && kind !== "root replacement") await fs.chmod(leaf, 0o600);
    const controlBytes = store === "base" && kind !== "root replacement" ? await fs.readFile(leaf) : undefined;
    let changed = false;
    f.cache.afterPhase = async phase => {
      if (phase !== "walk") return;
      changed = true;
      if (kind === "root replacement") {
        await fs.rename(objects, objects + "-original");
        await fs.mkdir(path.join(objects, "pack"), { recursive: true, mode: 0o700 });
      } else {
        if (kind === "replacement") await fs.rename(leaf, path.join(f.dir, store + "-original-control"));
        await fs.writeFile(leaf, controlBytes ? Buffer.concat([controlBytes, Buffer.from("\n#changed control\n")]) : "changed source object");
      }
    };
    await assert.rejects(f.importTip(), layout);
    assert.equal(changed, true);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    await f.assertRetained();
    if (controlBytes) assert.deepEqual(await fs.readFile(leaf),
      Buffer.concat([controlBytes, Buffer.from("\n#changed control\n")]), "attacked control and original custody bytes retained");
  });
}

for (const resource of ["sourceBytes", "sourceEntries", "adminEntries", "metadataBytes", "artifactBytes"] as const) {
  test("ported snapshot: second store cannot renew shared " + resource + " and cleans both copies", snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    const limits = { sourceBytes: 256 * 1024 ** 2, sourceEntries: 65536, adminEntries: 4096,
      metadataBytes: 256 * 1024 ** 2, artifactBytes: 256 * 1024 ** 2 };
    let reached = false, exhausted = false;
    const consume = MaintenanceResources.consume.bind(MaintenanceResources);
    t.mock.method(MaintenanceResources, "consume", (r: MaintenanceResource, n: number) => {
      try { return consume(r, n); }
      catch (error) {
        if (r === resource && error instanceof Error && error.message === "maintenance_resource_exhausted") exhausted = true;
        throw error;
      }
    });
    f.cache.afterPhase = async phase => {
      if (phase !== "walk") return;
      reached = true;
      const used = f.charges.filter(c => c.resource === resource).reduce((n, c) => n + c.amount, 0);
      f.charge(resource, limits[resource] - used);
    };
    await assert.rejects(f.importTip(), /maintenance_(resource_exhausted|token_invalid)/);
    assert.equal(reached, true);
    assert.equal(exhausted, true, "the actual shared counter blocks continuation");
    assert.equal(f.charges.filter(c => c.resource === resource).reduce((n, c) => n + c.amount, 0), limits[resource]);
    assert.throws(() => f.charge(resource, 1), { message: "maintenance_token_invalid" });
    await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    await f.assertRetained();
  });
}

test("ported snapshot: a hostile declared alternate contributes no real Git objects", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), hostile = path.join(f.dir, "hostile.git");
  await f.native(f.dir, ["init", "--bare", "-q", hostile]);
  const body = path.join(f.dir, "hostile-body");
  await fs.writeFile(body, "hostile object must not be supplied\n");
  const oid = await f.native(hostile, ["hash-object", "-w", body]);
  await f.native(hostile, ["cat-file", "-e", oid]);
  const control = path.join(f.clone, ".git", "objects", "info", "alternates");
  await fs.writeFile(control, path.join(hostile, "objects") + "\n");
  let externalOpens = 0;
  f.hooks.open = (_file, p) => { if (p === hostile || p.startsWith(hostile + "/")) externalOpens++; };
  await assert.rejects(f.importTip(), layout);
  assert.equal(externalOpens, 0);
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
  await assert.rejects(f.native(f.bare, ["cat-file", "-e", oid]));
  assert.equal(await fs.readFile(control, "utf8"), path.join(hostile, "objects") + "\n");
  await f.assertRetained();
});

for (const kind of ["control mutation", "control replacement", "source control directory", "late base control"]) {
  test("ported snapshot: both stores retain alternate metadata consistency: " + kind, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t), control = path.join(f.clone, ".git", "objects", "info", "alternates");
    let changed = false;
    if (kind === "source control directory") { await fs.unlink(control); await fs.mkdir(control); }
    else f.cache.afterPhase = async phase => {
      if (phase !== "walk") return;
      changed = true;
      if (kind === "late base control") {
        await fs.mkdir(path.join(f.bare, "objects", "info"), { recursive: true });
        await fs.writeFile(path.join(f.bare, "objects", "info", "alternates"), "late untrusted alternate\n");
      } else {
        if (kind === "control replacement") await fs.rename(control, path.join(f.dir, "original-alternates"));
        await fs.writeFile(control, kind === "control replacement" ? path.join(f.bare, "objects") + "\n" : "changed\n");
      }
    };
    await assert.rejects(f.importTip(), layout);
    assert.equal(changed, kind !== "source control directory");
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    await f.assertRetained();
  });
}

for (const direction of ["read", "write"]) {
  test("ported snapshot: second-store pending " + direction + " settles before revocation cleanup", snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    let enter!: () => void, release!: () => void;
    const entered = new Promise<void>(resolve => { enter = resolve; });
    const held = new Promise<void>(resolve => { release = resolve; });
    t.after(release);
    let pending = false, settled = false, earlyCloses = 0, reached = false;
    f.hooks.open = (file, p) => {
      if (p !== (direction === "read" ? path.join(f.clone, ".git", "objects", "info", "alternates") :
        path.join(f.request.outDir, "read", "HEAD"))) return;
      const close = file.close.bind(file);
      t.mock.method(file, "close", async () => { if (pending) earlyCloses++; await close(); });
      if (direction === "read") {
        const read = file.read.bind(file);
        t.mock.method(file, "read", async (b: Buffer, o: number, n: number, at: number) => {
          reached = true; pending = true; enter(); await held;
          try { return await read(b, o, n, at); } finally { pending = false; }
        });
      } else {
        const write = file.write.bind(file);
        t.mock.method(file, "write", async (b: Buffer, o: number, n: number, at: number) => {
          reached = true; pending = true; enter(); await held;
          try { return await write(b, o, n, at); } finally { pending = false; }
        });
      }
    };
    const outcome = f.importTip().then(value => ({ value }), error => ({ error })).finally(() => { settled = true; });
    try {
      await Promise.race([entered, outcome.then(() => { throw new Error("missed actual pending I/O"); })]);
      f.controller.abort();
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(settled, false);
      assert.equal(earlyCloses, 0);
      if (direction === "write") assert.ok((await fs.lstat(f.request.outDir)).isDirectory(), "cleanup waits for pending write");
    } finally { release(); }
    const result = await outcome;
    assert.equal(reached, true);
    assert.ok("error" in result && result.error instanceof Error);
    assert.equal(earlyCloses, 0);
    await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.assertRetained();
  });
}

test("ported snapshot: both stores consume the original logical source quota", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t);
  f.charge("sources", 127);
  await assert.rejects(f.importTip(), /maintenance_(resource_exhausted|token_invalid)/);
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
  await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
  await f.assertRetained();
});

test("ported snapshot: derived base sibling is bounded before concatenation", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t);
  const outDir = portLongPath(f.cache.recoveryRoot, 4090);
  await fs.mkdir(path.dirname(outDir), { recursive: true, mode: 0o700 });
  const mkdir = fs.mkdir.bind(fs);
  let readCreated = false, overlong = false;
  t.mock.method(fs, "mkdir", async (...args: Parameters<typeof fs.mkdir>) => {
    overlong ||= Buffer.byteLength(String(args[0])) > 4096;
    const result = await mkdir(...args);
    if (args[0] === path.join(outDir, "read")) readCreated = true;
    return result;
  });
  await assert.rejects(f.importTip({ outDir }), layout);
  assert.equal(readCreated, true, "bounded first control directory was created");
  assert.equal(overlong, false, "longer derived private-control path refused before creation");
  assert.equal(f.requests.length, 0);
  await assert.rejects(fs.lstat(outDir), { code: "ENOENT" });
  await f.assertRetained();
});

test("ported snapshot: detaches actual loose objects, pack/index and nested encoded bytes without source writes or launches", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), objects = path.join(f.clone, ".git", "objects");
  await f.native(f.clone, ["repack", "-a", "-d", "-q"]);
  const body = path.join(f.dir, "unreferenced-body");
  await fs.writeFile(body, "actual loose encoded object\n");
  const oid = await f.native(f.clone, ["hash-object", "-w", body]);
  assert.ok((await fs.lstat(path.join(objects, oid.slice(0, 2), oid.slice(2)))).isFile());
  const packNames = await fs.readdir(path.join(objects, "pack"));
  assert.ok(packNames.some(n => n.endsWith(".pack")) && packNames.some(n => n.endsWith(".idx")));
  const graphs = path.join(objects, "info", "commit-graphs");
  await fs.mkdir(graphs, { recursive: true });
  await fs.writeFile(path.join(graphs, "graph-" + "a".repeat(40) + ".graph"), Buffer.from([0, 255, 1]));
  await fs.writeFile(path.join(objects, "info", "multi-pack-index"), Buffer.from([0, 255, 2]));
  const before = new Map<string, { bytes: Buffer; stat: import("node:fs").BigIntStats }>();
  const inventory = async (p: string): Promise<void> => {
    for (const ent of await fs.readdir(p, { withFileTypes: true })) {
      const name = path.join(p, ent.name);
      if (ent.isDirectory()) await inventory(name);
      else before.set(name, { bytes: await fs.readFile(name), stat: await fs.lstat(name, { bigint: true }) });
    }
  };
  await inventory(objects);
  let metadataReads = 0;
  f.hooks.open = (file, p) => {
    if (p.startsWith(graphs + "/") || p.endsWith("/multi-pack-index"))
      t.mock.method(file, "read", async () => { metadataReads++; throw new Error("acceleration metadata must not be read"); });
  };
  await f.walk();
  assert.equal(metadataReads, 0);
  for (const [name, entry] of before) {
    const now = await fs.lstat(name, { bigint: true });
    assert.deepEqual(await fs.readFile(name), entry.bytes);
    assert.equal(now.ino, entry.stat.ino);
    assert.equal(now.mtimeNs, entry.stat.mtimeNs);
    assert.equal(now.ctimeNs, entry.stat.ctimeNs);
    assert.equal(now.nlink, entry.stat.nlink);
  }
  await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" }, "in-place walk creates no copied store");
  const unknown = path.join(objects, "nested");
  await fs.mkdir(unknown); await fs.writeFile(path.join(unknown, "bytes"), Buffer.from([0, 255, 3]));
  await assert.rejects(f.walk(), layout);
  assert.deepEqual(await fs.readFile(path.join(unknown, "bytes")), Buffer.from([0, 255, 3]), "unknown nested bytes retained on refusal");
  await f.assertRetained();
});

test("ported snapshot: accepts bounded caller-family paths beyond canonical two-level shape", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), refs = await f.refs();
  const deep = path.join(f.dir, "runner", "family", "generation", "attempt");
  await fs.mkdir(path.dirname(deep), { recursive: true, mode: 0o700 });
  await fs.rename(f.clone, deep);
  const source = await sourceAt(f, deep);
  // Encoded-layout admission only. The later canonical family metadata reader
  // still independently qualifies actual production paths.
  await f.walk({ source });
  assert.equal((await fs.lstat(deep)).isDirectory(), true);
  assert.equal(await f.refs(), refs);
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
});

for (const kind of ["wrong identity", "rounded identity", "relative source", "outside source", "unnormalized source",
  "relative output", "outside output", "root output", "existing output", "unsafe name"]) {
  test("ported snapshot: refuses " + kind, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t), source = { ...f.source };
    let outDir = f.request.outDir;
    if (kind === "wrong identity") source.identity = "0:0";
    if (kind === "rounded identity") source.identity += "\n";
    if (kind === "relative source") source.clonePath = "runner/repo/branch";
    if (kind === "outside source") source.clonePath = f.dir;
    if (kind === "unnormalized source") source.clonePath += "/../branch";
    if (kind === "relative output") outDir = "snapshot";
    if (kind === "outside output") outDir = path.join(f.dir, "foreign");
    if (kind === "root output") outDir = f.cache.recoveryRoot;
    if (kind === "existing output") await fs.mkdir(outDir, { mode: 0o700 });
    if (kind === "unsafe name") await fs.writeFile(path.join(f.clone, ".git", "objects", "bad\\name"), "retained hostile name");
    await assert.rejects(f.importTip({ source, outDir }), kind === "existing output" ? { code: "EEXIST" } : layout);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    if (kind === "existing output") assert.deepEqual(await fs.readdir(outDir), []);
    await f.assertRetained();
  });
}

for (const kind of ["source ancestor", "clone", ".git", "objects", "leaf", "target parent", "hardlink", "FIFO", "raw invalid UTF8"]) {
  test("ported snapshot: rejects " + kind, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t), refs = await f.refs();
    const objects = path.join(f.clone, ".git", "objects");
    const leaf = path.join(objects, f.tip.slice(0, 2), f.tip.slice(2));
    const originalBytes = await fs.readFile(leaf);
    let originalLeaf = leaf;
    const linkPath = kind === "source ancestor" ? path.dirname(f.clone) : kind === "clone" ? f.clone :
      kind === ".git" ? path.join(f.clone, ".git") : kind === "objects" ? objects :
      kind === "target parent" ? f.cache.recoveryRoot : leaf;
    if (kind === "hardlink") await fs.link(leaf, path.join(f.dir, "source-alias"));
    else if (kind === "FIFO") {
      originalLeaf = path.join(f.dir, "original-loose-object");
      await fs.rename(leaf, originalLeaf);
      execFileSync("/usr/bin/mkfifo", [leaf], { stdio: "pipe" });
    } else if (kind === "raw invalid UTF8") {
      await fs.writeFile(Buffer.concat([Buffer.from(objects + "/"), Buffer.from([255])]), "retained raw bytes");
    } else {
      await fs.rename(linkPath, linkPath + "-real");
      await fs.symlink(linkPath + "-real", linkPath);
    }
    await assert.rejects(f.importTip(), kind === "hardlink" ? { message: "maintenance_source_alias_refused" } : layout);
    assert.deepEqual(await fs.readFile(originalLeaf), originalBytes, "source bytes retained through original or explicitly retained fixture name");
    assert.equal(await f.refs(), refs);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    assert.match(await f.native(f.bare, ["config", "--get", "uzi-recovery.feature.clone"]), /"runId":"run"/,
      "canonical custody descriptor retained");
    if (kind === "hardlink") assert.equal((await fs.lstat(leaf)).nlink, 2);
    if (kind === "clone") assert.equal((await fs.lstat(f.clone + "-real")).isDirectory(), true);
    else assert.equal((await fs.lstat(f.clone)).isDirectory(), true);
  });
}

for (const control of ["alternates", "http-alternates"]) {
  for (const kind of ["empty", "link", "directory", "late insertion", "handoff insertion"]) {
    test("ported snapshot: " + control + " refuses " + kind + " without reading control bytes", snapshotLinux, async t => {
      const f = await snapshotPortFixture(t), info = path.join(f.clone, ".git", "objects", "info");
      const name = path.join(info, control);
      await fs.unlink(path.join(info, "alternates"));
      if (kind === "empty") await fs.writeFile(name, "");
      if (kind === "link") await fs.symlink(path.join(f.bare, "objects"), name);
      if (kind === "directory") await fs.mkdir(name);
      let reads = 0, inserted = false, atInvalidation = -1;
      f.hooks.open = (file, p) => {
        if (p === name) t.mock.method(file, "read", async () => { reads++; throw new Error("control bytes must not be read"); });
      };
      if (kind.includes("insertion")) f.cache.afterPhase = async phase => {
        if (phase !== (kind === "late insertion" ? "walk" : "proof")) return;
        await fs.writeFile(name, ""); inserted = true; atInvalidation = f.requests.length;
      };
      await assert.rejects(f.importTip(), layout);
      assert.equal(reads, 0);
      assert.equal(inserted, kind.includes("insertion"));
      if (inserted) {
        assert.equal(f.requests.length, atInvalidation, "no later Git after completed-phase insertion");
        assert.equal(await fs.readFile(name, "utf8"), "");
      }
      if (kind === "handoff insertion") assert.ok(f.requests.some(r => r.argv.includes("cat-file") && r.argv.includes("--batch")),
        "late handoff injection follows actual selected-body proof");
      assert.equal(f.requests.some(r => r.argv.includes("index-pack") && r.argv.includes("--stdin")), false);
      assert.equal(f.pairs(), 0);
      await f.assertRetained();
    });
  }
}

test("ported snapshot: no producer token or discovery scope can authorize a snapshot", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t);
  await assert.rejects(f.cache.importMaintenanceTip(f.request), { message: "maintenance_token_invalid" });
  await assert.rejects(MaintenanceResources.runDiscovery(() => f.cache.importMaintenanceTip(f.request)),
    { message: "maintenance_token_invalid" });
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
  await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
  await f.assertRetained();
});

for (const kind of ["mutation", "replacement", "late earlier leaf", "late directory replacement", "growth"]) {
  test("ported snapshot: rejects changed source: " + kind, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t), info = path.join(f.clone, ".git", "objects", "info");
    const control = path.join(info, "alternates"), original = await fs.readFile(control);
    let changed = false;
    if (kind.startsWith("late")) f.cache.afterPhase = async phase => {
      if (phase !== "walk") return;
      changed = true;
      if (kind === "late directory replacement") {
        await fs.rename(info, path.join(f.dir, "original-info"));
        await fs.mkdir(info); await fs.writeFile(control, original);
      } else await fs.writeFile(control, Buffer.alloc(original.length, 0x78));
    };
    else f.hooks.open = (file, p) => {
      if (p !== control) return;
      const read = file.read.bind(file);
      t.mock.method(file, "read", async (b: Buffer, o: number, n: number, at: number) => {
        const result = await read(b, o, n, at);
        if (!changed) {
          changed = true;
          if (kind === "replacement") {
            await fs.rename(control, path.join(f.dir, "original-alternates"));
            await fs.writeFile(control, original);
          } else await fs.writeFile(control, kind === "growth" ? Buffer.concat([original, Buffer.from("growth")]) :
            Buffer.alloc(original.length, 0x78));
        }
        return result;
      });
    };
    await assert.rejects(f.importTip(), layout);
    assert.equal(changed, true);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    await f.assertRetained();
  });
}

for (const resource of ["sourceBytes", "sourceEntries", "adminEntries", "metadataBytes", "artifactBytes"] as const) {
  test("ported snapshot: original shared quota precharges " + resource, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    const limits = { sourceBytes: 256 * 1024 ** 2, sourceEntries: 65536, adminEntries: 4096,
      metadataBytes: 256 * 1024 ** 2, artifactBytes: 256 * 1024 ** 2 };
    f.charge(resource, limits[resource] - (resource === "artifactBytes" ? 2 : 0));
    let writes = 0, privateFileOpens = 0;
    f.hooks.open = (file, p) => {
      if (!p.startsWith(f.request.outDir + "/")) return;
      privateFileOpens++;
      t.mock.method(file, "write", async () => { writes++; throw new Error("unreserved private write"); });
    };
    await assert.rejects(f.importTip(), /maintenance_(resource_exhausted|token_invalid)/);
    assert.equal(writes, 0);
    assert.equal(privateFileOpens, 0, "known control capacity reserved before file open");
    assert.equal(f.requests.length, 0);
    await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    await f.assertRetained();
  });
}

function portOpaquePack() {
  const header = Buffer.alloc(12); header.write("PACK"); header.writeUInt32BE(2, 4);
  return [header, Buffer.from("abc"), Buffer.alloc(20)];
}
// Encoded transport only; this deliberately opaque PACK payload is never indexed
// or authenticated and cannot yield an imported tip or any durable authority.
function portStage(f: Fixture, out = path.join(f.cache.recoveryRoot, "ported.pack"), before = f.request.prepare) {
  const stdout = Readable.from(portOpaquePack());
  return f.inScope(() => f.cache["stageMaintenancePack"]({ stdout, exited: Promise.resolve({ code: 0, stderr: "" }),
    abort: () => { stdout.destroy(); } }, out, 35, before, undefined, { disposable: true }));
}

test("ported snapshot: a different output path cannot renew the original artifact quota", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), out = path.join(f.cache.recoveryRoot, "ported.pack");
  f.charge("artifactBytes", 256 * 1024 ** 2 - 35);
  const result = await portStage(f, out);
  assert.equal(result.byteSize, 35);
  await assert.rejects(portStage(f, out + "-second"), /maintenance_(resource_exhausted|token_invalid)/);
  assert.deepEqual(await fs.readFile(out), Buffer.concat(portOpaquePack()));
  await assert.rejects(fs.lstat(out + "-second"), { code: "ENOENT" });
  assert.equal(f.requests.length, 0);
  await f.assertRetained();
});

test("ported snapshot: source DAC refusal never repairs permissions or launches a privileged reader", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), control = path.join(f.clone, ".git", "objects", "info", "alternates");
  const original = await fs.readFile(control), open = fs.open.bind(fs), chmod = fs.chmod.bind(fs);
  let permissionChanges = 0, refusedRead = false;
  t.mock.method(fs, "chmod", async (...args: Parameters<typeof fs.chmod>) => { permissionChanges++; return chmod(...args); });
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    if (f.physical(args[0]) === control) {
      refusedRead = true; throw Object.assign(new Error("fixture DAC refusal"), { code: "EACCES" });
    }
    return open(...args);
  });
  await assert.rejects(f.importTip(), { code: "EACCES" });
  assert.equal(refusedRead, true);
  assert.equal(permissionChanges, 0);
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
  assert.deepEqual(await fs.readFile(control), original);
  await f.assertRetained();
});

test("ported snapshot: source request accounting includes raw names, EOF and short read capacity", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), objects = path.join(f.clone, ".git", "objects");
  const info = path.join(objects, "info");
  const control = path.join(info, "alternates"), size = (await fs.stat(control)).size;
  let encodedSizeSum = 0;
  const directories = [objects];
  for (const directory of directories) {
    for (const name of await fs.readdir(directory)) {
      const entry = path.join(directory, name), stat = await fs.lstat(entry);
      if (stat.isDirectory()) directories.push(entry);
      else {
        assert.equal(stat.isFile(), true, "fixture object store contains only directories and regular files");
        encodedSizeSum += stat.size;
      }
    }
  }
  const capacities: number[] = [];
  let rawReads = 0, allRawReads = 0;
  const opendir = fs.opendir.bind(fs);
  t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    const dir = await opendir(...args);
    const physical = f.physical(args[0]);
    if (physical === objects || physical.startsWith(objects + path.sep)) {
      const read = dir.read.bind(dir);
      t.mock.method(dir, "read", async () => {
        allRawReads++;
        if (physical === info) rawReads++;
        return read();
      });
    }
    return dir;
  });
  f.hooks.open = (file, p) => {
    if (p !== control) return;
    const read = file.read.bind(file);
    t.mock.method(file, "read", async (b: Buffer, o: number, n: number, at: number) => {
      capacities.push(n); return read(b, o, Math.min(n, 1), at);
    });
  };
  await f.walk();
  assert.equal(rawReads, 2, "one info entry plus EOF");
  assert.deepEqual(capacities, Array.from({ length: size + 1 }, (_, i) => size + 1 - i));
  assert.deepEqual(capacities.slice(-4), [4, 3, 2, 1]);
  const used = f.charges.filter(c => c.resource === "sourceBytes").reduce((n, c) => n + c.amount, 0);
  assert.ok(used >= capacities.reduce((n, v) => n + v, 0) + rawReads * 255 + size,
    "read capacity, raw-name requests and conservative st_size are distinct charges");
  assert.equal(used, encodedSizeSum + allRawReads * 255 + capacities.reduce((n, v) => n + v, 0),
    "every encoded file size, raw-name request including EOF and actual read capacity is charged exactly");
  f.charge("sourceBytes", 256 * 1024 ** 2 - used);
  assert.throws(() => f.charge("sourceBytes", 1), { message: "maintenance_resource_exhausted" });
  await f.assertRetained();
});

test("ported snapshot: partial writes retry only unwritten tail and charge the chunk once", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), out = path.join(f.cache.recoveryRoot, "ported.pack");
  const attempts: Array<[number, number, number]> = [];
  const categories: Array<{ admin: number; source: number; previousAdmin: number; previousSource: number }> = [];
  let lastAdmin = 0, lastSource = 0, syncs = 0;
  f.hooks.open = (file, p) => {
    if (p !== out) return;
    t.mock.method(file, "sync", async () => { syncs++; });
    const write = file.write.bind(file);
    t.mock.method(file, "write", async (b: Buffer, o: number, n: number, at: number) => {
      if (b.toString() !== "abc") return write(b, o, n, at);
      const admin = f.charges.filter(c => c.resource === "adminEntries").reduce((sum, c) => sum + c.amount, 0);
      const source = f.charges.filter(c => c.resource === "sourceEntries").reduce((sum, c) => sum + c.amount, 0);
      categories.push({ admin, source, previousAdmin: lastAdmin, previousSource: lastSource });
      lastAdmin = admin; lastSource = source;
      attempts.push([o, n, at - 12]);
      return write(b, o, 1, at);
    });
  };
  await portStage(f, out);
  assert.equal(categories.length, 3);
  for (const { admin, source, previousAdmin, previousSource } of categories) {
    assert.equal(admin, previousAdmin, "disposable parent/file validation consumes no creation allowance");
    assert.ok(source > previousSource, "each retry charges active parent/file validation");
  }
  assert.deepEqual(attempts, [[0, 3, 0], [1, 2, 1], [2, 1, 2]]);
  assert.deepEqual(await fs.readFile(out), Buffer.concat(portOpaquePack()));
  assert.equal(f.charges.filter(c => c.resource === "artifactBytes").reduce((sum, c) => sum + c.amount, 0), 35);
  assert.equal(syncs, 0, "disposable scratch needs no fsync under U2");
  assert.equal(f.requests.length, 0);
  await f.assertRetained();
});

for (const kind of ["zero write", "read fault", "write fault", "read cancel", "write cancel"]) {
  test("ported snapshot: settles serial filesystem I/O and cleans owned output: " + kind, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    let enter!: () => void, release!: () => void;
    const entered = new Promise<void>(resolve => { enter = resolve; }), held = new Promise<void>(resolve => { release = resolve; });
    t.after(release);
    let reached = false, pending = false, settled = false, earlyCloses = 0, earlyCleanup = 0;
    const unlink = fs.unlink.bind(fs), rmdir = fs.rmdir.bind(fs);
    t.mock.method(fs, "unlink", async (...args: Parameters<typeof fs.unlink>) => {
      if (pending && String(args[0]).startsWith(f.request.outDir)) earlyCleanup++;
      return unlink(...args);
    });
    t.mock.method(fs, "rmdir", async (...args: Parameters<typeof fs.rmdir>) => {
      if (pending && String(args[0]).startsWith(f.request.outDir)) earlyCleanup++;
      return rmdir(...args);
    });
    f.hooks.open = (file, p) => {
      const reading = kind.startsWith("read");
      if (p !== (reading ? path.join(f.clone, ".git", "objects", "info", "alternates") : path.join(f.request.outDir, "read", "HEAD"))) return;
      const close = file.close.bind(file);
      t.mock.method(file, "close", async () => { if (pending) earlyCloses++; await close(); });
      if (reading) {
        const read = file.read.bind(file);
        t.mock.method(file, "read", async (b: Buffer, o: number, n: number, at: number) => {
          reached = true; pending = true; enter(); await held;
          try { if (kind === "read fault") throw new Error("private read fault"); return await read(b, o, n, at); }
          finally { pending = false; }
        });
      } else {
        const write = file.write.bind(file);
        t.mock.method(file, "write", async (b: Buffer, o: number, n: number, at: number) => {
          reached = true; pending = true; enter(); await held;
          try {
            if (kind === "zero write") return { bytesWritten: 0, buffer: b };
            if (kind === "write fault") throw new Error("private write fault");
            return await write(b, o, n, at);
          } finally { pending = false; }
        });
      }
    };
    const outcome = f.importTip().then(value => ({ value }), error => ({ error })).finally(() => { settled = true; });
    try {
      await Promise.race([entered, outcome.then(() => { throw new Error("missed held I/O"); })]);
      if (kind.endsWith("cancel")) f.controller.abort();
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(settled, false);
      assert.equal(earlyCloses, 0);
      assert.equal(earlyCleanup, 0);
    } finally { release(); }
    const result = await outcome;
    assert.equal(reached, true);
    assert.ok("error" in result && result.error instanceof Error);
    assert.equal(earlyCloses, 0);
    assert.equal(earlyCleanup, 0);
    await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    assert.equal(f.requests.length, 0);
    await f.assertRetained();
  });
}

test("ported snapshot: cleanup preserves a foreign replacement name", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), target = path.join(f.request.outDir, "read", "HEAD");
  let changed = false;
  f.hooks.open = (file, p) => {
    if (p !== target || changed) return;
    t.mock.method(file, "write", async () => {
      changed = true;
      await fs.rename(target, target + "-owned-renamed");
      await fs.writeFile(target, "foreign bytes");
      throw new Error("private write failure");
    });
  };
  await assert.rejects(f.importTip());
  assert.equal(changed, true);
  assert.equal(await fs.readFile(target, "utf8"), "foreign bytes");
  assert.ok((await fs.lstat(target + "-owned-renamed")).isFile());
  assert.equal(f.requests.length, 0);
  await f.assertRetained();
});

function modeledSnapshotOwnership(t: test.TestContext, f: Fixture, profile: { split: boolean; mode: number; gid?: number }) {
  const previous = process.env.UZI_UID_SPLIT;
  if (profile.split) process.env.UZI_UID_SPLIT = "1"; else delete process.env.UZI_UID_SPLIT;
  t.after(() => { if (previous === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = previous; });
  t.mock.method(process as NodeJS.Process & { getuid: () => number }, "getuid", () => 10001);
  t.mock.method(process as NodeJS.Process & { getgid: () => number }, "getgid", () => 10001);
  const project = <T extends { uid: bigint | number; gid: bigint | number; mode: bigint | number }>(p: string, s: T) => {
    if (p !== f.dir && !p.startsWith(f.dir + "/")) return s;
    const big = typeof s.uid === "bigint";
    const source = p === f.clone || p.startsWith(f.clone + "/");
    const uid = source && profile.split ? 10002 : 10001;
    s.uid = big ? BigInt(uid) : uid;
    s.gid = big ? BigInt(uid) : uid;
    if (p === f.dir) {
      s.mode = typeof s.mode === "bigint" ? (s.mode & ~0o7777n) | BigInt(profile.mode) : (s.mode & ~0o7777) | profile.mode;
      s.gid = big ? BigInt(profile.gid ?? 10001) : profile.gid ?? 10001;
    }
    return s;
  };
  const lstat = fs.lstat.bind(fs);
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => project(f.physical(args[0]), await lstat(...args)));
  f.hooks.open = (file, p) => {
    const stat = file.stat.bind(file);
    t.mock.method(file, "stat", async (...args: Parameters<typeof file.stat>) => project(p, (await stat(...args))!));
  };
}

test("ported snapshot: modeled supported split-worker 2775 strict ancestor keeps private output", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t);
  // Explicit UID/GID/stat model, not live DAC proof. Source is runner10002,
  // canonical/private files worker10001, and only the strict ancestor is2775.
  modeledSnapshotOwnership(t, f, { split: true, mode: 0o2775, gid: 10001 });
  let observed = false;
  f.cache.afterPhase = async phase => {
    if (phase !== "proof") return;
    observed = true;
    assert.equal((await fs.lstat(f.request.outDir)).mode & 0o5777, 0o700);
    assert.equal((await fs.lstat(path.join(f.request.outDir, "read", "HEAD"))).mode & 0o7777, 0o600);
  };
  assert.deepEqual(await f.importTip(), { kind: "imported", sourceSha: f.tip });
  assert.equal(observed, true);
  await f.assertRetained();
});

for (const profile of [{ split: false, mode: 0o2775 }, { split: true, mode: 0o2775, gid: 10002 },
  { split: true, mode: 0o2777 }]) {
  test("ported snapshot: refuses unsupported modeled parent profile " + JSON.stringify(profile), snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    modeledSnapshotOwnership(t, f, profile);
    await assert.rejects(f.importTip(), layout);
    await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.assertRetained();
  });
}

test("ported snapshot: refuses a source info change during the last control probe", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t), info = path.join(f.clone, ".git", "objects", "info");
  const lstat = fs.lstat.bind(fs);
  let probes = 0;
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    try { return await lstat(...args); }
    catch (error) {
      if (f.physical(args[0]) === path.join(info, "http-alternates") && ++probes === 2)
        await fs.writeFile(path.join(info, "late"), "retained source");
      throw error;
    }
  });
  await assert.rejects(f.importTip(), layout);
  assert.equal(probes, 2, "actual final walk-control probe reached");
  assert.equal(await fs.readFile(path.join(info, "late"), "utf8"), "retained source");
  await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
  assert.equal(f.requests.length, 0);
  await f.assertRetained();
});

test("ported snapshot: new private directories retain harmless inherited setgid", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t);
  await fs.chmod(f.cache.recoveryRoot, 0o2700);
  let observed = false;
  f.cache.afterPhase = async phase => {
    if (phase !== "proof") return;
    observed = true;
    assert.equal((await fs.lstat(f.request.outDir)).mode & 0o7777, 0o2700);
    assert.equal((await fs.lstat(path.join(f.request.outDir, "read", "HEAD"))).mode & 0o7777, 0o600);
  };
  assert.deepEqual(await f.importTip(), { kind: "imported", sourceSha: f.tip });
  assert.equal(observed, true);
  await f.assertRetained();
});

for (const mode of [0o4700, 0o1700]) {
  test("ported snapshot: refuses unexpected bits on a newly created directory: " + mode.toString(8), snapshotLinux, async t => {
    const f = await snapshotPortFixture(t), mkdir = fs.mkdir.bind(fs);
    let changed = false;
    t.mock.method(fs, "mkdir", async (...args: Parameters<typeof fs.mkdir>) => {
      const result = await mkdir(...args);
      if (args[0] === f.request.outDir) { changed = true; await fs.chmod(f.request.outDir, mode); }
      return result;
    });
    await assert.rejects(f.importTip(), layout);
    assert.equal(changed, true);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.assertRetained();
  });
}

test("ported snapshot: private root never inherits the group-writable ancestor exception", snapshotLinux, async t => {
  const f = await snapshotPortFixture(t);
  await fs.chmod(f.cache.recoveryRoot, 0o770);
  await assert.rejects(f.importTip(), layout);
  assert.equal(f.requests.length, 0);
  await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
  await f.assertRetained();
});

for (const message of ["maintenance_resource_exhausted", "maintenance_token_invalid", "maintenance_clock_invalid",
  "maintenance_resource_exhausted private", "private diagnostic"]) {
  test("ported snapshot: fixed refusal strips cause and properties: " + message, snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    await assert.rejects(portStage(f, undefined, async () => {
      throw Object.assign(new Error(message, { cause: "private" }), { stderr: "private" });
    }), error => {
      assert.ok(error instanceof Error);
      assert.equal(error.message, ["maintenance_resource_exhausted", "maintenance_token_invalid", "maintenance_clock_invalid"].includes(message) ?
        message : "maintenance_pack_staging_refused");
      assert.equal(error.cause, undefined);
      assert.deepEqual(Object.keys(error), []);
      return true;
    });
    assert.equal(f.requests.length, 0);
    await f.assertRetained();
  });
}

for (const target of ["active source", "active destination", "source ancestor", "destination ancestor"]) {
  test("ported snapshot: short write revalidates " + target + " before retry", snapshotLinux, async t => {
    const f = await snapshotPortFixture(t);
    let writes = 0, atInvalidation = -1;
    f.hooks.open = (file, p) => {
      if (p !== path.join(f.request.outDir, "proof", "objects", "pack", "stage.pack")) return;
      const write = file.write.bind(file);
      t.mock.method(file, "write", async (b: Buffer, o: number, n: number, at: number) => {
        writes++;
        const result = await write(b, o, Math.min(1, n), at);
        const directory = target === "active source" ? path.join(f.clone, ".git", "objects") :
          target === "active destination" ? path.dirname(p) : target === "source ancestor" ? path.join(f.clone, ".git") : f.cache.recoveryRoot;
        await fs.chmod(directory, 0o750);
        atInvalidation = f.requests.length;
        return result;
      });
    };
    await assert.rejects(f.importTip());
    assert.equal(writes, 1, "pin invalidation prevents the second admitted write");
    assert.equal(f.requests.length, atInvalidation, "no later Git after invalidation");
    assert.equal(f.pairs(), 0);
    await f.assertRetained();
  });
}
