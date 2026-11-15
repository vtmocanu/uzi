// The default Overview's single chip row. Its header slot is short, so native sticky
// positioning would stop at the end of that slot. Once the title leaves the window,
// the same row becomes fixed over the content column; its host keeps the flow height.

import { useEffect, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { cx } from "./ui";

interface ColumnRect {
  left: number;
  width: number;
}

export function OverviewStickyActions({
  titleRef,
  children,
}: {
  titleRef: RefObject<HTMLHeadingElement | null>;
  children: ReactNode;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const rowRef = useRef<HTMLDivElement>(null);
  const [stuck, setStuck] = useState(false);
  const [flowHeight, setFlowHeight] = useState(0);
  const [flowWidth, setFlowWidth] = useState(0);
  const [barHeight, setBarHeight] = useState(0);
  const [column, setColumn] = useState<ColumnRect>({ left: 0, width: 0 });

  useEffect(() => {
    const title = titleRef.current;
    const host = hostRef.current;
    if (!title || !host || typeof IntersectionObserver === "undefined" || !window.matchMedia) return;

    const wide = window.matchMedia("(min-width: 1280px)");
    const content = host.closest<HTMLElement>("[data-page-content-column]");
    const main = host.closest<HTMLElement>("main");
    const measure = () => {
      const rect = (content ?? main ?? host).getBoundingClientRect();
      setColumn((old) => old.left === rect.left && old.width === rect.width ? old : { left: rect.left, width: rect.width });
    };
    const update = () => setStuck(wide.matches && title.getBoundingClientRect().bottom <= 0);
    const observer = new IntersectionObserver(([entry]) => {
      setStuck(wide.matches && !entry.isIntersecting && entry.boundingClientRect.bottom <= 0);
    });
    observer.observe(title);
    const resize = typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    if (content) resize?.observe(content);
    if (main && main !== content) resize?.observe(main);
    window.addEventListener("resize", measure);
    wide.addEventListener("change", update);
    measure();
    update();
    return () => {
      observer.disconnect();
      resize?.disconnect();
      window.removeEventListener("resize", measure);
      wide.removeEventListener("change", update);
    };
  }, [titleRef]);

  useEffect(() => {
    const row = rowRef.current;
    if (!row) return;
    const measure = () => {
      const rect = row.getBoundingClientRect();
      if (row.dataset.stuck === "false") {
        setFlowHeight(rect.height);
        setFlowWidth(rect.width);
      } else setBarHeight(rect.height);
    };
    measure();
    const resize = typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    resize?.observe(row);
    return () => resize?.disconnect();
  }, []);

  // Positioning adds padding and a border without changing the row's content box,
  // so ResizeObserver alone does not observe the new outer height on the first stick.
  useLayoutEffect(() => {
    if (stuck && rowRef.current) setBarHeight(rowRef.current.getBoundingClientRect().height);
  }, [stuck, column.width]);

  useEffect(() => {
    if (!stuck) return;
    const style = document.documentElement.style;
    const previous = style.scrollPaddingTop;
    style.scrollPaddingTop = `${Math.ceil(barHeight || flowHeight) + 12}px`;
    return () => { style.scrollPaddingTop = previous; };
  }, [stuck, barHeight, flowHeight]);

  return (
    <div ref={hostRef} className="w-full" style={stuck ? { height: flowHeight, width: flowWidth } : undefined}>
      <div
        ref={rowRef}
        data-overview-chip-row=""
        data-stuck={stuck ? "true" : "false"}
        className={cx(
          "flex flex-wrap items-center gap-2 xl:justify-end",
          "min-[1440px]:gap-1 min-[1440px]:[&>div>button]:px-1.5 min-[1440px]:[&>div>button]:text-[11px] min-[1440px]:[&>button]:px-1.5 min-[1440px]:[&>button]:text-[11px]",
          stuck && "fixed top-0 z-20 border-b border-edge bg-ink py-1.5",
        )}
        style={stuck ? { left: column.left, width: column.width } : undefined}
      >
        {children}
      </div>
    </div>
  );
}
