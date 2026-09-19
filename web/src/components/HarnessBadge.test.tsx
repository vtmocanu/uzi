// @vitest-environment jsdom
//
// HarnessBadge (PRD #1429 M4a): Claude stays visually unmarked (renders nothing);
// Codex is explicit (renders a small badge naming it).
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { HarnessBadge } from "./HarnessBadge";

afterEach(cleanup);

describe("HarnessBadge", () => {
  it("renders nothing for claude (unmarked — adds no information)", () => {
    const { container } = render(<HarnessBadge harness="claude" />);
    expect(container.textContent).toBe("");
  });

  it("renders nothing for a null/undefined harness (fail-safe, not a false Codex claim)", () => {
    expect(render(<HarnessBadge harness={null} />).container.textContent).toBe("");
    expect(render(<HarnessBadge harness={undefined} />).container.textContent).toBe("");
  });

  it("renders an explicit Codex badge", () => {
    render(<HarnessBadge harness="codex" />);
    expect(screen.getByText("Codex")).toBeTruthy();
  });

  it("carries the caller's title on the Codex badge", () => {
    render(<HarnessBadge harness="codex" title="Pinned to Codex" />);
    expect(screen.getByText("Codex").closest("[title]")?.getAttribute("title")).toBe("Pinned to Codex");
  });
});
