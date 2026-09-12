// PRD #1287 C1 — the readable clause→layer map (D3), generated FROM the registry.
//
// D3 requires a readable clause-to-layer map that is generated from the registry OR asserted
// consistent with it. We do the former: renderClauseMap(ALL_CLAUSES) produces the markdown
// table on demand, and map.test.ts asserts it covers every row — so there is no separately
// committed markdown that could silently drift from the registry at C1.

import type { ClauseRow } from "./clause.js";

/** Escape a cell value for a GitHub-flavored markdown table (pipes + newlines). */
function cell(value: string): string {
  return value.replace(/\|/g, "\\|").replace(/\r?\n/g, " ");
}

/** The O-evidence state, compacted for the table's last column. */
function oSummary(c: ClauseRow): string {
  if (c.layer !== "O") return "";
  if (c.o === undefined) return "MALFORMED (missing)";
  switch (c.o.kind) {
    case "inherited":
      return `inherited ← ${c.o.source}`;
    case "receipt-present":
      return `receipt ← ${c.o.source}`;
    case "owed":
      return `OWED → ${c.o.owner}`;
  }
}

/** Render the whole registry as a readable clause→layer markdown table. Every clause row
 *  appears exactly once, in registry order, with its adapter, layer, family, seam, intended
 *  outcome, the executed tests that cover it, and (for O rows) its three-state summary. */
export function renderClauseMap(clauses: readonly ClauseRow[]): string {
  const header = [
    "# Codex/Claude M4 conformance clause map",
    "",
    "Generated from `e2e/codex-m4/registry.ts` (`ALL_CLAUSES`). Do not edit by hand — edit the",
    "per-adapter contribution files and regenerate. `map.test.ts` asserts this covers every row.",
    "",
    "| id | adapter | layer | family | seam | intended outcome | tests | O-evidence |",
    "| --- | --- | --- | --- | --- | --- | --- | --- |",
  ];
  const rows = clauses.map((c) => {
    const tests = c.tests.length === 0 ? "—" : c.tests.map(cell).join("<br>");
    return `| ${cell(c.id)} | ${c.adapter} | ${c.layer} | ${cell(c.family)} | ${cell(c.seam)} | `
      + `${cell(c.intendedOutcome)} | ${tests} | ${cell(oSummary(c))} |`;
  });
  const summary = [
    "",
    `Total clauses: ${clauses.length} `
    + `(${clauses.filter((c) => c.adapter === "claude").length} claude, `
    + `${clauses.filter((c) => c.adapter === "codex").length} codex; `
    + `U=${clauses.filter((c) => c.layer === "U").length}, `
    + `P=${clauses.filter((c) => c.layer === "P").length}, `
    + `O=${clauses.filter((c) => c.layer === "O").length}).`,
    "",
  ];
  return [...header, ...rows, ...summary].join("\n");
}
