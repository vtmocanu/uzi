import { expect, it } from "vitest";
import wording from "../../../fixtures/policy-hold/wording.json";
import { POLICY_HOLD_WORDING, isPolicyHoldResumeMode, policyHoldCauseText } from "./policyHoldWording";

it("carries the shared wording table verbatim", () => {
  const { _comment, ...table } = wording;
  void _comment;
  expect(POLICY_HOLD_WORDING).toStrictEqual(table);
});

it("falls back to unknown_cause for a null, absent, unknown or prototype-named cause", () => {
  expect(policyHoldCauseText("attempt_path")).toBe(wording.causes.attempt_path);
  for (const cause of [null, undefined, "", "new_future_cause", "constructor", "__proto__"]) {
    expect(policyHoldCauseText(cause)).toBe(wording.unknown_cause);
  }
});

it("recognizes only the modes the table words", () => {
  expect(["preserve", "fresh_from_saved_work", "fresh_one_claim"].every(isPolicyHoldResumeMode)).toBe(true);
  expect(isPolicyHoldResumeMode("resume")).toBe(false);
  expect(isPolicyHoldResumeMode("toString")).toBe(false);
});
