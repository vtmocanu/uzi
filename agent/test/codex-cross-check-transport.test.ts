import { it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { createCodexTransport, CodexTransportError } from "../src/codex/transport.js";

it("charges every raw byte before retaining split UTF-8 chunks", async () => {
 const inbound = new PassThrough();
 const transport = createCodexTransport({ inbound, outbound: new PassThrough(), maxProtocolBytes: 3 });
 const next = transport.notifications()[Symbol.asyncIterator]().next();
 inbound.write(Buffer.from([0xf0]));
 inbound.write(Buffer.from([0x9f]));
 inbound.write(Buffer.from([0x98]));
 inbound.write(Buffer.from([0x80]));
 await assert.rejects(next, /cumulative protocol budget/);
 await transport.close();
});

it("byte budget persists across consumed frames and charges RPC responses", async () => {
 const inbound = new PassThrough();
 const outbound = new PassThrough();
 const frame = JSON.stringify({ id: 1, result: {} }) + "\n";
 const transport = createCodexTransport({ inbound, outbound, maxProtocolBytes: Buffer.byteLength(frame) });
 const request = transport.request("initialize", {});
 inbound.write(frame);
 await request;
 const next = transport.notifications()[Symbol.asyncIterator]().next();
 inbound.write("\n");
 await assert.rejects(next, /cumulative protocol budget/);
 await transport.close();
});

it("frame budget is charged before parsing and auth interception, even after consumption", async () => {
 const inbound = new PassThrough();
 const transport = createCodexTransport({ inbound, outbound: new PassThrough(), maxProtocolFrames: 2 });
 let intercepted = 0;
 transport.installServerRequestInterceptor!(() => { intercepted++; return true; });
 inbound.write(JSON.stringify({ id: 8, method: "auth" }) + "\n");
 const notes = transport.notifications()[Symbol.asyncIterator]();
 const first = notes.next();
 inbound.write(JSON.stringify({ method: "activity" }) + "\n");
 assert.equal((await first).value!.method, "activity");
 const next = notes.next();
 // The over-budget frame is malformed: the cap must fire before JSON parsing.
 inbound.write("malformed\n");
 await assert.rejects(next, /cumulative protocol budget/);
 assert.equal(intercepted, 1);
 await transport.close();
});

it("one coalesced chunk dispatches at most the allowed number of frames", async () => {
 const inbound = new PassThrough();
 const transport = createCodexTransport({ inbound, outbound: new PassThrough(), maxProtocolFrames: 2 });
 let intercepted = 0;
 transport.installServerRequestInterceptor!(() => { intercepted++; return true; });
 const next = transport.notifications()[Symbol.asyncIterator]().next();
 inbound.write(Array.from({ length: 3 }, (_, id) => JSON.stringify({ id, method: "auth" })).join("\n") + "\n");
 await assert.rejects(next, /cumulative protocol budget/);
 assert.equal(intercepted, 2);
 await transport.close();
});

it("exact cumulative caps accept split multibyte frame", async () => {
 const inbound = new PassThrough();
 const bytes = Buffer.from(JSON.stringify({ method: "activity", params: { text: "😀" } }) + "\n");
 const transport = createCodexTransport({ inbound, outbound: new PassThrough(), maxProtocolBytes: bytes.length, maxProtocolFrames: 1 });
 const next = transport.notifications()[Symbol.asyncIterator]().next();
 for (const byte of bytes) inbound.write(Buffer.from([byte]));
 assert.deepEqual((await next).value!.params, { text: "😀" });
 await transport.close();
});

it("receipt observer captures phase before queued consumption without changing notes", async () => {
 const inbound = new PassThrough();
 let authenticated = false;
 const eligible = new WeakSet<object>();
 const transport = createCodexTransport({ inbound, outbound: new PassThrough(),
  onNotification: (note) => { if (authenticated) eligible.add(note); } });
 inbound.write(JSON.stringify({ method: "activity", params: { phase: "before" } }) + "\n");
 authenticated = true;
 inbound.write(JSON.stringify({ method: "activity", params: { phase: "after" } }) + "\n");
 const notes = transport.notifications();
 const before = (await notes.next()).value!;
 const after = (await notes.next()).value!;
 assert.equal(eligible.has(before), false);
 assert.equal(eligible.has(after), true);
 assert.deepEqual(before.params, { phase: "before" });
 assert.deepEqual(after.params, { phase: "after" });
 assert.equal(Object.hasOwn(before, "authenticated"), false);
 await transport.close();
});

it("receipt observer failure is a static sticky protocol failure", async () => {
 const inbound = new PassThrough();
 const transport = createCodexTransport({ inbound, outbound: new PassThrough(),
  onNotification: () => { throw new Error("private observer detail"); } });
 const next = transport.notifications().next();
 inbound.write(JSON.stringify({ method: "activity" }) + "\n");
 await assert.rejects(next, (error: CodexTransportError) => {
  assert.equal(error, transport.protocolFailure);
  assert.equal(error.failure.category, "protocol");
  assert.equal(error.message, "codex transport notification observer failed");
  return true;
 });
 await transport.close();
});

it("receipt observer preserves notification queue bounds", async () => {
 const inbound = new PassThrough();
 let received = 0;
 const transport = createCodexTransport({ inbound, outbound: new PassThrough(), maxInboundNotifications: 1,
  onNotification: () => { received++; } });
 inbound.write(JSON.stringify({ method: "activity" }) + "\n" + JSON.stringify({ method: "activity" }) + "\n");
 assert.equal(received, 2);
 const notes = transport.notifications();
 assert.equal((await notes.next()).value!.kind, "activity");
 await assert.rejects(notes.next(), /inbound notification buffer overflow/);
 await transport.close();
});

const capturedMessage = JSON.stringify({ error: { message: "SYNTHETIC model rejection marker B",
 type: "invalid_request_error", param: "model", code: "model_not_found" } });
const secret = "glpat-" + "0123456789abcdefghij";

for (const [label, message, data, tag] of [
 ["captured400", capturedMessage, undefined, "model_not_found"],
 ["prose", "model_not_found " + secret, undefined, undefined],
 ["rawdata", secret, { error: { code: "model_not_found", param: "model", type: "invalid_request_error" } }, undefined],
 ["unknowncode", capturedMessage.replace("model_not_found", "unsupported_model"), undefined, undefined],
 ["wrongparam", capturedMessage.replace('"param":"model"', '"param":"effort"'), undefined, undefined],
 ["wrongtype", capturedMessage.replace("invalid_request_error", "authentication_error"), undefined, undefined],
 ["recursive", JSON.stringify(capturedMessage), undefined, undefined],
 ["array", "[" + capturedMessage + "]", undefined, undefined],
 ["oversize", capturedMessage + " ".repeat(8193), undefined, undefined],
 ["exact8KiB", capturedMessage + " ".repeat(8192 - Buffer.byteLength(capturedMessage)), undefined, "model_not_found"],
 ["oneByteOver", capturedMessage + " ".repeat(8193 - Buffer.byteLength(capturedMessage)), undefined, undefined],
 ["utf8Over", capturedMessage + "é".repeat(4096), undefined, undefined],
] as const) {
 it("matched RPC carries only closed safe metadata: " + label, async () => {
  const inbound = new PassThrough();
  const transport = createCodexTransport({ inbound, outbound: new PassThrough() });
  const request = transport.request("turn/start", {});
  inbound.write(JSON.stringify({ id: 1, error: { code: -32000, message, data, cause: secret } }) + "\n");
  await assert.rejects(request, (err: CodexTransportError) => {
   assert.ok(err instanceof CodexTransportError);
   assert.equal(err.modelRejection, tag);
   assert.equal(err.message, "codex app-server returned a JSON-RPC error (code -32000)");
   for (const raw of [secret, "SYNTHETIC", "unsupported_model"]) {
    assert.equal(JSON.stringify(err).includes(raw), false);
    assert.equal(String(err).includes(raw), false);
   }
   assert.equal(Object.hasOwn(err, "cause"), false);
   return true;
  });
  await transport.close();
 });
}
it("unknown RPC ids and server requests never gain rejection metadata", async () => {
 const inbound = new PassThrough();
 const transport = createCodexTransport({ inbound, outbound: new PassThrough() });
 const notes = transport.notifications();
 for (const frame of [
  { id: 999, error: { code: -32000, message: capturedMessage, data: secret } },
  { id: "foreign", method: "error", params: { threadId: "t", turnId: "v", willRetry: false,
   error: { message: capturedMessage, codexErrorInfo: "other" } } },
 ]) {
  const next = notes.next(); inbound.write(JSON.stringify(frame) + "\n");
  const note = (await next).value!;
  assert.equal(note.kind, "activity");
  assert.equal(Object.hasOwn(note, "modelRejection"), false);
 }
 await transport.close();
});
it("typed async and terminal decoders retain safe tags only for proper frames", async () => {
 const inbound = new PassThrough();
 const transport = createCodexTransport({ inbound, outbound: new PassThrough() });
 const notes = transport.notifications();
 const error = { message: capturedMessage, codexErrorInfo: "other", additionalDetails: secret };
 for (const [frame, expected] of [
  [{ method: "error", params: { threadId: "t", turnId: "v", willRetry: false, error } }, "model_not_found"],
  [{ method: "error", params: { threadId: "t", turnId: "v", willRetry: "false", error } }, undefined],
  [{ method: "error", params: { threadId: "t", willRetry: false, error } }, undefined],
  [{ method: "error", params: { threadId: "t", turnId: "v", willRetry: false, error: { ...error, codexErrorInfo: "badRequest" } } }, undefined],
  [{ method: "turn/completed", params: { threadId: "t", turn: { id: "v", status: "failed", items: [], error } } }, "model_not_found"],
  [{ method: "turn/completed", params: { threadId: "t", turn: { id: "v", status: "interrupted", items: [], error } } }, undefined],
  [{ method: "turn/completed", params: { threadId: "t", turn: { id: "v", status: "failed", error } } }, undefined],
 ] as const) {
  const next = notes.next(); inbound.write(JSON.stringify(frame) + "\n");
  const note = (await next).value!;
  assert.equal("modelRejection" in note ? note.modelRejection : undefined, expected);
 }
 await transport.close();
});

for (const code of [undefined, "invalid_request_error", 0.5]) {
 it("malformed matched RPC error cannot carry a model tag: " + String(code), async () => {
  const inbound = new PassThrough();
  const transport = createCodexTransport({ inbound, outbound: new PassThrough() });
  const request = transport.request("thread/start", {});
  inbound.write(JSON.stringify({ id: 1, error: { code, message: capturedMessage } }) + "\n");
  await assert.rejects(request, (err: CodexTransportError) => {
   assert.ok(err instanceof CodexTransportError); assert.equal(err.modelRejection, undefined); return true;
  });
  await transport.close();
 });
}
