import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { PassThrough, Readable, Writable } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
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
  git = new GitCache(fx.dataDir, nullLogger());
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
  const rc = await git.createOrAttachRunnerClone(bare, 1, "run-1", false, undefined, opts);
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
  return { child, handle: { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed } };
}

/** A producer that STALLS: a real process that writes nothing and never exits on its own. It is
 *  ended only by losing its stdout (the EPIPE its next write would get, collapsed to "at once"),
 *  so it stops only if the import destroys that stream. `code` is its exit status once killed. */
function stalledProducer(): { child: ChildProcess; handle: BoundaryProcessHandle } {
  const child = spawn("sleep", ["600"], { stdio: ["pipe", "pipe", "pipe"] });
  child.stdout.once("close", () => child.kill("SIGKILL"));
  const completed = new Promise<{ code: number }>((resolve) => child.once("close", (code) => resolve({ code: code ?? 137 })));
  return { child, handle: { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed } };
}

/** A stdin that accepts and discards everything and never closes on its own (a supervisor
 *  transport need not close the pipe when its child exits). */
function openSink(): Writable {
  return new Writable({ write: (_chunk, _enc, cb) => cb() });
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
    const self = await git.createOrAttachRunnerClone(shared.bare, 2, "run-2", false, undefined, SELF);
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
      return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed };
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
          completed: new Promise((resolve) => setImmediate(() => resolve({ code: 1 }))),
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
          completed: new Promise((resolve) => setImmediate(() => resolve({ code: 1 }))),
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
          completed: producerDone.promise.then(() => ({ code: 1 })),
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
          completed: consumerDone.promise.then(() => ({ code: 1 })),
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
});
