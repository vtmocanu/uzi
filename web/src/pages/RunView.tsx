// Live run view. The header carries the status pill plus a LIVE STAGE label
// derived from the newest message — a task-status-pill idea that maps the
// latest tool slug to a human stage: "Running command", "Reading files",
// "Making edits"…
// — so you can tell what the agent is doing without reading the feed. Terminal
// states get a hero banner: the MR link is the run's entire output and must
// not hide in chrome. The breadcrumb keeps PRD #12's in-app board / issue links.

import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  api,
  ApiError,
  isHttpsUrl,
  preferForgeUrl,
  isTerminalRun,
  type Repo,
  type Run,
  type RunMessage,
  type Worker,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { canToggleWaitOnLimit, formatCountdown, runWindowLabel } from "../lib/limitWait";
import { canToggleMrRework, effectiveMrRework } from "../lib/mrRework";
import { stripUnsafeChars } from "../lib/safeText";
import { useNow } from "../lib/useNow";
import {
  effectiveRunStatus,
  formatElapsed,
  healthFlagLabel,
  isStoppedRun,
  milestoneBadge,
  milestoneBadgeText,
  mrChipState,
  mrChipSuffix,
  mrChipTitle,
  shouldShowHealthFlag,
} from "../lib/runBadge";
import { forgeNounLower, mrAbbrev, mrRefSymbol } from "../lib/forgeNoun";
import { useRunStream } from "../lib/useRunStream";
import { deriveRunUsage } from "../lib/runUsage";
import { CIFixRunHeader } from "../components/CIFixRunHeader";
import { RunIssueRef } from "../components/RunIssueRef";
import { RunCredential } from "../components/RunCredential";
import { RunPriorityBadge } from "../components/RunPriorityBadge";
import { formatDuration } from "../components/RunEvent";
import { RunUsagePanel } from "../components/RunUsage";
import { ActivityFeed } from "../components/ActivityFeed";
import { SteerQueueCard } from "../components/SteerQueueCard";
import { QuestionPanel, UnreadableQuestion } from "../components/QuestionPanel";
import { deriveOpenQuestion } from "../lib/runQuestion";
import { Markdown } from "../components/Markdown";
import { Alert, Badge, Button, Card, PageHeader, Spinner, StatusPill, cx, type BadgeTone } from "../components/ui";
import { summaryCollapse } from "../lib/prefs";
import { ExternalLinkIcon } from "../components/icons";
import { PlanPanel, SeededPlanPanel } from "./runView/PlanPanel";
import { JudgePanel } from "./runView/JudgePanel";

// The Plan cluster moved to ./runView/PlanPanel; re-exported so external importers
// and tests stay byte-identical.
export { PlanPanel, SeededPlanPanel, derivePlanRevision } from "./runView/PlanPanel";

// The Judge cluster moved to ./runView/JudgePanel; re-exported so external importers
// (Judge.tsx's TriageSummary) and tests stay byte-identical.
export { JudgePanel, JUDGE_POLL_MAX_TRIES, TriageSummary } from "./runView/JudgePanel";

// stageForMessages: latest-message → human stage label (a tool-slug → stage
// map, adapted to uzi's message kinds).
const TOOL_STAGE: Record<string, string> = {
  Bash: "Running a command",
  Read: "Reading files",
  Glob: "Reading files",
  Grep: "Searching code",
  Edit: "Making edits",
  MultiEdit: "Making edits",
  Write: "Making edits",
  NotebookEdit: "Making edits",
  WebFetch: "Searching the web",
  WebSearch: "Searching the web",
  Task: "Delegating to a subagent",
};

export function stageForMessages(messages: RunMessage[]): string {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m.kind === "tool_result") return "Working";
    if (m.kind === "tool_use") {
      const name = (m.payload as { name?: string } | null)?.name ?? "";
      return TOOL_STAGE[name] ?? "Working";
    }
    if (m.kind === "thinking") return "Thinking";
    if (m.kind === "text") return "Writing";
    if (m.kind === "plan") return "Planning";
  }
  return "Starting up";
}

// LiveElapsed ticks a wall-clock timer for a still-running run.
function LiveElapsed({ since }: { since: string }) {
  const now = useNow(1000);
  const start = new Date(since).getTime();
  if (!Number.isFinite(start)) return null;
  return <span className="text-xs tabular-nums text-faint">{formatDuration(now - start)}</span>;
}

// HealthFlag renders the run-health warn chip next to the LIVE STAGE label (PRD #47
// Decision 10): `⚠ <label> · stuck for Xm — <reason>`. The run view is owner/admin
// only, so health_reason is always present here (no gating needed). It ticks the
// "stuck for Xm" coarsely (30s) since a stalled run emits no messages to force a
// re-render. The VISIBLE pill renders only for a flagged, flaggable run; the sr-only
// live region beside it is ALWAYS mounted (empty otherwise) — see the note at its
// render for why the announcement cannot ride the pill itself (#185).
/**
 * The stuck/health pill. Exported for the same reason RunHeading and RunCompletedLine are:
 * `RunView` needs routing, a live stream and a dozen API mocks to mount, and `health_reason`
 * renders only in this branch — so without this the strip below could not be asserted.
 */
export function HealthFlag({ run }: { run: Run }) {
  const now = useNow(30_000);
  const show = shouldShowHealthFlag(run.health, run.status);
  const since = run.health_since ? Date.parse(run.health_since) : NaN;
  const stuck = show && Number.isFinite(since) ? ` · stuck for ${formatElapsed(now - since)}` : "";
  // What the live region announces — the flag's short label, once, when it arrives.
  // DELIBERATELY NOT the ticking "stuck for Xm": that changes every 30s and would make
  // the region re-announce on every tick, the same countdown-in-a-live-region hostility
  // the park panel's role="status" note warns about. The stuck time and reason stay in
  // the visible pill, which no screen reader is listening to.
  const announce = show ? (healthFlagLabel(run.health) ?? "") : "";
  return (
    <>
      {show && (
        // A PLAIN (non-live) span: the sr-only region below owns the announcement.
        // role="status" used to sit HERE, and that was the bug the comment denied — a
        // live region mounted in the same tick as its first message is silent, because
        // assistive tech announces CHANGES to a region that already existed (#185;
        // Board.tsx S5). This pill mounts together with the flag, so it never announced.
        // The reason is shown inline below, so no title tooltip (it would be redundant).
        <span className="inline-flex items-center gap-1 rounded-full border border-warn/40 bg-warn/10 px-2 py-0.5 text-[11px] font-medium text-warn">
          ⚠ {healthFlagLabel(run.health)}
          {stuck}
          {/* Issue #124, TEXT channel. 96ed275a stripped this field where it reaches a `title`
              attribute (runBadge's descriptor) and missed it here, where it renders as body
              text — the mirror of f399ab26, which stripped upgrade_detail's TEXT and left its
              attribute for 4a739bff. Same field, opposite channel, one commit apart.
              (This sentence named 4a739bff until D1: that is the commit which fixed the
              ATTRIBUTE, so the mirror read backwards in the clause whose whole point is the
              mirror. Second time those two were transposed — see the note in
              WorkerUpgradeBadge.test.tsx — because they are one apart, both about
              upgrade_detail, and differ only in channel, which is the distinction being
              taught.) */}
          {run.health_reason && <span className="font-normal"> — {stripUnsafeChars(run.health_reason)}</span>}
        </span>
      )}
      {/* ALWAYS MOUNTED, empty until a flag arrives — mirrors RunView's parkAnnounce
          region and Board.tsx's S5 pattern. This is what actually announces the flag: a
          region that already exists narrates its next content change, where one mounted
          together with its first message stays silent. */}
      <span className="sr-only" role="status" aria-live="polite">
        {announce}
      </span>
    </>
  );
}

// MilestoneBadge is the compact `M{done}/{total}` pill (PRD #122), rendered beside the
// header's iteration badge on a milestone-structured run. It reads milestoneBadge, so it
// renders NOTHING for a run with no frozen milestone list — a pre-#122 run keeps only its
// iteration badge. Exported for a direct render test (the page needs a live stream to mount).
export function MilestoneBadge({ run }: { run: Run }) {
  const progress = milestoneBadge(run);
  if (!progress) return null;
  // PRD #265 M2: render "not reported" (M–/N) apart from a genuine 0/N via the shared
  // helper, so the header pill matches the board and a done run never reads as failed.
  const badge = milestoneBadgeText(progress);
  return (
    <Badge tone="info" title={badge.title}>
      {badge.label}
    </Badge>
  );
}

