import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import { PassThrough } from "node:stream";
import { createHash } from "node:crypto";
import type { BigIntStats, Stats } from "node:fs";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { GitCache } from "../src/git.js";
import { MaintenanceResources, RecoveryMaintenance, type MaintenanceResource } from "../src/recovery-maintenance.js";
import { RecoveryClosureLimitError } from "../src/recovery-closure.js";
import { nullLogger } from "./helpers.js";

const exec = promisify(execFile);
const tick = () => new Promise<void>(resolve => setImmediate(resolve));
const latch = () => {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
};
async function fixture(t: test.TestContext, decoded?: number | ((total: number) => number)) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/private-verification-"));
  const native = async (cwd: string, args: string[]) => {
    const result = exec("/usr/bin/git", ["-C", cwd, ...args], {
    env: { PATH: "/usr/bin:/bin", HOME: dir, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_AUTHOR_NAME: "Fixture", GIT_AUTHOR_EMAIL: "fixture@example.com",
      GIT_COMMITTER_NAME: "Fixture", GIT_COMMITTER_EMAIL: "fixture@example.com" },
    });
    result.child.stdin?.end();
    return (await result).stdout.trim();
  };
  const seed = path.join(dir, "seed"), base = path.join(dir, "repos", "repo.git");
  const clone = path.join(dir, "runner", "repo", "attempt");
  await fs.mkdir(seed);
  await native(seed, ["init", "-q"]);
  await fs.writeFile(path.join(seed, "base"), "shared base body\n");
  await native(seed, ["add", "base"]); await native(seed, ["commit", "-qm", "base"]);
  await fs.mkdir(path.dirname(base), { recursive: true });
  await native(dir, ["clone", "--bare", "--no-hardlinks", "-q", seed, base]);
  await fs.mkdir(path.dirname(clone), { recursive: true });
  await native(dir, ["clone", "--shared", "-q", base, clone]);
  await fs.writeFile(path.join(clone, "extra"), "source body\n");
  await native(clone, ["add", "extra"]); await native(clone, ["commit", "-qm", "source"]);
  const tip = await native(clone, ["rev-parse", "HEAD"]);
  const oids = (await native(clone, ["rev-list", "--objects", "--no-object-names", tip])).split("\n");
  let total = 0;
  for (const oid of oids) total += Number(await native(clone, ["cat-file", "-s", oid]));
  const git = new GitCache(dir, nullLogger(), undefined, decoded === undefined ? {} :
    { recoveryDecodedLimitBytes: typeof decoded === "function" ? decoded(total) : decoded });
  const sourceStat = await fs.lstat(clone, { bigint: true }), baseStat = await fs.lstat(base, { bigint: true });
  const source = { clonePath: clone, identity: sourceStat.dev + ":" + sourceStat.ino };
  const knownBase = { barePath: base, identity: baseStat.dev + ":" + baseStat.ino };
  const poison = "[include\npoisoned and never parsed\n";
  await fs.writeFile(path.join(clone, ".git", "config"), poison);
  await fs.writeFile(path.join(base, "config"), poison);
  await fs.mkdir(git.recoveryRoot, { mode: 0o700 });
  const fingerprint = async () => {
    const hash = createHash("sha256");
    const walk = async (p: string): Promise<void> => {
      const stat = await fs.lstat(p);
      hash.update(p + ":" + stat.mode + ":" + stat.ino + ":");
      if (stat.isDirectory()) {
        for (const name of (await fs.readdir(p)).sort()) await walk(path.join(p, name));
      } else hash.update(await fs.readFile(p));
    };
    await walk(path.join(clone, ".git")); await walk(base);
    return hash.digest("hex");
  };
  const original = await fingerprint();

  // Project deployment ancestor permissions only. Objects, output identities, bytes,
  // modes, real Git and all child/drain settlement remain real.
  const ancestors = new Set<string>();
  for (let p = dir;; p = path.dirname(p)) {
    ancestors.add(p); if (path.dirname(p) === p) break;
  }
  const project = <T extends Stats | BigIntStats>(p: unknown, s: T): T => {
    if (typeof p === "string" && ancestors.has(p)) {
      const big = typeof s.mode === "bigint";
      Object.assign(s, { mode: big ? BigInt(Number(s.mode) & ~0o022) : Number(s.mode) & ~0o022,
        uid: big ? BigInt(process.getuid!()) : process.getuid!(),
        gid: big ? BigInt(process.getgid!()) : process.getgid!() });
    }
    return s;
  };
  const descriptors = new Map<number, string>();
  const physical = (p: unknown): string => {
    if (typeof p !== "string") return "";
    const match = /^\/proc\/self\/fd\/([0-9]+)(?:\/(.*))?$/.exec(p);
    return match && descriptors.has(Number(match[1]))
      ? path.join(descriptors.get(Number(match[1]))!, match[2] ?? "") : p;
  };
  const hooks: {
    open?: (file: Awaited<ReturnType<typeof fs.open>>, p: string) => Promise<void> | void;
    spawn?: (request: BoundaryProcessRequest) => Promise<void>;
    effect?: () => Promise<void>;
    completed?: () => Promise<void>;
    cancel?: () => Promise<void>;
    holdStderr?: boolean;
    stderrDestroy?: (error: Error | null, callback: (error?: Error | null) => void) => void;
    stderr?: PassThrough;
  } = {};
  const lstat = fs.lstat.bind(fs), open = fs.open.bind(fs);
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) =>
    project(physical(args[0]), await lstat(...args)));
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args), p = physical(args[0]);
    descriptors.set(file.fd, p);
    if (ancestors.has(p)) {
      const stat = file.stat.bind(file);
      t.mock.method(file, "stat", async (...params: Parameters<typeof file.stat>) =>
        project(p, (await stat(...params))!));
    }
    await hooks.open?.(file, p);
    return file;
  });

  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture" });
  const controller = new AbortController();
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  }, { signal: controller.signal });
  assert.ok(admitted.ok);
  const requests: BoundaryProcessRequest[] = [];
  let joined = 0, cancelled = 0, callbacks = 0, effects = 0;
  const ownedGit = async (request: BoundaryProcessRequest) => {
    requests.push(request);
    if (request.cwd.startsWith(git.recoveryRoot)) assert.ok(effects > 0);
    await hooks.spawn?.(request);
    const child = spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env });
    const completed = new Promise<{ code: number }>(resolve => {
      child.once("error", () => {});
      child.once("close", code => {
        void (async () => { await hooks.completed?.(); joined++; resolve({ code: code ?? -1 }); })();
      });
    });
    const stderr = hooks.holdStderr ? (hooks.stderr = new PassThrough({ destroy: hooks.stderrDestroy })) : child.stderr;
    if (hooks.stderr) child.stderr.pipe(hooks.stderr, { end: false });
    return { stdin: child.stdin, stdout: child.stdout, stderr, completed,
      cancel: async () => { cancelled++; child.kill("SIGKILL"); await hooks.cancel?.(); await completed; } };
  };
  const run = <T>(action: () => Promise<T>) => MaintenanceResources.run(admitted.value, () =>
    git.withBoundaryProcessSpawner(ownedGit, controller.signal, () =>
      git.withRecoveryOperation(controller.signal, Date.now() + 110000, action)));
  const before = async () => { effects++; await tick(); await hooks.effect?.(); };
  const seam = (name = "unit", consumer = async (_result: {
    pack: { path: string; byteSize: number; objectCount: number };
    index: { path: string; byteSize: number; objectCount: number }; sourceSha: string;
  }) => {}) => git["withVerifiedMaintenancePack"](source, tip, path.join(git.recoveryRoot, name),
    before, async result => { callbacks++; await consumer(result); }, knownBase);
  const charge = (resource: MaintenanceResource, amount: number) => MaintenanceResources.run(admitted.value,
    () => MaintenanceResources.consume(resource, amount));
  t.after(async () => {
    assert.equal(joined, requests.length, "every actual owned child must settle");
    await ledger.finish(admitted.value, "transient");
    await fs.rm(dir, { recursive: true, force: true });
  });
  return { dir, git, clone, base, seed, source, knownBase, tip, total, oids, poison, controller, requests, hooks,
    run, seam, charge, fingerprint, original, physical, callbacks: () => callbacks, joined: () => joined, cancelled: () => cancelled };
}

