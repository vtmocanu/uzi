import { afterEach, beforeEach, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import promises from "node:fs/promises";
import path from "node:path";
import { attemptClonePath } from "../src/attempt-path.js";
import { GitCache, type RecoveryJournalEntry } from "../src/git.js";
import { PinRestoreDecodeBudget, type PinRestoreLink, type RetainedPinEnvelope } from "../src/pin-restore-journal.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { parseRecoveryJournal as baseParseRecoveryJournal } from "./fixtures/base-parse-recovery-journal.js";

// M2 reader proof only: metadata below is not an authenticated restore record,
// process-quiescence proof, execution lease, or permission to reuse an old source.
const RUN = "run-pin-metadata";
const BRANCH = "agent/issue-2512";
const KEY = "issue-2512";
const OLD_EPOCH = "0123abcd-0123-4abc-8def-0123456789ab";
const NEW_EPOCH = "1123abcd-0123-4abc-8def-0123456789ab";
const ATTEMPT = "20261008T171401Z-g2-0123456789abcdef";
const OLD_ATTEMPT = "20261008T171400Z-g1-fedcba9876543210";
const ACTIVE_KEY = `uzi-recovery.${BRANCH}.clone`;
const RETAINED_KEY = `uzi-retained.${RUN}.journal`;
const LEDGER_KEY = `uzi-attempts.${BRANCH}.entry`;
const FENCE = { version: 1 as const, sourceUseEpoch: OLD_EPOCH, sourceCoreDigest: "a".repeat(64) };
const ENV: NodeJS.ProcessEnv = Object.fromEntries(
  Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")),
);
ENV.GIT_CONFIG_GLOBAL = "/dev/null";
ENV.GIT_CONFIG_SYSTEM = "/dev/null";
const digest = (raw: string): string => createHash("sha256").update(raw).digest("hex");

let fx: Fixture;
let cache: GitCache;
let bare: string;
let canonical: string;
let oldRaw: string;
let oldJournal: RecoveryJournalEntry;
let link: PinRestoreLink;
let envelope: RetainedPinEnvelope;

function git(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" });
}
function config(key: string, raw: string): void {
  git(bare, ["config", "--local", key, raw]);
}
function rawConfig(key: string): string {
  // Remove Git's output delimiter only; raw journal whitespace is significant.
  return git(bare, ["config", "--local", "--get", key]).slice(0, -1);
}
function writeOld(extra: Partial<RecoveryJournalEntry> = {}, fenced = true): void {
  oldJournal = { runId: RUN, clonePath: canonical, sourceGeneration: 1, ...extra,
    ...(fenced ? { completionSourceFence: FENCE } : {}) };
  oldRaw = ` \n\t${JSON.stringify(oldJournal, null, 2)}\n \t`;
  config(ACTIVE_KEY, oldRaw);
}
function prepare(): void {
  const head = git(canonical, ["rev-parse", "HEAD"]).trim();
  const tree = git(canonical, ["rev-parse", "HEAD^{tree}"]).trim();
  link = {
    version: 1, intentId: "intent-metadata", predecessorJournalDigest: digest(oldRaw),
    predecessorGeneration: 1, predecessorEpoch: OLD_EPOCH,
    successor: {
      runId: RUN, claimGeneration: 2, workerId: "fixture-worker", registerNonce: "fixture-register",
      authorityIncarnation: NEW_EPOCH, executorIncarnation: null, sourceEpoch: NEW_EPOCH,
      attemptId: ATTEMPT, clonePath: attemptClonePath(cache.runnerClonePath(bare, KEY), ATTEMPT),
    },
    restore: {
      captureId: "fixture-capture", authenticatedRecordDigest: "b".repeat(64),
      predecessorGeneration: 1, bareDir: path.basename(bare),
      pinRef: `refs/uzi-recovery-pin/${RUN}/1`, tip: head, contentSource: head,
      expectedHead: head, expectedTree: tree,
    },
  };
  envelope = {
    version: 2, runId: RUN, branch: BRANCH, key: KEY,
    predecessors: [{ journalDigest: digest(oldRaw), rawJournal: oldRaw, generation: 1, sourceEpoch: OLD_EPOCH }],
    intent: { version: 1, phase: "prepared", link },
  };
  // A real ref preserves Git objects; its existence is deliberately not restore verification.
  git(bare, ["update-ref", link.restore.pinRef, head]);
}
function retain(): void {
  config(RETAINED_KEY, JSON.stringify(envelope));
}
function newJournal(): RecoveryJournalEntry {
  return { runId: RUN, clonePath: link.successor.clonePath, attemptId: ATTEMPT,
    sourceGeneration: 2, pinRestore: link };
}
function activate(): void {
  git(bare, ["config", "--local", "--add", LEDGER_KEY, JSON.stringify({
    attemptId: ATTEMPT, runId: RUN, clonePath: link.successor.clonePath, state: "live",
  })]);
  config(ACTIVE_KEY, JSON.stringify(newJournal()));
}
function snapshot() {
  return {
    config: fs.readFileSync(path.join(bare, "config")),
    refs: git(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]),
    readme: fs.readFileSync(path.join(canonical, "README.md")),
  };
}
async function ack(runId = RUN) {
  return cache.withReportProofBudget(
    { signal: new AbortController().signal, deadline: Date.now() + 30_000 },
    () => cache.withBareLock(bare, () => cache.readAcknowledgedReportSourcesUnderLock(bare, runId)),
  );
}

