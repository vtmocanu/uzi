// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DocMarkdown } from "../components/DocMarkdown";
import { MarkdownCore } from "../components/MarkdownCore";
import { Markdown } from "../components/Markdown";
import { collectDocHeadings } from "./docHeadingsPipeline";

afterEach(cleanup);

function rendered(body: string) {
  const { container } = render(
    <MemoryRouter>
      <DocMarkdown content={body} isAdmin={false} />
    </MemoryRouter>,
  );
  return [...container.querySelectorAll("h1,h2,h3,h4,h5,h6")].map((h) => ({
    depth: Number(h.tagName[1]),
    text: h.textContent ?? "",
    id: h.getAttribute("id") ?? "",
  }));
}

const fixtures: Record<string, string> = {
  duplicate: "## x\n\n## x\n",
  collision: "# Foo\n\n## Foo\n",
  inline: "## Use `task` **now** [link](/a)\n",
  blockquote: "> ## Quoted\n",
  setext: "Title\n=====\n\nSub\n---\n",
  fenced: "```\n## not a heading\n```\n\n## real\n",
};

describe("collectDocHeadings matches the DocMarkdown render", () => {
  for (const [name, body] of Object.entries(fixtures)) {
    it(name, () => {
      const got = collectDocHeadings(body);
      expect(got.length).toBeGreaterThan(0);
      expect(got).toEqual(rendered(body));
    });
  }

  it("slugs duplicates and collisions", () => {
    expect(collectDocHeadings(fixtures.duplicate).map((h) => h.id)).toEqual(["x", "x-1"]);
    expect(collectDocHeadings(fixtures.fenced).map((h) => h.text)).toEqual(["real"]);
  });
});

describe("untrusted markdown never gets heading ids", () => {
  it("MarkdownCore", () => {
    const { container } = render(<MarkdownCore content="## x" />);
    const h = container.querySelector("h2");
    expect(h).not.toBeNull();
    expect(h?.hasAttribute("id")).toBe(false);
  });
  it("Markdown wrapper", () => {
    const { container } = render(<Markdown content="## x" />);
    const h = container.querySelector("h2");
    expect(h).not.toBeNull();
    expect(h?.hasAttribute("id")).toBe(false);
  });
});
