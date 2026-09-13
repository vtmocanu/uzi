// PRD #1287 — the redesigned D8 O-receipt merge gate, SEPARATE from the ordinary completeness gate.
//
// WHAT IT CERTIFIES. `check:codex-m4-receipts` (this module + run-receipts.ts) is the LEAD's
// pre-merge gate certifying the maintainer-only "O-layer" packaged security proofs for the codex
// worker: every committed O-layer clause (the STABLE requirements in clauses-codex.ts) is bound to
// a fresh, out-of-band packaged proof for the current candidate. It is deliberately NOT folded into
// gate:agent / test:codex-m4 — the worker cannot run packaged O proofs or know the merge-candidate
// digest, so none of these categories may redden the worker's ordinary gate, only the lead's check.
//
// WHY THE REDESIGN (the fixed-point / self-reference property). The OLD design committed candidate
// image digests INTO clauses-codex.ts and required `manifest.candidateCommit === git rev-parse HEAD`.
// That has NO FIXED POINT: committing a digest changes HEAD, and because the worker Dockerfiles
// `COPY . /opt/uzi-src` and stamp UZI_SRC_SHA, changing HEAD changes the image digest — so the
// just-committed digest can never match the image it names. The redesign moves ALL candidate-specific
// evidence (the proven image pair and the per-clause receipts) into a GITIGNORED manifest
// (receipt-manifest.json) and certifies the CURRENT tree by proving the tail from an already-proven
// base is "evidence-only" — it touches no runtime-affecting path. Recording the gitignored manifest
// makes NO commit, so it cannot invalidate the very candidate it certifies: that is the fixed point.
//
// THE EVIDENCE-ONLY-TAIL RULE. The manifest names a `provenBaseCommit` whose two images were
// packaged-proven. The gate accepts the current candidate iff (a) the proven base is an ANCESTOR of
// the candidate, and (b) every path in the `provenBaseCommit..candidate` diff classifies as
// `evidence` — i.e. the tail changes nothing in the shipped codex guardrail RUNTIME. An empty tail
// (candidate == proven base) trivially certifies; a tail touching a runtime path rejects.
//
// THE DENY / ALLOW LISTS (classifyPath). The shipped codex guardrail RUNTIME is everything under
// `agent/` EXCEPT `agent/test/` (agent/src is COPY'd to /app and run via tsx; agent/codex/supervisor
// is go-built; agent/templates holds the Dockerfiles + entrypoint; agent/package*.json,
// agent/tsconfig.json, agent/devbox-global, agent/bin are build inputs). Everything else that the
// worker image does not run as a guardrail — agent/test/, e2e/, prds/, docs/, adr/, specs/, .claude/,
// Taskfile.yml, and *.md — is EVIDENCE. The deny list (agent/ minus agent/test/) is checked FIRST so
// a runtime path can never be mis-shelved into the allow list; an unrecognized path is `unclassified`
// and rejects (fail closed — a new top-level path must be classified deliberately). receipts.test.ts
// PINS these lists to the real Dockerfile COPY set, so they cannot silently drift from what the
// worker image actually ships. A KEY invariant this encodes: `Taskfile.yml` (which merely names how
// to BUILD/prove) never determines the shipped guardrail image identity, so it is `evidence`.
//
// THE TRUST BOUNDARY. The gate TRUSTS the out-of-band manifest (the maintainer produced it from a
// real `UZI_CODEX_M3B_PACKAGED=1 task test:codex-m3b:packaged` proof). It verifies the manifest's
// INTERNAL CONSISTENCY (well-formed, real image digests), full COVERAGE (every committed O clause
// has exactly one discharging record whose digests match the proven pair AND whose disposition
// matches the committed requirement — a committed `owed` clause has NO prior proof, so it must be
// discharged by a FRESH `receipt-present` record, never an `inherited` one that would falsely claim a
// prior unchanged mechanism; a committed `inherited` clause accepts either disposition), and NO
// RUNTIME DRIFT in the tail. It does NOT — and cannot, from inside the worker — verify that the
// digests were truly built from `provenBaseCommit`; that is the maintainer's out-of-band responsibility.

