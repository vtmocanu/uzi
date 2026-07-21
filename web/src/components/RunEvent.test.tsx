// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, fireEvent, render } from "@testing-library/react";
import type { RunMessage } from "../lib/api";
import {
  CommandBlock,
  RunEventRow,
  buildToolIndex,
  describeError,
  describeStatus,
  formatDuration,
  highlightShell,
  resultToText,
  toolSummary,
  truncate,
} from "./RunEvent";

afterEach(cleanup);

function msg(partial: Partial<RunMessage> & { kind: string; seq: number }): RunMessage {
  return { agent: "lead", agent_instance: null, agent_label: null, payload: {}, created_at: "2026-07-04T00:00:00.000Z", ...partial };
}

describe("toolSummary", () => {
  it("keeps the full multi-line command for Bash (no first-line truncation)", () => {
    // PRD #38 Decision 1: the full command is the source of truth; truncation is
    // a display concern handled by CommandBlock, never a lossy transform here.
    const cmd = "cat <<'EOF'\nline one\nline two && echo done\nEOF";
    expect(toolSummary("Bash", { command: cmd })).toBe(cmd);
  });

  it("summarizes known tools by their salient argument", () => {
    expect(toolSummary("Bash", { command: "ls -la\nsecond line" })).toBe("ls -la\nsecond line");
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
  it("renders a success result from duration + turns (cost moved to the per-phase finish line, PRD #40)", () => {
    expect(
      describeStatus({
        event: "result",
        subtype: "success",
        duration_ms: 12400,
        num_turns: 3,
        // total_cost_usd is cumulative-across-resume (verdict b), so it is no longer
        // shown here — the per-phase delta cost rides the finish line's FinishTokens.
        total_cost_usd: 0.0731,
      }),
    ).toBe("agent finished (12.4s, 3 turns)");
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

  it("collapses a large non-error result into a chip with a mounted-but-hidden body", () => {
    const body = Array.from({ length: 20 }, (_, i) => `line ${i}`).join("\n");
    const { container, getByRole } = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Read", input: { file_path: "/x" } } })}
        result={msg({ seq: 2, kind: "tool_result", payload: { tool_use_id: "A", content: body } })}
        live={false}
      />,
    );
    const chip = getByRole("button", { name: /show 20 lines of Read output/i });
    expect(chip.getAttribute("aria-expanded")).toBe("false");
    expect(chip.textContent).toContain("20 lines");
    // The body stays mounted (hidden) so its text is in the DOM while collapsed.
    const pre = container.querySelector("pre");
    expect(pre?.hasAttribute("hidden")).toBe(true);
    expect(pre?.textContent).toContain("line 19");
  });

  it("renders a plan event as a terse one-liner (never the body)", () => {
    const { container } = render(
      <RunEventRow msg={msg({ seq: 1, kind: "plan", payload: { plan_md: "# huge plan body" } })} live={false} />,
    );
    expect(container.textContent).toContain("plan submitted");
    expect(container.textContent).not.toContain("huge plan body");
  });

  it("renders a plan_revising event as a terse one-liner with the round (PRD #41)", () => {
    const { container } = render(
      <RunEventRow msg={msg({ seq: 1, kind: "plan_revising", payload: { round: 2 } })} live={false} />,
    );
    expect(container.textContent).toContain("revising plan");
    expect(container.textContent).toContain("round 2");
  });

  it("renders plan_feedback text ESCAPED, never as a live element (PRD #41)", () => {
    // The steering text is untrusted, so a <script>/<img onerror> payload must render
    // as inert text through the hardened <Markdown> — never injected as a real node.
    const feedback = "drop step 1 <script>alert(1)</script> <img src=x onerror=alert(2)>";
    const { container } = render(
      <RunEventRow msg={msg({ seq: 1, kind: "plan_feedback", payload: { feedback } })} live={false} />,
    );
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("img")).toBeNull();
    // The safe portion of the feedback still renders as text.
    expect(container.textContent).toContain("drop step 1");
  });

  it("falls back for an unknown kind, surfacing any extractable text", () => {
    const { container } = render(
      <RunEventRow msg={msg({ seq: 1, kind: "mystery", payload: { text: "some detail" } })} live={false} />,
    );
    expect(container.textContent).toContain("unrenderable mystery event");
    expect(container.textContent).toContain("some detail");
  });

  it("renders a full multi-line Bash command through the code surface", () => {
    const command = "cat <<'EOF'\nfirst line\nsecond line && echo done\nEOF";
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Bash", input: { command } } })}
        live={false}
      />,
    );
    // No line is lost end-to-end (the bug PRD #38 fixes).
    expect(container.textContent).toContain("first line");
    expect(container.textContent).toContain("second line && echo done");
    expect(container.textContent).toContain("EOF");
    // Rendered on the code surface (❯ prompt) with the multi-line clamp toggle.
    expect(container.textContent).toContain("❯");
    expect(container.querySelector("button[aria-expanded='false']")?.textContent).toMatch(
      /show full command/i,
    );
  });
});

describe("highlightShell", () => {
  const renderCmd = (cmd: string) => render(<div data-testid="h">{highlightShell(cmd)}</div>);

  it("preserves the exact command text — no character dropped or added", () => {
    for (const cmd of [
      "ls -la",
      "cd api && go build ./...",
      "grep -rn 'foo bar' src | tee /tmp/out.log # note",
      'echo "a\\"b" || rm -rf /x',
      "env -i HOME=$HOME docker compose -p uzi run --rm api",
      "cat <<'EOF'\nline one\nline two\nEOF",
      "echo 'unclosed", // unterminated single quote — tail preserved, not dropped
      'echo "unclosed', // unterminated double quote
    ]) {
      const { getByTestId, unmount } = renderCmd(cmd);
      expect(getByTestId("h").textContent).toBe(cmd);
      unmount();
    }
  });

  it("caps DOM fan-out for a pathological command while preserving the text", () => {
    // ~200 KB of "a b a b …": uncapped this is ~100k DOM nodes and freezes the tab.
    const cmd = "a b ".repeat(50_000);
    const { getByTestId } = renderCmd(cmd);
    const host = getByTestId("h");
    expect(host.textContent).toBe(cmd); // every character still present
    expect(host.childNodes.length).toBeLessThanOrEqual(2001); // spans + remainder tail
    expect(host.querySelectorAll("span").length).toBeLessThanOrEqual(2000);
  });

  it("classifies command / flag / string / operator / comment tokens", () => {
    const { container } = renderCmd("git commit -m 'wip' && echo ok # done");
    expect(container.querySelector(".text-syn-cmd")?.textContent).toBe("git");
    expect(container.querySelector(".text-syn-flag")?.textContent).toBe("-m");
    expect(container.querySelector(".text-syn-str")?.textContent).toBe("'wip'");
    expect(container.querySelector(".text-syn-op")?.textContent).toBe("&&");
    expect(container.querySelector(".text-syn-comment")?.textContent).toBe("# done");
    // The word after a control operator is treated as a fresh command name.
    const cmds = Array.from(container.querySelectorAll(".text-syn-cmd")).map((e) => e.textContent);
    expect(cmds).toContain("echo");
  });

  it("renders a <script> payload inert as escaped text, not a real element", () => {
    const cmd = "echo <script>alert(1)</script>";
    const { container, getByTestId } = renderCmd(cmd);
    expect(container.querySelector("script")).toBeNull();
    expect(getByTestId("h").textContent).toBe(cmd);
  });
});

describe("CommandBlock", () => {
  it("renders every line of a multi-line command (full fidelity)", () => {
    const cmd = "cd web\nnpm run build\nnpm test";
    const { container } = render(<CommandBlock command={cmd} />);
    expect(container.textContent).toContain("cd");
    expect(container.textContent).toContain("npm run build");
    expect(container.textContent).toContain("npm test");
  });

  it("offers the clamp toggle for a multi-line OR long command (content heuristic)", () => {
    for (const cmd of ["echo one\necho two", `echo ${"x".repeat(200)}`]) {
      const { getByRole, unmount } = render(<CommandBlock command={cmd} />);
      const btn = getByRole("button", { name: /show full command/i });
      expect(btn.getAttribute("aria-expanded")).toBe("false");
      fireEvent.click(btn);
      expect(btn.getAttribute("aria-expanded")).toBe("true");
      expect(btn.textContent).toMatch(/show less/i);
      unmount();
    }
  });

  it("shows no clamp toggle for a short single-line command", () => {
    const { queryByRole } = render(<CommandBlock command="git status --short" />);
    expect(queryByRole("button")).toBeNull();
  });

  it("clamps via max-height with the ❯ prompt inline (not line-clamp), toggled off on expand", () => {
    const cmd = "echo one\necho two\necho three";
    const { container, getByRole } = render(<CommandBlock command={cmd} />);
    // Clamp is a max-height wrapper, never line-clamp (which would push ❯ to its
    // own line by forcing display:-webkit-box on the code).
    const clampEl = container.querySelector(".max-h-\\[3\\.25em\\]");
    expect(clampEl).not.toBeNull();
    expect(container.querySelector(".line-clamp-2")).toBeNull();
    // ❯ is an inline sibling INSIDE the clamped wrapper and never pollutes the
    // code's exact text.
    expect(clampEl?.querySelector("span[aria-hidden='true']")?.textContent).toBe("❯");
    expect(container.querySelector("code")?.textContent).toBe(cmd);
    // Expanding drops the clamp.
    fireEvent.click(getByRole("button", { name: /show full command/i }));
    expect(container.querySelector(".max-h-\\[3\\.25em\\]")).toBeNull();
  });
});

describe("result chips (PRD #38 Decision 13)", () => {
  const renderResult = (payload: Record<string, unknown>) =>
    render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Read", input: { file_path: "/x" } } })}
        result={msg({ seq: 2, kind: "tool_result", payload: { tool_use_id: "A", ...payload } })}
        live={false}
      />,
    );

  it("labels an error chip and auto-expands it with a danger body", () => {
    const { getByRole, container } = renderResult({ content: "boom\nstack", is_error: true });
    const chip = getByRole("button", { name: /hide Read error output/i });
    expect(chip.textContent).toContain("error");
    expect(chip.getAttribute("aria-expanded")).toBe("true");
    const pre = container.querySelector("pre");
    expect(pre?.hasAttribute("hidden")).toBe(false); // errors start open
    expect(pre?.getAttribute("aria-label")).toBe("Tool error output");
  });

  it("labels an empty or whitespace-only result 'ok'", () => {
    for (const content of ["", "   \n  \t"]) {
      const { getByRole, unmount } = renderResult({ content });
      const chip = getByRole("button", { name: /show Read output/i });
      expect(chip.textContent).toContain("ok");
      expect(chip.getAttribute("aria-expanded")).toBe("false");
      unmount();
    }
  });

  it("labels a multi-line result 'N lines' and a single-line result '1 line'", () => {
    const three = renderResult({ content: "a\nb\nc" });
    expect(three.getByRole("button", { name: /show 3 lines of Read output/i }).textContent).toContain(
      "3 lines",
    );
    three.unmount();
    const one = renderResult({ content: "only one line" });
    expect(one.getByRole("button", { name: /show 1 line of Read output/i }).textContent).toContain(
      "1 line",
    );
  });

  it("keeps the collapsed body mounted (hidden), then expands + focuses it", () => {
    const { container, getByRole } = renderResult({ content: "kept-in-dom" });
    const pre = container.querySelector("pre");
    expect(pre?.hasAttribute("hidden")).toBe(true);
    expect(pre?.textContent).toContain("kept-in-dom"); // still in the DOM
    fireEvent.click(getByRole("button"));
    expect(pre?.hasAttribute("hidden")).toBe(false);
    expect(document.activeElement).toBe(pre); // keyboard focus moves into it
  });

  it("surfaces a dropped non-text block as an '[image omitted]' first line", () => {
    const { container, getByRole } = renderResult({
      content: [
        { type: "text", text: "after image" },
        { type: "image", source: {} },
      ],
    });
    fireEvent.click(getByRole("button")); // expand
    const pre = container.querySelector("pre");
    expect(pre?.textContent?.startsWith("[image omitted]")).toBe(true);
    expect(pre?.textContent).toContain("after image");
  });
});

