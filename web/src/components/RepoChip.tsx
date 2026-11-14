// The Repos chip for the /overview/next header (issue #4334, admins only; the page decides
// whether to mount it). A compact "N repos active of M" trigger with a badge stack opens a
// popover listing which repos have listed active work, which ran in the last week and how
// many are idle. All figures come from the pure repoActivityModel; this component only draws
// it. The open/close model, focus handling and panel are a parallel copy of PeopleChip's.

import { useCallback, useEffect, useId, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { repoIdleText, type ActiveRepo, type RepoActivityModel } from "../lib/overviewRepos";
import { cx } from "./ui";

const INFO_TEXT =
  "Active counts repos with a listed non-terminal run: chat, judge and cross-check runs are not listed, and the list stops at 500 runs. Last run includes every run kind.";

const TONE_CLASS = {
  ok: "bg-ok/15 text-ok",
  warn: "bg-warn/15 text-warn",
  neutral: "border border-edge bg-surface text-muted",
} as const;

// Square where the People chip's avatar is round, so the two chips read as different kinds.
function Badge({ text, small = false }: { text: string; small?: boolean }) {
  return (
    <span
      aria-hidden="true"
      className={cx(
        "inline-flex shrink-0 items-center justify-center rounded-[5px] border border-edge bg-raised font-semibold text-fg",
        small ? "h-5 w-5 text-[9px] ring-1 ring-surface" : "h-6 w-6 text-[10px]",
      )}
    >
      {text}
    </span>
  );
}

function GroupLabel({ children }: { children: string }) {
  return <p className="mb-1 text-[11px] font-medium uppercase tracking-wide text-faint">{children}</p>;
}

type Active = RepoActivityModel["active"];

// The count wording always uses the plural "repos", including for 1.
function chipText(active: Active, total: number | null) {
  if (active.state === "unavailable") return { main: "Repos active unavailable", of: null };
  if (active.state === "loading") return { main: "", of: null };
  return {
    main: `${active.atLeast ? "at least " : ""}${active.count} repos active`,
    of: total != null ? `of ${total}` : null,
  };
}

function nameText(active: Active, total: number | null): string {
  if (active.state === "unavailable") return "Repos: active now unavailable";
  if (active.state === "loading") return "";
  const lead = active.atLeast ? "at least " : "";
  return `Repos: ${lead}${active.count} active now${total != null ? ` of ${total}` : ""}`;
}

function headerText(active: Active, total: number | null): string {
  if (active.state === "unavailable") return "Repos active unavailable";
  if (active.state === "loading") return "";
  const lead = active.atLeast ? "At least " : "";
  if (total != null) return `${lead}${active.count} of ${total} repos active now`;
  return `${lead}${active.count} repos active now`;
}

export function RepoChip({ model }: { model: RepoActivityModel }) {
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

  const { active, total, recent, idle, listFailed } = model;
  if (active.state === "loading") return null;

  const text = chipText(active, total);
  const repos: ActiveRepo[] = active.state === "ok" ? active.repos : [];
  const stack = repos.slice(0, 2);
  const extra = repos.length - stack.length;
  const idleLine = idle ? repoIdleText(idle) : null;

  return (
    <div ref={hostRef} className="relative inline-block min-w-0 max-w-full">
      <button
        ref={triggerRef}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? panelId : undefined}
        aria-label={nameText(active, total)}
        onClick={() => setOpen((v) => !v)}
        className="inline-flex max-w-full items-center gap-2 rounded-full border border-edge bg-surface px-2.5 py-1 text-xs text-fg hover:bg-raised"
      >
        {stack.length > 0 && (
          <span aria-hidden="true" className="flex items-center -space-x-0.5">
            {stack.map((r) => (
              <Badge key={r.key} text={r.badge} small />
            ))}
            {extra > 0 && (
              <span className="inline-flex h-5 min-w-5 items-center justify-center rounded-[5px] border border-edge bg-raised px-1 text-[9px] font-semibold text-muted ring-1 ring-surface">
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
          aria-label="Repos in uzi"
          tabIndex={-1}
          // Desktop: below the chip, right edge aligned to it. Below sm the panel becomes a bottom
          // sheet (fixed, 1rem insets), so it fits wherever the wrapped header puts the chip.
          className="absolute right-0 top-full z-30 mt-2 max-h-[70vh] w-[min(360px,calc(100vw-32px))] overflow-y-auto rounded-xl border border-edge bg-raised text-left shadow-2xl outline-hidden max-sm:fixed max-sm:inset-x-4 max-sm:bottom-4 max-sm:top-auto max-sm:mt-0 max-sm:max-h-[calc(100dvh-2rem)] max-sm:w-auto"
        >
          <div className="space-y-3 p-3">
            <div className="flex items-start justify-between gap-2">
              <h3 className="text-sm font-semibold text-fg">{headerText(active, total)}</h3>
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
              <GroupLabel>Active now</GroupLabel>
              {active.state === "unavailable" ? (
                <p className="text-sm text-muted">The admin run list could not be read.</p>
              ) : repos.length === 0 ? (
                <p className="text-sm text-muted">No repo has listed active work.</p>
              ) : (
                <ul className="space-y-1.5">
                  {repos.map((r) => (
                    // The path keeps at least 8rem; a long pill wraps inside the rest instead of
                    // squeezing the path to nothing.
                    <li key={r.key} className="grid grid-cols-[auto_minmax(8rem,1fr)_auto] items-center gap-2 text-sm">
                      <Badge text={r.badge} />
                      <span className="truncate font-mono text-xs text-fg" title={r.path}>
                        {r.path}
                      </span>
                      <span className={cx("rounded-xl px-2 py-0.5 text-right text-xs", TONE_CLASS[r.tone])}>{r.pill}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>

            {listFailed && <p className="text-sm text-muted">The repo list could not be read.</p>}

            {recent != null && recent.length > 0 && (
              <div>
                <GroupLabel>Run within 7 days, nothing active</GroupLabel>
                <ul className="space-y-1.5">
                  {recent.map((r) => (
                    <li key={r.key} className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2 text-sm">
                      <Badge text={r.badge} />
                      <span className="truncate font-mono text-xs text-fg" title={r.path}>
                        {r.path}
                      </span>
                      <span className="text-xs text-muted">{r.ago}</span>
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

          <div className="sticky bottom-0 flex items-center justify-end border-t border-edge bg-raised px-3 py-2 text-xs">
            <Link to="/repos" className="font-medium text-brand hover:text-brand-hover">
              Your repos
            </Link>
          </div>
        </div>
      )}
    </div>
  );
}
