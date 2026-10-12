import { afterEach, beforeEach, it, mock } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { constants as fsConstants, existsSync, type Dir } from "node:fs";
import { Readable } from "node:stream";
import fs from "node:fs/promises";
import path from "node:path";
import { GitCache, type ArchivedRecoveryPinCleanup, type BoundaryProcessSpawner } from "../src/git.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

const scratch = path.resolve("../.uzi/scratch");
const runId = "archived-run";
const ref = "refs/uzi-recovery-pin/archived-run/4";
const git = (cwd: string, ...args: string[]) => execFileSync("git", ["-C", cwd, ...args], {
  encoding: "utf8", stdio: ["ignore", "pipe", "pipe"],
  env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1" },
}).trim();
let root: string, repos: string, bare: string, sha: string, other: string, cache: GitCache;
const request = (extra: Partial<ArchivedRecoveryPinCleanup> = {}): ArchivedRecoveryPinCleanup =>
  ({ runId, generation: 4, sourceSha: sha, isEligible: () => true, ...extra });
const cleanup = (extra: Partial<ArchivedRecoveryPinCleanup> = {}) => cache.cleanupArchivedRecoveryPin(request(extra));
const present = (p = bare, r = ref) => git(p, "rev-parse", "--verify", r);
async function second() {
  const p = path.join(repos, "second.git");
  await fs.mkdir(p);
  git(p, "init", "--bare");
  git(p, "fetch", bare, sha + ":refs/heads/main", other + ":refs/heads/other");
  return p;
}
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
// Fault injection at subprocess startup; assertions exercise the public cleanup capability.
function intercept(fn: (cwd: string, args: string[]) => Promise<void> | void) {
  const seam = cache as unknown as { spawnGit: (...args: any[]) => Promise<any> };
  const original = seam.spawnGit.bind(cache);
  mock.method(seam, "spawnGit", async (...args: any[]) => {
    await fn(args[0], args[1]);
    return original(...args);
  });
}
beforeEach(async () => {
  await fs.mkdir(scratch, { recursive: true });
  root = await fs.mkdtemp(path.join(scratch, "pin-2263-"));
  // Scratch inherits the runner group/setgid bit; cache fixtures must be worker-private.
  await fs.chmod(root, 0o700);
  cache = new GitCache(path.join(root, "data"), nullLogger(), undefined, testGitCacheOptions());
  repos = path.join(root, "data", "repos");
  bare = path.join(repos, "one.git");
  await fs.mkdir(bare, { recursive: true });
  git(bare, "init", "--bare");
  const work = path.join(root, "work");
  await fs.mkdir(work);
  git(work, "init");
  git(work, "config", "user.name", "fixture");
  git(work, "config", "user.email", "fixture@example.invalid");
  git(work, "config", "commit.gpgsign", "false");
  git(work, "commit", "--allow-empty", "-m", "one");
  sha = git(work, "rev-parse", "HEAD");
  git(work, "commit", "--allow-empty", "-m", "two");
  other = git(work, "rev-parse", "HEAD");
  git(work, "push", bare, sha + ":refs/heads/main", other + ":refs/heads/other");
  git(bare, "update-ref", ref, sha);
  git(bare, "update-ref", "refs/uzi-recovery-pin/archived-run/9", sha);
  git(bare, "update-ref", "refs/uzi-settle/archived-run/hold/source", sha);
});
afterEach(async () => { mock.restoreAll(); mock.timers.reset(); await fs.rm(root, { recursive: true, force: true }); });

