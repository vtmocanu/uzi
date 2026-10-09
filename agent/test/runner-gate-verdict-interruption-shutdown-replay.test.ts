import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { execFileSync } from "node:child_process";
import path from "node:path";
import { SdkExecutor } from "../src/sdk-executor.js";
import { SteeringChannel } from "../src/steering.js";
import { StubExecutor } from "../src/executor.js";
import type { ClaimResponse, UserInput } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { forceIncompleteHomeHelper } from "./forced-home-helper.js";
import { scanRunProcesses, reapRunProcesses } from "../src/run-procs.js";
import { api, client, fx, git, installHarness } from "./runner-harness.js";
import {
  PLAN_V1,
  V1_MILESTONES,
  SID,
  promptly,
  STALE_APPROVE_NOTICE,
  REPLAY_STALE_VERDICT_NOTICE,
  REPLAY_UNJUDGED_NOTICE,
  revisedPlan,
  tick,
  until,
  type Flight,
  Scenario,
  scenario,
  assertDisposedApprove,
  assertNoApproval,
  releaseAtGate,
  assertHeldInputGet,
  pathB,
} from "./runner-gate-verdict-interruption-fixture.js";

installHarness();

describe("shutdown at an observed plan gate", () => {
  const gitEnv = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
  const commitWork = (clone: string): string => {
    fs.writeFileSync(path.join(clone, "SHUTDOWN-WORK.txt"), "committed before gate shutdown\n");
    execFileSync("git", ["-C", clone, "add", "SHUTDOWN-WORK.txt"], { env: gitEnv });
    execFileSync("git", ["-C", clone, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "keep gate work"], { env: gitEnv });
    return execFileSync("git", ["-C", clone, "rev-parse", "HEAD"], { env: gitEnv, encoding: "utf8" }).trim();
  };

  const readRetainedGateWork = (s: Scenario, flight: Flight, clone: string) => {
    const gate = s.gates(flight)[0];
    const failed = s.states(flight).find((state) => state.status === "failed");
    // A terminal flight also satisfies the readiness wait. It may have failed before a clone
    // existed, so require the gate before reading work and retain the primary failure reason.
    assert.ok(gate, `the resumed claim reached no plan gate: ${failed?.failure_reason ?? s.statuses(flight).join(",")}`);
    return { gate, content: fs.readFileSync(path.join(clone, "SHUTDOWN-WORK.txt"), "utf8") };
  };

  it("a reclaim refused before cloning reports its primary failure before reading retained work", {
    skip: process.platform !== "linux" ? "the HOME-attributed pre-clone reap is Linux-only" : false,
  }, (t) => scenario(async (s) => {
    const helper = forceIncompleteHomeHelper(t);
    const first = await s.toFirstGate();
    const bare = git.barePathFor(fx.originPath);
    const clone = git.runnerClonePath(bare, `issue-${s.base.issue_iid}`);
    const committedTip = commitWork(clone);
    await s.shutdown(first);
    const trackedTip = await git.trackingTip(bare, `agent/issue-${s.base.issue_iid}`);
    assert.ok(trackedTip, "the original work is captured before reclaim");
    execFileSync("git", ["-C", bare, "merge-base", "--is-ancestor", committedTip, trackedTip], { env: gitEnv });

    helper.enabled = true;
    const resumed = s.start(s.resumeClaim("none"), { executor: () => new SdkExecutor(nullLogger(), s.home, {
      queryFn: s.model.queryFn(), runProcesses: { scan: scanRunProcesses, reap: reapRunProcesses },
    }) });
    await s.finish(resumed);
    assert.ok(helper.calls > 0, "the real HOME helper received the forced incomplete result");
    assert.equal(s.gates(resumed).length, 0, "the failed reclaim presented no gate");
    assert.equal(s.states(resumed).find((state) => state.status === "failed")?.fail_origin, "worker_residue_blocked");
    assert.equal(fs.existsSync(clone), false, "the pre-clone refusal created no runner clone");
    assert.throws(() => readRetainedGateWork(s, resumed, clone), (error: unknown) =>
      error instanceof assert.AssertionError &&
      /reached no plan gate: worker_residue_blocked:.*no clone was fetched/.test(error.message),
    "the missing gate reports the primary refusal instead of a secondary ENOENT");
  }));

  it("wakes an idle gate on shutdown without a verdict or cancel input", () =>
    scenario(async (s) => {
      const flight = s.start(s.claim(), { runner: { planApprovalTimeoutMs: 0 } });
      assert.ok(await until(() => s.gates(flight).length > 0 || flight.finished), "the plan gate was observed");
      await s.shutdown(flight);
      assert.ok(!s.statuses(flight).some((status) => status === "failed" || status === "cancelled"));
      assert.equal(s.persistedGate()?.plan_md, PLAN_V1);
    }));

  for (const [planApprovalTimeoutMs, session, failHeldGet] of [[0, "kept", true], [60_000, "none", true], [0, "kept", false]] as const) {
    it(`finishes promptly without a cancel input when planApprovalTimeoutMs is ${planApprovalTimeoutMs} and the held GET ${failHeldGet ? "fails" : "succeeds"}`, (t) =>
      scenario(async (s) => {
        api.gateRevisions = true;
        api.stampGateBindings = true;
        client.protocolFeatures = ["claim_generation_fence", "gate_revision_v1"];
        let holdFirstGate = true;
        let gateReads = 0;
        let heldGet: ReturnType<typeof api.holdNextInputGet> | undefined;
        t.after(() => heldGet?.release());
        t.signal.addEventListener("abort", () => heldGet?.release(), { once: true });
        let stopped!: () => void;
        const pollingStopped = new Promise<void>((resolve) => { stopped = resolve; });
        const originalStop = SteeringChannel.prototype.stop;
        t.mock.method(SteeringChannel.prototype, "stop", function (this: SteeringChannel) {
          const drain = originalStop.call(this); // Sets stopped before awaiting its in-flight GET.
          stopped();
          return drain;
        });
        api.onState(s.runId, (body) => {
          if (body.status !== "awaiting_approval" || !holdFirstGate) return;
          holdFirstGate = false;
          gateReads = api.inputGets.get(s.runId) ?? 0;
          heldGet = api.holdNextInputGet(s.runId, gateReads);
          if (failHeldGet) api.failInputGets(s.runId, 1, 503);
        });
        const flight = s.start(s.claim(), { runner: { planApprovalTimeoutMs } });
        assert.ok(await until(() => s.gates(flight).length > 0 || flight.finished), "the plan gate was observed");
        const firstGate = s.gates(flight)[0]!;
        assert.equal(firstGate.plan_md, PLAN_V1, "the submitted plan was offered for approval");
        assert.ok(firstGate.presentation_id, "the observed gate has a presentation id");
        const persisted = api.gateOf(s.runId);
        assert.deepEqual(persisted, { revision: 1, presentationId: firstGate.presentation_id });
        const bare = git.barePathFor(fx.originPath);
        const clone = git.runnerClonePath(bare, `issue-${s.base.issue_iid}`);
        assert.ok(fs.existsSync(clone), "the gated runner clone exists");
        const committedTip = commitWork(clone);
        assert.equal(s.rowsOf("cancel").length, 0, "no cancel was sent before shutdown");

        // Hold a delivery read while the owner sends a bound approval. Shutdown must leave that
        // row pending for the next claim, rather than routing it on the first flight.
        assert.ok(heldGet, "the gate installed its input-read barrier");
        await heldGet.entered;
        assertHeldInputGet(s.runId);
        const [pending] = s.send(s.input("approve_plan"));
        const stored = api.inputRows(s.runId).find((row) => row.id === pending!.id)!;
        assert.equal(stored.gate_binding, "bound");
        assert.equal(stored.gate_revision, persisted.revision);
        flight.runner.shutdown();
        await promptly(Promise.race([
          pollingStopped,
          flight.done.then(() => { throw new Error("flight ended without stopping input polling"); }),
        ]).then(async () => {
          heldGet!.release(); // The stopped poller cannot acknowledge this pending verdict.
          await flight.done;
        }));
        assert.equal(flight.finished, true, "shutdown settled the gate flight");
        assert.equal(flight.error, undefined, "shutdown completed without an execution error");
        assert.equal(s.rowsOf("cancel").length, 0, "shutdown needed no cancel fallback");
        assert.ok(!s.statuses(flight).some((status) => status === "failed" || status === "cancelled"), s.statuses(flight).join(","));
        assert.equal(s.persistedGate()?.plan_md, PLAN_V1, "the submitted plan remains persisted");
        assert.deepEqual(s.persistedGate()?.milestones, V1_MILESTONES, "the candidate work remains persisted");
        const trackedTip = await git.trackingTip(bare, `agent/issue-${s.base.issue_iid}`);
        assert.ok(trackedTip, "shutdown fetched the committed work into the worker bare");
        execFileSync("git", ["-C", bare, "merge-base", "--is-ancestor", committedTip, trackedTip], { env: gitEnv });
        assert.deepEqual(api.gateOf(s.runId), persisted, "shutdown preserves the gate identity");
        if (failHeldGet) assert.equal(api.isAcked(s.runId, pending!.id), false, "shutdown left the verdict unread");
        assert.equal(api.isApplied(s.runId, pending!.id), false, "shutdown did not apply the verdict");
        assertNoApproval(s);
        api.delayInputGets(s.runId, 0, 0);

        const claim = session === "kept" ? s.resumeClaim("kept", api.gateResumeFields(s.runId)) : s.resumeClaim("none");
        assert.equal(claim.plan_approved, false, "the resumed claim still needs approval");
        const resumed = s.start(claim);
        assert.ok(await until(() => s.gates(resumed).length > 0 || resumed.finished, 3_000), s.statuses(resumed).join(","));
        const { gate: nextGate, content: retainedWork } = readRetainedGateWork(s, resumed,
          git.runnerClonePath(bare, `issue-${s.base.issue_iid}`));
        assert.equal(retainedWork,
          "committed before gate shutdown\n", "the resumed clone retains the committed work");
        if (session === "kept") {
          await s.finish(resumed);
          assert.equal(nextGate.plan_md, PLAN_V1, "the submitted plan is re-presented");
          assert.deepEqual(nextGate.milestones, V1_MILESTONES, "the candidate milestones are re-presented");
          assert.equal(nextGate.presentation_id, persisted.presentationId, "the same gate id is retained");
          assert.deepEqual(api.gateOf(s.runId), persisted, "the same revision is retained");
          assert.equal(api.inputReceiptCalls.filter((c) => c.runId === s.runId && c.kind === "applied" && c.ids.includes(pending!.id)).length, 1, "the bound approval is applied exactly once");
          assert.ok(api.humanPlanApproved(s.runId), "the pending approval was taken");
          assert.ok(s.statuses(resumed).includes("completed"), s.statuses(resumed).join(","));
        } else {
          assert.equal(nextGate.plan_md, PLAN_V1, "a fresh plan is shown");
          assert.notEqual(nextGate.presentation_id, persisted.presentationId, "the fresh gate has a new id");
          assert.equal(api.gateOf(s.runId).revision, persisted.revision + 1, "the fresh gate has a new revision");
          assert.ok(await until(() => s.acks(pending!.id, resumed) > 0, 3_000), "the pending verdict was read");
          assert.ok(await until(() => s.texts(resumed).some((line) => line.includes("ignored") && line.includes("re-send")), 3_000), "the unmatched verdict is explained");
          await assertDisposedApprove(s, pending!.id, "the stale approval");
          assertNoApproval(s);
          assert.equal(resumed.finished, false, "the fresh gate requires a fresh approval");
          const [fresh] = s.send(s.input("approve_plan"));
          await s.finish(resumed);
          assert.ok(api.isApplied(s.runId, fresh!.id), "the fresh approval is applied");
          assert.ok(s.statuses(resumed).includes("completed"), s.statuses(resumed).join(","));
        }
        assert.equal(s.rowsOf("cancel").length, 0, "neither claim required a cancel");
      }, {}, planApprovalTimeoutMs === 60_000 && failHeldGet ? "shutdown at an observed plan gate: 60000ms timeout, held GET fails, no session" : undefined));
  }
});

