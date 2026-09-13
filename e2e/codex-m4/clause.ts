// PRD #1287 C1 — the machine-readable clause-row TYPE and the committed O-evidence two-state model.
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
 *  packaged-OS evidence that is `inherited` or explicitly `owed` in the committed registry (D8) —
 *  the candidate receipts (and their image digests) live in the gitignored manifest receipts.ts
 *  binds, never in this registry. */
export type Layer = "U" | "P" | "O";

/** The pair of distinct proven merge-candidate image digests the candidate MANIFEST binds (D8): a
 *  merge candidate ships TWO images — `base` and `jvm` — and one proven image cannot vouch for the
 *  other. This type is imported by receipts.ts for the manifest's proven-image pair and each
 *  per-clause record's digests; it is NO LONGER embedded in the committed clause rows (candidate
 *  digests live only in the gitignored manifest). Both must independently resolve to a real
 *  `sha256:<64-hex>` before the receipts gate passes. */
export interface ImageDigests {
  readonly base: string;
  readonly jvm: string;
}

/**
 * The committed O-evidence TWO-state model (D8). The registry describes STABLE requirements, not
 * candidate-specific evidence — an O row can never be "executed" in this worker (no
 * Landlock/uid-split/descendant-reaping proof runs here), so its `o` field is one of:
 *   - `inherited` — a previously tested source proves the SAME unchanged mechanism (cites the
 *                   source, target, and WHY the enforcement path is unchanged at any candidate);
 *   - `owed`      — no valid proof exists yet; the maintainer owns running it. `owed` is a VALID
 *                   record for the ordinary completeness gate (it is not executed evidence) but the
 *                   SEPARATE receipts gate (receipts.ts) requires a discharging manifest record.
 *
 * The `receipt-present` disposition and the actual candidate image digests are NO LONGER part of
 * this committed model: they are MANIFEST-only, candidate-specific evidence (a gitignored
 * receipt-manifest.json checked by receipts.ts). Committing a digest into a row would change HEAD
 * and — because the worker Dockerfiles `COPY . /opt/uzi-src` and stamp UZI_SRC_SHA — the image
 * digest itself, so there is no fixed point; candidate digests are therefore kept OUT of the
 * committed registry.
 */
export type OState =
  | {
      readonly kind: "inherited";
      readonly source: string;
      readonly target: string;
      readonly unchangedJustification: string;
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
  /** REQUIRED for `layer === "O"`, ignored otherwise. The committed two-state O requirement record. */
  readonly o?: OState;
}

/** True when `value` is a well-formed {@link OState}. Used by the completeness checker to reject an
 *  O row whose `o` field is missing or malformed (D8), and by the receipts gate. Validates ONLY the
 *  two committed variants' required string fields — `inherited` needs non-empty string
 *  source/target/unchangedJustification; `owed` needs non-empty string target/reason/owner — and
 *  IGNORES any extra properties, so a row that still embeds `imageDigests` is NOT rejected here (the
 *  switch simply does not look at it). The "committed rows carry no candidate digests" invariant is
 *  enforced SEPARATELY by receipts.test.ts, not by isOState. The retired `receipt-present` variant,
 *  however, IS rejected — an unrecognized `kind` falls to the default arm and returns false. */
export function isOState(value: unknown): value is OState {
  if (value === null || typeof value !== "object") return false;
  const o = value as Record<string, unknown>;
  const str = (v: unknown): v is string => typeof v === "string" && v.length > 0;
  switch (o.kind) {
    case "inherited":
      return str(o.source) && str(o.target) && str(o.unchangedJustification);
    case "owed":
      return str(o.target) && str(o.reason) && str(o.owner);
    default:
      return false;
  }
}
