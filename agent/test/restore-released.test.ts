import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
import path from "node:path";
import {
  GitCache, RestoreCompositeError, type IndependentSeedInput, type PositiveOwedCandidateContext,
} from "../src/git.js";
import type { GitAuthorityHooks } from "../src/git-authority-hooks.js";
import { restoreDigest } from "../src/restore-journal.js";
import { attemptClonePath, formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #3644 amendment 14: the restore-aware exit recognises the predecessor P that v0.87.0-rc.2 RELEASED after the restore
// (a rollback to rc.2 and another claim of the branch), so the existing guarded discards (S, then P) can finish the restore.
// The states below are byte fixtures of what the archived rc.2 leaves (releaseAttemptInPlace: P's ledger `reclaimed` and its key
// cleared; retireRunnerClone: P moved to `runner-quarantine/<run>-<uuid>`, key cleared, ledger still `live`), then rc.2's
// markRecoveryCapture of the other run O writing O's journal into P's key. The rc.2 code itself is driven by
// independent-restore-rc2-compat.proof.ts; these tests run in CI with real Git and need no tag.
//
// Recognition grants no adoption, restore, FINAL, release or cleanup authority: the controls pin that every other path still
// refuses, that a partial, ambiguous or mismatched state refuses with nothing written, and that the holding copy is never touched.

function scrubGitEnv(base: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(base)) if (!key.startsWith("GIT_")) env[key] = value;
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
}
const ENV = scrubGitEnv(process.env);
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-3644";
const KEY = "issue-3644";
const RUN = "run-3644";
const OTHER = "run-9999";
const P_ID = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
const S_ID = formatAttemptId(new Date("2026-11-05T11:11:11Z"), 2, "fedcba9876543210");
const O_ID = formatAttemptId(new Date("2026-11-05T12:00:00Z"), 3, "aaaaaaaaaaaaaaaa");
const P_KEY = `uzi-recovery.${BRANCH}.clone`;
const S_KEY = `uzi-restore.${BRANCH}.journal`;
const LEDGER_KEY = `uzi-attempts.${BRANCH}.entry`;

let fx: Fixture;
let cache: GitCache;
let bare: string;
let P_PATH: string;
let S_PATH: string;
let O_PATH: string;
let CANONICAL: string;
let HOLDING_ROOT: string;
let pRaw: string;
let pinSha: string;
let syncs: string[];
let failSync: ((n: number) => boolean) | undefined;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const config = (): Buffer => fs.readFileSync(path.join(bare, "config"));
const rawKey = (key: string): string | undefined => { try { return gitIn(bare, ["config", "--get", key]) || undefined; } catch { return undefined; } };
const setKey = (key: string, value: string): void => { gitIn(bare, ["config", key, value]); };
const addLedger = (attemptId: string, clonePath: string, state: string, runId = RUN): void => {
  gitIn(bare, ["config", "--add", LEDGER_KEY, JSON.stringify({ attemptId, runId, clonePath, state })]);
};
const ledgerStates = (): Map<string, string> => {
  let values: string[] = [];
  try { values = gitIn(bare, ["config", "--get-all", LEDGER_KEY]).split("\n").filter(Boolean); } catch { /* none */ }
  return new Map(values.map(v => JSON.parse(v) as { attemptId: string; state: string }).map(e => [e.attemptId, e.state]));
};
const refList = (): string => gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]);
const skillsOf = (p: string): string => path.join(path.dirname(p), `.uzi-skills-${path.basename(p)}`);

/** Names, modes, sizes, mtimes and content digests of a tree: a byte-and-metadata inventory (absent tree: empty). */
function tree(dir: string): Record<string, string> {
  const out: Record<string, string> = {};
  const walk = (d: string): void => {
    for (const name of fs.readdirSync(d).sort()) {
      const p = path.join(d, name);
      const st = fs.lstatSync(p);
      const rel = path.relative(dir, p);
      if (st.isSymbolicLink()) out[rel] = `link ${fs.readlinkSync(p)}`;
      else if (st.isDirectory()) { out[rel] = `dir ${st.mode.toString(8)} ${st.mtimeMs}`; walk(p); }
      else out[rel] = `file ${st.mode.toString(8)} ${st.size} ${st.mtimeMs} ${createHash("sha256").update(fs.readFileSync(p)).digest("hex")}`;
    }
  };
  if (fs.existsSync(dir)) walk(dir);
  return out;
}

