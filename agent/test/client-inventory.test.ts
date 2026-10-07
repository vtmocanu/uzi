import assert from "node:assert/strict";
import { afterEach, beforeEach, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import type { RecoveryFinalDisposition, RecoveryReserveRequest } from "../src/protocol.js";
import { makeClaim, nullLogger } from "./helpers.js";

const FEATURE = "recovery_inventory_v1";
const RUN = "inventory-run";
const finalDisposition: RecoveryFinalDisposition = {
  kind: "archive", capture_id: "capture-1", coverage_digest: "a".repeat(64),
};
const reserve: RecoveryReserveRequest = {
  run_id: RUN, generation: 7, idempotency_key: "capture-source-1", source_sha: "b".repeat(40),
};
let features: string[] | undefined;
let claim: Record<string, unknown>;
let requests: { path: string; body: Record<string, unknown> }[];
let rejectStampedReport: boolean;
let client: WorkerClient;

beforeEach(() => {
  features = [FEATURE];
  claim = { ...makeClaim(), run_id: RUN, claim_generation: 7 };
  requests = [];
  rejectStampedReport = false;
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const path = new URL(String(input)).pathname;
    const body = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
    requests.push({ path, body });
    if (path.endsWith("/register")) {
      return Response.json({ worker_id: "worker-1", protocol_features: features });
    }
    if (path.endsWith("/runs/claim")) return Response.json(claim);
    if (path.endsWith("/state")) {
      if (rejectStampedReport && body.claim_generation !== undefined) {
        return Response.json({ error: "invalid request body" }, { status: 400 });
      }
      return Response.json({ status: "running" });
    }
    return Response.json({ capture_id: "capture-1", state: "preparing", holds_released: 1 });
  });
  client = new WorkerClient("http://inventory.test", "test-worker-token", "test", nullLogger(), {
    sleep: async () => {},
  });
});
afterEach(() => mock.restoreAll());

async function register(): Promise<void> {
  await client.register("inventory-worker");
}
async function guardedClaim(): Promise<void> {
  claim.inventory_guarded = true;
  await register();
  assert.equal((await client.claimRun())?.inventory_guarded, true);
}
function bodies(suffix: string): Record<string, unknown>[] {
  return requests.filter((r) => r.path.endsWith(suffix)).map((r) => r.body);
}

it("absent feature preserves positive unguarded generation and legacy reserve/release bodies", async () => {
  features = undefined;
  await register();
  const admitted = await client.claimRun();
  assert.equal(admitted?.claim_generation, 7);
  assert.equal(admitted?.inventory_guarded, undefined);
  await client.reserveRecoveryCapture(RUN, reserve);
  await client.releaseRecoveryCustody(RUN, 7);
  assert.deepEqual(bodies("/reserve"), [reserve]);
  assert.deepEqual(bodies("/release"), [{ generation: 7 }]);
});

it("old claims without generation keep unknown identity and empty legacy release", async () => {
  features = undefined;
  delete claim.claim_generation;
  await register();
  assert.equal((await client.claimRun())?.claim_generation, undefined);
  await client.releaseRecoveryCustody(RUN);
  assert.deepEqual(bodies("/release"), [{}]);
});

it("negotiated feature alone does not guard missing/false assertions; true admits the exact generation", async () => {
  await register();
  for (const guard of [undefined, false]) {
    if (guard === undefined) delete claim.inventory_guarded;
    else claim.inventory_guarded = guard;
    assert.equal((await client.claimRun())?.inventory_guarded, guard);
    await client.releaseRecoveryCustody(RUN, 7);
  }
  claim.inventory_guarded = true;
  assert.equal((await client.claimRun())?.inventory_guarded, true);
  await assert.rejects(client.releaseRecoveryCustody(RUN, 7), /legacy release refused/);
  assert.equal(bodies("/release").length, 2);
});

it("malformed guard assertions fail admission without protecting a rejected generation", async () => {
  await register();
  for (const guard of ["true", null, {}]) {
    claim.inventory_guarded = guard;
    await assert.rejects(client.claimRun(), /malformed or contradictory/);
  }
  await client.releaseRecoveryCustody(RUN, 7);
  assert.equal(bodies("/release").length, 1);
});

