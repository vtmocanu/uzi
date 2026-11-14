import type { ReactNode, RefObject } from "react";
import { cx } from "./ui";

// Only the markup and shared 360 px sheet geometry live here. useChipPopover owns focus.
export function ChipPopoverPanel({
  id,
  panelRef,
  label,
  alignment = "right",
  children,
}: {
  id: string;
  panelRef: RefObject<HTMLDivElement | null>;
  label: string;
  alignment?: "right" | "left";
  children: ReactNode;
}) {
  return (
    <div
      id={id}
      ref={panelRef}
      role="dialog"
      aria-label={label}
      tabIndex={-1}
      // Desktop: attach below the chip, right-aligned unless its caller needs the left edge.
      // Below sm: one fixed bottom sheet with 1rem side and bottom insets.
      className={cx(
        "absolute top-full z-30 mt-2 max-h-[70vh]",
        alignment === "left" ? "sm:left-0" : "right-0",
        "w-[min(360px,calc(100vw-32px))] overflow-y-auto rounded-xl border border-edge bg-raised text-left shadow-2xl outline-hidden max-sm:fixed max-sm:inset-x-4 max-sm:bottom-4 max-sm:top-auto max-sm:mt-0 max-sm:max-h-[calc(100dvh-2rem)] max-sm:w-auto",
      )}
    >
      {children}
    </div>
  );
}
