import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { noProofReseed } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();
function command(dir: string, ...args: string[]) {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}
for (const phase of ["before-successor", "partial-clone", "handoff-write-landed", "postreturn", "deadline", "identity-change"] as const) {
  test(`runner real Git adoption ${phase} parks, owner resumes once, then same grant reholds`, async t => {
    const claim = gitlabClaim(3527, { claim_generation: 2, worker_recovery_episode: 7, worker_recovery_owner_episode: 0 });
    const bare = await git.ensureClone(fx.originPath);
    fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
    const clone = await git.createOrAttachRunnerClone(bare, 3527, noProofReseed, claim.run_id);
    await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
    fs.writeFileSync(path.join(clone.path, "retained.txt"), "local retained work\n");
    let models = 0, captures = 0;
    const parks: unknown[] = [];
    client.protocolFeatures = ["recovery_cause_retained_recovery_adoption_failed", "recovery_cause_retained_recovery_exhausted"];
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (runId: Parameters<typeof report>[0], body: Parameters<typeof report>[1], signal?: AbortSignal) => {
      if (body.status === "recovery_wait" && body.recovery_cause?.startsWith("retained_recovery_")) {
        parks.push(body); return { status: body.status, recoveryCause: body.recovery_cause };
      }
      return report(runId, body, signal);
    });
    const capture = git.recordRecoveryCapture.bind(git);
    t.mock.method(git, "recordRecoveryCapture", async (...args: Parameters<typeof capture>) => { captures++; return capture(...args); });
    const seam = git as any;
    if (phase === "before-successor" || phase === "identity-change") t.mock.method(seam, "resolveMarkerWrapperAdoption", async () => { throw new Error("injected adoption"); });
    if (phase === "partial-clone") {
      const seed = seam.seedRunnerClone.bind(git);
      t.mock.method(seam, "seedRunnerClone", async (...args: any[]) => { await seed(...args); throw new Error("injected partial clone"); });
    }
    if (phase === "handoff-write-landed") {
      const run = seam.runGit.bind(git);
      t.mock.method(seam, "runGit", async (cwd: string, args: string[], ...rest: any[]) => {
        const result = await run(cwd, args, ...rest);
        if (args.at(-2) === `uzi-recovery.${clone.branch}.clone` &&
            JSON.parse(args.at(-1)!).recovery?.stage === "ready-for-model") throw new Error("injected landed handoff");
        return result;
      });
    }
    if (phase === "postreturn" || phase === "deadline") {
      const prepare = git.prepareRecoverySuccessor.bind(git);
      t.mock.method(git, "prepareRecoverySuccessor", async (...args: Parameters<typeof prepare>) => {
        await prepare(...args);
        if (phase === "deadline") {
          const j = JSON.parse(command(bare, "config", `uzi-recovery.${clone.branch}.clone`));
          t.mock.method(Date, "now", () => j.recovery.deadline);
        }
        throw new Error("injected after return");
      });
    }
    const read = () => JSON.parse(command(bare, "config", `uzi-recovery.${clone.branch}.clone`));
    const execute = async () => {
      api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation);
      const runner = runnerWith(runId => ({ homeDir: path.join(homeDir, runId),
        executor: { run: async () => { models++; throw new Error("must not model"); } } }),
        fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 });
      await runner.execute(claim);
    };
    await execute();
    assert.equal(models, 0);
    assert.equal(read().recovery.blocker, phase === "deadline" ? "budget_exhausted" : "adoption_failed");
    assert.equal(parks.length, 1, JSON.stringify(api.states));
    assert.ok(fs.existsSync(clone.path));
    if (phase === "deadline") return;
    claim.claim_generation = 3;
    claim.worker_recovery_episode = 8;
    claim.worker_recovery_owner_episode = 1;
    if (phase === "identity-change") {
      const record = command(bare, "config", `uzi-recoveryowner.${claim.run_id}.record`);
      const journal = JSON.stringify(read());
      const identity = JSON.parse(await seam.recoveryOwnerBareIdentity(bare)) as string[];
      identity[1] = String(BigInt(identity[1]!) + 1n);
      t.mock.method(seam, "recoveryOwnerBareIdentity", async () => JSON.stringify(identity));
      const captured = captures;
      await execute();
      assert.equal(api.states.some(s => s.body.status === "failed"), false, "device change leaves adoption owner-held instead of failed");
      assert.equal(parks.length, 2);
      assert.equal(captures, captured, "reholding needs quiescence, not another capture");
      assert.equal(models, 0);
      assert.equal(JSON.stringify(read()), journal);
      assert.equal(command(bare, "config", `uzi-recoveryowner.${claim.run_id}.record`), record);
      assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "local retained work\n");
      return;
    }
    await execute();
    assert.equal(parks.length, 2);
    const count = captures;
    claim.claim_generation = 4;
    claim.worker_recovery_episode = 99;
    await execute();
    assert.equal(parks.length, 3);
    assert.equal(captures, count, "same grant reholds without capture");
    assert.equal(models, 0);
    assert.equal(read().recovery.attempts, 1);
    assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "local retained work\n");
  });
}

