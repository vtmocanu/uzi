import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { test, type TestContext } from "node:test";
import { GitCache } from "../src/git.js";
import { RequestError } from "../src/client.js";
import { RecoveryCoordinator, type RecoveryArchiveClient } from "../src/recovery.js";
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
  "callback-interrupted" | "callback-guard" | "detail-400" |
  "no-token" | "deadline-rewrite" | "cold" | "cold-mismatch" | "cold-ownedtip" | "cold-no-record" | "cold-slow" |
  "cold-throws" | "cold-no-token" | "captured-journal-live" | "upload-failed";
// Modes whose journal is already blocked (or captured) before this claim runs, so its in-memory refusal class is absent.
const COLD = new Set<Mode>(["cold", "cold-mismatch", "cold-ownedtip", "cold-no-record", "cold-slow", "cold-throws",
  "cold-no-token", "captured-journal-live"]);

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
  const containsFlag = mode === "active" || mode === "git-unknown" || mode === "no-token" ||
    mode === "detail-400" || mode === "upload-failed" || COLD.has(mode);
  const s = commit(containsFlag ? "progress on the flagged tip" : "clean replacement",
    containsFlag ? q : mode === "callback-finding" ? historical : base);
  await fetch();
  assert.equal(await git.ancestry(bare, q, s), containsFlag ? "ancestor" : "divergent");
  client.protocolFeatures = mode === "no-token" || mode === "cold-no-token" ? ["claim_generation_fence", ADOPTION]
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
    teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 1,
    ...(mode === "cold-slow" ? { codexBoundaryDeadlineMs: 3000 } : {}) });
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
    // A real ownedTip mismatch: the captured restore source descends from the flagged commit (so the restricted roots
    // WOULD classify active against it) but is not the tip the remediation record owns.
    const s2 = mode === "cold-ownedtip" ? command(bare, "-c", "user.name=t", "-c", "user.email=t@example.com",
      "commit-tree", base + "^{tree}", "-p", s, "-m", "unowned descendant") : undefined;
    const coverage = s2 ? await cache.buildRecoveryCoverage(bare, {
      runId: claim.run_id, generation: 2, kind: "issue", branch, barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
    }, [q, s2], s2) : await coverageFor();
    await cache.recordRecoveryCapture(bare, branch, key4, source, iteration.attempts, coverage.sha,
      s2 ?? (mode === "cold-mismatch" ? base : s));
    if (mode !== "captured-journal-live") await cache.blockRecoveryEpisode(bare, branch, key4, source, "adoption_failed", iteration);
    if (mode === "cold-no-record") {
      const j = JSON.parse(journalRaw()); delete j.remediationRecovery;
      command(bare, "config", key, JSON.stringify(j));
    }
  }
  // Issue #1995 shape: an older archive was accepted, the latest upload failed, and the verified bundle is local only.
  let custody: { accepted?: string; failed?: string; bundlePath?: string; uploads: UploadClient } | undefined;
  if (mode === "upload-failed") {
    const uploads = new UploadClient();
    const coordinator = new RecoveryCoordinator({ client: uploads as unknown as RecoveryArchiveClient, git: cache, log: nullLogger(),
      recoveryRoot: path.join(fx.dataDir, "recovery-4177"), workerToken: "worker-join-token-abcdef0123456789" });
    const pinInput = { runId: claim.run_id, kind: "issue" as const, branch, generation: 1,
      bareDir: path.basename(bare), defaultBranch: "main", finalizationPin: true };
    const older = await coordinator.pin({ ...pinInput, sourceSha: q });
    const accepted = await coordinator.captureAndUpload({ record: older!, barePath: bare, defaultBranch: "main" });
    uploads.failNext = true;
    // A later claim generation journals its own record, so the accepted archive of the older one cannot satisfy it.
    const latest = await coordinator.pin({ ...pinInput, generation: 2, sourceSha: s });
    const failed = await coordinator.captureAndUpload({ record: latest!, barePath: bare, defaultBranch: "main" });
    const record = (await coordinator.inspect(claim.run_id)).find(r => r.sourceSha === s);
    custody = { accepted: accepted.state, failed: `${failed.state}:${failed.reason}`, bundlePath: record?.bundlePath, uploads };
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
  // Crash rediscovery seam: counted always, slowed or failed per mode.
  let classifierCalls = 0;
  const classify = cache.classifyBlockedRemediationAdoption.bind(cache);
  t.mock.method(cache, "classifyBlockedRemediationAdoption", async (...args: Parameters<typeof classify>) => {
    classifierCalls++;
    if (mode === "cold-slow") await new Promise(resolve => setTimeout(resolve, 3500));
    if (mode === "cold-throws") throw new Error("bare lock contended");
    // The rediscovery seam would say active; this flight's own unclassified refusal must still win.
    if (mode === "callback-guard") return "secret_remediation_active" as const;
    return classify(...args);
  });
  let installed = false;
  t.mock.method(runner as any, "restoreRemediationRestrictions", async (f: any) => {
    flight = f;
    if (!installed && f.predecessorCapture) {
      installed = true;
      (runner as any).latchRemediationFindings(f, bare, branch, [{ commit: q }]);
    }
    const restored = await restore(f);
    // An owned tip the callback's first guard refuses (a plain, unclassified refusal) while git's record would
    // classify active.
    return mode === "callback-guard" && restored ? { ...restored, ownedTip: base } : restored;
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
  // The iteration deadline reads as passed only to the admission callback's own clock read right after its unknown
  // probe, so the park itself is still a plain adoption failure and nothing else in the run sees the shift.
  let probed = false;
  if (mode === "callback-interrupted") {
    const realNow = Date.now.bind(Date);
    // Stack frames: 0 the message, 1 this mock, 2 node's mock wrapper, 3 the caller of Date.now.
    t.mock.method(Date, "now", () => probed && (new Error().stack ?? "").split("\n")[3]?.includes("admitRemediationSource")
      ? (probed = false, realNow() + 600_000) : realNow());
  }
  const ancestry = cache.ancestry.bind(cache);
  t.mock.method(cache, "ancestry", async (...args: Parameters<typeof ancestry>) => {
    if (mode === "git-unknown" && args[1] === q && args[2] === s) return "unknown";
    if (mode === "callback-unknown" && underB) return "unknown";
    if (mode === "callback-interrupted" && underB) { probed = true; return "unknown"; }
    return ancestry(...args);
  });
  if (mode === "detail-400") api.failStateWhen(claim.run_id, b => b.recovery_detail !== undefined, { httpStatus: 400 });
  try {
    await runner.execute(claim);
    if (mode === "deadline-rewrite") t.mock.restoreAll();
    return {
      parks: api.states.filter(x => x.runId === claim.run_id && x.body.status === "recovery_wait").map(x => x.body as unknown as Record<string, unknown>),
      reported: api.states.filter(x => x.runId === claim.run_id).map(x => String(x.body.status)),
      models, logs, classifierCalls, custody, wire: api.stateWire.filter(x => x.runId === claim.run_id).map(x => x.body as unknown as Record<string, unknown>),
      bare, branch, q, s, base, originBefore, journal: () => JSON.parse(journalRaw()),
      raw: journalRaw, coldBefore, flight,
      origin: () => ({ all: command(fx.originPath, "rev-list", "--all"), main: command(fx.originPath, "rev-parse", "refs/heads/main") }),
      clonePath: clone.path,
    };
  } finally { runner.shutdown(); }
}

let counter = 4300;

/** The archive api of issue #1995's live-recovery harness: accepts the first bundle, refuses the next with a transient 503. */
class UploadClient {
  uploads = 0; failNext = false; releases = 0;
  async reserveRecoveryCapture() { return { capture_id: "server-capture-1", state: "preparing" as const }; }
  async getRecoveryCaptureStatus(_run: string, captureId: string) { return { capture_id: captureId, state: "preparing" as const, manifest_bound: false }; }
  async uploadRecoveryBundle(_run: string, captureId: string, _manifest: unknown, bundle: AsyncIterable<unknown>) {
    for await (const _chunk of bundle) { /* drain */ }
    this.uploads++;
    if (this.failNext) throw new RequestError("POST", "/archives/x/upload", 503, JSON.stringify({ error: "msg", reason: "busy" }));
    return { capture_id: captureId, state: "available" as const, manifest_bound: true };
  }
  async releaseRecoveryCustody(runId: string) { this.releases++; return { run_id: runId, released: true, holds_released: 1 }; }
  async listRecoveryHolds(runId: string) { return { run_id: runId, holds: [] }; }
}

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
  ["cold-mismatch", undefined],
  ["cold-ownedtip", undefined],
  ["cold-no-record", undefined],
  ["cold-slow", undefined],
  ["cold-throws", undefined],
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
    assert.equal(r.classifierCalls, 1, "the claim rediscovered exactly once");
    assert.ok(!r.reported.includes("failed"), "rediscovery never turns the park into a failure: " + JSON.stringify(r.reported));
    if (mode === "cold" || mode === "cold-mismatch") assert.equal(r.raw(), r.coldBefore, "rediscovery wrote no journal content");
  });
}