it("true guard requires negotiated feature and a positive safe integer generation", async () => {
  claim.inventory_guarded = true;
  features = [];
  await register();
  await assert.rejects(client.claimRun(), /malformed or contradictory/);
  features = [FEATURE];
  await register();
  for (const generation of [undefined, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, "7", null]) {
    claim.claim_generation = generation;
    await assert.rejects(client.claimRun(), /malformed or contradictory/);
  }
  claim.claim_generation = 7;
  assert.equal((await client.claimRun())?.claim_generation, 7);
});

it("guarded custody survives clear and featureless registration; restoration sends the same final payload", async () => {
  await guardedClaim();
  for (const loss of ["clear", "register"]) {
    if (loss === "clear") client.clearFeatures();
    else {
      features = [];
      await register();
    }
    const before = requests.length;
    await assert.rejects(client.releaseRecoveryCustody(RUN, 7), /legacy release refused/);
    await assert.rejects(client.releaseRecoveryCustody(RUN, 7, "publication", finalDisposition), /feature unavailable/);
    assert.equal(requests.length, before, "blocked releases perform zero HTTP calls");
  }
  features = [FEATURE];
  await register();
  await client.releaseRecoveryCustody(RUN, 7, "publication", finalDisposition);
  assert.deepEqual(bodies("/release"), [{
    generation: 7, release_evidence: "publication", final_disposition: finalDisposition,
  }]);
});

it("real state strict-decode fallback clears negotiation while retaining guarded release protection", async () => {
  features = [FEATURE, "claim_generation_fence"];
  await guardedClaim();
  rejectStampedReport = true;
  await client.reportState(RUN, { status: "running", claim_generation: 7 });
  assert.deepEqual(bodies("/state"), [
    { status: "running", claim_generation: 7 }, { status: "running" },
  ]);
  assert.equal(client.hasFeature(FEATURE), false);
  const before = requests.length;
  await assert.rejects(client.releaseRecoveryCustody(RUN, 7), /legacy release refused/);
  await assert.rejects(client.releaseRecoveryCustody(RUN, 7, undefined, finalDisposition), /feature unavailable/);
  assert.equal(requests.length, before);
});

it("guard decisions are generation distinct and the same guarded generation cannot downgrade", async () => {
  await guardedClaim();
  claim.claim_generation = 8;
  delete claim.inventory_guarded;
  await client.claimRun();
  await client.releaseRecoveryCustody(RUN, 8);
  claim.claim_generation = 7;
  for (const guard of [undefined, false]) {
    if (guard === undefined) delete claim.inventory_guarded;
    else claim.inventory_guarded = guard;
    await assert.rejects(client.claimRun(), /cannot downgrade/);
  }
  await assert.rejects(client.releaseRecoveryCustody(RUN, 7), /legacy release refused/);
  assert.deepEqual(bodies("/release"), [{ generation: 8 }]);
});

it("coverage-bound reserve retains its digest until negotiation returns; legacy reserve still works", async () => {
  await guardedClaim();
  const bound = { ...reserve, coverage_digest: finalDisposition.coverage_digest };
  client.clearFeatures();
  const before = requests.length;
  await assert.rejects(client.reserveRecoveryCapture(RUN, bound), /inventory feature unavailable/);
  assert.equal(requests.length, before);
  await client.reserveRecoveryCapture(RUN, reserve);
  await register();
  await client.reserveRecoveryCapture(RUN, bound);
  assert.deepEqual(bodies("/reserve"), [reserve, bound]);
});

it("final inventory release requires an exact positive safe generation before HTTP", async () => {
  await register();
  const before = requests.length;
  for (const generation of [undefined, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
    await assert.rejects(
      client.releaseRecoveryCustody(RUN, generation, undefined, finalDisposition),
      /exact positive safe generation/,
    );
  }
  assert.equal(requests.length, before);
});

it("issue1924 a settled empty guarded generation stops reading as pending but keeps its release guards", async () => {
  await guardedClaim();
  assert.equal(client.knowsInventoryGuardedClaim(RUN, 7), true);
  client.markInventoryGuardedClaimSettled(RUN, 8); // never claimed: no effect
  client.markInventoryGuardedClaimSettled("other-run", 7);
  assert.equal(client.knowsInventoryGuardedClaim(RUN, 7), true);
  client.markInventoryGuardedClaimSettled(RUN, 7);
  assert.equal(client.knowsInventoryGuardedClaim(RUN, 7), false);
  await assert.rejects(client.releaseRecoveryCustody(RUN, 7), /legacy release refused/);
});
