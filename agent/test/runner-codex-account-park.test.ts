import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { CodexSessionStore } from "../src/codex/session-state.js";
import { RequestError } from "../src/client.js";
import { ResidueQuarantinedError, latchResidueQuarantine } from "../src/residue-quarantine.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import type { CredentialFreeSettleOutcome } from "../src/executor.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import {
  api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith, worktreeDirFor,
} from "./runner-harness.js";
import {
  commitInTree, codexRig, FakeCodexExecutor, statuses, feedTexts, parkReports,
  spyCustodySettle, workThenDefer, assertCaptured, spyPublishLands, sessionKept, enabledRecovery,
  VAULT_BODY_SENTINEL, installRecoveryRootCleanup,
} from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();
const FEATURE = "recovery_cause_codex_account_unavailable";
const REPORT_FAILED = "This run's Codex account was unavailable. Could not record the pause yet; keeping the local work and session and retrying.";

function observeWaits(runner: object): number[] {
  const waits: number[] = [];
  const r = runner as { waitRecoveryRetry: (flight: unknown, cancelStopsWait: boolean, ms: number) => Promise<void> };
  const original = r.waitRecoveryRetry.bind(runner);
  r.waitRecoveryRetry = async (flight, cancelStopsWait, ms) => {
    waits.push(ms);
    return original(flight, cancelStopsWait, ms);
  };
  return waits;
}

