// PRD #1287 C5 — the runnable completeness enforcer, invoked by `task test:codex-m4` AFTER the
// conformance tests have produced their executed-test evidence. It reads the evidence file
// (CODEX_M4_EVIDENCE) + the real ALL_CLAUSES and asserts checkCompleteness(...).ok for the C5
// required matrix. A non-zero exit fails the target.
//
// C5 tightens the required matrix from C1's minimal single codex/P cell to the FULL required U/P
// set — claude/U + codex/U + codex/P — now that test:codex-m4 assembles every layer's executed
// evidence in one place (the codex U/P cases plus the folded-in Claude preservation + D6 agent
// conformance files). The checker takes the matrix as a parameter, so widening needs no checker
// change. O rows are NOT in this matrix: they are receipt-gated by check:codex-m4-receipts (D8),
// never execution-enforced here (an `owed` O row must not deadlock the ordinary worker gate).

import { checkCompleteness, type RequiredCell } from "./completeness.js";
import { readEvidence } from "./evidence.js";
import { ALL_CLAUSES } from "./registry.js";

/** C5's tightened required matrix — the full required U/P set. claude/U (C2 preservation) and
 *  codex/U (C3/C4 policy/lifecycle/failure + the D6 HOME-screener) are now execution-enforced
 *  alongside the codex/P real-protocol cases. O rows stay OUT: they are records under the
 *  three-state model and gated separately by check:codex-m4-receipts (D3/D8). */
const REQUIRED_MATRIX_C5: readonly RequiredCell[] = [
  { adapter: "claude", layer: "U" },
  { adapter: "codex", layer: "U" },
  { adapter: "codex", layer: "P" },
];

function main(): void {
  const evidenceFile = process.env.CODEX_M4_EVIDENCE;
  if (evidenceFile === undefined || evidenceFile.trim().length === 0) {
    console.error("run-completeness: CODEX_M4_EVIDENCE is not set; cannot read executed-test evidence");
    process.exit(1);
  }
  const evidence = readEvidence(evidenceFile);
  const report = checkCompleteness(ALL_CLAUSES, evidence, REQUIRED_MATRIX_C5);
  if (!report.ok) {
    console.error(`run-completeness: FAILED (${report.failures.length} problem(s)):`);
    for (const failure of report.failures) console.error(`  - ${failure}`);
    process.exit(1);
  }
  const executed = Object.keys(evidence).length;
  console.log(
    `run-completeness: OK — ${ALL_CLAUSES.length} clauses catalogued, ${executed} executed test result(s), `
    + `required matrix [${REQUIRED_MATRIX_C5.map((c) => `${c.adapter}/${c.layer}`).join(", ")}] satisfied.`,
  );
}

main();
