import { describe, it, expect } from "vitest";
import {
  maskEmail,
  maskName,
  maskRepoPath,
  maskHost,
  maskUsername,
  maskIp,
  maskDomains,
} from "./demoMask";

describe("maskEmail", () => {
  it("is the identity function when disabled", () => {
    expect(maskEmail("vlad.mocanu@metaminds.com", false)).toBe("vlad.mocanu@metaminds.com");
  });

  it("derives a capitalized first name from the local-part when enabled", () => {
    expect(maskEmail("vlad.mocanu@metaminds.com", true)).toBe("Vlad");
    expect(maskEmail("vlad_mocanu@x.com", true)).toBe("Vlad");
    expect(maskEmail("vlad+tag@x.com", true)).toBe("Vlad");
    expect(maskEmail("vlad-m@x.com", true)).toBe("Vlad");
  });

  it("uses the whole local-part when it has no separator", () => {
    expect(maskEmail("vlad@x.com", true)).toBe("Vlad");
  });

  it("treats a value with no @ as the local-part", () => {
    expect(maskEmail("vlad", true)).toBe("Vlad");
  });

  it("passes empty/undefined/null through", () => {
    expect(maskEmail("", true)).toBe("");
    expect(maskEmail(undefined, true)).toBe("");
    expect(maskEmail(null, true)).toBe("");
  });
});

describe("maskName", () => {
  it("is the identity function when disabled", () => {
    expect(maskName("Vlad Mocanu", false)).toBe("Vlad Mocanu");
  });

  it("keeps the capitalized first whitespace token when enabled", () => {
    expect(maskName("Vlad Mocanu", true)).toBe("Vlad");
    expect(maskName("vlad mocanu", true)).toBe("Vlad");
  });

  it("returns a single-word name unchanged (still capitalized)", () => {
    expect(maskName("Vlad", true)).toBe("Vlad");
  });

  it("passes empty/undefined/null through", () => {
    expect(maskName("", true)).toBe("");
    expect(maskName(undefined, true)).toBe("");
    expect(maskName(null, true)).toBe("");
  });
});

describe("maskRepoPath", () => {
  it("is the identity function when disabled", () => {
    expect(maskRepoPath("vtmocanu/uzi", false)).toBe("vtmocanu/uzi");
  });

  it("replaces the namespace with demo, keeping the repo (two segments)", () => {
    expect(maskRepoPath("vtmocanu/uzi", true)).toBe("demo/uzi");
  });

  it("replaces a multi-segment subgroup path with demo/<repo>", () => {
    expect(maskRepoPath("group/sub/repo", true)).toBe("demo/repo");
  });

  it("prefixes a bare single segment with demo/", () => {
    expect(maskRepoPath("uzi", true)).toBe("demo/uzi");
  });

  it("tolerates trailing slashes", () => {
    expect(maskRepoPath("group/repo/", true)).toBe("demo/repo");
  });

  it("passes empty/undefined/null through", () => {
    expect(maskRepoPath("", true)).toBe("");
    expect(maskRepoPath(undefined, true)).toBe("");
    expect(maskRepoPath(null, true)).toBe("");
  });
});

describe("maskHost", () => {
  it("is the identity function when disabled", () => {
    expect(maskHost("https://gitlab.metaminds.com", false)).toBe("https://gitlab.metaminds.com");
  });

  it("replaces the host, keeping the scheme", () => {
    expect(maskHost("https://gitlab.metaminds.com", true)).toBe("https://forge.example.com");
    expect(maskHost("http://gitlab.metaminds.com", true)).toBe("http://forge.example.com");
  });

  it("drops path/search/port so no subpath leaks", () => {
    expect(maskHost("https://gitlab.metaminds.com:8443/foo?x=1", true)).toBe(
      "https://forge.example.com",
    );
    expect(maskHost("https://git.co.com/team", true)).toBe("https://forge.example.com");
  });

  it("falls back to a bare fake host for a value with no scheme", () => {
    expect(maskHost("gitlab.metaminds.com", true)).toBe("forge.example.com");
  });

  it("passes empty/undefined/null through", () => {
    expect(maskHost("", true)).toBe("");
    expect(maskHost(undefined, true)).toBe("");
    expect(maskHost(null, true)).toBe("");
  });
});

describe("maskUsername", () => {
  it("is the identity function when disabled", () => {
    expect(maskUsername("vlad-real", "human", false)).toBe("vlad-real");
    expect(maskUsername("uzi-bot-real", "bot", false)).toBe("uzi-bot-real");
  });

  it("masks a human to demo-user and a bot to demo-bot when enabled", () => {
    expect(maskUsername("vlad-real", "human", true)).toBe("demo-user");
    expect(maskUsername("uzi-bot-real", "bot", true)).toBe("demo-bot");
  });

  it("passes empty/undefined/null through", () => {
    expect(maskUsername("", "human", true)).toBe("");
    expect(maskUsername(undefined, "bot", true)).toBe("");
    expect(maskUsername(null, "human", true)).toBe("");
  });
});

describe("maskIp", () => {
  it("is the identity function when disabled", () => {
    expect(maskIp("192.168.1.42", false)).toBe("192.168.1.42");
  });

  it("masks to the TEST-NET-3 address when enabled", () => {
    expect(maskIp("192.168.1.42", true)).toBe("203.0.113.7");
  });

  it("passes empty/undefined/null through", () => {
    expect(maskIp("", true)).toBe("");
    expect(maskIp(undefined, true)).toBe("");
    expect(maskIp(null, true)).toBe("");
  });
});

describe("maskDomains", () => {
  it("is the identity function when disabled", () => {
    expect(maskDomains("metaminds.com, acme.io", false)).toBe("metaminds.com, acme.io");
  });

  it("masks the joined display string to example.com when enabled", () => {
    expect(maskDomains("metaminds.com, acme.io", true)).toBe("example.com");
  });

  it("passes empty/undefined/null through", () => {
    expect(maskDomains("", true)).toBe("");
    expect(maskDomains(undefined, true)).toBe("");
    expect(maskDomains(null, true)).toBe("");
  });
});
