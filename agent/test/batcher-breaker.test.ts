import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  MessageBatcher,
  MAX_BISECT_POSTS,
  TRANSIENT_TRIP_MS,
  type PermanentFailureInfo,
} from "../src/batcher.js";
import { RequestError, isTransient } from "../src/client.js";
import { makeTextRedactor } from "../src/redact.js";
import { recordingLogger } from "./helpers.js";
import { sleep } from "../src/util.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";

/**
 * PRD #108 M3 steps 4-6: bisection, the tombstone, 4xx-as-fatal and the breaker.
 *
 * The rule the whole milestone turns on:
 *   413            -> SIZE path. Split and retry. Never a poison verdict.
 *   400/other 4xx  -> POISON path. Bisect, tombstone the one bad message.
 *   401/403/404    -> TRIPPED at once. Bisecting only burns budget proving it.
 *   5xx/transport  -> transient, backed off, tripped only after ~10 minutes.
 */

const RUN = "run-brk";

interface Post {
  seqs: number[];
  msgs: OutgoingMessage[];
}

/**
 * A client whose verdict for each post is decided by a caller-supplied rule, so
 * every test states the api's behaviour rather than the batcher's.
 */
function scriptedApi(rule: (msgs: OutgoingMessage[], postIndex: number) => number | undefined): {
  client: WorkerClient;
  posts: Post[];
  landed: OutgoingMessage[];
} {
  const posts: Post[] = [];
  const landed: OutgoingMessage[] = [];
  const client = {
    async postMessages(_runId: string, msgs: OutgoingMessage[]): Promise<void> {
      const index = posts.length;
      posts.push({ seqs: msgs.map((m) => m.seq), msgs });
      const status = rule(msgs, index);
      if (status === undefined) {
        landed.push(...msgs);
        return;
      }
      throw new RequestError("POST", `/api/worker/runs/${RUN}/messages`, status, `{"error":"status ${status}"}`);
    },
  } as unknown as WorkerClient;
  return { client, posts, landed };
}

function fill(b: MessageBatcher, n: number): void {
  for (let i = 0; i < n; i++) b.emit({ kind: "text", agent: "lead", payload: { text: `m${i}` } });
}

describe("MessageBatcher classification (PRD #108 M3)", () => {
  it("413 is permanent under isTransient — pinned, not inherited", () => {
    // The 413 arm depends on this, and it holds only because 413 is not >= 500 and
    // is neither 408 nor 429. If someone widens isTransient, this fails loudly here
    // rather than silently turning every oversize batch into an infinite retry.
    const e413 = new RequestError("POST", "/x", 413, "");
    assert.strictEqual(isTransient(e413), false, "413 must not be retried as a transient");
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 500, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 408, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 429, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 400, "")), false);
  });
});

