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
let fetchedPaths: string[];
let workerId: string;
let registrationFails: boolean;
let holdsResponse: (() => Response) | undefined;

beforeEach(() => {
  features = [FEATURE, "claim_generation_fence", "recovery_inventory_v1"];
  response = structuredClone(fixture.ack);
  claim = { ...makeClaim(), run_id: RUN, claim_generation: 1, inventory_guarded: true,
    issue_iid: 7, repo: { ...makeClaim().repo, id: receipt.repo_id, forge_type: "gitlab" } };
  holds = [{ hold_id: receipt.hold_id, generation: 1, inventory_guarded: true, has_available_capture: false }];
  sent = [];
  fetchedPaths = [];
  workerId = receipt.worker_id;
  registrationFails = false;
  holdsResponse = undefined;
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const path = new URL(String(input)).pathname;
    fetchedPaths.push(path);
    if (path.endsWith("/register")) {
      if (registrationFails) return new Response("unauthorized", { status: 401 });
      return Response.json({ worker_id: workerId, protocol_features: features });
    }
    if (path.endsWith("/claim")) return Response.json(claim);
    if (path.endsWith("/recovery-holds")) return holdsResponse?.() ?? Response.json({ run_id: RUN, holds });
    sent.push(JSON.parse(String(init?.body)));
    return Response.json(response);
  });
  client = new WorkerClient("http://publication.test", "test-worker-token", "test", nullLogger(), {
    sleep: async () => {}, terminalRetrySchedule: [0, 0],
  });
});
afterEach(() => mock.restoreAll());
for (const lane of ["claim", "holds"] as const) {
  it("Unit 1: nonreceipt retirement excludes delayed " + lane + " observations", async () => {
    await client.register("worker");
    await client.claimRun();
    await client.listRecoveryHolds(RUN);
    const delayed = await delayedObservation(lane);
    client.releaseRetiredPublicationCompletion(RUN, 1, client.capturePublicationCompletionRetirement());
    delayed.finish(Response.json(lane === "claim" ? claim : { run_id: RUN,
      holds: [{ ...holds[0], hold_id: receipt.owner_id }] }));
    await delayed.settled;
    assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt?.hold_id, receipt.hold_id);
    assert.equal((Reflect.get(client, "completionClaims") as Map<string, unknown>).has(RUN + ":1"), false);
    assert.equal((Reflect.get(client, "completionHolds") as Map<string, unknown>).has(RUN + ":1"), false);
  });
}

for (const conflict of ["claim", "hold"] as const) {
  it("Unit 1: nonreceipt notification retains " + conflict + " conflict", async () => {
    await client.register("worker");
    await client.claimRun();
    await client.listRecoveryHolds(RUN);
    if (conflict === "claim") {
      claim = { ...claim, repo: { ...(claim.repo as object), id: receipt.owner_id } };
      await client.claimRun();
    } else {
      holds = [{ ...holds[0], hold_id: receipt.owner_id }];
      await client.listRecoveryHolds(RUN);
    }
    client.releaseRetiredPublicationCompletion(RUN, 1, client.capturePublicationCompletionRetirement());
    assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
  });
}

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

for (const lostACK of [false, true]) {
  it(`hold-only original replay survives unrelated ineligible claim: ${lostACK ? "lost ACK" : "accepted ACK"}`, async t => {
    const original = Object.freeze(structuredClone(fixture.request)) as StateRequest;
    const snapshot = structuredClone(original);
    await client.register("worker");
    await client.listRecoveryHolds(RUN);
    let losing = lostACK;
    let storedACK: Record<string, unknown> | undefined;
    const originalFetch = globalThis.fetch;
    t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      const res = await originalFetch(input, init);
      if (!new URL(String(input)).pathname.endsWith("/state")) return res;
      // The server persists the original receipt before every initial transport attempt fails.
      storedACK ??= await res.json() as Record<string, unknown>;
      if (losing) throw new Error("stored completion ACK lost");
      return Response.json(storedACK);
    });
    if (lostACK) await assert.rejects(client.reportState(RUN, original), /stored completion ACK lost/);
    else assert.deepEqual((await client.reportState(RUN, original)).completedPublicationReceipt, receipt);
    assert.deepEqual(storedACK, fixture.ack);
    const initialAttempts = sent.length;
    assert.equal(initialAttempts, lostACK ? 3 : 1, "lost ACK exhausts the immediate retry schedule");
    losing = false;
    claim = { ...claim, run_id: receipt.owner_id, claim_generation: 2, kind: "chat" };
    await client.claimRun();
    assert.deepEqual((await client.reportState(RUN, original)).completedPublicationReceipt, receipt);
    assert.equal(client.canStartPublicationCompletion(RUN, 1), true);
    await client.register("same-worker");
    assert.deepEqual((await client.reportState(RUN, original)).completedPublicationReceipt, receipt);
    assert.equal(client.canStartPublicationCompletion(RUN, 1), true);
    assert.deepEqual(sent, Array(initialAttempts + 2).fill(snapshot), "every attempt sends the immutable original");
    assert.deepEqual(original, snapshot);
    assert.equal(fetchedPaths.filter(path => path.endsWith("/claim")).length, 1, "only unrelated B is claimed");
    assert.equal(claim.run_id, receipt.owner_id, "A was never claimed");
    assert.equal(fetchedPaths.filter(path => path.endsWith("/recovery-holds")).length, 1, "A hold is never refetched");
  });
}

