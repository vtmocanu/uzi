// "Is this card uzi's to run?" — PRD #764.
//
// One predicate serves every consumer: the render filter, the runnable marker, the
// Start-run gate and the Promote affordance. That sharing is the decision, not a
// convenience. `uzi_label` is operator-configurable, so a stored boolean would go
// stale the moment someone renames it; deriving from the labels the card already
// carries cannot.
//
// These live outside Board.tsx so they can be tested without a DOM, and so the issue
// view uses the SAME answer the board does. Two implementations of "is this runnable"
// is exactly how the card's affordances and the detail page's come to disagree.
//
// PRD #764 collapses the old `primary ∪ extras` membership set and the separate
// run-eligible set into a single label: a card is a board member AND runnable iff it
// carries the configured `uzi` label. The board still shows every open issue behind
// the per-user "show all" toggle (D3); `uzi` adds a runnable marker plus the filter
// facet, so nothing disappears on upgrade.
//
// Filtering still happens at render, never at fetch: the payload already carries
// every open issue regardless of label, so the control is instant rather than a
// poll-cycle away.

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

/**
 * Whether a card is uzi's to run: it carries the configured `uzi` label (PRD #764) OR
 * the board's bot is one of its assignees (PRD #767 M5). Assignment is additive to the
 * label — the same single eligibility concept expressed two natural ways, matching the
 * server's OR gate. This is the single membership + runnable predicate, driving the
 * render filter, the runnable marker, the solid-vs-quiet treatment and the
 * Start-vs-Promote affordance, so the button state matches what a Start would do.
 *
 * `botForgeUserID > 0` guards an unresolved bot id (mirrors the server's `@bot_id>0` /
 * M2's `botID<=0` guard): a 0/absent id must never mark every assigned card runnable.
 *
 * Both `assignee_ids` and `botForgeUserID` are guarded for a rolling-deploy version
 * skew: a NEW web bootstrap can carry a positive bot id while an OLD api replica serves
 * a card payload WITHOUT `assignee_ids` (and vice versa, an old bootstrap omitting the
 * bot id). A missing `assignee_ids` is treated as `[]` and a missing bot id as 0, so the
 * predicate falls back to label-only rather than throwing on `undefined.includes`.
 */
export function isUziCard(
  card: Pick<Card, "labels" | "assignee_ids">,
  uziLabel: string,
  botForgeUserID: number | undefined,
): boolean {
  const botId = botForgeUserID ?? 0;
  return card.labels.includes(uziLabel) || (botId > 0 && (card.assignee_ids ?? []).includes(botId));
}

/**
 * Whether a card is uzi's own self-improvement tracking issue (Decision 13a).
 *
 * It is open, it lives on uzi's own repo, and it deliberately carries neither a `uzi`
 * nor an autopilot label. It is excluded from the "show all" render path and from
 * Promote, because promoting it would put the `uzi` label on internal machinery and
 * let a self-improve run be started by hand from a card.
 */
export function isSelfImproveTracker(card: Pick<Card, "labels">): boolean {
  return card.labels.includes(SELF_IMPROVE_LABEL);
}

/**
 * Whether Promote should be offered on a card (PRD #764, #767 M5). Promote is offered
 * when a card is NOT runnable — it is open, does not already carry the `uzi` label, is
 * not assigned to the bot, and is not the self-improve tracker. Promote adds the `uzi`
 * label server-side, which makes the card runnable, so offering it on an
 * already-runnable card (labelled OR assigned to the bot) would be a no-op button.
 */
export function canPromote(
  card: Pick<Card, "labels" | "assignee_ids" | "closed">,
  uziLabel: string,
  botForgeUserID: number | undefined,
): boolean {
  return !card.closed && !isUziCard(card, uziLabel, botForgeUserID) && !isSelfImproveTracker(card);
}

/**
 * The cards the LANES render, filtered from the payload set (PRD #764, D3).
 *
 * Filtering happens HERE, at render, and never at fetch: the non-member rows are in
 * the cache either way, and a fetch-time filter would make the control a sync setting
 * with a poll-cycle delay instead of an instant view preference.
 *
 * Membership is "carries `uzi` OR assigned to the bot" (PRD #764, #767 M5). With
 * `showAll` off, only those uzi's cards render. With `showAll` on, everything renders
 * EXCEPT the self-improve tracker and CLOSED non-member cards — unless the tracker
 * itself is a member, so a member is always shown. The tracker exclusion is not the
 * control being partial — "show all other issues" means "show me the repo's other OPEN
 * issues", and uzi's own bookkeeping issue is not one of them. The `!closed` guard
 * matches `canPromote` above (which also offers Promote only on open cards) and the
 * documented intent.
 */
export function visibleCards<T extends Pick<Card, "labels" | "assignee_ids" | "closed">>(
  cards: T[],
  uziLabel: string,
  botForgeUserID: number,
  showAll: boolean,
): T[] {
  if (!showAll) return cards.filter((c) => isUziCard(c, uziLabel, botForgeUserID));
  return cards.filter((c) => isUziCard(c, uziLabel, botForgeUserID) || (!isSelfImproveTracker(c) && !c.closed));
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
