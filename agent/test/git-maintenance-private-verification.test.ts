import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createHash } from "node:crypto";
import { maintenanceDeltaFixture, canonicalImport, gitVerb } from "./maintenance-delta-fixture.js";
import { type MaintenanceResource } from "../src/recovery-maintenance.js";
import { RecoveryClosureLimitError } from "../src/recovery-closure.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";

const LINUX_ONLY = process.platform !== "linux";
const tick = () => new Promise<void>(resolve => setImmediate(resolve));
const latch = () => {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
};
async function fingerprint(root: string): Promise<string> {
  const hash = createHash("sha256");
  const walk = async (p: string): Promise<void> => {
    const stat = await fs.lstat(p);
    hash.update(p + ":" + stat.mode + ":" + stat.ino + ":");
    if (stat.isDirectory()) {
      for (const name of (await fs.readdir(p)).sort()) await walk(path.join(p, name));
    } else hash.update(await fs.readFile(p));
  };
  await walk(root);
  return hash.digest("hex");
}
type Borrowed = {
  pack: { path: string; byteSize: number; objectCount: number };
  index: { path: string; byteSize: number; objectCount: number };
  sourceSha: string;
};
// This adapter observes only the test phase; importMaintenanceTip still returns
// solely kind/sourceSha. Borrowed files have no lifetime beyond the awaited callback.
async function fixture(t: test.TestContext, decodedLimit?: number) {
  const f = await maintenanceDeltaFixture(t, { decodedLimit });
  const oids = (await f.native(f.clone, ["rev-list", "--objects", "--no-object-names", f.tip, "--not", f.base])).split("\n");
  let total = 0;
  for (const oid of oids) total += Number(await f.native(f.clone, ["cat-file", "-s", oid]));
  const poison = "[include\npoisoned and never parsed\n";
  await fs.writeFile(path.join(f.clone, ".git", "config"), poison);
  const original = await fingerprint(path.join(f.clone, ".git"));
  const canonicalConfig = await fs.readFile(path.join(f.bare, "config"), "utf8");
  const canonicalOriginal = await fingerprint(f.bare);
  let callbacks = 0, effects = 0;
  const effectsHook: { effect?: () => Promise<void> } = {};
  f.hooks.prepare = async () => { effects++; await effectsHook.effect?.(); };
  const seam = (consumer?: (result: Borrowed) => Promise<void>) => {
    f.cache.afterPhase = async phase => {
      if (phase !== "proof") return;
      const packDir = path.join(f.request.outDir, "proof", "objects", "pack");
      const names = await fs.readdir(packDir);
      const packPath = path.join(packDir, names.find(n => n.endsWith(".pack"))!);
      const indexPath = path.join(packDir, names.find(n => n.endsWith(".idx"))!);
      callbacks++;
      await consumer?.({
        pack: { path: packPath, byteSize: (await fs.stat(packPath)).size, objectCount: oids.length },
        index: { path: indexPath, byteSize: (await fs.stat(indexPath)).size, objectCount: oids.length },
        sourceSha: f.tip,
      });
    };
    return f.cache.importMaintenanceTip(f.request);
  };
  const unchanged = async (canonical = true) => {
    assert.equal(await fingerprint(path.join(f.clone, ".git")), original, "source bytes and identity retained");
    assert.equal(await fs.readFile(path.join(f.bare, "config"), "utf8"), canonicalConfig, "canonical config remains valid and unchanged");
    if (canonical) assert.equal(await fingerprint(f.bare), canonicalOriginal, "no canonical effects before import");
    assert.deepEqual(await fs.readdir(f.cache.recoveryRoot), [], "all private artifacts disposed");
    assert.equal(f.joined(), f.requests.length, "all owned children joined");
  };
  return { ...f, git: f.cache, total, oids, poison, original, canonicalConfig, seam, unchanged,
    effectsHook, callbacks: () => callbacks, effects: () => effects };
}