describe("MessageBatcher 413 = the SIZE path (PRD #108 M3)", () => {
  it("splits and retries on 413, never bisects, and delivers everything", async () => {
    // 413 until the batch is 2 or fewer — an api whose real limit the worker's
    // local accounting under-estimated.
    const { client, posts, landed } = scriptedApi((msgs) => (msgs.length > 2 ? 413 : undefined));
    const { logger } = recordingLogger();
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger);

    fill(batcher, 8);
    await batcher.close();

    assert.strictEqual(landed.length, 8, "every message must be delivered; nothing is poison here");
    assert.deepStrictEqual(
      landed.map((m) => m.seq),
      [1, 2, 3, 4, 5, 6, 7, 8],
      "and in ascending seq order",
    );
    // Nothing was tombstoned: a 413 is a size signal, never a payload verdict.
    assert.ok(
      landed.every((m) => (m.payload as { event?: string }).event === undefined),
      "a 413 must never produce a tombstone above size 1",
    );
    assert.ok(posts.length > 1, "it took more than one post, i.e. it really did split");
    assert.strictEqual(batcher.isTripped(), false, "splitting is progress, not failure");
  });

  it("a 413 does NOT count toward the breaker, so a splitting run is never tripped", async () => {
    // Every post over size 1 is a 413. Without the streak reset this reads as
    // "the same batch failed again and again" and trips the breaker on a run whose
    // messages are all perfectly fine — Decision 4's failure mode through the 413 door.
    const { client, landed } = scriptedApi((msgs) => (msgs.length > 1 ? 413 : undefined));
    const { logger } = recordingLogger();
    let tripped: PermanentFailureInfo | undefined;
    const batcher = new MessageBatcher(client, RUN, 0, 2, logger, undefined, undefined, {
      onPermanentFailure: (info) => {
        tripped = info;
      },
    });

    fill(batcher, 12);
    await batcher.close();

    assert.strictEqual(tripped, undefined, "the breaker must not trip while the batcher is making progress");
    assert.strictEqual(batcher.isTripped(), false);
    assert.strictEqual(landed.length, 12, "all twelve land, one at a time");
  });

  it("a 413 on a SINGLE message tombstones it as message_truncated and says the emit cap failed", async () => {
    // Only the ORIGINAL seq-2 message is too large; the worker's small replacement
    // marker is accepted, exactly as a real api would accept it.
    const { client, landed } = scriptedApi((msgs) =>
      msgs.length === 1 && msgs[0]!.seq === 2 && (msgs[0]!.payload as { event?: string }).event === undefined
        ? 413
        : undefined,
    );
    const { logger, lines } = recordingLogger();
    // batchMs 0 so each emit flushes near-immediately and seq 2 gets posted alone.
    const batcher = new MessageBatcher(client, RUN, 0, 0, logger);
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "one" } });
    await sleep(10);
    batcher.emit({ kind: "tool_result", agent: "lead", payload: { text: "two" } });
    await sleep(10);
    await batcher.close();

    const two = landed.find((m) => m.seq === 2);
    assert.ok(two, "seq 2 must still be delivered — as a tombstone, so the stream stays contiguous");
    const payload = two.payload as Record<string, unknown>;
    assert.strictEqual(payload["event"], "message_truncated", "a 413 is a SIZE verdict, not message_dropped");
    assert.strictEqual(payload["kind"], "tool_result", "the original kind rides inside the payload");
    const loud = lines.find(
      (l): l is Record<string, unknown> =>
        !!l && typeof l === "object" && (l as { level?: string }).level === "error" &&
        String((l as { msg?: string }).msg).includes("SINGLE message as too large"),
    );
    assert.ok(loud, "reaching this means the emit-time cap failed, so it must be logged loudly");
  });
});

