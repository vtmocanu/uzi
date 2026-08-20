import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { buildSignalMcpServer, isSignalToolName, REPORT_MD_MAX_LEN, scanSignals } from "../src/signals.js";

// The plan/done signals are observed from the SDK message stream, so a scripted
// tool_use proves them with no live session.

function toolUse(name: string, input: unknown): unknown {
  return { type: "assistant", session_id: "s", message: { content: [{ type: "tool_use", id: "t", name, input }] } };
}

/** Same as toolUse but tagged as a REAL subagent frame (PRD #43 M2): the SDK
 *  stamps subagent-produced frames with BOTH a top-level `subagent_type` label
 *  (sibling of type/message/session_id — sdk.d.ts:2762-2777, the same field
 *  sdk-messages.ts:28-30 attributes by) AND a non-null `parent_tool_use_id` (the
 *  spawning Agent tool_use id). The default carries both, like the wire does;
 *  pass `extra` to isolate a single discriminator. */
function subagentToolUse(
  name: string,
  input: unknown,
  extra: Record<string, unknown> = { subagent_type: "coder", parent_tool_use_id: "toolu_parent" },
): unknown {
  return {
    type: "assistant",
    session_id: "s",
    ...extra,
    message: { content: [{ type: "tool_use", id: "t", name, input }] },
  };
}

describe("scanSignals", () => {
  it("extracts plan_md from a submit_plan tool_use", () => {
    assert.deepStrictEqual(scanSignals(toolUse("mcp__uzi__submit_plan", { plan_md: "# Plan" })), { plan: "# Plan" });
  });

  it("treats a submit_plan with no body as a submitted (empty) plan, not 'no plan'", () => {
    const r = scanSignals(toolUse("mcp__uzi__submit_plan", {}));
    assert.strictEqual(r.plan, "");
  });

  it("detects signal_done", () => {
    assert.deepStrictEqual(scanSignals(toolUse("mcp__uzi__signal_done", {})), { done: true });
  });

  it("returns nothing for plain text, user frames, and unrelated tools", () => {
    assert.deepStrictEqual(scanSignals({ type: "assistant", message: { content: [{ type: "text", text: "hi" }] } }), {});
    assert.deepStrictEqual(scanSignals(toolUse("Bash", { command: "ls" })), {});
    assert.deepStrictEqual(scanSignals({ type: "user", message: { content: [] } }), {});
    assert.deepStrictEqual(scanSignals(undefined), {});
    assert.deepStrictEqual(scanSignals("garbage"), {});
  });

  it("captures both signals if a message carries them together", () => {
    const msg = {
      type: "assistant",
      message: {
        content: [
          { type: "tool_use", name: "mcp__uzi__submit_plan", input: { plan_md: "P" } },
          { type: "tool_use", name: "mcp__uzi__signal_done", input: {} },
        ],
      },
    };
    assert.deepStrictEqual(scanSignals(msg), { plan: "P", done: true });
  });

  // PRD #43 M2 / Decision 3: the workflow signals gate the run and end the loop —
  // only the lead's MAIN-THREAD frames may carry them. A subagent frame reaching
  // signal_done/submit_plan (prompt-injected, buggy, or via a future tool leak)
  // must NOT latch done or the plan, or the loop ends on a partial, unreviewed
  // tree. This worker-side scan is the LOAD-BEARING guarantee for that success
  // criterion: it holds no matter what the SDK does with the server-level
  // `mcp__uzi` disallowedTools denial (whether that denial wins over a custom
  // template's explicit `tools` allowlist is unproven from the SDK types).
  it("ignores signal_done from a real subagent frame (subagent_type + parent_tool_use_id)", () => {
    const r = scanSignals(subagentToolUse("mcp__uzi__signal_done", {}));
    assert.deepStrictEqual(r, {}, "a subagent must not be able to end the run");
    assert.notStrictEqual(r.done, true);
  });

  it("ignores submit_plan from a real subagent frame (subagent_type + parent_tool_use_id)", () => {
    const r = scanSignals(subagentToolUse("mcp__uzi__submit_plan", { plan_md: "# injected" }));
    assert.deepStrictEqual(r, {}, "a subagent must not be able to submit the plan");
    assert.strictEqual(r.plan, undefined);
  });

  it("ignores signals when tagged ONLY by subagent_type (no parent_tool_use_id)", () => {
    // Each discriminator alone is sufficient — a subagent frame that omitted its
    // parent id still can't latch a signal.
    const r = scanSignals(subagentToolUse("mcp__uzi__signal_done", {}, { subagent_type: "reviewer" }));
    assert.deepStrictEqual(r, {});
  });

  it("ignores signals when tagged ONLY by parent_tool_use_id (no subagent_type)", () => {
    // The other half: a subagent frame is also identifiable by its non-null
    // parent_tool_use_id (sdk.d.ts:2765, required string|null — null only on the
    // main thread), so a frame that dropped its subagent_type label still can't
    // latch a signal.
    const r = scanSignals(subagentToolUse("mcp__uzi__signal_done", {}, { parent_tool_use_id: "toolu_abc" }));
    assert.deepStrictEqual(r, {});
  });

  it("still honors signals on a main-thread frame (parent_tool_use_id: null, no subagent_type)", () => {
    // Guard against over-rejection: a real lead frame carries parent_tool_use_id:
    // null and no subagent_type, and its signals must still latch.
    const r = scanSignals(subagentToolUse("mcp__uzi__signal_done", {}, { parent_tool_use_id: null }));
    assert.deepStrictEqual(r, { done: true });
  });
});

