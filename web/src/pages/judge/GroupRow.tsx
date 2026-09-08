import { useState } from "react";
import { Link } from "react-router-dom";
import {
  api,
  type JudgeBacklogBucket,
  type JudgeOccurrence,
  type JudgeRecommendationGroup,
  type Repo,
} from "../../lib/api";
import { recommendationLabel } from "../../lib/judge";
import { seenInRunsLabel } from "../../lib/judgeBacklog";
import { stripUnsafeChars } from "../../lib/safeText";
import { judgeBadge } from "../../lib/judgeBadge";
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
// OPEN occurrence — the first occurrence with bucket "todo", which the backlog grouper
// delivers newest-first (rv.updated_at DESC). The per-occurrence File issue button is gone:
// the expander is now for reading (run link · verdict · state chip), not acting.
export function GroupRow({
  group,
  selected,
  onToggleSelect,
  onDispose,
  repos,
  onFiled,
}: {
  group: JudgeRecommendationGroup;
  selected: boolean;
  onToggleSelect: () => void;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
  repos: Repo[];
  onFiled: () => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const [filing, setFiling] = useState(false);
  const openCount = group.open_count;
  // The group's newest OPEN occurrence: the first occurrence bucketed "todo" in the grouper's
  // wire order, which the backlog query sorts rv.updated_at DESC, so it is the newest review.
  const newestOpen = group.occurrences.find((o) => o.bucket === "todo");
  const canAct = openCount > 0;

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
            <span className="text-xs text-faint">{seenInRunsLabel(group.run_count)}</span>
            {group.bucket !== "todo" && <TriageStateChip state={rollupState(group.bucket)} />}
            {openCount > 0 && <span className="text-xs text-faint">{openCount} open</span>}
          </div>
          {group.rationale_preview.trim() !== "" && (
            <p className="mt-1.5 line-clamp-3 whitespace-pre-wrap text-sm text-muted">
              {stripUnsafeChars(group.rationale_preview)}
            </p>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          {/* Delegated mode: an absent handler hides its button, so a group with no open
              member (scope=open would settle nothing) shows no action, matching the old
              GroupDisposeControls that returned null when disabled. */}
          <TriageActions
            onFile={canAct && newestOpen ? () => setFiling(true) : undefined}
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
              await api.fileIssue(newestOpen.run_id, newestOpen.rec_id, {
                repo_id: values.repoId,
                title: values.title,
                description: values.description,
              });
              setFiling(false);
              onFiled();
            }}
            onCancel={() => setFiling(false)}
          />
        </div>
      )}

      {expanded && (
        <ul className="space-y-2 border-t border-edge px-3 py-2.5">
          {group.occurrences.map((occ) => (
            <OccurrenceRow key={`${occ.run_id} ${occ.rec_id}`} occ={occ} />
          ))}
        </ul>
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
