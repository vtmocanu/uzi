// The overview: stat tiles, an onboarding checklist derived from the same
// preconditions the board's start-run gate checks (lib/runStream.ts
// startRunGate), and the most recent runs. Replaces the old account-metadata
// table (moved to Settings), which told a new user nothing about what to do
// next.

import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, isTerminalRun, type AdminUsage, type RunListItem, type SelfUsage, type Worker } from "../lib/api";
import { hasAnthropicToken } from "../lib/hasToken";
import { effectiveRunStatus, milestoneBadge, milestoneBadgeText, mrChipState } from "../lib/runBadge";
import { MrChip } from "../components/MrChip";
import { RunIssueRef } from "../components/RunIssueRef";
import { mrAbbrev } from "../lib/forgeNoun";
import { YourUsageCard, FactoryTotalCard, PerUserUsageTable } from "../components/UsageCards";
import { RunHealthBadge } from "../components/RunHealthBadge";
import { WorkerStatLine, hasStats } from "../components/WorkerStats";
import { WorkerCordonBadge } from "../components/WorkerCordonBadge";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { Badge, Button, Card, cx, PageHeader, SectionTitle, Skeleton, StatTile, StatusPill } from "../components/ui";
import { CheckIcon, ChevronRightIcon } from "../components/icons";
import { stripUnsafeChars } from "../lib/safeText";
import { useDemoMode } from "../lib/demoMode";
import { maskName, maskRepoPath } from "../lib/demoMask";

interface Overview {
  runs: RunListItem[];
  // The full fleet drives both the online/total count and the per-worker load lines
  // (PRD #49); the mount load and the 10s poll both already fetch it.
  workers: Worker[];
  workersOnline: number;
  workersTotal: number;
  reposEnabled: number;
  templates: number;
  hasToken: boolean;
  hasForge: boolean;
  // PRD #40: the caller's own usage (everyone) + the factory view (admins only).
  // Best-effort: a usage fetch failure leaves these null and simply hides the cards,
  // never blocking the rest of the dashboard.
  selfUsage: SelfUsage | null;
  adminUsage: AdminUsage | null;
}

function Step({
  done,
  index,
  title,
  hint,
  to,
  cta,
}: {
  done: boolean;
  index: number;
  title: string;
  hint: string;
  to: string;
  cta: string;
}) {
  return (
    <li className="flex items-center gap-3 py-2.5">
      <span
        className={cx(
          "flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-xs",
          done ? "bg-ok/15 text-ok" : "bg-raised text-faint",
        )}
      >
        {done ? <CheckIcon /> : index}
      </span>
      <div className="min-w-0 flex-1">
        <p className={cx("text-sm font-medium", done ? "text-muted line-through" : "text-fg")}>
          {title}
        </p>
        {!done && <p className="text-xs text-faint">{hint}</p>}
      </div>
      {!done && (
        <Link to={to}>
          <Button variant="secondary" size="sm">
            {cta} <ChevronRightIcon />
          </Button>
        </Link>
      )}
    </li>
  );
}

