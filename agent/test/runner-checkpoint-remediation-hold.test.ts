// Issue #1964: remembered findings hold every checkpoint sink. Real git, deterministic scans.
import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { RunContext } from "../src/executor.js";
import type { CheckpointRange } from "../src/git.js";
import { LimitReachedError } from "../src/limit.js";
import type { RunRunner } from "../src/runner.js";
import type { Logger } from "../src/log.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, fakeGitlab, gitlabClaim, installHarness, runnerWith, homeDir } from "./runner-harness.js";

installHarness();

type State = {
  attempts: number;
  known?: object[];
  blocked?: object[];
  flaggedCommits?: string[];
  flaggedFindings?: object[];
  overflow?: boolean;
  everKnown?: boolean;
  cleanTip?: string;
};
type Flight = Record<string, unknown> & { secretRemediation?: State; runLog: Logger };
type Outcome = { published: boolean; reason?: string };
type Subject = {
  requireTrackingOwned: (...args: unknown[]) => Promise<string | null>;
  publishCheckpointOutcome: (f: Flight, bare: string, branch: string, overlay?: unknown,
    signal?: AbortSignal, pinned?: CheckpointRange, onStep?: unknown, reap?: boolean) => Promise<Outcome>;
  captureHoldContext: (claim: unknown, f: Flight, log: Logger) =>
    Promise<{ verified: boolean; published: boolean; mode: string; head: string | null }>;
  runSecretRemediationGate: (f: Flight, ...rest: unknown[]) => Promise<unknown>;
  captureRecoveryRestorePoint: (claim: unknown, f: Flight, log: Logger, site: string,
    opts?: { credentialFree?: boolean }) => Promise<{ verified: boolean; published: boolean }>;
};