describe("tool durations (PRD #38 Decision 6)", () => {
  const withSpan = (msAt: string, resAt: string) =>
    render(
      <RunEventRow
        msg={msg({
          seq: 1,
          kind: "tool_use",
          created_at: msAt,
          payload: { id: "A", name: "Bash", input: { command: "x" } },
        })}
        result={msg({
          seq: 2,
          kind: "tool_result",
          created_at: resAt,
          payload: { tool_use_id: "A", content: "ok" },
        })}
        live={false}
      />,
    );

  it("renders sub-100ms as 'instant' (raw value in the title), never '0.0s'", () => {
    const { container } = withSpan("2026-07-04T00:00:00.000Z", "2026-07-04T00:00:00.040Z");
    const dur = Array.from(container.querySelectorAll("span")).find((s) => s.textContent === "instant");
    expect(dur).toBeTruthy();
    expect(dur?.getAttribute("title")).toBe("40ms");
    expect(container.textContent).not.toContain("0.0s");
  });

  it("renders a normal span via formatDuration and tints a >1m span --warn", () => {
    const normal = withSpan("2026-07-04T00:00:00.000Z", "2026-07-04T00:00:04.100Z");
    expect(normal.container.textContent).toContain("4.1s");
    normal.unmount();
    const slow = withSpan("2026-07-04T00:00:00.000Z", "2026-07-04T00:01:12.000Z");
    const dur = Array.from(slow.container.querySelectorAll("span")).find((s) =>
      /1m 12s/.test(s.textContent ?? ""),
    );
    expect(dur?.className).toContain("text-warn");
  });

  it("keeps the >160-char 'more' affordance for a long non-Bash arg", () => {
    const pattern = "x".repeat(200);
    const { getByRole, container } = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Grep", input: { pattern } } })}
        live={false}
      />,
    );
    const more = getByRole("button", { name: "more" });
    expect(more).toBeTruthy();
    // M4: the interactive label clears contrast (--muted, not --faint) and gets a
    // ≥24px hit target.
    expect(more.className).toContain("text-muted");
    expect(more.className).toContain("min-h-[24px]");
    expect(container.textContent).not.toContain(pattern); // truncated while collapsed
    fireEvent.click(more);
    expect(container.textContent).toContain(pattern); // full arg revealed
  });
});

