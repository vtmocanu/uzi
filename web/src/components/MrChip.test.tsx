// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MrChip } from "./MrChip";

afterEach(cleanup);

const HREF = "https://gitlab.example.com/g/r/-/merge_requests/7";

describe("MrChip (board card MR pill, PRD #33)", () => {
  it("open: renders '!N' with no state label and links when href is set", () => {
    render(<MrChip mrIid={7} mrState="open" href={HREF} />);
    const link = screen.getByRole("link");
    expect(link.getAttribute("href")).toBe(HREF);
    expect(link.textContent).toBe("!7");
    expect(link.textContent).not.toContain("merged");
    expect(link.textContent).not.toContain("closed");
  });

  it("merged: keeps the link and appends a 'merged' label", () => {
    render(<MrChip mrIid={7} mrState="merged" href={HREF} />);
    const link = screen.getByRole("link");
    expect(link.textContent).toContain("!7");
    expect(link.textContent).toContain("merged");
    expect(link.getAttribute("title")).toContain("merged");
  });

  it("closed: appends a 'closed' label and strikes the number through", () => {
    render(<MrChip mrIid={7} mrState="closed" href={HREF} />);
    const link = screen.getByRole("link");
    expect(link.textContent).toContain("!7");
    expect(link.textContent).toContain("closed");
    // the "!N" span is struck through for a closed MR
    expect(link.querySelector(".line-through")?.textContent).toBe("!7");
  });

  it("renders a plain span (no link) when href is null", () => {
    render(<MrChip mrIid={9} mrState="open" href={null} />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("!9")).toBeTruthy();
  });
});
