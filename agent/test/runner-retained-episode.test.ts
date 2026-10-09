import { it, afterEach } from "node:test";
import { RecoveryClosureLimitError } from "../src/recovery-closure.js";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { deflateSync } from "node:zlib";
import { Readable } from "node:stream";
import type { ExecutorFactory } from "../src/runner.js";
import { Outbox } from "../src/outbox.js";
import { noProofReseed, nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();
function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
async function seed(iid = 2512, large = false) {
  const claim = gitlabClaim(iid, { claim_generation: 2, session_id: "predecessor-session" });
  if (large) {
    command(fx.originPath, "config", "user.name", "fixture");
    command(fx.originPath, "config", "user.email", "fixture@example.com");
    fs.writeFileSync(path.join(fx.originPath, "large-public"), randomBytes(64 * 1024));
    command(fx.originPath, "add", "large-public");
    command(fx.originPath, "commit", "-m", "public history");
  }
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone, key: `issue-${iid}` };
}
function readJournal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}
let callbackFailures: unknown[] = [];
afterEach(() => {
  const failures = callbackFailures;
  callbackFailures = [];
  assert.deepEqual(failures, [], "runner must not swallow model callback assertions");
});
function factory(run: (ctx: import("../src/executor.js").RunContext) => Promise<never>): ExecutorFactory {
  return runId => ({ homeDir: path.join(homeDir, runId), executor: { run: async ctx => {
    try { return await run(ctx); }
    catch (error) { if (error instanceof assert.AssertionError) callbackFailures.push(error); throw error; }
  } } });
}

for (const outageAt of [1, 2]) {
  it(`missing source finalization needs no ownership GET (former outage position ${outageAt})`, async () => {
    const { claim, bare, clone } = await seed();
    const before = readJournal(bare, clone.branch);
    client.protocolFeatures = ["claim_generation_fence"];
    const tip = command(clone.path, "rev-parse", "HEAD");
    const pin = `refs/uzi-recovery-episode/${claim.run_id}/${tip}`;
    command(bare, "update-ref", pin, tip);
    fs.renameSync(clone.path, clone.path + ".saved");
    let reads = 0, models = 0, fetches = 0, releases = 0;
    const ownership = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (...args) => {
      if (++reads === outageAt) throw new Error("ownership endpoint unavailable");
      return ownership(...args);
    };
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
    git.ensureClone = async () => { fetches++; throw new Error("must not refresh"); };
    await runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab).execute(claim);
    assert.equal(reads, 0, "discovery finalization uses the fenced terminal endpoint");
    assert.equal(models, 0);
    assert.equal(fetches, 0);
    assert.equal(releases, 0);
    const journal = readJournal(bare, clone.branch);
    assert.equal(journal.clonePath, before.clonePath);
    assert.equal(journal.runId, before.runId);
    assert.equal(journal.recovery.blocker, "source_missing");
    assert.equal(journal.recovery.attempts, 3);
    assert.equal(journal.recovery.stage, "blocked");
    assert.equal(command(bare, "rev-parse", pin), tip);
    assert.ok(api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2));
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
    assert.equal(fs.readFileSync(path.join(clone.path + ".saved", "retained.txt"), "utf8"), "only local dirty work\n");
  });
}

it("corrupt journal finalizes through the terminal fence with all ownership reads down", async () => {
  const { claim, bare, clone } = await seed();
  const corrupt = '{"runId":"broken"';
  client.protocolFeatures = ["claim_generation_fence"];
  command(bare, "config", `uzi-recovery.${clone.branch}.clone`, corrupt);
  let reads = 0, models = 0;
  client.getRunOwnership = async () => { reads++; throw new Error("ownership endpoint unavailable"); };
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab).execute(claim);
  assert.equal(reads, 0);
  assert.equal(models, 0);
  assert.equal(command(bare, "config", `uzi-recovery.${clone.branch}.clone`), corrupt);
  assert.ok(api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  assert.ok(fs.existsSync(clone.path));
});

