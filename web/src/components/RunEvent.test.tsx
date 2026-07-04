// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render } from "@testing-library/react";
import type { RunMessage } from "../lib/api";
import {
  RunEventRow,
  buildToolIndex,
  describeError,
  describeStatus,
  formatDuration,
  resultToText,
  toolSummary,
  truncate,
} from "./RunEvent";

afterEach(cleanup);

function msg(partial: Partial<RunMessage> & { kind: string; seq: number }): RunMessage {
  return { agent: "lead", payload: {}, created_at: "2026-07-04T00:00:00.000Z", ...partial };
}

describe("toolSummary", () => {
  it("summarizes known tools by their salient argument", () => {
    expect(toolSummary("Bash", { command: "ls -la\nsecond line" })).toBe("ls -la");
    expect(toolSummary("Read", { file_path: "/a/b.ts" })).toBe("/a/b.ts");
    expect(toolSummary("Edit", { file_path: "/a/b.ts", old_string: "x" })).toBe("/a/b.ts");
    expect(toolSummary("Grep", { pattern: "foo", path: "src" })).toBe("foo in src");
    expect(toolSummary("Glob", { pattern: "**/*.ts" })).toBe("**/*.ts");
    expect(toolSummary("WebFetch", { url: "https://x" })).toBe("https://x");
  });

  it("surfaces the subagent_type for Task spawns", () => {
    expect(toolSummary("Task", { subagent_type: "reviewer", description: "check the diff" })).toBe(
      "reviewer: check the diff",
    );
    expect(toolSummary("Task", { subagent_type: "coder" })).toBe("coder");
  });

  it("degrades an unknown tool to compact key: value pairs (no whole-frame JSON)", () => {
    expect(toolSummary("Frobnicate", { a: 1, b: "x", c: true })).toBe("a: 1, b: x, c: true");
    expect(toolSummary("Nested", { opts: { deep: 1 } })).toBe('opts: {"deep":1}');
  });
});

describe("resultToText", () => {
  it("passes a string through", () => {
    expect(resultToText("ok")).toEqual({ text: "ok", hadNonText: false });
  });
  it("walks a block array, joining text and flagging non-text blocks", () => {
    expect(
      resultToText([
        { type: "text", text: "first" },
        { type: "image", source: {} },
        { type: "text", text: "second" },
      ]),
    ).toEqual({ text: "first\nsecond", hadNonText: true });
  });
  it("returns empty for an unrecognized content shape", () => {
    expect(resultToText(undefined)).toEqual({ text: "", hadNonText: false });
  });
});

describe("formatDuration", () => {
  it("renders sub-minute spans in seconds and longer spans as m ss", () => {
    expect(formatDuration(800)).toBe("0.8s");
    expect(formatDuration(12400)).toBe("12.4s");
    expect(formatDuration(65000)).toBe("1m 05s");
    expect(formatDuration(-5)).toBe("0.0s");
  });
});

describe("truncate", () => {
  it("keeps short strings and ellipsizes long ones", () => {
    expect(truncate("short", 10)).toBe("short");
    expect(truncate("abcdefghij", 5)).toBe("abcd…");
  });
});

describe("describeStatus", () => {
  it("renders worker progress text as-is", () => {
    expect(describeStatus({ text: "worktree ready on agent/issue-7" })).toBe(
      "worktree ready on agent/issue-7",
    );
  });
  it("renders the SDK init frame", () => {
    expect(describeStatus({ event: "init", model: "claude-fable-5" })).toBe(
      "agent started (claude-fable-5)",
    );
  });
  it("renders a success result from the forwarded fields", () => {
    expect(
      describeStatus({
        event: "result",
        subtype: "success",
        duration_ms: 12400,
        num_turns: 3,
        total_cost_usd: 0.0731,
      }),
    ).toBe("agent finished (12.4s, 3 turns, $0.07)");
  });
  it("discriminates a non-success result by subtype (not event)", () => {
    expect(describeStatus({ event: "result", subtype: "error_max_turns" })).toBe(
      "status: result/error_max_turns",
    );
  });
});

describe("describeError", () => {
  it("renders worker error text", () => {
    expect(describeError({ text: "push failed" })).toBe("push failed");
  });
  it("renders the SDK result subtype and joined errors", () => {
    expect(describeError({ subtype: "error_max_turns", errors: ["hit the cap"] })).toBe(
      "error_max_turns: hit the cap",
    );
    expect(describeError({ subtype: "error_during_execution", errors: [] })).toBe(
      "error_during_execution",
    );
  });
});