// MilestoneMark is the per-row done / in-progress / left indicator. Kept tiny and
// aria-labelled so the state is legible to assistive tech, not carried by colour alone.
function MilestoneMark({ state }: { state: "done" | "in_progress" | "left" }) {
  if (state === "done") return <span className="text-ok" aria-label="done">✓</span>;
  if (state === "in_progress") return <span className="text-info" aria-label="in progress">◐</span>;
  return <span className="text-faint" aria-label="not started">○</span>;
}

// MilestoneChecklist renders a milestone-structured run's progress (PRD #122): every
// approved milestone with a done / in-progress / left indicator, driven by the frozen
// list + the reported-complete and in-progress id sets.
//
// 🔴 The header says "reported complete", NEVER "verified" (PRD Decision 6): the worker
// REPORTS completion and nothing in uzi has verified it, so the copy must not imply it
// has. Titles are REPO/agent-authored UNTRUSTED text, rendered as PLAIN JSX through
// stripUnsafeChars — never <Markdown>, the same rule repo_agents follow. Renders nothing
// for a run with no frozen milestone list. Exported for a direct render test.
export function MilestoneChecklist({ run }: { run: Run }) {
  const milestones = run.milestones ?? [];
  if (milestones.length === 0) return null;
  const completed = new Set(run.milestones_completed ?? []);
  const inProgress = new Set(run.milestones_in_progress ?? []);
  // Single source of truth for the done count — milestoneBadge counts frozen members
  // present in the completed set (immune to duplicate ids), the same rule this list
  // renders from. Non-null here since the frozen list is non-empty (checked above).
  const progress = milestoneBadge(run);
  const doneCount = progress?.done ?? 0;
  // PRD #265 M2: a run that reported nothing shows "not reported" (–/N) instead of a
  // 0/N that reads as failure. The rows below already render every milestone as ○
  // (not-started) in that case, which is honest; only the header count changes.
  const reported = progress?.reported ?? false;
  return (
    <Card className="p-4">
      <div className="mb-3 flex items-center justify-between gap-2">
        <h2 className="text-sm font-semibold text-fg">Milestones (reported complete)</h2>
        <span
          className="font-mono text-xs tabular-nums text-faint"
          title={reported ? undefined : "No milestone completion reported for this run"}
        >
          {reported ? `${doneCount}/${milestones.length}` : `–/${milestones.length}`}
        </span>
      </div>
      <ul className="space-y-1.5">
        {milestones.map((m) => {
          const state = completed.has(m.id) ? "done" : inProgress.has(m.id) ? "in_progress" : "left";
          return (
            <li key={m.id} className="flex items-center gap-2 text-sm">
              <MilestoneMark state={state} />
              <span
                title={stripUnsafeChars(m.title)}
                className={cx(
                  "min-w-0 truncate",
                  state === "done" ? "text-muted line-through" : state === "in_progress" ? "text-fg" : "text-faint",
                )}
              >
                {stripUnsafeChars(m.title)}
              </span>
            </li>
          );
        })}
      </ul>
    </Card>
  );
}

// PRD #362 M4: one delta's kind → its badge tone, glyph and label. added is a green +,
// changed a blue ~, dropped a red − — so the three read apart by shape AND colour, not
// colour alone (Decision 6 wants them "visually distinct"). An unexpected kind (the server
// validates the enum on persist, Decision 6, so this should not happen) falls back to a
// neutral bullet rather than crashing the list.
const DELTA_KIND: Record<string, { tone: BadgeTone; glyph: string; label: string }> = {
  added: { tone: "ok", glyph: "+", label: "added" },
  changed: { tone: "info", glyph: "~", label: "changed" },
  dropped: { tone: "danger", glyph: "−", label: "dropped" },
};

/**
 * PRD #362 M4 (Decisions 2/6/9/10): the run-summary cards — the intent summary ("what this
 * run will implement"), the plan summary (labelled proposed/approved from run status,
 * Decision 2 — never regenerated), and the plan's deltas from the original ask (Decision 6).
 *
 * Rendered ONLY once a summary exists; until then this returns null and the issue-title
 * header (RunHeading) stands as the fallback (Decision 1's accepted consequence, and the
 * seeded/pre-approved intent-only shape of Decision 5). Live update rides the existing
 * useRunStream: M1 emits a run-updated WS frame on summary persist, so refreshRun re-reads
 * the DTO and this re-renders with no code here.
 *
 * The whole section is collapsible and the choice is remembered PER RUN for 7 days via
 * summaryCollapse (Decision 9); default expanded. All summary text is UNTRUSTED — model
 * output over an attacker-influenceable issue/PRD/plan. The intent and plan paragraphs
 * render through the shared hardened <Markdown> sink (issue #423, revising the "rendered as
 * text (web)" clause of Decision 10) — the same untrusted-LLM trust boundary the judge
 * summary_md already crosses: stripUnsafeChars runs BEFORE the parse (bidi/zero-width gone,
 * even inside fenced code) and there is NO rehype-raw, so raw HTML stays inert text. The
 * deltas stay escaped plain text through stripUnsafeChars: they render inline next to a
 * badge and <Markdown>'s block-level docs-prose <p> would break that tight layout. The rest
 * of Decision 10 holds — the runner is still tool-less (untrusted text drives no action) and
 * the CLI still routes every summary string through cellText (plain). report_md (a different
 * surface, issue #279) remains escaped plain text; see its own comment below.
 *
 * Exported for a direct render test: RunView itself needs routing, a live stream and a dozen
 * API mocks to mount, so the card states could not otherwise be asserted.
 */
export function RunSummary({ run }: { run: Run }) {
  // The persisted per-run collapse choice, read once on mount (default expanded).
  const [collapsed, setCollapsed] = useState(() => summaryCollapse.getCollapsed(run.id));

  const intent = typeof run.summary_intent === "string" ? run.summary_intent.trim() : "";
  const plan = typeof run.summary_plan === "string" ? run.summary_plan.trim() : "";
  // Decision 6 (tolerate-on-read): anything that is not an array is treated as no-deltas,
  // never a crash. The server already coerces malformed jsonb to null (M1); this is the
  // defensive second line for a hand-built or future-shaped value reaching the renderer.
  const deltas = Array.isArray(run.summary_deltas) ? run.summary_deltas : [];
  const hasIntent = intent !== "";
  const hasPlan = plan !== "";

  // Nothing to show yet — the issue-title header stands (Decision 1/5).
  if (!hasIntent && !hasPlan) return null;

  const toggle = () => {
    const next = !collapsed;
    setCollapsed(next);
    summaryCollapse.setCollapsed(run.id, next);
  };

  // Decision 2: label derived from run status, never regenerated. "Proposed" at the gate,
  // "Approved" once the run is past it (a plan summary present on any non-gate status).
  const planLabel = run.status === "awaiting_approval" ? "Proposed plan" : "Approved plan";

  return (
    <Card className="space-y-4 p-4">
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-sm font-semibold text-fg">Run summary</h2>
        <button
          type="button"
          onClick={toggle}
          aria-expanded={!collapsed}
          className="text-xs font-medium text-muted transition-colors hover:text-fg"
        >
          {collapsed ? "Expand" : "Collapse"}
        </button>
      </div>

      {!collapsed && (
        <div className="space-y-4">
          {hasIntent && (
            <section className="space-y-1">
              <h3 className="text-xs font-semibold uppercase tracking-wider text-faint">
                What this run will implement
              </h3>
              <div className="judge-prose">
                <Markdown content={intent} />
              </div>
            </section>
          )}

          {hasPlan && (
            <section className="space-y-2">
              <h3 className="text-xs font-semibold uppercase tracking-wider text-faint">{planLabel}</h3>
              <div className="judge-prose">
                <Markdown content={plan} />
              </div>

              {/* Decision 6: the plan's deltas from the original ask, or a plain
                  "no deviations" line when the plan matches it (empty array or null). */}
              {deltas.length === 0 ? (
                <p className="text-sm italic text-faint">No deviations — the plan matches the original ask</p>
              ) : (
                <ul className="space-y-1.5">
                  {deltas.map((d, i) => {
                    const kind = DELTA_KIND[d?.kind] ?? { tone: "neutral" as BadgeTone, glyph: "•", label: "note" };
                    return (
                      <li key={`${i}-${kind.label}`} className="flex items-start gap-2 text-sm">
                        <Badge tone={kind.tone} title={kind.label}>
                          <span aria-hidden="true">{kind.glyph}</span> {kind.label}
                        </Badge>
                        <span className="min-w-0 flex-1 whitespace-pre-wrap text-muted">
                          {stripUnsafeChars(typeof d?.text === "string" ? d.text : "")}
                        </span>
                      </li>
                    );
                  })}
                </ul>
              )}
            </section>
          )}
        </div>
      )}
    </Card>
  );
}

