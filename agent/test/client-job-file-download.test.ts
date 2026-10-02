import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";

import { JobFileTimeoutError, RequestError, WorkerClient } from "../src/client.js";
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

  // The torn body must be observed from creation: a sink that is slow to start reading must not
  // let the stream's 'error' fire with no listener (an uncaught exception, issue #2019).
  async function tornBeforeRead(sink: (body: import("node:stream").Readable) => Promise<void>): Promise<{ err: unknown; uncaught: unknown[]; restored: boolean }> {
    const client = await serve((req, res) => {
      res.writeHead(200, { "Content-Length": "100" });
      res.write("only");
      setTimeout(() => req.socket.destroy(), 5);
    });
    const before = process.listenerCount("uncaughtException");
    const uncaught: unknown[] = [];
    const probe = (e: unknown): void => { uncaught.push(e); };
    process.on("uncaughtException", probe);
    let err: unknown;
    try {
      await client.downloadJobFile("r", "f", 1, async (body) => {
        const deadline = Date.now() + 5000;
        while (!body.destroyed && Date.now() < deadline) await new Promise((r) => setTimeout(r, 2));
        assert.ok(body.destroyed, "body never torn");
        await new Promise((r) => setTimeout(r, 20)); // let a would-be uncaught error surface
        await sink(body);
      });
    } catch (e) {
      err = e;
    } finally {
      process.off("uncaughtException", probe);
    }
    return { err, uncaught, restored: process.listenerCount("uncaughtException") === before };
  }

  it("a body torn before the sink reads it rejects with the original error and never raises an uncaught exception", async () => {
    const r = await tornBeforeRead(async (body) => { await collect(body); });
    assert.ok(r.restored);
    assert.deepStrictEqual(r.uncaught, []);
    assert.ok(!(r.err instanceof JobFileTimeoutError));
    assert.match((r.err as Error).message, /terminated/);
  });

  it("a sink that returns without reading a torn body still rejects with the original error", async () => {
    const r = await tornBeforeRead(async () => {});
    assert.ok(r.restored);
    assert.deepStrictEqual(r.uncaught, []);
    assert.ok(!(r.err instanceof JobFileTimeoutError));
    assert.match((r.err as Error).message, /terminated/);
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

  it("surfaces a per-file timeout that fires mid-body as JobFileTimeoutError, not a generic stream error", async () => {
    const client = await serve((_req, res) => {
      res.writeHead(200, { "Content-Length": "1000000" });
      res.write("x");
    });
    await assert.rejects(
      () => client.downloadJobFile("r", "f", 1, async (body) => { await collect(body); }, undefined, 50),
      (err: unknown) => err instanceof JobFileTimeoutError,
    );
  });

  it("does not call a caller's own abort a timeout", async () => {
    const client = await serve((_req, res) => {
      res.writeHead(200, { "Content-Length": "1000000" });
      res.write("x");
    });
    const ac = new AbortController();
    await assert.rejects(
      () => client.downloadJobFile("r", "f", 1, async (body) => { setTimeout(() => ac.abort(), 20); await collect(body); }, ac.signal),
      (err: unknown) => !(err instanceof JobFileTimeoutError),
    );
  });

  it("reads at most 4096 bytes of a hostile error body, returns promptly and releases the connection", async () => {
    let closed: Promise<void> = Promise.resolve();
    const client = await serve((req, res) => {
      closed = new Promise<void>((r) => req.socket.once("close", () => r()));
      res.writeHead(500, { "Content-Type": "text/plain" });
      const chunk = "e".repeat(16 * 1024);
      const pump = (): void => {
        // An unending body, written only as fast as the client drains it.
        while (res.write(chunk)) {
          /* keep filling until backpressure */
        }
        res.once("drain", pump);
      };
      pump();
      res.on("close", () => res.removeAllListeners("drain"));
    });
    const started = Date.now();
    // A per-call timeout far longer than the assertion: only a bounded read can finish in time.
    await assert.rejects(
      () => client.downloadJobFile("r", "f", 1, async () => undefined, undefined, 20_000),
      (err: unknown) => err instanceof RequestError && err.status === 500 && err.body.length === 4096 && /^e+$/.test(err.body),
    );
    assert.ok(Date.now() - started < 3000, "the error read did not wait for the unending body");
    await Promise.race([closed, new Promise((_, rej) => setTimeout(() => rej(new Error("connection still held after the error read")), 3000))]);
  });

  it("keeps a small error body whole and trimmed", async () => {
    const client = await serve((_req, res) => {
      res.writeHead(500);
      res.end('  {"error":"boom"}\n');
    });
    await assert.rejects(
      () => client.downloadJobFile("r", "f", 1, async () => undefined),
      (err: unknown) => err instanceof RequestError && err.body === '{"error":"boom"}',
    );
  });
});
