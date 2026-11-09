import { after, afterEach, before, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import fsp from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import {
  GitCache, RestoreCompositeError, type IndependentSeedInput, type PositiveOwedCandidateContext, type RecoveryJournalEntry,
} from "../src/git.js";
import { DindPruneGate } from "../src/dind-prune.js";
import { reclaimStrandedRunHomes } from "../src/home-reclaim.js";
import { RetainedSourceServer } from "../src/ops/retained-source-server.js";
import { writeRetainedHomeMarker } from "../src/retained-homes.js";
import { holdingNamePrefix, isHoldingName, parseRestoreValue, restoreDigest, restoreJournalKey } from "../src/restore-journal.js";
import { attemptClonePath, formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { runnerOverDisk, type RunnerOver } from "./independent-restore-rc2-runner.js";

// issue #3644 plan 4.3 / 3.11: the RELEASED reader, v0.87.0-rc.2, executed against a restore fixture written by this build.
//
// Standalone, like recovery-bindingless-compat.proof.ts (which is likewise run by the command in its ADR, with no package script
// or workflow job): run it from agent/ with
//   env GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null node --import tsx --test --test-timeout=120000 test/independent-restore-rc2-compat.proof.ts
// It needs the tag v0.87.0-rc.2 and the base commit 76d41e67 in the local clone
// (a shallow agent CI checkout cannot supply them); a missing tag or commit FAILS setup, it never skips. The archived source is
// executed untouched: the proof asserts its git.ts is byte-identical to the base commit's, imports the real modules, and
// drives them on the same on-disk fixture. Nothing is simulated.
//
// What rc.2 may do to a restored branch (plan 3.11, owner item 9): write a reservation and a `preservation_failed` blocker into
// P's journal, and delete P's legacy HOME and provision directories once the run is terminal. It must keep S's ledger entry,
// restore entry and clone, and P's clone bytes, owed refs and pins. Anything else is a finding for owner review.
//
// Tests titled "OWNER REVIEW PENDING (plan 3.11)" assert what rc.2 was OBSERVED to do; they are not an acceptance of it. The other-run
// claim chain they exercise is rc.2's runner.ts phaseClone: runnerCloneForClaim -> ForeignCaptureBlockedError(P) ->
// RunRunner.reclaimTerminalOrphan -> GitCache.releaseAttemptInPlace(P, "reclaimed") (attempt-path flights) or
// GitCache.retireRunnerClone(P, discard:false) (others) -> reseedAfterReclaim. reclaimTerminalOrphan is the archived method on an
// instance of the archived class; the classification endpoint and the process-quiescence proof are the only stubs.
//
// Amendment 14 (the owner's "recognise the rc.2-released predecessor"): the last describe below runs the actual rc.2 sequence (S
// terminal on this build, roll back to rc.2, another claim of the branch, markRecoveryCapture of the other run), then rolls forward
// with the current build on the same disk, with no hand edits. The first part of that describe drives GitCache.discardStoppedSource
// directly with stub marker hooks. The last describe (m4c) is the production end to end, one fixture per mode: a later claim of the run
// on the production RunRunner (test/independent-restore-rc2-runner.ts) with an owner Cancel queued, then S's and P's guarded discards through
// the production RetainedSourceServer over its socket with a real retained-home marker, the HOME sweeps of this build, and the controls
// (partial attribution, digest mismatch, a wrong-inode holding directory, an uncancelled claim as the unsafe-adoption control). The
// CI-gated twin, from a byte fixture of the same end state, is restore-released.test.ts.

const root = path.resolve(fileURLToPath(new URL("../..", import.meta.url)));
const scratch = path.join(root, ".uzi", "scratch");
const TAG = "v0.87.0-rc.2";
const TAG_COMMIT = "42b34de469c3296476f0c00cf51672b31544bb15";
const BASE = "76d41e67a68ea1b663be82648b9c5b5ece1cdf8b";

function scrubGitEnv(base: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(base)) if (!key.startsWith("GIT_")) env[key] = value;
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
}
const ENV = scrubGitEnv(process.env);
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const git = (dir: string, ...args: string[]): string =>
  execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
const repoGit = (...args: string[]): Buffer => execFileSync("git", args, { cwd: root, env: ENV, maxBuffer: 64 * 1024 * 1024, timeout: 60_000 });

const BRANCH = "agent/issue-3644";
const KEY = "issue-3644";
const RUN = "36443644-1111-4222-8333-444444444444"; // a uuid: the reader's HOME reclaim only looks at uuid-named directories
const P_ID = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
const S_ID = formatAttemptId(new Date("2026-11-05T11:11:11Z"), 2, "fedcba9876543210");
const P_KEY = `uzi-recovery.${BRANCH}.clone`;
const S_KEY = restoreJournalKey(BRANCH);
const LEDGER_KEY = `uzi-attempts.${BRANCH}.entry`;

// The rc.2 modules, loaded once from the archive.
type Rc2Git = typeof import("../src/git.js");
type Rc2Home = typeof import("../src/home-reclaim.js");
let suite: string;
let rc2Git: Rc2Git;
let rc2Home: Rc2Home;
let rc2Runner: Record<string, unknown>;

before(async () => {
  await fsp.mkdir(scratch, { recursive: true });
  suite = await fsp.mkdtemp(path.join(scratch, "restore-rc2-"));
  // Both revisions are mandatory: a missing tag or commit fails setup.
  assert.equal(repoGit("rev-parse", `${TAG}^{commit}`).toString("utf8").trim(), TAG_COMMIT, `${TAG} must resolve to the released commit`);
  assert.equal(repoGit("rev-parse", `${BASE}^{commit}`).toString("utf8").trim(), BASE, "the base commit must be present");
  const destination = path.join(suite, "reader-rc2");
  await fsp.mkdir(destination);
  const archive = repoGit("archive", TAG_COMMIT, "agent/src", "agent/package.json");
  execFileSync("tar", ["-x", "-C", destination], { input: archive, timeout: 60_000 });
  await fsp.symlink(path.join(root, "agent", "node_modules"), path.join(destination, "agent", "node_modules"), "dir");
  const src = path.join(destination, "agent", "src");
  // The archived file IS the released one, and it is the base commit's file.
  assert.deepEqual(await fsp.readFile(path.join(src, "git.ts")), repoGit("show", `${TAG_COMMIT}:agent/src/git.ts`), "executed source is the tagged source");
  assert.deepEqual(await fsp.readFile(path.join(src, "git.ts")), repoGit("show", `${BASE}:agent/src/git.ts`), "rc.2 git.ts is byte-identical to the base commit's");
  rc2Git = await import(pathToFileURL(path.join(src, "git.ts")).href) as Rc2Git;
  rc2Home = await import(pathToFileURL(path.join(src, "home-reclaim.ts")).href) as Rc2Home;
  rc2Runner = await import(pathToFileURL(path.join(src, "runner.ts")).href) as Record<string, unknown>;
});
// Only the directory this run created (the mkdtemp above) is removed, with the archive and every environment built under it.
after(async () => { if (suite) await fsp.rm(suite, { recursive: true, force: true }); });

interface Env {
  fx: Fixture; dataDir: string; bare: string; current: GitCache; rc2: InstanceType<Rc2Git["GitCache"]>;
  pPath: string; sPath: string; pRaw: string; pinSha: string; childSha: string;
  homeRoot: string; provisionRoot: string;
}

/** A restore written by THIS build: P (a real clone with later, unsaved bytes) plain-journaled, S seeded from the pin. */
async function build(): Promise<Env> {
  const fx = makeFixture();
  const authority = { admitSourceUse: async () => {}, beginBareMutation: async () => ({ settle: async () => {} }), assertBareClear: async () => {}, beginDestructive: async () => ({ settle: async () => {} }) };
  const current = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority }));
  const bare = await current.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const baseSha = git(bare, "rev-parse", "refs/remotes/origin/main");
  const fixtureClone = (await current.runnerCloneForBranch(bare, BRANCH, "issue-fixture", noProofReseed, RUN)).path;
  git(fixtureClone, ...IDENT, "commit", "--allow-empty", "-m", "the predecessor's saved work");
  const pinSha = git(fixtureClone, "rev-parse", "HEAD");
  const context: PositiveOwedCandidateContext = { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: baseSha } };
  assert.equal((await current.fetchAgentBranch(bare, fixtureClone, BRANCH, RUN, { context })).kind, "updated");
  git(fixtureClone, ...IDENT, "commit", "--allow-empty", "-m", "a descendant of the pin");
  const childSha = git(fixtureClone, "rev-parse", "HEAD");
  git(bare, "fetch", "-q", fixtureClone, "HEAD:refs/uzi-incoming/child");
  git(bare, "update-ref", "-d", "refs/uzi-incoming/child");
  git(bare, "config", "--unset-all", `uzi-trackowner.${BRANCH}.owner`);
  const canonical = current.runnerClonePath(bare, KEY);
  const pPath = attemptClonePath(canonical, P_ID);
  const sPath = attemptClonePath(canonical, S_ID);
  // P: a real clone on the branch at the pin, plus unsaved bytes.
  fs.mkdirSync(path.dirname(pPath), { recursive: true });
  execFileSync("git", ["clone", "-q", "--shared", "--no-checkout", "--", bare, pPath], { env: ENV });
  git(pPath, "checkout", "-q", "-b", BRANCH, pinSha);
  fs.writeFileSync(path.join(pPath, "later.txt"), "later bytes that were never saved");
  const pRaw = JSON.stringify({ runId: RUN, clonePath: pPath, attemptId: P_ID });
  git(bare, "config", P_KEY, pRaw);
  git(bare, "config", "--add", LEDGER_KEY, JSON.stringify({ attemptId: P_ID, runId: RUN, clonePath: pPath, state: "live" }));
  // P's legacy HOME and provision directory, and S's per-attempt ones.
  const homeRoot = path.join(fx.dataDir, "agent-home");
  const provisionRoot = path.join(fx.dataDir, "provision");
  for (const dir of [homeRoot, provisionRoot]) {
    fs.mkdirSync(path.join(dir, RUN), { recursive: true });
    fs.writeFileSync(path.join(dir, RUN, "p-state.txt"), "predecessor state");
    fs.mkdirSync(path.join(dir, `${RUN}.g2`), { recursive: true });
    fs.writeFileSync(path.join(dir, `${RUN}.g2`, "s-state.txt"), "successor state");
  }
  const seed: IndependentSeedInput = {
    barePath: bare, branch: BRANCH, key: KEY, runId: RUN, attemptId: S_ID,
    predecessor: { clonePath: pPath, attemptId: P_ID, generation: 1, journalDigest: restoreDigest(pRaw), slotOwner: { worker: "w1", epoch: "e1" },
      root: { dev: String(fs.lstatSync(pPath, { bigint: true }).dev), ino: String(fs.lstatSync(pPath, { bigint: true }).ino) } },
    boundary: { predecessor: "c".repeat(64), successor: "d".repeat(64) },
    pin: { captureId: "capture-1", generation: 1, sourceSha: pinSha, finalization: true }, defaultBranch: "main",
    predecessorUnchanged: async () => true, census: async () => ({ ok: true }), beforeRestoreAttribution: async () => {},
    isLive: () => false, beforeSeed: async () => {},
  };
  await current.seedIndependentAttempt(seed);
  fs.writeFileSync(path.join(sPath, "work.txt"), "the successor's own work");
  const rc2 = new rc2Git.GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions() as never);
  return { fx, dataDir: fx.dataDir, bare, current, rc2, pPath, sPath, pRaw, pinSha, childSha, homeRoot, provisionRoot };
}

