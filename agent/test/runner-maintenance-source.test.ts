import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { PauseNowSignal, SteeringChannel } from "../src/steering.js";
import { nullLogger } from "./helpers.js";
import {
  api, client, deferred, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith,
} from "./runner-harness.js";

installHarness();

// Read the actual pending tail, without injecting provenance or replacing execution.
// An observation names one fresh source; it never proves source-family completeness.
type TailView = { executionTails: ReadonlyMap<string, Promise<unknown>> };

for (const mode of ["normal", "latched-pause", "failed-stop", "late-shutdown", "late-fence", "failed-reap", "failed-cache", "failed-skills", "failed-retire", "failed-retired-append", "failed-holding-dispose", "failed-scratch-dispose", "failed-owed", "retention-failure", "canonical", "adopted-source"] as const) {
  it(`#3031 owner pause ${mode}: exact fresh source is observed only after cleanup`, async (t) => {
    const claim = gitlabClaim(3033, { claim_generation: 1 });
    const started = deferred(), finish = deferred(), cleaning = deferred(), finishCleanup = deferred();
    let expected: unknown;
    let parked = false;
    const seed = git.runnerCloneForBranch.bind(git);
    t.mock.method(git, "runnerCloneForBranch", async (...args: Parameters<typeof seed>) => {
      const attempt = args[7];
      if (attempt) {
        const onSeeded = attempt.onSeeded;
        args[7] = { ...attempt, onSeeded: clonePath => {
          expected = { genesis: { runId: claim.run_id, generation: 1, clonePath, attemptId: attempt.attemptId } };
          // Model an adoption seam that supplies a path without a fresh seed callback.
          if (mode !== "adopted-source") onSeeded?.(clonePath);
        } };
      }
      return seed(...args);
    });
    if (mode === "retention-failure") {
      // Main's retained-artifact pass is diagnostic only. Its failure cannot
      // invalidate the separately joined, positively proven source observation.
      t.mock.method(git as unknown as { diagnoseRetainedArtifacts(): Promise<void> }, "diagnoseRetainedArtifacts",
        async () => { throw new Error("retention diagnostic failed after onSeeded"); });
    }
    if (mode === "failed-owed") t.mock.method(git, "retainCurrentOwedCandidate", async () => ({ kind: "not_updated" as const, reason: "preservation_failed" as const }));
    if (mode === "failed-retire") t.mock.method(git, "retireRunnerClone", async () => { throw new Error("retire failed"); });
    let internalFaults = 0;
    const warnings: string[] = [];
    const runLogger = nullLogger();
    if (mode === "failed-retired-append" || mode === "failed-holding-dispose" || mode === "failed-scratch-dispose") {
      const logger = (git as unknown as { log: ReturnType<typeof nullLogger> }).log;
      t.mock.method(logger, "warn", (message: string) => { warnings.push(message); });
      t.mock.method(runLogger, "warn", (message: string) => { warnings.push(message); });
      if (mode === "failed-retired-append") {
        // Fail only the retired append; seeding and all physical cleanup stay real.
        const ledger = git as unknown as {
          appendAttemptLedger(bare: string, branch: string, entry: { state: string }): Promise<void>;
        };
        const append = ledger.appendAttemptLedger.bind(ledger);
        t.mock.method(ledger, "appendAttemptLedger", async (...args: Parameters<typeof append>) => {
          if (args[2].state === "retired") {
            internalFaults++;
            throw new Error("injected retired ledger append failure");
          }
          return append(...args);
        });
      } else {
        const holdingRoot = path.join(fx.dataDir, "runner-quarantine");
        if (mode === "failed-scratch-dispose") {
          // Model the cross-device holding move, retaining the actual intra-device rename.
          const rename = fs.rename.bind(fs);
          t.mock.method(fs, "rename", async (...args: Parameters<typeof rename>) => {
            if (String(args[1]).startsWith(holdingRoot + path.sep))
              throw Object.assign(new Error("cross-device holding move"), { code: "EXDEV" });
            return rename(...args);
          });
        }
        const rm = fs.rm.bind(fs);
        t.mock.method(fs, "rm", async (...args: Parameters<typeof rm>) => {
          const target = String(args[0]);
          const disposal = mode === "failed-holding-dispose"
            ? target.startsWith(holdingRoot + path.sep)
            : path.dirname(target) === path.join(fx.dataDir, "runner") && path.basename(target).startsWith(".retire-");
          if (disposal) {
            internalFaults++;
            throw new Error("injected retirement disposal failure");
          }
          return rm(...args);
        });
      }
    }
    const ackStarted = deferred(), releaseAck = deferred();
    if (mode === "late-fence") {
      const ack = client.ackInputs.bind(client);
      t.mock.method(client, "ackInputs", async (...args: Parameters<typeof ack>) => {
        ackStarted.resolve();
        await releaseAck.promise;
        return ack(...args);
      });
    }
    const stop = SteeringChannel.prototype.stop;
    t.mock.method(SteeringChannel.prototype, "stop", async function (this: SteeringChannel) {
      if (mode === "late-fence") {
        assert.equal(this.claimFence(), undefined);
        api.setInputClaimGeneration(claim.run_id, 2);
        releaseAck.resolve();
      }
      await stop.call(this);
      if (mode === "late-fence") assert.equal(this.claimFence(), "stale");
      if (mode === "failed-stop") throw new Error("steering join failed");
    });
    // The publish transport is stubbed; this is a runner seam test, not a
    // filesystem-completeness or remote-durability proof.
    t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof client.publishCheckpoint>) => {
      const pack = args[2];
      for await (const chunk of pack) void chunk;
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/fixture" } };
    });
    const runner = runnerWith(() => ({
      homeDir: path.join(homeDir, claim.run_id),
      executor: {
        run: async ctx => {
          await fs.writeFile(path.join(ctx.worktreePath, "pause-work.txt"), "fresh work\n");
          if (mode === "failed-cache") {
            const target = path.join(homeDir, claim.run_id);
            await fs.mkdir(path.join(target, "kept"), { recursive: true });
            await fs.symlink(path.join(target, "kept"), path.join(target, ".cache"));
          }
          if (mode === "late-fence" || mode === "latched-pause") {
            api.setInputClaimGeneration(claim.run_id, 1);
            api.setInputs(claim.run_id, [{ id: 1, kind: mode === "latched-pause" ? "pause" : "follow_up", body: mode === "latched-pause" ? "now" : "fixture input" }]);
            if (mode === "late-fence") await ackStarted.promise;
            else {
              const signal = ctx.signal!;
              if (!signal.aborted) await new Promise<void>(resolve => signal.addEventListener("abort", () => resolve(), { once: true }));
              assert.equal(signal.reason instanceof PauseNowSignal, true);
            }
          }
          started.resolve();
          await finish.promise;
          const pausedAt = { completedCount: 0 };
          assert.equal(await ctx.parkForPause?.(pausedAt), true, "actual owner park accepted");
          parked = true;
          return { branch: ctx.branch, pausedAt };
        },
        reapAttributedProcesses: async () => {
          if (!parked) return { complete: true, killed: [], left: [] };
          cleaning.resolve();
          await finishCleanup.promise;
          return { complete: mode !== "failed-reap", killed: [], left: [] };
        },
      },
    }), fakeGitlab().gitlab, undefined, runLogger, {
      ...(mode === "canonical" ? {} : { dockerHost: "http://unused.invalid" }),
      ...(mode === "failed-skills" ? { teardownTestDeps: { removeTreePinned: async () => { throw new Error("skills cleanup failed"); } } } : {}),
      quiesceRun: async () => ({
        process: { state: "quiescent", processes: [], killed: [], detail: "" },
        docker: { state: "not_wired", removed: [], detail: "" },
      }),
    });
    const executing = runner.execute(claim);
    try {
      await Promise.race([started.promise, executing.then(() => { throw new Error("executor never started"); })]);
      const tails = (runner as unknown as TailView).executionTails;
      const tail = tails.get(claim.run_id)!;
      let released = false;
      void tail.then(() => { released = true; });
      finish.resolve();
      await Promise.race([cleaning.promise, executing.then(() => { throw new Error("final cache reap never started"); })]);
      assert.equal(released, false, "pending tail includes the final cleanup join");
      if (mode === "late-shutdown") runner.shutdown();
      finishCleanup.resolve();
      assert.equal(await executing, undefined, "public execute remains void");
      assert.equal(api.states.some(s => s.body.status === "paused"), true);
      if (mode === "normal" || mode === "latched-pause" || mode === "retention-failure") {
        assert.ok(expected, "real attempt onSeeded ran");
        assert.deepEqual(await tail, { ...expected as object, terminal: "normal_park" });
      } else assert.equal(await tail, undefined);
      if (mode === "failed-retired-append" || mode === "failed-holding-dispose" || mode === "failed-scratch-dispose") {
        assert.equal(internalFaults, 1, "actual retirement reached the injected internal failure once");
        if (mode === "failed-retired-append") {
          assert.ok(warnings.includes("retireRunnerClone: could not record the attempt as retired; the gone path is compacted later"));
          assert.deepEqual(await fs.readdir(path.join(fx.dataDir, "runner-quarantine")), [], "ledger failure still disposes holding");
        } else if (mode === "failed-holding-dispose") {
          assert.ok(warnings.includes("retireRunnerClone: holding dispose failed"));
          assert.equal((await fs.readdir(path.join(fx.dataDir, "runner-quarantine"))).length, 1);
        } else {
          assert.equal((await fs.readdir(path.join(fx.dataDir, "runner"))).filter(name => name.startsWith(".retire-")).length, 1);
        }
        assert.ok(!warnings.includes("runner clone cleanup failed"), "helper still resolves after its internal failure");
      }
      assert.equal(tails.has(claim.run_id), false, "no completed registry");
    } finally {
      finish.resolve();
      finishCleanup.resolve();
      releaseAck.resolve();
      runner.shutdown();
      await executing;
    }
  });
}