describe("scanSignals prd_done_path (PRD #72 M4)", () => {
  const DONE = "mcp__uzi__signal_done";

  it("extracts a declared PRD path alongside done", () => {
    assert.deepStrictEqual(scanSignals(toolUse(DONE, { prd_done_path: "prds/done/72-x.md" })), {
      done: true,
      prdDonePath: "prds/done/72-x.md",
    });
  });

  it("omits the key entirely when signal_done carries no path", () => {
    const r = scanSignals(toolUse(DONE, {})) as Record<string, unknown>;
    assert.deepStrictEqual(r, { done: true });
    assert.ok(!("prdDonePath" in r), "absent, not undefined-valued");
  });

  it("forwards a path VERBATIM — the worker does not validate shape", () => {
    // Transport hygiene only. The grammar lives in api/internal/prdpath, once; a
    // second implementation here would drift silently in both directions, and
    // neither direction would ever be loud (the api is authoritative either way).
    const hostile = "prds/../../../etc/passwd";
    assert.strictEqual((scanSignals(toolUse(DONE, { prd_done_path: hostile })) as { prdDonePath?: string }).prdDonePath, hostile);
  });

  it("ignores a non-string path and still reports done", () => {
    // A malformed declaration must never throw and must never cost the run its
    // completion signal.
    for (const bad of [42, null, {}, ["prds/x.md"], true]) {
      const r = scanSignals(toolUse(DONE, { prd_done_path: bad })) as Record<string, unknown>;
      assert.strictEqual(r["done"], true, `done must survive input ${JSON.stringify(bad)}`);
      assert.ok(!("prdDonePath" in r), `non-string ${JSON.stringify(bad)} must not be captured`);
    }
  });

  it("IGNORES a subagent-borne declaration entirely (the main-thread guard covers the new field)", () => {
    // The single extraction point sits inside the SIGNAL_DONE branch, which is
    // only reached after isSubagentFrame has already returned {}. This field
    // drives a forge write against the run's issue, so a prompt-injected subagent
    // reaching it would re-open that threat model from inside the run.
    assert.deepStrictEqual(scanSignals(subagentToolUse(DONE, { prd_done_path: "prds/done/evil.md" })), {});
    // Both subagent markers independently, matching the existing done/plan cases.
    assert.deepStrictEqual(
      scanSignals(subagentToolUse(DONE, { prd_done_path: "prds/done/evil.md" }, { subagent_type: "coder" })),
      {},
    );
    assert.deepStrictEqual(
      scanSignals(subagentToolUse(DONE, { prd_done_path: "prds/done/evil.md" }, { parent_tool_use_id: "toolu_x" })),
      {},
    );
  });
});

