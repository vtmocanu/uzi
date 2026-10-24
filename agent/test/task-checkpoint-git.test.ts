import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { prepareFrozenHeld, validFrozenHeldPolicy } from "../src/frozen-held.js";
import { makeFixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { writeGitleaksShim, shimCalls } from "./gitleaks-shim.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";

const ROOT = "11111111-1111-4111-8111-111111111111";
const CHILD = "22222222-2222-4222-8222-222222222222";
const BRANCH = "uzi/task/11111111-1111-4111-8111-111111111111";
const CHILD_KEY = "uzi/task/22222222-2222-4222-8222-222222222222";
const REF = "refs/uzi-checkpoints/uzi/task/22222222-2222-4222-8222-222222222222";
function fixtureGitEnv(inherited: NodeJS.ProcessEnv = process.env): NodeJS.ProcessEnv {
  const env = { ...inherited };
  for (const key of Object.keys(env)) {
    if (key.startsWith("GIT_")) delete env[key];
  }
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
}
function cmd(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", stdio: "pipe",
    env: fixtureGitEnv() }).trim();
}
function commit(cwd: string, file: string, message = file, content = file): string {
  fs.mkdirSync(path.dirname(path.join(cwd, file)), { recursive: true });
  fs.writeFileSync(path.join(cwd, file), content);
  cmd(cwd, "add", file);
  cmd(cwd, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
    "-c", "commit.gpgsign=false", "commit", "-qm", message);
  return cmd(cwd, "rev-parse", "HEAD");
}
function rig() {
  const fx = makeFixture();
  const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  return { fx, cache, seed: (bare: string, run = CHILD, anchor?: string) =>
    cache.runnerCloneForBranch(bare, BRANCH, "task-" + run, noProofReseed, run, true, anchor,
      undefined, { checkpointWriteUnresolved: false, checkpointKey: "uzi/task/" + run }) };
}

for (const run of [ROOT, CHILD]) it("restores the exact authorized task ref for " + run, async () => {
  const r = rig();
  try {
    const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
    const tip = commit(r.fx.originPath, "retained");
    cmd(r.fx.originPath, "update-ref", "refs/uzi-checkpoints/uzi/task/" + run, tip);
    cmd(r.fx.originPath, "reset", "--hard", base);
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, "uzi/task/" + run);
    const clone = await r.seed(bare, run, tip);
    assert.equal(clone.branch, BRANCH);
    assert.equal(clone.seededFrom, "checkpoint");
    assert.equal(cmd(clone.path, "rev-parse", "HEAD"), tip);
    assert.equal(fs.readFileSync(path.join(clone.path, "retained"), "utf8"), "retained");
  } finally { r.fx.cleanup(); }
});

it("a warm stale mirror cannot mask remote deletion or a changed anchor", async () => {
  const r = rig();
  try {
    const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
    const tip = commit(r.fx.originPath, "retained");
    cmd(r.fx.originPath, "update-ref", REF, tip);
    cmd(r.fx.originPath, "reset", "--hard", base);
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    cmd(r.fx.originPath, "update-ref", "-d", REF);
    await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    assert.throws(() => cmd(bare, "rev-parse", "--verify", REF));
    await assert.rejects(r.seed(bare, CHILD, tip), /owner anchor mismatch/);
    cmd(r.fx.originPath, "update-ref", REF, base);
    await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    await assert.rejects(r.seed(bare, CHILD, tip), /owner anchor mismatch/);
  } finally { r.fx.cleanup(); }
});

