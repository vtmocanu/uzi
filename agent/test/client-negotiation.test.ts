import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";

import { WorkerClient, RequestError, isStrictDecodeError } from "../src/client.js";
import type { ActiveSnapshot, OutboxHeartbeatEntry, OutgoingMessage } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// PRD #1391 M2/M5 — the client's feature negotiation and the strict-decode rollback
// fallbacks, driven against a programmable HTTP server so each test states the api's
// exact wire behaviour (which feature it advertises, whether it 400s and with which
// body) and asserts what the client puts on the wire in response.

const TOKEN = "worker-join-token-0123456789";

interface Recorded {
  kind: "register" | "heartbeat" | "messages" | "claim" | "other";
  url: string;
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
    /** PRD #1390 M2a: the per-registration nonce the api mints; echoed by the client on
     *  every snapshot it sends. undefined ⇒ the register response omits the field. */
    registerNonce: string | undefined;
    /** PRD #1391 Run B M4: the server-side terminal-pending outbox cap the register response returns.
     *  undefined ⇒ the field is omitted (an older api). */
    workerOutboxMaxPending: number | undefined;
    heartbeat: (callIndex: number) => Reply;
    messages: (callIndex: number) => Reply;
    claim: (callIndex: number) => Reply;
    /** PRD #1391 Run B M4: the GET /runs/{id}/ownership reply (status + additive claim_generation). */
    ownership: () => Reply;
  };
  countOf: (kind: Recorded["kind"]) => number;
}

const INVALID_BODY: Reply = { status: 400, body: JSON.stringify({ error: "invalid request body" }) };
const OK: Reply = { status: 204 };

