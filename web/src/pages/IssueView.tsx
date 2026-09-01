import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, ApiError, isHttpsUrl, isOpenMRConflict, openMRConflictMRIID, preferForgeUrl, type IssueDetail, type RunListItem } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { hasAnthropicToken } from "../lib/hasToken";
import { startRunGate } from "../lib/runStream";
import { activeRunInHistory, effectiveRunStatus, isStoppedRun, mrChipState, runStatusTone } from "../lib/runBadge";
import { mergeRequestUrl, projectWebUrlFromIssue } from "../lib/forgeUrls";
import { chipLabels } from "../lib/labelChips";
import { canPromote, isUziCard } from "../lib/boardCards";
import { Markdown } from "../components/Markdown";
import { MrChip } from "../components/MrChip";
import { forgePlatform } from "../lib/forgeNoun";
import { formatDuration } from "../components/RunEvent";
import { Alert, Badge, Button, Card } from "../components/ui";
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

  // `issue` is also written by the `promote` handler (setIssue at :promote), so it
  // stays local and the fetcher sets it as a side effect rather than routing through
  // the hook's read-only `data`.
  const [issue, setIssue] = useState<IssueDetail | null>(null);
  // `error` is the load error (from the hook, as `loadError`) OR a startRun/promote
  // handler error kept here — the two share the one Alert slot below.
  const [error, setError] = useState("");
  const [starting, setStarting] = useState(false);
  const [promoting, setPromoting] = useState(false);
  // PRD #241: the "Schedule…" entry point, pre-pinned to this issue.
  const [scheduling, setScheduling] = useState(false);

  const { data, loading, error: loadError, reload } = useAsyncData(
    async () => {
      const [{ issue }, { runs }, { workers }, { secrets }] = await Promise.all([
        api.getIssue(repoId, iidNum),
        api.listRuns({ repoId, issueIid: iidNum }),
        api.listWorkers(),
        api.listSecrets(),
      ]);
      setIssue(issue);
      return {
        runs,
        hasWorker: workers.length > 0,
        hasToken: hasAnthropicToken(secrets),
      };
    },
    [repoId, iidNum],
    { fallback: "Failed to load the issue" },
  );
  const runs = data?.runs ?? [];
  const hasWorker = data?.hasWorker ?? false;
  const hasToken = data?.hasToken ?? false;

  const startRun = async () => {
    if (!issue) return;
    setError("");
    setStarting(true);
    // createAndOpen runs the create then navigates; the force path reuses it so
    // the retry does not duplicate the navigate.
    const createAndOpen = async (force?: boolean) => {
      const { run } = await api.createRun(repoId, issue.iid, force);
      // encodeURIComponent the id: per-call-site open-redirect hardening (see
      // safeNextPath in Login.tsx). A no-op for today's UUID ids.
      navigate(`/runs/${encodeURIComponent(run.id)}`);
    };
    try {
      await createAndOpen();
    } catch (err) {
      // issue_has_open_mr (issue #856): a completed prior run still owns an open
      // MR. Compose a web-specific confirm naming the MR (no --force jargon);
      // confirm, then retry with force.
      if (isOpenMRConflict(err) && err instanceof ApiError) {
        const mr = openMRConflictMRIID(err);
        const detail =
          mr != null ? `an open merge request (!${mr})` : "an open merge request";
        const proceed = window.confirm(
          `This issue already has ${detail} from a completed run. Starting a new run will plan and review it again from scratch. Start a new run anyway?`,
        );
        if (proceed) {
          try {
            await createAndOpen(true);
            return;
          } catch (retryErr) {
            setError(errorMessage(retryErr, "Could not start run"));
          }
        }
        // Declined (or forced retry failed): clear starting, no toast on decline.
        setStarting(false);
        reload();
        return;
      }
      setError(errorMessage(err, "Could not start run"));
      setStarting(false);
      reload();
    }
  };

  // PRD #764, #767 M5. The detail page drives its Start/Promote affordance off the
  // SAME predicate the board card uses, so a card's affordances and its detail page's
  // cannot disagree: an issue is runnable if it carries `uzi` OR is assigned to the
  // repo's bot. The bot id rides the issue-detail payload (per-connection), not the
  // session. A runnable issue offers Start run; a non-runnable one offers Promote.
  const isEligible = !!issue && isUziCard(issue, uziLabel, issue.bot_forge_user_id);
  const promotable = !!issue && canPromote(issue, uziLabel, issue.bot_forge_user_id);

  // Promote (Decision 15; PRD #764): add the `uzi` label forge-first, then adopt the
  // returned card's labels — no optimistic update.
  const promote = async () => {
    if (!issue) return;
    setError("");
    setPromoting(true);
    try {
      const { card } = await api.promoteIssue(repoId, issue.iid);
      setIssue({ ...issue, labels: card.labels });
    } catch (err) {
      setError(errorMessage(err, "Could not promote the issue"));
    } finally {
      setPromoting(false);
    }
  };

  const gate = issue
    ? startRunGate({
        closed: issue.closed,
        hasWorker,
        hasToken,
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

      {(loadError || error) && <Alert message={loadError || error} />}
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
  // the worker persisted (PRD #65 D8) — the only correct link on Forgejo — guarded
  // through isHttpsUrl by preferForgeUrl. A null (rows created before it landed, all
  // GitLab) falls back to the legacy GitLab reconstruction; when neither yields an
  // https URL the chip renders as plain text so it is never absent.
  const mrHref = preferForgeUrl(run.mr_web_url, run.mr_iid != null ? mergeRequestUrl(projectWebUrl, run.mr_iid) : null);
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
