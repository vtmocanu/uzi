import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs, { constants as fsConstants } from "node:fs";
import fsp from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
import path from "node:path";
import { mock, test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import {
  acquireWorkerLifetime, AuthorityLockBusy, AuthorityRefusal, AuthorityTransaction, mkdirPrivate, WorkerLifetime, withAuthorityTransaction,
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

/** The typed acquisition timeout: still an AuthorityRefusal with the original message and name, so no log line changes. */
function isBusy(error: unknown): boolean {
  return error instanceof AuthorityLockBusy && error instanceof AuthorityRefusal &&
    error.message === "authority lock timeout" && error.name === "AuthorityRefusal";
}

test("A: a second caller behind a stuck in-process holder fails at its own deadline and the queue stays usable", { timeout: 30_000 }, async (t) => {
  const { lifetime } = await lifetimeFor(t);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  let started!: () => void;
  const running = new Promise<void>((resolve) => { started = resolve; });
  const holder = withAuthorityTransaction(lifetime, {}, async () => { started(); await gate; return "held"; });
  await running;
  const began = performance.now();
  await assert.rejects(withAuthorityTransaction(lifetime, { timeoutMs: 150 }, async () => "never"), isBusy);
  assert.ok(performance.now() - began < 10_000, "failed at the deadline, not after the holder");
  release();
  assert.equal(await holder, "held");
  assert.equal(await withAuthorityTransaction(lifetime, { timeoutMs: 2000 }, async () => "after"), "after");
});

test("A: only the two timeout sites are AuthorityLockBusy; a corrupt lock inode and an abort are not", async (t) => {
  const { root, lifetime } = await lifetimeFor(t);
  fs.renameSync(lockFile(root), lockFile(root) + ".old");
  fs.writeFileSync(lockFile(root), "", { mode: 0o600 });
  await assert.rejects(withAuthorityTransaction(lifetime, {}, async () => undefined),
    (error: unknown) => error instanceof AuthorityRefusal && !(error instanceof AuthorityLockBusy));
  const controller = new AbortController();
  controller.abort(new Error("cancelled"));
  await assert.rejects(withAuthorityTransaction(lifetime, { signal: controller.signal }, async () => undefined),
    (error: unknown) => !(error instanceof AuthorityLockBusy));
});

test("W: a worker.lock name that no longer matches the held inode refuses new transactions", async (t) => {
  const { root, lifetime } = await lifetimeFor(t);
  assert.equal(await withAuthorityTransaction(lifetime, {}, async () => "ok"), "ok");
  fs.renameSync(lockFile(root), lockFile(root) + ".old");
  fs.writeFileSync(lockFile(root), "", { mode: 0o600 });
  let ran = false;
  await assert.rejects(withAuthorityTransaction(lifetime, {}, async () => { ran = true; }), /inode changed/);
  assert.equal(ran, false);
});

test("A: replacing authority.lock between open and flock refuses before the callback runs", async (t) => {
  const root = makeTmp(t);
  const base = testLockOptions();
  let swap = false;
  const lifetime = await acquireWorkerLifetime(root, {
    ...base,
    run: async (args, fd, signal, timeout) => {
      if (swap && args[0] !== "--version") {
        swap = false;
        const named = path.join(authorityDir(root), "authority.lock");
        fs.renameSync(named, named + ".old");
        fs.writeFileSync(named, "", { mode: 0o600 });
      }
      return base.run ? base.run(args, fd, signal, timeout) : realRun(args, fd);
    },
  });
  await withAuthorityTransaction(lifetime, {}, async () => undefined); // creates authority.lock
  swap = true;
  let ran = false;
  await assert.rejects(withAuthorityTransaction(lifetime, {}, async () => { ran = true; }), /inode changed/);
  assert.equal(ran, false);
  assert.equal(await withAuthorityTransaction(lifetime, {}, async () => "recovered"), "recovered");
});

test("W: replacing worker.lock while A is being acquired refuses before the callback runs", async (t) => {
  const root = makeTmp(t);
  const base = testLockOptions();
  let swap = false;
  const lifetime = await acquireWorkerLifetime(root, {
    ...base,
    run: async (args, fd, signal, timeout) => {
      if (swap && args[0] !== "--version") {
        swap = false;
        fs.renameSync(lockFile(root), lockFile(root) + ".old");
        fs.writeFileSync(lockFile(root), "", { mode: 0o600 });
      }
      return base.run ? base.run(args, fd, signal, timeout) : realRun(args, fd);
    },
  });
  await withAuthorityTransaction(lifetime, {}, async () => undefined);
  swap = true;
  let ran = false;
  await assert.rejects(withAuthorityTransaction(lifetime, {}, async () => { ran = true; }), /inode changed/);
  assert.equal(ran, false);
});

/** A flock stand-in that passes the util-linux trust check and then blocks forever. */
function fakeFlock(t: Parameters<typeof makeTmp>[0]): { script: string; pidFile: string } {
  const dir = makeTmp(t, "fake-flock-");
  const pidFile = path.join(dir, "helper.pid");
  const script = path.join(dir, "flock");
  fs.writeFileSync(script, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "flock from util-linux 2.41"; exit 0; fi
echo $$ > '${pidFile}'
exec sleep 300
`, { mode: 0o755 });
  return { script, pidFile };
}

async function readPid(file: string): Promise<number> {
  for (let i = 0; i < 400; i++) {
    try {
      const text = fs.readFileSync(file, "utf8");
      if (/^\d+\n$/.test(text)) return Number(text);
    } catch { /* not written yet */ }
    await delay(25);
  }
  throw new Error("helper never reported its pid");
}

function alive(pid: number): boolean {
  try { process.kill(pid, 0); return true; } catch (error) { return (error as NodeJS.ErrnoException).code !== "ESRCH"; }
}

test("the real flock helper is SIGKILLed and joined on abort, before acquisition returns", { timeout: 30_000 }, async (t) => {
  const { script, pidFile } = fakeFlock(t);
  const controller = new AbortController();
  const attempt = acquireWorkerLifetime(makeTmp(t), { supportedFs: async () => true, flockPath: script, signal: controller.signal });
  attempt.catch(() => undefined);
  const pid = await readPid(pidFile);
  assert.equal(alive(pid), true);
  controller.abort(new Error("stop now"));
  await assert.rejects(attempt, /stop now/);
  assert.equal(alive(pid), false, "the helper had exited by the time the lock descriptor was released");
});

test("the real flock helper is SIGKILLed and joined at its deadline", { timeout: 30_000 }, async (t) => {
  const { script, pidFile } = fakeFlock(t);
  const attempt = acquireWorkerLifetime(makeTmp(t), { supportedFs: async () => true, flockPath: script, timeoutMs: 300 });
  attempt.catch(() => undefined);
  await assert.rejects(attempt, /flock helper timeout/);
  assert.equal(alive(await readPid(pidFile)), false);
});

test("A: a flock helper still unanswered when A's deadline runs out is AuthorityLockBusy, joined before rejection", { timeout: 30_000 }, async (t) => {
  // W's one attempt succeeds; every later attempt (A's) records its pid and hangs.
  const dir = makeTmp(t, "fake-flock-");
  const pidFile = path.join(dir, "helper.pid");
  const wDone = path.join(dir, "w.done");
  const script = path.join(dir, "flock");
  fs.writeFileSync(script, `#!/bin/sh
if [ "$1" = "--version" ]; then echo "flock from util-linux 2.41"; exit 0; fi
if [ ! -e '${wDone}' ]; then : > '${wDone}'; exit 0; fi
echo $$ > '${pidFile}'
exec sleep 300
`, { mode: 0o755 });
  const lifetime = await acquireWorkerLifetime(makeTmp(t), { supportedFs: async () => true, flockPath: script });
  let ran = false;
  await assert.rejects(withAuthorityTransaction(lifetime, { timeoutMs: 300 }, async () => { ran = true; }), isBusy);
  assert.equal(ran, false);
  assert.equal(alive(await readPid(pidFile)), false, "the helper had exited by the time A rejected");
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
  await assert.rejects(withAuthorityTransaction(lifetime, { timeoutMs: 250 }, async () => "no"), isBusy);
  const blocked = withAuthorityTransaction(lifetime, { timeoutMs: 10_000 }, async () => "got it");
  external.stdin.end("go\n");
  assert.equal(await blocked, "got it");
});

test("A: a lock file is created inside the verified directory handle, and a swapped directory is refused", async (t) => {
  const { root, lifetime } = await lifetimeFor(t);
  const dir = authorityDir(root);
  let swapped = false;
  const realOpen = fsp.open;
  // Swap the directory name right after the lock code opens its handle on the directory.
  const patched = mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
    const handle = await realOpen(...args);
    const flags = typeof args[1] === "number" ? args[1] : 0;
    if (args[0] === dir && (flags & fsConstants.O_DIRECTORY) !== 0 && !swapped) {
      swapped = true;
      fs.renameSync(dir, `${dir}.old`);
      fs.mkdirSync(dir, { mode: 0o700 });
    }
    return handle;
  });
  syncBuiltinESMExports();
  t.after(() => { patched.mock.restore(); syncBuiltinESMExports(); });
  await assert.rejects(withAuthorityTransaction(lifetime, { timeoutMs: 2000 }, async () => "no"), /identity changed/);
  assert.equal(swapped, true);
  assert.deepEqual(fs.readdirSync(dir), [], "the lock file was not created in the swapped-in directory");
});

/** A data root that is setgid, like a Kubernetes fsGroup volume (drwxrwsr-x). Skips when the host refuses the bit. */
function setgidRoot(t: Parameters<typeof makeTmp>[0]): string | undefined {
  const root = makeTmp(t);
  fs.chmodSync(root, 0o2770);
  return (fs.statSync(root).mode & 0o2000) === 0 ? undefined : root;
}

test("W: a setgid data root still starts: the created authority directory is exactly 0700", async (t) => {
  const root = setgidRoot(t);
  if (root === undefined) { t.skip("host does not let this user set S_ISGID on its own directory"); return; }
  const lifetime = await acquireWorkerLifetime(root, testLockOptions());
  assert.equal(fs.statSync(authorityDir(root)).mode & 0o7777, 0o700, "mkdir inherited S_ISGID and it was not cleared");
  assert.equal(await withAuthorityTransaction(lifetime, { timeoutMs: 2000 }, async () => "ok"), "ok");
});

test("W: an existing authority directory at 02700 is accepted; setuid or sticky is not", async (t) => {
  const setgid = makeTmp(t);
  fs.mkdirSync(authorityDir(setgid), { mode: 0o700 });
  fs.chmodSync(authorityDir(setgid), 0o2700);
  if ((fs.statSync(authorityDir(setgid)).mode & 0o2000) === 0) { t.skip("host does not let this user set S_ISGID"); return; }
  await acquireWorkerLifetime(setgid, testLockOptions());

  const sticky = makeTmp(t);
  fs.mkdirSync(authorityDir(sticky), { mode: 0o700 });
  fs.chmodSync(authorityDir(sticky), 0o1700);
  await assert.rejects(acquireWorkerLifetime(sticky, testLockOptions()), /unsafe authority directory/);
});

test("W: lock-open failures are authority refusals with context, not raw errnos", async (t) => {
  const symlink = makeTmp(t);
  fs.mkdirSync(authorityDir(symlink), { mode: 0o700 });
  fs.writeFileSync(path.join(symlink, "target"), "", { mode: 0o600 });
  fs.symlinkSync(path.join(symlink, "target"), lockFile(symlink));
  const refusal = await acquireWorkerLifetime(symlink, testLockOptions()).then(() => undefined, (error: unknown) => error);
  assert.ok(refusal instanceof AuthorityRefusal, String(refusal));
  assert.match((refusal as Error).message, /authority lock open failed: ELOOP/);

  // No usable descriptor path (no procfs): the creating open of the via-directory name fails with ENOENT.
  const root = makeTmp(t);
  const realOpen = fsp.open;
  const patched = mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
    if (typeof args[0] === "string" && args[0].startsWith("/proc/self/fd/")) {
      throw Object.assign(new Error("ENOENT: no such file or directory"), { code: "ENOENT" });
    }
    return realOpen(...args);
  });
  syncBuiltinESMExports();
  t.after(() => { patched.mock.restore(); syncBuiltinESMExports(); });
  const noProc = await acquireWorkerLifetime(root, testLockOptions()).then(() => undefined, (error: unknown) => error);
  assert.ok(noProc instanceof AuthorityRefusal, String(noProc));
  assert.match((noProc as Error).message, /authority lock requires \/proc\/self\/fd/);
});

test("W: only the creating open's ENOENT blames procfs; a lock file that vanishes after EEXIST says so", async (t) => {
  const root = makeTmp(t);
  const realOpen = fsp.open;
  const patched = mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
    if (typeof args[0] === "string" && args[0].startsWith("/proc/self/fd/")) {
      const flags = typeof args[1] === "number" ? args[1] : 0;
      const code = (flags & fsConstants.O_EXCL) !== 0 ? "EEXIST" : "ENOENT";
      throw Object.assign(new Error(`${code}: injected`), { code });
    }
    return realOpen(...args);
  });
  syncBuiltinESMExports();
  t.after(() => { patched.mock.restore(); syncBuiltinESMExports(); });
  const vanished = await acquireWorkerLifetime(root, testLockOptions()).then(() => undefined, (error: unknown) => error);
  assert.ok(vanished instanceof AuthorityRefusal, String(vanished));
  assert.match((vanished as Error).message, /lock file vanished/);
  assert.doesNotMatch((vanished as Error).message, /requires/);
});

test("mkdirPrivate turns a name swapped or removed right after mkdir into an authority refusal", async (t) => {
  const realOpen = fsp.open;
  let code = "ELOOP";
  const patched = mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
    const flags = typeof args[1] === "number" ? args[1] : 0;
    if (typeof args[0] === "string" && args[0].endsWith("/swapped") && (flags & fsConstants.O_DIRECTORY) !== 0) {
      throw Object.assign(new Error(`${code}: injected`), { code });
    }
    return realOpen(...args);
  });
  syncBuiltinESMExports();
  t.after(() => { patched.mock.restore(); syncBuiltinESMExports(); });
  for (const [errno, message] of [["ELOOP", /unsafe authority directory/], ["ENOENT", /vanished/], ["EMFILE", /unavailable: EMFILE/]] as const) {
    code = errno;
    const root = makeTmp(t);
    const refusal = await mkdirPrivate(path.join(root, "swapped"), process.getuid?.() ?? -1).then(() => undefined, (error: unknown) => error);
    assert.ok(refusal instanceof AuthorityRefusal, `${errno}: ${String(refusal)}`);
    assert.match((refusal as Error).message, message);
  }
});
