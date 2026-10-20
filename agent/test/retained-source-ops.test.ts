import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import { attemptGeneration, formatAttemptId } from "../src/attempt-path.js";
import {
  GitCache,
  RETAINED_MAX_DESCRIPTORS,
  type AttemptLedgerState,
  type AttemptSeedOptions,
  type RetainedHoldsSnapshot,
  type RetainedLifecycleSnapshot,
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
function attemptModeOn(key: string): boolean {
  return cfgAll(`uzi-attemptmode.${key}.enabled`).length === 1;
}
function configBytes(): Buffer {
  return fs.readFileSync(path.join(bare, "config"));
}
function seedOpts(attemptId: string, over: Partial<AttemptSeedOptions> = {}): AttemptSeedOptions {
  return { attemptId, isLive: notLive, beforeSeed: async () => {}, ...over };
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

/** A kept canonical source: real clone, a dirty file, and the marker written (journal cleared). */
async function markedCanonical(runId: string, iid: number, generation: number | null) {
  const c = await seedCanonical(runId, iid);
  fs.writeFileSync(path.join(c.path, "dirty.txt"), "unpreserved\n");
  const journal = journalRaw(c.branch);
  assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId, clonePath: c.path, generation, reason: "stale_claim" }), "retained");
  return { ...c, journal };
}

/** The state a crash between the marker write and the journal clear leaves: bound journal, no mode. */
function crashWindow(c: { branch: string; key: string; journal: string }): void {
  gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, c.journal]);
  gitIn(bare, ["config", "--local", "--unset-all", `uzi-attemptmode.${c.key}.enabled`]);
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

const TERMINAL: RetainedLifecycleSnapshot = { state: "terminal", status: "completed" };

async function inspectOne(sel: RetainedSourceSelector, holds: RetainedHoldsSnapshot = NO_HOLDS, isLivePath: (p: string) => boolean = notLive,
  lifecycle?: RetainedLifecycleSnapshot) {
  const r = await git.inspectRetainedSources({ selector: sel, holds, ...(lifecycle ? { lifecycle } : {}), isLivePath, barePath: bare });
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

  it("disabled automatic sweep and compactAttemptLedger keep reclaimed and abandoned attempts past the diagnostic cap", async () => {
    const rec = recordingLogger();
    git = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions());
    const a = await seedAttempt("run-a", 1);
    fs.writeFileSync(path.join(a.path, "dirty.txt"), "unpreserved\n");
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    // Five abandoned attempts exceed the diagnostic threshold of three without authorizing deletion.
    const retained = [10, 11, 12, 13, 14].map((i) => plantAttempt(a.branch, a.key, attemptIdAt(i), `old-${i}`, "abandoned"));
    // Duplicate ledger values force a real compaction on the next seed.
    gitIn(bare, ["config", "--local", "--add", `uzi-attempts.${a.branch}.entry`,
      JSON.stringify({ attemptId: a.attemptId, runId: a.runId, clonePath: a.path, state: "reclaimed" })]);
    await seedAttempt("run-b", 20, { gen: 2 });
    for (const [i, artifact] of retained.entries()) {
      assert.equal(fs.readFileSync(path.join(artifact.dir, "WORK.txt"), "utf8"), attemptIdAt(10 + i));
      assert.equal(fs.readFileSync(path.join(artifact.skills, "plugin.json"), "utf8"), "{}");
      assert.equal(ledgerState(a.branch, attemptIdAt(10 + i)), "abandoned");
    }
    assert.ok(rec.lines.some((line) => (line as { msg?: string }).msg === "runner clone retention: automatic reclaim disabled"),
      "over-threshold inventory is diagnostic only");
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
    const journal = journalRaw(c.branch);
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation, reason: "stale_claim" }), "retained");
    return { ...c, journal };
  }

  it("writes one exact-shape marker bound to the journal digest, persists the attempt mode and clears the bound journal", async () => {
    const c = await marked(3);
    const values = cfgAll(`uzi-stopped.${c.runId}.entry`);
    assert.equal(values.length, 1);
    const m = JSON.parse(values[0]!);
    assert.deepEqual(Object.keys(m).sort(), ["at", "branch", "clonePath", "generation", "journalDigest", "key", "reason", "runId", "version"]);
    assert.equal(m.generation, 3);
    assert.equal(m.clonePath, c.path);
    assert.match(m.journalDigest, /^[0-9a-f]{64}$/);
    assert.notEqual(c.journal, "", "precondition: the plain journal existed");
    assert.equal(journalRaw(c.branch), "", "the journal the digest was computed from is cleared");
    assert.equal(attemptModeOn(c.key), true, "the key now seeds fresh attempt paths");
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 3, reason: "stale_claim" }), "retained");
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 1, "idempotent");
    assert.equal(journalRaw(c.branch), "");
  });

  it("completes a crash window on a repeat: a still-bound journal is cleared and the mode persisted", async () => {
    const c = await marked(3);
    crashWindow(c);
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 3, reason: "stale_claim" }), "retained");
    assert.equal(journalRaw(c.branch), "");
    assert.equal(attemptModeOn(c.key), true);
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 1);
  });

  it("keeps the journal when a residue latch lands during the marker write, but still answers retained", async () => {
    const c = await seedCanonical("run-c");
    const journal = journalRaw(c.branch);
    const seam = git as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const real = seam.runGit.bind(git);
    seam.runGit = async (cwd, args, ...rest) => {
      const out = await real(cwd, args, ...rest);
      if (args.includes("--add") && args.some((x) => x.startsWith("uzi-stopped."))) latchResidueQuarantine({ cause: "test", site: "test" });
      return out;
    };
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 1, reason: "stale_claim" }), "retained");
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 1, "the marker is durable");
    assert.equal(journalRaw(c.branch), journal, "the latch keeps the journal");
  });

  it("clears the journal only while its raw is still the one the digest was computed from", async () => {
    const c = await seedCanonical("run-c");
    const seam = git as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const real = seam.runGit.bind(git);
    const successor = JSON.stringify({ runId: "successor", clonePath: c.path, branch: c.branch });
    seam.runGit = async (cwd, args, ...rest) => {
      const out = await real(cwd, args, ...rest);
      // A same-run rewrite lands right after the marker, before the clear.
      if (args.includes("--add") && args.some((x) => x.startsWith("uzi-stopped."))) {
        await real(cwd, ["config", "--local", `uzi-recovery.${c.branch}.clone`, successor]);
      }
      return out;
    };
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 1, reason: "stale_claim" }), "retained");
    assert.equal(journalRaw(c.branch), successor, "another journal is never cleared");
  });

  it("an identity-changed journal writes nothing at all: no marker and no attempt mode", async () => {
    const c = await seedCanonical("run-c");
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, JSON.stringify({ runId: "run-new", clonePath: c.path })]);
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 1, reason: "stale_claim" }), "unknown");
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 0);
    assert.equal(attemptModeOn(c.key), false);
  });

  it("the canonical seed refuses the kept path, discovery finds no journal, and the attempt seed gets a fresh path; the kept bytes never change", async () => {
    const c = await marked(3);
    const dirty = fs.readFileSync(path.join(c.path, "dirty.txt"), "utf8");
    assert.equal(await git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, c.runId), undefined, "no journal to adopt or capture");
    for (const [label, runId] of [["same run", c.runId], ["another run", "run-other"]] as const) {
      await assert.rejects(git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, runId), (err: unknown) => {
        assert.ok(err instanceof RetainedStoppedSourceError, label);
        assert.ok(err instanceof RunResidueBlockedError, `${label}: follows the residue-blocked handling`);
        assert.match(err.message, /^worker_residue_blocked: /);
        assert.ok(err.message.includes(c.runId) && err.message.includes("generation 3") && err.message.includes(path.basename(c.path)), err.message);
        assert.ok(err.detail.length <= 160);
        return true;
      }, label);
    }
    const fresh = await git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, "run-other", false, undefined, seedOpts(attemptIdAt(5, 4)));
    assert.notEqual(fresh.path, c.path);
    assert.ok(fresh.path.includes(".attempt-"));
    assert.equal(fs.readFileSync(path.join(c.path, "dirty.txt"), "utf8"), dirty);
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 1, "the marker is untouched");
  });

  it("in the crash window (bound journal, no mode) the seeds still refuse and touch nothing, and only discovery heals", async () => {
    const c = await marked(3);
    crashWindow(c);
    const config = configBytes();
    const calls: Array<[string, () => Promise<unknown>]> = [
      ["canonical seed, same run", () => git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, c.runId)],
      ["canonical seed, another run", () => git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, "run-other")],
      ["attempt seed", () => git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, c.runId, false, undefined, seedOpts(attemptIdAt(5, 4)))],
    ];
    for (const [label, call] of calls) await assert.rejects(call(), RetainedStoppedSourceError, label);
    assert.deepEqual(configBytes(), config, "nothing was written");
    assert.equal(await git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, "run-other"), undefined, "healed for any claimant");
    assert.equal(journalRaw(c.branch), "", "the exact bound journal is cleared");
    assert.equal(attemptModeOn(c.key), true, "and the marker key's mode is persisted");
    assert.equal(fs.readFileSync(path.join(c.path, "dirty.txt"), "utf8"), "unpreserved\n");
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 1);
  });

  it("heal refuses (today's refusal) for a digest mismatch, a malformed marker, an overflow, a latch and a non-plain journal", async () => {
    const mk = async (iid: number) => {
      const c = await seedCanonical(`run-h${iid}`, iid);
      const journal = journalRaw(c.branch);
      assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 1, reason: "stale_claim" }), "retained");
      crashWindow({ ...c, journal });
      return { ...c, journal };
    };
    const refused = async (c: { branch: string; key: string; runId: string; journal: string }, label: string) => {
      const config = configBytes();
      await assert.rejects(git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, c.runId), RetainedStoppedSourceError, label);
      assert.deepEqual(configBytes(), config, `${label}: nothing written`);
    };
    const a = await mk(2701);
    gitIn(bare, ["config", "--local", `uzi-recovery.${a.branch}.clone`, JSON.stringify({ clonePath: JSON.parse(a.journal).clonePath, runId: a.runId })]);
    await refused(a, "digest mismatch");
    const b = await mk(2702);
    gitIn(bare, ["config", "--local", "--add", `uzi-stopped.${b.runId}.entry`, "{"]);
    await refused(b, "malformed marker");
    const o = await mk(2703);
    for (let i = 0; i < 64; i++) gitIn(bare, ["config", "--local", "--add", `uzi-stopped.${o.runId}.entry`, `{"n":${i}}`]);
    await refused(o, "overflow");
    const l = await mk(2704);
    latchResidueQuarantine({ cause: "test", site: "test" });
    await refused(l, "latched quarantine");
    resetResidueQuarantineForTests();
    const e = await mk(2705);
    gitIn(bare, ["config", "--local", `uzi-recovery.${e.branch}.clone`, JSON.stringify({ ...JSON.parse(e.journal), retainedSources: [{ runId: e.runId, clonePath: JSON.parse(e.journal).clonePath }] })]);
    await refused(e, "episode journal");
  });

  it("heal with a failed attempt-mode write throws and keeps the journal", async () => {
    const c = await marked(3);
    crashWindow(c);
    const seam = git as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const real = seam.runGit.bind(git);
    seam.runGit = async (cwd, args, ...rest) => {
      if (args.some((x) => x.startsWith("uzi-attemptmode."))) throw new Error("ENOSPC: injected");
      return real(cwd, args, ...rest);
    };
    await assert.rejects(git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, c.runId), /ENOSPC/);
    assert.equal(journalRaw(c.branch), c.journal, "the journal stays");
  });

  it("names a missing generation as none", async () => {
    const c = await marked(null);
    await assert.rejects(git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, c.runId), /generation none/);
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
    fs.rmSync(c.path, { recursive: true, force: true });
    gitIn(bare, ["config", "--local", "--unset-all", `uzi-attemptmode.${c.key}.enabled`]);
    const reseeded = await git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, "run-next");
    assert.ok(fs.existsSync(reseeded.path));
    assert.equal(cfgAll(`uzi-stopped.${c.runId}.entry`).length, 0, "the stale marker is retired");
  });
});

