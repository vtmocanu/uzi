import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { existsSync, statSync } from "node:fs";
import os from "node:os";
import path from "node:path";

import { Outbox, StaleClaimError, OUTBOX_RANGE_RESERVE_BYTES, type RawWriteSeam } from "../src/outbox.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { nullLogger, recordingLogger } from "./helpers.js";

// PRD #1391 Run A / M1 — the authenticated message-outbox STORE. Every test uses a
// fresh mkdtemp root and (where retention is involved) an injected clock, so no
// test depends on wall-clock time or another test's tree. The tests ARE the
// module's only consumer in Run A (M2 wires production), which is what keeps
// knip/deadcode green over its exports.

const DEFAULTS = { runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 };

const tmpRoots: string[] = [];

afterEach(async () => {
  for (const r of tmpRoots.splice(0)) await fs.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function mkRoot(): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "outbox-"));
  tmpRoots.push(dir);
  return path.join(dir, "outbox");
}

type OutboxOverrides = Partial<{
  runMaxBytes: number;
  maxBytes: number;
  retentionMs: number;
  now: () => number;
  rawWrite: RawWriteSeam;
  log: ReturnType<typeof recordingLogger>["logger"];
}>;

function makeOutbox(root: string, over: OutboxOverrides = {}): Outbox {
  return new Outbox({
    root,
    log: over.log ?? nullLogger(),
    runMaxBytes: over.runMaxBytes ?? DEFAULTS.runMaxBytes,
    maxBytes: over.maxBytes ?? DEFAULTS.maxBytes,
    retentionMs: over.retentionMs ?? DEFAULTS.retentionMs,
    now: over.now,
    rawWrite: over.rawWrite,
  });
}

function textMsg(seq: number, text: string): OutgoingMessage {
  return { seq, kind: "text", payload: { text } };
}

function collector(): { send: (m: OutgoingMessage[]) => Promise<void>; flat: () => OutgoingMessage[]; calls: () => number } {
  const batches: OutgoingMessage[][] = [];
  return {
    send: async (msgs) => {
      batches.push(msgs);
    },
    flat: () => batches.flat(),
    calls: () => batches.length,
  };
}

async function segFile(runDir: string): Promise<string> {
  const names = await fs.readdir(runDir);
  const seg = names.find((n) => n.startsWith("seg-") && n.endsWith(".json"));
  if (!seg) throw new Error(`expected a segment file in ${runDir}`);
  return path.join(runDir, seg);
}

/** Sum the on-disk bytes of a run's record files (the seg- and range- files).
 *  recordBytes in the store is set to the serialized length that is written
 *  verbatim, so this on-disk sum equals the store's byte accounting for the run. */
async function recordFileBytes(runDir: string): Promise<number> {
  const names = await fs.readdir(runDir);
  let total = 0;
  for (const n of names) {
    if (n.startsWith("seg-") || n.startsWith("range-")) total += statSync(path.join(runDir, n)).size;
  }
  return total;
}

/** Flip the first hex char of the record's MAC — keeps the JSON valid so the test
 *  exercises the MAC-mismatch path specifically, not the JSON-parse path. */
async function tamperMac(file: string): Promise<void> {
  const obj = JSON.parse(await fs.readFile(file, "utf8")) as { mac: string };
  obj.mac = (obj.mac[0] === "0" ? "1" : "0") + obj.mac.slice(1);
  await fs.writeFile(file, JSON.stringify(obj));
}

async function readManifest(root: string, runId: string): Promise<{ generation: number; records: unknown[] }> {
  return JSON.parse(await fs.readFile(path.join(root, runId, "manifest.json"), "utf8")) as {
    generation: number;
    records: unknown[];
  };
}

