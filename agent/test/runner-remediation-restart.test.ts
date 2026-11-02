import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { GitCache } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness } from "./runner-harness.js";

installHarness();
function command(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}

async function seedRemediationSource() {
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
  return { claim, branch, bare, base, clone, source, q, x, s, read, persisted, refsBefore, pins };
}

const journalToken = "fixture-restart-journal-mac";
async function joinAndShutdown(runner: RunRunner) {
  runner.shutdown();
  await Promise.allSettled((runner as any).executionTails.values());
  await runner.shutdownRecoveries();
}

for (const cut of ["adopting", "ready-before-progress", "episode-complete"] as const) {
  test(`cold remediation restrictions survive ${cut} and enrolled retirement refusal`, async t => {
    const f = await seedRemediationSource();
    const errors: unknown[] = [];
    const proof = async (action: () => unknown | Promise<unknown>) => {
      try { await action(); } catch (error) { errors.push(error); }
    };
    const makeCache = () => new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ gitleaksBin: (git as any).gitleaksBin }));
    let models = 0, admissions = 0, flight: any;
    const cache = makeCache();
    const runner = new RunRunner(client, cache, runId => ({
      homeDir: path.join(homeDir, runId), executor: { run: async ctx => {
        models++;
        await proof(async () => {
          assert.equal(ctx.sessionId, undefined);
          assert.notEqual(ctx.worktreePath, f.clone.path);
          assert.notEqual(f.read().attemptId, f.source.attemptId);
          assert.equal(command(ctx.worktreePath, "rev-parse", "HEAD"), f.s);
          assert.equal(f.read().recovery.stage, "ready-for-model");
          if (cut === "episode-complete") {
            await ctx.onModelTurnSettled!(1);
            assert.equal(f.read().recovery, undefined);
          }
        });
        throw new Error("fixture stops before another model turn");
      } },
    }), nullLogger(), 20, journalToken, { gitlab: fakeGitlab().gitlab,
      teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 1 });
    assert.ok((runner as any).recovery instanceof RecoveryCoordinator);
    const restore = (runner as any).restoreRemediationRestrictions.bind(runner);
    t.mock.method(runner as any, "restoreRemediationRestrictions", async (value: any) => {
      flight = value;
      // Negative finding knowledge, never authority to adopt or publish.
      if (value.predecessorCapture && !value.remediationRestrictions?.pending)
        (runner as any).latchRemediationFindings(value, f.bare, f.branch, [{ commit: f.q }]);
      return restore(value);
    });
    const capture = cache.recordRecoveryCapture.bind(cache);
    t.mock.method(cache, "recordRecoveryCapture", async (...args: Parameters<typeof capture>) => {
      const coverage = await cache.buildRecoveryCoverage(f.bare, {
        runId: f.claim.run_id, generation: 2, kind: "issue", branch: f.branch, barePath: f.bare,
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: f.base },
      }, [f.q, f.x, f.s], f.s);
      args[5] = coverage.sha; args[6] = f.s;
      return capture(...args);
    });
    t.mock.method(cache, "secretScanCheckpointRange", async () => ({ trusted: true as const, findings: [] }));
    const prepare = cache.prepareRecoverySuccessor.bind(cache);
    t.mock.method(cache, "prepareRecoverySuccessor", async (...args: Parameters<typeof prepare>) => {
      const admit = args[6]!.admitRemediationSource!;
      args[6] = { ...args[6], admitRemediationSource: async input => {
        const accepted = await admit(input);
        await proof(() => {
          assert.equal(accepted, true);
          assert.equal(input.clonePath, f.source.clonePath);
          assert.equal(input.attemptId, f.source.attemptId);
        });
        admissions++;
        return accepted;
      } };
      return prepare(...args);
    });
    if (cut === "adopting") {
      t.mock.method(cache as any, "seedRunnerClone", async () => {
        await proof(() => assert.equal(f.read().recovery.stage, "adopting"));
        // Lifecycle interruption preserves the real adopting write; no journal fabrication.
        runner.shutdown();
        throw new Error("fixture seed interrupted after adopting persisted");
      });
    }
    api.setOwnershipStatus(f.claim.run_id, "running", 2);
    try {
      const execution = runner.execute(f.claim);
      const tail = (runner as any).executionTails.get(f.claim.run_id);
      await execution;
      await tail;
    } finally { await joinAndShutdown(runner); }
    assert.deepEqual(errors, []);
    assert.equal(admissions, 1);
    assert.equal(models, cut === "adopting" ? 0 : 1);
    const saved = f.read();
    assert.equal(saved.recovery?.stage, cut === "episode-complete" ? undefined :
      cut === "adopting" ? "adopting" : "ready-for-model");
    assert.deepEqual(saved.remediationRecovery.edges, f.persisted.edges);
    assert.deepEqual(saved.remediationRecovery.flaggedOIDs, f.persisted.flaggedOIDs);
    assert.equal(saved.remediationRecovery.active, undefined);
    if (cut !== "adopting") assert.ok(saved.remediationRecovery.excludedRoots.includes(f.x),
      "independent excluded predecessor X persisted");
    assert.equal((runner as any).executionTails.size, 0, "every owned execution tail settled");

    // Snapshot ALL persisted config (including journal + attempt ledger), refs and source paths.
    const owed = await cache.enumerateOwedCandidates(f.bare, f.claim.run_id);
    const config = command(f.bare, "config", "--local", "--list");
    const refs = command(f.bare, "for-each-ref", "--format=%(refname) %(objectname)");
    const paths = [f.clone.path, saved.clonePath, saved.recovery?.successor?.clonePath]
      .filter((value): value is string => !!value);
    const pathState = paths.map(value => [value, fs.existsSync(value)]);
    const coldCache = makeCache();
    let coldModels = 0, coldFlight: any, bootstrap = 0;
    const cold = new RunRunner(client, coldCache, runId => ({
      homeDir: path.join(homeDir, runId), executor: { run: async () => {
        coldModels++; throw new Error("uncertain persisted source must not execute");
      } },
    }), nullLogger(), 20, journalToken, { gitlab: fakeGitlab().gitlab,
      teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 1 });
    assert.ok((cold as any).recovery instanceof RecoveryCoordinator);
    assert.notEqual((cold as any).recovery, (runner as any).recovery);
    assert.notEqual(coldCache, cache);
    try {
      let authorityCalls = 0;
      await assert.rejects(coldCache.retireRunnerClone(f.bare, saved.clonePath, f.branch,
        f.claim.run_id, { discard: true, attemptId: saved.attemptId,
          verifiedSuccessor: { key: "issue-3747", generation: 2, expected: saved,
            acknowledged: async () => { authorityCalls++; return false; } } }),
        /remediation recovery retirement requires proof/);
      assert.equal(authorityCalls, 0, "enrollment refuses before any FINAL authority callback");
      assert.equal(command(f.bare, "config", "--local", "--list"), config);
      assert.equal(command(f.bare, "for-each-ref", "--format=%(refname) %(objectname)"), refs);
      assert.deepEqual(paths.map(value => [value, fs.existsSync(value)]), pathState);
      const persistedSource = { runId: saved.runId, clonePath: saved.clonePath, attemptId: saved.attemptId };
      assert.deepEqual(await coldCache.readRuntimeRemediationRecovery(f.bare, f.branch, persistedSource),
        saved.remediationRecovery);

      const coldBootstrap = (cold as any).bootstrapRemediationRestrictions.bind(cold);
      t.mock.method(cold as any, "bootstrapRemediationRestrictions", async (value: any, ...args: any[]) => {
        coldFlight = value;
        const result = await coldBootstrap(value, ...args);
        bootstrap++;
        return result;
      });
      // Even a clean scanner with a floor hiding Q cannot erase persisted negative facts.
      const range = coldCache.resolveCheckpointRange.bind(coldCache);
      let hiddenFloor = f.q;
      t.mock.method(coldCache, "resolveCheckpointRange", async (...args: Parameters<typeof range>) => {
        const value = await range(...args);
        return value ? { ...value, excludeSha: hiddenFloor } : value;
      });
      t.mock.method(coldCache, "secretScanCheckpointRange", async () => ({ trusted: true as const, findings: [] }));
      const ancestry = coldCache.ancestry.bind(coldCache);
      const uncertain = t.mock.method(coldCache, "ancestry", async (...args: Parameters<typeof ancestry>) =>
        args[1] === f.q || args[1] === f.x ? "unknown" : ancestry(...args));
      api.setOwnershipStatus(f.claim.run_id, "running", 2);
      const execution = cold.execute(f.claim);
      const tail = (cold as any).executionTails.get(f.claim.run_id);
      await execution; await tail;
      assert.equal(coldModels, 0);
      assert.ok(bootstrap > 0);
      assert.notEqual(coldFlight, flight);
      assert.deepEqual([...coldFlight.remediationRestrictions.roots].sort(),
        [...new Set([...saved.remediationRecovery.flaggedOIDs,
          ...saved.remediationRecovery.edges.map((edge: { from: string }) => edge.from),
          ...(saved.remediationRecovery.excludedRoots ?? [])])].sort());
      if (cut !== "adopting") assert.ok(coldFlight.remediationRestrictions.roots.includes(f.x),
        "independent excluded predecessor X reconstructed");
      uncertain.mock.restore();
      // Exercise launch independently too: persisted attribution selects the source;
      // no predecessor flight, session, restrictions or current attempt is copied.
      const selected = {
        runId: saved.runId, barePath: f.bare, branch: f.branch, worktreePath: saved.clonePath,
        runnerClone: { path: saved.clonePath, branch: f.branch, attemptId: saved.attemptId },
        cancel: new AbortController(),
        steering: { lifecycleSignal: () => new AbortController().signal },
      };
      await assert.rejects((cold as any).requireLaunchAuthority(selected),
        /remediation continuation published floor conflict/);
      if (cut !== "adopting") {
        hiddenFloor = f.x;
        assert.equal(await coldCache.ancestry(f.bare, f.q, f.x), "divergent",
          "Q alone cannot reject the X-only floor");
        await assert.rejects((cold as any).requireLaunchAuthority(selected),
          /remediation continuation published floor conflict/,
          "persisted X independently rejects an X-only floor");
      }
      assert.ok(coldFlight.remediationRestrictions.roots.includes(f.q));
      for (const oid of saved.remediationRecovery.excludedRoots ?? [])
        assert.ok(coldFlight.remediationRestrictions.roots.includes(oid));
      assert.deepEqual(f.read().remediationRecovery, saved.remediationRecovery);
      const remaining = await coldCache.enumerateOwedCandidates(f.bare, f.claim.run_id);
      for (const root of owed) assert.deepEqual(remaining.find(value => value.sha === root.sha), root,
        "original owed root retains every coverage context");
      const afterPins = command(f.bare, "for-each-ref", "--format=%(refname) %(objectname)").split("\n");
      for (const pin of refs.split("\n")) assert.ok(afterPins.includes(pin), pin);
      for (const [value, exists] of pathState) assert.equal(fs.existsSync(value as string), exists);
    } finally { await joinAndShutdown(cold); }
    assert.deepEqual(errors, []);
  });
}
