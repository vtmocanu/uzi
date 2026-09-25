// @vitest-environment jsdom
//
// HarnessBadge (PRD #1429 M4a, #1653 D-W5, issue #1681): both providers have
// a default round chip and a bare 28px run-list logo. Both keep role="img",
// provider name, hover title, and an aria-hidden SVG.
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

  it.each(["claude", "codex"] as const)("keeps the default %s chip classes and 13px logo", (harness) => {
    render(<HarnessBadge harness={harness} />);
    const chip = screen.getByRole("img", { name: harness === "claude" ? "Claude" : "Codex" });
    expect(chip.className).toBe("inline-flex h-5 w-5 flex-none items-center justify-center rounded-full border border-edge bg-ink text-fg");
    expect(chip.querySelector("svg")?.getAttribute("class")).toContain("h-[13px] w-[13px]");
  });

  it.each(["claude", "codex"] as const)("renders the bare %s logo with 28px square geometry", (harness) => {
    render(<HarnessBadge harness={harness} variant="bare" />);
    const name = harness === "claude" ? "Claude" : "Codex";
    const logo = screen.getByRole("img", { name });
    expect(logo.getAttribute("title")).toBe(`Runs on ${name}`);
    expect(logo.className).toBe("relative z-10 inline-flex h-7 w-7 flex-none items-center justify-center text-fg");
    expect(logo.className).not.toMatch(/border|rounded|bg-/);
    expect(logo.querySelector("svg")?.getAttribute("class")).toContain("h-[22px] w-[22px]");
    expect(logo.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
  });

  it("renders nothing for a null/undefined harness (fail-safe, not a false provider claim)", () => {
    expect(render(<HarnessBadge harness={null} />).container.innerHTML).toBe("");
    expect(render(<HarnessBadge harness={undefined} />).container.innerHTML).toBe("");
  });
});
