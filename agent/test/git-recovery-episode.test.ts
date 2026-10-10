import { beforeEach, afterEach, test, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { Readable, PassThrough, Writable } from "node:stream";
import { createHash } from "node:crypto";
import { deflateSync } from "node:zlib";
import { RecoveryClosureLimitError } from "../src/recovery-closure.js";
import { GitCache, RetainedRecoveryBlockedError } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";

const branch = "agent/issue-2512";
const key = "issue-2512";
const runId = "run-2512";
const aid = (n: number) => `20261008T171401Z-g${n}-0123456789abcdef`;
let fx: Fixture;
let cache: GitCache;
let bare: string;
let source: { runId: string; clonePath: string };
function git(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    stdio: ["pipe", "pipe", "pipe"],
  }).trim();
}
function journal(): any {
  return JSON.parse(git(bare, ["config", `uzi-recovery.${branch}.clone`]));
}
function writeJournal(value: unknown): void {
  git(bare, ["config", `uzi-recovery.${branch}.clone`, JSON.stringify(value)]);
}
function recreate(): GitCache { return new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions()); }
beforeEach(async () => {
  fx = makeFixture();
  cache = recreate();
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId);
  source = { runId, clonePath: clone.path };
  await cache.markRecoveryCapture(bare, clone.path, branch, runId);
});
afterEach(() => fx.cleanup());


for (const fault of ["write", "clear", "readback", "ledger"] as const) {
  test(`terminal descriptor interruption at ${fault} preserves attribution and retries after recreation`, async () => {
    const { tip } = await ready();
    const before = journal();
    const snapshot = await cache.terminalRetainedSnapshot(bare, branch, key, before);
    assert.ok(snapshot);
    const seam = cache as unknown as { runGit: (cwd: string | undefined, args: string[], ...rest: unknown[]) => Promise<string> };
    const real = seam.runGit.bind(cache);
    let written = false;
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "config" && args.includes(`uzi-retained.${runId}.journal`)) {
        if (fault === "write") throw new Error("injected descriptor write failure");
        const out = await real(cwd, args, ...rest);
        written = true;
        return out;
      }
      if (fault === "readback" && written && args.includes("--list")) throw new Error("injected readback failure");
      if (fault === "clear" && args.includes(`uzi-recovery.${branch}.clone`) && args.at(-1) === "") throw new Error("injected clear failure");
      if (fault === "ledger" && args.includes("--add") && args.some(a => a.includes('"reclaimed"'))) throw new Error("injected ledger failure");
      return real(cwd, args, ...rest);
    };
    await assert.rejects(cache.detachTerminalRetained(bare, branch, key, snapshot.journal), /injected/);
    assert.deepEqual(journal(), before);
    assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
    const restarted = recreate();
    await restarted.detachTerminalRetained(bare, branch, key, snapshot.journal);
    await recreate().detachTerminalRetained(bare, branch, key, snapshot.journal);
    assert.equal(git(bare, ["config", `uzi-recovery.${branch}.clone`]), "");
    assert.deepEqual(JSON.parse(git(bare, ["config", `uzi-retained.${runId}.journal`])).journal, before);
    assert.equal(await restarted.discoverRetainedRecovery(fx.originPath, branch, key, runId), undefined);
    assert.equal(await restarted.recoveryAttemptMode(fx.originPath, key), true);
    assert.equal((await restarted.readInventoryCloneHeads(bare, runId)).kind, "verified");
    for (const p of snapshot.paths) assert.ok(fs.existsSync(p));
  });
}

test("terminal descriptor full snapshot refuses changed counters and changed siblings", async () => {
  await ready();
  const snapshot = (await cache.terminalRetainedSnapshot(bare, branch, key, journal()))!;
  for (const change of ["counter", "siblings"]) {
    const changed = structuredClone(snapshot.journal);
    if (change === "counter") changed.recovery!.attempts = 2;
    else changed.retainedSources = [];
    writeJournal(changed);
    await assert.rejects(cache.detachTerminalRetained(bare, branch, key, snapshot.journal), /snapshot changed/);
    assert.deepEqual(journal(), changed);
  }
});

test("protected descriptor conflicts and malformed evidence refuse detachment and inventory", async () => {
  await ready();
  const snapshot = (await cache.terminalRetainedSnapshot(bare, branch, key, journal()))!;
  const envelope = { version: 1, branch, key, journal: snapshot.journal };
  for (const value of ["{", JSON.stringify({ ...envelope, version: 2 }),
    JSON.stringify({ ...envelope, journal: { ...snapshot.journal, recovery: { ...snapshot.journal.recovery, attempts: 2 } } }),
    JSON.stringify({ ...envelope, key: "issue-other" })]) {
    git(bare, ["config", `uzi-retained.${runId}.journal`, value]);
    await assert.rejects(cache.detachTerminalRetained(bare, branch, key, snapshot.journal));
    assert.deepEqual(journal(), snapshot.journal);
    assert.equal((await cache.readInventoryCloneHeads(bare, runId)).kind, "unknown");
  }
  git(bare, ["config", `uzi-retained.${runId}.journal`, JSON.stringify(envelope)]);
  git(bare, ["config", "--add", `uzi-retained.${runId}.journal`, JSON.stringify(envelope)]);
  await assert.rejects(cache.detachTerminalRetained(bare, branch, key, snapshot.journal), /invalid protected/);
  assert.equal((await cache.readInventoryCloneHeads(bare, runId)).kind, "unknown");
});

test("unrelated protected record missing .git does not block canonical seed", async () => {
  const { tip } = await ready();
  const before = journal();
  await cache.detachTerminalRetained(bare, branch, key, before);
  assert.equal(git(bare, ["config", `uzi-recovery.${branch}.clone`]), "");
  const descriptor = git(bare, ["config", `uzi-retained.${runId}.journal`]);
  const pinRef = `refs/uzi-recovery-episode/${runId}/${tip}`;
  assert.equal(git(bare, ["rev-parse", pinRef]), tip);
  const workPath = path.join(source.clonePath, "work.txt");
  const workBytes = fs.readFileSync(workPath);

  fs.rmSync(path.join(source.clonePath, ".git"), { recursive: true });
  assert.equal(fs.existsSync(path.join(source.clonePath, ".git")), false);
  assert.ok(fs.statSync(source.clonePath).isDirectory());
  assert.deepEqual(fs.readFileSync(workPath), workBytes);

  const nextIid = 2513;
  const nextKey = "issue-2513";
  const nextRun = "run-2513";
  const next = await recreate().createOrAttachRunnerClone(bare, nextIid, noProofReseed, nextRun);
  assert.equal(next.path, cache.runnerClonePath(bare, nextKey));
  assert.equal(git(next.path, ["branch", "--show-current"]), "agent/issue-2513");
  assert.equal(git(next.path, ["rev-parse", "HEAD"]), git(bare, ["rev-parse", "refs/remotes/origin/main"]));
  assert.equal(fs.readFileSync(path.join(next.path, "README.md"), "utf8"), "# fixture\n");
  assert.equal(git(bare, ["config", `uzi-retained.${runId}.journal`]), descriptor);
  assert.equal(git(bare, ["rev-parse", pinRef]), tip);
  assert.ok(fs.statSync(source.clonePath).isDirectory());
  assert.deepEqual(fs.readFileSync(workPath), workBytes);
});

function protectedValue(value = journal()): string {
  return JSON.stringify({ version: 1, branch, key, journal: value });
}

function attemptSeed(n: number) {
  return { attemptId: aid(n), isLive: () => false, beforeSeed: async () => {}, quiescent: async () => true };
}

for (const overlap of [false, true]) {
  test("custody isolation: foreign damaged evidence permits own inventory and seed with overlap " + overlap, async () => {
    await ready();
    const before = journal();
    if (overlap) git(bare, ["config", `uzi-retained.${runId}.journal`, protectedValue(before)]);
    else await cache.detachTerminalRetained(bare, branch, key, before);
    const descriptor = git(bare, ["config", `uzi-retained.${runId}.journal`]);
    fs.rmSync(path.join(source.clonePath, ".git"), { recursive: true });
    const warnings: unknown[][] = [];
    const logger = nullLogger();
    logger.warn = (...args) => { warnings.push(args); };
    const reader = new GitCache(fx.dataDir, logger, undefined, testGitCacheOptions());
    const next = await reader.createOrAttachRunnerClone(bare, 2513, noProofReseed, "run-2513");
    await reader.markRecoveryCapture(bare, next.path, "agent/issue-2513", "run-2513");
    assert.equal(warnings.length, 1, "one summary per canonical discovery pass");
    assert.deepEqual(warnings[0], ["recovery custody discovery: incomplete physical evidence; attribution remains protected"]);
    warnings.length = 0;
    const own = await reader.readInventoryCloneHeads(bare, "run-2513");
    assert.equal(own.kind, "verified");
    if (own.kind === "verified") {
      assert.deepEqual(own.foreignOwners, [runId]);
      assert.ok(own.clones.some(c => c.clonePath === next.path));
    }
    assert.equal(warnings.length, 1);
    assert.deepEqual(await reader.readInventoryCloneHeads(bare, runId), { kind: "unknown", cause: "clone_head_unreadable" });
    assert.equal(warnings.length, 2, "overlapping active/protected evidence still logs only once per pass");
    const serialized = JSON.stringify(warnings);
    for (const privateValue of [bare, branch, key, runId, source.clonePath, JSON.stringify(before)]) {
      assert.equal(serialized.includes(privateValue), false);
    }
    if (overlap) {
      await assert.rejects(reader.discoverRetainedRecovery(fx.originPath, branch, key, runId));
      await assert.rejects(reader.prepareRecoverySuccessor(bare, branch, key, before, 1, aid(4)));
      assert.deepEqual(journal(), before);
    }
    assert.equal(git(bare, ["config", `uzi-retained.${runId}.journal`]), descriptor);
  });
}

for (const damage of ["root", "parent", "leaf", ".git", "distinct successor"] as const) {
  test("custody isolation: own retained inventory is unknown for absent " + damage, async () => {
    const { expected } = await ready();
    const before = journal();
    git(bare, ["config", `uzi-retained.${runId}.journal`, protectedValue(before)]);
    const target = damage === "root" ? path.dirname(path.dirname(source.clonePath))
      : damage === "parent" ? path.dirname(source.clonePath)
      : damage === "leaf" ? source.clonePath
      : damage === ".git" ? path.join(source.clonePath, ".git") : expected.clonePath;
    fs.rmSync(target, { recursive: true });
    const result = await cache.readInventoryCloneHeads(bare, runId);
    assert.deepEqual(result, { kind: "unknown", cause: damage === "root" || damage === "parent"
      ? "clone_ancestor_invalid" : damage === ".git" ? "clone_head_unreadable" : "clone_path_invalid" });
    assert.deepEqual(journal(), before);
    assert.equal(git(bare, ["config", `uzi-retained.${runId}.journal`]), protectedValue(before));
  });
}

