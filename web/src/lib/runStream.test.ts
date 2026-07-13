import { describe, it, expect } from "vitest";
import { applyFrame, emptyStream, ingest, ingestMany, startRunGate } from "./runStream";
import type { RunMessage } from "./api";

function msg(seq: number): RunMessage {
  return { seq, kind: "text", agent: null, payload: `m${seq}`, created_at: "" };
}

function seqs(msgs: RunMessage[]): number[] {
  return msgs.map((m) => m.seq);
}

describe("ingest", () => {
  it("appends contiguous messages and advances lastSeq", () => {
    let s = emptyStream();
    for (const n of [1, 2, 3]) s = ingest(s, msg(n)).state;
    expect(seqs(s.messages)).toEqual([1, 2, 3]);
    expect(s.lastSeq).toBe(3);
    expect(s.pending.size).toBe(0);
  });

  it("dedups a message at or below lastSeq (never double-renders)", () => {
    const s = ingestMany(emptyStream(), [msg(1), msg(2), msg(3)]).state;
    const r = ingest(s, msg(2));
    expect(r.gap).toBe(false);
    expect(seqs(r.state.messages)).toEqual([1, 2, 3]);
    expect(r.state).toBe(s); // no change → same reference
  });

  it("buffers an out-of-order message and reports a gap", () => {
    const s = ingestMany(emptyStream(), [msg(1)]).state;
    const r = ingest(s, msg(3)); // 2 is missing
    expect(r.gap).toBe(true);
    expect(seqs(r.state.messages)).toEqual([1]); // 3 not rendered yet
    expect(r.state.pending.has(3)).toBe(true);
    expect(r.state.lastSeq).toBe(1);
  });

  it("fills a gap and drains buffered pending in order (lossless)", () => {
    // A live frame 3 arrives before 2 (2 dropped) → buffered; the REST catch-up
    // then delivers 2, which renders 2 AND the buffered 3 contiguously.
    let s = ingestMany(emptyStream(), [msg(1)]).state;
    s = ingest(s, msg(3)).state;
    const r = ingest(s, msg(2));
    expect(r.gap).toBe(false);
    expect(seqs(r.state.messages)).toEqual([1, 2, 3]);
    expect(r.state.lastSeq).toBe(3);
    expect(r.state.pending.size).toBe(0);
  });

  it("stays lossless and dup-free across a reconnect replay", () => {
    // Rendered 1..3 live; the browser drops and reconnects. On reconnect it
    // REST-replays from lastSeq=3, getting [4,5]; a stale in-flight live frame 3
    // also lands and must be ignored.
    let s = ingestMany(emptyStream(), [msg(1), msg(2), msg(3)]).state;
    s = ingestMany(s, [msg(4), msg(5)]).state;
    s = ingest(s, msg(3)).state; // duplicate → dropped
    expect(seqs(s.messages)).toEqual([1, 2, 3, 4, 5]);
    expect(s.lastSeq).toBe(5);
  });

  it("ignores a re-buffered duplicate future seq", () => {
    let s = ingestMany(emptyStream(), [msg(1)]).state;
    s = ingest(s, msg(4)).state; // pending
    const r = ingest(s, msg(4)); // same future seq again
    expect(r.gap).toBe(false);
    expect(r.state.pending.size).toBe(1);
  });
});

