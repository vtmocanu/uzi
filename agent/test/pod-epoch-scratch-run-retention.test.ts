// Issue #4420 m2: the read-only run-retention classifier behind the scratch phase's `run-<uuid>` leaves. Every
// source has an active case, an ended case, an observed-absent case and an unreadable case; absence and
// unreadable are distinct (absent releases that source, unreadable keeps). Bare-config sources run against a real
// bare repository (GitCache.classifyEpochScratchRunRetention); the pure judgements and dataDir records are driven
// directly.
import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "node:test";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochScratchRunRetentionOptions } from "../src/git.js";
import {
  ENDED, classifyCredentialSwitchText, classifyRunDataDirRecords, judgeAuthoritySlot,
  judgeRetainedEvidence, judgeRunRefListing, normalizeRetention, type RetainedEvidenceShape,
} from "../src/pod-epoch-run-retention.js";
import { RecoveryMaintenance } from "../src/recovery-maintenance.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "11111111-2222-4333-8444-555555555555";
const OTHER = "99999999-8888-4777-8666-555555555555";
const BRANCH = "agent/issue-4420";
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };

let fx: Fixture;
let git: GitCache;
let bare: string;
let attempt: string;
let clonePath: string;

const gitIn = (args: string[]): string => execFileSync("git", ["-C", bare, ...args], { env: ENV, encoding: "utf8" }).trim();
const cfg = (key: string, value: string): void => { gitIn(["config", "--local", "--add", key, value]); };
const dataPath = (...parts: string[]): string => path.join(fx.dataDir, ...parts);
const ledger = (state: string, runId = RUN, clone = clonePath, id = attempt): string => JSON.stringify({ attemptId: id, runId, clonePath: clone, state });

function options(over: Partial<EpochScratchRunRetentionOptions> = {}): EpochScratchRunRetentionOptions {
  return {
    lifecycle: { state: "terminal", status: "completed" }, holds: { state: "read", holds: [] },
    signal: new AbortController().signal, deadline: Date.now() + 60_000, peekSlot: async () => null, peekAllSlots: async () => [],
    inspectMaintenance: inspectMaintenance(), ...over,
  };
}
const TOKEN = "fixture";
/** The production reader: the authenticated recovery-maintenance validator over the fixture's data dir. */
const inspectMaintenance = (): ((runId: string, bound?: { signal: AbortSignal; deadline: number }) => ReturnType<RecoveryMaintenance["inspectRun"]>) =>
  (runId, bound) => new RecoveryMaintenance({ root: path.join(fx.dataDir, "recovery-maintenance"), workerToken: TOKEN }).inspectRun(runId, bound);
const classify = (over: Partial<EpochScratchRunRetentionOptions> = {}, runId = RUN) => git.classifyEpochScratchRunRetention(runId, options(over));
const active = (source: string) => ({ state: "active", source });
const unknown = (source: string) => ({ state: "unknown", source });

beforeEach(async () => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  attempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000001");
  clonePath = `${git.runnerClonePath(bare, "issue-4420")}.attempt-${attempt}`;
  fs.mkdirSync(path.dirname(clonePath), { recursive: true });
});
afterEach(() => fx?.cleanup());

