import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import crypto from "node:crypto";
import { once } from "node:events";
import fs from "node:fs";
import path from "node:path";
import { PassThrough, Readable, Writable } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache, RunnerCloneImportError, type BoundaryProcessSpawner, type RunnerClone } from "../src/git.js";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "../src/harness.js";

// issue #1769 m2 — a self-contained (Codex) runner clone has no alternate into the worker bare,
// so the fresh default tip `fetchDefaultTip` brings into the bare at finalize is not readable in
// the clone until ensureRunnerCloneObjects streams its objects across (worker pack-objects in the
// bare piped into runner index-pack in the clone). Real temp repos throughout.

const GIT_ENV: NodeJS.ProcessEnv = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
delete GIT_ENV.GIT_ALTERNATE_OBJECT_DIRECTORIES;
delete GIT_ENV.GIT_OBJECT_DIRECTORY;
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const SELF = { selfContained: true } as const;
const UNKNOWN_SHA = "0123456789abcdef0123456789abcdef01234567";

let fx: Fixture;
let git: GitCache;

beforeEach(() => {
  fx = makeFixture({ "conflict.txt": "base\n" });
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
});

afterEach(() => fx.cleanup());

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
}

function resolves(dir: string, sha: string): boolean {
  try {
    gitIn(dir, ["cat-file", "-e", `${sha}^{commit}`]);
    return true;
  } catch {
    return false;
  }
}

/** Seed a clone, commit agent work, then advance origin main and fetch it into the bare (the
 *  finalize `fetchDefaultTip` shape). */
async function setup(opts?: { selfContained: true }): Promise<{
  bare: string;
  rc: RunnerClone;
  baseTip: string;
  defaultTip: string;
}> {
  const bare = await git.ensureClone(fx.originPath);
  const rc = await git.createOrAttachRunnerClone(bare, 1, noProofReseed, "run-1", false, undefined, undefined, opts);
  fs.writeFileSync(path.join(rc.path, "impl.ts"), "export const x = 1;\n");
  gitIn(rc.path, ["add", "."]);
  gitIn(rc.path, [...IDENT, "commit", "-m", "agent work"]);
  const baseTip = gitIn(rc.path, ["rev-parse", "HEAD"]);
  fs.writeFileSync(path.join(fx.originPath, "main-only.txt"), "main advances\n");
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", "main advances"]);
  const defaultTip = await git.fetchDefaultTip(bare, "main");
  return { bare, rc, baseTip, defaultTip };
}

type SpawnGitResult = { child?: import("node:child_process").ChildProcess; stdout: import("node:stream").Readable; exited: Promise<number> };
type ConsumerResult = { stdin: import("node:stream").Writable; exited: Promise<{ code: number; stderr: string }>; abort: () => void };
interface Internals {
  spawnGit(cwd: string, args: string[], stdin?: string): Promise<SpawnGitResult>;
  spawnGitAsRunnerWithStdin(cwd: string, args: string[]): Promise<ConsumerResult>;
}

/** Record every producer / consumer the import starts (TS-private, reached by a cast). */
function spyImport(cache: GitCache): { producers: { args: string[]; stdin?: string; result: SpawnGitResult }[]; consumers: ConsumerResult[] } {
  const internals = cache as unknown as Internals;
  const producers: { args: string[]; stdin?: string; result: SpawnGitResult }[] = [];
  const consumers: ConsumerResult[] = [];
  const origProducer = internals.spawnGit.bind(cache);
  const origConsumer = internals.spawnGitAsRunnerWithStdin.bind(cache);
  internals.spawnGit = async (cwd, args, stdin) => {
    const result = await origProducer(cwd, args, stdin);
    producers.push({ args, stdin, result });
    return result;
  };
  internals.spawnGitAsRunnerWithStdin = async (cwd, args) => {
    const result = await origConsumer(cwd, args);
    consumers.push(result);
    return result;
  };
  return { producers, consumers };
}

/** True when `p` has already settled (a settled promise wins a race against setImmediate). */
async function settled(p: Promise<unknown>): Promise<boolean> {
  const pending = Symbol("pending");
  const winner = await Promise.race([p.then(() => true, () => true), new Promise((r) => setImmediate(() => r(pending)))]);
  return winner === true;
}

function childGone(child: import("node:child_process").ChildProcess | undefined): boolean {
  return child === undefined || child.exitCode !== null || child.signalCode !== null;
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/** Spawn `request` for real, exactly as the plain boundary spawner in the ordering test does. */
function spawnReal(request: BoundaryProcessRequest): { child: ChildProcess; handle: BoundaryProcessHandle } {
  const [command, ...args] = request.argv;
  const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
  const completed = new Promise<{ code: number }>((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code) => resolve({ code: code ?? 128 }));
  });
  return { child, handle: { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, cancel: async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; }, completed } };
}

/** A producer that STALLS: a real process that writes nothing and never exits on its own. It is
 *  ended only by losing its stdout (the EPIPE its next write would get, collapsed to "at once"),
 *  so it stops only if the import destroys that stream. `code` is its exit status once killed. */
function stalledProducer(): { child: ChildProcess; handle: BoundaryProcessHandle } {
  const child = spawn("sleep", ["600"], { stdio: ["pipe", "pipe", "pipe"] });
  child.stdout.once("close", () => child.kill("SIGKILL"));
  const completed = new Promise<{ code: number }>((resolve) => child.once("close", (code) => resolve({ code: code ?? 137 })));
  return { child, handle: { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, cancel: async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; }, completed } };
}

/** A stdin that accepts and discards everything and never closes on its own (a supervisor
 *  transport need not close the pipe when its child exits). */
function openSink(): Writable {
  return new Writable({ write: (_chunk, _enc, cb) => cb() });
}

/** A consumer stdin forwarding to `child`'s real stdin that, when the child exits, closes with no
 *  error (as a supervisor transport may, and as Node does to a child's stdin on its exit). A late
 *  write's EPIPE is swallowed, so the pipe never breaks: only the exit status and the tip probe
 *  can judge the import. */
function closeOnExitStdin(child: ChildProcess): Writable {
  const target = child.stdin!;
  target.on("error", () => undefined);
  const stdin = new Writable({
    write(chunk: Buffer, _enc, cb) {
      if (target.destroyed || !target.writable) {
        cb();
        return;
      }
      target.write(chunk, () => cb());
    },
    final(cb) {
      if (!target.destroyed && target.writable) target.end();
      cb();
    },
    destroy(err, cb) {
      target.destroy();
      cb(err);
    },
  });
  child.once("exit", () => stdin.destroy());
  return stdin;
}

