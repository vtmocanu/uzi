import { describe, it, expect } from "vitest";
import {
  bodyError,
  byteLength,
  descriptionError,
  scopeBadgeTone,
  SCOPE_LABEL,
  SKILL_MAX_BYTES,
  SKILL_NAME_RE,
  skillNameError,
} from "./skills";

describe("skillNameError", () => {
  it("accepts kebab-case names", () => {
    for (const n of ["team-runbook", "a", "a1", "qdrant-kb", "x".repeat(64)]) {
      expect(skillNameError(n)).toBeNull();
      expect(SKILL_NAME_RE.test(n)).toBe(true);
    }
  });

  it("rejects empty, uppercase, leading hyphen, spaces, and over-long names", () => {
    expect(skillNameError("")).toMatch(/required/i);
    expect(skillNameError("   ")).toMatch(/required/i);
    expect(skillNameError("Bad-Name")).toMatch(/kebab-case/i);
    expect(skillNameError("-leading")).toMatch(/kebab-case/i);
    expect(skillNameError("has space")).toMatch(/kebab-case/i);
    expect(skillNameError("x".repeat(65))).toMatch(/kebab-case/i);
  });
});

describe("descriptionError", () => {
  it("accepts a non-empty single line", () => {
    expect(descriptionError("When to reach for this skill.")).toBeNull();
  });

  it("rejects empty and multi-line / control-char descriptions", () => {
    expect(descriptionError("")).toMatch(/required/i);
    expect(descriptionError("line one\nline two")).toMatch(/single line/i);
    expect(descriptionError("tab\ther")).toMatch(/single line/i);
    expect(descriptionError("bell" + String.fromCharCode(7) + "char")).toMatch(/single line/i);
  });
});

describe("bodyError", () => {
  it("accepts non-empty body within the cap", () => {
    expect(bodyError("# skill\n\nbody")).toBeNull();
  });

  it("rejects empty body", () => {
    expect(bodyError("   ")).toMatch(/required/i);
  });

  it("rejects a body over the byte cap", () => {
    const over = "x".repeat(SKILL_MAX_BYTES + 1);
    expect(bodyError(over)).toMatch(/over the/i);
  });
});

describe("byteLength", () => {
  it("counts UTF-8 bytes, not code units", () => {
    expect(byteLength("abc")).toBe(3);
    expect(byteLength("é")).toBe(2); // U+00E9 is 2 bytes in UTF-8
    expect(byteLength("😀")).toBe(4);
  });
});

describe("scope helpers", () => {
  it("labels each scope", () => {
    expect(SCOPE_LABEL.builtin).toBe("Builtin");
    expect(SCOPE_LABEL.global).toBe("Global");
    expect(SCOPE_LABEL.user).toBe("Mine");
  });

  it("maps scope to a badge tone", () => {
    expect(scopeBadgeTone("builtin")).toBe("brand");
    expect(scopeBadgeTone("global")).toBe("info");
    expect(scopeBadgeTone("user")).toBe("neutral");
  });
});
