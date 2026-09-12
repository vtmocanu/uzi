// PRD #1287 C1 — the machine-readable clause-row TYPE and the O-evidence three-state model.
//
// A clause is one adversarial case with a STABLE unique id, an adapter, a layer (U/P/O per
// D7), the production seam it exercises, a positive control, a negative-effect oracle, the
// intended outcome, and the named test titles that exercise it. The completeness checker
// (completeness.ts) consumes these rows plus executed-test evidence; the registry
// (registry.ts) aggregates the per-adapter contribution files via STATIC imports.
//
// This file defines TYPES ONLY (plus small type guards). It intentionally imports nothing
// from agent/src: the contract shape is frozen at C1 so C2/C3/C4 append rows without
// touching the checker or this file.

/** The two adapters under conformance. Claude and Codex map Codex-only wire operations to
 *  the equivalent construction/hook invariant (D5); an adapter is never omitted from a
 *  required row (D3). */
export type Adapter = "claude" | "codex";

/** The three evidence layers (D7). U/P are executed here (unit + real protocol); O is
 *  packaged-OS evidence that is inherited, receipted, or explicitly owed (D8). */
export type Layer = "U" | "P" | "O";

/**
 * The O-evidence three-state model (D8). An O row can never be "executed" in this worker
 * (no Landlock/uid-split/descendant-reaping proof runs here), so its evidence is one of:
 *   - `inherited`      — a previously tested source/image proves the SAME unchanged mechanism
 *                        (cite the source, image digest, target, case, and WHY the enforcement
 *                        path is unchanged at this candidate);
 *   - `receipt-present`— a fresh packaged case has been run and recorded for this candidate;
 *   - `owed`           — no valid proof exists yet; the maintainer owns running it. `owed` is a
 *                        VALID record for the ordinary completeness gate (it is not executed
 *                        evidence) but the SEPARATE receipts gate (receipts.ts) rejects it.
 */
export type OState =
  | {
      readonly kind: "inherited";
      readonly source: string;
      readonly imageDigest: string;
      readonly target: string;
      readonly unchangedJustification: string;
    }
  | {
      readonly kind: "receipt-present";
      readonly source: string;
      readonly imageDigest: string;
      readonly target: string;
      readonly recordedAt?: string;
    }
  | {
      readonly kind: "owed";
      readonly target: string;
      readonly reason: string;
      readonly owner: string;
    };

/** One conformance clause row. Required rows (per the checker's required matrix) name their
 *  layer, production seam, positive control, negative-effect oracle and intended outcome
 *  (D3). A single named test may exercise several rows only when it emits separate evidence
 *  for each (a row lists exactly the test titles that cover it). */
export interface ClauseRow {
  /** Stable, unique across the whole registry. Never renumber a landed id. */
  readonly id: string;
  readonly adapter: Adapter;
  readonly layer: Layer;
  /** The adversarial family (matches the PRD "Required clause inventory" table). */
  readonly family: string;
  /** The production seam this clause exercises (e.g. `codex/broker.ts:handleToolCall`). */
  readonly seam: string;
  /** A reachable permitted action (or a bounded test-only unsafe control) proving the
   *  fixture can otherwise reach the operation (D4). */
  readonly positiveControl: string;
  /** How absence of the forbidden effect is independently asserted (a handler counter, a
   *  filesystem marker, request content, a spawn counter, …) — denial text alone is
   *  insufficient (D4). */
  readonly negativeOracle: string;
  /** The intended outcome the assertion proves. */
  readonly intendedOutcome: string;
  /** The named test titles that exercise this clause. The checker matches these against the
   *  executed-test evidence for a required U/P row. */
  readonly tests: readonly string[];
  /** Clause ids that must be satisfied first (their tests must have executed pass). Each must
   *  be a defined clause id, else the checker reports an unknown id. */
  readonly prerequisites?: readonly string[];
  /** REQUIRED for `layer === "O"`, ignored otherwise. The three-state O evidence record. */
  readonly o?: OState;
}

/** True when `value` is a well-formed {@link OState}. Used by the completeness checker to
 *  reject an O row whose `o` field is missing or malformed (D8), and by the receipts gate. */
export function isOState(value: unknown): value is OState {
  if (value === null || typeof value !== "object") return false;
  const o = value as Record<string, unknown>;
  const str = (v: unknown): v is string => typeof v === "string" && v.length > 0;
  switch (o.kind) {
    case "inherited":
      return str(o.source) && str(o.imageDigest) && str(o.target) && str(o.unchangedJustification);
    case "receipt-present":
      return str(o.source) && str(o.imageDigest) && str(o.target)
        && (o.recordedAt === undefined || str(o.recordedAt));
    case "owed":
      return str(o.target) && str(o.reason) && str(o.owner);
    default:
      return false;
  }
}
