import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";

import { RequestError, WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

// PRD #1909 M3: WorkerClient.downloadJobFile streams the worker input download.

const TOKEN = "worker-join-token-0123456789";
let server: http.Server | undefined;
afterEach(async () => {
  const s = server;
  server = undefined;
  if (s) {
    s.closeAllConnections();
    await new Promise<void>((r) => s.close(() => r()));
  }
});

async function serve(handler: http.RequestListener): Promise<WorkerClient> {
  server = http.createServer(handler);
  await new Promise<void>((r) => server!.listen(0, "127.0.0.1", r));
  const port = (server.address() as AddressInfo).port;
  return new WorkerClient(`http://127.0.0.1:${port}`, TOKEN, "0.1.0-test", nullLogger());
}

async function collect(body: AsyncIterable<Uint8Array>): Promise<Buffer> {
  const parts: Buffer[] = [];
  for await (const c of body) parts.push(Buffer.from(c));
  return Buffer.concat(parts);
}

describe("WorkerClient.downloadJobFile", () => {
  it("GETs /api/worker/runs/{id}/files/{fileID}?claim_generation=N with the bearer and streams the body to the sink", async () => {
    let seen: { url?: string; auth?: string } = {};
    const client = await serve((req, res) => {
      seen = { url: req.url, auth: req.headers.authorization };
      res.writeHead(200, { "Content-Type": "application/octet-stream", "Content-Length": "5", "X-Uzi-File-Sha256": "ab" });
      res.end("hello");
    });
    let got: Buffer = Buffer.alloc(0);
    await client.downloadJobFile("run-1", "file-1", 7, async (body) => {
      got = await collect(body);
    });
    assert.strictEqual(seen.url, "/api/worker/runs/run-1/files/file-1?claim_generation=7");
    assert.strictEqual(seen.auth, `Bearer ${TOKEN}`);
    assert.strictEqual(got.toString(), "hello");
  });

  it("throws RequestError with the status on a non-200 and never calls the sink", async () => {
    const client = await serve((_req, res) => {
      res.writeHead(409, { "Content-Type": "application/json" });
      res.end('{"disposition":"stale_claim"}');
    });
    let called = false;
    await assert.rejects(
      () => client.downloadJobFile("r", "f", 1, async () => { called = true; }),
      (err: unknown) => err instanceof RequestError && err.status === 409 && /stale_claim/.test(err.body),
    );
    assert.strictEqual(called, false);
  });

  it("a connection the server tears down mid-body rejects the stream: a truncated 200 never reads as complete", async () => {
    const client = await serve((_req, res) => {
      res.writeHead(200, { "Content-Length": "100" });
      res.write("only-some");
      setTimeout(() => res.destroy(), 10);
    });
    await assert.rejects(() => client.downloadJobFile("r", "f", 1, async (body) => { await collect(body); }));
  });

  it("destroys the response body when the sink throws, so the socket is released rather than held", async () => {
    let closed: Promise<void> = Promise.resolve();
    const client = await serve((req, res) => {
      closed = new Promise<void>((r) => req.socket.once("close", () => r()));
      res.writeHead(200, { "Content-Length": "1000000" });
      res.write("first-chunk");
      // The body is never finished: only the client hanging up can end this connection early.
    });
    await assert.rejects(
      () => client.downloadJobFile("r", "f", 1, async () => { throw new Error("sink refused"); }),
      /sink refused/,
    );
    await Promise.race([closed, new Promise((_, rej) => setTimeout(() => rej(new Error("socket still held after a sink error")), 3000))]);
  });

  it("aborts a body in flight when the caller's signal fires", async () => {
    const client = await serve((_req, res) => {
      res.writeHead(200, { "Content-Length": "1000000" });
      res.write("first-chunk");
    });
    const ac = new AbortController();
    const started = Date.now();
    await assert.rejects(() =>
      client.downloadJobFile("r", "f", 1, async (body) => {
        setTimeout(() => ac.abort(), 30);
        await collect(body);
      }, ac.signal),
    );
    assert.ok(Date.now() - started < 3000, "the abort ended the download promptly");
  });

  it("honours a per-call timeout shorter than the default", async () => {
    const client = await serve((_req, res) => {
      res.writeHead(200, { "Content-Length": "1000000" });
      res.write("x");
    });
    const started = Date.now();
    await assert.rejects(() => client.downloadJobFile("r", "f", 1, async (body) => { await collect(body); }, undefined, 50));
    assert.ok(Date.now() - started < 3000);
  });
});
