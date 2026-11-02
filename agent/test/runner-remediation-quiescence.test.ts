import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { GitCache, type CheckpointRange } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { RecoveryCoordinator, type InventorySourceDiagnostic } from "../src/recovery.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { makeFakeProcRoot, plantInScope, withQuiescenceView } from "./fake-proc.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness } from "./runner-harness.js";

installHarness();
function command(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}

test("persisted remediation source requires scoped process quiescence and the exact owned tail before fresh adoption",
  { skip: process.platform !== "linux" }, async t => {
  const claim = gitlabClaim(3747, { claim_generation: 2, session_id: "predecessor-session" });
  const branch = "agent/issue-3747";
  const attemptId = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  const base = command(bare, "rev-parse", "refs/remotes/origin/main");
  const clone = await git.createOrAttachRunnerClone(bare, 3747, noProofReseed, claim.run_id, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  const source = { runId: claim.run_id, clonePath: clone.path, attemptId };
  await git.markRecoveryCapture(bare, clone.path, branch, claim.run_id, attemptId);
  const fetch = () => git.fetchAgentBranch(bare, clone.path, branch, claim.run_id, {
    context: { runId: claim.run_id, generation: 1, kind: "issue", branch, barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } }, producingSource: source,
  });
  const commit = (label: string, parent: string) => {
    const oid = command(clone.path, "-c", "user.name=t", "-c", "user.email=t@example.com",
      "commit-tree", base + "^{tree}", "-p", parent, "-m", label);
    command(clone.path, "reset", "--hard", oid);
    return oid;
  };
  await fetch();
  const x = commit("excluded predecessor root", base);
  const q = commit("flagged predecessor", x); await fetch();
  await git.recordRemediationScan(bare, branch, source,
    { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
  const s = commit("clean rewritten source", base); await fetch();
  await git.recordRemediationScan(bare, branch, source, { trusted: true, tip: s, flaggedOIDs: [] });
  assert.equal(await git.ancestry(bare, q, s), "divergent");
  const read = () => JSON.parse(command(bare, "config", "uzi-recovery." + branch + ".clone"));
  const persisted = read().remediationRecovery;
  assert.ok(persisted.edges.length > 0, "worker Git producer persisted a verified rewrite");
  assert.equal(persisted.active, undefined, "the producer closed its rewrite grant");
  assert.equal(persisted.edges[0].grant.origin, "pre-exit");
  assert.deepEqual(persisted.edges[0].grant.originSource, source);
  const refsBefore = command(bare, "for-each-ref", "--format=%(refname) %(objectname)");
  const pins = command(bare, "for-each-ref", "--format=%(refname) %(objectname)",
    "refs/uzi-owed/", "refs/uzi-coverage/", "refs/uzi-recovery-pin/", "refs/uzi-recovery-episode/").split("\n").filter(Boolean);
  assert.ok(pins.length > 0);
  let models = 0, adoptions = 0;
  // Independent consumer: no predecessor runner state, registry, cache or coordinator is copied.
  const authority = new AuthorityRuntime({
    lifetime: await acquireWorkerLifetime(fs.realpathSync(fx.dataDir), testLockOptions()),
    workerToken: "fixture-quiescence-authority", timeoutMs: 5000,
  });
  const cache = new GitCache(fx.dataDir, nullLogger(), undefined,
    testGitCacheOptions({ gitleaksBin: (git as any).gitleaksBin,
      authority: authority.hooks, authorityRequired: true }));
  let flight: any, modelError: unknown, admissions = 0;
  const fresh = new RunRunner(client, cache, runId => ({ homeDir: path.join(homeDir, runId), executor: {
    run: async ctx => {
      models++;
      try {
        assert.equal(adoptions, 1);
        assert.equal(admissions, 1);
        assert.equal(ctx.sessionId, undefined);
        assert.notEqual(ctx.worktreePath, clone.path);
        assert.notEqual(read().attemptId, source.attemptId);
        assert.equal(command(ctx.worktreePath, "rev-parse", "HEAD"), s);
        assert.equal(command(ctx.worktreePath, "write-tree"), command(bare, "rev-parse", s + "^{tree}"));
        for (const excluded of [q, x]) assert.equal(await cache.ancestry(bare, excluded, s), "divergent");
        assert.equal(read().remediationRecovery.active, undefined);
        assert.deepEqual(read().remediationRecovery.edges, persisted.edges);
      } catch (error) { modelError = error; }
      throw new Error("fixture ends successor");
    },
  } }), nullLogger(), 20, undefined, { gitlab: fakeGitlab().gitlab,
    teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 1, authority });
  assert.ok((fresh as any).recovery instanceof RecoveryCoordinator);
  const restore = (fresh as any).restoreRemediationRestrictions.bind(fresh);
  t.mock.method(fresh as any, "restoreRemediationRestrictions", async (f: any) => {
    flight = f;
    if (f.predecessorCapture && !f.remediationRestrictions?.pending)
      (fresh as any).latchRemediationFindings(f, bare, branch, [{ commit: q }]);
    return restore(f);
  });
  const capture = cache.recordRecoveryCapture.bind(cache);
  t.mock.method(cache, "recordRecoveryCapture", async (...args: Parameters<typeof capture>) => {
    const coverage = await cache.buildRecoveryCoverage(bare, {
      runId: claim.run_id, generation: 2, kind: "issue", branch, barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
    }, [q, x, s], s);
    args[5] = coverage.sha; args[6] = s;
    return capture(...args);
  });
  // The capture seam supplies coverage and the scanner supplies a trusted clean report.
  // Range resolution, admission, quiescence and authority transactions run production code.
  t.mock.method(cache, "secretScanCheckpointRange", async (_bare: string, range: CheckpointRange) => {
    assert.equal(range.tipSha, s);
    return { trusted: true as const, findings: [] };
  });
  const prepare = cache.prepareRecoverySuccessor.bind(cache);
  const prepareSpy = t.mock.method(cache, "prepareRecoverySuccessor", async (...args: Parameters<typeof prepare>) => {
    const admit = args[6]!.admitRemediationSource!;
    args[6] = { ...args[6], admitRemediationSource: async input => {
      assert.equal(input.clonePath, source.clonePath);
      assert.equal(input.attemptId, source.attemptId);
      assert.equal(input.runId, claim.run_id);
      const accepted = await admit(input);
      assert.equal(accepted, true, "production continuation-history admission positively accepted the source");
      admissions++;
      return accepted;
    } };
    const result = await prepare(...args);
    adoptions++;
    return result;
  });
  const owner = fresh as any;
  const processProof = t.mock.method(owner, "quiesceImpl");
  const root = makeFakeProcRoot("uzi-remediation-scoped-");
  let finish!: () => void;
  const cleanup = new Promise<void>(resolve => { finish = resolve; });
  let exactTail: Promise<unknown> | undefined, queuedTail: Promise<unknown> | undefined;
  let queuedExecution: Promise<void> | undefined, joined = false;
  try {
    await withQuiescenceView({ procRoot: root }, async () => {
      plantInScope(root, 61, clone.path);
      const diagnostics: InventorySourceDiagnostic[] = [];
      const context = { runId: claim.run_id, generation: 2, barePath: bare,
        reportDiagnostic: (d: InventorySourceDiagnostic) => diagnostics.push(d) };
      let actions = 0;
      assert.equal(await owner.withInventorySourceBoundary(context, async () => { actions++; }), "retained");
      assert.equal(diagnostics[0]?.check, "process_not_quiescent_before_git");
      const first = await processProof.mock.calls[0].result;
      assert.equal(first.process.state, "survivors");
      assert.deepEqual(first.process.processes.map((p: any) => [p.pid, p.reason]), [[61, "unattributed_in_scope"]]);
      assert.equal(fs.existsSync(path.join(root, "61")), true, "capture did not finish the fake lifetime");

      // Public execute observes completion while its exact registered tail still owns cleanup.
      // No terminal family is returned: this fixture owns no asynchronous recovery replay.
      const firstClaim = t.mock.method(owner, "executeClaim", async (_claim: unknown, _previous: unknown,
        own: (operation: Promise<void>) => void) => { own(cleanup); });
      const publicExecution = fresh.execute(claim);
      exactTail = owner.executionTails.get(claim.run_id);
      assert.ok(exactTail);
      void exactTail!.then(() => { joined = true; });
      await publicExecution;
      assert.equal(joined, false);
      assert.equal(owner.executionTails.get(claim.run_id), exactTail);
      assert.equal(firstClaim.mock.callCount(), 1);
      firstClaim.mock.restore();
      const actualClaim = t.mock.method(owner, "executeClaim");
      api.setOwnershipStatus(claim.run_id, "running", 2);
      queuedExecution = fresh.execute(claim);
      queuedTail = owner.executionTails.get(claim.run_id);
      assert.ok(queuedTail);
      assert.notEqual(queuedTail, exactTail);
      assert.equal(await Promise.race([queuedExecution.then(() => "settled"), Promise.resolve("pending")]), "pending");
      assert.equal(actualClaim.mock.callCount(), 0, "actual recovery waits behind the exact owned tail");
      assert.equal(fs.existsSync(path.join(root, "61")), true, "actual recovery was requested with a surviving scoped writer");
      diagnostics.length = 0;
      const processCalls = processProof.mock.callCount();
      assert.equal(await owner.withInventorySourceBoundary(context, async () => { actions++; }), "retained");
      assert.deepEqual(diagnostics, [{ check: "execution_tail_present" }]);
      assert.equal(processProof.mock.callCount(), processCalls, "tail refusal precedes process inspection");
      assert.equal(actions, 0);
      assert.equal(prepareSpy.mock.callCount(), 0);
      assert.equal(models, 0);
      assert.equal(read().clonePath, clone.path);
      assert.deepEqual(read().remediationRecovery, persisted);
      assert.equal(command(bare, "for-each-ref", "--format=%(refname) %(objectname)"), refsBefore);
      assert.equal(admissions, 0);

      fs.rmSync(path.join(root, "61"), { recursive: true });
      assert.deepEqual(fs.readdirSync(root), [], "the explicitly installed scoped process table is now empty");
      assert.equal(joined, false, "ending the process lifetime alone does not join owned cleanup");
      assert.equal(actualClaim.mock.callCount(), 0);
      assert.equal(owner.executionTails.get(claim.run_id), queuedTail);
      finish(); await exactTail;
      assert.equal(joined, true);
      await queuedExecution;
      await queuedTail;
      assert.equal(actualClaim.mock.callCount(), 1, "the queued execution invokes the real claim handler");
      if (modelError) throw modelError;
      assert.equal(models, 1);
      assert.equal(adoptions, 1);
      assert.equal(admissions, 1);
      assert.equal((fresh as any).executionTails.has(claim.run_id), false);
      assert.equal(flight.remediationRestrictions.pending.clonePath, read().clonePath);
      assert.equal(flight.sourceUsePaths.size, 2, "authority admitted the predecessor and successor sources");
      const actual = command(bare, "for-each-ref", "--format=%(refname) %(objectname)",
        "refs/uzi-owed/", "refs/uzi-coverage/", "refs/uzi-recovery-pin/", "refs/uzi-recovery-episode/").split("\n");
      for (const pin of pins) assert.ok(actual.includes(pin), pin);
    });
  } finally {
    finish();
    await Promise.allSettled([exactTail, queuedExecution, queuedTail]);
    fresh.shutdown();
    fs.rmSync(root, { recursive: true, force: true });
  }
});
