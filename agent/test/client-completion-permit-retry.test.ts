import { describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import { WorkerClient, RequestError } from "../src/client.js";

// e2e phase 52 (api-outage-outbox) regression: an interlocked run that finishes while the api is
// down asked for its completion permit ONCE; the transport error (`fetch failed`) reached the
// runner's generic catch and the run was journaled and replayed as `failed`. The permit request now
// waits the outage out: transient failures are retried with capped backoff inside a total budget,
// while a permanent error, a cancel, or an exhausted budget still throws.

const TOKEN = "worker-join-token-0123456789";
const ARGS = { contractRevision: 2, branch: "agent/issue-1", head: "head-sha" };

type Answer = { status: number; body: unknown } | "drop";

/** A stub api answering the permit endpoint from `answers` in order (the last one repeats).
 *  "drop" destroys the socket without a response, which undici surfaces as `fetch failed`. */
async function permitServer(
  answers: Answer[],
  onRequest?: () => void,
): Promise<{ url: string; posts: () => number; close: () => Promise<void> }> {
  let posts = 0;
  const server = http.createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      if ((req.url ?? "").endsWith("/worker/register")) {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ worker_id: "w1" }));
        return;
      }
      if (!(req.url ?? "").endsWith("/completion/permit")) {
        res.writeHead(404).end();
        return;
      }
      const answer = answers[Math.min(posts, answers.length - 1)]!;
      posts += 1;
      onRequest?.();
      if (answer === "drop") {
        req.socket.destroy();
        return;
      }
      res.writeHead(answer.status, { "Content-Type": "application/json" });
      res.end(JSON.stringify(answer.body));
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
  server.unref();
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    posts: () => posts,
    close: () => new Promise<void>((r) => server.close(() => r())),
  };
}

function client(url: string, sleeps: number[], budgetMs?: number): WorkerClient {
  return new WorkerClient(url, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async (ms) => {
      sleeps.push(ms);
    },
    ...(budgetMs === undefined ? {} : { permitRetryBudgetMs: budgetMs }),
  });
}

const GRANTED = { status: 200, body: { granted: true } };