test("custody isolation: attributed adopting successor absent before creation makes own inventory unknown", async () => {
  const { successor, before } = await legacyAdopting();
  git(bare, ["config", "--add", `uzi-attempts.${branch}.entry`, JSON.stringify({
    attemptId: successor.attemptId, runId, clonePath: successor.clonePath, state: "live",
  })]);
  assert.equal(fs.existsSync(successor.clonePath), false);
  assert.deepEqual(await cache.readInventoryCloneHeads(bare, runId), { kind: "unknown", cause: "clone_path_invalid" });
  assert.deepEqual(journal(), before);
  await cache.createOrAttachRunnerClone(bare, 2513, noProofReseed, "run-2513");
  assert.equal(fs.existsSync(successor.clonePath), false, "indexing never creates a successor");
});

test("custody isolation: every primary retained predecessor and successor survives sweep and compaction", async () => {
  const { expected, tip } = await ready();
  await cache.reserveRecoveryIteration(bare, branch, key, expected);
  await cache.recordRecoveryCapture(bare, branch, key, expected, 2, tip);
  await cache.prepareRecoverySuccessor(bare, branch, key, expected, 2, aid(3));
  const before = journal();
  await cache.detachTerminalRetained(bare, branch, key, before);
  const retained = [source.clonePath, expected.clonePath, before.clonePath];
  fs.rmSync(path.join(source.clonePath, ".git"), { recursive: true });
  fs.rmSync(expected.clonePath, { recursive: true });
  const descriptor = git(bare, ["config", `uzi-retained.${runId}.journal`]);
  // Duplicate ledger values force actual compaction; the absent retained attempt must stay attributed.
  const entry = JSON.stringify({ attemptId: aid(2), runId, clonePath: expected.clonePath, state: "reclaimed" });
  git(bare, ["config", "--add", `uzi-attempts.${branch}.entry`, entry]);
  const residues = Array.from({ length: 6 }, (_, i) => path.join(path.dirname(source.clonePath),
    `.uzi-residue-${key}.residue-00000000-0000-4000-8000-${String(i).padStart(12, "0")}`));
  for (const [i, residue] of residues.entries()) {
    fs.mkdirSync(residue);
    fs.utimesSync(residue, i + 1, i + 1);
  }
  const warnings: unknown[][] = [];
  const logger = nullLogger();
  logger.warn = (...args) => { warnings.push(args); };
  const reader = new GitCache(fx.dataDir, logger, undefined, testGitCacheOptions());
  const next = await reader.createOrAttachRunnerClone(bare, 2512, noProofReseed, "run-next", false, undefined, attemptSeed(4));
  assert.ok(fs.existsSync(next.path), "seed and retention sweep complete despite damaged custody");
  assert.equal(fs.existsSync(residues[0]!), false, "sweep deletes the oldest unrelated disposable residue");
  assert.ok(residues.slice(1).every(residue => fs.existsSync(residue)));
  assert.equal(warnings.length, 2, "compaction and sweep each emit one fixed summary");
  assert.ok(warnings.every(args => args.length === 1 && args[0] ===
    "recovery custody discovery: incomplete physical evidence; attribution remains protected"));
  const ledger = git(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`]).split("\n").map(raw => JSON.parse(raw));
  assert.equal(ledger.filter(e => e.attemptId === aid(2)).length, 1, "compaction ran");
  for (const [id, clonePath] of [[aid(2), expected.clonePath], [aid(3), before.clonePath]]) {
    assert.equal(await reader.classifyOwnerClonePath(bare, branch, key, runId, clonePath), "attempt");
    assert.ok(ledger.some(e => e.attemptId === id && e.clonePath === clonePath && e.runId === runId));
  }
  for (const clone of [retained[0]!, retained[2]!]) assert.equal(fs.readFileSync(path.join(clone, "work.txt"), "utf8"), "retained work\n");
  assert.equal(fs.existsSync(expected.clonePath), false);
  assert.equal(git(bare, ["config", `uzi-retained.${runId}.journal`]), descriptor);
  assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
  await assert.rejects(reader.discardRetainedRecovery(bare, branch, key, before, true));
});

test("custody isolation: selected discard ignores foreign descriptor filesystems and preserves newer active", async () => {
  const { tip } = await ready();
  const before = journal();
  await cache.detachTerminalRetained(bare, branch, key, before);
  const next = await cache.createOrAttachRunnerClone(bare, 2513, noProofReseed, "run-2513");
  await cache.markRecoveryCapture(bare, next.path, "agent/issue-2513", "run-2513");
  const foreign = { runId: "run-2513", clonePath: next.path, retainedSources: [{ runId: "run-2513", clonePath: next.path }] };
  git(bare, ["config", "uzi-retained.run-2513.journal", JSON.stringify({ version: 1, branch: "agent/issue-2513", key: "issue-2513", journal: foreign })]);
  git(bare, ["config", "uzi-recovery.agent/issue-2513.clone", JSON.stringify(foreign)]);
  fs.rmSync(path.join(next.path, ".git"), { recursive: true });
  const foreignDescriptor = git(bare, ["config", "uzi-retained.run-2513.journal"]);
  const newer = await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, "run-next", false, undefined, attemptSeed(4));
  await cache.markRecoveryCapture(bare, newer.path, branch, "run-next", newer.attemptId);
  const active = journal();
  git(bare, ["config", "uzi-retained.run-malformed.journal", "{"]);
  await cache.discardRetainedRecovery(bare, branch, key, before, true);
  assert.equal(git(bare, ["config", "uzi-retained.run-malformed.journal"]), "{", "foreign descriptors are not parsed during selected discard");
  assert.deepEqual(journal(), active);
  assert.equal(git(bare, ["config", "uzi-retained.run-2513.journal"]), foreignDescriptor);
  assert.throws(() => git(bare, ["rev-parse", "--verify", `refs/uzi-recovery-episode/${runId}/${tip}`]));
  assert.ok(fs.existsSync(source.clonePath));
});

for (const invalid of ["malformed", "duplicate", "branch", "key", "source", "active conflict", "unsafe selected", "run"] as const) {
  test("custody isolation: selected discard fails closed for " + invalid, async () => {
    const { tip } = await ready();
    const before = journal();
    const envelope = { version: 1, branch, key, journal: before };
    git(bare, ["config", `uzi-retained.${runId}.journal`, JSON.stringify(envelope)]);
    if (invalid === "malformed") git(bare, ["config", `uzi-retained.${runId}.journal`, "{"]);
    if (invalid === "duplicate") git(bare, ["config", "--add", `uzi-retained.${runId}.journal`, JSON.stringify(envelope)]);
    if (invalid === "branch" || invalid === "key") {
      git(bare, ["config", `uzi-retained.${runId}.journal`, JSON.stringify({ ...envelope, [invalid]: "issue-other" })]);
    }
    if (invalid === "source") git(bare, ["config", `uzi-retained.${runId}.journal`, protectedValue({ ...before, clonePath: source.clonePath + "-other" })]);
    if (invalid === "active conflict") writeJournal({ ...before, recovery: { ...before.recovery, attempts: 2 } });
    if (invalid === "unsafe selected") {
      fs.rmSync(path.join(source.clonePath, ".git"), { recursive: true });
      fs.symlinkSync(bare, path.join(source.clonePath, ".git"));
    }
    const active = journal();
    const descriptor = git(bare, ["config", "--get-all", `uzi-retained.${runId}.journal`]);
    const expected = invalid === "run" ? { ...before, runId: "../invalid" } : before;
    await assert.rejects(cache.discardRetainedRecovery(bare, branch, key, expected, true));
    assert.deepEqual(journal(), active);
    assert.equal(git(bare, ["config", "--get-all", `uzi-retained.${runId}.journal`]), descriptor);
    assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
  });
}

test("custody isolation: unattributed legacy successor fails indexing without repair", async () => {
  const { before } = await legacyAdopting();
  const config = fs.readFileSync(path.join(bare, "config"));
  await assert.rejects(cache.createOrAttachRunnerClone(bare, 2513, noProofReseed, "run-2513"), /does not belong/);
  assert.equal((await cache.readInventoryCloneHeads(bare, runId)).kind, "unknown");
  assert.deepEqual(journal(), before);
  assert.deepEqual(fs.readFileSync(path.join(bare, "config")), config);
});

for (const invalid of ["malformed", "duplicate", "conflicting ledger", "conflicting active", "unattributable"] as const) {
  test("custody isolation: indexing fails closed for " + invalid, async () => {
    await ready();
    const before = journal();
    git(bare, ["config", `uzi-retained.${runId}.journal`, protectedValue(before)]);
    fs.rmSync(path.join(source.clonePath, ".git"), { recursive: true });
    if (invalid === "malformed") git(bare, ["config", `uzi-retained.${runId}.journal`, "{"]);
    if (invalid === "duplicate") git(bare, ["config", "--add", `uzi-retained.${runId}.journal`, protectedValue(before)]);
    if (invalid === "conflicting ledger") git(bare, ["config", "--add", "uzi-attempts.agent/issue-other.entry", JSON.stringify({
      attemptId: before.attemptId, runId, clonePath: before.clonePath, state: "live",
    })]);
    if (invalid === "conflicting active") writeJournal({ ...before, recovery: { ...before.recovery, attempts: 2 } });
    if (invalid === "unattributable") git(bare, ["config", `uzi-retained.${runId}.journal`, JSON.stringify({
      version: 1, branch, key: "issue-other", journal: before,
    })]);
    const config = fs.readFileSync(path.join(bare, "config"));
    await assert.rejects(cache.createOrAttachRunnerClone(bare, 2513, noProofReseed, "run-2513"));
    assert.equal((await cache.readInventoryCloneHeads(bare, "run-2513")).kind, "unknown");
    assert.equal(fs.existsSync(cache.runnerClonePath(bare, "issue-2513")), false);
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), config);
  });
}

for (const stopAt of ["abort", "deadline"] as const) {
  for (const consumer of ["seed", "inventory"] as const) {
    test("custody isolation: " + stopAt + " during physical " + consumer + " discovery propagates without mutation", async () => {
      await ready();
      const before = journal();
      await cache.detachTerminalRetained(bare, branch, key, before);
      const config = fs.readFileSync(path.join(bare, "config"));
      const stop = new AbortController();
      const reason = new Error("fixture interrupted");
      const realLstat = fs.promises.lstat;
      const realNow = Date.now;
      const now = realNow();
      let probed = false;
      // A specific filesystem probe advances the operation clock or aborts it.
      // No timer race or host-speed dependency is needed.
      fs.promises.lstat = (async (target: fs.PathLike, ...rest: unknown[]) => {
        if (String(target) === path.join(source.clonePath, ".git")) {
          probed = true;
          if (stopAt === "abort") stop.abort(reason);
          else Date.now = () => now + 60_001;
          throw Object.assign(new Error("private filesystem failure"), { code: "ENOENT" });
        }
        return Reflect.apply(realLstat, fs.promises, [target, ...rest]);
      }) as typeof fs.promises.lstat;
      try {
        const operation = cache.withRecoveryOperation(stop.signal, now + 60_000, async () => {
          if (consumer === "seed") return cache.createOrAttachRunnerClone(bare, 2513, noProofReseed, "run-2513");
          return cache.readInventoryCloneHeads(bare, "run-2513");
        });
        if (stopAt === "abort") await assert.rejects(operation, error => error === reason);
        else await assert.rejects(operation, /recovery deadline exhausted/);
        assert.equal(probed, true);
      } finally {
        fs.promises.lstat = realLstat;
        Date.now = realNow;
      }
      assert.deepEqual(fs.readFileSync(path.join(bare, "config")), config);
      assert.equal(fs.existsSync(cache.runnerClonePath(bare, "issue-2513")), false);
    });
  }
}

test("discard exact detached run preserves newer active ownership and fresh attempt marker", async () => {
  const { tip } = await ready();
  const before = journal();
  await cache.detachTerminalRetained(bare, branch, key, before);
  const nextRun = "run-next";
  const next = await recreate().createOrAttachRunnerClone(bare, 2512, noProofReseed, nextRun, false, undefined, {
    attemptId: aid(3), isLive: () => false, beforeSeed: async () => {}, quiescent: async () => true,
  });
  await cache.markRecoveryCapture(bare, next.path, branch, nextRun, next.attemptId);
  const active = journal();
  assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip, "next seed keeps the old recovery pin");
  const entries = git(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`]).split("\n").map(raw => JSON.parse(raw));
  assert.equal(entries.filter(entry => entry.attemptId === aid(2)).at(-1)?.state, "reclaimed", "successor attribution survives seed compaction");
  const inventory = await recreate().readInventoryCloneHeads(bare, runId);
  assert.equal(inventory.kind, "verified");
  if (inventory.kind === "verified") {
    assert.ok(inventory.clones.some(clone => clone.clonePath === source.clonePath));
    assert.ok(inventory.clones.some(clone => clone.clonePath === before.clonePath));
  }
  await recreate().detachTerminalRetained(bare, branch, key, before);
  assert.deepEqual(journal(), active);
  await recreate().discardRetainedRecovery(bare, branch, key, before, true);
  assert.deepEqual(journal(), active);
  assert.equal(await cache.recoveryAttemptMode(fx.originPath, key), true);
  assert.throws(() => git(bare, ["config", `uzi-retained.${runId}.journal`]));
  assert.throws(() => git(bare, ["rev-parse", "--verify", `refs/uzi-recovery-episode/${runId}/${tip}`]));
  assert.ok(fs.existsSync(source.clonePath));
});

