import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { Readable } from "node:stream";
import { nullLogger } from "./helpers.js";
import { WorkerClient, RequestError } from "../src/client.js";
import type { RecoveryReserveRequest, RecoveryUploadManifest } from "../src/protocol.js";

// PRD #1296 M3 — the worker→API archive RPC WIRE, pinned against M2's committed handler
// (api/internal/handler/handler.go route mounts + api/internal/handler/recovery.go).
//
// The four worker routes M2 mounts under /api/worker are:
//   POST /runs/{id}/archives/reserve            -> WorkerRecoveryReserve
//   POST /runs/{id}/archives/release            -> WorkerRecoveryRelease
//   POST /runs/{id}/archives/{captureID}/upload -> WorkerRecoveryUpload  (octet-stream body
//                                                  + X-Uzi-Recovery-Manifest header)
//   GET  /runs/{id}/archives/{captureID}        -> WorkerRecoveryStatus
// This test drives the client against a recording HTTP server and asserts the EXACT
// method + path + headers + body the handler expects, so a client/handler drift fails here.

const TOKEN = "worker-join-token-0123456789";
const RUN_ID = "11111111-2222-3333-4444-555555555555";
const CAPTURE_ID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

interface Recorded {
  method: string;
  url: string;
  headers: http.IncomingHttpHeaders;
  body: Buffer;
}

let server: http.Server;
let baseUrl: string;
let recorded: Recorded[];
let respond: (req: Recorded) => { status: number; body: string };

