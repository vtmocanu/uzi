// Pins the committed replay corpus (fixtures/code-cross-check-replay). The hermetic layer needs no git
// objects. The pin layer rebuilds every pre-fix tree from the checkout's own objects and
// is skipped only in a shallow checkout where every pinned object is absent.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { after, before, describe, it } from "node:test";
import { fileURLToPath } from "node:url";
import {
  CORPUS_DIR, CORPUS_MANIFEST_SHA256, findingMatches, loadReplayCorpus, replayGit, verifyCasePins,
  type ReplayCase, type ReplayCorpus,
} from "../src/code-cross-check-replay.js";
import type { CodeFinding } from "../src/code-cross-check-contract.js";

const CHECKOUT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const CASE_IDS = ["d-2888", "d-3063", "d-2887", "c-2904", "c-2926", "c-3063"];
// A control is the merged squash tree of a defect case's change; it must lack that case's literal.
const COUNTERPART: Record<string, string> = { "c-2926": "d-2888", "c-3063": "d-3063" };

let corpus: ReplayCorpus;
const byId = (id: string): ReplayCase => corpus.cases.find((c) => c.id === id)!;
const patchLines = (patch: Buffer): string[] => patch.toString("utf8").split("\n");
// Added and removed lines only; the "+++"/"---" file headers are not content.
const added = (patch: Buffer): string[] => patchLines(patch).filter((l) => l.startsWith("+") && !l.startsWith("+++"));
const removedLines = (patch: Buffer): string[] => patchLines(patch).filter((l) => l.startsWith("-") && !l.startsWith("---"));

before(async () => { corpus = await loadReplayCorpus(CHECKOUT, { profile: "revised" }); });

describe("committed replay corpus (hermetic)", () => {
  it("loads with the source-pinned manifest hash and verifies every pin and artifact", async () => {
    const manifest = await fs.readFile(path.join(CHECKOUT, CORPUS_DIR, "manifest.json"));
    assert.equal(createHash("sha256").update(manifest).digest("hex"), CORPUS_MANIFEST_SHA256);
    assert.deepEqual(corpus.cases.map((c) => c.id), CASE_IDS);
  });

  it("labels every case as reconstructed with a synthetic plan and no milestones", async () => {
    for (const c of corpus.cases) {
      assert.equal(c.plan_source, "synthetic", c.id);
      assert.equal(c.input_provenance, "reconstructed", c.id);
      const raw = JSON.parse(await fs.readFile(path.join(CHECKOUT, CORPUS_DIR, "cases", c.id, "case.json"), "utf8")) as Record<string, unknown>;
      assert.equal("milestones" in raw, false, c.id);
      assert.match(c.inputs.plan, /^Synthetic plan \(reconstructed for the replay corpus; not the historically approved plan\)/, c.id);
    }
  });

  it("controls remove nothing, expect nothing and share their counterpart's squash", () => {
    for (const id of ["c-2904", "c-2926", "c-3063"]) {
      const c = byId(id);
      assert.equal(c.kind, "control");
      assert.equal(c.reversal, "none");
      assert.equal(c.removedPatch.length, 0, id);
      assert.deepEqual(c.expected, [], id);
      assert.deepEqual(c.witnesses, [], id);
    }
    for (const [control, defect] of Object.entries(COUNTERPART)) {
      assert.equal(byId(control).squash_sha, byId(defect).squash_sha);
      assert.equal(byId(control).base_sha, byId(defect).base_sha);
      assert.notEqual(byId(control).prefix_commit, byId(defect).prefix_commit);
      assert.notEqual(byId(control).prefix_tree, byId(defect).prefix_tree);
    }
  });

  it("records the intended reversal kinds and one read-only defect per defect case", () => {
    assert.deepEqual(
      ["d-2888", "d-3063", "d-2887"].map((id) => [id, byId(id).reversal, byId(id).expected.map((e) => [e.id, e.detectability])]),
      [["d-2888", "recovered", [["D3", "read_only"]]], ["d-3063", "inferred", [["D4", "read_only"]]], ["d-2887", "inferred", [["D1", "read_only"]]]],
    );
  });

  it("every declared signature agrees with its witness blob", async () => {
    for (const c of corpus.cases) {
      for (const d of c.expected) {
        const sig = d.signature;
        assert.ok(sig.witness, `${c.id}/${d.id} has a witness`);
        const w = c.witnesses.find((x) => x.file === sig.witness)!;
        assert.equal(w.path, sig.path);
        const blob = await fs.readFile(path.join(CHECKOUT, CORPUS_DIR, "cases", c.id, w.file), "utf8");
        assert.equal(blob.includes(sig.literal), sig.present, `${c.id}/${d.id}`);
      }
    }
  });

  it("D3: the awaited cancel is pre-existing code that the fix removes", () => {
    const c = byId("d-2888");
    const literal = c.expected[0]!.signature.literal;
    assert.equal(c.expected[0]!.in_diff, false);
    assert.ok(removedLines(c.removedPatch).some((l) => l.includes(literal)));
    assert.equal(c.prefixPatch.toString("utf8").includes(literal), false);
  });

  it("D4: the optional ids line is introduced by the change and rewritten by the fix", () => {
    const c = byId("d-3063");
    const literal = c.expected[0]!.signature.literal;
    assert.equal(c.expected[0]!.in_diff, true);
    assert.ok(removedLines(c.removedPatch).some((l) => l.includes(literal)));
    assert.ok(added(c.prefixPatch).some((l) => l.includes(literal)));
  });

  it("committed match patterns accept defect-specific phrasings and reject generic ones", () => {
    const CASES: Array<[string, string, string[], string[]]> = [
      ["d-2888", "D3", [
        "Awaited reader.cancel() can hang when the source never settles",
        "cancellation never settles, so the overflow error never reaches the caller",
        "Overflow error can be masked: cancel() promise may not resolve",
      ], [
        "AbortController cancel path is missing a test for this change",
        "This change drops the retry; cancel requests are not logged",
        "Request cancellation is not propagated to the unchanged poller",
        "reader.cancel() in the finally block is correct cleanup",
        "readBoundedText could use a comment",
      ]],
      ["d-2887", "D1", [
        "No test covers cleanup failure when removing the failed scratch root; the original normalization error could be replaced",
        "rmSync failure during cleanup would mask the original chown error",
      ], [
        "Consider removing the unused import; it fails lint",
        "cleanup could be simplified",
        // Word boundaries matter: without them "no" matches inside "Annotate".
        "Annotate the test: removeDir helper fails lint",
      ]],
      ["d-3063", "D4", [
        "ids is optional, so a grouped file request can omit ids",
      ], [
        "FindingGroupFileRequest could use a doc comment",
        "send_to_uzi flag on the group request is undocumented",
      ]],
    ];
    for (const [caseId, defectId, positives, negatives] of CASES) {
      const d = byId(caseId).expected.find((e) => e.id === defectId)!;
      // Off the line range, so only the keyword patterns decide.
      const of = (title: string): CodeFinding => ({ id: "F1", severity: "major", path: d.paths[0]!, line: 1, title, detail: "" });
      for (const p of positives) assert.equal(findingMatches(d, of(p)), true, `${defectId} should match: ${p}`);
      for (const n of negatives) assert.equal(findingMatches(d, of(n)), false, `${defectId} should not match: ${n}`);
    }
  });

  it("D1: the cleanup-failure test exists only in the removed hunks", () => {
    const c = byId("d-2887");
    const d = c.expected[0]!;
    assert.equal(d.in_diff, true);
    assert.equal(d.signature.present, false);
    assert.ok(added(c.removedPatch).some((l) => l.includes(d.signature.literal)));
    assert.equal(c.prefixPatch.toString("utf8").includes(d.signature.literal), false);
  });
});

