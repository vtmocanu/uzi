import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
import path from "node:path";
import {
  GitCache, RestoreCompositeBlockedError, RestoreCompositeError, restoreJournalJson,
  type BranchJournal, type MemoryParkRecord, type OpaqueDiscovery, type RecoveryJournalEntry,
} from "../src/git.js";
import { restoreDigest, restoreTombstoneJson, type RestoreBasis } from "../src/restore-journal.js";
import { attemptClonePath, formatAttemptId } from "../src/attempt-path.js";
import { serializeSourceFence, type CompletionSourceFence } from "../src/completion-source.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #3644 Gate B (plan 3.0, 3.5): opaque discovery, the branch-journal resolver, the guards on every writer of the
// predecessor's key, and the (a) sites that resolve to the successor's key. Real Git in temp dirs; nothing here creates a
// clone directory, because nothing under test may read one.

function scrubGitEnv(base: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(base)) if (!key.startsWith("GIT_")) env[key] = value;
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
}
const ENV = scrubGitEnv(process.env);
const BRANCH = "agent/issue-3644";
const KEY = "issue-3644";
const RUN = "run-3644";
const OTHER_RUN = "run-9999";
const P_ID = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
const S_ID = formatAttemptId(new Date("2026-11-05T11:11:11Z"), 2, "fedcba9876543210");
const SHA = "a".repeat(40);
const FENCE: CompletionSourceFence = { version: 1, sourceUseEpoch: "0123abcd-0123-4abc-8def-0123456789ab", sourceCoreDigest: "a".repeat(64) };
const UUID = "11111111-1111-4111-8111-111111111111";
const PARK: MemoryParkRecord = {
  version: 1, binding: { runId: RUN, claimGeneration: 2, workerId: "worker-1", registerNonce: "nonce-1", memoryEpisode: 1 },
  ack: "sent_unconfirmed", origin: { flightId: UUID, attemptId: null, executorIncarnation: null, authorityIncarnation: UUID, sourceEpoch: UUID },
};
const P_KEY = `uzi-recovery.${BRANCH}.clone`;
const S_KEY = `uzi-restore.${BRANCH}.journal`;
const LEDGER_KEY = `uzi-attempts.${BRANCH}.entry`;

let fx: Fixture;
let cache: GitCache;
let bare: string;
let P_PATH: string;
let S_PATH: string;
let pRaw: string;

type Internals = {
  writeRecovery(bare: string, branch: string, journal: RecoveryJournalEntry): Promise<void>;
  healBoundStoppedJournal(bare: string, branch: string, stored: { journal: RecoveryJournalEntry; raw: string }): Promise<boolean>;
  discardRetainedRecoveryUnderLock(bare: string, branch: string, key: string, expected: { runId: string; clonePath: string; attemptId?: string }): Promise<void>;
  checkedRecovery(bare: string, branch: string, key: string, expected: { runId: string; clonePath: string; attemptId?: string }): Promise<unknown>;
  journaledClonePaths(bare: string): Promise<Set<string>>;
  compactAttemptLedger(bare: string, branch: string): Promise<void>;
  gatherRetainedUnderLock(bare: string): Promise<{ records: Array<{ kind: string; runId: string; digest: string; parts: Array<{ clonePath: string; presence: string }>; problems: string[] }> }>;
  withLock<T>(bare: string, fn: () => Promise<T>): Promise<T>;
  execScoped(command: string, args: string[], options: unknown): Promise<{ stdout: string; stderr: string }>;
  runGit(cwd: string | undefined, args: string[]): Promise<string>;
};
const internals = (c: GitCache = cache): Internals => c as unknown as Internals;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const config = (): Buffer => fs.readFileSync(path.join(bare, "config"));
const rawKey = (key: string): string | undefined => {
  try { return gitIn(bare, ["config", "--get", key]) || undefined; } catch { return undefined; }
};
const setKey = (key: string, value: string): void => { gitIn(bare, ["config", key, value]); };
const addLedger = (attemptId: string, clonePath: string, state: string, runId = RUN): void => {
  gitIn(bare, ["config", "--add", LEDGER_KEY, JSON.stringify({ attemptId, runId, clonePath, state })]);
};
const pJournal = () => ({ runId: RUN, clonePath: P_PATH, attemptId: P_ID });
const sJournal = (extra: Record<string, unknown> = {}) => ({ runId: RUN, clonePath: S_PATH, attemptId: S_ID, ...extra });
const basis = (raw = pRaw): RestoreBasis => ({
  version: 1, predecessor: { clonePath: P_PATH, attemptId: P_ID, generation: 1, root: { dev: "2049", ino: "131072" } }, journalDigest: restoreDigest(raw), boundary: { predecessor: "c".repeat(64), successor: "d".repeat(64) },
  slotOwner: { worker: "worker-incarnation-1", epoch: "epoch-1" },
  pin: { captureId: "capture-1", generation: 1, sourceSha: SHA, finalization: true }, restoreTip: SHA,
});
const entryValue = (stage: "seeding" | "ready", extra: Record<string, unknown> = {}): string =>
  restoreJournalJson(basis(), stage, sJournal(extra) as RecoveryJournalEntry);
const tombstoneValue = (): string => restoreTombstoneJson(basis(), { runId: RUN, attemptId: S_ID, clonePath: S_PATH });

const reset = (): void => {
  for (const key of [P_KEY, S_KEY, LEDGER_KEY]) { try { gitIn(bare, ["config", "--unset-all", key]); } catch { /* absent */ } }
};

type SState = { stage: "seeding" | "ready" | "retired"; ledger: string | null };
/** P's plain journal, P's live ledger entry, S's ledger entry and S's restore value. */
function compose({ stage, ledger }: SState, extra: Record<string, unknown> = {}): void {
  setKey(P_KEY, pRaw);
  addLedger(P_ID, P_PATH, "live");
  if (ledger !== null) addLedger(S_ID, S_PATH, ledger);
  setKey(S_KEY, stage === "retired" ? tombstoneValue() : entryValue(stage, extra));
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const canonical = cache.runnerClonePath(bare, KEY);
  P_PATH = attemptClonePath(canonical, P_ID);
  S_PATH = attemptClonePath(canonical, S_ID);
  pRaw = JSON.stringify(pJournal());
});
afterEach(() => fx.cleanup());

