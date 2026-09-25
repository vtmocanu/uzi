// MoreActionsMenu — an APG menu button (PRD #1645 D13) for a row's less frequent actions.
//
// Keyboard: Enter, Space or ArrowDown on the button opens the menu with focus on the first
// item (ArrowUp opens on the last); inside, ArrowDown/ArrowUp move between items and wrap,
// Home/End jump to the ends, Enter/Space activate, Escape closes and returns focus to the
// button, and Tab closes. A pointerdown outside closes it. Escape and focus return follow
// TriageActions' Dismiss menu; the arrow-key navigation is new here.
//
// Items are role="menuitem" divs rather than <button>s so a Space keyup that lands on the
// first item right after opening cannot activate it natively. A disabled item stays
// focusable (APG), is marked aria-disabled, and carries its reason as a description.
//
// The menu is portalled into document.body and position:fixed, anchored to the button: a
// table wrapper's overflow cannot clip it, and an ancestor's stacking context (a paused
// row's opacity) cannot paint later rows over it. It is re-anchored on scroll and resize,
// and flips above the button when it would run off the bottom of the viewport; its right
// offset is clamped so its left edge stays on screen as well. It renders hidden until its
// first placement, and focus moves into it only once it is placed: a browser refuses focus
// on a visibility:hidden element.

import { useEffect, useId, useLayoutEffect, useRef, useState, type RefObject } from "react";
import { createPortal } from "react-dom";
import { cx } from "./ui";

export interface MoreActionsItem {
  key: string;
  label: string;
  // Shown under the label; the reason when the item is disabled.
  description?: string;
  disabled?: boolean;
  // Visual emphasis for a destructive action (Remove).
  danger?: boolean;
  onSelect: () => void;
}