beforeEach(async () => {
  recorded = [];
  respond = () => ({ status: 200, body: "{}" });
  server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(c as Buffer));
    req.on("end", () => {
      const rec: Recorded = {
        method: req.method ?? "",
        url: req.url ?? "",
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      recorded.push(rec);
      const { status, body } = respond(rec);
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(body);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address() as AddressInfo;
  baseUrl = `http://127.0.0.1:${addr.port}`;
});

afterEach(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

function newClient(): WorkerClient {
  return new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    terminalRetrySchedule: [1, 1, 1],
  });
}

describe("recovery archive client wire (PRD #1296 M3 ↔ M2 handler)", () => {
  it("reserve → POST /api/worker/runs/{id}/archives/reserve with the JSON body", async () => {
    respond = () => ({ status: 200, body: JSON.stringify({ capture_id: CAPTURE_ID, state: "preparing" }) });
    const req: RecoveryReserveRequest = {
      run_id: RUN_ID,
      idempotency_key: "durable-key-1",
      source_sha: "1111111111111111111111111111111111111111",
      attempted_head_sha: "2222222222222222222222222222222222222222",
    };
    const res = await newClient().reserveRecoveryCapture(RUN_ID, req);
    assert.equal(res.capture_id, CAPTURE_ID);
    assert.equal(recorded.length, 1);
    assert.equal(recorded[0]!.method, "POST");
    assert.equal(recorded[0]!.url, `/api/worker/runs/${RUN_ID}/archives/reserve`);
    assert.match(String(recorded[0]!.headers.authorization), /^Bearer /);
    assert.deepEqual(JSON.parse(recorded[0]!.body.toString("utf8")), req);
  });

  it("reconcile uses worker bearer, exact archive route and contract body", async () => {
    const client = newClient();
    client.protocolFeatures = ["recovery_inventory_v1"];
    const req = { generation: 7, source_sha: "a".repeat(40), coverage_digest: "b".repeat(64),
      checksum: "c".repeat(64), byte_size: 14 };
    const response = { run_id: RUN_ID, generation: 7, capture_id: CAPTURE_ID, outcome: "replaceable" };
    respond = () => ({ status: 200, body: JSON.stringify(response) });
    assert.deepEqual(await client.reconcileRecoveryCapture(RUN_ID, CAPTURE_ID, req), response);
    assert.equal(recorded[0]!.method, "POST");
    assert.equal(recorded[0]!.url, `/api/worker/runs/${RUN_ID}/archives/${CAPTURE_ID}/reconcile`);
    assert.match(String(recorded[0]!.headers.authorization), /^Bearer /);
    assert.deepEqual(JSON.parse(recorded[0]!.body.toString("utf8")), req);
    client.protocolFeatures = [];
    await assert.rejects(client.reconcileRecoveryCapture(RUN_ID, CAPTURE_ID, req), /feature unavailable/);
    assert.equal(recorded.length, 1);
    client.protocolFeatures = ["recovery_inventory_v1"];
    for (const status of [401, 404, 500]) {
      respond = () => ({ status, body: '{"error":"unavailable"}' });
      await assert.rejects(client.reconcileRecoveryCapture(RUN_ID, CAPTURE_ID, req), RequestError);
    }
  });

  for (const contentLength of [undefined, "1"]) {
    it(`reconcile bounds streamed bytes with ${contentLength === undefined ? "absent" : "lying"} Content-Length`, async (t) => {
      const client = newClient();
      client.protocolFeatures = ["recovery_inventory_v1"];
      const req = { generation: 7, source_sha: "a".repeat(40), coverage_digest: "b".repeat(64),
        checksum: "c".repeat(64), byte_size: 14 };
      const receipt = { run_id: RUN_ID, generation: 7, capture_id: CAPTURE_ID, outcome: "replaceable" };
      // Valid JSON below 16 KiB in characters but above it in UTF-8 bytes.
      const text = JSON.stringify({ ...receipt, reason: "界".repeat(10_000) });
      assert.ok(text.length < 16 * 1024);
      const bytes = Buffer.from(text);
      assert.ok(bytes.length > 16 * 1024);
      let delivered = 0;
      let cancelled = false;
      let ended = false;
      const body = new ReadableStream<Uint8Array>({
        pull(controller) {
          if (delivered === bytes.length) {
            ended = true;
            controller.close();
            return;
          }
          const chunk = bytes.subarray(delivered, delivered + 4096);
          delivered += chunk.length;
          controller.enqueue(chunk);
        },
        cancel() { cancelled = true; },
      }, { highWaterMark: 0 });
      const headers = contentLength === undefined ? {} : { "Content-Length": contentLength };
      let response = new Response(body, { headers });
      const fetch = t.mock.method(globalThis, "fetch", async () => response);
      await assert.rejects(client.reconcileRecoveryCapture(RUN_ID, CAPTURE_ID, req), {
        name: "ResponseBodyOverflowError",
        message: "response body exceeds 16384 bytes",
      });
      assert.equal(cancelled, true);
      assert.equal(ended, false);
      assert.ok(delivered < bytes.length, "overflow must stop before draining the response");
      assert.equal(fetch.mock.callCount(), 1);

      // An exact-cap healthy receipt must still reach EOF and parse successfully.
      const healthy = Buffer.from(JSON.stringify(receipt).padEnd(16 * 1024, " "));
      let offset = 0;
      response = new Response(new ReadableStream<Uint8Array>({
        pull(controller) {
          if (offset === healthy.length) {
            controller.close();
            return;
          }
          const chunk = healthy.subarray(offset, offset + 4096);
          offset += chunk.length;
          controller.enqueue(chunk);
        },
      }, { highWaterMark: 0 }), { headers });
      assert.deepEqual(await client.reconcileRecoveryCapture(RUN_ID, CAPTURE_ID, req), receipt);
      assert.equal(offset, healthy.length);
      assert.equal(fetch.mock.callCount(), 2);
    });
  }

  it("status → GET /api/worker/runs/{id}/archives/{captureID}", async () => {
    respond = () => ({
      status: 200,
      body: JSON.stringify({ capture_id: CAPTURE_ID, state: "available", manifest_bound: true }),
    });
    const res = await newClient().getRecoveryCaptureStatus(RUN_ID, CAPTURE_ID);
    assert.equal(res.manifest_bound, true);
    assert.equal(recorded[0]!.method, "GET");
    assert.equal(recorded[0]!.url, `/api/worker/runs/${RUN_ID}/archives/${CAPTURE_ID}`);
  });

  it("upload → POST /archives/{captureID}/upload, octet-stream body + X-Uzi-Recovery-Manifest header", async () => {
    respond = () => ({
      status: 200,
      body: JSON.stringify({ capture_id: CAPTURE_ID, state: "available", manifest_bound: true }),
    });
    const bundle = Buffer.from("real-bundle-bytes-\u00ff\x00-payload");
    const manifest: RecoveryUploadManifest = {
      byte_size: bundle.length,
      checksum: "abc123",
      chunk_count: 1,
      prerequisite_shas: ["3333333333333333333333333333333333333333"],
    };
    const res = await newClient().uploadRecoveryBundle(RUN_ID, CAPTURE_ID, manifest, Readable.from(bundle));
    assert.equal(res.state, "available");
    assert.equal(recorded[0]!.method, "POST");
    assert.equal(recorded[0]!.url, `/api/worker/runs/${RUN_ID}/archives/${CAPTURE_ID}/upload`);
    assert.equal(recorded[0]!.headers["content-type"], "application/octet-stream");
    // The manifest rides the header as compact JSON — the bundle bytes are the pure body.
    assert.deepEqual(
      JSON.parse(String(recorded[0]!.headers["x-uzi-recovery-manifest"])),
      manifest,
    );
    assert.match(String(recorded[0]!.headers.authorization), /^Bearer /);
    // The body is the raw bundle, byte-for-byte — never wrapped in JSON (D6).
    assert.ok(recorded[0]!.body.equals(bundle));
  });

  it("release → POST /api/worker/runs/{id}/archives/release", async () => {
    respond = () => ({
      status: 200,
      body: JSON.stringify({ run_id: RUN_ID, released: true, holds_released: 1 }),
    });
    const res = await newClient().releaseRecoveryCustody(RUN_ID);
    assert.equal(res.holds_released, 1);
    assert.equal(recorded[0]!.method, "POST");
    assert.equal(recorded[0]!.url, `/api/worker/runs/${RUN_ID}/archives/release`);
  });

  it("a non-2xx upload throws (the caller keeps the source pinned and retries)", async () => {
    respond = () => ({ status: 507, body: JSON.stringify({ error: "storage quota exceeded" }) });
    await assert.rejects(
      () =>
        newClient().uploadRecoveryBundle(
          RUN_ID,
          CAPTURE_ID,
          { byte_size: 4, checksum: "x", chunk_count: 1 },
          Readable.from(Buffer.from("data")),
        ),
    );
  });
});
