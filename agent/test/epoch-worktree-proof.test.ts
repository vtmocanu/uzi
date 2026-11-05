import { afterEach, beforeEach, it } from "node:test";
import type { TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFileSync, spawn, spawnSync, type SpawnOptions } from "node:child_process";
import { createHash } from "node:crypto";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProofOptions } from "../src/git.js";
import { GitIndexError, indexUnchanged, parseGitIndex, readBoundedIndex } from "../src/git-index.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// The automatic epoch worktree proof (issue #3431): every hostile or oversized input must be
// refused before Git can read, hash or execute anything for it. Each refusal below therefore
// asserts BOTH the verdict and that the hostile input was never reached: no clone-side Git launch
// at all (a launch is the only thing that could read an ignore or attributes file, hash a tracked
// file or run a filter), or none of the later stages.

const RUN = "run-worktree-proof";
const BRANCH = "agent/issue-3431";
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const EMPTY_TREE = "4b825dc642cb6eb9a060e54bf8d69288fbee4904";
const PINS = [
  "core.fsmonitor=false", "core.excludesFile=/dev/null", "core.trustctime=true", "core.checkStat=default",
  "core.fileMode=true", "core.ignoreCase=false", "core.untrackedCache=false", "core.attributesFile=/dev/null",
  "core.autocrlf=false", "core.eol=lf", "core.safecrlf=false", "core.symlinks=true", "core.sparseCheckout=false",
  "index.sparse=false", "core.splitIndex=false", "core.preloadIndex=false", "core.bigFileThreshold=1m",
  "core.hooksPath=/dev/null", "diff.external=/bin/false",
];
let fx: Fixture;
let git: GitCache;
let bare: string;
let clonePath: string;
let input: EpochDiscardProofOptions;
let live: boolean;
const cleanup: string[] = [];

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function commitAll(message: string): void {
  command(clonePath, "add", "-A");
  command(clonePath, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-qm", message);
}
function scratchDir(prefix: string): string {
  const dir = fs.mkdtempSync(path.join(process.env.TMPDIR ?? os.tmpdir(), prefix));
  cleanup.push(dir);
  return dir;
}
const inClone = (...parts: string[]) => path.join(clonePath, ...parts);
function sparse(file: string | Buffer, bytes: number, head = "x"): void {
  fs.writeFileSync(file, head);
  fs.truncateSync(file, bytes);
}
function mkfifo(file: string): void { execFileSync("mkfifo", [file]); }

beforeEach(async () => {
  fx = makeFixture({ "dir/inner.txt": "inner\n", "dir/other.txt": "other\n" });
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const attempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000001");
  const clone = await git.runnerCloneForBranch(bare, BRANCH, "issue-3431", noProofReseed, RUN, false, undefined,
    { attemptId: attempt, isLive: () => false, beforeSeed: async () => {} });
  clonePath = clone.path;
  await git.markRecoveryCapture(bare, clonePath, BRANCH, RUN, attempt);
  const raw = command(bare, "config", "--local", `uzi-recovery.${BRANCH}.clone`);
  const selector = { runId: RUN, generation: 1, attempt };
  const holds = { state: "read" as const, holds: [] };
  const lifecycle = { state: "terminal" as const, status: "completed" as const };
  const inspection = await git.inspectRetainedSources({ selector, barePath: bare, holds, lifecycle, epoch: true, isLivePath: () => false });
  assert.equal(inspection.partial, false);
  live = true;
  let pending = 0, stopped = false;
  const scope: PodEpochReclaimScope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let settled = false;
      return { settle() { if (!settled) { settled = true; pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { return [...stdio]; },
    stop() { stopped = true; },
    stopped() { return stopped; },
    assertSettled() { if (stopped || pending) throw new Error("unsettled"); },
    finish() { this.assertSettled(); },
  };
  input = { decision: { eligible: true, attested: true }, scope, selector, barePath: bare,
    paths: [clonePath], confirm: inspection.sources[0]!.descriptor, expectedRawJournal: raw,
    beforeEffect: async () => {}, checkLive: () => { if (!live) throw new Error("reservation expired"); },
    signal: new AbortController().signal, deadline: Date.now() + 30_000 };
});
afterEach(() => {
  fx?.cleanup();
  for (const dir of cleanup.splice(0)) fs.rmSync(dir, { recursive: true, force: true });
});

type Limits = { maxMetadataRecords?: number; maxMetadataBytes?: number };
/** The worktree stage alone, under its production native/limit scopes. */
async function prove(limits: Limits = {}): Promise<boolean> {
  const proof = git.createEpochDiscardProof(input);
  return git["epochRetained"].run(git["epochLimits"](limits), () =>
    git["withEpochNativeReads"](proof, async () => {
      // The stage requires the object-store observation to have run; this stage-alone helper stands in for it.
      const automatic = { roots: [], checkLive: () => {}, objectCoverage: async () => true, finalStoreCheck: async () => {} };
      // ... and the Node-only shape stage, which the full proof runs before any Git launch.
      if (!await git["automaticCloneShape"](clonePath, automatic)) return false;
      return git["worktreeCleanHardened"](clonePath, automatic);
    }));
}
/** The whole automatic content proof (object stores included). */
async function observation(limits: Limits = {}): Promise<boolean> {
  const proof = git.createEpochDiscardProof(input);
  const state = git["epochDiscardProofs"].get(proof)!;
  return git["epochRetained"].run(git["epochLimits"](limits), () =>
    git["withEpochNativeReads"](proof, () => git["automaticContentPreserved"](state)));
}

/** The stage-alone proof must end in AutomaticCompletenessUnknown (the PRD keeps unreadable object info UNKNOWN). */
async function proveUnknown(limits: Limits = {}): Promise<void> {
  await assert.rejects(prove(limits), error => (error as Error).constructor.name === "AutomaticCompletenessUnknown");
}

interface Launch { command: string; args: string[]; sub: string }
function subcommand(args: string[]): string {
  for (let i = 0; i < args.length; i++) {
    if (args[i] === "-C" || args[i] === "-c") { i++; continue; }
    if (!args[i]!.startsWith("-")) return args[i]!;
  }
  return "";
}
interface Spawned { command: string; args: string[]; env: NodeJS.ProcessEnv; sub: string }
/**
 * Records every native launch (argv before the launcher's own pins) and its audited environment.
 * With `spawns`, it also records the argv and environment the child process really received, after
 * the launcher spliced its pins in.
 */
function recordLaunches(t: TestContext, spawns = false) {
  const launches: Launch[] = [], envs: NodeJS.ProcessEnv[] = [];
  const spawned: Spawned[] = [];
  if (spawns) {
    (git as unknown as { epochNativeTestDependencies?: unknown }).epochNativeTestDependencies = {
      spawn: ((cmd: string, args: string[], opts: SpawnOptions) => {
        spawned.push({ command: cmd, args: [...args], env: { ...opts.env }, sub: subcommand(args) });
        return spawn(cmd, args, opts);
      }) as typeof spawn,
      alive: (pid: number) => {
        try { process.kill(-pid, 0); return true; }
        catch (error) { return (error as NodeJS.ErrnoException).code === "EPERM"; }
      },
      kill: (pid: number) => {
        try { process.kill(-pid, "SIGKILL"); } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error; }
      },
      joinMs: 2_000,
    };
  }
  const exec = git["execEpochNative"].bind(git);
  t.mock.method(git as unknown as { execEpochNative: typeof exec }, "execEpochNative", async (...a: Parameters<typeof exec>) => {
    launches.push({ command: a[0], args: [...a[1]], sub: subcommand(a[1]) });
    return exec(...a);
  });
  const preflight = git["preflightEpochNative"].bind(git);
  t.mock.method(git as unknown as { preflightEpochNative: typeof preflight }, "preflightEpochNative",
    (...a: Parameters<typeof preflight>) => { const env = preflight(...a); envs.push(env); return env; });
  return { launches, envs, spawned, count: (sub: string) => launches.filter(l => l.sub === sub).length };
}
type Recorder = ReturnType<typeof recordLaunches>;
function assertNoGit(rec: Recorder): void {
  assert.deepEqual(rec.launches.map(l => l.sub), [], "no Git launch may precede this refusal");
}
function assertNoContentRead(rec: Recorder): void {
  for (const sub of ["ls-files", "check-ignore", "status"]) assert.equal(rec.count(sub), 0, `${sub} must not run`);
}

/**
 * The proof no longer launches `status`. These tests still pin the argv and environment of the
 * launches it does make against hostile attribute, fsmonitor and hook inputs, so they build a
 * `status` launch from a real, pinned `ls-files` launch (same pins, same audited environment).
 */
function pinnedStatus(rec: Recorder): { command: string; args: string[]; env: NodeJS.ProcessEnv } {
  // Environment-hardening control: the argv and environment are those the child really received.
  const launch = rec.spawned.find(l => l.sub === "ls-files")!;
  return { command: launch.command, env: { ...launch.env },
    args: [...launch.args.slice(0, launch.args.indexOf("ls-files")), "status", "--porcelain"] };
}

// ---------------------------------------------------------------------------------------------
// Positives.

it("proves a clean clone, with each Git read pinned and in order", async t => {
  const rec = recordLaunches(t);
  assert.equal(await prove(), true);
  // `.uzi` is untracked and not itself ignored; its only entry, `scratch`, is: a second question.
  assert.deepEqual(rec.launches.map(l => l.sub), ["version", "config", "ls-tree", "ls-files", "check-ignore", "check-ignore"]);
  const tail = rec.launches.slice(1);
  for (const [i, launch] of tail.entries()) {
    const env = rec.envs[i + 1]!;
    assert.deepEqual(launch.args.slice(0, 2), ["-C", clonePath]);
    assert.ok(launch.args.includes(`--attr-source=${EMPTY_TREE}`), `${launch.sub} lacks --attr-source`);
    for (const pin of [...PINS, `core.worktree=${clonePath}`]) {
      const at = launch.args.indexOf(pin);
      assert.ok(at > 0 && launch.args[at - 1] === "-c", `${launch.sub} lacks -c ${pin}`);
    }
    assert.equal(env.GIT_ATTR_NOSYSTEM, "1");
    assert.equal(env.HOME, "/");
    assert.equal(env.GIT_CONFIG_GLOBAL, "/dev/null");
    assert.equal(env.GIT_CONFIG_SYSTEM, "/dev/null");
    assert.equal(env.GIT_OPTIONAL_LOCKS, "0");
    assert.equal(env.GIT_NO_REPLACE_OBJECTS, "1");
  }
  const tree = tail.find(l => l.sub === "ls-tree")!;
  assert.deepEqual(tree.args.slice(tree.args.indexOf("ls-tree")), ["ls-tree", "-r", "-z", "--full-tree", "HEAD"]);
  const files = tail.find(l => l.sub === "ls-files")!;
  assert.deepEqual(files.args.slice(files.args.indexOf("ls-files")), ["ls-files", "--others", "--directory", "-z"]);
  const ignore = tail.find(l => l.sub === "check-ignore")!;
  assert.deepEqual(ignore.args.slice(ignore.args.indexOf("check-ignore")), ["check-ignore", "--no-index", "--stdin", "-z"]);
});

it("keeps an ignored node_modules directory eligible without reading anything inside it", async t => {
  fs.writeFileSync(inClone(".gitignore"), "node_modules/\n*.log\n");
  commitAll("ignore rules");
  fs.mkdirSync(inClone("node_modules", "pkg"), { recursive: true });
  sparse(inClone("node_modules", "pkg", "big.bin"), 200 * 1024 * 1024);
  // Hostile files inside an ignored directory: never listed, never read.
  sparse(inClone("node_modules", "pkg", ".gitignore"), 99 * 1024 * 1024, "*\n");
  sparse(inClone("node_modules", "pkg", ".gitattributes"), 99 * 1024 * 1024, "* filter=x\n");
  fs.writeFileSync(inClone("dir", "debug.log"), "ignored\n");
  const rec = recordLaunches(t);
  assert.equal(await prove(), true);
  assert.equal(rec.count("status"), 0);
});

it("still proves a racily-clean tracked file under budget, and sees a same-size edit", async t => {
  const tracked = inClone("dir", "inner.txt");
  const future = new Date(Date.now() + 3600_000);
  fs.utimesSync(tracked, future, future);
  const rec = recordLaunches(t);
  assert.equal(await prove(), true);
  assert.equal(rec.count("status"), 0);
  fs.writeFileSync(tracked, "INNER\n");
  fs.utimesSync(tracked, future, future);
  assert.equal(await prove(), false);
});

it("accepts a tracked executable, symlink and an empty gitlink, and keeps a populated gitlink", async t => {
  fs.writeFileSync(inClone("run.sh"), "#!/bin/sh\n", { mode: 0o755 });
  fs.symlinkSync("dir/inner.txt", inClone("link"));
  fs.mkdirSync(inClone("sub"));
  command(clonePath, "update-index", "--add", "--cacheinfo", `160000,${command(clonePath, "rev-parse", "HEAD")},sub`);
  commitAll("shapes");
  assert.equal(await prove(), true);
  fs.writeFileSync(inClone("sub", "populated"), "x");
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoContentRead(rec);
});

it("proves the whole automatic content proof on a clean clone and refuses a planted shared index", async () => {
  assert.equal(await observation(), true);
  sparse(inClone(".git", "sharedindex." + "a".repeat(40)), 81 * 1024 * 1024);
  assert.equal(await observation(), false);
});

it("leaves the legacy proof on its own path: it still reads status and refuses an untracked file", async () => {
  assert.equal(await git["worktreeCleanHardened"](clonePath), true);
  fs.writeFileSync(inClone("untracked.txt"), "u");
  assert.equal(await git["worktreeCleanHardened"](clonePath), false);
});

// ---------------------------------------------------------------------------------------------
// H1-H4: the bypasses of the old "charge before status" proof.

/**
 * A tracked file that matches both HEAD and the worktree (a committed sparse file), so only the byte
 * budget can refuse it: without the tracked-content charge it would be opened, hashed and accepted.
 * Asserts the refusal reason and that the file is never opened or read.
 */
async function assertChargedBeforeAnyRead(t: TestContext, file: string | Buffer): Promise<void> {
  assert.equal(await prove(), true, "control: the committed file is provable under the default budget");
  const target = Buffer.from(file).toString("latin1");
  const touched: string[] = [];
  const open = fsp.open.bind(fsp);
  t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
    if (Buffer.from(args[0] as string | Buffer).toString("latin1") === target) touched.push("open");
    return open(...args);
  });
  const reasons: string[] = [];
  const debug = git["log"].debug.bind(git["log"]);
  t.mock.method(git["log"], "debug", (message: string, fields?: { error?: unknown }) => {
    if (typeof fields?.error === "string") reasons.push(fields.error);
    return debug(message, fields as never);
  });
  const rec = recordLaunches(t);
  assert.equal(await prove({ maxMetadataBytes: 65536 }), false);
  assert.deepEqual(touched, [], "the over-budget file must never be opened");
  assert.ok(reasons.some(reason => /tracked file: tracked content byte limit/.test(reason)), `refused by the charge: ${reasons.join("|")}`);
  assertNoGit(rec);
}

