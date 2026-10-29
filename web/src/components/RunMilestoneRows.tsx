import type { ReactNode } from "react";
import type { Run, RunActivity } from "../lib/api";
import { stripUnsafeChars } from "../lib/safeText";
import { useNow } from "../lib/useNow";
import { activityAge } from "../lib/runActivity";
import { effectiveMilestoneAgents, firstInProgressMilestoneId, uniqueLiveMatchMilestoneId } from "../lib/runBadge";
import { cx } from "./ui";
// MilestoneMark is the per-row done / in-progress / left indicator. Kept tiny and
// aria-labelled so the state is legible to assistive tech, not carried by colour alone.
function MilestoneMark({ state }: { state: "done" | "in_progress" | "left" }) {
  if (state === "done") return <span className="text-ok" aria-label="reported complete">✓</span>;
  if (state === "in_progress") return <span className="text-info" aria-label="in progress">◐</span>;
  return <span className="text-faint" aria-label="not started">○</span>;
}

// MilestoneNowStrip is the "now" line (PRD #1064 M3, D3/D5): a left-bordered strip that
// names the lane acting RIGHT NOW under the in-progress milestone (or unattached under
// the header when a milestone run has activity but nothing declared). Three variants:
//   - active   — a milestone is in progress. The pulsing green (ok) border + dot are
//                reserved for a strip backed by a REAL live frame (`live !== null`): it
//                then shows the role, its task, its last tool, and a client-side age (R7:
//                a stalled lane reads its real age, not a lie). A declared-owner-only
//                strip (idle owner, `live === null`) renders QUIET instead — faint border,
//                muted accent, a static faint dot with no pulse — so it never reads as
//                "working now" (PRD #1353 M1 honesty fix).
//   - waiting  — the run is parked on a self-resuming hold (limit_wait/pool_wait, or the
//                transient-recovery park recovery_wait, issue #1197): a warn border +
//                a waiting reason and age, so the strip does not pretend the lane
//                is working while it is blocked.
//   - unattached — activity exists but no milestone is declared in progress: a faint
//                border, sitting directly under the header. Always backed by a live frame.
//
// 🔴 ALL display fields are folded through stripUnsafeChars here (defense-in-depth): the
// server caps/strips only detail and agent_label, so agent/role and tool arrive UNSANITIZED
// on the wire — an auditor note. In the PRD #1224 per-milestone-attribution path (M5) the
// role/label are the DECLARED agent/agent_label (equally untrusted), so the same fold covers
// them. The pulsing dot (present only for a live or waiting strip) uses animate-pulse, which
// index.css neutralizes under prefers-reduced-motion.
//
// PRD #1224 M5 (D3): `live` carries the current tool/detail/age. It is non-null for TODAY's
// activity-driven strip (the D8-fallback path) AND for the single unique-match milestone in
// the attributed path; it is null for a declared-only strip (a sibling attribution, or every
// strip when a repeated role makes the live join ambiguous), which then shows role + label
// only — no tool line, no age. PRD #1353 M1: the dot/border/accent now key on whether a real
// live frame is present (`live !== null`), NOT on `variant` alone — an idle declared owner
// renders quiet (faint, no pulse). The `waiting` styling stays keyed on status.
function MilestoneNowStrip({
  role,
  label,
  live,
  status,
  now,
  variant,
}: {
  role: string;
  label: string;
  live: { tool: string; detail: string; at: string } | null;
  status: string;
  now: number;
  variant: "active" | "unattached";
}) {
  const waiting =
    status === "limit_wait" || status === "pool_wait" || status === "recovery_wait";
  const waitingLabel = status === "recovery_wait" ? "waiting to recover" : "waiting on rate limit";
  const safeRole = stripUnsafeChars(role);
  const safeLabel = stripUnsafeChars(label);
  // Live tool/age only when the caller passed a live frame; a declared-only strip shows the
  // role + label alone (age stays "" so neither the "X ago" token nor a waiting-suffix age
  // renders).
  const age = live ? activityAge(live.at, now) : "";
  const tool = live ? stripUnsafeChars(live.tool) : "";
  const detail = live ? stripUnsafeChars(live.detail) : "";
  const toolText = tool ? (detail ? `${tool} ${detail}` : tool) : detail;
  // The pulse + green styling is reserved for a strip backed by a REAL live frame (M1
  // honesty fix, PRD #1353): a declared-owner-only strip (idle owner, live === null) reads
  // QUIET — faint border, muted accent, faint static dot — so it never pretends to be
  // "working now". `waiting` is orthogonal and keeps its warn styling unchanged. The
  // unattached variant is always live (its caller passes a live frame), so it keeps its
  // faint border but a live green dot as before.
  const isLive = live !== null;
  const border = waiting
    ? "border-warning"
    : !isLive
      ? "border-faint"
      : variant === "unattached"
        ? "border-faint"
        : "border-ok/50";
  const accent = waiting ? "text-warning" : isLive ? "text-ok" : "text-muted";
  const dotBg = waiting ? "bg-warning" : isLive ? "bg-ok" : "bg-faint";
  const dotPulse = isLive;
  return (
    <div className={cx("mt-1.5 flex items-center gap-2 border-l-2 py-1 pl-3 text-xs", border)}>
      <span aria-hidden className={cx("h-1.5 w-1.5 shrink-0 rounded-full", dotBg, dotPulse && "animate-pulse")} />
      <span className={cx("shrink-0 font-medium", accent)}>{!isLive && <span>Assigned owner: </span>}<span>{safeRole}</span></span>
      {safeLabel && <span className="min-w-0 flex-1 truncate italic text-muted">{!isLive && <span> · </span>}<span>{safeLabel}</span></span>}
      {toolText && <span className="min-w-0 shrink truncate font-mono text-faint">{toolText}</span>}
      <span className="ml-auto shrink-0 whitespace-nowrap font-mono tabular-nums text-faint">
        {waiting ? `${waitingLabel}${age ? ` · ${age}` : ""}` : age ? `${age} ago` : ""}
      </span>
    </div>
  );
}