export function MoreActionsMenu({
  label,
  items,
  buttonRef: externalButtonRef,
}: {
  // The button's accessible name, naming the job and repo (D13).
  label: string;
  items: MoreActionsItem[];
  // Lets the owner return focus to the button after a flow the menu started (an inline
  // panel's Cancel, a failed add).
  buttonRef?: RefObject<HTMLButtonElement | null>;
}) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const [pos, setPos] = useState<{ top: number; right: number } | null>(null);
  const internalButtonRef = useRef<HTMLButtonElement>(null);
  const buttonRef = externalButtonRef ?? internalButtonRef;
  const menuRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const baseId = useId();
  const menuId = `${baseId}-menu`;
  const buttonId = `${baseId}-button`;

  const openAt = (index: number) => {
    setActive(index);
    setOpen(true);
  };
  const close = (returnFocus: boolean) => {
    setOpen(false);
    if (returnFocus) buttonRef.current?.focus();
  };

  // Anchor below the button (or above it when there is no room), recomputed while open.
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const btn = buttonRef.current;
      if (!btn) return;
      const r = btn.getBoundingClientRect();
      const h = menuRef.current?.offsetHeight ?? 0;
      const below = r.bottom + 4;
      const top = below + h > window.innerHeight && r.top - h - 4 > 0 ? r.top - h - 4 : below;
      // clientWidth excludes a vertical scrollbar, matching what `right` is measured from.
      // Right-align to the button, but keep the whole menu within [4, cw - 4]: a button near
      // the left edge of a narrow viewport would otherwise push the menu's left edge off screen.
      const cw = document.documentElement.clientWidth;
      const w = menuRef.current?.offsetWidth || 256;
      const right = Math.min(Math.max(4, cw - r.right), Math.max(4, cw - w - 4));
      setPos({ top, right });
    };
    place();
    window.addEventListener("scroll", place, true);
    window.addEventListener("resize", place);
    return () => {
      window.removeEventListener("scroll", place, true);
      window.removeEventListener("resize", place);
      // The next open starts hidden again, so it is never shown at a stale anchor.
      setPos(null);
    };
  }, [open, buttonRef]);

  // Move DOM focus with the active index (roving focus inside the menu), but only once the
  // menu is placed and visible; keyed on `placed` rather than `pos` so a scroll re-anchor
  // does not pull focus back to the active item.
  const placed = pos !== null;
  useEffect(() => {
    if (!open || !placed) return;
    itemRefs.current[active]?.focus({ preventScroll: true });
  }, [open, active, placed]);

  // A pointerdown outside the button and the menu closes it (focus stays where the user put it).
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: Event) => {
      const t = e.target as Node;
      if (menuRef.current?.contains(t) || buttonRef.current?.contains(t)) return;
      setOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open, buttonRef]);

  const activate = (item: MoreActionsItem) => {
    if (item.disabled) return;
    close(true);
    item.onSelect();
  };

  const onButtonKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" || e.key === " " || e.key === "ArrowDown") {
      e.preventDefault();
      openAt(0);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      openAt(items.length - 1);
    }
  };

  const onMenuKeyDown = (e: React.KeyboardEvent) => {
    const n = items.length;
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        setActive((i) => (i + 1) % n);
        break;
      case "ArrowUp":
        e.preventDefault();
        setActive((i) => (i - 1 + n) % n);
        break;
      case "Home":
        e.preventDefault();
        setActive(0);
        break;
      case "End":
        e.preventDefault();
        setActive(n - 1);
        break;
      case "Escape":
        e.preventDefault();
        e.stopPropagation();
        close(true);
        break;
      case "Tab":
        // The portalled menu sits at the end of <body>, so letting Tab run from inside it
        // would leave the page. Put focus back on the button first and let the default
        // action carry on from there: Tab lands after the button, Shift+Tab before it.
        close(true);
        break;
      case "Enter":
      case " ":
        e.preventDefault();
        if (items[active]) activate(items[active]);
        break;
    }
  };

  return (
    <>
      <button
        ref={buttonRef}
        id={buttonId}
        type="button"
        aria-label={label}
        title="More actions"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={() => (open ? close(false) : openAt(0))}
        onKeyDown={onButtonKeyDown}
        className="inline-flex h-7 shrink-0 items-center justify-center rounded-lg px-2.5 text-xs font-medium text-muted transition-colors hover:bg-raised hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
      >
        <DotsIcon />
      </button>
      {open &&
        createPortal(
          <div
            ref={menuRef}
            id={menuId}
            role="menu"
            aria-labelledby={buttonId}
            onKeyDown={onMenuKeyDown}
            style={{ position: "fixed", top: pos?.top ?? 0, right: pos?.right ?? 0, visibility: pos ? "visible" : "hidden" }}
            className="z-50 w-64 max-w-[calc(100vw-8px)] rounded-lg border border-edge-strong bg-surface p-1 text-left shadow-lg"
          >
            {items.map((item, i) => {
              // Name from the label span alone, so the description stays a description.
              const labelId = `${baseId}-label-${i}`;
              const descId = item.description ? `${baseId}-desc-${i}` : undefined;
              return (
                <div
                  key={item.key}
                  ref={(el) => {
                    itemRefs.current[i] = el;
                  }}
                  role="menuitem"
                  tabIndex={-1}
                  aria-disabled={item.disabled || undefined}
                  aria-labelledby={labelId}
                  aria-describedby={descId}
                  onClick={() => activate(item)}
                  onMouseEnter={() => setActive(i)}
                  className={cx(
                    "cursor-pointer rounded-md px-2.5 py-1.5 text-[13px] outline-hidden focus:bg-raised",
                    item.disabled ? "cursor-not-allowed text-faint" : item.danger ? "text-danger" : "text-fg",
                  )}
                >
                  <span id={labelId}>{item.label}</span>
                  {item.description && (
                    <div id={descId} className="mt-0.5 text-[11.5px] leading-snug text-faint">
                      {item.description}
                    </div>
                  )}
                </div>
              );
            })}
          </div>,
          document.body,
        )}
    </>
  );
}

// A horizontal three-dot glyph, local to the menu button (the shared icon set has none).
function DotsIcon() {
  return (
    <svg viewBox="0 0 24 24" width="1em" height="1em" fill="currentColor" aria-hidden="true">
      <circle cx="5" cy="12" r="1.8" />
      <circle cx="12" cy="12" r="1.8" />
      <circle cx="19" cy="12" r="1.8" />
    </svg>
  );
}