export function Dashboard() {
  const demo = useDemoMode();
  const { user } = useAuth();
  const [data, setData] = useState<Overview | null>(null);

  // First load fetches everything — volatile tiles plus the rarely-changing
  // fields the onboarding checklist reads. An error here keeps `data` null so the
  // skeletons stay (skeletons only ever show pre-first-load); the background poll
  // below never reaches this path.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [{ runs }, { workers }, { repos }, { templates }, { secrets }, { connections }, selfUsage, adminUsage] =
          await Promise.all([
            api.listRuns(),
            api.listWorkers(),
            api.listRepos(),
            api.listAgentTemplates(),
            api.listSecrets(),
            api.listConnections(),
            // Usage is best-effort (.catch → null) so it can never fail the core load,
            // and the admin view is fetched ONLY for an admin — a non-admin never hits
            // the admin-gated endpoint.
            api.getUsage().catch(() => null),
            user?.is_admin ? api.getAdminUsage().catch(() => null) : Promise.resolve(null),
          ]);
        if (cancelled) return;
        setData({
          runs,
          workers,
          workersOnline: workers.filter((w) => w.status === "online").length,
          workersTotal: workers.length,
          reposEnabled: repos.filter((r) => r.enabled).length,
          templates: templates.length,
          hasToken: hasAnthropicToken(secrets),
          hasForge: connections.length > 0,
          selfUsage,
          adminUsage,
        });
      } catch {
        if (!cancelled) setData(null);
      }
    })();
    return () => {
      cancelled = true;
    };
    // Deps list `user?.is_admin` (the value the effect branches on at the
    // getAdminUsage call above), NOT the whole `user` object. ProtectedRoute renders
    // this page only once auth has resolved to a non-null user, so is_admin holds its
    // final value at mount. On the ordinary focus/vault-lock refresh path it does not
    // flip: those replace the user object but re-read the same is_admin. React compares
    // deps with Object.is per element, so listing the boolean means the effect re-runs
    // ONLY if is_admin actually changes value — which on the normal path is never, so
    // it still runs exactly once. (The one case it would change — an admin grant/revoke
    // by another admin, then a refresh — correctly re-loads the dashboard rather than
    // showing stale admin/non-admin data, so the fix is safe there too.) Listing the
    // whole `user` object instead WOULD churn on every refresh; that is why it is the
    // boolean, not the object. Resolved #200 (the M3 exhaustive-deps review).
  }, [user?.is_admin]);

  // Liveness: re-fetch only the volatile endpoints (runs, workers) every 10s while
  // the tab is visible, so a run moving to awaiting_approval or a worker dropping
  // offline shows without a reload. Repos/templates/secrets/connections change
  // rarely and stay mount-only. A poll failure keeps the last-good data — unlike
  // the first load, a transient re-poll error must NOT blank the page back to
  // skeletons.
  const poll = useCallback(async () => {
    try {
      const [{ runs }, { workers }] = await Promise.all([api.listRuns(), api.listWorkers()]);
      setData((prev) =>
        prev
          ? {
              ...prev,
              runs,
              workers,
              workersOnline: workers.filter((w) => w.status === "online").length,
              workersTotal: workers.length,
            }
          : prev,
      );
    } catch {
      // keep the last-good data
    }
  }, []);
  usePollWhileVisible(poll, 10000);

  if (!user) return null;

  // PRD #35 (web-ux F2): "active" is still every non-terminal run — the TILE's count is
  // right, and a parked run genuinely is in flight. What was wrong is the HINT, which
  // said "agents at work" over a set that can contain runs where no agent is working
  // and none will for hours.
  //
  // Split rather than excluded, deliberately: dropping parked runs from the count would
  // make them vanish from the one surface that says how much is in flight, which is the
  // opposite failure. `!isTerminalRun` as a proxy for "actively working" is the actual
  // defect, and it is only visible now because limit_wait is the first non-terminal
  // status that lasts hours by design.
  //
  // Issue #754: pool_wait is the same KIND of hold — non-terminal, self-resuming,
  // blocked on a resource (a pooled token) rather than actively working — so it joins
  // the waiting bucket too. The copy is deliberately "waiting to resume" rather than the
  // old "waiting on a usage limit": that older wording is TRUE only for limit_wait and
  // false for a pool hold, so the honest generalization is the resource-agnostic phrase
  // that holds for both.
  const active = data?.runs.filter((r) => !isTerminalRun(r.status)) ?? [];
  const waiting = active.filter((r) => r.status === "limit_wait" || r.status === "pool_wait");
  const working = active.length - waiting.length;
  // "8 at work · 1 waiting to resume" only when there is something to disambiguate; a
  // factory with nothing parked keeps exactly the copy it had.
  const activeHint = !active.length
    ? "nothing in flight"
    : waiting.length
      ? `${working} at work · ${waiting.length} waiting to resume`
      : "agents at work";
  const recent = data?.runs.slice(0, 5) ?? [];
  const steps = data
    ? [data.hasToken, data.hasForge, data.reposEnabled > 0, data.workersOnline > 0]
    : [];
  const ready = steps.length > 0 && steps.every(Boolean);

  return (
    <div className="space-y-6">
      <PageHeader
        title={`Welcome${user.display_name ? `, ${maskName(user.display_name, demo)}` : ""}`}
        description="The factory floor at a glance."
      />

      {!data ? (
        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-24" />
          ))}
        </div>
      ) : (
        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          <StatTile
            label="Active runs"
            value={active.length}
            hint={activeHint}
            to="/runs"
          />
          <StatTile
            label="Workers online"
            value={`${data.workersOnline}/${data.workersTotal}`}
            hint="your fleet"
            to="/workers"
          />
          <StatTile label="Boards" value={data.reposEnabled} hint="enabled repos" to="/repos" />
          <StatTile label="Agent templates" value={data.templates} hint="roles available" to="/agents" />
        </div>
      )}

      {/* Per-worker resource load (PRD #49): a compact "cpu · mem" line per worker
          that has reported a sample, so the factory floor shows live pressure at a
          glance. Hidden until at least one worker reports (no empty card churn). */}
      {data && data.workers.some(hasStats) && (
        <Card>
          <div className="flex items-center justify-between">
            <SectionTitle>Worker load</SectionTitle>
            <Link to="/workers" className="text-xs font-medium text-brand hover:text-brand-hover">
              Workers →
            </Link>
          </div>
          <ul className="mt-2 divide-y divide-edge">
            {data.workers
              .filter(hasStats)
              .map((w) => (
                <li key={w.id} className="flex items-center justify-between gap-3 py-2">
                  <div className="flex min-w-0 items-center gap-2">
                    <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
                      {w.status}
                    </Badge>
                    <WorkerCordonBadge worker={w} />
                    <span className="truncate text-sm font-medium text-fg">{w.name}</span>
                  </div>
                  <WorkerStatLine worker={w} />
                </li>
              ))}
          </ul>
        </Card>
      )}

      {data && !ready && (
        <Card>
          <div className="flex items-center justify-between">
            <SectionTitle>Get the factory running</SectionTitle>
            <span className="text-xs text-faint">{steps.filter(Boolean).length}/4 done</span>
          </div>
          <ol className="mt-2 divide-y divide-edge">
            <Step
              done={data.hasToken}
              index={1}
              title="Add your Anthropic token"
              hint="Agents run on your own credentials; the token is encrypted at rest."
              to="/settings"
              cta="Settings"
            />
            <Step
              done={data.hasForge}
              index={2}
              title="Connect your bot"
              hint="A bot PAT lets uzi see and move your issues."
              to="/settings/forge"
              cta="Forge"
            />
            <Step
              done={data.reposEnabled > 0}
              index={3}
              title="Enable a repo"
              hint="Enabled repos get a kanban board of their PRD-labeled issues."
              to="/repos"
              cta="Repos"
            />
            <Step
              done={data.workersOnline > 0}
              index={4}
              title="Bring a worker online"
              hint="Generate a join token and start the uzi-agent container with it."
              to="/workers"
              cta="Workers"
            />
          </ol>
        </Card>
      )}

      {/* PRD #40 §3–4: "Your usage" for everyone; the factory total + per-user
          breakdown only when an admin view was fetched (data.adminUsage non-null).
          A non-admin never receives factory data, so it can never render. */}
      {data?.selfUsage &&
        (data.adminUsage ? (
          <div className="grid gap-4 md:grid-cols-2">
            <YourUsageCard usage={data.selfUsage} />
            <FactoryTotalCard admin={data.adminUsage} />
          </div>
        ) : (
          <YourUsageCard usage={data.selfUsage} />
        ))}
      {data?.adminUsage && <PerUserUsageTable admin={data.adminUsage} />}

      <Card>
        <div className="flex items-center justify-between">
          <SectionTitle>Recent runs</SectionTitle>
          <Link to="/runs" className="text-xs font-medium text-brand hover:text-brand-hover">
            All runs →
          </Link>
        </div>
        {!data ? (
          <div className="mt-3 space-y-2">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        ) : recent.length === 0 ? (
          <p className="mt-3 text-sm text-faint">
            No runs yet — start one from a board card once the checklist above is green.
          </p>
        ) : (
          <ul className="mt-2 divide-y divide-edge">
            {recent.map((r) => {
              const mrState = mrChipState(r.mr_state);
              // PRD #122: compact milestone progress; null for a non-milestone run.
              const ms = milestoneBadge(r);
              const msBadge = ms ? milestoneBadgeText(ms) : null;
              return (
              // Issue #485 NB1: RunIssueRef renders a real forge <a>, which cannot nest
              // inside the row's navigational <Link> (also an <a> — invalid HTML). The
              // <li> is a `group relative` container, the run-details <Link> is a
              // stretched absolute overlay, and the content row sits in normal flow with
              // the interactive ref raised above the overlay (relative z-10). The
              // row-hover tint lives on the content layer via group-hover so the
              // transparent overlay never paints over the content.
              <li key={r.id} className="group relative">
                <Link
                  to={`/runs/${r.id}`}
                  // Issue #485 review FIX 2: name the link by the run's visible title so
                  // multiple issue-less runs (all "Open run" otherwise) get distinct,
                  // meaningful accessible names. issue_title is untrusted forge text, so it
                  // stays sanitized (same stripUnsafeChars as the visible title).
                  aria-label={`Open run${r.issue_iid != null ? ` for issue #${r.issue_iid}` : ""}${
                    r.issue_title.trim() ? `: ${stripUnsafeChars(r.issue_title)}` : ""
                  }`}
                  className="absolute inset-0 rounded-md"
                />
                <div className="flex items-center gap-3 py-2.5 transition-colors group-hover:bg-raised/40">
                  <div className="min-w-0 flex-1">
                    {/* Issue #124: forge-supplied issue title, untrusted (see RunsList). */}
                    <p className="truncate text-sm font-medium text-fg">{stripUnsafeChars(r.issue_title)}</p>
                    <p className="flex flex-wrap items-center gap-x-1 text-xs text-faint">
                      {maskRepoPath(r.repo_path, demo)}{" "}
                      <RunIssueRef
                        issueIid={r.issue_iid}
                        issueWebUrl={r.issue_web_url}
                        kind={r.kind}
                        forgeType={r.forge_type}
                        raised
                      />
                      {r.mr_iid != null && (
                        <MrChip
                          variant="inline"
                          label={`${mrAbbrev(r.forge_type)} `}
                          forgeType={r.forge_type}
                          mrIid={r.mr_iid}
                          mrState={mrState}
                          href={null}
                          // Issue #485 review FIX 1: raised above the card overlay
                          // (relative z-10) so its `title` (MR state) fires on hover.
                          className="relative z-10 ml-2"
                        />
                      )}
                    </p>
                  </div>
                  {/* Issue #485 review FIX 1: raise the right-side badge cluster above the
                      card's stretched-link overlay (relative z-10) so each badge's native
                      `title` tooltip fires on hover — the transparent overlay otherwise
                      intercepts the pointer and carries no title. These status badges are
                      not click-to-navigate targets, so the trade is correct; the title/body
                      text stays under the overlay and still opens the run. The wrapper keeps
                      the row's gap-3 spacing so the layout is unchanged. */}
                  <div className="relative z-10 flex items-center gap-3">
                    {r.iteration_count > 0 && (
                      <Badge tone="neutral" title="implement ⇄ review iterations">
                        iter {r.iteration_count}
                      </Badge>
                    )}
                    {/* PRD #122: compact milestone progress next to the iter badge; a
                        non-milestone run keeps only the iter badge. PRD #265 M2: an
                        unreported tracker shows M–/N, not a 0/N that reads as failure. */}
                    {msBadge && (
                      <Badge tone="info" title={msBadge.title}>
                        {msBadge.label}
                      </Badge>
                    )}
                    <RunHealthBadge run={r} />
                    <StatusPill status={effectiveRunStatus(r)} />
                  </div>
                </div>
              </li>
              );
            })}
          </ul>
        )}
      </Card>
    </div>
  );
}
