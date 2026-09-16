import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import { WorkerClient, RequestError } from "../src/client.js";

// PRD #1247 fix round (E): the /state report path joins the SAME claim_generation send-gate spine
// as postMessages and the completion RPCs. /state DisallowUnknownFields-decodes, and the M5b
// reportState closure stamps claim_generation on every mutating report, so a rolled-back api that
// predates the field 400s the report; with force-roll off a busy newer worker would wedge every run.
// reportState now routes through includeClaimGeneration + withGenerationFallback: stamp only for a
// generation>0 capability/feature worker, and on the EXACT strict-decode 400 strip the field and
// retry the identical report ONCE (capability non-sticky, non-capability sticky). The transient-retry
// loop, AbortSignal, 200/409 single-body ACK parse and terminal handling are preserved unchanged.

const TOKEN = "worker-join-token-0123456789";

// A raw skew server that answers /register with the given body and every /state POST via `respond`,
// recording each posted body + the status it answered. Mirrors client-send-gate.test.ts's helper but
// for /state (a 200/409 answers a `{run: {status}}` body that readRunAck parses).
interface Post {
  body: Record<string, unknown>;
  status: number;
}
async function stateServer(
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
      if (req.method === "POST" && url.pathname.endsWith("/state")) {
        const attempt = posts.length;
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

const OK_RUNNING = { status: 200, body: { run: { status: "running" } } } as const;
const STRICT_DECODE_400 = { status: 400, body: { error: "invalid request body" } } as const;

describe("reportState send-gate eligibility (PRD #1247 fix round E)", () => {
  const servers: { close: () => Promise<void> }[] = [];
  afterEach(async () => {
    await Promise.all(servers.splice(0).map((s) => s.close()));
  });
  async function accepting(registerBody: Record<string, unknown>) {
    const srv = await stateServer(registerBody, () => OK_RUNNING);
    servers.push(srv);
    return srv;
  }

  it("a credential_switch_v1 worker STAMPS a gen>0 running report", async () => {
    const srv = await accepting({ worker_id: "w1" }); // no negotiated feature — capability path only
    const client = clientFor(srv.url);
    await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
    await client.reportState("run-1", { status: "running", claim_generation: 3 });
    assert.strictEqual(srv.posts.length, 1);
    assert.strictEqual("claim_generation" in srv.posts[0]!.body, true, "the capability worker stamped");
    assert.strictEqual(srv.posts[0]!.body.claim_generation, 3);
  });

  it("a credential_switch_v1 worker OMITS claim_generation for chat's legacy sentinel (generation 0)", async () => {
    const srv = await accepting({ worker_id: "w1" });
    const client = clientFor(srv.url);
    await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
    await client.reportState("run-1", { status: "running", claim_generation: 0 });
    assert.strictEqual("claim_generation" in srv.posts[0]!.body, false, "0 is never sent");
  });

  it("a report that carries NO generation (undefined) omits the field, unchanged", async () => {
    const srv = await accepting({ worker_id: "w1" });
    const client = clientFor(srv.url);
    await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
    await client.reportState("run-1", { status: "completed" });
    assert.strictEqual("claim_generation" in srv.posts[0]!.body, false);
  });

  it("a feature-only (non-capability) worker STAMPS a gen>0 report through the negotiated feature", async () => {
    const srv = await accepting({ worker_id: "w1", protocol_features: ["claim_generation_fence"] });
    const client = clientFor(srv.url);
    await client.register("feat"); // no capability advertised
    await client.reportState("run-1", { status: "running", claim_generation: 5 });
    assert.strictEqual(srv.posts[0]!.body.claim_generation, 5, "the feature gate stamps it");
  });

  it("a bare worker (neither capability nor feature) OMITS claim_generation even for gen>0", async () => {
    const srv = await accepting({ worker_id: "w1" });
    const client = clientFor(srv.url);
    await client.register("bare");
    await client.reportState("run-1", { status: "running", claim_generation: 6 });
    assert.strictEqual("claim_generation" in srv.posts[0]!.body, false, "no capability, no feature ⇒ omit");
  });
});

describe("reportState send-gate fallback (PRD #1247 fix round E)", () => {
  it("old→new skew: a capability worker strips+retries ONCE, then STAMPS again after the api rolls forward", async () => {
    const srv = await stateServer({ worker_id: "w1" }, (_b, attempt) =>
      attempt === 0 ? STRICT_DECODE_400 : OK_RUNNING,
    );
    try {
      const client = clientFor(srv.url);
      await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);

      // First report: stamped → strict-decode 400 → stripped → retried → applied.
      const ack = await client.reportState("run-1", { status: "running", claim_generation: 4 });
      assert.strictEqual(ack.status, "running");
      assert.strictEqual(srv.posts.length, 2, "one stamped attempt + one stripped retry");
      assert.strictEqual("claim_generation" in srv.posts[0]!.body, true, "first attempt carried the field");
      assert.strictEqual("claim_generation" in srv.posts[1]!.body, false, "the retry stripped the field");
      assert.strictEqual(srv.posts.filter((p) => p.status === 200).length, 1, "applied EXACTLY once");

      // Next report: a capability worker stays optimistic (features NOT cleared), so it STAMPS
      // again — proving new→old→new never wedges the run.
      await client.reportState("run-1", { status: "completed", claim_generation: 4 });
      assert.strictEqual(srv.posts.length, 3, "the rolled-forward report is a single stamped post");
      assert.strictEqual("claim_generation" in srv.posts[2]!.body, true, "the capability worker stamps again");
    } finally {
      await srv.close();
    }
  });

  it("non-capability worker: strips+retries once AND sticky-clears its features, so a later report OMITS the field", async () => {
    const srv = await stateServer(
      { worker_id: "w1", protocol_features: ["claim_generation_fence"] },
      (_b, attempt) => (attempt === 0 ? STRICT_DECODE_400 : OK_RUNNING),
    );
    try {
      const client = clientFor(srv.url);
      await client.register("feat"); // feature negotiated, NO capability

      await client.reportState("run-1", { status: "running", claim_generation: 7 });
      assert.strictEqual(srv.posts.length, 2);
      assert.strictEqual("claim_generation" in srv.posts[0]!.body, true);
      assert.strictEqual("claim_generation" in srv.posts[1]!.body, false, "the retry stripped the field");

      // The feature was sticky-cleared and there is no capability, so the send-gate now OMITS
      // the field entirely — a single post, no retry.
      await client.reportState("run-1", { status: "completed", claim_generation: 7 });
      assert.strictEqual(srv.posts.length, 3, "a single post, no fallback retry needed");
      assert.strictEqual("claim_generation" in srv.posts[2]!.body, false, "sticky-clear: the later report omits it");
    } finally {
      await srv.close();
    }
  });

  it("genuine-400 control: a non-strict-decode 400 propagates unchanged — no strip, no retry", async () => {
    const srv = await stateServer({ worker_id: "w1" }, () => ({
      status: 400,
      body: { error: "state must be one of running, ..." },
    }));
    try {
      const client = clientFor(srv.url);
      await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
      await assert.rejects(
        client.reportState("run-1", { status: "running", claim_generation: 9 }),
        (e: unknown) => e instanceof RequestError && e.status === 400,
      );
      assert.strictEqual(srv.posts.length, 1, "a genuine 400 is not retried");
      assert.strictEqual("claim_generation" in srv.posts[0]!.body, true, "not stripped off a genuine-invalid report");
    } finally {
      await srv.close();
    }
  });

  it("the 409 stale-claim ACK path is unchanged: no strip, single post, staleClaim surfaced", async () => {
    // A 409 is not a 400, so isStrictDecodeError is false — the send-gate must NOT interfere with the
    // existing single-body ACK parse (disposition:'stale_claim' → ack.staleClaim).
    const srv = await stateServer({ worker_id: "w1" }, () => ({
      status: 409,
      body: { run: { status: "running" }, disposition: "stale_claim" },
    }));
    try {
      const client = clientFor(srv.url);
      await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
      const ack = await client.reportState("run-1", { status: "running", claim_generation: 8 });
      assert.strictEqual(ack.staleClaim, true, "the 409 disposition still surfaces as staleClaim");
      assert.strictEqual(ack.applied, false, "a 409 is not applied");
      assert.strictEqual(srv.posts.length, 1, "the 409 ACK path does not strip or retry");
      assert.strictEqual("claim_generation" in srv.posts[0]!.body, true, "the stamped field rode the report");
    } finally {
      await srv.close();
    }
  });

  it("the transient-retry loop is preserved: a 503 then 200 is retried, and the field still rides both posts", async () => {
    const srv = await stateServer({ worker_id: "w1" }, (_b, attempt) =>
      attempt === 0 ? { status: 503, body: { error: "unavailable" } } : OK_RUNNING,
    );
    try {
      const client = clientFor(srv.url);
      await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
      const ack = await client.reportState("run-1", { status: "running", claim_generation: 2 });
      assert.strictEqual(ack.status, "running");
      assert.strictEqual(srv.posts.length, 2, "the transient 503 was retried");
      // A transient retry is NOT the strict-decode fallback: the field is NOT stripped on retry.
      assert.strictEqual("claim_generation" in srv.posts[0]!.body, true);
      assert.strictEqual("claim_generation" in srv.posts[1]!.body, true, "a transient retry keeps the field");
    } finally {
      await srv.close();
    }
  });
});
