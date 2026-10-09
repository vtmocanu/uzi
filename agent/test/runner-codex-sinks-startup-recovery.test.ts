import { describe, it } from "node:test";
import { createServer } from "node:http";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { type Readable } from "node:stream";
import {
  launchCodexEffectRoot,
  CodexLaunchError,
  hasVerifiedStartupCleanup,
  type SupervisorProcess,
} from "../src/codex/launcher.js";
import { boundaryProcessSpawnerForTest } from "../src/codex/codex-executor.js";
import { COMMAND_UID, WORKER_UID } from "../src/runner-uid.js";
import { RejectedStartupTransport } from "./codex-rejected-startup-fixture.js";
import type { Logger } from "../src/log.js";
import { WorkerClient } from "../src/client.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import {
  api,
  client,
  fakeGitHub,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";
import {
  GIT_ENV,
  commitInTree,
  drain,
  spyPublishLands,
  manualDeadline,
  codexRig,
  FakeCodexExecutor,
  statuses,
  VAULT_FEATURE,
  VAULT_UNVERIFIED,
  trackingTip,
  feedTexts,
  parkReports,
  spyCustodySettle,
  enabledRecovery,
  workThenDefer,
  assertCaptured,
  sessionKept,
  countPublishes,
  manualStartedDeadline,
  installRecoveryRootCleanup,
} from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

describe("RunRunner M2 — lost refresh replies reconcile before credential recovery", () => {
  for (const sink of ["finalize", "checkpoint"] as const) {
    for (const reply of ["vault_locked", "unavailable", "contended", "legacy_typed_first"] as const) {
      it(`${sink}: ${reply} HTTP reconciliation parks with exact cause and no further credential authority`, async () => {
        const requests: Array<Record<string, unknown>> = [];
        const server = createServer(async (req, res) => {
          let body = "";
          for await (const chunk of req) body += String(chunk);
          requests.push(JSON.parse(body));
          assert.match(req.url ?? "", /\/codex\/refresh$/);
          if (requests.length === 1 && reply !== "legacy_typed_first") {
            req.socket.destroy();
            return;
          }
          res.writeHead(409, { "Content-Type": "application/json" });
          res.end(JSON.stringify(reply === "vault_locked" || reply === "legacy_typed_first"
            ? { reason: "vault_locked", error: "fixture raw diagnostic must stay private" }
            : { error: reply === "contended" ? "codex refresh is contended; retry" : "codex credential is not available" }));
        });
        await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
        const address = server.address();
        assert.ok(address && typeof address !== "string");
        const credentialClient = new WorkerClient(`http://127.0.0.1:${address.port}`, "fixture-join", "test", nullLogger());
        const pubs = spyPublishLands();
        try {
          client.protocolFeatures = [VAULT_FEATURE];
          const { gitlab, calls: mrCalls } = fakeGitlab();
          const rig = codexRig({ credentialClient });
          const { logger, lines } = recordingLogger();
          const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
            commitInTree(ctx.worktreePath, "COMMITTED.txt", "committed, not yet fetched back\n");
            fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "uncommitted edit\n");
            if (sink === "checkpoint") await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
            return { branch: ctx.branch };
          }, rig.settle);
          const iid = 177000 + ["finalize", "checkpoint"].indexOf(sink) * 10 + ["vault_locked", "unavailable", "contended", "legacy_typed_first"].indexOf(reply);
          const claim = gitlabClaim(iid, { claim_generation: 3 });
          const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, { recoveryRetryMs: 1 });
          const custody = spyCustodySettle(runner);
          await runner.execute(claim);
          assert.equal(requests.length, reply === "legacy_typed_first" ? 1 : 2, "only the bounded reconciliation credential requests");
          if (requests.length === 2) assert.deepEqual(requests[1], requests[0], "same operation id, capability and observed generation after a lost reply");
          const parks = parkReports(claim.run_id);
          assert.equal(parks.length, 1);
          assert.equal(parks[0]!.recovery_cause, reply === "vault_locked" || reply === "legacy_typed_first" ? "vault_locked" : undefined);
          assertCaptured(iid);
          assert.deepEqual(rig.boundaries, [sink], "no new boundary after reconciliation deferred");
          assert.equal(rig.processSpawns(), 0, "no permit-held process action");
          assert.equal(api.completionPermitRequests.length, 0, "no completion authority requested");
          assert.equal(mrCalls.length, 0);
          assert.equal(custody(), 0);
          assert.ok(!statuses(claim.run_id).some((status) => status === "failed" || status === "completed"));
          const visible = JSON.stringify({ lines, states: api.states, feed: api.messages(claim.run_id) });
          assert.ok(!visible.includes("fixture raw diagnostic"));
          assert.ok(!visible.includes(String(requests[0]!.operation_id)), "operation metadata stays private");
          if (reply === "unavailable" || reply === "contended") assert.ok(feedTexts(claim.run_id).every((text) => !/vault/i.test(text)));
        } finally {
          pubs();
          await new Promise<void>((resolve, reject) => server.close((err) => err ? reject(err) : resolve()));
        }
      });
    }
  }
});


