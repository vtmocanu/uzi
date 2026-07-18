// Live run view. The header carries the status pill plus a LIVE STAGE label
// derived from the newest message — multica's task-status-pill idea
// (packages/views/chat/components/task-status-pill.tsx maps the latest tool
// slug to a human stage: "Running command", "Reading files", "Making edits"…)
// — so you can tell what the agent is doing without reading the feed. Terminal
// states get a hero banner: the MR link is the run's entire output and must
// not hide in chrome. The breadcrumb keeps PRD #12's in-app board / issue links.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  api,
  ApiError,
  isHttpsUrl,
  preferForgeUrl,
  isTerminalRun,
  type AgentSelectionInput,
  type Repo,
  type Run,
  type RunMessage,
  type RunReview,
} from "../lib/api";
import { recommendationLabel, verdictLabel, verdictTone } from "../lib/judge";
import { AgentPicker, selectionLabel, type OwnTemplate } from "../components/AgentPicker";
import {
  formatElapsed,
  healthFlagLabel,
  isStoppedRun,
  mrChipState,
  mrChipSuffix,
  mrChipTitle,
  shouldShowHealthFlag,
} from "../lib/runBadge";
import { forgeNounLower, mrAbbrev, mrRefSymbol } from "../lib/forgeNoun";
import { useRunStream } from "../lib/useRunStream";
import { deriveRunUsage } from "../lib/runUsage";
import { CIFixRunHeader } from "../components/CIFixRunHeader";
import { formatDuration } from "../components/RunEvent";
import { RunUsagePanel } from "../components/RunUsage";
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

