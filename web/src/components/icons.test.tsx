// @vitest-environment jsdom
//
// The provider logos (PRD #1653 D-W1): filled brand glyphs drawn from their own source
// viewBoxes, always aria-hidden (the surrounding accessible name or title carries the
// provider word). The OpenAI mark draws in currentColor so it follows the theme's text
// colour; the Claude mark carries its fixed brand hue through a token class.
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { ClaudeIcon, OpenAIIcon } from "./icons";

afterEach(cleanup);

describe("ClaudeIcon", () => {
  it("renders an aria-hidden svg on the simple-icons 24x24 viewBox with one filled path", () => {
    const { container } = render(<ClaudeIcon />);
    const svg = container.querySelector("svg");
    expect(svg).not.toBeNull();
    expect(svg?.getAttribute("viewBox")).toBe("0 0 24 24");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
    expect(svg?.querySelectorAll("path")).toHaveLength(1);
  });

  it("keeps a caller's size classes alongside its own", () => {
    const { container } = render(<ClaudeIcon className="h-4 w-4" />);
    expect(container.querySelector("svg")?.getAttribute("class")).toContain("h-4 w-4");
  });
});

describe("OpenAIIcon", () => {
  it("renders an aria-hidden svg on the Bootstrap Icons 16x16 viewBox", () => {
    const { container } = render(<OpenAIIcon />);
    const svg = container.querySelector("svg");
    expect(svg).not.toBeNull();
    expect(svg?.getAttribute("viewBox")).toBe("0 0 16 16");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
    expect(svg?.querySelectorAll("path")).toHaveLength(1);
  });

  it("fills with currentColor, so it takes the surrounding text colour", () => {
    const { container } = render(<OpenAIIcon />);
    expect(container.querySelector("svg")?.getAttribute("fill")).toBe("currentColor");
  });
});
