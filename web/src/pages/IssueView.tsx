import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, isHttpsUrl, preferForgeUrl, type IssueDetail, type RunListItem, type SecretMeta } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { hasAnthropicToken, hasAnyCodexCredential, isCodexUsable } from "../lib/hasToken";
import { startRunGate } from "../lib/runStream";
import { startRunWithCredential } from "../lib/startRun";
import { INHERIT_SELECTION, type CredentialSelection } from "../lib/credentialOverride";
import {
  bothHarnessesUsable,
  effectiveHarnessIsCodex,
  INHERIT_HARNESS,
  type HarnessSelection,
} from "../lib/harnessSelection";
import { TokenPicker } from "../components/TokenPicker";
import { HarnessPicker } from "../components/HarnessPicker";
import { activeRunInHistory, effectiveRunStatus, isStoppedRun, mrChipState, runStatusTone } from "../lib/runBadge";
import { mergeRequestUrl, projectWebUrlFromIssue } from "../lib/forgeUrls";
import { chipLabels } from "../lib/labelChips";
import { canPromote, isUziCard } from "../lib/boardCards";
import { Markdown } from "../components/Markdown";
import { MrChip } from "../components/MrChip";
import { forgePlatform } from "../lib/forgeNoun";
import { formatDuration } from "../components/RunEvent";
import { Alert, Badge, Button, Card, Field } from "../components/ui";
import { ClockIcon } from "../components/icons";
import { ScheduleModal } from "../components/ScheduleModal";
import { useAuth } from "../auth/AuthContext";
import { stripUnsafeChars } from "../lib/safeText";
import { useDemoMode } from "../lib/demoMode";
import { maskUsername } from "../lib/demoMask";

// columnLabel names the column the issue sits in, for the header chip. "Backlog"
// is the display name of the implicit column (PRD #102 M1) — the stored column is
// still the empty string, and the board's move wire string is still "open".
function columnLabel(issue: IssueDetail): string {
  if (issue.closed) return "Closed";
  if (issue.column === "") return "Backlog";
  return issue.column;
}

// runDuration renders a terminal run's wall-clock span, or null while it is still
// running (no finished_at yet — the live elapsed lives on the run view). Thin
// wrapper over formatDuration kept co-located with the history row it feeds.
function runDuration(run: RunListItem): string | null {
  if (!run.started_at || !run.finished_at) return null;
  return formatDuration(new Date(run.finished_at).getTime() - new Date(run.started_at).getTime());
}