describe("#1604 round 3 — a disposed approve is never applied, so no later claim reads it as approval (B1)", () => {
  // Round 4 (finding 2): a disposed approve is settled through /inputs/discarded (disposition
  // superseded), which the server never counts as approval and leaves out of the replay list, so the
  // next claim does not read it again. Against an api without the route (404) it stays unapplied and
  // every later claim disposes of it again (the round-3 behaviour, kept as the fallback).
  for (const route of ["discarded", "404"] as const) {
    const label = route === "404" ? " (an api without /inputs/discarded: left unapplied, disposed of again)" : " (discarded: never re-served)";
    it(`Path B, then a second interruption at the re-presented gate: the reclaim is not approved and gates B again${label}`, () =>
      scenario(async (s) => {
        api.discardRouteMissing = route === "404";
        const { flight, row } = await pathB(s, "approve_plan");
        assert.equal(s.gates(flight)[0]?.plan_md, revisedPlan(1), "plan B is re-presented");
        if (route === "discarded") await assertDisposedApprove(s, row.id);
        await s.shutdown(flight); // the second interruption, at B's gate
        const claim = s.resumeClaim("kept");
        assert.equal(claim.plan_approved, false, "the reclaim is not plan_approved");
        assert.equal(api.isApplied(s.runId, row.id), route === "discarded", route === "discarded" ? "the stale approve was discarded" : "the stale approve stayed unapplied");
        assertNoApproval(s);
        assert.equal(claim.resume_phase, "awaiting_approval", "the reclaim resumes at the gate, not implementing");
        const third = s.start(claim);
        assert.ok(await until(() => s.gates(third).length >= 1 || third.finished), s.statuses(third).join(","));
        await tick(200);
        assert.equal(s.gates(third)[0]?.plan_md, revisedPlan(1), "B is gated again");
        assert.equal(third.finished, false, `B waits for its own verdict: ${s.statuses(third).join(",")}`);
        assert.equal(s.model.count("implement"), 0, "nothing implemented without a verdict on B");
        if (route === "discarded") {
          assert.equal(s.acks(row.id, third), 0, "the discarded approve is not served to the next claim");
          assert.ok(!s.texts(third).includes(REPLAY_STALE_VERDICT_NOTICE), s.texts(third).join(" | "));
        } else assert.ok(s.texts(third).includes(REPLAY_STALE_VERDICT_NOTICE), "the stale approve is disposed of again");
        const [fresh] = s.send(s.input("approve_plan"));
        await s.finish(third);
        assert.ok(s.statuses(third).includes("completed"), s.statuses(third).join(","));
        assert.ok(api.isApplied(s.runId, fresh!.id), "the approve the gate took is applied");
        assert.equal(api.isDiscarded(s.runId, fresh!.id), false, "the taken approve is applied as the approval");
        assert.ok(!api.inputReceiptCalls.some((c) => c.kind === "applied" && c.ids.includes(row.id)), "the stale approve is never sent to /inputs/applied");
        if (route === "404") assert.equal(api.isApplied(s.runId, row.id), false, "the stale approve never is applied");
        assert.equal(s.model.count("implement"), 1);
      }, {}, `round 3 B1 Path B: second interruption gates B again; route=${route}`));

    it(`Path A, then a second interruption at the fresh plan's gate: the reclaim is not approved and gates again${label}`, () =>
      scenario(async (s) => {
        api.discardRouteMissing = route === "404";
        const { row } = await releaseAtGate(s, () => s.send(s.input("approve_plan"))[0]!, "unacked");
        const second = s.start(s.resumeClaim("none"));
        assert.ok(await until(() => s.gates(second).length >= 1 || second.finished), s.statuses(second).join(","));
        await until(() => s.texts(second).includes(STALE_APPROVE_NOTICE), 3_000);
        assert.ok(s.texts(second).includes(STALE_APPROVE_NOTICE), s.texts(second).join(" | "));
        if (route === "discarded") await assertDisposedApprove(s, row.id);
        await s.shutdown(second); // the second interruption, at the fresh plan's gate
        const claim = s.resumeClaim("none");
        assert.equal(claim.plan_approved, false, "the reclaim is not plan_approved");
        assertNoApproval(s);
        if (route === "404") assert.equal(api.isApplied(s.runId, row.id), false, "the stale approve stayed unapplied");
        assert.notEqual(claim.resume_phase, "implementing", "the reclaim is not implementing");
        const third = s.start(claim);
        assert.ok(await until(() => s.gates(third).length >= 1 || third.finished), s.statuses(third).join(","));
        await tick(200);
        assert.equal(third.finished, false, `the plan waits at the gate: ${s.statuses(third).join(",")}`);
        assert.equal(s.model.count("implement"), 0, "nothing implemented without a verdict");
        assert.equal(s.acks(row.id, third) > 0, route === "404", route === "404" ? "the unapplied approve is read again" : "the discarded approve is not served again");
        s.send(s.input("approve_plan"));
        await s.finish(third);
        assert.ok(s.statuses(third).includes("completed"), s.statuses(third).join(","));
        assert.ok(!api.inputReceiptCalls.some((c) => c.kind === "applied" && c.ids.includes(row.id)), "the stale approve is never sent to /inputs/applied");
        if (route === "404") assert.equal(api.isApplied(s.runId, row.id), false, "the stale approve never is applied");
      }));
  }
});

