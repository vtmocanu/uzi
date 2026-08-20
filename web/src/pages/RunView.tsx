// Live run view. The header carries the status pill plus a LIVE STAGE label
// derived from the newest message — multica's task-status-pill idea
// (packages/views/chat/components/task-status-pill.tsx maps the latest tool
// slug to a human stage: "Running command", "Reading files", "Making edits"…)
// — so you can tell what the agent is doing without reading the feed. Terminal
// states get a hero banner: the MR link is the run's entire output and must
// not hide in chrome. The breadcrumb keeps PRD #12's in-app board / issue links.

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import {
  api,
  ApiError,
  isHttpsUrl,
  preferForgeUrl,
  isTerminalRun,
  type AgentSelectionInput,
  type Disposition,
  type FiledIssue,
  type IssueDraft,
  type PendingJudge,
  type Repo,
  type ReviewRecommendation,
  type Run,
  type RunMessage,
  type RunReview,
  type TriageCounts,
} from "../lib/api";
import { coordKey, recommendationLabel, verdictLabel, verdictTone } from "../lib/judge";
import { canToggleWaitOnLimit, formatCountdown, runWindowLabel } from "../lib/limitWait";
import { stripUnsafeChars } from "../lib/safeText";
import { AgentPicker, selectionLabel, type OwnTemplate } from "../components/AgentPicker";
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
import { formatTokens, formatCost } from "../lib/formatTokens";
import { ActivityFeed } from "../components/ActivityFeed";
import { SteerQueueCard } from "../components/SteerQueueCard";
import { QuestionPanel, UnreadableQuestion } from "../components/QuestionPanel";
import { deriveOpenQuestion } from "../lib/runQuestion";
import { Markdown } from "../components/Markdown";
import { Alert, Badge, Button, Card, Input, PageHeader, Select, Spinner, StatusPill, Textarea, cx, type BadgeTone } from "../components/ui";
import { summaryCollapse } from "../lib/prefs";
import { ExternalLinkIcon, FileTextIcon } from "../components/icons";

// stageForMessages: latest-message → human stage label (multica's
// TOOL_KEY_BY_SLUG, adapted to uzi's message kinds).
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
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
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
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(id);
  }, []);
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
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    // Only while parked: off the park there is no clock, and a 1s interval running
    // for the life of every run view is pure waste. 1s (not HealthFlag's 30s)
    // because the countdown drops to seconds in its last minute, which is exactly
    // when someone is watching it.
    if (!parked) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [parked]);

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