describe("MessageBatcher bisection + tombstone (PRD #108 M3)", () => {
  it("isolates the one poisoned message, tombstones it, and lands everything else", async () => {
    const POISON = 6;
    const { client, posts, landed } = scriptedApi((msgs) =>
      // The poison is only poison while it still carries its original payload; the
      // worker's replacement marker is accepted, exactly as the api would.
      msgs.some((m) => m.seq === POISON && (m.payload as { event?: string }).event === undefined) ? 400 : undefined,
    );
    const { logger } = recordingLogger();
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger);

    fill(batcher, 12);
    await batcher.close();

    // Contiguity is the whole point: 1..12 with no hole.
    assert.deepStrictEqual(
      landed.map((m) => m.seq).sort((a, b) => a - b),
      [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12],
      "every seq lands, including the poisoned one as a tombstone",
    );
    const marker = landed.find((m) => m.seq === POISON)!;
    assert.strictEqual(marker.kind, "status", "the tombstone rides as `status` so RunEvent renders it");
    const payload = marker.payload as Record<string, unknown>;
    assert.strictEqual(payload["event"], "message_dropped");
    assert.strictEqual(payload["kind"], "text", "the ORIGINAL kind is preserved as data");
    assert.strictEqual(typeof payload["text"], "string", "a `text` field is what describeStatus returns verbatim");
    assert.match(String(payload["text"]), /message dropped/);
    assert.strictEqual(batcher.isTripped(), false, "one poison must cost one message, not the run");
    // ceil(log2 12) = 4 search posts + the tombstone + the surrounding flushes.
    assert.ok(posts.length < 12, `bisection must be logarithmic, took ${posts.length} posts`);
  });

  it("costs ceil(log2 n) search posts — the claim behind the PRD's ~8 for 239", async () => {
    const N = 64; // ceil(log2 64) = 6
    const POISON = 40;
    let searchPosts = 0;
    const { client } = scriptedApi((msgs) => {
      const poisoned = msgs.some((m) => m.seq === POISON && (m.payload as { event?: string }).event === undefined);
      // Count only the sub-batch posts the search itself issues: everything after
      // the first full-batch rejection and before the single-message tombstone.
      if (msgs.length > 1) searchPosts += 1;
      return poisoned ? 400 : undefined;
    });
    const { logger } = recordingLogger();
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger);
    fill(batcher, N);
    await batcher.close();

    // One-sided: one post per level. The first rejected full batch is included in
    // the count above, hence <= ceil(log2 N) + 1. A two-sided search would post both
    // halves per level and roughly double this.
    assert.ok(
      searchPosts <= Math.ceil(Math.log2(N)) + 1,
      `one-sided bisection must cost <= ${Math.ceil(Math.log2(N)) + 1} multi-message posts, took ${searchPosts}`,
    );
    assert.ok(searchPosts < MAX_BISECT_POSTS, "and it must stay well inside the budget");
  });

  it("keeps the loss to ONE message even when the whole rest of the batch is fine", async () => {
    const POISON = 3;
    const { client, landed } = scriptedApi((msgs) =>
      msgs.some((m) => m.seq === POISON && (m.payload as { event?: string }).event === undefined) ? 400 : undefined,
    );
    const { logger } = recordingLogger();
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger);
    fill(batcher, 20);
    await batcher.close();

    const tombstones = landed.filter((m) => (m.payload as { event?: string }).event === "message_dropped");
    assert.strictEqual(tombstones.length, 1, "exactly one message is lost, not the 239 the incident lost");
    assert.strictEqual(tombstones[0]?.seq, POISON);
    assert.strictEqual(landed.length, 20);
  });

  it("a rejected TOMBSTONE is the only true drop, and it trips the breaker", async () => {
    // Everything is refused, marker included — something is wrong beyond the payload.
    const { client } = scriptedApi(() => 400);
    const { logger } = recordingLogger();
    let tripped: PermanentFailureInfo | undefined;
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger, undefined, undefined, {
      onPermanentFailure: (info) => {
        tripped = info;
      },
    });
    fill(batcher, 8);
    await batcher.close();

    assert.ok(tripped, "the breaker must trip when even the worker's own marker is refused");
    assert.match(tripped.reason, /replacement marker/);
    assert.strictEqual(batcher.isTripped(), true);
  });
});

describe("MessageBatcher tombstone attribution (PRD #108 B4)", () => {
  it("carries agent_instance and agent_label onto the tombstone, keeping it in its lane", async () => {
    // A tombstoned SUBAGENT frame must stay in its lane: RunEvent groups on
    // agent_instance, so a marker that dropped them would render in the top-level
    // stream — the loss shown, but not where it happened.
    const POISON = 2;
    const { client, landed } = scriptedApi((msgs) =>
      msgs.some((m) => m.seq === POISON && (m.payload as { event?: string }).event === undefined) ? 400 : undefined,
    );
    const { logger } = recordingLogger();
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger);
    batcher.emit({ kind: "text", agent: "coder", payload: { text: "clean" } });
    batcher.emit({
      kind: "tool_result",
      agent: "coder",
      agentInstance: "toolu_SUB",
      agentLabel: "web gate UX",
      payload: { text: "poison" },
    });
    batcher.emit({ kind: "text", agent: "coder", payload: { text: "after" } });
    await batcher.close();

    const marker = landed.find((m) => m.seq === POISON)!;
    assert.ok(marker, "the poisoned subagent frame must still land as a tombstone");
    assert.strictEqual((marker.payload as { event?: string }).event, "message_dropped");
    assert.strictEqual(marker.agent_instance, "toolu_SUB", "the tombstone stays in the subagent lane");
    assert.strictEqual(marker.agent_label, "web gate UX");
  });
});

