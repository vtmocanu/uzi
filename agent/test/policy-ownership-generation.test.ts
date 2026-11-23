import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { policyOwnershipGeneration } from "../src/policy-resolve.js";

// policyOwnershipGeneration parses the server's run-ownership reply into three results that its
// callers (worker.ts observePolicyOwnership, runner.ts provePolicyClaim) treat differently:
//   number    - this claim generation currently owns the run (claimed/running, not released);
//   null      - a well-formed reply proving the run is NOT owned (recorded as an observation);
//   undefined - malformed or incomplete, so the conservative protection is kept (no observation).
// Mixing up null and undefined silently changes what the worker records, so every row pins the
// exact value with strictEqual.

const owned = (status: string, claim_generation: number) =>
  ({ status, claim_generation, claim_released: false });

describe("policyOwnershipGeneration tri-state parse", () => {
  it("returns the claim generation for a claimed or running, unreleased run", () => {
    assert.strictEqual(policyOwnershipGeneration(owned("running", 7)), 7);
    assert.strictEqual(policyOwnershipGeneration(owned("claimed", 3)), 3);
  });

  // The runs_status_check domain (api/internal/store/migrations/00206_run_recovery_wait.sql Up,
  // validated by 00207), minus the two owning statuses: every one is a well-formed "not owned".
  for (const status of ["queued", "awaiting_approval", "awaiting_input", "awaiting_followup", "limit_wait",
    "pool_wait", "paused", "recovery_wait", "completed", "failed", "cancelled"]) {
    it(`returns null for the in-domain, non-owning status ${status}`, () => {
      assert.strictEqual(policyOwnershipGeneration(owned(status, 7)), null);
    });
  }

  it("returns null once the claim is released, even while claimed or running", () => {
    assert.strictEqual(policyOwnershipGeneration({ status: "running", claim_generation: 7, claim_released: true }), null);
    assert.strictEqual(policyOwnershipGeneration({ status: "claimed", claim_generation: 3, claim_released: true }), null);
  });

  it("accepts a null-prototype object", () => {
    assert.strictEqual(policyOwnershipGeneration(Object.assign(Object.create(null) as object, owned("running", 7))), 7);
  });

  it("ignores the extra keys a real ownership reply carries", () => {
    // Shapes stubbed in runner-now-summary.test.ts and runner-retained-predecessor-transport.test.ts.
    assert.strictEqual(policyOwnershipGeneration({ ...owned("running", 7), hold_reason: null, inventory_guarded: true }), 7);
  });

  it("accepts the smallest and largest safe generations", () => {
    assert.strictEqual(policyOwnershipGeneration(owned("running", 1)), 1);
    assert.strictEqual(policyOwnershipGeneration(owned("running", Number.MAX_SAFE_INTEGER)), Number.MAX_SAFE_INTEGER);
  });

  it("returns undefined for a null reply instead of throwing", () => {
    assert.strictEqual(policyOwnershipGeneration(null), undefined);
  });

  it("returns undefined for an array, even one without a prototype", () => {
    assert.strictEqual(policyOwnershipGeneration(Object.assign(Object.setPrototypeOf([], null) as object, owned("running", 7))), undefined);
  });

  it("returns undefined for a class instance carrying valid fields", () => {
    class Reply {
      status = "running";
      claim_generation = 7;
      claim_released = false;
    }
    assert.strictEqual(policyOwnershipGeneration(new Reply()), undefined);
  });

  it("returns undefined for a generation that is not a positive safe integer", () => {
    assert.strictEqual(policyOwnershipGeneration(owned("running", 0)), undefined);
    assert.strictEqual(policyOwnershipGeneration(owned("running", 1.5)), undefined);
    assert.strictEqual(policyOwnershipGeneration({ status: "running", claim_generation: "7", claim_released: false }), undefined);
  });

  it("returns undefined when claim_released is not a boolean", () => {
    assert.strictEqual(policyOwnershipGeneration({ status: "running", claim_generation: 7, claim_released: "false" }), undefined);
  });

  it("returns undefined for a status outside the closed domain", () => {
    assert.strictEqual(policyOwnershipGeneration(owned("archived", 7)), undefined);
    assert.strictEqual(policyOwnershipGeneration(owned("RUNNING", 7)), undefined);
  });
});