describe("FINAL inventory of a kept canonical source", () => {
  it("lists the kept clone through its marker once the journal is cleared, for its own run only", async () => {
    const c = await markedCanonical("run-inv", 2800, 3);
    assert.equal(journalRaw(c.branch), "", "precondition: no journal names the kept clone");
    const own = await git.readInventoryCloneHeads(bare, "run-inv");
    assert.equal(own.kind, "verified");
    if (own.kind !== "verified") return;
    assert.deepEqual(own.clones.map((x) => [x.clonePath, x.branch, x.runId]), [[c.path, c.branch, "run-inv"]]);
    assert.deepEqual(own.heads, [gitIn(c.path, ["rev-parse", "HEAD"])]);
    const other = await git.readInventoryCloneHeads(bare, "run-other");
    assert.equal(other.kind, "verified");
    if (other.kind !== "verified") return;
    assert.deepEqual(other.clones, [], "another run does not inventory it");
    assert.deepEqual(other.foreignOwners, ["run-inv"]);
  });

  it("a missing kept clone makes its own run's inventory unknown, until discard, which is stricter than a plain own-run journal", async () => {
    const c = await markedCanonical("run-inv", 2801, 3);
    fs.rmSync(c.path, { recursive: true, force: true });
    const own = await git.readInventoryCloneHeads(bare, "run-inv");
    assert.equal(own.kind, "unknown");
    assert.equal((own as { cause?: string }).cause, "clone_path_invalid");
    assert.equal((await git.readInventoryCloneHeads(bare, "run-other")).kind, "verified");
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
  const verify = (a: Attempt, head: string | undefined, quiesce: (p: string[], phase: "before" | "after") => Promise<boolean> = quiet, generation = 1) =>
    git.verifyStoppedSourceCovered(bare, a.branch, a.key, { runId: a.runId, clonePath: a.path, attemptId: a.attemptId }, head, { runId: a.runId, generation }, quiesce);

  it("is covered for an unchanged, anchored source, and asks for quiescence before and after", async () => {
    const { a, head } = await captured();
    const calls: Array<[string[], string]> = [];
    assert.equal(await verify(a, head, async (p, phase) => { calls.push([p, phase]); return true; }), "covered");
    assert.deepEqual(calls, [[[a.path], "before"], [[a.path], "after"]]);
  });

  it("leaves the clone's .git/index bytes unchanged when a tracked file's mtime moved", async () => {
    const { a, head } = await captured();
    const when = new Date(Date.now() + 5000);
    fs.utimesSync(path.join(a.path, "work.txt"), when, when);
    const idx = path.join(a.path, ".git", "index");
    const before = fs.readFileSync(idx);
    assert.equal(await verify(a, head), "covered");
    assert.ok(before.equals(fs.readFileSync(idx)), "the hardened status did not refresh the index");
  });

  describe("submodules", () => {
    const SUB_IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
    const subGit = (cwd: string, ...args: string[]): string =>
      gitIn(cwd, [...SUB_IDENT, "-c", "protocol.file.allow=always", ...args]);

    /** A captured clone that also carries a committed, initialized submodule with `ignore = all`. */
    async function withSubmodule() {
      const a = await seedAttempt("run-sm", 1);
      const upstream = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-sub-up-"));
      fs.chmodSync(upstream, 0o700);
      subGit(upstream, "init", "-q", "-b", "main");
      fs.writeFileSync(path.join(upstream, "lib.txt"), "lib\n");
      subGit(upstream, "add", "lib.txt");
      subGit(upstream, "commit", "-q", "-m", "lib");
      subGit(a.path, "submodule", "add", "-q", "--", upstream, "sub");
      subGit(a.path, "config", "-f", ".gitmodules", "submodule.sub.ignore", "all");
      subGit(a.path, "add", ".gitmodules", "sub");
      subGit(a.path, "commit", "-q", "-m", "add submodule");
      await fixtureFetchTracking(git, bare, a.path, a.branch, a.runId, 1);
      const head = gitIn(a.path, ["rev-parse", "HEAD"]);
      return { a, head, upstream };
    }

    it("a populated submodule hiding a dirty or untracked file behind ignore=all is not covered", async () => {
      const { a, head, upstream } = await withSubmodule();
      try {
        assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "precondition: plain status is empty");
        fs.writeFileSync(path.join(a.path, "sub", "scratch.txt"), "only here\n");
        fs.appendFileSync(path.join(a.path, "sub", "lib.txt"), "dirty\n");
        assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "precondition: ignore=all hides the submodule bytes");
        assert.equal(await verify(a, head), "not_covered");
        assert.ok(fs.existsSync(path.join(a.path, "sub", "scratch.txt")));
      } finally {
        fs.rmSync(upstream, { recursive: true, force: true });
      }
    });

    it("a populated submodule hiding a newer commit behind ignore=all is not covered", async () => {
      const { a, head, upstream } = await withSubmodule();
      try {
        fs.appendFileSync(path.join(a.path, "sub", "lib.txt"), "newer\n");
        subGit(path.join(a.path, "sub"), "commit", "-q", "-am", "newer inside submodule");
        assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "precondition: ignore=all hides the moved submodule");
        assert.equal(await verify(a, head), "not_covered");
      } finally {
        fs.rmSync(upstream, { recursive: true, force: true });
      }
    });

    it("an uninitialized submodule (an empty directory at the gitlink) stays covered when the rest holds", async () => {
      const a = await seedAttempt("run-su", 1);
      fs.writeFileSync(path.join(a.path, ".gitmodules"), '[submodule "sub"]\n\tpath = sub\n\turl = ./nowhere\n');
      gitIn(a.path, ["update-index", "--add", "--cacheinfo", `160000,${gitIn(a.path, ["rev-parse", "HEAD"])},sub`]);
      gitIn(a.path, ["add", ".gitmodules"]);
      gitIn(a.path, ["commit", "-m", "declare submodule"]);
      fs.mkdirSync(path.join(a.path, "sub"));
      await fixtureFetchTracking(git, bare, a.path, a.branch, a.runId, 1);
      const head = gitIn(a.path, ["rev-parse", "HEAD"]);
      assert.equal(await verify(a, head), "covered");
      fs.writeFileSync(path.join(a.path, "sub", "stray.txt"), "x\n");
      assert.equal(await verify(a, head), "not_covered", "a file in the gitlink directory is not provable");
    });
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

  it("is not covered while a local branch holds a commit the captured head does not, and covered again once it is reachable", async () => {
    const { a, head } = await captured();
    gitIn(a.path, ["checkout", "-q", "-b", "side"]);
    fs.writeFileSync(path.join(a.path, "side.txt"), "only on a side branch\n");
    gitIn(a.path, ["add", "side.txt"]);
    gitIn(a.path, ["commit", "-m", "side work"]);
    gitIn(a.path, ["checkout", "-q", "--detach", head]);
    assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "the tree is clean at the captured head");
    assert.equal(await verify(a, head), "not_covered", "the side branch's commit is held by nothing");
    gitIn(a.path, ["branch", "-D", "side"]);
    gitIn(a.path, ["branch", "behind", head]);
    assert.equal(await verify(a, head), "covered", "a branch at the captured head itself loses nothing");
  });

  it("is not covered while a tag or a stash entry holds worker-only source", async () => {
    const { a, head } = await captured();
    fs.appendFileSync(path.join(a.path, "work.txt"), "stashed bytes\n");
    gitIn(a.path, ["-c", "user.email=t@t", "-c", "user.name=t", "stash", "push"]);
    assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "the tree is clean at the captured head");
    assert.equal(await verify(a, head), "not_covered", "a stash entry exists");
    gitIn(a.path, ["stash", "drop"]);
    assert.equal(await verify(a, head), "covered");
    gitIn(a.path, ["checkout", "-q", "-b", "tmp"]);
    fs.writeFileSync(path.join(a.path, "tagged.txt"), "tagged\n");
    gitIn(a.path, ["add", "tagged.txt"]);
    gitIn(a.path, ["commit", "-m", "tagged"]);
    gitIn(a.path, ["tag", "keep-me"]);
    gitIn(a.path, ["checkout", "-q", "--detach", head]);
    gitIn(a.path, ["branch", "-D", "tmp"]);
    assert.equal(await verify(a, head), "not_covered", "a tag names a commit the head does not hold");
  });

  /** A commit on a throwaway branch built on the clone's first parent-less base, left on `refName` only. */
  function commitOffBranch(a: Attempt, head: string, refName: string, file: string): string {
    gitIn(a.path, ["checkout", "-q", "-b", "tmp-off", `${head}~1`]);
    fs.writeFileSync(path.join(a.path, file), `${file}\n`);
    gitIn(a.path, ["add", file]);
    gitIn(a.path, ["-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", file]);
    const sha = gitIn(a.path, ["rev-parse", "HEAD"]);
    gitIn(a.path, ["update-ref", refName, sha]);
    gitIn(a.path, ["checkout", "-q", "--detach", head]);
    gitIn(a.path, ["branch", "-D", "tmp-off"]);
    return sha;
  }

  it("is covered when the clone's local default branch sits ahead of an older resumed head and the bare holds it (seed shape)", async () => {
    const { a, head } = await captured();
    const ahead = commitOffBranch(a, head, "refs/heads/main", "ahead.txt");
    gitIn(bare, ["fetch", "-q", "--", a.path, "refs/heads/main:refs/remotes/origin/ahead"]);
    assert.equal(gitIn(bare, ["rev-parse", "refs/remotes/origin/ahead"]), ahead);
    assert.equal(gitIn(a.path, ["status", "--porcelain"]), "");
    assert.equal(await verify(a, head), "covered", "a local ref the bare already holds loses nothing");
  });

  it("is covered when an off-branch tag exists in both the clone and the bare", async () => {
    const { a, head } = await captured();
    commitOffBranch(a, head, "refs/tags/release", "release.txt");
    gitIn(bare, ["fetch", "-q", "--", a.path, "refs/tags/release:refs/tags/release"]);
    assert.equal(await verify(a, head), "covered");
  });

  it("is not covered when a commit exists only under refs/remotes/ in the clone", async () => {
    const { a, head } = await captured();
    commitOffBranch(a, head, "refs/remotes/origin/x", "planted.txt");
    assert.equal(await verify(a, head), "not_covered", "a remote-tracking ref is no exemption");
  });

  it("is not covered when a linked worktree holds a clone-only commit that status and for-each-ref cannot see", async () => {
    const { a, head } = await captured();
    const wtDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-linked-wt-"));
    try {
      fs.rmSync(wtDir, { recursive: true, force: true });
      gitIn(a.path, ["worktree", "add", "--detach", wtDir, head]);
      gitIn(wtDir, ["-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "linked-only"]);
      assert.notEqual(gitIn(wtDir, ["rev-parse", "HEAD"]), head);
      assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "the clone itself looks clean");
      assert.equal(await verify(a, head), "not_covered", "a linked worktree HEAD is worker-only source");
    } finally {
      fs.rmSync(wtDir, { recursive: true, force: true });
    }
  });

  it("is not covered when a clone ref names a blob the bare holds only unreachably", async () => {
    const { a, head } = await captured();
    const blobFile = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "uzi-blob-")), "blob.txt");
    fs.writeFileSync(blobFile, "unreachable blob\n");
    const blob = gitIn(a.path, ["hash-object", "-w", blobFile]);
    assert.equal(gitIn(bare, ["hash-object", "-w", blobFile]), blob);
    fs.rmSync(path.dirname(blobFile), { recursive: true, force: true });
    assert.equal(gitIn(bare, ["cat-file", "-t", blob]), "blob");
    gitIn(a.path, ["update-ref", "refs/tags/blobref", blob]);
    assert.equal(gitIn(a.path, ["status", "--porcelain"]), "");
    assert.equal(await verify(a, head), "not_covered", "an object only present, not reachable, in the bare is not held");
  });

  it("is not covered when a clone replace ref would hide a staged change from status", async () => {
    // Defense in depth: the refusal also comes from the replace ref's own commit being absent from the bare.
    const { a, head } = await captured();
    fs.appendFileSync(path.join(a.path, "work.txt"), "staged only\n");
    gitIn(a.path, ["add", "work.txt"]);
    const tree = gitIn(a.path, ["write-tree"]);
    const fake = gitIn(a.path, ["-c", "user.email=t@t", "-c", "user.name=t", "commit-tree", tree, "-p", `${head}~1`, "-m", "fake"]);
    gitIn(a.path, ["update-ref", `refs/replace/${head}`, fake]);
    assert.equal(await verify(a, head), "not_covered");
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

  it("reclaims a kept canonical source (marker detached, no journal) with its generation or none, and the key seeds a fresh attempt path", async () => {
    let n = 40;
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
      assert.equal(ev.markerState, "detached");
      assert.equal(ev.journalState, "none");
      assert.deepEqual(ev.disposition, { kind: "local_discard" });
      const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
      assert.equal(result.kind, "done");
      assert.equal(fs.existsSync(c.path), false);
      assert.equal(cfgAll(`uzi-stopped.${run}.entry`).length, 0, "the marker is retired with the source");
      const reseed = await git.runnerCloneForBranch(bare, c.branch, c.key, noProofReseed, "run-next", false, undefined, seedOpts(attemptIdAt(n++)));
      assert.ok(fs.existsSync(reseed.path), "the key is usable again");
    }
  });

  it("a marker still bound to its plain journal (crash window) is bound, and its discard clears exactly that journal", async () => {
    const c = await markedCanonical("run-c", 2603, 2);
    crashWindow(c);
    const sel: RetainedSourceSelector = { runId: "run-c", generation: 2, attempt: "canonical" };
    const ev = await inspectOne(sel);
    assert.equal(ev.markerState, "bound");
    assert.equal(ev.journalState, "plain");
    assert.deepEqual(ev.disposition, { kind: "local_discard" });
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor))).kind, "done");
    assert.equal(journalRaw(c.branch), "", "the bound journal is cleared with the source");
    assert.equal(cfgAll("uzi-stopped.run-c.entry").length, 0);
  });

  it("a detached marker's discard never clears a successor's active journal, and the descriptor ignores it", async () => {
    const c = await markedCanonical("run-c", 2604, 2);
    const sel: RetainedSourceSelector = { runId: "run-c", generation: 2, attempt: "canonical" };
    const before = await inspectOne(sel);
    const successor = JSON.stringify({ runId: "run-next", clonePath: `${c.path}.attempt-${attemptIdAt(9, 3)}`, attemptId: attemptIdAt(9, 3) });
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, successor]);
    const ev = await inspectOne(sel);
    assert.equal(ev.markerState, "detached");
    assert.equal(ev.descriptor, before.descriptor, "a successor's journal is not part of the descriptor");
    assert.deepEqual(ev.disposition, { kind: "local_discard" });
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor))).kind, "done");
    assert.equal(journalRaw(c.branch), successor, "the successor's journal is untouched");
    assert.equal(cfgAll("uzi-stopped.run-c.entry").length, 0);
  });

  it("a detached marker whose path is missing is discardable: the marker is retired", async () => {
    const c = await markedCanonical("run-c", 2605, 2);
    fs.rmSync(c.path, { recursive: true, force: true });
    const sel: RetainedSourceSelector = { runId: "run-c", generation: 2, attempt: "canonical" };
    const ev = await inspectOne(sel);
    assert.equal(ev.presence, "missing");
    assert.equal(ev.markerState, "detached");
    assert.deepEqual(ev.disposition, { kind: "local_discard" });
    const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.equal(result.kind, "done");
    assert.deepEqual(result.kind === "done" && result.sources.map((x) => x.outcome), ["missing"]);
    assert.equal(cfgAll("uzi-stopped.run-c.entry").length, 0);
  });

  it("a non-plain journal naming the marker's path refuses (journal_not_plain), a journal elsewhere does not", async () => {
    const c = await markedCanonical("run-c", 2606, 2);
    const sel: RetainedSourceSelector = { runId: "run-c", generation: 2, attempt: "canonical" };
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, JSON.stringify({ runId: "run-c", clonePath: c.path, retainedSources: [{ runId: "run-c", clonePath: c.path }] })]);
    const ev = await inspectOne(sel);
    assert.equal(ev.markerState, "mismatch");
    assert.equal(ev.disposition?.kind, "refused");
    assert.ok(ev.consumers.includes("blocker:journal_not_plain"), ev.consumers.join());
  });

  it("an open server hold of any generation is the owner's to discard; a failed read refuses", async () => {
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
    // Any open hold of this worker on the run counts, whatever its generation: the run's inventory
    // includes every ledger entry across generations.
    assert.deepEqual((await inspectOne(sel, hold(9))).disposition, { kind: "owner_hold_discard", holdIds: ["hold-77"] }, "another generation's hold still depends on this source");
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor, { holds: hold(9) }))).kind, "refused");
    assert.ok(fs.existsSync(a.path));
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor))).kind, "done", "with no hold it is a local discard");
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
    // Exercise the descriptor cap independently of elapsed time, with a bounded real-time guard.
    const r = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive, now: () => 0, budgetMs: 30_000 });
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

