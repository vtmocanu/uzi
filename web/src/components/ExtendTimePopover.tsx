// PRD #1189: the Extend chooser — the owner's control for granting a run more wall-clock
// time. Extend NEVER adds time on the first click (Decision 5): it always opens this chooser
// with +2h preselected, showing what is used, how much allowance is left, and the deadline the
// pick would produce, so a state-changing action never hides its magnitude behind one tap.
//
// It is one component reused in three places — the header (while the near-timeout flag is up),
// the near-timeout panel, and the paused panel — so the chooser math and copy live in ONE
// place. The open/close model + absolute panel mirror BuildInfoPopover; the trigger variant
// and label are props so each host renders the button that fits it.
//
// The caller decides WHETHER to render this at all (it hides the whole affordance when the
// cap is unknown or 0 — see budget.extendEnabled). If it is rendered with a 0 cap anyway, the
// panel degrades honestly to "extensions are turned off" rather than offering a Confirm that
// would 409.

import { useEffect, useId, useRef, useState } from "react";
import {
  extendBudgetView,
  formatBudgetDuration,
  formatLocalTime,
  parseDurationToSeconds,
  type BudgetRun,
} from "../lib/budget";
import { useNow } from "../lib/useNow";
import { Button, cx } from "./ui";

// The quick-pick amounts, in seconds: +1h / +2h / +4h / +8h. +2h is preselected (index 1).
const QUICK_PICKS = [3600, 7200, 14400, 28800] as const;
const DEFAULT_PICK = 7200;

// The trigger button variants this popover is used with (a subset of ui.Button's variants).
type Variant = "primary" | "secondary";