function commit(tree: string, text = "fixture\n"): string {
  fs.writeFileSync(path.join(tree, "remembered.txt"), text);
  execFileSync("git", ["-C", tree, "add", "remembered.txt"]);
  execFileSync("git", ["-C", tree, "-c", "user.name=t", "-c", "user.email=t@t",
    "-c", "commit.gpgsign=false", "commit", "-qm", "fixture"]);
  return execFileSync("git", ["-C", tree, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
}

async function withFlight(body: (ctx: RunContext, f: Flight, r: Subject, runner: RunRunner) => Promise<void>, lifecycle = false): Promise<string> {
  let flight: Flight | undefined;
  let failure: unknown;
  const { gitlab } = fakeGitlab();
  const runner = runnerWith(() => ({
    homeDir,
    executor: { run: async (ctx: RunContext) => {
      try {
        await ctx.secretRemediationGate!();
        assert.ok(flight);
        await body(ctx, flight, subject, runner);
      } catch (e) { if (!lifecycle) failure = e; throw e; }
      return { branch: ctx.branch };
    } },
  }), gitlab, undefined, nullLogger(), { checkpointIntervalMs: 60_000 });
  const subject = runner as unknown as Subject;
  const gate = subject.runSecretRemediationGate.bind(runner);
  subject.runSecretRemediationGate = async (f, ...rest) => {
    flight = f;
    return gate(f, ...rest);
  };
  await runner.execute(gitlabClaim(1964, { wait_on_limit: true }));
  if (failure) throw failure;
  return String(flight?.runId);
}

it("credentialFree capture keeps a verified local restore point without publishing a remembered flagged SHA", async () => {
  let uploads = 0;
  client.publishCheckpoint = async (_id, _tip, pack) => {
    uploads++;
    for await (const _chunk of pack) { /* drain real producer */ }
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/fixture" } };
  };
  await withFlight(async (ctx, flight, subject) => {
    const sha = commit(ctx.worktreePath);
    flight.secretRemediation = { attempts: 0, flaggedCommits: [sha] };
    const result = await subject.captureRecoveryRestorePoint(gitlabClaim(1964), flight, flight.runLog,
      "recovery_capture", { credentialFree: true });
    assert.deepEqual(result, { verified: true, published: false });
    assert.equal(uploads, 0);
    assert.equal(await git.trackingTip(String(flight.barePath), ctx.branch), sha);
  });
});

function spyUploads(): string[] {
  const tips: string[] = [];
  client.publishCheckpoint = async (_id, tip, pack) => {
    tips.push(tip);
    for await (const _chunk of pack) { /* drain real producer */ }
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/fixture" } };
  };
  return tips;
}

const unchangedKeys = ["lastPublish", "pendingPublish", "lastPublishedTip", "lastCheckpointRefTip",
  "lastAttemptedCheckpointRefTip", "checkpointFloor", "landedCheckpoint", "publishedRealTips"];
function snapshot(f: Flight): unknown[] { return unchangedKeys.map((key) => f[key]); }

it("common source guard: state table, metadata independence, failed ancestry, no producer/state advancement and feed dedupe", async () => {
  const tips = spyUploads();
  await withFlight(async (ctx, f, r) => {
    const sha = commit(ctx.worktreePath);
    await r.captureRecoveryRestorePoint(gitlabClaim(1964), f, f.runLog, "recovery_capture", { credentialFree: true });
    assert.equal(tips.length, 1);
    const bare = String(f.barePath);
    const realPack = git.checkpointPack.bind(git);
    let packs = 0;
    git.checkpointPack = async (...args) => { packs++; return realPack(...args); };
    const states: State[] = [
      { attempts: 0, known: [{}] },
      { attempts: 0, blocked: [{}] },
      { attempts: 0, flaggedCommits: [sha] },
      { attempts: 0, flaggedCommits: [sha], flaggedFindings: [] },
      { attempts: 0, flaggedCommits: ["0".repeat(40), sha], flaggedFindings: [{}] },
      { attempts: 0, overflow: true },
      { attempts: 0, everKnown: true },
      { attempts: 0, everKnown: true, cleanTip: "0".repeat(40) },
    ];
    for (const state of states) {
      f.secretRemediation = state;
      const before = snapshot(f);
      for (let repeat = 0; repeat < 2; repeat++) {
        assert.deepEqual(await r.publishCheckpointOutcome(f, bare, ctx.branch),
          { published: false, reason: "secret_remediation_pending" }, JSON.stringify(state));
      }
      assert.deepEqual(snapshot(f), before);
    }
    const ancestry = git.ancestry.bind(git);
    f.secretRemediation = { attempts: 0, flaggedCommits: [sha] };
    for (const result of ["unknown", "error"] as const) {
      git.ancestry = async () => { if (result === "error") throw new Error("unreadable"); return result; };
      assert.equal((await r.publishCheckpointOutcome(f, bare, ctx.branch)).reason, "secret_remediation_pending");
    }
    git.ancestry = ancestry;
    const owned = r.requireTrackingOwned.bind(r);
    r.requireTrackingOwned = async () => null;
    assert.equal((await r.publishCheckpointOutcome(f, bare, ctx.branch)).reason, "secret_remediation_pending");
    r.requireTrackingOwned = owned;
    assert.equal(packs, 0, "source refusal happens before checkpointPack");
    assert.equal(tips.length, 1);
    await (f.batcher as { flush: () => Promise<void> }).flush();
    assert.equal(api.messages(String(f.runId)).filter((m) =>
      m.payload.text === "checkpoint publish skipped: secret_remediation_pending").length, 1);
    // A fresh flight has no remediation memory; the unscanned sink can publish this same history.
    f.secretRemediation = undefined;
    assert.deepEqual(await r.publishCheckpointOutcome(f, bare, ctx.branch), { published: true });
    assert.equal(tips.length, 2);
  });
});

it("rewrite/clean controls and reap policy: cleanTip restricts boundary publishes but not reap:false", async () => {
  const tips = spyUploads();
  await withFlight(async (ctx, f, r) => {
    const base = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
    const flagged = commit(ctx.worktreePath);
    await r.captureRecoveryRestorePoint(gitlabClaim(1964), f, f.runLog, "recovery_capture", { credentialFree: true });
    execFileSync("git", ["-C", ctx.worktreePath, "reset", "--hard", base]);
    const clean = commit(ctx.worktreePath, "rewrite\n");
    f.publishedTip = base;
    f.checkpointFloor = flagged;
    f.secretRemediation = { attempts: 0, flaggedCommits: [flagged], everKnown: true, cleanTip: clean };
    assert.deepEqual(await r.captureRecoveryRestorePoint(gitlabClaim(1964), f, f.runLog,
      "recovery_capture", { credentialFree: true }), { verified: true, published: false });
    // The published flag was bridged back into the candidate above; prove that post-bridge ancestry holds.
    assert.equal(await git.ancestry(String(f.barePath), flagged, (await git.trackingTip(String(f.barePath), ctx.branch))!), "ancestor");
    // Remove the durable floor only for this clean control; no bridge is requested by the common seam.
    f.publishedTip = undefined;
    f.lastCheckpointRefTip = undefined;
    f.checkpointFloor = undefined;
    f.publishedRealTips = [];
    await ctx.secretRemediationGate!();
    f.secretRemediation = { attempts: 0, flaggedCommits: [flagged], everKnown: true, cleanTip: clean };
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch), { published: true });
    f.secretRemediation.cleanTip = base;
    const before = snapshot(f);
    assert.equal((await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch)).reason, "secret_remediation_pending");
    assert.deepEqual(snapshot(f), before);
    assert.deepEqual(await r.publishCheckpointOutcome(f, String(f.barePath), ctx.branch,
      undefined, undefined, undefined, undefined, false), { published: true });
    assert.equal(tips.length, 3);
  });
});

