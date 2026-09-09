import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough, Writable, type Readable } from "node:stream";

import {
  createCodexTransport,
  CodexTransportError,
  type CodexNotification,
  type CodexTransport,
} from "../src/codex/transport.js";

// PRD #1171 (M3, plan §2.1) — the app-server JSON-RPC-over-stdio transport, driven with
// in-memory duplex pipes (no real Codex binary). The framing/vocabulary mirrored here is
// the frozen M0 characterization: newline-delimited `{id,method,params}` requests and
// `{id,result}`/`{id,error}` responses (e2e/codex-m0/harness.mjs:314-317, :292-297) and
// id-less `{method,params}` notifications (harness.mjs:273-276).

function tick(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}

/** Let queued stream `data` events drain into the transport before we inspect it. */
async function flush(): Promise<void> {
  for (let i = 0; i < 3; i++) await tick();
}

/** Poll until `fn` yields a value, letting the event loop flush stream data events. */
async function waitFor<T>(fn: () => T | undefined, label: string): Promise<T> {
  for (let i = 0; i < 2000; i++) {
    const v = fn();
    if (v !== undefined) return v;
    await tick();
  }
  throw new Error(`waitFor timed out: ${label}`);
}

/** Collect newline-delimited frames written to a readable (the outbound side). */
function collectFrames(stream: Readable): () => Array<Record<string, unknown>> {
  let buf = "";
  const frames: Array<Record<string, unknown>> = [];
  stream.setEncoding("utf8");
  stream.on("data", (chunk: string) => {
    buf += chunk;
    let nl = buf.indexOf("\n");
    while (nl !== -1) {
      const line = buf.slice(0, nl);
      buf = buf.slice(nl + 1);
      if (line.trim().length > 0) frames.push(JSON.parse(line) as Record<string, unknown>);
      nl = buf.indexOf("\n");
    }
  });
  return () => frames;
}

function makePair(
  opts: {
    maxFrameBytes?: number;
    maxOutboundFrames?: number;
    maxOutboundFrameBytes?: number;
    maxOutboundBytes?: number;
    maxInboundNotifications?: number;
    maxInboundBytes?: number;
  } = {},
): {
  inbound: PassThrough;
  outbound: PassThrough;
  transport: CodexTransport;
} {
  const inbound = new PassThrough();
  const outbound = new PassThrough();
  const transport = createCodexTransport({ inbound, outbound, ...opts });
  return { inbound, outbound, transport };
}

function writeFrame(inbound: PassThrough, value: unknown): void {
  inbound.write(`${JSON.stringify(value)}\n`);
}

describe("codex transport: request/response correlation", () => {
  it("matches out-of-order responses to the right pending request by id", async () => {
    const { inbound, outbound, transport } = makePair();
    const frames = collectFrames(outbound);

    const p1 = transport.request("thread/start", { model: "gpt-5-codex" });
    const p2 = transport.request("turn/start", { threadId: "th-1" });

    const sent = await waitFor(() => (frames().length >= 2 ? frames() : undefined), "two outbound frames");
    const id1 = sent[0]!.id as number;
    const id2 = sent[1]!.id as number;
    assert.equal(sent[0]!.method, "thread/start");
    assert.equal(sent[1]!.method, "turn/start");
    assert.notEqual(id1, id2);
    assert.equal(typeof id1, "number");

    // Respond to the SECOND request first — correlation is by id, not arrival order.
    writeFrame(inbound, { id: id2, result: { turn: { id: "tn-1" } } });
    writeFrame(inbound, { id: id1, result: { thread: { id: "th-1" } } });

    const [r1, r2] = await Promise.all([p1, p2]);
    assert.deepEqual(r1, { thread: { id: "th-1" } });
    assert.deepEqual(r2, { turn: { id: "tn-1" } });

    await transport.close();
  });

  it("rejects with a typed protocol error when the app-server returns a JSON-RPC error", async () => {
    const { inbound, outbound, transport } = makePair();
    const frames = collectFrames(outbound);
    const p = transport.request("thread/start", {});
    const sent = await waitFor(() => (frames().length >= 1 ? frames() : undefined), "one outbound frame");
    // Mirrors harness.mjs:285 `{ code, message }`; the message must NOT leak into ours.
    writeFrame(inbound, { id: sent[0]!.id, error: { code: -32601, message: "secret-provider-detail" } });
    await assert.rejects(p, (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /code -32601/);
      assert.doesNotMatch(err.message, /secret-provider-detail/);
      return true;
    });
    await transport.close();
  });
});

