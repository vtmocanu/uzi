import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  SteeringChannel,
  ChatSteering,
  PauseNowSignal,
  CredentialSwitchSignal,
  type PlanVerdict,
} from "../src/steering.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import type { UserInput } from "../src/protocol.js";
import { buildAgentGuardHook, NESTED_AGENT_TOOL } from "../src/guardrails.js";
import type { HookInput } from "@anthropic-ai/claude-agent-sdk";
import { nullLogger, stopStartedChannels, withReceipts } from "./helpers.js";

// The steering channel is the single /inputs poller; it routes verdicts,
// follow-ups, and cancel. Driven with a scripted getInputs — no live server.

// Issue #1663: stop every started channel after each test, even one whose assertion threw.
afterEach(stopStartedChannels);

function fakeClient(batches: UserInput[][]): WorkerClient {
  let i = 0;
  // PRD #1247 M5: getInputs now returns { inputs, credentialSwitch? }; the poller reads `.inputs`.
  return withReceipts({ getInputs: async () => ({ inputs: batches[i++] ?? [] }) } as unknown as WorkerClient);
}


// Every input gets a fresh id, as real run_user_inputs rows do (issue #1660: follow-ups are
// de-duplicated by id, so a shared constant id would read as one repeated row).
let nextInputId = 1;
const inp = (kind: UserInput["kind"], body?: string): UserInput => ({ id: nextInputId++, kind, body: body ?? null });
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
        return { inputs: calls === 2 ? [inp("approve_plan")] : [] };
      },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(withReceipts(client), "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "approve", selection: { status: "absent" } });
    await ch.stop();
  });
});

// PRD #1190 M2 — the owner-requested pause. `pause` sets the sticky pauseMode and, for the
// `now` mode ONLY, aborts the in-flight turn via PauseNowSignal (DISTINCT from cancel, which
// uses the default AbortError and fails the run). `pause_cancel` clears the flag; the claim
// seeds it on resume. The actual park BOUNDARY is decided server-side (the running-report ACK),
// so the channel only carries the request/mode and the `now` abort.
describe("SteeringChannel — pause (PRD #1190 M2)", () => {
  it("route('pause','milestone') sets pauseMode and does NOT abort the turn", async () => {
    const { ch, cancel } = makeChannel([[inp("pause", "milestone")]]);
    ch.start();
    await tick();
    assert.strictEqual(ch.getPauseMode(), "milestone", "the milestone mode is recorded");
    assert.strictEqual(cancel.signal.aborted, false, "a milestone pause must NOT abort the in-flight turn");
    await ch.stop();
  });

  it("route('pause','now') aborts the turn with a PauseNowSignal, NOT the cancel error", async () => {
    const { ch, cancel } = makeChannel([[inp("pause", "now")]]);
    ch.start();
    await tick();
    assert.strictEqual(ch.getPauseMode(), "now", "the now mode is recorded");
    assert.strictEqual(cancel.signal.aborted, true, "a now pause drops the in-flight turn");
    // The abort REASON is a PauseNowSignal — this is what lets the executor park rather than
    // fail: a cancel aborts the same controller with the default AbortError (a DOMException),
    // which must remain distinguishable from a pause.
    assert.ok(
      cancel.signal.reason instanceof PauseNowSignal,
      `the abort reason must be a PauseNowSignal, got ${String(cancel.signal.reason)}`,
    );
    assert.ok(
      !(cancel.signal.reason instanceof DOMException),
      "a now pause must NOT abort with the default AbortError the cancel path uses",
    );
    await ch.stop();
  });

  it("a bare 'pause' body defaults to the milestone (non-destructive) mode", async () => {
    const { ch, cancel } = makeChannel([[inp("pause")]]);
    ch.start();
    await tick();
    assert.strictEqual(ch.getPauseMode(), "milestone", "an absent body defaults to milestone");
    assert.strictEqual(cancel.signal.aborted, false, "the default mode does not abort");
    await ch.stop();
  });

  it("route('pause_cancel') clears a pending pauseMode", async () => {
    // Both in ONE batch (routed in order: pause sets the flag, pause_cancel clears it), so the
    // assertion does not race the poll cadence.
    const { ch } = makeChannel([[inp("pause", "milestone"), inp("pause_cancel")]]);
    ch.start();
    await tick();
    assert.strictEqual(ch.getPauseMode(), null, "pause_cancel (routed after the pause) withdraws it");
    await ch.stop();
  });

  it("seedPauseRequested reconstructs the flag from the claim on resume (no fresh input)", () => {
    const { ch } = makeChannel([[]]);
    assert.strictEqual(ch.getPauseMode(), null, "no pause pending on a fresh channel");
    ch.seedPauseRequested("now");
    assert.strictEqual(ch.getPauseMode(), "now", "the claim's pause_mode seeds the sticky flag");
  });

  it("seedPauseRequested ignores an unrecognised mode (leaves the flag null)", () => {
    const { ch } = makeChannel([[]]);
    ch.seedPauseRequested("garbage");
    assert.strictEqual(ch.getPauseMode(), null, "a garbage mode never sets a pause");
    ch.seedPauseRequested(undefined);
    assert.strictEqual(ch.getPauseMode(), null, "an absent mode never sets a pause");
  });

  it("a now pause after a cancel does not double-abort (cancel already won, it is terminal)", async () => {
    const { ch, cancel } = makeChannel([[inp("cancel")], [inp("pause", "now")]]);
    ch.start();
    await tick();
    assert.strictEqual(cancel.signal.aborted, true, "cancel aborted the controller");
    const reasonAfterCancel = cancel.signal.reason;
    await tick();
    // The pause routes and records its mode, but must NOT re-abort an already-aborted controller
    // (an AbortController cannot be reset) — the cancel reason stands.
    assert.strictEqual(cancel.signal.reason, reasonAfterCancel, "the cancel abort reason is not clobbered");
    assert.ok(
      !(cancel.signal.reason instanceof PauseNowSignal),
      "cancel remains the terminal outcome; the later now pause does not overwrite it",
    );
    await ch.stop();
  });

  // PRD #1190 rework (N2): the sticky cancel flag is exposed via isCancelled() so the executor can
  // re-check it at each loop boundary — a cancel that lands after the shared controller was spent by
  // a `now` pause is still honored because this durable flag records it.
  it("isCancelled() reports the sticky cancel flag", async () => {
    const { ch } = makeChannel([[inp("cancel")]]);
    assert.strictEqual(ch.isCancelled(), false, "no cancel seen on a fresh channel");
    ch.start();
    await tick();
    assert.strictEqual(ch.isCancelled(), true, "a cancel input sets the sticky flag");
    await ch.stop();
  });

  // PRD #1190 rework (N2): onPauseNow is a RE-ARMABLE interrupt invoked on EVERY `now` pause, so a
  // second `now` after the shared controller is already aborted still fires it — this is what lets
  // the executor drop the restarted turn on a second `now`.
  it("onPauseNow fires on every now pause, even after the controller is already aborted", async () => {
    // Two `now` pauses in separate batches; the controller aborts on the FIRST and cannot re-abort,
    // so the second `now` can only be delivered by the re-armable interrupt.
    const { ch, cancel } = makeChannel([[inp("pause", "now")], [inp("pause", "now")]]);
    let fired = 0;
    ch.onPauseNow(() => {
      fired++;
    });
    ch.start();
    await tick(30); // let both batches route (pollMs=1)
    assert.strictEqual(cancel.signal.aborted, true, "the first now pause aborted the shared controller");
    assert.strictEqual(
      fired,
      2,
      "the interrupt fired for BOTH now pauses — re-armed even after the controller was already spent",
    );
    await ch.stop();
  });

  // A `milestone` pause must NOT fire the pause-now interrupt (only `now` drops the turn).
  it("onPauseNow does NOT fire for a milestone pause", async () => {
    const { ch } = makeChannel([[inp("pause", "milestone")]]);
    let fired = 0;
    ch.onPauseNow(() => {
      fired++;
    });
    ch.start();
    await tick();
    assert.strictEqual(fired, 0, "a milestone pause never drops the in-flight turn");
    await ch.stop();
  });
});