// capitalise fronts the window clause when it stands as its own sentence. The clause
// is built lower-case because its other use is mid-sentence ("sooner than the 5-hour
// window reopens …"), and one string serving both readings beats two near-identical
// ones that can drift apart.
function capitalise(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/**
 * PRD #35: the usage-limit surface — the resume countdown while a run is parked, and
 * the per-run opt-in for the NEXT limit. One component for both because they are one
 * decision from the user's side, and splitting them would put the toggle in two
 * places depending on status.
 *
 * 🔴 THE TOGGLE DOES NOT UN-PARK A RUN, and the label has to survive being read in a
 * hurry by someone whose run is stuck. Flipping it off while parked changes what
 * happens at the NEXT limit; this park keeps its clock, and the way out of it is
 * Stop. That is why the checkbox says "future" in its own words rather than relying
 * on the section heading, and why it does not move or disappear when the run parks.
 *
 * Weight scales with state on purpose: parked, it is a warn box carrying the one
 * fact the user came for; otherwise it is a single faint line, because a control for
 * a limit nobody has hit yet must not outrank the run.
 *
 * Exported for the same reason HealthFlag and RunCompletedLine are — RunView needs
 * routing, a live stream and a dozen API mocks to mount, so the countdown and the
 * inertness rule would otherwise only be reachable through the whole page.
 */
export function LimitWaitPanel({
  run,
  busy,
  canSteer = true,
  onToggle,
  onStop,
}: {
  run: Run;
  busy: boolean;
  // False for a NON-OWNER viewer. Mirrors PlanPanel/QuestionPanel (MR !149): POST is
  // user-scoped, so a non-owner admin — who can legitimately open this owner-or-admin
  // run view — must not be shown a Stop or a wait-on-limit toggle that 404s. Both
  // controls are HIDDEN (not greyed) and replaced by inert text, because a disabled
  // control still invites a click and then refuses it. Defaults true so an owner is
  // never gated by an absent prop.
  canSteer?: boolean;
  onToggle: (enabled: boolean) => void;
  // web-ux F1: the panel carries its OWN Stop, rather than pointing at the one in the
  // steer card 596px below it. The old copy's "Stop it if you would rather not wait"
  // was the faintest text on the page (4.56 contrast) and named a control the user
  // then had to hunt for past the whole activity feed.
  onStop: () => void;
}) {
  const parked = run.status === "limit_wait";
  // Only while parked: off the park there is no clock, and a 1s interval running
  // for the life of every run view is pure waste. 1s (not HealthFlag's 30s)
  // because the countdown drops to seconds in its last minute, which is exactly
  // when someone is watching it.
  const now = useNow(parked ? 1000 : null);

  // Terminal: no future limit to have an opinion about, and the server would no-op
  // the write anyway. Nothing renders at all — not a disabled checkbox, which would
  // only invite the question of why it is there.
  if (!canToggleWaitOnLimit(run.status)) return null;

  // Non-owner (#183): the setting is the owner's to change, so render its CURRENT state
  // as inert text rather than a disabled checkbox — the affordance-that-lies rule the
  // canSteer prop exists for, mirroring QuestionPanel's inert non-owner options.
  const toggle = canSteer ? (
    <label className="flex items-center gap-2 text-xs">
      <input
        type="checkbox"
        // h-4 w-4, matching Settings and every other checkbox in the app. This PRD
        // introduced the only h-3.5 in the codebase (web-ux nit).
        className="h-4 w-4 accent-brand"
        checked={run.wait_on_limit}
        disabled={busy}
        onChange={(e) => onToggle(e.target.checked)}
      />
      <span className={parked ? "text-fg" : "text-muted"}>
        Wait out future Anthropic usage limits on this run
      </span>
    </label>
  ) : (
    <span className={cx("text-xs", parked ? "text-fg" : "text-muted")}>
      {run.wait_on_limit
        ? "Waiting out future Anthropic usage limits on this run — only its owner can change this."
        : "Not waiting out future Anthropic usage limits on this run — only its owner can change this."}
    </span>
  );

  if (!parked) return <div className="px-1">{toggle}</div>;

  // 🔴 THE COUNTDOWN READS retry_not_before, NEVER limit_resets_at. They are
  // different instants and the gap is not an offset: retry_not_before carries
  // jitter, is clamped, is cross-checked against the owner's own rate-limit gauge,
  // and is pool-aware — a user with a second credential that still has headroom is
  // promoted EARLIER than the dead credential's window reopens. limit_resets_at is
  // context ("which window, and when does it roll over"), which is why it renders as
  // a separate clause rather than as the number the user is waiting on.
  const countdown = formatCountdown(run.retry_not_before, now);
  const windowName = runWindowLabel(run.rate_limit_type);
  const resetsMs = run.limit_resets_at ? Date.parse(run.limit_resets_at) : NaN;
  const retryMs = run.retry_not_before ? Date.parse(run.retry_not_before) : NaN;
  const hasReset = Number.isFinite(resetsMs);

  // web-ux F4: rendered plainly, the two numbers read as a contradiction — "resumes in
  // 1h 36m" beside a window that reopens 57 minutes LATER. The code is right and the
  // gap is the pool-aware stamp doing its job, but nothing on the panel said so, and a
  // user who spots it concludes one of the numbers is wrong.
  //
  // Detected from the VALUES, not assumed: the ordering can go either way. The stamp
  // starts at max(worker reset, gauge reset), so jitter and the cross-check push it
  // LATER, while an alternative credential with headroom pulls it EARLIER. Only the
  // earlier case is surprising, so only it gets the clause.
  //
  // The wording claims the RULE, not the cause: retry_not_before means "the earliest
  // moment this user could spend anything", which is exactly "as soon as one of your
  // tokens can pay for it". Naming a second credential specifically would be a guess —
  // the DTO carries no field saying which leg of the computation won.
  const resumesEarly = hasReset && Number.isFinite(retryMs) && retryMs < resetsMs;

  // Seconds on a multi-hour horizon are noise (web-ux nit): toLocaleString() renders
  // "01:37:11" for an instant that is only known to within a poll interval anyway.
  const resetText = hasReset
    ? new Date(resetsMs).toLocaleString(undefined, {
        dateStyle: "medium",
        timeStyle: "short",
      })
    : null;

  // Kept as SEPARATE clauses rather than one joined string: the window clause has to
  // sit inside the "sooner than …" sentence when the run resumes early, and the
  // attempt has to stay outside it. Joining them with " · " first put "attempt 2"
  // between "reopens <date>" and "because uzi resumes…", splitting one sentence
  // around an unrelated fact.
  const windowClause = windowName && resetText ? `the ${windowName} reopens ${resetText}` : windowName;
  // "attempt 1" is noise on a first park; the count only becomes information once the
  // run has been round more than once, which is also when the retry budget starts to
  // matter. The CAP is deliberately not on RunDTO (one server constant does not belong
  // on every row of a list response), so this is "attempt N", not "attempt N of M".
  const attemptClause = run.limit_wait_count > 1 ? `Attempt ${run.limit_wait_count}.` : null;

  return (
    <div className="rounded-xl border border-warn/40 bg-warn/10 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          {/* role="status" announces the PARK when this panel mounts — the heading
              below, not the countdown, which sits in a sibling <p> outside this live
              region and is deliberately NOT announced: a per-second countdown inside
              a live region would interrupt a screen reader every second, which is
              hostile. The park's arrival is separately narrated by ActivityFeed's own
              polite region when the limit_wait message lands.
              (This comment previously claimed the countdown was announced here. It
              was never true — the behaviour is correct and the comment was the bug,
              which is the kind that stops the next person from checking.) */}
          <p role="status" className="text-sm font-semibold text-warn">
            <span aria-hidden="true">⏸ </span>
            Paused on an Anthropic usage limit
          </p>
          <p className="mt-0.5 text-xs text-muted">
            {countdown ? (
              <>
                Resumes in <span className="tabular-nums text-fg">{countdown}</span>
                {resumesEarly && windowClause ? (
                  <> — sooner than {windowClause}, because uzi resumes as soon as any of your tokens can pay for it.</>
                ) : (
                  <>.{windowClause && ` ${capitalise(windowClause)}.`}</>
                )}
              </>
            ) : (
              // Past retry_not_before: the promotion pass runs on a ticker, so the
              // run is waiting on the next sweep rather than overdue. Counting up
              // into a negative would read as a fault where there is none.
              <>Resuming shortly.{windowClause && ` ${capitalise(windowClause)}.`}</>
            )}
            {attemptClause && ` ${attemptClause}`}
          </p>
          {/* text-muted, not text-faint (web-ux F1): at 4.56 this was the faintest
              text on the panel while being the only thing telling a stuck user that
              nothing is lost. The pointer to Stop is now the button beside it. */}
          <p className="mt-1.5 text-xs text-muted">
            Nothing is lost — the run keeps its branch and its history and picks up where it left off.
          </p>
        </div>
        {/* A ROW, not a column. As a column with items-end it wrapped under the prose
            at ordinary widths and left the Stop button stranded mid-panel with the
            checkbox orphaned beneath it — the two controls read as unrelated. Side by
            side they wrap as one unit and stay legible from 390px up. */}
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          {toggle}
          {/* Non-owner (#183): no live Stop — inert text stating who can, never a greyed
              button that 404s. Mirrors the PlanPanel/QuestionPanel non-owner branches. */}
          {canSteer ? (
            <Button variant="danger" size="sm" disabled={busy} onClick={onStop}>
              Stop run
            </Button>
          ) : (
            <span className="text-xs text-muted">Only the run's owner can stop it.</span>
          )}
        </div>
      </div>
    </div>
  );
}