for (const stage of ["reservation", "discovery"] as const) {
  it(`cancelled queued ${stage} settles execute before bare lock release without a charge`, async () => {
    const { claim, bare, clone } = await seed();
    const before = readJournal(bare, clone.branch);
    const configBytes = fs.readFileSync(path.join(bare, "config"));
    let entered!: () => void, acquired!: () => void, release!: () => void;
    const entry = new Promise<void>(resolve => { entered = resolve; });
    const held = new Promise<void>(resolve => { acquired = resolve; });
    // This holder is outside the recovery ALS context.
    const holder = git.withBareLock(bare, async () => {
      acquired();
      await new Promise<void>(resolve => { release = resolve; });
    });
    await held;
    git.recoveryAttemptMode = async () => false;
    if (stage === "reservation") {
      git.discoverRetainedRecovery = async () => ({ barePath: bare, journal: before });
      const reserve = git.reserveRecoveryIteration.bind(git);
      git.reserveRecoveryIteration = async (...args) => { entered(); return reserve(...args); };
    } else {
      const discover = git.discoverRetainedRecovery.bind(git);
      git.discoverRetainedRecovery = async (...args) => { entered(); return discover(...args); };
    }
    const retainedHome = path.join(homeDir, claim.run_id);
    fs.mkdirSync(retainedHome, { recursive: true });
    const sessionArtifact = path.join(retainedHome, "retained-session.jsonl");
    fs.writeFileSync(sessionArtifact, "predecessor session evidence\n");
    let models = 0, settled = false;
    const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab);
    const execution = r.execute(claim).finally(() => { settled = true; });
    try {
      await entry;
      (r as any).activeRuns.get(claim.run_id).cancel.abort();
      await Promise.race([execution, new Promise<void>(resolve => setTimeout(resolve, 250))]);
      assert.equal(settled, true, "cancelled flight must settle while the lock is still held");
      assert.deepEqual(readJournal(bare, clone.branch), before);
    } finally {
      release();
      await holder;
      await execution;
    }
    await git.withBareLock(bare, async () => {});
    assert.deepEqual(readJournal(bare, clone.branch), before);
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), configBytes);
    assert.equal(fs.readFileSync(sessionArtifact, "utf8"), "predecessor session evidence\n",
      "a stop before discovery completes retains the predecessor HOME");
    assert.equal(models, 0);
    assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
    assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  });
}

it("lifecycle cancellation settles the actual attempt-mode child without discovery or a terminal", async () => {
  const { claim, bare, clone } = await seed();
  const before = fs.readFileSync(path.join(bare, "config"));
  let started!: () => void, finish!: (value: { code: number }) => void;
  const entry = new Promise<void>(resolve => { started = resolve; });
  const completed = new Promise<{ code: number }>(resolve => { finish = resolve; });
  let cancelled = false, settledChild = false, models = 0, discoveries = 0;
  const discover = git.discoverRetainedRecovery.bind(git);
  git.discoverRetainedRecovery = async (...args) => { discoveries++; return discover(...args); };
  const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab);
  const execution = git.withBoundaryProcessSpawner(async () => {
    started();
    return {
      stdout: Readable.from([]), stderr: Readable.from([]), stdin: null, completed,
      cancel: async () => { cancelled = true; settledChild = true; finish({ code: -1 }); },
    };
  }, new AbortController().signal, () => r.execute(claim));
  await entry;
  (r as any).activeRuns.get(claim.run_id).cancel.abort();
  await execution;
  assert.equal(cancelled, true);
  assert.equal(settledChild, true);
  assert.equal(discoveries, 0);
  assert.equal(models, 0);
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
  assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
});

