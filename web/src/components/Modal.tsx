// Modal — the app's accessible dialog shell, extracted from ScheduleModal's focus
// mechanics (PRD #66 M9 follow-up) so every overlay shares ONE correct
// implementation instead of re-deriving it. It provides the backdrop and the four
// a11y behaviours a role="dialog"/aria-modal="true" surface owes:
//
//   - on open, focus moves into the dialog container (tabIndex={-1} makes it
//     programmatically focusable) so Tab starts inside and a screen reader lands
//     on the dialog;
//   - Tab is trapped, wrapping at the first/last focusable element so focus never
//     leaks to the page behind the backdrop;
//   - Escape closes (same path as an explicit Cancel/×);
//   - on close, focus is restored to whatever was focused when the dialog opened.
//
// The caller supplies the panel content (a card, a form, …) and an aria-label; the
// backdrop + keyboard/focus wiring live here. A backdrop click closes when
// closeOnBackdrop is true (callers pass false while a submit is in flight).

import { useEffect, useRef } from "react";

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

export function Modal({
  label,
  onClose,
  closeOnBackdrop = true,
  className,
  children,
}: {
  label: string;
  onClose: () => void;
  // Whether a click on the backdrop closes the dialog. Callers set it false while a
  // write is in flight so a stray click cannot dismiss a submitting form.
  closeOnBackdrop?: boolean;
  // Extra classes for the backdrop container (rarely needed; the default centres a card).
  className?: string;
  children: React.ReactNode;
}) {
  // On open we move focus into the dialog container and, on close, restore it to
  // whatever was focused when the dialog opened — captured here without threading a
  // ref from the caller (mirrors the pattern ScheduleModal used before extraction).
  const dialogRef = useRef<HTMLDivElement>(null);
  const restoreFocusRef = useRef<Element | null>(null);
  useEffect(() => {
    restoreFocusRef.current = document.activeElement;
    dialogRef.current?.focus();
    return () => {
      const prev = restoreFocusRef.current;
      if (prev instanceof HTMLElement) prev.focus();
    };
  }, []);

  // Escape closes; Tab is trapped so focus cycles within the dialog instead of
  // reaching the page behind it. The trap is a boundary wrap at the first/last
  // focusable element — no new dependency.
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.stopPropagation();
      onClose();
      return;
    }
    if (e.key !== "Tab") return;
    const root = dialogRef.current;
    if (!root) return;
    const focusable = Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
      (el) => el.offsetParent !== null || el === document.activeElement,
    );
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    const active = document.activeElement;
    if (e.shiftKey) {
      if (active === first || active === root) {
        e.preventDefault();
        last.focus();
      }
    } else if (active === last) {
      e.preventDefault();
      first.focus();
    }
  };

  return (
    <div
      ref={dialogRef}
      tabIndex={-1}
      className={
        className ??
        "fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/60 p-4 outline-hidden sm:items-center"
      }
      role="dialog"
      aria-modal="true"
      aria-label={label}
      onKeyDown={onKeyDown}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget && closeOnBackdrop) onClose();
      }}
    >
      {children}
    </div>
  );
}
