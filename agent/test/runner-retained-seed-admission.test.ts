// issue #3865: capacity admission for a retained successor seed, inside the reserved recovery iteration.
// Real git, a real DiskGovernor (only for its effective hard threshold) and a DataVolumeGuard whose statfs,
// stat and reclaim seams are injected. Bytes AND inodes stay far above the claim-time floors throughout, so
// the existing floor preflight never fires and only the new admission decides.
import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { DiskGovernor, type CacheCapConfig } from "../src/cache-cap.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import type { ExecutorFactory } from "../src/runner.js";
import type { StateRequest } from "../src/protocol.js";
import { noProofReseed, nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const BSIZE = 4096;
const BLOCKS = 10_000_000; // 40.96 GB
const FILES = 10_000_000;
const ALL_FEATURES = ["claim_generation_fence", "recovery_cause_data_volume_full", "recovery_cause_retained_recovery_exhausted"];
// The governor's api threshold is 0.90 and its margin 0.03: the effective hard threshold is 0.87.
const GOVERNOR_ON: CacheCapConfig = {
  capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 2, hardStopEnabled: true, hardMargin: 0.03,
};

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
async function seed() {
  const claim = gitlabClaim(2512, { claim_generation: 2, session_id: "predecessor-session" });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, 2512, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone };
}
function journal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}
function factory(run: () => Promise<never>): ExecutorFactory {
  return runId => ({ homeDir: path.join(homeDir, runId), executor: { run } });
}

interface Opts {
  /** Initial used fraction of the data volume. */
  used: number;
  features?: string[];
  governor?: "on" | "hard-stop-off-cap-on" | "all-off" | "none";
  /** Device id by path; default every path is on device 7 (the data volume's). */
  dev?: (p: string, predecessorClone: string) => number;
  /** Called after each reclaim pass (1-based count). */
  onReclaim?: (n: number, set: (used: number) => void) => void;
  /** Stub the predecessor measurement. */
  estimate?: { bytes: number; inodes: number; complete: boolean };
}

async function drive(o: Opts, prepare?: (ctx: { claim: ReturnType<typeof gitlabClaim>; bare: string; clone: { path: string; branch: string } }) => void) {
  const { claim, bare, clone } = await seed();
  client.protocolFeatures = o.features ?? ALL_FEATURES;
  const state = { used: o.used };
  const r = { models: 0, ensureClones: 0, prepares: 0, reclaims: 0, releases: 0, parks: [] as StateRequest[], measures: 0 };
  const ensure = git.ensureClone.bind(git);
  git.ensureClone = async (...args) => { r.ensureClones++; return ensure(...args); };
  const prep = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => { r.prepares++; return prep(...args); };
  const measure = git.measureRetainedSeedFootprint.bind(git);
  git.measureRetainedSeedFootprint = async (p, opts) => {
    r.measures++;
    return o.estimate ?? measure(p, opts);
  };
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs: () => {
      const free = Math.round(BLOCKS * (1 - state.used));
      return { bsize: BSIZE, blocks: BLOCKS, bfree: free, bavail: free, files: FILES, ffree: Math.round(FILES * 0.8) };
    },
    stat: async p => ({ dev: o.dev?.(p, clone.path) ?? 7 }),
    reclaim: async () => { r.reclaims++; o.onReclaim?.(r.reclaims, used => { state.used = used; }); },
  });
  client.releaseRecoveryCustody = async () => { r.releases++; throw new Error("must retain custody"); };
  const report = client.reportState.bind(client);
  client.reportState = async (runId, body, signal) => {
    if (body.status === "recovery_wait" && body.recovery_cause === "retained_recovery_exhausted") {
      r.parks.push(body);
      api.setOwnershipStatus(runId, "recovery_wait", 2);
      return { applied: true, status: "recovery_wait", recoveryCause: "retained_recovery_exhausted" };
    }
    return report(runId, body, signal);
  };
  const mode = o.governor ?? "on";
  const diskGovernor = mode === "none" ? undefined : new DiskGovernor({
    config: mode === "on" ? GOVERNOR_ON
      : mode === "hard-stop-off-cap-on" ? { ...GOVERNOR_ON, hardStopEnabled: false }
        : { ...GOVERNOR_ON, capEnabled: false, hardStopEnabled: false },
    log: nullLogger(),
    volumeTotalBytes: () => BLOCKS * BSIZE,
    thresholdOf: () => 0.9,
  });
  prepare?.({ claim, bare, clone });
  await runnerWith(factory(async () => { r.models++; throw new Error("model interrupted before settled completion"); }),
    fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1, dataVolume: guard, ...(diskGovernor ? { diskGovernor } : {}) },
  ).execute(claim);
  return { ...r, claim, bare, clone, state };
}

