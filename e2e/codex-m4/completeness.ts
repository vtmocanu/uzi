// PRD #1287 C1 — the strict completeness gate core (D3).
//
// `checkCompleteness` is a PURE function over (clauses, executed-test evidence, required
// matrix). It fails on: duplicate id; unknown id (a prerequisite referencing an undefined
// clause); a missing required (adapter,layer) row; an adapter omitted from a required layer;
// a required U/P row that lists no test title (zero-test match); a required U/P row EVERY
// listed title of which must be `pass` — a title with no executed evidence (missing) OR whose
// evidence is skip|cancel|todo|fail each reject individually, so one listed title passing no
// longer masks a sibling that did not run; an unmet prerequisite of a required row; and any O
// row whose `o` field is missing or malformed. It PERMITS a well-formed `owed` O row (owed is
// a valid record for the ORDINARY gate — the separate receipts gate rejects it) and accepts
// well-formed `inherited`/`receipt-present` O rows.
//
// A PREREQUISITE (below) still needs only ≥1 passing test — it is not tightened to require every
// listed title, so `hasPassingTest` remains its oracle.
//
// The required matrix is a PARAMETER so C5 can widen it (add rows/cells) without rewriting
// this checker. At C1 the matrix is minimal (only the real, executed codex P smoke); the
// placeholder Claude U seeds and O seeds are catalogued but not execution-enforced.
//
// A hard-coded list of claimed successes, a fixture-import test, or a grep for test names is
// NOT execution evidence — this checker consumes the ACTUAL per-test results the run emitted.

import type { Adapter, ClauseRow, Layer } from "./clause.js";
import { isOState } from "./clause.js";
import type { EvidenceMap } from "./evidence.js";

/** One required (adapter, layer) coverage cell the milestone demands. */
export interface RequiredCell {
  readonly adapter: Adapter;
  readonly layer: Layer;
}

export interface CompletenessReport {
  readonly ok: boolean;
  /** Human-readable, most-specific-first failure lines; empty when `ok`. */
  readonly failures: string[];
}

function cellKey(adapter: Adapter, layer: Layer): string {
  return `${adapter}/${layer}`;
}

/** True when clause `c` is covered by ≥1 executed test that PASSED. */
function hasPassingTest(c: ClauseRow, evidence: EvidenceMap): boolean {
  return c.tests.some((title) => evidence[title] === "pass");
}

/**
 * Check the registry against the executed evidence and the milestone's required matrix.
 * Pure and side-effect-free; the runnable entry (run-completeness.ts) supplies real inputs.
 */
export function checkCompleteness(
  clauses: readonly ClauseRow[],
  evidence: EvidenceMap,
  requiredMatrix: readonly RequiredCell[],
): CompletenessReport {
  const failures: string[] = [];

  // 1. Duplicate ids (registry integrity).
  const byId = new Map<string, ClauseRow>();
  for (const c of clauses) {
    if (byId.has(c.id)) {
      failures.push(`duplicate clause id "${c.id}"`);
      continue;
    }
    byId.set(c.id, c);
  }

  // 2. Structural checks over every row: O evidence well-formedness + prerequisite ids.
  for (const c of clauses) {
    if (c.layer === "O" && !isOState(c.o)) {
      failures.push(`clause "${c.id}" is layer O but its o-state is missing or malformed`);
    }
    for (const prereq of c.prerequisites ?? []) {
      if (!byId.has(prereq)) {
        failures.push(`clause "${c.id}" references unknown prerequisite id "${prereq}"`);
      }
    }
  }

  // 3. Required matrix: every required (adapter,layer) cell must be present.
  const requiredCells = new Set<string>();
  for (const cell of requiredMatrix) {
    requiredCells.add(cellKey(cell.adapter, cell.layer));
    const layerRows = clauses.filter((c) => c.layer === cell.layer);
    if (layerRows.length === 0) {
      failures.push(`missing required (adapter,layer) row: ${cellKey(cell.adapter, cell.layer)}`);
      continue;
    }
    const adapterRows = layerRows.filter((c) => c.adapter === cell.adapter);
    if (adapterRows.length === 0) {
      failures.push(`adapter "${cell.adapter}" omitted from required layer "${cell.layer}"`);
    }
  }

  // 4. Execution + prerequisite enforcement for REQUIRED rows only. A U/P row under a
  //    required cell must list ≥1 title, and EVERY listed title must have `pass` evidence
  //    (a missing/skip/cancel/todo/fail title each rejects); an O row under a required cell
  //    is accepted when well-formed (owed included).
  for (const c of clauses) {
    if (!requiredCells.has(cellKey(c.adapter, c.layer))) continue;

    if (c.layer === "U" || c.layer === "P") {
      if (c.tests.length === 0) {
        failures.push(`zero-test match: required ${cellKey(c.adapter, c.layer)} clause "${c.id}" lists no test title`);
      }
      for (const title of c.tests) {
        const result = evidence[title];
        if (result === undefined) {
          failures.push(`required ${cellKey(c.adapter, c.layer)} clause "${c.id}" test "${title}" has NO executed evidence (missing)`);
        } else if (result !== "pass") {
          failures.push(`required ${cellKey(c.adapter, c.layer)} clause "${c.id}" test "${title}" is "${result}"`);
        }
      }
    }

    // Prerequisites of a required row must be satisfied: a U/P prereq needs ≥1 passing test,
    // an O prereq must be inherited/receipt-present (an owed prereq is unmet).
    for (const prereq of c.prerequisites ?? []) {
      const dep = byId.get(prereq);
      if (dep === undefined) continue; // already reported as unknown id in step 2
      const met =
        dep.layer === "O"
          ? isOState(dep.o) && dep.o.kind !== "owed"
          : hasPassingTest(dep, evidence);
      if (!met) {
        failures.push(`clause "${c.id}" has an unmet prerequisite "${prereq}"`);
      }
    }
  }

  return { ok: failures.length === 0, failures };
}
