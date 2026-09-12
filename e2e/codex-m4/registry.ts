// PRD #1287 C1 — the one machine-readable clause registry (D3).
//
// STATIC imports only: each adapter contributes a plain exported array, and this file
// concatenates them into ALL_CLAUSES. No glob/dynamic aggregation — a dynamic loader would
// make the contribution exports look unused to knip/deadcode and would let a renamed file
// silently drop its rows from the gate. A new adapter file is one more static import here.

import type { ClauseRow } from "./clause.js";
import { CLAUDE_CLAUSES } from "./clauses-claude.js";
import { CODEX_CLAUSES } from "./clauses-codex.js";

/** Every conformance clause, both adapters, all layers. The completeness checker, the
 *  receipts gate and the clause map all read THIS array. */
export const ALL_CLAUSES: readonly ClauseRow[] = [...CLAUDE_CLAUSES, ...CODEX_CLAUSES];
