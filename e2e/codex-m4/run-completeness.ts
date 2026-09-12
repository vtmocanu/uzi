// PRD #1287 C1 — the runnable completeness enforcer, invoked by `task test:codex-m4` AFTER the
// conformance tests have produced their executed-test evidence. It reads the evidence file
// (CODEX_M4_EVIDENCE) + the real ALL_CLAUSES and asserts checkCompleteness(...).ok for the C1
// required matrix. A non-zero exit fails the target.
//
// The C1 required matrix is MINIMAL: only the real, executed codex P smoke. C5 widens this
// matrix (the checker takes it as a parameter, so widening needs no checker change). The Claude
// U seeds + O seeds are catalogued in the registry but not execution-enforced at C1.

import { checkCompleteness, type RequiredCell } from "./completeness.js";
import { readEvidence } from "./evidence.js";
import { ALL_CLAUSES } from "./registry.js";

/** C1's minimal required matrix — the real executed P layer for the codex adapter. */
const REQUIRED_MATRIX_C1: readonly RequiredCell[] = [{ adapter: "codex", layer: "P" }];

function main(): void {
  const evidenceFile = process.env.CODEX_M4_EVIDENCE;
  if (evidenceFile === undefined || evidenceFile.trim().length === 0) {
    console.error("run-completeness: CODEX_M4_EVIDENCE is not set; cannot read executed-test evidence");
    process.exit(1);
  }
  const evidence = readEvidence(evidenceFile);
  const report = checkCompleteness(ALL_CLAUSES, evidence, REQUIRED_MATRIX_C1);
  if (!report.ok) {
    console.error(`run-completeness: FAILED (${report.failures.length} problem(s)):`);
    for (const failure of report.failures) console.error(`  - ${failure}`);
    process.exit(1);
  }
  const executed = Object.keys(evidence).length;
  console.log(
    `run-completeness: OK — ${ALL_CLAUSES.length} clauses catalogued, ${executed} executed test result(s), `
    + `required matrix [${REQUIRED_MATRIX_C1.map((c) => `${c.adapter}/${c.layer}`).join(", ")}] satisfied.`,
  );
}

main();
