import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { SteeringChannel, ChatSteering, type PlanVerdict } from "../src/steering.js";
import type { WorkerClient } from "../src/client.js";
import type { UserInput } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// The steering channel is the single /inputs poller; it routes verdicts,
// follow-ups, and cancel. Driven with a scripted getInputs — no live server.

function fakeClient(batches: UserInput[][]): WorkerClient {
  let i = 0;
  return { getInputs: async () => batches[i++] ?? [] } as unknown as WorkerClient;
}

const inp = (kind: UserInput["kind"], body?: string): UserInput => ({ id: 1, kind, body: body ?? null });
const tick = (ms = 10): Promise<void> => new Promise((r) => setTimeout(r, ms));

function makeChannel(batches: UserInput[][], cancel = new AbortController()): { ch: SteeringChannel; cancel: AbortController } {
  const ch = new SteeringChannel(fakeClient(batches), "run-1", 1, nullLogger(), cancel);
  return { ch, cancel };
}

describe("SteeringChannel", () => {
  it("resolves the gate verdict on approve_plan", async () => {
    const { ch } = makeChannel([[inp("approve_plan")]]);
    ch.start();
    const v = await ch.awaitVerdict();
    // An approve with no body carries an ABSENT selection parse (PRD #37): the
    // executor resolves it to the run's default source.
    assert.deepStrictEqual(v, { kind: "approve", selection: { status: "absent" } } satisfies PlanVerdict);
    await ch.stop();
  });

  it("resolves reject with the input body as the reason (defaulted when blank)", async () => {
    const { ch } = makeChannel([[inp("reject_plan", "do it differently")]]);
    ch.start();
    assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "reject", reason: "do it differently" });
    await ch.stop();

    const { ch: ch2 } = makeChannel([[inp("reject_plan", "")]]);
    ch2.start();
    assert.deepStrictEqual(await ch2.awaitVerdict(), { kind: "reject", reason: "plan rejected" });
    await ch2.stop();
  });

  it("buffers a verdict that arrives before the gate asks (no lost wakeup)", async () => {
    const { ch } = makeChannel([[inp("approve_plan")]]);
    ch.start();
    await tick(); // let the poll consume + buffer the verdict first
    assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "approve", selection: { status: "absent" } });
    await ch.stop();
  });

  it("aborts the cancel controller AND resolves a cancel verdict on cancel", async () => {
    const { ch, cancel } = makeChannel([[inp("cancel")]]);
    ch.start();
    assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "cancel" });
    assert.strictEqual(cancel.signal.aborted, true);
    await ch.stop();
  });

  it("queues follow-ups FIFO for injection between turns", async () => {
    const { ch } = makeChannel([[inp("follow_up", "first"), inp("follow_up", "second")]]);
    ch.start();
    await tick();
    assert.strictEqual(ch.pullFollowUp(), "first");
    assert.strictEqual(ch.pullFollowUp(), "second");
    assert.strictEqual(ch.pullFollowUp(), undefined);
    await ch.stop();
  });

  it("survives a poll error and keeps polling", async () => {
    let calls = 0;
    const client = {
      getInputs: async () => {
        calls++;
        if (calls === 1) throw new Error("transient");
        return calls === 2 ? [inp("approve_plan")] : [];
      },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "approve", selection: { status: "absent" } });
    await ch.stop();
  });
});