describe("buildToolIndex", () => {
  it("pairs each result with its own call by id — not by adjacency (parallel calls)", () => {
    const messages: RunMessage[] = [
      msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Read" } }),
      msg({ seq: 2, kind: "tool_use", payload: { id: "B", name: "Grep" } }),
      msg({ seq: 3, kind: "tool_result", payload: { tool_use_id: "A", content: "a" } }),
      msg({ seq: 4, kind: "tool_result", payload: { tool_use_id: "B", content: "b" } }),
    ];
    const idx = buildToolIndex(messages);
    expect(idx.resultByUseId.get("A")?.seq).toBe(3);
    expect(idx.resultByUseId.get("B")?.seq).toBe(4);
    expect(idx.toolUseIds.has("A")).toBe(true);
    expect(idx.toolUseIds.has("B")).toBe(true);
  });
  it("does not own a result whose tool_use_id has no matching call (orphan)", () => {
    const idx = buildToolIndex([
      msg({ seq: 1, kind: "tool_result", payload: { tool_use_id: "Z", content: "z" } }),
    ]);
    expect(idx.toolUseIds.has("Z")).toBe(false);
    expect(idx.resultByUseId.get("Z")?.seq).toBe(1);
  });
});

describe("RunEventRow rendering", () => {
  it("renders a text event as markdown (heading becomes an h1)", () => {
    const { container } = render(
      <RunEventRow msg={msg({ seq: 1, kind: "text", payload: { text: "# Title\n\nbody" } })} live={false} />,
    );
    expect(container.querySelector("h1")?.textContent).toBe("Title");
  });

  it("shows a running spinner for an in-flight tool_use (no result, live)", () => {
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Bash", input: { command: "sleep 60" } } })}
        live={true}
      />,
    );
    expect(container.textContent).toContain("Bash");
    expect(container.textContent).toContain("sleep 60");
    expect(container.textContent).toContain("running…");
  });

  it("shows a client-side duration once the result is paired", () => {
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "tool_use", created_at: "2026-07-04T00:00:00.000Z", payload: { id: "A", name: "Bash", input: { command: "ls" } } })}
        result={msg({ seq: 2, kind: "tool_result", created_at: "2026-07-04T00:00:01.000Z", payload: { tool_use_id: "A", content: "ok" } })}
        live={false}
      />,
    );
    expect(container.textContent).toContain("1.0s");
    expect(container.textContent).not.toContain("running…");
  });

  it("auto-expands an errored tool_result with a ✗ marker", () => {
    const errorBody = Array.from({ length: 12 }, (_, i) => `err line ${i}`).join("\n");
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Bash", input: { command: "false" } } })}
        result={msg({ seq: 2, kind: "tool_result", payload: { tool_use_id: "A", content: errorBody, is_error: true } })}
        live={false}
      />,
    );
    // Auto-expanded: the full body is present without a click.
    expect(container.textContent).toContain("err line 11");
    expect(container.textContent).toContain("✗");
  });

  it("collapses a large non-error result behind a 'show N lines' expander", () => {
    const body = Array.from({ length: 20 }, (_, i) => `line ${i}`).join("\n");
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Read", input: { file_path: "/x" } } })}
        result={msg({ seq: 2, kind: "tool_result", payload: { tool_use_id: "A", content: body } })}
        live={false}
      />,
    );
    expect(container.textContent).toContain("show 20 lines");
    expect(container.textContent).not.toContain("line 19");
    const btn = container.querySelector("button[aria-expanded]");
    expect(btn?.getAttribute("aria-expanded")).toBe("false");
  });

  it("renders a plan event as a terse one-liner (never the body)", () => {
    const { container } = render(
      <RunEventRow msg={msg({ seq: 1, kind: "plan", payload: { plan_md: "# huge plan body" } })} live={false} />,
    );
    expect(container.textContent).toContain("plan submitted");
    expect(container.textContent).not.toContain("huge plan body");
  });

  it("falls back for an unknown kind, surfacing any extractable text", () => {
    const { container } = render(
      <RunEventRow msg={msg({ seq: 1, kind: "mystery", payload: { text: "some detail" } })} live={false} />,
    );
    expect(container.textContent).toContain("unrenderable mystery event");
    expect(container.textContent).toContain("some detail");
  });
});
