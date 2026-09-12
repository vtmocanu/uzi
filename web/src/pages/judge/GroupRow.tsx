import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  api,
  type JudgeAdminGroup,
  type JudgeAdminOccurrence,
  type JudgeBacklogBucket,
  type JudgeOccurrence,
  type JudgeRecommendationGroup,
  type PendingJudge,
  type Repo,
  type ReviewVerdict,
  type RunReview,
} from "../../lib/api";
import { recommendationLabel } from "../../lib/judge";
import { openOfRunsLabel } from "../../lib/judgeBacklog";
import { stripUnsafeChars } from "../../lib/safeText";
import { judgeBadge } from "../../lib/judgeBadge";
import { Markdown } from "../../components/Markdown";
import { TriageActions } from "../../components/triage/TriageActions";
import { TriageStateChip } from "../../components/triage/TriageStateChip";
import { IssueDraftCard } from "../../components/triage/IssueDraftCard";
import { judgeState, type TriageState } from "../../components/triage/triageCopy";
import { Badge } from "../../components/ui";
import { ChevronDownIcon, ChevronRightIcon } from "../../components/icons";

// GroupRow is one deduped (category, target) row: the category + target header, the "seen
// in N runs" frequency chip, the group's rollup state chip, the shared triage action row,
// and the occurrence expander. rationale_preview and target are UNTRUSTED judge text —
// rendered as escaped React text with whitespace-pre-wrap, NEVER through a markdown renderer
// or dangerouslySetInnerHTML, and passed through stripUnsafeChars first (issue #124):
// escaping does not touch a bidi override, and the api's review-ingest scrub dropped Cc but
// not Cf until it learned both — which leaves every row stored before that fix still carrying
// them.
//
// The action row is TriageActions in DELEGATED mode (PRD #1183): File issue · Mark done ·
// Dismiss ▾, all secondary weight. Mark done / Dismiss forward to the page's bulk dispose
// fan-out; File issue opens the shared IssueDraftCard inside the row for the group's newest
// OPEN occurrence (see newestOpenOccurrence below). The per-occurrence File issue button is
// gone: the expander is now for reading (full rationale · run link · verdict · state chip),
// not acting.
//
// fetchReview is the OMITTABLE capability that upgrades the expander's clamped preview to the
// newest occurrence's full rationale on first expand (PRD #1183 M2). Judge.tsx passes
// api.getRunReview under `mine`; PRD #1184's admin scope (`all`) has no run_id to fetch with and
// omits it, and the expander then shows only the clamped preview.
type FetchReview = (
  runId: string,
) => Promise<{ review: RunReview | null; pending_judge: PendingJudge | null }>;

// AnyOccurrence is either scope's occurrence: the owner JudgeOccurrence (carries run_id/rec_id/
// run_title) or the attribution-hidden JudgeAdminOccurrence (no ids, no title). Both share the
// `bucket` and `judged_at` newestOpenOccurrence and the state chip read, so the picker and the
// chip work over the union; the owner-only file flow narrows back to JudgeOccurrence.
type AnyOccurrence = JudgeOccurrence | JudgeAdminOccurrence;

// newestOpenOccurrence picks the group's newest OPEN member — the coordinate the File-issue
// draft targets. Among the bucket "todo" occurrences it takes the largest judged_at compared
// as a PARSED timestamp, not as a string: RFC3339 does NOT sort lexicographically once the
// fractional-second precision differs (e.g. "…00.1Z" sorts AFTER "…00.12Z" because 'Z' > '2',
// yet .1s is earlier than .12s), which would target the wrong occurrence. A member missing (or
// unparseable) judged_at never wins over one carrying a valid later value, and with none present
// it falls back to wire order (the backlog query delivers rv.updated_at DESC, so the first todo
// is already the newest). Generic over AnyOccurrence so the admin scope (PRD #1184) reuses it —
// under `all` the picker only gates whether File issue is offered (canAct); the coordinate, not
// the occurrence, is what the admin filer resolves server-side.
function newestOpenOccurrence<T extends AnyOccurrence>(occurrences: T[]): T | undefined {
  return occurrences
    .filter((o) => o.bucket === "todo")
    .reduce<T | undefined>((best, o) => {
      if (!best) return o;
      const ot = o.judged_at ? Date.parse(o.judged_at) : NaN;
      const bt = best.judged_at ? Date.parse(best.judged_at) : NaN;
      if (!Number.isNaN(ot) && (Number.isNaN(bt) || ot > bt)) return o;
      return best;
    }, undefined);
}

