import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Executor, RunContext } from "../src/executor.js";
import type { GracefulShutdownCheckpoint } from "../src/harness.js";
import { RunDiskLocks } from "../src/run-disk-locks.js";
import type { SteeringChannel } from "../src/steering.js";
import {
  client, deferred, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith,
} from "./runner-harness.js";
import { codexRig, commitInTree, type CodexRig } from "./runner-codex-sinks-fixture.js";
import { shutdownContentProof } from "./shutdown-3101-content-fixture.js";

installHarness();

for (const refusal of ["timeout", "boundary_blocked"] as const) {
  it(`#3101 shutdown preflight ${refusal} retains unpublished clone bytes`, async t => {
    let now = Date.now();
    t.mock.method(Date, "now", () => now);
    const started = deferred();
    let context: RunContext | undefined;
    const executor: Executor = {
      run: async ctx => {
        context = ctx;
        executor.safety = codexRig({
          lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
        }).safety;
        commitInTree(ctx.worktreePath, "retained.txt", "committed before shutdown\n");
        fs.writeFileSync(path.join(ctx.worktreePath, "retained.txt"), "dirty retained bytes\n");
        fs.writeFileSync(path.join(ctx.worktreePath, "untracked.txt"), "untracked retained bytes\n");
        started.resolve();
        await new Promise<void>(resolve => ctx.signal!.addEventListener("abort", () => resolve(), { once: true }));
        throw new Error("worker shutdown");
      },
    };
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      checkpointTickIntervalMs: 0, codexBoundaryDeadlineMs: 1_000, shutdownPublishTimeoutMs: 1_000,
    });
    const claim = gitlabClaim(refusal === "timeout" ? 310107 : 310108);
    const execution = runner.execute(claim);
    try {
      await Promise.race([started.promise, execution.then(() => { throw new Error("run ended before readiness"); })]);
      const active = (runner as unknown as { activeRuns: Map<string, { steering: SteeringChannel }> })
        .activeRuns.get(claim.run_id)!;
      runner.shutdown();
      if (refusal === "timeout") now += 1_000;
      else active.steering.markCancelled();
      await execution;
      assert.equal(fs.existsSync(context!.worktreePath), true, "preflight refusal must retain the only unpublished source");
      assert.equal(fs.readFileSync(path.join(context!.worktreePath, "retained.txt"), "utf8"), "dirty retained bytes\n");
      assert.equal(fs.readFileSync(path.join(context!.worktreePath, "untracked.txt"), "utf8"), "untracked retained bytes\n");
    } finally {
      runner.shutdown();
      await execution;
    }
  });
}

it("#3101 actual Runner late admission inherits the original deadline across repeated shutdown", async t => {
  const start = Date.now();
  let now = start;
  t.mock.method(Date, "now", () => now);
  const locks = new RunDiskLocks();
  const first = gitlabClaim(310103);
  const second = gitlabClaim(310104);
  const releaseFirst = await locks.acquire(first.run_id);
  const releaseSecond = await locks.acquire(second.run_id);
  const { gitlab } = fakeGitlab();
  let factories = 0;
  let models = 0;
  let clones = 0;
  const runner = runnerWith(() => {
    factories++;
    return { executor: { run: async () => { models++; throw new Error("no late model admission"); } } };
  }, gitlab, undefined, undefined, {
    diskLocks: locks, codexBoundaryDeadlineMs: 1_000, shutdownPublishTimeoutMs: 2_000,
  });
  const deadlines: number[] = [];
  const reportErrors: unknown[] = [];
  const report = client.reportState.bind(client);
  client.reportState = async (runId, body, signal) => {
    if (body.status === "running") {
      try {
        // Observe phaseClone's actual registration at the existing first-running ACK seam.
        const active = (runner as unknown as { activeRuns: Map<string, {
          gracefulShutdownCheckpoint?: GracefulShutdownCheckpoint;
          steering: SteeringChannel;
          cancel: AbortController;
        }> }).activeRuns.get(runId);
        assert.ok(active);
        const cause = active.gracefulShutdownCheckpoint;
        assert.ok(cause, "late registration receives shutdown checkpoint authority");
        deadlines.push(cause.deadlineAt);
        assert.equal(cause.deadlineAt, start + 1_000, "elapsed admission time never renews the original grace");
        assert.equal(active.cancel.signal.aborted, true);
        assert.equal(active.steering.lifecycleSignal().aborted, true);
        assert.equal(active.steering.terminalLifecycleSignal().aborted, true);
        assert.equal(cause.stopSignal.aborted, false, "shutdown retains only bounded checkpoint authority");
        assert.equal(Object.isFrozen(cause), true);
      } catch (error) {
        reportErrors.push(error);
        throw error;
      }
    }
    return report(runId, body, signal);
  };
  git.ensureClone = async () => { clones++; throw new Error("shutdown must stop before clone admission"); };
  const firstExecution = runner.execute(first);
  let secondExecution: Promise<void> | undefined;
  try {
    assert.equal(runner.isExecuting(first.run_id), true);
    assert.equal(factories, 0, "real disk lock holds the claim before attempt registration");
    runner.shutdown();
    now += 100;
    runner.shutdown();
    secondExecution = runner.execute(second);
    assert.equal(factories, 0);
    releaseFirst();
    await firstExecution;
    now += 200;
    runner.shutdown();
    releaseSecond();
    await secondExecution;
    if (reportErrors.length > 0) throw reportErrors[0];
    assert.deepEqual(deadlines, [start + 1_000, start + 1_000]);
    assert.equal(factories, 2);
    assert.equal(models, 0);
    assert.equal(clones, 0);
  } finally {
    releaseFirst();
    releaseSecond();
    runner.shutdown();
    await Promise.all([firstExecution, secondExecution]);
  }
});