describe("MessageBatcher oversize during bisection (PRD #108 B1)", () => {
  it("a 413 mid-bisection never tombstones a clean message; the outer arm re-splits", async () => {
    // The freak case the arm exists for: local accounting under-counted, so a
    // sub-batch of the bisection draws a 413 even though the full batch drew a 400.
    // Poison is seq 4; a 413 answers the FIRST left-half probe [1,2]. The unfixed
    // bisect narrowed on that size signal (`hi = mid`) and tombstoned seq 2 — a CLEAN
    // message — with "payload rejected by the api", a false statement in the run's
    // permanent history. The fix abandons the search and lets the size machinery split.
    let probed = false;
    const { client, landed } = scriptedApi((msgs) => {
      const seqs = msgs.map((m) => m.seq).join(",");
      if (seqs === "1,2" && !probed) {
        probed = true; // the under-count fires exactly once, on the first probe
        return 413;
      }
      return msgs.some((m) => m.seq === 4 && (m.payload as { event?: string }).event === undefined) ? 400 : undefined;
    });
    const { logger } = recordingLogger();
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger);
    fill(batcher, 4);
    await batcher.close();

    assert.deepStrictEqual(
      landed.map((m) => m.seq).sort((a, b) => a - b),
      [1, 2, 3, 4],
      "every seq still lands, contiguous",
    );
    const tombstones = landed.filter((m) => (m.payload as { event?: string }).event === "message_dropped");
    assert.strictEqual(tombstones.length, 1, "exactly one tombstone — the unfixed code produced two");
    assert.strictEqual(tombstones[0]?.seq, 4, "and it is the real poison (seq 4), never the clean seq 2");
    const two = landed.find((m) => m.seq === 2)!;
    assert.strictEqual(
      (two.payload as { event?: string }).event,
      undefined,
      "seq 2 lands CLEAN — the api never rejected it, so no tombstone may claim it did",
    );
    assert.strictEqual(batcher.isTripped(), false);
  });
});

