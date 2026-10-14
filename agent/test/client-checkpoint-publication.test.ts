import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { Readable } from "node:stream";
import { afterEach, describe, it } from "node:test";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

// Issue #2888: the checkpoint publication acknowledgment is read under a streamed-byte cap.
const CAP = 64 * 1024;
const MARKER = "SECRET-BODY-MARKER";
const TIP = "a".repeat(40);
const enc = new TextEncoder();

const realFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = realFetch; });

interface Source { stream: ReadableStream<Uint8Array>; cancelled: () => boolean; }

/** A stream of the given chunks that records whether its source cancel hook ran. */
function chunked(chunks: Uint8Array[]): Source {
  let i = 0, cancelled = false;
  const stream = new ReadableStream<Uint8Array>({
    pull(c) { if (i < chunks.length) c.enqueue(chunks[i++]!); else c.close(); },
    cancel() { cancelled = true; },
  }, { highWaterMark: 0 });
  return { stream, cancelled: () => cancelled };
}

/** Valid JSON `{"published":true,"pad":"..."}` padded with whitespace to exactly `bytes` bytes. */
function padded(bytes: number, extra = ""): Uint8Array {
  const head = `{"published":true${extra}}`;
  const body = head + " ".repeat(bytes - enc.encode(head).length);
  const out = enc.encode(body);
  assert.equal(out.length, bytes);
  return out;
}

function stub(make: (init: RequestInit) => Response): void {
  globalThis.fetch = (async (_input: unknown, init?: RequestInit) => make(init ?? {})) as typeof fetch;
}

function publish(boundary?: AbortSignal) {
  const client = new WorkerClient("http://ckpt.test", "tok", "test", nullLogger());
  return client.publishCheckpoint("run-1", TIP, Readable.from(["pack"]), boundary);
}

const overflow = { name: "ResponseBodyOverflowError" };

