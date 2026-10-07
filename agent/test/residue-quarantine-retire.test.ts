import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { noProofReseed } from "./helpers.js";
import { fx, git, installHarness } from "./runner-harness.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";

// issue #2213 — a worker residue quarantine that latches DURING the awaits inside the git release
// methods (bare lock, journal read, protection read) must still keep the failed run's clone and
// journal: the latch is re-read synchronously immediately before each destructive step.

installHarness();
afterEach(() => resetResidueQuarantineForTests());

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

  it("releaseAttemptInPlace: a latch during the journal read throws, keeps the journal and records no ledger entry", async () => {
    const s = await seed(2205);
    latchOnRead(1);
    await assert.rejects(() => git.releaseAttemptInPlace(s.bare, s.clonePath, s.branch, owner, "abandoned"), /residue quarantine latched/);
    assert.deepEqual(journal(s.bare, s.branch), { runId: owner, clonePath: s.clonePath });
    assert.deepEqual(config(s.bare, `uzi-attempts.${s.branch}.entry`), []);
    assert.ok(fs.existsSync(path.join(s.clonePath, "OWNER.txt")));
  });
});
