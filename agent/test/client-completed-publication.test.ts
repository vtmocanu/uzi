import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import type { StateRequest } from "../src/protocol.js";
import { makeClaim, nullLogger } from "./helpers.js";

const fixture = JSON.parse(readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
const receipt = fixture.ack.completed_publication_receipt;
const RUN = receipt.run_id;
const FEATURE = "recovery_completed_publication_v1";
let client: WorkerClient;
let features: string[];
let response: Record<string, unknown>;
let claim: Record<string, unknown>;
let holds: Record<string, unknown>[];
let sent: Record<string, unknown>[];

beforeEach(() => {
  features = [FEATURE, "claim_generation_fence", "recovery_inventory_v1"];
  response = structuredClone(fixture.ack);
  claim = { ...makeClaim(), run_id: RUN, claim_generation: 1, inventory_guarded: true,
    issue_iid: 7, repo: { ...makeClaim().repo, id: receipt.repo_id, forge_type: "gitlab" } };
  holds = [{ hold_id: receipt.hold_id, generation: 1, inventory_guarded: true, has_available_capture: false }];
  sent = [];
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const path = new URL(String(input)).pathname;
    if (path.endsWith("/register")) return Response.json({ worker_id: receipt.worker_id, protocol_features: features });
    if (path.endsWith("/claim")) return Response.json(claim);
    if (path.endsWith("/recovery-holds")) return Response.json({ run_id: RUN, holds });
    sent.push(JSON.parse(String(init?.body)));
    return Response.json(response);
  });
  client = new WorkerClient("http://publication.test", "test-worker-token", "test", nullLogger());
});
afterEach(() => mock.restoreAll());
async function report(body: StateRequest = fixture.request) {
  await client.register("worker");
  return client.reportState(RUN, body);
}

it("accepts the literal shared ancestor receipt without inventing archive authority", async () => {
  await client.register("worker");
  await client.claimRun();
  await client.listRecoveryHolds(RUN);
  const ack = await client.reportState(RUN, fixture.request);
  assert.deepEqual(ack.completedPublicationReceipt, receipt);
  assert.notEqual(ack.completedPublicationReceipt?.observed_branch_head, fixture.request.completion_final_head);
  assert.equal("coverage_digest" in ack.completedPublicationReceipt!, false);
  assert.deepEqual(sent[0], fixture.request);
});

for (const [field, value] of [
  ["worker_id", receipt.owner_id], ["run_id", receipt.owner_id], ["generation", 2],
  ["final_head", "b".repeat(40)], ["observed_branch_head", "D".repeat(40)],
  ["hold_id", "bad"], ["owner_id", receipt.owner_id + "\n"], ["connection_id", "unsafe"],
  ["repo_id", receipt.repo_id + "\u202e"], ["project_id", 0], ["mr_iid", null],
  ["forge_type", "unknown"], ["forge_type", { toString: "gitlab" }], ["branch", "agent/issue-7\n"], ["branch", "agent/issue-\u202e7"],
  ["base_url", "https://user:pass@forge.e2e"], ["base_url", "https://forge.e2e/\u2066"],
] as const) {
  it(`rejects malformed or foreign receipt ${field}=${JSON.stringify(value)}`, async () => {
    response.completed_publication_receipt = { ...receipt, [field]: value };
    assert.equal((await report()).completedPublicationReceipt, undefined);
  });
}

for (const field of ["hold_id", "repo_id", "branch", "forge_type"]) {
  it(`rejects mismatched authenticated inventory ${field}`, async () => {
    await client.register("worker");
    await client.claimRun();
    await client.listRecoveryHolds(RUN);
    response.completed_publication_receipt = { ...receipt, [field]:
      field === "branch" ? "agent/issue-8" : field === "forge_type" ? "github" : receipt.owner_id };
    const body = { ...fixture.request };
    if (field === "branch") delete body.branch;
    assert.equal((await client.reportState(RUN, body)).completedPublicationReceipt, undefined);
  });
}

