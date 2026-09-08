import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  api,
  ApiError,
  type CreatedIssue,
  type Disposition,
  type FiledIssue,
  type PendingJudge,
  type Repo,
  type ReviewRecommendation,
  type Run,
  type RunReview,
  type TriageCounts,
} from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { coordKey, recommendationLabel, verdictLabel, verdictTone } from "../../lib/judge";
import { isJudgeEligible } from "../../lib/runKind";
import { stripUnsafeChars } from "../../lib/safeText";
import { useAsyncData } from "../../lib/useAsyncData";
import { formatDuration } from "../../components/RunEvent";
import { formatTokens, formatCost } from "../../lib/formatTokens";
import { Markdown } from "../../components/Markdown";
import { Alert, Badge, Button, Card, Spinner, cx } from "../../components/ui";
import { TriageActions } from "../../components/triage/TriageActions";
import { TriageDisposedRow } from "../../components/triage/TriageDisposedRow";
import { TriageStateChip } from "../../components/triage/TriageStateChip";
import { IssueDraftCard, type IssueDraftSeed, type IssueDraftValues } from "../../components/triage/IssueDraftCard";

// STALE_FILED_WARNING is the one line that survives the deleted "Issue created." box: a filed
// link is stale when it predates the current review revision (the judge re-ran and changed the
// recommendation after it was filed). It lands in the filed chip's title AND as a line under
// the row (PRD #1183). Comma form, no dashes, per the vocabulary contract.
const STALE_FILED_WARNING =
  "Filed for an earlier version of this recommendation, re-running the judge changed it since.";

// JustFiled is the run page's minimal local filed override, re-introduced from the pre-#1183
// RecommendationFiler after M2 dropped it for a pure refetch. It carries the issue a Create
// click just produced plus fileIssue's `warning`, and is kept per coordinate so a filed
// recommendation ALWAYS shows "Filed #N" and its warning even when the review refetch did not
// settle the link. `warning` is a SUCCESS signal — the forge issue WAS created, only its local
// link/cache could not settle — set EXACTLY when the link did not settle, so without this
// override the refetched review omits the coordinate, the row falls back to "To triage",
// File issue re-arms, and the user is silently invited to file a DUPLICATE. The settled
// refetch reconciles the rest through filedByCoord; this only guarantees the created issue
// and its warning are shown. Separate from STALE_FILED_WARNING (filed_at < review.updated_at),
// which is a different signal and stays untouched.
type JustFiled = { iid: number; web_url: string; warning: string };

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
  // Local just-filed overrides keyed by coordinate (see JustFiled): a coordinate whose file
  // succeeded but whose link did not settle server-side still shows "Filed #N" and its warning
  // here, so the refetch that omits it can never re-arm File issue against a live forge issue.
  const [justFiled, setJustFiled] = useState<Map<string, JustFiled>>(new Map());
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
                const ck = coordKey(rec.category, rec.target);
                const disp = dispByCoord.get(ck);
                const filed = filedByCoord.get(ck);
                // The local just-filed override for this coordinate (see JustFiled): present
                // once a Create click resolved, it keeps the filed chip and any warning on
                // screen when the refetch has not (or could not) settle the link.
                const jf = justFiled.get(ck);
                // Collapse-dismissed: hide a dismissed row while the toggle is off.
                if (!showDismissed && disp?.status === "dismissed") return null;
                // A filed LINK is stale when it predates the current review revision — a
                // DIFFERENT signal from disp.stale (the disposition's own rationale-hash
                // compare, rendered inside TriageDisposedRow). It rides the chip's title and
                // a line under the row, replacing the deleted "Issue created." box's warning.
                const staleFiled = filed !== undefined && new Date(filed.filed_at) < new Date(review.updated_at);
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
                      {/* The one triage ladder, via TriageStateChip. A disposition (done/
                          dismissed) wins the primary chip, but an already-filed link is kept
                          BESIDE it so a filed-then-done row still shows its issue link
                          (Resolved Q: file then later mark done). To-triage is the open default
                          only when neither a disposition nor a filed link exists. */}
                      <span
                        className="ml-auto flex flex-wrap items-center gap-2"
                        title={staleFiled ? STALE_FILED_WARNING : undefined}
                      >
                        {/* Settled link wins; the local just-filed override is the fallback
                            that keeps "Filed #N" up when the refetch did not settle it. */}
                        {filed ? (
                          <TriageStateChip state="filed" filed={filed} />
                        ) : (
                          jf && (
                            <TriageStateChip state="filed" filed={{ issue_iid: jf.iid, issue_url: jf.web_url }} />
                          )
                        )}
                        {disp ? (
                          disp.status === "done" ? (
                            <TriageStateChip state="done" />
                          ) : (
                            <TriageStateChip
                              state="dismissed"
                              reason={disp.reason === "not_an_issue" ? "not_an_issue" : "wont_do"}
                            />
                          )
                        ) : (
                          !filed && !jf && <TriageStateChip state="to_triage" />
                        )}
                      </span>
                    </div>
                    {staleFiled && <p className="mt-1 text-xs text-faint">{STALE_FILED_WARNING}</p>}
                    {/* The created-with-warning line, under the chip (same style as FindingCard):
                        the issue exists on the forge, only its local link/cache did not settle. */}
                    {jf?.warning && <p className="mt-1 text-xs text-muted">{jf.warning}</p>}
                    {rec.rationale_md.trim() !== "" && (
                      <div className="judge-prose mt-1.5">
                        <Markdown content={stripUnsafeChars(rec.rationale_md)} />
                      </div>
                    )}
                    <RecommendationTriage
                      runId={run.id}
                      rec={rec}
                      disp={disp}
                      filed={filed}
                      justFiled={jf}
                      repos={repos}
                      onChanged={fetchReview}
                      onJustFiled={(issue, warning) =>
                        setJustFiled((prev) =>
                          new Map(prev).set(ck, { iid: issue.iid, web_url: issue.web_url, warning }),
                        )
                      }
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

