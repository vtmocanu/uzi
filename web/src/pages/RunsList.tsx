// Runs index. Active runs are always visible; past (terminal) runs collapse
// behind "Show past runs (N)" — the pattern multica uses for per-issue
// execution logs (packages/views/issues/components/execution-log-section.tsx),
// including its sort rule that failed outranks cancelled outranks completed at
// equal timestamps (PAST_STATUS_RANK there). The row status pill keeps PRD #12's
// "a deliberate stop is not a failure" nuance: isStoppedRun collapses cancelled /
// stop_kind-stamped-failed runs (PRD #33) to a calm "stopped" pill, not "failed".

import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, isTerminalRun, type AdminWorker, type RunListItem, type RunUsage, type TriageCounts } from "../lib/api";
import { Alert, Badge, Card, EmptyState, ListSkeleton, PageHeader, SectionTitle, StatusPill } from "../components/ui";
import { TriageSummary } from "./RunView";
import { ActivityIcon, ChevronDownIcon, ChevronRightIcon } from "../components/icons";
import { MrChip } from "../components/MrChip";
import { mrAbbrev } from "../lib/forgeNoun";
import { isStoppedRun, mrChipState } from "../lib/runBadge";
import { formatTokens, formatCost } from "../lib/formatTokens";
import { hasTemplateDrift } from "../lib/workerTemplates";
import { WorkerRunBadge } from "../components/WorkerRunBadge";
import { RunHealthBadge } from "../components/RunHealthBadge";

const PAST_STATUS_RANK: Record<string, number> = { failed: 0, cancelled: 1, completed: 2 };

// The meta line's "tok" figure is the run's ALL-token total (fresh + cached + cache
// creation + output), matching the mock's single "1.33M tok".
function runUsageTotalTokens(u: RunUsage): number {
  return u.input_tokens + u.cache_read_tokens + u.cache_creation_tokens + u.output_tokens;
}

function sortPast(a: RunListItem, b: RunListItem): number {
  const t = b.updated_at.localeCompare(a.updated_at);
  if (t !== 0) return t;
  return (PAST_STATUS_RANK[a.status] ?? 3) - (PAST_STATUS_RANK[b.status] ?? 3);
}

function RunRow({
  run,
  showOwner,
  waitingForVault = false,
}: {
  run: RunListItem;
  showOwner?: boolean;
  // waitingForVault (PRD #32): this is the current user's own queued run and their
  // vault is locked, so it will not claim until they unlock — surfaced as a distinct
  // amber state instead of a bare "queued" pill.
  waitingForVault?: boolean;
}) {
  // A deliberate human stop (cancelled, or failed carrying a server-stamped
  // stop_kind — PRD #33) reads "stopped" / neutral, never "failed" / danger. Fold
  // that into the pill's status so the shared StatusPill palette renders it calm.
  const pillStatus = isStoppedRun(run.status, run.stop_kind) ? "stopped" : run.status;
  // MR chip state (PRD #33): open renders exactly as before; merged/closed get a
  // label and closed is muted + struck. This is a per-run frozen hint.
  const mrState = mrChipState(run.mr_state);
  return (
    <li>
      <Link
        to={`/runs/${run.id}`}
        className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 transition-colors hover:border-edge-strong hover:bg-raised/70"
      >
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm font-medium text-fg">{run.issue_title}</p>
          <p className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-faint">
            <span>
              {run.repo_path} #{run.issue_iid}
            </span>
            {run.worker_name && <span>· {run.worker_name}</span>}
            {showOwner && run.owner_email && <span>· {run.owner_email}</span>}
            <span>· {new Date(run.updated_at).toLocaleString()}</span>
            {run.mr_iid != null && (
              <MrChip
                variant="inline"
                label={`· ${mrAbbrev(run.forge_type)} `}
                forgeType={run.forge_type}
                mrIid={run.mr_iid}
                mrState={mrState}
                href={null}
                className="font-medium"
              />
            )}
            {/* PRD #40: tokens + cost join the meta line; hidden for a run with no
                usage rows (a pre-feature run) — never a fabricated 0. A running run
                shows its "so far" figure, which grows as phases fold. */}
            {run.usage && (
              <>
                <span className="font-mono tabular-nums">
                  · {formatTokens(runUsageTotalTokens(run.usage))} tok
                  {run.status === "running" ? " so far" : ""}
                </span>
                {run.usage.cost_usd > 0 && (
                  <span className="font-mono text-brand/90">· {formatCost(run.usage.cost_usd)}</span>
                )}
              </>
            )}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {run.auto_approve && (
            <Badge tone="brand" title="Autopilot: started from the label, plan auto-approved">
              autopilot
            </Badge>
          )}
          {waitingForVault ? (
            <Badge tone="warning" title="This run will claim once you unlock your vault.">
              <span aria-hidden="true">🔒</span> waiting for vault unlock
            </Badge>
          ) : (
            <>
              {/* The health flag (PRD #47) sits beside the status pill; hidden here
                  when waitingForVault already explains a locked queued run. */}
              <RunHealthBadge run={run} />
              <StatusPill status={pillStatus} />
            </>
          )}
        </div>
      </Link>
    </li>
  );
}