test("ported private: private verification fixture clears inherited setgid before creating descendants", { skip: LINUX_ONLY }, async t => {
  // Redirect the fixture's tmp root (os.tmpdir() reads TMPDIR per call) to a real setgid parent.
  const parent = await fs.mkdtemp(path.join(os.tmpdir(), "private-verification-setgid-"));
  const oldTmp = process.env.TMPDIR;
  t.after(async () => {
    if (oldTmp === undefined) delete process.env.TMPDIR; else process.env.TMPDIR = oldTmp;
    await fs.rm(parent, { recursive: true, force: true });
  });
  await fs.chmod(parent, 0o2775);
  assert.equal((await fs.stat(parent)).mode & 0o2000, 0o2000);
  process.env.TMPDIR = parent;
  const f = await maintenanceDeltaFixture(t);
  assert.equal(path.dirname(f.dir), parent, "the fixture directory is created under the setgid parent");
  assert.equal((await fs.stat(f.dir)).mode & 0o2000, 0, "fixture root must clear inherited setgid");
  assert.equal((await fs.stat(f.bare)).mode & 0o2000, 0, "fixture descendants must not inherit setgid");
  assert.equal((await fs.stat(f.clone)).mode & 0o2000, 0, "fixture descendants must not inherit setgid");
});

test("ported private: private maintenance refuses genuine capture with valid resources before snapshot effects", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t);
  const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(2852)),
    async () => { throw Error("unexpected generic launch"); }, undefined, undefined,
    async () => { throw Error("unexpected capture launch"); });
  let reads = 0;
  const lstat = fs.lstat.bind(fs), open = fs.open.bind(fs);
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => { reads++; return lstat(...args); });
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => { reads++; return open(...args); });
  try {
    const result = await f.inCaptureScope(() => safety.withCredentialFreeCapture(10000, context =>
      f.git.withCredentialFreeCapture(context, async () => {
        const before = reads;
        await assert.rejects(f.seam(), { message: "capture private verification refused" });
        assert.equal(reads, before, "capture refusal precedes all filesystem reads");
        assert.equal(f.effects(), 0);
        assert.equal(f.requests.length, 0);
        assert.equal(f.callbacks(), 0);
      })));
    assert.equal(result.kind, "observed_empty");
  } finally {
    assert.equal((await safety.dispose({ boundary: "checkpoint", deadlineMs: 1000 })).kind, "disposed");
  }
  await f.unchanged();
});

test("ported private: real shared-base private proof survives unavailable originals and ignores poisoned config", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t);
  f.hooks.beforeSpawn = async request => {
    assert.equal(request.argv[0], "/usr/bin/git");
    if (!request.cwd.startsWith(f.request.outDir)) return;
    assert.equal(request.env.GIT_CONFIG_GLOBAL, "/dev/null");
    assert.equal(request.env.GIT_ALLOW_PROTOCOL, "none");
    assert.equal(request.env.GIT_NO_LAZY_FETCH, "1");
    assert.equal(request.env.GIT_CONFIG_COUNT, undefined);
    if (request.cwd.endsWith("/proof")) {
      assert.equal(JSON.parse(request.env.GIT_ALTERNATE_OBJECT_DIRECTORIES!), path.join(f.bare, "objects"));
      assert.equal(request.env.GIT_OBJECT_DIRECTORY, path.join(request.cwd, "objects"));
    }
  };
  const result = await f.inScope(() => f.seam(async borrowed => {
    assert.equal(f.joined(), f.requests.length);
    assert.equal(borrowed.sourceSha, f.tip);
    assert.equal(borrowed.pack.objectCount, f.oids.length);
    assert.equal((await fs.readFile(borrowed.pack.path)).subarray(0, 4).toString(), "PACK");
    assert.ok(borrowed.index.byteSize > 0);
    await tick();
  }));
  assert.deepEqual(result, { kind: "imported", sourceSha: f.tip });
  assert.equal(f.callbacks(), 1);
  assert.equal(await f.native(f.bare, ["show", f.tip + ":new"]), "new selected blob");
  assert.equal(await f.native(f.bare, ["show", f.base + ":base"]), "canonical prerequisite");
  const pack = f.requests.find(r => gitVerb(r) === "pack-objects")!;
  for (const flag of ["--no-reuse-object", "--no-reuse-delta", "--window=0", "--depth=0", "--threads=1"])
    assert.ok(pack.argv.includes(flag));
  await f.unchanged(false);

  // Approved replacement: staged proof cannot be rescued by the clone, and the
  // canonical prerequisite must remain pinned through proof and import.
  for (const lost of ["clone", "canonical", "canonical replacement"] as const) {
    const g = await fixture(t);
    const target = lost === "clone" ? path.join(g.clone, ".git", "objects") : path.join(g.bare, "objects");
    const offline = target + ".offline";
    let changed = false;
    g.cache.beforePhase = async phase => {
      if (phase !== "proof") return;
      await fs.rename(target, offline);
      if (lost === "canonical replacement") await fs.mkdir(target);
      changed = true;
    };
    try {
      await assert.rejects(g.inScope(() => g.seam()));
      assert.equal(changed, true);
      assert.equal(g.callbacks(), 0);
      assert.equal(g.requests.filter(canonicalImport).length, 0);
    } finally {
      if (lost === "canonical replacement") await fs.rmdir(target);
      await fs.rename(offline, target);
    }
    await g.unchanged();
  }
  const g = await fixture(t);
  await fs.writeFile(path.join(g.bare, "config"), g.poison);
  try {
    await assert.rejects(g.inScope(() => g.seam()), { message: "maintenance_source_layout_unsupported" });
    assert.equal(g.callbacks(), 0, "canonical poisoned config cannot qualify admission");
    assert.equal(g.requests.filter(canonicalImport).length, 0);
  } finally { await fs.writeFile(path.join(g.bare, "config"), g.canonicalConfig); }
  await g.unchanged();
});

