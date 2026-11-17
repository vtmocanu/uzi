import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  classifySpendLimitTerminal, CREDIT_BALANCE_LOW_TEXT, SPEND_LIMIT_ORG_TEXT, SPEND_LIMIT_WORKSPACE_TEXT,
  SpendLimitHoldError,
} from "../src/limit.js";

// Issue #4426: the Claude CLI's credit-depletion sentence, written literally here so the holding
// cases below pin the classifier's acceptance, not merely the constant's value.
const CREDIT = "Credit balance is too low";
import { decodeResult } from "../src/sdk-messages.js";

// Issue #3767: the single text-matched classification. Structural facts AND an exact sentence.
const hold = (resultText: string | undefined, over: Record<string, unknown> = {}) =>
  classifySpendLimitTerminal({ isError: true, terminalReason: "api_error", apiErrorStatus: 400, resultText, ...over });

describe("classifySpendLimitTerminal (issue #3767)", () => {
  it("CREDIT_BALANCE_LOW_TEXT is the CLI's exact sentence (issue #4426)", () => {
    assert.equal(CREDIT_BALANCE_LOW_TEXT, CREDIT);
  });
  for (const sentence of [SPEND_LIMIT_ORG_TEXT, SPEND_LIMIT_WORKSPACE_TEXT, CREDIT_BALANCE_LOW_TEXT]) {
    for (const wrap of ["", "API Error: ", "API Error: 400 "]) {
      it(`holds ${JSON.stringify(wrap + sentence)}`, () => {
        const got = hold(`${wrap}${sentence} You will regain access on 2026-12-01.`);
        assert.ok(got);
        assert.ok(got.detail.startsWith(sentence));
      });
    }
  }
  const cases: Array<[string, string | undefined, Record<string, unknown>]> = [
    ["a different 400 text", "API Error: 400 invalid_request_error: bad field", {}],
    ["the prefix mid-string", `Note: ${SPEND_LIMIT_ORG_TEXT}`, {}],
    ["a lowercase variant", SPEND_LIMIT_ORG_TEXT.toLowerCase(), {}],
    ["a doubled wrapper", `API Error: 400 API Error: 400 ${SPEND_LIMIT_ORG_TEXT}`, {}],
    ["lowercase wrapper", `api error: 400 ${SPEND_LIMIT_ORG_TEXT}`, {}],
    ["status 429", SPEND_LIMIT_ORG_TEXT, { apiErrorStatus: 429 }],
    ["status 402", SPEND_LIMIT_ORG_TEXT, { apiErrorStatus: 402 }],
    ["null status", SPEND_LIMIT_ORG_TEXT, { apiErrorStatus: null }],
    ["terminal max_turns", SPEND_LIMIT_ORG_TEXT, { terminalReason: "max_turns" }],
    ["terminal undefined", SPEND_LIMIT_ORG_TEXT, { terminalReason: undefined }],
    ["isError false", SPEND_LIMIT_ORG_TEXT, { isError: false }],
    ["isError undefined", SPEND_LIMIT_ORG_TEXT, { isError: undefined }],
    ["no text", undefined, {}],
    // Issue #4426: the credit-depletion sentence takes every gate the spend sentences take.
    ["credit: a lowercase variant", CREDIT.toLowerCase(), {}],
    ["credit: the sentence mid-string", `Note: ${CREDIT}`, {}],
    ["credit: status 429", CREDIT, { apiErrorStatus: 429 }],
    ["credit: status 402", CREDIT, { apiErrorStatus: 402 }],
    ["credit: terminal max_turns", CREDIT, { terminalReason: "max_turns" }],
    ["credit: isError false", CREDIT, { isError: false }],
  ];
  it("holds the bare credit-depletion line the CLI emits (issue #4426)", () => {
    assert.deepEqual(hold(CREDIT), { detail: CREDIT });
  });
  for (const wrap of ["", "API Error: ", "API Error: 400 "]) {
    it(`holds the literal credit sentence under ${JSON.stringify(wrap)} (issue #4426)`, () => {
      assert.deepEqual(hold(`${wrap}${CREDIT}`), { detail: CREDIT });
    });
  }
  for (const [name, text, over] of cases) {
    it(`does not hold ${name}`, () => assert.equal(hold(text, over), undefined));
  }

  it("does not hold an explicit successful result row", () => {
    assert.equal(hold(undefined, { isError: false, terminalReason: "completed", apiErrorStatus: undefined }), undefined);
    assert.equal(hold(SPEND_LIMIT_ORG_TEXT, { isError: false, terminalReason: "completed", apiErrorStatus: undefined }), undefined);
  });

  it("SpendLimitHoldError carries its message and detail", () => {
    const e = new SpendLimitHoldError("provider error: 400 x", "x");
    assert.equal(e.name, "SpendLimitHoldError");
    assert.equal(e.message, "provider error: 400 x");
    assert.equal(e.detail, "x");
  });
});

describe("decodeResult isError feeds the classifier (issue #3767)", () => {
  const frame = (isError: unknown): Record<string, unknown> => ({
    type: "result", subtype: "success", api_error_status: 400, terminal_reason: "api_error",
    result: `API Error: 400 ${SPEND_LIMIT_ORG_TEXT}`, ...(isError === "absent" ? {} : { is_error: isError }),
  });
  it("holds only for the literal boolean true", () => {
    const d = decodeResult(frame(true));
    assert.equal(d.isError, true);
    assert.ok(classifySpendLimitTerminal(d));
  });
  for (const bad of [false, "absent", "true", 1]) {
    it(`does not hold with is_error ${JSON.stringify(bad)}`, () => {
      const d = decodeResult(frame(bad));
      assert.equal(d.isError, false);
      assert.equal(classifySpendLimitTerminal(d), undefined);
    });
  }
});
