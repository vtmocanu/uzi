import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";

import { WorkerClient, RequestError, isStrictDecodeError } from "../src/client.js";
import type { OutboxHeartbeatEntry, OutgoingMessage } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// PRD #1391 M2/M5 — the client's feature negotiation and the strict-decode rollback
// fallbacks, driven against a programmable HTTP server so each test states the api's
// exact wire behaviour (which feature it advertises, whether it 400s and with which
// body) and asserts what the client puts on the wire in response.

const TOKEN = "worker-join-token-0123456789";

interface Recorded {
  kind: "register" | "heartbeat" | "messages" | "other";
  body: Record<string, unknown> | undefined;
}
interface Reply {
  status: number;
  body?: string;
}

interface ProgServer {
  url: string;
  close: () => Promise<void>;
  requests: Recorded[];
  cfg: {
    features: string[];
    heartbeat: (callIndex: number) => Reply;
    messages: (callIndex: number) => Reply;
  };
  countOf: (kind: Recorded["kind"]) => number;
}

const INVALID_BODY: Reply = { status: 400, body: JSON.stringify({ error: "invalid request body" }) };
const OK: Reply = { status: 204 };

async function startServer(): Promise<ProgServer> {
  const requests: Recorded[] = [];
  const cfg: ProgServer["cfg"] = {
    features: [],
    heartbeat: () => OK,
    messages: () => OK,
  };
  const countOf = (kind: Recorded["kind"]): number => requests.filter((r) => r.kind === kind).length;

  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const raw = Buffer.concat(chunks).toString("utf8");
      let body: Record<string, unknown> | undefined;
      try {
        body = raw ? (JSON.parse(raw) as Record<string, unknown>) : undefined;
      } catch {
        body = undefined;
      }
      const url = req.url ?? "";
      let reply: Reply;
      if (url.endsWith("/register")) {
        requests.push({ kind: "register", body });
        reply = { status: 200, body: JSON.stringify({ worker_id: "w1", protocol_features: cfg.features }) };
      } else if (url.endsWith("/heartbeat")) {
        reply = cfg.heartbeat(countOf("heartbeat"));
        requests.push({ kind: "heartbeat", body });
      } else if (url.endsWith("/messages")) {
        reply = cfg.messages(countOf("messages"));
        requests.push({ kind: "messages", body });
      } else {
        requests.push({ kind: "other", body });
        reply = { status: 404 };
      }
      res.writeHead(reply.status, reply.body ? { "Content-Type": "application/json" } : {});
      res.end(reply.body ?? "");
    });
  });

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  return {
    url: `http://127.0.0.1:${port}`,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
    requests,
    cfg,
    countOf,
  };
}

let srv: ProgServer;
beforeEach(async () => {
  srv = await startServer();
});
afterEach(async () => {
  await srv.close();
});

function newClient(): WorkerClient {
  return new WorkerClient(srv.url, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    terminalRetrySchedule: [1, 1, 1],
  });
}

const entry: OutboxHeartbeatEntry = {
  run_id: "11111111-1111-1111-1111-111111111111",
  pending_messages: 3,
  pending_terminal: 0,
  stale_retired: 0,
  since: 1_700_000_000_000,
};
const msg: OutgoingMessage = { seq: 1, kind: "text", payload: { text: "hi" } };

function heartbeats(): Recorded[] {
  return srv.requests.filter((r) => r.kind === "heartbeat");
}
function messages(): Recorded[] {
  return srv.requests.filter((r) => r.kind === "messages");
}

describe("heartbeat outbox negotiation (PRD #1391 M5)", () => {
  it("does NOT attach `outbox` when the server never advertised heartbeat_outbox", async () => {
    srv.cfg.features = []; // an older api
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, [entry]);

    const hb = heartbeats();
    assert.strictEqual(hb.length, 1);
    assert.ok(hb[0]!.body && !("outbox" in hb[0]!.body), "send-only-when-advertised: the outbox field must be absent");
  });

  it("attaches `outbox` when heartbeat_outbox is negotiated AND there is depth to report", async () => {
    srv.cfg.features = ["heartbeat_outbox"];
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, [entry]);

    const hb = heartbeats();
    assert.strictEqual(hb.length, 1);
    assert.deepStrictEqual(hb[0]!.body?.outbox, [entry], "the negotiated depth rides the heartbeat");
  });

  it("rollback: a generic invalid-request-body 400 on an outbox-carrying heartbeat retries STRIPPED and CLEARS the feature set", async () => {
    srv.cfg.features = ["heartbeat_outbox"];
    // The first heartbeat 400s (a rolled-back api strict-decoding the outbox field);
    // every later heartbeat is fine.
    srv.cfg.heartbeat = (i) => (i === 0 ? INVALID_BODY : OK);
    const c = newClient();
    await c.register("w");

    // Must NOT throw — a heartbeat is never lost to a rolled-back api.
    await c.heartbeat(undefined, [entry]);

    const hb1 = heartbeats();
    assert.strictEqual(hb1.length, 2, "the 400 triggered exactly one stripped retry");
    assert.deepStrictEqual(hb1[0]!.body?.outbox, [entry], "the first attempt carried the outbox");
    assert.ok(hb1[1]!.body && !("outbox" in hb1[1]!.body), "the stripped retry carried NO outbox");
    assert.strictEqual(c.hasFeature("heartbeat_outbox"), false, "a stripped success clears the whole feature set");

    // And a subsequent heartbeat sends nothing negotiated, even with depth to report.
    await c.heartbeat(undefined, [entry]);
    const hb2 = heartbeats();
    assert.strictEqual(hb2.length, 3);
    assert.ok(hb2[2]!.body && !("outbox" in hb2[2]!.body), "features stay cleared until restart");
  });
});

