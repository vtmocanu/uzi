import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { TestContext } from "node:test";
import { GitCache } from "../src/git.js";
import { launchCodexEffectRoot, SUPERVISOR_BIN } from "../src/codex/launcher.js";
import { commandRootCommand, runnerCommand, workerBoundaryCommand } from "../src/runner-uid.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";

// Issue #4141 under the production identities: the stopped-source proof reads a runner-owned clone
// with runner-uid Git while the process is the worker. The m1 unit tests make the unreadable entries
// with the same uid; here the runner and runner-cmd uids create entries the runner cannot read.

const STATUS_MSG = "stopped source status reported a problem on stderr; not provable";
const REFS_MSG = "stopped source local refs read reported a problem on stderr; not provable";
const RUN_ID = "run-4141-unreadable-uid";
const IID = 4141;
const KEY = `issue-${IID}`;

const gitEnv = () => ({
  PATH: "/usr/local/bin:/usr/bin:/bin", LANG: "C", HOME: "/nonexistent",
  GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0",
});
const gitArgs = (cwd: string, args: string[]) => ["-c", "safe.directory=*", "-C", cwd, ...args];

function runnerExec(command: string, args: string[]): string {
  const wrapped = runnerCommand(command, args);
  return execFileSync(wrapped.command, wrapped.args, {
    env: gitEnv(), encoding: "utf8", stdio: "pipe", timeout: 15_000,
  }).trim();
}
const runnerGit = (cwd: string, ...args: string[]) => runnerExec("/usr/bin/git", gitArgs(cwd, args));
const runnerNode = (source: string, ...args: string[]) => runnerExec(process.execPath, ["-e", source, ...args]);

// One primary child per call, waited and disposed within 15s (as retained-permissions-fixture).
async function commandNode(cwd: string, source: string, ...args: string[]): Promise<string> {
  const root = await launchCodexEffectRoot({
    identity: "command", command: process.execPath, args: ["-e", source, ...args], cwd, env: gitEnv(),
    supervisorBin: SUPERVISOR_BIN,
  });
  let stdout = "", stderr = "";
  root.transport.stdout?.setEncoding("utf8").on("data", chunk => { stdout += chunk; });
  root.transport.stderr?.setEncoding("utf8").on("data", chunk => { stderr += chunk; });
  root.transport.stdin?.end();
  let code: number;
  try {
    assert.equal(root.started.uid, 10003);
    code = (await root.waitChild(15_000)).code;
  } finally {
    assert.equal((await root.dispose(15_000)).clean, true, "production command supervisor drained");
  }
  assert.equal(code, 0, stderr);
  return stdout.trim();
}

const WRITE_FILES = `
const fs = require("node:fs"), path = require("node:path");
const root = process.argv[1];
for (const [rel, body] of Object.entries(JSON.parse(process.argv[2]))) {
  fs.mkdirSync(path.dirname(path.join(root, rel)), { recursive: true });
  fs.writeFileSync(path.join(root, rel), body);
}
`;
// Best effort and bounded by the tree: the owning uid makes every directory it owns traversable
// again, then removes the children of the shared runner root.
const REMOVE_CHILDREN = `
const fs = require("node:fs"), path = require("node:path");
function open(p) {
  let s;
  try { s = fs.lstatSync(p); } catch { return; }
  if (s.isSymbolicLink() || !s.isDirectory()) return;
  try { fs.chmodSync(p, 0o770); } catch {}
  for (const n of fs.readdirSync(p)) open(path.join(p, n));
}
for (const n of fs.readdirSync(process.argv[1])) {
  const p = path.join(process.argv[1], n);
  open(p);
  fs.rmSync(p, { recursive: true, force: true });
}
`;

export interface Covered {
  git: GitCache;
  rec: { logger: unknown; lines: unknown[] };
  clone: string;
  head: string;
  verify: () => Promise<"covered" | "not_covered">;
}
type Warn = { level?: string; msg?: string; stderr?: unknown };
const warnsFor = (rec: { lines: unknown[] }, msg: string) =>
  (rec.lines as Warn[]).filter(l => l.level === "warn" && l.msg === msg);

/** Exactly one stderr warning of `msg`, whose `stderr` field is a nonempty string naming `cause`. */
function assertStderrWarn(rec: { lines: unknown[] }, msg: string, cause: RegExp): void {
  const warns = warnsFor(rec, msg);
  assert.equal(warns.length, 1, `exactly one "${msg}" warning`);
  const stderr = warns[0]!.stderr;
  assert.equal(typeof stderr, "string");
  assert.ok((stderr as string).length > 0, "the warning carries a nonempty stderr excerpt");
  assert.match(stderr as string, cause);
}
function assertNoStderrWarn(rec: { lines: unknown[] }): void {
  assert.equal(warnsFor(rec, STATUS_MSG).length, 0);
  assert.equal(warnsFor(rec, REFS_MSG).length, 0);
}

