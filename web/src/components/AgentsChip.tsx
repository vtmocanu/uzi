// The Agents chip for the new Overview header (/dashboard) (#4335), shown to every user. A compact
// "K agents in your runs of N" trigger opens a popover with the per-scope breakdown and, for an
// admin, the global-default count. All figures come from the pure agentCountModel; this
// component only draws it. The open/close model, focus handling and absolute panel follow
// PeopleChip.

import { useCallback, useEffect, useId, useRef, useState } from "react";
import { Link } from "react-router-dom";
import type { AgentCountModel } from "../lib/agentCounts";
import { BotIcon } from "./icons";

const INFO_TEXT =
  "Agents toggled on under In my runs on the Agents page. A run can still pick repo agents or exclude some at its plan gate.";

function GroupLabel({ children }: { children: string }) {
  return <p className="mb-1 text-[11px] font-medium uppercase tracking-wide text-faint">{children}</p>;
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <li className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-3 text-sm">
      <span className="text-muted">{label}</span>
      <span className="text-fg">{value}</span>
    </li>
  );
}

export function AgentsChip({ model }: { model: AgentCountModel }) {
  const [open, setOpen] = useState(false);
  const hostRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  // Pending click-away refocus frame, cancelled only on unmount (see PeopleChip).
  const refocusRaf = useRef(0);
  const panelId = useId();

  // keydown has no focus default action, so returning focus synchronously on Escape wins.
  const close = useCallback((returnFocus: boolean) => {
    setOpen(false);
    if (returnFocus) triggerRef.current?.focus();
  }, []);

  useEffect(() => {
    if (!open) return;
    panelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close(true);
    };
    const onDown = (e: MouseEvent) => {
      if (hostRef.current && !hostRef.current.contains(e.target as Node)) {
        setOpen(false);
        // The native mousedown moves focus to <body> after this handler, so refocus a frame later,
        // but only when focus was left on nothing: a control the user clicked keeps it.
        refocusRaf.current = requestAnimationFrame(() => {
          const a = document.activeElement;
          if (a == null || a === document.body) triggerRef.current?.focus({ preventScroll: true });
        });
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

  const { total, inRuns, byScope, globalDefaults } = model;

  return (
    <div ref={hostRef} className="relative inline-block min-w-0 max-w-full">
      <button
        ref={triggerRef}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? panelId : undefined}
        aria-label={`Agents: ${inRuns} in your runs of ${total}`}
        onClick={() => setOpen((v) => !v)}
        className="inline-flex max-w-full items-center gap-2 rounded-full border border-edge bg-surface px-2.5 py-1 text-xs text-fg hover:bg-raised"
      >
        <span
          aria-hidden="true"
          className="inline-flex h-5 w-5 shrink-0 items-center justify-center rounded-[5px] border border-edge bg-raised text-muted ring-1 ring-surface"
        >
          <BotIcon className="h-3.5 w-3.5" />
        </span>
        <span className="min-w-0 truncate">
          <b className="font-semibold">
            {inRuns} {inRuns === 1 ? "agent" : "agents"} in your runs
          </b>
          <span className="text-muted"> of {total}</span>
        </span>
      </button>
      {open && (
        <div
          id={panelId}
          ref={panelRef}
          role="dialog"
          aria-label="Agents in your runs"
          tabIndex={-1}
          // Desktop: below the chip, right edge aligned to it. Below sm the panel becomes a bottom
          // sheet: fixed with a 1rem inset on the sides and bottom (see PeopleChip).
          className="absolute right-0 top-full z-30 mt-2 max-h-[70vh] w-[min(360px,calc(100vw-32px))] overflow-y-auto rounded-xl border border-edge bg-raised text-left shadow-2xl outline-hidden max-sm:fixed max-sm:inset-x-4 max-sm:bottom-4 max-sm:top-auto max-sm:mt-0 max-sm:max-h-[calc(100dvh-2rem)] max-sm:w-auto"
        >
          <div className="space-y-3 p-3">
            <div className="flex items-start justify-between gap-2">
              <h3 className="text-sm font-semibold text-fg">
                {inRuns} of {total} agents ride your runs
              </h3>
              {/* Drawn, not a glyph: the bundled fonts lack U+24D8. */}
              <span
                className="inline-flex h-4 w-4 shrink-0 cursor-help items-center justify-center rounded-full border border-edge text-[10px] font-semibold leading-none text-faint"
                title={INFO_TEXT}
                aria-hidden="true"
              >
                i
              </span>
              <span className="sr-only">{INFO_TEXT}</span>
            </div>

            <div>
              <GroupLabel>By scope</GroupLabel>
              <ul className="space-y-1.5">
                {byScope.map((s) => (
                  <Row key={s.scope} label={s.label} value={`${s.total} · ${s.inRuns} on`} />
                ))}
              </ul>
            </div>

            {globalDefaults !== undefined && (
              <div>
                <GroupLabel>Admin only</GroupLabel>
                <ul>
                  <Row label="Global defaults for new users" value={String(globalDefaults)} />
                </ul>
              </div>
            )}
          </div>

          <div className="sticky bottom-0 flex items-center justify-end gap-3 border-t border-edge bg-raised px-3 py-2 text-xs">
            <Link to="/agents" onClick={() => close(false)} className="font-medium text-brand hover:text-brand-hover">
              Manage agents
            </Link>
          </div>
        </div>
      )}
    </div>
  );
}
