// One run row of the Overview's run list, shared by both Overview pages (PRD #3878). It is
// the Overview's own anatomy, deliberately NOT RunsList's RunRow: that one adds duration,
// tokens, cost and Expedite, and its "now" line lacks the held-status check below, so a
// parked run would show a stale, pulsing activity line.

import { Link } from "react-router-dom";
import { isTerminalRun, type RunSummaryItem } from "../lib/api";
import {
  effectiveRunStatus,
  firstInProgressMilestoneId,
  isAccountCooldownWaiting,
  milestoneBadge,
  milestoneBadgeText,
  mrChipState,
} from "../lib/runBadge";
import { activityAge } from "../lib/runActivity";
import { mrAbbrev } from "../lib/forgeNoun";
import { stripUnsafeChars } from "../lib/safeText";
import { maskEmail, maskRepoPath } from "../lib/demoMask";
import { MrChip } from "./MrChip";
import { RunIssueRef } from "./RunIssueRef";
import { RunHealthBadge } from "./RunHealthBadge";
import { RunProgressCell, progressShowsStall } from "./RunProgressCell";
import { Badge, StatusPill } from "./ui";

export function OverviewRunRow({
  run: r,
  now,
  demo,
  owner,
}: {
  run: RunSummaryItem;
  now: number;
  demo: boolean;
  // Another user's email on an admin's "Other users" list; masked in demo mode.
  owner?: string;
}) {
  const mrState = mrChipState(r.mr_state);
  // PRD #122: compact milestone progress; null for a non-milestone run.
  // PRD #1064 M3: the badge gains a ◐ suffix while a milestone is in progress.
  const ms = milestoneBadge(r);
  const msBadge = ms
    ? milestoneBadgeText(ms, (r.milestones_in_progress?.length ?? 0) > 0)
    : null;
  // PRD #1064 M3: the "now" line under the title uses current_activity from
  // the RunSummaryItem. Hidden when null, terminal, or held; untrusted fields
  // render escaped through stripUnsafeChars.
  const activity = r.current_activity;
  // Verified 2026-09-08: the API includes all non-terminal runs in its last
  // tool-use lookup, so holds can retain stale activity. The four waiting
  // states must not turn that history into a pulsing live-work indicator.
  const showNow =
    activity != null &&
    !isAccountCooldownWaiting(r) &&
    !isTerminalRun(r.status) &&
    r.status !== "paused" &&
    r.status !== "limit_wait" &&
    r.status !== "pool_wait" &&
    r.status !== "recovery_wait";
  const nowMilestone = firstInProgressMilestoneId(r);
  const ownerLabel = owner != null ? maskEmail(owner, demo) : null;
  return (
    // Issue #485 NB1: RunIssueRef renders a real forge <a>, which cannot nest
    // inside the row's navigational <Link> (also an <a> — invalid HTML). The
    // <li> is a `group relative` container, the run-details <Link> is a
    // stretched absolute overlay, and the content row sits in normal flow with
    // the interactive ref raised above the overlay (relative z-10). The
    // row-hover tint lives on the content layer via group-hover so the
    // transparent overlay never paints over the content.
    <li className="group relative">
      <Link
        to={`/runs/${r.id}`}
        // Issue #485 review FIX 2: name the link by the run's visible title so
        // multiple issue-less runs (all "Open run" otherwise) get distinct,
        // meaningful accessible names. issue_title is untrusted forge text, so it
        // stays sanitized (same stripUnsafeChars as the visible title).
        aria-label={`Open run${r.issue_iid != null ? ` for issue #${r.issue_iid}` : ""}${
          r.issue_title.trim() ? `: ${stripUnsafeChars(r.issue_title)}` : ""
        }`}
        className="absolute inset-0 rounded-md"
      />
      <div className="flex items-center gap-3 py-2.5 transition-colors group-hover:bg-raised/40">
        <div className="min-w-0 flex-1">
          {/* Issue #124: forge-supplied issue title, untrusted (see RunsList). */}
          <p className="truncate text-sm font-medium text-fg">{stripUnsafeChars(r.issue_title)}</p>
          {showNow && activity && (
            <p className="mt-0.5 flex items-center gap-1.5 text-xs text-faint">
              <span aria-hidden className="h-1.5 w-1.5 shrink-0 rounded-full bg-ok animate-pulse" />
              <span className="shrink-0 font-medium text-ok">{stripUnsafeChars(activity.agent)}</span>
              {nowMilestone && <span className="shrink-0 font-mono text-faint">{nowMilestone}</span>}
              <span className="min-w-0 truncate italic text-muted">
                {stripUnsafeChars(activity.agent_label || activity.detail || activity.tool)}
              </span>
              <span className="ml-auto shrink-0 whitespace-nowrap font-mono tabular-nums text-faint">
                {activityAge(activity.at, now)}
              </span>
            </p>
          )}
          <p className="flex flex-wrap items-center gap-x-1 text-xs text-faint">
            {/* PRD #1908: a repo-less job run shows only its kind chip. */}
            {r.repo_path && <>{maskRepoPath(r.repo_path, demo)}{" "}</>}
            <RunIssueRef
              issueIid={r.issue_iid}
              issueWebUrl={r.issue_web_url}
              kind={r.kind}
              forgeType={r.forge_type}
              raised
            />
            {r.mr_iid != null && (
              <MrChip
                variant="inline"
                label={`${mrAbbrev(r.forge_type)} `}
                forgeType={r.forge_type}
                mrIid={r.mr_iid}
                mrState={mrState}
                href={null}
                // Issue #485 review FIX 1: raised above the card overlay
                // (relative z-10) so its `title` (MR state) fires on hover.
                className="relative z-10 ml-2"
              />
            )}
            {ownerLabel != null && (
              <span className="relative z-10 ml-2 min-w-0 truncate" title={ownerLabel}>
                {ownerLabel}
              </span>
            )}
          </p>
        </div>
        {/* Issue #485 review FIX 1: raise the right-side badge cluster above the
            card's stretched-link overlay (relative z-10) so each badge's native
            `title` tooltip fires on hover — the transparent overlay otherwise
            intercepts the pointer and carries no title. These status badges are
            not click-to-navigate targets, so the trade is correct; the title/body
            text stays under the overlay and still opens the run. The wrapper keeps
            the row's gap-3 spacing so the layout is unchanged. */}
        <div className="relative z-10 flex items-center gap-3">
          {r.iteration_count > 0 && (
            <Badge tone="neutral" title="implement ⇄ review iterations">
              iter {r.iteration_count}
            </Badge>
          )}
          {/* PRD #122: compact milestone progress next to the iter badge; a
              non-milestone run keeps only the iter badge. PRD #265 M2: an
              unreported tracker shows M–/N, not a 0/N that reads as failure. */}
          {msBadge && (
            <Badge tone="info" title={msBadge.title}>
              {msBadge.label}
            </Badge>
          )}
          {/* PRD #2602: the server-derived waiting/stalled flags;
              renders nothing for a pre-feature server or a terminal run. This
              cluster does not wrap, so below sm the flags yield their space to the
              truncating title (the M badge still carries the count there); the
              Runs list rows wrap and keep it at every width. */}
          <span className="hidden sm:contents">
            <RunProgressCell run={r} now={now} />
          </span>
          {/* A stalled cell already says so (with the health word in its
              tooltip), so the health badge yields to it from sm up and stays
              the only stall signal on phones, where the cell is hidden. */}
          {progressShowsStall(r) ? (
            <span className="contents sm:hidden">
              <RunHealthBadge run={r} nowMs={now} />
            </span>
          ) : (
            <RunHealthBadge run={r} nowMs={now} />
          )}
          <StatusPill health={r.health} status={effectiveRunStatus(r)} progress={r.progress} recovery_wait_cause={r.recovery_wait_cause} recovery_retry_not_before={r.recovery_retry_not_before} recovery_wait_phase={r.recovery_wait_phase} />
        </div>
      </div>
    </li>
  );
}
