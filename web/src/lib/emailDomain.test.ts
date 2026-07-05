import { describe, it, expect } from "vitest";
import { emailDomain, emailDomainAllowed } from "./emailDomain";

describe("emailDomain", () => {
  it("returns the lowercased domain after the final @", () => {
    expect(emailDomain("alice@example.com")).toBe("example.com");
    expect(emailDomain("Alice@example.com")).toBe("example.com");
    expect(emailDomain("a@b@sub.example.com")).toBe("sub.example.com"); // final @
  });
  it("returns empty string when there is no domain", () => {
    expect(emailDomain("not-an-email")).toBe("");
    expect(emailDomain("")).toBe("");
  });
});

describe("emailDomainAllowed", () => {
  it("permits every domain when the allowlist is empty", () => {
    expect(emailDomainAllowed("anyone@gmail.com", [])).toBe(true);
  });
  it("matches exactly and case-insensitively, no subdomain wildcards", () => {
    const allowed = ["example.com"];
    expect(emailDomainAllowed("alice@example.com", allowed)).toBe(true);
    expect(emailDomainAllowed("alice@example.com", allowed)).toBe(true);
    expect(emailDomainAllowed("alice@gmail.com", allowed)).toBe(false);
    expect(emailDomainAllowed("alice@sub.example.com", allowed)).toBe(false);
  });
  it("accepts any of several allowed domains", () => {
    const allowed = ["example.com", "example.org"];
    expect(emailDomainAllowed("a@example.org", allowed)).toBe(true);
    expect(emailDomainAllowed("a@example.com", allowed)).toBe(false);
  });
});