describe("codex transport: notification decoding", () => {
  it("decodes known lifecycle methods and surfaces an unknown method as activity", async () => {
    const { inbound, transport } = makePair();
    const notes = transport.notifications();

    writeFrame(inbound, { method: "thread/started", params: { thread: { id: "th-9" } } });
    writeFrame(inbound, { method: "turn/started", params: { threadId: "th-9", turn: { id: "tn-9" } } });
    // An UNKNOWN method (a Codex item stream update) is liveness, never dropped or thrown.
    writeFrame(inbound, { method: "item/agent_message_delta", params: { delta: "hello" } });

    const n1 = await notes.next();
    assert.equal(n1.done, false);
    const v1 = n1.value as CodexNotification;
    assert.equal(v1.kind, "thread_started");
    assert.equal(v1.kind === "thread_started" ? v1.threadId : undefined, "th-9");

    const n2 = await notes.next();
    const v2 = n2.value as CodexNotification;
    assert.equal(v2.kind, "turn_started");
    if (v2.kind === "turn_started") {
      assert.equal(v2.threadId, "th-9");
      assert.equal(v2.turnId, "tn-9");
    }

    const n3 = await notes.next();
    const v3 = n3.value as CodexNotification;
    assert.equal(v3.kind, "activity");
    assert.equal(v3.kind === "activity" ? v3.method : undefined, "item/agent_message_delta");

    await transport.close();
  });

  it("surfaces a server-initiated request (method + id) as activity carrying its requestId", async () => {
    const { inbound, transport } = makePair();
    const notes = transport.notifications();
    // harness.mjs:277-291: a frame with BOTH method and id is a server→client request.
    writeFrame(inbound, { id: 42, method: "thread/tool/requestApproval", params: { callId: "c1" } });
    const n = await notes.next();
    const v = n.value as CodexNotification;
    assert.equal(v.kind, "activity");
    if (v.kind === "activity") {
      assert.equal(v.method, "thread/tool/requestApproval");
      assert.equal(v.requestId, 42);
    }
    await transport.close();
  });
});

describe("codex transport: server-request interceptor", () => {
  it("claims an auth request synchronously before the notification queue", async () => {
    const { inbound, outbound, transport } = makePair();
    const frames = collectFrames(outbound);
    const intercepted: CodexNotification[] = [];
    transport.installServerRequestInterceptor!((note) => {
      if (note.kind !== "activity" || note.method !== "account/chatgptAuthTokens/refresh") return false;
      intercepted.push(note);
      transport.respond(note.requestId!, { result: { handled: true } });
      return true;
    });
    const notes = transport.notifications();

    // One chunk deliberately carries a claimed auth request followed by ordinary
    // liveness. The claimed request must be answered directly and never occupy the
    // single-consumer queue that setup has not started draining yet.
    inbound.write(
      `${JSON.stringify({ id: 73, method: "account/chatgptAuthTokens/refresh", params: { reason: "unauthorized" } })}\n`
      + `${JSON.stringify({ method: "other/activity", params: {} })}\n`,
    );
    await flush();

    assert.equal(intercepted.length, 1);
    assert.equal(intercepted[0]?.kind, "activity");
    if (intercepted[0]?.kind === "activity") assert.equal(intercepted[0].requestId, 73);
    assert.deepEqual(frames(), [{ id: 73, result: { handled: true } }]);
    const queued = await notes.next();
    assert.equal(queued.done, false);
    assert.equal(queued.value?.kind, "activity");
    if (queued.value?.kind === "activity") assert.equal(queued.value.method, "other/activity");
    await transport.close();
  });
});