// PRD #1497 M2 — the wall-clock park. The sweep files a system-authored `pause` input whose body is
// "wall". It aborts the in-flight turn EXACTLY like `now` (so the run parks in seconds, not at the
// next milestone), records mode "wall", and is STICKY-STICKY: `pause_cancel` cannot clear it (only
// clearWallMode after a refused wall_park, or the park landing, does). The mode is read via
// getPauseMode(); the PauseNowSignal that drops the turn carries no mode.
describe("SteeringChannel — wall park (PRD #1497 M2)", () => {
  it("route('pause','wall') aborts the turn with a PauseNowSignal (like now) and records mode 'wall'", async () => {
    const { ch, cancel } = makeChannel([[inp("pause", "wall")]]);
    ch.start();
    await tick();
    assert.strictEqual(ch.getPauseMode(), "wall", "the wall mode is recorded");
    assert.strictEqual(cancel.signal.aborted, true, "a wall pause drops the in-flight turn like a now pause");
    assert.ok(
      cancel.signal.reason instanceof PauseNowSignal,
      `the abort reason must be a PauseNowSignal, got ${String(cancel.signal.reason)}`,
    );
    await ch.stop();
  });

  it("onPauseNow fires for a wall pause (it drops the in-flight turn, like now)", async () => {
    const { ch } = makeChannel([[inp("pause", "wall")]]);
    let fired = 0;
    ch.onPauseNow(() => {
      fired++;
    });
    ch.start();
    await tick();
    assert.strictEqual(fired, 1, "a wall pause drops the in-flight turn via the re-armable interrupt");
    await ch.stop();
  });

  it("seedPauseRequested('wall') seeds the sticky wall mode (a re-claim with pause_pending + wall)", () => {
    const { ch } = makeChannel([[]]);
    assert.strictEqual(ch.getPauseMode(), null, "no pause pending on a fresh channel");
    ch.seedPauseRequested("wall");
    assert.strictEqual(ch.getPauseMode(), "wall", "the claim's pause_mode='wall' seeds the sticky flag");
  });

  it("pause_cancel does NOT clear a pending 'wall' mode (the system's involuntary park is not the owner's to withdraw)", async () => {
    // pause (wall) then pause_cancel in ONE batch: the pause sets wall, the pause_cancel must NOT clear it.
    const { ch } = makeChannel([[inp("pause", "wall"), inp("pause_cancel")]]);
    ch.start();
    await tick();
    assert.strictEqual(ch.getPauseMode(), "wall", "pause_cancel cannot withdraw a wall park");
    await ch.stop();
  });

  it("clearWallMode() clears a 'wall' mode but a following owner pause survives; it is a no-op with no wall pending", () => {
    const { ch } = makeChannel([[]]);
    ch.seedPauseRequested("wall");
    ch.clearWallMode();
    assert.strictEqual(ch.getPauseMode(), null, "clearWallMode cleared the wall mode (refused wall_park path)");
    // clearWallMode on a NON-wall mode is a no-op (only wall is cleared).
    ch.seedPauseRequested("now");
    ch.clearWallMode();
    assert.strictEqual(ch.getPauseMode(), "now", "clearWallMode leaves a non-wall mode intact");
  });
});

// PRD #1247 M5b — the held-state credential switch. A `credential_switch {generation}` field rides
// EVERY /inputs response (incl. an empty one). The channel acts on it ONLY when the generation
// matches THIS claim's (claimGeneration): it records the pending switch, trips the shared controller
// with a CredentialSwitchSignal reason (DISTINCT from a cancel), invokes the re-armable interrupt,
// and rejects any parked gate/answer/follow-up waiter (a run idling at the gate/question/follow-up
// has no live SDK turn to abort). A mismatched generation targets a superseded claim and is ignored.
describe("SteeringChannel — credential switch (PRD #1247 M5b)", () => {
  // A client whose getInputs yields scripted { inputs?, credentialSwitch? } batches (min-clamped so
  // the last batch repeats every subsequent tick, modelling the signal riding EVERY poll).
  function switchClient(
    batches: Array<{ inputs?: UserInput[]; credentialSwitch?: { generation: number } }>,
  ): WorkerClient {
    let i = 0;
    return {
      getInputs: async () => {
        const b = batches[Math.min(i, batches.length - 1)] ?? {};
        i++;
        return { inputs: b.inputs ?? [], credentialSwitch: b.credentialSwitch };
      },
    } as unknown as WorkerClient;
  }

  it("fires on a matching generation: records the pending switch, aborts WITH a CredentialSwitchSignal reason, and interrupts", async () => {
    const cancel = new AbortController();
    let interrupts = 0;
    const ch = new SteeringChannel(
      switchClient([{ credentialSwitch: { generation: 7 } }]),
      "run-1",
      1,
      nullLogger(),
      cancel,
      { claimGeneration: 7 },
    );
    ch.onCredentialSwitch(() => {
      interrupts++;
    });
    ch.start();
    for (let n = 0; n < 300 && ch.pendingCredentialSwitch() === undefined; n++) await tick();
    assert.strictEqual(ch.pendingCredentialSwitch(), 7, "the pending switch generation is recorded");
    assert.strictEqual(cancel.signal.aborted, true, "the shared controller is aborted");
    assert.ok(
      cancel.signal.reason instanceof CredentialSwitchSignal,
      "aborted WITH a CredentialSwitchSignal reason (so the executor maps it to a switch, not a cancel)",
    );
    // Let several more matching polls run: the fire is idempotent (once per pending switch).
    await tick(20);
    assert.strictEqual(interrupts, 1, "the re-armable interrupt fires exactly once, not every tick");
    await ch.stop();
  });

  it("BLOCKING-3: rearmCredentialSwitch re-opens the trip so a LATER same-generation signal fires again", async () => {
    const cancel = new AbortController();
    let interrupts = 0;
    const ch = new SteeringChannel(
      switchClient([{ credentialSwitch: { generation: 7 } }]), // re-offered on EVERY poll
      "run-1",
      1,
      nullLogger(),
      cancel,
      { claimGeneration: 7 },
    );
    ch.onCredentialSwitch(() => {
      interrupts++;
    });
    ch.start();
    for (let n = 0; n < 300 && ch.pendingCredentialSwitch() === undefined; n++) await tick();
    assert.strictEqual(interrupts, 1, "the switch tripped once");
    await tick(20);
    assert.strictEqual(interrupts, 1, "and stays tripped-once while pending (the once-only guard drops repeats)");
    // A give-up whose clear the server POSITIVELY confirmed re-arms the channel. The SAME-generation
    // signal — a re-request the owner makes on the STILL-OPEN claim, whose generation is pinned to
    // claim_generation for the claim's lifetime — is still offered on every poll and must trip AGAIN.
    // Without the re-arm the once-only guard would drop every subsequent signal for the claim forever.
    ch.rearmCredentialSwitch();
    assert.strictEqual(ch.pendingCredentialSwitch(), undefined, "rearm clears the pending switch");
    for (let n = 0; n < 300 && interrupts < 2; n++) await tick();
    assert.strictEqual(interrupts, 2, "the re-armed channel fires the switch again on the next matching poll");
    assert.strictEqual(ch.pendingCredentialSwitch(), 7, "and re-records the pending generation");
    await ch.stop();
  });

  it("MAJOR-6: a signal inside a defer window is HELD (not tripped/aborting), then fires once the window closes", async () => {
    const cancel = new AbortController();
    let interrupts = 0;
    const ch = new SteeringChannel(
      switchClient([{ credentialSwitch: { generation: 7 } }]), // re-offered every poll
      "run-1",
      1,
      nullLogger(),
      cancel,
      { claimGeneration: 7 },
    );
    ch.onCredentialSwitch(() => {
      interrupts++;
    });
    // Open a defer window BEFORE the poll loop can trip (the executor opens it around a revision turn).
    ch.beginCredentialSwitchDefer();
    ch.start();
    await tick(30); // several polls inside the window
    assert.strictEqual(ch.pendingCredentialSwitch(), undefined, "inside the defer window the switch is NOT recorded (no pending set)");
    assert.strictEqual(interrupts, 0, "inside the defer window the re-armable interrupt does NOT fire");
    assert.strictEqual(cancel.signal.aborted, false, "inside the defer window the current turn is NOT aborted");
    // Close the window: the same-generation signal, still offered every poll, now trips.
    ch.endCredentialSwitchDefer();
    for (let n = 0; n < 300 && ch.pendingCredentialSwitch() === undefined; n++) await tick();
    assert.strictEqual(ch.pendingCredentialSwitch(), 7, "once the window closes the switch trips at the next matching poll");
    assert.strictEqual(interrupts, 1, "and the interrupt fires exactly once");
    await ch.stop();
  });

  it("does NOT act on a switch whose generation does not match this claim (a superseded claim)", async () => {
    const cancel = new AbortController();
    let interrupts = 0;
    const ch = new SteeringChannel(
      switchClient([{ credentialSwitch: { generation: 5 } }]),
      "run-1",
      1,
      nullLogger(),
      cancel,
      { claimGeneration: 7 },
    );
    ch.onCredentialSwitch(() => {
      interrupts++;
    });
    ch.start();
    await tick(20);
    assert.strictEqual(ch.pendingCredentialSwitch(), undefined, "a mismatched generation is ignored");
    assert.strictEqual(cancel.signal.aborted, false, "the controller is NOT aborted for a stale signal");
    assert.strictEqual(interrupts, 0, "the interrupt never fires for a stale signal");
    await ch.stop();
  });

  it("rejects a parked plan-gate waiter with a CredentialSwitchSignal (idle at the gate — no live turn to abort)", async () => {
    const ch = new SteeringChannel(
      switchClient([{ credentialSwitch: { generation: 7 } }]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
      { claimGeneration: 7 },
    );
    // Park the gate waiter BEFORE starting the poll loop, so the switch deterministically rejects an
    // already-parked waiter (the gate-switch case, D13: no SDK turn is running at the gate).
    const parked = ch.awaitVerdict();
    ch.start();
    await assert.rejects(parked, (e) => e instanceof CredentialSwitchSignal);
    await ch.stop();
  });

  it("rejects a parked answer waiter (a run idling at an ask_user question) with a CredentialSwitchSignal", async () => {
    const ch = new SteeringChannel(
      switchClient([{ credentialSwitch: { generation: 7 } }]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
      { claimGeneration: 7 },
    );
    const parked = ch.awaitAnswer("q-1");
    ch.start();
    await assert.rejects(parked, (e) => e instanceof CredentialSwitchSignal);
    await ch.stop();
  });

  it("rejects a parked interactive follow-up waiter (a task run idling between turns) with a CredentialSwitchSignal", async () => {
    // The audit flagged the follow-up waiter as untested for the switch: an interactive task run
    // parked at awaitFollowUp has no live SDK turn to abort, so — like the gate and answer waiters —
    // the parked promise itself must reject with a CredentialSwitchSignal so the executor's
    // follow-up-wait switch handling (runThroughSwitch) can release or re-park it.
    const ch = new SteeringChannel(
      switchClient([{ credentialSwitch: { generation: 7 } }]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
      { claimGeneration: 7 },
    );
    const parked = ch.awaitFollowUp(60_000);
    ch.start();
    await assert.rejects(parked, (e) => e instanceof CredentialSwitchSignal);
    await ch.stop();
  });
});

// issue #559 M2: the channel tracks the highest follow_up input id it has already DELIVERED
// to the executor (getLastDeliveredFollowUpId) — the wake-guard watermark the runner reports
// as open_followup_id at the interactive park. Buffering a follow-up does NOT advance it; only
// delivery (the takeFollowUp shift) does, which is what keeps it stable across the park
// report's DB round-trip.
describe("SteeringChannel — last-delivered follow-up watermark (issue #559)", () => {
  const inpId = (kind: UserInput["kind"], id: number, body?: string): UserInput => ({
    id,
    kind,
    body: body ?? null,
  });

  it("threads the input id through route/follow_up; pullFollowUp advances the watermark to the delivered id", async () => {
    // Mutation: drop the `id` param on route() (or store body only) → the queue loses the id
    // and the watermark can never advance past 0.
    const { ch } = makeChannel([
      [inpId("follow_up", 3, "a"), inpId("follow_up", 7, "b")],
    ]);
    ch.start();
    await tick(); // poll consumes + buffers both — buffering must NOT advance the watermark
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 0, "buffered, not delivered");
    assert.strictEqual(ch.pullFollowUp(), "a");
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 3, "advanced to the first delivered id");
    assert.strictEqual(ch.pullFollowUp(), "b");
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 7, "advanced to the second delivered id");
    assert.strictEqual(ch.pullFollowUp(), undefined);
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 7, "an empty pull leaves it unchanged");
    await ch.stop();
  });

  it("awaitFollowUp's drain-after-arm advances the watermark; buffering alone does NOT (the race property)", async () => {
    // The race the whole feature closes: a follow-up merely BUFFERED (consumed by the poll loop
    // while no waiter is armed) must not move the watermark, so a report in flight cannot fold
    // it in. Only arming + delivering it advances the watermark. Mutation: make pullFollowUp/
    // awaitFollowUp shift `this.followUps` directly instead of via takeFollowUp → delivery no
    // longer advances the watermark and the final assert reddens.
    const { ch } = makeChannel([[inpId("follow_up", 12, "task")]]);
    ch.start();
    await tick(); // buffered, no waiter armed yet
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 0, "a buffered follow-up does not advance it");
    const outcome = await ch.awaitFollowUp(60_000); // immediate-return branch drains the buffer
    assert.deepStrictEqual(outcome, { kind: "followup", body: "task" });
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 12, "delivery advanced it");
    await ch.stop();
  });

  it("serviceFollowUp (poll-loop delivery to a parked waiter) advances the watermark", async () => {
    // The third delivery site: the waiter is armed BEFORE the first poll routes, so the follow-up
    // is delivered from serviceFollowUp (post-route), not the immediate-return branch. Mutation:
    // leave serviceFollowUp shifting `this.followUps` directly → the watermark stays 0.
    const { ch } = makeChannel([[inpId("follow_up", 9, "y")]]);
    ch.start();
    const parked = ch.awaitFollowUp(60_000); // arm before the first poll routes anything
    const outcome = await parked;
    assert.deepStrictEqual(outcome, { kind: "followup", body: "y" });
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 9, "serviceFollowUp advanced it");
    await ch.stop();
  });

  it("the watermark is monotone (Math.max) across out-of-order delivered ids", async () => {
    // A lower id delivered after a higher one must NOT regress the watermark. Mutation: replace
    // Math.max(...) with a plain assignment in takeFollowUp → the second pull drops it to 4.
    // Issue #1673 routes one batch in id order, so the lower id arrives in a later batch.
    const { ch } = makeChannel([[inpId("follow_up", 10, "hi")], [inpId("follow_up", 4, "lo")]]);
    ch.start();
    await tick();
    assert.strictEqual(ch.pullFollowUp(), "hi");
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 10);
    await tick(30);
    assert.strictEqual(ch.pullFollowUp(), "lo");
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 10, "a lower delivered id never regresses it");
    await ch.stop();
  });
});