test("missing source discovery carries trusted coordinates and first blocker persists exhausted identity", async () => {
  fs.renameSync(source.clonePath, source.clonePath + ".saved");
  await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, runId), err => {
    assert.ok(err instanceof RetainedRecoveryBlockedError);
    assert.equal(err.barePath, bare);
    assert.equal(err.branch, branch);
    assert.equal(err.key, key);
    assert.equal(err.journal.clonePath, source.clonePath);
    return true;
  });
  await cache.blockRecoveryEpisode(bare, branch, key, source, "source_missing");
  assert.equal(journal().recovery.attempts, 3);
  assert.equal(journal().recovery.stage, "blocked");
  await cache.blockRecoveryEpisode(bare, branch, key, source, "capture_failed");
  assert.equal(journal().recovery.blocker, "source_missing");
  fs.renameSync(source.clonePath + ".saved", source.clonePath);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, source), /blocked/);
});

test("expected absent episode rejects a reservation created while finalizer waits for the lock", async () => {
  let acquired!: () => void, release!: () => void;
  const held = new Promise<void>(resolve => { acquired = resolve; });
  const holder = cache.withBareLock(bare, async () => {
    acquired();
    await new Promise<void>(resolve => { release = resolve; });
  });
  await held;
  // Queue reservation first, then the stale absence finalizer, on the same source.
  const reservation = cache.reserveRecoveryIteration(bare, branch, key, source);
  const finalizer = cache.blockRecoveryEpisode(bare, branch, key, source, "source_missing", null);
  const rejected = assert.rejects(finalizer, /episode changed/);
  release();
  await holder;
  await reservation;
  const before = journal();
  await rejected;
  assert.deepEqual(journal(), before);
  assert.equal(journal().recovery.stage, "capturing");
});

test("backward and invalid clocks fail closed at reservation, capture, adoption and reset", async () => {
  const realNow = Date.now;
  const first = await cache.reserveRecoveryIteration(bare, branch, key, source, 30_000);
  const tip = await capture();
  const original = journal();
  try {
    for (const now of [first.startedAt - 1, NaN, Infinity]) {
      for (const operation of ["reserve", "capture", "adopt", "complete"]) {
        Date.now = realNow;
        writeJournal(original);
        let expected: { runId: string; clonePath: string; attemptId?: string } = source;
        if (operation === "adopt" || operation === "complete") {
          await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
        }
        if (operation === "complete") {
          const clone = await cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(8));
          expected = { runId, clonePath: clone.path, attemptId: aid(8) };
        }
        Date.now = () => now;
        const call = operation === "reserve" ? cache.reserveRecoveryIteration(bare, branch, key, source)
          : operation === "capture" ? cache.recordRecoveryCapture(bare, branch, key, source, 1, tip)
          : operation === "adopt" ? cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(9))
          : cache.completeRecoveryEpisode(bare, branch, key, expected, { settled: true, processedEvents: 1, attemptId: aid(8) });
        await assert.rejects(call, /clock|current/);
        assert.equal(journal().recovery.stage, "blocked");
        assert.equal(journal().recovery.attempts, 1);
        assert.equal(journal().recovery.startedAt, first.startedAt);
        if (operation === "complete") {
          Date.now = realNow;
          fs.rmSync(expected.clonePath, { recursive: true, force: true });
          // Clear the fixture ledger to reuse this test-only successor identity.
          git(bare, ["config", "--unset-all", `uzi-attempts.${branch}.entry`]);
        }
      }
    }
  } finally { Date.now = realNow; }
});

test("backoff uses persisted attempt exponent and permits a 30-second base", async () => {
  assert.equal((await cache.reserveRecoveryIteration(bare, branch, key, source, 30_000)).backoffMs, 30_000);
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source, 30_000)).backoffMs, 60_000);
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source, 30_000)).backoffMs, 120_000);
});

test("untrusted bare config and caller coordinates cannot publish progress", async () => {
  const before = journal();
  await assert.rejects(cache.reserveRecoveryIteration(fx.originPath, branch, key, source));
  const config = path.join(bare, "config");
  fs.renameSync(config, config + ".saved");
  fs.symlinkSync(config + ".saved", config);
  await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, runId), /unsafe/);
  await assert.rejects(cache.blockRecoveryEpisode(bare, branch, key, source, "source_missing"), /unsafe/);
  fs.unlinkSync(config);
  fs.renameSync(config + ".saved", config);
  git(bare, ["config", "include.path", config + ".saved"]);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, source), /unsafe/);
  git(bare, ["config", "--unset", "include.path"]);
  assert.deepEqual(journal(), before);
});
async function capture(wip = false): Promise<string> {
  fs.writeFileSync(path.join(source.clonePath, "work.txt"), "retained work\n");
  git(source.clonePath, ["add", "work.txt"]);
  if (wip) await cache.commitWipMarker(source.clonePath);
  else git(source.clonePath, ["commit", "-m", "milestone"]);
  await fixtureFetchTracking(cache, bare, source.clonePath, branch, runId);
  return git(source.clonePath, ["rev-parse", "HEAD"]);
}
async function ready(wip = false) {
  const reservation = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture(wip);
  await cache.recordRecoveryCapture(bare, branch, key, source, reservation.attempts, tip);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, reservation.attempts, aid(2));
  return { successor, tip, expected: { runId, clonePath: successor.path, attemptId: aid(2) } };
}

test("legacy discovery is credential-free and precharged attempts survive worker recreation and reclaim", async () => {
  assert.equal(journal().recovery, undefined);
  assert.equal((await cache.discoverRetainedRecovery(fx.originPath, branch, key, runId))?.journal.clonePath, source.clonePath);
  const first = await cache.reserveRecoveryIteration(bare, branch, key, source, 20_000);
  await recreate().releaseAttemptInPlace(bare, source.clonePath, branch, runId, "reclaimed");
  await recreate().markRecoveryCapture(bare, source.clonePath, branch, runId);
  const second = await recreate().reserveRecoveryIteration(bare, branch, key, source, 20_000);
  assert.equal(second.attempts, 2);
  assert.equal(second.startedAt, first.startedAt);
  assert.equal(second.deadline, first.deadline);
  assert.equal(second.backoffMs, 40_000);
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source)).attempts, 3);
  await assert.rejects(recreate().reserveRecoveryIteration(bare, branch, key, source), /budget exhausted/);
  assert.equal(journal().recovery.stage, "blocked");
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, source), /blocked/);
});