// ---------------------------------------------------------------------------------------------
// Pin verification: rebuild each prefix tree from the checkout's own git objects.

const pinnedShas = (): string[] => [...new Set(corpus.cases.flatMap((c) => [c.base_sha, c.squash_sha]))];

async function objectPresence(): Promise<{ present: string[]; absent: string[] }> {
  const present: string[] = [];
  const absent: string[] = [];
  for (const sha of pinnedShas()) {
    try {
      await replayGit(["-C", CHECKOUT, "cat-file", "-e", `${sha}^{commit}`]);
      present.push(sha);
    } catch { absent.push(sha); }
  }
  return { present, absent };
}

describe("committed replay corpus (pins)", () => {
  let scratch: string;
  let presence: { present: string[]; absent: string[] };
  before(async () => {
    scratch = await fs.mkdtemp(path.join(os.tmpdir(), "replay-corpus-test-"));
    presence = await objectPresence();
  });
  after(async () => { await fs.rm(scratch, { recursive: true, force: true }); });

  it("has all pinned objects, none (shallow checkout, skipped unless required), but never some", (t) => {
    if (presence.absent.length === 0) return;
    if (presence.present.length > 0) assert.fail(`partially present pinned objects: ${presence.absent.length} of ${pinnedShas().length} absent`);
    if (process.env.UZI_REPLAY_CORPUS_REQUIRE_OBJECTS === "1") assert.fail("corpus objects absent (UZI_REPLAY_CORPUS_REQUIRE_OBJECTS=1)");
    t.skip("corpus objects absent (shallow checkout)");
  });

  for (const id of CASE_IDS) {
    it(`${id}: verifies every pin of the prefix tree and its signatures hold`, { timeout: 120_000 }, async (t) => {
      if (presence.present.length === 0 && presence.absent.length > 0 && process.env.UZI_REPLAY_CORPUS_REQUIRE_OBJECTS !== "1") {
        t.skip("corpus objects absent (shallow checkout)");
        return;
      }
      assert.equal(presence.absent.length, 0, "all pinned objects must be present");
      const c = byId(id);
      // Pins are proven and signature files read straight from the pinned tree: no snapshot export.
      const m = await verifyCasePins(CHECKOUT, c, scratch);
      const read = async (rel: string): Promise<string> => (await m.readFile(rel)).toString("utf8");
      try {
        for (const d of c.expected) {
          const text = await read(d.signature.path);
          assert.equal(text.includes(d.signature.literal), d.signature.present, `${id}/${d.id}`);
        }
        const counterpart = COUNTERPART[id];
        if (counterpart) {
          for (const d of byId(counterpart).expected) {
            const text = await read(d.signature.path);
            assert.equal(text.includes(d.signature.literal), !d.signature.present, `${id} must lack ${counterpart}/${d.id}`);
          }
        }
      } finally { await m.cleanup(); }
    });
  }
});
