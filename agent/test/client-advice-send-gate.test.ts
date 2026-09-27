import { describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { isStaleClaimRefusal } from "../src/terminal-resolve.js";
import type { ReviewRequest, TaskReviewRequest } from "../src/protocol.js";

// Issue #1423: the judge review and task-review advice posts stamp claim_generation through the
// SAME send-gate + strict-decode fallback as postMessages. A capability worker stamps a gen>0
// generation, 0/undefined never goes on the wire, a bare worker omits it, a rolled-back api's
// strict-decode 400 is retried once stripped, and a 409 stale_claim refusal surfaces as a
// RequestError that isStaleClaimRefusal recognises.

const TOKEN = "worker-join-token-0123456789";

interface Post {
  path: string;
  body: Record<string, unknown>;
  status: number;
}

async function adviceServer(
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
      if (req.method === "POST" && /\/(review|task-review)$/.test(url.pathname)) {
        const { status, body: respBody } = respond(body, posts.length);
        posts.push({ path: url.pathname, body, status });
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(respBody));
        return;
      }
      res.writeHead(404, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "not found" }));
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

const REVIEW: ReviewRequest = { verdict: "ok", summary: "s", model: "m", status: "complete", recommendations: [] };
const TASK_REVIEW: TaskReviewRequest = { status: "complete", summary: "s", findings: [] };
const OK = { status: 200, body: { status: "ok" } };

// Both advice posts, driven through one table so each assertion covers judge AND task review.
const LANES: { name: string; post: (c: WorkerClient, gen?: number, runId?: string) => Promise<void> }[] = [
  { name: "postReview", post: (c, gen, runId) => c.postReview("target-1", REVIEW, gen, runId) },
  { name: "postTaskReview", post: (c, gen, runId) => c.postTaskReview("target-1", TASK_REVIEW, gen, runId) },
];
const ADVICE_RUN = "11111111-2222-4333-8444-555555555555";

