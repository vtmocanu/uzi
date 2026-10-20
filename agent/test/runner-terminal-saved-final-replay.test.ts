import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { RecoveryCoordinator } from "../src/recovery.js";
import type { RecoveryExactHold, RecoveryFinalDisposition } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";
import { client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";
import { passingExclusion } from "./maintenance-exclusion-fixture.js";

installHarness();

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}

// Issue #3031: after an ordinary FAILED execution with a matched source family, RunRunner hands
// its exact tail to RecoveryCoordinator.resumeTerminalSource, which replays SAVED evidence for the
// run before any boot sweep or heartbeat-driven live pass. Here an earlier generation of the run
// left a minted, unacknowledged settled FINAL; the terminal pass must send it through the client.
it("#3031 runner terminal route replays a saved FINAL without a boot or live pass", async (t) => {
  const token = "fixture-journal-key";
  const claim = gitlabClaim(3031, { claim_generation: 2, inventory_guarded: true });
  const repoId = "00000000-0000-4000-8000-000000003032";
  claim.repo.id = repoId;
  const holds = new Map([[1, "00000000-0000-4000-8000-000000003101"], [2, "00000000-0000-4000-8000-000000003102"]]);
  // Generation 1 completed (its publication settled the inventory); generation 2 is this claim.
  let status = "completed", claimGeneration = 1;
  const feature = client.hasFeature.bind(client);
  t.mock.method(client, "hasFeature", (name: Parameters<typeof client.hasFeature>[0]) =>
    name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name));
  t.mock.method(client, "listRecoveryHolds", async () => ({ run_id: claim.run_id, holds: [...holds].map(([generation, hold_id]) =>
    ({ hold_id, generation, inventory_guarded: true, has_available_capture: false })) }));
  t.mock.method(client, "getRunOwnership", async () => ({ status, claim_generation: claimGeneration, inventory_guarded: true }));
  t.mock.method(client, "getExactRecoveryHold", async (runId: string, generation: number) => ({
    run_id: runId, hold_id: holds.get(generation)!, generation, repo_id: repoId, state: "open",
    original_worker_id: "worker", original_worker_identity: "worker", inventory_guarded: true,
    run_status: status, current_claim_generation: claimGeneration, claim_released: status !== "running",
  }) as RecoveryExactHold);
  let notified = false, passSettled = false;
  const releases: Array<{ generation: number; evidence?: string; disposition?: RecoveryFinalDisposition;
    afterNotification: boolean; beforePassSettled: boolean }> = [];
  t.mock.method(client, "releaseRecoveryCustody", async (runId: string, generation: number, evidence?: string,
    disposition?: RecoveryFinalDisposition) => {
    releases.push({ generation, evidence, disposition, afterNotification: notified, beforePassSettled: !passSettled });
    return { run_id: runId, generation, released: true, holds_released: 1 };
  });
  const report = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
    const ack = await report(...args);
    if (args[1].status === "failed") status = "failed";
    return ack;
  });

  // Generation 1's settled FINAL, minted by the production freeze over the real trusted bare and
  // journaled under the worker key, then left unsent (its holder stopped before the release).
  const bare = await git.ensureClone(claim.repo.clone_url);
  const main = command(bare, "rev-parse", "refs/remotes/origin/main");
  const seeder = new RecoveryCoordinator({ client, git, log: recordingLogger().logger,
    recoveryRoot: git.recoveryRoot, workerToken: token });
  const saved = await seeder.freezeInventory({
    context: { runId: claim.run_id, generation: 1, kind: "issue", branch: "agent/issue-3031", barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: main } },
    currentSha: main, defaultBranch: "main", settledEvidence: "publication", repoId,
  });
  assert.ok(saved?.finalRequest, "a saved FINAL request is journaled");
  assert.equal(saved.finalRequest.disposition.kind, "settled");
  assert.equal(saved.finalAcknowledged, undefined);
  assert.deepEqual(saved.exactHoldBinding, { holdId: holds.get(1), repoId });
  assert.equal(releases.length, 0, "seeding sends nothing");
  status = "running"; claimGeneration = 2;

  // Fail every production attempt so the run reaches the ordinary normal_failed family path.
  t.mock.method(git, "produceRecoveryBundle", async () => {
    throw new Error("fixture bundle production unavailable before installation");
  });
  const sweeps = { pending: 0, live: 0 };
  t.mock.method(RecoveryCoordinator.prototype, "resumePending", async () => { sweeps.pending++; });
  t.mock.method(RecoveryCoordinator.prototype, "resumeLive", async () => { sweeps.live++; });
  let pass: Promise<void> | undefined;
  const resume = RecoveryCoordinator.prototype.resumeTerminalSource;
  t.mock.method(RecoveryCoordinator.prototype, "resumeTerminalSource", function (this: RecoveryCoordinator,
    ...args: Parameters<typeof resume>) {
    notified = true;
    pass = resume.apply(this, args).finally(() => { passSettled = true; });
    return pass;
  });
  const unique = randomBytes(4096).toString("hex");
  const log = recordingLogger();
  const target = path.join(homeDir, claim.run_id);
  const runner = runnerWith(() => ({
    homeDir: target,
    executor: {
      run: async ctx => {
        const source = ctx.worktreePath;
        command(source, "config", "user.name", "fixture");
        command(source, "config", "user.email", "fixture@example.com");
        await fs.writeFile(path.join(source, "unique-commit"), unique);
        command(source, "add", "unique-commit");
        command(source, "commit", "-m", "unique local work");
        await fs.mkdir(target, { recursive: true });
        throw new Error("ordinary executor failure");
      },
      reapAttributedProcesses: async () => ({ complete: true, killed: [], left: [] }),
    },
  }), fakeGitlab().gitlab, token, log.logger, {
    maintenanceExclusion: passingExclusion(),
    dockerHost: "http://unused.invalid", recoveryRetryMs: 1,
    quiesceRun: async () => ({ process: { state: "quiescent", processes: [], killed: [], detail: "" },
      docker: { state: "not_wired", removed: [], detail: "" } }),
  });
  try {
    await runner.execute(claim);
    assert.equal(status, "failed", "the run finished FAILED");
    assert.equal(notified, true, "runner notified the terminal saved-evidence pass: " + JSON.stringify(log.lines.slice(-5)));
    await pass;
    assert.deepEqual(sweeps, { pending: 0, live: 0 }, "no boot sweep or live pass ran");
    assert.equal(releases.length, 1, "exactly the saved FINAL was sent: " + JSON.stringify(releases));
    const [release] = releases;
    assert.equal(release!.generation, 1);
    assert.equal(release!.evidence, "publication");
    assert.deepEqual(release!.disposition, saved.finalRequest.disposition, "byte-identical saved disposition");
    assert.equal(release!.afterNotification, true);
    assert.equal(release!.beforePassSettled, true, "sent inside the terminal pass");
    const after = await seeder.inspect(claim.run_id);
    const replayed = after.find(r => r.captureId === saved.captureId);
    assert.ok(!replayed || replayed.finalAcknowledged === true, "the ACK is recorded or the generation forgotten");
  } finally {
    runner.shutdown();
    await runner.shutdownRecoveries();
  }
});
