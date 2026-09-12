// PRD #1287 C1 — the runnable O-receipt merge gate (D8), invoked by `task check:codex-m4-receipts`.
// It loads the real ALL_CLAUSES, prints any owed (or malformed) O rows, and exits non-zero if
// any O assertion is still owed. This is the LEAD's merge gate — deliberately NOT wired into
// gate:agent / test:codex-m4, so an owed row never reddens the worker's ordinary gate (the
// worker cannot run packaged O proofs), only the maintainer's pre-merge check.

import { checkReceipts } from "./receipts.js";
import { ALL_CLAUSES } from "./registry.js";

function main(): void {
  const report = checkReceipts(ALL_CLAUSES);
  if (report.ok) {
    console.log("run-receipts: OK — every O-layer clause has an inherited/receipt-present record; none owed.");
    return;
  }
  if (report.malformed.length > 0) {
    console.error(`run-receipts: ${report.malformed.length} O row(s) with a missing/malformed o-state:`);
    for (const id of report.malformed) console.error(`  - ${id}`);
  }
  if (report.unresolved.length > 0) {
    console.error(
      `run-receipts: ${report.unresolved.length} inherited/receipt-present O row(s) with an `
      + "unresolved (placeholder) digest block merge (D8) — refresh to the real merge-candidate digest:",
    );
    for (const row of report.unresolved) {
      console.error(`  - ${row.id}`);
      console.error(`      digest: ${row.digest}`);
    }
  }
  if (report.owed.length > 0) {
    console.error(`run-receipts: ${report.owed.length} owed O assertion(s) block merge (D8):`);
    for (const owed of report.owed) {
      console.error(`  - ${owed.id}`);
      console.error(`      target: ${owed.target}`);
      console.error(`      reason: ${owed.reason}`);
      console.error(`      owner:  ${owed.owner}`);
    }
  }
  process.exit(1);
}

main();
