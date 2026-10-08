import { it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, type PositiveOwedCandidateContext } from "../src/git.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

async function withJournalInventory(check: (f: {
  cache: GitCache; bare: string;
  config: (value: string, add?: boolean) => void;
  ownClone: () => Promise<string>;
}) => Promise<void>): Promise<void> {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "inventory-tombstone-"));
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  const git = (cwd: string, args: string[]) => execFileSync("git", ["-C", cwd, ...args],
    { env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], timeout: 10000 }).trim();
  try {
    const data = path.join(root, "data"), bare = path.join(data, "repos", "repo.git");
    await fs.mkdir(path.dirname(bare), { recursive: true, mode: 0o700 });
    execFileSync("git", ["init", "--bare", bare], { env, stdio: "pipe", timeout: 10000 });
    const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    await check({
      cache, bare,
      config: (value, add = false) => { git(bare, ["config", "--local", ...(add ? ["--add"] : []),
        "uzi-recovery.retired.clone", value]); },
      ownClone: async () => {
        const clone = cache.runnerClonePath(bare, "issue-current");
        await fs.mkdir(clone, { recursive: true });
        git(clone, ["init", "-b", "task"]);
        git(clone, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
          "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture"]);
        await cache.markRecoveryCapture(bare, clone, "task", "run-current");
        return git(clone, ["rev-parse", "HEAD"]);
      },
    });
  } finally { await fs.rm(root, { recursive: true, force: true }); }
}

for (const withOwnClone of [false, true]) {
  for (const staleValue of [false, true]) {
    it(`cleared journal leaves inventory verified (own clone=${withOwnClone}, stale value=${staleValue})`, async () => {
      await withJournalInventory(async f => {
        const head = withOwnClone ? await f.ownClone() : undefined;
        assert.equal((await f.cache.readInventoryCloneHeads(f.bare, "run-current")).kind, "verified");
        if (staleValue) f.config(JSON.stringify({ runId: "other-run", clonePath: "/outside/never-read" }));
        f.config("", staleValue);
        const result = await f.cache.readInventoryCloneHeads(f.bare, "run-current");
        assert.equal(result.kind, "verified");
        if (result.kind === "verified") {
          assert.deepEqual(result.heads, head ? [head] : []);
          assert.equal(result.clones.length, withOwnClone ? 1 : 0);
          assert.deepEqual(result.foreignOwners, []);
        }
      });
    });
  }
}

it("a later valid journal supersedes an earlier empty value", async () => {
  await withJournalInventory(async f => {
    f.config("");
    f.config(JSON.stringify({ runId: "other-run", clonePath: "/outside/never-read" }), true);
    assert.deepEqual(await f.cache.readInventoryCloneHeads(f.bare, "run-current"),
      { kind: "verified", heads: [], clones: [], foreignOwners: ["other-run"] });
  });
});

for (const value of [" ", "{", JSON.stringify({ runId: "other-run" })]) {
  it(`malformed nonempty journal still refuses inventory (${JSON.stringify(value)})`, async () => {
    await withJournalInventory(async f => {
      f.config(value);
      assert.deepEqual(await f.cache.readInventoryCloneHeads(f.bare, "run-current"), { kind: "unknown" });
    });
  });
}

