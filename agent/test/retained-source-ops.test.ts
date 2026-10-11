import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { attemptGeneration, formatAttemptId } from "../src/attempt-path.js";
import {
  GitCache,
  RETAINED_MAX_DESCRIPTORS,
  type AttemptLedgerState,
  type AttemptSeedOptions,
  type RetainedHoldsSnapshot,
  type RetainedSourceSelector,
} from "../src/git.js";
import { RetainedStoppedSourceError, RunResidueBlockedError, latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";

// issue #1786 M0a — the GitCache seam of the stopped-source retention and the explicit operator
// reclaim path: retainStoppedAttempt, markStoppedCanonical, verifyStoppedSourceCovered, inspect and
// the plain/protected discard, all against a REAL GitCache and real Git.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const NO_HOLDS: RetainedHoldsSnapshot = { state: "read", holds: [] };
const quiet = async (): Promise<boolean> => true;
const notLive = (): boolean => false;

let fx: Fixture;
let git: GitCache;
let bare: string;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
function cfgAll(key: string): string[] {
  try {
    return gitIn(bare, ["config", "--local", "--get-all", key]).split("\n").filter((l) => l !== "");
  } catch {
    return [];
  }
}
function hex16(n: number): string {
  return n.toString(16).padStart(16, "0");
}
function attemptIdAt(n: number, ...rest: [gen?: number | undefined]): string {
  const gen = rest.length > 0 ? rest[0] : 1;
  return formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, n)), gen, hex16(n + 1));
}
function ledgerState(branch: string, attemptId: string): string | undefined {
  let state: string | undefined;
  for (const raw of cfgAll(`uzi-attempts.${branch}.entry`)) {
    const e = JSON.parse(raw) as { attemptId: string; state: string };
    if (e.attemptId === attemptId) state = e.state;
  }
  return state;
}
function journalRaw(branch: string): string {
  try {
    return gitIn(bare, ["config", "--local", `uzi-recovery.${branch}.clone`]);
  } catch {
    return "";
  }
}
function configBytes(): Buffer {
  return fs.readFileSync(path.join(bare, "config"));
}
function seedOpts(attemptId: string, over: Partial<AttemptSeedOptions> = {}): AttemptSeedOptions {
  return { attemptId, isLive: notLive, beforeSeed: async () => {}, quiescent: quiet, ...over };
}

interface Attempt {
  branch: string;
  key: string;
  runId: string;
  attemptId: string;
  path: string;
  clonePath: string;
}

/** A REAL attempt clone, seeded through the production seed and journaled like phaseClone does. */
async function seedAttempt(runId: string, n: number, opts: { iid?: number; gen?: number | undefined; journal?: boolean } = {}): Promise<Attempt> {
  const iid = opts.iid ?? 2512;
  const branch = `agent/issue-${iid}`;
  const key = `issue-${iid}`;
  const attemptId = attemptIdAt(n, "gen" in opts ? opts.gen : 1);
  const clone = await git.runnerCloneForBranch(bare, branch, key, noProofReseed, runId, false, undefined, seedOpts(attemptId));
  if (opts.journal !== false) await git.markRecoveryCapture(bare, clone.path, branch, runId, attemptId);
  return { branch, key, runId, attemptId, path: clone.path, clonePath: clone.path };
}

/** A REAL canonical clone with its plain journal. */
async function seedCanonical(runId: string, iid = 2600): Promise<{ branch: string; key: string; runId: string; path: string }> {
  const branch = `agent/issue-${iid}`;
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, runId);
  await git.markRecoveryCapture(bare, clone.path, branch, runId);
  return { branch, key: `issue-${iid}`, runId, path: clone.path };
}

/** A planted attempt dir (no git inside) with its skills sibling and a ledger value. */
function plantAttempt(branch: string, key: string, attemptId: string, runId: string, state: AttemptLedgerState): { dir: string; skills: string } {
  const canonical = git.runnerClonePath(bare, key);
  const dir = `${canonical}.attempt-${attemptId}`;
  const skills = path.join(path.dirname(dir), `.uzi-skills-${path.basename(dir)}`);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, "WORK.txt"), attemptId);
  fs.mkdirSync(skills, { recursive: true });
  fs.writeFileSync(path.join(skills, "plugin.json"), "{}");
  gitIn(bare, ["config", "--local", "--add", `uzi-attempts.${branch}.entry`, JSON.stringify({ attemptId, runId, clonePath: dir, state })]);
  return { dir, skills };
}

function selectorOf(a: { runId: string; attemptId?: string }, generation: number | "none"): RetainedSourceSelector {
  return { runId: a.runId, generation, attempt: a.attemptId ?? "canonical" };
}

async function inspectOne(sel: RetainedSourceSelector, holds: RetainedHoldsSnapshot = NO_HOLDS, isLivePath: (p: string) => boolean = notLive) {
  const r = await git.inspectRetainedSources({ selector: sel, holds, isLivePath, barePath: bare });
  assert.equal(r.sources.length, 1, `exactly one source for ${JSON.stringify(sel)}`);
  return r.sources[0]!;
}

function discardOpts(sel: RetainedSourceSelector, confirm: string, over: Record<string, unknown> = {}) {
  return { selector: sel, confirm, holds: NO_HOLDS, isLivePath: notLive, quiesce: quiet, barePath: bare, ...over };
}

beforeEach(async () => {
  fx = makeFixture();
  const dbg = process.env.OPS_DEBUG ? { ...nullLogger(), warn: (...a: unknown[]) => { console.error("WARN", JSON.stringify(a)); } } : nullLogger();
  git = new GitCache(fx.dataDir, dbg, undefined, testGitCacheOptions());
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
});

afterEach(() => {
  resetResidueQuarantineForTests();
  fx.cleanup();
});

describe("attemptGeneration", () => {
  it("parses the claim generation and reports none for gx or a non-id", () => {
    assert.equal(attemptGeneration(attemptIdAt(1, 7)), 7);
    assert.equal(attemptGeneration(attemptIdAt(1, 0)), 0);
    assert.equal(attemptGeneration(attemptIdAt(1, undefined)), undefined, "gx has no generation");
    assert.equal(attemptGeneration("20261008T171401Z-g5-nothex"), undefined);
    assert.equal(attemptGeneration("garbage"), undefined);
  });
});