it("unavailable freshness, lost acknowledgements and foreign keys never cold-adopt", async () => {
  const r = rig();
  try {
    const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
    const tip = commit(r.fx.originPath, "foreign");
    cmd(r.fx.originPath, "update-ref", REF, tip);
    cmd(r.fx.originPath, "reset", "--hard", base);
    let bare = await r.cache.ensureClone(r.fx.originPath);
    await assert.rejects(r.seed(bare, CHILD, tip), /freshness unavailable/);
    bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    await assert.rejects(r.seed(bare), /owner anchor mismatch/);
    cmd(r.fx.originPath, "update-ref", "-d", REF);
    cmd(r.fx.originPath, "update-ref", "refs/uzi-checkpoints/" + BRANCH, tip);
    await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    await assert.rejects(r.seed(bare, CHILD, tip), /owner anchor mismatch/);
    const fresh = await r.seed(bare);
    assert.equal(cmd(fresh.path, "rev-parse", "HEAD"), base, "root key is never the child's fallback");
  } finally { r.fx.cleanup(); }
});

it("enrollment alone allows the inherited branch, and verified local recovery survives remote absence", async () => {
  const r = rig();
  try {
    const inherited = commit(r.fx.originPath, "inherited");
    cmd(r.fx.originPath, "branch", BRANCH);
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    const first = await r.seed(bare);
    assert.equal(cmd(first.path, "rev-parse", "HEAD"), inherited);
    const tip = commit(first.path, "local");
    await fixtureFetchTracking(r.cache, bare, first.path, BRANCH, CHILD);
    const restored = await r.seed(bare, CHILD, tip);
    assert.equal(restored.seededFrom, "tracking");
    assert.equal(cmd(restored.path, "rev-parse", "HEAD"), tip);
  } finally { r.fx.cleanup(); }
});

it("report-only looks up the key while computing marker and content floors from the branch", async () => {
  const r = rig();
  try {
    const tip = commit(r.fx.originPath, "already-published");
    cmd(r.fx.originPath, "branch", BRANCH);
    cmd(r.fx.originPath, "update-ref", REF, tip);
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    assert.equal(await r.cache.hasCommittedCheckpoint(bare, BRANCH, undefined, CHILD_KEY), false);
    const marker = commit(r.fx.originPath, "uncommitted", "wip(park): fixture");
    cmd(r.fx.originPath, "update-ref", REF, marker);
    cmd(r.fx.originPath, "reset", "--hard", tip);
    await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    assert.equal(await r.cache.hasCommittedCheckpoint(bare, BRANCH, undefined, CHILD_KEY), false);
    const work = commit(r.fx.originPath, "committed");
    cmd(r.fx.originPath, "update-ref", REF, work);
    cmd(r.fx.originPath, "reset", "--hard", tip);
    await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    assert.equal(await r.cache.hasCommittedCheckpoint(bare, BRANCH, undefined, CHILD_KEY), true);
    assert.equal(await r.cache.hasCommittedCheckpoint(bare, BRANCH, undefined, null), false);
  } finally { r.fx.cleanup(); }
});