describe("codex transport: respond (server→client reply lane)", () => {
  it("frames a success reply as {id, result} on the outbound stream", async () => {
    const { outbound, transport } = makePair();
    const frames = collectFrames(outbound);
    // A success reply for a server→client request (e.g. item/tool/call) mirrors the M0
    // client reply `{ id, result }` (harness.mjs:291).
    const result = { success: true, contentItems: [{ type: "inputText", text: "ok" }] };
    transport.respond(42, { result });
    const sent = await waitFor(() => (frames().length >= 1 ? frames() : undefined), "one reply frame");
    assert.equal(sent[0]!.id, 42);
    assert.deepEqual(sent[0]!.result, result);
    // A reply is correlated by id; it must NOT carry a method (else a peer treats it as a
    // fresh notification), and a success reply carries no error.
    assert.equal("method" in sent[0]!, false);
    assert.equal("error" in sent[0]!, false);
    await transport.close();
  });

  it("frames an error reply as {id, error} on the outbound stream, preserving a string id", async () => {
    const { outbound, transport } = makePair();
    const frames = collectFrames(outbound);
    // An error reply mirrors the M0 client error reply `{ id, error }` (harness.mjs:285).
    const error = { code: -32601, message: "unsupported request" };
    transport.respond("call-1", { error });
    const sent = await waitFor(() => (frames().length >= 1 ? frames() : undefined), "one reply frame");
    assert.equal(sent[0]!.id, "call-1");
    assert.deepEqual(sent[0]!.error, error);
    assert.equal("method" in sent[0]!, false);
    assert.equal("result" in sent[0]!, false);
    await transport.close();
  });

  it("throws a typed transport failure when responding after close (the closed-guard)", async () => {
    const { transport } = makePair();
    await transport.close();
    assert.throws(
      () => transport.respond(7, { result: { success: true, contentItems: [] } }),
      (err: unknown) => {
        assert.ok(err instanceof CodexTransportError);
        // A caller-initiated close is not a framing/EOF violation.
        assert.equal(err.failure.category, "transport");
        return true;
      },
    );
  });
});

describe("codex transport: bounded framing", () => {
  it("turns an over-cap frame into a protocol failure and rejects the in-flight request", async () => {
    const { inbound, transport } = makePair({ maxFrameBytes: 64 });
    const notes = transport.notifications();
    const p = transport.request("thread/start", {});
    // A single line far larger than the 64-byte cap.
    inbound.write(`${JSON.stringify({ method: "flood", params: { blob: "z".repeat(500) } })}\n`);

    await assert.rejects(p, (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /exceeds size cap/);
      return true;
    });
    // The notification iterator observes the same terminal protocol failure.
    await assert.rejects(notes.next(), (err: unknown) => err instanceof CodexTransportError && err.failure.category === "protocol");
    await transport.close();
  });

  it("bounds a newline-less flood without an unbounded read buffer", async () => {
    const { inbound, transport } = makePair({ maxFrameBytes: 32 });
    const p = transport.request("x", {});
    // No newline at all — the partial-line bound must still trip.
    inbound.write("a".repeat(200));
    await assert.rejects(p, (err: unknown) => err instanceof CodexTransportError && err.failure.category === "protocol");
    await transport.close();
  });
});

