import { useEffect, useRef, useState } from "react";
import { Button } from "../../components/ui";
import { dismissMenuItems } from "../../components/triage/triageCopy";

// MultiSelectBar is the sticky action bar for the checkbox selection: it fans one verdict
// out across every selected group in one bulk call (Decision 3's multi-select). It matches
// the shared TriageActions Dismiss ▾ behaviour — Escape + outside-click close, the menu
// button carrying aria-haspopup="menu" — and sources its two reasoned menu items from
// triageCopy so the sublines cannot drift from the row's (PRD #1183). The menu opens UPWARD
// (`bottom-full`), unlike TriageActions' downward menu, because this bar is pinned to the
// bottom of the viewport.
export function MultiSelectBar({
  count,
  onClear,
  onDispose,
  allowDismiss = true,
}: {
  count: number;
  onClear: () => void;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
  // allowDismiss gates the Dismiss ▾ button (PRD #1184 M4): false under the admin `all` scope,
  // where there is no cross-user Dismiss — the bar then offers Mark done only. Default true keeps
  // every owner call site unchanged.
  allowDismiss?: boolean;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  // Escape closes the menu and returns focus to the Dismiss trigger (the first button in the
  // wrapper); a pointerdown outside closes it. Wired only while open, torn down on close —
  // the same discipline as TriageActions.
  useEffect(() => {
    if (!menuOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setMenuOpen(false);
        wrapRef.current?.querySelector<HTMLElement>("button")?.focus();
      }
    };
    const onPointerDown = (e: Event) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setMenuOpen(false);
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("pointerdown", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("pointerdown", onPointerDown);
    };
  }, [menuOpen]);

  // Issue #204: inset the bar past the sidebar at the desktop breakpoint. `inset-x-0`
  // spanned full width UNDER the w-60 (240px) z-30 sidebar, clipping the "N groups
  // selected" label. `lg:left-60` matches the app layout's `lg:pl-60` content inset
  // (AppShell), so on lg the bar starts at 240px; mobile stays full width. z-20 is left
  // BELOW the sidebar's z-30 deliberately — the fix is the inset, not stacking over it.
  // These class strings are literal (not interpolated) for Tailwind JIT.
  // The inset tracks the DEFAULT (expanded) sidebar; when a user collapses it to w-14
  // the bar over-insets by ~184px (cosmetic gap, never a clip). Tracking the collapse
  // state would need a shared signal plumbed out of AppShell's local state — out of
  // scope here; the label-clip bug this fixes is gone in both states.
  return (
    <div className="fixed left-0 right-0 bottom-0 z-20 border-t border-edge bg-surface/95 px-4 py-3 backdrop-blur-sm lg:left-60">
      <div className="mx-auto flex w-full max-w-[68rem] flex-wrap items-center gap-3">
        <span className="text-sm font-medium text-fg">
          {count} {count === 1 ? "group" : "groups"} selected
        </span>
        <span className="text-xs text-faint">Actions apply to open members only.</span>
        <div className="ml-auto flex items-center gap-2">
          <Button size="sm" variant="secondary" onClick={() => onDispose("done")}>
            Mark done
          </Button>
          {allowDismiss && (
            <div className="relative" ref={wrapRef}>
              <Button
                size="sm"
                variant="secondary"
                aria-haspopup="menu"
                aria-expanded={menuOpen}
                onClick={() => setMenuOpen((o) => !o)}
              >
                Dismiss ▾
              </Button>
              {menuOpen && (
                <div
                  role="menu"
                  className="absolute bottom-full right-0 z-10 mb-1 w-56 rounded-lg border border-edge-strong bg-surface p-1 shadow-lg"
                >
                  {dismissMenuItems("judge").map((item) => (
                    <button
                      key={item.reason}
                      type="button"
                      role="menuitem"
                      onClick={() => {
                        setMenuOpen(false);
                        onDispose("dismissed", item.reason);
                      }}
                      className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised"
                    >
                      {item.label}
                      <span className="text-xs text-faint">{item.subline}</span>
                    </button>
                  ))}
                </div>
              )}
            </div>
          )}
          <Button size="sm" variant="ghost" onClick={onClear}>
            Clear
          </Button>
        </div>
      </div>
    </div>
  );
}
