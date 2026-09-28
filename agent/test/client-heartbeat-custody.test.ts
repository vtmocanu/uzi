import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";

import { WorkerClient, RequestError } from "../src/client.js";
import type { OutboxHeartbeatEntry } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// issue #1759 M3: heartbeat() returns the api's `worker.retaining_unpublished_work`
// custody flag (the DinD prune's fail-closed input): the boolean when the response
// decodes and carries one, undefined otherwise, on the first attempt AND on the
// strict-decode stripped retry.

interface Reply {
  status: number;
  body?: string;
}

let server: http.Server;
let url: string;
let heartbeatReplies: Array<(i: number) => Reply>;
let heartbeatCount = 0;

beforeEach(async () => {
  heartbeatCount = 0;
  heartbeatReplies = [];
  server = http.createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      let reply: Reply;
      if ((req.url ?? "").endsWith("/register")) {
        reply = { status: 200, body: JSON.stringify({ worker_id: "w1", protocol_features: ["heartbeat_outbox"] }) };
      } else if ((req.url ?? "").endsWith("/heartbeat")) {
        const i = heartbeatCount++;
        reply = (heartbeatReplies[0] ?? (() => ({ status: 204 })))(i);
      } else {
        reply = { status: 404 };
      }
      res.writeHead(reply.status, reply.body !== undefined ? { "Content-Type": "application/json" } : {});
      res.end(reply.body ?? "");
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});

afterEach(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

function client(): WorkerClient {
  return new WorkerClient(url, "worker-join-token-0123456789", "0.1.0-test", nullLogger(), {
    sleep: async () => {},
  });
}

function reply(fn: (i: number) => Reply): void {
  heartbeatReplies = [fn];
}

const ok = (body: unknown): Reply => ({ status: 200, body: JSON.stringify(body) });

describe("heartbeat custody flag (issue #1759)", () => {
  it("returns false / true verbatim from worker.retaining_unpublished_work", async () => {
    reply(() => ok({ worker: { id: "w1", retaining_unpublished_work: false } }));
    assert.strictEqual(await client().heartbeat(), false);
    reply(() => ok({ worker: { id: "w1", retaining_unpublished_work: true } }));
    assert.strictEqual(await client().heartbeat(), true);
  });

  it("returns undefined when the field is absent, non-boolean, or the body does not decode", async () => {
    const bodies: Reply[] = [
      { status: 204 },
      ok({ worker: { id: "w1" } }),
      ok({ worker: { retaining_unpublished_work: "false" } }),
      ok({ worker: { retaining_unpublished_work: 0 } }),
      ok({ worker: { retaining_unpublished_work: null } }),
      ok({ retaining_unpublished_work: false }), // not under `worker`
      ok({ worker: null }),
      ok(null),
      ok([false]),
      { status: 200, body: "{not json" },
      { status: 200, body: "" },
    ];
    for (const b of bodies) {
      reply(() => b);
      assert.strictEqual(await client().heartbeat(), undefined, `for body ${JSON.stringify(b)}`);
    }
  });

  it("still throws on a non-2xx heartbeat (no custody value invented)", async () => {
    reply(() => ({ status: 500, body: JSON.stringify({ error: "boom" }) }));
    await assert.rejects(client().heartbeat(), RequestError);
  });

  it("returns the flag from the stripped retry after a strict-decode rollback 400", async () => {
    reply((i) =>
      i === 0
        ? { status: 400, body: JSON.stringify({ error: "invalid request body" }) }
        : ok({ worker: { retaining_unpublished_work: false } }),
    );
    const c = client();
    await c.register("w");
    const entry: OutboxHeartbeatEntry = {
      run_id: "11111111-1111-1111-1111-111111111111",
      pending_messages: 1,
      pending_terminal: 0,
      stale_retired: 0,
      since: 1,
    };
    assert.strictEqual(await c.heartbeat(undefined, [entry]), false);
    assert.strictEqual(heartbeatCount, 2, "exactly one stripped retry");
    assert.strictEqual(c.hasFeature("heartbeat_outbox"), false, "the fallback still clears the feature set");
  });
});

// PRD #1809 D5: the heartbeat response also carries the api's recycle threshold
// (`worker.disk_pressure_threshold`), from which the worker derives its soft threshold. The
// client keeps the last valid value; absent (an older api) or garbage never replaces it, and
// never disturbs the custody flag decoded from the same body.
describe("heartbeat disk pressure threshold (PRD #1809 D5)", () => {
  it("records a present threshold alongside the custody flag", async () => {
    const c = client();
    assert.strictEqual(c.diskPressureThreshold, undefined, "nothing before the first heartbeat");
    reply(() => ok({ worker: { retaining_unpublished_work: true, disk_pressure_threshold: 0.85 } }));
    assert.strictEqual(await c.heartbeat(), true, "the custody flag is unchanged by the new field");
    assert.strictEqual(c.diskPressureThreshold, 0.85);
    reply(() => ok({ worker: { retaining_unpublished_work: false, disk_pressure_threshold: 1 } }));
    await c.heartbeat();
    assert.strictEqual(c.diskPressureThreshold, 1, "1 is inside (0, 1]");
  });

  it("stays undefined on an api that never sends it (the controller then assumes 0.90)", async () => {
    const c = client();
    reply(() => ok({ worker: { retaining_unpublished_work: false } }));
    assert.strictEqual(await c.heartbeat(), false);
    assert.strictEqual(c.diskPressureThreshold, undefined);
  });

  it("rejects garbage and keeps the last good value", async () => {
    const c = client();
    reply(() => ok({ worker: { disk_pressure_threshold: 0.9 } }));
    await c.heartbeat();
    const garbage: Reply[] = [
      ok({ worker: { disk_pressure_threshold: 0 } }),
      ok({ worker: { disk_pressure_threshold: -0.5 } }),
      ok({ worker: { disk_pressure_threshold: 1.5 } }),
      ok({ worker: { disk_pressure_threshold: "0.8" } }),
      ok({ worker: { disk_pressure_threshold: null } }),
      ok({ worker: {} }),
      ok({ disk_pressure_threshold: 0.5 }), // not under `worker`
      { status: 200, body: '{"worker":{"disk_pressure_threshold":1e999}}' }, // Infinity
      { status: 200, body: "{not json" },
      { status: 204 },
    ];
    for (const b of garbage) {
      reply(() => b);
      assert.strictEqual(await c.heartbeat(), undefined, `custody stays fail-closed for ${JSON.stringify(b)}`);
      assert.strictEqual(c.diskPressureThreshold, 0.9, `last good value kept for ${JSON.stringify(b)}`);
    }
  });
});
