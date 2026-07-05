import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import {
  api,
  ApiError,
  isTerminalRun,
  type AdminWorker,
  type RunListItem,
} from "../lib/api";
import { Alert, Badge, Card } from "../components/ui";
import { isStoppedRun, runStatusTone } from "../lib/runBadge";

function RunRow({ run, showOwner }: { run: RunListItem; showOwner?: boolean }) {
  return (
    <li className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-slate-800 bg-slate-900/40 px-3 py-2">
      <div className="min-w-0">
        <Link
          to={`/runs/${run.id}`}
          className="block truncate font-medium text-slate-100 hover:text-indigo-300"
        >
          {run.issue_title}
        </Link>
        <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-slate-500">
          <span>
            {run.repo_path} #{run.issue_iid}
          </span>
          {run.worker_name && <span>· {run.worker_name}</span>}
          {showOwner && run.owner_email && <span>· {run.owner_email}</span>}
          <span>· {new Date(run.updated_at).toLocaleString()}</span>
        </div>
      </div>
      <Badge tone={runStatusTone(run.status, run.failure_reason)}>
        {isStoppedRun(run.status, run.failure_reason) ? "stopped" : run.status.replace("_", " ")}
      </Badge>
    </li>
  );
}

export function RunsList() {
  const { user } = useAuth();
  const isAdmin = !!user?.is_admin;

  const [runs, setRuns] = useState<RunListItem[]>([]);
  const [adminRuns, setAdminRuns] = useState<RunListItem[]>([]);
  const [adminWorkers, setAdminWorkers] = useState<AdminWorker[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setError("");
    try {
      const [{ runs }, admin] = await Promise.all([
        api.listRuns(),
        isAdmin
          ? Promise.all([api.adminListRuns(), api.adminListWorkers()])
          : Promise.resolve(null),
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

  const active = runs.filter((r) => !isTerminalRun(r.status));
  const finished = runs.filter((r) => isTerminalRun(r.status));

  return (
    <div className="space-y-6">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Runs</h1>
          <p className="mt-1 text-slate-400">Your agent runs. Open one to watch it live.</p>
        </div>
        <Link to="/settings/workers" className="text-sm text-indigo-400 hover:text-indigo-300">
          Workers
        </Link>
      </div>

      {error && <Alert message={error} />}
      {loading && <p className="text-slate-500">Loading…</p>}

      {!loading && (
        <Card className="space-y-4">
          <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">Active</h2>
          {active.length === 0 ? (
            <p className="text-sm text-slate-600">No active runs. Start one from a board card.</p>
          ) : (
            <ul className="space-y-2">
              {active.map((r) => (
                <RunRow key={r.id} run={r} />
              ))}
            </ul>
          )}
          {finished.length > 0 && (
            <>
              <h2 className="pt-2 text-sm font-semibold uppercase tracking-wide text-slate-500">
                Finished
              </h2>
              <ul className="space-y-2">
                {finished.map((r) => (
                  <RunRow key={r.id} run={r} />
                ))}
              </ul>
            </>
          )}
        </Card>
      )}

      {isAdmin && !loading && (
        <Card className="space-y-4 border-indigo-900/60">
          <h2 className="text-sm font-semibold uppercase tracking-wide text-indigo-300">
            Agents status (admin)
          </h2>
          <div>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500">
              Active runs · all users
            </h3>
            {adminRuns.length === 0 ? (
              <p className="text-sm text-slate-600">No active runs across the factory.</p>
            ) : (
              <ul className="space-y-2">
                {adminRuns.map((r) => (
                  <RunRow key={r.id} run={r} showOwner />
                ))}
              </ul>
            )}
          </div>
          <div>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500">
              Workers · all users
            </h3>
            {adminWorkers.length === 0 ? (
              <p className="text-sm text-slate-600">No workers registered.</p>
            ) : (
              <ul className="space-y-2">
                {adminWorkers.map((w) => (
                  <li
                    key={w.id}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-slate-800 bg-slate-900/40 px-3 py-2 text-sm"
                  >
                    <div>
                      <span className="font-medium text-slate-100">{w.name}</span>
                      <span className="ml-2 text-xs text-slate-500">{w.owner_email}</span>
                    </div>
                    <div className="flex items-center gap-1.5">
                      <Badge tone={w.status === "online" ? "neutral" : "danger"}>{w.status}</Badge>
                      {w.busy && <Badge tone="warning">busy</Badge>}
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
