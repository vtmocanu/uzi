import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  api,
  ApiError,
  isHttpsUrl,
  type Disposition,
  type FiledIssue,
  type IssueDraft,
  type PendingJudge,
  type Repo,
  type ReviewRecommendation,
  type Run,
  type RunReview,
  type TriageCounts,
} from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { coordKey, recommendationLabel, verdictLabel, verdictTone } from "../../lib/judge";
import { useDemoMode } from "../../lib/demoMode";
import { maskRepoPath } from "../../lib/demoMask";
import { isJudgeEligible } from "../../lib/runKind";
import { stripUnsafeChars } from "../../lib/safeText";
import { useAsyncData } from "../../lib/useAsyncData";
import { formatElapsed } from "../../lib/runBadge";
import { formatDuration } from "../../components/RunEvent";
import { formatTokens, formatCost } from "../../lib/formatTokens";
import { Markdown } from "../../components/Markdown";
import { Alert, Badge, Button, Card, Input, Select, Spinner, Textarea, cx } from "../../components/ui";
import { ExternalLinkIcon, FileTextIcon } from "../../components/icons";

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
//
// The poll's stop cap (PRD #119: 150 tries × 4s ≈ 10 min). Exposed as an injectable prop
// so tests can drive the exact stop boundary in a handful of ticks instead of 149 real
// event-loop turns (issue #227); production never passes it, so the default always holds.
export const JUDGE_POLL_MAX_TRIES = 150;

export function JudgePanel({
  run,
  pollMaxTries = JUDGE_POLL_MAX_TRIES,
}: {
  run: Run;
  pollMaxTries?: number;
}) {
  const [review, setReview] = useState<RunReview | null>(null);
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

  // Only issue / ci_fix runs are judged (the enqueue allowlist, via isJudgeEligible);
  // a chat/judge/self_improve run never has a review, so the panel is hidden for those.
  const eligible = isJudgeEligible(run.kind);

  // The mount load, via useAsyncData: enabled:eligible reproduces the old effect's
  // `if (!eligible) { setLoading(false); return; }` (eligible is derived from run.kind,
  // stable per mount). review/pendingJudge/baselineUpdatedAt are ALSO written by the poll
  // and the 409 re-fetch, so they stay local and are set as side effects here (never
  // bundled into the hook's data). The hook clears its error on success, so no explicit
  // clear is needed; the old catch's fallback becomes `fallback`.
  const {
    loading,
    error: loadErr,
    reload: fetchReview,
  } = useAsyncData(
    async ({ isCurrent }) => {
      const { review, pending_judge } = await api.getRunReview(run.id);
      if (!isCurrent()) return;
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
      // the whole app over a missing optional field. (api/internal/uzicli/client_runs.go
      // handles the same skew on the CLI side.)
      setPendingJudge(pending_judge ?? null);
    },
    [run.id],
    { enabled: eligible, fallback: "Failed to load the review" },
  );

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
  // Cap is JUDGE_POLL_MAX_TRIES (150) tries × 4s ≈ 10 minutes — a real judge takes minutes,
  // and the old 15-try (~1 min) cap gave up while the judge it was waiting on was still running.
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
        if (tries >= pollMaxTries) {
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
      if (nextPending === null || tries >= pollMaxTries) {
        setQueued(false);
        clearInterval(id);
      }
    }, 4000);
    return () => clearInterval(id);
  }, [polling, run.id, pollMaxTries]);

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
        setActionErr(errorMessage(e, "Could not re-run the judge"));
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
  const demo = useDemoMode();
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
      setDraftErr(errorMessage(e, "Could not load the draft"));
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
      setFileErr(errorMessage(e, "Could not file the issue"));
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
                    {maskRepoPath(r.path_with_namespace, demo)}
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
      onError(errorMessage(e, "Could not update the disposition"));
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
