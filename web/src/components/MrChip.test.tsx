// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MrChip } from "./MrChip";

afterEach(cleanup);

const HREF = "https://gitlab.example.com/g/r/-/merge_requests/7";

describe("MrChip pill (board card)", () => {
  it("open: renders '!N' with no state label and links when href is set", () => {
    render(<MrChip mrIid={7} mrState="open" href={HREF} />);
    const link = screen.getByRole("link");
    expect(link.getAttribute("href")).toBe(HREF);
    expect(link.textContent).toBe("!7");
    expect(link.textContent).not.toContain("merged");
    expect(link.textContent).not.toContain("closed");
  });

  it("merged: ok-toned, links, and the state is part of the accessible name", () => {
    render(<MrChip mrIid={7} mrState="merged" href={HREF} />);
    const link = screen.getByRole("link");
    // The "merged" word lives INSIDE the link, so its accessible name is "!7 merged".
    expect(link.textContent).toBe("!7 merged");
    expect(link.getAttribute("title")).toContain("merged");
    expect(link.className).toContain("text-ok");
  });

  it("closed: muted (AA-contrast text-muted, NOT text-faint), struck number, 'closed' label", () => {
    render(<MrChip mrIid={7} mrState="closed" href={HREF} />);
    const link = screen.getByRole("link");
    expect(link.textContent).toBe("!7 closed");
    expect(link.className).toContain("text-muted");
    expect(link.className).not.toContain("text-faint");
    expect(link.querySelector(".line-through")?.textContent).toBe("!7");
  });

  it("renders a plain span (no link) when href is null", () => {
    render(<MrChip mrIid={9} mrState="open" href={null} />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("!9")).toBeTruthy();
  });
});

describe("MrChip inline (meta-line surfaces)", () => {
  it("open keeps the surface's base tone (brand for the issue-history link)", () => {
    render(<MrChip variant="inline" openTone="brand" mrIid={7} mrState="open" href={HREF} />);
    const link = screen.getByRole("link");
    expect(link.className).toContain("text-brand");
    expect(link.className).not.toContain("rounded"); // no pill border/box
    expect(link.textContent).toBe("!7");
  });

  it("merged is ok-toned even when open would be brand (consistent green everywhere)", () => {
    render(<MrChip variant="inline" openTone="brand" mrIid={7} mrState="merged" href={HREF} />);
    const link = screen.getByRole("link");
    expect(link.className).toContain("text-ok");
    expect(link.className).not.toContain("text-brand");
    expect(link.textContent).toBe("!7 merged"); // state inside the link
  });

  it("closed is muted + struck with the state inside the accessible name", () => {
    render(<MrChip variant="inline" openTone="brand" mrIid={7} mrState="closed" href={HREF} />);
    const link = screen.getByRole("link");
    expect(link.className).toContain("text-muted");
    expect(link.textContent).toBe("!7 closed");
    expect(link.querySelector(".line-through")?.textContent).toBe("!7");
  });

  it("non-link inline (href null) renders the label + number + state as plain text", () => {
    const { container } = render(<MrChip variant="inline" label="MR " mrIid={5} mrState="merged" href={null} />);
    expect(container.querySelector("a")).toBeNull();
    expect(container.textContent).toBe("MR !5 merged");
  });
});
