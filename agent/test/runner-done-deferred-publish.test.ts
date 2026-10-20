import assert from "node:assert/strict";
import { it } from "node:test";
import { client, fakeGitlab, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, commitInTree, drain, FakeCodexExecutor, installRecoveryRootCleanup, statuses } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

for (const sink of ["done_checkpoint", "milestone_checkpoint"] as const) {
  it(`${sink} scans and publishes deferred work only after its Codex permit ends`, async t => {
    const rig = codexRig();
    let inCheckpoint = false;
    const withBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = async (request, action) => {
      if (request.boundary === "checkpoint") inCheckpoint = true;
      try { return await withBoundary(request, action); }
      finally { if (request.boundary === "checkpoint") inCheckpoint = false; }
    };
    const scan = git.secretScanCheckpointRange.bind(git);
    const scanPermits: boolean[] = [];
    t.mock.method(git, "secretScanCheckpointRange", async (...args: Parameters<typeof scan>) => {
      scanPermits.push(inCheckpoint);
      return scan(...args);
    });
    const publishPermits: boolean[] = [];
    t.mock.method(client, "publishCheckpoint", async (_runId: string, _tip: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
      publishPermits.push(inCheckpoint);
      await drain(pack);
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-fixture" } };
    });
    let observed = false;
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      commitInTree(ctx.worktreePath, "deferred.txt", "scan outside the permit\n");
      await ctx.checkpoint!({ reap: true, sink, progress: { completed: ["m1"], in_progress: [] } });
      observed = true;
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(sink === "done_checkpoint" ? 313201 : 313202);
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
      checkpointIntervalMs: 0,
      checkpointTickIntervalMs: 0,
    }).execute(claim);
    assert.deepEqual(scanPermits, [false], "the deferred secret scan must not run in the checkpoint permit");
    assert.deepEqual(publishPermits, [false], "the owed publish still runs before checkpoint returns with the ticker disabled");
    assert.equal(observed, true, "the checkpoint returned after its out-of-permit publication");
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.ok(!statuses(claim.run_id).includes("failed"));
  });
}