describe("RunRunner M2 — unknown refresh sink retries and local recovery", () => {
  it("refresh_unknown: fetch-back and park report failures visibly retry with source/session/custody retained", async () => {
    client.protocolFeatures = [VAULT_FEATURE];
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const rig = codexRig({ refreshUnknown: true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(177020, { claim_generation: 3 });
    let captureFailures = 0;
    let reportAttempts = 0;
    const { logger, lines } = recordingLogger();
    const { coord } = enabledRecovery();
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recovery: coord, recoveryRetryMs: 1 });
    const custody = spyCustodySettle(runner);
    const retained = () => {
      assert.equal(fs.readFileSync(path.join(worktreeDirFor(177020), "DIRTY.txt"), "utf8"), "uncommitted edit\n");
      assert.equal(sessionKept(lines), false, "the active session has not been torn down");
      assert.equal(custody(), 0);
      assert.equal(mrCalls.length, 0);
      assert.equal(api.completionPermitRequests.length, 0);
      assert.ok(!statuses(claim.run_id).includes("completed"));
    };
    const fetch = git.fetchAgentBranch.bind(git);
    git.fetchAgentBranch = (async (...args: Parameters<typeof fetch>) => {
      if (w.deferred() && captureFailures++ === 0) {
        retained();
        throw new Error("injected capture failure");
      }
      return fetch(...args);
    }) as typeof git.fetchAgentBranch;
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") {
        retained();
        if (++reportAttempts === 1) throw new Error("injected park report failure");
      }
      return report(runId, body, signal);
    };
    const restore = spyPublishLands();
    try {
      await runner.execute(claim);
      assert.equal(reportAttempts, 2);
      assert.equal(parkReports(claim.run_id).length, 1);
      assert.equal(parkReports(claim.run_id)[0]!.recovery_cause, undefined);
      assertCaptured(177020);
      const feed = feedTexts(claim.run_id);
      assert.equal(feed.filter((text) => text === VAULT_UNVERIFIED).length, 1);
      assert.equal(feed.filter((text) => text === "Could not record the recovery pause yet; keeping the local work and session and retrying.").length, 1);
      assert.ok(feed.every((text) => !/vault/i.test(text)));
      assert.equal(rig.refreshCalls(), 2, "bounded initial reconciliation only, no reexchange while retrying sinks");
      assert.equal(rig.releaseCalls(), 0);
      assert.equal(custody(), 0);
      assert.deepEqual(rig.boundaries, ["finalize"]);
    } finally { restore(); }
  });

  it("refresh_unknown: publication failure parks verified local-only work; same-worker resume retries publication and completes", async () => {
    client.protocolFeatures = [VAULT_FEATURE];
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const rig = codexRig({ refreshUnknown: true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(177021, { claim_generation: 3 });
    let pubs = 0;
    const publish = client.publishCheckpoint.bind(client);
    client.publishCheckpoint = async (_runId, _tip, pack) => {
      pubs++;
      await drain(pack);
      throw new Error("injected checkpoint publication outage");
    };
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.equal(pubs, 1);
    assertCaptured(177021);
    assert.equal(parkReports(claim.run_id).length, 1, "publication remains best effort after verified fetch-back");
    assert.equal(parkReports(claim.run_id)[0]!.recovery_cause, undefined);
    assert.ok(feedTexts(claim.run_id).includes("Paused for credential recovery. The recovery checkpoint is saved only on this worker; this run can resume at its next retry."));
    assert.ok(!feedTexts(claim.run_id).some((text) => text.includes("checkpoint is published")));
    assert.equal(mrCalls.length, 0);
    assert.equal(custody(), 0);
    assert.equal(api.completionPermitRequests.length, 0);
    assert.equal(rig.refreshCalls(), 2);
    client.publishCheckpoint = publish;
    const publication = countPublishes(() => true);
    try {
      const ready = codexRig();
      let checkpointRetried = false;
      const exec = new FakeCodexExecutor(ready.safety, async (ctx) => {
        assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "utf8"), "uncommitted edit\n", "local-only recovery source restored");
        await ctx.checkpoint!({ reap: true });
        // The overlay-less Codex checkpoint defers its scan outside the permit.
        await ctx.checkpoint!({ reap: false });
        checkpointRetried = true;
        return { branch: ctx.branch };
      }, ready.settle);
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute({ ...claim, claim_generation: 4 });
      assert.ok(checkpointRetried, `resume retries checkpoint publication through a ready boundary: ${JSON.stringify(api.states)}`);
      assert.equal(publication.count(), 1, "the direct resume retries checkpoint publication exactly once");
      assert.equal(parkReports(claim.run_id).length, 1, "no predecessor recapture flight");
      assert.deepEqual(ready.boundaries, ["checkpoint", "finalize"], "publication and MR cross ready boundaries");
      assert.ok(ready.refreshCalls() >= 1, "the resumed finalize reconciled ready");
      assert.equal(mrCalls.length, 1, "the resumed claim created the MR");
      assert.ok(statuses(claim.run_id).includes("completed"));
      assert.ok(!statuses(claim.run_id).includes("failed"));
    } finally { publication.restore(); }
  });
});