it("queued reservation deadline admits no proof or later charge and bounds finalization lock waits", async () => {
  const { claim, bare, clone, key } = await seed();
  await git.reserveRecoveryIteration(bare, clone.branch, key, { runId: claim.run_id, clonePath: clone.path }, 1);
  const before = readJournal(bare, clone.branch);
  before.recovery.startedAt = Date.now() - 299_000;
  before.recovery.deadline = before.recovery.startedAt + 300_000;
  command(bare, "config", `uzi-recovery.${clone.branch}.clone`, JSON.stringify(before));
  client.protocolFeatures = ["claim_generation_fence"];
  git.recoveryAttemptMode = async () => false;
  git.discoverRetainedRecovery = async () => ({ barePath: bare, journal: before });
  let release!: () => void, acquired!: () => void, entered!: () => void;
  const held = new Promise<void>(resolve => { acquired = resolve; });
  const entry = new Promise<void>(resolve => { entered = resolve; });
  const holder = git.withBareLock(bare, async () => {
    acquired();
    await new Promise<void>(resolve => { release = resolve; });
  });
  await held;
  const reserve = git.reserveRecoveryIteration.bind(git);
  git.reserveRecoveryIteration = async (...args) => { entered(); return reserve(...args); };
  let models = 0, proofs = 0, reads = 0, settled = false;
  client.getRunOwnership = async () => { reads++; throw new Error("must not read"); };
  const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    fakeGitlab().gitlab, undefined, nullLogger(), { codexBoundaryDeadlineMs: 100 });
  (r as any).quiesceRun = async () => { proofs++; throw new Error("must not prove"); };
  const execution = r.execute(claim).finally(() => { settled = true; });
  try {
    await entry;
    await Promise.race([execution, new Promise<void>(resolve => setTimeout(resolve, 2_000))]);
    assert.equal(settled, true, "deadline and bookkeeping must finish before unlock");
    assert.deepEqual(readJournal(bare, clone.branch), before);
  } finally {
    release();
    await holder;
    await execution;
  }
  await git.withBareLock(bare, async () => {});
  assert.deepEqual(readJournal(bare, clone.branch), before);
  assert.equal(models, 0);
  assert.equal(proofs, 0);
  assert.equal(reads, 0);
  assert.ok(api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});

for (const disposition of ["stale_claim", "released"]) {
  it(`missing source terminal refusal (${disposition}) preserves custody and replacement state`, async () => {
    const { claim, bare, clone } = await seed();
    client.protocolFeatures = ["claim_generation_fence"];
    fs.renameSync(clone.path, clone.path + ".saved");
    api.failStateWhen(claim.run_id, body => body.status === "failed", { runStatus: "running", disposition });
    let reports = 0, reads = 0, releases = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (...args) => {
      if (args[1].status === "failed") {
        reports++;
        assert.equal(args[1].claim_generation, 2);
        api.setOwnershipStatus(claim.run_id, "running", 3);
      }
      return report(...args);
    };
    client.getRunOwnership = async () => { reads++; throw new Error("must not read"); };
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
    await runnerWith(factory(async () => { throw new Error("must not model"); }), fakeGitlab().gitlab).execute(claim);
    assert.equal(reports, 1);
    assert.equal(reads, 0);
    assert.equal(releases, 0);
    assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
    assert.equal(readJournal(bare, clone.branch).recovery.blocker, "source_missing");
    assert.equal(fs.readFileSync(path.join(clone.path + ".saved", "retained.txt"), "utf8"), "only local dirty work\n");
  });
}

it("retained dirty work resumes locally on a fresh unwired path and resets only on trusted settled notification", async () => {
  const { claim, bare, clone, key } = await seed();
  let model = 0;
  const { gitlab } = fakeGitlab();
  client.publishCheckpoint = async () => { throw new Error("publication unknown"); };
  const r = runnerWith(factory(async ctx => {
    model++;
    assert.notEqual(ctx.worktreePath, clone.path);
    assert.match(ctx.worktreePath, /\.attempt-.*-g2-/);
    assert.equal(ctx.sessionId, undefined);
    assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "retained.txt"), "utf8"), "only local dirty work\n");
    const journal = readJournal(bare, clone.branch);
    assert.equal(journal.recovery.attempts, 1);
    assert.equal(journal.recovery.stage, "ready-for-model");
    await ctx.onModelTurnSettled!(1);
    assert.equal(readJournal(bare, clone.branch).recovery, undefined);
    throw new Error("fixture stops after settled turn");
  }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
  await r.execute(claim);
  assert.equal(model, 1);
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  assert.equal(await git.recoveryAttemptMode(fx.originPath, key), true);
  const journal = readJournal(bare, clone.branch);
  assert.ok(journal.retainedSources.some((s: any) => s.clonePath === clone.path));
  assert.equal(fs.existsSync(clone.path), true);
  assert.equal(fs.existsSync(journal.clonePath), true);
  assert.ok(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-recovery-episode/" + claim.run_id));
});

