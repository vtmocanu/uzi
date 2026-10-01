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
    ["loopback shorthand 127.1", "http://127.1:8080/cb"],
    ["loopback as a decimal integer", "http://2130706433:8080/cb"],
    ["loopback IPv6 spelled out", "http://[0:0:0:0:0:0:0:1]:8080/cb"],
    ["loopback port 0", "http://127.0.0.1:0/cb"],
    ["loopback port above 65535", "http://127.0.0.1:65536/cb"],
    ["loopback with an empty port", "http://127.0.0.1:/cb"],
    ["reserved query key code", "https://app.example.com/cb?code=x"],
    ["reserved query key state after another pair", "https://app.example.com/cb?a=1&state=x"],
    ["reserved query key iss", "https://app.example.com/cb?iss=x"],
    ["reserved query key error", "https://app.example.com/cb?error=x"],
    ["reserved query key error_description", "https://app.example.com/cb?error_description=x"],
    ["reserved query key error_uri", "https://app.example.com/cb?error_uri=x"],
    ["reserved query key with no value", "https://app.example.com/cb?code"],
    ["reserved query key percent-encoded", "https://app.example.com/cb?%63ode=x"],
    ["loopback with a reserved query key", "http://127.0.0.1:8123/cb?code=x"],
    ["over the byte cap", "https://app.example.com/" + "a".repeat(2100)],
  ])("refuses %s", (_name, uri) => {
    expect(redirectUrisError([uri])).not.toBeNull();
  });

  it("accepts a query whose keys only resemble the reserved ones", () => {
    expect(redirectUrisError(["https://app.example.com/cb?tenant=a&codex=1&states=2&Code=x&STATE=y"])).toBeNull();
  });

  it("accepts the loopback port bounds and a query on the authority", () => {
    expect(redirectUrisError(["http://127.0.0.1:1/cb", "http://[::1]:65535/cb", "http://127.0.0.1:8080?x=1"])).toBeNull();
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
