// Acceptance tests for the hash-attested maintenance WIP capture (W0-H, issue #3349 / #3031).
//
// The capture stages and commits INSIDE the clone as the runner uid with plumbing
// (add -A, write-tree, a pre-commit tree proof, commit-tree, update-ref CAS). These tests
// drive the real `captureMaintenanceWip` under a real admitted maintenance slot and assert on
// observable end state (HEAD, refs, index bytes, worktree bytes, object contents, events,
// charged resources), not on internal routing. Fixtures live under os.tmpdir() (R5: a
// container-internal case-sensitive filesystem), and the suite runs only on Linux because the
// runner-owned helpers address the clone through /proc/<pid>/fd.
//
// The shared policy port ("core"/8 MiB cases) lives in git-maintenance-wip-capture.test.ts;
// the runner-level FINAL boundary is exercised by runner-maintenance-terminal-source.test.ts.
import test, { describe } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import fsSync from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createHash } from "node:crypto";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { AGENT_GIT_IDENTITY, GitCache, WIP_PARK_COMMIT_PREFIX } from "../src/git.js";
import { MaintenanceResources, RecoveryMaintenance, type MaintenanceResource } from "../src/recovery-maintenance.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { nullLogger } from "./helpers.js";

type Step = "status" | "add" | "commit";
type Phase = "prepare" | "dispatch";
type GitIn = (...args: string[]) => string;
type Intercept = (request: BoundaryProcessRequest) => string | undefined;
type Callbacks = {
  beforeStep: (step: Step, phase: Phase) => Promise<void>;
  afterRunnerGit: (step: Step) => Promise<void>;
};
interface HookOptions {
  before?: (step: Step, phase: Phase, visit: number) => Promise<void> | void;
  after?: (step: Step) => Promise<void> | void;
}
interface FixtureOptions {
  monotonicNow?: () => number;
  // Runs before the base commit so the committed history can hold special files.
  setup?: (root: string, gitIn: GitIn) => Promise<void> | void;
}

const REFUSED = /maintenance_wip_capture_refused/;
const EXHAUSTED = /maintenance_resource_exhausted/;
// The O(1) metadata helper exits 1 on an unsafe .git shape; execScoped surfaces that as a plain
// subprocess failure. Both are refusals: nothing was launched after it and the source is retained.
const REFUSED_OR_HELPER = /maintenance_wip_capture_refused|subprocess exited 1/;
const MUTATING = ["write-tree", "commit-tree", "update-ref"] as const;
const CAP = 8 * 1024 * 1024;
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const BASE = "a1eba31831f9799a29857471514369d854ec8e72";
const BRANCH = "refs/heads/main";
const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
const gitEnv = { ...cleanEnv, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const gitCommands = ["status", "add", "write-tree", "commit-tree", "update-ref", "config", "ls-files", "rev-parse",
  "cat-file", "symbolic-ref", "for-each-ref", "diff-index", "diff-tree", "commit"];

// Precise launch label: the git subcommand, or the runner-owned helper named by the marker
// comment on its first script line (pin, census, payload), else the metadata/config helper.
function labelOf(argvIn: readonly string[]): string {
  const command = argvIn.find(arg => gitCommands.includes(arg));
  if (command) return command;
  const script = argvIn[1] === "-e" ? argvIn[2] ?? "" : "";
  for (const helper of ["pin", "census", "payload"]) if (script.startsWith("// uzi-wip-" + helper)) return helper;
  return "metadata";
}

// Replace .git/index with a byte-identical copy on a NEW inode (rename over), the strongest
// substitution that the checksum alone cannot see.
function replaceIndex(root: string): void {
  const index = path.join(root, ".git/index"), copy = index + ".replacement";
  const before = fsSync.statSync(index);
  fsSync.writeFileSync(copy, fsSync.readFileSync(index));
  fsSync.renameSync(copy, index);
  const after = fsSync.statSync(index);
  assert.notEqual(after.ino, before.ino, "the replacement must be a new inode");
}

async function harness(t: test.TestContext, dir: string, root: string, old: string, gitIn: GitIn, monotonicNow?: () => number) {
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture", monotonicNow });
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(admitted.ok, JSON.stringify(admitted));
  const git = new GitCache(dir, nullLogger());
  const signal = new AbortController();
  const events: string[] = [];
  const argvs: Array<{ label: string; argv: string[]; env: NodeJS.ProcessEnv }> = [];
  const visits = new Map<string, number>();
  // NUL-terminated rows each `git config --null --list` launch printed (the config allowlist listing).
  const configRows: number[] = [];
  let launches = 0, joins = 0;
  let intercept: Intercept | undefined;
  const owner = async (request: BoundaryProcessRequest) => {
    launches++;
    let argv = [...request.argv];
    // Under UZI_UID_SPLIT=1 the runner-uid wrapper (setpriv --reuid runner ...) would be prepended;
    // a non-root test user cannot initgroups, so the wrapper is recorded and stripped here.
    if (argv[0]?.endsWith("setpriv") && argv.includes("--")) { argv = argv.slice(argv.indexOf("--") + 1); }
    const label = labelOf(argv);
    events.push("launch:" + label);
    argvs.push({ label, argv, env: request.env });
    if (MaintenanceResources.active()) {
      if (label === "status") assert.equal(request.env.GIT_OPTIONAL_LOCKS, "0");
      if (gitCommands.includes(label)) assert.equal(request.env.GIT_NO_REPLACE_OBJECTS, "1");
    }
    const replacement = intercept?.({ ...request, argv });
    const child = replacement === undefined
      ? spawn(argv[0]!, argv.slice(1), { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] })
      : spawn(process.execPath, ["-e", replacement], { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
    if (label === "config" && replacement === undefined) {
      let rowsSeen = 0;
      configRows.push(0);
      const slot = configRows.length - 1;
      child.stdout.on("data", (chunk: Buffer) => { for (const byte of chunk) if (byte === 0) rowsSeen++; configRows[slot] = rowsSeen; });
    }
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => { joins++; events.push("join:" + label); resolve({ code: code ?? 1 }); });
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { child.kill("SIGTERM"); await completed; } };
  };
  const hooks = (options: HookOptions = {}): Callbacks => ({
    beforeStep: async (step, phase) => {
      events.push("before:" + step + ":" + phase);
      const key = step + ":" + phase, visit = (visits.get(key) ?? 0) + 1;
      visits.set(key, visit);
      await options.before?.(step, phase, visit);
    },
    afterRunnerGit: async step => {
      events.push("after:" + step);
      assert.equal(launches, joins);
      await options.after?.(step);
    },
  });
  const run = <T>(action: () => T) => MaintenanceResources.run(admitted.value, action);
  const owned = <T>(action: () => Promise<T>) => git.withBoundaryProcessSpawner(owner, signal.signal, action);
  const capture = (callbacks: Callbacks = hooks(), source = root, head = old) =>
    run(() => owned(() => git.captureMaintenanceWip(source, head, callbacks)));
  t.after(async () => {
    assert.equal(launches, joins);
    await ledger.finish(admitted.value, "transient");
  });
  return { root, dir, git, old, gitIn, run, owned, capture, hooks, events, argvs, signal, configRows,
    launches: () => launches,
    intercept: (fn: Intercept | undefined) => { intercept = fn; },
    resetVisits: () => visits.clear(),
    dirty: () => fs.writeFile(path.join(root, "tracked"), "changed"),
    head: () => gitIn("rev-parse", "HEAD").trim(),
    refs: () => gitIn("for-each-ref", "--format=%(refname) %(objectname)"),
    index: () => fs.readFile(path.join(root, ".git/index")),
    mutations: () => events.filter(e => (MUTATING as readonly string[]).map(c => "launch:" + c).includes(e))
      .map(e => e.slice("launch:".length)),
    launched: (label: string) => events.includes("launch:" + label),
  };
}

async function tmpDir(t: test.TestContext): Promise<string> {
  const dir = await fs.realpath(await fs.mkdtemp(path.join(os.tmpdir(), "uzi-wip-h-")));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  return dir;
}

