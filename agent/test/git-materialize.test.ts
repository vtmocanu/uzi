import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache, RunnerCloneMaterializationError, WIP_PARK_COMMIT_PREFIX, materializeEnv, type AttemptSeedOptions, type RunnerClone } from "../src/git.js";
import { mintAttemptId } from "../src/run-quiescence.js";

// issue #1769 m1 — a Codex (sandboxed) run's runner clone is made SELF-CONTAINED at seed:
// the objects it borrowed from the worker bare through `objects/info/alternates` are copied
// in and the alternates file retired, because the Codex command sandbox does not grant the
// bare. Real temp repos throughout; single-uid, so runnerCommand is a passthrough.

// No alternate-object env leaks into the fixture's own git: every "the clone works without
// the bare" assertion below must be proven by the clone alone.
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

let fx: Fixture;

beforeEach(() => {
  fx = makeFixture();
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

function commit(dir: string, file: string): string {
  fs.writeFileSync(path.join(dir, file), `${file}\n`);
  gitIn(dir, ["add", file]);
  gitIn(dir, [...IDENT, "commit", "-m", `work ${file}`]);
  return gitIn(dir, ["rev-parse", "HEAD"]);
}

function alternatesPath(clone: string): string {
  return path.join(clone, ".git", "objects", "info", "alternates");
}

function parkedPath(clone: string): string {
  return path.join(clone, ".git", "objects", "info", "alternates.uzi-materialize");
}

function refsUnder(clone: string, prefix: string): string {
  return gitIn(clone, ["for-each-ref", "--format=%(refname)", prefix]);
}

function worker(name: string, logger = nullLogger()): GitCache {
  const dataDir = path.join(fx.dataDir, name);
  fs.mkdirSync(dataDir, { recursive: true });
  return new GitCache(dataDir, logger, undefined, testGitCacheOptions());
}

/** Move the worker bare out of the way, so any object still borrowed from it is unreadable. */
function hideBare(bare: string): void {
  fs.renameSync(bare, `${bare}.hidden`);
}

/** The clone is usable on its own: status, log, fsck and a fresh commit all succeed. */
function assertStandalone(clone: string, rc: RunnerClone): void {
  assert.strictEqual(fs.existsSync(alternatesPath(clone)), false, "no alternates file");
  assert.strictEqual(fs.existsSync(parkedPath(clone)), false, "no parked alternates file");
  gitIn(clone, ["status", "--porcelain"]);
  assert.ok(gitIn(clone, ["log", "--format=%H"]).length > 0, "git log walks the history");
  gitIn(clone, ["fsck", "--connectivity-only", "--no-dangling", "--no-progress"]);
  for (const sha of [rc.baseCommit, rc.defaultBranchCommit]) {
    if (sha) assert.ok(resolves(clone, sha), `${sha} resolves without the bare`);
  }
  assert.strictEqual(refsUnder(clone, "refs/uzi-materialize/"), "", "no anchor refs remain");
  assert.strictEqual(refsUnder(clone, "refs/uzi-checkpoints/"), "", "no checkpoint refs in the clone");
  const before = gitIn(clone, ["rev-parse", "HEAD"]);
  const after = commit(clone, `AFTER-${Date.now()}.txt`);
  assert.notStrictEqual(after, before, "a new commit lands without the bare");
}

type ExecArgs = [string, string[], { env: NodeJS.ProcessEnv }, string?];
type Exec = (...a: ExecArgs) => Promise<{ stdout: string; stderr: string }>;

/** Wrap the private exec seam on one instance (TS-private, so a cast reaches it). */
function wrapExec(cache: GitCache, wrap: (orig: Exec, ...a: ExecArgs) => Promise<{ stdout: string; stderr: string }>): void {
  const internals = cache as unknown as { execScoped: Exec };
  const orig = internals.execScoped.bind(cache) as Exec;
  internals.execScoped = (...a: ExecArgs) => wrap(orig, ...a);
}

describe("runner clone materialization (issue #1769 m1)", () => {
  it("a fresh selfContained seed has no alternates and works with the bare gone", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1, noProofReseed, "run-1", false, undefined, undefined, SELF);
    assert.strictEqual(rc.seededFrom, "default");
    hideBare(bare);
    assertStandalone(rc.path, rc);
  });

  it("without the option the clone keeps its alternates (Claude path unchanged)", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 2, noProofReseed, "run-2");
    assert.strictEqual(fs.existsSync(alternatesPath(rc.path)), true, "alternates present");
    const explicitFalse = await git.createOrAttachRunnerClone(bare, 3, noProofReseed, "run-3", false, undefined, undefined, { selfContained: false });
    assert.strictEqual(fs.existsSync(alternatesPath(explicitFalse.path)), true, "selfContained:false keeps alternates");
  });

  it("owned tracking leg: recovered commits resolve with the bare gone", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const seed = await git.createOrAttachRunnerClone(bare, 10, noProofReseed, "run-A");
    const work = commit(seed.path, "WORK.txt");
    await git.fetchAgentBranch(bare, seed.path, "agent/issue-10", "run-A");
    const rc = await git.createOrAttachRunnerClone(bare, 10, noProofReseed, "run-A", true, undefined, undefined, SELF);
    assert.strictEqual(rc.seededFrom, "tracking");
    assert.strictEqual(rc.baseCommit, work);
    hideBare(bare);
    assert.ok(resolves(rc.path, work), "the recovered commit resolves");
    assertStandalone(rc.path, rc);
  });

  it("wip(park) marker leg: reset --soft onto the marker parent, staged WIP survives the bare going away", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const seed = await git.createOrAttachRunnerClone(bare, 11, noProofReseed, "run-A");
    const forkPoint = gitIn(seed.path, ["rev-parse", "HEAD"]);
    fs.writeFileSync(path.join(seed.path, "WIP.txt"), "in-progress work\n");
    assert.strictEqual(await git.commitWipMarker(seed.path), true);
    const marker = gitIn(seed.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, seed.path, "agent/issue-11", "run-A");

    const rc = await git.createOrAttachRunnerClone(bare, 11, noProofReseed, "run-A", true, undefined, undefined, SELF);
    assert.strictEqual(rc.wipRecovered, true);
    assert.strictEqual(rc.baseCommit, forkPoint);
    hideBare(bare);
    assert.ok(resolves(rc.path, marker), "the adopted marker (baseSha) resolves");
    assert.ok(resolves(rc.path, forkPoint), "the marker parent resolves");
    assert.match(gitIn(rc.path, ["status", "--porcelain"]), /WIP\.txt/, "the WIP is still staged");
    assert.strictEqual(fs.readFileSync(path.join(rc.path, "WIP.txt"), "utf8"), "in-progress work\n");
    assertStandalone(rc.path, rc);
  });

  it("owner-matched diverged checkpoint leg: the cherry-picked marker SHA resolves with the bare gone", async () => {
    const branch = "agent/issue-12";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const floor0 = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    // Worker A: a wip(park) marker over the pre-park floor, published as the checkpoint.
    const seed = await gitA.createOrAttachRunnerClone(bareA, 12, noProofReseed, "run-A");
    fs.writeFileSync(path.join(seed.path, "WIP.txt"), "diverged wip\n");
    assert.strictEqual(await gitA.commitWipMarker(seed.path), true);
    const marker = gitIn(seed.path, ["rev-parse", "HEAD"]);
    assert.ok(gitIn(seed.path, ["log", "-1", "--format=%s"]).startsWith(WIP_PARK_COMMIT_PREFIX));
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");
    const packed = await gitA.checkpointPack(bareA, branch);
    assert.ok(packed);
    const pack = await drain(packed!.pack);
    execFileSync("git", ["-C", fx.originPath, "index-pack", "--stdin", "--fix-thin"], {
      env: GIT_ENV,
      input: pack,
      stdio: ["pipe", "pipe", "pipe"],
    });
    gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, marker]);
    // A published origin/<branch> off the same floor on a disjoint file: the checkpoint diverges.
    const originTip = makeOriginBranch(branch, floor0, "ORIGIN_WORK.txt", "origin work\n");

    const { logger, lines } = recordingLogger();
    const gitB = worker("workerB", logger);
    const bareB = await gitB.ensureClone(fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 12, noProofReseed, "run-B", true, marker, undefined, SELF);
    assert.strictEqual(rc.wipRecovered, true, "the diverged WIP was cherry-picked");
    assert.strictEqual(rc.baseCommit, originTip);
    assert.ok(
      lines.some((l) => (l as { msg?: string }).msg === "runner clone: materialized (self-contained)"),
      "materialization ran",
    );
    hideBare(bareB);
    assert.ok(resolves(rc.path, marker), "the checkpoint marker SHA resolves");
    assert.ok(resolves(rc.path, originTip), "the origin/<branch> SHA resolves");
    assert.match(gitIn(rc.path, ["status", "--porcelain"]), /WIP\.txt/);
    assertStandalone(rc.path, rc);
  });

  it("origin/<branch> leg: the published branch SHA resolves with the bare gone", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const first = await git.createOrAttachRunnerClone(bare, 13, noProofReseed, "run-1");
    const pushed = commit(first.path, "PUSHED.txt");
    await git.fetchAgentBranch(bare, first.path, "agent/issue-13", "run-1");
    await git.pushBranch(bare, "agent/issue-13", "", fx.originPath);
    await git.removeRunnerClone(first.path);
    await git.ensureClone(fx.originPath);
    assert.strictEqual(await git.originBranchTip(bare, "agent/issue-13"), pushed);

    const rc = await git.createOrAttachRunnerClone(bare, 13, noProofReseed, "run-2", true, undefined, undefined, SELF);
    assert.strictEqual(rc.seededFrom, "origin");
    hideBare(bare);
    assert.ok(resolves(rc.path, pushed), "origin/<branch> resolves");
    assertStandalone(rc.path, rc);
  });

  it("a failing repack throws RunnerCloneMaterializationError and leaves the alternates byte-identical", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    let snapshot: Buffer | undefined;
    let clonePath = "";
    wrapExec(git, async (orig, command, args, options, identity) => {
      if (args.includes("repack")) {
        clonePath = args[args.indexOf("-C") + 1]!;
        snapshot = fs.readFileSync(alternatesPath(clonePath));
        throw Object.assign(new Error("exit 1"), { stderr: "injected repack failure" });
      }
      return orig(command, args, options, identity);
    });
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, 20, noProofReseed, "run-1", false, undefined, undefined, SELF),
      (err: unknown) =>
        err instanceof RunnerCloneMaterializationError &&
        err.name === "RunnerCloneMaterializationError" &&
        err.clonePath === clonePath &&
        /injected repack failure/.test(err.causeMessage),
    );
    assert.ok(snapshot, "repack was attempted");
    assert.deepStrictEqual(fs.readFileSync(alternatesPath(clonePath)), snapshot, "alternates byte-identical");
    assert.strictEqual(fs.existsSync(parkedPath(clonePath)), false, "no parked alternates");
    assert.strictEqual(refsUnder(clonePath, "refs/uzi-materialize/"), "", "anchors removed");
  });

  it("a failing verification (fsck) restores the renamed alternates byte-identical", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    let snapshot: Buffer | undefined;
    let clonePath = "";
    let parkedDuringFsck = false;
    wrapExec(git, async (orig, command, args, options, identity) => {
      if (args.includes("repack")) {
        clonePath = args[args.indexOf("-C") + 1]!;
        snapshot = fs.readFileSync(alternatesPath(clonePath));
      }
      if (args.includes("fsck")) {
        parkedDuringFsck = fs.existsSync(parkedPath(clonePath)) && !fs.existsSync(alternatesPath(clonePath));
        throw Object.assign(new Error("exit 1"), { stderr: "injected fsck failure" });
      }
      return orig(command, args, options, identity);
    });
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, 21, noProofReseed, "run-1", false, undefined, undefined, SELF),
      (err: unknown) => err instanceof RunnerCloneMaterializationError && /injected fsck failure/.test(err.message),
    );
    assert.strictEqual(parkedDuringFsck, true, "verification ran with the alternates renamed aside");
    assert.deepStrictEqual(fs.readFileSync(alternatesPath(clonePath)), snapshot, "alternates restored byte-identical");
    assert.strictEqual(fs.existsSync(parkedPath(clonePath)), false, "no parked alternates");
    assert.strictEqual(refsUnder(clonePath, "refs/uzi-materialize/"), "", "anchors removed");
  });

  it("materializeEnv strips alternate-object variables from the base env and keeps the rest", () => {
    const base: NodeJS.ProcessEnv = {
      GIT_ALTERNATE_OBJECT_DIRECTORIES: "/data/bare.git/objects",
      GIT_OBJECT_DIRECTORY: "/data/bare.git/objects",
      GIT_TERMINAL_PROMPT: "0",
      HOME: "/nonexistent",
    };
    const env = materializeEnv(base);
    assert.strictEqual("GIT_ALTERNATE_OBJECT_DIRECTORIES" in env, false);
    assert.strictEqual("GIT_OBJECT_DIRECTORY" in env, false);
    assert.strictEqual(env.GIT_TERMINAL_PROMPT, "0");
    assert.strictEqual(env.HOME, "/nonexistent");
    assert.ok(env.PATH, "the runner PATH is set");
    assert.strictEqual(base.GIT_OBJECT_DIRECTORY, "/data/bare.git/objects", "the base env is not mutated");
  });

  it("an alternates probe error other than ENOENT fails closed without running git", async () => {
    const git = worker("w");
    const clone = path.join(fx.dataDir, "probe-clone");
    fs.mkdirSync(path.join(clone, ".git", "objects"), { recursive: true });
    // objects/info is a FILE, so lstat(objects/info/alternates) fails with ENOTDIR, not ENOENT.
    fs.writeFileSync(path.join(clone, ".git", "objects", "info"), "");
    let spawned = 0;
    wrapExec(git, async (orig, ...a) => {
      spawned += 1;
      return orig(...a);
    });
    const internals = git as unknown as { materializeRunnerClone(p: string, shas: string[]): Promise<void> };
    await assert.rejects(
      internals.materializeRunnerClone(clone, []),
      (err: unknown) => err instanceof RunnerCloneMaterializationError && /ENOTDIR/.test(err.causeMessage),
    );
    assert.strictEqual(spawned, 0, "no subprocess after a failed probe");
  });

  it("a cleanup failure after verification is logged, not thrown: the seed is still self-contained", async () => {
    const { logger, lines } = recordingLogger();
    const git = worker("w", logger);
    const bare = await git.ensureClone(fx.originPath);
    wrapExec(git, async (orig, command, args, options, identity) => {
      if (args.includes("/bin/rm") || command === "/bin/rm") {
        throw Object.assign(new Error("exit 1"), { stderr: "injected rm failure" });
      }
      return orig(command, args, options, identity);
    });
    const rc = await git.createOrAttachRunnerClone(bare, 26, noProofReseed, "run-1", false, undefined, undefined, SELF);
    assert.strictEqual(fs.existsSync(alternatesPath(rc.path)), false, "alternates retired");
    assert.strictEqual(fs.existsSync(parkedPath(rc.path)), true, "the parked file is left behind");
    assert.ok(lines.some((l) => /could not remove the parked alternates file/.test(String((l as { msg?: string }).msg))));
    assert.ok(lines.some((l) => (l as { msg?: string }).msg === "runner clone: materialized (self-contained)"));
    hideBare(bare);
    assert.ok(resolves(rc.path, rc.baseCommit), "the base resolves without the bare");
  });

  it("the worker fetch-back works from a materialized clone after the agent commits", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 27, noProofReseed, "run-1", false, undefined, undefined, SELF);
    assert.strictEqual(fs.existsSync(alternatesPath(rc.path)), false);
    const work = commit(rc.path, "FETCHBACK.txt");
    const dst = await git.fetchAgentBranch(bare, rc.path, "agent/issue-27", "run-1");
    assert.strictEqual(gitIn(bare, ["rev-parse", dst]), work, "the bare tracking ref is the agent commit");
    assert.ok(resolves(bare, work), "the agent commit landed in the bare");
  });

  it("the helper is a no-op on an already self-contained clone", async () => {
    const { logger, lines } = recordingLogger();
    const git = worker("w", logger);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 23, noProofReseed, "run-1", false, undefined, undefined, SELF);
    let spawned = 0;
    wrapExec(git, async (orig, ...a) => {
      spawned += 1;
      return orig(...a);
    });
    const internals = git as unknown as { materializeRunnerClone(p: string, shas: string[]): Promise<void> };
    await internals.materializeRunnerClone(rc.path, [rc.baseCommit]);
    assert.strictEqual(spawned, 0, "no subprocess for a clone without alternates");
    assert.ok(lines.some((l) => /already self-contained/.test(String((l as { msg?: string }).msg))));
  });

  it("an operation queued on the bare lock during the seed starts only after materialization", async () => {
    const { logger, lines } = recordingLogger();
    const git = worker("w", logger);
    const bare = await git.ensureClone(fx.originPath);
    const seed = git.createOrAttachRunnerClone(bare, 24, noProofReseed, "run-1", false, undefined, undefined, SELF);
    let sawMaterialized = false;
    let seedSettled = false;
    void seed.then(() => { seedSettled = true; }, () => { seedSettled = true; });
    const queued = git.withBareLock(bare, async () => {
      sawMaterialized = lines.some(
        (l) => (l as { msg?: string }).msg === "runner clone: materialized (self-contained)",
      );
      return seedSettled;
    });
    const [rc] = await Promise.all([seed, queued]);
    assert.strictEqual(sawMaterialized, true, "materialization finished before the queued op started");
    assert.strictEqual(fs.existsSync(alternatesPath(rc.path)), false);
  });

  it("refuses to run inside a permit-held boundary", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 25, noProofReseed, "run-1");
    const internals = git as unknown as { materializeRunnerClone(p: string, shas: string[]): Promise<void> };
    await assert.rejects(
      git.withBoundaryProcessSpawner(
        async () => { throw new Error("no subprocess expected"); },
        new AbortController().signal,
        () => internals.materializeRunnerClone(rc.path, [rc.baseCommit]),
      ),
      RunnerCloneMaterializationError,
    );
    assert.strictEqual(fs.existsSync(alternatesPath(rc.path)), true, "alternates untouched");
  });
});

