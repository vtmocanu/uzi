import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { DataVolumeGuard } from "../src/disk-full.js";
import { WorkerClient } from "../src/client.js";
import { SteeringChannel } from "../src/steering.js";
import { rmTreePinned } from "../src/rmtree.js";
import type { Executor } from "../src/executor.js";
import { MaintenanceResources, type MaintenanceResource } from "../src/recovery-maintenance.js";
import { canonicalJson, RecoveryCoordinator, TERMINAL_ORIGIN_ADMISSION_ENABLED } from "../src/recovery.js";
import { recordingLogger, nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, deferred, runnerWith } from "./runner-harness.js";
import { passingExclusion } from "./maintenance-exclusion-fixture.js";
import { TickSpawner } from "../src/tick-spawner.js";
import { RecoveryMaintenance } from "../src/recovery-maintenance.js";

// A fresh source is observed only through the Linux-only quiescence / stopped-source proofs
// (src/runner.ts "non_linux_host" refusal, src/run-quiescence.ts), so it never qualifies on macOS (#1912).
// Linux runs every assertion.
const LINUX_ONLY = process.platform !== "linux"
  ? "needs the Linux-only run-quiescence / stopped-source proof (src/run-quiescence.ts, src/runner.ts refuse off Linux)"
  : false;

installHarness();

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}
type Observation = { terminal?: string; genesis: { runId: string; generation: number; clonePath: string; attemptId: string }; family?: unknown };
type Tails = { executionTails: ReadonlyMap<string, Promise<Observation | undefined>> };

