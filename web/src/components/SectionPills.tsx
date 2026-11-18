// The "Sections on this page" pill row an Admin tab pins under its tabs (AdminShell's
// `pinned` slot): one non-wrapping, sideways-scrolling row with edge fades, whose pill for
// the section being read carries aria-current="true" and is kept in the row's view (#4419).
// Shared by Admin → Instance and Admin → Workers (#4454).
//
// The page owns `endRef` (its zero-height end sentinel after the last card), `pendingRef`
// (the requested section: the page's deep-link effect sets it before its scrollIntoView,
// and a pill click sets it here) and `evaluateRef` (the page calls it in a
// requestAnimationFrame after that scroll). `anchorIds` is the page's observed id set.
// All four must keep a stable identity (a module-level Set and useRef objects): they are
// observer-effect dependencies, and a fresh value per render would rebuild the observers.

import { useCallback, useEffect, useRef, useState, type MutableRefObject, type RefObject } from "react";
import { cx } from "./ui";
import { keepInRowView, useEdgeFade } from "./TabRow";

export function SectionPills({
  sections,
  anchorIds,
  endRef,
  pendingRef,
  evaluateRef,
}: {
  sections: { id: string; label: string }[];
  anchorIds: ReadonlySet<string>;
  endRef: RefObject<HTMLElement | null>;
  pendingRef: MutableRefObject<string | null>;
  evaluateRef: MutableRefObject<(() => void) | null>;
}) {
  const sectionKey = sections.map((s) => s.id).join(",");
  const navRef = useRef<HTMLElement>(null);
  const [currentId, setCurrentId] = useState<string | null>(null);
  const currentRef = useRef<string | null>(null);
  const current = sectionKey.split(",").includes(currentId ?? "") ? currentId : (sections[0]?.id ?? null);

  const choose = useCallback((id: string) => {
    if (currentRef.current === id) return;
    currentRef.current = id;
    setCurrentId(id);
  }, [setCurrentId]);

  useEdgeFade(navRef, sectionKey);

  // Current-section highlight. Observers only TRIGGER evaluation (no scroll listener); each
  // evaluation reads fresh rects. The reading line sits LANDING below the pinned block's
  // bottom edge: the root scroll-padding gap (12) + the cards' scroll-mt-6 (24) + 4px slack,
  // so a pill jump, a deep link or the command menu puts the target at or above the line.
  // Current = the last pill section, in document order, whose top is at or above the line
  // (the first at the page top). A pill-less card spanning the line leaves the preceding pill
  // lit. Near the page end the line can no longer reach the last cards: the clicked pill
  // stays current while its section is still below the line, else the last pill.
  useEffect(() => {
    const nav = navRef.current;
    const block = nav?.closest<HTMLElement>("[data-pinned-block-root]");
    if (!sectionKey || !nav || !block || typeof IntersectionObserver === "undefined") return;
    const LANDING = 12 + 24 + 4;
    const ids = sectionKey.split(",");
    const els = ids
      .map((id) => document.getElementById(id))
      .filter((el): el is HTMLElement => el !== null);
    // Stuck geometry: the block's sticky top + height, independent of whether it is stuck
    // right now, so the observer band matches where evaluate() reads once scrolled.
    const stuckLineY = () =>
      (parseFloat(getComputedStyle(block).top) || 0) + block.getBoundingClientRect().height + LANDING;
    // Live position, used by evaluate() against fresh rects.
    const lineY = () => block.getBoundingClientRect().bottom + LANDING;
    const observed = [...new Set([...anchorIds, ...ids])]
      .map((id) => document.getElementById(id))
      .filter((el): el is HTMLElement => el !== null);
    const evaluate = () => {
      if (els.length === 0) return;
      const line = lineY();
      const end = endRef.current;
      const atEnd = window.scrollY > 0 && !!end && end.getBoundingClientRect().top <= window.innerHeight;
      if (atEnd) {
        const pending = pendingRef.current;
        const target = pending ? document.getElementById(pending) : null;
        if (pending && target && target.getBoundingClientRect().top >= line - 6) {
          // Only pill ids may be chosen: a pill-less card maps to the nearest pill section
          // before it in document order (the last pill when none precedes it).
          let pill = els[els.length - 1];
          let before: HTMLElement | null = null;
          for (const el of els) {
            if (el === target || el.compareDocumentPosition(target) & Node.DOCUMENT_POSITION_FOLLOWING) before = el;
          }
          if (before) pill = before;
          choose(pill.id);
        } else choose(els[els.length - 1].id);
        return;
      }
      pendingRef.current = null;
      let cur = els[0];
      for (const el of els) if (el.getBoundingClientRect().top <= line) cur = el;
      choose(cur.id);
    };

    let lineIO: IntersectionObserver | null = null;
    const build = () => {
      lineIO?.disconnect();
      // Whole-pixel 1px band: an unrounded edge let a card top land on a sub-pixel edge and
      // miss its crossing (the 20px-step browser test pins this).
      const y = Math.round(stuckLineY());
      const below = Math.max(0, window.innerHeight - y - 1);
      lineIO = new IntersectionObserver(evaluate, { root: document, rootMargin: `-${y}px 0px -${below}px 0px` });
      for (const el of observed) lineIO.observe(el);
    };
    build();
    evaluateRef.current = evaluate;

    // The block's stuck flip moves its live bottom (and so evaluate()'s line) without any
    // observed card crossing the band: re-evaluate once when it changes.
    const mo = new MutationObserver(evaluate);
    mo.observe(block, { attributes: true, attributeFilter: ["data-stuck"] });

    const endIO = new IntersectionObserver(evaluate, { root: document });
    if (endRef.current) endIO.observe(endRef.current);

    const ro = typeof ResizeObserver !== "undefined" ? new ResizeObserver(build) : null;
    ro?.observe(block);
    window.addEventListener("resize", build);
    window.addEventListener("uzi:app-bar-h", build);
    return () => {
      evaluateRef.current = null;
      lineIO?.disconnect();
      mo.disconnect();
      endIO.disconnect();
      ro?.disconnect();
      window.removeEventListener("resize", build);
      window.removeEventListener("uzi:app-bar-h", build);
    };
  }, [sectionKey, choose, anchorIds, endRef, pendingRef, evaluateRef]);

  useEffect(() => {
    const nav = navRef.current;
    if (!nav || !current) return;
    // Inset 24: clear of the row's 24px edge fade.
    keepInRowView(nav, nav.querySelector<HTMLElement>(`a[href="#${current}"]`), 24);
  }, [current]);

  return (
    <nav
      ref={navRef}
      aria-label="Sections on this page"
      data-fade-row=""
      className="flex flex-nowrap gap-1.5 overflow-x-auto overflow-y-hidden px-1 py-2"
    >
      {sections.map((s) => (
        <a
          key={s.id}
          href={`#${s.id}`}
          aria-current={s.id === current ? "true" : undefined}
          onClick={() => {
            pendingRef.current = s.id;
            choose(s.id);
          }}
          className={cx(
            "shrink-0 whitespace-nowrap rounded-full border px-2.5 py-1 text-xs font-medium transition-colors focus-visible:ring-inset focus-visible:ring-offset-0",
            s.id === current
              ? "border-brand text-fg"
              : "border-edge text-muted hover:border-edge-strong hover:text-fg",
          )}
        >
          {s.label}
        </a>
      ))}
    </nav>
  );
}
