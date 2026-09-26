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
