import { useEffect, useRef } from "react";
import { type RecommendationCategory } from "../../lib/api";
import { JUDGE_CATEGORIES, recommendationLabel } from "../../lib/judge";
import { cx } from "../../components/ui";
import { XIcon } from "../../components/icons";
import { keepInRowView, useEdgeFade } from "../../components/TabRow";

// LabelFilter is the recommendation-label chip row (PRD #235 M2): one toggle chip per
// JUDGE_CATEGORIES key, multi-select (OR semantics — a group has one category, so AND is
// meaningless), driving the ?category= URL param. The Clear control is always mounted
// (kept invisible and non-focusable when nothing is selected, so the panel height does not
// shift) and removes the param entirely.
//
// Each chip carries a per-category GROUP count from `counts` — the canonical
// /me/judge/category-stats matrix indexed by the active tab (PRD #270; the page derives this
// per-bucket slice and passes it here unchanged), NOT a tally of the on-screen groups (which
// are capped/bucket-filtered; tallying them is the banned anti-pattern). `counts[cat] ?? 0`
// per chip, so a category the aggregate never returned reads 0. A true-zero chip is DIMMED —
// "none of this kind" — rather than hidden, so the six chips stay a stable, learnable set
// (open question 3).
//
// The numeric badge is aria-hidden and the chip's accessible name is set explicitly via
// aria-label ("Improve uzi, 6 groups") so the count is announced honestly WITHOUT the raw
// digit polluting the name the way rendering it as plain button text would.
export function LabelFilter({
  selected,
  counts,
  onToggle,
  onClear,
  layout = "wrap",
}: {
  selected: RecommendationCategory[];
  counts: Record<string, number>;
  onToggle: (cat: RecommendationCategory) => void;
  onClear: () => void;
  /** `wrap` (default) wraps chips onto more lines; `row` keeps one non-wrapping, sideways-scrolling line (the pinned lg layout). */
  layout?: "wrap" | "row";
}) {
  const active = new Set(selected);
  const scrollerRef = useRef<HTMLDivElement>(null);
  useEdgeFade(scrollerRef, layout);
  const selectedKey = selected.join(",");
  useEffect(() => {
    const scroller = scrollerRef.current;
    if (!scroller) return;
    // Inset 24: clear of the row's 24px edge fade.
    keepInRowView(scroller, scroller.querySelector<HTMLElement>('[aria-pressed="true"]'), 24);
  }, [selectedKey, layout]);
  // One inline chip row (PRD #1183 M2): no surrounding box, no eyebrow, no caption line — the
  // "counts are groups, deduped by target" explanation moves onto the row's title attribute so
  // the count unit is still discoverable without a standing line of chrome. The Clear control
  // stays always-mounted (invisible + non-focusable when nothing is selected) so the row height
  // does not shift.
  const chips = (
    <>
      {JUDGE_CATEGORIES.map((cat) => {
        const on = active.has(cat);
        const count = counts[cat] ?? 0;
        const label = recommendationLabel(cat);
        const empty = count === 0;
        return (
          <button
            key={cat}
            type="button"
            aria-pressed={on}
            aria-label={`${label}, ${count} ${count === 1 ? "group" : "groups"}`}
            onClick={() => onToggle(cat)}
            className={cx(
              "inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-sm font-medium transition-colors",
              layout === "row" && "shrink-0 focus-visible:ring-inset focus-visible:ring-offset-0",
              on
                ? "border-brand/60 bg-brand/[0.13] text-brand"
                : "border-edge bg-raised text-muted hover:border-edge-strong hover:text-fg",
              // A true-zero whole-backlog chip is dimmed — "none of this kind" — but kept
              // visible and clickable (open question 3). Dim only, no `disabled`: a chip
              // already selected via ?category= whose count is 0 must stay pressable so the
              // user can still toggle it off to clear the filter.
              empty && "opacity-50",
            )}
          >
            <span>{label}</span>
            {/* The count pill mirrors the #235 mockup's `.chip .n` badge (a rounded
                well, tabular digits). text-faint is the deliberately AA-compliant
                token (index.css) — no opacity dimming, so the digit clears WCAG AA;
                the pressed chip recolors the badge like the mockup. */}
            <span
              aria-hidden="true"
              className={cx(
                "rounded-full px-1.5 py-px text-[0.68rem] tabular-nums",
                on ? "bg-brand/[0.12] text-brand-hover" : "bg-ink/60 inset-well text-faint",
              )}
            >
              {count}
            </span>
          </button>
        );
      })}
    </>
  );
  const clear = (
    <>
      {/* Clear rides at the end of the same row; always mounted so the row height is stable,
          invisible + non-focusable until something is selected. */}
      <button
        type="button"
        onClick={onClear}
        aria-hidden={active.size === 0}
        tabIndex={active.size === 0 ? -1 : 0}
        className={`${layout === "row" ? "shrink-0" : "ml-auto"} inline-flex min-h-[24px] items-center gap-1 rounded-md px-2 py-1 text-xs font-medium text-muted transition-colors hover:bg-raised hover:text-fg${
          active.size === 0 ? " invisible pointer-events-none" : ""
        }`}
      >
        <XIcon /> Clear
      </button>
    </>
  );
  if (layout === "row") {
    // The chips scroll sideways inside their own scroller; Clear sits OUTSIDE it, at the
    // row's right end, so an active filter can always be cleared however far the chips scroll.
    return (
      <div
        role="group"
        aria-label="Filter by recommendation label"
        title="counts are groups, deduped by target"
        className="flex items-center gap-2 py-1.5"
      >
        <div
          ref={scrollerRef}
          data-fade-row=""
          className="flex min-w-0 flex-1 flex-nowrap items-center gap-2 overflow-x-auto overflow-y-hidden"
        >
          {chips}
        </div>
        {clear}
      </div>
    );
  }
  return (
    <div
      role="group"
      aria-label="Filter by recommendation label"
      title="counts are groups, deduped by target"
      className="flex flex-wrap items-center gap-2"
    >
      {chips}
      {clear}
    </div>
  );
}
