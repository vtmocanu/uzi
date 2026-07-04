import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  api,
  ApiError,
  isHttpsUrl,
  isTerminalRun,
  type Repo,
  type Run,
  type RunMessage,
} from "../lib/api";
import { useRunStream } from "../lib/useRunStream";
import { Alert, Badge, Button, Card } from "../components/ui";

// statusTone maps a run status to a badge tone.
function statusTone(status: string): "neutral" | "warning" | "danger" {
  if (status === "awaiting_approval") return "warning";
  if (status === "failed" || status === "cancelled") return "danger";
  return "neutral";
}

// agentGroup is a run of consecutive messages produced by the same agent.
interface AgentGroup {
  agent: string;
  messages: RunMessage[];
}

function groupByAgent(messages: RunMessage[]): AgentGroup[] {
  const groups: AgentGroup[] = [];
  for (const m of messages) {
    const agent = m.agent ?? "lead";
    const last = groups[groups.length - 1];
    if (last && last.agent === agent) {
      last.messages.push(m);
    } else {
      groups.push({ agent, messages: [m] });
    }
  }
  return groups;
}

// payloadText renders a message payload as readable text, degrading to pretty
// JSON for structured kinds (tool_use/tool_result/…) whose shape the run view
// does not model.
function payloadText(payload: unknown): string {
  if (typeof payload === "string") return payload;
  if (payload && typeof payload === "object") {
    const obj = payload as Record<string, unknown>;
    if (typeof obj.text === "string") return obj.text;
    if (typeof obj.message === "string") return obj.message;
  }
  try {
    return JSON.stringify(payload, null, 2);
  } catch {
    return String(payload);
  }
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

  const groups = useMemo(() => groupByAgent(messages), [messages]);
  const activeAgent = run && run.status === "running" ? groups[groups.length - 1]?.agent : undefined;

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

      <Card className="space-y-4">
        <div className="flex items-center justify-between">
          <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
            Activity
          </h2>
          <span className="text-xs text-slate-500">{messages.length} messages</span>
        </div>
        {groups.length === 0 ? (
          <p className="py-6 text-center text-sm text-slate-600">
            {terminal ? "No messages were recorded for this run." : "Waiting for the agent…"}
          </p>
        ) : (
          <div className="space-y-4">
            {groups.map((g, i) => (
              <AgentBlock key={i} group={g} live={g.agent === activeAgent} />
            ))}
          </div>
        )}
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

function AgentBlock({ group, live }: { group: AgentGroup; live: boolean }) {
  return (
    <div className="rounded-lg border border-slate-800 bg-slate-900/40 p-3">
      <div className="mb-2 flex items-center gap-2">
        <span className="text-sm font-semibold text-slate-200">{group.agent}</span>
        <Badge tone={live ? "warning" : "neutral"} title={live ? "Most recent activity" : "Idle"}>
          {live ? "active" : "idle"}
        </Badge>
      </div>
      <div className="space-y-2">
        {group.messages.map((m) => (
          <MessageRow key={m.seq} msg={m} />
        ))}
      </div>
    </div>
  );
}

function MessageRow({ msg }: { msg: RunMessage }) {
  const text = payloadText(msg.payload);
  return (
    <div className="text-sm">
      <div className="flex items-center gap-2">
        <span className="rounded bg-slate-800 px-1.5 py-0.5 text-[11px] font-medium text-slate-400">
          {msg.kind}
        </span>
        <span className="text-[11px] text-slate-600">
          {new Date(msg.created_at).toLocaleTimeString()}
        </span>
      </div>
      <pre className="mt-1 whitespace-pre-wrap break-words font-sans text-slate-200">{text}</pre>
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
        <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-slate-800 bg-slate-900/60 p-3 font-sans text-sm text-slate-200">
          {plan}
        </pre>
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
  return (
    <Card className="text-sm text-slate-400">
      This run is <span className="font-medium text-slate-200">{run.status}</span>.
      {run.branch && (
        <>
          {" "}
          Branch <code className="rounded bg-slate-800 px-1 py-0.5 text-slate-200">{run.branch}</code>.
        </>
      )}
    </Card>
  );
}