describe("retainStoppedAttempt", () => {
  it("appends ledger reclaimed, clears the plain journal naming the source and leaves the bytes", async () => {
    const a = await seedAttempt("run-a", 1);
    fs.writeFileSync(path.join(a.path, "dirty.txt"), "unpreserved\n");
    assert.equal(ledgerState(a.branch, a.attemptId), "live");
    assert.notEqual(journalRaw(a.branch), "");
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed");
    assert.equal(journalRaw(a.branch), "", "the journal is cleared so no later claim captures or adopts it");
    assert.equal(fs.readFileSync(path.join(a.path, "dirty.txt"), "utf8"), "unpreserved\n");
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained", "idempotent");
    assert.equal(await git.discoverRetainedRecovery(fx.originPath, a.branch, a.key, a.runId), undefined);
  });

  it("a same-run successor seeds a fresh attempt with no capture or failure; the old attempt is untouched", async () => {
    const a = await seedAttempt("run-a", 1);
    fs.writeFileSync(path.join(a.path, "dirty.txt"), "unpreserved\n");
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const next = await seedAttempt("run-a", 2, { gen: 2 });
    assert.notEqual(next.path, a.path);
    assert.equal(fs.readFileSync(path.join(a.path, "dirty.txt"), "utf8"), "unpreserved\n");
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed");
    assert.equal(ledgerState(next.branch, next.attemptId), "live");
    assert.equal(JSON.parse(journalRaw(next.branch)).clonePath, next.path, "the successor's own journal");
  });

  it("a sweep, compactAttemptLedger and the abandoned cap keep a reclaimed stopped attempt", async () => {
    const a = await seedAttempt("run-a", 1);
    fs.writeFileSync(path.join(a.path, "dirty.txt"), "unpreserved\n");
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    // Five older abandoned attempts push the key past the cap of three; the oldest are swept.
    const swept = [10, 11, 12, 13, 14].map((i) => plantAttempt(a.branch, a.key, attemptIdAt(i), `old-${i}`, "abandoned"));
    // Duplicate ledger values force a real compaction on the next seed.
    gitIn(bare, ["config", "--local", "--add", `uzi-attempts.${a.branch}.entry`,
      JSON.stringify({ attemptId: a.attemptId, runId: a.runId, clonePath: a.path, state: "reclaimed" })]);
    await seedAttempt("run-b", 20, { gen: 2 });
    assert.equal(fs.existsSync(swept[0]!.dir), false, "the cap pass really ran and swept the oldest abandoned attempt");
    assert.equal(fs.readFileSync(path.join(a.path, "dirty.txt"), "utf8"), "unpreserved\n");
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed");
    assert.equal(cfgAll(`uzi-attempts.${a.branch}.entry`).filter((v) => v.includes(a.attemptId)).length, 1, "compaction ran and kept it");
  });

  it("leaves an episode journal untouched but still records reclaimed for the source", async () => {
    const e = await buildEpisode();
    const before = journalRaw(e.branch);
    assert.equal(ledgerState(e.branch, e.successorAttempt), "live");
    assert.equal(await git.retainStoppedAttempt(bare, e.branch, { runId: e.runId, clonePath: e.successorPath, attemptId: e.successorAttempt }), "retained");
    assert.equal(journalRaw(e.branch), before, "the episode journal is not cleared");
    assert.equal(ledgerState(e.branch, e.successorAttempt), "reclaimed");
  });

  it("writes the ledger even when the journal names another source, and keeps that journal", async () => {
    const a = await seedAttempt("run-a", 1);
    const b = plantAttempt(a.branch, a.key, attemptIdAt(2, 2), "run-a", "live");
    const bId = attemptIdAt(2, 2);
    gitIn(bare, ["config", "--local", `uzi-recovery.${a.branch}.clone`, JSON.stringify({ runId: "run-a", clonePath: b.dir, attemptId: bId })]);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed", "an unjournaled live entry would be a sweep candidate");
    assert.equal(JSON.parse(journalRaw(a.branch)).attemptId, bId, "the other source's journal is not touched");
  });

  it("returns unknown and writes nothing for an unattributable or unsafe source", async () => {
    const a = await seedAttempt("run-a", 1);
    const before = configBytes();
    const cases: Array<[string, () => Promise<"retained" | "unknown">]> = [
      ["wrong run", () => git.retainStoppedAttempt(bare, a.branch, { ...a, runId: "run-other" })],
      ["wrong path", () => git.retainStoppedAttempt(bare, a.branch, { ...a, clonePath: `${a.path}x` })],
      ["unknown attempt", () => git.retainStoppedAttempt(bare, a.branch, { ...a, attemptId: attemptIdAt(99) })],
      ["other branch", () => git.retainStoppedAttempt(bare, "agent/issue-1", a)],
    ];
    for (const [label, call] of cases) {
      assert.equal(await call(), "unknown", label);
      assert.deepEqual(configBytes(), before, `${label}: nothing written`);
    }
    // A replaced path: a symlink where the clone was, and a clone without its .git.
    const real = `${a.path}.moved`;
    fs.renameSync(a.path, real);
    fs.symlinkSync(real, a.path);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "unknown", "symlink");
    fs.unlinkSync(a.path);
    fs.renameSync(real, a.path);
    fs.renameSync(path.join(a.path, ".git"), path.join(a.path, ".git-gone"));
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "unknown", "no .git");
    assert.deepEqual(configBytes(), before);
    assert.ok(fs.existsSync(path.join(a.path, "README.md")));
  });

  it("a persistence failure keeps the source and the journal (unknown)", async () => {
    const a = await seedAttempt("run-a", 1);
    const seam = git as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const real = seam.runGit.bind(git);
    seam.runGit = async (cwd, args, ...rest) => {
      if (args.includes("--add") && args.some((x) => x.includes("uzi-attempts."))) throw new Error("ENOSPC: injected");
      return real(cwd, args, ...rest);
    };
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "unknown");
    assert.equal(ledgerState(a.branch, a.attemptId), "live");
    assert.equal(JSON.parse(journalRaw(a.branch)).attemptId, a.attemptId, "the journal still protects the source");
    seam.runGit = async (cwd, args, ...rest) => {
      if (args.includes(`uzi-recovery.${a.branch}.clone`) && args.at(-1) === "") throw new Error("injected clear failure");
      return real(cwd, args, ...rest);
    };
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "unknown", "a failed journal clear is not reported retained");
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed", "the ledger never goes backwards");
    assert.ok(fs.existsSync(path.join(a.path, "README.md")));
    assert.equal(JSON.parse(journalRaw(a.branch)).attemptId, a.attemptId);
  });

  it("with the residue quarantine latched it writes nothing and the journal keeps protecting the source", async () => {
    const a = await seedAttempt("run-a", 1);
    const before = configBytes();
    latchResidueQuarantine({ cause: "test", site: "test" });
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    assert.deepEqual(configBytes(), before);
    assert.equal(ledgerState(a.branch, a.attemptId), "live");
  });
});