// issue #1783 M2 x #1769 — a Docker-wired worker seeds every execution attempt at a FRESH
// `<key>.attempt-<id>` path (attemptCloneForBranch), not the canonical one. A Codex run there must
// get a self-contained clone exactly like the canonical seed: the option has to reach the attempt
// seed too, or the Codex command sandbox is handed a clone whose objects live in the bare.

function attemptOpts(claimGeneration: number): AttemptSeedOptions {
  return {
    attemptId: mintAttemptId(claimGeneration),
    isLive: () => false,
    beforeSeed: async () => {},
    quiescent: async () => true,
  };
}

describe("attempt-path seed materialization (issue #1783 M2 x #1769)", () => {
  it("a fresh Docker-wired attempt seed with selfContained has no alternates and works with the bare gone", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const attempt = attemptOpts(1);
    const rc = await git.runnerCloneForBranch(bare, "agent/issue-40", "issue-40", noProofReseed, "run-1", false, undefined, attempt, SELF);
    assert.strictEqual(rc.attemptId, attempt.attemptId);
    assert.ok(rc.path.endsWith(`issue-40.attempt-${attempt.attemptId}`), `an attempt path: ${rc.path}`);
    hideBare(bare);
    assertStandalone(rc.path, rc);
  });

  it("a Docker-wired RESUME (a second attempt of the same run) seeds a self-contained attempt clone off the tracking ref", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const first = await git.runnerCloneForBranch(bare, "agent/issue-41", "issue-41", noProofReseed, "run-A", false, undefined, attemptOpts(1), SELF);
    const work = commit(first.path, "WORK.txt");
    await git.fetchAgentBranch(bare, first.path, "agent/issue-41", "run-A");
    const second = attemptOpts(2);
    const rc = await git.runnerCloneForBranch(bare, "agent/issue-41", "issue-41", noProofReseed, "run-A", true, undefined, second, SELF);
    assert.notStrictEqual(rc.path, first.path, "the resume seeds a fresh attempt path");
    assert.strictEqual(rc.attemptId, second.attemptId);
    assert.strictEqual(rc.seededFrom, "tracking");
    assert.strictEqual(rc.baseCommit, work);
    hideBare(bare);
    assert.ok(resolves(rc.path, work), "the recovered commit resolves without the bare");
    assertStandalone(rc.path, rc);
  });

  it("without the option a Docker-wired attempt seed keeps its alternates (Claude path unchanged)", async () => {
    const git = worker("w");
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, "agent/issue-42", "issue-42", noProofReseed, "run-1", false, undefined, attemptOpts(1));
    assert.strictEqual(fs.existsSync(alternatesPath(rc.path)), true, "alternates present");
  });
});

