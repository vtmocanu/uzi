import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, ApiError, isHttpsUrl, preferForgeUrl, type IssueDetail, type RunListItem } from "../lib/api";
import { hasAnthropicToken } from "../lib/hasToken";
import { startRunGate } from "../lib/runStream";
import { activeRunInHistory, isStoppedRun, mrChipState, runStatusTone } from "../lib/runBadge";
import { mergeRequestUrl, projectWebUrlFromIssue } from "../lib/forgeUrls";
import { chipLabels } from "../lib/labelChips";
import { Markdown } from "../components/Markdown";
import { MrChip } from "../components/MrChip";
import { forgePlatform } from "../lib/forgeNoun";
import { formatDuration } from "../components/RunEvent";
import { Alert, Badge, Button, Card } from "../components/ui";
import { useAuth } from "../auth/AuthContext";
import { stripUnsafeChars } from "../lib/safeText";

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
  const { repoId = "", iid = "" } = useParams();
  const iidNum = Number(iid);
  const navigate = useNavigate();
  const { prdlessEnabled, prdlessLabel, prdLabel, autopilotLabel } = useAuth();

  const [issue, setIssue] = useState<IssueDetail | null>(null);
  const [runs, setRuns] = useState<RunListItem[]>([]);
  const [hasWorker, setHasWorker] = useState(false);
  const [hasToken, setHasToken] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [starting, setStarting] = useState(false);
  const [prdlessBusy, setPrdlessBusy] = useState(false);

  const load = useCallback(async () => {
    setError("");
    try {
      const [{ issue }, { runs }, { workers }, { secrets }] = await Promise.all([
        api.getIssue(repoId, iidNum),
        api.listRuns({ repoId, issueIid: iidNum }),
        api.listWorkers(),
        api.listSecrets(),
      ]);
      setIssue(issue);
      setRuns(runs);
      setHasWorker(workers.length > 0);
      setHasToken(hasAnthropicToken(secrets));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load the issue");
    } finally {
      setLoading(false);
    }
  }, [repoId, iidNum]);

  useEffect(() => {
    load();
  }, [load]);

  const startRun = async () => {
    if (!issue) return;
    setError("");
    setStarting(true);
    try {
      const { run } = await api.createRun(repoId, issue.iid);
      navigate(`/runs/${run.id}`);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not start run");
      setStarting(false);
      load();
    }
  };

  // PRDLESS toggle (PRD #22 M4): apply/remove the escape-hatch label directly.
  // Forge-first — wait for the 200 and adopt the returned card's labels; no
  // optimistic update, so a failed write leaves the issue's labels untouched.
  const prdlessApplied = !!issue && issue.labels.includes(prdlessLabel);
  // The bypass badge stands in for the "no PRD link" warning when the feature is on
  // and the label is applied (its condition is inlined at the badge below). It used
  // to also drive a conditional filter that kept the PRDLESS label from rendering
  // twice (S1); since PRD #102 M4 the shared chipLabels predicate excludes PRDLESS
  // unconditionally, so the double-render is impossible and the flag is gone.
  const togglePrdless = async () => {
    if (!issue) return;
    setError("");
    setPrdlessBusy(true);
    try {
      const { card } = await api.setIssuePrdless(repoId, issue.iid, !prdlessApplied);
      setIssue({ ...issue, labels: card.labels });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not update the label");
    } finally {
      setPrdlessBusy(false);
    }
  };

  const gate = issue
    ? startRunGate({
        hasPrdLink: issue.has_prd_link,
        prdlessBypass: prdlessEnabled && prdlessApplied,
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

      {error && <Alert message={error} />}
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
                <Badge tone="neutral">{columnLabel(issue)}</Badge>
                {/* Same predicate the board cards use (PRD #102 M4, Decision 6), so
                    the two surfaces agree on what is a content label. The issue view
                    knows only its own column, so that is the column exclusion set.
                    This is a behavior change here: PRD / autopilot / PRDLESS chips
                    used to render on this page and no longer do — PRDLESS was already
                    suppressed whenever its badge showed, and the Mark/Remove button
                    below still surfaces the label's state when it does not.
                    Issue #124: a label name is forge-supplied, so strip it for
                    display while the React key keeps the raw string. */}
                {chipLabels(issue.labels, {
                  prdLabel,
                  prdlessLabel,
                  autopilotLabel,
                  columnLabels: [issue.column],
                }).map((l) => (
                  <span
                    key={l}
                    title={stripUnsafeChars(l)}
                    className="rounded-md border border-edge bg-raised px-1.5 py-0.5 text-[11px] text-muted"
                  >
                    {stripUnsafeChars(l)}
                  </span>
                ))}
                {issue.author && <span className="text-xs text-faint">{issue.author}</span>}
                {!issue.has_prd_link &&
                  (prdlessEnabled && prdlessApplied ? (
                    <Badge tone="brand" title="PRD-link gate bypassed by label">
                      {prdlessLabel}
                    </Badge>
                  ) : (
                    <Badge
                      tone="warning"
                      title="Description has no link to a prds/*.md file; excluded from agent pickup"
                    >
                      no PRD link
                    </Badge>
                  ))}
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
              {/* Show the toggle when applying is meaningful (no PRD link) or the
                  label is already applied (so it can be removed); hide the pure
                  no-op case — an issue that already has a PRD link and no label (S2). */}
              {prdlessEnabled && !issue.closed && (prdlessApplied || !issue.has_prd_link) && (
                <Button
                  variant={prdlessApplied ? "secondary" : "ghost"}
                  disabled={prdlessBusy}
                  title={
                    prdlessApplied
                      ? `Remove the ${prdlessLabel} label and re-apply the PRD-link requirement`
                      : `Apply the ${prdlessLabel} label so a run can start without a PRD link`
                  }
                  onClick={togglePrdless}
                >
                  {prdlessBusy ? "…" : prdlessApplied ? `Remove ${prdlessLabel}` : `Mark ${prdlessLabel}`}
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
                {/* Issue #124 names issue DESCRIPTIONS alongside titles, and Markdown does
                    not close this: that pipeline is hardened against raw HTML and dangerous
                    URL schemes, neither of which is a bidi override. Stripping before the
                    renderer cannot inject markdown structure — it only deletes characters
                    that carry no markdown meaning. */}
                <Markdown content={stripUnsafeChars(issue.description)} />
              </div>
            ) : (
              <p className="text-sm text-faint">This issue has no description.</p>
            )}
          </Card>

          {!issue.closed && gate && (
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
    </div>
  );
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
        <Badge tone={runStatusTone(run.status, run.stop_kind)}>
          {stopped ? "stopped" : run.status.replace("_", " ")}
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
