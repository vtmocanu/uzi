// "Is this card uzi's work?" — PRD #102 M6, Decisions 12/13a/16.
//
// One predicate serves every consumer: the render filter, the Start-run gate, the
// Promote affordance and the PRDLESS toggle's visibility. That sharing is the
// decision, not a convenience. `prd_label` is operator-configurable, so a stored
// boolean would go stale the moment someone renames it; deriving from the labels
// the card already carries cannot.
//
// These live outside Board.tsx so they can be tested without a DOM, and so the
// issue view uses the SAME answer the board does. Two implementations of "is this a
// PRD card" is exactly how the card's affordances and the detail page's come to
// disagree.

import type { Card } from "./api";

/**
 * The label marking uzi's own self-improvement tracking issue, mirroring
 * `selfimprove.TrackingLabel` in the API.
 *
 * It is duplicated here rather than served from settings because it is not
 * configurable on either side: it is a compiled-in constant in the engine that
 * files the issue. The server enforces the same exclusion on Promote, so this
 * copy going stale hides a card it should show — it can never let a promote
 * through.
 */
export const SELF_IMPROVE_LABEL = "uzi-self-improve";

/** Whether a card carries the configured PRD label (Decision 12). Exact match, the same comparison the sync filter and the server-side run gate use. */
export function isPRDCard(card: Pick<Card, "labels">, prdLabel: string): boolean {
  return card.labels.includes(prdLabel);
}

/**
 * Whether a card is uzi's own self-improvement tracking issue (Decision 13a).
 *
 * It is open, it lives on uzi's own repo, and it deliberately carries neither a
 * PRD nor an autopilot label — so M6's additive fetch is the first thing that ever
 * put it on a board. It is excluded from the non-PRD render path and from Promote,
 * because promoting it would put the PRD label on internal machinery and let a
 * self-improve run be started by hand from a card.
 */
export function isSelfImproveTracker(card: Pick<Card, "labels">): boolean {
  return card.labels.includes(SELF_IMPROVE_LABEL);
}

/** Whether Promote should be offered on a card. */
export function canPromote(card: Pick<Card, "labels" | "closed">, prdLabel: string): boolean {
  return !card.closed && !isPRDCard(card, prdLabel) && !isSelfImproveTracker(card);
}

/**
 * The cards the LANES render, filtered from the payload set (Decision 12/13a and
 * the toggle).
 *
 * Filtering happens HERE, at render, and never at fetch: the non-PRD rows are in
 * the cache either way, and a fetch-time filter would make the toggle a sync
 * setting with a poll-cycle delay instead of an instant view preference.
 *
 * The self-improve tracker is excluded even with the toggle ON. That is not the
 * toggle being partial — the toggle means "show me the repo's other open issues",
 * and uzi's own bookkeeping issue is not one of them.
 */
export function visibleCards<T extends Pick<Card, "labels">>(
  cards: T[],
  prdLabel: string,
  showNonPRD: boolean,
): T[] {
  if (!showNonPRD) return cards.filter((c) => isPRDCard(c, prdLabel));
  return cards.filter((c) => isPRDCard(c, prdLabel) || !isSelfImproveTracker(c));
}
