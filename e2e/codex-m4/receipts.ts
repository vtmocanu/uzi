// PRD #1287 C1 — the O-receipt merge gate (D8), SEPARATE from the ordinary completeness gate.
//
// The ordinary gate (completeness.ts) permits a well-formed `owed` O row AND a well-formed
// `inherited`/`receipt-present` row whose digest is still a placeholder: both are valid RECORDS
// for the worker's gate, not test failures. This gate is stricter and is the LEAD's merge gate:
// it rejects if ANY O row is still `owed`, OR if an `inherited`/`receipt-present` row carries
// either merge-candidate digest (base or jvm) that is not a real merge-candidate sha256 (an
// "unresolved" digest — a placeholder like `sha256:PENDING-CANDIDATE-DIGEST` or any
// non-`sha256:<64-hex>` value).
//
// Syntax alone is NOT enough: a well-formed-but-fabricated sha256 would sail through a
// digest-shape check. So this gate additionally BINDS each recorded {base,jvm} digest to a
// trusted candidate MANIFEST for the EXPECTED merge-candidate commit — the packaged
// `UZI_CODEX_M3B_PACKAGED=1 task test:codex-m3b:packaged` proof, supplied out-of-band by the
// maintainer. It FAILS CLOSED when the manifest is absent, malformed, built for the wrong commit,
// missing an image, swapped (base recorded as jvm or vice versa), or simply mismatched. This keeps
// the C1 seed honest: an inherited row can neither CLAIM a receipt with a placeholder digest nor
// substitute an arbitrary real-looking digest for the actual merge-candidate one before the
// maintainer refreshes it against the trusted manifest.
//
// Deliberately NOT folded into gate:agent / test:codex-m4 — neither an owed row, an unresolved
// placeholder digest, nor an absent manifest must redden the worker's ordinary gate (the worker
// cannot run packaged O proofs or know the merge-candidate digest), only the lead's merge gate.

import type { ClauseRow } from "./clause.js";
import { isOState } from "./clause.js";

/** A real, resolved merge-candidate image digest: `sha256:` + 64 lowercase hex. A placeholder or
 *  otherwise malformed value is "unresolved" and blocks the merge gate. */
const REAL_IMAGE_DIGEST = /^sha256:[0-9a-f]{64}$/;

/** A full git commit hex: 40 (SHA-1) or 64 (SHA-256) lowercase hex chars. */
const FULL_GIT_HEX = /^[0-9a-f]{40}$|^[0-9a-f]{64}$/;

/** The two merge-candidate images every O proof binds (D8): a candidate ships `base` and `jvm`. */
const IMAGES = ["base", "jvm"] as const;

/** The trusted candidate manifest the lead supplies (out-of-band) for the expected merge
 *  candidate: the packaged proof's two image digests bound to the candidate commit. Every recorded
 *  O-row digest is checked against this — syntax alone (a real-looking sha256) is not enough. */
export interface CandidateManifest {
  /** Full git hex of the merge candidate the manifest proves: 40 or 64 lowercase hex. */
  readonly candidateCommit: string;
  /** The two proven image digests, each `sha256:` + 64 lowercase hex. */
  readonly images: { readonly base: string; readonly jvm: string };
  /** Non-empty description of HOW the proof was produced (e.g. the packaged-proof command). */
  readonly provenance: string;
  /** Optional ISO-ish timestamp of when the proof was recorded. */
  readonly recordedAt?: string;
}

/** Fail-closed STRUCTURAL validator for a raw (parsed-JSON) manifest. Returns `{ manifest }` with a
 *  normalized object on success, or `{ error }` naming the first structural violation. */
export function parseManifest(raw: unknown): { manifest: CandidateManifest } | { error: string } {
  if (raw === null || typeof raw !== "object") {
    return { error: "manifest is not a non-null object" };
  }
  const m = raw as Record<string, unknown>;

  if (typeof m.candidateCommit !== "string" || m.candidateCommit.length === 0) {
    return { error: "manifest.candidateCommit is missing or not a non-empty string" };
  }
  if (!FULL_GIT_HEX.test(m.candidateCommit)) {
    return { error: "manifest.candidateCommit is not a 40- or 64-char lowercase git hex" };
  }

  if (m.images === null || typeof m.images !== "object") {
    return { error: "manifest.images is missing or not an object" };
  }
  const images = m.images as Record<string, unknown>;
  for (const image of IMAGES) {
    const digest = images[image];
    if (typeof digest !== "string" || digest.length === 0) {
      return { error: `manifest.images.${image} is missing or empty` };
    }
    if (!REAL_IMAGE_DIGEST.test(digest)) {
      return { error: `manifest.images.${image} is not a real sha256:<64-hex>` };
    }
  }

  if (typeof m.provenance !== "string" || m.provenance.length === 0) {
    return { error: "manifest.provenance is missing or not a non-empty string" };
  }

  if (m.recordedAt !== undefined && typeof m.recordedAt !== "string") {
    return { error: "manifest.recordedAt, when present, must be a string" };
  }

  const manifest: CandidateManifest = {
    candidateCommit: m.candidateCommit,
    images: { base: images.base as string, jvm: images.jvm as string },
    provenance: m.provenance,
    ...(m.recordedAt === undefined ? {} : { recordedAt: m.recordedAt as string }),
  };
  return { manifest };
}

/** One O row still owed to the maintainer. */
export interface OwedRow {
  readonly id: string;
  readonly target: string;
  readonly reason: string;
  readonly owner: string;
}

