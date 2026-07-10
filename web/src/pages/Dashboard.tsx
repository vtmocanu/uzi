// The overview: stat tiles, an onboarding checklist derived from the same
// preconditions the board's start-run gate checks (lib/runStream.ts
// startRunGate), and the most recent runs. Replaces the old account-metadata
// table (moved to Settings), which told a new user nothing about what to do
// next.

import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, isTerminalRun, type RunListItem } from "../lib/api";
import { mrChipState } from "../lib/runBadge";
import { MrChip } from "../components/MrChip";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { Badge, Button, Card, cx, PageHeader, SectionTitle, Skeleton, StatTile, StatusPill } from "../components/ui";
import { CheckIcon, ChevronRightIcon } from "../components/icons";

interface Overview {
  runs: RunListItem[];
  workersOnline: number;
  workersTotal: number;
  reposEnabled: number;
  templates: number;
  hasToken: boolean;
  hasForge: boolean;
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
        <p className={cx("text-sm font-medium", done ? "text-muted line-through decoration-edge-strong" : "text-fg")}>
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
        const [{ runs }, { workers }, { repos }, { templates }, { secrets }, { connections }] =
          await Promise.all([
            api.listRuns(),
            api.listWorkers(),
            api.listRepos(),
            api.listAgentTemplates(),
            api.listSecrets(),
            api.listConnections(),
          ]);
        if (cancelled) return;
        setData({
          runs,
          workersOnline: workers.filter((w) => w.status === "online").length,
          workersTotal: workers.length,
          reposEnabled: repos.filter((r) => r.enabled).length,
          templates: templates.length,
          hasToken: secrets.some((s) => s.kind === "anthropic_token"),
          hasForge: connections.length > 0,
        });
      } catch {
        if (!cancelled) setData(null);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

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

  const active = data?.runs.filter((r) => !isTerminalRun(r.status)) ?? [];
  const recent = data?.runs.slice(0, 5) ?? [];
  const steps = data
    ? [data.hasToken, data.hasForge, data.reposEnabled > 0, data.workersOnline > 0]
    : [];
  const ready = steps.length > 0 && steps.every(Boolean);

  return (
    <div className="space-y-6">
      <PageHeader
        title={`Welcome${user.display_name ? `, ${user.display_name}` : ""}`}
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
            hint={active.length ? "agents at work" : "nothing in flight"}
            to="/runs"
          />
          <StatTile
            label="Workers online"
            value={`${data.workersOnline}/${data.workersTotal}`}
            hint="your fleet"
            to="/settings/workers"
          />
          <StatTile label="Boards" value={data.reposEnabled} hint="enabled repos" to="/repos" />
          <StatTile label="Agent templates" value={data.templates} hint="roles available" to="/agents" />
        </div>
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
              title="Connect your GitLab bot"
              hint="A bot PAT with the api scope lets uzi see and move your issues."
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
              to="/settings/workers"
              cta="Workers"
            />
          </ol>
        </Card>
      )}

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
              return (
              <li key={r.id}>
                <Link
                  to={`/runs/${r.id}`}
                  className="flex items-center gap-3 py-2.5 transition-colors hover:bg-raised/40"
                >
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm font-medium text-fg">{r.issue_title}</p>
                    <p className="text-xs text-faint">
                      {r.repo_path} #{r.issue_iid}
                      {r.mr_iid != null && (
                        <MrChip variant="inline" label="MR " mrIid={r.mr_iid} mrState={mrState} href={null} className="ml-2" />
                      )}
                    </p>
                  </div>
                  {r.iteration_count > 0 && (
                    <Badge tone="neutral" title="implement ⇄ review iterations">
                      iter {r.iteration_count}
                    </Badge>
                  )}
                  <StatusPill status={r.status} />
                </Link>
              </li>
              );
            })}
          </ul>
        )}
      </Card>
    </div>
  );
}
