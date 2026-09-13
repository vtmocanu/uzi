// PRD #1287 C1 — self-tests for the O-receipt merge gate (D8). The pure `checkReceipts` rejects
// any owed (or malformed) O row, rejects an inherited/receipt-present row whose digest is an
// unresolved placeholder, and — the D8 hardening — BINDS each recorded {base,jvm} digest to a
// trusted candidate MANIFEST for the expected commit, failing closed when the manifest is absent,
// invalid, for the wrong commit, or a recorded digest is fabricated/swapped/mismatched. It also
// asserts the REAL registry is red at C1 for BOTH the seeded owed rows AND the seeded inherited
// row's placeholder base/jvm digests (`sha256:PENDING-CANDIDATE-DIGEST-BASE`/`-JVM`) — all
// maintainer-owed before merge. A cross-check confirms the ORDINARY completeness checker still
// treats that placeholder row as well-formed (the divergence is deliberate: the worker gate stays
// green, the merge gate goes red).

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";

import { checkReceipts, parseManifest, type CandidateManifest } from "./receipts.js";
import { checkCompleteness } from "./completeness.js";
import { ALL_CLAUSES } from "./registry.js";
import type { ClauseRow, OState } from "./clause.js";

function orow(id: string, o: OState | undefined): ClauseRow {
  return {
    id,
    adapter: "codex",
    layer: "O",
    family: "F",
    seam: "seam",
    positiveControl: "pc",
    negativeOracle: "no",
    intendedOutcome: "io",
    tests: [],
    ...(o === undefined ? {} : { o }),
  };
}

/** Real, resolved merge-candidate digests (sha256: + 64 lowercase hex) for the two images. */
const REAL_BASE = `sha256:${"deadbeef".repeat(8)}`;
const REAL_JVM = `sha256:${"feedface".repeat(8)}`;
/** The exact placeholders the C1 registry seeds into its inherited O row's base/jvm digests. */
const PLACEHOLDER_BASE = "sha256:PENDING-CANDIDATE-DIGEST-BASE";
const PLACEHOLDER_JVM = "sha256:PENDING-CANDIDATE-DIGEST-JVM";

/** A fixed valid 40-hex commit, and the trusted manifest that binds REAL_BASE/REAL_JVM to it. Every
 *  checkReceipts call passes these so a recorded digest must MATCH the manifest, not merely look real. */
const EXPECTED_COMMIT = "a".repeat(40);
const MANIFEST: CandidateManifest = {
  candidateCommit: EXPECTED_COMMIT,
  images: { base: REAL_BASE, jvm: REAL_JVM },
  provenance: "test packaged proof",
};

const INHERITED: OState = {
  kind: "inherited",
  source: "src",
  imageDigests: { base: REAL_BASE, jvm: REAL_JVM },
  target: "image",
  unchangedJustification: "unchanged",
};
const INHERITED_PLACEHOLDER: OState = { ...INHERITED, imageDigests: { base: PLACEHOLDER_BASE, jvm: PLACEHOLDER_JVM } };
const RECEIPT: OState = { kind: "receipt-present", source: "src", imageDigests: { base: REAL_BASE, jvm: REAL_JVM }, target: "image" };
const OWED: OState = { kind: "owed", target: "image", reason: "needs proof", owner: "maintainer" };

