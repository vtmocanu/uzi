// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunProgressCard } from "./RunProgressCard";
import { RunUsagePanel } from "./RunUsage";
import { deriveRunUsage } from "../lib/runUsage";
import type { CostStatus, RunMessage, UsageTail } from "../lib/apiTypes";

afterEach(cleanup);
function message(kind: string, payload: unknown, seq: number): RunMessage {
  return { seq, kind, agent: "lead", agent_instance: null, agent_label: null, payload, created_at: "2026-10-12T00:00:00Z" };
}
function side(kind: string, status: CostStatus | "", seq: number) {
  return message(kind, { pass: "intent", text: "Working", model_usage: { "test-model": {
    inputTokens: 20, cacheReadInputTokens: 80, cacheCreationInputTokens: 0, outputTokens: 10, costUSD: 0.25, costStatus: status,
  } } }, seq);
}
const confirmed = message("status", { event: "result", subtype: "success", total_cost_usd: 99, modelUsage: { "test-model": { inputTokens: 1, cacheReadInputTokens: 999, cacheCreationInputTokens: 0, outputTokens: 30, costUSD: 2 } }, usage: { input_tokens: 1, cache_read_input_tokens: 999, cache_creation_input_tokens: 0, output_tokens: 30 } }, 1);
const live = message("text", { model: "test-model", usage: { input_tokens: 30, cache_read_input_tokens: 70, output_tokens: 999 } }, 2);
const tail = (cost_status: UsageTail["cost_status"], cost_usd: number | null): UsageTail => ({
  input_tokens: 20, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 5,
  cost_status, cost_usd, price_table_version: "test", coverage: "partial", coverage_reasons: [], models: [],
});
function mount(messages: RunMessage[], status: CostStatus | "" = "metered", estimatedTail?: UsageTail) {
  const usage = deriveRunUsage(messages);
  const view = render(<MemoryRouter>
    <RunProgressCard run={{ id: "r", status: "running", health_since: null, milestones: [], milestones_completed: null, retry_not_before: null, recovery_retry_not_before: null,
      progress: { state: "planning", pct: null, phase: "", active_milestone_id: "", milestone_done: 0, milestone_total: 0 },
      usage: { input_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 0, cost_usd: 99, cost_status: status }, usage_estimated_tail: estimatedTail,
    }} usage={usage} />
    <div id="run-full-usage"><RunUsagePanel usage={usage} costStatus={status} harness="codex" /></div>
  </MemoryRouter>);
  return { ...view, footer: view.container.querySelector("[data-progress-usage]") as HTMLElement };
}

it.each([["metered", "$2.00"], ["subscription", "No estimate"], ["unreported", "Unavailable"], ["", "Unavailable"]] as const)(
  "confirmed status %s uses run status and the same total as the panel", (status, headline) => {
    const { footer } = mount([confirmed], status);
    expect(footer.textContent).toContain(headline);
    expect(footer.textContent).toContain("1.0k in (99% cached)");
    expect(footer.textContent).toContain("30 out");
    expect(screen.getByRole("link", { name: "Full usage" }).getAttribute("href")).toBe("#run-full-usage");
    expect(screen.getByRole("group", { name: "Run usage totals" })).toBeTruthy();
    if (status !== "metered") expect(footer.textContent).not.toContain("$");
  });