describe("scanSignals milestones_completed on signal_done (PRD #265 M1)", () => {
  const DONE = "mcp__uzi__signal_done";

  it("extracts declared finished milestone ids alongside done", () => {
    assert.deepStrictEqual(scanSignals(toolUse(DONE, { milestones_completed: ["m1", "m2"] })), {
      done: true,
      milestonesCompleted: ["m1", "m2"],
    });
  });

  it("omits the key entirely when signal_done declares nothing", () => {
    const r = scanSignals(toolUse(DONE, {})) as Record<string, unknown>;
    assert.deepStrictEqual(r, { done: true });
    assert.ok(!("milestonesCompleted" in r), "absent, not undefined-valued");
  });

  it("defensively parses ids and still reports done on garbage", () => {
    // parseProgressIds coerces scalars, drops blanks/objects, dedupes. A garbage
    // list must never throw and must never cost the run its completion signal.
    const r = scanSignals(
      toolUse(DONE, { milestones_completed: ["m1", "m1", "", { x: 1 }, "m2"] }),
    ) as { done?: boolean; milestonesCompleted?: string[] };
    assert.strictEqual(r.done, true);
    assert.deepStrictEqual(r.milestonesCompleted, ["m1", "m2"]);
  });

  it("ignores a non-array declaration and still reports done", () => {
    for (const bad of [42, null, "m1", {}, true]) {
      const r = scanSignals(toolUse(DONE, { milestones_completed: bad })) as Record<string, unknown>;
      assert.strictEqual(r["done"], true, `done must survive input ${JSON.stringify(bad)}`);
      assert.ok(!("milestonesCompleted" in r), `non-array ${JSON.stringify(bad)} must not be captured`);
    }
  });

  it("IGNORES a subagent-borne declaration entirely (main-thread guard covers the new field)", () => {
    // Reconciliation moves the run's tracker, so a prompt-injected subagent must never
    // reach it — the same guarantee that keeps a subagent from latching done/prd_done_path.
    assert.deepStrictEqual(scanSignals(subagentToolUse(DONE, { milestones_completed: ["m9"] })), {});
    assert.deepStrictEqual(
      scanSignals(subagentToolUse(DONE, { milestones_completed: ["m9"] }, { subagent_type: "coder" })),
      {},
    );
    assert.deepStrictEqual(
      scanSignals(subagentToolUse(DONE, { milestones_completed: ["m9"] }, { parent_tool_use_id: "toolu_x" })),
      {},
    );
  });
});

describe("scanSignals report_only + summary on signal_done (issue #279)", () => {
  const DONE = "mcp__uzi__signal_done";

  it("extracts report_only + summary alongside done", () => {
    assert.deepStrictEqual(
      scanSignals(toolUse(DONE, { report_only: true, summary: "verified: no code change needed" })),
      { done: true, reportOnly: true, summary: "verified: no code change needed" },
    );
  });

  it("still scans a plain signal_done to exactly { done: true } (existing deepStrictEqual holds)", () => {
    const r = scanSignals(toolUse(DONE, {})) as Record<string, unknown>;
    assert.deepStrictEqual(r, { done: true });
    assert.ok(!("reportOnly" in r), "reportOnly absent, not undefined-valued");
    assert.ok(!("summary" in r), "summary absent, not undefined-valued");
  });

  it("clamps summary to REPORT_MD_MAX_LEN", () => {
    const big = "x".repeat(REPORT_MD_MAX_LEN + 5000);
    const r = scanSignals(toolUse(DONE, { summary: big })) as { done?: boolean; summary?: string };
    assert.strictEqual(r.done, true);
    assert.strictEqual(r.summary?.length, REPORT_MD_MAX_LEN);
  });

  it("captures summary without report_only, and report_only only when strictly true", () => {
    // summary alone (a normal completion may still carry a one-liner).
    const s = scanSignals(toolUse(DONE, { summary: "did the thing" })) as Record<string, unknown>;
    assert.deepStrictEqual(s, { done: true, summary: "did the thing" });
    // report_only must be strictly `true` — a truthy-but-not-true value is not report-only.
    for (const notTrue of [1, "true", {}, ["x"]]) {
      const r = scanSignals(toolUse(DONE, { report_only: notTrue })) as Record<string, unknown>;
      assert.strictEqual(r["done"], true, `done survives report_only=${JSON.stringify(notTrue)}`);
      assert.ok(!("reportOnly" in r), `report_only=${JSON.stringify(notTrue)} must not latch`);
    }
  });

  it("ignores a non-string summary and still reports done", () => {
    for (const bad of [42, null, {}, ["s"], true]) {
      const r = scanSignals(toolUse(DONE, { summary: bad })) as Record<string, unknown>;
      assert.strictEqual(r["done"], true, `done must survive summary ${JSON.stringify(bad)}`);
      assert.ok(!("summary" in r), `non-string summary ${JSON.stringify(bad)} must not be captured`);
    }
  });

  it("IGNORES a subagent-borne report_only/summary entirely (main-thread guard covers the new fields)", () => {
    // report_only drives a NO-MR terminal path, so a prompt-injected subagent must never
    // reach it — the same guarantee that keeps a subagent from latching done/prd_done_path.
    assert.deepStrictEqual(scanSignals(subagentToolUse(DONE, { report_only: true, summary: "x" })), {});
    assert.deepStrictEqual(
      scanSignals(subagentToolUse(DONE, { report_only: true, summary: "x" }, { subagent_type: "coder" })),
      {},
    );
    assert.deepStrictEqual(
      scanSignals(subagentToolUse(DONE, { report_only: true, summary: "x" }, { parent_tool_use_id: "toolu_x" })),
      {},
    );
  });
});