for (const pinned of [false, true]) {
  it(`actual candidate guard before producer: ${pinned ? "pinned" : "raw"} overlay resurrects a flagged commit`, async () => {
    const tips = spyUploads();
    await withFlight(async (ctx, f, r) => {
      const base = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
      const flagged = commit(ctx.worktreePath);
      // Transfer the flagged object locally without publishing it.
      await ctx.secretRemediationGate!();
      execFileSync("git", ["-C", ctx.worktreePath, "reset", "--hard", base]);
      const source = commit(ctx.worktreePath, "clean rewrite\n");
      await ctx.secretRemediationGate!();
      const bare = String(f.barePath);
      const tree = execFileSync("git", ["--git-dir", bare, "rev-parse", source + "^{tree}"], { encoding: "utf8" }).trim();
      const wrapper = execFileSync("git", ["--git-dir", bare, "-c", "user.name=t", "-c", "user.email=t@t",
        "commit-tree", tree, "-p", source, "-p", flagged, "-m", "overlay fixture"], { encoding: "utf8" }).trim();
      const overlayBuilder = git as unknown as { buildWorkflowOverlay: (...a: unknown[]) => Promise<string | null> };
      const build = overlayBuilder.buildWorkflowOverlay.bind(git);
      overlayBuilder.buildWorkflowOverlay = async () => wrapper;
      let producer = 0;
      const spawnable = git as unknown as { spawnGit: (...a: unknown[]) => Promise<unknown> };
      const spawn = spawnable.spawnGit.bind(git);
      spawnable.spawnGit = async (...args) => { if ((args[1] as string[])[0] === "pack-objects") producer++; return spawn(...args); };
      f.secretRemediation = { attempts: 0, flaggedCommits: [flagged], everKnown: true, cleanTip: source };
      const range = pinned ? await git.resolveCheckpointRange(bare, ctx.branch) : undefined;
      assert.ok(!pinned || range);
      const before = snapshot(f);
      assert.equal((await r.publishCheckpointOutcome(f, bare, ctx.branch, {}, undefined, range ?? undefined)).reason,
        "secret_remediation_pending");
      assert.equal(producer, 0);
      assert.equal(tips.length, 0);
      assert.deepEqual(snapshot(f), before);
      // cleanTip equality is against source, so an innocent wrapper may still publish.
      overlayBuilder.buildWorkflowOverlay = async () => source;
      assert.deepEqual(await r.publishCheckpointOutcome(f, bare, ctx.branch, {}, undefined, range ?? undefined), { published: true });
      assert.equal(producer, 1);
      overlayBuilder.buildWorkflowOverlay = build;
    });
  });
}

it("recovery/credential-switch and shared hold/wall captures retain local verification on a hold", async () => {
  const tips = spyUploads();
  await withFlight(async (ctx, f, r) => {
    const sha = commit(ctx.worktreePath);
    f.secretRemediation = { attempts: 0, flaggedCommits: [sha] };
    for (const site of ["recovery_capture", "credential_switch"]) {
      assert.deepEqual(await r.captureRecoveryRestorePoint(gitlabClaim(1964), f, f.runLog, site),
        { verified: true, published: false });
    }
    assert.deepEqual(await r.captureHoldContext(gitlabClaim(1964), f, f.runLog),
      { verified: true, published: false, mode: "same_worker_only", head: sha });
    assert.equal(tips.length, 0);
  });
});

it("pause fails rather than claiming a local-only pause for remembered findings", async () => {
  const tips = spyUploads();
  await withFlight(async (ctx, f) => {
    const sha = commit(ctx.worktreePath);
    f.secretRemediation = { attempts: 0, flaggedCommits: [sha] };
    assert.equal(await ctx.parkForPause!({ completedCount: 1, total: 2 }), false);
    assert.ok(api.states.some((s) => s.body.status === "pause_failed"));
    assert.ok(!api.states.some((s) => s.body.status === "paused"));
    assert.equal(tips.length, 0);
  });
});

