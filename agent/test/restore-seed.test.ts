import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
import path from "node:path";
import {
  GitCache, RestoreCompositeError, RestoreSeedError,
  type BranchJournal, type IndependentSeedInput, type PositiveOwedCandidateContext,
} from "../src/git.js";
import type { GitAuthorityHooks } from "../src/git-authority-hooks.js";
import { restoreDigest, type RestoreWindow } from "../src/restore-journal.js";
import { attemptClonePath, formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #3644 Gate B (plan 3.4, 3.9): GitCache.seedIndependentAttempt against a real bare and real Git. Every step's failure is a
// typed RestoreSeedError naming the window it left; a fresh discoverOpaque / resolveBranchJournal reads each window back; the
// seed never touches the predecessor's path.

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
const P_ID = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
const S_ID = formatAttemptId(new Date("2026-11-05T11:11:11Z"), 2, "fedcba9876543210");
const P_KEY = `uzi-recovery.${BRANCH}.clone`;
const S_KEY = `uzi-restore.${BRANCH}.journal`;
const LEDGER_KEY = `uzi-attempts.${BRANCH}.entry`;

let fx: Fixture;
let cache: GitCache;
let bare: string;
let P_PATH: string;
let S_PATH: string;
let pRaw: string;
let pinSha: string;
let baseSha: string;
let events: string[];
let syncs: string[];
let failSync: ((n: number, target: string, kind: string) => boolean) | undefined;
let denyOp: ((op: string) => boolean) | undefined;

type Internals = {
  configEntries(bare: string): Promise<string[]>;
  runGit(cwd: string | undefined, args: string[]): Promise<string>;
  execScoped(command: string, args: string[], options: { cwd?: string }): Promise<{ stdout: string; stderr: string }>;
  gatherRetainedUnderLock(bare: string): Promise<{ records: Array<{ digest: string; problems: string[]; parts: Array<{ clonePath: string; presence: string }> }> }>;
  withLock<T>(bare: string, fn: () => Promise<T>): Promise<T>;
};
const internals = (c: GitCache = cache): Internals => c as unknown as Internals;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const config = (): Buffer => fs.readFileSync(path.join(bare, "config"));
const rawKey = (key: string): string | undefined => { try { return gitIn(bare, ["config", "--get", key]) || undefined; } catch { return undefined; } };
const setKey = (key: string, value: string): void => { gitIn(bare, ["config", key, value]); };
const addLedger = (attemptId: string, clonePath: string, state: string, runId = RUN): void => {
  gitIn(bare, ["config", "--add", LEDGER_KEY, JSON.stringify({ attemptId, runId, clonePath, state })]);
};
const ledgerValues = (): string[] => { try { return gitIn(bare, ["config", "--get-all", LEDGER_KEY]).split("\n").filter(Boolean); } catch { return []; } };
const sha256 = (text: string): string => createHash("sha256").update(text).digest("hex");

/** names, bytes, modes and mtimes of a tree: the predecessor's byte identity. */
function treeDigest(root: string): string {
  const h = createHash("sha256");
  const walk = (dir: string): void => {
    for (const name of fs.readdirSync(dir).sort()) {
      const p = path.join(dir, name);
      const st = fs.lstatSync(p);
      h.update(`${path.relative(root, p)}\0${st.mode}\0${st.mtimeMs}\0${st.size}\0`);
      if (st.isDirectory()) walk(p); else if (st.isFile()) h.update(fs.readFileSync(p));
    }
  };
  walk(root);
  return h.digest("hex");
}
const skillsOf = (p: string): string => path.join(path.dirname(p), `.uzi-skills-${path.basename(p)}`);

beforeEach(async () => {
  events = []; syncs = []; failSync = undefined; denyOp = undefined;
  fx = makeFixture();
  const hooks: GitAuthorityHooks = {
    admitSourceUse: async (clonePath, op) => { events.push(`admit:${op}:${clonePath}`); if (denyOp?.(op)) throw new Error(`denied ${op}`); },
    beginBareMutation: async () => ({ settle: async () => {} }),
    assertBareClear: async (_bare, op) => { events.push(`clear:${op}`); if (denyOp?.(`clear:${op}`)) throw new Error(`denied clear ${op}`); },
    beginDestructive: async () => ({ settle: async () => {} }),
  };
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
    authority: hooks,
    restoreDurability: {
      sync: async (target, kind) => {
        syncs.push(`${kind}:${path.basename(target)}`);
        if (failSync?.(syncs.length, target, kind)) throw new Error(`injected ${kind} sync failure`);
        const handle = await fsp.open(target, fs.constants.O_RDONLY);
        try { await handle.sync(); } finally { await handle.close(); }
      },
    },
  }));
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  baseSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  // The predecessor's finalization pin H: a commit of its own work, owed to the bare at generation 1 through the real path.
  const fixtureClone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-fixture", noProofReseed, RUN)).path;
  gitIn(fixtureClone, [...IDENT, "commit", "--allow-empty", "-m", "the predecessor's saved work"]);
  pinSha = gitIn(fixtureClone, ["rev-parse", "HEAD"]);
  const context: PositiveOwedCandidateContext = { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: baseSha } };
  const fetched = await cache.fetchAgentBranch(bare, fixtureClone, BRANCH, RUN, { context });
  assert.equal(fetched.kind, "updated");
  gitIn(bare, ["config", "--unset-all", `uzi-trackowner.${BRANCH}.owner`]);
  const canonical = cache.runnerClonePath(bare, KEY);
  P_PATH = attemptClonePath(canonical, P_ID);
  S_PATH = attemptClonePath(canonical, S_ID);
  pRaw = JSON.stringify({ runId: RUN, clonePath: P_PATH, attemptId: P_ID });
  setKey(P_KEY, pRaw);
  addLedger(P_ID, P_PATH, "live");
  // P's clone with later, unsaved bytes, and its skills sibling.
  fs.mkdirSync(path.join(P_PATH, ".git"), { recursive: true });
  fs.writeFileSync(path.join(P_PATH, "later.txt"), "later bytes that were never saved");
  fs.mkdirSync(skillsOf(P_PATH), { recursive: true });
  fs.writeFileSync(path.join(skillsOf(P_PATH), "skill.md"), "skill");
  events = [];
});
afterEach(() => fx.cleanup());

interface Trace { steps: string[]; seedTargets?: string[]; canonical?: string; protectedMarkers?: string[] }
function seedInput(trace: Trace, over: Partial<IndependentSeedInput> = {}): IndependentSeedInput {
  return {
    barePath: bare, branch: BRANCH, key: KEY, runId: RUN, attemptId: S_ID,
    predecessor: { clonePath: P_PATH, attemptId: P_ID, generation: 1, journalDigest: restoreDigest(pRaw), slotOwner: { worker: "worker-incarnation-1", epoch: "epoch-1" }, root: { dev: "2049", ino: "131072" } },
    boundary: { predecessor: "c".repeat(64), successor: "d".repeat(64) },
    pin: { captureId: "capture-1", generation: 1, sourceSha: pinSha, finalization: true },
    defaultBranch: "main",
    predecessorUnchanged: async () => { trace.steps.push("predecessorUnchanged"); return true; },
    census: async config => { trace.steps.push(`census:${config.complete}`); return { ok: true }; },
    beforeRestoreAttribution: async () => { trace.steps.push("marker"); },
    isLive: () => false,
    beforeSeed: async (targets, canonical, markers) => { trace.steps.push("beforeSeed"); trace.seedTargets = targets; trace.canonical = canonical; trace.protectedMarkers = markers; },
    onSeeded: () => { trace.steps.push("onSeeded"); },
    ...over,
  };
}

