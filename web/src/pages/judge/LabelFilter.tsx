import { type RecommendationCategory } from "../../lib/api";
import { JUDGE_CATEGORIES, recommendationLabel } from "../../lib/judge";
import { cx } from "../../components/ui";
import { XIcon } from "../../components/icons";

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
}: {
  selected: RecommendationCategory[];
  counts: Record<string, number>;
  onToggle: (cat: RecommendationCategory) => void;
  onClear: () => void;
}) {
  const active = new Set(selected);
  return (
    <section
      aria-label="Filter by recommendation label"
      className="rounded-lg border border-brand/30 bg-brand/[0.05] px-3 py-3"
    >
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <span className="text-xs font-semibold uppercase tracking-wide text-muted">Filter by label</span>
        <button
          type="button"
          onClick={onClear}
          aria-hidden={active.size === 0}
          tabIndex={active.size === 0 ? -1 : 0}
          className={`ml-auto inline-flex min-h-[24px] items-center gap-1 rounded-md px-2 py-1 text-xs font-medium text-muted transition-colors hover:bg-raised hover:text-fg${
            active.size === 0 ? " invisible pointer-events-none" : ""
          }`}
        >
          <XIcon /> Clear
        </button>
      </div>
      <p className="mb-2 text-xs text-faint">counts are groups, deduped by target</p>
      <div role="group" aria-label="Recommendation labels" className="flex flex-wrap gap-2">
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
                  on ? "bg-brand/[0.12] text-brand-hover" : "bg-ink/60 text-faint",
                )}
              >
                {count}
              </span>
            </button>
          );
        })}
      </div>
    </section>
  );
}