test("missing-source identity mismatches and missing known own source never overwrite attribution or seed refusal", async () => {
  const before = git(bare, ["config", `uzi-recovery.${branch}.clone`]);
  await assert.rejects(cache.markRecoveryCapture(bare, source.clonePath, branch, "foreign"), /overwritten/);
  await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, "foreign"), /foreign/);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, "other-key", source), /key/);
  assert.equal(git(bare, ["config", `uzi-recovery.${branch}.clone`]), before);
  const { tip } = await ready();
  const value = journal();
  git(bare, ["config", `uzi-retained.${runId}.journal`, protectedValue(value)]);
  git(bare, ["update-ref", `refs/uzi-owed/${runId}/${tip}`, tip]);
  // Keep the real successor sibling and pins while removing the canonical source.
  const active = value.clonePath;
  const siblingHead = fs.readFileSync(path.join(active, ".git", "HEAD"));
  const siblingWork = fs.readFileSync(path.join(active, "work.txt"));
  const sourceHead = fs.readFileSync(path.join(source.clonePath, ".git", "HEAD"));
  const siblingBytes = fs.readFileSync(path.join(source.clonePath, "work.txt"));
  const snapshot = () => [
    git(bare, ["config", "--get-regexp", "^uzi-"]),
    git(bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi"]),
  ];
  const calls: string[] = [];
  // The canonical caller computes a different path for this successor. Use the own
  // canonical source coordinates for the seed refusal in both modes.
  writeJournal({ ...source, retainedSources: [{ runId, clonePath: active, attemptId: aid(2) }] });
  fs.renameSync(source.clonePath, source.clonePath + ".saved");
  const ownBefore = git(bare, ["config", `uzi-recovery.${branch}.clone`]);
  const ownEvidence = snapshot();
  const ownPaths = fs.readdirSync(path.dirname(active)).sort();
  for (const attempt of [false, true]) {
    await assert.rejects(cache.createOrAttachRunnerClone(bare, 2512, {
      beforeFree: async () => { calls.push("free"); },
    }, runId, false, undefined, attempt ? {
      attemptId: aid(3), isLive: () => { calls.push("live"); return false; },
      beforeSeed: async () => { calls.push("seed"); },
      quiescent: async () => { calls.push("sweep"); return true; },
    } : undefined), { message: "known recovery source is missing; nothing seeded" });
    assert.deepEqual(calls, []);
    assert.deepEqual(snapshot(), ownEvidence);
    assert.deepEqual(fs.readdirSync(path.dirname(active)).sort(), ownPaths);
    assert.equal(fs.existsSync(source.clonePath), false);
    assert.equal(fs.existsSync(source.clonePath + ".attempt-" + aid(3)), false);
    assert.deepEqual(fs.readFileSync(path.join(source.clonePath + ".saved", "work.txt")), siblingBytes);
    assert.deepEqual(fs.readFileSync(path.join(active, ".git", "HEAD")), siblingHead);
    assert.deepEqual(fs.readFileSync(path.join(active, "work.txt")), siblingWork);
    assert.deepEqual(fs.readFileSync(path.join(source.clonePath + ".saved", ".git", "HEAD")), sourceHead);
    assert.equal(git(bare, ["config", `uzi-recovery.${branch}.clone`]), ownBefore);
  }
});

test("invalid progress, unknown versions, attribution and unsafe paths fail closed", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const original = journal();
  for (const mutation of [
    { version: 2 }, { attempts: 4 }, { attempts: 1.5 }, { deadline: 0 }, { backoffMs: 480_001 },
    { stage: "ready-for-model" }, { stage: "blocked", blocker: "unsafe\noutput" },
    { source: { ...source, runId: "foreign" } }, { surprise: true },
  ]) {
    writeJournal({ ...original, recovery: { ...original.recovery, ...mutation } });
    await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, runId), /invalid|identity/);
  }
  writeJournal({ ...source, recovery: original.recovery, retainedSources: [{ ...source, clonePath: "/unsafe/../path" }] });
  await assert.rejects(cache.discoverRetainedRecovery(fx.originPath, branch, key, runId), /invalid/);
});

test("successor handoff preserves predecessor bytes, canonical inventory, pins and episode identity", async () => {
  const reservation = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  const headBytes = fs.readFileSync(path.join(source.clonePath, ".git", "HEAD"));
  const content = fs.readFileSync(path.join(source.clonePath, "work.txt"));
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, source, 2, aid(2)), /current/);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(2), { selfContained: true });
  assert.equal(successor.attemptId, aid(2));
  assert.equal(successor.attemptId, journal().attemptId);
  assert.notEqual(successor.path, source.clonePath);
  assert.equal(git(successor.path, ["rev-parse", "HEAD"]), tip);
  assert.deepEqual(fs.readFileSync(path.join(source.clonePath, ".git", "HEAD")), headBytes);
  assert.deepEqual(fs.readFileSync(path.join(source.clonePath, "work.txt")), content);
  assert.equal(journal().recovery.deadline, reservation.deadline);
  assert.equal(journal().recovery.source.clonePath, source.clonePath);
  assert.equal(journal().retainedSources[0].attemptId, undefined);
  assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
  const inventory = await cache.readInventoryCloneHeads(bare, runId);
  assert.equal(inventory.kind, "verified");
  if (inventory.kind === "verified") {
    assert.ok(inventory.clones.some(c => c.clonePath === source.clonePath));
    assert.ok(inventory.clones.some(c => c.clonePath === successor.path));
  }
  await cache.releaseAttemptInPlace(bare, successor.path, branch, runId, "abandoned");
  assert.equal(journal().recovery.attempts, 1);
  await assert.rejects(cache.retireRunnerClone(bare, source.clonePath, branch, runId, { discard: true }), /different clone|disposition/);
  const expected = { runId, clonePath: successor.path, attemptId: aid(2) };
  const second = await recreate().reserveRecoveryIteration(bare, branch, key, expected);
  assert.equal(second.attempts, 2);
  assert.equal(second.deadline, reservation.deadline);
  await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
    { settled: true, processedEvents: 1, attemptId: aid(2) }), /cannot complete/);
});

test("successor ledger precedes adopting journal and crash never seeds a clone", async (t) => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  const before = journal();
  const clonePath = source.clonePath + ".attempt-" + aid(2);
  const internal = cache as unknown as { writeRecovery(...args: unknown[]): Promise<void> };
  let injected = false;
  t.mock.method(internal, "writeRecovery", async () => {
    injected = true;
    throw new Error("injected journal crash");
  });
  await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(2)), /injected journal crash/);
  assert.equal(injected, true);
  assert.equal(fs.existsSync(clonePath), false);
  assert.deepEqual(journal(), before);
  assert.equal(await recreate().classifyOwnerClonePath(bare, branch, key, runId, clonePath), "attempt");
  assert.deepEqual(JSON.parse(JSON.stringify((await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId))!.journal)), before);
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source)).attempts, 2);
});

async function legacyAdopting() {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  const successor = { runId, clonePath: source.clonePath + ".attempt-" + aid(2), attemptId: aid(2), restoreTip: tip };
  writeJournal({ ...journal(), recovery: { ...journal().recovery, successor, stage: "adopting" } });
  return { successor, tip, before: journal() };
}

test("absent legacy adopting successor repairs exact attribution before rediscovery", async () => {
  const { successor, tip, before } = await legacyAdopting();
  assert.equal(await recreate().classifyOwnerClonePath(bare, branch, key, runId, successor.clonePath), undefined);
  assert.deepEqual(JSON.parse(JSON.stringify((await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId))!.journal)), before);
  assert.equal(await recreate().classifyOwnerClonePath(bare, branch, key, runId, successor.clonePath), "attempt");
  assert.deepEqual(JSON.parse(git(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`])), {
    attemptId: aid(2), runId, clonePath: successor.clonePath, state: "live",
  });
  assert.equal(fs.existsSync(successor.clonePath), false);
  assert.equal(fs.readFileSync(path.join(source.clonePath, "work.txt"), "utf8"), "retained work\n");
  assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source)).attempts, 2);
});

for (const ledgerBranch of [branch, "agent/other-branch"]) {
  test(`legacy adopting successor refuses repair for valueless ledger on ${ledgerBranch}`, async () => {
    const { successor, tip, before } = await legacyAdopting();
    fs.appendFileSync(path.join(bare, "config"), `\n[uzi-attempts "${ledgerBranch}"]\n\tentry\n`);
    const ledgerKey = `uzi-attempts.${ledgerBranch}.entry`;
    const entries = git(bare, ["config", "--null", "--list"]).split("\0");
    assert.ok(entries.includes(ledgerKey), "fixture must contain a valueless key without a value separator");
    assert.ok(!entries.some(e => e.startsWith(ledgerKey + "\n")), "fixture must not be an empty valued string");
    const configBefore = fs.readFileSync(path.join(bare, "config"));
    await assert.rejects(recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId),
      /unreadable or conflicting recovery successor ledger/);
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), configBefore);
    assert.deepEqual(journal(), before);
    assert.equal(await recreate().classifyOwnerClonePath(bare, branch, key, runId, successor.clonePath), undefined);
    assert.equal(fs.existsSync(successor.clonePath), false);
    assert.equal(fs.readFileSync(path.join(source.clonePath, "work.txt"), "utf8"), "retained work\n");
    assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
  });
}

test("legacy adopting successor repairs with an unrelated valueless nonledger key", async () => {
  const { successor, before } = await legacyAdopting();
  fs.appendFileSync(path.join(bare, "config"), '\n[fixture]\n\tvalueless\n');
  assert.ok(git(bare, ["config", "--null", "--list"]).split("\0").includes("fixture.valueless"));
  assert.deepEqual(JSON.parse(JSON.stringify((await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId))!.journal)), before);
  assert.equal(await recreate().classifyOwnerClonePath(bare, branch, key, runId, successor.clonePath), "attempt");
  assert.deepEqual(JSON.parse(git(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`])), {
    attemptId: aid(2), runId, clonePath: successor.clonePath, state: "live",
  });
  assert.ok(git(bare, ["config", "--null", "--list"]).split("\0").includes("fixture.valueless"));
  assert.equal(fs.existsSync(successor.clonePath), false);
});

