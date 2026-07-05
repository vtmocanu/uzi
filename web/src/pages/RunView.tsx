import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, ApiError, isHttpsUrl, isTerminalRun, type Repo, type Run } from "../lib/api";
import { useRunStream } from "../lib/useRunStream";
import { formatDuration } from "../components/RunEvent";
import { ActivityFeed } from "../components/ActivityFeed";
import { Markdown } from "../components/Markdown";
import { Alert, Badge, Button, Card } from "../components/ui";

// statusTone maps a run status to a badge tone.
function statusTone(status: string): "neutral" | "warning" | "danger" {
  if (status === "awaiting_approval") return "warning";
  if (status === "failed" || status === "cancelled") return "danger";
  return "neutral";
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
  return <span className="text-xs text-slate-500">{formatDuration(now - start)}</span>;
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

  if (!run) {
    return (
      <div className="space-y-4">
        {error ? <Alert message={error} /> : <p className="text-slate-500">Loading run…</p>}
        <Link to="/runs" className="text-sm text-indigo-400 hover:text-indigo-300">
          Back to runs
        </Link>
      </div>
    );
  }

  const terminal = isTerminalRun(run.status);
  const mrUrl =
    run.mr_iid != null && isHttpsUrl(repoWebUrl)
      ? `${repoWebUrl}/-/merge_requests/${run.mr_iid}`
      : null;

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <nav className="mb-2 flex items-center gap-1.5 text-xs text-slate-500">
            <Link to={`/repos/${run.repo_id}/board`} className="hover:text-indigo-300">
              Board
            </Link>
            <span>/</span>
            <Link
              to={`/repos/${run.repo_id}/issues/${run.issue_iid}`}
              className="hover:text-indigo-300"
            >
              #{run.issue_iid}
            </Link>
            <span>/</span>
            <span className="text-slate-400">Run</span>
          </nav>
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-2xl font-semibold">{run.issue_title}</h1>
            <span className="text-sm text-slate-500">#{run.issue_iid}</span>
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
            <Badge tone={statusTone(run.status)}>{run.status.replace("_", " ")}</Badge>
            <span
              title={connected ? "Live" : "Reconnecting…"}
              className={`inline-flex items-center gap-1 text-xs ${
                connected ? "text-emerald-400" : "text-slate-500"
              }`}
            >
              <span
                className={`h-2 w-2 rounded-full ${connected ? "bg-emerald-400" : "bg-slate-600"}`}
              />
              {connected ? "live" : "offline"}
            </span>
            {run.status === "running" && run.started_at && <LiveElapsed since={run.started_at} />}
            {run.iteration_count > 0 && (
              <span className="text-xs text-slate-500">iteration {run.iteration_count}</span>
            )}
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          {mrUrl && (
            <a href={mrUrl} target="_blank" rel="noreferrer">
              <Button variant="ghost">Merge request !{run.mr_iid}</Button>
            </a>
          )}
          <Link to="/runs">
            <Button variant="ghost">Back to runs</Button>
          </Link>
        </div>
      </div>

      {error && <Alert message={error} />}
      {actionErr && <Alert message={actionErr} />}

      {run.failure_reason && (
        <div className="rounded-lg border border-rose-800 bg-rose-950/50 px-3 py-2 text-sm text-rose-200">
          {run.failure_reason}
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

      <Card>
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

      {terminal && <TerminalSummary run={run} />}
    </div>
  );
}

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
    <Card className="space-y-3 border-amber-800/70">
      <h2 className="text-sm font-semibold uppercase tracking-wide text-amber-300">
        Plan awaiting approval
      </h2>
      {plan ? (
        <div className="max-h-96 overflow-auto rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <Markdown content={plan} />
        </div>
      ) : (
        <p className="text-sm text-slate-500">The agent has not attached a plan body.</p>
      )}
      {rejecting ? (
        <div className="space-y-2">
          <textarea
            className="w-full rounded-lg border border-slate-700 bg-slate-900 px-3 py-2 text-sm text-slate-100 outline-none focus:border-indigo-400"
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
      ) : (
        <div className="flex gap-2">
          <Button disabled={busy} onClick={onApprove}>
            Approve plan
          </Button>
          <Button variant="ghost" disabled={busy} onClick={() => setRejecting(true)}>
            Reject with reason
          </Button>
        </div>
      )}
    </Card>
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
    <Card className="space-y-3">
      <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">Steer this run</h2>
      <textarea
        className="w-full rounded-lg border border-slate-700 bg-slate-900 px-3 py-2 text-sm text-slate-100 outline-none focus:border-indigo-400"
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

function TerminalSummary({ run }: { run: Run }) {
  const duration =
    run.started_at && run.finished_at
      ? formatDuration(new Date(run.finished_at).getTime() - new Date(run.started_at).getTime())
      : null;
  return (
    <Card className="text-sm text-slate-400">
      This run is <span className="font-medium text-slate-200">{run.status}</span>.
      {duration && <> Ran for {duration}.</>}
      {run.branch && (
        <>
          {" "}
          Branch <code className="rounded bg-slate-800 px-1 py-0.5 text-slate-200">{run.branch}</code>.
        </>
      )}
    </Card>
  );
}
