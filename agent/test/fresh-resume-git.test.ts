import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { FreshConfigLockedError, FreshSeedError, GitCache, freshLocalState, parseAttemptLedgerEntry, type FreshSeedArtifact } from "../src/git.js";
import { InventorySourceExclusionRefusal } from "../src/inventory-exclusion.js";
import { FreshNoAuthority, commitFreshReceipt } from "../src/fresh-resume.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { freshJournalKey, parseFreshJournal } from "../src/fresh-journal.js";
import type { FreshResumeState } from "../src/client.js";

// issue #4628: the destination's git side of a fresh resume, against real git and file:// fixtures. The seed (L0 to L2), the receipt
// commit (L4), the fresh normalization of the seeded base, and the two refusals that must leave things untouched.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const BRANCH = "agent/issue-77";
const KEY = "issue-77";
const RUN = "run-fresh-1";
const GENERATION = 2;

let fx: Fixture;
let cache: GitCache;
let bare: string;

function sh(dir: string, args: string[], input?: string): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV, ...(input === undefined ? {} : { input }), stdio: ["pipe", "pipe", "pipe"] }).trim();
}

/** Commit `files` on a new branch off `parent` in the fixture origin and return the commit. Origin stays on main. */
function commitOn(name: string, parent: string, files: Record<string, string>, subject: string): string {
  sh(fx.originPath, ["checkout", "-q", "-B", name, parent]);
  for (const [rel, content] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(fx.originPath, rel)), { recursive: true });
    fs.writeFileSync(path.join(fx.originPath, rel), content);
  }
  sh(fx.originPath, ["add", "-A"]);
  sh(fx.originPath, ["commit", "-q", "-m", subject]);
  const sha = sh(fx.originPath, ["rev-parse", "HEAD"]);
  sh(fx.originPath, ["checkout", "-q", "main"]);
  return sha;
}

/** A transport overlay as checkpointPack builds it: base first, real tip last, differing from the real tip only under workflows. */
function overlayOf(real: string, base: string, subject: string, extra: Record<string, string> = {}): string {
  sh(fx.originPath, ["checkout", "-q", "--detach", real]);
  fs.mkdirSync(path.join(fx.originPath, ".github", "workflows"), { recursive: true });
  fs.writeFileSync(path.join(fx.originPath, ".github", "workflows", "ci.yml"), "name: swapped\n");
  for (const [rel, content] of Object.entries(extra)) fs.writeFileSync(path.join(fx.originPath, rel), content);
  sh(fx.originPath, ["add", "-A"]);
  const tree = sh(fx.originPath, ["write-tree"]);
  const sha = sh(fx.originPath, ["-c", "user.email=t@t", "-c", "user.name=t", "commit-tree", tree, "-p", base, "-p", real, "-m", subject]);
  sh(fx.originPath, ["reset", "-q", "--hard"]);
  sh(fx.originPath, ["checkout", "-q", "main"]);
  sh(fx.originPath, ["branch", "-f", `ov-${sha.slice(0, 8)}`, sha]);
  return sha;
}

function mainTip(): string {
  return sh(fx.originPath, ["rev-parse", "main"]);
}

function ledger(): string[] {
  try { return sh(bare, ["config", "--local", "--get-all", `uzi-attempts.${BRANCH}.entry`]).split("\n").filter(Boolean); } catch { return []; }
}

function configValue(key: string): string | undefined {
  try { return sh(bare, ["config", "--local", "--get-all", key]); } catch { return undefined; }
}

async function freshBare(): Promise<void> {
  bare = await cache.ensureClone(fx.originPath);
}

function artifact(kind: "capture" | "checkpoint_ref", sha: string): { frid: string; artifact: FreshSeedArtifact } {
  const frid = randomUUID();
  const artifactRef = kind === "capture" ? `refs/uzi-fresh/${frid}` : `refs/uzi-checkpoints/${BRANCH}`;
  sh(bare, ["update-ref", artifactRef, sha]);
  return { frid, artifact: { kind, artifactRef, artifactSha: sha } };
}

