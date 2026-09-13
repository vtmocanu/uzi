import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { ForeignCaptureBlockedError, StaleCaptureJournalError } from "../src/git.js";
import { type ExecutorFactory } from "../src/runner.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  homeDir,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

// issue #1315 — atomic runner-clone RELEASE (retireRunnerClone). The terminal cleanup
// bug was non-atomic: a recursive `fs.rm` racing a `git maintenance`/`fsmonitor` daemon
// threw ENOTEMPTY, the journal-clear never ran, and a later foreign run wedged forever.
// The fix renames the clone to a worker-only holding location (rename(2) beats the race)
// and only THEN clears the journal. These tests pin the guard classification, the
// runner's owner-probe-and-dispose, and every fail-closed edge of retireRunnerClone.

installHarness();

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};

/** Read the worker-owned recovery journal directly (bare git config). Returns undefined
 *  when the key is absent or was cleared to an empty value, mirroring readRecoveryCapture. */
function readJournal(bare: string, branch: string): { runId: string; clonePath: string } | undefined {
  let raw = "";
  try {
    raw = execFileSync("git", ["-C", bare, "config", "--local", "--get", `uzi-recovery.${branch}.clone`], {
      env: GIT_ENV,
      encoding: "utf8",
      stdio: "pipe",
    }).trim();
  } catch {
    return undefined; // git config exits 1 when the key is absent
  }
  if (!raw) return undefined;
  return JSON.parse(raw) as { runId: string; clonePath: string };
}

const holdingRoot = (): string => path.join(fx.dataDir, "runner-quarantine");
const runnerRoot = (): string => path.join(fx.dataDir, "runner");

/** Seed a clone at the branch's canonical path, drop an owner-only marker byte, and
 *  journal (ownerRunId, canonicalPath): a run whose terminal cleanup crashed mid-release
 *  and left the journal pointing at retained residue. */
async function seedResidue(
  iid: number,
  ownerRunId: string,
  marker: string,
): Promise<{ bare: string; branch: string; clonePath: string }> {
  const bare = await git.ensureClone(fx.originPath);
  const branch = `agent/issue-${iid}`;
  const clone = await git.createOrAttachRunnerClone(bare, iid, ownerRunId);
  fs.writeFileSync(path.join(clone.path, marker), "owner-only bytes\n");
  await git.markRecoveryCapture(bare, clone.path, branch, ownerRunId);
  return { bare, branch, clonePath: clone.path };
}