for (const invalid of ["existing path", "conflict", "unreadable ledger", "unsafe ancestor", "missing ancestor", "foreign owner", "unsafe sibling"] as const) {
  test(`legacy adopting successor refuses repair for ${invalid}`, async () => {
    const { successor, before } = await legacyAdopting();
    if (invalid === "existing path") fs.cpSync(source.clonePath, successor.clonePath, { recursive: true });
    if (invalid === "conflict" || invalid === "unreadable ledger") git(bare, ["config", "--add",
      `uzi-attempts.${branch}.entry`, invalid === "conflict"
        ? JSON.stringify({ ...successor, restoreTip: undefined, runId: "foreign", state: "live" }) : "{"]);
    const parent = path.dirname(source.clonePath);
    if (invalid === "unsafe ancestor" || invalid === "missing ancestor") {
      fs.renameSync(parent, parent + ".saved");
      if (invalid === "unsafe ancestor") fs.symlinkSync(parent + ".saved", parent);
    }
    if (invalid === "unsafe sibling") {
      fs.renameSync(source.clonePath, source.clonePath + ".saved");
      fs.symlinkSync(source.clonePath + ".saved", source.clonePath);
    }
    await assert.rejects(recreate().discoverRetainedRecovery(fx.originPath, branch, key,
      invalid === "foreign owner" ? "foreign" : runId));
    assert.deepEqual(journal(), before);
    const entries = git(bare, ["config", "--null", "--list"]).split("\0")
      .filter(e => e.startsWith(`uzi-attempts.${branch}.entry\n`));
    assert.equal(entries.length, invalid === "conflict" || invalid === "unreadable ledger" ? 1 : 0);
  });
}

test("multiple successor handoffs keep the original episode source, deadline and total budget", async () => {
  const { expected, tip } = await ready();
  const first = journal().recovery;
  await cache.reserveRecoveryIteration(bare, branch, key, expected);
  await cache.recordRecoveryCapture(bare, branch, key, expected, 2, tip);
  const second = await cache.prepareRecoverySuccessor(bare, branch, key, expected, 2, aid(3));
  assert.equal(journal().recovery.source.clonePath, source.clonePath);
  assert.equal(journal().recovery.startedAt, first.startedAt);
  assert.equal(journal().recovery.deadline, first.deadline);
  assert.equal(journal().recovery.attempts, 2);
  assert.ok(journal().retainedSources.some((s: any) => s.clonePath === expected.clonePath));
  const next = { runId, clonePath: second.path, attemptId: aid(3) };
  await cache.releaseAttemptInPlace(bare, second.path, branch, runId, "reclaimed");
  await cache.markRecoveryCapture(bare, second.path, branch, runId, aid(3));
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, next)).attempts, 3);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, next), /budget/);
});

test("bridge recovery preserves the exact tree and both parent histories", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  const base = git(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const tree = git(bare, ["rev-parse", `${tip}^{tree}`]);
  const floor = git(bare, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
    "commit-tree", tree, "-p", base, "-m", "parallel published history"]);
  const bridge = await cache.bridgeToFloors(bare, tip, [floor]);
  assert.equal(bridge.kind, "built");
  assert.ok(bridge.kind === "built");
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, bridge.sha);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(2));
  assert.equal(git(successor.path, ["rev-parse", "HEAD"]), bridge.sha);
  assert.equal(git(successor.path, ["write-tree"]), tree);
  git(successor.path, ["merge-base", "--is-ancestor", tip, "HEAD"]);
  git(successor.path, ["merge-base", "--is-ancestor", floor, "HEAD"]);
});

test("WIP unwrap positively preserves staged tree and committed parent history", async () => {
  const { successor, tip } = await ready(true);
  assert.equal(git(successor.path, ["rev-parse", "HEAD"]), git(bare, ["rev-parse", `${tip}^`]));
  assert.equal(git(successor.path, ["write-tree"]), git(bare, ["rev-parse", `${tip}^{tree}`]));
  assert.equal(fs.readFileSync(path.join(successor.path, "work.txt"), "utf8"), "retained work\n");
  assert.match(git(successor.path, ["diff", "--cached", "--name-only"]), /work.txt/);
});

test("only explicit completion with settled processing evidence resets budget; blocked never resets", async () => {
  const { expected, tip } = await ready();
  await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
    { settled: true, processedEvents: 0, attemptId: aid(2) }), /evidence/);
  fs.writeFileSync(path.join(bare, "config.lock"), "");
  await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
    { settled: true, processedEvents: 1, attemptId: aid(2) }));
  fs.unlinkSync(path.join(bare, "config.lock"));
  assert.equal(journal().recovery.attempts, 1);
  await cache.completeRecoveryEpisode(bare, branch, key, expected, { settled: true, processedEvents: 1, attemptId: aid(2) });
  assert.equal(journal().recovery, undefined);
  assert.ok(journal().retainedSources.some((s: any) => s.clonePath === source.clonePath));
  assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
  await cache.reserveRecoveryIteration(bare, branch, key, expected);
  await cache.blockRecoveryEpisode(bare, branch, key, expected, "capture_failed");
  await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
    { settled: true, processedEvents: 1, attemptId: aid(2) }), /cannot complete/);
  await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, expected), /blocked/);
});

test("settled model completion beyond recovery deadline durably resets without releasing retention", async () => {
  const { expected, tip } = await ready();
  const before = journal();
  const realNow = Date.now;
  try {
    Date.now = () => before.recovery.deadline + 600_000;
    await cache.completeRecoveryEpisode(bare, branch, key, expected,
      { settled: true, processedEvents: 1, attemptId: aid(2) });
  } finally { Date.now = realNow; }
  const persisted = (await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId))!.journal;
  const { recovery: _completed, ...retained } = before;
  assert.deepEqual(persisted, retained, "custody and retained source descriptors survive completion");
  assert.equal(fs.readFileSync(path.join(source.clonePath, "work.txt"), "utf8"), "retained work\n");
  assert.equal(git(bare, ["rev-parse", `refs/uzi-recovery-episode/${runId}/${tip}`]), tip);
});

test("pin write failure retains charged capturing stage; deadline is never refreshed", async () => {
  const first = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  fs.mkdirSync(path.join(bare, "refs", "uzi-recovery-episode", runId), { recursive: true });
  fs.writeFileSync(path.join(bare, "refs", "uzi-recovery-episode", runId, tip + ".lock"), "");
  await assert.rejects(cache.recordRecoveryCapture(bare, branch, key, source, 1, tip));
  assert.equal(journal().recovery.stage, "capturing");
  assert.equal(journal().recovery.attempts, 1);
  const expired = { ...journal(), recovery: { ...journal().recovery, startedAt: first.startedAt - 300_001, deadline: first.deadline - 300_001 } };
  writeJournal(expired);
  await assert.rejects(recreate().reserveRecoveryIteration(bare, branch, key, source), /budget/);
  assert.equal(journal().recovery.deadline, expired.recovery.deadline);
});

test("failed fresh successor retains adopting evidence and cannot be mistaken for execution authority", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  // A disjoint default prevents positive history verification after successor identity lands.
  const tree = git(bare, ["rev-parse", `${tip}^{tree}`]);
  const root = git(bare, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "commit-tree", tree, "-m", "unrelated"]);
  git(bare, ["update-ref", "refs/remotes/origin/main", root]);
  await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(2)), /history/);
  assert.equal(journal().clonePath, source.clonePath);
  assert.equal(journal().recovery.stage, "adopting");
  assert.equal(journal().recovery.successor.attemptId, aid(2));
  assert.equal((await recreate().reserveRecoveryIteration(bare, branch, key, source)).attempts, 2);
});

test("captured restart evidence verifies source tree and refuses changed work without refunding attempts", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  const first = journal();
  assert.equal(await recreate().verifiedRecoveryRestorePoint(bare, branch, key, source, tip), true);
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  assert.equal(journal().recovery.attempts, 2);
  assert.equal(journal().recovery.deadline, first.recovery.deadline);
  assert.equal(await cache.verifiedRecoveryRestorePoint(bare, branch, key, source, tip), true);
  fs.writeFileSync(path.join(source.clonePath, "work.txt"), "changed since capture");
  assert.equal(await cache.verifiedRecoveryRestorePoint(bare, branch, key, source, tip), false);
  assert.equal(journal().recovery.attempts, 2);
});

test("an existing orphan successor remains attributed when a retry adopts another fresh path", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  const base = git(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const tree = git(bare, ["rev-parse", `${tip}^{tree}`]);
  const root = git(bare, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "commit-tree", tree, "-m", "unrelated"]);
  git(bare, ["update-ref", "refs/remotes/origin/main", root]);
  await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, source, 1, aid(2)), /history/);
  const orphan = journal().recovery.successor;
  fs.cpSync(source.clonePath, orphan.clonePath, { recursive: true });
  git(bare, ["update-ref", "refs/remotes/origin/main", base]);
  await recreate().reserveRecoveryIteration(bare, branch, key, source);
  await cache.recordRecoveryCapture(bare, branch, key, source, 2, tip);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, 2, aid(3));
  assert.notEqual(successor.path, orphan.clonePath);
  assert.ok(journal().retainedSources.some((s: any) => s.clonePath === orphan.clonePath && s.attemptId === aid(2)));
  assert.equal(fs.readFileSync(path.join(orphan.clonePath, "work.txt"), "utf8"), "retained work\n");
  assert.equal(journal().recovery.attempts, 2);
});

test("recovery abort settles the active supervised child before releasing the bare lock", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const before = journal();
  const stop = new AbortController();
  let spawned!: () => void, finish!: (value: { code: number }) => void;
  let cancelled = false, settled = false;
  const started = new Promise<void>(resolve => { spawned = resolve; });
  const completed = new Promise<{ code: number }>(resolve => { finish = resolve; });
  const reason = new Error("lifecycle stopped");
  let producerEntered = false;
  const operation = cache.withRecoveryOperation(stop.signal, Date.now() + 240_000,
    () => cache.withBoundaryProcessSpawner(async () => {
      producerEntered = true;
      spawned();
      return {
        stdout: Readable.from([]), stderr: Readable.from([]), stdin: null,
        completed, cancel: async () => {
          cancelled = true;
          await new Promise(resolve => setTimeout(resolve, 20));
          settled = true;
          finish({ code: -1 });
        },
      };
    }, new AbortController().signal,
    () => cache.withBareLock(bare, () => cache.verifyRecoveryClosure(bare, git(source.clonePath, ["rev-parse", "HEAD"])))));
  const outcome = operation.then(value => value, error => error);
  let observer: Promise<void> | undefined;
  try {
    await requireProducerEntry(started, outcome);
    assert.equal(producerEntered, true);
    stop.abort(reason);
    observer = cache.withBareLock(bare, async () => { assert.equal(settled, true); });
    assert.equal(await outcome, reason);
    await observer;
  } finally {
    stop.abort(new Error("fixture cleanup"));
    await Promise.allSettled([operation, outcome, ...(producerEntered ? [completed] : []), ...(observer ? [observer] : [])]);
  }
  assert.equal(cancelled, true);
  assert.deepEqual(journal(), before);
});

