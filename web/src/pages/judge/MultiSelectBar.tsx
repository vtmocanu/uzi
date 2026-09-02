import { useEffect, useRef, useState } from "react";
import { Button } from "../../components/ui";

// MultiSelectBar is the sticky action bar for the checkbox selection: it fans one verdict
// out across every selected group in one bulk call (Decision 3's multi-select).
export function MultiSelectBar({
  count,
  onClear,
  onDispose,
}: {
  count: number;
  onClear: () => void;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!menuOpen) return;
    const onPointerDown = (e: Event) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setMenuOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
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
                <button
                  type="button"
                  role="menuitem"
                  onClick={() => {
                    setMenuOpen(false);
                    onDispose("dismissed", "wont_do");
                  }}
                  className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised"
                >
                  Won't do
                  <span className="text-xs text-faint">Valid, but not worth acting on</span>
                </button>
                <button
                  type="button"
                  role="menuitem"
                  onClick={() => {
                    setMenuOpen(false);
                    onDispose("dismissed", "not_an_issue");
                  }}
                  className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised"
                >
                  Not an issue
                  <span className="text-xs text-faint">False positive — the judge got it wrong</span>
                </button>
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