// GroupRow renders BOTH judge scopes off one component (PRD #1184 M4 — "do not fork a second
// row component"): the owner backlog (`scope="mine"`, a JudgeRecommendationGroup) and the admin
// cross-user aggregate (`scope="all"`, a JudgeAdminGroup). Under `all` it (a) shows the distinct
// user_count beside the frequency chip, (b) renders attribution-hidden occurrences — "A run,
// judged <time>" with a verdict + state chip and NO link/title — and (c) drives the File-issue
// flow through the admin draft/file endpoints (keyed by coordinate, not run/rec id). Whether the
// Dismiss button renders is the CALLER's choice: Judge.tsx passes no onDismiss under `all`, so
// TriageActions hides it (there is no cross-user Dismiss — dismissing another user's
// recommendation is their judgment). `scope` defaults to "mine" so every owner call site is
// unchanged.
export function GroupRow({
  group,
  scope = "mine",
  selected,
  onToggleSelect,
  onDispose,
  repos,
  onFiled,
  fetchReview,
}: {
  group: JudgeRecommendationGroup | JudgeAdminGroup;
  scope?: "mine" | "all";
  selected: boolean;
  onToggleSelect: () => void;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
  repos: Repo[];
  onFiled: () => void;
  fetchReview?: FetchReview;
}) {
  const isAdmin = scope === "all";
  const [expanded, setExpanded] = useState(false);
  const [filing, setFiling] = useState(false);
  // Local just-filed override: the issue a Create click produced plus fileIssue's `warning`.
  // fileIssue's `warning` is a created-with-warning SUCCESS — the forge issue WAS created, only
  // its local link/cache could not settle — set EXACTLY when the link did not settle, so the
  // backlog refetch below then OMITS the coordinate, the row stays in the `todo` bucket, and
  // File issue would re-arm against a forge issue that already exists (a silent duplicate).
  // This override keeps the "Filed #N" chip and the warning on the row regardless, while the
  // settled refetch still reconciles a link that DID settle. Survives onFiled's in-place reload
  // (the row keeps its coordKey key).
  const [justFiled, setJustFiled] = useState<{ iid: number; web_url: string; warning: string } | null>(null);
  const openCount = group.open_count;
  const newestOpen = newestOpenOccurrence<AnyOccurrence>(group.occurrences);
  // The full-rationale fetch is owner-only: the admin occurrence carries no run_id (attribution
  // is hidden) and Judge.tsx omits fetchReview under `all`, so newestOpenRunId is undefined there
  // and the expander's fetch effect no-ops, leaving the clamped preview.
  const newestOpenRunId = !isAdmin ? (newestOpen as JudgeOccurrence | undefined)?.run_id : undefined;
  const canAct = openCount > 0;

  // The newest open occurrence's FULL rationale, fetched ONCE per newest-open run on first
  // expand via fetchReview and cached here (this component's key is the coordinate, so the
  // cache survives an onFiled reload). The cache is keyed to the newest-open RUN: when a
  // backlog reload changes which run is newest-open, the stale rationale is dropped and the new
  // run is fetched. Until it lands — and on error, or when fetchReview is omitted — the expander
  // shows the clamped preview instead. null means "not loaded".
  const [rationaleMd, setRationaleMd] = useState<string | null>(null);
  const rationaleFetchedFor = useRef<string | null>(null);

  useEffect(() => {
    // Only after the user expands, and only when the capability and an open occurrence to fetch
    // it from are both present.
    if (!expanded || !fetchReview || !newestOpenRunId) return;
    // Fetch once per newest-open run. When the run changes across a backlog reload, drop the
    // previous run's rationale (so the clamped preview shows until the refetch lands) and fetch
    // the new one; when it is unchanged, keep the cached rationale and skip.
    if (rationaleFetchedFor.current === newestOpenRunId) return;
    rationaleFetchedFor.current = newestOpenRunId;
    setRationaleMd(null);
    let alive = true;
    void (async () => {
      try {
        const { review } = await fetchReview(newestOpenRunId);
        // Pull the recommendation at this group's coordinate; its rationale_md is the full
        // text the clamped preview was cut from.
        const rec = review?.recommendations.find(
          (r) => r.category === group.category && r.target === group.target,
        );
        if (alive && rec) setRationaleMd(rec.rationale_md);
      } catch {
        // Leave rationaleMd null so the clamped preview stays.
      }
    })();
    return () => {
      alive = false;
    };
  }, [expanded, fetchReview, newestOpenRunId, group.category, group.target]);

  // Judge.tsx keys a row by coordKey alone, so this instance is REUSED across a scope switch.
  // Under `all` the fetch effect above no-ops (newestOpenRunId is undefined), which would leave a
  // full rationale cached from the `mine` scope still rendering in the expander. Clear the cache
  // when entering the admin scope so `all` shows only the anonymized clamped preview (PRD #1184
  // M4, Finding [8]). The `mine`-scope behaviour is untouched.
  useEffect(() => {
    if (isAdmin) {
      setRationaleMd(null);
      rationaleFetchedFor.current = null;
    }
  }, [isAdmin]);

  return (
    <li className="rounded-lg border border-edge bg-raised/40">
      <div className="flex flex-wrap items-start gap-2 px-3 py-2.5">
        <input
          type="checkbox"
          checked={selected}
          onChange={onToggleSelect}
          aria-label={`Select ${recommendationLabel(group.category)} ${stripUnsafeChars(group.target)}`}
          className="mt-1 h-4 w-4 shrink-0 accent-brand"
        />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone="info">{recommendationLabel(group.category)}</Badge>
            {group.target.trim() !== "" && (
              <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">
                {stripUnsafeChars(group.target)}
              </code>
            )}
            {/* One frequency chip folding "seen in M runs" and the open count into one phrase
                (PRD #1183 M2): "N open of M runs", or "M runs, all settled" once none remain.
                Under the admin `all` scope a distinct-user tail follows — "· K users" — the
                cross-user "how widespread" signal the aggregate ranks by (PRD #1184 M4). */}
            <span className="text-xs text-faint">
              {openOfRunsLabel(group.open_count, group.run_count)}
              {isAdmin && ` · ${(group as JudgeAdminGroup).user_count} ${(group as JudgeAdminGroup).user_count === 1 ? "user" : "users"}`}
            </span>
            {/* The local just-filed override wins and shows the "Filed #N" link chip; otherwise
                the group rollup chip (never rendered for a still-`todo` group). */}
            {justFiled ? (
              <TriageStateChip state="filed" filed={{ issue_iid: justFiled.iid, issue_url: justFiled.web_url }} />
            ) : (
              group.bucket !== "todo" && <TriageStateChip state={rollupState(group.bucket)} />
            )}
          </div>
          {group.rationale_preview.trim() !== "" && (
            <p className="mt-1.5 line-clamp-3 whitespace-pre-wrap text-sm text-muted">
              {stripUnsafeChars(group.rationale_preview)}
            </p>
          )}
          {/* The created-with-warning line, under the chip (same style as FindingCard): the
              issue exists on the forge, only its local link/cache did not settle. */}
          {justFiled?.warning && <p className="mt-1 text-xs text-muted">{justFiled.warning}</p>}
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          {/* Delegated mode: an absent handler hides its button, so a group with no open
              member (scope=open would settle nothing) shows no action, matching the old
              GroupDisposeControls that returned null when disabled. */}
          <TriageActions
            // Once this coordinate is filed — locally (justFiled, incl. the created-with-warning
            // case) — File issue is withdrawn so it never re-arms over a live forge issue; Mark
            // done and Dismiss stay available.
            onFile={!justFiled && canAct && newestOpen ? () => setFiling(true) : undefined}
            onMarkDone={canAct ? () => onDispose("done") : undefined}
            // Dismiss is offered under `mine` only: there is no cross-user Dismiss (dismissing
            // another user's recommendation is their judgment, PRD #1184). Under `all` the absent
            // handler makes TriageActions render no Dismiss button.
            onDismiss={!isAdmin && canAct ? (reason) => onDispose("dismissed", reason) : undefined}
          />
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            aria-expanded={expanded}
            aria-label={expanded ? "Collapse occurrences" : "Expand occurrences"}
            className="rounded-md p-1 text-faint transition-colors hover:bg-raised hover:text-fg"
          >
            {expanded ? <ChevronDownIcon /> : <ChevronRightIcon />}
          </button>
        </div>
      </div>

      {/* Filing lives on the ROW now (#68 Decision 3 / PRD #1183): the shared draft card,
          prefilled from the newest open occurrence, using the same getIssueDraft / fileIssue
          endpoints the run page uses. A successful create re-reads the backlog (the coordinate
          moves to the `filed` rung, so the group rollup may change) and closes the card. */}
      {filing && newestOpen && (
        <div className="border-t border-edge px-3 py-2.5">
          {/* When more than one run is open, the draft targets the newest and the rest stay
              open until the group is marked done — say so above the card (PRD #1183 M2). */}
          {openCount > 1 && (
            <p className="mb-2 text-xs text-muted">
              Prefilled from the newest of {openCount} open runs. The other {openCount - 1} stay
              open until you mark the group done.
            </p>
          )}
          <IssueDraftCard
            repos={repos}
            loadDraft={async () => {
              // Under `all` the draft is keyed by COORDINATE (the admin endpoint resolves the
              // newest open occurrence server-side, since the wire carries no run/rec id); under
              // `mine` it is keyed by the newest open occurrence's run/rec id. Both KEEP the
              // provenance line (Decision 8) — filing publishes a user's worker text, so the
              // reader must see whose text it is, even in the anonymized aggregate.
              const { draft } = isAdmin
                ? await api.getAdminJudgeIssueDraft(group.category, group.target)
                : await api.getIssueDraft(
                    (newestOpen as JudgeOccurrence).run_id,
                    (newestOpen as JudgeOccurrence).rec_id,
                  );
              return {
                title: draft.title,
                description: draft.description,
                labels: draft.labels,
                provenance: draft.provenance,
                defaultRepoId: draft.default_repo_id,
                defaultNote: draft.default_note,
              };
            }}
            onCreate={async (values) => {
              // The admin file resolves the coordinate's newest open occurrence AGAIN at file
              // time (a fresher review moves the link) and files into the ADMIN's own repo; the
              // owner file targets the resolved run/rec id. Both return {issue, warning?}.
              const res = isAdmin
                ? await api.adminFileJudgeIssue({
                    category: group.category,
                    target: group.target,
                    repoId: values.repoId,
                    title: values.title,
                    description: values.description,
                  })
                : await api.fileIssue(
                    (newestOpen as JudgeOccurrence).run_id,
                    (newestOpen as JudgeOccurrence).rec_id,
                    {
                      repo_id: values.repoId,
                      title: values.title,
                      description: values.description,
                    },
                  );
              // Record the created issue + any warning locally so the row reflects the filing
              // even when the refetch below does not settle the link (created-with-warning).
              setJustFiled({ iid: res.issue.iid, web_url: res.issue.web_url, warning: res.warning ?? "" });
              setFiling(false);
              onFiled();
            }}
            onCancel={() => setFiling(false)}
          />
        </div>
      )}

      {expanded && (
        <div className="space-y-2 border-t border-edge px-3 py-2.5">
          {/* The newest open occurrence's FULL rationale, above the occurrence list (PRD #1183
              M2). It replaces the clamped preview once fetchReview lands; until then, on a
              fetch error, or when fetchReview is omitted (PRD #1184 admin scope), the clamped
              preview shows. Both render UNTRUSTED judge text through the hardened Markdown /
              stripUnsafeChars path the run page uses. */}
          {rationaleMd !== null ? (
            <div className="judge-prose">
              <Markdown content={stripUnsafeChars(rationaleMd)} />
            </div>
          ) : (
            group.rationale_preview.trim() !== "" && (
              <p className="line-clamp-3 whitespace-pre-wrap text-sm text-muted">
                {stripUnsafeChars(group.rationale_preview)}
              </p>
            )
          )}
          <ul className="space-y-2">
            {isAdmin
              ? (group.occurrences as JudgeAdminOccurrence[]).map((occ, i) => (
                  // No stable id under `all` (attribution hidden — no run/rec id), so the index
                  // keys it; the list is read-only and never reordered within a render.
                  <AdminOccurrenceRow key={i} occ={occ} />
                ))
              : (group.occurrences as JudgeOccurrence[]).map((occ) => (
                  <OccurrenceRow key={`${occ.run_id} ${occ.rec_id}`} occ={occ} />
                ))}
          </ul>
        </div>
      )}
    </li>
  );
}