async function fixture(t: test.TestContext, options: FixtureOptions = {}) {
  const dir = await tmpDir(t);
  const root = path.join(dir, "source");
  await fs.mkdir(root);
  const gitIn: GitIn = (...args) => execFileSync("/usr/bin/git", ["-C", root, ...args],
    { env: gitEnv, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
  gitIn("init", "-b", "main");
  for (const [key, value] of [["user.name", "fixture"], ["user.email", "fixture@example.com"],
    ["commit.gpgsign", "false"], ["maintenance.auto", "false"], ["gc.auto", "0"], ["core.fsmonitor", "false"]])
    gitIn("config", key!, value!);
  await fs.writeFile(path.join(root, "tracked"), "original");
  await fs.writeFile(path.join(root, ".gitignore"), "ignored\n");
  await options.setup?.(root, gitIn);
  gitIn("add", "-A"); gitIn("commit", "-m", "base");
  const old = gitIn("rev-parse", "HEAD").trim();
  const initialIndex = await fs.readFile(path.join(root, ".git/index"));
  const h = await harness(t, dir, root, old, gitIn, options.monotonicNow);
  return { ...h, initialIndex };
}
type Fixture = Awaited<ReturnType<typeof fixture>>;

// A spy over MaintenanceResources.consume that records every successful charge per category.
// Failed charges are recorded separately in `attempts`: they are never added to the totals.
function meter(t: test.TestContext) {
  const totals: Partial<Record<MaintenanceResource, number>> = {};
  const calls: Array<[MaintenanceResource, number]> = [];
  const attempts: Array<[MaintenanceResource, number]> = [];
  const original = MaintenanceResources.consume.bind(MaintenanceResources);
  t.mock.method(MaintenanceResources, "consume", (resource: MaintenanceResource, amount: number) => {
    attempts.push([resource, amount]);
    original(resource, amount);
    totals[resource] = (totals[resource] ?? 0) + amount;
    calls.push([resource, amount]);
  });
  return { totals, calls, attempts };
}

// Slot limits frozen in recovery-maintenance.ts (R2): the test only reads them to leave exact headroom.
const LIMIT = { sourceEntries: 65536, sourceBytes: 256 * 1024 ** 2 } as const;

async function treeOf(f: { gitIn: GitIn }, rev = "HEAD"): Promise<Map<string, string>> {
  const out = new Map<string, string>();
  for (const row of f.gitIn("ls-tree", "-r", "-z", rev).split("\0").filter(Boolean)) {
    const tab = row.indexOf("\t");
    const [mode, , oid] = row.slice(0, tab).split(" ");
    out.set(row.slice(tab + 1), mode + " " + oid);
  }
  return out;
}

// Nothing happened to the source: HEAD, every ref and the worktree bytes are as before.
function assertUnmoved(f: Fixture, refsBefore: string) {
  assert.equal(f.head(), f.old, "HEAD unchanged at the source SHA");
  assert.equal(f.refs(), refsBefore, "no ref written");
}

// Forge every cached stat field of `tracked` (except the mode word) so Git believes it is clean.
async function forgeStatCache(f: Fixture, name: string) {
  const indexPath = path.join(f.root, ".git/index");
  const index = await f.index();
  const st = await fs.stat(path.join(f.root, name), { bigint: true });
  let offset = 12, found = false;
  for (let i = 0; i < index.readUInt32BE(8); i++) {
    const end = index.indexOf(0, offset + 62);
    assert.ok(end >= offset + 62);
    if (index.toString("utf8", offset + 62, end) === name) {
      const fields = [st.ctimeNs / 1000000000n, st.ctimeNs % 1000000000n, st.mtimeNs / 1000000000n, st.mtimeNs % 1000000000n,
        st.dev, st.ino, undefined, st.uid, st.gid, st.size];
      fields.forEach((value, field) => {
        if (value !== undefined) index.writeUInt32BE(Number(BigInt.asUintN(32, value)), offset + field * 4);
      });
      found = true;
    }
    offset += Math.ceil((end + 1 - offset) / 8) * 8;
  }
  assert.ok(found);
  createHash("sha1").update(index.subarray(0, -20)).digest().copy(index, index.length - 20);
  await fs.writeFile(indexPath, index);
  const newer = Number(st.mtimeNs / 1000000000n) + 10;
  await fs.utimes(indexPath, newer, newer);
}

describe("hash-attested maintenance WIP capture (W0-H)", { skip: process.platform !== "linux" && "Linux /proc capture required" }, () => {
  test("plain clone/bundle transfer works from a nonrepository cwd", async t => {
    const cwd = await tmpDir(t);
    assert.deepEqual(await fs.readdir(cwd), []);
    const probe = spawnSync("/usr/bin/git", ["rev-parse", "--show-toplevel"], {
      cwd, env: gitEnv, encoding: "utf8", timeout: 5_000,
    });
    assert.equal(probe.error, undefined);
    assert.equal(probe.signal, null);
    assert.equal(probe.status, 128, probe.stdout + probe.stderr);
    assert.match(probe.stderr, /not a git repository/);

    const leaf = "clone HEAD = W, single parent, status clean, connectivity intact, and a plain clone/bundle of it works";
    const env: NodeJS.ProcessEnv = { ...gitEnv };
    delete env.NODE_TEST_CONTEXT;
    const child = spawnSync(process.execPath, [
      "--import", import.meta.resolve("tsx"),
      // Run the selected leaf in this process: a default isolated child would survive the launcher's SIGKILL.
      "--test", "--test-isolation=none", "--test-timeout=120000", "--test-reporter=tap",
      "--test-name-pattern=^hash-attested maintenance WIP capture \\(W0-H\\) " + leaf + "$",
      fileURLToPath(import.meta.url),
    ], { cwd, env, encoding: "utf8", timeout: 110_000, killSignal: "SIGKILL", maxBuffer: 1024 * 1024 });
    const diagnostics = child.stdout + child.stderr;
    assert.equal(child.error, undefined, diagnostics);
    assert.equal(child.signal, null, diagnostics);
    assert.equal(child.status, 0, diagnostics);
    const passedLeaves = child.stdout.split("\n")
      .filter(line => line.trim().replace(/^ok \d+ - /, "") === leaf);
    assert.equal(passedLeaves.length, 1, diagnostics);
    for (const summary of ["tests 1", "pass 1", "fail 0", "cancelled 0", "skipped 0"])
      assert.ok(child.stdout.split("\n").includes("# " + summary), diagnostics);
  });

  // ---------------------------------------------------------------- acceptance 1: refusals

  // Refused in P0/P1, before the status launch: nothing but read-only probes ran.
  const early: Array<{ name: string; setup?: FixtureOptions["setup"]; arrange: (f: Fixture) => Promise<void> | void }> = [
    { name: "assume-unchanged flag", arrange: f => { f.gitIn("update-index", "--assume-unchanged", "tracked"); } },
    { name: "skip-worktree flag", arrange: f => { f.gitIn("update-index", "--skip-worktree", "tracked"); } },
    { name: "unmerged entry", arrange: f => {
      const blob = f.gitIn("rev-parse", "HEAD:tracked").trim();
      execFileSync("/usr/bin/git", ["-C", f.root, "update-index", "--index-info"], {
        env: gitEnv, input: "0 " + "0".repeat(40) + "\ttracked\n100644 " + blob + " 1\ttracked\n" });
    } },
    { name: "gitlink", arrange: f => { f.gitIn("update-index", "--add", "--cacheinfo", "160000", f.old, "submodule"); } },
    { name: "shared (split) index", arrange: f => { f.gitIn("update-index", "--split-index"); } },
    { name: "redirected index (symlink)", arrange: async f => {
      await fs.rename(path.join(f.root, ".git/index"), path.join(f.root, ".git/index.real"));
      await fs.symlink("index.real", path.join(f.root, ".git/index"));
    } },
    { name: "hard-linked index", arrange: async f => { await fs.link(path.join(f.root, ".git/index"), path.join(f.root, ".git/index.alias")); } },
    { name: "stale index.lock", arrange: async f => { await fs.writeFile(path.join(f.root, ".git/index.lock"), ""); } },
    { name: "tracked bad path (backslash)", setup: root => { fsSync.writeFileSync(path.join(root, "back\\slash"), "x"); }, arrange: () => undefined },
    { name: "detached HEAD", arrange: f => { f.gitIn("checkout", "--detach"); } },
    { name: "branch ref that is itself symbolic", arrange: f => {
      f.gitIn("symbolic-ref", "refs/heads/alias", BRANCH); f.gitIn("symbolic-ref", "HEAD", "refs/heads/alias");
    } },
  ];
  for (const c of early) test("refuses " + c.name + " before any status launch: HEAD, refs and index unmoved", async t => {
    const f = await fixture(t, c.setup ? { setup: c.setup } : {});
    await f.dirty();
    await c.arrange(f);
    const refsBefore = f.refs(), indexBefore = await f.index();
    await assert.rejects(f.capture(), c.name === "redirected index (symlink)" || c.name === "stale index.lock" ? REFUSED_OR_HELPER : REFUSED);
    assert.ok(!f.launched("status"), "no status launch");
    assert.ok(!f.launched("add"), "no add launch");
    assert.deepEqual(f.mutations(), []);
    assertUnmoved(f, refsBefore);
    assert.deepEqual(await f.index(), indexBefore);
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "changed", "source bytes preserved");
    if (c.name === "stale index.lock") assert.ok(fsSync.existsSync(path.join(f.root, ".git/index.lock")), "the foreign lock is left alone");
  });

  // Refused after the status read-only passes, still before add -A.
  const late: Array<{ name: string; arrange: (f: Fixture) => Promise<void> | void }> = [
    { name: "untracked nested repository", arrange: async f => {
      execFileSync("/usr/bin/git", ["init", "-q", "-b", "main", path.join(f.root, "nested")], { env: gitEnv });
      await fs.writeFile(path.join(f.root, "nested/file"), "inner");
    } },
    { name: "untracked bad path (control byte)", arrange: async f => { await fs.writeFile(path.join(f.root, "ctl\u0001name"), "x"); } },
    { name: "symlinked ancestor", arrange: async f => {
      await fs.rename(path.join(f.root, "dir"), path.join(f.root, "dir.real"));
      await fs.symlink("dir.real", path.join(f.root, "dir"));
    } },
  ];
  for (const c of late) test("refuses " + c.name + " before add -A: HEAD, refs and index unmoved", async t => {
    const f = await fixture(t, { setup: async root => {
      await fs.mkdir(path.join(root, "dir")); await fs.writeFile(path.join(root, "dir/inner"), "inner");
    } });
    await f.dirty();
    await c.arrange(f);
    const refsBefore = f.refs(), indexBefore = await f.index();
    await assert.rejects(f.capture(), REFUSED);
    assert.ok(f.launched("status"), "the refusal is a manifest/census refusal, after status");
    assert.ok(!f.launched("add"), "no add launch");
    assert.deepEqual(f.mutations(), []);
    assertUnmoved(f, refsBefore);
    assert.deepEqual(await f.index(), indexBefore);
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "changed");
  });

  test("a tracked file replaced by a directory is refused before add", async t => {
    const f = await fixture(t);
    await fs.rm(path.join(f.root, "tracked"));
    await fs.mkdir(path.join(f.root, "tracked"));
    await fs.writeFile(path.join(f.root, "tracked/inner"), "inner");
    const refsBefore = f.refs(), indexBefore = await f.index();
    await assert.rejects(f.capture(), REFUSED);
    assert.ok(!f.launched("add"));
    assert.deepEqual(f.mutations(), []);
    assertUnmoved(f, refsBefore);
    assert.deepEqual(await f.index(), indexBefore);
    assert.equal(await fs.readFile(path.join(f.root, "tracked/inner"), "utf8"), "inner", "bytes kept");
  });

  test("an intent-to-add entry (git add -N) is refused before add", async t => {
    const f = await fixture(t);
    await f.dirty();
    await fs.writeFile(path.join(f.root, "intent"), "planned");
    f.gitIn("add", "-N", "intent");
    const refsBefore = f.refs(), indexBefore = await f.index();
    await assert.rejects(f.capture(), REFUSED);
    assert.ok(!f.launched("add"));
    assert.deepEqual(f.mutations(), []);
    assertUnmoved(f, refsBefore);
    assert.deepEqual(await f.index(), indexBefore);
    assert.equal(await fs.readFile(path.join(f.root, "intent"), "utf8"), "planned");
  });

  test("a conversion attribute refuses at the pre-commit tree proof: no commit-tree, no update-ref", async t => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, ".gitattributes"), "conv text eol=crlf\n");
    await fs.writeFile(path.join(f.root, "conv"), "a\r\nb\r\n");
    const refsBefore = f.refs();
    await assert.rejects(f.capture(), REFUSED);
    // Git normalised CRLF to LF on add, so the staged blob is not the hashed worktree content.
    assert.ok(f.launched("add"));
    assert.deepEqual(f.mutations(), ["write-tree"]);
    assertUnmoved(f, refsBefore);
    assert.equal(await fs.readFile(path.join(f.root, "conv"), "utf8"), "a\r\nb\r\n", "worktree bytes preserved");
  });

  test("file growing mid-read refuses before add: the payload identity check", async t => {
    const f = await fixture(t);
    await f.dirty();
    let grown = false;
    f.intercept(() => {
      if (f.argvs.at(-1)?.label === "payload" && !grown) {
        grown = true;
        fsSync.appendFileSync(path.join(f.root, "tracked"), " and then some");
      }
      return undefined;
    });
    const refsBefore = f.refs();
    await assert.rejects(f.capture(), REFUSED);
    assert.ok(grown, "the payload helper launched and the file grew before it read");
    assert.ok(!f.launched("add"));
    assertUnmoved(f, refsBefore);
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "changed and then some");
  });

  test("staged and unstaged changes that cancel out refuse before add, index left staged", async t => {
    const f = await fixture(t);
    await f.dirty();
    f.gitIn("add", "tracked");
    await fs.writeFile(path.join(f.root, "tracked"), "original");
    assert.equal(f.gitIn("status", "--porcelain"), "MM tracked\n");
    const refsBefore = f.refs(), indexBefore = await f.index();
    await assert.rejects(f.capture(), REFUSED);
    assert.ok(!f.launched("add"));
    assert.deepEqual(f.mutations(), []);
    assertUnmoved(f, refsBefore);
    assert.deepEqual(await f.index(), indexBefore);
    assert.equal(f.gitIn("show", ":tracked"), "changed");
  });

  // Each point where the index is replaced (same bytes, new inode) is caught by the pin
  // bracketing the launch, callback or stage. Pre-CAS: HEAD stays at the source SHA.
  const swapCases: Array<{ name: string; install: (f: Fixture, swap: () => void) => { intercept?: Intercept; hooks?: HookOptions };
    forbidden: string[] }> = [
    { name: "while status runs (P2)", forbidden: ["add", ...MUTATING],
      install: (f, swap) => { let done = false; return { intercept: () => { if (f.argvs.at(-1)?.label === "status" && !done) { done = true; swap(); } return undefined; } }; } },
    { name: "while the census runs (P3)", forbidden: ["add", ...MUTATING],
      install: (f, swap) => { let done = false; return { intercept: () => { if (f.argvs.at(-1)?.label === "census" && !done) { done = true; swap(); } return undefined; } }; } },
    { name: "while the payload helper runs (P3)", forbidden: ["add", ...MUTATING],
      install: (f, swap) => { let done = false; return { intercept: () => { if (f.argvs.at(-1)?.label === "payload" && !done) { done = true; swap(); } return undefined; } }; } },
    { name: "after status joins", forbidden: ["add", ...MUTATING],
      install: (_f, swap) => ({ hooks: { after: step => { if (step === "status") swap(); } } }) },
    { name: "before add -A (in its dispatch callback)", forbidden: ["add", ...MUTATING],
      install: (_f, swap) => ({ hooks: { before: (step, phase) => { if (step === "add" && phase === "dispatch") swap(); } } }) },
    { name: "between add -A and write-tree (after add joins)", forbidden: [...MUTATING],
      install: (_f, swap) => ({ hooks: { after: step => { if (step === "add") swap(); } } }) },
    { name: "across the write-tree dispatch callback", forbidden: [...MUTATING],
      install: (_f, swap) => ({ hooks: { before: (step, phase, visit) => { if (step === "commit" && phase === "dispatch" && visit === 1) swap(); } } }) },
    { name: "after the post-write-tree pin, before commit-tree", forbidden: ["commit-tree", "update-ref"],
      install: (_f, swap) => ({ hooks: { before: (step, phase, visit) => { if (step === "commit" && phase === "dispatch" && visit === 2) swap(); } } }) },
    { name: "before update-ref", forbidden: ["update-ref"],
      install: (_f, swap) => ({ hooks: { before: (step, phase, visit) => { if (step === "commit" && phase === "dispatch" && visit === 3) swap(); } } }) },
  ];
  for (const c of swapCases) test("index replaced " + c.name + " refuses pre-CAS with HEAD at the source SHA", async t => {
    const f = await fixture(t);
    await f.dirty();
    const refsBefore = f.refs();
    const { intercept, hooks } = c.install(f, () => replaceIndex(f.root));
    if (intercept) f.intercept(intercept);
    await assert.rejects(f.capture(f.hooks(hooks)), REFUSED);
    for (const label of c.forbidden) assert.ok(!f.launched(label), "no " + label + " launch after the replacement");
    assertUnmoved(f, refsBefore);
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "changed");
  });

  // Configuration that turns unsafe at each point is caught by the fresh check before the next effect.
  const configCases: Array<{ name: string; forbidden: string[] }> = [
    { name: "after status", forbidden: ["add", ...MUTATING] },
    { name: "after add", forbidden: [...MUTATING] },
    { name: "before write-tree", forbidden: [...MUTATING] },
    { name: "before commit-tree", forbidden: ["commit-tree", "update-ref"] },
    { name: "before update-ref", forbidden: ["update-ref"] },
  ];
  for (const c of configCases) test("configuration turned unsafe " + c.name + " refuses before the next effect", async t => {
    const f = await fixture(t);
    await f.dirty();
    const refsBefore = f.refs();
    const plant = () => { f.gitIn("config", "filter.plant.process", "touch marker"); };
    const hooks: HookOptions = {
      after: step => { if ((c.name === "after status" && step === "status") || (c.name === "after add" && step === "add")) plant(); },
      before: (step, phase, visit) => {
        if (step !== "commit" || phase !== "dispatch") return;
        if ((c.name === "before write-tree" && visit === 1) || (c.name === "before commit-tree" && visit === 2) ||
            (c.name === "before update-ref" && visit === 3)) plant();
      },
    };
    await assert.rejects(f.capture(f.hooks(hooks)), REFUSED);
    for (const label of c.forbidden) assert.ok(!f.launched(label), "no " + label + " launch");
    assertUnmoved(f, refsBefore);
    await assert.rejects(fs.stat(path.join(f.root, "marker")), { code: "ENOENT" });
  });

  test("an index.lock appearing after status refuses before add and is left alone", async t => {
    const f = await fixture(t);
    await f.dirty();
    const refsBefore = f.refs();
    await assert.rejects(f.capture(f.hooks({ after: async step => {
      if (step === "status") await fs.writeFile(path.join(f.root, ".git/index.lock"), "");
    } })), REFUSED_OR_HELPER);
    assert.ok(!f.launched("add"));
    assertUnmoved(f, refsBefore);
    assert.ok(fsSync.existsSync(path.join(f.root, ".git/index.lock")));
  });

  test("each of write-tree, commit-tree and update-ref is immediately preceded by its own (commit, dispatch) callback", async t => {
    const f = await fixture(t);
    await f.dirty();
    assert.equal((await f.capture()).kind, "committed");
    const mutating = f.events.filter(e => e === "before:commit:dispatch" || e === "launch:write-tree" ||
      e === "launch:commit-tree" || e === "launch:update-ref");
    assert.deepEqual(mutating, [
      "before:commit:dispatch", "launch:write-tree",
      "before:commit:dispatch", "launch:commit-tree",
      "before:commit:dispatch", "launch:update-ref",
    ]);
    // No other caller callback sits between a dispatch callback and the launch it guards.
    const sequence = f.events.filter(e => /^(before|after):/.test(e) || /^launch:(write-tree|commit-tree|update-ref)$/.test(e));
    for (const [i, event] of sequence.entries()) {
      if (event.startsWith("launch:")) assert.equal(sequence[i - 1], "before:commit:dispatch");
    }
    assert.equal(sequence.at(-1), "after:commit", "afterRunnerGit(commit) follows update-ref");
  });

  test("custody closed between add and update-ref: pre-CAS refusal, no ref written", async t => {
    const f = await fixture(t);
    await f.dirty();
    const refsBefore = f.refs();
    await assert.rejects(f.capture(f.hooks({ before: (step, phase, visit) => {
      if (step === "commit" && phase === "dispatch" && visit === 3) throw Error("custody closed");
    } })), /custody closed/);
    assert.deepEqual(f.mutations(), ["write-tree", "commit-tree"], "update-ref never launched");
    assert.ok(f.events.includes("after:commit"), "caller quiescence still joined");
    assertUnmoved(f, refsBefore);
  });

  test("branch moved before update-ref: the CAS refuses, the competing tip stays, nothing written", async t => {
    const f = await fixture(t);
    await f.dirty();
    let competing = "";
    await assert.rejects(f.capture(f.hooks({ before: (step, phase, visit) => {
      if (step !== "commit" || phase !== "dispatch" || visit !== 3) return;
      competing = f.gitIn("commit-tree", f.gitIn("rev-parse", "HEAD^{tree}").trim(), "-p", f.old, "-m", "competing writer").trim();
      f.gitIn("update-ref", BRANCH, competing, f.old);
    // `s`: newer Git (2.55) prints a grafts deprecation hint line before the fatal CAS error.
    } })), /update-ref --no-deref refs\/heads\/main .* failed: .*cannot lock ref 'refs\/heads\/main': is at .* but expected/s);
    assert.deepEqual(f.mutations(), ["write-tree", "commit-tree", "update-ref"], "the CAS was attempted");
    assert.match(competing, /^[0-9a-f]{40}$/);
    assert.equal(f.head(), competing, "the competing tip is kept");
    assert.equal(f.refs(), BRANCH + " " + competing + "\n", "the capture wrote no ref");
    assert.ok(!f.gitIn("reflog", "show", BRANCH).includes(WIP_PARK_COMMIT_PREFIX), "no WIP entry in the branch reflog");
    assert.equal(f.gitIn("show", "-s", "--format=%s", "HEAD").trim(), "competing writer");
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "changed");
  });

  // Post-CAS: W is on the branch; a P5 failure refuses with the source retained, nothing rewound.
  const tamper: Array<[string, (commit: string) => string]> = [
    ["wrong parent", c => c.replace(/^parent [0-9a-f]{40}$/m, "parent " + "b".repeat(40))],
    ["wrong identity", c => c.replace(/^author .*? </m, "author Mallory <")],
    ["wrong date", c => c.replace(/^(committer .*> )(\d+)/m, (_m, head: string, stamp: string) => head + (Number(stamp) + 1))],
  ];
  for (const [name, mutate] of tamper) test("post-capture proof: " + name + " refuses with W retained, no rewind", async t => {
    const f = await fixture(t);
    await f.dirty();
    await assert.rejects(f.capture(f.hooks({ after: step => {
      if (step !== "commit") return;
      const actual = f.gitIn("--no-replace-objects", "cat-file", "commit", f.head());
      const changed = mutate(actual);
      assert.notEqual(changed, actual, "the tamper applied");
      f.intercept(request => request.argv.includes("cat-file") ? "process.stdout.write(" + JSON.stringify(changed) + ")" : undefined);
    } })), REFUSED);
    const w = f.head();
    assert.notEqual(w, f.old, "W is retained on the branch");
    assert.equal(f.refs(), BRANCH + " " + w + "\n");
    assert.equal(f.gitIn("show", "-s", "--format=%P", w).trim(), f.old);
    assert.equal(f.gitIn("show", w + ":tracked"), "changed");
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "changed");
  });

  // A source commit whose committer header Git would normalise (or that is not canonical) must be
  // refused in P0, before any commit-group launch. Git itself accepts these hand-made commits.
  for (const [name, mutate] of [
    ["timezone -0060", (c: string) => c.replace(/^(committer .*> \d+) [+-]\d{4}$/m, "$1 -0060")],
    ["timezone -0000", (c: string) => c.replace(/^(committer .*> \d+) [+-]\d{4}$/m, "$1 -0000")],
    ["timezone +9999", (c: string) => c.replace(/^(committer .*> \d+) [+-]\d{4}$/m, "$1 +9999")],
    ["leading-zero timestamp", (c: string) => c.replace(/^(committer .*> )(\d+)( [+-]\d{4})$/m, "$100$2$3")],
  ] as const) test("source commit with a non-canonical committer date (" + name + ") is refused before any commit-group launch", async t => {
    const f = await fixture(t);
    const original = f.gitIn("cat-file", "commit", "HEAD");
    const crafted = mutate(original);
    assert.notEqual(crafted, original, "the craft applied");
    const sha = execFileSync("/usr/bin/git", ["-C", f.root, "hash-object", "--literally", "-t", "commit", "-w", "--stdin"],
      { env: gitEnv, input: crafted, encoding: "utf8" }).trim();
    f.gitIn("update-ref", BRANCH, sha, f.old);
    await f.dirty();
    const refsBefore = f.refs();
    assert.equal(refsBefore, BRANCH + " " + sha + "\n");
    await assert.rejects(f.capture(undefined, f.root, sha), REFUSED);
    assert.deepEqual(f.mutations(), [], "no write-tree, commit-tree or update-ref launched");
    assert.ok(!f.launched("add"));
    assert.equal(f.head(), sha, "HEAD stays at the crafted source SHA");
    assert.equal(f.refs(), refsBefore);
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "changed");
  });

  test("a non-canonical commit reaching commit-tree is caught before the CAS: update-ref never launches", async t => {
    const f = await fixture(t);
    await f.dirty();
    const refsBefore = f.refs();
    let altered = false;
    f.intercept(request => {
      // The FIRST cat-file of a commit that is not the source: the pre-CAS proof of W.
      const target = request.argv.at(-1) ?? "";
      if (f.argvs.at(-1)?.label !== "cat-file" || !f.launched("commit-tree") || altered || target === f.old) return undefined;
      altered = true;
      const actual = f.gitIn("--no-replace-objects", "cat-file", "commit", target);
      const changed = actual.replace(/^(committer .*> \d+) [+-]\d{4}$/m, "$1 -0060");
      assert.notEqual(changed, actual);
      return "process.stdout.write(" + JSON.stringify(changed) + ")";
    });
    await assert.rejects(f.capture(), REFUSED);
    assert.ok(altered, "the pre-CAS cat-file of W was intercepted");
    assert.deepEqual(f.mutations(), ["write-tree", "commit-tree"], "update-ref never launched");
    assertUnmoved(f, refsBefore);
  });

  test("drift after capture refuses with W retained: the post-capture status proof", async t => {
    const f = await fixture(t);
    await f.dirty();
    await assert.rejects(f.capture(f.hooks({ after: async step => {
      if (step === "commit") await fs.writeFile(path.join(f.root, "tracked"), "drifted");
    } })), REFUSED);
    assert.notEqual(f.head(), f.old);
    assert.equal(f.gitIn("show", "HEAD:tracked"), "changed", "W holds the captured bytes");
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "drifted");
  });

  test("index replaced after update-ref refuses at the post-write-tree pin with W retained", async t => {
    const f = await fixture(t);
    await f.dirty();
    await assert.rejects(f.capture(f.hooks({ after: step => { if (step === "commit") replaceIndex(f.root); } })), REFUSED);
    assert.notEqual(f.head(), f.old, "post-CAS: W retained, never rewound");
    assert.equal(f.gitIn("show", "HEAD:tracked"), "changed");
  });

  test("unsafe config planted in the commit callback refuses at P5 before any P5 launch, W retained", async t => {
    const f = await fixture(t);
    await f.dirty();
    await assert.rejects(f.capture(f.hooks({ after: step => {
      if (step === "commit") f.gitIn("config", "filter.planted.clean", "touch planted");
    } })), REFUSED);
    assert.notEqual(f.head(), f.old, "post-CAS: W retained, never rewound");
    assert.equal(f.gitIn("show", "HEAD:tracked"), "changed");
    const after = f.events.indexOf("after:commit");
    assert.deepEqual(f.events.slice(after).filter(e => e.startsWith("launch:")),
      ["launch:pin", "launch:metadata", "launch:config"], "only the pin and config check ran after the callback");
  });

  test("clean: an index replaced in the status callback refuses instead of returning clean", async t => {
    const f = await fixture(t);
    const refsBefore = f.refs();
    await assert.rejects(f.capture(f.hooks({ after: step => { if (step === "status") replaceIndex(f.root); } })), REFUSED);
    assert.deepEqual(f.mutations(), []);
    assert.ok(!f.launched("add"));
    assertUnmoved(f, refsBefore);
  });

  test("stage-1 census over cap refuses before any lstat", async t => {
    const f = await fixture(t);
    const m = meter(t);
    await f.dirty();
    const refsBefore = f.refs();
    await assert.rejects(f.capture(f.hooks({ before: (step, phase) => {
      // Leave exactly one sourceEntries unit: the single status row fits, the two-path census does not.
      if (step === "status" && phase === "dispatch")
        MaintenanceResources.consume("sourceEntries", LIMIT.sourceEntries - (m.totals.sourceEntries ?? 0) - 1);
    } })), EXHAUSTED);
    assert.ok(!f.launched("census"), "no census helper launched, so no lstat");
    assert.ok(!f.launched("payload"));
    assert.ok(!f.launched("add"));
    assert.deepEqual(f.mutations(), []);
    assertUnmoved(f, refsBefore);
    assert.deepEqual(m.attempts.at(-1), ["sourceEntries", 2], "the failing charge was the census precharge of two paths");
  });

  test("stage-2 payload over cap refuses before any open and refunds nothing", async t => {
    const f = await fixture(t);
    const m = meter(t);
    await f.dirty();
    const refsBefore = f.refs();
    await assert.rejects(f.capture(f.hooks({ before: (step, phase) => {
      // Payload = "changed" (7) + ".gitignore" (8) = 15 bytes; leave 14.
      if (step === "status" && phase === "dispatch")
        MaintenanceResources.consume("sourceBytes", LIMIT.sourceBytes - (m.totals.sourceBytes ?? 0) - 14);
    } })), EXHAUSTED);
    assert.equal(f.events.filter(e => e === "launch:census").length, 1, "stage 1 ran");
    assert.ok(!f.launched("payload"), "no payload helper launched, so no open");
    assert.ok(!f.launched("add"));
    assertUnmoved(f, refsBefore);
    assert.deepEqual(m.attempts.at(-1), ["sourceBytes", 15], "the payload byte precharge failed");
    // The stage-2 per-step precharge (5 entries x 2 files) was charged before the byte charge failed and is not refunded.
    assert.deepEqual(m.attempts.at(-2), ["sourceEntries", 10]);
    assert.deepEqual(m.calls.at(-1), ["sourceEntries", 10], "the charge stays recorded, not reversed");
    // The slot is revoked by the exhaustion: a later tiny consume still fails.
    assert.throws(() => f.run(() => MaintenanceResources.consume("sourceEntries", 1)), /maintenance_/);
    assert.throws(() => f.run(() => MaintenanceResources.consume("launches", 1)), /maintenance_/);
  });

  // Stage 2 opens a regular file with no pre-open lstat: open + fstat identity carry the check.
  const swaps: Array<{ name: string; swap: (target: string) => void }> = [
    { name: "none (positive control)", swap: () => undefined },
    { name: "symlink", swap: target => { fsSync.rmSync(target); fsSync.symlinkSync("tracked.real", target); } },
    { name: "FIFO", swap: target => { fsSync.rmSync(target); execFileSync("mkfifo", [target]); } },
  ];
  for (const c of swaps) test("a regular file swapped for " + c.name + " between census and payload", async t => {
    const f = await fixture(t);
    await f.dirty();
    await fs.writeFile(path.join(f.root, "tracked.real"), "changed");
    const refsBefore = f.refs();
    let swapped = 0;
    f.intercept(request => {
      const script = request.argv[1] === "-e" ? request.argv[2] ?? "" : "";
      if (script.startsWith("// uzi-wip-payload") && swapped++ === 0) c.swap(path.join(f.root, "tracked"));
      return undefined;
    });
    if (c.name.startsWith("none")) {
      const result = await f.capture();
      assert.equal(result.kind, "committed");
      assert.equal(f.gitIn("show", "HEAD:tracked"), "changed");
    } else {
      await assert.rejects(f.capture(), REFUSED);
      assert.ok(!f.launched("add"), "refused in the payload stage, before add");
      assert.deepEqual(f.mutations(), []);
      assertUnmoved(f, refsBefore);
    }
    assert.equal(swapped, 1, "the swap ran exactly at the payload launch");
  });

  // No pre-open lstat: the opened fd's isFile/identity check alone keeps a replacement's bytes unread.
  // The payload helper is relaunched with fs.readSync observed (inode per read) in a test-only log.
  for (const replace of [false, true]) test("payload reads " + (replace
    ? "nothing from a regular file replaced between census and payload"
    : "the unchanged census-time file (positive control)"), async t => {
    const f = await fixture(t);
    await f.dirty();
    const target = path.join(f.root, "tracked"), log = path.join(f.dir, "payload-reads.log");
    const original = fsSync.statSync(target, { bigint: true }).ino.toString();
    let replacement = "";
    f.intercept(request => {
      const script = request.argv[1] === "-e" ? request.argv[2] ?? "" : "";
      if (!script.startsWith("// uzi-wip-payload")) return undefined;
      if (replace) {
        // Created while the original still exists, then renamed over it: never a reused inode.
        fsSync.writeFileSync(target + ".replacement", "CHANGED");
        fsSync.renameSync(target + ".replacement", target);
        replacement = fsSync.statSync(target, { bigint: true }).ino.toString();
      }
      return `process.argv = [process.argv[0], ...${JSON.stringify(request.argv.slice(3))}];
const observed = require("node:fs"), readSync = observed.readSync;
observed.readSync = function (fd, ...rest) {
  observed.appendFileSync(${JSON.stringify(log)}, observed.fstatSync(fd, { bigint: true }).ino + "\\n");
  return readSync.call(observed, fd, ...rest);
};
` + script;
    });
    const refsBefore = f.refs();
    const reads = () => fsSync.existsSync(log) ? fsSync.readFileSync(log, "utf8").split("\n").filter(Boolean) : [];
    if (replace) {
      await assert.rejects(f.capture(), REFUSED);
      assert.notEqual(replacement, original, "the replacement is a new inode");
      assert.ok(!reads().includes(replacement), "no byte of the replacement was read");
      assert.ok(!f.launched("add"));
      assertUnmoved(f, refsBefore);
    } else {
      assert.equal((await f.capture()).kind, "committed");
      assert.ok(reads().includes(original), "the observation sees reads of the census-time file");
    }
  });

  // ------------------------------------------------- acceptance 2: forged clean index entry

  for (const visible of [false, true]) test("forged clean index entry over modified worktree bytes is refused protected (visible WIP: " + visible + ")", async t => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, "tracked"), "modified");
    if (visible) await fs.writeFile(path.join(f.root, "visible"), "new");
    await forgeStatCache(f, "tracked");
    // Git itself is fooled: status reports nothing for the forged path.
    assert.equal(f.gitIn("--no-optional-locks", "status", "--porcelain=v1"), visible ? "?? visible\n" : "");
    assert.equal(f.gitIn("show", ":tracked"), "original");
    const before = await f.index(), refsBefore = f.refs();
    await assert.rejects(f.capture(), REFUSED);
    assert.ok(f.launched("status"), "the capture reaches the stat-cache check");
    assert.ok(!f.launched("add"), "no add on an undeclared difference");
    assert.deepEqual(f.mutations(), []);
    assertUnmoved(f, refsBefore);
    assert.deepEqual(await f.index(), before);
    assert.equal(await fs.readFile(path.join(f.root, "tracked"), "utf8"), "modified", "bytes preserved, nothing finalised");
  });

  test("a declared difference is preserved exactly, even when the stat cache is forged for another path", async t => {
    const f = await fixture(t, { setup: root => { fsSync.writeFileSync(path.join(root, "other"), "base-other"); } });
    // `other` is honestly edited (declared); `tracked` keeps pristine bytes with a forged stat cache (still clean).
    await fs.writeFile(path.join(f.root, "other"), "edited-other");
    await forgeStatCache(f, "tracked");
    const result = await f.capture();
    assert.equal(result.kind, "committed");
    assert.equal(f.gitIn("show", "HEAD:other"), "edited-other");
    assert.equal(f.gitIn("show", "HEAD:tracked"), "original");
  });

  // ------------------------------------------------------------ acceptance 3: racy-clean

  test("racy-clean: add, then rewrite at the same size within the same second: the new bytes are in W", async t => {
    const f = await fixture(t);
    const file = path.join(f.root, "racy");
    await fs.writeFile(file, "aaaa");
    f.gitIn("add", "racy");
    const stamp = await fs.stat(file);
    await fs.writeFile(file, "bbbb"); // same size, immediately
    await fs.utimes(file, stamp.atime, stamp.mtime); // same mtime too: only the content differs
    assert.equal(f.gitIn("show", ":racy"), "aaaa");
    const result = await f.capture();
    assert.equal(result.kind, "committed");
    assert.equal(f.gitIn("show", "HEAD:racy"), "bbbb");
    assert.equal(await fs.readFile(file, "utf8"), "bbbb");
  });

  test("racy-clean on a tracked path: same size and mtime rewrite after git add is captured", async t => {
    const f = await fixture(t);
    const file = path.join(f.root, "tracked"); // "original" is 8 bytes
    await fs.writeFile(file, "ORIGINAL");
    f.gitIn("add", "tracked");
    const stamp = await fs.stat(file);
    await fs.writeFile(file, "OriginaL");
    await fs.utimes(file, stamp.atime, stamp.mtime);
    const result = await f.capture();
    assert.equal(result.kind, "committed");
    assert.equal(f.gitIn("show", "HEAD:tracked"), "OriginaL");
  });

  // ---------------------------------------------------------------- acceptance 4: coverage

  test("coverage: staged-only, unstaged, staged-plus-unstaged, deletion, rename, untracked, tracked-but-ignored and symlink", async t => {
    const f = await fixture(t, { setup: async (root, gitIn) => {
      await fs.appendFile(path.join(root, ".gitignore"), "*.log\n");
      for (const [name, data] of [["unstaged", "u0"], ["staged", "s0"], ["both", "b0"], ["gone", "g0"], ["renamed", "r0"], ["kept.log", "k0"], ["exec", "x0"]] as const)
        await fs.writeFile(path.join(root, name), data);
      await fs.symlink("tracked", path.join(root, "link"));
      gitIn("add", "-f", "kept.log");
    } });
    const r = (name: string) => path.join(f.root, name);
    await fs.writeFile(r("unstaged"), "u1");
    await fs.writeFile(r("staged"), "s1"); f.gitIn("add", "staged");
    await fs.writeFile(r("both"), "b-staged"); f.gitIn("add", "both"); await fs.writeFile(r("both"), "b-final");
    await fs.rm(r("gone"));
    f.gitIn("mv", "renamed", "renamed2");
    await fs.writeFile(r("fresh"), "untracked-bytes");
    await fs.mkdir(r("nested/deeper"), { recursive: true }); await fs.writeFile(r("nested/deeper/leaf"), "leaf");
    await fs.writeFile(r("kept.log"), "k1");
    await fs.writeFile(r("ignored"), "private");
    await fs.writeFile(r("also.log"), "untracked and ignored");
    await fs.rm(r("link")); await fs.symlink("fresh", r("link"));
    await fs.symlink("nested/deeper/leaf", r("newlink"));
    await fs.chmod(r("exec"), 0o755);
    const tracked = await treeOf(f); // HEAD before
    const result = await f.capture();
    assert.equal(result.kind, "committed");
    const tree = await treeOf(f);
    const blob = (mode: string, data: string | Buffer) => mode + " " +
      createHash("sha1").update(`blob ${Buffer.byteLength(data)}\0`).update(data).digest("hex");
    assert.equal(tree.get("unstaged"), blob("100644", "u1"));
    assert.equal(tree.get("staged"), blob("100644", "s1"));
    assert.equal(tree.get("both"), blob("100644", "b-final"), "final-worktree semantics: staged intermediate content is not preserved");
    assert.equal(tree.has("gone"), false);
    assert.equal(tree.has("renamed"), false);
    assert.equal(tree.get("renamed2"), tracked.get("renamed"));
    assert.equal(tree.get("fresh"), blob("100644", "untracked-bytes"));
    assert.equal(tree.get("nested/deeper/leaf"), blob("100644", "leaf"));
    assert.equal(tree.get("kept.log"), blob("100644", "k1"), "a tracked-but-ignored path is captured");
    assert.equal(tree.has("ignored"), false);
    assert.equal(tree.has("also.log"), false, "untracked ignored files stay out");
    assert.equal(tree.get("link"), blob("120000", "fresh"), "a retargeted symlink is captured as a link");
    assert.equal(tree.get("newlink"), blob("120000", "nested/deeper/leaf"));
    assert.equal(tree.get("exec"), blob("100755", "x0"), "a mode-only change is captured");
    assert.equal(await fs.readFile(r("ignored"), "utf8"), "private");
    assert.equal(f.gitIn("status", "--porcelain=v1", "--untracked-files=all", "--ignored"), "!! also.log\n!! ignored\n");
  });

  // ------------------------------------------------------------- acceptance 5: determinism

  test("identical content reproduces an identical W (separate clone copy, and re-capture after a mixed reset)", async t => {
    const f = await fixture(t);
    await f.dirty();
    await fs.writeFile(path.join(f.root, "fresh"), "new");
    const copy = path.join(f.dir, "copy");
    await fs.cp(f.root, copy, { recursive: true, preserveTimestamps: true });
    // One capture per admitted slot (one pass per slot): each extra capture gets its own harness and ledger.
    const another = async (root: string) => harness(t, await tmpDir(t), root, f.old,
      (...args) => execFileSync("/usr/bin/git", ["-C", root, ...args], { env: gitEnv, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }));
    const first = await f.capture();
    assert.equal(first.kind, "committed");
    const second = await (await another(copy)).capture();
    assert.equal(second.kind, "committed");
    assert.equal(second.head, first.head, "same source SHA + same content => same W");
    // Re-capture in place: move the branch back (index reset to the source), the worktree stays dirty.
    f.gitIn("reset", "--mixed", "-q", f.old);
    assert.equal(f.head(), f.old);
    const third = await (await another(f.root)).capture();
    assert.equal(third.head, first.head);
    const sourceDate = f.gitIn("show", "-s", "--format=%cd", "--date=raw", f.old).trim();
    assert.equal(f.gitIn("show", "-s", "--format=%an|%ae|%ad|%cn|%ce|%cd", "--date=raw", first.head).trim(),
      [AGENT_GIT_IDENTITY.name, AGENT_GIT_IDENTITY.email, sourceDate, AGENT_GIT_IDENTITY.name, AGENT_GIT_IDENTITY.email, sourceDate].join("|"),
      "both dates equal the committer date of the source SHA");
  });

  // ---------------------------------------------------- acceptance 6: HEAD = W and clean

  test("clone HEAD = W, single parent, status clean, connectivity intact, and a plain clone/bundle of it works", async t => {
    const f = await fixture(t);
    await f.dirty();
    await fs.writeFile(path.join(f.root, "fresh"), "new");
    await fs.rm(path.join(f.root, ".gitignore"));
    const result = await f.capture();
    assert.equal(result.kind, "committed");
    assert.equal(f.head(), result.head);
    assert.equal(f.gitIn("rev-list", "--parents", "-n", "1", "HEAD").trim(), `${result.head} ${f.old}`, "exactly one parent");
    assert.equal(f.gitIn("symbolic-ref", "HEAD").trim(), BRANCH, "HEAD still names the branch");
    assert.equal(f.gitIn("status", "--porcelain=v1", "--untracked-files=all"), "", "status clean");
    assert.equal(f.gitIn("fsck", "--connectivity-only", "--strict", "--no-dangling"), "");
    assert.ok(f.gitIn("rev-list", "--objects", "HEAD").length > 0);
    // The existing transfer consumes a plain fetch/bundle of the branch; the runner-level FINAL boundary is
    // exercised by runner-maintenance-terminal-source.test.ts.
    const bundle = path.join(f.dir, "w.bundle");
    f.gitIn("bundle", "create", bundle, "main");
    execFileSync("/usr/bin/git", ["bundle", "verify", bundle], { cwd: f.root, env: gitEnv, stdio: "pipe" });
    const clone = path.join(f.dir, "transferred");
    execFileSync("/usr/bin/git", ["clone", "--no-local", "-q", f.root, clone], { env: gitEnv, stdio: "pipe" });
    assert.equal(execFileSync("/usr/bin/git", ["-C", clone, "rev-parse", "HEAD"], { env: gitEnv, encoding: "utf8" }).trim(), result.head);
    assert.equal(await fs.readFile(path.join(clone, "tracked"), "utf8"), "changed");
    assert.equal(await fs.readFile(path.join(clone, "fresh"), "utf8"), "new");
    await assert.rejects(fs.stat(path.join(clone, ".gitignore")), { code: "ENOENT" });
    const bundleClone = path.join(f.dir, "from-bundle");
    execFileSync("/usr/bin/git", ["clone", "-q", "-b", "main", bundle, bundleClone], { env: gitEnv, stdio: "pipe" });
    assert.equal(execFileSync("/usr/bin/git", ["-C", bundleClone, "rev-parse", "HEAD"], { env: gitEnv, encoding: "utf8" }).trim(), result.head);
    assert.equal(execFileSync("/usr/bin/git", ["-C", bundleClone, "status", "--porcelain=v1", "--untracked-files=all"],
      { env: gitEnv, encoding: "utf8" }), "");
    assert.equal(await fs.readFile(path.join(bundleClone, "tracked"), "utf8"), "changed");
    assert.equal(await fs.readFile(path.join(bundleClone, "fresh"), "utf8"), "new");
    await assert.rejects(fs.stat(path.join(bundleClone, ".gitignore")), { code: "ENOENT" });
  });

  // ------------------------------------------------------------ acceptance 7: both uid modes

  // Under the boundary spawner the OWNER (the worker, in production) applies the runner-uid wrapper
  // (`setpriv --reuid runner ...`); the capture hands it plain argv and env. The test user is not root,
  // so a real uid switch is out of reach here: this exercises the capture end to end with UZI_UID_SPLIT
  // unset and with UZI_UID_SPLIT=1 + UZI_RUNNER_PATH set (the helpers must resolve through the runner
  // PATH, which is what changes at this seam). The actual uid switch is covered by the worker-image
  // boundary tests and the hosted deploy; the harness would strip a setpriv prefix if one were present.
  for (const mode of ["single-uid", "uid-split"] as const) {
    test("both uid modes: " + mode + " (dirty commits; clean stays explicit)", async t => {
      const saved = { split: process.env.UZI_UID_SPLIT, path: process.env.UZI_RUNNER_PATH, ambient: process.env.PATH ?? "" };
      t.after(() => {
        if (saved.split === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = saved.split;
        if (saved.path === undefined) delete process.env.UZI_RUNNER_PATH; else process.env.UZI_RUNNER_PATH = saved.path;
      });
      if (mode === "uid-split") { process.env.UZI_UID_SPLIT = "1"; process.env.UZI_RUNNER_PATH = "/nonexistent-uzi-runner-extra:" + saved.ambient; }
      else { delete process.env.UZI_UID_SPLIT; delete process.env.UZI_RUNNER_PATH; }
      const f = await fixture(t);
      // Clean: explicit, HEAD/index unchanged and only status attempted.
      assert.deepEqual(await f.capture(), { kind: "clean", head: f.old, parent: null });
      assert.equal(f.head(), f.old);
      assert.deepEqual(await f.index(), f.initialIndex);
      assert.deepEqual(f.events.filter(e => e.startsWith("after:")), ["after:status"]);
      assert.deepEqual(f.mutations(), []);
      // Dirty: committed in the clone as the runner uid.
      f.resetVisits();
      await f.dirty();
      await fs.writeFile(path.join(f.root, "fresh"), "new");
      const result = await f.capture();
      assert.equal(result.kind, "committed");
      assert.equal(f.head(), result.head);
      assert.equal(f.gitIn("show", "HEAD:tracked"), "changed");
      assert.equal(f.gitIn("status", "--porcelain=v1"), "");
      const helperPaths = f.argvs.filter(a => ["pin", "census", "payload", "metadata"].includes(a.label)).map(a => a.env.PATH);
      assert.ok(helperPaths.length > 0);
      const expectedPath = mode === "uid-split" ? "/nonexistent-uzi-runner-extra:" + saved.ambient : saved.ambient;
      assert.ok(helperPaths.every(p => p === expectedPath), "helpers resolve through runnerPath(): UZI_RUNNER_PATH under the split, ambient PATH otherwise");
    });
  }

  // ----------------------------------------------- acceptance 8: C1 discriminator (stale cache-tree)

  test("C1: a stale cache-tree makes write-tree rewrite the index and the capture still commits (P5 pin = post-write-tree pin)", async t => {
    const f = await fixture(t);
    await f.dirty();
    const seen: Buffer[] = [];
    const result = await f.capture(f.hooks({ before: async (step, phase) => {
      if (step === "commit" && phase === "dispatch") seen.push(await f.index());
    }, after: async step => { if (step === "commit") seen.push(await f.index()); } }));
    assert.equal(result.kind, "committed");
    // seen: [before write-tree, before commit-tree, before update-ref, after update-ref]
    assert.equal(seen.length, 4);
    assert.notDeepEqual(seen[0], seen[1], "write-tree rewrote the index (stale cache-tree): the second sanctioned transition");
    assert.deepEqual(seen[1], seen[2]);
    assert.deepEqual(seen[2], seen[3], "the P5 pin equals the post-write-tree pin: nothing else moved the index");
    // Plumbing commit only: no porcelain `git commit`, one commit-tree whose output is W.
    assert.ok(f.argvs.every(a => !a.argv.includes("commit")), "no porcelain commit");
    assert.equal(f.argvs.filter(a => a.label === "commit-tree").length, 1);
    assert.equal(f.gitIn("show", "HEAD:tracked"), "changed");
    assert.equal(f.gitIn("status", "--porcelain=v1"), "");
    assert.deepEqual(await f.index(), seen[3]);
  });

  // ------------------------------------------------- acceptance 9: C2 policy precision

  test("C2: a mode-only change to a committed core file or oversize file refuses", async t => {
    for (const [name, bytes] of [["core", 16], ["huge.bin", CAP + 1]] as const) {
      const f = await fixture(t, { setup: root => { fsSync.writeFileSync(path.join(root, name), Buffer.alloc(bytes, 0x63)); } });
      await fs.chmod(path.join(f.root, name), 0o755);
      assert.equal(f.gitIn("status", "--porcelain=v1"), " M " + name + "\n");
      const refsBefore = f.refs();
      await assert.rejects(f.capture(), REFUSED, name);
      assert.ok(!f.launched("add"), name + ": refused before add");
      assertUnmoved(f, refsBefore);
    }
  });

  test("C2: a staged unsafe blob restored in the worktree is not mistaken for HEAD content", async t => {
    for (const [name, big] of [["core", Buffer.from("staged-core")], ["huge.bin", Buffer.alloc(CAP + 1, 0x64)]] as const) {
      const f = await fixture(t);
      await fs.writeFile(path.join(f.root, name), big);
      f.gitIn("add", name); // the unsafe blob is now staged (index == worktree, HEAD lacks the path)
      await fs.writeFile(path.join(f.root, name), "small");
      await fs.writeFile(path.join(f.root, name), big); // restored: worktree equals the staged blob again
      const refsBefore = f.refs();
      await assert.rejects(f.capture(), REFUSED, name);
      assert.ok(!f.launched("add"), name + ": refused before add");
      assertUnmoved(f, refsBefore);
      assert.deepEqual(await fs.readFile(path.join(f.root, name)), big);
    }
  });

  test("C2: deleting a committed core file succeeds", async t => {
    const f = await fixture(t, { setup: root => {
      fsSync.writeFileSync(path.join(root, "core"), "committed earlier");
      fsSync.writeFileSync(path.join(root, "core.77"), "also committed");
    } });
    await fs.rm(path.join(f.root, "core")); await fs.rm(path.join(f.root, "core.77"));
    const result = await f.capture();
    assert.equal(result.kind, "committed");
    const tree = await treeOf(f);
    assert.equal(tree.has("core"), false);
    assert.equal(tree.has("core.77"), false);
    assert.ok(tree.has("tracked"));
  });

  test("C2: a working-tree-encoding file under 8 MiB raw that stages over 8 MiB refuses at the pre-commit tree proof", async t => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, ".gitattributes"), "enc.txt working-tree-encoding=ISO-8859-1\n");
    // 5 MiB of U+00E9 as ISO-8859-1 is 10 MiB as the UTF-8 blob Git stages.
    await fs.writeFile(path.join(f.root, "enc.txt"), Buffer.alloc(5 * 1024 ** 2, 0xe9));
    const refsBefore = f.refs();
    await assert.rejects(f.capture(), REFUSED);
    assert.ok(f.launched("add") && f.launched("write-tree"), "staged and tree written");
    assert.deepEqual(f.mutations(), ["write-tree"], "commit-tree and update-ref never launched");
    assertUnmoved(f, refsBefore);
    assert.equal((await fs.stat(path.join(f.root, "enc.txt"))).size, 5 * 1024 ** 2, "worktree bytes preserved");
  });

  // ---------------------------------------------------- acceptance 11: accounting

  test("accounting: exact launches, sourceEntries, sourceBytes and adminEntries for a one-edit capture", async t => {
    const f = await fixture(t);
    const m = meter(t);
    await f.dirty();
    const result = await f.capture();
    assert.equal(result.kind, "committed");
    const depth = f.root.split("/").filter(Boolean).length; // cloneDepth as the capture counts it
    assert.equal(f.configRows.length, 7, "seven configChecks listed the configuration");
    const configRows = f.configRows.reduce((sum, n) => sum + n, 0); // rows the seven listings actually printed
    // Fixture: T = {tracked, .gitignore} (2 paths); one unstaged edit of `tracked`; nothing staged or untracked.
    //
    // launches: every Git or helper child charges 1 (metered at launch).
    //   configCheck (metadata helper + `config`) x7: P0, status-dispatch, add-dispatch, 3 commit dispatches, P5 = 14
    //   P0 plain: --shared-index-path, symbolic-ref, for-each-ref, rev-parse HEAD, cat-file                  =  5
    //   P1: pin read, ls-files --stage, ls-files -v, repin                                                  =  4
    //   status dispatch: repin                                                                              =  1
    //   P2/P3: status, repin, diff-index, repin, census, repin, payload, repin                              =  8
    //   after status: repin                                                                                 =  1
    //   add: repin, race census, repin, add, post-add pin read, repin                                       =  6
    //   write-tree dispatch: repin x4 (prepare, pre-cb, pre-launch, post-diff-tree), race census,
    //     write-tree, post-write-tree pin read, diff-tree                                                   =  8
    //   commit-tree dispatch: repin x3 + commit-tree + the pre-CAS cat-file of W                           =  5
    //   update-ref dispatch: repin x3 + update-ref                                                          =  4
    //   P5: repin x2 (around its configCheck), cat-file, symbolic-ref, rev-parse, status, diff-tree,
    //     race census, repin                                                                                 =  9
    //                                                                                              total   = 65
    assert.equal(m.totals.launches, 65);
    assert.equal(f.launches(), 65, "every charged launch reached the owner exactly once");
    // sourceEntries: one unit per Git output row or helper step.
    //   P0 for-each-ref branch row 1; ls-files --stage 2; ls-files -v 2; status rows 1 (`1 .M ... tracked`);
    //   census stage 1: 1 per lstat x 2 paths = 2; payload stage 2: 5 per file (open, fstat, read, post-read fstat, post-read lstat) x 2 = 10;
    //   race census over the single manifest path: before add 1, before write-tree 1, P5 1 = 3;
    //   diff-tree rows after write-tree: raw row + path = 2; P5 diff-tree 2; P5 status rows 0.
    assert.equal(m.totals.sourceEntries, 1 + 2 + 2 + 1 + 2 + 10 + 3 + 2 + 2);
    // sourceBytes: the payload precharge is the sum of the current sizes: "changed" (7) + ".gitignore" "ignored\n" (8).
    assert.equal(m.totals.sourceBytes, 7 + 8);
    // adminEntries: each configCheck charges 20 x (depth + 3) helper visits plus one per printed config row
    //   (the fixture's own keys plus the code-exec pins every runner-uid git launch carries: read off the real
    //   `config --null --list` output of each of the seven checks, not recomputed);
    //   each census/payload stage charges (distinct directories incl. root = 1) + depth + 1: stage 1, payload,
    //   2 pre-launch race rechecks and the P5 recheck = 5 stages.
    assert.equal(m.totals.adminEntries, 7 * 20 * (depth + 3) + configRows + 5 * (1 + depth + 1));
    assert.equal(m.totals.rpcAttempts ?? 0, 0, "the capture itself makes no RPC");
    assert.equal(m.totals.metadataBytes !== undefined && m.totals.metadataBytes > 0, true);
  });

  // -------------------------------------------- acceptance 10: representative measurement

  const baseMissing = (() => {
    try { execFileSync("git", ["cat-file", "-e", BASE + "^{commit}"], { cwd: REPO_ROOT, env: gitEnv, stdio: "pipe" }); return false; }
    catch { return true; }
  })();
  test("representative measurement: a realistic WIP on a clone of this repository at the base SHA, real slot, no allowances",
    { skip: baseMissing && "base commit " + BASE + " is not present locally (shallow checkout)" }, async t => {
      const m = await measureRepresentative(t);
      assert.ok(m.ok, m.ok ? "" : String(m.error));
      for (const [name, value] of Object.entries(m.measure)) console.log(`MEASURE ${name}=${value}`);
      // Independent verification: W's tree extracted elsewhere equals the original worktree byte for byte.
      const out = path.join(m.dir, "extracted");
      await fs.mkdir(out);
      const archive = execFileSync("/usr/bin/git", ["-C", m.clone, "archive", m.head], { env: gitEnv, maxBuffer: 1024 ** 3 });
      execFileSync("tar", ["-x", "-C", out], { input: archive });
      const ignored = new Set(execFileSync("/usr/bin/git", ["-C", m.clone, "ls-files", "-z", "--others", "--ignored", "--exclude-standard"],
        { env: gitEnv, encoding: "utf8" }).split("\0").filter(Boolean));
      const walk = async (base: string, skipGit: boolean): Promise<Map<string, string>> => {
        const result = new Map<string, string>();
        const visit = async (rel: string): Promise<void> => {
          for (const entry of await fs.readdir(path.join(base, rel), { withFileTypes: true })) {
            const next = rel ? rel + "/" + entry.name : entry.name;
            if (skipGit && next === ".git") continue;
            if (entry.isDirectory()) await visit(next);
            else if (!ignored.has(next)) {
              const full = path.join(base, next), st = await fs.lstat(full);
              result.set(next, entry.isSymbolicLink() ? "l:" + await fs.readlink(full)
                : (st.mode & 0o111 ? "x:" : "f:") + createHash("sha256").update(await fs.readFile(full)).digest("hex"));
            }
          }
        };
        await visit("");
        return result;
      };
      const original = await walk(m.clone, true), extracted = await walk(out, false);
      assert.equal(extracted.size, original.size, "same file count");
      for (const [file, digest] of original) {
        if (extracted.get(file) !== digest) assert.fail(`first mismatch: ${file}: ${extracted.get(file)} != ${digest}`);
      }
      assert.ok(m.measure.sourceEntries > 7000, "a repository-scale capture, not a toy");
    });
});

