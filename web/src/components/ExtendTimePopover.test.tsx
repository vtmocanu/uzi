// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ExtendTimePopover } from "./ExtendTimePopover";
import type { BudgetRun } from "../lib/budget";

afterEach(() => cleanup());

// A running run 1h05m from its deadline, 8h budget, 16h allowance and no extension yet — the
// mock frame B shape. Tests override the extension/cap to exercise the allowance guard.
function runningRun(over: Partial<BudgetRun> = {}): BudgetRun {
  return {
    status: "running",
    started_at: new Date(Date.now() - (8 * 3600 - 65 * 60) * 1000).toISOString(),
    deadline_at: new Date(Date.now() + 65 * 60 * 1000).toISOString(),
    budget_wall_seconds: 28800,
    budget_total_seconds: 28800,
    budget_used_seconds: 28800 - 65 * 60,
    budget_extension_seconds: 0,
    budget_extension_cap_seconds: 57600,
    ...over,
  };
}

function open() {
  fireEvent.click(screen.getByRole("button", { name: /extend time/i }));
  return screen.getByRole("dialog", { name: /extend this run/i });
}

describe("ExtendTimePopover (PRD #1189)", () => {
  it("opens the chooser with +2h preselected and the facts, never adding time on the first click", () => {
    const onSubmit = vi.fn();
    render(<ExtendTimePopover run={runningRun()} busy={false} onSubmit={onSubmit} />);
    // The trigger opening the chooser is NOT a submit (Decision 5).
    const panel = open();
    expect(onSubmit).not.toHaveBeenCalled();
    // Facts are present…
    expect(within(panel).getByText(/Used/)).toBeTruthy();
    expect(within(panel).getByText(/Allowance left/)).toBeTruthy();
    // …and +2h is the preselected Confirm amount.
    expect(within(panel).getByRole("button", { name: /Extend by 2h/ })).toBeTruthy();
  });

  it("posts the chosen seconds on Confirm (default +2h → 7200)", () => {
    const onSubmit = vi.fn();
    render(<ExtendTimePopover run={runningRun()} busy={false} onSubmit={onSubmit} />);
    open();
    fireEvent.click(screen.getByRole("button", { name: /Extend by 2h/ }));
    expect(onSubmit).toHaveBeenCalledWith(7200);
  });

  it("a custom duration overrides the quick pick and posts its seconds", () => {
    const onSubmit = vi.fn();
    render(<ExtendTimePopover run={runningRun()} busy={false} onSubmit={onSubmit} />);
    const panel = open();
    fireEvent.change(within(panel).getByPlaceholderText(/custom/i), { target: { value: "90m" } });
    fireEvent.click(within(panel).getByRole("button", { name: /Extend by 1h 30m/ }));
    expect(onSubmit).toHaveBeenCalledWith(5400);
  });

  it("disables Confirm and names the allowance when the pick exceeds cap − extension", () => {
    const onSubmit = vi.fn();
    // 16h cap, 15h already granted → only 1h of allowance remains; the +2h default is over it.
    render(
      <ExtendTimePopover
        run={runningRun({ budget_extension_seconds: 54000, budget_total_seconds: 28800 + 54000 })}
        busy={false}
        onSubmit={onSubmit}
      />,
    );
    const panel = open();
    const confirm = within(panel).getByRole("button", { name: /Extend by 2h/ }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    // The reason names the remaining allowance (1h) AND the cap (16h).
    const reason = within(panel).getByText(/more than the 1h left of this run's 16h extension allowance/i);
    expect(reason).toBeTruthy();
    fireEvent.click(confirm);
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("says extensions are turned off when the cap is 0, and offers no Confirm", () => {
    const onSubmit = vi.fn();
    render(
      <ExtendTimePopover
        run={runningRun({ budget_extension_cap_seconds: 0 })}
        busy={false}
        onSubmit={onSubmit}
      />,
    );
    const panel = open();
    expect(within(panel).getByText(/extensions are turned off/i)).toBeTruthy();
    expect(within(panel).queryByRole("button", { name: /Extend by/ })).toBeNull();
  });
});