// rollupState maps a group's rollup BUCKET onto the normalised TriageState the shared
// TriageStateChip takes. A group rollup is never "all" (that is a filter, not a member
// state) and the "todo" rollup is not rendered as a chip (the caller guards on it), but the
// switch is total so the type stays exhaustive. Occurrence chips use triageCopy's judgeState
// adapter instead, which also carries set_via / filed_issue.
function rollupState(bucket: JudgeBacklogBucket): TriageState {
  switch (bucket) {
    case "filed":
      return "filed";
    case "done":
      return "done";
    case "dismissed":
      return "dismissed";
    default:
      return "to_triage";
  }
}

// OccurrenceRow is one run's instance inside the expander: the run link, its verdict, and the
// per-run triage state chip. It is read-only now — filing moved to the group's action row, so
// the per-occurrence File issue button is gone (PRD #1183). run_title is UNTRUSTED (the run's
// issue_title) — rendered as escaped React text.
function OccurrenceRow({ occ }: { occ: JudgeOccurrence }) {
  return (
    <li className="rounded-md border border-edge bg-surface/60 px-2.5 py-2">
      <div className="flex flex-wrap items-center gap-2">
        <Link
          to={`/runs/${occ.run_id}`}
          className="min-w-0 max-w-full truncate text-sm font-medium text-fg underline-offset-2 hover:underline"
        >
          {stripUnsafeChars(occ.run_title) || "Untitled run"}
        </Link>
        <OccurrenceVerdictBadge verdict={occ.verdict} />
        <TriageStateChip {...judgeState(occ)} />
      </div>
    </li>
  );
}

