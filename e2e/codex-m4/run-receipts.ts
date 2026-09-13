// PRD #1287 — the runnable redesigned D8 O-receipt merge gate, invoked by `task check:codex-m4-receipts`.
// It loads the real ALL_CLAUSES, resolves the candidate commit, loads the trusted (out-of-band,
// gitignored) candidate manifest, computes the `provenBaseCommit..candidate` tail, and runs the pure
// checkReceipts. It exits non-zero if the manifest/ancestry is unusable, the tail is not
// evidence-only, or any committed O clause is uncovered / any record is extra/duplicate/undischarged/
// mismatched / disposition-mismatched / any committed O row is malformed. This is the LEAD's merge
// gate — deliberately NOT wired into gate:agent / test:codex-m4, so none of these block the worker's
// ordinary gate (the worker cannot run packaged O proofs, know the merge-candidate digest, or supply
// the manifest).
//
// This file is a THIN entrypoint: the impure git-I/O helpers (resolveCandidateCommit, loadManifest,
// computeTail) live in receipts-io.ts (which has no top-level side effects, so a test can import
// computeTail without triggering this gate). main() wires them into the pure checkReceipts and prints
// diagnostics. See receipts-io.ts for the CODEX_M4_RECEIPT_MANIFEST / CODEX_M4_CANDIDATE_COMMIT inputs.

import { checkReceipts } from "./receipts.js";
import { computeTail, loadManifest, resolveCandidateCommit } from "./receipts-io.js";
import { ALL_CLAUSES } from "./registry.js";

function main(): void {
  const candidate = resolveCandidateCommit();
  const { manifest, malformed } = loadManifest();
  const tail = manifest ? computeTail(manifest.provenBaseCommit, candidate) : null;

  const report = checkReceipts(ALL_CLAUSES, manifest, tail);

  // A present-but-malformed manifest already printed its specific "candidate manifest is malformed:"
  // reason in loadManifest; checkReceipts(null) then reports manifestError "absent", so printing the
  // "manifest unusable [absent]" line too would misleadingly tell an operator the env var is unset.
  // Suppress it when malformed (the real reason was already printed); the exit code is unaffected.
  if (report.manifestError !== undefined && !malformed) {
    console.error(`run-receipts: manifest unusable [${report.manifestError.code}]: ${report.manifestError.message}`);
  }
  if (report.malformedClauses.length > 0) {
    console.error(`run-receipts: ${report.malformedClauses.length} committed O row(s) with a missing/malformed o-state:`);
    for (const id of report.malformedClauses) console.error(`  - ${id}`);
  }
  if (report.drift.length > 0) {
    console.error(
      `run-receipts: ${report.drift.length} runtime-affecting/unclassified path(s) in the `
      + "provenBase..candidate tail block merge (D8) — the tail must be evidence-only:",
    );
    for (const d of report.drift) console.error(`  - ${d.path} [${d.kind}]`);
  }
  if (report.missing.length > 0) {
    console.error(`run-receipts: ${report.missing.length} committed O clause(s) with no manifest record (undischarged):`);
    for (const id of report.missing) console.error(`  - ${id}`);
  }
  if (report.extra.length > 0) {
    console.error(`run-receipts: ${report.extra.length} manifest record(s) that are not a known O clause id:`);
    for (const id of report.extra) console.error(`  - ${id}`);
  }
  if (report.duplicate.length > 0) {
    console.error(`run-receipts: ${report.duplicate.length} manifest record id(s) appearing more than once:`);
    for (const id of report.duplicate) console.error(`  - ${id}`);
  }
  if (report.undischarged.length > 0) {
    console.error(`run-receipts: ${report.undischarged.length} manifest record(s) with an invalid disposition/image:`);
    for (const id of report.undischarged) console.error(`  - ${id}`);
  }
  if (report.mismatch.length > 0) {
    console.error(
      `run-receipts: ${report.mismatch.length} manifest record digest(s) that do NOT match the proven `
      + "candidate images block merge (D8):",
    );
    for (const row of report.mismatch) {
      console.error(`  - ${row.id} [${row.image}]${row.swapped ? " (swapped)" : ""}`);
      console.error(`      recorded: ${row.recorded}`);
      console.error(`      expected: ${row.expected}`);
    }
  }
  if (report.dispositionMismatch.length > 0) {
    console.error(
      `run-receipts: ${report.dispositionMismatch.length} manifest record(s) whose disposition does not `
      + "match the committed requirement (an owed clause needs a fresh receipt-present proof):",
    );
    for (const row of report.dispositionMismatch) {
      console.error(`  - ${row.id} committed=${row.committed} recorded=${row.recorded}`);
    }
  }

  if (report.ok && !malformed) {
    console.log(
      `run-receipts: OK — proven base ${manifest?.provenBaseCommit ?? "(none)"} → candidate ${candidate}; `
      + "evidence-only tail; every O clause bound to the trusted manifest (none drifting, missing, "
      + "extra, duplicate, undischarged, mismatched, or disposition-mismatched).",
    );
    return;
  }
  process.exit(1);
}

main();
