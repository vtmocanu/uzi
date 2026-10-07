import canonical from "../../../fixtures/schedule-presets/canonical.json";
import { describe, expect, it } from "vitest";
import {
  cronFromPreset,
  DEFAULT_PRESET_STATE,
  humanizeCron,
  parseHHMM,
  presetFromCron,
  type PresetState,
} from "./schedulePresets";

describe("cronFromPreset — canonical M2 forms", () => {
  it("weekdays = `M H * * 1-5`", () => {
    expect(cronFromPreset({ preset: "weekdays", hour: 2, minute: 0, everyN: 6 })).toBe("0 2 * * 1-5");
  });
  it("daily = `M H * * *`", () => {
    expect(cronFromPreset({ preset: "daily", hour: 3, minute: 30, everyN: 6 })).toBe("30 3 * * *");
  });
  it("weekly (Monday) = `M H * * 1`", () => {
    expect(cronFromPreset({ preset: "weekly", hour: 9, minute: 0, everyN: 6 })).toBe("0 9 * * 1");
  });
  it("every-N-hours = `0 */N * * *`", () => {
    expect(cronFromPreset({ preset: "everyNHours", hour: 0, minute: 0, everyN: 6 })).toBe("0 */6 * * *");
  });
  it("custom has no canonical cron", () => {
    expect(cronFromPreset({ ...DEFAULT_PRESET_STATE, preset: "custom" })).toBe("");
  });
});

describe("presetFromCron — recognises the four shapes, else custom", () => {
  it("weekdays", () => {
    expect(presetFromCron("0 2 * * 1-5")).toEqual({ preset: "weekdays", hour: 2, minute: 0, everyN: 6 });
  });
  it("daily", () => {
    expect(presetFromCron("30 3 * * *")).toEqual({ preset: "daily", hour: 3, minute: 30, everyN: 6 });
  });
  it("weekly", () => {
    expect(presetFromCron("0 9 * * 1")).toEqual({ preset: "weekly", hour: 9, minute: 0, everyN: 6 });
  });
  it("every-N-hours", () => {
    expect(presetFromCron("0 */6 * * *")).toEqual({ preset: "everyNHours", hour: 0, minute: 0, everyN: 6 });
  });

  it("flips to custom for a hand-edited cron no preset produces", () => {
    // A minute list, a day-of-month restriction, and a 6-field expression all fall through.
    expect(presetFromCron("0,30 2 * * 1-5").preset).toBe("custom");
    expect(presetFromCron("0 2 1 * *").preset).toBe("custom");
    expect(presetFromCron("0 2 * * 1-5 2026").preset).toBe("custom");
    // A day-of-week the presets don't cover (Tuesday) is custom, not weekly.
    expect(presetFromCron("0 9 * * 2").preset).toBe("custom");
  });

  it("keeps the fallback params on a custom result", () => {
    const fallback: PresetState = { preset: "weekdays", hour: 7, minute: 15, everyN: 4 };
    const got = presetFromCron("0 2 1 * *", fallback);
    expect(got.preset).toBe("custom");
    expect(got.hour).toBe(7);
    expect(got.everyN).toBe(4);
  });
});

describe("preset ↔ cron round-trip", () => {
  const states: PresetState[] = [
    { preset: "weekdays", hour: 2, minute: 0, everyN: 6 },
    { preset: "daily", hour: 14, minute: 5, everyN: 6 },
    { preset: "weekly", hour: 9, minute: 0, everyN: 6 },
    { preset: "everyNHours", hour: 0, minute: 0, everyN: 3 },
    { preset: "everyNHours", hour: 0, minute: 0, everyN: 12 },
  ];
  it.each(states)("cronFromPreset(presetFromCron(c)) === c for %o", (state) => {
    const cron = cronFromPreset(state);
    expect(cronFromPreset(presetFromCron(cron))).toBe(cron);
  });
});

describe("parseHHMM", () => {
  it("parses valid times", () => {
    expect(parseHHMM("02:00")).toEqual({ hour: 2, minute: 0 });
    expect(parseHHMM("23:59")).toEqual({ hour: 23, minute: 59 });
  });
  it("rejects out-of-range and garbage", () => {
    expect(parseHHMM("24:00")).toBeNull();
    expect(parseHHMM("12:60")).toBeNull();
    expect(parseHHMM("nope")).toBeNull();
  });
});

describe("humanizeCron", () => {
  it("describes presets and falls back to the raw expression", () => {
    expect(humanizeCron("0 2 * * 1-5")).toBe("Weekdays at 02:00");
    expect(humanizeCron("0 3 * * *")).toBe("Every day at 03:00");
    expect(humanizeCron("0 9 * * 1")).toBe("Every Monday at 09:00");
    expect(humanizeCron("0 */6 * * *")).toBe("Every 6 hours");
    expect(humanizeCron("0 2 1 * *")).toBe("0 2 1 * *");
  });
});

describe("shared Go/web canonical preset parity", () => {
  it.each(canonical)("recognises and renders $cron", ({ cron, preset, hour, minute, ok }) => {
    const names = { every_n_minutes: "everyNMinutes", every_n_hours: "everyNHours" };
    const expected = names[preset as keyof typeof names] ?? preset;
    const state = presetFromCron(cron);
    expect(state.preset).toBe(expected);
    if (ok) {
      if (preset === "every_n_minutes" || preset === "every_n_hours") {
        expect(state.everyN).toBe(hour);
      } else {
        expect(state.hour).toBe(hour);
        expect(state.minute).toBe(minute);
      }
      expect(cronFromPreset(state)).toBe(cron);
      expect(presetFromCron(cronFromPreset(state))).toEqual(state);
    } else {
      expect(state.preset).toBe("custom");
      expect(humanizeCron(cron)).toBe(cron);
    }
  });
  it.each([1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30])("round trips every %i minutes", (n) => {
    const cron = `*/${n} * * * *`;
    expect(cronFromPreset({ ...DEFAULT_PRESET_STATE, preset: "everyNMinutes", everyN: n })).toBe(cron);
    expect(presetFromCron(cron)).toMatchObject({ preset: "everyNMinutes", everyN: n });
    expect(humanizeCron(cron)).toBe(n === 1 ? "Every minute" : `Every ${n} minutes`);
  });
});
