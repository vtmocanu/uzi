import assert from "node:assert/strict";
import fs from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import test from "node:test";
import { WorkerClient } from "../src/client.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import { nullLogger } from "./helpers.js";

const metadataCap = 256 * 1024 ** 2;
const releaseCap = 16 * 1024;
async function fixture(t: test.TestContext) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/client-resources-"));
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture" });
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(admitted.ok, JSON.stringify(admitted));
  t.after(async () => { await ledger.finish(admitted.value, "transient"); await fs.rm(dir, { recursive: true, force: true }); });
  return <T>(action: () => T) => MaintenanceResources.run(admitted.value, action);
}
function client(response: Response) {
  return new WorkerClient("http://resources.test", "fixture", "test", nullLogger(), { fetch: async () => response });
}
function byteResponse(text: string, onPull?: () => void) {
  const bytes = new TextEncoder().encode(text);
  const views: number[] = [], supplied: ArrayBufferLike[] = [];
  let offset = 0, cancelled = 0;
  const response = new Response(new ReadableStream({
    type: "bytes",
    pull(controller) {
      onPull?.();
      const request = controller.byobRequest;
      assert.ok(request, "scoped reads must supply a BYOB view");
      const view = request.view!;
      views.push(view.byteLength);
      supplied.push(view.buffer);
      const size = Math.min(view.byteLength, bytes.length - offset);
      if (size === 0) { controller.close(); request.respond(0); return; }
      new Uint8Array(view.buffer, view.byteOffset, size).set(bytes.subarray(offset, offset + size));
      offset += size;
      request.respond(size);
    },
    cancel() { cancelled++; return new Promise(() => {}); },
  }, { highWaterMark: 0 }));
  return { response, views, supplied, cancelled: () => cancelled };
}

test("producer refuses a pull before allocation/read when its entire reservation is unavailable", async t => {
  const run = await fixture(t);
  const source = byteResponse('{"holds_released":1}');
  const reads = t.mock.method(ReadableStreamBYOBReader.prototype, "read");
  await run(async () => {
    MaintenanceResources.consume("metadataBytes", metadataCap - releaseCap + 1);
    await assert.rejects(client(source.response).releaseRecoveryCustody("run"), /maintenance_resource_exhausted/);
  });
  assert.equal(reads.mock.callCount(), 0);
  assert.deepEqual(source.views, []);
  assert.equal(source.cancelled(), 1);
});

test("discovery quota also refuses before BYOB read", async t => {
  const source = byteResponse('{"holds_released":1}');
  const reads = t.mock.method(ReadableStreamBYOBReader.prototype, "read");
  await assert.rejects(MaintenanceResources.runDiscovery(async () => {
    MaintenanceResources.consumeIfActive("metadataBytes", 32 * 1024 ** 2 - releaseCap + 1);
    await client(source.response).releaseRecoveryCustody("run");
  }), /maintenance_resource_exhausted/);
  assert.equal(reads.mock.callCount(), 0);
  assert.equal(source.cancelled(), 1);
});

test("short reads and EOF reserve the whole requested view without refund", async t => {
  const run = await fixture(t);
  const text = '{"holds_released":1}';
  const source = byteResponse(text);
  await run(async () => {
    assert.deepEqual(await client(source.response).releaseRecoveryCustody("run"), { holds_released: 1 });
    MaintenanceResources.consume("metadataBytes", metadataCap - (2 * releaseCap - text.length));
    assert.throws(() => MaintenanceResources.consume("metadataBytes", 1), /maintenance_resource_exhausted/);
  });
  assert.deepEqual(source.views, [releaseCap, releaseCap - text.length]);
});

