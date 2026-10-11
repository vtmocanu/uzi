// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { createElement } from "react";
import { rehypeHeadingIds } from "./rehypeHeadingIds";

afterEach(cleanup);
const plugins = [rehypeHeadingIds];
function markdown(content: string) {
  return createElement(ReactMarkdown, { remarkPlugins: [remarkGfm], rehypePlugins: plugins }, content);
}

it("assigns all levels using descendant inline text and excluding image alt", () => {
  render(markdown("# One\n\n## Two *emphasis* [link](https://example.com) `code` ![alt](img.png)\n\n### Three\n\n#### Four\n\n##### Five\n\n###### Six"));
  expect(screen.getAllByRole("heading").map((h) => [h.tagName, h.id])).toEqual([
    ["H1", "one"], ["H2", "two-emphasis-link-code-"], ["H3", "three"],
    ["H4", "four"], ["H5", "five"], ["H6", "six"],
  ]);
});

it("handles cross collisions in document order and resets with a stable plugin on rerender", () => {
  const { rerender } = render(markdown("# x\n\n## x\n\n### x-1"));
  expect(screen.getAllByRole("heading").map((h) => h.id)).toEqual(["x", "x-1", "x-1-1"]);
  rerender(markdown("# x\n\n## x\n\n### x"));
  expect(screen.getAllByRole("heading").map((h) => h.id)).toEqual(["x", "x-1", "x-2"]);
});

it("preserves heading properties while assigning a plain ID", () => {
  function headingProperties() {
    return (tree: { children?: { properties?: Record<string, unknown> }[] }) => {
      tree.children![0].properties = { title: "kept", className: ["existing"], id: "replaced" };
    };
  }
  const { container } = render(createElement(ReactMarkdown, {
    rehypePlugins: [headingProperties, rehypeHeadingIds],
  }, "# Heading"));
  const heading = container.querySelector("h1")!;
  expect(heading.id).toBe("heading");
  expect(heading.title).toBe("kept");
  expect(heading.className).toBe("existing");
});
