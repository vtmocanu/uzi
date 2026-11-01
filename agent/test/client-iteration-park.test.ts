import assert from "node:assert/strict";
import { it } from "node:test";
import { setImmediate as barrier } from "node:timers/promises";
import { WorkerClient } from "../src/client.js";
import type { IterationParkRequest } from "../src/protocol.js";
import { makeClaim, nullLogger } from "./helpers.js";

const evidence: IterationParkRequest = {
  claim_generation: 7, iteration_count: 10, max_iterations: 10,
  head: "a".repeat(40), published: false, checkpoint_contains_latest: false,
};
const parked = { applied: true, status: "paused", claim_generation: 7, hold_reason: "iteration_budget_exhausted" };
const refused = { applied: false, status: "running", claim_generation: 7, hold_reason: "", reason: "prior_attempt" };
function client(fetch: typeof globalThis.fetch, httpTimeoutMs = 1000) {
  return new WorkerClient("https://api.invalid", "fixture-worker", "test", nullLogger(), { fetch, httpTimeoutMs });
}

it("sends exactly one dedicated fenced request without feature negotiation", async () => {
  const sent: RequestInit[] = [];
  const worker = client(async (url, init) => {
    assert.equal(String(url), "https://api.invalid/api/worker/runs/run%2Fid/iteration-park");
    sent.push(init!);
    return Response.json(parked);
  });
  assert.deepEqual(await worker.reportIterationPark("run/id", evidence), { outcome: "parked" });
  assert.equal(sent.length, 1);
  assert.equal(sent[0]!.method, "POST");
  assert.deepEqual(JSON.parse(String(sent[0]!.body)), evidence);
});

it("accepts only confirmed matching-generation parks and typed refusal evidence", async () => {
  for (const reason of ["prior_attempt", "ineligible_lane", "not_running", "allowance_mismatch",
    "stale_claim", "released_claim", "pending_control"]) {
    const worker = client(async () => Response.json({ ...refused, reason, claim_generation: 8 }, { status: 409 }));
    assert.deepEqual(await worker.reportIterationPark("run", evidence),
      { outcome: "refused", reason, status: "running", claimGeneration: 8 });
  }
  const cases: [number, unknown][] = [
    [200, { ...parked, claim_generation: 8 }], [200, { ...parked, applied: false }],
    [200, { ...parked, status: "running" }], [200, { ...parked, hold_reason: "budget_exhausted" }],
    [200, { ...parked, claim_generation: undefined }], [200, { run: parked }], [200, null], [409, { ...refused, reason: "future" }],
    [409, { ...refused, applied: true }], [409, { ...refused, status: "future" }],
    [409, { ...refused, reason: undefined }], [409, { ...refused, claim_generation: "7" }],
    [409, { ...refused, claim_generation: 0 }], [409, { ...refused, hold_reason: null }],
    [400, parked], [404, parked], [500, parked], [201, parked], [202, parked],
  ];
  for (const [status, body] of cases) {
    let calls = 0;
    const worker = client(async () => { calls++; return Response.json(body, { status }); });
    assert.deepEqual(await worker.reportIterationPark("run", evidence), { outcome: "unknown" });
    assert.equal(calls, 1, "no stripped generation fallback or transport retry");
  }
});

it("rejects invalid evidence before HTTP, including UTF8 and int32 boundaries", async () => {
  let calls = 0;
  const worker = client(async () => { calls++; return Response.json(parked); });
  const invalid = [
    ...["claim_generation", "iteration_count", "max_iterations"].flatMap(key =>
      [undefined, null, 0, -1, 1.5, NaN, Infinity, key === "claim_generation" ? Number.MAX_SAFE_INTEGER + 1 : 2147483648, "10"].map(value => ({ ...evidence, [key]: value }))),
    { ...evidence, iteration_count: 9 }, { ...evidence, iteration_count: 12 },
    { ...evidence, head: "é".repeat(65) }, { ...evidence, head: "\ud800" },
    { ...evidence, head: null }, { ...evidence, published: undefined },
    { ...evidence, published: 1 }, { ...evidence, checkpoint_contains_latest: null },
    { ...evidence, checkpoint_contains_latest: "false" },
    { ...evidence, extra: "x".repeat(16 * 1024) }, { ...evidence, extra: true },
    { ...evidence, head: "\udc00" },
    { ...evidence, toJSON: () => ({ ...evidence, claim_generation: undefined }) },
  ];
  for (const body of invalid)
    await assert.rejects(worker.reportIterationPark("run", body as IterationParkRequest), /invalid iteration park evidence/);
  assert.equal(calls, 0);
  for (const body of [
    { ...evidence, head: "é".repeat(64), iteration_count: 11 },
    { ...evidence, claim_generation: 2147483648 },
    { ...evidence, claim_generation: Number.MAX_SAFE_INTEGER },
    { ...evidence, head: "", max_iterations: 2147483647, iteration_count: 2147483647, claim_generation: 2147483647 },
    { ...evidence, max_iterations: 2147483646, iteration_count: 2147483647 },
  ]) {
    assert.deepEqual(await worker.reportIterationPark("run", body),
      body.claim_generation === 7 ? { outcome: "parked" } : { outcome: "unknown" });
  }
  assert.equal(calls, 5);
});

