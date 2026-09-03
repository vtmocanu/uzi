// SchedulePauseControl — the "Pause all schedules" UI for the Schedules page (PRD #1093
// M4, Solution 6 + D9). Three visual states, ONE control per state:
//
//   - Running  → PauseAllButton, a secondary sm button at the right end of the tab row.
//   - Picker   → PausePanel, an inline warn-toned card in the banner slot between the
//                header and the tab row (covers nothing on the page).
//   - Paused   → PausedBanner, a warn-toned banner carrying Change… / Resume now.
//
// The page owns the state machine (loaded pause DTO + a pickerOpen flag) and places each
// piece; this file owns only the rendering + the preset picker's local form state.

import { useState } from "react";
import type { SchedulePauseDTO } from "../lib/api";
import { Button } from "./ui";
import { ChevronDownIcon } from "./icons";
import { formatStamp } from "./LastRun";
import { relativeFromNow } from "./ScheduleModal";
import {
  DEFAULT_PAUSE_PRESET,
  type PausePreset,
  resolvePausePresets,
  resolvePreset,
} from "../lib/schedulePausePresets";

// The one scope sentence shown in both the picker and the paused banner, so the two
// never drift. Kept as a single string constant to make the copy greppable.
const PAUSE_SCOPE_SENTENCE =
  "Every schedule you own, on every repo, default jobs and your own. Per-schedule switches are left as they are. Run now still works; runs already in flight are not stopped.";

// PauseAllButton — the tab-row control shown only in the running state (D9: right end of
// the tab row, under "New schedule", adding no header height).
export function PauseAllButton({ onClick, disabled }: { onClick: () => void; disabled?: boolean }) {
  return (
    <Button variant="secondary" size="sm" className="ml-auto" onClick={onClick} disabled={disabled}>
      Pause all
      <ChevronDownIcon className="h-3.5 w-3.5" />
    </Button>
  );
}

// PausedBanner — the warn-toned banner shown while paused. The tab-row button yields to
// this (one control per state); it carries the scope sentence and the two actions.
export function PausedBanner({
  pause,
  onChange,
  onResume,
  busy,
}: {
  pause: SchedulePauseDTO;
  onChange: () => void;
  onResume: () => void;
  busy: boolean;
}) {
  const headline = pause.until
    ? `All schedules paused until ${formatStamp(pause.until)} (${relativeFromNow(pause.until)})`
    : "All schedules paused until you resume them";
  return (
    <div className="rounded-xl border border-warn/40 bg-warn/10 p-4">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="max-w-xl">
          <p className="text-sm font-semibold text-warn">{headline}</p>
          <p className="mt-1 text-[12.5px] text-muted">{PAUSE_SCOPE_SENTENCE}</p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button variant="secondary" size="sm" onClick={onChange} disabled={busy}>
            Change…
          </Button>
          <Button size="sm" onClick={onResume} disabled={busy}>
            Resume now
          </Button>
        </div>
      </div>
    </div>
  );
}

// PausePanel — the inline preset picker (the "Picker" state). A pure preset row list, a
// custom datetime-local input, and a Cancel / primary submit pair. onSubmit receives the
// resolved absolute `until` as an RFC3339 string, or null for an indefinite pause.
export function PausePanel({
  onSubmit,
  onCancel,
  busy,
}: {
  onSubmit: (until: string | null) => void;
  onCancel: () => void;
  busy: boolean;
}) {
  // Pin `now` at mount so the resolved preset stamps are stable across re-renders (and a
  // fixed clock in a test resolves deterministically).
  const [now] = useState(() => new Date());
  const [preset, setPreset] = useState<PausePreset>(DEFAULT_PAUSE_PRESET);
  const [customLocal, setCustomLocal] = useState("");
  const presets = resolvePausePresets(now);

  const resolved = resolvePreset(preset, now, customLocal);
  // Gate on the RESOLVED instant, not on the raw field: a non-empty but unparseable
  // datetime-local value resolves to null, and null on submit means "indefinitely",
  // which is not what a custom pick intends.
  const customUnset = preset === "custom" && resolved === null;
  const submitLabel =
    preset === "indefinite"
      ? "Pause all indefinitely"
      : resolved
        ? `Pause all until ${formatStamp(resolved.toISOString())}`
        : "Pause all";

  const options: { value: PausePreset; label: string; resolved?: Date }[] = [
    { value: "tomorrow", label: "Until tomorrow 09:00", resolved: presets.tomorrow },
    { value: "24h", label: "For 24 hours", resolved: presets.in24h },
    { value: "monday", label: "Until Monday 09:00", resolved: presets.monday },
    { value: "indefinite", label: "Until I resume" },
    { value: "custom", label: "Custom date & time…" },
  ];

  return (
    <div className="rounded-xl border border-warn/40 bg-warn/10 p-4">
      <h3 className="text-sm font-semibold text-warn">Pause all schedules</h3>
      <p className="mt-1 max-w-xl text-[12.5px] text-muted">{PAUSE_SCOPE_SENTENCE}</p>

      <fieldset className="mt-3 flex flex-col gap-1.5" aria-label="Auto-resume">
        {options.map((o) => {
          const id = `pause-preset-${o.value}`;
          return (
            <div key={o.value}>
              <label htmlFor={id} className="flex items-center gap-2 text-[13px] text-fg">
                <input
                  id={id}
                  type="radio"
                  name="pause-preset"
                  value={o.value}
                  checked={preset === o.value}
                  onChange={() => setPreset(o.value)}
                  className="accent-brand"
                />
                <span>{o.label}</span>
                {o.resolved && (
                  <span className="font-mono text-[11.5px] text-faint">
                    {formatStamp(o.resolved.toISOString())}
                  </span>
                )}
              </label>
              {o.value === "custom" && preset === "custom" && (
                <input
                  type="datetime-local"
                  aria-label="Custom pause-until date and time"
                  value={customLocal}
                  onChange={(e) => setCustomLocal(e.target.value)}
                  className="ml-6 mt-1.5 rounded-md border border-edge bg-raised px-2 py-1 font-mono text-[12px] text-fg outline-hidden focus:border-brand/70"
                />
              )}
            </div>
          );
        })}
      </fieldset>

      <div className="mt-4 flex items-center justify-end gap-2">
        <Button variant="secondary" size="sm" onClick={onCancel} disabled={busy}>
          Cancel
        </Button>
        <Button
          size="sm"
          disabled={busy || customUnset}
          onClick={() => onSubmit(resolved ? resolved.toISOString() : null)}
        >
          {submitLabel}
        </Button>
      </div>
    </div>
  );
}
