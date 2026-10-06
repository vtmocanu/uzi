import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache } from "../src/git.js";
import { Outbox } from "../src/outbox.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import os from "node:os";
import { realpathSync } from "node:fs";

const scratch = realpathSync(os.tmpdir());
const run = "11111111-1111-4111-8111-111111111111";
test("bad MAC retains attempts above the limit, every ledger state, and terminal clone; control deletes", async () => {
  const root = fs.mkdtempSync(path.join(scratch, "terminal-retention-"));
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  const cmd = (dir: string, ...args: string[]) => execFileSync("git", ["-C", dir, ...args], { env, encoding: "utf8", stdio: "pipe" });
  const log = nullLogger();
  const box = new Outbox({ root: path.join(root, "outbox"), log, runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 1 });
  try {
    const origin = path.join(root, "origin");
    fs.mkdirSync(origin);
    cmd(origin, "init", "-b", "main");
    cmd(origin, "config", "maintenance.auto", "false");
    cmd(origin, "config", "gc.auto", "0");
    cmd(origin, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "seed");
    await box.init();
    await box.journalTerminal(run, 3, "implement", 0, { status: "failed", error: "original" });
    const terminal = path.join(root, "outbox", run, "terminal-3.json");
    const bad = JSON.parse(fs.readFileSync(terminal, "utf8"));
    bad.body = { status: "failed", error: "tampered" };
    fs.writeFileSync(terminal, JSON.stringify(bad));
    assert.equal((await box.observeTerminalAuthentication(run, 3)).kind, "mac_failure");
    const git = new GitCache(path.join(root, "data"), log, undefined, testGitCacheOptions({
      terminalRecordProtection: (id) => box.hasPhysicalTerminalProtection(id),
    }));
    const bare = await git.ensureClone(origin);
    const branch = "agent/issue-7";
    const canonical = git.runnerClonePath(bare, "issue-7");
    const ledgerKey = `uzi-attempts.${branch}.entry`;
    fs.mkdirSync(path.dirname(canonical), { recursive: true });
    const retained: string[] = [];
    for (let i = 0; i < 8; i++) {
      const id = formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, i)), 1, (i + 1).toString(16).padStart(16, "0"));
      const dir = `${canonical}.attempt-${id}`;
      const skills = path.join(path.dirname(dir), `.uzi-skills-${path.basename(dir)}`);
      const protectedRun = i < 4 ? run : `control-${i}`;
      fs.mkdirSync(dir); fs.mkdirSync(skills);
      fs.writeFileSync(path.join(dir, "WORK"), "last copy");
      if (i < 4) retained.push(dir, skills);
      cmd(bare, "config", "--add", ledgerKey, JSON.stringify({ attemptId: id, runId: protectedRun, clonePath: dir, state: "abandoned" }));
    }
    for (const [i, state] of ["live", "abandoned", "reclaimed", "retired"].entries()) {
      const id = formatAttemptId(new Date(Date.UTC(2026, 0, 2, 0, 0, i)), 1, (i + 20).toString(16).padStart(16, "0"));
      cmd(bare, "config", "--add", ledgerKey, JSON.stringify({ attemptId: id, runId: run, clonePath: `${canonical}.attempt-${id}`, state }));
    }
    const seeded = await git.runnerCloneForBranch(bare, branch, "issue-7", noProofReseed, "new-run", false, undefined, {
      attemptId: mintAttemptId(9), isLive: () => false, beforeSeed: async () => {}, quiescent: async () => true,
    });
    for (const p of retained) assert.ok(fs.existsSync(p), p);
    const entries = cmd(bare, "config", "--get-all", ledgerKey).trim().split("\n").map((v) => JSON.parse(v));
    assert.equal(entries.filter((e) => e.runId === run).length, 8);
    assert.ok(entries.some((e) => e.runId.startsWith("control-") && e.state === "retired"), "control deletion ran");
    await git.markRecoveryCapture(bare, seeded.path, branch, run);
    await assert.rejects(git.retireRunnerClone(bare, seeded.path, branch, run, { discard: true }), /terminal record custody/);
    assert.ok(fs.existsSync(seeded.path));
    assert.equal(JSON.parse(cmd(bare, "config", "--get", `uzi-recovery.${branch}.clone`)).runId, run);
    await assert.rejects(git.removeRunnerClone(seeded.path, run), /terminal record custody/);
    fs.unlinkSync(terminal);
    await git.retireRunnerClone(bare, seeded.path, branch, run, { discard: true });
    assert.equal(fs.existsSync(seeded.path), false);
  } finally {
    await box.closeTerminalObservationScan();
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("protection callback errors retain no-bare clones", async () => {
  const root = fs.mkdtempSync(path.join(scratch, "terminal-error-"));
  try {
    const git = new GitCache(root, nullLogger(), undefined, testGitCacheOptions({ terminalRecordProtection: async () => { throw new Error("unavailable"); } }));
    const clone = path.join(root, "clone");
    fs.mkdirSync(clone);
    await assert.rejects(git.removeRunnerClone(clone, run), /terminal record custody/);
    assert.ok(fs.existsSync(clone));
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
});
