import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type AgentTemplate } from "../lib/api";
import { summarizeTools } from "../lib/agentTemplates";
import { Alert, Button, Card } from "../components/ui";

export function Agents() {
  const { user } = useAuth();
  const [templates, setTemplates] = useState<AgentTemplate[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    try {
      const { templates } = await api.listAgentTemplates();
      setTemplates(templates);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load templates");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Agents</h1>
          <p className="mt-1 text-slate-400">
            Agent templates render to Claude Code subagent files. Everyone can view
            them{user?.is_admin ? "; admins can edit, reset, and add new ones." : "."}
          </p>
        </div>
        {user?.is_admin && (
          <Link to="/agents/new">
            <Button>New template</Button>
          </Link>
        )}
      </div>

      {error && <Alert message={error} />}

      <Card className="p-0">
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="border-b border-slate-800 text-slate-400">
              <tr>
                <th className="px-4 py-3 font-medium">Name</th>
                <th className="px-4 py-3 font-medium">Description</th>
                <th className="px-4 py-3 font-medium">Model</th>
                <th className="px-4 py-3 font-medium">Tools</th>
                <th className="px-4 py-3 font-medium">Kind</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800">
              {loading ? (
                <tr>
                  <td colSpan={5} className="px-4 py-6 text-center text-slate-500">
                    Loading…
                  </td>
                </tr>
              ) : templates.length === 0 ? (
                <tr>
                  <td colSpan={5} className="px-4 py-6 text-center text-slate-500">
                    No templates.
                  </td>
                </tr>
              ) : (
                templates.map((t) => (
                  <tr key={t.id} className="hover:bg-slate-900/40">
                    <td className="px-4 py-3">
                      <Link
                        to={`/agents/${t.id}`}
                        className="font-mono text-indigo-300 hover:text-indigo-200"
                      >
                        {t.name}
                      </Link>
                    </td>
                    <td className="px-4 py-3 text-slate-300">{t.description}</td>
                    <td className="px-4 py-3 text-slate-400">{t.model ?? "inherit"}</td>
                    <td className="max-w-xs truncate px-4 py-3 text-slate-400">
                      {summarizeTools(t)}
                    </td>
                    <td className="px-4 py-3">
                      {t.is_builtin ? (
                        <span className="rounded bg-indigo-950 px-2 py-0.5 text-xs text-indigo-300">
                          builtin
                        </span>
                      ) : (
                        <span className="rounded bg-slate-800 px-2 py-0.5 text-xs text-slate-400">
                          custom
                        </span>
                      )}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
}