test("already expired recovery scope admits no local work", async () => {
  let admitted = false;
  await assert.rejects(cache.withRecoveryOperation(new AbortController().signal, Date.now() - 1,
    async () => { admitted = true; }), { message: "recovery deadline exhausted" });
  assert.equal(admitted, false);
});

test("recovery deadline cancels a queued mutation without later changing its charged journal", async (t) => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const before = journal();
  let release!: () => void, acquired!: () => void;
  const held = new Promise<void>(resolve => { acquired = resolve; });
  const holder = cache.withBareLock(bare, async () => {
    acquired();
    await new Promise<void>(resolve => { release = resolve; });
  });
  await held;
  const expire = controlledDeadline(t);
  const stop = new AbortController();
  let entered = false, deadlineReason: unknown;
  const expired = cache.withRecoveryOperation(stop.signal, Date.now() + 240_000, signal => {
    signal.addEventListener("abort", () => { deadlineReason = signal.reason; }, { once: true });
    entered = true;
    const reservation = cache.reserveRecoveryIteration(bare, branch, key, source);
    queueMicrotask(expire);
    return reservation;
  });
  const outcome = expired.then(value => value, error => error);
  try {
    const result = await outcome;
    assert.equal(entered, true, "queued reservation entered before expiry");
    assert.ok(result instanceof Error);
    assert.equal(result.message, "permit-held git lock wait aborted: boundary deadline exceeded");
    assert.ok(deadlineReason instanceof Error);
    assert.equal(deadlineReason.message, "recovery deadline exhausted");
  } finally {
    stop.abort(new Error("fixture cleanup"));
    release();
    await Promise.allSettled([holder, expired, outcome]);
  }
  await cache.withBareLock(bare, async () => {});
  assert.deepEqual(journal(), before);
});

test("settled callback aborted during lock wait cannot erase its durable budget", async () => {
  const { expected } = await ready();
  const before = journal();
  const stop = new AbortController();
  let release!: () => void, acquired!: () => void;
  const held = new Promise<void>(resolve => { acquired = resolve; });
  const holder = cache.withBareLock(bare, async () => {
    acquired();
    await new Promise<void>(resolve => { release = resolve; });
  });
  await held;
  const completion = cache.completeRecoveryEpisode(bare, branch, key, expected,
    { settled: true, processedEvents: 1, attemptId: aid(2) }, () => stop.signal.throwIfAborted());
  stop.abort(new Error("lifecycle stopped"));
  release();
  await holder;
  await assert.rejects(completion, /lifecycle stopped/);
  assert.deepEqual(journal(), before);
});

test("effective attempt mode refuses unknown markers, unsafe coordinates and config read errors", async () => {
  const marker = `uzi-attemptmode.${key}.enabled`;
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, "../escape"), /key/);
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath + "-missing", key));
  for (const value of ["false", "", "unknown", "TRUE"]) {
    git(bare, ["config", marker, value]);
    await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key), /attempt mode/);
    await assert.rejects(cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId));
  }
  git(bare, ["config", marker, "true"]);
  git(bare, ["config", "--add", marker, "true"]);
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key), /attempt mode/);
  git(bare, ["config", "--unset-all", marker]);
  const config = path.join(bare, "config");
  fs.renameSync(config, config + ".saved");
  fs.symlinkSync(config + ".saved", config);
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key), /unsafe/);
  fs.unlinkSync(config);
  fs.writeFileSync(config, "[broken config");
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key));
  fs.unlinkSync(config);
  fs.renameSync(config + ".saved", config);
  git(bare, ["config", "include.path", config + ".saved"]);
  await assert.rejects(cache.recoveryAttemptMode(fx.originPath, key), /unsafe/);
  git(bare, ["config", "--unset", "include.path"]);
  assert.equal(await cache.recoveryAttemptMode(fx.originPath, key), false);
});

test("more than eight settled recovery episodes retain every source, pin and byte and adopt fresh paths", async () => {
  let expected: { runId: string; clonePath: string; attemptId?: string } = source;
  const retained: Array<{ clonePath: string; head: Buffer; bytes: Buffer; tip: string }> = [];
  // Ten episodes exceed both former descriptor limits; each performs real capture and handoff.
  for (let n = 1; n <= 10; n++) {
    const reservation = await recreate().reserveRecoveryIteration(bare, branch, key, expected);
    assert.equal(reservation.attempts, 1, "only the preceding recorded completion reset the budget");
    fs.writeFileSync(path.join(expected.clonePath, "work.txt"), "episode " + n + "\n");
    git(expected.clonePath, ["add", "work.txt"]);
    git(expected.clonePath, ["commit", "-m", "episode " + n]);
    await fixtureFetchTracking(cache, bare, expected.clonePath, branch, runId);
    const tip = git(expected.clonePath, ["rev-parse", "HEAD"]);
    retained.push({ clonePath: expected.clonePath, tip,
      head: fs.readFileSync(path.join(expected.clonePath, ".git", "HEAD")),
      bytes: fs.readFileSync(path.join(expected.clonePath, "work.txt")) });
    await cache.recordRecoveryCapture(bare, branch, key, expected, 1, tip);
    const successor = await cache.prepareRecoverySuccessor(bare, branch, key, expected, 1, aid(n + 1));
    assert.notEqual(successor.path, expected.clonePath);
    expected = { runId, clonePath: successor.path, attemptId: aid(n + 1) };
    assert.equal(journal().recovery.attempts, 1, "capture and handoff keep the charged budget");
    assert.equal(journal().recovery.deadline, reservation.deadline);
    await assert.rejects(cache.completeRecoveryEpisode(bare, branch, key, expected,
      { settled: true, processedEvents: 0, attemptId: aid(n + 1) }), /evidence/);
    assert.equal(journal().recovery.attempts, 1, "unrecorded processing cannot reset");
    await cache.completeRecoveryEpisode(bare, branch, key, expected,
      { settled: true, processedEvents: 1, attemptId: aid(n + 1) });
    assert.equal(journal().recovery, undefined);
  }
  const discovered = (await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId))!.journal;
  assert.equal(discovered.retainedSources!.length, 10);
  for (const saved of retained) {
    assert.ok(discovered.retainedSources!.some(s => s.clonePath === saved.clonePath));
    assert.deepEqual(fs.readFileSync(path.join(saved.clonePath, ".git", "HEAD")), saved.head);
    assert.deepEqual(fs.readFileSync(path.join(saved.clonePath, "work.txt")), saved.bytes);
    assert.equal(git(bare, ["rev-parse", "refs/uzi-recovery-episode/" + runId + "/" + saved.tip]), saved.tip);
  }
});

for (const stage of ["captured", "handoff", "completed"] as const) {
  test("explicit discard tracks distinct retry captures through " + stage + " and retries failed pin deletion", async () => {
    const first = await cache.reserveRecoveryIteration(bare, branch, key, source);
    const tipA = await capture();
    await cache.recordRecoveryCapture(bare, branch, key, source, 1, tipA);
    const retry = await recreate().reserveRecoveryIteration(bare, branch, key, source);
    assert.equal(retry.attempts, 2);
    assert.equal(retry.deadline, first.deadline);
    fs.writeFileSync(path.join(source.clonePath, "work.txt"), "second capture\n");
    git(source.clonePath, ["add", "work.txt"]);
    git(source.clonePath, ["commit", "-m", "distinct retry capture"]);
    await fixtureFetchTracking(cache, bare, source.clonePath, branch, runId);
    const tipB = git(source.clonePath, ["rev-parse", "HEAD"]);
    assert.notEqual(tipA, tipB);
    await cache.recordRecoveryCapture(bare, branch, key, source, 2, tipB);
    let expected: { runId: string; clonePath: string; attemptId?: string } = source;
    if (stage !== "captured") {
      const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, 2, aid(2));
      expected = { runId, clonePath: successor.path, attemptId: aid(2) };
      assert.equal(journal().recovery.attempts, 2);
      if (stage === "completed") await cache.completeRecoveryEpisode(bare, branch, key, expected,
        { settled: true, processedEvents: 1, attemptId: aid(2) });
    }
    const pins: [string, string] = [
      "refs/uzi-recovery-episode/" + runId + "/" + tipA,
      "refs/uzi-recovery-episode/" + runId + "/" + tipB,
    ];
    const sibling = "refs/uzi-recovery-episode/" + runId + "-sibling/" + tipA;
    const unrelated = "refs/keep/" + tipB;
    git(bare, ["update-ref", sibling, tipA]);
    git(bare, ["update-ref", unrelated, tipB]);
    for (const [i, pin] of pins.entries()) assert.equal(git(bare, ["rev-parse", pin]), [tipA, tipB][i]);
    const before = journal();
    const lock = path.join(bare, ...pins[1].split("/")) + ".lock";
    fs.writeFileSync(lock, "");
    await assert.rejects(recreate().discardRetainedRecovery(bare, branch, key, expected, true));
    assert.deepEqual(journal(), before, "failed cleanup retains trusted attribution");
    assert.equal(git(bare, ["rev-parse", pins[1]]), tipB);
    fs.unlinkSync(lock);
    await recreate().discardRetainedRecovery(bare, branch, key, expected, true);
    for (const pin of pins) assert.throws(() => git(bare, ["rev-parse", "--verify", pin]));
    assert.equal(git(bare, ["config", "uzi-recovery." + branch + ".clone"]), "");
    assert.equal(git(bare, ["rev-parse", sibling]), tipA);
    assert.equal(git(bare, ["rev-parse", unrelated]), tipB);
    assert.equal(fs.readFileSync(path.join(source.clonePath, "work.txt"), "utf8"), "second capture\n");
    assert.equal(fs.readFileSync(path.join(expected.clonePath, "work.txt"), "utf8"), "second capture\n");
    assert.equal(await cache.recoveryAttemptMode(fx.originPath, key), true);
  });
}

test("discard refuses malformed or changed run pin identity before deleting any pins or attribution", async () => {
  const { expected, tip } = await ready();
  const before = journal();
  const good = "refs/uzi-recovery-episode/" + runId + "/" + tip;
  const parent = git(bare, ["rev-parse", tip + "^"]);
  for (const bad of ["refs/uzi-recovery-episode/" + runId + "/unexpected",
    "refs/uzi-recovery-episode/" + runId + "/" + parent,
    "refs/uzi-recovery-episode/" + runId + "/nested/" + tip]) {
    git(bare, ["update-ref", bad, tip]);
    await assert.rejects(cache.discardRetainedRecovery(bare, branch, key, expected, true), /pin identity/);
    assert.deepEqual(journal(), before);
    assert.equal(git(bare, ["rev-parse", good]), tip);
    assert.equal(git(bare, ["rev-parse", bad]), tip);
    git(bare, ["update-ref", "-d", bad, tip]);
  }
});