// Drive the real runner delete timers; only the child and group kill are inert stand-ins.
// The first child is held until the test emits close; each later child removes only its fixture.
function controlledOperatorRm(killed: boolean) {
  const rec = recordingLogger();
  const events: string[] = [];
  let onSpawn!: () => void;
  const spawned = new Promise<void>(resolve => { onSpawn = resolve; });
  let first: ChildProcess | undefined;
  const children: ChildProcess[] = [];
  git = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({
    retentionDelete: {
      split: true, timeoutMs: 20, closeWaitMs: 50,
      spawn: (_command, args) => {
        const target = args.at(-1)!;
        events.push(`spawn ${target}`);
        const child = new EventEmitter() as unknown as ChildProcess;
        Object.assign(child, { pid: 920001 + children.length, stderr: null });
        children.push(child);
        if (!first) {
          first = child;
          onSpawn();
        } else {
          queueMicrotask(() => {
            fs.rmSync(target, { recursive: true, force: true });
            child.emit("close", 0, null);
          });
        }
        return child;
      },
      kill: (pid) => {
        assert.equal(pid, first!.pid);
        events.push("kill");
        return killed;
      },
    },
  }));
  return {
    ...rec, events, spawned,
    close: () => {
      events.push("close");
      first!.emit("close", null, "SIGKILL");
    },
    // Also release held fake listeners on assertion failure; no real process was started.
    cleanup: () => { for (const child of children) child.emit("close", null, "SIGKILL"); },
  };
}

