// PRD #1287 C1 — the executed-test evidence recorder + reader.
//
// node --test emits TAP, which is awkward to reparse into a per-clause pass/fail map. Instead
// each M4 conformance test, on completion, appends ONE JSON line to the evidence file named by
// `CODEX_M4_EVIDENCE`; run-completeness.ts reads that file plus the real ALL_CLAUSES and asserts
// checkCompleteness(...).ok. A synthetic self-test (completeness.test.ts) never touches this
// file — it hand-builds its evidence — so the checker is validated independently of a real run.
//
// The recorder is a bounded append: one `{title,result}` line, fsync-free, best-effort-safe.
// If `CODEX_M4_EVIDENCE` is unset (a bare `node --test` of one file, or a self-test), it is a
// no-op, so the tests stay runnable standalone.

import { appendFileSync, readFileSync } from "node:fs";

/** The node --test result vocabulary the checker distinguishes: a required U/P case must be
 *  `pass`; `skip`/`cancel`/`todo`/`fail` are hard failures for a required case (D3). */
export type EvidenceResult = "pass" | "fail" | "skip" | "cancel" | "todo";

/** title → result, last write wins (a test that records twice keeps its final outcome). */
export type EvidenceMap = Readonly<Record<string, EvidenceResult>>;

/** Append one executed-test result to `CODEX_M4_EVIDENCE`. No-op when the env var is unset
 *  (standalone / self-test runs). Never throws into the test body: an unwritable evidence
 *  file surfaces later as run-completeness's zero-test-match, not as a spurious test failure. */
export function recordEvidence(title: string, result: EvidenceResult): void {
  const file = process.env.CODEX_M4_EVIDENCE;
  if (file === undefined || file.trim().length === 0) return;
  try {
    appendFileSync(file, `${JSON.stringify({ title, result })}\n`, "utf8");
  } catch {
    /* recorder failure is not a test failure; the missing evidence fails completeness instead */
  }
}

/** Parse an evidence JSONL file into an {@link EvidenceMap}. A missing file yields an empty
 *  map (run-completeness then reports the required rows as zero-test-match). A malformed line
 *  is skipped rather than aborting the whole read. */
export function readEvidence(file: string): EvidenceMap {
  let raw: string;
  try {
    raw = readFileSync(file, "utf8");
  } catch {
    return {};
  }
  const out: Record<string, EvidenceResult> = {};
  for (const line of raw.split("\n")) {
    const trimmed = line.trim();
    if (trimmed.length === 0) continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(trimmed);
    } catch {
      continue;
    }
    if (parsed === null || typeof parsed !== "object") continue;
    const record = parsed as Record<string, unknown>;
    const title = record.title;
    const result = record.result;
    if (
      typeof title === "string"
      && (result === "pass" || result === "fail" || result === "skip" || result === "cancel" || result === "todo")
    ) {
      out[title] = result;
    }
  }
  return out;
}
