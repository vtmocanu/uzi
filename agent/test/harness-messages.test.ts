// issue #1562 (ADR-1562): the session-cumulative markers on the pure projection core.
//
// projectResult gains an OPTIONAL `usageBasis` that appends `usage_basis` to the payload
// (both success and failed branches) ONLY when set, and projectInit gains an OPTIONAL
// `freshSession` that appends `fresh_session: true` ONLY when true. These pin that an
// UNMARKED frame is byte-identical (key-for-key, in order) to the pre-#1562 shape, and a
// MARKED frame appends exactly the one key, last.

import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { projectResult, projectInit } from "../src/harness-messages.js";

const wire = {
  usage: { input_tokens: 10 },
  modelUsage: { "claude-fable-5": { costUSD: 0.5 } },
  num_turns: 2,
  duration_ms: 1000,
  total_cost_usd: 0.5,
};

describe("projectResult usage_basis marker (issue #1562)", () => {
  it("omits usage_basis on an unmarked success frame (byte-identical key order)", () => {
    const em = projectResult({ outcome: "success", subtype: "success", errors: [], wire });
    assert.deepStrictEqual(em.payload, {
      event: "result",
      subtype: "success",
      num_turns: 2,
      duration_ms: 1000,
      total_cost_usd: 0.5,
      usage: wire.usage,
      modelUsage: wire.modelUsage,
    });
    assert.ok(!("usage_basis" in em.payload));
  });

  it("appends usage_basis LAST on a marked success frame", () => {
    const em = projectResult({
      outcome: "success",
      subtype: "success",
      errors: [],
      usageBasis: "session_cumulative",
      wire,
    });
    assert.equal(em.payload["usage_basis"], "session_cumulative");
    assert.deepStrictEqual(Object.keys(em.payload), [
      "event",
      "subtype",
      "num_turns",
      "duration_ms",
      "total_cost_usd",
      "usage",
      "modelUsage",
      "usage_basis",
    ]);
  });

  it("omits usage_basis on an unmarked failed frame", () => {
    const em = projectResult({
      outcome: "failed",
      subtype: "error_max_turns",
      errors: ["cap"],
      wire,
    });
    assert.equal(em.kind, "error");
    assert.ok(!("usage_basis" in em.payload));
    assert.deepStrictEqual(Object.keys(em.payload), [
      "event",
      "subtype",
      "errors",
      "usage",
      "modelUsage",
      "total_cost_usd",
      "num_turns",
      "duration_ms",
    ]);
  });

  it("appends usage_basis LAST on a marked failed frame", () => {
    const em = projectResult({
      outcome: "failed",
      subtype: "error_max_turns",
      errors: ["cap"],
      usageBasis: "session_cumulative",
      wire,
    });
    assert.equal(em.kind, "error");
    assert.equal(em.payload["usage_basis"], "session_cumulative");
    assert.equal(Object.keys(em.payload).at(-1), "usage_basis");
  });
});

describe("projectInit fresh_session marker (issue #1562)", () => {
  it("omits fresh_session when freshSession is undefined (byte-identical shape)", () => {
    const em = projectInit("claude-fable-5");
    assert.deepStrictEqual(em, {
      kind: "status",
      agent: "lead",
      payload: { event: "init", model: "claude-fable-5" },
    });
  });

  it("omits fresh_session when freshSession is explicitly false", () => {
    const em = projectInit("claude-fable-5", false);
    assert.ok(!("fresh_session" in em.payload));
  });

  it("appends fresh_session: true when freshSession is true", () => {
    const em = projectInit("claude-fable-5", true);
    assert.equal(em.payload["fresh_session"], true);
    assert.deepStrictEqual(Object.keys(em.payload), ["event", "model", "fresh_session"]);
  });
});
