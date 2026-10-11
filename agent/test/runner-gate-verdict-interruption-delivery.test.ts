import { describe, it } from "node:test";
import { getEventListeners } from "node:events";
import assert from "node:assert/strict";

import { SteeringChannel } from "../src/steering.js";
import { StubExecutor } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { api, installHarness } from "./runner-harness.js";
import {
  PLAN_V1,
  V1_MILESTONES,
  SID,
  FEEDBACK,
  STATUS_RESUME_WITH_REVISION,
  STATUS_WAITING_DELIVERY,
  STALE_APPROVE_NOTICE,
  STALE_REJECT_NOTICE,
  STALE_REVISE_NOTICE,
  REPLAY_STALE_VERDICT_NOTICE,
  REPLAY_STALE_REVISE_NOTICE,
  REPLAY_UNJUDGED_NOTICE,
  APPROVED_REVISE_NOTICE,
  APPROVED_REJECT_NOTICE,
  revisedPlan,
  tick,
  until,
  scenario,
  assertRevisedOnResume,
  assertDisposedApprove,
  assertNoApproval,
  resumeAndApprove,
  releaseAtGate,
  resumeWith,
  assertHeldInputGet,
  approveFirstGate,
  RECOVERY_REASON,
  pathB,
} from "./runner-gate-verdict-interruption-fixture.js";

installHarness();