describe("operator discard runner delete timeout and unsettled coverage", () => {
  for (const killed of [false, true]) {
    for (const kind of ["attempt", "canonical"] as const) {
      it(`${kind}: group kill ${killed ? "succeeds" : "fails"} without close; retries refuse overlap until close`, async (t) => {
        const rm = controlledOperatorRm(killed);
        t.after(rm.cleanup);
        const c = kind === "canonical" ? await markedCanonical("run-timeout", 3090, 1) : undefined;
        const a = kind === "attempt"
          ? plantAttempt("agent/issue-3090", "issue-3090", attemptIdAt(9), "run-timeout", "abandoned")
          : undefined;
        const clone = c?.path ?? a!.dir;
        const skills = a?.skills ?? path.join(path.dirname(clone), `.uzi-skills-${path.basename(clone)}`);
        fs.mkdirSync(skills, { recursive: true });
        fs.writeFileSync(path.join(skills, "plugin.json"), "{}");
        const original = c ? path.join(clone, "dirty.txt") : path.join(clone, "WORK.txt");
        const bytes = fs.readFileSync(original);
        const sel: RetainedSourceSelector = { runId: "run-timeout", generation: 1, attempt: a ? attemptIdAt(9) : "canonical" };
        const ev = await inspectOne(sel);
        assert.equal(ev.disposition?.kind, "local_discard");
        const metadata = configBytes();
        t.mock.timers.enable({ apis: ["setTimeout"] });
        const pending = git.discardStoppedSource(discardOpts(sel, ev.descriptor));
        await rm.spawned;
        t.mock.timers.tick(20);
        assert.deepEqual(rm.events, [`spawn ${skills}`, "kill"]);
        t.mock.timers.tick(50);
        const result = await pending;
        assert.equal(result.kind, "incomplete");
        assert.deepEqual(result.sources, [{ clonePath: clone, outcome: "unknown" }]);
        assert.equal(result.metadata, "kept");
        assert.deepEqual(configBytes(), metadata);
        assert.deepEqual(fs.readFileSync(original), bytes);
        assert.equal(fs.readFileSync(path.join(skills, "plugin.json"), "utf8"), "{}");
        const warning = rm.lines.find(l => (l as { msg?: string }).msg === "retained source delete could not be settled; it and its records are kept") as { error?: string };
        assert.match(warning.error!, killed ? /no exit within 50ms/ : /group kill of pid \d+ failed after 20ms; no close within 50ms/);
        if (killed) assert.doesNotMatch(warning.error!, /group kill of pid .* failed/);
        const fresh = await inspectOne(sel);
        const retry = await git.discardStoppedSource(discardOpts(sel, fresh.descriptor));
        assert.equal(retry.kind, "incomplete");
        assert.deepEqual(rm.events, [`spawn ${skills}`, "kill"], "no overlapping rm");
        assert.deepEqual(configBytes(), metadata);
        assert.deepEqual(fs.readFileSync(original), bytes);
        assert.ok(rm.lines.some(l => (l as { path?: string; reason?: string }).path === skills
          && /may still be running/.test((l as { reason?: string }).reason ?? "")));
        const entered = rm.lines.find(l => (l as { msg?: string }).msg?.includes("recorded as unsettled")) as { level: string; path: string; pending: number };
        assert.deepEqual([entered.level, entered.path, entered.pending], ["warn", path.resolve(skills), 1]);
        assert.equal(rm.lines.some(l => (l as { msg?: string }).msg?.includes("left the unsettled record")), false);
        rm.close();
        const left = rm.lines.find(l => (l as { msg?: string }).msg?.includes("left the unsettled record")) as { level: string; path: string; pending: number };
        assert.deepEqual([left.level, left.path, left.pending], ["info", path.resolve(skills), 0]);
        const afterClose = await inspectOne(sel);
        const done = await git.discardStoppedSource(discardOpts(sel, afterClose.descriptor));
        assert.equal(done.kind, "done");
        assert.deepEqual(rm.events, [`spawn ${skills}`, "kill", "close", `spawn ${skills}`, `spawn ${clone}`]);
        assert.equal(fs.existsSync(clone), false);
        assert.equal(fs.existsSync(skills), false);
        if (a) assert.equal(ledgerState("agent/issue-3090", attemptIdAt(9)), "retired");
      });
    }
  }

  for (const closeWithinWait of [false, true]) {
    for (const killed of closeWithinWait ? [false, true] : [false]) {
      it(`protected multipart: ${closeWithinWait ? "close after kill settles ordinary timeout" : "unsettled delete stops before other part"} (kill=${killed})`, async (t) => {
        const rm = controlledOperatorRm(killed);
        t.after(rm.cleanup);
        const e = await buildEpisode();
        const sel = { runId: EP_RUN, generation: 2, attempt: aid(2) } as const;
        const ev = await inspectOne(sel);
        assert.equal(ev.disposition?.kind, "local_protected_discard");
        assert.equal(ev.sources.length, 2);
        const clones = ev.sources.map(s => s.clonePath);
        const skills = clones.map(p => path.join(path.dirname(p), `.uzi-skills-${path.basename(p)}`));
        const files: string[] = [];
        for (const p of [...clones, ...skills]) {
          fs.mkdirSync(p, { recursive: true });
          const file = path.join(p, "TIMEOUT-KEEP");
          fs.writeFileSync(file, `original ${p}\n`);
          files.push(file);
        }
        const original = files.map(p => fs.readFileSync(p));
        const metadata = configBytes();
        const journal = journalRaw(e.branch);
        const ledger = ledgerState(e.branch, e.successorAttempt);
        const pin = `refs/uzi-recovery-episode/${EP_RUN}/${e.tip}`;
        t.mock.timers.enable({ apis: ["setTimeout"] });
        let settled = false;
        const pending = git.discardProtectedSource(discardOpts(sel, ev.descriptor)).then(result => { settled = true; return result; });
        await rm.spawned;
        t.mock.timers.tick(20);
        await Promise.resolve();
        assert.equal(settled, false, "timeout/kill alone cannot settle disposal");
        assert.deepEqual(rm.events, [`spawn ${skills[0]}`, "kill"]);
        for (const [i, file] of files.entries()) assert.deepEqual(fs.readFileSync(file), original[i]);
        if (closeWithinWait) {
          t.mock.timers.tick(49);
          await Promise.resolve();
          assert.equal(settled, false, "still waiting for close before the bounded deadline");
          assert.deepEqual(rm.events, [`spawn ${skills[0]}`, "kill"]);
          rm.close();
        } else {
          t.mock.timers.tick(50);
        }
        const result = await pending;
        assert.equal(result.kind, "incomplete");
        assert.equal(result.metadata, "kept");
        assert.deepEqual(configBytes(), metadata);
        assert.equal(journalRaw(e.branch), journal);
        assert.equal(ledgerState(e.branch, e.successorAttempt), ledger);
        assert.equal(gitIn(bare, ["rev-parse", pin]), e.tip);
        assert.deepEqual(fs.readFileSync(files[0]!), original[0]);
        assert.deepEqual(fs.readFileSync(files[2]!), original[2]);
        if (closeWithinWait) {
          assert.deepEqual(rm.events, [`spawn ${skills[0]}`, "kill", "close", `spawn ${skills[1]}`, `spawn ${clones[1]}`]);
          assert.deepEqual(result.sources.map(s => s.outcome), ["retained", "removed"]);
          assert.equal(rm.lines.some(l => (l as { msg?: string }).msg?.includes("recorded as unsettled")), false);
          assert.equal(fs.existsSync(clones[1]!), false);
        } else {
          assert.deepEqual(rm.events, [`spawn ${skills[0]}`, "kill"]);
          assert.deepEqual(result.sources.map(s => s.outcome), ["unknown", "retained"]);
          for (const [i, file] of files.entries()) assert.deepEqual(fs.readFileSync(file), original[i]);
        }
      });
    }
  }
});

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

// ─── review rework of M0a ──────────────────────────────────────────────────────────────────

