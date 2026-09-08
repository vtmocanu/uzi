import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import type {
  HarnessContext,
  HarnessContextHook,
  HarnessEvent,
} from "../src/harness.js";

// issue #1197 (D-RC2a): the positively-observed empty-turn evidence the reducer folds
// onto ReducedTurnResult (numTurns, sawModelActivity). These pin the coercion + activity
// rules directly on the reducer, so the empty-turn detector (isPositivelyEmpty in
// sdk-executor) has a trustworthy input: MISSING/garbage metrics stay undefined (never
// 0), and any frame with items/usage is model activity.

const noContext: HarnessContextHook = {
  request() {},
  async get(): Promise<HarnessContext | undefined> {
    return undefined;
  },
};

/** A terminal (turn_finished) event carrying an arbitrary raw `num_turns` wire value. */
function terminal(numTurns: unknown): HarnessEvent {
  return {
    kind: "turn_finished",
    terminal: {
      outcome: "success",
      subtype: "success",
      errors: [],
      metrics: {
        cost: { kind: "unreported" },
        wire: { num_turns: numTurns, duration_ms: 1, total_cost_usd: 0 },
      },
    },
  };
}

/** A terminal whose metrics carry NO wire capsule at all (an older/leaner provider). */
function terminalNoWire(): HarnessEvent {
  return {
    kind: "turn_finished",
    terminal: {
      outcome: "success",
      subtype: "success",
      errors: [],
      metrics: { cost: { kind: "unreported" } },
    },
  };
}

/** A lead assistant frame carrying one text item (model output). */
function leadTextFrame(text: string): HarnessEvent {
  return {
    kind: "frame",
    origin: { kind: "main" },
    attribution: { agent: "lead" },
    items: [{ kind: "text", text }],
  };
}

async function reduce(events: HarnessEvent[]) {
  const r = new RunTurnReducerImpl(noContext);
  r.beginTurn();
  for (const e of events) await r.accept(e);
  return r.finish({ kind: "exhausted" }).result;
}

describe("RunTurnReducer — empty-turn evidence (issue #1197 D-RC2a)", () => {
  it("a zero-turn terminal with no assistant frames yields numTurns===0 && !sawModelActivity", async () => {
    const result = await reduce([{ kind: "initialized", model: "m" }, terminal(0)]);
    assert.strictEqual(result.numTurns, 0, "num_turns:0 coerces to 0");
    assert.ok(
      !result.sawModelActivity,
      "no assistant/tool frame or usage ⇒ no model activity",
    );
  });

  it("a normal turn (an assistant frame + num_turns>=1) yields activity and the reported count", async () => {
    const result = await reduce([
      { kind: "initialized", model: "m" },
      leadTextFrame("working on it"),
      terminal(3),
    ]);
    assert.strictEqual(result.numTurns, 3);
    assert.strictEqual(result.sawModelActivity, true, "an items-bearing frame is activity");
  });

  it("a terminal with num_turns ABSENT yields numTurns===undefined (NOT 0)", async () => {
    const result = await reduce([{ kind: "initialized", model: "m" }, terminal(undefined)]);
    assert.strictEqual(
      result.numTurns,
      undefined,
      "missing metrics MUST stay undefined — never defaulted to 0",
    );
  });

  it("a terminal with a GARBAGE num_turns (non-number) yields numTurns===undefined", async () => {
    const result = await reduce([
      { kind: "initialized", model: "m" },
      terminal("3" as unknown),
    ]);
    assert.strictEqual(result.numTurns, undefined, "a string is not a finite number");
  });

  it("a terminal with a non-finite num_turns (NaN) yields numTurns===undefined", async () => {
    const result = await reduce([{ kind: "initialized", model: "m" }, terminal(Number.NaN)]);
    assert.strictEqual(result.numTurns, undefined, "NaN is not finite");
  });

  it("a terminal with NO wire capsule yields numTurns===undefined", async () => {
    const result = await reduce([{ kind: "initialized", model: "m" }, terminalNoWire()]);
    assert.strictEqual(result.numTurns, undefined);
  });

  it("resets the evidence between turns (a prior turn's activity does not leak)", async () => {
    const r = new RunTurnReducerImpl(noContext);
    // Turn 1: activity + a real count.
    r.beginTurn();
    await r.accept(leadTextFrame("turn 1"));
    await r.accept(terminal(2));
    const first = r.finish({ kind: "exhausted" }).result;
    assert.strictEqual(first.sawModelActivity, true);
    assert.strictEqual(first.numTurns, 2);
    // Turn 2: a genuinely empty turn. The prior turn's evidence must NOT carry over.
    r.beginTurn();
    await r.accept(terminal(0));
    const second = r.finish({ kind: "exhausted" }).result;
    assert.strictEqual(second.numTurns, 0);
    assert.ok(!second.sawModelActivity, "sawModelActivity is per-turn, not latched across turns");
  });
});
