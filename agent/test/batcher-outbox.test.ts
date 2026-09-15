import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import {
  MessageBatcher,
  replaySegment,
  type PermanentFailureInfo,
} from "../src/batcher.js";
import { RequestError } from "../src/client.js";
import type { WorkerClient } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { sleep } from "../src/util.js";

// PRD #1391 M2 — spill, re-arm and ordered drain, wired to the REAL tmpdir Outbox
// (the M1 store) so a spill actually lands on disk and drains back over the messages
// route. Every test uses a fresh mkdtemp root and short real sleeps; the trip window
// is lowered through the batcher's `transientTripMs` opt so no test waits ten minutes.

const RUN = "run-spill-1";

const tmpRoots: string[] = [];
afterEach(async () => {
  for (const r of tmpRoots.splice(0)) await fs.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function mkOutbox(): Promise<Outbox> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-outbox-"));
  tmpRoots.push(dir);
  const o = new Outbox({
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await o.init();
  return o;
}

/** A client whose postMessages behaviour a test flips at will: "fail" throws the
 *  given transient/fatal status, "ok" records the landed batch and resolves. Extra
 *  batcher args (generation, signal) are ignored, matching the real call shape. */
function flippableClient(initial: "fail" | "ok" = "fail", status = 503): {
  client: WorkerClient;
  setMode: (m: "fail" | "ok") => void;
  setStatus: (s: number) => void;
  landed: OutgoingMessage[];
  posts: () => number;
} {
  let mode = initial;
  let code = status;
  const landed: OutgoingMessage[] = [];
  let posts = 0;
  const client = {
    async postMessages(_runId: string, msgs: OutgoingMessage[]): Promise<void> {
      posts += 1;
      if (mode === "fail") {
        throw new RequestError("POST", `/api/worker/runs/${RUN}/messages`, code, `{"error":"status ${code}"}`);
      }
      landed.push(...msgs);
    },
  } as unknown as WorkerClient;
  return {
    client,
    setMode: (m) => {
      mode = m;
    },
    setStatus: (s) => {
      code = s;
    },
    landed,
    posts: () => posts,
  };
}

function fill(b: MessageBatcher, n: number, tag = "m"): void {
  for (let i = 0; i < n; i++) b.emit({ kind: "text", agent: "lead", payload: { text: `${tag}${i}` } });
}

/** Assert `m` exists and return its payload as a bag (avoids optional-chaining into a
 *  cast, which oxlint's no-unsafe-optional-chaining flags). */
function pl(m: OutgoingMessage | undefined): Record<string, unknown> {
  assert.ok(m, "expected a message to be present");
  return m.payload as Record<string, unknown>;
}

/** Poll `pred` up to `ms` (20ms steps); throws with `label` if it never holds. */
async function until(pred: () => boolean, ms: number, label: string): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (pred()) return;
    await sleep(20);
  }
  assert.fail(`timed out waiting for: ${label}`);
}

async function collect(outbox: Outbox, runId: string): Promise<{ retired: boolean; seqs: number[]; msgs: OutgoingMessage[] }> {
  const msgs: OutgoingMessage[] = [];
  const res = await outbox.drainRun(runId, async (batch) => {
    msgs.push(...batch);
  });
  return { retired: res.retired, seqs: msgs.map((m) => m.seq), msgs };
}

describe("MessageBatcher spill/drain (PRD #1391 M2)", () => {
  it("a sustained transient outage ENTERS SPILL (never trips), writes segments, and an ordered drain delivers every message once", async () => {
    const outbox = await mkOutbox();
    const api = flippableClient("fail", 503);
    let perm: PermanentFailureInfo | undefined;
    const batcher = new MessageBatcher(api.client, RUN, 0, 5, nullLogger(), undefined, undefined, {
      outbox,
      generation: 3,
      transientTripMs: 40,
      onPermanentFailure: (info) => {
        perm = info;
      },
    });

    fill(batcher, 5);
    // The outage is transient (503), so after ~40ms of unbroken failure the batcher
    // switches its flush target to the outbox rather than tripping the breaker.
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => outbox.depthFor(RUN)?.pendingMessages === 5, 4000, "all 5 messages spilled to the outbox");

    assert.strictEqual(perm, undefined, "a transient outage of any length must NEVER produce a permanent-failure report");
    assert.strictEqual(batcher.isTripped(), false, "the breaker did not trip — it spilled");
    assert.strictEqual(batcher.isSpilled(), true);

    await batcher.close();

    // Recovery: the drainer replays the run's segments over the (now healthy) route.
    const drained = await collect(outbox, RUN);
    assert.strictEqual(drained.retired, true, "the run is fully retired after the drain");
    assert.deepStrictEqual(drained.seqs, [1, 2, 3, 4, 5], "every message once, ascending, contiguous");
    // The generation the messages were produced under is stamped on the segment.
    // (Replay rides it on the wire only under a fenced api; here it is inert.)
  });

  it("the spill-buffer cap DROPS the over-cap message into a range record; memory never grows unbounded", async () => {
    const outbox = await mkOutbox();
    const api = flippableClient("fail", 503);
    const batcher = new MessageBatcher(api.client, RUN, 0, 5, nullLogger(), undefined, undefined, {
      outbox,
      generation: 0,
      transientTripMs: 40,
      // A 1-byte cap: while spilled, every message after the first-into-an-empty-buffer
      // is over the cap and must be dropped into the pending range record.
      spillBufferBytes: 1,
    });

    // Drive a spill with two pre-spill messages (the cap does not apply before spill).
    fill(batcher, 2, "pre");
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => (outbox.depthFor(RUN)?.pendingMessages ?? 0) >= 2, 4000, "pre-spill messages land");
    await sleep(40); // let the spill flush settle so the buffer is empty + idle

    // Now, while spilled, emit a burst: the first fills the empty buffer and is kept;
    // the rest each exceed the 1-byte cap and are folded into the pending range.
    fill(batcher, 5, "burst");
    await batcher.close();

    // A range record file must exist — direct proof appendRangeRecord ran.
    const runDir = await runDirFor(RUN);
    const names = await fs.readdir(runDir);
    assert.ok(
      names.some((n) => n.startsWith("range-") && n.endsWith(".json")),
      `expected a range-*.json record in ${runDir}, got ${JSON.stringify(names)}`,
    );

    // The dropped seqs come back as per-seq tombstones so the stream stays contiguous.
    const drained = await collect(outbox, RUN);
    assert.strictEqual(drained.retired, true);
    // seqs 1..7 with no hole (2 pre-spill + 1 kept burst message + 4 dropped-as-tombstone).
    assert.deepStrictEqual(drained.seqs, [1, 2, 3, 4, 5, 6, 7], "contiguous seqs; the drops became tombstones");
    const tombstones = drained.msgs.filter((m) => (m.payload as { event?: string }).event === "message_dropped");
    assert.ok(tombstones.length >= 4, "the over-cap burst messages are tombstones, not real payloads");
  });

  it("a 404 still TRIPS (the permanent class is unchanged) and fires onPermanentFailure — spill is the transient class only", async () => {
    const outbox = await mkOutbox();
    const api = flippableClient("fail", 404);
    let perm: PermanentFailureInfo | undefined;
    const batcher = new MessageBatcher(api.client, RUN, 0, 5, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
      onPermanentFailure: (info) => {
        perm = info;
      },
    });

    fill(batcher, 3);
    await until(() => batcher.isTripped(), 4000, "a 404 trips the breaker at once");
    assert.ok(perm, "a permanent (404) failure must fire onPermanentFailure");
    assert.strictEqual(batcher.isSpilled(), false, "a 404 is the permanent class — it never spills");
    await batcher.close();
  });

  it("a message emitted AFTER the spill lands AFTER the spilled ones (ordering held on drain)", async () => {
    const outbox = await mkOutbox();
    const api = flippableClient("fail", 503);
    const batcher = new MessageBatcher(api.client, RUN, 0, 0, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
    });

    fill(batcher, 3, "first");
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => outbox.depthFor(RUN)?.pendingMessages === 3, 4000, "first three spilled");

    // Emit two more while spilled; they must also spill, at higher seqs.
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "after-A" } });
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "after-B" } });
    await until(() => outbox.depthFor(RUN)?.pendingMessages === 5, 4000, "after-spill messages spilled too");
    await batcher.close();

    const drained = await collect(outbox, RUN);
    assert.deepStrictEqual(drained.seqs, [1, 2, 3, 4, 5], "ascending, contiguous, after-spill messages last");
    assert.strictEqual(pl(drained.msgs[4]).text, "after-B", "seq 5 is the last after-spill emit");
  });

  it("rearm() returns the batcher to the NETWORK and materialises the in-memory pending range as per-seq tombstones", async () => {
    const outbox = await mkOutbox();
    const api = flippableClient("fail", 503);
    const batcher = new MessageBatcher(api.client, RUN, 0, 5, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
      spillBufferBytes: 1, // so the burst below drops into a pending range
    });

    fill(batcher, 2, "pre");
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => (outbox.depthFor(RUN)?.pendingMessages ?? 0) >= 2, 4000, "pre-spill messages land");
    await sleep(40); // settle: buffer empty, batcher idle

    // Synchronously (no await, so no flush timer can interleave): build an in-memory
    // pending range, then re-arm and flip the api healthy in the same tick.
    fill(batcher, 4, "burst"); // seq 3 kept, seqs 4,5,6 dropped into the pending range
    batcher.rearm();
    api.setMode("ok");

    assert.strictEqual(batcher.isSpilled(), false, "rearm returns the batcher to the network");

    // The dropped seqs must arrive over the network as tombstones (contiguity), plus
    // the kept burst message.
    await until(() => api.landed.some((m) => m.seq === 6), 4000, "the pending range's tombstones reach the network");
    const landedSeqs = new Set(api.landed.map((m) => m.seq));
    for (const s of [4, 5, 6]) assert.ok(landedSeqs.has(s), `dropped seq ${s} materialised as a network tombstone`);
    assert.strictEqual(
      pl(api.landed.find((m) => m.seq === 4)).event,
      "message_dropped",
      "seq 4 is a drop tombstone",
    );

    // And a message emitted AFTER rearm flushes over the network too.
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "post-rearm" } });
    await until(() => api.landed.some((m) => (m.payload as { text?: string }).text === "post-rearm"), 4000, "post-rearm emit lands over the network");
    await batcher.close();
  });

  it("close() while SPILLED spills the tail to the outbox (finalSpillOnClose), never dropping it", async () => {
    const outbox = await mkOutbox();
    const api = flippableClient("fail", 503);
    const batcher = new MessageBatcher(api.client, RUN, 0, 5, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
    });

    fill(batcher, 3, "body");
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => outbox.depthFor(RUN)?.pendingMessages === 3, 4000, "first three spilled");
    await sleep(40); // settle so the buffer is empty before we stage the tail

    // Stage a tail in the buffer, then close: a spilled close must spill the tail,
    // not drop it (finalSpillOnClose), so all six seqs survive to the drain.
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "tail-1" } });
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "tail-2" } });
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "tail-3" } });
    await batcher.close();

    const drained = await collect(outbox, RUN);
    assert.strictEqual(drained.retired, true);
    assert.deepStrictEqual(drained.seqs, [1, 2, 3, 4, 5, 6], "the tail was spilled on close, not dropped");
  });
});