for (const succeeds of [true, false]) {
  it(`early forge failures consume the durable three-iteration budget (success=${succeeds})`, async () => {
    const { claim, bare, clone } = await seed();
    const ensure = git.ensureClone.bind(git);
    let fetches = 0, models = 0;
    git.ensureClone = async (...args) => {
      fetches++;
      if (!succeeds || fetches < 3) throw new Error("temporary connection reset");
      return ensure(...args);
    };
    const { gitlab } = fakeGitlab();
    await runnerWith(factory(async () => {
      models++;
      assert.equal(readJournal(bare, clone.branch).recovery.attempts, 3);
      throw new Error("model interrupted without settled evidence");
    }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
    assert.equal(fetches, 3);
    assert.equal(models, succeeds ? 1 : 0);
    const episode = readJournal(bare, clone.branch).recovery;
    assert.equal(episode.attempts, 3);
    assert.equal(episode.stage, succeeds ? "ready-for-model" : "blocked");
    if (!succeeds) {
      assert.equal(episode.blocker, "budget_exhausted");
      assert.ok(api.states.some(s => s.body.status === "failed"));
    }
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
    assert.equal(fs.existsSync(clone.path), true);
  });
}

it("known missing source blocks before credentialed refresh and terminal retains journal", async () => {
  const { claim, bare, clone } = await seed();
  fs.renameSync(clone.path, clone.path + ".saved");
  let fetches = 0;
  git.ensureClone = async () => { fetches++; throw new Error("must not refresh"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { throw new Error("must not model"); }), gitlab).execute(claim);
  assert.equal(fetches, 0);
  assert.equal(readJournal(bare, clone.branch).recovery.blocker, "source_missing");
  assert.ok(api.states.some(s => s.body.status === "failed"));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});

it("lost exact ownership stops the retained retry without a terminal or a park", async () => {
  const { claim, bare, clone } = await seed();
  let fetches = 0, models = 0;
  git.ensureClone = async () => {
    fetches++;
    api.setOwnershipStatus(claim.run_id, "running", 3);
    throw new Error("connection reset");
  };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { models++; throw new Error("must not run"); }),
    gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(fetches, 1);
  assert.equal(models, 0);
  assert.equal(readJournal(bare, clone.branch).recovery.attempts, 1);
  assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
  assert.ok(fs.existsSync(clone.path));
});

it("typed exact ownership 404 stops after one charge without touching the source", async () => {
  const { claim, bare, clone } = await seed();
  const before = fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8");
  api.setOwnershipNotOwned(claim.run_id);
  let fetches = 0, models = 0, captures = 0;
  git.ensureClone = async () => { fetches++; throw new Error("must not refresh"); };
  git.recordRecoveryCapture = async () => { captures++; throw new Error("must not capture"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(readJournal(bare, clone.branch).recovery.attempts, 1);
  assert.equal(fetches, 0);
  assert.equal(models, 0);
  assert.equal(captures, 0);
  assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), before);
});

it("corrupt owned journal fails with custody while preserving the exact corrupt bytes", async () => {
  const { claim, bare, clone } = await seed();
  const corrupt = '{"runId":"broken"';
  command(bare, "config", `uzi-recovery.${clone.branch}.clone`, corrupt);
  let models = 0, fetches = 0;
  git.ensureClone = async () => { fetches++; throw new Error("must not fetch"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }), gitlab).execute(claim);
  assert.equal(models, 0);
  assert.equal(fetches, 0);
  assert.equal(command(bare, "config", `uzi-recovery.${clone.branch}.clone`), corrupt);
  assert.ok(api.states.some(s => s.body.status === "failed"));
  assert.ok(fs.existsSync(clone.path));
});

it("missing-source blocker persistence failure still reports retained terminal failure", async () => {
  const { claim, bare, clone } = await seed();
  const before = readJournal(bare, clone.branch);
  fs.renameSync(clone.path, clone.path + ".saved");
  git.blockRecoveryEpisode = async () => { throw new Error("disk write refused"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { throw new Error("must not model"); }), gitlab).execute(claim);
  assert.deepEqual(readJournal(bare, clone.branch), before);
  assert.ok(api.states.some(s => s.body.status === "failed"));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});

