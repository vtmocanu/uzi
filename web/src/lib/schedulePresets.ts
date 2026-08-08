// Preset ↔ cron translation for the Schedules modal (PRD #241 M5, Decision 6).
//
// The CRON STRING is the canonical form — it is what the API stores and what the
// CLI accepts, so web and CLI agree byte-for-byte. Presets are a thin, LOSSLESS
// translation layer on top: selecting a preset builds a cron string; editing the
// raw cron field that no longer matches any preset flips the dropdown to "Custom".
//
// The four preset cron shapes match M2's canonical forms EXACTLY:
//   weekdays  = `M H * * 1-5`
//   daily     = `M H * * *`
//   weekly    = `M H * * 1`   (Monday)
//   everyN    = `0 */N * * *`
// so cronFromPreset(presetFromCron(c)) === c for every c a preset can produce.

export type CronPreset =
  | "weekdays"
  | "daily"
  | "weekly"
  | "everyNHours"
  | "custom";

// PresetState is the modal's cadence sub-state: the chosen preset plus the two
// parameters presets read — a wall-clock time (for the day-based presets) and an
// interval N (for the every-N-hours preset). A custom preset ignores both and
// edits the raw cron directly.
export interface PresetState {
  preset: CronPreset;
  // 0..23
  hour: number;
  // 0..59
  minute: number;
  // 1..23 (the step in `0 */N * * *`)
  everyN: number;
}

// The default cadence a fresh recurring schedule opens on: weekdays at 02:00, the
// mock's example (§2). Exported so the modal and its tests seed from one place.
export const DEFAULT_PRESET_STATE: PresetState = {
  preset: "weekdays",
  hour: 2,
  minute: 0,
  everyN: 6,
};

// The dropdown options, in the mock's order. `custom` is last.
export const PRESET_OPTIONS: { value: CronPreset; label: string }[] = [
  { value: "weekdays", label: "Weekdays" },
  { value: "daily", label: "Every day" },
  { value: "weekly", label: "Every week (Monday)" },
  { value: "everyNHours", label: "Every N hours" },
  { value: "custom", label: "Custom (cron)…" },
];

function pad2(n: number): string {
  return n.toString().padStart(2, "0");
}

// hhmm renders a PresetState's time as an HH:MM string for the <input type="time">.
export function hhmm(state: PresetState): string {
  return `${pad2(state.hour)}:${pad2(state.minute)}`;
}

// parseHHMM reads an HH:MM string back into {hour, minute}, clamped to valid
// ranges; an unparseable value keeps the current time (returns null).
export function parseHHMM(v: string): { hour: number; minute: number } | null {
  const m = /^(\d{1,2}):(\d{2})$/.exec(v.trim());
  if (!m) return null;
  const hour = Number(m[1]);
  const minute = Number(m[2]);
  if (hour < 0 || hour > 23 || minute < 0 || minute > 59) return null;
  return { hour, minute };
}

// cronFromPreset builds the canonical 5-field cron string for a preset + params.
// A custom preset has no canonical form here — the caller edits the raw cron
// directly — so it returns the empty string, which the modal treats as "keep the
// raw value the user is typing".
export function cronFromPreset(state: PresetState): string {
  const { preset, hour, minute, everyN } = state;
  switch (preset) {
    case "weekdays":
      return `${minute} ${hour} * * 1-5`;
    case "daily":
      return `${minute} ${hour} * * *`;
    case "weekly":
      return `${minute} ${hour} * * 1`;
    case "everyNHours":
      return `0 */${everyN} * * *`;
    case "custom":
      return "";
  }
}

const INT_RE = /^\d{1,2}$/;

// presetFromCron is the inverse: it recognises exactly the four canonical shapes
// cronFromPreset produces and returns the matching PresetState; anything else
// (extra fields, ranges, lists, unhandled steps) resolves to `custom`, which is
// what flips the dropdown when a user hand-edits the raw cron. The returned
// everyN/hour/minute on a `custom` result are the passed-through fallbacks so the
// modal's parameter inputs keep a sensible value.
export function presetFromCron(
  cron: string,
  fallback: PresetState = DEFAULT_PRESET_STATE,
): PresetState {
  const custom: PresetState = { ...fallback, preset: "custom" };
  const fields = cron.trim().split(/\s+/);
  if (fields.length !== 5) return custom;
  const [min, hr, dom, mon, dow] = fields;
  if (dom !== "*" || mon !== "*") return custom;

  // Every-N-hours: `0 */N * * *`.
  const stepMatch = /^\*\/(\d{1,2})$/.exec(hr);
  if (min === "0" && stepMatch && dow === "*") {
    const everyN = Number(stepMatch[1]);
    if (everyN >= 1 && everyN <= 23) {
      return { preset: "everyNHours", hour: 0, minute: 0, everyN };
    }
    return custom;
  }

  // Day-based presets: plain integer minute + hour.
  if (!INT_RE.test(min) || !INT_RE.test(hr)) return custom;
  const minute = Number(min);
  const hour = Number(hr);
  if (minute > 59 || hour > 23) return custom;

  switch (dow) {
    case "1-5":
      return { preset: "weekdays", hour, minute, everyN: fallback.everyN };
    case "*":
      return { preset: "daily", hour, minute, everyN: fallback.everyN };
    case "1":
      return { preset: "weekly", hour, minute, everyN: fallback.everyN };
    default:
      return custom;
  }
}

// humanizeCron renders a short, friendly description of a cron string for the list
// page's "When" column, falling back to the raw expression for anything it does
// not recognise as a preset. Timezone is appended by the caller.
export function humanizeCron(cron: string): string {
  const st = presetFromCron(cron);
  const at = `${pad2(st.hour)}:${pad2(st.minute)}`;
  switch (st.preset) {
    case "weekdays":
      return `Weekdays at ${at}`;
    case "daily":
      return `Every day at ${at}`;
    case "weekly":
      return `Every Monday at ${at}`;
    case "everyNHours":
      return st.everyN === 1 ? "Every hour" : `Every ${st.everyN} hours`;
    case "custom":
      return cron;
  }
}
