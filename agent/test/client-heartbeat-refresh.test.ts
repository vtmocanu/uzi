import { it } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient, RequestError } from "../src/client.js";
import type { ActiveSnapshot } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const NONCE = Buffer.alloc(32, 7).toString("base64url");
const REFRESH = "heartbeat_protocol_refresh_v1";
const snapshot: ActiveSnapshot = { snapshot_epoch: 1, active: [], pending_overflow: false };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function fixture(register: Record<string, unknown> = {}) {
  const requests: { path: string; body: Record<string, unknown> }[] = [];
  const warnings: unknown[][] = [];
  let reply = () => json({});
  const logger = nullLogger();
  logger.warn = (...args) => { warnings.push(args); };
  const client = new WorkerClient("http://example.test", "join-fixture", "test", logger, {
    fetch: async (input, init) => {
      const path = String(input);
      requests.push({ path, body: JSON.parse(String(init?.body)) });
      if (path.endsWith("/register")) return json({
        worker_id: "w1", protocol_features: [], register_nonce: NONCE,
        worker_outbox_max_pending: 5, ...register,
      });
      if (path.includes("/runs/claim") || path.endsWith("/messages") || path.endsWith("/cross-check-verdict")) return new Response(null, { status: 204 });
      if (path.endsWith("/state")) return json({ run: { status: JSON.parse(String(init?.body)).status } });
      return reply();
    },
  });
  return { client, requests, warnings, setReply: (fn: () => Response) => { reply = fn; } };
}
const heartbeatBodies = (f: ReturnType<typeof fixture>) =>
  f.requests.filter((r) => r.path.endsWith("/heartbeat")).map((r) => r.body);

it("discovers on one ordinary tick and reports original unfiltered capabilities on the next, with one register", async () => {
  const f = fixture({ protocol_capabilities: ["server-accepted-only"] });
  const caps = ["credential_switch_v1", "unknown-new-cap"];
  await f.client.register("w", undefined, 4, undefined, caps, undefined, 2);
  caps[1] = "mutated";
  f.setReply(() => json({ protocol_features: [REFRESH], worker_outbox_max_pending: 9, register_nonce: "replacement" }));
  await f.client.heartbeat();
  assert.equal(heartbeatBodies(f)[0]?.protocol_refresh, undefined);
  assert.equal(f.client.workerOutboxMaxPending, 9);
  await f.client.heartbeat();
  assert.deepEqual(heartbeatBodies(f)[1]?.protocol_refresh, {
    protocol_capabilities: ["credential_switch_v1", "unknown-new-cap"],
    max_concurrent_runs: 4, max_cross_check_slots: 2, register_nonce: NONCE,
  });
  assert.equal(f.requests.filter((r) => r.path.endsWith("/register")).length, 1);
  assert.equal(heartbeatBodies(f).length, 2);
});

it("accepts an explicit empty implemented list and rejects missing or invalid local report fields", async () => {
  const cases: [number | undefined, number | undefined, string[] | undefined, boolean][] = [
    [1, 0, [], true], [256, 16, ["é".repeat(32)], true],
    [undefined, 0, [], false], [1, undefined, [], false], [1, 0, undefined, false],
    [0, 0, [], false], [257, 0, [], false], [1.5, 0, [], false],
    [1, -1, [], false], [1, 17, [], false], [1, 0.5, [], false],
    [1, 0, Array(257).fill("x"), false], [1, 0, ["é".repeat(33)], false],
    [1, 0, ["valid", 1] as unknown as string[], false],
    [1, 0, "garbage" as unknown as string[], false],
  ];
  for (const [runs, slots, caps, valid] of cases) {
    const f = fixture({ protocol_features: [REFRESH] });
    await f.client.register("w", undefined, runs, undefined, caps, undefined, slots);
    await f.client.heartbeat();
    assert.equal(heartbeatBodies(f)[0]?.protocol_refresh !== undefined, valid);
  }
});

it("replaces only complete string feature lists, accepts unknown large adverts, and preserves invalid quotas", async () => {
  const f = fixture({ protocol_features: ["old"] });
  await f.client.register("w");
  for (const features of [undefined, null, ["new", 1], {}, "garbage"]) {
    f.setReply(() => json({ protocol_features: features, worker_outbox_max_pending: 1.5 }));
    await f.client.heartbeat();
    assert.deepEqual(f.client.protocolFeatures, ["old"]);
    assert.equal(f.client.workerOutboxMaxPending, 5);
  }
  const names = [...Array.from({ length: 257 }, (_, i) => "unknown-" + i), "x".repeat(1000)];
  f.setReply(() => json({ protocol_features: names, worker_outbox_max_pending: 0 }));
  await f.client.heartbeat();
  assert.deepEqual(f.client.protocolFeatures, names);
  assert.equal(f.client.workerOutboxMaxPending, 0);
  for (const quota of [-1, 0.5, null, "4", undefined]) {
    f.setReply(() => json({ worker_outbox_max_pending: quota }));
    await f.client.heartbeat();
    assert.equal(f.client.workerOutboxMaxPending, 0);
  }
  f.setReply(() => json({ protocol_features: [] }));
  await f.client.heartbeat();
  assert.deepEqual(f.client.protocolFeatures, []);
});

