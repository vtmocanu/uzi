// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DocMarkdown } from "./DocMarkdown";
import { Markdown } from "./Markdown";

afterEach(cleanup);

it("assigns a plain h2 ID to rendered trusted docs", () => {
  render(<MemoryRouter><DocMarkdown content={"## The forge screens: `pulls` and `ci`"} isAdmin={false} /></MemoryRouter>);
  expect(screen.getByRole("heading", { level: 2 }).id).toBe("the-forge-screens-pulls-and-ci");
});

it("leaves a rendered untrusted Markdown heading without an ID", () => {
  render(<Markdown content="## The rule" />);
  expect(screen.getByRole("heading", { name: "The rule", level: 2 }).hasAttribute("id")).toBe(false);
});

it("renders all heading levels, inline text, collisions, and resets on rerender", () => {
  const content = "# x\n\n## x\n\n### x-1\n\n#### A *bold* [link](https://example.com) `code` ![excluded](img.png)\n\n##### Five\n\n###### Six";
  const { rerender } = render(<MemoryRouter><DocMarkdown content={content} isAdmin={false} /></MemoryRouter>);
  expect(screen.getAllByRole("heading").map((h) => [h.tagName, h.id])).toEqual([
    ["H1", "x"], ["H2", "x-1"], ["H3", "x-1-1"], ["H4", "a-bold-link-code-"], ["H5", "five"], ["H6", "six"],
  ]);
  rerender(<MemoryRouter><DocMarkdown content={"## x\n\n## x"} isAdmin={false} /></MemoryRouter>);
  expect(screen.getAllByRole("heading").map((h) => h.id)).toEqual(["x", "x-1"]);
});
