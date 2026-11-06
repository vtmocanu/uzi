// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunHarnessDecision } from "./RunHarnessDecision";
import { RunCredential } from "./RunCredential";
import type { HarnessDecision } from "../lib/apiTypes";

afterEach(cleanup);
const decision: HarnessDecision = {
  preferred: "claude", chosen: "codex", reason: "overflow",
  preferred_score: 0, other_score: 60, preferred_measured: 12, other_measured: 60,
  preferred_held: true, other_held: false,
  preferred_weekly_reset_at: null, other_weekly_reset_at: null,
};
const auto: HarnessDecision = {
  preferred: "claude", chosen: "codex", reason: "auto_soonest_reset",
  preferred_score: 70, other_score: 80, preferred_measured: 70, other_measured: 80,
  preferred_held: false, other_held: false,
  preferred_weekly_reset_at: "2026-11-09T10:00:00Z", other_weekly_reset_at: "2026-11-06T08:30:00Z",
};

describe("stored harness decision", () => {
  it("shows overflow and distinguishes comparison, measurement and capacity holds", () => {
    render(<RunHarnessDecision run={{ harness_decision: decision }} />);
    const region = screen.getByRole("region", { name: "Harness decision at creation time" });
    expect(region.textContent).toContain("preferred claude, chosen codex. Reason: overflow");
    expect(region.textContent).toContain("Preferred comparison score: 0%; measured headroom: 12%; capacity hold: yes");
    expect(region.textContent).toContain("Other harness comparison score: 60%; measured headroom: 60%; capacity hold: no");
    expect(region.textContent).toContain("Stored when this run was created, not live usage");
  });
  it.each(["preferred", "unknown_usage"])("shows a retained preferred harness and %s reason without inventing readings", reason => {
    render(<RunHarnessDecision run={{ harness_decision: {
      ...decision, chosen: "claude", reason, preferred_score: null, other_score: null,
      preferred_measured: null, other_measured: null, preferred_held: false,
    } }} />);
    expect(screen.getByText(/preferred claude, chosen claude/).textContent).toContain(`Reason: ${reason}`);
    expect(screen.getByText(/Preferred comparison score/).textContent).toContain("unknown; measured headroom: unknown; capacity hold: no");
    expect(screen.getByText(/Other harness comparison score/).textContent).toContain("unknown; measured headroom: unknown; capacity hold: no");
  });
  it("shows a hold without claiming a measured reading", () => {
    render(<RunHarnessDecision run={{ harness_decision: { ...decision, preferred_measured: null } }} />);
    expect(screen.getByText(/Preferred comparison score/).textContent).toContain("0%; measured headroom: unknown; capacity hold: yes");
  });
  it("explains an automatic soonest-reset choice and labels the evidence rows Claude and Codex", () => {
    render(<RunHarnessDecision run={{ harness_decision: auto }} />);
    const region = screen.getByRole("region", { name: "Harness decision at creation time" });
    expect(region.textContent).toContain("Chosen per run by weekly limits: Codex's weekly allowance resets sooner than Claude's");
    expect(region.textContent).toContain("automatic (Default · automatic), chosen codex. Reason: auto_soonest_reset");
    expect(region.textContent).not.toContain("preferred claude");
    expect(region.textContent).toContain("Claude comparison score: 70%; measured headroom: 70%; capacity hold: no; weekly reset: 2026-11-09 10:00 UTC");
    expect(region.textContent).toContain("Codex comparison score: 80%; measured headroom: 80%; capacity hold: no; weekly reset: 2026-11-06 08:30 UTC");
    expect(region.textContent).not.toContain("Preferred comparison score");
    expect(region.textContent).not.toContain("Other harness comparison score");
    expect(region.textContent).toContain("Stored when this run was created, not live usage");
  });
  it.each([
    ["auto_only_qualified", "codex", "only Codex had enough headroom"],
    ["auto_fallback", "claude", "Claude used: usage was unknown"],
  ])("explains %s in plain language", (reason, chosen, text) => {
    render(<RunHarnessDecision run={{ harness_decision: { ...auto, reason, chosen } }} />);
    expect(screen.getByRole("region", { name: "Harness decision at creation time" }).textContent).toContain(text);
  });
  it.each([
    ["both resets missing", null, null],
    ["equal resets", "2026-11-06T08:30:00Z", "2026-11-06T08:30:00Z"],
    ["only the losing side known", null, "2026-11-06T08:30:00Z"],
    ["the chosen side resetting later", "2026-11-10T08:30:00Z", "2026-11-06T08:30:00Z"],
  ])("uses neutral wording, not 'resets sooner', for %s", (_name, codexReset, claudeReset) => {
    render(<RunHarnessDecision run={{ harness_decision: {
      ...auto, chosen: "codex", preferred_weekly_reset_at: claudeReset, other_weekly_reset_at: codexReset,
    } }} />);
    const text = screen.getByRole("region", { name: "Harness decision at creation time" }).textContent ?? "";
    expect(text).toContain("Both accounts had headroom; Codex chosen (no earlier known weekly reset, so more headroom, then Claude, decides)");
    expect(text).not.toContain("resets sooner");
  });
  it("says only the chosen side had a known reset when the other side's is missing", () => {
    render(<RunHarnessDecision run={{ harness_decision: {
      ...auto, chosen: "codex", preferred_weekly_reset_at: null, other_weekly_reset_at: "2026-11-06T08:30:00Z",
    } }} />);
    const text = screen.getByRole("region", { name: "Harness decision at creation time" }).textContent ?? "";
    expect(text).toContain("Chosen per run by weekly limits: only Codex had a known upcoming weekly reset");
    expect(text).not.toContain("more headroom");
  });
  it("says resets sooner when Claude is chosen with a strictly earlier known reset", () => {
    render(<RunHarnessDecision run={{ harness_decision: {
      ...auto, chosen: "claude", preferred_weekly_reset_at: "2026-11-05T08:30:00Z", other_weekly_reset_at: "2026-11-06T08:30:00Z",
    } }} />);
    expect(screen.getByRole("region", { name: "Harness decision at creation time" }).textContent)
      .toContain("Claude's weekly allowance resets sooner than Codex's");
  });
  it("shows unknown weekly resets for rows created before they were recorded", () => {
    render(<RunHarnessDecision run={{ harness_decision: { ...auto, preferred_weekly_reset_at: null, other_weekly_reset_at: null } }} />);
    const text = screen.getByRole("region", { name: "Harness decision at creation time" }).textContent;
    expect(text).toContain("Claude comparison score: 70%; measured headroom: 70%; capacity hold: no; weekly reset: unknown");
    expect(text).toContain("Codex comparison score: 80%; measured headroom: 80%; capacity hold: no; weekly reset: unknown");
  });
  it("keeps Preferred and Other harness labels for non-automatic reasons", () => {
    render(<RunHarnessDecision run={{ harness_decision: decision }} />);
    const text = screen.getByRole("region", { name: "Harness decision at creation time" }).textContent;
    expect(text).toContain("Preferred comparison score");
    expect(text).toContain("weekly reset: unknown");
  });
  it("labels legacy or redacted decisions as not recorded", () => {
    render(<RunHarnessDecision run={{ harness_decision: null }} />);
    expect(screen.getByText(/Harness decision at creation time: not recorded/).textContent).toContain("legacy or unavailable");
  });
  it("remains visible beside a queued run with no credential label", () => {
    render(<><RunCredential run={{ harness: "codex", codex_secret_id: null, codex_secret_label: null,
      codex_select_reason: null, anthropic_secret_id: null, anthropic_secret_label: null,
      anthropic_select_reason: null, anthropic_headroom_pct: null }} />
      <RunHarnessDecision run={{ harness_decision: decision }} /></>);
    expect(screen.getByRole("region", { name: "Harness decision at creation time" })).toBeTruthy();
  });
  it("removes complete terminal sequences from display text", () => {
    render(<RunHarnessDecision run={{ harness_decision: {
      ...decision, preferred: "\x1b[31mclaude\x1b[0m",
      reason: "\x1b]8;;https://invalid.example\u0007overflow\x1b]8;;\u0007",
    } }} />);
    const region = screen.getByRole("region", { name: "Harness decision at creation time" });
    expect(region.textContent).toContain("preferred claude, chosen codex. Reason: overflow.");
    expect(region.textContent).not.toContain("invalid.example");
    expect(region.textContent).not.toContain("[31m");
  });
  it("escapes markup and strips bidi and controls from stored strings", () => {
    const { container } = render(<RunHarnessDecision run={{ harness_decision: {
      ...decision, preferred: "<script>bad</script>\u202e", chosen: "codex\u0007", reason: "<img src=x>\u2066",
    } }} />);
    expect(container.querySelector("script, img")).toBeNull();
    expect(container.textContent).toContain("<script>bad</script>, chosen codex. Reason: <img src=x>.");
    for (const character of ["\u202e", "\u0007", "\u2066"]) expect(container.textContent).not.toContain(character);
  });
});
