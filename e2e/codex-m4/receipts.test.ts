// PRD #1287 — self-tests for the redesigned D8 O-receipt merge gate. The pure `checkReceipts`
// certifies the current candidate against a trusted, gitignored candidate MANIFEST by proving three
// things: the manifest/ancestry is usable, the provenBaseCommit..candidate tail is EVIDENCE-ONLY
// (touches no shipped guardrail runtime path), and every committed O clause is discharged by exactly
// one manifest record whose digests match the proven image pair. The key SELF-REFERENCE test shows
// the fixed point — recording the gitignored manifest makes no commit, so an evidence-only (or
// empty) tail keeps the candidate certified, while a runtime tail rejects it. A DOCKERFILE-COPY
// pinning test binds classifyPath's deny/allow lists to the worker image's real COPY surface so they
// cannot silently drift, and a registry assertion confirms no committed O row embeds candidate
// digests (they are manifest-only now).

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";

import {
  checkReceipts,
  classifyPath,
  parseManifest,
  type CandidateManifest,
  type ClauseRecord,
  type Disposition,
  type TailInput,
} from "./receipts.js";
import { ALL_CLAUSES } from "./registry.js";
import type { ClauseRow, ImageDigests, OState } from "./clause.js";

/** Clearly-synthetic, real-shaped merge-candidate digests (sha256: + 64 lowercase hex). */
const REAL_BASE = `sha256:${"deadbeef".repeat(8)}`;
const REAL_JVM = `sha256:${"feedface".repeat(8)}`;
/** A fixed valid 40-hex proven-base commit. */
const PROVEN_BASE_COMMIT = "a".repeat(40);
/** The canonical happy tail: an evidence-only diff, base reachable from the candidate. */
const EVIDENCE_TAIL: TailInput = { paths: ["e2e/codex-m4/receipts.ts", "prds/1287-x.md"], baseIsAncestor: true };

const INHERITED: OState = { kind: "inherited", source: "src", target: "image", unchangedJustification: "unchanged" };
const OWED: OState = { kind: "owed", target: "image", reason: "needs proof", owner: "maintainer" };

/** Build a committed O ClauseRow. */
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

/** Build a manifest per-clause record. */
function rec(
  id: string,
  disposition: Disposition = "inherited",
  images: ImageDigests = { base: REAL_BASE, jvm: REAL_JVM },
  provenance = "receipt provenance",
): ClauseRecord {
  return { id, disposition, images, provenance };
}

/** The canonical committed O registry for these tests: one inherited requirement + one owed one. */
function committed(): ClauseRow[] {
  return [orow("codex-o-a", INHERITED), orow("codex-o-b", OWED)];
}

/** Records that fully discharge `committed()` with digests matching the proven pair. */
function fullRecords(): ClauseRecord[] {
  return [rec("codex-o-a", "inherited"), rec("codex-o-b", "receipt-present")];
}

/** A valid base manifest binding REAL_BASE/REAL_JVM to PROVEN_BASE_COMMIT. */
function baseManifest(clauses: ClauseRecord[] = fullRecords(), overrides: Partial<CandidateManifest> = {}): CandidateManifest {
  return {
    provenBaseCommit: PROVEN_BASE_COMMIT,
    images: { base: REAL_BASE, jvm: REAL_JVM },
    provenance: "test packaged proof",
    clauses,
    ...overrides,
  };
}

