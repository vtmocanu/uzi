import { describe, it, expect } from "vitest";
import { mergeRequestUrl, projectWebUrlFromIssue } from "./forgeUrls";

describe("projectWebUrlFromIssue", () => {
  it("strips the /-/issues/<iid> suffix to the project base", () => {
    expect(projectWebUrlFromIssue("https://gitlab.example.com/g/p/-/issues/42")).toBe(
      "https://gitlab.example.com/g/p",
    );
  });
  it("handles subgroup project paths", () => {
    expect(
      projectWebUrlFromIssue("https://gitlab.example.com/group/sub/proj/-/issues/7"),
    ).toBe("https://gitlab.example.com/group/sub/proj");
  });
  it("returns '' for a URL that is not an issue URL", () => {
    expect(projectWebUrlFromIssue("https://gitlab.example.com/g/p")).toBe("");
    expect(projectWebUrlFromIssue("https://example.com/-/merge_requests/3")).toBe("");
  });
  it("returns '' for empty / malformed input", () => {
    expect(projectWebUrlFromIssue("")).toBe("");
    expect(projectWebUrlFromIssue("not a url")).toBe("");
  });
});

describe("mergeRequestUrl", () => {
  it("builds the MR URL from an https project base", () => {
    expect(mergeRequestUrl("https://gitlab.example.com/g/p", 12)).toBe(
      "https://gitlab.example.com/g/p/-/merge_requests/12",
    );
  });
  it("returns null when the base is empty (issue URL did not match)", () => {
    expect(mergeRequestUrl("", 12)).toBeNull();
  });
  it("returns null for a non-https base (never link a hostile/plain scheme)", () => {
    expect(mergeRequestUrl("http://gitlab.example.com/g/p", 12)).toBeNull();
    expect(mergeRequestUrl("javascript:alert(1)", 12)).toBeNull();
  });
});
