// Issue #1626 (N-e): pins RunTurnReducerImpl's last-wins fold across the two EXCLUSIVE shapes of
// a submit_plan milestone list — a valid `milestones` list and a `rejectedMilestones` list (the
// wire-only shape the server rejects as a unit). Whichever submit_plan came LAST in the turn is
// what the gate sends; the earlier shape must be deleted, or `rejectedMilestones ?? milestones`
// at the gate would send a stale rejected list over a later valid one (or vice versa).

import test from "node:test";
import assert from "node:assert/strict";

import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import type { HarnessContext, HarnessContextHook, HarnessEvent, TurnSignals } from "../src/harness.js";

const noContext: HarnessContextHook = {
  request() {},
  async get(): Promise<HarnessContext | undefined> {
    return undefined;
  },
};

const signalFrame = (signals: Partial<TurnSignals>): HarnessEvent => ({
  kind: "frame",
  origin: { kind: "main" },
  attribution: {},
  items: [],
  signals,
});

async function reduce(events: HarnessEvent[]) {
  const r = new RunTurnReducerImpl(noContext);
  r.beginTurn();
  for (const e of events) await r.accept(e);
  return r.finish({ kind: "exhausted" }).result;
}

const valid = [{ id: "m1", title: "First" }];
const rejected = [
  { id: "m1", title: "First" },
  { id: "", title: "no id" },
];

test("issue #1626 reducer: a valid submit_plan after a malformed one in the same turn sends the valid list", async () => {
  const result = await reduce([
    signalFrame({ plan: "p1", rejectedMilestones: rejected }),
    signalFrame({ plan: "p2", milestones: valid }),
  ]);
  assert.equal(result.plan, "p2");
  assert.deepEqual(result.milestones, valid);
  assert.equal(result.rejectedMilestones, undefined, "the earlier rejected list must be cleared");
  // What the gate sends (sdk-executor / codex-executor: rejectedMilestones ?? milestones).
  assert.deepEqual(result.rejectedMilestones ?? result.milestones, valid);
});

test("issue #1626 reducer: a malformed submit_plan after a valid one in the same turn sends the rejected list", async () => {
  const result = await reduce([
    signalFrame({ plan: "p1", milestones: valid }),
    signalFrame({ plan: "p2", rejectedMilestones: rejected }),
  ]);
  assert.equal(result.plan, "p2");
  assert.deepEqual(result.rejectedMilestones, rejected);
  assert.equal(result.milestones, undefined, "the earlier valid list must be cleared");
  assert.deepEqual(result.rejectedMilestones ?? result.milestones, rejected);
});
