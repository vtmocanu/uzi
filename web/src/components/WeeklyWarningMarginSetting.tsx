// Settings → Run defaults → Usage limits: the weekly-limit warning margin (PRD #4123 module 6).
// How many percentage points before a credential's effective weekly limit (the reserve when
// set, else 100%) uzi starts warning: the run-page banner and the Overview decision item.
// 0 turns those early warnings off; a run parked on its weekly limit still explains
// why, whatever the margin.
//
// One endpoint owns it (PUT /api/me/weekly-warning-margin, the value read back from GET
// /me/settings), so the field saves on blur or Enter after validating a whole number 0..50: a
// half-typed "1" on the way to "15" is never sent. Writes are optimistic and serialized like
// UsageReserveSetting: a value committed while one is in flight is sent right after it, the
// last intent wins, and a failed write rolls the field back to the last stored value and names
// the failure.

import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { useAuth } from "../auth/AuthContext";
import { api } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Alert, Input } from "./ui";

const MAX_WARNING_MARGIN_PTS = 50;
const RANGE_MESSAGE = `Enter a whole number from 0 to ${MAX_WARNING_MARGIN_PTS}.`;

// parseMargin accepts only a plain whole number 0..50: "1.5", "-3", "1e1" and "" are refused
// here rather than by a server round-trip.
function parseMargin(raw: string): number | null {
  const trimmed = raw.trim();
  if (!/^\d+$/.test(trimmed)) return null;
  const n = Number(trimmed);
  return n <= MAX_WARNING_MARGIN_PTS ? n : null;
}

// What the margin means for this owner, in one sentence. The limit it counts back from is the
// reserve when one is set, so the sentence names the actual percentage warnings start at.
function effectText(margin: number, reservePct: number | null): string {
  if (margin === 0) {
    return "Early warnings are off: no banner and no Overview item before a limit. A run parked on its weekly limit still says why and what you can change.";
  }
  const limit = reservePct ?? 100;
  const start = Math.max(0, limit - margin);
  return reservePct !== null
    ? `Warnings start at ${start}% weekly usage, ${margin} points before your ${limit}% reserve.`
    : `Warnings start at ${start}% weekly usage, ${margin} points before the limit.`;
}

export function WeeklyWarningMarginSetting({
  saved,
  onSaved,
}: {
  /** The stored margin from GET /me/settings. */
  saved: number;
  /** Called with each value the server confirms, so the page's settings copy stays current. */
  onSaved: (marginPts: number) => void;
}) {
  const { user } = useAuth();
  const ids = useId();
  const helpId = `${ids}-help`;
  const inputId = `${ids}-input`;
  const errorId = `${ids}-error`;
  const effectId = `${ids}-effect`;

  // The value last confirmed by the server; the field shows the draft while it is edited.
  const [stored, setStored] = useState(saved);
  const [draft, setDraft] = useState(() => String(saved));
  const [inputError, setInputError] = useState("");
  const [saveError, setSaveError] = useState("");
  const editing = useRef(false);
  const pending = useRef<number | null>(null);
  const running = useRef(false);
  // The newest intent, else what is stored: what a new commit compares against.
  const intended = useRef(saved);

  // A change made elsewhere (another tab, the CLI) arrives through the page's next settings
  // read; adopt it unless the owner is typing or a save of ours is in flight.
  useEffect(() => {
    if (running.current || editing.current) return;
    setStored(saved);
    intended.current = saved;
    setDraft(String(saved));
  }, [saved]);

  const save = async (next: number) => {
    setSaveError("");
    pending.current = next;
    intended.current = next;
    if (running.current) return;
    running.current = true;
    let confirmed = stored;
    try {
      while (pending.current !== null) {
        const sending = pending.current;
        pending.current = null;
        const res = await api.setWeeklyWarningMargin(sending);
        confirmed = typeof res.weekly_warning_margin_pts === "number" ? res.weekly_warning_margin_pts : sending;
        setStored(confirmed);
        onSaved(confirmed);
      }
    } catch (err) {
      // The failed write and anything queued behind it are dropped; show the last stored value.
      pending.current = null;
      intended.current = confirmed;
      setStored(confirmed);
      if (!editing.current) setDraft(String(confirmed));
      setSaveError(errorMessage(err, "Failed to update the weekly limit warning"));
    } finally {
      running.current = false;
    }
  };

  const commit = () => {
    editing.current = false;
    const n = parseMargin(draft);
    if (n === null) {
      setInputError(RANGE_MESSAGE);
      return;
    }
    setInputError("");
    setDraft(String(n));
    if (n !== intended.current) void save(n);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter") {
      e.preventDefault();
      commit();
    } else if (e.key === "Escape") {
      setDraft(String(intended.current));
      setInputError("");
    }
  };

  // The effect line previews the draft while it is valid, so it follows typing.
  const preview = parseMargin(draft) ?? intended.current;

  return (
    <div className="space-y-3 border-t border-edge pt-4">
      <div>
        <h3 className="text-sm font-semibold text-fg">Weekly limit warning</h3>
        <p id={helpId} className="mt-1 text-sm text-muted">
          Hear about a credential before its 7-day allowance runs out. Once its weekly usage is
          within this many points of the limit (your weekly reserve when set, otherwise 100%), the
          run page and the Overview show whether each affected run can move and what to change. A linked Slack account also gets one DM per observed weekly period. Set 0 to turn early warnings off, and the DM with them.
        </p>
      </div>

      {saveError && <Alert message={saveError} />}

      <div className="flex flex-wrap items-center gap-2 text-sm">
        <label htmlFor={inputId} className="text-fg">
          Warn this many points before the limit
        </label>
        <span className="inline-flex items-center gap-1.5">
          <Input
            id={inputId}
            type="number"
            inputMode="numeric"
            min={0}
            max={MAX_WARNING_MARGIN_PTS}
            step={1}
            className="w-20 tabular-nums"
            value={draft}
            aria-invalid={inputError !== "" || undefined}
            aria-describedby={[helpId, inputError ? errorId : effectId].join(" ")}
            onChange={(e) => setDraft(e.target.value)}
            onFocus={() => {
              editing.current = true;
            }}
            onBlur={commit}
            onKeyDown={onKeyDown}
          />
          <span className="text-muted" aria-hidden="true">
            points
          </span>
        </span>
      </div>
      {inputError ? (
        <p id={errorId} className="text-xs text-danger" role="alert">
          {inputError}
        </p>
      ) : (
        <p id={effectId} data-margin-effect="" className="text-xs text-faint">
          {effectText(preview, user?.weekly_reserve_pct ?? null)}
        </p>
      )}
    </div>
  );
}
