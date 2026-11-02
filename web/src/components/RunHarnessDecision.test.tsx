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
