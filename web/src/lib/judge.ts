// Run-judge display helpers (PRD #46 M4): verdict tone/label and the
// recommendation-category label map. Kept in a lib (not inline in RunView) so the
// closed-enum → human-copy mapping is unit-tested and reused. None of these render
// untrusted text — they map the closed verdict/category enums to fixed UI strings;
// the untrusted free text (summary_md, rationale_md, target) is rendered separately
// as escaped React text.

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