function seedInput(frid: string, art: FreshSeedArtifact, over: Partial<Parameters<GitCache["seedFreshAttempt"]>[0]> = {}) {
  const attemptId = mintAttemptId(GENERATION);
  return {
    barePath: bare, branch: BRANCH, key: KEY, runId: RUN, attemptId, frid, generation: GENERATION, artifact: art,
    isLive: () => false, beforeSeed: async () => {}, ...over,
  };
}

const authority = (current: boolean): FreshResumeState => ({ state: "seeded", seeded_at: "2026-01-01T00:00:00Z", destination_attempt_id: null, seeded_generation: GENERATION, current_authority: current });

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  await freshBare();
});

afterEach(() => fx.cleanup());

describe("fresh resume seed: L0 to L2 and the receipt commit (L4)", () => {
  it("writes the journal (seeding), the pending ledger entry and the seed; L4 renames them into the live shape in one commit", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", sha);
    const input = seedInput(frid, art);
    const seeded = await cache.seedFreshAttempt(input);

    assert.equal(seeded.seededFrom, "fresh_resume");
    assert.equal(seeded.attemptId, input.attemptId);
    assert.deepEqual(seeded.freshResume, { artifactSha: sha, executionBase: sha, peeledOverlay: false, wipRecovered: false });
    assert.equal(fs.readFileSync(path.join(seeded.path, "a.txt"), "utf8"), "a\n");
    assert.match(input.attemptId, /-g2-/, "A carries G'");

    const journal = parseFreshJournal(configValue(freshJournalKey(BRANCH))!);
    assert.deepEqual(journal, { frid, attemptId: input.attemptId, generation: GENERATION, sha, stage: "seeding" });
    assert.equal(parseAttemptLedgerEntry(ledger().at(-1)!)?.state, "fresh_pending");
    assert.equal(configValue(`uzi-recovery.${BRANCH}.clone`), undefined, "the recovery journal is the receipt commit's, not the seed's");

    const commit = { barePath: bare, branch: BRANCH, runId: RUN, clonePath: seeded.path, attemptId: input.attemptId, frid, generation: GENERATION, sha };
    assert.equal(await commitFreshReceipt({ git: cache }, authority(true), commit), "committed");
    assert.deepEqual(JSON.parse(configValue(`uzi-recovery.${BRANCH}.clone`)!), { runId: RUN, clonePath: seeded.path, attemptId: input.attemptId });
    assert.equal(parseAttemptLedgerEntry(ledger().at(-1)!)?.state, "live");
    assert.equal(parseFreshJournal(configValue(freshJournalKey(BRANCH))!).stage, "ready");
    assert.equal(await commitFreshReceipt({ git: cache }, authority(true), commit), "already", "a replayed commit is a no-op");
    assert.equal(ledger().length, 2, "no extra ledger value on the replay");
  });

  it("without current authority there is no L4: the receipt commit throws and writes nothing", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", sha);
    const input = seedInput(frid, art);
    const seeded = await cache.seedFreshAttempt(input);
    const before = sh(bare, ["config", "--local", "--list"]);
    await assert.rejects(commitFreshReceipt({ git: cache }, authority(false),
      { barePath: bare, branch: BRANCH, runId: RUN, clonePath: seeded.path, attemptId: input.attemptId, frid, generation: GENERATION, sha }), FreshNoAuthority);
    assert.equal(sh(bare, ["config", "--local", "--list"]), before);
  });

  it("refuses to seed on top of any local state of the branch, and writes nothing", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", sha);
    sh(bare, ["config", "--local", `uzi-recovery.${BRANCH}.clone`, JSON.stringify({ runId: "other", clonePath: "/data/runner/x/issue-77" })]);
    const before = sh(bare, ["config", "--local", "--list"]);
    await assert.rejects(cache.seedFreshAttempt(seedInput(frid, art)), (err: unknown) => err instanceof FreshSeedError && err.reason === "local_state_present");
    assert.equal(sh(bare, ["config", "--local", "--list"]), before);
    assert.deepEqual(freshLocalState(before.split("\n").map(l => l.replace("=", "\n")), BRANCH, RUN), ["recovery_journal"]);
  });

  it("refuses an artifact ref that does not equal the server sha", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    const other = commitOn("work2", mainTip(), { "b.txt": "b\n" }, "work b");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", other);
    await assert.rejects(cache.seedFreshAttempt(seedInput(frid, { ...art, artifactSha: sha })),
      (err: unknown) => err instanceof FreshSeedError && err.reason === "artifact_ref_mismatch");
    assert.deepEqual(ledger(), []);
  });

  it("a seed that fails after L1 leaves the attempt fresh_pending (the caller marks it unbound only after a closed abandon) and keeps the journal", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", sha);
    const input = seedInput(frid, art, { beforeSeed: async () => { throw new Error("sweep refused"); } });
    await assert.rejects(cache.seedFreshAttempt(input), (err: unknown) => err instanceof FreshSeedError && err.reason === "seed_failed");
    assert.equal(parseAttemptLedgerEntry(ledger().at(-1)!)?.state, "fresh_pending");
    assert.equal(parseFreshJournal(configValue(freshJournalKey(BRANCH))!).stage, "seeding");
    // The caller's closed abandon is what moves it.
    assert.equal(await cache.markFreshUnbound(input), true);
    assert.equal(parseAttemptLedgerEntry(ledger().at(-1)!)?.state, "fresh_unbound");
  });
});