/** Every filesystem call that names a forbidden path is recorded and refused (both sync and promise APIs). */
function trapPaths(forbidden: (resolved: string) => boolean): { hits: string[]; restore(): void } {
  const hits: string[] = [];
  const under = (p: unknown): boolean => {
    if (typeof p !== "string" && !(p instanceof URL) && !Buffer.isBuffer(p)) return false;
    return forbidden(path.resolve(p instanceof URL ? p.pathname : String(p)));
  };
  const restores: Array<() => void> = [];
  const patch = (target: Record<string, unknown>, names: string[], label: string): void => {
    for (const name of names) {
      const original = target[name];
      if (typeof original !== "function") continue;
      target[name] = function (this: unknown, ...args: unknown[]) {
        if (under(args[0]) || (name === "rename" || name === "cp" || name === "copyFile" ? under(args[1]) : false)) {
          hits.push(`${label}.${name}(${String(args[0])})`);
          throw new Error(`runner root touched: ${label}.${name}`);
        }
        return (original as (...a: unknown[]) => unknown).apply(this, args);
      };
      restores.push(() => { target[name] = original; });
    }
  };
  const names = ["lstat", "stat", "open", "opendir", "readdir", "readFile", "access", "readlink", "realpath", "mkdir", "rm", "rename", "writeFile", "appendFile",
    "copyFile", "cp", "chmod", "utimes", "mkdtemp", "unlink", "rmdir", "statfs"];
  patch(fsp as unknown as Record<string, unknown>, names, "fs.promises");
  patch(fs as unknown as Record<string, unknown>, [...names.map(n => `${n}Sync`), "existsSync", "createReadStream", "createWriteStream", "opendirSync", "cpSync", "rmSync"], "fs");
  syncBuiltinESMExports();
  return { hits, restore: () => { for (const r of restores.reverse()) r(); syncBuiltinESMExports(); } };
}

/** Nothing under the runner root at all. */
function trapRunnerRoot(): { hits: string[]; restore(): void } {
  const root = path.resolve(fx.dataDir, "runner");
  return trapPaths(p => p === root || p.startsWith(root + path.sep));
}

/** The predecessor's clone, its parent chain excluded: any call that names it or something inside it, or its skills sibling. */
function trapPredecessor(): { hits: string[]; restore(): void } {
  const skills = path.join(path.dirname(P_PATH), `.uzi-skills-${path.basename(P_PATH)}`);
  return trapPaths(p => [P_PATH, skills].some(f => p === f || p.startsWith(f + path.sep)));
}

/** Records the argv of every git child, so a git run with a cwd or path argument under the runner root is visible. */
function recordGitArgs(c: GitCache): string[][] {
  const calls: string[][] = [];
  const i = internals(c);
  const original = i.execScoped.bind(c);
  i.execScoped = async (command, args, options) => { calls.push([command, ...args]); return original(command, args, options); };
  return calls;
}

describe("resolveBranchJournal", () => {
  it("reports none and plain, and never reads the ledger or any path for them", async () => {
    const trap = trapRunnerRoot();
    try {
      assert.deepEqual(await cache.resolveBranchJournal(bare, BRANCH), { kind: "none" });
      setKey(P_KEY, pRaw);
      const plain = await cache.resolveBranchJournal(bare, BRANCH);
      assert.equal(plain.kind, "plain");
      assert.equal(plain.kind === "plain" && plain.raw, pRaw);
      setKey(P_KEY, "");
      assert.deepEqual(await cache.resolveBranchJournal(bare, BRANCH), { kind: "none" });
      assert.deepEqual(trap.hits, []);
    } finally { trap.restore(); }
  });

  const VALID: SState[] = [
    { stage: "seeding", ledger: "live" }, { stage: "ready", ledger: "live" }, { stage: "ready", ledger: "retired" }, { stage: "retired", ledger: "retired" },
  ];
  for (const state of VALID) {
    it(`accepts ${state.stage} + ${state.ledger} and returns P, S and the ledger`, async () => {
      compose(state);
      const r = await cache.resolveBranchJournal(bare, BRANCH) as Extract<BranchJournal, { kind: "composite" }>;
      assert.equal(r.kind, "composite");
      assert.equal(r.stage, state.stage);
      assert.equal(r.ledger.state, state.ledger);
      assert.equal(r.predecessor.raw, pRaw);
      assert.deepEqual(r.basis, basis());
      if (state.stage === "retired") assert.deepEqual(r.successor.kind === "tombstone" && r.successor.successor, { runId: RUN, attemptId: S_ID, clonePath: S_PATH });
      else assert.equal(r.successor.kind === "entry" && r.successor.journal.attemptId, S_ID);
    });
  }

  const INVALID: SState[] = [
    { stage: "seeding", ledger: "retired" }, { stage: "seeding", ledger: "reclaimed" }, { stage: "seeding", ledger: "abandoned" }, { stage: "seeding", ledger: null },
    { stage: "ready", ledger: "reclaimed" }, { stage: "ready", ledger: "abandoned" }, { stage: "ready", ledger: null },
    { stage: "retired", ledger: "live" }, { stage: "retired", ledger: "reclaimed" }, { stage: "retired", ledger: null },
  ];
  for (const state of INVALID) {
    it(`refuses ${state.stage} + ${state.ledger ?? "no ledger entry"}`, async () => {
      compose(state);
      await assert.rejects(cache.resolveBranchJournal(bare, BRANCH), RestoreCompositeError);
    });
  }

  it("refuses an empty predecessor key, a changed predecessor digest and a basis that names another predecessor", async () => {
    compose({ stage: "ready", ledger: "live" });
    setKey(P_KEY, "");
    await assert.rejects(cache.resolveBranchJournal(bare, BRANCH), /predecessor journal is empty/);
    setKey(P_KEY, JSON.stringify({ ...pJournal(), peeledMarkers: [SHA] }));
    await assert.rejects(cache.resolveBranchJournal(bare, BRANCH), /predecessor journal changed/);
    setKey(P_KEY, "{not json");
    await assert.rejects(cache.resolveBranchJournal(bare, BRANCH), RestoreCompositeError);
    // A basis whose predecessor is not the journal's, even with a matching digest.
    const other = JSON.stringify({ runId: RUN, clonePath: attemptClonePath(cache.runnerClonePath(bare, KEY), formatAttemptId(new Date("2026-11-05T09:00:00Z"), 1, "1111111111111111")),
      attemptId: formatAttemptId(new Date("2026-11-05T09:00:00Z"), 1, "1111111111111111") });
    setKey(P_KEY, other);
    setKey(S_KEY, restoreJournalJson(basis(other), "ready", sJournal() as RecoveryJournalEntry));
    await assert.rejects(cache.resolveBranchJournal(bare, BRANCH), /basis does not match the predecessor/);
  });

  it("refuses a run mismatch, a ledger entry of another run or path, a repeated restore key and a successor equal to the predecessor", async () => {
    setKey(P_KEY, pRaw);
    addLedger(P_ID, P_PATH, "live");
    addLedger(S_ID, S_PATH, "live");
    setKey(S_KEY, restoreJournalJson(basis(), "ready", { ...sJournal(), runId: OTHER_RUN } as RecoveryJournalEntry));
    await assert.rejects(cache.resolveBranchJournal(bare, BRANCH), /another run/);
    setKey(S_KEY, entryValue("ready"));
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "composite");
    // The ledger's last value per attempt wins: a later entry for another run or another path breaks the pairing.
    addLedger(S_ID, S_PATH, "live", OTHER_RUN);
    await assert.rejects(cache.resolveBranchJournal(bare, BRANCH), /no matching attempt ledger entry/);
    addLedger(S_ID, `${S_PATH}x`, "live");
    await assert.rejects(cache.resolveBranchJournal(bare, BRANCH), /no matching attempt ledger entry/);
    addLedger(S_ID, S_PATH, "live");
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "composite");
    gitIn(bare, ["config", "--add", S_KEY, entryValue("ready")]);
    await assert.rejects(cache.resolveBranchJournal(bare, BRANCH), /ambiguous restore entry/);
  });
});

