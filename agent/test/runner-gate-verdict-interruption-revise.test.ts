import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { ClaimResponse, UserInput } from "../src/protocol.js";
import { api, installHarness } from "./runner-harness.js";
import {
  PLAN_V1,
  FEEDBACK,
  STALE_REJECT_NOTICE,
  SUPERSEDED_REJECT_NOTICE,
  revisedPlan,
  tick,
  until,
  scenario,
  assertRevisedOnResume,
  resumeAndApprove,
} from "./runner-gate-verdict-interruption-fixture.js";

installHarness();

describe("#1604 — (c) a switch requested during the revision turn", () => {
  for (const session of ["kept", "none"] as const) {
    it(`deferral holds, the revised plan and the revise settle before the release; the reclaim (${session === "kept" ? "session kept" : "no session"}) ${session === "kept" ? "re-presents the revised plan" : "plans afresh (no re-presentation)"} with no second revision`, () =>
      scenario(async (s) => {
        const first = await s.toFirstGate();
        const block = s.model.block("revise");
        const [row] = s.send(s.input("revise_plan", FEEDBACK));
        await block.entered;
        api.requestCredentialSwitch(s.runId, 1);
        await tick(100);
        assert.ok(!s.statuses(first).includes("credential_switch"), "the switch is deferred while the revision turn runs");
        assert.equal(first.finished, false, "the revision turn is not interrupted");
        block.release();
        await s.finish(first);
        const v2At = s.gateAt(revisedPlan(1));
        const releasedAt = s.stateAt("credential_switch");
        assert.ok(v2At >= 0 && releasedAt > v2At, `revised plan persisted before the release: ${s.statuses(first).join(",")}`);
        const appliedAt = s.appliedAt(row!.id);
        assert.ok(appliedAt > v2At && appliedAt < releasedAt, "the revise applied under switch_pending, after persistence, before the release");
        const { flight } = await resumeAndApprove(s, s.resumeClaim(session));
        assert.equal(api.isApplied(s.runId, row!.id), true, "nothing is left to replay");
        assert.equal(s.model.count("revise", flight.turnFrom), 0, "no second revision");
        if (session === "kept") {
          assert.equal(s.gates(flight)[0]?.plan_md, revisedPlan(1), "the resumed gate shows the revised plan");
          assert.equal(s.model.count("plan", flight.turnFrom), 0, "no re-plan");
        } else {
          // The #1604 plan (D3): a "" resume with no session and nothing pending plans from
          // scratch, as before; the persisted revised plan is not re-presented.
          assert.equal(s.model.count("plan", flight.turnFrom), 1, "the no-session resume plans from scratch");
          assert.equal(s.gates(flight)[0]?.plan_md, PLAN_V1, "the fresh plan is gated");
        }
        assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
        assert.equal(s.model.count("implement"), 1);
      }));
  }
});