for (const extra of [
  { recoveryPinBareDir: "one.git" }, { bareDir: "one.git" },
  { recoveryPinBareDir: "one.git", bareDir: "one.git" }, {},
]) it("conditionally deletes exact own pin: " + JSON.stringify(extra), async () => {
  assert.deepEqual(await cleanup(extra), { state: "deleted" });
  assert.throws(() => present());
  assert.equal(present(bare, "refs/uzi-recovery-pin/archived-run/9"), sha);
  assert.equal(present(bare, "refs/uzi-settle/archived-run/hold/source"), sha);
});
it("complete absence does not attribute an object-only repository", async () => {
  git(bare, "update-ref", "-d", ref);
  await second();
  assert.deepEqual(await cleanup(), { state: "already_absent" });
});
it("complete empty scan is absence", async () => {
  await fs.rm(bare, { recursive: true });
  assert.deepEqual(await cleanup(), { state: "already_absent" });
});
it("duplicate exact occurrences with differing targets refuse", async () => {
  const p = await second();
  git(p, "update-ref", ref, other);
  assert.deepEqual(await cleanup(), { state: "retained", reason: "ambiguous" });
  assert.equal(present(), sha);
  assert.equal(present(p), other);
});
for (const extra of [
  { runId: "" }, { runId: "archived/run" }, { runId: "archived-run\n" },
  { generation: -1 }, { generation: 0.5 }, { generation: Number.MAX_SAFE_INTEGER + 1 },
  { sourceSha: "a".repeat(40) + "\n" }, { sourceSha: "bad" },
  { recoveryPinBareDir: "../one.git" }, { recoveryPinBareDir: "" },
  { recoveryPinBareDir: "one.git", bareDir: "second.git" },
]) it("invalid authenticated identity never falls back: " + JSON.stringify(extra), async () => {
  assert.equal((await cleanup(extra)).state, "retained");
  assert.equal(present(), sha);
});
it("changed target before cleanup refuses", async () => {
  git(bare, "update-ref", ref, other);
  assert.deepEqual(await cleanup(), { state: "retained", reason: "target_changed" });
  assert.equal(present(), other);
});
it("CAS rejects changed target immediately before mutation", async () => {
  intercept((_cwd, args) => { if (args.includes("update-ref") && args.includes("--stdin")) git(bare, "update-ref", ref, other); });
  assert.equal((await cleanup()).state, "retained");
  assert.equal(present(), other);
});
it("packed exact pin deletes", async () => {
  git(bare, "pack-refs", "--all", "--prune");
  assert.deepEqual(await cleanup(), { state: "deleted" });
  assert.throws(() => present());
});
for (const dangling of [false, true]) it("symbolic pin refuses, dangling=" + dangling, async () => {
  git(bare, "symbolic-ref", ref, dangling ? "refs/heads/missing" : "refs/heads/main");
  assert.equal((await cleanup()).state, "retained");
  assert.equal(git(bare, "symbolic-ref", ref), dangling ? "refs/heads/missing" : "refs/heads/main");
  assert.equal(present(bare, "refs/heads/main"), sha);
});
it("direct-to-symbolic race refuses with alias and referent intact", async () => {
  intercept((_cwd, args) => { if (args.includes("update-ref") && args.includes("--stdin")) git(bare, "symbolic-ref", ref, "refs/heads/main"); });
  assert.equal((await cleanup()).state, "retained");
  assert.equal(git(bare, "symbolic-ref", ref), "refs/heads/main");
  assert.equal(present(bare, "refs/heads/main"), sha);
});
it("broken direct ref refuses", async () => {
  await fs.writeFile(path.join(bare, ref), "broken\n");
  assert.equal((await cleanup()).state, "retained");
});
it("final eligibility drift refuses", async () => {
  assert.deepEqual(await cleanup({ isEligible: () => false }), { state: "retained", reason: "ineligible" });
  assert.equal(present(), sha);
});
it("prepared transaction rechecks eligibility while exact Git lock is held", async () => {
  let locked = false;
  assert.deepEqual(await cleanup({ isEligible: () => {
    locked ||= existsSync(path.join(bare, ref + ".lock"));
    return !locked;
  } }), { state: "retained", reason: "ineligible" });
  assert.equal(locked, true);
  assert.equal(present(), sha);
  assert.equal(existsSync(path.join(bare, ref + ".lock")), false);
});
it("late prepared transaction spawn after cancellation never commits", async () => {
  const entered = deferred(), release = deferred(), drained = deferred();
  const seam = cache as unknown as { spawnGit: (...args: any[]) => Promise<any> };
  const original = seam.spawnGit.bind(cache);
  mock.method(seam, "spawnGit", async (...args: any[]) => {
    if (!args[1].includes("update-ref") || !args[1].includes("--stdin")) return original(...args);
    entered.resolve(); await release.promise;
    const child = await original(...args);
    void child.exited.then(() => drained.resolve());
    return child;
  });
  const stop = new AbortController();
  const result = cleanup({ signal: stop.signal });
  await entered.promise; stop.abort();
  assert.deepEqual(await result, { state: "retained", reason: "cancelled" });
  release.resolve(); await drained.promise;
  assert.equal(present(), sha);
  assert.equal(existsSync(path.join(bare, ref + ".lock")), false);
});
it("prepared transaction cancellation aborts and reaps before returning", async () => {
  const stop = new AbortController();
  let locked = false;
  assert.deepEqual(await cleanup({ signal: stop.signal, isEligible: () => {
    if (existsSync(path.join(bare, ref + ".lock"))) { locked = true; stop.abort(); }
    return true;
  } }), { state: "retained", reason: "cancelled" });
  assert.equal(locked, true);
  assert.equal(present(), sha);
  assert.equal(existsSync(path.join(bare, ref + ".lock")), false);
});
it("cancelled and expired calls cannot mutate", async () => {
  const stop = new AbortController(); stop.abort();
  assert.deepEqual(await cleanup({ signal: stop.signal }), { state: "retained", reason: "cancelled" });
  assert.deepEqual(await cleanup({ deadline: Date.now() - 1 }), { state: "retained", reason: "deadline" });
  assert.equal(present(), sha);
});
for (const key of ["include.path", "includeIf.gitdir:/.path", "core.worktree", "core.hooksPath", "core.sshCommand"]) {
  it("unsafe config " + key + " refuses", async () => {
    git(bare, "config", key, "/missing");
    assert.equal((await cleanup()).state, "retained");
    assert.equal(await fs.readFile(path.join(bare, ref), "utf8"), sha + "\n");
  });
}
for (const name of ["config", "packed-refs", ref]) it("symlink metadata " + name + " refuses", async () => {
  const p = path.join(bare, name), target = path.join(root, "target");
  const data = await fs.readFile(p).catch(() => Buffer.from(""));
  await fs.writeFile(target, data);
  await fs.rm(p, { force: true });
  await fs.symlink(target, p);
  assert.equal((await cleanup()).state, "retained");
});
it("symlink bare and unsafe sibling refuse discovery", async () => {
  await fs.symlink(bare, path.join(repos, "alias.git"));
  assert.equal((await cleanup()).state, "retained");
  assert.equal(present(), sha);
});
for (const p of ["root", "bare", "config", "refs"]) it("world-writable " + p + " refuses", async () => {
  await fs.chmod(p === "root" ? repos : p === "bare" ? bare : path.join(bare, p), 0o777);
  assert.equal((await cleanup()).state, "retained");
});
for (const [name, size] of [["config", 1024 * 1024 + 1], ["packed-refs", 16 * 1024 * 1024 + 1], [ref, 1025]] as const) {
  it("metadata admission limit " + name, async () => {
    const h = await fs.open(path.join(bare, name), "a"); await h.truncate(size); await h.close();
    assert.equal((await cleanup()).state, "retained");
  });
}
it("cumulative metadata budget refuses across scan candidates", async () => {
  const p = await second(), q = path.join(repos, "third.git");
  await fs.cp(p, q, { recursive: true });
  const packed = "# " + "x".repeat(16 * 1024 * 1024 - 3) + "\n";
  for (const d of [bare, p, q]) await fs.writeFile(path.join(d, "packed-refs"), packed);
  assert.deepEqual(await cleanup(), { state: "retained", reason: "unsafe_metadata" });
  // The deliberately oversized packed input is not valid Git syntax; inspect the exact loose pin.
  assert.equal(await fs.readFile(path.join(bare, ref), "utf8"), sha + "\n");
});
it("Git reference lock failure retains", async () => {
  await fs.writeFile(path.join(bare, ref + ".lock"), "");
  assert.equal((await cleanup()).state, "retained");
  assert.equal(present(), sha);
});
it("optional best-effort expected SHA preserves three-argument API", async () => {
  await cache.deleteRecoveryPin(bare, runId, 4, other);
  assert.equal(present(), sha);
  await cache.deleteRecoveryPin(bare, runId, 4, sha);
  assert.throws(() => present());
  git(bare, "update-ref", ref, sha);
  await cache.deleteRecoveryPin(bare, runId, 4);
  assert.throws(() => present());
});
it("optional best-effort direct-to-symbolic race preserves alias and referent", async () => {
  intercept((_cwd, args) => {
    if (args.includes("update-ref") && args.includes("--stdin")) git(bare, "symbolic-ref", ref, "refs/heads/main");
  });
  await cache.deleteRecoveryPin(bare, runId, 4, sha);
  assert.equal(git(bare, "symbolic-ref", ref), "refs/heads/main");
  assert.equal(present(bare, "refs/heads/main"), sha);
});
it("optional best-effort foreign path cannot redirect by basename", async () => {
  await cache.deleteRecoveryPin(path.join(root, "foreign", "one.git"), runId, 4, sha);
  assert.equal(present(), sha);
});
it("anchor aliases queue behind cleanup attribution even across bares", async () => {
  const p = await second(), entered = deferred(), release = deferred();
  let held = false;
  intercept(async (_cwd, args) => {
    if (!held && args.includes("--is-bare-repository")) {
      held = true; entered.resolve(); await release.promise;
    }
  });
  const removal = cleanup();
  await entered.promise;
  let anchored = false;
  const insertion = cache.anchorRecoveryHead(p, "archived/run", 4, sha).then(v => { anchored = v; return v; });
  await new Promise<void>(r => setImmediate(r));
  assert.equal(anchored, false);
  release.resolve();
  assert.deepEqual(await removal, { state: "deleted" });
  assert.equal(await insertion, true);
  assert.equal(present(p), sha);
});
it("blocked attribution waiter cancels without later mutation", async () => {
  const entered = deferred(), release = deferred();
  let held = false;
  intercept(async (_cwd, args) => {
    if (!held && args.includes("--is-bare-repository")) { held = true; entered.resolve(); await release.promise; }
  });
  const holder = cleanup({ isEligible: () => false });
  await entered.promise;
  const stop = new AbortController();
  const waiter = cleanup({ signal: stop.signal });
  stop.abort();
  assert.deepEqual(await waiter, { state: "retained", reason: "cancelled" });
  release.resolve();
  assert.equal((await holder).state, "retained");
  assert.equal(present(), sha);
});
it("blocked bare lock cancels without later mutation", async () => {
  const entered = deferred(), release = deferred();
  const seam = cache as unknown as { withLock: <T>(key: string, fn: () => Promise<T>) => Promise<T> };
  const holder = seam.withLock(bare, async () => { entered.resolve(); await release.promise; });
  await entered.promise;
  const stop = new AbortController();
  const waiter = cleanup({ signal: stop.signal });
  await new Promise<void>(r => setImmediate(r));
  stop.abort();
  assert.deepEqual(await waiter, { state: "retained", reason: "cancelled" });
  release.resolve(); await holder;
  assert.equal(present(), sha);
});
it("late filesystem read cannot start mutation after caller deadline", async () => {
  const original = fs.lstat.bind(fs), entered = deferred(), release = deferred(), drained = deferred();
  let held = false;
  mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    if (!held && args[0] === path.join(bare, "config")) {
      held = true; entered.resolve(); await release.promise;
      const result = await original(...args); drained.resolve(); return result;
    }
    return original(...args);
  });
  mock.timers.enable({ apis: ["setTimeout"] });
  const result = cleanup({ deadline: Date.now() + 30_000 });
  await entered.promise;
  mock.timers.tick(30_000);
  assert.deepEqual(await result, { state: "retained", reason: "deadline" });
  release.resolve(); await drained.promise;
  await new Promise<void>(r => setImmediate(r));
  assert.equal(present(), sha);
});
for (const which of ["root", "bare"]) it(which + " replacement under bare-lock wait refuses", async () => {
  const entered = deferred(), release = deferred();
  const seam = cache as unknown as { withLock: <T>(key: string, fn: () => Promise<T>) => Promise<T> };
  const holder = seam.withLock(bare, async () => { entered.resolve(); await release.promise; });
  await entered.promise;
  const ready = deferred();
  const original = seam.withLock.bind(cache);
  mock.method(seam, "withLock", async (key: string, fn: () => Promise<unknown>, ...rest: unknown[]) => {
    if (key === bare) ready.resolve();
    return (original as (...args: any[]) => Promise<unknown>)(key, fn, ...rest);
  });
  const removal = cleanup();
  await ready.promise;
  const target = which === "root" ? repos : bare;
  await fs.rename(target, target + ".old");
  await fs.mkdir(target);
  release.resolve(); await holder;
  assert.equal((await removal).state, "retained");
  assert.equal(present(which === "root" ? path.join(repos + ".old", "one.git") : bare + ".old"), sha);
});

