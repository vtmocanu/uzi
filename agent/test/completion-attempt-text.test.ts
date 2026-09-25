import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { RunContext } from "../src/executor.js";
import {
  STALL_LIMIT,
  REASON_COMPLETION_NO_PROGRESS,
  REASON_COMPLETION_BUDGET_EXHAUSTED,
  completionAttemptFingerprint,
  updateCompletionStreak,
  routeCompletionHold,
  buildCompletionReworkFollowUp,
} from "../src/completion-attempt.js";

describe("shared completion attempt", () => {
  const expected = [
    "Completion check (structural interlock): you signalled done, but the frozen completion",
    "contract still has milestone(s) NOT declared complete:",
    "",
    "- m2: Ship the fix",
    "- m3",
    "",
    "Finish the remaining milestone(s) and declare each complete (report_progress / signal_done),",
    "or record an explicit decision if one genuinely cannot be completed. Do not signal done again",
    "until every frozen milestone above is complete — an incomplete run cannot open its closing PR.",
  ].join("\n");

  it("keeps the autonomous follow-up text exact", () => {
    assert.equal(
      buildCompletionReworkFollowUp(
        ["m2", "m3"],
        [{ id: "m2", title: "Ship the fix" }],
      ),
      expected,
    );
  });

  it("appends trimmed owner direction to the exact base text", () => {
    assert.equal(
      buildCompletionReworkFollowUp(
        ["m2", "m3"],
        [{ id: "m2", title: "Ship the fix" }],
        "  Check the API contract.\nThen rerun the focused test.  ",
      ),
      [
        expected,
        "",
        "The run owner reviewed this completion block and chose to CONTINUE, with direction:",
        "",
        "Check the API contract.\nThen rerun the focused test.",
      ].join("\n"),
    );
    assert.equal(buildCompletionReworkFollowUp(["m2", "m3"], [{ id: "m2", title: "Ship the fix" }], "  "), expected);
  });

  it("sorts only the fingerprint and counts consecutive identical attempts", () => {
    const unmet = ["m3", "m2"];
    const first = completionAttemptFingerprint(unmet, "head", "head\n M file");
    assert.equal(first, JSON.stringify([["m2", "m3"], "head", "head\n M file"]));
    assert.deepEqual(unmet, ["m3", "m2"]);
    let streak = updateCompletionStreak(first, undefined, 0);
    assert.equal(streak, 1);
    streak = updateCompletionStreak(first, first, streak);
    streak = updateCompletionStreak(first, first, streak);
    assert.equal(streak, STALL_LIMIT);
    assert.equal(updateCompletionStreak(completionAttemptFingerprint(unmet, "new", "new\n"), first, streak), 1);
  });

  it("routes a hold only after an attempt and preserves the hold verdict", async () => {
    const reasons: string[] = [];
    const ctx = {
      enterCompletionHold: async (reason: string) => {
        reasons.push(reason);
        return false;
      },
    } as RunContext;
    assert.equal(await routeCompletionHold(ctx, REASON_COMPLETION_NO_PROGRESS, false), false);
    assert.deepEqual(reasons, []);
    assert.equal(await routeCompletionHold(ctx, REASON_COMPLETION_NO_PROGRESS, true), false);
    assert.deepEqual(reasons, [REASON_COMPLETION_NO_PROGRESS]);
    assert.equal(
      await routeCompletionHold(
        { enterCompletionHold: async () => true } as RunContext,
        REASON_COMPLETION_BUDGET_EXHAUSTED,
        true,
      ),
      true,
    );
  });
});
