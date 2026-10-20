import assert from "node:assert/strict";
import { it } from "node:test";
import { SinkGate } from "../src/sink-gate.js";
import type { CheckpointStop } from "../src/checkpoint-stop.js";
import { fakeGitlab, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, installRecoveryRootCleanup, virtualBoundaryClock } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

it("a forfeited done gate can only reconcile, even if safety invokes its action", async t => {
  const clock = virtualBoundaryClock();
  const rig = codexRig({ armDeadline: clock.armDeadline, now: clock.now });
  let active = false, reconciled = 0, settled = 0, quiesced = 0, spawned = 0, imported = 0;
  let released = false, observed = false;
  const run = SinkGate.prototype.run;
  t.mock.method(SinkGate.prototype, "run", async function<T>(this: SinkGate, fn: () => Promise<T>, stop?: CheckpointStop): Promise<T> {
    if (!active || !stop) return run.call(this, fn, stop) as Promise<T>;
    const release = this.tryAcquire(() => { clock.advance(60_000); });
    assert.ok(release);
    try { return await run.call(this, fn, stop) as T; }
    finally { release(); released = true; }
  });
  const boundary = rig.safety.withBoundary.bind(rig.safety);
  rig.safety.withBoundary = async (request, action) => {
    if (active && request.checkpointStop?.signal.aborted) {
      reconciled++;
      // Deliberately weaker than production safety: the caller must still not
      // admit checkpoint work after it forfeited the sink gate.
      return action({ signal: new AbortController().signal } as Parameters<typeof action>[0]);
    }
    return boundary(request, action);
  };
  const executor = Object.assign(new FakeCodexExecutor(rig.safety, async ctx => {
    active = true;
    try { await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" }); observed = true; }
    finally { active = false; }
    return { branch: ctx.branch };
  }), { closeAndSettlePlanSummary: async () => { if (active) settled++; } });
  const worker = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
    checkpointIntervalMs: 0, checkpointTickIntervalMs: 0,
    checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
  });
  const internals = worker as unknown as { quiesceOrBlockSink: (...args: unknown[]) => Promise<void> };
  const quiesce = internals.quiesceOrBlockSink.bind(worker);
  t.mock.method(internals, "quiesceOrBlockSink", async (...args: unknown[]) => {
    if (active) quiesced++;
    return quiesce(...args);
  });
  const spawner = git.withBoundaryProcessSpawner.bind(git);
  t.mock.method(git, "withBoundaryProcessSpawner", (...args: Parameters<typeof spawner>) => {
    if (active) spawned++;
    return spawner(...args);
  });
  const fetch = git.fetchAgentBranch.bind(git);
  t.mock.method(git, "fetchAgentBranch", (...args: Parameters<typeof fetch>) => {
    if (active) imported++;
    return fetch(...args);
  });
  await worker.execute(gitlabClaim(313203));
  assert.equal(reconciled, 1);
  assert.equal(released, true, "forfeited waiter does not retain its gate slot");
  assert.equal(settled, 0, "no plan-summary work outside the gate");
  assert.equal(quiesced, 0, "no source quiescence outside the gate");
  assert.equal(spawned, 0, "no Git process context outside the gate");
  assert.equal(imported, 0, "no checkpoint import outside the gate");
  assert.equal(observed, true, "the clean soft stop returns through the ordinary checkpoint handler");
});
