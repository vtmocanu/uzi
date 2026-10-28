import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import { createHash } from "node:crypto";
import path from "node:path";
import { GitBoundaryAbortError } from "../src/checkpoint-stop.js";
import { GitCache, type FetchAgentBranchOptions, type PositiveOwedCandidateContext, type RecoveryJournalEntry } from "../src/git.js";
import type { AuthoritySettlement, GitAuthorityHooks } from "../src/git-authority-hooks.js";
import { serializeSourceFence, type CompletionSourceFence } from "../src/completion-source.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { parseRecoveryJournal as baseParseRecoveryJournal } from "./fixtures/base-parse-recovery-journal.js";

// issue #3404 M2 U2: the completion-source fence in the recovery journal, its install and its
// consumption by completion retirement, and the authority hooks GitCache consults before effects.
// Real git in temp dirs; the hooks are a recording fake.

// An ambient GIT_DIR (a hook, a worktree gate) would redirect every `git -C` below at someone else's repo.
// Drop every GIT_* key (the repo convention, see makeFixture), then pin the global and system config off.
function scrubGitEnv(base: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(base)) if (!key.startsWith("GIT_")) env[key] = value;
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
}
const ENV = scrubGitEnv(process.env);
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-2512";
const KEY = "issue-2512";
const RUN = "run-2512";
const aid = (n: number) => `20261008T171401Z-g${n}-0123456789abcdef`;
const FENCE: CompletionSourceFence = { version: 1, sourceUseEpoch: "0123abcd-0123-4abc-8def-0123456789ab", sourceCoreDigest: "a".repeat(64) };
const OTHER: CompletionSourceFence = { ...FENCE, sourceCoreDigest: "b".repeat(64) };

let fx: Fixture;
let cache: GitCache;
let bare: string;
let clonePath: string;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const rawJournal = (): string => gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]);
const journal = (): Record<string, unknown> => JSON.parse(rawJournal()) as Record<string, unknown>;
const expected = () => ({ runId: RUN, clonePath });
const reconstructionSource = () => ({ runId: RUN, clonePath });

/** Recording hooks. `events` is shared with the git wrapper so cross-component order is observable. */
function recorder(events: string[], over: Partial<GitAuthorityHooks> = {}): GitAuthorityHooks {
  const settlement = (label: string): AuthoritySettlement => ({
    settle: async (outcome) => { events.push(`settle:${label}:${JSON.stringify(outcome)}`); },
  });
  return {
    admitSourceUse: async (p, op) => { events.push(`admit:${op}:${path.basename(p)}`); },
    beginBareMutation: async (_b, op, footprint) => { events.push(`bare:${op}:${footprint.join(",")}`); return settlement(op); },
    assertBareClear: async (_b, op) => { events.push(`clear:${op}`); },
    beginDestructive: async (paths, op) => { events.push(`destructive:${op}:${paths.map(p => path.basename(p)).join(",")}`); return settlement(op); },
    ...over,
  };
}

type Internals = { execScoped(command: string, args: string[], options: unknown): Promise<{ stdout: string; stderr: string }> };
/** Record (and optionally fail) git invocations matching `match` on this cache. */
function watchGit(c: GitCache, match: (args: string[]) => string | undefined, events: string[], failWith?: Error): void {
  const internals = c as unknown as Internals;
  const original = internals.execScoped.bind(c);
  internals.execScoped = async (command, args, options) => {
    const label = command === "git" ? match(args) : undefined;
    if (label) {
      events.push(`git:${label}`);
      if (failWith) throw failWith;
    }
    return original(command, args, options);
  };
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, RUN);
  clonePath = clone.path;
  await cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN);
});
afterEach(() => fx.cleanup());

describe("scrubGitEnv", () => {
  it("keeps decoy GIT_DIR and GIT_CONFIG from redirecting a config write away from the target repo", () => {
    const sentinel = fs.mkdtempSync(path.join(fx.dataDir, "sentinel-"));
    execFileSync("git", ["init", "-q", sentinel], { env: ENV });
    const sentinelConfig = path.join(sentinel, "sentinel.config");
    fs.writeFileSync(sentinelConfig, "");
    // Built from the scrubbed ENV so the unscrubbed controls carry only the decoy under test, never an
    // ambient GIT_* override that could send a control write into a real external config.
    const dirDecoy = { ...ENV, GIT_DIR: path.join(sentinel, ".git"), GIT_WORK_TREE: sentinel };
    const fileDecoy = { ...ENV, GIT_CONFIG: sentinelConfig };
    const both = { ...dirDecoy, ...fileDecoy };
    const cfg = (): string => fs.readFileSync(path.join(sentinel, ".git", "config"), "utf8");
    const extra = (): string => fs.readFileSync(sentinelConfig, "utf8");
    const refs = (): string => execFileSync("git", ["-C", sentinel, "for-each-ref"], { encoding: "utf8", env: ENV });
    const write = (env: NodeJS.ProcessEnv): void => {
      execFileSync("git", ["-C", bare, "config", "uzi-test.k", "v"], { env: { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } });
    };
    const cfgBefore = cfg(), refsBefore = refs(), extraBefore = extra();
    write(scrubGitEnv(both));
    assert.equal(gitIn(bare, ["config", "--get", "uzi-test.k"]), "v");
    assert.equal(cfg(), cfgBefore);
    assert.equal(extra(), extraBefore);
    assert.equal(refs(), refsBefore);
    // Controls: each unscrubbed decoy does hit its sentinel, so the scrub is what protects it.
    write(dirDecoy);
    assert.notEqual(cfg(), cfgBefore);
    assert.match(cfg(), /uzi-test/);
    write(fileDecoy);
    assert.notEqual(extra(), extraBefore);
    assert.match(extra(), /uzi-test/);
  });
});

describe("journal fence serialization", () => {
  it("installs the fence as the last key in fixed field order and round-trips it", async () => {
    const back = await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    assert.equal(serializeSourceFence(back), serializeSourceFence(FENCE));
    const raw = rawJournal();
    const keys = Object.keys(JSON.parse(raw) as object);
    assert.equal(keys.at(-1), "completionSourceFence");
    assert.ok(raw.endsWith(`"completionSourceFence":${serializeSourceFence(FENCE)}}`), raw);
    assert.deepEqual(Object.keys(journal().completionSourceFence as object), ["version", "sourceUseEpoch", "sourceCoreDigest"]);
  });

  it("keeps the fence last when a later markRecoveryCapture adds a peeled marker", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    await cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN, undefined, "c".repeat(40));
    assert.deepEqual(Object.keys(journal()).slice(-2), ["peeledMarkers", "completionSourceFence"]);
    assert.equal(JSON.stringify(journal().completionSourceFence), serializeSourceFence(FENCE));
  });

  it("refuses a malformed fence in the journal", async () => {
    for (const bad of [{ version: 2, sourceUseEpoch: FENCE.sourceUseEpoch, sourceCoreDigest: FENCE.sourceCoreDigest },
      { ...FENCE, sourceUseEpoch: "nope" }, { ...FENCE, extra: 1 }, { ...FENCE, sourceCoreDigest: "A".repeat(64) }, null]) {
      gitIn(bare, ["config", `uzi-recovery.${BRANCH}.clone`, JSON.stringify({ ...journal(), completionSourceFence: bad })]);
      await assert.rejects(cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN));
    }
  });
});