// PRD #1416 M2: the worker-authoritative safety-steer slot. It originates IN-PROCESS (the
// runner's divergence detection), never from a server input, so it is entirely separate from the
// follow-up machinery — not routed, no input id, no wake-guard watermark, and invisible to the
// follow-up-outcome peek (D3). These drive the slot directly; no poll loop is needed.
describe("SteeringChannel — worker-authoritative safety steer (PRD #1416 M2)", () => {
  it("push then pull returns the steer and clears the slot", () => {
    const { ch } = makeChannel([]);
    ch.pushSafetySteer("restore the published tip as an ancestor");
    assert.strictEqual(ch.pullSafetySteer(), "restore the published tip as an ancestor");
    assert.strictEqual(ch.pullSafetySteer(), undefined, "the slot is cleared after one pull");
  });

  it("pushing twice keeps the latest (a later arm overwrites an unconsumed one)", () => {
    const { ch } = makeChannel([]);
    ch.pushSafetySteer("first");
    ch.pushSafetySteer("second");
    assert.strictEqual(ch.pullSafetySteer(), "second");
    assert.strictEqual(ch.pullSafetySteer(), undefined);
  });

  it("pulling an empty slot returns undefined", () => {
    const { ch } = makeChannel([]);
    assert.strictEqual(ch.pullSafetySteer(), undefined);
  });

  it("does NOT advance the follow-up watermark and does NOT make hasPendingFollowUpOutcome true (D3)", () => {
    // The whole point of D3: the worker steer must not corrupt the follow-up wake-guard. Mutation:
    // routing the steer through the follow-up queue (push → this.followUps) would advance the
    // watermark and flip hasPendingFollowUpOutcome, reddening both asserts.
    const { ch } = makeChannel([]);
    ch.pushSafetySteer("worker guidance");
    assert.strictEqual(ch.getLastDeliveredFollowUpId(), 0, "the steer never touches the follow-up watermark");
    assert.strictEqual(ch.hasPendingFollowUpOutcome(), false, "the steer is not a follow-up outcome");
    // And it is still deliverable via its own slot, unaffected by the follow-up machinery.
    assert.strictEqual(ch.pullSafetySteer(), "worker guidance");
  });
});

