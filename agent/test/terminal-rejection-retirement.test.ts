import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import { RequestError, WorkerClient } from "../src/client.js";
import { makeClaim, nullLogger } from "./helpers.js";

const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

async function fixture() {
  const c = new WorkerClient("http://retirement.test", "join-token", "test", nullLogger());
  let registration: unknown = { worker_id: worker, protocol_features: ["terminal_rejection_report", "recovery_inventory_v1"] };
  let ownership: unknown = { status: "completed", claim_generation: 3, inventory_guarded: true };
  let custody: Record<string, unknown> = {
    run_id: run, worker_id: worker, generation: 3,
    exact_holds: [{ id: hold, state: "discarded" }], sibling_holds: [],
    exact_count: 1, sibling_count: 0, exact_complete: true, sibling_complete: true, complete: true, outcome: "settled",
  };
  let claim: unknown = { ...makeClaim(), run_id: run, claim_generation: 3, inventory_guarded: true };
  const paths: string[] = [];
  globalThis.fetch = async (input) => {
    const path = new URL(String(input)).pathname;
    paths.push(path);
    if (path.endsWith("/register")) {
      if (registration instanceof Error) throw registration;
      return Response.json(registration);
    }
    if (path.endsWith("/ownership")) {
      if (ownership instanceof Error) throw ownership;
      if (ownership instanceof Response) return ownership;
      return Response.json(ownership);
    }
    if (path.endsWith("/runs/claim")) return Response.json(claim);
    return Response.json(custody);
  };
  await c.register("test");
  return { c, paths, setRegistration: (v: unknown) => { registration = v; },
    setOwnership: (v: unknown) => { ownership = v; }, setCustody: (v: Record<string, unknown>) => { custody = v; },
    custody: () => structuredClone(custody), setClaim: (v: unknown) => { claim = v; } };
}

// Finite 1 KiB chunks, with no prefetch: uncapped readers reach EOF, while bounded
// readers cancel before the unread tail. No timers or background producers.
function streamedOwnership(body: string, status = 200, contentLength?: string) {
  const bytes = new TextEncoder().encode(body);
  let offset = 0;
  let cancelled = false;
  const stream = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (offset === bytes.length) { controller.close(); return; }
      const end = Math.min(offset + 1024, bytes.length);
      controller.enqueue(bytes.subarray(offset, end));
      offset = end;
    },
    cancel() { cancelled = true; },
  }, { highWaterMark: 0 });
  const headers = new Headers({ "Content-Type": "application/json", "Retry-After": "7" });
  if (contentLength !== undefined) headers.set("Content-Length", contentLength);
  return {
    response: new Response(stream, { status, headers }),
    cancelled: () => cancelled,
    bytesRead: () => offset,
  };
}

test("oversized streamed ownership refuses retirement and retains guarded custody", async () => {
  for (const state of ["absent", "pending"] as const) {
    for (const contentLength of [undefined, "1"]) {
      const f = await fixture();
      await f.c.claimRun();
      const body = JSON.stringify({
        status: "completed", claim_generation: 3, inventory_guarded: true,
        padding: "x".repeat(32 * 1024),
      });
      const streamed = streamedOwnership(body, 200, contentLength);
      f.setOwnership(streamed.response);
      await assert.rejects(f.c.hasRecoveryRetirementAuthority(run, 3, state), /response body exceeds 16384 bytes/);
      assert.equal(streamed.cancelled(), true);
      assert.equal(streamed.bytesRead(), 17 * 1024);
      assert.equal(f.c.knowsInventoryGuardedClaim(run, 3), true);
      assert.equal(f.paths.some(p => p.includes("terminal-rejection-custody")), false);
    }
  }
});