describe("messages claim_generation negotiation (PRD #1391 M2/M5)", () => {
  it("sends claim_generation ONLY when claim_generation_fence is negotiated", async () => {
    srv.cfg.features = ["claim_generation_fence"];
    const c = newClient();
    await c.register("w");
    await c.postMessages("run-x", [msg], 5);

    const m = messages();
    assert.strictEqual(m.length, 1);
    assert.strictEqual(m[0]!.body?.claim_generation, 5, "the fenced api receives the claim generation");
  });

  it("does NOT send claim_generation when the feature is not advertised, even if a generation is supplied", async () => {
    srv.cfg.features = []; // Run A's own api never advertises the fence
    const c = newClient();
    await c.register("w");
    await c.postMessages("run-x", [msg], 5);

    const m = messages();
    assert.strictEqual(m.length, 1);
    assert.ok(m[0]!.body && !("claim_generation" in m[0]!.body), "byte-identical wire on an un-fenced api");
  });

  it("messages-first rollback: an invalid-request-body 400 clears features and retries the IDENTICAL batch WITHOUT the field, returning only the second response", async () => {
    srv.cfg.features = ["claim_generation_fence"];
    srv.cfg.messages = (i) => (i === 0 ? INVALID_BODY : OK);
    const c = newClient();
    await c.register("w");

    // Resolves via the second (stripped) response, not the first 400.
    await c.postMessages("run-x", [msg], 5);

    const m = messages();
    assert.strictEqual(m.length, 2, "exactly one stripped retry of the same batch");
    assert.strictEqual(m[0]!.body?.claim_generation, 5, "the first attempt carried the generation");
    assert.ok(m[1]!.body && !("claim_generation" in m[1]!.body), "the retry stripped the generation");
    assert.deepStrictEqual(
      (m[0]!.body?.messages as unknown[]),
      (m[1]!.body?.messages as unknown[]),
      "the retry is the IDENTICAL batch, only the fence field removed",
    );
    assert.strictEqual(c.hasFeature("claim_generation_fence"), false, "the feature set is cleared after the fallback");
  });

  it("genuine-poison control: a 400 with the DEDICATED unstorable body is NOT a fallback trigger — it propagates and the generation is never stripped", async () => {
    srv.cfg.features = ["claim_generation_fence"];
    // A DIFFERENT 400 body: the unstorable/invalid-message answer, not strict-decode.
    srv.cfg.messages = () => ({ status: 400, body: JSON.stringify({ error: "message payload rejected: unstorable" }) });
    const c = newClient();
    await c.register("w");

    await assert.rejects(
      c.postMessages("run-x", [msg], 5),
      (err: unknown) => err instanceof RequestError && err.status === 400,
      "a genuine-poison 400 must propagate unchanged so the batcher's bisection runs",
    );

    const m = messages();
    assert.strictEqual(m.length, 1, "no stripped retry on a genuine-poison 400");
    assert.strictEqual(m[0]!.body?.claim_generation, 5, "the generation is NOT stripped off a poison batch");
    assert.strictEqual(c.hasFeature("claim_generation_fence"), true, "the feature set is left intact");
  });
});

describe("isStrictDecodeError (PRD #1391 D9)", () => {
  it("matches ONLY a 400 whose body is the generic invalid-request-body answer", () => {
    assert.strictEqual(isStrictDecodeError(new RequestError("POST", "/x", 400, "invalid request body")), true);
    assert.strictEqual(isStrictDecodeError(new RequestError("POST", "/x", 400, '{"error":"invalid request body"}')), true);
    // A different 400 body (the dedicated unstorable/invalid-message answer) is NOT it.
    assert.strictEqual(isStrictDecodeError(new RequestError("POST", "/x", 400, "message payload rejected")), false);
    // The status must be 400, and the error must be a RequestError.
    assert.strictEqual(isStrictDecodeError(new RequestError("POST", "/x", 500, "invalid request body")), false);
    assert.strictEqual(isStrictDecodeError(new Error("invalid request body")), false);
  });
});