describe("baseline and the bare-config sources", () => {
  it("a bare with no record of the run is ended, and the trackowner stamp neither keeps nor releases", async () => {
    assert.deepEqual(await classify(), ENDED);
    cfg(`uzi-trackowner.${BRANCH}.owner`, JSON.stringify({ runId: RUN, anything: true }));
    cfg(`uzi-trackowner.${BRANCH}.receiptversion`, "1");
    assert.deepEqual(await classify(), ENDED, "stamp present alone releases nothing and keeps nothing");
    cfg(`uzi-stopped.${RUN}.entry`, "{}");
    assert.deepEqual(await classify(), active("stopped_marker"), "with another active source the stamp does not release it");
  });

  it("a run id that is not a uuid, and a repos root that cannot be enumerated, are unknown", async () => {
    assert.deepEqual(await git.classifyEpochScratchRunRetention("not-a-uuid", options()), unknown("run_id"));
    fs.writeFileSync(path.join(fx.dataDir, "repos", "stray.git"), "file, not a bare directory");
    assert.deepEqual(await classify(), unknown("classifier"), "a non-directory .git candidate refuses the whole scan");
  });

  it("an absent repos root is observed absent (ended)", async () => {
    fs.rmSync(path.dirname(bare), { recursive: true, force: true });
    assert.deepEqual(await classify(), ENDED);
  });

  for (const [state, source] of [["live", "ledger_live"], ["abandoned", "ledger_abandoned"], ["reclaimed", "ledger_reclaimed"]] as const) {
    it(`a ${state} ledger entry of the run is active (${source}); another run's is ignored`, async () => {
      // Another run's row carries its own attempt identity (a shared attempt id is a conflict: see the ID1 tests).
      const otherAttempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 2, "0000000000000002");
      const otherClone = `${git.runnerClonePath(bare, "issue-4420")}.attempt-${otherAttempt}`;
      cfg(`uzi-attempts.${BRANCH}.entry`, ledger(state, OTHER, otherClone, otherAttempt));
      assert.deepEqual(await classify(), ENDED);
      cfg(`uzi-attempts.${BRANCH}.entry`, ledger(state));
      assert.deepEqual(await classify(), active(source));
    });
  }

  it("the last ledger value per attempt wins: live then retired with a missing footprint is ended", async () => {
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("live"));
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired"));
    assert.deepEqual(await classify(), ENDED);
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("live"));
    assert.deepEqual(await classify(), active("ledger_live"));
  });

  it("a malformed ledger value, or a valueless protected-family key, is unknown, never skipped", async () => {
    cfg(`uzi-attempts.${BRANCH}.entry`, "{not json");
    assert.deepEqual(await classify(), unknown("attempt_ledger"));
    gitIn(["config", "--local", "--unset-all", `uzi-attempts.${BRANCH}.entry`]);
    assert.deepEqual(await classify(), ENDED);
    fs.appendFileSync(path.join(bare, "config"), `[uzi-stopped "x"]\n\tentry\n`);
    assert.deepEqual(await classify(), unknown("bare_config"));
  });

  it("the recovery journal: cleared is ended, this run's is active, another run's valid one is ended, an unparseable one is unknown", async () => {
    const key = `uzi-recovery.${BRANCH}.clone`;
    cfg(key, "");
    assert.deepEqual(await classify(), ENDED);
    cfg(key, JSON.stringify({ runId: RUN, clonePath }));
    assert.deepEqual(await classify(), active("recovery_journal"));
    cfg(key, "");
    assert.deepEqual(await classify(), ENDED, "the last value wins: a cleared journal releases");
    cfg(key, JSON.stringify({ runId: OTHER, clonePath }));
    assert.deepEqual(await classify(), ENDED);
    cfg(key, "{broken");
    assert.deepEqual(await classify(), unknown("recovery_journal"));
  });

  for (const [family, suffix, source] of [["stopped", "entry", "stopped_marker"], ["retained", "journal", "retained_descriptor"],
    ["recoveryowner", "record", "recovery_owner"]] as const) {
    it(`uzi-${family}.<run>.${suffix}: this run is active, a cleared value and another run's valid value are ended, another run's garbage is unknown`, async () => {
      // The worker-wide census is stubbed clean: these fixtures are not full retained-source records, so the real
      // census would (correctly) call a foreign malformed record an incomplete inventory.
      git.discoverEpochRetainedSources = async () => ({ groups: [], inventoryComplete: true, physicalPaths: 0, overflow: 0, overflowExact: true, unreadable: 0 });
      cfg(`uzi-${family}.${OTHER}.${suffix}`, "{}");
      assert.deepEqual(await classify(), ENDED);
      cfg(`uzi-${family}.${RUN}.${suffix}`, "");
      assert.deepEqual(await classify(), ENDED, "a cleared value is nothing");
      cfg(`uzi-${family}.${RUN}.${suffix}`, "{}");
      assert.deepEqual(await classify(), active(source));
      gitIn(["config", "--local", "--unset-all", `uzi-${family}.${RUN}.${suffix}`]);
      cfg(`uzi-${family}.${OTHER}.${suffix}`, "garbage");
      assert.deepEqual(await classify(), unknown("bare_config"));
    });
  }

  it("a restore journal naming the run (as its successor) is active; an unparseable one is unknown; a cleared one is ended", async () => {
    const key = `uzi-restore.${BRANCH}.journal`;
    cfg(key, "");
    assert.deepEqual(await classify(), ENDED);
    const predecessor = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000002");
    const successorAttempt = formatAttemptId(new Date("2026-01-02T00:00:00Z"), 2, "0000000000000003");
    const basis = { version: 1, predecessor: { clonePath: dataPath("runner", "slug", `issue-4420.attempt-${predecessor}`), attemptId: predecessor, generation: 1,
      root: { dev: "1", ino: "2" } }, journalDigest: "a".repeat(64), boundary: { predecessor: "b".repeat(64), successor: "c".repeat(64) },
      slotOwner: { worker: "w", epoch: "e" }, pin: { captureId: "cap", generation: 1, sourceSha: "d".repeat(40), finalization: false },
      restoreTip: "e".repeat(40) };
    cfg(key, JSON.stringify({ restore: basis, restoreStage: "retired",
      successor: { runId: RUN, attemptId: successorAttempt, clonePath: dataPath("runner", "slug", `issue-4420.attempt-${successorAttempt}`) } }));
    assert.deepEqual(await classify(), active("restore_journal"));
    gitIn(["config", "--local", "--unset-all", key]);
    cfg(key, "{nope");
    assert.deepEqual(await classify(), unknown("restore_journal"));
  });

  it("the run's refs: each family is active, another run's refs are ignored, a failed read is unknown", async t => {
    const sha = gitIn(["rev-parse", "--verify", "refs/remotes/origin/main"]);
    for (const ref of [`refs/uzi-owed/${OTHER}/${sha}`, `refs/uzi-cross-check/${OTHER}`]) gitIn(["update-ref", ref, sha]);
    assert.deepEqual(await classify(), ENDED);
    for (const ref of [`refs/uzi-owed/${RUN}/${sha}`, `refs/uzi-recovery-episode/${RUN}/${sha}`, `refs/uzi-recovery-pin/${RUN}/1`,
      `refs/uzi-coverage/${RUN}/1/${"f".repeat(8)}`, `refs/uzi-settle/${RUN}/hold/source`, `refs/uzi-archive/${RUN}/g1`, `refs/uzi-cross-check/${RUN}`]) {
      gitIn(["update-ref", ref, sha]);
      assert.deepEqual(await classify(), active("run_refs"), ref.split("/").slice(0, 2).join("/"));
      gitIn(["update-ref", "-d", ref]);
      assert.deepEqual(await classify(), ENDED, `${ref.split("/")[1]} deleted`);
    }
    type Exec = (cmd: string, args: string[], o: unknown) => Promise<{ stdout: string; stderr: string }>;
    const original = (git as unknown as { execScoped: Exec }).execScoped.bind(git);
    let mode: "throw" | "warn" = "throw";
    t.mock.method(git as unknown as { execScoped: Exec }, "execScoped", async (cmd: string, args: string[], o: unknown) => {
      if (args.includes("for-each-ref")) {
        if (mode === "throw") throw new Error("injected read failure");
        // The real listing (empty: the storage is clean), with a warning on stderr: the wiring of stderr itself.
        const real = await original(cmd, args, o);
        return { ...real, stderr: "warning: ignoring broken ref\n" };
      }
      return original(cmd, args, o);
    });
    assert.deepEqual(await classify(), unknown("run_refs"), "a plain listing failure is unknown, not a classifier fault");
    mode = "warn";
    assert.deepEqual(await classify(), unknown("run_refs"), "an empty exit-0 listing with stderr is unknown though the storage is clean");
  });

  it("the ref listing is trusted only with exit 0 and no stderr at all", () => {
    assert.deepEqual(judgeRunRefListing({ code: 0, stdout: "", stderr: "" }), ENDED);
    assert.deepEqual(judgeRunRefListing({ code: 0, stdout: "", stderr: "\n" }), unknown("run_refs"));
    assert.deepEqual(judgeRunRefListing({ code: 0, stdout: "refs/uzi-owed/x\n", stderr: "warning: ignoring broken ref" }), unknown("run_refs"));
    assert.deepEqual(judgeRunRefListing({ code: 1, stdout: "", stderr: "" }), unknown("run_refs"));
    assert.deepEqual(judgeRunRefListing({ code: 0, stdout: "refs/uzi-owed/x\n", stderr: "" }), active("run_refs"));
  });

  describe("refs git cannot list are unknown, not ended", () => {
    const put = (rel: string, content: string): string => {
      const file = path.join(bare, rel);
      fs.mkdirSync(path.dirname(file), { recursive: true });
      fs.writeFileSync(file, content);
      return file;
    };

    it("a garbage loose pin, and a dangling symref", async () => {
      const f = put(`refs/uzi-recovery-pin/${RUN}/1`, "garbage\n");
      assert.deepEqual(await classify(), unknown("run_refs"));
      fs.rmSync(f);
      assert.deepEqual(await classify(), ENDED, "an empty run directory is nothing");
      put(`refs/uzi-owed/${RUN}/x`, "ref: refs/heads/nope\n");
      assert.deepEqual(await classify(), unknown("run_refs"));
    });

    it("a FIFO under the run's directory", async t => {
      const dir = path.join(bare, "refs", "uzi-settle", RUN);
      fs.mkdirSync(dir, { recursive: true });
      try { execFileSync("mkfifo", [path.join(dir, "pipe")]); } catch { t.skip("mkfifo unavailable"); return; }
      assert.deepEqual(await classify(), unknown("run_refs"));
    });

    it("a dangling symlink at a family directory, and a symlinked run directory", async () => {
      const fam = path.join(bare, "refs", "uzi-owed");
      fs.rmSync(fam, { recursive: true, force: true });
      fs.symlinkSync("/nonexistent-target", fam);
      assert.deepEqual(await classify(), unknown("run_refs"));
      fs.unlinkSync(fam);
      fs.mkdirSync(fam);
      fs.symlinkSync("/nonexistent-target", path.join(fam, RUN));
      assert.deepEqual(await classify(), unknown("run_refs"));
    });

    it("a cross-check entry of any type under the run's id", async () => {
      fs.mkdirSync(path.join(bare, "refs", "uzi-cross-check", RUN), { recursive: true });
      assert.deepEqual(await classify(), unknown("run_refs"));
    });

    it("a packed-refs line under a run prefix that the listing did not report", async () => {
      const file = path.join(bare, "packed-refs");
      const existing = fs.existsSync(file) ? fs.readFileSync(file, "utf8") : "# pack-refs with: peeled fully-peeled sorted \n";
      fs.writeFileSync(file, `${existing}zzzz refs/uzi-owed/${RUN}/bad\n`);
      assert.deepEqual(await classify(), unknown("run_refs"));
      fs.writeFileSync(file, `${existing}zzzz refs/uzi-cross-check/${RUN}\n`);
      assert.deepEqual(await classify(), unknown("run_refs"));
      fs.writeFileSync(file, `${existing}zzzz refs/uzi-cross-check/${OTHER}\n`);
      assert.deepEqual(await classify(), ENDED, "another run's line is not this run's");
    });

    it("reftable storage, by extension or by directory", async () => {
      fs.mkdirSync(path.join(bare, "reftable"));
      assert.deepEqual(await classify(), unknown("run_refs"));
      fs.rmdirSync(path.join(bare, "reftable"));
      assert.deepEqual(await classify(), ENDED);
      cfg("extensions.refstorage", "reftable");
      // git itself may refuse a repository whose declared storage it cannot open; either way it is not ended.
      assert.equal((await classify()).state, "unknown");
    });
  });

  it("positive controls: a readable pin (loose or packed) is active; an empty left-over family directory is ended", async () => {
    const sha = gitIn(["rev-parse", "--verify", "refs/remotes/origin/main"]);
    fs.mkdirSync(path.join(bare, "refs", "uzi-archive"), { recursive: true });
    assert.deepEqual(await classify(), ENDED);
    const ref = `refs/uzi-recovery-pin/${RUN}/1`;
    gitIn(["update-ref", ref, sha]);
    assert.deepEqual(await classify(), active("run_refs"));
    gitIn(["pack-refs", "--all"]);
    assert.deepEqual(await classify(), active("run_refs"));
    gitIn(["update-ref", "-d", ref]);
    assert.deepEqual(await classify(), ENDED);
  });

  it("another uzi-* key is ignored unless it names the run", async () => {
    cfg("uzi-future.thing.value", "x");
    assert.deepEqual(await classify(), ENDED);
    cfg("uzi-future.thing.other", `names ${RUN} somewhere`);
    assert.deepEqual(await classify(), active("bare_config"));
  });
});

