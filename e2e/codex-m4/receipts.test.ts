// PRD #1287 C1 — self-tests for the O-receipt merge gate (D8). The pure `checkReceipts` rejects
// any owed (or malformed) O row, rejects an inherited/receipt-present row whose digest is an
// unresolved placeholder, and accepts an all-inherited/receipt-present set with REAL digests. It
// also asserts the REAL registry is red at C1 for BOTH the seeded owed row AND the seeded inherited
// row's placeholder `sha256:PENDING-CANDIDATE-DIGEST` — both are maintainer-owed before merge. A
// cross-check confirms the ORDINARY completeness checker still treats that placeholder row as
// well-formed (the divergence is deliberate: the worker gate stays green, the merge gate goes red).

import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { checkReceipts } from "./receipts.js";
import { checkCompleteness } from "./completeness.js";
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

/** A real, resolved merge-candidate digest (sha256: + 64 lowercase hex). */
const REAL_DIGEST = `sha256:${"deadbeef".repeat(8)}`;
/** The exact placeholder the C1 registry seeds into its inherited O row. */
const PLACEHOLDER_DIGEST = "sha256:PENDING-CANDIDATE-DIGEST";

const INHERITED: OState = {
  kind: "inherited",
  source: "src",
  imageDigest: REAL_DIGEST,
  target: "image",
  unchangedJustification: "unchanged",
};
const INHERITED_PLACEHOLDER: OState = { ...INHERITED, imageDigest: PLACEHOLDER_DIGEST };
const RECEIPT: OState = { kind: "receipt-present", source: "src", imageDigest: REAL_DIGEST, target: "image" };
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

  it("accepts an all-inherited / receipt-present set with REAL digests", () => {
    const report = checkReceipts([orow("a", INHERITED), orow("b", RECEIPT)]);
    assert.deepEqual(report.owed, []);
    assert.deepEqual(report.unresolved, []);
    assert.deepEqual(report.malformed, []);
    assert.equal(report.ok, true);
  });

  it("(a) an inherited row with a real 64-hex sha256 digest is NOT flagged", () => {
    const report = checkReceipts([orow("real", INHERITED)]);
    assert.deepEqual(report.unresolved, []);
    assert.equal(report.ok, true);
  });

  it("(b) an inherited row with a placeholder digest is flagged as unresolved (not owed)", () => {
    const report = checkReceipts([orow("pend", INHERITED_PLACEHOLDER)]);
    assert.equal(report.ok, false);
    assert.deepEqual(report.owed, [], "a placeholder digest is a distinct category from owed");
    assert.equal(report.unresolved.length, 1);
    assert.equal(report.unresolved[0]?.id, "pend");
    assert.equal(report.unresolved[0]?.digest, PLACEHOLDER_DIGEST);
  });

  it("flags a receipt-present row whose digest is not sha256:<64-hex>", () => {
    const badReceipt: OState = { ...RECEIPT, imageDigest: "sha256:cafe" };
    const report = checkReceipts([orow("shortdigest", badReceipt)]);
    assert.equal(report.ok, false);
    assert.equal(report.unresolved[0]?.id, "shortdigest");
    assert.equal(report.unresolved[0]?.digest, "sha256:cafe");
  });

  it("(c) the ORDINARY completeness checker still treats the placeholder row as well-formed", () => {
    // Same placeholder inherited row the receipts gate flags as unresolved — completeness must
    // NOT fail on it (a well-formed inherited/owed record is permitted by the worker's gate).
    const placeholderRow = orow("codex-o-placeholder", INHERITED_PLACEHOLDER);
    const receipts = checkReceipts([placeholderRow]);
    assert.equal(receipts.ok, false, "the merge gate rejects the placeholder digest");

    const completeness = checkCompleteness([placeholderRow], {}, []);
    assert.equal(completeness.ok, true, "the ordinary completeness gate permits the same well-formed row");
    assert.deepEqual(completeness.failures, []);
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

  it("is red on the REAL registry for BOTH the owed row AND the placeholder inherited digest", () => {
    const report = checkReceipts(ALL_CLAUSES);
    assert.equal(report.ok, false, "the C1 registry seeds an owed O row + a placeholder digest, so the merge gate must be red");
    assert.ok(
      report.owed.some((row) => row.id === "codex-o-descendant-code-mode-host-absence"),
      "the descendant code-mode-host absence O row is owed to the maintainer",
    );
    // The seeded inherited row cites sha256:PENDING-CANDIDATE-DIGEST — a placeholder, not a real
    // merge-candidate digest — so the merge gate now also blocks on it as unresolved.
    assert.ok(
      report.unresolved.some(
        (row) => row.id === "codex-o-command-root-home-denial" && row.digest === "sha256:PENDING-CANDIDATE-DIGEST",
      ),
      "the inherited command-root HOME-denial row's placeholder digest is flagged unresolved",
    );
  });
});
