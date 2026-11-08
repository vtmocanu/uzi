// The People chip for the /overview/next header (issue #4042, admins only; the page decides
// whether to mount it). A compact "N active now of M" trigger with an avatar stack opens a
// popover listing who is doing what, who signed in recently and the week's usage. All figures
// come from the pure peopleModel; this component only draws it. The open/close model, focus
// handling and absolute panel follow ExtendTimePopover.

import { useCallback, useEffect, useId, useRef, useState } from "react";
import { idleText, type ActivePerson, type PeopleModel } from "../lib/overviewPeople";
import { cx } from "./ui";

const INFO_TEXT =
  "Counts people with listed active work: chat, judge and cross-check runs are not listed, and the list stops at 500 runs.";

const TONE_CLASS = {
  ok: "bg-ok/15 text-ok",
  warn: "bg-warn/15 text-warn",
  neutral: "border border-edge bg-surface text-muted",
} as const;

function Avatar({ initials, small = false }: { initials: string; small?: boolean }) {
  return (
    <span
      aria-hidden="true"
      className={cx(
        "inline-flex shrink-0 items-center justify-center rounded-full border border-edge bg-raised font-semibold text-fg",
        small ? "h-5 w-5 text-[9px]" : "h-6 w-6 text-[10px]",
      )}
    >
      {initials}
    </span>
  );
}

function GroupLabel({ children }: { children: string }) {
  return <p className="mb-1 text-[11px] font-medium uppercase tracking-wide text-faint">{children}</p>;
}

function Marks({ you, deactivated = false }: { you: boolean; deactivated?: boolean }) {
  return (
    <>
      {you && <span className="text-muted"> (you)</span>}
      {deactivated && <span className="text-muted"> (deactivated)</span>}
    </>
  );
}

function chipText(active: PeopleModel["active"], total: number | null) {
  if (active.state === "unavailable") return { main: "Active now unavailable", of: null };
  if (active.state === "loading") return { main: "", of: null };
  return {
    main: `${active.atLeast ? "at least " : ""}${active.count} active now`,
    of: total != null ? `of ${total}` : null,
  };
}

function headerText(active: PeopleModel["active"], total: number | null): string {
  if (active.state === "unavailable") return "Active now unavailable";
  if (active.state === "loading") return "";
  const lead = active.atLeast ? "At least " : "";
  if (total != null) return `${lead}${active.count} of ${total} people active now`;
  return `${lead}${active.count} ${active.count === 1 ? "person" : "people"} active now`;
}