describe("checkpoint publication acknowledgment", () => {
  it("returns a small valid ack, a skipped ack and an empty 2xx as before", async () => {
    stub(() => Response.json({ published: true }));
    assert.deepEqual(await publish(), { ok: true, body: { published: true } });
    stub(() => Response.json({ published: false, skipped: "not_descendant" }));
    assert.deepEqual(await publish(), { ok: true, body: { published: false, skipped: "not_descendant" } });
    stub(() => new Response("", { status: 202 }));
    assert.deepEqual(await publish(), { ok: false, httpStatus: 202 });
  });

  it("accepts exactly the cap", async () => {
    const src = chunked([padded(CAP)]);
    stub(() => new Response(src.stream, { status: 200 }));
    assert.deepEqual(await publish(), { ok: true, body: { published: true } });
  });

  it("rejects one byte over the cap without echoing the body, and cancels the source", async () => {
    const src = chunked([padded(CAP + 1, `,"m":"${MARKER}"`)]);
    stub(() => new Response(src.stream, { status: 200 }));
    await assert.rejects(publish(), (err: Error) => {
      assert.equal(err.name, overflow.name);
      assert.ok(!err.message.includes(MARKER));
      return true;
    });
    assert.equal(src.cancelled(), true);
  });

  it("stops pulling an endless stream near the cap", async () => {
    let pulled = 0, cancelled = false;
    const chunk = new Uint8Array(16 * 1024).fill(0x20);
    const stream = new ReadableStream<Uint8Array>({
      pull(c) {
        if (pulled >= 4 * 1024 * 1024) { c.error(new Error("sentinel limit reached")); return; }
        pulled += chunk.length;
        c.enqueue(chunk);
      },
      cancel() { cancelled = true; },
    }, { highWaterMark: 0 });
    stub(() => new Response(stream, { status: 200 }));
    await assert.rejects(publish(), overflow);
    assert.equal(cancelled, true);
    assert.ok(pulled <= CAP + 16 * 1024, `pulled ${pulled}`);
  });

  it("rejects an over-cap chunked body from a real server with no Content-Length", async () => {
    const server: Server = createServer((_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.write(Buffer.alloc(CAP, 0x20));
      res.write(Buffer.alloc(1024, 0x20));
      res.end();
    });
    await new Promise<void>(r => server.listen(0, "127.0.0.1", r));
    try {
      const { port } = server.address() as AddressInfo;
      const client = new WorkerClient(`http://127.0.0.1:${port}`, "tok", "test", nullLogger());
      await assert.rejects(client.publishCheckpoint("run-1", TIP, Readable.from(["pack"])), overflow);
    } finally {
      server.closeAllConnections();
      await new Promise<void>(r => server.close(() => r()));
    }
  });

  it("counts streamed bytes, not Content-Length", async () => {
    const big = chunked([padded(CAP + 1)]);
    stub(() => new Response(big.stream, { status: 200, headers: { "Content-Length": "10" } }));
    await assert.rejects(publish(), overflow);
    const small = chunked([padded(100)]);
    stub(() => new Response(small.stream, { status: 200, headers: { "Content-Length": "999999999" } }));
    assert.deepEqual(await publish(), { ok: true, body: { published: true } });
  });

  describe("multibyte UTF-8", () => {
    const withText = (bytes: number, text: string) => padded(bytes, `,"t":"${text}"`);
    it("accepts exactly the cap and decodes the characters", async () => {
      const text = "ü\u{1F600}x";
      stub(() => new Response(chunked([withText(CAP, text)]).stream, { status: 200 }));
      assert.deepEqual(await publish(), { ok: true, body: { published: true, t: text } } as never);
    });
    it("rejects one byte over even when the character count is under the cap", async () => {
      const text = "ü".repeat(1000);
      const body = withText(CAP + 1, text);
      assert.ok(new TextDecoder().decode(body).length < CAP);
      stub(() => new Response(chunked([body]).stream, { status: 200 }));
      await assert.rejects(publish(), overflow);
    });
    it("decodes a code point split across two chunks", async () => {
      const text = "\u{1F600}";
      const bytes = withText(200, text);
      const at = bytes.indexOf(0xf0) + 2;
      stub(() => new Response(chunked([bytes.subarray(0, at), bytes.subarray(at)]).stream, { status: 200 }));
      assert.deepEqual(await publish(), { ok: true, body: { published: true, t: text } } as never);
    });
  });

  it("propagates a mid-read stream error unchanged", async () => {
    const err = new Error("stream broke");
    let sent = false;
    const stream = new ReadableStream<Uint8Array>({
      pull(c) { if (!sent) { sent = true; c.enqueue(enc.encode("{")); } else c.error(err); },
    }, { highWaterMark: 0 });
    stub(() => new Response(stream, { status: 200 }));
    await assert.rejects(publish(), (e: unknown) => e === err);
  });

  describe("source cancellation that never settles", () => {
    /** Fails the test instead of waiting for the runner timeout when `p` stays pending. */
    const settles = <T>(p: Promise<T>) => Promise.race([p, new Promise<never>((_, rej) =>
      setTimeout(() => rej(new Error("publishCheckpoint did not settle")), 2000).unref())]);
    function hanging(chunks: Uint8Array[], cancel: () => Promise<void>): Source {
      let i = 0, cancelled = false;
      const stream = new ReadableStream<Uint8Array>({
        pull(c) { if (i < chunks.length) c.enqueue(chunks[i++]!); else return new Promise<void>(() => undefined); },
        cancel() { cancelled = true; return cancel(); },
      }, { highWaterMark: 0 });
      return { stream, cancelled: () => cancelled };
    }
    it("still rejects an over-cap body and invokes cancel", async () => {
      const src = hanging([padded(CAP + 1)], () => new Promise<void>(() => undefined));
      stub(() => new Response(src.stream, { status: 200 }));
      await assert.rejects(settles(publish()), overflow);
      assert.equal(src.cancelled(), true);
    });
    it("still rejects a boundary abort", async () => {
      const src = hanging([], () => new Promise<void>(() => undefined));
      const boundary = new AbortController();
      stub(() => new Response(src.stream, { status: 200 }));
      const pending = settles(publish(boundary.signal));
      setTimeout(() => boundary.abort(), 20);
      await assert.rejects(pending, { name: "AbortError" });
      assert.equal(src.cancelled(), true);
    });
    it("keeps the original error when cancel rejects", async () => {
      const src = hanging([padded(CAP + 1)], () => Promise.reject(new Error("cancel failed")));
      stub(() => new Response(src.stream, { status: 200 }));
      await assert.rejects(settles(publish()), overflow);
      assert.equal(src.cancelled(), true);
    });
  });

  it("aborts a stalled read when the boundary signal fires and cancels the source", async () => {
    let cancelled = false;
    const stream = new ReadableStream<Uint8Array>({
      pull: () => new Promise<void>(() => undefined),
      cancel() { cancelled = true; },
    }, { highWaterMark: 0 });
    const boundary = new AbortController();
    stub(() => new Response(stream, { status: 200 }));
    const pending = publish(boundary.signal);
    setTimeout(() => boundary.abort(), 20);
    await assert.rejects(pending, { name: "AbortError" });
    assert.equal(cancelled, true);
  });
});