describe("#1604 — boundaries of the revise receipt", () => {
  it("interrupted after the revised plan is persisted but before its APPLIED lands: the reclaim never offers the superseded plan", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      // The moment the revised plan lands, the claim is released under the flight (its later
      // receipts are refused), so any APPLIED still owed for the revise never lands.
      api.onState(s.runId, (b) => {
        if (b.status === "awaiting_approval" && b.plan_md === revisedPlan(1)) {
          api.setInputClaimGeneration(s.runId, 99);
          api.setInputFenceReason(s.runId, "released");
        }
      });
      s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.gateAt(revisedPlan(1)) >= 0 || first.finished), "the revised plan was persisted");
      // A harmless row makes a flight with no receipt still owed notice the release too.
      if (!(await until(() => first.finished, 1_000))) s.send(s.input("follow_up", "nudge"));
      assert.ok(await until(() => first.finished), "the released flight ended");
      api.onState(s.runId, () => {});
      const { flight } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assert.ok(!s.gates(flight).some((g) => g.plan_md === PLAN_V1), "never the superseded plan");
      assert.match(s.gates(flight)[0]?.plan_md ?? "", /# REVISED PLAN/, "a revised plan is offered");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
      assert.equal(s.rowsOf("revise_plan").length, 1, "no new revise row");
    }));

  it("a lost APPLIED reply for the revise is retried; one revision, applied after persistence", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      api.loseNextInputReceiptReply("applied");
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.equal(s.model.revisions, 1, "one revision");
      assert.ok(api.isApplied(s.runId, row!.id), "the revise is applied");
      assert.ok(s.appliedAt(row!.id) > s.gateAt(revisedPlan(1)), "applied only after its revised plan was persisted");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("an ask_user park and wake inside the revision turn: the question is answered, the revise settles after persistence", () =>
    scenario(async (s) => {
      s.model.askInNextRevise = true;
      const first = await s.toFirstGate();
      const answers: UserInput[] = [];
      api.onState(s.runId, (b) => {
        if (b.status === "awaiting_input" && b.open_question_id) answers.push(...s.send(s.answer(b.open_question_id)));
      });
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.ok(s.statuses(first).includes("awaiting_input"), `the revision turn parked on its question: ${s.statuses(first).join(",")}`);
      assert.equal(answers.length, 1, "one answer was sent");
      assert.ok(api.isApplied(s.runId, answers[0]!.id), "the answer is applied");
      assert.equal(s.gates(first)[1]?.plan_md, revisedPlan(1), "the revised plan is gated");
      assert.ok(s.appliedAt(row!.id) > s.gateAt(revisedPlan(1)), "the revise is applied only after its revised plan was persisted");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("a mixed revise / follow_up / answer batch: the others settle promptly, the revise after persistence", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      const [rev, fu, ans] = s.send(
        s.input("revise_plan", FEEDBACK),
        s.input("follow_up", "also update the changelog"),
        s.answer("q-not-open"),
      );
      await block.entered;
      assert.ok(await until(() => api.isApplied(s.runId, fu!.id) && api.isApplied(s.runId, ans!.id), 3_000), "the follow_up and answer are applied while the revision runs");
      assert.equal(api.isApplied(s.runId, rev!.id), false, "the revise waits for its revised plan");
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.ok(s.appliedAt(rev!.id) > s.gateAt(revisedPlan(1)), "the revise is applied after persistence");
      assert.equal(s.model.revisions, 1);
      // Not asserted: that the follow-up reaches an implement turn. A follow-up is folded into the
      // turn AFTER the current one, and this scripted lead signals done on its first implement
      // turn, so on the base code too it never reaches a prompt (independent of #1604).
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }, {}, "mixed revise / follow_up / answer batch"));

  it("repeated GETs of the deferred revise neither re-ACK nor re-route it, while newer inputs still flow", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      const [rev] = s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      const gets = api.inputGets.get(s.runId) ?? 0;
      assert.ok(await until(() => (api.inputGets.get(s.runId) ?? 0) >= gets + 10, 3_000), "the channel keeps polling during the revision");
      const [fu] = s.send(s.input("follow_up", "keep the public API"));
      assert.ok(await until(() => api.isApplied(s.runId, fu!.id), 3_000), "a newer input is ACKed and applied during the deferral");
      assert.equal(api.isApplied(s.runId, rev!.id), false, "the revise is still deferred");
      assert.equal(s.acks(rev!.id), 1, "the deferred revise was ACKed once, not per GET");
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.equal(s.model.revisions, 1, "one revision");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("a declined awaiting_approval ack leaves the revise unapplied; the reclaim replays it", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      api.failStateWhen(s.runId, (b) => b.status === "awaiting_approval" && b.plan_md === revisedPlan(1), { httpStatus: 409, runStatus: "running" });
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.model.revisions >= 1));
      await tick(150);
      assert.equal(api.isApplied(s.runId, row!.id), false, "a declined revised-plan report does not settle the revise");
      api.requestCredentialSwitch(s.runId, 1);
      await s.finish(first);
      assert.ok(s.statuses(first).includes("credential_switch"), s.statuses(first).join(","));
      assert.equal(s.persistedGate()?.plan_md, PLAN_V1, "the server still holds the submitted plan");
      const { flight, gateAt } = await resumeAndApprove(s, s.resumeClaim("kept"));
      assertRevisedOnResume(s, flight, FEEDBACK, "kept");
      assert.ok(s.appliedAt(row!.id, flight.timelineFrom) > gateAt, "applied after the replayed revision was persisted");
      assert.ok(s.statuses(flight).includes("completed"), s.statuses(flight).join(","));
    }));

  it("revision-limit exhaustion is explicit and the exhausted revise is applied", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const [row] = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.ok(s.texts(first).includes("revision budget exhausted — not revising the plan further"), s.texts(first).join(" | "));
      assert.equal(s.model.count("revise"), 0, "no revision turn past the limit");
      assert.deepEqual(s.gates(first).map((g) => g.plan_md), [PLAN_V1, PLAN_V1], "the current plan is re-gated");
      assert.ok(api.isApplied(s.runId, row!.id), "the exhausted revise is applied");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }, { config: { plan_max_revisions: 0 } as ClaimResponse["config"] }));

  it("a stale reject (sent during the revision) is applied with the stale notice", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      const [rej] = s.send(s.input("reject_plan", "no"));
      assert.ok(await until(() => api.isAcked(s.runId, rej!.id)));
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      s.send(s.input("approve_plan"));
      await s.finish(first);
      assert.ok(s.texts(first).includes(STALE_REJECT_NOTICE), s.texts(first).join(" | "));
      assert.ok(api.isApplied(s.runId, rej!.id), "the stale reject is applied");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }, {}, "stale reject during revision"));

  it("a reject superseded in the buffer by a newer same-epoch verdict is applied with a notice", () =>
    scenario(async (s) => {
      const block = s.model.block("plan");
      const first = s.start(s.claim());
      await block.entered;
      const [rej, ap] = s.send(s.input("reject_plan", "no"), s.input("approve_plan"));
      assert.ok(await until(() => api.isAcked(s.runId, ap!.id)));
      block.release();
      await s.finish(first);
      assert.ok(s.texts(first).includes(SUPERSEDED_REJECT_NOTICE), s.texts(first).join(" | "));
      assert.ok(api.isApplied(s.runId, rej!.id), "the superseded reject is applied");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));
});

