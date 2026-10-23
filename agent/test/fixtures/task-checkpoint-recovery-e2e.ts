// Driven only by TestTaskCheckpointCrossWorkerRecoveryE2E. All network Git uses
// the Go-owned smart-HTTP origin; no fixture installs checkpoint objects remotely.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import "../setup/hermetic-proc.js";
import { GitCache } from "../../src/git.js";
import { RunRunner } from "../../src/runner.js";
import { WorkerClient } from "../../src/client.js";
import type { ClaimResponse } from "../../src/protocol.js";
import { FakeApi } from "../fake-api.js";
import { fakeGitlab } from "../runner-harness.js";
import { portableTeardownTestDeps } from "../teardown-fixtures.js";
import { defaultGitleaksShim } from "../gitleaks-shim.js";
import { makeScratchRoot } from "../fixture-repo.js";
import { noProofReseed, recordingLogger, testGitCacheOptions } from "../helpers.js";

type Claim = {
  run_id: string; claim_generation: number; checkpoint_tip?: string;
  checkpoint_write_unresolved?: boolean;
  checkpoint_coordinates: { work_branch: string; checkpoint_key: string };
};
const [action, configPath] = process.argv.slice(2);
assert.ok(configPath);
const cfg = JSON.parse(fs.readFileSync(configPath, "utf8")) as {
  scratch: string; origin: string; url: string; claim: Claim; mode: string; continuation_tip?: string;
};
const env = { ...process.env };
for (const key of Object.keys(env)) if (key.startsWith("GIT_")) delete env[key];
Object.assign(env, { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" });
function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env, encoding: "utf8", stdio: "pipe" }).trim();
}
function commit(cwd: string, file: string, bytes: string): string {
  fs.writeFileSync(path.join(cwd, file), bytes);
  git(cwd, "add", file);
  git(cwd, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
    "-c", "commit.gpgsign=false", "commit", "-qm", file);
  return git(cwd, "rev-parse", "HEAD");
}
const statePath = path.join(cfg.scratch, "state.json");
const binary = Buffer.from([0, 255, 13, 10, 128, 42, 0]);
if (action === "init") {
  const seed = makeScratchRoot("seed-", cfg.scratch);
  git(seed, "init", "-b", "main");
  commit(seed, "tracked.txt", "initial tracked\n");
  git(seed, "clone", "--bare", seed, cfg.origin);
  git(cfg.origin, "config", "http.receivepack", "true");
  console.log("initialized");
} else if (action === "delete-A") {
  const data = path.join(cfg.scratch, "worker-A");
  assert.equal(fs.existsSync(data), true, "A survives until the publication attempt completes");
  fs.rmSync(data, { recursive: true });
  assert.equal(fs.existsSync(data), false, "complete A attempt and cache removed before B claim");
  console.log("deleted-complete-worker-A-data");
} else if (action === "advance-default") {
  const checkout = makeScratchRoot("default-", cfg.scratch);
  git(checkout, "clone", cfg.url, checkout + "/tree");
  commit(checkout + "/tree", "default-moved.txt", "new default unrelated to saved work\n");
  git(checkout + "/tree", "push", "origin", "main");
  console.log("default-advanced");
} else {
  const claim = cfg.claim;
  const { work_branch: branch, checkpoint_key: key } = claim.checkpoint_coordinates;
  assert.equal(key, "uzi/task/" + claim.run_id);
  const data = path.join(cfg.scratch, action === "prepare" ? "worker-A" : "worker-B-" + cfg.mode);
  assert.equal(fs.existsSync(data), false, "each worker starts with an empty cache");
  fs.mkdirSync(data, { mode: 0o700 });
  const { logger, lines } = recordingLogger();
  const cache = new GitCache(data, logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
  // Publication reaches this same origin through real HTTP receive-pack. Worker
  // clones use its file transport, avoiding the unrelated raw-file-URI escaping
  // bug for cache directories derived from HTTP URLs with explicit ports.
  const bare = await cache.ensureClone(cfg.origin, undefined, undefined, key);
  const seed = () => cache.runnerCloneForBranch(bare, branch, "task-e2e", noProofReseed,
    claim.run_id, true, claim.checkpoint_tip, undefined,
    { checkpointKey: key, checkpointWriteUnresolved: claim.checkpoint_write_unresolved });
  if (action === "prepare") {
    assert.equal(claim.checkpoint_write_unresolved, false);
    const clone = await seed();
    const base = git(clone.path, "rev-parse", "HEAD");
    const committed = commit(clone.path, "committed.txt", "worker A committed\n");
    fs.writeFileSync(path.join(clone.path, "tracked.txt"), "dirty tracked\n");
    fs.writeFileSync(path.join(clone.path, "untracked.txt"), "untracked exact\n");
    fs.writeFileSync(path.join(clone.path, "binary.bin"), binary);
    assert.equal(await cache.commitWipMarker(clone.path), true);
    const marker = git(clone.path, "rev-parse", "HEAD");
    assert.equal(git(clone.path, "rev-parse", marker + "^"), committed);
    assert.equal(git(clone.path, "status", "--porcelain"), "");
    const defaultBranch = await cache.defaultBranchName(bare);
    assert.ok(defaultBranch);
    const sha = await cache.originBranchTip(bare, defaultBranch);
    assert.ok(sha);
    const imported = await cache.fetchAgentBranch(bare, clone.path, branch, claim.run_id, {
      context: { barePath: bare, branch, runId: claim.run_id, generation: claim.claim_generation,
        kind: "task", defaultIdentity: { ref: "refs/remotes/origin/" + defaultBranch, sha } },
    });
    assert.equal(imported.kind, "updated");
    assert.equal(await cache.verifyRunnerTrackingCovers(bare, clone.path, branch), true, "existing capture verification covers the complete marker");
    assert.equal((await cache.committedTrackingOwnership(bare, branch, claim.run_id, marker,
      claim.claim_generation)).kind, "owned");
    const packed = await cache.checkpointPack(bare, branch, { defaultBranch: "main" });
    assert.ok(packed);
    const chunks: Buffer[] = [];
    for await (const chunk of packed.pack) chunks.push(Buffer.from(chunk));
    assert.equal(await packed.exited, 0);
    fs.writeFileSync(path.join(cfg.scratch, "checkpoint.pack"), Buffer.concat(chunks));
    fs.writeFileSync(statePath, JSON.stringify({ base, committed, marker, tip: packed.tipOid }));
    assert.equal(fs.existsSync(data), true, "A stays available until real publication");
    console.log(JSON.stringify({ tip: packed.tipOid }));
  } else {
    const state = JSON.parse(fs.readFileSync(statePath, "utf8")) as {
      base: string; committed: string; marker: string; tip: string;
    };
    assert.equal(fs.existsSync(path.join(cfg.scratch, "worker-A")), false);
    if (["absent", "mismatch", "unrelated", "unknown"].includes(cfg.mode)) {
      await assert.rejects(seed(), cfg.mode === "unknown" ? /write unresolved or unknown/ :
        cfg.mode === "unrelated" ? /cannot be safely adopted/ : /owner anchor mismatch/);
      console.log("refused:" + cfg.mode);
    } else if (cfg.mode === "refused") {
      assert.equal(claim.checkpoint_write_unresolved, false, "definite refusal carries explicit false");
      assert.equal(claim.checkpoint_tip, undefined);
      const clone = await seed();
      assert.ok(cfg.continuation_tip, "known refusal retains a validated inherited branch");
      assert.equal(git(clone.path, "rev-parse", "HEAD"), cfg.continuation_tip);
      assert.equal(clone.branch, branch);
      assert.equal(fs.existsSync(path.join(clone.path, "committed.txt")), false);
      console.log("safe-branch-continuation");
    } else if (cfg.mode === "runner-feed") {
      const api = new FakeApi("task-checkpoint-fixture");
      const client = new WorkerClient(await api.listen(), "task-checkpoint-fixture", "e2e", logger);
      const { gitlab } = fakeGitlab();
      let entered = false;
      const runner = new RunRunner(client, cache, () => ({ executor: { run: async ctx => {
        entered = true;
        assert.equal(git(ctx.worktreePath, "rev-parse", "HEAD"), state.committed);
        assert.equal(ctx.branch, branch);
        assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "tracked.txt"), "utf8"), "dirty tracked\n");
        assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "untracked.txt"), "utf8"), "untracked exact\n");
        assert.deepEqual(fs.readFileSync(path.join(ctx.worktreePath, "binary.bin")), binary);
        assert.deepEqual(git(ctx.worktreePath, "diff", "--cached", "--name-only").split("\n").sort(),
          ["binary.bin", "tracked.txt", "untracked.txt"]);
        // The executor may now continue normally; commit only AFTER proving the
        // saved bytes arrived uncommitted at the public execution seam.
        git(ctx.worktreePath, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
          "-c", "commit.gpgsign=false", "commit", "-qm", "continue recovered work");
        return { branch: ctx.branch };
      } } }), logger, 20, undefined, { gitlab, pollMs: 5, teardownTestDeps: portableTeardownTestDeps });
      try {
        const payload = structuredClone(cfg.claim) as unknown as ClaimResponse;
        payload.repo.clone_url = cfg.origin;
        await runner.execute(payload);
        assert.equal(entered, true, "production runner enters executor only after recovery");
        assert.ok(api.messages(claim.run_id).some(message =>
          message.kind === "status" && message.agent === "worker" &&
          message.payload.text === "recovered 1 commit(s) plus your uncommitted work-in-progress from a checkpoint on another worker"),
          "public worker feed identifies cross-worker committed plus uncommitted recovery");
        assert.ok(api.states.some(s => s.runId === claim.run_id && s.body.status === "completed"));
        assert.ok(lines.some(line => {
          const entry = line as Record<string, unknown>;
          return entry.level === "info" &&
            entry.msg === "runner clone: recovered wip(park) marker to uncommitted (reset --soft)" &&
            entry.branch === branch && entry.marker === state.marker &&
            entry.parent === state.committed && entry.seeded_from === "checkpoint";
        }), "real GitCache diagnostic identifies the exact peeled marker and parent");
      } finally { await api.close(); }
      console.log("runner-recovered-feed-and-diagnostics");
    } else {
      assert.notEqual(branch, key, "descendant recovery inherits root branch with a current-child key");
      assert.equal(claim.checkpoint_tip, state.tip);
      assert.equal(claim.checkpoint_write_unresolved, false);
      const clone = await seed();
      assert.equal(clone.seededFrom, "checkpoint");
      assert.equal(clone.wipRecovered, true);
      assert.equal(clone.branch, branch);
      assert.equal(git(clone.path, "symbolic-ref", "--short", "HEAD"), branch);
      assert.equal(git(clone.path, "rev-parse", "HEAD"), state.committed);
      git(clone.path, "merge-base", "--is-ancestor", state.base, "HEAD");
      assert.equal(fs.readFileSync(path.join(clone.path, "committed.txt"), "utf8"), "worker A committed\n");
      assert.equal(fs.readFileSync(path.join(clone.path, "tracked.txt"), "utf8"), "dirty tracked\n");
      assert.equal(fs.readFileSync(path.join(clone.path, "untracked.txt"), "utf8"), "untracked exact\n");
      assert.deepEqual(fs.readFileSync(path.join(clone.path, "binary.bin")), binary);
      assert.deepEqual(git(clone.path, "diff", "--cached", "--name-only").split("\n").sort(),
        ["binary.bin", "tracked.txt", "untracked.txt"]);
      assert.equal(fs.existsSync(path.join(clone.path, "default-moved.txt")), false);
      console.log("exact-bytes-uncommitted-ancestry-branch");
    }
  }
}