/** Names, modes, sizes, mtimes and content digests of a tree (symlinks by target): a byte-and-metadata inventory. */
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
const configEntries = (bare: string): Map<string, string[]> => {
  const out = new Map<string, string[]>();
  for (const item of git(bare, "config", "--local", "--null", "--list").split("\0").filter(Boolean)) {
    const nl = item.indexOf("\n");
    const key = nl < 0 ? item : item.slice(0, nl);
    out.set(key, [...(out.get(key) ?? []), nl < 0 ? "" : item.slice(nl + 1)]);
  }
  return out;
};
const refList = (bare: string): string => git(bare, "for-each-ref", "--format=%(refname) %(objectname)");

interface Snap {
  p: Record<string, string>; s: Record<string, string>; owedMeta: Record<string, string>; refs: string; config: Map<string, string[]>;
  sHome: Record<string, string>; pHome: Record<string, string>; sProvision: Record<string, string>; pProvision: Record<string, string>;
}
function snap(e: Env): Snap {
  return {
    p: tree(e.pPath), s: tree(e.sPath), owedMeta: tree(path.join(e.bare, "uzi-owed")), refs: refList(e.bare), config: configEntries(e.bare),
    pHome: tree(path.join(e.homeRoot, RUN)), sHome: tree(path.join(e.homeRoot, `${RUN}.g2`)),
    pProvision: tree(path.join(e.provisionRoot, RUN)), sProvision: tree(path.join(e.provisionRoot, `${RUN}.g2`)),
  };
}
/** Everything but the config keys named in `allowed`, compared exactly. */
function assertSame(before: Snap, after: Snap, allowedConfigKeys: string[], what: string): void {
  for (const k of ["p", "s", "owedMeta", "refs", "sHome", "sProvision"] as const) assert.deepEqual(after[k], before[k], `${what}: ${k} unchanged`);
  const keys = new Set([...before.config.keys(), ...after.config.keys()]);
  for (const key of keys) {
    if (allowedConfigKeys.includes(key)) continue;
    assert.deepEqual(after.config.get(key), before.config.get(key), `${what}: config ${key} unchanged`);
  }
}

// The other-run claim path of rc.2's REAL RunRunner. The class is the archived one (not a copy): an instance is made from its
// prototype with only the collaborators `reclaimTerminalOrphan` reads, and its own method runs. The caller chain it models is
// runner.ts phaseClone: runnerCloneForClaim -> ForeignCaptureBlockedError(P) -> `reclaimTerminalOrphan` -> `releaseAttemptInPlace(P,
// "reclaimed")` (git.ts) -> `reseedAfterReclaim` -> runnerCloneForClaim -> GitCache.runnerCloneForBranch. The collaborators stubbed
// here are the classification endpoint (a terminal owner of this repo) and the process quiescence proof (no survivors).
async function reclaimAsOtherRun(e: Env, attemptPaths: boolean): Promise<{ reclaimError?: unknown; diagnostics: string[] }> {
  const attempt = (id: string) => ({ attemptId: id, isLive: () => false, beforeSeed: async () => {} });
  const reseed = { beforeFree: async () => {}, isLive: () => false };
  const otherId = formatAttemptId(new Date("2026-11-05T12:00:00Z"), 3, "aaaaaaaaaaaaaaaa");
  let blocked: unknown;
  try { await e.rc2.runnerCloneForBranch(e.bare, BRANCH, KEY, reseed, OTHER, false, undefined, attempt(otherId) as never); }
  catch (err) { blocked = err; }
  assert.ok(blocked instanceof rc2Git.ForeignCaptureBlockedError, "the other run's claim is blocked by P's journal");
  const Runner = rc2Runner.RunRunner as unknown as { prototype: object };
  const diagnostics: string[] = [];
  const runner = Object.assign(Object.create(Runner.prototype) as Record<string, unknown>, {
    git: e.rc2, attemptPaths,
    log: { info: (m: string) => diagnostics.push(m), warn: (m: string) => diagnostics.push(m), error: (m: string) => diagnostics.push(m), debug: () => {} },
    shutdownSignal: new AbortController(),
    client: { getRunOrphanClassification: async () => ({ status: "completed", repo_id: "repo-1", kind: "issue", issue_iid: 3644, branch: BRANCH, pipeline_id: null, pipeline_ref: null }) },
    quiesceRun: async () => ({ outcome: { kind: "verified" }, blocked: false }),
  });
  const controller = new AbortController();
  const flight = { cancel: controller, steering: { lifecycleSignal: () => controller.signal, terminalLifecycleSignal: () => controller.signal, claimFence: () => undefined, isCancelled: () => false },
    effectiveAttemptPaths: attemptPaths, executor: undefined };
  const claim = { run_id: OTHER, repo: { id: "repo-1", default_branch: "main" } };
  const error = blocked as InstanceType<Rc2Git["ForeignCaptureBlockedError"]>;
  let reclaimError: unknown;
  try {
    await (runner as unknown as { reclaimTerminalOrphan(...a: unknown[]): Promise<void> })
      .reclaimTerminalOrphan(e.bare, claim, flight, error.clonePath, error.branch, error.ownerRunId, error);
  } catch (err) { reclaimError = err; }
  return { reclaimError, diagnostics };
}

const OTHER = "another-run";

/** The rc.2 claim by the other run, whole: its real reclaimTerminalOrphan, its reseed (attempt path in mode A, canonical in mode H) and
 *  its markRecoveryCapture, which writes O's journal into P's own key. */
