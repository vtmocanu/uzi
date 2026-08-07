// Run-judge display helpers (PRD #46 M4): verdict tone/label and the
// recommendation-category label map. Kept in a lib (not inline in RunView) so the
// closed-enum → human-copy mapping is unit-tested and reused. None of these render
// untrusted text — they map the closed verdict/category enums to fixed UI strings;
// the untrusted free text (summary_md, rationale_md, target, judge_model) is rendered
// separately as escaped React text, through lib/safeText's stripUnsafeChars — escaping
// alone does not touch bidi overrides (issue #124).

import type { RecommendationCategory, ReviewVerdict } from "./api";

// verdictTone maps a verdict to a Badge tone. "ideal" is a clean pass (ok/green),
// "ok" is fine-with-notes (info), "issues" is worth attention (warning) — never
// danger, since a review is advisory, not a failure.
export function verdictTone(v: ReviewVerdict): "ok" | "info" | "warning" {
  switch (v) {
    case "ideal":
      return "ok";
    case "issues":
      return "warning";
    default:
      return "info";
  }
}

// verdictLabel is the short human label for the verdict chip.
export function verdictLabel(v: ReviewVerdict): string {
  switch (v) {
    case "ideal":
      return "Ideal";
    case "issues":
      return "Issues found";
    default:
      return "OK";
  }
}

// RECOMMENDATION_LABELS is the user's taxonomy (specs/human.md) as UI copy. An
// unknown category (a future kind) falls back to a humanized slug via
// recommendationLabel, so the panel never renders a raw enum.
export const RECOMMENDATION_LABELS: Record<RecommendationCategory, string> = {
  enable_tool: "Enable a tool or skill",
  install_worker_tool: "Install a worker tool",
  adjust_template: "Adjust an agent template",
  improve_agent: "Improve an agent",
  add_agent: "Add a missing agent",
  improve_uzi: "Improve uzi",
};

export function recommendationLabel(category: string): string {
  return (
    RECOMMENDATION_LABELS[category as RecommendationCategory] ??
    category.replace(/_/g, " ")
  );
}

// JUDGE_CATEGORIES is the ordered taxonomy the label-filter chips iterate (PRD #235 M2).
// It is the keys of RECOMMENDATION_LABELS, in map (spec/human.md) order, so the chip row
// and the ?category= URL guard share ONE source of truth — a category added to the map is
// a chip and a valid URL value for free, and cannot exist in one place but not the other.
export const JUDGE_CATEGORIES = Object.keys(
  RECOMMENDATION_LABELS,
) as readonly RecommendationCategory[];

// isCategory validates a ?category= token the way isBucket (Judge.tsx) guards ?bucket=: the
// input is a raw URL string, so tsc cannot narrow it, and an unknown value must be dropped
// silently rather than rendering an empty list. Mirrors the closed-set membership check.
export function isCategory(v: string | null): v is RecommendationCategory {
  return v !== null && (JUDGE_CATEGORIES as readonly string[]).includes(v);
}

// coordKey is the ONE (category, target) key that matches a recommendation to its filed
// link and its disposition (PRD #68/#94/#98). It MUST be used at both the build and the
// lookup site: a separator mismatch silently drops a persisted filed link back to the
// idle "File issue" button (the row then 409s on Create and the stale flag never fires).
//
// It lives here, exported, rather than being redefined per consumer. It was defined three
// times — RunView, the Judge page and mockApi — under a comment in RunView calling itself
// "the SINGLE source of truth", and the three had already diverged in their separator
// BYTES: the Judge page used a literal NUL (U+0000) while the other two used a space
// (PRD #98 review B1/N3, 2026-07-21). Nothing broke, because no map is ever shared across
// the three, but the invariant the comment asserted was not true of the code — so the fix
// is one definition, not a fourth restatement of the rule.
//
// A single space is a sound separator because `category` is a closed enum with no spaces,
// so the split point is unambiguous however the arbitrary `target` is spelled. A NUL is
// not worth its cost: it renders a source file BINARY to git (zero-line diffs, "Binary
// files differ" in every future review) and invisible to plain grep/rg, which is how a
// 32 KB page landed with no reviewable diff.
export function coordKey(category: string, target: string): string {
  return `${category} ${target}`;
}