describe("replaySegment poison handling (PRD #1391 M2)", () => {
  it("bisects a poisoned message inside a spilled segment, tombstones the one rejected seq, and lands the rest", async () => {
    const POISON = 3;
    const landed: OutgoingMessage[] = [];
    const client = {
      async postMessages(_runId: string, msgs: OutgoingMessage[]): Promise<void> {
        // The poison is only poison while it still carries its ORIGINAL payload; the
        // worker's replacement tombstone (event set) is accepted, exactly as the api
        // would accept it.
        const poisonous = msgs.some(
          (m) => m.seq === POISON && (m.payload as { event?: string }).event === undefined,
        );
        if (poisonous) {
          throw new RequestError("POST", "/api/worker/runs/r/messages", 400, `{"error":"invalid message payload"}`);
        }
        landed.push(...msgs);
      },
    } as unknown as WorkerClient;

    const segment: OutgoingMessage[] = [1, 2, 3, 4, 5].map((seq) => ({
      seq,
      kind: "text",
      payload: { text: `body ${seq}` },
    }));

    await replaySegment(client, "r", segment, 7, nullLogger());

    const seqs = landed.map((m) => m.seq).sort((a, b) => a - b);
    assert.deepStrictEqual(seqs, [1, 2, 3, 4, 5], "every seq lands — the poison as a tombstone, the rest as themselves");
    assert.strictEqual(
      pl(landed.find((m) => m.seq === POISON)).event,
      "message_dropped",
      "the poisoned seq is delivered as a tombstone so the stream stays contiguous",
    );
    assert.strictEqual(pl(landed.find((m) => m.seq === 1)).text, "body 1", "the non-poison messages are unchanged");
  });

  it("a transient error during replay THROWS so the drainer leaves the record pending", async () => {
    const client = {
      async postMessages(): Promise<void> {
        throw new RequestError("POST", "/api/worker/runs/r/messages", 503, `{"error":"down"}`);
      },
    } as unknown as WorkerClient;
    const { logger } = recordingLogger();
    await assert.rejects(
      replaySegment(client, "r", [{ seq: 1, kind: "text", payload: { text: "x" } }], 0, logger),
      RequestError,
      "a transient replay error must propagate, not be swallowed, so the record stays pending",
    );
  });
});

/** Locate a run's on-disk directory under the outbox root (private, so reached via a
 *  known-shape probe: the store writes seg-/range-/manifest files there). */
async function runDirFor(runId: string): Promise<string> {
  // The store's files live at <root>/<runId>. The root is not exposed on the Outbox,
  // so derive it from the fresh tmp roots this file created.
  for (const root of tmpRoots) {
    const candidate = path.join(root, "outbox", runId);
    try {
      await fs.access(candidate);
      return candidate;
    } catch {
      // try the next tmp root
    }
  }
  assert.fail(`could not locate the run dir for ${runId}`);
}