async function drain(r: Readable): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const c of r) chunks.push(c as Buffer);
  return Buffer.concat(chunks);
}

/** refs/heads/<branch> in the fixture origin at a new commit over `parent` adding `file`
 *  (plumbing + a throwaway index, so origin's working tree is untouched). */
function makeOriginBranch(branch: string, parent: string, file: string, content: string): string {
  const idx = path.join(fx.originPath, ".git", `tmp-index-${branch.replace(/[^a-zA-Z0-9]/g, "_")}`);
  const env = { ...GIT_ENV, GIT_INDEX_FILE: idx };
  execFileSync("git", ["-C", fx.originPath, "read-tree", parent], { env });
  const blob = execFileSync("git", ["-C", fx.originPath, "hash-object", "-w", "--stdin"], {
    env: GIT_ENV,
    input: content,
    encoding: "utf8",
  }).trim();
  execFileSync("git", ["-C", fx.originPath, "update-index", "--add", "--cacheinfo", `100644,${blob},${file}`], { env });
  const tree = execFileSync("git", ["-C", fx.originPath, "write-tree"], { env, encoding: "utf8" }).trim();
  const sha = execFileSync(
    "git",
    ["-C", fx.originPath, ...IDENT, "commit-tree", tree, "-p", parent, "-m", `origin work ${file}`],
    { env: GIT_ENV, encoding: "utf8" },
  ).trim();
  gitIn(fx.originPath, ["update-ref", `refs/heads/${branch}`, sha]);
  fs.rmSync(idx, { force: true });
  return sha;
}