describe("#1604 — delivery on resume: no plan is offered before the inputs sent before the release are read", () => {
  it("delayed GET", () =>
    scenario(async (s) => {
      const hold = api.holdNextInputGet(s.runId);
      try {
        const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
        assert.ok(await until(() => (api.inputGets.get(s.runId) ?? 0) > 0), "the delayed GET entered the fake API");
        await hold.entered;
        assertHeldInputGet(s.runId);
        assert.equal(s.gates(flight).length, 0, "no plan is offered before the held GET returns");
        hold.release();
        await approveFirstGate(s, flight);
        assertRevisedOnResume(s, flight, FEEDBACK, "kept");
        assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      } finally {
        hold.release();
      }
    }));

  it("delayed ACK", () =>
    scenario(async (s) => {
      const release = api.holdNextInputReceipt("ack");
      try {
        const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
        assert.ok(await until(() => api.inputReceiptCalls.some((r) => r.runId === s.runId && r.kind === "ack")), "the ACK entered the fake API");
        assert.equal(api.inputReceiptReplies.filter((r) => r.runId === s.runId && r.kind === "ack").length, 0,
          "the held ACK has not replied before release");
        assert.equal(s.gates(flight).length, 0, "no plan is offered before the held ACK returns");
        release();
        await approveFirstGate(s, flight);
        assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      } finally {
        release();
      }
    }));

  for (const which of ["GET", "ACK"] as const) {
    it(`transient ${which} failures: the plan waits, one status line says why`, () =>
      scenario(async (s) => {
        if (which === "GET") api.failInputGets(s.runId, 5, 503);
        else api.failInputReceiptsTimes("ack", 5, 503);
        const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
        await approveFirstGate(s, flight);
        assert.equal(s.texts(flight).filter((t) => t === STATUS_WAITING_DELIVERY).length, 1, `one waiting status line: ${s.texts(flight).join(" | ")}`);
        assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      }));
  }

  it("a bounded transient give-up parks in recovery_wait (non-terminal, verdicts unapplied); a re-claim recovers", () =>
    scenario(async (s) => {
      api.recoveryWaitRequiresRunning = true;
      api.failInputGets(s.runId, Infinity, 503);
      const { flight, rows } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await until(() => ["recovery_wait", "awaiting_approval", "failed"].some((st) => s.statuses(flight).includes(st)), 90_000);
      assert.equal(s.gates(flight).length, 0, "the plan was never offered");
      const park = s.states(flight).find((b) => b.status === "recovery_wait");
      assert.ok(park, `the give-up parked in recovery_wait: ${s.statuses(flight).join(",")}`);
      assert.ok(
        JSON.stringify(park).includes(RECOVERY_REASON) || s.texts(flight).some((t) => t.includes(RECOVERY_REASON)),
        "the park names why",
      );
      assert.ok(!s.statuses(flight).includes("failed"), "non-terminal");
      assert.equal(api.isApplied(s.runId, rows[0]!.id), false, "the revise is unapplied");
      api.failInputGets(s.runId, 0);
      await s.finish(flight, 2_000);
      const { flight: again, gateAt } = await resumeAndApprove(s, s.resumeClaimAtV1());
      assertRevisedOnResume(s, again, FEEDBACK, "kept");
      assert.ok(s.appliedAt(rows[0]!.id, again.timelineFrom) > gateAt);
      assert.ok(s.statuses(again).includes("completed"), s.statuses(again).join(","));
    }));

  it("the give-up on a clean plan-only clone (no implementation commits) captures its restore point and parks; the poller keeps polling", () =>
    scenario(async (s) => {
      api.recoveryWaitRequiresRunning = true;
      api.failInputGets(s.runId, Infinity, 503);
      // The GETs made by the time the park is recorded. The flight ends right after the park
      // (handleRecoveryExhausted is unchanged), so the poller's liveness is proven across the
      // give-up-to-park window: every GET fails, the give-up comes at the bound's 60th failure
      // (2 x the channel's ACTIVE_APPLY_ATTEMPTS), and the poller keeps reading after it.
      let getsAtPark = -1;
      api.onState(s.runId, (b) => {
        if (b.status === "recovery_wait" && getsAtPark < 0) getsAtPark = api.inputGets.get(s.runId) ?? 0;
      });
      const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await until(() => ["recovery_wait", "awaiting_approval", "failed"].some((st) => s.statuses(flight).includes(st)), 90_000);
      assert.ok(s.statuses(flight).includes("recovery_wait"), `reached recovery_wait: ${s.statuses(flight).join(",")}`);
      assert.ok(!s.statuses(flight).includes("failed"), "the capture of a clean plan-only clone did not fail the run");
      assert.ok(getsAtPark >= 60 + 3, `the steering poller kept polling after the give-up, through the park: ${getsAtPark} GETs`);
      api.onState(s.runId, () => {});
      api.failInputGets(s.runId, 0);
      s.send(s.input("cancel"));
      await s.finish(flight, 3_000);
    }));

  it("a definitive protocol failure fails the run explicitly", () =>
    scenario(async (s) => {
      api.rawInputGets(s.runId, { inputs: "not-a-list", receipts: true });
      const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await until(() => s.statuses(flight).includes("failed") || s.gates(flight).length > 0, 20_000);
      assert.equal(s.gates(flight).length, 0, "the plan was never offered");
      assert.match(s.states(flight).find((b) => b.status === "failed")?.failure_reason ?? "", /^plan-gate input delivery failed: /);
      api.rawInputGets(s.runId, undefined, 0);
      await s.finish(flight, 2_000);
    }));

  it("a fenced claim ends quietly", () =>
    scenario(async (s) => {
      const { flight, rows } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      api.setInputClaimGeneration(s.runId, 7);
      api.setInputFenceReason(s.runId, "released");
      await s.finish(flight, 8_000);
      assert.deepEqual(s.statuses(flight).filter((st) => ["awaiting_approval", "failed", "completed"].includes(st)), [], s.statuses(flight).join(","));
      assert.equal(s.model.turns.length, 0, "no turn ran");
      assert.equal(api.isApplied(s.runId, rows[0]!.id), false, "the revise is left for the next claim");
    }));

  it("a cancel beats a pending revise (routed while the delivery waiter is parked)", () =>
    scenario(async (s) => {
      // The entered GET proves the delivery waiter is armed before the cancel and revise route.
      const hold = api.holdNextInputGet(s.runId);
      try {
        const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK), s.input("cancel"));
        assert.ok(await until(() => (api.inputGets.get(s.runId) ?? 0) > 0), "the delivery GET entered the fake API");
        await hold.entered;
        assertHeldInputGet(s.runId);
        assert.equal(s.gates(flight).length, 0, "no plan was offered while delivery was held");
        hold.release();
        await s.finish(flight);
        assert.equal(s.gates(flight).length, 0, "no plan was offered");
        assert.equal(s.model.turns.length, 0, "no revision turn");
        // The cancel ends the run as a cancel (REASON_CANCELLED), never as the AbortError a cancel
        // routed while the delivery waiter is parked used to reject it with.
        const failed = s.states(flight).find((b) => b.status === "failed");
        assert.equal(failed?.failure_reason, "run cancelled", `ended cancelled: ${s.statuses(flight).join(",")}`);
      } finally {
        hold.release();
      }
    }));

  it("an approve submitted during delayed delivery goes stale; the revise wins", () =>
    scenario(async (s) => {
      const hold = api.holdNextInputGet(s.runId);
      try {
        const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
        assert.ok(await until(() => (api.inputGets.get(s.runId) ?? 0) > 0), "the delivery GET entered the fake API");
        await hold.entered;
        assertHeldInputGet(s.runId);
        assert.equal(s.gates(flight).length, 0, "no gate was offered while the revise was unread");
        const [approve] = s.send(s.input("approve_plan"));
        hold.release();
        await approveFirstGate(s, flight);
        // This claim carries no resume_plan_at, so it fails closed: an approve read before its first
        // gate is stale, with the unjudged notice, and never applied as approval (discarded).
        assert.ok(s.texts(flight).includes(REPLAY_UNJUDGED_NOTICE), s.texts(flight).join(" | "));
        await assertDisposedApprove(s, approve!.id);
        assertRevisedOnResume(s, flight, FEEDBACK, "kept");
        assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      } finally {
        hold.release();
      }
    }));
});

