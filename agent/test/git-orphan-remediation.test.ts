import { afterEach, beforeEach, describe, test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { GitCache, RemediationRecoveryRetainedError, type FetchAgentBranchOptions, type RecoveryJournalEntry } from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import type { AuthoritySettlement, GitAuthorityHooks } from "../src/git-authority-hooks.js";
import type { CompletionSourceFence } from "../src/completion-source.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { legacyProtectedDetach } from "./legacy-protected-detach.js";

// issue #4058 M1: the git-layer classification of a terminal owner's journal that carries an active
// remediation recovery record, and the typed refusal of every release, retirement or detach of it.

const BRANCH = "agent/issue-4058";
const RUN = "run-4058";
const KEY = "issue-4058";
const ATTEMPT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"];
const FENCE: CompletionSourceFence = { version: 1, sourceUseEpoch: "0123abcd-0123-4abc-8def-0123456789ab", sourceCoreDigest: "a".repeat(64) };
let fx: Fixture;
let cache: GitCache;
let strict: GitCache;
let bare: string;
let clone: string;
let base: string;
let opts: FetchAgentBranchOptions;
const source = () => ({ runId: RUN, clonePath: clone, attemptId: ATTEMPT });

function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
function authority(): GitAuthorityHooks {
  const settlement: AuthoritySettlement = { settle: async () => {} };
  return {
    admitSourceUse: async () => {},
    beginBareMutation: async () => settlement,
    assertBareClear: async () => {},
    beginDestructive: async () => settlement,
  };
}
const rawJournal = () => git(bare, "config", "--get", "uzi-recovery." + BRANCH + ".clone");
const journal = (): Record<string, unknown> => JSON.parse(rawJournal()) as Record<string, unknown>;
const writeJournal = (value: unknown) => git(bare, "config", "uzi-recovery." + BRANCH + ".clone", JSON.stringify(value));
/** Every byte of the bare config: the journal, the attempt ledger, attempt-mode markers and descriptors. */
const configBytes = () => fs.readFileSync(path.join(bare, "config"));

async function enroll() {
  const tip = git(clone, "rev-parse", "HEAD");
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip, flaggedOIDs: [tip], origin: "pre-exit" });
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  strict = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: authority(), authorityRequired: true }));
  bare = await cache.ensureClone(fx.originPath);
  base = git(bare, "rev-parse", "refs/remotes/origin/main");
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, KEY, noProofReseed, RUN, false, undefined,
    { attemptId: ATTEMPT, isLive: () => false, beforeSeed: async () => {} })).path;
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  opts = { context: { runId: RUN, generation: 1, kind: "issue", branch: BRANCH, barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } }, producingSource: source() };
  const flagged = git(clone, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "flagged");
  git(clone, "update-ref", "refs/heads/" + BRANCH, flagged);
  const result = await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts);
  assert.equal(result.kind, "updated", JSON.stringify(result));
});
afterEach(() => fx.cleanup());

describe("readOrphanJournalFence remediation classification", () => {
  test("no remediation record: the key is absent on both result shapes", async () => {
    assert.deepEqual(await cache.readOrphanJournalFence(bare, BRANCH, RUN, clone), { state: "unfenced", retained: false });
    writeJournal({ ...journal(), completionSourceFence: FENCE });
    const fenced = await strict.readOrphanJournalFence(bare, BRANCH, RUN, clone);
    assert.equal(fenced.state, "fenced");
    assert.equal("remediation" in fenced, false);
  });

  test("an exact record is reported for authority and non-authority caches, read-only", async () => {
    await enroll();
    const before = configBytes();
    assert.deepEqual(await cache.readOrphanJournalFence(bare, BRANCH, RUN, clone), { state: "unfenced", retained: false, remediation: "exact" });
    assert.deepEqual(await strict.readOrphanJournalFence(bare, BRANCH, RUN, clone), { state: "unfenced", retained: false, remediation: "exact" });
    assert.deepEqual(configBytes(), before);
  });

  test("a fenced journal reports the exact record on the authority cache and unfenced on the other", async () => {
    await enroll();
    writeJournal({ ...journal(), completionSourceFence: FENCE });
    const fenced = await strict.readOrphanJournalFence(bare, BRANCH, RUN, clone);
    assert.equal(fenced.state, "fenced");
    assert.equal(fenced.state === "fenced" && fenced.remediation, "exact");
    assert.deepEqual(await cache.readOrphanJournalFence(bare, BRANCH, RUN, clone), { state: "unfenced", retained: false, remediation: "exact" });
  });

  test("a retained journal still reports the exact record", async () => {
    await enroll();
    await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source());
    const got = await cache.readOrphanJournalFence(bare, BRANCH, RUN, clone);
    assert.deepEqual(got, { state: "unfenced", retained: true, remediation: "exact" });
  });

  test("a schema-valid record for another branch is a mismatch, never exact", async () => {
    await enroll();
    const j = journal() as { remediationRecovery: Record<string, unknown> };
    writeJournal({ ...j, remediationRecovery: { ...j.remediationRecovery, branch: "agent/issue-9999" } });
    assert.deepEqual(await cache.readOrphanJournalFence(bare, BRANCH, RUN, clone), { state: "unfenced", retained: false, remediation: "mismatch" });
    assert.deepEqual(await strict.readOrphanJournalFence(bare, BRANCH, RUN, clone), { state: "unfenced", retained: false, remediation: "mismatch" });
  });

  test("a malformed record still throws, and another run or path is changed", async () => {
    await enroll();
    assert.deepEqual(await cache.readOrphanJournalFence(bare, BRANCH, "run-other", clone), { state: "changed" });
    assert.deepEqual(await cache.readOrphanJournalFence(bare, BRANCH, RUN, clone + "-x"), { state: "changed" });
    const j = journal() as { remediationRecovery: Record<string, unknown> };
    writeJournal({ ...j, remediationRecovery: { ...j.remediationRecovery, attemptId: "other" } });
    await assert.rejects(cache.readOrphanJournalFence(bare, BRANCH, RUN, clone), /^Error: invalid remediation recovery$/);
    writeJournal({ ...j, remediationRecovery: { ...j.remediationRecovery, unexpected: 1 } });
    await assert.rejects(cache.readOrphanJournalFence(bare, BRANCH, RUN, clone), /remediation/);
  });
});

