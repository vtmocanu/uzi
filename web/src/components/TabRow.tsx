// Shared tab row primitives (#4419): TabRow (the row container), tabClass (the tab
// look), keepInRowView (sideways keep-in-view that never scrolls the window) and
// PinnedBlock (the sticky wrapper that keeps a row visible below the app bar).
//
// The row draws its bottom line as an INSET box-shadow and tabs carry no `-mb-px`, so the
// row's box never overflows by a pixel (the old negative margin made `overflow-x-auto`
// rows grow a stray vertical scrollbar). The active tab's 2px bottom border paints over
// the 1px inset line because a child's border is drawn above its parent's inset shadow.

import { forwardRef, useEffect, useRef, type HTMLAttributes, type ReactNode, type RefObject } from "react";
import { cx } from "./ui";

type TabRowVariant = "scroll" | "wrap";

interface TabRowProps extends HTMLAttributes<HTMLDivElement> {
  /** `scroll` (default) scrolls sideways inside the row; `wrap` wraps tabs onto more lines. */
  variant?: TabRowVariant;
  children?: ReactNode;
}

export const TabRow = forwardRef<HTMLDivElement, TabRowProps>(function TabRow(
  { variant = "scroll", className, children, ...rest },
  ref,
) {
  return (
    <div
      ref={ref}
      {...rest}
      className={cx(
        "flex gap-1 shadow-[inset_0_-1px_0_rgb(var(--edge))]",
        variant === "wrap" ? "flex-wrap" : "overflow-x-auto overflow-y-hidden",
        className,
      )}
    >
      {children}
    </div>
  );
});

export type TabTone = "default" | "faint";

export function tabClass(active: boolean, tone: TabTone = "default"): string {
  return cx(
    "shrink-0 whitespace-nowrap border-b-2 px-3 py-2 text-sm font-medium transition-colors focus-visible:ring-inset focus-visible:ring-offset-0",
    active
      ? "border-brand text-fg"
      : tone === "faint"
        ? "border-transparent text-faint hover:border-edge-strong hover:text-muted"
        : "border-transparent text-muted hover:border-edge-strong hover:text-fg",
  );
}

/**
 * Adjust `row.scrollLeft` so `el` is fully visible inside `row`. Only the row scrolls:
 * unlike `scrollIntoView` this never moves the window (which, under a pinned block's
 * scroll-padding, would shift the page). `inset` (px) keeps `el` that far inside each edge,
 * so a faded row (`data-fade-row`, 24px fades) leaves the element clear of the fade. The
 * scroll is instant: it assigns scrollLeft, and no CSS sets `scroll-behavior`.
 */
export function keepInRowView(
  row: HTMLElement | null | undefined,
  el: HTMLElement | null | undefined,
  inset = 0,
): void {
  if (!row || !el) return;
  const r = row.getBoundingClientRect();
  const e = el.getBoundingClientRect();
  if (e.left < r.left + inset) row.scrollLeft += e.left - r.left - inset;
  else if (e.right > r.right - inset) row.scrollLeft += e.right - r.right + inset;
}

/**
 * Edge fade for a sideways-scrolling row (IntersectionObserver rooted at the row, no scroll
 * listener): `data-fade-start` / `data-fade-end` are set on the row only while its first /
 * last child is not fully visible. Re-runs when `key` changes (the children changed).
 */
export function useEdgeFade(rowRef: RefObject<HTMLElement | null>, key: string): void {
  useEffect(() => {
    const row = rowRef.current;
    if (!row || typeof IntersectionObserver === "undefined") return;
    const first = row.firstElementChild;
    const last = row.lastElementChild;
    const io = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          const attr = e.target === first ? "data-fade-start" : "data-fade-end";
          if (e.intersectionRatio < 0.99) row.setAttribute(attr, "");
          else row.removeAttribute(attr);
        }
      },
      { root: row, threshold: 0.99 },
    );
    if (first) io.observe(first);
    if (last && last !== first) io.observe(last);
    return () => {
      io.disconnect();
      row.removeAttribute("data-fade-start");
      row.removeAttribute("data-fade-end");
    };
  }, [rowRef, key]);
}

// Owner token for the <html> attributes: the latest mounted block owns them, and an
// unmount clears them only when it is still the owner (StrictMode double effects, or one
// page's block unmounting after the next page's block mounted).
let pinnedOwner: symbol | null = null;

export function PinnedBlock({ className, children }: { className?: string; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const sentinelRef = useRef<HTMLDivElement>(null);

  // Stuck detection observes a zero-size sentinel at the block's natural (static) top, not the
  // block: a threshold on the block's own ratio misses an instant jump from a partially
  // visible state. The sentinel leaves the root margin (shrunk by the sticky `top`) when the
  // block's natural position scrolls above the pin line.
  useEffect(() => {
    const el = ref.current;
    const sentinel = sentinelRef.current;
    if (!el || !sentinel || typeof IntersectionObserver === "undefined" || !window.matchMedia) return;
    const wide = window.matchMedia("(min-width: 1024px)");
    let io: IntersectionObserver | null = null;
    const build = () => {
      io?.disconnect();
      const top = parseFloat(getComputedStyle(el).top) || 0;
      io = new IntersectionObserver(
        (entries) => {
          // Records queue in time order when the callback is delayed; the newest one is current.
          const entry = entries[entries.length - 1];
          const stuck = !entry.isIntersecting && entry.boundingClientRect.top < top;
          el.dataset.stuck = stuck ? "true" : "false";
        },
        { root: document, rootMargin: `-${top}px 0px 0px 0px` },
      );
      io.observe(sentinel);
    };
    build();
    wide.addEventListener("change", build);
    // AppShell fires this after it rewrites --app-bar-h (the sticky top just changed).
    window.addEventListener("uzi:app-bar-h", build);
    return () => {
      wide.removeEventListener("change", build);
      window.removeEventListener("uzi:app-bar-h", build);
      io?.disconnect();
    };
  }, []);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const root = document.documentElement;
    const owner = Symbol("pinned-block");
    pinnedOwner = owner;
    const write = () => {
      // Only the owner (the latest mounted block) writes; a stale block's resize is ignored.
      if (pinnedOwner !== owner) return;
      root.style.setProperty("--pinned-h", `${el.getBoundingClientRect().height}px`);
      root.setAttribute("data-pinned-block", "");
    };
    write();
    const ro = typeof ResizeObserver !== "undefined" ? new ResizeObserver(write) : null;
    ro?.observe(el);
    return () => {
      ro?.disconnect();
      if (pinnedOwner === owner) {
        pinnedOwner = null;
        root.style.removeProperty("--pinned-h");
        root.removeAttribute("data-pinned-block");
      }
    };
  }, []);

  return (
    <>
      <div ref={sentinelRef} aria-hidden="true" className="pointer-events-none absolute h-0 w-0" />
      <div
        ref={ref}
        data-pinned-block-root=""
        data-stuck="false"
        className={cx(
          "sticky top-[var(--app-bar-h)] z-[15] -mx-4 bg-ink px-4 sm:-mx-6 sm:px-6 lg:top-0",
          "data-[stuck=true]:shadow-[0_1px_0_rgb(var(--edge))]",
          className,
        )}
      >
        {children}
      </div>
    </>
  );
}