for (const finish of ["none", "seeded", "live"] as const) {
  it(finish === "none"
    ? "#3101 shutdown checkpoint preserves newer C and dirty work with a genuinely absent installer promise"
    : `#3101 ${finish} finish before shutdown preserves newer C and dirty work with a genuinely absent installer promise`, async () => {
    client.protocolFeatures = ["run_checkpoint_durability"];
    const iid = finish === "none" ? 310102 : finish === "seeded" ? 310105 : 310106;
    const proof = shutdownContentProof(fx, client, iid);
    const started = deferred();
    let rig: CodexRig | undefined;
    let context: RunContext | undefined;
    const executor: Executor = {
      run: async (ctx) => {
        context = ctx;
        assert.ok(ctx.signal, "Runner supplies its genuine run signal");
        assert.ok(ctx.terminalLifecycleSignal, "Runner supplies its terminal lifecycle signal");
        // The provider alone is fake; registry, lifecycle, safety and installer reconcile
        // are real. No promise/result is populated on this shared production factory.
        rig = codexRig({
          lifecycleSignal: AbortSignal.any([ctx.signal, ctx.terminalLifecycleSignal]),
        });
        executor.safety = rig.safety;
        assert.equal(rig.installer.promise, undefined);
        await proof.prepare(ctx);
        started.resolve();
        try {
          await new Promise<void>((resolve) => {
            if (ctx.signal!.aborted) resolve();
            else ctx.signal!.addEventListener("abort", () => resolve(), { once: true });
          });
          throw new Error("runner shutdown interrupted provider");
        } finally {
          await rig.installer.settle();
          assert.equal(rig.installer.promise, undefined);
        }
      },
    };
    const { gitlab } = fakeGitlab();
    const { github } = fakeGitHub();
    const runner = runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      github,
      checkpointIntervalMs: 1,
      checkpointTickIntervalMs: 0,
      codexBoundaryDeadlineMs: 60_000,
      shutdownPublishTimeoutMs: 60_000,
    });
    const execution = runner.execute(gitlabClaim(iid, {
      stop_pending: finish === "seeded",
      repo: {
        id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath,
        forge_type: "github", default_branch: "main",
      },
    }));
    try {
      await Promise.race([
        started.promise,
        execution.then(() => { throw new Error("run ended before shutdown readiness"); }),
      ]);
      const active = (runner as unknown as { activeRuns: Map<string, {
        steering: SteeringChannel;
        gracefulShutdownCheckpoint?: GracefulShutdownCheckpoint;
      }> }).activeRuns.get(context!.runId);
      assert.ok(active);
      if (finish === "live") {
        // Deliver through SteeringChannel's existing input routing seam. The runner's
        // original poller remains the only poller; no sticky field is manufactured.
        (active.steering as unknown as {
          route(kind: string, body: undefined, id: number): void;
        }).route("stop", undefined, 310106);
      }
      if (finish !== "none") {
        assert.deepEqual(await active.steering.awaitFollowUp(10_000),
          { kind: "ended", reason: "stopped" });
        assert.equal(context!.signal!.aborted, false, "finish leaves the current provider turn alive");
        assert.equal(context!.terminalLifecycleSignal!.aborted, false);
      }
      runner.shutdown();
      await execution;
      assert.equal(context!.signal!.aborted, true);
      assert.equal(context!.terminalLifecycleSignal!.aborted, true);
      assert.ok(rig!.boundaries.includes("shutdown"));
      await proof.verify(git);
      assert.equal(active.gracefulShutdownCheckpoint!.stopSignal.aborted, false);
      assert.deepEqual(rig!.disposeBoundaries, ["terminal"]);
    } finally {
      runner.shutdown();
      await execution;
      proof.cleanup();
    }
  });
}