async function refusal(input: IndependentSeedInput): Promise<RestoreSeedError> {
  let caught: unknown;
  try { await cache.seedIndependentAttempt(input); } catch (err) { caught = err; }
  assert.ok(caught instanceof RestoreSeedError, `expected RestoreSeedError, got ${String(caught)}`);
  return caught;
}

/** A refusal before the first bare write: typed, window none, the bare's config and the predecessor's tree untouched, no successor. */
async function refusesCleanly(over: Partial<IndependentSeedInput>, reason: string, prepare?: () => void): Promise<RestoreSeedError> {
  prepare?.();
  const before = config();
  const tree = treeDigest(P_PATH);
  const trace: Trace = { steps: [] };
  const error = await refusal(seedInput(trace, over));
  assert.equal(error.reason, reason);
  assert.equal(error.window, "none", JSON.stringify(error.message));
  assert.deepEqual(config(), before, "the bare config is byte-identical");
  assert.equal(treeDigest(P_PATH), tree);
  assert.equal(fs.existsSync(S_PATH) && over.attemptId === undefined && reason !== "successor_path_exists", false, "no successor path");
  assert.equal(trace.steps.includes("marker"), false, "the marker is never written for a pre-check refusal");
  return error;
}

describe("seedIndependentAttempt: the happy path", () => {
  it("seeds S from the pin SHA in the bare, in plan order, and leaves P untouched", async () => {
    const trace: Trace = { steps: [] };
    const tree = treeDigest(P_PATH), skills = treeDigest(skillsOf(P_PATH));
    const clone = await cache.seedIndependentAttempt(seedInput(trace));
    assert.equal(clone.path, S_PATH);
    assert.equal(clone.attemptId, S_ID);
    assert.equal(clone.seededFrom, "restore_pin");
    assert.equal(clone.adoptedSource, pinSha);
    assert.equal(gitIn(S_PATH, ["rev-parse", "HEAD"]), pinSha);
    assert.equal(gitIn(S_PATH, ["rev-parse", "--abbrev-ref", "HEAD"]), BRANCH);
    assert.equal(fs.existsSync(path.join(S_PATH, "later.txt")), false, "P's later bytes are not in S");
    assert.equal(treeDigest(P_PATH), tree);
    assert.equal(treeDigest(skillsOf(P_PATH)), skills);
    assert.deepEqual(trace.steps, ["predecessorUnchanged", "census:true", "marker", "beforeSeed", "onSeeded"]);
    assert.deepEqual(trace.protectedMarkers, [`${RUN}:${P_ID}`]);
    assert.deepEqual(trace.seedTargets, [], "P exists on disk next to S, yet it is not a sweep target");
    assert.equal(trace.canonical, cache.runnerClonePath(bare, KEY));
    // P's journal key is byte-identical; S's own entry is ready; the ledger carries P live and S live.
    assert.equal(rawKey(P_KEY), pRaw);
    const r = await cache.resolveBranchJournal(bare, BRANCH) as Extract<BranchJournal, { kind: "composite" }>;
    assert.equal(r.kind, "composite");
    assert.equal(r.stage, "ready");
    assert.equal(r.ledger.state, "live");
    assert.equal(r.predecessor.raw, pRaw);
    assert.deepEqual(r.successor.kind === "entry" && { ...r.successor.journal }, { runId: RUN, clonePath: S_PATH, attemptId: S_ID });
    assert.deepEqual(r.basis.pin, { captureId: "capture-1", generation: 1, sourceSha: pinSha, finalization: true });
    assert.equal(r.basis.restoreTip, pinSha);
    assert.equal(r.basis.journalDigest, restoreDigest(pRaw));
    assert.deepEqual(r.basis.predecessor.root, { dev: "2049", ino: "131072" }, "the caller's root identity is persisted");
    assert.deepEqual(r.basis.boundary, { predecessor: "c".repeat(64), successor: "d".repeat(64) });
    assert.ok(ledgerValues().map(v => JSON.parse(v)).some(e => e.attemptId === S_ID && e.state === "live" && Object.keys(e).join() === "attemptId,runId,clonePath,state"),
      "the ledger entry has the four-field rc.2 shape");
    // The hook saw S's path under "seed-attempt" and never P's.
    assert.ok(events.includes(`admit:seed-attempt:${S_PATH}`));
    assert.ok(events.indexOf("clear:seed-attempt") >= 0 && events.indexOf("clear:seed-attempt") < events.indexOf(`admit:seed-attempt:${S_PATH}`));
    assert.ok(!events.some(e => e.includes(P_PATH)), JSON.stringify(events));
    // Durability: the config file then the bare directory, after the ledger append and each of the two entry writes.
    assert.deepEqual(syncs, ["file:config", "directory:" + path.basename(bare), "file:config", "directory:" + path.basename(bare), "file:config", "directory:" + path.basename(bare)]);
    const opaque = await cache.discoverOpaque(bare, BRANCH, RUN);
    assert.equal(opaque.kind, "composite");
  });

  it("a newer RETIRED ledger entry does not block, and a plain pin ref or the default branch bind an early pin", async () => {
    addLedger(formatAttemptId(new Date("2026-11-05T10:50:00Z"), 2, "1111111111111111"), `${cache.runnerClonePath(bare, KEY)}.attempt-${formatAttemptId(new Date("2026-11-05T10:50:00Z"), 2, "1111111111111111")}`, "retired");
    const early = { captureId: "capture-early", generation: 1, sourceSha: baseSha, finalization: false };
    const clone = await cache.seedIndependentAttempt(seedInput({ steps: [] }, { pin: early }));
    assert.equal(gitIn(clone.path, "rev-parse HEAD".split(" ")), baseSha);
    assert.equal(clone.seededFrom, "restore_pin");
  });

  it("an early pin binds through refs/uzi-recovery-pin/<run>/<G> as well", async () => {
    const other = gitIn(bare, [...IDENT, "commit-tree", `${baseSha}^{tree}`, "-p", baseSha, "-m", "unpublished start tip"]);
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${RUN}/1`, other]);
    const clone = await cache.seedIndependentAttempt(seedInput({ steps: [] },
      { defaultBranch: undefined, pin: { captureId: "capture-early", generation: 1, sourceSha: other, finalization: false } }));
    assert.equal(gitIn(clone.path, ["rev-parse", "HEAD"]), other);
  });

  it("never runs the forbidden P-side operations or compaction", async () => {
    const forbidden = ["compactAttemptLedger", "diagnoseRetainedArtifacts", "checkedRecovery", "recordRecoveryCapture", "prepareRecoverySuccessor",
      "verifiedRecoveryRestorePoint", "verifiedRetainedRecoveryRestorePoint", "nonLiveKeyPaths", "normalizeRetainedSourceObjects", "attributedRecovery"];
    const called: string[] = [];
    const target = cache as unknown as Record<string, unknown>;
    for (const name of forbidden) {
      assert.equal(typeof target[name], "function", name);
      target[name] = () => { called.push(name); throw new Error(`${name} must not run`); };
    }
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    assert.deepEqual(called, []);
  });
});

describe("seedIndependentAttempt: pre-checks refuse before the first bare write", () => {
  it("P's raw journal no longer hashes to the expected digest", async () => {
    await refusesCleanly({ predecessor: { ...seedInput({ steps: [] }).predecessor, journalDigest: restoreDigest("something else") } }, "predecessor_changed");
  });
  it("a malformed root identity or boundary digest refuses basis_malformed before any write", async () => {
    const p = seedInput({ steps: [] }).predecessor;
    for (const root of [{ dev: "0x1", ino: "5" }, { dev: "1", ino: "" }, { dev: "1", ino: "9".repeat(31) }, { dev: 1, ino: "5" }, undefined]) {
      await refusesCleanly({ predecessor: { ...p, root: root as never } }, "basis_malformed");
    }
    for (const boundary of [{ predecessor: "C".repeat(64), successor: "d".repeat(64) }, { predecessor: "c".repeat(64), successor: "d".repeat(63) },
      { predecessor: "c".repeat(64) }, undefined]) {
      await refusesCleanly({ boundary: boundary as never }, "basis_malformed");
    }
  });
  it("P's journal changed after the caller read it", async () => {
    await refusesCleanly({}, "predecessor_changed", () => setKey(P_KEY, JSON.stringify({ runId: RUN, clonePath: P_PATH, attemptId: P_ID, restoreTip: "a".repeat(40) })));
  });
  it("P's journal is anything but exactly {runId, clonePath, attemptId}", async () => {
    const fenced = JSON.stringify({ runId: RUN, clonePath: P_PATH, attemptId: P_ID,
      completionSourceFence: { version: 1, sourceUseEpoch: "0123abcd-0123-4abc-8def-0123456789ab", sourceCoreDigest: "a".repeat(64) } });
    await refusesCleanly({ predecessor: { ...seedInput({ steps: [] }).predecessor, journalDigest: restoreDigest(fenced) } }, "predecessor_not_plain", () => setKey(P_KEY, fenced));
  });
  it("P's journal names another run, path or attempt", async () => {
    for (const change of [{ runId: "run-9999" }, { clonePath: attemptClonePath(cache.runnerClonePath(bare, KEY), S_ID) }]) {
      const raw = JSON.stringify({ runId: RUN, clonePath: P_PATH, attemptId: P_ID, ...change });
      await refusesCleanly({ predecessor: { ...seedInput({ steps: [] }).predecessor, journalDigest: restoreDigest(raw) } }, "predecessor_not_plain", () => setKey(P_KEY, raw));
    }
  });
  it("a restore entry already exists", async () => {
    await refusesCleanly({}, "restore_entry_exists", () => setKey(S_KEY, "{}"));
  });
  it("a newer non-retired ledger entry exists (any state), but an older one does not matter", async () => {
    for (const state of ["live", "abandoned", "reclaimed"]) {
      const newer = formatAttemptId(new Date("2026-11-05T10:40:00Z"), 2, "2222222222222222");
      await refusesCleanly({}, "newer_attempt", () => addLedger(newer, attemptClonePath(cache.runnerClonePath(bare, KEY), newer), state));
      gitIn(bare, ["config", "--unset-all", LEDGER_KEY, newer]);
    }
    const older = formatAttemptId(new Date("2026-11-05T09:00:00Z"), 1, "3333333333333333");
    addLedger(older, attemptClonePath(cache.runnerClonePath(bare, KEY), older), "reclaimed");
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
  });
  it("P's own ledger entry is missing, retired, or names another path", async () => {
    gitIn(bare, ["config", "--unset-all", LEDGER_KEY]);
    await refusesCleanly({}, "predecessor_ledger");
    addLedger(P_ID, P_PATH, "retired");
    await refusesCleanly({}, "predecessor_ledger");
  });
  it("S's attempt id is already in the ledger, or older than P's, or carries no later generation", async () => {
    await refusesCleanly({}, "successor_id_taken", () => addLedger(S_ID, S_PATH, "retired"));
    gitIn(bare, ["config", "--unset-all", LEDGER_KEY, S_ID]);
    const older = formatAttemptId(new Date("2026-11-05T09:00:00Z"), 2, "4444444444444444");
    await refusesCleanly({ attemptId: older }, "attempt_order");
    const sameGen = formatAttemptId(new Date("2026-11-05T12:00:00Z"), 1, "5555555555555555");
    await refusesCleanly({ attemptId: sameGen }, "attempt_order");
    const noGen = formatAttemptId(new Date("2026-11-05T12:00:00Z"), undefined, "6666666666666666");
    await refusesCleanly({ attemptId: noGen }, "attempt_order");
  });
  it("S's own leaf already exists (and only that leaf is looked at)", async () => {
    fs.mkdirSync(S_PATH, { recursive: true });
    const error = await refusesCleanly({}, "successor_path_exists");
    assert.equal(error.window, "none");
  });
  it("P's slot classification changed", async () => {
    await refusesCleanly({ predecessorUnchanged: async () => false }, "predecessor_classification_changed");
    await refusesCleanly({ predecessorUnchanged: async () => { throw new Error("slot unreadable"); } }, "predecessor_check_failed");
  });
  it("a checkpoint tip that is not an ancestor of the pin", async () => {
    await refusesCleanly({ checkpointTip: "b".repeat(40) }, "checkpoint_not_ancestor");
    await refusesCleanly({ checkpointTip: pinSha.toUpperCase() }, "checkpoint_not_ancestor");
    const ok = await cache.seedIndependentAttempt(seedInput({ steps: [] }, { checkpointTip: baseSha }));
    assert.equal(ok.seededFrom, "restore_pin");
  });
  it("the pin is not bound in the bare: no owed ref, a retargeted pin, an early pin with no evidence", async () => {
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${pinSha}`]);
    await refusesCleanly({}, "pin_unbound");
    await refusesCleanly({ pin: { captureId: "c", generation: 1, sourceSha: baseSha, finalization: true } }, "pin_unbound");
    const unpublished = gitIn(bare, [...IDENT, "commit-tree", `${baseSha}^{tree}`, "-p", baseSha, "-m", "nobody pinned this"]);
    await refusesCleanly({ defaultBranch: undefined, pin: { captureId: "c", generation: 1, sourceSha: unpublished, finalization: false } }, "pin_unbound");
    await refusesCleanly({ defaultBranch: "main", pin: { captureId: "c", generation: 1, sourceSha: unpublished, finalization: false } }, "pin_unbound");
  });
  it("a pin whose object is absent from the bare", async () => {
    // The pin ref names a SHA the bare does not hold: it cannot bind, whatever the ref says.
    const absent = "c".repeat(40);
    await refusesCleanly({ defaultBranch: undefined, pin: { captureId: "c", generation: 1, sourceSha: absent, finalization: false } }, "pin_unbound");
  });
  it("a hook refusal of assertBareClear (a pending shared-bare deletion) refuses with nothing written", async () => {
    denyOp = op => op === "clear:seed-attempt";
    const error = await refusesCleanly({}, "authority_refused");
    assert.ok(error.cause instanceof Error);
  });
  it("the census refuses: its reason is the refusal, and the marker is never written", async () => {
    await refusesCleanly({ census: async () => ({ ok: false, reason: "census_foreign_active" }) }, "census_foreign_active");
    await refusesCleanly({ census: async () => ({ ok: false, reason: "attribution_unverifiable" }) }, "attribution_unverifiable");
    await refusesCleanly({ census: async () => { throw new Error("peek failed"); } }, "census_failed");
  });
  it("hands the census the bare's attribution config, complete", async () => {
    let seen: { keys: string[]; complete: boolean } | undefined;
    await cache.seedIndependentAttempt(seedInput({ steps: [] }, { census: async c => { seen = { keys: c.entries.map(e => e.key), complete: c.complete }; return { ok: true }; } }));
    assert.equal(seen?.complete, true);
    assert.ok(seen?.keys.includes(P_KEY) && seen.keys.includes(LEDGER_KEY));
    assert.ok(!seen?.keys.includes(S_KEY), "the census runs before the restore entry exists");
  });
});