/**
 * A fresh, otherwise COVERED fixture under the worker's scratch: a REAL GitCache, the worker bare, a
 * runner-owned canonical clone seeded by the production path (createOrAttachRunnerClone under a
 * runner-group runner root), its plain journal, and one runner-made commit fetched back so the head is
 * anchored in the bare exactly like a verified capture. `body` runs against it; the runner-owned
 * tree is removed by the runner and the rest by the worker afterwards.
 */
async function withCovered(files: Record<string, string>, body: (c: Covered, fx: Fixture) => Promise<void>): Promise<void> {
  const fx = makeFixture({}, { testName: "stopped-source-unreadable-uid" });
  const rec = recordingLogger();
  const git = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions());
  const runnerRoot = path.join(fx.dataDir, "runner");
  try {
    // makeFixture starts private; the runner must traverse to the fixture and use the runner root.
    fs.chmodSync(path.dirname(fx.originPath), 0o755);
    const bare = await git.ensureClone(fx.originPath);
    fs.mkdirSync(runnerRoot, { recursive: true });
    fs.chownSync(runnerRoot, -1, 10002);
    fs.chmodSync(runnerRoot, 0o2770);
    const seeded = await git.createOrAttachRunnerClone(bare, IID, noProofReseed, RUN_ID,
      false, undefined, undefined, { selfContained: true });
    const clone = seeded.path;
    assert.equal(fs.statSync(clone).uid, 10002, "the clone is owned by the runner uid");
    await git.markRecoveryCapture(bare, clone, seeded.branch, RUN_ID);
    runnerNode(WRITE_FILES, clone, JSON.stringify({ "work.txt": "captured\n", ...files }));
    runnerGit(clone, "add", "-A");
    runnerGit(clone, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
      "-c", "commit.gpgsign=false", "commit", "-m", "milestone");
    await fixtureFetchTracking(git, bare, clone, seeded.branch, RUN_ID, 1);
    const head = runnerGit(clone, "rev-parse", "HEAD");
    assert.equal(runnerGit(clone, "status", "--porcelain"), "", "precondition: clean at the captured head");
    const verify = () => git.verifyStoppedSourceCovered(bare, seeded.branch, KEY, { runId: RUN_ID, clonePath: clone },
      head, { runId: RUN_ID, generation: 1 }, async () => true);
    assert.equal(await verify(), "covered", "precondition: the fixture is covered");
    assertNoStderrWarn(rec);
    await body({ git, rec, clone, head, verify }, fx);
  } finally {
    try {
      if (fs.existsSync(runnerRoot)) runnerNode(REMOVE_CHILDREN, runnerRoot);
    } finally {
      fx.cleanup();
    }
  }
}

const hide = (dir: string) => runnerNode('require("node:fs").chmodSync(process.argv[1], 0)', dir);
const open = (p: string, mode: number) => runnerNode('require("node:fs").chmodSync(process.argv[1], Number(process.argv[2]))', p, String(mode));

/** (a) A nonignored untracked directory the runner cannot read. Also proves the read is dispatched as the runner. */
export async function untrackedUnreadable(t: TestContext): Promise<void> {
  await withCovered({}, async ({ git, rec, clone, verify }) => {
    const calls: Array<{ command: string; args: string[] }> = [];
    const internals = git as unknown as { execScoped: (command: string, args: string[], ...rest: unknown[]) => Promise<unknown> };
    const original = internals.execScoped.bind(git);
    t.mock.method(internals, "execScoped", async (command: string, args: string[], ...rest: unknown[]) => {
      calls.push({ command, args });
      return original(command, args, ...rest);
    });
    const dir = path.join(clone, "secret");
    runnerNode('const fs=require("node:fs"); fs.mkdirSync(process.argv[1]); fs.writeFileSync(process.argv[1]+"/key.txt","runner-only\\n");', dir);
    hide(dir);
    try {
      assert.equal(await verify(), "not_covered");
      assertStderrWarn(rec, STATUS_MSG, /Permission denied/);
    } finally {
      open(dir, 0o700);
    }
    const status = calls.filter(c => c.args.includes("status") && c.args.includes("--porcelain"));
    assert.equal(status.length, 1, "one status read");
    const runner = runnerCommand("git", []);
    assert.equal(status[0]!.command, runner.command);
    assert.deepEqual(status[0]!.args.slice(0, runner.args.length), runner.args, "dispatched through the runner wrapper");
    for (const other of [workerBoundaryCommand("git", []), commandRootCommand("git", [])]) {
      assert.notDeepEqual(status[0]!.args.slice(0, other.args.length), other.args, "not a boundary or command identity");
    }
  });
}

/** (b) A modified tracked file inside a tracked directory the runner cannot read. */
export async function trackedUnreadable(): Promise<void> {
  await withCovered({ "tracked/inner.txt": "original\n" }, async ({ rec, clone, verify }) => {
    const dir = path.join(clone, "tracked");
    runnerNode(WRITE_FILES, clone, JSON.stringify({ "tracked/inner.txt": "modified runner-only bytes\n" }));
    hide(dir);
    try {
      assert.equal(await verify(), "not_covered");
      assertStderrWarn(rec, STATUS_MSG, /Permission denied/);
    } finally {
      open(dir, 0o700);
    }
  });
}

