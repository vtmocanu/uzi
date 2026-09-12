// PRD #1287 C1 — self-tests for the O-receipt merge gate (D8). The pure `checkReceipts` rejects
// any owed (or malformed) O row and accepts an all-inherited/receipt-present set. Also asserts the
// REAL registry currently has the seeded owed row (so the LEAD merge gate is genuinely red at C1).

import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { checkReceipts } from "./receipts.js";
import { ALL_CLAUSES } from "./registry.js";
import type { ClauseRow, OState } from "./clause.js";

function orow(id: string, o: OState | undefined): ClauseRow {
  return {
    id,
    adapter: "codex",
    layer: "O",
    family: "F",
    seam: "seam",
    positiveControl: "pc",
    negativeOracle: "no",
    intendedOutcome: "io",
    tests: [],
    ...(o === undefined ? {} : { o }),
  };
}

const INHERITED: OState = {
  kind: "inherited",
  source: "src",
  imageDigest: "sha256:beef",
  target: "image",
  unchangedJustification: "unchanged",
};
const RECEIPT: OState = { kind: "receipt-present", source: "src", imageDigest: "sha256:cafe", target: "image" };
const OWED: OState = { kind: "owed", target: "image", reason: "needs proof", owner: "maintainer" };

describe("checkReceipts", () => {
  it("rejects when any O row is owed, listing target/reason/owner", () => {
    const report = checkReceipts([orow("a", INHERITED), orow("b", OWED)]);
    assert.equal(report.ok, false);
    assert.equal(report.owed.length, 1);
    assert.equal(report.owed[0]?.id, "b");
    assert.equal(report.owed[0]?.owner, "maintainer");
    assert.equal(report.owed[0]?.reason, "needs proof");
  });

  it("accepts an all-inherited / receipt-present set", () => {
    const report = checkReceipts([orow("a", INHERITED), orow("b", RECEIPT)]);
    assert.deepEqual(report.owed, []);
    assert.deepEqual(report.malformed, []);
    assert.equal(report.ok, true);
  });

  it("rejects a malformed O row (no receipt can be read)", () => {
    const report = checkReceipts([orow("bad", undefined)]);
    assert.equal(report.ok, false);
    assert.deepEqual(report.malformed, ["bad"]);
  });

  it("ignores non-O rows", () => {
    const nonO: ClauseRow = {
      id: "u",
      adapter: "claude",
      layer: "U",
      family: "F",
      seam: "s",
      positiveControl: "pc",
      negativeOracle: "no",
      intendedOutcome: "io",
      tests: ["t"],
    };
    assert.equal(checkReceipts([nonO]).ok, true);
  });

  it("finds the seeded owed O row in the REAL registry (the C1 merge gate is red on purpose)", () => {
    const report = checkReceipts(ALL_CLAUSES);
    assert.equal(report.ok, false, "the C1 registry seeds an owed O row, so the merge gate must be red");
    assert.ok(
      report.owed.some((row) => row.id === "codex-o-descendant-code-mode-host-absence"),
      "the descendant code-mode-host absence O row is owed to the maintainer",
    );
  });
});
