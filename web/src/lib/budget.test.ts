import { describe, it, expect } from "vitest";
import {
  budgetRightLabel,
  extendBudgetView,
  extendEnabled,
  formatBudgetDuration,
  formatLocalTime,
  parseDurationToSeconds,
  type BudgetRun,
} from "./budget";

describe("formatBudgetDuration (PRD #1189)", () => {
  it("renders the compact two-unit budget vocabulary, dropping a zero lower unit", () => {
    expect(formatBudgetDuration(28800)).toBe("8h"); // 8h exactly → no "0m"
    expect(formatBudgetDuration(7200)).toBe("2h");
    expect(formatBudgetDuration(3 * 3600 + 48 * 60)).toBe("3h 48m");
    expect(formatBudgetDuration(6 * 3600 + 55 * 60)).toBe("6h 55m");
    expect(formatBudgetDuration(2700)).toBe("45m");
    expect(formatBudgetDuration(45)).toBe("45s");
    expect(formatBudgetDuration(2 * 86400 + 4 * 3600)).toBe("2d 4h");
    expect(formatBudgetDuration(0)).toBe("0s");
  });

  it("never shows seconds once minutes are present (it is a glance value, not a stopwatch)", () => {
    // 3h 48m 12s → "3h 48m", not "3h 48m 12s" (unlike RunEvent.formatDuration).
    expect(formatBudgetDuration(3 * 3600 + 48 * 60 + 12)).toBe("3h 48m");
  });

  it("clamps a negative or NaN value to 0", () => {
    expect(formatBudgetDuration(-5)).toBe("0s");
    expect(formatBudgetDuration(Number.NaN)).toBe("0s");
  });
});

describe("parseDurationToSeconds (the chooser's custom input)", () => {
  it("parses the documented forms", () => {
    expect(parseDurationToSeconds("2h")).toBe(7200);
    expect(parseDurationToSeconds("90m")).toBe(5400);
    expect(parseDurationToSeconds("1h30m")).toBe(5400);
    expect(parseDurationToSeconds("1h 30m")).toBe(5400); // whitespace-insensitive
    expect(parseDurationToSeconds("1d")).toBe(86400);
    expect(parseDurationToSeconds("45s")).toBe(45);
    expect(parseDurationToSeconds("1d2h")).toBe(86400 + 7200);
  });

  it("returns null for anything that is not a positive duration", () => {
    expect(parseDurationToSeconds("")).toBeNull();
    expect(parseDurationToSeconds("   ")).toBeNull();
    expect(parseDurationToSeconds("2")).toBeNull(); // bare number, no unit
    expect(parseDurationToSeconds("abc")).toBeNull();
    expect(parseDurationToSeconds("2x")).toBeNull();
    expect(parseDurationToSeconds("0h")).toBeNull(); // zero total
    expect(parseDurationToSeconds("-2h")).toBeNull();
    expect(parseDurationToSeconds("2hh")).toBeNull();
  });
});

