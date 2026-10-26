// Settings → Run defaults → Usage limits: the weekly usage reserve (PRD #3522 M2).
// The 7-day utilization percent at which uzi stops spending the user's subscription,
// plus whether a running run also pauses after its current milestone.
//
// One endpoint owns the whole setting (PUT /api/me/usage-reserve, both fields always
// sent), so every control here writes the pair. Writes are optimistic: the control moves
// at once, a failed write rolls it back to the last saved pair and names the failure. The
// switch saves on click; the threshold saves on blur or Enter, after validating it as a
// whole number 1..100, so a half-typed "7" on the way to "70" is never sent.

import { useId, useState, type KeyboardEvent } from "react";
import { useAuth } from "../auth/AuthContext";
import { api } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Alert, Input, Toggle } from "./ui";

// What the threshold field offers when the reserve is first switched on. High on
// purpose: it holds work only near the end of the window, so turning the reserve on
// never silently stops a user's runs today.
const DEFAULT_RESERVE_PCT = 90;

const SWITCH_LABEL = "Keep a weekly usage reserve";
const RANGE_MESSAGE = "Enter a whole number from 1 to 100.";

interface Reserve {
  pct: number | null;
  pause: boolean;
}

// parseReservePct accepts only a plain whole number 1..100: "1.5", "-3", "1e2" and ""
// are refused here rather than by a server round-trip.
function parseReservePct(raw: string): number | null {
  const trimmed = raw.trim();
  if (!/^\d+$/.test(trimmed)) return null;
  const n = Number(trimmed);
  return n >= 1 && n <= 100 ? n : null;
}