// PRD #41: plan revision at the gate. The channel epoch-stamps every verdict/revise so
// one written against a stale plan version is discardable, and a revise both enqueues
// (FIFO) and wakes the gate.
describe("SteeringChannel — plan revision (PRD #41)", () => {
  /** A client whose input batches are pushed on demand, so a test can interleave epoch
   *  bumps between what the poll loop consumes. Returns [] until a batch is pushed. */
  function pushableClient(): { client: WorkerClient; push: (b: UserInput[]) => void } {
    const queue: UserInput[][] = [];
    const client = { getInputs: async () => queue.shift() ?? [] } as unknown as WorkerClient;
    return { client, push: (b) => queue.push(b) };
  }

  it("routes revise_plan into a FIFO queue, one round per awaitGateEvent", async () => {
    const { ch } = makeChannel([[inp("revise_plan", "first"), inp("revise_plan", "second")]]);
    const e = ch.bumpEpoch();
    ch.start();
    assert.deepStrictEqual(await ch.awaitGateEvent(e), { kind: "revise", feedback: "first" } satisfies PlanVerdict);
    assert.deepStrictEqual(await ch.awaitGateEvent(e), { kind: "revise", feedback: "second" });
    await ch.stop();
  });

  it("a lone revise wakes a parked gate immediately (no verdict buffered)", async () => {
    const { ch } = makeChannel([[inp("revise_plan", "adjust the approach")]]);
    const e = ch.bumpEpoch();
    ch.start();
    // awaitGateEvent parks first (nothing buffered), then the routed revise wakes it.
    assert.deepStrictEqual(await ch.awaitGateEvent(e), { kind: "revise", feedback: "adjust the approach" });
    await ch.stop();
  });

  it("a current-epoch revise beats a buffered current-epoch approve ([revise, approve] batch)", async () => {
    // Both land in ONE batch at the same epoch. The gate must take the revision round —
    // approving the pre-feedback plan would defeat the point of the feedback.
    const { ch } = makeChannel([[inp("revise_plan", "please tweak"), inp("approve_plan")]]);
    const e = ch.bumpEpoch();
    ch.start();
    assert.deepStrictEqual(await ch.awaitGateEvent(e), { kind: "revise", feedback: "please tweak" });
    await ch.stop();
  });

  it("discards a PRIOR-epoch approve with a feed notice; a current-epoch approve lands", async () => {
    const notices: string[] = [];
    const { client, push } = pushableClient();
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController(), { notify: (t) => notices.push(t) });
    ch.bumpEpoch(); // epoch 1
    ch.start();
    push([inp("approve_plan")]); // consumed + buffered at epoch 1
    await tick();
    const e2 = ch.bumpEpoch(); // the plan was revised; epoch 1 is now stale
    // The buffered epoch-1 approve is discarded (with a notice) the moment we await epoch 2.
    const p = ch.awaitGateEvent(e2);
    push([inp("approve_plan")]); // a fresh approve, stamped at epoch 2
    assert.deepStrictEqual(await p, { kind: "approve", selection: { status: "absent" } });
    assert.ok(notices.some((n) => n.includes("Approval ignored")), notices.join("\n"));
    await ch.stop();
  });

  it("discards a PRIOR-epoch reject with a verdict-specific feed notice (not 'Approval ignored')", async () => {
    // PRD #41 Decision 3: a stale REJECT is discarded exactly like a stale approve (only
    // cancel is epoch-exempt), but the feed wording must read correctly for a rejection.
    const notices: string[] = [];
    const { client, push } = pushableClient();
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController(), { notify: (t) => notices.push(t) });
    ch.bumpEpoch(); // epoch 1
    ch.start();
    push([inp("reject_plan", "no thanks")]); // consumed + buffered at epoch 1
    await tick();
    const e2 = ch.bumpEpoch(); // the plan was revised; the epoch-1 reject is now stale
    const p = ch.awaitGateEvent(e2);
    push([inp("approve_plan")]); // a fresh approve at epoch 2 resolves the gate
    assert.deepStrictEqual(await p, { kind: "approve", selection: { status: "absent" } });
    assert.ok(notices.some((n) => n.includes("Rejection ignored")), notices.join("\n"));
    assert.ok(!notices.some((n) => n.includes("Approval ignored")), "reject must not read as an approval notice");
    await ch.stop();
  });

  it("discards a stale (prior-epoch) queued revise with a feed notice", async () => {
    const notices: string[] = [];
    const { client, push } = pushableClient();
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController(), { notify: (t) => notices.push(t) });
    ch.bumpEpoch(); // epoch 1
    ch.start();
    push([inp("revise_plan", "old feedback")]); // queued at epoch 1
    await tick();
    const e2 = ch.bumpEpoch(); // plan moved on; the queued revise is now stale
    const p = ch.awaitGateEvent(e2); // drops the stale revise (with a notice), then parks
    push([inp("approve_plan")]); // fresh approve at epoch 2 resolves the gate
    assert.deepStrictEqual(await p, { kind: "approve", selection: { status: "absent" } });
    assert.ok(notices.some((n) => n.includes("Feedback ignored")), notices.join("\n"));
    await ch.stop();
  });

  it("cancel is epoch-exempt: it applies even when stamped at an older epoch", async () => {
    const cancel = new AbortController();
    const { client, push } = pushableClient();
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel);
    ch.bumpEpoch(); // epoch 1
    ch.start();
    push([inp("cancel")]); // seen at epoch 1
    await tick();
    ch.bumpEpoch(); // epoch 2 — a stale approve would be dropped here, but cancel is exempt
    assert.deepStrictEqual(await ch.awaitGateEvent(2), { kind: "cancel" } satisfies PlanVerdict);
    assert.strictEqual(cancel.signal.aborted, true);
    await ch.stop();
  });
});

