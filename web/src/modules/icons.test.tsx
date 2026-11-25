// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
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

  it.each(["rocket", "", "constructor", "__proto__", 3, null])("rejects %j", (v) => {
    expect(isModuleIconName(v)).toBe(false);
  });
});