beforeEach(async () => {
  syncs = []; failSync = undefined;
  fx = makeFixture();
  const hooks: GitAuthorityHooks = {
    admitSourceUse: async () => {}, beginBareMutation: async () => ({ settle: async () => {} }),
    assertBareClear: async () => {}, beginDestructive: async () => ({ settle: async () => {} }),
  };
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
    authority: hooks,
    restoreDurability: {
      sync: async (target, kind) => {
        syncs.push(`${kind}:${path.basename(target)}`);
        if (failSync?.(syncs.length)) throw new Error(`injected ${kind} sync failure`);
        const handle = await fsp.open(target, fs.constants.O_RDONLY);
        try { await handle.sync(); } finally { await handle.close(); }
      },
    },
  }));
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const baseSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const fixtureClone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-fixture", noProofReseed, RUN)).path;
  gitIn(fixtureClone, [...IDENT, "commit", "--allow-empty", "-m", "the predecessor's saved work"]);
  pinSha = gitIn(fixtureClone, ["rev-parse", "HEAD"]);
  const context: PositiveOwedCandidateContext = { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: baseSha } };
  assert.equal((await cache.fetchAgentBranch(bare, fixtureClone, BRANCH, RUN, { context })).kind, "updated");
  gitIn(bare, ["config", "--unset-all", `uzi-trackowner.${BRANCH}.owner`]);
  CANONICAL = cache.runnerClonePath(bare, KEY);
  P_PATH = attemptClonePath(CANONICAL, P_ID);
  S_PATH = attemptClonePath(CANONICAL, S_ID);
  O_PATH = attemptClonePath(CANONICAL, O_ID);
  HOLDING_ROOT = path.join(fx.dataDir, "runner-quarantine");
  pRaw = JSON.stringify({ runId: RUN, clonePath: P_PATH, attemptId: P_ID });
  setKey(P_KEY, pRaw);
  addLedger(P_ID, P_PATH, "live");
  fs.mkdirSync(path.join(P_PATH, ".git"), { recursive: true });
  fs.writeFileSync(path.join(P_PATH, "later.txt"), "later bytes that were never saved");
  fs.mkdirSync(skillsOf(P_PATH), { recursive: true });
  fs.writeFileSync(path.join(skillsOf(P_PATH), "skill.md"), "skill");
  syncs = [];
});
afterEach(() => fx.cleanup());

/** P's clone root identity as the authority's slot-key walk would record it (P is on disk when a restore is seeded). */
function pRoot(): { dev: string; ino: string } {
  const st = fs.lstatSync(P_PATH, { bigint: true });
  return { dev: st.dev.toString(), ino: st.ino.toString() };
}
function seedInput(over: Partial<IndependentSeedInput> = {}): IndependentSeedInput {
  return {
    barePath: bare, branch: BRANCH, key: KEY, runId: RUN, attemptId: S_ID,
    predecessor: { clonePath: P_PATH, attemptId: P_ID, generation: 1, journalDigest: restoreDigest(pRaw), slotOwner: { worker: "worker-incarnation-1", epoch: "epoch-1" }, root: pRoot() },
    boundary: { predecessor: "c".repeat(64), successor: "d".repeat(64) },
    pin: { captureId: "capture-1", generation: 1, sourceSha: pinSha, finalization: true }, defaultBranch: "main",
    predecessorUnchanged: async () => true, census: async () => ({ ok: true }), beforeRestoreAttribution: async () => {},
    isLive: () => false, beforeSeed: async () => {}, ...over,
  };
}

type Mode = "in_place" | "holding";
type PKey = "cleared" | "other_run";
const oJournal = (mode: Mode): string => JSON.stringify(mode === "in_place"
  ? { runId: OTHER, clonePath: O_PATH, attemptId: O_ID } : { runId: OTHER, clonePath: CANONICAL });

/** One holding directory the way rc.2's retireRunnerClone names it: `<run sanitised>-<uuid>`, under a 0700 root. */
function holdingDir(run = RUN): string {
  fs.mkdirSync(HOLDING_ROOT, { recursive: true, mode: 0o700 });
  fs.chmodSync(HOLDING_ROOT, 0o700);
  const dir = path.join(HOLDING_ROOT, `${run.replace(/[^A-Za-z0-9_-]/g, "_")}-${randomUUID()}`);
  fs.mkdirSync(dir);
  return dir;
}

/**
 * The byte fixture of rc.2's end state after S's failure (S seeded by this build, on disk): the rc.2 reclaim as the other run O, then O's
 * seed and markRecoveryCapture. `in_place`: P's ledger `reclaimed` (P's bytes in place). `holding`: P's directory moved byte for byte to
 * one holding directory, ledger still `live`. P's key is cleared, or holds O's journal (rc.2's markRecoveryCapture writes it there).
 */
async function rc2Release(mode: Mode, pKey: PKey = "other_run", successorId = S_ID): Promise<{ holding?: string }> {
  await cache.seedIndependentAttempt(seedInput({ attemptId: successorId }));
  fs.writeFileSync(path.join(attemptClonePath(CANONICAL, successorId), "work.txt"), "the successor's own work");
  let holding: string | undefined;
  if (mode === "in_place") addLedger(P_ID, P_PATH, "reclaimed");
  else {
    holding = holdingDir();
    fs.rmdirSync(holding);
    fs.renameSync(P_PATH, holding);
    fs.rmSync(skillsOf(P_PATH), { recursive: true, force: true });
  }
  setKey(P_KEY, "");
  if (pKey === "other_run") {
    setKey(P_KEY, oJournal(mode));
    if (mode === "in_place") addLedger(O_ID, O_PATH, "live", OTHER);
  }
  return holding === undefined ? {} : { holding };
}

const live = { isLivePath: () => false, quiesce: async () => true, hasRetainedHomeMarker: async () => true };
type Internals = { gatherRetainedUnderLock(bare: string): Promise<{ records: Array<{ digest: string; parts: Array<{ clonePath: string }> }> }>;
  withLock<T>(bare: string, fn: () => Promise<T>): Promise<T>; journaledClonePaths(bare: string): Promise<Set<string>> };