it("retains cache and quota on 204, invalid JSON and non-2xx", async () => {
  const f = fixture({ protocol_features: ["old"] });
  await f.client.register("w");
  for (const make of [
    () => new Response(null, { status: 204 }),
    () => new Response("{bad"),
    () => json({ protocol_features: [], worker_outbox_max_pending: 0 }, 503),
  ]) {
    f.setReply(make);
    if (make().status === 503) await assert.rejects(f.client.heartbeat(), RequestError);
    else assert.equal(await f.client.heartbeat(), undefined);
    assert.deepEqual(f.client.protocolFeatures, ["old"]);
    assert.equal(f.client.workerOutboxMaxPending, 5);
  }
});

it("requires actual EOF within 1 MiB, ignores content length, and never parses an overflow or failed prefix", async () => {
  const prefix = JSON.stringify({
    protocol_features: ["new"], worker_outbox_max_pending: 9,
    worker: { retaining_unpublished_work: false },
  });
  const exact = prefix + " ".repeat(1024 * 1024 - Buffer.byteLength(prefix));
  for (const contentLength of [undefined, "1", "9999999"]) {
    const f = fixture({ protocol_features: ["old"] });
    await f.client.register("w");
    f.setReply(() => new Response(exact, { headers: contentLength ? { "Content-Length": contentLength } : {} }));
    assert.equal(await f.client.heartbeat(), false);
    assert.deepEqual(f.client.protocolFeatures, ["new"]);
    assert.equal(f.client.workerOutboxMaxPending, 9);
  }
  for (const kind of ["extra-byte", "failed-prefix", "unreadable"] as const) {
    const f = fixture({ protocol_features: ["old"] });
    await f.client.register("w");
    f.setReply(() => {
      if (kind === "extra-byte") return new Response(exact + " ", { headers: { "Content-Length": "1" } });
      if (kind === "unreadable") {
        const response = new Response(prefix);
        void response.body!.getReader();
        return response;
      }
      let reads = 0;
      return new Response(new ReadableStream({
        pull(controller) {
          if (reads++ === 0) controller.enqueue(new TextEncoder().encode(prefix));
          else controller.error(new Error("untrusted secret text"));
        },
      }));
    });
    assert.equal(await f.client.heartbeat(), undefined);
    assert.equal(f.client.latestDindMaintenance, undefined);
    assert.deepEqual(f.client.protocolFeatures, ["old"]);
    assert.equal(f.client.workerOutboxMaxPending, 5);
    assert.deepEqual(f.warnings, [["heartbeat response unreadable or exceeds 1 MiB; ignoring response", {}]]);
  }
});

it("strips a refresh-only strict failure, ignores retry adverts, and rediscovers on later ordinary ticks", async () => {
  const f = fixture({ protocol_features: [REFRESH] });
  await f.client.register("w", undefined, 3, undefined, [], undefined, 0);
  let count = 0;
  f.setReply(() => count++ === 0
    ? json({ error: "invalid request body" }, 400)
    : json({ protocol_features: [REFRESH, "heartbeat_outbox"], worker_outbox_max_pending: 10,
        worker: { retaining_unpublished_work: false } }));
  assert.equal(await f.client.heartbeat(), false);
  assert.deepEqual(heartbeatBodies(f)[1], { version: "test" });
  assert.deepEqual(f.client.protocolFeatures, []);
  assert.equal(f.client.workerOutboxMaxPending, 5);
  await f.client.heartbeat();
  assert.equal(heartbeatBodies(f)[2]?.protocol_refresh, undefined);
  await f.client.heartbeat();
  assert.ok(heartbeatBodies(f)[3]?.protocol_refresh);
  assert.equal(heartbeatBodies(f).length, 4);
});

