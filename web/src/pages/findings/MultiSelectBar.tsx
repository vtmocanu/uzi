import { useEffect, useRef, useState } from "react";
import { Button } from "../../components/ui";
import { dismissMenuItems } from "../../components/triage/triageCopy";

// MultiSelectBar is the Findings page's sticky action bar for the checkbox selection (PRD #1183
// M4). It is the judge MultiSelectBar with the "Mark done" button REMOVED: a finding has no human
// "done" (done comes only from its filed issue closing), so bulk triage offers Dismiss ▾ + Clear
// only. It matches the shared TriageActions Dismiss ▾ behaviour — Escape + outside-click close,
// the menu button carrying aria-haspopup="menu" — and sources its two reasoned menu items from
// triageCopy("finding") so the worker-voiced sublines cannot drift from the row's. The menu opens
// UPWARD (`bottom-full`), like the judge bar, because this bar is pinned to the bottom of the
// viewport.
export function MultiSelectBar({
  count,
  onClear,
  onDismiss,
}: {
  count: number;
  onClear: () => void;
  onDismiss: (reason: "wont_do" | "not_an_issue") => void;
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

  // Inset past the sidebar at the desktop breakpoint, exactly like the judge bar (issue #204):
  // `lg:left-60` matches AppShell's `lg:pl-60` content inset so the "N selected" label is never
  // clipped under the sidebar; z-20 stays below the sidebar's z-30. Literal class strings for the
  // Tailwind JIT.
  return (
    <div className="fixed left-0 right-0 bottom-0 z-20 border-t border-edge bg-surface/95 px-4 py-3 backdrop-blur-sm lg:left-60">
      <div className="mx-auto flex w-full max-w-[68rem] flex-wrap items-center gap-3">
        <span className="text-sm font-medium text-fg">
          {count} {count === 1 ? "finding" : "findings"} selected
        </span>
        <span className="text-xs text-faint">Dismiss applies to open findings only.</span>
        <div className="ml-auto flex items-center gap-2">
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
                {dismissMenuItems("finding").map((item) => (
                  <button
                    key={item.reason}
                    type="button"
                    role="menuitem"
                    onClick={() => {
                      setMenuOpen(false);
                      onDismiss(item.reason);
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
          <Button size="sm" variant="ghost" onClick={onClear}>
            Clear
          </Button>
        </div>
      </div>
    </div>
  );
}
