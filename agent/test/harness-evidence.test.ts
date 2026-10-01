import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { evidencesModelProcessing, type HarnessEvent } from "../src/harness.js";

// Issue #1800: the predicate that decides when an owner follow-up counts as read by the lead
// model. Only a main-thread, assistant-authored frame with output counts.

const items = [{ kind: "text", text: "hi" }] as unknown as Extract<HarnessEvent, { kind: "frame" }>["items"];

function frame(over: Partial<Extract<HarnessEvent, { kind: "frame" }>>): HarnessEvent {
  return { kind: "frame", origin: { kind: "main" }, attribution: {}, items, ...over } as HarnessEvent;
}

describe("evidencesModelProcessing (issue #1800)", () => {
  it("counts a main-thread assistant-authored frame", () => {
    assert.equal(evidencesModelProcessing(frame({ assistantAuthored: true })), true);
  });
  it("counts an assistant-authored usage-only or signal-only frame", () => {
    assert.equal(
      evidencesModelProcessing(
        frame({ assistantAuthored: true, items: [], usage: { basis: "call", tokens: {}, wire: {} } as never }),
      ),
      true,
    );
    assert.equal(
      evidencesModelProcessing(frame({ assistantAuthored: true, items: [], signals: { done: true } as never })),
      true,
    );
  });
  it("rejects a subagent-origin frame even when assistant-authored", () => {
    assert.equal(evidencesModelProcessing(frame({ origin: { kind: "subagent" }, assistantAuthored: true })), false);
  });
  it("rejects an unmarked main frame", () => {
    assert.equal(evidencesModelProcessing(frame({})), false);
  });
  it("rejects an assistant-authored frame with no output", () => {
    assert.equal(evidencesModelProcessing(frame({ assistantAuthored: true, items: [] })), false);
  });
  it("rejects a synthetic-model frame", () => {
    assert.equal(evidencesModelProcessing(frame({ assistantAuthored: true, model: "<synthetic>" })), false);
  });
  it("counts only a modelInitiated activity, and never a terminal or init", () => {
    assert.equal(evidencesModelProcessing({ kind: "activity", modelInitiated: true } as HarnessEvent), true);
    assert.equal(evidencesModelProcessing({ kind: "activity" } as HarnessEvent), false);
    assert.equal(evidencesModelProcessing({ kind: "initialized" } as HarnessEvent), false);
  });
});