it("exact ineligible B refuses hold, eligible retry, admission and receipt until changed-worker registration", async () => {
  const runB = receipt.owner_id;
  const bodyB = { ...fixture.request, claim_generation: 2 };
  await client.register("worker");
  claim = { ...claim, run_id: runB, claim_generation: 2, kind: "chat" };
  await client.claimRun();
  holdsResponse = () => Response.json({ run_id: runB, holds: [{ ...holds[0], generation: 2 }] });
  const refuseB = async () => {
    await client.listRecoveryHolds(runB);
    claim = { ...claim, kind: "issue" };
    await client.claimRun();
    assert.equal(client.canStartPublicationCompletion(runB, 2), false);
    response = { ...fixture.ack, run: { ...fixture.ack.run, id: runB, worker_id: workerId },
      completed_publication_receipt: { ...receipt, run_id: runB, generation: 2, worker_id: workerId } };
    assert.equal((await client.reportState(runB, bodyB)).completedPublicationReceipt, undefined);
    assert.deepEqual(sent.at(-1), bodyB);
  };
  await client.register("same-worker");
  await refuseB();
  registrationFails = true;
  await assert.rejects(client.register("failed-worker"));
  await refuseB();
  registrationFails = false;
  await client.register("same-worker-again");
  await refuseB();
  workerId = receipt.connection_id;
  registrationFails = true;
  await assert.rejects(client.register("failed-different-worker"));
  assert.equal(client.canStartPublicationCompletion(runB, 2), false);
  registrationFails = false;
  await client.register("different-worker");
  await client.listRecoveryHolds(runB);
  await client.claimRun();
  assert.equal(client.canStartPublicationCompletion(runB, 2), true);
  response = { ...fixture.ack, run: { ...fixture.ack.run, id: runB, worker_id: workerId },
    completed_publication_receipt: { ...receipt, run_id: runB, generation: 2, worker_id: workerId } };
  assert.deepEqual((await client.reportState(runB, bodyB)).completedPublicationReceipt, response.completed_publication_receipt);
});

for (const provenance of ["hold-only", "receipt-only"] as const) {
  it(`ineligible A poisons its exact ${provenance} reservation`, async () => {
    await client.register("worker");
    if (provenance === "hold-only") await client.listRecoveryHolds(RUN);
    else assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt);
    claim = { ...claim, kind: "chat" };
    await client.claimRun();
    assert.equal(client.canStartPublicationCompletion(RUN, 1), false);
    assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
    await client.register("same-worker");
    claim = { ...claim, kind: "issue" };
    await client.claimRun();
    assert.equal(client.canStartPublicationCompletion(RUN, 1), false);
    assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
    assert.equal(client.canStartPublicationCompletion(RUN, 2), true, "contradiction is generation-exact");
  });
}

it("saturated ineligible filter preserves reserved original and refuses new identities and known hold conflicts", async () => {
  await client.register("worker");
  await client.listRecoveryHolds(RUN);
  const filter = Reflect.get(client, "ineligibleCompletionClaims") as Uint8Array;
  assert.equal(filter.byteLength, 64 * 1024);
  filter.fill(255);
  await client.register("same-worker");
  assert.ok(filter.every(byte => byte === 255), "same-worker registration preserves the monotonic filter");
  assert.equal(client.canStartPublicationCompletion(RUN, 1), true);
  assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt);
  assert.equal(client.canStartPublicationCompletion(RUN, 2), false);
  response.completed_publication_receipt = { ...receipt, generation: 2 };
  assert.equal((await client.reportState(RUN, { ...fixture.request, claim_generation: 2 })).completedPublicationReceipt, undefined);
  holds = [{ ...holds[0], hold_id: receipt.owner_id }];
  await client.listRecoveryHolds(RUN);
  response = structuredClone(fixture.ack);
  assert.equal(client.canStartPublicationCompletion(RUN, 1), false);
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
});