/**
 * PRD #841: the per-run "auto-rework this MR's review comments" toggle. Mirrors the
 * LimitWaitPanel checkbox block, with two deliberate divergences (D2/D3):
 *
 * - Visibility is gated by canToggleMrRework(run) — an `issue` run whose MR is null or
 *   `opened` — NOT by a non-terminal status. The watcher acts AFTER the run completes,
 *   so the toggle stays live on a completed run whose MR is still open and disappears
 *   only once the MR merges/closes.
 * - A non-owner (canSteer=false) sees NOTHING (returns null), not the inert
 *   current-state text LimitWaitPanel shows: this is a post-completion preference with
 *   no "when does it resume" fact a viewer needs, so there is nothing to render inert.
 *
 * The checkbox reflects the EFFECTIVE value (run override ?? owner default ?? on); a
 * click sends the explicit boolean. The setting is tri-state (inherit/on/off), so when the
 * run carries an EXPLICIT override (mr_rework_enabled is true or false, not null) a "Reset
 * to default" button is shown that sends null, returning the run to live inheritance of the
 * owner default (PRD #841) — so every state is reachable from the web, not only the CLI
 * --clear. Exported like LimitWaitPanel so the gate + copy are testable without mounting
 * the whole page.
 */
export function MrReworkPanel({
  run,
  busy,
  canSteer = true,
  userDefault,
  onToggle,
}: {
  run: Run;
  busy: boolean;
  canSteer?: boolean;
  // UserSettings.mr_rework_enabled: the owner's global default (null = never overrode
  // the default-ON state). Only the run's own null-override falls through to it.
  userDefault: boolean | null;
  // null clears the run override back to inherit (the account default); true/false set it.
  onToggle: (enabled: boolean | null) => void;
}) {
  // Hidden for a non-owner and once the MR is merged/closed (or a non-issue run).
  if (!canSteer) return null;
  if (!canToggleMrRework(run)) return null;
  const effective = effectiveMrRework(run, userDefault);
  // An explicit per-run override (true/false) is distinct from inherit (null/undefined);
  // only then is there something to reset back to the account default.
  const overridden = run.mr_rework_enabled != null;
  return (
    <div className="flex items-center gap-3 px-1">
      <label className="flex items-center gap-2 text-xs">
        <input
          type="checkbox"
          className="h-4 w-4 accent-brand"
          checked={effective}
          disabled={busy}
          onChange={(e) => onToggle(e.target.checked)}
        />
        <span className="text-muted">Auto-rework this MR&apos;s review comments</span>
      </label>
      {overridden && (
        <button
          type="button"
          className="text-xs font-medium text-muted transition-colors hover:text-fg disabled:opacity-50"
          disabled={busy}
          onClick={() => onToggle(null)}
        >
          Reset to default
        </button>
      )}
    </div>
  );
}

/**
 * Issue #754: the pool-empty hold panel + its Resume-now control — the analogue of
 * LimitWaitPanel for `pool_wait`.
 *
 * An `auto`-lane run parks at `pool_wait` when the owner's Anthropic token pool is
 * genuinely EMPTY. It is NOT a usage-limit park, so — unlike LimitWaitPanel — it
 * shows NO reset countdown: there is no reset. It resumes automatically the instant
 * a token is opted into the pool; "Resume now" (POST /runs/{id}/resume-now) skips
 * that wait and moves the run to `queued`, after which the WS/refetch re-renders and
 * this panel unmounts.
 *
 * Self-contained busy + inline message rather than RunView's shared act()/busy,
 * because the 409 ("run is not waiting for a pooled token") case wants a gentle
 * inline note on the panel, distinct from the page-level actionErr banner: a token
 * pooled — or the run resumed elsewhere — between render and click is not a failure.
 *
 * Exported like LimitWaitPanel so the copy and the resume/409 handling are reachable
 * without mounting the whole page.
 */
