import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  api,
  type JudgeBacklogBucket,
  type JudgeOccurrence,
  type JudgeRecommendationGroup,
  type PendingJudge,
  type Repo,
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
// api.getRunReview; PRD #1184's admin scope has no run_id to fetch with and omits it, and the
// expander then shows only the clamped preview.
type FetchReview = (
  runId: string,
) => Promise<{ review: RunReview | null; pending_judge: PendingJudge | null }>;

// newestOpenOccurrence picks the group's newest OPEN member — the coordinate the File-issue
// draft targets. Among the bucket "todo" occurrences it takes the largest judged_at (RFC3339
// sorts lexicographically = chronologically); a member missing judged_at never wins over one
// carrying a later value, and with none present it falls back to wire order (the backlog query
// delivers rv.updated_at DESC, so the first todo is already the newest).
function newestOpenOccurrence(group: JudgeRecommendationGroup): JudgeOccurrence | undefined {
  return group.occurrences
    .filter((o) => o.bucket === "todo")
    .reduce<JudgeOccurrence | undefined>((best, o) => {
      if (!best) return o;
      if (o.judged_at && (!best.judged_at || o.judged_at > best.judged_at)) return o;
      return best;
    }, undefined);
}

export function GroupRow({
  group,
  selected,
  onToggleSelect,
  onDispose,
  repos,
  onFiled,
  fetchReview,
}: {
  group: JudgeRecommendationGroup;
  selected: boolean;
  onToggleSelect: () => void;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
  repos: Repo[];
  onFiled: () => void;
  fetchReview?: FetchReview;
}) {
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
  const newestOpen = newestOpenOccurrence(group);
  const newestOpenRunId = newestOpen?.run_id;
  const canAct = openCount > 0;

  // The newest open occurrence's FULL rationale, fetched ONCE on first expand via fetchReview
  // and cached here per group (this component's key is the coordinate, so the cache survives
  // an onFiled reload). Until it lands — and on error, or when fetchReview is omitted — the
  // expander shows the clamped preview instead. null means "not loaded".
  const [rationaleMd, setRationaleMd] = useState<string | null>(null);
  const rationaleFetched = useRef(false);

  useEffect(() => {
    // Only after the user expands, only once, and only when the capability and an open
    // occurrence to fetch it from are both present.
    if (!expanded || rationaleFetched.current || !fetchReview || !newestOpenRunId) return;
    rationaleFetched.current = true;
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
                (PRD #1183 M2): "N open of M runs", or "M runs, all settled" once none remain. */}
            <span className="text-xs text-faint">{openOfRunsLabel(group.open_count, group.run_count)}</span>
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
            onDismiss={canAct ? (reason) => onDispose("dismissed", reason) : undefined}
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
              const { draft } = await api.getIssueDraft(newestOpen.run_id, newestOpen.rec_id);
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
              const res = await api.fileIssue(newestOpen.run_id, newestOpen.rec_id, {
                repo_id: values.repoId,
                title: values.title,
                description: values.description,
              });
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
            {group.occurrences.map((occ) => (
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
        <OccurrenceVerdictBadge occ={occ} />
        <TriageStateChip {...judgeState(occ)} />
      </div>
    </li>
  );
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
function OccurrenceVerdictBadge({ occ }: { occ: JudgeOccurrence }) {
  const badge = judgeBadge({ judge_verdict: occ.verdict, judge_todo_count: 0 });
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
    <Badge tone={badge.tone} title={`This run's judge verdict: ${occ.verdict}. Triage state is the chip beside it.`}>
      {badge.label}
    </Badge>
  );
}