test("real shared-base private proof survives unavailable originals and ignores poisoned config", async t => {
  const f = await fixture(t);
  let detached = false;
  f.hooks.spawn = async request => {
    assert.equal(request.argv[0], "/usr/bin/git");
    assert.equal(request.env.GIT_CONFIG_GLOBAL, "/dev/null");
    assert.equal(request.env.GIT_ALLOW_PROTOCOL, "none");
    assert.equal(request.env.GIT_NO_LAZY_FETCH, "1");
    assert.equal(request.env.GIT_CONFIG_COUNT, undefined);
    if (!detached) {
      detached = true;
      await fs.rename(path.join(f.clone, ".git", "objects"), path.join(f.clone, ".git", "offline"));
      await fs.rename(path.join(f.base, "objects"), path.join(f.base, "offline"));
    }
    if (request.cwd.endsWith("/proof")) {
      assert.equal(request.env.GIT_ALTERNATE_OBJECT_DIRECTORIES, "");
      assert.equal(request.env.GIT_OBJECT_DIRECTORY, path.join(request.cwd, "objects"));
      assert.deepEqual(await fs.readdir(path.join(request.cwd, "objects")), ["pack"]);
      const names = await fs.readdir(path.join(request.cwd, "objects", "pack"));
      assert.equal(names.length, 2);
      assert.ok(names.every(name => /^pack-[0-9a-f]{40}\.(pack|idx)$/.test(name)));
    }
  };
  let used = false;
  await f.run(() => f.seam("unit", async result => {
    assert.equal(f.joined(), f.requests.length);
    assert.ok(f.requests.some(r => r.cwd.endsWith("/proof") && r.argv.includes("--batch")));
    assert.equal(result.sourceSha, f.tip);
    assert.equal(result.pack.objectCount, f.oids.length);
    assert.equal((await fs.readFile(result.pack.path)).subarray(0, 4).toString(), "PACK");
    used = true; await tick();
  }));
  assert.equal(used, true); assert.equal(f.callbacks(), 1);
  await fs.rename(path.join(f.clone, ".git", "offline"), path.join(f.clone, ".git", "objects"));
  await fs.rename(path.join(f.base, "offline"), path.join(f.base, "objects"));
  assert.equal(await f.fingerprint(), f.original);
  assert.equal(await fs.readFile(path.join(f.clone, ".git", "config"), "utf8"), f.poison);
  assert.equal(await fs.readFile(path.join(f.base, "config"), "utf8"), f.poison);
  assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
  const pack = f.requests.find(r => r.argv.includes("pack-objects"))!;
  for (const flag of ["--no-reuse-object", "--no-reuse-delta", "--window=0", "--depth=0", "--threads=1"])
    assert.ok(pack.argv.includes(flag));
});

