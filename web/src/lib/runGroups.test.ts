import { describe, it, expect } from "vitest";
import { groupRuns, runBucket, runMatchesQuery } from "./runGroups";

// All fixtures are built through the LOCAL-time Date constructor, never ISO-Z
// literals, so the expectations hold in any timezone the suite runs in — runBucket's
// boundaries are local by design.
const local = (y: number, mo: number, d: number, h = 12) => new Date(y, mo, d, h);

// A fixed "now": Friday 2026-08-14, 15:00 local (month index 7). Its week runs
// Mon Aug 10 → Sun Aug 16; its month has weeks starting Jul 27, Aug 3, Aug 10.
const NOW = local(2026, 7, 14, 15).getTime();
const iso = (y: number, mo: number, d: number, h = 12) => local(y, mo, d, h).toISOString();

describe("runBucket", () => {
  it("names today and yesterday", () => {
    expect(runBucket(iso(2026, 7, 14, 9), NOW)).toEqual({ key: "today", label: "Today" });
    expect(runBucket(iso(2026, 7, 13, 23), NOW)).toEqual({ key: "yesterday", label: "Yesterday" });
  });

  it("folds a future-dated anchor (clock skew) into Today rather than inventing a bucket", () => {
    expect(runBucket(iso(2026, 7, 15, 1), NOW).key).toBe("today");
  });

  it("names earlier days of the current week by weekday", () => {
    // Wed Aug 12 and Mon Aug 10 are inside the current Mon-start week.
    const wed = runBucket(iso(2026, 7, 12), NOW);
    expect(wed.key).toBe("d:2026-08-12");
    expect(wed.label).toBe(local(2026, 7, 12).toLocaleDateString(undefined, { weekday: "long" }));
    expect(runBucket(iso(2026, 7, 10), NOW).key).toBe("d:2026-08-10");
  });

  it("buckets earlier weeks of the current month by their Monday", () => {
    // Sun Aug 9 belongs to the week of Mon Aug 3 — one week back, same month.
    const g = runBucket(iso(2026, 7, 9), NOW);
    expect(g.key).toBe("w:2026-08-03");
    expect(g.label).toBe(
      `Week of ${local(2026, 7, 3).toLocaleDateString(undefined, { month: "short", day: "numeric" })}`,
    );
    // Sat Aug 1 is in the current month but its week started Mon Jul 27 — still a
    // week bucket (the run is this month), labeled by that Monday.
    expect(runBucket(iso(2026, 7, 1), NOW).key).toBe("w:2026-07-27");
  });

  it("buckets anything before the current month by calendar month", () => {
    const july = runBucket(iso(2026, 6, 28), NOW);
    expect(july.key).toBe("m:2026-07");
    expect(july.label).toBe(
      local(2026, 6, 28).toLocaleDateString(undefined, { month: "long", year: "numeric" }),
    );
    // A different year stays distinct from the same month this year.
    expect(runBucket(iso(2025, 7, 14), NOW).key).toBe("m:2025-08");
  });
});

describe("groupRuns", () => {
  it("groups a newest-first list into contiguous buckets, preserving order", () => {
    const runs = [
      { id: "a", at: iso(2026, 7, 14, 9) }, // Today
      { id: "b", at: iso(2026, 7, 14, 7) }, // Today
      { id: "c", at: iso(2026, 7, 13) }, // Yesterday
      { id: "d", at: iso(2026, 7, 9) }, // Week of Aug 3
      { id: "e", at: iso(2026, 6, 2) }, // July 2026
    ];
    const groups = groupRuns(runs, (r) => r.at, NOW);
    expect(groups.map((g) => g.key)).toEqual(["today", "yesterday", "w:2026-08-03", "m:2026-07"]);
    expect(groups[0].runs.map((r) => r.id)).toEqual(["a", "b"]);
    expect(groups[3].runs.map((r) => r.id)).toEqual(["e"]);
  });

  it("returns no groups for no runs", () => {
    expect(groupRuns([], () => "", NOW)).toEqual([]);
  });
});

describe("runMatchesQuery", () => {
  const run = {
    issue_title: "Board card badges for MR pipeline status",
    repo_path: "vtmocanu/uzi",
    issue_iid: 426,
    worker_name: "hetzner-worker",
    status: "completed",
  };

  it("matches everything on an empty or whitespace query", () => {
    expect(runMatchesQuery(run, "")).toBe(true);
    expect(runMatchesQuery(run, "   ")).toBe(true);
  });

  it("matches title, repo path, worker name and status, case-insensitively", () => {
    expect(runMatchesQuery(run, "PIPELINE")).toBe(true);
    expect(runMatchesQuery(run, "vtmocanu")).toBe(true);
    expect(runMatchesQuery(run, "hetzner")).toBe(true);
    expect(runMatchesQuery(run, "completed")).toBe(true);
    expect(runMatchesQuery(run, "nonexistent")).toBe(false);
  });

  it("matches issue iids as substrings, with or without a leading '#'", () => {
    expect(runMatchesQuery(run, "#42")).toBe(true);
    expect(runMatchesQuery(run, "26")).toBe(true);
    expect(runMatchesQuery(run, "#999")).toBe(false);
    // A bare '#' has no remainder — it must not match every run.
    expect(runMatchesQuery(run, "#")).toBe(false);
  });

  it("never matches the iid arm on a run without an iid", () => {
    expect(runMatchesQuery({ ...run, issue_iid: null }, "#42")).toBe(false);
  });
});