// AdminOccurrenceRow is one run's instance in the admin `all` scope (PRD #1184 M4): the
// attribution-hidden twin of OccurrenceRow. The admin occurrence carries NO run_id/run_title —
// naming a run is attribution the aggregate hides — so there is NO link and NO title: it reads
// "A run, judged <relative time>" with the same verdict + state chips beside it. The state chip
// renders "Done by an admin" for a set_via='admin' occurrence, the cross-user done's provenance.
function AdminOccurrenceRow({ occ }: { occ: JudgeAdminOccurrence }) {
  return (
    <li className="rounded-md border border-edge bg-surface/60 px-2.5 py-2">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-fg" title={occ.judged_at ? new Date(occ.judged_at).toLocaleString() : undefined}>
          A run{occ.judged_at ? `, judged ${relativeTime(occ.judged_at)}` : ""}
        </span>
        <OccurrenceVerdictBadge verdict={occ.verdict} />
        <TriageStateChip {...judgeState(occ)} />
      </div>
    </li>
  );
}

// relativeTime renders an ISO instant as a coarse "time ago" (mirrors ActivityFeed's private
// helper). The absolute value lives in the row's title attribute. An unparseable value yields
// "" — the caller then shows the bare "A run".
function relativeTime(iso: string, now: number = Date.now()): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "";
  const s = Math.max(0, Math.floor((now - t) / 1000));
  if (s < 60) return "just now";
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

