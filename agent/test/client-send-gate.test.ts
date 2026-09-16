import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { FakeApi } from "./fake-api.js";
import { nullLogger } from "./helpers.js";
import { WorkerClient, RequestError } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";

// PRD #1247 fix round (m3): the send-gate spine for the MESSAGE path. postMessages routes
// the claim_generation stamp through two REUSABLE helpers on WorkerClient:
//   includeClaimGeneration(generation?)  — the eligibility predicate (0 is chat's legacy
//     sentinel; a credential_switch_v1 CAPABILITY worker stamps optimistically, a
//     non-capability #1391-era worker keeps the claim_generation_fence FEATURE gate)
//   withGenerationFallback(included, send) — the skew-safe strict-decode strip+retry (a
//     capability worker stays optimistic, a non-capability worker sticky-clears its features)
// A worker's capability is set only by calling register() with a protocolCapabilities array;
// the negotiated feature is set by register()'s captured RegisterResponse.protocol_features.

const TOKEN = "worker-join-token-0123456789";

const MSG: OutgoingMessage[] = [{ seq: 1, kind: "text", payload: { text: "hi" } }];
const MSG2: OutgoingMessage[] = [{ seq: 2, kind: "text", payload: { text: "bye" } }];

// ── The eligibility predicate + an ACCEPTING fenced api (FakeApi records the stamp) ──
describe("postMessages send-gate eligibility (PRD #1247 fix round)", () => {
  let api: FakeApi;
  let baseUrl: string;

  beforeEach(async () => {
    api = new FakeApi(TOKEN);
    baseUrl = await api.listen();
  });
  afterEach(async () => {
    await api.close();
  });

  function newClient(): WorkerClient {
    return new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
      sleep: async () => {},
      terminalRetrySchedule: [1, 1, 1],
    });
  }

  it("a credential_switch_v1 worker with NO cached feature STAMPS a gen>0 batch; a fenced api accepts it", async () => {
    const client = newClient();
    // FakeApi's register advertises no protocol_features, so claim_generation_fence is
    // NOT negotiated — the stamp rides purely on the capability's optimistic path.
    await client.register("cap-worker", undefined, 1, undefined, ["credential_switch_v1"]);
    await client.postMessages("run-1", MSG, 3);
    assert.deepStrictEqual(api.messageBatches, [{ runId: "run-1", claim_generation: 3, count: 1 }]);
    assert.deepStrictEqual(
      api.messages("run-1").map((m) => m.seq),
      [1],
      "the fenced api delivered the message once",
    );
  });

  it("a credential_switch_v1 worker OMITS claim_generation for chat's legacy sentinel (generation 0)", async () => {
    const client = newClient();
    await client.register("cap-worker", undefined, 1, undefined, ["credential_switch_v1"]);
    await client.postMessages("run-chat", MSG, 0);
    assert.deepStrictEqual(api.messageBatches, [{ runId: "run-chat", claim_generation: undefined, count: 1 }]);
  });

  it("a feature-only (non-capability) worker OMITS claim_generation for generation 0", async () => {
    api.setRegisterProtocolFeatures(["claim_generation_fence"]);
    const client = newClient();
    await client.register("feature-worker"); // no protocol capabilities advertised
    await client.postMessages("run-chat", MSG, 0);
    assert.deepStrictEqual(api.messageBatches, [{ runId: "run-chat", claim_generation: undefined, count: 1 }]);
  });

  it("a feature-only (non-capability) worker STAMPS a gen>0 batch through the negotiated feature", async () => {
    api.setRegisterProtocolFeatures(["claim_generation_fence"]);
    const client = newClient();
    await client.register("feature-worker");
    await client.postMessages("run-1", MSG, 2);
    assert.deepStrictEqual(api.messageBatches, [{ runId: "run-1", claim_generation: 2, count: 1 }]);
  });

  it("a bare worker (neither capability nor feature) OMITS claim_generation even for gen>0", async () => {
    const client = newClient();
    await client.register("bare-worker");
    await client.postMessages("run-1", MSG, 5);
    assert.deepStrictEqual(api.messageBatches, [{ runId: "run-1", claim_generation: undefined, count: 1 }]);
  });
});