for (const kind of ["missing", "corrupt"] as const) {
  test("ported private: pack-only proof refuses " + kind + " before consumer", { skip: LINUX_ONLY }, async t => {
    const f = await fixture(t); let changed = false;
    f.cache.beforePhase = async phase => {
      if (phase !== "proof") return;
      const packs = path.join(f.request.outDir, "proof", "objects", "pack");
      const pack = path.join(packs, (await fs.readdir(packs)).find(n => n.endsWith(".pack"))!);
      if (kind === "missing") await fs.unlink(pack);
      else {
        const file = await fs.open(pack, "r+");
        try { await file.write(Buffer.from("BROKEN"), 0, 6, 12); } finally { await file.close(); }
      }
      changed = true;
    };
    await assert.rejects(f.inScope(() => f.seam()));
    assert.equal(changed, true);
    assert.equal(f.callbacks(), 0);
    assert.equal(f.requests.filter(canonicalImport).length, 0);
    await f.unchanged();
  });
}

test("ported private: genuine below-C limit refuses before decoded producer or consumer", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t, 1);
  assert.ok(f.total > 1);
  await assert.rejects(f.inScope(() => f.seam()), RecoveryClosureLimitError);
  assert.equal(f.requests.some(r => r.argv.includes("--batch")), false);
  assert.equal(f.callbacks(), 0);
  await f.unchanged();
});

test("ported private: ordinary and nested private verification share cumulative original C", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t, 300);
  assert.ok(f.total < 300);
  await f.inScope(async () => {
    assert.equal(await f.git.verifyRecoveryClosure(f.bare, f.base), true);
    await assert.rejects(f.git.withRecoveryOperation(f.controller.signal, Date.now() + 100000,
      () => f.seam()), RecoveryClosureLimitError);
  });
  assert.equal(f.callbacks(), 0);
  await f.unchanged();
});

test("ported private: all pipeline work remains in the original charged reservation", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t);
  await f.inScope(() => f.seam());
  const used = new Map<MaintenanceResource, number>();
  for (const charge of f.charges) used.set(charge.resource, (used.get(charge.resource) ?? 0) + charge.amount);
  for (const resource of ["decoded", "closureEntries", "sourceBytes", "sourceEntries", "artifactBytes",
    "metadataBytes", "adminEntries", "sources", "launches"] as const) assert.ok((used.get(resource) ?? 0) > 0, resource);
  assert.equal(used.get("decoded"), f.total, "only selected delta bodies count toward C");
  assert.equal(used.get("rpcAttempts"), 2, "one import pair; no per-file RPC");
  assert.equal(f.pairs(), 1);
  await f.retained();
  f.charge("launches", 512 - used.get("launches")!);
  assert.throws(() => f.charge("launches", 1), { message: "maintenance_resource_exhausted" });
  await f.unchanged(false);
});

for (const [resource, limit] of Object.entries({
  decoded: 8 * 1024 ** 3, closureEntries: 65536, sourceBytes: 256 * 1024 ** 2,
  sourceEntries: 65536, artifactBytes: 256 * 1024 ** 2, metadataBytes: 256 * 1024 ** 2,
  adminEntries: 4096, sources: 128, launches: 512,
})) {
  test("ported private: private pipeline refuses exhausted original " + resource, { skip: LINUX_ONLY }, async t => {
    const f = await fixture(t);
    f.charge(resource as MaintenanceResource, limit);
    await assert.rejects(f.inScope(() => f.seam()));
    assert.equal(f.callbacks(), 0);
    assert.equal(f.requests.filter(canonicalImport).length, 0);
    await f.unchanged();
  });
}

