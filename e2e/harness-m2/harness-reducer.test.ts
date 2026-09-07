// M2 neutral-harness-seam differential test: the run-lane turn reducer.
//
// PRD #1146 (#1106 M2) milestone m4 validation. Characterizes RunTurnReducerImpl
// (agent/src/harness-reducer.ts) as a PURE, in-process fold over hand-built neutral
// HarnessEvents — no SDK, no credentials, no network. Every fixture below is a
// neutral literal; a token-shaped id here is DUMMY text constructed at runtime.

import test from "node:test";
import assert from "node:assert/strict";

import { RunTurnReducerImpl } from "../../agent/src/harness-reducer.js";
import type {
  HarnessContext,
  HarnessContextHook,
  HarnessEvent,
  HarnessItem,
  HarnessTerminal,
  HarnessUsage,
  TurnSignals,
} from "../../agent/src/harness.js";

// --- fixtures -------------------------------------------------------------

/** A context hook whose request()/get() are observable for the assertions. */
class FakeContextHook implements HarnessContextHook {
  requests = 0;
  constructor(private readonly ctx: HarnessContext | undefined) {}
  request(): void {
    this.requests += 1;
  }
  async get(): Promise<HarnessContext | undefined> {
    return this.ctx;
  }
}

function terminal(
  outcome: "success" | "failed",
  subtype: string,
): HarnessTerminal {
  return { outcome, subtype, errors: [], metrics: { cost: { kind: "unreported" } } };
}

function callUsage(wireUsage: unknown): HarnessUsage {
  return { basis: "call", tokens: {}, wire: { usage: wireUsage } };
}

const SIGNAL_ITEM: HarnessItem = {
  kind: "tool",
  phase: "started",
  id: "sig-1",
  name: "submit_plan",
  input: {},
  signal: "submit_plan",
};

function mainFrame(
  items: readonly HarnessItem[],
  extra: Partial<Extract<HarnessEvent, { kind: "frame" }>> = {},
): HarnessEvent {
  return {
    kind: "frame",
    origin: { kind: "main" },
    attribution: { agent: "lead" },
    items,
    ...extra,
  };
}

// --- session id -----------------------------------------------------------

test("firstSessionId fires once per run; empty never advances; per-turn is last-truthy", async () => {
  const reducer = new RunTurnReducerImpl(new FakeContextHook(undefined));

  // Turn 1.
  reducer.beginTurn();
  const rEmpty = await reducer.accept({ kind: "activity", sessionId: "" });
  assert.equal(rEmpty.firstSessionId, undefined, "empty string must not advance the run latch");

  const rFirst = await reducer.accept({ kind: "activity", sessionId: "sess-turn1-a" });
  assert.equal(rFirst.firstSessionId, "sess-turn1-a", "first truthy id reported once");

  const rSecond = await reducer.accept({ kind: "activity", sessionId: "sess-turn1-b" });
  assert.equal(rSecond.firstSessionId, undefined, "latch does not re-fire within the run");

  const c1 = reducer.finish({ kind: "exhausted" });
  assert.equal(c1.result.sessionId, "sess-turn1-b", "per-turn result carries the LAST truthy id");

  // Turn 2, SAME instance: the run-level latch persists, per-turn id is fresh.
  reducer.beginTurn();
  const rTurn2 = await reducer.accept({ kind: "activity", sessionId: "sess-turn2" });
  assert.equal(rTurn2.firstSessionId, undefined, "firstSessionId is a run-level latch, not per-turn");

  const c2 = reducer.finish({ kind: "exhausted" });
  assert.equal(c2.result.sessionId, "sess-turn2", "turn 2 result carries its own last truthy id");
});

// --- group-before-filter --------------------------------------------------

test("a started-tool item carrying a signal is dropped; text, non-signal tools and results survive", async () => {
  const reducer = new RunTurnReducerImpl(new FakeContextHook(undefined));
  reducer.beginTurn();

  const items: HarnessItem[] = [
    SIGNAL_ITEM,
    { kind: "text", text: "hello" },
    { kind: "tool", phase: "started", id: "t2", name: "Bash", input: { cmd: "ls" } },
    { kind: "tool", phase: "finished", id: "t2", output: "ok", isError: false },
  ];
  const r = await reducer.accept(mainFrame(items));

  assert.equal(r.messages.length, 3, "the signal tool_use is dropped; the other three survive");
  assert.deepEqual(
    r.messages.map((m) => m.kind),
    ["text", "tool_use", "tool_result"],
  );
  // The dropped item's identity (name submit_plan) must not appear in any survivor.
  assert.ok(
    r.messages.every((m) => m.payload["name"] !== "submit_plan"),
    "no surviving message carries the dropped signal tool's name",
  );
});