describe("rollback fence: a worker built before the fence refuses the journal", () => {
  it("the captured base parser accepts the plain journal and rejects the fenced one", async () => {
    assert.doesNotThrow(() => baseParseRecoveryJournal(rawJournal()));
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    assert.throws(() => baseParseRecoveryJournal(rawJournal()), /invalid retained recovery clone journal/);
  });
});

describe("installCompletionSourceFence", () => {
  it("is idempotent for an identical fence and refuses a different one", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    const before = rawJournal();
    const again = await cache.installCompletionSourceFence(bare, BRANCH, expected(), { ...FENCE });
    assert.equal(serializeSourceFence(again), serializeSourceFence(FENCE));
    assert.equal(rawJournal(), before);
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, expected(), OTHER), /different fence/);
    assert.equal(rawJournal(), before);
  });

  it("replaces a different fence only when the caller names that exact old fence", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    const before = rawJournal();
    // Naming a fence the journal does not carry changes nothing, and neither does naming none.
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, expected(), OTHER, { replaceStale: { expectedOld: OTHER } }), /different fence/);
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, expected(), OTHER, {}), /different fence/);
    assert.equal(rawJournal(), before);
    assert.equal(serializeSourceFence((await cache.readCompletionSourceFence(bare, BRANCH, expected()))!), serializeSourceFence(FENCE));
    const back = await cache.installCompletionSourceFence(bare, BRANCH, expected(), OTHER, { replaceStale: { expectedOld: { ...FENCE } } });
    assert.equal(serializeSourceFence(back), serializeSourceFence(OTHER));
    assert.equal(JSON.stringify(journal().completionSourceFence), serializeSourceFence(OTHER));
    assert.equal(Object.keys(journal()).at(-1), "completionSourceFence", "the fence stays the last key");
    // Every other refusal still applies to a replacement.
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, { runId: "run-other", clonePath }, FENCE, { replaceStale: { expectedOld: OTHER } }), /does not name this source/);
  });

  it("refuses a missing journal, a different path and a different attempt without writing", async () => {
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, { runId: RUN, clonePath: clonePath + "-x" }, FENCE), /does not name this source/);
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, { runId: RUN, clonePath, attemptId: aid(2) }, FENCE), /does not name this source/);
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, { runId: "run-other", clonePath }, FENCE), /does not name this source/);
    gitIn(bare, ["config", `uzi-recovery.${BRANCH}.clone`, ""]);
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE), /does not name this source/);
    assert.equal(gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]), "");
  });

  it("refuses a recovery episode and a protected descriptor", async () => {
    const source = reconstructionSource();
    await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source);
    const episode = rawJournal();
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE), /recovery episode/);
    assert.equal(rawJournal(), episode);
    gitIn(bare, ["config", `uzi-recovery.${BRANCH}.clone`, JSON.stringify({ runId: RUN, clonePath })]);
    gitIn(bare, ["config", `uzi-retained.${RUN}.journal`, "{not json"]);
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE));
    assert.equal(journal().completionSourceFence, undefined);
  });

  it("settles the config file and then the bare directory before returning", async () => {
    const synced: string[] = [];
    const out = await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE, {
      sync: async (handle, what) => { synced.push(what); await handle.sync(); },
    });
    assert.deepEqual(synced, ["config", "directory"]);
    assert.equal(serializeSourceFence(out), serializeSourceFence(FENCE));
  });

  for (const failing of ["config", "directory"] as const) {
    it(`returns no success when the ${failing} sync fails`, async () => {
      let returned: unknown;
      await assert.rejects(async () => {
        returned = await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE, {
          sync: async (_handle, what) => { if (what === failing) throw new Error("injected sync failure"); },
        });
      }, /injected sync failure/);
      assert.equal(returned, undefined);
    });
  }
});

describe("fence preserved by every reconstruction of the SAME source, never inherited by another", () => {
  it("commits owner accounting when reserving a fenced source without an existing recovery field", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    assert.equal(journal().recovery, undefined);

    const reservation = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, reconstructionSource(), 1_000, 1, 1, 0);
    assert.equal(reservation.stage, "capturing");
    const owner = JSON.parse(gitIn(bare, ["config", "--get", `uzi-recoveryowner.${RUN}.record`])) as { state: string };
    assert.equal(owner.state, "committed");
    assert.equal(JSON.stringify(journal().completionSourceFence), serializeSourceFence(FENCE));
    assert.equal(Object.keys(journal()).at(-1), "completionSourceFence");
  });

  it("survives reserve and capture of the same source", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    const source = reconstructionSource();
    const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source);
    assert.equal(JSON.stringify(journal().completionSourceFence), serializeSourceFence(FENCE));
    const head = gitIn(clonePath, ["rev-parse", "HEAD"]);
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, source, r.attempts, head);
    assert.equal(JSON.stringify(journal().completionSourceFence), serializeSourceFence(FENCE));
    assert.equal(Object.keys(journal()).at(-1), "completionSourceFence");
  });

  it("a successor journal carries no stale fence and can install its own once the predecessor is gone", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    const source = reconstructionSource();
    const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source);
    const head = gitIn(clonePath, ["rev-parse", "HEAD"]);
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, source, r.attempts, head);
    const successor = await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source, r.attempts, aid(2));
    const after = journal() as unknown as RecoveryJournalEntry;
    assert.equal(after.clonePath, successor.path, "the journal was replaced by the successor's");
    assert.equal(after.recovery?.stage, "ready-for-model");
    assert.equal(after.completionSourceFence, undefined, "the predecessor's fence is not the successor's");
    await cache.completeRecoveryEpisode(bare, BRANCH, KEY, { runId: RUN, clonePath: successor.path, attemptId: aid(2) },
      { settled: true, processedEvents: 1, attemptId: aid(2) });
    assert.equal(journal().completionSourceFence, undefined);
    assert.ok(Array.isArray(journal().retainedSources), "retained source rebuild still names the predecessor");

    // Predecessor disposed (the installer refuses a journal that still names one): the successor installs its own fence.
    const { retainedSources: _gone, ...plain } = journal();
    gitIn(bare, ["config", `uzi-recovery.${BRANCH}.clone`, JSON.stringify(plain)]);
    const mine = { runId: RUN, clonePath: successor.path, attemptId: aid(2) };
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, { ...mine, runId: "run-other" }, OTHER), /does not name this source/);
    const installed = await cache.installCompletionSourceFence(bare, BRANCH, mine, OTHER);
    assert.equal(serializeSourceFence(installed), serializeSourceFence(OTHER));
    assert.equal(JSON.stringify(journal().completionSourceFence), serializeSourceFence(OTHER));
  });
});

describe("completion retirement consumes the fence", () => {
  let events: string[];
  let strict: GitCache;
  const head = () => gitIn(clonePath, ["rev-parse", "HEAD"]);
  const retire = (fence?: CompletionSourceFence) => strict.retireRunnerClone(bare, clonePath, BRANCH, RUN, {
    discard: true,
    completionSource: { expectedHead: head(), canDelete: () => true, ...(fence ? { expectedFence: fence } : {}) },
  });

  beforeEach(() => {
    events = [];
    strict = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ authority: recorder(events), authorityRequired: true }));
  });

  it("refuses a missing expected fence, a missing journal fence and a mismatched fence before any effect", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    const before = fs.readFileSync(path.join(bare, "config"));
    await assert.rejects(retire(), /fence missing or changed/);
    await assert.rejects(retire(OTHER), /fence missing or changed/);
    await assert.rejects(retire({ ...FENCE, sourceUseEpoch: "1123abcd-0123-4abc-8def-0123456789ab" }), /fence missing or changed/);
    assert.ok(fs.existsSync(path.join(clonePath, ".git")), "the clone is untouched");
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before, "the journal is untouched");
    assert.deepEqual(events.filter(e => e.startsWith("destructive:")), []);
  });

  it("refuses when the journal carries no fence at all", async () => {
    await assert.rejects(retire(FENCE), /fence missing or changed/);
    assert.ok(fs.existsSync(path.join(clonePath, ".git")));
    assert.ok(rawJournal().includes(RUN));
  });

  it("retires with the exact fence and the journal clear removes it", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    await retire({ ...FENCE });
    assert.equal(fs.existsSync(clonePath), false);
    assert.equal(gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]), "");
  });

  it("the legacy path (no hooks, not required, no expected fence) still retires", async () => {
    await cache.retireRunnerClone(bare, clonePath, BRANCH, RUN, {
      discard: true, completionSource: { expectedHead: head(), canDelete: () => true },
    });
    assert.equal(fs.existsSync(clonePath), false);
  });
});