describe("accessibility (PRD #38 M4)", () => {
  it("renders a status event as a hairline meta divider (single source of truth)", () => {
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "status", payload: { event: "init", model: "claude-fable-5" } })}
        live={false}
      />,
    );
    expect(container.textContent).toContain("agent started (claude-fable-5)");
    expect(container.querySelector(".h-px")).not.toBeNull();
  });

  it("gives the thinking expander a muted ≥24px target", () => {
    const { getByRole } = render(
      <RunEventRow msg={msg({ seq: 1, kind: "thinking", payload: { text: "deliberating" } })} live={false} />,
    );
    const btn = getByRole("button", { name: "show" });
    expect(btn.className).toContain("text-muted");
    expect(btn.className).toContain("min-h-[24px]");
  });

  it("renders the meta divider two-tone: mono text-fg key + muted detail", () => {
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "status", payload: { event: "init", model: "claude-fable-5" } })}
        live={false}
      />,
    );
    // The event-type key is the mono, text-fg anchor…
    const key = container.querySelector(".font-mono.text-fg");
    expect(key?.textContent).toBe("agent started");
    // …and the parenthetical detail stays in the (italic muted) line, not the key.
    expect(key?.textContent).not.toContain("claude-fable-5");
    expect(container.textContent).toContain("agent started (claude-fable-5)");
  });

  it("names the tool in a paired result-chip aria-label, tool-agnostic for orphans", () => {
    const paired = render(
      <RunEventRow
        msg={msg({ seq: 1, kind: "tool_use", payload: { id: "A", name: "Bash", input: { command: "ls" } } })}
        result={msg({ seq: 2, kind: "tool_result", payload: { tool_use_id: "A", content: "a\nb\nc" } })}
        live={false}
      />,
    );
    expect(paired.getByRole("button", { name: "Show 3 lines of Bash output" })).toBeTruthy();
    paired.unmount();

    // An orphan result (no matching call) has no tool name — falls back.
    const orphan = render(
      <RunEventRow msg={msg({ seq: 1, kind: "tool_result", payload: { tool_use_id: "Z", content: "a\nb" } })} live={false} />,
    );
    expect(orphan.getByRole("button", { name: "Show 2 lines of output" })).toBeTruthy();
  });
});