export function PoolWaitPanel({
  run,
  canSteer = true,
  onResumed,
}: {
  run: Run;
  // False for a NON-OWNER viewer (mirrors LimitWaitPanel): resume is owner-scoped
  // (the server 404s a non-owner), so a non-owner admin who can open this run view
  // sees inert text rather than a button that 404s.
  canSteer?: boolean;
  // Re-read the run after a resume so the view reflects `queued` even if no WS frame
  // lands first (mirrors the expedite/wait-on-limit refetch pattern).
  onResumed: () => void | Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState("");

  // Only for a run actually parked on the pool. Every other status (including
  // terminal) renders nothing — there is no future-pool opt-in to offer.
  if (run.status !== "pool_wait") return null;

  const resume = async () => {
    setNote("");
    setBusy(true);
    try {
      await api.resumeRunNow(run.id);
      await onResumed();
    } catch (e) {
      // 409: the run is no longer at pool_wait — already resumed to `queued`, or a
      // token was pooled between render and click. Say so gently and re-sync rather
      // than surfacing it as a failure.
      if (e instanceof ApiError && e.status === 409) {
        setNote("This run is no longer waiting.");
        await onResumed();
      } else {
        setNote(errorMessage(e, "Could not resume the run."));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="rounded-xl border border-warn/40 bg-warn/10 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          {/* role="status" announces the hold when this panel mounts, mirroring
              LimitWaitPanel's heading. RunView's page-level parkAnnounce region also
              narrates pool_wait for a screen reader that arrives mid-stream. */}
          <p role="status" className="text-sm font-semibold text-warn">
            <span aria-hidden="true">⏸ </span>
            Waiting for a pooled token
          </p>
          <p className="mt-0.5 text-xs text-muted">
            This run is set to auto-select an Anthropic token, but the token pool is
            empty, so it is waiting. Add a token to the pool and it resumes
            automatically.
          </p>
          {/* No countdown, deliberately: a pool_wait hold has no reset window to
              count down to — resumption is event-driven (a token is pooled), not
              time-driven. */}
          <p className="mt-1.5 text-xs text-muted">
            Nothing is lost — the run keeps its branch and its history and picks up where it left off.
          </p>
          {/* Always mounted (sr-only when empty) so the 409 note is announced when it
              arrives — a region created in the same tick as its first content is
              typically silent to assistive tech (see RunView's parkAnnounce note). */}
          <p
            role="status"
            aria-live="polite"
            className={cx("text-xs font-medium text-warn", note ? "mt-1.5" : "sr-only")}
          >
            {note}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          {/* Non-owner: no live control — inert text, never a greyed button that 404s
              (mirrors LimitWaitPanel's non-owner Stop branch). */}
          {canSteer ? (
            <Button variant="secondary" size="sm" disabled={busy} onClick={resume}>
              Resume now
            </Button>
          ) : (
            <span className="text-xs text-muted">Only the run's owner can resume it.</span>
          )}
        </div>
      </div>
    </div>
  );
}

export function RunView() {
  const { id = "" } = useParams();
  const { run, messages, connected, error, submit, refreshRun, inputs, canSteer } = useRunStream(id);
  const [repoWebUrl, setRepoWebUrl] = useState<string | null>(null);
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [actionErr, setActionErr] = useState("");
  const [busy, setBusy] = useState(false);
  // PRD #841: the owner's global MR-rework default, for the per-run checkbox's effective
  // display when this run inherits (mr_rework_enabled null). Lives on UserSettings, not
  // the session User, so it is fetched here. Best-effort — a failed read leaves null,
  // which effectiveMrRework treats as the default-ON state.
  const [mrReworkDefault, setMrReworkDefault] = useState<boolean | null>(null);

  // Resolve the repo's web URL (for the MR link); the run itself does not carry
  // it. Best-effort — the MR iid is shown as text if the repo is not resolvable.
  useEffect(() => {
    if (!run) return;
    api
      .listRepos()
      .then(({ repos }: { repos: Repo[] }) => {
        const repo = repos.find((r) => r.id === run.repo_id);
        setRepoWebUrl(repo?.web_url ?? null);
      })
      .catch(() => setRepoWebUrl(null));
  }, [run]);

  // PRD #84 M4 4d: the workers list backs the plan-gate readiness summary — a required
  // capability is "met" when the run's ASSIGNED worker (run.worker_id) advertises it. Only
  // needed at the approval gate; best-effort (an empty list just renders every required cap
  // as unmet, which is the honest fail-closed reading). The Workers page owns the live poll;
  // this is a one-shot fetch when a run reaches the gate.
  useEffect(() => {
    if (run?.status !== "awaiting_approval") return;
    api
      .listWorkers()
      .then(({ workers }: { workers: Worker[] }) => setWorkers(workers))
      .catch(() => setWorkers([]));
  }, [run?.status]);

  // PRD #841: load the owner's MR-rework default only when the per-run checkbox will
  // actually show (owner viewing an issue run whose MR is still open) — the effective
  // display needs it only in the inherit case. Best-effort; a failed read stays null.
  const showMrRework = !!run && canSteer && canToggleMrRework(run);
  useEffect(() => {
    if (!showMrRework) return;
    api
      .getMySettings()
      .then(({ settings }) => setMrReworkDefault(settings.mr_rework_enabled ?? null))
      .catch(() => setMrReworkDefault(null));
  }, [showMrRework]);

  const act = async (fn: () => Promise<unknown>) => {
    setActionErr("");
    setBusy(true);
    try {
      await fn();
    } catch (e) {
      setActionErr(errorMessage(e, "Action failed"));
    } finally {
      setBusy(false);
    }
  };

  const stage = useMemo(
    () => (run?.status === "running" ? stageForMessages(messages) : null),
    [run?.status, messages],
  );

  // PRD #40: usage derived client-side from the stream (Decision 5) — a pure
  // reduction re-run as messages grow, so it folds in live (Decision 9) with no
  // accumulator. Feeds the usage panel + the per-phase finish lines in the feed.
  const usage = useMemo(() => deriveRunUsage(messages), [messages]);

  // PRD #88: the open clarification question, derived from the feed by seq exactly as
  // derivePlanRevision derives the gate state — there is NO DTO field for it (D-L), so
  // web, Slack and the CLI all read the same rule off the same messages.
  const openQuestion = useMemo(() => deriveOpenQuestion(messages), [messages]);

  // A parked run is announced to assistive tech. Measured in the browser: without this a
  // screen-reader user reading the feed gets NO signal that the run stopped and is now
  // waiting on them — it just goes quiet until the 24h deadline fails it.
  //
  // The region is rendered UNCONDITIONALLY below and only its CONTENT changes here. That
  // is the whole fix and it is not the obvious shape: a region created in the same tick
  // as its first message is typically silent, because assistive tech announces CHANGES to
  // a region that already existed. Board.tsx's S5 note records this and calls it "the
  // worst kind of accessibility bug: the markup looks right". Putting role="status" on
  // the QuestionPanel itself — which mounts with the park — would have been exactly that.
  const [parkAnnounce, setParkAnnounce] = useState("");
  // PRD #517: BOTH needs-you parks announce, not just awaiting_input — awaiting_followup is
  // classified identically by needsHumanAttention and shows the same loud ring, so a
  // screen-reader user parking into it must get a signal too. A single stable KEY drives
  // the re-announce logic: awaiting_input keys on the question IDENTITY (a second question
  // re-announces while a re-render of the same park stays quiet, and an unusable/absent
  // question does not announce — the UnreadableQuestion branch owns that state), while
  // awaiting_followup is one stable park. The text is derived from the key in the effect.
  // NOTE this is PURELY the sr-only announcement; the QuestionPanel below stays
  // awaiting_input-only.
  const questionId = run?.status === "awaiting_input" ? (openQuestion?.question.questionId ?? "") : "";
  // Issue #754: a pool_wait park announces too — it is a non-terminal hold a
  // screen-reader user must be told about, exactly like the follow-up park. One
  // stable key ("pool_wait"), since a pool hold has no per-instance identity to
  // re-announce on the way awaiting_input keys on the question.
  const parkKey =
    questionId !== ""
      ? `question:${questionId}`
      : run?.status === "awaiting_followup"
        ? "followup"
        : run?.status === "pool_wait"
          ? "pool_wait"
          : "";
  useEffect(() => {
    if (parkKey === "") {
      setParkAnnounce("");
      return;
    }
    setParkAnnounce(
      parkKey === "followup"
        ? "The run is waiting for your next follow-up."
        : parkKey === "pool_wait"
          ? "The run is waiting for a pooled Anthropic token. Add a token to the pool and it resumes automatically."
          : "The agent is asking you a question. The run is parked until you answer.",
    );
  }, [parkKey]);

  if (!run) {
    return (
      <div className="space-y-4">
        <PageHeader backTo="/runs" backLabel="Runs" title="Run" />
        {error ? <Alert message={error} /> : <Card className="animate-pulse text-sm text-faint">Loading run…</Card>}
      </div>
    );
  }

  const terminal = isTerminalRun(run.status);
  // A deliberate stop (cancel, or a stop-shaped `failed`) is calm, never rose:
  // the header pill and the terminal banner both go neutral so they agree with
  // the board/RunsList treatment (isStoppedRun).
  const stopped = isStoppedRun(run.status, run.stop_kind);
  // MR state (PRD #33): a per-run frozen hint. It appends "merged"/"closed" to the
  // MR affordance and (for closed) drops the ok tone; open is unchanged.
  const mrState = mrChipState(run.mr_state);
  // The MR/PR link (PRD #65 D8): prefer the forge-supplied URL the worker persisted
  // (the only correct link on Forgejo), guarded through isHttpsUrl by preferForgeUrl
  // before it becomes an anchor. A null (rows created before it landed — all GitLab)
  // falls back to the legacy GitLab reconstruction from the repo web url.
  const mrUrl = preferForgeUrl(
    run.mr_web_url,
    run.mr_iid != null && isHttpsUrl(repoWebUrl) ? `${repoWebUrl}/-/merge_requests/${run.mr_iid}` : null,
  );
  const duration =
    run.started_at && run.finished_at
      ? formatDuration(new Date(run.finished_at).getTime() - new Date(run.started_at).getTime())
      : null;

  return (
    <div className="space-y-5">
      <PageHeader
        titleNode={
          <div className="min-w-0">
            {/* PRD #12: in-app board + issue links (the issue view is served
                by IssueView, not the forge). */}
            <nav className="mb-2 flex items-center gap-1.5 text-xs text-faint">
              <Link to="/runs" className="transition-colors hover:text-fg">
                Runs
              </Link>
              <span>/</span>
              <Link to={`/repos/${run.repo_id}/board`} className="transition-colors hover:text-fg">
                Board
              </Link>
              {/* An issue run links its card; a ci_fix run (PRD #6) has no issue —
                  its breadcrumb tail is just "CI fix". */}
              {run.kind !== "ci_fix" && run.issue_iid != null && (
                <>
                  <span>/</span>
                  <Link
                    to={`/repos/${run.repo_id}/issues/${run.issue_iid}`}
                    className="transition-colors hover:text-fg"
                  >
                    #{run.issue_iid}
                  </Link>
                </>
              )}
              {run.kind === "ci_fix" && (
                <>
                  <span>/</span>
                  <span className="text-muted">CI fix</span>
                </>
              )}
              <span>/</span>
              <span className="text-muted">Run</span>
            </nav>
            <RunHeading run={run} />
            <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
              {/* A stopped run (cancel or stop-shaped failure) reads as a neutral
                  "stopped" pill — StatusPill's default tone — so it stays calm and
                  agrees with the board/RunsList. */}
              <StatusPill status={stopped ? "stopped" : effectiveRunStatus(run)} />
              {/* PRD #320 M6: the queue-priority pill + the owner's Expedite/undo action.
                  Both are QUEUED-ONLY (the pill self-hides on any other status; the action
                  is wrapped in the status guard) — the server is queued-only too (409). */}
              <RunPriorityBadge priority={run.priority} status={run.status} />
              {run.status === "queued" &&
                (canSteer ? (
                  <Button
                    variant="secondary"
                    size="sm"
                    disabled={busy}
                    // Modelled on the wait-on-limit toggle: act() runs the owner-scoped
                    // mutation, then refreshRun() re-reads the run so the recomputed
                    // priority pill lands (no WS frame announces a priority change).
                    onClick={() =>
                      act(async () => {
                        await api.expediteRun(run.id, run.priority !== "expedited");
                        await refreshRun();
                      })
                    }
                  >
                    {run.priority === "expedited" ? "Undo expedite" : "Expedite"}
                  </Button>
                ) : (
                  // Non-owner (mirrors LimitWaitPanel's inert branch): show the state, no
                  // button that would 404 — expediting is the owner's to do.
                  <span className="text-xs text-muted">
                    {run.priority === "expedited"
                      ? "Expedited — only its owner can change this."
                      : "Only the run's owner can expedite it."}
                  </span>
                ))}
              {run.auto_approve && (
                <Badge tone="brand" title="Autopilot: started from the label, plan auto-approved">
                  autopilot
                </Badge>
              )}
              {/* Neutral PRD-presence marker (PRD #764): a linked PRD file is optional
                  but still detected and implemented when present, so a run whose issue
                  has one shows a quiet "PRD" badge beside the autopilot badge. */}
              {run.has_prd_link && (
                <Badge tone="neutral" title="This run's issue links a PRD file">
                  PRD
                </Badge>
              )}
              {/* ci_fix runs (PRD #6): the failing-pipeline link (isHttpsUrl-guarded)
                  and the verdict chip, extracted for isolated testing. */}
              <CIFixRunHeader run={run} terminal={terminal} />
              {stage && (
                <span className="inline-flex items-center gap-1.5 rounded-full border border-info/40 bg-info/10 px-2 py-0.5 text-[11px] font-medium text-info">
                  <Spinner /> {stage}…
                </span>
              )}
              {/* Run-health warn chip (PRD #47), next to the LIVE STAGE label. */}
              <HealthFlag run={run} />
              {/* The live/offline WS indicator is only meaningful while the run is
                  active; a terminal run has no stream, so never show "completed • live".

                  PRD #35 (web-ux F2): a PARKED run is also excluded, and that is a
                  correctness fix rather than tidiness. `live` is drawn in the `ok`
                  tone — a green go-signal — so on a parked run it sat directly beside
                  the amber "limit wait" pill, telling the user two opposite things at
                  once. And it is not even a claim about the run: it reports the
                  WEBSOCKET, which is genuinely connected while nothing whatsoever
                  happens for hours.

                  This is Success Criterion 2 ("visibly waiting, never stalled")
                  failing from the other side: not a false alarm, a false all-clear.
                  `awaiting_approval` has the same shape and keeps the chip, because a
                  human gate is minutes and someone is expected to be looking; a park
                  is hours by design, which is what makes it different in kind.

                  PRD #517: `awaiting_followup` also KEEPS the chip — it is a needs-you
                  gate (the user is expected to send the next follow-up), the same kind
                  as awaiting_input, NOT the self-resuming clock park that limit_wait is.

                  Issue #754: `pool_wait` is excluded for the SAME reason as limit_wait —
                  it is a self-resuming hold (blocked on a pooled token, resumes on its
                  own hours later), so a green "live" chip beside its amber wait pill is
                  the same false all-clear. The two self-resuming holds are excluded; the
                  needs-you gates keep the chip. */}
              {!terminal && run.status !== "limit_wait" && run.status !== "pool_wait" && (
                <span
                  title={connected ? "Live" : "Reconnecting…"}
                  className={cx(
                    "inline-flex items-center gap-1 text-xs",
                    connected ? "text-ok" : "text-faint",
                  )}
                >
                  <span className={cx("h-1.5 w-1.5 rounded-full", connected ? "bg-ok" : "bg-faint")} />
                  {connected ? "live" : "offline"}
                </span>
              )}
              {run.status === "running" && run.started_at && <LiveElapsed since={run.started_at} />}
              {run.iteration_count > 0 && (
                <Badge tone="neutral" title="implement ⇄ review iterations">
                  iteration {run.iteration_count}
                </Badge>
              )}
              {/* PRD #122: the compact milestone progress pill, shown ALONGSIDE the
                  iteration badge on a milestone-structured run. milestoneBadge returns
                  null for a run with no frozen list, so a pre-#122 run shows only the
                  iteration badge — unchanged. */}
              <MilestoneBadge run={run} />
              {/* Which Anthropic credential this run spent (PRD #111 M1). Here in
                  the header rather than beside the usage panel because it must show
                  for every claimed run, and the usage panel only appears once a run
                  has reported usage. */}
              <RunCredential run={run} />
              {/* PRD #300: the per-schedule model this run froze at fire time, shown on
                  EVERY status (not just completed) so a wrong/typo'd model is visible on a
                  FAILED or stopped run too (Risks / SC6). null = inherited the owner's
                  per-user Worker default. */}
              {run.model && (
                <Badge tone="neutral" title="Model this run was fired with (frozen by its schedule)">
                  model {stripUnsafeChars(run.model)}
                </Badge>
              )}
              {/* PRD #305: the frozen "apply model also to agents" flag, shown on EVERY
                  status like the model badge (SC6) so a user can confirm a run applied its
                  model fleet-wide. Absent = pins won (today's default). */}
              {run.override_subagent_model && (
                <Badge
                  tone="neutral"
                  title="This run's model was applied to every subagent, overriding their own model pins"
                >
                  model on all agents
                </Badge>
              )}
            </div>
          </div>
        }
      />

      {/* PRD #88: ALWAYS MOUNTED, empty until a park announces itself — see the note at
          parkAnnounce for why the region cannot live inside QuestionPanel. sr-only
          because the panel below already carries the message for everyone else. The
          effect fires after mount, so the region exists before its content changes even
          on a page load that arrives at an already-parked run. */}
      <div className="sr-only" role="status" aria-live="polite">
        {parkAnnounce}
      </div>

      {error && <Alert message={error} />}
      {actionErr && <Alert message={actionErr} />}

      {/* Issue #754: the pool-empty hold + Resume-now. Ordered ABOVE the usage-limit
          strip deliberately (web-ux should-fix): on a pool_wait run the strip below
          renders its NON-parked "Wait out future Anthropic usage limits" toggle, and
          two Anthropic controls stacked let a user read that usage-limit checkbox as
          the way to un-wait the pool hold, which it is not. Putting the pool panel
          first makes the hold read as one self-contained unit (its own Resume-now is
          the action), with the unrelated future-limit toggle clearly beneath it. This
          does NOT disturb the limit_wait layout: PoolWaitPanel self-hides on every
          status but pool_wait, so on a limit_wait run the strip below is still the
          first thing rendered here. */}
      <PoolWaitPanel run={run} canSteer={canSteer} onResumed={refreshRun} />

      {/* PRD #35: the usage-limit strip. High in the stack because on a parked run it
          carries the only thing the user came to find out — when it resumes — and low
          in weight otherwise, where it is just the per-run opt-in. Renders nothing at
          all for a terminal run.
          PRD #841: the per-run MR-review-rework toggle. Self-hides for a non-owner and
          once the MR is merged/closed; visible on a completed issue run whose MR is
          still open, because the watcher acts after completion. */}
      {run.status === "limit_wait" ? (
        // Parked: keep the full-width countdown card stacked exactly as today (no flex
        // item → no shrinking). MrReworkPanel still self-hides / renders beneath it via
        // the page-wide space-y-5 rhythm, unchanged.
        <>
          <LimitWaitPanel
            run={run}
            busy={busy}
            canSteer={canSteer}
            onStop={() => act(() => submit("cancel"))}
            onToggle={(enabled) =>
              act(async () => {
                await api.setRunWaitOnLimit(run.id, enabled);
                // The flag is not a status change, so no WS frame announces it — without
                // this refetch the checkbox would snap back to the stale run on the next
                // render, which reads as the write having failed.
                await refreshRun();
              })
            }
          />
          <MrReworkPanel
            run={run}
            busy={busy}
            canSteer={canSteer}
            userDefault={mrReworkDefault}
            onToggle={(enabled) =>
              act(async () => {
                await api.setRunMrRework(run.id, enabled);
                // Not a status change, so no WS frame announces it — refetch so the checkbox
                // reflects the new run value instead of snapping back to the stale one.
                await refreshRun();
              })
            }
          />
        </>
      ) : (
        // Non-parked: the two simple inline toggles share one row from `sm` up, and stay
        // cleanly stacked (two lines) on mobile — the two ~87-char labels would wrap
        // raggedly if we relied on flex-wrap alone at narrow widths, so the breakpoint is
        // explicit. Gated so no always-present empty <div> adds a stray space-y-5 margin
        // on terminal runs where both panels render nothing.
        (canToggleWaitOnLimit(run.status) || (canSteer && canToggleMrRework(run))) && (
          <div className="flex flex-col gap-y-2 sm:flex-row sm:flex-wrap sm:items-center sm:gap-x-6">
            <LimitWaitPanel
              run={run}
              busy={busy}
              canSteer={canSteer}
              onStop={() => act(() => submit("cancel"))}
              onToggle={(enabled) =>
                act(async () => {
                  await api.setRunWaitOnLimit(run.id, enabled);
                  // The flag is not a status change, so no WS frame announces it — without
                  // this refetch the checkbox would snap back to the stale run on the next
                  // render, which reads as the write having failed.
                  await refreshRun();
                })
              }
            />
            <MrReworkPanel
              run={run}
              busy={busy}
              canSteer={canSteer}
              userDefault={mrReworkDefault}
              onToggle={(enabled) =>
                act(async () => {
                  await api.setRunMrRework(run.id, enabled);
                  // Not a status change, so no WS frame announces it — refetch so the checkbox
                  // reflects the new run value instead of snapping back to the stale one.
                  await refreshRun();
                })
              }
            />
          </div>
        )
      )}

      {/* PRD #362 M4: the plain-English run summary — intent, proposed/approved plan, and
          deltas from the original ask. Self-hides until a summary lands (the issue-title
          header carries the run until then), collapsible + remembered per run. */}
      <RunSummary run={run} />

      {/* Terminal hero: the outcome, front and center. */}
      {run.status === "completed" && (
        <div className="rounded-xl border border-ok/40 bg-ok/10 p-4">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <p className="text-sm font-semibold text-ok">Run completed</p>
              <RunCompletedLine run={run} duration={duration} mrState={mrState} />
            </div>
            {mrUrl ? (
              <a href={mrUrl} target="_blank" rel="noreferrer">
                <Button>
                  Open {forgeNounLower(run.forge_type)} {mrRefSymbol(run.forge_type)}
                  {run.mr_iid}
                  {mrChipSuffix(mrState)} <ExternalLinkIcon />
                </Button>
              </a>
            ) : run.mr_iid != null ? (
              <Badge tone={mrState === "closed" ? "neutral" : "ok"} title={mrChipTitle(mrState, run.forge_type)}>
                {mrAbbrev(run.forge_type)}{" "}
                <span className={mrState === "closed" ? "line-through" : undefined}>
                  {mrRefSymbol(run.forge_type)}
                  {run.mr_iid}
                </span>
                {mrChipSuffix(mrState)}
              </Badge>
            ) : (
              // issue #279: a report-only run intentionally opened no MR — explain the
              // empty slot with a neutral chip, the way ci_fix not_code does.
              run.report_only && (
                <Badge
                  tone="neutral"
                  title="This run's deliverable is a report; it intentionally opened no merge request."
                >
                  report only
                </Badge>
              )
            )}
          </div>
        </div>
      )}

      {/* issue #279: a report-only run's deliverable is report_md. Render it right
          under the completed hero, above the retrospective.

          report_md is UNTRUSTED worker/model-authored text. It is DELIBERATELY rendered
          as escaped plain text (React's default + whitespace-pre-wrap), never through
          <Markdown> — the ingest scrub does NOT cover markdown/link injection, exactly as
          review.summary_md is rendered below. If this is ever switched to a markdown/HTML
          renderer, add sanitization first. See lib/safeText.ts. */}
      {run.status === "completed" &&
        run.report_only &&
        run.report_md != null &&
        run.report_md.trim() !== "" && (
          <Card className="space-y-2 p-4">
            <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Findings</h2>
            {/* Cap a long summary and scroll it, like SeededPlanPanel, so the Activity feed
                below stays reachable without a long page scroll. */}
            <p className="max-h-96 overflow-auto whitespace-pre-wrap text-sm text-muted">
              {stripUnsafeChars(run.report_md)}
            </p>
          </Card>
        )}

      {terminal && run.status !== "completed" && (
        <div
          className={cx(
            "rounded-xl border p-4",
            stopped ? "border-edge bg-raised/50" : "border-danger/40 bg-danger/10",
          )}
        >
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <p className={cx("text-sm font-semibold", stopped ? "text-fg" : "text-danger")}>
                Run {stopped ? "stopped" : "failed"}
              </p>
              <RunFailureReason run={run} stopped={stopped} />
              <RunStopReason run={run} />
              {duration && <p className="mt-0.5 text-xs text-muted">Ran for {duration}.</p>}
            </div>
            {/* The MR link is the run's whole output; surface it even on a failed or
                stopped run, not just the completed hero (a calm secondary button). */}
            {mrUrl ? (
              <a href={mrUrl} target="_blank" rel="noreferrer">
                <Button variant="secondary">
                  Open {forgeNounLower(run.forge_type)} {mrRefSymbol(run.forge_type)}
                  {run.mr_iid}
                  {mrChipSuffix(mrState)} <ExternalLinkIcon />
                </Button>
              </a>
            ) : (
              run.mr_iid != null && (
                <Badge tone="neutral" title={mrChipTitle(mrState, run.forge_type)}>
                  {mrAbbrev(run.forge_type)}{" "}
                  <span className={mrState === "closed" ? "line-through" : undefined}>
                    {mrRefSymbol(run.forge_type)}
                    {run.mr_iid}
                  </span>
                  {mrChipSuffix(mrState)}
                </Badge>
              )
            )}
          </div>

          {/* PRD #377 M2: a GitHub run whose branch touched .github/workflows/** cannot be
              pushed by the bot's repo-only PAT, so it ends `failed` with the agent's diff
              preserved here. This is VALID work uzi can't auto-land — framed as "here's the
              diff to land as a human PR", NOT a crash. The authoritative next-step text lives
              in failure_reason (rendered above); this block is just the labelled diff.

              preserved_patch is UNTRUSTED worker/model-authored text, rendered as escaped
              plain text through stripUnsafeChars (same footing as report_md/failure_reason),
              never through <Markdown>. */}
          {run.preserved_patch != null && run.preserved_patch.trim() !== "" && (
            <div className="mt-3 space-y-2 border-t border-edge/60 pt-3">
              <p className="text-xs text-muted">
                This change is valid, but uzi&rsquo;s bot token can&rsquo;t push workflow files.
                Here&rsquo;s the diff to land as a human PR:
              </p>
              <pre className="max-h-96 overflow-auto rounded-md bg-raised/60 p-3 font-mono text-xs leading-relaxed text-fg whitespace-pre">
                {stripUnsafeChars(run.preserved_patch)}
              </pre>
            </div>
          )}
        </div>
      )}

      {/* Run retrospective (PRD #46 M4): the LLM judge's verdict + recommendations,
          shown once a run is finished. The panel fetches its own review and owns the
          re-run action; it renders nothing for a non-terminal or ineligible run. */}
      {terminal && <JudgePanel run={run} />}

      {run.status === "awaiting_approval" && (
        <PlanPanel
          run={run}
          messages={messages}
          workers={workers}
          busy={busy}
          canSteer={canSteer}
          onApprove={(selection, overrideCapabilities) =>
            act(() => submit("approve_plan", "", selection, overrideCapabilities))
          }
          onReject={(reason) => act(() => submit("reject_plan", reason))}
          onRequestChanges={(feedback) => act(() => submit("revise_plan", feedback))}
          onCancel={() => act(() => submit("cancel"))}
        />
      )}

      {/* PRD #209 M5: a SEEDED run's plan. It never enters awaiting_approval, so the
          PlanPanel above never renders and run.plan_md has no home on the page. This is
          the read-only surface for it, shown in every state (queued through terminal).
          Gated on plan_source alone: it is "seeded" ONLY for a seeded run and never
          overlaps the gate — M1 flips plan_source back to 'agent' in the same UPDATE that
          rewrites plan_md if a seeded run ever fell through to awaiting_approval (the D8
          safety fix), so the two panels are disjoint by construction, not by a status
          guard bolted on here. */}
      {run.plan_source === "seeded" && <SeededPlanPanel run={run} />}

      {/* PRD #88: the clarification park. Gated on BOTH the status and a derived open
          question: the status alone would render a composer with nothing to answer in
          the window before the `question` message replays (the worker flushes it first,
          but a reconnecting client can read the status back first), and the question
          alone would keep offering a composer after the run resumed. */}
      {run.status === "awaiting_input" &&
        (openQuestion ? (
          <QuestionPanel
            open={openQuestion}
            busy={busy}
            canSteer={canSteer}
            onAnswer={(body) => act(() => submit("answer", body))}
            onCancel={canSteer ? () => act(() => submit("cancel")) : undefined}
          />
        ) : (
          // Parked, but the question is unusable — no question_id, or nothing renderable.
          // Previously this rendered NOTHING: the run sat at "needs your answer" with no
          // panel and no explanation until the deadline failed it, and an absent
          // affordance reads as "not loaded yet", so the reasonable response was to wait.
          <UnreadableQuestion
            busy={busy}
            onCancel={canSteer ? () => act(() => submit("cancel")) : undefined}
          />
        ))}

      {/* Read-only record of which agents the run used, once a selection is made
          (at the gate or by an autopilot default). Shown for a live/terminal run;
          the picker above owns the awaiting_approval state. PRD #37 Decision 3(b). */}
      {run.status !== "awaiting_approval" && run.agent_source && (
        <AgentRosterSummary run={run} />
      )}

      {/* PRD #122: the milestone checklist — done / in-progress / left, driven by the
          frozen list + the reported-complete/in-progress id sets. Renders nothing for a
          run with no milestones. */}
      <MilestoneChecklist run={run} />

      {(usage.hasLiveTokens || usage.hasConfirmed) && (
        <Card className="p-4">
          <RunUsagePanel usage={usage} />
        </Card>
      )}

      <Card className="p-4">
        <ActivityFeed
          messages={messages}
          run={run}
          runningLive={run.status === "running"}
          connected={connected}
          terminal={terminal}
          phaseUsageBySeq={usage.phaseUsageBySeq}
          // `?? null` tells ActivityFeed "the parent already derived; do not re-derive"
          // (PRD #516 / issue #553): null distinguishes "no reading" from "not computed".
          leadContext={usage.leadContext ?? null}
        />
      </Card>

      {/* Steer queue + composer (PRD #95). Rendered UNCONDITIONALLY — including for a
          terminal run — so the queue survives the run finishing (Decision 7/B1); the card
          gates the composer/Stop on !terminal internally. Its inputs are lifted into
          useRunStream, not held here or in the composer. */}
      <SteerQueueCard
        inputs={inputs}
        terminal={terminal}
        status={run.status}
        canSteer={canSteer}
        busy={busy}
        onStop={() => act(() => submit("cancel"))}
        onSend={(text) => act(() => submit("follow_up", text))}
      />
    </div>
  );
}

// AgentRosterSummary: the read-only record of the locked-in selection, shown once
// a run is past the gate (PRD #37 Decision 3b). For a repo-source run it says so
// plainly, so a reader knows the internal review loop was repo-authored. Repo
// names/descriptions render as plain JSX text, never <Markdown>.
//
// PRD #209 M5: a SEEDED run carries agent_source="repo" from creation, so this card
// mounts immediately — but repo_agents stays null until the worker's post-checkout
// report lands the clone's roster. Rendering the definitive "used the repository's
// own agents" claim over an empty chip list would assert a roster the run does not
// yet have. So a SEEDED repo-source run with an empty roster shows a "roster pending"
// state instead. Keyed on plan_source==="seeded" (not on roster-emptiness alone): a
// normal repo-source run reports repo_agents before agent_source is even set, so its
// roster is never empty here — but keying on the seeded signal directly is more robust
// than inferring it from an empty roster, which has edges (e.g. a repo that reports []).
/**
 * The run header's title line. Extracted and exported for the same reason PlanPanel,
 * AgentRosterSummary and JudgePanel are: `RunView` itself needs routing, a live stream and
 * a dozen API mocks to mount, so an assertion about the heading could not otherwise be
 * written — and this line was the one #124 render site in the batch with no test at all
 * (dropping its strip left all 42 cases green).
 *
 * `issue_title` is the FORGE issue title: writable by anyone who can open an issue on the
 * target repo, so it is untrusted free text on the same footing as judge output. Display
 * only — nothing here is posted back or used as a key.
 */
/**
 * The completed-run hero's detail line. Exported for the same reason RunHeading is: `RunView`
 * needs routing, a live stream and a dozen API mocks to mount, so `run.branch` could not
 * otherwise be asserted — and an unverified strip is what this batch keeps finding.
 *
 * `run.branch` is WORKER-supplied and ingest stores it as `stripNULParam(req.Branch)` — NUL
 * only, no Cc/Cf — so it has the same profile as every other field this batch closed.
 */
/**
 * The failed/stopped hero's reason line, extracted so it can be asserted at all — it renders
 * in a different `RunView` branch from HealthFlag, so no single fixture reaches both.
 *
 * Issue #124, TEXT channel. `failure_reason` is worker-supplied, ingest is `stripNUL` +
 * truncate (`sanitizeFailureReason`), and `service.go` writes `err.Error()` straight in — so
 * a hostile repo can shape the error an agent fails with. This is the LARGER of the two
 * surfaces that carry it: a paragraph of body text, versus a tooltip you have to hover.
 */
export function RunFailureReason({ run, stopped }: { run: Run; stopped?: boolean }) {
  if (!run.failure_reason) return null;
  return (
    <p className={cx("mt-0.5 text-xs", stopped ? "text-muted" : "text-danger/80")}>
      {stripUnsafeChars(run.failure_reason)}
    </p>
  );
}

/**
 * The operator's free-text cancel reason (issue #525), shown in the stopped/failed hero
 * beside the failure_reason line. On the live-poller cancel path failure_reason is the
 * generic "run cancelled", so this is the line that actually says WHY. Untrusted free
 * text, same channel as failure_reason — through stripUnsafeChars. Renders nothing when unset.
 */
export function RunStopReason({ run }: { run: Run }) {
  if (!run.stop_reason) return null;
  return (
    <p className="mt-0.5 text-xs text-muted">
      Reason: {stripUnsafeChars(run.stop_reason)}
    </p>
  );
}

export function RunCompletedLine({
  run,
  duration,
  mrState,
}: {
  run: Run;
  duration?: string | null;
  mrState?: string | null;
}) {
  return (
    <p className="mt-0.5 text-xs text-muted">
      {duration && <>Ran for {duration}. </>}
      {/* issue #279: a report-only run has no branch and no MR — name the deliverable so the
          hero is not a silently-empty "Run completed". It is mutually exclusive with the
          branch/MR clause (a report_only completion pushes neither), so this branch guards
          against a contradictory "Branch … Report only" line, and only promises "findings
          below" when there is actually a report_md to render below. */}
      {run.report_only ? (
        <>
          Report only — no merge request
          {run.report_md != null && run.report_md.trim() !== "" ? "; findings below" : ""}.
        </>
      ) : (
        run.branch && (
          <>
            Branch <code className="rounded bg-raised px-1 py-0.5 text-fg">{stripUnsafeChars(run.branch)}</code>
            {run.mr_iid != null &&
              ` — ${forgeNounLower(run.forge_type)} ${mrState === "merged" ? "merged" : mrState === "closed" ? "closed" : "opened"}.`}
            {/* issue #150: the run's declared PRD-completion move. WORKER-DECLARED untrusted
                text, so it goes through stripUnsafeChars exactly like run.branch above. */}
            {run.prd_done_path && (
              <>
                {" · "}PRD moved to{" "}
                <code className="rounded bg-raised px-1 py-0.5 text-fg">{stripUnsafeChars(run.prd_done_path)}</code>
              </>
            )}
          </>
        )
      )}
    </p>
  );
}

export function RunHeading({ run }: { run: Run }) {
  return (
    <div className="flex flex-wrap items-center gap-x-2">
      <h1 className="truncate text-xl font-semibold tracking-tight">{stripUnsafeChars(run.issue_title)}</h1>
      <RunIssueRef
        issueIid={run.issue_iid}
        issueWebUrl={run.issue_web_url}
        kind={run.kind}
        forgeType={run.forge_type}
        className="text-sm text-faint"
      />
    </div>
  );
}

export function AgentRosterSummary({ run }: { run: Run }) {
  const excluded = new Set(run.agent_exclusions ?? []);
  const roster = run.agent_source === "repo" ? (run.repo_agents ?? []) : (run.own_agents ?? []);
  // own_agents now carries the own-source roster on the detail read, so this lists
  // the actual agent names for either source (M4-fix) instead of nothing for own.
  const included = roster.filter((a) => !excluded.has(a.name)).map((a) => a.name);
  // PRD #209 M5: a seeded run whose repo roster has not been reported yet (before its
  // post-checkout report). Keyed on the seeded signal directly. Only the repo branch can
  // be pending — an own-source run with no templates is a legitimate lead-only run, not a
  // missing roster, so it keeps its definitive copy.
  const repoRosterPending =
    run.plan_source === "seeded" && run.agent_source === "repo" && roster.length === 0;
  return (
    <Card className="space-y-2 p-4">
      <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">
        {repoRosterPending ? "Agents" : "Agents used"}
      </h2>
      {repoRosterPending ? (
        <p className="text-sm text-muted">
          This run uses the repository's own agents from{" "}
          <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">.claude/agents/</code>. The
          roster appears here once the worker checks out the repository.
        </p>
      ) : run.agent_source === "repo" ? (
        <p className="text-sm text-muted">
          This run used the repository's own agents from{" "}
          <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">.claude/agents/</code> — its
          internal review was performed by repo-authored agents, not uzi's built-in reviewer.
        </p>
      ) : (
        <p className="text-sm text-muted">This run used your uzi agent templates.</p>
      )}
      {included.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {included.map((name) => (
            <span
              key={name}
              className="rounded-full border border-edge-strong bg-raised px-2.5 py-[3px] font-mono text-[11.5px] text-fg"
            >
              {name}
            </span>
          ))}
        </div>
      )}
      {excluded.size > 0 && <p className="text-xs text-faint">Excluded: {[...excluded].join(", ")}</p>}
    </Card>
  );
}