test("ported private: fresh callback is awaited again at a later helper launch", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t);
  f.effectsHook.effect = async () => { if (f.requests.length === 1) throw Error("fresh custody lost"); };
  await assert.rejects(f.inScope(() => f.seam()));
  assert.equal(f.requests.length, 1);
  assert.equal(f.joined(), 1);
  assert.equal(f.callbacks(), 0);
  await f.unchanged();
});

test("ported private: consumer rejection is awaited, cleaned and propagated without touching originals", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t), entered = latch(), release = latch();
  let settled = false;
  const work = f.inScope(() => f.seam(async () => { entered.release(); await release.promise; throw Error("consumer"); }))
    .finally(() => { settled = true; });
  void work.catch(() => undefined);
  try {
    await entered.promise; await tick(); assert.equal(settled, false);
    assert.ok((await fs.readdir(path.join(f.request.outDir, "proof", "objects", "pack"))).length === 2);
  } finally { release.release(); }
  await assert.rejects(work, { message: "consumer" });
  assert.equal(f.callbacks(), 1);
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  await f.unchanged();
});

test("ported private: cancellation during consumer waits for callback settlement", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t), entered = latch(), release = latch();
  let settled = false;
  const work = f.inScope(() => f.seam(async () => { entered.release(); await release.promise; }))
    .finally(() => { settled = true; });
  void work.catch(() => undefined);
  try {
    await entered.promise; f.controller.abort(Error("cancel consumer"));
    await tick(); assert.equal(settled, false);
    assert.equal((await fs.readdir(path.join(f.request.outDir, "proof", "objects", "pack"))).length, 2);
  } finally { release.release(); }
  await assert.rejects(work);
  assert.equal(f.callbacks(), 1);
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  await f.unchanged();
});

test("ported private: custody callback refusal before helper launch never calls consumer", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t);
  f.effectsHook.effect = async () => { throw Error("custody revoked"); };
  await assert.rejects(f.inScope(() => f.seam()), { message: "custody revoked" });
  assert.equal(f.requests.length, 0); assert.equal(f.callbacks(), 0);
  await f.unchanged();
});

test("ported private: owned completion and raw stderr disposal both settle before continuation", { skip: LINUX_ONLY }, async t => {
  const f = await fixture(t), completed = latch(), release = latch(), disposal = latch();
  let settled = false, exitBeforeClose = false, cleanupBeforeClose = false;
  let held = false, observedExit = false, observedCleanup = false;
  f.hooks.beforeSpawn = async request => {
    if (gitVerb(request) !== "pack-objects") return;
    held = true;
    f.hooks.holdStderr = true;
    f.hooks.completed = async () => { completed.release(); await release.promise; };
  };
  f.hooks.stderrDestroy = (error, callback) => { void disposal.promise.then(() => callback(error)); };
  const spawnGit = f.git["spawnGit"].bind(f.git);
  t.mock.method(f.git as unknown as { spawnGit: typeof spawnGit }, "spawnGit",
    async (...args: Parameters<typeof spawnGit>) => {
      const child = await spawnGit(...args);
      return { ...child, exited: child.exited.then(code => {
        if (held) {
          observedExit = true;
          exitBeforeClose ||= !f.hooks.stderr!.closed;
        }
        return code;
      }) };
    });
  const rmdir = fs.rmdir.bind(fs);
  t.mock.method(fs, "rmdir", async (...args: Parameters<typeof fs.rmdir>) => {
    if (String(args[0]).startsWith(f.git.recoveryRoot)) {
      observedCleanup = true;
      cleanupBeforeClose ||= !f.hooks.stderr!.closed;
    }
    return rmdir(...args);
  });
  const work = f.inScope(() => f.seam()).finally(() => { settled = true; });
  void work.catch(() => undefined);
  try {
    await completed.promise;
    f.controller.abort(Error("cancel child"));
    await tick();
    assert.equal(settled, false, "actual child completion is still owned");
    assert.equal(f.hooks.stderr!.destroyed, true, "cancellation must request raw stderr disposal");
    assert.equal(f.hooks.stderr!.closed, false, "raw asynchronous disposal is deliberately held");
    release.release();
    await tick();
    assert.equal(exitBeforeClose, false, "exit must join actual raw stderr closure");
    assert.equal(settled, false, "raw asynchronous disposal is still owned");
  } finally {
    release.release(); disposal.release();
    f.hooks.stderr?.destroy();
    await assert.rejects(work);
  }
  assert.equal(held, true);
  assert.equal(observedExit, true, "actual spawnGit exit was observed");
  assert.equal(observedCleanup, true, "actual cleanup was observed");
  assert.equal(f.hooks.stderr!.closed, true);
  assert.equal(cleanupBeforeClose, false, "cleanup must follow actual raw stderr closure");
  assert.equal(f.callbacks(), 0); assert.ok(f.cancelled() > 0);
  await f.unchanged();
});