// Frozen milestone rows for the unified run progress card. Keep the existing attribution
// precedence: live lanes, uniquely matched declared owners, then the activity fallback.
// Every display field is untrusted plain text. Completion is reported, not verified.
export function MilestoneRows({ run, activity = null, activeContent }: { run: Pick<Run, "status" | "milestones" | "milestones_completed" | "milestones_in_progress" | "milestones_agents" | "milestones_live" | "progress"> & Partial<Pick<Run, "health">>; activity?: RunActivity | null; activeContent?: ReactNode }) {
  // useNow ages the "now" line's client-side elapsed token; called before the early
  // return so the hook order is stable (rules of hooks). A slow cadence is enough — the
  // strip shows a coarse "40s"/"6m" token, not a live countdown.
  const now = useNow(30_000);
  const milestones = run.milestones ?? [];
  if (milestones.length === 0) return null;
  const terminal = ["completed", "failed", "cancelled"].includes(run.status);
  const accountWait = run.health === "waiting_account_cooldown";
  if (terminal || run.status === "paused" || accountWait) activity = null;
  const completed = new Set(run.milestones_completed ?? []);
  const inProgress = new Set(run.milestones_in_progress ?? []);
  // Fallback activity attaches to the first in-progress milestone in frozen order.
  const firstInProgress = firstInProgressMilestoneId({...run, milestones_in_progress: (run.milestones_in_progress ?? []).filter(id => !completed.has(id))});
  // PRD #1224 M5 (D8): "effective attribution" = milestones_agents entries whose milestone
  // is CURRENTLY in progress, in frozen order. ONLY a non-empty list activates per-milestone
  // rendering; null / [] / stale-only fall back to the pre-#1224 render below (the M4
  // baseline guards that path). We branch on this LENGTH, never on milestones_agents != null.
  const effective = terminal ? [] : effectiveMilestoneAgents(run);
  const attributed = effective.length > 0;
  const effectiveById = new Map(effective.map((e) => [e.id, e]));
  // D3: live tool/age enriches a strip ONLY when EXACTLY ONE effective attribution's declared
  // agent byte-matches the live activity agent; zero or 2+ matches (a repeated role) suppress
  // live on ALL strips — every strip then shows declared role + label only.
  const uniqueMatchId = activity ? uniqueLiveMatchMilestoneId(effective, activity.agent) : null;
  // PRD #1353 M4: the server-derived per-in-progress-milestone LIVE LANES (each lane a
  // subagent working that milestone RIGHT NOW). When ANY milestone has a live lane, this is
  // the AUTHORITATIVE live display and supersedes the single current_activity now-line: the
  // declared owner renders QUIET (a non-pulsing "assigned to" line) and one green+pulsing
  // strip renders per lane, keyed by agent_instance so distinct subagents (reviewer×2) show
  // as distinct lines. We branch on PRESENCE of a lane (D5 back-compat), never on a nil test —
  // null / [] / all-empty-lanes ALL fall through to the pre-#1353 render below, byte-for-byte.
  const lanes = terminal || run.status === "paused" || accountWait ? [] : run.milestones_live ?? [];
  const milestonesLiveById = new Map(lanes.map((e) => [e.milestone_id, e.lanes]));
  const hasLanes = lanes.some((e) => inProgress.has(e.milestone_id) && !completed.has(e.milestone_id) && e.lanes.length > 0);
  return (
    <div className="min-w-0">
      {/* Unattached strip: a milestone run with activity but nothing declared in progress
          shows the "now" line directly under the header (D5's "nothing declared" variant).
          ONLY in the D8-fallback (unattributed) path — in the attributed path every strip
          hangs off its own milestone row, so no unattached strip renders (and effective is
          non-empty only when some milestone is in progress, so firstInProgress is set). */}
      {!hasLanes && !attributed && !firstInProgress && activity && (
        <MilestoneNowStrip
          role={activity.agent}
          label={activity.agent_label}
          live={{ tool: activity.tool, detail: activity.detail, at: activity.at }}
          status={run.status}
          now={now}
          variant="unattached"
        />
      )}
      <ul className="mt-2 space-y-1.5">
        {milestones.map((m) => {
          const state = completed.has(m.id) ? "done" : inProgress.has(m.id) ? "in_progress" : "left";
          const attribution = effectiveById.get(m.id);
          // PRD #1353 M4 (tui-ux dedup): this milestone's live lanes, and whether the DECLARED
          // owner is itself one of them (the common case — the dispatched owner is the one
          // working). When it is, the quiet owner strip would DUPLICATE the owner's live lane
          // strip (same role + label, once quiet + once live), so it is suppressed and the lane
          // strip carries it. An IDLE owner (agent not among the lanes) still gets its quiet
          // "assigned to" strip above the lanes.
          const laneList = milestonesLiveById.get(m.id) ?? [];
          const ownerIsLive = attribution ? laneList.some((l) => l.agent === attribution.agent) : false;
          return (
            <li key={m.id} className={cx("text-sm", !terminal && m.id === run.progress?.active_milestone_id && "rounded-md bg-brand/5 px-3 py-2")}>
              <div className="flex items-center gap-2">
                <MilestoneMark state={state} />
                <span
                  title={stripUnsafeChars(m.title)}
                  className={cx(
                    "min-w-0 truncate",
                    state === "done" ? "text-muted line-through" : state === "in_progress" ? "text-fg" : "text-faint",
                  )}
                >
                  <span className="mr-2 font-mono text-xs text-faint">{stripUnsafeChars(m.id)}</span>{stripUnsafeChars(m.title)}
                </span>
              </div>
              {m.id === run.progress?.active_milestone_id && activeContent}
              {hasLanes ? (
                // PRD #1353 M4 LIVE-LANES path (the authoritative live display). For each
                // in-progress milestone: the DECLARED owner renders QUIET (live=null → M1's
                // non-pulsing "assigned to" line) ONLY when its agent is NOT already one of the
                // lanes — otherwise the owner is live and its lane strip carries it, so the quiet
                // strip is suppressed to avoid drawing the same role + label twice (tui-ux dedup).
                // ONE live green+pulsing strip renders per lane, keyed by agent_instance. Neither
                // the D8-fallback/unattached strip nor uniqueMatchId is consulted here — lanes
                // supersede the current_activity now-line.
                state === "in_progress" && (
                  <>
                    {attribution && !ownerIsLive && (
                      <MilestoneNowStrip
                        role={attribution.agent}
                        label={attribution.agent_label}
                        live={null}
                        status={run.status}
                        now={now}
                        variant="active"
                      />
                    )}
                    {laneList.map((lane) => (
                      <MilestoneNowStrip
                        key={lane.agent_instance}
                        role={lane.agent}
                        label={lane.agent_label}
                        live={{ tool: lane.tool, detail: lane.detail, at: lane.at }}
                        status={run.status}
                        now={now}
                        variant="active"
                      />
                    ))}
                  </>
                )
              ) : attributed ? (
                // PRD #1224 M5 attributed path (D8/D3/D9): every in-progress milestone with an
                // effective attribution shows its DECLARED role + label; live tool/age is added
                // ONLY to the single unique-match milestone (D3). The ◐ mark above is untouched
                // (D9 — attribution is strictly additive).
                attribution && (
                  <MilestoneNowStrip
                    role={attribution.agent}
                    label={attribution.agent_label}
                    live={
                      m.id === uniqueMatchId && activity
                        ? { tool: activity.tool, detail: activity.detail, at: activity.at }
                        : null
                    }
                    status={run.status}
                    now={now}
                    variant="active"
                  />
                )
              ) : (
                // D8-fallback (unattributed): the "now" line sits under the FIRST in-progress
                // row only (D4), sourced from the activity prop — structurally today's render.
                activity &&
                m.id === firstInProgress && (
                  <MilestoneNowStrip
                    role={activity.agent}
                    label={activity.agent_label}
                    live={{ tool: activity.tool, detail: activity.detail, at: activity.at }}
                    status={run.status}
                    now={now}
                    variant="active"
                  />
                )
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
