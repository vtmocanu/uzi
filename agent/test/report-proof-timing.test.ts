import { it } from "node:test";
import type { TestContext } from "node:test";
import assert from "node:assert/strict";
import { git, installHarness } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
import { reportProofTiming } from "../src/report-proof-timing.js";

installHarness();
// Archive report retirement needs Linux procfs/no-follow content inspection.
const linux = { skip: process.platform !== "linux" && "report proof needs Linux procfs" };

/** Offset clock: Date.now() is real time plus `offset` (or a frozen instant plus `offset`).
 * No real waiting is ever involved. */
function offsetClock(t: TestContext, frozen = false) {
  const realNow = Date.now.bind(Date);
  const base = realNow();
  let offset = 0;
  const mock = t.mock.method(Date, "now", () => (frozen ? base : realNow()) + offset);
  return { advance: (ms: number) => { offset += ms; }, restore: () => mock.mock.restore() };
}

/** Injects `step` once per proof attempt, inside the real withReportProofBudget scope, and records
 * that attempt's deadline. `beginAttempt` marks a fresh retireRecoveryReport call. */
function injectStep(t: TestContext, f: Awaited<ReturnType<typeof reportRetirementFixture>>,
  step: () => void) {
  const state = { deadlines: [] as number[], reached: 0, fresh: false };
  const retire = f.r.retireRecoveryReport.bind(f.r);
  t.mock.method(f.r, "retireRecoveryReport", (...args: Parameters<typeof retire>) => {
    state.fresh = true;
    return retire(...args);
  });
  const real = git.withReportProofBudget.bind(git);
  t.mock.method(git, "withReportProofBudget", <T>(
    budget: { signal: AbortSignal; deadline: number }, action: () => Promise<T>) => {
    if (state.fresh) {
      state.fresh = false;
      state.deadlines.push(budget.deadline);
      return real(budget, async () => { state.reached++; step(); return action(); });
    }
    return real(budget, action);
  });
  return state;
}

it("a proof crossing the default 1000 ms budget is retained, each attempt with a fresh deadline", linux, async t => {
  const f = await reportRetirementFixture(t);
  const clock = offsetClock(t);
  try {
    const state = injectStep(t, f, () => clock.advance(1_001));
    for (let n = 1; n <= 3; n++) {
      await f.replay();
      assert.equal(state.reached, n, "the injected step ran inside this attempt's proof");
      assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, f.generation), true, "expired proof retains the report");
    }
    assert.equal(state.deadlines.length, 3);
    assert.ok(state.deadlines[1]! > state.deadlines[0]! && state.deadlines[2]! > state.deadlines[1]!,
      "every retry computes a fresh deadline");
  } finally { clock.restore(); }
  await f.assertCustody();
});

it("the same proof retires under the positive-stage headroom", linux, async t => {
  const f = await reportRetirementFixture(t);
  const clock = offsetClock(t);
  const restore = f.useHeadroom();
  try {
    const state = injectStep(t, f, () => clock.advance(1_001));
    const attempts = await f.positiveStage("headroom proof", f.replay,
      () => !f.outbox.hasPendingTerminal(f.claim.run_id, f.generation), f.assertPending);
    assert.equal(attempts, 1);
    assert.equal(state.reached, 1, "the clock was pushed past the default budget inside the proof");
  } finally { clock.restore(); restore(); }
  await f.assertCustody();
});

it("an earlier caller deadline still wins over headroom", linux, async t => {
  const f = await reportRetirementFixture(t);
  const captured = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, f.generation))!;
  const clock = offsetClock(t);
  const restore = f.useHeadroom();
  try {
    const state = injectStep(t, f, () => clock.advance(501));
    const callerDeadline = Date.now() + 500;
    await f.r.retireRecoveryReport(f.claim.run_id, f.generation, { ...captured.context, deadline: callerDeadline });
    assert.equal(state.reached, 1);
    assert.deepEqual(state.deadlines, [callerDeadline], "proof budget is the caller deadline, not the headroom");
  } finally { clock.restore(); restore(); }
  await f.assertPending();
});

it("cancellation still aborts under headroom", linux, async t => {
  const f = await reportRetirementFixture(t);
  const captured = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, f.generation))!;
  const cancel = new AbortController();
  const restore = f.useHeadroom();
  try {
    const state = injectStep(t, f, () => cancel.abort(new Error("test cancel")));
    await f.r.retireRecoveryReport(f.claim.run_id, f.generation, { ...captured.context, signal: cancel.signal });
    assert.equal(state.reached, 1);
  } finally { restore(); }
  await f.assertPending();
});

it("without the helper the production proof budget is 1000 ms", linux, async t => {
  const f = await reportRetirementFixture(t);
  assert.equal(reportProofTiming.proofBudgetMs(), 1_000);
  assert.equal(reportProofTiming.finalizeAuthorityMs(), 1_000);
  assert.equal(reportProofTiming.finalizePassMs(), 5_000);
  const captured = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, f.generation))!;
  const clock = offsetClock(t, true);
  try {
    const state = injectStep(t, f, () => clock.advance(1_001));
    const before = Date.now();
    await f.r.retireRecoveryReport(f.claim.run_id, f.generation, captured.context);
    assert.equal(state.reached, 1);
    assert.deepEqual(state.deadlines, [before + 1_000], "the proof's own deadline is exactly now + 1000 ms");
  } finally { clock.restore(); }
  await f.assertPending();
});
