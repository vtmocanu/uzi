import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WorkerClient } from "../src/client.js";
import type { TerminalRejectionCustodyResponse } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const request = { rejections: [{ run_id: run, claim_generation: 3, reason: "mac_failure" as const }] };
const disposition = { dispositions: [{ ...request.rejections[0], disposition: "recorded" }] };
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });
function client() { return new WorkerClient("http://test.invalid", "join-token", "test", nullLogger(), { httpTimeoutMs: 500 }); }
function custody(): TerminalRejectionCustodyResponse {
  return { run_id: run, worker_id: worker, generation: 3, exact_holds: [{ id: hold, state: "released" }],
    sibling_holds: [], exact_count: 1, sibling_count: 0, exact_complete: true, sibling_complete: true, complete: true, outcome: "settled" };
}
function fakeResponse(body: unknown) { globalThis.fetch = async () => Response.json(body); }

test("missing/clear/shared-set/register replacement gates both transports", async () => {
  const c = client();
  let calls = 0;
  globalThis.fetch = async (_url, init) => {
    calls++;
    return Response.json(init?.method === "GET" ? custody() : disposition);
  };
  assert.equal(await c.reportTerminalRejections(request), undefined);
  assert.equal(await c.getTerminalRejectionCustody(run, 3, worker), undefined);
  assert.equal(calls, 0);
  c.protocolFeatures = ["terminal_rejection_report"];
  assert.deepEqual(await c.reportTerminalRejections(request), disposition);
  assert.deepEqual(await c.getTerminalRejectionCustody(run, 3, worker), custody());
  c.clearFeatures();
  assert.equal(await c.reportTerminalRejections(request), undefined);
  assert.equal(await c.getTerminalRejectionCustody(run, 3, worker), undefined);
  assert.equal(calls, 2);
  globalThis.fetch = async () => Response.json({ worker_id: worker, protocol_features: ["terminal_rejection_report"] });
  await c.register("test");
  assert.equal(c.hasFeature("terminal_rejection_report"), true);
  globalThis.fetch = async () => Response.json({ worker_id: worker });
  await c.register("test");
  assert.equal(await c.reportTerminalRejections(request), undefined);
  assert.equal(await c.getTerminalRejectionCustody(run, 3, worker), undefined);
});

test("shared feature gate is rechecked immediately before actual transport", async () => {
  const c = client();
  c.protocolFeatures = ["terminal_rejection_report"];
  let calls = 0;
  globalThis.fetch = async () => { calls++; return Response.json(disposition); };
  const race = { rejections: [{ get run_id() { c.clearFeatures(); return run; }, claim_generation: 3, reason: "mac_failure" as const }] };
  assert.equal(await c.reportTerminalRejections(race), undefined);
  assert.equal(calls, 0);
});

test("actual routes, payloads, identity and disposition validation", async () => {
  const c = client();
  c.protocolFeatures = ["terminal_rejection_report"];
  const calls: { url: string; init?: RequestInit }[] = [];
  globalThis.fetch = async (url, init) => { calls.push({ url: String(url), init }); return Response.json(init?.method === "GET" ? custody() : disposition); };
  await c.reportTerminalRejections(request);
  await c.getTerminalRejectionCustody(run, 3, worker);
  assert.equal(calls[0]!.url, "http://test.invalid/api/worker/terminal-rejections");
  assert.equal(calls[0]!.init!.method, "POST");
  assert.deepEqual(JSON.parse(calls[0]!.init!.body as string), request);
  assert.equal(calls[1]!.url, `http://test.invalid/api/worker/runs/${run}/terminal-rejection-custody?generation=3`);
  assert.equal(calls[1]!.init!.body, undefined);
  for (const body of [{ dispositions: [] }, { dispositions: [{ ...disposition.dispositions[0], run_id: worker }] },
    { dispositions: [{ ...disposition.dispositions[0], claim_generation: 4 }] },
    { dispositions: [{ ...disposition.dispositions[0], disposition: "approved" }] },
    { dispositions: [{ ...disposition.dispositions[0], reason: "malformed" }] }]) {
    fakeResponse(body);
    await assert.rejects(c.reportTerminalRejections(request), /invalid terminal rejection response/);
  }
  fakeResponse({ dispositions: [{ ...disposition.dispositions[0], disposition: "skipped" }] });
  assert.equal((await c.reportTerminalRejections(request))!.dispositions[0]!.disposition, "skipped");
  await assert.rejects(c.reportTerminalRejections({ rejections: Array(257).fill(request.rejections[0]) }));
  await assert.rejects(c.getTerminalRejectionCustody(run, Number.MAX_SAFE_INTEGER + 1, worker));
});

