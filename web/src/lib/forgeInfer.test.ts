import { describe, it, expect } from "vitest";
import { inferForgeType, defaultUrlForType } from "./forgeInfer";

const ALL = ["gitlab", "forgejo", "github"];

describe("inferForgeType", () => {
  it("recognizes github/gitlab/forgejo by host substring", () => {
    expect(inferForgeType("https://github.com", ALL)).toBe("github");
    expect(inferForgeType("https://gitlab.com", ALL)).toBe("gitlab");
    expect(inferForgeType("https://forgejo.example.com", ALL)).toBe("forgejo");
  });

  it("matches a subdomain host", () => {
    expect(inferForgeType("https://gitlab.example.com", ALL)).toBe("gitlab");
  });

  it("ignores ports (hostname strips them)", () => {
    expect(inferForgeType("https://gitlab.example.com:8443", ALL)).toBe("gitlab");
  });

  it("resolves the codeberg.org alias to forgejo, bare and by suffix", () => {
    expect(inferForgeType("https://codeberg.org", ALL)).toBe("forgejo");
    expect(inferForgeType("https://git.codeberg.org", ALL)).toBe("forgejo");
  });

  it("returns null for an unrecognized/self-hosted host", () => {
    expect(inferForgeType("https://git.example.com", ALL)).toBe(null);
  });

  it("returns null when the recognized type is not advertised (D4 guard)", () => {
    expect(inferForgeType("https://github.com", ["gitlab"])).toBe(null);
  });

  it("returns null for a malformed URL and never throws", () => {
    expect(() => inferForgeType("not a url", ALL)).not.toThrow();
    expect(inferForgeType("not a url", ALL)).toBe(null);
  });
});

describe("defaultUrlForType", () => {
  it("returns the first allowlist URL whose host infers to the type", () => {
    expect(
      defaultUrlForType("github", ["https://gitlab.example.com", "https://github.com"]),
    ).toBe("https://github.com");
  });

  it("returns null when no allowlist URL is a recognized host for the type", () => {
    expect(
      defaultUrlForType("forgejo", ["https://gitlab.example.com", "https://git.example.com"]),
    ).toBe(null);
  });

  it("returns the FIRST matching URL when several match", () => {
    expect(
      defaultUrlForType("gitlab", [
        "https://github.com",
        "https://gitlab.com",
        "https://gitlab.example.com",
      ]),
    ).toBe("https://gitlab.com");
  });
});
