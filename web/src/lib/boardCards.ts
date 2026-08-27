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
//
// PRD #196 M1 generalises the RENDER FILTER — and only the render filter — from one
// label to a set. A board's MEMBERSHIP is `primary ∪ extras` (Decision 2): the
// configured primary label plus a per-user, per-repo set of extra labels. The extras
// are a VIEW preference and are never the run-eligible set (eligibility is a
// separate, admin-only list wired in M4), so an eligible-by-default label such as
// `bug` stays hideable — the union rule was rejected precisely because it made that
// impossible. `isPRDCard`/`canPromote`/`isSelfImproveTracker` stay PRIMARY-based:
// they answer eligibility/Promote/tracker questions, not "does this card belong on
// the board", so M1 does NOT touch them.
//
// Filtering still happens at render, never at fetch (Decision 12): the payload
// already carries every open issue regardless of label, so the control is instant
// rather than a poll-cycle away.

import type { Card } from "./api";

/**
 * The label marking uzi's own self-improvement tracking issue, mirroring
 * `schedsvc.SelfImproveTrackingLabel` in the API.
 *
 * It is duplicated here rather than served from settings because it is not
 * configurable on either side: it is a compiled-in constant in the scheduler's
 * self-improve fire path that files the issue. The server enforces the same
 * exclusion on Promote, so this copy going stale hides a card it should show — it
 * can never let a promote through.
 */
export const SELF_IMPROVE_LABEL = "uzi-self-improve";

/** Whether a card carries the configured PRD label (Decision 12). Exact match, the same comparison the sync filter and the server-side run gate use. */
export function isPRDCard(card: Pick<Card, "labels">, prdLabel: string): boolean {
  return card.labels.includes(prdLabel);
}

/**
 * The compiled-in fallback board-extra labels (PRD #196 M1, Decision 4/open
 * question 1). Membership on a board is `primary ∪ extras`, and until a user has
 * saved their own set these are the extras in effect.
 *
 * M2 will replace this with the admin-configured default delivered on the board
 * payload; for M1 it is the client-side fallback so the board can show `PRD` + `bug`
 * out of the box with no server change.
 */
export const DEFAULT_BOARD_EXTRA_LABELS = ["bug"] as const;

/**
 * Whether a card belongs to a board whose membership set is `membershipLabels`
 * (PRD #196 M1). A card is a member if it carries ANY of the membership labels,
 * matched exactly — the same comparison `isPRDCard` uses, generalised from one label
 * to a set. `membershipLabels` is `primary ∪ extras`, so the primary is always among
 * them and this stays a superset of `isPRDCard`.
 */
export function isMemberCard(card: Pick<Card, "labels">, membershipLabels: readonly string[]): boolean {
  return membershipLabels.some((l) => card.labels.includes(l));
}

/**
 * Whether a card is RUN-ELIGIBLE (PRD #196 M4): it carries ANY label in the
 * admin-configured run-eligible set, matched exactly (any-of). This is the same
 * shape as `isMemberCard`, but it answers a DISTINCT question — "may uzi work this?"
 * — and is deliberately its own named predicate rather than a reuse of the membership
 * check, because eligibility (admin-only instance policy) and membership (a per-user
 * VIEW preference) are orthogonal by design (Decision 1/2): a card can be visible but
 * not runnable, or runnable but hidden.
 *
 * `eligibleLabels` always includes the primary (the server guarantees it, and
 * `useAuth()` falls back to `[prdLabel]` for an older server), so this stays a
 * superset of `isPRDCard`. It is what drives the card's Start-vs-Promote affordance,
 * the solid-vs-quiet treatment and the PRDLESS toggle's visibility — mirroring the
 * server-side run gate so the button state matches what a Start would actually do.
 */