describe("attempt footprint and authority slot (a retired ledger entry is ended only with a verified missing footprint)", () => {
  const retire = () => cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired"));
  const slot = (over: Record<string, unknown> = {}) => ({ state: "ineligible", pending: null, retiring: null, publication: null, ...over });

  it("a retired row whose path is not this bare's canonical attempt clone is unknown (an unrelated missing child is not an observed-absent footprint)", async () => {
    const stray = dataPath("runner", "slug", `issue-4420.attempt-${attempt}`);
    fs.mkdirSync(path.dirname(stray), { recursive: true });
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired", RUN, stray));
    assert.deepEqual(await classify(), unknown("attempt_ledger"));
    gitIn(["config", "--local", "--unset-all", `uzi-attempts.${BRANCH}.entry`]);
    retire();
    assert.deepEqual(await classify(), ENDED, "positive control: the canonical path with a missing footprint is ended");
  });

  it("a retired row whose attempt id differs from its path's attempt id is unknown", async () => {
    const other = formatAttemptId(new Date("2026-01-02T00:00:00Z"), 1, "0000000000000002");
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired", RUN, clonePath, other));
    assert.deepEqual(await classify(), unknown("attempt_ledger"));
    gitIn(["config", "--local", "--unset-all", `uzi-attempts.${BRANCH}.entry`]);
    retire();
    assert.deepEqual(await classify(), ENDED, "positive control");
  });

  it("a retired row whose attempt id is well-formed in its path but carries an unreadable (non-safe-integer) generation is unknown", async () => {
    const huge = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000001").replace("-g1-", "-g99999999999999999999-");
    const hugePath = `${git.runnerClonePath(bare, "issue-4420")}.attempt-${huge}`;
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired", RUN, hugePath, huge));
    assert.deepEqual(await classify(), unknown("attempt_ledger"));
    gitIn(["config", "--local", "--unset-all", `uzi-attempts.${BRANCH}.entry`]);
    const safe = huge.replace("-g99999999999999999999-", "-g12-");
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired", RUN, `${git.runnerClonePath(bare, "issue-4420")}.attempt-${safe}`, safe));
    assert.deepEqual(await classify(), ENDED, "positive control: the same shape with a readable generation");
  });

  it("a row recorded in bare A whose path is canonical under a second bare B is unknown", async () => {
    const fx2 = makeFixture();
    try {
      const bareB = await git.ensureClone(fx2.originPath);
      assert.notEqual(bareB, bare);
      const underB = `${git.runnerClonePath(bareB, "issue-4420")}.attempt-${attempt}`;
      assert.notEqual(underB, clonePath);
      cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired", RUN, underB));
      assert.deepEqual(await classify(), unknown("attempt_ledger"));
      gitIn(["config", "--local", "--unset-all", `uzi-attempts.${BRANCH}.entry`]);
      retire();
      assert.deepEqual(await classify(), ENDED, "positive control: bare A's own canonical path");
    } finally { fx2.cleanup(); }
  });

  it("ID1: a later row of another run (or branch, or path) reusing an attempt id never erases this run's row: unknown", async () => {
    const otherBranch = "agent/issue-9999";
    fs.mkdirSync(clonePath);
    const cases: Array<[string, string, string]> = [
      ["another run, same branch", BRANCH, ledger("retired", OTHER)],
      ["another run, another branch", otherBranch, ledger("retired", OTHER)],
      ["same run, another branch", otherBranch, ledger("retired")],
      ["same run, another path", BRANCH, ledger("retired", RUN, `${clonePath}-x`)],
    ];
    for (const [label, branch, value] of cases) {
      cfg(`uzi-attempts.${BRANCH}.entry`, ledger("live"));
      cfg(`uzi-attempts.${branch}.entry`, value);
      assert.deepEqual(await classify(), unknown("attempt_ledger"), label);
      gitIn(["config", "--local", "--unset-all", `uzi-attempts.${BRANCH}.entry`]);
      if (branch !== BRANCH) gitIn(["config", "--local", "--unset-all", `uzi-attempts.${branch}.entry`]);
    }
  });

  it("ID1: a later row of the same run and branch reusing an attempt id at another CANONICAL clone path is unknown, not folded", async () => {
    // Canonical for this attempt id (same repo dir, valid attempt basename), so the per-entry identity check accepts it:
    // only the ledger fold's path comparison can refuse it.
    const otherPath = `${git.runnerClonePath(bare, "issue-4421")}.attempt-${attempt}`;
    assert.notEqual(otherPath, clonePath);
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("live"));
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired", RUN, otherPath));
    assert.deepEqual(await classify(), unknown("attempt_ledger"));
  });

  it("ID1 control: rows of one identical identity keep last-value-wins (retired, live, retired is ended; live last is active)", async () => {
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired"));
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("live"));
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("retired"));
    assert.deepEqual(await classify(), ENDED);
    cfg(`uzi-attempts.${BRANCH}.entry`, ledger("live"));
    assert.deepEqual(await classify(), active("ledger_live"));
  });

  it("retired with the clone directory present is active; with the skills sibling present is active", async () => {
    retire();
    fs.mkdirSync(clonePath);
    assert.deepEqual(await classify(), active("attempt_footprint"));
    fs.rmdirSync(clonePath);
    assert.deepEqual(await classify(), ENDED);
    fs.mkdirSync(path.join(path.dirname(clonePath), `.uzi-skills-${path.basename(clonePath)}`));
    assert.deepEqual(await classify(), active("attempt_footprint"));
  });

  it("a symlink, a non-directory or a missing ancestor cannot prove the footprint absent (unknown, not ended)", async () => {
    retire();
    fs.symlinkSync("/nonexistent-target", clonePath);
    assert.deepEqual(await classify(), unknown("attempt_footprint"));
    fs.unlinkSync(clonePath);
    fs.writeFileSync(clonePath, "x");
    assert.deepEqual(await classify(), unknown("attempt_footprint"));
    fs.unlinkSync(clonePath);
    fs.rmdirSync(path.dirname(clonePath));
    assert.deepEqual(await classify(), unknown("attempt_footprint"), "a missing ancestor is not an observed-absent footprint");
  });

  it("authority: an absent slot, an ineligible slot with every field null and a retired tombstone are ended", async () => {
    retire();
    for (const read of [async () => null, async () => slot(), async () => slot({ state: "retired" })]) {
      assert.deepEqual(await classify({ peekSlot: read as never }), ENDED);
    }
  });

  it("authority: each of pending, retiring and publication alone keeps; any live state keeps", async () => {
    retire();
    for (const over of [{ pending: {} }, { retiring: {} }, { publication: {} }, { state: "active" }, { state: "retiring" }, { state: "sealed" }]) {
      assert.deepEqual(await classify({ peekSlot: (async () => slot(over)) as never }), active("authority_slot"), JSON.stringify(Object.keys(over)));
    }
  });

  it("authority: a malformed slot and a read error keep as unknown; a missing accessor keeps", async () => {
    retire();
    assert.deepEqual(await classify({ peekSlot: (async () => slot({ pending: undefined })) as never }), unknown("authority_slot"));
    assert.deepEqual(await classify({ peekSlot: (async () => ({ state: 7 })) as never }), unknown("authority_slot"));
    assert.deepEqual(await classify({ peekSlot: async () => { throw new Error("unauthenticated store"); } }), unknown("authority_slot"));
    assert.deepEqual(await classify({ peekSlot: undefined }), unknown("authority_slot"));
  });

  it("authority: an ineligible slot never stands in for a present footprint (and is not even read)", async () => {
    retire();
    fs.mkdirSync(clonePath);
    let reads = 0;
    const peekSlot = (async () => { reads++; return slot(); }) as never;
    assert.deepEqual(await classify({ peekSlot }), active("attempt_footprint"));
    assert.equal(reads, 0);
  });

  it("a run with no ledger entry reads no authority slot", async () => {
    let reads = 0;
    assert.deepEqual(await classify({ peekSlot: (async () => { reads++; return null; }) as never }), ENDED);
    assert.equal(reads, 0);
  });

  it("authority inventory: a retained publication slot naming the run keeps it even with NO attempt ledger entry (compacted away)", async () => {
    const publication = (runId: string) => ({ harness: "codex", sourceCoreDigest: "d".repeat(64), cohort: { runId, generation: 1, captureIds: [] } });
    assert.deepEqual(await classify({ peekAllSlots: async () => [slot({ state: "sealed", publication: publication(OTHER) })] as never }), ENDED, "another run's slot");
    assert.deepEqual(await classify({ peekAllSlots: async () => [slot({ state: "sealed", publication: publication(RUN) })] as never }),
      active("authority_slot"));
    assert.deepEqual(await classify({ peekAllSlots: async () => [slot({ state: "ineligible", publication: publication(RUN.toUpperCase()) })] as never }),
      active("authority_slot"), "kept through ineligible, matched case-insensitively");
  });

  it("authority inventory: an unreadable, over-cap or missing enumeration, or a malformed publication, is unknown", async () => {
    assert.deepEqual(await classify({ peekAllSlots: async () => { throw new Error("unauthenticated store"); } }), unknown("authority_inventory"));
    assert.deepEqual(await classify({ peekAllSlots: undefined }), unknown("authority_inventory"));
    assert.deepEqual(await classify({ peekAllSlots: (async () => [slot({ publication: { cohort: {} } })]) as never }), unknown("authority_inventory"));
    let cap = 0;
    await classify({ peekAllSlots: async max => { cap = max; return []; } });
    assert.ok(cap > 0 && Number.isSafeInteger(cap), "the enumeration is bounded");
  });

  it("authority inventory: a publication-less slot with no ledger entry is unknown, whatever its state", async () => {
    for (const over of [{ state: "active" }, { state: "retiring" }, { pending: {} }, { retiring: {} }, { state: "weird" },
      { state: "ineligible", key: "k1" }, { state: "retired", key: "k1" }]) {
      assert.deepEqual(await classify({ peekAllSlots: (async () => [slot(over)]) as never }), unknown("authority_inventory"), JSON.stringify(over));
    }
    assert.deepEqual(await classify({ peekAllSlots: (async () => [null]) as never }), unknown("authority_inventory"), "a malformed slot");
    assert.deepEqual(await classify({ peekAllSlots: (async () => [slot({ publication: undefined })]) as never }), unknown("authority_inventory"));
  });

  it("authority inventory: a tombstone attributed through the ledger is ended; an unattributed sibling makes it unknown", async () => {
    retire();
    const attributed = slot({ state: "retired", key: "k1" });
    const peekSlot = (async () => attributed) as never;
    assert.deepEqual(await classify({ peekSlot, peekAllSlots: (async () => [attributed]) as never }), ENDED);
    assert.deepEqual(await classify({ peekSlot, peekAllSlots: (async () => [attributed, slot({ state: "retired", key: "k2" })]) as never }),
      unknown("authority_inventory"));
    assert.deepEqual(await classify({ peekSlot, peekAllSlots: (async () => [slot({ state: "retired", key: "k2" })]) as never }),
      unknown("authority_inventory"), "the attributed slot is not the one in the inventory");
    for (const over of [{ state: "active" }, { state: "retiring" }, { pending: {} }, { retiring: {} }]) {
      assert.deepEqual(await classify({ peekSlot, peekAllSlots: (async () => [slot({ state: "retired", key: "k1", ...over })]) as never }),
        unknown("authority_inventory"), `same key, differing inventory snapshot ${JSON.stringify(over)}`);
    }
  });

  it("authority inventory: the pass's signal and deadline reach the accessor, and an interruption mid-read is unknown", async t => {
    for (const mode of ["abort", "deadline"] as const) {
      // A controlled clock: the budget cannot expire during setup, and expires exactly when the accessor has the bound.
      let clock = Date.now();
      const clockMock = t.mock.method(Date, "now", () => clock);
      try {
        const controller = new AbortController();
        const deadline = clock + 60_000;
        let received: { signal: AbortSignal; deadline: number } | undefined;
        const peekAllSlots = async (_max: number, bound?: { signal: AbortSignal; deadline: number }) => {
          received = bound;
          if (mode === "abort") controller.abort();
          else clock = deadline + 1;
          // The production accessor refuses between its reads once the bound is spent.
          if (bound && (bound.signal.aborted || Date.now() >= bound.deadline)) throw new Error("authority read interrupted");
          return [];
        };
        const result = await classify({ signal: controller.signal, deadline, peekAllSlots });
        assert.equal(received?.signal, controller.signal, mode);
        assert.equal(received?.deadline, deadline, mode);
        assert.deepEqual(result, unknown(mode === "abort" ? "aborted" : "classifier"), mode);
      } finally {
        clockMock.mock.restore();
      }
    }
  });

  it("per-attempt slot read: the pass's signal and deadline reach peekSlot as its bound", async () => {
    retire();
    const controller = new AbortController();
    const deadline = Date.now() + 60_000;
    let calls = 0;
    let received: unknown;
    const peekSlot = (async (_clonePath: string, bound?: unknown) => { calls++; received = bound; return null; }) as never;
    assert.deepEqual(await classify({ signal: controller.signal, deadline, peekSlot }), ENDED);
    assert.equal(calls, 1, "the retired row with a missing footprint reads the slot once");
    assert.deepEqual(received, { signal: controller.signal, deadline });
    assert.equal((received as { signal: AbortSignal }).signal, controller.signal);
  });

  it("authority inventory: read once per pass through the memo", async () => {
    let reads = 0;
    const memo: NonNullable<EpochScratchRunRetentionOptions["memo"]> = {};
    const peekAllSlots = async () => { reads++; return []; };
    assert.deepEqual(await classify({ memo, peekAllSlots }), ENDED);
    assert.deepEqual(await classify({ memo, peekAllSlots }, OTHER), ENDED);
    assert.equal(reads, 1);
  });
});