describe("a canonical source left under its plain journal (no marker)", () => {
  const lone = async (runId: string, iid: number) => {
    const c = await seedCanonical(runId, iid);
    fs.writeFileSync(path.join(c.path, "dirty.txt"), "unpreserved\n");
    const skills = path.join(path.dirname(c.path), `.uzi-skills-${path.basename(c.path)}`);
    fs.mkdirSync(skills, { recursive: true });
    fs.writeFileSync(path.join(skills, "plugin.json"), "{}");
    const sel: RetainedSourceSelector = { runId, generation: "none", attempt: "canonical" };
    return { ...c, skills, sel };
  };
  const leftoverOpts = (sel: RetainedSourceSelector, confirm: string, over: Record<string, unknown> = {}) =>
    discardOpts(sel, confirm, { lifecycle: TERMINAL, ...over });

  it("is listed by the aggregate and selector views, and a terminal leftover with no hold is discardable; the key then seeds fresh", async () => {
    const c = await lone("run-t", 2900);
    const all = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    const listed = all.sources.filter((s) => s.runId === "run-t");
    assert.equal(listed.length, 1);
    assert.deepEqual([listed[0]!.kind, listed[0]!.markerState, listed[0]!.journalState, listed[0]!.disposition],
      ["canonical", "none", "plain", undefined]);
    const ev = await inspectOne(c.sel, NO_HOLDS, notLive, TERMINAL);
    assert.deepEqual(ev.holds, { state: "read", ids: [] });
    assert.deepEqual(ev.localRecords, []);
    assert.ok(ev.consumers.includes("run_state:terminal:completed"), ev.consumers.join());
    assert.equal(ev.consumers.some((x) => x.startsWith("blocker:")), false);
    assert.equal(ev.descriptor, listed[0]!.descriptor);
    assert.deepEqual(ev.disposition, { kind: "local_discard" });
    const result = await git.discardStoppedSource(leftoverOpts(c.sel, ev.descriptor));
    assert.deepEqual(result, { kind: "done", sources: [{ clonePath: c.path, outcome: "removed" }], metadata: "retired" });
    assert.equal(fs.existsSync(c.path), false);
    assert.equal(fs.existsSync(c.skills), false);
    assert.equal(journalRaw(c.branch), "", "the journal that named it is cleared");
    assert.equal((await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive })).sources.length, 0);
    const reseed = await git.createOrAttachRunnerClone(bare, 2900, noProofReseed, "run-next");
    assert.ok(fs.existsSync(reseed.path), "a later canonical seed succeeds fresh");
    assert.equal(fs.existsSync(path.join(reseed.path, "dirty.txt")), false);
  });

  it("without positive terminal evidence a leftover is kept: unknown, non-terminal and absent lifecycle all refuse and delete nothing", async () => {
    const c = await lone("run-k", 2909);
    const journal = journalRaw(c.branch);
    const ev = await inspectOne(c.sel, NO_HOLDS, notLive, TERMINAL);
    const cases: Array<[string, RetainedLifecycleSnapshot | undefined, string, string]> = [
      ["unknown", { state: "unknown" }, "run_state_unknown", "run_state:unknown"],
      ["not_terminal", { state: "not_terminal", status: "paused" }, "run_state_not_terminal", "run_state:paused"],
      ["absent", undefined, "run_state_unknown", "run_state:unknown"],
    ];
    for (const [label, lifecycle, blocker, consumer] of cases) {
      const view = await inspectOne(c.sel, NO_HOLDS, notLive, lifecycle);
      assert.ok(view.consumers.includes(consumer), `${label}: ${view.consumers.join()}`);
      assert.ok(view.consumers.includes(`blocker:${blocker}`), `${label}: ${view.consumers.join()}`);
      assert.deepEqual(view.disposition, { kind: "refused", consumers: [blocker] }, label);
      const r = await git.discardStoppedSource(discardOpts(c.sel, ev.descriptor, lifecycle ? { lifecycle } : {}));
      assert.equal(r.kind, "refused", label);
      assert.deepEqual(r.kind === "refused" && r.consumers, [blocker], label);
      assert.ok(fs.existsSync(c.path) && fs.existsSync(c.skills), `${label}: nothing deleted`);
      assert.equal(journalRaw(c.branch), journal, `${label}: the plain journal survives`);
    }
    const done = await git.discardStoppedSource(leftoverOpts(c.sel, ev.descriptor));
    assert.equal(done.kind, "done");
    assert.equal(fs.existsSync(c.path), false);
  });

  it("an open hold or a local record still wins over a non-terminal lifecycle", async () => {
    const c = await lone("run-w", 2910);
    const hold: RetainedHoldsSnapshot = { state: "read", holds: [{ id: "hold-9", generation: 1 }] };
    const ev = await inspectOne(c.sel, hold, notLive, { state: "not_terminal", status: "paused" });
    assert.deepEqual(ev.disposition, { kind: "owner_hold_discard", holdIds: ["hold-9"] });
  });

  it("the aggregate view reads no lifecycle and reports run_state:not_read without a blocker", async () => {
    await lone("run-agg", 2911);
    const all = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    const s = all.sources.find((x) => x.runId === "run-agg")!;
    assert.ok(s.consumers.includes("run_state:not_read"), s.consumers.join());
    assert.equal(s.consumers.some((x) => x.startsWith("blocker:")), false);
  });

  it("an actively parked source (open hold of the run) is the owner's to discard and a discard deletes nothing", async () => {
    const c = await lone("run-p", 2901);
    const hold: RetainedHoldsSnapshot = { state: "read", holds: [{ id: "hold-5", generation: 1 }] };
    const ev = await inspectOne(c.sel, hold);
    assert.deepEqual(ev.disposition, { kind: "owner_hold_discard", holdIds: ["hold-5"] });
    const journal = journalRaw(c.branch);
    const refused = await git.discardStoppedSource(leftoverOpts(c.sel, ev.descriptor, { holds: hold }));
    assert.deepEqual([refused.kind, refused.kind === "refused" && refused.disposition?.kind], ["refused", "owner_hold_discard"]);
    assert.ok(fs.existsSync(c.path) && fs.existsSync(c.skills));
    assert.equal(journalRaw(c.branch), journal);
    for (const holds of [{ state: "unknown" } as RetainedHoldsSnapshot]) {
      assert.equal((await git.discardStoppedSource(leftoverOpts(c.sel, ev.descriptor, { holds }))).kind, "refused");
    }
    assert.ok(fs.existsSync(c.path));
  });

  it("a local recovery record is resolve_local_record and refuses; terminal protection is named too", async () => {
    const c = await lone("run-l", 2902);
    fs.mkdirSync(path.join(fx.dataDir, "recovery", "run-l"), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery", "run-l", "capture-1.json"), JSON.stringify({ generation: 1 }));
    const ev = await inspectOne(c.sel, NO_HOLDS, notLive, TERMINAL);
    assert.equal(ev.disposition?.kind, "resolve_local_record");
    assert.deepEqual(ev.localRecords.map((r) => [r.kind, r.name]), [["recovery", "capture-1.json"]]);
    assert.equal((await git.discardStoppedSource(leftoverOpts(c.sel, ev.descriptor))).kind, "refused");
    assert.ok(fs.existsSync(c.path));
    assert.notEqual(journalRaw(c.branch), "");
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ terminalRecordProtection: async () => true }));
    fs.rmSync(path.join(fx.dataDir, "recovery", "run-l"), { recursive: true });
    const terminal = await inspectOne(c.sel);
    assert.equal(terminal.disposition?.kind, "resolve_local_record");
    assert.deepEqual(terminal.localRecords.map((r) => r.kind), ["terminal"]);
  });

  it("a live flight's clone refuses", async () => {
    const c = await lone("run-v", 2903);
    const ev = await inspectOne(c.sel, NO_HOLDS, notLive, TERMINAL);
    const live = await git.discardStoppedSource(leftoverOpts(c.sel, ev.descriptor, { isLivePath: (p: string) => p === c.path }));
    assert.deepEqual([live.kind, live.kind === "refused" && live.consumers.some((x) => x.startsWith("live:"))], ["refused", true]);
    assert.ok(fs.existsSync(c.path));
    assert.equal((await inspectOne(c.sel, NO_HOLDS, (p) => p === c.path)).disposition?.kind, "refused");
  });

  it("a journal that changed between inspect and discard is descriptor_changed", async () => {
    const c = await lone("run-d", 2904);
    const ev = await inspectOne(c.sel, NO_HOLDS, notLive, TERMINAL);
    const changed = JSON.stringify({ clonePath: c.path, runId: "run-d" });
    assert.notEqual(changed, journalRaw(c.branch));
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, changed]);
    const r = await git.discardStoppedSource(leftoverOpts(c.sel, ev.descriptor));
    assert.deepEqual([r.kind, r.kind === "refused" && r.reason], ["refused", "descriptor_changed"]);
    assert.ok(fs.existsSync(c.path));
  });

  it("a discard never clears a journal that no longer names that source", async () => {
    const c = await lone("run-s", 2905);
    const ev = await inspectOne(c.sel, NO_HOLDS, notLive, TERMINAL);
    const successor = JSON.stringify({ runId: "run-next", clonePath: `${c.path}.attempt-${attemptIdAt(9, 3)}`, attemptId: attemptIdAt(9, 3) });
    const r = await git.discardStoppedSource(leftoverOpts(c.sel, ev.descriptor, {
      quiesce: async () => {
        gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, successor]);
        return true;
      },
    }));
    assert.equal(r.kind, "done");
    assert.equal(fs.existsSync(c.path), false);
    assert.equal(journalRaw(c.branch), successor, "the successor's journal is untouched");
  });

  it("a marker that names the path (valid or malformed text) keeps it out of this view", async () => {
    const c = await lone("run-m", 2906);
    gitIn(bare, ["config", "--local", "--add", "uzi-stopped.run-other.entry", `not json ${JSON.stringify(c.path)}`]);
    const all = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    assert.equal(all.sources.filter((s) => s.runId === "run-m").length, 0);
    assert.equal((await git.inspectRetainedSources({ selector: c.sel, holds: NO_HOLDS, isLivePath: notLive, barePath: bare })).sources.length, 0);
  });

  it("malformed text naming the path unquoted still keeps it out of this view (fail closed)", async () => {
    const c = await lone("run-u", 2907);
    gitIn(bare, ["config", "--local", "--add", "uzi-stopped.run-other2.entry", `garbage ${c.path} trailing`]);
    const all = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    assert.equal(all.sources.filter((s) => s.runId === "run-u").length, 0);
  });

  it("a marker for a longer key (issue-290, valid or malformed) does not hide the issue-29 leftover", async () => {
    const short = await lone("run-short", 29);
    const longer = await markedCanonical("run-long", 290, 1);
    gitIn(bare, ["config", "--local", "--add", "uzi-stopped.run-other.entry", `not json ${JSON.stringify(longer.path)}`]);
    const all = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    assert.equal(all.sources.filter((s) => s.runId === "run-short").length, 1, "the prefix path is still listed");
    const ev = await inspectOne(short.sel);
    const r = await git.discardStoppedSource(leftoverOpts(short.sel, ev.descriptor));
    assert.equal(r.kind, "done");
    assert.equal(fs.existsSync(short.path), false);
    assert.ok(fs.existsSync(longer.path), "the longer key's source is untouched");
  });

  it("an unrelated run's marker overflow does not hide another run's leftover", async () => {
    const c = await lone("run-o", 2907);
    // Overflow for run-other only: more valid markers than the reader's cap.
    for (let i = 0; i < 70; i++) {
      gitIn(bare, ["config", "--local", "--add", "uzi-stopped.run-other.entry",
        JSON.stringify({ runId: "run-other", clonePath: `${c.path}-x${i}`, branch: "agent/issue-1", key: "issue-1", generation: 1, reason: "r", journalDigest: "d" })]);
    }
    const all = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    assert.equal(all.sources.filter((s) => s.runId === "run-o").length, 1);
  });

  it("the leftover's own run overflowing its markers keeps it out of the view (fail closed)", async () => {
    const c = await lone("run-self", 2908);
    for (let i = 0; i < 70; i++) {
      gitIn(bare, ["config", "--local", "--add", "uzi-stopped.run-self.entry", `junk ${i}`]);
    }
    const all = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    assert.equal(all.sources.filter((s) => s.runId === "run-self").length, 0);
    assert.ok(fs.existsSync(c.path));
  });
});