it("ownership read outages consume all three durable attempts without source mutation or model admission", async () => {
  const { claim, bare, clone } = await seed();
  let models = 0, fetches = 0;
  client.getRunOwnership = async () => { throw new Error("ownership read connection reset"); };
  git.ensureClone = async () => { fetches++; throw new Error("must not fetch"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(models, 0);
  assert.equal(fetches, 0);
  const exhausted = readJournal(bare, clone.branch).recovery;
  assert.equal(exhausted.attempts, 3);
  assert.equal(exhausted.stage, "blocked");
  assert.equal(exhausted.blocker, "budget_exhausted");
  assert.ok(fs.existsSync(clone.path));
  assert.equal(api.states.some(s => s.body.status === "failed"), true,
    "the fenced terminal endpoint ends exhaustion even while ownership reads are unavailable");
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});

it("persisted deadline interrupts a blocked ownership read and keeps the charged budget", async () => {
  const { claim, bare, clone, key } = await seed();
  await git.reserveRecoveryIteration(bare, clone.branch, key, { runId: claim.run_id, clonePath: clone.path }, 1);
  const old = readJournal(bare, clone.branch);
  const startedAt = Date.now() - 299_700;
  old.recovery.startedAt = startedAt;
  old.recovery.deadline = startedAt + 300_000;
  command(bare, "config", `uzi-recovery.${clone.branch}.clone`, JSON.stringify(old));
  claim.inventory_guarded = true;
  const feature = client.hasFeature.bind(client);
  client.hasFeature = name => name === "recovery_inventory_v1" || feature(name);
  const outbox = new Outbox({
    root: path.join(fx.dataDir, "deadline-outbox"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000,
  });
  await outbox.init();
  let reads = 0, interrupted = false, models = 0, lateReads = 0, lateWork = 0, releases = 0;
  const authority = client.hasRecoveryRetirementAuthority.bind(client);
  client.hasRecoveryRetirementAuthority = async (...args) => {
    if (Date.now() >= old.recovery.deadline) lateWork++;
    return authority(...args);
  };
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
  const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    fakeGitlab().gitlab, "fixture-journal-key", nullLogger(), { recoveryRetryMs: 1, outbox });
  for (const method of ["quiesceRun", "transferRestorePointToTrustedBare"] as const) {
    const original = (r as any)[method].bind(r);
    (r as any)[method] = async (...args: unknown[]) => {
      if (Date.now() >= old.recovery.deadline) lateWork++;
      return original(...args);
    };
  }
  const report = client.reportState.bind(client);
  client.reportState = async (...args) => {
    if (args[1].status === "failed") {
      assert.equal(readJournal(bare, clone.branch).recovery.blocker, "budget_exhausted");
      assert.equal(readJournal(bare, clone.branch).recovery.stage, "blocked");
    }
    return report(...args);
  };
  client.getRunOwnership = async (_run, signal) => {
    if (Date.now() >= old.recovery.deadline) lateReads++;
    if (++reads !== 1) {
      await new Promise(resolve => setTimeout(resolve, 600));
      return { status: "running", claim_generation: 2 };
    }
    assert.ok(signal);
    await new Promise<void>((_resolve, reject) => {
      signal!.addEventListener("abort", () => { interrupted = true; reject(signal!.reason); }, { once: true });
      if (signal!.aborted) { interrupted = true; reject(signal!.reason); }
    });
    throw new Error("unreachable");
  };
  const timers = process.getActiveResourcesInfo().filter(resource => resource === "Timeout").length;
  await r.execute(claim);
  assert.ok(process.getActiveResourcesInfo().filter(resource => resource === "Timeout").length <= timers);
  assert.ok(await outbox.readTerminalJournal(claim.run_id, claim.claim_generation!));
  assert.equal(lateReads, 0);
  assert.equal(reads, 1);
  assert.equal(lateWork, 0);
  assert.equal(releases, 0);
  assert.equal(interrupted, true);
  assert.equal(models, 0);
  assert.equal(readJournal(bare, clone.branch).recovery.attempts, 2);
  assert.equal(readJournal(bare, clone.branch).recovery.blocker, "budget_exhausted");
  assert.ok(api.states.some(s => s.body.status === "failed"));
});

it("late blocker finalization cannot change a replacement episode on the same source", async () => {
  const { claim, bare, clone, key } = await seed();
  const expected = { runId: claim.run_id, clonePath: clone.path };
  const old = await git.reserveRecoveryIteration(bare, clone.branch, key, expected, 1);
  const replacement = readJournal(bare, clone.branch);
  replacement.recovery.startedAt += 1;
  replacement.recovery.deadline += 1;
  command(bare, "config", `uzi-recovery.${clone.branch}.clone`, JSON.stringify(replacement));
  await assert.rejects(git.blockRecoveryEpisode(bare, clone.branch, key, expected, "budget_exhausted", old),
    /recovery episode changed/);
  assert.deepEqual(readJournal(bare, clone.branch), replacement);
});

it("capture-closing executor is replaced before actual resumed model execution", async () => {
  const { claim, bare, clone } = await seed();
  let created = 0, models = 0, closed = false;
  const { gitlab } = fakeGitlab();
  const make: ExecutorFactory = runId => {
    const ordinal = ++created;
    return { homeDir: path.join(homeDir, runId), executor: {
      // A boundary-aware fake: retained capture closes admission permanently.
      settleForCredentialFreeCapture: async () => { closed = true; return { kind: "observed_empty" }; },
      sandboxesCommands: true,
      run: async ctx => {
        assert.equal(ordinal, 2);
        assert.ok(closed);
        models++;
        assert.notEqual(ctx.worktreePath, clone.path);
        throw new Error("model interruption");
      },
    } };
  };
  // The factory recreation condition also covers capture-capable executors without a live safety epoch.
  await runnerWith(make, gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(created, 2);
  assert.equal(models, 1);
  assert.equal(readJournal(bare, clone.branch).recovery.stage, "ready-for-model");
});

for (const rejects of [false, true]) {
  it(`unclean predecessor disposal after adoption blocks both sources (rejects=${rejects})`, async () => {
    const { claim, bare, clone } = await seed();
    let created = 0, models = 0, disposed = 0;
    const make: ExecutorFactory = runId => {
      created++;
      return { homeDir: path.join(homeDir, runId), executor: {
        sandboxesCommands: true,
        settleForCredentialFreeCapture: async () => ({ kind: "observed_empty" }),
        safety: {
          kind: "codex",
          withBoundary: async (request, action) => {
            const controller = new AbortController();
            const timer = setTimeout(() => controller.abort(new Error("fixture boundary expired")), request.deadlineMs);
            try {
              return await action({ epoch: 1, boundary: request.boundary, signal: controller.signal } as any);
            } finally { clearTimeout(timer); }
          },
          spawnBoundaryProcess: async (_permit, request) => {
            const [executable, ...args] = request.argv;
            const child = spawn(executable!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
            const completed = new Promise<{ code: number }>((resolve, reject) => {
              child.once("error", reject);
              child.once("exit", code => resolve({ code: code ?? 1 }));
            });
            return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
              cancel: async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; },
              completed };
          },
          dispose: async () => {
            disposed++;
            if (rejects) throw new Error("disposal rejected");
            return { kind: "incomplete", errors: [{ category: "protocol", message: "fixture undrained" }] };
          },
        },
        run: async () => { models++; throw new Error("must not model"); },
      } };
    };
    const timers = process.getActiveResourcesInfo().filter(resource => resource === "Timeout").length;
    await runnerWith(make, fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
    assert.ok(process.getActiveResourcesInfo().filter(resource => resource === "Timeout").length <= timers);
    assert.equal(created, 1);
    assert.equal(models, 0);
    assert.ok(disposed > 0);
    const journal = readJournal(bare, clone.branch);
    assert.notEqual(journal.clonePath, clone.path);
    assert.equal(journal.recovery.stage, "blocked");
    assert.equal(journal.recovery.blocker, "quiescence_failed");
    assert.equal(journal.recovery.attempts, 1);
    assert.ok(journal.retainedSources.some((s: any) => s.clonePath === clone.path));
    for (const source of [clone.path, journal.clonePath]) {
      assert.ok(fs.existsSync(source));
      assert.equal(fs.readFileSync(path.join(source, "retained.txt"), "utf8"), "only local dirty work\n");
    }
    assert.ok(api.states.some(s => s.body.status === "failed"));
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  });
}

it("verified guarded thin fallback permits local adoption despite unknown upload and keeps every source", async () => {
  const { claim, bare, clone } = await seed(2512, true);
  claim.inventory_guarded = true;
  const feature = client.hasFeature.bind(client);
  client.hasFeature = name => name === "recovery_inventory_v1" || feature(name);
  client.getRunOwnership = async () => ({ status: "running", claim_generation: 2, inventory_guarded: true });
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
    hold_id: "fixture-hold", generation: 2, inventory_guarded: true, has_available_capture: false,
  }] });
  client.reserveRecoveryCapture = async () => ({ capture_id: "00000000-0000-4000-8000-000000002512", state: "preparing" });
  client.uploadRecoveryBundle = async (_run, _capture, manifest, stream) => {
    for await (const chunk of stream) assert.ok(chunk.length);
    assert.ok(manifest.prerequisite_shas!.length > 0);
    throw new Error("upload response unknown");
  };
  client.getRecoveryCaptureStatus = async () => { throw new Error("status unavailable"); };
  let releases = 0, models = 0, productions = 0, publications = 0;
  client.publishCheckpoint = async (_run, _tip, stream) => {
    publications++;
    for await (const chunk of stream) assert.ok(chunk.length);
    throw new Error("checkpoint publication response unknown");
  };
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("must keep custody"); };
  const produce = git.produceRecoveryBundle.bind(git);
  git.produceRecoveryBundle = async (p, opts) => {
    productions++;
    // Lower only the cap: the real guarded self-contained -> thin producer still runs.
    return produce(p, { ...opts, maxBytes: 8 * 1024 });
  };
  const { gitlab } = fakeGitlab();
  const log = recordingLogger();
  const r = runnerWith(factory(async ctx => {
    models++;
    assert.equal(productions, 1, JSON.stringify(log.lines));
    assert.equal(readJournal(bare, clone.branch).recovery.attempts, 1);
    assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "retained.txt"), "utf8"), "only local dirty work\n");
    assert.notEqual(ctx.worktreePath, clone.path);
    await ctx.onModelTurnSettled!(1);
    throw new Error("fixture model stops");
  }), gitlab, "fixture-journal-key", log.logger, { recoveryRetryMs: 1 });
  await r.execute(claim);
  assert.equal(models, 1, JSON.stringify({ logs: log.lines, states: api.states, journal: readJournal(bare, clone.branch), records: await r.snapshotBootRecoveries() }));
  // The model interruption can create another terminal archive after model admission.
  assert.ok(productions >= 1);
  assert.ok(publications > 0);
  assert.equal(releases, 0);
  const records = await r.snapshotBootRecoveries();
  const thin = records.find(record => record.selfContained === false && record.prerequisiteShas!.length > 0);
  assert.ok(thin);
  const localProof = (r as unknown as { recovery: { verifiedLocalCapture: (
    record: typeof thin, signal?: AbortSignal,
  ) => Promise<unknown> } }).recovery;
  assert.equal(await localProof.verifiedLocalCapture(thin, AbortSignal.abort()), undefined,
    "cancelled local integrity proof cannot grant adoption authority");
  assert.ok(fs.existsSync(thin.bundlePath!), "cancellation retains the archive bytes");
  const journal = readJournal(bare, clone.branch);
  assert.equal(journal.recovery, undefined);
  assert.ok(fs.existsSync(clone.path));
  assert.ok(fs.existsSync(journal.clonePath));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});

