import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, ApiError, isHttpsUrl, type IssueDetail, type RunListItem } from "../lib/api";
import { startRunGate } from "../lib/runStream";
import { activeRunInHistory, isStoppedRun } from "../lib/runBadge";
import { Markdown } from "../components/Markdown";
import { formatDuration } from "../components/RunEvent";
import { Alert, Badge, Button, Card } from "../components/ui";

// columnLabel names the column the issue sits in, for the header chip.
function columnLabel(issue: IssueDetail): string {
  if (issue.closed) return "Closed";
  if (issue.column === "") return "Open";
  return issue.column;
}

// runTone mirrors RunsList: a deliberate stop (cancelled, or a cancel-shaped
// failed) is calm/neutral, never rose (PRD #12 §2).
function runTone(status: string, failureReason: string | null): "neutral" | "warning" | "danger" {
  if (status === "awaiting_approval") return "warning";
  if (isStoppedRun(status, failureReason)) return "neutral";
  if (status === "failed") return "danger";
  return "neutral";
}

// runDuration renders a terminal run's wall-clock span, or null while it is still
// running (no finished_at yet — the live elapsed lives on the run view).
function runDuration(run: RunListItem): string | null {
  if (!run.started_at || !run.finished_at) return null;
  return formatDuration(new Date(run.finished_at).getTime() - new Date(run.started_at).getTime());
}

// projectWebUrlFromIssue recovers the project base URL from an issue's web_url.
// GitLab issue URLs are `${projectWebUrl}/-/issues/${iid}`; stripping that suffix
// gives the base used to build the run-history MR links (GitLab-only product, the
// same `/-/merge_requests/` path the board hardcodes). "" when the shape doesn't
// match, which isHttpsUrl then rejects so the MR renders as plain text.
function projectWebUrlFromIssue(issueWebUrl: string): string {
  const i = issueWebUrl.indexOf("/-/issues/");
  return i >= 0 ? issueWebUrl.slice(0, i) : "";
}

export function IssueView() {
  const { repoId = "", iid = "" } = useParams();
  const iidNum = Number(iid);
  const navigate = useNavigate();

  const [issue, setIssue] = useState<IssueDetail | null>(null);
  const [runs, setRuns] = useState<RunListItem[]>([]);
  const [hasWorker, setHasWorker] = useState(false);
  const [hasToken, setHasToken] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [starting, setStarting] = useState(false);

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
      setHasToken(secrets.some((s) => s.kind === "anthropic_token"));
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

  const gate = issue
    ? startRunGate({
        hasPrdLink: issue.has_prd_link,
        closed: issue.closed,
        hasWorker,
        hasToken,
        activeRunExists: activeRunInHistory(runs),
      })
    : null;

  return (
    <div className="space-y-5">
      <nav className="flex items-center gap-1.5 text-xs text-slate-500">
        <Link to={`/repos/${repoId}/board`} className="hover:text-indigo-300">
          Board
        </Link>
        <span>/</span>
        <span className="text-slate-400">#{iid}</span>
      </nav>

      {error && <Alert message={error} />}
      {loading && <p className="text-slate-500">Loading issue…</p>}

      {issue && (
        <>
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <h1 className="truncate text-2xl font-semibold">{issue.title}</h1>
                <span className="text-sm text-slate-500">#{issue.iid}</span>
              </div>
              <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
                <Badge tone="neutral">{columnLabel(issue)}</Badge>
                {issue.author && <span className="text-xs text-slate-500">{issue.author}</span>}
                {!issue.has_prd_link && (
                  <Badge
                    tone="warning"
                    title="Description has no link to a prds/*.md file; excluded from agent pickup"
                  >
                    no PRD link
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
              {isHttpsUrl(issue.web_url) && (
                <a href={issue.web_url} target="_blank" rel="noreferrer">
                  <Button variant="ghost">Open on GitLab</Button>
                </a>
              )}
            </div>
          </div>

          <Card className="space-y-3">
            <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
              Description
            </h2>
            {issue.description.trim() ? (
              <div className="docs-prose max-w-none">
                <Markdown content={issue.description} />
              </div>
            ) : (
              <p className="text-sm text-slate-600">This issue has no description.</p>
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
              {!gate.enabled && <p className="mt-1 text-xs text-slate-500">{gate.reason}</p>}
            </div>
          )}

          <Card className="space-y-3">
            <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
              Run history
            </h2>
            {runs.length === 0 ? (
              <p className="text-sm text-slate-600">No runs yet for this issue.</p>
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
  const stopped = isStoppedRun(run.status, run.failure_reason);
  const duration = runDuration(run);
  // PRD §3 asks for an MR *link* in the history; link it when we can build an https
  // URL (like the board), else fall back to a plain "!N" chip so it is never absent.
  const mrHref =
    run.mr_iid != null && isHttpsUrl(projectWebUrl)
      ? `${projectWebUrl}/-/merge_requests/${run.mr_iid}`
      : null;
  return (
    <li className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-slate-800 bg-slate-900/40 px-3 py-2">
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-x-2 text-xs text-slate-500">
          <span>{new Date(run.created_at).toLocaleString()}</span>
          {run.worker_name && <span>· {run.worker_name}</span>}
          {duration && <span>· {duration}</span>}
          {run.mr_iid != null && (
            <span>
              ·{" "}
              {mrHref ? (
                <a
                  href={mrHref}
                  target="_blank"
                  rel="noreferrer"
                  title="Open the merge request on GitLab"
                  className="text-indigo-400 hover:text-indigo-300"
                >
                  !{run.mr_iid}
                </a>
              ) : (
                <>!{run.mr_iid}</>
              )}
            </span>
          )}
        </div>
      </div>
      <div className="flex items-center gap-2">
        <Badge tone={runTone(run.status, run.failure_reason)}>
          {stopped ? "stopped" : run.status.replace("_", " ")}
        </Badge>
        {/* Every run here is the viewer's own (the endpoint is owner-scoped), so
            the run view is always reachable — no is_mine gate needed. */}
        <Link to={`/runs/${run.id}`} className="text-xs text-indigo-400 hover:text-indigo-300">
          view →
        </Link>
      </div>
    </li>
  );
}
