import assert from "node:assert/strict";
import { it } from "node:test";
import { WorkerClient, RequestError, isRunOwnershipLost } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const RUN = "run-1";
const PATH = "/api/worker/runs/run-1/ownership";
const JSON_BODY = '{"error":"run not found for this worker"}';
type Mode = "no signal" | "caller signal" | "deadline only";
const modes: Mode[] = ["no signal", "caller signal", "deadline only"];
const padded = (n: number) => JSON_BODY + " ".repeat(n - JSON_BODY.length);
function client(fetchImpl: typeof fetch) {
  return new WorkerClient("http://ownership.test", "fixture", "test", nullLogger(), { fetch: fetchImpl });
}
async function probe(body: string | ReadableStream<Uint8Array>, mode: Mode): Promise<unknown> {
  const c = client(async () => new Response(body, { status: 404, headers: { "Retry-After": "7" } }));
  return c.getRunOwnership(RUN, mode === "no signal" ? undefined :
    mode === "caller signal" ? new AbortController().signal : { deadline: Date.now() + 5000 })
    .then(() => assert.fail("expected error"), (err: unknown) => err);
}

it("regression no signal oversized whitespace is not ownership loss", async () => {
  const err = await probe(padded(1025), "no signal");
  assert.equal(isRunOwnershipLost(err, RUN), false, "oversized whitespace must not produce lost=true");
});
it("regression no signal truncated JSON prefix is not ownership loss", async () => {
  const err = await probe(padded(4096) + "garbage", "no signal");
  assert.equal(isRunOwnershipLost(err, RUN), false, "truncated JSON prefix must not produce lost=true");
});

for (const mode of modes) {
  for (const [name, body, lost] of [
    ["Go JSON newline", JSON_BODY + "\n", true],
    ["exactly 1024 bytes", padded(1024), true],
    ["1025 bytes", padded(1025), false],
    ["oversized padding", padded(2048), false],
  ] as const) {
    it(`${mode}: ${name}`, async () => {
      const err = await probe(body, mode);
      assert.ok(err instanceof RequestError);
      assert.equal(isRunOwnershipLost(err, RUN), lost);
      assert.deepEqual(err.bodyEvidence, { complete: true, bytes: Buffer.byteLength(body) });
    });
  }
  it(`${mode}: capped diagnostic cannot authorize loss`, async () => {
    const body = padded(4096) + "garbage";
    const err = await probe(body, mode);
    assert.ok(err instanceof RequestError);
    assert.equal(isRunOwnershipLost(err, RUN), false);
    assert.deepEqual(err.bodyEvidence, { complete: mode !== "no signal", bytes: mode === "no signal" ? 4096 : body.length });
    assert.equal(err.body, mode === "no signal" ? JSON_BODY : body.slice(0, 4096));
    assert.equal(err.retryAfterHeaderMs, 7000);
  });
  it(`${mode}: raw UTF-8 byte count and diagnostic slicing`, async () => {
    const body = "  " + "é".repeat(2200);
    const err = await probe(body, mode);
    assert.ok(err instanceof RequestError);
    assert.deepEqual(err.bodyEvidence, { complete: mode !== "no signal", bytes: mode === "no signal" ? 4096 : Buffer.byteLength(body) });
    assert.equal(err.body, mode === "no signal" ? "é".repeat(2047) : body.slice(0, 4096));
    assert.equal(isRunOwnershipLost(err, RUN), false);
  });
  it(`${mode}: failed body read is unknown`, async () => {
    const failure = new Error("fixture connection reset");
    const err = await probe(new ReadableStream<Uint8Array>({
      start(c) { c.enqueue(new TextEncoder().encode(JSON_BODY)); },
      pull() { throw failure; },
    }), mode);
    assert.equal(isRunOwnershipLost(err, RUN), false);
    if (mode === "no signal") {
      assert.ok(err instanceof RequestError);
      assert.deepEqual(err.bodyEvidence, { complete: false, bytes: 0 });
    } else assert.equal(err, failure);
  });
  if (mode !== "no signal") {
    it(`${mode}: exact 16KiB EOF supplies complete evidence`, async () => {
      const err = await probe(padded(16384), mode);
      assert.ok(err instanceof RequestError);
      assert.deepEqual(err.bodyEvidence, { complete: true, bytes: 16384 });
      assert.equal(isRunOwnershipLost(err, RUN), false);
    });
    it(`${mode}: over 16KiB propagates overflow`, async () => {
      const err = await probe(padded(16385), mode);
      assert.ok(err instanceof Error);
      assert.equal(err.name, "ResponseBodyOverflowError");
      assert.equal(isRunOwnershipLost(err, RUN), false);
    });
  }
}

