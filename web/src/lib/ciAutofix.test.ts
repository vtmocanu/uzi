import { describe, it, expect } from "vitest";
import { canToggleCiAutofix, effectiveCiAutofix } from "./ciAutofix";

describe("canToggleCiAutofix (issue #4617)", () => {
  it("is true for issue, ci_fix, prompt and self_improve runs with a repo", () => {
    for (const kind of ["issue", "ci_fix", "prompt", "self_improve"] as const) {
      expect(canToggleCiAutofix({ kind, repo_id: "repo1" })).toBe(true);
    }
  });

  it("is false for kinds that own no fixable branch", () => {
    for (const kind of ["chat", "judge", "mr_rework", "job"] as const) {
      expect(canToggleCiAutofix({ kind, repo_id: "repo1" })).toBe(false);
    }
  });

  it("is false without a repo", () => {
    expect(canToggleCiAutofix({ kind: "issue", repo_id: null })).toBe(false);
  });
});

describe("effectiveCiAutofix (issue #4617)", () => {
  it("lets an explicit run override win over the user default", () => {
    expect(effectiveCiAutofix({ ci_autofix_enabled: false }, true)).toBe(false);
    expect(effectiveCiAutofix({ ci_autofix_enabled: true }, false)).toBe(true);
  });

  it("inherits the user default when the run has no override", () => {
    expect(effectiveCiAutofix({ ci_autofix_enabled: null }, false)).toBe(false);
    expect(effectiveCiAutofix({ ci_autofix_enabled: null }, true)).toBe(true);
    expect(effectiveCiAutofix({}, false)).toBe(false);
  });

  it("defaults to ON when neither the run nor the user chose", () => {
    expect(effectiveCiAutofix({ ci_autofix_enabled: null }, null)).toBe(true);
    expect(effectiveCiAutofix({}, undefined)).toBe(true);
  });
});