describe("discoverOpaque", () => {
  const shapes = (name: string): Record<string, unknown> => ({
    plain: pJournal(),
    fenced: { ...pJournal(), completionSourceFence: FENCE },
    episode: { ...pJournal(), recovery: { version: 1, source: pJournal(), attempts: 1, startedAt: 0, deadline: 300000, backoffMs: 0, stage: "capturing" } },
    retainedSources: { ...pJournal(), retainedSources: [{ runId: RUN, clonePath: S_PATH, attemptId: S_ID }] },
    memoryPark: { ...pJournal(), memoryPark: PARK },
    peeledMarkers: { ...pJournal(), peeledMarkers: [SHA] },
    restoreTip: { ...pJournal(), restoreTip: SHA },
  } as Record<string, Record<string, unknown>>)[name]!;
  for (const name of ["plain", "fenced", "episode", "retainedSources", "memoryPark", "peeledMarkers", "restoreTip"]) {
    it(`returns sameRun for a ${name} journal of this run, with its raw bytes, without touching the runner root or the config`, async () => {
      const raw = JSON.stringify(shapes(name));
      setKey(P_KEY, raw);
      const before = config();
      const trap = trapRunnerRoot();
      const calls = recordGitArgs(cache);
      let found: OpaqueDiscovery;
      try { found = await cache.discoverOpaque(bare, BRANCH, RUN); } finally { trap.restore(); }
      assert.deepEqual(trap.hits, []);
      assert.ok(found.kind === "sameRun", JSON.stringify(found));
      assert.equal(found.raw, raw);
      assert.equal(found.source, "active");
      assert.equal(found.journal.runId, RUN);
      assert.equal(found.journal.clonePath, P_PATH);
      assert.deepEqual(config(), before);
      assert.ok(calls.length > 0 && calls.every(call => call[0] === "git" && call[1] === "-C" && call[2] === bare && call.every(a => !a.startsWith(path.join(fx.dataDir, "runner")))), JSON.stringify(calls));
      assert.ok(calls.every(call => !call.includes("--work-tree")));
    });
  }

  it("returns other for no journal, another run's journal and an undecodable one, and leaves the bare untouched", async () => {
    assert.deepEqual(await cache.discoverOpaque(bare, BRANCH, RUN), { kind: "other" });
    setKey(P_KEY, JSON.stringify({ ...pJournal(), runId: OTHER_RUN }));
    assert.deepEqual(await cache.discoverOpaque(bare, BRANCH, RUN), { kind: "other" });
    for (const junk of ["{nope", JSON.stringify({ runId: RUN }), JSON.stringify({ ...pJournal(), extra: 1 }), JSON.stringify({ ...pJournal(), clonePath: "relative" })]) {
      setKey(P_KEY, junk);
      const before = config();
      assert.deepEqual(await cache.discoverOpaque(bare, BRANCH, RUN), { kind: "other" }, junk);
      assert.deepEqual(config(), before);
    }
  });

  it("reads this run's protected descriptor when the active key is cleared", async () => {
    const journal = { ...pJournal(), retainedSources: [{ runId: RUN, clonePath: S_PATH, attemptId: S_ID }] };
    const descriptor = JSON.stringify({ version: 1, branch: BRANCH, key: KEY, journal });
    setKey(`uzi-retained.${RUN}.journal`, descriptor);
    const found = await cache.discoverOpaque(bare, BRANCH, RUN);
    assert.ok(found.kind === "sameRun" && found.source === "descriptor" && found.raw === descriptor);
    assert.deepEqual(await cache.discoverOpaque(bare, BRANCH, OTHER_RUN), { kind: "other" });
    setKey(`uzi-retained.${RUN}.journal`, JSON.stringify({ version: 1, branch: "other/branch", key: KEY, journal }));
    assert.deepEqual(await cache.discoverOpaque(bare, BRANCH, RUN), { kind: "other" });
  });

  it("returns the composite for every valid state of its own run, never touching the runner root", async () => {
    for (const state of [{ stage: "seeding", ledger: "live" }, { stage: "ready", ledger: "live" }, { stage: "ready", ledger: "retired" }, { stage: "retired", ledger: "retired" }] as SState[]) {
      reset();
      compose(state);
      const trap = trapRunnerRoot();
      let found: OpaqueDiscovery;
      try { found = await cache.discoverOpaque(bare, BRANCH, RUN); } finally { trap.restore(); }
      assert.deepEqual(trap.hits, [], state.stage);
      assert.ok(found.kind === "composite" && found.composite.stage === state.stage, JSON.stringify(state));
    }
  });

  it("stops on another run's composite (blocked) and on an invalid one (composite error), writing nothing", async () => {
    compose({ stage: "ready", ledger: "live" });
    const before = config();
    await assert.rejects(cache.discoverOpaque(bare, BRANCH, OTHER_RUN), (e: unknown) => e instanceof RestoreCompositeBlockedError && e.restoreRunId === RUN);
    setKey(P_KEY, JSON.stringify({ ...pJournal(), peeledMarkers: [SHA] }));
    const changed = config();
    await assert.rejects(cache.discoverOpaque(bare, BRANCH, RUN), (e: unknown) => e instanceof RestoreCompositeError && !(e instanceof RestoreCompositeBlockedError));
    assert.deepEqual(config(), changed);
    assert.notDeepEqual(before, changed);
  });

  it("is a read under the bare lock: a queued writer cannot interleave", async () => {
    setKey(P_KEY, pRaw);
    const events: string[] = [];
    const i = internals();
    let release!: () => void;
    const held = i.withLock(bare, () => new Promise<void>(resolve => { release = resolve; events.push("held"); }));
    const discovery = cache.discoverOpaque(bare, BRANCH, RUN).then(r => { events.push("discovered"); return r; });
    await new Promise(resolve => setTimeout(resolve, 50));
    assert.deepEqual(events, ["held"]);
    release();
    await held;
    assert.equal((await discovery).kind, "sameRun");
  });
});