// --- usage + model co-gate ------------------------------------------------

test("usage and model attach to the FIRST surviving item only", async () => {
  const reducer = new RunTurnReducerImpl(new FakeContextHook(undefined));
  reducer.beginTurn();

  const r = await reducer.accept(
    mainFrame([SIGNAL_ITEM, { kind: "text", text: "a" }, { kind: "text", text: "b" }], {
      usage: callUsage({ input_tokens: 7 }),
      model: "claude-dummy",
    }),
  );

  assert.equal(r.messages.length, 2);
  assert.deepEqual(r.messages[0].payload["usage"], { input_tokens: 7 });
  assert.equal(r.messages[0].payload["model"], "claude-dummy");
  assert.equal("usage" in r.messages[1].payload, false, "second survivor gets no usage");
  assert.equal("model" in r.messages[1].payload, false, "second survivor gets no model");
});

test("when every item is filtered, neither usage nor model attaches and nothing is emitted", async () => {
  const hook = new FakeContextHook({ used: 1, window: 2, pct: 0.5 });
  const reducer = new RunTurnReducerImpl(hook);
  reducer.beginTurn();

  const r = await reducer.accept(
    mainFrame([SIGNAL_ITEM], { usage: callUsage({ input_tokens: 9 }), model: "m" }),
  );

  assert.equal(r.messages.length, 0, "the only item was a dropped signal tool");
  assert.equal(hook.requests, 0, "no surviving usage-bearing lead item, so no context read is requested");
});

// --- signal fold, origin gated --------------------------------------------

test("signals fold only under origin main: last-wins, latch, and concat", async () => {
  const reducer = new RunTurnReducerImpl(new FakeContextHook(undefined));
  reducer.beginTurn();

  const first: Readonly<Partial<TurnSignals>> = {
    plan: "plan-v1",
    milestones: [{ id: "m1", title: "one" }],
    progress: { completed: ["m1"], in_progress: [] },
    done: true,
    checkpoint: true,
    reportOnly: true,
    questions: [{ question: "q1?", header: "Q1" }],
  };
  const r1 = await reducer.accept(mainFrame([{ kind: "text", text: "x" }], { signals: first }));
  // progress is delivered immediately off the hot stream via the reduction.
  assert.deepEqual(r1.progress, { completed: ["m1"], in_progress: [] });

  const second: Readonly<Partial<TurnSignals>> = {
    plan: "plan-v2",
    questions: [{ question: "q2?", header: "Q2" }],
  };
  await reducer.accept(mainFrame([{ kind: "text", text: "y" }], { signals: second }));

  const { result } = reducer.finish({ kind: "exhausted" });
  assert.equal(result.plan, "plan-v2", "plan is last-wins");
  assert.equal(result.done, true, "done latches");
  assert.equal(result.checkpoint, true, "checkpoint latches");
  assert.equal(result.reportOnly, true, "reportOnly latches");
  assert.deepEqual(
    result.questions?.map((q) => q.question),
    ["q1?", "q2?"],
    "questions concatenate across frames",
  );
});

test("the same signals under origin subagent are NOT folded", async () => {
  const reducer = new RunTurnReducerImpl(new FakeContextHook(undefined));
  reducer.beginTurn();

  const signals: Readonly<Partial<TurnSignals>> = {
    plan: "should-not-apply",
    done: true,
    checkpoint: true,
    questions: [{ question: "q?", header: "Q" }],
  };
  await reducer.accept({
    kind: "frame",
    origin: { kind: "subagent", role: "coder" },
    attribution: { agent: "coder" },
    items: [{ kind: "text", text: "sub" }],
    signals,
  });

  const { result } = reducer.finish({ kind: "exhausted" });
  assert.equal(result.plan, undefined, "subagent-origin plan is ignored");
  assert.equal(result.done, false, "subagent-origin done does not latch");
  assert.equal(result.checkpoint, undefined);
  assert.equal(result.questions, undefined);
});

// --- clean EOF ------------------------------------------------------------

test("finish(exhausted) returns the accumulated result without throwing", () => {
  const reducer = new RunTurnReducerImpl(new FakeContextHook(undefined));
  reducer.beginTurn();

  const completion = reducer.finish({ kind: "exhausted" });
  assert.equal(completion.end.kind, "exhausted");
  assert.equal(completion.result.done, false);
  assert.equal(completion.result.finalText, undefined);
});

