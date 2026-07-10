// Live run view. The header carries the status pill plus a LIVE STAGE label
// derived from the newest message — multica's task-status-pill idea
// (packages/views/chat/components/task-status-pill.tsx maps the latest tool
// slug to a human stage: "Running command", "Reading files", "Making edits"…)
// — so you can tell what the agent is doing without reading the feed. Terminal
// states get a hero banner: the MR link is the run's entire output and must
// not hide in chrome. The breadcrumb keeps PRD #12's in-app board / issue links.

import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, ApiError, isHttpsUrl, isTerminalRun, type Repo, type RunMessage } from "../lib/api";
import { isStoppedRun } from "../lib/runBadge";
import { useRunStream } from "../lib/useRunStream";
import { CIFixRunHeader } from "../components/CIFixRunHeader";
import { formatDuration } from "../components/RunEvent";
import { ActivityFeed } from "../components/ActivityFeed";
import { Markdown } from "../components/Markdown";
import { Alert, Badge, Button, Card, PageHeader, Spinner, StatusPill, Textarea, cx } from "../components/ui";
import { ExternalLinkIcon } from "../components/icons";

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

export function RunView() {
  const { id = "" } = useParams();
  const { run, messages, connected, error, submit } = useRunStream(id);
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
  const mrUrl =
    run.mr_iid != null && isHttpsUrl(repoWebUrl)
      ? `${repoWebUrl}/-/merge_requests/${run.mr_iid}`
      : null;
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
            <div className="flex flex-wrap items-center gap-x-2">
              <h1 className="truncate text-xl font-semibold tracking-tight">{run.issue_title}</h1>
              {run.issue_iid != null && <span className="text-sm text-faint">#{run.issue_iid}</span>}
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
              {/* A stopped run (cancel or stop-shaped failure) reads as a neutral
                  "stopped" pill — StatusPill's default tone — so it stays calm and
                  agrees with the board/RunsList. */}
              <StatusPill status={stopped ? "stopped" : run.status} />
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
              {/* The live/offline WS indicator is only meaningful while the run is
                  active; a terminal run has no stream, so never show "completed • live". */}
              {!terminal && (
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
            </div>
          </div>
        }
      />

      {error && <Alert message={error} />}
      {actionErr && <Alert message={actionErr} />}

      {/* Terminal hero: the outcome, front and center. */}
      {run.status === "completed" && (
        <div className="rounded-xl border border-ok/40 bg-ok/10 p-4">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <p className="text-sm font-semibold text-ok">Run completed</p>
              <p className="mt-0.5 text-xs text-muted">
                {duration && <>Ran for {duration}. </>}
                {run.branch && (
                  <>
                    Branch <code className="rounded bg-raised px-1 py-0.5 text-fg">{run.branch}</code>
                    {run.mr_iid != null && " — merge request opened."}
                  </>
                )}
              </p>
            </div>
            {mrUrl ? (
              <a href={mrUrl} target="_blank" rel="noreferrer">
                <Button>
                  Open merge request !{run.mr_iid} <ExternalLinkIcon />
                </Button>
              </a>
            ) : (
              run.mr_iid != null && <Badge tone="ok">MR !{run.mr_iid}</Badge>
            )}
          </div>
        </div>
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
              {run.failure_reason && (
                <p className={cx("mt-0.5 text-xs", stopped ? "text-muted" : "text-danger/80")}>
                  {run.failure_reason}
                </p>
              )}
              {duration && <p className="mt-0.5 text-xs text-muted">Ran for {duration}.</p>}
            </div>
            {/* The MR link is the run's whole output; surface it even on a failed or
                stopped run, not just the completed hero (a calm secondary button). */}
            {mrUrl ? (
              <a href={mrUrl} target="_blank" rel="noreferrer">
                <Button variant="secondary">
                  Open merge request !{run.mr_iid} <ExternalLinkIcon />
                </Button>
              </a>
            ) : (
              run.mr_iid != null && <Badge tone="neutral">MR !{run.mr_iid}</Badge>
            )}
          </div>
        </div>
      )}

      {run.status === "awaiting_approval" && (
        <PlanPanel
          plan={run.plan_md ?? ""}
          busy={busy}
          onApprove={() => act(() => submit("approve_plan"))}
          onReject={(reason) => act(() => submit("reject_plan", reason))}
        />
      )}

      <Card className="p-4">
        <ActivityFeed
          messages={messages}
          runningLive={run.status === "running"}
          connected={connected}
          terminal={terminal}
        />
      </Card>

      {!terminal && (
        <FollowUpComposer
          busy={busy}
          onStop={() => act(() => submit("cancel"))}
          onSend={(text) => act(() => submit("follow_up", text))}
        />
      )}
    </div>
  );
}

// PlanPanel: the run's one human decision point — visually the loudest thing on
// the page while it is pending.
function PlanPanel({
  plan,
  busy,
  onApprove,
  onReject,
}: {
  plan: string;
  busy: boolean;
  onApprove: () => void;
  onReject: (reason: string) => void;
}) {
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState("");
  return (
    <div className="overflow-hidden rounded-xl border border-warn/50 bg-warn/5">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-warn/30 bg-warn/10 px-4 py-3">
        <div>
          <h2 className="text-sm font-semibold text-warn">Plan awaiting your approval</h2>
          <p className="text-xs text-muted">The run is parked until you decide.</p>
        </div>
        {!rejecting && (
          <div className="flex gap-2">
            <Button disabled={busy} onClick={onApprove}>
              Approve plan
            </Button>
            <Button variant="secondary" disabled={busy} onClick={() => setRejecting(true)}>
              Reject with reason
            </Button>
          </div>
        )}
      </div>
      <div className="p-4">
        {plan ? (
          <div className="max-h-96 overflow-auto rounded-lg border border-edge bg-surface p-3">
            <Markdown content={plan} />
          </div>
        ) : (
          <p className="text-sm text-faint">The agent has not attached a plan body.</p>
        )}
        {rejecting && (
          <div className="mt-3 space-y-2">
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
      </div>
    </div>
  );
}

function FollowUpComposer({
  busy,
  onStop,
  onSend,
}: {
  busy: boolean;
  onStop: () => void;
  onSend: (text: string) => void;
}) {
  const [text, setText] = useState("");
  const send = () => {
    const t = text.trim();
    if (!t) return;
    onSend(t);
    setText("");
  };
  return (
    <Card className="space-y-3 p-4">
      <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Steer this run</h2>
      <Textarea
        rows={2}
        placeholder="Send a follow-up message (resumes the agent as its next turn)"
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="flex gap-2">
        <Button disabled={busy || text.trim() === ""} onClick={send}>
          Send follow-up
        </Button>
        <Button variant="danger" disabled={busy} onClick={onStop}>
          Stop run
        </Button>
      </div>
    </Card>
  );
}
