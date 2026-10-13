import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { makeFixture, makeScratchRoot, type Fixture } from "./fixture-repo.js";
import { GitCache } from "../src/git.js";
import { createLogger } from "../src/log.js";

const SELF = fileURLToPath(import.meta.url);
const GUARD = path.resolve(path.dirname(SELF), "../../scripts/tmpdir-leak-guard.sh");
const AGENT_DIR = path.resolve(path.dirname(SELF), "..");

// The guard's end-to-end child (same-file pattern as checkpoint-publish-early-response.test.ts):
// create a fixture, clean it up, then recreate a directory under it, as the leak in #2020 did.
function child(): void {
  const fx = makeFixture({}, { testName: "recreator" });
  fx.cleanup();
  fs.mkdirSync(path.join(path.dirname(fx.dataDir), "data", "x"), { recursive: true });
}

function readLedger(file: string): Array<Record<string, unknown>> {
  return fs
    .readFileSync(file, "utf8")
    .split("\n")
    .filter((l) => l !== "")
    .map((l) => JSON.parse(l) as Record<string, unknown>);
}

const POISON_SCENARIOS = ["repository", "index", "objects", "runtime-config", "config-files", "combined"] as const;
type PoisonScenario = (typeof POISON_SCENARIOS)[number];

// This environment is independent of both makeFixture and inherited Git variables.
// Real Git creation and inspection must not accidentally follow the poison being tested.
function cleanGitEnv(): NodeJS.ProcessEnv {
  return {
    PATH: process.env.PATH,
    TMPDIR: process.env.TMPDIR,
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_TERMINAL_PROMPT: "0",
  };
}

function repoBytes(dir: string): Record<string, string> {
  const result: Record<string, string> = {};
  function visit(current: string): void {
    for (const entry of fs.readdirSync(current, { withFileTypes: true })) {
      const target = path.join(current, entry.name);
      if (entry.isDirectory()) visit(target);
      else result[path.relative(dir, target)] = fs.readFileSync(target).toString("hex");
    }
  }
  visit(dir);
  return result;
}

