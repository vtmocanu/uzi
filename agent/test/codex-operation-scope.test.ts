import { it } from "node:test";
import assert from "node:assert/strict";
import { acquireCodexOperationScope, codexOperationAuthorityIdentity } from "../src/codex-operation-scope.js";
import { AdmissionClock, flushUntil } from "./operation-admission-clock.js";
import type { CodexOperationRequest } from "../src/protocol.js";

it("authority identity is opaque and stable, and isolates replacement scope ownership", async () => {
  const capabilityA = ["authority", "a"].join("-");
  const capabilityB = ["authority", "b"].join("-");
  const identityA = codexOperationAuthorityIdentity(capabilityA);
  const identityB = codexOperationAuthorityIdentity(capabilityB);
  assert.ok(identityA);
  assert.ok(identityB);
  // Boolean assertions keep credential values out of failure diffs.
  assert.ok(identityA === codexOperationAuthorityIdentity(capabilityA));
  assert.ok(identityA !== identityB);
  assert.ok(!identityA.includes(capabilityA) && !identityB.includes(capabilityB));
  assert.match(identityA, /^[a-f0-9]{64}$/);
  const client = {};
  const acquire = (identity: string) =>
    acquireCodexOperationScope(client, "run", 7, {}, undefined, undefined, undefined, identity);
  const ownerA = acquire(identityA);
  const adviceA = acquire(identityA);
  const ownerB = acquire(identityB);
  const adviceB = acquire(identityB);
  let resume!: () => void;
  let entered!: () => void;
  const started = new Promise<void>(resolve => { entered = resolve; });
  const delayed = new Promise<void>(resolve => { resume = resolve; });
  ownerA.bind(async () => () => true);
  const signal = new AbortController().signal;
  const oldPermit = await adviceA.prepare("advice", signal, () => {});
  const callback = ownerA.bind(async () => {
    entered();
    await delayed;
    return () => true;
  });
  ownerB.own();
  try {
    assert.ok(ownerA.controller === adviceA.controller);
    assert.ok(ownerB.controller === adviceB.controller);
    assert.ok(ownerA.controller !== ownerB.controller);
    const pending = adviceA.prepare("advice", signal, () => {});
    const denied = assert.rejects(pending, /closed/);
    await started;
    ownerA.close();
    assert.throws(oldPermit, /closed/);
    resume();
    await denied;
    await assert.rejects(adviceA.prepare("advice", signal, () => {}), /owner closed/);
    await assert.rejects(callback(signal, () => {}), /scope closed/);
    const permitB = await adviceB.prepare("advice", signal, () => {});
    assert.doesNotThrow(() => permitB());
  } finally {
    resume();
    ownerA.close();
    adviceA.close();
    ownerB.close();
    adviceB.close();
  }
});

it("scope records final negotiation loss before a simultaneous source guard throws", async () => {
  const clock = new AdmissionClock();
  let supported = true;
  let epoch = 1;
  let reads = 0;
  let stale = false;
  const client = {
    hasFeature: () => supported,
    get codexOperationNegotiation() { return epoch; },
    checkCodexOperationAdmission: async (_run: string, request: CodexOperationRequest) => {
      reads++;
      return { decision: "admit" as const, operation_id: request.operation_id, claim_generation: request.claim_generation };
    },
  };
  const scope = acquireCodexOperationScope(client, "run", 7, { now: () => clock.now, sleep: clock.sleep });
  scope.bind((signal, check, kind = "root") => scope.controller.admit({
    operation_id: "00000000-0000-4000-8000-000000000001", kind,
  }, signal, check, async () => { check(); }));
  const abort = new AbortController();
  let successor: Promise<() => boolean | void> | undefined;
  try {
    const permit = await scope.prepare("root", abort.signal, () => { if (stale) throw new Error("source replaced"); });
    supported = false;
    stale = true;
    assert.throws(permit, /source replaced/);
    stale = false;
    supported = true;
    epoch++;
    successor = scope.prepare("child", abort.signal, () => {});
    await flushUntil(() => clock.sleepers.size === 1 || reads > 1);
    assert.equal(reads, 1, "replacement cannot bypass unsuccessful admission's shared deadline");
    clock.advance(14_999);
    assert.equal(reads, 1);
    clock.advance(1);
    const next = await successor;
    assert.equal(next(), true);
    assert.equal(reads, 2);
  } finally { abort.abort(); await successor?.catch(() => {}); scope.close(); }
});

it("malformed authority has no identity and cannot reach crypto hashing", () => {
  for (const capability of [undefined, null, 42, {}, ""]) {
    assert.equal(codexOperationAuthorityIdentity(capability), undefined);
  }
});
