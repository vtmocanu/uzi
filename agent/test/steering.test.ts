import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { SteeringChannel, type PlanVerdict } from "../src/steering.js";
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
    assert.deepStrictEqual(v, { kind: "approve" } satisfies PlanVerdict);
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
    assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "approve" });
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
    assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "approve" });
    await ch.stop();
  });
});