async function rc2ClaimsBranch(e: Env, attemptPaths: boolean): Promise<{ oPath: string }> {
  const { reclaimError, diagnostics } = await reclaimAsOtherRun(e, attemptPaths);
  assert.equal(reclaimError, undefined, `reclaim failed: ${String(reclaimError)} ${diagnostics.join("|")}`);
  const otherId = formatAttemptId(new Date("2026-11-05T12:00:00Z"), 3, "bbbbbbbbbbbbbbbb");
  const reseed = { beforeFree: async () => {}, isLive: () => false };
  const clone = await e.rc2.runnerCloneForBranch(e.bare, BRANCH, KEY, reseed, OTHER, false, undefined,
    (attemptPaths ? { attemptId: otherId, isLive: () => false, beforeSeed: async () => {} } : undefined) as never);
  await e.rc2.markRecoveryCapture(e.bare, clone.path, BRANCH, OTHER, attemptPaths ? otherId : undefined);
  assert.equal(JSON.parse(configEntries(e.bare).get(P_KEY)![0]!).runId, OTHER, "rc.2 wrote O's journal into P's key");
  return { oPath: clone.path };
}

describe("rc.2 against a restore written by this build", () => {
  it("the released source is the tagged source, ignores the restore prefix, and loads (git, home reclaim, runner)", () => {
    const src = path.join(suite, "reader-rc2", "agent", "src");
    for (const file of fs.readdirSync(src).filter(n => n.endsWith(".ts"))) {
      assert.ok(!fs.readFileSync(path.join(src, file), "utf8").includes("uzi-restore"), `${file} has no notion of the restore prefix`);
    }
    assert.equal(typeof rc2Git.GitCache, "function");
    assert.equal(typeof rc2Home.reclaimStrandedRunHomes, "function");
    assert.equal(typeof rc2Runner.RunRunner, "function");
  });

  it("discovery: rc.2 reads P's plain journal as its retained source and writes nothing; S's key is invisible to it", async () => {
    const e = await build();
    try {
      const before = snap(e);
      const found = await e.rc2.discoverRetainedRecovery(e.fx.originPath, BRANCH, KEY, RUN);
      assert.ok(found, "rc.2 discovers P's source");
      assert.deepEqual({ ...found.journal }, { runId: RUN, clonePath: e.pPath, attemptId: P_ID });
      assert.equal(configEntries(e.bare).get(P_KEY)?.[0], e.pRaw);
      assertSame(before, snap(e), [], "discovery");
      assert.ok(snap(e).config.has(S_KEY), "S's restore entry is still there");
    } finally { e.fx.cleanup(); }
  });

  it("claim: rc.2 refuses to seed over P (same run) and refuses another run, writing nothing", async () => {
    const e = await build();
    try {
      const before = snap(e);
      const attempt = (id: string) => ({ attemptId: id, isLive: () => false, beforeSeed: async () => {} });
      const reseed = { beforeFree: async () => {}, isLive: () => false };
      const sameRun = formatAttemptId(new Date("2026-11-05T12:00:00Z"), 3, "aaaaaaaaaaaaaaaa");
      await assert.rejects(() => e.rc2.runnerCloneForBranch(e.bare, BRANCH, KEY, reseed, RUN, true, undefined, attempt(sameRun) as never),
        (err: unknown) => (err as Error).name === "PendingRecoveryCaptureError" || err instanceof rc2Git.PendingRecoveryCaptureError);
      await assert.rejects(() => e.rc2.runnerCloneForBranch(e.bare, BRANCH, KEY, reseed, "another-run", false, undefined, attempt(sameRun) as never),
        (err: unknown) => err instanceof rc2Git.ForeignCaptureBlockedError);
      assertSame(before, snap(e), [], "claim");
    } finally { e.fx.cleanup(); }
  });

  it("OWNER REVIEW PENDING (plan 3.11): item 9, first allowance (a reservation and a preservation_failed blocker in P's journal) plus an observed THIRD write, uzi-attemptmode.<key>.enabled", async () => {
    const e = await build();
    try {
      const before = snap(e);
      const expected = { runId: RUN, clonePath: e.pPath, attemptId: P_ID };
      const progress = await e.rc2.reserveRecoveryIteration(e.bare, BRANCH, KEY, expected, 1000, 2);
      assert.equal(progress.attempts, 1);
      await e.rc2.blockRecoveryEpisode(e.bare, BRANCH, KEY, expected, "preservation_failed");
      const after = snap(e);
      // P's journal: the original keys byte for byte, plus exactly the episode with the blocker.
      const journal = JSON.parse(after.config.get(P_KEY)![0]!) as RecoveryJournalEntry & { recovery: { stage: string; blocker: string } };
      const { recovery, ...rest } = journal;
      assert.equal(JSON.stringify(rest), e.pRaw, "P's original journal fields are unchanged");
      assert.equal(recovery.stage, "blocked");
      assert.equal(recovery.blocker, "preservation_failed");
      // The third write is not one of item 9's two allowances: rc.2 also records its attempt-mode flag for the key
      // (`uzi-attemptmode.<key>.enabled`). Observed and listed here explicitly; S's trace is untouched.
      assert.ok(after.config.has(`uzi-attemptmode.${KEY}.enabled`), "the extra write exists");
      assert.equal(before.config.has(`uzi-attemptmode.${KEY}.enabled`), false, "and was not there before");
      const changed = [...new Set([...before.config.keys(), ...after.config.keys()])].filter(k => JSON.stringify(before.config.get(k)) !== JSON.stringify(after.config.get(k))).sort();
      assert.deepEqual(changed, [P_KEY, `uzi-attemptmode.${KEY}.enabled`].sort());
      assertSame(before, after, [P_KEY, `uzi-attemptmode.${KEY}.enabled`], "reservation and blocker");
      assert.equal(after.config.get(S_KEY)?.[0], before.config.get(S_KEY)?.[0], "S's restore entry is byte-identical");
      assert.deepEqual(after.config.get(LEDGER_KEY), before.config.get(LEDGER_KEY), "the ledger is byte-identical");
    } finally { e.fx.cleanup(); }
  });

  it("item 9, second allowance: P's legacy HOME and provision directory go once the run is terminal; S's per-attempt ones and every clone stay", async () => {
    const e = await build();
    try {
      const before = snap(e);
      const sweep = (root: string, status: string) => rc2Home.reclaimStrandedRunHomes(root, async () => status, nullLogger(), { minAgeMs: 0, now: () => Date.now() + 10_000 });
      for (const root of [e.homeRoot, e.provisionRoot]) {
        const running = await sweep(root, "running");
        assert.equal(running.removed, 0);
        assert.equal(running.skippedNotTerminal, 1, "the legacy directory is the only candidate; the .g2 name is not a run directory to rc.2");
      }
      assertSame(before, snap(e), [], "non-terminal sweep");
      assert.deepEqual(snap(e).pHome, before.pHome);
      for (const root of [e.homeRoot, e.provisionRoot]) assert.equal((await sweep(root, "failed")).removed, 1);
      const after = snap(e);
      assert.deepEqual(after.pHome, {}, "P's legacy HOME is deleted once terminal");
      assert.deepEqual(after.pProvision, {}, "P's provision directory is deleted once terminal");
      assertSame(before, after, [], "terminal sweep");
      assert.deepEqual(after.sHome, before.sHome, "S's per-attempt HOME is orphaned, not deleted");
      assert.deepEqual(after.sProvision, before.sProvision);
    } finally { e.fx.cleanup(); }
  });

  it("rc.2's seed-time retention sweeps keep S: ledger compaction drops nothing whose path exists, and S's path is never a retention target", async () => {
    const e = await build();
    try {
      const before = snap(e);
      await (e.rc2 as unknown as { compactAttemptLedger(bare: string, branch: string): Promise<void> }).compactAttemptLedger(e.bare, BRANCH);
      assertSame(before, snap(e), [], "compaction");
    } finally { e.fx.cleanup(); }
  });

  it("OWNER REVIEW PENDING (plan 3.11): rc.2's real reclaimTerminalOrphan (the other-run claim path) clears P's journal and appends ledger reclaimed; P's bytes and S are untouched", async () => {
    const e = await build();
    try {
      const before = snap(e);
      const { reclaimError, diagnostics } = await reclaimAsOtherRun(e, true);
      assert.equal(reclaimError, undefined, `reclaim failed: ${String(reclaimError)} ${diagnostics.join("|")}`);
      const after = snap(e);
      // Observed, not accepted: this is the one rc.2 path that rewrites P's journal beyond item 9's allowances.
      assert.equal(after.config.get(P_KEY)?.[0] ?? "", "", "P's journal is cleared");
      const states = (after.config.get(LEDGER_KEY) ?? []).map(v => JSON.parse(v) as { attemptId: string; state: string });
      assert.equal(states.filter(x => x.attemptId === P_ID).at(-1)?.state, "reclaimed", "P's ledger entry is now reclaimed");
      assert.deepEqual(states.filter(x => x.attemptId === S_ID), (before.config.get(LEDGER_KEY) ?? []).map(v => JSON.parse(v) as { attemptId: string; state: string }).filter(x => x.attemptId === S_ID));
      assert.deepEqual(after.p, before.p, "P's directory bytes and metadata are unchanged by the reclaim");
      assert.deepEqual(after.s, before.s);
      assert.equal(after.config.get(S_KEY)?.[0], before.config.get(S_KEY)?.[0]);
      assert.equal(after.refs, before.refs);
      // And this build's reader then refuses fail-closed on the state rc.2 left.
      await assert.rejects(() => e.current.resolveBranchJournal(e.bare, BRANCH), /predecessor journal is empty/);
    } finally { e.fx.cleanup(); }
  });

  it("OWNER REVIEW PENDING (plan 3.11): after that reclaim, rc.2's reseed for the other run seeds a NEW attempt path and does not read, move or delete P's directory", async () => {
    const e = await build();
    try {
      const { reclaimError } = await reclaimAsOtherRun(e, true);
      assert.equal(reclaimError, undefined);
      const before = snap(e);
      const otherId = formatAttemptId(new Date("2026-11-05T12:00:00Z"), 3, "bbbbbbbbbbbbbbbb");
      const reseed = { beforeFree: async () => {}, isLive: () => false };
      const clone = await e.rc2.runnerCloneForBranch(e.bare, BRANCH, KEY, reseed, "another-run", false, undefined,
        { attemptId: otherId, isLive: () => false, beforeSeed: async () => {} } as never);
      assert.equal(clone.path, attemptClonePath(e.current.runnerClonePath(e.bare, KEY), otherId), "a new attempt path, never P's");
      const after = snap(e);
      assert.deepEqual(after.p, before.p, "P's unsaved bytes are preserved exactly");
      assert.equal(fs.readFileSync(path.join(e.pPath, "later.txt"), "utf8"), "later bytes that were never saved");
      assert.deepEqual(after.s, before.s);
      const states = (after.config.get(LEDGER_KEY) ?? []).map(v => JSON.parse(v) as { attemptId: string; state: string });
      assert.equal(states.filter(x => x.attemptId === P_ID).at(-1)?.state, "reclaimed", "P stays reclaimed (retained forever)");
    } finally { e.fx.cleanup(); }
  });

  it("OWNER REVIEW PENDING (plan 3.11): a flight NOT on attempt paths takes retireRunnerClone(discard:false): P's directory is MOVED, bytes intact, to a worker-only holding root, and P's journal is cleared", async () => {
    const e = await build();
    try {
      const before = snap(e);
      const { reclaimError, diagnostics } = await reclaimAsOtherRun(e, false);
      assert.equal(reclaimError, undefined, `reclaim failed: ${String(reclaimError)} ${diagnostics.join("|")}`);
      assert.equal(fs.existsSync(e.pPath), false, "P's directory is no longer at its path");
      const holders: string[] = [];
      const walk = (dir: string): void => {
        for (const name of fs.readdirSync(dir)) {
          const p = path.join(dir, name);
          const st = fs.lstatSync(p);
          if (st.isDirectory()) walk(p); else if (name === "later.txt" && p !== path.join(e.sPath, name)) holders.push(p);
        }
      };
      walk(e.dataDir);
      assert.equal(holders.length, 1, `exactly one retained copy of P's unsaved file exists: ${JSON.stringify(holders)}`);
      assert.equal(fs.readFileSync(holders[0]!, "utf8"), "later bytes that were never saved");
      assert.ok(!holders[0]!.startsWith(path.dirname(e.pPath) + path.sep), "the copy is outside the runner repo directory");
      assert.equal(snap(e).config.get(P_KEY)?.[0] ?? "", "", "P's journal is cleared");
      assert.deepEqual(snap(e).s, before.s, "S is untouched");
    } finally { e.fx.cleanup(); }
  });

  it("UNSAFE CONTROL: S's attribution written unfenced into P's own key is cleared by rc.2", async () => {
    const e = await build();
    try {
      // The layout this design rejects: S's journal in P's key.
      git(e.bare, "config", S_KEY, "");
      git(e.bare, "config", P_KEY, JSON.stringify({ runId: RUN, clonePath: e.sPath, attemptId: S_ID }));
      await e.rc2.releaseAttemptInPlace(e.bare, e.sPath, BRANCH, RUN, "reclaimed");
      assert.equal(configEntries(e.bare).get(P_KEY)?.[0] ?? "", "", "rc.2 cleared the key that carried S's attribution");
      // Control for the control: the layout this build writes survives the same call on S's path, because rc.2 never reads S's key.
      const safe = await build();
      try {
        await assert.rejects(() => safe.rc2.releaseAttemptInPlace(safe.bare, safe.sPath, BRANCH, RUN, "reclaimed"));
        assert.equal(configEntries(safe.bare).get(P_KEY)?.[0], safe.pRaw);
        assert.ok(configEntries(safe.bare).get(S_KEY)?.[0]);
      } finally { safe.fx.cleanup(); }
    } finally { e.fx.cleanup(); }
  });
});