const internals = (): Internals => cache as unknown as Internals;
async function digestOf(clonePath: string): Promise<string> {
  const g = await internals().withLock(bare, () => internals().gatherRetainedUnderLock(bare));
  return g.records.find(r => r.parts.some(p => p.clonePath === clonePath))?.digest ?? "no-record";
}
async function discard(clonePath: string, attempt: string, generation: number, over: Record<string, unknown> = {}) {
  return cache.discardStoppedSource({
    selector: { runId: RUN, generation, attempt }, confirm: (over.confirm as string | undefined) ?? await digestOf(clonePath), holds: { state: "read", holds: [] },
    lifecycle: { state: "terminal", status: "failed" }, barePath: bare, ...live, ...over,
  } as never);
}
const discardS = (over: Record<string, unknown> = {}) => discard(S_PATH, S_ID, 2, over);
const discardP = (over: Record<string, unknown> = {}) => discard(P_PATH, P_ID, 1, over);

interface World { config: Buffer; refs: string; p: Record<string, string>; s: Record<string, string>; holding: Record<string, string> }
const world = (): World => ({ config: config(), refs: refList(), p: tree(P_PATH), s: tree(S_PATH), holding: tree(HOLDING_ROOT) });

/** Both discards are refused (or unknown, never done), the strict resolver still throws, and nothing at all changed. `configFacts` says
 *  whether the bare's config alone still shows the released shape (the filesystem half then fails) or already refutes it. */
async function assertRefusedUnchanged(what: string, configFacts: boolean): Promise<void> {
  const before = world();
  for (const [attempt, p, g] of [[S_ID, S_PATH, 2], [P_ID, P_PATH, 1]] as const) {
    const r = await discard(p, attempt, g, { removeRetainedHomeMarker: async () => { throw new Error(`${what}: the marker must not be touched`); } });
    assert.ok(r.kind === "refused" || r.kind === "unknown", `${what}: discard of ${attempt} gave ${JSON.stringify(r)}`);
  }
  // The strict resolver's message is config-derived: it names the released shape exactly when the config facts hold.
  await assert.rejects(() => cache.resolveBranchJournal(bare, BRANCH), (err: unknown) => {
    assert.ok(err instanceof RestoreCompositeError, what);
    assert.equal(err.message.includes("(predecessor released or missing)"), configFacts, `${what}: ${err.message}`);
    return true;
  });
  const after = world();
  assert.deepEqual(after.config, before.config, `${what}: config byte-identical`);
  assert.equal(after.refs, before.refs, `${what}: refs`);
  assert.deepEqual(after.p, before.p, `${what}: P's tree`);
  assert.deepEqual(after.s, before.s, `${what}: S's tree`);
  assert.deepEqual(after.holding, before.holding, `${what}: the holding root`);
}

/** Every filesystem call that names a forbidden path is recorded and refused (promise and sync APIs). */
function trapPaths(forbidden: (resolved: string) => boolean): { hits: string[]; restore(): void } {
  const hits: string[] = [];
  const under = (p: unknown): boolean => (typeof p === "string" || p instanceof URL || Buffer.isBuffer(p)) && forbidden(path.resolve(p instanceof URL ? p.pathname : String(p)));
  const restores: Array<() => void> = [];
  const names = ["lstat", "stat", "open", "opendir", "readdir", "readFile", "access", "readlink", "realpath", "mkdir", "rm", "rename", "writeFile", "chmod", "unlink", "rmdir", "statfs"];
  const patch = (target: Record<string, unknown>, list: string[], label: string): void => {
    for (const name of list) {
      const original = target[name];
      if (typeof original !== "function") continue;
      target[name] = function (this: unknown, ...args: unknown[]) {
        if (under(args[0])) { hits.push(`${label}.${name}(${String(args[0])})`); throw new Error(`forbidden path touched: ${label}.${name}`); }
        return (original as (...a: unknown[]) => unknown).apply(this, args);
      };
      restores.push(() => { target[name] = original; });
    }
  };
  patch(fsp as unknown as Record<string, unknown>, names, "fs.promises");
  patch(fs as unknown as Record<string, unknown>, [...names.map(n => `${n}Sync`), "existsSync", "opendirSync"], "fs");
  syncBuiltinESMExports();
  return { hits, restore: () => { for (const r of restores.reverse()) r(); syncBuiltinESMExports(); } };
}

const MODES: Array<{ mode: Mode; pKey: PKey }> = [
  { mode: "in_place", pKey: "other_run" }, { mode: "in_place", pKey: "cleared" },
  { mode: "holding", pKey: "other_run" }, { mode: "holding", pKey: "cleared" },
];