describe("codex transport: bounded inbound notification queue", () => {
  it("terminates on the aggregate-byte ceiling before the count bound when nothing drains", async () => {
    // A generous item-count bound, a tight byte ceiling: near-cap frames must trip BYTES
    // first. A pending request observes the terminal failure without a note consumer.
    const { inbound, transport } = makePair({ maxInboundNotifications: 1000, maxInboundBytes: 9000 });
    const p = transport.request("thread/start", {});
    const blob = "z".repeat(4000); // ~4 KB/frame: two fit under 9 KB, the third overflows.
    for (let i = 0; i < 3; i++) writeFrame(inbound, { method: "flood", params: { i, blob } });

    await assert.rejects(p, (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "transport");
      // BYTES tripped, not the (nowhere-near) item count.
      assert.match(err.message, /byte overflow/);
      return true;
    });
    await transport.close();
  });

  it("a draining consumer decrements the byte sum so a sustained burst never trips the ceiling", async () => {
    const { inbound, transport } = makePair({ maxInboundNotifications: 1000, maxInboundBytes: 9000 });
    const notes = transport.notifications();
    const blob = "z".repeat(4000); // ~4 KB/frame; two fit under the 9 KB ceiling, three do not.

    // Three rounds of {queue two, drain two}. Six ~4 KB frames total (~24 KB) dwarf the
    // 9 KB ceiling, so absent the per-consume decrement the second round's first push
    // would overflow and terminate the transport.
    for (let round = 0; round < 3; round++) {
      writeFrame(inbound, { method: "flood", params: { round, n: 0, blob } });
      writeFrame(inbound, { method: "flood", params: { round, n: 1, blob } });
      await flush(); // both queued (no waiter pending) before we drain them
      for (let n = 0; n < 2; n++) {
        const note = await notes.next();
        assert.equal(note.done, false);
        assert.equal((note.value as CodexNotification).kind, "activity");
      }
    }
    await transport.close();
  });

  it("terminates on the item-count bound when frames are tiny", async () => {
    // The audit noted the count bound was untested. Tiny frames stay far below the
    // default 64 MiB byte ceiling, so only the count bound can trip.
    const { inbound, transport } = makePair({ maxInboundNotifications: 3 });
    const p = transport.request("thread/start", {});
    for (let i = 0; i < 5; i++) writeFrame(inbound, { method: "tick", params: { i } });

    await assert.rejects(p, (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "transport");
      assert.match(err.message, /buffer overflow/);
      return true;
    });
    await transport.close();
  });
});

describe("codex transport: unexpected EOF", () => {
  it("rejects a pending request with a protocol failure when the stream closes mid-request", async () => {
    const { inbound, transport } = makePair();
    const p = transport.request("thread/start", {});
    await tick();
    inbound.end(); // peer closed while the call is in flight

    await assert.rejects(p, (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /closed mid-request/);
      return true;
    });
    await transport.close();
  });

  it("ends the notification stream cleanly on EOF with no request in flight", async () => {
    const { inbound, transport } = makePair();
    const notes = transport.notifications();
    inbound.end();
    const n = await notes.next();
    assert.equal(n.done, true);
    await transport.close();
  });
});

describe("codex transport: cancellation", () => {
  it("aborting the signal rejects the pending request with an aborted failure", async () => {
    const { transport } = makePair();
    const ac = new AbortController();
    const p = transport.request("thread/start", {}, { signal: ac.signal });
    ac.abort();
    await assert.rejects(p, (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "aborted");
      return true;
    });
    await transport.close();
  });

  it("rejects immediately when the signal is already aborted", async () => {
    const { transport } = makePair();
    const p = transport.request("thread/start", {}, { signal: AbortSignal.abort() });
    await assert.rejects(p, (err: unknown) => err instanceof CodexTransportError && err.failure.category === "aborted");
    await transport.close();
  });

  it("observes an abort fired synchronously by outbound.write", async () => {
    const inbound = new PassThrough();
    const ac = new AbortController();
    const outbound = new Writable({
      write(_chunk, _encoding, callback): void {
        ac.abort();
        callback();
      },
    });
    const transport = createCodexTransport({ inbound, outbound });
    await assert.rejects(
      transport.request("thread/start", {}, { signal: ac.signal, deadlineMs: 100 }),
      (err: unknown) => err instanceof CodexTransportError && err.failure.category === "aborted",
    );
    await transport.close();
  });

  it("handles a response delivered synchronously by outbound.write", async () => {
    const inbound = new PassThrough();
    const outbound = new Writable({
      write(chunk, _encoding, callback): void {
        const request = JSON.parse(String(chunk)) as { id: number };
        writeFrame(inbound, { id: request.id, result: { synchronous: true } });
        callback();
      },
    });
    const transport = createCodexTransport({ inbound, outbound });
    assert.deepEqual(
      await transport.request("thread/start", {}, { signal: new AbortController().signal, deadlineMs: 100 }),
      { synchronous: true },
    );
    await transport.close();
  });

  it("a deadline elapsing rejects the pending request with a timeout failure", async () => {
    const { transport } = makePair();
    const p = transport.request("thread/start", {}, { deadlineMs: 5 });
    await assert.rejects(p, (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "timeout");
      return true;
    });
    await transport.close();
  });
});