type Result = Awaited<ReturnType<typeof drive>>;
function attemptDirs(clone: { path: string }): string[] {
  return fs.readdirSync(path.dirname(clone.path)).filter(name => name.includes(".attempt-"));
}
function feed(r: Result): string[] {
  return api.messages(r.claim.run_id)
    .filter(m => m.kind === "status")
    .map(m => String((m.payload as { text?: unknown }).text));
}
function assertNoDiskPark(): void {
  assert.equal(api.states.some(s => s.body.recovery_cause === "data_volume_full"), false);
}
/** A fully refused three-iteration window: the existing budget_exhausted blocker and exhausted park. */
function assertRefusedWindow(r: Result): void {
  assert.equal(r.models, 0, "no model call");
  assert.equal(r.ensureClones, 0, "no clone refresh");
  assert.equal(r.prepares, 0, "no successor prepared");
  assert.deepEqual(attemptDirs(r.clone), [], "no attempt dir next to the predecessor");
  assert.equal(r.releases, 0, "custody is never released");
  assertNoDiskPark();
  assert.equal(api.states.some(s => s.body.status === "failed"), false, "the exhausted park replaces the failed report");
  assert.equal(r.parks.length, 1, "exactly one exhausted park");
  assert.deepEqual(r.parks[0], { status: "recovery_wait", recovery_cause: "retained_recovery_exhausted", claim_generation: 2 });
  const retained = journal(r.bare, r.clone.branch);
  assert.equal(retained.clonePath, r.clone.path, "no successor replaces the predecessor");
  assert.equal(retained.recovery.stage, "blocked");
  assert.equal(retained.recovery.blocker, "budget_exhausted");
  assert.equal(retained.recovery.attempts, 3);
  assert.equal(r.reclaims, 3, "one bounded reclaim per refused iteration");
  assert.equal(fs.readFileSync(path.join(r.clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  const texts = feed(r).filter(t => /retained recovery deferred/.test(t));
  assert.equal(texts.length, 3, JSON.stringify(feed(r)));
  for (const t of texts) {
    assert.match(t, /^retained recovery deferred: data volume at \d+\.\d%.* \(hard threshold 87\.0%\); not seeding a fresh attempt this iteration; predecessor work and custody retained$/);
  }
}
function assertSeeded(r: Result): void {
  assert.equal(r.models, 1, "the seed proceeds to the model");
  assert.equal(r.prepares, 1);
  assert.equal(r.parks.length, 0);
  assertNoDiskPark();
}

it("(a) current pressure at the hard threshold refuses every seed in the window without a park or successor", async () => {
  const r = await drive({ used: 0.89 });
  assertRefusedWindow(r);
  assert.equal(r.measures, 0, "current pressure refuses before any measurement");
  assert.ok(feed(r).some(t => t.includes("data volume at 89.0% (hard threshold 87.0%)")), JSON.stringify(feed(r)));
});

it("(b) current below the threshold but projected at or over it refuses the same way", async () => {
  const r = await drive({ used: 0.80, estimate: { bytes: 8 * 1024 ** 3, inodes: 10, complete: true } });
  assertRefusedWindow(r);
  assert.equal(r.measures, 6, "a measurement before and after each iteration's reclaim");
  assert.ok(feed(r).every(t => /projected \d+\.\d%/.test(t)), JSON.stringify(feed(r)));
});

it("control: headroom seeds exactly as before", async () => {
  const r = await drive({ used: 0.30 });
  assertSeeded(r);
  assert.equal(r.reclaims, 0);
  assert.equal(r.measures, 1);
  assert.deepEqual(feed(r).filter(t => /retained recovery deferred/.test(t)), []);
});

it("control: refused first iteration, admitted on attempt 2 once usage drops", async () => {
  const r = await drive({ used: 0.89, onReclaim: (n, set) => { if (n === 2) set(0.30); } });
  assert.equal(r.models, 1);
  assert.equal(r.prepares, 1);
  assert.equal(r.reclaims, 2);
  assert.equal(r.parks.length, 0);
  assertNoDiskPark();
  assert.equal(journal(r.bare, r.clone.branch).recovery.attempts, 2);
  assert.equal(feed(r).filter(t => /retained recovery deferred/.test(t)).length, 1);
});

it("control: a reclaim that frees space during the wait admits in iteration 1", async () => {
  const r = await drive({ used: 0.89, onReclaim: (_n, set) => set(0.30) });
  assert.equal(r.reclaims, 1);
  assert.equal(r.models, 1);
  assert.equal(journal(r.bare, r.clone.branch).recovery.attempts, 1);
  assert.deepEqual(feed(r).filter(t => /retained recovery deferred/.test(t)), []);
  assertNoDiskPark();
});

for (const missing of [
  ["recovery_cause_data_volume_full"], ["recovery_cause_retained_recovery_exhausted"],
  ["recovery_cause_data_volume_full", "recovery_cause_retained_recovery_exhausted"],
]) {
  it(`control: feature(s) missing (${missing.join(", ")}) keep today's behaviour even at 0.89 used`, async () => {
    const r = await drive({ used: 0.89, features: ALL_FEATURES.filter(f => !missing.includes(f)) });
    assert.equal(r.models, 1, "seed proceeds");
    assert.equal(r.prepares, 1);
    assert.equal(r.measures, 0);
    assert.equal(r.reclaims, 0);
    assert.deepEqual(feed(r).filter(t => /retained recovery deferred/.test(t)), []);
  });
}

for (const governor of ["hard-stop-off-cap-on", "all-off"] as const) {
  it(`control: governor with ${governor} still refuses (the check does not follow the hard-stop switch)`, async () => {
    assertRefusedWindow(await drive({ used: 0.89, governor }));
  });
}

it("control: no governor configuration wired keeps today's behaviour", async () => {
  const r = await drive({ used: 0.89, governor: "none" });
  assert.equal(r.models, 1);
  assert.equal(r.prepares, 1);
  assert.equal(r.reclaims, 0);
});

it("control: a destination on another device admits", async () => {
  const r = await drive({ used: 0.89, dev: (p, clone) => (p === path.dirname(clone) ? 8 : 7) });
  assert.equal(r.models, 1);
  assert.equal(r.prepares, 1);
  assert.equal(r.reclaims, 0);
});

it("control: a predecessor on another device is still checked against the destination's volume", async () => {
  assertRefusedWindow(await drive({ used: 0.89, dev: (p, clone) => (p === clone ? 9 : 7) }));
});

it("control: an iteration deadline that passes during the reclaim wait ends as budget_exhausted with the exhausted park", async t => {
  let ctx: { bare: string; clone: { branch: string } } | undefined;
  const r = await drive({
    used: 0.89,
    onReclaim: () => {
      const deadline = journal(ctx!.bare, ctx!.clone.branch).recovery.deadline as number;
      t.mock.method(Date, "now", () => deadline + 1);
    },
  }, c => { ctx = c; });
  assert.equal(r.models, 0);
  assert.equal(r.prepares, 0);
  assert.deepEqual(attemptDirs(r.clone), []);
  assert.equal(r.reclaims, 1);
  assert.equal(r.parks.length, 1);
  assert.equal(r.parks[0]!.recovery_cause, "retained_recovery_exhausted");
  assertNoDiskPark();
  const retained = journal(r.bare, r.clone.branch);
  assert.equal(retained.recovery.stage, "blocked");
  assert.equal(retained.recovery.blocker, "budget_exhausted");
  assert.equal(retained.recovery.attempts, 1, "the deadline, not the attempt cap, ended the window");
  assert.equal(fs.readFileSync(path.join(r.clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
});

it("control: a renewed adoption_failed episode refused for capacity ends budget_exhausted, never adoption failed", async t => {
  const { claim, bare, clone } = await seed();
  client.protocolFeatures = [...ALL_FEATURES, "recovery_cause_retained_recovery_adoption_failed"];
  const parks: string[] = [];
  const report = client.reportState.bind(client);
  client.reportState = async (runId, body, signal) => {
    if (body.status === "recovery_wait" && body.recovery_cause?.startsWith("retained_recovery_")) {
      parks.push(body.recovery_cause);
      api.setOwnershipStatus(runId, "recovery_wait", body.claim_generation as number);
      return { applied: true, status: "recovery_wait", recoveryCause: body.recovery_cause };
    }
    return report(runId, body, signal);
  };
  let prepares = 0, models = 0;
  const prep = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => { prepares++; return prep(...args); };
  t.mock.method(git as any, "resolveMarkerWrapperAdoption", async () => { throw new Error("injected adoption"); });
  const execute = async (extra: Record<string, unknown>) => {
    api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation);
    await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
      fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1, ...extra }).execute(claim);
  };
  (claim as any).worker_recovery_episode = 7;
  (claim as any).worker_recovery_owner_episode = 0;
  await execute({});
  assert.deepEqual(parks, ["retained_recovery_adoption_failed"]);
  assert.equal(journal(bare, clone.branch).recovery.blocker, "adoption_failed");
  const preparesBefore = prepares;
  // The owner resumes: a renewed window on a pressured volume.
  claim.claim_generation = 3;
  (claim as any).worker_recovery_episode = 8;
  (claim as any).worker_recovery_owner_episode = 1;
  const free = Math.round(BLOCKS * 0.11);
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs: () => ({ bsize: BSIZE, blocks: BLOCKS, bfree: free, bavail: free, files: FILES, ffree: Math.round(FILES * 0.8) }),
    stat: async () => ({ dev: 7 }),
  });
  const diskGovernor = new DiskGovernor({ config: GOVERNOR_ON, log: nullLogger(), volumeTotalBytes: () => BLOCKS * BSIZE, thresholdOf: () => 0.9 });
  await execute({ dataVolume: guard, diskGovernor });
  assert.equal(models, 0);
  assert.equal(prepares, preparesBefore, "the refused renewal seeds nothing");
  assert.deepEqual(attemptDirs(clone), []);
  assert.deepEqual(parks, ["retained_recovery_adoption_failed", "retained_recovery_exhausted"]);
  assert.equal(journal(bare, clone.branch).recovery.blocker, "budget_exhausted");
  assert.equal(api.states.some(s => s.body.status === "failed"), false);
  assertNoDiskPark();
});

it("control: an owner cancel during the refusal takes the existing cancel path without a successor or a disk park", async () => {
  let runId = "";
  const r = await drive({
    used: 0.89,
    onReclaim: () => { api.setOwnershipStatus(runId, "cancelled", 2); },
  }, c => { runId = c.claim.run_id; });
  assert.equal(r.models, 0);
  assert.equal(r.prepares, 0);
  assert.deepEqual(attemptDirs(r.clone), []);
  assert.equal(r.reclaims, 1, "no further iteration after the cancel");
  assert.equal(r.parks.length, 0, "a cancelled run is not parked");
  assert.equal(journal(r.bare, r.clone.branch).recovery.attempts, 1, "no further iteration was charged");
  assert.equal(r.releases, 0);
  assertNoDiskPark();
});
