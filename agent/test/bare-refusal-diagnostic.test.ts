import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";
import { BARE_DELETE_OPS, classifyBareFootprint } from "../src/bare-refusal-diagnostic.js";

// issue #4066 M4: the footprint classifier behind the `bare_slot_refusal` diagnostic.

const SHA = "0123456789abcdef0123456789abcdef01234567";
const FP = "0123456789abcdef".repeat(4);
const unparsed = { deletionClass: "unparsed", subjectRun: "unknown", generation: "unknown" };

describe("classifyBareFootprint", () => {
  it("accepts a recovery pin for both pin ops and the cleanup op", () => {
    for (const op of ["recovery-pin-delete-ref", "recovery-pin-archive-delete-ref", "recovery-cleanup-delete-ref"]) {
      assert.deepEqual(classifyBareFootprint(`${op}: refs/uzi-recovery-pin/run_1-a/7`),
        { deletionClass: op, subjectRun: "run_1-a", generation: 7 });
    }
  });

  it("accepts a coverage ref only for the cleanup op", () => {
    const ref = `refs/uzi-coverage/run1/3/${FP}`;
    assert.deepEqual(classifyBareFootprint(`recovery-cleanup-delete-ref: ${ref}`),
      { deletionClass: "recovery-cleanup-delete-ref", subjectRun: "run1", generation: 3 });
    for (const op of ["recovery-pin-delete-ref", "recovery-pin-archive-delete-ref", "owed-reconcile-delete-ref"]) {
      assert.deepEqual(classifyBareFootprint(`${op}: ${ref}`), unparsed);
    }
  });

  it("accepts an owed ref for the cleanup and reconcile ops, with no generation", () => {
    const ref = `refs/uzi-owed/run1/${SHA}`;
    for (const op of ["recovery-cleanup-delete-ref", "owed-reconcile-delete-ref"]) {
      assert.deepEqual(classifyBareFootprint(`${op}: ${ref}`), { deletionClass: op, subjectRun: "run1", generation: "unknown" });
    }
    for (const op of ["recovery-pin-delete-ref", "recovery-pin-archive-delete-ref"]) {
      assert.deepEqual(classifyBareFootprint(`${op}: ${ref}`), unparsed);
    }
  });

  it("rejects a pin ref for the reconcile op", () => {
    assert.deepEqual(classifyBareFootprint("owed-reconcile-delete-ref: refs/uzi-recovery-pin/run1/1"), unparsed);
  });

  it("rejects malformed generations, run ids, shas and shapes", () => {
    const op = "recovery-cleanup-delete-ref";
    const bad = [
      `${op}: refs/uzi-recovery-pin/run1/-1`,
      `${op}: refs/uzi-recovery-pin/run1/1.5`,
      `${op}: refs/uzi-recovery-pin/run1/`,
      `${op}: refs/uzi-recovery-pin/run1/99999999999999999999`,
      `${op}: refs/uzi-recovery-pin/run1/9007199254740993`,
      `${op}: refs/uzi-recovery-pin//1`,
      `${op}: refs/uzi-recovery-pin/run.1/1`,
      `${op}: refs/uzi-recovery-pin/${"a".repeat(129)}/1`,
      `${op}: refs/uzi-coverage/run1/1/a/b`,
      `${op}: refs/uzi-coverage/run1/x/${FP}`,
      `${op}: refs/uzi-coverage/run1/1/abc123def`,
      `${op}: refs/uzi-coverage/run1/1/${FP.toUpperCase()}`,
      `${op}: refs/uzi-coverage/run1/1/${FP.slice(1)}`,
      `${op}: refs/uzi-coverage/run1/1/${FP}0`,
      `${op}: refs/uzi-owed/run1/${SHA.slice(1)}`,
      `${op}: refs/uzi-owed/run1/${SHA.toUpperCase()}`,
      `${op}: refs/heads/main`,
      `${op}: refs/uzi-recovery-pin/run1/1, refs/uzi-recovery-pin/run1/2`,
      `${op}: refs/uzi-recovery-pin/run1/1\n`,
      `${op}:refs/uzi-recovery-pin/run1/1`,
      `unknown-op: refs/uzi-recovery-pin/run1/1`,
      `: refs/uzi-recovery-pin/run1/1`,
      "",
    ];
    for (const text of bad) assert.deepEqual(classifyBareFootprint(text), unparsed, JSON.stringify(text));
  });

  it("treats an oversized footprint digest and an oversized text as unparsed", () => {
    assert.deepEqual(classifyBareFootprint(`sha256:${"a".repeat(64)}`), unparsed);
    assert.deepEqual(classifyBareFootprint(`recovery-pin-delete-ref: refs/uzi-recovery-pin/run1/1${"x".repeat(4096)}`), unparsed);
  });

  it("matches the set of ops git.ts passes to beginBare", () => {
    const source = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src", "git.ts"), "utf8");
    const found = new Set<string>();
    for (const match of source.matchAll(/\bbeginBare\(\s*[^,()]+,\s*"([^"]+)"/g)) found.add(match[1]!);
    assert.ok(found.size > 0, "the source scan found call sites");
    assert.deepEqual([...found].sort(), [...BARE_DELETE_OPS].sort());
  });
});
