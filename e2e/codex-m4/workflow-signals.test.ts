// PRD #1287 C3 (D7 layer U) — workflow signals + callback identity, through the REAL broker and
// the REAL ExecutionRegistry.
//
// A workflow signal (submit_plan / signal_done / …) may be latched ONLY by the root/main origin;
// a child or unknown origin can never move the run's workflow. A callback identity is idempotent:
// a replay of a settled (thread,turn,call) returns the cached terminal with NO second effect, and
// a same-id reuse with a CHANGED payload is a forgery the registry poisons. These are direct
// malformed/replayed identity injections — a UNIT/broker layer (D3), never a forged upstream event
// observed through the CLI.
//
// Negative-effect oracles (D4): the origin denials carry no scanned signal output (no reducer
// input to fold); the replay runs the command spawn seam exactly once (the recording spy is the
// oracle, not the denial text).

import { before, describe, it } from "node:test";
import assert from "node:assert/strict";

import { loadUModules, makeUBroker, leadGrants, type UModules } from "./harness-u.js";
import { recordEvidence } from "./evidence.js";
import { CODEX_U_SIGNAL_ORIGIN_TITLE, CODEX_U_SIGNAL_REPLAY_TITLE } from "./titles-c3.js";

let mods: UModules;
before(async () => {
  mods = await loadUModules();
});

describe("codex U workflow signals + callback identity (real broker / registry)", () => {
  it(CODEX_U_SIGNAL_ORIGIN_TITLE, async () => {
    const h = makeUBroker(mods, { grants: leadGrants() });
    const id = (callId: string): { threadId: string; turnId: string; callId: string } => ({ threadId: "th-1", turnId: "tn-1", callId });

    // CHILD and UNKNOWN origins can never latch a signal.
    const childPlan = await h.broker.handleToolCall(id("s1"), "submit_plan", { plan_md: "child plan" }, "child");
    assert.equal(childPlan.ok, false, "a child submit_plan is denied");
    if (!childPlan.ok) assert.equal(childPlan.code, "signal_root_only");
    const childDone = await h.broker.handleToolCall(id("s2"), "signal_done", {}, "child");
    assert.equal(childDone.ok, false, "a child signal_done is denied");
    const unknownPlan = await h.broker.handleToolCall(id("s3"), "submit_plan", { plan_md: "x" }, "unknown");
    assert.equal(unknownPlan.ok, false, "an unknown-origin submit_plan is denied");
    const unknownDone = await h.broker.handleToolCall(id("s4"), "signal_done", {}, "unknown");
    assert.equal(unknownDone.ok, false, "an unknown-origin signal_done is denied");

    // NEGATIVE ORACLE: no denied signal produced a scanned payload (nothing for a reducer to fold),
    // and no signal ever touched the command/file surfaces.
    for (const denied of [childPlan, childDone, unknownPlan, unknownDone]) {
      assert.equal("output" in denied, false, "a denied signal carries no scanned output");
    }
    assert.equal(h.spawn.calls.length, 0, "signals never reach the command spawn seam");
    assert.equal(h.fileop.ops.length, 0, "signals never reach the fileop client");

    // A VALID ROOT signal latches exactly once (the registry admits it), producing the scanned payload.
    const rootPlan = await h.broker.handleToolCall(id("s5"), "submit_plan", { plan_md: "the real plan" }, "root");
    assert.equal(rootPlan.ok, true, "a valid root submit_plan is latched");
    if (rootPlan.ok) assert.deepEqual(rootPlan.output, { plan: "the real plan" }, "the scanned plan is surfaced for the reducer");
    const rootDone = await h.broker.handleToolCall(id("s6"), "signal_done", {}, "root");
    assert.equal(rootDone.ok, true, "a valid root signal_done is latched");
    if (rootDone.ok) assert.deepEqual(rootDone.output, { done: true }, "the scanned done is surfaced for the reducer");

    recordEvidence(CODEX_U_SIGNAL_ORIGIN_TITLE, "pass");
  });

  it(CODEX_U_SIGNAL_REPLAY_TITLE, async () => {
    // Replay: the SAME (thread,turn,call) with the SAME payload after it settled returns the cached
    // terminal and runs NO second effect. A fresh broker/registry so the poison of the changed-
    // reuse case below cannot bleed in.
    const replay = makeUBroker(mods, { grants: leadGrants() });
    const rtFixed = { threadId: "th-1", turnId: "tn-1", callId: "c-replay" };
    const first = await replay.broker.handleToolCall(rtFixed, "Bash", { command: "echo once" }, "root");
    assert.equal(first.ok, true, "the first callback ran");
    assert.equal(replay.spawn.calls.length, 1, "the effect ran once");
    const second = await replay.broker.handleToolCall(rtFixed, "Bash", { command: "echo once" }, "root");
    assert.equal(second.ok, true, "the replay returns a terminal");
    if (second.ok) assert.deepEqual(second.output, { replay: true }, "the replay is the cached terminal, not a re-execution");
    assert.equal(replay.spawn.calls.length, 1, "NEGATIVE ORACLE: the replay ran NO second effect (spawn count unchanged)");

    // A valid ROOT signal also latches exactly once — a replay of a settled signal returns the
    // cached terminal, never a second latch.
    const sigBroker = makeUBroker(mods, { grants: leadGrants() });
    const sigId = { threadId: "th-1", turnId: "tn-1", callId: "c-sig" };
    const s1 = await sigBroker.broker.handleToolCall(sigId, "signal_done", {}, "root");
    assert.equal(s1.ok, true, "the root signal latched");
    const s2 = await sigBroker.broker.handleToolCall(sigId, "signal_done", {}, "root");
    assert.equal(s2.ok, true, "the replayed signal returns a terminal");
    if (s2.ok) assert.deepEqual(s2.output, { replay: true }, "the signal latched exactly once (replay is cached)");

    // Changed-reuse: the SAME call id with a CHANGED payload is a forgery the registry poisons.
    const forge = makeUBroker(mods, { grants: leadGrants() });
    const forgeId = { threadId: "th-1", turnId: "tn-1", callId: "c-forge" };
    const original = await forge.broker.handleToolCall(forgeId, "Bash", { command: "echo a" }, "root");
    assert.equal(original.ok, true, "the original callback ran");
    assert.equal(forge.spawn.calls.length, 1, "the original effect ran once");
    const changed = await forge.broker.handleToolCall(forgeId, "Bash", { command: "echo b" }, "root");
    assert.equal(changed.ok, false, "a same-id reuse with a changed payload is denied");
    if (!changed.ok) assert.equal(changed.code, "changed_reuse", "the changed reuse poisons the epoch");
    assert.equal(forge.spawn.calls.length, 1, "NEGATIVE ORACLE: the forged reuse ran NO second effect");

    recordEvidence(CODEX_U_SIGNAL_REPLAY_TITLE, "pass");
  });
});