test("oversized ownership errors remain bounded and preserve status and guarded custody", async () => {
  for (const state of ["absent", "pending"] as const) {
    for (const status of [404, 503]) {
      for (const contentLength of [undefined, "1"]) {
        const f = await fixture();
        await f.c.claimRun();
        const streamed = streamedOwnership("x".repeat(32 * 1024), status, contentLength);
        f.setOwnership(streamed.response);
        await assert.rejects(f.c.hasRecoveryRetirementAuthority(run, 3, state), (err: unknown) => {
          assert.ok(err instanceof RequestError);
          assert.equal(err.method, "GET");
          assert.equal(err.path, `/api/worker/runs/${run}/ownership`);
          assert.equal(err.status, status);
          assert.equal(err.body, "x".repeat(4096));
          assert.equal(err.retryAfterHeaderMs, 7000);
          return true;
        });
        assert.equal(streamed.cancelled(), true);
        assert.equal(streamed.bytesRead(), 4096);
        assert.equal(f.c.knowsInventoryGuardedClaim(run, 3), true);
        assert.equal(f.paths.some(p => p.includes("terminal-rejection-custody")), false);
      }
    }
  }
});

test("bounded ownership preserves normal responses, exact-cap bodies and small errors", async () => {
  const f = await fixture();
  const ownership = { status: "completed", claim_generation: 3, inventory_guarded: true };
  for (const size of [undefined, 16 * 1024]) {
    const json = JSON.stringify(ownership);
    const body = size === undefined ? json : json.padEnd(size, " ");
    f.setOwnership(streamedOwnership(body, 200, "1").response);
    assert.deepEqual(await f.c.getRunOwnership(run), ownership);
    await f.c.claimRun();
    f.setOwnership(streamedOwnership(body, 200, "1").response);
    assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), true);
    assert.equal(f.c.knowsInventoryGuardedClaim(run, 3), false);
  }
  f.setOwnership(streamedOwnership("not owned", 404, "1").response);
  await assert.rejects(f.c.getRunOwnership(run), (err: unknown) => {
    assert.ok(err instanceof RequestError);
    assert.equal(err.status, 404);
    assert.equal(err.body, "not owned");
    assert.equal(err.retryAfterHeaderMs, 7000);
    return true;
  });
});

test("pending journal requires all discarded; absent journal accepts complete released custody", async () => {
  const f = await fixture();
  await f.c.claimRun();
  f.setCustody({ ...f.custody(), exact_holds: [{ id: hold, state: "released" }] });
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), false);
  assert.equal(f.c.knowsInventoryGuardedClaim(run, 3), true);
  f.setCustody({ ...f.custody(), exact_holds: [{ id: hold, state: "discarded" }] });
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), true);
  assert.equal(f.c.knowsInventoryGuardedClaim(run, 3), false);
  f.c.clearFeatures();
  f.setClaim({ ...makeClaim(), run_id: run, claim_generation: 3, inventory_guarded: false });
  await assert.rejects(f.c.claimRun(), /cannot downgrade/);
  await assert.rejects(f.c.releaseRecoveryCustody(run, 3), /guarded/);
  // A genuinely fresh client has no remembered claim membership.
  const fresh = await fixture();
  fresh.setCustody({ ...fresh.custody(), exact_holds: [{ id: hold, state: "released" }] });
  assert.equal(await fresh.c.hasRecoveryRetirementAuthority(run, 3, "absent"), true);
});

test("fresh absent journal needs exact explicit legacy ownership, without report feature", async () => {
  const f = await fixture();
  f.c.clearFeatures();
  for (const ownership of [null, {}, { status: 7, claim_generation: 3, inventory_guarded: false },
    { status: "bogus", claim_generation: 3, inventory_guarded: false },
    { status: "running", claim_generation: 3, inventory_guarded: false },
    { status: "completed", claim_generation: 0, inventory_guarded: false },
    { status: "completed", claim_generation: 3 }, { status: "completed", claim_generation: 3, inventory_guarded: "false" },
    { status: "completed", claim_generation: 4, inventory_guarded: false },
    { status: "completed", claim_generation: Number.MAX_SAFE_INTEGER + 1, inventory_guarded: false }]) {
    f.setOwnership(ownership);
    assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "absent"), false);
  }
  f.setOwnership(new Error("unavailable"));
  await assert.rejects(f.c.hasRecoveryRetirementAuthority(run, 3, "absent"));
  f.setOwnership({ status: "completed", claim_generation: 3, inventory_guarded: false });
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 0, "absent"), false);
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "absent"), true);
  assert.equal(f.paths.some(p => p.includes("terminal-rejection-custody")), false);
});

