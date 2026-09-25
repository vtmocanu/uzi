// @vitest-environment jsdom
//
// PRD #1648 D6: the shared severity marks. Severity is asserted by FORM (which SVG shape is
// drawn) and by WORD (the visible text), never by a colour class, and no font glyph may
// leak into the rendered text.
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { SeverityBadge, SeverityShape } from "./healthSeverity";

afterEach(cleanup);

// The old font glyphs, built from code points so this file carries none of them literally.
const GLYPHS = [0x25cf, 0x25b2, 0x25c6, 0x25cb].map((c) => String.fromCodePoint(c));

function noGlyphs(text: string | null) {
  for (const g of GLYPHS) expect(text ?? "").not.toContain(g);
}

// What the drawn shape is, read off the SVG's single child: element, fill, dash.
function shapeOf(svg: SVGSVGElement | null) {
  if (!svg) throw new Error("no svg rendered");
  const el = svg.firstElementChild;
  if (!el) throw new Error("empty svg");
  return {
    tag: el.tagName.toLowerCase(),
    filled: el.getAttribute("fill") === "currentColor",
    dashed: el.hasAttribute("stroke-dasharray"),
    points: el.getAttribute("points")?.split(" ").length ?? 0,
  };
}

describe("SeverityBadge", () => {
  const cases = [
    { sev: "ok", word: "OK", shape: { tag: "circle", filled: true, dashed: false, points: 0 } },
    { sev: "warn", word: "Warn", shape: { tag: "polygon", filled: true, dashed: false, points: 3 } },
    { sev: "danger", word: "Danger", shape: { tag: "polygon", filled: true, dashed: false, points: 4 } },
    { sev: "unknown", word: "Unknown", shape: { tag: "circle", filled: false, dashed: false, points: 0 } },
    { sev: "na", word: "N/A", shape: { tag: "circle", filled: false, dashed: true, points: 0 } },
  ];

  for (const c of cases) {
    it(`${c.sev}: renders the word "${c.word}", a decorative ${c.shape.tag} shape, and no font glyph`, () => {
      const { container } = render(<SeverityBadge severity={c.sev} />);
      expect(screen.getByText(c.word)).toBeTruthy();
      const svg = container.querySelector("svg");
      expect(shapeOf(svg)).toEqual(c.shape);
      // The shape sits next to its word, so it is hidden from assistive tech.
      expect(svg?.getAttribute("aria-hidden")).toBe("true");
      expect(container.textContent).toBe(c.word);
      noGlyphs(container.textContent);
    });
  }

  it("prefixes a count to the word when given", () => {
    const { container } = render(<SeverityBadge severity="danger" count={3} />);
    expect(screen.getByText("3 Danger")).toBeTruthy();
    noGlyphs(container.textContent);
  });

  it("renders an unrecognised severity as Unknown, never as OK", () => {
    render(<SeverityBadge severity="bogus" />);
    expect(screen.getByText("Unknown")).toBeTruthy();
    expect(screen.queryByText("OK")).toBeNull();
  });
});

describe("SeverityShape", () => {
  it("is decorative by default and an img with the given name when labelled", () => {
    const { container } = render(<SeverityShape severity="danger" />);
    expect(container.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
    cleanup();
    render(<SeverityShape severity="warn" label="Warn" />);
    expect(screen.getByRole("img", { name: "Warn" })).toBeTruthy();
  });

  it("draws the quiet OK check mark (an unfilled path) for variant=check, at a given size", () => {
    const { container } = render(<SeverityShape severity="ok" variant="check" size={18} />);
    const svg = container.querySelector("svg");
    expect(svg?.firstElementChild?.tagName.toLowerCase()).toBe("path");
    expect(svg?.firstElementChild?.getAttribute("fill")).toBe("none");
    expect(svg?.getAttribute("width")).toBe("18");
  });
});
