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

function makePair(opts: { maxFrameBytes?: number; maxOutboundFrames?: number } = {}): {
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
