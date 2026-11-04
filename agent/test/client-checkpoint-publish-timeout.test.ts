import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import { Readable } from "node:stream";
import { CHECKPOINT_PUBLISH_TIMEOUT_MS, WorkerClient } from "../src/client.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { nullLogger } from "./helpers.js";

const TIP = "a".repeat(40);

// A server that drains the pack, waits `delayMs`, then acknowledges the publish.
async function ackAfter(t: { after: (fn: () => void) => void }, delayMs: number): Promise<string> {
  const server = http.createServer((req, res) => {
    req.resume();
    req.on("end", () => setTimeout(() => {
      if (res.destroyed) return;
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ published: true, ref: "refs/uzi-checkpoints/branch" }));
    }, delayMs));
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => { server.closeAllConnections(); server.close(); });
  return `http://127.0.0.1:${(server.address() as { port: number }).port}`;
}
const pack = () => Readable.from(["pack"]);

it("a publish slower than httpTimeoutMs succeeds inside the checkpoint allowance", async (t) => {
  const url = await ackAfter(t, 600);
  const client = new WorkerClient(url, "test", "test", nullLogger(), { httpTimeoutMs: 200, checkpointPublishTimeoutMs: 2_000 });
  assert.equal(client.checkpointPublishTimeoutMs, 2_000);
  const res = await client.publishCheckpoint("run-1", TIP, pack());
  assert.deepEqual(res, { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/branch" } });
});

it("a client built without the option uses the production allowance, not httpTimeoutMs", async (t) => {
  const url = await ackAfter(t, 600);
  const client = new WorkerClient(url, "test", "test", nullLogger(), { httpTimeoutMs: 200 });
  assert.equal(client.checkpointPublishTimeoutMs, CHECKPOINT_PUBLISH_TIMEOUT_MS);
  const res = await client.publishCheckpoint("run-1", TIP, pack());
  assert.deepEqual(res, { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/branch" } });
});

it("the allowance still ends a publish that outlasts it", async (t) => {
  const url = await ackAfter(t, 3_000);
  const client = new WorkerClient(url, "test", "test", nullLogger(), { httpTimeoutMs: 200, checkpointPublishTimeoutMs: 400 });
  const started = performance.now();
  await assert.rejects(client.publishCheckpoint("run-1", TIP, pack()));
  const elapsed = performance.now() - started;
  assert.ok(elapsed >= 300 && elapsed < 2_000, `ended after ${elapsed} ms, expected about 400`);
});

it("a caller boundary signal ends the publish before the allowance", async (t) => {
  const url = await ackAfter(t, 3_000);
  const client = new WorkerClient(url, "test", "test", nullLogger(), { httpTimeoutMs: 200, checkpointPublishTimeoutMs: 20_000 });
  const started = performance.now();
  await assert.rejects(client.publishCheckpoint("run-1", TIP, pack(), AbortSignal.timeout(300)));
  const elapsed = performance.now() - started;
  assert.ok(elapsed >= 200 && elapsed < 2_000, `ended after ${elapsed} ms, expected about 300`);
});

it("a current MaintenanceBudget with less time left ends the publish before the allowance", async (t) => {
  const url = await ackAfter(t, 3_000);
  const client = new WorkerClient(url, "test", "test", nullLogger(), { httpTimeoutMs: 200, checkpointPublishTimeoutMs: 20_000 });
  const budget = new MaintenanceBudget(300);
  t.after(() => budget.dispose());
  const started = performance.now();
  await assert.rejects(MaintenanceBudget.run(budget, () => client.publishCheckpoint("run-1", TIP, pack())));
  const elapsed = performance.now() - started;
  assert.ok(elapsed < 2_000, `ended after ${elapsed} ms, expected about 300`);
});

it("undici's own headers timeout does not cut a publish the allowance still covers", async (t) => {
  // undici runs these timeouts on its coarse fast timers (about 1 s resolution), so the answer is
  // held well past the scaled 200 ms timeout.
  const url = await ackAfter(t, 2_500);
  // Load Node's fetch implementation so the global dispatcher slot is filled, then stand a 200 ms
  // headers/body-timeout Agent of the same class in for undici's 300 s defaults (time-scaled).
  await fetch(`${url}/warm`, { method: "POST", body: "x" }).then(r => r.text());
  const slot = Symbol.for("undici.globalDispatcher.2");
  const g = globalThis as unknown as Record<symbol, { constructor: new (o: object) => { close(): Promise<void> } }>;
  const original = g[slot];
  assert.ok(original, "the global dispatcher slot is filled after a fetch");
  const scaled = new original.constructor({ headersTimeout: 200, bodyTimeout: 200 });
  g[slot] = scaled as unknown as typeof original;
  t.after(async () => { g[slot] = original; await scaled.close(); });

  // Control: a plain fetch on the scaled default is cut at its headers timeout.
  await assert.rejects(fetch(url, { method: "POST", body: "pack" }),
    (e: { cause?: { code?: string } }) => e.cause?.code === "UND_ERR_HEADERS_TIMEOUT");

  const client = new WorkerClient(url, "test", "test", nullLogger(), { httpTimeoutMs: 100, checkpointPublishTimeoutMs: 6_000 });
  const res = await client.publishCheckpoint("run-1", TIP, pack());
  assert.deepEqual(res, { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/branch" } });
});

it("the worker allowance matches the committed budget fixture and exceeds the api write deadline", () => {
  const fixture = JSON.parse(fs.readFileSync(
    new URL("../../fixtures/checkpoint-publish-budget.json", import.meta.url), "utf8")) as
    { api_write_deadline_ms: number; worker_allowance_ms: number };
  assert.equal(CHECKPOINT_PUBLISH_TIMEOUT_MS, fixture.worker_allowance_ms);
  assert.ok(CHECKPOINT_PUBLISH_TIMEOUT_MS > fixture.api_write_deadline_ms);
});
