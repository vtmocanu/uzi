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

  // PRD #1190 M2 — the server-decided pause boundary rides the same {run: RunDTO} body.
  it("reads pause_requested:true off the run body", async () => {
    const out = await readRunAck(jsonResponse({ run: { pause_requested: true } }));
    assert.equal(out.pauseRequested, true, "the server-decided pause boundary is passed through");
  });

  it("reads pause_requested:false off the run body", async () => {
    const out = await readRunAck(jsonResponse({ run: { pause_requested: false } }));
    assert.equal(out.pauseRequested, false, "a false boundary is passed through, not dropped");
  });

  it("leaves pauseRequested absent when the field is missing or non-boolean (older server)", async () => {
    const absent = await readRunAck(jsonResponse({ run: { milestones_completed: [] } }));
    assert.equal(absent.pauseRequested, undefined, "absent field ⇒ absent (read as no pause)");
    const nonBool = await readRunAck(jsonResponse({ run: { pause_requested: "yes" } }));
    assert.equal(nonBool.pauseRequested, undefined, "a non-boolean value is ignored, never coerced");
  });

  // PRD #1226 M4 (D3) — the server-decided budget_exhausted steer rides the same {run: RunDTO}
  // body beside pause_requested, off the run's completion_budget_exhausted field.
  it("reads completion_budget_exhausted:true off the run body", async () => {
    const out = await readRunAck(jsonResponse({ run: { completion_budget_exhausted: true } }));
    assert.equal(out.budgetExhausted, true, "the server-decided budget steer is passed through");
  });

  it("reads completion_budget_exhausted:false off the run body", async () => {
    const out = await readRunAck(jsonResponse({ run: { completion_budget_exhausted: false } }));
    assert.equal(out.budgetExhausted, false, "a false steer is passed through, not dropped");
  });

  it("leaves budgetExhausted absent when the field is missing or non-boolean (older server)", async () => {
    const absent = await readRunAck(jsonResponse({ run: { milestones_completed: [] } }));
    assert.equal(absent.budgetExhausted, undefined, "absent field ⇒ absent (read as no steer)");
    const nonBool = await readRunAck(jsonResponse({ run: { completion_budget_exhausted: "yes" } }));
    assert.equal(nonBool.budgetExhausted, undefined, "a non-boolean value is ignored, never coerced");
  });

  // PRD #1189 M1 (D6) — the served TOTAL wall (frozen budget + owner extension) rides the same
  // {run: RunDTO} body as the other budget fields. The sdk-executor prefers it over
  // budget_wall_seconds; readRunAck must surface it as a number, and leave it ABSENT (never coerced)
  // for null / missing / non-number so the executor's back-compat fallback to budget_wall_seconds fires.
  it("reads budget_total_seconds off the run body as budgetTotalSeconds", async () => {
    const out = await readRunAck(jsonResponse({ run: { status: "running", budget_total_seconds: 36000 } }));
    assert.equal(out.budgetTotalSeconds, 36000, "the served total wall is passed through as a number");
  });

  it("leaves budgetTotalSeconds absent when budget_total_seconds is null (a run with no wall deadline)", async () => {
    const out = await readRunAck(jsonResponse({ run: { status: "running", budget_total_seconds: null } }));
    assert.equal(out.budgetTotalSeconds, undefined, "null ⇒ absent, so the executor falls back to budget_wall_seconds");
  });

  it("leaves budgetTotalSeconds absent when the field is missing (older api)", async () => {
    const out = await readRunAck(jsonResponse({ run: { status: "running" } }));
    assert.equal(out.budgetTotalSeconds, undefined, "an older api that omits the field ⇒ absent");
  });

  it("ignores a non-number budget_total_seconds, never coercing it", async () => {
    const str = await readRunAck(jsonResponse({ run: { budget_total_seconds: "36000" } }));
    assert.equal(str.budgetTotalSeconds, undefined, "a string is ignored, not coerced to a number");
    const bool = await readRunAck(jsonResponse({ run: { budget_total_seconds: true } }));
    assert.equal(bool.budgetTotalSeconds, undefined, "a boolean is ignored");
  });

  // PRD #1392 M2 (D10) — the forge-unreachable park ack rides a {run, reason?} body: the
  // disposition `reason` is TOP-LEVEL (stale_claim / custody_unsettled on a 409), while the
  // retry-not-before stamp is a FIELD ON the RunDTO (run.recovery_retry_not_before). Both
  // string-only; a missing / wrong-typed value leaves them ABSENT so the dispatch reads its safe
  // defaults ("no reason" / "retry after backoff").
  it("reads the disposition reason off the TOP LEVEL, not from run", async () => {
    const out = await readRunAck(
      jsonResponse({ reason: "stale_claim", run: { status: "queued" } }),
    );
    assert.equal(out.reason, "stale_claim", "reason is parsed from the top-level body");
  });

  it("does NOT read reason from inside run (it is top-level only)", async () => {
    const out = await readRunAck(jsonResponse({ run: { status: "queued", reason: "stale_claim" } }));
    assert.equal(out.reason, undefined, "a reason nested under run is ignored — it is a top-level field");
  });

  it("reads recovery_retry_not_before off the RUN body", async () => {
    const out = await readRunAck(
      jsonResponse({ run: { status: "recovery_wait", recovery_retry_not_before: "2026-09-15T16:00:00Z" } }),
    );
    assert.equal(
      out.recoveryRetryNotBefore,
      "2026-09-15T16:00:00Z",
      "the retry stamp is parsed from the RunDTO",
    );
  });

  it("does NOT read recovery_retry_not_before from the top level (it is a run field)", async () => {
    const out = await readRunAck(
      jsonResponse({ recovery_retry_not_before: "2026-09-15T16:00:00Z", run: { status: "recovery_wait" } }),
    );
    assert.equal(out.recoveryRetryNotBefore, undefined, "a top-level stamp is ignored — it lives on run");
  });

  it("leaves reason and recovery_retry_not_before absent when missing or wrong-typed", async () => {
    const missing = await readRunAck(jsonResponse({ run: { status: "recovery_wait" } }));
    assert.equal(missing.reason, undefined, "absent top-level reason ⇒ absent");
    assert.equal(missing.recoveryRetryNotBefore, undefined, "absent run stamp ⇒ absent");
    const wrongType = await readRunAck(
      jsonResponse({ reason: 7, run: { status: "recovery_wait", recovery_retry_not_before: 123 } }),
    );
    assert.equal(wrongType.reason, undefined, "a non-string reason is ignored, never coerced");
    assert.equal(wrongType.recoveryRetryNotBefore, undefined, "a non-string stamp is ignored, never coerced");
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

// Issue #1626 — the frozen completion-contract revision rides the same {run: RunDTO} body
// (RunDTO.completion_revision), so a fresh interlocked run learns the revision frozen AFTER its
// claim. A positive integer only; anything else leaves it undefined ("no revision update").
describe("readRunAck completion_revision (issue #1626)", () => {
  it("reads a numeric completion_revision off the run body", async () => {
    const out = await readRunAck(jsonResponse({ run: { status: "running", completion_revision: 1 } }));
    assert.equal(out.contractRevision, 1, "the frozen revision is passed through");
    const bumped = await readRunAck(jsonResponse({ run: { completion_revision: 3 } }));
    assert.equal(bumped.contractRevision, 3, "a revised contract's revision is passed through too");
  });

  it("leaves contractRevision undefined for null, string, absent or non-positive-integer values", async () => {
    for (const value of [null, "1", 0, -1, 1.5, true]) {
      const out = await readRunAck(jsonResponse({ run: { completion_revision: value } }));
      assert.equal(out.contractRevision, undefined, `completion_revision ${JSON.stringify(value)} is ignored`);
    }
    const absent = await readRunAck(jsonResponse({ run: { status: "running" } }));
    assert.equal(absent.contractRevision, undefined, "an absent field (legacy/never-frozen run, older server) is undefined");
  });
});