it("limit park does not claim remote durability when the source is held", async () => {
  const tips = spyUploads();
  client.protocolFeatures = ["run_checkpoint_durability"];
  await withFlight(async (ctx, f) => {
    const sha = commit(ctx.worktreePath);
    f.secretRemediation = { attempts: 0, flaggedCommits: [sha] };
    throw new LimitReachedError({ resetsAtMs: Date.now() + 3600_000, rateLimitType: "five_hour" });
  }, true);
  assert.equal(tips.length, 0);
  const park = api.states.find((s) => s.body.status === "limit_wait");
  assert.ok(park, JSON.stringify(api.states));
  assert.equal(park.body.checkpoint_contains_latest, undefined);
});

it("shutdown names secret_remediation_pending as a typed local outcome", async () => {
  const tips = spyUploads();
  const runId = await withFlight(async (ctx, f, _r, runner) => {
    const sha = commit(ctx.worktreePath);
    f.secretRemediation = { attempts: 0, flaggedCommits: [sha] };
    runner.shutdown();
    throw new Error("shutdown fixture");
  }, true);
  assert.equal(tips.length, 0);
  assert.ok(api.messages(runId).some((m) =>
    typeof m.payload.text === "string" && /shutdown checkpoint.*secret_remediation_pending/.test(m.payload.text)));
});

it("deterministic gate finding on S requests remediation and holds every following unscanned capture", async () => {
  const tips = spyUploads();
  await withFlight(async (ctx, f, r) => {
    const sha = commit(ctx.worktreePath, "TOKEN=" + "gh" + "p_" + "a".repeat(36) + "\n");
    git.secretScanCheckpointRange = async () => ({ trusted: true,
      findings: [{ file: "remembered.txt", startLine: 1, commit: sha, ruleId: "github-pat" }] });
    assert.equal((await ctx.secretRemediationGate!()).action, "remediate");
    assert.equal(f.secretRemediation?.everKnown, true);
    assert.deepEqual(await r.captureRecoveryRestorePoint(gitlabClaim(1964), f, f.runLog,
      "recovery_capture", { credentialFree: true }), { verified: true, published: false });
    assert.equal(tips.length, 0);
  });
});

it("real gitleaks gate finding on S also holds credentialFree capture", async (t) => {
  let real: string;
  try { real = execFileSync("which", ["gitleaks"], { encoding: "utf8" }).trim(); }
  catch { t.skip("gitleaks is unavailable; deterministic regressions remain active"); return; }
  (git as unknown as { gitleaksBin: string }).gitleaksBin = real;
  const tips = spyUploads();
  await withFlight(async (ctx, f, r) => {
    commit(ctx.worktreePath, "TOKEN=" + "gh" + "p_" + "a1b2c3d4e5f6".repeat(3) + "\n");
    assert.equal((await ctx.secretRemediationGate!()).action, "remediate");
    assert.deepEqual(await r.captureRecoveryRestorePoint(gitlabClaim(1964), f, f.runLog,
      "recovery_capture", { credentialFree: true }), { verified: true, published: false });
    assert.equal(tips.length, 0);
  });
});

it("checkpointBody preserves the closed time gate and a defensive seam hold leaves its owed publish untouched", async () => {
  const tips = spyUploads();
  await withFlight(async (ctx, f) => {
    const sha = commit(ctx.worktreePath);
    f.secretRemediation = { attempts: 0, flaggedCommits: [sha] };
    const before = snapshot(f);
    await ctx.checkpoint!({ reap: false });
    assert.deepEqual(snapshot(f), before);
    await (f.batcher as { flush: () => Promise<void> }).flush();
    assert.equal(api.messages(String(f.runId)).filter((m) =>
      m.payload.text === "checkpoint publish skipped: secret_remediation_pending").length, 0,
      "a closed time gate retains its own outcome");
    f.secretRemediation = { attempts: 0 };
    f.pendingPublish = true;
    git.secretScanCheckpointRange = async () => {
      f.secretRemediation = { attempts: 0, overflow: true };
      return { trusted: true, findings: [] };
    };
    commit(ctx.worktreePath, "fresh owed work\n");
    const owed = snapshot(f);
    await ctx.checkpoint!({ reap: false, progress: { completed: ["hold-progress"], in_progress: [] } });
    assert.deepEqual(snapshot(f), owed, "seam refusal is not an attempt");
    assert.ok(api.states.some((s) => s.body.status === "running" &&
      s.body.milestones_completed?.includes("hold-progress")), "held activity still reports progress");
    assert.equal(tips.length, 0);
  });
});

