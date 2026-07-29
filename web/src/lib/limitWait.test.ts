import { describe, it, expect } from "vitest";
import {
  RATE_LIMIT_WINDOW_LABELS,
  canToggleWaitOnLimit,
  feedWindowLabel,
  formatCountdown,
  parseFeedInstant,
  runWindowLabel,
} from "./limitWait";

// The two `rate_limit_type` fields (PRD #35). These describes are split by TRUST
// LEVEL rather than by function, because that is the distinction the code exists to
// enforce and a reader scanning for "the rate_limit_type tests" has to land on the
// right one.

describe("feedWindowLabel — the UNTRUSTED field (a run_message payload)", () => {
  it("resolves every member of the SDK vocabulary", () => {
    // Enumerated from the map rather than hand-listed: a member added to the
    // vocabulary without a label would otherwise pass unnoticed here and render as
    // nothing on the feed row.
    for (const type of Object.keys(RATE_LIMIT_WINDOW_LABELS)) {
      expect(feedWindowLabel(type)).toBe(RATE_LIMIT_WINDOW_LABELS[type]);
    }
  });

  it("🔴 NEVER echoes an unrecognised value, not even escaped", () => {
    // The server's allowlist does NOT reach this field — run_messages payloads are
    // worker-authored JSON with only a NUL strip and a rune cap, and the allowlist
    // people reach for guards the run ROW's column on a different write path. So the
    // assertion is not "it is escaped", it is "the input does not appear in the
    // output at all", which is the stronger property and the one being built.
    const hostile = "<img src=x onerror=alert(1)>";
    expect(feedWindowLabel(hostile)).toBeNull();
    expect(feedWindowLabel("five_hour‮evil")).toBeNull();
    expect(feedWindowLabel("FIVE_HOUR")).toBeNull();
    expect(feedWindowLabel("")).toBeNull();
  });

  it("survives a payload field that is not a string at all", () => {
    // Decoded JSON: the field can be anything, and a `string`-typed parameter would
    // just push the cast (and the rot) to the call site.
    expect(feedWindowLabel(undefined)).toBeNull();
    expect(feedWindowLabel(null)).toBeNull();
    expect(feedWindowLabel(5)).toBeNull();
    expect(feedWindowLabel({ toString: () => "five_hour" })).toBeNull();
    expect(feedWindowLabel(["five_hour"])).toBeNull();
  });
});

describe("runWindowLabel — the SERVER-ALLOWLISTED field (a run row column)", () => {
  it("prefers the human label for a known member", () => {
    expect(runWindowLabel("five_hour")).toBe("5-hour window");
    expect(runWindowLabel("seven_day_opus")).toBe("7-day Opus window");
  });

  it("treats `unknown` as a real member, not an absence", () => {
    // The server WRITES this literal when a worker reports a type outside the set, so
    // "a limit was hit and we do not know which window" is a fact with its own words.
    expect(runWindowLabel("unknown")).toBe("usage window");
  });

  it("renders an unrecognised value HONESTLY — the opposite of the feed path", () => {
    // This is the deliberate asymmetry with feedWindowLabel above. The column cannot
    // hold worker free text (CoerceRateLimitType maps anything else to "unknown"), so
    // a value this build does not know means a NEWER SERVER knows something it does
    // not — and dropping it would hide the one fact the user opened the page for.
    expect(runWindowLabel("nine_hour_experimental")).toBe("nine_hour_experimental");
  });

  it("still strips format characters out of that honest rendering", () => {
    // Belt and braces: the field is allowlisted server-side, but this converges on
    // the same display-time scrub every other free-ish string on this surface gets.
    const out = runWindowLabel("five‮hour​");
    expect(out ?? "").not.toMatch(/[\p{Cf}]/u);
  });

  it("is null only for a genuinely absent value", () => {
    expect(runWindowLabel(null)).toBeNull();
    expect(runWindowLabel(undefined)).toBeNull();
    expect(runWindowLabel("")).toBeNull();
    // Nothing but format characters is indistinguishable from absent once stripped.
    expect(runWindowLabel("‮​")).toBeNull();
  });
});