describe("checkReceipts", () => {
  it("rejects when any O row is owed, listing target/reason/owner", () => {
    const report = checkReceipts([orow("a", INHERITED), orow("b", OWED)], MANIFEST, EXPECTED_COMMIT);
    assert.equal(report.ok, false);
    assert.equal(report.owed.length, 1);
    assert.equal(report.owed[0]?.id, "b");
    assert.equal(report.owed[0]?.owner, "maintainer");
    assert.equal(report.owed[0]?.reason, "needs proof");
  });

  it("accepts an all-inherited / receipt-present set with REAL digests", () => {
    const report = checkReceipts([orow("a", INHERITED), orow("b", RECEIPT)], MANIFEST, EXPECTED_COMMIT);
    assert.deepEqual(report.owed, []);
    assert.deepEqual(report.unresolved, []);
    assert.deepEqual(report.mismatch, []);
    assert.deepEqual(report.malformed, []);
    assert.equal(report.manifestError, undefined);
    assert.equal(report.ok, true);
  });

  it("(a) an inherited row with a real 64-hex sha256 digest is NOT flagged", () => {
    const report = checkReceipts([orow("real", INHERITED)], MANIFEST, EXPECTED_COMMIT);
    assert.deepEqual(report.unresolved, []);
    assert.deepEqual(report.mismatch, []);
    assert.equal(report.ok, true);
  });

  it("(b) an inherited row with placeholder digests is flagged as unresolved (not owed)", () => {
    const report = checkReceipts([orow("pend", INHERITED_PLACEHOLDER)], MANIFEST, EXPECTED_COMMIT);
    assert.equal(report.ok, false);
    assert.deepEqual(report.owed, [], "a placeholder digest is a distinct category from owed");
    // BOTH base and jvm are placeholders → one unresolved row per image; not real sha256, so not mismatch.
    assert.equal(report.unresolved.length, 2);
    assert.deepEqual(report.mismatch, []);
    for (const row of report.unresolved) assert.equal(row.id, "pend");
    assert.deepEqual(report.unresolved.map((r) => r.image).sort(), ["base", "jvm"]);
  });

  it("binds BOTH merge-candidate images: one proven image cannot vouch for the other", () => {
    // base real (matches manifest), jvm placeholder → still unresolved (only jvm), NOT accepted.
    const halfProven: OState = { ...INHERITED, imageDigests: { base: REAL_BASE, jvm: PLACEHOLDER_JVM } };
    const half = checkReceipts([orow("half", halfProven)], MANIFEST, EXPECTED_COMMIT);
    assert.equal(half.ok, false, "a real base cannot vouch for an unresolved jvm");
    assert.equal(half.unresolved.length, 1);
    assert.equal(half.unresolved[0]?.id, "half");
    assert.equal(half.unresolved[0]?.image, "jvm");
    assert.equal(half.unresolved[0]?.digest, PLACEHOLDER_JVM);
    // both real AND matching the manifest → passes.
    const bothReal: OState = { ...INHERITED, imageDigests: { base: REAL_BASE, jvm: REAL_JVM } };
    const full = checkReceipts([orow("full", bothReal)], MANIFEST, EXPECTED_COMMIT);
    assert.equal(full.ok, true, "both images proven and manifest-bound → the receipts gate passes");
    assert.deepEqual(full.unresolved, []);
    assert.deepEqual(full.mismatch, []);
  });

  it("flags a receipt-present row whose digest is not sha256:<64-hex>", () => {
    const badReceipt: OState = { ...RECEIPT, imageDigests: { base: REAL_BASE, jvm: "sha256:cafe" } };
    const report = checkReceipts([orow("shortdigest", badReceipt)], MANIFEST, EXPECTED_COMMIT);
    assert.equal(report.ok, false);
    assert.equal(report.unresolved.length, 1);
    assert.equal(report.unresolved[0]?.id, "shortdigest");
    assert.equal(report.unresolved[0]?.image, "jvm");
    assert.equal(report.unresolved[0]?.digest, "sha256:cafe");
  });

  it("(a2) a real-but-wrong digest is a mismatch (not unresolved), swapped=false", () => {
    // base is a real sha256 but is NOT the manifest's base and is NOT the manifest's jvm either.
    const wrong: OState = { ...INHERITED, imageDigests: { base: `sha256:${"11".repeat(32)}`, jvm: REAL_JVM } };
    const report = checkReceipts([orow("wrong", wrong)], MANIFEST, EXPECTED_COMMIT);
    assert.equal(report.ok, false);
    assert.deepEqual(report.unresolved, [], "a real-looking sha256 is not 'unresolved' — it is a mismatch");
    assert.equal(report.mismatch.length, 1);
    assert.equal(report.mismatch[0]?.id, "wrong");
    assert.equal(report.mismatch[0]?.image, "base");
    assert.equal(report.mismatch[0]?.recorded, `sha256:${"11".repeat(32)}`);
    assert.equal(report.mismatch[0]?.expected, REAL_BASE);
    assert.equal(report.mismatch[0]?.swapped, false);
  });

  it("(b2) base/jvm swapped → two mismatches, both flagged swapped", () => {
    const swapped: OState = { ...INHERITED, imageDigests: { base: REAL_JVM, jvm: REAL_BASE } };
    const report = checkReceipts([orow("swap", swapped)], MANIFEST, EXPECTED_COMMIT);
    assert.equal(report.ok, false);
    assert.equal(report.mismatch.length, 2);
    for (const row of report.mismatch) {
      assert.equal(row.id, "swap");
      assert.equal(row.swapped, true, "each recorded digest is the manifest's OTHER image");
    }
    const base = report.mismatch.find((r) => r.image === "base");
    const jvm = report.mismatch.find((r) => r.image === "jvm");
    assert.equal(base?.recorded, REAL_JVM);
    assert.equal(base?.expected, REAL_BASE);
    assert.equal(jvm?.recorded, REAL_BASE);
    assert.equal(jvm?.expected, REAL_JVM);
  });

  it("(c) a null (absent) manifest fails closed, yet owed/unresolved still populate", () => {
    const report = checkReceipts(
      [orow("x", INHERITED), orow("owed1", OWED), orow("pend", INHERITED_PLACEHOLDER)],
      null,
      EXPECTED_COMMIT,
    );
    assert.equal(report.ok, false);
    assert.equal(report.manifestError?.code, "absent");
    // The report is still a complete maintainer TODO list even with no manifest to bind against.
    assert.ok(report.owed.some((r) => r.id === "owed1"), "an owed row still surfaces under a null manifest");
    assert.equal(report.unresolved.length, 2, "the placeholder row's base+jvm still surface under a null manifest");
    for (const row of report.unresolved) assert.equal(row.id, "pend");
    assert.deepEqual(report.mismatch, [], "no mismatch is computed when the manifest is unusable");
  });

  it("(d) a manifest for the wrong commit fails closed", () => {
    const wrongCommit: CandidateManifest = {
      candidateCommit: "b".repeat(40),
      images: { base: REAL_BASE, jvm: REAL_JVM },
      provenance: "test packaged proof",
    };
    const report = checkReceipts([orow("x", INHERITED)], wrongCommit, EXPECTED_COMMIT);
    assert.equal(report.ok, false);
    assert.equal(report.manifestError?.code, "wrong-commit");
  });

  it("(e) parseManifest rejects a missing/invalid image; checkReceipts fails closed on an invalid-image manifest", () => {
    const missingJvm = parseManifest({ candidateCommit: EXPECTED_COMMIT, images: { base: REAL_BASE }, provenance: "p" });
    assert.ok("error" in missingJvm, "a manifest missing the jvm image is rejected");

    const badBase = parseManifest({ candidateCommit: EXPECTED_COMMIT, images: { base: "nope", jvm: REAL_JVM }, provenance: "p" });
    assert.ok("error" in badBase, "a manifest whose base is not a real sha256 is rejected");

    // A hand-built invalid-image manifest passed directly (bypassing parseManifest) still fails closed.
    const report = checkReceipts(
      [orow("x", INHERITED)],
      { candidateCommit: EXPECTED_COMMIT, images: { base: "nope", jvm: REAL_JVM }, provenance: "p" } as CandidateManifest,
      EXPECTED_COMMIT,
    );
    assert.equal(report.ok, false);
    assert.equal(report.manifestError?.code, "invalid-image");
  });

  it("(f) rows whose base/jvm equal the manifest for the matching commit pass with no mismatch", () => {
    const report = checkReceipts([orow("a", INHERITED), orow("b", RECEIPT)], MANIFEST, EXPECTED_COMMIT);
    assert.equal(report.ok, true);
    assert.deepEqual(report.mismatch, []);
    assert.equal(report.manifestError, undefined);
  });

  it("(g) the committed example manifest still matches the parseManifest schema", () => {
    // The e2e tree has no package.json, so its .ts files are CommonJS-typed (no import.meta) — the
    // whole tree resolves paths via __dirname (see provision.ts / packaged-modules.ts). Load the
    // tracked template relative to this test file the same way.
    const raw = readFileSync(path.join(__dirname, "receipt-manifest.example.json"), "utf8");
    const result = parseManifest(JSON.parse(raw));
    assert.ok("manifest" in result, "the tracked example template must parse (guards against schema drift)");
  });

  it("parseManifest accepts a well-formed object and preserves its fields", () => {
    const good = {
      candidateCommit: EXPECTED_COMMIT,
      images: { base: REAL_BASE, jvm: REAL_JVM },
      provenance: "packaged proof",
      recordedAt: "2026-09-13",
    };
    const result = parseManifest(good);
    assert.ok("manifest" in result);
    assert.equal(result.manifest.candidateCommit, EXPECTED_COMMIT);
    assert.equal(result.manifest.images.base, REAL_BASE);
    assert.equal(result.manifest.images.jvm, REAL_JVM);
    assert.equal(result.manifest.provenance, "packaged proof");
    assert.equal(result.manifest.recordedAt, "2026-09-13");
  });

  it("(c-cross) the ORDINARY completeness checker still treats the placeholder row as well-formed", () => {
    // Same placeholder inherited row the receipts gate flags as unresolved — completeness must
    // NOT fail on it (a well-formed inherited/owed record is permitted by the worker's gate).
    const placeholderRow = orow("codex-o-placeholder", INHERITED_PLACEHOLDER);
    const receipts = checkReceipts([placeholderRow], MANIFEST, EXPECTED_COMMIT);
    assert.equal(receipts.ok, false, "the merge gate rejects the placeholder digest");

    const completeness = checkCompleteness([placeholderRow], {}, []);
    assert.equal(completeness.ok, true, "the ordinary completeness gate permits the same well-formed row");
    assert.deepEqual(completeness.failures, []);
  });

  it("rejects a malformed O row (no receipt can be read)", () => {
    const report = checkReceipts([orow("bad", undefined)], MANIFEST, EXPECTED_COMMIT);
    assert.equal(report.ok, false);
    assert.deepEqual(report.malformed, ["bad"]);
  });

  it("ignores non-O rows", () => {
    const nonO: ClauseRow = {
      id: "u",
      adapter: "claude",
      layer: "U",
      family: "F",
      seam: "s",
      positiveControl: "pc",
      negativeOracle: "no",
      intendedOutcome: "io",
      tests: ["t"],
    };
    assert.equal(checkReceipts([nonO], MANIFEST, EXPECTED_COMMIT).ok, true);
  });

  it("is red on the REAL registry for BOTH the owed row AND the placeholder inherited digest", () => {
    const report = checkReceipts(ALL_CLAUSES, MANIFEST, EXPECTED_COMMIT);
    assert.equal(report.ok, false, "the C1 registry seeds an owed O row + a placeholder digest, so the merge gate must be red");
    assert.ok(
      report.owed.some((row) => row.id === "codex-o-descendant-code-mode-host-absence"),
      "the descendant code-mode-host absence O row is owed to the maintainer",
    );
    // The seeded inherited row cites two placeholders (base + jvm), not real merge-candidate
    // digests — so the merge gate now blocks on BOTH as unresolved.
    assert.ok(
      report.unresolved.some((r) => r.id === "codex-o-command-root-home-denial" && r.image === "base" && r.digest === "sha256:PENDING-CANDIDATE-DIGEST-BASE"),
      "the inherited command-root HOME-denial row's placeholder BASE digest is flagged unresolved",
    );
    assert.ok(
      report.unresolved.some((r) => r.id === "codex-o-command-root-home-denial" && r.image === "jvm" && r.digest === "sha256:PENDING-CANDIDATE-DIGEST-JVM"),
      "... and its placeholder JVM digest is flagged unresolved",
    );
  });
});