describe("checkReceipts", () => {
  it("HAPPY PATH: every committed O clause discharged, evidence-only tail → ok", () => {
    const report = checkReceipts(committed(), baseManifest(), EVIDENCE_TAIL);
    assert.equal(report.ok, true);
    assert.deepEqual(report.drift, []);
    assert.deepEqual(report.missing, []);
    assert.deepEqual(report.extra, []);
    assert.deepEqual(report.duplicate, []);
    assert.deepEqual(report.undischarged, []);
    assert.deepEqual(report.mismatch, []);
    assert.deepEqual(report.malformedClauses, []);
    assert.equal(report.manifestError, undefined);
  });

  it("SELF-REFERENCE SEAM: a runtime tail rejects, an evidence-only/empty tail certifies", () => {
    // A tail touching the shipped codex guardrail runtime rejects the candidate...
    const runtime = checkReceipts(committed(), baseManifest(), { paths: ["agent/src/guardrails.ts"], baseIsAncestor: true });
    assert.equal(runtime.ok, false);
    assert.deepEqual(runtime.drift, [{ path: "agent/src/guardrails.ts", kind: "runtime" }]);

    // ...as does an unrecognized (unclassified) path, which fails closed...
    const unknown = checkReceipts(committed(), baseManifest(), { paths: ["api/foo.go"], baseIsAncestor: true });
    assert.equal(unknown.ok, false);
    assert.deepEqual(unknown.drift, [{ path: "api/foo.go", kind: "unclassified" }]);

    // ...but an EMPTY tail (candidate == proven base) keeps the candidate certified. THIS is the
    // fixed point: recording the gitignored manifest makes no commit, so the tail stays empty (or
    // evidence-only) and never invalidates the very candidate it certifies.
    const empty = checkReceipts(committed(), baseManifest(), { paths: [], baseIsAncestor: true });
    assert.equal(empty.ok, true);
    assert.deepEqual(empty.drift, []);
  });

  it("a null (absent) manifest fails closed; missing lists every committed O id", () => {
    const report = checkReceipts(committed(), null, EVIDENCE_TAIL);
    assert.equal(report.ok, false);
    assert.equal(report.manifestError?.code, "absent");
    assert.deepEqual([...report.missing].sort(), ["codex-o-a", "codex-o-b"]);
    assert.deepEqual(report.drift, []);
  });

  it("base-not-ancestor: a non-ancestor base OR a null tail fails closed", () => {
    const notAncestor = checkReceipts(committed(), baseManifest(), { paths: [], baseIsAncestor: false });
    assert.equal(notAncestor.ok, false);
    assert.equal(notAncestor.manifestError?.code, "base-not-ancestor");

    const nullTail = checkReceipts(committed(), baseManifest(), null);
    assert.equal(nullTail.ok, false);
    assert.equal(nullTail.manifestError?.code, "base-not-ancestor");
  });

  it("invalid-image: a non-sha256 proven image fails closed (before base-not-ancestor)", () => {
    // Constructed directly, bypassing parseManifest (which would reject it structurally).
    const bad = baseManifest(fullRecords(), { images: { base: "nope", jvm: REAL_JVM } });
    const report = checkReceipts(committed(), bad, EVIDENCE_TAIL);
    assert.equal(report.ok, false);
    assert.equal(report.manifestError?.code, "invalid-image");

    // First-applicable-wins: an invalid image is reported ahead of a non-ancestor base.
    const both = checkReceipts(committed(), bad, { paths: [], baseIsAncestor: false });
    assert.equal(both.manifestError?.code, "invalid-image");
  });

  it("missing record: dropping an O clause's record lists it in missing", () => {
    const m = baseManifest([rec("codex-o-a", "inherited")]); // codex-o-b has no record
    const report = checkReceipts(committed(), m, EVIDENCE_TAIL);
    assert.equal(report.ok, false);
    assert.deepEqual(report.missing, ["codex-o-b"]);
  });

  it("extra record: a record id that is not a known O clause is listed in extra", () => {
    const m = baseManifest([...fullRecords(), rec("not-an-o-clause")]);
    const report = checkReceipts(committed(), m, EVIDENCE_TAIL);
    assert.equal(report.ok, false);
    assert.deepEqual(report.extra, ["not-an-o-clause"]);
  });

  it("duplicate record: an id appearing more than once is listed once in duplicate", () => {
    const m = baseManifest([rec("codex-o-a", "inherited"), rec("codex-o-a", "inherited"), rec("codex-o-b", "receipt-present")]);
    const report = checkReceipts(committed(), m, EVIDENCE_TAIL);
    assert.equal(report.ok, false);
    assert.deepEqual(report.duplicate, ["codex-o-a"]);
  });

  it("a record for a real but non-O clause id is extra (it is not in requiredOIds)", () => {
    const pClause = ALL_CLAUSES.find((c) => c.layer !== "O");
    assert.ok(pClause !== undefined, "the registry has a non-O clause to borrow a real id from");
    const m = baseManifest([rec(pClause.id)]);
    const report = checkReceipts(ALL_CLAUSES, m, EVIDENCE_TAIL);
    assert.ok(report.extra.includes(pClause.id), "a non-O clause id is never a discharge target");
  });

  it("an owed disposition in the JSON is rejected by parseManifest as malformed", () => {
    const result = parseManifest({
      provenBaseCommit: PROVEN_BASE_COMMIT,
      images: { base: REAL_BASE, jvm: REAL_JVM },
      provenance: "p",
      clauses: [{ id: "codex-o-a", disposition: "owed", images: { base: REAL_BASE, jvm: REAL_JVM }, provenance: "p" }],
    });
    assert.ok("error" in result);
    assert.match(result.error, /disposition/);
  });

  it("undischarged: a directly-constructed record with a bad disposition fails closed", () => {
    const m = baseManifest([
      { id: "codex-o-a", disposition: "owed" as unknown as Disposition, images: { base: REAL_BASE, jvm: REAL_JVM }, provenance: "p" },
      rec("codex-o-b", "receipt-present"),
    ]);
    const report = checkReceipts(committed(), m, EVIDENCE_TAIL);
    assert.equal(report.ok, false);
    assert.deepEqual(report.undischarged, ["codex-o-a"]);
  });

  it("mismatch: a real-but-wrong recorded digest is a mismatch (swapped=false)", () => {
    const wrong = `sha256:${"11".repeat(32)}`;
    const m = baseManifest([rec("codex-o-a", "inherited", { base: wrong, jvm: REAL_JVM }), rec("codex-o-b", "receipt-present")]);
    const report = checkReceipts(committed(), m, EVIDENCE_TAIL);
    assert.equal(report.ok, false);
    assert.deepEqual(report.mismatch, [{ id: "codex-o-a", image: "base", recorded: wrong, expected: REAL_BASE, swapped: false }]);
  });

  it("swapped: base/jvm swapped in a record → two mismatches, both flagged swapped", () => {
    const m = baseManifest([rec("codex-o-a", "inherited", { base: REAL_JVM, jvm: REAL_BASE }), rec("codex-o-b", "receipt-present")]);
    const report = checkReceipts(committed(), m, EVIDENCE_TAIL);
    assert.equal(report.ok, false);
    const forA = report.mismatch.filter((x) => x.id === "codex-o-a");
    assert.equal(forA.length, 2);
    for (const row of forA) assert.equal(row.swapped, true, "each recorded digest is the manifest's OTHER image");
    const base = forA.find((r) => r.image === "base");
    const jvm = forA.find((r) => r.image === "jvm");
    assert.equal(base?.recorded, REAL_JVM);
    assert.equal(base?.expected, REAL_BASE);
    assert.equal(jvm?.recorded, REAL_BASE);
    assert.equal(jvm?.expected, REAL_JVM);
  });

  it("malformedClauses: a committed O row with a broken o-state is listed (its requirement can't be read)", () => {
    const clauses = [orow("codex-o-bad", undefined), orow("codex-o-b", INHERITED)];
    const m = baseManifest([rec("codex-o-bad"), rec("codex-o-b")]);
    const report = checkReceipts(clauses, m, EVIDENCE_TAIL);
    assert.equal(report.ok, false);
    assert.deepEqual(report.malformedClauses, ["codex-o-bad"]);
  });

  it("ignores non-O rows (no O requirement + an empty manifest → ok)", () => {
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
    const report = checkReceipts([nonO], baseManifest([]), EVIDENCE_TAIL);
    assert.equal(report.ok, true);
  });

  it("the REAL registry with no manifest fails closed, every O clause missing (fail-closed at HEAD)", () => {
    const report = checkReceipts(ALL_CLAUSES, null, null);
    assert.equal(report.ok, false);
    assert.equal(report.manifestError?.code, "absent");
    const oIds = ALL_CLAUSES.filter((c) => c.layer === "O").map((c) => c.id);
    assert.ok(oIds.length >= 3, "the C1 registry seeds at least three O clauses");
    assert.deepEqual([...report.missing].sort(), [...oIds].sort());
  });
});

