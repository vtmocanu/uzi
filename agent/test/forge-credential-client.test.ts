import { it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

it("forge renewal binds its request to run path and claim generation and bounds secret responses", async () => {
  const requests: Array<{ url?: string; auth?: string; body: string }> = [];
  let result: unknown = { forge_pat: "installation-result-canary", expires_at: new Date(Date.now() + 3600000).toISOString() };
  const server = http.createServer(async (req, res) => {
    let body = ""; for await (const chunk of req) body += String(chunk);
    requests.push({ url: req.url, auth: req.headers.authorization, body });
    res.setHeader("Content-Type", "application/json"); res.end(JSON.stringify(result));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
    const client = new WorkerClient(base, "worker-join-canary", "test", nullLogger());
    const response = await client.refreshForgeCredential("run-1", 9);
    assert.equal(response.forge_pat, "installation-result-canary");
    assert.equal(requests[0]?.url, "/api/worker/runs/run-1/forge/credential");
    assert.equal(requests[0]?.auth, "Bearer worker-join-canary");
    assert.deepEqual(JSON.parse(requests[0]?.body ?? ""), { claim_generation: 9 });
    await assert.rejects(client.refreshForgeCredential("run-1", 0), /invalid claim generation/);
    assert.equal(requests.length, 1);
    result = { forge_pat: "installation-result-canary", expires_at: "bad" };
    await assert.rejects(client.refreshForgeCredential("run-1", 9), /invalid forge credential response/);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) => server.close((err) => err ? reject(err) : resolve()));
  }
});
