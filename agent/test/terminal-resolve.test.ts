import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { existsSync } from "node:fs";
import os from "node:os";
import path from "node:path";

import { Outbox, canonicalizeTerminalBody } from "../src/outbox.js";
import {
  journalAndResolveTerminal,
  resolvePendingTerminal,
  makeTerminalOutboxDeps,
  type TerminalOutboxDeps,
} from "../src/terminal-resolve.js";
import { RequestError } from "../src/client.js";
import type { MessageGapsResponse, OutgoingMessage, StateAck, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// PRD #1391 Run B M3b — the write-ahead terminal-report SEND path. Every test drives the real
// Outbox store (a fresh mkdtemp root) plus a fake client + a scripted `send`, so the classification,
// gap-fill, fence negotiation and first-writer arbitration are exercised end to end against the real
// journal on disk.

const tmpRoots: string[] = [];
afterEach(async () => {
  for (const r of tmpRoots.splice(0)) await fs.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function mkOutbox(): Promise<{ outbox: Outbox; root: string }> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "term-resolve-"));
  tmpRoots.push(dir);
  const root = path.join(dir, "outbox");
  const outbox = new Outbox({
    root,
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return { outbox, root };
}

/** A fake of the deps.client surface (hasFeature / getMessageGaps / postMessages). Scriptable per
 *  test; records the tombstone posts and the gaps reads. */
class FakeClient {
  features = new Set<string>();
  gapsCalls: Array<{ through: number; cursor: number }> = [];
  postCalls: Array<{ msgs: OutgoingMessage[]; generation: number | undefined }> = [];
  gapsResponder: (through: number, cursor: number) => MessageGapsResponse = () => ({ gaps: [] });
  postResponder: (msgs: OutgoingMessage[], generation: number | undefined) => void = () => undefined;

  hasFeature(f: string): boolean {
    return this.features.has(f);
  }
  async getMessageGaps(_runId: string, through: number, _limit?: number, cursor?: number): Promise<MessageGapsResponse> {
    this.gapsCalls.push({ through, cursor: cursor ?? 0 });
    return this.gapsResponder(through, cursor ?? 0);
  }
  async postMessages(_runId: string, msgs: OutgoingMessage[], generation?: number): Promise<void> {
    this.postCalls.push({ msgs, generation });
    this.postResponder(msgs, generation);
  }
}

/** A scripted `send`: records every terminal body it is asked to send and returns the next scripted
 *  ack (or throws the next scripted error). */
function scriptedSend(acks: Array<StateAck | Error>): {
  send: (body: StateRequest) => Promise<StateAck>;
  bodies: StateRequest[];
} {
  const bodies: StateRequest[] = [];
  let i = 0;
  const send = async (body: StateRequest): Promise<StateAck> => {
    bodies.push(structuredClone(body));
    const next = acks[Math.min(i, acks.length - 1)];
    i++;
    if (next instanceof Error) throw next;
    return next ?? { applied: true, status: "completed" };
  };
  return { send, bodies };
}

function depsFor(outbox: Outbox, client: FakeClient, over: Partial<{ gapFillMax: number }> = {}): TerminalOutboxDeps {
  const d = makeTerminalOutboxDeps(outbox, client, {
    gapFillMax: over.gapFillMax ?? 10_000,
    terminalMaxBytes: 1 << 20,
    log: nullLogger(),
  });
  assert.ok(d, "deps built (outbox enabled)");
  return d;
}

const GEN = 7;
const FENCE = 42;

describe("resolvePendingTerminal / journalAndResolveTerminal (PRD #1391 Run B M3b)", () => {
  it("journals BEFORE the first POST; the fence is the durable emitted tail; sent only with terminal_fence", async () => {
    const { outbox, root } = await mkOutbox();
    const client = new FakeClient();
    client.features.add("terminal_fence");
    const deps = depsFor(outbox, client);

    let journalExistedAtSend = false;
    let sentFence: number | undefined = -1;
    const send = async (body: StateRequest): Promise<StateAck> => {
      journalExistedAtSend = existsSync(path.join(root, "r1", `terminal-${GEN}.json`));
      sentFence = body.messages_through_seq;
      return { applied: true, status: "completed" };
    };

    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: FENCE,
      body: { status: "completed", branch: "agent/issue-9" },
      send,
    });

    assert.equal(journalExistedAtSend, true, "the journal was on disk before the first POST");
    assert.equal(sentFence, FENCE, "the sent fence is the durable emitted tail passed in, never a server value");
    // 200 applied ⇒ retired.
    assert.equal(outbox.depthFor("r1")?.pendingTerminal, 0, "a 200 retires the journal");
    assert.equal(outbox.hasPendingTerminal("r1", GEN), false, "no pending terminal remains after a 200");
    assert.equal(existsSync(path.join(root, "r1", `terminal-${GEN}.json`)), false, "the journal file was unlinked");
  });

  it("does NOT send messages_through_seq to an api that never advertised terminal_fence (D9)", async () => {
    const { outbox } = await mkOutbox();
    const client = new FakeClient(); // no terminal_fence feature
    const deps = depsFor(outbox, client);
    const { send, bodies } = scriptedSend([{ applied: true, status: "completed" }]);

    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: FENCE,
      body: { status: "completed" },
      send,
    });

    assert.equal(bodies[0]?.messages_through_seq, undefined, "no fence on the wire without terminal_fence");
  });

  it("a 409 `running` KEEPS the journal; a later resolve retires it once the api commits (response-loss-after-commit)", async () => {
    const { outbox } = await mkOutbox();
    const client = new FakeClient();
    const deps = depsFor(outbox, client);

    // First send: a benign 409 running (nothing changed server-side) → keep.
    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: 0,
      body: { status: "failed", failure_reason: "x" },
      send: scriptedSend([{ applied: false, status: "running" }]).send,
    });
    assert.equal(outbox.depthFor("r1")?.pendingTerminal, 1, "a 409 running keeps the journal");

    // A later resolve: the api had actually committed (the ack was lost) and now answers a 409 whose
    // returned status is terminal → retire.
    await resolvePendingTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      send: scriptedSend([{ applied: false, status: "completed" }]).send,
    });
    assert.equal(outbox.depthFor("r1")?.pendingTerminal, 0, "a 409 terminal retires the journal");
    assert.equal(outbox.hasPendingTerminal("r1", GEN), false);
  });

  it("a stale_claim 409 stale-retires LOCALLY: increments stale_retired, no server write, never rebinds", async () => {
    const { outbox } = await mkOutbox();
    const client = new FakeClient();
    const deps = depsFor(outbox, client);
    const scripted = scriptedSend([{ applied: false, status: "running", staleClaim: true }]);

    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: 3,
      body: { status: "completed" },
      send: scripted.send,
    });

    assert.equal(outbox.depthFor("r1")?.pendingTerminal, 0, "the journal is retired locally");
    assert.equal(outbox.depthFor("r1")?.staleRetired, 1, "the D11 stale-retired counter advanced by one");
    assert.equal(scripted.bodies.length, 1, "exactly ONE send — no rebind, no re-report under a new generation");
    assert.equal(client.postCalls.length, 0, "no server write (no tombstones, no rebind)");
  });

  it("messages_pending triggers the GAP-FILL loop: missing seqs get per-seq tombstones (carrying the gen), then the terminal applies", async () => {
    const { outbox } = await mkOutbox();
    const client = new FakeClient();
    client.features.add("terminal_fence");
    const deps = depsFor(outbox, client);
    // The gaps read reports seqs 2 and 3 missing in [1..3]; after the fill it is empty.
    let filled = false;
    client.gapsResponder = (_through, _cursor) => (filled ? { gaps: [] } : { gaps: [{ first: 2, last: 3 }] });
    client.postResponder = () => {
      filled = true;
    };
    // First send: messages_pending. Second send (after the fill): applied.
    const scripted = scriptedSend([
      { applied: false, status: "running", reason: "messages_pending" },
      { applied: true, status: "completed" },
    ]);

    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: 3,
      body: { status: "completed" },
      send: scripted.send,
    });

    assert.equal(client.postCalls.length, 1, "one tombstone batch was posted for the gap");
    const seqs = client.postCalls[0]?.msgs.map((m) => m.seq);
    assert.deepEqual(seqs, [2, 3], "one per-seq tombstone for each missing seq");
    assert.equal(client.postCalls[0]?.generation, GEN, "the tombstones carry the journal's claim generation");
    const kind = client.postCalls[0]?.msgs[0]?.kind;
    assert.equal(kind, "status", "the tombstone matches Run A's status/message_dropped shape");
    assert.equal(scripted.bodies.length, 2, "the terminal was re-sent after the fill");
    assert.equal(outbox.depthFor("r1")?.pendingTerminal, 0, "the terminal applied and the journal retired");
  });

  it("a stale_claim on a tombstone append STOPS the fill and stale-retires the journal (D11)", async () => {
    const { outbox } = await mkOutbox();
    const client = new FakeClient();
    const deps = depsFor(outbox, client);
    client.gapsResponder = () => ({ gaps: [{ first: 2, last: 2 }] });
    client.postResponder = () => {
      // A newer flight owns the run: the fenced tombstone append is refused stale_claim.
      throw new RequestError("POST", "/messages", 409, '{"disposition":"stale_claim"}');
    };
    const scripted = scriptedSend([{ applied: false, status: "running", reason: "messages_pending" }]);

    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: 3,
      body: { status: "completed" },
      send: scripted.send,
    });

    assert.equal(outbox.depthFor("r1")?.pendingTerminal, 0, "the journal is stale-retired, not left pending");
    assert.equal(outbox.depthFor("r1")?.staleRetired, 1, "stale_retired advanced");
    assert.equal(scripted.bodies.length, 1, "the terminal was NOT re-sent after the stale-claim stop");
  });

  it("gap_unrecoverable on the first send marks the journal BLOCKED (D13)", async () => {
    const { outbox } = await mkOutbox();
    const client = new FakeClient();
    const deps = depsFor(outbox, client);

    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: 3,
      body: { status: "completed" },
      send: scriptedSend([{ applied: false, status: undefined, reason: "gap_unrecoverable" }]).send,
    });

    assert.equal(outbox.depthFor("r1")?.pendingTerminal, 1, "the journal stays (blocked, not retired)");
    assert.equal(outbox.depthFor("r1")?.blockedReason, "gap_unrecoverable", "it is marked blocked with the reason");
  });

  it("a gap wider than gapFillMax marks the journal BLOCKED (never spins)", async () => {
    const { outbox } = await mkOutbox();
    const client = new FakeClient();
    const deps = depsFor(outbox, client, { gapFillMax: 5 });
    // A single 100-wide gap, far above the bound of 5 → blocked before any fill.
    client.gapsResponder = () => ({ gaps: [{ first: 1, last: 100 }] });

    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: 100,
      body: { status: "completed" },
      send: scriptedSend([{ applied: false, status: "running", reason: "messages_pending" }]).send,
    });

    assert.equal(outbox.depthFor("r1")?.blockedReason, "gap_unrecoverable", "past the bound ⇒ blocked");
    assert.equal(client.postCalls.length, 0, "nothing was posted once the hole was declared unrecoverable");
  });

  it("a send TRANSPORT failure leaves the journal for a later resolve, and never throws (a run-lane catch sees no second-failed path)", async () => {
    const { outbox } = await mkOutbox();
    const client = new FakeClient();
    const deps = depsFor(outbox, client);

    // The send exhausts its retries and throws — resolve must swallow it and keep the journal.
    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: 0,
      body: { status: "failed", failure_reason: "x" },
      send: scriptedSend([new Error("network down")]).send,
    });
    assert.equal(outbox.depthFor("r1")?.pendingTerminal, 1, "the durable journal is kept for a later resolve");
    assert.equal(outbox.hasPendingTerminal("r1", GEN), true, "the executor catch can observe the journaled outcome");
  });

  it("two outcomes for one generation keep the FIRST durable install (D4): a racing completion cannot reverse a journaled failed", async () => {
    const { outbox, root } = await mkOutbox();
    const client = new FakeClient();
    const deps = depsFor(outbox, client);

    // First writer: `failed` (the permanent-failure hook). Its send is a benign keep (409 running).
    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: 0,
      body: { status: "failed", failure_reason: "message transport failed" },
      send: scriptedSend([{ applied: false, status: "running" }]).send,
    });

    // A racing completion journals the SAME generation: the no-replace install adopts the first
    // winner, and its send resolves the ALREADY-journalled `failed` body (never the completed one).
    // Its send is a benign keep (409 running) so the journal file survives for inspection.
    const completion = scriptedSend([{ applied: false, status: "running" }]);
    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: 0,
      body: { status: "completed", branch: "agent/issue-9" },
      send: completion.send,
    });

    // The on-disk journal is still the FIRST `failed`, and what the completion path sent was the
    // journalled `failed` (read from disk by resolvePendingTerminal), never its own `completed`.
    const raw = JSON.parse(await fs.readFile(path.join(root, "r1", `terminal-${GEN}.json`), "utf8")) as {
      body: { status: string };
    };
    assert.equal(raw.body.status, "failed", "the first durable winner (failed) stands");
    assert.equal(completion.bodies.at(-1)?.status, "failed", "the racing completion re-sent the journalled failed, not completed");
  });

  it("the canonicaliser runs ONCE: the journalled body and the sent body are byte-identical", async () => {
    const { outbox, root } = await mkOutbox();
    const client = new FakeClient();
    client.features.add("terminal_fence");
    const deps = depsFor(outbox, client);
    const { send, bodies } = scriptedSend([{ applied: false, status: "running" }]); // keep, so we can read the file

    const body: StateRequest = { status: "completed", branch: "agent/issue-9", report_md: "done" };
    await journalAndResolveTerminal(deps, {
      runId: "r1",
      claimGeneration: GEN,
      phase: "running",
      messagesThroughSeq: FENCE,
      body,
      send,
    });

    const raw = JSON.parse(await fs.readFile(path.join(root, "r1", `terminal-${GEN}.json`), "utf8")) as {
      body: Record<string, unknown>;
    };
    const expected = canonicalizeTerminalBody(body as unknown as Record<string, unknown>, 1 << 20);
    assert.deepEqual(raw.body, expected, "the journalled body is the canonical body");
    // The sent body is the canonical body + only the fence (added at send time).
    const sent = { ...bodies[0] };
    delete sent.messages_through_seq;
    assert.deepEqual(sent, expected, "the sent body is the SAME canonical bytes, plus only the negotiated fence");
  });
});
