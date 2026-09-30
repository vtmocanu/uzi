import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { Readable } from "node:stream";

import { RequestError, WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

// PRD #1909 M4: WorkerClient.uploadJobFile, the worker output upload.

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

async function readAll(req: http.IncomingMessage): Promise<Buffer> {
  const parts: Buffer[] = [];
  for await (const c of req) parts.push(Buffer.from(c as Uint8Array));
  return Buffer.concat(parts);
}

const META = { claim_generation: 7, display_name: "report.md", size: 5, sha256: "a".repeat(64) };
const DTO = {
  id: "f1",
  display_name: "report.md",
  storage_name: `${"a".repeat(64)}.md`,
  content_type: "text/markdown",
  byte_size: 5,
  sha256: "a".repeat(64),
  state: "attached",
  expires_at: null,
};

describe("WorkerClient.uploadJobFile", () => {
  it("POSTs the raw bytes with the bearer and the metadata in X-Uzi-Job-File; a Buffer body", async () => {
    let seen: { method?: string; url?: string; auth?: string; ct?: string; meta?: string; body?: Buffer } = {};
    const client = await serve(async (req, res) => {
      seen = {
        method: req.method,
        url: req.url,
        auth: req.headers.authorization,
        ct: req.headers["content-type"],
        meta: req.headers["x-uzi-job-file"] as string,
        body: await readAll(req),
      };
      res.writeHead(201, { "Content-Type": "application/json" });
      res.end(JSON.stringify(DTO));
    });
    const out = await client.uploadJobFile("run-1", META, Buffer.from("hello"));
    assert.strictEqual(out.status, 201);
    assert.strictEqual(out.file.id, "f1");
    assert.strictEqual(seen.method, "POST");
    assert.strictEqual(seen.url, "/api/worker/runs/run-1/files");
    assert.strictEqual(seen.auth, `Bearer ${TOKEN}`);
    assert.strictEqual(seen.ct, "application/octet-stream");
    assert.deepStrictEqual(JSON.parse(seen.meta!), META);
    assert.strictEqual(seen.body!.toString(), "hello");
  });

  it("streams a Readable factory body, and re-calls the factory for a second upload", async () => {
    const bodies: string[] = [];
    const client = await serve(async (req, res) => {
      bodies.push((await readAll(req)).toString());
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify(DTO));
    });
    let calls = 0;
    const factory = async (): Promise<Readable> => {
      calls++;
      return Readable.from([Buffer.from("he"), Buffer.from("llo")]);
    };
    assert.strictEqual((await client.uploadJobFile("r", META, factory)).status, 200);
    await client.uploadJobFile("r", META, factory);
    assert.strictEqual(calls, 2);
    assert.deepStrictEqual(bodies, ["hello", "hello"]);
  });

  it("escapes a non-ASCII display name so the header stays valid, and it decodes to the same name", async () => {
    let meta = "";
    const client = await serve(async (req, res) => {
      meta = req.headers["x-uzi-job-file"] as string;
      await readAll(req);
      res.writeHead(201, { "Content-Type": "application/json" });
      res.end(JSON.stringify(DTO));
    });
    const name = "résumé 報告 😀.md";
    await client.uploadJobFile("r", { ...META, display_name: name }, Buffer.from("hello"));
    assert.ok(/^[\x20-\x7e]*$/.test(meta), "the header is printable ASCII");
    assert.strictEqual(JSON.parse(meta).display_name, name);
  });

  it("throws RequestError with the status and a body read that is bounded", async () => {
    const client = await serve(async (req, res) => {
      await readAll(req);
      res.writeHead(413, { "Content-Type": "application/json" });
      res.write('{"reason":"file_too_large","error":"');
      res.end("x".repeat(64 * 1024) + '"}');
    });
    await assert.rejects(
      () => client.uploadJobFile("r", META, Buffer.from("hello")),
      (err: unknown) => err instanceof RequestError && err.status === 413 && /file_too_large/.test(err.body) && err.body.length <= 4096,
    );
  });

  it("exposes the response's Retry-After (whole seconds) on the RequestError, in milliseconds", async () => {
    for (const [header, want] of [["5", 5000], ["0", 0], ["Wed, 21 Oct 2026 07:28:00 GMT", undefined], ["-3", undefined], ["", undefined]] as const) {
      const client = await serve(async (req, res) => {
        await readAll(req);
        res.writeHead(503, { "Content-Type": "application/json", ...(header === "" ? {} : { "Retry-After": header }) });
        res.end('{"reason":"uploads_busy"}');
      });
      await assert.rejects(
        () => client.uploadJobFile("r", META, Buffer.from("hello")),
        (err: unknown) => err instanceof RequestError && err.status === 503 && err.retryAfterHeaderMs === want,
        `Retry-After ${JSON.stringify(header)}`,
      );
      const s = server!;
      server = undefined;
      s.closeAllConnections();
      await new Promise<void>((r) => s.close(() => r()));
    }
  });

  it("a timeout aborts the request", async () => {
    const client = await serve(() => {
      /* never answers */
    });
    await assert.rejects(() => client.uploadJobFile("r", META, Buffer.from("hello"), undefined, 50));
  });
});