describe("atomic runner-clone release (#1315)", () => {
  it("T2: a TERMINAL foreign owner is quarantined (retained) and the reclaiming run seeds fresh", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1401;
    const ownerRunId = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const foreignRunId = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN_RESIDUE.txt");
    api.setOwnershipStatus(ownerRunId, "completed"); // authoritatively terminal

    let observed:
      | { worktree: string; freshExists: boolean; residueGone: boolean; journal: ReturnType<typeof readJournal>; quarantineHasResidue: boolean }
      | undefined;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, foreignRunId),
      executor: {
        run: async (ctx) => {
          // phaseClone has quarantined the foreign residue and reseeded a FRESH clone.
          const dirs = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
          observed = {
            worktree: ctx.worktreePath,
            freshExists: fs.existsSync(ctx.worktreePath),
            residueGone: !fs.existsSync(path.join(ctx.worktreePath, "FOREIGN_RESIDUE.txt")),
            journal: readJournal(bare, branch),
            quarantineHasResidue: dirs.some((d) => fs.existsSync(path.join(holdingRoot(), d, "FOREIGN_RESIDUE.txt"))),
          };
          throw new Error("stop after phaseClone reseed");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    await runner.execute(gitlabClaim(iid, { run_id: foreignRunId }));

    assert.ok(observed, "executor.run was reached, so phaseClone reseeded");
    assert.equal(observed!.worktree, clonePath, "the reseed lands at the canonical clone path");
    assert.equal(observed!.freshExists, true);
    assert.equal(observed!.residueGone, true, "the fresh clone carries NONE of the foreign residue bytes");
    assert.equal(observed!.journal?.runId, foreignRunId, "the journal is re-owned by the reclaiming run");
    assert.equal(observed!.journal?.clonePath, clonePath);
    assert.equal(observed!.quarantineHasResidue, true, "the foreign residue was RENAMED to a retained quarantine");
    // The foreign quarantine is RETAINED forever (discard:false) — the reclaiming run's OWN
    // terminal cleanup retires its reseeded clone but never touches the foreign quarantine.
    const survivors = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
    assert.ok(
      survivors.some((d) => fs.existsSync(path.join(holdingRoot(), d, "FOREIGN_RESIDUE.txt"))),
      "the foreign quarantine survives the run",
    );
  });

  for (const scenario of ["running", "notOwned404", "transient503"] as const) {
    it(`T3: a ${scenario} foreign owner fails closed — journal, clone untouched, no quarantine`, async () => {
      const { gitlab } = fakeGitlab();
      const n = scenario === "running" ? 1 : scenario === "notOwned404" ? 2 : 3;
      const iid = 1410 + n;
      const ownerRunId = `c0000000-0000-4000-8000-00000000000${n}`;
      const foreignRunId = `d0000000-0000-4000-8000-00000000000${n}`;
      const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN_RESIDUE.txt");
      if (scenario === "running") api.setOwnershipStatus(ownerRunId, "running");
      else if (scenario === "notOwned404") api.setOwnershipNotOwned(ownerRunId);
      else api.failOwnership(ownerRunId, 503);

      let retireCalls = 0;
      const origRetire = git.retireRunnerClone.bind(git);
      git.retireRunnerClone = async (b, c, br, r, o) => {
        retireCalls++;
        return origRetire(b, c, br, r, o);
      };
      let ran = false;
      const factory: ExecutorFactory = () => ({
        homeDir: path.join(homeDir, foreignRunId),
        executor: {
          run: async () => {
            ran = true;
            throw new Error("the model must never start on a fail-closed clone");
          },
        },
      });
      const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
      try {
        await runner.execute(gitlabClaim(iid, { run_id: foreignRunId }));
      } finally {
        git.retireRunnerClone = origRetire;
      }

      assert.equal(ran, false, "the model never starts");
      assert.equal(retireCalls, 0, "no quarantine on an unproven (non-terminal / 404 / transient) owner");
      assert.ok(api.states.some((s) => s.body.status === "failed"), "the run fails closed");
      assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath }, "the journal is untouched");
      assert.equal(
        fs.readFileSync(path.join(clonePath, "FOREIGN_RESIDUE.txt"), "utf8"),
        "owner-only bytes\n",
        "the canonical clone is untouched",
      );
      assert.equal(fs.existsSync(holdingRoot()), false, "no quarantine subtree is created");
    });
  }

  it("T4: the owner's terminal retire releases the canonical even when the trash delete fails", async () => {
    const iid = 1420;
    const ownerRunId = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "TRASH_ME.txt");
    const hRoot = holdingRoot();
    // Make ONLY the post-rename trash delete fail (targeted by path so no other fs.rm is
    // affected); this file's tests run in an isolated process, and we restore in finally.
    const origRm = fsp.rm.bind(fsp);
    (fsp as { rm: typeof fsp.rm }).rm = (async (p: fs.PathLike, opts?: Parameters<typeof origRm>[1]) => {
      if (String(p).startsWith(hRoot)) throw Object.assign(new Error("injected trash delete failure"), { code: "EPERM" });
      return origRm(p, opts);
    }) as typeof fsp.rm;
    try {
      await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true });
    } finally {
      (fsp as { rm: typeof fsp.rm }).rm = origRm;
    }

    // The RELEASE (atomic rename + journal clear) is durable regardless of disposal.
    assert.equal(fs.existsSync(clonePath), false, "the canonical is free (renamed away)");
    assert.equal(readJournal(bare, branch), undefined, "the (runId, clonePath) journal is cleared");
    // The isolated trash residue remains (the delete failed), and NOT at canonical.
    const dirs = fs.readdirSync(hRoot);
    assert.equal(dirs.length, 1, "the failed delete leaves exactly the one isolated trash dir");
    assert.equal(fs.readFileSync(path.join(hRoot, dirs[0]!, "TRASH_ME.txt"), "utf8"), "owner-only bytes\n");
    // A subsequent run seeds cleanly on the now-free branch.
    const reseeded = await git.createOrAttachRunnerClone(bare, iid, "ffffffff-ffff-4fff-8fff-ffffffffffff");
    assert.equal(reseeded.path, clonePath);
    assert.equal(fs.existsSync(clonePath), true);
  });

  it("T5: a journal rewritten between the guard and retire makes retire move NOTHING", async () => {
    const iid = 1430;
    const ownerRunId = "11111111-1111-4111-8111-111111111111";
    const successorRunId = "22222222-2222-4222-8222-222222222222";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "SUCCESSOR.txt");
    // The lock gap: after the guard threw ForeignCaptureBlockedError(ownerRunId), a
    // successor re-journals the SAME canonical path under a DIFFERENT runId.
    await git.markRecoveryCapture(bare, clonePath, branch, successorRunId);

    await assert.rejects(
      git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: false }),
      StaleCaptureJournalError,
      "retire fails closed on a pre-rename pair mismatch",
    );

    assert.equal(fs.existsSync(clonePath), true, "the successor's clone is untouched");
    assert.equal(fs.readFileSync(path.join(clonePath, "SUCCESSOR.txt"), "utf8"), "owner-only bytes\n");
    assert.deepEqual(readJournal(bare, branch), { runId: successorRunId, clonePath }, "the successor journal is untouched");
    assert.equal(fs.existsSync(holdingRoot()), false, "nothing was moved to a quarantine");
  });

  it("T6: the quarantine is OUTSIDE runnerRoot and the holding subtree is mode 0700", async () => {
    const iid = 1440;
    const ownerRunId = "33333333-3333-4333-8333-333333333333";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "Q.txt");
    await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: false });

    const hRoot = holdingRoot();
    assert.equal(fs.existsSync(hRoot), true);
    assert.ok(
      !hRoot.startsWith(runnerRoot() + path.sep) && hRoot !== runnerRoot(),
      "the quarantine root is a SIBLING of runnerRoot, never under the runner-writable tree",
    );
    const mode = fs.statSync(hRoot).mode & 0o777;
    assert.equal(mode & 0o077, 0, "the holding subtree grants NOTHING to group/other");
    assert.equal(mode & 0o700, 0o700, "the owner holds rwx on the holding subtree (0700)");
    const dirs = fs.readdirSync(hRoot);
    assert.equal(dirs.length, 1, "the residue is retained (discard:false)");
    assert.equal(fs.existsSync(path.join(hRoot, dirs[0]!, "Q.txt")), true);
  });

  it("T7a: a missing SOURCE is treated as already-free (journal cleared, no throw)", async () => {
    const iid = 1450;
    const ownerRunId = "44444444-4444-4444-8444-444444444444";
    const bare = await git.ensureClone(fx.originPath);
    const branch = `agent/issue-${iid}`;
    const clonePath = worktreeDirFor(iid); // under runnerRoot, but NEVER created on disk
    await git.markRecoveryCapture(bare, clonePath, branch, ownerRunId);

    // rename ENOENT -> lstat(source) ENOENT -> canonical already free -> clear the journal.
    await git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true });
    assert.equal(readJournal(bare, branch), undefined, "a confirmed-free canonical clears the journal");
  });

  it("T7b: a rename ENOENT with the source present surfaces as a real error (journal intact)", async () => {
    const iid = 1451;
    const ownerRunId = "55555555-5555-4555-8555-555555555555";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "KEEP.txt");
    // Simulate a rename ENOENT (a DESTINATION-parent problem) while the SOURCE is present.
    const origRename = fsp.rename.bind(fsp);
    (fsp as { rename: typeof fsp.rename }).rename = (async () => {
      throw Object.assign(new Error("simulated missing destination parent"), { code: "ENOENT" });
    }) as typeof fsp.rename;
    try {
      await assert.rejects(
        git.retireRunnerClone(bare, clonePath, branch, ownerRunId, { discard: true }),
        /simulated missing destination parent/,
        "a rename ENOENT while the source is present is a REAL error, never treated as free",
      );
    } finally {
      (fsp as { rename: typeof fsp.rename }).rename = origRename;
    }
    assert.equal(fs.readFileSync(path.join(clonePath, "KEEP.txt"), "utf8"), "owner-only bytes\n", "the source is untouched");
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath }, "the journal is NOT cleared");
  });

  it("T8: a journaled clonePath OUTSIDE runnerRoot fails closed and moves nothing", async () => {
    const iid = 1460;
    const ownerRunId = "66666666-6666-4666-8666-666666666666";
    const bare = await git.ensureClone(fx.originPath);
    const branch = `agent/issue-${iid}`;
    // A journaled path OUTSIDE the runner-writable tree (a sibling under dataDir).
    const outside = path.join(fx.dataDir, "not-runner", "evil-clone");
    fs.mkdirSync(outside, { recursive: true });
    fs.writeFileSync(path.join(outside, "OUTSIDE.txt"), "outside bytes\n");
    await git.markRecoveryCapture(bare, outside, branch, ownerRunId);

    await assert.rejects(
      git.retireRunnerClone(bare, outside, branch, ownerRunId, { discard: true }),
      StaleCaptureJournalError,
      "a path outside runnerRoot is never retired, whatever the journal claims",
    );
    assert.equal(fs.existsSync(outside), true, "the out-of-tree path is not moved");
    assert.equal(fs.readFileSync(path.join(outside, "OUTSIDE.txt"), "utf8"), "outside bytes\n");
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath: outside }, "the journal is untouched");
  });

  it("T9: an in-tree DIFFERENT clone journaled to a TERMINAL owner is Case A — no probe, no retire", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1470;
    const ownerRunId = "77777777-7777-4777-8777-777777777777";
    const foreignRunId = "88888888-8888-4888-8888-888888888888";
    const bare = await git.ensureClone(fx.originPath);
    const branch = `agent/issue-${iid}`;
    // A DIFFERENT in-tree clone (both under runnerRoot): seed the canonical for a DIFFERENT
    // issue, then journal THIS branch to point at it — a stale-for-this-branch journal whose
    // journaled path still exists on disk and is itself under runnerRoot.
    const other = await git.createOrAttachRunnerClone(bare, 9470, ownerRunId);
    fs.writeFileSync(path.join(other.path, "OTHER_TREE.txt"), "other in-tree bytes\n");
    await git.markRecoveryCapture(bare, other.path, branch, ownerRunId);
    api.setOwnershipStatus(ownerRunId, "completed"); // authoritatively terminal — must NOT matter

    let ownershipProbes = 0;
    const origOwn = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId) => {
      if (runId === ownerRunId) ownershipProbes++;
      return origOwn(runId);
    };
    let retireCalls = 0;
    const origRetire = git.retireRunnerClone.bind(git);
    git.retireRunnerClone = async (b, c, br, r, o) => {
      retireCalls++;
      return origRetire(b, c, br, r, o);
    };
    let ran = false;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, foreignRunId),
      executor: {
        run: async () => {
          ran = true;
          throw new Error("the model must never start");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    try {
      await runner.execute(gitlabClaim(iid, { run_id: foreignRunId }));
    } finally {
      client.getRunOwnership = origOwn;
      git.retireRunnerClone = origRetire;
    }

    assert.equal(ran, false, "the model never starts");
    assert.ok(api.states.some((s) => s.body.status === "failed"), "Case A fails the run closed");
    assert.equal(ownershipProbes, 0, "the runner NEVER owner-probes on a StaleCaptureJournalError");
    assert.equal(retireCalls, 0, "the runner NEVER retires on a StaleCaptureJournalError");
    assert.equal(
      fs.readFileSync(path.join(other.path, "OTHER_TREE.txt"), "utf8"),
      "other in-tree bytes\n",
      "the journaled in-tree residue is untouched",
    );
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath: other.path }, "the journal is untouched");
    assert.equal(fs.existsSync(holdingRoot()), false, "no quarantine is created");
  });

  it("T1(git): the guard classifies same-run / same-path-foreign / different-path", async () => {
    const iid = 1480;
    const ownerRunId = "99999999-9999-4999-8999-999999999999";
    const foreignRunId = "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "GUARD.txt");

    // Case C — same run: PendingRecoveryCaptureError.
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, iid, ownerRunId),
      (err: unknown) => {
        assert.equal((err as Error).name, "PendingRecoveryCaptureError");
        return true;
      },
      "same-run reseed must be captured first",
    );
    // Case B — same canonical path, foreign owner: ForeignCaptureBlockedError with fields.
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, iid, foreignRunId),
      (err: unknown) => {
        assert.ok(err instanceof ForeignCaptureBlockedError);
        assert.equal(err.ownerRunId, ownerRunId);
        assert.equal(err.clonePath, clonePath);
        assert.equal(err.branch, branch);
        return true;
      },
      "same-path foreign owner is the reclaimable case",
    );
    // Case A — a different clone key computes a different path: StaleCaptureJournalError.
    await assert.rejects(
      git.runnerCloneForBranch(bare, branch, "different-kind-clone", foreignRunId),
      (err: unknown) => {
        assert.ok(err instanceof StaleCaptureJournalError);
        assert.equal(err.journaledPath, clonePath);
        assert.equal(err.branch, branch);
        return true;
      },
      "a different clone key is never reclaimable",
    );
    // The journal is untouched by all three fail-closed classifications.
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath });
  });
});
