import { it } from "node:test";
import assert from "node:assert/strict";
import type { Executor, RunContext } from "../src/executor.js";
import type { GracefulShutdownCheckpoint } from "../src/harness.js";
import { RunDiskLocks } from "../src/run-disk-locks.js";
import type { SteeringChannel } from "../src/steering.js";
import {
  client, deferred, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith,
} from "./runner-harness.js";
import { codexRig, type CodexRig } from "./runner-codex-sinks-fixture.js";
import { shutdownContentProof } from "./shutdown-3101-content-fixture.js";

installHarness();

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
  const report = client.reportState.bind(client);
  client.reportState = async (runId, body, signal) => {
    if (body.status === "running") {
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

it("#3101 shutdown checkpoint preserves newer C and dirty work with a genuinely absent installer promise", async () => {
  client.protocolFeatures = ["run_checkpoint_durability"];
  const iid = 310102;
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
    runner.shutdown();
    await execution;
    assert.equal(context!.signal!.aborted, true);
    assert.equal(context!.terminalLifecycleSignal!.aborted, true);
    assert.ok(rig!.boundaries.includes("shutdown"));
    await proof.verify(git);
    assert.deepEqual(rig!.disposeBoundaries, ["terminal"]);
  } finally {
    runner.shutdown();
    await execution;
    proof.cleanup();
  }
});