describe("M2 account park W3-W6/W9", () => {
  for (const sink of ["finalize", "checkpoint", "done_checkpoint"] as const) {
    for (const advertised of [true, false]) {
      it(`W3/W4: ${sink} captures committed-not-fetched and uncommitted work; feature=${advertised}`, async () => {
        client.protocolFeatures = ["claim_generation_fence", "run_checkpoint_durability", ...(advertised ? [FEATURE] : [])];
        const { gitlab, calls } = fakeGitlab();
        const rig = codexRig({ accountUnavailable: () => true });
        const events: string[] = [];
        const { logger, lines } = recordingLogger();
        const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
          commitInTree(ctx.worktreePath, "COMMITTED.txt", "committed, not yet fetched back\n");
          fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "uncommitted edit\n");
          if (sink !== "finalize") await ctx.checkpoint!({
            reap: true, progress: { completed: ["m1"], in_progress: [] },
            sink: sink === "checkpoint" ? "milestone_checkpoint" : "done_checkpoint",
          });
          return { branch: ctx.branch };
        }, async (ms) => { events.push("settle"); return rig.settle(ms); });
        const iid = 159500 + ["finalize", "checkpoint", "done_checkpoint"].indexOf(sink) * 2 + Number(advertised);
        const claim = gitlabClaim(iid, { claim_generation: 3 });
        const report = client.reportState.bind(client);
        client.reportState = async (id, body, signal) => {
          if (body.status === "running") events.push("confirm");
          if (body.status === "recovery_wait") {
            events.push("park");
            assertCaptured(iid);
          }
          return report(id, body, signal);
        };
        const restore = spyPublishLands();
        try {
          const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, { recoveryRetryMs: 1 });
          const custody = spyCustodySettle(runner);
          await runner.execute(claim);
          const parks = parkReports(claim.run_id);
          assert.equal(parks.length, 1);
          assert.equal(parks[0]!.recovery_cause, advertised ? "codex_account_unavailable" : undefined);
          assert.equal(parks[0]!.claim_generation, 3);
          assert.equal(parks[0]!.checkpoint_contains_latest, true);
          assertCaptured(iid);
          assert.ok(events.lastIndexOf("confirm") < events.indexOf("settle"));
          assert.ok(events.indexOf("settle") < events.indexOf("park"));
          assert.deepEqual(rig.boundaries, [sink === "finalize" ? "finalize" : "checkpoint"]);
          assert.equal(rig.refreshCalls(), 1);
          assert.equal(rig.releaseCalls(), 0);
          assert.equal(custody(), 0, "ACK keeps custody without reapThenSettle");
          assert.equal(calls.length, 0);
          assert.equal(api.completionPermitRequests.length, 0);
          assert.ok(!statuses(claim.run_id).some((s) => s === "failed" || s === "completed"));
          assert.ok(feedTexts(claim.run_id).some((s) => s.includes("Codex account was unavailable") && s.includes("checkpoint is published")));
          assert.doesNotMatch(JSON.stringify({ lines, states: api.states, feed: api.messages(claim.run_id) }), /vault|vault-body-sentinel|operation_id/i);
        } finally { restore(); }
      });
    }
  }

  for (const exit of ["paused", "wall", "superseded"] as const) {
    it(`W6: ${exit} confirms precedence without park or terminal report`, async () => {
      client.protocolFeatures = [FEATURE];
      const { gitlab, calls } = fakeGitlab();
      const rig = codexRig({ accountUnavailable: () => true });
      const w = workThenDefer(rig);
      const iid = 159520 + ["paused", "wall", "superseded"].indexOf(exit);
      const claim = gitlabClaim(iid, { claim_generation: 3 });
      if (exit === "superseded") {
        api.setOwnershipStatus(claim.run_id, "running", 4);
        api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { httpStatus: 400 });
      } else {
        api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", exit === "wall" ? {
          runStatus: "paused", holdReason: "budget_exhausted", disposition: "stale_claim",
        } : { runStatus: "paused" });
      }
      const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
      const custody = spyCustodySettle(runner);
      await runner.execute(claim);
      assert.equal(parkReports(claim.run_id).length, 0);
      assert.ok(!statuses(claim.run_id).some((s) => s === "failed" || s === "completed"));
      assert.equal(calls.length, 0);
      assert.equal(custody(), 0);
      assert.equal(api.completionPermitRequests.length, 0);
      assert.equal(rig.refreshCalls(), 1);
      if (exit === "paused") {
        assertCaptured(iid);
        assert.ok(fs.existsSync(worktreeDirFor(iid)));
        assert.ok(feedTexts(claim.run_id).some((s) => s.includes("Codex account was unavailable") && s.includes("not running")));
      }
      if (exit === "wall") assert.equal(fs.readFileSync(path.join(worktreeDirFor(iid), "DIRTY.txt"), "utf8"), "uncommitted edit\n");
      assert.doesNotMatch(JSON.stringify(feedTexts(claim.run_id)), /vault/i);
    });
  }

  it("W6: owner cancel captures first and retries cancellation without credentialed custody settlement", async () => {
    client.protocolFeatures = [FEATURE];
    const { gitlab } = fakeGitlab();
    const { coord, archive } = enabledRecovery();
    const rig = codexRig({ accountUnavailable: () => true });
    let w: ReturnType<typeof workThenDefer>;
    let ready!: () => void;
    const entered = new Promise<void>((resolve) => { ready = resolve; });
    let attempts = 0;
    w = workThenDefer(rig, async (ms) => {
      if (!w.ctx()?.cancelRequested?.()) {
        if (++attempts === 2) ready();
        return { kind: "incomplete", errors: [{ category: "timeout", message: "writer alive" }] };
      }
      return rig.settle(ms);
    });
    const claim = gitlabClaim(159523);
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 1, recovery: coord,
    });
    const custody = spyCustodySettle(runner);
    let cancelled = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (id, body, signal) => {
      if (body.status === "failed" && body.failure_reason === "run cancelled") {
        assertCaptured(159523);
        if (++cancelled === 1) throw new Error("cancellation lost");
      }
      return report(id, body, signal);
    };
    const execution = runner.execute(claim);
    try {
      await Promise.race([entered, execution.then(() => assert.fail("ended before cancellation"))]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;
      assert.equal(cancelled, 2);
      assert.equal(parkReports(claim.run_id).length, 0);
      assert.equal(custody(), 0);
      assert.equal(archive.releaseCalls.length, 0);
      assert.equal(rig.refreshCalls(), 1);
      assert.deepEqual(rig.boundaries, ["finalize"]);
      assert.ok(feedTexts(claim.run_id).some((s) => s.startsWith("Could not record the cancellation")));
      assert.doesNotMatch(JSON.stringify(feedTexts(claim.run_id)), /vault/i);
    } finally { runner.shutdown(); await execution; }
  });

  it("W6: shutdown during unsettled capture retains clone/session/custody and reports nothing", async () => {
    client.protocolFeatures = [FEATURE];
    const { gitlab } = fakeGitlab();
    const { coord, archive } = enabledRecovery();
    const rig = codexRig({ accountUnavailable: () => true });
    const { logger, lines } = recordingLogger();
    let ready!: () => void;
    const entered = new Promise<void>((resolve) => { ready = resolve; });
    const w = workThenDefer(rig, async () => {
      ready();
      return { kind: "incomplete", errors: [{ category: "timeout", message: "writer alive" }] };
    });
    const claim = gitlabClaim(159524);
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 1, recovery: coord });
    const custody = spyCustodySettle(runner);
    const execution = runner.execute(claim);
    try {
      await Promise.race([entered, execution.then(() => assert.fail("ended before shutdown"))]);
      runner.shutdown();
      await execution;
      assert.equal(parkReports(claim.run_id).length, 0);
      assert.ok(!statuses(claim.run_id).some((s) => s === "failed" || s === "completed"));
      assert.ok(fs.existsSync(path.join(worktreeDirFor(159524), "DIRTY.txt")));
      assert.ok(sessionKept(lines));
      assert.equal(custody(), 0);
      assert.equal(archive.releaseCalls.length, 0);
      assert.equal(rig.refreshCalls(), 1);
    } finally { runner.shutdown(); await execution; }
  });

  it("W6: residue refusal during capture fails worker_residue_blocked with custody kept", async () => {
    client.protocolFeatures = [FEATURE];
    const { gitlab } = fakeGitlab();
    const { coord, archive } = enabledRecovery();
    const rig = codexRig({ accountUnavailable: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(159525);
    git.fetchAgentBranch = async () => {
      latchResidueQuarantine({ cause: "fixture residue", runId: claim.run_id, site: "recovery_capture" }, nullLogger());
      throw new ResidueQuarantinedError("git", "fixture residue");
    };
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1, recovery: coord });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.fail_origin === "worker_residue_blocked"));
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.equal(custody(), 0);
    assert.equal(archive.releaseCalls.length, 0);
    assert.ok(fs.existsSync(path.join(worktreeDirFor(159525), "DIRTY.txt")));
    assert.equal(rig.refreshCalls(), 1);
  });

  it("W4: plain legacy 409 remains a failure without account inference", async () => {
    client.protocolFeatures = [FEATURE];
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ refuseReconcile: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(159510);
    await runnerWith(() => ({ executor: w.exec }), gitlab).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.equal(calls.length, 0);
    assert.equal(rig.refreshCalls(), 1);
  });

  it("W9: repeated rollback 400s retain custody/source/session with deduplicated feed and capped waits", async () => {
    client.protocolFeatures = [FEATURE];
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ accountUnavailable: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(159511, { claim_generation: 3 });
    const session = randomUUID();
    const epoch = path.join(homeDir, "model-session");
    const store = path.join(homeDir, "codex-session-store");
    const rollout = "rollout-" + session + ".jsonl";
    const contents = JSON.stringify({ type: "session_meta", payload: { id: session } }) + "\n";
    fs.mkdirSync(path.join(epoch, "sessions"), { recursive: true });
    fs.writeFileSync(path.join(epoch, "sessions", rollout), contents);
    const run = w.exec.run.bind(w.exec);
    // The fd-anchored session store needs Linux /proc/self/fd. Elsewhere the retry, rollback,
    // custody and feed assertions still run; only the physical-session checks are Linux-only.
    const physicalSession = process.platform === "linux";
    w.exec.run = async (ctx) => {
      ctx.onSessionId?.(session);
      if (physicalSession) await CodexSessionStore.persist(epoch, store);
      return run(ctx);
    };
    const assertSession = async (): Promise<void> => {
      if (!physicalSession) return;
      assert.equal(await CodexSessionStore.inspectSession(store, session), "present");
      const generation = fs.readFileSync(path.join(store, "current"), "utf8").trim();
      assert.equal(fs.readFileSync(path.join(store, "generations", generation, "sessions", rollout), "utf8"), contents);
    };
    const { logger } = recordingLogger();
    const runner = runnerWith(() => ({ executor: w.exec, homeDir }), gitlab, undefined, logger, { recoveryRetryMs: 1 });
    const custody = spyCustodySettle(runner);
    const waits = observeWaits(runner);
    let reports = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (id, body, signal) => {
      if (body.status === "recovery_wait") {
        assert.equal(body.recovery_cause, "codex_account_unavailable");
        assertCaptured(159511);
        assert.ok(fs.existsSync(worktreeDirFor(159511)));
        await assertSession();
        assert.equal(body.session_id, session);
        assert.equal(custody(), 0);
        assert.ok(!statuses(claim.run_id).some((s) => s === "failed" || s === "completed"));
        if (++reports <= 7) {
          try {
            throw new RequestError("POST", "/state", 400, "unknown cause");
          } catch (error) {
            await assertSession(); // Inspect after this rejected report, before the next retry.
            throw error;
          }
        }
        const ack = await report(id, body, signal);
        await assertSession();
        return ack;
      }
      return report(id, body, signal);
    };
    const restore = spyPublishLands();
    try {
      await runner.execute(claim);
      await assertSession();
      assert.equal(reports, 8);
      assert.deepEqual(waits, [1, 2, 4, 8, 16, 16, 16]);
      assert.equal(feedTexts(claim.run_id).filter((s) => s === REPORT_FAILED).length, 1);
      assert.equal(parkReports(claim.run_id).length, 1);
      assert.equal(custody(), 0);
      assert.equal(calls.length, 0);
      assert.equal(rig.refreshCalls(), 1);
    } finally { restore(); }
  });

  it("W5/W6 capture-discovered account deferral replaces cause/feed and freshly confirms/settles before retry park", async () => {
    client.protocolFeatures = [FEATURE, "claim_generation_fence"];
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ accountUnavailable: () => true });
    const claim = gitlabClaim(159512, { claim_generation: 3 });
    const events: string[] = [];
    let settled = 0;
    let proofs = 0;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "COMMITTED.txt", "committed, not yet fetched back\n");
      fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "uncommitted edit\n");
      events.push("transient");
      throw new TransientRecoveryError();
    }, async (ms) => { settled++; events.push("settle"); return rig.settle(ms); });
    const { logger, lines } = recordingLogger();
    const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, {
      recoveryRetryMs: 1, checkpointIntervalMs: 0,
      quiesceRun: async (request) => {
        if (request.site === "recovery_capture") {
          proofs++;
          if (proofs > 1 && proofs <= 8) {
            assert.equal(settled, 1, "fresh credential-free settlement after discovered deferral");
            assert.equal(custody(), 0);
            assert.ok(fs.existsSync(path.join(worktreeDirFor(159512), "DIRTY.txt")));
            assert.ok(!statuses(claim.run_id).some((s) => ["failed", "completed", "recovery_wait"].includes(s)));
            return {
              process: { state: "unverified", processes: [], killed: [], detail: "capture proof blocked" },
              docker: { state: "not_wired", removed: [], detail: "" },
            };
          }
        }
        return {
          process: { state: "quiescent", processes: [], killed: [], detail: "" },
          docker: { state: "not_wired", removed: [], detail: "" },
        };
      },
    });
    const custody = spyCustodySettle(runner);
    const waits = observeWaits(runner);
    const report = client.reportState.bind(client);
    client.reportState = async (id, body, signal) => {
      if (events.includes("transient") && body.status === "running") {
        events.push("fresh-confirm");
        assert.equal(body.claim_generation, 3);
      }
      if (body.status === "recovery_wait") {
        events.push("park");
        assertCaptured(159512);
      }
      return report(id, body, signal);
    };
    // The first post-deferral confirm is unknown, to expose the replacement feed.
    const confirm = client.reportState.bind(client);
    let lostConfirm = false;
    client.reportState = async (id, body, signal) => {
      if (rig.refreshCalls() && body.status === "running" && !lostConfirm) {
        lostConfirm = true;
        throw new RequestError("POST", "/state", 400, "confirm lost");
      }
      return confirm(id, body, signal);
    };
    const ownership = client.getRunOwnership.bind(client);
    let lostOwnership = false;
    client.getRunOwnership = async (id) => {
      if (rig.refreshCalls() && !lostOwnership) { lostOwnership = true; throw new Error("probe lost"); }
      return ownership(id);
    };
    const restore = spyPublishLands();
    try {
      await runner.execute(claim);
      assert.ok(proofs > 8, "capture-discovered deferral survives old blocked cap");
      assert.equal(settled, 1);
      assert.ok(events.indexOf("fresh-confirm") > events.indexOf("transient"));
      assert.ok(events.indexOf("fresh-confirm") < events.indexOf("settle"));
      assert.ok(events.indexOf("settle") < events.indexOf("park"));
      assert.equal(parkReports(claim.run_id)[0]!.recovery_cause, "codex_account_unavailable", "cause replaced");
      const feed = feedTexts(claim.run_id);
      assert.ok(feed.some((s) => s.startsWith("This run's Codex account was unavailable. Could not confirm")), "feed replaced");
      assert.doesNotMatch(JSON.stringify(feed), /vault/i);
      assert.ok(waits.length >= 8);
      assert.ok(waits.every((ms) => ms <= 16));
      assert.equal(custody(), 0);
      assert.equal(calls.length, 0);
      assert.equal(rig.refreshCalls(), 1);
      assert.equal(rig.releaseCalls(), 0);
      assert.doesNotMatch(JSON.stringify(lines), new RegExp(VAULT_BODY_SENTINEL));
    } finally { restore(); }
  });

  it("W5: incomplete settle and failed fetch-back retain running work and heartbeat before verified park", async () => {
    client.protocolFeatures = [FEATURE];
    const { gitlab } = fakeGitlab();
    const rig = codexRig({ accountUnavailable: () => true });
    const claim = gitlabClaim(159513);
    let settles = 0;
    const settle = async (ms: number): Promise<CredentialFreeSettleOutcome> => {
      if (++settles <= 6) return { kind: "incomplete", errors: [{ category: "timeout", message: "writer alive" }] };
      return rig.settle(ms);
    };
    const w = workThenDefer(rig, settle);
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
    const custody = spyCustodySettle(runner);
    const waits = observeWaits(runner);
    const fetch = git.fetchAgentBranch.bind(git);
    let failures = 0;
    git.fetchAgentBranch = async (...args: Parameters<typeof fetch>) => {
      if (w.deferred() && failures++ < 6) {
        assert.equal(parkReports(claim.run_id).length, 0);
        assert.equal(custody(), 0);
        assert.ok(fs.existsSync(path.join(worktreeDirFor(159513), "DIRTY.txt")));
        throw new Error("fetch-back unavailable");
      }
      return fetch(...args);
    };
    const restore = spyPublishLands();
    try {
      await runner.execute(claim);
      assert.equal(settles, 7);
      assert.ok(waits.every((ms) => ms <= 16));
      assert.ok(waits.includes(16));
      assertCaptured(159513);
      assert.equal(parkReports(claim.run_id).length, 1);
      assert.equal(custody(), 0);
      assert.equal(rig.refreshCalls(), 1);
      assert.ok(!statuses(claim.run_id).includes("failed"));
    } finally { restore(); }
  });
});
