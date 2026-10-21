import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { sleep } from "../src/util.js";
import { MessageBatcher, MAX_MESSAGE_BYTES, replaySegment } from "../src/batcher.js";
import { RequestError } from "../src/client.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// Read-only accessors the cross-check runner uses to prove its usage frames were delivered.

function api(rule: (msgs: OutgoingMessage[]) => number | undefined): { client: WorkerClient; landed: OutgoingMessage[] } {
  const landed: OutgoingMessage[] = [];
  const client = {
    async postMessages(_run: string, msgs: OutgoingMessage[]): Promise<void> {
      const status = rule(msgs);
      if (status === undefined) { landed.push(...msgs); return; }
      throw new RequestError("POST", "/messages", status, "{}");
    },
  } as unknown as WorkerClient;
  return { client, landed };
}

const text = (t: string) => ({ kind: "text" as const, agent: "lead", payload: { text: t } });

describe("MessageBatcher.undeliveredFromSeq", () => {
  it("is undefined after a fully delivered close", async () => {
    const a = api(() => undefined);
    const b = new MessageBatcher(a.client, "run", 0, 1, nullLogger());
    b.emit(text("one")); b.emit(text("two"));
    await b.close();
    assert.equal(b.undeliveredFromSeq(), undefined);
    assert.equal(a.landed.length, 2);
  });

  it("names the lowest seq a close could not deliver", async () => {
    const a = api(() => 503);
    const b = new MessageBatcher(a.client, "run", 4, 1, nullLogger());
    b.emit(text("one")); b.emit(text("two"));
    await b.close();
    assert.equal(b.undeliveredFromSeq(), 5);
    assert.equal(b.currentSeq(), 6);
  });

  it("counts a seq still buffered before any flush", () => {
    const a = api(() => undefined);
    const b = new MessageBatcher(a.client, "run", 0, 60_000, nullLogger());
    assert.equal(b.undeliveredFromSeq(), undefined);
    b.emit(text("one"));
    assert.equal(b.undeliveredFromSeq(), 1);
  });
});