describe("custody and the retained groups", () => {
  it("recovery and settlement records of the run are active; an empty directory and another run's are ended", async () => {
    fs.mkdirSync(dataPath("recovery", RUN), { recursive: true });
    assert.deepEqual(await classify(), ENDED, "an empty run directory holds nothing");
    fs.mkdirSync(dataPath("recovery", OTHER));
    fs.writeFileSync(dataPath("recovery", OTHER, "a.json"), "{}");
    assert.deepEqual(await classify(), ENDED);
    fs.writeFileSync(dataPath("recovery", RUN, "a.json"), "{}");
    assert.deepEqual(await classify(), active("custody"));
    fs.rmSync(dataPath("recovery", RUN), { recursive: true });
    fs.mkdirSync(dataPath("recovery-settlement", RUN), { recursive: true });
    fs.writeFileSync(dataPath("recovery-settlement", RUN, "a.json"), "{}");
    assert.deepEqual(await classify(), active("custody"));
  });

  it("an unreadable custody store is held (fail closed, folded into active), not ended", async () => {
    fs.writeFileSync(dataPath("recovery"), "a file where the store directory belongs");
    assert.deepEqual(await classify(), active("custody"));
  });

  it("terminal protection reported by the configured callback is custody; a throwing callback is custody", async () => {
    for (const protection of [async () => true, async () => { throw new Error("unreadable outbox"); }]) {
      const guarded = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ terminalRecordProtection: protection }));
      assert.deepEqual(await guarded.classifyEpochScratchRunRetention(RUN, options()), active("custody"));
    }
    const clear = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ terminalRecordProtection: async () => false }));
    assert.deepEqual(await clear.classifyEpochScratchRunRetention(RUN, options()), ENDED);
  });

  it("the memoised census is shared across calls: a second run leaf does not rescan", async () => {
    const discovery: NonNullable<EpochScratchRunRetentionOptions["discovery"]> = {};
    let scans = 0;
    const original = git.discoverEpochRetainedSources.bind(git);
    git.discoverEpochRetainedSources = async o => { scans++; return original(o); };
    assert.deepEqual(await classify({ discovery }), ENDED);
    assert.deepEqual(await classify({ discovery }, OTHER), ENDED);
    assert.equal(scans, 1);
  });

  it("with a pass memo each bare's config is read once across run leaves; without one each leaf reads it", async t => {
    const original = (git as unknown as { runGit: (cwd: string, args: string[]) => Promise<string> }).runGit.bind(git);
    let configReads = 0;
    t.mock.method(git as unknown as { runGit: typeof original }, "runGit", async (cwd: string, args: string[]) => {
      if (args.join(" ") === "config --local --null --list") configReads++;
      return original(cwd, args);
    });
    const memo: NonNullable<EpochScratchRunRetentionOptions["memo"]> = {};
    const discovery: NonNullable<EpochScratchRunRetentionOptions["discovery"]> = {};
    assert.deepEqual(await classify({ memo, discovery }), ENDED);
    const afterFirst = configReads;
    assert.ok(afterFirst >= 1);
    assert.deepEqual(await classify({ memo, discovery }, OTHER), ENDED);
    assert.equal(configReads, afterFirst, "a second leaf reads no bare config again");
    cfg(`uzi-stopped.${OTHER}.entry`, "{}");
    assert.deepEqual(await classify({ memo, discovery }, OTHER), ENDED, "the memoised listing is the pass's snapshot");
    assert.equal(configReads, afterFirst);
    assert.deepEqual(await classify({ discovery }, OTHER), active("stopped_marker"));
    assert.equal(configReads, afterFirst + 1, "without a memo the leaf reads the bare's config itself");
  });

  it("a memoised config read failure stays unknown for every leaf", async t => {
    const original = (git as unknown as { runGit: (cwd: string, args: string[]) => Promise<string> }).runGit.bind(git);
    let configReads = 0;
    t.mock.method(git as unknown as { runGit: typeof original }, "runGit", async (cwd: string, args: string[]) => {
      if (args.join(" ") === "config --local --null --list") { configReads++; throw new Error("boom"); }
      return original(cwd, args);
    });
    const memo: NonNullable<EpochScratchRunRetentionOptions["memo"]> = {};
    assert.deepEqual(await classify({ memo }), unknown("classifier"));
    assert.deepEqual(await classify({ memo }, OTHER), unknown("classifier"));
    assert.equal(configReads, 1);
  });

  it("an inventory that is incomplete or overflowing may hide the run's groups: unknown", async () => {
    for (const census of [{ inventoryComplete: false, unreadable: 0, overflow: 0 }, { inventoryComplete: true, unreadable: 1, overflow: 0 },
      { inventoryComplete: true, unreadable: 0, overflow: 2 }]) {
      git.discoverEpochRetainedSources = async () => ({ groups: [], physicalPaths: 0, overflowExact: true, ...census });
      assert.deepEqual(await classify(), unknown("retained_inventory"));
    }
  });

  it("an inspection that is partial or returns the wrong number of results is unknown", async () => {
    const group = { barePath: bare, selector: { runId: RUN, generation: 1 as const, attempt: attempt }, paths: [clonePath] };
    git.discoverEpochRetainedSources = async () => ({ groups: [group], inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0 });
    git.inspectEpochRetainedGroups = async () => [];
    assert.deepEqual(await classify(), unknown("retained_inventory"));
    git.inspectEpochRetainedGroups = async () => [{ sources: [], partial: true, unreadable: 0, sampledAt: "" }];
    assert.deepEqual(await classify(), unknown("retained_inventory"));
    git.inspectEpochRetainedGroups = async () => [{ sources: [], partial: false, unreadable: 1, sampledAt: "" }];
    assert.deepEqual(await classify(), unknown("retained_inventory"));
    git.inspectEpochRetainedGroups = async () => [{ sources: [], partial: false, unreadable: 0, sampledAt: "" }];
    assert.deepEqual(await classify(), ENDED, "a complete inspection with no source of the run has nothing left to keep");
  });
});