// ---- amendment 14: roll forward from the rc.2-released predecessor -----------------------------------------------------------------

describe("amendment 14: roll forward from the state rc.2 leaves after another claim of the branch", () => {
  const holdingRoot = (e: Env): string => path.join(e.dataDir, "runner-quarantine");
  const quiet = { isLivePath: () => false, quiesce: async () => true };

  /** One of the owner's own discards on the rolled-forward build: inspect for the confirm digest, then discard. */
  async function discardOn(e: Env, runId: string, attempt: string, generation: number, over: Record<string, unknown> = {}) {
    const selector = { runId, generation, attempt };
    const holds = { state: "read" as const, holds: [] };
    const lifecycle = { state: "terminal" as const, status: "failed" as const };
    const inspected = await e.current.inspectRetainedSources({ selector, holds, lifecycle, isLivePath: () => false, barePath: e.bare });
    const confirm = inspected.sources[0]?.descriptor ?? "no-record";
    return e.current.discardStoppedSource({ selector, confirm, holds, lifecycle, barePath: e.bare, ...quiet,
      hasRetainedHomeMarker: async () => true, removeRetainedHomeMarker: async () => {}, ...over } as never);
  }

  async function rollForwardBothDiscards(e: Env, attemptPaths: boolean, sFirstTerminalTombstone: boolean): Promise<void> {
    const { oPath } = await rc2ClaimsBranch(e, attemptPaths);
    const holdingBefore = tree(holdingRoot(e));
    const pBefore = tree(e.pPath);
    const oBefore = tree(oPath);
    const pKeyBefore = configEntries(e.bare).get(P_KEY);
    // (i) the rolled-forward strict resolver and every other consumer refuse, writing nothing.
    const before = snap(e);
    await assert.rejects(() => e.current.resolveBranchJournal(e.bare, BRANCH), /predecessor released or missing/);
    await assert.rejects(() => e.current.discoverOpaque(e.bare, BRANCH, OTHER), RestoreCompositeError);
    await assert.rejects(() => e.current.discoverOpaque(e.bare, BRANCH, RUN), RestoreCompositeError);
    assertSame(before, snap(e), [], "refusals");
    assert.deepEqual(configEntries(e.bare), before.config, "refusals leave the config byte-identical");
    assert.deepEqual(tree(holdingRoot(e)), holdingBefore);
    // (ii) S's discard: tombstone, no hand edit.
    if (!sFirstTerminalTombstone) {
      const s = await discardOn(e, RUN, S_ID, 2);
      assert.equal(s.kind, "done", JSON.stringify(s));
      assert.equal(fs.existsSync(e.sPath), false);
      assert.ok(configEntries(e.bare).get(S_KEY)?.[0]?.includes('"restoreStage":"retired"'), "the basis survives as the tombstone");
    }
    assert.deepEqual(configEntries(e.bare).get(P_KEY), pKeyBefore, "S's exit never writes P's key (it holds O's journal)");
    assert.deepEqual(tree(e.pPath), pBefore, "P's bytes are untouched by S's exit");
    assert.deepEqual(tree(oPath), oBefore, "O's clone is untouched");
    // (iii) P's discard.
    const removed: string[] = [];
    const p = await discardOn(e, RUN, P_ID, 1, { removeRetainedHomeMarker: async (run: string) => { removed.push(run); } });
    assert.equal(p.kind, "done", JSON.stringify(p));
    assert.deepEqual(removed, [RUN], "the marker hook runs once");
    assert.deepEqual(configEntries(e.bare).get(P_KEY), pKeyBefore, "P's key is byte-identical after P's exit");
    assert.equal(configEntries(e.bare).get(S_KEY)?.[0] ?? "", "", "the restore entry is gone");
    const states = (configEntries(e.bare).get(LEDGER_KEY) ?? []).map(v => JSON.parse(v) as { attemptId: string; state: string });
    assert.equal(states.filter(x => x.attemptId === P_ID).at(-1)?.state, "retired");
    assert.deepEqual(tree(oPath), oBefore, "O's clone is still untouched");
    assert.deepEqual(tree(holdingRoot(e)), holdingBefore, "the holding root is byte-identical: P's exit never touches it");
    if (attemptPaths) assert.equal(fs.existsSync(e.pPath), false, "the owner's authorised discard removed P's directory");
    else {
      assert.equal(p.kind === "done" && typeof p.holding === "string" && fs.existsSync(path.join(holdingRoot(e), p.holding)), true, "the holding directory is reported and intact");
      assert.equal(fs.readFileSync(path.join(holdingRoot(e), (p as { holding: string }).holding, "later.txt"), "utf8"), "later bytes that were never saved");
    }
    // (iv) O's own branch is O's again: its journal is an ordinary plain journal of O.
    assert.equal((await e.current.discoverOpaque(e.bare, BRANCH, OTHER)).kind, "sameRun");
  }

  it("mode A (attempt-path flight): rc.2 reclaim as O + reseed + markRecoveryCapture, roll forward: discard S then discard P reach done, P's key byte-identical", async () => {
    const e = await build();
    try { await rollForwardBothDiscards(e, true, false); } finally { e.fx.cleanup(); }
  });

  it("mode H (holding root): the same sequence on a flight that is not on attempt paths; the holding copy is reported and never touched", async () => {
    const e = await build();
    try { await rollForwardBothDiscards(e, false, false); } finally { e.fx.cleanup(); }
  });

  /** S discarded and its tombstone written on this build BEFORE the rollback. */
  async function tombstoneBeforeRollback(e: Env): Promise<void> {
    fs.rmSync(e.sPath, { recursive: true, force: true });
    git(e.bare, "config", "--add", LEDGER_KEY, JSON.stringify({ attemptId: S_ID, runId: RUN, clonePath: e.sPath, state: "retired" }));
    await e.current.advanceRestoreEntry(e.bare, BRANCH, "retired");
  }

  it("S terminal and its tombstone written BEFORE the rollback (mode H): only P's discard is needed and it reaches done", async () => {
    const e = await build();
    try { await tombstoneBeforeRollback(e); await rollForwardBothDiscards(e, false, true); } finally { e.fx.cleanup(); }
  });

  it("S terminal and its tombstone written BEFORE the rollback (mode A): rc.2's attempt seed compacts away S's retired ledger value, so the roll-forward REFUSES fail-closed (disclosed residual, same mechanism as B2)", async () => {
    const e = await build();
    try {
      await tombstoneBeforeRollback(e);
      const { oPath } = await rc2ClaimsBranch(e, true);
      const states = (configEntries(e.bare).get(LEDGER_KEY) ?? []).map(v => JSON.parse(v) as { attemptId: string; state: string });
      assert.equal(states.some(x => x.attemptId === S_ID), false, "rc.2's compactAttemptLedger dropped S's gone, unjournaled, retired entry");
      assert.equal(states.filter(x => x.attemptId === P_ID).at(-1)?.state, "reclaimed", "P's reclaimed entry is never compacted");
      const before = snap(e);
      const oBefore = tree(oPath);
      await assert.rejects(() => e.current.resolveBranchJournal(e.bare, BRANCH), RestoreCompositeError);
      const p = await discardOn(e, RUN, P_ID, 1);
      assert.notEqual(p.kind, "done", JSON.stringify(p));
      assertSame(before, snap(e), [], "dropped evidence");
      assert.deepEqual(configEntries(e.bare), before.config, "nothing was written");
      assert.deepEqual(tree(oPath), oBefore);
      assert.equal(fs.readFileSync(path.join(e.pPath, "later.txt"), "utf8"), "later bytes that were never saved", "P's bytes are intact");
    } finally { e.fx.cleanup(); }
  });

  it("B2 (disclosed residual): a SECOND rollback after S's discard, then an rc.2 attempt seed (whose ledger compaction runs once P's key holds no journal), drops the evidence; roll forward refuses fail-closed with P's bytes intact", async () => {
    const e = await build();
    try {
      // First rollback: rc.2 reclaims P (mode H: moved to the holding root, key cleared, ledger `live`). No reseed yet.
      const { reclaimError, diagnostics } = await reclaimAsOtherRun(e, false);
      assert.equal(reclaimError, undefined, `reclaim failed: ${String(reclaimError)} ${diagnostics.join("|")}`);
      // Roll forward: the recognised state lets S's own discard finish (the tombstone keeps the basis).
      assert.equal((await discardOn(e, RUN, S_ID, 2)).kind, "done");
      const heldBefore = tree(holdingRoot(e));
      // Second rollback: an rc.2 attempt seed on the branch (an attempt flight of the other run). Its seed-time compaction drops
      // a gone, unjournaled, non-reclaimed ledger entry unless the run has terminal protection.
      const third = formatAttemptId(new Date("2026-11-06T09:00:00Z"), 4, "cccccccccccccccc");
      const reseed = { beforeFree: async () => {}, isLive: () => false };
      await e.rc2.runnerCloneForBranch(e.bare, BRANCH, KEY, reseed, OTHER, false, undefined, { attemptId: third, isLive: () => false, beforeSeed: async () => {} } as never);
      const ledger = (configEntries(e.bare).get(LEDGER_KEY) ?? []).map(v => JSON.parse(v) as { attemptId: string; state: string });
      const dropped = [P_ID, S_ID].filter(id => !ledger.some(x => x.attemptId === id));
      assert.deepEqual(dropped, [P_ID, S_ID], "rc.2's compaction dropped P's live and S's retired entries (their paths are gone and nothing journals them)");
      const before = snap(e);
      await assert.rejects(() => e.current.resolveBranchJournal(e.bare, BRANCH), RestoreCompositeError);
      const p = await discardOn(e, RUN, P_ID, 1);
      assert.notEqual(p.kind, "done", `dropped evidence must refuse: ${JSON.stringify(p)}`);
      assert.equal(p.kind, "refused", "P has no ledger record left to select, so there is nothing to confirm");
      assert.deepEqual(configEntries(e.bare), before.config, "nothing was written");
      assert.deepEqual(tree(holdingRoot(e)), heldBefore, "P's bytes (the holding copy) are intact");
      assertSame(before, snap(e), [], "second rollback");
    } finally { e.fx.cleanup(); }
  });

  it("N1 control: rc.2 retainStoppedAttempt on a plain P writes the mode-A shape too, but is unreachable after an episode reservation, which it never clears", async () => {
    // (a) With the episode journal rc.2 reserves before it ever runs in P, retainStoppedAttempt appends `reclaimed` and does NOT clear
    //     the key: P's key still holds a journal of the run, which is not the released shape. Both exits stay refused, nothing written.
    const e = await build();
    try {
      const expected = { runId: RUN, clonePath: e.pPath, attemptId: P_ID };
      await e.rc2.reserveRecoveryIteration(e.bare, BRANCH, KEY, expected, 1000, 2);
      assert.equal(await e.rc2.retainStoppedAttempt(e.bare, BRANCH, expected), "retained");
      const key = JSON.parse(configEntries(e.bare).get(P_KEY)![0]!) as { runId: string; recovery?: unknown };
      assert.equal(key.runId, RUN);
      assert.ok(key.recovery, "the episode journal is not cleared");
      const before = snap(e);
      await assert.rejects(() => e.current.resolveBranchJournal(e.bare, BRANCH), /predecessor journal changed/);
      const s = await discardOn(e, RUN, S_ID, 2);
      assert.notEqual(s.kind, "done");
      assertSame(before, snap(e), [], "episode + reclaimed");
      assert.deepEqual(configEntries(e.bare), before.config);
    } finally { e.fx.cleanup(); }
    // (b) On a plain P (no episode) the same call yields exactly the mode-A end state, and the roll-forward recognises it like any other
    //     mode-A state: recognition grants the guarded discards and nothing more.
    const f = await build();
    try {
      assert.equal(await f.rc2.retainStoppedAttempt(f.bare, BRANCH, { runId: RUN, clonePath: f.pPath, attemptId: P_ID }), "retained");
      assert.equal(configEntries(f.bare).get(P_KEY)?.[0] ?? "", "");
      await assert.rejects(() => f.current.resolveBranchJournal(f.bare, BRANCH), /predecessor released or missing/);
      assert.equal((await discardOn(f, RUN, S_ID, 2)).kind, "done");
      assert.equal((await discardOn(f, RUN, P_ID, 1)).kind, "done");
    } finally { f.fx.cleanup(); }
  });
});