it("caps actual streamed bytes before parsing success, refusal and error bodies on every status", async () => {
  for (const status of [200, 409, 400, 404, 500, 202]) {
    for (const contentLength of [undefined, "1", "99999999"]) {
      const bytes = Buffer.alloc(128 * 1024, " ");
      bytes.set(Buffer.from(JSON.stringify(status === 409 ? refused : parked)));
      let consumed = 0;
      let cancelled = false;
      const response = new Response(new ReadableStream<Uint8Array>({
        pull(controller) {
          if (consumed === bytes.length) { controller.close(); return; }
          controller.enqueue(bytes.subarray(consumed, consumed + 1024));
          consumed += 1024;
        },
        cancel() { cancelled = true; },
      }, { highWaterMark: 0 }), { status, headers: contentLength ? { "Content-Length": contentLength } : {} });
      Object.defineProperty(response, "text", { value: async () => { assert.fail("unbounded text must not be used"); } });
      const worker = client(async () => response);
      assert.deepEqual(await worker.reportIterationPark("run", evidence), { outcome: "unknown" });
      assert.equal(consumed, 16 * 1024 + 1024);
      assert.equal(cancelled, true);
    }
  }
});

it("accepts exactly 16KiB and rejects an oversized single chunk and invalid UTF8", async () => {
  const exact = JSON.stringify(parked).padEnd(16 * 1024, " ");
  for (const body of [exact, exact + " ", Buffer.from([0xff])]) {
    const worker = client(async () => new Response(body));
    assert.deepEqual(await worker.reportIterationPark("run", evidence),
      body === exact ? { outcome: "parked" } : { outcome: "unknown" });
  }
});

it("maps transport, unreadable, malformed and timed-out bodies to unknown", async () => {
  const workers = [
    client(async () => { throw new Error("transport fixture"); }),
    client(async () => new Response(new ReadableStream({ start(c) { c.error(new Error("read fixture")); } }))),
    client(async () => new Response("{invalid")),
    client(async () => new Response(new ReadableStream({ pull() { return new Promise(() => {}); } })), 10),
    client(async (_url, init) => new Promise((_resolve, reject) => {
      init!.signal!.addEventListener("abort", () => reject(new Error("timeout fixture")), { once: true });
    }), 10),
  ];
  // AbortSignal.timeout is unref'ed; retain a handle while deliberately waiting on mock IO.
  const keepAlive = setInterval(() => {}, 1000);
  try {
    for (const worker of workers)
      assert.deepEqual(await worker.reportIterationPark("run", evidence), { outcome: "unknown" });
  } finally { clearInterval(keepAlive); }
});

it("retires matching Codex admission authority before sending the park", async () => {
  const claim = makeClaim({ claim_generation: 7, secrets: { forge_pat: "fixture-pat", codex: {
    auth_mode: "subscription", access_token: "fixture-access", capability: "fixture-capability",
    generation: 3, chatgpt_account_id: "fixture-account", chatgpt_plan_type: null,
  } } });
  let parkCalls = 0;
  let admissionCalls = 0;
  const worker = client(async (url) => {
    const path = String(url);
    if (path.includes("/runs/claim")) return Response.json(claim);
    if (path.endsWith("/iteration-park")) {
      parkCalls++;
      assert.equal(worker.codexAdmissionReporter(claim.run_id, 7), undefined);
      return Response.json(refused, { status: 409 });
    }
    admissionCalls++;
    return new Response(null, { status: 204 });
  });
  worker.protocolFeatures = ["codex_account_admission_signal_v1", "claim_generation_fence"];
  await worker.claimRun();
  const reporter = worker.codexAdmissionReporter(claim.run_id, 7);
  assert.ok(reporter);
  await worker.reportIterationPark(claim.run_id, evidence);
  const attempt = reporter.begin("thread");
  attempt.bind("turn");
  attempt.observe({ kind: "codex_error", method: "error", threadId: "thread", turnId: "turn", willRetry: true,
    params: { threadId: "thread", turnId: "turn", error: { codexErrorInfo: "rateLimitExceeded" } } });
  await barrier();
  assert.equal(parkCalls, 1);
  assert.equal(admissionCalls, 0);
});