describe("fresh resume seed: a retained inventory exclusion on the key (issue #4643)", () => {
  let verdict: "clear" | "not_clear";
  const canonical = (): string => cache.runnerClonePath(bare, KEY);

  /** A section that started git and found the clone quiescent, with a probe that does not clear, leaves an exclusion entry. */
  async function installExclusion(): Promise<void> {
    verdict = "not_clear";
    cache.setInventoryExclusionProbe(async () => verdict);
    const r = await cache.withInventorySourceLock(bare, { clonePath: canonical(), branch: BRANCH, isLive: () => false }, async s => {
      s.markGitStarted();
      await s.stage(async () => 1);
      s.afterScanQuiescent(true);
    });
    assert.equal(r.kind, "ran");
    assert.equal(r.kind === "ran" && r.entryKept, true, "the fixture really left an exclusion entry");
  }

  it("refuses typed before any write: no clone path, journal, ledger entry or ref", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", sha);
    await installExclusion();
    const input = seedInput(frid, art);
    const refsBefore = sh(bare, ["for-each-ref"]);
    const configBefore = sh(bare, ["config", "--local", "--list"]);
    await assert.rejects(cache.seedFreshAttempt(input), (err: unknown) =>
      err instanceof InventorySourceExclusionRefusal && !(err instanceof FreshSeedError) && err.reason === "excluded");
    assert.equal(fs.existsSync(`${canonical()}.attempt-${input.attemptId}`), false, "A's attempt clone path is absent");
    assert.equal(configValue(freshJournalKey(BRANCH)), undefined, "no L0 journal");
    assert.deepEqual(ledger(), [], "no L1 ledger entry");
    assert.equal(sh(bare, ["for-each-ref"]), refsBefore, "no ref was created or moved in the bare");
    assert.equal(sh(bare, ["config", "--local", "--list"]), configBefore, "no config key was written");
  });

  it("positive control: the same setup with a clearing probe seeds", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", sha);
    await installExclusion();
    verdict = "clear";
    const seeded = await cache.seedFreshAttempt(seedInput(frid, art));
    assert.equal(seeded.seededFrom, "fresh_resume");
    assert.equal(parseAttemptLedgerEntry(ledger().at(-1)!)?.state, "fresh_pending");
  });
});

describe("fresh resume seed: the bare lock excludes a same-key inventory writer through L2", () => {
  it("a concurrent markRecoveryCapture waits for the seed and runs only after it", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", sha);
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let atSweep!: () => void;
    const reached = new Promise<void>(resolve => { atSweep = resolve; });
    const input = seedInput(frid, art, { beforeSeed: async () => { atSweep(); await gate; } });
    const seeding = cache.seedFreshAttempt(input);
    await reached;
    // L2 holds the bare lock here. A writer of the same key must not get in.
    let writerDone = false;
    const otherClone = path.join(path.dirname(input.barePath), "..", "runner", "x", `${KEY}.attempt-${mintAttemptId(3)}`);
    const writer = cache.markRecoveryCapture(bare, path.resolve(otherClone), BRANCH, "run-other").then(() => { writerDone = true; }, () => { writerDone = true; });
    await new Promise(resolve => setTimeout(resolve, 150));
    assert.equal(writerDone, false, "the inventory writer is excluded while L2 holds the bare lock");
    assert.equal(configValue(`uzi-recovery.${BRANCH}.clone`), undefined, "and nothing of it is visible yet");
    release();
    await seeding;
    await writer;
    assert.equal(writerDone, true, "it proceeds once L2 released the lock");
  });
});