describe("a read-only status of a kept clone", () => {
  it("worktreeStatus leaves the clone's .git/index bytes unchanged when a tracked file's mtime moved", async () => {
    const c = await seedCanonical("run-idx", 2950);
    const file = gitIn(c.path, ["ls-files"]).split("\n")[0]!;
    // Make the index stat cache stale: the file's mtime moves, its content does not.
    const when = new Date(Date.now() + 5000);
    fs.utimesSync(path.join(c.path, file), when, when);
    const idx = path.join(c.path, ".git", "index");
    const before = fs.readFileSync(idx);
    assert.deepEqual(await git.worktreeStatus(c.path), []);
    assert.ok(before.equals(fs.readFileSync(idx)), "git did not refresh the kept clone's index");
  });
});

describe("rework: local custody, holds and stale markers", () => {
  const recordFile = (root: string, runId: string, name: string, body: object): void => {
    fs.mkdirSync(path.join(fx.dataDir, root, runId), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, root, runId, name), JSON.stringify(body));
  };

  it("a plain source with a local custody record is resolve_local_record and a discard refuses without deleting", async () => {
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    recordFile("recovery", "run-a", "capture-1.json", { generation: 1 });
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    assert.equal(ev.disposition?.kind, "resolve_local_record");
    assert.deepEqual(ev.localRecords.map((r) => [r.kind, r.name]), [["recovery", "capture-1.json"]]);
    const refused = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.deepEqual([refused.kind, refused.kind === "refused" && refused.disposition?.kind], ["refused", "resolve_local_record"]);
    assert.ok(fs.existsSync(a.path));
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed");
    fs.rmSync(path.join(fx.dataDir, "recovery", "run-a"), { recursive: true });
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor))).kind, "done", "resolved record: a local discard");
  });

  it("an abandoned attempt with a settlement record and a marked canonical source with a recovery record refuse the same way", async () => {
    const planted = plantAttempt("agent/issue-3001", "issue-3001", attemptIdAt(3), "run-ab", "abandoned");
    recordFile("recovery-settlement", "run-ab", "hold-1.json", { predecessorGeneration: 1 });
    const absel: RetainedSourceSelector = { runId: "run-ab", generation: 1, attempt: attemptIdAt(3) };
    const ab = await inspectOne(absel);
    assert.equal(ab.disposition?.kind, "resolve_local_record");
    assert.deepEqual(ab.localRecords.map((r) => [r.kind, r.name]), [["settlement", "hold-1.json"]]);
    assert.equal((await git.discardStoppedSource(discardOpts(absel, ab.descriptor))).kind, "refused");
    assert.ok(fs.existsSync(planted.dir));

    const c = await seedCanonical("run-c");
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: c.runId, clonePath: c.path, generation: 2, reason: "claim_fence" }), "retained");
    recordFile("recovery", "run-c", "capture-2.json", { generation: 2 });
    const csel: RetainedSourceSelector = { runId: "run-c", generation: 2, attempt: "canonical" };
    const ev = await inspectOne(csel);
    assert.equal(ev.disposition?.kind, "resolve_local_record");
    assert.equal((await git.discardStoppedSource(discardOpts(csel, ev.descriptor))).kind, "refused");
    assert.ok(fs.existsSync(c.path));
    assert.equal(cfgAll("uzi-stopped.run-c.entry").length, 1);
  });

  it("terminal record protection also blocks a plain discard", async () => {
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ terminalRecordProtection: async () => true }));
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    assert.equal(ev.disposition?.kind, "resolve_local_record");
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor))).kind, "refused");
    assert.ok(fs.existsSync(a.path));
  });

  it("every open server hold of the run is listed whatever its generation, and a non-array holds list refuses", async () => {
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const holds: RetainedHoldsSnapshot = { state: "read", holds: [{ id: "hold-b", generation: 9 }, { id: "hold-a", generation: 1 }] };
    const ev = await inspectOne(sel, holds);
    assert.deepEqual(ev.disposition, { kind: "owner_hold_discard", holdIds: ["hold-a", "hold-b"] });
    assert.deepEqual(ev.holds, { state: "read", ids: ["hold-a", "hold-b"] });
    const malformed = { state: "read", holds: "hold-a" } as unknown as RetainedHoldsSnapshot;
    assert.equal((await inspectOne(sel, malformed)).disposition?.kind, "refused");
    const refused = await git.discardStoppedSource(discardOpts(sel, ev.descriptor, { holds: malformed }));
    assert.equal(refused.kind, "refused", "a non-array holds list is a refusal, not a thrown error");
    assert.ok(fs.existsSync(a.path));
  });


  it("a marker whose journal and path are gone is retired by the next seed, so a later discovery does not throw", async () => {
    const c = await markedCanonical("run-c", 2600, 3);
    fs.rmSync(c.path, { recursive: true, force: true });
    gitIn(bare, ["config", "--local", "--unset-all", `uzi-attemptmode.${c.key}.enabled`]);
    const next = await git.createOrAttachRunnerClone(bare, 2600, noProofReseed, "run-next");
    assert.equal(cfgAll("uzi-stopped.run-c.entry").length, 0, "the stale marker was retired where it was evaluated");
    await git.markRecoveryCapture(bare, next.path, c.branch, "run-next");
    const found = await git.discoverRetainedRecovery(fx.originPath, c.branch, c.key, "run-next");
    assert.equal(found?.journal.runId, "run-next");
  });

  it("a stale generation-1 marker cannot rebind to the same run's generation-2 source at the same path", async () => {
    const c = await markedCanonical("run-c", 2600, 1);
    fs.rmSync(c.path, { recursive: true, force: true });
    gitIn(bare, ["config", "--local", "--unset-all", `uzi-attemptmode.${c.key}.enabled`]);
    const c2 = await seedCanonical("run-c", 2600);
    fs.writeFileSync(path.join(c2.path, "gen2.txt"), "generation two work\n");
    assert.equal(cfgAll("uzi-stopped.run-c.entry").length, 0, "the old marker is gone");
    assert.equal(await git.markStoppedCanonical(bare, c2.branch, c2.key, { runId: "run-c", clonePath: c2.path, generation: 2, reason: "stale_claim" }), "retained");
    const values = cfgAll("uzi-stopped.run-c.entry").map((v) => JSON.parse(v) as { generation: number });
    assert.deepEqual(values.map((v) => v.generation), [2]);
    const one: RetainedSourceSelector = { runId: "run-c", generation: 1, attempt: "canonical" };
    assert.equal((await git.inspectRetainedSources({ selector: one, holds: NO_HOLDS, isLivePath: notLive, barePath: bare })).sources.length, 0);
    const ev = await inspectOne({ ...one, generation: 2 });
    const wrong = await git.discardStoppedSource(discardOpts(one, ev.descriptor));
    assert.equal(wrong.kind, "refused");
    assert.equal(fs.readFileSync(path.join(c2.path, "gen2.txt"), "utf8"), "generation two work\n");
  });

  it("an existing marker of the path with another generation is never rewritten or inherited", async () => {
    const c = await markedCanonical("run-c", 2600, 3);
    const before = configBytes();
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: "run-c", clonePath: c.path, generation: 4, reason: "stale_claim" }), "unknown");
    assert.equal(await git.markStoppedCanonical(bare, c.branch, c.key, { runId: "run-c", clonePath: c.path, generation: null, reason: "stale_claim" }), "unknown");
    assert.deepEqual(configBytes(), before);
  });

  it("retire, remove and detach refuse a path a valid marker names, and keep everything", async () => {
    const c = await markedCanonical("run-c", 2600, 3);
    const before = configBytes();
    // The journal is cleared: retire fails closed on the missing pair, remove and detach on the marker.
    await assert.rejects(git.retireRunnerClone(bare, c.path, c.branch, c.runId, { discard: true }), /different clone path/);
    await assert.rejects(git.removeRunnerClone(c.path, c.runId), RetainedStoppedSourceError);
    await assert.rejects(git.detachTerminalRetained(bare, c.branch, c.key, JSON.parse(c.journal)), RetainedStoppedSourceError);
    assert.equal(fs.readFileSync(path.join(c.path, "dirty.txt"), "utf8"), "unpreserved\n");
    assert.deepEqual(configBytes(), before, "marker and ledger are untouched");
    // With the bound journal back (the crash window) the marker itself refuses the retire.
    crashWindow(c);
    await assert.rejects(git.retireRunnerClone(bare, c.path, c.branch, c.runId, { discard: true }), RetainedStoppedSourceError);
    assert.equal(fs.readFileSync(path.join(c.path, "dirty.txt"), "utf8"), "unpreserved\n");
    // An unmarked canonical clone is still retired normally.
    const plain = await seedCanonical("run-plain", 2610);
    await git.retireRunnerClone(bare, plain.path, plain.branch, plain.runId, { discard: true });
    assert.equal(fs.existsSync(plain.path), false);
  });

  it("a malformed marker naming the path (or an overflowing run) makes retire, remove and detach refuse too", async () => {
    const c = await seedCanonical("run-m", 2620);
    fs.writeFileSync(path.join(c.path, "dirty.txt"), "unpreserved\n");
    const planted = JSON.stringify({ version: 1, runId: c.runId, clonePath: c.path, extra: "key" });
    gitIn(bare, ["config", "--local", "--add", `uzi-stopped.${c.runId}.entry`, planted]);
    const before = configBytes();
    await assert.rejects(git.retireRunnerClone(bare, c.path, c.branch, c.runId, { discard: true }), RetainedStoppedSourceError);
    await assert.rejects(git.removeRunnerClone(c.path, c.runId), RetainedStoppedSourceError);
    await assert.rejects(git.detachTerminalRetained(bare, c.branch, c.key, JSON.parse(journalRaw(c.branch))), RetainedStoppedSourceError);
    assert.equal(fs.readFileSync(path.join(c.path, "dirty.txt"), "utf8"), "unpreserved\n", "the dirty clone stays");
    assert.deepEqual(configBytes(), before);
    gitIn(bare, ["config", "--local", "--unset-all", `uzi-stopped.${c.runId}.entry`]);
    for (let i = 0; i < 65; i++) gitIn(bare, ["config", "--local", "--add", `uzi-stopped.${c.runId}.entry`, `{"n":${i}}`]);
    await assert.rejects(git.retireRunnerClone(bare, c.path, c.branch, c.runId, { discard: true }), RetainedStoppedSourceError);
    await assert.rejects(git.removeRunnerClone(c.path, c.runId), RetainedStoppedSourceError);
    assert.ok(fs.existsSync(c.path));
  });

  it("a malformed marker of the owner run refuses retire and remove even when its text names no path", async () => {
    const c = await seedCanonical("run-m", 2621);
    fs.writeFileSync(path.join(c.path, "dirty.txt"), "unpreserved\n");
    gitIn(bare, ["config", "--local", "--add", `uzi-stopped.${c.runId}.entry`, `{"version":1,"runId":"${c.runId}","clo`]);
    await assert.rejects(git.removeRunnerClone(c.path, c.runId), RetainedStoppedSourceError);
    await assert.rejects(git.retireRunnerClone(bare, c.path, c.branch, c.runId, { discard: true }), RetainedStoppedSourceError);
    assert.equal(fs.readFileSync(path.join(c.path, "dirty.txt"), "utf8"), "unpreserved\n", "the dirty clone stays");
  });

  it("the marker comparison resolves the caller's path (a trailing slash does not bypass it)", async () => {
    const c = await markedCanonical("run-c", 2600, 3);
    await assert.rejects(git.removeRunnerClone(`${c.path}/`, c.runId), RetainedStoppedSourceError);
    assert.equal(fs.readFileSync(path.join(c.path, "dirty.txt"), "utf8"), "unpreserved\n");
  });

  it("discarding one marked source never retires the marker of a source whose path it prefixes (issue-26 vs issue-260)", async () => {
    const short = await markedCanonical("run-m", 26, 1);
    const long = await markedCanonical("run-m", 260, 2);
    const sel: RetainedSourceSelector = { runId: "run-m", generation: 1, attempt: "canonical" };
    const ev = await inspectOne(sel);
    assert.equal((await git.discardStoppedSource(discardOpts(sel, ev.descriptor))).kind, "done");
    assert.equal(fs.existsSync(short.path), false);
    const left = cfgAll("uzi-stopped.run-m.entry").map((v) => (JSON.parse(v) as { clonePath: string }).clonePath);
    assert.deepEqual(left, [long.path], "the other source's marker survives");
    assert.ok(fs.existsSync(long.path));
    const other = await inspectOne({ runId: "run-m", generation: 2, attempt: "canonical" });
    assert.equal(other.markerState, "detached");
  });

  it("a journal naming another run is labelled other, not episode", async () => {
    const c = await markedCanonical("run-c", 2600, 3);
    gitIn(bare, ["config", "--local", `uzi-recovery.${c.branch}.clone`, JSON.stringify({ runId: "run-new", clonePath: c.path })]);
    const ev = await inspectOne({ runId: "run-c", generation: 3, attempt: "canonical" });
    assert.equal(ev.journalState, "other");
    assert.equal(ev.markerState, "mismatch");
  });

  it("a g<N> beyond a safe integer is attribution_unknown and refuses, never generation none", async () => {
    const id = `20260101T000009Z-g99999999999999999999-${hex16(10)}`;
    const planted = plantAttempt("agent/issue-3001", "issue-3001", id, "run-big", "reclaimed");
    const sel: RetainedSourceSelector = { runId: "run-big", generation: "none", attempt: id };
    const ev = await inspectOne(sel);
    assert.equal(ev.disposition?.kind, "refused");
    assert.ok(ev.consumers.includes("blocker:attribution_unknown"), ev.consumers.join());
    const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.equal(result.kind, "refused");
    assert.ok(fs.existsSync(planted.dir));
  });
});

