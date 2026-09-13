// PRD #1287 C1 — the runnable O-receipt merge gate (D8), invoked by `task check:codex-m4-receipts`.
// It loads the real ALL_CLAUSES, BINDS each recorded base/jvm digest to a trusted candidate
// manifest for the expected merge-candidate commit, prints any owed / unresolved / mismatched (or
// malformed) O rows, and exits non-zero if any O assertion is owed, any digest fails to bind, or the
// manifest itself is unusable. This is the LEAD's merge gate — deliberately NOT wired into
// gate:agent / test:codex-m4, so none of these block the worker's ordinary gate (the worker cannot
// run packaged O proofs or know the merge-candidate digest), only the maintainer's pre-merge check.
//
// Inputs (both out-of-band, supplied by the lead):
//   CODEX_M4_RECEIPT_MANIFEST  — path to the trusted candidate manifest JSON (the packaged proof's
//                                base/jvm digests for the candidate). Absent/unreadable → fail closed
//                                as "manifest absent"; present-but-malformed → fail closed + exit 1.
//   CODEX_M4_CANDIDATE_COMMIT  — the expected merge-candidate commit; when unset, derived from
//                                `git rev-parse HEAD`.

import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";

import { checkReceipts, parseManifest, type CandidateManifest } from "./receipts.js";
import { ALL_CLAUSES } from "./registry.js";

function resolveExpectedCommit(): string {
  const env = process.env.CODEX_M4_CANDIDATE_COMMIT;
  if (env !== undefined && env.trim().length > 0) return env.trim();
  try {
    return execFileSync("git", ["rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  } catch (err) {
    console.error(
      "run-receipts: cannot determine expected commit — set CODEX_M4_CANDIDATE_COMMIT or run inside a "
      + `git checkout (${(err as Error).message})`,
    );
    process.exit(1);
  }
}

/** Load the trusted candidate manifest, failing closed. Returns the parsed manifest, or null when
 *  it is absent/unreadable/malformed; `malformed` is true only for a present-but-broken file (a
 *  distinct, always-exit-1 case from a simply-absent one). */
function loadManifest(): { manifest: CandidateManifest | null; malformed: boolean } {
  const envPath = process.env.CODEX_M4_RECEIPT_MANIFEST;
  if (envPath === undefined || envPath.trim().length === 0) {
    return { manifest: null, malformed: false }; // unset → absent (checkReceipts reports it)
  }
  const resolved = path.resolve(process.cwd(), envPath.trim());

  let raw: string;
  try {
    raw = readFileSync(resolved, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") {
      return { manifest: null, malformed: false }; // file does not exist → absent
    }
    console.error(`run-receipts: candidate manifest is malformed: cannot read ${resolved}: ${(err as Error).message}`);
    return { manifest: null, malformed: true };
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    console.error(`run-receipts: candidate manifest is malformed: ${(err as Error).message}`);
    return { manifest: null, malformed: true };
  }

  const result = parseManifest(parsed);
  if ("error" in result) {
    console.error(`run-receipts: candidate manifest is malformed: ${result.error}`);
    return { manifest: null, malformed: true };
  }
  return { manifest: result.manifest, malformed: false };
}

function main(): void {
  const expectedCommit = resolveExpectedCommit();
  const { manifest, malformed } = loadManifest();

  const report = checkReceipts(ALL_CLAUSES, manifest, expectedCommit);

  if (report.manifestError !== undefined) {
    console.error(`run-receipts: manifest unusable [${report.manifestError.code}]: ${report.manifestError.message}`);
  }
  if (report.malformed.length > 0) {
    console.error(`run-receipts: ${report.malformed.length} O row(s) with a missing/malformed o-state:`);
    for (const id of report.malformed) console.error(`  - ${id}`);
  }
  if (report.unresolved.length > 0) {
    console.error(
      `run-receipts: ${report.unresolved.length} inherited/receipt-present O row(s) with an `
      + "unresolved (placeholder) base/jvm digest block merge (D8) — refresh BOTH to the real merge-candidate digests:",
    );
    for (const row of report.unresolved) {
      console.error(`  - ${row.id} [${row.image}]`);
      console.error(`      digest: ${row.digest}`);
    }
  }
  if (report.mismatch.length > 0) {
    console.error(
      `run-receipts: ${report.mismatch.length} inherited/receipt-present O row(s) whose recorded base/jvm `
      + "digest does NOT match the trusted candidate manifest block merge (D8):",
    );
    for (const row of report.mismatch) {
      console.error(`  - ${row.id} [${row.image}]${row.swapped ? " (swapped)" : ""}`);
      console.error(`      recorded: ${row.recorded}`);
      console.error(`      expected: ${row.expected}`);
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

  if (report.ok && !malformed) {
    console.log(
      "run-receipts: OK — every O-layer clause is inherited/receipt-present with base/jvm digests "
      + `bound to the trusted candidate manifest for ${expectedCommit}; none owed, unresolved, or mismatched.`,
    );
    return;
  }
  process.exit(1);
}

main();
