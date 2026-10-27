import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { buildPolicyRevision, safeRevisionInputId } from "../src/policy-revision.js";

const rowId = "12345678-1234-5678-9abc-123456789abc";
const childId = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const gate = { revision: "7", presentation_id: rowId, payload_digest: "a".repeat(64) };
const human = { kind: "revise", feedback: "same feedback", inputId: 13 } as const;

describe("policy revision producer wire", () => {
  it("distinguishes absent coverage, explicit null and a verified gate", () => {
    assert.equal(buildPolicyRevision(undefined, human), undefined);
    assert.deepEqual(buildPolicyRevision(null, human), {
      gate: null, revision: { source: "run_user_input", input_id: "13" },
    });
    assert.deepEqual(buildPolicyRevision(gate, human), {
      gate, revision: { source: "run_user_input", input_id: "13" },
    });
  });

  it("copies only exact closed gate keys without retaining or mutating the caller", () => {
    const caller = { ...gate };
    const carrier = buildPolicyRevision(caller, human)!;
    assert.notEqual(carrier.gate, caller);
    assert.deepEqual(caller, gate);
    caller.revision = "8";
    assert.equal(carrier.gate?.revision, "7");
    assert.deepEqual(Object.keys(carrier).sort(), ["gate", "revision"]);
    assert.deepEqual(Object.keys(carrier.gate!).sort(), ["payload_digest", "presentation_id", "revision"]);
    assert.deepEqual(Object.keys(carrier.revision).sort(), ["input_id", "source"]);
    for (const bad of [
      {}, [], "gate", { ...gate, extra: null },
      { revision: "7", presentation_id: rowId }, { ...gate, presentation_id: rowId.toUpperCase() },
      { ...gate, presentation_id: rowId + "\n" }, { ...gate, payload_digest: "A".repeat(64) },
      { ...gate, payload_digest: "a".repeat(64) + "\n" },
      { ...gate, payload_digest: "a".repeat(63) },
    ]) assert.equal(buildPolicyRevision(bad, human), undefined);
  });

  it("accepts canonical positive int64 gate boundaries and rejects noncanonical values", () => {
    for (const revision of ["1", "9223372036854775807"]) {
      assert.equal(buildPolicyRevision({ ...gate, revision }, human)?.gate?.revision, revision);
    }
    for (const revision of ["0", "-1", "+1", "01", "1.0", "1e2", " 1", "1\n",
      "9223372036854775808", "99999999999999999999", "", 7, null, undefined]) {
      assert.equal(buildPolicyRevision({ ...gate, revision }, human), undefined);
    }
  });

  it("preserves the human safe maximum and rejects missing, malformed and unsafe numeric identities", () => {
    assert.deepEqual(buildPolicyRevision(null, { ...human, inputId: Number.MAX_SAFE_INTEGER }), {
      gate: null, revision: { source: "run_user_input", input_id: "9007199254740991" },
    });
    assert.equal(safeRevisionInputId(Number.MAX_SAFE_INTEGER), Number.MAX_SAFE_INTEGER);
    assert.equal(buildPolicyRevision(null, { kind: "revise", feedback: "legacy" }), undefined);
    for (const inputId of [undefined, null, "13", 0, -1, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      assert.equal(safeRevisionInputId(inputId), undefined);
      const verdict = Object.assign({ ...human }, { inputId });
      assert.equal(buildPolicyRevision(null, verdict as typeof human), undefined);
    }
  });

  it("uses the exact automatic row UUID rather than the child, round or text and omits legacy IDs", () => {
    const automatic = Object.assign({ kind: "revise" as const, automatic: true as const,
      feedback: "same feedback", round: 13 }, { crossCheckId: rowId, checker_run_id: childId });
    assert.equal(buildPolicyRevision(undefined, automatic), undefined);
    assert.deepEqual(buildPolicyRevision(null, automatic), {
      gate: null, revision: { source: "plan_cross_check", cross_check_id: rowId },
    });
    assert.deepEqual(Object.keys(buildPolicyRevision(gate, automatic)!.revision).sort(), ["cross_check_id", "source"]);
    for (const crossCheckId of [undefined, "", childId.toUpperCase(), "13", rowId + "\n"]) {
      assert.equal(buildPolicyRevision(gate, { ...automatic, crossCheckId }), undefined);
    }
  });
});