test("capture replaces only its own symbolic pin without updating the unrelated target", async () => {
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  const tip = await capture();
  const parent = git(bare, ["rev-parse", tip + "^"]);
  const target = "refs/keep/capture-target";
  const pin = "refs/uzi-recovery-episode/" + runId + "/" + tip;
  git(bare, ["update-ref", target, parent]);
  git(bare, ["symbolic-ref", pin, target]);
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  assert.equal(git(bare, ["rev-parse", target]), parent);
  assert.equal(git(bare, ["rev-parse", pin]), tip);
  assert.throws(() => git(bare, ["symbolic-ref", pin]));
  assert.equal(journal().recovery.stage, "captured");
});

test("discard rejects symbolic pins before touching unrelated targets or attribution", async () => {
  const { expected, tip } = await ready();
  const before = journal();
  const good = "refs/uzi-recovery-episode/" + runId + "/" + tip;
  const parent = git(bare, ["rev-parse", tip + "^"]);
  const target = "refs/keep/discard-target";
  const symbolic = "refs/uzi-recovery-episode/" + runId + "/" + parent;
  git(bare, ["update-ref", target, parent]);
  git(bare, ["symbolic-ref", symbolic, target]);
  await assert.rejects(cache.discardRetainedRecovery(bare, branch, key, expected, true), /pin identity/);
  assert.deepEqual(journal(), before);
  assert.equal(git(bare, ["rev-parse", good]), tip);
  assert.equal(git(bare, ["rev-parse", target]), parent);
  assert.equal(git(bare, ["symbolic-ref", symbolic]), target);
});

for (const packed of [false, true]) {
  test("discard rejects dangling symbolic pins with " + (packed ? "packed" : "loose") + " valid pins before any deletion", async () => {
    const { expected, tip } = await ready();
    const before = journal();
    const prefix = "refs/uzi-recovery-episode/" + runId + "/";
    const parent = git(bare, ["rev-parse", tip + "^"]);
    const valid = [tip, parent];
    git(bare, ["update-ref", prefix + parent, parent]);
    if (packed) {
      git(bare, ["pack-refs", "--all", "--prune"]);
      for (const oid of valid) assert.equal(fs.existsSync(path.join(bare, prefix + oid)), false);
    }
    const symbolic = prefix + "f".repeat(40);
    const target = "refs/keep/absent-discard-target";
    assert.throws(() => git(bare, ["rev-parse", "--verify", target]));
    git(bare, ["symbolic-ref", symbolic, target]);
    const symbolicBytes = fs.readFileSync(path.join(bare, symbolic));
    assert.equal(git(bare, ["for-each-ref", "--format=%(refname)", prefix]).includes(symbolic), false);
    await assert.rejects(cache.discardRetainedRecovery(bare, branch, key, expected, true), /pin identity/);
    assert.deepEqual(journal(), before);
    for (const oid of valid) assert.equal(git(bare, ["rev-parse", prefix + oid]), oid);
    assert.equal(git(bare, ["symbolic-ref", symbolic]), target);
    assert.deepEqual(fs.readFileSync(path.join(bare, symbolic)), symbolicBytes);
    assert.throws(() => git(bare, ["rev-parse", "--verify", target]));
  });
}

test("discard refuses malformed ref backend settings before deleting pins or attribution", async () => {
  const { expected, tip } = await ready();
  const before = journal();
  const pin = "refs/uzi-recovery-episode/" + runId + "/" + tip;
  const configPath = path.join(bare, "config");
  const originalConfig = fs.readFileSync(configPath);
  // Change only the declared backend, never convert the fixture's existing refs.
  fs.appendFileSync(configPath, "[core]\n\trepositoryformatversion = 1\n[extensions]\n\trefStorage = reftable\n");
  try {
    await assert.rejects(cache.discardRetainedRecovery(bare, branch, key, expected, true), /unsupported recovery ref backend|unsupported.*extension/i);
  } finally {
    fs.writeFileSync(configPath, originalConfig);
  }
  assert.deepEqual(journal(), before);
  assert.equal(git(bare, ["rev-parse", pin]), tip);
});

test("discard refuses malformed loose pin paths and symlink ancestors before any deletion", async () => {
  const { expected, tip } = await ready();
  const before = journal();
  const prefix = "refs/uzi-recovery-episode/" + runId + "/";
  const good = prefix + tip;
  const bad = path.join(bare, prefix + "f".repeat(40));
  for (const shape of ["directory", "symlink", "broken"] as const) {
    if (shape === "directory") fs.mkdirSync(bad);
    else if (shape === "symlink") fs.symlinkSync(path.join(bare, good), bad);
    else fs.writeFileSync(bad, "invalid ref bytes\n");
    await assert.rejects(cache.discardRetainedRecovery(bare, branch, key, expected, true), /pin identity/);
    assert.deepEqual(journal(), before);
    assert.equal(git(bare, ["rev-parse", good]), tip);
    fs.rmSync(bad, { recursive: true });
  }
  for (const relative of ["refs", "refs/uzi-recovery-episode", prefix]) {
    const dir = path.resolve(bare, relative);
    const saved = dir + ".saved";
    fs.renameSync(dir, saved);
    fs.symlinkSync(saved, dir);
    try {
      await assert.rejects(cache.discardRetainedRecovery(bare, branch, key, expected, true), /unsafe recovery pin namespace/);
      assert.deepEqual(journal(), before);
      assert.equal(git(bare, ["rev-parse", good]), tip);
    } finally {
      fs.unlinkSync(dir);
      fs.renameSync(saved, dir);
    }
  }
});

test("discard refuses writable loose namespace and pin files before clearing attribution", async () => {
  const { expected, tip } = await ready();
  const before = journal();
  const prefix = "refs/uzi-recovery-episode/" + runId + "/";
  const pin = path.join(bare, prefix + tip);
  for (const target of [pin, path.join(bare, "refs"), path.join(bare, "refs/uzi-recovery-episode"), path.join(bare, prefix)]) {
    const mode = fs.statSync(target).mode;
    fs.chmodSync(target, mode | 0o002);
    try {
      await assert.rejects(cache.discardRetainedRecovery(bare, branch, key, expected, true), /pin identity|pin namespace/);
      assert.deepEqual(journal(), before);
      assert.equal(git(bare, ["rev-parse", prefix + tip]), tip);
    } finally {
      fs.chmodSync(target, mode);
    }
  }
});

test("discard accepts packed-only pins when the loose run namespace is absent", async () => {
  const { expected, tip } = await ready();
  const prefix = "refs/uzi-recovery-episode/" + runId + "/";
  git(bare, ["config", "core.repositoryFormatVersion", "1"]);
  git(bare, ["config", "extensions.refStorage", "files"]);
  git(bare, ["pack-refs", "--all", "--prune"]);
  assert.equal(fs.existsSync(path.join(bare, prefix + tip)), false);
  fs.rmSync(path.join(bare, prefix), { recursive: true, force: true });
  await cache.discardRetainedRecovery(bare, branch, key, expected, true);
  assert.throws(() => git(bare, ["rev-parse", "--verify", prefix + tip]));
  assert.equal(git(bare, ["config", "uzi-recovery." + branch + ".clone"]), "");
});

test("explicit owner discard keeps paths intact and per-key attempt mode outlives cleared journal", async () => {
  assert.equal(await cache.recoveryAttemptMode(fx.originPath, key), false);
  const { expected, tip } = await ready();
  assert.equal(await cache.recoveryAttemptMode(fx.originPath, key), true);
  await cache.discardRetainedRecovery(bare, branch, key, expected, true);
  assert.equal(git(bare, ["config", `uzi-recovery.${branch}.clone`]), "");
  assert.equal(await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId), undefined);
  assert.equal(await recreate().recoveryAttemptMode(fx.originPath, key), true);
  assert.ok(fs.existsSync(source.clonePath));
  assert.ok(fs.existsSync(expected.clonePath));
  assert.throws(() => git(bare, ["rev-parse", "--verify", `refs/uzi-recovery-episode/${runId}/${tip}`]));
  await assert.rejects(cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId), /fresh attempt/);
  const clone = await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId, true, undefined, {
    attemptId: aid(3), isLive: () => false, beforeSeed: async () => {}, quiescent: async () => false,
  });
  assert.ok(clone.path.endsWith(".attempt-" + aid(3)));
});

test("nested recovery verification shares one delivered-byte budget across prerequisite roots", async () => {
  const tip = git(source.clonePath, ["rev-parse", "HEAD"]);
  await cache.withRecoveryOperation(new AbortController().signal, Date.now() + 10_000, async () => {
    assert.equal(await cache.verifyRecoveryClosure(bare, tip), true);
    const budget = (cache as any).recoveryOperations.getStore().closureBudget;
    assert.ok(budget.delivered > 0);
    const delivered = budget.delivered;
    budget.limit = delivered;
    await assert.rejects(cache.withRecoveryOperation(new AbortController().signal, Date.now() + 10_000,
      () => cache.verifyRecoveryClosure(bare, tip)), RecoveryClosureLimitError);
    assert.equal(budget.delivered, delivered);
  });
});
test("invalid SHA lists and batch metadata never request raw content", async () => {
  const tip = git(source.clonePath, ["rev-parse", "HEAD"]);
  const internals = cache as any;
  let contents = 0;
  internals.spawnGit = async () => { contents++; throw new Error("must not spawn"); };
  for (const [listing, metadata] of [
    [tip + "\n" + tip + "\n", ""],
    [tip + " filename\n", ""],
    [tip, ""],
    [tip + "\n", tip + " blob -1\n"],
    [tip + "\n", tip + " blob 01\n"],
    [tip + "\n", tip + " unknown 1\n"],
    [tip + "\n", tip + " missing\n"],
    [tip + "\n", tip + " blob 1\nextra\n"],
  ]) {
    internals.execScoped = async (_command: string, args: string[]) => ({
      stdout: args.includes("rev-parse") ? tip + "\n" : args.includes("rev-list") ? listing : metadata,
    });
    assert.equal(await cache.verifyRecoveryClosure(bare, tip), false);
  }
  assert.equal(contents, 0);
});

