import { describe, it, expect } from "vitest";
import { mergeRequestUrl, projectWebUrlFromIssue } from "./forgeUrls";
import { preferForgeUrl } from "./api";

describe("projectWebUrlFromIssue", () => {
  it("strips the GitLab /-/issues/<iid> suffix to the project base", () => {
    expect(projectWebUrlFromIssue("https://gitlab.example.com/g/p/-/issues/42")).toBe(
      "https://gitlab.example.com/g/p",
    );
  });
  it("handles GitLab subgroup project paths", () => {
    expect(
      projectWebUrlFromIssue("https://gitlab.example.com/group/sub/proj/-/issues/7"),
    ).toBe("https://gitlab.example.com/group/sub/proj");
  });
  it("strips the GitHub /issues/<n> suffix to the project base (#1486)", () => {
    expect(projectWebUrlFromIssue("https://github.com/ns/repo/issues/42")).toBe(
      "https://github.com/ns/repo",
    );
  });
  it("strips the Forgejo /issues/<n> suffix to the project base (#1486)", () => {
    expect(projectWebUrlFromIssue("https://forge.example.com/ns/repo/issues/7")).toBe(
      "https://forge.example.com/ns/repo",
    );
  });
  it("splits at the LAST /issues/ so a repo or owner named 'issues' resolves (#1486)", () => {
    // A repo literally named "issues": the base must keep it, not truncate at the first
    // /issues/ (indexOf would yield .../ns and a /pull/ 404).
    expect(projectWebUrlFromIssue("https://github.com/ns/issues/issues/42")).toBe(
      "https://github.com/ns/issues",
    );
    // An owner literally named "issues".
    expect(projectWebUrlFromIssue("https://github.com/issues/repo/issues/42")).toBe(
      "https://github.com/issues/repo",
    );
  });
  it("returns '' for a URL that is not an issue URL", () => {
    expect(projectWebUrlFromIssue("https://gitlab.example.com/g/p")).toBe("");
    expect(projectWebUrlFromIssue("https://github.com/ns/repo")).toBe("");
  });
  it("returns '' for empty / malformed input", () => {
    expect(projectWebUrlFromIssue("")).toBe("");
    expect(projectWebUrlFromIssue("not a url")).toBe("");
  });
});

describe("mergeRequestUrl", () => {
  it("builds the GitLab MR URL from an https project base", () => {
    expect(mergeRequestUrl("https://gitlab.example.com/g/p", 12, "gitlab")).toBe(
      "https://gitlab.example.com/g/p/-/merge_requests/12",
    );
  });
  it("builds a GitHub /pull/<n> URL (#1486)", () => {
    expect(mergeRequestUrl("https://github.com/ns/repo", 12, "github")).toBe(
      "https://github.com/ns/repo/pull/12",
    );
  });
  it("builds a Forgejo /pulls/<n> URL (#1486)", () => {
    expect(mergeRequestUrl("https://forge.example.com/ns/repo", 12, "forgejo")).toBe(
      "https://forge.example.com/ns/repo/pulls/12",
    );
  });
  it("defaults unknown/absent forge_type to GitLab's /-/merge_requests/ path (#1486)", () => {
    expect(mergeRequestUrl("https://forge.example.com/ns/repo", 12, "")).toBe(
      "https://forge.example.com/ns/repo/-/merge_requests/12",
    );
    expect(mergeRequestUrl("https://forge.example.com/ns/repo", 12, null)).toBe(
      "https://forge.example.com/ns/repo/-/merge_requests/12",
    );
    expect(mergeRequestUrl("https://forge.example.com/ns/repo", 12, undefined)).toBe(
      "https://forge.example.com/ns/repo/-/merge_requests/12",
    );
  });
  it("returns null when the base is empty (issue URL did not match)", () => {
    expect(mergeRequestUrl("", 12, "github")).toBeNull();
    expect(mergeRequestUrl(null, 12, "github")).toBeNull();
    expect(mergeRequestUrl(undefined, 12, "github")).toBeNull();
  });
  it("returns null for a non-https base (never link a hostile/plain scheme)", () => {
    expect(mergeRequestUrl("http://gitlab.example.com/g/p", 12, "gitlab")).toBeNull();
    expect(mergeRequestUrl("javascript:alert(1)", 12, "github")).toBeNull();
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