function poisonedChild(scenario: PoisonScenario): void {
  const outer = process.env.TMPDIR!;
  const poison = path.join(outer, "poison");
  const configFile = path.join(outer, "hostile.config");
  const env = cleanGitEnv();
  const git = (repo: string, args: string[]): string =>
    execFileSync("git", ["-C", repo, ...args], { env, encoding: "utf8", stdio: "pipe" }).trim();
  fs.mkdirSync(poison);
  let fx: Fixture | undefined;
  try {
    git(poison, ["init", "-b", "poison"]);
    for (const [key, value] of [
      ["user.name", "poison"],
      ["user.email", "poison@example.invalid"],
      ["commit.gpgsign", "false"],
      ["maintenance.auto", "false"],
      ["gc.auto", "0"],
      ["core.fsmonitor", "false"],
    ]) git(poison, ["config", key!, value!]);
    fs.writeFileSync(path.join(poison, "sentinel.txt"), "leave poison alone\n");
    git(poison, ["add", "."]);
    git(poison, ["commit", "-m", "poison"]);
    fs.writeFileSync(configFile, "[core]\n\tbare = true\n[fixture]\n\thostile = true\n");
    fs.writeFileSync(path.join(poison, ".git", "hooks", "pre-commit"), "#!/bin/sh\nexit 1\n", { mode: 0o755 });
    const beforeRepo = repoBytes(poison);
    const poisonHead = git(poison, ["rev-parse", "HEAD"]);
    const groups: Record<Exclude<PoisonScenario, "combined">, NodeJS.ProcessEnv> = {
      repository: { GIT_DIR: path.join(poison, ".git"), GIT_WORK_TREE: poison },
      index: { GIT_INDEX_FILE: path.join(poison, ".git", "index") },
      objects: { GIT_OBJECT_DIRECTORY: path.join(poison, ".git", "objects") },
      "runtime-config": {
        GIT_CONFIG_COUNT: "2",
        GIT_CONFIG_KEY_0: "core.hooksPath",
        GIT_CONFIG_VALUE_0: path.join(poison, ".git", "hooks"),
        GIT_CONFIG_KEY_1: "fixture.hostile",
        GIT_CONFIG_VALUE_1: "true",
      },
      "config-files": { GIT_CONFIG_GLOBAL: configFile, GIT_CONFIG_SYSTEM: configFile, GIT_CONFIG: configFile },
    };
    Object.assign(process.env, scenario === "combined" ? Object.assign({}, ...Object.values(groups)) : groups[scenario]);
    const beforeEnv = { ...process.env };
    try {
      fx = makeFixture({ "owned.txt": "fixture owns this\n" }, { testName: scenario });
    } finally {
      // Check the actual child environment even if construction throws.
      assert.deepEqual({ ...process.env }, beforeEnv);
    }
    assert.equal(path.dirname(path.dirname(fx.originPath)), outer, "TMPDIR determines fixture location");
    assert.equal(process.env.PATH, env.PATH, "Git remains discoverable through inherited PATH");
    // Git reports the physical path (macOS TMPDIR /var is a symlink to /private/var).
    assert.equal(git(fx.originPath, ["rev-parse", "--show-toplevel"]), fs.realpathSync(fx.originPath));
    assert.equal(git(fx.originPath, ["symbolic-ref", "--short", "HEAD"]), "main");
    assert.equal(git(fx.originPath, ["rev-list", "--count", "HEAD"]), "1");
    assert.equal(git(fx.originPath, ["log", "-1", "--format=%s"]), "init");
    assert.equal(git(fx.originPath, ["show", "HEAD:README.md"]), "# fixture");
    assert.equal(git(fx.originPath, ["show", "HEAD:owned.txt"]), "fixture owns this");
    assert.equal(git(fx.originPath, ["ls-files"]), "README.md\nowned.txt");
    assert.ok(fs.statSync(path.join(fx.originPath, ".git", "index")).isFile());
    assert.equal(git(fx.originPath, ["diff", "--cached", "--name-only", "HEAD"]), "");
    for (const [key, value] of [
      ["user.name", "fixture"],
      ["user.email", "fixture@uzi.local"],
      ["commit.gpgsign", "false"],
      ["maintenance.auto", "false"],
      ["gc.auto", "0"],
      ["core.fsmonitor", "false"],
    ]) assert.equal(git(fx.originPath, ["config", "--local", "--get", key!]), value);
    assert.doesNotMatch(git(fx.originPath, ["config", "--list"]), /fixture\.hostile/);
    assert.equal(git(poison, ["rev-parse", "HEAD"]), poisonHead);
    assert.deepEqual(repoBytes(poison), beforeRepo, "poison worktree, config, index and objects are unchanged");
    assert.equal(fs.readFileSync(configFile, "utf8"), "[core]\n\tbare = true\n[fixture]\n\thostile = true\n");
  } finally {
    try {
      fx?.cleanup();
    } finally {
      fs.rmSync(poison, { recursive: true, force: true });
      fs.rmSync(configFile, { force: true });
    }
  }
  assert.deepEqual(fs.readdirSync(outer), [], "child teardown leaves its private TMPDIR empty");
}

