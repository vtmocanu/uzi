import { afterEach, beforeEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type GitCacheOptions } from "../src/git.js";
import { managedBareGroupTrustForTest } from "../src/runner-group-trust.js";
import { createHibernationCensus } from "../src/hibernation-census.js";
import type { CensusResult, InventoryEntry } from "../src/hibernation-inventory.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { assertWireEntries, EMPTY_DOCKER, startFakeDocker, type FakeDocker } from "./hibernation-census-wire.js";

// The census's content proof, against real git: a clone whose tracked bytes cannot be proven equal to HEAD
// without trusting the index stat cache must not come out as content_proven (PRD #4208 V18), and bytes the proof
// does not account for (ignored files) are listed (V19).

const RUN = "99999999-9999-4999-8999-999999999991";
const BRANCH = "agent/issue-4208";
const ENV: NodeJS.ProcessEnv = {
  ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))),
  GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0",
};

let fx: Fixture;
let git: GitCache;
let clonePath: string;
let docker: FakeDocker;

const command = (cwd: string, ...args: string[]) =>
  execFileSync("git", ["-C", cwd, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();

function makeScope(): PodEpochReclaimScope {
  let pending = 0, stopped = false;
  return {
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
}

beforeEach(async () => {
  fx = makeFixture({ "dir/inner.txt": "inner\n", "dir/other.txt": "other\n", ".gitignore": "ignored.log\n" });
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const attempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000001");
  const clone = await git.runnerCloneForBranch(bare, BRANCH, "issue-4208", noProofReseed, RUN, false, undefined,
    { attemptId: attempt, isLive: () => false, beforeSeed: async () => {} });
  clonePath = clone.path;
  await git.markRecoveryCapture(bare, clonePath, BRANCH, RUN, attempt);
  docker = await startFakeDocker(EMPTY_DOCKER);
});
afterEach(async () => { await docker?.close(); fx?.cleanup(); });

async function census(): Promise<CensusResult> {
  const scope = makeScope();
  const seams = createHibernationCensus({
    dataDir: fx.dataDir,
    layout: { outbox: "outbox", agentHome: "agent-home", jobs: "jobs" },
    git,
    client: {
      getRunOwnership: async () => ({ status: "completed" }) as never,
      listRecoveryHolds: async (runId: string) => ({ run_id: runId, holds: [], run_open_holds: { run_id: runId, count: 0 } }) as never,
    },
    ownership: { beginReclaim: () => scope },
    dockerHost: docker.host,
  });
  const result = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assertWireEntries(result.entries);
  return result;
}
const attemptEntries = (result: CensusResult): InventoryEntry[] =>
  result.entries.filter(e => e.kind === "retained_attempt");
const rel = () => path.relative(fx.dataDir, clonePath);

it("a clean clone is content_proven and needs the run's receipt for the verified tip", async () => {
  const result = await census();
  const [entry] = attemptEntries(result);
  assert.ok(entry, JSON.stringify(result.entries));
  assert.equal(entry.path, rel());
  assert.equal(entry.classification, "needs_receipt", JSON.stringify(result.entries));
  assert.equal(entry.content_state, "content_proven");
  assert.equal(entry.run_id, RUN);
  assert.equal(entry.tip, command(clonePath, "rev-parse", "HEAD"));
  assert.equal(result.entries.filter(e => e.kind === "worktree").length, 0, "no unaccounted bytes in a clean clone");
  assert.equal(result.complete, true, JSON.stringify(result.uncovered));
});

it("V18: a tracked file modified behind skip-worktree is not content_proven", async () => {
  command(clonePath, "update-index", "--skip-worktree", "dir/inner.txt");
  fs.writeFileSync(path.join(clonePath, "dir/inner.txt"), "tampered\n");
  const [entry] = attemptEntries(await census());
  assert.equal(entry?.content_state, "unproven_worktree_bytes");
  assert.equal(entry?.classification, "unknown");
  assert.equal(entry?.tip, undefined);
});

it("V18: a tracked file modified behind assume-valid is not content_proven", async () => {
  command(clonePath, "update-index", "--assume-unchanged", "dir/inner.txt");
  fs.writeFileSync(path.join(clonePath, "dir/inner.txt"), "tampered\n");
  const [entry] = attemptEntries(await census());
  assert.equal(entry?.content_state, "unproven_worktree_bytes");
  assert.equal(entry?.classification, "unknown");
});

it("V18: a rewrite with the same size, a restored mtime and a stale stat cache is not content_proven", async () => {
  const file = path.join(clonePath, "dir/inner.txt");
  const before = fs.statSync(file);
  fs.writeFileSync(file, "innen\n"); // same length as "inner\n"
  fs.utimesSync(file, before.atime, before.mtime);
  const [entry] = attemptEntries(await census());
  assert.equal(entry?.content_state, "unproven_worktree_bytes");
  assert.equal(entry?.classification, "unknown");
});

it("V19: an ignored file present in a proven clone becomes one unproven_worktree_bytes entry", async () => {
  fs.writeFileSync(path.join(clonePath, "ignored.log"), "left behind\n");
  fs.mkdirSync(path.join(clonePath, "dir/cache"));
  fs.writeFileSync(path.join(clonePath, "dir/cache/blob"), "x");
  // An ignored file that git is told to ignore through the baseline exclude only if the exclude holds it; use .gitignore.
  const result = await census();
  const [entry] = attemptEntries(result);
  // The proof refuses untracked, non-ignored content (dir/cache/blob), so the clone is unproven; remove it and retry.
  assert.equal(entry?.content_state, "unproven_worktree_bytes");
  fs.rmSync(path.join(clonePath, "dir/cache"), { recursive: true });
  const again = await census();
  assert.equal(attemptEntries(again)[0]?.content_state, "content_proven");
  const loose = again.entries.filter(e => e.kind === "worktree");
  assert.equal(loose.length, 1, JSON.stringify(again.entries));
  assert.equal(loose[0]!.path, rel());
  assert.equal(loose[0]!.classification, "unknown");
  assert.equal(loose[0]!.content_state, "unproven_worktree_bytes");
  assert.equal(loose[0]!.detail, "unaccounted_1");
});

it("a missing reclaim scope leaves the clone unproven and the proof unavailable", async () => {
  const seams = createHibernationCensus({
    dataDir: fx.dataDir, layout: { outbox: "outbox", agentHome: "agent-home", jobs: "jobs" }, git,
    client: {
      getRunOwnership: async () => ({ status: "completed" }) as never,
      listRecoveryHolds: async (runId: string) => ({ run_id: runId, holds: [], run_open_holds: { run_id: runId, count: 0 } }) as never,
    },
    ownership: undefined,
  });
  const result = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assert.equal(attemptEntries(result)[0]?.content_state, "unproven_worktree_bytes");
  assert.ok(result.uncovered.includes("content_proof_unavailable"));
});

it("an open custody hold keeps the source protected and skips the proof", async () => {
  const scope = makeScope();
  const seams = createHibernationCensus({
    dataDir: fx.dataDir, layout: { outbox: "outbox", agentHome: "agent-home", jobs: "jobs" }, git,
    client: {
      getRunOwnership: async () => ({ status: "failed" }) as never,
      listRecoveryHolds: async (runId: string) => ({ run_id: runId, holds: [{ hold_id: "h1", generation: 1 }] }) as never,
    },
    ownership: { beginReclaim: () => scope },
  });
  const result = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assertWireEntries(result.entries);
  const [entry] = attemptEntries(result);
  assert.equal(entry?.classification, "protected");
  assert.equal(entry?.tip, undefined);
});

it("a failed lifecycle read makes the source unknown and the census incomplete", async () => {
  const scope = makeScope();
  const seams = createHibernationCensus({
    dataDir: fx.dataDir, layout: { outbox: "outbox", agentHome: "agent-home", jobs: "jobs" }, git,
    client: {
      getRunOwnership: async () => { throw new Error("api down"); },
      listRecoveryHolds: async () => { throw new Error("api down"); },
    },
    ownership: { beginReclaim: () => scope },
  });
  const result = await seams.census({ signal: new AbortController().signal, deadlineMs: 60_000 });
  assert.equal(result.complete, false);
  assert.equal(attemptEntries(result)[0]?.classification, "unknown");
});

it("a .uzi-skills-<clone> sibling with a payload is listed unknown; an empty one is disposable; an absent one lists nothing", async () => {
  const skills = path.join(path.dirname(clonePath), `.uzi-skills-${path.basename(clonePath)}`);
  const skillsRel = path.relative(fx.dataDir, skills);
  assert.equal((await census()).entries.some(e => e.path === skillsRel), false, "absent: nothing listed");
  fs.mkdirSync(skills);
  const emptied = (await census()).entries.find(e => e.path === skillsRel);
  assert.equal(emptied?.classification, "disposable");
  fs.writeFileSync(path.join(skills, "payload"), "bytes that no other entry covers");
  const result = await census();
  const entry = result.entries.find(e => e.path === skillsRel);
  assert.equal(entry?.kind, "unknown_path", JSON.stringify(result.entries));
  assert.equal(entry?.classification, "unknown");
  assert.equal(entry?.detail, "skills_not_empty");
  assert.equal(result.entries.filter(e => e.path === skillsRel).length, 1);
});

// Issue #4486: under the uid split with umask 002 the managed bares are 2775. The census classifies such a clone as
// proven only under an issued runner-group trust and a clean ACL observation; without them it stays unknown.
function groupWritableCache(over: GitCacheOptions): GitCache {
  const status = (uid: number) => ["Name:\tcat", `Uid:\t${uid}\t${uid}\t${uid}\t${uid}`, `Gid:\t${uid}\t${uid}\t${uid}\t${uid}`,
    "Groups:\t10002", "CapInh:\t0000000000000000", "CapPrm:\t0000000000000000", "CapEff:\t0000000000000000",
    "CapBnd:\t00000000000000c0", "CapAmb:\t0000000000000000", "NoNewPrivs:\t1"].join("\n") + "\n";
  const trust = managedBareGroupTrustForTest({ runnerStatus: status(10002), commandStatus: status(10003),
    anyIdentity: true, gid: process.getgid!() });
  return new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
    managedBareGroupTrust: trust, managedBareStatfs: async () => ({ type: 0xef53 }), ...over }));
}
function widenBare(): void {
  const repos = path.join(fx.dataDir, "repos");
  fs.chmodSync(repos, 0o775);
  for (const name of fs.readdirSync(repos)) fs.chmodSync(path.join(repos, name), 0o2775);
}

