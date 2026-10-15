import assert from "node:assert/strict";
import { afterEach, mock, test } from "node:test";
import { HeldFeatureUnavailableError, WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const repo = "44444444-4444-4444-8444-444444444444";
const v2 = "recovery_held_publication_v2";
const expected = { holdId: hold, repoId: repo };
const open = () => ({ run_id: run, hold_id: hold, generation: 1, repo_id: repo,
  original_worker_id: worker, original_worker_identity: "original", live_worker_id: worker,
  live_run_id: run, inventory_guarded: true, state: "open", run_status: "failed",
  current_claim_generation: 1, claim_released: false });
afterEach(() => mock.restoreAll());

async function setup(features: string[] = [v2], caps: string[] = [v2]) {
 let body: unknown = { run_id: run, holds: [], exact_hold: open() };
 const paths: string[] = [];
 const posts: unknown[] = [];
 mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
  const path = new URL(String(url));
  if (path.pathname.endsWith("/register")) return Response.json({ worker_id: worker, protocol_features: features });
  paths.push(path.pathname + path.search);
  if (init?.method === "POST") posts.push(JSON.parse(String(init.body)));
  return Response.json(body);
 });
 const client = new WorkerClient("http://exact.test", "worker-token", "test", nullLogger());
 await client.register("worker", undefined, 1, undefined, caps);
 return { client, paths, posts, answer: (value: unknown) => { body = value; } };
}

test("closed held proof replays only the immutable FINAL after publication capability loss", async () => {
 const e = await setup(["recovery_inventory_v1"]);
 const disposition = { kind: "held_publication" as const, publication_id: repo,
  source_sha: "a".repeat(40), coverage_digest: "b".repeat(64) };
 const closed = { ...open(), state: "released", live_worker_id: undefined, live_run_id: undefined,
  release_evidence: "held_publication", final_receipt: disposition };
 e.answer({ run_id: run, exact_hold: closed });
 const proof = await e.client.getExactRecoveryHold(run, 1, expected);
 assert.ok(proof);
 assert.equal((proof as unknown as Record<string, unknown>).released, undefined);
 await assert.rejects(e.client.releaseRecoveryCustody(run, 1, undefined, disposition), HeldFeatureUnavailableError);
 assert.equal(e.posts.length, 0);
 for (const mutation of [{ state: "discarded" }, { release_evidence: "owner_discard" },
  { original_worker_id: repo }, { original_worker_identity: "" }, { run_id: repo }, { generation: 2 },
  { hold_id: "bad" }, { repo_id: "bad" }, { current_claim_generation: 0 },
  { live_run_id: run }, { live_worker_id: worker }, { inventory_guarded: false },
  { final_receipt: { ...disposition, source_sha: "c".repeat(40) } },
  { final_receipt: { ...disposition, coverage_digest: "c".repeat(64) } },
  { final_receipt: { ...disposition, publication_id: hold } },
  { final_receipt: { ...disposition, extra: true } }]) {
  await assert.rejects(e.client.releaseRecoveryCustody(run, 1, undefined, disposition, undefined,
   { ...proof, ...mutation } as typeof proof), HeldFeatureUnavailableError);
 }
 await assert.rejects(e.client.releaseRecoveryCustody(run, 1, undefined,
  { ...disposition, publication_id: hold }, undefined, proof), HeldFeatureUnavailableError);
 await assert.rejects(e.client.releaseRecoveryCustody(run, 1, "publication", disposition, undefined, proof), HeldFeatureUnavailableError);
 assert.equal(e.posts.length, 0);
 const ack = { run_id: run, generation: 1, released: true, holds_released: 1 };
 e.answer(ack);
 assert.deepEqual(await e.client.releaseRecoveryCustody(run, 1, undefined, disposition, undefined, proof), ack);
 assert.deepEqual(e.posts, [{ generation: 1, final_disposition: disposition }]);
 await e.client.register("worker", undefined, 1, undefined, ["recovery_held_publication_v1"]);
 await assert.rejects(e.client.releaseRecoveryCustody(run, 1, undefined, disposition, undefined, proof), HeldFeatureUnavailableError);
 assert.equal(e.posts.length, 1);
});