describe("extendBudgetView (PRD #1189)", () => {
  const nowMs = Date.parse("2026-01-01T12:00:00Z");

  it("ages a running run's used off deadline_at so it agrees with the same-header deadline", () => {
    // deadline 1h05m out; total 8h → used = 8h − 1h05m = 6h55m, all from the ONE clock.
    const run: BudgetRun = {
      status: "running",
      started_at: "2026-01-01T05:05:00Z",
      deadline_at: new Date(nowMs + (65 * 60 + 0) * 1000).toISOString(),
      budget_wall_seconds: 28800,
      budget_total_seconds: 28800,
      budget_used_seconds: 24900, // server snapshot; ignored when deadline present
      budget_extension_seconds: 0,
      budget_extension_cap_seconds: 57600,
    };
    const v = extendBudgetView(run, nowMs)!;
    expect(v.running).toBe(true);
    expect(v.remainingSec).toBe(65 * 60);
    expect(v.usedSec).toBe(28800 - 65 * 60); // 6h55m
    expect(v.wallSec).toBe(28800);
    expect(v.extSec).toBe(0);
    expect(v.capSec).toBe(57600);
    expect(v.allowanceLeftSec).toBe(57600);
  });

  it("folds the extension into the allowance-left and right-label, never into wallSec", () => {
    const run: BudgetRun = {
      status: "running",
      deadline_at: new Date(nowMs + 3600 * 1000).toISOString(),
      budget_wall_seconds: 28800,
      budget_total_seconds: 36000, // 8h + 2h
      budget_extension_seconds: 7200,
      budget_extension_cap_seconds: 57600,
    };
    const v = extendBudgetView(run, nowMs)!;
    expect(v.wallSec).toBe(28800); // the frozen budget, NOT total
    expect(v.extSec).toBe(7200);
    expect(v.allowanceLeftSec).toBe(57600 - 7200);
    expect(budgetRightLabel(v)).toBe("8h+2h"); // never "10h"
  });

  it("derives a paused run's 'remaining when resumed' from the frozen budget (deadline is null)", () => {
    const run: BudgetRun = {
      status: "paused",
      budget_wall_seconds: 28800,
      budget_total_seconds: null, // null while paused — the deadline moves
      budget_used_seconds: 13320, // 3h42m
      budget_extension_seconds: 0,
      budget_extension_cap_seconds: 57600,
    };
    const v = extendBudgetView(run, nowMs)!;
    expect(v.running).toBe(false);
    expect(v.deadlineIso).toBeNull();
    expect(v.usedSec).toBe(13320);
    expect(v.remainingSec).toBe(28800 - 13320); // 4h18m
  });

  it("returns null when there is no budget to show (rollout skew or a null-budget kind)", () => {
    expect(extendBudgetView({ status: "running", budget_total_seconds: null }, nowMs)).toBeNull();
    expect(extendBudgetView({ status: "running" }, nowMs)).toBeNull(); // older api omits it
    expect(extendBudgetView({ status: "paused" }, nowMs)).toBeNull(); // paused, no wall
    // Rollout skew: a paused run can carry budget_wall_seconds WITHOUT budget_used_seconds.
    // Coercing the missing used to 0 would fabricate "0s used" and a full remaining budget, so
    // the view is null and the caller keeps the legacy plain elapsed. (Fails on the pre-fix code,
    // which returned usedSec: 0.)
    expect(extendBudgetView({ status: "paused", budget_wall_seconds: 28800 }, nowMs)).toBeNull();
    expect(extendBudgetView({ status: "queued" }, nowMs)).toBeNull();
    expect(extendBudgetView({ status: "completed", budget_total_seconds: null }, nowMs)).toBeNull();
  });
});

describe("budgetRightLabel / extendEnabled / formatLocalTime", () => {
  it("budgetRightLabel appends +<ext> only when extended", () => {
    expect(budgetRightLabel({ wallSec: 28800, extSec: 0 } as never)).toBe("8h");
    expect(budgetRightLabel({ wallSec: 28800, extSec: 7200 } as never)).toBe("8h+2h");
  });

  it("extendEnabled treats undefined (unknown) and 0 (off) the same for VISIBILITY: hidden", () => {
    expect(extendEnabled({ budget_extension_cap_seconds: 57600 })).toBe(true);
    expect(extendEnabled({ budget_extension_cap_seconds: 0 })).toBe(false);
    expect(extendEnabled({ budget_extension_cap_seconds: undefined })).toBe(false);
    expect(extendEnabled({})).toBe(false);
  });

  it("formatLocalTime is null-safe", () => {
    expect(formatLocalTime(null)).toBeNull();
    expect(formatLocalTime("not-a-date")).toBeNull();
    expect(typeof formatLocalTime("2026-01-01T12:00:00Z")).toBe("string");
  });
});