for (const { mode, pKey } of MODES) {
  describe(`released predecessor, mode ${mode}, P's key ${pKey}`, () => {
    it("the strict resolver and every other consumer refuse (config only), the exit-only recognizer alone proceeds", async () => {
      const { holding } = await rc2Release(mode, pKey);
      const before = world();
      await assert.rejects(() => cache.resolveBranchJournal(bare, BRANCH), /predecessor released or missing/);
      // Discovery and the clone-path census read the config alone: no path under the runner root or the holding root is touched.
      const trap = trapPaths(p => [path.join(fx.dataDir, "runner"), HOLDING_ROOT].some(r => p === r || p.startsWith(r + path.sep)));
      try {
        await assert.rejects(() => cache.discoverOpaque(bare, BRANCH, RUN), RestoreCompositeError);
        await assert.rejects(() => cache.discoverOpaque(bare, BRANCH, OTHER), RestoreCompositeError);
        await assert.rejects(() => internals().journaledClonePaths(bare), RestoreCompositeError);
        await assert.rejects(() => cache.resolveBranchJournal(bare, BRANCH), RestoreCompositeError);
      } finally { trap.restore(); }
      assert.deepEqual(trap.hits, []);
      assert.deepEqual(world(), before, "refusals write nothing");
      void holding;
    });

    it("discard S writes the tombstone (the basis survives), then discard P retires the restore without writing P's key and never touches the holding root", async () => {
      const { holding } = await rc2Release(mode, pKey);
      const pKeyBefore = rawKey(P_KEY);
      const holdingBefore = tree(HOLDING_ROOT);
      const pBytes = tree(P_PATH);
      // P first: S is still live.
      const early = await discardP();
      assert.equal(early.kind, "unknown", "P's exit waits for S's");
      assert.deepEqual(tree(P_PATH), pBytes);
      const s = await discardS({ removeRetainedHomeMarker: async () => { throw new Error("S must not remove the marker"); } });
      assert.equal(s.kind, "done", JSON.stringify(s));
      assert.equal(fs.existsSync(S_PATH), false);
      assert.equal(ledgerStates().get(S_ID), "retired");
      assert.ok(rawKey(S_KEY)?.includes('"restoreStage":"retired"'), "S's key holds the tombstone");
      assert.equal(rawKey(P_KEY), pKeyBefore, "P's key is byte-identical after S's exit");
      assert.deepEqual(tree(P_PATH), pBytes, "P's bytes are untouched by S's exit");
      assert.deepEqual(tree(HOLDING_ROOT), holdingBefore);
      // The ref protection of the basis outlives S's discard.
      assert.equal(await cache.restoreKeepsRecoveryRef(bare, RUN, { sha: pinSha }), true);
      assert.equal(await cache.restoreKeepsRecoveryRef(bare, RUN, { generation: 1 }), true);
      // P's discard: an open hold of the run refuses until it is closed.
      const held = await discardP({ holds: { state: "read", holds: [{ id: "hold-1", generation: 1 }] } });
      assert.equal(held.kind, "refused");
      assert.equal(rawKey(P_KEY), pKeyBefore);
      const removed: string[] = [];
      const p = await discardP({ removeRetainedHomeMarker: async (run: string) => { removed.push(run); } });
      assert.equal(p.kind, "done", JSON.stringify(p));
      assert.deepEqual(removed, [RUN]);
      assert.equal(rawKey(S_KEY) ?? "", "");
      assert.equal(ledgerStates().get(P_ID), "retired");
      assert.equal(rawKey(P_KEY), pKeyBefore, "P's key is byte-identical: it may hold another run's journal");
      assert.deepEqual(tree(HOLDING_ROOT), holdingBefore, "the holding root is byte-identical");
      if (mode === "holding") {
        assert.equal(p.kind === "done" && p.holding, path.basename(holding!), "the holding directory's name is reported");
        assert.equal(fs.existsSync(holding!), true);
      } else {
        assert.equal(p.kind === "done" && p.holding, undefined);
        assert.equal(fs.existsSync(P_PATH), false, "the owner's authorised discard removed P");
        assert.equal(fs.existsSync(skillsOf(P_PATH)), false);
      }
      assert.equal(fs.existsSync(path.join(holding ?? P_PATH, "later.txt")) || mode === "in_place", true);
      // The branch is released: the strict resolver reads P's key as an ordinary plain journal (or none) again.
      const after = await cache.resolveBranchJournal(bare, BRANCH);
      assert.equal(after.kind, pKey === "other_run" ? "plain" : "none");
      if (pKey === "other_run") assert.equal((await cache.discoverOpaque(bare, BRANCH, OTHER)).kind, "sameRun");
      // A finished exit is not found.
      const third = await discardP({ confirm: "x" });
      assert.equal(third.kind, "refused");
    });
  });
}