describe("#1604 round 3 — a claim without resume_plan_at fails closed (B2)", () => {
  for (const kind of ["approve_plan", "reject_plan"] as const) {
    it(`Path B (${kind}) with resume_plan_at absent: the replayed verdict is stale; only a fresh approve after the re-present acts`, () =>
      scenario(async (s) => {
        const { flight, row } = await pathB(s, kind, { omitResumePlanAt: true });
        assert.equal(s.gates(flight)[0]?.plan_md, revisedPlan(1), "plan B is re-presented");
        assert.equal(flight.finished, false, `B waits for its own verdict: ${s.statuses(flight).join(",")}`);
        assert.ok(!s.statuses(flight).includes("failed"), "the replayed reject did not fail the run");
        assert.equal(s.model.count("implement"), 0, "nothing implemented without a fresh approve");
        // Round 4 (finding 6): the unjudged mode's own notice, not "sent before this plan was shown".
        assert.ok(s.texts(flight).includes(REPLAY_UNJUDGED_NOTICE), s.texts(flight).join(" | "));
        assert.ok(!s.texts(flight).includes(REPLAY_STALE_VERDICT_NOTICE), s.texts(flight).join(" | "));
        if (kind === "approve_plan") await assertDisposedApprove(s, row.id, "the replayed approve");
        const [fresh] = s.send(s.input("approve_plan"));
        await s.finish(flight);
        assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
        assert.ok(api.isApplied(s.runId, fresh!.id), "the fresh approve sent after the re-present applies");
        assert.equal(s.model.count("implement"), 1);
        assert.equal(s.model.count("revise", flight.turnFrom), 0, "no second revision");
      }));
  }
});