for (const mode of ["fresh", "canonical", "failed-stop", "late-shutdown", "failed-home"] as const) {
  it(`#3031 ${mode}: pending source tail joins final HOME cleanup and retains no completed observation`, async (t) => {
    const claim = gitlabClaim(3031, { claim_generation: 1 });
    const started = deferred(), finish = deferred(), cleaning = deferred(), finishCleanup = deferred();
    const target = path.join(homeDir, claim.run_id);
    let executions = 0;
    const paths: string[] = [];
    if (mode === "failed-stop") {
      const stop = SteeringChannel.prototype.stop;
      t.mock.method(SteeringChannel.prototype, "stop", async function (this: SteeringChannel) {
        await stop.call(this);
        throw new Error("owned steering join failed");
      });
    }
    const runner = runnerWith(() => ({
      homeDir: target,
      executor: { run: async ctx => {
        executions++;
        paths.push(ctx.worktreePath);
        if (executions === 1) {
          await fs.mkdir(target, { recursive: true });
          started.resolve();
          await finish.promise;
        }
        throw new Error("fixture ends without publishing");
      } },
    }), fakeGitlab().gitlab, undefined, nullLogger(), {
      ...(mode === "canonical" ? {} : { dockerHost: "http://unused.invalid",
        quiesceRun: async () => ({
          process: { state: "quiescent" as const, processes: [], killed: [], detail: "" },
          docker: { state: "not_wired" as const, removed: [], detail: "" },
        }) }),
      teardownTestDeps: { removeTreePinned: async (parent, name) => {
        if (path.join(parent, name) === target) {
          cleaning.resolve();
          await finishCleanup.promise;
          if (mode === "failed-home") throw new Error("HOME join failed");
        }
        await fs.rm(path.join(parent, name), { recursive: true, force: true });
        return "removed";
      } },
    });
    const tails = (runner as unknown as TailView).executionTails;
    const first = runner.execute(claim);
    let second = Promise.resolve();
    try {
      await Promise.race([started.promise, first.then(() => { throw new Error("executor never started"); })]);
      const firstTail = tails.get(claim.run_id)!;
      assert.ok(firstTail);
      let released = false;
      void firstTail.then(() => { released = true; });
      second = runner.execute({ ...claim, claim_generation: 2 });
      const secondTail = tails.get(claim.run_id)!;
      assert.notEqual(secondTail, firstTail);
      api.setOwnershipStatus(claim.run_id, "running", 2);
      finish.resolve();
      await Promise.race([cleaning.promise, first.then(() => { throw new Error("HOME cleanup never started"); })]);
      assert.equal(released, false, "tail must include cleanup after steering.stop");
      assert.equal(executions, 1, "queued successor cannot execute during final cleanup");
      assert.equal(runner.isExecuting(claim.run_id), true);
      if (mode === "late-shutdown") runner.shutdown();
      finishCleanup.resolve();
      assert.equal(await first, undefined, "public execute stays void");
      assert.equal(await firstTail, undefined, "an unsupported exit cannot qualify from fresh seeding alone");
      assert.equal(await second, undefined);
      assert.equal(await secondTail, undefined, "unknown predecessors cannot be laundered");
      assert.equal(tails.has(claim.run_id), false, "no completed provenance registry");
      assert.equal(runner.isExecuting(claim.run_id), false);
      assert.equal(path.basename(paths[0]!).includes(".attempt-"), mode !== "canonical");
      if (mode !== "late-shutdown") assert.equal(executions, 2, "ordinary queued execution still proceeds");
    } finally {
      finish.resolve();
      finishCleanup.resolve();
      runner.shutdown();
      await Promise.allSettled([first, second]);
    }
  });
}

