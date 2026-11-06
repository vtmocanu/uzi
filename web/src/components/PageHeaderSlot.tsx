// A header slot the app shell renders ABOVE its banner stack (PRD #3878). A page that wants
// its header to sit above the banners wraps it in InShellHeader: the header is portalled
// into the slot node, so the shell's one banner stack stays mounted at the same tree
// position on every route (no banner remounts on navigation, so snooze/dismiss state
// survives) while the header precedes it in the DOM.
//
// Outside a shell (no slot node, e.g. a page rendered on its own in a test) the header
// renders inline, where it was written.

import { createContext, useContext, type ReactNode } from "react";
import { createPortal } from "react-dom";

export const PageHeaderSlotContext = createContext<HTMLElement | null>(null);

export function InShellHeader({ children }: { children: ReactNode }) {
  const slot = useContext(PageHeaderSlotContext);
  if (!slot) return <>{children}</>;
  // The slot div adds no spacing when empty; the wrapper gives a filled slot its bottom
  // spacing so the banners and the content below do not touch the header.
  return createPortal(<div className="mb-6">{children}</div>, slot);
}
