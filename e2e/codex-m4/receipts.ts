// PRD #1287 C1 — the O-receipt merge gate (D8), SEPARATE from the ordinary completeness gate.
//
// The ordinary gate (completeness.ts) permits a well-formed `owed` O row AND a well-formed
// `inherited`/`receipt-present` row whose digest is still a placeholder: both are valid RECORDS
// for the worker's gate, not test failures. This gate is stricter and is the LEAD's merge gate:
// it rejects if ANY O row is still `owed`, OR if an `inherited`/`receipt-present` row carries a
// digest that is not a real merge-candidate sha256 (an "unresolved" digest — a placeholder like
// `sha256:PENDING-CANDIDATE-DIGEST` or any non-`sha256:<64-hex>` value). This keeps the C1 seed
// honest: an inherited row that merely CLAIMS a receipt without a real digest cannot let parent
// M4 be marked complete before the maintainer refreshes it to the actual merge-candidate digest.
//
// Deliberately NOT folded into gate:agent / test:codex-m4 — neither an owed row nor an unresolved
// placeholder digest must redden the worker's ordinary gate (the worker cannot run packaged O
// proofs or know the merge-candidate digest), only the lead's merge gate.

import type { ClauseRow } from "./clause.js";
import { isOState } from "./clause.js";

/** A real, resolved merge-candidate image digest: `sha256:` + 64 lowercase hex. A placeholder or
 *  otherwise malformed value is "unresolved" and blocks the merge gate. */
const REAL_IMAGE_DIGEST = /^sha256:[0-9a-f]{64}$/;

/** One O row still owed to the maintainer. */
export interface OwedRow {
  readonly id: string;
  readonly target: string;
  readonly reason: string;
  readonly owner: string;
}

/** One inherited/receipt-present O row whose digest is not a real merge-candidate sha256. */
export interface UnresolvedRow {
  readonly id: string;
  readonly digest: string;
}

export interface ReceiptsReport {
  readonly ok: boolean;
  readonly owed: OwedRow[];
  /** inherited/receipt-present O rows whose `imageDigest` is a placeholder/non-real sha256 — a
   *  distinct blocking category from `owed`: the receipt cites no real merge-candidate image. */
  readonly unresolved: UnresolvedRow[];
  /** O rows whose `o` field is missing/malformed — also a rejection (a receipt cannot be read). */
  readonly malformed: string[];
}

/** Pure receipts check: ok=false if ANY O row is `owed`, carries an unresolved (placeholder)
 *  digest, or has a malformed o-state. */
export function checkReceipts(clauses: readonly ClauseRow[]): ReceiptsReport {
  const owed: OwedRow[] = [];
  const unresolved: UnresolvedRow[] = [];
  const malformed: string[] = [];
  for (const c of clauses) {
    if (c.layer !== "O") continue;
    if (!isOState(c.o)) {
      malformed.push(c.id);
      continue;
    }
    if (c.o.kind === "owed") {
      owed.push({ id: c.id, target: c.o.target, reason: c.o.reason, owner: c.o.owner });
      continue;
    }
    // inherited | receipt-present: the cited digest must be a real merge-candidate sha256, not a
    // placeholder. (isOState already rejected an empty/missing digest as malformed above.)
    if (!REAL_IMAGE_DIGEST.test(c.o.imageDigest)) {
      unresolved.push({ id: c.id, digest: c.o.imageDigest });
    }
  }
  return {
    ok: owed.length === 0 && unresolved.length === 0 && malformed.length === 0,
    owed,
    unresolved,
    malformed,
  };
}