async function permissionsChild(): Promise<void> {
  const outer = process.env.TMPDIR!;
  const parent = fs.statSync(outer);
  assert.equal(parent.gid, Number(process.env.UZI_FIXTURE_PARENT_GID));
  assert.equal(parent.mode & 0o7777, Number(process.env.UZI_FIXTURE_PARENT_MODE));
  process.umask(Number(process.env.UZI_FIXTURE_UMASK));
  let fx: Fixture | undefined;
  try {
    fx = makeFixture();
    const base = path.dirname(fx.dataDir);
    const cache = new GitCache(fx.dataDir, createLogger("error"));
    let bare: string | undefined;
    let cloneError: unknown;
    try { bare = await cache.ensureClone(fx.originPath); }
    catch (error) { cloneError = error; }
    const metadata: Array<{ path: string; mode: number; uid: number; gid: number }> = [];
    // Read the whole tree before any fixture assertion, including a failed clone's partial state.
    function collect(target: string): void {
      const stat = fs.lstatSync(target);
      metadata.push({ path: path.relative(base, target), mode: stat.mode & 0o7777, uid: stat.uid, gid: stat.gid });
      if (stat.isDirectory()) {
        for (const entry of fs.readdirSync(target)) collect(path.join(target, entry));
      }
    }
    collect(base);
    console.log(JSON.stringify({ parent: { gid: parent.gid, mode: parent.mode & 0o7777 }, metadata }));
    assert.equal(metadata[0]!.mode, 0o700, "fixture root clears all special bits");
    for (const entry of metadata) {
      assert.equal(entry.uid, process.getuid!(), entry.path + " uid");
      assert.equal(entry.gid, process.getgid!(), entry.path + " gid");
      assert.equal(entry.mode & 0o002, 0, entry.path + " world write");
    }
    assert.ifError(cloneError);
    assert.ok(bare);
    for (const target of [fx.dataDir, path.join(fx.dataDir, "repos"), bare]) {
      assert.ok(metadata.some(entry => entry.path === path.relative(base, target)), target);
    }
    const after = fs.statSync(outer);
    assert.deepEqual([after.uid, after.gid, after.mode], [parent.uid, parent.gid, parent.mode], "parent ownership and mode remain unchanged");
  } finally { fx?.cleanup(); }
}

function normalizationFailureChild(): void {
  const operation = process.env.UZI_FIXTURE_NORMALIZATION_FAILURE as "chownSync" | "chmodSync";
  const originals = { chownSync: fs.chownSync, chmodSync: fs.chmodSync };
  const failure = new Error("injected fixture normalization failure");
  let target: fs.PathLike | undefined;
  let caught: unknown;
  try {
    // Both filesystem operations receive the root as their first argument.
    const fail = (root: fs.PathLike): never => { target = root; throw failure; };
    if (operation === "chownSync") fs.chownSync = fail;
    else fs.chmodSync = fail;
    try { makeFixture(); } catch (error) { caught = error; }
  } finally {
    if (operation === "chownSync") fs.chownSync = originals.chownSync;
    else fs.chmodSync = originals.chmodSync;
  }
  assert.equal(fs[operation], originals[operation]);
  assert.equal(caught, failure, "preserves the exact normalization error");
  assert.ok(target);
  assert.equal(fs.existsSync(target), false);
  const lines = readLedger(process.env.UZI_TMPDIR_GUARD_LEDGER!);
  assert.deepEqual(lines.map(line => [line.event, line.path]), [["created", target], ["removed", target]]);
  assert.deepEqual(fs.readdirSync(process.env.TMPDIR!), ["ledger"]);
}

type Meta = { path: string; mode: number; uid: number; gid: number };

function collectMetadata(base: string): Meta[] {
  const metadata: Meta[] = [];
  function collect(target: string): void {
    const stat = fs.lstatSync(target);
    metadata.push({ path: path.relative(base, target), mode: stat.mode & 0o7777, uid: stat.uid, gid: stat.gid });
    if (stat.isDirectory()) for (const entry of fs.readdirSync(target)) collect(path.join(target, entry));
  }
  collect(base);
  return metadata;
}