test("uppercase UUID requests normalize the report and custody wire identities", async () => {
  const c = client();
  c.protocolFeatures = ["terminal_rejection_report"];
  const lower = "abcdefab-1111-4111-8111-111111111111";
  const calls: { url: string; init?: RequestInit }[] = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url: String(url), init });
    return Response.json(init?.method === "GET"
      ? { ...custody(), run_id: lower }
      : { dispositions: [{ ...disposition.dispositions[0], run_id: lower }] });
  };
  await c.reportTerminalRejections({ rejections: [{ run_id: lower.toUpperCase(), claim_generation: 3, reason: "mac_failure" }] });
  await c.getTerminalRejectionCustody(lower.toUpperCase(), 3, worker);
  assert.equal(JSON.parse(calls[0]!.init!.body as string).rejections[0].run_id, lower);
  assert.ok(calls[1]!.url.includes(lower));
});

test("whole custody validates identities, safe counts, completeness, enums and duplicate hold IDs", async () => {
  const c = client();
  c.protocolFeatures = ["terminal_rejection_report"];
  const invalid = [
    { ...custody(), run_id: worker }, { ...custody(), worker_id: run }, { ...custody(), generation: 4 },
    { ...custody(), exact_count: 0 }, { ...custody(), exact_count: Number.MAX_SAFE_INTEGER + 1 },
    { ...custody(), exact_complete: false }, { ...custody(), complete: false },
    { ...custody(), outcome: "approved" }, { ...custody(), exact_holds: [{ id: hold, state: "unknown" }] },
    { ...custody(), exact_count: 2, exact_holds: [{ id: hold, state: "released" }, { id: hold, state: "discarded" }] },
    { ...custody(), exact_holds: [{ id: hold, state: "open" }] },
    { ...custody(), exact_count: 0, exact_holds: [] },
    { ...custody(), sibling_count: 1, sibling_holds: [{ id: worker, generation: 4 }] },
    { ...custody(), outcome: "unknown", sibling_count: 1, sibling_holds: [{ id: hold, generation: 4 }] },
    { ...custody(), outcome: "unknown", sibling_count: 1, sibling_holds: [{ id: worker, generation: 3 }] },
  ];
  for (const body of invalid) {
    fakeResponse(body);
    await assert.rejects(c.getTerminalRejectionCustody(run, 3, worker), /invalid terminal rejection response/);
  }
  fakeResponse({ ...custody(), outcome: "retained", exact_holds: [{ id: hold, state: "open" }] });
  assert.equal((await c.getTerminalRejectionCustody(run, 3, worker))!.outcome, "retained");
  fakeResponse({ ...custody(), exact_holds: [], exact_count: 0, outcome: "unknown" });
  assert.equal((await c.getTerminalRejectionCustody(run, 3, worker))!.outcome, "unknown");
});

test("incomplete capped inventories remain conservative and cannot say settled", async () => {
  const c = client();
  c.protocolFeatures = ["terminal_rejection_report"];
  const capped = { ...custody(), exact_count: 257, exact_complete: false, complete: false, outcome: "unknown",
    exact_holds: Array.from({ length: 256 }, (_, i) => ({ id: `${i.toString(16).padStart(8, "0")}-1111-4111-8111-111111111111`, state: "released" })) };
  fakeResponse(capped);
  assert.equal((await c.getTerminalRejectionCustody(run, 3, worker))!.outcome, "unknown");
  fakeResponse({ ...capped, outcome: "settled" });
  await assert.rejects(c.getTerminalRejectionCustody(run, 3, worker));
  fakeResponse({ ...capped, exact_holds: capped.exact_holds.slice(0, 255) });
  await assert.rejects(c.getTerminalRejectionCustody(run, 3, worker));
});