for (const kind of ["missing", "corrupt"] as const) {
  test(`pack-only proof refuses ${kind} before consumer`, async t => {
    const f = await fixture(t); let changed = false;
    f.hooks.spawn = async request => {
      if (!changed && request.cwd.endsWith("/proof")) {
        changed = true;
        const packs = path.join(request.cwd, "objects", "pack");
        const pack = path.join(packs, (await fs.readdir(packs)).find(name => name.endsWith(".pack"))!);
        if (kind === "corrupt") {
          const file = await fs.open(pack, "r+");
          try { await file.write(Buffer.from("BROKEN"), 0, 6, 12); } finally { await file.close(); }
        } else await fs.unlink(pack);
        // Originals remain available; a missing pack may never use them as fallback.
        assert.equal(request.env.GIT_ALTERNATE_OBJECT_DIRECTORIES, "");
      }
    };
    await assert.rejects(f.run(() => f.seam()));
    assert.equal(changed, true); assert.equal(f.callbacks(), 0);
    assert.equal(await fs.readFile(path.join(f.base, "config"), "utf8"), f.poison);
  });
}

test("genuine below-C limit refuses before decoded producer or consumer", async t => {
  const f = await fixture(t, 1);
  assert.ok(f.total > 1);
  await assert.rejects(f.run(() => f.seam()), RecoveryClosureLimitError);
  assert.equal(f.requests.some(r => r.argv.includes("--batch")), false);
  assert.equal(f.callbacks(), 0);
});

test("ordinary and nested private verification share cumulative original C", async t => {
  const f = await fixture(t, total => total + 1);
  const baseTip = (await exec("/usr/bin/git", ["-C", f.seed, "rev-parse", "HEAD"])).stdout.trim();
  await f.run(async () => {
    assert.equal(await f.git.verifyRecoveryClosure(path.join(f.seed, ".git"), baseTip), true);
    await assert.rejects(f.git.withRecoveryOperation(f.controller.signal, Date.now() + 100000,
      () => f.seam()), RecoveryClosureLimitError);
  });
  assert.equal(f.callbacks(), 0);
});

