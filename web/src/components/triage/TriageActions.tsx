import { useEffect, useRef, useState } from "react";
import { Button } from "../ui";
import { dismissMenuItems } from "./triageCopy";

// TriageActions is the one action row every triage surface shows: File issue · Mark done ·
// Dismiss ▾, all at equal weight (PRD #1183). It replaces the run page's lone orange File
// issue button on its own line, the Judge occurrence's per-run File issue, and the Findings
// row's one-click File / Dismiss pair.
//
// It is PRESENTATIONAL: an absent handler hides its button, so a surface renders only the
// actions it offers (the Findings row passes no onMarkDone — a finding's done comes only from
// its issue closing). The mutation itself lives in the CALLER's handler, which decides between
// the two modes the PRD describes:
//   * owned (run page, finding card): the handler does the optimistic call, the single-row
//     refetch, and owns the successor-focus move onto Undo. Because its row swaps THIS
//     component out for TriageDisposedRow on a mutation, an announce hosted here would unmount
//     with the message mid-read — so the owned caller hosts its OWN persistent live region a
//     level up and passes `callerHostsLiveRegion`, and we render no second one here.
//   * delegated (Judge group row, both multi-select bars): the handler forwards to the page's
//     bulk call and toast Undo, with no successor focus (a settled group may leave the bucket
//     on reload, so there is no successor to focus). The row is not swapped under it, so the
//     region below is enough — these callers omit `callerHostsLiveRegion` and keep it.
//
// The Dismiss ▾ menu (aria-haspopup="menu", Escape + outside-click close) is lifted from the
// judge's GroupDisposeControls / DispositionControls, with the two reasoned items sourced from
// triageCopy by `dismissCopy`.
export function TriageActions({
  onFile,
  onMarkDone,
  onDismiss,
  dismissCopy = "judge",
  busy = false,
  announce = "",
  callerHostsLiveRegion = false,
}: {
  onFile?: () => void;
  onMarkDone?: () => void;
  onDismiss?: (reason: "wont_do" | "not_an_issue") => void;
  dismissCopy?: "judge" | "finding";
  busy?: boolean;
  announce?: string;
  // Set by an owned-mode caller that hosts its OWN persistent live region because its row
  // swaps this component out on a mutation (see the mode note above). When true we render no
  // internal region — the caller's survives the swap. Default false keeps the delegated
  // callers' behaviour (and this component's own region) unchanged.
  callerHostsLiveRegion?: boolean;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuWrapRef = useRef<HTMLDivElement>(null);

  // Escape closes the menu and returns focus to the Dismiss trigger (the first button in the
  // wrapper); a pointerdown outside closes it. Wired only while open, torn down on close.
  useEffect(() => {
    if (!menuOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setMenuOpen(false);
        menuWrapRef.current?.querySelector<HTMLElement>("button")?.focus();
      }
    };
    const onPointerDown = (e: Event) => {
      if (menuWrapRef.current && !menuWrapRef.current.contains(e.target as Node)) {
        setMenuOpen(false);
      }
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("pointerdown", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("pointerdown", onPointerDown);
    };
  }, [menuOpen]);

  return (
    <div className="flex flex-wrap items-center gap-2">
      {/* A polite sr-only live region for the delegated mode's announcement. The owned mode
          (run page, finding card) instead hosts its OWN persistent region a level up and passes
          callerHostsLiveRegion, because its row swaps this component out for TriageDisposedRow
          on a mutation and a region here would unmount with the message mid-read. */}
      {!callerHostsLiveRegion && (
        <span className="sr-only" role="status" aria-live="polite">
          {announce}
        </span>
      )}
      {onFile && (
        <Button size="sm" variant="secondary" disabled={busy} onClick={onFile}>
          File issue
        </Button>
      )}
      {onMarkDone && (
        <Button size="sm" variant="secondary" disabled={busy} onClick={onMarkDone}>
          Mark done
        </Button>
      )}
      {onDismiss && (
        <div className="relative" ref={menuWrapRef}>
          <Button
            size="sm"
            variant="secondary"
            disabled={busy}
            aria-haspopup="menu"
            aria-expanded={menuOpen}
            onClick={() => setMenuOpen((o) => !o)}
          >
            Dismiss ▾
          </Button>
          {menuOpen && (
            <div
              role="menu"
              className="absolute right-0 z-10 mt-1 w-56 rounded-lg border border-edge-strong bg-surface p-1 shadow-lg"
            >
              {dismissMenuItems(dismissCopy).map((item) => (
                <button
                  key={item.reason}
                  type="button"
                  role="menuitem"
                  disabled={busy}
                  onClick={() => {
                    setMenuOpen(false);
                    onDismiss(item.reason);
                  }}
                  className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised disabled:opacity-50"
                >
                  {item.label}
                  <span className="text-xs text-faint">{item.subline}</span>
                </button>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