it("overlay chains and cross-worker peeling belong to the child's key, independently of its root's ref", async () => {
  const r = rig();
  try {
    commit(r.fx.originPath, ".github/workflows/ci.yml", "workflow v1", "name: v1\n");
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    const clone = await r.seed(bare);
    const real1 = commit(clone.path, "first");
    await fixtureFetchTracking(r.cache, bare, clone.path, BRANCH, CHILD);
    commit(r.fx.originPath, ".github/workflows/ci.yml", "workflow v2", "name: v2\n");
    async function publish(previous?: string): Promise<string> {
      const packed = await r.cache.checkpointPack(bare, BRANCH, { defaultBranch: "main", prevCheckpointTip: previous });
      assert.ok(packed);
      const chunks: Buffer[] = [];
      for await (const chunk of packed.pack) chunks.push(Buffer.from(chunk));
      assert.equal(await packed.exited, 0);
      execFileSync("git", ["-C", r.fx.originPath, "index-pack", "--stdin", "--fix-thin"], {
        input: Buffer.concat(chunks), stdio: "pipe", env: fixtureGitEnv() });
      cmd(r.fx.originPath, "update-ref", REF, packed.tipOid);
      return packed.tipOid;
    }
    const overlay1 = await publish();
    assert.notEqual(overlay1, real1);
    const real2 = commit(clone.path, "second");
    await fixtureFetchTracking(r.cache, bare, clone.path, BRANCH, CHILD);
    const overlay2 = await publish(overlay1);
    assert.deepEqual(cmd(bare, "show", "-s", "--format=%P", overlay2).split(" "), [overlay1, real2]);
    cmd(r.fx.originPath, "update-ref", "refs/uzi-checkpoints/" + BRANCH, overlay1);
    await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    const localRestored = await r.seed(bare, CHILD, overlay2);
    assert.equal(localRestored.seededFrom, "tracking");
    assert.equal(cmd(localRestored.path, "rev-parse", "HEAD"), real2,
      "local real content proves containment of its own verified overlay anchor");
    const other = new GitCache(path.join(r.fx.dataDir, "other"), nullLogger(), undefined, testGitCacheOptions());
    const otherBare = await other.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    const restored = await other.runnerCloneForBranch(otherBare, BRANCH, "child", noProofReseed, CHILD, true,
      overlay2, undefined, { checkpointWriteUnresolved: false, checkpointKey: CHILD_KEY });
    assert.equal(cmd(restored.path, "rev-parse", "HEAD"), real2);
    assert.equal(fs.readFileSync(path.join(restored.path, ".github/workflows/ci.yml"), "utf8"), "name: v1\n");
    await assert.rejects(other.runnerCloneForBranch(otherBare, BRANCH, "wrong", noProofReseed, CHILD, true,
      overlay1, undefined, { checkpointWriteUnresolved: false, checkpointKey: CHILD_KEY }), /owner anchor mismatch/);
    await other.ensureClone(r.fx.originPath, undefined, undefined, BRANCH);
    const root = await other.runnerCloneForBranch(otherBare, BRANCH, "root", noProofReseed, ROOT, true,
      overlay1, undefined, { checkpointWriteUnresolved: false, checkpointKey: BRANCH });
    assert.equal(cmd(root.path, "rev-parse", "HEAD"), real1);
  } finally { r.fx.cleanup(); }
});

it("frozen held packs and secret scan floors use the work branch, with task confirmation bound to its key", async () => {
  const r = rig();
  try {
    const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
    const publishedBranch = commit(r.fx.originPath, "published-branch");
    cmd(r.fx.originPath, "branch", BRANCH);
    const confirmed = commit(r.fx.originPath, "confirmed-checkpoint");
    cmd(r.fx.originPath, "update-ref", REF, confirmed);
    cmd(r.fx.originPath, "reset", "--hard", base);
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    const clone = await r.seed(bare, CHILD, confirmed);
    const source = commit(clone.path, "pending");
    await fixtureFetchTracking(r.cache, bare, clone.path, BRANCH, CHILD);
    const policy = { version: 1 as const, blocked: false, everKnown: false, flaggedCommits: [],
      ciGlobs: null, confirmedTip: confirmed, checkpointKey: CHILD_KEY, extraFloors: [] };
    assert.equal(validFrozenHeldPolicy(policy, { kind: "task", runId: CHILD }), true);
    assert.equal(validFrozenHeldPolicy(policy, { kind: "task", runId: ROOT }), false);
    assert.equal(validFrozenHeldPolicy({ ...policy, checkpointKey: undefined }, { kind: "task", runId: CHILD }), false);
    const ranges: Array<{ excludeSha: string; scanFloorShas?: string[] }> = [];
    r.cache.secretScanCheckpointRange = async (_bare, range) => {
      ranges.push(range);
      return { trusted: true, findings: [] };
    };
    const result = await prepareFrozenHeld({ git: r.cache, barePath: bare, branch: BRANCH, sourceSha: source,
      policy, deadlineMs: 30_000 });
    assert.ok("pack" in result);
    assert.equal(ranges.length, 1);
    assert.equal(ranges[0]!.excludeSha, publishedBranch);
    assert.ok(ranges[0]!.scanFloorShas?.includes(confirmed));
    execFileSync("git", ["-C", r.fx.originPath, "index-pack", "--stdin", "--fix-thin"], {
      input: result.pack, stdio: "pipe", env: fixtureGitEnv() });
    assert.equal(cmd(r.fx.originPath, "rev-parse", source + "^{commit}"), source);
  } finally { r.fx.cleanup(); }
});