describe("parseManifest structural validation (first violation wins)", () => {
  const ok = { provenBaseCommit: PROVEN_BASE_COMMIT, images: { base: REAL_BASE, jvm: REAL_JVM }, provenance: "p", clauses: [] as unknown[] };
  const err = (raw: unknown): string => {
    const result = parseManifest(raw);
    assert.ok("error" in result, "expected a structural error");
    return result.error;
  };

  it("rejects a non-object", () => {
    assert.ok("error" in parseManifest(null));
    assert.ok("error" in parseManifest(42));
  });

  it("rejects a missing/bad provenBaseCommit", () => {
    assert.match(err({ ...ok, provenBaseCommit: "" }), /provenBaseCommit/);
    assert.match(err({ ...ok, provenBaseCommit: "xyz" }), /provenBaseCommit/);
  });

  it("rejects missing/bad images", () => {
    assert.match(err({ ...ok, images: undefined }), /images/);
    assert.match(err({ ...ok, images: { base: "nope", jvm: REAL_JVM } }), /images\.base/);
  });

  it("rejects an empty provenance", () => {
    assert.match(err({ ...ok, provenance: "" }), /provenance/);
  });

  it("rejects non-array clauses", () => {
    assert.match(err({ ...ok, clauses: {} }), /clauses/);
  });

  it("rejects a clause missing id / bad disposition / bad images / empty provenance", () => {
    const withClause = (c: unknown): string => err({ ...ok, clauses: [c] });
    assert.match(withClause({ disposition: "inherited", images: { base: REAL_BASE, jvm: REAL_JVM }, provenance: "p" }), /id/);
    assert.match(withClause({ id: "x", disposition: "nope", images: { base: REAL_BASE, jvm: REAL_JVM }, provenance: "p" }), /disposition/);
    assert.match(withClause({ id: "x", disposition: "inherited", images: { base: "nope", jvm: REAL_JVM }, provenance: "p" }), /images/);
    assert.match(withClause({ id: "x", disposition: "inherited", images: { base: REAL_BASE, jvm: REAL_JVM }, provenance: "" }), /provenance/);
  });

  it("accepts a well-formed manifest and preserves its fields (incl. recordedAt)", () => {
    const result = parseManifest({
      ...ok,
      clauses: [{ id: "codex-o-a", disposition: "receipt-present", images: { base: REAL_BASE, jvm: REAL_JVM }, provenance: "rp" }],
      recordedAt: "2026-09-13",
    });
    assert.ok("manifest" in result);
    assert.equal(result.manifest.provenBaseCommit, PROVEN_BASE_COMMIT);
    assert.equal(result.manifest.images.base, REAL_BASE);
    assert.equal(result.manifest.clauses.length, 1);
    assert.equal(result.manifest.clauses[0]?.id, "codex-o-a");
    assert.equal(result.manifest.clauses[0]?.disposition, "receipt-present");
    assert.equal(result.manifest.recordedAt, "2026-09-13");
  });

  it("the committed example manifest still matches the parseManifest schema", () => {
    // The e2e tree has no package.json, so its .ts files are CommonJS-typed (no import.meta) — the
    // whole tree resolves paths via __dirname. Load the tracked template relative to this test file.
    const raw = readFileSync(path.join(__dirname, "receipt-manifest.example.json"), "utf8");
    const result = parseManifest(JSON.parse(raw));
    assert.ok("manifest" in result, "the tracked example template must parse (guards against schema drift)");
    assert.equal(result.manifest.clauses.length, 3, "the example discharges all three committed O clauses");
  });
});