describe("M2 existing completion hold deferral control", () => {
  it("unknown refresh during a verified hold capture stays nonterminal", async () => {
    api.setCompletionHoldResponse("paused");
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ refreshUnknown: true });
    let entered = false;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "HOLD.txt", "committed work before an existing completion hold\n");
      fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "dirty work\n");
      entered = await ctx.enterCompletionHold!("existing pending owner decision");
      return entered ? { branch: ctx.branch, completionHeld: { reason: "existing pending owner decision" } } : { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(177090, { claim_generation: 3 });
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
    assert.ok(!statuses(claim.run_id).includes("failed"), "an unknown refresh must not fail this hold");
    assert.equal(rig.refreshCalls(), 2, "verified local-only hold ends the flight without more credential requests");
    assert.equal(entered, true, "existing completion hold is preserved after verified local capture");
    assert.equal(api.completionHoldRequests.length, 1);
    assert.ok(trackingTip(177090), "verified tracking snapshot survives the local-only hold");
    assert.equal(calls.length, 0);
    assert.equal(api.completionPermitRequests.length, 0);
  });
});

describe("RunRunner M2 fatal rejected startup", () => {
  const cases = [
    { mode: "unconfirmed", classification: "started_deadline", sink: "milestone_checkpoint" },
    { mode: "hard", classification: "started_deadline", sink: "done_checkpoint" },
    { mode: "posture", classification: "posture", sink: "milestone_checkpoint" },
    { mode: "protocol", classification: "protocol_evidence", sink: "done_checkpoint" },
    { mode: "exit", classification: "supervisor_exit", sink: "milestone_checkpoint" },
    { mode: "errno", classification: "spawn_EAGAIN", sink: "done_checkpoint" },
  ] as const;
  for (const [index, scenario] of cases.entries()) {
    it(`${scenario.mode}: real launcher classification survives soft expiry in logs and failed state`, async () => {
      const { gitlab } = fakeGitlab();
      const { github } = fakeGitHub();
      const { logger, lines } = recordingLogger();
      const gitLog = git as unknown as { log: Logger };
      const originalGitLog = gitLog.log;
      gitLog.log = logger;
      const hard = manualDeadline("checkpoint");
      const privatePath = "/private/fatal-startup-fixture/provider-cache";
      const provider = ["glpat-", "fatalFixtureBody12345"].join("");
      const evidence = "fatal-private-evidence-marker";
      const rawDiagnostic = [privatePath, provider, evidence, "x".repeat(2048)].join(" ");
      let armedTarget = false;
      let injections = 0;
      let softCallback: (() => void) | undefined;
      let softFired = 0;
      let transport: RejectedStartupTransport | undefined;
      let launchError: unknown;
      let setupFailure: unknown;
      let checkpointReturned = false;
      const startupTimer = scenario.mode === "unconfirmed" || scenario.mode === "hard"
        ? manualStartedDeadline() : undefined;
      const fireSoft = (): void => {
        assert.ok(softCallback, "soft callback is armed after the target spawn begins");
        softFired += 1;
        softCallback();
      };
      const rig = codexRig({
        armDeadline: hard.armDeadline,
        boundaryProcess: async (request, startupMs, startupCleanup) => {
          if (!armedTarget || injections || request.identity !== "worker_pat" ||
            !request.argv.includes("fetch") || !request.argv.includes("origin")) return undefined;
          injections += 1;
          assert.equal(request.recoverableTimeout, true);
          assert.ok(startupCleanup);
          assert.ok(startupCleanup.hardDeadlineAt - Date.now() > 40_000);
          const authorization = startupCleanup;
          transport = new RejectedStartupTransport(request.identity);
          const child = transport;
          const spawnError = Object.assign(new Error(rawDiagnostic), { code: "EAGAIN" });
          const spawner = boundaryProcessSpawnerForTest("required", (spec, launchMs, forwarded) => {
            assert.equal(forwarded, authorization, "original hard authorization is forwarded unchanged");
            assert.equal(launchMs, startupMs);
            return launchCodexEffectRoot(spec, {
              env: { UZI_UID_SPLIT: "1" },
              resolveWorkerUid: () => WORKER_UID, resolveCommandUid: () => COMMAND_UID,
              startupCleanup: forwarded, deadlines: { started: launchMs },
              ...(startupTimer ? { armStartedDeadlineForTest: startupTimer.arm } : {}),
              spawnSupervisor: () => {
                if (scenario.mode === "errno") {
                  queueMicrotask(fireSoft);
                  throw spawnError;
                }
                if (scenario.mode === "posture" || scenario.mode === "protocol" || scenario.mode === "exit") {
                  queueMicrotask(() => {
                    fireSoft();
                    if (scenario.mode === "posture") child.unsafeStarted(rawDiagnostic);
                    if (scenario.mode === "protocol") child.malformedEvidence(rawDiagnostic);
                    if (scenario.mode === "exit") child.exitBeforeStarted(rawDiagnostic);
                  });
                }
                return child as unknown as SupervisorProcess;
              },
            });
          });
          const result = spawner(request, startupMs, startupCleanup).then(
            () => ({ error: undefined }), (error: unknown) => ({ error }),
          );
          if (scenario.mode === "unconfirmed" || scenario.mode === "hard") {
            assert.ok(startupTimer);
            assert.equal(await startupTimer.installed, startupMs, "exact supplied startup residual");
            startupTimer.expire();
            await new Promise<void>(resolve => setImmediate(resolve));
            assert.ok(child.events.includes("dispose"), "startup expiry must request disposal before cleanup");
            await child.disposeRequested;
            assert.equal(rig.registry.pendingLaunchCount(), 1);
            fireSoft();
            if (scenario.mode === "hard") {
              // Event-gated permit abort; the launcher's absolute authorization stays unchanged.
              hard.fire();
              child.releaseVerifiedCleanup();
            } else {
              child.releaseUnconfirmedCleanup();
            }
          }
          const { error } = await result;
          launchError = error;
          assert.ok(error instanceof CodexLaunchError, "rejection is minted by the production launcher");
          assert.equal(error.classification, scenario.classification);
          if (scenario.mode === "hard") {
            assert.equal(error.cleanupStatus, "verified");
            assert.equal(hasVerifiedStartupCleanup(error, authorization), true);
            child.assertClosed();
          } else {
            assert.equal(error.cleanupStatus, "unconfirmed");
            assert.equal(hasVerifiedStartupCleanup(error, authorization), false);
          }
          if (scenario.mode === "errno") assert.equal(error.cause, spawnError);
          if (scenario.mode === "posture" || scenario.mode === "protocol") {
            assert.ok(String(error.cause).includes(evidence), "private raw cause remains authentic");
          }
          throw error;
        },
      });
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        try {
          commitInTree(ctx.worktreePath, "M1.txt", "fatal startup checkpoint\n");
          await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
          fs.mkdirSync(path.join(fx.originPath, ".github", "workflows"), { recursive: true });
          commitInTree(fx.originPath, ".github/workflows/ci.yml", "name: test\non: push\n# default advanced\n");
          armedTarget = true;
        } catch (error) { setupFailure = error; throw error; }
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: scenario.sink });
        checkpointReturned = true;
        return { branch: ctx.branch };
      });
      const claim = gitlabClaim(247560 + index, {
        repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
      });
      const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, {
        github, codexBoundaryDeadlineMs: 60_000, checkpointIntervalMs: 0, checkpointTickIntervalMs: 0,
        checkpointTestHooks: {
          softDeadlineMs: 10_000,
          armSoftDeadline: (fire, ms) => {
            softCallback = fire;
            return { deadlineAt: Date.now() + ms, cancel: () => { softCallback = undefined; } };
          },
        },
      });
      try { await runner.execute(claim); }
      finally { gitLog.log = originalGitLog; transport?.destroy(); }
      assert.equal(setupFailure, undefined);
      startupTimer?.assertCancelled();
      assert.equal(injections, 1);
      assert.ok(launchError instanceof CodexLaunchError);
      assert.equal(softFired, 1, "actual soft callback fired after spawn");
      assert.equal(hard.fired(), scenario.mode === "hard" ? 1 : 0);
      assert.equal(checkpointReturned, false, "fatal rejection cannot become a soft skip");
      assert.equal(rig.registry.isPoisoned(), true);
      assert.ok(statuses(claim.run_id).includes("failed"));
      assert.ok(!statuses(claim.run_id).includes("completed"));
      const failed = api.states.filter((state) => state.runId === claim.run_id && state.body.status === "failed");
      assert.ok(failed.length > 0);
      for (const state of failed) {
        const reason = String(state.body.failure_reason);
        assert.ok(reason.includes(scenario.classification), reason);
        assert.ok(reason.length <= 512, "Runner MAX_FAILURE_REASON_LEN");
      }
      const diagnostic = lines.find((line) => (line as { msg?: string }).msg === "codex boundary failed") as
        { detail?: string } | undefined;
      assert.ok(diagnostic?.detail, JSON.stringify(lines));
      assert.ok(diagnostic.detail.includes(scenario.classification));
      assert.ok(diagnostic.detail.length <= 500, "CODEX_BOUNDARY_DIAGNOSTIC_MAX_CHARS");
      const published = JSON.stringify({ logs: lines, failed });
      for (const sentinel of [privatePath, provider, evidence]) assert.ok(!published.includes(sentinel), sentinel);
    });
  }
});