it("#3031 actual stale ACK arriving during steering.stop cannot qualify a fresh source", async (t) => {
  const claim = gitlabClaim(3032, { claim_generation: 1 });
  const ackStarted = deferred(), releaseAck = deferred(), finish = deferred();
  const ack = client.ackInputs.bind(client);
  t.mock.method(client, "ackInputs", async (...args: Parameters<typeof ack>) => {
    ackStarted.resolve();
    await releaseAck.promise;
    return ack(...args);
  });
  const stop = SteeringChannel.prototype.stop;
  let stopCalls = 0;
  let fenceAfterStop: string | undefined;
  t.mock.method(SteeringChannel.prototype, "stop", async function (this: SteeringChannel) {
    if (++stopCalls === 1) {
      assert.equal(this.claimFence(), undefined, "catch selection has not observed the stale ACK");
      api.setInputClaimGeneration(claim.run_id, 2);
      releaseAck.resolve();
      await stop.call(this);
      fenceAfterStop = this.claimFence();
    } else await stop.call(this);
  });
  const runner = runnerWith(() => ({ executor: { run: async () => {
    api.setInputClaimGeneration(claim.run_id, 1);
    api.setInputs(claim.run_id, [{ id: 1, kind: "follow_up", body: "fixture input" }]);
    await ackStarted.promise;
    await finish.promise;
    throw new Error("normal failure before delayed stale receipt");
  } } }), fakeGitlab().gitlab, undefined, nullLogger(), {
    dockerHost: "http://unused.invalid",
    quiesceRun: async () => ({
      process: { state: "quiescent", processes: [], killed: [], detail: "" },
      docker: { state: "not_wired", removed: [], detail: "" },
    }),
  });
  const first = runner.execute(claim);
  try {
    await Promise.race([ackStarted.promise, first.then(() => { throw new Error("ACK never started"); })]);
    const tail = (runner as unknown as TailView).executionTails.get(claim.run_id)!;
    finish.resolve();
    await first;
    assert.equal(fenceAfterStop, "stale", "real receipt processing fences during the join");
    assert.equal(await tail, undefined);
    assert.equal(runner.isExecuting(claim.run_id), false);
  } finally {
    releaseAck.resolve();
    finish.resolve();
    runner.shutdown();
    await Promise.allSettled([first]);
  }
});