for (const failure of ["write failure", "cancel", "callback rejection"]) {
  test("ported private: composed snapshot sole cleanup follows all handle closes: " + failure, { skip: LINUX_ONLY }, async t => {
    const f = await fixture(t), closing = latch(), release = latch();
    const removals = new Map<string, number>();
    let opened = 0, closed = 0, injected = false, outputReady = false, earlyCleanup = false;
    let heldCloseSettled = false, phaseSettled = false, phaseBeforeClose = false;
    f.cache.afterPhase = async phase => { if (phase === "pack") phaseSettled = true; };
    const record = (p: unknown) => {
      const name = f.physical(p);
      if (!name.startsWith(f.request.outDir + "/") && name !== f.request.outDir) return;
      earlyCleanup ||= closed !== opened;
      phaseBeforeClose ||= !heldCloseSettled;
      removals.set(name, (removals.get(name) ?? 0) + 1);
    };
    const unlink = fs.unlink.bind(fs), rmdir = fs.rmdir.bind(fs);
    t.mock.method(fs, "unlink", async (...args: Parameters<typeof fs.unlink>) => { record(args[0]); return unlink(...args); });
    t.mock.method(fs, "rmdir", async (...args: Parameters<typeof fs.rmdir>) => { record(args[0]); return rmdir(...args); });
    f.hooks.open = (file, p) => {
      opened++;
      const close = file.close.bind(file);
      t.mock.method(file, "close", async () => {
        if (p === path.join(f.clone, ".git", "objects")) { closing.release(); await release.promise; }
        await close(); closed++;
        if (p === path.join(f.clone, ".git", "objects")) heldCloseSettled = true;
      });
      if (p !== path.join(f.request.outDir, "proof", "objects", "pack", "stage.pack")) return;
      outputReady = true;
      const write = file.write.bind(file);
      t.mock.method(file, "write", async (...args: Parameters<typeof file.write>) => {
        if (!injected && failure !== "callback rejection") {
          injected = true;
          if (failure === "cancel") f.controller.abort(Error("staging cancel"));
          throw Error("staging write failure");
        }
        return write(...args);
      });
    };
    f.effectsHook.effect = async () => {
      if (outputReady && !injected && failure === "callback rejection") {
        injected = true; throw Error("staging callback rejection");
      }
    };
    let settled = false;
    const work = f.inScope(() => f.seam()).finally(() => { settled = true; });
    void work.catch(() => undefined);
    try {
      await closing.promise;
      // Bounded observation while the source pin close remains deliberately held.
      for (let i = 0; i < 64; i++) await tick();
      assert.equal(injected, true);
      assert.equal(removals.size, 0, "sole cleanup must await held descriptor close");
      assert.equal(settled, false, "importer must await held descriptor close");
      assert.equal(phaseSettled, false, "failed staging cannot publish a completed pack");
    } finally {
      release.release();
      await assert.rejects(work);
    }
    assert.equal(phaseBeforeClose, false, "cleanup follows held pin close");
    assert.equal(earlyCleanup, false, "every opened handle closes before any removal");
    assert.equal(opened, closed);
    assert.ok(removals.size > 0);
    assert.ok([...removals.values()].every(count => count === 1), "one removal attempt per owned name");
    assert.equal(f.callbacks(), 0);
    assert.equal(f.requests.filter(canonicalImport).length, 0);
    assert.ok(f.requests.some(r => gitVerb(r) === "pack-objects"), "real producer was exercised");
    await f.unchanged();
  });
}

