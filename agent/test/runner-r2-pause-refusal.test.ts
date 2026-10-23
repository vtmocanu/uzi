import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { api, client, git, homeDir, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor, codexRig, manualDeadline, statuses } from "./runner-codex-sinks-fixture.js";

installHarness();

it("R2 expiry after WIP entry does not read source, recapture, or undo the marker", async t => {
  const deadline = manualDeadline("checkpoint");
  const rig = codexRig({ armDeadline: deadline.armDeadline });
  let expired = false, captures = 0;
  const callsAfterExpiry: string[] = [];
  const marker = git.commitWipMarker.bind(git);
  t.mock.method(git, "commitWipMarker", async (...args: Parameters<typeof marker>) => {
    if (expired) callsAfterExpiry.push("commitWipMarker");
    const created = await marker(...args);
    assert.equal(created, true);
    expired = true;
    deadline.fire();
    return created;
  });
  for (const method of ["worktreeStatus", "worktreeHead", "branchTip", "undoWipMarker"] as const) {
    const original = git[method].bind(git);
    t.mock.method(git, method, async (...args: unknown[]) => {
      if (expired) callsAfterExpiry.push(method);
      return Reflect.apply(original, git, args);
    });
  }
  const exec = new FakeCodexExecutor(rig.safety, async ctx => {
    fs.writeFileSync(path.join(ctx.worktreePath, "R2-WIP.txt"), "exact work before expiry");
    assert.equal(await ctx.parkForPause!({ completedCount: 1 }), false);
    assert.deepEqual(callsAfterExpiry, []);
    assert.equal(captures, 1, "refusal cannot recapture to replace failed completion evidence");
    return { branch: ctx.branch, pausedAt: { completedCount: 1 } };
  }, rig.settle, (...args) => { captures++; return rig.capture(...args); });
  const runner = runnerWith(() => ({ executor: exec, homeDir }), fakeGitlab().gitlab,
    undefined, undefined, { checkpointIntervalMs: 0 });
  const claim = { ...gitlabClaim(2852), claim_generation: 1, park_run_supported: true };
  await runner.execute(claim);
  assert.deepEqual(statuses(claim.run_id).filter(s => s === "failed"), []);
  assert.equal(api.states.filter(s => s.body.status === "pause_failed").length, 1);
});

it("R2 no-scope incomplete pause reports publication refusal and preserves clone/session", async t => {
  const rig = codexRig();
  const claim = { ...gitlabClaim(2852), claim_generation: 1, park_run_supported: true };
  let refusing = false, captures = 0, clone = "", inode = 0;
  const protectedCalls: string[] = [];
  for (const method of ["worktreeStatus", "commitWipMarker", "worktreeHead", "branchTip", "undoWipMarker"] as const) {
    const original = git[method].bind(git);
    t.mock.method(git, method, async (...args: unknown[]) => {
      if (refusing) protectedCalls.push(method);
      return Reflect.apply(original, git, args);
    });
  }
  const publish = client.publishCheckpoint.bind(client);
  t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof publish>) => {
    if (refusing) protectedCalls.push("publishCheckpoint");
    return publish(...args);
  });
  let flight: any;
  const exec = new FakeCodexExecutor(rig.safety, async ctx => {
    clone = ctx.worktreePath;
    inode = fs.statSync(clone).ino;
    fs.writeFileSync(path.join(clone, "R2.txt"), "exact owner work");
    fs.writeFileSync(path.join(homeDir, "r2-session"), "exact session");
    refusing = true;
    try {
      assert.equal(await ctx.parkForPause!({ completedCount: 1, total: 2 }), false);
      assert.equal(flight.sourceJoinFailed, true);
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(flight.preserveSession, true);
      assert.notEqual(flight.parked, true);
      assert.deepEqual(protectedCalls, [], "no protected read, mutation, or publication without a scope");
    } finally { refusing = false; }
    // This fixture stops at the public pause return; executor continuation is covered
    // by the existing Codex lifecycle refusal, cancellation, and vault-lock tests.
    return { branch: ctx.branch, pausedAt: { completedCount: 1, total: 2 } };
  }, rig.settle, async (ms, action, purpose, signal) => {
    if (refusing) {
      captures++;
      return { kind: "incomplete", errors: [{ category: "timeout", message: "capture acquisition expired" }] };
    }
    return rig.capture(ms, action, purpose, signal);
  });
  const runner = runnerWith(() => ({ executor: exec, homeDir }), fakeGitlab().gitlab,
    undefined, undefined, { checkpointIntervalMs: 0 });
  const internal = runner as any;
  const capture = internal.runFreeCapture.bind(runner);
  t.mock.method(internal, "runFreeCapture", (...args: any[]) => {
    flight = args[1];
    return capture(...args);
  });
  await runner.execute(claim);
  assert.equal(captures, 1);
  assert.deepEqual(statuses(claim.run_id).filter(s => ["pause_failed", "paused", "failed"].includes(s)), ["pause_failed"]);
  const messages = api.messages(claim.run_id);
  assert.equal(messages.some(m => m.kind === "paused"), false);
  const failed = messages.filter(m => m.kind === "pause_failed");
  assert.equal(failed.length, 1);
  assert.match(JSON.stringify(failed), /checkpoint could not be published/);
  assert.doesNotMatch(JSON.stringify(failed), /process|residue|worker_residue_blocked/i);
  assert.equal(api.states.some(s => s.runId === claim.run_id && s.body.fail_origin === "worker_residue_blocked"), false);
  assert.equal(fs.statSync(clone).ino, inode);
  assert.equal(fs.readFileSync(path.join(clone, "R2.txt"), "utf8"), "exact owner work");
  assert.equal(fs.readFileSync(path.join(homeDir, "r2-session"), "utf8"), "exact session");
});
