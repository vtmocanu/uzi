// PRD #1287 C2 (D5) — the run-lane reducer/projection is UNCHANGED (discriminating pins).
//
// ADDITIVE ONLY. Sits beside harness-reducer-evidence.test.ts (which pins the #1197 empty-turn
// evidence fields) and does NOT rewrite it. These cases pin DIFFERENT current behavior of
// RunTurnReducerImpl (harness-reducer.ts): the projection/fallback for an empty or garbage turn,
// the origin-gated signal fold (a subagent-origin frame's signals are never folded, a main-origin
// frame's are), and the lead-vs-subagent text attribution. D5 requires existing event/reducer
// behavior to stay unchanged; this proves it discriminates the way it does today. Pure, no SDK.

import test from "node:test";
import assert from "node:assert/strict";

import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import type { HarnessContext, HarnessContextHook, HarnessEvent } from "../src/harness.js";

const noContext: HarnessContextHook = {
  request() {},
  async get(): Promise<HarnessContext | undefined> {
    return undefined;
  },
};

/** Reduce a turn, returning the final result AND every projected message across the turn. */
async function reduceCollect(events: HarnessEvent[]) {
  const r = new RunTurnReducerImpl(noContext);
  r.beginTurn();
  const messages = [];
  for (const e of events) {
    const reduction = await r.accept(e);
    messages.push(...reduction.messages);
  }
  const { result } = r.finish({ kind: "exhausted" });
  return { result, messages };
}

const successTerminal = (): HarnessEvent => ({
  kind: "turn_finished",
  terminal: {
    outcome: "success",
    subtype: "success",
    errors: [],
    metrics: { cost: { kind: "unreported" }, wire: { num_turns: 1, duration_ms: 1, total_cost_usd: 0 } },
  },
});

test("claude C2 reducer: an empty turn projects only its terminal and leaves done=false, finalText undefined", async () => {
  const { result, messages } = await reduceCollect([{ kind: "initialized", model: "m" }, successTerminal()]);
  // Fallback is unchanged: no plan, not done, no accumulated lead text, no subagent activity.
  assert.equal(result.done, false, "an empty turn is not done");
  assert.equal(result.plan, undefined, "no plan latched");
  assert.equal(result.finalText, undefined, "no lead text ⇒ finalText undefined");
  assert.equal(result.subagentActivity, undefined, "no subagent frame ⇒ no subagent activity");
  // Exactly the init status + the terminal result status are projected (nothing else).
  assert.equal(messages.length, 2);
  assert.equal(messages[0]!.payload["event"], "init");
  assert.equal(messages[1]!.payload["event"], "result");
  assert.equal(messages[1]!.kind, "status");
});

test("claude C2 reducer: a garbage/activity-only turn adds no messages and preserves the fallback result", async () => {
  const garbageFrame: HarnessEvent = {
    kind: "frame",
    origin: { kind: "main" },
    attribution: {},
    items: [], // no items, no signals
  };
  const { result, messages } = await reduceCollect([
    { kind: "activity" },
    garbageFrame,
    { kind: "activity" },
  ]);
  assert.deepEqual(messages, [], "activity + an empty frame project no messages");
  assert.equal(result.done, false);
  assert.equal(result.plan, undefined);
  assert.equal(result.finalText, undefined);
});

test("claude C2 reducer: signals from a subagent-origin frame are not folded", async () => {
  const subagentSignalFrame: HarnessEvent = {
    kind: "frame",
    origin: { kind: "subagent", role: "coder" },
    attribution: { agent: "coder" },
    items: [],
    signals: { plan: "child plan", done: true },
  };
  const { result } = await reduceCollect([subagentSignalFrame, successTerminal()]);
  // The reducer folds signals ONLY when event.origin.kind === "main".
  assert.equal(result.done, false, "a subagent-origin done is not latched");
  assert.equal(result.plan, undefined, "a subagent-origin plan is not latched");
});

test("claude C2 reducer: signals from a main-origin frame are folded once", async () => {
  const mainSignalFrame: HarnessEvent = {
    kind: "frame",
    origin: { kind: "main" },
    attribution: { agent: "lead" },
    items: [],
    signals: { plan: "root plan", done: true },
  };
  const { result } = await reduceCollect([mainSignalFrame, successTerminal()]);
  assert.equal(result.done, true, "a main-origin done is latched");
  assert.equal(result.plan, "root plan", "a main-origin plan is latched");
});

test("claude C2 reducer: lead text becomes finalText while subagent text only sets subagentActivity", async () => {
  const leadText: HarnessEvent = {
    kind: "frame",
    origin: { kind: "main" },
    attribution: { agent: "lead" },
    items: [{ kind: "text", text: "lead speaking" }],
  };
  const subagentText: HarnessEvent = {
    kind: "frame",
    origin: { kind: "subagent", role: "reviewer" },
    attribution: { agent: "reviewer" },
    items: [{ kind: "text", text: "reviewed unit A" }],
  };
  const { result, messages } = await reduceCollect([leadText, subagentText, successTerminal()]);
  assert.equal(result.finalText, "lead speaking", "only lead text accumulates into finalText");
  assert.equal(result.subagentActivity, true, "a subagent-attributed frame sets subagentActivity");
  // Both text frames are still projected (nothing dropped) plus the terminal.
  const texts = messages.filter((m) => m.kind === "text").map((m) => m.payload["text"]);
  assert.deepEqual(texts, ["lead speaking", "reviewed unit A"]);
});
