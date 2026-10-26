import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";
import {
  acquireWorkerLifetime, AuthorityTransaction, WorkerLifetime, withAuthorityTransaction,
} from "../src/authority-lock.js";
import { makeTmp, spawnWorkerLockHolder, testLockOptions, utilLinux } from "./authority-test-support.js";

const authorityDir = (root: string): string => path.join(root, ".uzi-authority");
const lockFile = (root: string): string => path.join(authorityDir(root), "worker.lock");

test("the production flock trust check accepts only util-linux", async (t) => {
  const root = makeTmp(t);
  const attempt = acquireWorkerLifetime(root, { supportedFs: async () => true });
  if (utilLinux) await attempt;
  else await assert.rejects(attempt, /util-linux/);
});

test("W: a second process refuses while the first holds it; SIGKILL releases it in the kernel", async (t) => {
  const root = makeTmp(t);
  const holder = spawnWorkerLockHolder(root);
  t.after(() => { holder.child.kill("SIGKILL"); });
  assert.equal(await holder.first, "READY");
  await assert.rejects(acquireWorkerLifetime(root, testLockOptions()), /already holds the worker lifetime lock/);
  holder.child.kill("SIGKILL");
  await holder.exited;
  const lifetime = await acquireWorkerLifetime(root, testLockOptions());
  assert.equal(lifetime.dataDir, root);
  const second = spawnWorkerLockHolder(root);
  t.after(() => { second.child.kill("SIGKILL"); });
  assert.match(await second.first, /^REFUSED:.*already holds/);
});

test("W: contention in the same process also refuses immediately", async (t) => {
  const root = makeTmp(t);
  await acquireWorkerLifetime(root, testLockOptions());
  await assert.rejects(acquireWorkerLifetime(root, testLockOptions()), /already holds/);
});

test("W: replacing the named lock inode during acquisition refuses", async (t) => {
  const root = makeTmp(t);
  const base = testLockOptions();
  await assert.rejects(acquireWorkerLifetime(root, {
    ...base,
    run: async (args, fd, signal, timeout) => {
      if (args[0] !== "--version") {
        fs.renameSync(lockFile(root), lockFile(root) + ".old");
        fs.writeFileSync(lockFile(root), "", { mode: 0o600 });
      }
      return base.run ? base.run(args, fd, signal, timeout) : realRun(args, fd);
    },
  }), /inode changed/);
});

function realRun(args: string[], fd: number | undefined): Promise<{ code: number; output: string }> {
  return new Promise((resolve, reject) => {
    const child = spawn("/usr/bin/flock", args, { stdio: ["ignore", "pipe", "pipe", ...(fd === undefined ? [] : [fd])] });
    let output = "";
    child.stdout!.on("data", (c: Buffer) => { output += c.toString(); });
    child.once("error", reject);
    child.once("close", (code) => resolve({ code: code ?? 2, output }));
  });
}

test("W: unsafe lock metadata refuses (mode, link count, symlink, directory mode)", async (t) => {
  const mode = makeTmp(t);
  fs.mkdirSync(authorityDir(mode), { mode: 0o700 });
  fs.writeFileSync(lockFile(mode), "", { mode: 0o600 });
  fs.chmodSync(lockFile(mode), 0o644);
  await assert.rejects(acquireWorkerLifetime(mode, testLockOptions()), /unsafe authority lock metadata/);

  const links = makeTmp(t);
  fs.mkdirSync(authorityDir(links), { mode: 0o700 });
  fs.writeFileSync(lockFile(links), "", { mode: 0o600 });
  fs.linkSync(lockFile(links), path.join(authorityDir(links), "other"));
  await assert.rejects(acquireWorkerLifetime(links, testLockOptions()), /unsafe authority lock metadata/);

  const symlink = makeTmp(t);
  fs.mkdirSync(authorityDir(symlink), { mode: 0o700 });
  fs.writeFileSync(path.join(symlink, "target"), "", { mode: 0o600 });
  fs.symlinkSync(path.join(symlink, "target"), lockFile(symlink));
  await assert.rejects(acquireWorkerLifetime(symlink, testLockOptions()), /ELOOP|symbolic/i);

  const dir = makeTmp(t);
  fs.mkdirSync(authorityDir(dir), { mode: 0o700 });
  fs.chmodSync(authorityDir(dir), 0o755);
  await assert.rejects(acquireWorkerLifetime(dir, testLockOptions()), /unsafe authority directory/);

  const foreign = makeTmp(t);
  await assert.rejects(acquireWorkerLifetime(foreign, testLockOptions({ uid: (process.getuid?.() ?? 0) + 1 })),
    /unsafe authority/);
});

test("W: missing, untrusted or unsupported infrastructure refuses startup", async (t) => {
  await assert.rejects(acquireWorkerLifetime(makeTmp(t), {
    supportedFs: async () => true, flockPath: "/nonexistent/flock",
  }), /util-linux flock required/);
  await assert.rejects(acquireWorkerLifetime(makeTmp(t), {
    supportedFs: async () => true, run: async () => ({ code: 0, output: "flock (BusyBox)\n" }),
  }), /util-linux flock required/);
  await assert.rejects(acquireWorkerLifetime(makeTmp(t), testLockOptions({ supportedFs: async () => false })),
    /ext4 or XFS/);
  await assert.rejects(acquireWorkerLifetime(makeTmp(t), testLockOptions({
    supportedFs: async () => { throw new Error("statfs failed"); },
  })), /unreadable/);
  await assert.rejects(acquireWorkerLifetime("relative/dir", testLockOptions()), /normalized absolute/);
});

