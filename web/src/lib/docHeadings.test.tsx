// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DocMarkdown } from "../components/DocMarkdown";
import { docHeadingsFor } from "./docHeadings";
import { docsForIndex, getDoc } from "./docs";

afterEach(cleanup);

describe("build-time doc heading index", () => {
  it("has an entry for every viewer-readable doc", () => {
    for (const d of docsForIndex(true)) {
      expect(docHeadingsFor(d.slug).length, d.slug).toBeGreaterThan(0);
    }
  });

  it("getting-started ids equal the rendered ids", () => {
    const body = getDoc("getting-started")!.body;
    const { container } = render(
      <MemoryRouter>
        <DocMarkdown content={body} isAdmin={false} />
      </MemoryRouter>,
    );
    const rendered = [...container.querySelectorAll("h1,h2,h3,h4,h5,h6")].map((h) => ({
      depth: Number(h.tagName[1]),
      text: h.textContent ?? "",
      id: h.getAttribute("id") ?? "",
    }));
    expect(docHeadingsFor("getting-started")).toEqual(rendered);
  });
});