test("all pipeline work remains in the original charged reservation", async t => {
  const f = await fixture(t);
  const used = new Map<MaintenanceResource, number>();
  const consume = MaintenanceResources.consume.bind(MaintenanceResources);
  t.mock.method(MaintenanceResources, "consume", (resource: MaintenanceResource, amount: number) => {
    used.set(resource, (used.get(resource) ?? 0) + amount); return consume(resource, amount);
  });
  await f.run(() => f.seam());
  for (const resource of ["decoded", "closureEntries", "sourceBytes", "sourceEntries", "artifactBytes",
    "metadataBytes", "adminEntries", "sources", "launches"] as const) assert.ok((used.get(resource) ?? 0) > 0, resource);
  t.diagnostic("actual private fixture resources " + JSON.stringify(Object.fromEntries(used)));
  assert.equal(used.get("decoded"), f.total);
  assert.equal(used.get("rpcAttempts") ?? 0, 0);
  assert.equal(await f.fingerprint(), f.original);
  const launchCount = used.get("launches")!;
  f.charge("launches", 512 - launchCount);
  assert.throws(() => f.charge("launches", 1), { message: "maintenance_resource_exhausted" });
});

for (const [resource, limit] of Object.entries({
  decoded: 8 * 1024 ** 3, closureEntries: 65536, sourceBytes: 256 * 1024 ** 2,
  sourceEntries: 65536, artifactBytes: 256 * 1024 ** 2, metadataBytes: 256 * 1024 ** 2,
  adminEntries: 4096, sources: 128, launches: 512,
})) {
  test("private pipeline refuses exhausted original " + resource, async t => {
    const f = await fixture(t);
    f.charge(resource as MaintenanceResource, limit);
    await assert.rejects(f.run(() => f.seam()));
    assert.equal(f.callbacks(), 0);
    assert.equal(await f.fingerprint(), f.original);
  });
}

test("fresh callback is awaited again at a later helper launch", async t => {
  const f = await fixture(t);
  f.hooks.effect = async () => { if (f.requests.length === 1) throw Error("fresh custody lost"); };
  await assert.rejects(f.run(() => f.seam()));
  assert.equal(f.requests.length, 1); assert.equal(f.joined(), 1); assert.equal(f.callbacks(), 0);
});

test("consumer rejection is awaited, cleaned and propagated without touching originals", async t => {
  const f = await fixture(t), entered = latch(), release = latch();
  let settled = false;
  const work = f.run(() => f.seam("unit", async () => { entered.release(); await release.promise; throw Error("consumer"); }))
    .finally(() => { settled = true; });
  await entered.promise; await tick(); assert.equal(settled, false);
  release.release(); await assert.rejects(work, { message: "consumer" });
  assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
  assert.equal(await fs.readFile(path.join(f.clone, ".git", "config"), "utf8"), f.poison);
});

test("cancellation during consumer waits for callback settlement", async t => {
  const f = await fixture(t), entered = latch(), release = latch();
  let settled = false;
  const work = f.run(() => f.seam("unit", async () => { entered.release(); await release.promise; }))
    .finally(() => { settled = true; });
  await entered.promise; f.controller.abort(Error("cancel consumer"));
  await tick(); assert.equal(settled, false);
  release.release(); await assert.rejects(work);
  assert.equal(f.callbacks(), 1); assert.equal(f.joined(), f.requests.length);
});

test("custody callback refusal before helper launch never calls consumer", async t => {
  const f = await fixture(t);
  f.hooks.effect = async () => { throw Error("custody revoked"); };
  await assert.rejects(f.run(() => f.seam()), { message: "custody revoked" });
  assert.equal(f.requests.length, 0); assert.equal(f.callbacks(), 0);
});

test("owned completion and raw stderr disposal both settle before continuation", async t => {
  const f = await fixture(t), completed = latch(), release = latch(), disposal = latch();
  let settled = false, exitBeforeClose = false, cleanupBeforeClose = false;
  f.hooks.holdStderr = true;
  f.hooks.stderrDestroy = (error, callback) => { void disposal.promise.then(() => callback(error)); };
  f.hooks.completed = async () => { completed.release(); await release.promise; };
  const spawnGit = f.git["spawnGit"].bind(f.git);
  t.mock.method(f.git as unknown as { spawnGit: typeof spawnGit }, "spawnGit",
    async (...args: Parameters<typeof spawnGit>) => {
      const child = await spawnGit(...args);
      return { ...child, exited: child.exited.then(code => {
        exitBeforeClose ||= !f.hooks.stderr!.closed && !f.hooks.stderr!.readableEnded;
        return code;
      }) };
    });
  const rmdir = fs.rmdir.bind(fs);
  t.mock.method(fs, "rmdir", async (...args: Parameters<typeof fs.rmdir>) => {
    if (String(args[0]).startsWith(f.git.recoveryRoot))
      cleanupBeforeClose ||= !f.hooks.stderr!.closed && !f.hooks.stderr!.readableEnded;
    return rmdir(...args);
  });
  const work = f.run(() => f.seam()).finally(() => { settled = true; });
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
  assert.equal(cleanupBeforeClose, false, "cleanup must follow actual raw stderr closure");
  assert.equal(f.callbacks(), 0); assert.ok(f.cancelled() > 0);
});

