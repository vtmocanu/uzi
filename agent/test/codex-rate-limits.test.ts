import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { CodexRateLimitObserver } from "../src/codex/rate-limits.js";
import { classifyLimitEvidence } from "../src/limit.js";

const reset = 2_000_000_000;
const w = (usedPercent = 100, windowDurationMins = 300, resetsAt: unknown = reset) =>
  ({ usedPercent, windowDurationMins, resetsAt });
const evidence = (updates: unknown[], auth: "subscription" | "api_key" | undefined = "subscription") => {
  const observer = new CodexRateLimitObserver();
  for (const update of updates) observer.observe(update);
  return observer.classify(auth);
};
const rejected = (resetsAtMs: number | undefined, window = "five_hour") =>
  ({ explicitExhaustion: true, latest: { status: "rejected", resetsAtMs, window } });

describe("Codex rate-limit evidence #2360", () => {
  it("requires explicit subscription, latest snapshot, spend and reached-type permission", () => {
    const observer = new CodexRateLimitObserver();
    assert.equal(observer.classify("subscription"), undefined);
    observer.observe({ primary: w() });
    assert.equal(observer.classify(undefined), undefined);
    assert.equal(observer.classify("api_key"), undefined);
    for (const type of ["workspace_owner_credits_depleted", "workspace_member_credits_depleted",
      "workspace_owner_usage_limit_reached", "workspace_member_usage_limit_reached", "private-unrecognized", false]) {
      assert.equal(evidence([{ primary: w(), rateLimitReachedType: type }]), undefined);
    }
    assert.equal(evidence([{ primary: w(), spendControlReached: true }]), undefined);
    for (const spend of [false, null, "true", 1]) {
      assert.deepEqual(evidence([{ primary: w(), spendControlReached: spend }]), rejected(reset * 1000));
    }
    assert.equal(evidence([{ primary: w(99) }]), undefined);
  });

  it("replaces windows, reached type and spend including null clears", () => {
    assert.equal(evidence([{ primary: w() }, { primary: null, secondary: null }]), undefined);
    assert.equal(evidence([{ rateLimitReachedType: "rate_limit_reached" }, { rateLimitReachedType: null }]), undefined);
    assert.deepEqual(evidence([{ primary: w(), spendControlReached: true }, { primary: w(), spendControlReached: null }]), rejected(reset * 1000));
    assert.deepEqual(evidence([{ primary: w(), rateLimitReachedType: "workspace_owner_credits_depleted" },
      { primary: w(), rateLimitReachedType: null }]), rejected(reset * 1000));
  });

  it("selects the last bucket, defaults absent/null IDs, poisons invalid and fifth last updates", () => {
    assert.equal(evidence([{ limitId: "a", primary: w() }, { limitId: "b", primary: w(1) }]), undefined);
    assert.deepEqual(evidence([{ limitId: "a", primary: w() }, { limitId: "b", primary: w(1) },
      { limitId: "a", secondary: w(100, 10080) }]), rejected(reset * 1000, "seven_day"));
    for (const id of ["", "x".repeat(65), "bad id", "bad\n", 3, {}, false]) {
      assert.equal(evidence([{ primary: w() }, { limitId: id, primary: w() }]), undefined);
    }
    for (const id of [null, undefined, "a".repeat(64), "A-z_0.:-"]) {
      assert.deepEqual(evidence([{ limitId: id, primary: w() }]), rejected(reset * 1000));
    }
    const four = ["a", "b", "c", "d"].map(limitId => ({ limitId, primary: w() }));
    assert.equal(evidence([...four, { limitId: "e", primary: w() }]), undefined);
    assert.deepEqual(evidence([...four, { limitId: "e" }, { limitId: "a", primary: w() }]), rejected(reset * 1000));
    assert.equal(evidence([{ primary: w() }, { limitId: null, primary: null }]), undefined);
    assert.equal(evidence([{ primary: w() }, null]), undefined);
  });

  it("handles rounding, ties, latest exhausted reset, partial and missing resets", () => {
    assert.deepEqual(evidence([{ primary: w(99), secondary: w(60, 10080, reset + 1000), rateLimitReachedType: "rate_limit_reached" }]), rejected(reset * 1000));
    assert.deepEqual(evidence([{ primary: w(99), secondary: w(99, 10080, reset + 1000), rateLimitReachedType: "rate_limit_reached" }]), rejected((reset + 1000) * 1000, "seven_day"));
    assert.deepEqual(evidence([{ primary: w(), secondary: w(101, 10080, reset + 1000) }]), rejected((reset + 1000) * 1000, "seven_day"));
    assert.deepEqual(evidence([{ primary: w(100, 300, null), secondary: w(100, 10080) }]), rejected(undefined, "seven_day"));
    assert.deepEqual(evidence([{ primary: w(100, 300, null), secondary: w(100, 10080, null) }]), rejected(undefined, "unknown"));
    assert.deepEqual(evidence([{ rateLimitReachedType: "rate_limit_reached" }]), rejected(undefined, "unknown"));
  });

  it("bounds every integer and converts seconds once", () => {
    for (const usedPercent of [-1, 99.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      assert.equal(evidence([{ primary: { ...w(), usedPercent } }]), undefined);
    }
    assert.deepEqual(evidence([{ primary: w(Number.MAX_SAFE_INTEGER) }]), rejected(reset * 1000));
    for (const duration of [0, 44641, 300.5, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      assert.deepEqual(evidence([{ primary: w(100, duration) }]), rejected(reset * 1000, "unknown"));
    }
    for (const duration of [1, 44640]) {
      assert.deepEqual(evidence([{ primary: w(100, duration) }]), rejected(reset * 1000, "unknown"));
    }
    for (const resetsAt of [1e9 - 1, 4.1e9 + 1, reset + .5, Infinity, Number.MAX_SAFE_INTEGER + 1, null]) {
      assert.deepEqual(evidence([{ primary: w(100, 300, resetsAt) }]), rejected(undefined, "unknown"));
    }
    for (const resetsAt of [1e9, 4.1e9]) {
      assert.deepEqual(evidence([{ primary: w(100, 300, resetsAt) }]), rejected(resetsAt * 1000));
    }
  });

  it("shared classifier discards past resets while retaining explicit exhaustion", () => {
    const ev = evidence([{ primary: w() }])!;
    assert.deepEqual(classifyLimitEvidence(ev, reset * 1000), { resetsAtMs: undefined, window: "five_hour" });
    assert.deepEqual(classifyLimitEvidence(ev, reset * 1000 - 1), { resetsAtMs: reset * 1000, window: "five_hour" });
  });
});
