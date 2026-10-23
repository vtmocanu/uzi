import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { performance } from "node:perf_hooks";
import { RecoveryCoordinator } from "../src/recovery.js";
import { RequestError } from "../src/client.js";
import { SteeringChannel } from "../src/steering.js";
import type { CodexExecutionSafety, BoundaryPermit } from "../src/harness.js";
import type { Executor } from "../src/executor.js";
import type { RecoveryExactHold, RecoveryFinalDisposition } from "../src/protocol.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";
import { passingExclusion } from "./maintenance-exclusion-fixture.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";

// The runner's stopped-source and quiescence proofs (src/runner.ts, src/run-quiescence.ts) refuse off
// Linux, so this scenario cannot reach its post-proof assertions on macOS (#1912). Linux runs it unchanged.
const LINUX_ONLY = process.platform !== "linux"
  ? "needs the Linux-only run-quiescence / stopped-source proof (src/run-quiescence.ts, src/runner.ts refuse off Linux)"
  : false;

installHarness();

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}

// Issue #3031: after an ordinary FAILED execution with a matched source family, RunRunner hands
// its exact tail to RecoveryCoordinator.resumeTerminalSource, which replays SAVED evidence for the
// run before any boot sweep or heartbeat-driven live pass. Here an earlier generation of the run
// left a minted, unacknowledged settled FINAL; the terminal pass must send it through the client.
it("#3031 runner terminal route replays a saved FINAL without a boot or live pass", { skip: LINUX_ONLY }, async (t) => {
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

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

type ReplayMode = "ordinary" | "upload-rejected" | "upload-failed" | "commands-unconfirmed" | "commands-rejected" |
  "disposed" | "incomplete" | "legacy_in_process" | "dispose-rejected" | "steering-rejected" | "executor-replaced" | "shutdown" |
  "memory-enrolled" | "memory-operation-pending" | "memory-executor-pending" | "memory-closing-pending" | "memory-sink-receipt";

for (const mode of ["ordinary", "upload-rejected", "upload-failed", "commands-unconfirmed", "commands-rejected",
  "disposed", "incomplete", "legacy_in_process", "dispose-rejected", "steering-rejected", "executor-replaced", "shutdown",
  "memory-enrolled", "memory-operation-pending", "memory-executor-pending", "memory-closing-pending",
  "memory-sink-receipt"] as ReplayMode[]) {
  it("N current accepted archive replay: " + mode, {
    skip: process.platform !== "linux",
  }, async t => {
    const token = "current-archive-fixture";
    const claim = gitlabClaim(3037, { claim_generation: 1, inventory_guarded: true });
    // A real enrolled worker-memory flight (issue #3031 N): the runner binds it from the claim's memory
    // fields and the registered memory incarnation, and teardown ends it without removing the object.
    const memoryMode = mode.startsWith("memory-");
    let memoryRuntime: WorkerMemoryRuntime | undefined;
    if (memoryMode) {
      api.memoryRegisterNonce = "terminal-replay-memory-registration";
      api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence", "recovery_inventory_v1"]);
      await client.register("memory-replay-fixture", "base");
      Object.assign(claim, { memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: 0 });
      memoryRuntime = new WorkerMemoryRuntime({ enabled: true, reserveBytes: 100, hysteresisBytes: 100, sampleMs: 2,
        responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 }, client, nullLogger(), {
        reader: { sample: () => {
          const time = performance.now();
          return { available: true, sampleId: 1, startedAtMs: time, completedAtMs: time, currentBytes: 0, limitBytes: 1000,
            stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
            pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 }, full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
        } },
      });
      t.after(async () => { await memoryRuntime!.stop(); });
    }
    const repoId = "00000000-0000-4000-8000-000000003037";
    claim.repo.id = repoId;
    let status = "running";
    const feature = client.hasFeature.bind(client);
    t.mock.method(client, "hasFeature", (name: Parameters<typeof client.hasFeature>[0]) =>
      name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name));
    t.mock.method(client, "listRecoveryHolds", async () => ({ run_id: claim.run_id, holds: [
      { hold_id: repoId, generation: 1, inventory_guarded: true, has_available_capture: false },
    ] }));
    t.mock.method(client, "getRunOwnership", async () => ({ status, claim_generation: 1, inventory_guarded: true }));
    t.mock.method(client, "getExactRecoveryHold", async () => ({
      run_id: claim.run_id, hold_id: repoId, generation: 1, repo_id: repoId, state: "open",
      original_worker_id: "worker", original_worker_identity: "worker", inventory_guarded: true,
      run_status: status, current_claim_generation: 1, claim_released: status !== "running",
    }) as RecoveryExactHold);
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      const ack = await report(...args);
      if (args[1].status === "failed") status = "failed";
      return ack;
    });
    let uploads = 0;
    const manifests = new Map<string, { checksum: string; byte_size: number }>();
    t.mock.method(client, "reserveRecoveryCapture", async (...[_run, req]: Parameters<typeof client.reserveRecoveryCapture>) =>
      ({ capture_id: req.idempotency_key, state: "preparing" }));
    t.mock.method(client, "uploadRecoveryBundle", async (...[_run, capture, _manifest, body]: Parameters<typeof client.uploadRecoveryBundle>) => {
      for await (const _chunk of body) { /* consume the real current bundle */ }
      uploads++;
      manifests.set(capture, _manifest);
      if (mode === "upload-rejected") throw new RequestError("POST", "/fixture-upload", 400, "fixture rejected");
      if (mode === "upload-failed") throw new Error("fixture upload failed");
      return { capture_id: capture, state: "available", manifest_bound: true };
    });
    t.mock.method(client, "getRecoveryCaptureStatus", async (...[_run, capture]: Parameters<typeof client.getRecoveryCaptureStatus>) => ({
      capture_id: capture, state: "available", manifest_bound: true,
      ...manifests.get(capture), expires_at: "2099-01-01T00:00:00Z",
    }));
    let notifications = 0, sourceNotifications = 0, pass: Promise<void> | undefined;
    const sourceResume = RecoveryCoordinator.prototype.resumeTerminalSource;
    t.mock.method(RecoveryCoordinator.prototype, "resumeTerminalSource", function (this: RecoveryCoordinator,
      ...args: Parameters<typeof sourceResume>) {
      sourceNotifications++;
      return sourceResume.apply(this, args);
    });
    const sweeps = { boot: 0, live: 0 };
    t.mock.method(RecoveryCoordinator.prototype, "resumePending", async () => { sweeps.boot++; });
    t.mock.method(RecoveryCoordinator.prototype, "resumeLive", async () => { sweeps.live++; });
    // Optional reflection keeps the unfixed baseline compilable: its failure is the missing
    // notification/FINAL assertion, rather than a missing-method exception.
    const proto = RecoveryCoordinator.prototype as unknown as {
      resumeTerminalEvidence?: RecoveryCoordinator["resumeTerminalSource"];
    };
    const resume = proto.resumeTerminalEvidence;
    let capturedTail: unknown;
    type PendingMemory = { operation?: Promise<void>; settled?: boolean; executorRun?: Promise<void>;
      executorSettled?: boolean; closing?: Promise<void>; closingSettled?: boolean; ended: boolean };
    let memoryFlight: { workerMemory?: PendingMemory } | undefined;
    const tails = () => (runner as unknown as { executionTails: Map<string, unknown> }).executionTails;
    if (resume) t.mock.method(proto as { resumeTerminalEvidence: NonNullable<typeof resume> }, "resumeTerminalEvidence", function (this: RecoveryCoordinator,
      ...args: Parameters<typeof resume>) {
      notifications++;
      assert.equal(tails().has(claim.run_id), false, "notification follows exact tail release");
      pass = resume.apply(this, args);
      return pass;
    });
    const releases: Array<{ disposition?: RecoveryFinalDisposition; notifications: number; tailPresent: boolean }> = [];
    t.mock.method(client, "releaseRecoveryCustody", async (...[run, generation, _evidence, disposition]: Parameters<typeof client.releaseRecoveryCustody>) => {
      releases.push({ disposition, notifications, tailPresent: tails().get(run) === capturedTail });
      return { run_id: run, generation, released: true, holds_released: 1 };
    });
    const entered = deferred(), release = deferred();
    const stop = SteeringChannel.prototype.stop;
    t.mock.method(SteeringChannel.prototype, "stop", async function (this: SteeringChannel) {
      entered.resolve();
      await release.promise;
      await stop.call(this);
      if (mode === "steering-rejected") throw new Error("fixture steering join rejected");
    });
    let disposals = 0, replacementDisposals = 0;
    const codex = ["disposed", "incomplete", "legacy_in_process", "dispose-rejected", "executor-replaced"].includes(mode);
    const safety: CodexExecutionSafety | undefined = codex ? {
      kind: "codex",
      withBoundary: async (request, action) => action({ epoch: 1, boundary: request.boundary,
        signal: new AbortController().signal } as BoundaryPermit),
      spawnBoundaryProcess: async () => { throw new Error("no subprocess admission in fixture"); },
      dispose: async () => {
        disposals++;
        if (mode === "dispose-rejected") throw new Error("fixture disposal rejection");
        if (mode === "incomplete") return { kind: "incomplete", errors: [] };
        return { kind: mode === "legacy_in_process" ? "legacy_in_process" : "disposed" };
      },
    } : undefined;
    let commandFailures = 0;
    const unique = randomBytes(4096).toString("hex");
    const log = recordingLogger();
    const runner = runnerWith(() => ({
      executor: {
        safety,
        settleForCredentialFreeCapture: safety ? async () => ({ kind: "observed_empty", evidence: "supervisor_echild", epoch: 1 }) : undefined,
        settleWorkerCommands: mode.startsWith("commands-") ? async () => {
          if (uploads === 0) return { confirmed: true };
          commandFailures++;
          if (mode === "commands-rejected") throw new Error("fixture command settlement rejection");
          return { confirmed: false };
        } : undefined,
        run: async ctx => {
          command(ctx.worktreePath, "config", "user.name", "fixture");
          command(ctx.worktreePath, "config", "user.email", "fixture@example.com");
          await fs.writeFile(path.join(ctx.worktreePath, "current-work"), unique);
          command(ctx.worktreePath, "add", "current-work");
          command(ctx.worktreePath, "commit", "-m", "current ordinary work");
          throw new Error("ordinary executor failure");
        },
        reapAttributedProcesses: async () => ({ complete: true, killed: [], left: [] }),
      },
    }), fakeGitlab().gitlab, token, log.logger, {
      maintenanceExclusion: passingExclusion(), dockerHost: "http://unused.invalid", recoveryRetryMs: 1,
      quiesceRun: async () => ({ process: { state: "quiescent", processes: [], killed: [], detail: "" },
        docker: { state: "not_wired", removed: [], detail: "" } }),
      ...(memoryRuntime ? { memoryRuntime, codexBoundaryDeadlineMs: 200 } : {}),
    });
    let replaced = false;
    if (memoryMode) {
      const owner = runner as unknown as { archiveAccepted: (flight: { workerMemory?: PendingMemory }, record: unknown) => void };
      const accepted = owner.archiveAccepted;
      t.mock.method(owner, "archiveAccepted", (flight: Parameters<typeof accepted>[0], record: unknown) => {
        accepted.call(runner, flight, record);
        memoryFlight = flight;
        const never = new Promise<void>(() => {});
        const memory = flight.workerMemory;
        assert.ok(memory, "the claim enrolled a real worker memory flight");
        if (mode === "memory-operation-pending") { memory.operation = never; memory.settled = false; }
        if (mode === "memory-executor-pending") { memory.executorRun = never; memory.executorSettled = false; }
        if (mode === "memory-closing-pending") { memory.closing = never; memory.closingSettled = false; }
        if (mode === "memory-sink-receipt") {
          t.mock.getter(memoryRuntime!, "sinkReceipts", () => [{ context: { flight }, completion: never }] as never);
        }
      });
    }
    if (mode === "executor-replaced") {
      const owner = runner as unknown as {
        archiveAccepted: (flight: { executor: Executor }, record: unknown) => void;
      };
      const accepted = owner.archiveAccepted;
      assert.ok(accepted);
      t.mock.method(owner, "archiveAccepted", (flight: Parameters<typeof accepted>[0], record: unknown) => {
        accepted.call(runner, flight, record);
        if (!replaced) {
          // Retained recovery can replace flight.executor after preserving an archive.
          // A receipt from the old runtime cannot settle this new, unconfirmed owner.
          flight.executor = { ...flight.executor, safety: { ...safety!, dispose: async () => {
            replacementDisposals++;
            return { kind: "incomplete", errors: [] };
          } } };
          replaced = true;
        }
      });
    }
    const inspect = new RecoveryCoordinator({ client, git, workerToken: token, recoveryRoot: git.recoveryRoot,
      log: log.logger });
    const running = runner.execute(claim);
    try {
      await Promise.race([entered.promise, running]);
      assert.equal(uploads > 0, true, "a CURRENT real bundle upload was attempted: " + JSON.stringify(log.lines.slice(-10)));
      const before = await inspect.inspect(claim.run_id);
      if (mode !== "upload-rejected" && mode !== "upload-failed") {
        assert.ok(before.some(r => r.state === "uploaded" && r.selfContained === true && !r.finalRequest),
          "accepted CURRENT self-contained archive has no earlier minted FINAL");
      }
      capturedTail = tails().get(claim.run_id);
      assert.ok(capturedTail, "capture the exact execution tail before its release");
      assert.equal(notifications, 0, "actual steering join is still pending");
      assert.equal(releases.length, 0);
      if (mode === "shutdown") runner.shutdown();
      release.resolve();
      await running;
      await pass;
      if (mode.startsWith("commands-")) assert.ok(commandFailures > 0, "actual command settlement failed AFTER upload acceptance");
      if (mode === "executor-replaced") {
        assert.equal(replaced, true);
        assert.ok(disposals > 0, "the original runtime has a real disposal result");
        assert.equal(replacementDisposals, 0, "no disposal result confirms the replacement runtime");
      }
      if (memoryMode) {
        assert.ok(memoryFlight?.workerMemory, "the memory flight object survives teardown");
        assert.equal(memoryFlight.workerMemory.ended, true, "normal teardown ended the memory flight");
      }
      const expected = mode === "ordinary" || mode === "disposed" || mode === "memory-enrolled";
      if (expected) assert.equal(sourceNotifications, 0, "ordinary upload has no source-family notification");
      assert.equal(releases.length, expected ? 1 : 0, (expected ? "CURRENT archive gets a newly minted FINAL and ACK: " : "unconfirmed or rejected work cannot mint FINAL: ") + JSON.stringify({ records: (await inspect.inspect(claim.run_id)).map(r => ({ state: r.state, reason: r.reason, final: !!r.finalRequest, ack: r.finalAcknowledged })), logs: log.lines.slice(-5) }));
      assert.equal(notifications, expected ? 1 : 0, "accepted archive notification requires confirmed joins: " + JSON.stringify(log.lines.slice(-8)));
      for (const sent of releases) {
        assert.equal(sent.notifications, 1, "new FINAL is sent inside the notification");
        assert.equal(sent.tailPresent, false, "FINAL follows tail release");
        assert.equal(sent.disposition?.kind, "archive");
      }
      assert.deepEqual(sweeps, { boot: 0, live: 0 });
      if (expected) {
        const after = await inspect.inspect(claim.run_id);
        assert.ok(after.every(r => r.finalAcknowledged === true));
        assert.ok(after.every(r => !r.reason?.includes("execution_tail_present")));
      }
    } finally {
      release.resolve();
      await running;
      runner.shutdown();
      await runner.shutdownRecoveries();
      await inspect.shutdown();
    }
  });
}