export function PeopleChip({ model, onShowUsageByUser }: { model: PeopleModel; onShowUsageByUser: () => void }) {
  const [open, setOpen] = useState(false);
  const hostRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  // Pending click-away refocus frame, cancelled only on unmount (see ExtendTimePopover).
  const refocusRaf = useRef(0);
  const panelId = useId();

  // keydown has no focus default action, so returning focus synchronously on Escape wins.
  const close = useCallback((returnFocus: boolean) => {
    setOpen(false);
    if (returnFocus) triggerRef.current?.focus();
  }, []);

  useEffect(() => {
    if (!open) return;
    const focusTrigger = () => triggerRef.current?.focus();
    panelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close(true);
    };
    const onDown = (e: MouseEvent) => {
      if (hostRef.current && !hostRef.current.contains(e.target as Node)) {
        setOpen(false);
        // The native mousedown moves focus to <body> after this handler, so refocus a frame later.
        refocusRaf.current = requestAnimationFrame(focusTrigger);
      }
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onDown);
    };
  }, [open, close]);

  useEffect(
    () => () => {
      if (refocusRaf.current) cancelAnimationFrame(refocusRaf.current);
    },
    [],
  );

  const { active, total, signedIn, idle, week, usageAction } = model;
  if (active.state === "loading") return null;

  const text = chipText(active, total);
  const people: ActivePerson[] = active.state === "ok" ? active.people : [];
  const stack = people.slice(0, 3);
  const extra = people.length - stack.length;
  const idleLine = idle ? idleText(idle) : null;

  return (
    <div ref={hostRef} className="relative inline-block">
      <button
        ref={triggerRef}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? panelId : undefined}
        aria-label={`People: ${text.main}${text.of ? ` ${text.of}` : ""}`}
        onClick={() => setOpen((v) => !v)}
        className="inline-flex max-w-full items-center gap-2 rounded-full border border-edge bg-surface px-2.5 py-1 text-xs text-fg hover:bg-raised"
      >
        {stack.length > 0 && (
          <span aria-hidden="true" className="flex items-center -space-x-1.5">
            {stack.map((p) => (
              <Avatar key={p.key} initials={p.initials} small />
            ))}
            {extra > 0 && (
              <span className="inline-flex h-5 min-w-5 items-center justify-center rounded-full border border-edge bg-raised px-1 text-[9px] font-semibold text-muted">
                +{extra}
              </span>
            )}
          </span>
        )}
        <span className="min-w-0 truncate">
          {active.state === "unavailable" ? (
            <span className="text-muted">{text.main}</span>
          ) : (
            <>
              <b className="font-semibold">{text.main}</b>
              {text.of && <span className="text-muted"> {text.of}</span>}
            </>
          )}
        </span>
      </button>
      {open && (
        <div
          id={panelId}
          ref={panelRef}
          role="dialog"
          aria-label="People using uzi"
          tabIndex={-1}
          // Desktop: below the chip, right edge aligned to it. Below sm the header actions can wrap
          // so the chip may sit anywhere horizontally; the panel then switches to fixed with a 1rem
          // inset on both sides (top stays at its in-flow position under the chip), so it can never
          // leave the viewport. No measuring needed.
          className="absolute right-0 top-full z-30 mt-2 max-h-[70vh] w-[min(360px,calc(100vw-32px))] overflow-y-auto rounded-xl border border-edge bg-raised text-left shadow-2xl outline-hidden max-sm:fixed max-sm:inset-x-4 max-sm:top-auto max-sm:w-auto"
        >
          <div className="space-y-3 p-3">
            <div className="flex items-start justify-between gap-2">
              <h3 className="text-sm font-semibold text-fg">{headerText(active, total)}</h3>
              <span className="shrink-0 cursor-help text-xs text-faint" title={INFO_TEXT} aria-hidden="true">
                ⓘ
              </span>
              <span className="sr-only">{INFO_TEXT}</span>
            </div>

            <div>
              <GroupLabel>Active now</GroupLabel>
              {active.state === "unavailable" ? (
                <p className="text-sm text-muted">The admin run list could not be read.</p>
              ) : people.length === 0 ? (
                <p className="text-sm text-muted">Nobody has listed active work.</p>
              ) : (
                <ul className="space-y-1.5">
                  {people.map((p) => (
                    <li key={p.key} className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2 text-sm">
                      <Avatar initials={p.initials} />
                      <span className="truncate text-fg" title={p.name}>
                        {p.name}
                        <Marks you={p.you} deactivated={p.deactivated} />
                      </span>
                      <span className={cx("rounded-full px-2 py-0.5 text-xs", TONE_CLASS[p.tone])}>{p.pill}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>

            {signedIn != null && signedIn.length > 0 && (
              <div>
                <GroupLabel>Signed in within 24 hours, no listed active run</GroupLabel>
                <ul className="space-y-1.5">
                  {signedIn.map((p) => (
                    <li key={p.key} className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2 text-sm">
                      <Avatar initials={p.initials} />
                      <span className="truncate text-fg" title={p.name}>
                        {p.name}
                        <Marks you={p.you} />
                      </span>
                      <span className="text-xs text-muted">{p.ago}</span>
                    </li>
                  ))}
                </ul>
              </div>
            )}

            {idleLine != null && (
              <div>
                <GroupLabel>Idle</GroupLabel>
                <p className="text-sm text-muted">{idleLine}</p>
              </div>
            )}
          </div>

          <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 border-t border-edge px-3 py-2 text-xs text-muted">
            <span>
              {week == null
                ? "This week: unavailable"
                : `This week: ${week} ${week === 1 ? "person had" : "people had"} runs with recorded usage`}
            </span>
            {usageAction === "available" ? (
              <button
                type="button"
                onClick={() => {
                  // Focus belongs to the By user section next, so do not return it to the chip.
                  close(false);
                  onShowUsageByUser();
                }}
                className="font-medium text-brand hover:text-brand-hover"
              >
                Usage by user →
              </button>
            ) : (
              <button type="button" disabled aria-disabled="true" className="cursor-not-allowed text-faint">
                Usage by user unavailable
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