describe("MessageBatcher breaker (PRD #108 M3)", () => {
  for (const status of [401, 403, 404]) {
    it(`${status} trips immediately, with no bisection`, async () => {
      const { client, posts } = scriptedApi(() => status);
      const { logger } = recordingLogger();
      let tripped: PermanentFailureInfo | undefined;
      const batcher = new MessageBatcher(client, RUN, 0, 5, logger, undefined, undefined, {
        onPermanentFailure: (info) => {
          tripped = info;
        },
      });
      fill(batcher, 16);
      await batcher.close();

      assert.ok(tripped, `${status} must trip the breaker`);
      // A dead token or a vanished run fails every message; searching for a poison
      // that does not exist would burn the whole budget proving it.
      assert.strictEqual(posts.length, 1, `${status} must not bisect, took ${posts.length} posts`);
      assert.ok(tripped.dropped > 0, "and it reports how many messages were lost");
    });
  }

  it("does NOT trip on a long run of transient failures short of the ~10-minute net", async () => {
    // This is the case a tight N=5 would have killed: an ordinary api restart.
    const { client, posts } = scriptedApi(() => 500);
    const { logger } = recordingLogger();
    let tripped: PermanentFailureInfo | undefined;
    const batcher = new MessageBatcher(client, RUN, 0, 1, logger, undefined, undefined, {
      onPermanentFailure: (info) => {
        tripped = info;
      },
    });
    fill(batcher, 4);
    await sleep(200);

    assert.ok(posts.length >= 3, `expected repeated transient retries, saw ${posts.length}`);
    assert.strictEqual(tripped, undefined, "a healthy run riding out an outage must not be failed by the breaker");
    assert.strictEqual(batcher.isTripped(), false);
    assert.ok(TRANSIENT_TRIP_MS >= 10 * 60_000, "the transient net must stay generous (>= 10 minutes)");
    await batcher.close();
  });

  it("the breaker's explanation goes OUT OF BAND and is scrubbed", async () => {
    const SECRET = "super-secret-token-abcdef123456";
    const { client } = scriptedApi(() => 403);
    const { logger } = recordingLogger();
    let tripped: PermanentFailureInfo | undefined;
    const batcher = new MessageBatcher(
      client,
      RUN,
      0,
      5,
      logger,
      undefined,
      // reportState does NO redaction of its own, so the batcher must scrub the
      // text before handing it over.
      makeTextRedactor([SECRET]),
      { onPermanentFailure: (info) => { tripped = info; } },
    );
    fill(batcher, 3);
    await batcher.close();

    assert.ok(tripped);
    assert.ok(!tripped.reason.includes(SECRET), "the reason must be scrubbed before it leaves the batcher");
    assert.match(tripped.reason, /message-transport failure, not an agent failure/);
  });

  it("a tripped breaker makes close() skip the drain instead of buying futile round-trips", async () => {
    const { client, posts } = scriptedApi(() => 401);
    const { logger, lines } = recordingLogger();
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger);
    fill(batcher, 5);
    await sleep(30);
    const postsBeforeClose = posts.length;
    await batcher.close();

    assert.strictEqual(posts.length, postsBeforeClose, "close() must not retry after a trip");
    const warn = lines.find(
      (l): l is Record<string, unknown> =>
        !!l && typeof l === "object" &&
        (l as { msg?: string }).msg === "message batcher closed with undelivered messages",
    );
    assert.ok(warn, "the drop is still reported");
    assert.ok(warn["trip_reason"], "and it carries why, plus the seq it died on");
    assert.strictEqual(warn["last_rejected_seq"], 1);
  });

  it("an emit AFTER close is reported, never silently swallowed", async () => {
    // The pre-existing trap all three runner paths hit: close() then a throwing
    // terminal report, whose catch emits an `error` frame into a closed batcher.
    const { client } = scriptedApi(() => undefined);
    const { logger, lines } = recordingLogger();
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger);
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "before" } });
    await batcher.close();
    batcher.emit({ kind: "error", agent: "worker", payload: { text: "the terminal report threw" } });

    const warn = lines.find(
      (l): l is Record<string, unknown> =>
        !!l && typeof l === "object" &&
        (l as { msg?: string }).msg === "run message emitted after the batcher closed; not delivered",
    );
    assert.ok(warn, "a dropped error frame must be loud — it is the class this PRD exists to remove");
    assert.strictEqual(warn["kind"], "error");
  });

  it("a permanent batch whose FIRST bisect probe hits a transient backs off, not a no-backoff storm (PRD #108)", async () => {
    // PRD #108 no-backoff-storm regression. A 2-message batch (seq 1 clean, seq 2
    // poison) draws a 400 as a whole, so the permanent arm bisects. bisect's first
    // left-half probe [1] draws a 500 (transient): it makes NO progress and hands the
    // whole batch back. The old permanent arm reset consecutiveFailures/failingSince
    // and returned false, so doFlush re-posted immediately with no backoff — an
    // unbounded loop that on the unfixed code runs straight to the 404 safety ceiling
    // (posts ≈ 12 and/or the breaker trips). The fix treats a no-progress bisect as
    // the transient failure it is: it backs off after the first abandon (posts == 2)
    // and never trips. Deterministic and hang-proof — we assert IMMEDIATELY after
    // flush() resolves, before the unref'd backoff timer can fire, and the 404 ceiling
    // guarantees termination even on buggy code.
    const { client, posts } = scriptedApi((msgs, postIndex) => {
      if (postIndex >= 12) return 404; // safety ceiling so even the unfixed hot loop stops fast
      const seqs = msgs.map((m) => m.seq);
      if (seqs.length === 1 && seqs[0] === 1) return 500; // the first left-half probe: transient, no progress
      if (seqs.includes(2)) return 400; // the poison makes the whole batch permanent
      return undefined;
    });
    const { logger } = recordingLogger();
    const batcher = new MessageBatcher(client, RUN, 0, 5, logger);
    fill(batcher, 2); // seq 1 (clean) + seq 2 (poison)
    await batcher.flush();

    // No awaits between flush() resolving and these assertions: the backed-off retry
    // timer is unref'd and cannot fire here.
    assert.ok(
      posts.length <= 3,
      `no-backoff storm: expected <= 3 posts (fix does full batch + one probe = 2), saw ${posts.length}`,
    );
    assert.strictEqual(batcher.isTripped(), false, "a single no-progress bisect must back off, never trip");
  });
});
