import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { WorkerClient } from "../src/client.js";
import { MaintenanceResources } from "../src/recovery-maintenance.js";
import { nullLogger, recordingLogger } from "./helpers.js";

const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const repo = "44444444-4444-4444-8444-444444444444";
const other = "55555555-5555-4555-8555-555555555555";
const capability = "recovery_held_publication_v2";
const row = () => ({ hold_id: hold, generation: 1, inventory_guarded: true, has_available_capture: false });
const exact = () => ({ run_id: run, hold_id: hold, generation: 1, repo_id: repo,
  original_worker_id: worker, original_worker_identity: "original", live_worker_id: worker,
  live_run_id: run, inventory_guarded: true, state: "open", run_status: "failed",
  current_claim_generation: 1, claim_released: false });

async function setup(caps = [capability], logger = nullLogger()) {
  const state = {
    listing: { run_id: run, holds: [row()] } as unknown,
    exact: { run_id: run, holds: [], exact_hold: exact() } as unknown,
    listingResponse: undefined as (() => Response | Promise<Response>) | undefined,
    exactResponse: undefined as (() => Response | Promise<Response>) | undefined,
    registrationFails: false, worker, paths: [] as string[], methods: [] as string[],
  };
  const transport: typeof fetch = async (input, init) => {
    const url = new URL(String(input));
    if (url.pathname.endsWith("/register")) {
      if (state.registrationFails) return new Response("registration refused", { status: 401 });
      return Response.json({ worker_id: state.worker, protocol_features: [] });
    }
    state.paths.push(url.pathname + url.search);
    state.methods.push(init?.method ?? "GET");
    if (url.search) return state.exactResponse?.() ?? Response.json(state.exact);
    return state.listingResponse?.() ?? Response.json(state.listing);
  };
  const client = new WorkerClient("http://maintenance.test", "worker-token", "test", logger, { fetch: transport });
  const register = () => client.register("worker", undefined, 1, undefined, caps);
  await register();
  return { client, state, register };
}

for (const status of ["failed", "cancelled", "completed"]) {
  test("unique terminal observation: " + status + ", including older terminal generation", async () => {
    const { client, state } = await setup();
    state.listing = { run_id: run, holds: [row(), { ...row(), hold_id: other, generation: 3, inventory_guarded: false }] };
    state.exact = { run_id: run, holds: [], exact_hold: { ...exact(), run_status: status, current_claim_generation: 3 } };
    const result = await MaintenanceResources.runDiscovery(() => client.observeTerminalMaintenanceHold(run, 1));
    assert.equal(result.ok, true);
    if (!result.ok) return;
    assert.equal(result.hold.repo_id, repo);
    assert.equal(result.hold.generation, 1);
    assert.equal(client.isCurrentTerminalMaintenanceHold(result.token), true);
    assert.equal(client.isCurrentTerminalMaintenanceHold({}), false);
    assert.deepEqual(state.paths, [
      `/api/worker/runs/${run}/recovery-holds`,
      `/api/worker/runs/${run}/recovery-holds?generation=1`,
    ]);
    assert.deepEqual(state.methods, ["GET", "GET"]);
  });
}

for (const field of ["run", "hold", "row_hold", "repo", "expected_repo", "run_worker_id", "worker"] as const) {
  test("producer refuses final-newline UUID: " + field, async () => {
    const { client, state, register } = await setup();
    const runId = field === "run" ? run + "\n" : run;
    const holdId = field === "hold" || field === "row_hold" ? hold + "\n" : hold;
    const repoId = field === "repo" || field === "expected_repo" ? repo + "\n" : repo;
    if (field === "worker") { state.worker = worker + "\n"; await register(); }
    state.listing = { run_id: runId, holds: [{ ...row(), hold_id: holdId }] };
    state.exact = { run_id: runId, exact_hold: { ...exact(), run_id: runId, live_run_id: runId,
      hold_id: holdId, repo_id: repoId, original_worker_id: state.worker, live_worker_id: state.worker,
      ...(field === "run_worker_id" ? { run_worker_id: worker + "\n" } : {}) } };
    const expected = field === "hold" || field === "expected_repo" ? { holdId, repoId } : undefined;
    assert.equal((await client.observeTerminalMaintenanceHold(runId, 1, expected)).ok, false);
    if (["run", "hold", "expected_repo", "worker"].includes(field)) assert.deepEqual(state.paths, []);
  });
}