// HealthFlag renders the run-health warn chip next to the LIVE STAGE label (PRD #47
// Decision 10): `⚠ <label> · stuck for Xm — <reason>`. The run view is owner/admin
// only, so health_reason is always present here (no gating needed). It ticks the
// "stuck for Xm" coarsely (30s) since a stalled run emits no messages to force a
// re-render. Renders nothing for a healthy run or a non-flaggable status.
function HealthFlag({ run }: { run: Run }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(id);
  }, []);
  if (!shouldShowHealthFlag(run.health, run.status)) return null;
  const since = run.health_since ? Date.parse(run.health_since) : NaN;
  const stuck = Number.isFinite(since) ? ` · stuck for ${formatElapsed(now - since)}` : "";
  return (
    // role="status" so a screen reader announces the flag when it arrives over WS.
    // The reason is shown inline below, so no title tooltip (it would be redundant).
    <span
      role="status"
      className="inline-flex items-center gap-1 rounded-full border border-warn/40 bg-warn/10 px-2 py-0.5 text-[11px] font-medium text-warn"
    >
      ⚠ {healthFlagLabel(run.health)}
      {stuck}
      {run.health_reason && <span className="font-normal"> — {run.health_reason}</span>}
    </span>
  );
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

  // PRD #40: usage derived client-side from the stream (Decision 5) — a pure
  // reduction re-run as messages grow, so it folds in live (Decision 9) with no
  // accumulator. Feeds the usage panel + the per-phase finish lines in the feed.
  const usage = useMemo(() => deriveRunUsage(messages), [messages]);

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
              {/* Run-health warn chip (PRD #47), next to the LIVE STAGE label. */}
              <HealthFlag run={run} />
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
                    {run.mr_iid != null &&
                      ` — ${forgeNounLower(run.forge_type)} ${mrState === "merged" ? "merged" : mrState === "closed" ? "closed" : "opened"}.`}
                  </>
                )}
              </p>
            </div>
            {mrUrl ? (
              <a href={mrUrl} target="_blank" rel="noreferrer">
                <Button>
                  Open {forgeNounLower(run.forge_type)} {mrRefSymbol(run.forge_type)}
                  {run.mr_iid}
                  {mrChipSuffix(mrState)} <ExternalLinkIcon />
                </Button>
              </a>
            ) : (
              run.mr_iid != null && (
                <Badge tone={mrState === "closed" ? "neutral" : "ok"} title={mrChipTitle(mrState, run.forge_type)}>
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
        </div>
      )}

      {/* Run retrospective (PRD #46 M4): the LLM judge's verdict + recommendations,
          shown once a run is finished. The panel fetches its own review and owns the
          re-run action; it renders nothing for a non-terminal or ineligible run. */}
      {terminal && <JudgePanel run={run} />}

      {run.status === "awaiting_approval" && (
        <PlanPanel
          run={run}
          busy={busy}
          onApprove={(selection) => act(() => submit("approve_plan", "", selection))}
          onReject={(reason) => act(() => submit("reject_plan", reason))}
        />
      )}

      {/* Read-only record of which agents the run used, once a selection is made
          (at the gate or by an autopilot default). Shown for a live/terminal run;
          the picker above owns the awaiting_approval state. PRD #37 Decision 3(b). */}
      {run.status !== "awaiting_approval" && run.agent_source && (
        <AgentRosterSummary run={run} />
      )}

      {usage.hasUsage && (
        <Card className="p-4">
          <RunUsagePanel usage={usage} />
        </Card>
      )}

      <Card className="p-4">
        <ActivityFeed
          messages={messages}
          runningLive={run.status === "running"}
          connected={connected}
          terminal={terminal}
          phaseUsageBySeq={usage.phaseUsageBySeq}
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
// the page while it is pending. Grows the PRD #37 agent picker: the user chooses
// the subagent roster (repo agents when detected, else their templates) with the
// approve verdict; the choice is submitted as a structured selection on approve.
export function PlanPanel({
  run,
  busy,
  onApprove,
  onReject,
}: {
  run: Run;
  busy: boolean;
  onApprove: (selection: AgentSelectionInput) => void;
  onReject: (reason: string) => void;
}) {
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState("");

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

  return (
    <div className="overflow-hidden rounded-xl border border-warn/50 bg-warn/5">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-warn/30 bg-warn/10 px-4 py-3">
        <div>
          <h2 className="text-sm font-semibold text-warn">Plan awaiting your approval</h2>
          <p className="text-xs text-muted">The run is parked until you decide. Agent choice locks in on approval.</p>
        </div>
        {!rejecting && (
          <div className="flex gap-2">
            <Button disabled={busy} onClick={() => onApprove(selection)}>
              {approveLabel}
            </Button>
            <Button variant="secondary" disabled={busy} onClick={() => setRejecting(true)}>
              Reject with reason
            </Button>
          </div>
        )}
      </div>
      <div className="space-y-4 p-4">
        <AgentPicker repoAgents={repoAgents} ownTemplates={ownTemplates} onChange={onSelectionChange} />

        {run.plan_md ? (
          <div className="max-h-96 overflow-auto rounded-lg border border-edge bg-surface p-3">
            <Markdown content={run.plan_md} />
          </div>
        ) : (
          <p className="text-sm text-faint">The agent has not attached a plan body.</p>
        )}
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
      </div>
    </div>
  );
}

// AgentRosterSummary: the read-only record of the locked-in selection, shown once
// a run is past the gate (PRD #37 Decision 3b). For a repo-source run it says so
// plainly, so a reader knows the internal review loop was repo-authored. Repo
// names/descriptions render as plain JSX text, never <Markdown>.
export function AgentRosterSummary({ run }: { run: Run }) {
  const excluded = new Set(run.agent_exclusions ?? []);
  const roster = run.agent_source === "repo" ? (run.repo_agents ?? []) : (run.own_agents ?? []);
  // own_agents now carries the own-source roster on the detail read, so this lists
  // the actual agent names for either source (M4-fix) instead of nothing for own.
  const included = roster.filter((a) => !excluded.has(a.name)).map((a) => a.name);
  return (
    <Card className="space-y-2 p-4">
      <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Agents used</h2>
      {run.agent_source === "repo" ? (
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

// Only issue / ci_fix runs are judged (the enqueue allowlist); a chat/judge/
// self_improve run never has a review, so the panel is hidden for those kinds.
const JUDGE_ELIGIBLE_KINDS = new Set(["issue", "ci_fix"]);

// JudgePanel is the run retrospective (PRD #46 M4): the LLM judge's verdict +
// structured recommendations, plus the "re-run judge" action. It fetches its own
// review (owner-or-admin scoped server-side) and, after a re-run, polls a bounded
// number of times for the fresh verdict (the new judge run finishes asynchronously).
// All judge free text (summary, rationale, target) renders as escaped React text —
// never markdown/HTML — since it is untrusted judge/worker output (audit carry-forward).
export function JudgePanel({ run }: { run: Run }) {
  const [review, setReview] = useState<RunReview | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadErr, setLoadErr] = useState("");
  const [actionErr, setActionErr] = useState("");
  const [rerunning, setRerunning] = useState(false);
  const [queued, setQueued] = useState(false);
  // The verdict's updated_at at the moment a re-run was fired; the poll below stops
  // once the review's updated_at moves past it (or a first-ever review lands).
  const baselineUpdatedAt = useRef<string | null>(null);

  const eligible = JUDGE_ELIGIBLE_KINDS.has(run.kind);

  const fetchReview = useCallback(async () => {
    try {
      const { review } = await api.getRunReview(run.id);
      setReview(review);
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

  // Bounded background poll after a re-run: the fresh verdict arrives when the new
  // judge run finishes, so check every few seconds for a changed updated_at, giving
  // up after ~1 minute so a stuck/queued judge doesn't poll forever.
  useEffect(() => {
    if (!queued) return;
    let tries = 0;
    const id = setInterval(async () => {
      tries += 1;
      let next: RunReview | null = null;
      try {
        next = (await api.getRunReview(run.id)).review;
      } catch {
        next = null;
      }
      if (next && next.updated_at !== baselineUpdatedAt.current) {
        setReview(next);
        setQueued(false);
      } else if (tries >= 15) {
        setQueued(false);
      }
    }, 4000);
    return () => clearInterval(id);
  }, [queued, run.id]);

  const rerun = async () => {
    setActionErr("");
    setRerunning(true);
    try {
      await api.rerunJudge(run.id);
      baselineUpdatedAt.current = review?.updated_at ?? null;
      setQueued(true);
    } catch (e) {
      setActionErr(e instanceof ApiError ? e.message : "Could not re-run the judge");
    } finally {
      setRerunning(false);
    }
  };

  if (!eligible) return null;
  if (loading) {
    return <Card className="animate-pulse p-4 text-sm text-faint">Loading review…</Card>;
  }

  const rerunLabel = review ? "Re-run judge" : "Run judge";
  const rerunButton = (
    <Button variant="secondary" size="sm" disabled={rerunning || queued} onClick={rerun}>
      {rerunning ? "Re-queuing…" : rerunLabel}
    </Button>
  );

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
          {review?.judge_model && <span className="text-xs text-faint">via {review.judge_model}</span>}
        </div>
        {rerunButton}
      </div>

      {actionErr && <Alert message={actionErr} />}
      {loadErr && <Alert message={loadErr} />}
      {queued && (
        <p className="text-xs text-info">Judge re-queued — the new verdict will appear here when it finishes.</p>
      )}

      {!review ? (
        <p className="text-sm text-faint">
          This run hasn't been judged yet. Running the judge reviews the run on your Anthropic token.
        </p>
      ) : (
        <>
          {/* summary_md and each rationale_md below are UNTRUSTED judge/worker output.
              They are DELIBERATELY rendered as escaped plain text (React's default +
              whitespace-pre-wrap), never markdown/HTML. If these are ever switched to a
              markdown/HTML renderer, add sanitization first: the review-POST ingest scrub
              (ScrubSecrets + control-strip) does NOT cover markdown/link injection. */}
          {review.summary_md.trim() !== "" && (
            <p className="whitespace-pre-wrap text-sm text-muted">{review.summary_md}</p>
          )}
          {review.recommendations.length > 0 ? (
            <ul className="space-y-2">
              {review.recommendations.map((rec) => (
                <li key={rec.id} className="rounded-lg border border-edge bg-raised/40 px-3 py-2.5">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge tone="info">{recommendationLabel(rec.category)}</Badge>
                    {rec.target.trim() !== "" && (
                      <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">{rec.target}</code>
                    )}
                    {rec.confidence && <span className="text-xs text-faint">{rec.confidence} confidence</span>}
                  </div>
                  {rec.rationale_md.trim() !== "" && (
                    <p className="mt-1.5 whitespace-pre-wrap text-sm text-muted">{rec.rationale_md}</p>
                  )}
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-sm text-faint">No recommendations — the judge found nothing to change.</p>
          )}
        </>
      )}
    </Card>
  );
}