import type { ClauseRow, ImageDigests } from "./clause.js";
import { isOState } from "./clause.js";

/** A real, resolved image digest: `sha256:` + 64 lowercase hex. Anything else is not a real digest. */
const REAL_IMAGE_DIGEST = /^sha256:[0-9a-f]{64}$/;

/** A full git commit hex: 40 (SHA-1) or 64 (SHA-256) lowercase hex chars. */
const FULL_GIT_HEX = /^[0-9a-f]{40}$|^[0-9a-f]{64}$/;

/** The two merge-candidate images every O proof binds (D8): a candidate ships `base` and `jvm`. */
const IMAGES = ["base", "jvm"] as const;

/** How a candidate O receipt was produced. `inherited` — an unchanged mechanism carried from a
 *  prior proof; `receipt-present` — a fresh packaged snapshot recorded for this candidate. (The
 *  committed registry no longer carries a disposition; a receipt's disposition is manifest-only.) */
export type Disposition = "inherited" | "receipt-present";

/** One per-clause candidate receipt inside the manifest: the candidate-specific evidence that
 *  discharges a committed O clause requirement. */
export interface ClauseRecord {
  readonly id: string;
  readonly disposition: Disposition;
  readonly images: ImageDigests;
  readonly provenance: string;
}

/** The trusted, out-of-band candidate manifest (gitignored receipt-manifest.json). It pins the
 *  proven base revision + its two proven image digests, and carries one discharging record per
 *  committed O clause. Recording it makes NO commit, so it never invalidates the candidate it
 *  certifies (the fixed-point property, see the module header). */
export interface CandidateManifest {
  /** The proven revision whose images were packaged-proven; the tail is measured from here. */
  readonly provenBaseCommit: string;
  /** The two proven candidate image digests (`sha256:<64hex>` each). */
  readonly images: ImageDigests;
  /** How the proof was produced (free text, presence-checked). */
  readonly provenance: string;
  /** The per-clause receipts. An ARRAY (not a map) so duplicate ids are detectable. */
  readonly clauses: readonly ClauseRecord[];
  /** Optional ISO-ish timestamp of when the proof was recorded. */
  readonly recordedAt?: string;
}

/** Fail-closed STRUCTURAL validator for an `images` sub-object (the manifest's proven pair or a
 *  record's pair). Returns `{ images }` normalized, or `{ error }` naming the first violation. */
function parseImages(raw: unknown, label: string): { images: ImageDigests } | { error: string } {
  if (raw === null || typeof raw !== "object") {
    return { error: `${label} is missing or not an object` };
  }
  const images = raw as Record<string, unknown>;
  for (const image of IMAGES) {
    const digest = images[image];
    if (typeof digest !== "string" || digest.length === 0) {
      return { error: `${label}.${image} is missing or empty` };
    }
    if (!REAL_IMAGE_DIGEST.test(digest)) {
      return { error: `${label}.${image} is not a real sha256:<64-hex>` };
    }
  }
  return { images: { base: images.base as string, jvm: images.jvm as string } };
}

/** Fail-closed STRUCTURAL validator for a raw (parsed-JSON) manifest, first-violation-wins. Returns
 *  `{ manifest }` with a normalized object on success, or `{ error }` naming the first violation. */