for (const mode of ["normal", "origin-production-default", "origin-admission", "origin-already-present", "origin-pressure", "origin-source-limit",
  "origin-transfer-final-family", "origin-transfer-head", "origin-transfer-revoked", "origin-transfer-import-refused", "origin-transfer-layout-refused", "origin-transfer-post-import-range", "origin-transfer-missing", "notification-throw", "notification-reject", "unapplied", "lost", "stale", "wrong-status", "missing-status",
  "missing-process", "missing-reproof", "unverified", "failed-reap", "late-fence",
  "failed-home", "failed-skills", "failed-owed", "record-none", "prior-record-none", "reclaimed", "nonmatching-error", "ordinary-producer-shaped-error", "post-run-error"] as const) {
it(`#3031 fresh ordinary FAILED unsaved source: ${mode}`, { skip: mode.startsWith("origin-transfer-") ||
  ["origin-admission", "origin-already-present", "origin-pressure", "failed-reap"].includes(mode) ? LINUX_ONLY : false }, async (t) => {
  const originMode = mode.startsWith("origin-");
  const transferOrigin = mode === "origin-admission" || mode === "origin-already-present" || mode.startsWith("origin-transfer-");
  const dirtyOrigin = transferOrigin && mode !== "origin-already-present";
  const importRefused = mode === "origin-transfer-import-refused";
  const layoutRefused = mode === "origin-transfer-layout-refused";
  const postImportRefused = mode === "origin-transfer-post-import-range";
  const transferFails = importRefused || layoutRefused || postImportRefused;
  let retainedImportedPack = "", retainedImportedBytes: Buffer | undefined, unknownCanonicalEntry = "";
  const acceptedOutput = mode === "origin-admission" || mode === "origin-already-present";
  const expectedRPC = mode === "origin-already-present" ? 8 : layoutRefused ? 14 :
    ["origin-transfer-final-family", "origin-transfer-head", "origin-transfer-import-refused", "origin-transfer-post-import-range"].includes(mode) ? 16 : 18;
  let importPhase = false, importDispatched = false, ownerJoined = false, outputPairObserved = false, outputIndexCalls = 0, indexesBeforeJoin = 0;
  let captureRPC = -1;
  const semanticTrace: string[] = [];
  const transferResults: string[] = [];
  let transferCalls = 0, transferEntered = 0, transferBodyComplete = false, transferSettled = false;
  let capturedSignal: AbortSignal | undefined;
  let pressure = false;
  // Production default: the fixed guard keeps new-origin admission unreachable. Every other
  // origin mode exercises the dormant admission path behind a test-only substitution.
  const productionDefault = mode === "origin-production-default";
  if (originMode && !productionDefault)
    t.mock.method(RecoveryCoordinator.prototype as unknown as { terminalOriginAdmissionEnabled(): boolean },
      "terminalOriginAdmissionEnabled", () => true);
  // Modeled trusted ancestor DAC only: the harness lives below host /tmp.
  // Descendant identities, ownership, bytes and Git execution remain real.
  const ancestors = new Set<string>();
  for (let p = path.dirname(git.recoveryRoot);; p = path.dirname(p)) {
    ancestors.add(p);
    if (p === path.dirname(p)) break;
  }
  const descriptors = new Map<number, string>();
  const physical = (p: unknown): string => {
    if (typeof p !== "string") return "";
    const m = /^\/proc\/self\/fd\/([0-9]+)(?:\/(.*))?$/.exec(p);
    return m && descriptors.has(Number(m[1])) ? path.join(descriptors.get(Number(m[1]))!, m[2] ?? "") : p;
  };
  const project = <T extends { mode: number | bigint; uid: number | bigint; gid: number | bigint }>(p: string, s: T): T => {
    if (ancestors.has(p)) {
      const big = typeof s.mode === "bigint";
      s.mode = big ? BigInt(Number(s.mode) & ~0o7022) : Number(s.mode) & ~0o7022;
      s.uid = big ? BigInt(process.getuid!()) : process.getuid!();
      s.gid = big ? BigInt(process.getgid!()) : process.getgid!();
    }
    return s;
  };
  if (transferOrigin) {
    // This harness disables uid split; create genuinely private worker-owned dirs.
    const umask = process.umask(0o077);
    t.after(() => { process.umask(umask); });
    const lstat = fs.lstat.bind(fs), open = fs.open.bind(fs);
    t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) =>
      project(physical(args[0]), await lstat(...args)));
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args), p = physical(args[0]);
      descriptors.set(file.fd, p);
      const stat = file.stat.bind(file);
      t.mock.method(file, "stat", async (...args: Parameters<typeof file.stat>) => project(p, (await stat(...args))!));
      return file;
    });
  }
  let boundaryEntered = 0;
  const exclusion = passingExclusion();
  const guard = new DataVolumeGuard({ dataDir: path.dirname(git.recoveryRoot) });
  t.mock.method(guard, "preflight", () => pressure ? "data_volume_full" : "not_disk_full");
  const claim = gitlabClaim(3031, { claim_generation: 1, inventory_guarded: true });
  const feature = client.hasFeature.bind(client);
  t.mock.method(client, "hasFeature", (name: Parameters<typeof client.hasFeature>[0]) =>
    name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name));
  t.mock.method(client, "listRecoveryHolds", async () => ({ run_id: claim.run_id, holds: [{ hold_id: "fixture-hold", generation: 1, inventory_guarded: true, has_available_capture: false }] }));
  let chargedTransports = 0, discoveryTransports = 0;
  const resourceTotals: Partial<Record<MaintenanceResource, number>> = {};
  if (originMode) {
    const consume = MaintenanceResources.consume.bind(MaintenanceResources);
    t.mock.method(MaintenanceResources, "consume", (resource: MaintenanceResource, amount: number) => {
      try { consume(resource, amount); }
      catch (error) {
        t.diagnostic(JSON.stringify({ refusedCharge: { resource, amount, used: resourceTotals[resource] ?? 0 },
          site: error instanceof Error ? error.stack?.split("\n").filter(s => s.includes("git.ts:")).slice(0, 4) : [] }));
        throw error;
      }
      resourceTotals[resource] = (resourceTotals[resource] ?? 0) + amount;
    });
    claim.repo.id = "00000000-0000-4000-8000-000000000033";
    const workerId = "00000000-0000-4000-8000-000000000034";
    const holdId = "00000000-0000-4000-8000-000000000032";
    // The production strict client validates the complete unique inventory and exact
    // terminal hold; the runner still produces its own private original observation.
    const strict = new WorkerClient("http://origin.test", "fixture-journal-key", "test", nullLogger(), {
      fetch: async input => {
        const url = new URL(String(input));
        if (url.pathname.endsWith("/register")) return Response.json({ worker_id: workerId, protocol_features: [] });
        if (MaintenanceResources.discoveryActive()) discoveryTransports++;
        else if (MaintenanceResources.active()) {
          chargedTransports++;
          semanticTrace.push(url.search ? "pair:exact" : "pair:list");
          if (transferBodyComplete) {
            outputPairObserved = true;
            assert.equal(transferSettled, true, "output pair follows returned transfer owner");
            assert.equal(ownerJoined, true, "actual child owner joined before output pair");
          }
        }
        return Response.json(url.search ? { run_id: claim.run_id, exact_hold: {
          run_id: claim.run_id, hold_id: holdId, generation: 1, repo_id: claim.repo.id,
          original_worker_id: workerId, original_worker_identity: "human-label", live_worker_id: workerId,
          live_run_id: claim.run_id, inventory_guarded: true, state: "open", run_status: "failed",
          current_claim_generation: 1, claim_released: true,
          ...((mode === "origin-transfer-revoked" && transferBodyComplete) || (importRefused && importPhase)
            ? { run_status: "stopped" } : {}),
        } } : { run_id: claim.run_id, holds: [{ hold_id: holdId, generation: 1,
          inventory_guarded: true, has_available_capture: false }] });
      },
    });
    await strict.register("worker", undefined, 1, undefined, ["recovery_held_publication_v2"]);
    t.mock.method(client, "observeTerminalMaintenanceHold", strict.observeTerminalMaintenanceHold.bind(strict));
    t.mock.method(client, "isCurrentTerminalMaintenanceHold", strict.isCurrentTerminalMaintenanceHold.bind(strict));
  }
  if (mode === "origin-source-limit") {
    let priorWork = false;
    const scan = git.matchMaintenanceSourceFamily.bind(git);
    t.mock.method(git, "matchMaintenanceSourceFamily", async (...args: Parameters<typeof scan>) => {
      if (MaintenanceResources.active() && !MaintenanceResources.discoveryActive() && !priorWork) {
        priorWork = true;
        MaintenanceResources.consumeIfActive("sources", 127);
      }
      return scan(...args);
    });
  }
  let attempts = 0, models = 0, source = "", branch = "", committed = "";
  const unique = randomBytes(16 * 1024).toString("hex");
  // Fail EVERY production attempt before any bundle bytes or immutable installation.
  t.mock.method(git, "produceRecoveryBundle", async () => {
    attempts++;
    throw new Error("fixture bundle production unavailable before installation");
  });
  const ackStarted = deferred(), releaseAck = deferred();
  if (mode === "late-fence") {
    const ack = client.ackInputs.bind(client);
    t.mock.method(client, "ackInputs", async (...args: Parameters<typeof ack>) => {
      ackStarted.resolve();
      await releaseAck.promise;
      return ack(...args);
    });
    const stop = SteeringChannel.prototype.stop;
    t.mock.method(SteeringChannel.prototype, "stop", async function (this: SteeringChannel) {
      api.setInputClaimGeneration(claim.run_id, 2);
      releaseAck.resolve();
      await stop.call(this);
      assert.equal(this.claimFence(), "stale");
    });
  }
  let terminalAcks = 0;
  const report = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
    const ack = await report(...args);
    if (args[1].status !== "failed") return ack;
    assert.equal(ack.applied, true, "actual server applied FAILED");
    assert.equal(ack.status, "failed");
    terminalAcks++;
    if (mode === "lost") throw new Error("lost failed ACK");
    if (mode === "unapplied") return { ...ack, applied: false };
    if (mode === "stale") return { ...ack, staleClaim: true };
    if (mode === "wrong-status") return { ...ack, status: "running" };
    if (mode === "missing-status") return { ...ack, status: undefined };
    return ack;
  });
  if (mode === "failed-owed") {
    const retain = git.retainCurrentOwedCandidate.bind(git);
    t.mock.method(git, "retainCurrentOwedCandidate", async (...args: Parameters<typeof retain>) => {
      // Leave transfer/freeze real; fail the final cleanup join only.
      if (terminalAcks) return { kind: "not_updated" as const, reason: "preservation_failed" as const };
      return retain(...args);
    });
  }
  const log = recordingLogger();
  const target = path.join(homeDir, claim.run_id);
  const runner = runnerWith(() => ({
    homeDir: target,
    executor: {
      run: async ctx => {
        models++;
        source = ctx.worktreePath;
        branch = ctx.branch;
        command(source, "config", "user.name", "fixture");
        command(source, "config", "user.email", "fixture@example.com");
        await fs.writeFile(path.join(source, "unique-commit"), unique);
        command(source, "add", "unique-commit");
        command(source, "commit", "-m", "unique local work");
        committed = command(source, "rev-parse", "HEAD");
        await fs.writeFile(path.join(source, "README.md"), "tracked " + unique);
        await fs.writeFile(path.join(source, "unique-untracked"), "untracked " + unique);
        await fs.mkdir(target, { recursive: true });
        if (mode === "late-fence") {
          api.setInputClaimGeneration(claim.run_id, 1);
          api.setInputs(claim.run_id, [{ id: 1, kind: "follow_up", body: "fixture" }]);
          await ackStarted.promise;
        }
        if (mode === "post-run-error") {
          ctx.codeCrossCheckSummary = async () => { throw new Error("post-execution summary failure"); };
          return { branch: ctx.branch };
        }
        throw new Error("ordinary executor failure");
      },
      reapAttributedProcesses: async () => ({ complete: mode !== "failed-reap", killed: [], left: [] }),
    },
  }), fakeGitlab().gitlab, "fixture-journal-key", log.logger, {
    maintenanceExclusion: exclusion,
    dockerHost: "http://unused.invalid", recoveryRetryMs: 1,
    ...(mode === "origin-pressure" ? { dataVolume: guard } : {}),
    ...(mode === "failed-home" || mode === "failed-skills" ? {
      teardownTestDeps: { removeTreePinned: async (parent, name, options) => {
        const isHome = path.join(parent, name) === target;
        const isSkills = name.startsWith(".uzi-skills-");
        if ((mode === "failed-home" && isHome) || (mode === "failed-skills" && isSkills))
          throw new Error("fixture cleanup failed");
        return rmTreePinned(parent, name, options);
      } },
    } : {}),
    quiesceRun: async request => {
      if (request.producerWorkLimits !== undefined) {
        assert.equal(request.site, "retained_ops");
        assert.equal(request.mode, "capture");
        assert.deepEqual(request.producerWorkLimits, { metadataBytes: 16 * 1024 ** 2, adminEntries: 128 });
        MaintenanceResources.check();
      } else if (MaintenanceResources.active() && !MaintenanceResources.discoveryActive() && request.site === "retained_ops") {
        assert.fail("charged family proof omitted producer envelope");
      }
      return {
        process: models && (mode === "missing-process" || (mode === "missing-reproof" && request.site === "settle_transfer:after_runner_git"))
        ? undefined : { state: (models && mode === "unverified") ||
          (mode === "origin-transfer-final-family" && transferBodyComplete) ? "unverified" : "quiescent", processes: [], killed: [], detail: "" },
        docker: { state: originMode && request.site === "retained_ops" ? "docker_unconfirmed" : "not_wired", removed: [], detail: "" },
      };
    },
  });
  if (originMode) {
    const observer = (git as any).observeMaintenanceTransferPhase.bind(git);
    t.mock.method(git as any, "observeMaintenanceTransferPhase", async (phase: string, action: () => Promise<unknown>) => {
      if (phase === "import") importPhase = true;
      const rpcBefore = chargedTransports;
      t.diagnostic(JSON.stringify({ transferPhase: phase, before: { ...resourceTotals } }));
      const value = await observer(phase, action);
      if (phase !== "import") assert.equal(chargedTransports, rpcBefore, phase + " adds no remote pair");
      if (layoutRefused && phase === "proof") {
        const bare = git.barePathFor(claim.repo.clone_url);
        await fs.writeFile(path.join(bare, "shallow"), command(bare, "rev-parse", "refs/heads/main") + "\n");
      }
      t.diagnostic(JSON.stringify({ transferPhase: phase, after: { ...resourceTotals } }));
      return value;
    });
    const scoped = git.withBoundaryProcessSpawner.bind(git);
    t.mock.method(git as any, "withBoundaryProcessSpawner", (...args: Parameters<typeof scoped>) => {
      const [spawn, signal, action, hooks] = args;
      return scoped(async request => {
        if (MaintenanceResources.active() && !MaintenanceResources.discoveryActive()) {
          const canonical = request.argv.includes("index-pack") && request.argv.includes("--stdin");
          const verb = request.argv.find(arg => ["config", "rev-parse", "for-each-ref", "merge-base", "rev-list", "cat-file", "pack-objects", "index-pack"].includes(arg)) ?? "capture";
          if (transferEntered) {
            if (canonical) {
              assert.deepEqual(semanticTrace.slice(-2), ["pair:list", "pair:exact"], "fresh import pair has no intervening Git metadata");
              assert.equal(chargedTransports, captureRPC + 2);
              importDispatched = true;
            } else assert.equal(chargedTransports, captureRPC + (importDispatched ? 2 : 0), "reads/private proofs do not add pairs");
          }
          semanticTrace.push("git:dispatch:" + verb);
        }
        const canonical = request.argv.includes("index-pack") && request.argv.includes("--stdin");
        const packDir = path.join(git.barePathFor(claim.repo.clone_url), "objects", "pack");
        const previous = postImportRefused && canonical ? new Set(await fs.readdir(packDir)) : undefined;
        const child = await spawn(request);
        if (!previous) return child;
        return { ...child, completed: child.completed.then(async result => {
          assert.equal(result.code, 0, "actual canonical strict import succeeded before fault injection");
          const indices = (await fs.readdir(packDir)).filter(name => name.endsWith(".idx") && !previous.has(name));
          assert.equal(indices.length, 1);
          retainedImportedPack = path.join(packDir, indices[0]!.replace(/\.idx$/, ".pack"));
          retainedImportedBytes = await fs.readFile(retainedImportedPack);
          await fs.unlink(path.join(packDir, indices[0]!));
          unknownCanonicalEntry = path.join(packDir, "c1-unknown-residue");
          await fs.writeFile(unknownCanonicalEntry, "unknown canonical bytes retained");
          return result;
        }) };
      }, signal, action, hooks);
    });
    const runGit = git["runGit"].bind(git);
    t.mock.method(git as any, "runGit", (...args: Parameters<typeof runGit>) => {
      if (MaintenanceResources.active() && !MaintenanceResources.discoveryActive()) semanticTrace.push("git:start:" + args[1][0]);
      return runGit(...args);
    });
    const runnerGit = git["runGitAsRunner"].bind(git);
    t.mock.method(git as any, "runGitAsRunner", (...args: Parameters<typeof runnerGit>) => {
      if (MaintenanceResources.active() && !MaintenanceResources.discoveryActive()) semanticTrace.push("git:start:runner");
      return runnerGit(...args);
    });
    const settle = TickSpawner.prototype.settled;
    t.mock.method(TickSpawner.prototype, "settled", async function (this: TickSpawner) {
      await settle.call(this);
      if (transferEntered && MaintenanceResources.active()) {
        assert.deepEqual(this.survivors(), []);
        ownerJoined = true; semanticTrace.push("owner:joined");
      }
    });
    const indexOutput = RecoveryMaintenance.prototype.beforeMutation;
    t.mock.method(RecoveryMaintenance.prototype, "beforeMutation", async function (this: RecoveryMaintenance,
      ...args: Parameters<typeof indexOutput>) {
      // Unconditional: any ledger index once the transfer owner has been entered must follow its join.
      if (transferEntered && !ownerJoined) indexesBeforeJoin++;
      if (transferSettled) {
        outputIndexCalls++;
        assert.equal(ownerJoined, true);
        assert.deepEqual(semanticTrace.slice(-2), ["pair:list", "pair:exact"], "fresh output pair has no intervening metadata Git");
        semanticTrace.push("ledger:wip-index");
      }
      return indexOutput.apply(this, args);
    });
    const importer = git.importMaintenanceTip.bind(git);
    t.mock.method(git, "importMaintenanceTip", async (...args: Parameters<typeof importer>) => {
      try {
        assert.ok((git as any).recoveryOperations.getStore(), "real runner supplies recovery-operation ALS context");
        const result = await importer(...args); transferResults.push(result.kind); return result;
      }
      catch (error) {
        t.diagnostic(JSON.stringify({ importerFailureSite: error instanceof Error
          ? error.stack?.match(/git\.ts:[0-9]+:[0-9]+/g)?.slice(0, 3) : [] }));
        throw error;
      }
    });
    const boundary = runner.withRetainedSourceBoundary.bind(runner);
    t.mock.method(runner, "withRetainedSourceBoundary", async (...[runId, paths, action]: Parameters<typeof boundary>) =>
      boundary(runId, paths, async ctx => {
        boundaryEntered++;
        const bare = git.barePathFor(claim.repo.clone_url);
        const snapshot = async () => ({
          bareConfig: await fs.readFile(path.join(bare, "config"), "utf8"),
          refs: command(bare, "show-ref"),
          index: await fs.readFile(path.join(source, ".git", "index")),
          config: await fs.readFile(path.join(source, ".git", "config"), "utf8"),
          head: command(source, "rev-parse", "HEAD"),
          tracked: await fs.readFile(path.join(source, "README.md"), "utf8"),
          untracked: await fs.readFile(path.join(source, "unique-untracked"), "utf8"),
        });
        if (dirtyOrigin) await fs.writeFile(path.join(source, "maintenance-wip"), unique);
        const before = await snapshot(), beforeAttempts = attempts, beforeModels = models;
        const upload = t.mock.method(client, "uploadRecoveryBundle");
        const reserve = t.mock.method(client, "reserveRecoveryCapture");
        const held = t.mock.method(client, "publishHeld");
        const releaseCustody = t.mock.method(client, "releaseRecoveryCustody");
        const forwarded: typeof ctx = {
          ...ctx,
          captureWip: async (...args) => {
            const value = await ctx.captureWip(...args);
            capturedSignal = MaintenanceResources.signalIfActive();
            captureRPC = chargedTransports;
            return value;
          },
          withMaintenanceTransfer: async action => {
            transferCalls++;
            assert.ok(capturedSignal, "WIP has actually returned before transfer");
            assert.equal(MaintenanceResources.signalIfActive(), capturedSignal, "same original resource slot");
            const value = await ctx.withMaintenanceTransfer(async check => {
              transferEntered++;
              check();
              const result = await action(check);
              transferBodyComplete = true;
              if (mode === "origin-transfer-head") command(source, "reset", "--soft", "HEAD^");
              return result;
            });
            transferSettled = true;
            return value;
          },
        };
        if (mode === "origin-transfer-missing") Reflect.deleteProperty(forwarded, "withMaintenanceTransfer");
        const result = await action(forwarded);
        assert.equal(models, beforeModels);
        assert.equal(upload.mock.callCount(), 0);
        assert.equal(reserve.mock.callCount(), 0);
        assert.equal(held.mock.callCount(), 0);
        assert.equal(releaseCustody.mock.callCount(), 0, "transfer grants no FINAL/custody-release authority");
        upload.mock.restore(); reserve.mock.restore(); held.mock.restore(); releaseCustody.mock.restore();
        const after = await snapshot();
        if (transferOrigin && mode !== "origin-transfer-missing") {
          assert.equal(transferCalls, 1);
          assert.equal(transferEntered, 1);
          assert.equal(transferSettled, mode !== "origin-transfer-final-family" && !transferFails);
          // W0-H: the commit group has three dispatches (write-tree, commit-tree, update-ref), +4 rpcAttempts (2 extra dispatches x 2 fetches): 12/14 -> 16/18.
          assert.equal(chargedTransports, expectedRPC, "warm real WorkerClient list/exact transports");
          assert.equal(discoveryTransports, 4, "discovery has separate counters");
          t.diagnostic(JSON.stringify({ chargedTransports, discoveryTransports, transferCalls, transferEntered, transferSettled }));
          if (!dirtyOrigin) assert.deepEqual(after, before, "verified no-op leaves source and refs/config unchanged");
          else {
            if (mode !== "origin-transfer-head") assert.notEqual(after.head, before.head);
            assert.notDeepEqual(after.index, before.index);
            assert.deepEqual({ ...after, head: before.head, index: before.index }, before);
          }
          if (dirtyOrigin && mode !== "origin-transfer-head") {
            assert.equal(command(source, "rev-parse", "HEAD^"), before.head);
            assert.equal(command(source, "status", "--porcelain"), "");
          }
        } else assert.deepEqual(after, before, "refusal leaves source unchanged");
        assert.equal(attempts, beforeAttempts, "origin publication grants no capture permit");
        return result;
      }));
  }
  if (mode === "record-none" || mode === "prior-record-none" || mode === "nonmatching-error" || mode === "ordinary-producer-shaped-error") {
    // Exercise the real retention helper before generic reporting. No observation
    // fields are injected: even an ACK after record:none must not undo its denial.
    type Flight = { executor: Executor };
    const view = runner as unknown as {
      reportGenericFailure(claim: ReturnType<typeof gitlabClaim>, flight: Flight, error: unknown): Promise<void>;
      phasePreflightHandoff(claim: ReturnType<typeof gitlabClaim>, flight: Flight, session: string | undefined,
        rejection: { rejected: boolean; value?: unknown }): Promise<unknown>;
      retainStoppedSource(flight: Flight, executor: Executor, reason: string,
        handler: { memo: boolean; stale: boolean; record: "none" }): Promise<unknown>;
    };
    const retain = (flight: Flight) => view.retainStoppedSource(flight, flight.executor, "fixture record none",
      { memo: false, stale: false, record: "none" });
    if (mode === "prior-record-none" || mode === "nonmatching-error" || mode === "ordinary-producer-shaped-error") {
      const handoff = view.phasePreflightHandoff.bind(view);
      t.mock.method(view, "phasePreflightHandoff", async (...args: Parameters<typeof handoff>) => {
        try { return await handoff(...args); }
        catch (error) {
          if (mode === "nonmatching-error" || mode === "ordinary-producer-shaped-error")
            throw new Error(mode === "ordinary-producer-shaped-error" ? "maintenance_source_alias_refused" : "different dispatch failure");
          await retain(args[1]);
          throw error;
        }
      });
    } else {
      const generic = view.reportGenericFailure.bind(view);
      t.mock.method(view, "reportGenericFailure", async (...args: Parameters<typeof generic>) => {
        await retain(args[1]);
        return generic(...args);
      });
    }
  }
  if (mode === "reclaimed") {
    const stop = SteeringChannel.prototype.stop;
    t.mock.method(SteeringChannel.prototype, "stop", async function (this: SteeringChannel) {
      await stop.call(this);
      const bare = git.barePathFor(claim.repo.clone_url);
      const journal = JSON.parse(command(bare, "config", "--get", "uzi-recovery." + branch + ".clone"));
      command(bare, "config", "--add", "uzi-attempts." + branch + ".entry", JSON.stringify({
        attemptId: journal.attemptId, runId: journal.runId, clonePath: journal.clonePath, state: "reclaimed",
      }));
    });
  }
  const scanEntered = deferred(), scanRelease = deferred();
  const open = fs.opendir.bind(fs);
  if (mode === "normal") t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    if (notifiedTail && String(args[0]) === path.join(git.recoveryRoot, claim.run_id)) {
      scanEntered.resolve();
      await scanRelease.promise;
    }
    return open(...args);
  });
  let notifiedWork: Promise<void> | undefined;
  let notifiedTail: Promise<unknown> | undefined;
  let notifiedWhileReserved = false;
  const resume = RecoveryCoordinator.prototype.resumeTerminalSource;
  t.mock.method(RecoveryCoordinator.prototype, "resumeTerminalSource", function (this: RecoveryCoordinator,
    ...args: Parameters<typeof resume>) {
    notifiedTail = args[1];
    notifiedWhileReserved = (runner as unknown as Tails).executionTails.get(claim.run_id) === args[1];
    if (mode === "notification-throw") throw new Error("notification failure fixture");
    if (mode === "notification-reject") return Promise.reject(new Error("notification failure fixture"));
    if (mode === "origin-pressure") pressure = true;
    notifiedWork = resume.apply(this, args);
    return notifiedWork;
  });
  const executing = runner.execute(claim);
  const tails = (runner as unknown as Tails).executionTails;
  const tail = tails.get(claim.run_id)!;
  try {
    await executing;
    if (originMode) await notifiedWork;
    if (transferOrigin && mode !== "origin-transfer-missing") {
      // Assert outside the coordinator callback: its refusal catch must not swallow a failed test.
      assert.equal(transferCalls, 1, "active coordinator enters the transfer owner");
      assert.ok(exclusion.entered > 0 && exclusion.proofs > 0, "admission runs under the writer exclusion");
      assert.equal(transferEntered, 1);
      t.diagnostic(JSON.stringify({ resourceTotals, chargedTransports, discoveryTransports,
        refusals: log.lines.filter(line => (line as { reason?: string }).reason).map(line => {
          const { reason, stage, msg } = line as { reason?: string; stage?: string; msg?: string };
          return { reason, stage, msg };
        }) }));
      assert.equal(transferBodyComplete, !transferFails);
      assert.equal(ownerJoined, true, "even refused import joins its real owner before return");
      assert.equal(importDispatched, mode !== "origin-already-present" && !importRefused && !layoutRefused);
      assert.equal(outputIndexCalls, acceptedOutput ? 1 : 0);
      assert.equal(indexesBeforeJoin, 0, "no ledger index is written before the transfer owner joins");
      if (layoutRefused) assert.deepEqual(log.lines.filter(line => (line as { msg?: string }).msg === "recovery: terminal origin refused")
        .map(line => (line as { reason?: string }).reason), ["maintenance_source_layout_unsupported"],
        "fixed shallow/layout diagnostic survives the real transfer-owner collapse");
      assert.equal(outputPairObserved, acceptedOutput || mode === "origin-transfer-revoked");
      if (acceptedOutput) assert.deepEqual(transferResults, [mode === "origin-already-present" ? "already_present" : "imported"]);
      assert.equal(transferSettled, mode !== "origin-transfer-final-family" && !transferFails);
      // W0-H: the commit group has three dispatches (write-tree, commit-tree, update-ref), +4 rpcAttempts (2 extra dispatches x 2 fetches): 12/14 -> 16/18.
      assert.equal(chargedTransports, expectedRPC);
      assert.equal(discoveryTransports, 4);
      assert.equal(resourceTotals.rpcAttempts, chargedTransports, "all charged strict client attempts counted");
      const caps = { decoded: git.recoveryDecodedLimitBytes, closureEntries: 65536, sourceBytes: 256 * 1024 ** 2,
        sourceEntries: 65536, artifactBytes: 256 * 1024 ** 2, metadataBytes: 256 * 1024 ** 2,
        adminEntries: 4096, sources: 128, launches: 512, rpcAttempts: 32 };
      for (const resource of Object.keys(caps) as MaintenanceResource[])
        assert.ok((resourceTotals[resource] ?? 0) <= caps[resource], resource);
      t.diagnostic(JSON.stringify({ resourceTotals, chargedTransports, discoveryTransports,
        transferCalls, transferEntered, transferSettled, importDispatched, outputPairObserved, outputIndexCalls,
        semanticTrace, quiescence: "injected; not coupled helper capacity proof" }));
    }
    if (productionDefault) {
      assert.equal(TERMINAL_ORIGIN_ADMISSION_ENABLED, false, "staged groundwork ships admission disabled");
      assert.equal(boundaryEntered, 0, "no retained-source boundary, WIP capture or transfer");
      assert.equal(exclusion.entered, 0, "writer exclusion never entered");
      assert.equal(exclusion.proofs, 0, "no exclusive-idle proof");
      assert.equal(transferCalls, 0);
      assert.equal(capturedSignal, undefined);
      assert.equal(chargedTransports, 0, "no charged producer transport");
      assert.equal(discoveryTransports, 0, "no producer discovery transport");
      assert.deepEqual(resourceTotals, {}, "no producer resource charge");
      await assert.rejects(fs.stat(path.join(git.recoveryRoot, "..", "recovery-maintenance")), { code: "ENOENT" },
        "no producer ledger initialised");
    }
    if (mode === "origin-transfer-missing") {
      assert.equal(transferCalls, 0);
      assert.equal(capturedSignal, undefined, "missing transfer capability refuses before source capture");
      await assert.rejects(fs.stat(path.join(git.recoveryRoot, "..", "recovery-maintenance")), { code: "ENOENT" });
    }
    assert.equal(models, 1);
    if (mode === "ordinary-producer-shaped-error") {
      assert.equal(boundaryEntered, 0);
      assert.equal(exclusion.entered, 0);
      assert.equal(log.lines.some(line => (line as { msg?: string }).msg === "recovery: terminal origin refused"), false,
        "ordinary error-shaped message is not projected by the guarded producer catch");
      assert.equal(transferCalls, 0);
      await assert.rejects(fs.stat(path.join(git.recoveryRoot, "..", "recovery-maintenance")), { code: "ENOENT" });
    }
    assert.ok(api.states.some(s => s.body.status === "failed"));
    assert.ok(terminalAcks > 0, "actual FAILED ACK was received before fixture faults");
    const records = await new RecoveryCoordinator({
      client, git, log: log.logger, recoveryRoot: git.recoveryRoot, workerToken: "fixture-journal-key",
    }).snapshotBootRecords();
    if (originMode) assert.equal(JSON.stringify(records).includes(".maintenance-transfer-"), false,
      "borrowed scratch paths never enter authenticated journals");
    const snapshotProven = !["unverified", "failed-reap", "record-none", "prior-record-none"].includes(mode);
    if (snapshotProven) {
      assert.ok(attempts > 0, "actual archive production was attempted: " + JSON.stringify(log.lines));
      assert.ok(records.length > 0, "authenticated boot records survive failed production");
      assert.ok(records.every(r => r.runId === claim.run_id && r.generation === 1));
      assert.ok(records.some(r => r.reason === "bundle_failed"), "authenticated failed-production record remains");
    }
    if (originMode) {
      const prefixes = records.filter(r => r.captureId.startsWith("maintenance-v1-"));
      const originalObservation = await tail;
      assert.ok((originalObservation as unknown as { originalContext?: unknown })?.originalContext, JSON.stringify(originalObservation));
      assert.equal(prefixes.length, transferOrigin && mode !== "origin-transfer-missing" ? 1 : 0, JSON.stringify({ records: records.map(r => ({ id: r.captureId, state: r.state })), logs: log.lines.slice(-3) }));
      if (mode === "origin-pressure") {
        assert.equal(guard.preflight(), "data_volume_full");
        const ops = await runner.withRetainedSourceBoundary(claim.run_id, [source], async ctx => {
          assert.equal(ctx.open(), true, "operator admission keeps its existing full-volume semantics");
          assert.equal(await ctx.quiesce([source]), true);
        });
        assert.equal(ops.entered, true);
        await assert.rejects(fs.stat(path.join(git.recoveryRoot, "..", "recovery-maintenance")), { code: "ENOENT" });
      } else if (mode === "origin-source-limit") {
        const ledger = JSON.parse(await fs.readFile(path.join(git.recoveryRoot, "..", "recovery-maintenance", "complete.json"), "utf8")).value;
        assert.equal(ledger.cycles[0].slots.length, 1, "paid descriptor refusal consumes its existing slot");
        assert.equal(ledger.cycles[0].fenced, true);
      } else if (transferOrigin && mode !== "origin-transfer-missing") {
        const prefix = prefixes[0];
        assert.ok(prefix);
        const resultHead = command(source, "rev-parse", "HEAD");
        assert.equal(prefix.sourceSha, mode === "origin-transfer-head" || !dirtyOrigin ? resultHead : command(source, "rev-parse", "HEAD^"), JSON.stringify(log.lines.slice(-5)));
        assert.equal(prefix.attemptedHeadSha, undefined);
        const ledger = JSON.parse(await fs.readFile(path.join(git.recoveryRoot, "..", "recovery-maintenance", "complete.json"), "utf8")).value;
        assert.equal(JSON.stringify(ledger).includes(".maintenance-transfer-"), false,
          "borrowed scratch paths never enter ledger lineage");
        const cycle = ledger.cycles[0];
        const output = createHash("sha256").update(canonicalJson(["maintenance-wip-v1", cycle.identity, resultHead])).digest("hex");
        assert.equal(cycle.outputs.includes(output), acceptedOutput, "only settled current transfer accepts WIP lineage");
        assert.equal(cycle.outputs.length, acceptedOutput ? 2 : 1, "failed transfer retains only original CID fence");
        assert.equal(cycle.slots.length, 1);
        assert.equal(cycle.fenced, true);
        if (transferFails) {
          assert.throws(() => command(git.barePathFor(claim.repo.clone_url), "cat-file", "-e", resultHead),
            postImportRefused ? "post-import selected-range object is missing" : "refusal prevents the canonical import");
          if (postImportRefused) {
            assert.ok(retainedImportedPack);
            assert.deepEqual(await fs.readFile(retainedImportedPack), retainedImportedBytes,
              "imported pack is never rolled back after failed range proof");
            assert.equal(await fs.readFile(unknownCanonicalEntry, "utf8"), "unknown canonical bytes retained");
          }
        } else if (mode !== "origin-transfer-head") {
          assert.equal(command(git.barePathFor(claim.repo.clone_url), "rev-parse", resultHead + "^{tree}"),
            command(source, "rev-parse", resultHead + "^{tree}"), "successful transfer/no-op is connected in canonical bare");
        }
        assert.equal(prefix.heldPolicy, undefined);
        assert.equal(prefix.generation, 1);
      }
    }
    assert.equal(records.filter(r => r.bundlePath).length, 0);
    const artifacts = await fs.readdir(git.recoveryRoot, { recursive: true });
    assert.deepEqual(artifacts.filter(p => p.includes(".bundle")), [], "no physical installed or temporary bundles");
    assert.equal(command(source, "show", committed + ":unique-commit"), unique);
    assert.equal(await fs.readFile(path.join(source, "README.md"), "utf8"), "tracked " + unique);
    assert.equal(await fs.readFile(path.join(source, "unique-untracked"), "utf8"), "untracked " + unique);
    if (snapshotProven) {
      assert.equal(command(source, "show", "HEAD:README.md"), "tracked " + unique);
      assert.equal(command(source, "show", "HEAD:unique-untracked"), "untracked " + unique);
    }
    const bare = git.barePathFor(claim.repo.clone_url);
    const journal = JSON.parse(command(bare, "config", "--get", "uzi-recovery." + branch + ".clone"));
    assert.equal(journal.clonePath, source, "plain source journal physically retained");
    const config = command(bare, "config", "--local", "--list");
    assert.ok(!config.includes("uzi-stopped."));
    assert.equal(config.includes('"state":"reclaimed"'), mode === "reclaimed");
    assert.ok(config.includes('"state":"live"'), "live attempt attribution remains");
    const observed = await tail;
    if (mode === "normal" || originMode || mode === "notification-throw" || mode === "notification-reject") {
      assert.equal(notifiedTail, tail, "automatic notification captures the exact pending local tail");
      assert.equal(notifiedWhileReserved, true, "notification precedes original map deletion");
      assert.equal(observed?.terminal, "normal_failed");
      if (mode === "normal") {
        await scanEntered.promise;
        let joined = false;
        const drain = runner.shutdownRecoveries().then(() => { joined = true; });
        await new Promise<void>(resolve => setImmediate(resolve));
        assert.equal(joined, false, "execute returned while coordinator still owns the metadata child");
        scanRelease.resolve();
        await drain;
      } else if (!originMode) {
        assert.ok(JSON.stringify(log.lines).includes("recovery: terminal saved evidence pass failed; source retained"));
        assert.ok(!JSON.stringify(log.lines).includes("notification failure fixture"));
      }
      assert.ok(observed?.family, "fresh normal FAILED returns positive source family: " + JSON.stringify(log.lines.slice(-5)));
      assert.equal(observed.genesis.clonePath, source);
      assert.equal(observed.genesis.generation, 1);
      await assert.rejects(fs.stat(target), { code: "ENOENT" });
    } else assert.equal(observed, undefined, "uncertain or stopped source stays unknown");
    assert.equal(tails.has(claim.run_id), false);
  } finally {
    releaseAck.resolve();
    scanRelease.resolve();
    runner.shutdown();
    await runner.shutdownRecoveries();
    await executing;
  }
});
}
