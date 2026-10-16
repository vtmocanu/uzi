import { it } from "node:test";
import assert from "node:assert/strict";
import { client, installHarness } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";

installHarness();

async function policy(f: Awaited<ReturnType<typeof reportRetirementFixture>>, source = 2, head = f.head) {
  const written = await f.outbox.journalPolicy(f.claim.run_id, {
    outcome_id: "11111111-1111-4111-8111-111111111111", source_generation: source,
    detail: "refused", tag: "cyberPolicy", phase: "implementation",
    correlation_id: "pr-" + "a".repeat(32) + "-1", session_id: "session", head, published: false,
  });
  assert.equal(written.journaled, true);
  if (!written.journaled) throw new Error("policy not journaled");
  const bound = await f.outbox.bindPolicyDelivery(f.claim.run_id, source, 2, written.context.expectedIdentity!);
  assert.equal(bound.bound, true);
  if (!bound.bound) throw new Error("policy not bound");
  await f.outbox.markPolicyServerSettled(f.claim.run_id, source, bound.context);
  f.policyCustody.generations.length = 0;
  return bound.context;
}

it("policy production proof retires exactly the ACKed report with closed custody and covering archive", async t => {
  const f = await reportRetirementFixture(t, { policyCustody: true });
  const context = await policy(f);
  const restore = f.useHeadroom();
  try {
    assert.equal(await f.r.retirePolicyReport(f.claim.run_id, 2, context), process.platform === "linux");
    assert.deepEqual(f.policyCustody.generations, process.platform === "linux" ? [2] : []);
    assert.equal(f.outbox.listPendingPolicies(f.claim.run_id).length, process.platform === "linux" ? 0 : 1);
    await f.assertPending(); // Ordinary report, MAC journals, source clones and pins all remain.
  } finally { restore(); }
});

it("degraded evidence with no preserved head can retire after a complete later source proof", async t => {
  const f = await reportRetirementFixture(t, { policyCustody: true });
  const context = await policy(f, 2, "");
  const restore = f.useHeadroom();
  try {
    assert.equal(await f.r.retirePolicyReport(f.claim.run_id, 2, context), process.platform === "linux");
    await f.assertPending();
  } finally { restore(); }
});

it("an explicit legacy current generation can use an earlier covering guarded archive", async t => {
  const f = await reportRetirementFixture(t, { policyCustody: true, archiveGeneration: 1, policyLegacyCurrent: true });
  const context = await policy(f, 1);
  const restore = f.useHeadroom();
  try {
    assert.equal(await f.r.retirePolicyReport(f.claim.run_id, 1, context), process.platform === "linux");
    if (process.platform === "linux") assert.deepEqual(f.policyCustody.generations, [1]);
    await f.assertPending();
  } finally { restore(); }
});

it("legacy classification cannot erase authenticated guarded-current evidence", async t => {
  const f = await reportRetirementFixture(t, { policyCustody: true });
  t.mock.method(client, "getRunOwnership", async () => ({ status: "cancelled", claim_generation: 2, inventory_guarded: false }));
  const context = await policy(f);
  const restore = f.useHeadroom();
  try {
    assert.equal(await f.r.retirePolicyReport(f.claim.run_id, 2, context), false);
    assert.deepEqual(f.policyCustody.generations, []);
    await f.assertPending();
  } finally { restore(); }
});

it("old source ACK cannot retire policy while current delivery custody is open", async t => {
  const f = await reportRetirementFixture(t, { policyCustody: true, olderOpenHold: true });
  const context = await policy(f, 1);
  f.policyCustody.openGeneration = 2;
  const restore = f.useHeadroom();
  try {
    assert.equal(await f.r.retirePolicyReport(f.claim.run_id, 1, context), false);
    if (process.platform === "linux") assert.deepEqual(f.policyCustody.generations, [1, 2]);
    assert.equal(f.outbox.listPendingPolicies(f.claim.run_id).length, 1);
    await f.assertPending();
  } finally { restore(); }
});

it("immutable refusal head outside the archive retains policy before custody transport", async t => {
  const f = await reportRetirementFixture(t, { policyCustody: true });
  // An actual later commit exists locally but was never included in the frozen archive.
  const uncovered = f.commit("after-archive.txt");
  const context = await policy(f, 2, uncovered);
  const restore = f.useHeadroom();
  try {
    assert.equal(await f.r.retirePolicyReport(f.claim.run_id, 2, context), false);
    assert.deepEqual(f.policyCustody.generations, []);
    assert.equal(f.outbox.listPendingPolicies(f.claim.run_id).length, 1);
    await f.assertPending();
  } finally { restore(); }
});

it("sparse actual generations require only their three custody checks", async t => {
  const f = await reportRetirementFixture(t, { policyCustody: true });
  const old = await policy(f, 1);
  const rebound = await f.outbox.bindPolicyDelivery(f.claim.run_id, 1, 1_000_000, old.expectedIdentity!);
  assert.equal(rebound.bound, true);
  if (!rebound.bound) throw new Error("delivery not rebound");
  await f.outbox.markPolicyServerSettled(f.claim.run_id, 1, rebound.context);
  t.mock.method(client, "getRunOwnership", async () => ({ status: "cancelled", claim_generation: 1_000_000, inventory_guarded: true }));
  const restore = f.useHeadroom();
  try {
    assert.equal(await f.r.retirePolicyReport(f.claim.run_id, 1, rebound.context), process.platform === "linux");
    assert.deepEqual(f.policyCustody.generations, process.platform === "linux" ? [1, 2, 1_000_000] : []);
    await f.assertPending();
  } finally { restore(); }
});

for (const obstruction of ["unknown custody", "live flight", "expired deadline", "cancelled proof"] as const) {
  it(`policy report retains all sources with ${obstruction}`, async t => {
    const f = await reportRetirementFixture(t, { policyCustody: true, liveFlight: obstruction === "live flight" });
    const context = await policy(f);
    f.policyCustody.unknown = obstruction === "unknown custody";
    const restore = f.useHeadroom();
    try {
      assert.equal(await f.r.retirePolicyReport(f.claim.run_id, 2, { ...context,
        ...(obstruction === "expired deadline" ? { deadline: Date.now() - 1 } : {}),
        ...(obstruction === "cancelled proof" ? { signal: AbortSignal.abort() } : {}),
      }), false);
      assert.equal(f.outbox.listPendingPolicies(f.claim.run_id).length, 1);
      await f.assertPending();
    } finally { restore(); }
  });
}

for (const change of ["delivery rebound", "worker re-registration"] as const) {
  it(`fresh custody cannot unlink after ${change}`, async t => {
    const f = await reportRetirementFixture(t, { policyCustody: true });
    const context = await policy(f);
    const get = client.getTerminalRejectionCustody.bind(client);
    let requests = 0;
    t.mock.method(client, "getTerminalRejectionCustody", async (...args: Parameters<typeof get>) => {
      const result = await get(...args);
      requests++;
      if (change === "delivery rebound") {
        const rebound = await f.outbox.bindPolicyDelivery(f.claim.run_id, 2, 3, context.expectedIdentity!);
        assert.equal(rebound.bound, true);
      } else {
        await client.register("test");
      }
      return result;
    });
    const restore = f.useHeadroom();
    try {
      assert.equal(await f.r.retirePolicyReport(f.claim.run_id, 2, context), false);
      if (process.platform === "linux") assert.equal(requests, 1, "the registration/delivery changed during actual typed custody HTTP");
      assert.equal(f.outbox.listPendingPolicies(f.claim.run_id).length, 1);
      await f.assertPending();
    } finally { restore(); }
  });
}