for (const mutation of ["earlier source sibling", "earlier output sibling", "source control", "output control"]) {
  test("ported private: snapshot final proof blocks every Git launch after " + mutation, { skip: LINUX_ONLY }, async t => {
    const f = await fixture(t);
    const objects = path.join(f.clone, ".git", "objects");
    const originals = new Map<string, Buffer>();
    const modes = new Map<string, number>();
    let previous: string | undefined, changed = false, launchesAtChange = -1;
    f.hooks.open = (file, p) => {
      if (mutation === "earlier source sibling") {
        if (!p.startsWith(objects + "/") || !/\/[0-9a-f]{2}\/[0-9a-f]{38}$/.test(p)) return;
        const close = file.close.bind(file);
        t.mock.method(file, "close", async () => {
          await close();
          if (!changed && previous && previous !== p) {
            originals.set(previous, await fs.readFile(previous));
            modes.set(previous, (await fs.stat(previous)).mode & 0o7777);
            await fs.chmod(previous, 0o600);
            await fs.appendFile(previous, "changed");
            changed = true; launchesAtChange = f.requests.length;
          }
          previous = p;
        });
        return;
      }
      if (!p.startsWith(f.request.outDir + "/")) return;
      const write = file.write.bind(file);
      t.mock.method(file, "write", async (...args: Parameters<typeof file.write>) => {
        const result = await write(...args);
        if (!changed && previous && previous !== p &&
            (mutation !== "output control" || p === path.join(f.request.outDir, "proof", "HEAD"))) {
          if (mutation === "earlier output sibling") await fs.appendFile(previous, "changed");
          else if (mutation === "source control") {
            const control = path.join(objects, "info", "alternates");
            originals.set(control, await fs.readFile(control));
            await fs.appendFile(control, "changed");
          } else {
            // The earlier view config must remain pinned while creating the next view.
            await fs.appendFile(path.join(f.request.outDir, "read", "config"), "changed");
          }
          changed = true; launchesAtChange = f.requests.length;
        }
        previous = p;
        return result;
      });
    };
    try {
      await assert.rejects(f.inScope(() => f.seam()));
      assert.equal(changed, true, "intended sibling/control change happened");
      assert.equal(launchesAtChange, 0, "change precedes every Git launch");
      assert.equal(f.requests.length, launchesAtChange, "final proof blocks every later Git launch");
      assert.equal(f.callbacks(), 0);
    } finally {
      for (const [p, bytes] of originals) {
        await fs.writeFile(p, bytes);
        if (modes.has(p)) await fs.chmod(p, modes.get(p)!);
      }
    }
    await f.unchanged();
  });
}

// Exercise both directory orders rather than depending on filesystem enumeration.
for (const order of ["info first", "info last"] as const) {
  test("ported private: snapshot final proof refuses a source control change with " + order + " in the listing", { skip: LINUX_ONLY }, async t => {
    const f = await fixture(t);
    const objects = path.join(f.clone, ".git", "objects");
    const control = path.join(objects, "info", "alternates"), original = await fs.readFile(control);
    const opendir = fs.opendir.bind(fs);
    let rootOrder: string[] = [], previous: string | undefined, changed = false;
    t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
      const dir = await opendir(...args);
      const entries: Array<{ name: unknown }> = [];
      for (let ent = await dir.read(); ent; ent = await dir.read()) entries.push(ent as { name: unknown });
      const info = (e: { name: unknown }) => String(e.name) === "info";
      entries.sort((a, b) => order === "info first" ? Number(info(b)) - Number(info(a)) : Number(info(a)) - Number(info(b)));
      if (f.physical(args[0]) === objects) rootOrder = entries.map(e => String(e.name));
      return { read: async () => entries.shift() ?? null, close: () => dir.close() } as unknown as Awaited<ReturnType<typeof fs.opendir>>;
    });
    f.hooks.open = (file, p) => {
      if (!p.startsWith(objects + "/") || !/\/[0-9a-f]{2}\/[0-9a-f]{38}$/.test(p)) return;
      const close = file.close.bind(file);
      t.mock.method(file, "close", async () => {
        await close();
        if (!changed && previous && previous !== p) {
          await fs.appendFile(control, "changed");
          changed = true;
        }
        previous = p;
      });
    };
    try {
      await assert.rejects(f.inScope(() => f.seam()), { message: "maintenance_source_layout_unsupported" });
      assert.equal(changed, true, "source control change occurred between real source leaf visits");
      assert.ok(rootOrder.length > 2);
      assert.equal(order === "info first" ? rootOrder[0] : rootOrder.at(-1), "info");
      assert.equal(f.requests.length, 0, "no rev-list, pack producer or verifier launch");
      assert.equal(f.callbacks(), 0);
    } finally { await fs.writeFile(control, original); }
    await f.unchanged();
  });
}
