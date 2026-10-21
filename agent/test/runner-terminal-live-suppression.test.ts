import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { RecoveryCoordinator } from "../src/recovery.js";
import type { RecoveryExactHold, RecoveryFinalDisposition } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";
import { client, deferred, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";
import { passingExclusion } from "./maintenance-exclusion-fixture.js";

// The runner's stopped-source and quiescence proofs (src/runner.ts, src/run-quiescence.ts) refuse off
// Linux, so this scenario cannot reach its post-proof assertions on macOS (#1912). Linux runs it unchanged.
const LINUX_ONLY = process.platform !== "linux"
  ? "needs the Linux-only run-quiescence / stopped-source proof (src/run-quiescence.ts, src/runner.ts refuse off Linux)"
  : false;

installHarness();

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}

// Issue #3031: a terminal saved-evidence pass occupies the coordinator's pass set, and
// RecoveryCoordinator.resumeLive returns early while any pass is in flight. A live re-drive is
// therefore suppressed (not lost) while a terminal pass is blocked, and resumes after it settles.
it("#3031 live re-drive is suppressed during a blocked terminal pass and resumes once after it", { skip: LINUX_ONLY }, async (t) => {
  const token = "fixture-journal-key";
  const claim = gitlabClaim(3031, { claim_generation: 2, inventory_guarded: true });
  const runA = claim.run_id, runB = "00000000-0000-4000-8000-00000000b031";
  const repoId = "00000000-0000-4000-8000-000000003032";
  claim.repo.id = repoId;
  const holds = new Map([
    [`${runA}:1`, "00000000-0000-4000-8000-000000003101"], [`${runA}:2`, "00000000-0000-4000-8000-000000003102"],
    [`${runB}:1`, "00000000-0000-4000-8000-000000003201"],
  ]);
  // Run B's generation 1 completed and is not claimed again; run A's generation 2 is this claim.
  const ownership = new Map([[runA, { status: "completed", claim_generation: 1 }], [runB, { status: "completed", claim_generation: 1 }]]);
  const feature = client.hasFeature.bind(client);
  t.mock.method(client, "hasFeature", (name: Parameters<typeof client.hasFeature>[0]) =>
    name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name));
  t.mock.method(client, "listRecoveryHolds", async (runId: string) => ({ run_id: runId,
    holds: [...holds].filter(([key]) => key.startsWith(runId + ":")).map(([key, hold_id]) =>
      ({ hold_id, generation: Number(key.split(":")[1]), inventory_guarded: true, has_available_capture: false })) }));
  t.mock.method(client, "getRunOwnership", async (runId: string) => ({ ...ownership.get(runId)!, inventory_guarded: true }));
  t.mock.method(client, "getExactRecoveryHold", async (runId: string, generation: number) => {
    const own = ownership.get(runId)!;
    return {
      run_id: runId, hold_id: holds.get(`${runId}:${generation}`)!, generation, repo_id: repoId, state: "open",
      original_worker_id: "worker", original_worker_identity: "worker", inventory_guarded: true,
      run_status: own.status, current_claim_generation: own.claim_generation, claim_released: own.status !== "running",
    } as RecoveryExactHold;
  });
  const releases: Array<{ runId: string; generation: number; disposition?: RecoveryFinalDisposition }> = [];
  const blockedEntered = deferred(), unblock = deferred();
  let blocked = false;
  t.mock.method(client, "releaseRecoveryCustody", async (runId: string, generation: number, _evidence?: string,
    disposition?: RecoveryFinalDisposition) => {
    releases.push({ runId, generation, disposition });
    if (runId === runA && !blocked) {
      // Hold the terminal pass inside its pendingStep, mid-FINAL.
      blocked = true;
      blockedEntered.resolve();
      await unblock.promise;
    }
    return { run_id: runId, generation, released: true, holds_released: 1 };
  });
  const report = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
    const ack = await report(...args);
    if (args[1].status === "failed" && args[0] === runA) ownership.set(runA, { status: "failed", claim_generation: 2 });
    return ack;
  });

  // Both runs' generation-1 settled FINALs are minted by the production freeze over the real
  // trusted bare and journaled under the worker key, then left unsent.
  const bare = await git.ensureClone(claim.repo.clone_url);
  const main = command(bare, "rev-parse", "refs/remotes/origin/main");
  const seeder = new RecoveryCoordinator({ client, git, log: recordingLogger().logger,
    recoveryRoot: git.recoveryRoot, workerToken: token });
  const seed = async (runId: string, branch: string) => {
    const saved = await seeder.freezeInventory({
      context: { runId, generation: 1, kind: "issue", branch, barePath: bare,
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: main } },
      currentSha: main, defaultBranch: "main", settledEvidence: "publication", repoId,
    });
    assert.ok(saved?.finalRequest && !saved.finalAcknowledged, `saved FINAL for ${runId}`);
    return saved;
  };
  const savedA = await seed(runA, "agent/issue-3031");
  const savedB = await seed(runB, "agent/issue-3032");
  assert.equal(releases.length, 0, "seeding sends nothing");
  ownership.set(runA, { status: "running", claim_generation: 2 });

  // Fail every production attempt so run A reaches the ordinary normal_failed family path.
  t.mock.method(git, "produceRecoveryBundle", async () => {
    throw new Error("fixture bundle production unavailable before installation");
  });
  const notifications: Array<{ coordinator: RecoveryCoordinator; args: Parameters<RecoveryCoordinator["resumeTerminalSource"]>;
    pass: Promise<void> }> = [];
  const resume = RecoveryCoordinator.prototype.resumeTerminalSource;
  t.mock.method(RecoveryCoordinator.prototype, "resumeTerminalSource", function (this: RecoveryCoordinator,
    ...args: Parameters<typeof resume>) {
    const pass = resume.apply(this, args);
    notifications.push({ coordinator: this, args, pass });
    return pass;
  });
  const unique = randomBytes(4096).toString("hex");
  const log = recordingLogger();
  const target = path.join(homeDir, runA);
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
  const forB = () => releases.filter(r => r.runId === runB);
  const forA1 = () => releases.filter(r => r.runId === runA && r.generation === 1);
  try {
    await runner.execute(claim);
    assert.equal(notifications.length, 1, "runner notified the terminal pass: " + JSON.stringify(log.lines.slice(-5)));
    const [first] = notifications;
    const passes = (first!.coordinator as unknown as { passes: Set<Promise<void>> }).passes;
    await blockedEntered.promise;
    assert.equal(passes.size, 1, "the blocked terminal pass is the only registered pass");

    // (1) A heartbeat-driven live re-drive during the blocked pass does no work.
    await runner.resumeLiveRecoveries(Date.now());
    await runner.resumeLiveRecoveries(Date.now());
    assert.deepEqual(forB(), [], "run B's retry-ready FINAL is not re-driven while the terminal pass holds");

    // (2) Repeated runner notifications for the same exact tail each register a pass.
    const repeats: Promise<void>[] = [];
    for (let i = 0; i < 3; i++) {
      repeats.push(resume.apply(first!.coordinator, first!.args));
      assert.equal(passes.size, 2 + i, `repeat ${i + 1} registered while the first pass is blocked`);
    }
    assert.equal(forA1().length, 1, "repeats wait behind the blocked FINAL");
    unblock.resolve();
    await Promise.all([first!.pass, ...repeats]);
    assert.equal(passes.size, 0, "every terminal pass settled and deregistered");
    assert.equal(forA1().length, 1, "no duplicate FINAL from the repeated notifications");
    assert.deepEqual(forA1()[0]!.disposition, savedA.finalRequest!.disposition);
    assert.deepEqual(forB(), [], "nothing re-drove run B yet");

    // (3) The next live re-drive runs and sends run B's saved FINAL exactly once.
    await runner.resumeLiveRecoveries(Date.now());
    assert.equal(forB().length, 1, "run B's FINAL re-driven once: " + JSON.stringify(releases));
    assert.equal(forB()[0]!.generation, 1);
    assert.deepEqual(forB()[0]!.disposition, savedB.finalRequest!.disposition);
    await runner.resumeLiveRecoveries(Date.now() + 1);
    assert.equal(forB().length, 1, "an acknowledged FINAL is not re-sent");

    // (4) Run A's saved FINAL was sent exactly once overall.
    assert.equal(forA1().length, 1);
    t.diagnostic(JSON.stringify(releases.map(r => ({ run: r.runId === runA ? "A" : "B", generation: r.generation,
      kind: r.disposition?.kind }))));
  } finally {
    unblock.resolve();
    runner.shutdown();
    await runner.shutdownRecoveries();
  }
});