describe("buildSignalMcpServer prd_done_path schema gate (PRD #72 M4)", () => {
  // Gating the SCHEMA is the strongest layer available: on a non-issue run the
  // model never sees the parameter, rather than seeing it and having the value
  // dropped two hops downstream.
  const doneToolShape = (server: unknown): Record<string, unknown> => {
    const s = server as { instance?: unknown };
    const tools = (s.instance as { _registeredTools?: Record<string, { inputSchema?: unknown }> } | undefined)?._registeredTools;
    assert.ok(tools, "expected the sdk server to expose its registered tools");
    const done = tools!["signal_done"];
    assert.ok(done, `expected a signal_done tool; got ${Object.keys(tools!).join(", ")}`);
    const shape = (done.inputSchema as { shape?: Record<string, unknown> } | undefined)?.shape;
    assert.ok(shape, "expected a zod object schema with a shape");
    return shape!;
  };

  it("omits prd_done_path by default (and so on ci_fix / self_improve)", () => {
    for (const server of [buildSignalMcpServer(), buildSignalMcpServer({}), buildSignalMcpServer({ prdDonePath: false })]) {
      const shape = doneToolShape(server);
      assert.ok(!("prd_done_path" in shape), `prd_done_path must be absent; got ${Object.keys(shape).join(", ")}`);
      assert.ok("summary" in shape, "the pre-existing summary param is unchanged");
    }
  });

  it("exposes prd_done_path when enabled", () => {
    const shape = doneToolShape(buildSignalMcpServer({ prdDonePath: true }));
    assert.ok("prd_done_path" in shape, `expected prd_done_path; got ${Object.keys(shape).join(", ")}`);
    assert.ok("summary" in shape);
  });

  it("gates signal_done milestones_completed on the issue-run (milestones) discriminator (PRD #265 M1)", () => {
    // Invisible to the model on a non-issue run, exposed only when milestones are enabled.
    for (const server of [buildSignalMcpServer(), buildSignalMcpServer({ milestones: false })]) {
      const shape = doneToolShape(server);
      assert.ok(!("milestones_completed" in shape), `milestones_completed must be absent; got ${Object.keys(shape).join(", ")}`);
    }
    const enabled = doneToolShape(buildSignalMcpServer({ milestones: true }));
    assert.ok("milestones_completed" in enabled, `expected milestones_completed; got ${Object.keys(enabled).join(", ")}`);
    assert.ok("summary" in enabled);
  });

  it("gates signal_done report_only on the issue-run discriminator (issue #279)", () => {
    // Invisible to the model on a non-issue run, exposed only when reportOnly is enabled.
    for (const server of [buildSignalMcpServer(), buildSignalMcpServer({}), buildSignalMcpServer({ reportOnly: false })]) {
      const shape = doneToolShape(server);
      assert.ok(!("report_only" in shape), `report_only must be absent; got ${Object.keys(shape).join(", ")}`);
      assert.ok("summary" in shape, "the pre-existing summary param is unchanged");
    }
    const enabled = doneToolShape(buildSignalMcpServer({ reportOnly: true }));
    assert.ok("report_only" in enabled, `expected report_only; got ${Object.keys(enabled).join(", ")}`);
    assert.ok("summary" in enabled);
  });
});