describe("a fenced journal is never cleared or rewritten without its fence by a non-certified path", () => {
  let strict: GitCache;
  const configBytes = (): Buffer => fs.readFileSync(path.join(bare, "config"));
  const unfence = (): void => {
    const { completionSourceFence: _drop, ...plain } = journal();
    gitIn(bare, ["config", `uzi-recovery.${BRANCH}.clone`, JSON.stringify(plain)]);
  };

  beforeEach(async () => {
    strict = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ authority: recorder([]), authorityRequired: true }));
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
  });

  it("orphan reclaim (retireRunnerClone without a completion source) retains the clone and the journal", async () => {
    const before = configBytes();
    await assert.rejects(strict.retireRunnerClone(bare, clonePath, BRANCH, RUN, { discard: false }), /completion source fence/);
    assert.ok(fs.existsSync(path.join(clonePath, ".git")), "the clone is untouched");
    assert.deepEqual(configBytes(), before, "the journal is untouched");
    // The same call on a journal without a fence still reclaims, so the refusal is the fence's.
    unfence();
    await strict.retireRunnerClone(bare, clonePath, BRANCH, RUN, { discard: false });
    assert.equal(gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]), "");
  });

  it("releaseAttemptInPlace refuses before the ledger append or the clear", async () => {
    const before = configBytes();
    await assert.rejects(strict.releaseAttemptInPlace(bare, clonePath, BRANCH, RUN, "reclaimed"), /completion source fence/);
    assert.deepEqual(configBytes(), before);
    unfence();
    await strict.releaseAttemptInPlace(bare, clonePath, BRANCH, RUN, "reclaimed");
    assert.equal(gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]), "");
  });

  it("stopped-source retention answers unknown and writes nothing", async () => {
    const before = configBytes();
    assert.equal(await strict.markStoppedCanonical(bare, BRANCH, KEY, { runId: RUN, clonePath, generation: null, reason: "test" }), "unknown");
    assert.deepEqual(configBytes(), before);
    unfence();
    assert.equal(await strict.markStoppedCanonical(bare, BRANCH, KEY, { runId: RUN, clonePath, generation: null, reason: "test" }), "retained");
  });

  it("adopting a successor over a fenced episode refuses before any effect (the successor object carries no fence)", async () => {
    const source = reconstructionSource();
    const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source);
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, source, r.attempts, gitIn(clonePath, ["rev-parse", "HEAD"]));
    const before = configBytes();
    const entries = fs.readdirSync(path.dirname(clonePath));
    await assert.rejects(strict.prepareRecoverySuccessor(bare, BRANCH, KEY, source, r.attempts, aid(2)), /completion source fence/);
    assert.deepEqual(configBytes(), before);
    assert.deepEqual(fs.readdirSync(path.dirname(clonePath)), entries, "no successor clone was created");
  });

  describe("survivors of the non-certified paths", () => {
    const AB = "agent/issue-2513", AKEY = "issue-2513", ARUN = "run-2513";
    const aGit = (args: string[]): string => gitIn(bare, args);
    const aRaw = (): string => aGit(["config", "--get", `uzi-recovery.${AB}.clone`]);
    const aUnfence = (): void => {
      const { completionSourceFence: _drop, ...plain } = JSON.parse(aRaw()) as Record<string, unknown>;
      aGit(["config", `uzi-recovery.${AB}.clone`, JSON.stringify(plain)]);
    };

    /** A real attempt clone whose journal is fenced; the ledger says `live`. */
    async function fencedAttempt(): Promise<{ path: string; attemptId: string }> {
      const attemptId = aid(5);
      const info = await cache.runnerCloneForBranch(bare, AB, AKEY, {
        isLive: () => false, beforeFree: async () => { throw new Error("unexpected incumbent"); },
      }, ARUN, false, undefined, { attemptId, isLive: () => false, beforeSeed: async () => {} });
      await cache.markRecoveryCapture(bare, info.path, AB, ARUN, attemptId);
      await cache.installCompletionSourceFence(bare, AB, { runId: ARUN, clonePath: info.path, attemptId }, FENCE);
      return { path: info.path, attemptId };
    }
    const retireLedger = (c: { path: string; attemptId: string }): Promise<void> =>
      (cache as unknown as { appendAttemptLedger(b: string, br: string, e: unknown): Promise<void> })
        .appendAttemptLedger(bare, AB, { attemptId: c.attemptId, runId: ARUN, clonePath: c.path, state: "retired" });

    it("retainStoppedAttempt answers unknown on a fenced attempt journal and leaves the config alone", async () => {
      const c = await fencedAttempt();
      const before = configBytes();
      assert.equal(await strict.retainStoppedAttempt(bare, AB, { runId: ARUN, clonePath: c.path, attemptId: c.attemptId }), "unknown");
      assert.deepEqual(configBytes(), before, "neither the ledger append nor the journal clear happened");
      aUnfence();
      assert.equal(await strict.retainStoppedAttempt(bare, AB, { runId: ARUN, clonePath: c.path, attemptId: c.attemptId }), "retained");
      assert.equal(aGit(["config", "--get", `uzi-recovery.${AB}.clone`]), "");
    });

    it("the non-certified already-retired check refuses a fenced journal; unfenced it retires and clears", async () => {
      const c = await fencedAttempt();
      await retireLedger(c);
      fs.rmSync(c.path, { recursive: true, force: true });
      const before = configBytes();
      assert.equal(await strict.completionSourceAlreadyRetired(bare, AB, ARUN, c.path, c.attemptId), false);
      assert.deepEqual(configBytes(), before, "the fenced journal is untouched");
      aUnfence();
      assert.equal(await strict.completionSourceAlreadyRetired(bare, AB, ARUN, c.path, c.attemptId), true);
      assert.equal(aGit(["config", "--get", `uzi-recovery.${AB}.clone`]), "");
    });

    it("healBoundStoppedJournal does not clear a fenced journal even when a marker's digest matches its raw value", async () => {
      const marker = (raw: string): string => JSON.stringify({ version: 1, runId: RUN, generation: null, branch: BRANCH, key: KEY, clonePath,
        journalDigest: createHash("sha256").update(raw).digest("hex"), reason: "test", at: "2026-10-27T00:00:00.000Z" });
      const markerKey = `uzi-stopped.${RUN}.entry`;
      gitIn(bare, ["config", "--add", markerKey, marker(rawJournal())]);
      const before = configBytes();
      await assert.rejects(strict.discoverRetainedRecovery(fx.originPath, BRANCH, KEY, RUN));
      assert.deepEqual(configBytes(), before, "the fenced journal and the marker are untouched");
      // The same state without the fence heals: the refusal above is the fence's.
      unfence();
      gitIn(bare, ["config", "--unset-all", markerKey]);
      gitIn(bare, ["config", "--add", markerKey, marker(rawJournal())]);
      assert.equal(await strict.discoverRetainedRecovery(fx.originPath, BRANCH, KEY, RUN), undefined);
      assert.equal(gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]), "");
    });

    it("the public discardRetainedRecovery refuses a fenced journal before any effect", async () => {
      const before = configBytes();
      await assert.rejects(strict.discardRetainedRecovery(bare, BRANCH, KEY, { runId: RUN, clonePath }, true), /completion source fence/);
      assert.deepEqual(configBytes(), before);
    });

    describe("clearRetiredCompletionJournal (the certified crash-resume clear)", () => {
      const RET = { retiring: true };
      const clear = (c: GitCache, fence: CompletionSourceFence = FENCE) => c.clearRetiredCompletionJournal(bare, BRANCH, RUN, clonePath, undefined, fence, RET);

      it("canonical: clears only when the clone is gone and the journal names the run and path with exactly the expected fence", async () => {
        const before = configBytes();
        assert.equal(await clear(strict), false, "the clone is still there");
        fs.rmSync(clonePath, { recursive: true, force: true });
        assert.equal(await clear(strict, OTHER), false, "a different fence");
        assert.equal(await strict.clearRetiredCompletionJournal(bare, BRANCH, "run-other", clonePath, undefined, FENCE, RET), false, "a different run");
        assert.equal(await strict.clearRetiredCompletionJournal(bare, BRANCH, RUN, clonePath, aid(2), FENCE, RET), false, "a different attempt");
        // c2: a different absent clone path with the correct fence/run/attempt names no journal.
        assert.equal(await strict.clearRetiredCompletionJournal(bare, BRANCH, RUN, `${clonePath}-elsewhere`, undefined, FENCE, RET), false, "a different clone path");
        assert.deepEqual(configBytes(), before, "nothing changed");
        assert.equal(await clear(strict), true);
        assert.equal(gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]), "");
      });

      it("canonical: absence alone is not attribution; without the retiring authority nothing is cleared", async () => {
        fs.rmSync(clonePath, { recursive: true, force: true });
        const before = configBytes();
        assert.equal(await strict.clearRetiredCompletionJournal(bare, BRANCH, RUN, clonePath, undefined, FENCE), false);
        assert.deepEqual(configBytes(), before);
      });

      it("canonical: a journal without a fence is not this step's to clear", async () => {
        unfence();
        fs.rmSync(clonePath, { recursive: true, force: true });
        const before = configBytes();
        assert.equal(await clear(strict), false);
        assert.deepEqual(configBytes(), before);
      });

      it("attempt: needs the ledger to say retired", async () => {
        const c = await fencedAttempt();
        fs.rmSync(c.path, { recursive: true, force: true });
        const before = configBytes();
        const call = (fence = FENCE) => strict.clearRetiredCompletionJournal(bare, AB, ARUN, c.path, c.attemptId, fence);
        assert.equal(await call(), false, "ledger still live");
        assert.deepEqual(configBytes(), before);
        await retireLedger(c);
        const withLedger = configBytes();
        assert.equal(await call(OTHER), false, "a different fence");
        assert.deepEqual(configBytes(), withLedger);
        // c3: an undefined attempt against the attempt journal names a different source.
        assert.equal(await strict.clearRetiredCompletionJournal(bare, AB, ARUN, c.path, undefined, FENCE, RET), false, "no attempt id");
        assert.deepEqual(configBytes(), withLedger);
        assert.equal(await call(), true);
        assert.equal(aGit(["config", "--get", `uzi-recovery.${AB}.clone`]), "");
      });

      it("attempt: with the retiring authority a still-live ledger entry (crash before the ledger append) completes and appends retired", async () => {
        const c = await fencedAttempt();
        fs.rmSync(c.path, { recursive: true, force: true });
        const before = configBytes();
        assert.equal(await strict.clearRetiredCompletionJournal(bare, AB, ARUN, c.path, c.attemptId, FENCE), false, "no retiring authority");
        assert.equal(await strict.clearRetiredCompletionJournal(bare, AB, ARUN, c.path, c.attemptId, OTHER, RET), false, "a different fence");
        assert.equal(await strict.clearRetiredCompletionJournal(bare, AB, "run-other", c.path, c.attemptId, FENCE, RET), false, "a different run");
        assert.deepEqual(configBytes(), before, "nothing changed");
        assert.equal(await strict.clearRetiredCompletionJournal(bare, AB, ARUN, c.path, c.attemptId, FENCE, RET), true);
        assert.equal(aGit(["config", "--get", `uzi-recovery.${AB}.clone`]), "");
        assert.match(aGit(["config", "--get-all", `uzi-attempts.${AB}.entry`]), /"state":"retired"/, "the ledger now says retired");
      });

      it("attempt: an abandoned ledger entry never qualifies even with the retiring authority", async () => {
        const c = await fencedAttempt();
        await (cache as unknown as { appendAttemptLedger(b: string, br: string, e: unknown): Promise<void> })
          .appendAttemptLedger(bare, AB, { attemptId: c.attemptId, runId: ARUN, clonePath: c.path, state: "abandoned" });
        fs.rmSync(c.path, { recursive: true, force: true });
        const before = configBytes();
        assert.equal(await strict.clearRetiredCompletionJournal(bare, AB, ARUN, c.path, c.attemptId, FENCE, RET), false);
        assert.deepEqual(configBytes(), before);
      });
    });

    it("detachTerminalRetained copies the fence into the protected descriptor, which the captured base parser rejects", async () => {
      const source = reconstructionSource();
      const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source);
      await cache.recordRecoveryCapture(bare, BRANCH, KEY, source, r.attempts, gitIn(clonePath, ["rev-parse", "HEAD"]));
      const active = journal() as unknown as RecoveryJournalEntry;
      assert.deepEqual(active.completionSourceFence, FENCE);
      await cache.detachTerminalRetained(bare, BRANCH, KEY, active);
      assert.equal(gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]), "", "the active key is cleared");
      const descriptor = JSON.parse(gitIn(bare, ["config", "--get", `uzi-retained.${RUN}.journal`])) as { journal: unknown };
      assert.deepEqual((descriptor.journal as RecoveryJournalEntry).completionSourceFence, FENCE, "the fence rides in the descriptor");
      assert.throws(() => baseParseRecoveryJournal(JSON.stringify(descriptor.journal)), /invalid retained recovery clone journal/);
    });
  });
});

