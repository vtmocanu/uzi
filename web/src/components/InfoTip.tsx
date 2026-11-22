// A small explanation that opens on hover, keyboard focus or click/tap. The mechanics follow
// RepoSetupChip.tsx (its comments are the authority): hover and focus are tracked on the HOST so
// the pointer can move onto the panel, click only opens, Escape closes however it was opened (a
// document listener while open, so a hover-opened tip closes without focus), and the panel stays
// mounted when closed so its text remains the trigger's accessible description
// (aria-describedby + role="tooltip"). No native `title` anywhere. Unlike RepoSetupChip, hover,
// focus and click are tracked separately, so losing one never closes a tip another still holds.
//
// Two triggers: an (i) icon button named by `label`, or, with `children`, the children themselves
// underlined with dots (a figure whose detail moved into the tip).
//
// The panel is position: fixed at the trigger's viewport coordinates, clamped to the viewport and
// flipped above when there is no room below. A tip inside a table's horizontal scroller (which
// clips both axes) or near a phone's edge is therefore never cut off.

import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { cx } from "./ui";

const GAP = 6; // between trigger and panel
const MARGIN = 8; // between panel and viewport edge
const CLOSE_DELAY_MS = 120; // time to cross the gap onto the panel

export function InfoTip({ tip, label, children }: { tip: ReactNode; label?: string; children?: ReactNode }) {
  // Each input keeps the tip open on its own (WCAG 1.4.13 persistence): it closes only when none
  // remains, or on Escape, which dismisses until the next hover, focus or click.
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const [clicked, setClicked] = useState(false);
  const [dismissed, setDismissed] = useState(false);
  const open = !dismissed && (hovered || focused || clicked);
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  const tipId = useId();
  const hostRef = useRef<HTMLSpanElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const tipRef = useRef<HTMLSpanElement>(null);
  const closeTimer = useRef<number | undefined>(undefined);

  const enter = () => {
    window.clearTimeout(closeTimer.current);
    setHovered(true);
    setDismissed(false);
  };
  const leaveSoon = () => {
    window.clearTimeout(closeTimer.current);
    closeTimer.current = window.setTimeout(() => setHovered(false), CLOSE_DELAY_MS);
  };
  useEffect(() => () => window.clearTimeout(closeTimer.current), []);

  // While open: Escape dismisses however it was opened (a hover-opened tip has no focus to key
  // on), and a press outside ends a click/tap opening (a tapped button is not always focused).
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setDismissed(true);
    };
    const onPointerDown = (e: PointerEvent) => {
      if (!hostRef.current?.contains(e.target as Node | null)) setClicked(false);
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("pointerdown", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("pointerdown", onPointerDown);
    };
  }, [open]);

  // Measure after render, before paint, and follow the trigger while open (any scroller, resize).
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const t = triggerRef.current?.getBoundingClientRect();
      const p = tipRef.current?.getBoundingClientRect();
      if (!t || !p) return;
      const left = Math.min(Math.max(t.left + t.width / 2 - p.width / 2, MARGIN), window.innerWidth - p.width - MARGIN);
      const below = t.bottom + GAP;
      const top = below + p.height <= window.innerHeight - MARGIN ? below : Math.max(t.top - GAP - p.height, MARGIN);
      setPos({ top, left: Math.max(left, MARGIN) });
    };
    place();
    window.addEventListener("scroll", place, true);
    window.addEventListener("resize", place);
    return () => {
      window.removeEventListener("scroll", place, true);
      window.removeEventListener("resize", place);
    };
  }, [open]);

  const shown = open && pos != null;
  return (
    <span
      ref={hostRef}
      className="inline-flex"
      onMouseEnter={enter}
      onMouseLeave={leaveSoon}
      onFocus={() => {
        setFocused(true);
        setDismissed(false);
      }}
      onBlur={(e) => {
        if (e.currentTarget.contains(e.relatedTarget as Node | null)) return;
        setFocused(false);
        setClicked(false);
      }}
    >
      <button
        ref={triggerRef}
        type="button"
        aria-label={children == null ? label : undefined}
        aria-describedby={tipId}
        onClick={() => {
          setClicked(true);
          setDismissed(false);
        }}
        className={
          children == null
            ? "inline-flex h-4 w-4 cursor-help items-center justify-center rounded-full border border-current font-serif text-[10px] font-semibold italic leading-none normal-case tracking-normal text-faint hover:text-fg focus-visible:text-fg"
            : "cursor-help rounded-sm underline decoration-faint decoration-dotted underline-offset-[3px] hover:decoration-fg focus-visible:decoration-fg"
        }
      >
        {children ?? <span aria-hidden="true">i</span>}
      </button>
      <span
        ref={tipRef}
        id={tipId}
        role="tooltip"
        data-open={open ? "true" : "false"}
        style={pos ? { top: pos.top, left: pos.left } : { top: 0, left: 0 }}
        className={cx(
          // The trigger may sit beside an uppercase, tracked heading or in a bold total row: reset
          // case, tracking and weight so the explanation always reads as plain sentence text.
          "fixed z-50 w-max max-w-[min(22rem,calc(100vw-1rem))] whitespace-normal rounded-lg border border-edge bg-raised px-3 py-2 text-left text-xs font-normal normal-case leading-relaxed tracking-normal text-fg shadow-2xl",
          "transition-opacity duration-150 motion-reduce:transition-none",
          shown ? "opacity-100" : "pointer-events-none opacity-0",
        )}
      >
        {tip}
      </span>
    </span>
  );
}