describe("scanSignals milestones (PRD #122 M1)", () => {
  const PLAN = "mcp__uzi__submit_plan";

  it("extracts milestones from a main-thread submit_plan tool_use", () => {
    const r = scanSignals(
      toolUse(PLAN, {
        plan_md: "# Plan",
        milestones: [
          { id: "m1", title: "Wire the protocol" },
          { id: "m2", title: "Plumb the executor" },
        ],
      }),
    );
    assert.strictEqual(r.plan, "# Plan");
    assert.deepStrictEqual(r.milestones, [
      { id: "m1", title: "Wire the protocol" },
      { id: "m2", title: "Plumb the executor" },
    ]);
  });

  it("does NOT latch milestones from a subagent frame", () => {
    const r = scanSignals(
      subagentToolUse(PLAN, {
        plan_md: "# injected",
        milestones: [{ id: "m1", title: "escape" }],
      }),
    );
    // The whole frame is dropped by the isSubagentFrame guard, so nothing latches.
    assert.strictEqual(r.milestones, undefined);
  });

  it("drops malformed milestones without throwing", () => {
    // Non-array ⇒ no milestones key (plan still captured).
    const nonArray = scanSignals(toolUse(PLAN, { plan_md: "p", milestones: "nope" })) as Record<string, unknown>;
    assert.ok(!("milestones" in nonArray), "non-array milestones must not be captured");

    // Array of junk / entries missing id or title ⇒ all dropped, no key set.
    const junk = scanSignals(
      toolUse(PLAN, {
        plan_md: "p",
        milestones: [
          42,
          "str",
          null,
          {},
          { id: "m1" }, // no title
          { title: "no id" }, // no id
          { id: "  ", title: "blank id" }, // trimmed-empty id
          { id: "m2", title: "   " }, // trimmed-empty title
        ],
      }),
    ) as Record<string, unknown>;
    assert.ok(!("milestones" in junk), "all-junk milestones must not be captured");

    // A mixed list keeps only the well-formed entries.
    const mixed = scanSignals(
      toolUse(PLAN, {
        plan_md: "p",
        milestones: [{ id: "m1", title: "good" }, { id: "m2" }, 7],
      }),
    );
    assert.deepStrictEqual(mixed.milestones, [{ id: "m1", title: "good" }]);
  });

  it("clamps over-long id and title (worker-side hygiene, not the real cap)", () => {
    const longId = "x".repeat(200);
    const longTitle = "y".repeat(500);
    const r = scanSignals(toolUse(PLAN, { plan_md: "p", milestones: [{ id: longId, title: longTitle }] }));
    assert.strictEqual([...(r.milestones![0]!.id)].length, 64);
    assert.strictEqual([...(r.milestones![0]!.title)].length, 200);
  });
});

describe("buildSignalMcpServer milestones schema gate (PRD #122 M1)", () => {
  // Mirrors the prd_done_path schema-gate test: gating the schema keeps the model
  // from ever seeing `milestones` on a non-issue run (Decision 13).
  const planToolShape = (server: unknown): Record<string, unknown> => {
    const s = server as { instance?: unknown };
    const tools = (s.instance as { _registeredTools?: Record<string, { inputSchema?: unknown }> } | undefined)?._registeredTools;
    assert.ok(tools, "expected the sdk server to expose its registered tools");
    const plan = tools!["submit_plan"];
    assert.ok(plan, `expected a submit_plan tool; got ${Object.keys(tools!).join(", ")}`);
    const shape = (plan.inputSchema as { shape?: Record<string, unknown> } | undefined)?.shape;
    assert.ok(shape, "expected a zod object schema with a shape");
    return shape!;
  };

  it("omits milestones by default (and so on ci_fix / self_improve)", () => {
    for (const server of [buildSignalMcpServer(), buildSignalMcpServer({}), buildSignalMcpServer({ milestones: false })]) {
      const shape = planToolShape(server);
      assert.ok(!("milestones" in shape), `milestones must be absent; got ${Object.keys(shape).join(", ")}`);
      assert.ok("plan_md" in shape, "the pre-existing plan_md param is unchanged");
    }
  });

  it("exposes milestones when enabled", () => {
    const shape = planToolShape(buildSignalMcpServer({ milestones: true }));
    assert.ok("milestones" in shape, `expected milestones; got ${Object.keys(shape).join(", ")}`);
    assert.ok("plan_md" in shape);
  });
});