it("accepts exact stored terminal replay after fresh registration without claim or hold fetches", async () => {
  const original = Object.freeze(structuredClone(fixture.request)) as StateRequest;
  const snapshot = structuredClone(original);
  const registration = await client.register("fresh-replay-worker");
  assert.equal(registration.worker_id, receipt.worker_id);
  const ack = await client.reportState(RUN, original);
  assert.deepEqual(ack.completedPublicationReceipt, receipt);
  assert.deepEqual(sent, [snapshot], "replay sends the exact original terminal body");
  assert.deepEqual(original, snapshot, "replay preserves the persisted original body");
  assert.deepEqual(fetchedPaths, ["/api/worker/register", `/api/worker/runs/${RUN}/state`],
    "fresh replay needs registration and state ACK only");
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

it("retains hold and guarded claim provenance across failed same-worker registration", async () => {
  await client.register("worker");
  await client.claimRun();
  await client.listRecoveryHolds(RUN);
  registrationFails = true;
  await assert.rejects(client.register("worker"));
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
  registrationFails = false;
  await client.register("worker");
  holds = [];
  await client.listRecoveryHolds(RUN);
  response.completed_publication_receipt = { ...receipt, hold_id: receipt.owner_id };
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
  response.completed_publication_receipt = { ...receipt, branch: "foreign/branch" };
  assert.equal((await client.reportState(RUN, { ...fixture.request, branch: "foreign/branch" })).completedPublicationReceipt, undefined);
});

it("clears provenance only after successful registration as a different worker", async () => {
  await client.register("worker");
  claim = { ...claim, inventory_guarded: false };
  await client.claimRun();
  holds = [{ ...holds[0], hold_id: receipt.owner_id }];
  await client.listRecoveryHolds(RUN);
  workerId = receipt.owner_id;
  await client.register("other-worker");
  response = { ...fixture.ack, run: { ...fixture.ack.run, worker_id: workerId },
    completed_publication_receipt: { ...receipt, worker_id: workerId } };
  assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, response.completed_publication_receipt);
});

for (const code of [0, 0x1f, 0x7f, 0x9f, 0xad, 0x600, 0x61c, 0x180e, 0x200b, 0x200c,
  0x200d, 0x2060, 0x206a, 0x206b, 0x206c, 0x206d, 0x206e, 0x206f, 0xfeff, 0xfff9, 0xe0001, 0xe0020]) {
  it(`rejects Unicode control/format U+${code.toString(16)} in publication identifiers`, async () => {
    for (const field of ["branch", "base_url"]) {
      const value = receipt[field] + String.fromCodePoint(code);
      response.completed_publication_receipt = { ...receipt, [field]: value };
      assert.equal((await report({ ...fixture.request, branch: field === "branch" ? value : receipt.branch })).completedPublicationReceipt, undefined);
    }
  });
}

for (const header of [undefined, "1", "2097152"]) {
  it(`bounds actual streamed recovery-holds bytes with content-length ${header}`, async () => {
    await client.register("worker");
    await client.listRecoveryHolds(RUN);
    const payload = JSON.stringify({ run_id: RUN, holds, padding: "é".repeat(600_000) });
    holdsResponse = () => new Response(new ReadableStream({
      start(controller) {
        const bytes = new TextEncoder().encode(payload);
        for (let i = 0; i < bytes.length; i += 8192) controller.enqueue(bytes.subarray(i, i + 8192));
        controller.close();
      },
    }), { headers: header === undefined ? {} : { "content-length": header } });
    await assert.rejects(client.listRecoveryHolds(RUN));
    assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
  });
}

it("rejects excessive hold items before caching and retains refusal across empty listing and retry", async () => {
  await client.register("worker");
  holds = Array.from({ length: 1025 }, (_, i) => ({ ...holds[0], generation: i + 1 }));
  await assert.rejects(client.listRecoveryHolds(RUN));
  holds = [];
  await client.listRecoveryHolds(RUN);
  registrationFails = true;
  await assert.rejects(client.register("worker"));
  registrationFails = false;
  await client.register("worker");
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
});

it("bounds accumulated hold and claim bindings without evicting provenance", async () => {
  await client.register("worker");
  await client.listRecoveryHolds(RUN);
  // Each response is below the item and byte caps; the combined persistent budget is 4096.
  for (let page = 0; page < 4; page++) {
    holds = Array.from({ length: 1024 }, (_, i) => ({
      hold_id: receipt.hold_id, generation: page * 1024 + i + 1, inventory_guarded: true,
    }));
    await client.listRecoveryHolds(RUN);
  }
  claim = { ...claim, claim_generation: 4097 };
  await client.claimRun();
  const body = { ...fixture.request, claim_generation: 4097 };
  response.completed_publication_receipt = { ...receipt, generation: 4097 };
  assert.equal((await client.reportState(RUN, body)).completedPublicationReceipt, undefined);
  response.completed_publication_receipt = { ...receipt, hold_id: receipt.owner_id };
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
  await client.register("worker");
  holds = [];
  await client.listRecoveryHolds(RUN);
  assert.equal((await client.reportState(RUN, body)).completedPublicationReceipt, undefined);
  workerId = receipt.owner_id;
  await client.register("other-worker");
  response = { ...fixture.ack, run: { ...fixture.ack.run, worker_id: workerId },
    completed_publication_receipt: { ...receipt, worker_id: workerId } };
  assert.ok((await client.reportState(RUN, fixture.request)).completedPublicationReceipt);
});

it("bounds repeated hold responses without granting an unseen generation", async () => {
  await client.register("worker");
  for (let page = 0; page < 5; page++) {
    holds = Array.from({ length: 1024 }, (_, i) => ({
      hold_id: receipt.hold_id, generation: page * 1024 + i + 1, inventory_guarded: true,
    }));
    await client.listRecoveryHolds(RUN);
  }
  response.completed_publication_receipt = { ...receipt, generation: 4097 };
  assert.equal((await client.reportState(RUN, { ...fixture.request, claim_generation: 4097 })).completedPublicationReceipt, undefined);
});

it("preserves known unguarded claim across failed same-worker retry", async () => {
  await client.register("worker");
  claim = { ...claim, inventory_guarded: false };
  await client.claimRun();
  registrationFails = true;
  await assert.rejects(client.register("worker"));
  registrationFails = false;
  await client.register("worker");
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
});

it("bounds repeated claim accumulation without a hold listing", async () => {
  await client.register("worker");
  for (let generation = 1; generation <= 4097; generation++) {
    claim = { ...claim, claim_generation: generation };
    await client.claimRun();
  }
  response.completed_publication_receipt = { ...receipt, generation: 4097 };
  assert.equal((await client.reportState(RUN, { ...fixture.request, claim_generation: 4097 })).completedPublicationReceipt, undefined);
});

// Inspect retained values as well as the public ACK: a refusal alone would not detect
// oversized strings or raw payload objects retained by completionClaims.
for (const [field, value] of [
  ["repo_id", "r".repeat(1_048_576)], ["repo_id", { payload: "r".repeat(4096) }],
  ["repo_id", null], ["repo_id", 7], ["repo_id", "not-a-uuid"],
  ["forge_type", "f".repeat(1_048_576)], ["forge_type", { toString: null }],
  ["forge_type", null], ["forge_type", ["gitlab"]], ["forge_type", "unsupported"],
  ["branch", "b".repeat(1_048_576)], ["branch", "b".repeat(2049)],
  ["branch", "é".repeat(1025)], ["branch", { payload: "b".repeat(4096) }],
  ["branch", null], ["branch", 7], ["branch", "review/unsafe\u202e"],
  ["repo", null], ["repo", ["invalid"]],
] as const) {
  it(`retains bounded refusal marker for malformed claim provenance ${field} ${typeof value}`, async () => {
    const repo = { ...(claim.repo as Record<string, unknown>) };
    claim = { ...claim, kind: "mr_rework", branch: receipt.branch };
    if (field === "repo_id") repo.id = value;
    else if (field === "forge_type") repo.forge_type = value;
    else if (field === "branch") claim.branch = value;
    claim.repo = field === "repo" ? value : repo;
    await client.register("worker");
    const returned = await client.claimRun();
    assert.deepEqual(returned, claim, "claimRun keeps its existing returned payload");
    const bindings = Reflect.get(client, "completionClaims") as Map<string, unknown>;
    assert.equal(bindings.has(`${RUN}:1`), true, "invalid provenance must not become unknown");
    assert.equal(bindings.get(`${RUN}:1`), null, "retain only a small refusal marker");
    const ack = await client.reportState(RUN, fixture.request);
    assert.equal(ack.applied, true);
    assert.equal(ack.status, "completed");
    assert.equal(ack.completedPublicationReceipt, undefined);
    assert.equal(client.canStartPublicationCompletion(RUN, 2), true, "malformed incumbent does not block another identity");
  });
}

it("bounds retained provenance for sixteen oversized claims without saturation", async () => {
  await client.register("worker");
  for (let generation = 1; generation <= 16; generation++) {
    claim = { ...claim, kind: "mr_rework", claim_generation: generation, branch: "b".repeat(1_048_576) };
    await client.claimRun();
  }
  const bindings = Reflect.get(client, "completionClaims") as Map<string, unknown>;
  assert.equal(bindings.size, 16);
  assert.equal([...bindings.values()].every((value) => value === null), true,
    "oversized claim payloads leave only refusal markers");
  assert.equal(client.canStartPublicationCompletion(RUN, 17), true, "bounded refusal markers leave identity headroom");
  response.completed_publication_receipt = { ...receipt, generation: 16 };
  const ack = await client.reportState(RUN, { ...fixture.request, claim_generation: 16 });
  assert.equal(ack.applied, true);
  assert.equal(ack.status, "completed");
  assert.equal(ack.completedPublicationReceipt, undefined);
});

it("retains malformed provenance refusal across a valid same-generation claim retry", async () => {
  await client.register("worker");
  claim = { ...claim, repo: null };
  await client.claimRun();
  claim = { ...claim, repo: { ...makeClaim().repo, id: receipt.repo_id, forge_type: "gitlab" } };
  await client.claimRun();
  await client.register("worker");
  const ack = await client.reportState(RUN, fixture.request);
  assert.equal(ack.applied, true);
  assert.equal(ack.status, "completed");
  assert.equal(ack.completedPublicationReceipt, undefined);
});

it("copies only validated primitive provenance from an otherwise large valid claim", async () => {
  claim = { ...claim, kind: "mr_rework", branch: receipt.branch,
    repo: { ...(claim.repo as Record<string, unknown>), extra: { payload: "x".repeat(4096) } } };
  await client.register("worker");
  await client.claimRun();
  const bindings = Reflect.get(client, "completionClaims") as Map<string, unknown>;
  assert.deepEqual(bindings.get(`${RUN}:1`), {
    repoId: receipt.repo_id, forgeType: "gitlab", branch: receipt.branch, eligible: true, guarded: true,
  });
  assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt);
});

