import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import path from "node:path";
import { GitCache, type FetchAgentBranchOptions, type PositiveOwedCandidateContext, type RecoveryJournalEntry } from "../src/git.js";
import type { AuthoritySettlement, GitAuthorityHooks } from "../src/git-authority-hooks.js";
import { serializeSourceFence, type CompletionSourceFence } from "../src/completion-source.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { parseRecoveryJournal as baseParseRecoveryJournal } from "./fixtures/base-parse-recovery-journal.js";

// issue #3404 M2 U2: the completion-source fence in the recovery journal, its install and its
// consumption by completion retirement, and the authority hooks GitCache consults before effects.
// Real git in temp dirs; the hooks are a recording fake.

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
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
const expected = () => ({ clonePath });
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

  it("refuses a missing journal, a different path and a different attempt without writing", async () => {
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, { clonePath: clonePath + "-x" }, FENCE), /does not name this source/);
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, { clonePath, attemptId: aid(2) }, FENCE), /does not name this source/);
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

describe("fence preserved by every journal reconstruction", () => {
  it("survives reserve, capture, prepareRecoverySuccessor and the successor replacement", async () => {
    await cache.installCompletionSourceFence(bare, BRANCH, expected(), FENCE);
    const source = reconstructionSource();
    const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source);
    assert.equal(JSON.stringify(journal().completionSourceFence), serializeSourceFence(FENCE));
    const head = gitIn(clonePath, ["rev-parse", "HEAD"]);
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, source, r.attempts, head);
    assert.equal(JSON.stringify(journal().completionSourceFence), serializeSourceFence(FENCE));
    const successor = await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source, r.attempts, aid(2));
    const after = journal() as unknown as RecoveryJournalEntry;
    assert.equal(after.clonePath, successor.path, "the journal was replaced by the successor's");
    assert.equal(after.recovery?.stage, "ready-for-model");
    assert.equal(JSON.stringify(after.completionSourceFence), serializeSourceFence(FENCE));
    assert.equal(Object.keys(after).at(-1), "completionSourceFence");
    await cache.completeRecoveryEpisode(bare, BRANCH, KEY, { runId: RUN, clonePath: successor.path, attemptId: aid(2) },
      { settled: true, processedEvents: 1, attemptId: aid(2) });
    assert.equal(JSON.stringify(journal().completionSourceFence), serializeSourceFence(FENCE));
    assert.ok(Array.isArray(journal().retainedSources), "retained source rebuild still names the predecessor");
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
    assert.deepEqual(events, ["admit:seed-canonical"]);
    assert.deepEqual(fs.readdirSync(path.dirname(clonePath)), target);
    const admitting = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: recorder(events = []) }));
    await admitting.createOrAttachRunnerClone(bare, 2700, noProofReseed, "run-27");
    assert.deepEqual(events, ["admit:seed-canonical:issue-2700"]);
  });

  describe("shared-bare mutation", () => {
    const base = () => gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    let context: PositiveOwedCandidateContext;
    let pinned: string;
    const OWED_RUN = "19241924-1111-2222-3333-444444444444";
    const BR = "agent/issue-1924";

    beforeEach(async () => {
      const dir = (await cache.runnerCloneForBranch(bare, BR, "issue-1924", noProofReseed, OWED_RUN)).path;
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
      watchGit(hooked, isDelete, events, Object.assign(new Error("timed out"), { code: 128, stdout: "", stderr: "" }));
      await assert.rejects(hooked.reconcileOwedCandidates(bare, OWED_RUN, pinned));
      assert.deepEqual(events.map(e => e.split(":")[0]), ["bare", "git"]);
      assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/${OWED_RUN}/${pinned}`]), pinned);
    });

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
      const second = gitIn(bare, [...IDENT, "commit-tree", `${pinned}^{tree}`, "-m", "second", "-p", pinned]);
      gitIn(bare, ["update-ref", "refs/uzi-test/second", second]);
      const before = gitIn(bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-owed/"]);
      await hooked.fetchAgentBranch(bare, path.join(fx.dataDir, "runner"), BR, OWED_RUN, {
        context: { runId: OWED_RUN, generation: 1, branch: BR, kind: "issue", barePath: bare,
          defaultIdentity: { ref: "refs/remotes/origin/main", sha: base() } } }).catch(() => undefined);
      assert.equal(gitIn(bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-owed/"]), before);
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

  describe("destructive runner-tree delete", () => {
    /** A fake detached child: the rm runs when `run` is called, then the fake reports `finish`. */
    function fakeSpawn(events: string[], finish: (child: EventEmitter) => void, remove = true) {
      return (_command: string, args: string[]) => {
        events.push("spawn");
        const child = Object.assign(new EventEmitter(), { stderr: new EventEmitter(), pid: 4242 });
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