describe("scanSignals report_progress (PRD #122 M2)", () => {
  const PROGRESS = "mcp__uzi__report_progress";

  it("extracts progress from a main-thread report_progress tool_use", () => {
    const r = scanSignals(
      toolUse(PROGRESS, { completed: ["m1", "m2"], in_progress: ["m3"] }),
    );
    assert.deepStrictEqual(r.progress, {
      completed: ["m1", "m2"],
      in_progress: ["m3"],
    });
  });

  it("does NOT latch progress from a subagent frame (the main-thread guard holds)", () => {
    // A subagent must never be able to move the run's progress. Both markers together,
    // and each independently, must be dropped by the isSubagentFrame guard.
    assert.strictEqual(
      scanSignals(subagentToolUse(PROGRESS, { completed: ["m1"], in_progress: [] })).progress,
      undefined,
    );
    assert.strictEqual(
      scanSignals(
        subagentToolUse(PROGRESS, { completed: ["m1"] }, { subagent_type: "coder" }),
      ).progress,
      undefined,
    );
    assert.strictEqual(
      scanSignals(
        subagentToolUse(PROGRESS, { completed: ["m1"] }, { parent_tool_use_id: "toolu_x" }),
      ).progress,
      undefined,
    );
  });

  it("drops malformed input without throwing (all-empty = no signal, PRD #390 D3)", () => {
    // PRD #390 D3: a completely empty call is NO SIGNAL — it must not persist a
    // misleading `[]`, so progress stays undefined.
    assert.strictEqual(scanSignals(toolUse(PROGRESS, {})).progress, undefined);
    // Non-array sides parse to two empty arrays ⇒ still no signal ⇒ undefined.
    assert.strictEqual(
      scanSignals(toolUse(PROGRESS, { completed: "nope", in_progress: 42 })).progress,
      undefined,
    );
    // Junk entries dropped, blanks removed, ids trimmed, numbers/booleans coerced, deduped.
    assert.deepStrictEqual(
      scanSignals(
        toolUse(PROGRESS, {
          completed: ["m1", " m2 ", "m1", "", "  ", null, {}, [], 7, true],
          in_progress: [],
        }),
      ).progress,
      { completed: ["m1", "m2", "7", "true"], in_progress: [] },
    );
  });

  it("clamps an over-long id (worker-side hygiene, not the real cap)", () => {
    const longId = "x".repeat(200);
    const r = scanSignals(toolUse(PROGRESS, { completed: [longId], in_progress: [] }));
    assert.strictEqual([...r.progress!.completed[0]!].length, 64);
  });

  it("last-wins within one message when report_progress is called twice", () => {
    const msg = {
      type: "assistant",
      session_id: "s",
      message: {
        content: [
          { type: "tool_use", id: "a", name: PROGRESS, input: { completed: ["m1"], in_progress: ["m2"] } },
          { type: "tool_use", id: "b", name: PROGRESS, input: { completed: ["m1", "m2"], in_progress: [] } },
        ],
      },
    };
    assert.deepStrictEqual(scanSignals(msg).progress, {
      completed: ["m1", "m2"],
      in_progress: [],
    });
  });

  it("a later all-empty report does NOT wipe an earlier real one (PRD #390 D3)", () => {
    const msg = {
      type: "assistant",
      session_id: "s",
      message: {
        content: [
          { type: "tool_use", id: "a", name: PROGRESS, input: { completed: ["m1"], in_progress: ["m2"] } },
          { type: "tool_use", id: "b", name: PROGRESS, input: {} },
        ],
      },
    };
    assert.deepStrictEqual(scanSignals(msg).progress, {
      completed: ["m1"],
      in_progress: ["m2"],
    });
  });
});

describe("buildSignalMcpServer report_progress schema gate (PRD #122 M2)", () => {
  // Mirrors the milestones/prd_done_path schema-gate tests: the tool is registered only
  // for issue runs (Decision 13), so the model never sees it on a non-issue run.
  const registeredTools = (server: unknown): Record<string, unknown> => {
    const s = server as { instance?: unknown };
    const tools = (s.instance as { _registeredTools?: Record<string, unknown> } | undefined)?._registeredTools;
    assert.ok(tools, "expected the sdk server to expose its registered tools");
    return tools!;
  };

  it("does NOT register report_progress by default (ci_fix / self_improve)", () => {
    for (const server of [buildSignalMcpServer(), buildSignalMcpServer({}), buildSignalMcpServer({ progress: false })]) {
      const tools = registeredTools(server);
      assert.ok(!("report_progress" in tools), `report_progress must be absent; got ${Object.keys(tools).join(", ")}`);
    }
  });

  it("registers report_progress (with both id-array params) when enabled", () => {
    const tools = registeredTools(buildSignalMcpServer({ progress: true }));
    const progress = tools["report_progress"] as { inputSchema?: { shape?: Record<string, unknown> } } | undefined;
    assert.ok(progress, `expected a report_progress tool; got ${Object.keys(tools).join(", ")}`);
    const shape = progress!.inputSchema?.shape;
    assert.ok(shape, "expected a zod object schema with a shape");
    assert.ok("completed" in shape! && "in_progress" in shape!, `expected completed + in_progress; got ${Object.keys(shape!).join(", ")}`);
  });
});

