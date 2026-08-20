import { describe, it, expect } from "vitest";
import {
  CHANGELOG_REPO_BASE,
  prdIssueUrl,
  releaseTagUrl,
  linkifyPrdRefs,
} from "./changelogLinks";

describe("changelogLinks base + url builders", () => {
  it("defaults the repo base to the canonical repo with no trailing slash", () => {
    expect(CHANGELOG_REPO_BASE).toBe("https://github.com/vtmocanu/uzi");
  });

  it("builds issue and release-tag URLs", () => {
    expect(prdIssueUrl(123)).toBe("https://github.com/vtmocanu/uzi/issues/123");
    expect(releaseTagUrl("0.48.0")).toBe(
      "https://github.com/vtmocanu/uzi/releases/tag/v0.48.0",
    );
  });
});

describe("linkifyPrdRefs", () => {
  it("links a plain PRD ref to its issue", () => {
    expect(linkifyPrdRefs("Did a thing (PRD #123).")).toBe(
      "Did a thing ([PRD #123](https://github.com/vtmocanu/uzi/issues/123)).",
    );
  });

  it("leaves an already-linked PR ref untouched", () => {
    const input = "Fixed it [#413](https://github.com/vtmocanu/uzi/pull/413).";
    expect(linkifyPrdRefs(input)).toBe(input);
  });

  it("leaves a bare #N untouched", () => {
    const input = "See #7 for context.";
    expect(linkifyPrdRefs(input)).toBe(input);
  });

  it("links every PRD ref when several appear in one string", () => {
    expect(linkifyPrdRefs("PRD #1 and PRD #22 both landed")).toBe(
      "[PRD #1](https://github.com/vtmocanu/uzi/issues/1) and " +
        "[PRD #22](https://github.com/vtmocanu/uzi/issues/22) both landed",
    );
  });

  it("does not touch a PR ref while still linking an adjacent PRD ref", () => {
    expect(linkifyPrdRefs("PRD #5 via [#6](https://github.com/vtmocanu/uzi/pull/6)")).toBe(
      "[PRD #5](https://github.com/vtmocanu/uzi/issues/5) via " +
        "[#6](https://github.com/vtmocanu/uzi/pull/6)",
    );
  });
});
