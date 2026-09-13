import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { GitCache } from "../src/git.js";

// issue #1308 (m1-m3) — the #1197 recovery-capture journal (worker-owned bare config,
// key `uzi-recovery.<branch>.clone`, value JSON `{runId, clonePath}`) used to brick every
// later run on a branch: terminal cleanup cleared the journal ONLY when the clone rm
// succeeded, so an ENOTEMPTY/EBUSY leftover left the journal pointing at a run that would
// never come back. These tests drive the git-layer fix directly with REAL git (no mocks
// beyond a spied/overridden `removeRunnerClone`):
//   - removeRunnerCloneAndReleaseRecovery: releases the journal EVEN WHEN the delete
//     fails, but ONLY when it still exactly names (runId, clonePath) — a same-key
//     successor's live reclaim must never be destroyed by a stale predecessor's cleanup.
//   - reclaimTerminalRecoveryClone: the runner's self-heal primitive — same exact-owner
//     CAS, plus a containment check so a corrupted/tampered journal naming a path outside
//     the runner root is never deleted (though the journal is still released).
//
// `refs/uzi-runner/<branch>` (the durable off-PVC recovery artifact) is asserted UNCHANGED
// throughout — this whole mechanism is about the runner-owned CLONE and its journal, never
// the worker-side tracking ref.

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV }).trim();
}

function recoveryConfigKey(branch: string): string {
  return `uzi-recovery.${branch}.clone`;
}

/** Read the journal's raw config value with plain git (not the private git.ts reader), so
 *  the assertion is independent of the code under test. `undefined` when the key is
 *  entirely absent (git config --get exits 1); `""` when it was cleared (present, empty). */
function readJournalRaw(bare: string, branch: string): string | undefined {
  try {
    const out = execFileSync(
      "git",
      ["-C", bare, "config", "--local", "--get", recoveryConfigKey(branch)],
      { encoding: "utf8", env: GIT_ENV },
    );
    return out.replace(/\n$/, "");
  } catch (e) {
    const err = e as NodeJS.ErrnoException & { status?: number };
    if (err.status === 1) return undefined;
    throw e;
  }
}

function assertJournalCleared(bare: string, branch: string, msg?: string): void {
  const raw = readJournalRaw(bare, branch);
  assert.ok(
    raw === undefined || raw === "",
    msg ?? `expected the journal for ${branch} to be cleared, got ${JSON.stringify(raw)}`,
  );
}

function assertJournalNames(
  bare: string,
  branch: string,
  runId: string,
  clonePath: string,
  msg?: string,
): void {
  const raw = readJournalRaw(bare, branch);
  assert.ok(raw, `expected a live journal entry for ${branch}, got ${JSON.stringify(raw)}`);
  assert.deepStrictEqual(JSON.parse(raw!), { runId, clonePath }, msg);
}

let fx: Fixture;
let git: GitCache;

beforeEach(() => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger());
});

afterEach(() => fx.cleanup());

describe("removeRunnerCloneAndReleaseRecovery (issue #1308 m1/m2)", () => {
  it("releases the owner's journal even when removeRunnerClone throws ENOTEMPTY, leaving the leftover dir on disk", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const branch = "agent/issue-9001";
    const runId = "10000000-0000-4000-8000-000000000001";
    const clonePath = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-recover-p1-"));
    try {
      fs.writeFileSync(path.join(clonePath, "marker.txt"), "still here\n");
      await git.markRecoveryCapture(bare, clonePath, branch, runId);
      assertJournalNames(bare, branch, runId, clonePath, "precondition: the journal names this run");

      let removeCalls = 0;
      git.removeRunnerClone = (async () => {
        removeCalls++;
        const err: NodeJS.ErrnoException = new Error("ENOTEMPTY: directory not empty");
        err.code = "ENOTEMPTY";
        throw err;
      }) as typeof git.removeRunnerClone;

      await git.removeRunnerCloneAndReleaseRecovery(bare, clonePath, branch, runId);

      assert.strictEqual(removeCalls, 1, "the removal was attempted exactly once");
      assertJournalCleared(bare, branch, "a terminal run's journal is released EVEN THOUGH the delete failed");
      assert.strictEqual(fs.existsSync(clonePath), true, "the leftover directory survives the failed rm — this is the #1197 bricking shape, minus the bricking");
    } finally {
      fs.rmSync(clonePath, { recursive: true, force: true });
    }
  });

  it("never destroys a same-key SUCCESSOR's live clone/journal (exact-owner CAS on both the delete and the clear)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const branch = "agent/issue-9002";
    const run1 = "10000000-0000-4000-8000-000000000002";
    const run2 = "10000000-0000-4000-8000-000000000003";
    const successorClone = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-recover-p2-"));
    try {
      fs.writeFileSync(path.join(successorClone, "live.txt"), "run2's live work\n");
      // run2 reclaimed the journal and reseeded its OWN clone at this path BEFORE run1's
      // delayed terminal cleanup runs — the exact race the CAS guard exists for.
      await git.markRecoveryCapture(bare, successorClone, branch, run2);

      const originalRemove = git.removeRunnerClone.bind(git);
      let removeCalls = 0;
      git.removeRunnerClone = (async (p: string) => {
        removeCalls++;
        return originalRemove(p);
      }) as typeof git.removeRunnerClone;

      // run1's terminal cleanup — its OWN old clonePath happened to be this same path
      // (same branch, same on-disk key), but the journal no longer names run1.
      await git.removeRunnerCloneAndReleaseRecovery(bare, successorClone, branch, run1);

      assert.strictEqual(removeCalls, 0, "a mismatched owner must never even attempt the delete");
      assert.strictEqual(fs.existsSync(successorClone), true, "the successor's live clone survives");
      assert.strictEqual(fs.existsSync(path.join(successorClone, "live.txt")), true);
      assertJournalNames(bare, branch, run2, successorClone, "the successor's journal entry is byte-for-byte untouched");
    } finally {
      fs.rmSync(successorClone, { recursive: true, force: true });
    }
  });
});