it("strict classifier rejects unknown evidence and other routes or bodies", () => {
  const evidence = { complete: true, bytes: JSON_BODY.length };
  assert.equal(isRunOwnershipLost(new RequestError("GET", PATH, 404, JSON_BODY, undefined, evidence), RUN), true);
  for (const e of [undefined, { complete: false, bytes: JSON_BODY.length }, { complete: true, bytes: 1025 }]) {
    assert.equal(isRunOwnershipLost(new RequestError("GET", PATH, 404, JSON_BODY, undefined, e), RUN), false);
  }
  for (const [method, path, status, body] of [
    ["POST", PATH, 404, JSON_BODY], ["GET", PATH, 403, JSON_BODY],
    ["GET", PATH.replace(RUN, "run-2"), 404, JSON_BODY],
    ["GET", PATH + "?purpose=code_snapshot", 404, JSON_BODY],
    ["GET", PATH + "/", 404, JSON_BODY],
    ...["{", "null", "[]", '"text"', "42", '{"error":"other"}',
      '{"error":"run not found for this worker","reason":"run_not_owned"}',
      '{"error":"run not found for this worker","extra":true}',
      " ".repeat(1025) + JSON_BODY].map(body => ["GET", PATH, 404, body]),
  ] as [string, string, number, string][]) {
    assert.equal(isRunOwnershipLost(new RequestError(method, path, status, body, undefined, evidence), RUN), false);
  }
  assert.equal(isRunOwnershipLost(new Error(JSON_BODY), RUN), false);
});

for (const mode of ["caller abort", "deadline expiry"] as const) {
  it(`${mode}: stalled error body cancels promptly`, async (t) => {
    const beforeTimers = process.getActiveResourcesInfo().filter(r => r === "Timeout").length;
    const controller = new AbortController();
    let ready!: () => void;
    const reading = new Promise<void>(resolve => { ready = resolve; });
    let cancelled = false;
    const c = client(async () => new Response(new ReadableStream<Uint8Array>({
      pull() { ready(); },
      cancel() { cancelled = true; },
    }), { status: 404 }));
    const started = performance.now();
    // The watchdog bounds both readiness and rejection and is always cleared.
    let watchdog!: ReturnType<typeof setTimeout>;
    const timeout = new Promise<never>((_, reject) => {
      watchdog = setTimeout(() => reject(new Error("body-read watchdog expired")), 2000);
    });
    try {
      const result = c.getRunOwnership(RUN, mode === "caller abort" ? controller.signal : { deadline: Date.now() + 100 })
        .then(() => assert.fail("expected cancellation"), (err: unknown) => err);
      await Promise.race([reading, timeout]);
      if (mode === "caller abort") controller.abort(new Error("fixture caller abort"));
      const err = await Promise.race([result, timeout]);
      assert.ok(err instanceof Error);
      assert.equal(err.name, mode === "caller abort" ? "Error" : "TimeoutError");
      if (mode === "caller abort") assert.equal(err, controller.signal.reason);
      assert.equal(isRunOwnershipLost(err, RUN), false);
      assert.equal(cancelled, true);
      assert.ok(performance.now() - started < 1500);
    } finally { clearTimeout(watchdog); }
    assert.ok(process.getActiveResourcesInfo().filter(r => r === "Timeout").length <= beforeTimers,
      "no referenced timer remains after clearing watchdog");
    t.diagnostic(JSON.stringify({ runtime: process.version, platform: process.platform, arch: process.arch,
      elapsedMs: performance.now() - started, resources: process.getActiveResourcesInfo() }));
  });
}
