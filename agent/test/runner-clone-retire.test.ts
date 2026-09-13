import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { ForeignCaptureBlockedError, CapturePathMismatchError } from "../src/git.js";
import { type ExecutorFactory } from "../src/runner.js";
import { nullLogger } from "./helpers.js";
import {
  api,
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

/** Like seedResidue, but for a NON-issue clone keyed on an arbitrary branch+slug (the
 *  ci_fix / mr_rework shapes): seed the canonical clone for `branch` at `<runnerRoot>/…/<key>`,
 *  drop an owner-only marker, and journal (ownerRunId, canonicalPath). */
async function seedResidueForBranch(
  branch: string,
  key: string,
  ownerRunId: string,
  marker: string,
): Promise<{ bare: string; branch: string; clonePath: string }> {
  const bare = await git.ensureClone(fx.originPath);
  const clone = await git.runnerCloneForBranch(bare, branch, key, ownerRunId);
  fs.writeFileSync(path.join(clone.path, marker), "owner-only bytes\n");
  await git.markRecoveryCapture(bare, clone.path, branch, ownerRunId);
  return { bare, branch, clonePath: clone.path };
}

describe("atomic runner-clone release (#1315) + owner-derived reclaim (#1319)", () => {
  it("Test 1 (Gap 1, Case A cross-kind): an mr_rework reclaims a TERMINAL issue owner's residue and reseeds at its own slug", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1401;
    const ownerRunId = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const claimantRunId = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
    // An ISSUE owner seeded at `.../issue-N`, journaled under branch `agent/issue-N`.
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
    // The claimant is an mr_rework on the SAME branch → slug `agent-issue-N`, so the journal
    // path (`.../issue-N`) diverges from this claimant's computed clone → Case A.
    const freshPath = git.runnerClonePath(bare, `agent-issue-${iid}`);
    // branch NULL exercises the issue_iid-only derivation (the realistic pre-completion case).
    api.setOrphanClassification(ownerRunId, {
      status: "completed",
      repo_id: "r1",
      kind: "issue",
      issue_iid: iid,
      branch: null,
      pipeline_ref: null,
      pipeline_id: null,
    });

    let observed:
      | { worktree: string; freshExists: boolean; residueGone: boolean; journal: ReturnType<typeof readJournal>; quarantineHasResidue: boolean; oldGone: boolean }
      | undefined;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, claimantRunId),
      executor: {
        run: async (ctx) => {
          const dirs = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
          observed = {
            worktree: ctx.worktreePath,
            freshExists: fs.existsSync(ctx.worktreePath),
            residueGone: !fs.existsSync(path.join(ctx.worktreePath, "FOREIGN.txt")),
            journal: readJournal(bare, branch),
            quarantineHasResidue: dirs.some((d) => fs.existsSync(path.join(holdingRoot(), d, "FOREIGN.txt"))),
            oldGone: !fs.existsSync(clonePath),
          };
          throw new Error("stop after phaseClone reseed");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    await runner.execute(gitlabClaim(iid, { run_id: claimantRunId, kind: "mr_rework", branch: `agent/issue-${iid}` }));

    assert.ok(observed, "executor.run was reached, so phaseClone reseeded");
    assert.equal(observed!.worktree, freshPath, "the reseed lands at the mr_rework slug `agent-issue-N`");
    assert.equal(observed!.freshExists, true);
    assert.equal(observed!.residueGone, true, "the fresh clone carries NONE of the owner residue bytes");
    assert.equal(observed!.oldGone, true, "the old `.../issue-N` was RENAMED into the quarantine");
    assert.equal(observed!.quarantineHasResidue, true, "the owner residue was RENAMED to a retained quarantine");
    assert.equal(observed!.journal?.runId, claimantRunId, "the journal is re-owned by the claimant");
    assert.equal(observed!.journal?.clonePath, freshPath, "the journal points at the fresh `agent-issue-N` clone");
  });

  it("Test 2 (Gap 2, Case B same-slug worker-move): reclaim succeeds via the NEW owner-scoped read despite an ownership 404", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1402;
    const ownerRunId = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
    const claimantRunId = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
    const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
    // Simulate the worker move: the OLD worker-scoped ownership probe 404s (owner not on this
    // worker), but the NEW owner-scoped orphan-classification read is authoritative.
    api.setOwnershipNotOwned(ownerRunId);
    api.setOrphanClassification(ownerRunId, {
      status: "completed",
      repo_id: "r1",
      kind: "issue",
      issue_iid: iid,
      branch: null,
      pipeline_ref: null,
      pipeline_id: null,
    });

    let observed:
      | { worktree: string; freshExists: boolean; residueGone: boolean; journal: ReturnType<typeof readJournal>; quarantineHasResidue: boolean }
      | undefined;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, claimantRunId),
      executor: {
        run: async (ctx) => {
          const dirs = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
          observed = {
            worktree: ctx.worktreePath,
            freshExists: fs.existsSync(ctx.worktreePath),
            residueGone: !fs.existsSync(path.join(ctx.worktreePath, "FOREIGN.txt")),
            journal: readJournal(bare, branch),
            quarantineHasResidue: dirs.some((d) => fs.existsSync(path.join(holdingRoot(), d, "FOREIGN.txt"))),
          };
          throw new Error("stop after phaseClone reseed");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    await runner.execute(gitlabClaim(iid, { run_id: claimantRunId }));

    assert.ok(observed, "executor.run was reached, so phaseClone reseeded via the owner-scoped read");
    assert.equal(observed!.worktree, clonePath, "the reseed lands at the canonical `.../issue-N`");
    assert.equal(observed!.freshExists, true);
    assert.equal(observed!.residueGone, true, "the fresh clone carries NONE of the owner residue bytes");
    assert.equal(observed!.quarantineHasResidue, true, "the residue was RENAMED to a retained quarantine");
    assert.equal(observed!.journal?.runId, claimantRunId, "the journal is re-owned by the claimant");
    assert.equal(observed!.journal?.clonePath, clonePath);
  });

  it("Test 2b (ci_fix pipeline_id arm, moved-worker default-branch reclaim end-to-end)", async () => {
    const { gitlab } = fakeGitlab();
    const pid = 5150;
    const ownerRunId = "e1111111-1111-4111-8111-111111111111";
    const claimantRunId = "e2222222-2222-4222-8222-222222222222";
    // A ci_fix owner on the default branch: branch `ci-fix/pipeline-5150`, slug `ci-fix-pipeline-5150`.
    const { bare, branch, clonePath } = await seedResidueForBranch(
      `ci-fix/pipeline-${pid}`,
      `ci-fix-pipeline-${pid}`,
      ownerRunId,
      "FOREIGN.txt",
    );
    // The owner-side deriveCloneKey reconstructs `ci-fix/pipeline-5150` from pipeline_id +
    // pipeline_ref + the claimant's default branch "main" → predicates (c)/(d) hold.
    api.setOrphanClassification(ownerRunId, {
      status: "failed",
      repo_id: "r1",
      kind: "ci_fix",
      issue_iid: null,
      branch: null,
      pipeline_ref: "main",
      pipeline_id: pid,
    });

    let observed:
      | { worktree: string; freshExists: boolean; residueGone: boolean; journal: ReturnType<typeof readJournal>; quarantineHasResidue: boolean }
      | undefined;
    const factory: ExecutorFactory = () => ({
      homeDir: path.join(homeDir, claimantRunId),
      executor: {
        run: async (ctx) => {
          const dirs = fs.existsSync(holdingRoot()) ? fs.readdirSync(holdingRoot()) : [];
          observed = {
            worktree: ctx.worktreePath,
            freshExists: fs.existsSync(ctx.worktreePath),
            residueGone: !fs.existsSync(path.join(ctx.worktreePath, "FOREIGN.txt")),
            journal: readJournal(bare, branch),
            quarantineHasResidue: dirs.some((d) => fs.existsSync(path.join(holdingRoot(), d, "FOREIGN.txt"))),
          };
          throw new Error("stop after phaseClone reseed");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    await runner.execute(
      gitlabClaim(0, {
        run_id: claimantRunId,
        kind: "ci_fix",
        pipeline: { id: pid, ref: "main", sha: "0".repeat(40), web_url: "https://x/p", failed_jobs: [] },
        repo: { id: "r1", url: "https://x/r", clone_url: fx.originPath, default_branch: "main" },
      }),
    );

    assert.ok(observed, "executor.run was reached, so phaseClone reseeded");
    assert.equal(observed!.worktree, clonePath, "the reseed lands at the ci-fix slug `ci-fix-pipeline-5150`");
    assert.equal(observed!.freshExists, true);
    assert.equal(observed!.residueGone, true, "the fresh clone carries NONE of the owner residue bytes");
    assert.equal(observed!.quarantineHasResidue, true, "the residue was RENAMED to a retained quarantine");
    assert.equal(observed!.journal?.runId, claimantRunId, "the journal is re-owned by the claimant");
    assert.equal(observed!.journal?.clonePath, clonePath);
  });

  // Test 3 — fail closed, one row per predicate. Each row asserts: the model never starts,
  // retireRunnerClone is NEVER called, the run ends failed, the journal + seeded clone are
  // untouched, and no quarantine subtree is created.
  interface FailClosedRow {
    name: string;
    iid: number;
    /** Optional claim-field overrides merged into the claimant's `gitlabClaim`. Absent ⇒ a
     *  plain ISSUE claim on `iid` (the four original rows). A row sets this to make the
     *  claimant a different kind (e.g. a `task` run, whose slug is DECOUPLED from its branch)
     *  so a single predicate can be isolated. */
    claimOverrides?: Record<string, unknown>;
    /** Build the (bare, branch, residue, expected journal) fixture and set the fake's read.
     *  The claimant is an issue run on `iid`, so each row is Case A or Case B as noted. */
    setup: (ctx: {
      iid: number;
      ownerRunId: string;
    }) => Promise<{ bare: string; branch: string; residuePath: string; expectJournal: { runId: string; clonePath: string } }>;
  }

  const failClosedRows: FailClosedRow[] = [
    {
      name: "(b) wrong repo",
      iid: 1411,
      setup: async ({ iid, ownerRunId }) => {
        const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r2",
          kind: "issue",
          issue_iid: iid,
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: clonePath, expectJournal: { runId: ownerRunId, clonePath } };
      },
    },
    {
      name: "(c) wrong owner-derived branch",
      iid: 1412,
      setup: async ({ iid, ownerRunId }) => {
        const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r1",
          kind: "issue",
          issue_iid: 9999, // → owner-derived branch `agent/issue-9999` ≠ journal branch
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: clonePath, expectJournal: { runId: ownerRunId, clonePath } };
      },
    },
    {
      name: "(d) wrong in-tree journal path",
      iid: 1413,
      setup: async ({ iid, ownerRunId }) => {
        const bare = await git.ensureClone(fx.originPath);
        const branch = `agent/issue-${iid}`;
        // A DIFFERENT in-tree clone; journal THIS branch to point at it → Case A. (c) passes
        // (owner branch `agent/issue-N`), (d) fails (owner slug `issue-N` ≠ `.../issue-9470`).
        const other = await git.createOrAttachRunnerClone(bare, 9470, ownerRunId);
        fs.writeFileSync(path.join(other.path, "FOREIGN.txt"), "owner-only bytes\n");
        await git.markRecoveryCapture(bare, other.path, branch, ownerRunId);
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r1",
          kind: "issue",
          issue_iid: iid,
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: other.path, expectJournal: { runId: ownerRunId, clonePath: other.path } };
      },
    },
    {
      name: "(malformed) insufficient identity",
      iid: 1414,
      setup: async ({ iid, ownerRunId }) => {
        const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
        // mr_rework with no branch (pipeline_ref null) → deriveCloneKey returns undefined.
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r1",
          kind: "mr_rework",
          issue_iid: null,
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: clonePath, expectJournal: { runId: ownerRunId, clonePath } };
      },
    },
    {
      // Isolates predicate (c) — it is the ONLY guard a `task` owner can trip here, because a
      // task's slug is `task-<runId>` (DECOUPLED from its branch), so a mismatched owner-derived
      // BRANCH still reproduces the journaled PATH: (d) passes, only (c) fires. (The issue-kind
      // "(c) wrong owner-derived branch" row above ALSO trips (d), since issue derives both from
      // issue_iid — deleting the (c) line leaves it green, but reddens THIS row.)
      name: "(c-isolated) task owner: branch mismatch but slug/path match",
      iid: 1415,
      claimOverrides: { kind: "task", branch: "uzi/task/foo" },
      setup: async ({ ownerRunId }) => {
        // Seed the OWNER's task clone at branch `uzi/task/foo`, slug `task-<ownerRunId>`: the
        // journal is keyed under `uzi/task/foo` and points at `.../task-<ownerRunId>`. The
        // claimant (a task on `uzi/task/foo`, slug `task-<claimantRunId>`) reads that journal
        // and computes a DIFFERENT path → Case A / CapturePathMismatchError.
        const { bare, branch, clonePath } = await seedResidueForBranch(
          "uzi/task/foo",
          `task-${ownerRunId}`,
          ownerRunId,
          "FOREIGN.txt",
        );
        // Owner-derived branch `uzi/task/DIFFERENT` ≠ journal branch `uzi/task/foo` → (c) FIRES,
        // but owner-derived slug `task-<ownerRunId>` reproduces the journaled path exactly → (d)
        // PASSES. This is the scenario predicate (c) exclusively guards.
        api.setOrphanClassification(ownerRunId, {
          status: "completed",
          repo_id: "r1",
          kind: "task",
          issue_iid: null,
          branch: "uzi/task/DIFFERENT",
          pipeline_ref: null,
          pipeline_id: null,
        });
        return { bare, branch, residuePath: clonePath, expectJournal: { runId: ownerRunId, clonePath } };
      },
    },
  ];

  for (const row of failClosedRows) {
    it(`Test 3: fail closed ${row.name} — no probe-driven reclaim, journal & clone untouched`, async () => {
      const { gitlab } = fakeGitlab();
      const ownerRunId = `f0000000-0000-4000-8000-00000000${row.iid}`;
      const claimantRunId = `f1000000-0000-4000-8000-00000000${row.iid}`;
      const { bare, branch, residuePath, expectJournal } = await row.setup({ iid: row.iid, ownerRunId });

      let retireCalls = 0;
      const origRetire = git.retireRunnerClone.bind(git);
      git.retireRunnerClone = async (b, c, br, r, o) => {
        retireCalls++;
        return origRetire(b, c, br, r, o);
      };
      let ran = false;
      const factory: ExecutorFactory = () => ({
        homeDir: path.join(homeDir, claimantRunId),
        executor: {
          run: async () => {
            ran = true;
            throw new Error("the model must never start on a fail-closed clone");
          },
        },
      });
      const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
      try {
        await runner.execute(gitlabClaim(row.iid, { run_id: claimantRunId, ...row.claimOverrides }));
      } finally {
        git.retireRunnerClone = origRetire;
      }

      assert.equal(ran, false, "the model never starts");
      assert.equal(retireCalls, 0, "retireRunnerClone is NEVER called on an unmet predicate");
      assert.ok(api.states.some((s) => s.body.status === "failed"), "the run fails closed");
      assert.deepEqual(readJournal(bare, branch), expectJournal, "the journal is untouched");
      assert.equal(
        fs.readFileSync(path.join(residuePath, "FOREIGN.txt"), "utf8"),
        "owner-only bytes\n",
        "the seeded clone is untouched",
      );
      assert.equal(fs.existsSync(holdingRoot()), false, "no quarantine subtree is created");
    });
  }

  // Test 4 — fail closed on the probe itself. issue owner + claimant issue run (Case B).
  for (const scenario of ["non-terminal", "404", "transient"] as const) {
    it(`Test 4: fail closed on probe (${scenario}) — no reclaim, journal & clone untouched`, async () => {
      const { gitlab } = fakeGitlab();
      const n = scenario === "non-terminal" ? 1 : scenario === "404" ? 2 : 3;
      const iid = 1420 + n;
      const ownerRunId = `a3000000-0000-4000-8000-00000000000${n}`;
      const claimantRunId = `a4000000-0000-4000-8000-00000000000${n}`;
      const { bare, branch, clonePath } = await seedResidue(iid, ownerRunId, "FOREIGN.txt");
      if (scenario === "non-terminal") {
        api.setOrphanClassification(ownerRunId, {
          status: "running",
          repo_id: "r1",
          kind: "issue",
          issue_iid: iid,
          branch: null,
          pipeline_ref: null,
          pipeline_id: null,
        });
      } else if (scenario === "404") {
        api.setOrphanNotFound(ownerRunId);
      } else {
        api.failOrphanClassification(ownerRunId, 503);
      }

      let retireCalls = 0;
      const origRetire = git.retireRunnerClone.bind(git);
      git.retireRunnerClone = async (b, c, br, r, o) => {
        retireCalls++;
        return origRetire(b, c, br, r, o);
      };
      let ran = false;
      const factory: ExecutorFactory = () => ({
        homeDir: path.join(homeDir, claimantRunId),
        executor: {
          run: async () => {
            ran = true;
            throw new Error("the model must never start on a fail-closed clone");
          },
        },
      });
      const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
      try {
        await runner.execute(gitlabClaim(iid, { run_id: claimantRunId }));
      } finally {
        git.retireRunnerClone = origRetire;
      }

      assert.equal(ran, false, "the model never starts");
      assert.equal(retireCalls, 0, "no reclaim on a non-terminal / 404 / transient probe");
      assert.ok(api.states.some((s) => s.body.status === "failed"), "the run fails closed");
      assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath }, "the journal is untouched");
      assert.equal(
        fs.readFileSync(path.join(clonePath, "FOREIGN.txt"), "utf8"),
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
      CapturePathMismatchError,
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
      CapturePathMismatchError,
      "a path outside runnerRoot is never retired, whatever the journal claims",
    );
    assert.equal(fs.existsSync(outside), true, "the out-of-tree path is not moved");
    assert.equal(fs.readFileSync(path.join(outside, "OUTSIDE.txt"), "utf8"), "outside bytes\n");
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath: outside }, "the journal is untouched");
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
    // Case A — a different clone key computes a different path: CapturePathMismatchError.
    await assert.rejects(
      git.runnerCloneForBranch(bare, branch, "different-kind-clone", foreignRunId),
      (err: unknown) => {
        assert.ok(err instanceof CapturePathMismatchError);
        assert.equal(err.journaledPath, clonePath);
        assert.equal(err.branch, branch);
        // issue #1319: ownerRunId is now load-bearing — the runner's Case A reclaim probes it.
        assert.equal(err.ownerRunId, ownerRunId);
        return true;
      },
      "a different clone key is the runner's owner-derived reclaim case (#1319)",
    );
    // The journal is untouched by all three fail-closed classifications.
    assert.deepEqual(readJournal(bare, branch), { runId: ownerRunId, clonePath });
  });
});