for (const legacyBudget of [false, true]) test((legacyBudget ? "legacy budget renewal: " : "") + "owner Resume verifies the successor before model admission and preserves its consumed grant after settlement", async t => {
  const claim = gitlabClaim(3527, { claim_generation: 2, worker_recovery_episode: 7, worker_recovery_owner_episode: 0 });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, 3527, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  if (legacyBudget) {
    const source = { runId: claim.run_id, clonePath: clone.path };
    for (let i = 0; i < 3; i++) await git.reserveRecoveryIteration(bare, clone.branch, "issue-3527", source, 1, 1, 7);
    await assert.rejects(git.reserveRecoveryIteration(bare, clone.branch, "issue-3527", source, 1, 1, 7), /budget/);
    assert.throws(() => command(bare, "config", `uzi-recoveryowner.${claim.run_id}.record`), "old budget journal has no owner record");
    claim.worker_recovery_episode = 8;
  }
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "owner retained work\n");
  client.protocolFeatures = ["recovery_cause_retained_recovery_adoption_failed"];
  let failAdoption = true, parks = 0, models = 0, settlements = 0, verified = false;
  const modelErrors: unknown[] = [];
  const seam = git as any;
  const resolve = seam.resolveMarkerWrapperAdoption.bind(git);
  t.mock.method(seam, "resolveMarkerWrapperAdoption", async (...args: any[]) => {
    if (failAdoption) throw new Error("initial integrity refusal");
    return resolve(...args);
  });
  const prepare = git.prepareRecoverySuccessor.bind(git);
  t.mock.method(git, "prepareRecoverySuccessor", async (...args: Parameters<typeof prepare>) => {
    const successor = await prepare(...args);
    verified = true;
    return successor;
  });
  const report = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (runId: Parameters<typeof report>[0], body: Parameters<typeof report>[1], signal?: AbortSignal) => {
    if (body.status === "recovery_wait" && body.recovery_cause === "retained_recovery_adoption_failed") {
      parks++;
      return { status: body.status, recoveryCause: body.recovery_cause };
    }
    return report(runId, body, signal);
  });
  const read = () => JSON.parse(command(bare, "config", `uzi-recovery.${clone.branch}.clone`));
  const owner = () => JSON.parse(command(bare, "config", `uzi-recoveryowner.${claim.run_id}.record`));
  const execute = async () => {
    api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation);
    const runner = runnerWith(runId => ({ homeDir: path.join(homeDir, runId), executor: {
      run: async (ctx: import("../src/executor.js").RunContext) => {
        models++;
        try {
          assert.equal(verified, true);
          const journal = read();
          assert.equal(journal.recovery.stage, "ready-for-model");
          assert.equal(journal.recovery.claimGeneration, 3);
          assert.equal(journal.recovery.attempts, 1);
          assert.equal(journal.clonePath, ctx.worktreePath);
          assert.notEqual(ctx.worktreePath, clone.path);
          assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "retained.txt"), "utf8"), "owner retained work\n");
          assert.equal(owner().consumed, 1);
          await ctx.onModelTurnSettled!(1);
          settlements++;
          assert.equal(read().recovery, undefined);
          assert.equal(owner().consumed, 1);
        } catch (error) { modelErrors.push(error); throw error; }
        throw new Error("stop after the settled model turn");
      },
    } }), fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 });
    await runner.execute(claim);
  };
  await execute();
  assert.equal(parks, 1); assert.equal(models, 0); assert.equal(owner().baseline, 0);
  failAdoption = false;
  claim.claim_generation = 3;
  claim.worker_recovery_episode = (claim.worker_recovery_episode ?? 0) + 1;
  claim.worker_recovery_owner_episode = 1;
  await execute();
  assert.equal(parks, 1); assert.equal(models, 1); assert.equal(settlements, 1);
  assert.deepEqual(modelErrors, []);
  assert.equal(owner().consumed, 1);
});