it("#4486: a 2775 managed-bare clone with trust and a clean ACL observation is content_proven and complete", async () => {
  const observed: string[][] = [];
  git = groupWritableCache({ managedBareAclObserve: async paths => { observed.push(paths); } });
  widenBare();
  const result = await census();
  const [entry] = attemptEntries(result);
  assert.equal(entry?.content_state, "content_proven", JSON.stringify(result.entries));
  assert.equal(entry?.classification, "needs_receipt");
  assert.equal(entry?.tip, command(clonePath, "rev-parse", "HEAD"));
  assert.equal(result.complete, true, JSON.stringify(result.uncovered));
  assert.ok(observed.length >= 2, "the bare tree was observed by every full snapshot");
});

it("#4486: an absent getfacl binary reaches the census as an unknown entry with detail completeness_unknown", async () => {
  const rejections: unknown[] = [];
  const onRej = (e: unknown) => { rejections.push(e); };
  process.on("unhandledRejection", onRej);
  try {
    // No ACL observer seam: the real launch path runs and the nonexistent absolute binary fails with ENOENT.
    git = groupWritableCache({ managedBareGetfaclBin: path.join(fx.dataDir, "no-such-getfacl") });
    widenBare();
    const result = await census();
    const [entry] = attemptEntries(result);
    assert.notEqual(entry?.content_state, "content_proven", JSON.stringify(result.entries));
    assert.equal(entry?.classification, "unknown", JSON.stringify(entry));
    assert.equal(entry?.detail, "completeness_unknown", JSON.stringify(entry));
    assert.equal(entry?.content_state, "unproven_worktree_bytes");
    assert.equal(entry?.tip, undefined);
    // The census lists the unproven clone as an unknown entry (it blocks reclaim by classification); `complete`
    // stays true because the clone is accounted for, as in the observed-ACL case above.
    await new Promise((r) => setImmediate(r));
    assert.deepEqual(rejections, []);
  } finally {
    process.off("unhandledRejection", onRej);
  }
});

it("#4486: the same 2775 clone stays unproven without a trusted group, or with an observed ACL", async () => {
  for (const over of [{ managedBareGroupTrust: undefined, managedBareAclObserve: async () => {} },
    { managedBareAclObserve: async () => { throw new Error("named ACL entry"); } }] as GitCacheOptions[]) {
    git = groupWritableCache(over);
    widenBare();
    const [entry] = attemptEntries(await census());
    assert.notEqual(entry?.content_state, "content_proven");
    assert.equal(entry?.classification, "unknown");
    assert.equal(entry?.tip, undefined);
  }
});
