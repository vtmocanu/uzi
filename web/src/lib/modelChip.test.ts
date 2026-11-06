import { describe, expect, it } from "vitest";
import { modelChipText } from "./modelChip";

describe("modelChipText", () => {
  it.each([null, undefined, "", "   "])("reads default for %j", (m) => {
    expect(modelChipText(m)).toBe("model: default");
  });
  it("marks a pinned value", () => {
    expect(modelChipText("claude-sonnet-4-5")).toBe("model: claude-sonnet-4-5 · pinned");
  });
  it("does not interpret markup in the value", () => {
    expect(modelChipText("<b>x</b>")).toBe("model: <b>x</b> · pinned");
  });
});