describe("committed O registry (finding [11]: no candidate digests in the tracked rows)", () => {
  it("every committed O row carries NO imageDigests and is inherited|owed", () => {
    const oRows = ALL_CLAUSES.filter((c) => c.layer === "O");
    assert.ok(oRows.length > 0, "the registry seeds O rows");
    for (const c of oRows) {
      assert.ok(c.o !== undefined, `${c.id} has an o-state`);
      assert.ok(!("imageDigests" in c.o), `${c.id} must not embed a candidate imageDigests block`);
      assert.ok(c.o.kind === "inherited" || c.o.kind === "owed", `${c.id} kind is inherited|owed`);
    }
  });
});

describe("classifyPath is pinned to the Dockerfile COPY surface (anti-drift)", () => {
  /** Parse every real repo-path COPY source out of a Dockerfile: drop flags (`--...`), SKIP a
   *  build-stage copy (`--from=`), take all-but-last (dest) as sources, SKIP the chat-only `.`. */
  function copySources(dockerfile: string): string[] {
    const sources: string[] = [];
    for (const rawLine of dockerfile.split("\n")) {
      const line = rawLine.trim();
      if (!line.startsWith("COPY ")) continue;
      const tokens = line.slice("COPY ".length).trim().split(/\s+/).filter((t) => t.length > 0);
      if (tokens.some((t) => t.startsWith("--from="))) continue; // build-stage copy, not a repo path
      const withoutFlags = tokens.filter((t) => !t.startsWith("--"));
      if (withoutFlags.length < 2) continue; // need at least one source + a dest
      for (const src of withoutFlags.slice(0, -1)) {
        if (src === ".") continue; // the chat-only whole-repo copy (COPY . /opt/uzi-src)
        sources.push(src);
      }
    }
    return sources;
  }

  it("every COPY'd repo path in the base + jvm worker images classifies as runtime", () => {
    // This PINS classifyPath's deny/allow lists to the worker image's real runtime COPY surface, so
    // they cannot silently drift from what the guardrail image actually ships.
    const base = readFileSync(path.join(__dirname, "..", "..", "agent", "templates", "base", "Dockerfile"), "utf8");
    const jvm = readFileSync(path.join(__dirname, "..", "..", "agent", "templates", "jvm", "Dockerfile"), "utf8");
    const sources = [...copySources(base), ...copySources(jvm)];
    // Guard against a vacuous pass: a broken parser that finds too few sources fails loudly.
    assert.ok(sources.length >= 5, `expected >= 5 asserted COPY sources, the parser found ${sources.length}`);
    for (const src of sources) {
      assert.equal(classifyPath(src), "runtime", `COPY source ${src} must classify as runtime`);
    }
  });
});