describe("applyFrame", () => {
  it("ingests a contiguous message frame with no replay", () => {
    const s = ingestMany(emptyStream(), [msg(1)]).state;
    const r = applyFrame(s, { type: "message", seq: 2, kind: "text", payload: "x" });
    expect(r.effects).toEqual({ replay: false, refreshRun: false });
    expect(seqs(r.state.messages)).toEqual([1, 2]);
  });

  it("flags a replay when a message frame arrives past a gap", () => {
    const s = ingestMany(emptyStream(), [msg(1)]).state;
    const r = applyFrame(s, { type: "message", seq: 5, kind: "text", payload: "x" }); // 2..4 missing
    expect(r.effects.replay).toBe(true);
    expect(seqs(r.state.messages)).toEqual([1]); // not rendered ahead of the gap
  });

  it("backfills a hub-dropped tail message on a state frame, no reconnect", () => {
    // The hub dropped message 3 (the last one) right before the run went
    // awaiting_approval. Only a state frame arrives over WS.
    const s = ingestMany(emptyStream(), [msg(1), msg(2)]).state;
    const r = applyFrame(s, { type: "state", status: "awaiting_approval" });
    // The state frame carries no message data, but asks for a re-read AND a replay.
    expect(r.effects).toEqual({ replay: true, refreshRun: true });
    expect(seqs(r.state.messages)).toEqual([1, 2]);
    // The replay it triggers fetches messages after lastSeq (2) → [3]; ingesting
    // that batch completes the stream — the dropped tail is recovered live.
    const filled = ingestMany(r.state, [msg(3)]);
    expect(seqs(filled.state.messages)).toEqual([1, 2, 3]);
    expect(filled.state.lastSeq).toBe(3);
  });

  it("a health frame re-reads the run but does not replay (PRD #47)", () => {
    const s = ingestMany(emptyStream(), [msg(1), msg(2)]).state;
    const r = applyFrame(s, { type: "health" });
    // A health flip carries no message data and does not imply a dropped tail, so
    // it asks for a re-read (to pick up the owner-gated health fields) with no replay.
    expect(r.effects).toEqual({ replay: false, refreshRun: true });
    expect(seqs(r.state.messages)).toEqual([1, 2]);
  });

  it("ignores an unknown frame type", () => {
    const s = ingestMany(emptyStream(), [msg(1)]).state;
    const r = applyFrame(s, { type: "bogus" as unknown as "state" });
    expect(r.effects).toEqual({ replay: false, refreshRun: false });
    expect(r.state).toBe(s);
  });
});

describe("startRunGate", () => {
  const ok = {
    hasPrdLink: true,
    closed: false,
    hasWorker: true,
    hasToken: true,
    activeRunExists: false,
  };

  it("enables when every precondition is met", () => {
    expect(startRunGate(ok)).toEqual({ enabled: true, reason: "" });
  });

  it("blocks a closed issue before anything else", () => {
    const g = startRunGate({ ...ok, closed: true, hasPrdLink: false, hasWorker: false });
    expect(g.enabled).toBe(false);
    expect(g.reason).toMatch(/closed/i);
  });

  it("requires a PRD link", () => {
    const g = startRunGate({ ...ok, hasPrdLink: false });
    expect(g.enabled).toBe(false);
    expect(g.reason).toMatch(/prds/i);
  });

  // PRD #22: the prdless bypass short-circuits the PRD-link requirement — the full
  // bypass × hasPrdLink matrix.
  it("prdless bypass lets a no-PRD-link issue through the PRD-link gate", () => {
    // no link + bypass → the PRD-link precondition is skipped (other preconditions met → enabled).
    expect(startRunGate({ ...ok, hasPrdLink: false, prdlessBypass: true })).toEqual({
      enabled: true,
      reason: "",
    });
    // no link + no bypass → still blocked on the link.
    expect(startRunGate({ ...ok, hasPrdLink: false, prdlessBypass: false }).reason).toMatch(/prds/i);
    // link + bypass → enabled (bypass is a no-op when a link already satisfies the gate).
    expect(startRunGate({ ...ok, hasPrdLink: true, prdlessBypass: true }).enabled).toBe(true);
    // link + no bypass → enabled (the pre-prdless baseline).
    expect(startRunGate({ ...ok, hasPrdLink: true, prdlessBypass: false }).enabled).toBe(true);
  });

  it("prdless bypass does not skip the OTHER preconditions", () => {
    // A bypassed no-PRD-link issue with no worker is still blocked on the worker.
    expect(startRunGate({ ...ok, hasPrdLink: false, prdlessBypass: true, hasWorker: false }).reason).toMatch(
      /worker/i,
    );
  });

  it("requires a connected worker", () => {
    expect(startRunGate({ ...ok, hasWorker: false }).reason).toMatch(/worker/i);
  });

  it("requires an Anthropic token", () => {
    expect(startRunGate({ ...ok, hasToken: false }).reason).toMatch(/token/i);
  });

  it("blocks a second active run for the same issue", () => {
    expect(startRunGate({ ...ok, activeRunExists: true }).reason).toMatch(/already in progress/i);
  });
});
