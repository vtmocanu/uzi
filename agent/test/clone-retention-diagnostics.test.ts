import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, type AttemptLedgerState } from "../src/git.js";
import { formatAttemptId, formatResidueName } from "../src/attempt-path.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { noProofReseed, recordingLogger, testGitCacheOptions } from "./helpers.js";

const WARNING = "runner clone retention: automatic reclaim disabled";
type Observation = { count: number; complete: boolean };
type Diagnostic = { msg: string; key: string; attempts: Observation; residue: Observation; orphanedSkills: Observation };
let fx: Fixture;
beforeEach(() => { fx = makeFixture(); });
afterEach(() => fx.cleanup());

function cache(protection?: (runId: string) => Promise<boolean>) {
  const rec = recordingLogger();
  const git = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({
    gitleaksBin: defaultGitleaksShim(), terminalRecordProtection: protection,
    retentionDelete: { split: true, run: async (_command, args) => {
      const target = args.at(-1)!;
      assert.ok(!path.basename(target).startsWith(".uzi-") && !target.includes(".attempt-"), "diagnostics must never delete retained artifacts");
      fs.rmSync(target, { recursive: true, force: true });
    } },
  }));
  return { git, warnings: () => rec.lines.filter(l => (l as { msg?: string }).msg === WARNING) as Diagnostic[] };
}
function config(bare: string, ...args: string[]): string {
  return execFileSync("git", ["-C", bare, "config", "--local", ...args], { encoding: "utf8", env: {
    ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
  } });
}
function id(n: number): string {
  return formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, n)), 1, n.toString(16).padStart(16, "0"));
}
function attempt(bare: string, canonical: string, n: number, state: AttemptLedgerState): string {
  const p = `${canonical}.attempt-${id(n)}`;
  fs.mkdirSync(p, { recursive: true });
  fs.writeFileSync(path.join(p, "sentinel"), `attempt-${n}`);
  config(bare, "--add", "uzi-attempts.agent/retention.entry", JSON.stringify({ attemptId: id(n), clonePath: p, runId: `run-${n}`, state }));
  return p;
}
async function seed(git: GitCache, bare: string, key: string, live: (p: string) => boolean = () => false) {
  return git.runnerCloneForBranch(bare, "agent/retention", key, noProofReseed, "seed", false, undefined, {
    attemptId: mintAttemptId(2), isLive: live, beforeSeed: async () => {},
  });
}
function residue(canonical: string, n: number): string {
  const p = path.join(path.dirname(canonical), formatResidueName(path.basename(canonical), `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`));
  fs.mkdirSync(p, { recursive: true });
  fs.writeFileSync(path.join(p, "sentinel"), "residue");
  return p;
}

