import assert from "node:assert/strict";
import { it } from "node:test";
import { decodeCodexOperationDecision } from "../src/codex-operation-admission.js";

it("accepts a real server cooldown shorter than a millisecond without truncating timestamp order", () => {
  const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
  const request = { operation_id: id, kind: "root" as const, claim_generation: 1 };
  const base = { decision: "wait", operation_id: id, claim_generation: 1,
    class: "rate_limit", episode_id: id, server_now: "2026-10-27T12:00:00.123100000Z" };
  for (const eligible_at of ["2026-10-27T12:00:00.123200Z", "2026-10-27T12:00:00.123100001Z"]) {
    const wire = { ...base, eligible_at };
    assert.deepEqual(decodeCodexOperationDecision(wire, request), wire);
  }
  for (const eligible_at of ["2026-10-27T12:00:00.123100000Z", "2026-10-27T12:00:00.123099999Z",
    "2026-10-27T13:00:00.123100000+01:00"]) {
    assert.throws(() => decodeCodexOperationDecision({ ...base, eligible_at }, request));
  }
});