// ---- m4c: the PRODUCTION Cancel path and the PRODUCTION guarded discard, on the state rc.2 leaves ---------------------------------
//
// One fixture per mode, end to end: the unmodified rc.2 terminal/reclaim sequence (rc2ClaimsBranch), the same files read by this build,
// a later claim of RUN on the production RunRunner (independent-restore-rc2-runner.ts) with an owner Cancel queued, then the guarded
// discard of S and of P through the production RetainedSourceServer over its socket, with a real retained-home marker on disk.

const homeSweep = (root: string) => reclaimStrandedRunHomes(root, async () => "failed", nullLogger(), { minAgeMs: 0, now: () => Date.now() + 10_000 });
type Json = Record<string, any>;
const ident = (p: string): { dev: string; ino: string } => {
  const st = fs.lstatSync(p, { bigint: true });
  return { dev: String(st.dev), ino: String(st.ino) };
};
/** The GitCache entry points that adopt, seed, capture, reserve, release or discard: a stopped claim must call none of them. */
const MUTATORS = ["seedIndependentAttempt", "markRecoveryCapture", "reserveRecoveryIteration", "blockRecoveryEpisode", "recordRecoveryCapture",
  "retireRunnerClone", "releaseAttemptInPlace", "installCompletionSourceFence", "discardStoppedSource", "discardProtectedSource"] as const;