// PRD #41: plan revision at the gate. The channel epoch-stamps every verdict/revise so
// one written against a stale plan version is discardable, and a revise both enqueues
// (FIFO) and wakes the gate.
describe("SteeringChannel — plan revision (PRD #41)", () => {
  /** A client whose input batches are pushed on demand, so a test can interleave epoch
   *  bumps between what the poll loop consumes. Returns [] until a batch is pushed.
   *
   *  `consumed()` is the deterministic replacement for a bare `tick()` between a `push`
   *  and the `bumpEpoch()` that must follow the consumption (issue #242). The poll loop
   *  routes (and epoch-stamps) a dispensed batch synchronously BEFORE it polls again, so
   *  any getInputs call after a dispense proves the previously dispensed batch was routed
   *  at the epoch that was current when it was dispensed. Awaiting `consumed()` therefore
   *  waits — with no fixed delay, so it holds under any CPU contention — until every batch
   *  pushed so far has been routed. A bare `tick(10)` only *assumed* that finished in 10ms;
   *  under load the input could be consumed AFTER the epoch bump and stamped one epoch too
   *  late, which is the flake this replaces. */
  function pushableClient(): {
    client: WorkerClient;
    push: (b: UserInput[]) => void;
    consumed: () => Promise<void>;
  } {
    const queue: UserInput[][] = [];
    let pushedBatches = 0; // non-empty batches pushed
    let routedBatches = 0; // non-empty batches the loop has dispensed AND routed
    let dispensedPending = false;
    let waiters: { need: number; resolve: () => void }[] = [];
    const settle = (): void => {
      waiters = waiters.filter((w) => {
        if (routedBatches >= w.need) {
          w.resolve();
          return false;
        }
        return true;
      });
    };
    const client = {
      getInputs: async () => {
        // Any poll after a dispense proves the prior dispensed batch was routed.
        if (dispensedPending) {
          dispensedPending = false;
          routedBatches++;
          settle();
        }
        const b = queue.shift();
        if (b && b.length) {
          dispensedPending = true;
          return { inputs: b };
        }
        return { inputs: [] };
      },
    } as unknown as WorkerClient;
    return {
      client,
      push: (b) => {
        if (b.length) pushedBatches++;
        queue.push(b);
      },
      consumed: () =>
        new Promise<void>((resolve) => {
          waiters.push({ need: pushedBatches, resolve });
          settle();
        }),
    };
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

  it("order within the batch does not matter: [approve, revise] still takes the revise, not the approve", async () => {
    // The gate is serviced once per poll batch (not per input), so an approve at the HEAD
    // of the batch can't resolve the gate before the trailing revise routes. Servicing
    // per-input would silently drop the revise (and still burn a server cap slot). The
    // approve is left buffered — proven stale at the next epoch — not consumed.
    const notices: string[] = [];
    const { client, push } = pushableClient();
    const ch = new SteeringChannel(withReceipts(client), "run-1", 1, nullLogger(), new AbortController(), { notify: (t) => notices.push(t) });
    const e = ch.bumpEpoch();
    ch.start();
    push([inp("approve_plan"), inp("revise_plan", "tweak it")]); // approve FIRST in the batch
    assert.deepStrictEqual(await ch.awaitGateEvent(e), { kind: "revise", feedback: "tweak it" });
    // The batched approve was buffered, not dropped: at the next epoch it is the stale
    // pre-feedback version and is discarded with a notice, and a fresh approve then lands.
    const e2 = ch.bumpEpoch();
    const p = ch.awaitGateEvent(e2);
    push([inp("approve_plan")]);
    assert.deepStrictEqual(await p, { kind: "approve", selection: { status: "absent" } });
    assert.ok(notices.some((n) => n.includes("Approval ignored")), notices.join("\n"));
    await ch.stop();
  });

  it("discards a PRIOR-epoch approve with a feed notice; a current-epoch approve lands", async () => {
    const notices: string[] = [];
    const { client, push, consumed } = pushableClient();
    const ch = new SteeringChannel(withReceipts(client), "run-1", 1, nullLogger(), new AbortController(), { notify: (t) => notices.push(t) });
    ch.bumpEpoch(); // epoch 1
    ch.start();
    push([inp("approve_plan")]); // consumed + buffered at epoch 1
    await consumed(); // deterministically wait until it is routed at epoch 1 (issue #242)
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
    const { client, push, consumed } = pushableClient();
    const ch = new SteeringChannel(withReceipts(client), "run-1", 1, nullLogger(), new AbortController(), { notify: (t) => notices.push(t) });
    ch.bumpEpoch(); // epoch 1
    ch.start();
    push([inp("reject_plan", "no thanks")]); // consumed + buffered at epoch 1
    await consumed(); // deterministically wait until it is routed at epoch 1 (issue #242)
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
    const { client, push, consumed } = pushableClient();
    const ch = new SteeringChannel(withReceipts(client), "run-1", 1, nullLogger(), new AbortController(), { notify: (t) => notices.push(t) });
    ch.bumpEpoch(); // epoch 1
    ch.start();
    push([inp("revise_plan", "old feedback")]); // queued at epoch 1
    await consumed(); // deterministically wait until it is routed at epoch 1 (issue #242)
    const e2 = ch.bumpEpoch(); // plan moved on; the queued revise is now stale
    const p = ch.awaitGateEvent(e2); // drops the stale revise (with a notice), then parks
    push([inp("approve_plan")]); // fresh approve at epoch 2 resolves the gate
    assert.deepStrictEqual(await p, { kind: "approve", selection: { status: "absent" } });
    assert.ok(notices.some((n) => n.includes("Feedback ignored")), notices.join("\n"));
    await ch.stop();
  });

  it("cancel is epoch-exempt: it applies even when stamped at an older epoch", async () => {
    const cancel = new AbortController();
    const { client, push, consumed } = pushableClient();
    const ch = new SteeringChannel(withReceipts(client), "run-1", 1, nullLogger(), cancel);
    ch.bumpEpoch(); // epoch 1
    ch.start();
    push([inp("cancel")]); // seen at epoch 1
    await consumed(); // deterministically wait until the cancel is routed at epoch 1 (no wall-clock tick; issue #242)
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
          return { inputs: [inp("follow_up", "raced-in")] }; // ...but a follow_up arrives THIS poll
        }
        return { inputs: [] };
      },
    } as unknown as WorkerClient;
    const ch = new ChatSteering(withReceipts(client), "chat-1", 1, nullLogger(), new AbortController(), { now: () => clock });
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

  it("retries a failed GET before idle and delivers the follow-up", async () => {
    let gets = 0;
    const row = inp("follow_up", "after GET retry");
    const client = withReceipts({ getInputs: async () => {
      if (++gets === 1) throw Error("lost GET reply");
      return { inputs: gets === 2 ? [row] : [] };
    } } as unknown as WorkerClient);
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController());
    ch.start();
    assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "message", text: "after GET retry" });
    await ch.stop();
    assert.ok(gets >= 2);
  });

  it("still services the idle deadline after a GET failure", async () => {
    let clock = 0;
    const client = { getInputs: async () => { clock = 100; throw Error("temporary GET failure"); } } as unknown as WorkerClient;
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController(), { now: () => clock });
    const outcome = ch.awaitFollowUp(50);
    ch.start();
    assert.deepStrictEqual(await outcome, { kind: "idle" });
    await ch.stop();
  });

  it("holds one ACK batch through reply loss, routes every row in ID order, and drains on stop", async () => {
    const rows = [{ id: 8, kind: "follow_up", body: "eight" }, { id: 7, kind: "follow_up", body: "seven" }] as UserInput[];
    let gets = 0;
    let acks = 0;
    let applies = 0;
    const client = {
      getInputs: async () => { gets++; return { receipts: true, inputs: rows }; },
      ackInputs: async (_run: string, ids: number[], generation: number) => {
        assert.deepStrictEqual(ids, [8, 7]);
        assert.strictEqual(generation, 4);
        if (++acks === 1) throw Error("lost ACK reply");
        return { inputs: rows, active: true };
      },
      applyInputs: async (_run: string, ids: number[]) => { applies++; assert.deepStrictEqual(ids, [8, 7]); return { inputs: rows, active: true }; },
    } as unknown as WorkerClient;
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController(), {}, 4);
    ch.start();
    assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "message", text: "seven" });
    assert.strictEqual(gets, 1);
    assert.strictEqual(applies, 0, "waiter was serviced before applied began");
    assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "message", text: "eight" });
    await ch.stop();
    assert.strictEqual(acks, 2);
    assert.strictEqual(applies, 1);
    assert.strictEqual(gets, 1);
  });

  it("delivers the follow-up before an applied reply is lost, then retries its held IDs", async () => {
    const row = inp("follow_up", "delivered once");
    let gets = 0;
    let applies = 0;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const client = {
      getInputs: async () => { gets++; return { receipts: true, inputs: [row] }; },
      ackInputs: async () => ({ inputs: [row], active: true }),
      applyInputs: async () => {
        if (++applies === 1) { await gate; throw Error("lost applied reply"); }
        return { inputs: [row], active: true };
      },
    } as unknown as WorkerClient;
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "message", text: "delivered once" });
      assert.strictEqual(applies, 0);
      const stopped = ch.stop();
      release();
      await stopped;
      assert.strictEqual(applies, 2);
      assert.strictEqual(gets, 1);
    } finally {
      release();
      await ch.stop();
    }
  });

  it("retries a lost applied reply without GET or rerouting, even after cancel and stop", async () => {
    const rows = [inp("follow_up", "first"), inp("cancel")];
    const cancel = new AbortController();
    let gets = 0;
    let applies = 0;
    const client = {
      getInputs: async () => { gets++; return { receipts: true, inputs: rows }; },
      ackInputs: async () => ({ inputs: rows, active: true }),
      applyInputs: async () => { if (++applies === 1) throw Error("lost applied reply"); return { inputs: rows, active: true }; },
    } as unknown as WorkerClient;
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), cancel);
    ch.start();
    assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "ended" });
    assert.strictEqual(cancel.signal.aborted, true);
    await ch.stop();
    assert.strictEqual(applies, 2);
    assert.strictEqual(gets, 1);
  });

  it("keeps idle parked while a known follow-up ACK is uncertain", async () => {
    let clock = 0;
    const row = inp("follow_up", "raced");
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    let ackStarted = false;
    const client = {
      getInputs: async () => { clock = 100; return { receipts: true, inputs: [row] }; },
      ackInputs: async () => { ackStarted = true; await gate; return { inputs: [row], active: true }; },
      applyInputs: async () => ({ inputs: [row], active: true }),
    } as unknown as WorkerClient;
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController(), { now: () => clock });
    const outcome = ch.awaitFollowUp(50);
    ch.start();
    try {
      while (!ackStarted) await tick(1);
      let settled = false;
      void outcome.then(() => { settled = true; });
      await tick(2);
      assert.strictEqual(settled, false);
      release();
      assert.deepStrictEqual(await outcome, { kind: "message", text: "raced" });
    } finally {
      release();
      await ch.stop();
    }
  });

  it("fails the chat visibly when stop gives up a routed follow-up's APPLIED, so it does not complete", async () => {
    const row = inp("follow_up", "unapplied");
    let applies = 0;
    const client = {
      getInputs: async () => ({ receipts: true, inputs: [row] }),
      ackInputs: async () => ({ inputs: [row], active: true }),
      applyInputs: async () => { applies++; throw new RequestError("POST", "/inputs/applied", 503, "unavailable"); },
    } as unknown as WorkerClient;
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "message", text: "unapplied" });
      await ch.stop();
      assert.ok(applies >= 1);
      assert.strictEqual(ch.unconfirmedInput(), "could not confirm your message was applied; please resend it");
      assert.strictEqual(ch.claimLost(), false, "nothing requeues a chat, so the loss must be visible, not silent");
    } finally {
      await ch.stop();
    }
  });

  it("fails the chat visibly when a routed follow-up's APPLIED fails 30 times on the active claim", async () => {
    const row = inp("follow_up", "never confirmed");
    let applies = 0;
    const client = {
      getInputs: async () => ({ receipts: true, inputs: [row] }),
      ackInputs: async () => ({ inputs: [row], active: true }),
      applyInputs: async () => { applies++; throw new RequestError("POST", "/inputs/applied", 503, "unavailable"); },
    } as unknown as WorkerClient;
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "message", text: "never confirmed" });
      for (let i = 0; i < 500 && !ch.unconfirmedInput(); i++) await tick(2);
      assert.strictEqual(ch.unconfirmedInput(), "could not confirm your message was applied; please resend it");
      assert.strictEqual(ch.claimLost(), false);
      assert.strictEqual(applies, 30);
    } finally {
      await ch.stop();
    }
  });

  it("marks the claim lost when an APPLIED retry reports the claim inactive", async () => {
    const row = inp("follow_up", "applied then released");
    let applies = 0;
    const client = {
      getInputs: async () => ({ receipts: true, inputs: [row] }),
      ackInputs: async () => ({ inputs: [row], active: true }),
      applyInputs: async () => {
        // The first reply is lost after the commit; the retry sees the claim already released.
        if (++applies === 1) throw Error("lost applied reply");
        return { inputs: [row], active: false, reason: "released" };
      },
    } as unknown as WorkerClient;
    const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "message", text: "applied then released" });
      for (let i = 0; i < 200 && applies < 2; i++) await tick(2);
      await ch.stop();
      assert.strictEqual(ch.claimLost(), true);
    } finally {
      await ch.stop();
    }
  });

  it("ends an inactive ACK and a definitive applied 409 without another GET", async () => {
    for (const staleAt of ["ack", "applied"]) {
      const row = inp("follow_up", "stale");
      let gets = 0;
      let applies = 0;
      const client = {
        getInputs: async () => { gets++; return { receipts: true, inputs: [row] }; },
        ackInputs: async () => ({ inputs: [row], active: staleAt !== "ack" }),
        applyInputs: async () => { applies++; throw new RequestError("POST", "/inputs/applied", 409, JSON.stringify({ error: "fenced", reason: "stale" })); },
      } as unknown as WorkerClient;
      const ch = new ChatSteering(client, "chat-1", 1, nullLogger(), new AbortController());
      ch.start();
      await ch.stop();
      assert.strictEqual(ch.claimLost(), true);
      assert.strictEqual(gets, 1);
      assert.strictEqual(applies, staleAt === "ack" ? 0 : 1);
    }
  });
});

