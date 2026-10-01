import { describe, expect, it } from "vitest";
import { parseRedirectUriLines, redirectUrisError } from "./oauthClient";

describe("redirectUrisError", () => {
  it("accepts https and loopback http with a port", () => {
    expect(redirectUrisError(["https://app.example.com/cb", "http://127.0.0.1:8123/cb", "http://[::1]:8123/cb"])).toBeNull();
    expect(redirectUrisError([])).toBeNull();
  });

  it.each([
    ["localhost", "http://localhost:8080/cb"],
    ["http non-loopback", "http://app.example.com/cb"],
    ["loopback without a port", "http://127.0.0.1/cb"],
    ["fragment", "https://app.example.com/cb#x"],
    ["userinfo", "https://u:p@app.example.com/cb"],
    ["custom scheme", "myapp://cb"],
    ["whitespace", "https://app.example.com/a b"],
    ["over the byte cap", "https://app.example.com/" + "a".repeat(2100)],
  ])("refuses %s", (_name, uri) => {
    expect(redirectUrisError([uri])).not.toBeNull();
  });

  it("refuses a sixth URI and a duplicate", () => {
    const six = ["a", "b", "c", "d", "e", "f"].map((s) => `https://${s}.example.com/cb`);
    expect(redirectUrisError(six)).toMatch(/at most 5/);
    expect(redirectUrisError(["https://a.example.com/cb", "https://a.example.com/cb"])).toMatch(/duplicate/);
  });
});

describe("parseRedirectUriLines", () => {
  it("trims lines and drops blanks", () => {
    expect(parseRedirectUriLines("  https://a.example.com/cb \n\n\thttps://b.example.com/cb\n")).toEqual([
      "https://a.example.com/cb",
      "https://b.example.com/cb",
    ]);
  });
});