describe("#1604 — receipt scheduling around a revise that becomes ready", () => {
  it("during another batch's slow ACK: the revise is applied while that ACK is in flight; later reports wait for both", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      const [rev] = s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      api.delayInputReceipts("ack", 1_500);
      const [fu] = s.send(s.input("follow_up", "keep the changelog"));
      assert.ok(await until(() => api.inputReceiptCalls.some((c) => c.kind === "ack" && c.ids.includes(fu!.id))));
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished), s.statuses(first).join(","));
      api.delayInputReceipts("ack", 0);
      s.send(s.input("approve_plan"));
      await s.finish(first);
      const fuAckReply = api.timeline.findIndex((e) => e.type === "receipt_reply" && e.kind === "ack" && e.ids.includes(fu!.id) && e.httpStatus === 200);
      const revApplied = s.appliedAt(rev!.id);
      const v2At = s.gateAt(revisedPlan(1));
      assert.ok(revApplied > v2At, "the revise is applied after its revised plan was persisted");
      assert.ok(revApplied < fuAckReply, "the ready revise was applied while the other batch's slow ACK was in flight");
      const runningAt = s.stateAt("running", v2At);
      assert.ok(runningAt > Math.max(revApplied, s.appliedAt(fu!.id)), "the next report waited for both receipts");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));

  it("during a slow APPLIED: both settle and the next report waits for both", () =>
    scenario(async (s) => {
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      const [rev] = s.send(s.input("revise_plan", FEEDBACK));
      await block.entered;
      api.delayInputReceipts("applied", 800);
      const [fu] = s.send(s.input("follow_up", "keep the changelog"));
      assert.ok(await until(() => api.inputReceiptCalls.some((c) => c.kind === "applied" && c.ids.includes(fu!.id))));
      block.release();
      assert.ok(await until(() => s.gates(first).length >= 2 || first.finished, 10_000), s.statuses(first).join(","));
      api.delayInputReceipts("applied", 0);
      s.send(s.input("approve_plan"));
      await s.finish(first);
      const v2At = s.gateAt(revisedPlan(1));
      assert.ok(s.appliedAt(rev!.id) > v2At, "the revise is applied after its revised plan was persisted");
      assert.ok(s.appliedAt(fu!.id) >= 0, "the follow-up is applied");
      const runningAt = s.stateAt("running", v2At);
      assert.ok(runningAt > Math.max(s.appliedAt(rev!.id), s.appliedAt(fu!.id)), "the next report waited for both receipts");
      assert.ok(s.statuses(first).includes("completed"), s.statuses(first).join(","));
    }));
});

/** A single resumed claim at the gate (session kept) with `rows` sent before it was claimed. */