describe("parseFeedInstant", () => {
  const ms = Date.UTC(2026, 6, 27, 21, 0, 0);

  it("accepts an ISO string", () => {
    expect(parseFeedInstant("2026-07-27T21:00:00.000Z")).toBe(ms);
  });

  it("accepts epoch MILLISECONDS unchanged", () => {
    expect(parseFeedInstant(ms)).toBe(ms);
  });

  it("promotes epoch SECONDS, so a seconds payload does not render as 1970", () => {
    // The wire contract normalizes on the same `< 10^12` threshold, and a payload
    // built straight off that field can carry either. Without this the row would
    // claim the window reopened in January 1970 — plausible-looking garbage, which
    // is worse than no clause.
    expect(parseFeedInstant(ms / 1000)).toBe(ms);
  });

  it("is null for anything that is not a usable instant", () => {
    expect(parseFeedInstant(undefined)).toBeNull();
    expect(parseFeedInstant(null)).toBeNull();
    expect(parseFeedInstant("soon")).toBeNull();
    expect(parseFeedInstant(NaN)).toBeNull();
    expect(parseFeedInstant(Infinity)).toBeNull();
    expect(parseFeedInstant(0)).toBeNull();
    expect(parseFeedInstant(-1)).toBeNull();
    expect(parseFeedInstant({ resets_at: ms })).toBeNull();
  });
});

describe("formatCountdown", () => {
  const now = Date.UTC(2026, 6, 27, 12, 0, 0);
  const inMs = (ms: number) => new Date(now + ms).toISOString();

  it("counts seconds inside the last minute", () => {
    // The one moment the exact number matters, and the reason this is not
    // formatElapsed (runBadge.ts), whose smallest unit is seconds only below 60 too
    // but which then goes straight to minutes.
    expect(formatCountdown(inMs(45_000), now)).toBe("45s");
    expect(formatCountdown(inMs(1_000), now)).toBe("1s");
  });

  it("rounds UP, so a countdown never shows 0s while still waiting", () => {
    expect(formatCountdown(inMs(1), now)).toBe("1s");
  });

  it("counts minutes and seconds under an hour", () => {
    expect(formatCountdown(inMs(90_000), now)).toBe("1m 30s");
    expect(formatCountdown(inMs(605_000), now)).toBe("10m 05s");
  });

  it("counts hours and minutes under a day", () => {
    expect(formatCountdown(inMs(2 * 3_600_000 + 14 * 60_000), now)).toBe("2h 14m");
  });

  it("counts DAYS beyond that — the case both existing formatters get wrong", () => {
    // A park is clamped by RUN_LIMIT_MAX_PARK, whose documented worst case is days.
    // formatElapsed would say "192h 0m" and formatDuration "11520m 00s".
    expect(formatCountdown(inMs(8 * 86_400_000), now)).toBe("8d 0h");
    expect(formatCountdown(inMs(36 * 3_600_000), now)).toBe("1d 12h");
  });

  it("is null once the instant has passed, so nothing counts up into a negative", () => {
    // The promotion pass runs on a ticker: a run whose clock expired is waiting on
    // the next tick, not late. The caller says "Resuming shortly".
    expect(formatCountdown(inMs(0), now)).toBeNull();
    expect(formatCountdown(inMs(-5_000), now)).toBeNull();
  });

  it("degrades a malformed or absent timestamp to the same honest phrasing", () => {
    expect(formatCountdown(null, now)).toBeNull();
    expect(formatCountdown(undefined, now)).toBeNull();
    expect(formatCountdown("", now)).toBeNull();
    expect(formatCountdown("not a date", now)).toBeNull();
  });
});

describe("canToggleWaitOnLimit", () => {
  it("🔴 is ENABLED on an already-parked run, which is not the obvious answer", () => {
    // The toggle changes the NEXT limit, never the current status. A user who parked
    // by accident wants to stop the next park — flipping it off here does not un-park,
    // cancel or fail the run, and Stop is the control that does. The server's endpoint
    // is guarded by the same negative predicate as the cancel path, which admits
    // limit_wait for free; this is the UI agreeing with it.
    expect(canToggleWaitOnLimit("limit_wait")).toBe(true);
  });

  it("is enabled for every other non-terminal status", () => {
    for (const s of ["queued", "claimed", "running", "awaiting_approval"]) {
      expect(canToggleWaitOnLimit(s)).toBe(true);
    }
  });

  it("is inert on every terminal status — there is no future limit to opt into", () => {
    for (const s of ["completed", "failed", "cancelled"]) {
      expect(canToggleWaitOnLimit(s)).toBe(false);
    }
  });
});
