import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import type { ExecutorFactory } from "../src/runner.js";
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
function factory(run: (ctx: import("../src/executor.js").RunContext) => Promise<never>): ExecutorFactory {
  return runId => ({ homeDir: path.join(homeDir, runId), executor: { run } });
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
  assert.equal(readJournal(bare, clone.branch).recovery.attempts, 3);
  assert.ok(fs.existsSync(clone.path));
  assert.equal(api.states.some(s => s.body.status === "failed"), false);
});

it("persisted deadline interrupts a blocked ownership read and keeps the charged budget", async () => {
  const { claim, bare, clone, key } = await seed();
  await git.reserveRecoveryIteration(bare, clone.branch, key, { runId: claim.run_id, clonePath: clone.path }, 1);
  const old = readJournal(bare, clone.branch);
  const startedAt = Date.now() - 299_700;
  old.recovery.startedAt = startedAt;
  old.recovery.deadline = startedAt + 300_000;
  command(bare, "config", `uzi-recovery.${clone.branch}.clone`, JSON.stringify(old));
  let reads = 0, interrupted = false, models = 0;
  client.getRunOwnership = async (_run, signal) => {
    if (++reads !== 1) return { status: "running", claim_generation: 2 };
    assert.ok(signal);
    await new Promise<void>((_resolve, reject) => {
      signal!.addEventListener("abort", () => { interrupted = true; reject(signal!.reason); }, { once: true });
      if (signal!.aborted) { interrupted = true; reject(signal!.reason); }
    });
    throw new Error("unreachable");
  };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(interrupted, true);
  assert.equal(models, 0);
  assert.equal(readJournal(bare, clone.branch).recovery.attempts, 2);
  assert.equal(readJournal(bare, clone.branch).recovery.blocker, "budget_exhausted");
  assert.ok(api.states.some(s => s.body.status === "failed"));
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
  assert.ok(records.some(record => record.selfContained === false && record.prerequisiteShas!.length > 0));
  const journal = readJournal(bare, clone.branch);
  assert.equal(journal.recovery, undefined);
  assert.ok(fs.existsSync(clone.path));
  assert.ok(fs.existsSync(journal.clonePath));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});
