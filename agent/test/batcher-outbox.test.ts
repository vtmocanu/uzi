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

/** An Outbox that FAILED CLOSED at init — its root is a symlink, which `init()`
 *  refuses (following it would escape /data), so `disabled` is set and every write is
 *  a silent no-op. Used to prove the batcher trips instead of "spilling" into it. */
async function mkDisabledOutbox(): Promise<Outbox> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-outbox-disabled-"));
  tmpRoots.push(dir);
  const real = path.join(dir, "real");
  await fs.mkdir(real, { recursive: true });
  const root = path.join(dir, "outbox");
  await fs.symlink(real, root); // a symlinked root → init disables the store
  const o = new Outbox({
    root,
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await o.init();
  return o;
}

/** A real tmpdir Outbox whose write seam a test flips at will (via `failOn`): while a
 *  record kind is armed, every raw write of that kind throws, exercising a write
 *  failure without corrupting the store. `fresh()` opens a SEPARATE Outbox over the
 *  SAME root, so a test can read the on-disk spill-unclean flag exactly as a restart
 *  would (its `uncleanRuns()` reflects the manifest at that instant). */
async function mkOutboxWithSeam(): Promise<{
  outbox: Outbox;
  failOn: (kind: "segment" | "range" | "manifest" | null) => void;
  fresh: () => Promise<Outbox>;
}> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-outbox-seam-"));
  tmpRoots.push(dir);
  const base = {
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  };
  let failKind: "segment" | "range" | "manifest" | null = null;
  const outbox = new Outbox({
    ...base,
    rawWrite: async (write, ctx) => {
      if (failKind !== null && ctx.kind === failKind) throw new Error(`injected ${ctx.kind} write failure`);
      await write();
    },
  });
  await outbox.init();
  return {
    outbox,
    failOn: (kind) => {
      failKind = kind;
    },
    fresh: async () => {
      const o = new Outbox({ ...base });
      await o.init();
      return o;
    },
  };
}

/** A real tmpdir Outbox whose write seam TIMESTAMPS every raw write of `failKind` and
 *  throws it (until `disarm()`), so a test can observe the retry CADENCE of a persistent
 *  spill-write failure. Every other write proceeds, so the batcher still enters spill
 *  (network 503 + manifest writes) before the armed kind starts failing. */
async function mkOutboxTimedSeam(
  failKind: "segment" | "range",
): Promise<{ outbox: Outbox; attempts: number[]; disarm: () => void }> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-outbox-timed-"));
  tmpRoots.push(dir);
  const attempts: number[] = [];
  let armed = true;
  const outbox = new Outbox({
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
    rawWrite: async (write, ctx) => {
      if (armed && ctx.kind === failKind) {
        attempts.push(Date.now());
        throw new Error(`injected ${ctx.kind} write failure`);
      }
      await write();
    },
  });
  await outbox.init();
  return {
    outbox,
    attempts,
    disarm: () => {
      armed = false;
    },
  };
}

/** A real tmpdir Outbox whose write seam a test arms to fail a chosen record kind AND
 *  records observability the FIX 2 tests need: the timestamps of every FAILED manifest
 *  write (the mark-unclean re-mark), and every segment/range write ATTEMPTED while a
 *  kind is armed (proof a fail-closed path did NOT proceed to a record write). `fresh()`
 *  opens a separate Outbox over the same root to read the on-disk unclean flag as a
 *  restart would. */
async function mkCountingSeam(): Promise<{
  outbox: Outbox;
  failOn: (kind: "segment" | "range" | "manifest" | null) => void;
  fresh: () => Promise<Outbox>;
  failedManifestWrites: number[];
  recordWritesWhileArmed: string[];
}> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-outbox-count-"));
  tmpRoots.push(dir);
  const base = {
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  };
  let failKind: "segment" | "range" | "manifest" | null = null;
  const failedManifestWrites: number[] = [];
  const recordWritesWhileArmed: string[] = [];
  const outbox = new Outbox({
    ...base,
    rawWrite: async (write, ctx) => {
      if (failKind !== null && ctx.kind === failKind) {
        if (ctx.kind === "manifest") failedManifestWrites.push(Date.now());
        throw new Error(`injected ${ctx.kind} write failure`);
      }
      if (failKind !== null && (ctx.kind === "segment" || ctx.kind === "range")) {
        recordWritesWhileArmed.push(ctx.kind);
      }
      await write();
    },
  });
  await outbox.init();
  return {
    outbox,
    failOn: (kind) => {
      failKind = kind;
    },
    fresh: async () => {
      const o = new Outbox({ ...base });
      await o.init();
      return o;
    },
    failedManifestWrites,
    recordWritesWhileArmed,
  };
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