describe("codex transport: backpressure", () => {
  it("rejects sends once the bounded outbound queue is full while the peer stalls", async () => {
    // A writable that never invokes its callback, so the transport can never advance
    // past the first in-flight frame and the bounded queue fills.
    const stalled = new Writable({
      highWaterMark: 1,
      write(): void {
        /* deliberately never call the callback */
      },
    });
    const inbound = new PassThrough();
    const transport = createCodexTransport({ inbound, outbound: stalled, maxOutboundFrames: 2 });

    let overflow: unknown;
    try {
      for (let i = 0; i < 20; i++) transport.notify("ping", { i });
    } catch (err) {
      overflow = err;
    }
    assert.ok(overflow instanceof CodexTransportError, "a full outbound queue must throw");
    assert.equal((overflow as CodexTransportError).failure.category, "transport");
    assert.match((overflow as CodexTransportError).message, /outbound queue is full/);

    await transport.close();
  });

  it("rejects on queued bytes before the frame-count ceiling", async () => {
    const stalled = new Writable({
      highWaterMark: 1,
      write(): void {
        /* deliberately never call the callback */
      },
    });
    const inbound = new PassThrough();
    const transport = createCodexTransport({
      inbound,
      outbound: stalled,
      maxOutboundFrames: 100,
      maxOutboundFrameBytes: 1024,
      maxOutboundBytes: 180,
    });

    transport.notify("first", { body: "x".repeat(80) }); // held by Writable
    transport.notify("queued", { body: "x".repeat(80) });
    assert.throws(
      () => transport.notify("overflow", { body: "x".repeat(80) }),
      (err: unknown) => err instanceof CodexTransportError && /byte queue/.test(err.message),
    );
    await transport.close();
  });

  it("rejects one oversized outbound frame", async () => {
    const { transport } = makePair({ maxOutboundFrameBytes: 64, maxOutboundBytes: 1024 });
    assert.throws(
      () => transport.notify("oversized", { body: "x".repeat(128) }),
      (err: unknown) => err instanceof CodexTransportError && /frame exceeds/.test(err.message),
    );
    await transport.close();
  });
});

describe("codex transport: close vs protocol classification", () => {
  it("rejects a request made after a caller-initiated close with a transport failure, not protocol", async () => {
    const { transport } = makePair();
    await transport.close();
    await assert.rejects(transport.request("thread/start", {}), (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      // A caller-initiated close is not a framing/EOF violation.
      assert.equal(err.failure.category, "transport");
      return true;
    });
  });

  it("rejects pending requests with a transport failure when the caller closes explicitly", async () => {
    const { transport } = makePair();
    const p = transport.request("thread/start", {});
    await tick();
    await transport.close(); // caller-initiated closure of an in-flight call
    await assert.rejects(p, (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "transport");
      return true;
    });
  });

  it("rejects a request made after a clean EOF (no call in flight) with a transport failure", async () => {
    const { inbound, transport } = makePair();
    inbound.end(); // clean close with nothing pending — an ordinary close, not protocol
    await flush();
    await assert.rejects(transport.request("thread/start", {}), (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "transport");
      return true;
    });
    await transport.close();
  });

  it("still rejects an in-flight request with a protocol failure on a genuine EOF mid-request", async () => {
    const { inbound, transport } = makePair();
    const p = transport.request("thread/start", {});
    await tick();
    inbound.end(); // EOF WHILE a call is in flight — a genuine protocol failure
    await assert.rejects(p, (err: unknown) => {
      assert.ok(err instanceof CodexTransportError);
      assert.equal(err.failure.category, "protocol");
      assert.match(err.message, /closed mid-request/);
      return true;
    });
    await transport.close();
  });
});

describe("codex transport: lifecycle", () => {
  it("rejects requests made after close and is idempotent", async () => {
    const { transport } = makePair();
    await transport.close();
    await transport.close(); // idempotent
    await assert.rejects(transport.request("x", {}), (err: unknown) => err instanceof CodexTransportError);
  });

  it("notifications() is single-consumer", () => {
    const { transport } = makePair();
    transport.notifications();
    assert.throws(() => transport.notifications(), (err: unknown) => err instanceof CodexTransportError);
  });
});
