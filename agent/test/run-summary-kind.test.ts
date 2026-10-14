import assert from "node:assert/strict";
import { it } from "node:test";
import { isRunSummaryKind } from "../src/run-kind.js";
import { RUN_KINDS } from "../src/protocol.js";

it("M1 shared summary eligibility covers the complete typed kind registry", () => {
  assert.equal(isRunSummaryKind(undefined), true);
  for (const kind of RUN_KINDS) {
    assert.equal(isRunSummaryKind(kind), ["issue", "self_improve", "prompt"].includes(kind), kind);
  }
});