describe("#1858 retention diagnostic observations", () => {
  it("uses strict thresholds, counts protected abandoned attempts and excludes seeding/live/journal/custody identities", async () => {
    const { git, warnings } = cache();
    const b = await git.ensureClone(fx.originPath);
    const canonical = git.runnerClonePath(b, "retention");
    const abandoned = [0, 1, 2].map(n => attempt(b, canonical, n, "abandoned"));
    const live = attempt(b, canonical, 3, "live");
    const journaled = attempt(b, canonical, 4, "live");
    config(b, "uzi-recovery.agent/other.clone", JSON.stringify({ runId: "run-4", clonePath: journaled }));
    attempt(b, canonical, 5, "reclaimed");
    attempt(b, canonical, 6, "retired");
    fs.mkdirSync(path.join(fx.dataDir, "recovery", "run-0"), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery", "run-0", "capture.json"), "{}");
    const residues = Array.from({ length: 5 }, (_, n) => residue(canonical, n));
    await seed(git, b, "retention", p => p === live);
    assert.equal(warnings().length, 0);
    // The previous seed is a crash-window live orphan now: count it, but not the new seed.
    await seed(git, b, "retention", p => p === live);
    assert.equal(warnings().length, 1);
    assert.deepEqual(warnings()[0]!.attempts, { count: 4, complete: true });
    assert.deepEqual(warnings()[0]!.residue, { count: 5, complete: true });
    assert.deepEqual(warnings()[0]!.orphanedSkills, { count: 0, complete: true });
    for (const [n, p] of abandoned.entries()) assert.equal(fs.readFileSync(path.join(p, "sentinel"), "utf8"), `attempt-${n}`);
    assert.ok(residues.every(p => fs.readFileSync(path.join(p, "sentinel"), "utf8") === "residue"));
  });

  for (const kind of ["physical protection", "callback failure", "custody", "unreadable custody"] as const) {
    it(`${kind} uses the unchanged fail-closed predicate once and marks crash-window classification partial`, async t => {
      let calls = 0;
      const { git, warnings } = cache(async () => {
        calls++;
        if (kind === "callback failure") throw new Error("unavailable");
        return kind === "physical protection";
      });
      const b = await git.ensureClone(fx.originPath);
      const canonical = git.runnerClonePath(b, "retention");
      const p = attempt(b, canonical, 0, "live");
      for (let n = 0; n < 6; n++) residue(canonical, n);
      if (kind === "custody") {
        fs.mkdirSync(path.join(fx.dataDir, "recovery", "run-0"), { recursive: true });
        fs.writeFileSync(path.join(fx.dataDir, "recovery", "run-0", "capture.json"), "{}");
      }
      const original = fs.promises.readdir.bind(fs.promises);
      if (kind === "unreadable custody") t.mock.method(fs.promises, "readdir", async (...args: Parameters<typeof fs.promises.readdir>) => {
        if (args[0] === path.join(fx.dataDir, "recovery", "run-0")) throw new Error("EACCES");
        return original(...args);
      });
      await seed(git, b, "retention");
      assert.equal(calls, 1, "no second diagnostic protection callback");
      assert.deepEqual(warnings()[0]!.attempts, { count: 0, complete: false });
      assert.equal(fs.readFileSync(path.join(p, "sentinel"), "utf8"), "attempt-0");
    });
  }

  it("counts orphan skills only for absent, non-live, non-journaled, non-reclaimed attempts", async () => {
    const { git, warnings } = cache();
    const b = await git.ensureClone(fx.originPath);
    const canonical = git.runnerClonePath(b, "retention");
    const skills: string[] = [];
    for (let n = 0; n < 7; n++) {
      const p = n === 0 ? `${canonical}.attempt-${id(n)}` : attempt(b, canonical, n, n === 1 ? "abandoned" : n === 2 ? "retired" : n === 3 ? "live" : n === 4 ? "reclaimed" : "abandoned");
      if (n > 0 && n < 6) fs.rmSync(p, { recursive: true });
      if (n === 3 || n === 4 || n === 5) config(b, `uzi-recovery.agent/other-${n}.clone`, JSON.stringify({ runId: `run-${n}`, clonePath: p }));
      const sibling = path.join(path.dirname(p), `.uzi-skills-${path.basename(p)}`);
      fs.mkdirSync(sibling, { recursive: true }); fs.writeFileSync(path.join(sibling, "sentinel"), "skills"); skills.push(sibling);
    }
    await seed(git, b, "retention");
    assert.deepEqual(warnings()[0]!.orphanedSkills, { count: 3, complete: true });
    assert.ok(skills.every(p => fs.readFileSync(path.join(p, "sentinel"), "utf8") === "skills"));
  });

  it("retains ledger identities across repeated seeds and a new GitCache", async () => {
    const first = cache();
    const b = await first.git.ensureClone(fx.originPath);
    const canonical = first.git.runnerClonePath(b, "retention");
    const paths = Array.from({ length: 6 }, (_, n) => attempt(b, canonical, n, n % 2 ? "live" : "abandoned"));
    for (const g of [first.git, first.git, cache().git]) await seed(g, b, "retention");
    const entries = config(b, "--get-all", "uzi-attempts.agent/retention.entry").trim().split("\n").map(s => JSON.parse(s) as { clonePath: string; state: string });
    for (const [n, p] of paths.entries()) {
      assert.equal(entries.find(e => e.clonePath === p)?.state, n % 2 ? "live" : "abandoned");
      assert.equal(fs.readFileSync(path.join(p, "sentinel"), "utf8"), `attempt-${n}`);
    }
  });

  it("rate limits for one hour without resetting below cap, and restarts may warn", async t => {
    let now = Date.now();
    t.mock.method(Date, "now", () => now);
    const { git, warnings } = cache();
    const b = await git.ensureClone(fx.originPath);
    const canonical = git.runnerClonePath(b, "retention");
    fs.mkdirSync(path.dirname(canonical), { recursive: true });
    const paths = Array.from({ length: 6 }, (_, n) => residue(canonical, n));
    await git.runnerCloneForBranch(b, "agent/retention", "retention", noProofReseed);
    now += 3599_999;
    await git.runnerCloneForBranch(b, "agent/retention", "retention", noProofReseed);
    assert.equal(warnings().length, 1);
    fs.rmSync(paths[0]!, { recursive: true });
    await git.runnerCloneForBranch(b, "agent/retention", "retention", noProofReseed);
    residue(canonical, 0);
    await git.runnerCloneForBranch(b, "agent/retention", "retention", noProofReseed);
    assert.equal(warnings().length, 1);
    now++;
    await git.runnerCloneForBranch(b, "agent/retention", "retention", noProofReseed);
    assert.equal(warnings().length, 2);
    const restarted = cache();
    await restarted.git.runnerCloneForBranch(b, "agent/retention", "retention", noProofReseed);
    assert.equal(restarted.warnings().length, 1);
  });

  it("bounds diagnostic LRU at 128 full canonical keys, touching a key preserves it while eviction may warn", async () => {
    const { git, warnings } = cache();
    const b = await git.ensureClone(fx.originPath);
    const diagnose = (key: string) => (git as unknown as {
      diagnoseRetainedArtifacts: (bare: string, canonical: string, seeding: undefined, predicates: { isLive: (p: string) => boolean }) => Promise<void>;
    }).diagnoseRetainedArtifacts(b, git.runnerClonePath(b, key), undefined, noProofReseed);
    for (let n = 0; n < 129; n++) {
      const key = `retention-${n}`;
      const canonical = git.runnerClonePath(b, key);
      fs.mkdirSync(path.dirname(canonical), { recursive: true });
      for (let r = 0; r < 6; r++) residue(canonical, r);
      if (n === 0) {
        await git.runnerCloneForBranch(b, "agent/retention", key, noProofReseed);
        assert.equal(warnings().length, 1, "real public seed produces the first diagnostic");
      } else await diagnose(key);
      if (n === 127) await diagnose("retention-0");
    }
    assert.equal(warnings().length, 129);
    await diagnose("retention-0");
    assert.equal(typeof (warnings()[0] as unknown as { retainedUndeletable?: unknown }).retainedUndeletable, "number", "#3153: the warning carries the failed-removal-attempt counter");
    assert.equal(warnings().length, 129, "touched key remains cached");
    await diagnose("retention-1");
    assert.equal(warnings().length, 130, "evicted key may warn again");
  });

  it("truncates logged keys while keeping distinct full canonical keys for rate limiting", async () => {
    const { git, warnings } = cache();
    const b = await git.ensureClone(fx.originPath);
    for (const suffix of ["a", "b"]) {
      const key = "k".repeat(180) + suffix;
      const canonical = git.runnerClonePath(b, key);
      fs.mkdirSync(path.dirname(canonical), { recursive: true });
      for (let n = 0; n < 6; n++) residue(canonical, n);
      await git.runnerCloneForBranch(b, "agent/retention", key, noProofReseed);
    }
    assert.equal(warnings().length, 2);
    for (const line of warnings()) { assert.ok(line.key.length <= 160); assert.ok(!/[\r\n]/.test(line.key)); }
  });
});