// Issue #1660: an operator follow-up is also a persistent RUN CONSTRAINT. The channel records it
// on receipt (it is already consumed server-side by then), independently of the lead's FIFO
// delivery, and the Agent guard attaches every recorded constraint to each later dispatch.
describe("SteeringChannel operator constraints (issue #1660)", () => {
  it("records follow-ups on receipt, in order, and keeps them after the lead's pull", async () => {
    const { ch } = makeChannel([[inp("follow_up", "  first  "), inp("follow_up", "   "), inp("revise_plan", "not a constraint")], [inp("follow_up", "second")]]);
    assert.deepStrictEqual(ch.operatorConstraints(), [], "nothing before any input is routed");
    ch.start();
    try {
      await tick();
      assert.deepStrictEqual(ch.operatorConstraints(), ["first", "second"], "trimmed, blanks and other kinds skipped");
      // The lead's delivery is unchanged and does not consume the constraint record.
      assert.strictEqual(ch.pullFollowUp(), "first");
      assert.strictEqual(ch.pullFollowUp(), "second");
      assert.deepStrictEqual(ch.operatorConstraints(), ["first", "second"]);
    } finally {
      await ch.stop();
    }
  });

  it("returns a copy: a caller cannot rewrite the run's constraints", async () => {
    const { ch } = makeChannel([[inp("follow_up", "keep")]]);
    ch.start();
    try {
      await tick();
      (ch.operatorConstraints() as string[]).push("injected");
      assert.deepStrictEqual(ch.operatorConstraints(), ["keep"]);
    } finally {
      await ch.stop();
    }
  });

  it("acceptance: a follow-up received after run start reaches a later dispatch and no earlier one", async () => {
    const queue: UserInput[][] = [];
    const client = { getInputs: async () => ({ inputs: queue.shift() ?? [] }) } as unknown as WorkerClient;
    const ch = new SteeringChannel(withReceipts(client), "run-1", 1, nullLogger(), new AbortController());
    const hook = buildAgentGuardHook(["reviewer", "auditor"], nullLogger(), () => ch.operatorConstraints());
    const dispatch = async (subagent_type: string): Promise<string> => {
      const out = (await hook({
        session_id: "s",
        transcript_path: "/t",
        cwd: "/w",
        hook_event_name: "PreToolUse",
        tool_name: NESTED_AGENT_TOOL,
        tool_input: { subagent_type, prompt: `${subagent_type} task` },
        tool_use_id: "tu",
      } as HookInput)) as { hookSpecificOutput?: { updatedInput?: { prompt?: string } } };
      return out.hookSpecificOutput?.updatedInput?.prompt ?? "";
    };
    const rule = "never execute a string containing kill; screen strings only";

    ch.start();
    try {
      await tick();
      const early = await dispatch("reviewer");
      assert.strictEqual(early, "reviewer task", "dispatch before the follow-up is unchanged");

      queue.push([inp("follow_up", rule)]);
      await tick();
      const late = await dispatch("auditor");
      assert.ok(late.startsWith("auditor task\n\n"));
      assert.ok(late.includes(rule), "dispatch after the follow-up carries it");
      // A later dispatch still carries it after the lead consumed its own copy.
      assert.strictEqual(ch.pullFollowUp(), rule);
      assert.ok((await dispatch("reviewer")).includes(rule), "persistent for the rest of the run");
    } finally {
      await ch.stop();
    }
  });
});

// Issue #1660: a re-claim starts a fresh channel, so the worker seeds it with the follow-ups
// earlier claims consumed (GET /follow-ups) before the first dispatch.
describe("SteeringChannel.seedOperatorConstraints (issue #1660)", () => {
  const withId = (id: number, kind: UserInput["kind"], body: string | null): UserInput => ({ id, kind, body });

  it("seeds consumed follow-ups ahead of live ones, follow_up only, blanks skipped, in server (id) order", async () => {
    const { ch } = makeChannel([[withId(20, "follow_up", "live")]]);
    ch.seedOperatorConstraints([
      withId(4, "follow_up", "earlier"),
      withId(9, "follow_up", " later "),
      withId(6, "revise_plan", "not a constraint"),
      withId(7, "follow_up", "   "),
      withId(8, "follow_up", null),
    ]);
    assert.deepStrictEqual(ch.operatorConstraints(), ["earlier", "later"]);
    ch.start();
    try {
      await tick();
      assert.deepStrictEqual(ch.operatorConstraints(), ["earlier", "later", "live"]);
      // Seeded constraints are NOT re-delivered to the lead: its FIFO holds only the live one.
      assert.strictEqual(ch.pullFollowUp(), "live");
      assert.strictEqual(ch.pullFollowUp(), undefined);
    } finally {
      await ch.stop();
    }
  });

  it("de-duplicates by input id between the seed and the live drain", async () => {
    const { ch } = makeChannel([[withId(5, "follow_up", "same row")]]);
    ch.seedOperatorConstraints([withId(5, "follow_up", "same row"), withId(5, "follow_up", "same row")]);
    ch.start();
    try {
      await tick();
      assert.deepStrictEqual(ch.operatorConstraints(), ["same row"]);
      assert.strictEqual(ch.pullFollowUp(), "same row", "the lead still gets the live delivery");
    } finally {
      await ch.stop();
    }
  });
});