it("H1: a tracked non-UTF-8 path over budget is charged by its raw bytes and refused before any read", async t => {
  const name = Buffer.concat([Buffer.from(clonePath + "/"), Buffer.from([0x62, 0xff, 0x2e, 0x62, 0x69, 0x6e])]);
  sparse(name, 1024 * 1024);
  commitAll("large non-utf8 path");
  await assertChargedBeforeAnyRead(t, name);
});

it("F3: a committed, index-matched tracked file over the byte budget is refused by the charge, unopened", async t => {
  const big = inClone("dir", "big.bin");
  sparse(big, 1024 * 1024);
  commitAll("large tracked file");
  await assertChargedBeforeAnyRead(t, big);
});

it("H2: a split index is refused before any Git command, by file name and by extension", async t => {
  command(clonePath, "update-index", "--split-index");
  assert.ok(fs.readdirSync(inClone(".git")).some(n => n.startsWith("sharedindex.")));
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("H2: an 81 MB uncharged shared-index file is refused without being read", async t => {
  sparse(inClone(".git", "sharedindex." + "b".repeat(40)), 81 * 1024 * 1024);
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("H3: untracked self-ignored .gitignore files (5 x 99 MiB) are refused before any Git read", async t => {
  const big = 99 * 1024 * 1024;
  sparse(inClone(".gitignore"), big, ".gitignore\n");
  sparse(inClone("dir", ".gitignore"), big, ".gitignore\n");
  for (const name of ["a", "b", "c"]) {
    fs.mkdirSync(inClone("dir", name));
    sparse(inClone("dir", name, ".gitignore"), big, ".gitignore\n");
  }
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("H3: a self-ignoring file inside an untracked directory is never read; its name is seen first", async t => {
  fs.mkdirSync(inClone("u"));
  fs.writeFileSync(inClone("u", "file"), "x");
  sparse(inClone("u", ".gitignore"), 99 * 1024 * 1024, "*\n");
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assert.equal(rec.count("check-ignore"), 1, "only the top-level question may be asked");
  assert.equal(rec.count("status"), 0);
});

it("H3: an untracked .gitignore FIFO (invisible to ls-files) is refused without a check-ignore", async t => {
  mkfifo(inClone(".gitignore"));
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

// Git probes every top-level untracked directory for a nested repository during `ls-files --others
// --directory`: it reads `<dir>/.git` as a gitfile and opens `<gitdir>/HEAD`, which blocks on a FIFO.
async function refusedPromptly(t: TestContext): Promise<void> {
  const rec = recordLaunches(t);
  const started = Date.now();
  assert.equal(await prove(), false);
  assert.ok(Date.now() - started < 10_000, "the refusal must not wait for Git");
  assert.equal(rec.count("ls-files"), 0, "ls-files must not launch");
  assertNoGit(rec);
}

it("N1: an untracked directory with a .git directory whose HEAD is a FIFO is refused without launching ls-files", async t => {
  fs.mkdirSync(inClone("u", ".git", "objects"), { recursive: true });
  fs.mkdirSync(inClone("u", ".git", "refs"));
  mkfifo(inClone("u", ".git", "HEAD"));
  await refusedPromptly(t);
});

it("N1b: an untracked directory whose gitfile points outside at a FIFO HEAD is refused without launching ls-files", async t => {
  const outside = scratchDir("nested-gitdir-");
  fs.mkdirSync(path.join(outside, "objects"));
  fs.mkdirSync(path.join(outside, "refs"));
  mkfifo(path.join(outside, "HEAD"));
  fs.mkdirSync(inClone("v"));
  fs.writeFileSync(inClone("v", ".git"), `gitdir: ${outside}\n`);
  await refusedPromptly(t);
});

it("N2: 64 untracked directories each with a 1 MiB sparse .git file are refused without launching ls-files", async t => {
  for (let i = 0; i < 64; i++) {
    fs.mkdirSync(inClone(`s${i}`));
    sparse(inClone(`s${i}`, ".git"), 1024 * 1024, "gitdir: x\n");
  }
  await refusedPromptly(t);
});

it("N3: a .git entry of any type inside a tracked non-root directory is refused without a launch", async t => {
  fs.mkdirSync(inClone("dir", ".git"));
  await refusedPromptly(t);
});

it("N4 (control): a nested repository two levels inside an untracked directory is not probed by ls-files, which the proof then expands and refuses", async t => {
  fs.mkdirSync(inClone("u", "w", ".git", "objects"), { recursive: true });
  fs.mkdirSync(inClone("u", "w", ".git", "refs"));
  mkfifo(inClone("u", "w", ".git", "HEAD"));
  const rec = recordLaunches(t);
  const started = Date.now();
  assert.equal(await prove(), false);
  assert.ok(Date.now() - started < 10_000, "Git must not have opened the nested HEAD");
  assert.equal(rec.count("ls-files"), 1);
});

for (const [name, file] of [[".gitattributes", ".gitattributes"], [".gitmodules", "dir/.gitmodules"], [".gitattributes", "dir/.gitattributes"]] as const) {
  it(`H4: an untracked ${file} is refused before any Git read`, async t => {
    sparse(inClone(file), 99 * 1024 * 1024, `* filter=x\n# ${name}\n`);
    const rec = recordLaunches(t);
    assert.equal(await prove(), false);
    assertNoGit(rec);
  });
}

// ---------------------------------------------------------------------------------------------
// P1/P4/P6/P7 and the attribute channels.

function filterRun(t: TestContext, channel: "worktree" | "tracked" | "attr.tree" | "info" | "config" | "home") {
  const dir = scratchDir("wt-filter-");
  const marker = path.join(dir, "marker");
  const home = path.join(dir, "home");
  const attributes = path.join(dir, "attributes");
  fs.writeFileSync(inClone("tracked.txt"), "tracked\n");
  commitAll("tracked");
  const rec = recordLaunches(t, true);
  return (async () => {
    // The real launches of a pristine clone supply the exact argv and environment. The committed
    // tracked file is unpublished, so only the worktree stage (not the whole observation) can pass.
    assert.equal(await prove(), true);
    const { env, ...launch } = pinnedStatus(rec);
    const rule = "tracked.txt filter=x\n";
    if (channel === "worktree") fs.writeFileSync(inClone(".gitattributes"), rule);
    if (channel === "tracked") { fs.writeFileSync(inClone(".gitattributes"), rule); commitAll("attributes"); }
    if (channel === "attr.tree") {
      fs.writeFileSync(inClone(".gitattributes"), rule); commitAll("attributes");
      command(clonePath, "rm", "--cached", "-q", ".gitattributes"); fs.rmSync(inClone(".gitattributes"));
      command(clonePath, "config", "attr.tree", "HEAD");
    }
    if (channel === "info") fs.writeFileSync(inClone(".git", "info", "attributes"), rule);
    if (channel === "config") { fs.writeFileSync(attributes, rule); command(clonePath, "config", "core.attributesFile", attributes); }
    if (channel === "home") {
      fs.mkdirSync(path.join(home, ".config", "git"), { recursive: true });
      fs.writeFileSync(path.join(home, ".config", "git", "attributes"), rule);
    }
    command(clonePath, "config", "filter.x.clean", `sh -c 'echo ran > ${marker}; cat'`);
    const future = new Date(Date.now() + 7200_000);
    fs.utimesSync(inClone("tracked.txt"), future, future);
    const homeEnv = channel === "home" ? { HOME: home } : {};
    const launched = rec.launches.length;
    // Control: an unpinned status really runs the filter through this channel.
    spawnSync("/usr/bin/git", ["-C", clonePath, "status", "--porcelain"],
      { env: { ...ENV, GIT_OPTIONAL_LOCKS: "0", ...homeEnv } });
    const naive = fs.existsSync(marker);
    fs.rmSync(marker, { force: true });
    if (channel === "info") {
      // No pin reaches `.git/info/attributes`; the shape gate refuses its presence instead.
      assert.equal(await prove(), false);
      assert.equal(rec.launches.length, launched, "refused before any launch");
      return { naive, pinned: fs.existsSync(marker) };
    }
    const result = spawnSync(launch.command, launch.args, { env: { ...env, ...homeEnv } });
    assert.equal(result.status, 0, String(result.stderr));
    return { naive, pinned: fs.existsSync(marker) };
  })();
}
for (const channel of ["worktree", "tracked", "attr.tree", "info", "config", "home"] as const) {
  it(`P1: the status launch never runs an attribute filter named through ${channel}`, async t => {
    const { naive, pinned } = await filterRun(t, channel);
    assert.equal(naive, true, "control: the channel selects the filter for an unpinned status");
    assert.equal(pinned, false, "the proof's own status launch must not run it");
  });
}

it("P1: a filter or attribute configuration in the clone is refused before status can run it", async t => {
  const marker = path.join(scratchDir("wt-filter-"), "marker");
  fs.writeFileSync(inClone("tracked.txt"), "tracked\n");
  commitAll("tracked");
  fs.writeFileSync(inClone(".gitattributes"), "tracked.txt filter=x\n");
  commitAll("attributes");
  command(clonePath, "config", "filter.x.clean", `sh -c 'echo ran > ${marker}; cat'`);
  const future = new Date(Date.now() + 3600_000);
  fs.utimesSync(inClone("tracked.txt"), future, future);
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assert.equal(rec.count("status"), 0);
  assert.equal(fs.existsSync(marker), false);
});

it("P4: no status launch exists, so no rename detection or index stat cache is consulted", async t => {
  const rec = recordLaunches(t);
  assert.equal(await prove(), true);
  assert.equal(rec.count("status"), 0);
  assert.equal(rec.count("diff"), 0);
});

it("P6: a FIFO at a tracked .gitignore is refused by its type before any launch", async t => {
  fs.writeFileSync(inClone(".gitignore"), "*.log\n");
  commitAll("ignore");
  fs.rmSync(inClone(".gitignore"));
  mkfifo(inClone(".gitignore"));
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("P6: a FIFO at .git/info/exclude is refused before any launch", async t => {
  fs.rmSync(inClone(".git", "info", "exclude"));
  mkfifo(inClone(".git", "info", "exclude"));
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("P6: a FIFO at .git/config or .git/index is refused before any launch", async t => {
  for (const name of ["config", "index"]) {
    const file = inClone(".git", name), moved = file + ".moved";
    fs.renameSync(file, moved);
    mkfifo(file);
    const rec = recordLaunches(t);
    assert.equal(await prove(), false);
    assertNoGit(rec);
    fs.rmSync(file);
    fs.renameSync(moved, file);
    t.mock.restoreAll();
  }
});

it("P7: .git/commondir, a gitfile and a symlinked .git are refused before any launch", async t => {
  const rec = recordLaunches(t);
  fs.writeFileSync(inClone(".git", "commondir"), "../../elsewhere\n");
  assert.equal(await prove(), false);
  fs.rmSync(inClone(".git", "commondir"));
  const real = inClone(".git"), moved = inClone(".git-real");
  fs.renameSync(real, moved);
  fs.writeFileSync(real, `gitdir: ${moved}\n`);
  assert.equal(await prove(), false);
  fs.rmSync(real);
  fs.symlinkSync(moved, real);
  assert.equal(await prove(), false);
  assertNoGit(rec);
  fs.rmSync(real);
  fs.renameSync(moved, real);
  assert.equal(await prove(), true);
});

it("refuses each other .git entry the proof cannot account for, before any launch", async t => {
  const rec = recordLaunches(t);
  for (const plant of ["info/attributes", "info/sparse-checkout", "modules", "reftable"]) {
    const file = inClone(".git", plant);
    fs.writeFileSync(file, "x");
    assert.equal(await prove(), false, plant);
    fs.rmSync(file);
  }
  assertNoGit(rec);
});

// ---------------------------------------------------------------------------------------------
// Config allowlist.

for (const [key, value] of [
  ["filter.x.clean", "cat"], ["core.attributesFile", "/dev/null"], ["include.path", "x"], ["includeIf.gitdir:/.path", "x"],
  ["core.hooksPath", "/tmp"], ["extensions.worktreeConfig", "true"], ["core.sparseCheckout", "true"],
  ["status.renames", "true"], ["diff.renames", "true"], ["core.worktree", "/tmp"], ["core.ignoreCase", "true"],
  ["core.untrackedCache", "true"], ["core.fsmonitor", "true"], ["core.bare", "true"], ["attr.tree", "HEAD"],
  ["core.repositoryformatversion", "1"], ["index.sparse", "true"], ["core.splitIndex", "true"],
  ["core.autocrlf", "true"], ["safe.directory", "*"], ["extensions.refstorage", "reftable"],
  ["extensions.partialclone", "origin"],
] as const) {
  it(`config: ${key} is not an admitted clone key`, async t => {
    command(clonePath, "config", "--local", key, value);
    const rec = recordLaunches(t);
    assert.equal(await prove(), false);
    assertNoContentRead(rec);
  });
}
it("config: the keys uzi writes are admitted, and any branch tracking key", async () => {
  command(clonePath, "config", "--local", "branch.agent/issue-1.remote", "origin");
  command(clonePath, "config", "--local", "branch.agent/issue-1.merge", "refs/heads/agent/issue-1");
  command(clonePath, "config", "--local", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*");
  assert.equal(await prove(), true);
});

// ---------------------------------------------------------------------------------------------
// Bounds, typing and the final re-check.

it("a missing tracked path or a non-directory ancestor is refused before any launch", async t => {
  const rec = recordLaunches(t);
  fs.rmSync(inClone("dir", "other.txt"));
  assert.equal(await prove(), false);
  fs.writeFileSync(inClone("dir", "other.txt"), "other\n");
  fs.rmSync(inClone("dir"), { recursive: true });
  fs.symlinkSync(os.tmpdir(), inClone("dir"));
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("a tracked file replaced by a symlink or a directory of the wrong type is refused before any launch", async t => {
  const rec = recordLaunches(t);
  fs.rmSync(inClone("README.md"));
  fs.symlinkSync("dir/inner.txt", inClone("README.md"));
  assert.equal(await prove(), false);
  fs.rmSync(inClone("README.md"));
  fs.mkdirSync(inClone("README.md"));
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("charges the index to the metadata budget at its exact size before any launch", async t => {
  const sizes = [["config"], ["info", "exclude"], ["packed-refs"], ["index"]].map(p => fs.statSync(inClone(".git", ...p)).size);
  const rec = recordLaunches(t);
  const charged = sizes.reduce((a, b) => a + b, 0);
  assert.equal(await prove({ maxMetadataBytes: charged - 1 }), false);
  assertNoGit(rec);
  assert.equal(await prove({ maxMetadataBytes: charged + 8192 }), true);
});

it("charges a tracked symlink's target length to the worktree-content counter", async t => {
  for (const name of ["l1", "l2"]) fs.symlinkSync("x".repeat(3000), inClone(name));
  commitAll("links");
  assert.equal(await prove(), true);
  const rec = recordLaunches(t);
  assert.equal(await prove({ maxMetadataBytes: 4096 }), false);
  assertNoGit(rec);
});

it("caps cumulative tracked-directory entries before any launch", async t => {
  for (let i = 0; i < 50; i++) fs.writeFileSync(inClone("dir", `ignored-${i}.tmp`), "x");
  const rec = recordLaunches(t);
  assert.equal(await prove({ maxMetadataRecords: 20 }), false);
  assertNoGit(rec);
});

it("caps index entries at the record limit before any launch", async t => {
  const rec = recordLaunches(t);
  assert.equal(await prove({ maxMetadataRecords: 2 }), false);
  assertNoGit(rec);
});

it("refuses an untracked non-ignored file or directory content and accepts ignored ones", async t => {
  fs.mkdirSync(inClone("fresh", "nested"), { recursive: true });
  fs.writeFileSync(inClone("fresh", "nested", "file"), "x");
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assert.equal(rec.count("status"), 0);
  fs.rmSync(inClone("fresh"), { recursive: true });
  fs.writeFileSync(inClone("loose.txt"), "x");
  assert.equal(await prove(), false);
  fs.writeFileSync(inClone(".gitignore"), "loose.txt\n");
  commitAll("ignore rules");
  assert.equal(await prove(), true);
});

it("refuses a pathspec-magic looking untracked name instead of letting check-ignore reinterpret it", async t => {
  fs.writeFileSync(inClone(".gitignore"), "*\n");
  command(clonePath, "add", "-f", ".gitignore");
  commitAll("ignore everything");
  fs.writeFileSync(inClone(":(top)x"), "x");
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assert.equal(rec.count("check-ignore"), 0);
});

it("a Git older than 2.40 cannot supply --attr-source, so no clone read runs", async t => {
  const execScoped = git["execScoped"].bind(git);
  t.mock.method(git as unknown as { execScoped: typeof execScoped }, "execScoped",
    async (...a: Parameters<typeof execScoped>) => a[1].length === 1 && a[1][0] === "version"
      ? { stdout: "git version 2.39.5\n", stderr: "" } : execScoped(...a));
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoContentRead(rec);
  assert.equal(rec.count("config"), 0);
});

it("a tracked file that grows while Git runs is an unknown keep", async t => {
  const exec = git["execEpochNative"].bind(git);
  t.mock.method(git as unknown as { execEpochNative: typeof exec }, "execEpochNative", async (...a: Parameters<typeof exec>) => {
    const result = await exec(...a);
    if (subcommand(a[1]) === "check-ignore") fs.appendFileSync(inClone("dir", "inner.txt"), "grown");
    return result;
  });
  assert.equal(await prove(), false);
});

it("an index that changes while Git runs is an unknown keep", async t => {
  const exec = git["execEpochNative"].bind(git);
  t.mock.method(git as unknown as { execEpochNative: typeof exec }, "execEpochNative", async (...a: Parameters<typeof exec>) => {
    const result = await exec(...a);
    if (subcommand(a[1]) === "check-ignore") {
      const index = inClone(".git", "index");
      const bytes = fs.readFileSync(index);
      bytes.writeUInt8(bytes.readUInt8(bytes.length - 1) ^ 1, bytes.length - 1);
      fs.writeFileSync(index, bytes);
    }
    return result;
  });
  assert.equal(await prove(), false);
});

it("admits exactly the new argv shapes and no old or widened ones", async t => {
  const rec = recordLaunches(t);
  assert.equal(await prove(), true);
  const find = (sub: string) => rec.launches.find(l => l.sub === sub)!.args;
  const statusBase = find("ls-files");
  const internal = git as unknown as {
    preflightEpochNative(command: string, args: string[], env: NodeJS.ProcessEnv, identity: "command" | "worker_pat"): NodeJS.ProcessEnv;
  };
  const tail = (args: string[], sub: string) => args.slice(0, args.indexOf(sub));
  // `status` is never launched, so its (refused) shapes borrow the prefix of a real launch.
  const shape = (sub: string, ...rest: string[]) => [...tail(find(sub === "status" ? "ls-files" : sub), sub === "status" ? "ls-files" : sub), sub, ...rest];
  const proof = git.createEpochDiscardProof(input);
  await git["withEpochNativeReads"](proof, async () => {
    const admit = (args: string[]) => internal.preflightEpochNative("/usr/bin/git", args, {}, "command");
    const refuse = (args: string[]) => assert.throws(() => admit(args), /unaudited/);
    refuse(shape("status", "--porcelain", "-uno", "--no-renames", "--ignore-submodules=none"));
    admit(shape("ls-files", "--others", "--directory", "-z"));
    admit(shape("ls-tree", "-r", "-z", "--full-tree", "HEAD"));
    refuse(shape("ls-tree", "-r", "-z", "HEAD"));
    refuse(shape("ls-tree", "-r", "-z", "--full-tree", "refs/heads/x"));
    refuse(shape("ls-tree", "-r", "-z", "--full-tree", "HEAD", "--", "dir"));
    admit(shape("check-ignore", "--no-index", "--stdin", "-z"));
    refuse(shape("status", "--porcelain"));
    refuse(shape("ls-files", "--stage", "-z"));
    refuse(shape("ls-files", "-v", "-z"));
    refuse(shape("ls-files", "--others", "-z"));
    refuse(shape("ls-files", "--others", "--directory", "--exclude-standard", "-z"));
    refuse(shape("check-ignore", "--stdin", "-z"));
    refuse(shape("check-ignore", "--no-index", "--stdin", "-z", "-v"));
    refuse([...statusBase.slice(0, 2), "-c", "core.hooksPath=/tmp", ...statusBase.slice(2)]);
    refuse([...statusBase.slice(0, 2), "-c", "status.showUntrackedFiles=all", ...statusBase.slice(2)]);
    refuse(statusBase.map(v => v.startsWith("--attr-source=") ? "--attr-source=" + "a".repeat(40) : v));
  });
});

// ---------------------------------------------------------------------------------------------
// Index parser: fixtures, refusals and a differential against `git ls-files`.

const sha1 = (data: Buffer) => createHash("sha1").update(data).digest();
function reseal(body: Buffer): Buffer { return Buffer.concat([body, sha1(body)]); }
function body(index: Buffer): Buffer { return Buffer.from(index.subarray(0, index.length - 20)); }
function withExtension(index: Buffer, signature: string, data = Buffer.alloc(0)): Buffer {
  const header = Buffer.alloc(8);
  header.write(signature, 0, "latin1"); header.writeUInt32BE(data.length, 4);
  return reseal(Buffer.concat([body(index), header, data]));
}
function patched(index: Buffer, edit: (b: Buffer) => void): Buffer {
  const copy = body(index);
  edit(copy);
  return reseal(copy);
}
function indexInfo(repo: string, rows: { mode: string; sha: string; stage?: number; path: Buffer }[]): void {
  const stdin = Buffer.concat(rows.map(r => Buffer.concat([Buffer.from(`${r.mode} ${r.sha} ${r.stage ?? 0}\t`), r.path, Buffer.from([0])])));
  execFileSync("git", ["-C", repo, "update-index", "--add", "-z", "--index-info"], { env: ENV, input: stdin });
}
function newRepo(): string {
  const repo = scratchDir("wt-index-");
  execFileSync("git", ["init", "-q", repo], { env: ENV });
  return repo;
}
function blob(repo: string, text: string): string {
  return execFileSync("git", ["-C", repo, "hash-object", "-w", "--stdin"], { env: ENV, input: text, encoding: "utf8" }).trim();
}
const readIndex = (repo: string) => fs.readFileSync(path.join(repo, ".git", "index"));
const pathOfLength = (n: number): Buffer => {
  const parts: string[] = [];
  let used = 0;
  while (n - used > 201) { parts.push("d".repeat(200)); used += 201; }
  parts.push("f".repeat(n - used));
  return Buffer.from(parts.join("/"));
};

function plainRepo(): { repo: string; index: Buffer } {
  const repo = newRepo();
  const sha = blob(repo, "x");
  indexInfo(repo, [
    { mode: "100644", sha, path: Buffer.from("a") },
    { mode: "100644", sha, path: Buffer.from("dir/b") },
    { mode: "100755", sha, path: Buffer.from("x.sh") },
    { mode: "120000", sha, path: Buffer.from("ln") },
    { mode: "160000", sha: "1".repeat(40), path: Buffer.from("sub") },
    { mode: "100644", sha, path: Buffer.from([0x66, 0xff, 0x6f]) },
    { mode: "100644", sha, path: Buffer.from([0xe2, 0x82]) },
    ...[4094, 4095, 4096].map(n => ({ mode: "100644", sha, path: pathOfLength(n) })),
  ]);
  return { repo, index: readIndex(repo) };
}
function gitView(repo: string) {
  const stage = execFileSync("git", ["-C", repo, "ls-files", "--stage", "-z"], { env: ENV }).toString("latin1")
    .split("\0").filter(Boolean).map(row => {
      const m = /^(\d{6}) ([0-9a-f]{40}) (\d)\t([\s\S]*)$/.exec(row)!;
      return { mode: parseInt(m[1]!, 8), sha: m[2]!, stage: Number(m[3]), path: Buffer.from(m[4]!, "latin1") };
    });
  const tags = execFileSync("git", ["-C", repo, "ls-files", "-v", "-z"], { env: ENV }).toString("latin1")
    .split("\0").filter(Boolean).map(row => ({ tag: row[0]!, path: Buffer.from(row.slice(2), "latin1") }));
  return { stage, tags };
}
function expectTag(e: { assumeValid: boolean; skipWorktree: boolean; stage: number }): string {
  if (e.stage !== 0) return "M";
  if (e.assumeValid) return e.skipWorktree ? "s" : "h";
  return e.skipWorktree ? "S" : "H";
}

it("index parser agrees with git ls-files --stage and -v on a v2 index (modes, raw paths, long paths)", () => {
  const { repo, index } = plainRepo();
  assert.equal(index.readUInt32BE(4), 2);
  const parsed = parseGitIndex(index, { maxEntries: 100 });
  const view = gitView(repo);
  assert.equal(parsed.entries.length, view.stage.length);
  parsed.entries.forEach((e, i) => {
    assert.deepEqual({ mode: e.mode, sha: e.sha, stage: e.stage }, { mode: view.stage[i]!.mode, sha: view.stage[i]!.sha, stage: view.stage[i]!.stage });
    assert.ok(e.path.equals(view.stage[i]!.path), `path ${i}`);
    assert.ok(e.path.equals(view.tags[i]!.path));
    assert.equal(expectTag(e), view.tags[i]!.tag);
  });
  assert.ok(parsed.entries.some(e => e.path.length >= 4095), "long (saturated name length) paths are covered");
  assert.ok(parsed.entries.some(e => e.path.equals(Buffer.from([0x66, 0xff, 0x6f]))));
  assert.deepEqual(parsed.extensions.filter(s => s !== "TREE" && s !== "REUC"), []);
});

it("index parser agrees with git on a v3 index (assume-valid, skip-worktree, intent-to-add, stages)", () => {
  const repo = newRepo();
  const sha = blob(repo, "x");
  indexInfo(repo, [
    { mode: "100644", sha, path: Buffer.from("assumed") },
    { mode: "100644", sha, path: Buffer.from("normal") },
    { mode: "100644", sha, path: Buffer.from("skipped") },
    { mode: "100644", sha, stage: 1, path: Buffer.from("conflict") },
    { mode: "100644", sha, stage: 2, path: Buffer.from("conflict") },
    { mode: "100644", sha, stage: 3, path: Buffer.from("conflict") },
  ]);
  fs.writeFileSync(path.join(repo, "added"), "later");
  command(repo, "add", "-N", "added");
  command(repo, "update-index", "--assume-unchanged", "assumed");
  command(repo, "update-index", "--skip-worktree", "skipped");
  const index = readIndex(repo);
  assert.equal(index.readUInt32BE(4), 3);
  const parsed = parseGitIndex(index, { maxEntries: 100 }, "inspect");
  const view = gitView(repo);
  assert.equal(parsed.entries.length, view.stage.length);
  parsed.entries.forEach((e, i) => {
    assert.deepEqual({ mode: e.mode, sha: e.sha, stage: e.stage }, { mode: view.stage[i]!.mode, sha: view.stage[i]!.sha, stage: view.stage[i]!.stage });
    assert.ok(e.path.equals(view.tags[i]!.path));
    assert.equal(expectTag(e), view.tags[i]!.tag, e.path.toString());
  });
  const by = (name: string) => parsed.entries.find(e => e.path.toString() === name)!;
  assert.equal(by("assumed").assumeValid, true);
  assert.equal(by("skipped").skipWorktree, true);
  assert.equal(by("added").intentToAdd, true);
  assert.equal(by("normal").assumeValid || by("normal").skipWorktree || by("normal").intentToAdd, false);
  assert.throws(() => parseGitIndex(index, { maxEntries: 100 }), /./);
});

it("index parser refuses each hostile shape in strict mode and names the reason", () => {
  const { index } = plainRepo();
  parseGitIndex(index, { maxEntries: 100 });
  const refused = (data: Buffer, pattern: RegExp, kind: "refused" | "limit" = "refused", max = 100) => {
    assert.throws(() => parseGitIndex(data, { maxEntries: max }), error => {
      assert.ok(error instanceof GitIndexError);
      assert.equal(error.kind, kind);
      assert.match(error.message, pattern);
      return true;
    });
  };
  refused(patched(index, b => b.writeUInt32BE(5, 4)), /version/);
  refused(patched(index, b => b.writeUInt32BE(1, 4)), /version/);
  refused(patched(index, b => b.write("XXXX", 0, "latin1")), /signature/);
  refused(index.subarray(0, 20), /short/);
  refused(index.subarray(0, index.length - 30), /checksum|truncated/);
  refused(Buffer.concat([index.subarray(0, index.length - 20), Buffer.alloc(20)]), /checksum absent/);
  refused(Buffer.concat([index.subarray(0, 40), Buffer.from([index[40]! ^ 1]), index.subarray(41)]), /checksum mismatch/);
  refused(index, /entry limit/, "limit", 3);
  refused(patched(index, b => b.writeUInt32BE(b.readUInt32BE(8) + 1, 8)), /truncated|extension|padding/);
  refused(patched(index, b => b.writeUInt32BE(0o100600, 12 + 24)), /mode/);
  refused(patched(index, b => b.writeUInt32BE(0o040000, 12 + 24)), /mode/);
  refused(patched(index, b => b.writeUInt16BE(b.readUInt16BE(12 + 60) | 0x8000, 12 + 60)), /assume-valid/);
  refused(patched(index, b => b.writeUInt16BE(b.readUInt16BE(12 + 60) | 0x1000, 12 + 60)), /unmerged/);
  refused(patched(index, b => b.writeUInt16BE(b.readUInt16BE(12 + 60) | 0x4000, 12 + 60)), /extended|truncated/);
  for (const signature of ["link", "sdir", "UNTR", "FSMN", "ZZZZ", "tree", "ÿÿÿÿ"])
    refused(withExtension(index, signature, Buffer.from("data")), /unsupported index extension/);
  refused(reseal(Buffer.concat([body(index), Buffer.from("TREE"), Buffer.from([0, 0, 1, 0])])), /extension length/);
  refused(reseal(Buffer.concat([body(index), Buffer.from("TRE")])), /truncated index extension/);
  // EOIE and IEOT are never validated by this parser, so strict mode does not admit them (inspect still walks them).
  refused(withExtension(index, "EOIE", Buffer.alloc(24)), /unsupported index extension EOIE/);
  refused(withExtension(index, "IEOT", Buffer.alloc(12)), /unsupported index extension IEOT/);
  assert.deepEqual(parseGitIndex(withExtension(index, "EOIE", Buffer.alloc(24)), { maxEntries: 100 }, "inspect").extensions, ["EOIE"]);
  parseGitIndex(withExtension(withExtension(index, "TREE", Buffer.alloc(3)), "REUC"), { maxEntries: 100 });
});

it("index parser refuses unsafe tracked paths and unsorted entries", () => {
  const one = newRepo();
  indexInfo(one, [{ mode: "100644", sha: blob(one, "x"), path: Buffer.from("aa") }]);
  const single = readIndex(one);
  for (const text of ["..", "/a", "a/"])
    assert.throws(() => parseGitIndex(patched(single, b => b.write(text, 12 + 62, "latin1")), { maxEntries: 4 }), /unsafe/);
  const two = newRepo();
  const sha = blob(two, "x");
  indexInfo(two, [{ mode: "100644", sha, path: Buffer.from("a") }, { mode: "100644", sha, path: Buffer.from("b") }]);
  const swapped = patched(readIndex(two), b => { b.write("b", 12 + 62, "latin1"); b.write("a", 12 + 64 + 62, "latin1"); });
  assert.throws(() => parseGitIndex(swapped, { maxEntries: 4 }), /sorted/);
  assert.equal(parseGitIndex(swapped, { maxEntries: 4 }, "inspect").entries.length, 2);
  // Git itself refuses to add these names, so patch same-length valid ones.
  const tail = newRepo();
  indexInfo(tail, [{ mode: "100644", sha: blob(tail, "x"), path: Buffer.from("a/bbbb") }]);
  const shape = readIndex(tail);
  for (const text of ["a/.git", "a/.GIT", "a/./b", "a/../", "a//bb"])
    assert.throws(() => parseGitIndex(patched(shape, b => b.write(text, 12 + 62, "latin1")), { maxEntries: 4 }), /unsafe/, text);
  assert.equal(parseGitIndex(patched(shape, b => b.write("a/.git", 12 + 62, "latin1")), { maxEntries: 4 }, "inspect").entries.length, 1);
});

it("readBoundedIndex reads one regular file within its byte bound and refuses links and FIFOs without blocking", async () => {
  const { index } = plainRepo();
  const dir = scratchDir("wt-read-");
  const file = path.join(dir, "index");
  fs.writeFileSync(file, index);
  const read = await readBoundedIndex(file, { maxBytes: index.length, maxEntries: 100 });
  assert.equal(read.size, index.length);
  assert.equal(read.entries.length, 10);
  assert.match(read.signature, /^\d+:\d+:\d+:\d+:\d+:\d+$/);
  await assert.rejects(readBoundedIndex(file, { maxBytes: index.length - 1, maxEntries: 100 }),
    error => error instanceof GitIndexError && error.kind === "limit");
  fs.symlinkSync(file, path.join(dir, "link"));
  await assert.rejects(readBoundedIndex(path.join(dir, "link"), { maxBytes: 1 << 20, maxEntries: 100 }),
    error => error instanceof GitIndexError && error.kind === "refused");
  mkfifo(path.join(dir, "fifo"));
  await assert.rejects(readBoundedIndex(path.join(dir, "fifo"), { maxBytes: 1 << 20, maxEntries: 100 }),
    error => error instanceof GitIndexError && error.kind === "refused");
});

// Hostile index shapes reaching the proof itself: verdict plus no Git launch at all.
const HOSTILE: [string, (index: Buffer) => Buffer][] = [
  ["a version 5 index", index => patched(index, b => b.writeUInt32BE(5, 4))],
  ["a version 4 header over version 2 entries", index => patched(index, b => b.writeUInt32BE(4, 4))],
  ["a link extension", index => withExtension(index, "link", Buffer.alloc(20))],
  ["an sdir extension", index => withExtension(index, "sdir")],
  ["an UNTR extension", index => withExtension(index, "UNTR", Buffer.alloc(8))],
  ["an FSMN extension", index => withExtension(index, "FSMN", Buffer.alloc(8))],
  ["an unknown extension", index => withExtension(index, "ABCD")],
  ["an odd entry mode", index => patched(index, b => b.writeUInt32BE(0o100600, 12 + 24))],
  ["an assume-valid entry", index => patched(index, b => b.writeUInt16BE(b.readUInt16BE(12 + 60) | 0x8000, 12 + 60))],
  ["a staged entry", index => patched(index, b => b.writeUInt16BE(b.readUInt16BE(12 + 60) | 0x1000, 12 + 60))],
  ["a zero checksum", index => Buffer.concat([index.subarray(0, index.length - 20), Buffer.alloc(20)])],
  ["a bad checksum", index => Buffer.concat([index.subarray(0, 40), Buffer.from([index[40]! ^ 1]), index.subarray(41)])],
  ["a truncated file", index => index.subarray(0, index.length - 9)],
];
for (const [name, mutate] of HOSTILE) {
  it(`index: ${name} is refused before any Git launch`, async t => {
    const file = inClone(".git", "index");
    fs.writeFileSync(file, mutate(fs.readFileSync(file)));
    const rec = recordLaunches(t);
    assert.equal(await prove(), false);
    assertNoGit(rec);
  });
}
it("index: assume-valid, skip-worktree and intent-to-add entries made by Git are refused before any launch", async t => {
  const rec = recordLaunches(t);
  command(clonePath, "update-index", "--assume-unchanged", "README.md");
  assert.equal(await prove(), false);
  command(clonePath, "update-index", "--no-assume-unchanged", "README.md");
  command(clonePath, "update-index", "--skip-worktree", "README.md");
  assert.equal(await prove(), false);
  command(clonePath, "update-index", "--no-skip-worktree", "README.md");
  fs.writeFileSync(inClone("intent.txt"), "later");
  command(clonePath, "add", "-N", "intent.txt");
  assert.equal(await prove(), false);
  assertNoGit(rec);
});
it("index: the TREE cache extension Git writes is accepted", async () => {
  const file = inClone(".git", "index");
  assert.ok(parseGitIndex(fs.readFileSync(file), { maxEntries: 100 }).extensions.includes("TREE"));
  assert.equal(await prove(), true);
});

// ---------------------------------------------------------------------------------------------
// B1: index versus HEAD is compared from the parsed entries, never trusted to `status`.

/** Replace the index's cache-tree extension with `data`, resealing the checksum. */
function withCacheTree(index: Buffer, data: Buffer): Buffer {
  const content = body(index);
  for (let at = content.indexOf("TREE"); at >= 0; at = content.indexOf("TREE", at + 1)) {
    if (at >= 12 && at + 8 + content.readUInt32BE(at + 4) === content.length) {
      const header = Buffer.alloc(8);
      header.write("TREE", 0, "latin1"); header.writeUInt32BE(data.length, 4);
      return reseal(Buffer.concat([content.subarray(0, at), header, data]));
    }
  }
  throw new Error("index has no trailing TREE extension to replace");
}

it("B1: a forged cache-tree hides a staged change from status, and the proof refuses it by comparing with HEAD", async t => {
  fs.writeFileSync(inClone("dir", "inner.txt"), "staged change\n");
  command(clonePath, "add", "dir/inner.txt");
  const subtree = command(clonePath, "rev-parse", "HEAD:dir");
  const file = inClone(".git", "index");
  // Root invalid (-1 entries, 1 subtree); `dir` claims to be exactly HEAD:dir.
  const forged = Buffer.concat([Buffer.from("\0-1 1\n"), Buffer.from("dir\x002 0\n", "latin1"), Buffer.from(subtree, "hex")]);
  fs.writeFileSync(file, withCacheTree(fs.readFileSync(file), forged));
  // Control: the staged change is real, yet a plain `git status` on the forged index prints nothing.
  assert.match(command(clonePath, "diff", "--cached", "--name-only", "--no-renames"), /dir\/inner\.txt|^$/);
  const naive = spawnSync("git", ["-C", clonePath, "status", "--porcelain"], { env: { ...ENV, GIT_OPTIONAL_LOCKS: "0" }, encoding: "utf8" });
  assert.equal(naive.stdout, "", "control: the forged cache-tree makes status report clean");
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assert.equal(rec.count("ls-tree"), 1);
  assert.equal(rec.count("status"), 0, "refused by the HEAD comparison, before status");
});

it("B1: an honestly staged change, a staged deletion and a staged addition are each refused before status", async t => {
  const rec = recordLaunches(t);
  fs.writeFileSync(inClone("dir", "inner.txt"), "staged change\n");
  command(clonePath, "add", "dir/inner.txt");
  assert.equal(await prove(), false);
  command(clonePath, "reset", "-q", "--hard", "HEAD");
  command(clonePath, "rm", "-q", "dir/other.txt");
  assert.equal(await prove(), false);
  command(clonePath, "reset", "-q", "--hard", "HEAD");
  fs.writeFileSync(inClone("added.txt"), "new\n");
  command(clonePath, "add", "added.txt");
  assert.equal(await prove(), false);
  assert.equal(rec.count("status"), 0);
  command(clonePath, "reset", "-q", "--hard", "HEAD");
  fs.rmSync(inClone("added.txt"), { force: true });
  assert.equal(await prove(), true);
});

it("B1: a staged mode change and a retargeted symlink are refused by the HEAD comparison", async t => {
  fs.symlinkSync("dir/inner.txt", inClone("link"));
  commitAll("link");
  assert.equal(await prove(), true);
  const rec = recordLaunches(t);
  fs.chmodSync(inClone("README.md"), 0o755);
  command(clonePath, "add", "README.md");
  assert.equal(await prove(), false);
  command(clonePath, "reset", "-q", "--hard", "HEAD");
  fs.rmSync(inClone("link"));
  fs.symlinkSync("dir/other.txt", inClone("link"));
  command(clonePath, "add", "link");
  assert.equal(await prove(), false);
  assert.equal(rec.count("status"), 0);
});

// ---------------------------------------------------------------------------------------------
// B2: `.git` is an allowlist; every state file Git might read is typed and charged.

it("B2: a FIFO at .git/sequencer/todo is refused without hanging and without any Git launch", async t => {
  fs.mkdirSync(inClone(".git", "sequencer"));
  mkfifo(inClone(".git", "sequencer", "todo"));
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("B2: a FIFO at .git/rebase-merge/head-name (with interactive) is refused without a hang or a status", async t => {
  fs.mkdirSync(inClone(".git", "rebase-merge"));
  mkfifo(inClone(".git", "rebase-merge", "head-name"));
  fs.writeFileSync(inClone(".git", "rebase-merge", "interactive"), "");
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
  assert.equal(rec.count("status"), 0);
});

it("B2: a 200 MB regular file in .git/sequencer or .git/rebase-merge is never read", async t => {
  fs.mkdirSync(inClone(".git", "sequencer"));
  sparse(inClone(".git", "sequencer", "todo"), 200 * 1024 * 1024, "pick abc\n");
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  fs.rmSync(inClone(".git", "sequencer"), { recursive: true });
  fs.mkdirSync(inClone(".git", "rebase-merge"));
  sparse(inClone(".git", "rebase-merge", "head-name"), 200 * 1024 * 1024, "refs/heads/x\n");
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

for (const name of ["MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG",
  "BISECT_START", "AUTO_MERGE", "rebase-apply", "shallow", "worktrees", "rr-cache", "unknown-state"]) {
  it(`B2: .git/${name} is not an admitted entry and is refused before any launch`, async t => {
    if (name === "rebase-apply" || name === "worktrees" || name === "rr-cache") fs.mkdirSync(inClone(".git", name));
    else fs.writeFileSync(inClone(".git", name), "0".repeat(40) + "\n");
    const rec = recordLaunches(t);
    assert.equal(await prove(), false);
    assertNoGit(rec);
  });
}

it("B2: HEAD must be a small regular file naming a branch ref or an object id", async t => {
  const head = inClone(".git", "HEAD"), saved = fs.readFileSync(head);
  const rec = recordLaunches(t);
  const refused = async (what: string, plant: () => void) => {
    fs.rmSync(head); plant();
    assert.equal(await prove(), false, what);
    fs.rmSync(head, { force: true }); fs.writeFileSync(head, saved);
  };
  await refused("FIFO", () => mkfifo(head));
  await refused("symlink", () => fs.symlinkSync("../elsewhere", head));
  await refused("oversize", () => sparse(head, 200 * 1024 * 1024, "ref: refs/heads/main\n"));
  await refused("non-branch ref", () => fs.writeFileSync(head, "ref: refs/remotes/origin/main\n"));
  await refused("traversal", () => fs.writeFileSync(head, "ref: refs/heads/../../x\n"));
  await refused("no newline", () => fs.writeFileSync(head, "ref: refs/heads/main"));
  assertNoGit(rec);
  t.mock.restoreAll();
  assert.equal(await prove(), true);
});

it("B2: the loose ref HEAD names is typed and charged: a FIFO, an oversize file and a symref are refused", async t => {
  const loose = inClone(".git", "refs", "heads", "agent", "issue-3431"), saved = fs.readFileSync(loose);
  const rec = recordLaunches(t);
  for (const plant of [() => mkfifo(loose), () => sparse(loose, 200 * 1024 * 1024, "0".repeat(40) + "\n"),
    () => fs.writeFileSync(loose, "ref: refs/heads/main\n"), () => fs.symlinkSync("/dev/zero", loose)]) {
    fs.rmSync(loose); plant();
    assert.equal(await prove(), false);
    fs.rmSync(loose, { force: true }); fs.writeFileSync(loose, saved);
  }
  assertNoGit(rec);
  // A symlinked ancestor of the loose ref is refused too, before it can be resolved.
  const parent = inClone(".git", "refs", "heads", "agent"), moved = parent + ".moved";
  fs.renameSync(parent, moved);
  fs.symlinkSync(moved, parent);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("B2: a detached HEAD at an object id, a packed-only branch and regular ORIG_HEAD/FETCH_HEAD are admitted", async () => {
  const sha = command(clonePath, "rev-parse", "HEAD");
  fs.writeFileSync(inClone(".git", "ORIG_HEAD"), sha + "\n");
  fs.writeFileSync(inClone(".git", "FETCH_HEAD"), `${sha}\t\tbranch 'main' of x\n`);
  assert.equal(await prove(), true);
  command(clonePath, "pack-refs", "--all", "--prune");
  assert.equal(await prove(), true);
  command(clonePath, "checkout", "-q", "--detach");
  assert.equal(await prove(), true);
  for (const name of ["ORIG_HEAD", "FETCH_HEAD", "COMMIT_EDITMSG"]) {
    const file = inClone(".git", name);
    fs.rmSync(file, { force: true }); mkfifo(file);
    assert.equal(await prove(), false, name);
    fs.rmSync(file);
  }
});

// ---------------------------------------------------------------------------------------------
// V4: prefix-compressed names, differential against `git ls-files --stage -z`.

it("index parser agrees with git ls-files --stage on a version 4 index (prefix-compressed names)", () => {
  const repo = newRepo();
  const sha = blob(repo, "x");
  indexInfo(repo, [
    { mode: "100644", sha, path: Buffer.from("a") },
    { mode: "100644", sha, path: Buffer.from("alpha/one.txt") },
    { mode: "100644", sha, path: Buffer.from("alpha/one.txt.bak") },
    { mode: "100755", sha, path: Buffer.from("alpha/two.sh") },
    { mode: "100644", sha, path: Buffer.from("alpha/sub/deeper/file") },
    { mode: "100644", sha, path: Buffer.from("alpha/sub/deeper/files") },
    { mode: "100644", sha, path: Buffer.from("beta") },
    { mode: "120000", sha, path: Buffer.from("beta.lnk") },
    { mode: "160000", sha: "2".repeat(40), path: Buffer.from("mod") },
    { mode: "100644", sha, path: Buffer.from([0x7a, 0x2f, 0xff, 0x6f]) },
    { mode: "100644", sha, path: Buffer.from([0x7a, 0x2f, 0xff, 0x70]) },
    { mode: "100644", sha, path: Buffer.from("x".repeat(300) + "/" + "y".repeat(200)) },
    { mode: "100644", sha, path: Buffer.from("x".repeat(300) + "/" + "y".repeat(200) + "z") },
  ]);
  fs.writeFileSync(path.join(repo, "real.txt"), "hello");
  command(repo, "add", "real.txt");
  command(repo, "update-index", "--index-version", "4");
  const index = readIndex(repo);
  assert.equal(index.readUInt32BE(4), 4);
  const parsed = parseGitIndex(index, { maxEntries: 100 });
  assert.equal(parsed.version, 4);
  const view = gitView(repo);
  assert.equal(parsed.entries.length, view.stage.length);
  parsed.entries.forEach((e, i) => {
    assert.deepEqual({ mode: e.mode, sha: e.sha, stage: e.stage }, { mode: view.stage[i]!.mode, sha: view.stage[i]!.sha, stage: view.stage[i]!.stage });
    assert.ok(e.path.equals(view.stage[i]!.path), `path ${i}`);
  });
  // Raw stat fields are kept for a later racily-clean check.
  const real = parsed.entries.find(e => e.path.toString() === "real.txt")!;
  const st = fs.statSync(path.join(repo, "real.txt"), { bigint: true });
  assert.deepEqual({ size: real.size, mtimeS: real.mtimeS, ctimeS: real.ctimeS, ino: real.ino, uid: real.uid },
    { size: 5, mtimeS: Number(st.mtimeMs / 1000n), ctimeS: Number(st.ctimeMs / 1000n), ino: Number(st.ino & 0xffffffffn), uid: Number(st.uid) });
});

it("index parser refuses hostile version 4 names", () => {
  const repo = newRepo();
  const sha = blob(repo, "x");
  indexInfo(repo, [{ mode: "100644", sha, path: Buffer.from("aaaa") }, { mode: "100644", sha, path: Buffer.from("aaab") }]);
  command(repo, "update-index", "--index-version", "4");
  const index = readIndex(repo);
  parseGitIndex(index, { maxEntries: 4 });
  // Entry layout: 62 fixed bytes, a varint strip count, then the NUL-terminated suffix.
  const first = 12, second = 12 + 62 + 1 + 5;
  assert.equal(index[first + 62], 0);
  assert.equal(index[second + 62], 1);
  const refused = (data: Buffer, pattern: RegExp) => assert.throws(() => parseGitIndex(data, { maxEntries: 4 }), pattern);
  refused(patched(index, b => { b[first + 62] = 1; }), /strip length exceeds/);
  refused(patched(index, b => { b[second + 62] = 9; }), /strip length exceeds/);
  refused(patched(index, b => { b[second + 62] = 0x80; }), /./);
  refused(patched(index, b => { b[first + 62 + 1 + 4] = 0x61; }), /./);
});

it("index: a version 4 index Git wrote for the clone is proven", async () => {
  command(clonePath, "update-index", "--index-version", "4");
  assert.equal(parseGitIndex(fs.readFileSync(inClone(".git", "index")), { maxEntries: 100 }).version, 4);
  assert.equal(await prove(), true);
});

// ---------------------------------------------------------------------------------------------
// Differential against this repository's own index.

it("index parser agrees with git ls-files --stage on this repository's own index", t => {
  const here = path.dirname(new URL(import.meta.url).pathname);
  const top = spawnSync("git", ["-C", here, "rev-parse", "--show-toplevel"], { env: ENV, encoding: "utf8" });
  if (top.status !== 0) { t.skip("not inside a git checkout: no repository index to compare"); console.log("SKIP: not inside a git checkout"); return; }
  const root = top.stdout.trim();
  for (let attempt = 0; ; attempt++) {
    const where = execFileSync("git", ["-C", root, "rev-parse", "--git-path", "index"], { env: ENV, encoding: "utf8" }).trim();
    const file = path.resolve(root, where);
    const bytes = fs.readFileSync(file);
    const listing = execFileSync("git", ["-C", root, "ls-files", "--stage", "-z"], { env: ENV, maxBuffer: 1 << 28 }).toString("latin1");
    if (!bytes.equals(fs.readFileSync(file))) { assert.ok(attempt < 3, "the repository index kept changing"); continue; }
    const parsed = parseGitIndex(bytes, { maxEntries: 5_000_000 }, "inspect");
    if (parsed.extensions.includes("link")) { t.skip("split index: entries are partial"); return; }
    const rows = listing.split("\0").filter(Boolean);
    assert.equal(parsed.entries.length, rows.length);
    for (const [i, row] of rows.entries()) {
      const m = /^(\d{6}) ([0-9a-f]{40}) (\d)\t([\s\S]*)$/.exec(row)!;
      const e = parsed.entries[i]!;
      if (e.mode !== parseInt(m[1]!, 8) || e.sha !== m[2] || e.stage !== Number(m[3]) || !e.path.equals(Buffer.from(m[4]!, "latin1")))
        assert.fail(`first mismatch at entry ${i}: ${e.path.toString("latin1").slice(0, 80)}`);
    }
    console.log(`checked ${rows.length} entries of ${file} (version ${parsed.version})`);
    return;
  }
});

// ---------------------------------------------------------------------------------------------
// Planted executing inputs and the launcher's environment.

function markerScript(): { dir: string; marker: string; script: string } {
  const dir = scratchDir("wt-exec-");
  const marker = path.join(dir, "marker");
  const script = path.join(dir, "run.sh");
  fs.writeFileSync(script, `#!/bin/sh\necho ran >> '${marker}'\nprintf '\\0'\n`, { mode: 0o755 });
  return { dir, marker, script };
}

it("a planted executing core.fsmonitor is refused, and the proof's own status launch never runs it", async t => {
  const { marker, script } = markerScript();
  const rec = recordLaunches(t, true);
  assert.equal(await observation(), true);
  const { env, ...launch } = pinnedStatus(rec);
  command(clonePath, "config", "core.fsmonitor", script);
  const future = new Date(Date.now() + 7200_000);
  fs.utimesSync(inClone("README.md"), future, future);
  spawnSync("git", ["-C", clonePath, "status", "--porcelain"], { env: { ...ENV, GIT_OPTIONAL_LOCKS: "0" } });
  assert.equal(fs.existsSync(marker), true, "control: an unpinned status runs the fsmonitor script");
  fs.rmSync(marker);
  const result = spawnSync(launch.command, launch.args, { env });
  assert.equal(result.status, 0, String(result.stderr));
  assert.equal(fs.existsSync(marker), false, "the pinned launch must not run it");
  const before = rec.launches.length;
  assert.equal(await observation(), false);
  assert.equal(rec.launches.slice(before).some(l => l.sub === "status"), false);
  assert.equal(fs.existsSync(marker), false);
});

it("a planted executing core.hooksPath hook is refused and the proof's own status launch never runs it", async t => {
  const { dir, marker } = markerScript();
  const hooks = path.join(dir, "hooks");
  fs.mkdirSync(hooks);
  for (const hook of ["post-index-change", "fsmonitor-watchman", "pre-commit", "reference-transaction"])
    fs.writeFileSync(path.join(hooks, hook), `#!/bin/sh\necho ran >> '${marker}'\n`, { mode: 0o755 });
  for (const hook of ["post-index-change", "fsmonitor-watchman"])
    fs.writeFileSync(inClone(".git", "hooks", hook), `#!/bin/sh\necho ran >> '${marker}'\n`, { mode: 0o755 });
  const rec = recordLaunches(t, true);
  assert.equal(await observation(), true);
  assert.equal(fs.existsSync(marker), false, "a hook in the default hooks directory ran during the proof");
  const { env, ...launch } = pinnedStatus(rec);
  command(clonePath, "config", "core.hooksPath", hooks);
  const future = new Date(Date.now() + 7200_000);
  fs.utimesSync(inClone("README.md"), future, future);
  const result = spawnSync(launch.command, launch.args, { env });
  assert.equal(result.status, 0, String(result.stderr));
  assert.equal(fs.existsSync(marker), false, "the pinned launch must not run a hook");
  const before = rec.launches.length;
  assert.equal(await observation(), false);
  assert.equal(rec.launches.slice(before).some(l => l.sub === "status"), false);
  assert.equal(fs.existsSync(marker), false);
});

it("a global or system config with include.path (naming a filter) is never read by any launch", async t => {
  const dir = scratchDir("wt-global-");
  const marker = path.join(dir, "marker");
  const included = path.join(dir, "included");
  const global = path.join(dir, "global");
  fs.writeFileSync(included, `[filter "x"]\n\tclean = sh -c 'echo ran >> ${marker}; cat'\n[core]\n\thooksPath = ${dir}\n`);
  fs.writeFileSync(global, `[include]\n\tpath = ${included}\n`);
  const saved = { global: process.env.GIT_CONFIG_GLOBAL, system: process.env.GIT_CONFIG_SYSTEM };
  process.env.GIT_CONFIG_GLOBAL = global;
  process.env.GIT_CONFIG_SYSTEM = global;
  try {
    const rec = recordLaunches(t, true);
    assert.equal(await observation(), true);
    assert.ok(rec.spawned.length >= 6);
    for (const { env } of rec.spawned) {
      assert.equal(env.GIT_CONFIG_GLOBAL, "/dev/null");
      assert.equal(env.GIT_CONFIG_SYSTEM, "/dev/null");
      assert.equal(env.GIT_CONFIG_NOSYSTEM, "1");
      assert.equal(env.HOME, "/");
    }
    // Control: with the file in the environment, git does read the include.
    const listed = (env: NodeJS.ProcessEnv) => spawnSync("git", ["-C", clonePath, "config", "--list"], { env, encoding: "utf8" }).stdout;
    assert.match(listed({ ...ENV, GIT_CONFIG_GLOBAL: global }), /filter\.x\.clean=/);
    assert.doesNotMatch(listed({ ...rec.envs[1]!, PATH: process.env.PATH }), /filter\.x\.clean=|include\.path=/);
    assert.equal(fs.existsSync(marker), false);
  } finally {
    for (const [key, value] of [["GIT_CONFIG_GLOBAL", saved.global], ["GIT_CONFIG_SYSTEM", saved.system]] as const) {
      if (value === undefined) delete process.env[key]; else process.env[key] = value;
    }
  }
});

// ---------------------------------------------------------------------------------------------
// Review rework (issue #3431): F1 version 4 name length, F2 forged stat cache, F3 info/exclude
// baseline, F4 config includes and object-store entries Git opens at startup.

it("F1: a version 4 entry whose flags length differs from its real name is refused, as Git would walk it differently", async t => {
  const repo = newRepo();
  const sha = blob(repo, "x");
  indexInfo(repo, [{ mode: "100644", sha, path: Buffer.from("aaaa") }, { mode: "100644", sha, path: Buffer.from("aaab") }]);
  command(repo, "update-index", "--index-version", "4");
  const index = readIndex(repo);
  assert.equal(parseGitIndex(index, { maxEntries: 4 }).entries.length, 2, "control: Git's own v4 index parses");
  const flagsAt = 12 + 60;
  assert.equal(index.readUInt16BE(flagsAt) & 0xfff, 4);
  const withLength = (n: number) => patched(index, b => b.writeUInt16BE((b.readUInt16BE(flagsAt) & ~0xfff) | n, flagsAt));
  for (const n of [3, 5, 0, 0xfff]) {
    assert.throws(() => parseGitIndex(withLength(n), { maxEntries: 4 }),
      error => error instanceof GitIndexError && error.kind === "refused" && /disagrees/.test(error.message), `length ${n}`);
  }
  // Through the proof: the clone's own index rewritten the same way is refused before any launch.
  command(clonePath, "update-index", "--index-version", "4");
  const file = inClone(".git", "index");
  const real = fs.readFileSync(file);
  assert.equal(await prove(), true);
  fs.writeFileSync(file, patched(real, b => b.writeUInt16BE((b.readUInt16BE(flagsAt) & ~0xfff) | 0xfff, flagsAt)));
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

/**
 * The reviewer's forgery: edit a tracked file, stage it, then rewrite the index so the entry carries
 * HEAD's object id while keeping the edited file's real stat data, and let the index be rewritten
 * more than a second after the edit so Git does not call the entry racily clean.
 */
async function forgeEntryToHead(rel: string): Promise<void> {
  const headOid = command(clonePath, "rev-parse", `HEAD:${rel}`);
  command(clonePath, "add", rel);
  await new Promise(resolve => setTimeout(resolve, 1100));
  const file = inClone(".git", "index");
  const original = fs.readFileSync(file);
  const at = original.indexOf(Buffer.concat([Buffer.from(rel), Buffer.from([0])]));
  assert.ok(at >= 12 + 62, "entry name located in the index");
  const forged = patched(original, b => { Buffer.from(headOid, "hex").copy(b, at - 22); });
  fs.writeFileSync(file, withCacheTree(forged, Buffer.alloc(0)));
}

it("F2: an entry carrying HEAD's oid with the edited file's real stat data is refused by hashing, before any launch", async t => {
  fs.writeFileSync(inClone("dir", "other.txt"), "edited!\n");
  await forgeEntryToHead("dir/other.txt");
  const naive = spawnSync("git", ["-C", clonePath, "status", "--porcelain", "-uno"], { env: { ...ENV, GIT_OPTIONAL_LOCKS: "0" }, encoding: "utf8" });
  assert.equal(naive.stdout, "", "control: the forged stat cache makes status report clean");
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("F2: a forged tracked .gitignore rewritten to '*' cannot hide untracked work, and is refused before check-ignore", async t => {
  fs.writeFileSync(inClone(".gitignore"), "# nothing ignored\n");
  commitAll("ignore file");
  assert.equal(await prove(), true);
  fs.writeFileSync(inClone(".gitignore"), "*\n");
  fs.writeFileSync(inClone("work.txt"), "unsaved work\n");
  await forgeEntryToHead(".gitignore");
  const naive = spawnSync("git", ["-C", clonePath, "status", "--porcelain", "-uno"], { env: { ...ENV, GIT_OPTIONAL_LOCKS: "0" }, encoding: "utf8" });
  assert.equal(naive.stdout, "", "control: the forged entry makes status report clean");
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
  assert.equal(rec.count("check-ignore"), 0);
});

it("F2: a same-size edit, a lost executable bit and a retargeted symlink are each refused", async t => {
  fs.symlinkSync("dir/inner.txt", inClone("link"));
  fs.writeFileSync(inClone("run.sh"), "#!/bin/sh\n", { mode: 0o755 });
  commitAll("shapes");
  assert.equal(await prove(), true);
  const rec = recordLaunches(t);
  fs.writeFileSync(inClone("dir", "other.txt"), "OTHER\n");
  assert.equal(await prove(), false);
  fs.writeFileSync(inClone("dir", "other.txt"), "other\n");
  fs.chmodSync(inClone("run.sh"), 0o644);
  assert.equal(await prove(), false);
  fs.chmodSync(inClone("run.sh"), 0o755);
  fs.rmSync(inClone("link"));
  fs.symlinkSync("dir/other.txt", inClone("link"));
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("F3: an info/exclude rule beyond the worker baseline is refused before check-ignore, whatever it hides", async t => {
  fs.appendFileSync(inClone(".git", "info", "exclude"), "*\n");
  fs.writeFileSync(inClone("work.txt"), "unsaved work\n");
  const rec = recordLaunches(t);
  assert.equal(await observation(), false);
  assertNoGit(rec);
  for (const rule of ["!keep", "/.uzi/scratch", " /.uzi/scratch/", "# ok\r\nfoo", "\0"]) {
    fs.writeFileSync(inClone(".git", "info", "exclude"), `# seeded\n\n/.uzi/scratch/\n${rule}\n`);
    assert.equal(await observation(), false, JSON.stringify(rule));
  }
  assertNoGit(rec);
});

it("F3: the seeded baseline, comments and blank lines still prove", async () => {
  fs.writeFileSync(inClone(".git", "info", "exclude"), "# comment\n\n   \n/.uzi/scratch/\n/.uzi/scratch/\n");
  assert.equal(await prove(), true);
});

it("F4: an include.path naming a FIFO is refused promptly, without any Git launch", async t => {
  const fifo = path.join(scratchDir("wt-include-"), "fifo");
  mkfifo(fifo);
  fs.appendFileSync(inClone(".git", "config"), `[include]\n\tpath = ${fifo}\n`);
  const rec = recordLaunches(t);
  const started = Date.now();
  assert.equal(await prove(), false);
  assert.ok(Date.now() - started < 10_000, "must not wait on the FIFO");
  assertNoGit(rec);
});

it("F4: an includeIf naming a 300 MiB file is refused without a launch, so the file is never read", async t => {
  const big = path.join(scratchDir("wt-include-"), "big");
  sparse(big, 300 * 1024 * 1024, "[core]\n");
  fs.appendFileSync(inClone(".git", "config"), `[includeIf "gitdir:/"]\n\tpath = ${big}\n[ INCLUDE ]\n\tpath = ${big}\n`);
  const rec = recordLaunches(t);
  assert.equal(await prove(), false);
  assertNoGit(rec);
});

it("F4: a FIFO at .git/objects/info/alternates is UNKNOWN promptly, without any Git launch", async t => {
  const alternates = inClone(".git", "objects", "info", "alternates");
  fs.mkdirSync(path.dirname(alternates), { recursive: true });
  fs.rmSync(alternates, { force: true });
  mkfifo(alternates);
  const rec = recordLaunches(t);
  const started = Date.now();
  await proveUnknown();
  assert.ok(Date.now() - started < 10_000, "must not wait on the FIFO");
  assertNoGit(rec);
});

it("F4: http-alternates, an unknown entry or a symlinked objects/info are UNKNOWN; alternates, packs and commit-graph files are admitted", async t => {
  const info = inClone(".git", "objects", "info");
  fs.mkdirSync(info, { recursive: true });
  fs.writeFileSync(path.join(info, "packs"), "\n");
  fs.writeFileSync(path.join(info, "commit-graph"), "unread: core.commitGraph is pinned off");
  fs.mkdirSync(path.join(info, "commit-graphs"));
  assert.equal(await prove(), true);
  fs.rmSync(path.join(info, "commit-graph"));
  fs.rmSync(path.join(info, "commit-graphs"), { recursive: true });
  const rec = recordLaunches(t);
  for (const name of ["http-alternates", "unknown-entry"]) {
    const file = path.join(info, name);
    fs.writeFileSync(file, "x");
    await proveUnknown();
    fs.rmSync(file);
  }
  mkfifo(path.join(info, "commit-graph"));
  await proveUnknown();
  fs.rmSync(path.join(info, "commit-graph"));
  fs.rmSync(info, { recursive: true });
  fs.symlinkSync(os.tmpdir(), info);
  await proveUnknown();
  assertNoGit(rec);
});

// ---------------------------------------------------------------------------------------------
// Review rework guards: path bound for every index version, byte-exact re-read, launch pins, and
// the Node-only clone shape stage ahead of every Git launch.

it("index: a path over 4096 bytes is refused in a version 2 index, while 4096 is accepted", () => {
  const repo = newRepo();
  const sha = blob(repo, "x");
  indexInfo(repo, [{ mode: "100644", sha, path: pathOfLength(4096) }]);
  assert.equal(parseGitIndex(readIndex(repo), { maxEntries: 4 }).version, 2);
  indexInfo(repo, [{ mode: "100644", sha, path: pathOfLength(4097) }]);
  const index = readIndex(repo);
  assert.equal(index.readUInt32BE(4), 2);
  assert.throws(() => parseGitIndex(index, { maxEntries: 4 }), /exceeds PATH_MAX/);
});

it("indexUnchanged compares the bytes, not only the stat signature and the trailer", async () => {
  const { index } = plainRepo();
  const dir = scratchDir("wt-unchanged-");
  const file = path.join(dir, "index");
  fs.writeFileSync(file, index);
  const read = await readBoundedIndex(file, { maxBytes: index.length, maxEntries: 100 });
  assert.equal(await indexUnchanged(file, read), true);
  // Same signature (the file is untouched) and same trailer, but the image that was parsed differs
  // in a body byte: a trailer-only comparison would still say "unchanged".
  const forged = Buffer.from(read.bytes);
  forged[forged.length - 21]! ^= 1;
  assert.deepEqual(forged.subarray(forged.length - 20), read.bytes.subarray(read.bytes.length - 20));
  assert.equal(await indexUnchanged(file, { ...read, bytes: forged }), false);
  // A rewrite with identical content still changes ctime, so the signature alone refuses it.
  fs.writeFileSync(file, index);
  assert.equal(await indexUnchanged(file, read), false);
});

it("every native launch carries its pins, and a launch missing one is refused", async t => {
  const rec = recordLaunches(t, true);
  assert.equal(await prove(), true);
  const clone = rec.spawned.filter(l => l.args.includes("-C") && l.sub !== "version");
  assert.ok(clone.length >= 4, "clone-side reads were launched");
  const state = { paths: [clonePath], attrSource: true };
  const require = (args: string[]) => git["requireEpochNativePins"](args, args.indexOf("-C"), state, false);
  for (const launch of clone) {
    assert.doesNotThrow(() => require(launch.args), launch.sub);
    for (const pin of [...PINS, `core.worktree=${clonePath}`]) {
      const at = launch.args.indexOf(pin);
      assert.ok(at > 0 && launch.args[at - 1] === "-c", `${launch.sub} carries -c ${pin}`);
      const without = launch.args.filter((_, i) => i !== at && i !== at - 1);
      assert.throws(() => require(without), /lacks its pins/, `${launch.sub} without ${pin}`);
    }
    const attr = launch.args.find(arg => arg.startsWith("--attr-source="))!;
    assert.throws(() => require(launch.args.filter(arg => arg !== attr)), /lacks its pins/, `${launch.sub} without --attr-source`);
  }
});

it("a FIFO or oversized .git entry keeps the source before any Git launch of the whole content proof", async t => {
  fs.mkdirSync(inClone(".git", "sequencer"));
  mkfifo(inClone(".git", "sequencer", "todo"));
  const rec = recordLaunches(t);
  assert.equal(await observation(), false);
  assertNoGit(rec);
  fs.rmSync(inClone(".git", "sequencer"), { recursive: true });
  sparse(inClone(".git", "packed-refs"), 4 * 1024 * 1024, "# pack-refs\n");
  assert.equal(await observation({ maxMetadataBytes: 65536 }), false);
  assertNoGit(rec);
});

// ---------------------------------------------------------------------------------------------
// F1: Git reads every loose ref, so the shape stage walks `.git/refs` in Node before any launch.

const REF_BYTES = 1024;
async function assertRefsRefusedBeforeGit(t: TestContext, why: string): Promise<void> {
  const rec = recordLaunches(t);
  assert.equal(await observation(), false, why);
  assertNoGit(rec);
}
it("F1: a sparse oversize loose ref keeps the source with no native launch", async t => {
  const big = inClone(".git", "refs", "heads", "zzbig");
  sparse(big, 3 * 1024 * 1024 * 1024);
  await assertRefsRefusedBeforeGit(t, "3 GiB loose ref");
  fs.rmSync(big);
  sparse(big, REF_BYTES + 1);
  await assertRefsRefusedBeforeGit(t, "one byte over the ref limit");
  fs.rmSync(big);
  sparse(big, REF_BYTES);
  assert.equal(await prove(), true, "the shape stage admits a regular ref of exactly the limit");
  fs.rmSync(big);
  assert.equal(await observation(), true, "control: the clone is provable again");
});
it("F1: a FIFO under refs/heads keeps the source with no native launch", async t => {
  const fifo = inClone(".git", "refs", "heads", "zzfifo");
  mkfifo(fifo);
  const started = Date.now();
  await assertRefsRefusedBeforeGit(t, "FIFO ref");
  assert.ok(Date.now() - started < 10_000, "must not wait on the FIFO");
});
it("F1: a symlinked refs subdirectory or ref file keeps the source with no native launch", async t => {
  const dir = inClone(".git", "refs", "heads", "linked");
  fs.symlinkSync(os.tmpdir(), dir);
  await assertRefsRefusedBeforeGit(t, "symlinked refs directory");
  fs.rmSync(dir);
  fs.symlinkSync(inClone("dir", "inner.txt"), inClone(".git", "refs", "heads", "linkedfile"));
  await assertRefsRefusedBeforeGit(t, "symlinked ref file");
});
it("F1: every refs entry counts against the record limit, nested directories included", async t => {
  assert.equal(await observation({ maxMetadataRecords: 64 }), true, "control: the clone fits the limit");
  const dir = inClone(".git", "refs", "heads", "many", "deeper");
  fs.mkdirSync(dir, { recursive: true });
  for (let i = 0; i < 80; i++) fs.writeFileSync(path.join(dir, `r${i}`), "0".repeat(40) + "\n");
  const rec = recordLaunches(t);
  assert.equal(await observation({ maxMetadataRecords: 64 }), false);
  assertNoGit(rec);
});
it("F1: a ref changed after it was typed is a keep at the final re-check", async () => {
  const ref = inClone(".git", "refs", "heads", "late");
  fs.writeFileSync(ref, command(clonePath, "rev-parse", "HEAD") + "\n");
  const proof = git.createEpochDiscardProof(input);
  const result = await git["epochRetained"].run(git["epochLimits"]({}), () => git["withEpochNativeReads"](proof, async () => {
    const automatic = { roots: [], checkLive: () => {}, objectCoverage: async () => true, finalStoreCheck: async () => {} };
    if (!await git["automaticCloneShape"](clonePath, automatic)) return "shape";
    fs.writeFileSync(ref, "0".repeat(40) + "\n");
    return git["worktreeCleanHardened"](clonePath, automatic);
  }));
  assert.equal(result, false);
});
it("F1: any for-each-ref stderr (a broken ref warning) is unknown, never clean", async () => {
  fs.writeFileSync(inClone(".git", "refs", "heads", "broken"), "not-an-object-id\n");
  await assert.rejects(observation(), error => (error as Error).constructor.name === "AutomaticCompletenessUnknown" &&
    /for-each-ref/.test((error as Error).message));
});

// ---------------------------------------------------------------------------------------------
// F4: the index must have exactly HEAD's entries; a staged addition that sorts last is not clean.

it("F4: a staged addition that sorts after every HEAD entry is a keep even when its blob is published", async () => {
  fs.copyFileSync(inClone("dir", "inner.txt"), inClone("zz-copy.md"));
  command(clonePath, "add", "zz-copy.md");
  assert.equal(command(clonePath, "ls-files").split("\n").at(-1), "zz-copy.md");
  assert.equal(await prove(), false);
  command(clonePath, "rm", "--cached", "-q", "zz-copy.md");
  fs.rmSync(inClone("zz-copy.md"));
  assert.equal(await prove(), true, "control: without the addition the clone is provable");
});