test("a live adoption after a captured journal classifies in memory and never rediscovers", async t => {
  const r = await drive(t, "captured-journal-live", counter++);
  assert.equal(r.models, 0);
  assert.ok(r.parks.length >= 1, JSON.stringify({ reported: r.reported, logs: r.logs }));
  for (const park of r.parks) {
    assert.equal(park.recovery_cause, "retained_recovery_adoption_failed");
    assert.equal(park.recovery_detail, "secret_remediation_active");
  }
  assert.equal(r.classifierCalls, 0, "this flight classified its own refusal");
});

test("an unclassified refusal in this flight is not replaced by a rediscovered class", async t => {
  // The callback's first guard refuses this flight's adoption without a class. Rediscovery (stubbed to answer active)
  // must not run and so cannot label the park with a refusal this flight never saw.
  const r = await drive(t, "callback-guard", counter++);
  assert.ok(r.parks.length >= 1, JSON.stringify({ reported: r.reported, logs: r.logs }));
  assert.ok(r.parks.every(p => p.recovery_cause === "retained_recovery_adoption_failed" && !("recovery_detail" in p)), JSON.stringify(r.parks));
  assert.equal(r.classifierCalls, 0);
});

test("an unknown probe after the iteration deadline fired carries no class", async t => {
  const r = await drive(t, "callback-interrupted", counter++);
  assert.ok(r.parks.length >= 1, JSON.stringify({ reported: r.reported, logs: r.logs }));
  assert.ok(r.parks.every(p => p.recovery_cause === "retained_recovery_adoption_failed" && !("recovery_detail" in p)), JSON.stringify(r.parks));
});

