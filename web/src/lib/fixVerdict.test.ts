import { describe, it, expect } from "vitest";
import { fixVerdictChip } from "./fixVerdict";

describe("fixVerdictChip", () => {
  it("maps each settled verdict to a chip", () => {
    expect(fixVerdictChip("verified", true)).toMatchObject({ label: "verified ✓", tone: "ok" });
    expect(fixVerdictChip("fix_failed", true)).toMatchObject({ label: "fix failed ✗", tone: "danger" });
    expect(fixVerdictChip("not_code", true)).toMatchObject({ label: "not a code problem", tone: "neutral" });
  });

  it("shows 'unverified' for a terminal fix run with no verdict yet", () => {
    expect(fixVerdictChip(null, true)).toMatchObject({ label: "unverified", tone: "warning" });
  });

  it("shows no chip while the run is still working", () => {
    expect(fixVerdictChip(null, false)).toBeNull();
  });
});