it("feature growth without an original nonce cannot send snapshot or refresh or adopt a response nonce", async () => {
  for (const nonce of [undefined, "bad", "A".repeat(42) + "B"]) {
    const f = fixture({ register_nonce: nonce });
    await f.client.register("w", undefined, 1, undefined, [], undefined, 0);
    f.setReply(() => json({ protocol_features: [REFRESH, "active_run_snapshot"], register_nonce: NONCE }));
    await f.client.heartbeat();
    await f.client.heartbeat(undefined, undefined, snapshot);
    await f.client.claimRun(snapshot);
    await f.client.claimCrossCheck(snapshot);
    assert.equal(heartbeatBodies(f)[1]?.protocol_refresh, undefined);
    assert.equal(heartbeatBodies(f)[1]?.active_snapshot !== undefined, nonce !== undefined);
    const claims = f.requests.filter((r) => r.path.includes("/runs/claim"));
    assert.equal(claims.length, 2);
    for (const claim of claims) {
      assert.equal(claim.body.active_snapshot !== undefined, nonce !== undefined);
      if (nonce !== undefined) assert.equal((claim.body.active_snapshot as ActiveSnapshot).register_nonce, nonce);
    }
  }
});

it("sticky credential capability keeps stamping messages across feature growth and shrink", async () => {
  const f = fixture();
  await f.client.register("w", undefined, 1, undefined, ["credential_switch_v1"], undefined, 0);
  for (const features of [["claim_generation_fence"], []]) {
    f.setReply(() => json({ protocol_features: features }));
    await f.client.heartbeat();
    await f.client.postMessages("run-fixture", [{ seq: 1, kind: "text", payload: { text: "hi" } }], 7);
    assert.equal(f.requests.at(-1)?.body.claim_generation, 7);
  }
});

it("feature shrink closes new optional sends without changing held or checker generation contracts", async () => {
  const sticky = fixture();
  await sticky.client.register("w", undefined, 1, undefined, ["credential_switch_v1", "cross_check_v1"], undefined, 1);
  for (const features of [["claim_generation_fence", "active_run_snapshot"], []]) {
    sticky.setReply(() => json({ protocol_features: features }));
    await sticky.client.heartbeat();
    await sticky.client.reportState("held-run", { status: "credential_switch", claim_generation: 7 });
    assert.equal(sticky.requests.at(-1)?.body.claim_generation, 7);
    await sticky.client.reportCrossCheckVerdict("checker-run", 9, { verdict: "approve", reason_class: "approve", summary: "fixture", items: [] });
    assert.equal(sticky.requests.at(-1)?.body.claim_generation, 9);
    await sticky.client.claimCrossCheck(snapshot);
    assert.equal(sticky.requests.at(-1)?.body.active_snapshot !== undefined, features.length > 0);
  }
  const ordinary = fixture();
  await ordinary.client.register("w", undefined, 1, undefined, [], undefined, 0);
  for (const features of [["claim_generation_fence"], []]) {
    ordinary.setReply(() => json({ protocol_features: features }));
    await ordinary.client.heartbeat();
    await ordinary.client.postMessages("ordinary-run", [{ seq: 1, kind: "text", payload: { text: "hi" } }], 11);
    assert.equal(ordinary.requests.at(-1)?.body.claim_generation !== undefined, features.length > 0);
  }
});

it("strict fallback strips refresh and every sibling extension while retaining stats", async () => {
  const f = fixture({ protocol_features: [REFRESH, "heartbeat_outbox", "active_run_snapshot", "worker_residue_quarantine"] });
  await f.client.register("w", undefined, 2, undefined, [], undefined, 1);
  const stats = { mem_bytes: 100, mem_limit_bytes: null, source: "process" as const };
  let count = 0;
  f.setReply(() => count++ === 0 ? json({ error: "invalid request body" }, 400) : json({ protocol_features: [REFRESH] }));
  await f.client.heartbeat(stats,
    [{ run_id: "run", pending_messages: 1, pending_terminal: 0, stale_retired: 0, since: 1 }],
    snapshot, undefined, { cause: "fixture", latchedAt: "2026-10-14T00:00:00Z", runId: undefined, site: "pre_clone" });
  const bodies = heartbeatBodies(f);
  assert.ok(bodies[0]?.protocol_refresh);
  assert.ok(bodies[0]?.outbox);
  assert.ok(bodies[0]?.active_snapshot);
  assert.ok(bodies[0]?.residue_quarantine);
  assert.deepEqual(bodies[1], { version: "test", stats });
  assert.deepEqual(f.client.protocolFeatures, []);
});

it("register quota rejects fractional and negative values", async () => {
  for (const quota of [1.5, -1]) {
    const f = fixture({ worker_outbox_max_pending: quota });
    await f.client.register("w");
    assert.equal(f.client.workerOutboxMaxPending, undefined);
  }
});
