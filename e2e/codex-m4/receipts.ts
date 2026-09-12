// PRD #1287 C1 — the O-receipt merge gate (D8), SEPARATE from the ordinary completeness gate.
//
// The ordinary gate (completeness.ts) permits a well-formed `owed` O row: owed is a valid
// RECORD of maintainer-owed packaged evidence, not a test failure. This gate is stricter and
// is the LEAD's merge gate: it rejects if ANY O row is still `owed`, listing what is owed so
// the maintainer can run the packaged proof (or record its receipt) before parent M4 merge.
//
// Deliberately NOT folded into gate:agent / test:codex-m4 — an owed row must not redden the
// worker's ordinary gate (the worker cannot run packaged O proofs), only the lead's merge gate.

import type { ClauseRow } from "./clause.js";
import { isOState } from "./clause.js";

/** One O row still owed to the maintainer. */
export interface OwedRow {
  readonly id: string;
  readonly target: string;
  readonly reason: string;
  readonly owner: string;
}

export interface ReceiptsReport {
  readonly ok: boolean;
  readonly owed: OwedRow[];
  /** O rows whose `o` field is missing/malformed — also a rejection (a receipt cannot be read). */
  readonly malformed: string[];
}

/** Pure receipts check: ok=false if ANY O row is `owed` (or has a malformed o-state). */
export function checkReceipts(clauses: readonly ClauseRow[]): ReceiptsReport {
  const owed: OwedRow[] = [];
  const malformed: string[] = [];
  for (const c of clauses) {
    if (c.layer !== "O") continue;
    if (!isOState(c.o)) {
      malformed.push(c.id);
      continue;
    }
    if (c.o.kind === "owed") {
      owed.push({ id: c.id, target: c.o.target, reason: c.o.reason, owner: c.o.owner });
    }
  }
  return { ok: owed.length === 0 && malformed.length === 0, owed, malformed };
}