describe("RunRunner M2 positive rejected startup acceptance", () => {
  const cases = [
    { target: "fetch", fire: true, sink: "milestone_checkpoint" },
    { target: "fetch", fire: false, sink: "milestone_checkpoint" },
    { target: "pack", fire: true, sink: "milestone_checkpoint" },
    { target: "pack", fire: false, sink: "milestone_checkpoint" },
    { target: "pack", fire: false, sink: "done_checkpoint" },
    { target: "owed", fire: false, sink: "milestone_checkpoint" },
  ] as const;
  for (const [index, scenario] of cases.entries()) {
    it(`${scenario.target} ${scenario.sink}: verified rejected startup with soft callback ${scenario.fire ? "fired" : "pending"}`, async () => {
      const { gitlab } = fakeGitlab();
      const { github } = fakeGitHub();
      const { logger, lines } = recordingLogger();
      const gitLog = git as unknown as { log: Logger };
      const originalGitLog = gitLog.log;
      gitLog.log = logger;
      let softCallback: (() => void) | undefined;
      let softFired = false;
      let injections = 0;
      let armedTarget = false;
      let owedActive = false;
      let uploads = 0;
      let acks = 0;
      let firstReturned = false;
      let retryReturned = false;
      let turnFailure: unknown;
      let transport: RejectedStartupTransport | undefined;
      const events: string[] = [];
      const publishedStates: Array<{ lastPublishedTip?: string; checkpointFloor?: string }> = [];
      type PublicationState = {
        lastPublishedTip?: string; checkpointFloor?: string;
        lastCheckpointRefTip?: string; landedCheckpoint?: boolean;
      };
      type FlightObservation = PublicationState & { batcher: { flush: () => Promise<void> } };
      let observedFlight: FlightObservation | undefined;
      let lockFailure: unknown;
      const startupTimer = manualStartedDeadline();
      const snapshot = (): PublicationState => {
        assert.ok(observedFlight, "the Runner checkpoint owns the observed flight");
        return {
          lastPublishedTip: observedFlight.lastPublishedTip,
          checkpointFloor: observedFlight.checkpointFloor,
          lastCheckpointRefTip: observedFlight.lastCheckpointRefTip,
          landedCheckpoint: observedFlight.landedCheckpoint,
        };
      };
      const rig = codexRig({
        boundaryProcess: async (request, startupMs, startupCleanup) => {
          if (!armedTarget || injections !== 0) return undefined;
          const selected = scenario.target === "fetch"
            ? request.identity === "worker_pat" && request.argv.includes("fetch") && request.argv.includes("origin")
            : scenario.target === "pack"
              ? request.argv.includes("pack-objects")
              : owedActive && request.argv.includes("for-each-ref");
          if (!selected) return undefined;
          injections += 1;
          assert.equal(request.recoverableTimeout, true);
          assert.ok(startupCleanup);
          assert.ok(startupMs > 0 && startupMs <= request.timeoutMs!);
          assert.ok(startupCleanup.hardDeadlineAt - Date.now() > 40_000,
            "startup uses the soft residual, with ample original hard budget for cleanup");
          const seamAuthorization = startupCleanup;
          const hardDeadlineAt = startupCleanup.hardDeadlineAt;
          const attempt = startupCleanup.attempt;
          transport = new RejectedStartupTransport(request.identity);
          const childTransport = transport;
          const spawner = boundaryProcessSpawnerForTest("required", (spec, launchMs, authorization) => {
            assert.equal(launchMs, startupMs, "spawner forwards the exact safety startup budget");
            assert.equal(authorization, seamAuthorization, "spawner preserves the exact attempt object");
            assert.equal(authorization!.attempt, attempt);
            assert.equal(authorization!.hardDeadlineAt, hardDeadlineAt, "hard deadline is never rebased");
            assert.equal(spec.identity, request.identity);
            // Drive the real startup expiry after arming, retaining its exact supplied residual.
            return launchCodexEffectRoot(spec, {
              env: { UZI_UID_SPLIT: "1" },
              resolveWorkerUid: () => WORKER_UID, resolveCommandUid: () => COMMAND_UID,
              startupCleanup: authorization, deadlines: { started: launchMs },
              armStartedDeadlineForTest: startupTimer.arm,
              spawnSupervisor: () => childTransport as unknown as SupervisorProcess,
            });
          });
          const launch = spawner(request, startupMs, startupCleanup);
          // Observe rejection immediately to avoid an unhandled promise if a test assertion fails.
          let launchSettled = false;
          const result = launch.then(
            () => { launchSettled = true; return { error: undefined }; },
            (error: unknown) => { launchSettled = true; return { error }; },
          );
          assert.equal(await startupTimer.installed, startupMs, "exact supplied startup residual");
          startupTimer.expire();
          await new Promise<void>(resolve => setImmediate(resolve));
          assert.ok(childTransport.events.includes("dispose"), "startup expiry must request disposal before cleanup");
          await childTransport.disposeRequested;
          assert.equal(launchSettled, false, "launcher must remain pending while cleanup is held");
          events.push("cleanup_held");
          assert.equal(rig.registry.pendingLaunchCount(), 1,
            "the reservation remains held while real rejected-startup cleanup is pending");
          assert.equal(rig.registry.isPoisoned(), false);
          assert.equal(firstReturned, false);
          assert.ok(!events.includes("lock_release"), "the bare lock cannot release during cleanup");
          assert.ok(softCallback, "the manual soft callback is still armed");
          if (scenario.fire) { softFired = true; softCallback(); }
          assert.equal(softFired, scenario.fire);
          childTransport.releaseVerifiedCleanup();
          const { error } = await result;
          assert.ok(error instanceof CodexLaunchError, "the real launcher rejects rather than returning a handle");
          assert.equal(error.classification, "started_deadline");
          assert.equal(error.cleanupStatus, "verified");
          assert.equal(hasVerifiedStartupCleanup(error, seamAuthorization), true);
          childTransport.assertClosed();
          events.push("verified_rejection");
          throw error;
        },
      });
      const originalScope = git.withBoundaryProcessSpawner;
      git.withBoundaryProcessSpawner = ((spawner, signal, action, hooks) =>
        originalScope.call(git, spawner, signal, action, {
          ...hooks,
          beforeLockRelease: async (key) => {
            await hooks?.beforeLockRelease?.(key);
            if (injections && !events.includes("lock_release")) {
              try {
                transport!.assertClosed();
                assert.ok(events.includes("verified_rejection"));
                assert.equal(rig.registry.pendingLaunchCount(), 0);
                assert.equal(rig.registry.isPoisoned(), false);
                events.push("lock_release");
              } catch (error) { lockFailure = error; throw error; }
            }
          },
        })) as typeof git.withBoundaryProcessSpawner;
      const originalReconcile = git.reconcileOwedCandidates;
      git.reconcileOwedCandidates = async (...args) => {
        owedActive = acks > 0;
        try { return await originalReconcile.apply(git, args); }
        finally { owedActive = false; }
      };
      const originalPublish = client.publishCheckpoint;
      client.publishCheckpoint = (async (_runId: string, _tip: string, pack: Readable) => {
        uploads += 1;
        await drain(pack);
        acks += 1;
        return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/positive-startup" } };
      }) as typeof client.publishCheckpoint;
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        try {
          commitInTree(ctx.worktreePath, "M1.txt", "actual rejected startup cleanup\n");
          const committedTip = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { env: GIT_ENV }).toString().trim();
          await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
          const before = snapshot();
          fs.mkdirSync(path.join(fx.originPath, ".github", "workflows"), { recursive: true });
          commitInTree(fx.originPath, ".github/workflows/ci.yml", "name: test\non: push\n# default advanced\n");
          armedTarget = true;
          await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: scenario.sink });
          firstReturned = true;
          assert.equal(injections, 1);
          transport!.assertClosed();
          assert.deepEqual(events, scenario.target === "pack"
            ? ["cleanup_held", "verified_rejection"]
            : ["cleanup_held", "verified_rejection", "lock_release"]);
          assert.equal(rig.registry.pendingLaunchCount(), 0);
          assert.equal(rig.registry.isPoisoned(), false);
          assert.equal(softFired, scenario.fire, "pending recovery does not depend on firing the soft callback");
          assert.equal(softCallback, undefined, "checkpoint cancels its manual callback");
          assert.ok(api.states.some((state) => state.runId === ctx.runId && state.body.status === "running" &&
            state.body.milestones_completed?.includes("m1")), "local milestone completion remains reported");
          if (scenario.target === "owed") {
            assert.equal(acks, 1, "broker publication stays confirmed after later owed-Git startup recovery");
            assert.equal(uploads, 1);
            assert.equal(publishedStates.length, 1);
            assert.ok(snapshot().lastCheckpointRefTip);
            assert.equal(snapshot().landedCheckpoint, true);
            assert.ok(lines.some((line) => (line as { msg?: string }).msg === "owed reconciliation failed; extra pins retained"));
          } else {
            assert.equal(uploads, 0, "no rejected-startup pack reaches the broker");
            assert.equal(acks, 0);
            assert.deepEqual(publishedStates, []);
            assert.deepEqual(snapshot(), before, "unpublished floor and publication bookkeeping stay unchanged");
            assert.ok(lines.some((line) => (line as { msg?: string }).msg === "checkpoint publish skipped: soft deadline"),
              "Runner records the soft-skip outcome");
            await observedFlight!.batcher.flush();
            assert.ok(feedTexts(ctx.runId).some((line) => line.includes("checkpoint publish skipped: soft deadline")));
            assert.ok(!feedTexts(ctx.runId).some((line) => /checkpoint publishing recovered|published to origin/.test(line)));
            await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] }, sink: scenario.sink });
            retryReturned = true;
            assert.equal(uploads, 1, "only the successful retry uploads a pack");
            assert.equal(acks, 1);
            assert.equal(publishedStates.length, 1);
            assert.equal(execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { env: GIT_ENV }).toString().trim(), committedTip,
              "retry publishes the same committed work without a new commit");
          }
          assert.ok(lines.some((line) => {
            const entry = line as { msg?: string; classification?: string; cleanup?: string };
            return entry.msg === "checkpoint child startup timeout recovered" &&
              entry.classification === "started_deadline" && entry.cleanup === "verified";
          }), "worker Git logger records the fixed recovery classification");
          return { branch: ctx.branch };
        } catch (error) { turnFailure = error; throw error; }
      });
      const claim = gitlabClaim(247500 + index, {
        repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
      });
      const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, {
        github, codexBoundaryDeadlineMs: 60_000, checkpointIntervalMs: 0, checkpointTickIntervalMs: 0,
        checkpointTestHooks: {
          softDeadlineMs: 10_000,
          armSoftDeadline: (fire, ms) => {
            softCallback = fire;
            return { deadlineAt: Date.now() + ms, cancel: () => { softCallback = undefined; } };
          },
          afterUnpinnedPublish: (state) => publishedStates.push(state),
        },
      });
      // Observe bookkeeping without replacing any publication or recovery behavior.
      const runnerSeam = runner as unknown as {
        runGatedSink: <T>(flight: FlightObservation, action: () => Promise<T>) => Promise<T>;
      };
      const originalSink = runnerSeam.runGatedSink;
      runnerSeam.runGatedSink = <T>(flight: FlightObservation, action: () => Promise<T>): Promise<T> => {
        observedFlight = flight;
        return originalSink.call(runner, flight, action) as Promise<T>;
      };
      try { await runner.execute(claim); }
      finally {
        git.withBoundaryProcessSpawner = originalScope;
        git.reconcileOwedCandidates = originalReconcile;
        client.publishCheckpoint = originalPublish;
        gitLog.log = originalGitLog;
        transport?.destroy();
      }
      assert.equal(turnFailure, undefined, `executor assertions: ${String(turnFailure)}`);
      assert.equal(lockFailure, undefined, `lock-release assertion: ${String(lockFailure)}`);
      startupTimer.assertCancelled();
      assert.equal(firstReturned, true, `checkpoint returned after verified cleanup: ${String(turnFailure)}`);
      assert.equal(retryReturned, scenario.target !== "owed", `retry result: ${String(turnFailure)}`);
      assert.ok(statuses(claim.run_id).includes("completed"), JSON.stringify({ states: api.states, logs: lines.filter((line) => (line as { level?: string }).level === "error") }));
      assert.ok(!statuses(claim.run_id).includes("failed"));
      assert.equal(rig.registry.isPoisoned(), false);
      assert.equal(rig.registry.pendingLaunchCount(), 0);
    });
  }
});