it("refuses malformed claim provenance at the ACK even with valid receipt and known hold", async () => {
  claim = { ...claim, kind: "mr_rework", branch: { payload: "oversized provenance" } };
  await client.register("worker");
  await client.claimRun();
  await client.listRecoveryHolds(RUN);
  const ack = await client.reportState(RUN, fixture.request);
  assert.equal(ack.applied, true);
  assert.equal(ack.status, "completed");
  assert.equal(ack.completedPublicationReceipt, undefined);
});

for (const branch of ["b".repeat(2048), "é".repeat(1024)]) {
  it(`accepts bounded primitive claim provenance at the 2048-byte branch boundary ${branch.length}`, async () => {
    claim = { ...claim, kind: "mr_rework", branch };
    response.completed_publication_receipt = { ...receipt, branch };
    await client.register("worker");
    await client.claimRun();
    assert.deepEqual((await client.reportState(RUN, { ...fixture.request, branch })).completedPublicationReceipt,
      response.completed_publication_receipt);
  });
}

for (const missing of ["repo", "repo_id", "forge_type", "branch", "kind"] as const) {
  it(`preserves safe legacy claim fallback with absent ${missing}`, async () => {
    const repo = { ...(claim.repo as Record<string, unknown>) };
    claim = { ...claim, kind: "mr_rework", branch: receipt.branch };
    if (missing === "repo_id") delete repo.id;
    if (missing === "forge_type") delete repo.forge_type;
    claim.repo = repo;
    if (missing === "repo" || missing === "branch" || missing === "kind") delete claim[missing];
    await client.register("worker");
    await client.claimRun();
    assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt);
  });
}