// Clone this repository at the base SHA and capture a realistic WIP through the real
// captureMaintenanceWip under a real admitted slot (no injected allowances, no-op callbacks).
async function measureRepresentative(t: test.TestContext) {
  const dir = await tmpDir(t);
  const clone = path.join(dir, "clone");
  const sh = (cwd: string, ...args: string[]) => execFileSync("/usr/bin/git", args, { cwd, env: gitEnv, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], maxBuffer: 1024 ** 3 });
  sh(dir, "clone", "--no-local", "--no-hardlinks", "-q", "--no-checkout", REPO_ROOT, clone);
  sh(clone, "checkout", "-q", "-b", "wip-measure", BASE);
  for (const [key, value] of [["user.name", "fixture"], ["user.email", "fixture@example.com"], ["maintenance.auto", "false"], ["gc.auto", "0"]] as const)
    sh(clone, "config", key, value);
  const at = (rel: string) => path.join(clone, rel);
  // Realistic WIP: tracked edits, deletions, untracked files, one staged-plus-unstaged file.
  for (const rel of ["agent/src/git.ts", "README.md", "Taskfile.yml", "agent/package.json"]) await fs.appendFile(at(rel), "\n// wip edit\n");
  await fs.rm(at("docs/issue-3031-delivery.md")); await fs.rm(at("agent/test/helpers.ts"));
  await fs.mkdir(at("wip-new/sub"), { recursive: true });
  await fs.writeFile(at("wip-new/notes.txt"), "scratch notes\n");
  await fs.writeFile(at("wip-new/sub/data.json"), JSON.stringify({ wip: true }));
  await fs.writeFile(at("wip-new/blob.bin"), Buffer.alloc(100 * 1024, 7));
  await fs.appendFile(at("agent/tsconfig.json"), "\n"); sh(clone, "add", "agent/tsconfig.json"); await fs.appendFile(at("agent/tsconfig.json"), " ");
  const old = sh(clone, "rev-parse", "HEAD").trim();
  assert.equal(old, BASE);
  const h = await harness(t, dir, clone, old, (...args) => sh(clone, ...args));
  const m = meter(t);
  const noop = { beforeStep: async () => undefined, afterRunnerGit: async () => undefined };
  const started = performance.now();
  try {
    const result = await h.capture(noop);
    const elapsed = Math.round(performance.now() - started);
    assert.equal(result.kind, "committed");
    const measure = { sourceEntries: m.totals.sourceEntries ?? 0, sourceBytes: m.totals.sourceBytes ?? 0,
      metadataBytes: m.totals.metadataBytes ?? 0, adminEntries: m.totals.adminEntries ?? 0,
      launches: m.totals.launches ?? 0, rpcAttempts: m.totals.rpcAttempts ?? 0, elapsed };
    return { ok: true as const, dir, clone, head: result.head, measure };
  } catch (error) {
    return { ok: false as const, dir, clone, error, charged: { ...m.totals }, elapsed: Math.round(performance.now() - started) };
  }
}
