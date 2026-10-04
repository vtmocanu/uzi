import { it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { createCodexTransport } from "../src/codex/transport.js";

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