export function RunView() {
  const { id = "" } = useParams();
  const { run, messages, connected, error, submit, refreshRun, inputs, canSteer } = useRunStream(id);
  const [repoWebUrl, setRepoWebUrl] = useState<string | null>(null);
  const [actionErr, setActionErr] = useState("");
  const [busy, setBusy] = useState(false);

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

  const act = async (fn: () => Promise<unknown>) => {
    setActionErr("");
    setBusy(true);
    try {
      await fn();
    } catch (e) {
      setActionErr(e instanceof ApiError ? e.message : "Action failed");
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
  const parked = run?.status === "awaiting_input" ? (openQuestion?.question.questionId ?? "") : "";
  useEffect(() => {
    // Keyed on the question IDENTITY, so a second question re-announces while a re-render
    // of the same park stays quiet.
    if (parked === "") {
      setParkAnnounce("");
      return;
    }
    setParkAnnounce("The agent is asking you a question. The run is parked until you answer.");
  }, [parked]);

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
                  is hours by design, which is what makes it different in kind. */}
              {!terminal && run.status !== "limit_wait" && (
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

      {/* PRD #35: the usage-limit strip. High in the stack because on a parked run it
          carries the only thing the user came to find out — when it resumes — and low
          in weight otherwise, where it is just the per-run opt-in. Renders nothing at
          all for a terminal run. */}
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
          busy={busy}
          canSteer={canSteer}
          onApprove={(selection) => act(() => submit("approve_plan", "", selection))}
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

// The server enforces the real revision cap; 3 is only the display default for the
// "revision N of MAX" counter (PRD #41 Decision 9).
const MAX_REVISION_ROUNDS = 3;

// PlanRevision is the panel's derived view of the revision history (PRD #41 Decision
// 9): all of it comes from the run's feed messages, never a separate fetch.
export interface PlanRevision {
  // versions = number of `plan` messages so far (v1, v2, …).
  versions: number;
  // rounds = number of `plan_feedback` rounds — the "revision N of MAX" counter.
  rounds: number;
  // revising = the LATEST of {`plan`, `plan_revising`} by seq is a `plan_revising`
  // frame, i.e. the planner is reworking and the gate is parked (not open).
  revising: boolean;
  // latestFeedback = the newest `plan_feedback` steering text (the user's bubble).
  latestFeedback: string | null;
  // priorPlans = the plan_md of every SUPERSEDED plan version (all but the latest),
  // oldest-first — the collapsed history accordion once a v2+ is re-gated.
  priorPlans: string[];
}

// derivePlanRevision folds the feed into the panel's revision state. Exported for a
// direct unit test of the derivation (the component test drives the UI on top of it).
export function derivePlanRevision(messages: RunMessage[]): PlanRevision {
  const plans = messages.filter((m) => m.kind === "plan");
  const feedbacks = messages.filter((m) => m.kind === "plan_feedback");

  // The gate's live state is the latest of {plan, plan_revising} BY SEQ — a newer
  // `plan` (re-gated) beats an earlier `plan_revising`, and vice-versa.
  let latestGating: RunMessage | undefined;
  for (const m of messages) {
    if (m.kind !== "plan" && m.kind !== "plan_revising") continue;
    if (!latestGating || m.seq > latestGating.seq) latestGating = m;
  }

  let latestFeedback: string | null = null;
  let latestFeedbackSeq = -Infinity;
  for (const m of feedbacks) {
    const fb = (m.payload as { feedback?: string } | null)?.feedback;
    if (typeof fb === "string" && m.seq > latestFeedbackSeq) {
      latestFeedback = fb;
      latestFeedbackSeq = m.seq;
    }
  }

  const priorPlans = plans
    .slice(0, Math.max(0, plans.length - 1))
    .map((m) => (m.payload as { plan_md?: string } | null)?.plan_md ?? "")
    .filter((s) => s.trim() !== "");

  return {
    versions: plans.length,
    rounds: feedbacks.length,
    revising: latestGating?.kind === "plan_revising",
    latestFeedback,
    priorPlans,
  };
}

// VersionChip is the mono v1/v2 badge in the panel head. Info-toned in the revising
// (parked) state, warn-toned at an open gate — matching the panel's own tone.
function VersionChip({ label, parked }: { label: string; parked?: boolean }) {
  return (
    <span
      className={cx(
        "inline-flex items-center rounded-md border px-1.5 py-px font-mono text-[11px] font-semibold",
        parked ? "border-info/40 bg-info/[0.12] text-info" : "border-warn/40 bg-warn/[0.12] text-warn",
      )}
    >
      {label}
    </span>
  );
}

// RevisionThread renders the user's steering bubble (PRD #41). The feedback is
// UNTRUSTED, so it goes through the hardened <Markdown> — never a raw sink. `working`
// appends the info-toned "planner is reworking" spinner line (the parked state).
function RevisionThread({ feedback, working = false }: { feedback: string | null; working?: boolean }) {
  if (!feedback && !working) return null;
  return (
    <div className="space-y-2.5">
      <span className="text-[11px] font-semibold uppercase tracking-wider text-faint">Revision thread</span>
      {feedback && (
        <div className="ml-auto max-w-[85%] rounded-lg border border-brand/30 bg-brand/[0.12] px-3 py-2 text-sm">
          <span className="mb-1 block text-[11px] font-semibold text-brand">You requested</span>
          <Markdown content={feedback} />
        </div>
      )}
      {working && (
        <div className="flex items-center gap-2 text-sm italic text-muted">
          <Spinner /> planner is reworking the plan…
        </div>
      )}
    </div>
  );
}

// PlanPanel: the run's one human decision point — visually the loudest thing on
// the page while it is pending. Grows the PRD #37 agent picker: the user chooses
// the subagent roster (repo agents when detected, else their templates) with the
// approve verdict; the choice is submitted as a structured selection on approve.
// PRD #41 adds the third gate action (Request changes → revise_plan), the revising
// parked state, the version chip, and the collapsed history of superseded plans.
export function PlanPanel({
  run,
  messages = [],
  busy,
  canSteer = true,
  onApprove,
  onReject,
  onRequestChanges,
  onCancel,
}: {
  run: Run;
  // The run's feed, used to derive the version chip / round counter / revising state
  // (PRD #41 Decision 9). Optional so a caller/test that only exercises the base gate
  // can omit it — the derivation degrades to v1 / revision 0 of 3.
  messages?: RunMessage[];
  busy: boolean;
  // False for a NON-OWNER viewer. PRE-EXISTING and unrelated to PRD #88: POST /inputs is
  // user-scoped, so a non-owner admin — who can legitimately OPEN this owner-or-admin run
  // view — got an Approve button that 404s. useRunStream states the rule ("never a broken
  // Send that 404s") and SteerQueueCard already obeyed it; this panel never did. Fixed
  // alongside the question composer because fixing only the newer one would have left the
  // identical hole one panel over. Defaults true so an owner is never gated by an absent
  // prop, and it hides the ACTIONS only — the plan body stays readable, which is the whole
  // reason a non-owner admin opens this page.
  canSteer?: boolean;
  onApprove: (selection: AgentSelectionInput) => void;
  onReject: (reason: string) => void;
  // Request-changes (PRD #41) and the revising-state Cancel-run affordance. Optional
  // with a no-op default so a base-gate-only caller need not wire them.
  onRequestChanges?: (feedback: string) => void;
  onCancel?: () => void;
}) {
  const [rejecting, setRejecting] = useState(false);
  const [requesting, setRequesting] = useState(false);
  const [reason, setReason] = useState("");
  const [feedback, setFeedback] = useState("");

  const rev = useMemo(() => derivePlanRevision(messages), [messages]);

  const repoAgents = useMemo(() => run.repo_agents ?? [], [run.repo_agents]);
  const repoDetected = repoAgents.length > 0;

  // The "My agent templates" card is sourced from the run's own_agents — the
  // server's allocation-resolved roster (what the worker actually runs for
  // source="own"), with the lead already stripped. Reading it off the run (instead
  // of a separate listAgentTemplates fetch of the broader VISIBLE set) is the
  // M4-fix: a chip can never name a template the approve validator rejects, and the
  // count is exact for owners with a disabled/shadowed template. own_agents carries
  // no scope, so the "custom" badge is not shown here.
  const ownTemplates = useMemo<OwnTemplate[]>(
    () => (run.own_agents ?? []).map((a) => ({ name: a.name, description: a.description, custom: false })),
    [run.own_agents],
  );

  // The picker reports the live selection here; the approve button submits it. The
  // default (repo when detected, else own, no exclusions) is what the picker emits
  // on mount, so approving without touching anything sends the right thing.
  const [selection, setSelection] = useState<AgentSelectionInput>({
    source: repoDetected ? "repo" : "own",
    exclusions: [],
  });
  const onSelectionChange = useCallback((s: AgentSelectionInput) => setSelection(s), []);

  const activeRoster = selection.source === "repo" ? repoAgents.map((a) => a.name) : ownTemplates.map((t) => t.name);
  const activeCount = activeRoster.length - selection.exclusions.length;
  const approveLabel =
    activeRoster.length > 0 ? `Approve plan · ${selectionLabel(selection.source, activeCount)}` : "Approve plan";

  // The rounds counter is always shown at the head; MAX is the display default (the
  // server owns the real cap).
  const roundsLabel = `revision ${rev.rounds} of ${MAX_REVISION_ROUNDS}`;

  // Revising (parked) state: the planner is reworking after a request-changes. The
  // panel goes info-toned, swaps the gate for the revision thread + a Cancel-run
  // affordance, and shows a v(N)→v(N+1) chip (the next version has not landed yet).
  if (rev.revising) {
    return (
      <div className="overflow-hidden rounded-xl border border-info/40 bg-info/5">
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-info/25 bg-info/[0.08] px-4 py-3">
          <div>
            <h2 className="flex items-center gap-2 text-sm font-semibold text-info">
              Revising the plan
              <VersionChip parked label={`v${rev.versions} → v${rev.versions + 1}`} />
            </h2>
            <p className="text-xs text-muted">
              Your feedback was sent to the planning session. The updated plan will return here for approval.
            </p>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-[11px] text-faint">{roundsLabel}</span>
            {canSteer && (
              <Button variant="danger" disabled={busy} onClick={() => onCancel?.()}>
                Cancel run
              </Button>
            )}
          </div>
        </div>
        <div className="p-4">
          <RevisionThread feedback={rev.latestFeedback} working />
        </div>
      </div>
    );
  }

  // Open gate. A v2+ re-gate reads "Updated plan…" and shows the superseded history.
  const revised = rev.versions > 1;
  const currentVersion = Math.max(rev.versions, 1);
  const disclosing = rejecting || requesting;

  return (
    <div className="overflow-hidden rounded-xl border border-warn/50 bg-warn/5">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-warn/30 bg-warn/10 px-4 py-3">
        <div>
          <h2 className="flex items-center gap-2 text-sm font-semibold text-warn">
            {/* The HEADING is conditional on canSteer for the same reason the subtitle
                below is. Leaving it unconditional put "Plan awaiting your approval"
                directly above "Only they can approve or reject it" for a non-owner —
                a card contradicting itself on adjacent lines. QuestionPanel's non-owner
                branch changes both, and this is the older panel's half of that fix. */}
            {!canSteer
              ? revised
                ? "Updated plan awaiting the owner's approval"
                : "Plan awaiting the owner's approval"
              : revised
                ? "Updated plan awaiting your approval"
                : "Plan awaiting your approval"}
            <VersionChip label={`v${currentVersion}`} />
          </h2>
          <p className="text-xs text-muted">
            {!canSteer
              ? "The run is parked until its owner decides. Only they can approve or reject it."
              : requesting
                ? "Describe what should change; the other actions return if you cancel."
                : "The run is parked until you decide. Agent choice locks in on approval."}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <span className="text-[11px] text-faint">{roundsLabel}</span>
          {canSteer && !disclosing && (
            <div className="flex gap-2">
              <Button disabled={busy} onClick={() => onApprove(selection)}>
                {approveLabel}
              </Button>
              <Button variant="secondary" disabled={busy} onClick={() => setRequesting(true)}>
                Request changes
              </Button>
              <Button variant="danger" disabled={busy} onClick={() => setRejecting(true)}>
                Reject
              </Button>
            </div>
          )}
        </div>
      </div>
      <div className="space-y-4 p-4">
        {/* The picker exists to shape the approve verdict, so it is an action surface:
            shown only to someone who can approve. The locked-in roster is a separate,
            read-only card (AgentRosterSummary) once the run is past the gate. */}
        {canSteer && (
          <AgentPicker repoAgents={repoAgents} ownTemplates={ownTemplates} onChange={onSelectionChange} />
        )}

        {/* PRD #122: the PRE-APPROVAL candidate milestones, shown ONLY at the plan gate,
            beside the plan body. Rendered as PLAIN JSX (never <Markdown>): a candidate
            title is agent/repo-authored untrusted text, and this is an approval dialog —
            the exact place an attacker-authored title must not become a clickable link
            (same rule as the repo-agent descriptions in the picker above). Nothing renders
            when the list is null/empty. */}
        {run.milestones_candidate && run.milestones_candidate.length > 0 && (
          <div className="rounded-lg border border-edge bg-surface/60 p-3">
            <p className="mb-2 text-[11px] font-semibold uppercase tracking-wider text-faint">Proposed milestones</p>
            <ol className="list-decimal space-y-1 pl-5 text-sm text-fg">
              {run.milestones_candidate.map((m) => (
                <li key={m.id} className="min-w-0">
                  {stripUnsafeChars(m.title)}
                </li>
              ))}
            </ol>
          </div>
        )}

        {run.plan_md ? (
          <div className="max-h-96 overflow-auto rounded-lg border border-edge bg-surface p-3">
            <Markdown content={run.plan_md} />
          </div>
        ) : (
          <p className="text-sm text-faint">The agent has not attached a plan body.</p>
        )}

        {/* Request-changes composer: disclosed only after selecting the action, the
            same pattern as the reject-with-reason disclosure. "Send & revise" submits
            revise_plan; Cancel restores the header actions. */}
        {requesting && (
          <div className="space-y-2">
            <Textarea
              rows={3}
              placeholder="What should change? (sent to the planning session; the plan returns here for approval)"
              value={feedback}
              onChange={(e) => setFeedback(e.target.value)}
            />
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="text-[11px] text-faint">
                Feedback goes to the planning session; the agent revises and the plan returns here for approval.{" "}
                {MAX_REVISION_ROUNDS} revision rounds max.
              </span>
              <div className="flex gap-2">
                <Button variant="ghost" disabled={busy} onClick={() => setRequesting(false)}>
                  Cancel
                </Button>
                <Button disabled={busy || feedback.trim() === ""} onClick={() => onRequestChanges?.(feedback)}>
                  Send &amp; revise
                </Button>
              </div>
            </div>
          </div>
        )}

        {/* Reject-with-reason disclosure (unchanged semantics). */}
        {rejecting && (
          <div className="space-y-2">
            <Textarea
              rows={3}
              placeholder="What should change? (sent back to the agent as the next turn)"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
            <div className="flex gap-2">
              <Button variant="danger" disabled={busy} onClick={() => onReject(reason)}>
                Send rejection
              </Button>
              <Button variant="ghost" disabled={busy} onClick={() => setRejecting(false)}>
                Cancel
              </Button>
            </div>
          </div>
        )}

        {/* After a revision the user's steering bubble stays visible above the gate. */}
        {revised && !disclosing && <RevisionThread feedback={rev.latestFeedback} />}

        {/* Collapsed history of superseded plan versions (v1, …) once a v2+ is gated. */}
        {rev.priorPlans.length > 0 && !disclosing && (
          <details className="rounded-lg border border-edge bg-surface/50">
            <summary className="cursor-pointer px-3 py-2 text-xs text-muted">
              {rev.priorPlans.length === 1
                ? "Plan v1 · superseded"
                : `${rev.priorPlans.length} superseded plan versions`}
            </summary>
            <div className="space-y-3 border-t border-edge p-3">
              {rev.priorPlans.map((md, i) => (
                <div key={i} className="border-l-2 border-edge-strong pl-3 text-xs text-muted">
                  <div className="mb-1 font-semibold text-faint">Plan v{i + 1}</div>
                  <Markdown content={md} />
                </div>
              ))}
            </div>
          </details>
        )}
      </div>
    </div>
  );
}

// SeededPlanPanel (PRD #209 M5): a seeded run receives its plan from the user at
// create time and never enters awaiting_approval, so PlanPanel — the approval UI —
// never renders and run.plan_md would otherwise be UNREACHABLE on the run page. This
// read-only collapsible surfaces the seeded plan in any state (queued through
// terminal). It is deliberately NOT the gate: it carries no approve/reject/revise
// actions and does not read the feed, so it cannot be confused with the approval UI
// and does not touch the awaiting_approval path (Success Criterion 2 / anti-regression).
//
// The plan is UNTRUSTED input (user-authored, D5) but is size-capped and secret-scrubbed
// server-side at create time; it renders through the same hardened <Markdown> the gate
// uses for plan_md, never a raw HTML sink. Defaults OPEN: the whole point of the
// milestone is that this content was unreachable, so hiding it behind a click would
// re-hide it; the inner max-h scroll keeps a long plan from dominating the page.
export function SeededPlanPanel({ run }: { run: Run }) {
  // Belt-and-braces: a seeded run always carries a non-empty plan_md (M1 rejects an
  // empty/whitespace plan with a 422 at create time), but render nothing rather than an
  // empty box if a caller ever passes a run without one.
  if (!run.plan_md) return null;
  return (
    <details open className="overflow-hidden rounded-xl border border-edge bg-raised/40">
      <summary className="cursor-pointer px-4 py-3 text-sm font-semibold text-fg">
        Seeded plan
        <span className="ml-2 text-xs font-normal text-faint">
          you supplied this plan when starting the run — it is implemented without an approval gate
        </span>
      </summary>
      <div className="border-t border-edge p-4">
        <div className="max-h-96 overflow-auto rounded-lg border border-edge bg-surface p-3">
          <Markdown content={run.plan_md} />
        </div>
      </div>
    </details>
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

// Only issue / ci_fix runs are judged (the enqueue allowlist); a chat/judge/
// self_improve run never has a review, so the panel is hidden for those kinds.
const JUDGE_ELIGIBLE_KINDS = new Set(["issue", "ci_fix"]);

// JUDGE_STAT_K is the tile label class, mirroring RunUsage.tsx's K_CLASS so the judge
// strip reads identically to the run's own usage strip.
const JUDGE_STAT_K = "text-[10.5px] font-semibold uppercase tracking-[0.07em] text-faint";

function JudgeStat({ label, value, cost }: { label: string; value: string; cost?: boolean }) {
  return (
    <div className="bg-raised/75 px-3.5 py-2.5">
      <div className={JUDGE_STAT_K}>{label}</div>
      <div className={cx("mt-0.5 font-mono text-[17px] font-semibold tabular-nums", cost && "text-brand")}>{value}</div>
    </div>
  );
}

// JudgeUsageStrip is the judge run's OWN cost/time strip (PRD #69 M6, Decision 10): the
// tokens + duration + cost of the retrospective itself, surfaced on the reviewed run's
// panel. It mirrors RunUsagePanel's 4-tile confirmed strip (Tokens in · Tokens out ·
// Duration · Cost). Rendered ONLY when the judge posted a result frame (usage present);
// a pre-feature judge has no run_usage row and renders NOTHING here, never a fabricated 0.
// Duration = finished_at - started_at; absent when either stamp is missing.
function JudgeUsageStrip({ judgeRun }: { judgeRun: NonNullable<RunReview["judge_run"]> }) {
  const usage = judgeRun.usage;
  if (!usage) return null;
  const durationMs =
    judgeRun.started_at !== null && judgeRun.finished_at !== null
      ? new Date(judgeRun.finished_at).getTime() - new Date(judgeRun.started_at).getTime()
      : null;
  return (
    <div
      role="group"
      aria-label="Judge run cost and time"
      className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border border-edge bg-edge sm:grid-cols-4"
    >
      <JudgeStat label="Tokens in" value={formatTokens(usage.input_tokens)} />
      <JudgeStat label="Tokens out" value={formatTokens(usage.output_tokens)} />
      <JudgeStat label="Duration" value={durationMs !== null ? formatDuration(durationMs) : "—"} />
      {/* A $0 cost with real tokens is a subscription-auth run the SDK prices at $0
          (formatTokens.ts money convention) — render "—", never a misleading "$0.00". */}
      <JudgeStat label="Cost" value={usage.cost_usd > 0 ? formatCost(usage.cost_usd) : "—"} cost />
    </div>
  );
}

// JudgePanel is the run retrospective (PRD #46 M4): the LLM judge's verdict +
// structured recommendations, plus the "re-run judge" action. It fetches its own
// review (owner-or-admin scoped server-side) and, while a judge is in flight, polls a
// bounded number of times for the fresh verdict (a judge run finishes asynchronously).
// PRD #119: the same fetch also reports the ACTIVE judge run for this target, which is
// what lets the panel tell "never judged" from "a verdict is already coming" — and,
// because that answer is server truth rather than the local click flag, the in-flight
// state survives a reload and is visible to anyone viewing the run.
// All judge free text (summary, rationale, target) renders as escaped React text —
// never markdown/HTML — since it is untrusted judge/worker output (audit carry-forward).
export function JudgePanel({ run }: { run: Run }) {
  const [review, setReview] = useState<RunReview | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadErr, setLoadErr] = useState("");
  const [actionErr, setActionErr] = useState("");
  const [rerunning, setRerunning] = useState(false);
  const [queued, setQueued] = useState(false);
  // Server truth: the active judge run for this target, or null (PRD #119). Set from
  // EVERY getRunReview response — the mount fetch, the poll, and the 409 re-fetch — so
  // it can never go stale against a response the panel already has in hand.
  const [pendingJudge, setPendingJudge] = useState<PendingJudge | null>(null);
  // The caller's connected repos back the file-issue draft picker (PRD #68 M4). Fetched
  // once for the panel; a failure just leaves the picker empty (the draft still opens).
  const [repos, setRepos] = useState<Repo[]>([]);
  // The updated_at of the verdict CURRENTLY ON SCREEN (null when there is none). The poll
  // below compares each response against it to decide whether a NEWER verdict arrived and
  // the panel should swap to it.
  //
  // INVARIANT: this is written next to every setReview, and only there — the mount/409
  // fetch and the poll's swap. That pairing is the whole point. #119 made the poll start
  // from server truth rather than only from a click, so it can now begin on a mount that
  // ALREADY HAS a verdict; a baseline seeded only by the click would then be null while
  // the on-screen review has a real timestamp, and the very first tick would "detect" the
  // old, already-displayed verdict as newly landed.
  const baselineUpdatedAt = useRef<string | null>(null);

  const eligible = JUDGE_ELIGIBLE_KINDS.has(run.kind);

  const fetchReview = useCallback(async () => {
    try {
      const { review, pending_judge } = await api.getRunReview(run.id);
      setReview(review);
      // Seed the poll's baseline to the verdict this fetch just put on screen (see the
      // invariant at baselineUpdatedAt): without it a poll that starts from server truth
      // on a fresh mount reads a null baseline against a real updated_at.
      baselineUpdatedAt.current = review?.updated_at ?? null;
      // `?? null`, not the raw value: api and web are separate Deployments, so during a
      // rollout a web pod can be served by an api that predates the pending_judge key.
      // Absent destructures to undefined, and `undefined !== null` is true — every
      // `pendingJudge !== null` guard would then walk into `pendingJudge.state` and throw
      // during render. There is no ErrorBoundary in web/src, so that TypeError would blank
      // the whole app over a missing optional field. (api/internal/uzicli/client.go
      // handles the same skew on the CLI side.)
      setPendingJudge(pending_judge ?? null);
      setLoadErr("");
    } catch (e) {
      setLoadErr(e instanceof ApiError ? e.message : "Failed to load the review");
    } finally {
      setLoading(false);
    }
  }, [run.id]);

  useEffect(() => {
    if (!eligible) {
      setLoading(false);
      return;
    }
    fetchReview();
  }, [eligible, fetchReview]);

  // The file-issue picker lists every repo the caller has connected (PRD #68 Decision 4).
  // Best-effort: a failure (or a bare test double) just leaves the picker empty.
  useEffect(() => {
    if (!eligible) return;
    let alive = true;
    (async () => {
      try {
        const { repos } = await api.listRepos();
        if (alive) setRepos(repos);
      } catch {
        /* picker stays empty; the draft still opens */
      }
    })();
    return () => {
      alive = false;
    };
  }, [eligible]);

  // Filed links keyed by coordinate so a recommendation renders its filed row instead of
  // the File-issue button (PRD #68). Keyed (category, target) — the same coordinate the
  // link table uses — so re-judged siblings that collapse to one coordinate all resolve.
  const filedByCoord = useMemo(() => {
    const m = new Map<string, FiledIssue>();
    for (const f of review?.filed_issues ?? []) m.set(coordKey(f.category, f.target), f);
    return m;
  }, [review]);

  // Triage dispositions keyed by the SAME coordinate (PRD #94), mirroring filedByCoord
  // — so a row renders its status chip, its Undo control, and the server-computed stale
  // flag. Only coordinates with a current matching recommendation are in the DTO.
  const dispByCoord = useMemo(() => {
    const m = new Map<string, Disposition>();
    for (const d of review?.dispositions ?? []) m.set(coordKey(d.category, d.target), d);
    return m;
  }, [review]);

  // Panel-level collapse for the dismissed rows (default: show). The toggle label
  // reads the server-computed count DIRECTLY (PRD #94 Decision 7 — never re-derive a
  // triage aggregate in TS); dismissed is the top of the ladder, so this equals the
  // number of dismissed rows on screen.
  const [showDismissed, setShowDismissed] = useState(true);
  const dismissedCount = review?.triage?.dismissed ?? 0;

  // polling is the effect's ONLY dependency that can change while a judge is in flight,
  // and it is deliberately a BOOLEAN rather than `pendingJudge` itself. A judge moving
  // scheduled → running produces a new pendingJudge object on (almost) every tick; had
  // the effect depended on that object it would tear down and re-create the interval —
  // resetting the local `tries` counter each time, which turns the cap into "poll
  // forever" for exactly the judge that is making progress, and re-arming a 4s timer on
  // every response. Keying on "is anything pending at all" means the effect runs once
  // per in-flight episode: true on the first pending answer (or an optimistic click),
  // false when the last one clears, and never in between.
  const polling = queued || pendingJudge !== null;

  // Bounded background poll while a judge is in flight (local optimistic `queued` OR
  // server-truth pendingJudge — PRD #119 generalized this from the post-re-run-only poll).
  //
  // A landed verdict drives the SWAP; it does NOT stop the poll. The only stop conditions
  // are "the judge left the active set" and the cap. That split is forced by the API's
  // write ordering: PostReview (api/internal/workersvc/judge_review.go) opens with
  // authorizeJudgeTrace, which requires the calling worker to own a still-ACTIVE judge run
  // — so the review row is written BEFORE the judge run goes terminal, and the run only
  // leaves the active set on the worker's later completion report. A tick landing in that
  // window legitimately sees (fresh review, pending_judge still non-null); stopping there
  // would freeze a disabled "Judge running…" button and an in-flight note on top of a
  // verdict that had already arrived, with nothing left to ever clear them. That window is
  // the COMMON auto-judge path, not a race corner.
  //
  // Termination, given the swap no longer stops anything:
  //   • judge that dies with no verdict — pending_judge clears, no review ever moves;
  //     the cleared pending is what ends it, exactly as before;
  //   • verdict lands (first-ever or a re-judge) — the swap happens, then the completion
  //     report clears pending_judge a tick or two later and the poll ends;
  //   • the local `queued` window before the server reports a pending judge — the first
  //     response with pending_judge null ends it, same as before;
  //   • everything else — the 150-try cap.
  // Cap is 150 tries × 4s ≈ 10 minutes — a real judge takes minutes, and the old 15-try
  // (~1 min) cap gave up while the judge it was waiting on was still running.
  useEffect(() => {
    if (!polling) return;
    let tries = 0;
    const id = setInterval(async () => {
      tries += 1;
      let next: { review: RunReview | null; pending_judge: PendingJudge | null } | null = null;
      try {
        next = await api.getRunReview(run.id);
      } catch {
        // Swallowed: a transient failure mid-poll is not worth an error banner over a
        // panel that is already showing a correct in-flight state. The cap bounds it.
        next = null;
      }
      if (!next) {
        // The cap has to clear the interval on THIS path too, not just below. Reaching
        // it only via setQueued(false) does not stop the timer: `polling` stays true
        // while pendingJudge holds its last non-null value, so the effect never re-runs
        // and never re-runs its cleanup — a permanently-failing endpoint would be
        // polled every 4s until unmount.
        if (tries >= 150) {
          setQueued(false);
          clearInterval(id);
        }
        return;
      }
      // `?? null` for the rollout-skew reason documented in fetchReview: an api that
      // predates the key would otherwise put `undefined` into state and throw on render.
      const nextPending = next.pending_judge ?? null;
      setPendingJudge(nextPending);
      // A verdict NEWER than the one on screen: swap to it and advance the baseline in the
      // same step, so the invariant holds and later ticks re-serving that same verdict do
      // not read as another landing. Deliberately not a stop condition — see above.
      const landed =
        next.review !== null && next.review.updated_at !== baselineUpdatedAt.current
          ? next.review
          : null;
      if (landed !== null) {
        setReview(landed);
        baselineUpdatedAt.current = landed.updated_at;
      }
      if (nextPending === null || tries >= 150) {
        setQueued(false);
        clearInterval(id);
      }
    }, 4000);
    return () => clearInterval(id);
  }, [polling, run.id]);

  const rerun = async () => {
    setActionErr("");
    setRerunning(true);
    try {
      await api.rerunJudge(run.id);
      // No baseline write here on purpose. It used to live here, and being the ONLY writer
      // is what left the baseline null on any poll that did not start from a click. Now
      // that every setReview seeds it, `baselineUpdatedAt.current === review?.updated_at`
      // already holds at this point and re-writing it would be a no-op — one that invites
      // the same bug back by making "the click seeds the baseline" look load-bearing.
      setQueued(true);
    } catch (e) {
      // The 409 backstop (PRD #119). The button is disabled whenever the last fetch saw
      // a pending judge, but that answer is POINT-IN-TIME: an auto-judge can enqueue in
      // the gap between the fetch and the click (TOCTOU), so a click can still race into
      // the one-active-judge-per-target index. The window shrinks; it does not close.
      // On that 409 the click is ABSORBED — re-fetch, converge to the pending state, no
      // error banner — because the user asked for a judge and a judge is running: that
      // is a success they are being shown, not a failure.
      //
      // The route returns 409 for a SECOND, unrelated reason: ErrJudgeDisabled ("run
      // judging is disabled", handler/judge.go). That one must never be swallowed — a
      // user who turned the judge off has to see why nothing happened. The wire carries
      // no error code to discriminate on, only the message, so we match the
      // already-active message and let EVERYTHING else fall through to today's Alert.
      // The match is deliberately in that direction: if the server ever rewords the
      // already-active message, this degrades to showing the error (the pre-#119
      // behaviour), never to silently eating "judging is disabled".
      if (e instanceof ApiError && e.status === 409 && /already in progress/i.test(e.message)) {
        await fetchReview();
      } else {
        setActionErr(e instanceof ApiError ? e.message : "Could not re-run the judge");
      }
    } finally {
      setRerunning(false);
    }
  };

  if (!eligible) return null;
  if (loading) {
    return <Card className="animate-pulse p-4 text-sm text-faint">Loading review…</Card>;
  }

  // Label precedence (PRD #119): this tab's own in-flight POST first, then the
  // server's pending judge (whoever started it, whenever), then today's labels. A
  // pending judge also DISABLES the button — the click's only outcome would be the
  // 409 the panel now explains instead of provoking.
  const pendingLabel =
    pendingJudge === null ? null : pendingJudge.state === "scheduled" ? "Judge scheduled" : "Judge running…";
  const rerunLabel = pendingLabel ?? (review ? "Re-run judge" : "Run judge");
  const rerunButton = (
    <Button
      variant="secondary"
      size="sm"
      disabled={rerunning || queued || pendingJudge !== null}
      onClick={rerun}
    >
      {rerunning ? "Re-queuing…" : rerunLabel}
    </Button>
  );

  // The in-flight note above the panel body. Its render condition is unchanged
  // (`pendingJudge !== null ? review !== null : queued` — see the comment at its render
  // site); what is state-driven is the WORDING, and the two arms deliberately say
  // different things because they know different facts:
  //   • armed from pendingJudge — SERVER truth. All it says is that a judge run for this
  //     target is in the active set. It does NOT say who enqueued it: an auto-judge fires
  //     at the terminal transition, and another admin viewing the same run gets the same
  //     answer. "Judge re-queued" would assert "you re-queued this" to every viewer of a
  //     judge nobody in this tab started, so this arm is neutral and keyed off the state
  //     the server reported — which also puts it in the same tense as the button beside
  //     it (scheduled / running) instead of contradicting it.
  //   • armed from `queued` — this tab's own optimistic flag, set in the one place it can
  //     be: immediately after THIS viewer's rerunJudge POST resolved, before the next
  //     fetch has returned. There "re-queued" is exactly true, so it is kept verbatim.
  // Both are null when nothing is in flight, which is what suppresses the note.
  const inFlightNote =
    pendingJudge !== null
      ? review !== null
        ? pendingJudge.state === "scheduled"
          ? "A judge is scheduled for this run — the new verdict will appear here when it finishes."
          : "A judge is running for this run — the new verdict will appear here when it finishes."
        : null
      : queued
        ? "Judge re-queued — the new verdict will appear here when it finishes."
        : null;
  // The pending empty-state copy, hoisted so the live region below can announce the same
  // sentence the sighted user reads rather than a second, drifting one.
  const pendingEmptyCopy =
    review !== null || pendingJudge === null
      ? null
      : pendingJudge.state === "scheduled"
        ? "Judge scheduled — the verdict will appear here when it finishes."
        : "Judge in progress…";
  // Mutually exclusive by construction: inFlightNote needs a review (or no pendingJudge
  // at all), pendingEmptyCopy needs no review AND a pendingJudge.
  const judgeAnnounce = inFlightNote ?? pendingEmptyCopy ?? "";

  return (
    <Card className="space-y-4 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Run review</h2>
          {review && <Badge tone={verdictTone(review.verdict)}>{verdictLabel(review.verdict)}</Badge>}
          {review?.status === "failed" && (
            <Badge tone="neutral" title="The judge model call failed; the deterministic findings below still landed.">
              judge incomplete
            </Badge>
          )}
          {/* Issue #124: worker-reported, scrubbed at ingest by sanitizeSelfReported, so
              rows written before that strip learned Cf still carry it. NOTE this is the
              REVIEW DTO's judge_model (api.ts:1085), not the admin SETTING of the same name
              (api.ts:428) — that one is edited in a controlled input and stripping it would
              filter an admin's own keystrokes, the mistake the draft-seed ruling argued
              against. */}
          {review?.judge_model && (
            <span className="text-xs text-faint">via {stripUnsafeChars(review.judge_model)}</span>
          )}
        </div>
        {rerunButton}
      </div>

      {actionErr && <Alert message={actionErr} />}
      {loadErr && <Alert message={loadErr} />}
      {/* ALWAYS MOUNTED, empty until a judge is in flight — the same convention as the
          park region in RunView and the triage rows' announce span below, and mounted
          unconditionally for the same reason: a live region that appears at the same
          moment as its text is not reliably announced.
          It is needed here because NOTHING else in this panel reaches a screen reader
          when the judge state moves. The state changes with no user action at all (the
          bounded poll swaps scheduled → running, and the verdict lands minutes later),
          and the control that carries it is a DISABLED button — removed from the tab
          order, so a keyboard/SR user cannot even land on it to hear the new label. */}
      <span className="sr-only" role="status" aria-live="polite">
        {judgeAnnounce}
      </span>
      {/* Armed from server truth OR the local click (PRD #119): `queued` alone only
          existed in the tab that pressed the button, so a reload mid-judge lost the
          note and re-offered the action. pendingJudge is the same fact read from the
          server, so the in-flight state now shows on any load, to any viewer.
          Suppressed in exactly one case — a pending judge with NO review — because the
          empty state below already says a verdict is coming, and this line would sit
          directly above it saying nearly the same sentence. (The wording of each arm is
          decided at inFlightNote above, where the two arms' different knowledge is.) */}
      {inFlightNote !== null && <p className="text-xs text-info">{inFlightNote}</p>}

      {!review ? (
        // The empty state is the whole point of PRD #119: "no verdict" has two causes
        // and they call for opposite affordances. With a pending judge the copy says a
        // verdict is coming (and the button above is disabled); without one it is the
        // unchanged never-judged copy next to a live Run-judge button.
        pendingEmptyCopy !== null ? (
          // items-START, not items-center: the spinner is one line tall and the sentence
          // is two at a narrow viewport (measured at 375px), where centering floated the
          // spinner into the gap between the lines — 10px below the text it belongs to.
          // No leading override is needed to keep it right on ONE line: both flex items
          // inherit the paragraph's text-sm leading (1.25rem), so their line boxes are
          // the same height and their first baselines already coincide.
          <p className="flex items-start gap-2 text-sm text-faint">
            <Spinner />
            {pendingEmptyCopy}
          </p>
        ) : (
          <p className="text-sm text-faint">
            This run hasn't been judged yet. Running the judge reviews the run on your Anthropic token.
          </p>
        )
      ) : (
        <>
          {/* summary_md and rationale_md below are UNTRUSTED judge/worker output. They now
              render through the SAME hardened <Markdown> component that plan_md uses on this
              page (see the plan_md sites above and components/Markdown.tsx), so this surface
              is no worse than the already-shipped plan_md one. That component is hardened for
              exactly this untrusted case: NO rehype-raw, so raw HTML (e.g. <script>, <img
              onerror>) stays inert text; react-markdown's urlTransform plus our own
              schemeIsDangerous strip javascript:/data:/file: URLs; links are forced external
              (target="_blank" rel="noopener noreferrer"); images are size-capped.
              The issue #124 Cf/bidi-override strip is now CENTRALIZED inside <Markdown>
              itself (see components/Markdown.tsx), so it applies to this surface by
              construction — the stripUnsafeChars wrap kept below is therefore redundant but
              harmless: stripUnsafeChars is idempotent, so the second pass is a no-op, and it
              keeps this value identical to the non-Markdown rec.target/judge_model sinks that
              still strip per-site. Markdown syntax chars (`*`/`` ` ``/`#`/`-`/`[]()`) are not
              control chars, so the strip preserves them while removing bidi overrides. Rows
              written before the ingest-side Cf strip landed still arrive carrying bidi
              overrides, which is why the render still strips. See lib/safeText.ts for why the
              strip lives at display time and not at the API boundary.
              rec.target (the <code> coordinate just below) DELIBERATELY stays inert escaped
              plaintext — it is a coordinate the page posts back, not prose, and must NOT
              become a markdown/link sink.
              If a markdown/HTML renderer is ever swapped in here, do NOT add rehype-raw or
              relax schemeIsDangerous. */}
          {review.summary_md.trim() !== "" && (
            <div className="judge-prose">
              <Markdown content={stripUnsafeChars(review.summary_md)} />
            </div>
          )}

          {/* Judge run cost/time strip (PRD #69 M6): the retrospective's OWN tokens,
              duration and cost. Rendered only when the judge posted a result frame (a
              run_usage row exists); absent for a pre-feature judge, never a fabricated 0. */}
          {review.judge_run?.usage && <JudgeUsageStrip judgeRun={review.judge_run} />}

          {/* Triage bar (PRD #94): the server-bucketed per-review counts + a segmented
              meter, rendered DIRECTLY from review.triage — never re-derived from the
              rows on screen, so it agrees with `uzi review show` and the global strip. */}
          <TriageSummary
            triage={review.triage}
            title="Triage"
            aside={`${review.triage.filed + review.triage.done + review.triage.dismissed} of ${review.triage.total} handled`}
          />

          {review.recommendations.length > 0 ? (
            <ul className="space-y-2">
              {review.recommendations.map((rec) => {
                const disp = dispByCoord.get(coordKey(rec.category, rec.target));
                const filed = filedByCoord.get(coordKey(rec.category, rec.target));
                // Collapse-dismissed: hide a dismissed row while the toggle is off.
                if (!showDismissed && disp?.status === "dismissed") return null;
                return (
                  <li key={rec.id} className="rounded-lg border border-edge bg-raised/40 px-3 py-2.5">
                    <div className="flex flex-wrap items-center gap-2">
                      <Badge tone="info">{recommendationLabel(rec.category)}</Badge>
                      {rec.target.trim() !== "" && (
                        <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">
                          {stripUnsafeChars(rec.target)}
                        </code>
                      )}
                      {rec.confidence && <span className="text-xs text-faint">{rec.confidence} confidence</span>}
                      <span className="ml-auto">
                        <DispositionChip disp={disp} filedSettled={filed !== undefined} />
                      </span>
                    </div>
                    {rec.rationale_md.trim() !== "" && (
                      <div className="judge-prose mt-1.5">
                        <Markdown content={stripUnsafeChars(rec.rationale_md)} />
                      </div>
                    )}
                    {/* A settled disposition (done/dismissed) hides the create-issue
                        affordance (File / draft) but NOT an already-filed link: a rec that
                        was filed and then marked done keeps both facts visible (Resolved Q:
                        "you can file then later mark done"). RecommendationFiler renders the
                        filed-issue link regardless, and `actionHidden` suppresses only the
                        create action once a disposition exists. */}
                    <RecommendationFiler
                      runId={run.id}
                      rec={rec}
                      filed={filed}
                      reviewUpdatedAt={review.updated_at}
                      repos={repos}
                      actionHidden={disp !== undefined}
                    />
                    <DispositionControls
                      runId={run.id}
                      recId={rec.id}
                      disp={disp}
                      onChanged={fetchReview}
                      onError={setActionErr}
                    />
                  </li>
                );
              })}
              {dismissedCount > 0 && (
                <li>
                  <button
                    type="button"
                    onClick={() => setShowDismissed((v) => !v)}
                    aria-expanded={showDismissed}
                    className="inline-flex items-center gap-1 text-xs font-medium text-faint underline underline-offset-2 transition-colors hover:text-fg"
                  >
                    {showDismissed ? "Hide" : "Show"} dismissed ({dismissedCount})
                  </button>
                </li>
              )}
            </ul>
          ) : (
            <p className="text-sm text-faint">No recommendations — the judge found nothing to change.</p>
          )}
        </>
      )}
    </Card>
  );
}

// JustFiled is the local filed state after a successful Create click (mock C), so the row
// flips without re-fetching the review. warning carries a created-with-warning message
// (the issue exists on the forge but its local link/cache could not be settled).
type JustFiled = { iid: number; web_url: string; warning?: string };

// RecommendationFiler is the per-recommendation File-issue affordance (PRD #68 M4): the
// idle button (mock A), the ProposalCard-shaped inline draft (mock B / no-default D /
// forge-error E), and the filed row (mock C, from a server link OR a just-filed local
// one). Every draft field is INERT text like ProposalCard — title/description render in an
// editable control, never through Markdown, and the load-bearing sanitizer re-runs
// server-side at the POST. The draft shows RAW markdown (no rendered preview) by design.
function RecommendationFiler({
  runId,
  rec,
  filed,
  reviewUpdatedAt,
  repos,
  actionHidden = false,
}: {
  runId: string;
  rec: ReviewRecommendation;
  filed?: FiledIssue;
  reviewUpdatedAt: string;
  repos: Repo[];
  // A disposed row suppresses the create-issue affordance (the "File issue" button and
  // its draft) while STILL showing an existing filed link below — so a filed-then-done
  // rec keeps its clickable issue link but offers no way to file a second issue.
  actionHidden?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<IssueDraft | null>(null);
  const [loadingDraft, setLoadingDraft] = useState(false);
  const [draftErr, setDraftErr] = useState("");
  const [repoId, setRepoId] = useState("");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [busy, setBusy] = useState(false);
  const [fileErr, setFileErr] = useState("");
  const [local, setLocal] = useState<JustFiled | null>(null);

  // Filed already (server link) or just now (local) → the filed row (mock C). A server
  // link is stale when it predates the current review revision (filed_at < updated_at:
  // "filed for an earlier version"); a just-filed local link is by definition current.
  if (filed || local) {
    const iid = local ? local.iid : filed!.issue_iid;
    const url = local ? local.web_url : filed!.issue_url;
    const stale = !local && filed ? new Date(filed.filed_at) < new Date(reviewUpdatedAt) : false;
    return (
      <div className="mt-2.5 rounded-lg border border-ok/40 bg-ok/10 px-3 py-2 text-sm text-ok">
        <span className="font-medium">Issue created.</span>{" "}
        {isHttpsUrl(url) ? (
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 font-medium underline underline-offset-2 hover:text-ok"
          >
            #{iid} <ExternalLinkIcon />
          </a>
        ) : (
          <span className="font-medium">#{iid}</span>
        )}
        {local?.warning ? (
          <p className="mt-1 text-xs text-warn">{local.warning}</p>
        ) : stale ? (
          <p className="mt-1 text-xs text-faint">
            Filed for an earlier version of this recommendation — re-running the judge changed it since.
          </p>
        ) : (
          <span className="text-ok/80"> — open it on the board to start a run.</span>
        )}
      </div>
    );
  }

  // Past this point is the create-issue affordance. A disposed row with no filed link
  // renders nothing here (the filed row above already returned when a link exists).
  if (actionHidden) return null;

  const openDraft = async () => {
    setOpen(true);
    setLoadingDraft(true);
    setDraftErr("");
    try {
      const { draft } = await api.getIssueDraft(runId, rec.id);
      setDraft(draft);
      setRepoId(draft.default_repo_id);
      // Issue #124 / LOW-1: sanitize what UZI supplies, leave what the USER types alone.
      // These are controlled components, so the state IS what gets POSTed — filtering
      // `value=` would silently rewrite the user's own typing. The SEED is uzi-supplied
      // (a server draft templated from `rec.target` + `rationale_md`), so it is exactly
      // the boundary that may be cleaned.
      //
      // The ingest strip means new rows can no longer carry Cf at all; this covers reviews
      // stored BEFORE it, the same argument the renderer-side strip rests on. It is also
      // on the SAFE side of the quick-action ordering trap: measured, a Cf strip applied
      // BEFORE the server's StripUnfencedSlashLines makes `<ZWSP>/label ~backdoor` into a
      // line the slash-pass then DROPS, whereas stripping AFTER that pass would turn an
      // inert line into a live GitLab quick action.
      setTitle(stripUnsafeChars(draft.title));
      setDescription(stripUnsafeChars(draft.description));
    } catch (e) {
      setDraftErr(e instanceof ApiError ? e.message : "Could not load the draft");
    } finally {
      setLoadingDraft(false);
    }
  };

  const create = async () => {
    setFileErr("");
    setBusy(true);
    try {
      const { issue, warning } = await api.fileIssue(runId, rec.id, { repo_id: repoId, title, description });
      setLocal({ iid: issue.iid, web_url: issue.web_url, warning });
    } catch (e) {
      // Forge rejected the write (mock E): the draft stays open with its edits intact.
      setFileErr(e instanceof ApiError ? e.message : "Could not file the issue");
    } finally {
      setBusy(false);
    }
  };

  if (!open) {
    return (
      <div className="mt-2.5">
        <Button size="sm" onClick={openDraft}>
          File issue
        </Button>
      </div>
    );
  }

  return (
    <div className="mt-2.5 overflow-hidden rounded-xl border border-brand/40 bg-brand/[0.06]">
      <div className="flex items-center justify-between gap-2 border-b border-brand/20 bg-brand/10 px-3 py-2">
        <span className="inline-flex items-center gap-1.5 text-xs font-semibold text-brand">
          <span aria-hidden="true">
            <FileTextIcon />
          </span>
          Draft issue
        </span>
        <Badge tone="brand">needs your review</Badge>
      </div>

      <div className="space-y-3 px-3 py-3">
        {loadingDraft && (
          <p role="status" className="text-sm text-faint">
            Loading draft…
          </p>
        )}
        {draftErr && <Alert message={draftErr} />}
        {/* A draft-load failure must not trap the card: with no draft there is neither the
            Cancel below (inside the draft guard) nor the File-issue button (open===true),
            so offer Retry + Cancel here. */}
        {draftErr && !draft && (
          <div className="flex flex-wrap gap-2">
            <Button size="sm" onClick={openDraft}>
              Retry
            </Button>
            <Button size="sm" variant="secondary" onClick={() => setOpen(false)}>
              Cancel
            </Button>
          </div>
        )}
        {draft && (
          <>
            {/* Provenance (Decision 8): whose worker produced this (attacker-influencable)
                text — prominent (boxed + labeled) so an admin filing another user's review
                notices whose text they are about to publish. */}
            {draft.provenance && (
              <div className="rounded-md border border-edge bg-raised/50 px-2.5 py-1.5 text-xs text-muted">
                <span className="font-semibold text-fg">Source:</span> {draft.provenance}
              </div>
            )}
            {fileErr && <Alert message={fileErr} />}

            <div className="space-y-1">
              <label className="block text-xs text-muted">Repo</label>
              <Select value={repoId} onChange={(e) => setRepoId(e.target.value)}>
                <option value="">Select a repo…</option>
                {repos.map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.path_with_namespace}
                  </option>
                ))}
              </Select>
              {draft.default_note && (
                <p
                  role="status"
                  className={cx(
                    "text-xs",
                    repoId
                      ? "text-faint"
                      : "rounded-md border border-info/40 bg-info/10 px-2.5 py-1.5 text-info",
                  )}
                >
                  {draft.default_note}
                </p>
              )}
            </div>

            {/* Every field below is inert text (never Markdown): the title/description are
                edited raw, and the server re-sanitizes at the POST boundary. */}
            <div className="space-y-1">
              <label className="block text-xs text-muted">Title</label>
              <Input value={title} onChange={(e) => setTitle(e.target.value)} />
            </div>

            <div className="space-y-1">
              <label className="block text-xs text-muted">Description</label>
              <Textarea
                rows={10}
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                className="max-h-72 font-mono text-xs"
              />
            </div>

            <div className="space-y-1">
              <label className="block text-xs text-muted">Labels</label>
              <div className="flex flex-wrap gap-1">
                {draft.labels.map((l) => (
                  <Badge key={l} tone="neutral">
                    {l}
                  </Badge>
                ))}
              </div>
              <p className="text-xs text-faint">
                Lands on the board and is startable without a PRD file. No autopilot label — nothing runs until you click
                Start.
              </p>
            </div>

            <div className="flex flex-wrap gap-2 pt-0.5">
              <Button size="sm" disabled={busy || !repoId || title.trim() === ""} onClick={create}>
                Create issue
              </Button>
              <Button size="sm" variant="secondary" disabled={busy} onClick={() => setOpen(false)}>
                Cancel
              </Button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}

// DispositionChip renders a recommendation's triage status by the D#2 precedence
// ladder — disposition (done/dismissed) wins over a settled filed link, which wins
// over the open "To do" default. Tones mirror the mockup: a not_an_issue (false
// positive) reads danger and reserves the only warm/red chip; a wont_do reads neutral
// grey (a valid-but-parked call is not a warning), done reads ok, filed reads info.
function DispositionChip({ disp, filedSettled }: { disp?: Disposition; filedSettled: boolean }) {
  if (disp?.status === "dismissed") {
    return disp.reason === "not_an_issue" ? (
      <Badge tone="danger">Dismissed · Not an issue</Badge>
    ) : (
      <Badge tone="neutral">Dismissed · Won't do</Badge>
    );
  }
  // The ✓ is decorative — aria-hidden so a screen reader reads just "Done", not
  // "check mark Done".
  if (disp?.status === "done")
    return (
      <Badge tone="ok">
        <span aria-hidden="true">✓</span> Done
      </Badge>
    );
  if (filedSettled) return <Badge tone="info">Filed</Badge>;
  return <Badge tone="neutral">To do</Badge>;
}

// resolvedAgo renders a disposition's set_at as a coarse "resolved Xh ago". The panel
// shows only a relative time, never the actor — under owner-only the setter is always
// the owner (D#6). Guards an unparseable timestamp.
function resolvedAgo(setAt: string): string {
  const t = Date.parse(setAt);
  if (!Number.isFinite(t)) return "resolved";
  return `resolved ${formatElapsed(Date.now() - t)} ago`;
}

// DispositionControls is the per-row triage affordance (PRD #94). With no disposition
// it offers Mark done + Dismiss ▾ (Won't do / Not an issue); with one it shows the
// server-computed stale flag and an Undo. EVERY mutation refetches the review
// (onChanged) so the triage bar, chips, and stale flag re-read from the server — the
// panel never re-derives triage state in TS.
function DispositionControls({
  runId,
  recId,
  disp,
  onChanged,
  onError,
}: {
  runId: string;
  recId: string;
  disp?: Disposition;
  onChanged: () => Promise<void>;
  onError: (msg: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  // A polite sr-only live region announces the mutation result; it lives OUTSIDE the
  // disp/no-disp branch so the branch swap after a mutation doesn't drop the message.
  const [announce, setAnnounce] = useState("");

  // Refs for a11y. The ui Button is a plain (non-forwardRef) component, so focus targets
  // that are Buttons (Mark done, Dismiss trigger) are located by querySelector off a stable
  // container ref rather than a direct ref; Undo is a raw <button> and takes a ref directly.
  // menuWrapRef also backs the outside-click hit test. rootRef is the no-disp branch root.
  const menuWrapRef = useRef<HTMLDivElement>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const undoRef = useRef<HTMLButtonElement>(null);
  // Set true just before the refetch so the disp-transition effect below knows the change
  // was user-initiated (skips focus-stealing on the initial mount / passive re-renders).
  const focusAfterMutation = useRef(false);

  // Escape closes the menu (focus back to the Dismiss trigger — the first button in the
  // wrapper); a pointerdown outside the wrapper closes it too. Wired only while open, torn
  // down on close/unmount.
  useEffect(() => {
    if (!menuOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setMenuOpen(false);
        menuWrapRef.current?.querySelector<HTMLElement>("button")?.focus();
      }
    };
    const onPointerDown = (e: Event) => {
      if (menuWrapRef.current && !menuWrapRef.current.contains(e.target as Node)) {
        setMenuOpen(false);
      }
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("pointerdown", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("pointerdown", onPointerDown);
    };
  }, [menuOpen]);

  // After a mutation + refetch re-renders this row into the other branch, move focus to the
  // successor control that just mounted (disp → Undo; no disp → Mark done, the first button
  // in the root). Keyed on disp AND busy: the successor button is `disabled={busy}` and busy
  // only clears in the finally AFTER the refetch, so we defer the focus until busy drops
  // (a disabled element ignores .focus()). The armed flag survives the intervening renders.
  useEffect(() => {
    if (!focusAfterMutation.current || busy) return;
    focusAfterMutation.current = false;
    if (disp) undoRef.current?.focus();
    else rootRef.current?.querySelector<HTMLElement>("button")?.focus();
  }, [disp, busy]);

  const act = async (fn: () => Promise<unknown>, message: string) => {
    onError("");
    setBusy(true);
    try {
      await fn();
      // Arm the focus move BEFORE the refetch so the disp-transition effect (fired by the
      // parent's re-render on refetch) sees the flag set.
      focusAfterMutation.current = true;
      setAnnounce(message);
      await onChanged();
    } catch (e) {
      focusAfterMutation.current = false;
      onError(e instanceof ApiError ? e.message : "Could not update the disposition");
    } finally {
      setBusy(false);
      setMenuOpen(false);
    }
  };

  // The live region is shared by both branches so an announcement survives the branch swap.
  const liveRegion = (
    <span className="sr-only" role="status" aria-live="polite">
      {announce}
    </span>
  );

  if (disp) {
    return (
      <div className="mt-2 flex flex-wrap items-center gap-2 text-xs">
        {liveRegion}
        <span className="text-faint">{resolvedAgo(disp.set_at)}</span>
        {disp.stale && (
          <Badge
            tone="warning"
            title="The judge re-ran and this recommendation's rationale changed since you resolved it."
          >
            recommendation changed since you resolved
          </Badge>
        )}
        <button
          type="button"
          ref={undoRef}
          disabled={busy}
          onClick={() => act(() => api.deleteDisposition(runId, recId), "Disposition undone")}
          className="font-medium text-faint underline underline-offset-2 transition-colors hover:text-fg disabled:opacity-50"
        >
          Undo
        </button>
      </div>
    );
  }

  return (
    <div ref={rootRef} className="mt-2 flex flex-wrap items-center gap-2">
      {liveRegion}
      <Button
        size="sm"
        variant="secondary"
        disabled={busy}
        onClick={() => act(() => api.setDisposition(runId, recId, "done"), "Marked done")}
      >
        Mark done
      </Button>
      <div className="relative" ref={menuWrapRef}>
        <Button
          size="sm"
          variant="secondary"
          disabled={busy}
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen((o) => !o)}
        >
          Dismiss ▾
        </Button>
        {menuOpen && (
          <div
            role="menu"
            className="absolute z-10 mt-1 w-56 rounded-lg border border-edge-strong bg-surface p-1 shadow-lg"
          >
            <button
              type="button"
              role="menuitem"
              disabled={busy}
              onClick={() => act(() => api.setDisposition(runId, recId, "dismissed", "wont_do"), "Dismissed — won't do")}
              className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised disabled:opacity-50"
            >
              Won't do
              <span className="text-xs text-faint">Valid, but not worth acting on</span>
            </button>
            <button
              type="button"
              role="menuitem"
              disabled={busy}
              onClick={() =>
                act(() => api.setDisposition(runId, recId, "dismissed", "not_an_issue"), "Dismissed — not an issue")
              }
              className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised disabled:opacity-50"
            >
              Not an issue
              <span className="text-xs text-faint">False positive — the judge got it wrong</span>
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

// TriageSummary renders a TriageCounts bundle DIRECTLY — a segmented meter plus the
// counts line (to do / filed / done / dismissed, with the false-positive sub-count).
// It never derives a number itself: the same server bundle backs the per-review bar,
// the global strip, and `uzi review show`, so they cannot disagree (D#7/D#8). Exported
// so RunsList's global strip renders the identical visual from getJudgeStats.
export function TriageSummary({
  triage,
  title,
  aside,
  className = "",
}: {
  triage: TriageCounts;
  title: string;
  aside?: ReactNode;
  className?: string;
}) {
  return (
    <div className={cx("rounded-lg border border-edge bg-ink/40 p-3", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-xs font-semibold uppercase tracking-wider text-faint">{title}</h3>
        {aside != null && <span className="ml-auto text-xs text-faint">{aside}</span>}
      </div>
      <TriageMeter triage={triage} />
      <div className="mt-3 flex flex-wrap items-baseline gap-x-4 gap-y-1 text-xs">
        <TriageCount dotClass="bg-warn" n={triage.todo} label="to do" />
        <TriageCount dotClass="bg-info" n={triage.filed} label="filed" />
        <TriageCount dotClass="bg-ok" n={triage.done} label="done" />
        <TriageCount dotClass="bg-muted" n={triage.dismissed} label="dismissed" />
        {triage.dismissed > 0 && (
          <span className="text-faint">
            {triage.false_positives} of {triage.dismissed} dismissed{" "}
            {triage.dismissed === 1 ? "was a false positive" : "were false positives"}
          </span>
        )}
      </div>
    </div>
  );
}

function TriageCount({ dotClass, n, label }: { dotClass: string; n: number; label: string }) {
  return (
    <span className="inline-flex items-baseline gap-1.5">
      <span aria-hidden="true" className={cx("inline-block h-2 w-2 self-center rounded-full", dotClass)} />
      <b className="text-sm font-semibold tabular-nums text-fg">{n}</b>
      <span className="uppercase tracking-wide text-faint">{label}</span>
    </span>
  );
}

// TriageMeter is the segmented bar: one span per non-zero bucket, width proportional
// to its share of the total, tinted with the same tone tokens as the counts dots. A
// total of 0 yields an empty track. `todo` takes bg-warn (amber, the "needs attention"
// token) so the actionable backlog leads the eye, leaving grey (bg-muted) to the single
// inert bucket, dismissed — the two used to be near-identical slate greys (#243).
function TriageMeter({ triage }: { triage: TriageCounts }) {
  const total = triage.total;
  const seg = (n: number, cls: string, key: string) =>
    n > 0 && total > 0 ? (
      <span key={key} className={cx("h-full", cls)} style={{ width: `${(n / total) * 100}%` }} />
    ) : null;
  return (
    <div className="mt-2 flex h-2 overflow-hidden rounded-full bg-raised" aria-hidden="true">
      {seg(triage.todo, "bg-warn", "todo")}
      {seg(triage.filed, "bg-info", "filed")}
      {seg(triage.done, "bg-ok", "done")}
      {seg(triage.dismissed, "bg-muted", "dismissed")}
    </div>
  );
}
