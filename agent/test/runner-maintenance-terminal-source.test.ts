import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { DataVolumeGuard } from "../src/disk-full.js";
import { WorkerClient } from "../src/client.js";
import { SteeringChannel } from "../src/steering.js";
import { rmTreePinned } from "../src/rmtree.js";
import type { Executor } from "../src/executor.js";
import { MaintenanceResources } from "../src/recovery-maintenance.js";
import { canonicalJson, RecoveryCoordinator } from "../src/recovery.js";
import { recordingLogger, nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, deferred, runnerWith } from "./runner-harness.js";

installHarness();

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}
type Observation = { terminal?: string; genesis: { runId: string; generation: number; clonePath: string; attemptId: string }; family?: unknown };
type Tails = { executionTails: ReadonlyMap<string, Promise<Observation | undefined>> };

for (const mode of ["normal", "origin-admission", "origin-pressure", "origin-source-limit", "notification-throw", "notification-reject", "unapplied", "lost", "stale", "wrong-status", "missing-status",
  "missing-process", "missing-reproof", "unverified", "failed-reap", "late-fence",
  "failed-home", "failed-skills", "failed-owed", "record-none", "prior-record-none", "reclaimed", "nonmatching-error", "post-run-error"] as const) {
it(`#3031 fresh ordinary FAILED unsaved source: ${mode}`, async (t) => {
  const originMode = mode === "origin-admission" || mode === "origin-pressure" || mode === "origin-source-limit";
  let pressure = false;
  const guard = new DataVolumeGuard({ dataDir: path.dirname(git.recoveryRoot) });
  t.mock.method(guard, "preflight", () => pressure ? "data_volume_full" : "not_disk_full");
  const claim = gitlabClaim(3031, { claim_generation: 1, inventory_guarded: true });
  const feature = client.hasFeature.bind(client);
  t.mock.method(client, "hasFeature", (name: Parameters<typeof client.hasFeature>[0]) =>
    name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name));
  t.mock.method(client, "listRecoveryHolds", async () => ({ run_id: claim.run_id, holds: [{ hold_id: "fixture-hold", generation: 1, inventory_guarded: true, has_available_capture: false }] }));
  if (originMode) {
    claim.repo.id = "00000000-0000-4000-8000-000000000033";
    const workerId = "00000000-0000-4000-8000-000000000034";
    const holdId = "00000000-0000-4000-8000-000000000032";
    // The production strict client validates the complete unique inventory and exact
    // terminal hold; the runner still produces its own private original observation.
    const strict = new WorkerClient("http://origin.test", "fixture-journal-key", "test", nullLogger(), {
      fetch: async input => {
        const url = new URL(String(input));
        if (url.pathname.endsWith("/register")) return Response.json({ worker_id: workerId, protocol_features: [] });
        return Response.json(url.search ? { run_id: claim.run_id, exact_hold: {
          run_id: claim.run_id, hold_id: holdId, generation: 1, repo_id: claim.repo.id,
          original_worker_id: workerId, original_worker_identity: "human-label", live_worker_id: workerId,
          live_run_id: claim.run_id, inventory_guarded: true, state: "open", run_status: "failed",
          current_claim_generation: 1, claim_released: true,
        } } : { run_id: claim.run_id, holds: [{ hold_id: holdId, generation: 1,
          inventory_guarded: true, has_available_capture: false }] });
      },
    });
    await strict.register("worker", undefined, 1, undefined, ["recovery_held_publication_v2"]);
    t.mock.method(client, "observeTerminalMaintenanceHold", strict.observeTerminalMaintenanceHold.bind(strict));
    t.mock.method(client, "isCurrentTerminalMaintenanceHold", strict.isCurrentTerminalMaintenanceHold.bind(strict));
  }
  if (mode === "origin-source-limit") {
    let priorWork = false;
    const scan = git.matchMaintenanceSourceFamily.bind(git);
    t.mock.method(git, "matchMaintenanceSourceFamily", async (...args: Parameters<typeof scan>) => {
      if (MaintenanceResources.active() && !MaintenanceResources.discoveryActive() && !priorWork) {
        priorWork = true;
        MaintenanceResources.consumeIfActive("sources", 127);
      }
      return scan(...args);
    });
  }
  let attempts = 0, models = 0, source = "", branch = "", committed = "";
  const unique = randomBytes(16 * 1024).toString("hex");
  // Fail EVERY production attempt before any bundle bytes or immutable installation.
  t.mock.method(git, "produceRecoveryBundle", async () => {
    attempts++;
    throw new Error("fixture bundle production unavailable before installation");
  });
  const ackStarted = deferred(), releaseAck = deferred();
  if (mode === "late-fence") {
    const ack = client.ackInputs.bind(client);
    t.mock.method(client, "ackInputs", async (...args: Parameters<typeof ack>) => {
      ackStarted.resolve();
      await releaseAck.promise;
      return ack(...args);
    });
    const stop = SteeringChannel.prototype.stop;
    t.mock.method(SteeringChannel.prototype, "stop", async function (this: SteeringChannel) {
      api.setInputClaimGeneration(claim.run_id, 2);
      releaseAck.resolve();
      await stop.call(this);
      assert.equal(this.claimFence(), "stale");
    });
  }
  let terminalAcks = 0;
  const report = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
    const ack = await report(...args);
    if (args[1].status !== "failed") return ack;
    assert.equal(ack.applied, true, "actual server applied FAILED");
    assert.equal(ack.status, "failed");
    terminalAcks++;
    if (mode === "lost") throw new Error("lost failed ACK");
    if (mode === "unapplied") return { ...ack, applied: false };
    if (mode === "stale") return { ...ack, staleClaim: true };
    if (mode === "wrong-status") return { ...ack, status: "running" };
    if (mode === "missing-status") return { ...ack, status: undefined };
    return ack;
  });
  if (mode === "failed-owed") {
    const retain = git.retainCurrentOwedCandidate.bind(git);
    t.mock.method(git, "retainCurrentOwedCandidate", async (...args: Parameters<typeof retain>) => {
      // Leave transfer/freeze real; fail the final cleanup join only.
      if (terminalAcks) return { kind: "not_updated" as const, reason: "preservation_failed" as const };
      return retain(...args);
    });
  }
  const log = recordingLogger();
  const target = path.join(homeDir, claim.run_id);
  const runner = runnerWith(() => ({
    homeDir: target,
    executor: {
      run: async ctx => {
        models++;
        source = ctx.worktreePath;
        branch = ctx.branch;
        command(source, "config", "user.name", "fixture");
        command(source, "config", "user.email", "fixture@example.com");
        await fs.writeFile(path.join(source, "unique-commit"), unique);
        command(source, "add", "unique-commit");
        command(source, "commit", "-m", "unique local work");
        committed = command(source, "rev-parse", "HEAD");
        await fs.writeFile(path.join(source, "README.md"), "tracked " + unique);
        await fs.writeFile(path.join(source, "unique-untracked"), "untracked " + unique);
        await fs.mkdir(target, { recursive: true });
        if (mode === "late-fence") {
          api.setInputClaimGeneration(claim.run_id, 1);
          api.setInputs(claim.run_id, [{ id: 1, kind: "follow_up", body: "fixture" }]);
          await ackStarted.promise;
        }
        if (mode === "post-run-error") {
          ctx.codeCrossCheckSummary = async () => { throw new Error("post-execution summary failure"); };
          return { branch: ctx.branch };
        }
        throw new Error("ordinary executor failure");
      },
      reapAttributedProcesses: async () => ({ complete: mode !== "failed-reap", killed: [], left: [] }),
    },
  }), fakeGitlab().gitlab, "fixture-journal-key", log.logger, {
    dockerHost: "http://unused.invalid", recoveryRetryMs: 1,
    ...(mode === "origin-pressure" ? { dataVolume: guard } : {}),
    ...(mode === "failed-home" || mode === "failed-skills" ? {
      teardownTestDeps: { removeTreePinned: async (parent, name, options) => {
        const isHome = path.join(parent, name) === target;
        const isSkills = name.startsWith(".uzi-skills-");
        if ((mode === "failed-home" && isHome) || (mode === "failed-skills" && isSkills))
          throw new Error("fixture cleanup failed");
        return rmTreePinned(parent, name, options);
      } },
    } : {}),
    quiesceRun: async request => ({
      process: models && (mode === "missing-process" || (mode === "missing-reproof" && request.site === "settle_transfer:after_runner_git"))
        ? undefined : { state: models && mode === "unverified" ? "unverified" : "quiescent", processes: [], killed: [], detail: "" },
      docker: { state: originMode && request.site === "retained_ops" ? "docker_unconfirmed" : "not_wired", removed: [], detail: "" },
    }),
  });
  if (originMode) {
    const boundary = runner.withRetainedSourceBoundary.bind(runner);
    t.mock.method(runner, "withRetainedSourceBoundary", async (...[runId, paths, action]: Parameters<typeof boundary>) =>
      boundary(runId, paths, async ctx => {
        const bare = git.barePathFor(claim.repo.clone_url);
        const snapshot = async () => ({
          bareConfig: await fs.readFile(path.join(bare, "config"), "utf8"),
          refs: command(bare, "show-ref"),
          index: await fs.readFile(path.join(source, ".git", "index")),
          config: await fs.readFile(path.join(source, ".git", "config"), "utf8"),
          head: command(source, "rev-parse", "HEAD"),
          tracked: await fs.readFile(path.join(source, "README.md"), "utf8"),
          untracked: await fs.readFile(path.join(source, "unique-untracked"), "utf8"),
        });
        if (mode === "origin-admission") await fs.writeFile(path.join(source, "maintenance-wip"), unique);
        const before = await snapshot(), beforeAttempts = attempts, beforeModels = models;
        const upload = t.mock.method(client, "uploadRecoveryBundle");
        const reserve = t.mock.method(client, "reserveRecoveryCapture");
        const held = t.mock.method(client, "publishHeld");
        const result = await action(ctx);
        assert.equal(models, beforeModels);
        assert.equal(upload.mock.callCount(), 0);
        assert.equal(reserve.mock.callCount(), 0);
        assert.equal(held.mock.callCount(), 0);
        upload.mock.restore(); reserve.mock.restore(); held.mock.restore();
        const after = await snapshot();
        if (mode === "origin-admission") {
          assert.notEqual(after.head, before.head);
          assert.notDeepEqual(after.index, before.index);
          assert.deepEqual({ ...after, head: before.head, index: before.index }, before);
          assert.equal(command(source, "rev-parse", "HEAD^"), before.head);
          assert.equal(command(source, "status", "--porcelain"), "");
        } else assert.deepEqual(after, before, "refusal leaves source unchanged");
        assert.equal(attempts, beforeAttempts, "origin publication grants no capture permit");
        return result;
      }));
  }
  if (mode === "record-none" || mode === "prior-record-none" || mode === "nonmatching-error") {
    // Exercise the real retention helper before generic reporting. No observation
    // fields are injected: even an ACK after record:none must not undo its denial.
    type Flight = { executor: Executor };
    const view = runner as unknown as {
      reportGenericFailure(claim: ReturnType<typeof gitlabClaim>, flight: Flight, error: unknown): Promise<void>;
      phasePreflightHandoff(claim: ReturnType<typeof gitlabClaim>, flight: Flight, session: string | undefined,
        rejection: { rejected: boolean; value?: unknown }): Promise<unknown>;
      retainStoppedSource(flight: Flight, executor: Executor, reason: string,
        handler: { memo: boolean; stale: boolean; record: "none" }): Promise<unknown>;
    };
    const retain = (flight: Flight) => view.retainStoppedSource(flight, flight.executor, "fixture record none",
      { memo: false, stale: false, record: "none" });
    if (mode === "prior-record-none" || mode === "nonmatching-error") {
      const handoff = view.phasePreflightHandoff.bind(view);
      t.mock.method(view, "phasePreflightHandoff", async (...args: Parameters<typeof handoff>) => {
        try { return await handoff(...args); }
        catch (error) {
          if (mode === "nonmatching-error") throw new Error("different dispatch failure");
          await retain(args[1]);
          throw error;
        }
      });
    } else {
      const generic = view.reportGenericFailure.bind(view);
      t.mock.method(view, "reportGenericFailure", async (...args: Parameters<typeof generic>) => {
        await retain(args[1]);
        return generic(...args);
      });
    }
  }
  if (mode === "reclaimed") {
    const stop = SteeringChannel.prototype.stop;
    t.mock.method(SteeringChannel.prototype, "stop", async function (this: SteeringChannel) {
      await stop.call(this);
      const bare = git.barePathFor(claim.repo.clone_url);
      const journal = JSON.parse(command(bare, "config", "--get", "uzi-recovery." + branch + ".clone"));
      command(bare, "config", "--add", "uzi-attempts." + branch + ".entry", JSON.stringify({
        attemptId: journal.attemptId, runId: journal.runId, clonePath: journal.clonePath, state: "reclaimed",
      }));
    });
  }
  const scanEntered = deferred(), scanRelease = deferred();
  const open = fs.opendir.bind(fs);
  if (mode === "normal") t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    if (notifiedTail && String(args[0]) === path.join(git.recoveryRoot, claim.run_id)) {
      scanEntered.resolve();
      await scanRelease.promise;
    }
    return open(...args);
  });
  let notifiedWork: Promise<void> | undefined;
  let notifiedTail: Promise<unknown> | undefined;
  let notifiedWhileReserved = false;
  const resume = RecoveryCoordinator.prototype.resumeTerminalSource;
  t.mock.method(RecoveryCoordinator.prototype, "resumeTerminalSource", function (this: RecoveryCoordinator,
    ...args: Parameters<typeof resume>) {
    notifiedTail = args[1];
    notifiedWhileReserved = (runner as unknown as Tails).executionTails.get(claim.run_id) === args[1];
    if (mode === "notification-throw") throw new Error("notification failure fixture");
    if (mode === "notification-reject") return Promise.reject(new Error("notification failure fixture"));
    if (mode === "origin-pressure") pressure = true;
    notifiedWork = resume.apply(this, args);
    return notifiedWork;
  });
  const executing = runner.execute(claim);
  const tails = (runner as unknown as Tails).executionTails;
  const tail = tails.get(claim.run_id)!;
  try {
    await executing;
    if (originMode) await notifiedWork;
    assert.equal(models, 1);
    assert.ok(api.states.some(s => s.body.status === "failed"));
    assert.ok(terminalAcks > 0, "actual FAILED ACK was received before fixture faults");
    const records = await new RecoveryCoordinator({
      client, git, log: log.logger, recoveryRoot: git.recoveryRoot, workerToken: "fixture-journal-key",
    }).snapshotBootRecords();
    const snapshotProven = !["unverified", "failed-reap", "record-none", "prior-record-none"].includes(mode);
    if (snapshotProven) {
      assert.ok(attempts > 0, "actual archive production was attempted: " + JSON.stringify(log.lines));
      assert.ok(records.length > 0, "authenticated boot records survive failed production");
      assert.ok(records.every(r => r.runId === claim.run_id && r.generation === 1));
      assert.ok(records.some(r => r.reason === "bundle_failed"), "authenticated failed-production record remains");
    }
    if (originMode) {
      const prefixes = records.filter(r => r.captureId.startsWith("maintenance-v1-"));
      const originalObservation = await tail;
      assert.ok((originalObservation as unknown as { originalContext?: unknown })?.originalContext, JSON.stringify(originalObservation));
      assert.equal(prefixes.length, mode === "origin-admission" ? 1 : 0, JSON.stringify({ records: records.map(r => ({ id: r.captureId, state: r.state })), logs: log.lines.slice(-3) }));
      if (mode === "origin-pressure") {
        assert.equal(guard.preflight(), "data_volume_full");
        const ops = await runner.withRetainedSourceBoundary(claim.run_id, [source], async ctx => {
          assert.equal(ctx.open(), true, "operator admission keeps its existing full-volume semantics");
          assert.equal(await ctx.quiesce([source]), true);
        });
        assert.equal(ops.entered, true);
        await assert.rejects(fs.stat(path.join(git.recoveryRoot, "..", "recovery-maintenance")), { code: "ENOENT" });
      } else if (mode === "origin-source-limit") {
        const ledger = JSON.parse(await fs.readFile(path.join(git.recoveryRoot, "..", "recovery-maintenance", "complete.json"), "utf8")).value;
        assert.equal(ledger.cycles[0].slots.length, 1, "paid descriptor refusal consumes its existing slot");
        assert.equal(ledger.cycles[0].fenced, true);
      } else {
        const prefix = prefixes[0];
        assert.ok(prefix);
        const resultHead = command(source, "rev-parse", "HEAD");
        assert.equal(prefix.sourceSha, command(source, "rev-parse", "HEAD^"), JSON.stringify(log.lines.slice(-5)));
        assert.equal(prefix.attemptedHeadSha, undefined);
        const ledger = JSON.parse(await fs.readFile(path.join(git.recoveryRoot, "..", "recovery-maintenance", "complete.json"), "utf8")).value;
        const cycle = ledger.cycles[0];
        const output = createHash("sha256").update(canonicalJson(["maintenance-wip-v1", cycle.identity, resultHead])).digest("hex");
        assert.ok(cycle.outputs.includes(output));
        assert.equal(cycle.slots.length, 1);
        assert.equal(cycle.fenced, true);
        assert.equal(prefix.heldPolicy, undefined);
        assert.equal(prefix.generation, 1);
      }
    }
    assert.equal(records.filter(r => r.bundlePath).length, 0);
    const artifacts = await fs.readdir(git.recoveryRoot, { recursive: true });
    assert.deepEqual(artifacts.filter(p => p.includes(".bundle")), [], "no physical installed or temporary bundles");
    assert.equal(command(source, "show", committed + ":unique-commit"), unique);
    assert.equal(await fs.readFile(path.join(source, "README.md"), "utf8"), "tracked " + unique);
    assert.equal(await fs.readFile(path.join(source, "unique-untracked"), "utf8"), "untracked " + unique);
    if (snapshotProven) {
      assert.equal(command(source, "show", "HEAD:README.md"), "tracked " + unique);
      assert.equal(command(source, "show", "HEAD:unique-untracked"), "untracked " + unique);
    }
    const bare = git.barePathFor(claim.repo.clone_url);
    const journal = JSON.parse(command(bare, "config", "--get", "uzi-recovery." + branch + ".clone"));
    assert.equal(journal.clonePath, source, "plain source journal physically retained");
    const config = command(bare, "config", "--local", "--list");
    assert.ok(!config.includes("uzi-stopped."));
    assert.equal(config.includes('"state":"reclaimed"'), mode === "reclaimed");
    assert.ok(config.includes('"state":"live"'), "live attempt attribution remains");
    const observed = await tail;
    if (mode === "normal" || originMode || mode === "notification-throw" || mode === "notification-reject") {
      assert.equal(notifiedTail, tail, "automatic notification captures the exact pending local tail");
      assert.equal(notifiedWhileReserved, true, "notification precedes original map deletion");
      assert.equal(observed?.terminal, "normal_failed");
      if (mode === "normal") {
        await scanEntered.promise;
        let joined = false;
        const drain = runner.shutdownRecoveries().then(() => { joined = true; });
        await new Promise<void>(resolve => setImmediate(resolve));
        assert.equal(joined, false, "execute returned while coordinator still owns the metadata child");
        scanRelease.resolve();
        await drain;
      } else if (!originMode) {
        assert.ok(JSON.stringify(log.lines).includes("recovery: terminal saved evidence pass failed; source retained"));
        assert.ok(!JSON.stringify(log.lines).includes("notification failure fixture"));
      }
      assert.ok(observed?.family, "fresh normal FAILED returns positive source family: " + JSON.stringify(log.lines.slice(-5)));
      assert.equal(observed.genesis.clonePath, source);
      assert.equal(observed.genesis.generation, 1);
      await assert.rejects(fs.stat(target), { code: "ENOENT" });
    } else assert.equal(observed, undefined, "uncertain or stopped source stays unknown");
    assert.equal(tails.has(claim.run_id), false);
  } finally {
    releaseAck.resolve();
    scanRelease.resolve();
    runner.shutdown();
    await runner.shutdownRecoveries();
    await executing;
  }
});
}
