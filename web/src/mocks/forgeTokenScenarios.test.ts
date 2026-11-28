import { describe, expect, it } from "vitest";
import { forgeTokenScenario, mockConnection } from "./data";
import { classifyConnection } from "../lib/forgeTokenBanner";

const now = Date.parse("2026-06-01T12:00:00Z");

function classify(name: string) {
  const report = forgeTokenScenario(name, now);
  if (!report) throw new Error(`no scenario ${name}`);
  return classifyConnection({ ...mockConnection, privilege_report: report, privilege_status: report.status }, now);
}

describe("forgeTokenScenario", () => {
  it.each([
    ["forge-token-expired", "expired"],
    ["forge-token-expiring", "expiring"],
    ["forge-token-unverified", "unverified"],
  ])("%s classifies as %s", (name, cls) => {
    expect(classify(name)).toBe(cls);
  });

  it("returns null for the default and unknown scenarios", () => {
    expect(forgeTokenScenario("", now)).toBeNull();
    expect(forgeTokenScenario("no-such-scenario", now)).toBeNull();
  });
});