for (const latestJournal of [false, true]) {
  it(`restart refuses older H/H2 archive when failed transfer leaves clone-only H3 (source journal=${latestJournal})`, async () => {
    const root = await fs.mkdtemp(path.join(os.tmpdir(), "inventory-clone-"));
    const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
    const cmd = (cwd: string, args: string[]) => execFileSync("git", ["-C", cwd, ...args],
      { env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], timeout: 10000 }).trim();
    try {
      const data = path.join(root, "data");
      const bare = path.join(data, "repos", "repo.git");
      await fs.mkdir(path.dirname(bare), { recursive: true, mode: 0o700 });
      execFileSync("git", ["init", "--bare", bare], { env, stdio: "pipe", timeout: 10000 });
      const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
      const clone = cache.runnerClonePath(bare, "issue-1945");
      await fs.mkdir(clone, { recursive: true });
      execFileSync("git", ["init", "-b", "task", clone], { env, stdio: "pipe", timeout: 10000 });
      cmd(clone, ["config", "user.name", "fixture"]);
      cmd(clone, ["config", "user.email", "fixture@example.com"]);
      cmd(clone, ["config", "commit.gpgsign", "false"]);
      const commit = async (text: string) => {
        await fs.writeFile(path.join(clone, "work.txt"), text);
        cmd(clone, ["add", "work.txt"]);
        cmd(clone, ["commit", "-m", text]);
        return cmd(clone, ["rev-parse", "HEAD"]);
      };
      const base = await commit("base");
      const h = await commit("H");
      cmd(clone, ["checkout", "--detach", base]);
      const h2 = await commit("H2");
      cmd(clone, ["push", bare, h + ":refs/heads/producer", h2 + ":refs/heads/current"]);
      const context: PositiveOwedCandidateContext = {
        barePath: bare, runId: "run-clone", generation: 7, kind: "issue", branch: "task",
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
      };
      for (const sha of [h, h2]) {
        assert.equal((await cache.updateTrackingRef(bare, "task", sha, { context })).kind, "updated");
      }
      // This worker-owned ownership evidence predates any model work or latest source journal.
      await cache.markRecoveryCapture(bare, clone, "task", context.runId);
      let expiry = "invalid", finals = 0, open = true;
      let manifest: { checksum: string; byte_size: number } | undefined;
      const client = {
        hasFeature: () => true,
        getRunOwnership: async () => ({ status: "failed", claim_generation: 7, inventory_guarded: true }),
        listRecoveryHolds: async () => ({ run_id: context.runId, holds: open ? [{
          hold_id: "hold", generation: 7, inventory_guarded: true, has_available_capture: false,
        }] : [] }),
        reserveRecoveryCapture: async () => ({ capture_id: "server", state: "preparing" }),
        uploadRecoveryBundle: async (_run: string, _id: string,
          m: { checksum: string; byte_size: number }, stream: AsyncIterable<Uint8Array>) => {
          for await (const chunk of stream) assert.ok(chunk.length);
          manifest = m;
          return { capture_id: "server", state: "available", manifest_bound: true };
        },
        getRecoveryCaptureStatus: async () => ({
          capture_id: "server", state: "available", manifest_bound: true,
          checksum: manifest?.checksum, byte_size: manifest?.byte_size, expires_at: expiry,
        }),
        releaseRecoveryCustody: async () => {
          finals++; open = false;
          return { run_id: context.runId, generation: 7, released: true, holds_released: 1 };
        },
      };
      const make = (git: GitCache) => new RecoveryCoordinator({
        recoveryRoot: path.join(data, "recovery"), workerToken: "fixture-worker",
        git, client: client as never, log: nullLogger(),
      });
      const coordinator = make(cache);
      const older = await coordinator.freezeInventory({ context, currentSha: h2, defaultBranch: "main" });
      assert.ok(older);
      assert.equal((await coordinator.captureAndUpload({ record: older, barePath: bare, defaultBranch: "main" })).state, "uploaded");
      assert.equal(finals, 0);
      if (latestJournal) cmd(clone, ["checkout", "-b", "clone-current"]);
      const h3 = await commit("clone-only H3");
      if (latestJournal) cmd(clone, ["pack-refs", "--all"]);
      cache.fetchAgentBranch = async () => { throw new Error("injected transfer failure before object/pin metadata"); };
      await assert.rejects(cache.fetchAgentBranch(bare, clone, "task", context.runId, { context }), /injected transfer failure/);
      assert.throws(() => cmd(bare, ["cat-file", "-t", h3]), "H3 is absent from the bare");
      assert.equal((await cache.enumerateOwedCandidates(bare, context.runId)).some(c => c.sha === h3), false);
      if (latestJournal) await coordinator.pin({
        runId: context.runId, generation: 7, inventoryGuarded: true,
        sourceSha: h3, kind: "issue", branch: "task",
      });
      // No RAM ownership or cache/coordinator identity crosses restart.
      const restartedCache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
      const restarted = make(restartedCache);
      expiry = "2099-01-01T00:00:00Z";
      await restarted.resumePending();
      const records = await restarted.inspect(context.runId);
      assert.equal(finals, 0);
      assert.equal(open, true);
      assert.equal(records.find(r => r.captureId === older.captureId)?.finalAcknowledged, undefined);
      await restarted.forgetGeneration(context.runId, 7);
      assert.ok((await restarted.inspect(context.runId)).some(r => r.captureId === older.captureId));
      assert.equal(cmd(clone, ["rev-parse", "HEAD"]), h3);

      // Positive foreign ownership is distinct from an unreadable own path.
      await restartedCache.markRecoveryCapture(bare, "/outside/never-read", "foreign", "other-run");
      const verified = await restartedCache.readInventoryCloneHeads(bare, context.runId);
      assert.equal(verified.kind, "verified");
      if (verified.kind === "verified") {
        assert.deepEqual(verified.heads, [h3]);
        assert.deepEqual(verified.foreignOwners, ["other-run"]);
      }
      await fs.rename(path.join(clone, ".git", "HEAD"), path.join(clone, ".git", "saved-HEAD"));
      await fs.symlink(path.join(clone, ".git", "saved-HEAD"), path.join(clone, ".git", "HEAD"));
      assert.deepEqual(await restartedCache.readInventoryCloneHeads(bare, context.runId), { kind: "unknown" });
    } finally { await fs.rm(root, { recursive: true, force: true }); }
  });
}