// --- beginTurn reset ------------------------------------------------------

test("beginTurn clears per-turn state after a normal finish", async () => {
  const reducer = new RunTurnReducerImpl(new FakeContextHook(undefined));

  // Turn 1 accumulates lead text, a signal and a session id.
  reducer.beginTurn();
  await reducer.accept({ kind: "activity", sessionId: "sess-1" });
  await reducer.accept(
    mainFrame([{ kind: "text", text: "lead-said-this" }], { signals: { done: true } }),
  );
  const c1 = reducer.finish({ kind: "exhausted" });
  assert.equal(c1.result.done, true);
  assert.equal(c1.result.finalText, "lead-said-this");
  assert.equal(c1.result.sessionId, "sess-1");

  // Turn 2 starts clean WITHOUT depending on finish having reset anything.
  reducer.beginTurn();
  const c2 = reducer.finish({ kind: "exhausted" });
  assert.equal(c2.result.done, false, "done did not leak");
  assert.equal(c2.result.finalText, undefined, "leadText did not leak");
  assert.equal(c2.result.sessionId, undefined, "turnSessionId did not leak");
});

test("beginTurn clears per-turn state even when the prior turn SKIPPED finish (throw path)", async () => {
  const reducer = new RunTurnReducerImpl(new FakeContextHook(undefined));

  // Turn A accumulates state, then the turn "throws": finish is never called.
  reducer.beginTurn();
  await reducer.accept({ kind: "activity", sessionId: "sess-A" });
  await reducer.accept(
    mainFrame([{ kind: "text", text: "leaked?" }], { signals: { done: true, checkpoint: true } }),
  );
  // (no finish() here — simulates the turn ending by throw)

  // Turn B: beginTurn is the single owner of the reset and must clear everything.
  reducer.beginTurn();
  const cB = reducer.finish({ kind: "exhausted" });
  assert.equal(cB.result.done, false);
  assert.equal(cB.result.checkpoint, undefined);
  assert.equal(cB.result.finalText, undefined);
  assert.equal(cB.result.sessionId, undefined);
});

// --- context hook ---------------------------------------------------------

test("context request fires once on the first usage-bearing lead item and attaches on the terminal", async () => {
  const ctx: HarnessContext = { used: 10, window: 100, pct: 0.1 };
  const hook = new FakeContextHook(ctx);
  const reducer = new RunTurnReducerImpl(hook);
  reducer.beginTurn();

  // First usage-bearing lead item requests the read exactly once.
  await reducer.accept(
    mainFrame([{ kind: "text", text: "lead" }], { usage: callUsage({ input_tokens: 1 }) }),
  );
  // A second usage-bearing frame must not re-request.
  await reducer.accept(
    mainFrame([{ kind: "text", text: "more" }], { usage: callUsage({ input_tokens: 2 }) }),
  );
  assert.equal(hook.requests, 1, "the context read is requested once per turn");

  const r = await reducer.accept({ kind: "turn_finished", terminal: terminal("success", "success") });
  const term = r.messages[r.messages.length - 1];
  assert.deepEqual(term.payload["context"], { used: 10, window: 100, pct: 0.1 });
});

test("an undefined context read omits context and never fails the turn", async () => {
  const hook = new FakeContextHook(undefined);
  const reducer = new RunTurnReducerImpl(hook);
  reducer.beginTurn();

  await reducer.accept(
    mainFrame([{ kind: "text", text: "lead" }], { usage: callUsage({ input_tokens: 1 }) }),
  );
  const r = await reducer.accept({ kind: "turn_finished", terminal: terminal("success", "success") });
  const term = r.messages[r.messages.length - 1];
  assert.equal("context" in term.payload, false, "absent context is omitted, not attached as undefined");
});

test("a usage-bearing SUBAGENT item does not request context, so the terminal carries none", async () => {
  const hook = new FakeContextHook({ used: 5, window: 50, pct: 0.1 });
  const reducer = new RunTurnReducerImpl(hook);
  reducer.beginTurn();

  await reducer.accept({
    kind: "frame",
    origin: { kind: "subagent", role: "coder" },
    attribution: { agent: "coder" },
    items: [{ kind: "text", text: "sub" }],
    usage: callUsage({ input_tokens: 3 }),
    model: "m",
  });
  assert.equal(hook.requests, 0, "usage attaches but the read is lead-gated");

  const r = await reducer.accept({ kind: "turn_finished", terminal: terminal("success", "success") });
  const term = r.messages[r.messages.length - 1];
  assert.equal("context" in term.payload, false);
});