/** One inherited/receipt-present O row with an unresolved (placeholder/non-real) image digest.
 *  `image` names WHICH merge-candidate image failed (base or jvm); a row can appear once per
 *  unresolved image, so a half-proven row (one real, one placeholder) still blocks the gate. */
export interface UnresolvedRow {
  readonly id: string;
  readonly image: "base" | "jvm";
  readonly digest: string;
}

/** One inherited/receipt-present O row whose recorded digest IS a real sha256 but does NOT match
 *  the trusted manifest's digest for that image — a fabricated or stale value. `swapped` is true
 *  when the recorded digest is instead the manifest's OTHER image (base recorded as jvm, or v.v.). */
export interface MismatchRow {
  readonly id: string;
  readonly image: "base" | "jvm";
  /** The digest recorded in the clause row. */
  readonly recorded: string;
  /** The trusted digest the manifest binds for this image. */
  readonly expected: string;
  /** True when `recorded` equals the manifest's OTHER image digest (a base/jvm swap). */
  readonly swapped: boolean;
}

export interface ReceiptsReport {
  readonly ok: boolean;
  readonly owed: OwedRow[];
  /** inherited/receipt-present O rows whose base/jvm `imageDigests` carry a placeholder/non-real
   *  sha256 — a distinct blocking category from `owed`: the receipt cites no real merge-candidate
   *  image. One row per unresolved image, so a half-proven row (one real, one placeholder) shows up
   *  once for the failing image. */
  readonly unresolved: UnresolvedRow[];
  /** inherited/receipt-present O rows whose recorded (real-sha256) digest does not match the trusted
   *  manifest for the expected commit — computed ONLY when the manifest is usable. */
  readonly mismatch: MismatchRow[];
  /** O rows whose `o` field is missing/malformed — also a rejection (a receipt cannot be read). */
  readonly malformed: string[];
  /** Set (fail closed) when the manifest itself is unusable, so no digest can be bound: `absent`
   *  (none supplied), `invalid-image` (a hand-built manifest with a non-real image digest), or
   *  `wrong-commit` (a manifest for some other candidate). When set, `mismatch` is NOT computed, but
   *  owed/unresolved/malformed still populate so the report stays a complete maintainer TODO list. */
  readonly manifestError?: {
    readonly code: "absent" | "invalid-image" | "wrong-commit";
    readonly message: string;
  };
}

/** Pure receipts check: ok=false if ANY O row is `owed`, carries an unresolved (placeholder)
 *  digest, has a malformed o-state, is bound to a mismatched trusted-manifest digest, or the
 *  manifest itself is unusable (absent/invalid/wrong-commit). Binds each recorded {base,jvm} digest
 *  to `manifest` for `expectedCommit`; fails closed when the manifest cannot be trusted. */
export function checkReceipts(
  clauses: readonly ClauseRow[],
  manifest: CandidateManifest | null,
  expectedCommit: string,
): ReceiptsReport {
  const owed: OwedRow[] = [];
  const unresolved: UnresolvedRow[] = [];
  const mismatch: MismatchRow[] = [];
  const malformed: string[] = [];

  // 1. Determine manifest usability, fail closed, in order.
  let manifestError: ReceiptsReport["manifestError"];
  if (manifest === null) {
    manifestError = {
      code: "absent",
      message: "candidate manifest absent — supply CODEX_M4_RECEIPT_MANIFEST (fail closed, D8)",
    };
  } else if (!REAL_IMAGE_DIGEST.test(manifest.images.base) || !REAL_IMAGE_DIGEST.test(manifest.images.jvm)) {
    // Defensive: a hand-built manifest passed directly (bypassing parseManifest) must still fail closed.
    manifestError = {
      code: "invalid-image",
      message: "manifest base/jvm digest is not a real sha256:<64-hex>",
    };
  } else if (manifest.candidateCommit !== expectedCommit) {
    manifestError = {
      code: "wrong-commit",
      message: `manifest is for commit ${manifest.candidateCommit} but the expected merge candidate is ${expectedCommit}`,
    };
  }
  const manifestUsable = manifestError === undefined && manifest !== null;

  // 2. Scan every O row for owed / malformed / unresolved, and (only when the manifest is usable)
  //    bind each real recorded digest to the trusted manifest.
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
    // inherited | receipt-present: BOTH merge-candidate images (base AND jvm) must resolve to a
    // real sha256 AND match the trusted manifest; one proven image cannot vouch for the other.
    // (isOState already rejected a missing/empty base or jvm as malformed above.)
    for (const image of IMAGES) {
      const digest = c.o.imageDigests[image];
      if (!REAL_IMAGE_DIGEST.test(digest)) {
        unresolved.push({ id: c.id, image, digest });
        continue;
      }
      if (manifestUsable && manifest !== null) {
        const expected = manifest.images[image];
        if (digest !== expected) {
          const other = image === "base" ? "jvm" : "base";
          mismatch.push({ id: c.id, image, recorded: digest, expected, swapped: digest === manifest.images[other] });
        }
      }
    }
  }

  return {
    ok:
      owed.length === 0
      && unresolved.length === 0
      && mismatch.length === 0
      && malformed.length === 0
      && manifestError === undefined,
    owed,
    unresolved,
    mismatch,
    malformed,
    ...(manifestError === undefined ? {} : { manifestError }),
  };
}