export function IssueView() {
  const demo = useDemoMode();
  const { repoId = "", iid = "" } = useParams();
  const iidNum = Number(iid);
  const navigate = useNavigate();
  const { uziLabel, autopilotLabel } = useAuth();

  // `issue` is also written by the `promote` handler and cleared by the route reset
  // effect (both below), so it stays local and the fetcher sets it as a side effect
  // rather than routing through the hook's read-only `data`.
  const [issue, setIssue] = useState<IssueDetail | null>(null);
  // `actionError` is a startRun/promote handler error, kept apart from the hook's load
  // error (`loadError`); the two share the one Alert slot below. It is deliberately NOT
  // cleared by the hook's onFetchStart: a failed start reloads the run history, and that
  // reload's fetch start used to wipe the error it was reloading after (issue #1727).
  // It clears at the start of the next attempt and on route navigation (below) instead.
  const [actionError, setActionError] = useState("");
  // The route identity the page currently shows. A start/promote handler captures it when
  // its attempt begins and, if it changed by the time the response lands, drops the
  // attempt's error, a promote's label adoption, and the busy-flag clear (plus a start's
  // run-history reload): the component is reused across issues, so without this a late
  // failure for issue A would surface on issue B after the reset effect below had already
  // cleared (#1727). A successful start still navigates to the new run on purpose: that
  // run really was created, so the user is taken to it wherever they are. The reset effect
  // below is also what keeps the ref current.
  const routeKeyRef = useRef(`${repoId}/${iidNum}`);
  const [starting, setStarting] = useState(false);
  const [promoting, setPromoting] = useState(false);
  // PRD #241: the "Schedule…" entry point, pre-pinned to this issue.
  const [scheduling, setScheduling] = useState(false);
  // PRD #1247 M7: the token the run will spend, chosen before Start run. Default
  // inherit (shown explicitly) so an untouched start follows the worker binding.
  const [credential, setCredential] = useState<CredentialSelection>(INHERIT_SELECTION);
  // PRD #1429 M4a: the harness the run will start on, chosen before Start run. Default
  // inherit (the picker is shown only when both harnesses are usable — D2), so an
  // untouched start follows the server's D11 resolver exactly as before this milestone.
  const [harness, setHarness] = useState<HarnessSelection>(INHERIT_HARNESS);
  // PRD #1247: this component is reused across route-param changes without remounting (the
  // data effect below keys on [repoId, iidNum] and refetches), so reset the picked
  // credential to inherit when the route identity changes (no-op at mount). Issue #961
  // item 4a: the same reset drops a start/promote error, so it cannot bleed onto the next
  // issue — a route change, not every refetch, is what makes that error stale. Issue #1727
  // review: it also drops the previous issue and its busy flags, so while the next issue
  // loads neither the old header nor its Start/Promote buttons (which would act on the
  // old issue) stay on screen, and the next issue does not inherit the old spinner.
  useEffect(() => {
    routeKeyRef.current = `${repoId}/${iidNum}`;
    setIssue(null);
    setStarting(false);
    setPromoting(false);
    setCredential(INHERIT_SELECTION);
    setHarness(INHERIT_HARNESS);
    setActionError("");
  }, [repoId, iidNum]);

  const { data, loading, error: loadError, reload } = useAsyncData(
    async ({ isCurrent }) => {
      const [{ issue }, { runs }, { workers }, { secrets }, { settings }] = await Promise.all([
        api.getIssue(repoId, iidNum),
        api.listRuns({ repoId, issueIid: iidNum }),
        api.listWorkers(),
        api.listSecrets(),
        // PRD #1429 M4a review Fix 1: the viewer's own default_harness, so the
        // not-yet-created start dialog's effective-Codex gate can mirror D11 rule 2
        // (a usable default wins on an untouched "inherit" pick) — RunDefaults is the
        // only other page that already fetches this, and it does so separately too.
        api.getMySettings(),
      ]);
      if (isCurrent()) setIssue(issue);
      return {
        runs,
        hasWorker: workers.length > 0,
        // PRD #1429 M4a, D3: harness-aware credential facts. claudeUsable/hasToken stay
        // the pre-M4a Anthropic-only check; codexUsable/hasCodexCredential are new.
        hasToken: hasAnthropicToken(secrets),
        codexUsable: isCodexUsable(secrets),
        hasCodexCredential: hasAnyCodexCredential(secrets),
        // Pass the already-loaded tokens to the picker so it need not re-fetch.
        tokens: secrets.filter((s: SecretMeta) => s.kind === "anthropic_token"),
        defaultHarness: settings.default_harness,
      };
    },
    [repoId, iidNum],
    // "deps": a route change re-arms the loading line, since the reset effect above has
    // just cleared the previous issue and the page would otherwise sit blank.
    { fallback: "Failed to load the issue", skeleton: "deps" },
  );
  const runs = data?.runs ?? [];
  const hasWorker = data?.hasWorker ?? false;
  const hasToken = data?.hasToken ?? false;
  const codexUsable = data?.codexUsable ?? false;
  const hasCodexCredential = data?.hasCodexCredential ?? false;
  const tokens = data?.tokens ?? [];
  const defaultHarness = data?.defaultHarness ?? null;
  // D2: the harness control appears only when BOTH harnesses are usable — a
  // single-harness user (Claude-only or Codex-only) sees no picker at all.
  const showHarnessPicker = bothHarnessesUsable(hasToken, codexUsable);
  // Fix 2 (M4a review): gate the Anthropic TokenPicker on the EFFECTIVE harness, not the
  // raw picker selection — a Codex-only user never sees the harness picker (showHarnessPicker
  // is false), so `harness` stays "inherit" even though the run WILL resolve to Codex. Fix 1
  // (M4a review follow-up): also thread the viewer's default_harness, so a BOTH-usable user
  // who set a Codex default (Run Defaults) and leaves the picker on "inherit" gets the same
  // hide — touching the Anthropic picker in that case 422s (D11 rule 2 resolves them to Codex).
  const startingOnCodex = effectiveHarnessIsCodex(harness, hasToken, codexUsable, defaultHarness);

  const startRun = async () => {
    if (!issue) return;
    // Act only on the issue the route shows: this render's repoId paired with the issue
    // it holds must be the current route identity, or the click would start a run on a
    // repo/issue mix (#1727 review).
    const attemptKey = `${repoId}/${issue.iid}`;
    if (routeKeyRef.current !== attemptKey) return;
    setActionError("");
    setStarting(true);
    // The shared helper carries the chosen credential override AND harness, and
    // preserves both across the open-MR force retry (issue #856). onSettled clears the
    // starting flag and reloads the run history, but leaves the error onError just set
    // on screen (issue #1727): the next attempt clears it above.
    await startRunWithCredential(repoId, issue.iid, credential, {
      // encodeURIComponent the id: per-call-site open-redirect hardening (see
      // safeNextPath in Login.tsx). A no-op for today's UUID ids.
      onCreated: (runId) => navigate(`/runs/${encodeURIComponent(runId)}`),
      onError: (msg) => {
        if (routeKeyRef.current === attemptKey) setActionError(msg);
      },
      onSettled: () => {
        if (routeKeyRef.current !== attemptKey) return;
        setStarting(false);
        reload();
      },
    }, harness);
  };

  // PRD #764, #767 M5. The detail page drives its Start/Promote affordance off the
  // SAME predicate the board card uses, so a card's affordances and its detail page's
  // cannot disagree: an issue is runnable if it carries `uzi` OR is assigned to the
  // repo's bot. The bot id rides the issue-detail payload (per-connection), not the
  // session. A runnable issue offers Start run; a non-runnable one offers Promote.
  const isEligible = !!issue && isUziCard(issue, uziLabel, issue.bot_forge_user_id);
  const promotable = !!issue && canPromote(issue, uziLabel, issue.bot_forge_user_id);

  // Promote (Decision 15; PRD #764): add the `uzi` label forge-first, then adopt the
  // returned card's labels — no optimistic update. Both outcomes are dropped when the
  // user has navigated to another issue meanwhile: adopting the labels there would mark
  // the new issue with this one's promotion (#1727).
  const promote = async () => {
    if (!issue) return;
    const target = issue;
    const attemptKey = `${repoId}/${target.iid}`;
    if (routeKeyRef.current !== attemptKey) return;
    setActionError("");
    setPromoting(true);
    try {
      const { card } = await api.promoteIssue(repoId, target.iid);
      // Adopt the labels onto the issue currently shown, and only if it is still the
      // one promoted (a same-route refetch may have replaced the object meanwhile).
      if (routeKeyRef.current === attemptKey) {
        setIssue((cur) => (cur && cur.iid === target.iid ? { ...cur, labels: card.labels } : cur));
      }
    } catch (err) {
      if (routeKeyRef.current === attemptKey) {
        setActionError(errorMessage(err, "Could not promote the issue"));
      }
    } finally {
      if (routeKeyRef.current === attemptKey) setPromoting(false);
    }
  };

  const gate = issue
    ? startRunGate({
        closed: issue.closed,
        hasWorker,
        claudeUsable: hasToken,
        codexUsable,
        hasCodexCredential,
        harness,
        activeRunExists: activeRunInHistory(runs),
      })
    : null;

  return (
    <div className="space-y-5">
      <nav className="flex items-center gap-1.5 text-xs text-faint">
        <Link to={`/repos/${repoId}/board`} className="hover:text-fg">
          Board
        </Link>
        <span>/</span>
        <span className="text-muted">#{iid}</span>
      </nav>

      {(loadError || actionError) && <Alert message={loadError || actionError} />}
      {loading && <p className="text-faint">Loading issue…</p>}

      {issue && (
        <>
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                {/* Issue #124: forge-supplied, untrusted (see Board). */}
                <h1 className="truncate text-2xl font-semibold">{stripUnsafeChars(issue.title)}</h1>
                <span className="text-sm text-faint">#{issue.iid}</span>
              </div>
              <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
                {/* S3: `wrap` plus a bounded width, because a column name is
                    user-supplied and effectively unbounded. The Columns editor accepts
                    any length (no maxlength, no validation) and GitLab allows 255
                    characters; measured at 375x812, a 105-char name rendered a 594px
                    badge inside a 375px viewport and pushed document.scrollWidth to
                    610, scrolling the whole page sideways. The threshold is around 60
                    characters, which a real column name can reach by accident.

                    The board's own column header already handles the same string by
                    wrapping — this makes the detail page agree with it. min-w-0 is
                    required for the wrap to take effect inside the flex row. */}
                <Badge tone="neutral" wrap>
                  <span className="min-w-0 break-words">{columnLabel(issue)}</span>
                </Badge>
                {/* Same predicate the board cards use (PRD #102 M4, Decision 6). The
                    issue view knows only its own column, so that is the column
                    exclusion set — which means the two surfaces do NOT agree on a
                    CONFLICTED issue: the board excludes every configured column label,
                    so an issue carrying two of them chips neither there, while here the
                    second one still chips. An earlier version of this comment claimed
                    they agree; they agree on the ordinary case only, and the sentence
                    above names exactly why (review m-4 / fact-check R2).

                    The autopilot chip is excluded here and surfaced as the badge below
                    instead, so one fact reads one way. The `uzi` runnable marker is
                    excluded for the same reason (PRD #764): it is surfaced as the brand
                    "runnable" badge below, so it must not ALSO render as a plain content
                    chip here. (The board keeps `uzi` as a highlighted chip instead, so
                    chipLabels itself does not exclude it — this view drops it locally.)

                    Issue #124: a label name is forge-supplied, so strip it for display
                    while the React key keeps the raw string. */}
                {chipLabels(issue.labels, {
                  autopilotLabel,
                  columnLabels: [issue.column],
                })
                  .filter((l) => l !== uziLabel)
                  .map((l) => (
                  <span
                    key={l}
                    title={stripUnsafeChars(l)}
                    className="rounded-md border border-edge bg-raised px-1.5 py-0.5 text-[11px] text-muted"
                  >
                    {stripUnsafeChars(l)}
                  </span>
                ))}
                {/* The autopilot label has had NO user-visible surface in web/ since M4
                    removed it from the chip list (correctly — it is a workflow marker,
                    not content). A BADGE is not a chip, so Decision 6 is untouched: the
                    chip row still excludes it, and this says the distinct thing the chip
                    never did, which is that the issue is armed for an unattended run.
                    Mirrors RunView's autopilot badge so one fact reads one way. Without
                    it, an armed issue shows nothing at all until a run exists. */}
                {issue.labels.includes(autopilotLabel) && (
                  <Badge tone="brand" title="Autopilot: a run starts automatically, with the plan auto-approved">
                    {autopilotLabel}
                  </Badge>
                )}
                {/* The runnable marker (PRD #764, widened by #767 M5): an issue is uzi's
                    to run if it carries the `uzi` label OR is assigned to the repo's bot.
                    It becomes an explicit badge here — the only form that reaches a screen
                    reader — brand-toned like the card's highlighted `uzi` chip. The two
                    paths get DISTINCT copy so the marker never claims a label an
                    assignment-only issue does not carry. An issue with neither offers
                    Promote instead (below). */}
                {isEligible &&
                  (issue.labels.includes(uziLabel) ? (
                    <Badge
                      tone="brand"
                      title={`This issue carries the ${uziLabel} label, so uzi will run it.`}
                    >
                      {uziLabel}
                    </Badge>
                  ) : (
                    <Badge
                      tone="brand"
                      title="This issue is assigned to the uzi bot, so it's eligible for a uzi run — start it, or let autopilot or an enabled sweep pick it up."
                    >
                      assigned
                    </Badge>
                  ))}
                {issue.author && <span className="text-xs text-faint">{maskUsername(issue.author, "human", demo)}</span>}
                {/* Neutral PRD-presence marker (PRD #764): a linked prds/*.md is optional
                    but still detected, so an issue that has one shows a quiet "PRD" badge. */}
                {issue.has_prd_link && (
                  <Badge tone="neutral" title="This issue links a prds/*.md file">
                    PRD
                  </Badge>
                )}
                {issue.conflict && (
                  <Badge
                    tone="danger"
                    title="Issue carries multiple column labels; shown in the highest column until the next move"
                  >
                    conflict
                  </Badge>
                )}
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              {/* Promote (Decision 15; PRD #764) is the action for a non-runnable issue:
                  it adds the `uzi` label, making the issue runnable. */}
              {promotable && (
                <Button
                  variant="secondary"
                  disabled={promoting}
                  title={`Add the ${uziLabel} label so uzi can work this issue`}
                  onClick={promote}
                >
                  {promoting ? "…" : `Promote to ${uziLabel}`}
                </Button>
              )}
              {/* Schedule… (PRD #241 M5, mock §3): opens the schedule modal
                  pre-pinned to this issue (target=issue, locked). Available
                  regardless of the immediate-run gate — you can schedule ahead. */}
              {!issue.closed && (
                <Button variant="secondary" onClick={() => setScheduling(true)} title="Schedule a run for this issue">
                  <ClockIcon /> Schedule…
                </Button>
              )}
              {isHttpsUrl(issue.web_url) && (
                <a href={issue.web_url} target="_blank" rel="noreferrer">
                  <Button variant="ghost">Open on {forgePlatform(issue.forge_type)}</Button>
                </a>
              )}
            </div>
          </div>

          <Card className="space-y-3">
            <h2 className="text-sm font-semibold uppercase tracking-wide text-faint">
              Description
            </h2>
            {issue.description.trim() ? (
              <div className="docs-prose max-w-none">
                {/* Issue #124 names issue DESCRIPTIONS alongside titles. Since #319 the
                    <Markdown> pipeline strips Cf/bidi centrally, so this per-site
                    stripUnsafeChars wrap is redundant-but-harmless (idempotent) — kept so
                    the value matches the escaped-JSX title sink. Stripping before the
                    renderer cannot inject markdown structure — it only deletes characters
                    that carry no markdown meaning. */}
                <Markdown content={stripUnsafeChars(issue.description)} />
              </div>
            ) : (
              <p className="text-sm text-faint">This issue has no description.</p>
            )}
          </Card>

          {/* Hidden entirely on a non-eligible issue rather than shown gated: the server
              refuses the run (Decision 14), and Promote above is the one-click answer,
              so a disabled button explaining a rule the user can resolve in place would
              be noise. Keyed on eligibility (PRD #196 M4), so a runnable `bug` issue
              DOES show Start run. */}
          {!issue.closed && isEligible && gate && (
            <div className="flex flex-wrap items-end gap-3">
              {/* PRD #1429 M4a: choose the harness the run starts on, shown ONLY when
                  both harnesses are usable (D2) — a single-harness user never sees this
                  redundant picker. */}
              {showHarnessPicker && (
                <Field label="Harness" htmlFor="start-run-harness">
                  <HarnessPicker
                    id="start-run-harness"
                    label="Harness for this run"
                    className="h-9 w-40 text-sm"
                    value={harness}
                    onChange={setHarness}
                    disabled={starting}
                  />
                </Field>
              )}
              {/* PRD #1247 M7: choose the token the run spends before starting it. Inherit
                  (the default, shown explicitly) follows the worker's binding. Hidden when
                  the run will EFFECTIVELY start on Codex — an explicit pick, OR (M4a review
                  fix) a Codex-only user whose picker is hidden but who WILL resolve to Codex
                  implicitly — a Codex run spends a Codex credential, never an Anthropic one,
                  so the picker would be noise (and using it would 422). */}
              {!startingOnCodex && (
                <Field label="Anthropic token" htmlFor="start-run-token">
                  <TokenPicker
                    id="start-run-token"
                    label="Anthropic token for this run"
                    className="h-9 w-56 text-sm"
                    value={credential}
                    onChange={setCredential}
                    tokens={tokens}
                    disabled={!gate.enabled || starting}
                  />
                </Field>
              )}
              <div>
                <Button
                  variant={gate.enabled ? "primary" : "ghost"}
                  disabled={!gate.enabled || starting}
                  title={gate.enabled ? "Queue an agent run for this issue" : gate.reason}
                  onClick={startRun}
                >
                  {starting ? "Starting…" : "Start run"}
                </Button>
                {!gate.enabled && <p className="mt-1 text-xs text-faint">{gate.reason}</p>}
              </div>
            </div>
          )}

          <Card className="space-y-3">
            <h2 className="text-sm font-semibold uppercase tracking-wide text-faint">
              Run history
            </h2>
            {runs.length === 0 ? (
              <p className="text-sm text-faint">No runs yet for this issue.</p>
            ) : (
              <ul className="space-y-2">
                {runs.map((run) => (
                  <RunHistoryRow
                    key={run.id}
                    run={run}
                    projectWebUrl={projectWebUrlFromIssue(issue.web_url)}
                  />
                ))}
              </ul>
            )}
          </Card>
        </>
      )}

      {scheduling && issue && (
        <ScheduleModal
          pinned={{ repoId, repoPath: repoPathFromWebUrl(issue.web_url), issueIid: issue.iid }}
          onClose={() => setScheduling(false)}
          onSaved={() => setScheduling(false)}
        />
      )}
    </div>
  );
}