// Issue #1660: a claim whose earlier constraints could not be reloaded must not dispatch
// subagents without them: the channel reports them unavailable (null) until the run is retried.
describe("SteeringChannel.markOperatorConstraintsUnavailable (issue #1660)", () => {
  it("reports null, even after follow-ups arrive live", async () => {
    const { ch } = makeChannel([[inp("follow_up", "live")]]);
    ch.markOperatorConstraintsUnavailable();
    assert.strictEqual(ch.operatorConstraints(), null);
    ch.start();
    try {
      await tick();
      assert.strictEqual(ch.operatorConstraints(), null);
      assert.strictEqual(ch.pullFollowUp(), "live", "the lead's delivery is unaffected");
    } finally {
      await ch.stop();
    }
  });
});

describe("input receipts", () => {
  const until = async (ready: () => boolean): Promise<void> => {
    for (let i = 0; i < 100 && !ready(); i++) await tick(2);
    assert.ok(ready(), "receipt operation reached");
  };
  it("retries one ACK batch before a newer GET and routes IDs in order", async () => {
    const rows: UserInput[] = [
      { id: 8, kind: "follow_up", body: "eight" },
      { id: 7, kind: "follow_up", body: "seven" },
    ];
    let gets = 0;
    let acks = 0;
    let applied = 0;
    const client = {
      getInputs: async () => { gets++; return { receipts: true, inputs: rows }; },
      ackInputs: async (_run: string, ids: number[]) => {
        acks++;
        assert.deepStrictEqual(ids, [8, 7]);
        if (acks === 1) throw Error("lost ACK reply");
        return { inputs: rows, active: true };
      },
      applyInputs: async () => { applied++; return { inputs: rows, active: true }; },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      await until(() => applied > 0);
      assert.strictEqual(ch.pullFollowUp(), "seven");
      assert.strictEqual(ch.pullFollowUp(), "eight");
      assert.strictEqual(ch.pullFollowUp(), undefined);
      assert.ok(acks >= 2);
      assert.ok(applied >= 1);
      assert.ok(gets >= 1);
    } finally {
      await ch.stop();
    }
  });

  it("retries an uncertain applied receipt through stop", async () => {
    const row: UserInput = { id: 7, kind: "follow_up", body: "one" };
    let attempts = 0;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const client = {
      getInputs: async () => ({ receipts: true, inputs: [row] }),
      ackInputs: async () => ({ inputs: [row], active: true }),
      applyInputs: async () => {
        attempts++;
        if (attempts === 1) { await gate; throw Error("lost apply reply"); }
        return { inputs: [row], active: true };
      },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    const stopped = ch.stop();
    release();
    await stopped;
    assert.ok(attempts >= 2);
  });

  it("routes the full ACK in ID order and services a waiter before apply", async () => {
    const rows: UserInput[] = [
      { id: 8, kind: "follow_up", body: "eight" },
      { id: 7, kind: "follow_up", body: "seven" },
    ];
    let applyCalls = 0;
    let releaseApply!: () => void;
    const applyGate = new Promise<void>((resolve) => { releaseApply = resolve; });
    const client = {
      getInputs: async () => ({ receipts: true, inputs: rows }),
      ackInputs: async () => ({ inputs: rows, active: true }),
      applyInputs: async () => { applyCalls++; await applyGate; return { inputs: rows, active: true }; },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "followup", body: "seven" });
      assert.strictEqual(applyCalls, 0, "the waiter is serviced before apply begins");
      assert.strictEqual(ch.pullFollowUp(), "eight");
      assert.strictEqual(ch.pullFollowUp(), undefined);
    } finally {
      releaseApply();
      await ch.stop();
    }
  });

  it("retries a lost apply reply with the same IDs and never routes twice", async () => {
    const rows: UserInput[] = [{ id: 7, kind: "follow_up", body: "seven" }];
    let gets = 0;
    let acks = 0;
    const appliedIds: number[][] = [];
    const client = {
      getInputs: async () => { gets++; return { receipts: true, inputs: gets === 1 ? rows : [] }; },
      ackInputs: async () => { acks++; return { inputs: rows, active: true }; },
      applyInputs: async (_run: string, ids: number[]) => {
        appliedIds.push([...ids]);
        if (appliedIds.length === 1) throw Error("lost apply reply");
        return { inputs: rows, active: true };
      },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      await until(() => appliedIds.length >= 1);
      await ch.awaitReceiptSettlement();
      assert.deepStrictEqual(appliedIds, [[7], [7]]);
      assert.strictEqual(acks, 1);
      assert.strictEqual(gets, 1, "no GET precedes settlement");
      assert.strictEqual(ch.pullFollowUp(), "seven");
      assert.strictEqual(ch.pullFollowUp(), undefined);
    } finally {
      await ch.stop();
    }
  });

  it("keeps idle parked when GET holds a follow-up and ACK is uncertain", async () => {
    let clock = 0;
    const row: UserInput = { id: 7, kind: "follow_up", body: "seven" };
    let ackCalls = 0;
    let releaseAck!: () => void;
    const ackGate = new Promise<void>((resolve) => { releaseAck = resolve; });
    const client = {
      getInputs: async () => { clock = 100; return { receipts: true, inputs: [row] }; },
      ackInputs: async () => {
        ackCalls++;
        if (ackCalls === 1) { await ackGate; throw Error("lost ACK reply"); }
        return { inputs: [row], active: true };
      },
      applyInputs: async () => ({ inputs: [row], active: true }),
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController(), { now: () => clock });
    const outcome = ch.awaitFollowUp(50);
    ch.start();
    try {
      await tick();
      let settled = false;
      void outcome.then(() => { settled = true; });
      assert.strictEqual(settled, false);
      releaseAck();
      assert.deepStrictEqual(await outcome, { kind: "followup", body: "seven" });
    } finally {
      releaseAck();
      await ch.stop();
    }
  });

  it("routes nothing from a switch_pending ACK, keeps polling and does not cancel the flight", async () => {
    // A pending credential switch: the rows belong to the next claim, and the switch signal on a
    // later GET releases this one. A local cancel would report the run cancelled mid-switch.
    const row: UserInput = { id: 7, kind: "cancel", body: null };
    let gets = 0;
    let applied = 0;
    let acked = 0;
    const cancel = new AbortController();
    const client = {
      getInputs: async () => { gets++; return { receipts: true, inputs: gets === 1 ? [row] : [] }; },
      ackInputs: async () => { acked++; return { inputs: [row], active: false, reason: "switch_pending" }; },
      applyInputs: async () => { applied++; return { inputs: [row], active: true }; },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel);
    ch.start();
    try {
      await until(() => gets >= 3);
      await ch.awaitReceiptSettlement();
      assert.strictEqual(acked, 1);
      assert.strictEqual(applied, 0);
      assert.strictEqual(cancel.signal.aborted, false, "the inactive batch's cancel was not routed");
      assert.strictEqual(ch.isCancelled(), false);
    } finally {
      await ch.stop();
    }
  });

  it("rejects a malformed ACK row and retries the held IDs without routing it", async () => {
    const row: UserInput = { id: 7, kind: "follow_up", body: "seven" };
    let acks = 0;
    let gets = 0;
    let getsAtRetry = -1;
    const client = {
      getInputs: async () => { gets++; return { receipts: true, inputs: gets === 1 ? [row] : [] }; },
      ackInputs: async (_run: string, ids: number[]) => {
        assert.deepStrictEqual(ids, [7]);
        acks++;
        if (acks === 1) return { inputs: [{ id: 7, kind: "follow_up", body: 42 }], active: true };
        getsAtRetry = gets;
        return { inputs: [row], active: true };
      },
      applyInputs: async () => ({ inputs: [row], active: true }),
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      await until(() => acks === 2);
      assert.strictEqual(getsAtRetry, 1, "the retry reused the held ids without a newer GET");
      await tick();
      assert.strictEqual(ch.pullFollowUp(), "seven", "routed once, from the well-formed receipt");
      assert.strictEqual(ch.pullFollowUp(), undefined);
    } finally {
      await ch.stop();
    }
  });

  it("drops the batch on a switch_pending applied 409 and never routes a replay of it again", async () => {
    // A `now` pause fires the interrupt on every route, so a second route of the replayed row is
    // observable (follow-ups are also de-duplicated by the lead queue, so they would not show it).
    const row: UserInput = { id: 7, kind: "pause", body: "now" };
    let attempts = 0;
    let gets = 0;
    let interrupts = 0;
    const client = {
      // The claim is fenced and then reinstated (a failed credential switch): the same unapplied
      // row comes back on a later GET of this claim.
      getInputs: async () => { gets++; return { receipts: true, inputs: gets === 1 || gets === 3 ? [row] : [] }; },
      ackInputs: async () => ({ inputs: [row], active: true }),
      applyInputs: async () => {
        attempts++;
        // A switch is pending, then fails: the batch is dropped and later replayed on this claim.
        if (attempts === 1) throw new RequestError("POST", "/inputs/applied", 409, JSON.stringify({ error: "conflict", reason: "switch_pending" }));
        return { inputs: [row], active: true };
      },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.onPauseNow(() => { interrupts++; });
    ch.start();
    try {
      await until(() => attempts >= 2);
      await ch.awaitReceiptSettlement();
      assert.strictEqual(interrupts, 1, "the replayed row was applied, not rerouted");
    } finally {
      await ch.stop();
    }
  });

  it("ends the flight on a released or stale claim, from a 200 or a 409 receipt", async () => {
    for (const reason of ["released", "stale"]) {
      for (const via of ["200", "409"]) {
        const row: UserInput = { id: 7, kind: "approve_plan", body: null };
        let gets = 0;
        const cancel = new AbortController();
        const client = {
          getInputs: async () => { gets++; return { receipts: true, inputs: [row] }; },
          ackInputs: async () => {
            if (via === "409") throw new RequestError("POST", "/inputs/ack", 409, JSON.stringify({ error: "fenced", reason }));
            return { inputs: [row], active: false, reason };
          },
          applyInputs: async () => { throw Error("a fenced batch must not apply"); },
        } as unknown as WorkerClient;
        const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel);
        const gate = ch.awaitVerdict();
        ch.start();
        try {
          await assert.rejects(gate, (err: Error) => err.name === "ClaimFencedSignal", `${reason} via ${via}`);
          assert.strictEqual((cancel.signal.reason as Error).name, "ClaimFencedSignal");
          assert.strictEqual(ch.isCancelled(), false, "the fenced batch's inputs were not routed");
          await assert.rejects(ch.awaitFollowUp(100_000), (err: Error) => err.name === "ClaimFencedSignal", "a later park fails at once");
          const settled = gets;
          await tick(20);
          assert.strictEqual(gets, settled, "the old flight stopped polling");
        } finally {
          await ch.stop();
        }
      }
    }
  });

  it("keeps a switch_pending flight parked and delivers the batch once the claim is active again", async () => {
    const row: UserInput = { id: 7, kind: "approve_plan", body: null };
    let acks = 0;
    const cancel = new AbortController();
    const client = {
      getInputs: async () => ({ receipts: true, inputs: [row] }),
      ackInputs: async () => {
        acks++;
        // The first two ACKs race a pending credential switch, which then fails: the claim is active again.
        if (acks <= 2) throw new RequestError("POST", "/inputs/ack", 409, JSON.stringify({ error: "fenced", reason: "switch_pending" }));
        return { inputs: [row], active: true };
      },
      applyInputs: async () => ({ inputs: [row], active: true }),
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel);
    ch.start();
    try {
      assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "approve", selection: { status: "absent" } });
      assert.ok(acks >= 3);
      assert.strictEqual(cancel.signal.aborted, false);
    } finally {
      await ch.stop();
    }
  });

  it("fails a waiting state report and later parks when APPLIED keeps failing on the active claim", async () => {
    for (const kind of ["approve_plan", "follow_up"] as const) {
      const row: UserInput = { id: 7, kind, body: kind === "follow_up" ? "seven" : null };
      let applies = 0;
      const client = {
        getInputs: async () => ({ receipts: true, inputs: [row] }),
        ackInputs: async () => ({ inputs: [row], active: true }),
        applyInputs: async () => { applies++; throw new RequestError("POST", "/inputs/applied", 503, "unavailable"); },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
      ch.start();
      try {
        // The routed input reaches the executor, which then reports the resume.
        if (kind === "approve_plan") assert.strictEqual((await ch.awaitVerdict()).kind, "approve");
        else assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "followup", body: "seven" });
        await assert.rejects(ch.awaitReceiptSettlement(), (err: Error) => err.name === "InputReceiptError",
          "the guarded report never goes out as if the input were applied");
        assert.strictEqual(applies, 30, "bounded on the active claim");
        await assert.rejects(ch.awaitReceiptSettlement(), (err: Error) => err.name === "InputReceiptError");
        await assert.rejects(ch.awaitAnswer("q1"), (err: Error) => err.name === "InputReceiptError");
      } finally {
        await ch.stop();
      }
    }
  });

  it("retries an untyped 404 on ACK or APPLIED (an api pod without the route) instead of ending the flight", async () => {
    for (const at of ["ack", "applied"] as const) {
      const row: UserInput = { id: 7, kind: "approve_plan", body: null };
      let acks = 0;
      let applies = 0;
      const notFound = (path: string): RequestError => new RequestError("POST", path, 404, "404 page not found");
      const cancel = new AbortController();
      const client = {
        getInputs: async () => ({ receipts: true, inputs: [row] }),
        ackInputs: async () => {
          if (at === "ack" && ++acks <= 2) throw notFound("/inputs/ack");
          return { inputs: [row], active: true };
        },
        applyInputs: async () => {
          if (at === "applied" && ++applies <= 2) throw notFound("/inputs/applied");
          return { inputs: [row], active: true };
        },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel);
      ch.start();
      try {
        assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "approve", selection: { status: "absent" } }, at);
        await until(() => (at === "ack" ? acks : applies) >= 3);
        await ch.awaitReceiptSettlement();
        assert.strictEqual(ch.claimFence(), undefined, `${at}: an untyped 404 is not a fence`);
        assert.strictEqual(cancel.signal.aborted, false);
      } finally {
        await ch.stop();
      }
    }
  });

  it("gives up an applied receipt by elapsed time when the request hangs or is slow", async () => {
    for (const mode of ["hung", "slow"] as const) {
      const row: UserInput = { id: 7, kind: "follow_up", body: "seven" };
      let applies = 0;
      const client = {
        getInputs: async () => ({ receipts: true, inputs: [row] }),
        ackInputs: async () => ({ inputs: [row], active: true }),
        applyInputs: async () => {
          applies++;
          // A hung request answers only at the client's HTTP timeout (here 500 ms, past the
          // 100 ms deadline); a slow one fails after 20 ms each time.
          await tick(mode === "hung" ? 500 : 20);
          throw new RequestError("POST", "/inputs/applied", 504, "timeout");
        },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController(), { receiptDeadlineMs: 100 });
      ch.start();
      try {
        assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "followup", body: "seven" });
        const started = Date.now();
        await assert.rejects(ch.awaitReceiptSettlement(), (err: Error) => err.name === "InputReceiptError", mode);
        assert.ok(Date.now() - started < 2_000, `${mode}: bounded by the deadline, not the attempt count`);
        assert.ok(applies < 30, `${mode}: gave up after ${applies} attempts`);
      } finally {
        await ch.stop();
      }
    }
  });

  it("rejects a settlement wait on a cancel or the report's own abort, so the report is not sent", async () => {
    for (const via of ["cancel", "report"] as const) {
      const row: UserInput = { id: 7, kind: "approve_plan", body: null };
      const cancel = new AbortController();
      const client = {
        getInputs: async () => ({ receipts: true, inputs: [row] }),
        ackInputs: async () => ({ inputs: [row], active: true }),
        // Slow enough that only the abort can end the wait first.
        applyInputs: async () => { await tick(300); return { inputs: [row], active: true }; },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel);
      ch.start();
      try {
        await ch.awaitVerdict();
        await tick();
        const report = new AbortController();
        const waiting = ch.awaitReceiptSettlement(report.signal);
        const started = Date.now();
        (via === "cancel" ? cancel : report).abort();
        await assert.rejects(waiting, (err: Error) => err.name === "ReceiptWaitInterrupted", via);
        assert.ok(Date.now() - started < 200, `${via}: the abort ended the wait before the applied reply`);
        if (via === "report") {
          // The report's own signal, already aborted: refused at once while the receipt is uncertain.
          await assert.rejects(ch.awaitReceiptSettlement(report.signal), (err: Error) => err.name === "ReceiptWaitInterrupted");
        } else {
          // The shared controller aborts once and stays aborted; that stale abort says nothing
          // about a later report, which waits for the applied receipt and then goes out.
          await ch.awaitReceiptSettlement();
        }
      } finally {
        await ch.stop();
      }
    }
  });

  it("ends the flight on a typed 404 (reason stale) from ACK or APPLIED", async () => {
    for (const at of ["ack", "applied"] as const) {
      const row: UserInput = { id: 7, kind: at === "ack" ? "approve_plan" : "follow_up", body: at === "ack" ? null : "seven" };
      const typed = (path: string): RequestError =>
        new RequestError("POST", path, 404, JSON.stringify({ error: "run not found", reason: "stale" }));
      const cancel = new AbortController();
      const client = {
        getInputs: async () => ({ receipts: true, inputs: [row] }),
        ackInputs: async () => {
          if (at === "ack") throw typed("/inputs/ack");
          return { inputs: [row], active: true };
        },
        applyInputs: async () => { throw typed("/inputs/applied"); },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel);
      ch.start();
      try {
        await until(() => ch.claimFence() !== undefined);
        assert.strictEqual(ch.claimFence(), "stale", at);
        assert.strictEqual((cancel.signal.reason as Error).name, "ClaimFencedSignal");
      } finally {
        await ch.stop();
      }
    }
  });

  it("gives up an ACK-phase batch at its attempt or time bound, so newer GETs flow", async () => {
    for (const bound of ["attempts", "deadline"] as const) {
      const row: UserInput = { id: 7, kind: "approve_plan", body: null };
      let gets = 0;
      let acks = 0;
      const client = {
        getInputs: async () => { gets++; return { receipts: true, inputs: [row] }; },
        ackInputs: async () => {
          acks++;
          // Attempts: fail fast. Deadline: each attempt takes 20 ms, past a 50 ms deadline.
          if (bound === "deadline") await tick(20);
          throw new RequestError("POST", "/inputs/ack", 503, "unavailable");
        },
        applyInputs: async () => { throw Error("an unACKed batch must not apply"); },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController(), {
        receiptDeadlineMs: bound === "deadline" ? 50 : 600_000,
      });
      ch.start();
      try {
        await until(() => gets >= 2);
        assert.ok(bound === "attempts" ? acks >= 30 : acks < 30, `${bound}: ${acks} ACK attempts before the next GET`);
        assert.strictEqual(ch.claimFence(), undefined, "an ACK bound drops the batch, it does not fence");
      } finally {
        await ch.stop();
      }
    }
  });

  it("after a declined `now` park, a later follow-up's report still waits for its APPLIED", async () => {
    // A `now` pause aborts the shared controller once; the park is declined and the turn restarts
    // with the controller still aborted. A follow-up routed afterwards must still hold its report.
    const pause: UserInput = { id: 7, kind: "pause", body: "now" };
    const followUp: UserInput = { id: 8, kind: "follow_up", body: "eight" };
    let gets = 0;
    let appliedFollowUp = false;
    const cancel = new AbortController();
    const client = {
      getInputs: async () => { gets++; return { receipts: true, inputs: gets === 1 ? [pause] : gets === 2 ? [followUp] : [] }; },
      ackInputs: async (_run: string, ids: number[]) => ({ inputs: ids[0] === 7 ? [pause] : [followUp], active: true }),
      applyInputs: async (_run: string, ids: number[]) => {
        if (ids[0] === 8) { await tick(150); appliedFollowUp = true; }
        return { inputs: ids[0] === 7 ? [pause] : [followUp], active: true };
      },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel);
    ch.start();
    try {
      await until(() => cancel.signal.aborted);
      assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "followup", body: "eight" });
      await ch.awaitReceiptSettlement();
      assert.strictEqual(appliedFollowUp, true, "the running report waited for the follow-up's APPLIED");
    } finally {
      await ch.stop();
    }
  });

  it("after a given-up credential switch, a later follow-up's report still waits for its APPLIED", async () => {
    // The switch trips the shared controller once; the give-up is confirmed and the flight continues
    // in place with the controller still aborted. A follow-up routed afterwards must hold its report.
    const followUp: UserInput = { id: 8, kind: "follow_up", body: "eight" };
    let gets = 0;
    let appliedFollowUp = false;
    const cancel = new AbortController();
    const client = {
      getInputs: async () => {
        gets++;
        if (gets === 1) return { receipts: true, inputs: [], credentialSwitch: { generation: 3 } };
        return { receipts: true, inputs: gets === 3 ? [followUp] : [] };
      },
      ackInputs: async () => ({ inputs: [followUp], active: true }),
      applyInputs: async () => { await tick(150); appliedFollowUp = true; return { inputs: [followUp], active: true }; },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel, { claimGeneration: 3 });
    ch.start();
    try {
      await until(() => cancel.signal.aborted);
      assert.strictEqual((cancel.signal.reason as Error).name, "CredentialSwitchSignal");
      ch.rearmCredentialSwitch(); // the give-up's stamp-clear was confirmed; the flight continues
      assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "followup", body: "eight" });
      await ch.awaitReceiptSettlement();
      assert.strictEqual(appliedFollowUp, true, "the running report waited for the follow-up's APPLIED");
    } finally {
      await ch.stop();
    }
  });

  it("rejects a waiting report when APPLIED is refused with a definitive 4xx after routing", async () => {
    for (const refusal of [
      new RequestError("POST", "/inputs/applied", 409, JSON.stringify({ error: "conflict", reason: "" })),
      new RequestError("POST", "/inputs/applied", 400, "invalid input ids"),
    ]) {
      const row: UserInput = { id: 7, kind: "follow_up", body: "seven" };
      let release!: () => void;
      const gate = new Promise<void>((resolve) => { release = resolve; });
      const client = {
        getInputs: async () => ({ receipts: true, inputs: [row] }),
        ackInputs: async () => ({ inputs: [row], active: true }),
        applyInputs: async () => { await gate; throw refusal; },
      } as unknown as WorkerClient;
      const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
      ch.start();
      try {
        assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "followup", body: "seven" });
        await tick();
        const waiting = ch.awaitReceiptSettlement();
        release();
        await assert.rejects(waiting, (err: Error) => err.name === "InputReceiptError", `status ${refusal.status}`);
      } finally {
        release();
        await ch.stop();
      }
    }
  });

  it("routes a GET reply without the receipts marker at once, with no ACK or APPLIED (an older api pod)", async () => {
    const rows: UserInput[] = [{ id: 7, kind: "approve_plan", body: null }, { id: 8, kind: "follow_up", body: "eight" }];
    let gets = 0;
    const client = {
      getInputs: async () => { gets++; return { inputs: gets === 1 ? rows : [] }; },
      ackInputs: async () => { throw Error("a consume-on-read reply must not be ACKed"); },
      applyInputs: async () => { throw Error("a consume-on-read reply must not be applied"); },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "approve", selection: { status: "absent" } });
      assert.strictEqual(ch.pullFollowUp(), "eight");
      await ch.awaitReceiptSettlement();
    } finally {
      await ch.stop();
    }
  });

  it("takes the credential-switch path on a switch_pending APPLIED: the waiting report rejects with the switch signal", async () => {
    const row: UserInput = { id: 7, kind: "follow_up", body: "seven" };
    let gets = 0;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const client = {
      getInputs: async () => { gets++; return { receipts: true, inputs: gets === 1 ? [row] : [] }; },
      ackInputs: async () => ({ inputs: [row], active: true }),
      applyInputs: async () => {
        await gate;
        throw new RequestError("POST", "/inputs/applied", 409, JSON.stringify({ error: "conflict", reason: "switch_pending" }));
      },
    } as unknown as WorkerClient;
    const cancel = new AbortController();
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), cancel, { claimGeneration: 4 });
    ch.start();
    try {
      assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "followup", body: "seven" });
      await tick();
      const waiting = ch.awaitReceiptSettlement();
      release();
      await assert.rejects(waiting, (err: Error) => err.name === "CredentialSwitchSignal",
        "the resume report must not go out unapplied, and must enter the switch path, not a failure");
      assert.strictEqual(ch.pendingCredentialSwitch(), 4, "the switch is tripped for this claim");
      assert.strictEqual((cancel.signal.reason as Error).name, "CredentialSwitchSignal");
      await ch.awaitReceiptSettlement(); // not sticky: the switch's own release report still goes out
      const seen = gets;
      await until(() => gets > seen);
      assert.strictEqual(ch.claimFence(), undefined);
    } finally {
      release();
      await ch.stop();
    }
  });

  it("gives up a routed receipt after a bounded number of applied attempts at stop", async () => {
    const row: UserInput = { id: 7, kind: "approve_plan", body: null };
    let attempts = 0;
    const client = {
      getInputs: async () => ({ receipts: true, inputs: [row] }),
      ackInputs: async () => ({ inputs: [row], active: true }),
      applyInputs: async () => { attempts++; throw Error("api down"); },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    await until(() => attempts >= 1);
    await ch.stop();
    const atStop = attempts;
    await ch.awaitReceiptSettlement();
    await tick();
    assert.ok(atStop <= 1 + 3 + 1, `bounded applied attempts, got ${atStop}`);
    assert.strictEqual(attempts, atStop, "no attempt after stop returned");
  });

  it("holds a state report behind a routed batch until its applied receipt lands", async () => {
    const row: UserInput = { id: 7, kind: "follow_up", body: "seven" };
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    let appliedDone = false;
    const client = {
      getInputs: async () => ({ receipts: true, inputs: [row] }),
      ackInputs: async () => ({ inputs: [row], active: true }),
      applyInputs: async () => { await gate; appliedDone = true; return { inputs: [row], active: true }; },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController());
    ch.start();
    try {
      assert.deepStrictEqual(await ch.awaitFollowUp(100000), { kind: "followup", body: "seven" });
      let reported = false;
      const report = ch.awaitReceiptSettlement().then(() => { reported = true; });
      await tick();
      assert.strictEqual(reported, false, "the wake report waits for the applied receipt");
      release();
      await report;
      assert.strictEqual(appliedDone, true);
    } finally {
      release();
      await ch.stop();
    }
  });
});