// ChatSteering (PRD #39 Decision 2): the chat lane's blocking await-next-follow-up.
// It owns the idle clock inside the poll loop, so a follow_up consumed on the same
// poll where idle would elapse is delivered, never dropped (team task #8).
describe("ChatSteering", () => {
  it("delivers a follow_up as a message", async () => {
    const ch = new ChatSteering(fakeClient([[inp("follow_up", "how does X work?")]]), "chat-1", 1, nullLogger(), new AbortController());
    ch.start();
    assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "message", text: "how does X work?" });
    await ch.stop();
  });

  it("delivers a follow_up buffered during a turn (no waiter registered yet)", async () => {
    const ch = new ChatSteering(fakeClient([[inp("follow_up", "buffered")]]), "chat-1", 1, nullLogger(), new AbortController());
    ch.start();
    await tick(); // the poll consumes + buffers it while nobody is parked
    assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "message", text: "buffered" });
    await ch.stop();
  });

  it("idle-completes after idleMs of no input (source owns the idle clock)", async () => {
    let clock = 0;
    const ch = new ChatSteering(fakeClient([[]]), "chat-1", 1, nullLogger(), new AbortController(), { now: () => clock });
    ch.start();
    const p = ch.awaitFollowUp(50); // parked at clock=0
    await tick(); // several polls, clock still 0 → not idle
    clock = 500; // advance past idleMs
    assert.deepStrictEqual(await p, { kind: "idle" });
    await ch.stop();
  });

  it("does NOT drop a follow_up that races the idle tick (team task #8)", async () => {
    // The poll that returns the follow_up ALSO advances the clock past idleMs — idle
    // and a message are both "due" in the same poll. Because the poll loop routes then
    // services the waiter (message checked before idle), the message wins. There is no
    // separate idle timer that could have fired first, so the consumed input is never lost.
    let clock = 0;
    let calls = 0;
    const client = {
      getInputs: async () => {
        calls++;
        if (calls >= 3) {
          clock = 10_000; // idle window (50) long elapsed...
          return [inp("follow_up", "raced-in")]; // ...but a follow_up arrives THIS poll
        }
        return [];
      },
    } as unknown as WorkerClient;
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController(), { now: () => clock });
    ch.start();
    assert.deepStrictEqual(await ch.awaitFollowUp(50), { kind: "message", text: "raced-in" });
    await ch.stop();
  });

  it("ends (and aborts the shared controller) on a cancel input — End chat", async () => {
    const cancel = new AbortController();
    const ch = new ChatSteering(fakeClient([[inp("cancel")]]), "chat-1", 1, nullLogger(), cancel);
    ch.start();
    assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "ended" });
    assert.strictEqual(cancel.signal.aborted, true, "cancel aborts the controller so a turn in flight also stops");
    await ch.stop();
  });

  it("settles a parked waiter with ended on stop() (worker shutdown)", async () => {
    const ch = new ChatSteering(fakeClient([[]]), "chat-1", 1, nullLogger(), new AbortController());
    ch.start();
    const p = ch.awaitFollowUp(100000); // parks (no input, huge idle)
    await tick();
    await ch.stop();
    assert.deepStrictEqual(await p, { kind: "ended" });
  });
});