describe("rework: coverage proof does not trust the clone's git config or index flags", () => {
  async function captured() {
    const a = await seedAttempt("run-v", 1);
    fs.writeFileSync(path.join(a.path, "work.txt"), "captured\n");
    gitIn(a.path, ["add", "work.txt"]);
    gitIn(a.path, ["commit", "-m", "milestone"]);
    await fixtureFetchTracking(git, bare, a.path, a.branch, a.runId, 1);
    return { a, head: gitIn(a.path, ["rev-parse", "HEAD"]) };
  }
  const verify = (a: Attempt, head: string) =>
    git.verifyStoppedSourceCovered(bare, a.branch, a.key, { runId: a.runId, clonePath: a.path, attemptId: a.attemptId }, head, { runId: a.runId, generation: 1 }, quiet);

  it("an untracked file is not covered even when the clone config hides untracked files", async () => {
    const { a, head } = await captured();
    gitIn(a.path, ["config", "status.showUntrackedFiles", "no"]);
    assert.equal(await verify(a, head), "covered");
    fs.writeFileSync(path.join(a.path, "unpreserved.txt"), "new\n");
    assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "the clone's own config really hides it");
    assert.equal(await verify(a, head), "not_covered");
  });

  it("a dirty tracked file hidden by assume-unchanged or skip-worktree is not covered", async () => {
    const { a, head } = await captured();
    gitIn(a.path, ["update-index", "--assume-unchanged", "work.txt"]);
    fs.appendFileSync(path.join(a.path, "work.txt"), "unpreserved\n");
    assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "status really hides the change");
    assert.equal(await verify(a, head), "not_covered", "assume-unchanged");
    gitIn(a.path, ["update-index", "--no-assume-unchanged", "work.txt"]);
    gitIn(a.path, ["checkout", "--", "work.txt"]);
    assert.equal(await verify(a, head), "covered");
    gitIn(a.path, ["update-index", "--skip-worktree", "work.txt"]);
    assert.equal(await verify(a, head), "not_covered", "skip-worktree");
  });

  it("a clone config that moves the excludes file or the worktree root is not covered", async () => {
    const { a, head } = await captured();
    const ignores = path.join(a.path, "..", "evil-excludes");
    fs.writeFileSync(ignores, "hidden.txt\n");
    gitIn(a.path, ["config", "core.excludesFile", ignores]);
    assert.equal(await verify(a, head), "covered");
    fs.writeFileSync(path.join(a.path, "hidden.txt"), "unpreserved\n");
    assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "the clone's own config really hides it");
    assert.equal(await verify(a, head), "not_covered", "core.excludesFile");
    fs.rmSync(path.join(a.path, "hidden.txt"));
    gitIn(a.path, ["config", "--unset", "core.excludesFile"]);
    assert.equal(await verify(a, head), "covered");

    const elsewhere = fs.mkdtempSync(path.join(path.dirname(a.path), "elsewhere-"));
    gitIn(a.path, ["config", "core.worktree", elsewhere]);
    fs.appendFileSync(path.join(a.path, "work.txt"), "unpreserved\n");
    assert.equal(await verify(a, head), "not_covered", "core.worktree");
  });

  it("core.ignorecase=true in the clone config cannot hide an untracked README behind a tracked readme", async () => {
    const a = await seedAttempt("run-ic", 1);
    fs.writeFileSync(path.join(a.path, "readme"), "captured\n");
    gitIn(a.path, ["add", "readme"]);
    gitIn(a.path, ["commit", "-m", "milestone"]);
    await fixtureFetchTracking(git, bare, a.path, a.branch, a.runId, 1);
    const head = gitIn(a.path, ["rev-parse", "HEAD"]);
    assert.equal(await verify(a, head), "covered");
    gitIn(a.path, ["config", "core.ignorecase", "true"]);
    fs.writeFileSync(path.join(a.path, "README"), "unpreserved\n");
    assert.equal(gitIn(a.path, ["status", "--porcelain"]), "", "the clone's own config really hides it");
    assert.equal(await verify(a, head), "not_covered");
  });

  it("a clone claiming core.ignorecase in any form git reads as true is not covered even when clean", async () => {
    // A clean tree: only the refusal (not the status override) can turn these away.
    const { a, head } = await captured();
    assert.equal(await verify(a, head), "covered");
    for (const value of ["true", "yes", "on", "1", "2", "0x1", "1k"]) {
      gitIn(a.path, ["config", "core.ignorecase", value]);
      assert.equal(await verify(a, head), "not_covered", `core.ignorecase=${value}`);
    }
    fs.appendFileSync(path.join(a.path, ".git", "config"), "[core]\n\tignorecase\n");
    assert.equal(await verify(a, head), "not_covered", "bare key");
    gitIn(a.path, ["config", "--unset-all", "core.ignorecase"]);
    gitIn(a.path, ["config", "core.ignorecase", "false"]);
    assert.equal(await verify(a, head), "covered", "false is not a claim");
  });

  it("a clone with a filter driver configured or an info/attributes file is not covered", async () => {
    const { a, head } = await captured();
    gitIn(a.path, ["config", "filter.lfsish.clean", "cat"]);
    assert.equal(await verify(a, head), "not_covered", "filter config");
    gitIn(a.path, ["config", "--unset", "filter.lfsish.clean"]);
    assert.equal(await verify(a, head), "covered");
    fs.mkdirSync(path.join(a.path, ".git", "info"), { recursive: true });
    fs.writeFileSync(path.join(a.path, ".git", "info", "attributes"), "* -diff\n");
    assert.equal(await verify(a, head), "not_covered", "info/attributes");
  });

  it("a filter driver reached through include.path, config.worktree or core.attributesFile is not covered", async () => {
    const { a, head } = await captured();
    const orig = path.join(path.dirname(a.path), "orig-work.txt");
    fs.writeFileSync(orig, "captured\n");
    fs.writeFileSync(path.join(a.path, ".gitattributes"), "work.txt filter=x\n");
    gitIn(a.path, ["add", ".gitattributes"]);
    gitIn(a.path, ["commit", "-m", "attributes"]);
    await fixtureFetchTracking(git, bare, a.path, a.branch, a.runId, 1);
    const tip = gitIn(a.path, ["rev-parse", "HEAD"]);
    assert.notEqual(tip, head);
    assert.equal(await verify(a, tip), "covered");
    const included = path.join(path.dirname(a.path), "included.cfg");
    fs.writeFileSync(included, `[filter "x"]\n\tclean = cat ${orig}\n`);
    gitIn(a.path, ["config", "include.path", included]);
    fs.writeFileSync(path.join(a.path, "work.txt"), "UNSAVED!\n");
    assert.equal(await verify(a, tip), "not_covered", "include.path");
    gitIn(a.path, ["config", "--unset", "include.path"]);
    gitIn(a.path, ["config", "extensions.worktreeConfig", "true"]);
    gitIn(a.path, ["config", "--worktree", "filter.y.clean", `cat ${orig}`]);
    assert.equal(await verify(a, tip), "not_covered", "config.worktree");
    gitIn(a.path, ["config", "--worktree", "--unset", "filter.y.clean"]);
    gitIn(a.path, ["config", "--unset", "extensions.worktreeConfig"]);
    gitIn(a.path, ["config", "core.attributesFile", path.join(path.dirname(a.path), "attrs")]);
    assert.equal(await verify(a, tip), "not_covered", "core.attributesFile");
  });

  // core.trustctime=false with a same-size edit and a restored mtime cannot be reproduced reliably
  // here (the index stat data, including the ctime, changes with the edit and the filesystem's ctime
  // granularity decides whether git re-reads the content), so that override has no test.
});

