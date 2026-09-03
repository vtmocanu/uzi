// Pause-all preset resolution (PRD #1093 M4, Solution 6).
//
// The Schedules page's "Pause all" picker offers a few presets, each of which resolves
// to a concrete auto-resume instant. The resolution is a PURE function of `now` (a Date)
// so the picker's tests can pin a fixed clock and assert the exact resolved stamps — no
// hidden `Date.now()` / `new Date()` inside. All arithmetic is in LOCAL time (the browser
// timezone), matching the CLI's client-side resolution rule (D8); the server only ever
// receives an absolute RFC3339 instant.

// The preset the picker is currently on. "indefinite" resolves to null (until I resume);
// "custom" reads a datetime-local input instead of a computed instant.
export type PausePreset = "tomorrow" | "24h" | "monday" | "indefinite" | "custom";

// The default preset — "tonight" is the motivating case, so tomorrow-09:00 is default (D9).
export const DEFAULT_PAUSE_PRESET: PausePreset = "tomorrow";

// The resolved instants for the three computed presets, given `now`.
export interface ResolvedPausePresets {
  // The next calendar day at 09:00 local.
  tomorrow: Date;
  // Exactly now + 24 hours.
  in24h: Date;
  // The next Monday at 09:00 local, STRICTLY after now (today if it is Monday and 09:00
  // is still ahead, else the following Monday). Same rule as the CLI's weekday form.
  monday: Date;
}

export function resolvePausePresets(now: Date): ResolvedPausePresets {
  const tomorrow = new Date(now);
  tomorrow.setDate(tomorrow.getDate() + 1);
  tomorrow.setHours(9, 0, 0, 0);

  const in24h = new Date(now.getTime() + 24 * 60 * 60 * 1000);

  const monday = new Date(now);
  monday.setHours(9, 0, 0, 0);
  // getDay(): 0=Sun … 1=Mon … 6=Sat. Days until the next Monday (0 when today is Monday).
  const daysUntilMonday = (1 - monday.getDay() + 7) % 7;
  let add = daysUntilMonday;
  // Strictly-after-now: if today is Monday but 09:00 has already passed (or is exactly
  // now), jump to next Monday rather than an instant in the past.
  if (add === 0 && monday.getTime() <= now.getTime()) add = 7;
  monday.setDate(monday.getDate() + add);
  return { tomorrow, in24h, monday };
}

// resolvePreset maps a chosen preset to its absolute `until` instant, or null for the
// indefinite / custom-unset cases. `customLocal` is the raw datetime-local input value
// (e.g. "2026-09-04T09:00", browser timezone) when the preset is "custom".
export function resolvePreset(
  preset: PausePreset,
  now: Date,
  customLocal?: string,
): Date | null {
  const presets = resolvePausePresets(now);
  switch (preset) {
    case "tomorrow":
      return presets.tomorrow;
    case "24h":
      return presets.in24h;
    case "monday":
      return presets.monday;
    case "indefinite":
      return null;
    case "custom": {
      if (!customLocal) return null;
      const d = new Date(customLocal);
      return Number.isNaN(d.getTime()) ? null : d;
    }
  }
}
