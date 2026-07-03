import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, type ForgeConnection, type Repo } from "../lib/api";
import { Alert, Button, Card, Select } from "../components/ui";

export function Repos() {
  const [connections, setConnections] = useState<ForgeConnection[]>([]);
  const [connectionId, setConnectionId] = useState("");
  const [repos, setRepos] = useState<Repo[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [busyId, setBusyId] = useState<string | null>(null);

  useEffect(() => {
    (async () => {
      try {
        const { connections } = await api.listConnections();
        setConnections(connections);
        if (connections.length > 0) setConnectionId(connections[0].id);
      } catch (err) {
        setError(err instanceof ApiError ? err.message : "Failed to load connections");
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  const loadProjects = useCallback(async (connId: string) => {
    if (!connId) return;
    setError("");
    setRefreshing(true);
    try {
      const { repos } = await api.listProjects(connId);
      setRepos(repos);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load projects");
    } finally {
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    if (connectionId) loadProjects(connectionId);
  }, [connectionId, loadProjects]);

  const toggle = async (repo: Repo) => {
    setError("");
    setBusyId(repo.id);
    try {
      const { repo: updated } = await api.setRepoEnabled(repo.id, !repo.enabled);
      setRepos((prev) => prev.map((r) => (r.id === updated.id ? updated : r)));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Update failed");
    } finally {
      setBusyId(null);
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Repos</h1>
          <p className="mt-1 text-slate-400">
            Projects your bot can see. Enable one to track its PRD issues on a board.
          </p>
        </div>
        {connectionId && (
          <Button variant="ghost" disabled={refreshing} onClick={() => loadProjects(connectionId)}>
            {refreshing ? "Refreshing…" : "Refresh list"}
          </Button>
        )}
      </div>

      {error && <Alert message={error} />}

      {loading ? (
        <p className="text-slate-500">Loading…</p>
      ) : connections.length === 0 ? (
        <Card>
          <p className="text-slate-400">
            No forge connection yet. Add one under{" "}
            <Link to="/settings/forge" className="text-indigo-400 hover:text-indigo-300">
              Settings → Forge
            </Link>
            .
          </p>
        </Card>
      ) : (
        <>
          {connections.length > 1 && (
            <div className="max-w-md">
              <Select value={connectionId} onChange={(e) => setConnectionId(e.target.value)}>
                {connections.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.bot_username} — {c.base_url}
                  </option>
                ))}
              </Select>
            </div>
          )}

          <Card className="p-0">
            <div className="overflow-x-auto">
              <table className="w-full text-left text-sm">
                <thead className="border-b border-slate-800 text-slate-400">
                  <tr>
                    <th className="px-4 py-3 font-medium">Project</th>
                    <th className="px-4 py-3 font-medium">Default branch</th>
                    <th className="px-4 py-3 font-medium">Status</th>
                    <th className="px-4 py-3 text-right font-medium">Actions</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-800">
                  {repos.length === 0 ? (
                    <tr>
                      <td colSpan={4} className="px-4 py-6 text-center text-slate-500">
                        {refreshing ? "Loading…" : "No projects found for this bot."}
                      </td>
                    </tr>
                  ) : (
                    repos.map((r) => (
                      <tr key={r.id}>
                        <td className="px-4 py-3">
                          <a
                            href={r.web_url}
                            target="_blank"
                            rel="noreferrer"
                            className="font-medium text-slate-100 hover:text-indigo-300"
                          >
                            {r.path_with_namespace}
                          </a>
                        </td>
                        <td className="px-4 py-3 text-slate-400">{r.default_branch ?? "—"}</td>
                        <td className="px-4 py-3">
                          <span className={r.enabled ? "text-emerald-400" : "text-slate-500"}>
                            {r.enabled ? "Enabled" : "Disabled"}
                          </span>
                        </td>
                        <td className="px-4 py-3 text-right">
                          <div className="flex justify-end gap-2">
                            {r.enabled && (
                              <Link to={`/repos/${r.id}/board`}>
                                <Button variant="ghost">Open board</Button>
                              </Link>
                            )}
                            <Button
                              variant={r.enabled ? "danger" : "primary"}
                              disabled={busyId === r.id}
                              onClick={() => toggle(r)}
                            >
                              {r.enabled ? "Disable" : "Enable"}
                            </Button>
                          </div>
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>
          </Card>
        </>
      )}
    </div>
  );
}