/** `until` for an async predicate (e.g. re-opening a fresh Outbox each poll). */
async function untilAsync(pred: () => Promise<boolean>, ms: number, label: string): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await pred()) return;
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

  it("a DISABLED outbox TRIPS (never silently spills into the void) on a sustained transient outage", async () => {
    // A disabled store's writes are silent no-ops. If the batcher entered spill against
    // it, doSpillFlush's appendSegment would "succeed" writing NOTHING while takePrefix
    // has already removed the batch — the whole buffer silently dropped with no trip and
    // no onPermanentFailure, and the batcher stuck spilled forever. It must instead TRIP
    // (a disabled store is "no durable store"), exactly as with no outbox at all.
    const outbox = await mkDisabledOutbox();
    assert.strictEqual(outbox.isDisabled(), true, "the symlinked-root store failed closed at init");
    const api = flippableClient("fail", 503); // a genuine transient (5xx) that today would spill
    let perm: PermanentFailureInfo | undefined;
    const batcher = new MessageBatcher(api.client, RUN, 0, 5, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
      onPermanentFailure: (info) => {
        perm = info;
      },
    });

    fill(batcher, 4);
    await until(() => batcher.isTripped(), 4000, "the batcher trips against a disabled outbox");
    assert.ok(perm, "the failure is SURFACED via onPermanentFailure — not silently dropped");
    assert.strictEqual(batcher.isSpilled(), false, "a disabled outbox is never a spill target");
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

  it("finalSpillOnClose re-marks spilled_unclean BEFORE the close-time write, so a FAILED write leaves the loss ADMITTED (never silent)", async () => {
    // The silent-loss sequence the PRD forbids: (1) a clean periodic doSpillFlush empties
    // the buffer and CLEARS spilled_unclean; (2) more messages emit while still spilled;
    // (3) close()'s finalSpillOnClose write FAILS. finalSpillOnClose must NOT rely on the
    // flag "still being set from enterSpill" — a prior clean flush cleared it — so it must
    // re-mark unclean at its TOP (like doSpillFlush). Without that, restart's uncleanRuns()
    // omits the run and the tail loss is silent. FAILS on the unfixed code: the flag stays
    // clear across the failed close-time write, so the fresh Outbox does NOT list the run.
    const { outbox, failOn, fresh } = await mkOutboxWithSeam();
    const api = flippableClient("fail", 503);
    const batcher = new MessageBatcher(api.client, RUN, 0, 5, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
    });

    // (1) Spill a body cleanly: the periodic doSpillFlush drains it and CLEARS the flag.
    fill(batcher, 3, "body");
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => outbox.depthFor(RUN)?.pendingMessages === 3, 4000, "the body spilled to the outbox");
    // Poll a fresh Outbox over the same root until it observes the flag CLEARED (proof the
    // clean flush ran clearSpillUnclean — the precondition that makes the bug reachable).
    await untilAsync(
      async () => !(await fresh()).uncleanRuns().includes(RUN),
      4000,
      "the clean periodic doSpillFlush cleared spilled_unclean",
    );

    // (2) Emit a tail while still spilled, (3) arm the next SEGMENT write to fail, then
    // close SYNCHRONOUSLY (no await between emit and close, so close clears the flush timer
    // before it can fire and the tail spills only via finalSpillOnClose). The manifest write
    // that markSpillUnclean makes is NOT armed to fail, so the flag is set before the doomed
    // segment write; the segment write then throws and the catch swallows it.
    failOn("segment");
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "tail-1" } });
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "tail-2" } });
    await batcher.close();

    // (4) A fresh Outbox now lists the run: the possible tail loss is ADMITTED at restart.
    const afterClose = await fresh();
    assert.ok(
      afterClose.uncleanRuns().includes(RUN),
      "a failed close-time write must leave spilled_unclean SET so restart admits the loss",
    );
  });

  it("a PERIODIC (timer-driven, non-close) spill flush writes the pending dropped-range as a range record BEFORE any close", async () => {
    // Every spill-cap test above exits via close(), whose finalSpillOnClose has its OWN
    // range-record write — so a mutation that removed doSpillFlush's appendRangeRecord
    // call went uncaught. This keeps the batcher OPEN and lets a scheduled doSpillFlush
    // run, then asserts the range record is on disk BEFORE any close: it pins the
    // PERIODIC path's range write independently. (Fails if doSpillFlush's
    // appendRangeRecord call is removed — no range-*.json would appear while open.)
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

    // While spilled AND OPEN, emit a burst: seq 3 fills the empty buffer and is kept;
    // seqs 4..7 exceed the 1-byte cap and are folded into the pending range. The
    // scheduled (timer-driven) doSpillFlush must write that range as a range record.
    fill(batcher, 5, "burst");

    const runDir = await runDirFor(RUN);
    let rangeFile: string | undefined;
    const deadline = Date.now() + 4000;
    while (Date.now() < deadline) {
      const names = await fs.readdir(runDir).catch(() => [] as string[]);
      rangeFile = names.find((n) => n.startsWith("range-") && n.endsWith(".json"));
      if (rangeFile) break;
      await sleep(20);
    }
    assert.ok(
      rangeFile,
      "a periodic (non-close) doSpillFlush must write the pending dropped-range as a range-*.json record",
    );

    // Only now (assertion already made against the OPEN batcher) do we close, purely to
    // stop the timer for a clean teardown.
    await batcher.close();
  });

  it("persistent spill SEGMENT-write failures BACK OFF (advance the failure clock), not a flat retry cadence", async () => {
    // On the unfixed code the spill segment-write catch never advances
    // consecutiveFailures, so nextDelayMs() reschedules at the flat batchMs cadence on a
    // persistent disk/quota failure. The fix increments the failure counter so the
    // retries back off exponentially — the same five attempts then span far longer than
    // a flat cadence could.
    const { outbox, attempts, disarm } = await mkOutboxTimedSeam("segment");
    const api = flippableClient("fail", 503);
    const batchMs = 20;
    const batcher = new MessageBatcher(api.client, RUN, 0, batchMs, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
    });

    fill(batcher, 3);
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => attempts.length >= 5, 4000, "five persistent segment-write attempts observed");

    const span = attempts[4]! - attempts[0]!;
    const flatCadence = batchMs * 4; // ~4 gaps at the flat batchMs cadence
    assert.ok(
      span > 250,
      `persistent spill-write failures must back off (5 attempts spanned ${span}ms; a flat cadence is ~${flatCadence}ms)`,
    );

    disarm();
    await batcher.close();
  });

  it("persistent spill RANGE-record-write failures BACK OFF (advance the failure clock), not a flat retry cadence", async () => {
    // The range-record write catch has the SAME defect and the SAME fix. A 1-byte spill
    // buffer folds the burst below into a pending range whose write fails persistently;
    // the retries must back off rather than reschedule at the flat batchMs cadence.
    const { outbox, attempts, disarm } = await mkOutboxTimedSeam("range");
    const api = flippableClient("fail", 503);
    const batchMs = 20;
    const batcher = new MessageBatcher(api.client, RUN, 0, batchMs, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
      spillBufferBytes: 1, // so the burst folds into a pending range record
    });

    fill(batcher, 2, "pre");
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => (outbox.depthFor(RUN)?.pendingMessages ?? 0) >= 2, 4000, "pre-spill messages land as a segment");
    await sleep(40); // settle: buffer empty, idle

    // Burst while spilled: seq 3 fills the empty buffer (kept); seqs 4..7 exceed the
    // 1-byte cap and fold into the pending range, whose record write then fails.
    fill(batcher, 5, "burst");
    await until(() => attempts.length >= 5, 4000, "five persistent range-record-write attempts observed");

    const span = attempts[4]! - attempts[0]!;
    const flatCadence = batchMs * 4;
    assert.ok(
      span > 250,
      `persistent range-write failures must back off (5 attempts spanned ${span}ms; a flat cadence is ~${flatCadence}ms)`,
    );

    disarm();
    await batcher.close();
  });

  it("a PERIODIC spill flush whose mark-unclean re-mark FAILS fails closed: backs off, writes NO range/segment, preserves the data", async () => {
    // FIX 2a: doSpillFlush's re-mark of spilled_unclean must FAIL CLOSED. A marker-write
    // failure must NOT proceed to write range/segments — a crash after that would read as
    // clean at restart (silent tail loss). The fix advances the failure clock (backs off,
    // like the write-failure backoff tests) and returns, preserving the buffer + pending
    // range for the rescheduled retry. The re-mark only ATTEMPTS a manifest write once the
    // flag has been CLEARED by a prior clean flush, so we clear it first.
    const { outbox, failOn, fresh, failedManifestWrites, recordWritesWhileArmed } = await mkCountingSeam();
    const api = flippableClient("fail", 503);
    const batchMs = 20;
    const batcher = new MessageBatcher(api.client, RUN, 0, batchMs, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
    });

    // (1) Spill a body cleanly so the periodic doSpillFlush clears spilled_unclean.
    fill(batcher, 2, "pre");
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => outbox.depthFor(RUN)?.pendingMessages === 2, 4000, "the body spilled to the outbox");
    await untilAsync(
      async () => !(await fresh()).uncleanRuns().includes(RUN),
      4000,
      "the clean periodic doSpillFlush cleared spilled_unclean",
    );

    // (2) Arm the manifest write to fail (so markSpillUnclean throws), then emit a tail so
    // the next PERIODIC doSpillFlush has pending data and tries to re-mark unclean.
    failOn("manifest");
    fill(batcher, 3, "tail");

    // The re-mark keeps failing; the failure clock must advance so the retries BACK OFF.
    await until(() => failedManifestWrites.length >= 5, 4000, "five failed mark-unclean re-mark attempts observed");
    const span = failedManifestWrites[4]! - failedManifestWrites[0]!;
    assert.ok(
      span > 250,
      `persistent mark-unclean failures must back off (5 attempts spanned ${span}ms; a flat cadence is ~${batchMs * 4}ms)`,
    );

    // Fail closed: NO range/segment write was attempted while the marker kept failing, and
    // the tail is preserved in memory (nothing lost, nothing written durably yet).
    assert.deepStrictEqual(
      recordWritesWhileArmed,
      [],
      "a failed mark-unclean must NOT proceed to any range/segment write (fail closed)",
    );
    assert.strictEqual(batcher.bufferedCount(), 3, "the tail is preserved for the rescheduled retry");
    assert.strictEqual(
      outbox.depthFor(RUN)?.pendingMessages,
      2,
      "only the pre-spill body is durable; the tail was never written",
    );

    failOn(null);
    await batcher.close();
  });

  it("close() while SPILLED fails closed on a mark-unclean OR segment-write failure: no partial silent loss, accurate 'may be lost' count", async () => {
    // FIX 2b: if finalSpillOnClose's mark-unclean re-mark FAILS, it must abort BEFORE
    //   takePrefix drains the buffer — no partial write, and the "may be lost" warning
    //   reports the FULL dropped count.
    // FIX 2c: if a close-time appendSegment FAILS, the drained batch is re-buffered so the
    //   "may be lost" warning still reports the FULL dropped count (not an under-count).

    // Scenario A (2b) — the MARK fails.
    {
      const { outbox, failOn, fresh, recordWritesWhileArmed } = await mkCountingSeam();
      const api = flippableClient("fail", 503);
      const { logger, lines } = recordingLogger();
      const batcher = new MessageBatcher(api.client, "run-A", 0, 5, logger, undefined, undefined, {
        outbox,
        transientTripMs: 40,
      });

      // Spill a body cleanly so a later re-mark actually ATTEMPTS a manifest write.
      fill(batcher, 2, "body");
      await until(() => batcher.isSpilled(), 4000, "A: batcher enters spill");
      await until(() => outbox.depthFor("run-A")?.pendingMessages === 2, 4000, "A: body spilled");
      await untilAsync(
        async () => !(await fresh()).uncleanRuns().includes("run-A"),
        4000,
        "A: the clean flush cleared spilled_unclean",
      );

      // Arm the manifest write to fail, stage a tail, then close SYNCHRONOUSLY (close clears
      // the flush timer before it fires, so the tail spills only via finalSpillOnClose).
      failOn("manifest");
      batcher.emit({ kind: "text", agent: "lead", payload: { text: "tail-A1" } });
      batcher.emit({ kind: "text", agent: "lead", payload: { text: "tail-A2" } });
      await batcher.close();

      const warn = (lines as Array<{ level: string; msg: string; dropped?: number }>).find(
        (l) => l.msg === "message batcher: spilling the tail to the outbox on close failed; it may be lost",
      );
      assert.ok(warn, "A: a failed mark-unclean must ADMIT the loss via the 'may be lost' warning");
      assert.strictEqual(warn.dropped, 2, "A: the warning reports the FULL undrained count (aborted before takePrefix)");
      assert.deepStrictEqual(
        recordWritesWhileArmed,
        [],
        "A: the close aborts before any range/segment write (no partial silent loss)",
      );
    }

    // Scenario B (2c) — the MARK succeeds but the close-time SEGMENT write fails.
    {
      const { outbox, failOn, fresh } = await mkCountingSeam();
      const api = flippableClient("fail", 503);
      const { logger, lines } = recordingLogger();
      const batcher = new MessageBatcher(api.client, "run-B", 0, 5, logger, undefined, undefined, {
        outbox,
        transientTripMs: 40,
      });

      fill(batcher, 2, "body");
      await until(() => batcher.isSpilled(), 4000, "B: batcher enters spill");
      await until(() => outbox.depthFor("run-B")?.pendingMessages === 2, 4000, "B: body spilled");
      await untilAsync(
        async () => !(await fresh()).uncleanRuns().includes("run-B"),
        4000,
        "B: the clean flush cleared spilled_unclean",
      );

      // Arm the SEGMENT write to fail (the mark still succeeds), stage a tail, close.
      failOn("segment");
      batcher.emit({ kind: "text", agent: "lead", payload: { text: "tail-B1" } });
      batcher.emit({ kind: "text", agent: "lead", payload: { text: "tail-B2" } });
      await batcher.close();

      const warn = (lines as Array<{ level: string; msg: string; dropped?: number }>).find(
        (l) => l.msg === "message batcher: spilling the tail to the outbox on close failed; it may be lost",
      );
      assert.ok(warn, "B: a failed close-time write must ADMIT the loss via the 'may be lost' warning");
      assert.strictEqual(
        warn.dropped,
        2,
        "B: the failed batch is re-buffered so the warning reports the FULL dropped count, not an under-count",
      );
      // The mark succeeded, so restart admits the loss via the unclean flag too.
      assert.ok(
        (await fresh()).uncleanRuns().includes("run-B"),
        "B: spilled_unclean stays SET so restart admits the loss",
      );
    }
  });

  it("rearm() drains a WIDE in-memory dropped-range in BOUNDED chunks (memory-safe), landing every seq once, ascending", async () => {
    // FIX 3: rearm must NOT synchronously materialise the whole in-memory dropped-range
    // into the buffer (a long outage can make it arbitrarily wide → one huge allocation).
    // It flips to the network and schedules a flush; the network flush loop materialises
    // the range one bounded chunk at a time. Drive a spill with a 1-byte cap, fold a range
    // far wider than REARM_TOMBSTONE_CHUNK (500), then rearm with the client STILL failing
    // and assert the buffer never holds more than ~one chunk. Then flip the client OK and
    // assert every tombstone lands once, ascending (contiguity preserved).
    const CHUNK = 500; // mirrors the module-private REARM_TOMBSTONE_CHUNK
    const outbox = await mkOutbox();
    const api = flippableClient("fail", 503);
    const batcher = new MessageBatcher(api.client, RUN, 0, 5, nullLogger(), undefined, undefined, {
      outbox,
      transientTripMs: 40,
      spillBufferBytes: 1, // every burst message after the first folds into the pending range
    });

    fill(batcher, 2, "pre");
    await until(() => batcher.isSpilled(), 4000, "batcher enters spill");
    await until(() => (outbox.depthFor(RUN)?.pendingMessages ?? 0) >= 2, 4000, "pre-spill messages land");
    await sleep(40); // settle: buffer empty, idle

    // Synchronously (no await, so no flush timer interleaves): the first burst message fills
    // the empty buffer (kept); the next RANGE_WIDTH fold into the pending range. Keep the
    // client failing so nothing drains, then rearm.
    const RANGE_WIDTH = 1600; // > 3x CHUNK
    fill(batcher, RANGE_WIDTH + 1, "burst");
    batcher.rearm();

    // MEMORY SAFETY: rearm must not have materialised the whole range. On the pre-fix code
    // the buffer would already hold the kept message + the entire RANGE_WIDTH tombstones.
    assert.ok(
      batcher.bufferedCount() <= CHUNK + 1,
      `rearm must not materialise the whole range at once (buffered ${batcher.bufferedCount()}, range width ${RANGE_WIDTH})`,
    );
    assert.strictEqual(batcher.isSpilled(), false, "rearm returns the batcher to the network");

    // Now let it drain over a healthy network; every seq in the range must land once, ascending.
    api.setMode("ok");
    const keptSeq = 3; // seqs 1,2 = pre (spilled to the outbox); seq 3 = the kept burst
    const lastSeq = 2 + RANGE_WIDTH + 1; // last folded seq
    await until(() => api.landed.some((m) => m.seq === lastSeq), 8000, "the whole range drains to the network");
    await batcher.close();

    const landedSeqs = api.landed.map((m) => m.seq);
    for (let i = 1; i < landedSeqs.length; i++) {
      assert.ok(landedSeqs[i]! > landedSeqs[i - 1]!, `network seqs land strictly ascending (index ${i})`);
    }
    const set = new Set(landedSeqs);
    assert.strictEqual(landedSeqs.length, set.size, "no seq landed twice");
    assert.ok(set.has(keptSeq), "the kept burst message landed over the network");
    for (let s = keptSeq + 1; s <= lastSeq; s++) {
      assert.ok(set.has(s), `dropped seq ${s} materialised as a network tombstone`);
    }
    // A folded seq lands as a drop tombstone, not a real payload.
    const mid = api.landed.find((m) => m.seq === Math.floor((keptSeq + 1 + lastSeq) / 2));
    assert.strictEqual(pl(mid).event, "message_dropped", "a folded seq is a drop tombstone");
  });

  it("the batcher's claim generation flows through into the spilled segments (the mechanism carries whatever it is given)", async () => {
    // The chat lane wires the SAME MessageBatcher, and chat's REAL claim generation is
    // 0 (chat has no claim generation, D6/D10). But 0 is ALSO the batcher's default
    // (`opts.generation ?? 0`), so asserting `generation === 0` cannot discriminate "the
    // option flowed into the segment" from "nothing set it". Use a NON-ZERO generation
    // and assert the persisted segments replay under exactly that value: this pins that
    // the mechanism carries whatever generation the lane passes it (0 for chat elsewhere,
    // a live claim generation here). Fails if the batcher hardcodes/zeroes the generation.
    const GEN = 7;
    const outbox = await mkOutbox();
    const api = flippableClient("fail", 503);
    const batcher = new MessageBatcher(api.client, RUN, 0, 5, nullLogger(), undefined, undefined, {
      outbox,
      generation: GEN,
      transientTripMs: 40,
    });

    fill(batcher, 4, "chat");
    await until(() => batcher.isSpilled(), 4000, "the batcher enters spill");
    await until(() => outbox.depthFor(RUN)?.pendingMessages === 4, 4000, "all 4 messages spilled");
    assert.strictEqual(batcher.isTripped(), false, "a transient outage spills; it never trips");
    await batcher.close();

    // Drain, capturing the generation each segment replays under.
    const gens: number[] = [];
    const msgs: OutgoingMessage[] = [];
    const res = await outbox.drainRun(RUN, async (batch, gen) => {
      gens.push(gen);
      msgs.push(...batch);
    });
    assert.strictEqual(res.retired, true, "the run fully retires after the drain");
    assert.deepStrictEqual(
      msgs.map((m) => m.seq),
      [1, 2, 3, 4],
      "every message once, ascending, contiguous",
    );
    assert.ok(gens.length > 0, "at least one segment drained");
    assert.ok(
      gens.every((g) => g === GEN),
      `every spilled segment must replay under the generation it was produced (${GEN}), not the default 0`,
    );
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