it("retains known hold identity after an empty inventory and rejects conflicting holds", async () => {
  await client.register("worker");
  await client.listRecoveryHolds(RUN);
  holds = [];
  await client.listRecoveryHolds(RUN);
  response.completed_publication_receipt = { ...receipt, hold_id: receipt.owner_id };
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
});

it("strips a persisted original on feature loss without weakening guarded generation", async () => {
  await client.register("worker");
  await client.claimRun();
  client.protocolFeatures = ["claim_generation_fence"];
  const original = Object.freeze({ ...fixture.request });
  const ack = await client.reportState(RUN, original);
  assert.equal(sent[0]?.completion_final_head, undefined);
  assert.equal(sent[0]?.claim_generation, 1);
  assert.equal(original.completion_final_head, receipt.final_head);
  assert.equal(client.knowsInventoryGuardedClaim(RUN, 1), true);
  assert.equal(ack.completedPublicationReceipt, undefined);
  assert.equal(ack.completedPublicationReason, "unsupported_feature");
});

it("old API completion ACK remains safe and ordinary completion still succeeds", async () => {
  features = [];
  response = { run: { status: "completed" } };
  const ack = await report();
  assert.equal(ack.applied, true);
  assert.equal(ack.status, "completed");
  assert.equal(ack.completedPublicationReceipt, undefined);
  assert.equal(sent[0]?.completion_final_head, undefined);
});

it("requires registration and completed request/run identities, and no competing refusal", async () => {
  client.protocolFeatures = features;
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
  await client.register("worker");
  for (const run of [
    { ...fixture.ack.run, id: receipt.owner_id },
    { ...fixture.ack.run, worker_id: receipt.owner_id },
    { ...fixture.ack.run, status: "failed" },
  ]) {
    response = { ...fixture.ack, run };
    assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
  }
  response = structuredClone(fixture.ack);
  assert.equal((await client.reportState(RUN, { ...fixture.request, status: "running" })).completedPublicationReceipt, undefined);
  response.completed_publication_reason = "identity_changed";
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
});

it("projects only bounded refusal reasons", async () => {
  for (const reason of ["not_ancestor", "forge_timeout", "identity_changed", "unknown", "forge_timeout\n", "not_ancestor\u202e"]) {
    response = { run: fixture.ack.run, completed_publication_reason: reason };
    const ack = await report();
    assert.equal(ack.completedPublicationReason, ["not_ancestor", "forge_timeout", "identity_changed"].includes(reason) ? reason : undefined);
  }
});

for (const kind of ["mr_rework", "self_improve"] as const) {
  it(`validates the server branch for ${kind}`, async () => {
    const branch = kind === "mr_rework" ? "review/frozen" : `uzi/self-improve/${RUN}`;
    claim = { ...claim, kind, branch, pipeline: { ref: "poisoned/pipeline-ref" } };
    response.completed_publication_receipt = { ...receipt, branch };
    await client.register("worker");
    await client.claimRun();
    assert.ok((await client.reportState(RUN, { ...fixture.request, branch })).completedPublicationReceipt);
    response.completed_publication_receipt = { ...receipt, branch: "foreign/branch" };
    assert.equal((await client.reportState(RUN, { ...fixture.request, branch: "foreign/branch" })).completedPublicationReceipt, undefined);
  });
}

it("preserves known identity over same-worker featureless registration", async () => {
  await client.register("worker");
  await client.listRecoveryHolds(RUN);
  features = [];
  await client.register("worker");
  client.protocolFeatures = [FEATURE, "claim_generation_fence"];
  response.completed_publication_receipt = { ...receipt, hold_id: receipt.owner_id };
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
});

it("refuses duplicate or unguarded observed holds", async () => {
  await client.register("worker");
  holds.push({ ...holds[0], hold_id: receipt.owner_id });
  await client.listRecoveryHolds(RUN);
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
});

it("refuses known noneligible or unguarded claims", async () => {
  for (const changes of [{ inventory_guarded: false }, { kind: "ci_fix" }]) {
    claim = { ...claim, ...changes };
    await client.register("worker");
    await client.claimRun();
    assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
  }
});
