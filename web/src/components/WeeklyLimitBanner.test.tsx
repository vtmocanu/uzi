// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render as rtlRender, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { WeeklyLimit } from "../lib/api";
import { WeeklyLimitBanner, weeklyLimitParked } from "./WeeklyLimitBanner";

// The banner links to Settings in its rename and reserve hints.
function render(ui: Parameters<typeof rtlRender>[0]) {
  return rtlRender(<MemoryRouter>{ui}</MemoryRouter>);
}

afterEach(cleanup);

function block(over: Partial<WeeklyLimit> = {}): WeeklyLimit {
  return {
    credential_label: "console-key",
    credential_key: "anthropic:sec-console",
    harness: "claude",
    pct: 97,
    effective_limit: 100,
    resets_at: "2026-11-28T15:00:00Z",
    tier: "warning",
    reason: "headroom_outside_pool",
    alternative_label: "ultra-key",
    future_runs: [],
    fix: [{ kind: "pool_on", target_id: "sec-u", target_label: "ultra-key", command: "uzi token pool --on --kind anthropic -- ultra-key" }],
    wait: "waits",
    ...over,
  };
}

function region() {
  return screen.getByRole("region", { name: "Weekly usage limit" });
}

describe("WeeklyLimitBanner (PRD #4123)", () => {
  it("renders nothing when the run carries no block", () => {
    const { container } = render(<WeeklyLimitBanner block={undefined} parked />);
    expect(container.innerHTML).toBe("");
    const { container: c2 } = render(<WeeklyLimitBanner block={null} parked />);
    expect(c2.innerHTML).toBe("");
  });

  it("warning tier: credential, pct vs limit, reset, reason, wait and a copyable fix", () => {
    render(<WeeklyLimitBanner block={block()} parked />);
    const r = region();
    expect(r.getAttribute("data-weekly-tier")).toBe("warning");
    expect(r.className).toContain("border-warn/40");
    const text = r.textContent ?? "";
    expect(text).toMatch(/^“console-key” is at 97% of its weekly limit \(resets .+\)\./);
    expect(text).toContain("“ultra-key” has weekly headroom but is not in the auto-select pool.");
    expect(text).toContain("then resumes.");
    expect(text).toContain("To change this run:");
    expect(text).toContain("Add “ultra-key” to the auto-select pool");
    const code = r.querySelector("[data-weekly-command]");
    expect(code?.textContent).toBe("uzi token pool --on --kind anthropic -- ultra-key");
    expect(within(r).getByRole("button", { name: "Copy command" })).toBeTruthy();
  });

  it("notice tier: quiet styling, no fix section, future-run advice shown", () => {
    render(
      <WeeklyLimitBanner
        parked
        block={block({
          tier: "notice",
          reason: "account_bound",
          harness: "codex",
          fix: [],
          future_runs: [{ kind: "auto_select", command: "uzi settings codex-mode auto" }],
          wait: "fails",
        })}
      />,
    );
    const r = region();
    expect(r.getAttribute("data-weekly-tier")).toBe("notice");
    expect(r.className).not.toContain("border-warn/40");
    expect(r.className).toContain("border-edge");
    const text = r.textContent ?? "";
    expect(text).not.toContain("To change this run:");
    expect(text).toContain("For runs you start later:");
    expect(r.querySelector("[data-weekly-command]")?.textContent).toBe("uzi settings codex-mode auto");
  });

  it("states unknown usage and unknown reset plainly", () => {
    render(<WeeklyLimitBanner block={block({ pct: null, resets_at: null, effective_limit: 90 })} parked />);
    const text = region().textContent ?? "";
    expect(text).toContain("Weekly usage of “console-key” is unknown (reset time unknown).");
  });

  const reasons: [string, string][] = [
    ["pinned", "This run is pinned to “console-key”."],
    ["not_auto", "This run does not auto-select tokens."],
    ["can_switch", "This run can move to “ultra-key” on its next claim."],
    ["selection_unchanged", "Auto-select still prefers this credential, even though another pooled one is usable."],
    ["headroom_outside_pool", "“ultra-key” has weekly headroom but is not in the auto-select pool."],
    ["pool_exhausted", "Every other pooled token is at its weekly limit or held by the reserve."],
    ["no_alternative", "There is no other usable token to switch to."],
    ["account_bound", "This Codex run stays on the account it was created with."],
    ["switch_pending", "A credential switch is already in progress for this run."],
    ["unknown", "It is not known whether this run can switch."],
    ["some_future_reason", "It is not known whether this run can switch."],
  ];
  for (const [reason, sentence] of reasons) {
    it(`reason ${reason} renders its plain sentence`, () => {
      render(<WeeklyLimitBanner block={block({ reason })} parked />);
      expect(region().textContent).toContain(sentence);
    });
  }

  it("names no alternative when the server sent none", () => {
    render(<WeeklyLimitBanner block={block({ reason: "can_switch", alternative_label: undefined })} parked />);
    expect(region().textContent).toContain("This run can move to another token on its next claim.");
  });

  it("promises a resume only for waits, never for fails or unknown", () => {
    const sentences: Record<WeeklyLimit["wait"], RegExp> = {
      waits: /It waits for the reset on .+, then resumes\./,
      fails: /It fails at the limit instead of waiting for the reset\./,
      reserve_pause: /It stays paused until a fresh reading shows usage below your reserve, or until you resume it\./,
      unknown: /Whether it waits for the reset is not known\./,
    };
    for (const wait of Object.keys(sentences) as WeeklyLimit["wait"][]) {
      const { unmount } = render(<WeeklyLimitBanner block={block({ wait })} parked />);
      const text = region().textContent ?? "";
      expect(text).toMatch(sentences[wait]);
      if (wait !== "waits") expect(text).not.toMatch(/resumes/);
      unmount();
    }
  });

  it("renders a hostile label literally, as text, in every label slot", () => {
    // Assembled at runtime: quotes, backticks and command substitution.
    const hostile = "o'k\"" + "`id`" + "$" + "(touch /tmp/x)" + "<b>x</b>";
    const { container } = render(
      <WeeklyLimitBanner
        parked
        block={block({
          credential_label: hostile,
          alternative_label: hostile,
          reason: "headroom_outside_pool",
          fix: [{ kind: "set_token", target_id: "s", target_label: hostile, command_unavailable: "label" }],
        })}
      />,
    );
    const text = region().textContent ?? "";
    expect(text).toContain(`“${hostile}” is at 97%`);
    expect(text).toContain(`“${hostile}” has weekly headroom`);
    expect(text).toContain(`Switch this run to “${hostile}”`);
    // No markup leaked from the label.
    expect(container.querySelector("b")).toBeNull();
  });

  it("shows the rename hint instead of a command when the label is not command-safe", () => {
    render(
      <WeeklyLimitBanner
        parked
        block={block({ fix: [{ kind: "pool_on", target_id: "s", target_label: "bad", command_unavailable: "label" }] })}
      />,
    );
    const r = region();
    expect(r.textContent).toContain("Rename this token in Settings to get a copyable command");
    expect(r.querySelector("[data-weekly-command]")).toBeNull();
    expect(within(r).queryByRole("button", { name: "Copy command" })).toBeNull();
    expect(within(r).getByRole("link", { name: "Rename this token in Settings" }).getAttribute("href")).toBe("/settings");
  });

  it("points a Settings-only reserve fix at Run defaults", () => {
    render(<WeeklyLimitBanner block={block({ wait: "reserve_pause", fix: [{ kind: "edit_reserve" }] })} parked />);
    const r = region();
    expect(r.textContent).toContain("Change your weekly reserve");
    expect(within(r).getByRole("link", { name: "Open Run defaults" }).getAttribute("href")).toBe("/settings/run-defaults");
  });

  it("headline: reached at 100, and a reserve named as the owner's threshold", () => {
    const { unmount } = render(<WeeklyLimitBanner block={block({ pct: 100 })} parked />);
    expect(region().textContent).toMatch(/^“console-key” has reached its weekly limit \(resets .+\)\./);
    unmount();
    const r2 = render(<WeeklyLimitBanner block={block({ pct: 76, effective_limit: 90 })} parked={false} />);
    expect(region().textContent).toMatch(/^“console-key” is at 76% of its weekly allowance; your reserve holds runs at 90% \(resets .+\)\./);
    r2.unmount();
    render(<WeeklyLimitBanner block={block({ pct: 92, effective_limit: 90, resets_at: null })} parked />);
    expect(region().textContent).toContain("“console-key” is at 92% of its weekly allowance, past your 90% reserve (reset time unknown).");
  });

  // PRD #4123 M2, example 1: a near-limit run that is still running.
  it("a running near-limit run reads as a warning ahead of the limit, with a conditional wait", () => {
    render(<WeeklyLimitBanner block={block({
          pct: 86,
          credential_label: "A",
          alternative_label: "B",
          fix: [{ kind: "pool_on", target_id: "sec-b", target_label: "B", command: "uzi token pool --on --kind anthropic -- B" }],
        })} parked={false} />);
    const text = region().textContent ?? "";
    expect(text).toMatch(/^“A” is at 86% of its weekly limit \(resets .+\)\./);
    expect(text).toContain("“B” has weekly headroom but is not in the auto-select pool.");
    expect(text).toMatch(/If it reaches the limit, it waits for the reset on .+, then resumes\./);
    expect(text).toContain("Add “B” to the auto-select pool");
  });

  it("not parked: every wait line is conditional, and only waits mentions a resume", () => {
    const sentences: Record<WeeklyLimit["wait"], RegExp> = {
      waits: /If it reaches the limit, it waits for the reset on .+, then resumes\./,
      fails: /If it reaches the limit, it fails instead of waiting for the reset\./,
      reserve_pause: /At your reserve it pauses after its current milestone, until a fresh reading shows usage below the reserve or you resume it\./,
      unknown: /Whether it waits for the reset at the limit is not known\./,
    };
    for (const wait of Object.keys(sentences) as WeeklyLimit["wait"][]) {
      const { unmount } = render(<WeeklyLimitBanner block={block({ wait, pct: 86 })} parked={false} />);
      const text = region().textContent ?? "";
      expect(text).toMatch(sentences[wait]);
      expect(text).not.toMatch(/^It (waits|fails|stays)/m);
      if (wait !== "waits") expect(text).not.toMatch(/resumes/);
      unmount();
    }
  });

  it("weeklyLimitParked is the seven_day limit_wait or the weekly-reserve pause only", () => {
    expect(weeklyLimitParked({ status: "limit_wait", rate_limit_type: "seven_day" })).toBe(true);
    expect(weeklyLimitParked({ status: "paused", hold_reason: "weekly_reserve" })).toBe(true);
    expect(weeklyLimitParked({ status: "limit_wait", rate_limit_type: "five_hour" })).toBe(false);
    expect(weeklyLimitParked({ status: "paused", hold_reason: null })).toBe(false);
    expect(weeklyLimitParked({ status: "running" })).toBe(false);
    expect(weeklyLimitParked({ status: "queued" })).toBe(false);
  });
});