test("128 KiB actual streamed cap includes whitespace, ignores missing/lying length, cancels before JSON and errors never call text", async () => {
  const c = client();
  c.protocolFeatures = ["terminal_rejection_report"];
  for (const method of ["post", "get"] as const) {
    for (const header of [undefined, "1"]) {
      let cancelled = false;
      let pulls = 0;
      globalThis.fetch = async () => {
        const stream = new ReadableStream<Uint8Array>({
          pull(controller) { pulls++; controller.enqueue(new TextEncoder().encode(pulls === 1 ? JSON.stringify(method === "post" ? disposition : custody()) : " ".repeat(32 * 1024))); },
          cancel() { cancelled = true; },
        });
        const response = new Response(stream, { headers: header ? { "Content-Length": header } : undefined });
        Object.defineProperty(response, "text", { value: async () => { throw new Error("text must not be called"); } });
        return response;
      };
      await assert.rejects(method === "post" ? c.reportTerminalRejections(request) : c.getTerminalRejectionCustody(run, 3, worker), /invalid terminal rejection response/);
      assert.equal(cancelled, true);
      assert.ok(pulls <= 7);
    }
  }
  globalThis.fetch = async () => {
    const response = new Response("private error", { status: 500 });
    Object.defineProperty(response, "text", { value: async () => { throw new Error("text must not be called"); } });
    return response;
  };
  await assert.rejects(c.reportTerminalRejections(request), (err: any) => err.status === 500 && err.body === "");
});

test("exact byte cap accepted; stalled body is bounded by caller abort and request deadline", async () => {
  const c = client();
  c.protocolFeatures = ["terminal_rejection_report"];
  const json = JSON.stringify(disposition);
  globalThis.fetch = async () => new Response(json + " ".repeat(128 * 1024 - Buffer.byteLength(json)));
  assert.deepEqual(await c.reportTerminalRejections(request), disposition);
  let cancelled = false;
  globalThis.fetch = async () => new Response(new ReadableStream({ cancel() { cancelled = true; } }));
  await assert.rejects(c.reportTerminalRejections(request, undefined, 20));
  assert.equal(cancelled, true);
  const abort = new AbortController();
  const pending = c.getTerminalRejectionCustody(run, 3, worker, abort.signal);
  abort.abort();
  await assert.rejects(pending);
});

test("real HTTP whitespace stream exceeds byte cap without a content-length", async () => {
  const server = http.createServer((_req, res) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.write(JSON.stringify(disposition));
    res.end(" ".repeat(128 * 1024 + 1));
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  try {
    const c = new WorkerClient(`http://127.0.0.1:${(server.address() as AddressInfo).port}`, "join", "test", nullLogger(), { httpTimeoutMs: 500 });
    c.protocolFeatures = ["terminal_rejection_report"];
    await assert.rejects(c.reportTerminalRejections(request), /invalid terminal rejection response/);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) => server.close(err => err ? reject(err) : resolve()));
  }
});

test("custody enums reject array coercion in full and incomplete inventories", async () => {
  const c = client();
  c.protocolFeatures = ["terminal_rejection_report"];
  for (const incomplete of [false, true]) {
    const body = incomplete
      ? { ...custody(), exact_count: 257, exact_complete: false, complete: false, outcome: "unknown",
          exact_holds: Array.from({ length: 256 }, (_, i) => ({ id: `${i.toString(16).padStart(8, "0")}-1111-4111-8111-111111111111`, state: "released" })) }
      : custody();
    for (const malformed of [
      { ...body, outcome: [body.outcome] },
      { ...body, exact_holds: body.exact_holds.map((h, i) => i === 0 ? { ...h, state: [h.state] } : h) },
    ]) {
      fakeResponse(malformed);
      await assert.rejects(c.getTerminalRejectionCustody(run, 3, worker), /invalid terminal rejection response/);
    }
  }
});

test("streamed responses ignore zero-byte chunks before a valid response", async () => {
  const c = client();
  c.protocolFeatures = ["terminal_rejection_report"];
  globalThis.fetch = async () => new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      for (let i = 0; i < 1000; i++) controller.enqueue(new Uint8Array(0));
      controller.enqueue(new TextEncoder().encode(JSON.stringify(disposition)));
      controller.close();
    },
  }));
  assert.deepEqual(await c.reportTerminalRejections(request), disposition);
});
