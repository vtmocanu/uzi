import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readRunAck } from "../src/client.js";

// PRD #634 follow-up (M1) — readRunAck maps the {run: RunDTO} state-ACK body onto the
// worker's control fields. The regression this file guards: a run whose lead never
// reported progress marshals `milestones_completed` as JSON null (a nil Go slice), and the
// loop-top scope honor gate requires `typeof completedCount === "number"`. Before the fix a
// null/absent field left completedCount undefined, so a `uzi run stop` mapping to
// scope_ceiling=0 was silently NOT honored. The null path must now read completedCount 0.

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body));
}

describe("readRunAck", () => {
  it("treats milestones_completed:null as a completed count of 0 (ceiling-0 regression)", async () => {
    const out = await readRunAck(jsonResponse({ run: { milestones_completed: null, scope_ceiling: 0 } }));
    assert.equal(out.scopeCeiling, 0, "scope_ceiling: 0 is a real ceiling, not unbounded");
    assert.equal(out.completedCount, 0, "null milestones_completed reads as 0, not undefined");
  });

  it("reads the array length on the populated path", async () => {
    const out = await readRunAck(jsonResponse({ run: { milestones_completed: ["m1", "m2"], scope_ceiling: 4 } }));
    assert.equal(out.completedCount, 2, "length of the reported-progress array");
    assert.equal(out.scopeCeiling, 4);
  });

  it("treats an absent milestones_completed as a completed count of 0", async () => {
    const out = await readRunAck(jsonResponse({ run: { scope_ceiling: 3 } }));
    assert.equal(out.completedCount, 0, "absent field reads as 0, same as null");
    assert.equal(out.scopeCeiling, 3);
  });

  it("returns {} on an empty body (existing catch/total behavior)", async () => {
    const out = await readRunAck(new Response(""));
    assert.deepEqual(out, {}, "an empty body yields the fields absent");
  });

  it("returns {} on an unparseable body", async () => {
    const out = await readRunAck(new Response("not json{"));
    assert.deepEqual(out, {}, "malformed JSON yields the fields absent, never a throw");
  });
});
