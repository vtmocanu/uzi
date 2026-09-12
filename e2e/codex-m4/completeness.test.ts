// PRD #1287 C1 — SYNTHETIC-reporter self-tests for the completeness checker (D3). Each failure
// mode is fed a hand-built clause set + evidence and asserted to fire; a well-formed `owed` O row
// plus a fully-covered required U/P set is asserted to PASS. These self-tests depend on NEITHER
// the real registry NOR the real binary — they validate the checker in isolation.

import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { checkCompleteness, type RequiredCell } from "./completeness.js";
import type { ClauseRow, OState } from "./clause.js";
import type { EvidenceMap } from "./evidence.js";

/** A minimal well-formed U/P clause. */
function uprow(overrides: Partial<ClauseRow> & Pick<ClauseRow, "id" | "adapter" | "layer">): ClauseRow {
  return {
    family: "F",
    seam: "seam",
    positiveControl: "pc",
    negativeOracle: "no",
    intendedOutcome: "io",
    tests: ["t"],
    ...overrides,
  };
}

const INHERITED_O: OState = {
  kind: "inherited",
  source: "src",
  imageDigest: "sha256:deadbeef",
  target: "image",
  unchangedJustification: "unchanged",
};

const OWED_O: OState = { kind: "owed", target: "image", reason: "needs packaged proof", owner: "maintainer" };

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

const REQ_CODEX_P: readonly RequiredCell[] = [{ adapter: "codex", layer: "P" }];

function firstFailure(report: { ok: boolean; failures: string[] }): string {
  assert.equal(report.ok, false, `expected a failure, got ok with [${report.failures.join("; ")}]`);
  return report.failures.join(" | ");
}

describe("checkCompleteness failure modes", () => {
  it("fires on a duplicate id", () => {
    const clauses = [
      uprow({ id: "dup", adapter: "codex", layer: "P" }),
      uprow({ id: "dup", adapter: "codex", layer: "P" }),
    ];
    const evidence: EvidenceMap = { t: "pass" };
    assert.match(firstFailure(checkCompleteness(clauses, evidence, REQ_CODEX_P)), /duplicate clause id "dup"/);
  });

  it("fires on an unknown prerequisite id", () => {
    const clauses = [uprow({ id: "a", adapter: "codex", layer: "P", prerequisites: ["ghost"] })];
    const evidence: EvidenceMap = { t: "pass" };
    assert.match(firstFailure(checkCompleteness(clauses, evidence, REQ_CODEX_P)), /unknown prerequisite id "ghost"/);
  });

  it("fires on a missing required (adapter,layer) row", () => {
    const clauses = [uprow({ id: "u1", adapter: "codex", layer: "U" })];
    const evidence: EvidenceMap = { t: "pass" };
    // Require codex/P but only a U row exists.
    assert.match(firstFailure(checkCompleteness(clauses, evidence, REQ_CODEX_P)), /missing required \(adapter,layer\) row: codex\/P/);
  });

  it("fires when a required layer exists but the required adapter is omitted", () => {
    const clauses = [uprow({ id: "p-codex", adapter: "codex", layer: "P" })];
    const evidence: EvidenceMap = { t: "pass" };
    const req: readonly RequiredCell[] = [{ adapter: "claude", layer: "P" }];
    assert.match(firstFailure(checkCompleteness(clauses, evidence, req)), /adapter "claude" omitted from required layer "P"/);
  });

  it("fires on a zero-test match for a required U/P row", () => {
    const clauses = [uprow({ id: "p1", adapter: "codex", layer: "P", tests: ["never-run"] })];
    const evidence: EvidenceMap = {}; // nothing executed
    assert.match(firstFailure(checkCompleteness(clauses, evidence, REQ_CODEX_P)), /zero-test match: required codex\/P clause "p1"/);
  });

  it("fires on a required U/P case whose evidence is skip|cancel|todo|fail", () => {
    const clauses = [uprow({ id: "p1", adapter: "codex", layer: "P", tests: ["t"] })];
    for (const bad of ["skip", "cancel", "todo", "fail"] as const) {
      const evidence: EvidenceMap = { t: bad };
      assert.match(firstFailure(checkCompleteness(clauses, evidence, REQ_CODEX_P)), new RegExp(`test "t" is "${bad}"`));
    }
  });

  it("fires on an unmet prerequisite of a required row", () => {
    const clauses = [
      uprow({ id: "dep", adapter: "codex", layer: "U", tests: ["dep-t"] }),
      uprow({ id: "p1", adapter: "codex", layer: "P", tests: ["t"], prerequisites: ["dep"] }),
    ];
    const evidence: EvidenceMap = { t: "pass" }; // dep-t never ran → prerequisite unmet
    assert.match(firstFailure(checkCompleteness(clauses, evidence, REQ_CODEX_P)), /unmet prerequisite "dep"/);
  });

  it("fires on an O row with a missing or malformed o-state", () => {
    const clausesMissing = [
      uprow({ id: "p1", adapter: "codex", layer: "P" }),
      orow("o-missing", undefined),
    ];
    const evidence: EvidenceMap = { t: "pass" };
    assert.match(firstFailure(checkCompleteness(clausesMissing, evidence, REQ_CODEX_P)), /layer O but its o-state is missing or malformed/);

    const clausesMalformed = [
      uprow({ id: "p1", adapter: "codex", layer: "P" }),
      { ...orow("o-bad", undefined), o: { kind: "inherited", source: "" } as unknown as OState },
    ];
    assert.match(firstFailure(checkCompleteness(clausesMalformed, evidence, REQ_CODEX_P)), /o-state is missing or malformed/);
  });
});

describe("checkCompleteness pass case", () => {
  it("passes a fully-covered required U/P set with a well-formed owed O row", () => {
    const clauses = [
      uprow({ id: "p1", adapter: "codex", layer: "P", tests: ["t"] }),
      orow("o-owed", OWED_O), // owed is a valid record for the ORDINARY gate
      orow("o-inherited", INHERITED_O),
    ];
    const evidence: EvidenceMap = { t: "pass" };
    const report = checkCompleteness(clauses, evidence, REQ_CODEX_P);
    assert.deepEqual(report.failures, []);
    assert.equal(report.ok, true);
  });

  it("passes when a required row's prerequisite has a passing test", () => {
    const clauses = [
      uprow({ id: "dep", adapter: "codex", layer: "P", tests: ["dep-t"] }),
      uprow({ id: "p1", adapter: "codex", layer: "P", tests: ["t"], prerequisites: ["dep"] }),
    ];
    const evidence: EvidenceMap = { t: "pass", "dep-t": "pass" };
    assert.equal(checkCompleteness(clauses, evidence, REQ_CODEX_P).ok, true);
  });
});