describe("markStoppedCanonical and the marker refusal", () => {
  async function marked(generation: number | null = 3) {
    const c = await seedCanonical("run-c");
    fs.writeFileSync(path.join(c.path, "dirty.txt"), "unpreserved\n");
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation, reason: "stale_claim" }), "retained");
    return c;
  }

  it("writes one exact-shape marker bound to the journal digest and keeps the journal", async () => {
    const c = await marked(3);
    const values = cfgAll(`uzi-stopped.${c.runId}.entry`);
    assert.equal(values.length, 1);
    const m = JSON.parse(values[0]!);
    assert.deepEqual(Object.keys(m).sort(), ["at", "branch", "clonePath", "generation", "journalDigest", "key", "reason", "runId", "version"]);
    assert.equal(m.generation, 3);
    assert.equal(m.clonePath, c.path);
    assert.match(m.journalDigest, /^[0-9a-f]{64}$/);
    assert.notEqual(journalRaw(c.branch), "", "the plain journal is kept");
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 3, reason: "stale_claim" }), "retained");
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 1, "idempotent");
  });

  it("discovery, the canonical seed and the attempt seed throw RetainedStoppedSourceError and touch nothing", async () => {
    const c = await marked(3);
    const config = configBytes();
    const calls: Array<[string, () => Promise<unknown>]> = [
      ["discoverRetainedRecovery", () => git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, c.runId)],
      ["canonical seed, same run", () => git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, c.runId)],
      ["canonical seed, another run", () => git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, "run-other")],
      ["attempt seed", () => git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, c.runId, false, undefined, seedOpts(attemptIdAt(5, 4)))],
    ];
    for (const [label, call] of calls) {
      await assert.rejects(call(), (err: unknown) => {
        assert.ok(err instanceof RetainedStoppedSourceError, label);
        assert.ok(err instanceof RunResidueBlockedError, `${label}: follows the residue-blocked handling`);
        assert.match(err.message, /^worker_residue_blocked: /);
        assert.ok(err.message.includes(c.runId) && err.message.includes("generation 3") && err.message.includes(path.basename(c.path)), err.message);
        assert.ok(err.detail.length <= 160);
        return true;
      }, label);
    }
    assert.deepEqual(configBytes(), config, "the marker, journal and ledger are unchanged");
    assert.equal(fs.readFileSync(path.join(c.path, "dirty.txt"), "utf8"), "unpreserved\n");
  });

  it("names a missing generation as none", async () => {
    const c = await marked(null);
    await assert.rejects(git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, c.runId), /generation none/);
  });

  it("refuses without writing for a successor journal, an episode journal, a replaced path or a wrong key", async () => {
    const c = await seedCanonical("run-c");
    const before = configBytes();
    const call = (over: Record<string, unknown> = {}, key = c.key) =>
      git.markStoppedCanonical(bare, c.branch, key, { runId: c.runId, clonePath: c.path, generation: 1, reason: "stale_claim", ...over });
    assert.equal(await call({ runId: "run-other" }), "unknown", "another run's identity");
    assert.equal(await call({}, "issue-1"), "unknown", "a key whose canonical path is not this one");
    assert.equal(await call({ clonePath: `${c.path}.attempt-${attemptIdAt(1)}` }), "unknown", "an attempt path");
    assert.equal(await call({ generation: -1 }), "unknown");
    assert.deepEqual(configBytes(), before);
    // A successor journal replaced the plain one.
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, JSON.stringify({ runId: "run-new", clonePath: c.path })]);
    assert.equal(await call(), "unknown");
    // An episode-shaped journal.
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, JSON.stringify({ runId: c.runId, clonePath: c.path, retainedSources: [{ runId: c.runId, clonePath: c.path }] })]);
    assert.equal(await call(), "unknown");
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, JSON.stringify({ runId: c.runId, clonePath: c.path })]);
    // A replaced path.
    fs.renameSync(c.path, `${c.path}.moved`);
    fs.symlinkSync(`${c.path}.moved`, c.path);
    assert.equal(await call(), "unknown");
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 0);
  });

  for (const [label, mutate] of [
    ["not json", (_m: Record<string, unknown>) => "{"],
    ["an extra key", (m: Record<string, unknown>) => JSON.stringify({ ...m, extra: 1 })],
    ["a missing key", (m: Record<string, unknown>) => JSON.stringify({ ...m, reason: undefined })],
    ["an attempt path", (m: Record<string, unknown>) => JSON.stringify({ ...m, clonePath: `${String(m.clonePath)}.attempt-${attemptIdAt(1)}` })],
    ["a bad digest", (m: Record<string, unknown>) => JSON.stringify({ ...m, journalDigest: "xyz" })],
    ["an unsafe key", (m: Record<string, unknown>) => JSON.stringify({ ...m, key: "a/b" })],
    ["an invalid branch", (m: Record<string, unknown>) => JSON.stringify({ ...m, branch: "bad..branch" })],
    ["an oversized value", (m: Record<string, unknown>) => JSON.stringify({ ...m, reason: "x".repeat(5000) })],
    ["another run's identity", (m: Record<string, unknown>) => JSON.stringify({ ...m, runId: "run-other" })],
  ] as const) {
    it(`a malformed marker (${label}) refuses reuse, is kept, and blocks a new marker`, async () => {
      const c = await seedCanonical("run-c");
      const good = { version: 1, runId: c.runId, generation: 1, branch: c.branch, key: c.key, clonePath: c.path, journalDigest: "0".repeat(64), reason: "stale_claim", at: new Date().toISOString() };
      const bad = mutate(good);
      gitIn(bare, ["config", "--local", "--add", `uzi-stopped.${c.runId}.entry`, bad]);
      const before = configBytes();
      await assert.rejects(git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, c.runId), RetainedStoppedSourceError);
      await assert.rejects(git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, c.runId), RetainedStoppedSourceError);
      assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 1, reason: "stale_claim" }), "unknown");
      assert.deepEqual(configBytes(), before, "the malformed marker is kept verbatim and nothing is added");
      assert.ok(fs.existsSync(path.join(c.path, "README.md")));
    });
  }

  it("more than 64 markers for a run are overflow: unknown, never truncated, and reuse is refused", async () => {
    const c = await seedCanonical("run-c");
    for (let i = 0; i < 65; i++) gitIn(bare, ["config", "--local", "--add", `uzi-stopped.${c.runId}.entry`, `{"n":${i}}`]);
    const before = configBytes();
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 1, reason: "stale_claim" }), "unknown");
    await assert.rejects(git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, c.runId), RetainedStoppedSourceError);
    assert.deepEqual(configBytes(), before);
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 65, "nothing truncated");
  });

  it("a journal whose bytes changed no longer matches the marker digest: reuse is still refused and discard is not offered", async () => {
    const c = await marked(2);
    // The same logical source, different bytes.
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, JSON.stringify({ clonePath: c.path, runId: c.runId })]);
    await assert.rejects(git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, c.runId), RetainedStoppedSourceError);
    const ev = await inspectOne(selectorOf(c, 2));
    assert.equal(ev.markerState, "mismatch");
    assert.equal(ev.disposition?.kind, "refused");
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 1, "the marker is kept");
  });

  it("a persistence failure writes no marker (unknown) and keeps the journal", async () => {
    const c = await seedCanonical("run-c");
    const seam = git as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const real = seam.runGit.bind(git);
    seam.runGit = async (cwd, args, ...rest) => {
      if (args.includes("--add") && args.some((x) => x.startsWith("uzi-stopped."))) throw new Error("ENOSPC: injected");
      return real(cwd, args, ...rest);
    };
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 1, reason: "stale_claim" }), "unknown");
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 0);
    assert.notEqual(journalRaw(c.branch), "");
  });

  it("a stale marker (journal gone, path gone) protects nothing and is ignored by the seed", async () => {
    const c = await marked(1);
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, ""]);
    fs.rmSync(c.path, { recursive: true, force: true });
    const reseeded = await git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, "run-next");
    assert.ok(fs.existsSync(reseeded.path));
  });
});

