import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { checkpointPublishDispatcher } from "../agent/src/checkpoint-publish-transport.js";

// CI-only probe (never merged): one test per undici timeout, so a mutation that removes exactly one
// of headersTimeout/bodyTimeout from checkpointPublishDispatcher fails exactly its own test. The
// production dispatcher is built with a 200 ms limit against a server that stalls 2 s, far past it
// and far under undici's 300 s defaults, and no request signal is passed.
async function stalled(phase: "headers" | "body"): Promise<unknown> {
  const server = http.createServer((_req, res) => {
    if (phase === "headers") { setTimeout(() => res.end("late"), 2_000); return; }
    res.writeHead(200);
    res.write("first");
    setTimeout(() => res.end("late"), 2_000);
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const dispatcher = checkpointPublishDispatcher(200);
  try {
    const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/`;
    const response = await fetch(url, { dispatcher } as RequestInit);
    await response.text();
    return undefined;
  } catch (error) {
    return (error as { cause?: { code?: string } }).cause?.code;
  } finally {
    await dispatcher.destroy();
    server.closeAllConnections();
    server.close();
  }
}

test("probe: a stalled response header is cut by headersTimeout", async () => {
  assert.equal(await stalled("headers"), "UND_ERR_HEADERS_TIMEOUT");
});

test("probe: a stalled response body is cut by bodyTimeout", async () => {
  assert.equal(await stalled("body"), "UND_ERR_BODY_TIMEOUT");
});
