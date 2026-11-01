import { it } from "node:test";
import assert from "node:assert/strict";
import { gatedPlanPresentation } from "../src/checked-plan.js";

const raw = { plan: "Implement A\u202e", milestones: [{ id: "m1", title: "A glpat-" + "a".repeat(20) }] };
const checked = { plan: "Implement A", milestones: [{ id: "m1", title: "A [redacted]" }], candidate_digest: "a".repeat(64) };

it("semantic gated presentation preserves raw plan bytes but uses checked normalized milestones", () => {
  assert.deepEqual(gatedPlanPresentation(raw, checked, checked.candidate_digest), {
    plan: raw.plan, milestones: checked.milestones, candidate_digest: checked.candidate_digest,
  });
});

it("structural gated presentation uses the full normalized candidate", () => {
  assert.deepEqual(gatedPlanPresentation(raw, checked), checked);
});

it("semantic gated presentation preserves an unchanged candidate", () => {
  assert.deepEqual(gatedPlanPresentation(checked, checked, checked.candidate_digest), checked);
});