it("ignores malformed optional receipt forge object with null toString without failing completed ACK", async () => {
  response.completed_publication_receipt = { ...receipt, forge_type: { toString: null } };
  await client.register("worker");
  await client.claimRun();
  const ack = await client.reportState(RUN, fixture.request);
  assert.equal(ack.applied, true);
  assert.equal(ack.status, "completed");
  assert.equal(ack.completedPublicationReceipt, undefined);
  assert.deepEqual(sent[0], fixture.request);
});

it("refuses known noneligible or unguarded claims", async () => {
  for (const changes of [{ inventory_guarded: false }, { kind: "ci_fix" }]) {
    claim = { ...claim, ...changes };
    await client.register("worker");
    await client.claimRun();
    assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
  }
});

// Start a real public observation and hold only its authenticated response.
async function delayedObservation(lane: "claim" | "holds", runId = RUN, signal?: AbortSignal) {
  let finish!: (value: Response | Promise<Response>) => void;
  let entered!: () => void;
  const started = new Promise<void>(resolve => { entered = resolve; });
  const delayed = new Promise<Response>(resolve => { finish = resolve; });
  const originalFetch = globalThis.fetch;
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const pathname = new URL(String(input)).pathname;
    if (pathname.endsWith(lane === "claim" ? "/claim" : "/recovery-holds")) {
      entered();
      return delayed;
    }
    return originalFetch(input, init);
  });
  const pending = lane === "claim" ? client.claimRun(undefined, signal) : client.listRecoveryHolds(runId);
  // Attach the rejection handler immediately, before delivering transport failures.
  const settled = pending.then(value => ({ value }), error => ({ error }));
  await started;
  return { finish, settled };
}