type Internals = {
  execScoped(command: string, args: string[], options: { cwd?: string }, identity?: unknown):
    Promise<{ stdout: string; stderr: string }>;
  runGitAsRunner(cwd: string | undefined, args: string[], options?: unknown): Promise<string>;
  pathPresent(p: string): Promise<boolean>;
  compactAttemptLedger(bare: string, branch: string): Promise<void>;
};

/** Throw AND record: production's fail-closed catches must not hide a forbidden probe. */
function guardSources(t: TestContext, roots = [canonical]) {
  const blocked = roots.flatMap(p => [p, path.join(path.dirname(p), `.uzi-skills-${path.basename(p)}`)]);
  const attempts: string[] = [];
  const observations: string[] = [];
  const protectedPath = (value: unknown): boolean => {
    const p = value instanceof URL ? value.pathname : Buffer.isBuffer(value) ? value.toString() : value;
    return typeof p === "string" && blocked.some(root => p === root || p.startsWith(root + path.sep));
  };
  const record = (kind: string, p: unknown): void => {
    observations.push(kind);
    if (protectedPath(p)) {
      attempts.push(`${kind}:${String(p)}`);
      throw new Error("forbidden predecessor probe");
    }
  };
  // git.ts imports this same default promises object.
  const lstat = promises.lstat;
  t.mock.method(promises, "lstat", async (...args: Parameters<typeof promises.lstat>) => {
    record("lstat", args[0]);
    return Reflect.apply(lstat, promises, args);
  });
  const readFile = promises.readFile;
  t.mock.method(promises, "readFile", async (...args: Parameters<typeof promises.readFile>) => {
    record("readFile", args[0]);
    return Reflect.apply(readFile, promises, args);
  });
  const open = promises.open;
  t.mock.method(promises, "open", async (...args: Parameters<typeof promises.open>) => {
    record("open", args[0]);
    return Reflect.apply(open, promises, args);
  });
  const readdir = promises.readdir;
  t.mock.method(promises, "readdir", async (...args: Parameters<typeof promises.readdir>) => {
    record("readdir", args[0]);
    return Reflect.apply(readdir, promises, args);
  });
  const internal = cache as unknown as Internals;
  const exec = internal.execScoped.bind(cache);
  t.mock.method(internal, "execScoped", async (command: string, args: string[], options: { cwd?: string }, identity?: unknown) => {
    record("exec-cwd", options.cwd);
    if (command === "git") {
      for (let i = 0; i < args.length; i++) {
        if (args[i] === "-C" || args[i] === "--git-dir" || args[i] === "--work-tree") record("git-dir", args[i + 1]);
        for (const prefix of ["--git-dir=", "--work-tree="]) {
          if (args[i]!.startsWith(prefix)) record("git-dir", args[i]!.slice(prefix.length));
        }
      }
    }
    return exec(command, args, options, identity);
  });
  const runner = internal.runGitAsRunner.bind(cache);
  t.mock.method(internal, "runGitAsRunner", async (cwd: string | undefined, args: string[], options?: unknown) => {
    record("runner-git", cwd);
    return runner(cwd, args, options);
  });
  const present = internal.pathPresent.bind(cache);
  t.mock.method(internal, "pathPresent", async (p: string) => {
    record("pathPresent", p);
    return present(p);
  });
  return {
    attempts, observations,
    isLivePath: (p: string) => { record("isLivePath", p); return false; },
    beforeTree: (p: string) => { record("beforeTree", p); },
    assertUntouched: () => assert.deepEqual(attempts, [], "predecessor paths must never be probed"),
  };
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  canonical = (await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, RUN)).path;
  await cache.markRecoveryCapture(bare, canonical, BRANCH, RUN);
  // Reader fixtures preserve deliberately noncanonical raw predecessor bytes.
  writeOld();
  prepare();
});
afterEach(() => fx.cleanup());