describe("#1604 — replay across claims", () => {
  it("claim N's late APPLIED is refused once claim N+1 holds the run; the replay makes no new row", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const release = api.holdNextInputReceipt("applied");
      const block = s.model.block("revise");
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      await tick(60);
      first.runner.shutdown();
      await until(() => first.finished, 1_500);
      const claim = s.resumeClaim("kept");
      release();
      await s.finish(first, 5_000);
      assert.equal(api.isApplied(s.runId, row!.id), false, "claim 1's late APPLIED did not apply the revise");
      const { flight, gateAt } = await resumeAndApprove(s, claim);
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      assert.equal(s.rowsOf("revise_plan").length, 1, "the replay created no new revise row");
      assert.ok(s.appliedAt(row!.id, flight.timelineFrom) > gateAt);
    }));
});

/** Path B: the owner sends `kind` against plan A during the revision turn while a switch is pending
 *  (the fenced GET never drains it); B is persisted and the claim released; the reclaim re-presents
 *  B with the verdict replayed. */
describe("#1604 review — a replayed verdict never applies to a plan no human saw (finding 1)", () => {
  it("Path A: a replayed approve does not approve the fresh plan of a no-session re-plan", () =>
    scenario(async (s) => {
      const { first, row } = await releaseAtGate(s, () => s.send(s.input("approve_plan"))[0]!, "unacked");
      assert.ok(s.statuses(first).includes("credential_switch"), s.statuses(first).join(","));
      const flight = s.start(s.resumeClaim("none"));
      assert.ok(await until(() => s.gates(flight).length >= 1 || flight.finished), s.statuses(flight).join(","));
      await tick(200);
      assert.equal(flight.finished, false, `the fresh plan waits at the gate: ${s.statuses(flight).join(",")}`);
      assert.equal(s.model.count("plan", flight.turnFrom), 1, "the no-session claim planned afresh");
      assert.equal(s.model.count("implement"), 0, "nothing is implemented before a verdict on the fresh plan");
      assert.ok(s.texts(flight).includes(STALE_APPROVE_NOTICE), s.texts(flight).join(" | "));
      assert.ok(api.isAcked(s.runId, row.id), "the replayed approve was read");
      await assertDisposedApprove(s, row.id);
      assertNoApproval(s);
      s.send(s.input("approve_plan"));
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  for (const kind of ["approve_plan", "reject_plan"] as const) {
    it(`Path B (${kind}): a verdict sent against plan A does not settle the re-presented plan B`, () =>
      scenario(async (s) => {
        const { flight, row } = await pathB(s, kind);
        assert.equal(s.gates(flight)[0]?.plan_md, revisedPlan(1), "plan B is re-presented");
        assert.equal(flight.finished, false, `B waits for its own verdict: ${s.statuses(flight).join(",")}`);
        assert.ok(!s.statuses(flight).includes("failed"), "the run is not failed");
        assert.equal(s.model.count("implement"), 0, "nothing implemented");
        assert.ok(s.texts(flight).includes(REPLAY_STALE_VERDICT_NOTICE), s.texts(flight).join(" | "));
        assert.ok(!s.texts(flight).includes(STALE_APPROVE_NOTICE) && !s.texts(flight).includes(STALE_REJECT_NOTICE), "never the plan-changed notice");
        assert.ok(!s.texts(flight).some((t) => t.startsWith("the re-presented plan was")), "no status line claims B was settled");
        if (kind === "reject_plan") assert.ok(await until(() => api.isApplied(s.runId, row.id), 3_000), "the stale reject is applied");
        else {
          assert.ok(api.isAcked(s.runId, row.id), "the replayed approve was read");
          await assertDisposedApprove(s, row.id);
          assertNoApproval(s);
        }
        s.send(s.input("approve_plan"));
        await s.finish(flight);
        assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
        assert.equal(s.model.count("revise", flight.turnFrom), 0, "no second revision");
      }, {}, kind === "reject_plan" ? "Path B (reject_plan)" : undefined));
  }

  it("a replayed revise older than the persisted revised plan is stale: no second revision", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      // The claim is released the moment the revised plan lands, so the revise's APPLIED never does.
      api.onState(s.runId, (b) => {
        if (b.status === "awaiting_approval" && b.plan_md === revisedPlan(1)) {
          api.setInputClaimGeneration(s.runId, 99);
          api.setInputFenceReason(s.runId, "released");
        }
      });
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.gateAt(revisedPlan(1)) >= 0 || first.finished), "the revised plan was persisted");
      if (!(await until(() => first.finished, 1_000))) s.send(s.input("follow_up", "nudge"));
      assert.ok(await until(() => first.finished), "the released flight ended");
      api.onState(s.runId, () => {});
      assert.equal(api.isApplied(s.runId, row!.id), false, "the revise was left unapplied");
      const { flight } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assert.equal(s.gates(flight)[0]?.plan_md, revisedPlan(1), "the persisted revised plan is re-presented");
      assert.equal(s.model.count("revise", flight.turnFrom), 0, "no second revision");
      assert.ok(s.texts(flight).includes(REPLAY_STALE_REVISE_NOTICE), s.texts(flight).join(" | "));
      assert.ok(api.isApplied(s.runId, row!.id), "the stale revise is applied");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  it("a stub (Codex-shaped) executor never re-presents: its first gate waits for the replayed backlog, and a replayed approve goes stale at it (finding 6; round 4 finding 1)", () =>
    scenario(async (s) => {
      const { row } = await releaseAtGate(s, () => s.send(s.input("approve_plan"))[0]!, "unacked");
      api.failInputGets(s.runId, 5, 503);
      const afterReads = (api.inputGets.get(s.runId) ?? 0) + 5;
      const hold = api.holdNextInputGet(s.runId, afterReads);
      try {
        const flight = s.start(s.resumeClaim("kept"), { executor: () => new StubExecutor(nullLogger(), { planGate: true }) });
        assert.ok(await until(() => (api.inputGets.get(s.runId) ?? 0) > afterReads, 3_000), "the post-retry GET entered the fake API");
        await hold.entered;
        assertHeldInputGet(s.runId);
        await new Promise<void>((resolve) => setImmediate(resolve));
        assert.equal(s.gates(flight).length, 0, "the stub cannot offer its gate before the replayed backlog is read");
        hold.release();
        assert.ok(await until(() => s.gates(flight).length >= 1 || flight.finished), s.statuses(flight).join(","));
        assert.ok(await until(() => api.isAcked(s.runId, row.id), 3_000), "the approve was read");
        await assertDisposedApprove(s, row.id);
        assertNoApproval(s);
        assert.equal(flight.finished, false, `the stub's plan waits at the gate: ${s.statuses(flight).join(",")}`);
        assert.ok(await until(() => s.texts(flight).includes(STALE_APPROVE_NOTICE), 3_000), s.texts(flight).join(" | "));
        // Round 4 (finding 1): every executor's first gate waits for the replayed backlog, so the
        // transient read failures earn the one waiting line (the stub no longer offers its plan first).
        assert.equal(s.texts(flight).filter((t) => t === STATUS_WAITING_DELIVERY).length, 1, s.texts(flight).join(" | "));
        const ackAt = api.timeline.findIndex((e, i) => i >= flight.timelineFrom && e.type === "receipt_reply" && e.kind === "ack" && e.httpStatus === 200 && e.ids.includes(row.id));
        assert.ok(ackAt >= 0 && s.stateAt("awaiting_approval", flight.timelineFrom) > ackAt, "the gate is reported only after the replayed approve was read");
        s.send(s.input("cancel"));
        await s.finish(flight);
      } finally {
        hold.release();
      }
    }));
});

describe("#1604 review — the ready lane never mixes kinds under a pending switch (finding 2)", () => {
  it("revise taken, stale reject during the revision turn, deferred switch: released, revise applied, the reject re-disposed on the next claim", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      const [rev] = s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      const [rej] = s.send(s.input("reject_plan", "no"));
      assert.ok(await until(() => api.isAcked(s.runId, rej!.id)));
      api.requestCredentialSwitch(s.runId, 1);
      await tick(60);
      // The revise's first APPLIED is slow and fails, so its retry finds the stale reject ready too.
      api.delayInputReceipts("applied", 100);
      api.failInputReceiptsTimes("applied", 1, 503);
      block.release();
      await s.finish(first);
      api.delayInputReceipts("applied", 0);
      assert.ok(s.statuses(first).includes("credential_switch"), `released, not retained: ${s.statuses(first).join(",")}`);
      assert.ok(!s.statuses(first).includes("failed"), s.statuses(first).join(","));
      assert.ok(api.isApplied(s.runId, rev!.id), "the revise was applied under switch_pending");
      assert.equal(api.isApplied(s.runId, rej!.id), false, "the stale reject is left to the next claim");
      assert.equal(s.model.revisions, 1);
      const { flight } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assert.ok(!s.statuses(flight).includes("failed"), `the stale reject did not fail the run: ${s.statuses(flight).join(",")}`);
      assert.ok(s.texts(flight).includes(REPLAY_STALE_VERDICT_NOTICE), s.texts(flight).join(" | "));
      assert.ok(api.isApplied(s.runId, rej!.id), "the reject is re-disposed and applied");
      assert.equal(s.model.count("revise", flight.turnFrom), 0, "no double revision");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));
});

describe("#1604 review — resumed-gate edges", () => {
  it("a server-approved plan re-planned for lack of a session never takes the resumed-gate path (finding 4)", () =>
    scenario(async (s) => {
      s.send(s.input("revise_plan", FEEDBACK));
      const flight = s.start(s.claim({ plan_md: PLAN_V1, plan_source: "agent", plan_approved: true, session_id: null, milestones: V1_MILESTONES }));
      assert.ok(await until(() => s.gates(flight).length >= 1 || flight.finished), s.statuses(flight).join(","));
      assert.ok(!s.texts(flight).includes(STATUS_RESUME_WITH_REVISION), s.texts(flight).join(" | "));
      assert.equal(s.model.count("plan", flight.turnFrom), 1, "the approved-but-unrecoverable plan is re-planned");
      s.send(s.input("cancel"));
      await s.finish(flight);
    }));

  it("a revise or reject routed after the approve was taken is final at once (finding 5)", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("implement");
      s.send(s.input("approve_plan"));
      await block.entered;
      const [rev, rej] = s.send(s.input("revise_plan", "too late"), s.input("reject_plan", "too late"));
      assert.ok(await until(() => api.isApplied(s.runId, rev!.id) && api.isApplied(s.runId, rej!.id), 3_000), "both are applied while implementing");
      block.release();
      await s.finish(first);
      // L2: never the re-send notices (re-sending would be ignored the same way).
      assert.ok(s.texts(first).includes(APPROVED_REVISE_NOTICE), s.texts(first).join(" | "));
      assert.ok(s.texts(first).includes(APPROVED_REJECT_NOTICE), s.texts(first).join(" | "));
      assert.ok(!s.texts(first).includes(STALE_REVISE_NOTICE) && !s.texts(first).includes(STALE_REJECT_NOTICE), s.texts(first).join(" | "));
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
      assert.equal(s.model.revisions, 0);
    }));

  it("a 409 row conflict on the first ACK is transient: dropped, re-read, then delivered (finding 7)", () =>
    scenario(async (s) => {
      // A slow first GET parks the executor on the delivery wait before the refused ACK lands.
      api.delayInputGets(s.runId, 600);
      api.failInputReceiptsTimes("ack", 2, 409);
      const { flight } = resumeWith(s, s.input("revise_plan", FEEDBACK));
      await approveFirstGate(s, flight);
      assert.ok(!s.statuses(flight).includes("failed"), s.statuses(flight).join(","));
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
    }));

  it("a GET carrying a switch signal for another generation counts as delivered (finding 8)", () =>
    scenario(async (s) => {
      api.signalForeignCredentialSwitch(s.runId, 99);
      const { flight } = resumeWith(s);
      assert.ok(await until(() => s.gates(flight).length >= 1 || flight.finished, 3_000), `the re-presented gate is offered: ${s.statuses(flight).join(",")}`);
      assert.equal(s.gates(flight)[0]?.plan_md, PLAN_V1);
      s.send(s.input("approve_plan"));
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  it("an approval timeout at a re-presented gate is named as a timeout, not a rejection (finding 10)", () =>
    scenario(async (s) => {
      s.writeTranscript();
      const flight = s.start(
        s.claim({ resume_phase: "awaiting_approval", plan_md: PLAN_V1, milestones: V1_MILESTONES, plan_source: "agent", plan_approved: false, session_id: SID }),
        { runner: { planApprovalTimeoutMs: 400 } },
      );
      await s.finish(flight);
      assert.ok(s.statuses(flight).includes("failed"), s.statuses(flight).join(","));
      const lines = s.texts(flight).filter((t) => t.startsWith("the re-presented plan"));
      assert.deepEqual(lines, ["the re-presented plan timed out waiting for approval — failing the run"]);
    }));

  it("clears the losing gate waiter and abort listener when approval times out", () =>
    scenario(async (s) => {
      const original = SteeringChannel.prototype.awaitGateEvent;
      const channels: SteeringChannel[] = [];
      let waitSignal: AbortSignal | undefined;
      SteeringChannel.prototype.awaitGateEvent = function (epoch, signal) {
        channels.push(this);
        waitSignal = signal;
        return original.call(this, epoch, signal);
      };
      try {
        s.writeTranscript();
        const flight = s.start(
          s.claim({ resume_phase: "awaiting_approval", plan_md: PLAN_V1, milestones: V1_MILESTONES, plan_source: "agent", plan_approved: false, session_id: SID }),
          { runner: { planApprovalTimeoutMs: 100 } },
        );
        await s.finish(flight);
        assert.ok(s.statuses(flight).includes("failed"), "the approval timeout still fails the run");
        assert.ok(waitSignal?.aborted, "the losing gate wait was aborted after the timeout verdict");
        assert.equal(getEventListeners(waitSignal, "abort").length, 0, "the waiter removed its abort listener");
        assert.equal((channels[0] as unknown as { gateWaiter?: unknown }).gateWaiter, undefined, "no gate waiter remains parked");
      } finally {
        SteeringChannel.prototype.awaitGateEvent = original;
      }
    }));
});