for (const lane of ["claim", "holds"] as const) {
  it("retiring A preserves unrelated delayed B " + lane, async () => {
    await client.register("worker");
    await client.claimRun();
    const original = (await client.reportState(RUN, fixture.request)).completedPublicationReceipt!;
    const runB = receipt.owner_id;
    const delayed = await delayedObservation(lane, runB);
    client.releasePublicationCompletion(original);
    delayed.finish(Response.json(lane === "claim"
      ? { ...claim, run_id: runB, claim_generation: 2 }
      : { run_id: runB, holds: [{ ...holds[0], generation: 2 }] }));
    assert.equal("error" in await delayed.settled, false);
    assert.equal(client.canStartPublicationCompletion(runB, 2), true);
    response = { ...fixture.ack, run: { ...fixture.ack.run, id: runB },
      completed_publication_receipt: { ...receipt, run_id: runB, generation: 2 } };
    assert.deepEqual((await client.reportState(runB, { ...fixture.request, claim_generation: 2 })).completedPublicationReceipt,
      response.completed_publication_receipt);
  });

  for (const outcome of ["success", "http", "transport", "decode", "abort"] as const) {
    if (lane === "holds" && outcome === "abort") continue; // This public endpoint has no caller signal.
    it(lane + " retired reservation detaches after " + outcome, async () => {
      await client.register("worker");
      await client.claimRun();
      const original = (await client.reportState(RUN, fixture.request)).completedPublicationReceipt!;
      const abort = new AbortController();
      const delayed = await delayedObservation(lane, RUN, abort.signal);
      client.releasePublicationCompletion(original);
      for (let slot = 0; slot < 2047; slot++)
        assert.equal(client.canStartPublicationCompletion(receipt.owner_id, slot + 1), true);
      assert.equal(client.canStartPublicationCompletion(receipt.owner_id, 2048), false,
        "retired identity counts against the real combined budget");
      if (outcome === "abort") abort.abort();
      if (outcome === "transport") delayed.finish(Promise.reject(new Error("transport failure")));
      else if (outcome === "http") delayed.finish(new Response("unavailable", { status: 503 }));
      else if (outcome === "decode") delayed.finish(new Response("{invalid"));
      else delayed.finish(Response.json(lane === "claim" ? claim : { run_id: RUN, holds }));
      const result = await delayed.settled;
      assert.equal("error" in result, outcome !== "success");
      assert.equal((Reflect.get(client, "completionTickets") as Set<unknown>).size, 0);
      assert.equal((Reflect.get(client, "completionReservations") as Map<string, unknown>).has(RUN + ":1"), false);
      assert.equal((Reflect.get(client, lane === "claim" ? "completionClaims" : "completionHolds") as Map<string, unknown>).has(RUN + ":1"), false);
      if (!(lane === "holds" && outcome === "decode"))
        assert.equal(client.canStartPublicationCompletion(receipt.owner_id, 2048), true, "drainage reuses the budget");
    });
  }

  it(lane + " replay before drainage preserves exclusions across repeated retirement", async () => {
    await client.register("worker");
    // Receipt-only replay must reserve the identity too.
    const first = (await client.reportState(RUN, fixture.request)).completedPublicationReceipt!;
    const early = await delayedObservation(lane);
    client.releasePublicationCompletion(first);
    const replay = (await client.reportState(RUN, fixture.request)).completedPublicationReceipt!;
    assert.ok(replay);
    const later = await delayedObservation(lane);
    client.releasePublicationCompletion(replay);
    for (let slot = 0; slot < 2047; slot++) assert.equal(client.canStartPublicationCompletion(receipt.owner_id, slot + 1), true);
    const stale = () => Response.json(lane === "claim"
      ? { ...claim, repo: { ...(claim.repo as object), id: receipt.owner_id } }
      : { run_id: RUN, holds: [{ ...holds[0], hold_id: receipt.owner_id }] });
    early.finish(stale());
    await early.settled;
    assert.equal(client.canStartPublicationCompletion(receipt.owner_id, 2048), false, "later exclusion retains one shared reservation");
    later.finish(stale());
    await later.settled;
    assert.equal(client.canStartPublicationCompletion(RUN, 1), true);
    assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt,
      "neither pre-retirement response recreated a conflict");
  });

  it(lane + " tickets overflow before fetch and reuse after drainage", async () => {
    await client.register("worker");
    let finish!: (response: Response) => void;
    const delayed = new Promise<Response>(resolve => { finish = resolve; });
    const originalFetch = globalThis.fetch;
    let fetches = 0;
    mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      if (new URL(String(input)).pathname.endsWith(lane === "claim" ? "/claim" : "/recovery-holds")) {
        fetches++;
        return (await delayed).clone();
      }
      return originalFetch(input, init);
    });
    const start = () => lane === "claim" ? client.claimRun() : client.listRecoveryHolds(RUN);
    const requests = Array.from({ length: 2048 }, start);
    await assert.rejects(start(), /bookkeeping limit/);
    assert.equal(fetches, 2048, "overflow makes no transport call");
    assert.equal((Reflect.get(client, "completionClaims") as Map<string, unknown>).size, 0);
    assert.equal((Reflect.get(client, "completionHolds") as Map<string, unknown>).size, 0);
    finish(Response.json(lane === "claim" ? claim : { run_id: RUN, holds }));
    await Promise.all(requests);
    assert.equal(client.canStartPublicationCompletion(RUN, 1), true);
    await start();
    assert.equal(fetches, 2049, "drained ticket slot is reusable");
  });

  it(lane + " retirement excludes only exact generation", async () => {
    await client.register("worker");
    await client.claimRun();
    const original = (await client.reportState(RUN, fixture.request)).completedPublicationReceipt!;
    const delayed = await delayedObservation(lane);
    client.releasePublicationCompletion(original);
    delayed.finish(Response.json(lane === "claim" ? { ...claim, claim_generation: 2 }
      : { run_id: RUN, holds: [holds[0], { ...holds[0], generation: 2 }] }));
    await delayed.settled;
    assert.equal(client.canStartPublicationCompletion(RUN, 2), true);
    response = { ...fixture.ack, completed_publication_receipt: { ...receipt, generation: 2 } };
    assert.ok((await client.reportState(RUN, { ...fixture.request, claim_generation: 2 })).completedPublicationReceipt);
    assert.equal((Reflect.get(client, lane === "claim" ? "completionClaims" : "completionHolds") as Map<string, unknown>).has(RUN + ":1"), false);
  });
}