describe("retained pin metadata: exact predecessor bytes and phase relations", () => {
  for (const fenced of [false, true]) {
    it(`reads prepared-old with${fenced ? "" : "out"} a fence without touching either source`, async t => {
      writeOld({}, fenced);
      prepare();
      retain();
      const before = snapshot();
      const guard = guardSources(t, [canonical, link.successor.clonePath]);
      const records = await cache.readRetainedPinMetadata(bare);
      guard.assertUntouched();
      assert.equal(records.length, 1);
      assert.equal(records[0]!.relation, "prepared-old");
      assert.equal(records[0]!.activeRaw, oldRaw);
      assert.equal(records[0]!.originals[0]!.raw, oldRaw);
      assert.deepEqual(records[0]!.originals[0]!.journal, oldJournal);
      assert.equal(records[0]!.originals[0]!.sourceEpoch, OLD_EPOCH);
      assert.equal(records[0]!.originals[0]!.generation, 1);
      assert.equal(records[0]!.envelope.predecessors[0]!.journalDigest, digest(oldRaw));
      assert.equal(rawConfig(ACTIVE_KEY), oldRaw);
      assert.deepEqual(snapshot(), before);
      assert.ok(guard.observations.includes("lstat"), "the shared filesystem instrument must see bare checks");
    });
  }

  for (const phase of ["prepared", "ready"] as const) {
    it(`accepts ${phase}-new from a live ledger while the new path is absent and preserves the old source`, async t => {
      envelope.intent.phase = phase;
      activate();
      retain();
      assert.equal(fs.existsSync(link.successor.clonePath), false);
      const before = snapshot();
      const guard = guardSources(t, [canonical, link.successor.clonePath]);
      const records = await cache.readRetainedPinMetadata(bare);
      guard.assertUntouched();
      assert.equal(records.length, 1);
      assert.equal(records[0]!.relation, `${phase}-new`);
      assert.equal(records[0]!.activeRaw, JSON.stringify(newJournal()));
      assert.equal(records[0]!.originals[0]!.raw, oldRaw);
      assert.deepEqual(records[0]!.originals[0]!.journal.completionSourceFence, FENCE);
      assert.deepEqual(snapshot(), before);
    });
  }

  it("reports an absent active slot as metadata absence, preserving all predecessor evidence", async t => {
    retain();
    git(bare, ["config", "--local", "--unset-all", ACTIVE_KEY]);
    const before = snapshot();
    const guard = guardSources(t);
    const records = await cache.readRetainedPinMetadata(bare);
    guard.assertUntouched();
    assert.equal(records.length, 1);
    assert.equal(records[0]!.relation, "absent");
    assert.equal(records[0]!.activeRaw, undefined);
    assert.equal(records[0]!.originals[0]!.raw, oldRaw);
    assert.deepEqual(snapshot(), before);
  });
});

describe("retained pin metadata refuses ambiguous or inconsistent attribution before source access", () => {
  const cases: Array<[string, () => void]> = [
    ["ready envelope with old active slot", () => { envelope.intent.phase = "ready"; retain(); }],
    ["wrong exact-raw digest", () => { envelope.predecessors[0]!.journalDigest = "c".repeat(64); retain(); }],
    ["equivalent JSON with different raw whitespace", () => { config(ACTIVE_KEY, JSON.stringify(oldJournal)); }],
    ["unknown envelope key", () => { config(RETAINED_KEY, JSON.stringify({ ...envelope, extra: true })); }],
    ["unknown predecessor journal key", () => {
      oldRaw = JSON.stringify({ ...oldJournal, extra: true }); prepare(); retain(); config(ACTIVE_KEY, oldRaw);
    }],
    ["duplicate JSON key", () => {
      config(RETAINED_KEY, JSON.stringify(envelope).replace('"version":2', '"version":2,"version":2'));
    }],
    ["duplicate retained config values", () => {
      git(bare, ["config", "--local", "--add", RETAINED_KEY, JSON.stringify(envelope)]);
    }],
    ["duplicate active config values", () => { git(bare, ["config", "--local", "--add", ACTIVE_KEY, oldRaw]); }],
    ["wrong predecessor generation", () => { envelope.predecessors[0]!.generation = 2; retain(); }],
    ["wrong successor claim generation", () => { link.successor.claimGeneration = 3; retain(); }],
    ["wrong predecessor pin generation", () => { link.restore.pinRef = `refs/uzi-recovery-pin/${RUN}/2`; retain(); }],
    ["wrong bare repository identity", () => { link.restore.bareDir = "other.git"; retain(); }],
    ["ready successor without a live ledger", () => {
      envelope.intent.phase = "ready"; config(ACTIVE_KEY, JSON.stringify(newJournal())); retain();
    }],
    ["ready successor whose ledger is abandoned", () => {
      activate(); envelope.intent.phase = "ready";
      git(bare, ["config", "--local", "--add", LEDGER_KEY, JSON.stringify({
        attemptId: ATTEMPT, runId: RUN, clonePath: link.successor.clonePath, state: "abandoned",
      })]); retain();
    }],
    ["foreign attempt claiming the successor path", () => {
      activate();
      git(bare, ["config", "--local", "--add", "uzi-attempts.agent/foreign.entry", JSON.stringify({
        attemptId: OLD_ATTEMPT, runId: "foreign-run", clonePath: link.successor.clonePath, state: "live",
      })]);
    }],
    ["foreign ledger entry claiming the canonical predecessor path", () => {
      git(bare, ["config", "--local", "--add", "uzi-attempts.agent/foreign.entry", JSON.stringify({
        attemptId: OLD_ATTEMPT, runId: "foreign-run", clonePath: canonical, state: "live",
      })]);
    }],
    ["identical linked active journal on an unrelated branch", () => {
      activate();
      config("uzi-recovery.agent/foreign.clone", JSON.stringify(newJournal()));
    }],
    ["duplicate ledger JSON fields", () => {
      activate();
      git(bare, ["config", "--local", "--add", LEDGER_KEY,
        `{"attemptId":"${ATTEMPT}","runId":"${RUN}","clonePath":"${link.successor.clonePath}","state":"live","state":"retired"}`]);
    }],
    ["unknown ledger JSON fields", () => {
      activate();
      git(bare, ["config", "--local", "--add", LEDGER_KEY, JSON.stringify({
        attemptId: ATTEMPT, runId: RUN, clonePath: link.successor.clonePath, state: "live", futureSchema: true,
      })]);
    }],
    ["unknown active schema identifier", () => {
      config("uzi-recovery.agent/unrelated.clone", JSON.stringify({ runId: "unrelated", clonePath: canonical, futureSchema: 3 }));
    }],
    ["malformed orphan link", () => {
      config("uzi-recovery.agent/unrelated.clone", JSON.stringify({ runId: "unrelated", clonePath: canonical, pinRestore: null }));
    }],
    ["active successor with wrong sourceGeneration", () => {
      activate(); config(ACTIVE_KEY, JSON.stringify({ ...newJournal(), sourceGeneration: 3 }));
    }],
  ];
  for (const [name, corrupt] of cases) {
    it(name, async t => {
      retain();
      corrupt();
      const before = snapshot();
      const guard = guardSources(t, [canonical, link.successor.clonePath]);
      await assert.rejects(cache.readRetainedPinMetadata(bare), /pin|journal|attribution|generation|identity/i);
      guard.assertUntouched();
      assert.deepEqual(snapshot(), before);
    });
  }

  it("refuses an orphan linked active journal without an immutable envelope", async t => {
    activate();
    const before = snapshot();
    const guard = guardSources(t, [canonical, link.successor.clonePath]);
    await assert.rejects(cache.readRetainedPinMetadata(bare), /orphaned pin restore active journal/);
    guard.assertUntouched();
    assert.deepEqual(snapshot(), before);
  });

  it("the historical parser accepts the plain journal and rejects each new rollback fence independently", () => {
    assert.doesNotThrow(() => baseParseRecoveryJournal(JSON.stringify({ runId: RUN, clonePath: canonical })));
    assert.throws(() => baseParseRecoveryJournal(JSON.stringify({
      runId: RUN, clonePath: canonical, sourceGeneration: 1,
    })), /invalid retained recovery clone journal/);
    assert.throws(() => baseParseRecoveryJournal(JSON.stringify({
      runId: RUN, clonePath: canonical, pinRestore: link,
    })), /invalid retained recovery clone journal/);
  });
});

