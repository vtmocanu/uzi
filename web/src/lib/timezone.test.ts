// @vitest-environment node
//
// browserTimezone (issue #660): returns the browser's resolved IANA zone when Intl can
// detect one, and falls back to "UTC" when detection yields an empty string or throws.
import { afterEach, describe, expect, it, vi } from "vitest";
import { browserTimezone } from "./timezone";

// Force Intl.DateTimeFormat().resolvedOptions().timeZone to a chosen value (or make the
// constructor throw) so each case is deterministic regardless of the host's real zone.
function stubResolvedZone(timeZone: string) {
  return vi.spyOn(Intl, "DateTimeFormat").mockImplementation(
    () => ({ resolvedOptions: () => ({ timeZone }) }) as unknown as Intl.DateTimeFormat,
  );
}

describe("browserTimezone", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("returns the resolved IANA zone when Intl reports one", () => {
    stubResolvedZone("Europe/Bucharest");
    expect(browserTimezone()).toBe("Europe/Bucharest");
  });

  it("falls back to 'UTC' when the resolved zone is empty", () => {
    stubResolvedZone("");
    expect(browserTimezone()).toBe("UTC");
  });

  it("falls back to 'UTC' when detection throws", () => {
    vi.spyOn(Intl, "DateTimeFormat").mockImplementation(() => {
      throw new Error("Intl unavailable");
    });
    expect(browserTimezone()).toBe("UTC");
  });
});