for (const state of [true, undefined, null, "false", 0]) it(
  "task write state " + JSON.stringify(state) + " refuses cold fallback despite proven exact-ref absence",
  async () => {
    const r = rig();
    try {
      const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
      cmd(r.fx.originPath, "branch", BRANCH);
      const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
      await assert.rejects(r.cache.runnerCloneForBranch(bare, BRANCH, "unknown", noProofReseed, CHILD, false,
        undefined, undefined, { checkpointKey: CHILD_KEY, checkpointWriteUnresolved: state as boolean | undefined }),
      /write unresolved or unknown/);
      assert.equal(cmd(bare, "rev-parse", "refs/remotes/origin/" + BRANCH), base);
    } finally { r.fx.cleanup(); }
  });

for (const state of [true, undefined]) it(
  "an older anchor and same-run tracking cannot discharge write state " + state,
  async () => {
    const r = rig();
    try {
      const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
      const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
      const local = await r.seed(bare);
      const older = commit(local.path, "older-anchor");
      await fixtureFetchTracking(r.cache, bare, local.path, BRANCH, CHILD);
      cmd(r.fx.originPath, "fetch", local.path, "HEAD");
      cmd(r.fx.originPath, "update-ref", REF, older);
      // A later uncertain write may have reached the exact key, beyond the claim anchor.
      const pending = commit(local.path, "pending-write");
      cmd(r.fx.originPath, "fetch", local.path, "HEAD");
      cmd(r.fx.originPath, "update-ref", REF, pending);
      cmd(r.fx.originPath, "reset", "--hard", base);
      await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
      await assert.rejects(r.cache.runnerCloneForBranch(bare, BRANCH, "pending", noProofReseed, CHILD, true,
        older, undefined, { checkpointKey: CHILD_KEY, checkpointWriteUnresolved: state }),
      /write unresolved or unknown/);
    } finally { r.fx.cleanup(); }
  });

for (const competing of [false, true]) it(
  "a stale same-run tracking stamp cannot drop its published anchor; competing branch=" + competing,
  async () => {
    const r = rig();
    try {
      const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
      const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
      const local = await r.seed(bare);
      commit(local.path, "stale-local");
      await fixtureFetchTracking(r.cache, bare, local.path, BRANCH, CHILD);
      const anchor = commit(r.fx.originPath, "published-anchor");
      cmd(r.fx.originPath, "update-ref", REF, anchor);
      cmd(r.fx.originPath, "reset", "--hard", base);
      if (competing) {
        commit(r.fx.originPath, "competing-published");
        cmd(r.fx.originPath, "branch", BRANCH);
        cmd(r.fx.originPath, "reset", "--hard", base);
      }
      await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
      await assert.rejects(r.seed(bare, CHILD, anchor), /cannot be safely adopted/);
    } finally { r.fx.cleanup(); }
  });

it("a divergent competing branch and moved default never replace a task checkpoint", async () => {
  const r = rig();
  try {
    const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
    const anchor = commit(r.fx.originPath, "published-anchor");
    cmd(r.fx.originPath, "update-ref", REF, anchor);
    cmd(r.fx.originPath, "reset", "--hard", base);
    commit(r.fx.originPath, "competing-published");
    cmd(r.fx.originPath, "branch", BRANCH);
    cmd(r.fx.originPath, "reset", "--hard", base);
    commit(r.fx.originPath, "default-advanced");
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    await assert.rejects(r.seed(bare, CHILD, anchor), /cannot be safely adopted/);
  } finally { r.fx.cleanup(); }
});

