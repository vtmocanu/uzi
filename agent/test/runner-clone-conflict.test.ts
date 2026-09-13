import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { type ExecutorFactory } from "../src/runner.js";
import { recordingLogger } from "./helpers.js";
import {
  api,
  deferred,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  homeDir,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// issue #1308 m2/m3 — the runner-layer half of the cross-run clone-conflict fix. When a
// DIFFERENT run's stale recovery-capture journal (worker-owned bare config,
// `uzi-recovery.<branch>.clone`) refuses this run's clone seed (RunnerCloneConflictError),
// the runner self-heals EXACTLY when the journal's owner run is definitively terminal
// (completed/failed/cancelled) AND not locally active — reclaiming the owner's residue and
// retrying the seed once. Every other case must fail CLOSED: the journal, the owner's
// clone, and refs/uzi-runner/<branch> are all left exactly as they were, and the run
// terminates with `fail_origin: "runner_clone_conflict"` rather than silently destroying
// or adopting another run's retained work.

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};

function recoveryConfigKey(branch: string): string {
  return `uzi-recovery.${branch}.clone`;
}

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

function writeJournalRaw(bare: string, branch: string, value: string): void {
  execFileSync("git", ["-C", bare, "config", "--local", recoveryConfigKey(branch), value], { env: GIT_ENV });
}

/** Seed a stale journal on `branch` naming `ownerRunId` at `branch`'s deterministic clone
 *  path, with a real directory (containing a marker file) so the git-layer's `fs.lstat`
 *  existence gate actually fires (a journal naming a vanished path is silently ignored,
 *  not a conflict). Returns the seeded clone path. */
async function seedStaleOwnerJournal(iid: number, ownerRunId: string): Promise<{ bare: string; branch: string; clonePath: string }> {
  const bare = await git.ensureClone(fx.originPath);
  const branch = `agent/issue-${iid}`;
  const clonePath = worktreeDirFor(iid);
  fs.mkdirSync(clonePath, { recursive: true });
  fs.writeFileSync(path.join(clonePath, "OWNER_MARKER.txt"), "stale owner clone\n");
  await git.markRecoveryCapture(bare, clonePath, branch, ownerRunId);
  return { bare, branch, clonePath };
}

/** Spy on `git.reclaimTerminalRecoveryClone`, recording every call and forwarding to the
 *  real implementation so the test still exercises production behaviour. */
function spyReclaim(): { calls: Array<{ barePath: string; branch: string; ownerRunId: string; ownerClonePath: string }> } {
  const original = git.reclaimTerminalRecoveryClone.bind(git);
  const calls: Array<{ barePath: string; branch: string; ownerRunId: string; ownerClonePath: string }> = [];
  git.reclaimTerminalRecoveryClone = (async (barePath: string, branch: string, ownerRunId: string, ownerClonePath: string) => {
    calls.push({ barePath, branch, ownerRunId, ownerClonePath });
    return original(barePath, branch, ownerRunId, ownerClonePath);
  }) as typeof git.reclaimTerminalRecoveryClone;
  return { calls };
}

/** A minimal executor that just proves the model was reached, then throws a plain
 *  (non-typed) error so the run terminates quickly without needing full finalize/push
 *  machinery. Distinguishing this from a `runner_clone_conflict` failure is exactly the
 *  point: reaching here at all is proof the self-heal retried the seed successfully. */
function modelReachedFactory(claimRunId: string, started: () => void): ExecutorFactory {
  return () => ({
    homeDir: path.join(homeDir, claimRunId),
    executor: {
      run: async () => {
        started();
        throw new Error("stop after reaching the model (test)");
      },
    },
  });
}

function failedStateFor(runId: string): { fail_origin?: string; failure_reason?: string } | undefined {
  return api.states.find((s) => s.runId === runId && s.body.status === "failed")?.body;
}

