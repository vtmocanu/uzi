import { describe, expect, it } from "vitest";
import { checkerModelWarning } from "./crossCheckSettings";

describe("checker model family writes", () => {
  it.each(["gpt-6.2-sol", "gpt-6.2-astra"])("keeps %s in the Codex lane", (model) => {
    expect(checkerModelWarning(`  ${model}  `, "codex")).toBe("");
    expect(checkerModelWarning(`  ${model}  `, "claude")).toMatch(/model family/);
  });
  it("preserves legacy Claude checker luna pins", () => {
    expect(checkerModelWarning(" gpt-6-luna ", "claude")).toBe("");
  });
  it.each(["custom-root", "custom-planning", "claude-custom"])("keeps custom ID %s provider validated", (model) => {
    expect(checkerModelWarning(model, "codex")).toBe("");
    expect(checkerModelWarning(model, "claude")).toBe("");
  });
  it("rejects Claude aliases on Codex and keeps malformed values invalid", () => {
    expect(checkerModelWarning("sonnet", "codex")).toMatch(/model family/);
    expect(checkerModelWarning("sonnet", "claude")).toBe("");
    expect(checkerModelWarning("gpt-6.2-sol\u200b", "codex")).not.toBe("");
  });
});
