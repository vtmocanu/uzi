import { describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import { WorkerClient, RequestError } from "../src/client.js";

// PRD #1247 fix round (m4): the SAME send-gate spine m3 added for postMessages, now applied to
// the three completion RPCs — recordCompletionAttempt, requestCompletionPermit,
// requestCompletionHold. Each stamps `claim_generation` through the two REUSABLE helpers:
//   includeClaimGeneration(generation?)  — eligibility (0/unset omits; a credential_switch_v1
//     CAPABILITY worker stamps optimistically, a non-capability #1391-era worker keeps the
//     claim_generation_fence FEATURE gate)
//   withGenerationFallback(included, send) — the skew-safe strict-decode strip+retry-once (a
//     capability worker stays optimistic, a non-capability worker sticky-clears its features)
// Before m4 the three RPCs stamped behind a bare `if (claimGeneration !== undefined)` with NO
// feature gate and NO fallback, so a completion RPC racing an api rollback hard-400'd with no
// recovery. These tests prove the recovery AND that a genuine business error is NOT mistaken for
// a rollback.
//
// FakeApi has no /completion/attempt handler and cannot key a 400 on field-presence, so all three
// RPCs are driven through a purpose-built stub (mirrors m3's skewServer). It answers register the
// real way (so a client's capability/feature is established through register()) and records every
// completion POST with its endpoint, body, and answered status.

const TOKEN = "worker-join-token-0123456789";
const STRICT_DECODE_400 = { status: 400, body: { error: "invalid request body" } } as const;

type Endpoint = "attempt" | "permit" | "hold";

interface Post {
  endpoint: Endpoint;
  body: Record<string, unknown>;
  status: number;
}

function endpointOf(pathname: string): Endpoint | null {
  if (pathname.endsWith("/completion/attempt")) return "attempt";
  if (pathname.endsWith("/completion/permit")) return "permit";
  if (pathname.endsWith("/completion/hold")) return "hold";
  return null;
}

/**
 * A stub api that answers register() with `registerBody` and each completion POST via `respond`.
 * `attempt` is the 0-based index of THIS post among the posts to the SAME endpoint, so a test can
 * distinguish the field-carrying first request from its stripped retry. The stub only ever mutates
 * on the response it returns, so counting the accepted (2xx) posts is the effective-apply count.
 */
async function completionServer(
  registerBody: Record<string, unknown>,
  respond: (endpoint: Endpoint, body: Record<string, unknown>, attempt: number) => { status: number; body: unknown },
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
      const endpoint = endpointOf(url.pathname);
      if (req.method === "POST" && endpoint) {
        const attempt = posts.filter((p) => p.endpoint === endpoint).length;
        const { status, body: respBody } = respond(endpoint, body, attempt);
        posts.push({ endpoint, body, status });
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

// Register helpers: a capability worker advertises credential_switch_v1 (its runs are fenced
// server-side, so it stamps optimistically even with NO negotiated feature); a feature-only worker
// negotiates claim_generation_fence with NO capability; a bare worker has neither.
async function capabilityClient(url: string): Promise<WorkerClient> {
  const client = clientFor(url);
  await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
  return client;
}
async function featureClient(url: string): Promise<WorkerClient> {
  const client = clientFor(url);
  await client.register("feat"); // no capability advertised; the api's registerBody carries the feature
  return client;
}
async function bareClient(url: string): Promise<WorkerClient> {
  const client = clientFor(url);
  await client.register("bare");
  return client;
}

const ATTEMPT_ARGS = (claimGeneration?: number): {
  milestonesCompleted: string[];
  head: string | null;
  worktreeFingerprint: string | null;
  claimGeneration?: number;
} => ({ milestonesCompleted: ["m1"], head: "head-sha", worktreeFingerprint: "fp-1", claimGeneration });

const PERMIT_ARGS = (claimGeneration?: number): {
  contractRevision: number;
  branch: string;
  head: string;
  claimGeneration?: number;
} => ({ contractRevision: 2, branch: "agent/issue-1", head: "head-sha", claimGeneration });

const HOLD_ARGS = (claimGeneration?: number): { head: string; claimGeneration?: number } => ({
  head: "head-sha",
  claimGeneration,
});

function has(post: Post): boolean {
  return "claim_generation" in post.body;
}

// ── recordCompletionAttempt ──────────────────────────────────────────────────────────
describe("recordCompletionAttempt send-gate (PRD #1247 fix round)", () => {
  it("a credential_switch_v1 worker (no cached feature) STAMPS a gen>0 attempt; a fenced api records it once", async () => {
    const srv = await completionServer({ worker_id: "w1" }, () => ({
      status: 200,
      body: { unmet: [], attempt_count: 1 },
    }));
    try {
      const client = await capabilityClient(srv.url);
      const r = await client.recordCompletionAttempt("run-1", ATTEMPT_ARGS(3));
      assert.strictEqual(srv.posts.length, 1, "one attempt, no fallback needed");
      assert.strictEqual(has(srv.posts[0]!), true, "the capability worker stamped the field");
      assert.strictEqual(srv.posts[0]!.body.claim_generation, 3);
      assert.deepStrictEqual(r, { unmet: [], attemptCount: 1 });
    } finally {
      await srv.close();
    }
  });

  it("a non-capability worker (no feature) OMITS the field even for gen>0", async () => {
    const srv = await completionServer({ worker_id: "w1" }, () => ({
      status: 200,
      body: { unmet: [], attempt_count: 1 },
    }));
    try {
      const client = await bareClient(srv.url);
      await client.recordCompletionAttempt("run-1", ATTEMPT_ARGS(5));
      assert.strictEqual(srv.posts.length, 1);
      assert.strictEqual(has(srv.posts[0]!), false, "no capability, no feature ⇒ field omitted");
    } finally {
      await srv.close();
    }
  });

  it("old→new→old→new skew: strips+retries ONCE with ZERO side effects on the rejected request, then re-stamps after rollforward (capability stays optimistic)", async () => {
    // Keyed on field-presence: an old (rolled-back) api strict-decodes the unknown field → 400;
    // a stripped body is accepted. After the deploy rolls forward the field is accepted too.
    // `applies` counts ONLY accepted requests, so it is the effective-apply count — the rejected
    // first request must leave it untouched.
    let rolledForward = false;
    let applies = 0;
    const srv = await completionServer({ worker_id: "w1" }, (_endpoint, body) => {
      if (!rolledForward && "claim_generation" in body) return STRICT_DECODE_400;
      applies += 1;
      return { status: 200, body: { unmet: [], attempt_count: applies } };
    });
    try {
      const client = await capabilityClient(srv.url);

      // First attempt, old api: stamped → rejected → stripped → retried → recorded.
      const r1 = await client.recordCompletionAttempt("run-1", ATTEMPT_ARGS(4));
      assert.strictEqual(srv.posts.length, 2, "one stamped attempt + one stripped retry");
      assert.strictEqual(has(srv.posts[0]!), true, "first attempt carried the field");
      assert.strictEqual(has(srv.posts[1]!), false, "the retry stripped the field");
      assert.strictEqual(
        srv.posts.filter((p) => p.status === 200).length,
        1,
        "exactly one request was accepted (the rejected field-carrying request had ZERO side effects)",
      );
      assert.strictEqual(applies, 1, "the recompute+attempt applied EXACTLY ONCE");
      assert.strictEqual(r1.attemptCount, 1, "the returned server-authoritative count reflects one apply");

      // The api rolls forward and now accepts the field. A capability worker did NOT clear its
      // features, so it stays optimistic and STAMPS again — a single accepted post, no re-strip.
      rolledForward = true;
      const r2 = await client.recordCompletionAttempt("run-1", ATTEMPT_ARGS(4));
      assert.strictEqual(srv.posts.length, 3, "the rolled-forward attempt is one stamped post, no strip");
      assert.strictEqual(has(srv.posts[2]!), true, "the capability worker stamps again after rollforward");
      assert.strictEqual(srv.posts[2]!.body.claim_generation, 4);
      assert.strictEqual(applies, 2, "exactly one more apply");
      assert.strictEqual(r2.attemptCount, 2);
    } finally {
      await srv.close();
    }
  });

  it("non-capability worker: strips+retries once AND sticky-clears its features, so a later attempt OMITS the field", async () => {
    let applies = 0;
    const srv = await completionServer(
      { worker_id: "w1", protocol_features: ["claim_generation_fence"] },
      (_endpoint, body) => {
        if ("claim_generation" in body) return STRICT_DECODE_400;
        applies += 1;
        return { status: 200, body: { unmet: [], attempt_count: applies } };
      },
    );
    try {
      const client = await featureClient(srv.url); // feature negotiated, NO capability

      await client.recordCompletionAttempt("run-1", ATTEMPT_ARGS(7));
      assert.strictEqual(srv.posts.length, 2);
      assert.strictEqual(has(srv.posts[0]!), true, "first attempt carried the field via the feature");
      assert.strictEqual(has(srv.posts[1]!), false, "the retry stripped the field");
      assert.strictEqual(applies, 1, "recorded exactly once");

      // Second attempt: the feature was sticky-cleared and there is no capability, so the field
      // is omitted entirely — one post, no field, no retry.
      await client.recordCompletionAttempt("run-1", ATTEMPT_ARGS(7));
      assert.strictEqual(srv.posts.length, 3, "a single post, no fallback retry needed");
      assert.strictEqual(has(srv.posts[2]!), false, "sticky-clear: the later attempt omits the field");
      assert.strictEqual(applies, 2);
    } finally {
      await srv.close();
    }
  });

  it("genuine-business-400 control: a 'not interlocked' 400 propagates unchanged — no strip, no retry", async () => {
    const srv = await completionServer({ worker_id: "w1" }, () => ({
      status: 400,
      body: { error: "run is not interlocked" },
    }));
    try {
      const client = await capabilityClient(srv.url);
      await assert.rejects(
        client.recordCompletionAttempt("run-1", ATTEMPT_ARGS(9)),
        (e: unknown) => e instanceof RequestError && e.status === 400,
      );
      assert.strictEqual(srv.posts.length, 1, "a genuine business 400 is not retried");
      assert.strictEqual(has(srv.posts[0]!), true, "the field is not stripped off a genuine-error attempt");
    } finally {
      await srv.close();
    }
  });
});

// ── requestCompletionPermit ──────────────────────────────────────────────────────────
describe("requestCompletionPermit send-gate (PRD #1247 fix round)", () => {
  it("a credential_switch_v1 worker (no cached feature) STAMPS a gen>0 permit request", async () => {
    const srv = await completionServer({ worker_id: "w1" }, () => ({ status: 200, body: { granted: true } }));
    try {
      const client = await capabilityClient(srv.url);
      const r = await client.requestCompletionPermit("run-1", PERMIT_ARGS(3));
      assert.strictEqual(srv.posts.length, 1);
      assert.strictEqual(has(srv.posts[0]!), true, "the capability worker stamped the field");
      assert.strictEqual(srv.posts[0]!.body.claim_generation, 3);
      assert.strictEqual(r.granted, true);
    } finally {
      await srv.close();
    }
  });

  it("a non-capability worker (no feature) OMITS the field even for gen>0", async () => {
    const srv = await completionServer({ worker_id: "w1" }, () => ({ status: 200, body: { granted: true } }));
    try {
      const client = await bareClient(srv.url);
      await client.requestCompletionPermit("run-1", PERMIT_ARGS(5));
      assert.strictEqual(srv.posts.length, 1);
      assert.strictEqual(has(srv.posts[0]!), false, "no capability, no feature ⇒ field omitted");
    } finally {
      await srv.close();
    }
  });

  it("old→new→old→new skew: strips+retries ONCE with ZERO side effects, then re-stamps after rollforward (capability stays optimistic)", async () => {
    // Permit ISSUANCE is idempotent server-side; the stub issues (increments `issued`) only on a
    // 200, so the rejected field-carrying request issues nothing.
    let rolledForward = false;
    let issued = 0;
    const srv = await completionServer({ worker_id: "w1" }, (_endpoint, body) => {
      if (!rolledForward && "claim_generation" in body) return STRICT_DECODE_400;
      issued += 1;
      return { status: 200, body: { granted: true } };
    });
    try {
      const client = await capabilityClient(srv.url);

      const r1 = await client.requestCompletionPermit("run-1", PERMIT_ARGS(4));
      assert.strictEqual(srv.posts.length, 2, "one stamped request + one stripped retry");
      assert.strictEqual(has(srv.posts[0]!), true, "first request carried the field");
      assert.strictEqual(has(srv.posts[1]!), false, "the retry stripped the field");
      assert.strictEqual(
        srv.posts.filter((p) => p.status === 200).length,
        1,
        "exactly one request was accepted (the rejected field-carrying request issued nothing)",
      );
      assert.strictEqual(issued, 1, "the permit was issued EXACTLY ONCE");
      assert.strictEqual(r1.granted, true);

      rolledForward = true;
      const r2 = await client.requestCompletionPermit("run-1", PERMIT_ARGS(4));
      assert.strictEqual(srv.posts.length, 3, "the rolled-forward request is one stamped post, no strip");
      assert.strictEqual(has(srv.posts[2]!), true, "the capability worker stamps again after rollforward");
      assert.strictEqual(issued, 2, "exactly one more issue");
      assert.strictEqual(r2.granted, true);
    } finally {
      await srv.close();
    }
  });

  it("non-capability worker: strips+retries once AND sticky-clears its features, so a later request OMITS the field", async () => {
    let issued = 0;
    const srv = await completionServer(
      { worker_id: "w1", protocol_features: ["claim_generation_fence"] },
      (_endpoint, body) => {
        if ("claim_generation" in body) return STRICT_DECODE_400;
        issued += 1;
        return { status: 200, body: { granted: true } };
      },
    );
    try {
      const client = await featureClient(srv.url);

      await client.requestCompletionPermit("run-1", PERMIT_ARGS(7));
      assert.strictEqual(srv.posts.length, 2);
      assert.strictEqual(has(srv.posts[0]!), true, "first request carried the field via the feature");
      assert.strictEqual(has(srv.posts[1]!), false, "the retry stripped the field");
      assert.strictEqual(issued, 1, "issued exactly once");

      await client.requestCompletionPermit("run-1", PERMIT_ARGS(7));
      assert.strictEqual(srv.posts.length, 3, "a single post, no fallback retry needed");
      assert.strictEqual(has(srv.posts[2]!), false, "sticky-clear: the later request omits the field");
      assert.strictEqual(issued, 2);
    } finally {
      await srv.close();
    }
  });

  it("a granted:false denial is a 200 body, not an error: returned as-is, the field is never stripped", async () => {
    // A denial is a normal 200, so it never enters the fallback's error path — proving the strip
    // only ever fires on the EXACT strict-decode 400, never on a business rejection.
    const srv = await completionServer({ worker_id: "w1" }, () => ({
      status: 200,
      body: { granted: false, deny_reason: "stale_claim" },
    }));
    try {
      const client = await capabilityClient(srv.url);
      const r = await client.requestCompletionPermit("run-1", PERMIT_ARGS(4));
      assert.strictEqual(r.granted, false);
      assert.strictEqual(r.denyReason, "stale_claim");
      assert.strictEqual(srv.posts.length, 1, "a 200 denial is not retried");
      assert.strictEqual(has(srv.posts[0]!), true, "the field is not stripped off a denied request");
    } finally {
      await srv.close();
    }
  });

  it("genuine transport 400 control: a non-strict-decode 400 propagates unchanged — no strip, no retry", async () => {
    const srv = await completionServer({ worker_id: "w1" }, () => ({
      status: 400,
      body: { error: "malformed permit payload" },
    }));
    try {
      const client = await capabilityClient(srv.url);
      await assert.rejects(
        client.requestCompletionPermit("run-1", PERMIT_ARGS(9)),
        (e: unknown) => e instanceof RequestError && e.status === 400,
      );
      assert.strictEqual(srv.posts.length, 1, "a genuine 400 is not retried");
      assert.strictEqual(has(srv.posts[0]!), true, "the field is not stripped off a genuine-error request");
    } finally {
      await srv.close();
    }
  });
});

// ── requestCompletionHold ────────────────────────────────────────────────────────────
// Hold reads BOTH 200 and 409 off the body via readRunAck (a 409 is a refusal that RETAINS the
// run, NOT an error), so the closure returns the full fetchRaw+readRunAck handling. A stripped
// retry must therefore re-run the identical request AND preserve the 200/409 read; only a genuine
// non-200/409 throw (a rolled-back api's strict-decode 400) reaches the fallback.
describe("requestCompletionHold send-gate (PRD #1247 fix round)", () => {
  const HELD = (runId: string, status: string): { run: { id: string; status: string } } => ({
    run: { id: runId, status },
  });

  it("a credential_switch_v1 worker (no cached feature) STAMPS a gen>0 hold; the 200 read yields the parked status", async () => {
    const srv = await completionServer({ worker_id: "w1" }, () => ({ status: 200, body: HELD("run-1", "paused") }));
    try {
      const client = await capabilityClient(srv.url);
      const r = await client.requestCompletionHold("run-1", HOLD_ARGS(3));
      assert.strictEqual(srv.posts.length, 1);
      assert.strictEqual(has(srv.posts[0]!), true, "the capability worker stamped the field");
      assert.strictEqual(srv.posts[0]!.body.claim_generation, 3);
      assert.deepStrictEqual(r, { status: "paused" });
    } finally {
      await srv.close();
    }
  });

  it("a non-capability worker (no feature) OMITS the field even for gen>0", async () => {
    const srv = await completionServer({ worker_id: "w1" }, () => ({ status: 200, body: HELD("run-1", "paused") }));
    try {
      const client = await bareClient(srv.url);
      await client.requestCompletionHold("run-1", HOLD_ARGS(5));
      assert.strictEqual(srv.posts.length, 1);
      assert.strictEqual(has(srv.posts[0]!), false, "no capability, no feature ⇒ field omitted");
    } finally {
      await srv.close();
    }
  });

  it("old→new→old→new skew: strips+retries ONCE with ZERO side effects, then re-stamps after rollforward (capability stays optimistic)", async () => {
    // The park is the effect; `parked` counts only accepted (200) posts. A rolled-back api answers
    // the fence field with a strict-decode 400 (NOT a 409), which the fallback catches and retries.
    let rolledForward = false;
    let parked = 0;
    const srv = await completionServer({ worker_id: "w1" }, (_endpoint, body) => {
      if (!rolledForward && "claim_generation" in body) return STRICT_DECODE_400;
      parked += 1;
      return { status: 200, body: HELD("run-1", "paused") };
    });
    try {
      const client = await capabilityClient(srv.url);

      const r1 = await client.requestCompletionHold("run-1", HOLD_ARGS(4));
      assert.strictEqual(srv.posts.length, 2, "one stamped request + one stripped retry");
      assert.strictEqual(has(srv.posts[0]!), true, "first request carried the field");
      assert.strictEqual(has(srv.posts[1]!), false, "the retry stripped the field");
      assert.strictEqual(
        srv.posts.filter((p) => p.status === 200).length,
        1,
        "exactly one request was accepted (the rejected field-carrying request parked nothing)",
      );
      assert.strictEqual(parked, 1, "the run was parked EXACTLY ONCE");
      assert.deepStrictEqual(r1, { status: "paused" });

      rolledForward = true;
      const r2 = await client.requestCompletionHold("run-1", HOLD_ARGS(4));
      assert.strictEqual(srv.posts.length, 3, "the rolled-forward request is one stamped post, no strip");
      assert.strictEqual(has(srv.posts[2]!), true, "the capability worker stamps again after rollforward");
      assert.strictEqual(parked, 2, "exactly one more park");
      assert.deepStrictEqual(r2, { status: "paused" });
    } finally {
      await srv.close();
    }
  });

  it("non-capability worker: strips+retries once AND sticky-clears its features, so a later hold OMITS the field", async () => {
    let parked = 0;
    const srv = await completionServer(
      { worker_id: "w1", protocol_features: ["claim_generation_fence"] },
      (_endpoint, body) => {
        if ("claim_generation" in body) return STRICT_DECODE_400;
        parked += 1;
        return { status: 200, body: HELD("run-1", "paused") };
      },
    );
    try {
      const client = await featureClient(srv.url);

      await client.requestCompletionHold("run-1", HOLD_ARGS(7));
      assert.strictEqual(srv.posts.length, 2);
      assert.strictEqual(has(srv.posts[0]!), true, "first request carried the field via the feature");
      assert.strictEqual(has(srv.posts[1]!), false, "the retry stripped the field");
      assert.strictEqual(parked, 1, "parked exactly once");

      await client.requestCompletionHold("run-1", HOLD_ARGS(7));
      assert.strictEqual(srv.posts.length, 3, "a single post, no fallback retry needed");
      assert.strictEqual(has(srv.posts[2]!), false, "sticky-clear: the later hold omits the field");
      assert.strictEqual(parked, 2);
    } finally {
      await srv.close();
    }
  });

  it("a 409 refusal is NOT an error and NOT a strict-decode trigger: returns the run's real status, field kept", async () => {
    // The 409 refusal (RETAIN the run) is read off the body like a 200 — it must not throw and must
    // not trip the fallback (a stripped retry would be wrong).
    const srv = await completionServer({ worker_id: "w1" }, () => ({ status: 409, body: HELD("run-1", "running") }));
    try {
      const client = await capabilityClient(srv.url);
      const r = await client.requestCompletionHold("run-1", HOLD_ARGS(4));
      assert.deepStrictEqual(r, { status: "running" }, "the 409 body's status is returned, not thrown");
      assert.strictEqual(srv.posts.length, 1, "a 409 is not retried");
      assert.strictEqual(has(srv.posts[0]!), true, "the field is not stripped off a 409 refusal");
    } finally {
      await srv.close();
    }
  });

  it("genuine non-200/409 control: a non-strict-decode 400 propagates unchanged — no strip, no retry", async () => {
    const srv = await completionServer({ worker_id: "w1" }, () => ({
      status: 400,
      body: { error: "unprocessable hold" },
    }));
    try {
      const client = await capabilityClient(srv.url);
      await assert.rejects(
        client.requestCompletionHold("run-1", HOLD_ARGS(9)),
        (e: unknown) => e instanceof RequestError && e.status === 400,
      );
      assert.strictEqual(srv.posts.length, 1, "a genuine 400 is not retried");
      assert.strictEqual(has(srv.posts[0]!), true, "the field is not stripped off a genuine-error hold");
    } finally {
      await srv.close();
    }
  });
});