describe("fresh resume: a stale config.lock is never removed and never retried", () => {
  it("L0 meets it: nothing is written, the lock is untouched, the failure is typed", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", sha);
    const lock = path.join(bare, "config.lock");
    fs.writeFileSync(lock, "stale\n");
    const before = fs.readFileSync(path.join(bare, "config"), "utf8");
    await assert.rejects(cache.seedFreshAttempt(seedInput(frid, art)), FreshConfigLockedError);
    assert.equal(fs.readFileSync(lock, "utf8"), "stale\n", "the lock was not removed or rewritten");
    assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), before);
  });

  it("L4 meets it: the lock is untouched and the journal stays at seeding", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact("capture", sha);
    const input = seedInput(frid, art);
    const seeded = await cache.seedFreshAttempt(input);
    const lock = path.join(bare, "config.lock");
    fs.writeFileSync(lock, "stale\n");
    await assert.rejects(commitFreshReceipt({ git: cache }, authority(true),
      { barePath: bare, branch: BRANCH, runId: RUN, clonePath: seeded.path, attemptId: input.attemptId, frid, generation: GENERATION, sha }), FreshConfigLockedError);
    assert.equal(fs.readFileSync(lock, "utf8"), "stale\n");
    assert.equal(parseFreshJournal(configValue(freshJournalKey(BRANCH))!).stage, "seeding");
    assert.equal(configValue(`uzi-recovery.${BRANCH}.clone`), undefined);
  });
});

describe("fresh resume normalization of the seeded base", () => {
  async function seedFrom(kind: "capture" | "checkpoint_ref", sha: string) {
    await cache.ensureClone(fx.originPath);
    const { frid, artifact: art } = artifact(kind, sha);
    return cache.seedFreshAttempt(seedInput(frid, art));
  }

  it("a plain tip is the execution base as it is", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    const seeded = await seedFrom("checkpoint_ref", sha);
    assert.deepEqual(seeded.freshResume, { artifactSha: sha, executionBase: sha, peeledOverlay: false, wipRecovered: false });
    assert.equal(sh(seeded.path, ["rev-parse", "HEAD"]), sha);
    assert.equal(seeded.priorCommits, 1);
  });

  it("a genuine overlay on a checkpoint ref is peeled to its real tip, the artifact sha staying the overlay", async () => {
    const real = commitOn("real", mainTip(), { "a.txt": "a\n" }, "real work");
    const overlay = overlayOf(real, mainTip(), "ckpt(overlay): transport");
    const seeded = await seedFrom("checkpoint_ref", overlay);
    assert.deepEqual(seeded.freshResume, { artifactSha: overlay, executionBase: real, peeledOverlay: true, wipRecovered: false });
    assert.equal(sh(seeded.path, ["rev-parse", "HEAD"]), real);
    assert.equal(fs.existsSync(path.join(seeded.path, ".github", "workflows", "ci.yml")), false, "the swapped workflow tree is not branch content");
  });

  it("a forged overlay on a checkpoint ref (real files differ) is not peeled", async () => {
    const real = commitOn("real", mainTip(), { "a.txt": "a\n" }, "real work");
    const forged = overlayOf(real, mainTip(), "ckpt(overlay): looks like transport", { "a.txt": "changed work\n" });
    const seeded = await seedFrom("checkpoint_ref", forged);
    assert.equal(seeded.freshResume?.peeledOverlay, false);
    assert.equal(sh(seeded.path, ["rev-parse", "HEAD"]), forged);
  });

  it("an overlay subject on a capture is a forgery and is never peeled, even when its diff is workflow-only", async () => {
    const real = commitOn("real", mainTip(), { "a.txt": "a\n" }, "real work");
    const overlay = overlayOf(real, mainTip(), "ckpt(overlay): transport");
    const seeded = await seedFrom("capture", overlay);
    assert.deepEqual(seeded.freshResume, { artifactSha: overlay, executionBase: overlay, peeledOverlay: false, wipRecovered: false });
    assert.equal(sh(seeded.path, ["rev-parse", "HEAD"]), overlay);
  });

  for (const kind of ["capture", "checkpoint_ref"] as const) {
    it(`a wip(park) marker on a ${kind} is reset to uncommitted work on its parent`, async () => {
      const real = commitOn("real", mainTip(), { "a.txt": "a\n" }, "real work");
      const marker = commitOn("park", real, { "wip.txt": "unfinished\n" }, "wip(park): auto-save");
      const seeded = await seedFrom(kind, marker);
      assert.deepEqual(seeded.freshResume, { artifactSha: marker, executionBase: real, peeledOverlay: false, wipRecovered: true });
      assert.equal(sh(seeded.path, ["rev-parse", "HEAD"]), real);
      assert.equal(fs.readFileSync(path.join(seeded.path, "wip.txt"), "utf8"), "unfinished\n");
      assert.match(sh(seeded.path, ["status", "--porcelain"]), /wip\.txt/);
    });
  }
});