// repoPathFromWebUrl derives a "namespace/repo" display path from an issue's forge
// web URL, tolerating both GitLab (/-/issues/) and GitHub/Forgejo (/issues/) grammars.
// Best-effort — an unparseable URL yields "", which the modal renders gracefully.
function repoPathFromWebUrl(webUrl: string): string {
  try {
    const p = new URL(webUrl).pathname;
    return p
      .replace(/\/-\/issues\/.*/, "")
      .replace(/\/issues\/.*/, "")
      .replace(/^\/+/, "");
  } catch {
    return "";
  }
}

function RunHistoryRow({ run, projectWebUrl }: { run: RunListItem; projectWebUrl: string }) {
  const stopped = isStoppedRun(run.status, run.stop_kind);
  const duration = runDuration(run);
  // PRD §3 asks for an MR/PR *link* in the history. Prefer the forge-supplied URL
  // the worker persisted (PRD #65 D8) — the forge's own canonical URL — guarded
  // through isHttpsUrl by preferForgeUrl. A null (rows created before it landed) falls
  // back to a forge-aware reconstruction: mergeRequestUrl picks the path segment per
  // forge (GitLab /-/merge_requests/, GitHub /pull/, Forgejo /pulls/); when neither
  // yields an https URL the chip renders as plain text so it is never absent.
  const mrHref = preferForgeUrl(
    run.mr_web_url,
    run.mr_iid != null ? mergeRequestUrl(projectWebUrl, run.mr_iid, run.forge_type) : null,
  );
  // MR state (PRD #33): a per-run frozen hint; open renders exactly as before,
  // merged/closed get a label and closed is muted + struck ("as of last sync").
  const mrState = mrChipState(run.mr_state);
  // §3 "started": show when the run began; fall back to its queued time for a run
  // that has not started yet (started_at null).
  const stamp = run.started_at ?? run.created_at;
  return (
    <li className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2">
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-x-2 text-xs text-faint">
          <span>{new Date(stamp).toLocaleString()}</span>
          {run.worker_name && <span>· {run.worker_name}</span>}
          {duration && <span>· {duration}</span>}
          {run.mr_iid != null && (
            <span>
              · <MrChip variant="inline" openTone="brand" forgeType={run.forge_type} mrIid={run.mr_iid} mrState={mrState} href={mrHref} />
            </span>
          )}
        </div>
      </div>
      <div className="flex items-center gap-2">
        <Badge tone={runStatusTone(effectiveRunStatus(run), run.stop_kind)}>
          {stopped ? "stopped" : effectiveRunStatus(run).replace("_", " ")}
        </Badge>
        {/* Every run here is the viewer's own (the endpoint is owner-scoped), so
            the run view is always reachable — no is_mine gate needed. */}
        <Link to={`/runs/${run.id}`} className="text-xs text-brand hover:text-brand-hover">
          view →
        </Link>
      </div>
    </li>
  );
}