describe("readBareAttributionConfig", () => {
  it("returns every attribution key raw, in config order, and nothing else", async () => {
    setKey(P_KEY, pRaw);
    addLedger(P_ID, P_PATH, "live");
    setKey(S_KEY, "");
    setKey(`uzi-stopped.${RUN}.entry`, "{}");
    setKey(`uzi-retained.${RUN}.journal`, "{}");
    setKey("uzi-attemptmode.issue-1.enabled", "true");
    setKey("uzi-test.other", "x");
    const read = await cache.readBareAttributionConfig(bare);
    assert.equal(read.complete, true);
    assert.deepEqual(read.entries.map(e => e.key), [P_KEY, LEDGER_KEY, S_KEY, `uzi-stopped.${RUN}.entry`, `uzi-retained.${RUN}.journal`]);
    assert.equal(read.entries[0]!.value, pRaw);
    assert.equal(read.entries[2]!.value, "");
    assert.ok(read.entries.every(e => e.valueless === undefined));
  });

  it("marks a valueless key and reports incomplete on a read error", async () => {
    fs.appendFileSync(path.join(bare, "config"), `[uzi-restore "${BRANCH}"]\n\tjournal\n`);
    const read = await cache.readBareAttributionConfig(bare);
    assert.deepEqual(read.entries.find(e => e.key === S_KEY), { key: S_KEY, value: "", valueless: true });
    const i = internals();
    const original = i.runGit.bind(cache);
    i.runGit = async () => { throw new Error("epoch metadata record limit"); };
    try {
      assert.deepEqual(await cache.readBareAttributionConfig(bare), { entries: [], complete: false });
    } finally { i.runGit = original; }
  });
});

function guards(): Array<[string, () => Promise<unknown>]> {
  const expectedP = { runId: RUN, clonePath: P_PATH, attemptId: P_ID };
  const i = internals();
  const progress = { version: 1 as const, source: expectedP, attempts: 1, startedAt: 0, deadline: 300000, backoffMs: 0, stage: "capturing" as const };
  return [
    ["writeRecovery", () => i.writeRecovery(bare, BRANCH, pJournal() as RecoveryJournalEntry)],
    ["writeRecovery (a fence on P)", () => i.writeRecovery(bare, BRANCH, { ...pJournal(), completionSourceFence: FENCE } as RecoveryJournalEntry)],
    ["checkedRecovery (the lstat of every recorded source)", () => i.checkedRecovery(bare, BRANCH, KEY, expectedP)],
    ["detachTerminalRetained", () => cache.detachTerminalRetained(bare, BRANCH, KEY, pJournal() as RecoveryJournalEntry)],
    ["healBoundStoppedJournal", () => i.healBoundStoppedJournal(bare, BRANCH, { journal: pJournal() as RecoveryJournalEntry, raw: pRaw })],
    ["prepareRecoverySuccessor", () => cache.prepareRecoverySuccessor(bare, BRANCH, KEY, expectedP, 1, S_ID)],
    ["discardRetainedRecovery", () => cache.discardRetainedRecovery(bare, BRANCH, KEY, expectedP, true)],
    ["discardRetainedRecoveryUnderLock", () => i.discardRetainedRecoveryUnderLock(bare, BRANCH, KEY, expectedP)],
    ["releaseAttemptInPlace", () => cache.releaseAttemptInPlace(bare, P_PATH, BRANCH, RUN, "abandoned")],
    ["canRenewRecoveryAdoption", () => cache.canRenewRecoveryAdoption(bare, BRANCH, KEY, expectedP, 2, 1)],
    ["recoveryReservationSnapshot", () => cache.recoveryReservationSnapshot(bare, BRANCH, KEY, expectedP, progress)],
    ["reserveRecoveryIteration", () => cache.reserveRecoveryIteration(bare, BRANCH, KEY, expectedP)],
    ["verifiedRetainedRecoveryRestorePoint", () => cache.verifiedRetainedRecoveryRestorePoint(bare, BRANCH, KEY, pJournal() as RecoveryJournalEntry)],
    ["verifiedRecoveryRestorePoint", () => cache.verifiedRecoveryRestorePoint(bare, BRANCH, KEY, expectedP, SHA)],
    ["recordRecoveryCapture", () => cache.recordRecoveryCapture(bare, BRANCH, KEY, expectedP, 1, SHA)],
    ["blockRecoveryEpisode", () => cache.blockRecoveryEpisode(bare, BRANCH, KEY, expectedP, "capture_failed")],
    ["completeRecoveryEpisode", () => cache.completeRecoveryEpisode(bare, BRANCH, KEY, expectedP, { settled: true, processedEvents: 1, attemptId: P_ID })],
    ["normalizeRetainedSourceObjects", () => cache.withRecoveryOperation(new AbortController().signal, Date.now() + 60_000,
      () => cache.normalizeRetainedSourceObjects(bare, BRANCH, KEY, expectedP))],
    ["terminalRetainedSnapshot", () => cache.terminalRetainedSnapshot(bare, BRANCH, KEY, expectedP)],
    ["discoverRetainedRecovery (legacy discovery)", () => cache.discoverRetainedRecovery(fx.originPath, BRANCH, KEY, RUN)],
  ];
}

