import { describe, it, expect } from "vitest";
import { emptyStream, ingest, ingestMany, startRunGate } from "./runStream";
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