export function parseManifest(raw: unknown): { manifest: CandidateManifest } | { error: string } {
  if (raw === null || typeof raw !== "object") {
    return { error: "manifest is not a non-null object" };
  }
  const m = raw as Record<string, unknown>;

  if (typeof m.provenBaseCommit !== "string" || m.provenBaseCommit.length === 0) {
    return { error: "manifest.provenBaseCommit is missing or not a non-empty string" };
  }
  if (!FULL_GIT_HEX.test(m.provenBaseCommit)) {
    return { error: "manifest.provenBaseCommit is not a 40- or 64-char lowercase git hex" };
  }

  const imagesResult = parseImages(m.images, "manifest.images");
  if ("error" in imagesResult) return imagesResult;

  if (typeof m.provenance !== "string" || m.provenance.length === 0) {
    return { error: "manifest.provenance is missing or not a non-empty string" };
  }

  if (!Array.isArray(m.clauses)) {
    return { error: "manifest.clauses is missing or not an array" };
  }
  const clauses: ClauseRecord[] = [];
  for (let i = 0; i < m.clauses.length; i++) {
    const recRaw: unknown = m.clauses[i];
    if (recRaw === null || typeof recRaw !== "object") {
      return { error: `manifest.clauses[${i}] is not an object` };
    }
    const rec = recRaw as Record<string, unknown>;
    if (typeof rec.id !== "string" || rec.id.length === 0) {
      return { error: `manifest.clauses[${i}].id is missing or not a non-empty string` };
    }
    if (rec.disposition !== "inherited" && rec.disposition !== "receipt-present") {
      return { error: `manifest.clauses[${i}].disposition must be "inherited" or "receipt-present"` };
    }
    const recImages = parseImages(rec.images, `manifest.clauses[${i}].images`);
    if ("error" in recImages) return recImages;
    if (typeof rec.provenance !== "string" || rec.provenance.length === 0) {
      return { error: `manifest.clauses[${i}].provenance is missing or not a non-empty string` };
    }
    clauses.push({
      id: rec.id,
      disposition: rec.disposition,
      images: recImages.images,
      provenance: rec.provenance,
    });
  }

  if (m.recordedAt !== undefined && typeof m.recordedAt !== "string") {
    return { error: "manifest.recordedAt, when present, must be a string" };
  }

  const manifest: CandidateManifest = {
    provenBaseCommit: m.provenBaseCommit,
    images: imagesResult.images,
    provenance: m.provenance,
    clauses,
    ...(m.recordedAt === undefined ? {} : { recordedAt: m.recordedAt as string }),
  };
  return { manifest };
}

/** The three path classes the evidence-only-tail rule turns on. `runtime` — part of the shipped
 *  codex guardrail runtime; `evidence` — not shipped as a guardrail (tests/docs/e2e/tooling);
 *  `unclassified` — an unrecognized path that fails closed (a new top-level path must be classified
 *  deliberately). The deny list is checked FIRST so a runtime path can never fall into the allow
 *  list. Pinned to the real Dockerfile COPY set by receipts.test.ts (see the module header). */
export type PathClass = "evidence" | "runtime" | "unclassified";

export function classifyPath(p: string): PathClass {
  if (p.startsWith("agent/") && !p.startsWith("agent/test/")) return "runtime";
  if (
    p.startsWith("e2e/") || p.startsWith("prds/") || p.startsWith("docs/") ||
    p.startsWith("adr/") || p.startsWith("specs/") || p.startsWith(".claude/") ||
    p.startsWith("agent/test/") || p === "Taskfile.yml" || p.endsWith(".md")
  ) return "evidence";
  return "unclassified";
}

/** The `provenBaseCommit..candidate` tail, resolved out-of-band by run-receipts.ts. `paths` is the
 *  diff's changed paths (empty when candidate == proven base); `baseIsAncestor` is whether the
 *  proven base is reachable from the candidate. */
export interface TailInput {
  readonly paths: readonly string[];
  readonly baseIsAncestor: boolean;
}