describe("Outbox M1 (PRD #1391 Run A)", () => {
  it("1. a write interrupted before its rename never corrupts the prior good state", async () => {
    // The crash-atomicity guarantee (temp -> fsync -> rename -> dir fsync): a write
    // that dies before the rename leaves the destination untouched and adopts no
    // partial file. The `rawWrite` seam INTERRUPTS the SECOND append's manifest
    // write partway (writes a truncated body to the target path, then throws before
    // the rename), then a fresh Outbox loads the same root.
    //
    // Mutation guard: if writeFileAtomic were folded to write straight to the
    // destination (no temp -> fsync -> rename), the interrupted write would leave
    // `manifest.json` itself half-written and unparseable, the run would fail to
    // load, and the prior good seq-1 record would be lost — so this test would fail.
    const root = await mkRoot();
    let manifestWrites = 0;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "manifest") {
        manifestWrites++;
        if (manifestWrites === 2) {
          // Simulate the process dying mid-write: leave a truncated body at the
          // write's target path, then throw before writeFileAtomic can rename it.
          await fs.writeFile(ctx.path, '{"version":1,"runId":"r1","gen');
          throw new Error("simulated crash before rename");
        }
      }
      await write();
    };
    const a = makeOutbox(root, { rawWrite });
    await a.init();
    await a.appendSegment("r1", 1, [textMsg(1, "first")]); // fully lands (manifest write #1)
    // The second append's manifest write is interrupted; the call rejects.
    await assert.rejects(a.appendSegment("r1", 1, [textMsg(2, "second")]), /simulated crash/);

    // A fresh store over the same root must load the intact gen-1 manifest + seg-1,
    // never a partial file, and replay exactly the first message.
    const b = makeOutbox(root);
    await b.init();
    assert.equal(b.depthFor("r1")?.pendingMessages, 1, "only the committed record is pending");
    const c = collector();
    const res = await b.drainRun("r1", c.send);
    assert.equal(res.retired, true);
    assert.deepEqual(
      c.flat().map((m) => m.seq),
      [1],
      "the prior good segment is intact; the interrupted second write is not adopted",
    );
    assert.equal((c.flat()[0]?.payload as { text: string } | undefined)?.text, "first");
    assert.equal(c.flat()[0]?.kind, "text", "the record is the real message, not a partial-file tombstone");
  });

  it("2a. a valid manifest referencing a tampered segment yields per-seq gap tombstones", async () => {
    const root = await mkRoot();
    const a = makeOutbox(root);
    await a.init();
    await a.appendSegment("r1", 1, [textMsg(1, "a"), textMsg(2, "b"), textMsg(3, "c")]);
    await tamperMac(await segFile(path.join(root, "r1")));

    const b = makeOutbox(root);
    await b.init();
    const c = collector();
    await b.drainRun("r1", c.send);
    const sent = c.flat();
    assert.deepEqual(
      sent.map((m) => m.seq),
      [1, 2, 3],
    );
    for (const m of sent) {
      assert.equal(m.kind, "status");
      assert.equal((m.payload as { event: string }).event, "message_dropped");
    }
  });

  it("2b. a tampered manifest is rejected and yields NO gap (a bad manifest proves nothing)", async () => {
    const root = await mkRoot();
    const a = makeOutbox(root);
    await a.init();
    await a.appendSegment("r2", 1, [textMsg(1, "a"), textMsg(2, "b")]);
    await tamperMac(path.join(root, "r2", "manifest.json"));

    const { logger, lines } = recordingLogger();
    const b = makeOutbox(root, { log: logger });
    await b.init();
    const c = collector();
    const res = await b.drainRun("r2", c.send);
    assert.equal(c.calls(), 0, "send must never be called for a rejected manifest");
    assert.equal(res.staleRetired, 0);
    assert.ok(
      (lines as Array<{ level: string; msg: string }>).some((l) => l.level === "warn" && l.msg.includes("MAC mismatch")),
    );
  });

  it("3. a symlinked run dir is refused (skipped, not followed)", async () => {
    const root = await mkRoot();
    const a = makeOutbox(root);
    await a.init();
    await a.appendSegment("r1", 1, [textMsg(1, "real")]);

    // Replace the run dir with a symlink pointing at a renamed copy of the data.
    await fs.rename(path.join(root, "r1"), path.join(root, "r1-real"));
    await fs.symlink(path.join(root, "r1-real"), path.join(root, "r1"));

    const { logger, lines } = recordingLogger();
    const b = makeOutbox(root, { log: logger });
    await b.init();
    assert.ok(!b.runsWithPending().includes("r1"), "the symlink must be refused");
    assert.ok(b.runsWithPending().includes("r1-real"), "the real dir still loads");
    assert.equal(b.depthFor("r1"), undefined);
    const c = collector();
    await b.drainRun("r1", c.send);
    assert.equal(c.calls(), 0);
    assert.ok(
      (lines as Array<{ level: string; msg: string }>).some((l) => l.level === "warn" && l.msg.includes("symlink")),
    );
  });

  it("3b. a symlinked root fails closed (disabled: writes no-op, drain replays nothing)", async () => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "outbox-"));
    tmpRoots.push(dir);
    const real = path.join(dir, "outbox-real");
    await fs.mkdir(real, { recursive: true });
    const root = path.join(dir, "outbox");
    await fs.symlink(real, root); // the ROOT itself is a symlink (following it escapes /data)

    const { logger, lines } = recordingLogger();
    const o = makeOutbox(root, { log: logger });
    await o.init();
    await o.appendSegment("r1", 1, [textMsg(1, "x")]);
    assert.deepEqual(o.runsWithPending(), [], "a symlinked root disables every write");
    assert.ok(
      (lines as Array<{ level: string; msg: string }>).some((l) => l.level === "error" && l.msg.includes("root is a symlink")),
    );
  });

  it("3c. a symlinked .key fails closed (disabled)", async () => {
    const root = await mkRoot();
    await fs.mkdir(root, { recursive: true });
    // A `.key` symlink is refused before it is ever read (lstat, never followed).
    await fs.symlink(path.join(root, "key-target-does-not-exist"), path.join(root, ".key"));

    const { logger, lines } = recordingLogger();
    const o = makeOutbox(root, { log: logger });
    await o.init();
    await o.appendSegment("r1", 1, [textMsg(1, "x")]);
    assert.deepEqual(o.runsWithPending(), [], "a symlinked .key disables the store");
    assert.ok(
      (lines as Array<{ level: string; msg: string }>).some((l) => l.level === "error" && l.msg.includes(".key is a symlink")),
    );
  });

  it("4. quota replaces the oldest segment with a COMPACT range record → contiguous per-seq tombstones", async () => {
    const root = await mkRoot();
    // runMaxBytes = 1 forces eviction of the prior segment on every append.
    const o = makeOutbox(root, { runMaxBytes: 1 });
    await o.init();
    const big = "x".repeat(5000);
    await o.appendSegment("r1", 1, [textMsg(1, big)]);

    const runDir = path.join(root, "r1");
    // Capture the evicted segment's size + the run's accounted bytes before eviction.
    const segPath = await segFile(runDir);
    const segSize = statSync(segPath).size;
    assert.ok(segSize > 5000, "the segment embeds the 5000-byte message");
    const bytesBefore = await recordFileBytes(runDir);

    await o.appendSegment("r1", 1, [textMsg(2, "two")]);

    // Exactly one range record (evicted seg1) plus the surviving segment.
    const manifest = await readManifest(root, "r1");
    const kinds = (manifest.records as Array<{ kind: string; firstSeq: number; lastSeq: number }>).map((r) => r.kind);
    assert.deepEqual(kinds.sort(), ["range", "segment"]);
    assert.equal(o.depthFor("r1")?.pendingMessages, 2);

    // (a) The range record is COMPACT — it does not embed the evicted messages.
    // Mutation guard: if evictSegment wrote the original `messages` into the range
    // body, the range file would be ~segSize, not a few hundred bytes.
    const names = await fs.readdir(runDir);
    const rangeName = names.find((n) => n.startsWith("range-1-1-"));
    assert.ok(rangeName, "seg1 was evicted to a range record covering its exact range");
    const rangeSize = statSync(path.join(runDir, rangeName as string)).size;
    assert.ok(rangeSize < 1024, `the range record is well under 1 KiB (was ${rangeSize} bytes)`);
    assert.ok(rangeSize < segSize / 4, "the range record is far smaller than the evicted segment");
    assert.ok(!existsSync(segPath), "the evicted segment file was unlinked (D12: after the manifest install)");

    // (b) The run's accounted bytes dropped: the 5000-byte segment gave way to a
    // compact range + one small surviving segment.
    const bytesAfter = await recordFileBytes(runDir);
    assert.ok(bytesAfter < bytesBefore, `accounted bytes dropped after eviction (${bytesBefore} -> ${bytesAfter})`);

    const c = collector();
    await o.drainRun("r1", c.send);
    const sent = c.flat();
    assert.deepEqual(
      sent.map((m) => m.seq),
      [1, 2],
      "stream stays contiguous across the dropped range",
    );
    assert.equal(sent[0]?.kind, "status", "the dropped seq replays as a tombstone");
    assert.equal((sent[0]?.payload as { event: string } | undefined)?.event, "message_dropped");
    assert.equal(sent[1]?.kind, "text", "the surviving segment replays its real message");
  });

  it("4b. multiple consecutive evictions replay back-to-back range records as one contiguous stream", async () => {
    const root = await mkRoot();
    // runMaxBytes = 1 evicts the oldest surviving segment on every append, so after
    // four appends seqs 1-3 are all range records and only seq4 remains a segment.
    const o = makeOutbox(root, { runMaxBytes: 1 });
    await o.init();
    await o.appendSegment("r1", 1, [textMsg(1, "one")]);
    await o.appendSegment("r1", 1, [textMsg(2, "two")]);
    await o.appendSegment("r1", 1, [textMsg(3, "three")]);
    await o.appendSegment("r1", 1, [textMsg(4, "four")]);

    const manifest = await readManifest(root, "r1");
    const recs = (manifest.records as Array<{ kind: string; firstSeq: number }>).sort((a, b) => a.firstSeq - b.firstSeq);
    assert.deepEqual(
      recs.map((r) => r.kind),
      ["range", "range", "range", "segment"],
      "seqs 1-3 evicted to range records; seq4 survives",
    );

    const c = collector();
    const res = await o.drainRun("r1", c.send);
    assert.equal(res.retired, true);
    const sent = c.flat();
    assert.deepEqual(
      sent.map((m) => m.seq),
      [1, 2, 3, 4],
      "the back-to-back range records replay as one contiguous per-seq tombstone stream",
    );
    assert.deepEqual(
      sent.map((m) => m.kind),
      ["status", "status", "status", "text"],
      "seqs 1-3 are tombstones; seq4 is the surviving real message",
    );
  });

  it("5. retention removes a fully-retired run past retentionMs, never one with undrained records", async () => {
    const root = await mkRoot();
    let clock = 1_000_000;
    const o = makeOutbox(root, { retentionMs: 1000, now: () => clock });
    await o.init();
    await o.appendSegment("r1", 1, [textMsg(1, "x")]);
    await o.appendSegment("r2", 1, [textMsg(1, "y")]); // r2 stays undrained forever

    // r1 has records → not removed even far past the window.
    await o.sweepRetention(clock + 10_000);
    assert.ok(o.runsWithPending().includes("r1"));

    // Drain r1 to empty, then age it off.
    const c = collector();
    const res = await o.drainRun("r1", c.send);
    assert.equal(res.retired, true);
    await o.sweepRetention(clock + 1001);
    assert.equal(o.depthFor("r1"), undefined, "the fully-retired run aged off");

    // r2 never drained: retention must never remove it, however old.
    await o.sweepRetention(clock + 10_000_000);
    assert.ok(o.runsWithPending().includes("r2"), "an undrained run is never retention-removed");
  });

  it("6. the manifest generation and the .key survive a simulated restart", async () => {
    const root = await mkRoot();
    const a = makeOutbox(root);
    await a.init();
    await a.appendSegment("r1", 1, [textMsg(1, "a")]);
    await a.appendSegment("r1", 1, [textMsg(2, "b")]);
    const keyBefore = await fs.readFile(path.join(root, ".key"));
    const genBefore = (await readManifest(root, "r1")).generation;
    assert.ok(genBefore >= 2, "generation advanced across appends");

    const b = makeOutbox(root);
    await b.init();
    const keyAfter = await fs.readFile(path.join(root, ".key"));
    assert.ok(keyBefore.equals(keyAfter), ".key was not re-minted on restart");
    assert.equal((await readManifest(root, "r1")).generation, genBefore, "init did not rewrite the manifest");

    // A restarted store can still replay what the first session wrote.
    const c = collector();
    const res = await b.drainRun("r1", c.send);
    assert.equal(res.retired, true);
    assert.deepEqual(
      c.flat().map((m) => m.seq),
      [1, 2],
    );
  });

  it("7. the key is token-independent: a second store over the same root reads the records with no token", async () => {
    const root = await mkRoot();
    // No token is passed to either constructor — the key lives in .key, not in any
    // join token, so token rotation can never strand records.
    const a = makeOutbox(root);
    await a.init();
    await a.appendSegment("r1", 7, [textMsg(1, "kept")]);

    assert.equal((await fs.readFile(path.join(root, ".key"))).length, 32, ".key is a 32-byte worker-local secret");

    const b = makeOutbox(root);
    await b.init();
    const c = collector();
    await b.drainRun("r1", c.send);
    assert.equal((c.flat()[0]?.payload as { text: string } | undefined)?.text, "kept");
  });

  it("8. a missing .key with records present fails closed (disabled: replays nothing, append is a no-op)", async () => {
    const root = await mkRoot();
    const a = makeOutbox(root);
    await a.init();
    await a.appendSegment("r1", 1, [textMsg(1, "a")]);
    await fs.rm(path.join(root, ".key"));

    const { logger, lines } = recordingLogger();
    const b = makeOutbox(root, { log: logger });
    await b.init();
    // Disabled: drain replays nothing and append is a no-op.
    const c = collector();
    await b.drainRun("r1", c.send);
    assert.equal(c.calls(), 0);
    await b.appendSegment("r1", 1, [textMsg(2, "b")]);
    assert.deepEqual(b.runsWithPending(), [], "disabled store tracks and writes nothing");
    assert.ok(
      (lines as Array<{ level: string; msg: string }>).some(
        (l) => l.level === "error" && l.msg.includes("records present"),
      ),
    );
  });

  it("8b. a wrong-length .key fails closed (disabled: replays nothing, append is a no-op)", async () => {
    const root = await mkRoot();
    const a = makeOutbox(root);
    await a.init();
    await a.appendSegment("r1", 1, [textMsg(1, "a")]);
    // Replace the 32-byte key with a wrong-length one: fail closed rather than mint
    // over it and orphan every existing MAC.
    await fs.writeFile(path.join(root, ".key"), Buffer.alloc(16), { mode: 0o600 });

    const { logger, lines } = recordingLogger();
    const b = makeOutbox(root, { log: logger });
    await b.init();
    const c = collector();
    await b.drainRun("r1", c.send);
    assert.equal(c.calls(), 0);
    await b.appendSegment("r1", 1, [textMsg(2, "b")]);
    assert.deepEqual(b.runsWithPending(), [], "disabled store tracks and writes nothing");
    assert.ok(
      (lines as Array<{ level: string; msg: string }>).some((l) => l.level === "error" && l.msg.includes("unreadable")),
    );
  });

  it("9. the reserve admits an appendRangeRecord on a full filesystem, then is replenished", async () => {
    const root = await mkRoot();
    const reservePath = path.join(root, ".reserve");
    // Seam: the FIRST range write hits ENOSPC (with the reserve still present);
    // the store must release the reserve to free space, so the RETRY range write
    // sees the reserve gone and succeeds. Every other write proceeds.
    let firstRangeWrite = true;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "range") {
        if (firstRangeWrite) {
          firstRangeWrite = false;
          assert.ok(existsSync(reservePath), "reserve present when ENOSPC first hits");
          const err = new Error("no space left on device") as NodeJS.ErrnoException;
          err.code = "ENOSPC";
          throw err;
        }
        assert.ok(!existsSync(reservePath), "reserve released to admit the retry write");
      }
      await write();
    };
    const o = makeOutbox(root, { rawWrite });
    await o.init();
    assert.ok(existsSync(reservePath), "reserve preallocated at init");

    await o.appendRangeRecord("r1", 1, 5, 7);

    // The range record landed (3 seqs) despite the simulated full volume.
    assert.equal(o.depthFor("r1")?.pendingMessages, 3);
    const c = collector();
    await o.drainRun("r1", c.send);
    assert.deepEqual(
      c.flat().map((m) => m.seq),
      [5, 6, 7],
    );
    // The reserve was released to admit the write, then replenished.
    assert.ok(existsSync(reservePath), "reserve replenished after use");
    assert.equal(statSync(reservePath).size, OUTBOX_RANGE_RESERVE_BYTES);
  });

  it("10. spilledUnclean survives restart via uncleanRuns(), and clearSpillUnclean clears it", async () => {
    const root = await mkRoot();
    const a = makeOutbox(root);
    await a.init();
    await a.markSpillUnclean("r1");

    const b = makeOutbox(root);
    await b.init();
    assert.ok(b.uncleanRuns().includes("r1"), "a set spill-unclean flag is reported after restart");

    await b.clearSpillUnclean("r1");
    const d = makeOutbox(root);
    await d.init();
    assert.ok(!d.uncleanRuns().includes("r1"), "a cleared flag is not reported after restart");
  });

  it("11a. drainRun replays segments in ascending seq order and retires each on success", async () => {
    const root = await mkRoot();
    const o = makeOutbox(root);
    await o.init();
    await o.appendSegment("r1", 1, [textMsg(1, "a")]);
    await o.appendSegment("r1", 1, [textMsg(2, "b")]);
    await o.appendSegment("r1", 1, [textMsg(3, "c")]);

    const c = collector();
    const res = await o.drainRun("r1", c.send);
    assert.equal(res.retired, true);
    assert.deepEqual(
      c.flat().map((m) => m.seq),
      [1, 2, 3],
    );
    assert.deepEqual(o.runsWithPending(), []);
  });

  it("11b. a StaleClaimError retires the record locally and counts staleRetired", async () => {
    const root = await mkRoot();
    const o = makeOutbox(root);
    await o.init();
    await o.appendSegment("r1", 1, [textMsg(1, "a")]);
    await o.appendSegment("r1", 1, [textMsg(2, "b")]);
    await o.appendSegment("r1", 1, [textMsg(3, "c")]);

    let attempts = 0;
    const res = await o.drainRun("r1", async () => {
      attempts++;
      throw new StaleClaimError();
    });
    assert.equal(attempts, 3, "each pending record was attempted");
    assert.equal(res.retired, true, "all records retired locally");
    assert.equal(res.staleRetired, 3, "each stale-retired seq is counted");
    assert.deepEqual(o.runsWithPending(), [], "nothing left pending");
  });

  it("11c. a non-stale throw stops the drain and leaves the remaining records pending", async () => {
    const root = await mkRoot();
    const o = makeOutbox(root);
    await o.init();
    await o.appendSegment("r1", 1, [textMsg(1, "a")]);
    await o.appendSegment("r1", 1, [textMsg(2, "b")]);
    await o.appendSegment("r1", 1, [textMsg(3, "c")]);

    let calls = 0;
    const res = await o.drainRun("r1", async () => {
      calls++;
      if (calls === 2) throw new Error("transient network");
    });
    assert.equal(res.retired, false);
    assert.equal(calls, 2, "the drain stopped at the failing record");
    assert.ok(o.runsWithPending().includes("r1"));
    assert.equal(o.depthFor("r1")?.pendingMessages, 2, "seq 1 retired; seqs 2 and 3 remain pending");
  });

  it("12. the worker-total quota evicts the GLOBALLY-oldest segment (by since) across runs", async () => {
    const root = await mkRoot();
    const msg = "z".repeat(4000); // each segment ~4.2 KiB
    let clock = 0;
    // runMaxBytes huge so only the worker-total quota binds; maxBytes admits two
    // segments but not three, so the third append evicts exactly one — the oldest.
    const o = makeOutbox(root, { runMaxBytes: 1 << 30, maxBytes: 10_000, now: () => clock });
    await o.init();
    // r2 is created FIRST (iterated first, so it is the initial "best") but is the
    // NEWER run; r1 is created second at an EARLIER `since`. The globally-oldest
    // pick must flip to r1 via the `since <` comparison, not insertion order.
    clock = 2000;
    await o.appendSegment("r2", 1, [textMsg(1, msg)]); // since 2000 (newer)
    clock = 1000;
    await o.appendSegment("r1", 1, [textMsg(1, msg)]); // since 1000 (older)
    clock = 3000;
    await o.appendSegment("r2", 1, [textMsg(2, msg)]); // trips the worker quota

    const m1 = await readManifest(root, "r1");
    assert.equal((m1.records[0] as { kind: string }).kind, "range", "the globally-oldest run (r1) was evicted");
    const m2 = await readManifest(root, "r2");
    assert.ok(
      (m2.records as Array<{ kind: string }>).every((r) => r.kind === "segment"),
      "the newer run's segments survive",
    );
  });

  it("12b. the globally-oldest tie-break falls to the smaller firstSeq at equal since", async () => {
    const root = await mkRoot();
    const msg = "z".repeat(4000);
    const clock = 5000; // both runs created at the SAME since → the tie-break is firstSeq
    const o = makeOutbox(root, { runMaxBytes: 1 << 30, maxBytes: 10_000, now: () => clock });
    await o.init();
    // rA (firstSeq 5) is iterated first as the initial "best"; rB (firstSeq 1) must
    // win the tie via `firstSeq < best.rec.firstSeq`.
    await o.appendSegment("rA", 1, [textMsg(5, msg)]);
    await o.appendSegment("rB", 1, [textMsg(1, msg)]);
    await o.appendSegment("rA", 1, [textMsg(6, msg)]); // trips the worker quota

    const mB = await readManifest(root, "rB");
    assert.equal((mB.records[0] as { kind: string }).kind, "range", "the smaller-firstSeq segment (rB seq1) was evicted");
    const mA = await readManifest(root, "rA");
    assert.ok(
      (mA.records as Array<{ kind: string }>).every((r) => r.kind === "segment"),
      "rA's segments survive the tie-break",
    );
  });

  it("H1. a non-bare runId cannot escape the root (retireRun/appendSegment refuse it)", async () => {
    const root = await mkRoot();
    const { logger, lines } = recordingLogger();
    const o = makeOutbox(root, { log: logger });
    await o.init();

    // A sentinel OUTSIDE the root that a `../` escape would reach:
    // path.join(root, "../victim") resolves to a sibling of the outbox root.
    const outside = path.join(path.dirname(root), "victim");
    await fs.mkdir(outside, { recursive: true });
    await fs.writeFile(path.join(outside, "sentinel.txt"), "keep me");

    // retireRun's recursive delete must refuse the escaping component.
    await o.retireRun("../victim");
    assert.ok(existsSync(path.join(outside, "sentinel.txt")), "retireRun must not delete outside the root");

    // appendSegment must not write outside the root either.
    await o.appendSegment("../escape", 1, [textMsg(1, "nope")]);
    assert.ok(!existsSync(path.join(path.dirname(root), "escape")), "appendSegment must not write outside the root");
    assert.deepEqual(o.runsWithPending(), [], "no run is tracked for an escaping id");

    assert.ok(
      (lines as Array<{ level: string; msg: string }>).some((l) => l.level === "warn" && l.msg.includes("runId")),
      "an escape attempt is logged",
    );
  });

  it("H2. concurrent appendSegment on the SAME run both persist (no lost generation)", async () => {
    const root = await mkRoot();
    const o = makeOutbox(root);
    await o.init();

    // Without the per-run mutex both calls read the same manifest generation (empty
    // records), each installs `[...[], ownRef]`, and the later rename wins — losing
    // one record. The mutex serializes them, so both land.
    await Promise.all([
      o.appendSegment("r1", 1, [textMsg(1, "a")]),
      o.appendSegment("r1", 1, [textMsg(2, "b")]),
    ]);

    const manifest = await readManifest(root, "r1");
    assert.equal(manifest.records.length, 2, "both segments are listed (neither generation was lost)");
    assert.ok(manifest.generation >= 2, "each append advanced the generation");
    assert.equal(o.depthFor("r1")?.pendingMessages, 2);

    // A fresh store over the same root replays both, in seq order.
    const b = makeOutbox(root);
    await b.init();
    const c = collector();
    const res = await b.drainRun("r1", c.send);
    assert.equal(res.retired, true);
    assert.deepEqual(
      c.flat().map((m) => m.seq).sort((x, y) => x - y),
      [1, 2],
      "both records survived and drain in seq order",
    );
  });

  it("H3. a cross-run worker-quota eviction never mutates a BUSY victim's manifest (try-lock/defer)", async () => {
    // The cross-run hazard the M1 fix closes. The worker-total quota (maxBytes)
    // evicts the globally-oldest segment, which may belong to a DIFFERENT run B than
    // the appending run A. That eviction is a read-modify-write of B's manifest.json
    // generation. If B's OWN batcher is mid-append (holding B's per-run lock) the two
    // writers race on manifest.json and one generation is lost — B drops or reorders a
    // committed record. The fix takes a cross-run victim's lock NON-BLOCKING: a busy
    // victim is skipped and the eviction deferred (maxBytes is a SOFT target), so A
    // never touches B's manifest while B's lock is held.
    //
    // DISCRIMINATOR: while B's lock is held, A's append must create NO file in B's
    // directory. Under the unfixed code A's evictSegment(B) writes a range-*.json into
    // B's dir (and unlinks the evicted segment / rewrites B's manifest) regardless of
    // who wins the manifest race, so B's directory listing changes — and this test
    // fails. Under the fix the listing is untouched.
    const root = await mkRoot();
    const big = "x".repeat(5000); // each segment ~5.2 KiB
    const bDirName = "run-b";
    const bDir = path.join(root, bDirName);

    // Seam: once armed, pause ONLY run B's next manifest write (op_B, the slow
    // mutating op that will hold B's lock across A's append). B's initial append runs
    // BEFORE the gate is armed, and every other write — A's own writes, and on the
    // unfixed path A's eviction writes into B's dir — proceeds untouched.
    let armGate = false;
    let paused = false;
    let reachedOpBWrite!: () => void;
    const opBAtWrite = new Promise<void>((r) => {
      reachedOpBWrite = r;
    });
    let releaseOpB!: () => void;
    const opBGate = new Promise<void>((r) => {
      releaseOpB = r;
    });
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      const underB = ctx.path.includes(`${path.sep}${bDirName}${path.sep}`);
      if (armGate && !paused && ctx.kind === "manifest" && underB) {
        paused = true;
        reachedOpBWrite();
        await opBGate; // hold B's per-run lock until the test releases it
      }
      await write();
    };

    // runMaxBytes huge so only the worker-total quota binds; maxBytes admits ONE
    // segment but not two, so A's append trips the worker quota with B as the victim.
    const o = makeOutbox(root, { runMaxBytes: 1 << 30, maxBytes: 8000, rawWrite });
    await o.init();

    // B commits one big segment (fully) — the globally-oldest eviction target.
    await o.appendSegment(bDirName, 1, [textMsg(1, big)]);
    const bDirBefore = (await fs.readdir(bDir)).sort();
    const bGenBefore = (await readManifest(root, bDirName)).generation;

    // Start a slow mutating op on B that pauses at its manifest write, holding B's
    // per-run lock for the duration of A's append.
    armGate = true;
    const opB = o.markSpillUnclean(bDirName);
    await opBAtWrite;

    // A appends and trips the worker-total quota; B is the globally-oldest segment but
    // its lock is held, so the eviction must be deferred and B left untouched.
    await o.appendSegment("run-a", 1, [textMsg(1, big)]);

    // DISCRIMINATOR: A's cross-run eviction attempt wrote nothing into B's directory.
    const bDirDuring = (await fs.readdir(bDir)).sort();
    assert.deepEqual(
      bDirDuring,
      bDirBefore,
      "A must not mutate a busy victim's directory (cross-run eviction deferred)",
    );

    // Release B's slow op and let it finish cleanly.
    releaseOpB();
    await opB;

    // A's append succeeded despite the deferred eviction.
    assert.equal(o.depthFor("run-a")?.pendingMessages, 1, "A's append landed");

    // B lost no committed generation: exactly one mutation (op_B) advanced its
    // generation, its record set is intact, and its committed record replays as the
    // real message — never a dropped-range tombstone from a stolen eviction.
    const bManifestAfter = await readManifest(root, bDirName);
    assert.equal(bManifestAfter.generation, bGenBefore + 1, "B's generation advanced by exactly its own one mutation");
    assert.equal(bManifestAfter.records.length, 1, "B keeps its committed segment (no cross-run eviction)");
    assert.equal((bManifestAfter.records[0] as { kind: string }).kind, "segment", "B's record is still a real segment");

    const ca = collector();
    const resA = await o.drainRun("run-a", ca.send);
    assert.equal(resA.retired, true);
    assert.deepEqual(
      ca.flat().map((m) => m.seq),
      [1],
    );
    assert.equal(ca.flat()[0]?.kind, "text", "A's record replays as a real message");

    const cb = collector();
    const resB = await o.drainRun(bDirName, cb.send);
    assert.equal(resB.retired, true);
    assert.deepEqual(
      cb.flat().map((m) => m.seq),
      [1],
      "B's committed record replays contiguously",
    );
    assert.equal(cb.flat()[0]?.kind, "text", "B's record survived as the real message (no lost generation)");
    assert.equal((cb.flat()[0]?.payload as { text: string } | undefined)?.text, big);
  });

  it("H4. retention re-checks under the lock: a record appended during the sweep is NOT deleted", async () => {
    // The retention-sweep TOCTOU the M1 fix closes. sweepRetention decides a run is
    // removable (records empty, past retentionMs) OUTSIDE the per-run lock, collecting
    // a toRemove list, then deletes each UNDER the lock. Because the per-run mutex now
    // orders things deterministically, a legitimate appendSegment for that run that
    // wins the lock chain between the decision and the removal commits FIRST; the fix
    // re-checks emptiness + age under the lock and skips the run, so removeRun never
    // deletes the freshly-committed record.
    //
    // DISCRIMINATOR: while a paused appendSegment holds r1's lock (its manifest write
    // suspended, so r1's in-memory records are still empty), sweepRetention collects r1
    // and queues the removal BEHIND that lock. Releasing the paused write commits the
    // new record and frees the lock; the queued removal then runs. Under the UNFIXED
    // code removeRun deletes r1's whole subtree unconditionally, so the record is lost
    // (depthFor undefined, a fresh Outbox replays nothing) and this test fails. Under
    // the fix the re-check sees r1 tracked with one record and skips it, so it survives.
    const root = await mkRoot();
    const clock = 1_000_000;

    // Seam: once armed, pause r1's NEXT manifest write (the paused append that holds
    // r1's lock across the sweep). The seg-1 append and the drain-to-empty run BEFORE
    // the gate is armed, so their manifest writes proceed untouched.
    let armGate = false;
    let paused = false;
    let reachedAppendWrite!: () => void;
    const appendAtWrite = new Promise<void>((r) => {
      reachedAppendWrite = r;
    });
    let releaseAppend!: () => void;
    const appendGate = new Promise<void>((r) => {
      releaseAppend = r;
    });
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (armGate && !paused && ctx.kind === "manifest") {
        paused = true;
        reachedAppendWrite();
        await appendGate; // hold r1's per-run lock until the test releases it
      }
      await write();
    };

    const o = makeOutbox(root, { retentionMs: 1000, now: () => clock, rawWrite });
    await o.init();

    // r1 gets one record, then is drained empty and (via the sweep's `now`) aged past
    // retentionMs relative to its last write.
    await o.appendSegment("r1", 1, [textMsg(1, "first")]);
    const c0 = collector();
    const drained = await o.drainRun("r1", c0.send);
    assert.equal(drained.retired, true);
    assert.equal(o.depthFor("r1")?.pendingMessages, 0, "r1 is empty before the sweep");

    const sweepNow = clock + 5000; // well past retentionMs relative to r1's last write

    // Start (do not await) a legitimate appendSegment for a NEW record. It pauses at
    // its manifest write, holding r1's lock, so its record is not yet committed.
    armGate = true;
    const appendP = o.appendSegment("r1", 1, [textMsg(2, "second")]);
    await appendAtWrite;

    // sweepRetention collects r1 as empty (the paused append has not committed) — the
    // collection loop is synchronous, so it reads the still-empty in-memory manifest
    // before the append commits — then queues the removal behind r1's held lock.
    const sweepP = o.sweepRetention(sweepNow);

    // Release the paused append: it commits the new record and frees r1's lock; the
    // queued removal then runs and must re-check + SKIP, not delete.
    releaseAppend();
    await appendP;
    await sweepP;

    // The appended record SURVIVES: r1 is still tracked with one pending record.
    assert.equal(o.depthFor("r1")?.pendingMessages, 1, "the record appended during the sweep survives");

    // And a fresh Outbox over the same root replays it (r1's directory was NOT deleted).
    const b = makeOutbox(root, { retentionMs: 1000, now: () => clock });
    await b.init();
    const c = collector();
    const res = await b.drainRun("r1", c.send);
    assert.equal(res.retired, true);
    assert.deepEqual(
      c.flat().map((m) => m.seq),
      [2],
      "the just-appended record replays; r1 was not silently deleted",
    );
    assert.equal((c.flat()[0]?.payload as { text: string } | undefined)?.text, "second");
    assert.equal(c.flat()[0]?.kind, "text", "the survivor is the real message, not a tombstone");
  });

  it("H4b. ordinary reclaim still deletes an empty, aged run with no concurrent append", async () => {
    // The fix does not weaken the ordinary reclaim path: with no concurrent append the
    // under-lock re-check still finds the run empty and aged, so it IS removed.
    const root = await mkRoot();
    const clock = 1_000_000;
    const o = makeOutbox(root, { retentionMs: 1000, now: () => clock });
    await o.init();
    await o.appendSegment("r1", 1, [textMsg(1, "x")]);
    const c = collector();
    const res = await o.drainRun("r1", c.send);
    assert.equal(res.retired, true);
    assert.equal(o.depthFor("r1")?.pendingMessages, 0);

    await o.sweepRetention(clock + 5000);
    assert.equal(o.depthFor("r1"), undefined, "an empty, aged run with no concurrent append is reclaimed");
    assert.ok(!existsSync(path.join(root, "r1")), "its directory was deleted");
  });
});
