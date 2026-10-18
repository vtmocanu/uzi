import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { SteeringChannel } from "../src/steering.js";
import { rmTreePinned } from "../src/rmtree.js";
import type { Executor } from "../src/executor.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, deferred, runnerWith } from "./runner-harness.js";

installHarness();

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}
type Observation = { genesis: { runId: string; generation: number; clonePath: string; attemptId: string }; family?: unknown };
type Tails = { executionTails: ReadonlyMap<string, Promise<Observation | undefined>> };

for (const mode of ["normal", "unapplied", "lost", "stale", "wrong-status", "missing-status",
  "missing-process", "missing-reproof", "unverified", "failed-reap", "late-fence",
  "failed-home", "failed-skills", "failed-owed", "record-none", "prior-record-none", "reclaimed", "nonmatching-error", "post-run-error"] as const) {
it(`#3031 fresh ordinary FAILED unsaved source: ${mode}`, async (t) => {
  const claim = gitlabClaim(3031, { claim_generation: 1, inventory_guarded: true });
  const feature = client.hasFeature.bind(client);
  t.mock.method(client, "hasFeature", (name: Parameters<typeof client.hasFeature>[0]) =>
    name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name));
  t.mock.method(client, "listRecoveryHolds", async () => ({ run_id: claim.run_id, holds: [{ hold_id: "fixture-hold", generation: 1, inventory_guarded: true, has_available_capture: false }] }));
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
      docker: { state: "not_wired", removed: [], detail: "" },
    }),
  });
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
  const executing = runner.execute(claim);
  const tails = (runner as unknown as Tails).executionTails;
  const tail = tails.get(claim.run_id)!;
  try {
    await executing;
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
    if (mode === "normal") {
      assert.ok(observed?.family, "fresh normal FAILED returns positive source family: " + JSON.stringify(log.lines.slice(-5)));
      assert.equal(observed.genesis.clonePath, source);
      assert.equal(observed.genesis.generation, 1);
      await assert.rejects(fs.stat(target), { code: "ENOENT" });
    } else assert.equal(observed, undefined, "uncertain or stopped source stays unknown");
    assert.equal(tails.has(claim.run_id), false);
  } finally {
    releaseAck.resolve();
    runner.shutdown();
    await executing;
  }
});
}