describe("advice post send-gate (issue #1423)", () => {
  for (const lane of LANES) {
    it(`${lane.name}: a credential_switch_v1 worker stamps a gen>0 claim_generation`, async () => {
      const srv = await adviceServer({ worker_id: "w1" }, () => OK);
      try {
        const client = clientFor(srv.url);
        await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
        await lane.post(client, 7);
        assert.equal(srv.posts.length, 1);
        assert.equal(srv.posts[0]!.body.claim_generation, 7);
      } finally {
        await srv.close();
      }
    });

    it(`${lane.name}: never sends 0 or undefined`, async () => {
      const srv = await adviceServer({ worker_id: "w1" }, () => OK);
      try {
        const client = clientFor(srv.url);
        await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
        await lane.post(client, 0);
        await lane.post(client, undefined);
        assert.equal(srv.posts.length, 2);
        for (const p of srv.posts) {
          assert.equal("claim_generation" in p.body, false, "0/undefined never goes on the wire");
        }
      } finally {
        await srv.close();
      }
    });

    it(`${lane.name}: a bare worker (no capability, no feature) omits claim_generation`, async () => {
      const srv = await adviceServer({ worker_id: "w1" }, () => OK);
      try {
        const client = clientFor(srv.url);
        await client.register("bare");
        await lane.post(client, 5);
        assert.equal("claim_generation" in srv.posts[0]!.body, false);
      } finally {
        await srv.close();
      }
    });

    it(`${lane.name}: a rolled-back api's strict-decode 400 is retried ONCE with the field stripped`, async () => {
      const srv = await adviceServer({ worker_id: "w1" }, (_b, attempt) =>
        attempt === 0 ? { status: 400, body: { error: "invalid request body" } } : OK,
      );
      try {
        const client = clientFor(srv.url);
        await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
        await lane.post(client, 4);
        assert.equal(srv.posts.length, 2, "one stamped attempt + one stripped retry");
        assert.equal(srv.posts[0]!.body.claim_generation, 4);
        assert.equal("claim_generation" in srv.posts[1]!.body, false, "the retry stripped the field");
      } finally {
        await srv.close();
      }
    });

    it(`${lane.name}: a 409 stale_claim refusal throws a RequestError isStaleClaimRefusal recognises`, async () => {
      const srv = await adviceServer({ worker_id: "w1" }, () => ({ status: 409, body: { disposition: "stale_claim" } }));
      try {
        const client = clientFor(srv.url);
        await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
        let caught: unknown;
        try {
          await lane.post(client, 3);
        } catch (err) {
          caught = err;
        }
        assert.ok(caught, "the refusal throws");
        assert.equal(isStaleClaimRefusal(caught), true);
        assert.equal(srv.posts.length, 1, "a stale_claim 409 is not retried stripped");
      } finally {
        await srv.close();
      }
    });
  }

  for (const lane of LANES) {
    it(`${lane.name}: advice_run_id rides with claim_generation`, async () => {
      const srv = await adviceServer({ worker_id: "w1" }, () => OK);
      try {
        const client = clientFor(srv.url);
        await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
        await lane.post(client, 7, ADVICE_RUN);
        assert.equal(srv.posts[0]!.body.claim_generation, 7);
        assert.equal(srv.posts[0]!.body.advice_run_id, ADVICE_RUN);
      } finally {
        await srv.close();
      }
    });

    it(`${lane.name}: advice_run_id is omitted whenever claim_generation is (gen 0, bare worker)`, async () => {
      const srv = await adviceServer({ worker_id: "w1" }, () => OK);
      try {
        const cap = clientFor(srv.url);
        await cap.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
        await lane.post(cap, 0, ADVICE_RUN);
        const bare = clientFor(srv.url);
        await bare.register("bare");
        await lane.post(bare, 5, ADVICE_RUN);
        assert.equal(srv.posts.length, 2);
        for (const p of srv.posts) {
          assert.equal("claim_generation" in p.body, false);
          assert.equal("advice_run_id" in p.body, false, "advice_run_id never rides without the generation");
        }
      } finally {
        await srv.close();
      }
    });

    it(`${lane.name}: the strict-decode retry strips advice_run_id together with claim_generation`, async () => {
      const srv = await adviceServer({ worker_id: "w1" }, (_b, attempt) =>
        attempt === 0 ? { status: 400, body: { error: "invalid request body" } } : OK,
      );
      try {
        const client = clientFor(srv.url);
        await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
        await lane.post(client, 4, ADVICE_RUN);
        assert.equal(srv.posts.length, 2, "one stamped attempt + one stripped retry");
        assert.equal(srv.posts[0]!.body.claim_generation, 4);
        assert.equal(srv.posts[0]!.body.advice_run_id, ADVICE_RUN);
        assert.equal("claim_generation" in srv.posts[1]!.body, false);
        assert.equal("advice_run_id" in srv.posts[1]!.body, false, "the retry stripped both fields");
      } finally {
        await srv.close();
      }
    });
  }

  it("does not mutate the caller's request object", async () => {
    const srv = await adviceServer({ worker_id: "w1" }, () => OK);
    try {
      const client = clientFor(srv.url);
      await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
      const review: ReviewRequest = { ...REVIEW };
      await client.postReview("target-1", review, 9, ADVICE_RUN);
      assert.equal("claim_generation" in review, false);
      assert.equal("advice_run_id" in review, false);
      assert.equal(srv.posts[0]!.body.claim_generation, 9);
    } finally {
      await srv.close();
    }
  });

  it("isStaleClaimRefusal is false for a non-stale 409 and for non-RequestErrors", async () => {
    const srv = await adviceServer({ worker_id: "w1" }, () => ({
      status: 409,
      body: { error: "this worker must stamp claim_generation on every advice post" },
    }));
    try {
      const client = clientFor(srv.url);
      await client.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
      let caught: unknown;
      try {
        await client.postReview("target-1", REVIEW, 2);
      } catch (err) {
        caught = err;
      }
      assert.ok(caught);
      assert.equal(isStaleClaimRefusal(caught), false);
      assert.equal(isStaleClaimRefusal(new Error("stale_claim")), false);
    } finally {
      await srv.close();
    }
  });
});