function loose(oid: string, bytes: Buffer): string {
  const dir = path.join(bare, "objects", oid.slice(0, 2));
  fs.mkdirSync(dir, { recursive: true });
  const file = path.join(dir, oid.slice(2));
  fs.writeFileSync(file, bytes);
  return file;
}
test("unreachable compressed bomb and corrupt blob do not affect reachable verification", async () => {
  const tip = git(source.clonePath, ["rev-parse", "HEAD"]);
  loose("e".repeat(40), deflateSync(Buffer.from("blob 1073741825\0")));
  loose("d".repeat(40), Buffer.from("corrupt unreachable blob"));
  assert.equal(await cache.verifyRecoveryClosure(bare, tip), true);
});
for (const damage of ["unreadable", "hash mismatch"] as const) {
  test("reachable blob " + damage + " rejects capture without a pin or journal change", async () => {
    await cache.reserveRecoveryIteration(bare, branch, key, source);
    const tip = await capture();
    const before = journal();
    const oid = git(bare, ["rev-parse", tip + ":work.txt"]);
    const file = path.join(bare, "objects", oid.slice(0, 2), oid.slice(2));
    fs.chmodSync(file, 0o600);
    if (damage === "unreadable") fs.writeFileSync(file, "broken");
    else loose(oid, deflateSync(Buffer.from("blob 4\0oops")));
    assert.equal(await cache.verifyRecoveryClosure(bare, tip), false);
    await assert.rejects(cache.recordRecoveryCapture(bare, branch, key, source, 1, tip));
    assert.deepEqual(journal(), before);
    assert.throws(() => git(bare, ["rev-parse", "--verify", `refs/uzi-recovery-episode/${runId}/${tip}`]));
    assert.ok(fs.existsSync(source.clonePath));
  });
}
test("production oversized declared loose object is refused before any content producer", async () => {
  const oid = "b".repeat(40);
  loose(oid, deflateSync(Buffer.from("blob 1073741825\0")));
  const treeBytes = Buffer.concat([Buffer.from("100644 huge\0"), Buffer.from(oid, "hex")]);
  const tree = execFileSync("git", ["-C", bare, "hash-object", "-t", "tree", "-w", "--stdin"], { input: treeBytes, encoding: "utf8" }).trim();
  const tip = git(bare, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "commit-tree", tree, "-m", "oversize"]);
  let contents = 0;
  const internals = cache as any;
  const original = internals.spawnGit.bind(cache);
  internals.spawnGit = async (...args: any[]) => { contents++; return original(...args); };
  await assert.rejects(cache.verifyRecoveryClosure(bare, tip), RecoveryClosureLimitError);
  assert.equal(contents, 0);
});

/** Entry is required: an already observed rejection must fail promptly, rather than hang. */
async function requireProducerEntry(started: Promise<void>, outcome: Promise<unknown>): Promise<void> {
  const entered = await Promise.race([started.then(() => true), outcome.then(() => false)]);
  assert.equal(entered, true, "verification ended before producer entry");
}

/** Keep the original deadline; fire its timer only after the producer seam is reached. */
function controlledDeadline(t: TestContext): () => void {
  const realSetTimeout = globalThis.setTimeout;
  let fire: (() => void) | undefined;
  t.mock.method(globalThis, "setTimeout", (callback: (...args: any[]) => void, delay?: number, ...args: any[]) => {
    if (delay !== undefined && delay > 200_000 && delay <= 300_000) fire ??= () => callback(...args);
    return realSetTimeout(callback, delay, ...args);
  });
  return () => { assert.ok(fire, "recovery deadline timer was armed"); fire(); };
}

test("preentry rejection fails the entry assertion promptly", async () => {
  const stop = new AbortController();
  const reason = new Error("fixture rejects before spawning");
  stop.abort(reason);
  let entered!: () => void, producers = 0;
  const started = new Promise<void>(resolve => { entered = resolve; });
  const operation = cache.withRecoveryOperation(stop.signal, Date.now() + 30_000,
    () => cache.withBoundaryProcessSpawner(async () => {
      producers++;
      entered();
      throw new Error("must not spawn");
    }, stop.signal, () => cache.verifyRecoveryClosure(bare, git(source.clonePath, ["rev-parse", "HEAD"]))));
  const outcome = operation.then(value => value, error => error);
  const entryCheck = requireProducerEntry(started, outcome);
  let watchdog: ReturnType<typeof setTimeout> | undefined;
  try {
    const observed = Promise.race([entryCheck, new Promise<never>((_resolve, reject) => {
      watchdog = setTimeout(() => reject(new Error("entry observer did not notice operation rejection")), 1_000);
    })]);
    await assert.rejects(observed, error => {
      assert.ok(error instanceof assert.AssertionError);
      assert.equal(error.actual, false);
      assert.equal(error.expected, true);
      assert.ok(error.message.startsWith("verification ended before producer entry"));
      return true;
    });
    assert.equal(await outcome, reason);
    assert.equal(producers, 0);
  } finally {
    if (watchdog) clearTimeout(watchdog);
    stop.abort(new Error("fixture cleanup"));
    entered();
    await Promise.allSettled([operation, outcome, entryCheck]);
  }
});

for (const stopAt of ["abort", "timeout"] as const) {
  test("actual streaming Git child " + stopAt + " settles before proof releases the lock", async (t) => {
    await cache.reserveRecoveryIteration(bare, branch, key, source);
    const before = journal();
    const tip = git(source.clonePath, ["rev-parse", "HEAD"]);
    const refsBefore = git(bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-recovery-episode/"]);
    const stop = new AbortController();
    let streamed!: () => void;
    const started = new Promise<void>(resolve => { streamed = resolve; });
    let cancelled = false, settled = false, decoded = 0;
    const expire = controlledDeadline(t);
    const reason = new Error("interrupted actual stream");
    const producers: Promise<unknown>[] = [];
    const operation = cache.withRecoveryOperation(stop.signal, Date.now() + 240_000,
      () => cache.withBoundaryProcessSpawner(async request => {
        const [executable, ...args] = request.argv;
        const child = spawn(executable!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
        const batch = args.includes("--batch");
        const completed = new Promise<{ code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => { if (batch) settled = true; resolve({ code: code ?? -1 }); });
        });
        producers.push(completed);
        // Keep only the content producer's stdin open after delivering its real OID list.
        const stdin = batch ? new Writable({
          write(chunk, _encoding, callback) { child.stdin.write(chunk, callback); },
          final(callback) { callback(); },
        }) : child.stdin;
        if (batch) child.stdout.once("data", bytes => { decoded += bytes.length; streamed(); });
        return { stdin, stdout: child.stdout, stderr: child.stderr, completed,
          cancel: async () => {
            if (batch) cancelled = true;
            if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
            await completed;
          } };
      }, new AbortController().signal,
      () => cache.withBareLock(bare, () => cache.verifyRecoveryClosure(bare, tip))));
    const outcome = operation.then(value => value, error => error);
    let observer: Promise<void> | undefined;
    try {
      await requireProducerEntry(started, outcome);
      if (stopAt === "abort") stop.abort(reason);
      else expire();
      observer = cache.withBareLock(bare, async () => assert.equal(settled, true));
      const result = await outcome;
      await observer;
      if (stopAt === "abort") assert.equal(result, reason);
      else { assert.ok(result instanceof Error); assert.equal(result.message, "recovery deadline exhausted"); }
    } finally {
      stop.abort(new Error("fixture cleanup"));
      await Promise.allSettled([operation, outcome, ...producers, ...(observer ? [observer] : [])]);
    }
    assert.ok(decoded > 0, "actual Git content reached the parser");
    assert.equal(cancelled, true);
    assert.equal(settled, true);
    assert.deepEqual(journal(), before);
    assert.equal(git(bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-recovery-episode/"]), refsBefore);
  });
}

for (const stopAt of ["parser", "abort", "timeout"] as const) {
  test(stopAt + " during raw verification settles supervised producer before lock release", async (t) => {
    const tip = git(source.clonePath, ["rev-parse", "HEAD"]);
    const raw = Buffer.from("blob 1\0x");
    const oid = createHash("sha1").update(raw).digest("hex");
    const internals = cache as any;
    internals.execScoped = async (_command: string, args: string[]) => ({
      stdout: args.includes("rev-parse") ? tip + "\n" : args.includes("rev-list") ? tip + "\n" :
        `${tip} blob 1\n`,
    });
    const stop = new AbortController();
    let entered!: () => void, finish!: (value: { code: number }) => void;
    const started = new Promise<void>(resolve => { entered = resolve; });
    const completed = new Promise<{ code: number }>(resolve => { finish = resolve; });
    const stdout = new PassThrough();
    let cancelled = false, settled = false;
    const expire = controlledDeadline(t);
    const reason = new Error("interrupted verification");
    let producerEntered = false;
    const operation = cache.withRecoveryOperation(stop.signal, Date.now() + 240_000,
      () => cache.withBoundaryProcessSpawner(async () => {
        producerEntered = true;
        entered();
        if (stopAt === "parser") stdout.end(`${oid} blob 1\nx\n`);
        return { stdout, stderr: Readable.from([]), stdin: null, completed,
          cancel: async () => {
            cancelled = true;
            await new Promise(resolve => setTimeout(resolve, 20));
            settled = true;
            stdout.destroy();
            finish({ code: -1 });
          } };
      }, new AbortController().signal, () => cache.withBareLock(bare, () => cache.verifyRecoveryClosure(bare, tip))));
    // Install the rejection observer before timeout/abort can fire.
    const outcome = operation.then(value => value, error => error);
    let observer: Promise<void> | undefined;
    try {
      await requireProducerEntry(started, outcome);
      assert.equal(producerEntered, true);
      if (stopAt === "abort") stop.abort(reason);
      if (stopAt === "timeout") expire();
      observer = cache.withBareLock(bare, async () => assert.equal(settled, true));
      const result = await outcome;
      await observer;
      assert.equal(cancelled, true);
      if (stopAt === "parser") assert.equal(result, false);
      else if (stopAt === "abort") assert.equal(result, reason);
      else { assert.ok(result instanceof Error); assert.equal(result.message, "recovery deadline exhausted"); }
    } finally {
      stop.abort(new Error("fixture cleanup"));
      await Promise.allSettled([operation, outcome, ...(producerEntered ? [completed] : []), ...(observer ? [observer] : [])]);
    }
  });
}