export function ExtendTimePopover({
  run,
  busy,
  onSubmit,
  triggerLabel = "Extend time…",
  triggerVariant = "secondary",
}: {
  run: BudgetRun;
  busy: boolean;
  // Wired by the page to act(() => submit("extend", String(seconds))) → refreshRun. Awaited so
  // the panel closes only once the write settled (or its error surfaced on the page banner).
  onSubmit: (seconds: number) => void | Promise<void>;
  triggerLabel?: string;
  triggerVariant?: Variant;
}) {
  const [open, setOpen] = useState(false);
  const [picked, setPicked] = useState<number>(DEFAULT_PICK);
  const [custom, setCustom] = useState("");
  const hostRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const panelId = useId();
  // How far to nudge the panel horizontally so it never renders off-screen. The panel is
  // anchored `right-0`, so on a narrow (mobile) viewport its left edge — the +1h quick-pick and
  // the left labels — can fall past the LEFT viewport edge when the trigger is not hard against
  // the right. This shift, measured once the panel is laid out, pushes it back on-screen.
  const [shiftPx, setShiftPx] = useState(0);

  // Tick only while open, and slowly (5s): the facts drift with the deadline countdown but a
  // per-second tick on a small chooser is waste, and the numbers are glance values.
  const now = useNow(open ? 5000 : null);

  // Escape closes it, and an outside click closes it — both attached only while open, so a shut
  // popover costs nothing (there can be several on the page).
  //
  // Focus management (a11y, role="dialog"): moving focus INTO the panel on open means a
  // keyboard/AT user lands inside the chooser, and RETURNING focus to the trigger on close
  // (Escape or click-away) keeps them where they were instead of dropping onto <body>.
  useEffect(() => {
    if (!open) return;
    const focusTrigger = () =>
      hostRef.current?.querySelector<HTMLButtonElement>(":scope > button")?.focus();
    // Move focus into the panel itself (tabIndex=-1) so the first Tab lands on a control inside.
    panelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setOpen(false);
        focusTrigger();
      }
    };
    const onDown = (e: MouseEvent) => {
      if (hostRef.current && !hostRef.current.contains(e.target as Node)) {
        setOpen(false);
        focusTrigger();
      }
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onDown);
    };
  }, [open]);

  // Keep the panel inside the viewport at mobile width. Measured from the host's right edge and
  // the panel's laid-out width (both independent of any shift already applied, so this is
  // idempotent across re-measures), then re-run on resize. In a no-layout environment (jsdom in
  // tests) clientWidth is 0, so we skip and apply no transform.
  useEffect(() => {
    if (!open) {
      setShiftPx(0);
      return;
    }
    const clamp = () => {
      const host = hostRef.current;
      const panel = panelRef.current;
      if (!host || !panel) return;
      const vw = document.documentElement.clientWidth;
      if (!vw) return;
      const margin = 8;
      const width = panel.offsetWidth;
      const hostRight = host.getBoundingClientRect().right;
      const naturalLeft = hostRight - width; // right-0 anchors the panel to the host's right edge
      let shift = 0;
      if (naturalLeft < margin) shift = margin - naturalLeft; // off the LEFT edge → push right
      else if (hostRight > vw - margin) shift = vw - margin - hostRight; // off the right → pull left
      setShiftPx(shift);
    };
    clamp();
    window.addEventListener("resize", clamp);
    return () => window.removeEventListener("resize", clamp);
  }, [open]);

  const view = extendBudgetView(run, now);
  const capDisabled = (view?.capSec ?? run.budget_extension_cap_seconds ?? 0) <= 0;

  // The effective pick: a non-empty custom input overrides the quick pick. parseDurationToSeconds
  // returns null for an unparseable custom value, which disables Confirm with a hint.
  const customTrimmed = custom.trim();
  const effective = customTrimmed !== "" ? parseDurationToSeconds(custom) : picked;

  const allowanceLeft = view?.allowanceLeftSec ?? 0;
  const overAllowance = effective != null && effective > allowanceLeft;
  const canConfirm = !busy && !capDisabled && effective != null && !overAllowance && allowanceLeft > 0;

  // The disabled reason, named so the owner knows why: an invalid custom value, or a pick above
  // what the allowance still permits (naming the remaining allowance AND the cap).
  let reason: string | null = null;
  if (!capDisabled && view) {
    if (allowanceLeft <= 0) {
      reason = `This run has used its full ${formatBudgetDuration(view.capSec)} extension allowance.`;
    } else if (customTrimmed !== "" && effective == null) {
      reason = "Enter a duration like 2h, 90m or 1h30m.";
    } else if (overAllowance && effective != null) {
      reason = `+${formatBudgetDuration(effective)} is more than the ${formatBudgetDuration(
        allowanceLeft,
      )} left of this run's ${formatBudgetDuration(view.capSec)} extension allowance.`;
    }
  }

  const submit = async () => {
    if (!canConfirm || effective == null) return;
    await onSubmit(effective);
    setOpen(false);
    setCustom("");
    setPicked(DEFAULT_PICK);
  };

  return (
    <div ref={hostRef} className="relative inline-block">
      <Button
        type="button"
        variant={triggerVariant}
        size="sm"
        disabled={busy}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        {triggerLabel}
      </Button>
      {open && (
        <div
          id={panelId}
          ref={panelRef}
          role="dialog"
          aria-label="Extend this run"
          tabIndex={-1}
          style={shiftPx ? { transform: `translateX(${shiftPx}px)` } : undefined}
          className="absolute right-0 top-full z-20 mt-2 w-72 max-w-[calc(100vw-1rem)] rounded-xl border border-edge bg-raised p-3 text-left shadow-2xl outline-hidden"
        >
          <p className="text-sm font-semibold text-fg">Extend time</p>
          {capDisabled || !view ? (
            <p className="mt-2 text-xs text-muted">
              Extensions are turned off for this instance. Ask an admin to raise the extension
              allowance to grant a run more time.
            </p>
          ) : (
            <>
              {/* Quick picks: a non-empty custom field overrides the highlighted pick. */}
              <div className="mt-2 flex flex-wrap gap-1.5">
                {QUICK_PICKS.map((sec) => {
                  const active = customTrimmed === "" && picked === sec;
                  const tooBig = sec > allowanceLeft;
                  return (
                    <button
                      key={sec}
                      type="button"
                      disabled={tooBig}
                      onClick={() => {
                        setPicked(sec);
                        setCustom("");
                      }}
                      className={cx(
                        "rounded-md border px-2 py-1 text-xs font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-40",
                        active
                          ? "border-brand bg-brand/10 text-fg"
                          : "border-edge bg-raised text-muted hover:border-edge-strong hover:text-fg",
                      )}
                    >
                      +{formatBudgetDuration(sec)}
                    </button>
                  );
                })}
              </div>
              <label className="mt-2 block text-xs text-muted">
                <span className="sr-only">Custom duration</span>
                <input
                  type="text"
                  inputMode="text"
                  placeholder="Custom, e.g. 1h30m"
                  value={custom}
                  onChange={(e) => setCustom(e.target.value)}
                  className="w-full rounded-md border border-edge bg-raised px-2 py-1 text-xs text-fg placeholder:text-faint outline-hidden focus:border-brand/70"
                />
              </label>

              {/* The facts the pick is chosen against (mock frame B): what is used, what
                  allowance is left, and the deadline the pick would produce. */}
              <dl className="mt-3 space-y-1 text-xs text-muted">
                <div className="flex justify-between gap-2">
                  <dt>Used</dt>
                  <dd className="tabular-nums text-fg">
                    {formatBudgetDuration(view.usedSec)} of {formatBudgetDuration(view.wallSec)}
                  </dd>
                </div>
                <div className="flex justify-between gap-2">
                  <dt>Allowance left</dt>
                  <dd className="tabular-nums text-fg">
                    {formatBudgetDuration(view.allowanceLeftSec)} of {formatBudgetDuration(view.capSec)}
                  </dd>
                </div>
                {effective != null && !overAllowance && (
                  <div className="flex justify-between gap-2">
                    <dt>After +{formatBudgetDuration(effective)}</dt>
                    <dd className="tabular-nums text-fg">
                      {view.running
                        ? `${formatBudgetDuration(view.remainingSec + effective)} left${
                            afterDeadline(view.deadlineIso, effective)
                              ? `, stops at ${afterDeadline(view.deadlineIso, effective)}`
                              : ""
                          }`
                        : `${formatBudgetDuration(view.remainingSec + effective)} left when resumed`}
                    </dd>
                  </div>
                )}
              </dl>

              {reason && <p className="mt-2 text-xs text-warn">{reason}</p>}

              <div className="mt-3 flex items-center justify-between gap-2">
                <span className="text-[11px] text-faint">Owner only. Logged in the run's history.</span>
                <Button type="button" variant="primary" size="sm" disabled={!canConfirm} onClick={submit}>
                  {effective != null ? `Extend by ${formatBudgetDuration(effective)}` : "Extend"}
                </Button>
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}

// afterDeadline renders the local wall-clock deadline a +N pick would produce, or null when
// there is no live deadline (a paused run's deadline moves, so the chooser says "left when
// resumed" instead of naming a time).
function afterDeadline(deadlineIso: string | null, addSeconds: number): string | null {
  if (!deadlineIso) return null;
  const t = Date.parse(deadlineIso);
  if (!Number.isFinite(t)) return null;
  return formatLocalTime(new Date(t + addSeconds * 1000).toISOString());
}