it("task recovery preserves exact old history when default advances without a competing branch", async () => {
  const r = rig();
  try {
    const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
    const anchor = commit(r.fx.originPath, "published-anchor");
    cmd(r.fx.originPath, "update-ref", REF, anchor);
    cmd(r.fx.originPath, "reset", "--hard", base);
    commit(r.fx.originPath, "default-advanced");
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    const clone = await r.cache.runnerCloneForBranch(bare, BRANCH, "fresh-claim", noProofReseed, CHILD,
      false, anchor, undefined, { checkpointKey: CHILD_KEY, checkpointWriteUnresolved: false });
    assert.equal(cmd(clone.path, "rev-parse", "HEAD"), anchor);
    assert.equal(fs.existsSync(path.join(clone.path, "default-advanced")), false);
  } finally { r.fx.cleanup(); }
});

for (const state of [true, undefined]) it("a verified exact source capture can recover write state " + state, async () => {
  const r = rig();
  try {
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    const clone = await r.seed(bare);
    const source = { runId: CHILD, clonePath: clone.path };
    await r.cache.markRecoveryCapture(bare, clone.path, BRANCH, CHILD);
    const reservation = await r.cache.reserveRecoveryIteration(bare, BRANCH, "task-" + CHILD, source);
    const tip = commit(clone.path, "captured-work");
    await fixtureFetchTracking(r.cache, bare, clone.path, BRANCH, CHILD);
    await r.cache.recordRecoveryCapture(bare, BRANCH, "task-" + CHILD, source, reservation.attempts, tip, tip);
    const successor = await r.cache.prepareRecoverySuccessor(bare, BRANCH, "task-" + CHILD, source,
      reservation.attempts, "20261023T171401Z-g2-0123456789abcdef",
      { checkpointKey: CHILD_KEY, checkpointWriteUnresolved: state });
    assert.equal(cmd(successor.path, "rev-parse", "HEAD"), tip);
    assert.equal(fs.readFileSync(path.join(successor.path, "captured-work"), "utf8"), "captured-work");
  } finally { r.fx.cleanup(); }
});

it("held task publication scans new secrets beyond the branch and confirmed key floors, and guards workflows", async () => {
  const fx = makeFixture();
  try {
    const scanner = writeGitleaksShim(fx.dataDir, "detect");
    const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: scanner }));
    const base = cmd(fx.originPath, "rev-parse", "HEAD");
    const published = commit(fx.originPath, "published", "already published", "ghp_" + "a".repeat(36));
    cmd(fx.originPath, "branch", BRANCH);
    const confirmed = commit(fx.originPath, "confirmed");
    cmd(fx.originPath, "update-ref", REF, confirmed);
    cmd(fx.originPath, "reset", "--hard", base);
    const bare = await cache.ensureClone(fx.originPath, undefined, undefined, CHILD_KEY);
    const clone = await cache.runnerCloneForBranch(bare, BRANCH, "held", noProofReseed, CHILD, true,
      confirmed, undefined, { checkpointKey: CHILD_KEY, checkpointWriteUnresolved: false });
    const policy = { version: 1 as const, blocked: false, everKnown: false, flaggedCommits: [],
      ciGlobs: null, confirmedTip: confirmed, checkpointKey: CHILD_KEY, extraFloors: [] };
    const clean = commit(clone.path, "clean");
    await fixtureFetchTracking(cache, bare, clone.path, BRANCH, CHILD);
    const good = await prepareFrozenHeld({ git: cache, barePath: bare, branch: BRANCH, sourceSha: clean,
      policy, deadlineMs: 30_000 });
    assert.ok("pack" in good, "an already published secret stays below the branch floor");
    const calls = shimCalls(scanner).filter(c => c.startsWith("git "));
    assert.ok(calls.some(c => c.includes("^" + published) && c.includes("^" + confirmed)),
      "the actual scanner receives both authorized content floors");
    const dirty = commit(clone.path, "new-secret", "new secret", "ghp_" + "b".repeat(36));
    await fixtureFetchTracking(cache, bare, clone.path, BRANCH, CHILD);
    assert.deepEqual(await prepareFrozenHeld({ git: cache, barePath: bare, branch: BRANCH, sourceSha: dirty,
      policy, deadlineMs: 30_000 }), { reason: "secret_found" });
    cmd(clone.path, "reset", "--hard", clean);
    const workflow = commit(clone.path, ".github/workflows/ci.yml");
    await fixtureFetchTracking(cache, bare, clone.path, BRANCH, CHILD);
    assert.deepEqual(await prepareFrozenHeld({ git: cache, barePath: bare, branch: BRANCH, sourceSha: workflow,
      policy, deadlineMs: 30_000 }), { reason: "workflow_change" });
    cmd(clone.path, "reset", "--hard", clean);
    const ci = commit(clone.path, ".gitlab-ci.yml");
    await fixtureFetchTracking(cache, bare, clone.path, BRANCH, CHILD);
    assert.deepEqual(await prepareFrozenHeld({ git: cache, barePath: bare, branch: BRANCH, sourceSha: ci,
      policy: { ...policy, ciGlobs: [".gitlab-ci.yml"] }, deadlineMs: 30_000 }), { reason: "ci_config_guard" });
  } finally { fx.cleanup(); }
});

