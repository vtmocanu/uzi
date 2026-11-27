// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { NAV_SECTIONS } from "../components/navItems";
import { isModuleIconName, MODULE_ICON_NAMES, moduleIcon } from "./icons";

describe("module icons", () => {
  it("offers a fixed, non-empty vocabulary of host glyphs", () => {
    expect(MODULE_ICON_NAMES.length).toBeGreaterThan(10);
    expect(new Set(MODULE_ICON_NAMES).size).toBe(MODULE_ICON_NAMES.length);
  });

  it.each(MODULE_ICON_NAMES)("renders %s as an svg", (name) => {
    expect(isModuleIconName(name)).toBe(true);
    const { container } = render(<>{moduleIcon(name)}</>);
    expect(container.querySelector("svg")).not.toBeNull();
  });

  it("offers chart-line, a glyph distinct from every built-in nav entry (issue #4777)", () => {
    expect(isModuleIconName("chart-line")).toBe(true);
    const chart = renderToStaticMarkup(moduleIcon("chart-line") as ReactElement);
    expect(chart).toContain('d="M3 3v18h18"');
    expect(chart).toContain('d="M7 15l4-4 3 3 5-6"');
    expect(chart).toMatch(/<circle cx="19" cy="8" r="1" fill="currentColor"/);
    const builtins = NAV_SECTIONS.flatMap((s) => s.items);
    expect(builtins.length).toBeGreaterThan(10);
    for (const def of builtins) {
      expect(renderToStaticMarkup(def.icon as ReactElement), def.key).not.toBe(chart);
    }
  });

  it.each(["rocket", "", "constructor", "__proto__", 3, null])("rejects %j", (v) => {
    expect(isModuleIconName(v)).toBe(false);
  });
});
