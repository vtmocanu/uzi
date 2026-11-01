import assert from "node:assert/strict";
import { afterEach, mock, test } from "node:test";
import { WorkerClient } from "../src/client.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { nullLogger } from "./helpers.js";

const feature = "recovery_bindingless_closed_v1";
const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const repo = "44444444-4444-4444-8444-444444444444";
const open = () => ({ run_id: run, hold_id: hold, generation: 1, repo_id: repo,
  original_worker_id: worker, original_worker_identity: "original", live_worker_id: worker,
  live_run_id: run, inventory_guarded: true, state: "open", run_status: "failed",
  current_claim_generation: 1, claim_released: false });
const closed = () => ({ ...open(), state: "released", live_worker_id: undefined, live_run_id: undefined,
  release_evidence: "archive", final_receipt: { kind: "archive", capture_id: repo,
    source_sha: "a".repeat(40), coverage_digest: "b".repeat(64) } });
const envelope = (value: unknown) => ({ run_id: run, exact_hold: value });
const response = (value: unknown) => Response.json(value, { headers: { "Uzi-Recovery-Observation": feature } });
const deferred = <T>() => {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(r => { resolve = r; });
  return { promise, resolve };
};
afterEach(() => mock.restoreAll());

async function setup(features: string[] = [feature], caps: string[] = [feature]) {
  let read = async () => response(envelope(closed()));
  let registration = async () => Response.json({ worker_id: worker, protocol_features: features });
  const requests: { path: string; method: string | undefined }[] = [];
  mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
    const path = new URL(String(url));
    if (path.pathname.endsWith("/register")) return registration();
    requests.push({ path: path.pathname + path.search, method: init?.method });
    return read();
  });
  const client = new WorkerClient("http://bindingless.test", "worker-token", "test", nullLogger());
  const register = () => client.register("worker", undefined, 1, undefined, caps);
  await register();
  return { client, register, requests,
    answer: (value: unknown) => { read = async () => response(value); },
    read: (next: typeof read) => { read = next; },
    registration: (next: typeof registration) => { registration = next; } };
}

test("bindingless observation independently negotiates capability, feature and exact header with writes disabled", async () => {
  for (const [features, caps] of [[[], [feature]], [[feature], []], [["recovery_held_publication_v2"], [feature]]]) {
    const e = await setup(features, caps);
    assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined);
    assert.deepEqual(e.requests, []);
    mock.restoreAll();
  }
  const e = await setup();
  assert.equal(e.client.hasHeldPublicationV2(), false);
  assert.deepEqual(await e.client.getBindinglessRecoveryHold(run, 1), JSON.parse(JSON.stringify(closed())));
  assert.deepEqual(e.requests, [{ path: `/api/worker/runs/${run}/recovery-holds?generation=1`, method: "GET" }]);
  for (const marker of [undefined, "recovery_held_publication_v2", `${feature}, other`, feature.toUpperCase()]) {
    e.read(async () => Response.json(envelope(closed()), { headers: marker ? { "Uzi-Recovery-Observation": marker } : {} }));
    assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined);
  }
  e.answer({ run_id: run, holds: [] });
  assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined);
  e.client.clearFeatures();
  e.answer(envelope(closed()));
  assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined);
});

test("bindingless observation validates all identity fields and authenticates open without a local binding", async () => {
  const e = await setup([feature], [feature, "recovery_held_publication_v2"]);
  e.answer(envelope(open()));
  assert.deepEqual(await e.client.getBindinglessRecoveryHold(run, 1), open());
  assert.equal(await e.client.getExactRecoveryHold(run, 1, { holdId: repo, repoId: hold }), undefined);
  for (const mutation of [{ run_id: repo }, { hold_id: "bad" }, { repo_id: "bad" }, { generation: 2 },
    { original_worker_id: repo }, { original_worker_identity: " " }, { inventory_guarded: false },
    { current_claim_generation: 0 }, { current_claim_generation: 1.5 }, { claim_released: "false" },
    { run_status: "unknown" }, { run_worker_id: "bad" }, { recovery_wait_cause: 1 },
    { live_worker_id: repo }, { live_run_id: repo }, { release_evidence: "archive" },
    { final_receipt: {} }, { completed_publication_receipt: {} }]) {
    e.answer(envelope({ ...open(), ...mutation }));
    assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined, JSON.stringify(mutation));
  }
  e.answer(envelope({ ...open(), current_claim_generation: 2 }));
  assert.ok(await e.client.getBindinglessRecoveryHold(run, 1));
  for (const [id, gen] of [["bad", 1], [run, 0], [run, -1], [run, 1.5], [run, Infinity]] as const)
    assert.equal(await e.client.getBindinglessRecoveryHold(id, gen), undefined);
});

