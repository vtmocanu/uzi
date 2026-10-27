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
for (const phase of ["before-successor", "partial-clone", "handoff-write-landed", "postreturn", "deadline"] as const) {
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
    if (phase === "before-successor") t.mock.method(seam, "resolveMarkerWrapperAdoption", async () => { throw new Error("injected adoption"); });
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