export interface ReceiptsReport {
  readonly ok: boolean;
  /** Set (fail closed) when the manifest / ancestry is unusable so the candidate cannot be
   *  certified at all: `absent` (none supplied), `malformed` (a present-but-broken file — set by
   *  the runner, not this pure check), `invalid-image` (the manifest's proven pair is not real),
   *  or `base-not-ancestor` (the proven base is not reachable from the candidate). */
  readonly manifestError?: { readonly code: "absent" | "malformed" | "invalid-image" | "base-not-ancestor"; readonly message: string };
  /** Tail paths that are NOT evidence-only: a `runtime` path shipped in the guardrail image, or an
   *  `unclassified` path that fails closed. Any drift rejects the candidate. */
  readonly drift: { readonly path: string; readonly kind: "runtime" | "unclassified" }[];
  /** Committed O clause ids with no discharging manifest record. */
  readonly missing: string[];
  /** Record ids that are not a known committed O clause id. */
  readonly extra: string[];
  /** Record ids appearing more than once (listed once each). */
  readonly duplicate: string[];
  /** Record ids with an invalid disposition or a non-real image (defensive — parseManifest already
   *  rejects these, but a directly-constructed manifest must still fail closed). */
  readonly undischarged: string[];
  /** Records whose real digest does NOT match the manifest's proven pair for that image. `swapped`
   *  is true when the recorded digest is instead the OTHER image (base recorded as jvm, or v.v.). */
  readonly mismatch: { readonly id: string; readonly image: "base" | "jvm"; readonly recorded: string; readonly expected: string; readonly swapped: boolean }[];
  /** Otherwise-valid records (known id, valid disposition, real images) whose disposition does NOT
   *  match the committed requirement: a committed `owed` clause has no prior proof, so it must be
   *  discharged by a FRESH `receipt-present` record — an `inherited` record would falsely claim a
   *  prior unchanged mechanism that does not exist. (A committed `inherited` clause accepts either
   *  disposition, so it is never listed here.) */
  readonly dispositionMismatch: { readonly id: string; readonly committed: "owed"; readonly recorded: Disposition }[];
  /** Committed O rows whose o-state is missing/malformed (their requirement cannot be read). */
  readonly malformedClauses: string[];
}

/** Pure receipts check. ok=false if the manifest/ancestry is unusable, the tail is not evidence-only,
 *  or any committed O clause is uncovered / any record is extra, duplicate, undischarged, mismatched,
 *  or disposition-mismatched (a committed `owed` clause discharged by something other than a fresh
 *  `receipt-present` record), or any committed O row is malformed. Fails closed: a null manifest
 *  reports every O clause as `missing`; a non-ancestor base or an invalid proven-image pair sets
 *  `manifestError`. */
