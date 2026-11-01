import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { GitCache, type CheckpointRange } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore } from "../src/completion-authority.js";
import { testLockOptions } from "./authority-test-support.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { noProofReseed, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness } from "./runner-harness.js";

installHarness();
function command(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}

// The scanner reports and default-empty process view are harness seams, not hosted
// quiescence proof. Late-seed separately checks real persisted authority epochs.
for (const mode of ["clean", "clean-no-pending", "untrusted", "unknown", "excluded", "scan-floor", "missing-pending",
  "missing-RR", "historical", "late-scan-pending", "late-scan-root", "late-seed"] as const) {
  test("production remediation adoption: " + mode, async t => {
    const succeeds = mode === "clean" || mode === "clean-no-pending";
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
    const commit = (label: string, parent = base) => {
      const oid = command(clone.path, "-c", "user.name=t", "-c", "user.email=t@example.com",
        "commit-tree", base + "^{tree}", "-p", parent, "-m", label);
      command(clone.path, "reset", "--hard", oid);
      return oid;
    };
    await fetch();
    const x = commit("actual excluded inventory root");
    const q = commit("previously flagged", x); await fetch();
    await git.recordRemediationScan(bare, branch, source, { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
    const historical = command(bare, "-c", "user.name=t", "-c", "user.email=t@example.com",
      "commit-tree", base + "^{tree}", "-p", base, "-m", "historical only finding");
    const s = commit("clean replacement", mode === "historical" ? historical : base);
    await fetch();
    assert.equal(await git.ancestry(bare, q, s), "divergent");
    if (mode === "historical") assert.equal(await git.ancestry(bare, historical, s), "ancestor");
    const key = "uzi-recovery." + branch + ".clone";
    const read = () => JSON.parse(command(bare, "config", key));
    const before = read().remediationRecovery;
    const pins = command(bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-owed/");
    if (mode === "missing-RR") {
      const j = read(); delete j.remediationRecovery; command(bare, "config", key, JSON.stringify(j));
    }
    // Reconstruct cache, runner and its coordinator from persisted predecessor artifacts;
    // the cases below explicitly inject any additional worker-local negative knowledge.
    const authority = mode === "late-seed" ? new AuthorityRuntime({
      lifetime: await acquireWorkerLifetime(fs.realpathSync(fx.dataDir), testLockOptions()),
      workerToken: "fixture-adoption-authority", timeoutMs: 5000,
    }) : undefined;
    const cache = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ gitleaksBin: (git as any).gitleaksBin,
        ...(authority ? { authority: authority.hooks, authorityRequired: true } : {}) }));
    let models = 0, callbacks = 0, scans = 0, writes = 0, underB = false;
    let oldFactsInstalled = false;
    let flight: any, callbackError: unknown;
    const logs = recordingLogger();
    const runner = new RunRunner(client, cache, runId => ({ homeDir: path.join(homeDir, runId), executor: {
      run: async ctx => {
        models++;
        try {
          assert.ok(succeeds);
          assert.equal(ctx.sessionId, undefined, "the predecessor provider session is not reused");
          assert.equal(command(ctx.worktreePath, "rev-parse", "HEAD"), s);
          assert.equal(command(ctx.worktreePath, "write-tree"), command(bare, "rev-parse", s + "^{tree}"));
          assert.equal(read().remediationRecovery.active, undefined);
          assert.deepEqual(read().remediationRecovery.edges, before.edges);
          assert.equal(flight.remediationRestrictions.pending.attemptId, read().attemptId);
          assert.equal(flight.remediationRestrictions.pending.clonePath, ctx.worktreePath);
        } catch (error) { callbackError = error; }
        throw new Error("fixture ends successor");
      },
    } }), logs.logger, 20, undefined, { gitlab: fakeGitlab().gitlab,
      teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 1, authority });
    const endSource = authority ? t.mock.method(authority, "endSourceUse") : undefined;
    const publication = t.mock.method(runner as any, "publishFlightSource");
    api.setOwnershipStatus(claim.run_id, "running", 2);
    const latch = () => (runner as any).latchRemediationFindings(flight, bare, branch, [{ commit: q }]);
    // Supply a genuine C[D,S] coverage artifact at the existing capture seam. X is an
    // eligible ancestor of D but divergent from S, so its proposed exclusion is new.
    const capture = cache.recordRecoveryCapture.bind(cache);
    t.mock.method(cache, "recordRecoveryCapture", async (...args: Parameters<typeof capture>) => {
      const coverage = await cache.buildRecoveryCoverage(bare, {
        runId: claim.run_id, generation: 2, kind: "issue", branch, barePath: bare,
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
      }, [q, x, s], s);
      args[5] = coverage.sha; args[6] = s;
      return capture(...args);
    });
    const prepare = cache.prepareRecoverySuccessor.bind(cache);
    t.mock.method(cache, "prepareRecoverySuccessor", async (...args: Parameters<typeof prepare>) => {
      const original = args[6]!.admitRemediationSource!;
      args[6] = { ...args[6], admitRemediationSource: async input => {
        callbacks++; underB = true;
        assert.ok(input.restrictedRoots.includes(x));
        assert.ok(!input.flaggedOIDs.includes(x), "a proposed excluded root is not global finding evidence");
        assert.ok(!flight.remediationRestrictions.roots.includes(x));
        try { return await original(input); } finally { underB = false; }
      } };
      return prepare(...args);
    });
    // Install independently selected old facts immediately before the production restore.
    const restore = (runner as any).restoreRemediationRestrictions.bind(runner);
    t.mock.method(runner as any, "restoreRemediationRestrictions", async (f: any) => {
      flight = f;
      if (!oldFactsInstalled && f.predecessorCapture) {
        oldFactsInstalled = true;
        if (mode !== "clean-no-pending")
          (runner as any).latchRemediationFindings(f, bare, branch,
            mode === "late-scan-pending" || mode === "late-seed" ? [] :
              [{ commit: mode === "missing-pending" ? x : q }]);
        else assert.equal(f.remediationRestrictions.pending, undefined);
      }
      return restore(f);
    });
    // Trusted scanner stub: Git range resolution, history and adoption remain real.
    t.mock.method(cache, "secretScanCheckpointRange", async (_bare: string, range: CheckpointRange) => {
      if (!underB) return { trusted: true as const, findings: [] };
      scans++; assert.equal(range.tipSha, s);
      await Promise.resolve();
      if (mode === "late-scan-pending") {
        assert.deepEqual(flight.remediationRestrictions.pending.requiredOIDs, []);
        latch();
      }
      if (mode === "late-scan-root") flight.remediationRestrictions.roots.push(base);
      if (mode === "untrusted") return { trusted: false as const, reason: "fixture untrusted", findings: [] };
      return { trusted: true as const, findings: mode === "historical" ? [{ commit: historical } as any] : [] };
    });
    const range = cache.resolveCheckpointRange.bind(cache);
    t.mock.method(cache, "resolveCheckpointRange", async (...args: Parameters<typeof range>) => {
      const value = await range(...args);
      if (underB && value && mode === "excluded") return { ...value, excludeSha: q };
      if (underB && value && mode === "scan-floor") return { ...value, scanFloorShas: [q] };
      return value;
    });
    const ancestry = cache.ancestry.bind(cache);
    t.mock.method(cache, "ancestry", async (...args: Parameters<typeof ancestry>) =>
      underB && mode === "unknown" ? "unknown" : ancestry(...args));
    const seed = (cache as any).seedRunnerClone.bind(cache);
    t.mock.method(cache as any, "seedRunnerClone", async (...args: any[]) => {
      const result = await seed(...args);
      if (mode === "late-seed") latch();
      return result;
    });
    const persist = cache.recordRemediationRestrictions.bind(cache);
    t.mock.method(cache, "recordRemediationRestrictions", async (...args: Parameters<typeof persist>) => {
      assert.equal(underB, false); writes++;
      assert.ok(flight.remediationRestrictions.pending.requiredOIDs.includes(historical));
      return persist(...args);
    });
    try {
      await runner.execute(claim);
      if (callbackError) throw callbackError;
      assert.equal(models, succeeds ? 1 : 0, JSON.stringify({ states: api.states, logs }));
      assert.equal(callbacks, mode === "missing-pending" || mode === "missing-RR" ? 0 : 1, JSON.stringify({ states: api.states, logs }));
      if (callbacks) assert.equal(scans, 1);
      assert.equal(command(bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-owed/"), pins);
      assert.ok(fs.existsSync(clone.path));
      if (!succeeds) {
        assert.equal(flight.remediationRestrictions.pending.clonePath, source.clonePath);
        assert.equal(flight.remediationRestrictions.pending.attemptId, source.attemptId);
        if (mode === "missing-pending") {
          assert.ok(flight.remediationRestrictions.roots.includes(x), "unconfirmed finding knowledge remains latched");
          assert.ok(!read().remediationRecovery.flaggedOIDs.includes(x));
        } else {
          assert.ok(!flight.remediationRestrictions.roots.includes(x), "proposal never installs exclusions on refusal");
        }
      }
      if (mode === "historical") {
        assert.equal(writes, 1);
        assert.ok(read().remediationRecovery.flaggedOIDs.includes(historical));
        assert.deepEqual(read().remediationRecovery.active, before.active, "negative finding grants no origin");
        const restarted = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
        assert.ok((await restarted.readRuntimeRemediationRecovery(bare, branch, source))!.flaggedOIDs.includes(historical));
        // A fresh consumer must honor the persisted fact even if the scanner now
        // excludes that history and reports clean. This is negative admission proof,
        // not permission to execute in the predecessor or a process-quiescence test.
        const resolve = restarted.resolveCheckpointRange.bind(restarted);
        t.mock.method(restarted, "resolveCheckpointRange", async (...args: Parameters<typeof resolve>) => {
          const actual = await resolve(...args);
          return actual ? { ...actual, excludeSha: historical } : actual;
        });
        t.mock.method(restarted, "secretScanCheckpointRange", async () => ({ trusted: true as const, findings: [] }));
        const fresh = new RunRunner(client, restarted, runId => ({ homeDir: path.join(homeDir, runId),
          executor: { run: async () => { throw new Error("unused executor"); } } }), nullLogger(), 20,
          undefined, { gitlab: fakeGitlab().gitlab, teardownTestDeps: portableTeardownTestDeps });
        try {
          await assert.rejects((fresh as any).requireLaunchAuthority({
            runId: claim.run_id, barePath: bare, branch, worktreePath: clone.path,
            runnerClone: clone, cancel: new AbortController(),
            steering: { lifecycleSignal: () => new AbortController().signal },
          }), /history restricted/);
        } finally { fresh.shutdown(); }
      }
      if (mode === "late-seed") {
        assert.equal(read().recovery.stage, "ready-for-model");
        assert.equal(flight.sourceTailsJoined, true, "production teardown positively joined its tails");
        assert.equal(flight.evidenceProcessJoined, flight.executor);
        assert.equal(flight.sourceUsePaths.size, 2, "A recorded old and successor admissions");
        assert.equal(endSource!.mock.callCount(), 0, "no end can be queued");
        assert.equal(publication.mock.callCount(), 0);
        await authority!.withTransaction(async () => undefined);
        for (const admitted of flight.sourceUsePaths) {
          const slot = await authority!.withTransaction(async tx => {
            const store = await openAuthorityStore(tx, { workerToken: "fixture-adoption-authority" });
            return store.readSlot(tx, await store.slotKey(tx, admitted));
          });
          assert.equal(slot?.state, "active");
        }
      }
    } finally { runner.shutdown(); }
  });
}
