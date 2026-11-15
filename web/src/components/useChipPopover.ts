import { useCallback, useEffect, useId, useRef, useState } from "react";

// The People, Repos and Agents chips share one focus-and-dismiss contract. Escape has no
// native focus action, so it returns focus synchronously. A click-away's native mousedown
// may leave focus on body; wait one frame and refocus the chip only in that case, preserving
// focus on a different control the user clicked. The pending frame is cancelled on unmount.
export function useChipPopover() {
  const [open, setOpen] = useState(false);
  const hostRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const refocusRaf = useRef(0);
  const panelId = useId();

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

  return { open, setOpen, hostRef, panelRef, triggerRef, panelId, close };
}
