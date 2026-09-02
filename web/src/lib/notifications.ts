// A tiny in-app event so the bell badge (in AppShell) refreshes the moment the
// user marks something read on the inbox page, without threading state through the
// router. The badge also polls on navigation (PRD #46 M2: "poll on navigation, no
// WS needed"), so this is a responsiveness nicety layered on top of that poll, not
// the source of truth.

const NOTIFICATIONS_CHANGED_EVENT = "uzi:notifications-changed";

// emitNotificationsChanged signals that the caller's unread set may have changed.
export function emitNotificationsChanged(): void {
  window.dispatchEvent(new Event(NOTIFICATIONS_CHANGED_EVENT));
}

// onNotificationsChanged subscribes to the change signal and returns an
// unsubscribe function (for a useEffect cleanup).
export function onNotificationsChanged(cb: () => void): () => void {
  window.addEventListener(NOTIFICATIONS_CHANGED_EVENT, cb);
  return () => window.removeEventListener(NOTIFICATIONS_CHANGED_EVENT, cb);
}

// notificationTitle / notificationBody read the soft { title, body } convention
// notification producers follow (the judge is tenant #1), tolerating any payload
// shape. An absent title falls back to a humanized kind so an unknown producer
// still renders something legible.
export function notificationTitle(kind: string, payload: Record<string, unknown>): string {
  const t = payload.title;
  if (typeof t === "string" && t.trim() !== "") return t;
  // The early-limit-reset ping (PRD #1020) is account-level and carries no run — an
  // absent/empty payload.title must NOT fall through to the humanized kind
  // ("early limit reset"), so give it a fixed human title here.
  if (kind === EARLY_LIMIT_RESET_KIND) return "7-day rate limit reset early";
  return kind.replace(/_/g, " ");
}

export function notificationBody(payload: Record<string, unknown>): string {
  const b = payload.body;
  return typeof b === "string" ? b : "";
}

// The kind the judge's post-review ping carries (PRD #46). Spelled once, here, because M5's
// retarget and M5's grouping both key on it and a typo in either is silent.
const JUDGE_REVIEW_KIND = "judge_review";

// The kind the coalesced incidental-findings ping carries (PRD #333 M6/M7). Spelled once
// here because the inbox renderer and notificationLink both key on it — a typo is silent.
// Its payload is { run_id, repo_id, repo_path, count, finding_ids }; the deep-link opens the
// Findings backlog filtered to that run.
export const INCIDENTAL_FINDING_KIND = "incidental_finding";

// The kind the early-limit-reset alert carries (PRD #1020). Account-level (no run_id), so
// notificationLink correctly returns null for it — the only renderer seam it needs is a
// fixed inbox title in notificationTitle, since its payload may omit `title`.
export const EARLY_LIMIT_RESET_KIND = "early_limit_reset";

// notificationLink is the inbox's deep-link, and it is KIND-CONDITIONAL on purpose.
//
// Read the shape of the bug this guards before simplifying it. The inbox linked
// `/runs/${run_id}` for EVERY kind carrying a run id, so "retarget the judge notification"
// looks like a one-line URL change — and that change silently redirects every other kind
// that has a run id (run failures, MR events, anything a future producer adds) to a page
// about judge recommendations. Worse, a test that only checks the judge row PASSES UNDER
// EXACTLY THAT BUG: the judge row lands on /judge either way. So the gate is a NON-judge
// notification still landing on /runs/..., and it is asserted in this module's tests.
//
// Returning null for a row with no run id keeps "there is nothing to open" distinct from
// "open the wrong thing".
//
// WHY THE RAW INTERPOLATION IS SAFE, stated because it is not self-evident and because this
// comment explains the guard at length while saying nothing about the value being
// interpolated. `runID` is a server-generated UUID, and the fixed `/judge?run=` prefix makes
// the result a same-origin PATH whatever it contains — so the worst a hostile value could do
// is append extra query parameters, which this app ignores. That is why there is no
// encodeURIComponent. It stops being true if either half changes: if a caller ever passes
// user-supplied text here, or if this is ever used to build an ABSOLUTE url, encode it. The
// invariant lives here rather than being inferred from the DTO, because removing the
// server-side guarantee elsewhere is what would make this unsafe.
export function notificationLink(kind: string, runID: string | null | undefined): string | null {
  if (!runID) return null;
  if (kind === JUDGE_REVIEW_KIND) return `/judge?run=${runID}`;
  // A findings ping opens the per-repo Findings backlog filtered to the flagging run (PRD
  // #333 M7) — the same kind-conditional guard the judge retarget uses, so a future run-id
  // kind still lands on /runs rather than being silently redirected here. runID is a
  // server-generated UUID and the fixed prefix keeps this a same-origin path, so no encode.
  if (kind === INCIDENTAL_FINDING_KIND) return `/findings?run=${runID}`;
  return `/runs/${runID}`;
}

// ---- inbox grouping of consecutive judge rows (PRD #98 M5, Decision 5) -----------------

// A minimal structural view of a notification — everything the grouper needs and nothing
// else, so this module does not depend on the API DTO.
export type GroupableNotification = {
  id: string;
  kind: string;
  created_at: string;
  read_at?: string | null;
};

export type NotificationGroup<T extends GroupableNotification> =
  | { kind: "single"; item: T }
  | { kind: "judge_group"; items: T[] };

// JUDGE_GROUP_WINDOW_MS bounds a group in TIME as well as in adjacency (Decision 5 keys on
// "kind + a time window"). Adjacency alone would fold a review from last month under a
// header describing this afternoon's burst, purely because nothing else happened in
// between — and the header's count would then describe a span the user cannot see. 24h is
// chosen to match how the flood actually arrives: a busy factory day produces the run of
// rows this exists to collapse, and a gap longer than a day means a different sitting.
export const JUDGE_GROUP_WINDOW_MS = 24 * 60 * 60 * 1000;

// groupNotifications collapses each maximal run of ADJACENT judge_review rows that are
// within JUDGE_GROUP_WINDOW_MS of their neighbour into one group. It is render-only: every
// input row appears exactly once in the output, in the same order, so read-state,
// per-row ids and the API's paging are all untouched (Decision 5: no new storage, no
// scheduler).
//
// A run of ONE stays a single row. A lone judge notification under a "1 review ready"
// header is strictly worse than the row itself — it costs a click to reach what was already
// on screen.
export function groupNotifications<T extends GroupableNotification>(items: T[]): NotificationGroup<T>[] {
  const out: NotificationGroup<T>[] = [];
  let run: T[] = [];
  const flush = () => {
    if (run.length > 1) out.push({ kind: "judge_group", items: run });
    else if (run.length === 1) out.push({ kind: "single", item: run[0] });
    run = [];
  };
  for (const n of items) {
    if (n.kind !== JUDGE_REVIEW_KIND) {
      flush();
      out.push({ kind: "single", item: n });
      continue;
    }
    const prev = run[run.length - 1];
    // An unparseable timestamp yields NaN, and NaN > window is false — which would fold the
    // row in silently. Break the run instead: an unreadable date is a reason to show the row
    // on its own, not a reason to trust adjacency.
    if (prev) {
      const gap = Math.abs(new Date(prev.created_at).getTime() - new Date(n.created_at).getTime());
      if (!Number.isFinite(gap) || gap > JUDGE_GROUP_WINDOW_MS) flush();
    }
    run.push(n);
  }
  flush();
  return out;
}