export function RunsList() {
  const { user, vaultUnlocked } = useAuth();
  const isAdmin = !!user?.is_admin;

  const [runs, setRuns] = useState<RunListItem[]>([]);
  const [adminRuns, setAdminRuns] = useState<RunListItem[]>([]);
  const [adminWorkers, setAdminWorkers] = useState<AdminWorker[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [showPast, setShowPast] = useState(false);
  // Global judge-triage backlog (PRD #94): the caller's counts across ALL their runs,
  // all-time. It DELIBERATELY ignores the list's filters/paging — it is a global
  // backlog, not the filtered view — so it rides its own fetch, not `load`.
  const [judgeStats, setJudgeStats] = useState<TriageCounts | null>(null);

  const load = useCallback(async () => {
    setError("");
    try {
      const [{ runs }, admin] = await Promise.all([
        api.listRuns(),
        isAdmin ? Promise.all([api.adminListRuns(), api.adminListWorkers()]) : Promise.resolve(null),
      ]);
      setRuns(runs);
      if (admin) {
        setAdminRuns(admin[0].runs);
        setAdminWorkers(admin[1].workers);
      }
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load runs");
    } finally {
      setLoading(false);
    }
  }, [isAdmin]);

  useEffect(() => {
    load();
  }, [load]);

  // The global strip fetches once on mount. On error (or total 0) it stays hidden —
  // an unobtrusive backlog, never a loud failure surface on the runs page.
  useEffect(() => {
    let alive = true;
    api
      .getJudgeStats()
      .then((stats) => {
        if (alive) setJudgeStats(stats);
      })
      .catch(() => {
        if (alive) setJudgeStats(null);
      });
    return () => {
      alive = false;
    };
  }, []);

  const active = runs.filter((r) => !isTerminalRun(r.status));
  const past = runs.filter((r) => isTerminalRun(r.status)).sort(sortPast);

  return (
    <div className="space-y-6">
      <PageHeader title="Runs" description="Your agent runs. Open one to watch it live." />

      {judgeStats && judgeStats.total > 0 && (
        <TriageSummary
          triage={judgeStats}
          title="Judge recommendations · all your runs"
          aside="all time"
        />
      )}

      {error && <Alert message={error} />}
      {loading && <ListSkeleton rows={4} />}

      {!loading && (
        <>
          {active.length === 0 && past.length === 0 ? (
            <EmptyState
              icon={<ActivityIcon />}
              title="No runs yet"
              description="Open a board and press Start run on a PRD card — the agent plans, waits for your approval, then implements and opens a merge/pull request."
              action={
                <Link to="/repos" className="text-sm font-medium text-brand hover:text-brand-hover">
                  Go to boards →
                </Link>
              }
            />
          ) : (
            <div className="space-y-4">
              <div className="space-y-2">
                <SectionTitle>Active</SectionTitle>
                {active.length === 0 ? (
                  <p className="text-sm text-faint">Nothing in flight right now.</p>
                ) : (
                  <ul className="space-y-2">
                    {active.map((r) => (
                      <RunRow
                        key={r.id}
                        run={r}
                        waitingForVault={!vaultUnlocked && r.status === "queued"}
                      />
                    ))}
                  </ul>
                )}
              </div>

              {past.length > 0 && (
                <div className="space-y-2">
                  <button
                    type="button"
                    onClick={() => setShowPast((v) => !v)}
                    aria-expanded={showPast}
                    className="inline-flex items-center gap-1 text-xs font-semibold uppercase tracking-wider text-faint transition-colors hover:text-muted"
                  >
                    {showPast ? <ChevronDownIcon /> : <ChevronRightIcon />}
                    {showPast ? "Past runs" : `Show past runs (${past.length})`}
                  </button>
                  {showPast && (
                    <ul className="space-y-2">
                      {past.map((r) => (
                        <RunRow key={r.id} run={r} />
                      ))}
                    </ul>
                  )}
                </div>
              )}
            </div>
          )}
        </>
      )}

      {isAdmin && !loading && (
        <Card className="space-y-4 border-brand/20">
          <SectionTitle className="text-brand">Factory status (admin)</SectionTitle>
          <div>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wider text-faint">
              Active runs · all users
            </h3>
            {adminRuns.length === 0 ? (
              <p className="text-sm text-faint">No active runs across the factory.</p>
            ) : (
              <ul className="space-y-2">
                {adminRuns.map((r) => (
                  <RunRow
                    key={r.id}
                    run={r}
                    showOwner
                    // Only the current admin's OWN queued rows can show the vault state —
                    // another owner's vault status is unknown here (PRD #32), so theirs
                    // render as plain "queued".
                    waitingForVault={
                      !vaultUnlocked && r.status === "queued" && r.owner_email === user?.email
                    }
                  />
                ))}
              </ul>
            )}
          </div>
          <div>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wider text-faint">
              Workers · all users
            </h3>
            {adminWorkers.length === 0 ? (
              <p className="text-sm text-faint">No workers registered.</p>
            ) : (
              <ul className="space-y-2">
                {adminWorkers.map((w) => (
                  <li
                    key={w.id}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2 text-sm"
                  >
                    <div>
                      <span className="font-medium text-fg">{w.name}</span>
                      <span className="ml-2 text-xs text-faint">{w.owner_email}</span>
                      {(w.template_reported || w.template_declared) && (
                        <span className="ml-2 text-xs text-faint">
                          template {w.template_reported ?? `${w.template_declared} (declared)`}
                        </span>
                      )}
                    </div>
                    <div className="flex items-center gap-1.5">
                      {hasTemplateDrift(w.template_declared, w.template_reported) && (
                        <Badge
                          tone="warning"
                          title={`Declared ${w.template_declared}, worker reports ${w.template_reported}`}
                        >
                          template drift
                        </Badge>
                      )}
                      <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
                        {w.status}
                      </Badge>
                      <WorkerRunBadge worker={w} />
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </Card>
      )}
    </div>
  );
}
