// @vitest-environment jsdom
//
// HarnessBadge (PRD #1429 M4a, reworked by PRD #1653 D-W5): every run carries a round
// provider-logo chip, Claude included. The chip is role="img" named "Claude" / "Codex"
// with a "Runs on …" title; the logo inside is aria-hidden. The retired "Codex" text
// badge is gone.
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { HarnessBadge } from "./HarnessBadge";

afterEach(cleanup);

describe("HarnessBadge", () => {
  it("renders a Claude chip for a claude run", () => {
    render(<HarnessBadge harness="claude" />);
    const chip = screen.getByRole("img", { name: "Claude" });
    expect(chip.getAttribute("title")).toBe("Runs on Claude");
    const logo = chip.querySelector("svg");
    expect(logo?.getAttribute("viewBox")).toBe("0 0 24 24");
    expect(logo?.getAttribute("aria-hidden")).toBe("true");
    expect(screen.queryByRole("img", { name: "Codex" })).toBeNull();
  });

  it("renders a Codex chip with the OpenAI logo for a codex run", () => {
    const { container } = render(<HarnessBadge harness="codex" />);
    const chip = screen.getByRole("img", { name: "Codex" });
    expect(chip.getAttribute("title")).toBe("Runs on Codex");
    const logo = chip.querySelector("svg");
    expect(logo?.getAttribute("viewBox")).toBe("0 0 16 16");
    expect(logo?.getAttribute("aria-hidden")).toBe("true");
    // No visible provider word: the old "Codex" text badge is gone.
    expect(container.textContent).toBe("");
    expect(screen.queryByText("Codex")).toBeNull();
  });

  it("renders nothing for a null/undefined harness (fail-safe, not a false provider claim)", () => {
    expect(render(<HarnessBadge harness={null} />).container.innerHTML).toBe("");
    expect(render(<HarnessBadge harness={undefined} />).container.innerHTML).toBe("");
  });
});