describe("every writer of the predecessor's key refuses while a restore entry exists", () => {
  for (const state of [{ stage: "seeding", ledger: "live" }, { stage: "ready", ledger: "live" }, { stage: "retired", ledger: "retired" }] as SState[]) {
    it(`with ${state.stage}: RestoreCompositeError before any write, the bare config byte-identical`, async () => {
      compose(state);
      const before = config();
      const trap = trapRunnerRoot();
      try {
        for (const [name, call] of guards()) {
          // normalizeRetainedSourceObjects maps every failure to its own coded refusal.
          await assert.rejects(call(), (e: unknown) => name === "normalizeRetainedSourceObjects"
            ? e instanceof Error && e.message === "retained_readiness_validation" : e instanceof RestoreCompositeError, name);
          assert.deepEqual(config(), before, name);
        }
      } finally { trap.restore(); }
      assert.deepEqual(trap.hits, []);
    });
  }

  it("the same calls for another run are the nonterminal block", async () => {
    compose({ stage: "ready", ledger: "live" });
    const before = config();
    const other = { runId: OTHER_RUN, clonePath: P_PATH, attemptId: P_ID };
    const i = internals();
    const rows: Array<[string, () => Promise<unknown>]> = [
      ["writeRecovery", () => i.writeRecovery(bare, BRANCH, other as RecoveryJournalEntry)],
      ["detachTerminalRetained", () => cache.detachTerminalRetained(bare, BRANCH, KEY, other as RecoveryJournalEntry)],
      ["prepareRecoverySuccessor", () => cache.prepareRecoverySuccessor(bare, BRANCH, KEY, other, 1, S_ID)],
      ["releaseAttemptInPlace", () => cache.releaseAttemptInPlace(bare, P_PATH, BRANCH, OTHER_RUN, "reclaimed")],
      ["a canonical seed of the branch", () => cache.runnerCloneForBranch(bare, BRANCH, KEY, { beforeFree: async () => {}, isLive: () => false }, OTHER_RUN)],
      ["an attempt seed of the branch", () => cache.runnerCloneForBranch(bare, BRANCH, KEY, { beforeFree: async () => {}, isLive: () => false }, OTHER_RUN, false, undefined,
        { attemptId: formatAttemptId(new Date("2026-11-05T12:00:00Z"), 3, "2222222222222222"), isLive: () => false, beforeSeed: async () => {} })],
    ];
    for (const [name, call] of rows) {
      await assert.rejects(call(), (e: unknown) => e instanceof RestoreCompositeBlockedError && e.restoreRunId === RUN, name);
      assert.deepEqual(config(), before, name);
    }
  });

  it("the swallowing writers report unknown or not-covered and write nothing", async () => {
    compose({ stage: "ready", ledger: "live" });
    const before = config();
    assert.equal(await cache.markStoppedCanonical(bare, BRANCH, KEY, { runId: RUN, clonePath: cache.runnerClonePath(bare, KEY), generation: null, reason: "t" }), "unknown");
    assert.equal(await cache.retainStoppedAttempt(bare, BRANCH, { runId: RUN, clonePath: P_PATH, attemptId: P_ID }), "unknown");
    assert.equal(await cache.retainStoppedAttempt(bare, BRANCH, { runId: RUN, clonePath: S_PATH, attemptId: S_ID }), "unknown");
    assert.equal(await cache.verifyStoppedSourceCovered(bare, BRANCH, KEY, { runId: RUN, clonePath: P_PATH, attemptId: P_ID }, SHA, { runId: RUN, generation: 1 }, async () => true), "not_covered");
    const discard = await cache.discardStoppedSource({
      selector: { runId: RUN, generation: 1, attempt: P_ID }, confirm: "0".repeat(64), holds: { state: "read", holds: [] },
      isLivePath: () => false, quiesce: async () => true, barePath: bare,
    });
    assert.notEqual(discard.kind, "done");
    assert.deepEqual(config(), before);
  });

  it("the guard does not touch a bare without a restore entry", async () => {
    setKey(P_KEY, pRaw);
    await assert.doesNotReject(internals().writeRecovery(bare, BRANCH, { ...pJournal(), completionSourceFence: FENCE } as RecoveryJournalEntry));
    assert.equal(rawKey(P_KEY), JSON.stringify({ ...pJournal(), completionSourceFence: FENCE }));
  });
});