describe("metadata regression boundaries", () => {
  const epochRefusals: Array<[string, () => void]> = [
    ["duplicate sourceGeneration", () => {
      config(ACTIVE_KEY, JSON.stringify(oldJournal).replace('"sourceGeneration":1',
        '"sourceGeneration":1,"sourceGeneration":1'));
    }],
    ["duplicate pinRestore", () => {
      config(ACTIVE_KEY, JSON.stringify(newJournal()).replace('"pinRestore":',
        '"pinRestore":null,"pinRestore":'));
    }],
    ["duplicate unknown metadata", () => {
      config(ACTIVE_KEY, JSON.stringify({ runId: RUN, clonePath: canonical }).replace(
        '"runId":', '"futureSchema":1,"futureSchema":1,"runId":'));
    }],
    ["duplicate V2 version", () => {
      retain();
      config(RETAINED_KEY, JSON.stringify(envelope).replace('"version":2', '"version":2,"version":2'));
    }],
    ["malformed V2", () => {
      config(RETAINED_KEY, JSON.stringify(envelope).slice(0, -1));
    }],
    ["incomplete lexical classification", () => {
      config(ACTIVE_KEY, '{"runId":' + "[".repeat(33));
    }],
  ];
  for (const [name, corrupt] of epochRefusals) {
    it("epoch inspection refuses " + name + " before source access", async t => {
      corrupt();
      const before = snapshot();
      const guard = guardSources(t, [canonical, link.successor.clonePath]);
      const result = await cache.inspectRetainedSources({
        barePath: bare, epoch: true,
        selector: { runId: RUN, generation: 1, attempt: "canonical" },
        holds: { state: "read", holds: [] },
        lifecycle: { state: "terminal", status: "completed" },
        isLivePath: guard.isLivePath, beforeTree: guard.beforeTree,
      });
      assert.equal(result.unreadable, 1);
      assert.deepEqual(result.sources, []);
      guard.assertUntouched();
      assert.deepEqual(snapshot(), before);
    });
  }

  it("binds historical links to the actual bare even with consistent history digests", async t => {
    activate();
    const second = newJournal();
    let raw = JSON.stringify(second);
    envelope.predecessors.push({ journalDigest: digest(raw), rawJournal: raw,
      generation: 2, sourceEpoch: NEW_EPOCH });
    const next = structuredClone(link);
    next.predecessorGeneration = 2;
    next.predecessorEpoch = NEW_EPOCH;
    next.predecessorJournalDigest = digest(raw);
    next.successor.claimGeneration = 3;
    next.successor.attemptId = ATTEMPT.replace("-g2-", "-g3-");
    next.successor.clonePath = attemptClonePath(canonical, next.successor.attemptId);
    next.successor.sourceEpoch = "2123abcd-0123-4abc-8def-0123456789ab";
    next.restore.predecessorGeneration = 2;
    next.restore.pinRef = `refs/uzi-recovery-pin/${RUN}/2`;
    envelope.intent = { version: 1, phase: "prepared", link: next };
    retain();
    const guard = guardSources(t, [canonical, link.successor.clonePath, next.successor.clonePath]);
    assert.equal((await cache.readRetainedPinMetadata(bare))[0]!.originals.length, 2);
    second.pinRestore = structuredClone(link);
    second.pinRestore.restore.bareDir = "other.git";
    raw = JSON.stringify(second);
    envelope.predecessors[1] = { journalDigest: digest(raw), rawJournal: raw,
      generation: 2, sourceEpoch: NEW_EPOCH };
    next.predecessorJournalDigest = digest(raw);
    config(ACTIVE_KEY, raw);
    retain();
    await assert.rejects(cache.readRetainedPinMetadata(bare), /historical pin restore repository identity/);
    guard.assertUntouched();
  });

  it("charges standalone generation journal decoding once before parsing, including escaped keys", async t => {
    const plain = JSON.stringify(oldJournal).replace('"sourceGeneration"', '"\\u0073ourceGeneration"');
    const raw = plain + " ".repeat(16384 - Buffer.byteLength(plain));
    config(ACTIVE_KEY, raw);
    let actual = 0, charged = 0;
    const parse = JSON.parse;
    const consume = PinRestoreDecodeBudget.prototype.consume;
    t.mock.method(JSON, "parse", (text: string, ...args: unknown[]) => {
      if (text === raw) actual += Buffer.byteLength(text);
      return Reflect.apply(parse, JSON, [text, ...args]);
    });
    t.mock.method(PinRestoreDecodeBudget.prototype, "consume", function(this: PinRestoreDecodeBudget, bytes: number) {
      charged += bytes;
      return consume.call(this, bytes);
    });
    await cache.withRecoveryOperation(new AbortController().signal, Date.now() + 60000, async () => {
      for (let i = 0; i < 64; i++) await cache.readRetainedPinMetadata(bare);
      assert.equal(actual, 1048576);
      assert.equal(actual, charged);
      await assert.rejects(cache.readRetainedPinMetadata(bare), /invalid pin restore journal/);
      assert.equal(actual, 1048576, "refuse the next actual decode before parsing");
      assert.equal(charged, 1064960, "charge failed read attempts too");
    });
  });

  for (const count of [33, 65]) {
    it(`inspection shares validated metadata and the 1MiB decode limit for ${count} envelopes`, async t => {
      config(ACTIVE_KEY, JSON.stringify({ runId: RUN, clonePath: canonical }));
      for (let i = 0; i < count; i++) {
        const runId = "run-budget-" + i, branch = "agent/budget-" + i, key = "budget-" + i;
        const clonePath = cache.runnerClonePath(bare, key);
        const plain = JSON.stringify({ runId, clonePath, sourceGeneration: 1 });
        const raw = plain + " ".repeat(16384 - Buffer.byteLength(plain));
        const l = structuredClone(link);
        l.predecessorJournalDigest = digest(raw);
        l.successor.runId = runId;
        l.successor.clonePath = attemptClonePath(clonePath, ATTEMPT);
        l.restore.pinRef = "refs/uzi-recovery-pin/" + runId + "/1";
        config("uzi-retained." + runId + ".journal", JSON.stringify({
          ...envelope, runId, branch, key,
          predecessors: [{ rawJournal: raw, journalDigest: digest(raw), generation: 1, sourceEpoch: OLD_EPOCH }],
          intent: { version: 1, phase: "prepared", link: l },
        }));
        config("uzi-recovery." + branch + ".clone", raw);
      }
      let decoded = 0;
      const parse = JSON.parse;
      t.mock.method(JSON, "parse", (raw: string, ...args: unknown[]) => {
        if (Buffer.byteLength(raw) === 16384) decoded += Buffer.byteLength(raw);
        return Reflect.apply(parse, JSON, [raw, ...args]);
      });
      let total = 0;
      const consume = PinRestoreDecodeBudget.prototype.consume;
      t.mock.method(PinRestoreDecodeBudget.prototype, "consume", function(this: PinRestoreDecodeBudget, bytes: number) {
        total += bytes;
        return consume.call(this, bytes);
      });
      const result = await cache.inspectRetainedSources({ barePath: bare, isLivePath: () => false,
        budgetMs: 60000, selector: { runId: "run-budget-0", generation: 1, attempt: "canonical" } });
      assert.equal(result.unreadable, count === 33 ? 0 : 1);
      assert.equal(total, count * 16384);
      assert.equal(decoded, count === 33 ? 540672 : 1048576, "charge before every actual nested decode");
    });
  }

  it("shares the actual decode cap across repeated reads in one recovery operation", async () => {
    writeOld({}, false);
    const plain = JSON.stringify(oldJournal);
    oldRaw = plain + " ".repeat(16384 - Buffer.byteLength(plain));
    config(ACTIVE_KEY, oldRaw);
    prepare(); retain();
    await cache.withRecoveryOperation(new AbortController().signal, Date.now() + 30000, async () => {
      for (let i = 0; i < 64; i++) await cache.readRetainedPinMetadata(bare);
      await assert.rejects(cache.readRetainedPinMetadata(bare), /invalid pin restore journal/);
    });
    // A later independent operation has its own budget.
    assert.equal((await cache.readRetainedPinMetadata(bare)).length, 1);
  });

  for (const episode of ["retainedSources", "recovery"] as const) {
    it("prepared-old compaction avoids predecessor probes with " + episode, async t => {
      const source = { runId: RUN, clonePath: canonical };
      writeOld(episode === "retainedSources" ? { retainedSources: [source] }
        : { recovery: { version: 1, stage: "capturing", source, attempts: 1,
          startedAt: 1_000_000, deadline: 1_300_000, backoffMs: 1000 } });
      prepare();
      retain();
      const before = snapshot();
      const guard = guardSources(t);
      assert.equal((await cache.readRetainedPinMetadata(bare))[0]!.relation, "prepared-old");
      await cache.withBareLock(bare, () => (cache as unknown as Internals).compactAttemptLedger(bare, BRANCH));
      guard.assertUntouched();
      assert.deepEqual(snapshot(), before);
    });
  }

  it("inspection isolates malformed unrelated V1 active journals per record", async () => {
    config(ACTIVE_KEY, JSON.stringify({ runId: RUN, clonePath: canonical }));
    config("uzi-recovery.agent/unrelated.clone", "{malformed-legacy-journal");
    assert.deepEqual(await cache.readRetainedPinMetadata(bare), []);
    const result = await cache.inspectRetainedSources({ barePath: bare,
      selector: { runId: RUN, generation: "none", attempt: "canonical" }, isLivePath: () => false });
    assert.equal(result.unreadable, 0);
  });
});

