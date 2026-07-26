// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { RunUsagePanel } from "./RunUsage";
import { deriveRunUsage } from "../lib/runUsage";
import type { RunMessage } from "../lib/api";

afterEach(cleanup);

let seq = 0;
function m(kind: string, agent: string | null, payload: unknown): RunMessage {
  return { seq: ++seq, kind, agent, agent_instance: null, agent_label: null, payload, created_at: "2026-07-12T00:00:00Z" };
}

// A two-phase run: plan + one implement iteration, with per-agent assistant usage.
function twoPhase(): RunMessage[] {
  seq = 0;
  return [
    m("status", "lead", { event: "init", model: "claude-sonnet-5" }),
    m("text", "lead", { text: "planning", usage: { input_tokens: 20_000, cache_read_input_tokens: 180_000, cache_creation_input_tokens: 1_000, output_tokens: 6_000 } }),
    m("status", "lead", { event: "result", subtype: "success", num_turns: 9, duration_ms: 100_000, total_cost_usd: 0.24, usage: { input_tokens: 21_400, cache_read_input_tokens: 188_000, cache_creation_input_tokens: 0, output_tokens: 6_100 } }),
    m("text", "coder", { text: "implementing", usage: { input_tokens: 50_000, cache_read_input_tokens: 600_000, cache_creation_input_tokens: 0, output_tokens: 28_000 } }),
    m("status", "lead", { event: "result", subtype: "success", num_turns: 34, duration_ms: 200_000, total_cost_usd: 1.26, usage: { input_tokens: 80_300, cache_read_input_tokens: 800_300, cache_creation_input_tokens: 0, output_tokens: 34_800 } }),
  ];
}

describe("RunUsagePanel", () => {
  it("renders the strip totals, per-phase deltas, and per-agent attribution", () => {
    const { getByText, getAllByText } = render(<RunUsagePanel usage={deriveRunUsage(twoPhase())} />);

    // Strip: tokens-in = fresh(80.3k) + cached(800.3k) = 880.6k, model, cache bar.
    expect(getByText("Tokens in")).toBeTruthy();
    expect(getByText("880.6k")).toBeTruthy();
    expect(getByText(/from cache/)).toBeTruthy();
    expect(getByText("claude-sonnet-5")).toBeTruthy();
    // The run total cost shows in both the strip and the per-phase total row (they
    // agree by construction — the strip is the sum of the phase deltas).
    expect(getAllByText("$1.26").length).toBeGreaterThanOrEqual(1);

    // Per-phase table: labelled rows + the differenced iteration + a run total.
    expect(getByText("Plan")).toBeTruthy();
    expect(getByText("Implement · iteration 1")).toBeTruthy();
    expect(getByText("Run total")).toBeTruthy();
    // The iteration-1 delta fresh = 80.3k − 21.4k = 58.9k (unique to that row).
    expect(getByText("58.9k")).toBeTruthy();

    // Per-agent table: one row per agent + the attribution footnote.
    expect(getByText("lead")).toBeTruthy();
    expect(getByText("coder")).toBeTruthy();
    expect(getByText("Attributed total")).toBeTruthy();
    expect(getByText(/may not sum to the run total/)).toBeTruthy();
  });

  it("renders nothing for a run with no usage (pre-feature)", () => {
    const { container } = render(<RunUsagePanel usage={deriveRunUsage([m("text", "lead", { text: "hi" })])} />);
    expect(container.firstChild).toBeNull();
  });
});

// PRD #93: the per-agent Model column. `model` rides the same frame as `usage`.
function assistantFrame(agent: string, out: number, model?: string): RunMessage {
  return m("text", agent, {
    text: "…",
    ...(model ? { model } : {}),
    usage: { input_tokens: 1_000, cache_read_input_tokens: 0, cache_creation_input_tokens: 0, output_tokens: out },
  });
}
function resultFrame(): RunMessage {
  return m("status", "lead", {
    event: "result",
    subtype: "success",
    num_turns: 3,
    duration_ms: 5_000,
    total_cost_usd: 0.5,
    usage: { input_tokens: 3_000, cache_read_input_tokens: 0, cache_creation_input_tokens: 0, output_tokens: 300 },
  });
}

describe("RunUsagePanel model column (PRD #93)", () => {
  it("renders each agent's model and 'N models' on the total row for a mixed run", () => {
    seq = 0;
    const { getByText, getAllByText } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          assistantFrame("lead", 100, "claude-opus-4-8"),
          assistantFrame("coder", 200, "claude-sonnet-5"),
          resultFrame(),
        ])}
      />,
    );

    expect(getByText("Model")).toBeTruthy(); // the new per-agent header
    expect(getAllByText("claude-opus-4-8").length).toBe(1);
    expect(getByText("claude-sonnet-5")).toBeTruthy();
    expect(getByText("2 models")).toBeTruthy(); // the "Attributed total" cell
  });

  it("suffixes '+1' for an agent that ran on more than one model", () => {
    seq = 0;
    const { getByText } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          assistantFrame("coder", 100, "claude-sonnet-5"),
          assistantFrame("coder", 100, "claude-sonnet-5"),
          assistantFrame("coder", 100, "claude-opus-4-8"),
          resultFrame(),
        ])}
      />,
    );
    expect(getByText("claude-sonnet-5 +1")).toBeTruthy();
    // One agent, two distinct models → the total row still reads "2 models".
    expect(getByText("2 models")).toBeTruthy();
  });

  it("shows the single model string on the total row when the run used exactly one", () => {
    seq = 0;
    const { getAllByText } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          assistantFrame("lead", 100, "claude-opus-4-8"),
          assistantFrame("coder", 200, "claude-opus-4-8"),
          resultFrame(),
        ])}
      />,
    );
    // Two agent rows + the total row all read the same model string.
    expect(getAllByText("claude-opus-4-8").length).toBe(3);
  });

  it("renders '—' in the Model column for a pre-feature run (usage, no models)", () => {
    seq = 0;
    const { getAllByText, getByText } = render(
      <RunUsagePanel usage={deriveRunUsage([assistantFrame("lead", 100), resultFrame()])} />,
    );
    expect(getByText("Model")).toBeTruthy();
    // The agent row and the "Attributed total" row both degrade to a dash, and the
    // panel renders without throwing.
    expect(getAllByText("—").length).toBeGreaterThanOrEqual(2);
  });
});

describe("RunUsagePanel $0 cost (Decision 8)", () => {
  it("renders a $0 cost as '—' in the strip and per-phase total, never '$0.00'", () => {
    seq = 0;
    const messages = [
      m("status", "lead", {
        event: "result",
        subtype: "success",
        num_turns: 3,
        duration_ms: 5000,
        total_cost_usd: 0,
        usage: { input_tokens: 1000, cache_read_input_tokens: 0, cache_creation_input_tokens: 0, output_tokens: 200 },
      }),
    ];
    const { container } = render(<RunUsagePanel usage={deriveRunUsage(messages)} />);
    expect(container.textContent).toContain("—");
    expect(container.textContent).not.toContain("$0.00");
  });
});