// Child of the makeScratchRoot permission matrix. TMPDIR is the parent under test.
//   portable: a plain parent at the default umask; no POSIX identity needed.
//   scratch:  makeScratchRoot, then the production GitCache guards over a real clone.
//   raw:      a RAW mkdtemp root in the same parent (acceptance control / refusal calibration).
async function scratchPermissionsChild(): Promise<void> {
  const mode = process.env.UZI_SCRATCH_PERMISSIONS_CHILD as "portable" | "scratch" | "raw";
  const kind = process.env.UZI_SCRATCH_PARENT_KIND;
  const outer = process.env.TMPDIR!;
  if (process.env.UZI_SCRATCH_UMASK !== undefined) process.umask(Number(process.env.UZI_SCRATCH_UMASK));
  if (mode === "portable") {
    const root = makeScratchRoot("scratch-portable-");
    try {
      assert.equal(path.dirname(fs.realpathSync(root)), fs.realpathSync(outer), "the root is a new directory inside the parent");
      const nested = path.join(root, "data", "repos", "x");
      fs.mkdirSync(nested, { recursive: true });
      fs.writeFileSync(path.join(nested, "f.txt"), "hello\n");
      assert.equal(fs.readFileSync(path.join(nested, "f.txt"), "utf8"), "hello\n");
    } finally { fs.rmSync(root, { recursive: true, force: true }); }
    assert.deepEqual(fs.readdirSync(outer), [], "removing the root leaves the parent as before");
    return;
  }
  const origin = makeFixture();
  let root: string | undefined;
  try {
    root = mode === "scratch" ? makeScratchRoot("scratch-matrix-") : fs.mkdtempSync(path.join(outer, "raw-matrix-"));
    const rootStat = fs.lstatSync(root);
    if (mode === "scratch") {
      assert.equal(rootStat.uid, process.getuid!(), "root uid");
      assert.equal(rootStat.gid, process.getgid!(), "root gid");
      assert.equal(rootStat.mode & 0o7777, 0o700, "root mode");
    }
    const dataDir = path.join(root, "data");
    fs.mkdirSync(dataDir);
    const cache = new GitCache(dataDir, createLogger("error"));
    const bare = await cache.ensureClone(origin.originPath);
    const meta = collectMetadata(root);
    console.log(JSON.stringify({ mode, kind, meta }));
    const repos = meta.find(m => m.path === path.join("data", "repos"))!;
    const discover = (): Promise<unknown> =>
      cache.discoverRetainedRecovery(origin.originPath, "agent/issue-1", "issue-1", "run-1");
    if (mode === "raw" && kind === "foreign") {
      // Calibration: a raw root under a foreign-group setgid parent at umask 002 hands `repos`
      // a foreign group with group-write, which the production guard refuses.
      assert.notEqual(repos.gid, process.getgid!(), "repos inherited the foreign gid");
      assert.ok((repos.mode & 0o2000) !== 0, "repos is setgid");
      assert.ok((repos.mode & 0o020) !== 0, "repos is group-writable");
      await assert.rejects(cache.codeSnapshotCandidate(bare, "lead"), /unsafe worker bare directory/);
      await assert.rejects(discover(), /unsafe (worker bare directory|recovery bare config)/);
    } else {
      if (mode === "scratch") {
        for (const entry of meta) {
          assert.equal(entry.uid, process.getuid!(), entry.path + " uid");
          assert.equal(entry.gid, process.getgid!(), entry.path + " gid");
          assert.equal(entry.mode & 0o002, 0, entry.path + " world write");
        }
      }
      assert.equal(await cache.codeSnapshotCandidate(bare, "lead"), undefined);
      if (mode === "scratch") assert.equal(await discover(), undefined, "recovery discovery raises no guard error");
    }
  } finally {
    try { if (root) fs.rmSync(root, { recursive: true, force: true }); } finally { origin.cleanup(); }
  }
  assert.deepEqual(fs.readdirSync(outer), [], "child teardown leaves the parent empty");
}

function scratchNormalizationFailureChild(): void {
  const operation = process.env.UZI_SCRATCH_NORMALIZATION_FAILURE as "chownSync" | "chmodSync";
  // With UZI_SCRATCH_CLEANUP_FAILURE the root's removal also fails: the original error must survive.
  const cleanupFails = process.env.UZI_SCRATCH_CLEANUP_FAILURE === "1";
  const originals = { chownSync: fs.chownSync, chmodSync: fs.chmodSync, rmSync: fs.rmSync };
  const failure = new Error("injected scratch normalization failure");
  const cleanupFailure = new Error("injected scratch cleanup failure");
  let target: fs.PathLike | undefined;
  let caught: unknown;
  try {
    const fail = (root: fs.PathLike): never => { target = root; throw failure; };
    if (operation === "chownSync") fs.chownSync = fail;
    else fs.chmodSync = fail;
    if (cleanupFails) {
      fs.rmSync = ((p: fs.PathLike, opts?: fs.RmOptions) => {
        if (target !== undefined && String(p) === String(target)) throw cleanupFailure;
        return originals.rmSync(p, opts);
      }) as typeof fs.rmSync;
    }
    try { makeScratchRoot("scratch-fail-"); } catch (error) { caught = error; }
  } finally {
    if (operation === "chownSync") fs.chownSync = originals.chownSync;
    else fs.chmodSync = originals.chmodSync;
    fs.rmSync = originals.rmSync;
  }
  assert.equal(fs[operation], originals[operation]);
  assert.equal(fs.rmSync, originals.rmSync);
  assert.equal(caught, failure, "preserves the exact normalization error");
  assert.ok(target);
  const lines = readLedger(process.env.UZI_TMPDIR_GUARD_LEDGER!);
  if (cleanupFails) {
    // The failed removal is not recorded, and the retained root is cleaned up here.
    assert.equal(fs.existsSync(target), true);
    assert.deepEqual(lines.map(line => [line.event, line.path]), [["created", target]]);
    fs.rmSync(target, { recursive: true, force: true });
  } else {
    assert.equal(fs.existsSync(target), false);
    assert.deepEqual(lines.map(line => [line.event, line.path]), [["created", target], ["removed", target]]);
  }
  assert.deepEqual(fs.readdirSync(process.env.TMPDIR!), ["ledger"]);
}