for (const field of ["hold_id", "repo_id", "owner_id", "connection_id"] as const) {
  test("producer refuses final-newline receipt UUID: " + field, async () => {
    const { client, state } = await setup();
    const fixture = JSON.parse(readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
    const receipt = { ...fixture.ack.completed_publication_receipt,
      hold_id: other, run_id: run, worker_id: worker, repo_id: repo, generation: 2 };
    receipt[field] += "\n";
    state.listing = { run_id: run, holds: [row(), { ...row(), generation: 2,
      hold_id: receipt.hold_id, completed_publication_receipt: receipt }] };
    assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
    assert.equal(state.paths.length, 1);
  });
}

for (const phase of ["listing", "exact"] as const) {
  test("rejected streaming HTTP " + phase + " cancels unread body without interpretation or raw diagnostics", { timeout: 2000 }, async () => {
    for (const cancellation of ["pending", "rejected"] as const) {
      const { logger, lines } = recordingLogger();
      const { client, state } = await setup([capability], logger);
      let cancelled = false, reads = 0;
      const response = new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode(JSON.stringify(phase === "listing" ? state.listing : state.exact)));
        },
        pull() { reads++; },
        cancel() {
          cancelled = true;
          return cancellation === "pending" ? new Promise<void>(() => {}) : Promise.reject(new Error("raw-body-secret"));
        },
      }, { highWaterMark: 0 }), { status: 409 });
      const unexpectedRead = () => { reads++; throw new Error("raw-body-secret"); };
      Object.defineProperties(response, { text: { value: unexpectedRead }, json: { value: unexpectedRead } });
      Object.defineProperty(response.body, "getReader", { value: unexpectedRead });
      state[phase === "listing" ? "listingResponse" : "exactResponse"] = () => response;
      assert.deepEqual(await client.observeTerminalMaintenanceHold(run, 1), { ok: false, reason: "unavailable" });
      assert.equal(cancelled, true);
      assert.equal(reads, 0);
      assert.equal(state.paths.length, phase === "listing" ? 1 : 2);
      assert.equal(JSON.stringify(lines).includes("raw-body-secret"), false);
    }
  });
}

test("whole listing rejects duplicates and malformed siblings before exact lookup", async () => {
  const { client, state } = await setup();
  const invalid = [
    undefined, {}, { run_id: other, holds: [row()] }, { run_id: run, holds: [] },
    { run_id: run, holds: null },
    ...[
      row(), { ...row(), hold_id: other }, { ...row(), generation: 2 },
      { ...row(), hold_id: "bad", generation: 2 },
      { ...row(), hold_id: other, generation: 0 },
      { ...row(), hold_id: other, generation: 1.5 },
      { ...row(), hold_id: other, generation: 2, inventory_guarded: "true" },
      { ...row(), hold_id: other, generation: 2, has_available_capture: null },
      { ...row(), hold_id: other, generation: 2, capture_state: "secret-unknown" },
      { ...row(), hold_id: other, generation: 2, completed_publication_receipt: {} },
      { ...row(), hold_id: other, generation: 2, completed_publication_reason: "secret-unknown" },
    ].map(sibling => ({ run_id: run, holds: [row(), sibling] })),
    { run_id: run, holds: [{ ...row(), inventory_guarded: false }] },
    { run_id: run, holds: Array.from({ length: 1025 }, () => row()) },
  ];
  for (const listing of invalid) {
    state.paths.length = 0;
    state.listing = listing;
    assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
    assert.equal(state.paths.length, 1);
  }
});

test("bounded EOF: empty, partial JSON, byte overflow, stream failure and exact byte boundary", async () => {
  const { client, state } = await setup();
  const valid = JSON.stringify(state.listing);
  for (const text of ["", valid.slice(0, -1), valid + " ".repeat(1024 * 1024 - valid.length + 1)]) {
    state.listingResponse = () => new Response(text, { headers: { "Content-Length": "1" } });
    assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
  }
  state.listingResponse = () => new Response(new ReadableStream({
    start(controller) { controller.enqueue(new TextEncoder().encode(valid)); controller.error(new Error("secret")); },
  }));
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
  state.listingResponse = () => new Response(valid + " ".repeat(1024 * 1024 - valid.length));
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, true);
});

test("1024 distinct inventory rows are accepted and exact bodies share the byte cap", async () => {
  const { client, state } = await setup();
  state.listing = { run_id: run, holds: [row(), ...Array.from({ length: 1023 }, (_, index) => ({
    ...row(), hold_id: `aaaaaaaa-aaaa-4aaa-8aaa-${(index + 1).toString(16).padStart(12, "0")}`,
    generation: index + 2,
  }))] };
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, true);
  const body = JSON.stringify(state.exact);
  state.exactResponse = () => new Response(body + " ".repeat(1024 * 1024 - body.length + 1));
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
  state.exactResponse = () => new Response(body.slice(0, -1));
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
});

