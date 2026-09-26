import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { GitCache, RunnerCloneImportError, type BoundaryProcessSpawner, type RunnerClone } from "../src/git.js";

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
});