export function UsageReserveSetting() {
  const { user, refresh } = useAuth();
  const ids = useId();
  const helpId = `${ids}-help`;
  const pctId = `${ids}-pct`;
  const pctErrorId = `${ids}-pct-error`;
  const pauseHelpId = `${ids}-pause-help`;

  // The saved pair comes from the session user. `local` holds an optimistic or
  // just-confirmed value until the session refresh catches up; null follows the user.
  const saved: Reserve = {
    pct: user?.weekly_reserve_pct ?? null,
    pause: user?.weekly_reserve_pause_running ?? true,
  };
  const [local, setLocal] = useState<Reserve | null>(null);
  const reserve = local ?? saved;
  const on = reserve.pct !== null;

  const [draft, setDraft] = useState(() => String(user?.weekly_reserve_pct ?? DEFAULT_RESERVE_PCT));
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [inputError, setInputError] = useState("");

  const save = async (next: Reserve) => {
    const previous = reserve;
    setSaveError("");
    setLocal(next);
    setBusy(true);
    try {
      const { user: updated } = await api.setUsageReserve(next.pct, next.pause);
      // Adopt what the server stored; an older response without the fields keeps ours.
      setLocal({
        pct: updated.weekly_reserve_pct === undefined ? next.pct : updated.weekly_reserve_pct,
        pause: updated.weekly_reserve_pause_running ?? next.pause,
      });
      await refresh();
    } catch (err) {
      setLocal(previous);
      if (previous.pct !== null) setDraft(String(previous.pct));
      setSaveError(errorMessage(err, "Failed to update the weekly usage reserve"));
    } finally {
      setBusy(false);
    }
  };

  const toggleReserve = (enable: boolean) => {
    setInputError("");
    if (!enable) {
      void save({ pct: null, pause: reserve.pause });
      return;
    }
    const pct = parseReservePct(draft) ?? DEFAULT_RESERVE_PCT;
    setDraft(String(pct));
    void save({ pct, pause: reserve.pause });
  };

  const commitDraft = () => {
    if (!on) return;
    const pct = parseReservePct(draft);
    if (pct === null) {
      setInputError(RANGE_MESSAGE);
      return;
    }
    setInputError("");
    setDraft(String(pct));
    if (pct !== reserve.pct) void save({ pct, pause: reserve.pause });
  };

  const onPctKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter") {
      e.preventDefault();
      commitDraft();
    } else if (e.key === "Escape" && reserve.pct !== null) {
      setDraft(String(reserve.pct));
      setInputError("");
    }
  };

  // The gauge previews the draft while it is valid, so the reserved slice follows typing.
  const previewPct = parseReservePct(draft) ?? reserve.pct ?? DEFAULT_RESERVE_PCT;

  return (
    <div className="space-y-3 border-t border-edge pt-4">
      <div>
        <h3 className="text-sm font-semibold text-fg">Weekly usage reserve</h3>
        <p id={helpId} className="mt-1 text-sm text-muted">
          Keep the end of your 7-day subscription window for yourself. Once your 7-day usage
          reaches the threshold, uzi stops starting your runs on that credential until a fresh
          reading is below it again. Chat is not affected, and the 5-hour window is ignored.
          Off by default.
        </p>
      </div>

      {saveError && <Alert message={saveError} />}

      <div className="flex items-center gap-3 text-sm">
        <Toggle
          checked={on}
          onChange={toggleReserve}
          disabled={busy}
          focusableWhenDisabled
          label={SWITCH_LABEL}
          aria-describedby={helpId}
        />
        <span className="text-fg" aria-hidden="true">
          {SWITCH_LABEL}
        </span>
      </div>

      {on && (
        <div className="space-y-3 pl-12">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <label htmlFor={pctId} className="text-fg">
              Stop starting runs when your 7-day usage reaches
            </label>
            <span className="inline-flex items-center gap-1.5">
              <Input
                id={pctId}
                type="number"
                inputMode="numeric"
                min={1}
                max={100}
                step={1}
                className="w-20 tabular-nums"
                value={draft}
                aria-invalid={inputError !== "" || undefined}
                aria-describedby={inputError ? pctErrorId : undefined}
                onChange={(e) => setDraft(e.target.value)}
                onBlur={commitDraft}
                onKeyDown={onPctKeyDown}
              />
              <span className="text-muted" aria-hidden="true">
                %
              </span>
            </span>
          </div>
          {inputError && (
            <p id={pctErrorId} className="text-xs text-danger" role="alert">
              {inputError}
            </p>
          )}

          {/* The window as one bar: the brand slice past the threshold is what the
              reserve keeps for you. Decorative; the sentence beside it says the same. */}
          <div className="max-w-sm space-y-1.5">
            <div aria-hidden="true" className="relative h-1.5 overflow-hidden rounded-full bg-edge">
              <div
                className="absolute inset-y-0 right-0 bg-brand/40"
                style={{ width: `${100 - previewPct}%` }}
              />
              <div
                className="absolute inset-y-0 w-0.5 bg-brand"
                style={{ left: `calc(${previewPct}% - 1px)` }}
              />
            </div>
            <p className="text-xs text-faint">
              {previewPct === 100
                ? "Runs start until the window is used up."
                : `The last ${100 - previewPct}% of your 7-day window stays free for chat and your own use.`}
            </p>
          </div>

          <label className="flex items-center gap-3 text-sm">
            <input
              type="checkbox"
              className="h-4 w-4 accent-brand"
              checked={reserve.pause}
              disabled={busy}
              aria-describedby={pauseHelpId}
              onChange={(e) => void save({ pct: reserve.pct, pause: e.target.checked })}
            />
            <span className="text-fg">Also pause running runs after their current milestone</span>
          </label>
          <p id={pauseHelpId} className="text-xs text-faint">
            A running run finishes the milestone it is on, checkpoints, and pauses. It resumes by
            itself once a fresh reading is below the threshold, or straight away when you resume it.
          </p>
          <p className="text-xs text-faint">
            Best effort: uzi acts on the latest usage reading, so a run can go a little past the
            threshold while it finishes its milestone. With no fresh reading, new work is held
            rather than started.
          </p>
        </div>
      )}
    </div>
  );
}