describe("judgeRetainedEvidence: no disposition alone yields ended", () => {
  const settled = (over: Partial<RetainedEvidenceShape> = {}): RetainedEvidenceShape => ({
    kind: "attempt", ledgerState: "retired", presence: "missing", sources: [{ presence: "missing", live: false }],
    epochSettlement: { basis: "strict_plain", disposition: { kind: "local_discard" } }, ...over });

  it("only the verified settled shape is ended", () => {
    assert.deepEqual(judgeRetainedEvidence(settled()), ENDED);
  });
  it("a bare local_discard disposition, on any other shape, is active", () => {
    const local = { disposition: { kind: "local_discard" } };
    assert.deepEqual(judgeRetainedEvidence(settled({ epochSettlement: undefined, ...local })), active("retained_source"));
    assert.deepEqual(judgeRetainedEvidence(settled({ presence: "present", sources: [{ presence: "present", live: false }] })), active("attempt_footprint"));
    assert.deepEqual(judgeRetainedEvidence(settled({ sources: [{ presence: "missing", live: false }, { presence: "present", live: false }] })), active("attempt_footprint"));
    assert.deepEqual(judgeRetainedEvidence(settled({ sources: [{ presence: "missing", live: true }] })), active("attempt_footprint"));
    assert.deepEqual(judgeRetainedEvidence(settled({ sources: [] })), active("attempt_footprint"));
    assert.deepEqual(judgeRetainedEvidence(settled({ kind: "canonical" })), active("retained_source"));
    assert.deepEqual(judgeRetainedEvidence(settled({ epochSettlement: { basis: "other", disposition: { kind: "local_discard" } } })), active("retained_source"));
  });
  it("protected, owner-hold, local-record and refused dispositions each name their keep", () => {
    assert.deepEqual(judgeRetainedEvidence(settled({ epochSettlement: undefined, disposition: { kind: "local_protected_discard" } })), active("protected_source"));
    assert.deepEqual(judgeRetainedEvidence(settled({ kind: "protected" })), active("protected_source"));
    assert.deepEqual(judgeRetainedEvidence(settled({ epochSettlement: undefined, disposition: { kind: "owner_hold_discard" } })), active("owner_hold"));
    assert.deepEqual(judgeRetainedEvidence(settled({ epochSettlement: undefined, disposition: { kind: "resolve_local_record" } })), active("local_record"));
    assert.deepEqual(judgeRetainedEvidence(settled({ epochSettlement: undefined, disposition: { kind: "refused" } })), active("retained_refused"));
  });
  it("a non-retired ledger state keeps under its own name even when the strict basis says local_discard", () => {
    for (const state of ["live", "abandoned", "reclaimed"]) {
      assert.deepEqual(judgeRetainedEvidence(settled({ ledgerState: state })), active(`ledger_${state}`));
    }
  });
});

