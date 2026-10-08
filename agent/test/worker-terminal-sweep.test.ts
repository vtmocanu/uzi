import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";

import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { OutboxHeartbeatEntry, StateAck, StateRequest } from "../src/protocol.js";
import { mkSweepOutbox, pollUntil, recordingLogger, startSweepWorker, sweepClient } from "./worker-sweep-rig.js";
import { sleep } from "../src/util.js";

// Issue #1512: the heartbeat-driven terminal sweep. A journaled run terminal whose send failed and
// whose run has NO message-outbox records used to strand until boot or re-claim, because the only
// live re-resolve was the drainer's retire hook and the drainer iterates message records only. Each
// test drives a real Worker over a real Outbox on a tmp dir.

const RUN = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const RUN2 = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";

const roots: string[] = [];
afterEach(async () => {
  for (const r of roots.splice(0)) await fs.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function rig() {
  const r = await mkSweepOutbox();
  roots.push(r.root);
  return r.outbox;
}

const applied = (status = "completed"): StateAck => ({ applied: true, status });

describe("Worker heartbeat terminal sweep (issue #1512)", () => {
  it("live G2 terminal retirement preserves guarded pending G1 finalize", async () => {
    const outbox = await rig();
    let beats = 0;
    let registrations = 0;
    let g1Pending = true;
    const checked: number[] = [];
    const client = sweepClient({ heartbeat: async () => { beats++; }, reportState: async () => applied() });
    const register = client.register.bind(client);
    client.register = async (...args) => {
      registrations++;
      return register(...args);
    };
    const w = startSweepWorker({
      outbox, client,
      runner: { recoveryInventoryPending: async (_runId, generation) => {
        checked.push(generation);
        return generation === 1 && g1Pending;
      } },
    });
    try {
      await pollUntil(() => beats >= 3, 3000, "boot completed");
      await outbox.journalFinalize(RUN, 1);
      await outbox.journalFinalize(RUN, 2);
      await outbox.journalTerminal(RUN, 2, "running", 0, { status: "completed" });
      await pollUntil(() => checked.includes(1) && !outbox.hasPendingTerminal(RUN, 2) &&
        !outbox.listPendingFinalizes().some(e => e.claim_generation === 2), 3000, "G2 retired");
      const seen = beats;
      await pollUntil(() => {
        assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: RUN, claim_generation: 1 }],
          "guarded G1 finalize remains pending during live heartbeats");
        assert.equal(outbox.hasPendingTerminal(RUN, 2), false, "G2 terminal stays retired");
        assert.equal(registrations, 1, "the worker has not re-registered");
        return beats >= seen + 5;
      }, 3000, "several heartbeats preserve guarded G1");
      g1Pending = false;
      await pollUntil(() => outbox.listPendingFinalizes().length === 0, 3000,
        "a live heartbeat retires G1 after its recovery inventory clears");
    } finally { await w.stop(); }
    assert.ok(checked.includes(1), "G1 recovery inventory was checked");
    assert.equal(outbox.hasPendingTerminal(RUN, 2), false, "G2 terminal remains retired");
    assert.deepEqual(outbox.listPendingFinalizes(), []);
    assert.equal(registrations, 1, "G1 retirement requires no restart or re-registration");
  });

  it("2. a no-spill `completed` journal whose boot send failed is delivered by a later heartbeat", async () => {
    const outbox = await rig();
    await outbox.journalTerminal(RUN, 3, "running", 0, { status: "completed", branch: "agent/issue-1" });
    // Health flips on an observed event (the boot resolve's failed send), never on time or a count.
    const sent: Array<{ runId: string; body: StateRequest }> = [];
    let failedSends = 0;
    const client = sweepClient({
      reportState: async (runId, body) => {
        if (failedSends === 0) {
          failedSends += 1;
          throw new Error("api down");
        }
        sent.push({ runId, body });
        return applied();
      },
    });
    const w = startSweepWorker({ outbox, client });
    try {
      await pollUntil(() => sent.length > 0, 3000, "a heartbeat delivers the journaled completed terminal");
      await pollUntil(() => !outbox.hasPendingTerminal(RUN, 3), 3000, "the delivered journal retires");
    } finally {
      await w.stop();
    }
    assert.ok(failedSends >= 1, "the boot resolve's send failed during the outage");
    assert.equal(sent.length, 1, "delivered exactly once");
    assert.equal(sent[0]!.body.status, "completed");
    assert.equal(sent[0]!.body.claim_generation, 3, "stamped with the journal's generation");
  });

  it("3. post-drain replay failure: the drain-retire send fails, a later heartbeat sweep delivers", async () => {
    const outbox = await rig();
    await outbox.appendSegment(RUN, 2, [{ seq: 1, kind: "text", payload: { text: "hi" } }]);
    // The journal is installed only AFTER the boot resolve has finished (a few heartbeats in), and the
    // segment stays unreplayable until then, so the drain-retire's resolve is the ONLY send that can
    // fail after the drain: a base without the heartbeat sweep has nothing that retries it.
    let replayAllowed = false;
    let failedAfterDrain = 0;
    let beats = 0;
    const sent: StateRequest[] = [];
    const client = sweepClient({
      heartbeat: async () => {
        beats += 1;
      },
      postMessages: async () => {
        if (!replayAllowed) throw new Error("api down");
      },
      reportState: async (_runId, body) => {
        if (failedAfterDrain === 0) {
          if (!outbox.hasUndrainedMessages(RUN)) failedAfterDrain += 1;
          throw new Error("api down");
        }
        sent.push(body);
        return applied("failed");
      },
    });
    const w = startSweepWorker({ outbox, client });
    try {
      await pollUntil(() => beats >= 3, 3000, "worker booted and the boot resolve finished");
      await outbox.journalTerminal(RUN, 2, "running", 1, { status: "failed" });
      replayAllowed = true;
      await pollUntil(() => sent.length > 0, 3000, "the sweep delivers after the drain-retire send failed");
      await pollUntil(() => !outbox.hasPendingTerminal(RUN, 2), 3000, "journal retires");
    } finally {
      await w.stop();
    }
    assert.equal(outbox.hasUndrainedMessages(RUN), false, "the spilled segment drained");
    assert.equal(failedAfterDrain, 1, "a post-drain replay failed first, then a later heartbeat delivered");
    assert.equal(sent.length, 1);
  });

  it("4. a non-terminal ack keeps the journal; a later heartbeat's applied ack retires it", async () => {
    const outbox = await rig();
    await outbox.journalTerminal(RUN, 4, "running", 0, { status: "failed" });
    let calls = 0;
    const client = sweepClient({
      reportState: async () => {
        calls += 1;
        return calls === 1 ? { applied: false, status: "running" } : applied("failed");
      },
    });
    const w = startSweepWorker({ outbox, client });
    try {
      await pollUntil(() => !outbox.hasPendingTerminal(RUN, 4), 3000, "a later heartbeat retires the journal");
    } finally {
      await w.stop();
    }
    assert.equal(calls, 2, "boot's non-terminal ack kept it; one sweep send applied it");
  });

  it("5a. a HELD terminal is not sent by the sweep and the skip is recorded", async () => {
    const outbox = await rig();
    let sends = 0;
    let beats = 0;
    const client = sweepClient({
      heartbeat: async () => {
        beats += 1;
      },
      reportState: async () => {
        sends += 1;
        return applied("failed");
      },
    });
    const w = startSweepWorker({ outbox, client });
    try {
      await pollUntil(() => beats > 0, 2000, "worker booted");
      outbox.holdTerminalResolve(RUN, 6);
      await outbox.journalTerminal(RUN, 6, "running", 0, { status: "failed" });
      const seen = beats;
      await pollUntil(() => beats >= seen + 5, 2000, "several heartbeats while held");
      assert.equal(sends, 0, "a held terminal is never sent by the sweep");
      assert.deepEqual(outbox.releaseTerminalResolve(RUN, 6), { skipped: true }, "the sweep recorded its skip");
      await pollUntil(() => sends === 1, 2000, "once released, a later sweep delivers");
    } finally {
      await w.stop();
    }
  });

  it("5b. a BLOCKED journal is never sent; it shows in the heartbeat depth with its reason", async () => {
    const outbox = await rig();
    let sends = 0;
    let latest: OutboxHeartbeatEntry[] | undefined;
    const client = sweepClient({
      heartbeat: async (entries) => {
        latest = entries;
      },
      reportState: async () => {
        sends += 1;
        return applied("failed");
      },
    });
    // Journal and block BEFORE the worker starts: a heartbeat landing between the two calls
    // could otherwise send and retire the still-unblocked journal during setup.
    await outbox.journalTerminal(RUN, 2, "running", 0, { status: "failed" });
    await outbox.markTerminalBlocked(RUN, 2, "gap_unrecoverable");
    const w = startSweepWorker({ outbox, client });
    try {
      await pollUntil(
        () => (latest ?? []).some((e) => e.run_id === RUN && e.blocked_reason === "gap_unrecoverable"),
        2000,
        "blocked journal reported with its reason",
      );
      await sleep(40);
    } finally {
      await w.stop();
    }
    assert.equal(sends, 0, "a blocked journal is owner-resolved, never auto-sent");
    const entry = latest!.find((e) => e.run_id === RUN)!;
    assert.equal(entry.pending_terminal, 1);
    assert.equal(entry.blocked_reason, "gap_unrecoverable");
  });

  it("5c. a run with undrained message segments is left to the drainer, not the sweep", async () => {
    const outbox = await rig();
    let sends = 0;
    let beats = 0;
    const client = sweepClient({
      heartbeat: async () => {
        beats += 1;
      },
      // The drain cannot land: the segment stays undrained.
      postMessages: async () => {
        throw new Error("messages route down");
      },
      reportState: async () => {
        sends += 1;
        return applied("failed");
      },
    });
    const w = startSweepWorker({ outbox, client });
    try {
      await pollUntil(() => beats > 0, 2000, "worker booted");
      await outbox.appendSegment(RUN, 1, [{ seq: 1, kind: "text", payload: { text: "x" } }]);
      await outbox.journalTerminal(RUN, 1, "running", 1, { status: "failed" });
      const seen = beats;
      await pollUntil(() => beats >= seen + 5, 2000, "several heartbeats");
    } finally {
      await w.stop();
    }
    assert.equal(outbox.hasUndrainedMessages(RUN), true, "precondition: segment still undrained");
    assert.equal(sends, 0, "the sweep did not send for a run the drainer owns");
  });

  it("5d. a stale_claim ack stale-retires the journal with no further sends", async () => {
    const outbox = await rig();
    let beats = 0;
    let sends = 0;
    const client = sweepClient({
      heartbeat: async () => {
        beats += 1;
      },
      reportState: async () => {
        sends += 1;
        return { applied: false, staleClaim: true };
      },
    });
    const w = startSweepWorker({ outbox, client });
    try {
      await pollUntil(() => beats > 0, 2000, "worker booted");
      await outbox.journalTerminal(RUN, 3, "running", 0, { status: "failed" });
      // staleRetired is committed after the journal leaves the pending list, so poll the final state.
      await pollUntil(() => outbox.depthFor(RUN)?.staleRetired === 1, 2000, "stale-retired");
      const seen = beats;
      await pollUntil(() => beats >= seen + 4, 2000, "more heartbeats");
    } finally {
      await w.stop();
    }
    assert.equal(sends, 1, "no re-send after the stale retire");
    assert.equal(outbox.depthFor(RUN)?.staleRetired, 1);
  });

  it("5e. a run live in this process (ActiveRunRegistry) is not swept; once removed it is", async () => {
    const outbox = await rig();
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);
    let beats = 0;
    let sends = 0;
    const client = sweepClient({
      heartbeat: async () => {
        beats += 1;
      },
      reportState: async () => {
        sends += 1;
        return applied("failed");
      },
    });
    const w = startSweepWorker({ outbox, client, activeRuns: registry });
    try {
      await pollUntil(() => beats > 0, 2000, "worker booted");
      registry.add(RUN, 5);
      await outbox.journalTerminal(RUN, 5, "running", 0, { status: "failed" });
      const seen = beats;
      await pollUntil(() => beats >= seen + 5, 2000, "heartbeats while live");
      assert.equal(sends, 0, "a live run's terminal is owned by its own lane");
      registry.remove(RUN);
      await pollUntil(() => sends === 1, 2000, "delivered once no longer live");
    } finally {
      await w.stop();
    }
  });

  it("5f. HOLD and LIVE together: neither the sweep nor the drain-retire sends, both record the held skip", async () => {
    const outbox = await rig();
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);
    let beats = 0;
    let sends = 0;
    const rec = recordingLogger();
    const heldSkips = (via: string) =>
      rec.info.filter((e) => e.msg.includes("held by the live run's permanent-failure hook") && e.fields.via === via).length;
    const client = sweepClient({
      heartbeat: async () => {
        beats += 1;
      },
      reportState: async () => {
        sends += 1;
        return applied("failed");
      },
    });
    const w = startSweepWorker({ outbox, client, activeRuns: registry, log: rec.log });
    try {
      await pollUntil(() => beats > 0, 2000, "worker booted");
      registry.add(RUN, 9);
      outbox.holdTerminalResolve(RUN, 9);
      await outbox.journalTerminal(RUN, 9, "running", 1, { status: "failed" });
      // Sweep path: no message records, so only the sweep reaches it.
      const seen = beats;
      await pollUntil(() => beats >= seen + 4, 2000, "heartbeats");
      assert.equal(sends, 0, "sweep: held+live is not sent");
      assert.ok(heldSkips("sweep") >= 1, "the sweep path itself logged the held skip");
      assert.deepEqual(outbox.releaseTerminalResolve(RUN, 9), { skipped: true }, "sweep recorded the held skip (hold checked BEFORE liveness)");

      // Drain-retire path: a spilled segment whose retire fires the drain-retire resolve.
      outbox.holdTerminalResolve(RUN, 9);
      await outbox.appendSegment(RUN, 9, [{ seq: 1, kind: "text", payload: { text: "y" } }]);
      // The sweep skips a run with undrained segments, so the first held skip logged via "drain" is
      // the drain-retire's own (resolveRunTerminal after the segment retired).
      await pollUntil(() => heldSkips("drain") >= 1, 2000, "the drain-retire logged its own held skip");
      assert.ok(!outbox.hasUndrainedMessages(RUN), "the drain retired the segment");
      assert.equal(sends, 0, "drain-retire: held+live is not sent");
      assert.deepEqual(outbox.releaseTerminalResolve(RUN, 9), { skipped: true }, "drain-retire recorded the held skip");
    } finally {
      await w.stop();
    }
  });

  it("6. concurrency: boot resolve + drain-retire + several sweeps over one journal send exactly once at a time", async () => {
    const outbox = await rig();
    await outbox.appendSegment(RUN, 3, [{ seq: 1, kind: "text", payload: { text: "z" } }]);
    await outbox.journalTerminal(RUN, 3, "running", 1, { status: "failed" });
    // Observe the drain retiring the segment: the drain-retire resolve runs right after it, inside
    // the same drainOutbox step, while the boot resolve still holds the journal in flight.
    let drainRetired = false;
    const realDrainRun = outbox.drainRun.bind(outbox);
    outbox.drainRun = (async (...args: Parameters<typeof realDrainRun>) => {
      const res = await realDrainRun(...args);
      if (res.retired) drainRetired = true;
      return res;
    }) as typeof outbox.drainRun;
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let concurrent = 0;
    let maxConcurrent = 0;
    let calls = 0;
    let beats = 0;
    const client = sweepClient({
      heartbeat: async () => {
        beats += 1;
      },
      reportState: async () => {
        calls += 1;
        concurrent += 1;
        maxConcurrent = Math.max(maxConcurrent, concurrent);
        await gate;
        concurrent -= 1;
        return applied("failed");
      },
    });
    const w = startSweepWorker({ outbox, client });
    try {
      await pollUntil(() => calls === 1, 2000, "the boot resolve is in flight");
      // Boot blocks the claim gate, but the heartbeat loop runs: let sweeps fire while it is held.
      await pollUntil(() => drainRetired, 2000, "the drain retired the spilled segment (drain-retire fires)");
      const seen = beats;
      await pollUntil(() => beats >= seen + 4, 2000, "heartbeats after the drain-retire while the boot send is gated");
      assert.equal(calls, 1, "sweeps skipped while a resolve for the journal was in flight");
      assert.equal(maxConcurrent, 1);
      release();
      await pollUntil(() => !outbox.hasPendingTerminal(RUN, 3), 2000, "the owner retires the journal");
    } finally {
      release();
      await w.stop();
    }
    assert.equal(calls, 1, "exactly one send overall");
  });

  it("7. telemetry: a terminal-only run appears in the heartbeat outbox depth", async () => {
    const outbox = await rig();
    let latest: OutboxHeartbeatEntry[] | undefined;
    const client = sweepClient({
      heartbeat: async (entries) => {
        latest = entries;
      },
      reportState: async () => {
        throw new Error("api down");
      },
    });
    await outbox.journalTerminal(RUN2, 1, "running", 0, { status: "failed" });
    const w = startSweepWorker({ outbox, client });
    try {
      await pollUntil(() => latest !== undefined, 2000, "a heartbeat carrying outbox depth");
    } finally {
      await w.stop();
    }
    const entry = latest!.find((e) => e.run_id === RUN2);
    assert.ok(entry, "the terminal-only run is reported");
    assert.equal(entry.pending_messages, 0);
    assert.equal(entry.pending_terminal, 1);
  });
});