it("A-B-A delayed claim finally cannot detach a new incarnation ticket", async () => {
  await client.register("A");
  const old = await delayedObservation("claim");
  workerId = receipt.owner_id;
  await client.register("B");
  workerId = receipt.worker_id;
  await client.register("A-again");
  const current = await delayedObservation("holds");
  old.finish(Response.json({ ...claim, repo: { ...(claim.repo as object), id: receipt.owner_id } }));
  await old.settled;
  assert.equal((Reflect.get(client, "completionTickets") as Set<unknown>).size, 1);
  current.finish(Response.json({ run_id: RUN, holds }));
  await current.settled;
  assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt);
});

async function saturateClaims() {
  for (let generation = 2; generation <= 4097; generation++) {
    claim.claim_generation = generation;
    await client.claimRun();
  }
  claim.claim_generation = 1;
}

it("MR2598 regression 1: accepted claim-only completion replays at capacity and preserves conflict refusal", async () => {
  await client.register("worker");
  await client.claimRun();
  const original = Object.freeze(structuredClone(fixture.request)) as StateRequest;
  assert.deepEqual((await client.reportState(RUN, original)).completedPublicationReceipt, receipt);
  assert.equal(fetchedPaths.some(path => path.endsWith("/recovery-holds")), false,
    "original completion is accepted without hold inventory");
  await saturateClaims();
  assert.deepEqual((await client.reportState(RUN, original)).completedPublicationReceipt, receipt,
    "exact accepted original remains recognizable without a fetched hold at capacity");
  assert.equal(client.canStartPublicationCompletion(RUN, 1), true,
    "accepted original retains admission under pressure");
  assert.deepEqual(sent, [original, original], "replay preserves the original wire body");
  holds = [{ ...holds[0], hold_id: receipt.owner_id }];
  await client.listRecoveryHolds(RUN);
  assert.equal((await client.reportState(RUN, original)).completedPublicationReceipt, undefined,
    "known conflicting hold still refuses the accepted original");
});

it("MR2598 regression 2: retiring A does not discard an unrelated delayed guarded claim B", async t => {
  await client.register("worker");
  await client.claimRun();
  const ack = await client.reportState(RUN, fixture.request);
  assert.ok(ack.completedPublicationReceipt);
  const runB = "00000000-0000-4000-8000-000000002598";
  const claimB = { ...claim, run_id: runB, claim_generation: 2 };
  let finish!: (value: Response) => void;
  const delayed = new Promise<Response>(resolve => { finish = resolve; });
  let entered!: () => void;
  const started = new Promise<void>(resolve => { entered = resolve; });
  const originalFetch = globalThis.fetch;
  t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    if (new URL(String(input)).pathname.endsWith("/claim")) {
      entered();
      return delayed;
    }
    return originalFetch(input, init);
  });
  const pendingClaim = client.claimRun();
  try {
    await started;
    client.releasePublicationCompletion(ack.completedPublicationReceipt);
    finish(Response.json(claimB));
    const returned = await pendingClaim;
    assert.equal(returned?.run_id, runB);
    assert.equal(returned?.claim_generation, 2);
    assert.equal(client.canStartPublicationCompletion(runB, 2), true,
      "unrelated retirement must not saturate B admission");
    const bindings = Reflect.get(client, "completionClaims") as Map<string, unknown>;
    assert.ok(bindings.get(runB + ":2"), "delayed eligible B provenance remains cached");
  } finally {
    finish(Response.json(claimB));
    await pendingClaim;
  }
});