describe("judgeAuthoritySlot and normalizeRetention", () => {
  it("an absent slot is ended; a malformed one is unknown", () => {
    assert.deepEqual(judgeAuthoritySlot("missing", null), ENDED);
    assert.deepEqual(judgeAuthoritySlot("missing", "slot" as never), unknown("authority_slot"));
    assert.deepEqual(judgeAuthoritySlot("missing", { state: "ineligible", pending: null, retiring: null } as never), unknown("authority_slot"));
  });
  it("ended requires the footprint verdict to be missing, whatever the slot says", () => {
    assert.deepEqual(judgeAuthoritySlot("present", null), active("attempt_footprint"));
    assert.deepEqual(judgeAuthoritySlot("unknown", null), unknown("attempt_footprint"));
    assert.deepEqual(judgeAuthoritySlot("bogus" as never, null), unknown("attempt_footprint"));
  });
  it("a classifier result that is not a fixed-token verdict never reads as ended", () => {
    assert.deepEqual(normalizeRetention(undefined), unknown("classifier"));
    assert.deepEqual(normalizeRetention({ state: "done" }), unknown("classifier"));
    assert.deepEqual(normalizeRetention({ state: "active", source: "/data/runner/run-x" }), active("unspecified"));
    assert.deepEqual(normalizeRetention({ state: "unknown" }), unknown("unspecified"));
    assert.deepEqual(normalizeRetention({ state: "ended" }), ENDED);
  });
});