describe("(a) sites resolve to the successor's key", () => {
  const expectedS = () => ({ runId: RUN, clonePath: S_PATH, attemptId: S_ID });

  it("installCompletionSourceFence writes S's entry (fence last) and leaves P's raw value alone; the reads see S", async () => {
    compose({ stage: "ready", ledger: "live" });
    const installed = await cache.installCompletionSourceFence(bare, BRANCH, expectedS(), FENCE);
    assert.equal(serializeSourceFence(installed), serializeSourceFence(FENCE));
    assert.equal(rawKey(P_KEY), pRaw);
    assert.equal(rawKey(S_KEY), restoreJournalJson(basis(), "ready", sJournal({ completionSourceFence: FENCE }) as RecoveryJournalEntry));
    assert.equal(serializeSourceFence((await cache.readCompletionSourceFence(bare, BRANCH, expectedS()))!), serializeSourceFence(FENCE));
    assert.equal(await cache.readCompletionSourceFence(bare, BRANCH, { runId: RUN, clonePath: P_PATH, attemptId: P_ID }), undefined);
    const orphan = await cache.readOrphanJournalFence(bare, BRANCH, RUN, S_PATH);
    assert.equal(orphan.state === "unfenced" || orphan.state === "fenced", true);
    // Idempotent, and a different fence refuses; P is never fenced.
    await cache.installCompletionSourceFence(bare, BRANCH, expectedS(), FENCE);
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, expectedS(), { ...FENCE, sourceCoreDigest: "b".repeat(64) }), /different fence/);
    await assert.rejects(cache.installCompletionSourceFence(bare, BRANCH, { runId: RUN, clonePath: P_PATH, attemptId: P_ID }, FENCE), RestoreCompositeError);
    assert.equal(rawKey(P_KEY), pRaw);
  });

  it("recordMemoryPark and readConfirmedMemoryPark use S's entry with whole-value exact read-back; P's park write is refused", async () => {
    compose({ stage: "ready", ledger: "live" });
    assert.equal(await cache.recordMemoryPark(bare, BRANCH, expectedS(), PARK), "recorded");
    const expected = restoreJournalJson(basis(), "ready", sJournal({ memoryPark: PARK }) as RecoveryJournalEntry);
    assert.equal(rawKey(S_KEY), expected);
    assert.equal(rawKey(P_KEY), pRaw);
    // The settled two-phase write: draft, then confirmed, each fsynced and read back whole.
    const settled: MemoryParkRecord = { ...PARK, ack: "confirmed", settlement: { version: 1, confirmed: true, sources: [{ path: S_PATH, authorityKey: "1".padStart(64, "a"), attemptId: S_ID, epoch: UUID, seq: 1 }], evidence: {
      parkAck: PARK.binding, interventionIds: [], processJoin: "executor", commands: "not_required", steering: "joined", auxiliaries: "none",
      feedback: "drained", sinkReceipts: "joined", parkReap: "not_applicable" } } };
    const readbacks: Array<string | undefined> = [];
    assert.equal(await cache.recordMemoryPark(bare, BRANCH, expectedS(), settled, { readback: async read => { const v = await read(); readbacks.push(v); return v; } }), "settled");
    assert.equal(readbacks.length, 2);
    assert.ok(readbacks.every(v => v?.startsWith('{"restore":')));
    assert.equal(rawKey(P_KEY), pRaw);
    const confirmed = await cache.readConfirmedMemoryPark(bare, BRANCH, { ...expectedS(), record: settled });
    assert.equal(confirmed?.settlement?.confirmed, true);
    // A read-back that differs is refused, so nothing unverified is trusted.
    assert.equal(await cache.readConfirmedMemoryPark(bare, BRANCH, { ...expectedS(), record: settled }, { readback: async () => "tampered" }), undefined);
    const beforeP = config();
    await assert.rejects(cache.recordMemoryPark(bare, BRANCH, { runId: RUN, clonePath: P_PATH, attemptId: P_ID }, PARK), RestoreCompositeError);
    assert.deepEqual(config(), beforeP);
  });

  it("markRecoveryCapture rewrites S's own entry identically and refuses any other source", async () => {
    compose({ stage: "ready", ledger: "live" });
    const before = config();
    await cache.markRecoveryCapture(bare, S_PATH, BRANCH, RUN, S_ID);
    assert.deepEqual(config(), before);
    await assert.rejects(cache.markRecoveryCapture(bare, P_PATH.replace(P_ID, "x"), BRANCH, RUN), /cannot be overwritten/);
    assert.equal(rawKey(P_KEY), pRaw);
  });

  it("completion retirement metadata turns S's entry into the tombstone, never clearing P's key", async () => {
    compose({ stage: "ready", ledger: "retired" });
    assert.equal(await cache.completionSourceAlreadyRetired(bare, BRANCH, RUN, S_PATH, S_ID), true);
    assert.equal(rawKey(S_KEY), tombstoneValue());
    assert.equal(rawKey(P_KEY), pRaw);
    assert.equal(await cache.completionSourceAlreadyRetired(bare, BRANCH, RUN, S_PATH, S_ID), true, "idempotent on the tombstone");
    assert.equal(rawKey(S_KEY), tombstoneValue());
    // The predecessor is never reported retired through this path.
    assert.equal(await cache.completionSourceAlreadyRetired(bare, BRANCH, RUN, P_PATH, P_ID), false);
    assert.equal(rawKey(P_KEY), pRaw);
  });

  it("clearRetiredCompletionJournal crash-resume appends nothing for P and tombstones a fenced S", async () => {
    compose({ stage: "ready", ledger: "live" }, { completionSourceFence: FENCE });
    assert.equal(await cache.clearRetiredCompletionJournal(bare, BRANCH, RUN, S_PATH, S_ID, FENCE, { retiring: true }), true);
    assert.equal(rawKey(S_KEY), tombstoneValue());
    assert.equal(rawKey(P_KEY), pRaw);
    assert.equal(await cache.clearRetiredCompletionJournal(bare, BRANCH, RUN, P_PATH, P_ID, FENCE, { retiring: true }), false);
  });

  it("completionSourceJournalAbsent sees S's entry as the journal naming S and P as untouched", async () => {
    compose({ stage: "ready", ledger: "live" });
    assert.equal(await cache.completionSourceJournalAbsent(bare, BRANCH, RUN, S_PATH, S_ID), false);
    compose({ stage: "retired", ledger: "retired" });
    assert.equal(await cache.completionSourceJournalAbsent(bare, BRANCH, RUN, S_PATH, S_ID), true);
  });

  it("retireRunnerClone refuses P and any non-completion retirement under a composite, and another run is blocked", async () => {
    compose({ stage: "ready", ledger: "live" });
    const before = config();
    await assert.rejects(cache.retireRunnerClone(bare, P_PATH, BRANCH, RUN, { discard: true, attemptId: P_ID }), (e: unknown) => e instanceof RestoreCompositeError && !(e instanceof RestoreCompositeBlockedError));
    await assert.rejects(cache.retireRunnerClone(bare, S_PATH, BRANCH, RUN, { discard: true, attemptId: S_ID }), RestoreCompositeError);
    await assert.rejects(cache.retireRunnerClone(bare, P_PATH, BRANCH, OTHER_RUN, { discard: true }), RestoreCompositeBlockedError);
    assert.deepEqual(config(), before);
  });
});