/** Count every call of the MUTATORS on `target` (mock.method throws when a name is not a function there). */
function spyMutators(target: object): Record<string, number> {
  const calls: Record<string, number> = {};
  const t = target as Record<string, (...a: unknown[]) => unknown>;
  for (const name of MUTATORS) {
    const original = t[name]!.bind(target);
    mock.method(t, name, (...args: unknown[]) => { calls[name] = (calls[name] ?? 0) + 1; return original(...args); });
  }
  return calls;
}
/** What the production runner logs for a later claim of RUN on the released state, in order (recorded from the run, not chosen). */
const STOP_LOG = ["run claimed", "restore entry present; claim stopped", "retained recovery stopped; source and accounting kept",
  "retained recovery stopped; preserving plugin dir and HOME for a same-worker resume"];
/** What it logs, in natural order (no waiting), when the owner Cancel is persisted server-side. Two orders occur and both are the
 *  one cancelled report. The proof queues a real cancel input row and steering routes it either before discoverOpaque returns (so
 *  the lifecycle guard throws before the stop line), or during the stop's cancel observation's awaits before its read (the read
 *  then sees the terminal lifecycle and returns unknown without committing); which of the two happens is timing, not mode. Either
 *  way the discovery catch's terminal guard (runner.ts, retainedTerminalGuard) reports it. Neither accepted sequence contains the
 *  observation's commit line, so its COMMIT (observeCancelAtHeldSeam at the restore stops) is not exercised here;
 *  runner-independent-restore-cancel.test.ts proves it. Derived from the code and recorded from the runs, not chosen. */
const CANCEL_TAIL = ["run failed", "run event", "outbox: terminal journaled write-ahead", "recovery: unadopted guarded generation disposition",
  "retained recovery stopped; source and accounting kept", "retained recovery stopped; preserving plugin dir and HOME for a same-worker resume"];
const CANCEL_LOGS = [["run claimed", ...CANCEL_TAIL], ["run claimed", "restore entry present; claim stopped", ...CANCEL_TAIL]];

