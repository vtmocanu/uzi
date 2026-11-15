// @vitest-environment jsdom
// #4335: the Overview Agents chip and its popover.
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AgentsChip } from "./AgentsChip";
import type { AgentCountModel } from "../lib/agentCounts";

afterEach(cleanup);

const model: AgentCountModel = {
  total: 14,
  inRuns: 9,
  byScope: [
    { scope: "builtin", label: "Builtin", total: 12, inRuns: 8 },
    { scope: "global", label: "Global", total: 1, inRuns: 1 },
    { scope: "user", label: "Mine", total: 1, inRuns: 0 },
  ],
};

function mount(m: AgentCountModel) {
  return render(
    <MemoryRouter>
      <AgentsChip model={m} />
    </MemoryRouter>,
  );
}

const chip = () => screen.getByRole("button", { name: /^Agents:/ });

describe("AgentsChip", () => {
  it("reads the agents in your runs of the total, with a matching accessible name", () => {
    mount(model);
    expect(chip().getAttribute("aria-label")).toBe("Agents: 9 in your runs of 14");
    expect(chip().textContent).toContain("9 agents in your runs");
    expect(chip().textContent).toContain("of 14");
    expect(chip().getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("uses the singular for one agent", () => {
    mount({ total: 3, inRuns: 1, byScope: [{ scope: "builtin", label: "Builtin", total: 3, inRuns: 1 }] });
    expect(chip().textContent).toContain("1 agent in your runs");
    expect(chip().textContent).not.toContain("1 agents");
  });

  it("opens a popover with the header and one row per scope", () => {
    mount(model);
    fireEvent.click(chip());
    const dialog = screen.getByRole("dialog", { name: "Agents in your runs" });
    expect(chip().getAttribute("aria-expanded")).toBe("true");
    expect(within(dialog).getByRole("heading", { name: "9 of 14 agents ride your runs" })).toBeTruthy();
    expect(within(dialog).getByText("By scope")).toBeTruthy();
    expect(within(dialog).getByText("Builtin").nextElementSibling?.textContent).toBe("12 · 8 on");
    expect(within(dialog).getByText("Global").nextElementSibling?.textContent).toBe("1 · 1 on");
    expect(within(dialog).getByText("Mine").nextElementSibling?.textContent).toBe("1 · 0 on");
  });

  it("shows the Admin only group when global defaults are known, and not otherwise", () => {
    const { unmount } = mount({ ...model, globalDefaults: 13 });
    fireEvent.click(chip());
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText("Admin only")).toBeTruthy();
    expect(within(dialog).getByText("Global defaults for new users").nextElementSibling?.textContent).toBe("13");
    unmount();

    mount(model);
    fireEvent.click(chip());
    expect(screen.queryByText("Admin only")).toBeNull();
    expect(screen.queryByText("Global defaults for new users")).toBeNull();
  });

  it("shows a zero global default count for an admin", () => {
    mount({ ...model, globalDefaults: 0 });
    fireEvent.click(chip());
    expect(screen.getByText("Global defaults for new users").nextElementSibling?.textContent).toBe("0");
  });

  it("links to the Agents page and closes when the link is used", () => {
    mount(model);
    fireEvent.click(chip());
    const link = screen.getByRole("link", { name: "Manage agents" });
    expect(link.getAttribute("href")).toBe("/agents");
    fireEvent.click(link);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("closes on Escape and returns focus to the chip", () => {
    mount(model);
    fireEvent.click(chip());
    expect(document.activeElement).toBe(screen.getByRole("dialog"));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(chip());
  });

  it("closes on a click outside", () => {
    mount(model);
    fireEvent.click(chip());
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