export function checkReceipts(
  clauses: readonly ClauseRow[],
  manifest: CandidateManifest | null,
  tail: TailInput | null,
): ReceiptsReport {
  // 1. The committed O registry defines the STABLE requirement set. Every O clause id must be
  //    discharged by exactly one manifest record; a committed O row whose o-state cannot be read is
  //    itself a rejection.
  const requiredOIds: string[] = [];
  const requiredOIdSet = new Set<string>();
  const malformedClauses: string[] = [];
  // The committed disposition requirement per O clause id (only for isOState-valid rows): an `owed`
  // clause has no prior proof and needs a fresh `receipt-present` record; an `inherited` clause
  // accepts either. A malformed O row contributes no entry, so its record is never disposition-checked.
  const committedOKind = new Map<string, "inherited" | "owed">();
  for (const c of clauses) {
    if (c.layer !== "O") continue;
    requiredOIds.push(c.id);
    requiredOIdSet.add(c.id);
    if (!isOState(c.o)) malformedClauses.push(c.id);
    else committedOKind.set(c.id, c.o.kind);
  }

  const drift: ReceiptsReport["drift"] = [];
  const missing: string[] = [];
  const extra: string[] = [];
  const duplicate: string[] = [];
  const undischarged: string[] = [];
  const mismatch: ReceiptsReport["mismatch"] = [];
  const dispositionMismatch: ReceiptsReport["dispositionMismatch"] = [];

  // 2. No manifest → fail closed. Every O requirement is `missing` (nothing binds it); the report
  //    stays a complete maintainer TODO list (malformed committed rows still surface).
  if (manifest === null) {
    return {
      ok: false,
      manifestError: {
        code: "absent",
        message: "candidate manifest absent — supply CODEX_M4_RECEIPT_MANIFEST (fail closed, D8)",
      },
      drift,
      missing: [...requiredOIds],
      extra,
      duplicate,
      undischarged,
      mismatch,
      dispositionMismatch,
      malformedClauses,
    };
  }

  // 3-4. Manifest usability, fail closed, first-applicable-wins: an invalid proven-image pair
  //      (defensive; parseManifest already rejects it) before a non-ancestor base. (Absence was
  //      handled in step 2.)
  const imagesUsable =
    REAL_IMAGE_DIGEST.test(manifest.images.base) && REAL_IMAGE_DIGEST.test(manifest.images.jvm);
  let manifestError: ReceiptsReport["manifestError"];
  if (!imagesUsable) {
    manifestError = {
      code: "invalid-image",
      message: "manifest proven base/jvm digest is not a real sha256:<64-hex>",
    };
  } else if (tail === null || !tail.baseIsAncestor) {
    manifestError = {
      code: "base-not-ancestor",
      message:
        `manifest provenBaseCommit ${manifest.provenBaseCommit} is not an ancestor of the candidate `
        + "— an evidence-only tail cannot be proven (fail closed, D8)",
    };
  }

  // 5. Evidence-only-tail drift: only computable when the base IS an ancestor (so the paths are the
  //    real provenBaseCommit..candidate diff). A runtime-affecting or unclassified path rejects the
  //    candidate; an evidence-only path is ignored. This is the self-reference seam: recording the
  //    gitignored manifest makes no commit, so an evidence-only (or empty) tail keeps the candidate
  //    certified, while a runtime tail rejects it.
  if (tail !== null && tail.baseIsAncestor) {
    for (const p of tail.paths) {
      const cls = classifyPath(p);
      if (cls === "runtime") drift.push({ path: p, kind: "runtime" });
      else if (cls === "unclassified") drift.push({ path: p, kind: "unclassified" });
    }
  }

  // 6. Records reconciliation against the committed O registry.
  const counts = new Map<string, number>();
  for (const rec of manifest.clauses) counts.set(rec.id, (counts.get(rec.id) ?? 0) + 1);
  for (const [id, n] of counts) {
    if (n > 1) duplicate.push(id);
  }

  const recordedIds = new Set<string>();
  const extraSeen = new Set<string>();
  const undischargedSeen = new Set<string>();
  for (const rec of manifest.clauses) {
    recordedIds.add(rec.id);
    if (!requiredOIdSet.has(rec.id)) {
      if (!extraSeen.has(rec.id)) {
        extraSeen.add(rec.id);
        extra.push(rec.id);
      }
      continue;
    }
    // Defensive: parseManifest already rejects a bad disposition / non-real image, but a
    // directly-constructed manifest must still fail closed here.
    const dispositionOk = rec.disposition === "inherited" || rec.disposition === "receipt-present";
    const imagesOk = REAL_IMAGE_DIGEST.test(rec.images.base) && REAL_IMAGE_DIGEST.test(rec.images.jvm);
    if (!dispositionOk || !imagesOk) {
      if (!undischargedSeen.has(rec.id)) {
        undischargedSeen.add(rec.id);
        undischarged.push(rec.id);
      }
      continue;
    }
    // Cross-check the record disposition against the committed requirement. Only reachable for an
    // otherwise-valid record (known id, valid disposition, real images). A committed `owed` clause has
    // NO prior proof, so an `inherited` record cannot discharge it — only a fresh `receipt-present`
    // one can. (A committed `inherited` clause accepts either, and a malformed O row has no entry.)
    if (committedOKind.get(rec.id) === "owed" && rec.disposition !== "receipt-present") {
      dispositionMismatch.push({ id: rec.id, committed: "owed", recorded: rec.disposition });
    }
    if (imagesUsable) {
      for (const image of IMAGES) {
        const recorded = rec.images[image];
        const expected = manifest.images[image];
        if (recorded !== expected) {
          const other = image === "base" ? "jvm" : "base";
          mismatch.push({ id: rec.id, image, recorded, expected, swapped: recorded === manifest.images[other] });
        }
      }
    }
  }
  for (const id of requiredOIds) {
    if (!recordedIds.has(id)) missing.push(id);
  }

  const ok =
    manifestError === undefined
    && drift.length === 0
    && missing.length === 0
    && extra.length === 0
    && duplicate.length === 0
    && undischarged.length === 0
    && mismatch.length === 0
    && dispositionMismatch.length === 0
    && malformedClauses.length === 0;

  return {
    ok,
    ...(manifestError === undefined ? {} : { manifestError }),
    drift,
    missing,
    extra,
    duplicate,
    undischarged,
    mismatch,
    dispositionMismatch,
    malformedClauses,
  };
}