describe("dataDir records", () => {
  const records = (id = RUN, over: { maxDirectoryEntries?: number } = {}) =>
    classifyRunDataDirRecords(fx.dataDir, id, { interrupt: () => undefined, maxDirectoryEntries: over.maxDirectoryEntries ?? 16384, inspectMaintenance: inspectMaintenance() });
  const mkfile = (parts: string[], text = "{}") => { fs.mkdirSync(dataPath(...parts.slice(0, -1)), { recursive: true }); fs.writeFileSync(dataPath(...parts), text); };

  it("a data dir with none of the run's records is ended (every store root absent is observed absent)", async () => {
    assert.deepEqual(await records(), ENDED);
  });

  it("agent-home: the run's directory and its restore generations are active, another run's is ended", async () => {
    fs.mkdirSync(dataPath("agent-home", OTHER), { recursive: true });
    fs.mkdirSync(dataPath("agent-home", `${OTHER}.g2`));
    assert.deepEqual(await records(), ENDED);
    fs.mkdirSync(dataPath("agent-home", `${RUN}.g3`));
    assert.deepEqual(await records(), active("run_home"));
    fs.rmdirSync(dataPath("agent-home", `${RUN}.g3`));
    fs.mkdirSync(dataPath("agent-home", RUN));
    assert.deepEqual(await records(), active("run_home"));
  });

  it("agent-home: an unreadable listing and a listing over the cap are unknown", async () => {
    fs.writeFileSync(dataPath("agent-home"), "a file");
    assert.deepEqual(await records(), unknown("run_home"));
    fs.rmSync(dataPath("agent-home"));
    fs.mkdirSync(dataPath("agent-home", "a"), { recursive: true });
    fs.mkdirSync(dataPath("agent-home", "b"));
    assert.deepEqual(await records(RUN, { maxDirectoryEntries: 1 }), unknown("run_home"));
  });

  it("retained-homes marker: present is active, an unreadable store is unknown", async () => {
    mkfile(["retained-homes", `${RUN}.json`]);
    assert.deepEqual(await records(), active("retained_home"));
    fs.rmSync(dataPath("retained-homes"), { recursive: true });
    fs.writeFileSync(dataPath("retained-homes"), "a file where the store belongs");
    assert.deepEqual(await records(), unknown("retained_home"));
  });

  it("credential-switch-ops: sending and ambiguous are active; settled and clean_route_absent are ended; absent is ended", async () => {
    const file = ["credential-switch-ops", `${RUN}.json`];
    assert.deepEqual(await records(), ENDED);
    for (const [states, expected] of [[["settled", "clean_route_absent"], ENDED], [["settled", "sending"], active("credential_switch")],
      [["ambiguous"], active("credential_switch")]] as const) {
      mkfile(file, JSON.stringify({ ops: Object.fromEntries(states.map((state, i) => [`op${i}`, { generation: 1, state }])) }));
      assert.deepEqual(await records(), expected, states.join("+"));
    }
  });

  it("credential-switch-ops: malformed, oversized, symlinked and unreadable files are unknown", async () => {
    const file = ["credential-switch-ops", `${RUN}.json`];
    for (const text of ["{nope", JSON.stringify({ ops: [] }), JSON.stringify({ ops: { a: { generation: 1, state: "weird" } } }), JSON.stringify({ nothing: 1 })]) {
      mkfile(file, text);
      assert.deepEqual(await records(), unknown("credential_switch"), text.slice(0, 20));
    }
    mkfile(file, "x".repeat(1024 * 1024 + 1));
    assert.deepEqual(await records(), unknown("credential_switch"));
    fs.rmSync(dataPath(...file));
    fs.symlinkSync(dataPath("elsewhere.json"), dataPath(...file));
    assert.deepEqual(await records(), unknown("credential_switch"));
    fs.rmSync(dataPath(...file));
    fs.mkdirSync(dataPath(...file));
    assert.deepEqual(await records(), unknown("credential_switch"));
  });

  it("runner-quarantine: an entry with the run's holding prefix is active; other entries are ended; bad roots and over-cap listings are unknown", async () => {
    fs.mkdirSync(dataPath("runner-quarantine", `${OTHER}-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee`), { recursive: true });
    assert.deepEqual(await records(), ENDED);
    fs.mkdirSync(dataPath("runner-quarantine", `${RUN}-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee`));
    assert.deepEqual(await records(), active("runner_quarantine"));
    fs.rmSync(dataPath("runner-quarantine"), { recursive: true });
    fs.writeFileSync(dataPath("runner-quarantine"), "a file");
    assert.deepEqual(await records(), unknown("runner_quarantine"));
    fs.rmSync(dataPath("runner-quarantine"));
    fs.symlinkSync(dataPath("agent-home"), dataPath("runner-quarantine"));
    assert.deepEqual(await records(), unknown("runner_quarantine"));
    fs.rmSync(dataPath("runner-quarantine"));
    fs.mkdirSync(dataPath("runner-quarantine"));
    for (let i = 0; i < 4097; i++) fs.writeFileSync(dataPath("runner-quarantine", `entry-${i}`), "");
    assert.deepEqual(await records(), unknown("runner_quarantine"));
  });

  it("terminal-preservation and recovery-archive: the run's entry is active; an unreadable parent is unknown", async () => {
    fs.mkdirSync(dataPath("terminal-preservation", OTHER), { recursive: true });
    fs.mkdirSync(dataPath("recovery-archive", OTHER), { recursive: true });
    assert.deepEqual(await records(), ENDED);
    fs.mkdirSync(dataPath("terminal-preservation", RUN));
    assert.deepEqual(await records(), active("terminal_preservation"));
    fs.rmdirSync(dataPath("terminal-preservation", RUN));
    fs.mkdirSync(dataPath("recovery-archive", RUN));
    assert.deepEqual(await records(), active("quarantine_archive"), "its contract has no ended predicate: presence alone keeps");
    fs.rmSync(dataPath("recovery-archive"), { recursive: true });
    fs.writeFileSync(dataPath("recovery-archive"), "a file");
    assert.deepEqual(await records(), unknown("quarantine_archive"));
  });

  const maintenanceAt = (root: string) => new RecoveryMaintenance({ root, workerToken: TOKEN });
  /** One real, authenticated cycle of `run` in the ledger at `root`, then finished as `outcome` (complete = exhausted). */
  const admitCycle = async (root: string, run: string, outcome: "complete" | "transient", seed = "b") => {
    const ledger = maintenanceAt(root);
    const admitted = await ledger.admit({
      identity: { run, generation: 1, hold: "hold", repo: "repo", custodian: "worker", bareBasename: "x.git",
        origin: "a".repeat(64), originalSource: seed.repeat(64), sourceConfig: "c".repeat(64) },
      config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: seed.repeat(64),
    });
    assert.ok(admitted.ok);
    assert.ok((await ledger.finish(admitted.value, outcome)).ok);
  };

  it("recovery-maintenance: through the authenticated validator, a cycle of the run that is not exhausted is active; exhausted and other runs' are ended", async () => {
    const root = dataPath("recovery-maintenance");
    await admitCycle(root, OTHER, "transient", "d");
    await admitCycle(root, RUN, "complete");
    assert.deepEqual(await records(), ENDED);
    fs.rmSync(root, { recursive: true });
    await admitCycle(root, OTHER, "transient", "d");
    await admitCycle(root, RUN.toUpperCase(), "transient");
    assert.deepEqual(await records(), active("recovery_maintenance"));
  });

  it("recovery-maintenance: a pair the validator refuses (unauthenticated, mismatched hash, other ledger's table, incomplete, malformed marker) is unknown", async () => {
    const root = dataPath("recovery-maintenance");
    await admitCycle(root, RUN, "complete");
    assert.deepEqual(await records(), ENDED, "the valid exhausted pair is ended");
    const marker = fs.readFileSync(path.join(root, "initialized.json"), "utf8");
    const table = fs.readFileSync(path.join(root, "complete.json"), "utf8");
    // a flipped hash digit: the envelope MAC no longer matches
    fs.writeFileSync(path.join(root, "initialized.json"), marker.replace(/("tableHash":")([0-9a-f])/, (_m, k, c) => `${k}${c === "0" ? "1" : "0"}`));
    assert.deepEqual(await records(), unknown("recovery_maintenance"), "mismatched hash");
    fs.writeFileSync(path.join(root, "initialized.json"), marker);
    // another (valid, authenticated) ledger's table next to this ledger's marker
    fs.mkdirSync(dataPath("foreign"));
    const foreign = dataPath("foreign", "recovery-maintenance");
    await admitCycle(foreign, OTHER, "transient", "d");
    fs.copyFileSync(path.join(foreign, "complete.json"), path.join(root, "complete.json"));
    assert.deepEqual(await records(), unknown("recovery_maintenance"), "table of another ledger");
    fs.writeFileSync(path.join(root, "complete.json"), table.slice(0, table.length >> 1));
    assert.deepEqual(await records(), unknown("recovery_maintenance"), "incomplete table");
    fs.writeFileSync(path.join(root, "complete.json"), table);
    fs.writeFileSync(path.join(root, "initialized.json"), "{bad");
    assert.deepEqual(await records(), unknown("recovery_maintenance"), "malformed marker");
    fs.writeFileSync(path.join(root, "initialized.json"), marker);
    assert.deepEqual(await records(), ENDED, "restored");
    fs.rmSync(path.join(root, "initialized.json"));
    assert.deepEqual(await records(), unknown("recovery_maintenance"), "the pair is written together: one without the other is not 'absent'");
  });

  it("recovery-maintenance: the classifier's own signal and deadline are the bound handed to the maintenance reader", async () => {
    fs.mkdirSync(dataPath("recovery-maintenance"));
    const controller = new AbortController(), deadline = Date.now() + 123_456;
    const bounds: unknown[] = [];
    const result = await classify({ signal: controller.signal, deadline, inspectMaintenance: async (_id: string, bound?: unknown) => { bounds.push(bound); return "active" as const; } });
    assert.deepEqual(result, active("recovery_maintenance"));
    assert.equal(bounds.length, 1);
    assert.ok(bounds[0], "a bound is handed over");
    const bound = bounds[0] as { signal: AbortSignal; deadline: number };
    assert.equal(bound.signal, controller.signal);
    assert.equal(bound.deadline, deadline);
  });

  it("recovery-maintenance: a real ended table is ended; a deadline passing while the complete.json read is held stops before initialized.json and is unknown (classifier)", async t => {
    const root = dataPath("recovery-maintenance");
    await admitCycle(root, RUN, "complete");
    assert.deepEqual(await classify(), ENDED, "control: no interruption");
    let clock = Date.now();
    t.mock.method(Date, "now", () => clock);
    const opened: string[] = [], closed: string[] = [];
    let release!: () => void, entered!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    const reached = new Promise<void>(resolve => { entered = resolve; });
    const realOpen = fsp.open.bind(fsp);
    t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
      const handle = await realOpen(...args);
      const name = path.basename(String(args[0]));
      if (name !== "complete.json" && name !== "initialized.json") return handle;
      opened.push(name);
      const read = handle.read.bind(handle), close = handle.close.bind(handle);
      (handle as { read: unknown }).read = async (...rest: unknown[]) => {
        if (name === "complete.json" && !closed.length) { entered(); await gate; }
        return (read as (...a: unknown[]) => Promise<unknown>)(...rest);
      };
      (handle as { close: unknown }).close = async () => { closed.push(name); return close(); };
      return handle;
    });
    const deadline = clock + 60_000;
    let settled = false;
    const pass = classify({ deadline }).finally(() => { settled = true; });
    try {
      await reached;
      clock = deadline + 1;
      await new Promise(resolve => setImmediate(resolve));
      assert.equal(settled, false, "the held read is awaited, never abandoned");
    } finally { release(); }
    assert.deepEqual(await pass, unknown("classifier"));
    assert.deepEqual(opened, ["complete.json"], "initialized.json is never opened once the deadline passed");
    assert.deepEqual(closed, ["complete.json"]);
  });

  it("recovery-maintenance: a legacy unauthenticated shape, a leftover initialisation stage and a missing validator are unknown; absent is ended", async () => {
    mkfile(["recovery-maintenance", "complete.json"], JSON.stringify({ version: 1, cycles: [{ identity: { run: RUN }, exhausted: true }] }));
    mkfile(["recovery-maintenance", "initialized.json"], JSON.stringify({ version: 1 }));
    assert.deepEqual(await records(), unknown("recovery_maintenance"), "valid JSON with the right field names but no authentication");
    assert.deepEqual(await classifyRunDataDirRecords(fx.dataDir, RUN, { interrupt: () => undefined, maxDirectoryEntries: 16 }), unknown("recovery_maintenance"),
      "no validator supplied: a present store is kept");
    fs.rmSync(dataPath("recovery-maintenance"), { recursive: true });
    fs.mkdirSync(dataPath("recovery-maintenance.initializing"));
    assert.deepEqual(await records(), unknown("recovery_maintenance"));
    fs.rmdirSync(dataPath("recovery-maintenance.initializing"));
    assert.deepEqual(await records(), ENDED);
  });

  it("every store parent must be a real directory: a dangling symlink or a symlink to an empty directory is unknown, absent is ended", async () => {
    const stores: Array<[string, string]> = [["agent-home", "run_home"], ["retained-homes", "retained_home"],
      ["credential-switch-ops", "credential_switch"], ["runner-quarantine", "runner_quarantine"],
      ["terminal-preservation", "terminal_preservation"], ["recovery-archive", "quarantine_archive"]];
    for (const [store, source] of stores) {
      fs.symlinkSync(dataPath("no-such-target"), dataPath(store));
      assert.deepEqual(await records(), unknown(source), `${store}: dangling symlink`);
      fs.rmSync(dataPath(store));
      fs.mkdirSync(dataPath("empty-target"), { recursive: true });
      fs.symlinkSync(dataPath("empty-target"), dataPath(store));
      assert.deepEqual(await records(), unknown(source), `${store}: symlink to an empty directory`);
      fs.rmSync(dataPath(store));
      assert.deepEqual(await records(), ENDED, `${store}: absent`);
    }
  });

  it("the data dir itself must be a real directory", async () => {
    const real = dataPath("real-data");
    fs.mkdirSync(real);
    const link = dataPath("link-data");
    fs.symlinkSync(real, link);
    const opts = { interrupt: () => undefined, maxDirectoryEntries: 16 };
    assert.deepEqual(await classifyRunDataDirRecords(link, RUN, opts), unknown("data_dir"));
    assert.deepEqual(await classifyRunDataDirRecords(dataPath("missing"), RUN, opts), unknown("data_dir"));
  });

  it("credential-switch-ops: entries the ledger's own parser refuses are unknown (not an object, bad generation, bad outcome)", async () => {
    const file = ["credential-switch-ops", `${RUN}.json`];
    for (const ops of [{ a: { state: "settled", generation: "invalid" } }, { a: "settled" }, { a: null }, { a: { state: "settled", generation: 1.5 } },
      { a: { state: "settled", generation: 1, outcome: 7 } }, { a: { state: "weird", generation: 1 } }]) {
      assert.equal(classifyCredentialSwitchText(JSON.stringify({ ops })).state, "unknown", JSON.stringify(ops));
      mkfile(file, JSON.stringify({ ops }));
      assert.deepEqual(await records(), unknown("credential_switch"), JSON.stringify(ops));
      assert.deepEqual(await classify(), unknown("credential_switch"), `through the epoch path: ${JSON.stringify(ops)}`);
    }
    mkfile(file, JSON.stringify({ ops: { a: { state: "settled", generation: 1, outcome: "applied" } } }));
    assert.deepEqual(await records(), ENDED);
  });

  it("classifyCredentialSwitchText is pure", () => {
    assert.deepEqual(classifyCredentialSwitchText(JSON.stringify({ ops: {} })), ENDED);
    assert.deepEqual(classifyCredentialSwitchText(JSON.stringify({ ops: { a: { state: "ambiguous", generation: 2 } } })), active("credential_switch"));
  });

  it("the git classifier reads the same records (integration through the bare scan)", async () => {
    fs.mkdirSync(dataPath("agent-home", RUN), { recursive: true });
    assert.deepEqual(await classify(), active("run_home"));
  });
});

describe("a real retired attempt (driven through the production retire code)", () => {
  it("is ended once retired with a missing footprint, and active again if the footprint reappears", async () => {
    const real = formatAttemptId(new Date("2026-02-01T00:00:00Z"), 1, "0000000000000009");
    const clone = await git.runnerCloneForBranch(bare, BRANCH, "issue-4420", noProofReseed, RUN, false, undefined,
      { attemptId: real, isLive: () => false, beforeSeed: async () => {} });
    await git.markRecoveryCapture(bare, clone.path, BRANCH, RUN, real);
    assert.deepEqual(await classify(), active("recovery_journal"), "a journaled attempt is custody before its ledger is even read");
    await git.retireRunnerClone(bare, clone.path, BRANCH, RUN, { discard: true, attemptId: real });
    assert.equal(gitIn(["config", "--local", `uzi-recovery.${BRANCH}.clone`]), "");
    assert.deepEqual(await classify(), ENDED);
    fs.mkdirSync(clone.path);
    assert.deepEqual(await classify(), active("attempt_footprint"));
  });
});