describe("fresh resume bundle import", () => {
  function bundleOf(sha: string, ref = "recovered-source"): Buffer {
    sh(fx.originPath, ["branch", "-f", ref, sha]);
    const out = path.join(fx.dataDir, `${ref}.bundle`);
    execFileSync("git", ["-C", fx.originPath, "bundle", "create", out, `main..${ref}`], { env: GIT_ENV, stdio: "pipe" });
    return fs.readFileSync(out);
  }

  it("streams, verifies and fetches the bundle into a ref equal to the server sha", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    const bytes = bundleOf(sha);
    const frid = randomUUID();
    const ref = await cache.fetchFreshBundle(bare, { frid, sha }, sink => pipeline(Readable.from([bytes]), sink));
    assert.equal(ref, `refs/uzi-fresh/${frid}`);
    assert.equal(sh(bare, ["rev-parse", ref]), sha);
    assert.deepEqual(fs.readdirSync(bare).filter(n => n.startsWith(".uzi-fresh-")), [], "the temporary bundle file is gone");
  });

  it("a temporary file left by a crashed earlier attempt with the same transfer id does not block the fetch", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    const bytes = bundleOf(sha);
    const frid = randomUUID();
    const stale = path.join(bare, `.uzi-fresh-${frid}.bundle.tmp`);
    fs.writeFileSync(stale, "half a bundle from a crash");
    const ref = await cache.fetchFreshBundle(bare, { frid, sha }, sink => pipeline(Readable.from([bytes]), sink));
    assert.equal(sh(bare, ["rev-parse", ref]), sha);
    assert.deepEqual(fs.readdirSync(bare).filter(n => n.startsWith(".uzi-fresh-")), []);
  });

  it("refuses a bundle whose tip is not the server sha, and leaves no ref", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    const other = commitOn("work2", mainTip(), { "b.txt": "b\n" }, "work b");
    const bytes = bundleOf(other);
    const frid = randomUUID();
    await assert.rejects(cache.fetchFreshBundle(bare, { frid, sha }, sink => pipeline(Readable.from([bytes]), sink)));
    assert.throws(() => sh(bare, ["rev-parse", "--verify", "--quiet", `refs/uzi-fresh/${frid}`]));
  });

  it("refuses bytes that are not a bundle", async () => {
    const sha = commitOn("work", mainTip(), { "a.txt": "a\n" }, "work a");
    const frid = randomUUID();
    await assert.rejects(cache.fetchFreshBundle(bare, { frid, sha }, sink => pipeline(Readable.from([Buffer.from("not a bundle")]), sink)));
    assert.deepEqual(fs.readdirSync(bare).filter(n => n.startsWith(".uzi-fresh-")), []);
  });
});
