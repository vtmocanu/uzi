import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { admitPolicyTurn, validatePolicyRefusal, ProviderPolicyRefusal } from "../src/provider-policy-refusal.js";

describe("policy refusal closed metadata (#2321)", () => {
  const root = () => ({ ...admitPolicyTurn("plan"), event: "provider_policy_refusal" as const,
    provider: "codex" as const, category: "policy_refusal" as const, policy_tag: "cyberPolicy" as const, origin: "root" as const });
  it("mints unique opaque bounded correlations with immutable phase", () => {
    const ids = new Set(Array.from({ length: 100 }, () => root().correlation_id));
    assert.equal(ids.size, 100);
    assert.ok([...ids].every(id => /^pr-[0-9a-f]{32}-[1-9][0-9]*$/.test(id) && id.length < 60));
    const p = validatePolicyRefusal(root())!;
    assert.ok(Object.isFrozen(p));
    assert.throws(() => { (p as { phase: string }).phase = "implement"; }, TypeError);
  });
  it("rejects malformed/oversized IDs, fields and shapes rather than truncating", () => {
    const p = root();
    for (const bad of ["", "provider-secret-id", p.correlation_id + "x", p.correlation_id + "\n", p.correlation_id + "\r", "x".repeat(10000),
      p.correlation_id.replace(/-[0-9]+$/, "-0"), p.correlation_id.replace(/-[0-9]+$/, "-9007199254740992")]) {
      assert.equal(validatePolicyRefusal({ ...p, correlation_id: bad }), undefined);
    }
    for (const extra of [{ message: "SECRET" }, { role: "coder" }, { parent_correlation_id: p.correlation_id }]) {
      assert.equal(validatePolicyRefusal({ ...p, ...extra }), undefined);
    }
    assert.equal(validatePolicyRefusal({ ...p, policy_tag: { cyberPolicy: {} } }), undefined);
    assert.equal(validatePolicyRefusal({ ...p, provider: "SECRET" }), undefined);
    assert.equal(validatePolicyRefusal({ ...p, category: "unknown" }), undefined);
    assert.equal(validatePolicyRefusal(Object.assign(Object.create(p), { extra: "SECRET" })), undefined);
    const child = { ...p, ...admitPolicyTurn("implement"), origin: "child", role: "coder", parent_correlation_id: p.correlation_id };
    assert.ok(validatePolicyRefusal(child));
    assert.equal(validatePolicyRefusal({ ...p, phase: "plan" }), undefined);
    for (const key of ["role", "parent_correlation_id"] as const) {
      const inherited = Object.assign(Object.create({ [key]: child[key] }), child);
      delete inherited[key];
      inherited.extra = "replacement own key";
      assert.equal(validatePolicyRefusal(inherited), undefined);
    }
    const bounded = validatePolicyRefusal({ ...child, role: "x".repeat(64),
      correlation_id: "pr-" + "f".repeat(32) + "-9007199254740991",
      parent_correlation_id: "pr-" + "e".repeat(32) + "-9007199254740991" })!;
    assert.ok(bounded);
    assert.ok(Buffer.byteLength(JSON.stringify(bounded)) <= 400);
    const rootBounded = validatePolicyRefusal({ ...p, correlation_id: bounded.correlation_id })!;
    assert.ok(Buffer.byteLength(JSON.stringify(rootBounded)) <= 256);
    for (const suffix of ["\n", "\r"]) {
      assert.equal(validatePolicyRefusal({ ...child, parent_correlation_id: p.correlation_id + suffix }), undefined);
    }
    for (const role of ["", "x".repeat(65), "coder\n", "coder\r", "cödér", "coder\nSECRET", "coder SECRET"]) {
      assert.equal(validatePolicyRefusal({ ...child, role }), undefined);
    }
    assert.equal(validatePolicyRefusal({ ...child, parent_correlation_id: child.correlation_id }), undefined);
  });
  it("typed exception carries only the closed fixed message and metadata", () => {
    const p = root();
    const err = new ProviderPolicyRefusal(p);
    assert.equal(err.message, "Codex provider safety-policy refusal (cyberPolicy)");
    assert.deepEqual(err.policyRefusal, p);
    assert.ok(!JSON.stringify(err).includes("message"));
  });
});