test("an api that 400s the detail-bearing park is retried with the plain body and parks", async t => {
  const r = await drive(t, "detail-400", counter++);
  const detailed = r.wire.filter(b => b.recovery_detail !== undefined);
  assert.equal(detailed.length, 1, "the detail was tried once and refused: " + JSON.stringify(r.wire));
  assert.ok(r.parks.length >= 1, JSON.stringify({ reported: r.reported, logs: r.logs }));
  assert.ok(r.parks.every(p => p.recovery_cause === "retained_recovery_adoption_failed" && p.claim_generation === 2 &&
    !("recovery_detail" in p)), JSON.stringify(r.parks));
  assert.ok(!r.reported.includes("failed"));
});

test("an upload that failed with the verified bundle local still parks with the detail and keeps custody", async t => {
  const r = await drive(t, "upload-failed", counter++);
  const custody = r.custody!;
  assert.equal(custody.accepted, "uploaded", "the older archive was accepted");
  assert.ok(custody.failed?.startsWith("needs_action"), "the latest upload failed: " + custody.failed);
  assert.ok(custody.bundlePath && fs.existsSync(custody.bundlePath), "the local bundle is journaled on disk");
  assert.ok(r.parks.length >= 1, JSON.stringify({ reported: r.reported, logs: r.logs }));
  for (const park of r.parks) {
    assert.equal(park.recovery_cause, "retained_recovery_adoption_failed");
    assert.equal(park.recovery_detail, "secret_remediation_active");
  }
  assert.equal(custody.uploads.releases, 0, "custody was not released");
  assert.equal(r.models, 0);
  // The verified local bundle holds the retained source tip, and the retained source itself is untouched.
  command(r.bare, "bundle", "verify", custody.bundlePath);
  assert.ok(command(r.bare, "bundle", "list-heads", custody.bundlePath).split("\n").some(l => l.startsWith(r.s)),
    "the bundle carries the retained source tip");
  assert.ok(fs.existsSync(r.clonePath));
  assert.equal(r.journal().recovery.blocker, "adoption_failed");
});

test("without the api token a cold claim does not even rediscover", async t => {
  const r = await drive(t, "cold-no-token", counter++);
  assert.ok(r.parks.length >= 1, JSON.stringify({ reported: r.reported, logs: r.logs }));
  assert.ok(r.parks.every(p => p.recovery_cause === "retained_recovery_adoption_failed" && !("recovery_detail" in p)));
  assert.equal(r.classifierCalls, 0, "rediscovery is gated on the advertised feature");
});

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
