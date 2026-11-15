import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { it } from "node:test";
import { SourceUseHeldRefusal, acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { ForeignCaptureBlockedError, GitCache, recoveryCaptureKey } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { RUN_REGISTRATION_EVIDENCE_FEATURE } from "../src/protocol.js";
import { testLockOptions } from "./authority-test-support.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// These are runner/report regressions. FakeApi supplies wire ACKs, not PostgreSQL
// transactions; source_end_declined_livedb_test.go owns the M1 server proof.
const transports = ["exact", "missing-detail", "wrong-detail", "foreign-run", "foreign-worker",
  "stale", "missing-generation", "wrong-generation", "registration-rotation", "409", "lost-ACK", "400",
  "cancel-before-await", "current-ownership"] as const;
for (const site of ["early", "later-race", "phaseClone-reseed"] as const) {
  // Transport negatives share the real early refusal fixture; the three call
  // sites retain their independent admission coverage without a Cartesian suite.
  const cases = (["exact-declined", "retirement-only", "queued-end"] as const).flatMap(evidence =>
    transports.filter(transport => transport === "exact" || (site === "early" && evidence === "exact-declined"))
      .map(transport => ({ evidence, transport })));
  for (const { evidence, transport } of cases) {
    it(`source end refusal: ${site} / ${evidence} / ${transport}`, async t => {
      const token = randomUUID();
      const rt = new AuthorityRuntime({
        lifetime: await acquireWorkerLifetime(fx.dataDir, testLockOptions()),
        workerToken: token, timeoutMs: 5000,
      });
      const registry = new ActiveRunRegistry();
      const nonce = randomUUID();
      api.registerNonce = nonce;
      api.setRegisterProtocolFeatures([
        "claim_generation_fence", RUN_REGISTRATION_EVIDENCE_FEATURE, "retained_source_end_hold_v1",
        "recovery_cause_retained_recovery_adoption_failed",
      ]);
      await client.register("refusal-sites", "test", 1, undefined, [], registry.buildRegisterSnapshot(), 0);
      api.modelCrossCheckRunningRule = true;
      api.modelOrdinaryRunningRule = true;
      let holdSends = 0;
      const wireBodies: string[] = [];
      const fetch = globalThis.fetch;
      t.mock.method(globalThis, "fetch", async (...args: Parameters<typeof fetch>) => {
        const bytes = typeof args[1]?.body === "string" ? args[1].body : undefined;
        if (bytes && JSON.parse(bytes).recovery_detail === "source_end_declined") {
          wireBodies.push(bytes);
          holdSends++;
        }
        return fetch(...args);
      });
      api.stateAckWire = (body, run) => {
        if (body.status === "running" && body.registration_evidence)
          return { status: 200, body: { run, registration_evidence: "recorded" } };
        if (body.status === "recovery_wait") {
          if (transport === "400") return { status: 400, body: { error: "invalid request body" } };
          const held = { ...run, recovery_wait_cause: body.recovery_cause,
            ...(transport === "missing-detail" || transport === "current-ownership" ? {} : { recovery_wait_detail:
              transport === "wrong-detail" ? "remediation_proof_unknown" : body.recovery_detail }),
            ...(transport === "foreign-run" ? { id: randomUUID() } : {}),
            ...(transport === "foreign-worker" ? { worker_id: randomUUID() } : {}) };
          return { status: transport === "409" || transport === "stale" ? 409 : 200,
            body: { run: held, ...(transport === "stale" ? { disposition: "stale_claim" } : {}) } };
        }
        return undefined;
      };
      const iid = 436520;
      const claim = gitlabClaim(iid, { claim_generation: 1, plan_approved: true,
        plan_source: "seeded", plan_md: "# Plan\nKeep the wall source", session_id: null });
      let starts = 0;
      let source = "";
      let second = "";
      let oldSink: Set<string> | undefined;
      let pending: Awaited<ReturnType<typeof rt.hooks.beginDestructive>> | undefined;
      let runner = runnerWith(runId => ({
        homeDir: path.join(homeDir, runId),
        executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => {
          starts++;
          assert.equal(starts, 1, "the refused continuation must not start a model");
          source = ctx.worktreePath;
          fs.writeFileSync(path.join(source, "retained.txt"), "wall source\n");
          assert.equal(await ctx.parkForWall?.({ completedCount: 0 }), "parked");
          return { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } };
        } },
      }), fakeGitlab({}).gitlab, undefined, undefined, { authority: rt, activeRuns: registry });
      type Flight = { sourceUsePaths?: Set<string>; sourceTailsJoined?: boolean };
      let internals = runner as unknown as {
        endFlightSourceUse(f: Flight, log: unknown): Promise<unknown>;
        admitPredecessorSource(...args: unknown[]): Promise<void>;
        runnerCloneForClaim(...args: unknown[]): Promise<unknown>;
        reclaimTerminalOrphan(...args: unknown[]): Promise<void>;
        disposeHeldSource(...args: unknown[]): Promise<never>;
      };
      const end = internals.endFlightSourceUse.bind(runner);
      let ends = 0;
      t.mock.method(internals, "endFlightSourceUse", async (flight: Flight, log: unknown) => {
        if (ends++ === 0) {
          oldSink = flight.sourceUsePaths!;
          // A second genuinely admitted source shares the flight's no-end decision.
          second = path.join(fx.dataDir, "runner", "second-source");
          fs.mkdirSync(second, { recursive: true });
          await rt.recordAdmissions(oldSink, () => rt.bindSource(second));
          if (evidence === "queued-end") {
            pending = await rt.recordAdmissions(oldSink, () =>
              rt.withTransaction(() => rt.hooks.beginDestructive([source, second], "runner-uid-delete")));
            flight.sourceTailsJoined = true;
          } else if (evidence === "retirement-only") {
            rt.retireSink(oldSink);
            assert.equal(await rt.awaitSinkQuiescent(), true);
            return;
          } else {
            flight.sourceTailsJoined = false;
          }
        }
        return end(flight, log);
      });
      await runner.execute(claim);
      assert.ok(oldSink);
      assert.equal(starts, 1);
      assert.equal(client.sourceEndDeclinedRegistration(claim.run_id, 1), nonce,
        "the wall flight recorded its registration witness");
      assert.equal(api.wallParkRequests.filter(r => r.runId === claim.run_id).length, 1);
      const retained = await git.discoverRetainedRecovery(fx.originPath,
        `agent/issue-${iid}`, `issue-${iid}`, claim.run_id);
      assert.ok(retained);
      assert.equal(retained.journal.clonePath, source);
      const before = await rt.peekSlot(source);
      const sibling = await rt.peekSlot(second);
      assert.equal(before?.state, "active");
      assert.equal(sibling?.state, "active");
      assert.equal(await rt.sourceEndDeclined(source, new Set()), evidence === "exact-declined");

      let admissionCalls = 0;
      let reclaimed = 0;
      let realHeldRefusals = 0;
      if (site === "later-race") {
        // Miss only the early advisory read. Restore the real predicate before
        // the authoritative admission, whose actual held refusal must be caught.
        const held = rt.heldByOtherSink.bind(rt);
        let advisory = true;
        t.mock.method(rt, "heldByOtherSink", (...args: Parameters<typeof held>) =>
          advisory && args[0] === source ? false : held(...args));
        const admit = internals.admitPredecessorSource.bind(runner);
        t.mock.method(internals, "admitPredecessorSource", async (...args: unknown[]) => {
          admissionCalls++;
          advisory = false;
          try { return await admit(...args); }
          catch (error) {
            assert.ok(error instanceof SourceUseHeldRefusal);
            assert.equal(error.blockedSourceKey, source);
            realHeldRefusals++;
            throw error;
          }
        });
      }
      if (site === "phaseClone-reseed") {
        // Same orphan classification/reclaim seams as runner-source-use-end.
        // The second clone call performs real source admission.
        const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
          authority: rt.hooks, authorityRequired: true, gitleaksBin: defaultGitleaksShim(),
        }));
        runner = new RunRunner(client, hooked, () => ({
          executor: { run: async () => { starts++; throw new Error("refused reseed launched a model"); } },
        }), nullLogger(), 20, undefined, {
          pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600,
          gitlab: fakeGitlab({}).gitlab, teardownTestDeps: portableTeardownTestDeps,
          authority: rt, activeRuns: registry, checkpointTestHooks: { retryBackoffMs: [0, 0] },
        });
        internals = runner as unknown as typeof internals;
        assert.equal(source, hooked.runnerClonePath(hooked.barePathFor(fx.originPath), `issue-${iid}`),
          "the real reseed must target the held source");
        // Model an unfenced terminal orphan, rather than the wall journal's
        // different owner (which correctly requests readmission).
        t.mock.method(hooked, "readOrphanJournalFence", async () => ({ state: "unfenced", retained: false }));
        t.mock.method(hooked, "discoverRetainedRecovery", async () => undefined);
        t.mock.method(client, "getRunOrphanClassification", async () => ({
          status: "completed", repo_id: claim.repo.id, kind: "issue", issue_iid: iid,
          branch: `agent/issue-${iid}`,
        }));
        t.mock.method(hooked, "classifyOwnerClonePath", async () => "canonical");
        t.mock.method(hooked, "terminalRetainedSnapshot", async () => undefined);
        t.mock.method(hooked, "retireRunnerClone", async () => {
          // Model successful orphan retirement in the metadata too. Unlike the
          // empty-source source-use-end4066 fixture, this test first parked real
          // work: leaving its journal makes runnerCloneForBranch throw
          // PendingRecoveryCaptureError before the real seed admission.
          // Keep both physical sources and their authority slots untouched.
          execFileSync("git", ["config", "--local", recoveryCaptureKey(`agent/issue-${iid}`), ""], {
            cwd: hooked.barePathFor(fx.originPath), timeout: 5000,
          });
          return "quarantined";
        });
        const reclaim = internals.reclaimTerminalOrphan.bind(runner);
        t.mock.method(internals, "reclaimTerminalOrphan", async (...args: unknown[]) => {
          await reclaim(...args);
          reclaimed++;
        });
        const clone = internals.runnerCloneForClaim.bind(runner);
        t.mock.method(internals, "runnerCloneForClaim", async (...args: unknown[]) => {
          if (++admissionCalls === 1)
            throw new ForeignCaptureBlockedError(source, `agent/issue-${iid}`, randomUUID());
          assert.equal(reclaimed, 1, "reseed follows successful reclaim, not readmission");
          try { return await clone(...args); }
          catch (error) {
            assert.ok(error instanceof SourceUseHeldRefusal,
              `real reseed refused before held-source admission: ${String(error).slice(0, 400)}`);
            assert.equal(error.blockedSourceKey, source);
            realHeldRefusals++;
            throw error;
          }
        });
      }
      let dispositions = 0;
      let refusedFlight: Record<string, unknown> | undefined;
      const report = client.reportState.bind(client);
      t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
        const ack = await report(...args);
        if (args[1].recovery_detail === "source_end_declined") {
          // Generation is local sent-attempt evidence, not a server row field.
          if (transport === "missing-generation") delete ack.sentClaimGeneration;
          if (transport === "wrong-generation") ack.sentClaimGeneration = 1;
          if (transport === "registration-rotation") {
            api.registerNonce = randomUUID();
            await client.register("refusal-sites", "test", 1, undefined, [], registry.buildRegisterSnapshot(), 0);
          }
        }
        return ack;
      });
      const dispose = internals.disposeHeldSource.bind(runner);
      t.mock.method(internals, "disposeHeldSource", async (...args: unknown[]) => {
        dispositions++;
        assert.equal(args[2], source, "the disposition names the held source");
        refusedFlight = args[1] as Record<string, unknown>;
        if (transport === "cancel-before-await")
          (refusedFlight.cancel as AbortController).abort(new Error("cancel before held await"));
        if (transport === "current-ownership") api.setOwnershipStatus(claim.run_id, "running", 2);
        return dispose(...args);
      });
      if (transport === "lost-ACK") api.afterPersistState(claim.run_id,
        body => body.recovery_detail === "source_end_declined", "drop");
      const selectedBefore = await rt.peekSlot(source);
      api.setOwnershipStatus(claim.run_id, "claimed", 2);
      const crashStage = process.env.UZI_SOURCE_END_CRASH_STAGE;
      const manifest = process.env.UZI_SOURCE_END_CRASH_MANIFEST;
      if (crashStage && manifest && site === "early" && evidence === "exact-declined") {
        const crash = async (): Promise<never> => {
          fs.writeFileSync(manifest, JSON.stringify({
            pid: process.pid, stage: crashStage, token, dataDir: fx.dataDir,
            originPath: fx.originPath, homeDir, iid, runId: claim.run_id,
            sources: [source, second], slots: [await rt.peekSlot(source), await rt.peekSlot(second)],
            retained, starts, reports: api.states.filter(s =>
              s.body.recovery_detail === "source_end_declined"),
          }));
          process.exit(73); // Abrupt exit: no runner finally or fixture cleanup.
        };
        let holdAck = false;
        const report = client.reportState.bind(client);
        t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
          if (args[1].recovery_detail !== "source_end_declined") return report(...args);
          if (crashStage === "BeforeHoldCommit") return crash();
          const ack = await report(...args);
          if (crashStage === "AfterCommitBeforeAck") return crash();
          assert.equal(ack.status, "recovery_wait");
          assert.equal(ack.runId, claim.run_id);
          assert.equal(ack.sentClaimGeneration, 2);
          assert.equal(ack.recoveryCause, "retained_recovery_adoption_failed");
          assert.equal(ack.recoveryDetail, "source_end_declined");
          assert.equal(ack.workerIdentityMatches, true);
          holdAck = true;
          return ack;
        });
        const finish = internals.endFlightSourceUse.bind(runner);
        t.mock.method(internals, "endFlightSourceUse", async (flight: Flight, log: unknown) => {
          if (crashStage === "AfterAckBeforeEnd") {
            assert.equal(holdAck, true, "crash only after the exact hold ACK");
            return crash();
          }
          return finish(flight, log);
        });
      }
      try {
        await runner.execute(gitlabClaim(iid, { ...claim, claim_generation: 2 }));
        if (transport !== "registration-rotation") assert.equal(client.sourceEndDeclinedRegistration(claim.run_id, 2), nonce,
          "the refused generation recorded its own registration witness");
        if (site === "phaseClone-reseed") {
          const diagnostic = () => JSON.stringify({ admissionCalls, reclaimed, realHeldRefusals, dispositions,
            states: api.states.filter(s => s.runId === claim.run_id).slice(-6).map(s => ({
              status: s.body.status, detail: String(s.body.recovery_detail ?? "").slice(0, 200),
            })) });
          assert.equal(admissionCalls, 2, `phaseClone must reach the real reseed: ${diagnostic()}`);
          assert.equal(reclaimed, 1, `phaseClone must finish orphan reclaim: ${diagnostic()}`);
          assert.equal(realHeldRefusals, 1, `real seed admission must refuse the held source: ${diagnostic()}`);
          assert.equal(dispositions, 1, `reseed refusal must reach disposition: ${diagnostic()}`);
        }
        const reports = api.states.filter(s => s.runId === claim.run_id &&
          s.body.recovery_detail === "source_end_declined");
        const noSend = evidence !== "exact-declined" || transport === "cancel-before-await";
        if (noSend) assert.equal(holdSends, 0);
        else assert.ok(holdSends >= 1 && holdSends <= 2, "bounded exact hold attempts");
        if (transport === "exact") assert.equal(reports.length, evidence === "exact-declined" ? 1 : 0);
        assert.ok(refusedFlight);
        assert.equal(refusedFlight.parked === true, evidence === "exact-declined" &&
          (transport === "exact" || transport === "lost-ACK" || transport === "409"), "only an exact current ACK adopts parked state");
        for (const latch of ["retainedEpisodeCustody", "retainedLocalCustody", "preserveRecoveryClone",
          "preserveSession", "keepGuardedInventoryOpen"])
          assert.equal(refusedFlight[latch], true, latch);
        for (const bytes of wireBodies) assert.equal(bytes, JSON.stringify({
          status: "recovery_wait", recovery_cause: "retained_recovery_adoption_failed",
          claim_generation: 2, register_nonce: nonce, recovery_detail: "source_end_declined",
        }), "every send preserves exact detail, nonce and generation");
        if (transport === "lost-ACK") assert.equal(holdSends, 2, "lost ACK replays byte-identical hold");
        if (transport === "409" || transport === "400" || transport === "stale") assert.equal(holdSends, 1);
        if (["missing-detail", "wrong-detail", "foreign-run", "foreign-worker", "missing-generation",
          "wrong-generation", "current-ownership"].includes(transport)) assert.equal(holdSends, 2);
        if (transport === "registration-rotation") assert.notEqual(
          client.sourceEndDeclinedRegistration(claim.run_id, 2), nonce, "rotation invalidated the awaited witness");
        assert.equal(api.states.some(s => s.runId === claim.run_id && s.body.status === "recovery_wait" &&
          s.body.recovery_detail !== "source_end_declined"), false, "no generic recovery fallback");
        if (site === "early") assert.deepEqual(await git.discoverRetainedRecovery(fx.originPath,
          `agent/issue-${iid}`, `issue-${iid}`, claim.run_id), retained, "transport cannot consume retained journal");
        if (reports.length) {
          assert.equal(reports[0]?.body.status, "recovery_wait");
          assert.equal(reports[0]?.body.register_nonce, nonce);
          assert.equal(reports[0]?.body.claim_generation, 2);
        }
        assert.equal(dispositions, 1);
        assert.equal(realHeldRefusals, site === "early" ? 0 : 1);
        if (site === "later-race") assert.equal(admissionCalls, 1);
        if (site === "phaseClone-reseed") {
          assert.equal(admissionCalls, 2);
          assert.equal(reclaimed, 1);
        }
        assert.equal(starts, 1);
        assert.deepEqual(await rt.peekSlot(source), selectedBefore);
        assert.deepEqual(await rt.peekSlot(second), sibling);
        assert.equal(api.states.some(s => s.runId === claim.run_id &&
          (s.body.status === "failed" || s.body.status === "completed")), false);
        // A late race may lawfully reserve an attempt before admission. Do not
        // claim zero journal effects at that seam.
      } finally {
        if (pending) await rt.withTransaction(() => pending!.settle({ ok: true }));
      }
    });
  }
}