// ── The skew-safe strict-decode strip+retry fallback ────────────────────────────────
// FakeApi cannot answer the generic `invalid request body` 400 keyed on the field being
// present, so these use a purpose-built stub that inspects each /messages body and records
// every post with its answered status. It also answers register(), so a client's capability
// and negotiated feature are established the real way.
describe("postMessages send-gate fallback (PRD #1247 fix round)", () => {
  interface Post {
    body: Record<string, unknown>;
    status: number;
  }
  async function skewServer(
    registerBody: Record<string, unknown>,
    respond: (body: Record<string, unknown>, attempt: number) => { status: number; body: unknown },
  ): Promise<{ url: string; posts: Post[]; close: () => Promise<void> }> {
    const posts: Post[] = [];
    const server = http.createServer((req, res) => {
      let raw = "";
      req.on("data", (c) => (raw += String(c)));
      req.on("end", () => {
        const body = raw ? (JSON.parse(raw) as Record<string, unknown>) : {};
        const url = new URL(req.url ?? "/", "http://fake");
        if (req.method === "POST" && url.pathname === "/api/worker/register") {
          res.writeHead(200, { "Content-Type": "application/json" });
          res.end(JSON.stringify(registerBody));
          return;
        }
        if (req.method === "POST" && url.pathname.endsWith("/messages")) {
          const attempt = posts.length; // 0-based index of THIS post
          const { status, body: respBody } = respond(body, attempt);
          posts.push({ body, status });
          res.writeHead(status, { "Content-Type": "application/json" });
          res.end(JSON.stringify(respBody));
          return;
        }
        res.writeHead(404, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "not found", path: url.pathname }));
      });
    });
    await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
    server.unref();
    const { port } = server.address() as AddressInfo;
    return {
      url: `http://127.0.0.1:${port}`,
      posts,
      close: () => new Promise<void>((r) => server.close(() => r())),
    };
  }

  function clientFor(url: string): WorkerClient {
    return new WorkerClient(url, TOKEN, "0.1.0-test", nullLogger(), {
      sleep: async () => {},
      terminalRetrySchedule: [1, 1, 1],
    });
  }

  const STRICT_DECODE_400 = { status: 400, body: { error: "invalid request body" } } as const;

  it("old→new skew: a capability worker strips+retries ONCE, delivers exactly once, then STAMPS again after the api rolls forward", async () => {
    // attempt 0 (field present): a rolled-back api strict-decodes the unknown field → 400.
    // attempt 1 (stripped retry) and everything after: the api accepts.
    const srv = await skewServer({ worker_id: "w1" }, (_body, attempt) =>
      attempt === 0 ? STRICT_DECODE_400 : { status: 200, body: { accepted: 1 } },
    );
    try {
      const client = clientFor(srv.url);
      await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);

      // First batch: stamped → rejected → stripped → retried → delivered.
      await client.postMessages("run-1", MSG, 4);
      assert.strictEqual(srv.posts.length, 2, "one stamped attempt + one stripped retry");
      assert.strictEqual("claim_generation" in srv.posts[0]!.body, true, "first attempt carried the field");
      assert.strictEqual("claim_generation" in srv.posts[1]!.body, false, "the retry stripped the field");
      assert.strictEqual(
        srv.posts.filter((p) => p.status === 200).length,
        1,
        "the batch was delivered EXACTLY ONCE (only the stripped retry was accepted)",
      );

      // Next batch: a capability worker stays optimistic (features NOT cleared), so it
      // STAMPS again — proving new→old→new never wedges.
      await client.postMessages("run-1", MSG2, 4);
      assert.strictEqual(srv.posts.length, 3, "the rolled-forward batch is a single stamped post, no re-strip");
      assert.strictEqual("claim_generation" in srv.posts[2]!.body, true, "the capability worker stamps again");
      assert.strictEqual(srv.posts[2]!.body.claim_generation, 4);
    } finally {
      await srv.close();
    }
  });

  it("non-capability worker: strips+retries once AND sticky-clears its features, so a later batch OMITS the field", async () => {
    const srv = await skewServer(
      { worker_id: "w1", protocol_features: ["claim_generation_fence"] },
      (_body, attempt) => (attempt === 0 ? STRICT_DECODE_400 : { status: 200, body: {} }),
    );
    try {
      const client = clientFor(srv.url);
      await client.register("feat"); // feature negotiated, NO capability advertised

      // First batch: stamped via the feature → 400 → clearFeatures() → stripped retry.
      await client.postMessages("run-1", MSG, 7);
      assert.strictEqual(srv.posts.length, 2);
      assert.strictEqual("claim_generation" in srv.posts[0]!.body, true, "first attempt carried the field");
      assert.strictEqual("claim_generation" in srv.posts[1]!.body, false, "the retry stripped the field");

      // Second batch: the feature was sticky-cleared and there is no capability, so the
      // send-gate now OMITS the field entirely — one post, no field, no retry.
      await client.postMessages("run-1", MSG2, 7);
      assert.strictEqual(srv.posts.length, 3, "a single post, no fallback retry needed");
      assert.strictEqual(
        "claim_generation" in srv.posts[2]!.body,
        false,
        "sticky-clear: the later batch omits the field",
      );
    } finally {
      await srv.close();
    }
  });

  it("genuine-400 control: a non-strict-decode 400 propagates unchanged — the field is NOT stripped and there is no retry", async () => {
    // A dedicated unstorable/invalid-message 400 has a DIFFERENT body; isStrictDecodeError
    // is false, so it must propagate so the batcher's bisection still runs.
    const srv = await skewServer({ worker_id: "w1" }, () => ({
      status: 400,
      body: { error: "message is not storable" },
    }));
    try {
      const client = clientFor(srv.url);
      await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
      await assert.rejects(
        client.postMessages("run-1", MSG, 9),
        (e: unknown) => e instanceof RequestError && e.status === 400,
      );
      assert.strictEqual(srv.posts.length, 1, "a genuine 400 is not retried");
      assert.strictEqual(
        "claim_generation" in srv.posts[0]!.body,
        true,
        "the field is not stripped off a genuine-poison batch",
      );
    } finally {
      await srv.close();
    }
  });
});
