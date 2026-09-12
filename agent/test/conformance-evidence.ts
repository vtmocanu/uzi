// PRD #1287 C5 — agent-scoped executed-test evidence recorder.
//
// This is the AGENT-package sibling of e2e/codex-m4/evidence.ts's `recordEvidence`, and MUST stay
// format-compatible with it (same `{title,result}\n` JSONL, same `CODEX_M4_EVIDENCE` env var, same
// no-op-when-unset, same never-throw). The C2 Claude preservation files and the C3 D6 HOME-screening
// regression live under agent/test and run in the ordinary `npm test`; C5 additionally runs them
// under `test:codex-m4` so the strict completeness gate can assemble ALL required U evidence in one
// place. Rather than reach across the tree into e2e/codex-m4/evidence.ts (a cross-tree import that
// would widen typecheck:agent / knip:agent), this file re-implements the SAME append contract here.

import { appendFileSync } from "node:fs";

/** The node --test result vocabulary the completeness checker distinguishes; mirrors
 *  e2e/codex-m4/evidence.ts:EvidenceResult. A required U/P case must be `pass`. */
type ConformanceResult = "pass" | "fail" | "skip" | "cancel" | "todo";

/** Append one executed-test result to `CODEX_M4_EVIDENCE`. No-op when the env var is unset (an
 *  ordinary standalone `npm test` run, where this evidence is not consumed), so existing test
 *  behavior is unchanged there. Never throws into the test body: an unwritable evidence file
 *  surfaces later as run-completeness's zero-test-match, not as a spurious test failure. The line
 *  format is byte-for-byte identical to e2e/codex-m4/evidence.ts so both recorders can append to
 *  the same file. */
export function recordConformanceEvidence(title: string, result: ConformanceResult): void {
  const file = process.env.CODEX_M4_EVIDENCE;
  if (file === undefined || file.trim().length === 0) return;
  try {
    appendFileSync(file, `${JSON.stringify({ title, result })}\n`, "utf8");
  } catch {
    /* recorder failure is not a test failure; the missing evidence fails completeness instead */
  }
}