test("exact observation requires both rollout directions and treats old API absence as unknown", async () => {
 const incapable = await setup([v2], []);
 assert.equal(await incapable.client.getExactRecoveryHold(run, 1, expected), undefined);
 assert.deepEqual(incapable.paths, []);
 mock.restoreAll();
 const e = await setup(["recovery_held_publication_v1"], [v2]);
 e.answer({ run_id: run, holds: [] });
 assert.equal(await e.client.getExactRecoveryHold(run, 1, expected), undefined, "old API open list is not closed proof");
 assert.deepEqual(e.paths, [`/api/worker/runs/${run}/recovery-holds?generation=1`]);
});

test("V2 publication is mutually negotiated and registration resets the worker capability", async () => {
 const e = await setup([v2], [v2]);
 assert.equal(e.client.hasHeldPublicationV2(), true);
 await e.client.register("worker", undefined, 1, undefined, []);
 assert.equal(e.client.hasHeldPublicationV2(), false);
 mock.restoreAll();
 const serverOnly = await setup([v2], []);
 assert.equal(serverOnly.client.hasHeldPublicationV2(), false);
 mock.restoreAll();
 const workerOnly = await setup([], [v2]);
 assert.equal(workerOnly.client.hasHeldPublicationV2(), false);
});

test("publication disabled advertisement does not disable exact custody observations", async () => {
 const e = await setup([], [v2]);
 assert.deepEqual(await e.client.getExactRecoveryHold(run, 1, expected), open());
 assert.deepEqual(e.paths, [`/api/worker/runs/${run}/recovery-holds?generation=1`]);
});

test("exact observation validates identity and closed evidence without minting a release ACK", async () => {
 const e = await setup();
 assert.deepEqual(await e.client.getExactRecoveryHold(run, 1, expected), open());
 assert.equal(e.paths[0], `/api/worker/runs/${run}/recovery-holds?generation=1`);
 for (const mutation of [{ run_id: repo }, { hold_id: repo }, { repo_id: hold }, { generation: 2 },
   { original_worker_id: repo }, { inventory_guarded: false }, { live_worker_id: repo }, { live_run_id: repo },
   { current_claim_generation: 0 }, { state: "released" }]) {
  e.answer({ run_id: run, holds: [], exact_hold: { ...open(), ...mutation } });
  assert.equal(await e.client.getExactRecoveryHold(run, 1, expected), undefined, JSON.stringify(mutation));
 }
 const closed = { ...open(), state: "released", live_worker_id: undefined, live_run_id: undefined,
  release_evidence: "held_publication", final_receipt: { kind: "held_publication", publication_id: repo,
  source_sha: "a".repeat(40), coverage_digest: "b".repeat(64) } };
 e.answer({ run_id: run, holds: [], exact_hold: closed });
 const observed = await e.client.getExactRecoveryHold(run, 1, expected);
 assert.equal(observed?.final_receipt?.publication_id, repo);
 assert.equal((observed as unknown as Record<string, unknown>)?.released, undefined);
 for (const size of [40, 64]) {
  const source = "a".repeat(size);
  e.answer({ run_id: run, holds: [], exact_hold: { ...closed, final_receipt: { ...closed.final_receipt, source_sha: source } } });
  assert.equal((await e.client.getExactRecoveryHold(run, 1, expected))?.final_receipt?.source_sha, source, `valid ${size}-character held tip`);
 }
 for (const size of [39, 41, 63, 65]) {
  e.answer({ run_id: run, holds: [], exact_hold: { ...closed, final_receipt: { ...closed.final_receipt, source_sha: "a".repeat(size) } } });
  assert.equal(await e.client.getExactRecoveryHold(run, 1, expected), undefined, `invalid ${size}-character held tip`);
 }
 e.answer({ run_id: run, holds: [], exact_hold: { ...closed, final_receipt: { ...closed.final_receipt, publication_id: "bad" } } });
 assert.equal(await e.client.getExactRecoveryHold(run, 1, expected), undefined);
 const policy = { ...closed, state: "discarded", release_evidence: "merged_predecessor_discard", final_receipt: undefined };
 e.answer({ run_id: run, holds: [], exact_hold: policy });
 assert.equal((await e.client.getExactRecoveryHold(run, 1, expected))?.release_evidence, "merged_predecessor_discard", "typed policy discard is settled observation");
 // The existing list method still returns the default list and does not request exact observation.
 e.answer({ run_id: run, holds: [] });
 assert.deepEqual(await e.client.listRecoveryHolds(run), { run_id: run, holds: [] });
 assert.equal(e.paths.at(-1), `/api/worker/runs/${run}/recovery-holds`);
});