test("absent missing or malformed guard retains until later valid authority", async () => {
  const f = await fixture();
  for (const inventory_guarded of [undefined, null, "true", [], {}]) {
    f.setOwnership({ status: "completed", claim_generation: 3, inventory_guarded });
    assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "absent"), false);
  }
  f.setOwnership({ status: "completed", claim_generation: 3, inventory_guarded: true });
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "absent"), true);
});

test("feature negotiation alone does not substitute for a registered worker identity", async () => {
  const f = await fixture();
  const c = new WorkerClient("http://retirement.test", "join-token", "test", nullLogger());
  c.protocolFeatures = ["terminal_rejection_report"];
  assert.equal(await c.hasRecoveryRetirementAuthority(run, 3, "absent"), false);
  f.c.clearFeatures();
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), false);
});

test("remembered guarded claim cannot become legacy on feature loss", async () => {
  const f = await fixture();
  await f.c.claimRun();
  f.c.clearFeatures();
  f.setOwnership({ status: "completed", claim_generation: 3, inventory_guarded: false });
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "absent"), false);
  assert.equal(f.c.knowsInventoryGuardedClaim(run, 3), true);
});

test("guarded authority validates terminal ownership status and safe current generation", async () => {
  const f = await fixture();
  for (const status of ["completed", "failed", "cancelled"]) {
    f.setOwnership({ status, claim_generation: 4 });
    assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), true);
  }
  for (const ownership of [null, [], {}, { status: "running", claim_generation: 3 },
    { status: "completed", claim_generation: 2 }, { status: "completed", claim_generation: "3" },
    { status: "completed", claim_generation: 3.5 }, { status: "completed" },
    { status: "completed", claim_generation: 3, inventory_guarded: "true" },
    { status: "completed", claim_generation: Number.MAX_SAFE_INTEGER + 1 }]) {
    f.setOwnership(ownership);
    assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), false);
  }
});

test("authority cannot reuse identity during an in-flight registration attempt", async () => {
  const f = await fixture();
  const previousFetch = globalThis.fetch;
  let finish!: () => void;
  const gate = new Promise<void>(resolve => { finish = resolve; });
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/register")) {
      await gate;
      return Response.json({ worker_id: worker, protocol_features: ["terminal_rejection_report"] });
    }
    return previousFetch(input, init);
  };
  const registering = f.c.register("test");
  try {
    assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), false);
  } finally { finish(); await registering; }
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), true);
});

test("registration identity is cleared before failed or invalid re-registration", async () => {
  const f = await fixture();
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), true);
  f.setRegistration(new Error("registration failed"));
  await assert.rejects(f.c.register("test"));
  assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), false);
  for (const worker_id of [undefined, "bad-worker", 7]) {
    f.setRegistration({ worker_id, protocol_features: ["terminal_rejection_report"] });
    await f.c.register("test");
    assert.equal(await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"), false);
  }
});

test("incomplete, empty, open, sibling and wrong-identity custody never grants authority", async () => {
  const f = await fixture();
  await f.c.claimRun();
  const closed = f.custody();
  for (const change of [
    { exact_holds: [], exact_count: 0, outcome: "unknown" },
    { exact_holds: [{ id: hold, state: "open" }], outcome: "retained" },
    { sibling_holds: [{ id: "44444444-4444-4444-8444-444444444444", generation: 2 }], sibling_count: 1, outcome: "unknown" },
    { complete: false }, { exact_complete: false }, { generation: 4 }, { worker_id: run }, { run_id: worker },
  ]) {
    f.setCustody({ ...closed, ...change });
    let allowed = false;
    try { allowed = await f.c.hasRecoveryRetirementAuthority(run, 3, "pending"); } catch {}
    assert.equal(allowed, false);
    assert.equal(f.c.knowsInventoryGuardedClaim(run, 3), true);
  }
});
