import { afterEach, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { noProofReseed } from "./helpers.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { CloneRetainedByQuarantineError, type AttemptSeedOptions } from "../src/git.js";
import { fx, git, installHarness } from "./runner-harness.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";

// issue #2213 — a worker residue quarantine that latches DURING the awaits inside the git release
// methods (bare lock, journal read, protection read) must still keep the failed run's clone and
// journal: the latch is re-read synchronously immediately before each destructive step.

installHarness();
afterEach(() => {
  mock.restoreAll();
  resetResidueQuarantineForTests();
});

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const owner = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const holdingRoot = (): string => path.join(fx.dataDir, "runner-quarantine");
const latch = (): void => latchResidueQuarantine({ cause: "test latch", runId: owner, site: "test" });

function config(bare: string, key: string): string[] {
  try {
    return execFileSync("git", ["-C", bare, "config", "--local", "--get-all", key], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" })
      .trim().split("\n").filter(Boolean);
  } catch {
    return [];
  }
}
const journal = (bare: string, branch: string): { runId: string; clonePath: string } | undefined => {
  const raw = config(bare, `uzi-recovery.${branch}.clone`).at(-1);
  return raw ? (JSON.parse(raw) as { runId: string; clonePath: string }) : undefined;
};

async function seed(iid: number): Promise<{ bare: string; branch: string; clonePath: string }> {
  const bare = await git.ensureClone(fx.originPath);
  const branch = `agent/issue-${iid}`;
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, owner);
  fs.writeFileSync(path.join(clone.path, "OWNER.txt"), "owner bytes\n");
  await git.markRecoveryCapture(bare, clone.path, branch, owner);
  return { bare, branch, clonePath: clone.path };
}

/** Latch the quarantine as the Nth readRecoveryCapture call RETURNS (so it lands during the awaits). */
function latchOnRead(n: number): void {
  const target = git as unknown as { readRecoveryCapture: (...a: unknown[]) => Promise<unknown> };
  const orig = target.readRecoveryCapture.bind(git);
  let calls = 0;
  target.readRecoveryCapture = async (...a: unknown[]) => {
    const out = await orig(...a);
    if (++calls === n) latch();
    return out;
  };
}

const holdingEntries = (): string[] => (fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : []);

describe("worker residue quarantine latching inside the git release methods (#2213)", () => {
  it("control: an unlatched retire moves the clone, clears the journal and disposes the holding dir", async () => {
    const s = await seed(2201);
    await git.retireRunnerClone(s.bare, s.clonePath, s.branch, owner, { discard: true });
    assert.equal(fs.existsSync(s.clonePath), false);
    assert.equal(journal(s.bare, s.branch), undefined);
    assert.deepEqual(holdingEntries(), []);
  });

  it("retire: a latch during the journal read leaves the clone in place and the journal intact", async () => {
    const s = await seed(2202);
    latchOnRead(1);
    await assert.rejects(() => git.retireRunnerClone(s.bare, s.clonePath, s.branch, owner, { discard: true }), /residue quarantine latched/);
    assert.equal(fs.readFileSync(path.join(s.clonePath, "OWNER.txt"), "utf8"), "owner bytes\n");
    assert.deepEqual(journal(s.bare, s.branch), { runId: owner, clonePath: s.clonePath });
    assert.deepEqual(holdingEntries(), []);
  });

  it("retire: a latch during the post-move journal re-read moves the clone back and keeps the journal", async () => {
    const s = await seed(2203);
    latchOnRead(2);
    await assert.rejects(() => git.retireRunnerClone(s.bare, s.clonePath, s.branch, owner, { discard: true }), /residue quarantine latched/);
    assert.equal(fs.readFileSync(path.join(s.clonePath, "OWNER.txt"), "utf8"), "owner bytes\n");
    assert.deepEqual(journal(s.bare, s.branch), { runId: owner, clonePath: s.clonePath });
    assert.deepEqual(holdingEntries(), [], "the moved clone is not left (or disposed) in the holding dir");
  });

  it("removeRunnerClone: a latch during the protection read keeps the clone", async () => {
    const s = await seed(2204);
    const target = git as unknown as { hasPhysicalTerminalProtection: (id: string) => Promise<boolean> };
    const orig = target.hasPhysicalTerminalProtection.bind(git);
    target.hasPhysicalTerminalProtection = async (id: string) => {
      const out = await orig(id);
      latch();
      return out;
    };
    await assert.rejects(() => git.removeRunnerClone(s.clonePath, owner), /residue quarantine latched/);
    assert.ok(fs.existsSync(path.join(s.clonePath, "OWNER.txt")));
  });

  it("a latch during pinned teardown preparation keeps the standalone clone", async () => {
    const s = await seed(22992);
    const origOpen = fsp.open.bind(fsp);
    let fired = false;
    mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
      const handle = await origOpen(...args);
      if (!fired && String(args[0]) === path.dirname(s.clonePath)) {
        fired = true;
        latch();
      }
      return handle;
    });
    await git.removeRunnerClone(s.clonePath, owner).catch(() => undefined);
    assert.equal(fired, true, "latch fired during the pinned removal's parent open");
    assert.equal(fs.existsSync(path.join(s.clonePath, "OWNER.txt")), true, "the latched clone must remain");
  });

  it("retire: a latch during the journal read means the clone is never moved (pre-rename recheck)", async () => {
    const s = await seed(2206);
    const renames: string[] = [];
    const origRename = fsp.rename.bind(fsp);
    mock.method(fsp, "rename", async (from: string, to: string) => {
      renames.push(String(from));
      return origRename(from, to);
    });
    latchOnRead(1);
    await assert.rejects(() => git.retireRunnerClone(s.bare, s.clonePath, s.branch, owner, { discard: true }), CloneRetainedByQuarantineError);
    assert.deepEqual(renames.filter((f) => f === s.clonePath), [], "the clone was never renamed, not even moved and moved back");
    assert.ok(fs.existsSync(path.join(s.clonePath, "OWNER.txt")));
  });

  it("retire: a latch during the post-lock protection read keeps the holding dir (no disposal)", async () => {
    const s = await seed(2207);
    const target = git as unknown as { hasPhysicalTerminalProtection: (id: string) => Promise<boolean> };
    const orig = target.hasPhysicalTerminalProtection.bind(git);
    let calls = 0;
    target.hasPhysicalTerminalProtection = async (id: string) => {
      const out = await orig(id);
      // Call 1 is the in-lock check (discard); call 2 is the step-6 disposal check.
      if (++calls === 2) latch();
      return out;
    };
    const disposition = await git.retireRunnerClone(s.bare, s.clonePath, s.branch, owner, { discard: true });
    assert.equal(calls, 2);
    assert.equal(disposition, "quarantined");
    assert.equal(fs.existsSync(s.clonePath), false);
    assert.equal(journal(s.bare, s.branch), undefined);
    const held = holdingEntries();
    assert.equal(held.length, 1, "the latched worker keeps the holding dir");
    assert.equal(fs.readFileSync(path.join(holdingRoot(), held[0]!, "OWNER.txt"), "utf8"), "owner bytes\n");
  });

  /** An attempt-path clone (journal carries the attemptId), as a Docker-wired worker seeds it. */
  async function seedAttempt(iid: number): Promise<{ bare: string; branch: string; clonePath: string; attemptId: string }> {
    const bare = await git.ensureClone(fx.originPath);
    const branch = `agent/issue-${iid}`;
    const attemptId = mintAttemptId(1);
    const seedOpts: AttemptSeedOptions = { attemptId, isLive: () => false, beforeSeed: async () => {}, quiescent: async () => true };
    const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, owner, false, undefined, seedOpts);
    fs.writeFileSync(path.join(clone.path, "OWNER.txt"), "owner bytes\n");
    await git.markRecoveryCapture(bare, clone.path, branch, owner, clone.attemptId);
    assert.equal(clone.attemptId, attemptId);
    return { bare, branch, clonePath: clone.path, attemptId };
  }
  const attemptJournal = (s: { bare: string; branch: string; clonePath: string; attemptId: string }) => ({
    runId: owner,
    clonePath: s.clonePath,
    attemptId: s.attemptId,
  });

  it("releaseAttemptInPlace: a latch during the journal read throws CloneRetainedByQuarantineError, keeps the journal, writes no ledger entry", async () => {
    const s = await seedAttempt(2205);
    const ledgerBefore = config(s.bare, `uzi-attempts.${s.branch}.entry`);
    latchOnRead(1);
    await assert.rejects(() => git.releaseAttemptInPlace(s.bare, s.clonePath, s.branch, owner, "abandoned"), CloneRetainedByQuarantineError);
    assert.deepEqual(journal(s.bare, s.branch), attemptJournal(s));
    assert.deepEqual(config(s.bare, `uzi-attempts.${s.branch}.entry`), ledgerBefore);
    assert.equal(ledgerBefore.some((e) => e.includes('"abandoned"')), false);
    assert.ok(fs.existsSync(path.join(s.clonePath, "OWNER.txt")));
  });

  it("releaseAttemptInPlace: a latch during the ledger append throws CloneRetainedByQuarantineError (not a journal-stage release error) and keeps the journal", async () => {
    const s = await seedAttempt(2208);
    const target = git as unknown as { appendAttemptLedger: (...a: unknown[]) => Promise<void> };
    const orig = target.appendAttemptLedger.bind(git);
    target.appendAttemptLedger = async (...a: unknown[]) => {
      await orig(...a);
      latch();
    };
    await assert.rejects(
      () => git.releaseAttemptInPlace(s.bare, s.clonePath, s.branch, owner, "abandoned"),
      (err: unknown) => {
        assert.ok(err instanceof CloneRetainedByQuarantineError, `got ${String(err)}`);
        return true;
      },
    );
    assert.deepEqual(journal(s.bare, s.branch), attemptJournal(s));
  });
});
