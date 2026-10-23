import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { api, client, git, homeDir, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor, codexRig, manualDeadline, statuses } from "./runner-codex-sinks-fixture.js";

installHarness();

it("R2 own deadline after real fetch-back context validation retains source and session", async t => {
  const deadline = manualDeadline("checkpoint");
  let fetching = false, expired = false, clone = "", inode = 0;
  const after: string[] = [];
  const rig = codexRig({ armDeadline: deadline.armDeadline, boundaryProcess: () => {
    if (expired) after.push("spawnBoundaryProcess");
    return undefined;
  } });
  const publish = client.publishCheckpoint.bind(client);
  t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof publish>) => {
    if (expired) after.push("publishCheckpoint");
    return publish(...args);
  });
  const fetch = git.fetchAgentBranch.bind(git);
  t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof fetch>) => {
    if (expired) after.push("fetchAgentBranch");
    fetching = true;
    try { return await fetch(...args); } finally { fetching = false; }
  });
  const internalGit = git as any;
  const validate = internalGit.validateOwedContext.bind(git);
  t.mock.method(internalGit, "validateOwedContext", async (...args: unknown[]) => {
    const result = await validate(...args);
    if (fetching && !expired) { expired = true; deadline.fire(); }
    return result;
  });
  for (const method of ["worktreeStatus", "commitWipMarker", "worktreeHead", "branchTip", "undoWipMarker"] as const) {
    const original = git[method].bind(git);
    t.mock.method(git, method, async (...args: unknown[]) => {
      if (expired) after.push(method);
      return Reflect.apply(original, git, args);
    });
  }
  let flight: any;
  const exec = new FakeCodexExecutor(rig.safety, async ctx => {
    clone = ctx.worktreePath;
    inode = fs.statSync(clone).ino;
    fs.writeFileSync(path.join(clone, "R2-fetch.txt"), "owner source");
    fs.writeFileSync(path.join(homeDir, "r2-fetch-session"), "owner session");
    assert.equal(await ctx.parkForPause!({ completedCount: 1 }), false);
    assert.equal(expired, true);
    assert.equal(flight.sourceJoinFailed, true);
    assert.equal(flight.preserveRecoveryClone, true);
    assert.equal(flight.preserveSession, true);
    assert.deepEqual(after, []);
    return { branch: ctx.branch, pausedAt: { completedCount: 1 } };
  }, rig.settle, rig.capture);
  const runner = runnerWith(() => ({ executor: exec, homeDir }), fakeGitlab().gitlab,
    undefined, undefined, { checkpointIntervalMs: 0 });
  const internal = runner as any;
  const capture = internal.runFreeCapture.bind(runner);
  t.mock.method(internal, "runFreeCapture", (...args: any[]) => { flight = args[1]; return capture(...args); });
  const claim = { ...gitlabClaim(2852), claim_generation: 1, park_run_supported: true };
  await runner.execute(claim);
  assert.equal(statuses(claim.run_id).includes("failed"), false);
  assert.ok(api.states.filter(s => s.body.status === "pause_failed").length <= 1);
  assert.equal(fs.statSync(clone).ino, inode);
  assert.equal(fs.readFileSync(path.join(clone, "R2-fetch.txt"), "utf8"), "owner source");
  assert.equal(fs.readFileSync(path.join(homeDir, "r2-fetch-session"), "utf8"), "owner session");
});