it("real production declared overflow blocks runner with decoded_history_limit and retains source custody", async () => {
  const { claim, bare, clone } = await seed();
  claim.inventory_guarded = false;
  let models = 0, releases = 0, proofs = 0;
  const log = recordingLogger();
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
  const record = git.recordRecoveryCapture.bind(git);
  git.recordRecoveryCapture = async (...args) => {
    proofs++;
    const tip = args[5];
    const oid = command(bare, "rev-parse", tip + ":retained.txt");
    const objectPath = path.join(bare, "objects", oid.slice(0, 2), oid.slice(2));
    fs.mkdirSync(path.dirname(objectPath), { recursive: true });
    if (fs.existsSync(objectPath)) fs.chmodSync(objectPath, 0o600);
    // Capture is complete; tamper before the real metadata proof, without allocating the declared body.
    fs.writeFileSync(objectPath, deflateSync(Buffer.from("blob 1073741825\0")));
    assert.equal(command(bare, "cat-file", "-s", oid), "1073741825");
    return record(...args);
  };
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(proofs, 1);
  assert.equal(models, 0);
  assert.equal(releases, 0);
  const journal = readJournal(bare, clone.branch);
  assert.equal(journal.clonePath, clone.path);
  assert.equal(journal.recovery.stage, "blocked");
  assert.equal(journal.recovery.blocker, "decoded_history_limit");
  assert.equal(journal.recovery.attempts, 1);
  assert.equal(journal.recovery.restoreTip, undefined);
  assert.equal(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-recovery-episode/" + claim.run_id), "");
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  assert.ok(api.states.some(s => s.body.status === "failed" &&
    s.body.failure_reason?.includes("1 GiB decoded verification limit")));
  assert.ok(log.lines.some(line => JSON.stringify(line).includes("1 GiB decoded verification limit")));
});

for (const failure of ["cap", "timeout", "interruption"] as const) {
  it("closure verification " + failure + " keeps all source custody and never admits a model", async () => {
    const { claim, bare, clone, key } = await seed();
    let releases = 0, models = 0, verified = 0;
    const log = recordingLogger();
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain"); };
    const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
      fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 });
    git.verifyRecoveryClosure = async () => {
      verified++;
      if (failure === "cap") throw new RecoveryClosureLimitError();
      if (failure === "interruption") {
        (r as any).activeRuns.get(claim.run_id).cancel.abort();
        const scope = (git as any).recoveryOperations.getStore();
        scope.signal.throwIfAborted();
        throw new Error("fixture interruption");
      }
      // A persisted original deadline bounds this blocked verifier, without refreshing it.
      const scope = (git as any).recoveryOperations.getStore();
      await new Promise<void>((_resolve, reject) => {
        const aborted = () => reject(scope.signal.reason);
        scope.signal.addEventListener("abort", aborted, { once: true });
        if (scope.signal.aborted) aborted();
      });
      return false;
    };
    if (failure === "timeout") {
      await git.reserveRecoveryIteration(bare, clone.branch, key, { runId: claim.run_id, clonePath: clone.path }, 1);
      const before = readJournal(bare, clone.branch);
      before.recovery.startedAt = Date.now() - 297_000;
      before.recovery.deadline = before.recovery.startedAt + 300_000;
      command(bare, "config", `uzi-recovery.${clone.branch}.clone`, JSON.stringify(before));
    }
    await r.execute(claim);
    assert.equal(verified, 1);
    assert.equal(models, 0);
    assert.equal(releases, 0);
    const journal = readJournal(bare, clone.branch);
    assert.equal(journal.clonePath, clone.path);
    assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
    assert.equal(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-recovery-episode/" + claim.run_id), "");
    assert.equal(journal.recovery.restoreTip, undefined);
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
    if (failure === "interruption") {
      assert.equal(journal.recovery.stage, "capturing");
    } else {
      assert.equal(journal.recovery.stage, "blocked");
      assert.equal(journal.recovery.blocker, failure === "cap" ? "decoded_history_limit" : "budget_exhausted");
      assert.ok(api.states.some(s => s.body.status === "failed"));
    }
    if (failure === "cap") {
      assert.ok(api.states.some(s => s.body.failure_reason?.includes("1 GiB decoded verification limit")));
      assert.ok(log.lines.some(line => JSON.stringify(line).includes("1 GiB decoded verification limit")));
      assert.equal(journal.recovery.attempts, 1);
    }
  });
}