/** Await `p`, or fail after `ms` (killing `children` first) instead of hanging the suite: a torn-down
 *  path that is missing shows up as an import that never settles. */
async function settleWithin<T>(p: Promise<T>, ms: number, children: () => (ChildProcess | undefined)[]): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      for (const c of children()) if (c && !childGone(c)) c.kill("SIGKILL");
      reject(new Error(`the import did not settle within ${ms}ms: a stalled side was never torn down`));
    }, ms);
  });
  try {
    return await Promise.race([p, timeout]);
  } finally {
    clearTimeout(timer);
  }
}

/** The import's rejection, which must be a RunnerCloneImportError. */
async function importError(p: Promise<void>): Promise<RunnerCloneImportError> {
  try {
    await p;
  } catch (err) {
    assert.ok(err instanceof RunnerCloneImportError, `expected RunnerCloneImportError, got ${String(err)}`);
    return err;
  }
  assert.fail("the import resolved; expected a RunnerCloneImportError");
}

describe("GitCache.ensureRunnerCloneObjects (issue #1769 m2)", () => {
  it("imports the fresh default tip into a self-contained clone; align then works with the bare gone", async () => {
    const { bare, rc, baseTip, defaultTip } = await setup(SELF);
    assert.strictEqual(resolves(rc.path, defaultTip), false, "the fresh tip is not in the clone yet");
    assert.throws(() => gitIn(rc.path, ["update-ref", "refs/uzi-test/probe", defaultTip]), "update-ref fails before the import");

    const spy = spyImport(git);
    await git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit, rc.defaultBranchCommit ?? ""]);
    assert.strictEqual(spy.producers.length, 1);
    assert.deepStrictEqual(spy.producers[0]!.args, ["pack-objects", "--revs", "--stdout", "-q"], "a non-thin pack");
    assert.ok(resolves(rc.path, defaultTip), "the tip resolves after the import");
    assert.strictEqual(fs.existsSync(path.join(rc.path, ".git", "objects", "info", "alternates")), false);

    fs.renameSync(bare, `${bare}.hidden`);
    const res = await git.alignBranchWithDefault(rc.path, "agent/issue-1", baseTip, defaultTip, "merge");
    assert.strictEqual(res, "aligned");
    assert.strictEqual(gitIn(rc.path, ["show", "HEAD:impl.ts"]), "export const x = 1;");
    assert.strictEqual(gitIn(rc.path, ["show", "HEAD:main-only.txt"]), "main advances");
    gitIn(rc.path, ["fsck", "--connectivity-only", "--no-dangling", "--no-progress"]);
  });

  it("is a no-op when the tip is already present: no pack-objects is spawned", async () => {
    // A --shared (Claude) clone reaches the fresh tip through its alternate.
    const shared = await setup();
    const spy = spyImport(git);
    await git.ensureRunnerCloneObjects(shared.bare, shared.rc.path, shared.defaultTip, [shared.rc.baseCommit]);
    // A self-contained clone asked for a tip it already holds (its own base).
    const self = await git.createOrAttachRunnerClone(shared.bare, 2, noProofReseed, "run-2", false, undefined, undefined, SELF);
    await git.ensureRunnerCloneObjects(shared.bare, self.path, self.baseCommit, []);
    assert.strictEqual(spy.producers.length, 0, "no producer spawned");
    assert.strictEqual(spy.consumers.length, 0, "no consumer spawned");
  });

  it("filters haves to non-empty object ids the bare holds", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    const spy = spyImport(git);
    await git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, ["", UNKNOWN_SHA, "--all", "HEAD", rc.baseCommit, rc.baseCommit]);
    assert.ok(resolves(rc.path, defaultTip));
    assert.strictEqual(spy.producers[0]!.stdin, `${defaultTip}\n--not\n${rc.baseCommit}\n`, "only the known have is excluded");
  });

  it("a producer failure (unknown tip) throws RunnerCloneImportError with no child left running", async () => {
    const { bare, rc } = await setup(SELF);
    const spy = spyImport(git);
    await assert.rejects(git.ensureRunnerCloneObjects(bare, rc.path, UNKNOWN_SHA, [rc.baseCommit]), RunnerCloneImportError);
    assert.strictEqual(spy.producers.length, 1);
    assert.strictEqual(spy.consumers.length, 1);
    assert.notStrictEqual(await spy.producers[0]!.result.exited, 0);
    assert.ok(childGone(spy.producers[0]!.result.child), "the producer has exited");
    assert.ok(await settled(spy.consumers[0]!.exited), "the consumer has exited");
  });

  it("a malformed tip is refused before anything is spawned", async () => {
    const { bare, rc } = await setup(SELF);
    const spy = spyImport(git);
    await assert.rejects(git.ensureRunnerCloneObjects(bare, rc.path, "--all\nHEAD", []), RunnerCloneImportError);
    assert.strictEqual(spy.producers.length, 0);
  });

  it("a symbolic tip the clone resolves is refused, not accepted as already present", async () => {
    const { bare, rc } = await setup(SELF);
    assert.ok(resolves(rc.path, "HEAD"), "the clone resolves HEAD");
    const probes: string[][] = [];
    const internals = git as unknown as { runGitAsRunner(cwd: string | undefined, args: string[]): Promise<string> };
    const orig = internals.runGitAsRunner.bind(git);
    internals.runGitAsRunner = async (cwd, args) => {
      probes.push(args);
      return orig(cwd, args);
    };
    const err = await importError(git.ensureRunnerCloneObjects(bare, rc.path, "HEAD", []));
    assert.match(err.causeMessage, /not a full object id/);
    assert.deepStrictEqual(probes, [], "the tip is validated before any probe runs");
  });

  it("a consumer failure (unwritable pack dir) throws RunnerCloneImportError with no child left running", async (t) => {
    if (process.getuid?.() === 0) {
      t.skip("root ignores the directory mode");
      return;
    }
    const { bare, rc, defaultTip } = await setup(SELF);
    const packDir = path.join(rc.path, ".git", "objects", "pack");
    const spy = spyImport(git);
    fs.chmodSync(packDir, 0o555);
    try {
      await assert.rejects(
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]),
        (err: unknown) => err instanceof RunnerCloneImportError && /index-pack/.test(err.message),
      );
    } finally {
      fs.chmodSync(packDir, 0o755);
    }
    assert.ok(childGone(spy.producers[0]!.result.child), "the producer has exited");
    assert.ok(await settled(spy.consumers[0]!.exited), "the consumer has exited");
    assert.strictEqual(resolves(rc.path, defaultTip), false, "nothing was imported");
  });

  it("inside a boundary: the worker_pat producer spawns before the command consumer, and stdin is piped", async () => {
    const { bare, rc, baseTip, defaultTip } = await setup(SELF);
    const spawned: { identity: string; argv: readonly string[]; cwd: string }[] = [];
    const spawner: BoundaryProcessSpawner = async (request) => {
      spawned.push({ identity: request.identity, argv: request.argv, cwd: request.cwd });
      const [command, ...args] = request.argv;
      const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const completed = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("close", (code) => resolve({ code: code ?? 128 }));
      });
      return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, cancel: async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; }, completed };
    };
    await git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
      git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]),
    );
    const producer = spawned.findIndex((s) => s.argv.includes("pack-objects"));
    const consumer = spawned.findIndex((s) => s.argv.includes("index-pack"));
    assert.ok(producer >= 0 && consumer >= 0, "both sides ran through the boundary spawner");
    assert.strictEqual(spawned[producer]!.identity, "worker_pat");
    assert.strictEqual(spawned[consumer]!.identity, "command");
    assert.ok(producer < consumer, "the worker_pat producer is spawned first");
    assert.strictEqual(spawned[consumer]!.cwd, rc.path, "the consumer is rooted at the clone");
    assert.deepStrictEqual(spawned[consumer]!.argv, ["/usr/bin/git", "-C", rc.path, "index-pack", "--stdin"]);

    assert.ok(resolves(rc.path, defaultTip), "the pack streamed through the boundary stdin");
    fs.renameSync(bare, `${bare}.hidden`);
    assert.strictEqual(await git.alignBranchWithDefault(rc.path, "agent/issue-1", baseTip, defaultTip, "merge"), "aligned");
  });

  it("inside a boundary: a producer that exits before the consumer has started loses no pack bytes", async () => {
    // Under the real supervisor the command consumer's launch takes long enough that a small
    // pack-objects can exit (and its root reap) first. Node resumes every readable stdio stream
    // of a ChildProcess on its 'exit' (child_process flushStdio), so a producer stdout nobody
    // was reading yet was drained into nothing and index-pack read an empty stdin ("early
    // EOF"). Here the consumer spawn waits until the producer's 'exit' has fired and that
    // flush has run, which makes the old loss deterministic.
    const { bare, rc, defaultTip } = await setup(SELF);
    let producerExited: Promise<void> | undefined;
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("index-pack")) {
        assert.ok(producerExited, "the producer spawned first");
        await producerExited;
        for (let i = 0; i < 3; i++) await new Promise((r) => setImmediate(r));
      }
      const { child, handle } = spawnReal(request);
      if (request.argv.includes("pack-objects")) {
        producerExited = new Promise((resolve) => child.once("exit", () => resolve()));
      }
      return handle;
    };
    await git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
      git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]),
    );
    assert.ok(resolves(rc.path, defaultTip), "the whole pack reached index-pack");
  });

  // Teardown. The fixtures above end both processes naturally, so these stall one side on
  // purpose: each case settles only through the teardown path it names.

  it("inside a boundary: a consumer that fails fast tears down a producer stalled before its first write", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    let producer: ChildProcess | undefined;
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("pack-objects")) {
        const stalled = stalledProducer();
        producer = stalled.child;
        return stalled.handle;
      }
      if (request.argv.includes("index-pack")) {
        return {
          stdin: openSink(),
          stdout: null,
          stderr: Readable.from(["fatal: consumer broke\n"]),
          cancel: async () => {}, completed: new Promise((resolve) => setImmediate(() => resolve({ code: 1 }))),
        };
      }
      return spawnReal(request).handle;
    };
    const spy = spyImport(git);
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      10_000,
      () => [producer],
    );
    assert.match(err.causeMessage, /^index-pack exited 1: fatal: consumer broke; pack-objects exited \d+ \(stopped after its peer failed\)$/);
    assert.ok(producer && childGone(producer), "the stalled producer process has exited");
    assert.ok(await settled(spy.producers[0]!.result.exited), "the producer's exited has settled");
  });

  it("inside a boundary: a producer that fails fast tears down a consumer stalled reading stdin", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    let consumer: ChildProcess | undefined;
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("pack-objects")) {
        // Fails without writing and without ending its stdout (the supervisor transport stays open).
        return {
          stdin: openSink(),
          stdout: new PassThrough(),
          stderr: Readable.from(["fatal: producer broke\n"]),
          cancel: async () => {}, completed: new Promise((resolve) => setImmediate(() => resolve({ code: 1 }))),
        };
      }
      if (request.argv.includes("index-pack")) {
        // The real index-pack: it blocks reading stdin until EOF.
        const real = spawnReal(request);
        consumer = real.child;
        return real.handle;
      }
      return spawnReal(request).handle;
    };
    const spy = spyImport(git);
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      10_000,
      () => [consumer],
    );
    assert.match(err.causeMessage, /^pack-objects exited 1: .*producer broke; index-pack exited \d+.* \(stopped after its peer failed\)$/s);
    assert.ok(consumer && childGone(consumer), "the stalled consumer process has exited");
    assert.ok(await settled(spy.consumers[0]!.exited), "the consumer's exited has settled");
  });

  it("inside a boundary: a broken consumer pipe with both exits pending is blamed on index-pack", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    let producer: ChildProcess | undefined;
    const producerDone = deferred<void>();
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("pack-objects")) {
        const stalled = stalledProducer();
        producer = stalled.child;
        void stalled.handle.completed.then(() => producerDone.resolve());
        return stalled.handle;
      }
      if (request.argv.includes("index-pack")) {
        // The consumer's input breaks first; its exit status is only reported once the producer
        // is gone, so nothing but the pipe break can start the teardown.
        const stdin = openSink();
        setImmediate(() => stdin.destroy(Object.assign(new Error("write EPIPE"), { code: "EPIPE" })));
        return {
          stdin,
          stdout: null,
          stderr: Readable.from(["fatal: early EOF\n"]),
          cancel: async () => {}, completed: producerDone.promise.then(() => ({ code: 1 })),
        };
      }
      return spawnReal(request).handle;
    };
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      10_000,
      () => [producer],
    );
    assert.match(err.causeMessage, /^index-pack exited 1: fatal: early EOF; pack-objects exited \d+ \(stopped after its peer failed\)$/);
    assert.ok(producer && childGone(producer), "the stalled producer process has exited");
  });

  it("inside a boundary: a producer stream error with both exits pending is blamed on pack-objects", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    let consumer: ChildProcess | undefined;
    const consumerDone = deferred<void>();
    const producerOut = new PassThrough();
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("pack-objects")) {
        // The producer's stream fails first; its exit status arrives only after the consumer's.
        return {
          stdin: openSink(),
          stdout: producerOut,
          stderr: null,
          cancel: async () => {}, completed: consumerDone.promise.then(() => ({ code: 1 })),
        };
      }
      if (request.argv.includes("index-pack")) {
        const real = spawnReal(request);
        consumer = real.child;
        void real.handle.completed.then(() => consumerDone.resolve(), () => consumerDone.resolve());
        setImmediate(() => producerOut.destroy(new Error("pack stream broke")));
        return real.handle;
      }
      return spawnReal(request).handle;
    };
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      10_000,
      () => [consumer],
    );
    assert.match(err.causeMessage, /^pack-objects exited 1: pack stream broke; index-pack exited \d+.* \(stopped after its peer failed\)$/s);
    assert.ok(consumer && childGone(consumer), "the consumer process has exited");
  });

  it("outside a boundary: a broken consumer pipe SIGKILLs a producer that ignores losing its stdout", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    const internals = git as unknown as Internals;
    let producer: ChildProcess | undefined;
    const producerDone = deferred<void>();
    internals.spawnGit = async () => {
      // A real child that never writes and keeps running without its stdout: only a kill ends it.
      const child = spawn("sleep", ["600"], { stdio: ["ignore", "pipe", "ignore"] });
      producer = child;
      const exited = new Promise<number>((resolve) => child.once("close", (code) => resolve(code ?? -1)));
      void exited.then(() => producerDone.resolve());
      return { child, stdout: child.stdout, exited };
    };
    internals.spawnGitAsRunnerWithStdin = async () => {
      const stdin = openSink();
      setImmediate(() => stdin.destroy(Object.assign(new Error("write EPIPE"), { code: "EPIPE" })));
      return {
        stdin,
        exited: producerDone.promise.then(() => ({ code: 1, stderr: "fatal: early EOF" })),
        abort: () => stdin.destroy(),
      };
    };
    const err = await settleWithin(
      importError(git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit])),
      10_000,
      () => [producer],
    );
    assert.match(err.causeMessage, /^index-pack exited 1: fatal: early EOF; pack-objects exited -1 \(stopped after its peer failed\)$/);
    assert.ok(producer && producer.signalCode === "SIGKILL", "the producer was SIGKILLed");
  });

  it("outside a boundary: a producer that ends its stream cleanly then exits nonzero SIGKILLs a still-running consumer", async () => {
    // The pipe completes normally here, so no stream error reaches pipeline: only the producer's
    // exit handler can stop the consumer.
    const { bare, rc, defaultTip } = await setup(SELF);
    const internals = git as unknown as Internals;
    let consumer: ChildProcess | undefined;
    const consumerInputDone = deferred<void>();
    internals.spawnGit = async () => ({
      stdout: Readable.from([]),
      exited: consumerInputDone.promise.then(() => 1),
    });
    internals.spawnGitAsRunnerWithStdin = async () => {
      // A real child that keeps running after its input ends: only a kill ends it.
      const child = spawn("sleep", ["600"], { stdio: ["pipe", "ignore", "ignore"] });
      consumer = child;
      child.stdin.on("error", () => undefined);
      child.stdin.once("finish", () => consumerInputDone.resolve());
      const exited = new Promise<{ code: number; stderr: string }>((resolve) =>
        child.once("close", (code) => resolve({ code: code ?? -1, stderr: "" })));
      return {
        stdin: child.stdin,
        exited,
        abort: () => {
          child.stdin.destroy();
          if (!childGone(child)) child.kill("SIGKILL");
        },
      };
    };
    const err = await settleWithin(
      importError(git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit])),
      10_000,
      () => [consumer],
    );
    assert.strictEqual(err.causeMessage, "pack-objects exited 1; index-pack exited -1 (stopped after its peer failed)");
    assert.ok(consumer && consumer.signalCode === "SIGKILL", "the consumer was SIGKILLed");
  });

  it("a producer that exits nonzero after streaming a usable pack still fails the import", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    const consumerDone = deferred<void>();
    const spawner: BoundaryProcessSpawner = async (request) => {
      const real = spawnReal(request);
      if (request.argv.includes("pack-objects")) {
        // The real producer streams the real pack; only its reported status is forced to 1, and
        // only after the consumer has indexed everything, so the tip is resolvable afterwards.
        const completed = real.handle.completed.then(async () => {
          await consumerDone.promise;
          return { code: 1 };
        });
        return { ...real.handle, completed };
      }
      if (request.argv.includes("index-pack")) {
        void real.handle.completed.then(() => consumerDone.resolve(), () => consumerDone.resolve());
      }
      return real.handle;
    };
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      30_000,
      () => [],
    );
    assert.match(err.causeMessage, /^pack-objects exited 1/);
    assert.ok(resolves(rc.path, defaultTip), "the pack did land: only the producer's status fails the import");
  });

  it("a consumer that indexes the whole pack and exits 0 before the producer's exit is known still imports", async () => {
    // issue #1863: index-pack stops reading at the pack trailer and exits, and Node destroys a
    // child's stdin on its exit, so the consumer's stdin can close (unfinished, no error) while
    // the producer's exit-gated stdout still waits for pack-objects' exit status. Under the
    // Landlock command sandbox that window was wide enough to fail real imports with "pack stream
    // failed: Premature close" although both sides exited 0 and the tip had landed. Here the
    // producer's status is held until the consumer has exited, which makes that order certain.
    const { bare, rc, defaultTip } = await setup(SELF);
    const consumerDone = deferred<void>();
    const spawner: BoundaryProcessSpawner = async (request) => {
      const real = spawnReal(request);
      if (request.argv.includes("pack-objects")) {
        const completed = real.handle.completed.then(async (res) => {
          await consumerDone.promise;
          return res;
        });
        return { ...real.handle, completed };
      }
      if (request.argv.includes("index-pack")) {
        void real.handle.completed.then(() => consumerDone.resolve(), () => consumerDone.resolve());
      }
      return real.handle;
    };
    await settleWithin(
      git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit])),
      30_000,
      () => [],
    );
    assert.ok(resolves(rc.path, defaultTip), "the tip resolves after the import");
  });

  // issue #1863: a consumer stdin that closes with no error no longer fails the pipe, so a
  // consumer that exits 0 WITHOUT indexing the pack passes both exit checks; only the tip
  // re-probe can refuse it. The replacement consumer's stdin forwards to a real child and, like a
  // supervisor transport or Node on a child's exit, closes with no error when that child exits
  // (an EPIPE on a late write is swallowed), so the probe is the only verdict left.
  for (const [label, argv] of [
    ["exits at once", ["true"]],
    ["reads only a prefix", ["sh", "-c", "head -c 40 >/dev/null"]],
  ] as const) {
    it(`inside a boundary: a consumer that ${label} with status 0 without indexing fails the tip probe`, async () => {
      const { bare, rc, defaultTip } = await setup(SELF);
      let consumer: ChildProcess | undefined;
      const spawner: BoundaryProcessSpawner = async (request) => {
        if (!request.argv.includes("index-pack")) return spawnReal(request).handle;
        const real = spawnReal({ ...request, argv: [...argv] });
        consumer = real.child;
        return { ...real.handle, stdin: closeOnExitStdin(real.child) };
      };
      const err = await settleWithin(
        importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
          git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
        30_000,
        () => [consumer],
      );
      assert.strictEqual(err.causeMessage, "the imported pack did not make the tip resolvable in the clone");
      assert.ok(consumer && consumer.exitCode === 0, "the replacement consumer exited 0");
      assert.strictEqual(resolves(rc.path, defaultTip), false, "nothing was indexed");
    });
  }

  it("inside a boundary: a pipe that breaks on the consumer side while both sides exit 0 names the side and both statuses", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    const producerOut = new PassThrough();
    const producerDone = deferred<void>();
    producerOut.once("close", () => producerDone.resolve());
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("pack-objects")) {
        // Holds its output open until the import tears it down, then reports a clean exit.
        return { stdin: openSink(), stdout: producerOut, stderr: null, cancel: async () => {}, completed: producerDone.promise.then(() => ({ code: 0 })) };
      }
      if (request.argv.includes("index-pack")) {
        const stdin = openSink();
        setImmediate(() => stdin.destroy(Object.assign(new Error("write EPIPE"), { code: "EPIPE" })));
        return { stdin, stdout: null, stderr: null, cancel: async () => {}, completed: producerDone.promise.then(() => ({ code: 0 })) };
      }
      return spawnReal(request).handle;
    };
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      10_000,
      () => [],
    );
    assert.strictEqual(err.causeMessage, "pack stream failed (consumer side; pack-objects exited 0, index-pack exited 0): write EPIPE");
  });

  it("inside a boundary: a pipe that breaks on the producer side while both sides exit 0 names the side and both statuses", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    const producerOut = new PassThrough();
    const producerDone = deferred<void>();
    producerOut.once("close", () => producerDone.resolve());
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("pack-objects")) {
        return { stdin: openSink(), stdout: producerOut, stderr: null, cancel: async () => {}, completed: producerDone.promise.then(() => ({ code: 0 })) };
      }
      if (request.argv.includes("index-pack")) {
        setImmediate(() => producerOut.destroy(new Error("pack stream broke")));
        return { stdin: openSink(), stdout: null, stderr: null, cancel: async () => {}, completed: producerDone.promise.then(() => ({ code: 0 })) };
      }
      return spawnReal(request).handle;
    };
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      10_000,
      () => [],
    );
    assert.match(err.causeMessage, /^pack stream failed \(producer side; pack-objects exited 0, index-pack exited 0\): .*pack stream broke/);
  });
});