describe("rework: discard under the residue quarantine and a failed delete", () => {
  it("a latch landing inside the caller's quiescence deletes nothing and retires nothing", async () => {
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor, {
      quiesce: async () => { latchResidueQuarantine({ cause: "test", site: "test" }); return true; },
    }));
    assert.equal(result.kind, "unknown", JSON.stringify(result));
    assert.ok(fs.existsSync(a.path));
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed");
  });

  it("a latch landing between two deletes keeps the remaining source and the whole descriptor", async () => {
    let calls = 0;
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
      retentionDelete: {
        split: true,
        run: async (_command, args) => {
          fs.rmSync(args.at(-1)!, { recursive: true, force: true });
          if (++calls === 1) latchResidueQuarantine({ cause: "test", site: "test" });
        },
      },
    }));
    const e = await buildEpisode();
    const ev = await inspectOne({ runId: EP_RUN, generation: 2, attempt: aid(2) });
    const result = await git.discardProtectedSource(discardOpts({ runId: EP_RUN, generation: 2, attempt: aid(2) }, ev.descriptor));
    assert.equal(result.kind, "unknown", JSON.stringify(result));
    assert.equal(calls, 1, "no second delete after the latch");
    assert.deepEqual(result.kind === "unknown" ? result.sources.map((x) => x.outcome).sort() : [], ["removed", "retained"]);
    assert.equal(result.kind === "unknown" ? result.metadata : "", "kept");
    assert.ok(fs.existsSync(e.sourcePath) !== fs.existsSync(e.successorPath), "exactly one source remains");
    assert.equal(cfgAll(`uzi-recovery.${e.branch}.clone`).length, 1, "the journal is kept");
  });

  it("a latch landing after the skills sibling delete keeps the clone of the same source", async () => {
    let calls = 0;
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
      retentionDelete: {
        split: true,
        run: async (_command, args) => {
          fs.rmSync(args.at(-1)!, { recursive: true, force: true });
          if (++calls === 1) latchResidueQuarantine({ cause: "test", site: "test" });
        },
      },
    }));
    const planted = plantAttempt("agent/issue-3002", "issue-3002", attemptIdAt(5), "run-l1", "reclaimed");
    const sel: RetainedSourceSelector = { runId: "run-l1", generation: 1, attempt: attemptIdAt(5) };
    const ev = await inspectOne(sel);
    const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.equal(result.kind, "unknown", JSON.stringify(result));
    assert.equal(calls, 1, "the clone delete never started");
    assert.equal(fs.existsSync(planted.skills), false, "the skills sibling went first");
    assert.ok(fs.existsSync(planted.dir), "the clone remains");
    assert.equal(ledgerState("agent/issue-3002", attemptIdAt(5)), "reclaimed");
  });

  it("a delete that reports success while the path is still present keeps the metadata", async () => {
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
      retentionDelete: { split: true, run: async () => {} },
    }));
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const sel = selectorOf(a, 1);
    const ev = await inspectOne(sel);
    const result = await git.discardStoppedSource(discardOpts(sel, ev.descriptor));
    assert.deepEqual(result, { kind: "incomplete", sources: [{ clonePath: a.path, outcome: "unknown" }], metadata: "kept" });
    assert.ok(fs.existsSync(a.path));
    assert.equal(ledgerState(a.branch, a.attemptId), "reclaimed");
  });
});

describe("rework: bounded measurement and one-lock inspection", () => {
  async function tree(): Promise<{ a: Attempt; sel: RetainedSourceSelector }> {
    const a = await seedAttempt("run-a", 1);
    for (const d of ["x", "y", "z"]) {
      fs.mkdirSync(path.join(a.path, "big", d), { recursive: true });
      for (let i = 0; i < 4; i++) fs.writeFileSync(path.join(a.path, "big", d, `f${i}`), "x");
    }
    fs.mkdirSync(path.join(a.path, "n1", "n2", "n3", "n4"), { recursive: true });
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    return { a, sel: selectorOf(a, 1) };
  }

  for (const [label, caps] of [
    ["entries", { maxEntries: 5 }], ["pending directories", { maxPending: 1 }], ["depth", { maxDepth: 2 }],
  ] as const) {
    it(`a cap on ${label} is partial with unknown bytes, never a complete total`, async () => {
      const { sel } = await tree();
      const full = await inspectOne(sel);
      assert.equal(full.bytesState, "measured", "default caps measure this tree fully");
      const r = await git.inspectRetainedSources({ selector: sel, holds: NO_HOLDS, isLivePath: notLive, barePath: bare, measureCaps: caps });
      assert.equal(r.partial, true);
      assert.notEqual(r.sources[0]!.bytesState, "measured");
      assert.equal(r.sources[0]!.allocatedBytes, null);
    });
  }

  it("inspection evaluates inside the lock that gathered, and an exhausted budget while the lock is queued is partial", async () => {
    const a = await seedAttempt("run-a", 1);
    assert.equal(await git.retainStoppedAttempt(bare, a.branch, a), "retained");
    const seam = git as unknown as {
      withLock: <T>(key: string, fn: () => Promise<T>) => Promise<T>;
      evaluateRetained: (...args: unknown[]) => Promise<unknown>;
    };
    let held = false;
    const realLock = seam.withLock.bind(git);
    seam.withLock = (key, fn) => realLock(key, async () => {
      if (key === bare) held = true;
      try { return await fn(); } finally { if (key === bare) held = false; }
    });
    const evaluations: boolean[] = [];
    const realEvaluate = seam.evaluateRetained.bind(git);
    seam.evaluateRetained = async (...args) => { evaluations.push(held); return realEvaluate(...args); };
    const ok = await git.inspectRetainedSources({ selector: selectorOf(a, 1), holds: NO_HOLDS, isLivePath: notLive, barePath: bare });
    assert.equal(ok.sources.length, 1);
    assert.deepEqual(evaluations, [true], "the disposition is computed from the lock-held snapshot");

    let release!: () => void;
    const holder = realLock(bare, () => new Promise<void>((resolve) => { release = resolve; }));
    const timer = setTimeout(() => release(), 2500);
    const started = Date.now();
    const starved = await git.inspectRetainedSources({ selector: selectorOf(a, 1), holds: NO_HOLDS, isLivePath: notLive, barePath: bare, budgetMs: 200 });
    const waited = Date.now() - started;
    clearTimeout(timer);
    release();
    await holder;
    assert.equal(starved.partial, true, "a budget exhausted behind the lock is partial");
    assert.equal(starved.sources.length, 0);
    assert.ok(waited < 2000, `the lock wait is bounded by the budget (${waited} ms)`);
  });

  it("the aggregate view omits a ledger-live attempt the worker reports live, and lists it otherwise", async () => {
    const a = await seedAttempt("run-a", 1);
    const listed = await git.inspectRetainedSources({ barePath: bare, isLivePath: notLive });
    assert.equal(listed.sources.length, 1);
    assert.equal(listed.sources[0]!.ledgerState, "live");
    const hidden = await git.inspectRetainedSources({ barePath: bare, isLivePath: (p) => p === a.path });
    assert.equal(hidden.sources.length, 0);
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
