// Issue #3974: a plan-REVISION planning turn runs inside a credential-switch defer window (the SDK
// executor's ctx.deferCredentialSwitch). A switch stamped meanwhile withholds every input row, so an
// owner Cancel never arrives through GET inputs; the worker's ownership probe finds it. Driven through
// the real SdkExecutor and RunRunner (runner-gate-verdict-interruption-fixture.ts), the cancel ends the
// run as cancelled: the revised plan is never stored, no gate is presented for it, and the switch is
// never serviced.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { UserInput } from "../src/protocol.js";
import { api, installHarness } from "./runner-harness.js";
import { waitForTestEvent } from "./helpers.js";
import {
  FEEDBACK,
  PLAN_V1,
  revisedPlan,
  until,
  scenario,
  type Flight,
  type Scenario,
} from "./runner-gate-verdict-interruption-fixture.js";

installHarness();

/** The run ended cancelled under the withheld switch: nothing revised, nothing presented, no switch. */
function assertCancelledUnderDeferredSwitch(s: Scenario, flight: Flight, withheld: UserInput[]): void {
  const states = s.states(flight);
  assert.ok(states.some((st) => st.status === "failed" && st.failure_reason?.includes("run cancelled")),
    `terminal failure "run cancelled"; got ${JSON.stringify(states.map((st) => [st.status, st.failure_reason]))}`);
  assert.deepEqual(states.map((st) => st.status).filter((st) => st.startsWith("credential_switch")), [],
    "no credential_switch* report");
  assert.equal(s.gateAt(revisedPlan(1)), -1, "the revised plan was never stored");
  assert.deepEqual(s.gates(flight).map((g) => g.plan_md), [PLAN_V1], "no gate beyond the first presentation");
  assert.ok(!s.statuses(flight).includes("completed"));
  for (const row of withheld) {
    assert.equal(api.isAcked(s.runId, row.id), false, `row ${row.id} was never ACKed by the old flight`);
    assert.equal(api.isApplied(s.runId, row.id), false, `row ${row.id} was never applied by the old flight`);
  }
}

describe("#3974 — an owner cancel during a revision turn under a deferred credential switch", () => {
  it("interrupts the revision turn promptly; the switch is never serviced", () =>
    scenario(async (s) => {
      api.deriveCancellationRequested = true;
      const first = await s.toFirstGate();
      const block = s.model.block("revise");
      s.send(s.input("revise_plan", FEEDBACK));
      await waitForTestEvent(block.entered, "model turn entered");
      api.requestCredentialSwitch(s.runId, 1);
      const sent = s.send(s.input("cancel"));
      // The turn is never released: only the cancel can end it.
      assert.ok(await until(() => first.finished), "the cancel interrupted the held revision turn");
      assertCancelledUnderDeferredSwitch(s, first, sent);
      assert.equal(s.model.revisions, 0, "no revision was produced");
    }, {}, "cancel interrupts the deferred revision turn"));

  it("an ask_user sub-park inside the revision turn resolves as a cancel", () =>
    scenario(async (s) => {
      api.deriveCancellationRequested = true;
      s.model.askInNextRevise = true;
      const first = await s.toFirstGate();
      const revise = s.send(s.input("revise_plan", FEEDBACK));
      assert.ok(await until(() => s.statuses(first).includes("awaiting_input")), "the revision turn parked on its question");
      api.requestCredentialSwitch(s.runId, 1);
      const sent = s.send(s.input("cancel"));
      assert.ok(await until(() => first.finished), "the cancel ended the question park");
      assertCancelledUnderDeferredSwitch(s, first, sent);
      assert.ok(s.statuses(first).includes("awaiting_input"));
      assert.equal(api.isApplied(s.runId, revise[0]!.id), false, "the revise waiting on its revised plan was never applied");
    }, {}, "cancel resolves the ask_user sub-park"));

  it("a revision turn that ignores the abort and submits its revised plan after the cancel was committed: the plan is never stored", () =>
    scenario(async (s) => {
      api.deriveCancellationRequested = true;
      const first = await s.toFirstGate();
      const block = s.model.block("revise", { ignoreAbort: true });
      s.send(s.input("revise_plan", FEEDBACK));
      await waitForTestEvent(block.entered, "model turn entered");
      api.requestCredentialSwitch(s.runId, 1);
      const sent = s.send(s.input("cancel"));
      // The probe finds the cancel while the turn (which ignores the abort) is still inside the defer window.
      assert.ok(await until(() => api.ownershipRequests.length > 0), "the worker probed ownership");
      assert.equal(first.finished, false, "the held turn keeps the flight alive until it finishes");
      block.release();
      assert.ok(await until(() => first.finished), "the flight ended once the turn finished");
      assertCancelledUnderDeferredSwitch(s, first, sent);
    }, {}, "turn finishing after the committed cancel"));
});
