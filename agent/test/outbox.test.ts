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
  it("1. atomic write survives a simulated crash between temp and install", async () => {
    const root = await mkRoot();
    const a = makeOutbox(root);
    await a.init();
    await a.appendSegment("r1", 1, [textMsg(1, "hello")]);

    // Simulate a crash mid-install: leave stray *.tmp files behind. init must
    // ignore them and still load the last good manifest + segment.
    const runDir = path.join(root, "r1");
    await fs.writeFile(path.join(runDir, "manifest.json.crash.tmp"), "partial");
    await fs.writeFile(path.join(runDir, "seg-1-1-v9.json.crash.tmp"), "partial");

    const b = makeOutbox(root);
    await b.init();
    const c = collector();
    const res = await b.drainRun("r1", c.send);
    assert.equal(res.retired, true);
    assert.deepEqual(
      c.flat().map((m) => m.seq),
      [1],
    );
    assert.equal((c.flat()[0]?.payload as { text: string } | undefined)?.text, "hello");
    // The stray tmp files were ignored, not consumed or deleted.
    assert.ok(existsSync(path.join(runDir, "manifest.json.crash.tmp")));
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

  it("4. quota replaces the oldest segment with one range record → contiguous per-seq tombstones", async () => {
    const root = await mkRoot();
    // runMaxBytes = 1 forces eviction of the prior segment on every append.
    const o = makeOutbox(root, { runMaxBytes: 1 });
    await o.init();
    await o.appendSegment("r1", 1, [textMsg(1, "one")]);
    await o.appendSegment("r1", 1, [textMsg(2, "two")]);

    // Exactly one range record (evicted seg1) plus the surviving segment.
    const manifest = await readManifest(root, "r1");
    const kinds = (manifest.records as Array<{ kind: string; firstSeq: number; lastSeq: number }>).map((r) => r.kind);
    assert.deepEqual(kinds.sort(), ["range", "segment"]);
    assert.equal(o.depthFor("r1")?.pendingMessages, 2);

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
});
