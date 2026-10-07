import { it } from "node:test";
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
