// PRD #1287 — the runnable redesigned D8 O-receipt merge gate, invoked by `task check:codex-m4-receipts`.
// It loads the real ALL_CLAUSES, resolves the candidate commit, loads the trusted (out-of-band,
// gitignored) candidate manifest, computes the `provenBaseCommit..candidate` tail, and runs the pure
// checkReceipts. It exits non-zero if the manifest/ancestry is unusable, the tail is not
// evidence-only, or any committed O clause is uncovered / any record is extra/duplicate/undischarged/
// mismatched / any committed O row is malformed. This is the LEAD's merge gate — deliberately NOT
// wired into gate:agent / test:codex-m4, so none of these block the worker's ordinary gate (the
// worker cannot run packaged O proofs, know the merge-candidate digest, or supply the manifest).
//
// Inputs (both out-of-band, supplied by the lead):
//   CODEX_M4_RECEIPT_MANIFEST  — path to the trusted candidate manifest JSON (the packaged proof's
//                                proven base/jvm digests + one discharging record per committed O
//                                clause, all bound to a provenBaseCommit). Absent/unreadable → fail
//                                closed as "manifest absent"; present-but-malformed → fail closed +
//                                exit 1. It is gitignored (receipt-manifest.json), so recording it
//                                makes NO commit and cannot invalidate the candidate it certifies.
//   CODEX_M4_CANDIDATE_COMMIT  — the candidate commit; when unset, derived from `git rev-parse HEAD`.

import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";

import { checkReceipts, parseManifest, type CandidateManifest, type TailInput } from "./receipts.js";
import { ALL_CLAUSES } from "./registry.js";

/** The candidate commit being certified: the explicit env override, else `git rev-parse HEAD`. */
function resolveCandidateCommit(): string {
  const env = process.env.CODEX_M4_CANDIDATE_COMMIT;
  if (env !== undefined && env.trim().length > 0) return env.trim();
  try {
    return execFileSync("git", ["rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  } catch (err) {
    console.error(
      "run-receipts: cannot determine the candidate commit — set CODEX_M4_CANDIDATE_COMMIT or run "
      + `inside a git checkout (${(err as Error).message})`,
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

/** Compute the `provenBase..candidate` tail via git. `git merge-base --is-ancestor` (exit 0 →
 *  ancestor, non-zero/throw → not) decides reachability; when it is an ancestor, `git diff
 *  --name-only provenBase..candidate` yields the changed paths. A total git failure returns null so
 *  checkReceipts fails closed (base-not-ancestor) rather than certifying on missing evidence. */
function computeTail(provenBase: string, candidate: string): TailInput | null {
  try {
    let baseIsAncestor: boolean;
    try {
      execFileSync("git", ["merge-base", "--is-ancestor", provenBase, candidate], { stdio: "ignore" });
      baseIsAncestor = true;
    } catch {
      baseIsAncestor = false; // exit 1 (not an ancestor) or a bad-object error → treat as not reachable
    }
    if (!baseIsAncestor) return { paths: [], baseIsAncestor: false };

    const out = execFileSync("git", ["diff", "--name-only", `${provenBase}..${candidate}`], { encoding: "utf8" });
    const paths = out.split("\n").map((line) => line.trim()).filter((line) => line.length > 0);
    return { paths, baseIsAncestor: true };
  } catch (err) {
    console.error(`run-receipts: cannot compute the provenBase..candidate tail (fail closed): ${(err as Error).message}`);
    return null;
  }
}

function main(): void {
  const candidate = resolveCandidateCommit();
  const { manifest, malformed } = loadManifest();
  const tail = manifest ? computeTail(manifest.provenBaseCommit, candidate) : null;

  const report = checkReceipts(ALL_CLAUSES, manifest, tail);

  if (report.manifestError !== undefined) {
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

  if (report.ok && !malformed) {
    console.log(
      `run-receipts: OK — proven base ${manifest?.provenBaseCommit ?? "(none)"} → candidate ${candidate}; `
      + "evidence-only tail; every O clause bound to the trusted manifest (none drifting, missing, "
      + "extra, duplicate, undischarged, or mismatched).",
    );
    return;
  }
  process.exit(1);
}

main();