if (process.env.UZI_SCRATCH_PERMISSIONS_CHILD) {
  await scratchPermissionsChild();
} else if (process.env.UZI_SCRATCH_NORMALIZATION_FAILURE) {
  scratchNormalizationFailureChild();
} else if (process.env.UZI_FIXTURE_PERMISSIONS_CHILD) {
  await permissionsChild();
} else if (process.env.UZI_FIXTURE_NORMALIZATION_FAILURE) {
  normalizationFailureChild();
} else if (process.env.UZI_FIXTURE_REPO_CHILD === "1") {
  child();
} else if (process.env.UZI_FIXTURE_REPO_POISON_CHILD) {
  poisonedChild(process.env.UZI_FIXTURE_REPO_POISON_CHILD as PoisonScenario);
} else {
  describe("makeFixture root permissions", () => {
    const supported = process.platform !== "win32" && !!process.getgid && !!process.getuid && !!process.getgroups;
    const foreignGid = supported ? process.getgroups!().find(gid => gid !== process.getgid!()) : undefined;
    const parents = [
      { name: "plain", gid: supported ? process.getgid!() : 0, mode: 0o770 },
      { name: "own-group setgid", gid: supported ? process.getgid!() : 0, mode: 0o2770 },
      { name: "foreign-group setgid", gid: foreignGid, mode: 0o2770 },
    ];
    for (const parent of parents) {
      for (const mask of [0o002, 0o022]) {
        it(`normalizes ${parent.name} parent with umask ${mask.toString(8)}`, {
          skip: !supported ? "POSIX identity unavailable" : parent.gid === undefined ? "foreign supplementary group unavailable" : false,
        }, () => {
          const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-permissions-"));
          try {
            // After selecting a group, a setup or verification failure is a real failure.
            fs.chownSync(outer, process.getuid!(), parent.gid!);
            fs.chmodSync(outer, parent.mode);
            const stat = fs.statSync(outer);
            assert.equal(stat.gid, parent.gid);
            assert.equal(stat.mode & 0o7777, parent.mode);
            const childEnv: NodeJS.ProcessEnv = { ...process.env, TMPDIR: outer, TSX_DISABLE_CACHE: "1",
              UZI_FIXTURE_PERMISSIONS_CHILD: "1", UZI_FIXTURE_PARENT_GID: String(parent.gid),
              UZI_FIXTURE_PARENT_MODE: String(parent.mode), UZI_FIXTURE_UMASK: String(mask) };
            for (const key of Object.keys(childEnv)) {
              if (key.startsWith("GIT_") || key === "UZI_TMPDIR_GUARD_LEDGER" ||
                  key === "UZI_FIXTURE_REPO_CHILD" || key === "UZI_FIXTURE_REPO_POISON_CHILD" ||
                  key === "UZI_FIXTURE_NORMALIZATION_FAILURE" || key.startsWith("UZI_SCRATCH_")) delete childEnv[key];
            }
            const r = spawnSync(process.execPath, ["--import", "tsx", SELF], {
              cwd: AGENT_DIR, env: childEnv, encoding: "utf8", timeout: 60_000,
            });
            assert.ifError(r.error);
            assert.equal(r.status, 0, r.stdout + r.stderr);
            assert.deepEqual(fs.readdirSync(outer), []);
          } finally { fs.rmSync(outer, { recursive: true, force: true }); }
        });
      }
    }
    for (const operation of ["chownSync", "chmodSync"]) {
      it(`cleans up and records a failed root ${operation}`, { skip: !supported }, () => {
        const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-normalization-"));
        try {
          const childEnv: NodeJS.ProcessEnv = { ...process.env, TMPDIR: outer, TSX_DISABLE_CACHE: "1",
            UZI_FIXTURE_NORMALIZATION_FAILURE: operation, UZI_TMPDIR_GUARD_LEDGER: path.join(outer, "ledger") };
          for (const key of Object.keys(childEnv)) {
            if (key.startsWith("GIT_") || key === "UZI_FIXTURE_PERMISSIONS_CHILD" ||
                key === "UZI_FIXTURE_REPO_CHILD" || key === "UZI_FIXTURE_REPO_POISON_CHILD" ||
                key.startsWith("UZI_SCRATCH_")) delete childEnv[key];
          }
          const r = spawnSync(process.execPath, ["--import", "tsx", SELF], {
            cwd: AGENT_DIR, env: childEnv, encoding: "utf8", timeout: 60_000,
          });
          assert.ifError(r.error);
          assert.equal(r.status, 0, r.stdout + r.stderr);
        } finally { fs.rmSync(outer, { recursive: true, force: true }); }
      });
    }
  });

  describe("makeScratchRoot root permissions", () => {
    const supported = process.platform !== "win32" && !!process.getgid && !!process.getuid && !!process.getgroups;
    const foreignGid = supported ? process.getgroups!().find(gid => gid !== process.getgid!()) : undefined;

    // Runs one child with TMPDIR=<parent>. Child flags are stripped from the inherited environment.
    function runChild(parent: string, extra: Record<string, string>, ledger?: string): void {
      const childEnv: NodeJS.ProcessEnv = { ...process.env, TMPDIR: parent, TEMP: parent, TMP: parent,
        TSX_DISABLE_CACHE: "1", ...extra };
      for (const key of Object.keys(childEnv)) {
        if (key.startsWith("GIT_") || key === "UZI_TMPDIR_GUARD_LEDGER" || key === "UZI_FIXTURE_PERMISSIONS_CHILD" ||
            key === "UZI_FIXTURE_REPO_CHILD" || key === "UZI_FIXTURE_REPO_POISON_CHILD" ||
            key === "UZI_FIXTURE_NORMALIZATION_FAILURE" ||
            (key.startsWith("UZI_SCRATCH_") && !(key in extra))) delete childEnv[key];
      }
      if (ledger) childEnv.UZI_TMPDIR_GUARD_LEDGER = ledger;
      const r = spawnSync(process.execPath, ["--import", "tsx", SELF], {
        cwd: AGENT_DIR, env: childEnv, encoding: "utf8", timeout: 90_000,
      });
      assert.ifError(r.error);
      assert.equal(r.status, 0, r.stdout + r.stderr);
    }

    it("returns a private root inside a plain parent that nested directories can use and that removes cleanly", () => {
      const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-scratch-plain-"));
      try {
        runChild(outer, { UZI_SCRATCH_PERMISSIONS_CHILD: "portable", UZI_SCRATCH_PARENT_KIND: "plain" });
        assert.deepEqual(fs.readdirSync(outer), []);
      } finally { fs.rmSync(outer, { recursive: true, force: true }); }
    });

    it("gives an owned 0700 root whose bare clones pass the production guards (plain parent, default umask)",
      { skip: !supported ? "POSIX identity unavailable" : false }, () => {
        const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-scratch-posix-"));
        try {
          runChild(outer, { UZI_SCRATCH_PERMISSIONS_CHILD: "scratch", UZI_SCRATCH_PARENT_KIND: "plain" });
          assert.deepEqual(fs.readdirSync(outer), []);
        } finally { fs.rmSync(outer, { recursive: true, force: true }); }
      });

    const parents = [
      { kind: "plain", name: "plain", gid: supported ? process.getgid!() : 0, mode: 0o770, raw: false },
      { kind: "own", name: "own-group setgid", gid: supported ? process.getgid!() : 0, mode: 0o2770, raw: true },
      { kind: "foreign", name: "foreign-group setgid", gid: foreignGid, mode: 0o2770, raw: true },
    ];
    // Sets up an owned outer parent with the given group and mode, runs `children`, then checks
    // the parent is unchanged and empty.
    function withParent(parent: (typeof parents)[number], mask: number, children: string[]): void {
      const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-scratch-"));
      try {
        fs.chownSync(outer, process.getuid!(), parent.gid!);
        fs.chmodSync(outer, parent.mode);
        const before = fs.statSync(outer);
        assert.equal(before.gid, parent.gid);
        assert.equal(before.mode & 0o7777, parent.mode);
        const env = { UZI_SCRATCH_PARENT_KIND: parent.kind, UZI_SCRATCH_UMASK: String(mask) };
        for (const child of children) runChild(outer, { ...env, UZI_SCRATCH_PERMISSIONS_CHILD: child });
        const after = fs.statSync(outer);
        assert.deepEqual([after.uid, after.gid, after.mode], [before.uid, before.gid, before.mode],
          "parent ownership and mode remain unchanged");
        assert.deepEqual(fs.readdirSync(outer), []);
      } finally { fs.rmSync(outer, { recursive: true, force: true }); }
    }
    const identitySkip = (parent: (typeof parents)[number]) =>
      !supported ? "POSIX identity unavailable" : parent.gid === undefined ? "foreign supplementary group unavailable" : false;
    for (const parent of parents) {
      for (const mask of [0o002, 0o022]) {
        // The own-group raw root is accepted on every POSIX platform; the foreign-group raw refusal
        // is a Linux setgid-inheritance calibration and runs in its own Linux-only test below.
        const rawCase = parent.raw && parent.kind !== "foreign";
        it(`${parent.name} parent with umask ${mask.toString(8)}: the scratch root is accepted${rawCase ? "; a raw root is accepted" : ""}`, {
          skip: identitySkip(parent),
        }, () => withParent(parent, mask, rawCase ? ["scratch", "raw"] : ["scratch"]));
      }
    }
    {
      const foreign = parents.find(p => p.kind === "foreign")!;
      it("foreign-group setgid parent with umask 2: a raw root inherits the foreign group and is refused (Linux setgid calibration)", {
        skip: process.platform !== "linux" ? "Linux setgid directory inheritance required" : identitySkip(foreign),
      }, () => withParent(foreign, 0o002, ["raw"]));
    }

    for (const operation of ["chownSync", "chmodSync"]) {
      it(`cleans up and records a failed scratch root ${operation}`, { skip: !supported ? "POSIX identity unavailable" : false }, () => {
        const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-scratch-failure-"));
        try {
          runChild(outer, { UZI_SCRATCH_NORMALIZATION_FAILURE: operation }, path.join(outer, "ledger"));
        } finally { fs.rmSync(outer, { recursive: true, force: true }); }
      });
    }

    for (const operation of ["chownSync", "chmodSync"]) {
      it(`keeps the ${operation} error when removing the failed scratch root also fails`, { skip: !supported ? "POSIX identity unavailable" : false }, () => {
        const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-scratch-cleanup-failure-"));
        try {
          runChild(outer, { UZI_SCRATCH_NORMALIZATION_FAILURE: operation, UZI_SCRATCH_CLEANUP_FAILURE: "1" }, path.join(outer, "ledger"));
        } finally { fs.rmSync(outer, { recursive: true, force: true }); }
      });
    }
  });

  describe("makeFixture inherited Git environment isolation", () => {
    for (const scenario of POISON_SCENARIOS) {
      it(`isolates ${scenario} without mutating the child environment or poison repo`, () => {
        // The parent owns the outer directory even when the baseline constructor
        // throws before returning a Fixture with a cleanup method.
        const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-poison-"));
        try {
          const childEnv: NodeJS.ProcessEnv = {
            ...process.env,
            TMPDIR: outer,
            TSX_DISABLE_CACHE: "1", // the loader must not leave its own cache in the private TMPDIR
            UZI_FIXTURE_REPO_POISON_CHILD: scenario,
          };
          for (const key of Object.keys(childEnv)) {
            if (key.startsWith("GIT_") || key.startsWith("UZI_SCRATCH_")) delete childEnv[key];
          }
          delete childEnv.UZI_FIXTURE_REPO_CHILD;
          delete childEnv.UZI_TMPDIR_GUARD_LEDGER;
          const r = spawnSync(process.execPath, ["--import", "tsx", SELF], {
            cwd: AGENT_DIR,
            env: childEnv,
            encoding: "utf8",
            timeout: 60_000,
          });
          assert.ifError(r.error);
          assert.equal(r.status, 0, r.stdout + r.stderr);
          assert.deepEqual(fs.readdirSync(outer), []);
        } finally {
          fs.rmSync(outer, { recursive: true, force: true });
        }
      });
    }
  });

  describe("makeFixture ledger (scripts/tmpdir-leak-guard.sh evidence)", () => {
    it("records a created line, then a removed line on cleanup", () => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-ledger-"));
      const ledger = path.join(dir, "ledger");
      const saved = process.env.UZI_TMPDIR_GUARD_LEDGER;
      process.env.UZI_TMPDIR_GUARD_LEDGER = ledger;
      let fx: Fixture | undefined;
      try {
        fx = makeFixture({}, { testName: "x" });
        const base = path.dirname(fx.dataDir);
        let lines = readLedger(ledger);
        assert.equal(lines.length, 1);
        const created = lines[0] as Record<string, unknown>;
        assert.equal(created.event, "created");
        assert.equal(created.entry, path.basename(base));
        assert.equal(created.path, base);
        assert.equal(created.test, "x");
        assert.equal(created.pid, process.pid);
        assert.match(String(created.file), /fixture-repo\.test\.ts$/);
        assert.ok(Array.isArray(created.site));
        assert.ok((created.site as string[]).some((s) => s.includes("fixture-repo.test.ts:")));

        fx.cleanup();
        assert.equal(fs.existsSync(base), false);
        lines = readLedger(ledger);
        assert.equal(lines.length, 2);
        assert.equal((lines[1] as Record<string, unknown>).event, "removed");
        assert.equal((lines[1] as Record<string, unknown>).entry, path.basename(base));
      } finally {
        // A failed assertion above must not leak the fixture into the real guard's TMPDIR,
        // where its created line (in this private ledger) would be gone.
        if (saved === undefined) delete process.env.UZI_TMPDIR_GUARD_LEDGER;
        else process.env.UZI_TMPDIR_GUARD_LEDGER = saved;
        try {
          if (fx) fs.rmSync(path.dirname(fx.dataDir), { recursive: true, force: true });
        } finally {
          fs.rmSync(dir, { recursive: true, force: true });
        }
      }
    });

    it("names the creator of a directory recreated after cleanup, under the real guard", () => {
      const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-guard-"));
      try {
        const childEnv: NodeJS.ProcessEnv = { ...process.env, TMPDIR: outer, UZI_FIXTURE_REPO_CHILD: "1" };
        delete childEnv.UZI_TMPDIR_GUARD_LEDGER; // the guard sets its own
        for (const key of Object.keys(childEnv)) if (key.startsWith("UZI_SCRATCH_")) delete childEnv[key];
        const r = spawnSync(GUARD, [process.execPath, "--import", "tsx", SELF], {
          cwd: AGENT_DIR,
          env: childEnv,
          encoding: "utf8",
          timeout: 60_000,
        });
        assert.equal(r.status, 1, r.stderr);
        assert.ok(r.stderr.includes("fixture-repo.test.ts"), r.stderr);
        assert.ok(r.stderr.includes("recreator"), r.stderr);
        assert.ok(r.stderr.includes("cleanup ran; the directory was recreated afterwards"), r.stderr);
        assert.ok(r.stderr.includes("data/x"), r.stderr);
      } finally {
        fs.rmSync(outer, { recursive: true, force: true });
      }
    });
  });
}