it.each([false, true])("live-only provisional input, tail %s, never prices live or shows output", withTail => {
  const { footer } = mount([live], "metered", withTail ? tail("unpriced", null) : undefined);
  expect(footer.textContent).toContain("100 in (70% cached) · provisional input");
  expect(footer.textContent).toContain("live, not billed yet");
  expect(footer.textContent).not.toContain(" out");
  expect(footer.textContent).not.toContain("$");
  expect(screen.getByRole("table", { name: "In-flight per-model tokens" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Full usage" })).toBeTruthy();
});

it.each([
  ["metered", "metered", "$0.50"],
  ["subscription", "subscription", "No estimate"],
  ["unreported", "unreported", "Unavailable"],
  ["", "", "Unavailable"],
  ["metered", "unreported", "Unavailable"],
  ["metered", "", "Unavailable"],
] as const)("side statuses %s/%s own the display with no confirmed result", (noteStatus, summaryStatus, expected) => {
  const { footer } = mount([side("progress_note", noteStatus, 3), side("summary_usage", summaryStatus, 4)], "metered");
  expect(footer.textContent).toContain("Summary usage");
  expect(footer.textContent).toContain(expected);
  expect(footer.textContent).toContain("200 in (80% cached)");
  expect(footer.textContent).toContain("20 out");
  expect(screen.getByRole("table", { name: "Summary usage" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Full usage" })).toBeTruthy();
  if (expected === "Unavailable") expect(footer.textContent).not.toContain("$");
});

it.each(["progress_note", "summary_usage"])("single %s side pass records spend while live input stays separate", kind => {
  const { footer } = mount([live, side(kind, "metered", 3)], "unreported");
  expect(footer.textContent).toContain("$0.25");
  expect(footer.textContent).toContain("100 in (80% cached)");
  expect(footer.textContent).toContain("10 out");
  expect(footer.textContent).toContain("100 in (70% cached) · provisional input");
  expect(footer.textContent).not.toContain("999 out");
});

it.each([false, true])("confirmed plus sides counts each once and suppresses live input, live %s", withLive => {
  const { footer } = mount([confirmed, side("progress_note", "metered", 3), side("summary_usage", "metered", 4), ...(withLive ? [live] : [])]);
  expect(footer.textContent).toContain("$2.50");
  expect(footer.textContent).toContain("1.2k in (97% cached)");
  expect(footer.textContent).toContain("50 out");
  expect(footer.textContent).not.toContain("provisional input");
});

it.each([
  ["unpriced", null, "+ unpriced tail"],
  ["estimated", 0.5, "+ ~$0.50 estimated unrecorded"],
  ["estimated", 0.001, "+ <$0.01 estimated unrecorded"],
] as const)("tail-only %s/%s has no panel or Full usage link", (status, dollars, copy) => {
  const { footer, container } = mount([], "metered", tail(status, dollars));
  expect(footer.textContent).toBe(copy);
  expect(screen.queryByRole("link", { name: "Full usage" })).toBeNull();
  expect(container.querySelector("#run-full-usage")?.textContent).toBe("");
});

it.each([
  [false, false, false], [false, false, true], [false, true, false], [false, true, true],
  [true, false, false], [true, false, true], [true, true, false], [true, true, true],
] as const)("unpriced tail and link for confirmed/live/side %s/%s/%s", (hasConfirmed, hasLive, hasSide) => {
  const { footer } = mount([
    ...(hasConfirmed ? [confirmed] : []),
    ...(hasLive ? [live] : []),
    ...(hasSide ? [side("summary_usage", "metered", 3)] : []),
  ], "metered", tail("unpriced", null));
  expect(footer.textContent).toContain("+ unpriced tail");
  expect(footer.textContent).not.toContain("+ ~$0");
  expect(Boolean(screen.queryByRole("link", { name: "Full usage" }))).toBe(hasConfirmed || hasLive || hasSide);
});

it.each([[0, 100, "100"], [100, 0, "0"], [999, 1, "1"]] as const)(
  "live cache fresh=%s cached=%s preserves endpoint honesty", (fresh, cached, pct) => {
    const { footer } = mount([message("text", { model: "test-model", usage: { input_tokens: fresh, cache_read_input_tokens: cached } }, 1)]);
    expect(footer.textContent).toContain(`(${pct}% cached) · provisional input`);
    expect(footer.textContent).not.toContain(" out");
  });

it("empty usage has no footer or link; a complete zero tail stays hidden", () => {
  const { footer } = mount([], "", { ...tail("estimated", 0), input_tokens: 0, output_tokens: 0, coverage: "complete" });
  expect(footer).toBeNull();
  expect(screen.queryByRole("link", { name: "Full usage" })).toBeNull();
});