// RecommendationTriage is the per-recommendation triage area on the run page (PRD #1183):
// the shared TriageActions row (File issue · Mark done · Dismiss ▾) while the coordinate is
// open, or TriageDisposedRow (resolved Xh ago · the server stale badge · Undo) once a
// disposition exists, with IssueDraftCard opening BELOW the row on File issue. It replaces
// the old lone-orange RecommendationFiler + DispositionControls pair and the "Issue created."
// box (the filed state is now the "Filed #N ↗" chip in the row's state slot).
//
// This is the OWNED mutation mode: each handler does the optimistic api call, then the
// single-row refetch (onChanged) so the triage bar, chips and stale flag re-read from the
// server (never re-derived in TS), sets the live-region message, and arms the successor-focus
// move onto whichever control just mounted (the disposed row's Undo, or — after an Undo — the
// action row's first button).
//
// LIVE-REGION FIX (PRD #1183 review carry-forward): the sr-only region is hosted HERE, OUTSIDE
// the disp/no-disp branch, so a "Marked done" / "Dismissed" / "Undone" announcement survives
// the TriageActions→TriageDisposedRow swap that unmounts TriageActions the instant the message
// is set. TriageActions is told the caller hosts the region (callerHostsLiveRegion) so it
// renders no second, doomed one; the delegated-mode callers keep TriageActions' own region.
function RecommendationTriage({
  runId,
  rec,
  disp,
  filed,
  justFiled,
  repos,
  onChanged,
  onJustFiled,
  onError,
}: {
  runId: string;
  rec: ReviewRecommendation;
  disp?: Disposition;
  filed?: FiledIssue;
  justFiled?: JustFiled;
  repos: Repo[];
  onChanged: () => Promise<void>;
  onJustFiled: (issue: CreatedIssue, warning: string) => void;
  onError: (msg: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [announce, setAnnounce] = useState("");
  const [draftOpen, setDraftOpen] = useState(false);

  // rootRef contains the persistent live region + whichever branch is showing, so the
  // successor-focus effect can find "the first button" for both branches (the disposed row's
  // Undo is its only button; the action row's first button after an Undo).
  const rootRef = useRef<HTMLDivElement>(null);
  // Armed just before a refetch so the successor-focus effect knows the pending re-render is
  // user-initiated (and does not steal focus on mount or a passive refetch).
  const focusAfterMutation = useRef(false);

  // After a mutation + refetch swaps this row into the other branch, move focus to the
  // control that just mounted. disp → the disposed row's Undo; no disp (after an Undo) → the
  // action row's first button. Both are the FIRST <button> in the container, so one query
  // serves both. Keyed on disp AND busy: the successor is `disabled={busy}` and busy only
  // clears in the finally AFTER the refetch, so the focus is deferred until busy drops (a
  // disabled element ignores .focus()); the armed flag survives the intervening renders.
  useEffect(() => {
    if (!focusAfterMutation.current || busy) return;
    focusAfterMutation.current = false;
    rootRef.current?.querySelector<HTMLElement>("button")?.focus();
  }, [disp, busy]);

  const act = async (fn: () => Promise<unknown>, message: string) => {
    onError("");
    setBusy(true);
    try {
      await fn();
      // Arm the focus move BEFORE the refetch so the effect (fired by the parent's re-render
      // on refetch swapping the disp prop) sees the flag set.
      focusAfterMutation.current = true;
      setAnnounce(message);
      await onChanged();
    } catch (e) {
      focusAfterMutation.current = false;
      onError(errorMessage(e, "Could not update the disposition"));
    } finally {
      setBusy(false);
    }
  };

  // Adapts the server IssueDraft (default_repo_id + default_note) onto the card's normalised
  // seed. The card stripUnsafeChars's the title/description seed itself.
  const loadDraft = async (): Promise<IssueDraftSeed> => {
    const { draft } = await api.getIssueDraft(runId, rec.id);
    return {
      title: draft.title,
      description: draft.description,
      labels: draft.labels,
      provenance: draft.provenance,
      defaultRepoId: draft.default_repo_id,
      defaultNote: draft.default_note,
    };
  };

  // Create posts the existing file-issue call, records the created issue + any warning as the
  // local just-filed override (so the coordinate shows "Filed #N" even if the link did not
  // settle — fileIssue's `warning` is a created-with-warning success, not a retry signal),
  // then refetches (the settled link lands in filedByCoord and takes over) and closes the card.
  const createIssue = async ({ repoId, title, description }: IssueDraftValues) => {
    const res = await api.fileIssue(runId, rec.id, { repo_id: repoId, title, description });
    onJustFiled(res.issue, res.warning ?? "");
    setDraftOpen(false);
    await onChanged();
  };

  return (
    <div ref={rootRef}>
      {/* Persistent sr-only live region — see the LIVE-REGION FIX note above. Always mounted
          (empty until a mutation), the convention the run page's other live regions follow. */}
      <span className="sr-only" role="status" aria-live="polite">
        {announce}
      </span>
      {disp ? (
        <TriageDisposedRow
          resolvedAt={disp.set_at}
          stale={disp.stale}
          busy={busy}
          onUndo={() => act(() => api.deleteDisposition(runId, rec.id), "Undone")}
        />
      ) : (
        <>
          <div className="mt-2">
            <TriageActions
              // File issue only when not already filed — a settled link (filed) OR the local
              // just-filed override (justFiled, the created-with-warning case where the link did
              // not settle) both hide it, so File never re-arms over a live forge issue — and
              // not while the draft is open (the card below is the filing UI).
              onFile={filed || justFiled || draftOpen ? undefined : () => setDraftOpen(true)}
              onMarkDone={() => act(() => api.setDisposition(runId, rec.id, "done"), "Marked done")}
              onDismiss={(reason) =>
                act(
                  () => api.setDisposition(runId, rec.id, "dismissed", reason),
                  reason === "not_an_issue" ? "Dismissed, not an issue" : "Dismissed, won't do",
                )
              }
              dismissCopy="judge"
              busy={busy}
              callerHostsLiveRegion
            />
          </div>
          {draftOpen && (
            <IssueDraftCard loadDraft={loadDraft} onCreate={createIssue} onCancel={() => setDraftOpen(false)} repos={repos} />
          )}
        </>
      )}
    </div>
  );
}

// TriageSummary renders a TriageCounts bundle DIRECTLY — a segmented meter plus the
// counts line (to triage / filed / done / dismissed, with the false-positive sub-count).
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
    <div className={cx("rounded-lg border border-edge bg-ink/40 inset-panel p-3", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-xs font-semibold uppercase tracking-wider text-faint">{title}</h3>
        {aside != null && <span className="ml-auto text-xs text-faint">{aside}</span>}
      </div>
      <TriageMeter triage={triage} />
      <div className="mt-3 flex flex-wrap items-baseline gap-x-4 gap-y-1 text-xs">
        <TriageCount dotClass="bg-warn" n={triage.todo} label="to triage" />
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