describe("cross-run clone-conflict self-heal (issue #1308 m2/m3)", () => {
  const TERMINAL_STATUSES = ["completed", "failed", "cancelled"] as const;
  TERMINAL_STATUSES.forEach((ownerStatus, i) => {
    it(`reclaims a terminal (${ownerStatus}) owner's stale journal and lets the retry reach the model`, async () => {
      const iid = 9200 + i;
      const ownerRunId = `30000000-0000-4000-8000-00000000000${i}`;
      const { bare, branch, clonePath } = await seedStaleOwnerJournal(iid, ownerRunId);
      api.setOwnershipStatus(ownerRunId, ownerStatus);
      const reclaim = spyReclaim();

      let modelStarted = false;
      const claim = gitlabClaim(iid);
      const { gitlab } = fakeGitlab();
      await runnerWith(modelReachedFactory(claim.run_id, () => { modelStarted = true; }), gitlab).execute(claim);

      assert.strictEqual(reclaim.calls.length, 1, "the self-heal reclaim fires exactly once");
      assert.deepStrictEqual(
        reclaim.calls[0],
        { barePath: bare, branch, ownerRunId, ownerClonePath: clonePath },
        "the reclaim targets exactly the journal's recorded owner",
      );
      assert.strictEqual(modelStarted, true, "a successful self-heal must let the retried seed reach the model");
      assert.strictEqual(
        fs.existsSync(path.join(clonePath, "OWNER_MARKER.txt")),
        false,
        "the terminal owner's clone content is gone — a fresh clone was reseeded at the same path",
      );
      const failed = failedStateFor(claim.run_id);
      assert.ok(failed, "the run still terminates (the injected model error), but NOT via the conflict path");
      assert.notStrictEqual(failed!.fail_origin, "runner_clone_conflict", "a successful self-heal must not stamp the conflict fail_origin");
    });
  });

  it("fails CLOSED when the journal's owner run is NOT terminal — nothing is touched, fail_origin is the typed conflict", async () => {
    const iid = 9210;
    const ownerRunId = "30000000-0000-4000-8000-000000000010";
    const { bare, branch, clonePath } = await seedStaleOwnerJournal(iid, ownerRunId);
    api.setOwnershipStatus(ownerRunId, "running");
    const reclaim = spyReclaim();
    const { logger, lines } = recordingLogger();

    let modelStarted = false;
    const claim = gitlabClaim(iid);
    const { gitlab } = fakeGitlab();
    await runnerWith(modelReachedFactory(claim.run_id, () => { modelStarted = true; }), gitlab, undefined, logger).execute(claim);

    assert.strictEqual(reclaim.calls.length, 0, "a non-terminal owner must never be reclaimed");
    assert.strictEqual(modelStarted, false, "the model must never start on a fail-closed conflict");
    assert.strictEqual(fs.existsSync(path.join(clonePath, "OWNER_MARKER.txt")), true, "the owner's clone survives, byte-for-byte");
    const failed = failedStateFor(claim.run_id);
    assert.ok(failed, "the challenger run must fail");
    assert.strictEqual(failed!.fail_origin, "runner_clone_conflict");
    assert.match(failed!.failure_reason ?? "", /another run/);
    assert.deepStrictEqual(JSON.parse(readJournalRaw(bare, branch)!), { runId: ownerRunId, clonePath }, "the journal is unchanged");
    assert.ok(
      lines.some((l) => (l as { level?: string; msg?: string }).msg === "runner clone conflict: owner run not terminal; not reclaiming"),
      "the non-terminal-owner guard must fire and be observable",
    );
  });

  for (const mode of ["not-owned (404)", "transient error (5xx)"] as const) {
    it(`fails CLOSED when the ownership probe errors — ${mode}`, async () => {
      const iid = mode.startsWith("not-owned") ? 9211 : 9212;
      const ownerRunId = mode.startsWith("not-owned")
        ? "30000000-0000-4000-8000-000000000011"
        : "30000000-0000-4000-8000-000000000012";
      const { bare, branch, clonePath } = await seedStaleOwnerJournal(iid, ownerRunId);
      if (mode.startsWith("not-owned")) api.setOwnershipNotOwned(ownerRunId);
      else api.failOwnership(ownerRunId, 500);
      const reclaim = spyReclaim();
      const { logger, lines } = recordingLogger();

      let modelStarted = false;
      const claim = gitlabClaim(iid);
      const { gitlab } = fakeGitlab();
      await runnerWith(modelReachedFactory(claim.run_id, () => { modelStarted = true; }), gitlab, undefined, logger).execute(claim);

      assert.strictEqual(reclaim.calls.length, 0, "an unreachable/uncertain ownership probe must never be reclaimed");
      assert.strictEqual(modelStarted, false);
      assert.strictEqual(fs.existsSync(path.join(clonePath, "OWNER_MARKER.txt")), true, "the owner's clone survives");
      const failed = failedStateFor(claim.run_id);
      assert.ok(failed, "the challenger run must fail");
      assert.strictEqual(failed!.fail_origin, "runner_clone_conflict");
      assert.deepStrictEqual(JSON.parse(readJournalRaw(bare, branch)!), { runId: ownerRunId, clonePath }, "the journal is unchanged");
      assert.ok(
        lines.some((l) => (l as { level?: string; msg?: string }).msg === "runner clone conflict: owner terminality probe failed; not reclaiming"),
        "the probe-failure guard must fire and be observable",
      );
    });
  }

  it("fails CLOSED on an unknown/non-terminal ownership status (e.g. awaiting_input)", async () => {
    const iid = 9213;
    const ownerRunId = "30000000-0000-4000-8000-000000000013";
    const { bare, branch, clonePath } = await seedStaleOwnerJournal(iid, ownerRunId);
    api.setOwnershipStatus(ownerRunId, "awaiting_input");
    const reclaim = spyReclaim();

    let modelStarted = false;
    const claim = gitlabClaim(iid);
    const { gitlab } = fakeGitlab();
    await runnerWith(modelReachedFactory(claim.run_id, () => { modelStarted = true; }), gitlab).execute(claim);

    assert.strictEqual(reclaim.calls.length, 0, "an unrecognised status must not be treated as terminal");
    assert.strictEqual(modelStarted, false);
    assert.strictEqual(fs.existsSync(path.join(clonePath, "OWNER_MARKER.txt")), true);
    const failed = failedStateFor(claim.run_id);
    assert.ok(failed);
    assert.strictEqual(failed!.fail_origin, "runner_clone_conflict");
    assert.deepStrictEqual(JSON.parse(readJournalRaw(bare, branch)!), { runId: ownerRunId, clonePath });
  });

  it("fails CLOSED on a malformed (non-JSON) journal — no reclaim is even attempted, the malformed value survives", async () => {
    const iid = 9214;
    const branch = `agent/issue-${iid}`;
    const bare = await git.ensureClone(fx.originPath);
    writeJournalRaw(bare, branch, "not-json-at-all");
    const reclaim = spyReclaim();

    let modelStarted = false;
    const claim = gitlabClaim(iid);
    const { gitlab } = fakeGitlab();
    await runnerWith(modelReachedFactory(claim.run_id, () => { modelStarted = true; }), gitlab).execute(claim);

    assert.strictEqual(reclaim.calls.length, 0, "a malformed journal is not a RunnerCloneConflictError — no reclaim attempt");
    assert.strictEqual(modelStarted, false, "the model never starts over an unreadable journal");
    const failed = failedStateFor(claim.run_id);
    assert.ok(failed, "the run fails rather than silently reseeding over a corrupt journal");
    assert.notStrictEqual(failed!.fail_origin, "runner_clone_conflict", "a parse error is not the typed conflict");
    assert.strictEqual(readJournalRaw(bare, branch), "not-json-at-all", "the malformed journal is left exactly as it was");
  });

  it("fails CLOSED on a malformed (shape-invalid) journal — missing clonePath — with the typed parse error surfaced", async () => {
    const iid = 9215;
    const branch = `agent/issue-${iid}`;
    const bare = await git.ensureClone(fx.originPath);
    writeJournalRaw(bare, branch, JSON.stringify({ runId: "some-run-id" }));
    const reclaim = spyReclaim();

    let modelStarted = false;
    const claim = gitlabClaim(iid);
    const { gitlab } = fakeGitlab();
    await runnerWith(modelReachedFactory(claim.run_id, () => { modelStarted = true; }), gitlab).execute(claim);

    assert.strictEqual(reclaim.calls.length, 0);
    assert.strictEqual(modelStarted, false);
    const failed = failedStateFor(claim.run_id);
    assert.ok(failed);
    assert.notStrictEqual(failed!.fail_origin, "runner_clone_conflict");
    assert.match(failed!.failure_reason ?? "", /invalid retained recovery clone journal/);
    assert.strictEqual(readJournalRaw(bare, branch), JSON.stringify({ runId: "some-run-id" }), "the malformed journal is left exactly as it was");
  });

  it("(case 11) an owner still active in THIS worker's local activeRuns is never reclaimed, even though the API already reports it terminal", async () => {
    const iid = 9216;
    const branch = `agent/issue-${iid}`;
    await git.ensureClone(fx.originPath);
    const { gitlab } = fakeGitlab();
    const { logger, lines } = recordingLogger();

    const ownerStarted = deferred();
    const releaseOwner = deferred();
    const challengerClaim = gitlabClaim(iid);
    const ownerClaim = gitlabClaim(iid);
    let modelStartedChallenger = false;

    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async () => {
          if (runId === challengerClaim.run_id) {
            modelStartedChallenger = true;
            return { branch };
          }
          // The owner run: register as active, then block until released.
          ownerStarted.resolve();
          await releaseOwner.promise;
          throw new Error("owner torn down after the probe (test)");
        },
      },
    });
    const runner = runnerWith(factory, gitlab, undefined, logger);
    const ownerExecution = runner.execute(ownerClaim);
    try {
      await ownerStarted.promise;
      // The server-side row already reads terminal — the local activeRuns entry is the
      // ONLY thing that must stop the challenger from reclaiming a live owner.
      api.setOwnershipStatus(ownerClaim.run_id, "completed");

      await runner.execute(challengerClaim);

      assert.strictEqual(modelStartedChallenger, false, "the challenger must fail closed, never reaching the model");
      const failed = failedStateFor(challengerClaim.run_id);
      assert.ok(failed, "the challenger run fails");
      assert.strictEqual(failed!.fail_origin, "runner_clone_conflict");
      assert.ok(
        lines.some((l) => {
          const r = l as { level?: string; msg?: string; owner_run_id?: string };
          return (
            r.level === "warn" &&
            r.msg === "runner clone conflict: owner run still active locally; not reclaiming" &&
            r.owner_run_id === ownerClaim.run_id
          );
        }),
        "the still-active-locally guard must fire and be observable, naming the correct owner",
      );
      assert.strictEqual(
        fs.existsSync(worktreeDirFor(iid)),
        true,
        "the still-active owner's clone is untouched",
      );
    } finally {
      releaseOwner.resolve();
      await ownerExecution;
    }
  });
});
