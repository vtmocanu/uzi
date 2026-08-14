// Date bucketing + free-text search for the runs index's past section (ux-tweaks
// item 3). Pure and clock-explicit (the runBadge.ts discipline): every function
// takes `now` so tests pin the wall clock. The grain follows recency — days inside
// the current week, weeks inside the current month, calendar months beyond — so the
// list reads like memory does: sharp about this week, coarse about last quarter.
//
// All boundaries are LOCAL time (the reader's "today", not UTC's), matching every
// timestamp the list already renders via toLocaleString.

export interface RunGroup<T> {
  key: string;
  label: string;
  runs: T[];
}

const DAY_MS = 86_400_000;

// Local midnight for the day containing `d`.
function startOfDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

// Local midnight of the Monday starting the week containing `d` (ISO weeks — the
// convention every timezone this product ships in reads by).
function startOfWeek(d: Date): Date {
  const day = startOfDay(d);
  const dow = (day.getDay() + 6) % 7; // Mon=0 … Sun=6
  return new Date(day.getTime() - dow * DAY_MS);
}

const pad = (n: number) => String(n).padStart(2, "0");
const dayKey = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;

/**
 * The bucket a past run falls into, from its anchor timestamp:
 *   - today / yesterday by name;
 *   - earlier days of the CURRENT week by weekday ("Monday");
 *   - earlier weeks of the CURRENT month as "Week of Aug 3";
 *   - anything older by calendar month ("July 2026").
 * A timestamp at or ahead of `now` (clock skew, a just-finished run) folds into
 * Today rather than inventing a future bucket.
 */
export function runBucket(iso: string, now: number): { key: string; label: string } {
  const d = new Date(iso);
  const n = new Date(now);
  const today = startOfDay(n);
  const day = startOfDay(d);
  const diffDays = Math.round((today.getTime() - day.getTime()) / DAY_MS);

  if (diffDays <= 0) return { key: "today", label: "Today" };
  if (diffDays === 1) return { key: "yesterday", label: "Yesterday" };
  if (day.getTime() >= startOfWeek(n).getTime()) {
    return { key: `d:${dayKey(day)}`, label: day.toLocaleDateString(undefined, { weekday: "long" }) };
  }
  if (day.getFullYear() === n.getFullYear() && day.getMonth() === n.getMonth()) {
    const ws = startOfWeek(day);
    return {
      key: `w:${dayKey(ws)}`,
      label: `Week of ${ws.toLocaleDateString(undefined, { month: "short", day: "numeric" })}`,
    };
  }
  return {
    key: `m:${day.getFullYear()}-${pad(day.getMonth() + 1)}`,
    label: day.toLocaleDateString(undefined, { month: "long", year: "numeric" }),
  };
}

/**
 * Group an already-sorted (newest-first) list into contiguous date buckets, keyed by
 * `anchor(run)`. Insertion-ordered: because bucketing is monotone over time, a
 * time-sorted input yields buckets in the same newest-first order.
 */
export function groupRuns<T>(runs: T[], anchor: (r: T) => string, now: number): RunGroup<T>[] {
  const byKey = new Map<string, RunGroup<T>>();
  const out: RunGroup<T>[] = [];
  for (const r of runs) {
    const { key, label } = runBucket(anchor(r), now);
    let g = byKey.get(key);
    if (!g) {
      g = { key, label, runs: [] };
      byKey.set(key, g);
      out.push(g);
    }
    g.runs.push(r);
  }
  return out;
}

/**
 * The runs-list search predicate, the board's matchesQuery transposed to run rows
 * (boardCards.ts): case-insensitive substring across the issue title, repo path,
 * worker name and status, plus the board's `#iid` arm (a single leading '#' is
 * stripped; a partial issue number is a useful search). Empty/whitespace queries
 * match everything. indexOf-based, never `new RegExp(q)` — q is user input.
 */
export function runMatchesQuery(
  run: {
    issue_title: string;
    repo_path: string;
    issue_iid: number | null;
    worker_name: string | null;
    status: string;
  },
  q: string,
): boolean {
  const trimmed = q.trim();
  if (trimmed === "") return true;
  const needle = trimmed.toLowerCase();
  if (run.issue_title.toLowerCase().includes(needle)) return true;
  if (run.repo_path.toLowerCase().includes(needle)) return true;
  if (run.worker_name && run.worker_name.toLowerCase().includes(needle)) return true;
  if (run.status.toLowerCase().includes(needle)) return true;
  const iidQuery = trimmed.startsWith("#") ? trimmed.slice(1) : trimmed;
  return iidQuery !== "" && run.issue_iid != null && String(run.issue_iid).includes(iidQuery);
}