describe("verifyStoppedSourceCovered", () => {
  /** A real attempt clone with one captured commit, anchored in the bare as a verified capture. */
  async function captured() {
    const a = await seedAttempt("run-v", 1);
    fs.writeFileSync(path.join(a.path, "work.txt"), "captured\n");
    gitIn(a.path, ["add", "work.txt"]);
    gitIn(a.path, ["commit", "-m", "milestone"]);
    await fixtureFetchTracking(git, bare, a.path, a.branch, a.runId, 1);
    const head = gitIn(a.path, ["rev-parse", "HEAD"]);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-runner/${a.branch}`]), head);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/${a.runId}/${head}`]), head, "the fetch pinned the head as owed");
    return { a, head };
  }
  const verify = (a: Attempt, head: string | undefined, quiesce: (p: string[]) => Promise<boolean> = quiet, generation = 1) =>
    git.verifyStoppedSourceCovered(bare, a.branch, a.key, { runId: a.runId, clonePath: a.path, attemptId: a.attemptId }, head, { runId: a.runId, generation }, quiesce);

  it("is covered for an unchanged, anchored source, and asks for quiescence before and after", async () => {
    const { a, head } = await captured();
    const calls: string[][] = [];
    assert.equal(await verify(a, head, async (p) => { calls.push(p); return true; }), "covered");
    assert.deepEqual(calls, [[a.path], [a.path]]);
  });

  it("is covered through an owed pin alone when the shared tracking ref has moved on", async () => {
    const { a, head } = await captured();
    const other = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    gitIn(bare, ["update-ref", `refs/uzi-runner/${a.branch}`, other]);
    assert.equal(await verify(a, head), "covered");
  });

  it("is not covered when the tracking ref moved and no pin anchors the head", async () => {
    const { a, head } = await captured();
    gitIn(bare, ["update-ref", `refs/uzi-runner/${a.branch}`, gitIn(bare, ["rev-parse", "refs/remotes/origin/main"])]);
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${a.runId}/${head}`]);
    assert.equal(await verify(a, head), "not_covered");
  });

  it("is not covered when tracking ownership no longer names the run and no pin anchors the head", async () => {
    const { a, head } = await captured();
    assert.equal(await verify(a, head, quiet, 2), "covered", "the pin still anchors a different generation's claim");
    const ownerKey = `uzi-trackowner.${a.branch}.owner`;
    gitIn(bare, ["config", "--local", "--replace-all", ownerKey, "run-thief"]);
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${a.runId}/${head}`]);
    assert.equal(await verify(a, head), "not_covered");
  });

  it("is not covered once the generation differs from the committed ownership and no pin remains", async () => {
    const { a, head } = await captured();
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${a.runId}/${head}`]);
    assert.equal(await verify(a, head, quiet, 1), "covered");
    assert.equal(await verify(a, head, quiet, 2), "not_covered");
  });

  it("is not covered when HEAD moved, a tracked file is dirty or an untracked file appeared", async () => {
    const { a, head } = await captured();
    fs.writeFileSync(path.join(a.path, "untracked.txt"), "new\n");
    assert.equal(await verify(a, head), "not_covered", "new untracked file");
    fs.rmSync(path.join(a.path, "untracked.txt"));
    assert.equal(await verify(a, head), "covered");
    fs.appendFileSync(path.join(a.path, "work.txt"), "more\n");
    assert.equal(await verify(a, head), "not_covered", "dirty tracked file");
    gitIn(a.path, ["checkout", "--", "work.txt"]);
    assert.equal(await verify(a, head), "covered");
    fs.writeFileSync(path.join(a.path, "later.txt"), "later\n");
    gitIn(a.path, ["add", "later.txt"]);
    gitIn(a.path, ["commit", "-m", "later"]);
    assert.equal(await verify(a, head), "not_covered", "HEAD moved past the captured head");
  });

  it("is not covered for a missing or replaced path, a blocked or throwing proof, or a degraded capture", async () => {
    const { a, head } = await captured();
    assert.equal(await verify(a, head, async () => false), "not_covered", "blocked proof before");
    let n = 0;
    assert.equal(await verify(a, head, async () => ++n < 2), "not_covered", "blocked proof after the reads");
    assert.equal(await verify(a, head, async () => { throw new Error("proof failed"); }), "not_covered");
    assert.equal(await verify(a, undefined), "not_covered", "no captured head");
    assert.equal(await verify(a, "not-a-sha"), "not_covered");
    assert.equal(await verify({ ...a, runId: "run-other" }, head), "not_covered", "owner mismatch");
    const real = `${a.path}.moved`;
    fs.renameSync(a.path, real);
    assert.equal(await verify(a, head), "not_covered", "missing path");
    fs.symlinkSync(real, a.path);
    assert.equal(await verify(a, head), "not_covered", "replaced by a symlink");
    fs.unlinkSync(a.path);
    fs.renameSync(real, a.path);
    assert.equal(await verify(a, head), "covered");
  });

  it("is not covered once the journal no longer names the source (already retained) or a marker exists", async () => {
    const { a, head } = await captured();
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    assert.equal(await verify(a, head), "not_covered");
    const c = await seedCanonical("run-m", 2650);
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 1, reason: "stale_claim" }), "retained");
    assert.equal(await git.verifyStoppedSourceCovered(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path }, head, { runId: c.runId, generation: 1 }, quiet), "not_covered");
  });
});

describe("inspect and the plain discard", () => {
  it("reports a reclaimed attempt content-free, then reclaims it with no hold and retires the ledger", async () => {
    const a = await seedAttempt("run-a", 1);
    fs.writeFileSync(path.join(a.path, "dirty.txt"), "x".repeat(20_000));
    const skills = path.join(path.dirname(a.path), `.uzi-skills-${path.basename(a.path)}`);
    fs.mkdirSync(skills);
    fs.writeFileSync(path.join(skills, "plugin.json"), "{}");
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    assert.deepEqual([ev.kind, ev.runId, ev.generation, ev.attempt, ev.ledgerState, ev.journalState, ev.presence],
      ["attempt", "run-a", 1, a.attemptId, "reclaimed", "none", "present"]);
    assert.match(ev.descriptor, /^[0-9a-f]{64}$/);
    assert.equal(ev.bytesState, "measured");
    assert.ok(ev.allocatedBytes !== null && ev.allocatedBytes >= 20_000, `allocated ${ev.allocatedBytes}`);
    assert.deepEqual(ev.disposition, { kind: "local_discard" });
    assert.equal(JSON.stringify(ev).includes("xxxx"), false, "no file content");
    const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.deepEqual(result, { kind: "done", sources: [{ clonePath: a.path, outcome: "removed" }], metadata: "retired" });
    assert.equal(fs.existsSync(a.path), false);
    assert.equal(fs.existsSync(skills), false, "the skills sibling goes with it");
    assert.equal(ledgerState(a.branch, a.attemptId), "retired");
    assert.equal((await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive })).sources.length, 0);
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor))).kind, "refused", "nothing left to discard");
  });

  it("reclaims an abandoned attempt and a gx attempt (generation none)", async () => {
    const planted = plantAttempt("agent/issue-3001", "issue-3001", attemptIdAt(3), "run-ab", "abandoned");
    const ab = await inspectOne({ runId: "run-ab", generation: 1, attempt: attemptIdAt(3) });
    assert.equal(ab.ledgerState, "abandoned");
    assert.equal((await git.discardStoppedSource(discardOpts({ runId: "run-ab", generation: 1, attempt: attemptIdAt(3) }, ab.descriptor))).kind, "done");
    assert.equal(fs.existsSync(planted.dir), false);

    const gxId = attemptIdAt(4, undefined);
    const gx = plantAttempt("agent/issue-3001", "issue-3001", gxId, "run-gx", "reclaimed");
    const none: RetainedSourceSelector = { runId: "run-gx", generation: "none", attempt: gxId };
    const numeric = await git.inspectRetainedSources({ selector: { ...none, generation: 0 }, holds: NO_HOLDS, isLivePath: notLive, barePath: bare });
    assert.equal(numeric.sources.length, 0, "generation 0 is never substituted for none");
    const ev = await inspectOne(none);
    assert.equal(ev.generation, "none");
    assert.equal((await git.discardStoppedSource(discardOpts({ ...none, generation: 0 }, ev.descriptor))).kind, "refused");
    assert.ok(fs.existsSync(gx.dir));
    assert.equal((await git.discardStoppedSource(discardOpts(none, ev.descriptor))).kind, "done");
    assert.equal(fs.existsSync(gx.dir), false);
  });

  it("reclaims a marked canonical source with its generation or none, clearing journal and marker, and the key seeds again", async () => {
    for (const generation of [4, null] as const) {
      const run = generation === null ? "run-none" : "run-four";
      const c = await seedCanonical(run, generation === null ? 2601 : 2602);
      fs.writeFileSync(path.join(c.path, "dirty.txt"), "unpreserved\n");
      assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation, reason: "claim_fence" }), "retained");
      const sel: RetainedSourceSelector = { runId: run, generation: generation ?? "none", attempt: "canonical" };
      const wrong: RetainedSourceSelector = { ...sel, generation: generation === null ? 4 : "none" };
      assert.equal((await git.inspectRetainedSources({ selector: wrong, holds: NO_HOLDS, isLivePath: notLive, barePath: bare })).sources.length, 0, "a different generation is a different source");
      const ev = await inspectOne(sel);
      assert.equal(ev.kind, "canonical");
      assert.equal(ev.markerState, "bound");
      assert.equal(ev.journalState, "plain");
      assert.deepEqual(ev.disposition, { kind: "local_discard" });
      const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
      assert.equal(result.kind, "done");
      assert.equal(fs.existsSync(c.path), false);
      assert.equal(journalRaw(c.branch), "", "the journal is cleared with the source");
      assert.equal(cfgAll(`uzi-stopped.${run}.entry`).length, 0, "and so is the marker");
      const reseed = await git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, "run-next");
      assert.ok(fs.existsSync(reseed.path), "the key is usable again");
    }
  });

  it("an open server hold of the generation is the owner's to discard; other generations and a failed read behave", async () => {
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const hold = (generation: number): RetainedHoldsSnapshot => ({ state: "read", holds: [{ id: "hold-77", generation }] });
    const ev = await inspectOne(sel, hold(1));
    assert.deepEqual(ev.disposition, { kind: "owner_hold_discard", holdIds: ["hold-77"] });
    assert.deepEqual(ev.holds, { state: "read", ids: ["hold-77"] });
    const refused = await git.discardStoppedSource(discardOpts(sel, ev.descriptor, { holds: hold(1) }));
    assert.equal(refused.kind, "refused");
    assert.ok(fs.existsSync(a.path));
    const unknown = await git.discardStoppedSource(discardOpts(sel, ev.descriptor, { holds: { state: "unknown" } }));
    assert.equal(unknown.kind, "refused");
    assert.ok(fs.existsSync(a.path));
    assert.equal((await inspectOne(sel, { state: "unknown" })).disposition?.kind, "refused");
    assert.equal((await inspectOne(sel, hold(9))).disposition?.kind, "local_discard", "another generation's hold is not this source's");
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor, { holds: hold(9) }))).kind, "done");
  });

  it("refuses unknown attribution without deleting", async () => {
    const orphan = plantAttempt("agent/issue-3001", "issue-3001", attemptIdAt(6), "", "reclaimed");
    const unknownKey = path.join(path.dirname(orphan.dir), "weird");
    fs.mkdirSync(unknownKey);
    gitIn(bare, ["config", "--local", "--add", "uzi-attempts.agent/issue-3001.entry",
      JSON.stringify({ attemptId: attemptIdAt(7), runId: "run-x", clonePath: unknownKey, state: "reclaimed" })]);
    const all = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    const weird = all.sources.find((s) => s.sources[0]?.clonePath === unknownKey);
    assert.ok(weird, "listed, not hidden");
    assert.ok(weird.consumers.includes("blocker:attribution_unknown"));
    const sel: RetainedSourceSelector = { runId: "run-x", generation: 1, attempt: attemptIdAt(7) };
    const result = await git.discardStoppedSource(discardOpts(sel, weird.descriptor));
    assert.equal(result.kind, "refused");
    assert.ok(fs.existsSync(unknownKey));
    const runless = all.sources.find((s) => s.sources[0]?.clonePath === orphan.dir);
    assert.ok(runless?.consumers.includes("blocker:attribution_unknown"), "a run-less ledger entry is unattributed");
    const noRun = await git.discardStoppedSource(discardOpts({ runId: "", generation: 1, attempt: attemptIdAt(6) }, runless!.descriptor));
    assert.equal(noRun.kind, "refused");
    assert.ok(fs.existsSync(orphan.dir));
  });

  it("refuses a live source, a live successor attempt, a path named by a journal and a shared owed pin", async () => {
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    const live = await git.discardStoppedSource(discardOpts(sel, ev.descriptor, { isLivePath: (p: string) => p === a.path }));
    assert.deepEqual([live.kind, live.kind === "refused" && live.consumers.some((c) => c.startsWith("live:"))], ["refused", true]);
    const next = await seedAttempt("run-a", 2, { gen: 2 });
    const successor = await git.discardStoppedSource(discardOpts(sel, ev.descriptor, { isLivePath: (p: string) => p === next.path }));
    assert.deepEqual([successor.kind, successor.kind === "refused" && successor.consumers.some((c) => c.startsWith("live_successor:"))], ["refused", true]);
    // Another branch's journal names the path.
    gitIn(bare, ["config", "--local", "uzi-recovery.agent/other.clone", JSON.stringify({ runId: "run-other", clonePath: a.path })]);
    const named = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.deepEqual([named.kind, named.kind === "refused" && named.consumers.some((c) => c.startsWith("named_by_journal:"))], ["refused", true]);
    gitIn(bare, ["config", "--local", "uzi-recovery.agent/other.clone", ""]);
    // A shared owed pin: another run pins the same sha as this run.
    const sha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    gitIn(bare, ["update-ref", `refs/uzi-owed/run-a/${sha}`, sha]);
    gitIn(bare, ["update-ref", `refs/uzi-owed/run-other/${sha}`, sha]);
    const shared = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.deepEqual([shared.kind, shared.kind === "refused" && shared.consumers.includes("shared_owed_pin")], ["refused", true]);
    assert.ok(fs.existsSync(a.path));
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/run-other/${sha}`]);
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor))).kind, "done", "own owed pins alone are not a refusal");
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/run-a/${sha}`]), sha, "owed evidence is kept");
  });

  it("refuses a changed descriptor, a replaced path and a symlink without deleting through it", async () => {
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    assert.equal((await git.discardStoppedSource(discardOpts(sel, "0".repeat(64)))).kind, "refused", "wrong confirm");
    assert.equal((await git.discardStoppedSource(discardOpts(sel, "short"))).kind, "refused", "malformed confirm");
    // The directory is replaced by an identical-looking one: a new dev:ino.
    const moved = `${a.path}.moved`;
    fs.renameSync(a.path, moved);
    fs.mkdirSync(path.join(a.path, ".git"), { recursive: true });
    const replaced = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.deepEqual([replaced.kind, replaced.kind === "refused" && replaced.reason], ["refused", "descriptor_changed"]);
    fs.rmSync(a.path, { recursive: true });
    // A symlink to a victim directory.
    const victim = path.join(fx.dataDir, "victim");
    fs.mkdirSync(victim);
    fs.writeFileSync(path.join(victim, "keep.txt"), "keep");
    fs.symlinkSync(victim, a.path);
    const ev2 = await inspectOne(sel);
    assert.equal(ev2.presence, "unsafe");
    const linked = await git.discardStoppedSource(discardOpts(sel, ev2.descriptor));
    assert.equal(linked.kind, "refused");
    assert.equal(fs.readFileSync(path.join(victim, "keep.txt"), "utf8"), "keep");
    assert.ok(fs.lstatSync(a.path).isSymbolicLink());
    assert.ok(fs.existsSync(moved));
  });

  it("refuses under the residue quarantine and when the quiescence proof fails; the deadline keeps the source", async () => {
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    const busy = await git.discardStoppedSource(discardOpts(sel, ev.descriptor, { quiesce: async () => false }));
    assert.deepEqual([busy.kind, busy.kind === "refused" && busy.reason], ["refused", "not_quiescent"]);
    assert.ok(fs.existsSync(a.path));
    // The operation is cancelled while the quiescence proof runs: nothing is deleted after it.
    const controller = new AbortController();
    const slow = await git.discardStoppedSource(discardOpts(sel, ev.descriptor, {
      signal: controller.signal,
      quiesce: async () => { controller.abort(new Error("operation deadline")); return true; },
    }));
    assert.equal(slow.kind, "unknown", `an interrupted operation is unknown, not success: ${JSON.stringify(slow)}`);
    assert.ok(fs.existsSync(a.path));
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed", "metadata not retired");
    latchResidueQuarantine({ cause: "test", site: "test" });
    const latched = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.equal(latched.kind, "refused");
    assert.ok(fs.existsSync(a.path));
  });

  it("a runner-uid delete failure keeps the source and the ledger (incomplete)", async () => {
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
      retentionDelete: { split: true, run: async () => { throw new Error("rm: permission denied"); } },
    }));
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.deepEqual(result, { kind: "incomplete", sources: [{ clonePath: a.path, outcome: "retained" }], metadata: "kept" });
    assert.ok(fs.existsSync(a.path));
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed");
  });

  it("reports an absent emptyDir source as missing, then retires its metadata on discard", async () => {
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    fs.rmSync(a.path, { recursive: true });
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    assert.equal(ev.presence, "missing");
    assert.equal(ev.allocatedBytes, 0);
    const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.deepEqual(result, { kind: "done", sources: [{ clonePath: a.path, outcome: "missing" }], metadata: "retired" });
    assert.equal(ledgerState(a.branch, a.attemptId), "retired");
  });

  it("measures within one budget: an exhausted clock is partial with unknown bytes, never zero", async () => {
    const a = await seedAttempt("run-a", 1);
    fs.mkdirSync(path.join(a.path, "deep", "er"), { recursive: true });
    for (let i = 0; i < 20; i++) fs.writeFileSync(path.join(a.path, "deep", "er", `f${i}`), "x");
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    let t = 0;
    const r = await git.inspectRetainedSources({
      selector: selectorOf(a, 1), holds: NO_HOLDS, isLivePath: notLive, barePath: bare,
      now: () => (t += 10), budgetMs: 100,
    });
    assert.equal(r.partial, true);
    const ev = r.sources[0]!;
    assert.equal(ev.bytesState, "unknown");
    assert.equal(ev.allocatedBytes, null, "an unmeasured source is never reported as zero bytes");
    assert.equal(ev.sources[0]!.allocatedBytes, null);
  });

  it("caps an aggregate view at 64 descriptors and reports it partial", async () => {
    for (let i = 0; i < RETAINED_MAX_DESCRIPTORS + 2; i++) plantAttempt("agent/issue-3001", "issue-3001", attemptIdAt(100 + i), `run-${i}`, "abandoned");
    const r = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    assert.equal(r.sources.length, RETAINED_MAX_DESCRIPTORS);
    assert.equal(r.partial, true);
    assert.ok(r.sources.every((s) => s.disposition === undefined), "no disposition without a selector and its run's holds");
  });

  it("does not follow a symlink while measuring", async () => {
    const a = await seedAttempt("run-a", 1);
    const big = path.join(fx.dataDir, "big");
    fs.mkdirSync(big);
    fs.writeFileSync(path.join(big, "huge"), Buffer.alloc(4 * 1024 * 1024, 1));
    fs.symlinkSync(big, path.join(a.path, "link"));
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const ev = await inspectOne(selectorOf(a, 1));
    assert.ok(ev.allocatedBytes !== null && ev.allocatedBytes < 4 * 1024 * 1024, `${ev.allocatedBytes}`);
  });
});

// ─── protected / episode sources ───────────────────────────────────────────────────────────

const EP_RUN = "run-2700";
const aid = (n: number): string => `20261008T171401Z-g${n}-0123456789abcdef`;

/** A stopped predecessor-capture episode: a canonical predecessor with a verified capture and a
 *  ready attempt successor (what a fenced recoverRetainedClaim flight leaves behind). */
async function buildEpisode(opts: { descriptor?: boolean } = {}) {
  const iid = 2700;
  const branch = `agent/issue-${iid}`;
  const key = `issue-${iid}`;
  const source = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, EP_RUN);
  await git.markRecoveryCapture(bare, source.path, branch, EP_RUN);
  const src = { runId: EP_RUN, clonePath: source.path };
  const reservation = await git.reserveRecoveryIteration(bare, branch, key, src);
  fs.writeFileSync(path.join(source.path, "work.txt"), "retained work\n");
  gitIn(source.path, ["add", "work.txt"]);
  gitIn(source.path, ["commit", "-m", "milestone"]);
  await fixtureFetchTracking(git, bare, source.path, branch, EP_RUN);
  const tip = gitIn(source.path, ["rev-parse", "HEAD"]);
  await git.recordRecoveryCapture(bare, branch, key, src, reservation.attempts, tip);
  const successor = await git.prepareRecoverySuccessor(bare, branch, key, src, reservation.attempts, aid(2));
  if (opts.descriptor) {
    const snapshot = await git.terminalRetainedSnapshot(bare, branch, key, JSON.parse(journalRaw(branch)));
    await git.detachTerminalRetained(bare, branch, key, snapshot!.journal);
  }
  return { branch, key, runId: EP_RUN, tip, sourcePath: source.path, successorPath: successor.path, successorAttempt: aid(2) };
}

describe("protected-source disposition", () => {
  const epSel = (): RetainedSourceSelector => ({ runId: EP_RUN, generation: 2, attempt: aid(2) });
  const pinRef = (e: { tip: string }): string => `refs/uzi-recovery-episode/${EP_RUN}/${e.tip}`;

  for (const descriptor of [false, true]) {
    it(`no open hold and no local records: inspect names the consumers and local_protected_discard; discard removes everything (${descriptor ? "descriptor" : "active episode"})`, async () => {
      const e = await buildEpisode({ descriptor });
      const ev = await inspectOne(epSel());
      assert.equal(ev.kind, "protected");
      assert.equal(ev.journalState, descriptor ? "descriptor" : "episode");
      assert.equal(ev.sources.length, 2, "predecessor and successor");
      assert.deepEqual(ev.sources.map((s) => s.clonePath).sort(), [e.sourcePath, e.successorPath].sort());
      assert.ok(ev.consumers.includes("episode_pins:1"), ev.consumers.join());
      assert.ok(ev.consumers.some((c) => c.startsWith("owed_pins:")));
      assert.deepEqual(ev.localRecords, []);
      assert.deepEqual(ev.holds, { state: "read", ids: [] });
      assert.deepEqual(ev.disposition, { kind: "local_protected_discard" });
      assert.equal(gitIn(bare, ["rev-parse", pinRef(e)]), e.tip);
      const result = await git.discardProtectedSource(discardOpts(epSel(), ev.descriptor));
      assert.equal(result.kind, "done", JSON.stringify(result));
      assert.equal(fs.existsSync(e.sourcePath), false);
      assert.equal(fs.existsSync(e.successorPath), false);
      assert.throws(() => gitIn(bare, ["rev-parse", "--verify", pinRef(e)]), "the episode pin is gone");
      assert.equal(cfgAll(`uzi-retained.${EP_RUN}.journal`).length, 0, "the descriptor is gone");
      assert.equal(journalRaw(e.branch), "", "the active journal is cleared");
      assert.equal(ledgerState(e.branch, e.successorAttempt), "retired");
      assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/${EP_RUN}/${e.tip}`]), e.tip, "owed evidence is not the descriptor's to delete");
      assert.equal((await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive })).sources.length, 0);
    });
  }

  it("an open server hold with absent local records is the owner's to discard, by the server's id", async () => {
    const e = await buildEpisode();
    const holds: RetainedHoldsSnapshot = { state: "read", holds: [{ id: "hold-from-server", generation: 1 }] };
    const ev = await inspectOne(epSel(), holds);
    assert.deepEqual(ev.disposition, { kind: "owner_hold_discard", holdIds: ["hold-from-server"] });
    const result = await git.discardProtectedSource(discardOpts(epSel(), ev.descriptor, { holds }));
    assert.equal(result.kind, "refused");
    assert.ok(fs.existsSync(e.sourcePath) && fs.existsSync(e.successorPath));
    assert.equal(gitIn(bare, ["rev-parse", pinRef(e)]), e.tip);
  });

  it("a released hold with surviving recovery and settlement records names each and deletes nothing", async () => {
    const e = await buildEpisode();
    fs.mkdirSync(path.join(fx.dataDir, "recovery", EP_RUN), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery", EP_RUN, "capture-1.json"), JSON.stringify({ generation: 1 }));
    fs.mkdirSync(path.join(fx.dataDir, "recovery-settlement", EP_RUN), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery-settlement", EP_RUN, "hold-9.json"), JSON.stringify({ predecessorGeneration: 1 }));
    const ev = await inspectOne(epSel(), NO_HOLDS);
    assert.equal(ev.disposition?.kind, "resolve_local_record");
    assert.deepEqual(ev.localRecords.map((r) => [r.kind, r.generation, r.name]), [
      ["recovery", 1, "capture-1.json"], ["settlement", 1, "hold-9.json"],
    ]);
    const result = await git.discardProtectedSource(discardOpts(epSel(), ev.descriptor));
    assert.equal(result.kind, "refused");
    assert.ok(fs.existsSync(e.sourcePath) && fs.existsSync(e.successorPath));
    assert.equal(gitIn(bare, ["rev-parse", pinRef(e)]), e.tip);
    assert.equal(cfgAll(`uzi-recovery.${e.branch}.clone`).length, 1);
  });

  it("terminal protection is a named local record too", async () => {
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ terminalRecordProtection: async () => true }));
    const e = await buildEpisode();
    const ev = await inspectOne(epSel());
    assert.deepEqual(ev.disposition, { kind: "resolve_local_record", records: [
      { kind: "terminal", generation: null, name: "terminal_protection", resolution: "terminal record retirement" },
    ] });
    assert.ok(fs.existsSync(e.sourcePath));
  });

  it("a failed, oversized or malformed holds read is unknown and refuses", async () => {
    const e = await buildEpisode();
    const ev = await inspectOne(epSel());
    for (const holds of [{ state: "unknown" } as RetainedHoldsSnapshot, { state: "read", holds: [{ id: 5, generation: "x" }] } as unknown as RetainedHoldsSnapshot]) {
      assert.equal((await inspectOne(epSel(), holds)).disposition?.kind, "refused");
      assert.equal((await git.discardProtectedSource(discardOpts(epSel(), ev.descriptor, { holds }))).kind, "refused");
    }
    assert.ok(fs.existsSync(e.sourcePath) && fs.existsSync(e.successorPath));
  });

  it("refuses a live successor, a source named by another journal, a shared owed pin, a changed digest and unknown attribution", async () => {
    const e = await buildEpisode();
    const ev = await inspectOne(epSel());
    const intact = (): void => {
      assert.ok(fs.existsSync(e.sourcePath) && fs.existsSync(e.successorPath));
      assert.equal(gitIn(bare, ["rev-parse", pinRef(e)]), e.tip);
      assert.equal(ledgerState(e.branch, e.successorAttempt), "live");
    };
    const refusedWith = async (label: string, token: string, over: Record<string, unknown> = {}, confirm = ev.descriptor) => {
      const r = await git.discardProtectedSource(discardOpts(epSel(), confirm, over));
      assert.equal(r.kind, "refused", label);
      if (r.kind === "refused" && token) assert.ok(r.consumers.some((c) => c.includes(token)) || r.reason.includes(token), `${label}: ${JSON.stringify(r)}`);
      intact();
    };
    await refusedWith("live successor", "live:", { isLivePath: (p: string) => p === e.successorPath });
    await refusedWith("live predecessor", "live:", { isLivePath: (p: string) => p === e.sourcePath });
    gitIn(bare, ["config", "--local", "uzi-recovery.agent/other.clone", JSON.stringify({ runId: "run-other", clonePath: e.sourcePath })]);
    await refusedWith("named by another journal", "named_by_other_journal");
    gitIn(bare, ["config", "--local", "uzi-recovery.agent/other.clone", ""]);
    gitIn(bare, ["update-ref", `refs/uzi-owed/run-other/${e.tip}`, e.tip]);
    await refusedWith("shared owed pin", "shared_owed_pin");
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/run-other/${e.tip}`]);
    await refusedWith("changed digest", "descriptor_changed", {}, "f".repeat(64));
    // Unknown attribution: the successor's ledger entry names another path.
    gitIn(bare, ["config", "--local", "--add", `uzi-attempts.${e.branch}.entry`, JSON.stringify({ attemptId: e.successorAttempt, runId: "run-someone-else", clonePath: e.successorPath, state: "live" })]);
    const after = await inspectOne(epSel());
    assert.equal(after.disposition?.kind, "refused");
    const r = await git.discardProtectedSource(discardOpts(epSel(), after.descriptor));
    assert.equal(r.kind, "refused");
    assert.ok(fs.existsSync(e.sourcePath) && fs.existsSync(e.successorPath));
    assert.equal(gitIn(bare, ["rev-parse", pinRef(e)]), e.tip);
  });

  it("a plain discard never takes a protected source, and the protected discard never takes a plain one", async () => {
    await buildEpisode();
    const ev = await inspectOne(epSel());
    assert.equal((await git.discardStoppedSource(discardOpts(epSel(), ev.descriptor))).kind, "refused");
    const a = await seedAttempt("run-a", 1, { iid: 2800 });
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const plain = await inspectOne(selectorOf(a, 1));
    assert.equal((await git.discardProtectedSource(discardOpts(selectorOf(a, 1), plain.descriptor))).kind, "refused");
    assert.ok(fs.existsSync(a.path));
  });

  it("enters the bare lock exactly once and holds it from quiescence through the deletes to metadata retirement", async () => {
    const e = await buildEpisode({ descriptor: true });
    const ev = await inspectOne(epSel());
    const seam = git as unknown as {
      withLock: <T>(key: string, fn: () => Promise<T>) => Promise<T>;
      deleteRetainedArtifact: (...a: unknown[]) => Promise<boolean>;
      runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string>;
    };
    let entries = 0;
    let held = false;
    const events: string[] = [];
    const realLock = seam.withLock.bind(git);
    seam.withLock = (key, fn) => {
      if (key === bare) entries++;
      return realLock(key, async () => {
        if (key === bare) held = true;
        try { return await fn(); } finally { if (key === bare) held = false; }
      });
    };
    const realDelete = seam.deleteRetainedArtifact.bind(git);
    seam.deleteRetainedArtifact = async (...args) => { events.push(`delete:${held}`); return realDelete(...args); };
    const realGit = seam.runGit.bind(git);
    seam.runGit = async (cwd, args, ...rest) => {
      if (args.includes("--unset-all") && args.some((x) => String(x).startsWith("uzi-retained."))) events.push(`descriptor:${held}`);
      if (args[0] === "update-ref" && args.includes("-d")) events.push(`pin:${held}`);
      return realGit(cwd, args, ...rest);
    };
    const result = await git.discardProtectedSource(discardOpts(epSel(), ev.descriptor, {
      quiesce: async (paths: string[]) => { events.push(`quiesce:${held}:${paths.length}`); return true; },
    }));
    assert.equal(result.kind, "done", JSON.stringify(result));
    assert.equal(entries, 1, "the bare lock is entered exactly once");
    assert.deepEqual(events.filter((x) => x.startsWith("quiesce")), ["quiesce:true:4"], "both sources and both skills siblings");
    assert.equal(events.filter((x) => x.startsWith("delete")).length, 4);
    assert.ok(events.every((x) => x.endsWith(":true") || x.includes(":true:")), events.join());
    assert.ok(events.indexOf("descriptor:true") > events.lastIndexOf("delete:true"), "metadata retires only after every delete");
    assert.ok(events.includes("pin:true"));
    assert.equal(fs.existsSync(e.sourcePath) || fs.existsSync(e.successorPath), false);
  });

  it("a partial delete failure leaves the descriptor and the remaining paths; a retry finishes", async () => {
    let failFor: string | undefined;
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
      retentionDelete: {
        split: true,
        run: async (_command, args) => {
          const target = args.at(-1)!;
          if (target === failFor) throw new Error("rm: permission denied");
          fs.rmSync(target, { recursive: true, force: true });
        },
      },
    }));
    const e = await buildEpisode({ descriptor: true });
    failFor = e.successorPath;
    const ev = await inspectOne(epSel());
    const first = await git.discardProtectedSource(discardOpts(epSel(), ev.descriptor));
    assert.equal(first.kind, "incomplete", JSON.stringify(first));
    if (first.kind === "incomplete") {
      const by = new Map(first.sources.map((s) => [s.clonePath, s.outcome]));
      assert.equal(by.get(e.successorPath), "retained");
      assert.ok([...by.values()].includes("removed") || by.get(e.sourcePath) === "retained");
      assert.equal(first.metadata, "kept");
    }
    assert.ok(fs.existsSync(e.successorPath), "the remaining path is kept");
    assert.equal(cfgAll(`uzi-retained.${EP_RUN}.journal`).length, 1, "the descriptor is kept");
    assert.equal(gitIn(bare, ["rev-parse", pinRef(e)]), e.tip, "the episode pin is kept");
    failFor = undefined;
    const again = await inspectOne(epSel());
    assert.notEqual(again.descriptor, ev.descriptor, "the descriptor reflects the partial state");
    const second = await git.discardProtectedSource(discardOpts(epSel(), again.descriptor));
    assert.equal(second.kind, "done", JSON.stringify(second));
    assert.equal(cfgAll(`uzi-retained.${EP_RUN}.journal`).length, 0);
    assert.equal(fs.existsSync(e.successorPath), false);
  });

  it("the locked core refuses when the descriptor changed since the snapshot; the public wrapper is unchanged", async () => {
    const e = await buildEpisode({ descriptor: true });
    const seam = git as unknown as {
      discardRetainedRecoveryUnderLock: (b: string, branch: string, key: string, expected: object, snap?: { descriptor: string | undefined; active: string | undefined }) => Promise<void>;
      withLock: <T>(key: string, fn: () => Promise<T>) => Promise<T>;
    };
    const journal = JSON.parse(cfgAll(`uzi-retained.${EP_RUN}.journal`)[0]!).journal;
    const expected = { runId: journal.runId, clonePath: journal.clonePath, attemptId: journal.attemptId };
    await assert.rejects(
      seam.withLock(bare, () => seam.discardRetainedRecoveryUnderLock(bare, e.branch, e.key, expected, { descriptor: "stale", active: undefined })),
      /changed since the snapshot/,
    );
    assert.equal(cfgAll(`uzi-retained.${EP_RUN}.journal`).length, 1);
    assert.throws(() => gitIn(bare, ["rev-parse", "--verify", "refs/heads/nothing"]));
    await git.discardRetainedRecovery(bare, e.branch, e.key, expected, true);
    assert.equal(cfgAll(`uzi-retained.${EP_RUN}.journal`).length, 0);
  });
});

describe("logging", () => {
  it("never logs the path contents when retention fails", async () => {
    const rec = recordingLogger();
    git = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions());
    const a = await seedAttempt("run-a", 1);
    fs.writeFileSync(path.join(a.path, "secret.txt"), "SECRET-CONTENT");
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, { ...a, runId: "run-other" }), "unknown");
    assert.equal(JSON.stringify(rec.lines).includes("SECRET-CONTENT"), false);
  });
});
