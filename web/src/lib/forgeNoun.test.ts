import { describe, it, expect } from "vitest";
import { forgeNoun, forgeNounLower, forgeNounSentence, forgePlatform, mrAbbrev, mrRefSymbol } from "./forgeNoun";

describe("forgeNoun", () => {
  it("returns the per-forge Title-Case noun", () => {
    expect(forgeNoun("gitlab")).toBe("Merge Request");
    expect(forgeNoun("forgejo")).toBe("Pull Request");
  });

  it("defaults unknown/absent forge_type to GitLab's noun (dark-landing: only GitLab exists today)", () => {
    expect(forgeNoun("")).toBe("Merge Request");
    expect(forgeNoun(null)).toBe("Merge Request");
    expect(forgeNoun(undefined)).toBe("Merge Request");
    expect(forgeNoun("something_else")).toBe("Merge Request");
  });

  it("derives the lower and sentence casings from the single noun literal", () => {
    expect(forgeNounLower("gitlab")).toBe("merge request");
    expect(forgeNounLower("forgejo")).toBe("pull request");
    expect(forgeNounSentence("gitlab")).toBe("Merge request");
    expect(forgeNounSentence("forgejo")).toBe("Pull request");
  });

  it("gives the compact abbreviation and reference sigil per forge", () => {
    expect(mrAbbrev("gitlab")).toBe("MR");
    expect(mrAbbrev("forgejo")).toBe("PR");
    expect(mrRefSymbol("gitlab")).toBe("!");
    expect(mrRefSymbol("forgejo")).toBe("#");
  });

  it("names the destination platform per forge, defaulting to GitLab", () => {
    expect(forgePlatform("gitlab")).toBe("GitLab");
    expect(forgePlatform("forgejo")).toBe("Forgejo");
    // dark-landing: unknown/absent renders GitLab, so an existing card is unchanged.
    expect(forgePlatform("")).toBe("GitLab");
    expect(forgePlatform(null)).toBe("GitLab");
    expect(forgePlatform(undefined)).toBe("GitLab");
  });
});