const inventoryRun = "run-inventory";
const inventoryAttempt = "20260907T030405Z-g3-0123456789abcdef";
const otherInventoryAttempt = "20260907T030405Z-g3-fedcba9876543210";
const emptyInventory = { kind: "verified", heads: [], clones: [], foreignOwners: [] };

async function inventoryFixture(attempt: boolean) {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "inventory-clone-"));
  const data = path.join(root, "data");
  const bare = path.join(data, "repos", "repo.git");
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  const git = (args: string[]) => execFileSync("git", args,
    { env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], timeout: 10000 }).trim();
  try {
    await fs.mkdir(path.dirname(bare), { recursive: true });
    git(["init", "--bare", bare]);
    const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    const runner = path.join(data, "runner");
    const canonical = cache.runnerClonePath(bare, "issue-2433");
    const clone = attempt ? canonical + ".attempt-" + inventoryAttempt : canonical;
    const parent = path.dirname(clone);
    const journal = (value: unknown, add = false, branch = "task") =>
      git(["-C", bare, "config", "--local", add ? "--add" : "--replace-all",
        `uzi-recovery.${branch}.clone`, JSON.stringify(value)]);
    const ledger = (value: unknown) =>
      git(["-C", bare, "config", "--local", "--replace-all", "uzi-attempts.task.entry", JSON.stringify(value)]);
    journal({ runId: inventoryRun, clonePath: clone, ...(attempt ? { attemptId: inventoryAttempt } : {}) });
    if (attempt) ledger({ attemptId: inventoryAttempt, runId: inventoryRun, clonePath: clone, state: "live" });
    const read = () => new GitCache(data, nullLogger(), undefined, testGitCacheOptions())
      .readInventoryCloneHeads(bare, inventoryRun);
    return { root, bare, runner, parent, clone, canonical, journal, ledger, read, git };
  } catch (err) {
    await fs.rm(root, { recursive: true, force: true });
    throw err;
  }
}

function traceInventoryLstat(t: TestContext) {
  const original = fs.lstat;
  const probes: string[] = [];
  const stub = t.mock.method(fs, "lstat", (async (...args: Parameters<typeof fs.lstat>) => {
    probes.push(String(args[0]));
    return original(...args);
  }) as typeof fs.lstat);
  return { probes, restore: () => stub.mock.restore() };
}

for (const attempt of [false, true]) {
  for (const missing of ["repo parent", "runner root", "clone leaf"]) {
    it(`inventory verifies absent ${missing} (${attempt ? "attempt" : "canonical"})`, async (t) => {
      const f = await inventoryFixture(attempt);
      const trace = traceInventoryLstat(t);
      try {
        if (missing === "repo parent") await fs.mkdir(f.runner);
        if (missing === "clone leaf") await fs.mkdir(f.parent, { recursive: true });
        assert.deepEqual(await f.read(), emptyInventory);
        assert.ok(trace.probes.includes(f.runner));
        if (missing === "runner root") {
          assert.equal(trace.probes.includes(f.parent), false);
          assert.equal(trace.probes.includes(f.clone), false);
        } else if (missing === "repo parent") {
          assert.ok(trace.probes.includes(f.parent));
          assert.equal(trace.probes.includes(f.clone), false);
        } else {
          assert.ok(trace.probes.includes(f.clone));
        }
      } finally {
        trace.restore();
        await fs.rm(f.root, { recursive: true, force: true });
      }
    });
  }
}

for (const attempt of [false, true]) {
  for (const ancestor of ["runner", "parent"] as const) {
    for (const unsafe of ["symlink", "dangling symlink", "file"]) {
      it(`inventory refuses ${unsafe} ${ancestor} (${attempt ? "attempt" : "canonical"})`, async () => {
        const f = await inventoryFixture(attempt);
        try {
          const target = f[ancestor];
          await fs.mkdir(path.dirname(target), { recursive: true });
          if (unsafe === "file") await fs.writeFile(target, "not a directory");
          else {
            const destination = path.join(f.root, "destination");
            if (unsafe === "symlink") await fs.mkdir(destination);
            await fs.symlink(destination, target);
          }
          assert.deepEqual(await f.read(), { kind: "unknown" });
        } finally { await fs.rm(f.root, { recursive: true, force: true }); }
      });
    }
    for (const code of ["EACCES", "EIO"]) {
      it(`inventory refuses ${code} at ${ancestor} (${attempt ? "attempt" : "canonical"})`, async (t) => {
        const f = await inventoryFixture(attempt);
        const original = fs.lstat;
        let injected = false;
        const stub = t.mock.method(fs, "lstat", (async (...args: Parameters<typeof fs.lstat>) => {
          if (String(args[0]) === f[ancestor]) {
            injected = true;
            throw Object.assign(new Error("injected ancestor read failure"), { code });
          }
          return original(...args);
        }) as typeof fs.lstat);
        try {
          await fs.mkdir(f.parent, { recursive: true });
          assert.deepEqual(await f.read(), { kind: "unknown" });
          assert.equal(injected, true);
        } finally {
          stub.mock.restore();
          await fs.rm(f.root, { recursive: true, force: true });
        }
      });
    }
  }
}