async function startServer(): Promise<ProgServer> {
  const requests: Recorded[] = [];
  const cfg: ProgServer["cfg"] = {
    features: [],
    registerNonce: undefined,
    workerOutboxMaxPending: undefined,
    heartbeat: () => OK,
    messages: () => OK,
    claim: () => OK,
    ownership: () => ({ status: 200, body: JSON.stringify({ status: "running" }) }),
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
        requests.push({ kind: "register", url, body });
        const registerBody: Record<string, unknown> = { worker_id: "w1", protocol_features: cfg.features };
        if (cfg.registerNonce !== undefined) registerBody.register_nonce = cfg.registerNonce;
        if (cfg.workerOutboxMaxPending !== undefined) registerBody.worker_outbox_max_pending = cfg.workerOutboxMaxPending;
        reply = { status: 200, body: JSON.stringify(registerBody) };
      } else if (url.endsWith("/ownership")) {
        requests.push({ kind: "other", url, body });
        reply = cfg.ownership();
      } else if (url.includes("/message-gaps?")) {
        requests.push({ kind: "other", url, body });
        reply = { status: 200, body: JSON.stringify({ gaps: [] }) };
      } else if (url.endsWith("/heartbeat")) {
        reply = cfg.heartbeat(countOf("heartbeat"));
        requests.push({ kind: "heartbeat", url, body });
      } else if (url.endsWith("/messages")) {
        reply = cfg.messages(countOf("messages"));
        requests.push({ kind: "messages", url, body });
      } else if (url.endsWith("/runs/claim")) {
        reply = cfg.claim(countOf("claim"));
        requests.push({ kind: "claim", url, body });
      } else {
        requests.push({ kind: "other", url, body });
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

/** A #1390 worker snapshot: one run at `running`, terminal_pending false, no overflow.
 *  `register_nonce` is OMITTED here on purpose — the client stamps it on send. */
function snapshot(epoch: number): ActiveSnapshot {
  return {
    snapshot_epoch: epoch,
    active: [
      {
        run_id: "22222222-2222-2222-2222-222222222222",
        claim_generation: 4,
        phase: "running",
        terminal_pending: false,
      },
    ],
    pending_overflow: false,
  };
}

function heartbeats(): Recorded[] {
  return srv.requests.filter((r) => r.kind === "heartbeat");
}
function messages(): Recorded[] {
  return srv.requests.filter((r) => r.kind === "messages");
}
function claims(): Recorded[] {
  return srv.requests.filter((r) => r.kind === "claim");
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

describe("active-run snapshot negotiation (PRD #1390 M2a)", () => {
  it("does NOT attach `active_snapshot` when the server never advertised active_run_snapshot", async () => {
    srv.cfg.features = []; // an older api
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, undefined, snapshot(1));

    const hb = heartbeats();
    assert.strictEqual(hb.length, 1);
    assert.ok(
      hb[0]!.body && !("active_snapshot" in hb[0]!.body),
      "send-only-when-advertised: the snapshot field must be absent",
    );
  });

  it("attaches `active_snapshot` with the ECHOED register nonce and epoch when negotiated", async () => {
    srv.cfg.features = ["active_run_snapshot"];
    srv.cfg.registerNonce = "nonce-abc";
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, undefined, snapshot(1));

    const hb = heartbeats();
    assert.strictEqual(hb.length, 1);
    const snap = hb[0]!.body?.active_snapshot as Record<string, unknown> | undefined;
    assert.ok(snap, "the negotiated snapshot rides the heartbeat");
    assert.strictEqual(snap.register_nonce, "nonce-abc", "the client stamps the echoed nonce");
    assert.strictEqual(snap.snapshot_epoch, 1, "the caller's epoch rides through");
    assert.strictEqual(snap.pending_overflow, false, "a #1390 worker never overflows");
    assert.deepStrictEqual(
      snap.active,
      [
        {
          run_id: "22222222-2222-2222-2222-222222222222",
          claim_generation: 4,
          phase: "running",
          terminal_pending: false,
        },
      ],
      "the live-run entry rides verbatim, terminal_pending false",
    );
  });

  it("epoch is monotonic across successive heartbeats (a later heartbeat carries a higher epoch)", async () => {
    srv.cfg.features = ["active_run_snapshot"];
    srv.cfg.registerNonce = "n1";
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, undefined, snapshot(1));
    await c.heartbeat(undefined, undefined, snapshot(2));

    const hb = heartbeats();
    assert.strictEqual(hb.length, 2);
    const s0 = hb[0]!.body?.active_snapshot as Record<string, unknown> | undefined;
    const s1 = hb[1]!.body?.active_snapshot as Record<string, unknown> | undefined;
    assert.ok(s0 && s1, "both heartbeats carried a snapshot");
    assert.ok((s1.snapshot_epoch as number) > (s0.snapshot_epoch as number), "the second heartbeat's epoch is strictly greater");
  });

  it("the run-lane claim carries `active_snapshot` (nonce-stamped) when negotiated", async () => {
    srv.cfg.features = ["active_run_snapshot"];
    srv.cfg.registerNonce = "nonce-xyz";
    const c = newClient();
    await c.register("w");
    const res = await c.claimRun(snapshot(1));
    assert.strictEqual(res, null, "204 = queue idle");

    const cl = claims();
    assert.strictEqual(cl.length, 1);
    const snap = cl[0]!.body?.active_snapshot as Record<string, unknown> | undefined;
    assert.ok(snap, "the claim body carries the snapshot");
    assert.strictEqual(snap.register_nonce, "nonce-xyz", "the claim snapshot echoes the nonce");
    assert.strictEqual(snap.snapshot_epoch, 1);
  });

  it("the run-lane claim posts an EMPTY body (no snapshot) when NOT negotiated", async () => {
    srv.cfg.features = []; // an older api
    const c = newClient();
    await c.register("w");
    await c.claimRun(snapshot(1));

    const cl = claims();
    assert.strictEqual(cl.length, 1);
    assert.ok(
      !cl[0]!.body || !("active_snapshot" in cl[0]!.body),
      "an un-negotiated claim posts {} — an old api ignores the unread body",
    );
  });

  it("whole-set fallback: a strict-decode 400 on a snapshot-carrying heartbeat retries STRIPPED, clears the feature set, and a credential_switch_v1 worker STILL stamps claim_generation afterward", async () => {
    srv.cfg.features = ["active_run_snapshot"];
    srv.cfg.registerNonce = "n1";
    // The first heartbeat 400s (a rolled-back api strict-decoding the snapshot field);
    // every later heartbeat is fine.
    srv.cfg.heartbeat = (i) => (i === 0 ? INVALID_BODY : OK);
    const c = newClient();
    // Register as a credential_switch_v1 CAPABILITY worker (protocolCapabilities arg).
    await c.register("w", undefined, undefined, undefined, ["credential_switch_v1"]);

    // Must NOT throw — a heartbeat is never lost to a rolled-back api.
    await c.heartbeat(undefined, undefined, snapshot(1));

    const hb = heartbeats();
    assert.strictEqual(hb.length, 2, "the 400 triggered exactly one stripped retry");
    assert.ok(hb[0]!.body?.active_snapshot, "the first attempt carried the snapshot");
    assert.ok(
      hb[1]!.body && !("active_snapshot" in hb[1]!.body),
      "the stripped retry carried NO active_snapshot",
    );
    assert.strictEqual(
      c.hasFeature("active_run_snapshot"),
      false,
      "a stripped success clears the WHOLE negotiated feature set",
    );

    // The load-bearing regression: the sticky credential_switch_v1 capability is stored
    // SEPARATELY from the feature set and is NOT cleared, so a subsequent messages call
    // still stamps claim_generation optimistically.
    await c.postMessages("run-x", [msg], 7);
    const m = messages();
    assert.strictEqual(m.length, 1);
    assert.strictEqual(
      m[0]!.body?.claim_generation,
      7,
      "the credential capability survives the whole-set clear and keeps stamping the generation",
    );
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

describe("register snapshot + cap + ownership generation (PRD #1391 Run B M4)", () => {
  it("carries the initial active_snapshot on the register body (nonce-EXEMPT — sent verbatim)", async () => {
    srv.cfg.features = ["active_run_snapshot"];
    srv.cfg.registerNonce = "nonce-xyz";
    const c = newClient();
    const initial: ActiveSnapshot = { snapshot_epoch: 1, active: [], pending_overflow: true };
    await c.register("w", undefined, undefined, undefined, undefined, initial);

    const reg = srv.requests.find((r) => r.kind === "register");
    assert.deepStrictEqual(
      reg!.body?.active_snapshot,
      initial,
      "the boot snapshot rides the register body verbatim, WITHOUT a stamped nonce (exempt)",
    );
  });

  it("omits active_snapshot when no initial snapshot is passed (byte-identical register wire)", async () => {
    const c = newClient();
    await c.register("w");
    const reg = srv.requests.find((r) => r.kind === "register");
    assert.ok(reg!.body && !("active_snapshot" in reg!.body), "an ordinary worker sends no snapshot on register");
  });

  it("captures worker_outbox_max_pending off the register response", async () => {
    srv.cfg.workerOutboxMaxPending = 32;
    const c = newClient();
    await c.register("w");
    assert.strictEqual(c.workerOutboxMaxPending, 32, "the server cap is stashed for the registry to read");
  });

  it("leaves workerOutboxMaxPending undefined when the api omits it (older api ⇒ cap-0 floor)", async () => {
    srv.cfg.workerOutboxMaxPending = undefined;
    const c = newClient();
    await c.register("w");
    assert.strictEqual(c.workerOutboxMaxPending, undefined, "an older api ⇒ undefined ⇒ the registry treats it as cap 0");
  });

  it("getRunOwnership passes claim_generation through from the probe body", async () => {
    srv.cfg.ownership = () => ({ status: 200, body: JSON.stringify({ status: "running", claim_generation: 7 }) });
    const c = newClient();
    const probe = await c.getRunOwnership("33333333-3333-3333-3333-333333333333");
    assert.strictEqual(probe.status, "running");
    assert.strictEqual(probe.claim_generation, 7, "the additive claim_generation flows through the full JSON body");
  });

  it("getRunOwnership leaves claim_generation undefined for an older api that omits it", async () => {
    srv.cfg.ownership = () => ({ status: 200, body: JSON.stringify({ status: "running" }) });
    const c = newClient();
    const probe = await c.getRunOwnership("33333333-3333-3333-3333-333333333333");
    assert.strictEqual(probe.claim_generation, undefined, "absent ⇒ undefined (the router cannot prove a mismatch)");
  });

  it("getMessageGaps sends the exact claim generation with the keyset page", async () => {
    const c = newClient();
    await c.getMessageGaps("33333333-3333-3333-3333-333333333333", 7, 42, 9, 3);

    const req = srv.requests.find((r) => r.url.includes("/message-gaps?"));
    assert.ok(req, "the message-gaps request reached the server");
    const query = new URL(req.url, srv.url).searchParams;
    assert.strictEqual(query.get("claim_generation"), "7", "the journal generation fences the read");
    assert.strictEqual(query.get("through"), "42");
    assert.strictEqual(query.get("limit"), "9");
    assert.strictEqual(query.get("cursor"), "3");
  });
});