test("exact response must be open terminal custody for the selected identity", async () => {
  const { client, state } = await setup();
  for (const mutation of [
    { hold_id: other }, { run_id: other }, { generation: 2 }, { repo_id: "bad" },
    { original_worker_id: other }, { live_worker_id: other }, { live_run_id: other },
    { original_worker_identity: "" }, { original_worker_identity: "bad\nidentity" },
    { original_worker_identity: "x".repeat(2049) }, { inventory_guarded: false },
    { state: "released", live_worker_id: undefined, live_run_id: undefined },
    { state: "discarded" }, { current_claim_generation: 0 }, { claim_released: "false" },
    { run_worker_id: "bad" }, { recovery_wait_cause: false }, { release_evidence: "archive" },
    { final_receipt: {} }, { completed_publication_receipt: {} },
    ...["running", "claimed", "queued", "paused", "recovery_wait", "limit_wait", "pool_wait", "unknown"].map(run_status => ({ run_status })),
  ]) {
    state.exact = { run_id: run, exact_hold: { ...exact(), ...mutation } };
    assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false, JSON.stringify(mutation));
  }
  for (const body of [{}, { run_id: run, holds: [] }, { run_id: other, exact_hold: exact() }]) {
    state.exact = body;
    assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
  }
  state.exact = { run_id: run, exact_hold: exact() };
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1, { holdId: other, repoId: repo })).ok, false);
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1, { holdId: hold, repoId: other })).ok, false);
  assert.equal((await client.observeTerminalMaintenanceHold(run, 2)).ok, false);
  for (const status of [404, 409, 500, 204]) {
    state.exactResponse = () => new Response(null, { status });
    assert.deepEqual(await client.observeTerminalMaintenanceHold(run, 1), { ok: false, reason: "unavailable" });
  }
});

test("all supported capture summaries and well-formed receipt siblings remain valid", async () => {
  const { client, state } = await setup();
  const fixture = JSON.parse(readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
  for (const capture_state of ["", "preparing", "uploading", "available", "needs_action", "expired", "discarded"]) {
    state.listing = { run_id: run, holds: [{ ...row(), capture_state }, {
      ...row(), hold_id: other, generation: 2, completed_publication_reason: "not_completed",
      completed_publication_receipt: { ...fixture.ack.completed_publication_receipt,
        hold_id: other, run_id: run, worker_id: worker, repo_id: repo, generation: 2 },
    }] };
    assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, true);
  }
  state.exact = { run_id: run, exact_hold: { ...exact(), secret_extra: "do-not-return" } };
  const result = await client.observeTerminalMaintenanceHold(run, 1);
  assert.ok(result.ok);
  assert.equal("secret_extra" in result.hold, false);
});

test("administrative entries are charged before interpretation", async () => {
  const { client, state } = await setup();
  // Discovery permits 4096 administrative entries in aggregate. These invalid rows
  // still consume their entries, so the fifth 1024-row read must exhaust the meter.
  state.listing = { run_id: run, holds: Array.from({ length: 1024 }, () => null) };
  await assert.rejects(MaintenanceResources.runDiscovery(async () => {
    for (let attempt = 0; attempt < 5; attempt++) {
      assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
    }
  }), /maintenance_resource_exhausted/);
});

test("unsupported capability never requests inventory", async () => {
  const { client, state } = await setup([]);
  assert.deepEqual(await client.observeTerminalMaintenanceHold(run, 1), { ok: false, reason: "unsupported" });
  assert.deepEqual(state.paths, []);
});

test("supported invalid requests never request inventory", async () => {
  const { client, state } = await setup();
  const requests: [string, number, { holdId: string; repoId: string }?][] = [
    ...[0, -1, 1.5, NaN, Infinity, -Infinity, Number.MAX_SAFE_INTEGER + 1]
      .map(generation => [run, generation] as [string, number]),
    ...["", "bad", run + "\n"].map(id => [id, 1] as [string, number]),
    ...["", "bad", hold + "\n"].map(holdId => [run, 1, { holdId, repoId: repo }] as [string, number, { holdId: string; repoId: string }]),
    ...["", "bad", repo + "\n"].map(repoId => [run, 1, { holdId: hold, repoId }] as [string, number, { holdId: string; repoId: string }]),
  ];
  for (const [runId, generation, expected] of requests) {
    assert.deepEqual(await client.observeTerminalMaintenanceHold(runId, generation, expected),
      { ok: false, reason: "invalid_request" });
    assert.deepEqual(state.paths, []);
  }
});

