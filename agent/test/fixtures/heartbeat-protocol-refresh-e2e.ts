import { it } from "node:test";
import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";
import { writeFileSync } from "node:fs";
import { performance } from "node:perf_hooks";
import type { WorkerClient as CurrentClient } from "../../src/client.js";
import { nullLogger } from "../helpers.js";

const { WorkerClient } = await import(pathToFileURL(process.env.UZI_REFRESH_CLIENT!).href) as { WorkerClient: typeof CurrentClient };
const name = process.env.UZI_REFRESH_CASE!;
const primary = process.env.UZI_REFRESH_URL!;
const old = process.env.UZI_REFRESH_OLD_URL!;
const token = process.env.UZI_REFRESH_TOKEN!;
const runID = process.env.UZI_REFRESH_RUN!;
const requests: { path: string; body: Record<string, unknown>; bytes: number; responseBytes: number; ms: number }[] = [];
let endpoint = name === "old_worker_new_api" ? primary : old;
const client = new WorkerClient(primary, token, "m3", nullLogger(), {
  fetch: async (input, init) => {
    const path = new URL(String(input)).pathname;
    const body = String(init?.body ?? "{}");
    const start = performance.now();
    const response = await fetch(endpoint + path, init);
    // Count actual bytes without decoding or retaining text. EOF is required even
    // at the limit; headers and a valid JSON prefix cannot establish completeness.
    let responseBytes = 0;
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
    try {
      reader = response.clone().body?.getReader();
      if (reader) {
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          responseBytes += value.byteLength;
          if (responseBytes > 1024 * 1024) throw new Error("overflow");
        }
      }
    } catch {
      // Start both cancellations before awaiting either: a tee branch's cancel
      // can wait for the other branch. One failure must not prevent its sibling.
      await Promise.allSettled([
        reader?.cancel().catch(() => {}),
        response.body?.cancel().catch(() => {}),
      ]);
      assert.fail("fixture response unreadable or exceeds 1 MiB");
    } finally {
      reader?.releaseLock();
    }
    requests.push({ path, body: JSON.parse(body), bytes: Buffer.byteLength(body), responseBytes, ms: performance.now() - start });
    return response;
  },
});
const snapshot = (epoch: number) => ({ snapshot_epoch: epoch, active: [], pending_overflow: false });
const heartbeats = () => requests.filter(r => r.path.endsWith("/heartbeat"));

it(name, async () => {
  const registration = await client.register("m3", "base", 4, undefined,
    ["completion_interlock_v1", "credential_switch_v1"], undefined, 2);
  assert.match(registration.register_nonce!, /^[A-Za-z0-9_-]{43}$/);
  writeFileSync(process.env.UZI_REFRESH_STATE!, JSON.stringify({
    worker_id: registration.worker_id, register_nonce: registration.register_nonce,
  }));
  if (name === "client_feature_adoption") {
    endpoint = primary;
    await fetch(primary + "/__enable", { method: "POST" });
    await client.heartbeat(undefined, undefined, snapshot(1));
    await client.heartbeat(undefined, undefined, snapshot(2));
    const ticks = heartbeats();
    assert.equal(ticks.length, 2);
    assert.equal(ticks[0]!.body.active_snapshot, undefined);
    assert.equal(ticks[0]!.body.protocol_refresh, undefined);
    assert.ok(ticks[1]!.body.active_snapshot, "feature X must be used by tick 2");
    assert.ok(ticks[1]!.body.protocol_refresh, "refresh must be discovered by tick 2");
    assert.equal((ticks[1]!.body.protocol_refresh as Record<string, unknown>).register_nonce, registration.register_nonce);
  } else if (name === "server_capability_claim_restoration") {
    assert.equal(await client.claimRun(), null, "calibrated registration must block actual SQL claim");
    endpoint = primary;
    await fetch(primary + "/__enable", { method: "POST" });
    assert.equal(await client.claimRun(), null, "switching API alone must not change DB eligibility");
    await client.heartbeat();
    assert.equal(await client.claimRun(), null, "discovery tick alone must not grant claim eligibility");
    assert.equal(heartbeats()[0]!.body.protocol_refresh, undefined);
    await client.heartbeat();
    const claim = await client.claimRun();
    assert.equal(claim?.run_id, runID, "same queued run must become claimable by tick 2");
    const report = heartbeats().at(-1)!.body.protocol_refresh as Record<string, unknown>;
    assert.equal(report.register_nonce, registration.register_nonce);
    assert.equal(report.max_concurrent_runs, 4);
    assert.equal(report.max_cross_check_slots, 2);
  } else if (name === "new_worker_old_api") {
    await client.heartbeat();
    await client.heartbeat();
    assert.equal(heartbeats().length, 2);
    for (const tick of heartbeats()) assert.equal(tick.body.protocol_refresh, undefined);
  } else if (name === "old_worker_new_api") {
    await fetch(primary + "/__enable", { method: "POST" });
    await client.heartbeat();
    await client.heartbeat();
    assert.equal(heartbeats().length, 2);
    for (const tick of heartbeats()) assert.equal(tick.body.protocol_refresh, undefined);
  } else {
    assert.fail("unknown named control");
  }
  // Three successful steady ticks measure the fixed report without another registration,
  // follow-up request or snapshot epoch. Each response here is complete and below 1 MiB.
  for (let tick = 0; tick < 3; tick++) {
    const before = heartbeats().length;
    assert.equal(await client.heartbeat(), false, "healthy empty worker custody stays clear");
    assert.equal(heartbeats().length, before + 1);
  }
  assert.equal(heartbeats().length, 5);
  assert.equal(requests.filter(r => r.path.endsWith("/register")).length, 1);
  console.log("M3_WIRE " + JSON.stringify({ name, implemented_count: 2, heartbeats: heartbeats().map(({ bytes, responseBytes, ms }) => ({ bytes, responseBytes, ms })) }));
});
