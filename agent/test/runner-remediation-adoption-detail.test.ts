import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { test, type TestContext } from "node:test";
import { GitCache } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { noProofReseed, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness } from "./runner-harness.js";

// issue #4177 m2: a retained adoption refusal on an active (or unprovable) secret-remediation record parks with a
// closed-set recovery_detail, only to an api that advertised it. Real Git and the real retained runner; the
// scanner and the ancestry probe are the only seams.

installHarness();
const ADOPTION = "recovery_cause_retained_recovery_adoption_failed";
const DETAIL = "recovery_detail_adoption_refusal_v1";
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
function command(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();
}

type Mode = "active" | "git-unknown" | "callback-finding" | "callback-untrusted" | "callback-unknown" |
  "no-token" | "deadline-rewrite" | "cold" | "cold-mismatch" | "cold-no-record" | "cold-captured";
const COLD = new Set<Mode>(["cold", "cold-mismatch", "cold-no-record", "cold-captured"]);

async function drive(t: TestContext, mode: Mode, iid: number) {
  const claim = gitlabClaim(iid, { claim_generation: 2, session_id: "predecessor-session" });
  const branch = `agent/issue-${iid}`;
  const attemptId = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  const base = command(bare, "rev-parse", "refs/remotes/origin/main");
  const originBefore = command(fx.originPath, "rev-parse", "refs/heads/main");
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id, false, undefined,
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
  const q = commit("previously flagged"); await fetch();
  await git.recordRemediationScan(bare, branch, source, { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
  // Active: the owned source still contains the flagged commit. Otherwise a clean replacement divergent from it.
  const historical = command(bare, "-c", "user.name=t", "-c", "user.email=t@example.com",
    "commit-tree", base + "^{tree}", "-p", base, "-m", "historical only finding");
  const containsFlag = mode === "active" || mode === "git-unknown" || mode === "no-token" || COLD.has(mode);
  const s = commit(containsFlag ? "progress on the flagged tip" : "clean replacement",
    containsFlag ? q : mode === "callback-finding" ? historical : base);
  await fetch();
  assert.equal(await git.ancestry(bare, q, s), containsFlag ? "ancestor" : "divergent");
  client.protocolFeatures = mode === "no-token" ? ["claim_generation_fence", ADOPTION]
    : mode === "deadline-rewrite" ? ["claim_generation_fence", ADOPTION, DETAIL, "recovery_cause_retained_recovery_exhausted"]
    : ["claim_generation_fence", ADOPTION, DETAIL];
  const key = "uzi-recovery." + branch + ".clone";
  const journalRaw = () => command(bare, "config", key);

  const cache = new GitCache(fx.dataDir, nullLogger(), undefined,
    testGitCacheOptions({ gitleaksBin: (git as any).gitleaksBin }));
  let models = 0, underB = false;
  const logs = recordingLogger();
  const runner = new RunRunner(client, cache, runId => ({ homeDir: path.join(homeDir, runId), executor: {
    run: async () => { models++; throw new Error("fixture ends successor"); },
  } }), logs.logger, 20, undefined, { gitlab: fakeGitlab().gitlab,
    teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 1 });
  api.setOwnershipStatus(claim.run_id, "running", 2);
  const coverageFor = () => cache.buildRecoveryCoverage(bare, {
    runId: claim.run_id, generation: 2, kind: "issue", branch, barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
  }, [q, s], s);
  if (COLD.has(mode)) {
    // A previous process charged an iteration, recorded its capture and (mostly) blocked the episode; its in-memory
    // refusal class died with it. This claim holds no owner grant and a brand new runner reads the journal cold.
    const key4 = `issue-${iid}`;
    const iteration = await cache.reserveRecoveryIteration(bare, branch, key4, source);
    const coverage = await coverageFor();
    await cache.recordRecoveryCapture(bare, branch, key4, source, iteration.attempts, coverage.sha,
      mode === "cold-mismatch" ? base : s);
    if (mode !== "cold-captured") await cache.blockRecoveryEpisode(bare, branch, key4, source, "adoption_failed", iteration);
    if (mode === "cold-no-record") {
      const j = JSON.parse(journalRaw()); delete j.remediationRecovery;
      command(bare, "config", key, JSON.stringify(j));
    }
  }
  const coldBefore = COLD.has(mode) ? journalRaw() : undefined;
  const capture = cache.recordRecoveryCapture.bind(cache);
  t.mock.method(cache, "recordRecoveryCapture", async (...args: Parameters<typeof capture>) => {
    const coverage = await coverageFor();
    args[5] = coverage.sha; args[6] = s;
    return capture(...args);
  });
  let flight: any;
  const restore = (runner as any).restoreRemediationRestrictions.bind(runner);
  let installed = false;
  t.mock.method(runner as any, "restoreRemediationRestrictions", async (f: any) => {
    flight = f;
    if (!installed && f.predecessorCapture) {
      installed = true;
      (runner as any).latchRemediationFindings(f, bare, branch, [{ commit: q }]);
    }
    return restore(f);
  });
  t.mock.method(cache, "secretScanCheckpointRange", async () => {
    if (!underB) return { trusted: true as const, findings: [] };
    if (mode === "callback-untrusted") return { trusted: false as const, reason: "fixture untrusted", findings: [] };
    return { trusted: true as const, findings: mode === "callback-finding" ? [{ commit: historical } as any] : [] };
  });
  const prepare = cache.prepareRecoverySuccessor.bind(cache);
  let realNow: (() => number) | undefined;
  t.mock.method(cache, "prepareRecoverySuccessor", async (...args: Parameters<typeof prepare>) => {
    const original = args[6]!.admitRemediationSource!;
    args[6] = { ...args[6], admitRemediationSource: async input => {
      underB = true;
      try {
        const admitted = await original(input);
        if (mode === "deadline-rewrite") {
          // The refusal is classified, then the iteration deadline passes before the park decision.
          realNow = Date.now.bind(Date);
          t.mock.method(Date, "now", () => realNow!() + 600_000);
        }
        return admitted;
      } finally { underB = false; }
    } };
    return prepare(...args);
  });
  const ancestry = cache.ancestry.bind(cache);
  t.mock.method(cache, "ancestry", async (...args: Parameters<typeof ancestry>) => {
    if (mode === "git-unknown" && args[1] === q && args[2] === s) return "unknown";
    if (mode === "callback-unknown" && underB) return "unknown";
    return ancestry(...args);
  });
  try {
    await runner.execute(claim);
    if (mode === "deadline-rewrite") t.mock.restoreAll();
    return {
      parks: api.states.filter(x => x.runId === claim.run_id && x.body.status === "recovery_wait").map(x => x.body as unknown as Record<string, unknown>),
      reported: api.states.filter(x => x.runId === claim.run_id).map(x => String(x.body.status)),
      models, logs, bare, branch, q, s, base, originBefore, journal: () => JSON.parse(journalRaw()),
      raw: journalRaw, coldBefore, flight,
      origin: () => ({ all: command(fx.originPath, "rev-list", "--all"), main: command(fx.originPath, "rev-parse", "refs/heads/main") }),
      clonePath: clone.path,
    };
  } finally { runner.shutdown(); }
}

let counter = 4300;

for (const [mode, expected] of [
  ["active", "secret_remediation_active"],
  ["git-unknown", "remediation_proof_unknown"],
  ["callback-finding", "secret_remediation_active"],
  ["callback-untrusted", "remediation_proof_unknown"],
  ["callback-unknown", "remediation_proof_unknown"],
] as const) {
  test("adoption refusal parks with the closed-set detail: " + mode, async t => {
    const r = await drive(t, mode, counter++);
    assert.equal(r.models, 0, "no model turn, no adoption");
    // The park is reported twice under one deadline (a replay), byte-identical.
    assert.ok(r.parks.length >= 1, JSON.stringify({ reported: r.reported, logs: r.logs }));
    for (const park of r.parks) {
      assert.equal(park.recovery_cause, "retained_recovery_adoption_failed");
      assert.equal(park.recovery_detail, expected);
      assert.equal(park.claim_generation, 2);
      assert.equal(JSON.stringify(park), JSON.stringify(r.parks[0]));
    }
    // Custody retained: the blocked journal, the capture pin and the predecessor clone survive the park.
    const journal = r.journal();
    assert.equal(journal.recovery.stage, "blocked");
    assert.equal(journal.recovery.blocker, "adoption_failed");
    assert.ok(journal.recovery.restoreTip, "a verified local restore point was recorded before the park");
    assert.equal(command(r.bare, "rev-parse", `refs/uzi-recovery-episode/${journal.runId}/${journal.recovery.restoreTip}`), journal.recovery.restoreTip);
    assert.ok(fs.existsSync(r.clonePath));
    // Nothing reached the origin: the flagged commit is not published and its branch is unchanged.
    const origin = r.origin();
    assert.equal(origin.main, r.originBefore);
    assert.ok(!origin.all.split("\n").includes(r.q), "the flagged commit never reached the origin");
    assert.ok(!origin.all.split("\n").includes(r.s), "the refused source never reached the origin");
    assert.equal(command(fx.originPath, "for-each-ref", "refs/heads/" + r.branch), "", "no agent branch was published");
  });
}

for (const [mode, expected] of [
  ["cold", "secret_remediation_active"],
  ["cold-captured", "secret_remediation_active"],
  ["cold-mismatch", undefined],
  ["cold-no-record", undefined],
] as const) {
  test("cold worker rediscovers the refusal class from the journal: " + mode, async t => {
    const r = await drive(t, mode, counter++);
    assert.equal(r.models, 0);
    assert.ok(r.parks.length >= 1, JSON.stringify({ reported: r.reported, logs: r.logs }));
    for (const park of r.parks) {
      assert.equal(park.recovery_cause, "retained_recovery_adoption_failed");
      if (expected) assert.equal(park.recovery_detail, expected);
      else assert.ok(!("recovery_detail" in park), "an unverifiable record carries no class");
    }
    if (mode === "cold" || mode === "cold-mismatch") assert.equal(r.raw(), r.coldBefore, "rediscovery wrote no journal content");
  });
}

test("the detail is omitted without the api token; the park itself is unchanged", async t => {
  const plain = await drive(t, "no-token", counter++);
  assert.ok(plain.parks.length >= 1, JSON.stringify({ reported: plain.reported, logs: plain.logs }));
  for (const park of plain.parks) {
    assert.equal(park.recovery_cause, "retained_recovery_adoption_failed");
    assert.equal(park.claim_generation, 2);
    assert.ok(!("recovery_detail" in park), "an older api would 400 the unknown field");
  }
});

test("no detail is sent when the final blocker was rewritten to budget exhaustion", async t => {
  const r = await drive(t, "deadline-rewrite", counter++);
  assert.ok(r.parks.length >= 1, JSON.stringify({ reported: r.reported, logs: r.logs }));
  assert.ok(r.parks.every(p => !("recovery_detail" in p)), JSON.stringify(r.parks));
  assert.ok(r.parks.every(p => p.recovery_cause === "retained_recovery_exhausted"), JSON.stringify(r.parks));
});