describe("full inventory, ACK, inspection and ledger compaction preserve V2 predecessors", () => {
  for (const ready of [false, true]) {
    for (const selectedRun of [RUN, "foreign-inventory-reader"]) {
      it(`${ready ? "ready-new" : "prepared-old"} returns unknown to inventory and ACK for ${selectedRun}`, async t => {
        if (ready) { activate(); envelope.intent.phase = "ready"; }
        retain();
        const before = snapshot();
        const guard = guardSources(t);
        const inventory = await cache.readInventoryCloneHeads(bare, selectedRun);
        const report = await ack(selectedRun);
        guard.assertUntouched();
        assert.deepEqual(inventory, { kind: "unknown", cause: "attribution_unreadable" });
        assert.deepEqual(report, { kind: "unknown", cause: "retained_pin_predecessor" });
        assert.deepEqual(snapshot(), before);
      });
    }
  }

  it("inspect reports unknown predecessor presence and bytes, keeps supplied holds, and never calls old-source callbacks", async t => {
    activate();
    envelope.intent.phase = "ready";
    retain();
    const before = snapshot();
    const guard = guardSources(t);
    const result = await cache.inspectRetainedSources({
      barePath: bare, selector: { runId: RUN, generation: 1, attempt: "canonical" },
      holds: { state: "read", holds: [{ id: "hold-z", generation: 1 }, { id: "hold-a", generation: 9 }] },
      isLivePath: guard.isLivePath, beforeTree: guard.beforeTree,
    });
    guard.assertUntouched();
    assert.equal(result.unreadable, 0);
    assert.equal(result.sources.length, 1);
    const source = result.sources[0]!;
    assert.equal(source.kind, "protected");
    assert.equal(source.presence, "unknown");
    assert.equal(source.allocatedBytes, null);
    assert.equal(source.bytesState, "unknown");
    assert.deepEqual(source.sources.map(p => ({ path: p.clonePath, presence: p.presence, bytes: p.allocatedBytes })),
      [{ path: canonical, presence: "unknown", bytes: null }]);
    assert.deepEqual(source.holds, { state: "read", ids: ["hold-a", "hold-z"] });
    assert.deepEqual(source.disposition, { kind: "refused", consumers: ["retained_pin_predecessor"] });
    assert.ok(source.consumers.includes("immutable_pin_restore_metadata"));
    assert.deepEqual(snapshot(), before);
  });

  it("compaction keeps a missing protected attempt row without a pathPresent or skills probe", async t => {
    const oldAttemptPath = attemptClonePath(cache.runnerClonePath(bare, KEY), OLD_ATTEMPT);
    const entry = { attemptId: OLD_ATTEMPT, runId: RUN, clonePath: oldAttemptPath, state: "reclaimed" };
    git(bare, ["config", "--local", "--add", LEDGER_KEY, JSON.stringify(entry)]);
    writeOld({ retainedSources: [{ runId: RUN, clonePath: oldAttemptPath, attemptId: OLD_ATTEMPT }] });
    prepare();
    activate();
    envelope.intent.phase = "ready";
    retain();
    assert.equal(fs.existsSync(oldAttemptPath), false);
    const before = snapshot();
    const guard = guardSources(t, [canonical, oldAttemptPath]);
    const metadata = await cache.readRetainedPinMetadata(bare);
    assert.equal(metadata.length, 1);
    assert.equal(metadata[0]!.relation, "ready-new");
    assert.ok(metadata[0]!.originals[0]!.journal.retainedSources?.some(s => s.clonePath === oldAttemptPath));
    await cache.withBareLock(bare, () => (cache as unknown as Internals).compactAttemptLedger(bare, BRANCH));
    guard.assertUntouched();
    assert.equal(git(bare, ["config", "--local", "--get-all", LEDGER_KEY]),
      JSON.stringify(entry) + "\n" + JSON.stringify({
        attemptId: ATTEMPT, runId: RUN, clonePath: link.successor.clonePath, state: "live",
      }) + "\n");
    assert.deepEqual(snapshot(), before);
  });

  it("the source tripwire detects the exact old path and its paired skills directory", async t => {
    const guard = guardSources(t);
    await assert.rejects(promises.lstat(canonical), /forbidden predecessor probe/);
    const skills = path.join(path.dirname(canonical), `.uzi-skills-${path.basename(canonical)}`);
    await assert.rejects(promises.lstat(skills), /forbidden predecessor probe/);
    assert.deepEqual(guard.attempts, [`lstat:${canonical}`, `lstat:${skills}`]);
  });

  it("a normal source without V2 still executes real source reads and returns verified heads", async t => {
    config(ACTIVE_KEY, JSON.stringify({ runId: RUN, clonePath: canonical }));
    const before = snapshot();
    const lstatPaths: string[] = [];
    const original = promises.lstat;
    t.mock.method(promises, "lstat", (...args: Parameters<typeof promises.lstat>) => {
      lstatPaths.push(String(args[0]));
      return Reflect.apply(original, promises, args);
    });
    assert.deepEqual(await cache.readRetainedPinMetadata(bare), []);
    const expectedHead = git(canonical, ["rev-parse", "HEAD"]).trim();
    const inventory = await cache.readInventoryCloneHeads(bare, RUN);
    const report = await ack();
    assert.equal(inventory.kind, "verified");
    if (inventory.kind === "verified") assert.ok(inventory.heads.includes(expectedHead));
    assert.equal(report.kind, "verified");
    if (report.kind === "verified") assert.ok(report.heads.includes(expectedHead));
    assert.ok(lstatPaths.includes(canonical), "normal readers must still inspect real source presence");
    assert.deepEqual(snapshot(), before);
  });

  it("a V1 protected descriptor retains its physical inspection behavior", async () => {
    const journal = { runId: RUN, clonePath: canonical, retainedSources: [{ runId: RUN, clonePath: canonical }] };
    config(ACTIVE_KEY, JSON.stringify(journal));
    config(RETAINED_KEY, JSON.stringify({ version: 1, branch: BRANCH, key: KEY, journal }));
    const before = snapshot();
    assert.deepEqual(await cache.readRetainedPinMetadata(bare), []);
    const live: string[] = [], trees: string[] = [];
    const result = await cache.inspectRetainedSources({
      barePath: bare, selector: { runId: RUN, generation: "none", attempt: "canonical" },
      holds: { state: "read", holds: [] },
      isLivePath: p => { live.push(p); return false; }, beforeTree: p => { trees.push(p); },
    });
    assert.equal(result.unreadable, 0);
    assert.equal(result.sources.length, 1);
    assert.equal(result.sources[0]!.presence, "present");
    assert.equal(result.sources[0]!.bytesState, "measured");
    assert.equal(typeof result.sources[0]!.allocatedBytes, "number");
    assert.ok(live.includes(canonical));
    assert.ok(trees.includes(canonical));
    assert.deepEqual(snapshot(), before);
  });
});

