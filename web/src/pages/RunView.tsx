// Live run view. The header carries the status pill plus a LIVE STAGE label
// derived from the newest message — multica's task-status-pill idea
// (packages/views/chat/components/task-status-pill.tsx maps the latest tool
// slug to a human stage: "Running command", "Reading files", "Making edits"…)
// — so you can tell what the agent is doing without reading the feed. Terminal
// states get a hero banner: the MR link is the run's entire output and must
// not hide in chrome. The breadcrumb keeps PRD #12's in-app board / issue links.

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import {
  api,
  ApiError,
  isHttpsUrl,
  preferForgeUrl,
  isTerminalRun,
  type AgentSelectionInput,
  type Disposition,
  type FiledIssue,
  type IssueDraft,
  type Repo,
  type ReviewRecommendation,
  type Run,
  type RunMessage,
  type RunReview,
  type TriageCounts,
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
import { Alert, Badge, Button, Card, Input, PageHeader, Select, Spinner, StatusPill, Textarea, cx } from "../components/ui";
import { ExternalLinkIcon, FileTextIcon } from "../components/icons";

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

// coordKey is the SINGLE source of truth for the (category, target) key that matches a
// recommendation to its filed link (PRD #68). It MUST be used at both the build and the
// lookup site — a separator mismatch silently drops a persisted filed link back to the
// idle "File issue" button (the row 409s on Create, the stale flag never fires). category
// is a fixed enum with no spaces, so a single space cleanly separates it from the
// arbitrary target.
function coordKey(category: string, target: string): string {
  return `${category} ${target}`;
}

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
  // The caller's connected repos back the file-issue draft picker (PRD #68 M4). Fetched
  // once for the panel; a failure just leaves the picker empty (the draft still opens).
  const [repos, setRepos] = useState<Repo[]>([]);
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
                        <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">{rec.target}</code>
                      )}
                      {rec.confidence && <span className="text-xs text-faint">{rec.confidence} confidence</span>}
                      <span className="ml-auto">
                        <DispositionChip disp={disp} filedSettled={filed !== undefined} />
                      </span>
                    </div>
                    {rec.rationale_md.trim() !== "" && (
                      <p className="mt-1.5 whitespace-pre-wrap text-sm text-muted">{rec.rationale_md}</p>
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
      setTitle(draft.title);
      setDescription(draft.description);
    } catch (e) {
      setDraftErr(e instanceof ApiError ? e.message : "Could not load the draft");
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
      setFileErr(e instanceof ApiError ? e.message : "Could not file the issue");
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
                    {r.path_with_namespace}
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
      onError(e instanceof ApiError ? e.message : "Could not update the disposition");
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
        <TriageCount dotClass="bg-edge-strong" n={triage.todo} label="to do" />
        <TriageCount dotClass="bg-info" n={triage.filed} label="filed" />
        <TriageCount dotClass="bg-ok" n={triage.done} label="done" />
        <TriageCount dotClass="bg-muted/60" n={triage.dismissed} label="dismissed" />
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
// total of 0 yields an empty track.
function TriageMeter({ triage }: { triage: TriageCounts }) {
  const total = triage.total;
  const seg = (n: number, cls: string, key: string) =>
    n > 0 && total > 0 ? (
      <span key={key} className={cx("h-full", cls)} style={{ width: `${(n / total) * 100}%` }} />
    ) : null;
  return (
    <div className="mt-2 flex h-2 overflow-hidden rounded-full bg-raised" aria-hidden="true">
      {seg(triage.todo, "bg-edge-strong", "todo")}
      {seg(triage.filed, "bg-info", "filed")}
      {seg(triage.done, "bg-ok", "done")}
      {seg(triage.dismissed, "bg-muted/60", "dismissed")}
    </div>
  );
}