describe("typed refusal of every release, retirement and detach", () => {
  test("releaseAttemptInPlace rejects with the typed error and changes nothing", async () => {
    await enroll();
    const before = configBytes();
    const err = await cache.releaseAttemptInPlace(bare, clone, BRANCH, RUN, "abandoned").then(() => undefined, (e: unknown) => e);
    assert.ok(err instanceof RemediationRecoveryRetainedError);
    assert.equal((err as Error).message, "remediation recovery rebind requires proof");
    assert.deepEqual(configBytes(), before);
  });

  test("retireRunnerClone rejects with the typed error and changes nothing", async () => {
    await enroll();
    const before = configBytes();
    const err = await cache.retireRunnerClone(bare, clone, BRANCH, RUN, { discard: true, attemptId: ATTEMPT }).then(() => undefined, (e: unknown) => e);
    assert.ok(err instanceof RemediationRecoveryRetainedError);
    assert.equal((err as Error).message, "remediation recovery retirement requires proof");
    assert.deepEqual(configBytes(), before);
    assert.ok(fs.existsSync(clone));
  });

  test("detachTerminalRetained rejects with the typed error when the snapshot carries the record", async () => {
    await enroll();
    const before = configBytes();
    const err = await cache.detachTerminalRetained(bare, BRANCH, KEY, journal() as unknown as RecoveryJournalEntry).then(() => undefined, (e: unknown) => e);
    assert.ok(err instanceof RemediationRecoveryRetainedError);
    assert.equal((err as Error).message, "remediation recovery detach requires proof");
    assert.deepEqual(configBytes(), before);
  });

  test("detachTerminalRetained refuses a record enrolled after the snapshot, before any write", async () => {
    const stale = journal() as unknown as RecoveryJournalEntry;
    await enroll();
    const before = configBytes();
    const err = await cache.detachTerminalRetained(bare, BRANCH, KEY, stale).then(() => undefined, (e: unknown) => e);
    assert.ok(err instanceof RemediationRecoveryRetainedError);
    assert.equal((err as Error).message, "remediation recovery detach requires proof");
    assert.deepEqual(configBytes(), before);
  });

  test("detachTerminalRetained refuses on the expected record alone when a prior descriptor exists and the journal is cleared", async () => {
    await enroll();
    await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source());
    const expected = journal() as unknown as RecoveryJournalEntry;
    // Legacy (pre-#4058) protected descriptor carrying the record; the active journal is then cleared.
    await legacyProtectedDetach(cache, bare, BRANCH, KEY, expected);
    assert.equal(rawJournal(), "");
    const before = configBytes();
    const err = await cache.detachTerminalRetained(bare, BRANCH, KEY, expected).then(() => undefined, (e: unknown) => e);
    assert.ok(err instanceof RemediationRecoveryRetainedError);
    assert.equal((err as Error).message, "remediation recovery detach requires proof");
    assert.deepEqual(configBytes(), before);
  });

  test("without a record a retained journal still detaches", async () => {
    await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source());
    await cache.detachTerminalRetained(bare, BRANCH, KEY, journal() as unknown as RecoveryJournalEntry);
    assert.equal(rawJournal(), "");
  });
});

describe("pin: discardProtectedSource on a journal carrying a remediation record", () => {
  // This pins OBSERVED behaviour, not authorization. ADR-1296 refuses enrolled retirement and no owner route
  // is granted by this test; it only records what the existing protected discard does today.
  test("observed outcome", async () => {
    await enroll();
    await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source());
    assert.ok(journal().remediationRecovery, "the journal carries both the episode and the remediation record");
    const selector = { runId: RUN, generation: 1, attempt: ATTEMPT };
    const holds = { state: "read" as const, holds: [] };
    const inspected = await cache.inspectRetainedSources({ holds, isLivePath: () => false, barePath: bare });

    const found = inspected.sources[0];
    assert.ok(found, JSON.stringify(inspected));
    assert.equal(found.kind, "protected");
    const result = await cache.discardProtectedSource({
      selector, confirm: found.descriptor, holds, isLivePath: () => false, quiesce: async () => true, barePath: bare,
    });
    // Observed: the protected discard removes the source and clears the journal, remediation record included.
    assert.equal(result.kind, "done", JSON.stringify(result));
    assert.equal(result.kind === "done" && result.metadata, "retired");
    assert.equal(rawJournal(), "");
    assert.equal(fs.existsSync(clone), false);
  });
});