describe("authority hooks", () => {
  it("authorityRequired without hooks refuses the seed before any effect", async () => {
    const required = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authorityRequired: true }));
    const entries = fs.readdirSync(path.dirname(clonePath));
    await assert.rejects(required.createOrAttachRunnerClone(bare, 2601, noProofReseed, "run-other"), /authority hooks required/);
    assert.deepEqual(fs.readdirSync(path.dirname(clonePath)), entries);
    assert.ok(fs.existsSync(path.join(clonePath, ".git")), "the existing clone was not freed");
    await assert.rejects(required.createOrAttachRunnerClone(bare, 2600, noProofReseed, "run-26", false, undefined,
      { attemptId: aid(3), beforeSeed: async () => {}, isLive: () => false }), /authority hooks required/);
    assert.equal(fs.readdirSync(path.dirname(clonePath)).some(n => n.includes(aid(3))), false);
  });

  it("an admitSourceUse refusal aborts the seed; an admission names the op and path", async () => {
    let events: string[] = [];
    const refusing = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
      authority: recorder(events, { admitSourceUse: async (p, op) => { events.push(`admit:${op}`); throw new Error(`refused ${path.basename(p)}`); } }),
    }));
    const target = fs.readdirSync(path.dirname(clonePath));
    await assert.rejects(refusing.createOrAttachRunnerClone(bare, 2700, noProofReseed, "run-27"), /refused issue-2700/);
    assert.deepEqual(events, ["clear:seed-canonical", "admit:seed-canonical"]);
    assert.deepEqual(fs.readdirSync(path.dirname(clonePath)), target);
    const admitting = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: recorder(events = []) }));
    await admitting.createOrAttachRunnerClone(bare, 2700, noProofReseed, "run-27");
    assert.deepEqual(events, ["clear:seed-canonical", "admit:seed-canonical:issue-2700"]);
  });

  it("an unresolved bare mutation refuses a seed before admission: canonical, attempt and replace", async () => {
    const events: string[] = [];
    const blocked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
      authority: recorder(events, { assertBareClear: async (_b, op) => { events.push(`clear:${op}`); throw new Error("bare not clear"); } }),
    }));
    const before = fs.readdirSync(path.dirname(clonePath));
    await assert.rejects(blocked.createOrAttachRunnerClone(bare, 2800, noProofReseed, "run-28"), /bare not clear/);
    await assert.rejects(blocked.createOrAttachRunnerClone(bare, 2801, noProofReseed, "run-2801", false, undefined,
      { attemptId: aid(4), beforeSeed: async () => {}, isLive: () => false }), /bare not clear/);
    assert.deepEqual(fs.readdirSync(path.dirname(clonePath)), before, "no clone, ledger entry or directory was created");

    // replace: a reserved, captured source with a successor prepared against it.
    const source = reconstructionSource();
    const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source);
    const head = gitIn(clonePath, ["rev-parse", "HEAD"]);
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, source, r.attempts, head);
    const journalBefore = rawJournal();
    await assert.rejects(blocked.prepareRecoverySuccessor(bare, BRANCH, KEY, source, r.attempts, aid(2)), /bare not clear/);
    assert.equal(rawJournal(), journalBefore, "the journal was not rewritten");
    assert.equal(fs.readdirSync(path.dirname(clonePath)).some(n => n.includes(aid(2))), false);

    assert.deepEqual(events, ["clear:seed-canonical", "clear:seed-attempt", "clear:replace"], "refused before any admitSourceUse");
  });

  describe("shared-bare mutation", () => {
    const base = () => gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    let context: PositiveOwedCandidateContext;
    let pinned: string;
    const OWED_RUN = "19241924-1111-2222-3333-444444444444";
    const BR = "agent/issue-1924";

    let cloneDir: string;
    /** A new head on BR in the real clone, on top of `pinned`. */
    const commitNewHead = (): string => {
      const head = gitIn(cloneDir, [...IDENT, "commit-tree", `${pinned}^{tree}`, "-m", "next", "-p", pinned]);
      gitIn(cloneDir, ["update-ref", `refs/heads/${BR}`, head, pinned]);
      return head;
    };
    const newHeadOptions = (): FetchAgentBranchOptions => ({ context: { runId: OWED_RUN, generation: 1, branch: BR, kind: "issue",
      barePath: bare, defaultIdentity: { ref: "refs/remotes/origin/main", sha: base() } } });

    beforeEach(async () => {
      const dir = (await cache.runnerCloneForBranch(bare, BR, "issue-1924", noProofReseed, OWED_RUN)).path;
      cloneDir = dir;
      const tree = gitIn(dir, ["rev-parse", `${base()}^{tree}`]);
      pinned = gitIn(dir, [...IDENT, "commit-tree", tree, "-m", "pinned", "-p", base()]);
      gitIn(dir, ["update-ref", `refs/heads/${BR}`, pinned]);
      const opts: FetchAgentBranchOptions = { context: { runId: OWED_RUN, generation: 1, branch: BR, kind: "issue", barePath: bare,
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: base() } } };
      const result = await cache.fetchAgentBranch(bare, dir, BR, OWED_RUN, opts);
      assert.equal(result.kind, "updated");
      context = opts.context as PositiveOwedCandidateContext;
    });

    const isDelete = (args: string[]) => args.includes("update-ref") && args.includes("-d") ? "update-ref-d" : undefined;

    it("reconcile registers before update-ref -d and settles after the child is joined", async () => {
      const events: string[] = [];
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: recorder(events) }));
      watchGit(hooked, isDelete, events);
      const result = await hooked.reconcileOwedCandidates(bare, OWED_RUN, pinned);
      assert.deepEqual(result.removedShas, [pinned]);
      assert.deepEqual(events, [
        `bare:owed-reconcile-delete-ref:refs/uzi-owed/${OWED_RUN}/${pinned}`,
        "git:update-ref-d",
        `settle:owed-reconcile-delete-ref:{"ok":true}`,
      ]);
    });

    it("an unjoined child (rejected update-ref) is never settled and the pin stays", async () => {
      const events: string[] = [];
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: recorder(events) }));
      watchGit(hooked, isDelete, events, Object.assign(new Error("timed out"),
        { code: null, killed: true, signal: "SIGTERM", stdout: "", stderr: "" }));
      await assert.rejects(hooked.reconcileOwedCandidates(bare, OWED_RUN, pinned));
      assert.deepEqual(events.map(e => e.split(":")[0]), ["bare", "git"]);
      assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/${OWED_RUN}/${pinned}`]), pinned);
    });

    /** Intercept `update-ref -d`: optionally run the real child first, then fail; optionally fail the read-back. */
    function interceptDelete(c: GitCache, events: string[], plan: { error?: Error; runFirst?: boolean; readError?: boolean }): void {
      const internals = c as unknown as Internals;
      const original = internals.execScoped.bind(c);
      let deleted: string | undefined;
      internals.execScoped = async (command, args, options) => {
        if (command === "git" && isDelete(args)) {
          events.push("git:update-ref-d");
          if (plan.error && !plan.runFirst) throw plan.error;
          const out = await original(command, args, options);
          deleted = args[args.indexOf("-d") + 1];
          if (plan.error) throw plan.error;
          return out;
        }
        if (plan.readError && deleted && command === "git" && args.includes("for-each-ref") && args.includes(deleted)) {
          throw new Error("read-back boom");
        }
        return original(command, args, options);
      };
    }
    const childError = (over: object): Error => Object.assign(new Error("child failed"), { stdout: "", stderr: "", ...over });

    type Site = { name: string; op: string; prepare(h: GitCache): Promise<() => Promise<unknown>> };
    const sites: Site[] = [
      { name: "reconcileOwedCandidates", op: "owed-reconcile-delete-ref",
        prepare: async (h) => () => h.reconcileOwedCandidates(bare, OWED_RUN, pinned) },
      { name: "cleanupRecoveryGeneration", op: "recovery-cleanup-delete-ref",
        prepare: async (h) => {
          const frozen = await h.enumerateOwedCandidates(bare, OWED_RUN);
          const coverage = await cache.buildRecoveryCoverage(bare, context, [pinned], pinned);
          gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${OWED_RUN}/1`, coverage.sha]);
          return () => h.cleanupRecoveryGeneration(bare, context, frozen,
            [{ fingerprint: coverage.fingerprint, sha: coverage.sha }], [pinned, coverage.sha], coverage.sha);
        } },
    ];
    type Outcome = "ok" | "residue" | "unsettled";
    const cases: Array<{ name: string; plan: Parameters<typeof interceptDelete>[2]; outcome: Outcome; reject?: RegExp | Function }> = [
      { name: "exit 0 and ref gone settles ok", plan: {}, outcome: "ok" },
      { name: "nonzero exit with the ref gone settles ok and rejects", plan: { error: childError({ code: 1 }), runFirst: true },
        outcome: "ok", reject: /git update-ref -d .* failed/ },
      { name: "nonzero exit with the ref remaining settles residue and rejects", plan: { error: childError({ code: 1 }) },
        outcome: "residue", reject: /git update-ref -d .* failed/ },
      { name: "a failed read-back settles residue and rejects", plan: { readError: true }, outcome: "residue", reject: /read-back boom/ },
      { name: "a spawn failure settles residue and rejects", plan: { error: childError({ code: "ENOENT", syscall: "spawn git" }) },
        outcome: "residue", reject: /git update-ref -d .* failed/ },
      { name: "a timeout kill stays unsettled", plan: { error: childError({ code: null, killed: true, signal: "SIGTERM" }) },
        outcome: "unsettled", reject: /git update-ref -d .* failed/ },
      { name: "a numeric code with killed stays unsettled", plan: { error: childError({ code: 1, killed: true }), runFirst: true },
        outcome: "unsettled", reject: /git update-ref -d .* failed/ },
      { name: "a signal death stays unsettled", plan: { error: childError({ code: 1, signal: "SIGKILL" }) },
        outcome: "unsettled", reject: /git update-ref -d .* failed/ },
      { name: "a non-spawn string code (output limit) stays unsettled",
        plan: { error: childError({ code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER" }), runFirst: true },
        outcome: "unsettled", reject: /git update-ref -d .* failed/ },
      { name: "a spawn-syscall string code with killed stays unsettled",
        plan: { error: childError({ code: "ENOENT", syscall: "spawn git", killed: true }) },
        outcome: "unsettled", reject: /git update-ref -d .* failed/ },
      { name: "a nonzero exit with a failed read-back settles residue and rejects with the child's error",
        plan: { error: childError({ code: 1 }), runFirst: true, readError: true }, outcome: "residue",
        reject: (e: unknown) => {
          assert.match((e as Error).message, /git update-ref -d .* failed/);
          assert.doesNotMatch((e as Error).message, /read-back boom/);
          return true;
        } },
      { name: "a boundary abort propagates unsettled", plan: { error: new GitBoundaryAbortError("aborted") },
        outcome: "unsettled", reject: GitBoundaryAbortError },
    ];
    for (const site of sites) {
      for (const c of cases) {
        it(`${site.name}: ${c.name}`, async () => {
          const events: string[] = [];
          const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: recorder(events) }));
          const run = await site.prepare(hooked);
          interceptDelete(hooked, events, c.plan);
          if (c.reject) await assert.rejects(run(), c.reject);
          else assert.ok(await run());
          const first = events.findIndex(e => e.startsWith("bare:"));
          const ref = events[first]!.slice(`bare:${site.op}:`.length);
          const settles = events.filter(e => e.startsWith("settle:"));
          if (c.outcome === "unsettled") {
            assert.deepEqual(events.map(e => e.split(":")[0]), ["bare", "git"]);
            return;
          }
          const want = c.outcome === "ok" ? `settle:${site.op}:{"ok":true}` : `settle:${site.op}:{"ok":false,"residue":["${ref}"]}`;
          assert.equal(settles[0], want);
          if (c.reject) assert.equal(settles.length, 1, "a rejected deletion stops after its own settlement");
        });
      }
    }

    it("a refused beginBareMutation aborts before update-ref -d", async () => {
      const events: string[] = [];
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events, { beginBareMutation: async () => { throw new Error("authority refused"); } }),
      }));
      watchGit(hooked, isDelete, events);
      await assert.rejects(hooked.reconcileOwedCandidates(bare, OWED_RUN, pinned), /authority refused/);
      assert.deepEqual(events, []);
      assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/${OWED_RUN}/${pinned}`]), pinned);
    });

    it("an assertBareClear refusal aborts coverage-ref renewal and pin renewal", async () => {
      const events: string[] = [];
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events, { assertBareClear: async (_b, op) => { events.push(`clear:${op}`); throw new Error("bare not clear"); } }),
      }));
      await assert.rejects(hooked.buildRecoveryCoverage(bare, context, [pinned], pinned), /bare not clear/);
      assert.equal(gitIn(bare, ["for-each-ref", `refs/uzi-coverage/${OWED_RUN}/`]), "");
      assert.ok(events.includes("clear:recovery-coverage-ref"));
      assert.equal(await hooked.anchorRecoveryHead(bare, OWED_RUN, 1, pinned), false);
      assert.equal(gitIn(bare, ["for-each-ref", "refs/uzi-recovery-pin/"]), "");
      assert.ok(events.includes("clear:recovery-pin-anchor"));
      const before = gitIn(bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-owed/"]);
      events.length = 0;
      commitNewHead();
      const result = await hooked.fetchAgentBranch(bare, cloneDir, BR, OWED_RUN, newHeadOptions());
      assert.deepEqual(result, { kind: "not_updated", reason: "preservation_failed" });
      assert.ok(events.includes("clear:owed-pin"), events.join("\n"));
      assert.equal(gitIn(bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-owed/"]), before);
    });

    it("control: the same fetch with non-refusing hooks reaches pin renewal", async () => {
      const events: string[] = [];
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: recorder(events) }));
      const newHead = commitNewHead();
      const result = await hooked.fetchAgentBranch(bare, cloneDir, BR, OWED_RUN, newHeadOptions());
      assert.equal(result.kind, "updated");
      assert.ok(events.includes("clear:owed-pin"), events.join("\n"));
      assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/${OWED_RUN}/${newHead}`]), newHead);
    });

    it("cleanupRecoveryGeneration registers each ref deletion before the child and settles after read-back", async () => {
      const events: string[] = [];
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: recorder(events) }));
      const frozen = await hooked.enumerateOwedCandidates(bare, OWED_RUN);
      const coverage = await cache.buildRecoveryCoverage(bare, context, [pinned], pinned);
      gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${OWED_RUN}/1`, coverage.sha]);
      watchGit(hooked, isDelete, events);
      assert.equal(await hooked.cleanupRecoveryGeneration(bare, context, frozen,
        [{ fingerprint: coverage.fingerprint, sha: coverage.sha }], [pinned, coverage.sha], coverage.sha), "removed");
      const deletes = events.filter(e => e.startsWith("bare:") || e.startsWith("git:") || e.startsWith("settle:"));
      assert.ok(deletes.length >= 6 && deletes.length % 3 === 0, deletes.join("\n"));
      for (let i = 0; i < deletes.length; i += 3) {
        assert.match(deletes[i]!, /^bare:recovery-cleanup-delete-ref:/);
        assert.equal(deletes[i + 1], "git:update-ref-d");
        assert.equal(deletes[i + 2], 'settle:recovery-cleanup-delete-ref:{"ok":true}');
      }
    });
  });

  describe("recovery pin deletion (N1)", () => {
    const OWED_RUN = "19241924-1111-2222-3333-444444444444";
    const PIN = `refs/uzi-recovery-pin/${OWED_RUN}/1`;
    let sha: string;
    const pinExists = (): boolean => gitIn(bare, ["for-each-ref", PIN]) !== "";
    beforeEach(async () => {
      sha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
      assert.equal(await cache.anchorRecoveryHead(bare, OWED_RUN, 1, sha), true);
      assert.equal(pinExists(), true);
    });
    const isDelete = (args: string[]) => args.includes("update-ref") && args.includes("-d") ? "update-ref-d" : undefined;
    const hookedWith = (events: string[], over: Partial<GitAuthorityHooks> = {}): GitCache =>
      new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events, {
          // The registration must precede the effect: record whether the pin still exists when it happens.
          beginBareMutation: async (_b, op, footprint) => {
            events.push(`bare:${op}:${footprint.join(",")}:pin=${pinExists()}`);
            return { settle: async (outcome) => { events.push(`settle:${op}:pin=${pinExists()}:${JSON.stringify(outcome)}`); } };
          },
          ...over,
        }),
      }));

    it("deleteRecoveryPin registers before the child and settles ok only after it is joined and the ref is gone", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events);
      watchGit(hooked, isDelete, events);
      await hooked.deleteRecoveryPin(bare, OWED_RUN, 1);
      assert.deepEqual(events, [
        `bare:recovery-pin-delete-ref:${PIN}:pin=true`, "git:update-ref-d", 'settle:recovery-pin-delete-ref:pin=false:{"ok":true}',
      ]);
      assert.equal(pinExists(), false);
    });

    it("deleteRecoveryPin leaves the record unsettled when the child has no real exit status", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events);
      watchGit(hooked, isDelete, events, Object.assign(new Error("timed out"), { code: "ETIMEDOUT", stdout: "", stderr: "" }));
      await hooked.deleteRecoveryPin(bare, OWED_RUN, 1);
      assert.deepEqual(events, [`bare:recovery-pin-delete-ref:${PIN}:pin=true`, "git:update-ref-d"]);
      assert.equal(pinExists(), true);
    });

    it("deleteRecoveryPin settles a joined nonzero exit that left the ref as residue", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events);
      watchGit(hooked, isDelete, events, Object.assign(new Error("exit 1"), { code: 1, stdout: "", stderr: "" }));
      await hooked.deleteRecoveryPin(bare, OWED_RUN, 1);
      assert.deepEqual(events, [
        `bare:recovery-pin-delete-ref:${PIN}:pin=true`, "git:update-ref-d",
        `settle:recovery-pin-delete-ref:pin=true:{"ok":false,"residue":["${PIN}"]}`,
      ]);
    });

    it("a refused registration deletes nothing", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events, { beginBareMutation: async () => { throw new Error("authority refused"); } });
      watchGit(hooked, isDelete, events);
      await assert.rejects(hooked.deleteRecoveryPin(bare, OWED_RUN, 1), /authority refused/);
      assert.deepEqual(events, []);
      assert.equal(pinExists(), true);
    });

    it("cleanupArchivedRecoveryPin registers before the prepared child and settles ok after a joined commit", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events);
      const result = await hooked.cleanupArchivedRecoveryPin({
        runId: OWED_RUN, generation: 1, sourceSha: sha, recoveryPinBareDir: path.basename(bare), isEligible: () => true,
      });
      assert.deepEqual(result, { state: "deleted" });
      assert.deepEqual(events, [
        `bare:recovery-pin-archive-delete-ref:${PIN}:pin=true`, 'settle:recovery-pin-archive-delete-ref:pin=false:{"ok":true}',
      ]);
    });

    it("cleanupArchivedRecoveryPin settles an aborted (uncommitted) child as ok only when the pin and lock are untouched", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events);
      let calls = 0;
      // The first eligibility check precedes the registration; the second runs while the child holds the prepared lock.
      const result = await hooked.cleanupArchivedRecoveryPin({
        runId: OWED_RUN, generation: 1, sourceSha: sha, recoveryPinBareDir: path.basename(bare), isEligible: () => ++calls < 2,
      });
      assert.deepEqual(result, { state: "retained", reason: "ineligible" });
      assert.deepEqual(events, [
        `bare:recovery-pin-archive-delete-ref:${PIN}:pin=true`, 'settle:recovery-pin-archive-delete-ref:pin=true:{"ok":true}',
      ]);
      assert.equal(fs.existsSync(path.join(bare, `${PIN}.lock`)), false);
    });

    it("cleanupArchivedRecoveryPin does not settle when the child's exit is unverified (-1)", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events);
      const internals = hooked as unknown as { spawnGit(...a: unknown[]): Promise<{ exited: Promise<number> }> };
      const real = internals.spawnGit.bind(hooked);
      internals.spawnGit = async (...a) => {
        const child = await real(...a);
        // Only the prepared delete child: the metadata probes must keep their real status.
        return (a[1] as string[]).includes("--stdin") ? Object.assign(child, { exited: child.exited.then(() => -1) }) : child;
      };
      await hooked.cleanupArchivedRecoveryPin({
        runId: OWED_RUN, generation: 1, sourceSha: sha, recoveryPinBareDir: path.basename(bare), isEligible: () => true,
      });
      assert.deepEqual(events, [`bare:recovery-pin-archive-delete-ref:${PIN}:pin=true`]);
    });

    it("deleteRecoveryPin settles a Node spawn failure (no child ever ran) as residue, but not a timeout", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events);
      watchGit(hooked, isDelete, events, Object.assign(new Error("spawn git ENOENT"), { code: "ENOENT", syscall: "spawn git", stdout: "", stderr: "" }));
      await hooked.deleteRecoveryPin(bare, OWED_RUN, 1);
      assert.deepEqual(events, [
        `bare:recovery-pin-delete-ref:${PIN}:pin=true`, "git:update-ref-d",
        `settle:recovery-pin-delete-ref:pin=true:{"ok":false,"residue":["${PIN}"]}`,
      ]);
      const killed: string[] = [];
      const timedOut = hookedWith(killed);
      watchGit(timedOut, isDelete, killed, Object.assign(new Error("timed out"), { code: "ENOENT", syscall: "spawn git", killed: true, stdout: "", stderr: "" }));
      await timedOut.deleteRecoveryPin(bare, OWED_RUN, 1);
      assert.deepEqual(killed, [`bare:recovery-pin-delete-ref:${PIN}:pin=true`, "git:update-ref-d"], "a killed child may have started: left open");
    });

    it("deleteRecoveryPin surfaces a rejected settlement as an authority error", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events, {
        beginBareMutation: async () => ({ settle: async () => { throw new Error("cannot write slot"); } }),
      });
      await assert.rejects(hooked.deleteRecoveryPin(bare, OWED_RUN, 1), /completion authority refused recovery-pin-delete-ref settlement: cannot write slot/);
    });

    it("cleanupArchivedRecoveryPin settles as residue when spawnGit rejects before any child exists", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events);
      const internals = hooked as unknown as { spawnGit(...a: unknown[]): Promise<unknown> };
      const real = internals.spawnGit.bind(hooked);
      internals.spawnGit = async (...a) => {
        if ((a[1] as string[]).includes("--stdin")) throw new Error("spawn EAGAIN");
        return real(...a);
      };
      const result = await hooked.cleanupArchivedRecoveryPin({
        runId: OWED_RUN, generation: 1, sourceSha: sha, recoveryPinBareDir: path.basename(bare), isEligible: () => true,
      });
      assert.deepEqual(result, { state: "retained", reason: "git_failure" });
      assert.deepEqual(events, [
        `bare:recovery-pin-archive-delete-ref:${PIN}:pin=true`, `settle:recovery-pin-archive-delete-ref:pin=true:{"ok":false,"residue":["${PIN}"]}`,
      ]);
      assert.equal(pinExists(), true);
    });

    it("cleanupArchivedRecoveryPin surfaces a rejected settlement as an authority error, not a retained pin", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events, {
        beginBareMutation: async () => ({ settle: async () => { throw new Error("cannot write slot"); } }),
      });
      await assert.rejects(hooked.cleanupArchivedRecoveryPin({
        runId: OWED_RUN, generation: 1, sourceSha: sha, recoveryPinBareDir: path.basename(bare), isEligible: () => true,
      }), /completion authority refused recovery-pin-archive-delete-ref settlement: cannot write slot/);
    });

    it("cleanupArchivedRecoveryPin retains the pin without spawning when registration is refused", async () => {
      const events: string[] = [];
      const hooked = hookedWith(events, { beginBareMutation: async () => { throw new Error("authority refused"); } });
      const result = await hooked.cleanupArchivedRecoveryPin({
        runId: OWED_RUN, generation: 1, sourceSha: sha, recoveryPinBareDir: path.basename(bare), isEligible: () => true,
      });
      assert.deepEqual(result, { state: "retained", reason: "ineligible" });
      assert.equal(pinExists(), true);
      assert.deepEqual(events, []);
    });
  });

  describe("destructive runner-tree delete", () => {
    /** A fake detached child: the rm runs when `run` is called, then the fake reports `finish`. */
    function fakeSpawn(events: string[], finish: (child: EventEmitter) => void, remove = true, pid: number | null = 4242) {
      return (_command: string, args: string[]) => {
        events.push("spawn");
        const child = Object.assign(new EventEmitter(), { stderr: new EventEmitter(), pid: pid ?? undefined });
        setImmediate(() => {
          if (remove) fs.rmSync(args.at(-1)!, { recursive: true, force: true });
          finish(child);
        });
        return child as unknown as import("node:child_process").ChildProcess;
      };
    }
    const reseed = (c: GitCache) => c.createOrAttachRunnerClone(bare, 2512, noProofReseed, "run-next");
    const free = async (): Promise<void> => {
      // the canonical journal would block a reseed; the seeded canonical clone is what gets freed
      gitIn(bare, ["config", `uzi-recovery.${BRANCH}.clone`, ""]);
    };

    it("registers before the detached child spawns and settles ok after a clean close", async () => {
      const events: string[] = [];
      await free();
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events),
        retentionDelete: { split: true, spawn: fakeSpawn(events, c => c.emit("close", 0, null)) },
      }));
      await reseed(hooked);
      const ours = events.filter(e => /^(destructive|spawn|settle)/.test(e));
      assert.deepEqual(ours, ["destructive:runner-uid-delete:issue-2512", "spawn", 'settle:runner-uid-delete:{"ok":true}']);
    });

    it("a joined failure settles with the target as residue", async () => {
      const events: string[] = [];
      await free();
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events),
        retentionDelete: { split: true, spawn: fakeSpawn(events, c => c.emit("close", 1, null), false) },
      }));
      await reseed(hooked); // a failed delete is quarantined in place by the caller, the reseed continues
      const settle = events.find(e => e.startsWith("settle:"));
      assert.match(settle ?? "", /^settle:runner-uid-delete:\{"ok":false,"residue":\["[^"]*issue-2512"\]\}$/);
    });

    it("a spawn 'error' event leaves the record unsettled", async () => {
      const events: string[] = [];
      await free();
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events),
        retentionDelete: { split: true, spawn: fakeSpawn(events, c => c.emit("error", new Error("spawn EAGAIN")), false) },
      }));
      await reseed(hooked);
      assert.deepEqual(events.filter(e => /^(destructive|spawn|settle)/.test(e)), ["destructive:runner-uid-delete:issue-2512", "spawn"]);
    });

    it("a spawn 'error' with no pid (the child never started) settles as a joined failure with the target as residue", async () => {
      const events: string[] = [];
      await free();
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events),
        retentionDelete: { split: true, spawn: fakeSpawn(events, c => c.emit("error", new Error("spawn EAGAIN")), false, null) },
      }));
      await reseed(hooked);
      const ours = events.filter(e => /^(destructive|spawn|settle)/.test(e));
      assert.equal(ours.length, 3, ours.join("\n"));
      assert.match(ours[2]!, /^settle:runner-uid-delete:\{"ok":false,"residue":\["[^"]*issue-2512"\]\}$/);
    });

    it("a spawn seam that throws synchronously settles {ok:false} with the target as residue, never ok", async () => {
      const events: string[] = [];
      await free();
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events),
        retentionDelete: { split: true, spawn: () => { events.push("spawn"); throw new Error("spawn threw synchronously"); } },
      }));
      await reseed(hooked);
      const settles = events.filter(e => e.startsWith("settle:"));
      assert.equal(settles.length, 1, settles.join("\n"));
      assert.match(settles[0]!, /^settle:runner-uid-delete:\{"ok":false,"residue":\["[^"]*issue-2512"\]\}$/);
    });

    it("a rejected settlement after a successful in-process delete is an authority error, not a failed delete", async () => {
      const events: string[] = [];
      await free();
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events, {
          beginDestructive: async (paths) => ({ settle: async (outcome) => {
            events.push(`settle:${JSON.stringify(outcome)}`); throw new Error(`cannot write ${path.basename(paths[0]!)}`);
          } }),
        }),
        retentionDelete: { split: true, run: async (_command: string, args: string[]) => { fs.rmSync(args.at(-1)!, { recursive: true, force: true }); } },
      }));
      await assert.rejects(reseed(hooked), /completion authority refused runner-uid-delete settlement: cannot write issue-2512/);
      assert.deepEqual(events.filter(e => e.startsWith("settle:")), ['settle:{"ok":true}']);
      assert.equal(fs.readdirSync(path.dirname(clonePath)).some(n => /residue/i.test(n)), false, "nothing was quarantined as a failed delete");
    });

    it("a rejected ok-settlement after the spawned rm exited 0 is an authority error, not a failed delete", async () => {
      const events: string[] = [];
      await free();
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events, {
          beginDestructive: async (paths) => ({ settle: async (outcome) => {
            events.push(`settle:${JSON.stringify(outcome)}`); throw new Error(`cannot write ${path.basename(paths[0]!)}`);
          } }),
        }),
        retentionDelete: { split: true, spawn: fakeSpawn(events, c => c.emit("close", 0, null)) },
      }));
      await assert.rejects(reseed(hooked), /completion authority refused runner-uid-delete settlement: cannot write issue-2512/);
      assert.deepEqual(events.filter(e => e.startsWith("settle:")), ['settle:{"ok":true}']);
      assert.equal(fs.readdirSync(path.dirname(clonePath)).some(n => /residue/i.test(n)), false, "nothing was quarantined as a failed delete");
    });

    it("a refused beginDestructive deletes nothing and never spawns", async () => {
      const events: string[] = [];
      await free();
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        authority: recorder(events, { beginDestructive: async () => { throw new Error("destructive refused"); } }),
        retentionDelete: { split: true, spawn: fakeSpawn(events, c => c.emit("close", 0, null)) },
      }));
      await assert.rejects(reseed(hooked), /destructive refused/);
      assert.equal(events.includes("spawn"), false);
      assert.ok(fs.existsSync(path.join(clonePath, ".git")));
    });
  });
});