it("checkpointBody judges SHA-only ancestry after a local bridge and before resolving an overlay", async () => {
  const tips = spyUploads();
  await withFlight(async (ctx, f, _r, runner) => {
    const base = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
    const flagged = commit(ctx.worktreePath);
    await ctx.secretRemediationGate!(); // transfer flagged object without publishing
    execFileSync("git", ["-C", ctx.worktreePath, "reset", "--hard", base]);
    commit(ctx.worktreePath, "rewrite\n");
    f.publishedTip = base;
    f.checkpointFloor = flagged; // local-only floor restores S as an ancestor
    f.secretRemediation = { attempts: 0, flaggedCommits: [flagged] };
    let overlays = 0;
    const overlay = runner as unknown as { buildCheckpointOverlay: (...a: unknown[]) => Promise<unknown> };
    overlay.buildCheckpointOverlay = async () => { overlays++; return {}; };
    await ctx.checkpoint!({ reap: true });
    assert.equal(overlays, 0, "pre-overlay hold skips even overlay context construction");
    assert.equal(tips.length, 0);
    const tip = (await git.trackingTip(String(f.barePath), ctx.branch))!;
    assert.equal(await git.ancestry(String(f.barePath), flagged, tip), "ancestor");
  });
});

for (const sink of ["completion", "wall"] as const) {
  it(`${sink} lifecycle parks with same-worker custody and no remote publish on a hold`, async () => {
    const tips = spyUploads();
    if (sink === "completion") api.setCompletionHoldResponse("paused", 200);
    await withFlight(async (ctx, f) => {
      const sha = commit(ctx.worktreePath);
      f.secretRemediation = { attempts: 0, flaggedCommits: [sha] };
      if (sink === "completion") {
        assert.equal(await ctx.enterCompletionHold!("fixture hold"), true);
        assert.equal(api.completionHoldRequests[0]?.body.head, sha);
        assert.equal(f.preserveRecoveryClone, true);
        assert.equal(f.preserveSession, true);
      } else {
        assert.equal(await ctx.parkForWall!({ completedCount: 1, total: 2 }), "parked");
        assert.equal(api.wallParkRequests[0]?.body.head, sha);
        assert.equal(f.preserveRecoveryClone, true);
        assert.equal(f.preserveSession, true);
        assert.equal(api.wallParkRequests[0]?.body.published, false);
      }
      assert.equal(tips.length, 0);
    });
  });
}

it("new flight/reclaim loses remediation memory: a prior flagged local commit can publish from an unscanned sink before a new finding", async () => {
  const tips = spyUploads();
  const claim = gitlabClaim(1964, { wait_on_limit: true, claim_generation: 1 });
  const { gitlab } = fakeGitlab();
  let first = true;
  let flagged = "";
  let inspected = false;
  const runner = runnerWith(() => ({ homeDir, executor: { run: async (ctx: RunContext) => {
    if (first) {
      flagged = commit(ctx.worktreePath);
      git.secretScanCheckpointRange = async () => ({ trusted: true,
        findings: [{ file: "remembered.txt", startLine: 1, commit: flagged, ruleId: "fixture" }] });
      assert.equal((await ctx.secretRemediationGate!()).action, "remediate");
    } else {
      assert.equal(execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { encoding: "utf8" }).trim(), flagged);
      // Deliberately no gate/scan before the unscanned limit sink.
    }
    throw new LimitReachedError({ resetsAtMs: Date.now() + 3600_000, rateLimitType: "five_hour" });
  } } }), gitlab, undefined, nullLogger(), { checkpointIntervalMs: 60_000 });
  const subject = runner as unknown as Subject;
  const publish = subject.publishCheckpointOutcome.bind(runner);
  subject.publishCheckpointOutcome = async (f, ...args) => {
    if (!first) { inspected = true; assert.equal(f.secretRemediation, undefined); }
    return publish(f, ...args);
  };
  await runner.execute(claim);
  assert.equal(tips.length, 0);
  first = false;
  git.secretScanCheckpointRange = async () => { throw new Error("unscanned sink must not scan"); };
  await runner.execute({ ...claim, claim_generation: 2 });
  assert.equal(inspected, true, JSON.stringify(api.states));
  assert.deepEqual(tips, [flagged]);
});