test("producer preserves valid worker provenance labels", async () => {
  const { client, state } = await setup();
  for (const identity of ["original", "My laptop", "Équipe 東京", " My laptop ", "x".repeat(2048), "é".repeat(1024)]) {
    state.exact = { run_id: run, exact_hold: { ...exact(), original_worker_identity: identity } };
    const result = await client.observeTerminalMaintenanceHold(run, 1);
    assert.ok(result.ok, "valid worker label must be accepted unchanged");
    assert.equal(result.hold.original_worker_identity, identity);
    assert.equal(result.hold.original_worker_id, worker);
  }
});

test("producer rejects unsafe or unbounded worker labels without logging them", async () => {
  const { logger, lines } = recordingLogger();
  const { client, state } = await setup([capability], logger);
  for (const identity of [undefined, null, 1, "", "   ", "bad\nidentity", "bad\tidentity",
    "bad\u007fidentity", "bad\u0085identity", "bad\u202eidentity", "bad\u2066identity",
    "x".repeat(2049), "é".repeat(1025)]) {
    state.exact = { run_id: run, exact_hold: { ...exact(), original_worker_identity: identity } };
    assert.deepEqual(await client.observeTerminalMaintenanceHold(run, 1), { ok: false, reason: "invalid_exact_hold" });
  }
  assert.deepEqual(lines, []);
});

test("registration entry invalidates tokens even on same-worker failure or refresh", async () => {
  const { client, state, register } = await setup();
  const first = await client.observeTerminalMaintenanceHold(run, 1);
  assert.ok(first.ok);
  await register();
  assert.equal(client.isCurrentTerminalMaintenanceHold(first.token), false);
  const refreshed = await client.observeTerminalMaintenanceHold(run, 1);
  assert.ok(refreshed.ok);
  state.registrationFails = true;
  const pending = register();
  assert.equal(client.isCurrentTerminalMaintenanceHold(refreshed.token), false);
  await assert.rejects(pending);
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
});

for (const phase of ["listing", "exact"] as const) {
  test("in-flight " + phase + " refuses registration replacement and same-ID failed registration", async () => {
    for (const failure of [false, true]) {
      const { client, state, register } = await setup();
      let release!: (response: Response) => void;
      let entered!: () => void;
      const started = new Promise<void>(resolve => { entered = resolve; });
      const delayed = () => { entered(); return new Promise<Response>(resolve => { release = resolve; }); };
      if (phase === "listing") state.listingResponse = delayed;
      else state.exactResponse = delayed;
      const observation = client.observeTerminalMaintenanceHold(run, 1);
      await started;
      state.registrationFails = failure;
      if (!failure) state.worker = other;
      if (failure) await assert.rejects(register());
      else await register();
      release(Response.json(phase === "listing" ? state.listing : state.exact));
      assert.deepEqual(await observation, { ok: false, reason: "registration_changed" });
    }
  });
}

test("second observation catches a close race without granting effect authority", async () => {
  const { client, state } = await setup();
  const first = await client.observeTerminalMaintenanceHold(run, 1);
  assert.ok(first.ok);
  state.exact = { run_id: run, exact_hold: { ...exact(), state: "released", live_worker_id: undefined, live_run_id: undefined } };
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1, { holdId: hold, repoId: repo })).ok, false);
  assert.equal(client.isCurrentTerminalMaintenanceHold(first.token), true, "currency guard only fences registration; custody requires re-observation");
});

test("observer never changes completion inventory or creates receipt ACK authority", async () => {
  const { client, state } = await setup();
  assert.equal(client.canStartPublicationCompletion(run, 1), true);
  state.listing = { run_id: run, holds: [row(), row()] };
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
  assert.equal(client.canStartPublicationCompletion(run, 1), true, "invalid discovery cannot poison ordinary completion");
  state.listing = { run_id: run, holds: [row()] };
  const result = await client.observeTerminalMaintenanceHold(run, 1);
  assert.ok(result.ok);
  assert.equal("completedPublicationReceipt" in result, false);
  const fixture = JSON.parse(readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
  state.listing = { run_id: run, holds: [{ ...row(), completed_publication_receipt: fixture.ack.completed_publication_receipt }] };
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, false);
  assert.deepEqual(state.methods, state.methods.map(() => "GET"));
  state.listing = { run_id: run, holds: [row(), row()] };
  await client.listRecoveryHolds(run);
  assert.equal(client.canStartPublicationCompletion(run, 1), false);
  state.listing = { run_id: run, holds: [row()] };
  assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, true);
  assert.equal(client.canStartPublicationCompletion(run, 1), false, "fresh producer facts cannot clear ordinary completion conflict");
});
