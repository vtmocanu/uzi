import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import {
  GitCache, type IndependentSeedInput, type PositiveOwedCandidateContext, type RecoveryJournalEntry,
} from "../src/git.js";
import { restoreDigest, restoreJournalKey } from "../src/restore-journal.js";
import { attemptClonePath, formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

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
after(async () => { if (suite) await fsp.rm(path.join(suite, "reader-rc2"), { recursive: true, force: true }); });

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
  execFileSync("git", ["clone", "-q", "--shared", "--no-checkout", bare, pPath], { env: ENV });
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
    predecessor: { clonePath: pPath, attemptId: P_ID, generation: 1, journalDigest: restoreDigest(pRaw), slotOwner: { worker: "w1", epoch: "e1" } },
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

  it("item 9, first allowance: a reservation and a preservation_failed blocker change P's journal and nothing else", async () => {
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
      // The only other bare-config write is rc.2's own attempt-mode flag for the key; S's trace is untouched.
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

  it("KNOWN RESIDUAL (not an item-9 allowance): rc.2's reclaim of a terminal owner's attempt releases P in place, clearing P's journal", async () => {
    const e = await build();
    try {
      const before = snap(e);
      await e.rc2.releaseAttemptInPlace(e.bare, e.pPath, BRANCH, RUN, "reclaimed");
      const after = snap(e);
      // Observed and recorded: this is the one rc.2 path that rewrites P's journal beyond item 9's allowances. P's bytes, S and the
      // pins are untouched; a roll-forward reads P's empty key against S's entry and refuses fail-closed. Plan 3.11 / #3981.
      assert.equal(after.config.get(P_KEY)?.[0] ?? "", "", "P's journal is cleared");
      assert.deepEqual(after.p, before.p);
      assert.deepEqual(after.s, before.s);
      assert.equal(after.config.get(S_KEY)?.[0], before.config.get(S_KEY)?.[0]);
      assert.equal(after.refs, before.refs);
      await assert.rejects(() => e.current.resolveBranchJournal(e.bare, BRANCH), /predecessor journal is empty/);
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