it("P1-b/c: unresolved bound receipt survives capacity and same-worker registration without forgetting conflict", async () => {
  await client.register("worker");
  await client.claimRun();
  await client.listRecoveryHolds(RUN);
  await saturateClaims();
  await client.register("same-worker");
  assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt,
    "fully observed unresolved original remains eligible under ordinary pressure");
  assert.equal(client.canStartPublicationCompletion(RUN, 1), true,
    "fully observed eligible generation remains admissible after saturation");
  holds = [{ ...holds[0], hold_id: receipt.owner_id }];
  await client.listRecoveryHolds(RUN);
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined,
    "known hold conflict must survive capacity");
});

it("P1-b: inventory scans known conflict sibling after unseen overflow", async () => {
  await client.register("worker");
  await client.claimRun();
  await client.listRecoveryHolds(RUN);
  await saturateClaims();
  holds = [{ ...holds[0], generation: 4098 }, { ...holds[0], hold_id: receipt.owner_id }];
  await client.listRecoveryHolds(RUN);
  assert.equal((Reflect.get(client, "completionHolds") as Map<string, unknown>).get(RUN + ":1"), null,
    "known contradictory sibling after unseen overflow must be poisoned");
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
});

it("P1-b: ineligible history uses no binding budget and preserves eligible completion", async () => {
  await client.register("worker");
  await client.claimRun();
  await client.listRecoveryHolds(RUN);
  for (let generation = 2; generation <= 4097; generation++) {
    claim = { ...claim, kind: "chat", claim_generation: generation };
    await client.claimRun();
  }
  assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt,
    "ineligible history must not exhaust eligible completion bindings");
  claim.claim_generation = 1;
  await client.claimRun();
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined,
    "eligible to ineligible contradiction remains refused");
});

for (const returnToA of [false, true]) {
  it("P1-b: delayed inventory from A cannot poison " + (returnToA ? "A after A-B-A" : "B"), async () => {
    await client.register("A");
    let finish!: (value: Response) => void;
    const pending = new Promise<Response>(resolve => { finish = resolve; });
    holdsResponse = () => pending as unknown as Response;
    const inventory = client.listRecoveryHolds(RUN);
    workerId = receipt.owner_id;
    await client.register("B");
    if (returnToA) { workerId = receipt.worker_id; await client.register("A-again"); }
    holdsResponse = undefined;
    await client.claimRun();
    await client.listRecoveryHolds(RUN);
    finish(Response.json({ run_id: RUN, holds: [{ ...holds[0], hold_id: receipt.owner_id }] }));
    await inventory;
    response = { ...fixture.ack, run: { ...fixture.ack.run, worker_id: workerId },
      completed_publication_receipt: { ...receipt, worker_id: workerId } };
    assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt,
      response.completed_publication_receipt);
  });
}

for (const failure of ["503", "transport"] as const) {
  it("transient inventory " + failure + " preserves cached receipt and new admission", async () => {
    await client.register("worker");
    await client.claimRun();
    await client.listRecoveryHolds(RUN);
    holdsResponse = () => {
      if (failure === "transport") throw new Error("temporary network failure");
      return Response.json({ error: "temporary outage" }, { status: 503 });
    };
    await assert.rejects(client.listRecoveryHolds(RUN));
    assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt);
    assert.equal(client.canStartPublicationCompletion(RUN, 1), true);
    holdsResponse = undefined;
    claim.claim_generation = 2;
    await client.claimRun();
    holds = [{ ...holds[0], generation: 2 }];
    await client.listRecoveryHolds(RUN);
    assert.equal(client.canStartPublicationCompletion(RUN, 2), true);
    response.completed_publication_receipt = { ...receipt, generation: 2 };
    assert.deepEqual((await client.reportState(RUN, { ...fixture.request, claim_generation: 2 })).completedPublicationReceipt,
      { ...receipt, generation: 2 });
  });
}

it("oversized inventory containing known contradiction hard refuses cached completion", async () => {
  await client.register("worker");
  await client.claimRun();
  await client.listRecoveryHolds(RUN);
  holdsResponse = () => Response.json({ run_id: RUN,
    holds: [{ ...holds[0], hold_id: receipt.owner_id }], padding: "x".repeat(1024 * 1024) });
  await assert.rejects(client.listRecoveryHolds(RUN));
  assert.equal(client.canStartPublicationCompletion(RUN, 1), false);
  assert.equal((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, undefined);
});

it("admission reserves an unknown key before later provenance fills its counterpart", async () => {
  await client.register("worker");
  assert.equal(client.canStartPublicationCompletion(RUN, 1), true);
  await saturateClaims();
  await client.claimRun();
  await client.listRecoveryHolds(RUN);
  assert.deepEqual((await client.reportState(RUN, fixture.request)).completedPublicationReceipt, receipt);
});