/** (c) A nonignored directory created by the command uid with mode 0700, unreadable to the runner. */
export async function commandPrivateDirectory(): Promise<void> {
  await withCovered({}, async ({ rec, clone, verify }) => {
    const dir = path.join(clone, "command-private");
    try {
      // The clone directory is runner:runner-group writable; uid 10003 holds the runner group.
      await commandNode(clone, `
const fs = require("node:fs");
if (process.getuid() !== 10003) throw Error("command identity");
fs.mkdirSync(process.argv[1], { mode: 0o700 });
fs.writeFileSync(process.argv[1] + "/note.txt", "command-private\\n");
`, dir);
      const st = fs.statSync(dir, { throwIfNoEntry: false });
      assert.equal(st?.uid, 10003);
      assert.equal((st?.mode ?? 0) & 0o777, 0o700);
      assert.equal(await verify(), "not_covered");
      assertStderrWarn(rec, STATUS_MSG, /Permission denied/);
    } finally {
      await commandNode(clone, 'require("node:fs").rmSync(process.argv[1], { recursive: true, force: true })', dir);
    }
  });
}

/** (d) A runner-unreadable loose ref naming a commit only the clone holds, with HEAD back at the captured head. */
export async function looseRefUnreadable(): Promise<void> {
  await withCovered({}, async ({ rec, clone, head, verify }) => {
    runnerGit(clone, "checkout", "-q", "-b", "side");
    runnerNode(WRITE_FILES, clone, JSON.stringify({ "side.txt": "only on a side branch\n" }));
    runnerGit(clone, "add", "side.txt");
    runnerGit(clone, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
      "-c", "commit.gpgsign=false", "commit", "-m", "side work");
    runnerGit(clone, "checkout", "-q", "--detach", head);
    assert.equal(runnerGit(clone, "status", "--porcelain"), "", "the tree is clean at the captured head");
    const ref = path.join(clone, ".git", "refs", "heads", "side");
    hide(ref);
    try {
      assert.equal(await verify(), "not_covered");
      assertStderrWarn(rec, REFS_MSG, /broken ref/);
    } finally {
      open(ref, 0o600);
    }
  });
}

/**
 * Control: a tracked file with unchanged content and a newer mtime is stat-dirty, which makes Git
 * refresh the index. This control uses the image-provided HOME and expects no Git warning. The
 * unreadable-HOME control in retained-source-ops.test.ts separately verifies conservative retention
 * when status reports an attributes-file warning; no attributes-file override is added.
 */
export async function statDirtyControl(): Promise<void> {
  await withCovered({}, async ({ rec, clone, verify }) => {
    runnerNode(`const fs=require("node:fs"); const p=process.argv[1]+"/work.txt";
fs.writeFileSync(p,"captured\\n"); const when=new Date(Date.now()+5000); fs.utimesSync(p,when,when);`, clone);
    assert.equal(await verify(), "covered");
    assertNoStderrWarn(rec);
  });
}

/** Control: an unreadable directory matched by a `.gitignore` committed in the captured head. */
export async function gitignoredControl(): Promise<void> {
  await withCovered({ ".gitignore": "/secret/\n" }, async ({ rec, clone, verify }) => {
    const dir = path.join(clone, "secret");
    runnerNode('const fs=require("node:fs"); fs.mkdirSync(process.argv[1]); fs.writeFileSync(process.argv[1]+"/key.txt","ignored\\n");', dir);
    hide(dir);
    try {
      assert.equal(await verify(), "covered");
      assertNoStderrWarn(rec);
    } finally {
      open(dir, 0o700);
    }
  });
}

/** Control: an unreadable directory matched by `.git/info/exclude`. */
export async function excludedControl(): Promise<void> {
  await withCovered({}, async ({ rec, clone, verify }) => {
    runnerNode('require("node:fs").appendFileSync(process.argv[1]+"/.git/info/exclude","/secret/\\n")', clone);
    const dir = path.join(clone, "secret");
    runnerNode('const fs=require("node:fs"); fs.mkdirSync(process.argv[1]); fs.writeFileSync(process.argv[1]+"/key.txt","excluded\\n");', dir);
    hide(dir);
    try {
      assert.equal(await verify(), "covered");
      assertNoStderrWarn(rec);
    } finally {
      open(dir, 0o700);
    }
  });
}

/** Control: a benign Git warning on stderr is retained conservatively (the source stays). */
export async function deprecatedKeyWarning(): Promise<void> {
  await withCovered({}, async ({ rec, clone, verify }) => {
    runnerNode('require("node:fs").appendFileSync(process.argv[1]+"/.git/config","[core]\\n\\tfsyncObjectFiles = true\\n")', clone);
    assert.equal(await verify(), "not_covered");
    assertStderrWarn(rec, STATUS_MSG, /fsyncObjectFiles/);
  });
}