// OccurrenceVerdictBadge renders the run's judge verdict in the ONE grammar the product
// uses for that fact: `⚖ issues · 2`, exactly as /runs renders it (PRD #98 review N8).
//
// It previously rendered `<Badge>{verdictLabel(occ.verdict)}</Badge>` → "Issues found",
// which is a SECOND grammar for the same fact on a different screen. Collapsing the mock's
// two grammars into one was a load-bearing correction earlier in this PRD, and that is
// precisely the regression this reintroduced.
//
// It reuses judgeBadge() rather than re-deriving the label, which is why JudgeBadgeable is a
// structural subset (`{judge_verdict, judge_todo_count}`) instead of taking RunListItem —
// the type was made that shape SO this page could pass an occurrence-shaped object.
//
// judge_todo_count is 0 here on purpose. M4's badge counts a RUN's open recommendations, and
// an occurrence is a single coordinate, not a run — synthesising a count would state a
// number this DTO does not carry. judgeBadge drops the count entirely at 0, so the label is
// the bare `⚖ issues`: the same grammar, minus a claim we cannot make.
function OccurrenceVerdictBadge({ verdict }: { verdict: ReviewVerdict }) {
  const badge = judgeBadge({ judge_verdict: verdict, judge_todo_count: 0 });
  // Unreachable while the DTO types verdict as a non-null enum; judgeBadge returns null only
  // for an unjudged run, and an occurrence exists because a review produced it.
  if (!badge) return null;
  // THE TITLE IS OVERRIDDEN, and the label deliberately is not (PRD #98 review N-b).
  //
  // One glyph, two inference rules: on /runs the count is always rendered when > 0, so a
  // bare `⚖ issues` there genuinely means "nothing left to triage" (M4 behaviour (c)). Here
  // it means "no count is carried" — and these badges sit on rows that are by construction
  // still open. The two render byte-identically, so a reader who learned the grammar on
  // /runs would parse this as the opposite of the truth.
  //
  // The fix goes in the title rather than the label because the shared visual grammar is
  // what the fable review fought for, and splitting it again to disambiguate would trade one
  // false inference for the two-grammars problem N8 just removed. The distinguishing claim
  // belongs where the claim actually lives.
  return (
    <Badge tone={badge.tone} title={`This run's judge verdict: ${verdict}. Triage state is the chip beside it.`}>
      {badge.label}
    </Badge>
  );
}