for (const failure of ["write failure", "cancel", "callback rejection"]) {
  test("composed snapshot sole cleanup follows all handle closes: " + failure, async t => {
    const f = await fixture(t), closing = latch(), release = latch();
    const removals = new Map<string, number>();
    let opened = 0, closed = 0, injected = false, outputReady = false, earlyCleanup = false;
    let heldCloseSettled = false, snapshotSettled = false, snapshotBeforeClose = false;
    const snapshot = f.git["snapshotMaintenanceObjects"].bind(f.git);
    t.mock.method(f.git as unknown as { snapshotMaintenanceObjects: typeof snapshot }, "snapshotMaintenanceObjects",
      async (...args: Parameters<typeof snapshot>) => {
        try { return await snapshot(...args); }
        finally { snapshotSettled = true; snapshotBeforeClose ||= !heldCloseSettled; }
      });
    const unlink = fs.unlink.bind(fs), rmdir = fs.rmdir.bind(fs);
    const record = (p: unknown) => {
      const name = f.physical(p);
      if (!name.startsWith(f.git.recoveryRoot + "/unit")) return;
      earlyCleanup ||= closed !== opened;
      removals.set(name, (removals.get(name) ?? 0) + 1);
    };
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
      if (p.startsWith(path.join(f.git.recoveryRoot, "unit", "source") + "/")) {
        const write = file.write.bind(file);
        t.mock.method(file, "write", async (...args: Parameters<typeof file.write>) => {
          if (!injected && failure !== "callback rejection") {
            injected = true;
            if (failure === "cancel") f.controller.abort(Error("snapshot cancel"));
            throw Error("snapshot write failure");
          }
          return write(...args);
        });
        outputReady = true;
      }
    };
    f.hooks.effect = async () => {
      if (outputReady && !injected && failure === "callback rejection") {
        injected = true; throw Error("snapshot callback rejection");
      }
    };
    const work = assert.rejects(f.run(() => f.seam()));
    try {
      await closing.promise;
      // Let the fixture progress for a bounded 64 event-loop turns while close stays held.
      for (let i = 0; i < 64; i++) await tick();
      assert.equal(removals.size, 0, "outer cleanup must await held descriptor close");
      assert.equal(snapshotSettled, false, "snapshot must await held descriptor close before returning");
    } finally {
      release.release();
      await work;
    }
    assert.equal(snapshotBeforeClose, false, "snapshot return must follow held descriptor close");
    assert.equal(earlyCleanup, false, "every snapshot handle closed before outer cleanup");
    assert.equal(injected, true);
    assert.ok(removals.size > 0);
    assert.ok([...removals.values()].every(count => count === 1), "one removal attempt per owned name");
    assert.deepEqual(await fs.readdir(f.git.recoveryRoot), []);
    assert.equal(f.requests.length, 0); assert.equal(f.callbacks(), 0);
  });
}

for (const mutation of ["earlier source sibling", "earlier output sibling", "source control", "output control"]) {
  test("snapshot final proof blocks every Git launch after " + mutation, async t => {
    const f = await fixture(t);
    let previous: string | undefined, changed = false;
    const objects = path.join(f.clone, ".git", "objects");
    f.hooks.open = (file, p) => {
      if (!p.startsWith(path.join(f.git.recoveryRoot, "unit", "source") + "/")) return;
      const write = file.write.bind(file);
      t.mock.method(file, "write", async (...args: Parameters<typeof file.write>) => {
        if (!changed && previous && previous !== p) {
          if (mutation === "earlier source sibling") {
            const source = path.join(objects, path.relative(path.join(f.git.recoveryRoot, "unit", "source"), previous));
            await fs.chmod(source, 0o600);
            await fs.appendFile(source, "changed");
          } else if (mutation === "earlier output sibling") {
            await fs.appendFile(previous, "changed");
          } else {
            const info = mutation === "source control" ? path.join(objects, "info") :
              path.join(f.git.recoveryRoot, "unit", "source", "info");
            await fs.mkdir(info, { recursive: true });
            await fs.writeFile(path.join(info, "alternates"), "");
          }
          changed = true;
        }
        previous = p;
        return write(...args);
      });
    };
    await assert.rejects(f.run(() => f.seam()));
    assert.equal(changed, true, "intended fixture mutation must complete before guard refusal");
    assert.equal(f.requests.length, 0, "no rev-list, pack producer or verifier launch");
    assert.equal(f.callbacks(), 0);
  });
}