describe("#1604 round 4 — no gate before the replayed backlog is drained (finding 1)", () => {
  /** A persisted plan A whose approve (sent after A was shown) is left unread by a release, then a
   *  reclaim whose claim cannot say when A was shown (resume_plan_at omitted). */
  async function unjudgedAfterRelease(s: Scenario): Promise<{ row: UserInput; claim: ClaimResponse }> {
    const { row } = await releaseAtGate(s, () => s.send(s.input("approve_plan"))[0]!, "unacked");
    api.omitResumePlanAt = true;
    const claim = s.resumeClaim("kept");
    assert.equal(claim.resume_plan_at, undefined);
    return { row, claim };
  }

  it("a Codex-shaped executor (no resumesAtGate), resume_plan_at absent, the approve's GET failing past the plan turn: the fresh plan is never approved by it", () =>
    scenario(async (s) => {
      const { row, claim } = await unjudgedAfterRelease(s);
      // Five transient failures, then a read slow enough that the stub's plan is ready long before it.
      api.failInputGets(s.runId, 5, 503);
      api.delayInputGets(s.runId, 1_000, 1, (api.inputGets.get(s.runId) ?? 0) + 5);
      const flight = s.start(claim, { executor: () => new StubExecutor(nullLogger(), { planGate: true }) });
      assert.ok(await until(() => s.gates(flight).length >= 1 || flight.finished), s.statuses(flight).join(","));
      assert.ok(await until(() => api.isAcked(s.runId, row.id), 3_000), "the replayed approve was read");
      await tick(300);
      assert.equal(flight.finished, false, `the plan waits for its own verdict: ${s.statuses(flight).join(",")}`);
      assert.ok(!s.statuses(flight).includes("completed"), "the replayed approve did not approve the plan");
      assert.ok(s.texts(flight).includes(REPLAY_UNJUDGED_NOTICE), s.texts(flight).join(" | "));
      await assertDisposedApprove(s, row.id, "the replayed approve");
      assertNoApproval(s);
      const ackAt = api.timeline.findIndex((e, i) => i >= flight.timelineFrom && e.type === "receipt_reply" && e.kind === "ack" && e.httpStatus === 200 && e.ids.includes(row.id));
      assert.ok(ackAt >= 0 && s.stateAt("awaiting_approval", flight.timelineFrom) > ackAt, "awaiting_approval is reported only after the replayed approve was read");
      const [fresh] = s.send(s.input("approve_plan"));
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.ok(api.isApplied(s.runId, fresh!.id) && !api.isDiscarded(s.runId, fresh!.id), "the fresh approve is the approval");
    }));

  it("an SDK claim whose replayed approve lies past the first GET batch: judged replayed, the re-presented plan is not approved by it", () =>
    scenario(async (s) => {
      api.inputPageSize = 2;
      s.send(s.input("follow_up", "one"), s.input("follow_up", "two"), s.input("follow_up", "three"));
      const [approve] = s.send(s.input("approve_plan"));
      // Hold this claim's second GET, so a gate offered after the first batch would be shown (and
      // its epoch bumped) before the approve is read.
      api.delayInputGets(s.runId, 400, 1, (api.inputGets.get(s.runId) ?? 0) + 1);
      s.writeTranscript();
      const flight = s.start(
        s.claim({ resume_phase: "awaiting_approval", plan_md: PLAN_V1, milestones: V1_MILESTONES, plan_source: "agent", plan_approved: false, session_id: SID }),
      );
      assert.ok(await until(() => s.gates(flight).length >= 1 || flight.finished), s.statuses(flight).join(","));
      await tick(300);
      assert.equal(s.gates(flight)[0]?.plan_md, PLAN_V1, "the submitted plan is re-presented");
      assert.equal(flight.finished, false, `the plan waits for its own verdict: ${s.statuses(flight).join(",")}`);
      assert.equal(s.model.count("implement"), 0, "nothing implemented on the replayed approve");
      assert.ok(s.texts(flight).includes(REPLAY_UNJUDGED_NOTICE), s.texts(flight).join(" | "));
      await assertDisposedApprove(s, approve!.id, "the replayed approve");
      const ackAt = api.timeline.findIndex((e, i) => i >= flight.timelineFrom && e.type === "receipt_reply" && e.kind === "ack" && e.httpStatus === 200 && e.ids.includes(approve!.id));
      assert.ok(ackAt >= 0 && s.stateAt("awaiting_approval", flight.timelineFrom) > ackAt, "the plan is offered only after the approve past the first batch was read");
      const [fresh] = s.send(s.input("approve_plan"));
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.ok(api.isApplied(s.runId, fresh!.id) && !api.isDiscarded(s.runId, fresh!.id));
    }));

  it("a Codex-shaped executor: a definitive read failure fails the run before any plan is offered", () =>
    scenario(async (s) => {
      const { claim } = await unjudgedAfterRelease(s);
      api.rawInputGets(s.runId, { inputs: "not-a-list", receipts: true });
      const flight = s.start(claim, { executor: () => new StubExecutor(nullLogger(), { planGate: true }) });
      await until(() => s.statuses(flight).includes("failed") || s.gates(flight).length > 0, 20_000);
      assert.equal(s.gates(flight).length, 0, "the plan was never offered");
      assert.match(s.states(flight).find((b) => b.status === "failed")?.failure_reason ?? "", /^plan-gate input delivery failed: /);
      api.rawInputGets(s.runId, undefined, 0);
      await s.finish(flight, 2_000);
    }));

  it("a Codex-shaped executor: the bounded transient give-up parks in recovery_wait, never offering the plan", () =>
    scenario(async (s) => {
      const { row, claim } = await unjudgedAfterRelease(s);
      api.recoveryWaitRequiresRunning = true;
      api.failInputGets(s.runId, Infinity, 503);
      const flight = s.start(claim, { executor: () => new StubExecutor(nullLogger(), { planGate: true }) });
      await until(() => ["recovery_wait", "awaiting_approval", "failed"].some((st) => s.statuses(flight).includes(st)), 90_000);
      assert.equal(s.gates(flight).length, 0, "the plan was never offered");
      assert.ok(s.statuses(flight).includes("recovery_wait"), `parked for recovery: ${s.statuses(flight).join(",")}`);
      assert.ok(!s.statuses(flight).includes("failed"), "non-terminal");
      assert.equal(api.isApplied(s.runId, row.id), false, "the replayed approve is left for the next claim");
      api.failInputGets(s.runId, 0);
      s.send(s.input("cancel"));
      await s.finish(flight, 3_000);
    }));

  it("a Codex-shaped executor: a fenced claim ends quietly before any plan is offered", () =>
    scenario(async (s) => {
      const { claim } = await unjudgedAfterRelease(s);
      api.failInputGets(s.runId, 3, 503);
      api.setInputClaimGeneration(s.runId, 99);
      api.setInputFenceReason(s.runId, "released");
      const flight = s.start(claim, { executor: () => new StubExecutor(nullLogger(), { planGate: true }) });
      await s.finish(flight, 8_000);
      assert.deepEqual(s.statuses(flight).filter((st) => ["awaiting_approval", "failed", "completed"].includes(st)), [], s.statuses(flight).join(","));
    }));
});