describe("reclaimTerminalRecoveryClone (issue #1308 m2 self-heal primitive)", () => {
  it("deletes the terminal owner's clone, clears the journal, and never touches refs/uzi-runner/<branch>", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const branch = "agent/issue-9003";
    const owner = "20000000-0000-4000-8000-000000000001";
    const ownerClone = path.join(fx.dataDir, "runner", "reclaim-happy", "issue-9003");
    fs.mkdirSync(ownerClone, { recursive: true });
    fs.writeFileSync(path.join(ownerClone, "owner.txt"), "owner clone\n");
    await git.markRecoveryCapture(bare, ownerClone, branch, owner);

    const trackingRef = `refs/uzi-runner/${branch}`;
    const sha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    gitIn(bare, ["update-ref", trackingRef, sha]);

    await git.reclaimTerminalRecoveryClone(bare, branch, owner, ownerClone);

    assert.strictEqual(fs.existsSync(ownerClone), false, "the terminal owner's clone is actually deleted");
    assertJournalCleared(bare, branch);
    assert.strictEqual(
      gitIn(bare, ["rev-parse", trackingRef]),
      sha,
      "the durable off-PVC tracking ref is untouched by a clone-level reclaim",
    );
  });

  it("is a no-op when the journal no longer names EXACTLY the given (ownerRunId, ownerClonePath)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const branch = "agent/issue-9004";
    const owner = "20000000-0000-4000-8000-000000000002";
    const ownerClone = path.join(fx.dataDir, "runner", "reclaim-cas", "issue-9004");
    fs.mkdirSync(ownerClone, { recursive: true });
    await git.markRecoveryCapture(bare, ownerClone, branch, owner);

    const differentPath = path.join(fx.dataDir, "runner", "reclaim-cas", "not-the-real-owner-clone");
    await git.reclaimTerminalRecoveryClone(bare, branch, owner, differentPath);

    assert.strictEqual(fs.existsSync(ownerClone), true, "the real owner clone is untouched by a mismatched-path reclaim call");
    assertJournalNames(bare, branch, owner, ownerClone, "a CAS mismatch leaves the journal exactly as it was");
  });

  it("never deletes a recorded clone path OUTSIDE the runner root, but still releases the journal (deletion never gates the clear)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const branch = "agent/issue-9005";
    const owner = "20000000-0000-4000-8000-000000000003";
    const outsidePath = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-recover-outside-"));
    try {
      fs.writeFileSync(path.join(outsidePath, "sentinel.txt"), "must survive\n");
      // A corrupted/tampered journal — or an old worker layout — naming a path elsewhere
      // on disk. markRecoveryCapture itself never validates containment, so this is a
      // realistic shape for the read side to defend against.
      await git.markRecoveryCapture(bare, outsidePath, branch, owner);

      await git.reclaimTerminalRecoveryClone(bare, branch, owner, outsidePath);

      assert.strictEqual(fs.existsSync(outsidePath), true, "containment blocks the delete of a path outside runnerRoot");
      assert.strictEqual(fs.existsSync(path.join(outsidePath, "sentinel.txt")), true);
      assertJournalCleared(bare, branch, "the journal is still released — containment gates the DELETE, not the clear");
    } finally {
      fs.rmSync(outsidePath, { recursive: true, force: true });
    }
  });
});