describe("MessageBatcher.undeliveredFromSeq with a spilled pending range", () => {
  it("includes the first seq dropped into the pending range, not just the buffer", async () => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-accessors-"));
    try {
      const outbox = new Outbox({ root: path.join(dir, "outbox"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
      await outbox.init();
      const a = api(() => 503);
      const b = new MessageBatcher(a.client, "run", 0, 5, nullLogger(), undefined, undefined,
        { outbox, transientTripMs: 40, spillBufferBytes: 1 });
      b.emit(text("pre1")); b.emit(text("pre2"));
      for (let i = 0; i < 200 && !b.isSpilled(); i++) await sleep(20);
      assert.equal(b.isSpilled(), true, "the failing api drives the batcher into spill");
      for (let i = 0; i < 200 && (outbox.depthFor("run")?.pendingMessages ?? 0) < 2; i++) await sleep(20);
      await sleep(40);
      // Synchronously (no flush can interleave): seq 3 is kept, seqs 4..6 fold into the pending range.
      for (const t of ["x", "y", "z", "w"]) b.emit(text(t));
      assert.equal(b.undeliveredFromSeq(), 3, "the buffered seq is the lowest");
      const priv = b as unknown as { buffer: unknown[] };
      const held = priv.buffer;
      priv.buffer = [];
      assert.equal(b.undeliveredFromSeq(), 4, "with nothing buffered the pending range's first seq is reported");
      priv.buffer = held;
      await b.close();
    } finally { await fs.rm(dir, { recursive: true, force: true }); }
  });

  it("tombstones seqs dropped into the pending range, so they still read as lost after close", async () => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-accessors-"));
    try {
      const outbox = new Outbox({ root: path.join(dir, "outbox"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
      await outbox.init();
      const a = api(() => 503);
      const b = new MessageBatcher(a.client, "run", 0, 5, nullLogger(), undefined, undefined,
        { outbox, transientTripMs: 40, spillBufferBytes: 1 });
      b.emit(text("pre1")); b.emit(text("pre2"));
      for (let i = 0; i < 200 && !b.isSpilled(); i++) await sleep(20);
      assert.equal(b.isSpilled(), true, "the failing api drives the batcher into spill");
      for (let i = 0; i < 200 && (outbox.depthFor("run")?.pendingMessages ?? 0) < 2; i++) await sleep(20);
      await sleep(40);
      // seq 3 is kept, seqs 4..6 are dropped by the spill-buffer cap.
      for (const t of ["x", "y", "z", "w"]) b.emit(text(t));
      await b.close();
      // close journals the range as a loss record, so undeliveredFromSeq no longer names it:
      // only the tombstone set can tell the cross-check runner those frames never arrived.
      const t = b.tombstonedSeqs();
      for (const seq of [4, 5, 6]) assert.equal(t.seqs.has(seq), true, `dropped seq ${seq} is tombstoned`);
      assert.equal(t.seqs.has(3), false, "the kept seq is not tombstoned");
    } finally { await fs.rm(dir, { recursive: true, force: true }); }
  });
});

describe("MessageBatcher.journalOnlySeqs", () => {
  it("retains loss evidence after replay tombstones usage and the worker rearms", async () => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-replay-proof-"));
    try {
      const outbox = new Outbox({ root: path.join(dir, "outbox"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
      await outbox.init();
      let outage = true;
      const a = api((msgs) => outage ? 503 : msgs.some((m) => m.payload.event === "init") ? 400 : undefined);
      const b = new MessageBatcher(a.client, "run", 0, 5, nullLogger(), undefined, undefined,
        { outbox, transientTripMs: 40 });
      b.emit({ kind: "status", payload: { event: "init" } });
      for (let i = 0; i < 200 && !b.journalOnlySeqs().seqs.has(1); i++) await sleep(20);
      assert.equal(b.journalOnlySeqs().seqs.has(1), true);
      outage = false;
      const result = await outbox.drainRun("run", (msgs, gen) =>
        replaySegment(a.client, "run", msgs, gen, nullLogger()));
      assert.equal(result.retired, true);
      assert.equal(a.landed.some((m) => m.payload.event === "message_dropped"), true);
      assert.equal(a.landed.some((m) => m.payload.event === "init"), false);
      b.rearm();
      await b.close();
      assert.equal(b.journalOnlySeqs().seqs.has(1) || b.tombstonedSeqs().seqs.has(1), true,
        "a retired tombstone is not proof that the original usage frame arrived");
    } finally { await fs.rm(dir, { recursive: true, force: true }); }
  });

  it("names seqs only journaled to the outbox, and a re-arm does not clear them", async () => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-accessors-"));
    try {
      const outbox = new Outbox({ root: path.join(dir, "outbox"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
      await outbox.init();
      const a = api(() => 503);
      const b = new MessageBatcher(a.client, "run", 0, 5, nullLogger(), undefined, undefined,
        { outbox, transientTripMs: 40 });
      assert.deepEqual([...b.journalOnlySeqs().seqs], []);
      b.emit(text("one")); b.emit(text("two"));
      for (let i = 0; i < 200 && !b.isSpilled(); i++) await sleep(20);
      assert.equal(b.isSpilled(), true, "the failing api drives the batcher into spill");
      for (let i = 0; i < 200 && b.journalOnlySeqs().seqs.size < 2; i++) await sleep(20);
      assert.deepEqual([...b.journalOnlySeqs().seqs].sort(), [1, 2], "journaled, not yet delivered");
      assert.equal(b.undeliveredFromSeq(), undefined, "the journal hides them from undeliveredFromSeq");
      b.rearm();
      assert.deepEqual([...b.journalOnlySeqs().seqs].sort(), [1, 2], "a drain does not prove the original payloads arrived");
      await b.close();
    } finally { await fs.rm(dir, { recursive: true, force: true }); }
  });
});

describe("MessageBatcher.tombstonedSeqs", () => {
  it("is empty when nothing was replaced", async () => {
    const a = api(() => undefined);
    const b = new MessageBatcher(a.client, "run", 0, 1, nullLogger());
    b.emit(text("one"));
    await b.close();
    assert.deepEqual([...b.tombstonedSeqs().seqs], []);
    assert.equal(b.tombstonedSeqs().overflow, false);
  });

  it("records an emit-time oversize replacement", async () => {
    const a = api(() => undefined);
    const b = new MessageBatcher(a.client, "run", 0, 1, nullLogger());
    b.emit(text("ok"));
    b.emit(text("x".repeat(MAX_MESSAGE_BYTES + 10)));
    await b.close();
    assert.deepEqual([...b.tombstonedSeqs().seqs], [2]);
  });

  it("records a single message the api rejects as too large (413)", async () => {
    const a = api((msgs) => msgs.some((m) => (m.payload as { text?: string }).text === "big") ? 413 : undefined);
    const b = new MessageBatcher(a.client, "run", 0, 1, nullLogger());
    b.emit(text("big"));
    await b.close();
    assert.deepEqual([...b.tombstonedSeqs().seqs], [1]);
    assert.equal(a.landed.length, 1);
    assert.equal((a.landed[0]!.payload as { event?: string }).event, "message_truncated");
  });

  it("records a permanently rejected single message", async () => {
    const a = api((msgs) => msgs.some((m) => (m.payload as { text?: string }).text === "bad") ? 400 : undefined);
    const b = new MessageBatcher(a.client, "run", 0, 1, nullLogger());
    b.emit(text("bad"));
    await b.close();
    assert.deepEqual([...b.tombstonedSeqs().seqs], [1]);
    assert.equal(b.undeliveredFromSeq(), undefined);
  });

  it("records a bisected poison inside a batch", async () => {
    const a = api((msgs) => msgs.some((m) => (m.payload as { text?: string }).text === "bad") ? 400 : undefined);
    const b = new MessageBatcher(a.client, "run", 0, 60_000, nullLogger());
    for (const t of ["a", "b", "bad", "d"]) b.emit(text(t));
    await b.close();
    assert.deepEqual([...b.tombstonedSeqs().seqs], [3]);
    assert.equal(a.landed.length, 4);
  });

  it("is bounded and flags overflow", async () => {
    const a = api(() => undefined);
    const b = new MessageBatcher(a.client, "run", 0, 1, nullLogger());
    const big = "x".repeat(MAX_MESSAGE_BYTES + 10);
    for (let i = 0; i < 1030; i++) b.emit(text(big));
    await b.close();
    const t = b.tombstonedSeqs();
    assert.equal(t.seqs.size, 1024);
    assert.equal(t.overflow, true);
  });
});