describe("RunEventRow finish-line usage (PRD #40)", () => {
  const phaseUsage = {
    seq: 5,
    label: "Implement · iteration 2",
    turns: 18,
    durationMs: 492_000,
    fresh: 34_100,
    cached: 371_200,
    out: 13_400,
    costUsd: 0.61,
    isError: false,
  };

  it("appends the phase's delta tokens + cost to a success result finish line", () => {
    const { container } = render(
      <RunEventRow
        msg={msg({
          seq: 5,
          kind: "status",
          payload: { event: "result", subtype: "success", duration_ms: 492_000, num_turns: 18, total_cost_usd: 1.87 },
        })}
        live={false}
        phaseUsage={phaseUsage}
      />,
    );
    // describeStatus keeps duration + turns; the cumulative $1.87 is NOT shown here.
    expect(container.textContent).toContain("agent finished (8m 12s, 18 turns)");
    expect(container.textContent).not.toContain("$1.87");
    // FinishTokens shows the per-phase delta + per-phase cost.
    expect(container.textContent).toContain("34.1k in · 371.2k cached · 13.4k out");
    expect(container.textContent).toContain("$0.61");
  });

  it("shows tokens on an error result finish line too (success AND error)", () => {
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 6, kind: "error", payload: { event: "result", subtype: "error_max_turns", errors: ["cap"] } })}
        live={false}
        phaseUsage={{ ...phaseUsage, isError: true, fresh: 500, cached: 0, out: 120, costUsd: 0.03 }}
      />,
    );
    expect(container.textContent).toContain("500 in · 0 cached · 120 out");
    expect(container.textContent).toContain("$0.03");
  });

  it("renders the plain finish line when no phase usage is supplied (pre-feature)", () => {
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 7, kind: "status", payload: { event: "result", subtype: "success", num_turns: 3 } })}
        live={false}
      />,
    );
    expect(container.textContent).toContain("agent finished");
    expect(container.textContent).not.toContain(" in · ");
  });
});

describe("FinishTokens $0 cost (Decision 8)", () => {
  it("shows tokens but DROPS a $0 cost on the finish line (subscription auth)", () => {
    const { container } = render(
      <RunEventRow
        msg={msg({ seq: 8, kind: "status", payload: { event: "result", subtype: "success", duration_ms: 1000, num_turns: 2 } })}
        live={false}
        phaseUsage={{ seq: 8, label: "Plan", turns: 2, durationMs: 1000, fresh: 1000, cached: 0, out: 200, costUsd: 0, isError: false }}
      />,
    );
    expect(container.textContent).toContain("1.0k in · 0 cached · 200 out");
    // Nonzero tokens, zero cost → no "$" figure at all, never "$0.00".
    expect(container.textContent).not.toContain("$");
  });
});
