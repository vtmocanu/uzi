import { describe, it, expect } from "vitest";
import {
  MAX_MODEL_LEN,
  frontmatterFieldWarning,
  isLeadTemplateName,
  modelFieldWarning,
  TEMPLATE_SCOPE_LABEL,
  templateScopeBadgeTone,
} from "./agentTemplates";

describe("template scope helpers", () => {
  it("labels each scope for the UI", () => {
    expect(TEMPLATE_SCOPE_LABEL.builtin).toBe("Builtin");
    expect(TEMPLATE_SCOPE_LABEL.global).toBe("Global");
    expect(TEMPLATE_SCOPE_LABEL.user).toBe("Mine");
  });

  it("maps each scope to a distinct badge tone", () => {
    expect(templateScopeBadgeTone("builtin")).toBe("brand");
    expect(templateScopeBadgeTone("global")).toBe("info");
    expect(templateScopeBadgeTone("user")).toBe("neutral");
  });
});

describe("modelFieldWarning", () => {
  it("accepts blank (inherit), curated aliases, and full IDs", () => {
    for (const m of ["", "   ", "opus", "sonnet", "claude-fable-5", "us.anthropic.claude-x:v1"]) {
      expect(modelFieldWarning(m)).toBe("");
    }
  });

  it("trims first, so a stray trailing space is fine (matches the server)", () => {
    expect(modelFieldWarning("opus  ")).toBe("");
  });

  it("rejects interior whitespace", () => {
    expect(modelFieldWarning("claude 3")).not.toBe("");
  });

  it("rejects newlines / control characters", () => {
    expect(modelFieldWarning("opus\nmodel: sonnet")).not.toBe("");
  });

  it("caps the length at MAX_MODEL_LEN", () => {
    expect(modelFieldWarning("x".repeat(MAX_MODEL_LEN))).toBe("");
    expect(modelFieldWarning("x".repeat(MAX_MODEL_LEN + 1))).not.toBe("");
  });
});

describe("frontmatterFieldWarning model gating", () => {
  it("surfaces the tightened model rule through the shared submit gate", () => {
    expect(frontmatterFieldWarning({ description: "d", model: "claude 3", tools: [] })).not.toBe("");
    expect(frontmatterFieldWarning({ description: "d", model: "opus", tools: [] })).toBe("");
  });
});

describe("isLeadTemplateName", () => {
  it("matches lead / orchestrator case-insensitively and nothing else", () => {
    for (const n of ["lead", "orchestrator", "Lead", "ORCHESTRATOR"]) {
      expect(isLeadTemplateName(n)).toBe(true);
    }
    for (const n of ["coder", "reviewer", "leader", "lead-x", ""]) {
      expect(isLeadTemplateName(n)).toBe(false);
    }
  });
});