describe("own capture source generation durability", () => {
  for (const generation of [undefined, 2]) {
    it(`reconstructs a legacy successor using only the actual episode generation ${generation}`, async () => {
      writeOld({}, false);
      const source = { runId: RUN, clonePath: canonical };
      const reservation = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source, 1, generation, 0, 0);
      await fixtureFetchTracking(cache, bare, canonical, BRANCH, RUN);
      const tip = git(canonical, ["rev-parse", "HEAD"]).trim();
      await cache.recordRecoveryCapture(bare, BRANCH, KEY, source, reservation.attempts, tip);
      const successor = await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source, reservation.attempts, ATTEMPT);
      const journal = JSON.parse(rawConfig(ACTIVE_KEY));
      assert.equal(journal.clonePath, successor.path);
      assert.equal(journal.sourceGeneration, generation);
      assert.equal(Object.hasOwn(journal, "sourceGeneration"), generation !== undefined);
    });
  }

  it("adds an actual own generation only when explicitly supplied", async () => {
    writeOld({ sourceGeneration: undefined });
    await cache.markRecoveryCapture(bare, canonical, BRANCH, RUN);
    assert.equal(Object.hasOwn(JSON.parse(rawConfig(ACTIVE_KEY)), "sourceGeneration"), false);
    await cache.markRecoveryCapture(bare, canonical, BRANCH, RUN, undefined, undefined, 1);
    assert.equal(JSON.parse(rawConfig(ACTIVE_KEY)).sourceGeneration, 1);
  });

  it("persists the own generation and preserves the fence and all pending fields", async () => {
    writeOld({ memoryPark: {
      version: 1, binding: { runId: RUN, claimGeneration: 1, workerId: "worker", registerNonce: "nonce", memoryEpisode: 1 },
      ack: "sent_unconfirmed",
      origin: { flightId: OLD_EPOCH, attemptId: null, executorIncarnation: null,
        authorityIncarnation: OLD_EPOCH, sourceEpoch: OLD_EPOCH },
    } });
    const before = JSON.parse(rawConfig(ACTIVE_KEY));
    await cache.markRecoveryCapture(bare, canonical, BRANCH, RUN, undefined, undefined, 1);
    assert.deepEqual(JSON.parse(rawConfig(ACTIVE_KEY)), before);
    assert.equal(JSON.parse(rawConfig(ACTIVE_KEY)).sourceGeneration, 1);
    const raw = rawConfig(ACTIVE_KEY);
    await assert.rejects(cache.markRecoveryCapture(bare, canonical, BRANCH, RUN, undefined, undefined, 2), /cannot be overwritten/);
    assert.equal(rawConfig(ACTIVE_KEY), raw);
  });

  it("preserves active pin linkage while recording the identical own generation", async () => {
    retain();
    activate();
    const journal = { ...newJournal(), completionSourceFence: { ...FENCE, sourceUseEpoch: NEW_EPOCH } };
    config(ACTIVE_KEY, JSON.stringify(journal));
    assert.equal((await cache.readRetainedPinMetadata(bare))[0]!.relation, "prepared-new");
    const retainedRaw = rawConfig(RETAINED_KEY);
    await cache.markRecoveryCapture(bare, link.successor.clonePath, BRANCH, RUN, ATTEMPT, undefined, 2);
    assert.deepEqual(JSON.parse(rawConfig(ACTIVE_KEY)), journal);
    assert.equal(rawConfig(RETAINED_KEY), retainedRaw);
    assert.equal((await cache.readRetainedPinMetadata(bare))[0]!.relation, "prepared-new");
  });

  it("validates generation and attempt path before effects", async () => {
    const before = snapshot();
    for (const generation of [0, -1, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      await assert.rejects(cache.markRecoveryCapture(bare, canonical, BRANCH, RUN, undefined, undefined, generation), /invalid source generation/);
    }
    await assert.rejects(cache.markRecoveryCapture(bare, canonical, BRANCH, RUN, ATTEMPT, undefined, 2), /invalid source generation/);
    await assert.rejects(cache.markRecoveryCapture(bare, link.successor.clonePath, BRANCH, RUN, ATTEMPT, undefined, 1), /invalid source generation/);
    assert.deepEqual(snapshot(), before);
  });

  it("syncs config then directory before exact raw readback", async t => {
    const events: string[] = [];
    const open = promises.open;
    t.mock.method(promises, "open", async (...args: Parameters<typeof promises.open>) => {
      const h = await Reflect.apply(open, promises, args);
      if (String(args[0]) === path.join(bare, "config") || String(args[0]) === bare) {
        const what = String(args[0]) === bare ? "directory" : "config";
        const sync = h.sync.bind(h);
        t.mock.method(h, "sync", async () => { events.push(what); await sync(); });
      }
      return h;
    });
    const internal = cache as unknown as { readRecoveryCaptureRaw(b: string, branch: string): Promise<string | undefined> };
    const read = internal.readRecoveryCaptureRaw.bind(cache);
    t.mock.method(internal, "readRecoveryCaptureRaw", async (b: string, branch: string) => {
      if (events.length) events.push("readback");
      return read(b, branch);
    });
    await cache.markRecoveryCapture(bare, canonical, BRANCH, RUN, undefined, undefined, 1);
    assert.deepEqual(events, ["config", "directory", "readback"]);
  });

  for (const fail of ["config", "directory", "readback"]) {
    it(`refuses generation admission on ${fail} failure`, async t => {
      if (fail === "readback") {
        const internal = cache as unknown as { readRecoveryCaptureRaw(b: string, branch: string): Promise<string | undefined> };
        const read = internal.readRecoveryCaptureRaw.bind(cache);
        t.mock.method(internal, "readRecoveryCaptureRaw", async (b: string, branch: string) => {
          const value = await read(b, branch);
          return value + " ";
        });
      } else {
        const open = promises.open;
        t.mock.method(promises, "open", async (...args: Parameters<typeof promises.open>) => {
          const h = await Reflect.apply(open, promises, args);
          if (String(args[0]) === (fail === "config" ? path.join(bare, "config") : bare)) {
            t.mock.method(h, "sync", async () => { throw new Error("injected generation fsync"); });
          }
          return h;
        });
      }
      await assert.rejects(cache.markRecoveryCapture(bare, canonical, BRANCH, RUN, undefined, undefined, 1),
        fail === "readback" ? /read-back differs/ : /injected generation fsync/);
    });
  }
});
