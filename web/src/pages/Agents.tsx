import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type AgentTemplate } from "../lib/api";
import { isLeadTemplateName, summarizeTools } from "../lib/agentTemplates";
import { Alert, Badge, Button, Card, ListSkeleton, PageHeader } from "../components/ui";
import { PlusIcon } from "../components/icons";

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
      <PageHeader
        title="Agents"
        description={`Agent templates render to Claude Code subagent files. Everyone can view them${user?.is_admin ? "; admins can edit, reset, and add new ones." : "."}`}
        actions={
          user?.is_admin ? (
            <Link to="/agents/new">
              <Button size="sm">
                <PlusIcon /> New template
              </Button>
            </Link>
          ) : undefined
        }
      />

      {error && <Alert message={error} />}

      {loading ? (
        <ListSkeleton rows={6} />
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Name</th>
                  <th className="px-4 py-3 font-medium">Description</th>
                  <th className="px-4 py-3 font-medium">Model</th>
                  <th className="px-4 py-3 font-medium">Tools</th>
                  <th className="px-4 py-3 font-medium">Kind</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {templates.length === 0 ? (
                  <tr>
                    <td colSpan={5} className="px-4 py-6 text-center text-faint">
                      No templates.
                    </td>
                  </tr>
                ) : (
                  templates.map((t) => (
                    <tr key={t.id} className="transition-colors hover:bg-raised/30">
                      <td className="px-4 py-3">
                        <span className="inline-flex items-center gap-2">
                          <Link
                            to={`/agents/${t.id}`}
                            className="font-mono text-brand hover:text-brand-hover"
                          >
                            {t.name}
                          </Link>
                          {isLeadTemplateName(t.name) && (
                            <Badge tone="brand" title="The orchestrator: the main agent thread that plans and delegates.">
                              orchestrator
                            </Badge>
                          )}
                        </span>
                      </td>
                      <td className="max-w-md px-4 py-3 text-muted">{t.description}</td>
                      <td className="px-4 py-3 text-muted">{t.model ?? "inherit"}</td>
                      <td className="max-w-xs truncate px-4 py-3 font-mono text-xs text-muted">
                        {summarizeTools(t)}
                      </td>
                      <td className="px-4 py-3">
                        <Badge tone={t.is_builtin ? "brand" : "neutral"}>
                          {t.is_builtin ? "builtin" : "custom"}
                        </Badge>
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </div>
  );
}