test("bindingless closed observations share the full supported proof validator", async () => {
  const e = await setup();
  const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
  const completed = { hold_id: hold, run_id: run, repo_id: repo, worker_id: worker, generation: 1,
    final_head: "a".repeat(40), observed_branch_head: "b".repeat(40), owner_id: repo, connection_id: hold,
    project_id: 1, mr_iid: 2, forge_type: "github", branch: "work", base_url: "https://example.com" };
  const proofs = [closed(), { ...closed(), release_evidence: "prerequisite_closure" },
    { ...closed(), release_evidence: "held_publication", final_receipt: { kind: "held_publication",
      publication_id: repo, source_sha: "a".repeat(40), coverage_digest: "b".repeat(64) } },
    ...["publication", "forge_no_output"].map(release_evidence => ({ ...closed(), release_evidence,
      final_receipt: { kind: "settled", coverage_digest: emptyDigest } })),
    { ...closed(), release_evidence: "no_adopted_source", final_receipt: { kind: "no_adopted_source", coverage_digest: "" } },
    { ...closed(), release_evidence: "completed_publication", final_receipt: { kind: "completed_publication", coverage_digest: "" }, completed_publication_receipt: completed },
    ...["owner_discard", "merged_predecessor_discard"].map(release_evidence => ({ ...closed(), state: "discarded", release_evidence, final_receipt: undefined }))];
  for (const proof of proofs) {
    e.answer(envelope(proof));
    assert.deepEqual(await e.client.getBindinglessRecoveryHold(run, 1), JSON.parse(JSON.stringify(proof)), proof.release_evidence);
  }
  for (const mutation of [{ release_evidence: "unknown" }, { state: "unknown" }, { live_worker_id: worker },
    { live_run_id: run }, { final_receipt: undefined }, { final_receipt: { ...closed().final_receipt, capture_id: "bad" } },
    { final_receipt: { ...closed().final_receipt, coverage_digest: "bad" } },
    { final_receipt: { ...closed().final_receipt, source_sha: "bad!" } },
    { release_evidence: "held_publication" }, { completed_publication_receipt: completed }]) {
    e.answer(envelope({ ...closed(), ...mutation }));
    assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined, JSON.stringify(mutation));
  }
  e.answer(envelope({ ...proofs[6], completed_publication_receipt: { ...completed, hold_id: repo } }));
  assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined);
});

test("bindingless reads refuse inaccessible, malformed, truncated, partial and oversized responses", async () => {
  const e = await setup();
  for (const status of [403, 404, 409, 206, 500]) {
    e.read(async () => new Response(JSON.stringify(envelope(closed())), { status, headers: { "Uzi-Recovery-Observation": feature } }));
    assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined, String(status));
  }
  for (const body of ["{", JSON.stringify(envelope(closed())).slice(0, -1), " ".repeat(1024 * 1024 + 1), "null", "[]", JSON.stringify({ ...envelope(closed()), run_id: repo })]) {
    e.read(async () => new Response(body, { headers: { "Uzi-Recovery-Observation": feature } }));
    assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined);
  }
  e.read(async () => new Response(new ReadableStream({ start(c) { c.error(new Error("truncated")); } }), { headers: { "Uzi-Recovery-Observation": feature } }));
  assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined);
  e.read(async () => { throw new Error("network failure"); });
  await assert.rejects(e.client.getBindinglessRecoveryHold(run, 1), /network failure/);
});

test("registration entry invalidates in-flight bindingless reads including failed same-ID and changed-worker registration", async () => {
  for (const [nextWorker, failure] of [[worker, false], [worker, true], [repo, false]] as const) {
    const e = await setup();
    const entered = deferred<void>(), release = deferred<Response>();
    e.read(async () => { entered.resolve(); return release.promise; });
    const pending = e.client.getBindinglessRecoveryHold(run, 1);
    await entered.promise;
    e.registration(async () => { if (failure) throw new Error("registration failed"); return Response.json({ worker_id: nextWorker, protocol_features: [feature] }); });
    if (failure) await assert.rejects(e.register(), /registration failed/);
    else await e.register();
    release.resolve(response(envelope(closed())));
    assert.equal(await pending, undefined);
    e.answer(envelope({ ...closed(), original_worker_id: nextWorker }));
    assert.equal(Boolean(await e.client.getBindinglessRecoveryHold(run, 1)), !failure);
    mock.restoreAll();
  }
});

test("registration during body read invalidates an observation; out-of-order replies cannot restore capability", async () => {
  const e = await setup();
  const pulled = deferred<void>(), release = deferred<void>();
  e.read(async () => new Response(new ReadableStream({ async pull(controller) {
    pulled.resolve(); await release.promise;
    controller.enqueue(new TextEncoder().encode(JSON.stringify(envelope(closed())))); controller.close();
  } }), { headers: { "Uzi-Recovery-Observation": feature } }));
  const pending = e.client.getBindinglessRecoveryHold(run, 1);
  await pulled.promise;
  const older = deferred<Response>(), newer = deferred<Response>();
  e.registration(() => older.promise);
  const first = e.register();
  e.registration(() => newer.promise);
  const second = e.register();
  newer.resolve(Response.json({ worker_id: worker, protocol_features: [] }));
  await second;
  older.resolve(Response.json({ worker_id: repo, protocol_features: [feature] }));
  await first;
  release.resolve();
  assert.equal(await pending, undefined);
  e.answer(envelope(closed()));
  assert.equal(await e.client.getBindinglessRecoveryHold(run, 1), undefined);
});

test("body cancellation and mandatory maintenance exhaustion propagate", async () => {
  const e = await setup();
  const controller = new AbortController();
  e.read(async () => new Response(new ReadableStream({ pull() { controller.abort(new Error("caller cancelled")); } }),
    { headers: { "Uzi-Recovery-Observation": feature } }));
  await assert.rejects(e.client.getBindinglessRecoveryHold(run, 1, controller.signal), /caller cancelled/);
  let now = 0;
  const budget = new MaintenanceBudget(1000, () => now);
  e.read(async () => new Response(new ReadableStream({ pull(c) {
    now = 1001;
    c.enqueue(new TextEncoder().encode("{")); c.close();
  } }), { headers: { "Uzi-Recovery-Observation": feature } }));
  await assert.rejects(async () => MaintenanceBudget.run(budget, () => e.client.getBindinglessRecoveryHold(run, 1)), /maintenance/);
});
