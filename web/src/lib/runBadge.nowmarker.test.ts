import { describe, expect, it } from "vitest";
import { firstInProgressMilestoneId, milestoneBadgeText } from "./runBadge";

// PRD #1064 M3: the ◐ in-progress marker on the milestone badge, and the D4 first-in-
// progress selection helper the run view / board / list all share.

describe("milestoneBadgeText ◐ suffix (PRD #1064 M3)", () => {
  it("appends ' ◐' to the label when a milestone is in progress", () => {
    expect(milestoneBadgeText({ done: 1, total: 6, reported: true }, true).label).toBe("M1/6 ◐");
  });

  it("leaves the label untouched when nothing is in progress (default arg)", () => {
    expect(milestoneBadgeText({ done: 1, total: 6, reported: true }).label).toBe("M1/6");
    expect(milestoneBadgeText({ done: 1, total: 6, reported: true }, false).label).toBe("M1/6");
  });

  it("marks an unreported run in progress as 'M–/N ◐' (numerator unchanged)", () => {
    expect(milestoneBadgeText({ done: 0, total: 4, reported: false }, true).label).toBe("M–/4 ◐");
  });

  it("does not change the tooltip when in progress", () => {
    const off = milestoneBadgeText({ done: 1, total: 6, reported: true });
    const on = milestoneBadgeText({ done: 1, total: 6, reported: true }, true);
    expect(on.title).toBe(off.title);
  });
});

describe("firstInProgressMilestoneId (D4 selection)", () => {
  const milestones = [
    { id: "m1", title: "one" },
    { id: "m2", title: "two" },
    { id: "m3", title: "three" },
  ];

  it("returns the first frozen id that is in progress, by FROZEN order not input order", () => {
    // Input order is [m3, m2] but frozen order is m1,m2,m3 → m2 wins.
    expect(firstInProgressMilestoneId({ milestones, milestones_in_progress: ["m3", "m2"] })).toBe("m2");
  });

  it("returns null when nothing is in progress", () => {
    expect(firstInProgressMilestoneId({ milestones, milestones_in_progress: [] })).toBeNull();
    expect(firstInProgressMilestoneId({ milestones, milestones_in_progress: null })).toBeNull();
  });

  it("returns null when the run carries no frozen milestone list", () => {
    expect(firstInProgressMilestoneId({ milestones: null, milestones_in_progress: ["m2"] })).toBeNull();
    expect(firstInProgressMilestoneId({})).toBeNull();
  });

  it("ignores an in-progress id that is not a frozen member", () => {
    expect(firstInProgressMilestoneId({ milestones, milestones_in_progress: ["ghost"] })).toBeNull();
  });
});