test("W: a helper failure other than contention refuses", async (t) => {
  await assert.rejects(acquireWorkerLifetime(makeTmp(t), {
    supportedFs: async () => true,
    run: async (args) => (args[0] === "--version"
      ? { code: 0, output: "flock from util-linux 2.41\n" } : { code: 1, output: "" }),
  }), /flock failed: 1/);
});

test("capabilities cannot be forged or used after the fact", async (t) => {
  assert.throws(() => new WorkerLifetime(Symbol("x"), "/x"), /cannot be constructed/);
  assert.throws(() => new AuthorityTransaction(Symbol("x")), /cannot be constructed/);
  const forged = { dataDir: makeTmp(t) } as unknown as WorkerLifetime;
  await assert.rejects(withAuthorityTransaction(forged, {}, async () => 1), /live worker lifetime/);
});

async function lifetimeFor(t: Parameters<typeof makeTmp>[0]): Promise<{ root: string; lifetime: WorkerLifetime }> {
  const root = makeTmp(t);
  return { root, lifetime: await acquireWorkerLifetime(root, testLockOptions()) };
}

test("A: sibling callers in one process never overlap", async (t) => {
  const { lifetime } = await lifetimeFor(t);
  const order: string[] = [];
  let active = 0;
  let maxActive = 0;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  let aStarted!: () => void;
  const startedA = new Promise<void>((resolve) => { aStarted = resolve; });
  const body = (name: string, wait: Promise<void>) => async (): Promise<string> => {
    if (name === "a") aStarted();
    active++;
    maxActive = Math.max(maxActive, active);
    order.push(`${name}-start`);
    await wait;
    order.push(`${name}-end`);
    active--;
    return name;
  };
  const first = withAuthorityTransaction(lifetime, {}, body("a", gate));
  const second = withAuthorityTransaction(lifetime, {}, body("b", Promise.resolve()));
  await startedA;
  // Give an unserialized second caller every chance to start while the first is held.
  for (let i = 0; i < 20; i++) { await new Promise((r) => setImmediate(r)); await fsp.stat(lifetime.dataDir); }
  assert.deepEqual(order, ["a-start"]);
  release();
  assert.deepEqual(await Promise.all([first, second]), ["a", "b"]);
  assert.equal(maxActive, 1);
  assert.deepEqual(order, ["a-start", "a-end", "b-start", "b-end"]);
});

test("A: nested calls borrow; a nested acquisition and a stale token refuse", async (t) => {
  const { lifetime } = await lifetimeFor(t);
  let captured: AuthorityTransaction | undefined;
  const result = await withAuthorityTransaction(lifetime, {}, async (tx) => {
    captured = tx;
    const borrowed = await withAuthorityTransaction(tx, {}, async (inner) => inner === tx);
    await assert.rejects(withAuthorityTransaction(lifetime, {}, async () => 0), /must borrow/);
    return borrowed;
  });
  assert.equal(result, true);
  await assert.rejects(withAuthorityTransaction(captured!, {}, async () => 0), /active authority transaction/);
  // The lock was released even though the nested attempt threw inside the callback.
  assert.equal(await withAuthorityTransaction(lifetime, {}, async () => "again"), "again");
});

test("A: released only after the callback settles, even when it throws", async (t) => {
  const { lifetime } = await lifetimeFor(t);
  await assert.rejects(withAuthorityTransaction(lifetime, {}, async () => { throw new Error("boom"); }), /boom/);
  assert.equal(await withAuthorityTransaction(lifetime, { timeoutMs: 2000 }, async () => "ok"), "ok");
});

test("A: aborting a queued caller frees its slot without disturbing the holder", async (t) => {
  const { lifetime } = await lifetimeFor(t);
  const controller = new AbortController();
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  let started!: () => void;
  const running = new Promise<void>((resolve) => { started = resolve; });
  const holder = withAuthorityTransaction(lifetime, {}, async () => { started(); await gate; return "held"; });
  await running;
  const queued = withAuthorityTransaction(lifetime, { signal: controller.signal }, async () => "never");
  controller.abort(new Error("cancelled"));
  await assert.rejects(queued, /cancelled/);
  const after = withAuthorityTransaction(lifetime, {}, async () => "after");
  release();
  assert.equal(await holder, "held");
  assert.equal(await after, "after");
});

test("A: another process holding the authority lock blocks until a finite deadline, then releases", async (t) => {
  const { root, lifetime } = await lifetimeFor(t);
  await withAuthorityTransaction(lifetime, {}, async () => undefined); // creates authority.lock
  const external = spawn("/usr/bin/flock", ["-x", path.join(authorityDir(root), "authority.lock"), "sh", "-c", "echo READY; read x"],
    { stdio: ["pipe", "pipe", "inherit"] });
  t.after(() => { external.kill("SIGKILL"); });
  await new Promise<void>((resolve, reject) => {
    external.stdout.once("data", () => resolve());
    external.once("error", reject);
    external.once("close", () => reject(new Error("locker exited early")));
  });
  await assert.rejects(withAuthorityTransaction(lifetime, { timeoutMs: 250 }, async () => "no"), /timeout/);
  const blocked = withAuthorityTransaction(lifetime, { timeoutMs: 10_000 }, async () => "got it");
  external.stdin.end("go\n");
  assert.equal(await blocked, "got it");
});
