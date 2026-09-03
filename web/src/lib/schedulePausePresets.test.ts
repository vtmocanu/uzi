import { describe, expect, it } from "vitest";

import {
  DEFAULT_PAUSE_PRESET,
  resolvePausePresets,
  resolvePreset,
} from "./schedulePausePresets";

// An INDEPENDENT oracle for the pause-all preset math (PRD #1093 M4): every expected
// instant is a hand-written `new Date(y, m, d, h, min)` literal, NOT re-derived from
// resolvePausePresets, so a wrong weekday/rollover computation cannot pass by circularity.
// All arithmetic in the helper is local-time, and both sides construct local Dates here,
// so the assertions are timezone-agnostic (the fixed dates avoid US DST transitions).
//
// 2026-01-05 is a Monday (2026-01-01 is a Thursday), which anchors the weekday cases.
describe("resolvePausePresets", () => {
  it("tomorrow is the next calendar day at 09:00 local", () => {
    const now = new Date(2026, 0, 5, 8, 0, 0); // Mon 08:00
    expect(resolvePausePresets(now).tomorrow.getTime()).toBe(
      new Date(2026, 0, 6, 9, 0, 0).getTime(),
    );
  });

  it("tomorrow rolls the month/year at Dec 31", () => {
    const now = new Date(2026, 11, 31, 10, 0, 0); // Thu 2026-12-31 10:00
    expect(resolvePausePresets(now).tomorrow.getTime()).toBe(
      new Date(2027, 0, 1, 9, 0, 0).getTime(),
    );
  });

  it("in24h is exactly now + 24h", () => {
    const now = new Date(2026, 0, 31, 15, 30, 0); // crosses into February
    expect(resolvePausePresets(now).in24h.getTime()).toBe(
      new Date(2026, 1, 1, 15, 30, 0).getTime(),
    );
  });

  it("monday is today when today is Monday and 09:00 is still ahead", () => {
    const now = new Date(2026, 0, 5, 8, 0, 0); // Mon 08:00
    expect(resolvePausePresets(now).monday.getTime()).toBe(
      new Date(2026, 0, 5, 9, 0, 0).getTime(),
    );
  });

  it("monday rolls to next Monday when today is Monday at exactly 09:00 (strictly after)", () => {
    const now = new Date(2026, 0, 5, 9, 0, 0); // Mon 09:00 exactly
    expect(resolvePausePresets(now).monday.getTime()).toBe(
      new Date(2026, 0, 12, 9, 0, 0).getTime(),
    );
  });

  it("monday rolls to next Monday when today is Monday after 09:00", () => {
    const now = new Date(2026, 0, 5, 10, 0, 0); // Mon 10:00
    expect(resolvePausePresets(now).monday.getTime()).toBe(
      new Date(2026, 0, 12, 9, 0, 0).getTime(),
    );
  });

  it("monday is the coming Monday from mid-week", () => {
    const now = new Date(2026, 0, 7, 12, 0, 0); // Wed
    expect(resolvePausePresets(now).monday.getTime()).toBe(
      new Date(2026, 0, 12, 9, 0, 0).getTime(),
    );
  });

  it("monday from Sunday is the very next day", () => {
    const now = new Date(2026, 0, 4, 23, 30, 0); // Sun 23:30
    expect(resolvePausePresets(now).monday.getTime()).toBe(
      new Date(2026, 0, 5, 9, 0, 0).getTime(),
    );
  });
});

describe("resolvePreset", () => {
  const now = new Date(2026, 0, 5, 8, 0, 0); // Mon 08:00

  it("maps each computed preset to its resolved instant", () => {
    expect(resolvePreset("tomorrow", now)?.getTime()).toBe(new Date(2026, 0, 6, 9, 0, 0).getTime());
    expect(resolvePreset("24h", now)?.getTime()).toBe(new Date(2026, 0, 6, 8, 0, 0).getTime());
    expect(resolvePreset("monday", now)?.getTime()).toBe(new Date(2026, 0, 5, 9, 0, 0).getTime());
  });

  it("resolves indefinite to null", () => {
    expect(resolvePreset("indefinite", now)).toBeNull();
  });

  it("parses a custom datetime-local value as a local instant", () => {
    expect(resolvePreset("custom", now, "2026-03-04T14:15")?.getTime()).toBe(
      new Date(2026, 2, 4, 14, 15, 0).getTime(),
    );
  });

  it("resolves custom to null when unset or unparseable", () => {
    expect(resolvePreset("custom", now)).toBeNull();
    expect(resolvePreset("custom", now, "not-a-date")).toBeNull();
  });

  it("defaults to tomorrow", () => {
    expect(DEFAULT_PAUSE_PRESET).toBe("tomorrow");
  });
});