// issue #1769 (reconciled with #1804) — spawnGit hands back exitGatedStream's PassThrough, piped
// from the child's stdout at once and ended only on a clean exit, on BOTH spawn paths: the
// boundary branch (a supervised handle) and the plain `spawn` branch. These cases pin its
// contract for the finalize import: bytes are never lost to a late consumer, a failure still
// reaches a late reader, backpressure holds with no reader, errors cross in both directions, and
// the import settles with every child gone. Each case runs on both paths.
describe("GitCache spawnGit exit-gated stdout, finalize import (issue #1769)", () => {
  const BIG_MIB = 8;
  const PACK_ARGS = ["pack-objects", "--revs", "--stdout", "-q"];
  const faults: string[] = [];
  const onRejection = (reason: unknown): void => {
    faults.push(`unhandledRejection: ${String(reason)}`);
  };
  const onException = (err: unknown): void => {
    faults.push(`uncaughtException: ${String(err)}`);
  };

  beforeEach(() => {
    faults.length = 0;
    process.on("unhandledRejection", onRejection);
    process.on("uncaughtException", onException);
  });

  // Every real child a case starts, so a failed assertion cannot leave one blocked on a full
  // pipe (which would hold the test process open instead of failing it).
  const live = new Set<ChildProcess>();
  const track = <T extends ChildProcess | undefined>(child: T): T => {
    if (child) live.add(child);
    return child;
  };

  afterEach(async () => {
    const leftover = [...live].filter((c) => !childGone(c));
    for (const c of leftover) c.kill("SIGKILL");
    live.clear();
    // One more turn so a late 'error' emitted on nextTick after the test body still lands here.
    await new Promise((r) => setImmediate(r));
    process.off("unhandledRejection", onRejection);
    process.off("uncaughtException", onException);
    assert.deepStrictEqual(faults, [], "no unhandled rejection or uncaught exception");
    assert.strictEqual(leftover.length, 0, "no child was left running");
  });

  /** A self-contained clone plus a fresh default tip carrying BIG_MIB of incompressible blobs,
   *  so the pack is far larger than any stream buffer (64 KiB highWaterMark, 64 KiB pipe). */
  async function setupBig(): Promise<{ bare: string; rc: RunnerClone; defaultTip: string; revs: string }> {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1, noProofReseed, "run-1", false, undefined, undefined, SELF);
    for (let i = 0; i < BIG_MIB; i++) fs.writeFileSync(path.join(fx.originPath, `blob-${i}.bin`), crypto.randomBytes(1 << 20));
    gitIn(fx.originPath, ["add", "."]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "main gains big blobs"]);
    const defaultTip = await git.fetchDefaultTip(bare, "main");
    return { bare, rc, defaultTip, revs: `${defaultTip}\n--not\n${rc.baseCommit}\n` };
  }

  /** A whole pack: the "PACK" magic and a trailer that is the SHA-1 of every preceding byte. */
  function assertWholePack(buf: Buffer): void {
    assert.ok(buf.length > BIG_MIB << 20, `the pack carries the blobs (${buf.length} bytes)`);
    assert.strictEqual(buf.subarray(0, 4).toString(), "PACK");
    const body = buf.subarray(0, buf.length - 20);
    assert.deepStrictEqual(crypto.createHash("sha1").update(body).digest(), buf.subarray(buf.length - 20));
  }

  async function drain(stream: Readable): Promise<Buffer> {
    const chunks: Buffer[] = [];
    for await (const c of stream) chunks.push(Buffer.isBuffer(c) ? c : Buffer.from(c as string));
    return Buffer.concat(chunks);
  }

  /** Poll until the pipe is full with nobody reading: the source paused by backpressure and the
   *  returned stream holding bytes. Fails (not hangs) after `ms`. */
  async function untilBlocked(source: () => Readable | undefined, out: () => Readable | undefined, ms = 10_000): Promise<void> {
    const deadline = Date.now() + ms;
    for (;;) {
      const s = source();
      const o = out();
      if (s && o && s.readableFlowing === false && o.readableLength > 0) break;
      if (Date.now() > deadline) assert.fail("the producer never blocked on a full pipe");
      await new Promise((r) => setTimeout(r, 20));
    }
    // Let anything in flight land, so the bound below is measured at rest.
    await new Promise((r) => setTimeout(r, 200));
  }

  /** Bytes held by the returned PassThrough (readable plus writable side). With nobody reading it
   *  is bounded by its highWaterMarks plus the few pipe chunks already read in the same turn the
   *  source was paused (measured: up to 4 x 64 KiB on the plain path), however large the pack.
   *  The bound, 512 KiB, is a sixteenth of the pack. Returns the held count. */
  function assertBounded(out: Readable): number {
    const pt = out as PassThrough;
    const held = pt.readableLength + pt.writableLength;
    const bound = 512 * 1024;
    assert.ok(held <= bound, `the returned stream holds ${held} bytes; bound ${bound}`);
    return held;
  }

  /** A boundary spawner running every request for real, recording the pack-objects child and
   *  the source stream the handle exposes. `beforeConsumer` delays the index-pack spawn. */
  function realSpawner(beforeConsumer?: () => Promise<void>): {
    spawner: BoundaryProcessSpawner;
    producer: () => ChildProcess | undefined;
    source: () => Readable | undefined;
  } {
    let producer: ChildProcess | undefined;
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("index-pack") && beforeConsumer) await beforeConsumer();
      const { child, handle } = spawnReal(request);
      track(child);
      if (request.argv.includes("pack-objects")) producer = child;
      return handle;
    };
    return { spawner, producer: () => producer, source: () => producer?.stdout ?? undefined };
  }

  /** Plain path: delay the consumer spawn until `gate(producer)` resolves. */
  function delayPlainConsumer(cache: GitCache, gate: (p: SpawnGitResult) => Promise<void>): { producer: () => SpawnGitResult | undefined } {
    const internals = cache as unknown as Internals;
    let producer: SpawnGitResult | undefined;
    const origProducer = internals.spawnGit.bind(cache);
    const origConsumer = internals.spawnGitAsRunnerWithStdin.bind(cache);
    internals.spawnGit = async (cwd, args, stdin) => {
      producer = await origProducer(cwd, args, stdin);
      track(producer.child);
      return producer;
    };
    internals.spawnGitAsRunnerWithStdin = async (cwd, args) => {
      assert.ok(producer, "the producer spawned first");
      await gate(producer);
      return origConsumer(cwd, args);
    };
    return { producer: () => producer };
  }

  async function turns(n: number): Promise<void> {
    for (let i = 0; i < n; i++) await new Promise((r) => setImmediate(r));
  }

  // (a) A late consumer loses no bytes.

  it("plain spawn: a producer that exits before the consumer has started loses no pack bytes", async () => {
    // The plain-path twin of the boundary case in the m2 block: ChildProcess flushStdio drains an
    // unread stdout on 'exit' whichever branch spawned it.
    const { bare, rc, defaultTip } = await setup(SELF);
    const delayed = delayPlainConsumer(git, async (p) => {
      await p.exited;
      await turns(3);
    });
    await settleWithin(git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]), 30_000, () => [delayed.producer()?.child]);
    assert.ok(resolves(rc.path, defaultTip), "the whole pack reached index-pack");
    assert.ok(childGone(delayed.producer()?.child));
  });

  it("plain spawn: a multi-MiB pack with the consumer started only once the producer blocked imports whole", async () => {
    const { bare, rc, defaultTip } = await setupBig();
    const spy = { out: undefined as Readable | undefined };
    const delayed = delayPlainConsumer(git, async (p) => {
      spy.out = p.stdout;
      await untilBlocked(() => p.child?.stdout ?? undefined, () => p.stdout);
      assert.strictEqual(await settled(p.exited), false, "the producer is blocked, not finished");
      assertBounded(p.stdout);
    });
    await settleWithin(git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]), 60_000, () => [delayed.producer()?.child]);
    assert.ok(resolves(rc.path, defaultTip), "the tip resolves in the clone");
    assert.strictEqual(await delayed.producer()!.exited, 0);
    assert.ok(childGone(delayed.producer()?.child));
  });

  it("inside a boundary: a multi-MiB pack with the consumer started only once the producer blocked imports whole", async () => {
    const { bare, rc, defaultTip } = await setupBig();
    const spy = spyImport(git);
    const real = realSpawner(async () => {
      await untilBlocked(real.source, () => spy.producers[0]?.result.stdout);
      assert.strictEqual(await settled(spy.producers[0]!.result.exited), false, "the producer is blocked, not finished");
      assertBounded(spy.producers[0]!.result.stdout);
    });
    await settleWithin(
      git.withBoundaryProcessSpawner(real.spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit])),
      60_000,
      () => [real.producer()],
    );
    assert.ok(resolves(rc.path, defaultTip), "the tip resolves in the clone");
    assert.strictEqual(await spy.producers[0]!.result.exited, 0);
    assert.ok(childGone(real.producer()));
  });

  // (b) The producer fails before the consumer exists.

  it("plain spawn: a producer that fails before the consumer starts is blamed on pack-objects", async () => {
    const { bare, rc } = await setup(SELF);
    const delayed = delayPlainConsumer(git, async (p) => {
      assert.notStrictEqual(await p.exited, 0);
      await turns(3);
    });
    const err = await settleWithin(
      importError(git.ensureRunnerCloneObjects(bare, rc.path, UNKNOWN_SHA, [rc.baseCommit])),
      30_000,
      () => [delayed.producer()?.child],
    );
    // git's own stderr survives: the exit status is reported on the stream the import reads,
    // even though the source had already ended before the consumer existed.
    assert.match(err.causeMessage, /^pack-objects exited 128: git pack-objects .* exited 128: fatal: /s);
    assert.ok(childGone(delayed.producer()?.child));
  });

  it("inside a boundary: a producer that fails before the consumer starts is blamed on pack-objects", async () => {
    const { bare, rc } = await setup(SELF);
    const spy = spyImport(git);
    const real = realSpawner(async () => {
      assert.notStrictEqual(await spy.producers[0]!.result.exited, 0);
      await turns(3);
    });
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(real.spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, UNKNOWN_SHA, [rc.baseCommit]))),
      30_000,
      () => [real.producer()],
    );
    // git's own stderr survives: the exit status is reported on the stream the import reads,
    // even though the source had already ended before the consumer existed.
    assert.match(err.causeMessage, /^pack-objects exited 128: git pack-objects .* exited 128: fatal: /s);
    assert.ok(childGone(real.producer()));
    assert.ok(await settled(spy.consumers[0]!.exited), "the consumer's exited has settled");
  });

  // (c) The consumer cannot start, or fails at once, while a real producer is blocked mid-pack.

  it("plain spawn: a consumer that cannot start tears down a producer blocked mid-pack", async () => {
    const { bare, rc, defaultTip } = await setupBig();
    const internals = git as unknown as Internals;
    let producer: SpawnGitResult | undefined;
    const orig = internals.spawnGit.bind(git);
    internals.spawnGit = async (cwd, args, stdin) => {
      producer = await orig(cwd, args, stdin);
      track(producer.child);
      return producer;
    };
    internals.spawnGitAsRunnerWithStdin = async () => {
      await untilBlocked(() => producer?.child?.stdout ?? undefined, () => producer?.stdout);
      throw new Error("runner spawn refused");
    };
    const err = await settleWithin(
      importError(git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit])),
      30_000,
      () => [producer?.child],
    );
    assert.match(err.causeMessage, /^index-pack could not start: .*runner spawn refused/);
    assert.ok(childGone(producer?.child), "the producer has exited");
    assert.ok(await settled(producer!.exited));
    assert.strictEqual(resolves(rc.path, defaultTip), false, "nothing was imported");
  });

  it("inside a boundary: a consumer that cannot start tears down a producer blocked mid-pack", async () => {
    // A boundary handle cannot be signalled: only destroying the returned stream, which must
    // reach the child's real stdout, makes pack-objects hit EPIPE and exit.
    const { bare, rc, defaultTip } = await setupBig();
    const spy = spyImport(git);
    let producer: ChildProcess | undefined;
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("index-pack")) {
        await untilBlocked(() => producer?.stdout ?? undefined, () => spy.producers[0]?.result.stdout);
        throw new Error("supervisor refused the command root");
      }
      const { child, handle } = spawnReal(request);
      track(child);
      if (request.argv.includes("pack-objects")) producer = child;
      return handle;
    };
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      30_000,
      () => [producer],
    );
    assert.match(err.causeMessage, /^index-pack could not start: .*supervisor refused the command root/);
    assert.ok(childGone(producer), "the producer process has exited");
    assert.ok(producer!.stdout!.destroyed, "the child's real stdout was destroyed through the returned stream");
    assert.ok(await settled(spy.producers[0]!.result.exited));
  });

  it("plain spawn: a consumer that exits at once tears down a producer blocked mid-pack", async () => {
    const { bare, rc, defaultTip } = await setupBig();
    const internals = git as unknown as Internals;
    let producer: SpawnGitResult | undefined;
    let consumer: ConsumerResult | undefined;
    const origProducer = internals.spawnGit.bind(git);
    const origConsumer = internals.spawnGitAsRunnerWithStdin.bind(git);
    internals.spawnGit = async (cwd, args, stdin) => {
      producer = await origProducer(cwd, args, stdin);
      track(producer.child);
      return producer;
    };
    internals.spawnGitAsRunnerWithStdin = async (_cwd, args) => {
      await untilBlocked(() => producer?.child?.stdout ?? undefined, () => producer?.stdout);
      // The real index-pack, pointed at a directory that does not exist: it exits 128 at once.
      consumer = await origConsumer(path.join(fx.dataDir, "no-such-clone"), args);
      return consumer;
    };
    const err = await settleWithin(
      importError(git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit])),
      30_000,
      () => [producer?.child],
    );
    assert.match(err.causeMessage, /^index-pack exited 128: .*; pack-objects exited -?\d+ \(stopped after its peer failed\)$/s);
    assert.ok(childGone(producer?.child), "the producer has exited");
    assert.ok(await settled(consumer!.exited));
  });

  it("inside a boundary: a consumer that exits at once tears down a producer blocked mid-pack", async () => {
    const { bare, rc, defaultTip } = await setupBig();
    const spy = spyImport(git);
    let producer: ChildProcess | undefined;
    const missing = path.join(fx.dataDir, "no-such-clone");
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("index-pack")) {
        await untilBlocked(() => producer?.stdout ?? undefined, () => spy.producers[0]?.result.stdout);
        const argv = request.argv.map((a) => (a === rc.path ? missing : a));
        const real = spawnReal({ ...request, argv });
        track(real.child);
        return real.handle;
      }
      const { child, handle } = spawnReal(request);
      track(child);
      if (request.argv.includes("pack-objects")) producer = child;
      return handle;
    };
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      30_000,
      () => [producer],
    );
    assert.match(err.causeMessage, /^index-pack exited 128: .*; pack-objects exited -?\d+ \(stopped after its peer failed\)$/s);
    assert.ok(childGone(producer), "the producer process has exited");
    assert.ok(await settled(spy.producers[0]!.result.exited));
    assert.ok(await settled(spy.consumers[0]!.exited));
  });

  // (d) Backpressure with no reader, then a full drain.

  it("plain spawn: with no reader the returned stream stays bounded, then drains a whole pack", async () => {
    const { bare, revs } = await setupBig();
    const res = await (git as unknown as Internals).spawnGit(bare, PACK_ARGS, revs);
    track(res.child);
    await untilBlocked(() => res.child?.stdout ?? undefined, () => res.stdout);
    assert.strictEqual(await settled(res.exited), false, "the producer is blocked on the full pipe");
    const held = assertBounded(res.stdout);
    await new Promise((r) => setTimeout(r, 300));
    assert.strictEqual(assertBounded(res.stdout), held, "nothing more was buffered while nobody read");
    assertWholePack(await settleWithin(drain(res.stdout), 30_000, () => [res.child]));
    assert.strictEqual(await res.exited, 0);
    assert.ok(childGone(res.child));
  });

  it("inside a boundary: with no reader the returned stream stays bounded, then drains a whole pack", async () => {
    const { bare, revs } = await setupBig();
    const real = realSpawner();
    const res = await git.withBoundaryProcessSpawner(real.spawner, new AbortController().signal, () =>
      (git as unknown as Internals).spawnGit(bare, PACK_ARGS, revs));
    await untilBlocked(real.source, () => res.stdout);
    assert.strictEqual(await settled(res.exited), false, "the producer is blocked on the full pipe");
    const held = assertBounded(res.stdout);
    await new Promise((r) => setTimeout(r, 300));
    assert.strictEqual(assertBounded(res.stdout), held, "nothing more was buffered while nobody read");
    assertWholePack(await settleWithin(drain(res.stdout), 30_000, () => [real.producer()]));
    assert.strictEqual(await res.exited, 0);
    assert.ok(childGone(real.producer()));
  });

  // (e) Errors cross in both directions.

  it("plain spawn: a source error errors the returned stream with that error", async () => {
    const { bare, revs } = await setupBig();
    const res = await (git as unknown as Internals).spawnGit(bare, PACK_ARGS, revs);
    track(res.child);
    await untilBlocked(() => res.child?.stdout ?? undefined, () => res.stdout);
    const boom = new Error("source broke");
    const errored = once(res.stdout, "error");
    res.child!.stdout!.destroy(boom);
    const [err] = await settleWithin(errored, 10_000, () => [res.child]);
    assert.strictEqual(err, boom);
    assert.notStrictEqual(await settleWithin(res.exited, 10_000, () => [res.child]), 0, "pack-objects died on the lost pipe");
    assert.ok(childGone(res.child));
  });

  it("plain spawn: destroying the returned stream destroys the source and ends the producer", async () => {
    const { bare, revs } = await setupBig();
    const res = await (git as unknown as Internals).spawnGit(bare, PACK_ARGS, revs);
    track(res.child);
    await untilBlocked(() => res.child?.stdout ?? undefined, () => res.stdout);
    res.stdout.destroy();
    assert.notStrictEqual(await settleWithin(res.exited, 10_000, () => [res.child]), 0);
    assert.ok(res.child!.stdout!.destroyed, "the child's stdout was destroyed");
    assert.ok(childGone(res.child));
  });

  it("inside a boundary: a source error errors the returned stream with that error", async () => {
    const source = new PassThrough();
    const done = deferred<{ code: number }>();
    const spawner: BoundaryProcessSpawner = async () => ({ stdin: openSink(), stdout: source, stderr: null, cancel: async () => {}, completed: done.promise });
    const res = await git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
      (git as unknown as Internals).spawnGit(fx.dataDir, PACK_ARGS, ""));
    source.write("partial");
    const boom = new Error("transport broke");
    const errored = once(res.stdout, "error");
    source.destroy(boom);
    const [err] = await settleWithin(errored, 10_000, () => []);
    assert.strictEqual(err, boom);
    done.resolve({ code: 1 });
    assert.strictEqual(await settleWithin(res.exited, 10_000, () => []), 1);
  });

  // A source that CLOSES without 'end' or 'error' (a premature close) under a clean exit: the
  // returned stream must error, not wait forever for an end that never comes.

  it("inside a boundary: a source that closes before its end errors the returned stream", async () => {
    const source = new PassThrough();
    const done = deferred<{ code: number }>();
    const spawner: BoundaryProcessSpawner = async () => ({ stdin: openSink(), stdout: source, stderr: null, cancel: async () => {}, completed: done.promise });
    const res = await git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
      (git as unknown as Internals).spawnGit(fx.dataDir, PACK_ARGS, ""));
    source.write("partial");
    const errored = once(res.stdout, "error");
    source.destroy();
    done.resolve({ code: 0 });
    assert.strictEqual(await settleWithin(res.exited, 10_000, () => []), 0);
    const [err] = await settleWithin(errored, 10_000, () => []);
    assert.match(String(err), /premature close/);
    assert.ok(res.stdout.destroyed, "the returned stream is destroyed, never ended cleanly");
    assert.strictEqual(res.stdout.readableEnded, false);
  });

  it("inside a boundary: a source that closes before its end fails the import instead of hanging", async () => {
    const { bare, rc, defaultTip } = await setup(SELF);
    const source = new PassThrough();
    const done = deferred<{ code: number }>();
    let consumer: ChildProcess | undefined;
    const spawner: BoundaryProcessSpawner = async (request) => {
      if (request.argv.includes("pack-objects")) {
        return { stdin: openSink(), stdout: source, stderr: null, cancel: async () => {}, completed: done.promise };
      }
      const { child, handle } = spawnReal(request);
      track(child);
      if (request.argv.includes("index-pack")) {
        consumer = child;
        // Once the pipe is wired: the source closes with no end and no error, the exit is clean.
        setImmediate(() => {
          source.destroy();
          done.resolve({ code: 0 });
        });
      }
      return handle;
    };
    const err = await settleWithin(
      importError(git.withBoundaryProcessSpawner(spawner, new AbortController().signal, () =>
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, [rc.baseCommit]))),
      10_000,
      () => [consumer],
    );
    assert.match(err.causeMessage, /^index-pack exited \d+.* \(stopped after its peer failed\)$/s);
    assert.ok(consumer && childGone(consumer), "the consumer process has exited");
    assert.strictEqual(resolves(rc.path, defaultTip), false, "nothing was imported");
  });

  it("inside a boundary: destroying the returned stream destroys the source", async () => {
    const { bare, revs } = await setupBig();
    const real = realSpawner();
    const res = await git.withBoundaryProcessSpawner(real.spawner, new AbortController().signal, () =>
      (git as unknown as Internals).spawnGit(bare, PACK_ARGS, revs));
    await untilBlocked(real.source, () => res.stdout);
    res.stdout.destroy();
    // No kill is available through a boundary handle: the destroyed source is what ends it.
    assert.notStrictEqual(await settleWithin(res.exited, 10_000, () => [real.producer()]), 0);
    assert.ok(real.source()!.destroyed, "the child's stdout was destroyed");
    assert.ok(childGone(real.producer()));
  });

  // A nonzero exit that lands after the source's EOF, before anybody reads (the checkpoint
  // publish upload's shape too): the reader still sees git's failure, never a clean end.

  it("plain spawn: a nonzero exit reaches a reader that starts after the producer is gone", async () => {
    const { bare } = await setup(SELF);
    const res = await (git as unknown as Internals).spawnGit(bare, PACK_ARGS, `${UNKNOWN_SHA}\n`);
    track(res.child);
    assert.strictEqual(await res.exited, 128);
    await turns(3);
    await assert.rejects(settleWithin(drain(res.stdout), 10_000, () => [res.child]), /pack-objects .*exited 128: fatal: /s);
  });

  it("inside a boundary: a nonzero exit reaches a reader that starts after the producer is gone", async () => {
    const { bare } = await setup(SELF);
    const real = realSpawner();
    const res = await git.withBoundaryProcessSpawner(real.spawner, new AbortController().signal, () =>
      (git as unknown as Internals).spawnGit(bare, PACK_ARGS, `${UNKNOWN_SHA}\n`));
    assert.strictEqual(await res.exited, 128);
    await turns(3);
    await assert.rejects(settleWithin(drain(res.stdout), 10_000, () => [real.producer()]), /pack-objects .*exited 128: fatal: /s);
  });
});