describe("scanSignals checkpoint (PRD #122 M6)", () => {
  const CHECKPOINT = "mcp__uzi__checkpoint";

  it("extracts checkpoint:true from a main-thread checkpoint tool_use", () => {
    assert.deepStrictEqual(scanSignals(toolUse(CHECKPOINT, {})), { checkpoint: true });
  });

  it("does NOT latch checkpoint from a subagent frame (the main-thread guard holds)", () => {
    // A subagent must never be able to force a checkpoint (which reaps the agent tree +
    // fetches the branch back). Both markers together, and each independently, are dropped.
    assert.strictEqual(scanSignals(subagentToolUse(CHECKPOINT, {})).checkpoint, undefined);
    assert.strictEqual(
      scanSignals(subagentToolUse(CHECKPOINT, {}, { subagent_type: "coder" })).checkpoint,
      undefined,
    );
    assert.strictEqual(
      scanSignals(subagentToolUse(CHECKPOINT, {}, { parent_tool_use_id: "toolu_x" })).checkpoint,
      undefined,
    );
  });

  it("still honors checkpoint on a main-thread frame (parent_tool_use_id: null)", () => {
    const r = scanSignals(subagentToolUse(CHECKPOINT, {}, { parent_tool_use_id: null }));
    assert.deepStrictEqual(r, { checkpoint: true });
  });
});

describe("buildSignalMcpServer checkpoint schema gate (PRD #122 M6)", () => {
  // Mirrors the report_progress schema-gate test: the tool is registered only for issue
  // runs (Decision 13), so the model never sees it on a non-issue run.
  const registeredTools = (server: unknown): Record<string, unknown> => {
    const s = server as { instance?: unknown };
    const tools = (s.instance as { _registeredTools?: Record<string, unknown> } | undefined)?._registeredTools;
    assert.ok(tools, "expected the sdk server to expose its registered tools");
    return tools!;
  };

  it("does NOT register checkpoint by default (ci_fix / self_improve)", () => {
    for (const server of [buildSignalMcpServer(), buildSignalMcpServer({}), buildSignalMcpServer({ checkpoint: false })]) {
      const tools = registeredTools(server);
      assert.ok(!("checkpoint" in tools), `checkpoint must be absent; got ${Object.keys(tools).join(", ")}`);
    }
  });

  it("registers a no-argument checkpoint tool when enabled", () => {
    const tools = registeredTools(buildSignalMcpServer({ checkpoint: true }));
    const checkpoint = tools["checkpoint"] as { inputSchema?: { shape?: Record<string, unknown> } } | undefined;
    assert.ok(checkpoint, `expected a checkpoint tool; got ${Object.keys(tools).join(", ")}`);
    // It takes no arguments — an empty zod object shape.
    const shape = checkpoint!.inputSchema?.shape ?? {};
    assert.deepStrictEqual(Object.keys(shape), [], "checkpoint takes no arguments");
  });
});

describe("isSignalToolName", () => {
  it("matches the qualified signal tools only", () => {
    assert.strictEqual(isSignalToolName("mcp__uzi__submit_plan"), true);
    assert.strictEqual(isSignalToolName("mcp__uzi__signal_done"), true);
    // PRD #122 M2: report_progress is filtered from the persisted stream too.
    assert.strictEqual(isSignalToolName("mcp__uzi__report_progress"), true);
    // PRD #122 M6: checkpoint is filtered from the persisted stream too.
    assert.strictEqual(isSignalToolName("mcp__uzi__checkpoint"), true);
    assert.strictEqual(isSignalToolName("submit_plan"), false);
    assert.strictEqual(isSignalToolName("Bash"), false);
    assert.strictEqual(isSignalToolName(undefined), false);
  });
});

describe("buildSignalMcpServer", () => {
  it("builds an in-process (sdk) MCP server named uzi", () => {
    const s = buildSignalMcpServer() as unknown as { type: string; name: string };
    assert.strictEqual(s.type, "sdk");
    assert.strictEqual(s.name, "uzi");
  });
});