describe("released predecessor: crash windows of the exits", () => {
  it("S: a crash after S's retired ledger value and before the tombstone resumes through P's exit, which retires the tombstone-less state", async () => {
    await rc2Release("in_place");
    // S's exit up to its retired ledger value, then the tombstone write dies (the sync seam fails after the append's own sync).
    failSync = n => n >= 2;
    const crashed = await discardS();
    assert.equal(crashed.kind, "unknown");
    failSync = undefined;
    assert.equal(ledgerStates().get(S_ID), "retired");
    // ready + retired is a valid pair; P's exit accepts it and ends the restore without a tombstone ever existing.
    const p = await discardP();
    assert.equal(p.kind, "done", JSON.stringify(p));
    assert.equal(rawKey(S_KEY) ?? "", "");
    assert.equal(ledgerStates().get(P_ID), "retired");
  });

  it("P: a failure inside the rewrite changes nothing and the same call completes; a failure after the rename and before the sync keeps the marker", async () => {
    await rc2Release("holding");
    assert.equal((await discardS()).kind, "done");
    const before = config();
    const keyBefore = rawKey(P_KEY);
    const i = cache as unknown as { rewriteConfigAtomically(...a: unknown[]): Promise<void> };
    const original = i.rewriteConfigAtomically;
    i.rewriteConfigAtomically = async () => { throw new Error("injected rewrite failure"); };
    const marker = { present: true, removals: 0 };
    const hooks = { hasRetainedHomeMarker: async () => marker.present, removeRetainedHomeMarker: async () => { marker.present = false; marker.removals++; } };
    const first = await discardP(hooks);
    i.rewriteConfigAtomically = original;
    assert.equal(first.kind, "unknown");
    assert.deepEqual(config(), before);
    assert.equal(marker.present, true);
    const confirm = await digestOf(P_PATH);
    syncs = [];
    failSync = () => true;
    const second = await discardP({ ...hooks, confirm });
    failSync = undefined;
    assert.equal(second.kind, "unknown");
    assert.equal(second.kind === "unknown" && second.metadata, "kept", "the retirement is not reported until it is durable");
    assert.equal(marker.present, true);
    assert.equal(rawKey(P_KEY), keyBefore);
    const again = await discardP({ ...hooks, confirm });
    assert.equal(again.kind, "done", JSON.stringify(again));
    assert.equal(marker.present, false);
    assert.equal(marker.removals, 1);
    assert.equal(ledgerStates().get(P_ID), "retired");
    assert.equal(rawKey(P_KEY), keyBefore);
  });

  it("P: a failure of the marker hook after the durable rewrite reports retired, and the retry removes the marker and is then not found", async () => {
    await rc2Release("in_place");
    assert.equal((await discardS()).kind, "done");
    const marker = { present: true, removals: 0 };
    let fail = true;
    const hooks = { hasRetainedHomeMarker: async () => marker.present,
      removeRetainedHomeMarker: async () => { if (fail) throw new Error("marker busy"); marker.present = false; marker.removals++; } };
    const confirm = await digestOf(P_PATH);
    const first = await discardP({ ...hooks, confirm });
    assert.equal(first.kind, "unknown");
    assert.equal(first.kind === "unknown" && first.metadata, "retired");
    assert.equal(fs.existsSync(P_PATH), false);
    fail = false;
    const retry = await discardP({ ...hooks, confirm });
    assert.equal(retry.kind, "done", JSON.stringify(retry));
    assert.equal(marker.present, false);
    const third = await discardP({ ...hooks, confirm });
    assert.equal(third.kind === "refused" && third.reason, "not_found");
    assert.equal(marker.removals, 1);
  });
});