it("fixture Git ignores inherited repository, index, object and config overrides", () => {
  const r = rig();
  try {
    const inherited: NodeJS.ProcessEnv = { ...process.env, GIT_DIR: path.join(r.fx.dataDir, "foreign.git"),
      GIT_WORK_TREE: r.fx.dataDir, GIT_INDEX_FILE: path.join(r.fx.dataDir, "foreign-index"),
      GIT_OBJECT_DIRECTORY: path.join(r.fx.dataDir, "foreign-objects"), GIT_CONFIG_COUNT: "1",
      GIT_CONFIG_KEY_0: "core.bare", GIT_CONFIG_VALUE_0: "true" };
    const env = fixtureGitEnv(inherited);
    assert.equal(env.PATH, inherited.PATH);
    assert.equal(env.TMPDIR, inherited.TMPDIR);
    assert.deepEqual(Object.keys(env).filter(key => key.startsWith("GIT_")).sort(),
      ["GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_TERMINAL_PROMPT"]);
    // Read-only: prove repository selection before any Git write with this environment.
    const selected = execFileSync("git", ["-C", r.fx.originPath, "rev-parse", "--show-toplevel"],
      { env, encoding: "utf8", stdio: "pipe" }).trim();
    assert.equal(fs.realpathSync(selected), fs.realpathSync(r.fx.originPath));
    assert.equal(inherited.GIT_DIR, path.join(r.fx.dataDir, "foreign.git"));
  } finally { r.fx.cleanup(); }
});

it("evicting an old task freshness proof refuses cold recovery despite a mirrored exact ref", async () => {
  const r = rig();
  try {
    const base = cmd(r.fx.originPath, "rev-parse", "HEAD");
    cmd(r.fx.originPath, "update-ref", REF, base);
    const bare = await r.cache.ensureClone(r.fx.originPath, undefined, undefined, CHILD_KEY);
    // Populate intervening preparations without 255 network/Git preparations. Exercise
    // eviction through ensureClone and recovery through the public clone boundary.
    const proofs = (r.cache as unknown as { taskCheckpointFreshness: Map<string, string | null> }).taskCheckpointFreshness;
    for (let i = 0; i < 255; i++) proofs.set("intervening-" + i, null);
    await r.cache.ensureClone(r.fx.originPath, undefined, undefined, BRANCH);
    assert.equal(proofs.size, 256);
    assert.equal(cmd(bare, "rev-parse", REF), base, "the mirrored ref still exists");
    await assert.rejects(r.seed(bare, CHILD, base), /freshness unavailable/);
    const root = await r.seed(bare, ROOT);
    assert.equal(cmd(root.path, "rev-parse", "HEAD"), base, "the newest absence proof remains usable");
  } finally { r.fx.cleanup(); }
});