describe("seedIndependentAttempt: P's path is never reached", () => {
  function trap(): { hits: string[]; restore(): void } {
    const hits: string[] = [];
    const skills = skillsOf(P_PATH);
    const under = (p: unknown): boolean => {
      if (typeof p !== "string" && !(p instanceof URL) && !Buffer.isBuffer(p)) return false;
      const resolved = path.resolve(p instanceof URL ? p.pathname : String(p));
      return [P_PATH, skills].some(f => resolved === f || resolved.startsWith(f + path.sep));
    };
    const restores: Array<() => void> = [];
    const patch = (target: Record<string, unknown>, names: string[], label: string): void => {
      for (const name of names) {
        const original = target[name];
        if (typeof original !== "function") continue;
        target[name] = function (this: unknown, ...args: unknown[]) {
          if (under(args[0]) || ((name === "rename" || name === "cp" || name === "copyFile") && under(args[1]))) {
            hits.push(`${label}.${name}(${String(args[0])})`);
            throw new Error(`predecessor touched: ${label}.${name}`);
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

  it("no lstat/stat/open/readdir under P and no Git with a cwd under P, across the whole seed", async () => {
    const cwds: string[] = [];
    const i = internals();
    const original = i.execScoped.bind(cache);
    i.execScoped = async (command, args, options) => { cwds.push(String(options?.cwd ?? ""), ...args); return original(command, args, options); };
    const t = trap();
    try { await cache.seedIndependentAttempt(seedInput({ steps: [] })); } finally { t.restore(); i.execScoped = original; }
    assert.deepEqual(t.hits, []);
    assert.deepEqual(cwds.filter(c => c === P_PATH || c.startsWith(P_PATH + path.sep) || c.includes(path.basename(P_PATH) + path.sep)), []);
  });

  it("a refused seed reaches P no more than a successful one", async () => {
    const tree = treeDigest(P_PATH);
    const t = trap();
    const reasons: string[] = [];
    try {
      reasons.push((await refusal(seedInput({ steps: [] }, { census: async () => ({ ok: false, reason: "census_pending" }) }))).reason);
      gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${pinSha}`]);
      reasons.push((await refusal(seedInput({ steps: [] }))).reason);
    } finally { t.restore(); }
    assert.deepEqual(reasons, ["census_pending", "pin_unbound"]);
    assert.deepEqual(t.hits, []);
    assert.equal(treeDigest(P_PATH), tree);
  });
});

describe("seedIndependentAttempt: failure windows (plan 3.9), each read back by a fresh resolver", () => {
  async function read(): Promise<{ journal: BranchJournal; opaque: string; ledger: Array<{ attemptId: string; state: string }> }> {
    const journal = await new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions()).resolveBranchJournal(bare, BRANCH);
    const opaque = (await new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions()).discoverOpaque(bare, BRANCH, RUN)).kind;
    return { journal, opaque, ledger: ledgerValues().map(v => JSON.parse(v)) };
  }
  const windowOf = (e: RestoreSeedError): RestoreWindow => e.window;

  it("marker failure: window marker_only, the bare untouched, plain and sameRun on a fresh read", async () => {
    const before = config();
    const error = await refusal(seedInput({ steps: [] }, { beforeRestoreAttribution: async () => { throw new Error("marker write failed"); } }));
    assert.equal(error.reason, "marker_failed");
    assert.equal(windowOf(error), "marker_only");
    assert.deepEqual(config(), before);
    const r = await read();
    assert.equal(r.journal.kind, "plain");
    assert.equal(r.opaque, "sameRun");
  });

  it("ledger append failure: window ledger_live (the write may have landed), the bare still plain", async () => {
    const i = internals();
    const original = i.runGit.bind(cache);
    i.runGit = async (cwd, args) => {
      if (args.includes("--add") && args.includes(LEDGER_KEY)) throw new Error("injected ledger append failure");
      return original(cwd, args);
    };
    const before = config();
    const error = await refusal(seedInput({ steps: [] })).finally(() => { i.runGit = original; });
    assert.equal(error.reason, "seed_failed");
    assert.equal(error.window, "ledger_live");
    assert.equal(error.step, "ledger");
    assert.deepEqual(config(), before);
    assert.equal((await read()).journal.kind, "plain");
  });

  it("ledger durability failure: window ledger_live with S's live entry and no restore entry; the resolver reads plain", async () => {
    failSync = n => n === 1;
    const error = await refusal(seedInput({ steps: [] }));
    assert.equal(error.window, "ledger_live");
    const r = await read();
    assert.equal(r.journal.kind, "plain");
    assert.equal(r.opaque, "sameRun");
    assert.ok(r.ledger.some(e => e.attemptId === S_ID && e.state === "live"));
    assert.equal(rawKey(S_KEY), undefined);
    assert.equal(fs.existsSync(S_PATH), false);
  });

  for (const [label, n, kind] of [["the config fsync", 3, "file"], ["the directory fsync", 4, "directory"]] as const) {
    it(`entry durability failure (${label}): window seeding, the entry and ledger exist, a composite on a fresh read, no clone`, async () => {
      failSync = (count, _target, k) => count === n && k === kind;
      const error = await refusal(seedInput({ steps: [] }));
      assert.equal(error.window, "seeding");
      assert.equal(error.step, "entry");
      const r = await read();
      assert.equal(r.journal.kind, "composite");
      assert.equal(r.journal.kind === "composite" && r.journal.stage, "seeding");
      assert.equal(r.opaque, "composite");
      assert.equal(fs.existsSync(S_PATH), false);
      assert.equal(rawKey(P_KEY), pRaw);
    });
  }

  it("entry read-back failure (a stale read after the write): window seeding", async () => {
    const i = internals();
    const original = i.configEntries.bind(cache);
    let stale = false;
    failSync = (_n, _t, kind) => { if (syncs.length === 4 && kind === "directory") stale = true; return false; };
    i.configEntries = async b => { const e = await original(b); return stale ? e.filter(x => !x.startsWith("uzi-restore.")) : e; };
    const error = await refusal(seedInput({ steps: [] })).finally(() => { i.configEntries = original; });
    assert.equal(error.window, "seeding");
    assert.match(String(error.cause), /read-back differs/);
    assert.equal((await read()).journal.kind, "composite");
  });

  it("the authority refuses S's path: window seeding, reason authority_refused, no clone", async () => {
    denyOp = op => op === "seed-attempt";
    const error = await refusal(seedInput({ steps: [] }));
    assert.equal(error.reason, "authority_refused");
    assert.equal(error.window, "seeding");
    assert.equal(error.step, "admit");
    assert.equal(fs.existsSync(S_PATH), false);
  });

  it("beforeSeed refuses (a protected process blocks the seed): window seeding, nothing killed, no clone", async () => {
    const error = await refusal(seedInput({ steps: [] }, { beforeSeed: async () => { throw new Error("a protected attempt marker is still running"); } }));
    assert.equal(error.reason, "before_seed_refused");
    assert.equal(error.window, "seeding");
    assert.equal(fs.existsSync(S_PATH), false);
    assert.equal((await read()).journal.kind, "composite");
  });

  it("the clone fails: window seeding with the entry still seeding and a ledger entry left live (not abandoned)", async () => {
    const i = internals();
    const original = i.runGit.bind(cache);
    // seedRunnerClone's local clone runs as the runner through execScoped; fail it at the Git level.
    const exec = i.execScoped.bind(cache);
    i.execScoped = async (command, args, options) => {
      if (args.includes("clone") && args.includes("--no-checkout")) throw Object.assign(new Error("injected clone failure"), { code: 128, stdout: "", stderr: "x" });
      return exec(command, args, options);
    };
    const error = await refusal(seedInput({ steps: [] })).finally(() => { i.execScoped = exec; });
    void original;
    assert.equal(error.window, "seeding");
    assert.equal(error.step, "seed");
    const r = await read();
    assert.equal(r.journal.kind, "composite");
    assert.equal(r.journal.kind === "composite" && r.journal.ledger.state, "live");
  });

  it("the ready write is not durable: window seeding, the clone exists, the composite resolves (ready or seeding)", async () => {
    failSync = n => n === 5;
    const error = await refusal(seedInput({ steps: [] }));
    assert.equal(error.window, "seeding");
    assert.equal(error.step, "ready");
    assert.equal(fs.existsSync(path.join(S_PATH, ".git")), true);
    const r = await read();
    assert.equal(r.journal.kind, "composite");
    assert.equal(r.opaque, "composite");
  });

  it("a Git deadline expiry anywhere refuses with git_deadline, names a window, and leaves a resolvable bare", async () => {
    const error = await refusal(seedInput({ steps: [] }, {
      deadlineMs: 150,
      census: async () => { await new Promise(r => setTimeout(r, 400)); return { ok: true }; },
    }));
    assert.equal(error.reason, "git_deadline");
    assert.ok(["none", "marker_only", "ledger_live", "seeding"].includes(error.window));
    const r = await read();
    assert.ok(r.journal.kind === "plain" || r.journal.kind === "composite");
    assert.equal(rawKey(P_KEY), pRaw);
  });
});

describe("the exits after each window", () => {
  const live = { isLivePath: () => false, quiesce: async () => true, hasRetainedHomeMarker: async () => true };
  async function digestOf(clonePath: string): Promise<string> {
    const g = await internals().withLock(bare, () => internals().gatherRetainedUnderLock(bare));
    return g.records.find(r => r.parts.some(p => p.clonePath === clonePath))!.digest;
  }
  async function discard(clonePath: string, attempt: string, generation: number, over: Record<string, unknown> = {}) {
    return cache.discardStoppedSource({
      selector: { runId: RUN, generation, attempt }, confirm: (over.confirm as string | undefined) ?? await digestOf(clonePath), holds: { state: "read", holds: [] },
      lifecycle: { state: "terminal", status: "failed" }, barePath: bare, ...live, ...over,
    } as never);
  }
  it("window ledger_live (S's live entry, no restore entry): the plain discard of P retires S first, clears P's journal and marker", async () => {
    failSync = n => n === 1;
    await refusal(seedInput({ steps: [] }));
    const removed: string[] = [];
    const result = await discard(P_PATH, P_ID, 1, { removeRetainedHomeMarker: async (run: string) => { removed.push(run); } });
    assert.equal(result.kind, "done", JSON.stringify(result));
    assert.deepEqual(removed, [RUN]);
    assert.equal(fs.existsSync(P_PATH), false);
    assert.equal(fs.existsSync(skillsOf(P_PATH)), false);
    assert.ok(!rawKey(P_KEY));
    const states = new Map(ledgerValues().map(v => JSON.parse(v)).map((e: { attemptId: string; state: string }) => [e.attemptId, e.state]));
    assert.equal(states.get(S_ID), "retired");
    assert.equal(states.get(P_ID), "retired");
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "none");
  });

  it("window seeding (a partial S, no .git): S's discard skips the quiesce, retires S and clears S's key; then P's discard succeeds", async () => {
    const error = await refusal(seedInput({ steps: [] }, { beforeSeed: async () => { fs.mkdirSync(S_PATH, { recursive: true }); throw new Error("seed interrupted"); } }));
    assert.equal(error.window, "seeding");
    assert.equal(fs.existsSync(S_PATH), true);
    let quiesced = 0;
    const s = await discard(S_PATH, S_ID, 2, { quiesce: async () => { quiesced++; return true; }, removeRetainedHomeMarker: async () => { throw new Error("S must not remove the marker"); } });
    assert.equal(s.kind, "done", JSON.stringify(s));
    assert.equal(quiesced, 0, "a partial S has nothing to quiesce");
    assert.equal(fs.existsSync(S_PATH), false);
    assert.equal(rawKey(S_KEY) ?? "", "");
    assert.equal(rawKey(P_KEY), pRaw, "P's journal is untouched by S's exit");
    assert.equal(fs.existsSync(path.join(P_PATH, "later.txt")), true, "P's bytes are untouched by S's exit");
    // P now has no entry and S's retired ledger entry.
    const removed: string[] = [];
    const p = await discard(P_PATH, P_ID, 1, { removeRetainedHomeMarker: async (run: string) => { removed.push(run); } });
    assert.equal(p.kind, "done", JSON.stringify(p));
    assert.deepEqual(removed, [RUN]);
    assert.equal(fs.existsSync(P_PATH), false);
    assert.ok(!rawKey(P_KEY));
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "none");
  });

  it("ready, S failed with a real clone: S's discard quiesces S only, P's discard is refused while S is live, then both exits complete", async () => {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    fs.writeFileSync(path.join(S_PATH, "work.txt"), "S's own work");
    const before = config();
    // P while a live S entry exists: refused (RestoreCompositeError, reported unknown), nothing written, P intact.
    const refused = await discard(P_PATH, P_ID, 1);
    assert.equal(refused.kind, "unknown");
    assert.deepEqual(config(), before);
    assert.equal(fs.existsSync(path.join(P_PATH, "later.txt")), true);
    // S's discard: not terminal refuses, and an open hold of S's generation refuses.
    assert.equal((await discard(S_PATH, S_ID, 2, { lifecycle: { state: "not_terminal", status: "running" } })).kind, "refused");
    const held = await discard(S_PATH, S_ID, 2, { holds: { state: "read", holds: [{ id: "hold-2", generation: 2 }] } });
    assert.equal(held.kind, "refused");
    assert.deepEqual(config(), before);
    // P's own open hold (generation 1) does not block S's exit.
    const quiesced: string[][] = [];
    const s = await discard(S_PATH, S_ID, 2, { holds: { state: "read", holds: [{ id: "hold-1", generation: 1 }] },
      quiesce: async (paths: string[]) => { quiesced.push(paths); return true; } });
    assert.equal(s.kind, "done", JSON.stringify(s));
    assert.deepEqual(quiesced, [[S_PATH, skillsOf(S_PATH)]], "only S is quiesced");
    assert.equal(fs.existsSync(S_PATH), false);
    assert.equal(rawKey(S_KEY) ?? "", "");
    assert.equal(rawKey(P_KEY), pRaw);
    // P: its open hold refuses until it is closed; then it exits and a later seed has a clean slate.
    const stillHeld = await discard(P_PATH, P_ID, 1, { holds: { state: "read", holds: [{ id: "hold-1", generation: 1 }] } });
    assert.equal(stillHeld.kind, "refused");
    const p = await discard(P_PATH, P_ID, 1, { removeRetainedHomeMarker: async () => {} });
    assert.equal(p.kind, "done", JSON.stringify(p));
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "none");
  });

  it("ledger retired and no tombstone (an interrupted retirement): P's discard clears S's key and P's journal in one write", async () => {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    fs.rmSync(S_PATH, { recursive: true, force: true });
    addLedger(S_ID, S_PATH, "retired");
    const r = await cache.resolveBranchJournal(bare, BRANCH);
    assert.equal(r.kind === "composite" && r.stage, "ready");
    const p = await discard(P_PATH, P_ID, 1);
    assert.equal(p.kind, "done", JSON.stringify(p));
    assert.equal(rawKey(S_KEY) ?? "", "");
    assert.ok(!rawKey(P_KEY));
  });

  it("a tombstone: P's discard clears the tombstone and P's journal together and calls the marker hook once", async () => {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    fs.rmSync(S_PATH, { recursive: true, force: true });
    addLedger(S_ID, S_PATH, "retired");
    await cache.advanceRestoreEntry(bare, BRANCH, "retired");
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind === "composite" && (await cache.resolveBranchJournal(bare, BRANCH) as { stage: string }).stage, "retired");
    const removed: string[] = [];
    const p = await discard(P_PATH, P_ID, 1, { removeRetainedHomeMarker: async (run: string) => { removed.push(run); } });
    assert.equal(p.kind, "done", JSON.stringify(p));
    assert.deepEqual(removed, [RUN]);
    assert.equal(rawKey(S_KEY) ?? "", "");
    assert.ok(!rawKey(P_KEY));
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "none");
  });

  it("a failing marker hook runs after the metadata is retired and durable, and the retry removes the marker", async () => {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    fs.rmSync(S_PATH, { recursive: true, force: true });
    addLedger(S_ID, S_PATH, "retired");
    await cache.advanceRestoreEntry(bare, BRANCH, "retired");
    const confirm = await digestOf(P_PATH);
    const failed = await discard(P_PATH, P_ID, 1, { confirm, removeRetainedHomeMarker: async () => { throw new Error("marker busy"); } });
    assert.equal(failed.kind, "unknown");
    assert.equal(failed.kind === "unknown" && failed.metadata, "retired");
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "none", "the tombstone and P's journal went in the one rename");
    const removed: string[] = [];
    const again = await discard(P_PATH, P_ID, 1, { confirm, removeRetainedHomeMarker: async (run: string) => { removed.push(run); } });
    assert.equal(again.kind, "done", JSON.stringify(again));
    assert.deepEqual(removed, [RUN]);
  });

  it("another run's discard on the restored branch is blocked, and the restore-aware exit needs a terminal lifecycle", async () => {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    const unknown = await discard(S_PATH, S_ID, 2, { lifecycle: undefined });
    assert.equal(unknown.kind, "refused");
    assert.equal(unknown.kind === "refused" && unknown.reason, "run_state_not_terminal");
  });
});

describe("review rework (#3644 m2b): resumable P exit, disposition guards, pin recheck", () => {
  const live = { isLivePath: () => false, quiesce: async () => true, hasRetainedHomeMarker: async () => true };
  async function digestOf(clonePath: string): Promise<string> {
    const g = await internals().withLock(bare, () => internals().gatherRetainedUnderLock(bare));
    return g.records.find(r => r.parts.some(p => p.clonePath === clonePath))!.digest;
  }
  async function discardP(over: Record<string, unknown> = {}) {
    return cache.discardStoppedSource({
      selector: { runId: RUN, generation: 1, attempt: P_ID }, confirm: (over.confirm as string | undefined) ?? await digestOf(P_PATH), holds: { state: "read", holds: [] },
      lifecycle: { state: "terminal", status: "failed" }, barePath: bare, ...live, removeRetainedHomeMarker: async () => {}, ...over,
    } as never);
  }
  const ledgerStates = (): Map<string, string> =>
    new Map(ledgerValues().map(v => JSON.parse(v)).map((e: { attemptId: string; state: string }) => [e.attemptId, e.state]));
  /** After the retry no ledger entry of P or S is live and neither journal key holds a value. */
  function assertSettled(): void {
    assert.ok(!rawKey(P_KEY));
    assert.equal(rawKey(S_KEY) ?? "", "");
    assert.equal(ledgerStates().get(P_ID), "retired");
    assert.notEqual(ledgerStates().get(S_ID), "live");
  }

  /** A marker that behaves like the real one: present until removed, so the retry's evidence is what a worker would see. */
  function statefulMarker(removeFails?: () => boolean) {
    const m = { present: true, removals: 0 };
    return {
      m,
      over: {
        hasRetainedHomeMarker: async () => m.present,
        removeRetainedHomeMarker: async () => { if (removeFails?.()) throw new Error("marker busy"); m.present = false; m.removals++; },
      },
    };
  }

  it("window 2: a failure after the rename and before its sync keeps the marker; the retry syncs, removes it and completes", async () => {
    failSync = n => n === 1;
    await refusal(seedInput({ steps: [] }));
    const { m, over } = statefulMarker();
    failSync = () => true; // every durability sync after the rewrite throws
    const confirm = await digestOf(P_PATH);
    const first = await discardP({ confirm, ...over });
    assert.equal(first.kind, "unknown");
    assert.equal(first.kind === "unknown" && first.metadata, "kept", "the retirement is not reported until it is durable");
    assert.equal(m.present, true, "the marker outlives a rewrite that is not proven durable");
    assert.ok(!rawKey(P_KEY), "P's key was cleared by the same rename that retired the ledger");
    assert.equal(ledgerStates().get(P_ID), "retired");
    assert.equal(ledgerStates().get(S_ID), "retired");
    failSync = undefined;
    const again = await discardP({ confirm, ...over });
    assert.equal(again.kind, "done", JSON.stringify(again));
    assert.equal(again.kind === "done" && again.metadata, "retired");
    assert.equal(m.present, false);
    assertSettled();
  });

  it("window 2: a failure after the sync and before the marker removal reports retired; the retry removes the marker", async () => {
    failSync = n => n === 1;
    await refusal(seedInput({ steps: [] }));
    let fail = true;
    const { m, over } = statefulMarker(() => fail);
    const confirm = await digestOf(P_PATH);
    const first = await discardP({ confirm, ...over });
    assert.equal(first.kind, "unknown");
    assert.equal(first.kind === "unknown" && first.metadata, "retired");
    assert.equal(m.present, true);
    assert.equal(ledgerStates().get(P_ID), "retired");
    fail = false;
    const again = await discardP({ confirm, ...over });
    assert.equal(again.kind, "done", JSON.stringify(again));
    assert.equal(m.present, false);
    assertSettled();
    const third = await discardP({ confirm, ...over });
    assert.equal(third.kind === "refused" && third.reason, "not_found", "a finished exit is not found, and removes nothing more");
    assert.equal(m.removals, 1);
  });

  it("window 2: a failure inside the rewrite changes nothing, keeps the marker and the same call completes", async () => {
    failSync = n => n === 1;
    await refusal(seedInput({ steps: [] }));
    const before = config();
    const { m, over } = statefulMarker();
    const i = cache as unknown as { rewriteConfigAtomically(...a: unknown[]): Promise<void> };
    const original = i.rewriteConfigAtomically;
    i.rewriteConfigAtomically = async () => { throw new Error("injected rewrite failure"); };
    const first = await discardP(over);
    i.rewriteConfigAtomically = original;
    assert.notEqual(first.kind, "done");
    assert.equal(first.kind === "unknown" && first.metadata, "kept");
    assert.deepEqual(config(), before);
    assert.equal(m.present, true, "the marker is not removed before the rewrite");
    assert.equal(fs.existsSync(P_PATH), false, "P's bytes are already gone: the retry still completes on marker and ledger evidence");
    const again = await discardP(over);
    assert.equal(again.kind, "done", JSON.stringify(again));
    assert.equal(m.present, false);
    assertSettled();
  });

  it("tombstone: a failure after the rename and before its sync keeps the marker and the retry completes", async () => {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    fs.rmSync(S_PATH, { recursive: true, force: true });
    addLedger(S_ID, S_PATH, "retired");
    await cache.advanceRestoreEntry(bare, BRANCH, "retired");
    const { m, over } = statefulMarker();
    failSync = () => true;
    const confirm = await digestOf(P_PATH);
    const first = await discardP({ confirm, ...over });
    assert.equal(first.kind, "unknown");
    assert.equal(first.kind === "unknown" && first.metadata, "kept");
    assert.equal(m.present, true);
    failSync = undefined;
    const again = await discardP({ confirm, ...over });
    assert.equal(again.kind, "done", JSON.stringify(again));
    assert.equal(m.present, false);
    assertSettled();
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "none");
  });

  it("a shared owed pin or a foreign journal naming the source still refuses the restore exit", async () => {
    failSync = n => n === 1;
    await refusal(seedInput({ steps: [] }));
    const before = config();
    gitIn(bare, ["update-ref", `refs/uzi-owed/other-run/${pinSha}`, pinSha]);
    const shared = await discardP();
    assert.equal(shared.kind, "refused", JSON.stringify(shared));
    assert.ok(shared.kind === "refused" && shared.consumers.includes("shared_owed_pin"), JSON.stringify(shared));
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/other-run/${pinSha}`]);
    setKey("uzi-recovery.agent/other.clone", JSON.stringify({ runId: "other-run", clonePath: P_PATH, attemptId: P_ID }));
    const named = await discardP();
    assert.notEqual(named.kind, "done", JSON.stringify(named));
    assert.ok(named.kind !== "refused" || named.consumers.some(c => c.startsWith("named_by_journal:") && c.includes("agent/other")), JSON.stringify(named));
    assert.equal(fs.existsSync(path.join(P_PATH, "later.txt")), true, "P's bytes survive both refusals");
    gitIn(bare, ["config", "--unset-all", "uzi-recovery.agent/other.clone"]);
    assert.equal(rawKey(P_KEY), pRaw);
    assert.ok(before.length > 0);
  });

  it("the marker evidence is asked for with this bare and P's attempt id, and a marker bound elsewhere keeps P an ordinary source", async () => {
    addLedger(S_ID, S_PATH, "retired");
    const asked: unknown[] = [];
    const r = await discardP({ hasRetainedHomeMarker: async (_run: string, binding?: { barePath: string; attemptId: string }) => { asked.push(binding); return false; } });
    assert.deepEqual(asked, [{ barePath: bare, attemptId: P_ID }]);
    assert.equal(r.kind, "refused", JSON.stringify(r));
    assert.equal(fs.existsSync(path.join(P_PATH, "later.txt")), true, "P's unsaved bytes are untouched");
    assert.equal(rawKey(P_KEY), pRaw);
  });

  it("no restore evidence (no retained-home marker, or no later generation): an ordinary newer same-run entry never makes P restore-exitable", async () => {
    // An ordinary newer retired entry of the same run, path absent: not a restore.
    addLedger(S_ID, S_PATH, "retired");
    const noMarker = await discardP({ hasRetainedHomeMarker: undefined });
    assert.equal(noMarker.kind, "refused", JSON.stringify(noMarker));
    const markerFalse = await discardP({ hasRetainedHomeMarker: async () => false });
    assert.equal(markerFalse.kind, "refused", JSON.stringify(markerFalse));
    assert.equal(fs.existsSync(path.join(P_PATH, "later.txt")), true, "P's unsaved bytes are untouched");
    assert.equal(rawKey(P_KEY), pRaw);
    // A marker without a later generation is not evidence either (same generation, later timestamp).
    const sameGen = formatAttemptId(new Date("2026-11-05T12:12:12Z"), 1, "aaaaaaaaaaaaaaaa");
    const sameGenPath = attemptClonePath(cache.runnerClonePath(bare, KEY), sameGen);
    gitIn(bare, ["config", "--unset-all", LEDGER_KEY]);
    addLedger(P_ID, P_PATH, "live");
    addLedger(sameGen, sameGenPath, "live");
    const equal = await discardP();
    assert.equal(equal.kind, "refused", JSON.stringify(equal));
    assert.equal(fs.existsSync(path.join(P_PATH, "later.txt")), true);
  });

  it("a pin deleted from inside beforeSeed refuses at the pin recheck and the pin stays protected", async () => {
    const error = await refusal(seedInput({ steps: [] }, {
      beforeSeed: async () => { gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${pinSha}`]); },
    }));
    assert.equal(error.reason, "pin_unbound");
    assert.equal(fs.existsSync(S_PATH), false, "no clone is seeded");
    assert.equal(await cache.restoreKeepsRecoveryRef(bare, RUN, { sha: pinSha }), true);
  });

  it("a pin deleted from inside the census callback refuses at window seeding and the pin stays protected", async () => {
    const error = await refusal(seedInput({ steps: [] }, {
      census: async () => { gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${pinSha}`]); return { ok: true }; },
    }));
    assert.equal(error.reason, "pin_unbound");
    assert.equal(error.window, "seeding");
    assert.equal(fs.existsSync(S_PATH), false, "no clone is seeded");
    const r = await cache.resolveBranchJournal(bare, BRANCH);
    assert.equal(r.kind, "composite");
    assert.equal(r.kind === "composite" && r.stage, "seeding", "never ready");
    assert.equal(await cache.restoreKeepsRecoveryRef(bare, RUN, { sha: pinSha }), true);
  });
});

describe("the ordinary discard dispositions never reach P or S (#3431 negative)", () => {
  it("inspect reports no local_discard for the restored predecessor or the live successor, in every composite state", async () => {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    const kinds = async (): Promise<Map<string, string | undefined>> => {
      const out = new Map<string, string | undefined>();
      for (const [attempt, generation] of [[P_ID, 1], [S_ID, 2]] as const) {
        const inspected = await cache.inspectRetainedSources({
          selector: { runId: RUN, generation, attempt }, holds: { state: "read", holds: [] },
          lifecycle: { state: "terminal", status: "failed" }, isLivePath: () => false, barePath: bare,
        });
        for (const s of inspected.sources) out.set(attempt, s.disposition?.kind);
      }
      return out;
    };
    for (const [attempt, kind] of await kinds()) {
      assert.notEqual(kind, "local_discard", `${attempt} must not be locally discardable through the ordinary rules`);
      assert.equal(kind, "refused", attempt);
    }
    // P's presence is reported without a probe; the problem names the restore.
    const g = await internals().withLock(bare, () => internals().gatherRetainedUnderLock(bare));
    const p = g.records.find(r => r.parts.some(part => part.clonePath === P_PATH))!;
    assert.ok(p.problems.includes("restore_predecessor_retained"));
    assert.equal(p.parts[0]!.presence, "unsafe");
  });

  it("a discard of S or P through the exit never deletes the other, and an interrupted P exit (P's bytes already gone) is resumed by the same call", async () => {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    fs.rmSync(S_PATH, { recursive: true, force: true });
    addLedger(S_ID, S_PATH, "retired");
    await cache.advanceRestoreEntry(bare, BRANCH, "retired");
    // An earlier, interrupted run already removed P's directory; the first attempt here dies at the marker hook, which now
    // runs after the metadata write, so the retry finds the record gone and finishes the marker.
    fs.rmSync(P_PATH, { recursive: true, force: true });
    const g = await internals().withLock(bare, () => internals().gatherRetainedUnderLock(bare));
    const confirm = g.records.find(r => r.parts.some(part => part.clonePath === P_PATH))!.digest;
    const base = { selector: { runId: RUN, generation: 1, attempt: P_ID }, confirm, holds: { state: "read", holds: [] },
      lifecycle: { state: "terminal", status: "failed" }, isLivePath: () => false, quiesce: async () => true, barePath: bare } as const;
    const first = await cache.discardStoppedSource({ ...base, removeRetainedHomeMarker: async () => { throw new Error("interrupted"); } } as never);
    assert.notEqual(first.kind, "done");
    assert.equal(first.kind === "unknown" && first.metadata, "retired");
    const second = await cache.discardStoppedSource({ ...base, hasRetainedHomeMarker: async () => true, removeRetainedHomeMarker: async () => {} } as never);
    assert.equal(second.kind, "done", JSON.stringify(second));
    assert.equal((await cache.resolveBranchJournal(bare, BRANCH)).kind, "none");
  });
});

describe("the orphan marker finisher is bound to this bare's retired ledger entry (#3644 review)", () => {
  const MADE_UP = formatAttemptId(new Date("2026-11-05T13:13:13Z"), 1, "bbbbbbbbbbbbbbbb");
  const spy = () => {
    const s = { present: true, removals: 0, order: [] as string[] };
    return { s, hooks: {
      hasRetainedHomeMarker: async () => s.present,
      removeRetainedHomeMarker: async () => { s.order.push("remove"); s.present = false; s.removals++; },
    } };
  };
  const retry = (barePath: string, over: Record<string, unknown> = {}, attempt = P_ID, generation = 1) => cache.discardStoppedSource({
    selector: { runId: RUN, generation, attempt }, confirm: "0".repeat(64), holds: { state: "read", holds: [] },
    lifecycle: { state: "terminal", status: "failed" }, isLivePath: () => false, quiesce: async () => true, barePath, ...over,
  } as never);
  /** The completed P exit whose marker removal never ran: P's record is gone, its ledger value is retired, the marker is still there. */
  async function completedExit(): Promise<ReturnType<typeof spy>> {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    fs.rmSync(S_PATH, { recursive: true, force: true });
    addLedger(S_ID, S_PATH, "retired");
    await cache.advanceRestoreEntry(bare, BRANCH, "retired");
    const g = await internals().withLock(bare, () => internals().gatherRetainedUnderLock(bare));
    const confirm = g.records.find(r => r.parts.some(part => part.clonePath === P_PATH))!.digest;
    const m = spy();
    const first = await cache.discardStoppedSource({
      selector: { runId: RUN, generation: 1, attempt: P_ID }, confirm, holds: { state: "read", holds: [] },
      lifecycle: { state: "terminal", status: "failed" }, isLivePath: () => false, quiesce: async () => true, barePath: bare,
      hasRetainedHomeMarker: m.hooks.hasRetainedHomeMarker, removeRetainedHomeMarker: async () => { throw new Error("interrupted"); },
    } as never);
    assert.equal(first.kind === "unknown" && first.metadata, "retired");
    return m;
  }
  const untouched = (r: { kind: string }, m: { s: { present: boolean; removals: number } }, what: string): void => {
    assert.notEqual(r.kind, "done", `${what}: ${JSON.stringify(r)}`);
    assert.equal(m.s.removals, 0, `${what}: the marker hook must not run`);
    assert.equal(m.s.present, true, `${what}: the marker stays`);
  };
  const notFound = (r: { kind: string; reason?: string }, what: string): void => assert.equal(r.kind === "refused" && r.reason, "not_found", `${what}: ${JSON.stringify(r)}`);

  it("control: the completed exit's retry (retired ledger entry for exactly the selector's attempt) syncs, then removes the marker", async () => {
    const m = await completedExit();
    failSync = () => { m.s.order.push("sync"); return false; };
    const r = await retry(bare, m.hooks);
    failSync = undefined;
    assert.equal(r.kind, "done", JSON.stringify(r));
    assert.equal(m.s.removals, 1);
    assert.ok(m.s.order.includes("sync"), "a durability sync ran");
    assert.equal(m.s.order.at(-1), "remove");
    assert.equal(m.s.order.lastIndexOf("sync") < m.s.order.indexOf("remove"), true, "every sync precedes the removal");
  });

  it("the auditor's two-bare probe: a live restore in one bare and an unrelated bare as the caller's never remove the marker", async () => {
    await cache.seedIndependentAttempt(seedInput({ steps: [] }));
    const fx2 = makeFixture();
    try {
      const bare2 = await cache.ensureClone(fx2.originPath);
      const m = spy();
      const probe = await retry(bare2, { ...m.hooks, holds: { state: "unknown" } }, MADE_UP);
      notFound(probe, "unknown holds, made-up attempt, other bare");
      untouched(probe, m, "probe");
      const read = await retry(bare2, m.hooks, MADE_UP);
      notFound(read, "read holds, made-up attempt, other bare");
      const real = await retry(bare2, m.hooks, P_ID);
      notFound(real, "P's own attempt id against an unrelated bare");
      untouched(real, m, "P's attempt id, other bare");
      assert.equal(rawKey(P_KEY), pRaw, "P stays a live record in its own bare");
    } finally { fx2.cleanup(); }
  });

  it("holds unknown or open, a made-up attempt id and a non-terminal lifecycle each leave the marker", async () => {
    const m = await completedExit();
    const unknown = await retry(bare, { ...m.hooks, holds: { state: "unknown" } });
    untouched(unknown, m, "holds unknown");
    const open = await retry(bare, { ...m.hooks, holds: { state: "read", holds: [{ id: "h1", generation: 1 }] } });
    untouched(open, m, "an open hold");
    const madeUp = await retry(bare, m.hooks, MADE_UP);
    notFound(madeUp, "no retired entry for the selector's attempt id");
    untouched(madeUp, m, "made-up attempt id");
    const wrongGeneration = await retry(bare, m.hooks, P_ID, 2);
    notFound(wrongGeneration, "the selector's generation is not the attempt id's");
    untouched(wrongGeneration, m, "wrong generation");
    const running = await retry(bare, { ...m.hooks, lifecycle: { state: "running" } });
    untouched(running, m, "non-terminal lifecycle");
    const none = await retry(bare, { ...m.hooks, lifecycle: undefined });
    untouched(none, m, "no lifecycle");
    const canonical = await retry(bare, m.hooks, "canonical" as never, "none" as never);
    untouched(canonical, m, "canonical selector");
  });

  it("P still a record, a live ledger value, a restore entry, a descriptor or a stopped marker of the run each leave the marker", async () => {
    const m = await completedExit();
    setKey(P_KEY, pRaw);
    untouched(await retry(bare, m.hooks), m, "P's journal is back (still a record)");
    gitIn(bare, ["config", "--unset-all", P_KEY]);
    const later = formatAttemptId(new Date("2026-11-05T14:14:14Z"), 3, "cccccccccccccccc");
    addLedger(later, attemptClonePath(cache.runnerClonePath(bare, KEY), later), "live");
    notFound(await retry(bare, m.hooks), "a live ledger value of the run");
    untouched(await retry(bare, m.hooks), m, "live ledger value");
    gitIn(bare, ["config", "--unset-all", LEDGER_KEY]);
    addLedger(P_ID, P_PATH, "retired");
    notFound(await retry(bare, m.hooks, MADE_UP), "restored ledger without the made-up id");
    // Back to the completed state (retired P entry, nothing else): the control proves the finisher would run, then each extra key blocks it.
    for (const [key, value] of [
      [S_KEY, JSON.stringify({ runId: RUN, clonePath: S_PATH, attemptId: S_ID })],
      [`uzi-retained.${RUN}.journal`, JSON.stringify({ runId: RUN })],
      [`uzi-stopped.${RUN}.entry`, JSON.stringify({ runId: RUN })],
    ] as const) {
      setKey(key, value);
      let r: { kind: string } | undefined;
      try { r = await retry(bare, m.hooks); } catch { r = { kind: "threw" }; }
      untouched(r, m, `config key ${key}`);
      gitIn(bare, ["config", "--unset-all", key]);
    }
    const ok = await retry(bare, m.hooks);
    assert.equal(ok.kind, "done", `the cleaned state finishes the marker: ${JSON.stringify(ok)}`);
  });
});

describe("sanity of the happy-path fixture", () => {
  it("P's journal digest is sha256 of the raw value", () => {
    assert.equal(restoreDigest(pRaw), sha256(pRaw));
    assert.ok(RestoreCompositeError);
  });
});