describe("transitions of the restore entry", () => {
  const reopen = (): GitCache => new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());

  it("seeding -> ready -> tombstone -> cleared, each one validated write with read-back, across restarts; P's key is never written", async () => {
    setKey(P_KEY, pRaw);
    addLedger(P_ID, P_PATH, "live");
    assert.deepEqual(await cache.resolveBranchJournal(bare, BRANCH).then(r => r.kind), "plain");
    // The entry cannot exist before S's live ledger entry.
    await assert.rejects(cache.advanceRestoreEntry(bare, BRANCH, "seeding", { basis: basis(), journal: sJournal() as RecoveryJournalEntry }), RestoreCompositeError);
    assert.equal(rawKey(S_KEY), undefined);
    addLedger(S_ID, S_PATH, "live");
    await cache.advanceRestoreEntry(bare, BRANCH, "seeding", { basis: basis(), journal: sJournal() as RecoveryJournalEntry });
    assert.equal(rawKey(S_KEY), entryValue("seeding"));
    let again = reopen();
    assert.equal((await again.resolveBranchJournal(bare, BRANCH) as { stage: string }).stage, "seeding");
    await assert.rejects(cache.advanceRestoreEntry(bare, BRANCH, "retired"), RestoreCompositeError);
    await assert.rejects(cache.advanceRestoreEntry(bare, BRANCH, "cleared"), RestoreCompositeError);
    await assert.rejects(cache.advanceRestoreEntry(bare, BRANCH, "seeding", { basis: basis(), journal: sJournal() as RecoveryJournalEntry }), RestoreCompositeError);

    await cache.advanceRestoreEntry(bare, BRANCH, "ready");
    assert.equal(rawKey(S_KEY), entryValue("ready"));
    again = reopen();
    assert.equal((await again.resolveBranchJournal(bare, BRANCH) as { stage: string }).stage, "ready");
    // ready -> tombstone needs the retired ledger entry (S's retirement appends it first).
    await assert.rejects(cache.advanceRestoreEntry(bare, BRANCH, "retired"), RestoreCompositeError);
    assert.equal(rawKey(S_KEY), entryValue("ready"));
    addLedger(S_ID, S_PATH, "retired");
    assert.equal((await reopen().resolveBranchJournal(bare, BRANCH) as { stage: string }).stage, "ready");
    await cache.advanceRestoreEntry(bare, BRANCH, "retired");
    assert.equal(rawKey(S_KEY), tombstoneValue());
    await cache.advanceRestoreEntry(bare, BRANCH, "retired");
    assert.equal(rawKey(S_KEY), tombstoneValue(), "idempotent");
    assert.equal((await reopen().resolveBranchJournal(bare, BRANCH) as { stage: string }).stage, "retired");
    assert.equal(rawKey(P_KEY), pRaw);

    await cache.advanceRestoreEntry(bare, BRANCH, "cleared");
    assert.equal(rawKey(S_KEY), undefined);
    assert.equal(rawKey(P_KEY), pRaw);
    assert.equal((await reopen().resolveBranchJournal(bare, BRANCH)).kind, "plain");
  });

  it("ready + retired clears directly (the guarded discard of S), and a ready entry with a live attempt does not", async () => {
    compose({ stage: "ready", ledger: "live" });
    await assert.rejects(cache.advanceRestoreEntry(bare, BRANCH, "cleared"), RestoreCompositeError);
    addLedger(S_ID, S_PATH, "retired");
    await cache.advanceRestoreEntry(bare, BRANCH, "cleared");
    assert.equal(rawKey(S_KEY), undefined);
    assert.equal(rawKey(P_KEY), pRaw);
  });

  it("refuses a seeding entry whose basis digest is not P's raw value", async () => {
    setKey(P_KEY, pRaw);
    addLedger(P_ID, P_PATH, "live");
    addLedger(S_ID, S_PATH, "live");
    await assert.rejects(cache.advanceRestoreEntry(bare, BRANCH, "seeding", { basis: basis("other"), journal: sJournal() as RecoveryJournalEntry }), /predecessor journal changed/);
    assert.equal(rawKey(S_KEY), undefined);
  });
});

describe("enumerators exclude the predecessor's path", () => {
  it("the retention sweep keeps S's path, and the restore prefix is in the inventory's metadata caps", async () => {
    compose({ stage: "ready", ledger: "live" });
    const paths = await internals().journaledClonePaths(bare);
    assert.ok(paths.has(S_PATH) && paths.has(P_PATH));
    compose({ stage: "retired", ledger: "retired" });
    assert.ok(!(await internals().journaledClonePaths(bare)).has(S_PATH));
  });

  it("the inventory reader reports S and the predecessor, and never stats the predecessor or its head", async () => {
    compose({ stage: "ready", ledger: "live" });
    fs.mkdirSync(path.join(S_PATH, ".git"), { recursive: true });
    fs.writeFileSync(path.join(S_PATH, ".git", "HEAD"), `${SHA}\n`);
    const trap = trapPredecessor();
    let heads;
    try { heads = await cache.readInventoryCloneHeads(bare, RUN); } finally { trap.restore(); }
    assert.deepEqual(trap.hits, []);
    assert.ok(heads.kind === "verified", JSON.stringify(heads));
    assert.deepEqual(heads.clones.map(c => c.clonePath), [S_PATH]);
    assert.deepEqual(heads.heads, [SHA]);
    assert.deepEqual(heads.retainedPredecessors, [{ clonePath: P_PATH, branch: BRANCH, runId: RUN, attemptId: P_ID, generation: 1 }]);
    // Without a composite nothing about the shape changes.
    setKey(S_KEY, "");
    const plain = await cache.readInventoryCloneHeads(bare, RUN);
    assert.ok(plain.kind === "verified" && plain.retainedPredecessors === undefined);
  });

  it("a cloneIdentity read under a restore composite reports retainedPredecessors and never stats or returns P's path", async () => {
    compose({ stage: "ready", ledger: "live" });
    fs.mkdirSync(path.join(S_PATH, ".git"), { recursive: true });
    fs.writeFileSync(path.join(S_PATH, ".git", "HEAD"), `${SHA}\n`);
    const expected = [{ clonePath: P_PATH, branch: BRANCH, runId: RUN, attemptId: P_ID, generation: 1 }];
    const reads: Array<[string, () => ReturnType<GitCache["readInventoryCloneHeads"]>]> = [
      ["options form", () => cache.readInventoryCloneHeads(bare, RUN, { cloneIdentity: true })],
      ["boolean form", () => cache.readInventoryCloneHeads(bare, RUN, true)],
      ["under the lock", () => cache.withBareLock(bare, () => cache.readInventoryCloneHeadsUnderLock(bare, RUN, true))],
    ];
    for (const [name, read] of reads) {
      const trap = trapPredecessor();
      let heads;
      try { heads = await read(); } finally { trap.restore(); }
      assert.deepEqual(trap.hits, [], `${name}: P is never touched`);
      assert.ok(heads.kind === "verified", `${name}: ${JSON.stringify(heads)}`);
      assert.deepEqual(heads.retainedPredecessors, expected, name);
      assert.ok(!heads.clones.some(c => c.clonePath === P_PATH), `${name}: P's path is not a clone entry`);
      assert.ok(!JSON.stringify(heads.clones).includes(P_PATH), `${name}: P's path is not returned among the clones`);
      assert.ok(!(heads.foreignOwners ?? []).some(o => JSON.stringify(o).includes(P_PATH)), `${name}: nor among the foreign owners`);
    }
  });

  it("the retained-source inspector lists the predecessor without a presence probe", async () => {
    compose({ stage: "ready", ledger: "live" });
    const trap = trapPredecessor();
    let gathered;
    try { gathered = await internals().withLock(bare, () => internals().gatherRetainedUnderLock(bare)); } finally { trap.restore(); }
    assert.deepEqual(trap.hits, []);
    const p = gathered.records.find(r => r.parts.some(part => part.clonePath === P_PATH))!;
    assert.equal(p.parts[0]!.presence, "unsafe");
    assert.ok(p.problems.includes("restore_predecessor_retained"));
    assert.ok(!gathered.records.find(r => r.parts.some(part => part.clonePath === S_PATH))?.problems.includes("restore_predecessor_retained"));
  });
});