for (const malformed of ["journal", "ledger"]) {
  it(`inventory refuses malformed ${malformed} before absent-path handling`, async () => {
    const f = await inventoryFixture(true);
    try {
      if (malformed === "journal") f.journal({ runId: inventoryRun });
      else f.ledger({ attemptId: inventoryAttempt, state: "invalid" });
      assert.deepEqual(await f.read(), { kind: "unknown" });
    } finally { await fs.rm(f.root, { recursive: true, force: true }); }
  });
}

for (const owner of ["missing", "wrong"]) {
  it(`inventory refuses ${owner} attempt ledger owner even with absent runner`, async () => {
    const f = await inventoryFixture(true);
    try {
      if (owner === "missing") f.git(["-C", f.bare, "config", "--local", "--unset-all", "uzi-attempts.task.entry"]);
      else f.ledger({ attemptId: inventoryAttempt, runId: "other-run", clonePath: f.clone, state: "live" });
      assert.deepEqual(await f.read(), { kind: "unknown" });
    } finally { await fs.rm(f.root, { recursive: true, force: true }); }
  });
}

for (const present of [false, true]) {
  it(`inventory refuses journal ID A/path B with valid B ledger (present=${present})`, async () => {
    const f = await inventoryFixture(true);
    try {
      f.journal({ runId: inventoryRun, clonePath: f.clone, attemptId: otherInventoryAttempt });
      if (present) {
        await fs.mkdir(f.clone, { recursive: true });
        f.git(["init", "-b", "task", f.clone]);
        f.git(["-C", f.clone, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
          "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture"]);
      }
      assert.deepEqual(await f.read(), { kind: "unknown" });
    } finally { await fs.rm(f.root, { recursive: true, force: true }); }
  });
}

it("inventory refuses explicit attempt ID on absent canonical path", async () => {
  const f = await inventoryFixture(false);
  try {
    f.journal({ runId: inventoryRun, clonePath: f.clone, attemptId: inventoryAttempt });
    assert.deepEqual(await f.read(), { kind: "unknown" });
  } finally { await fs.rm(f.root, { recursive: true, force: true }); }
});

it("inventory accepts legacy attempt journal and uses last journal and ledger values", async () => {
  const f = await inventoryFixture(true);
  try {
    f.ledger({ attemptId: inventoryAttempt, runId: "other-run", clonePath: f.clone, state: "live" });
    f.git(["-C", f.bare, "config", "--local", "--add", "uzi-attempts.task.entry",
      JSON.stringify({ attemptId: inventoryAttempt, runId: inventoryRun, clonePath: f.clone, state: "live" })]);
    f.journal({ runId: inventoryRun, clonePath: f.clone, attemptId: otherInventoryAttempt });
    f.journal({ runId: inventoryRun, clonePath: f.clone }, true);
    await fs.mkdir(f.clone, { recursive: true });
    f.git(["init", "-b", "task", f.clone]);
    f.git(["-C", f.clone, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
      "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture"]);
    const head = f.git(["-C", f.clone, "rev-parse", "HEAD"]);
    assert.deepEqual(await f.read(), {
      kind: "verified", heads: [head],
      clones: [{ clonePath: f.clone, branch: "task", runId: inventoryRun }], foreignOwners: [],
    });
  } finally { await fs.rm(f.root, { recursive: true, force: true }); }
});

it("inventory continues past absent sibling and refuses later unsafe sibling", async (t) => {
  const f = await inventoryFixture(false);
  const trace = traceInventoryLstat(t);
  try {
    // Both paths have valid canonical attribution; the absent leaf must not hide the unsafe sibling.
    const absent = path.join(f.root, "missing-target");
    const missingLeaf = f.canonical + "-absent";
    f.journal({ runId: inventoryRun, clonePath: missingLeaf });
    f.journal({ runId: inventoryRun, clonePath: f.clone }, false, "unsafe");
    await fs.mkdir(f.parent, { recursive: true });
    await fs.symlink(absent, f.clone);
    assert.deepEqual(await f.read(), { kind: "unknown" });
    assert.ok(trace.probes.indexOf(missingLeaf) >= 0);
    assert.ok(trace.probes.indexOf(f.clone) > trace.probes.indexOf(missingLeaf));
  } finally {
    trace.restore();
    await fs.rm(f.root, { recursive: true, force: true });
  }
});