describe("controls: partial, ambiguous or mismatched states stay refused with nothing written", () => {
  it("C1 partial transition (mode H): no holding directory, two, a malformed name, a different ctime era, and P's path present", async () => {
    await rc2Release("holding");
    // The directory rc.2 moved P to is removed: key cleared + ledger live + no holding directory.
    const dir = fs.readdirSync(HOLDING_ROOT)[0]!;
    const aside = path.join(fx.dataDir, "aside");
    fs.renameSync(path.join(HOLDING_ROOT, dir), aside);
    await assertRefusedUnchanged("no holding directory", true);
    fs.renameSync(aside, path.join(HOLDING_ROOT, dir));
    // Two directories of the run.
    const second = holdingDir();
    await assertRefusedUnchanged("two holding directories", true);
    fs.rmSync(second, { recursive: true });
    // A malformed name of the run.
    const bad = path.join(HOLDING_ROOT, `${RUN}-not-a-uuid`);
    fs.mkdirSync(bad);
    await assertRefusedUnchanged("a malformed holding name", true);
    fs.rmSync(bad, { recursive: true });
    // Another run's directory does not stand in for this run's.
    fs.renameSync(path.join(HOLDING_ROOT, dir), path.join(HOLDING_ROOT, dir.replace(RUN, "run-4444")));
    await assertRefusedUnchanged("another run's holding directory", true);
    fs.renameSync(path.join(HOLDING_ROOT, dir.replace(RUN, "run-4444")), path.join(HOLDING_ROOT, dir));
    // P's path present again: P was not moved.
    fs.mkdirSync(path.join(P_PATH, ".git"), { recursive: true });
    await assertRefusedUnchanged("P's path present", true);
  });

  it("C1 (mode H binding, amendment 14): the sole run-matching holding directory must be P's own root (same dev/ino), not another directory", async () => {
    await rc2Release("holding");
    const dir = fs.readdirSync(HOLDING_ROOT)[0]!;
    const real = path.join(HOLDING_ROOT, dir);
    const pIdentity = fs.lstatSync(real, { bigint: true });
    assert.equal(String(pIdentity.ino), JSON.parse(rawKey(S_KEY)!).restore.predecessor.root.ino, "control: the recorded root is the inode of the holding copy");
    // A different, perfectly well-formed, run-prefixed directory (S's own retired holding copy would look like this) stands in for P's.
    const aside = path.join(fx.dataDir, "p-aside");
    fs.renameSync(real, aside);
    const impostor = holdingDir();
    assert.notEqual(fs.lstatSync(impostor, { bigint: true }).ino, pIdentity.ino);
    await assertRefusedUnchanged("a holding directory with another inode", true);
    // The same device but the wrong inode is refused, and so is a missing directory.
    fs.rmSync(impostor, { recursive: true });
    await assertRefusedUnchanged("no holding directory", true);
    // Control: P's own directory, renamed back (same inode, new ctime), is accepted again.
    fs.renameSync(aside, real);
    assert.equal((await discardS()).kind, "done");
    const p = await discardP();
    assert.equal(p.kind === "done" && p.holding, dir, JSON.stringify(p));
  });

  it("C1 (mode H binding): a basis recording another root inode refuses even though the holding directory is P's", async () => {
    await cache.seedIndependentAttempt(seedInput({ predecessor: { ...seedInput().predecessor, root: { dev: pRoot().dev, ino: String(BigInt(pRoot().ino) + 1n) } } }));
    const holding = holdingDir();
    fs.rmdirSync(holding);
    fs.renameSync(P_PATH, holding);
    fs.rmSync(skillsOf(P_PATH), { recursive: true, force: true });
    setKey(P_KEY, "");
    await assertRefusedUnchanged("a basis whose root differs from the holding directory", true);
  });

  it("C1 (mode H attribution): a holding root that is not a real 0700 directory, or a holding entry that is not a real directory, refuses", async () => {
    await rc2Release("holding");
    const dir = fs.readdirSync(HOLDING_ROOT)[0]!;
    fs.chmodSync(HOLDING_ROOT, 0o755);
    await assertRefusedUnchanged("holding root mode 0755", true);
    fs.chmodSync(HOLDING_ROOT, 0o700);
    const full = path.join(HOLDING_ROOT, dir);
    const moved = path.join(fx.dataDir, "held-real");
    fs.renameSync(full, moved);
    fs.symlinkSync(moved, full);
    await assertRefusedUnchanged("holding entry is a symlink", true);
    fs.unlinkSync(full);
    fs.renameSync(moved, full);
    const asideRoot = path.join(fx.dataDir, "quarantine-real");
    fs.renameSync(HOLDING_ROOT, asideRoot);
    fs.symlinkSync(asideRoot, HOLDING_ROOT);
    await assertRefusedUnchanged("holding root is a symlink", true);
  });

  it("C1 (mode H attribution): a holding directory older than S's attempt id is not attributed", async () => {
    // S's id encodes a far-future instant, so the holding directory (ctime: now) predates S: it cannot be rc.2's move for this restore.
    const future = formatAttemptId(new Date("2999-01-01T00:00:00Z"), 2, "fedcba9876543210");
    const futurePath = attemptClonePath(CANONICAL, future);
    await rc2Release("holding", "other_run", future);
    await assert.rejects(() => cache.resolveBranchJournal(bare, BRANCH), /predecessor released or missing/, "the config facts hold (the message is config-derived)");
    const before = world();
    const r = await discard(futurePath, future, 2);
    assert.ok(r.kind === "refused" || r.kind === "unknown", JSON.stringify(r));
    assert.deepEqual(config(), before.config);
    assert.deepEqual(tree(HOLDING_ROOT), before.holding);
  });

  it("C4 (valueless P key): a P key that exists without a value is ambiguous evidence and refuses", async () => {
    await rc2Release("in_place", "cleared");
    gitIn(bare, ["config", "--unset-all", P_KEY]);
    fs.appendFileSync(path.join(bare, "config"), `[uzi-recovery "${BRANCH}"]\n\tclone\n`);
    await assertRefusedUnchanged("a valueless P key", false);
  });

  it("the released state is re-proven after the caller's quiescence: a holding directory that changes meanwhile refuses before any byte is deleted", async () => {
    await rc2Release("holding");
    assert.equal((await discardS()).kind, "done");
    const before = world();
    const extra = (): string => holdingDir();
    const r = await discardP({ quiesce: async () => { extra(); return true; } });
    assert.equal(r.kind, "refused", JSON.stringify(r));
    assert.equal(r.kind === "refused" && r.reason, "restore_state_changed");
    assert.deepEqual(config(), before.config);
    assert.equal(ledgerStates().get(P_ID), "live", "P stays live");
    assert.equal(rawKey(S_KEY)?.includes("retired"), true);
  });

  it("the released state is re-proven after the caller's quiescence: the holding directory renamed to another valid name meanwhile refuses restore_state_changed", async () => {
    const { holding } = await rc2Release("holding");
    assert.equal((await discardS()).kind, "done");
    const renamed = path.join(HOLDING_ROOT, `${RUN.replace(/[^A-Za-z0-9_-]/g, "_")}-${randomUUID()}`);
    const r = await discardP({ quiesce: async () => { fs.renameSync(holding!, renamed); return true; } });
    assert.equal(r.kind === "refused" && r.reason, "restore_state_changed", JSON.stringify(r));
    assert.equal(fs.existsSync(renamed), true, "the holding copy is untouched");
    assert.equal(ledgerStates().get(P_ID), "live", "P stays live");
    assert.equal(rawKey(S_KEY)?.includes("retired"), true);
  });

  it("C2 (Q4): ledger reclaimed with P's key still P's journal is an ordinary composite and the normal exit reaches done", async () => {
    await cache.seedIndependentAttempt(seedInput());
    addLedger(P_ID, P_PATH, "reclaimed");
    const r = await cache.resolveBranchJournal(bare, BRANCH);
    assert.equal(r.kind, "composite", "the strict resolver accepts it: the released variant is not produced");
    assert.equal(rawKey(P_KEY), pRaw);
    const s = await discardS();
    assert.equal(s.kind, "done", JSON.stringify(s));
    assert.equal(rawKey(P_KEY), pRaw, "S's exit never writes P's key");
    const removed: string[] = [];
    const p = await discardP({ removeRetainedHomeMarker: async (run: string) => { removed.push(run); } });
    assert.equal(p.kind, "done", JSON.stringify(p));
    assert.deepEqual(removed, [RUN]);
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "none");
    assert.equal(ledgerStates().get(P_ID), "retired");
    assert.equal(p.kind === "done" && p.holding, undefined);
  });

  it("C2 (mode H analog of Q4): P moved but its key not cleared is an ordinary composite too; it is not recognised as released and the holding copy is never touched", async () => {
    await cache.seedIndependentAttempt(seedInput());
    const holding = holdingDir();
    fs.rmdirSync(holding);
    fs.renameSync(P_PATH, holding);
    const heldTree = tree(holding);
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "composite");
    assert.equal((await discardS()).kind, "done");
    const p = await discardP();
    assert.equal(p.kind === "done" && p.holding, undefined, "the released arm was never used");
    assert.deepEqual(tree(holding), heldTree);
  });

  it("C3 mismatched identities: P's ledger value of another run, path or attempt; S's ledger value; each refuses", async () => {
    await rc2Release("in_place");
    addLedger(P_ID, P_PATH, "reclaimed", OTHER);
    await assertRefusedUnchanged("P's ledger names another run", false);
    addLedger(P_ID, `${P_PATH}x`, "reclaimed");
    await assertRefusedUnchanged("P's ledger names another path", false);
    addLedger(P_ID, P_PATH, "reclaimed");
    addLedger(S_ID, S_PATH, "live", OTHER);
    await assertRefusedUnchanged("S's ledger names another run", false);
    addLedger(S_ID, `${S_PATH}x`, "live");
    await assertRefusedUnchanged("S's ledger names another path", false);
    addLedger(S_ID, S_PATH, "live");
    addLedger(P_ID, P_PATH, "abandoned");
    await assertRefusedUnchanged("P's ledger is abandoned", false);
  });

  it("C3 (mode H): P's ledger value of another run refuses even with a holding directory", async () => {
    await rc2Release("holding");
    addLedger(P_ID, P_PATH, "live", OTHER);
    await assertRefusedUnchanged("P's live ledger names another run", false);
  });

  const third = formatAttemptId(new Date("2026-11-05T13:00:00Z"), 3, "bbbbbbbbbbbbbbbb");
  const fourth = formatAttemptId(new Date("2026-11-05T13:30:00Z"), 3, "cccccccccccccccc");
  const DUPLICATES: Array<[string, () => void]> = [
    ["a second attempt names P's path", () => addLedger(third, P_PATH, "live")],
    ["a retired second attempt still names P's path", () => addLedger(third, P_PATH, "retired")],
    ["a second attempt names S's path", () => addLedger(third, S_PATH, "retired")],
    ["a third live attempt of the run at P's generation or later", () => addLedger(fourth, attemptClonePath(CANONICAL, fourth), "live")],
    ["a descriptor of the run (the rc.2 detach shape)", () => setKey(`uzi-retained.${RUN}.journal`, "{}")],
    ["a stopped marker of the run", () => setKey(`uzi-stopped.${RUN}.entry`, "{}")],
    ["a repeated restore key", () => gitIn(bare, ["config", "--add", S_KEY, rawKey(S_KEY)!])],
  ];
  for (const [name, mutate] of DUPLICATES) {
    it(`C4 duplicates: ${name}`, async () => {
      await rc2Release("in_place");
      mutate();
      await assertRefusedUnchanged(name, false);
    });
  }

  it("C4 (N2): an older retired or pre-generation attempt of the run does not block, a non-retired one at or above P's generation does", async () => {
    await rc2Release("in_place");
    const older = formatAttemptId(new Date("2026-11-04T09:00:00Z"), 0, "dddddddddddddddd");
    addLedger(older, attemptClonePath(CANONICAL, older), "reclaimed");
    // generation 0 < G: not an attribution of the restore.
    const s = await discardS();
    assert.equal(s.kind, "done", JSON.stringify(s));
  });

  it("C5: P's key holding a journal of the run (any digest) or an undecodable value is not the released shape", async () => {
    await rc2Release("in_place");
    setKey(P_KEY, JSON.stringify({ runId: RUN, clonePath: P_PATH, attemptId: P_ID, peeledMarkers: ["a".repeat(40)] }));
    await assertRefusedUnchanged("a journal of the run with another digest", false);
    setKey(P_KEY, JSON.stringify({ runId: RUN, clonePath: CANONICAL }));
    await assertRefusedUnchanged("another journal of the run that names neither P nor S", false);
    setKey(P_KEY, "{not json");
    await assertRefusedUnchanged("an undecodable value", false);
    setKey(P_KEY, JSON.stringify({ runId: OTHER, clonePath: P_PATH, attemptId: P_ID }));
    await assertRefusedUnchanged("another run's journal naming P's path", false);
    setKey(P_KEY, JSON.stringify({ runId: OTHER, clonePath: path.join(HOLDING_ROOT, "x") }));
    await assertRefusedUnchanged("another run's journal naming a holding directory", false);
    gitIn(bare, ["config", "--add", P_KEY, oJournal("in_place")]);
    await assertRefusedUnchanged("a repeated P key", false);
  });

  it("C6 unsafe adoption on the released state: no other path gains authority and nothing is written", async () => {
    await rc2Release("in_place");
    const before = world();
    const reseed = { beforeFree: async () => {}, isLive: () => false };
    const attempt = (id: string) => ({ attemptId: id, isLive: () => false, beforeSeed: async () => {} });
    const newId = formatAttemptId(new Date("2026-11-06T12:00:00Z"), 4, "eeeeeeeeeeeeeeee");
    const expected = { runId: RUN, clonePath: P_PATH, attemptId: P_ID };
    // Each is refused by a typed guard (the restore composite error, the seed's typed window, or the existing foreign-journal block for
    // a claim that meets O's journal in P's key), never by an unrelated failure and never by success.
    const guarded = async (what: string, fn: () => Promise<unknown>): Promise<void> => {
      await assert.rejects(fn, (err: unknown) => {
        const e = err as Error & { cause?: unknown; reason?: string };
        const typed = err instanceof RestoreCompositeError || e.cause instanceof RestoreCompositeError || (e.name === "RestoreSeedError" && e.reason === "restore_entry_exists") ||
          (what.startsWith("runnerCloneForBranch") && e.name === "ForeignCaptureBlockedError");
        assert.ok(typed, `${what}: ${e.name}: ${e.message}`);
        return true;
      });
    };
    await guarded("discoverOpaque(R)", () => cache.discoverOpaque(bare, BRANCH, RUN));
    await guarded("discoverOpaque(O)", () => cache.discoverOpaque(bare, BRANCH, OTHER));
    for (const run of [RUN, OTHER]) {
      await guarded(`runnerCloneForBranch(${run})`, () => cache.runnerCloneForBranch(bare, BRANCH, KEY, reseed, run, false, undefined, attempt(newId) as never));
    }
    await guarded("seedIndependentAttempt", () => cache.seedIndependentAttempt(seedInput({ attemptId: newId })));
    await guarded("reserveRecoveryIteration", () => cache.reserveRecoveryIteration(bare, BRANCH, KEY, expected, 1000, 2));
    await guarded("markRecoveryCapture(P)", () => cache.markRecoveryCapture(bare, P_PATH, BRANCH, RUN, P_ID));
    await guarded("markRecoveryCapture(O)", () => cache.markRecoveryCapture(bare, O_PATH, BRANCH, OTHER, O_ID));
    const heads = await cache.readInventoryCloneHeads(bare, RUN);
    assert.equal(heads.kind, "unknown");
    await guarded("installCompletionSourceFence", () => cache.installCompletionSourceFence(bare, BRANCH, expected,
      { version: 1, sourceUseEpoch: "0123abcd-0123-4abc-8def-0123456789ab", sourceCoreDigest: "a".repeat(64) }));
    await guarded("recordRecoveryCapture", () => cache.recordRecoveryCapture(bare, BRANCH, KEY, expected, 1, pinSha));
    for (const discard of [false, true]) {
      await guarded(`retireRunnerClone(discard ${discard})`, () => cache.retireRunnerClone(bare, P_PATH, BRANCH, RUN,
        discard ? { discard, completionSource: { expectedHead: pinSha, canDelete: () => true } } : { discard }));
    }
    // The ref protection of the released composite keeps G's refs and the pin.
    assert.equal(await cache.restoreKeepsRecoveryRef(bare, RUN, { sha: pinSha }), true);
    assert.deepEqual(world(), before);
  });

  it("C7: P's exit before S's, and any exit while the run is not terminal, is refused", async () => {
    await rc2Release("in_place");
    const before = world();
    assert.equal((await discardP()).kind, "unknown");
    assert.equal((await discardS({ lifecycle: { state: "not_terminal", status: "running" } })).kind, "refused");
    assert.equal((await discardS({ lifecycle: undefined })).kind, "refused");
    assert.deepEqual(world(), before);
    assert.equal((await discardS()).kind, "done");
    const notTerminal = await discardP({ lifecycle: { state: "not_terminal", status: "running" } });
    assert.equal(notTerminal.kind, "refused");
    assert.ok(rawKey(S_KEY)?.includes("retired"));
    assert.equal(fs.existsSync(path.join(P_PATH, "later.txt")), true);
  });
});
