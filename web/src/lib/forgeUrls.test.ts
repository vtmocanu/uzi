import { describe, it, expect } from "vitest";
import { mergeRequestUrl, projectWebUrlFromIssue } from "./forgeUrls";
import { preferForgeUrl } from "./api";

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

// PRD #65 D8: the worker-persisted mr_web_url is rendered directly, but only after
// the isHttpsUrl guard — a hostile scheme must never become a clickable anchor.
describe("preferForgeUrl (D8 persisted-URL guard)", () => {
  const legacy = "https://gitlab.example.com/g/p/-/merge_requests/12";

  it("uses the persisted URL when it is https (the only correct link on Forgejo)", () => {
    expect(preferForgeUrl("https://forge.example.com/g/p/pulls/12", legacy)).toBe(
      "https://forge.example.com/g/p/pulls/12",
    );
  });

  it("falls back to the legacy reconstruction when the persisted URL is null (old rows)", () => {
    expect(preferForgeUrl(null, legacy)).toBe(legacy);
    expect(preferForgeUrl(undefined, legacy)).toBe(legacy);
  });

  it("REJECTS a hostile-scheme persisted URL — it never becomes the href", () => {
    // A javascript:/http: mr_web_url must not be returned; the caller falls back to
    // the (safe) legacy reconstruction, or to null when there is none.
    expect(preferForgeUrl("javascript:alert(1)", legacy)).toBe(legacy);
    expect(preferForgeUrl("http://forge.example.com/g/p/pulls/12", legacy)).toBe(legacy);
    expect(preferForgeUrl("javascript:alert(1)", null)).toBeNull();
    expect(preferForgeUrl("http://evil.example.com", null)).toBeNull();
  });
});