it("discovery entry cap refuses an otherwise matching pin", async () => {
  await second();
  const open = fs.opendir.bind(fs);
  let count = 0;
  mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    if (args[0] !== repos) return open(...args);
    return {
      read: async () => ++count <= 257 ? { name: count === 1 ? "one.git" : "second.git" } : null,
      close: async () => {},
    } as unknown as Dir;
  });
  const seam = cache as unknown as { spawnGit: (...args: any[]) => Promise<any> };
  const spawn = seam.spawnGit.bind(cache);
  mock.method(seam, "spawnGit", async (...args: any[]) => {
    const flags = args[1] as string[];
    if (flags.includes("--name-only") || flags.includes("--is-bare-repository")) {
      return { stdout: Readable.from([flags.includes("--name-only") ? "" : "true\n"]),
        exited: Promise.resolve(0), cancel: () => {} };
    }
    return spawn(...args);
  });
  assert.deepEqual(await cleanup(), { state: "retained", reason: "scan_limit" });
  assert.equal(await fs.readFile(path.join(bare, ref), "utf8"), sha + "\n");
});
it("oversized Git probe output refuses before mutation", async () => {
  const seam = cache as unknown as { spawnGit: (...args: any[]) => Promise<any> };
  const spawn = seam.spawnGit.bind(cache);
  mock.method(seam, "spawnGit", async (...args: any[]) => {
    if ((args[1] as string[]).includes("--name-only")) {
      return { stdout: Readable.from(["x".repeat(65537)]), exited: Promise.resolve(0), cancel: () => {} };
    }
    return spawn(...args);
  });
  assert.deepEqual(await cleanup(), { state: "retained", reason: "unsafe_metadata" });
  assert.equal(present(), sha);
});
it("config growth between admission and open refuses", async () => {
  const open = fs.open.bind(fs);
  let configOpens = 0;
  mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    if (args[0] === path.join(bare, "config") && ++configOpens === 2) {
      await fs.appendFile(path.join(bare, "config"), "\n# growing metadata\n");
    }
    return open(...args);
  });
  assert.deepEqual(await cleanup(), { state: "retained", reason: "unsafe_metadata" });
  assert.equal(present(), sha);
});
it("regular-file to FIFO replacement uses nonblocking open and refuses", async () => {
  const open = fs.open.bind(fs);
  let nonblocking = false;
  let replaced = false;
  mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    if (!replaced && args[0] === path.join(bare, "config")) {
      replaced = true;
      nonblocking = typeof args[1] === "number" && !!(args[1] & fsConstants.O_NONBLOCK);
      // A missing flag must make the assertion fail without hanging this regression.
      if (nonblocking) {
        await fs.rm(path.join(bare, "config"));
        execFileSync("mkfifo", [path.join(bare, "config")]);
      }
    }
    return open(...args);
  });
  const result = await cleanup();
  assert.equal(nonblocking, true);
  assert.deepEqual(result, { state: "retained", reason: "unsafe_metadata" });
  assert.equal(await fs.readFile(path.join(bare, ref), "utf8"), sha + "\n");
});
it("missing explicit bare never falls back to the surviving pin", async () => {
  assert.equal((await cleanup({ recoveryPinBareDir: "missing.git" })).state, "retained");
  assert.equal(present(), sha);
});
it("non-bare sibling refuses complete attribution", async () => {
  const work = path.join(repos, "nonbare");
  await fs.mkdir(work);
  git(work, "init");
  assert.equal((await cleanup()).state, "retained");
  assert.equal(present(), sha);
});
for (const symbolic of [false, true]) it("prepared cleanup inside supervised boundary, symbolic race=" + symbolic, async () => {
  let active = 0;
  let transaction = false;
  const timeouts: number[] = [];
  const spawner: BoundaryProcessSpawner = async request => {
    if (request.argv.includes("update-ref") && request.argv.includes("--stdin")) {
      transaction = true;
      if (symbolic) git(bare, "symbolic-ref", ref, "refs/heads/main");
    }
    assert.ok(request.timeoutMs && request.timeoutMs > 0 && request.timeoutMs <= 30_000);
    timeouts.push(request.timeoutMs!);
    const child = spawn(request.argv[0]!, request.argv.slice(1), {
      cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"],
      timeout: request.timeoutMs, killSignal: "SIGKILL",
    });
    active++;
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => { active--; resolve({ code: code ?? 128 }); });
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => {
        if (child.exitCode === null && child.signalCode === null) child.kill("SIGTERM");
        await completed;
      } };
  };
  const result = await cache.withBoundaryProcessSpawner(spawner, new AbortController().signal, () => cleanup());
  assert.equal(transaction, true);
  assert.ok(timeouts.length > 0);
  assert.equal(active, 0, "all supervised children completed before cleanup returned");
  if (symbolic) {
    assert.equal(result.state, "retained");
    assert.equal(git(bare, "symbolic-ref", ref), "refs/heads/main");
    assert.equal(git(bare, "rev-parse", "refs/heads/main"), sha);
  } else {
    assert.deepEqual(result, { state: "deleted" });
    assert.throws(() => present());
  }
});

