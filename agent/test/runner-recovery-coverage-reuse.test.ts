import "./setup/hermetic-proc.js";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { after, afterEach, before, describe, test } from "node:test";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { GitCache, formatRetainedRecoveryDiagnostic, type PositiveOwedCandidateContext } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import type { RecoveryRecord } from "../src/recovery.js";
import { StubExecutor } from "../src/executor.js";
import { WorkerClient } from "../src/client.js";
import { nullLogger, noProofReseed, makeClaim, testGitCacheOptions } from "./helpers.js";
import { FakeRecoveryClient, makeRecoveryCoordinator } from "./codex-reap-fixture.js";
import { commitInTree } from "./runner-codex-sinks-fixture.js";
import { fakeGitlab } from "./runner-harness.js";
import { FakeApi } from "./fake-api.js";
import { makeFixture } from "./fixture-repo.js";
import { fixtureFetchTracking, fixtureTrackingOptions } from "./runner-tracking-fixture.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";

function command(dir: string, ...args: string[]) {
  return execFileSync("git", ["--no-replace-objects", "-C", dir, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}

for (const legacy of [false, true]) describe(legacy ? "legacy ninth-wrapper owner Resume" : "repeated clean provider-outage recovery", { concurrency: false }, () => {
  let execute: () => Promise<any>, cleanup: () => Promise<void>, enableReuse: () => void;
  let claim: ReturnType<typeof makeClaim>, bare: string, physicalHead: string;
  let assertPublished: () => void;
  const captures: Array<Readonly<{ record: string; wrapper: string; current: string | undefined; roots: readonly string[] }>> = [];
  const lifecycle: string[] = [];
  let uploads = 0, preparations = 0, prepared = 0;
  let observingRetained = false;
  let models = 0, healthy = false;
  let activeRunner: RunRunner | undefined, activeExecution: Promise<void> | undefined;
  const reports: any[] = [], diagnostics: any[] = [], captureSources: string[] = [];
  before(async () => {
    const token = "outage-local-fixture-token";
    const api = new FakeApi(token), url = await api.listen(), fx = makeFixture();
    const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    const home = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "recovery-coverage-home-"));
    const client = new WorkerClient(url, token, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
    claim = makeClaim({
      issue_iid: 3527, issue_title: "coverage recovery",
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
      claim_generation: legacy ? 9 : 1, worker_recovery_episode: 0, worker_recovery_owner_episode: 0, inventory_guarded: true,
    });
    bare = await git.ensureClone(fx.originPath);
    fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
    const clone = await git.createOrAttachRunnerClone(bare, 3527, noProofReseed, claim.run_id);
    commitInTree(clone.path, "RETAINED.txt", "work before outage\n");
    await fixtureFetchTracking(git, bare, clone.path, clone.branch, claim.run_id);
    const originalHead = command(clone.path, "rev-parse", "HEAD"), tree = command(clone.path, "rev-parse", "HEAD^{tree}");
    physicalHead = originalHead;
    if (legacy) {
      const options = await fixtureTrackingOptions(git, bare, clone.branch, claim.run_id);
      for (let generation = 1; generation <= 8; generation++)
        physicalHead = (await git.buildRecoveryCoverage(bare, { ...options.context, generation } as PositiveOwedCandidateContext, [originalHead], physicalHead)).sha;
      command(clone.path, "fetch", bare, physicalHead);
      command(clone.path, "reset", "--hard", physicalHead);
      await fixtureFetchTracking(git, bare, clone.path, clone.branch, claim.run_id, 9);
    }
    await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
    client.protocolFeatures = ["claim_generation_fence", "recovery_inventory_v1", "recovery_cause_provider_outage",
      "recovery_cause_retained_recovery_exhausted", "recovery_cause_retained_recovery_adoption_failed"];
    class Archive extends FakeRecoveryClient {
      hasFeature(name: string) { return name === "recovery_inventory_v1"; }
      async getRunOwnership(runId: string) { return client.getRunOwnership(runId); }
      override async reserveRecoveryCapture(_runId: string, req: any): Promise<any> {
        this.reserveCalls.push(req); return { capture_id: randomUUID(), state: "preparing" };
      }
    }
    const archive = new Archive();
    archive.holds = Array.from({ length: 30 }, (_, i) => ({ generation: i + 1, inventory_guarded: true, hold_id: randomUUID() })) as never;
    const { coord, root } = makeRecoveryCoordinator(archive, git);
    cleanup = async () => {
      try { await api.close(); } finally {
        fx.cleanup(); fs.rmSync(home, { recursive: true, force: true }); fs.rmSync(root, { recursive: true, force: true });
      }
    };
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      reports.push(body);
      if (body.status === "recovery_wait") {
        api.setOwnershipStatus(runId, "recovery_wait", claim.claim_generation, true);
        return { applied: true, status: body.status, recoveryCause: body.recovery_cause };
      }
      return report(runId, body, signal);
    };
    const build = git.buildRecoveryCoverage.bind(git);
    let oldProducer = legacy;
    git.buildRecoveryCoverage = (...args) => build(args[0], args[1], args[2], args[3], oldProducer ? {} : args[4]);
    enableReuse = () => { oldProducer = false; };
    const freeze = coord.freezeInventory.bind(coord);
    coord.freezeInventory = async (...args) => {
      const record = await freeze(...args);
      if (record && observingRetained) {
        const roots = Object.freeze([...new Set([
          ...record.originalRoots!.map(root => root.sha), record.originalSourceSha!,
        ])].sort());
        captures.push(Object.freeze({
          record: JSON.stringify(record), wrapper: record.sourceSha, current: record.inventoryCurrentSha, roots,
        }));
        lifecycle.push("freeze");
        assert.ok(roots.length > 0, "the actual frozen inventory has retained roots");
        for (const root of roots) command(bare, "merge-base", "--is-ancestor", root, record.sourceSha);
        if (!legacy) {
          assert.equal(record.inventoryCurrentSha, originalHead);
          assert.notEqual(record.sourceSha, originalHead, "the actual capture W is distinct from S");
          assert.deepEqual(roots, captures[0]!.roots, "no-progress captures keep the same root set");
        }
      }
      return record;
    };
    const capture = coord.captureAndUpload.bind(coord);
    coord.captureAndUpload = async (...args) => {
      if (!observingRetained) return capture(...args);
      const frozen = captures.at(-1);
      assert.ok(frozen);
      const frozenRecord: RecoveryRecord = JSON.parse(frozen.record);
      assert.deepEqual(args[0].record, frozenRecord, "upload receives the actual immutable freeze snapshot");
      assert.equal(args[0].record.sourceSha, frozen.wrapper);
      assert.equal(args[0].record.inventoryCurrentSha, frozen.current);
      const before = archive.uploadCalls.length;
      const outcome = await capture(...args);
      const uploaded = archive.uploadCalls.length - before;
      assert.equal(uploaded, 1, "each actual capture uploads one bundle");
      assert.equal(outcome.state, "uploaded");
      uploads += uploaded;
      lifecycle.push("upload");
      return outcome;
    };
    assertPublished = () => {
      const published = command(fx.originPath, "rev-parse", `refs/heads/${clone.branch}`);
      assert.notEqual(published, originalHead, "the ordinary finalizer publishes new real work");
      assert.equal(command(fx.originPath, "show", `${published}:RETAINED.txt`), "work before outage");
      assert.equal(command(fx.originPath, "show", `${published}:HEALTHY.txt`), "work after recovery");
      command(fx.originPath, "merge-base", "--is-ancestor", originalHead, published);
      if (!legacy) {
        const ancestors = new Set(command(fx.originPath, "rev-list", published).split("\n"));
        for (const { wrapper } of captures) assert.equal(ancestors.has(wrapper), false, "published ancestry excludes observed recovery envelopes");
        assert.equal(command(fx.originPath, "log", "--format=%s", published).split("\n")
          .some(subject => subject.startsWith("uzi recovery coverage ")), false);
      }
      // Publication can independently release owed pins; preservation is checked at preparation.
      for (const { wrapper, roots } of captures) {
        command(bare, "cat-file", "-e", `${wrapper}^{commit}`);
        for (const root of roots) command(bare, "merge-base", "--is-ancestor", root, wrapper);
      }
    };
    const prepare = git.prepareRecoverySuccessor.bind(git);
    git.prepareRecoverySuccessor = async (...args) => {
      const refs = command(bare, "for-each-ref", "--format=%(refname) %(objectname)");
      const candidates = structuredClone(await git.enumerateOwedCandidates(bare, claim.run_id));
      const inventory = structuredClone(await coord.inspect(claim.run_id));
      const sourceHead = command(args[3].clonePath, "rev-parse", "HEAD");
      preparations++;
      try {
        const successor = await prepare(...args);
        prepared++;
        lifecycle.push("prepare");
        if (!legacy) {
          assert.equal(command(successor.path, "rev-parse", "HEAD"), originalHead);
          assert.equal(command(successor.path, "write-tree"), tree);
          assert.equal(command(successor.path, "status", "--porcelain"), "");
          assert.equal(successor.adoptedContentSource, originalHead);
          assert.equal(command(successor.path, "log", "--format=%s").split("\n")
            .some(subject => subject.startsWith("uzi recovery coverage ")), false);
        }
        return successor;
      }
      catch (error) { diagnostics.push(formatRetainedRecoveryDiagnostic(error, s => s)); throw error; }
      finally {
        assert.equal(command(bare, "for-each-ref", "--format=%(refname) %(objectname)"), refs, "preparation preserves every ref");
        assert.deepEqual(await git.enumerateOwedCandidates(bare, claim.run_id), candidates, "preparation preserves owed candidates");
        assert.deepEqual(await coord.inspect(claim.run_id), inventory, "preparation preserves archive custody");
        assert.ok(fs.existsSync(args[3].clonePath), "the retained source path survives preparation");
        assert.equal(command(args[3].clonePath, "rev-parse", "HEAD"), sourceHead);
      }
    };
    const record = git.recordRecoveryCapture.bind(git);
    git.recordRecoveryCapture = async (...args) => {
      if (args[6]) captureSources.push(args[6]);
      const result = await record(...args);
      lifecycle.push("record");
      assert.equal(args[5], captures.at(-1)!.wrapper, "record the actual uploaded capture W");
      assert.equal(args[6], captures.at(-1)!.current, "record its actual frozen current source");
      if (!legacy) assert.equal(args[6], originalHead);
      return result;
    };
    execute = async () => {
      const before = models, offset = reports.length, eventOffset = lifecycle.length;
      const capturesBefore = captures.length, uploadsBefore = uploads, preparationsBefore = preparations, preparedBefore = prepared;
      observingRetained = true;
      api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation, true);
      const log = nullLogger();
      const runner = new RunRunner(client, git, runId => ({
        homeDir: path.join(home, runId), executor: { run: async ctx => {
          observingRetained = false;
          models++;
          assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "RETAINED.txt"), "utf8"), "work before outage\n");
          command(ctx.worktreePath, "merge-base", "--is-ancestor", originalHead, "HEAD");
          if (!healthy) {
            assert.equal(command(ctx.worktreePath, "status", "--porcelain"), "", "no progress leaves no WIP to capture");
            throw new TransientRecoveryError("simulated provider outage", { providerOrigin: true });
          }
          commitInTree(ctx.worktreePath, "HEALTHY.txt", "work after recovery\n");
          await ctx.onModelTurnSettled?.(1);
          return new StubExecutor(log).run(ctx);
        } },
      }), log, 20, undefined, { pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, prDescriptionHeadLagMs: 0,
        gitlab: fakeGitlab().gitlab, teardownTestDeps: portableTeardownTestDeps, recovery: coord, recoveryRetryMs: 1,
        checkpointTestHooks: { retryBackoffMs: [0, 0] } });
      activeRunner = runner;
      activeExecution = runner.execute(claim);
      try { await activeExecution; }
      finally { activeRunner = undefined; activeExecution = undefined; }
      const states = reports.slice(offset);
      const last = states.filter(r => ["recovery_wait", "failed", "paused", "completed"].includes(r.status)).at(-1);
      if (!healthy) {
        const tip = JSON.parse(command(bare, "config", `uzi-recovery.${clone.branch}.clone`)).restoreTip;
        assert.equal(command(bare, "rev-parse", tip + "^{tree}"), tree);
        const subjects = command(bare, "log", "--format=%s", tip).split("\n");
        assert.equal(subjects.some(s => s.startsWith("wip(park):")), false);
        if (!legacy) assert.equal(subjects.filter(s => s.startsWith("uzi recovery coverage ")).length, 1);
      }
      const events = lifecycle.slice(eventOffset);
      const count = captures.length - capturesBefore;
      assert.equal(uploads - uploadsBefore, count);
      assert.equal(captureSources.length, captures.length);
      assert.equal(preparations - preparationsBefore, count);
      if (!legacy) {
        assert.equal(prepared - preparedBefore, count);
        assert.deepEqual(events, Array.from({ length: count }, () => ["freeze", "upload", "record", "prepare"]).flat());
        if (last?.recovery_cause === "retained_recovery_exhausted") {
          assert.equal(count, 0, "budget exhaustion alone does not count as another capture");
          assert.equal(models, before);
        }
      }
      return { last, states, entries: models - before, captures: count, uploads: uploads - uploadsBefore };
    };
  });
  afterEach(async () => {
    const pending = activeExecution;
    activeRunner?.shutdown();
    await pending;
  });
  after(async () => { await cleanup?.(); });

  if (!legacy) {
    for (let generation = 1; generation <= 11; generation++) test(`generation ${generation} remains adoptable without progress`, async t => {
      claim.claim_generation = generation;
      const result = await execute();
      t.diagnostic(JSON.stringify({ generation, models, cause: result.last?.recovery_cause, captures: result.captures, uploads: result.uploads }));
      assert.equal(result.last?.status, "recovery_wait");
      assert.notEqual(result.last?.recovery_cause, "retained_recovery_adoption_failed");
      if (result.last?.recovery_cause === "retained_recovery_exhausted") claim.worker_recovery_episode!++;
    });
    test("restored provider completes after nine no-progress cycles", async () => {
      assert.equal(models, 9);
      healthy = true;
      claim.claim_generation = 12;
      let result = await execute();
      if (result.last?.recovery_cause === "retained_recovery_exhausted") {
        claim.worker_recovery_episode!++; claim.claim_generation = 13;
        result = await execute();
      }
      assert.equal(result.entries, 1);
      assert.equal(result.last?.status, "completed", JSON.stringify(reports));
      assert.equal(reports.some(r => r.status === "failed"), false);
      assert.deepEqual(diagnostics, []);
      assertPublished();
      assert.ok(captures.length >= 9, "the bounded proof observes actual repeated captures");
    });
  } else {
    test("genuine old production aggregates establish the ninth-wrapper adoption hold", async () => {
      const result = await execute();
      assert.equal(models, 0);
      assert.equal(result.last?.recovery_cause, "retained_recovery_adoption_failed");
      assert.equal(diagnostics.at(-1)?.diagnostic_code, "retained_successor_wrapper_aggregate_bound");
      assert.equal(captureSources.at(-1), physicalHead);
    });
    test("owner Resume reuses the physical eight-deep source and completes", async () => {
      enableReuse(); healthy = true;
      claim.claim_generation = 10; claim.worker_recovery_episode = 1; claim.worker_recovery_owner_episode = 1;
      const result = await execute();
      assert.equal(captureSources.at(-1), physicalHead, "Resume captures the last adopted physical HEAD, not the blocked ninth wrapper");
      assert.equal(result.entries, 1);
      assert.equal(result.last?.status, "completed", JSON.stringify(reports));
      assert.equal(reports.some(r => r.status === "failed"), false);
      const owner = JSON.parse(command(bare, "config", `uzi-recoveryowner.${claim.run_id}.record`));
      assert.equal(owner.consumed, 1);
      assert.equal(diagnostics.length, 1, "the fixed Resume adds no verifier refusal");
      assert.equal(preparations, 2);
      assert.equal(prepared, 1);
      assertPublished();
    });
  }
});