describe("requestCompletionPermit waits out an api outage (e2e phase 52 regression)", () => {
  it("a transport failure (fetch failed) is retried until the api answers, then the permit is granted", async () => {
    const srv = await permitServer(["drop", "drop", "drop", GRANTED]);
    const sleeps: number[] = [];
    try {
      const r = await client(srv.url, sleeps).requestCompletionPermit("run-1", ARGS);
      assert.strictEqual(r.granted, true, "the run completes once the api is back");
      assert.strictEqual(srv.posts(), 4, "three dropped requests, then the granted one");
      assert.deepStrictEqual(sleeps, [1_000, 2_000, 4_000], "capped exponential backoff between attempts");
    } finally {
      await srv.close();
    }
  });

  it("5xx / 429 are transient too; the backoff caps at 30s", async () => {
    const answers: Answer[] = [
      ...Array.from({ length: 7 }, () => ({ status: 503, body: { error: "unavailable" } })),
      { status: 429, body: { error: "slow down" } },
      GRANTED,
    ];
    const srv = await permitServer(answers);
    const sleeps: number[] = [];
    try {
      const r = await client(srv.url, sleeps).requestCompletionPermit("run-1", ARGS);
      assert.strictEqual(r.granted, true);
      assert.deepStrictEqual(sleeps, [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000]);
    } finally {
      await srv.close();
    }
  });

  it("a structured denial (200, granted:false) is an answer, not a retry", async () => {
    const srv = await permitServer([{ status: 200, body: { granted: false, deny_reason: "missing_milestones" } }]);
    const sleeps: number[] = [];
    try {
      const r = await client(srv.url, sleeps).requestCompletionPermit("run-1", ARGS);
      assert.strictEqual(r.granted, false);
      assert.strictEqual(r.denyReason, "missing_milestones");
      assert.strictEqual(srv.posts(), 1);
      assert.deepStrictEqual(sleeps, []);
    } finally {
      await srv.close();
    }
  });

  it("a permanent HTTP error (409 stale claim) throws immediately, unretried", async () => {
    const srv = await permitServer([{ status: 409, body: { error: "stale claim" } }, GRANTED]);
    const sleeps: number[] = [];
    try {
      await assert.rejects(
        client(srv.url, sleeps).requestCompletionPermit("run-1", ARGS),
        (err: unknown) => err instanceof RequestError && err.status === 409,
      );
      assert.strictEqual(srv.posts(), 1);
      assert.deepStrictEqual(sleeps, []);
    } finally {
      await srv.close();
    }
  });

  it("an outage longer than the budget still throws the transport error (bounded, never a hang)", async () => {
    // A fake monotonic clock: only the sleeps advance it here.
    let t = 0;
    const srv = await permitServer(["drop"]);
    const sleeps: number[] = [];
    const c = new WorkerClient(srv.url, TOKEN, "0.1.0-test", nullLogger(), {
      sleep: async (ms) => {
        sleeps.push(ms);
        t += ms;
      },
      now: () => t,
      permitRetryBudgetMs: 7_500,
    });
    try {
      await assert.rejects(c.requestCompletionPermit("run-1", ARGS));
      assert.deepStrictEqual(sleeps, [1_000, 2_000, 4_000], "7s spent; the next 8s backoff would pass the 7.5s deadline");
      assert.strictEqual(srv.posts(), 4);
    } finally {
      await srv.close();
    }
  });

  it("request time counts against the budget, not only the backoff", async () => {
    // Each request 'takes' 20s of the fake clock (a black-holed connect timing out): with a 60s
    // budget only three requests fit, although the backoff alone (1s + 2s) is far below it.
    let t = 0;
    const srv = await permitServer([{ status: 503, body: { error: "down" } }], () => {
      t += 20_000;
    });
    const sleeps: number[] = [];
    const c = new WorkerClient(srv.url, TOKEN, "0.1.0-test", nullLogger(), {
      sleep: async (ms) => {
        sleeps.push(ms);
        t += ms;
      },
      now: () => t,
      permitRetryBudgetMs: 60_000,
    });
    try {
      await assert.rejects(c.requestCompletionPermit("run-1", ARGS), (err: unknown) => err instanceof RequestError && err.status === 503);
      assert.strictEqual(srv.posts(), 3, "20s + 1s + 20s + 2s + 20s = 63s: the fourth request never starts");
      assert.deepStrictEqual(sleeps, [1_000, 2_000]);
    } finally {
      await srv.close();
    }
  });

  it("the strict-decode fallback request is budgeted too: none is sent once the deadline has passed", async () => {
    // A credential_switch_v1 worker stamps claim_generation; a rolled-back api answers the exact
    // strict-decode 400, and withGenerationFallback retries once without the field. Here the first
    // request eats the whole budget, so the fallback must not start at all.
    let t = 0;
    const srv = await permitServer([{ status: 400, body: { error: "invalid request body" } }, GRANTED], () => {
      t += 10_000;
    });
    const c = new WorkerClient(srv.url, TOKEN, "0.1.0-test", nullLogger(), {
      sleep: async (ms) => {
        t += ms;
      },
      now: () => t,
      permitRetryBudgetMs: 10_000,
    });
    try {
      await c.register("cap", undefined, 1, undefined, ["credential_switch_v1"]);
      await assert.rejects(c.requestCompletionPermit("run-1", { ...ARGS, claimGeneration: 3 }));
      assert.strictEqual(srv.posts(), 1, "the stripped retry never started past the deadline");
    } finally {
      await srv.close();
    }
  });

  it("a fractional clock near the deadline still sends a request (integer timeout)", async () => {
    // performance.now() is fractional, and AbortSignal.timeout rejects a fractional delay: with a
    // fractional remaining budget the request must go out with a whole-ms timeout, not ERR_OUT_OF_RANGE.
    // Each clock read advances 0.25ms: the deadline is set at 0.25 (+3000 = 3000.25), and the request is
    // built at 0.5 or later, so the remaining time is fractional (2999.75 at most: seconds of real
    // headroom for the socket round trip, still below httpTimeoutMs).
    let t = 0;
    const srv = await permitServer([GRANTED]);
    const c = new WorkerClient(srv.url, TOKEN, "0.1.0-test", nullLogger(), {
      sleep: async () => {},
      now: () => (t += 0.25),
      permitRetryBudgetMs: 3_000,
    });
    try {
      const r = await c.requestCompletionPermit("run-1", ARGS);
      assert.strictEqual(r.granted, true);
      assert.strictEqual(srv.posts(), 1);
    } finally {
      await srv.close();
    }
  });

  it("an already-cancelled flight sends nothing", async () => {
    const srv = await permitServer([GRANTED]);
    const ac = new AbortController();
    ac.abort();
    try {
      await assert.rejects(client(srv.url, []).requestCompletionPermit("run-1", ARGS, ac.signal));
      assert.strictEqual(srv.posts(), 0, "no permit request after the cancel");
    } finally {
      await srv.close();
    }
  });

  it("a GRANTED response that races the cancel is discarded, never acted on", async () => {
    const ac = new AbortController();
    // The api grants, but the flight is cancelled while that reply is on its way back.
    const srv = await permitServer([GRANTED], () => ac.abort());
    try {
      await assert.rejects(client(srv.url, []).requestCompletionPermit("run-1", ARGS, ac.signal));
      assert.strictEqual(srv.posts(), 1);
    } finally {
      await srv.close();
    }
  });

  it("a cancelled flight stops the retry instead of waiting out the budget", async () => {
    const srv = await permitServer(["drop"]);
    const ac = new AbortController();
    const c = new WorkerClient(srv.url, TOKEN, "0.1.0-test", nullLogger(), {
      sleep: async () => {
        ac.abort();
      },
    });
    try {
      await assert.rejects(c.requestCompletionPermit("run-1", ARGS, ac.signal));
      assert.ok(srv.posts() <= 2, `stopped promptly after the cancel (posts=${srv.posts()})`);
    } finally {
      await srv.close();
    }
  });
});