for (const kind of ["first", "budget", "adoption"] as const) {
  test(`runner entrypoint pending AFTER ${kind} stops without capture, model or recovery reports`, async t => {
    const claim = gitlabClaim(3527, { claim_generation: 3, worker_recovery_episode: 8, worker_recovery_owner_episode: 1 });
    const bare = await git.ensureClone(fx.originPath);
    fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
    const clone = await git.createOrAttachRunnerClone(bare, 3527, noProofReseed, claim.run_id);
    const source = { runId: claim.run_id, clonePath: clone.path };
    const key = "issue-3527";
    await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
    fs.writeFileSync(path.join(clone.path, "interrupted.txt"), "keep these bytes\n");
    if (kind !== "first") {
      const old = await git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 1, 7, 0);
      if (kind === "budget") {
        await git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 1, 7, 0);
        await git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 1, 7, 0);
        await assert.rejects(git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 1, 7, 0), /budget/);
      } else await git.blockRecoveryEpisode(bare, clone.branch, key, source, "adoption_failed", old);
    }
    const seam = git as any;
    const run = seam.runGit.bind(git);
    const journalKey = `uzi-recovery.${clone.branch}.clone`;
    t.mock.method(seam, "runGit", async (cwd: string, args: string[], ...rest: any[]) => {
      const result = await run(cwd, args, ...rest);
      if (args.at(-2) === journalKey) throw new Error("interrupted AFTER charge");
      return result;
    });
    await assert.rejects(git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 2, 8, 1), /interrupted/);
    t.mock.restoreAll();
    const raw = command(bare, "config", journalKey);
    const ownerKey = `uzi-recoveryowner.${claim.run_id}.record`;
    const before = JSON.parse(command(bare, "config", ownerKey));
    let captures = 0, models = 0;
    const reports: unknown[] = [];
    const capture = git.recordRecoveryCapture.bind(git);
    t.mock.method(git, "recordRecoveryCapture", async (...args: Parameters<typeof capture>) => { captures++; return capture(...args); });
    t.mock.method(client, "reportState", async (_runId: string, body: any) => { reports.push(body); return {}; });
    api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation);
    const runner = runnerWith(runId => ({ homeDir: path.join(homeDir, runId),
      executor: { run: async () => { models++; throw new Error("must not model"); } } }),
      fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 });
    await runner.execute(claim);
    assert.equal(captures, 0); assert.equal(models, 0);
    assert.deepEqual(reports, [{ status: "running", claim_generation: 3 }], "only the entrypoint running acknowledgment precedes recovery");
    assert.equal(command(bare, "config", journalKey), raw);
    assert.deepEqual(JSON.parse(command(bare, "config", ownerKey)), { ...before, state: "committed" });
    assert.equal(fs.readFileSync(path.join(clone.path, "interrupted.txt"), "utf8"), "keep these bytes\n");
  });
}

test("runner entrypoint pending BEFORE adoption reholds consumed grant and admits a newly owned grant", async t => {
  const claim = gitlabClaim(3527, { claim_generation: 3, worker_recovery_episode: 99, worker_recovery_owner_episode: 1 });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, 3527, noProofReseed, claim.run_id);
  const source = { runId: claim.run_id, clonePath: clone.path };
  const key = "issue-3527", journalKey = `uzi-recovery.${clone.branch}.clone`;
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "before-work.txt"), "keep BEFORE bytes\n");
  const old = await git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 1, 7, 0);
  await git.blockRecoveryEpisode(bare, clone.branch, key, source, "adoption_failed", old);
  const seam = git as any;
  const run = seam.runGit.bind(git);
  t.mock.method(seam, "runGit", async (cwd: string, args: string[], ...rest: any[]) => {
    if (args.at(-2) === journalKey) throw new Error("interrupted BEFORE charge");
    return run(cwd, args, ...rest);
  });
  await assert.rejects(git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 2, 8, 1), /interrupted/);
  t.mock.restoreAll();
  const raw = command(bare, "config", journalKey);
  let captures = 0, models = 0, parks = 0;
  const failures: unknown[] = [], modelErrors: unknown[] = [];
  client.protocolFeatures = ["recovery_cause_retained_recovery_adoption_failed"];
  const report = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (runId: Parameters<typeof report>[0], body: Parameters<typeof report>[1], signal?: AbortSignal) => {
    if (body.status === "recovery_wait") {
      parks++; return { status: body.status, recoveryCause: body.recovery_cause };
    }
    if (body.status === "failed") failures.push(body);
    return report(runId, body, signal);
  });
  const capture = git.recordRecoveryCapture.bind(git);
  t.mock.method(git, "recordRecoveryCapture", async (...args: Parameters<typeof capture>) => { captures++; return capture(...args); });
  const execute = async () => {
    api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation);
    const runner = runnerWith(runId => ({ homeDir: path.join(homeDir, runId), executor: {
      run: async (ctx: import("../src/executor.js").RunContext) => {
        models++;
        try {
          const journal = JSON.parse(command(bare, "config", journalKey));
          assert.equal(journal.recovery.claimGeneration, 4);
          assert.equal(journal.recovery.attempts, 1);
          assert.equal(journal.recovery.stage, "ready-for-model");
          const owner = JSON.parse(command(bare, "config", `uzi-recoveryowner.${claim.run_id}.record`));
          assert.equal(owner.state, "committed"); assert.equal(owner.consumed, 2);
          assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "before-work.txt"), "utf8"), "keep BEFORE bytes\n");
        } catch (error) { modelErrors.push(error); }
        throw new Error("stop after model admission");
      },
    } }), fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 });
    await runner.execute(claim);
  };
  await execute();
  assert.equal(parks, 1); assert.equal(captures, 0); assert.equal(models, 0);
  assert.deepEqual(failures, []);
  assert.equal(command(bare, "config", journalKey), raw);
  claim.claim_generation = 4; claim.worker_recovery_owner_episode = 2;
  await execute();
  assert.equal(models, 1); assert.ok(captures > 0);
  assert.deepEqual(modelErrors, []);
  assert.equal(fs.readFileSync(path.join(clone.path, "before-work.txt"), "utf8"), "keep BEFORE bytes\n");
});