export function isEligibleCard(card: Pick<Card, "labels">, eligibleLabels: readonly string[]): boolean {
  return eligibleLabels.some((l) => card.labels.includes(l));
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

/**
 * Whether Promote should be offered on a card (PRD #196 M4). Promote is offered when a
 * card is NOT runnable — it is open, does not already carry a run-eligible label, and is
 * not the self-improve tracker. Promote adds the PRIMARY label server-side, which makes
 * the card runnable, so offering it on an already-eligible card would be a no-op button.
 *
 * Keys off the ELIGIBLE set, not the primary alone (M4): a `bug` card in an instance
 * whose eligible set includes `bug` is already runnable and gets Start run, not Promote
 * (mock §4); a visibility-only `documentation` card that is a board member but NOT
 * eligible still gets Promote (mock §7).
 */
export function canPromote(card: Pick<Card, "labels" | "closed">, eligibleLabels: readonly string[]): boolean {
  return !card.closed && !isEligibleCard(card, eligibleLabels) && !isSelfImproveTracker(card);
}

/**
 * The cards the LANES render, filtered from the payload set (Decision 12/13a and
 * the membership set).
 *
 * Filtering happens HERE, at render, and never at fetch: the non-member rows are in
 * the cache either way, and a fetch-time filter would make the control a sync
 * setting with a poll-cycle delay instead of an instant view preference.
 *
 * `membershipLabels` is `primary ∪ extras` (PRD #196 Decision 2). With `showAll`
 * off, only member cards render. With `showAll` on, everything renders EXCEPT the
 * self-improve tracker — unless the tracker is itself a member, which preserves the
 * old single-label semantics exactly (a member is always shown). The tracker
 * exclusion is not the control being partial — "show all other issues" means "show
 * me the repo's other open issues", and uzi's own bookkeeping issue is not one of
 * them.
 */
export function visibleCards<T extends Pick<Card, "labels">>(
  cards: T[],
  membershipLabels: readonly string[],
  showAll: boolean,
): T[] {
  if (!showAll) return cards.filter((c) => isMemberCard(c, membershipLabels));
  return cards.filter((c) => isMemberCard(c, membershipLabels) || !isSelfImproveTracker(c));
}

/**
 * The board search predicate (PRD #304 M2): does this card match the free-text query
 * `q`? Case-insensitive across the card's title, any of its labels, and its `#iid`. An
 * empty or whitespace-only query matches every card (the board with no filter). Runs
 * over `renderCards` AFTER `visibleCards`, so a card excluded by membership is
 * unfindable (Decision 9) — search narrows the board, it does not widen it.
 *
 * The iid arm strips a single leading '#' from the trimmed query and, if anything
 * remains, tests `String(iid).includes(remainder)` — so `#42` matches iid 429 and 142,
 * a substring match by design (a partial issue number is a useful search).
 */
export function matchesQuery(card: Pick<Card, "iid" | "title" | "labels">, q: string): boolean {
  const trimmed = q.trim();
  if (trimmed === "") return true;
  const needle = trimmed.toLowerCase();
  if (card.title.toLowerCase().includes(needle)) return true;
  if (card.labels.some((l) => l.toLowerCase().includes(needle))) return true;
  const iidQuery = trimmed.startsWith("#") ? trimmed.slice(1) : trimmed;
  return iidQuery !== "" && String(card.iid).includes(iidQuery);
}

/**
 * Split `text` into consecutive segments, marking every case-insensitive,
 * non-overlapping occurrence of `q` as `hit:true` and the rest `hit:false`, so a caller
 * can wrap the hits in `<mark>` (PRD #304 M2/M5). Concatenating the segment texts
 * reproduces `text` exactly — the ORIGINAL casing is preserved; only the match test is
 * lowercased. An empty/whitespace query yields a single non-hit segment (or `[]` when
 * `text` is itself empty).
 *
 * Matching is indexOf-based, never `new RegExp(q)`: `q` is user input and must never be
 * interpreted as a pattern. This function does NO sanitizing — the caller passes the
 * already display-sanitized string (titles are rendered through `stripUnsafeChars`), so
 * the segments it returns are safe to render for the same reason their input was.
 */
export function highlightSegments(text: string, q: string): { text: string; hit: boolean }[] {
  const needle = q.trim().toLowerCase();
  if (needle === "") return text === "" ? [] : [{ text, hit: false }];
  const hay = text.toLowerCase();
  const segments: { text: string; hit: boolean }[] = [];
  let from = 0;
  let i = hay.indexOf(needle, from);
  while (i !== -1) {
    if (i > from) segments.push({ text: text.slice(from, i), hit: false });
    segments.push({ text: text.slice(i, i + needle.length), hit: true });
    from = i + needle.length;
    i = hay.indexOf(needle, from);
  }
  if (from < text.length) segments.push({ text: text.slice(from), hit: false });
  return segments;
}