describe("m4c: the production Cancel path and the production guarded discard on the state rc.2 leaves", () => {
  const cleanups: Array<() => Promise<void> | void> = [];
  afterEach(async () => {
    mock.restoreAll();
    for (const c of cleanups.splice(0).reverse()) await c();
  });
  const holdingRootOf = (e: Env): string => path.join(e.dataDir, "runner-quarantine");
  const markerFile = (e: Env): string => path.join(e.dataDir, "retained-homes", `${RUN}.json`);

  interface Released { e: Env; oPath: string; pHolder: string; modeA: boolean }

  /** The P/S restore of this build, S terminal, the real marker the seed writes, then rc.2's own claim of the branch by another run. */
  async function released(modeA: boolean): Promise<Released> {
    const e = await build();
    cleanups.push(() => e.fx.cleanup());
    await writeRetainedHomeMarker(e.dataDir, { version: 1, runId: RUN, predecessorGeneration: 1, predecessorAttemptId: P_ID, barePath: e.bare });
    const { oPath } = await rc2ClaimsBranch(e, modeA);
    let pHolder = e.pPath;
    if (!modeA) {
      const held = fs.readdirSync(holdingRootOf(e)).filter(n => isHoldingName(n, RUN));
      assert.equal(held.length, 1, `rc.2 moved P to exactly one holding directory: ${JSON.stringify(held)}`);
      pHolder = path.join(holdingRootOf(e), held[0]!);
    }
    return { e, oPath, pHolder, modeA };
  }

  /** Every byte and every binding the discard flow may not change, in one comparable object. */
  function world(r: Released): Record<string, unknown> {
    const { e } = r;
    return { config: configEntries(e.bare), refs: refList(e.bare), pHolder: tree(r.pHolder), holding: tree(holdingRootOf(e)), s: tree(e.sPath), o: tree(r.oPath),
      pHome: tree(path.join(e.homeRoot, RUN)), pProvision: tree(path.join(e.provisionRoot, RUN)),
      sHome: tree(path.join(e.homeRoot, `${RUN}.g2`)), sProvision: tree(path.join(e.provisionRoot, `${RUN}.g2`)),
      marker: fs.existsSync(markerFile(e)) ? fs.readFileSync(markerFile(e), "utf8") : null };
  }
  function assertWorld(before: Record<string, unknown>, after: Record<string, unknown>, except: string[], what: string): void {
    for (const key of Object.keys(before)) if (!except.includes(key)) assert.deepEqual(after[key], before[key], `${what}: ${key} unchanged`);
  }
  const ledgerOf = (e: Env): Array<{ attemptId: string; runId: string; clonePath: string; state: string }> =>
    (configEntries(e.bare).get(LEDGER_KEY) ?? []).map(v => JSON.parse(v) as { attemptId: string; runId: string; clonePath: string; state: string });

  /** Plan 3.11's exact bindings of the rolled-forward state, read from the files alone. */
  function assertBindings(r: Released): void {
    const { e } = r;
    const config = configEntries(e.bare);
    const value = parseRestoreValue(config.get(S_KEY)![0]!);
    assert.equal(value.kind, "entry", "S's restore entry is intact");
    const basis = value.basis;
    assert.deepEqual({ clonePath: basis.predecessor.clonePath, attemptId: basis.predecessor.attemptId, generation: basis.predecessor.generation },
      { clonePath: e.pPath, attemptId: P_ID, generation: 1 }, "the basis names P's path, attempt and generation");
    assert.equal(basis.journalDigest, restoreDigest(e.pRaw), "the basis digest is P's own journal's");
    assert.deepEqual(basis.pin, { captureId: "capture-1", generation: 1, sourceSha: e.pinSha, finalization: true });
    assert.deepEqual(basis.predecessor.root, ident(r.pHolder), "the recorded root dev/ino is the inode of P's bytes wherever they are now");
    assert.equal(fs.readFileSync(path.join(r.pHolder, "later.txt"), "utf8"), "later bytes that were never saved");
    const ledger = ledgerOf(e);
    assert.deepEqual(ledger.filter(x => x.attemptId === S_ID).at(-1), { attemptId: S_ID, runId: RUN, clonePath: e.sPath, state: "live" });
    assert.deepEqual(ledger.filter(x => x.attemptId === P_ID).at(-1), { attemptId: P_ID, runId: RUN, clonePath: e.pPath, state: r.modeA ? "reclaimed" : "live" },
      "P's ledger value is the rc.2 one of its mode");
    const pKey = JSON.parse(config.get(P_KEY)![0]!) as { runId: string; clonePath: string };
    assert.equal(pKey.runId, OTHER, "P's key holds O's journal");
    assert.equal(pKey.clonePath, r.oPath);
    assert.equal(fs.existsSync(e.pPath), r.modeA, r.modeA ? "mode A: P is at its own path" : "mode H: P moved to the holding root");
    assert.equal(fs.existsSync(path.join(e.sPath, "work.txt")), true, "S's clone is intact");
    const marker = JSON.parse(fs.readFileSync(markerFile(e), "utf8")) as Json;
    assert.deepEqual({ runId: marker.runId, attempt: marker.predecessorAttemptId, bare: marker.barePath }, { runId: RUN, attempt: P_ID, bare: e.bare });
  }

  /** A later claim of RUN on the production RunRunner over the proof's own files. */
  async function laterClaim(r: Released, cancel: boolean): Promise<{ run: RunnerOver; calls: Record<string, number> }> {
    const run = await runnerOverDisk({ dataDir: r.e.dataDir, originPath: r.e.fx.originPath, runId: RUN, iid: 3644, claimGeneration: 3,
      homeDir: path.join(r.e.homeRoot, RUN), cancel });
    cleanups.push(() => run.close());
    const calls = spyMutators(run.git);
    await run.runner.execute(run.claim);
    return { run, calls };
  }
  /** The observed Cancel outcome: the claim's `running` ack, then ONE terminal report, `failed` / "run cancelled" (the runner's cancelled report;
   *  the dispatcher keeps custody with it), and no adoption, seed, capture, reservation, FINAL, release or discard of any kind. */
  function assertCancelledReport(run: RunnerOver, calls: Record<string, number>): void {
    assert.deepEqual(run.api.states.map(s => ({ status: s.body.status, reason: s.body.failure_reason })),
      [{ status: "running", reason: undefined }, { status: "failed", reason: "run cancelled" }], "running ack, then the cancelled report, nothing else");
    assert.deepEqual(calls, {}, "no adoption, restore seed, capture, reservation, FINAL, release or discard was attempted");
    assert.equal(run.counters.ran, 0, "no model turn");
    const logged = (run.logs.lines as Array<{ msg: string }>).map(l => l.msg);
    assert.ok(CANCEL_LOGS.some(expected => JSON.stringify(expected) === JSON.stringify(logged)), `a natural-order Cancel log: ${JSON.stringify(logged)}`);
  }
  /** An uncancelled claim: the composite stop, nonterminal. */
  function assertNonterminalStop(run: RunnerOver, calls: Record<string, number>): void {
    assert.deepEqual(run.api.states.map(s => s.body.status), ["running"], "the claim's running ack and nothing else: no failed, completed, paused or cancelled report");
    assert.deepEqual(calls, {}, "no adoption, restore seed, capture, reservation, FINAL, release or discard was attempted");
    assert.equal(run.counters.ran, 0, "no model turn");
    const lines = run.logs.lines as Array<{ msg: string; error?: string }>;
    assert.deepEqual(lines.map(l => l.msg), STOP_LOG);
    assert.match(lines[1]!.error ?? "", /restore entry names another run \(predecessor released or missing\)/);
  }

  async function serve(r: Released, run: RunnerOver): Promise<string> {
    const server = new RetainedSourceServer({
      dataDir: r.e.dataDir, git: run.git, gate: new DindPruneGate(), runner: run.runner, log: nullLogger(), isIdle: () => true, authority: run.authority,
      client: { listRecoveryHolds: async (runId: string) => ({ run_id: runId, holds: [] }), getRunOwnership: async () => ({ status: "failed", claim_generation: 3 }) } as never,
    });
    const ac = new AbortController();
    const served = server.serve(ac.signal);
    await server.ready;
    cleanups.push(async () => { ac.abort(); await server.close(); await served.catch(() => undefined); });
    return server.path;
  }
  function send(sock: string, req: unknown): Promise<Json> {
    return new Promise((resolve, reject) => {
      const conn = net.createConnection(sock);
      const chunks: Buffer[] = [];
      conn.on("error", reject);
      conn.on("connect", () => conn.write(`${JSON.stringify(req)}\n`));
      conn.on("data", c => chunks.push(Buffer.from(c)));
      conn.on("end", () => { try { resolve(JSON.parse(Buffer.concat(chunks).toString("utf8")) as Json); } catch (err) { reject(err); } });
    });
  }
  /** inspect, then discard with the digest inspect returned: the owner's own two requests. `confirm` overrides the digest. */
  async function ownerDiscard(sock: string, generation: number, confirm?: string): Promise<Json & { found: boolean }> {
    const listed = await send(sock, { op: "inspect" });
    const found = (listed.sources as Json[]).filter(x => x.runId === RUN && x.generation === generation);
    if (found.length !== 1) return { found: false, result: { kind: "not_listed", sources: found.length } } as Json & { found: boolean };
    const source = found[0]!;
    const out = await send(sock, { op: "discard", run: RUN, generation: source.generation, attempt: source.attempt, confirm: confirm ?? source.descriptor });
    return { ...out, found: true };
  }
  /** A server over a fresh runner that has not claimed anything. */
  async function idleServer(r: Released): Promise<string> {
    const run = await runnerOverDisk({ dataDir: r.e.dataDir, originPath: r.e.fx.originPath, runId: RUN, iid: 3644, claimGeneration: 3, homeDir: path.join(r.e.homeRoot, RUN) });
    cleanups.push(() => run.close());
    return serve(r, run);
  }

  it("wiring control: the MUTATORS spy records a call to each mutator, so an empty record on a stop is not vacuous", () => {
    const stub: Record<string, () => string> = {};
    for (const name of MUTATORS) stub[name] = () => name;
    const calls = spyMutators(stub);
    assert.deepEqual(calls, {});
    for (const name of MUTATORS) assert.equal(stub[name]!(), name, "the spied method still delegates");
    assert.deepEqual(calls, Object.fromEntries(MUTATORS.map(name => [name, 1])));
  });

  for (const modeA of [true, false]) {
    const mode = modeA ? "A (attempt paths, in_place)" : "H (holding root)";

    it(`mode ${mode}: the rolled-forward files bind exactly P, S, the ledger, the restore entry and the inode`, async () => {
      assertBindings(await released(modeA));
    });

    it(`mode ${mode}: an owner Cancel on a later claim of RUN is reported cancelled and touches nothing; then S's and P's guarded discards run through the production server`, async () => {
      const r = await released(modeA);
      assertBindings(r);
      const before = world(r);
      // 3. The production Cancel path: RunRunner.execute on the rolled-forward state, an owner Cancel queued for the run.
      const { run, calls } = await laterClaim(r, true);
      assertCancelledReport(run, calls);
      assertWorld(before, world(r), [], "the Cancel claim");
      assertBindings(r);
      // 4. The guarded discards, through the production RetainedSourceServer and the marker hooks it wires.
      const sock = await serve(r, run);
      const sDone = await ownerDiscard(sock, 2);
      assert.equal(sDone.result.kind, "done", JSON.stringify(sDone));
      assert.equal(fs.existsSync(r.e.sPath), false, "S's clone is gone");
      assert.ok(configEntries(r.e.bare).get(S_KEY)?.[0]?.includes('"restoreStage":"retired"'), "the basis survives as the tombstone");
      const afterS = world(r);
      assertWorld(before, afterS, ["config", "s", "sHome", "sProvision"], "S's discard");
      assert.deepEqual(afterS.marker, before.marker, "S's discard leaves the marker");
      assert.deepEqual(configEntries(r.e.bare).get(P_KEY), (before.config as Map<string, string[]>).get(P_KEY), "S's exit never writes P's key (it holds O's journal)");
      assert.equal(fs.readFileSync(path.join(r.pHolder, "later.txt"), "utf8"), "later bytes that were never saved", "P's bytes survive S's exit");
      // P's own authorised discard.
      const pDone = await ownerDiscard(sock, 1);
      assert.equal(pDone.result.kind, "done", JSON.stringify(pDone));
      assert.equal(fs.existsSync(markerFile(r.e)), false, "the production hook removed the marker with P's exit");
      assert.deepEqual(configEntries(r.e.bare).get(P_KEY), (before.config as Map<string, string[]>).get(P_KEY), "P's key (O's journal) is byte-identical after P's exit");
      assert.equal(configEntries(r.e.bare).get(S_KEY)?.[0] ?? "", "", "the restore entry is gone");
      assert.equal(ledgerOf(r.e).filter(x => x.attemptId === P_ID).at(-1)?.state, "retired");
      assert.equal(ledgerOf(r.e).filter(x => x.attemptId === S_ID).at(-1)?.state, "retired");
      assert.deepEqual(tree(r.oPath), before.o, "O's clone is untouched by every step");
      assert.deepEqual(tree(path.join(r.e.homeRoot, RUN)), before.pHome, "P's legacy HOME is not touched by the discard");
      assert.deepEqual(tree(path.join(r.e.provisionRoot, RUN)), before.pProvision);
      if (modeA) assert.equal(fs.existsSync(r.e.pPath), false, "the owner's authorised discard removed P's directory");
      else {
        assert.deepEqual(tree(holdingRootOf(r.e)), before.holding, "the holding root is byte-identical after P's exit: the copy stays for the owner");
        assert.equal(pDone.result.holding, path.basename(r.pHolder), "the holding copy is reported by name");
      }
      assert.equal((await r.e.current.discoverOpaque(r.e.bare, BRANCH, OTHER)).kind, "sameRun", "O's branch is O's again");
    });

    it(`mode ${mode}: on the released state every adoption, restore, FINAL, release and cleanup refuses and writes nothing`, async () => {
      const r = await released(modeA);
      const before = world(r);
      const { e } = r;
      const reseed = { beforeFree: async () => {}, isLive: () => false };
      const attempt = (id: string) => ({ attemptId: id, isLive: () => false, beforeSeed: async () => {} });
      const newId = formatAttemptId(new Date("2026-11-06T12:00:00Z"), 4, "eeeeeeeeeeeeeeee");
      const expected = { runId: RUN, clonePath: e.pPath, attemptId: P_ID };
      const guarded = async (what: string, fn: () => Promise<unknown>): Promise<void> => {
        await assert.rejects(fn, (err: unknown) => {
          const x = err as Error & { cause?: unknown; reason?: string };
          const typed = err instanceof RestoreCompositeError || x.cause instanceof RestoreCompositeError || (x.name === "RestoreSeedError" && x.reason === "restore_entry_exists") ||
            (what.startsWith("runnerCloneForBranch") && x.name === "ForeignCaptureBlockedError");
          assert.ok(typed, `${what}: ${x.name}: ${x.message}`);
          return true;
        });
      };
      await guarded("resolveBranchJournal", () => e.current.resolveBranchJournal(e.bare, BRANCH));
      await guarded("discoverOpaque(RUN)", () => e.current.discoverOpaque(e.bare, BRANCH, RUN));
      await guarded("discoverOpaque(O)", () => e.current.discoverOpaque(e.bare, BRANCH, OTHER));
      for (const run of [RUN, OTHER]) await guarded(`runnerCloneForBranch(${run})`, () => e.current.runnerCloneForBranch(e.bare, BRANCH, KEY, reseed, run, false, undefined, attempt(newId) as never));
      await guarded("reserveRecoveryIteration", () => e.current.reserveRecoveryIteration(e.bare, BRANCH, KEY, expected, 1000, 2));
      await guarded("markRecoveryCapture(P)", () => e.current.markRecoveryCapture(e.bare, e.pPath, BRANCH, RUN, P_ID));
      await guarded("recordRecoveryCapture", () => e.current.recordRecoveryCapture(e.bare, BRANCH, KEY, expected, 1, e.pinSha));
      await guarded("installCompletionSourceFence", () => e.current.installCompletionSourceFence(e.bare, BRANCH, expected,
        { version: 1, sourceUseEpoch: "0123abcd-0123-4abc-8def-0123456789ab", sourceCoreDigest: "a".repeat(64) }));
      for (const discard of [false, true]) {
        await guarded(`retireRunnerClone(discard ${discard})`, () => e.current.retireRunnerClone(e.bare, e.pPath, BRANCH, RUN,
          discard ? { discard, completionSource: { expectedHead: e.pinSha, canDelete: () => true } } : { discard }));
      }
      assert.equal(await e.current.restoreKeepsRecoveryRef(e.bare, RUN, { sha: e.pinSha }), true, "the pin stays protected");
      assertWorld(before, world(r), [], "refusals");
      // Automatic cleanup: this build's own HOME sweeps, the run terminal, keep P's legacy HOME and provision directory (the marker). What
      // they do reclaim is S's per-attempt `<run>.g2` directories, which the marker deliberately does not cover (disk-reclaim.ts).
      for (const root of [e.homeRoot, e.provisionRoot]) {
        const swept = await homeSweep(root);
        assert.equal(swept.skippedRetainedHome, 1, `${root}: the marker protects the legacy directory`);
        assert.equal(swept.removed, 1, `${root}: only S's own per-attempt directory is reclaimed`);
      }
      const swept = world(r);
      for (const key of ["pHome", "pProvision", "config", "refs", "pHolder", "holding", "s", "o", "marker"]) assert.deepEqual(swept[key], before[key], `sweeps: ${key} unchanged`);
      assert.deepEqual([swept.sHome, swept.sProvision], [{}, {}], "S's per-attempt HOME and provision directory follow their terminal run");
    });

    it(`mode ${mode}: UNSAFE-ADOPTION CONTROL: an uncancelled claim of RUN on the released state adopts nothing and stops as the Cancel claim does`, async () => {
      const r = await released(modeA);
      const before = world(r);
      const { run, calls } = await laterClaim(r, false);
      assertNonterminalStop(run, calls);
      assertWorld(before, world(r), [], "the uncancelled claim");
    });
  }

  it("controls (mode H, partial attribution): no holding directory, two holding directories and a malformed name refuse P's discard; P's own directory then completes it", async t => {
    const r = await released(false);
    const sock = await idleServer(r);
    assert.equal((await ownerDiscard(sock, 2)).result.kind, "done");
    const held = r.pHolder;
    const aside = path.join(r.e.dataDir, "p-aside");
    const refused = async (what: string): Promise<void> => {
      const before = world(r);
      const out = await ownerDiscard(sock, 1);
      t.diagnostic(`${what}: ${JSON.stringify({ found: out.found, kind: out.result.kind, reason: out.result.reason })}`);
      assert.equal(out.found, true, `${what}: the source is listed, so the refusal comes from the discard and not a missing record: ${JSON.stringify(out)}`);
      assert.deepEqual(out.result, { kind: "unknown", sources: [], metadata: "kept" }, `${what}: the discard answers unknown and keeps the metadata: ${JSON.stringify(out)}`);
      assert.deepEqual(world(r), before, `${what}: nothing written`);
    };
    fs.renameSync(held, aside);
    await refused("no holding directory");
    fs.renameSync(aside, held);
    const second = path.join(holdingRootOf(r.e), `${holdingNamePrefix(RUN)}${randomUUID()}`);
    fs.mkdirSync(second, { mode: 0o700 });
    await refused("two holding directories");
    fs.rmdirSync(second);
    const malformed = path.join(holdingRootOf(r.e), `${RUN}-not-a-uuid`);
    fs.mkdirSync(malformed, { mode: 0o700 });
    await refused("a malformed holding name of the run");
    fs.rmdirSync(malformed);
    const done = await ownerDiscard(sock, 1);
    assert.equal(done.result.kind, "done", `control: ${JSON.stringify(done)}`);
    assert.equal(done.result.holding, path.basename(held));
  });

  it("controls (digest mismatch): a wrong confirm digest is refused before anything is touched; P's key holding a journal of RUN is not the released shape", async t => {
    const r = await released(true);
    const sock = await idleServer(r);
    const before = world(r);
    const wrong = await ownerDiscard(sock, 2, "0".repeat(64));
    assert.equal(wrong.result.kind, "refused", JSON.stringify(wrong));
    assert.equal(wrong.result.reason, "descriptor_changed");
    assertWorld(before, world(r), [], "wrong confirm digest");
    // P's key now holds a journal of RUN itself (its digest is not the basis's, and it is not O's): ambiguous, never released.
    git(r.e.bare, "config", P_KEY, JSON.stringify({ runId: RUN, clonePath: r.e.pPath, attemptId: P_ID, peeledMarkers: ["a".repeat(40)] }));
    const tampered = world(r);
    for (const generation of [2, 1]) {
      const out = await ownerDiscard(sock, generation);
      assert.equal(out.found, true, `generation ${generation}: the source is listed: ${JSON.stringify(out)}`);
      assert.deepEqual(out.result, { kind: "unknown", sources: [], metadata: "kept" }, `generation ${generation}: the discard answers unknown and keeps the metadata: ${JSON.stringify(out)}`);
      t.diagnostic(`journal of RUN in P's key, generation ${generation}: ${JSON.stringify({ found: out.found, kind: out.result.kind })}`);
    }
    assertWorld(tampered, world(r), [], "a journal of RUN in P's key");
  });

  it("controls (mode H, wrong inode): a sole run-prefixed directory that is a COPY of P's refuses P's discard; P's own inode completes it", async t => {
    const r = await released(false);
    const sock = await idleServer(r);
    assert.equal((await ownerDiscard(sock, 2)).result.kind, "done");
    const real = r.pHolder;
    const recorded = parseRestoreValue(configEntries(r.e.bare).get(S_KEY)![0]!).basis.predecessor.root;
    assert.deepEqual(ident(real), recorded, "control: the recorded inode is the holding copy's");
    const aside = path.join(r.e.dataDir, "p-aside");
    fs.renameSync(real, aside);
    execFileSync("cp", ["-a", aside, real], { env: ENV });
    assert.notDeepEqual(ident(real), recorded, "the copy has another inode");
    assert.equal(fs.readFileSync(path.join(real, "later.txt"), "utf8"), "later bytes that were never saved", "and the same bytes");
    const before = world(r);
    const out = await ownerDiscard(sock, 1);
    assert.equal(out.found, true, "the copy is listed as P's source, so the refusal below comes from the binding and not from a missing record");
    assert.notEqual(out.result.kind, "done", JSON.stringify(out));
    t.diagnostic(`wrong inode: ${JSON.stringify({ kind: out.result.kind, reason: out.result.reason })}`);
    assert.deepEqual(world(r), before, "nothing written, nothing deleted");
    assert.equal(fs.existsSync(markerFile(r.e)), true);
    fs.rmSync(real, { recursive: true });
    fs.renameSync(aside, real);
    const done = await ownerDiscard(sock, 1);
    assert.equal(done.result.kind, "done", `control: ${JSON.stringify(done)}`);
    assert.equal(fs.existsSync(markerFile(r.e)), false);
  });
});