describe("discardRetainedSource refuses a restored branch with the real gathered digest", () => {
  it("throws RestoreCompositeError before quiescence or any delete (logged, result unknown), nothing written", async () => {
    compose({ stage: "ready", ledger: "live" });
    const gathered = await internals().withLock(bare, () => internals().gatherRetainedUnderLock(bare));
    const rec = gathered.records.find(r => r.parts.some(part => part.clonePath === P_PATH))!;
    assert.match(rec.digest, /^[0-9a-f]{64}$/);
    const warnings: unknown[][] = [];
    const log = nullLogger();
    log.warn = (...args: unknown[]) => { warnings.push(args); };
    const logged = new GitCache(fx.dataDir, log, undefined, testGitCacheOptions());
    const before = config();
    let quiesced = false;
    const result = await logged.discardStoppedSource({
      selector: { runId: RUN, generation: 1, attempt: P_ID }, confirm: rec.digest, holds: { state: "read", holds: [] },
      isLivePath: () => false, quiesce: async () => { quiesced = true; return true; }, barePath: bare,
    });
    assert.equal(result.kind, "unknown");
    assert.deepEqual(result.sources, []);
    assert.equal(quiesced, false);
    assert.ok(warnings.some(w => /restore composite/.test(JSON.stringify(w[1]))), JSON.stringify(warnings));
    assert.deepEqual(config(), before);
  });
});

describe("matchMaintenanceSourceFamily decides a restore value strictly", () => {
  const known = () => [{ runId: RUN, clonePath: P_PATH, attemptId: P_ID }];
  const call = (runId = RUN) => cache.matchMaintenanceSourceFamily(bare, BRANCH, KEY, runId, runId === RUN ? known() : [{ runId, clonePath: P_PATH, attemptId: P_ID }]);
  const plainFamily = (): void => {
    fs.mkdirSync(P_PATH, { recursive: true });
    setKey(P_KEY, pRaw);
    addLedger(P_ID, P_PATH, "live");
  };
  it("control: a plain family matches", async () => {
    plainFamily();
    assert.ok(await call());
  });
  it("a loose or undecodable restore value refuses the whole bare, whatever run it names", async () => {
    plainFamily();
    for (const remainder of [{}, { runId: 42 }, { runId: OTHER_RUN }, { runId: RUN }]) {
      setKey(S_KEY, JSON.stringify({ restore: basis(), restoreStage: "ready", ...remainder }));
      assert.equal(await call(), undefined, JSON.stringify(remainder));
    }
    setKey(S_KEY, JSON.stringify({ restore: basis(), restoreStage: "retired", successor: { runId: 42 } }));
    assert.equal(await call(), undefined);
  });
  it("a valid entry refuses only the run it names", async () => {
    plainFamily();
    setKey(S_KEY, entryValue("ready"));
    assert.equal(await call(), undefined);
    setKey(S_KEY, restoreJournalJson(basis(), "ready", { ...sJournal(), runId: OTHER_RUN } as RecoveryJournalEntry));
    assert.ok(await call());
  });
});

describe("compactAttemptLedger waits only for its own branch's restore entry", () => {
  const OTHER_BRANCH = "agent/issue-77";
  const otherLedger = `uzi-attempts.${OTHER_BRANCH}.entry`;
  const goneAttempt = formatAttemptId(new Date("2026-11-04T09:09:09Z"), 1, "aaaaaaaaaaaaaaaa");
  const goneEntry = (branchKey: string) => path.join(path.dirname(P_PATH), `${branchKey}.attempt-${goneAttempt}`);
  it("another branch's gone entry is still compacted while this branch has a restore entry; this branch's is not", async () => {
    compose({ stage: "ready", ledger: "live" });
    const gonePath = goneEntry("issue-77");
    gitIn(bare, ["config", "--add", otherLedger, JSON.stringify({ attemptId: goneAttempt, runId: "run-77", clonePath: gonePath, state: "retired" })]);
    await internals().compactAttemptLedger(bare, OTHER_BRANCH);
    assert.ok(!(rawKey(otherLedger) ?? "").includes(goneAttempt), "the other branch's gone entry was compacted");
    // This branch (restore entry present): compaction leaves the ledger as it was.
    gitIn(bare, ["config", "--add", LEDGER_KEY, JSON.stringify({ attemptId: goneAttempt, runId: "run-77", clonePath: gonePath, state: "retired" })]);
    const before = config();
    await internals().compactAttemptLedger(bare, BRANCH);
    assert.deepEqual(config(), before);
  });
});