for (const overflow of [false, true]) {
  test(`strict exact release envelope uses a one-byte ${overflow ? "overflow" : "EOF"} probe`, async t => {
    const run = await fixture(t);
    const text = '{"holds_released":1}'.padEnd(releaseCap, " ") + (overflow ? " " : "");
    const source = byteResponse(text);
    await run(async () => {
      MaintenanceResources.consume("metadataBytes", metadataCap - releaseCap - 1);
      const pending = client(source.response).releaseRecoveryCustody("run");
      if (overflow) await assert.rejects(pending, { name: "ResponseBodyOverflowError" });
      else assert.deepEqual(await pending, { holds_released: 1 });
      assert.throws(() => MaintenanceResources.consume("metadataBytes", 1), /maintenance_resource_exhausted/,
        "EOF and unused view capacity are never refunded");
    });
    assert.deepEqual(source.views, [releaseCap, 1]);
    assert.ok(source.supplied.every(buffer => buffer.byteLength === 0), "even the EOF buffer detaches");
    assert.notEqual(source.supplied[0], source.supplied[1]);
  });
}

test("strict EOF probe is refused before read when the envelope consumes all quota", async t => {
  const run = await fixture(t);
  const source = byteResponse('{"holds_released":1}'.padEnd(releaseCap, " "));
  await run(async () => {
    MaintenanceResources.consume("metadataBytes", metadataCap - releaseCap);
    await assert.rejects(client(source.response).releaseRecoveryCustody("run"), /maintenance_resource_exhausted/);
  });
  assert.deepEqual(source.views, [releaseCap]);
});

test("discovery bounds views to 64 KiB and the remaining hold envelope", async () => {
  const source = byteResponse(JSON.stringify({ run_id: "run", holds: [], padding: "x".repeat(150_000) }));
  const result = await MaintenanceResources.runDiscovery(() => client(source.response).listRecoveryHolds("run"));
  assert.deepEqual(result.holds, []);
  assert.ok(source.views.length > 2);
  assert.equal(Math.max(...source.views), 64 * 1024);
  assert.ok(source.views.every(size => size > 0 && size <= 64 * 1024));
});

test("unsupported producer stream is cancelled without a default-reader fallback", async t => {
  const run = await fixture(t);
  let pulls = 0, cancelled = 0;
  const response = new Response(new ReadableStream<Uint8Array>({
    pull(controller) { pulls++; controller.enqueue(new TextEncoder().encode('{"holds_released":1}')); controller.close(); },
    cancel() { cancelled++; return new Promise(() => {}); },
  }, { highWaterMark: 0 }));
  await run(() => assert.rejects(client(response).releaseRecoveryCustody("run"), TypeError));
  assert.equal(pulls, 0);
  assert.equal(cancelled, 1);
});

test("ordinary default stream retains overflow and detached cancellation behavior", async () => {
  let pulls = 0, cancelled = 0;
  const response = new Response(new ReadableStream<Uint8Array>({
    pull(controller) { pulls++; controller.enqueue(new TextEncoder().encode(" ".repeat(releaseCap + 1))); },
    cancel() { cancelled++; return new Promise(() => {}); },
  }, { highWaterMark: 0 }));
  await assert.rejects(client(response).releaseRecoveryCustody("run"), { name: "ResponseBodyOverflowError" });
  assert.equal(pulls, 1);
  assert.equal(cancelled, 1);
  const small = new Response(new ReadableStream<Uint8Array>({
    start(controller) { controller.enqueue(new TextEncoder().encode('{"holds_released":1}')); controller.close(); },
  }));
  assert.deepEqual(await client(small).releaseRecoveryCustody("run"), { holds_released: 1 });
});

test("scope revoked during a BYOB pull cannot deliver a successful release", async t => {
  const run = await fixture(t);
  const source = byteResponse('{"holds_released":1}', () => {
    assert.throws(() => MaintenanceResources.consume("metadataBytes", metadataCap), /maintenance_resource_exhausted/);
  });
  await run(() => assert.rejects(client(source.response).releaseRecoveryCustody("run"), /maintenance_token_invalid|maintenance_resource_exhausted/));
});

test("real loopback fetch supports scoped BYOB release", async t => {
  const server = http.createServer((_req, res) => { res.end('{"holds_released":1}'); });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise<void>((resolve, reject) => {
    server.close(error => error ? reject(error) : resolve());
    server.closeAllConnections();
  }));
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  const worker = new WorkerClient(`http://127.0.0.1:${address.port}`, "fixture", "test", nullLogger());
  assert.deepEqual(await MaintenanceResources.runDiscovery(() => worker.releaseRecoveryCustody("run")), { holds_released: 1 });
});
